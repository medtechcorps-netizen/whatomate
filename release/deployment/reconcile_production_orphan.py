#!/usr/bin/env python3
"""Classify an orphaned production mutation using GETs only.

This controller has deliberately no provider mutation or branch-unlock method.  It
turns a durable v2 mutation intent, a signed single-operator lock assertion, and
two exact live observations into content-free evidence.  Any later unlock is a
different, separately attested workflow.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import apply_production_change as apply_control
import rollback_production_change as rollback_control
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/reconcile-production-orphan.yml"
AUTHORITY = "production-orphan-reconciliation-receipt"
ASSERTION_AUTHORITY = "production-main-lock-ownership-assertion"
ACTOR_PROVENANCE = "single-operator-assertion-not-audit-log"
MAX_ASSERTION_AGE_SECONDS = 900


class ReadOnlyProductionClient:
    """Exact Apps API GET allowlist; no mutation method exists in this class."""

    def __init__(self, app_id: str, token: str, *, opener: Any | None = None) -> None:
        self.app_id = common.require_uuid(app_id, "reconciliation app identity")
        if type(token) is not str or len(token) < 20 or any(ch in token for ch in "\r\n\x00"):
            common.fail("production reconciliation read token is invalid")
        self._token = token
        self._opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), common.RejectRedirects()
        )
        self.request_log: list[tuple[str, str]] = []

    @property
    def app_path(self) -> str:
        return f"/v2/apps/{self.app_id}"

    def deployment_path(self, deployment_id: str) -> str:
        return (
            f"/v2/apps/{self.app_id}/deployments/"
            f"{common.require_uuid(deployment_id, 'reconciliation deployment identity')}"
        )

    def _get(self, path: str, label: str) -> Any:
        if path != self.app_path and not apply_control.re_full_deployment_path(path, self.app_id):
            common.fail("reconciliation provider path is outside the GET allowlist")
        url = common.API_ORIGIN + path
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "https"
            or parsed.hostname != "api.digitalocean.com"
            or parsed.port not in (None, 443)
            or parsed.username is not None
            or parsed.password is not None
            or parsed.query
            or parsed.fragment
        ):
            common.fail("reconciliation provider URL differs")
        request = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._token}",
                "User-Agent": "rereply-production-orphan-reconcile/1",
            },
        )
        try:
            with self._opener.open(request, timeout=20) as response:
                if response.geturl() != url or apply_control._response_status(response) != 200:
                    common.fail("reconciliation provider response differs")
                content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
                if content_type != "application/json":
                    common.fail("reconciliation provider content type differs")
                raw = response.read(common.MAX_JSON_BYTES + 1)
                if not raw or len(raw) > common.MAX_JSON_BYTES:
                    common.fail("reconciliation provider response size differs")
                value = common.loads_strict(raw)
        except common.ReleaseError:
            raise
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError) as exc:
            raise common.ReleaseError("reconciliation provider GET failed") from exc
        self.request_log.append(("GET", label))
        return value

    def get_app(self) -> Any:
        return self._get(self.app_path, "app")

    def get_deployment(self, deployment_id: str) -> Any:
        return self._get(self.deployment_path(deployment_id), "deployment")

    def scrub(self) -> None:
        self._token = ""


def _active_observation(
    app_response: Any, deployment_response: Any, app_id: str
) -> dict[str, Any]:
    app = apply_control._app_object(app_response)
    deployment = apply_control._deployment_object(deployment_response)
    if common.require_uuid(app.get("id"), "reconciliation app identity") != app_id:
        common.fail("reconciliation app identity differs")
    active = app.get("active_deployment")
    if type(active) is not dict or active.get("phase") != "ACTIVE":
        common.fail("reconciliation active deployment is unavailable")
    active_id = common.require_uuid(active.get("id"), "reconciliation active deployment identity")
    if (
        common.require_uuid(deployment.get("id"), "reconciliation deployment identity") != active_id
        or deployment.get("phase") != "ACTIVE"
    ):
        common.fail("reconciliation deployment response is not the active deployment")
    active_spec = deployment.get("spec")
    live_spec = app.get("spec")
    if type(active_spec) is not dict or type(live_spec) is not dict:
        common.fail("reconciliation provider spec is malformed")
    ingress = common.exact_string(app.get("default_ingress"), "reconciliation ingress")
    updated_at = common.exact_string(app.get("updated_at"), "reconciliation app update time")
    mode = common.source_mode(active_spec)
    images = (
        common.sanitized_image_records(common.extract_image_digests(active_spec))
        if mode == "digest-images"
        else []
    )
    active_spec_sha256 = common.sha256_value(active_spec)
    app_spec_sha256 = common.sha256_value(live_spec)
    if app_spec_sha256 != active_spec_sha256:
        common.fail("reconciliation app spec differs from the active deployment spec")
    public = {
        "app_identity_sha256": common.sha256_bytes(app_id.encode("utf-8")),
        "default_ingress_sha256": common.sha256_bytes(ingress.encode("utf-8")),
        "app_updated_at_sha256": common.sha256_bytes(updated_at.encode("utf-8")),
        "active_deployment_identity_sha256": common.sha256_bytes(active_id.encode("utf-8")),
        "canonical_spec_sha256": active_spec_sha256,
        "environment_values_sha256": common.environment_value_fingerprint(active_spec),
        "non_source_projection_sha256": common.non_source_fingerprint(active_spec),
        "source_mode": mode,
        "images": images,
    }
    common._validate_public_provider_state(public, "reconciliation active state", allow_legacy=True)
    transitions: list[dict[str, Any]] = []
    for key in ("in_progress_deployment", "pending_deployment", "pinned_deployment"):
        item = app.get(key)
        if item is None:
            continue
        if type(item) is not dict:
            common.fail("reconciliation transition is malformed")
        candidate_spec = item.get("spec")
        transitions.append(
            {
                "kind": key,
                "identity_sha256": common.sha256_bytes(
                    common.require_uuid(item.get("id"), "transition identity").encode("utf-8")
                ),
                "phase": common.exact_string(item.get("phase"), "transition phase"),
                "canonical_spec_sha256": (
                    common.sha256_value(candidate_spec) if type(candidate_spec) is dict else None
                ),
            }
        )
    migration_succeeded = False
    try:
        migration_succeeded = apply_control._migration_succeeded(deployment)
    except common.ReleaseError:
        migration_succeeded = False
    return {
        "public": public,
        "app_spec_sha256": app_spec_sha256,
        "transitions": transitions,
        "migration_succeeded": migration_succeeded,
    }


def observe_twice(client: ReadOnlyProductionClient) -> dict[str, Any]:
    rounds: list[dict[str, Any]] = []
    for _ in range(2):
        app_response = client.get_app()
        app = apply_control._app_object(app_response)
        active = app.get("active_deployment")
        if type(active) is not dict:
            common.fail("reconciliation active deployment is missing")
        deployment_response = client.get_deployment(
            common.require_uuid(active.get("id"), "reconciliation active deployment identity")
        )
        rounds.append(_active_observation(app_response, deployment_response, client.app_id))
    if rounds[0] != rounds[1]:
        common.fail("production changed between reconciliation observations")
    if client.request_log != [
        ("GET", "app"), ("GET", "deployment"),
        ("GET", "app"), ("GET", "deployment"),
    ]:
        common.fail("reconciliation provider request ledger differs")
    return rounds[0]


def validate_lock_assertion(
    value: Any,
    *,
    intent: Mapping[str, Any],
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    assertion = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "created_at", "control",
            "actor_provenance", "original_workflow_path", "original_control_sha",
            "original_run_id", "original_run_attempt", "rule_id",
            "rule_identity_sha256", "current_main_sha", "mutation_intent_sha256",
            "typed_confirmation_sha256", "original_provider_job",
        },
        "production main lock assertion",
    )
    common.exact_int(assertion["schema_version"], "lock assertion schema", 1, 1)
    if (
        assertion["authority"] != ASSERTION_AUTHORITY
        or assertion["repository"] != common.REPOSITORY
        or assertion["actor_provenance"] != ACTOR_PROVENANCE
    ):
        common.fail("lock assertion authority differs")
    created = common.require_timestamp(assertion["created_at"], "lock assertion creation time")
    if now is not None:
        checked = now.astimezone(dt.timezone.utc).replace(microsecond=0)
        if created > checked or (checked - created).total_seconds() > MAX_ASSERTION_AGE_SECONDS:
            common.fail("lock assertion is stale or future-dated")
    control = common.exact_keys(
        assertion["control"], {"workflow_sha", "workflow_path", "run_id", "run_attempt"},
        "lock assertion control",
    )
    common.require_sha1(control["workflow_sha"], "lock assertion workflow SHA")
    if control["workflow_path"] != WORKFLOW_PATH:
        common.fail("lock assertion workflow differs")
    common.require_run_id(control["run_id"], "lock assertion run ID")
    common.exact_int(control["run_attempt"], "lock assertion run attempt", 1, 1)
    if (
        assertion["original_workflow_path"] != intent["control"]["workflow_path"]
        or assertion["original_control_sha"] != intent["control"]["workflow_sha"]
        or str(assertion["original_run_id"]) != str(intent["control"]["run_id"])
        or assertion["original_run_attempt"] != intent["control"]["run_attempt"]
        or assertion["rule_id"] != intent["lock"]["rule_id"]
        or assertion["rule_identity_sha256"] != intent["lock"]["rule_identity_sha256"]
        or assertion["current_main_sha"] != control["workflow_sha"]
        or assertion["mutation_intent_sha256"]
        != common.sha256_bytes(common.canonical_file_bytes(intent))
    ):
        common.fail("lock assertion does not bind the exact orphan authority")
    common.require_sha1(assertion["original_control_sha"], "original control SHA")
    common.require_run_id(assertion["original_run_id"], "original run ID")
    common.exact_int(assertion["original_run_attempt"], "original run attempt", 1, 1)
    common.exact_string(assertion["rule_id"], "lock assertion rule ID")
    if common.sha256_bytes(assertion["rule_id"].encode("utf-8")) != common.require_sha256(
        assertion["rule_identity_sha256"], "lock assertion rule hash"
    ):
        common.fail("lock assertion rule hash differs")
    common.require_sha1(assertion["current_main_sha"], "lock assertion current main SHA")
    common.require_sha256(assertion["mutation_intent_sha256"], "lock assertion intent hash")
    common.require_sha256(assertion["typed_confirmation_sha256"], "typed confirmation hash")
    common.validate_original_provider_job(
        assertion["original_provider_job"],
        workflow_path=assertion["original_workflow_path"],
    )
    common.sanitize_public(assertion, allowed_keys=("created_at",))
    return assertion


def build_lock_assertion(
    *,
    request: Mapping[str, Any],
    intent: Mapping[str, Any],
    intent_sha256: str,
    control: Mapping[str, Any],
    now: dt.datetime,
) -> dict[str, Any]:
    intent = common.validate_mutation_intent(intent)
    if intent["schema_version"] != 2:
        common.fail("legacy mutation evidence cannot use the automatic lock assertion lane")
    expected_hash = common.require_sha256(intent_sha256, "asserted mutation intent hash")
    if common.sha256_bytes(common.canonical_file_bytes(intent)) != expected_hash:
        common.fail("asserted mutation intent exact-file hash differs")
    request = common.exact_keys(
        request,
        {
            "original_workflow_path", "original_control_sha", "original_run_id",
            "original_run_attempt", "rule_id", "current_main_sha",
            "typed_confirmation", "original_provider_job",
        },
        "lock assertion request",
    )
    expected_phrase = f"RECONCILE LOCKED PRODUCTION {intent['control']['run_id']} {expected_hash}"
    if request["typed_confirmation"] != expected_phrase:
        common.fail("lock assertion typed confirmation differs")
    if (
        request["original_workflow_path"] != intent["control"]["workflow_path"]
        or request["original_control_sha"] != intent["control"]["workflow_sha"]
        or str(request["original_run_id"]) != str(intent["control"]["run_id"])
        or request["original_run_attempt"] != intent["control"]["run_attempt"]
        or request["rule_id"] != intent["lock"]["rule_id"]
        or request["current_main_sha"] != control["workflow_sha"]
    ):
        common.fail("lock assertion request differs from the v2 intent")
    assertion = {
        "schema_version": 1,
        "authority": ASSERTION_AUTHORITY,
        "repository": common.REPOSITORY,
        "created_at": common.format_timestamp(now),
        "control": dict(control),
        "actor_provenance": ACTOR_PROVENANCE,
        "original_workflow_path": request["original_workflow_path"],
        "original_control_sha": request["original_control_sha"],
        "original_run_id": common.require_run_id(request["original_run_id"], "original run ID"),
        "original_run_attempt": common.exact_int(request["original_run_attempt"], "original attempt", 1, 1),
        "rule_id": common.exact_string(request["rule_id"], "lock assertion rule ID"),
        "rule_identity_sha256": common.sha256_bytes(request["rule_id"].encode("utf-8")),
        "current_main_sha": common.require_sha1(request["current_main_sha"], "current main SHA"),
        "mutation_intent_sha256": expected_hash,
        "typed_confirmation_sha256": common.sha256_bytes(
            request["typed_confirmation"].encode("utf-8")
        ),
        "original_provider_job": common.project_original_provider_job(
            request["original_provider_job"],
            workflow_path=request["original_workflow_path"],
        ),
    }
    return validate_lock_assertion(assertion, intent=intent, now=now)


def _desired_matches(after: Mapping[str, Any], desired: Mapping[str, Any], before: Mapping[str, Any]) -> bool:
    return (
        after["canonical_spec_sha256"] == desired["canonical_spec_sha256"]
        and after["environment_values_sha256"] == desired["environment_values_sha256"]
        and after["non_source_projection_sha256"] == desired["non_source_projection_sha256"]
        and after["source_mode"] == "digest-images"
        and after["images"] == desired["images"]
        and after["app_identity_sha256"] == before["app_identity_sha256"]
        and after["default_ingress_sha256"] == before["default_ingress_sha256"]
    )


def _validate_original_receipt(
    value: Mapping[str, Any] | None,
    binding: Mapping[str, Any] | None,
    intent: Mapping[str, Any],
    intent_sha256: str,
) -> tuple[dict[str, Any] | None, dict[str, Any] | None]:
    if value is None or binding is None:
        if value is not None or binding is not None:
            common.fail("original receipt value and authority must be supplied together")
        return None, None
    authority = value.get("authority") if type(value) is dict else None
    if authority == "production-phase-apply-receipt":
        receipt = common.validate_apply_receipt(value)
        kind = "apply"
        path = ".github/workflows/apply-production-phase.yml"
        prefix = "production-phase-apply"
    elif authority == "production-phase-rollback-receipt":
        receipt = rollback_control.validate_rollback_receipt(value)
        kind = "rollback"
        path = rollback_control.WORKFLOW_PATH
        prefix = "production-phase-rollback"
    elif authority == "production-orphan-rollback-receipt":
        receipt = rollback_control.validate_orphan_rollback_receipt(value)
        kind = "orphan-rollback"
        path = rollback_control.ORPHAN_WORKFLOW_PATH
        prefix = "production-orphan-rollback"
    else:
        common.fail("original receipt authority is not allowlisted")
    binding = common.validate_full_artifact_binding(binding, "original signed receipt")
    if (
        binding["artifact_name"] != f"{prefix}-{binding['run_id']}-1"
        or binding["sha256"] != common.sha256_bytes(common.canonical_file_bytes(receipt))
        or receipt["control"]["workflow_path"] != path
        or str(receipt["control"]["run_id"]) != str(binding["run_id"])
        or receipt["control"]["run_attempt"] != binding["run_attempt"]
        or receipt["lineage"] != intent["lineage"]
        or receipt["before"] != intent["before"]
        or receipt["provider_transition"]["mutation_fingerprint_sha256"]
        != intent["mutation"]["mutation_fingerprint_sha256"]
        or receipt["authorities"]["mutation_intent"]["sha256"] != intent_sha256
    ):
        common.fail("original signed receipt differs from the v2 mutation intent")
    return receipt, {"kind": kind, "workflow_path": path, "binding": binding}


def build_reconciliation_receipt(
    *,
    control: Mapping[str, Any],
    intent: Mapping[str, Any],
    intent_binding: Mapping[str, Any],
    lock_assertion: Mapping[str, Any],
    lock_assertion_binding: Mapping[str, Any],
    observed: Mapping[str, Any],
    request_log: Sequence[tuple[str, str]],
    original_receipt: Mapping[str, Any] | None,
    original_receipt_binding: Mapping[str, Any] | None,
    completed_at: dt.datetime,
) -> dict[str, Any]:
    intent = common.validate_mutation_intent(intent)
    intent_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
    intent_binding = common.validate_full_artifact_binding(intent_binding, "reconciled mutation intent")
    operation_name = (
        "apply" if intent["operation"] == "activate"
        else (
            "orphan-rollback"
            if intent["lock"]["strategy"] == "inherit"
            else "rollback"
        )
    )
    if (
        intent_binding["sha256"] != intent_hash
        or intent_binding["artifact_name"]
        != f"production-mutation-intent-{operation_name}-{intent_binding['run_id']}-1"
    ):
        common.fail("reconciled mutation intent binding differs")
    lock_assertion = validate_lock_assertion(
        lock_assertion, intent=intent, now=completed_at
    )
    lock_assertion_binding = common.validate_full_artifact_binding(
        lock_assertion_binding, "reconciliation lock assertion"
    )
    if (
        lock_assertion_binding["sha256"] != common.sha256_bytes(
            common.canonical_file_bytes(lock_assertion)
        )
        or lock_assertion_binding["artifact_name"]
        != f"production-main-lock-assertion-{lock_assertion_binding['run_id']}-1"
    ):
        common.fail("lock assertion artifact binding differs")
    original, original_authority = _validate_original_receipt(
        original_receipt, original_receipt_binding, intent, intent_hash
    )
    after = copy.deepcopy(observed["public"])
    app_spec_sha256 = common.require_sha256(
        observed.get("app_spec_sha256"), "reconciliation app spec hash"
    )
    if app_spec_sha256 != after["canonical_spec_sha256"]:
        common.fail("reconciliation app spec differs from the active deployment spec")
    transitions = observed["transitions"]
    transition_absent = transitions == []
    desired_match = _desired_matches(after, intent["desired"], intent["before"])
    transition_targets_desired = bool(transitions) and all(
        item["canonical_spec_sha256"] == intent["desired"]["canonical_spec_sha256"]
        for item in transitions
    )
    if desired_match and transition_absent and observed["migration_succeeded"]:
        outcome = "already-receipted" if original is not None else "committed"
    elif (
        common.provider_states_share_semantic_lineage(
            after,
            intent["before"],
            allow_legacy=intent["operation"] == "activate",
        )
        and transition_absent
        and original is None
        and lock_assertion["original_provider_job"]["never_started"] is True
    ):
        outcome = "no-mutation"
    elif transition_targets_desired and original is None:
        outcome = "pending"
    else:
        outcome = "indeterminate"
    semantics = copy.deepcopy(common.RECONCILIATION_OUTCOMES[outcome])
    semantics.setdefault("original_receipt_present", original is not None)
    should_succeed = outcome in {"committed", "already-receipted"}
    receipt = {
        "schema_version": 1,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "completed_at": common.format_timestamp(completed_at),
        "control": dict(control),
        "intent": {
            "schema_version": intent["schema_version"],
            "operation": intent["operation"],
            "workflow_path": intent["control"]["workflow_path"],
            "binding": dict(intent_binding),
            "lock": copy.deepcopy(intent["lock"]),
        },
        "lock_assertion": {
            "authority": lock_assertion["authority"],
            "actor_provenance": lock_assertion["actor_provenance"],
            "original_workflow_path": lock_assertion["original_workflow_path"],
            "original_control_sha": lock_assertion["original_control_sha"],
            "original_run_id": lock_assertion["original_run_id"],
            "original_run_attempt": lock_assertion["original_run_attempt"],
            "rule_id": lock_assertion["rule_id"],
            "rule_identity_sha256": lock_assertion["rule_identity_sha256"],
            "current_main_sha": lock_assertion["current_main_sha"],
            "mutation_intent_sha256": lock_assertion["mutation_intent_sha256"],
            "typed_confirmation_sha256": lock_assertion["typed_confirmation_sha256"],
            "original_provider_job": copy.deepcopy(
                lock_assertion["original_provider_job"]
            ),
            "binding": dict(lock_assertion_binding),
        },
        "lineage": copy.deepcopy(intent["lineage"]),
        "authorities": {
            "upstream": copy.deepcopy(intent["authorities"]),
            "original_receipt": original_authority,
        },
        "classification": {"outcome": outcome, **semantics},
        "provider_observation": {
            "http_methods_used": ["GET"],
            "http_request_count": len(request_log),
            "mutation_request_count": 0,
            "endpoint_labels": ["app", "deployment"],
            "observation_rounds": 2,
            "double_read_equal": True,
            "app_spec_matches_active_deployment": True,
            "transition_absent": transition_absent,
            "migration_succeeded": bool(observed["migration_succeeded"]),
        },
        "before": copy.deepcopy(intent["before"]),
        "desired": copy.deepcopy(intent["desired"]),
        "after": after,
        "gates": {
            "artifacts_authenticated": True,
            "main_unchanged": True,
            "lock_owned": True,
            "get_only": True,
            "double_read_complete": True,
            "app_spec_matches_active_deployment": True,
            "deployment_succeeded": should_succeed,
            "migration_succeeded": should_succeed,
        },
        "rollback": copy.deepcopy(intent["rollback"]),
        "canary": {
            "required": should_succeed,
            "eligible": should_succeed,
            "completed": False,
            "endpoint_labels": copy.deepcopy(intent["canary"]["endpoint_labels"]),
            "route_contract_sha256": intent["canary"]["route_contract_sha256"],
        },
    }
    return common.validate_reconciliation_receipt(receipt)


def reconcile(
    *,
    target: Mapping[str, str],
    token: str,
    control: Mapping[str, Any],
    intent: Mapping[str, Any],
    intent_binding: Mapping[str, Any],
    lock_assertion: Mapping[str, Any],
    lock_assertion_binding: Mapping[str, Any],
    original_receipt: Mapping[str, Any] | None,
    original_receipt_binding: Mapping[str, Any] | None,
    opener: Any | None = None,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    target = common.validate_target_descriptor(dict(target))
    client = ReadOnlyProductionClient(target["app_id"], token, opener=opener)
    try:
        observed = observe_twice(client)
        if observed["public"]["default_ingress_sha256"] != common.sha256_bytes(
            target["default_ingress"].encode("utf-8")
        ):
            common.fail("reconciliation protected ingress differs")
        receipt = build_reconciliation_receipt(
            control=control,
            intent=intent,
            intent_binding=intent_binding,
            lock_assertion=lock_assertion,
            lock_assertion_binding=lock_assertion_binding,
            observed=observed,
            request_log=client.request_log,
            original_receipt=original_receipt,
            original_receipt_binding=original_receipt_binding,
            completed_at=now or dt.datetime.now(dt.timezone.utc),
        )
        common.sanitize_public(receipt, private_values=tuple(target.values()))
        return receipt
    finally:
        client.scrub()


def _control(args: argparse.Namespace, *, include_hashes: bool) -> dict[str, Any]:
    control: dict[str, Any] = {
        "workflow_sha": common.require_sha1(args.workflow_sha, "workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(args.workflow_run_id, "workflow run ID"),
        "run_attempt": common.exact_int(args.workflow_run_attempt, "workflow run attempt", 1, 1),
    }
    if include_hashes:
        control.update(
            {
                "runner_environment": "github-hosted",
                "release_policy_sha256": common.sha256_bytes(Path(args.policy).read_bytes()),
                "change_schema_sha256": common.sha256_bytes(Path(args.change_schema).read_bytes()),
                "mutation_intent_schema_sha256": common.sha256_bytes(Path(args.intent_schema).read_bytes()),
                "reconciliation_schema_sha256": common.sha256_bytes(Path(args.reconciliation_schema).read_bytes()),
                "controller_sha256": common.require_sha256(args.controller_sha256, "reconciliation controller hash"),
            }
        )
    return control


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="GET-only production orphan reconciliation")
    commands = parser.add_subparsers(dest="command", required=True)
    assertion = commands.add_parser("prepare-lock-assertion")
    assertion.add_argument("--assertion-request", required=True)
    assertion.add_argument("--mutation-intent", required=True)
    assertion.add_argument("--mutation-intent-sha256", required=True)
    assertion.add_argument("--workflow-sha", required=True)
    assertion.add_argument("--workflow-run-id", required=True)
    assertion.add_argument("--workflow-run-attempt", required=True, type=int)
    assertion.add_argument("--runner-temp", required=True)
    assertion.add_argument("--output", required=True)

    classify = commands.add_parser("reconcile")
    classify.add_argument("--target-env", required=True, choices=("DO_PRODUCTION_TARGET_JSON",))
    classify.add_argument("--intent", required=True)
    classify.add_argument("--intent-authority", required=True)
    classify.add_argument("--lock-assertion", required=True)
    classify.add_argument("--lock-assertion-authority", required=True)
    classify.add_argument("--original-receipt")
    classify.add_argument("--original-receipt-authority")
    classify.add_argument("--contract", required=True)
    classify.add_argument("--policy", required=True)
    classify.add_argument("--change-schema", required=True)
    classify.add_argument("--intent-schema", required=True)
    classify.add_argument("--reconciliation-schema", required=True)
    classify.add_argument("--controller-sha256", required=True)
    classify.add_argument("--workflow-sha", required=True)
    classify.add_argument("--workflow-run-id", required=True)
    classify.add_argument("--workflow-run-attempt", required=True, type=int)
    classify.add_argument("--runner-temp", required=True)
    classify.add_argument("--output", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    intent_path = Path(args.mutation_intent if args.command == "prepare-lock-assertion" else args.intent)
    intent = common.load_json(intent_path, "production mutation intent")
    if args.command == "prepare-lock-assertion":
        assertion = build_lock_assertion(
            request=common.load_json(Path(args.assertion_request), "lock assertion request"),
            intent=intent,
            intent_sha256=args.mutation_intent_sha256,
            control=_control(args, include_hashes=False),
            now=dt.datetime.now(dt.timezone.utc),
        )
        common.write_canonical_output(Path(args.output), assertion, Path(args.runner_temp))
        return 0
    loaded_controls: dict[str, Any] = {}
    for path, label in (
        (args.contract, "production contract"),
        (args.policy, "release policy"),
        (args.change_schema, "phase schema"),
        (args.intent_schema, "mutation intent schema"),
        (args.reconciliation_schema, "reconciliation schema"),
    ):
        loaded_controls[label] = common.load_json(Path(path), label)
    current_hashes = {
        "release_policy_sha256": common.sha256_bytes(Path(args.policy).read_bytes()),
        "change_schema_sha256": common.sha256_bytes(Path(args.change_schema).read_bytes()),
        "mutation_intent_schema_sha256": common.sha256_bytes(Path(args.intent_schema).read_bytes()),
    }
    intent = common.validate_mutation_intent(intent)
    for key, expected in current_hashes.items():
        if intent["control"][key] != expected:
            common.fail("mutation intent control document hash differs")
    target_raw = os.environ.pop(args.target_env, "")
    token = os.environ.pop("DO_PRODUCTION_PLAN_TOKEN", "")
    target = common.loads_strict(target_raw)
    del target_raw
    target = common.validate_target_descriptor(target)
    contract = loaded_controls["production contract"]
    provider = contract.get("provider") if type(contract) is dict else None
    if (
        type(provider) is not dict
        or common.sha256_bytes(target["app_id"].encode("utf-8"))
        != provider.get("app_id_sha256")
        or common.sha256_bytes(target["default_ingress"].encode("utf-8"))
        != provider.get("default_ingress_sha256")
    ):
        common.fail("protected reconciliation target differs from the production contract")
    original_path = Path(args.original_receipt) if args.original_receipt else None
    original_authority_path = (
        Path(args.original_receipt_authority) if args.original_receipt_authority else None
    )
    if (original_path is None) is not (original_authority_path is None):
        common.fail("original receipt arguments must be supplied together")
    receipt = reconcile(
        target=target,
        token=token,
        control=_control(args, include_hashes=True),
        intent=intent,
        intent_binding=common.load_json(Path(args.intent_authority), "mutation intent authority"),
        lock_assertion=common.load_json(Path(args.lock_assertion), "lock assertion"),
        lock_assertion_binding=common.load_json(
            Path(args.lock_assertion_authority), "lock assertion authority"
        ),
        original_receipt=(common.load_json(original_path, "original receipt") if original_path else None),
        original_receipt_binding=(
            common.load_json(original_authority_path, "original receipt authority")
            if original_authority_path else None
        ),
    )
    del token, target
    common.write_canonical_output(Path(args.output), receipt, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production orphan reconciliation failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
