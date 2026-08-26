#!/usr/bin/env python3
"""Build an offline, fail-closed pre-release authority for an orphan main lock.

This module has no network client and no branch/provider mutation capability.  It
only validates an immutable terminal evidence chain and emits a short-lived
authorization that a separate, token-isolated job may consume.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import sys
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import reconcile_production_orphan as reconcile_control
import rollback_production_change as rollback_control
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/finalize-production-orphan-lock.yml"
AUTHORITY = "production-orphan-lock-finalization"
STATE = "pre-unlock-authorization"
PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-lock-finalization/v1"
)
PROVENANCE_PREDICATE_TYPE = "https://slsa.dev/provenance/v1"
ACTOR_PROVENANCE = (
    "single-operator-break-glass-assertion-not-cryptographic-actor-provenance"
)
AUTHORIZATION_SCOPE = "release-main-lock-only"
MAX_AUTHORIZATION_AGE_SECONDS = 600
CANARY_WORKFLOW_PATH = ".github/workflows/verify-production-crm-canary.yml"
CANARY_PREDICATE_TYPE = "https://rereply.app/attestations/production-phase-state/v1"
INTENT_PREDICATE_TYPE = "https://rereply.app/attestations/production-mutation-intent/v2"
ASSERTION_PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-main-lock-assertion/v1"
)
RECONCILIATION_PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-reconciliation/v1"
)
ORPHAN_ROLLBACK_PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-rollback-receipt/v1"
)

CLOSURE_RECONCILIATION = "reconciliation-canary"
CLOSURE_NO_MUTATION = "no-mutation-never-started"
CLOSURE_ORPHAN_ROLLBACK = "orphan-rollback-canary"
CLOSURE_KINDS = {
    CLOSURE_RECONCILIATION,
    CLOSURE_NO_MUTATION,
    CLOSURE_ORPHAN_ROLLBACK,
}


def _normalized_binding(value: Any, label: str) -> dict[str, Any]:
    binding = common.validate_full_artifact_binding(value, label)
    return {
        "run_id": common.require_run_id(binding["run_id"], f"{label} run ID"),
        "run_attempt": binding["run_attempt"],
        "artifact_id": common.require_run_id(
            binding["artifact_id"], f"{label} artifact ID"
        ),
        "artifact_name": binding["artifact_name"],
        "artifact_digest": binding["artifact_digest"],
        "sha256": binding["sha256"],
    }


def _same_binding(left: Any, right: Any, label: str) -> bool:
    return _normalized_binding(left, label + " left") == _normalized_binding(
        right, label + " right"
    )


def validate_attested_artifact_authority(
    value: Any,
    *,
    subject: Mapping[str, Any],
    workflow_path: str,
    predicate_type: str,
    artifact_name: str,
    label: str,
) -> dict[str, Any]:
    authority = common.exact_keys(
        value,
        {
            "binding", "signer_workflow", "signer_digest", "source_digest",
            "source_ref", "runner_environment", "provenance_predicate_type",
            "policy_predicate_type", "provenance_verification_sha256",
            "policy_verification_sha256",
        },
        label,
    )
    control = subject.get("control") if type(subject) is dict else None
    if type(control) is not dict:
        common.fail(f"{label} subject control is missing")
    binding = _normalized_binding(authority["binding"], f"{label} binding")
    exact_hash = common.sha256_bytes(common.canonical_file_bytes(subject))
    expected_signer = f"{common.REPOSITORY}/{workflow_path}"
    if (
        binding["run_id"] != common.require_run_id(control.get("run_id"), f"{label} subject run")
        or binding["run_attempt"] != control.get("run_attempt")
        or binding["artifact_name"] != artifact_name
        or binding["sha256"] != exact_hash
        or authority["signer_workflow"] != expected_signer
        or authority["signer_digest"] != control.get("workflow_sha")
        or authority["source_digest"] != control.get("workflow_sha")
        or authority["source_ref"] != "refs/heads/main"
        or authority["runner_environment"] != "github-hosted"
        or authority["provenance_predicate_type"] != PROVENANCE_PREDICATE_TYPE
        or authority["policy_predicate_type"] != predicate_type
    ):
        common.fail(f"{label} attested artifact authority differs")
    common.require_sha1(authority["signer_digest"], f"{label} signer digest")
    common.require_sha1(authority["source_digest"], f"{label} source digest")
    common.require_sha256(
        authority["provenance_verification_sha256"],
        f"{label} provenance verification hash",
    )
    common.require_sha256(
        authority["policy_verification_sha256"],
        f"{label} policy verification hash",
    )
    normalized = {**dict(authority), "binding": binding}
    common.sanitize_public(normalized)
    return normalized


def _control(
    *,
    workflow_sha: Any,
    workflow_run_id: Any,
    workflow_run_attempt: Any,
    policy_sha256: str,
    change_schema_sha256: str,
    intent_schema_sha256: str,
    reconciliation_schema_sha256: str,
    finalization_schema_sha256: str,
    controller_sha256: str,
) -> dict[str, Any]:
    control = {
        "workflow_sha": common.require_sha1(workflow_sha, "finalizer workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(workflow_run_id, "finalizer run ID"),
        "run_attempt": common.exact_int(
            workflow_run_attempt, "finalizer run attempt", 1, 1
        ),
        "runner_environment": "github-hosted",
        "release_policy_sha256": common.require_sha256(
            policy_sha256, "finalizer policy hash"
        ),
        "change_schema_sha256": common.require_sha256(
            change_schema_sha256, "finalizer change schema hash"
        ),
        "mutation_intent_schema_sha256": common.require_sha256(
            intent_schema_sha256, "finalizer intent schema hash"
        ),
        "reconciliation_schema_sha256": common.require_sha256(
            reconciliation_schema_sha256, "finalizer reconciliation schema hash"
        ),
        "finalization_schema_sha256": common.require_sha256(
            finalization_schema_sha256, "finalizer schema hash"
        ),
        "controller_sha256": common.require_sha256(
            controller_sha256, "finalizer controller hash"
        ),
    }
    return control


def _operation_slug(intent: Mapping[str, Any]) -> str:
    if intent["operation"] == "activate":
        return "apply"
    return "orphan-rollback" if intent["lock"]["strategy"] == "inherit" else "rollback"


def _assert_unambiguous_root_lock(intent: Mapping[str, Any], current_main: str) -> None:
    lock = intent["lock"]
    owner_run = common.require_run_id(lock["owner_run_id"], "root lock owner run")
    root = _normalized_binding(lock["root_acquire_intent"], "root acquire intent")
    expected_name = f"production-main-lock-{lock['owner_operation']}-{owner_run}-1"
    if (
        root["run_id"] != owner_run
        or root["run_attempt"] != lock["owner_run_attempt"]
        or root["artifact_name"] != expected_name
        or lock["owner_control_sha"] != current_main
        or lock["rule_identity_sha256"]
        != common.sha256_bytes(lock["rule_id"].encode("utf-8"))
    ):
        common.fail("root main-lock ownership is ambiguous")


def _assert_reconciliation_chain(
    *,
    intent: Mapping[str, Any],
    intent_authority: Mapping[str, Any],
    assertion: Mapping[str, Any],
    assertion_authority: Mapping[str, Any],
    receipt: Mapping[str, Any],
    control: Mapping[str, Any],
) -> None:
    intent_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
    if receipt["intent"]["schema_version"] != 2:
        common.fail("legacy mutation intent cannot finalize an orphan lock")
    if (
        receipt["control"]["workflow_sha"] != control["workflow_sha"]
        or receipt["control"]["release_policy_sha256"]
        != control["release_policy_sha256"]
        or receipt["control"]["change_schema_sha256"]
        != control["change_schema_sha256"]
        or receipt["control"]["mutation_intent_schema_sha256"]
        != control["mutation_intent_schema_sha256"]
        or receipt["control"]["reconciliation_schema_sha256"]
        != control["reconciliation_schema_sha256"]
        or receipt["intent"]["operation"] != intent["operation"]
        or receipt["intent"]["workflow_path"] != intent["control"]["workflow_path"]
        or receipt["intent"]["lock"] != intent["lock"]
        or receipt["lineage"] != intent["lineage"]
        or receipt["authorities"]["upstream"] != intent["authorities"]
        or receipt["before"] != intent["before"]
        or receipt["desired"] != intent["desired"]
        or receipt["rollback"] != intent["rollback"]
        or receipt["canary"]["route_contract_sha256"]
        != intent["canary"]["route_contract_sha256"]
        or not _same_binding(
            receipt["intent"]["binding"], intent_authority["binding"],
            "reconciliation mutation intent",
        )
        or intent_authority["binding"]["sha256"] != intent_hash
    ):
        common.fail("reconciliation is unrelated to the exact v2 mutation intent")
    projected_assertion = {
        key: copy.deepcopy(assertion[key])
        for key in (
            "authority", "actor_provenance", "original_workflow_path",
            "original_control_sha", "original_run_id", "original_run_attempt",
            "rule_id", "rule_identity_sha256", "current_main_sha",
            "mutation_intent_sha256", "typed_confirmation_sha256",
            "original_provider_job",
        )
    }
    projected_assertion["binding"] = copy.deepcopy(assertion_authority["binding"])
    embedded_assertion = copy.deepcopy(receipt["lock_assertion"])
    if not _same_binding(
        embedded_assertion.pop("binding"), assertion_authority["binding"],
        "reconciliation lock assertion",
    ) or embedded_assertion != {key: value for key, value in projected_assertion.items() if key != "binding"}:
        common.fail("reconciliation lock assertion is cross-spliced")
    if (
        assertion["control"]["workflow_sha"] != control["workflow_sha"]
        or assertion["control"]["run_id"] != receipt["control"]["run_id"]
        or assertion["control"]["run_attempt"] != receipt["control"]["run_attempt"]
        or assertion["current_main_sha"] != control["workflow_sha"]
        or assertion["original_control_sha"] != intent["control"]["workflow_sha"]
        or str(assertion["original_run_id"]) != str(intent["control"]["run_id"])
        or assertion["original_run_attempt"] != intent["control"]["run_attempt"]
        or assertion["mutation_intent_sha256"] != intent_hash
        or assertion["rule_id"] != intent["lock"]["rule_id"]
    ):
        common.fail("reconciliation original lock authority differs")
    _assert_unambiguous_root_lock(intent, control["workflow_sha"])


def _assert_phase_state_chain(
    state: Mapping[str, Any],
    *,
    receipt: Mapping[str, Any],
    receipt_hash: str,
    receipt_kind: str,
    control: Mapping[str, Any],
) -> None:
    lineage = state["lineage"]
    receipt_lineage = receipt["lineage"]
    for key in (
        "event_sequence", "phase_ordinal", "operation", "from", "to", "phase",
        "phase_source_sha",
    ):
        if lineage[key] != receipt_lineage[key]:
            common.fail("canary phase state lineage differs from its change receipt")
    if (
        lineage["predecessor_kind"] != receipt_kind
        or lineage["predecessor_state_sha256"] != receipt_hash
        or state["evidence"]["change_receipt_sha256"] != receipt_hash
        or state["provider_state"] != receipt["after"]
        or state["control"]["workflow_sha"] != control["workflow_sha"]
        or state["control"]["release_policy_sha256"]
        != control["release_policy_sha256"]
        or state["control"]["change_schema_sha256"]
        != control["change_schema_sha256"]
        or common.require_timestamp(state["completed_at"], "phase state completion")
        < common.require_timestamp(receipt["completed_at"], "change completion")
    ):
        common.fail("canary phase state is unrelated to its exact change receipt")
    if receipt_kind == "reconciliation-receipt":
        upstream = receipt["authorities"]["upstream"]
        expected_rollout = upstream["rollout_plan_sha256"]
        expected_plan = (
            upstream["production_plan"]["sha256"]
            if receipt["lineage"]["operation"] == "activate"
            else upstream["target_authority"]["production_plan_sha256"]
        )
        expected_recovery = upstream["recovery"]["sha256"]
    else:
        expected_rollout = receipt["authorities"]["rollout_plan_sha256"]
        expected_plan = receipt["target_authority"]["production_plan_sha256"]
        expected_recovery = receipt["authorities"]["recovery"]["sha256"]
    if (
        state["evidence"]["rollout_plan_sha256"] != expected_rollout
        or state["evidence"]["production_plan_sha256"] != expected_plan
        or state["evidence"]["recovery_sha256"] != expected_recovery
    ):
        common.fail("canary phase state upstream authority differs")


def _assert_orphan_rollback_chain(
    *,
    reconciliation: Mapping[str, Any],
    reconciliation_authority: Mapping[str, Any],
    original_intent: Mapping[str, Any],
    orphan_intent: Mapping[str, Any],
    orphan_intent_authority: Mapping[str, Any],
    orphan_receipt: Mapping[str, Any],
    control: Mapping[str, Any],
) -> None:
    reconciliation_hash = common.sha256_bytes(common.canonical_file_bytes(reconciliation))
    original_lock = original_intent["lock"]
    inherited = orphan_intent["lock"]
    expected_owner_intent = (
        reconciliation["intent"]["binding"]["sha256"]
        if original_lock["strategy"] == "acquire"
        else original_lock["owner_intent_sha256"]
    )
    for key in (
        "branch", "rule_id", "rule_identity_sha256", "expected_post_lock",
        "root_acquire_intent", "owner_operation", "owner_run_id",
        "owner_run_attempt", "owner_control_sha",
    ):
        if inherited[key] != original_lock[key]:
            common.fail("orphan rollback inherited lock root differs")
    if (
        inherited["mode"] != "planned"
        or inherited["strategy"] != "inherit"
        or inherited["expected_pre_lock"] != {
            "lock_branch": True,
            "is_admin_enforced": True,
            "lock_allows_fetch_and_merge": False,
        }
        or inherited["owner_intent_sha256"] != expected_owner_intent
        or orphan_intent["control"]["workflow_path"]
        != rollback_control.ORPHAN_WORKFLOW_PATH
        or orphan_intent["control"]["workflow_sha"] != control["workflow_sha"]
        or orphan_intent["lineage"]["predecessor_kind"]
        != "reconciliation-receipt"
        or orphan_intent["lineage"]["predecessor_state_sha256"]
        != reconciliation_hash
        or orphan_intent["before"] != reconciliation["after"]
        or not _same_binding(
            _current_binding_as_full(
                orphan_intent["authorities"]["current_state"],
                reconciliation_authority["binding"]["artifact_name"],
                "orphan intent current reconciliation",
            ),
            reconciliation_authority["binding"],
            "orphan current reconciliation",
        )
    ):
        common.fail("orphan rollback intent does not descend from reconciliation")
    orphan_hash = common.sha256_bytes(common.canonical_file_bytes(orphan_intent))
    if (
        orphan_receipt["control"]["workflow_sha"] != control["workflow_sha"]
        or orphan_receipt["lineage"] != orphan_intent["lineage"]
        or orphan_receipt["before"] != orphan_intent["before"]
        or orphan_receipt["canary"]["route_contract_sha256"]
        != reconciliation["canary"]["route_contract_sha256"]
        or not _same_binding(
            orphan_receipt["authorities"]["mutation_intent"],
            orphan_intent_authority["binding"], "orphan rollback intent receipt",
        )
        or orphan_intent_authority["binding"]["sha256"] != orphan_hash
        or not _same_binding(
            _current_binding_as_full(
                orphan_receipt["authorities"]["current_state"],
                reconciliation_authority["binding"]["artifact_name"],
                "orphan receipt current reconciliation",
            ),
            reconciliation_authority["binding"],
            "orphan rollback receipt predecessor",
        )
    ):
        common.fail("orphan rollback receipt is unrelated")
    _assert_unambiguous_root_lock(orphan_intent, control["workflow_sha"])


def _current_binding_as_full(value: Any, artifact_name: str, label: str) -> dict[str, Any]:
    without_name = {
        "kind", "run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"
    }
    with_name = without_name | {"artifact_name"}
    if type(value) is not dict or frozenset(value) not in {
        frozenset(without_name), frozenset(with_name)
    }:
        common.fail(f"{label} keys differ")
    current = value
    if current["kind"] != "reconciliation-receipt":
        common.fail(f"{label} kind differs")
    if "artifact_name" in current and current["artifact_name"] != artifact_name:
        common.fail(f"{label} artifact name differs")
    return {
        "run_id": current["run_id"],
        "run_attempt": current["run_attempt"],
        "artifact_id": current["artifact_id"],
        "artifact_name": artifact_name,
        "artifact_digest": current["artifact_digest"],
        "sha256": current["sha256"],
    }


def _closure_inputs(
    *,
    outcome: str,
    orphan_intent: Mapping[str, Any] | None,
    orphan_receipt: Mapping[str, Any] | None,
    phase_state: Mapping[str, Any] | None,
) -> str:
    orphan_present = orphan_intent is not None or orphan_receipt is not None
    if outcome == "no-mutation":
        if orphan_present or phase_state is not None:
            common.fail("no-mutation closure forbids rollback and phase-state evidence")
        return CLOSURE_NO_MUTATION
    if outcome not in {"committed", "already-receipted"}:
        common.fail("non-terminal orphan reconciliation cannot release main")
    if phase_state is None:
        common.fail("committed closure requires a canary-certified phase state")
    if orphan_present:
        if orphan_intent is None or orphan_receipt is None:
            common.fail("orphan rollback closure evidence is incomplete")
        return CLOSURE_ORPHAN_ROLLBACK
    return CLOSURE_RECONCILIATION


def validate_finalization_authorization(
    value: Any, *, now: dt.datetime | None = None
) -> dict[str, Any]:
    authorization = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "prepared_at", "expires_at",
            "state", "control", "break_glass", "orphan", "resolution",
            "authorities", "branch_action", "execution_boundary",
        },
        "production orphan lock finalization",
    )
    common.exact_int(authorization["schema_version"], "finalization schema", 1, 1)
    if (
        authorization["authority"] != AUTHORITY
        or authorization["repository"] != common.REPOSITORY
        or authorization["state"] != STATE
    ):
        common.fail("finalization identity differs")
    prepared = common.require_timestamp(
        authorization["prepared_at"], "finalization preparation"
    )
    expires = common.require_timestamp(
        authorization["expires_at"], "finalization expiry"
    )
    if expires <= prepared or (expires - prepared).total_seconds() != MAX_AUTHORIZATION_AGE_SECONDS:
        common.fail("finalization validity window differs")
    if now is not None:
        if now.tzinfo is None or now.utcoffset() is None:
            common.fail("finalization authority clock is invalid")
        checked = now.astimezone(dt.timezone.utc)
        if checked < prepared or checked >= expires:
            common.fail("finalization authority is stale or future-dated")
    control = common.exact_keys(
        authorization["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "release_policy_sha256", "change_schema_sha256",
            "mutation_intent_schema_sha256", "reconciliation_schema_sha256",
            "finalization_schema_sha256", "controller_sha256",
        },
        "finalization control",
    )
    if control["workflow_path"] != WORKFLOW_PATH or control["runner_environment"] != "github-hosted":
        common.fail("finalization control identity differs")
    common.require_sha1(control["workflow_sha"], "finalization control SHA")
    common.require_run_id(control["run_id"], "finalization control run")
    common.exact_int(control["run_attempt"], "finalization control attempt", 1, 1)
    for key in (
        "release_policy_sha256", "change_schema_sha256",
        "mutation_intent_schema_sha256", "reconciliation_schema_sha256",
        "finalization_schema_sha256", "controller_sha256",
    ):
        common.require_sha256(control[key], f"finalization {key}")
    break_glass = common.exact_keys(
        authorization["break_glass"],
        {"actor_provenance", "authorization_scope", "typed_confirmation_sha256"},
        "finalization break-glass assertion",
    )
    if (
        break_glass["actor_provenance"] != ACTOR_PROVENANCE
        or break_glass["authorization_scope"] != AUTHORIZATION_SCOPE
    ):
        common.fail("finalization actor provenance differs")
    common.require_sha256(
        break_glass["typed_confirmation_sha256"], "finalization confirmation hash"
    )
    orphan = common.exact_keys(
        authorization["orphan"],
        {
            "branch", "current_main_sha", "rule_id", "rule_identity_sha256",
            "original_operation", "original_workflow_path", "original_control_sha",
            "original_run_id", "original_run_attempt", "root_acquire_intent",
            "mutation_intent_schema_version",
        },
        "finalization orphan identity",
    )
    if (
        orphan["branch"] != "main"
        or orphan["current_main_sha"] != control["workflow_sha"]
        or orphan["mutation_intent_schema_version"] != 2
        or orphan["original_operation"] not in {"activate", "rollback"}
    ):
        common.fail("finalization orphan identity differs")
    common.require_sha1(orphan["original_control_sha"], "original control SHA")
    common.require_run_id(orphan["original_run_id"], "original run ID")
    common.exact_int(orphan["original_run_attempt"], "original run attempt", 1, 1)
    common.exact_string(orphan["rule_id"], "finalization rule ID")
    if common.sha256_bytes(orphan["rule_id"].encode("utf-8")) != common.require_sha256(
        orphan["rule_identity_sha256"], "finalization rule hash"
    ):
        common.fail("finalization rule hash differs")
    root = _normalized_binding(orphan["root_acquire_intent"], "finalization lock root")
    expected_root_names = {
        f"production-main-lock-apply-{root['run_id']}-1",
        f"production-main-lock-rollback-{root['run_id']}-1",
    }
    if root["run_attempt"] != 1 or root["artifact_name"] not in expected_root_names:
        common.fail("finalization root lock authority differs")
    resolution = common.exact_keys(
        authorization["resolution"],
        {
            "closure_kind", "outcome", "terminal", "reconciliation_sha256",
            "closure_receipt_sha256", "orphan_rollback_sha256",
            "phase_state_sha256", "provider_job_never_started", "canary_certified",
        },
        "finalization resolution",
    )
    if resolution["closure_kind"] not in CLOSURE_KINDS or resolution["terminal"] is not True:
        common.fail("finalization resolution is not terminal")
    for key in ("reconciliation_sha256", "closure_receipt_sha256"):
        common.require_sha256(resolution[key], f"finalization {key}")
    authorities = common.exact_keys(
        authorization["authorities"],
        {
            "mutation_intent", "lock_assertion", "reconciliation",
            "orphan_rollback_intent", "orphan_rollback", "phase_state",
        },
        "finalization authorities",
    )
    def embedded(
        key: str, *, workflow_path: str, predicate: str, name: str
    ) -> dict[str, Any]:
        item = common.exact_keys(
            authorities[key],
            {
                "binding", "signer_workflow", "signer_digest", "source_digest",
                "source_ref", "runner_environment", "provenance_predicate_type",
                "policy_predicate_type", "provenance_verification_sha256",
                "policy_verification_sha256",
            },
            f"finalization {key} authority",
        )
        binding = _normalized_binding(item["binding"], f"finalization {key} binding")
        if (
            item["signer_workflow"] != f"{common.REPOSITORY}/{workflow_path}"
            or item["signer_digest"] != orphan["current_main_sha"]
            or item["source_digest"] != orphan["current_main_sha"]
            or item["source_ref"] != "refs/heads/main"
            or item["runner_environment"] != "github-hosted"
            or item["provenance_predicate_type"] != PROVENANCE_PREDICATE_TYPE
            or item["policy_predicate_type"] != predicate
            or binding["artifact_name"] != name
        ):
            common.fail(f"finalization {key} attestation differs")
        common.require_sha256(
            item["provenance_verification_sha256"],
            f"finalization {key} provenance hash",
        )
        common.require_sha256(
            item["policy_verification_sha256"],
            f"finalization {key} policy hash",
        )
        return {**dict(item), "binding": binding}
    original_run = common.require_run_id(orphan["original_run_id"], "original run")
    intent_slug = (
        "apply" if orphan["original_operation"] == "activate"
        else (
            "orphan-rollback"
            if orphan["original_workflow_path"] == rollback_control.ORPHAN_WORKFLOW_PATH
            else "rollback"
        )
    )
    intent_embedded = embedded(
        "mutation_intent",
        workflow_path=orphan["original_workflow_path"],
        predicate=INTENT_PREDICATE_TYPE,
        name=f"production-mutation-intent-{intent_slug}-{original_run}-1",
    )
    if (
        intent_embedded["binding"]["run_id"] != original_run
        or intent_embedded["binding"]["run_attempt"] != orphan["original_run_attempt"]
        or orphan["original_control_sha"] != orphan["current_main_sha"]
    ):
        common.fail("finalization original intent authority differs")
    reconciliation_binding = _normalized_binding(
        authorities["reconciliation"]["binding"], "finalization reconciliation binding"
    )
    reconciliation_run = reconciliation_binding["run_id"]
    reconciliation_embedded = embedded(
        "reconciliation",
        workflow_path=reconcile_control.WORKFLOW_PATH,
        predicate=RECONCILIATION_PREDICATE_TYPE,
        name=f"production-orphan-reconciliation-{reconciliation_run}-1",
    )
    lock_embedded = embedded(
        "lock_assertion",
        workflow_path=reconcile_control.WORKFLOW_PATH,
        predicate=ASSERTION_PREDICATE_TYPE,
        name=f"production-main-lock-assertion-{reconciliation_run}-1",
    )
    if (
        lock_embedded["binding"]["run_id"] != reconciliation_run
        or resolution["reconciliation_sha256"]
        != reconciliation_embedded["binding"]["sha256"]
        or break_glass["typed_confirmation_sha256"]
        != common.sha256_bytes(
            (
                f"FINALIZE LOCKED PRODUCTION {reconciliation_run} "
                f"{resolution['reconciliation_sha256']}"
            ).encode("utf-8")
        )
    ):
        common.fail("finalization reconciliation authority differs")
    closure = resolution["closure_kind"]
    if closure == CLOSURE_NO_MUTATION:
        if (
            resolution["outcome"] != "no-mutation"
            or resolution["provider_job_never_started"] is not True
            or resolution["canary_certified"] is not False
            or resolution["orphan_rollback_sha256"] is not None
            or resolution["phase_state_sha256"] is not None
            or authorities["orphan_rollback_intent"] is not None
            or authorities["orphan_rollback"] is not None
            or authorities["phase_state"] is not None
            or resolution["closure_receipt_sha256"] != resolution["reconciliation_sha256"]
        ):
            common.fail("no-mutation finalization semantics differ")
    else:
        if (
            resolution["outcome"] not in {"committed", "already-receipted"}
            or resolution["canary_certified"] is not True
            or type(resolution["phase_state_sha256"]) is not str
            or authorities["phase_state"] is None
        ):
            common.fail("canary finalization semantics differ")
        common.require_sha256(resolution["phase_state_sha256"], "phase state hash")
        if closure == CLOSURE_RECONCILIATION:
            if (
                resolution["orphan_rollback_sha256"] is not None
                or authorities["orphan_rollback_intent"] is not None
                or authorities["orphan_rollback"] is not None
                or resolution["closure_receipt_sha256"] != resolution["reconciliation_sha256"]
            ):
                common.fail("direct reconciliation closure differs")
        elif (
            type(resolution["orphan_rollback_sha256"]) is not str
            or authorities["orphan_rollback_intent"] is None
            or authorities["orphan_rollback"] is None
            or resolution["closure_receipt_sha256"] != resolution["orphan_rollback_sha256"]
        ):
            common.fail("orphan rollback closure differs")
    if authorities["phase_state"] is not None:
        phase_binding = _normalized_binding(
            authorities["phase_state"]["binding"], "finalization phase binding"
        )
        phase_embedded = embedded(
            "phase_state",
            workflow_path=CANARY_WORKFLOW_PATH,
            predicate=CANARY_PREDICATE_TYPE,
            name=f"production-phase-state-{phase_binding['run_id']}-1",
        )
        if phase_embedded["binding"]["sha256"] != resolution["phase_state_sha256"]:
            common.fail("finalization phase-state hash differs")
    orphan_intent_embedded: dict[str, Any] | None = None
    if authorities["orphan_rollback_intent"] is not None:
        orphan_intent_binding = _normalized_binding(
            authorities["orphan_rollback_intent"]["binding"],
            "finalization orphan intent binding",
        )
        orphan_intent_embedded = embedded(
            "orphan_rollback_intent",
            workflow_path=rollback_control.ORPHAN_WORKFLOW_PATH,
            predicate=INTENT_PREDICATE_TYPE,
            name=(
                "production-mutation-intent-orphan-rollback-"
                f"{orphan_intent_binding['run_id']}-1"
            ),
        )
    if authorities["orphan_rollback"] is not None:
        orphan_receipt_binding = _normalized_binding(
            authorities["orphan_rollback"]["binding"],
            "finalization orphan receipt binding",
        )
        orphan_embedded = embedded(
            "orphan_rollback",
            workflow_path=rollback_control.ORPHAN_WORKFLOW_PATH,
            predicate=ORPHAN_ROLLBACK_PREDICATE_TYPE,
            name=f"production-orphan-rollback-{orphan_receipt_binding['run_id']}-1",
        )
        if orphan_embedded["binding"]["sha256"] != resolution["orphan_rollback_sha256"]:
            common.fail("finalization orphan rollback hash differs")
        if (
            orphan_intent_embedded is None
            or orphan_intent_embedded["binding"]["run_id"]
            != orphan_embedded["binding"]["run_id"]
            or orphan_intent_embedded["binding"]["run_attempt"]
            != orphan_embedded["binding"]["run_attempt"]
        ):
            common.fail("finalization orphan rollback run authority differs")
    action = common.exact_keys(
        authorization["branch_action"],
        {
            "action", "expected_pre_release", "authorized_post_release",
            "branch_mutation_request_count", "release_performed",
        },
        "finalization branch action",
    )
    if action != {
        "action": "release-main-lock",
        "expected_pre_release": {
            "lock_branch": True, "is_admin_enforced": True,
            "lock_allows_fetch_and_merge": False,
        },
        "authorized_post_release": {
            "lock_branch": False, "is_admin_enforced": True,
            "lock_allows_fetch_and_merge": False,
        },
        "branch_mutation_request_count": 0,
        "release_performed": False,
    }:
        common.fail("pre-release authority improperly claims branch mutation")
    boundary = common.exact_keys(
        authorization["execution_boundary"],
        {
            "controller_network_access", "provider_network_access",
            "provider_mutation_request_count", "branch_mutation_request_count",
            "provider_token_present", "branch_admin_token_present",
            "authorization_only",
        },
        "finalization execution boundary",
    )
    if boundary != {
        "controller_network_access": False,
        "provider_network_access": False,
        "provider_mutation_request_count": 0,
        "branch_mutation_request_count": 0,
        "provider_token_present": False,
        "branch_admin_token_present": False,
        "authorization_only": True,
    }:
        common.fail("finalization execution boundary differs")
    common.sanitize_public(authorization, allowed_keys=("prepared_at",))
    return authorization


def _expected_break_glass_request(
    *,
    control: Mapping[str, Any],
    intent: Mapping[str, Any],
    reconciliation: Mapping[str, Any],
    closure_kind: str,
    closure_receipt_sha256: str,
    orphan_rollback_sha256: str | None,
    phase_state_sha256: str | None,
) -> dict[str, Any]:
    reconciliation_sha = common.sha256_bytes(
        common.canonical_file_bytes(reconciliation)
    )
    phrase = (
        f"FINALIZE LOCKED PRODUCTION {reconciliation['control']['run_id']} "
        f"{reconciliation_sha}"
    )
    return {
        "actor_provenance": ACTOR_PROVENANCE,
        "authorization_scope": AUTHORIZATION_SCOPE,
        "branch": "main",
        "current_main_sha": control["workflow_sha"],
        "control_sha": control["workflow_sha"],
        "control_run_id": control["run_id"],
        "control_run_attempt": control["run_attempt"],
        "rule_id": intent["lock"]["rule_id"],
        "rule_identity_sha256": intent["lock"]["rule_identity_sha256"],
        "original_operation": intent["operation"],
        "original_workflow_path": intent["control"]["workflow_path"],
        "original_control_sha": intent["control"]["workflow_sha"],
        "original_run_id": common.require_run_id(
            intent["control"]["run_id"], "original run ID"
        ),
        "original_run_attempt": intent["control"]["run_attempt"],
        "reconciliation_run_id": common.require_run_id(
            reconciliation["control"]["run_id"], "reconciliation run ID"
        ),
        "reconciliation_run_attempt": reconciliation["control"]["run_attempt"],
        "reconciliation_sha256": reconciliation_sha,
        "reconciliation_outcome": reconciliation["classification"]["outcome"],
        "closure_kind": closure_kind,
        "closure_receipt_sha256": closure_receipt_sha256,
        "orphan_rollback_sha256": orphan_rollback_sha256,
        "phase_state_sha256": phase_state_sha256,
        "typed_confirmation_sha256": common.sha256_bytes(phrase.encode("utf-8")),
    }


def build_finalization_authorization(
    *,
    request: Mapping[str, Any],
    control: Mapping[str, Any],
    mutation_intent: Mapping[str, Any],
    mutation_intent_authority: Mapping[str, Any],
    lock_assertion: Mapping[str, Any],
    lock_assertion_authority: Mapping[str, Any],
    reconciliation: Mapping[str, Any],
    reconciliation_authority: Mapping[str, Any],
    orphan_rollback_intent: Mapping[str, Any] | None,
    orphan_rollback_intent_authority: Mapping[str, Any] | None,
    orphan_rollback: Mapping[str, Any] | None,
    orphan_rollback_authority: Mapping[str, Any] | None,
    phase_state: Mapping[str, Any] | None,
    phase_state_authority: Mapping[str, Any] | None,
    now: dt.datetime,
) -> dict[str, Any]:
    intent = common.validate_mutation_intent(mutation_intent)
    if intent["schema_version"] != 2:
        common.fail("legacy mutation intent cannot finalize an orphan lock")
    assertion = reconcile_control.validate_lock_assertion(
        lock_assertion, intent=intent
    )
    receipt = common.validate_reconciliation_receipt(reconciliation)
    outcome = receipt["classification"]["outcome"]
    orphan_args = (
        orphan_rollback_intent,
        orphan_rollback_intent_authority,
        orphan_rollback,
        orphan_rollback_authority,
    )
    if any(value is not None for value in orphan_args) and not all(
        value is not None for value in orphan_args
    ):
        common.fail("orphan rollback authority arguments must be supplied together")
    if (phase_state is None) is not (phase_state_authority is None):
        common.fail("phase-state value and authority must be supplied together")
    validated_orphan_intent = (
        common.validate_mutation_intent(orphan_rollback_intent)
        if orphan_rollback_intent is not None else None
    )
    validated_orphan_receipt = (
        rollback_control.validate_orphan_rollback_receipt(orphan_rollback)
        if orphan_rollback is not None else None
    )
    validated_phase = (
        common.validate_phase_state(phase_state, now=now)
        if phase_state is not None else None
    )
    closure_kind = _closure_inputs(
        outcome=outcome,
        orphan_intent=validated_orphan_intent,
        orphan_receipt=validated_orphan_receipt,
        phase_state=validated_phase,
    )
    intent_slug = _operation_slug(intent)
    intent_auth = validate_attested_artifact_authority(
        mutation_intent_authority,
        subject=intent,
        workflow_path=intent["control"]["workflow_path"],
        predicate_type=INTENT_PREDICATE_TYPE,
        artifact_name=(
            f"production-mutation-intent-{intent_slug}-"
            f"{common.require_run_id(intent['control']['run_id'], 'intent run')}-1"
        ),
        label="mutation intent authority",
    )
    assertion_auth = validate_attested_artifact_authority(
        lock_assertion_authority,
        subject=assertion,
        workflow_path=reconcile_control.WORKFLOW_PATH,
        predicate_type=ASSERTION_PREDICATE_TYPE,
        artifact_name=(
            "production-main-lock-assertion-"
            f"{common.require_run_id(assertion['control']['run_id'], 'assertion run')}-1"
        ),
        label="lock assertion authority",
    )
    reconciliation_auth = validate_attested_artifact_authority(
        reconciliation_authority,
        subject=receipt,
        workflow_path=reconcile_control.WORKFLOW_PATH,
        predicate_type=RECONCILIATION_PREDICATE_TYPE,
        artifact_name=(
            "production-orphan-reconciliation-"
            f"{common.require_run_id(receipt['control']['run_id'], 'reconciliation run')}-1"
        ),
        label="reconciliation authority",
    )
    _assert_reconciliation_chain(
        intent=intent,
        intent_authority=intent_auth,
        assertion=assertion,
        assertion_authority=assertion_auth,
        receipt=receipt,
        control=control,
    )
    orphan_intent_auth: dict[str, Any] | None = None
    orphan_receipt_auth: dict[str, Any] | None = None
    closure_receipt: Mapping[str, Any] = receipt
    closure_receipt_kind = "reconciliation-receipt"
    if closure_kind == CLOSURE_ORPHAN_ROLLBACK:
        if (
            validated_orphan_intent is None
            or validated_orphan_receipt is None
            or orphan_rollback_intent_authority is None
            or orphan_rollback_authority is None
        ):
            common.fail("orphan rollback closure authority is incomplete")
        orphan_intent_auth = validate_attested_artifact_authority(
            orphan_rollback_intent_authority,
            subject=validated_orphan_intent,
            workflow_path=rollback_control.ORPHAN_WORKFLOW_PATH,
            predicate_type=INTENT_PREDICATE_TYPE,
            artifact_name=(
                "production-mutation-intent-orphan-rollback-"
                f"{common.require_run_id(validated_orphan_intent['control']['run_id'], 'orphan intent run')}-1"
            ),
            label="orphan rollback intent authority",
        )
        orphan_receipt_auth = validate_attested_artifact_authority(
            orphan_rollback_authority,
            subject=validated_orphan_receipt,
            workflow_path=rollback_control.ORPHAN_WORKFLOW_PATH,
            predicate_type=ORPHAN_ROLLBACK_PREDICATE_TYPE,
            artifact_name=(
                "production-orphan-rollback-"
                f"{common.require_run_id(validated_orphan_receipt['control']['run_id'], 'orphan receipt run')}-1"
            ),
            label="orphan rollback receipt authority",
        )
        _assert_orphan_rollback_chain(
            reconciliation=receipt,
            reconciliation_authority=reconciliation_auth,
            original_intent=intent,
            orphan_intent=validated_orphan_intent,
            orphan_intent_authority=orphan_intent_auth,
            orphan_receipt=validated_orphan_receipt,
            control=control,
        )
        closure_receipt = validated_orphan_receipt
        closure_receipt_kind = "orphan-rollback-receipt"
    closure_hash = common.sha256_bytes(common.canonical_file_bytes(closure_receipt))
    phase_auth: dict[str, Any] | None = None
    phase_hash: str | None = None
    if validated_phase is not None:
        if phase_state_authority is None:
            common.fail("phase-state attestation authority is missing")
        phase_hash = common.sha256_bytes(common.canonical_file_bytes(validated_phase))
        phase_auth = validate_attested_artifact_authority(
            phase_state_authority,
            subject=validated_phase,
            workflow_path=CANARY_WORKFLOW_PATH,
            predicate_type=CANARY_PREDICATE_TYPE,
            artifact_name=(
                "production-phase-state-"
                f"{common.require_run_id(validated_phase['control']['run_id'], 'phase run')}-1"
            ),
            label="phase state authority",
        )
        _assert_phase_state_chain(
            validated_phase,
            receipt=closure_receipt,
            receipt_hash=closure_hash,
            receipt_kind=closure_receipt_kind,
            control=control,
        )
    provider_never_started = receipt["lock_assertion"]["original_provider_job"][
        "never_started"
    ]
    if closure_kind == CLOSURE_NO_MUTATION and provider_never_started is not True:
        common.fail("no-mutation closure lacks signed never-started evidence")
    if outcome == "committed" and provider_never_started is True:
        common.fail("committed closure has ambiguous provider-job ownership")
    reconciliation_hash = reconciliation_auth["binding"]["sha256"]
    orphan_hash = (
        orphan_receipt_auth["binding"]["sha256"]
        if orphan_receipt_auth is not None else None
    )
    expected_request = _expected_break_glass_request(
        control=control,
        intent=intent,
        reconciliation=receipt,
        closure_kind=closure_kind,
        closure_receipt_sha256=closure_hash,
        orphan_rollback_sha256=orphan_hash,
        phase_state_sha256=phase_hash,
    )
    request_value = common.exact_keys(
        request, set(expected_request), "break-glass finalization request"
    )
    normalized_request = dict(request_value)
    for key in (
        "control_run_id", "original_run_id", "reconciliation_run_id"
    ):
        normalized_request[key] = common.require_run_id(
            normalized_request[key], f"break-glass {key}"
        )
    if normalized_request != expected_request:
        common.fail("break-glass finalization request differs")
    prepared = now.astimezone(dt.timezone.utc).replace(microsecond=0)
    authorization = {
        "schema_version": 1,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "prepared_at": common.format_timestamp(prepared),
        "expires_at": common.format_timestamp(
            prepared + dt.timedelta(seconds=MAX_AUTHORIZATION_AGE_SECONDS)
        ),
        "state": STATE,
        "control": dict(control),
        "break_glass": {
            "actor_provenance": ACTOR_PROVENANCE,
            "authorization_scope": AUTHORIZATION_SCOPE,
            "typed_confirmation_sha256": expected_request[
                "typed_confirmation_sha256"
            ],
        },
        "orphan": {
            "branch": "main",
            "current_main_sha": control["workflow_sha"],
            "rule_id": intent["lock"]["rule_id"],
            "rule_identity_sha256": intent["lock"]["rule_identity_sha256"],
            "original_operation": intent["operation"],
            "original_workflow_path": intent["control"]["workflow_path"],
            "original_control_sha": intent["control"]["workflow_sha"],
            "original_run_id": common.require_run_id(
                intent["control"]["run_id"], "original run"
            ),
            "original_run_attempt": intent["control"]["run_attempt"],
            "root_acquire_intent": _normalized_binding(
                intent["lock"]["root_acquire_intent"], "finalization root lock"
            ),
            "mutation_intent_schema_version": 2,
        },
        "resolution": {
            "closure_kind": closure_kind,
            "outcome": outcome,
            "terminal": True,
            "reconciliation_sha256": reconciliation_hash,
            "closure_receipt_sha256": closure_hash,
            "orphan_rollback_sha256": orphan_hash,
            "phase_state_sha256": phase_hash,
            "provider_job_never_started": provider_never_started,
            "canary_certified": validated_phase is not None,
        },
        "authorities": {
            "mutation_intent": intent_auth,
            "lock_assertion": assertion_auth,
            "reconciliation": reconciliation_auth,
            "orphan_rollback_intent": orphan_intent_auth,
            "orphan_rollback": orphan_receipt_auth,
            "phase_state": phase_auth,
        },
        "branch_action": {
            "action": "release-main-lock",
            "expected_pre_release": {
                "lock_branch": True,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "authorized_post_release": {
                "lock_branch": False,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "branch_mutation_request_count": 0,
            "release_performed": False,
        },
        "execution_boundary": {
            "controller_network_access": False,
            "provider_network_access": False,
            "provider_mutation_request_count": 0,
            "branch_mutation_request_count": 0,
            "provider_token_present": False,
            "branch_admin_token_present": False,
            "authorization_only": True,
        },
    }
    return validate_finalization_authorization(authorization, now=prepared)


def _load_optional(path: str | None, label: str) -> Any | None:
    return common.load_json(Path(path), label) if path is not None else None


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    if args.command == "validate":
        path = Path(args.authorization)
        value = common.load_json(path, "orphan finalization authorization")
        expected = common.require_sha256(args.sha256, "authorization exact-file hash")
        if common.sha256_bytes(path.read_bytes()) != expected:
            common.fail("authorization exact-file hash differs")
        validate_finalization_authorization(value)
        return 0
    paths = {
        "policy": Path(args.policy),
        "change_schema": Path(args.change_schema),
        "intent_schema": Path(args.intent_schema),
        "reconciliation_schema": Path(args.reconciliation_schema),
        "finalization_schema": Path(args.finalization_schema),
    }
    for label, path in paths.items():
        common.load_json(path, label.replace("_", " "))
    control = _control(
        workflow_sha=args.workflow_sha,
        workflow_run_id=args.workflow_run_id,
        workflow_run_attempt=args.workflow_run_attempt,
        policy_sha256=common.sha256_bytes(paths["policy"].read_bytes()),
        change_schema_sha256=common.sha256_bytes(paths["change_schema"].read_bytes()),
        intent_schema_sha256=common.sha256_bytes(paths["intent_schema"].read_bytes()),
        reconciliation_schema_sha256=common.sha256_bytes(
            paths["reconciliation_schema"].read_bytes()
        ),
        finalization_schema_sha256=common.sha256_bytes(
            paths["finalization_schema"].read_bytes()
        ),
        controller_sha256=args.controller_sha256,
    )
    authorization = build_finalization_authorization(
        request=common.load_json(Path(args.request), "break-glass request"),
        control=control,
        mutation_intent=common.load_json(
            Path(args.mutation_intent), "mutation intent"
        ),
        mutation_intent_authority=common.load_json(
            Path(args.mutation_intent_authority), "mutation intent authority"
        ),
        lock_assertion=common.load_json(
            Path(args.lock_assertion), "main-lock assertion"
        ),
        lock_assertion_authority=common.load_json(
            Path(args.lock_assertion_authority), "main-lock assertion authority"
        ),
        reconciliation=common.load_json(
            Path(args.reconciliation), "orphan reconciliation"
        ),
        reconciliation_authority=common.load_json(
            Path(args.reconciliation_authority), "orphan reconciliation authority"
        ),
        orphan_rollback_intent=_load_optional(
            args.orphan_rollback_intent, "orphan rollback intent"
        ),
        orphan_rollback_intent_authority=_load_optional(
            args.orphan_rollback_intent_authority, "orphan rollback intent authority"
        ),
        orphan_rollback=_load_optional(
            args.orphan_rollback, "orphan rollback receipt"
        ),
        orphan_rollback_authority=_load_optional(
            args.orphan_rollback_authority, "orphan rollback receipt authority"
        ),
        phase_state=_load_optional(args.phase_state, "phase state"),
        phase_state_authority=_load_optional(
            args.phase_state_authority, "phase state authority"
        ),
        now=dt.datetime.now(dt.timezone.utc),
    )
    common.write_canonical_output(
        Path(args.output), authorization, Path(args.runner_temp)
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Offline production orphan main-lock finalization authority"
    )
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare")
    for name in (
        "request", "mutation-intent", "mutation-intent-authority",
        "lock-assertion", "lock-assertion-authority", "reconciliation",
        "reconciliation-authority", "policy", "change-schema", "intent-schema",
        "reconciliation-schema", "finalization-schema", "controller-sha256",
        "workflow-sha", "workflow-run-id", "runner-temp", "output",
    ):
        prepare.add_argument(f"--{name}", required=True)
    prepare.add_argument("--workflow-run-attempt", required=True, type=int)
    for name in (
        "orphan-rollback-intent", "orphan-rollback-intent-authority",
        "orphan-rollback", "orphan-rollback-authority", "phase-state",
        "phase-state-authority",
    ):
        prepare.add_argument(f"--{name}")
    validate = commands.add_parser("validate")
    validate.add_argument("--authorization", required=True)
    validate.add_argument("--sha256", required=True)
    return parser


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production orphan lock finalization failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
