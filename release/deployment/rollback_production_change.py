#!/usr/bin/env python3
"""Roll production back by one signed digest tuple using one exact Apps PUT.

The DigitalOcean provider rollback endpoints are intentionally not implemented.
Rollback is an ordinary full-spec-preserving update to a signed prior image
tuple, subject to the rollout's explicit rollback floors.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import os
import sys
import time
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import apply_production_change as apply_control
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/rollback-production-phase.yml"
AUTHORITY = "production-phase-rollback-receipt"
ORPHAN_WORKFLOW_PATH = ".github/workflows/rollback-production-orphan.yml"
ORPHAN_AUTHORITY = "production-orphan-rollback-receipt"


def _binding(value: Any, label: str) -> dict[str, Any]:
    item = common.exact_keys(
        value,
        {"run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"},
        label,
    )
    return {
        "run_id": common.require_run_id(item["run_id"], f"{label} run ID"),
        "run_attempt": common.exact_int(item["run_attempt"], f"{label} run attempt", 1, 1),
        "artifact_id": common.require_run_id(item["artifact_id"], f"{label} artifact ID"),
        "artifact_digest": common.require_digest(item["artifact_digest"], f"{label} artifact digest"),
        "sha256": common.require_sha256(item["sha256"], f"{label} file hash"),
    }


def _current_binding(value: Any, label: str) -> dict[str, Any]:
    item = common.exact_keys(
        value,
        {"kind", "run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"},
        label,
    )
    kind = common.exact_string(item["kind"], f"{label} kind")
    if kind not in {"phase-state", "apply-receipt", "reconciliation-receipt"}:
        common.fail(f"{label} kind differs")
    binding = _binding({key: item[key] for key in item if key != "kind"}, label)
    return {"kind": kind, **binding}


def validate_request(value: Any) -> dict[str, Any]:
    request = common.exact_keys(
        value,
        {"current_state", "target_state", "recovery", "rollout_plan_sha256"},
        "rollback evidence request",
    )
    return {
        "current_state": _current_binding(request["current_state"], "current production authority"),
        "target_state": _binding(request["target_state"], "target phase-state authority"),
        "recovery": _binding(request["recovery"], "rollback recovery authority"),
        "rollout_plan_sha256": common.require_sha256(request["rollout_plan_sha256"], "rollback rollout plan hash"),
    }


def _digests(state: Mapping[str, Any]) -> dict[str, str]:
    output: dict[str, str] = {}
    for item in state["provider_state"]["images"]:
        component = item["component"]
        if component in output:
            common.fail("phase state contains duplicate images")
        output[component] = common.require_digest(item["digest"], "phase-state image digest")
    if set(output) != {"web", "meta-relay", "gmail-relay"}:
        common.fail("phase-state image tuple differs")
    return output


def _validate_rollback_receipt(
    value: Any,
    *,
    expected_authority: str,
    expected_workflow_path: str,
) -> dict[str, Any]:
    receipt = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "completed_at", "control",
            "lineage", "authorities", "provider_transition", "before", "after",
            "target_authority", "gates", "rollback", "canary",
        },
        "production rollback receipt",
    )
    if receipt["schema_version"] != 1 or receipt["authority"] != expected_authority or receipt["repository"] != common.REPOSITORY:
        common.fail("rollback receipt authority differs")
    common.require_timestamp(receipt["completed_at"], "rollback completion time")
    control = common.exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt", "runner_environment",
            "release_policy_sha256", "change_schema_sha256", "controller_sha256",
        },
        "rollback control",
    )
    common.require_sha1(control["workflow_sha"], "rollback workflow SHA")
    if control["workflow_path"] != expected_workflow_path or control["runner_environment"] != "github-hosted":
        common.fail("rollback workflow identity differs")
    common.require_run_id(control["run_id"], "rollback run ID")
    common.exact_int(control["run_attempt"], "rollback run attempt", 1, 1)
    for key in ("release_policy_sha256", "change_schema_sha256", "controller_sha256"):
        common.require_sha256(control[key], f"rollback {key}")
    lineage = common.exact_keys(
        receipt["lineage"],
        {
            "event_sequence", "phase_ordinal", "operation", "from", "to",
            "predecessor_kind", "predecessor_state_sha256", "phase",
            "phase_source_sha",
        },
        "rollback lineage",
    )
    current = common.validate_phase(lineage["from"], "rollback source phase")
    target = common.validate_phase(lineage["to"], "rollback target phase")
    common.validate_rollback_transition(current, target)
    common.exact_int(lineage["event_sequence"], "rollback event sequence", 2)
    if (
        lineage["phase_ordinal"] != common.PHASES.index(target) + 1
        or lineage["operation"] != "rollback"
        or lineage["phase"] != target
        or lineage["predecessor_kind"] not in {
            "phase-state", "apply-receipt", "reconciliation-receipt",
        }
    ):
        common.fail("rollback lineage differs")
    common.require_sha256(lineage["predecessor_state_sha256"], "rollback predecessor hash")
    common.require_sha1(lineage["phase_source_sha"], "rollback target source SHA")
    authorities = common.exact_keys(
        receipt["authorities"],
        {
            "rollout_plan_sha256", "current_state", "target_state", "recovery",
            "mutation_intent", "main_lock_proof",
        },
        "rollback authorities",
    )
    common.require_sha256(authorities["rollout_plan_sha256"], "rollback rollout plan hash")
    _current_binding(authorities["current_state"], "rollback current authority")
    for key in ("target_state", "recovery"):
        _binding(authorities[key], f"rollback {key} authority")
    common.validate_full_artifact_binding(
        authorities["mutation_intent"], "rollback mutation intent authority"
    )
    common.validate_full_artifact_binding(
        authorities["main_lock_proof"], "rollback main lock proof authority"
    )
    target_authority = common.exact_keys(
        receipt["target_authority"], {"production_plan_sha256"}, "rollback target authority"
    )
    common.require_sha256(
        target_authority["production_plan_sha256"], "rollback target production plan hash"
    )
    if (
        lineage["predecessor_kind"] != authorities["current_state"]["kind"]
        or lineage["predecessor_state_sha256"] != authorities["current_state"]["sha256"]
    ):
        common.fail("rollback receipt predecessor authority differs")
    transition = common.exact_keys(
        receipt["provider_transition"],
        {
            "http_methods_used", "http_request_count", "mutation_request_count",
            "endpoint_labels", "mutation_fingerprint_sha256", "ambiguous_reconciled",
        },
        "rollback provider transition",
    )
    if transition["http_methods_used"] != ["GET", "PUT"] or transition["mutation_request_count"] != 1:
        common.fail("rollback provider method ledger differs")
    common.exact_int(transition["http_request_count"], "rollback request count", 11, 10_000)
    if transition["endpoint_labels"] != ["app", "deployment"]:
        common.fail("rollback endpoint labels differ")
    common.require_sha256(transition["mutation_fingerprint_sha256"], "rollback mutation fingerprint")
    common.exact_bool(transition["ambiguous_reconciled"], "rollback ambiguity proof")
    before = common._validate_public_provider_state(receipt["before"], "rollback before", allow_legacy=False)
    after = common._validate_public_provider_state(receipt["after"], "rollback after", allow_legacy=False)
    for key in ("app_identity_sha256", "default_ingress_sha256", "environment_values_sha256", "non_source_projection_sha256"):
        if before[key] != after[key]:
            common.fail("rollback did not preserve production state")
    if receipt["gates"] != {"deployment_succeeded": True, "migration_succeeded": True}:
        common.fail("rollback gates are incomplete")
    if receipt["rollback"] != common.ROLLBACK_FLOORS[target]:
        common.fail("post-rollback floor differs")
    canary = common.exact_keys(
        receipt["canary"],
        {"required", "completed", "endpoint_labels", "route_contract_sha256"},
        "rollback canary requirement",
    )
    if canary != {
        "required": True,
        "completed": False,
        "endpoint_labels": apply_control.ENDPOINT_LABELS,
        "route_contract_sha256": canary["route_contract_sha256"],
    }:
        common.fail("rollback canary requirement differs")
    common.require_sha256(canary["route_contract_sha256"], "rollback route contract hash")
    common.sanitize_public(receipt)
    return receipt


def validate_rollback_receipt(value: Any) -> dict[str, Any]:
    return _validate_rollback_receipt(
        value, expected_authority=AUTHORITY, expected_workflow_path=WORKFLOW_PATH
    )


def validate_orphan_rollback_receipt(value: Any) -> dict[str, Any]:
    receipt = _validate_rollback_receipt(
        value,
        expected_authority=ORPHAN_AUTHORITY,
        expected_workflow_path=ORPHAN_WORKFLOW_PATH,
    )
    if receipt["lineage"]["predecessor_kind"] != "reconciliation-receipt":
        common.fail("orphan rollback must descend from a reconciliation receipt")
    return receipt


def _require_rollout_plan_authority(
    authorities: Mapping[str, Any],
    current_kind: str,
    current_state: Mapping[str, Any],
    target_state: Mapping[str, Any],
) -> None:
    """Reject cross-release splicing of a current authority and rollback target."""
    reviewed = common.require_sha256(
        authorities["rollout_plan_sha256"], "rollback rollout plan hash"
    )
    if current_kind == "phase-state":
        current_rollout = current_state["evidence"]["rollout_plan_sha256"]
    elif current_kind == "apply-receipt":
        current_rollout = current_state["authorities"]["rollout_plan_sha256"]
    elif current_kind == "reconciliation-receipt":
        current_rollout = current_state["authorities"]["upstream"]["rollout_plan_sha256"]
    else:  # Defensive: callers normally pass validate_request output.
        common.fail("rollback current authority kind differs")
    current_rollout = common.require_sha256(
        current_rollout, "current production rollout plan hash"
    )
    target_rollout = common.require_sha256(
        target_state["evidence"]["rollout_plan_sha256"],
        "target production rollout plan hash",
    )
    if reviewed != current_rollout or reviewed != target_rollout:
        common.fail("rollback current and target authorities belong to different rollout plans")


def _load_current_authority_state(
    current_kind: str, current_state: Mapping[str, Any]
) -> tuple[dict[str, Any], dict[str, Any], str, int]:
    if current_kind == "phase-state":
        state = common.validate_phase_state(current_state)
        return (
            state,
            state["provider_state"],
            state["lineage"]["phase"],
            state["lineage"]["event_sequence"],
        )
    if current_kind == "apply-receipt":
        state = common.validate_apply_receipt(current_state)
    elif current_kind == "reconciliation-receipt":
        state = common.validate_reconciliation_receipt(current_state)
        if state["classification"]["outcome"] not in {
            "committed", "already-receipted",
        }:
            common.fail("rollback reconciliation authority is not committed")
    else:
        common.fail("rollback current authority kind differs")
    return (
        state,
        state["after"],
        state["lineage"]["phase"],
        state["lineage"]["event_sequence"],
    )


def _current_artifact_name(binding: Mapping[str, Any]) -> str:
    prefixes = {
        "phase-state": "production-phase-state",
        "apply-receipt": "production-phase-apply",
        "reconciliation-receipt": "production-orphan-reconciliation",
    }
    return f"{prefixes[binding['kind']]}-{binding['run_id']}-{binding['run_attempt']}"


def _bind_rollback_lock_authority(
    current_kind: str,
    current_state: Mapping[str, Any],
    lock_authority: Mapping[str, Any],
) -> dict[str, Any]:
    """Bind an inherited lock to the exact reconciled orphan chain.

    A first compensation inherits the original acquired lock and records the
    original v2 intent hash as its owner.  A later compensation keeps the same
    root/owner tuple.  This prevents a valid locked rule or acquire artifact
    from another orphan chain being spliced into the rollback intent.
    """
    supplied = copy.deepcopy(dict(lock_authority))
    if current_kind != "reconciliation-receipt":
        if supplied.get("mode") != "planned" or supplied.get("strategy") != "acquire":
            common.fail("ordinary rollback requires its newly acquired lock")
        return supplied
    source_lock = current_state["intent"].get("lock")
    if type(source_lock) is not dict:
        common.fail("reconciliation authority omits its mutation lock")
    expected = copy.deepcopy(source_lock)
    if expected.get("mode") != "planned":
        common.fail("reconciliation mutation lock mode differs")
    if expected.get("strategy") == "acquire":
        expected["strategy"] = "inherit"
        expected["expected_pre_lock"]["lock_branch"] = True
        expected["owner_intent_sha256"] = current_state["intent"]["binding"]["sha256"]
    elif expected.get("strategy") != "inherit":
        common.fail("reconciliation mutation lock strategy differs")
    if supplied != expected:
        common.fail("orphan rollback lock is not the exact reconciled lock chain")
    return supplied


def prepare_rollback_mutation_intent(
    *,
    control: Mapping[str, Any],
    current_state: Mapping[str, Any],
    current_state_sha256: str,
    target_state: Mapping[str, Any],
    target_state_sha256: str,
    recovery: Mapping[str, Any],
    recovery_sha256: str,
    authorities: Mapping[str, Any],
    lock_authority: Mapping[str, Any],
    release_policy_sha256: str,
    change_schema_sha256: str,
    mutation_intent_schema_sha256: str,
    controller_sha256: str,
    route_contract_sha256: str,
    now: dt.datetime,
) -> dict[str, Any]:
    checked = apply_control._clock_value(lambda: now)
    current_kind = authorities["current_state"]["kind"]
    current_state, before, current_phase, current_sequence = _load_current_authority_state(
        current_kind, current_state
    )
    lock_authority = _bind_rollback_lock_authority(
        current_kind, current_state, lock_authority
    )
    target_state = common.validate_phase_state(target_state)
    if common.sha256_bytes(common.canonical_file_bytes(current_state)) != common.require_sha256(
        current_state_sha256, "current authority hash"
    ):
        common.fail("current authority exact-file hash differs")
    if common.sha256_bytes(common.canonical_file_bytes(target_state)) != common.require_sha256(
        target_state_sha256, "target phase-state hash"
    ):
        common.fail("target state exact-file hash differs")
    if (
        authorities["current_state"]["sha256"] != current_state_sha256
        or authorities["target_state"]["sha256"] != target_state_sha256
        or authorities["recovery"]["sha256"] != recovery_sha256
    ):
        common.fail("rollback artifact authority differs")
    target_phase = target_state["lineage"]["phase"]
    common.validate_rollback_transition(current_phase, target_phase)
    _require_rollout_plan_authority(authorities, current_kind, current_state, target_state)
    recovery = apply_control.validate_recovery(recovery, recovery_sha256, checked)
    target_images = _digests(target_state)
    desired = apply_control._desired_projection(
        canonical_spec_sha256=target_state["provider_state"]["canonical_spec_sha256"],
        before=before,
        target_digests=target_images,
    )
    prepared_at = common.format_timestamp(checked)
    expires_at = common.format_timestamp(
        min(
            checked + dt.timedelta(seconds=common.MAX_PLAN_AGE_SECONDS),
            common.require_timestamp(recovery["expires_at"], "rollback recovery expiry"),
        )
    )
    current_binding = {
        "kind": current_kind,
        **dict(authorities["current_state"]),
        "artifact_name": _current_artifact_name(authorities["current_state"]),
    }
    target_binding = apply_control._full_binding(
        authorities["target_state"],
        f"production-phase-state-{authorities['target_state']['run_id']}-{authorities['target_state']['run_attempt']}",
    )
    recovery_binding = apply_control._full_binding(
        authorities["recovery"],
        f"production-recovery-readiness-{authorities['recovery']['run_id']}-{authorities['recovery']['run_attempt']}",
    )
    intent_control = {
        **dict(control),
        "release_policy_sha256": common.require_sha256(
            release_policy_sha256, "rollback release policy hash"
        ),
        "change_schema_sha256": common.require_sha256(
            change_schema_sha256, "rollback phase schema hash"
        ),
        "mutation_intent_schema_sha256": common.require_sha256(
            mutation_intent_schema_sha256, "rollback mutation intent schema hash"
        ),
        "controller_sha256": common.require_sha256(
            controller_sha256, "rollback controller hash"
        ),
    }
    lineage = {
        "event_sequence": current_sequence + 1,
        "phase_ordinal": common.PHASES.index(target_phase) + 1,
        "operation": "rollback",
        "from": current_phase,
        "to": target_phase,
        "predecessor_kind": current_kind,
        "predecessor_state_sha256": current_state_sha256,
        "phase": target_phase,
        "phase_source_sha": target_state["lineage"]["phase_source_sha"],
    }
    before_sha = common.sha256_value(before)
    desired_sha = common.sha256_value(desired)
    intent = {
        "schema_version": 2,
        "authority": "production-mutation-intent",
        "repository": common.REPOSITORY,
        "prepared_at": prepared_at,
        "expires_at": expires_at,
        "control": intent_control,
        "operation": "rollback",
        "lineage": lineage,
        "authorities": {
            "rollout_plan_sha256": authorities["rollout_plan_sha256"],
            "current_state": current_binding,
            "target_state": target_binding,
            "recovery": recovery_binding,
            "target_authority": {
                "production_plan_sha256": target_state["evidence"]["production_plan_sha256"]
            },
        },
        "lock": lock_authority,
        "before": copy.deepcopy(before),
        "desired": desired,
        "mutation": {
            "http_method": "PUT",
            "endpoint_label": "app",
            "update_all_source_versions": False,
            "before_sha256": before_sha,
            "desired_sha256": desired_sha,
            "mutation_fingerprint_sha256": common.sha256_value(
                {
                    "before_sha256": before_sha,
                    "desired_sha256": desired_sha,
                    "http_method": "PUT",
                    "endpoint_label": "app",
                    "update_all_source_versions": False,
                }
            ),
        },
        "rollback": copy.deepcopy(target_state["rollback"]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": apply_control.ENDPOINT_LABELS,
            "route_contract_sha256": common.require_sha256(
                route_contract_sha256, "rollback route contract hash"
            ),
        },
    }
    return common.validate_mutation_intent(intent, now=checked)


def rollback_change(
    *,
    target_descriptor: Mapping[str, str],
    control: Mapping[str, Any],
    token: str,
    current_state: Mapping[str, Any],
    current_state_sha256: str,
    target_state: Mapping[str, Any],
    target_state_sha256: str,
    recovery: Mapping[str, Any],
    recovery_sha256: str,
    authorities: Mapping[str, Any],
    release_policy_sha256: str,
    change_schema_sha256: str,
    controller_sha256: str,
    route_contract_sha256: str | None,
    mutation_intent: Mapping[str, Any] | None = None,
    mutation_intent_sha256: str | None = None,
    mutation_intent_authority: Mapping[str, Any] | None = None,
    main_lock_proof: Mapping[str, Any] | None = None,
    main_lock_proof_sha256: str | None = None,
    main_lock_proof_authority: Mapping[str, Any] | None = None,
    now: dt.datetime | None = None,
    clock: Callable[[], dt.datetime] | None = None,
    opener: Any | None = None,
    sleeper: Callable[[float], None] = time.sleep,
    poll_limit: int = apply_control.POLL_LIMIT,
) -> dict[str, Any]:
    time_source = clock or (
        (lambda fixed=now: fixed)
        if now is not None
        else (lambda: dt.datetime.now(dt.timezone.utc))
    )
    checked = apply_control._clock_value(time_source)
    if mutation_intent is None or mutation_intent_sha256 is None or mutation_intent_authority is None:
        common.fail("a durable signed rollback mutation intent is required")
    mutation_intent = common.validate_mutation_intent(mutation_intent, now=checked)
    mutation_intent_hash = common.require_sha256(
        mutation_intent_sha256, "rollback mutation intent exact-file hash"
    )
    if common.sha256_bytes(common.canonical_file_bytes(mutation_intent)) != mutation_intent_hash:
        common.fail("rollback mutation intent exact-file hash differs")
    mutation_intent_authority = common.validate_full_artifact_binding(
        mutation_intent_authority, "rollback mutation intent authority"
    )
    orphan_mode = mutation_intent["lock"]["strategy"] == "inherit"
    intent_prefix = "orphan-rollback" if orphan_mode else "rollback"
    if (
        mutation_intent_authority["sha256"] != mutation_intent_hash
        or mutation_intent_authority["artifact_name"]
        != f"production-mutation-intent-{intent_prefix}-{mutation_intent_authority['run_id']}-1"
    ):
        common.fail("rollback mutation intent artifact authority differs")
    expected_workflow_path = ORPHAN_WORKFLOW_PATH if orphan_mode else WORKFLOW_PATH
    if (
        control.get("workflow_path") != expected_workflow_path
        or mutation_intent["control"]["workflow_path"] != expected_workflow_path
        or mutation_intent["control"]["workflow_sha"] != control.get("workflow_sha")
        or str(mutation_intent["control"]["run_id"]) != str(control.get("run_id"))
        or mutation_intent["control"]["run_attempt"] != control.get("run_attempt")
        or mutation_intent["control"]["controller_sha256"]
        != common.require_sha256(controller_sha256, "rollback controller hash")
    ):
        common.fail("rollback mutation intent control authority differs")
    if main_lock_proof is None or main_lock_proof_sha256 is None or main_lock_proof_authority is None:
        common.fail("a signed post-lock proof is required before rollback mutation")
    main_lock_proof = common.validate_main_lock_proof(
        main_lock_proof, mutation_intent=mutation_intent, now=checked
    )
    proof_hash = common.require_sha256(
        main_lock_proof_sha256, "rollback main lock proof exact-file hash"
    )
    proof_authority = common.validate_full_artifact_binding(
        main_lock_proof_authority, "rollback main lock proof artifact authority"
    )
    proof_prefix = "orphan-rollback" if orphan_mode else "rollback"
    if (
        common.sha256_bytes(common.canonical_file_bytes(main_lock_proof)) != proof_hash
        or proof_authority["sha256"] != proof_hash
        or proof_authority["artifact_name"]
        != f"production-main-lock-proof-{proof_prefix}-{proof_authority['run_id']}-1"
        or str(proof_authority["run_id"]) != str(control.get("run_id"))
        or proof_authority["run_attempt"] != control.get("run_attempt")
        or main_lock_proof["mutation_intent"] != mutation_intent_authority
    ):
        common.fail("rollback main lock proof authority differs")
    target_descriptor = common.validate_target_descriptor(dict(target_descriptor))
    reviewed_route_hash = common.public_route_contract_sha256(
        target_descriptor["default_ingress"]
    )
    if route_contract_sha256 is not None and route_contract_sha256 != reviewed_route_hash:
        common.fail("rollback route contract differs from the protected ingress")
    current_kind = authorities["current_state"]["kind"]
    current_state, current_provider_state, current_phase, current_sequence = (
        _load_current_authority_state(current_kind, current_state)
    )
    if orphan_mode != (current_kind == "reconciliation-receipt"):
        common.fail("rollback inherited lock mode differs from current authority")
    _bind_rollback_lock_authority(
        current_kind, current_state, mutation_intent["lock"]
    )
    target_state = common.validate_phase_state(target_state)
    if (
        current_state["control"]["run_id"] != authorities["current_state"]["run_id"]
        or current_state["control"]["run_attempt"]
        != authorities["current_state"]["run_attempt"]
        or target_state["control"]["run_id"] != authorities["target_state"]["run_id"]
        or target_state["control"]["run_attempt"]
        != authorities["target_state"]["run_attempt"]
    ):
        common.fail("rollback artifact control authority differs")
    if common.sha256_bytes(common.canonical_file_bytes(current_state)) != common.require_sha256(current_state_sha256, "current state hash"):
        common.fail("current phase-state exact-file hash differs")
    if common.sha256_bytes(common.canonical_file_bytes(target_state)) != common.require_sha256(target_state_sha256, "target state hash"):
        common.fail("target phase-state exact-file hash differs")
    target_phase = target_state["lineage"]["phase"]
    common.validate_rollback_transition(current_phase, target_phase)
    if authorities["current_state"]["sha256"] != current_state_sha256 or authorities["target_state"]["sha256"] != target_state_sha256:
        common.fail("rollback phase-state authority differs")
    if authorities["recovery"]["sha256"] != recovery_sha256:
        common.fail("rollback recovery authority differs")
    _require_rollout_plan_authority(
        authorities, current_kind, current_state, target_state
    )
    recovery = apply_control.validate_recovery(recovery, recovery_sha256, checked)
    if (
        recovery["control"]["run_id"] != authorities["recovery"]["run_id"]
        or recovery["control"]["run_attempt"]
        != authorities["recovery"]["run_attempt"]
    ):
        common.fail("rollback recovery control authority differs")
    client = apply_control.ProductionAppClient(target_descriptor["app_id"], token, opener=opener)
    try:
        before, before_spec, _ = apply_control.observe_stable(client)
        if before != current_provider_state:
            common.fail("live production differs from the current signed phase state")
        if before["default_ingress_sha256"] != common.sha256_bytes(target_descriptor["default_ingress"].encode("utf-8")):
            common.fail("protected default ingress differs")
        desired = common.set_phase_images(before_spec, _digests(target_state))
        common.require_exact_image_change(before_spec, desired)
        if common.sha256_value(desired) != target_state["provider_state"]["canonical_spec_sha256"]:
            common.fail("rollback desired spec differs from the signed target state")
        desired_projection = apply_control._desired_projection(
            canonical_spec_sha256=common.sha256_value(desired),
            before=before,
            target_digests=_digests(target_state),
        )
        if (
            mutation_intent["operation"] != "rollback"
            or mutation_intent["before"] != before
            or mutation_intent["desired"] != desired_projection
            or mutation_intent["lineage"]["predecessor_state_sha256"]
            != current_state_sha256
            or mutation_intent["authorities"]["rollout_plan_sha256"]
            != authorities["rollout_plan_sha256"]
            or mutation_intent["authorities"]["recovery"]["sha256"]
            != recovery_sha256
            or mutation_intent["canary"]["route_contract_sha256"]
            != reviewed_route_hash
        ):
            common.fail("durable mutation intent differs from the rollback candidate")
        cas_before, cas_spec, _ = apply_control.observe_stable(client)
        if cas_before != before or cas_spec != before_spec:
            common.fail("production changed immediately before rollback")
        mutation_checked = apply_control.require_fresh_immediately_before_mutation(
            recovery=recovery,
            clock=time_source,
        )
        common.validate_mutation_intent(mutation_intent, now=mutation_checked)
        mutation_fingerprint = mutation_intent["mutation"]["mutation_fingerprint_sha256"]
        try:
            client.put_app_once(desired)
        except common.AmbiguousMutation:
            pass
        app_response, deployment_response, ambiguous = apply_control.reconcile_until_active(
            client, desired, sleeper=sleeper, poll_limit=poll_limit
        )
        after, after_spec, after_deployment = apply_control.provider_snapshot(
            app_response, deployment_response, client.app_id
        )
        final, final_spec, final_deployment = apply_control.observe_stable(client)
        if (after, after_spec, after_deployment) != (final, final_spec, final_deployment):
            common.fail("production changed during rollback final read")
        if common.extract_image_digests(after_spec) != _digests(target_state):
            common.fail("rollback image tuple differs")
        if after["environment_values_sha256"] != before["environment_values_sha256"] or after["non_source_projection_sha256"] != before["non_source_projection_sha256"]:
            common.fail("rollback changed non-image production state")
        apply_control._migration_succeeded(final_deployment)
        if sum(method == "PUT" for method, _ in client.request_log) != 1 or any(method not in {"GET", "PUT"} for method, _ in client.request_log):
            common.fail("rollback mutation ledger differs")
        completed = common.format_timestamp(
            checked if now is not None else dt.datetime.now(dt.timezone.utc)
        )
        receipt = {
            "schema_version": 1,
            "authority": ORPHAN_AUTHORITY if orphan_mode else AUTHORITY,
            "repository": common.REPOSITORY,
            "completed_at": completed,
            "control": {
                **dict(control),
                "release_policy_sha256": common.require_sha256(release_policy_sha256, "rollback policy hash"),
                "change_schema_sha256": common.require_sha256(change_schema_sha256, "rollback schema hash"),
                "controller_sha256": common.require_sha256(controller_sha256, "rollback controller hash"),
            },
            "lineage": {
                "event_sequence": current_sequence + 1,
                "phase_ordinal": common.PHASES.index(target_phase) + 1,
                "operation": "rollback",
                "from": current_phase,
                "to": target_phase,
                "predecessor_kind": current_kind,
                "predecessor_state_sha256": current_state_sha256,
                "phase": target_phase,
                "phase_source_sha": target_state["lineage"]["phase_source_sha"],
            },
            "authorities": {
                "rollout_plan_sha256": authorities["rollout_plan_sha256"],
                "current_state": dict(authorities["current_state"]),
                "target_state": dict(authorities["target_state"]),
                "recovery": dict(authorities["recovery"]),
                "mutation_intent": dict(mutation_intent_authority),
                "main_lock_proof": dict(proof_authority),
            },
            "target_authority": {
                "production_plan_sha256": target_state["evidence"]["production_plan_sha256"],
            },
            "provider_transition": {
                "http_methods_used": ["GET", "PUT"],
                "http_request_count": len(client.request_log),
                "mutation_request_count": 1,
                "endpoint_labels": ["app", "deployment"],
                "mutation_fingerprint_sha256": mutation_fingerprint,
                "ambiguous_reconciled": ambiguous,
            },
            "before": before,
            "after": after,
            "gates": {"deployment_succeeded": True, "migration_succeeded": True},
            "rollback": copy.deepcopy(target_state["rollback"]),
            "canary": {
                "required": True,
                "completed": False,
                "endpoint_labels": apply_control.ENDPOINT_LABELS,
                "route_contract_sha256": reviewed_route_hash,
            },
        }
        if orphan_mode:
            validate_orphan_rollback_receipt(receipt)
        else:
            validate_rollback_receipt(receipt)
        common.sanitize_public(receipt, private_values=tuple(target_descriptor.values()))
        return receipt
    finally:
        client.scrub()


def _add_common_inputs(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--evidence-request", required=True)
    parser.add_argument("--current-state", required=True)
    parser.add_argument("--target-state", required=True)
    parser.add_argument("--recovery", required=True)
    parser.add_argument("--release-policy-sha256", required=True)
    parser.add_argument("--change-schema-sha256", required=True)
    parser.add_argument("--mutation-intent-schema-sha256", required=True)
    parser.add_argument("--controller-sha256", required=True)
    parser.add_argument("--workflow-path", required=True, choices=(WORKFLOW_PATH, ORPHAN_WORKFLOW_PATH))
    parser.add_argument("--workflow-sha", required=True)
    parser.add_argument("--workflow-run-id", required=True)
    parser.add_argument("--workflow-run-attempt", required=True, type=int)
    parser.add_argument("--runner-temp", required=True)
    parser.add_argument("--output", required=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Roll back one signed production phase")
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare-intent")
    _add_common_inputs(prepare)
    prepare.add_argument("--lock-authority", required=True)
    prepare.add_argument("--route-contract-sha256", required=True)
    execute = commands.add_parser("execute")
    _add_common_inputs(execute)
    execute.add_argument("--target", required=True)
    execute.add_argument("--mutation-intent", required=True)
    execute.add_argument("--mutation-intent-authority", required=True)
    execute.add_argument("--main-lock-proof", required=True)
    execute.add_argument("--main-lock-proof-authority", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    request = validate_request(common.load_json(Path(args.evidence_request), "rollback evidence request"))
    current_path = Path(args.current_state)
    target_path = Path(args.target_state)
    recovery_path = Path(args.recovery)
    target_descriptor = common.loads_strict(args.target)
    control = {
        "workflow_sha": common.require_sha1(args.workflow_sha, "workflow SHA"),
        "workflow_path": args.workflow_path,
        "run_id": common.require_run_id(args.workflow_run_id, "workflow run ID"),
        "run_attempt": common.exact_int(args.workflow_run_attempt, "workflow run attempt", 1, 1),
        "runner_environment": "github-hosted",
    }
    current_value = common.load_json(current_path, "current production authority")
    target_value = common.load_json(target_path, "target phase state")
    recovery_value = common.load_json(recovery_path, "rollback recovery readiness")
    if args.command == "prepare-intent":
        intent = prepare_rollback_mutation_intent(
            control=control,
            current_state=current_value,
            current_state_sha256=common.sha256_bytes(current_path.read_bytes()),
            target_state=target_value,
            target_state_sha256=common.sha256_bytes(target_path.read_bytes()),
            recovery=recovery_value,
            recovery_sha256=common.sha256_bytes(recovery_path.read_bytes()),
            authorities=request,
            lock_authority=common.load_json(Path(args.lock_authority), "rollback lock authority"),
            release_policy_sha256=args.release_policy_sha256,
            change_schema_sha256=args.change_schema_sha256,
            mutation_intent_schema_sha256=args.mutation_intent_schema_sha256,
            controller_sha256=args.controller_sha256,
            route_contract_sha256=args.route_contract_sha256,
            now=dt.datetime.now(dt.timezone.utc),
        )
        common.write_canonical_output(Path(args.output), intent, Path(args.runner_temp))
        return 0
    token = os.environ.pop("DO_PRODUCTION_APPLY_TOKEN", "")
    intent_path = Path(args.mutation_intent)
    intent_value = common.load_json(intent_path, "rollback mutation intent")
    proof_path = Path(args.main_lock_proof)
    proof_value = common.load_json(proof_path, "rollback main lock proof")
    proof_authority = common.load_json(
        Path(args.main_lock_proof_authority), "rollback main lock proof authority"
    )
    if (
        type(intent_value) is not dict
        or type(intent_value.get("control")) is not dict
        or intent_value["control"].get("mutation_intent_schema_sha256")
        != common.require_sha256(
            args.mutation_intent_schema_sha256, "current mutation intent schema hash"
        )
    ):
        common.fail("rollback mutation intent is not bound to the current schema")
    receipt = rollback_change(
        target_descriptor=target_descriptor,
        control=control,
        token=token,
        current_state=current_value,
        current_state_sha256=common.sha256_bytes(current_path.read_bytes()),
        target_state=target_value,
        target_state_sha256=common.sha256_bytes(target_path.read_bytes()),
        recovery=recovery_value,
        recovery_sha256=common.sha256_bytes(recovery_path.read_bytes()),
        authorities=request,
        release_policy_sha256=args.release_policy_sha256,
        change_schema_sha256=args.change_schema_sha256,
        controller_sha256=args.controller_sha256,
        route_contract_sha256=None,
        mutation_intent=intent_value,
        mutation_intent_sha256=common.sha256_bytes(intent_path.read_bytes()),
        mutation_intent_authority=common.load_json(
            Path(args.mutation_intent_authority), "rollback mutation intent authority"
        ),
        main_lock_proof=proof_value,
        main_lock_proof_sha256=common.sha256_bytes(proof_path.read_bytes()),
        main_lock_proof_authority=proof_authority,
    )
    del token, target_descriptor
    common.write_canonical_output(Path(args.output), receipt, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production rollback failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
