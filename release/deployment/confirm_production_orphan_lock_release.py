#!/usr/bin/env python3
"""Confirm an exactly authorized production orphan-lock release offline.

This controller has no network client and no branch/provider mutation method. The
workflow performs one isolated branch-rule mutation between a pre-read and a
post-read, then passes only a sanitized request to this module. Only the receipt
emitted here may claim that the orphaned main lock was released.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import re
import sys
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import finalize_production_orphan_lock as finalizer
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/finalize-production-orphan-lock.yml"
AUTHORITY = "production-orphan-lock-release-receipt"
STATE = "confirmed-released"
PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-lock-release-receipt/v1"
)
EXPECTED_PRE_RELEASE = {
    "lock_branch": True,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
EXPECTED_POST_RELEASE = {
    "lock_branch": False,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
OUTCOMES = {"applied", "ambiguous-reconciled"}
RULE_ID_RE = re.compile(r"^[A-Za-z0-9_+/=-]+$")


def _release_projection(value: Any, label: str) -> dict[str, bool]:
    projection = common.exact_keys(
        value,
        {"lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge"},
        label,
    )
    for key in projection:
        common.exact_bool(projection[key], f"{label} {key}")
    return dict(projection)


def _release_fingerprint(request: Mapping[str, Any]) -> str:
    return common.sha256_value(
        {
            "action": "release-main-lock",
            "main_sha": request["main_sha"],
            "rule_id": request["rule_id"],
            "rule_identity_sha256": request["rule_identity_sha256"],
            "pre_release": request["pre_release"],
            "authorized_post_release": request["post_release"],
            "mutation_request_count": 1,
        }
    )


def _root_acquire_intent(value: Any, label: str) -> dict[str, Any]:
    root = common.validate_full_artifact_binding(value, label)
    root = copy.deepcopy(dict(root))
    root["run_id"] = common.require_run_id(root["run_id"], f"{label} run ID")
    root["artifact_id"] = common.require_run_id(
        root["artifact_id"], f"{label} artifact ID"
    )
    expected_names = {
        f"production-main-lock-apply-{root['run_id']}-1",
        f"production-main-lock-rollback-{root['run_id']}-1",
    }
    if root["artifact_name"] not in expected_names:
        common.fail(f"{label} artifact identity differs")
    return root


def validate_release_request(value: Any) -> dict[str, Any]:
    request = common.exact_keys(
        value,
        {
            "preauthorization_sha256", "main_sha", "rule_id",
            "rule_identity_sha256", "http_methods_used",
            "graphql_operations_used", "mutation_request_count", "outcome",
            "mutation_response_received", "read_confirmed", "pre_release",
            "post_release", "mutation_fingerprint_sha256",
        },
        "orphan-lock release request",
    )
    common.require_sha256(
        request["preauthorization_sha256"], "preauthorization exact-file hash"
    )
    common.require_sha1(request["main_sha"], "release main SHA")
    rule_id = common.exact_string(
        request["rule_id"], "release rule ID", RULE_ID_RE
    )
    if common.sha256_bytes(rule_id.encode("utf-8")) != common.require_sha256(
        request["rule_identity_sha256"], "release rule hash"
    ):
        common.fail("release rule identity hash differs")
    if (
        request["http_methods_used"] != ["POST"]
        or request["graphql_operations_used"] != ["query", "mutation", "query"]
        or common.exact_int(
            request["mutation_request_count"], "branch mutation count", 1, 1
        )
        != 1
    ):
        common.fail("orphan-lock release mutation ledger differs")
    outcome = common.exact_string(request["outcome"], "release outcome")
    response_received = common.exact_bool(
        request["mutation_response_received"], "release mutation response flag"
    )
    if outcome not in OUTCOMES or response_received is not (outcome == "applied"):
        common.fail("orphan-lock release outcome differs")
    if common.exact_bool(request["read_confirmed"], "release read confirmation") is not True:
        common.fail("orphan-lock release was not GET-confirmed")
    pre = _release_projection(request["pre_release"], "pre-release projection")
    post = _release_projection(request["post_release"], "post-release projection")
    if pre != EXPECTED_PRE_RELEASE or post != EXPECTED_POST_RELEASE:
        common.fail("orphan-lock release projection differs")
    if request["mutation_fingerprint_sha256"] != _release_fingerprint(request):
        common.fail("orphan-lock release mutation fingerprint differs")
    normalized = copy.deepcopy(dict(request))
    normalized["pre_release"] = pre
    normalized["post_release"] = post
    common.sanitize_public(normalized)
    return normalized


def _control(
    *,
    workflow_sha: Any,
    workflow_run_id: Any,
    workflow_run_attempt: Any,
    release_schema_sha256: Any,
    controller_sha256: Any,
) -> dict[str, Any]:
    return {
        "workflow_sha": common.require_sha1(workflow_sha, "release workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(workflow_run_id, "release run ID"),
        "run_attempt": common.exact_int(
            workflow_run_attempt, "release run attempt", 1, 1
        ),
        "runner_environment": "github-hosted",
        "release_schema_sha256": common.require_sha256(
            release_schema_sha256, "release receipt schema hash"
        ),
        "controller_sha256": common.require_sha256(
            controller_sha256, "release receipt controller hash"
        ),
    }


def build_release_receipt(
    *,
    request: Any,
    preauthorization: Any,
    preauthorization_authority: Any,
    control: Mapping[str, Any],
    completed_at: dt.datetime,
) -> dict[str, Any]:
    request = validate_release_request(request)
    preauthorization = finalizer.validate_finalization_authorization(
        preauthorization, now=completed_at
    )
    control = common.exact_keys(
        dict(control),
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "release_schema_sha256", "controller_sha256",
        },
        "orphan-lock release control",
    )
    if (
        control["workflow_path"] != WORKFLOW_PATH
        or control["runner_environment"] != "github-hosted"
        or control["workflow_sha"] != preauthorization["control"]["workflow_sha"]
        or str(control["run_id"]) != str(preauthorization["control"]["run_id"])
        or control["run_attempt"] != preauthorization["control"]["run_attempt"]
    ):
        common.fail("release receipt control differs from preauthorization")
    for key in ("workflow_sha", "controller_sha256", "release_schema_sha256"):
        if key == "workflow_sha":
            common.require_sha1(control[key], f"release control {key}")
        else:
            common.require_sha256(control[key], f"release control {key}")

    preauthorization_hash = common.sha256_bytes(
        common.canonical_file_bytes(preauthorization)
    )
    if request["preauthorization_sha256"] != preauthorization_hash:
        common.fail("release request preauthorization hash differs")
    preauthorization_authority = finalizer.validate_attested_artifact_authority(
        preauthorization_authority,
        subject=preauthorization,
        workflow_path=WORKFLOW_PATH,
        predicate_type=finalizer.PREDICATE_TYPE,
        artifact_name=(
            f"production-orphan-lock-finalization-"
            f"{preauthorization['control']['run_id']}-1"
        ),
        label="orphan-lock finalization authority",
    )
    orphan = preauthorization["orphan"]
    if (
        request["main_sha"] != orphan["current_main_sha"]
        or request["rule_id"] != orphan["rule_id"]
        or request["rule_identity_sha256"] != orphan["rule_identity_sha256"]
    ):
        common.fail("release request moved from the authorized main or rule")
    root = _root_acquire_intent(
        orphan["root_acquire_intent"], "orphan root acquire intent"
    )
    resolution = preauthorization["resolution"]
    resolution_projection = {
        "closure_kind": resolution["closure_kind"],
        "reconciliation_sha256": resolution["reconciliation_sha256"],
        "closure_receipt_sha256": resolution["closure_receipt_sha256"],
        "orphan_rollback_sha256": resolution["orphan_rollback_sha256"],
        "phase_state_sha256": resolution["phase_state_sha256"],
    }
    receipt = {
        "schema_version": 1,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "completed_at": common.format_timestamp(completed_at),
        "state": STATE,
        "control": copy.deepcopy(dict(control)),
        "preauthorization": {
            "authority": copy.deepcopy(preauthorization_authority),
            "actor_provenance": preauthorization["break_glass"]["actor_provenance"],
            "authorization_scope": preauthorization["break_glass"][
                "authorization_scope"
            ],
            "typed_confirmation_sha256": preauthorization["break_glass"][
                "typed_confirmation_sha256"
            ],
        },
        "branch_release": {
            "main_sha": request["main_sha"],
            "rule_id": request["rule_id"],
            "rule_identity_sha256": request["rule_identity_sha256"],
            "root_acquire_intent": copy.deepcopy(root),
            "http_methods_used": copy.deepcopy(request["http_methods_used"]),
            "graphql_operations_used": copy.deepcopy(
                request["graphql_operations_used"]
            ),
            "mutation_request_count": request["mutation_request_count"],
            "outcome": request["outcome"],
            "mutation_response_received": request["mutation_response_received"],
            "read_confirmed": request["read_confirmed"],
            "pre_release": copy.deepcopy(request["pre_release"]),
            "post_release": copy.deepcopy(request["post_release"]),
            "mutation_fingerprint_sha256": request["mutation_fingerprint_sha256"],
        },
        "resolution": resolution_projection,
        "gates": {
            "preauthorization_authenticated": True,
            "main_unchanged": True,
            "rule_identity_unchanged": True,
            "one_shot_mutation": True,
            "post_read_confirmed": True,
            "lock_released": True,
        },
    }
    validate_release_receipt(
        receipt,
        preauthorization=preauthorization,
        preauthorization_authority=preauthorization_authority,
    )
    return receipt


def validate_release_receipt(
    value: Any,
    *,
    preauthorization: Any,
    preauthorization_authority: Any,
) -> dict[str, Any]:
    receipt = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "completed_at", "state",
            "control", "preauthorization", "branch_release", "resolution", "gates",
        },
        "production orphan-lock release receipt",
    )
    common.exact_int(receipt["schema_version"], "release receipt schema", 1, 1)
    if (
        receipt["authority"] != AUTHORITY
        or receipt["repository"] != common.REPOSITORY
        or receipt["state"] != STATE
    ):
        common.fail("orphan-lock release receipt identity differs")
    common.require_timestamp(receipt["completed_at"], "release completion time")
    control = common.exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "release_schema_sha256", "controller_sha256",
        },
        "release receipt control",
    )
    if control["workflow_path"] != WORKFLOW_PATH or control["runner_environment"] != "github-hosted":
        common.fail("release receipt workflow identity differs")
    common.require_sha1(control["workflow_sha"], "release receipt workflow SHA")
    common.require_run_id(control["run_id"], "release receipt run ID")
    common.exact_int(control["run_attempt"], "release receipt run attempt", 1, 1)
    common.require_sha256(control["release_schema_sha256"], "release schema hash")
    common.require_sha256(control["controller_sha256"], "release controller hash")
    embedded_preauthorization = common.exact_keys(
        receipt["preauthorization"],
        {
            "authority", "actor_provenance", "authorization_scope",
            "typed_confirmation_sha256",
        },
        "release preauthorization",
    )
    common.exact_keys(
        embedded_preauthorization["authority"],
        {
            "binding", "signer_workflow", "signer_digest", "source_digest",
            "source_ref", "runner_environment", "provenance_predicate_type",
            "policy_predicate_type", "provenance_verification_sha256",
            "policy_verification_sha256",
        },
        "release preauthorization authority",
    )
    binding = common.validate_full_artifact_binding(
        embedded_preauthorization["authority"]["binding"],
        "release preauthorization binding",
    )
    authority = embedded_preauthorization["authority"]
    if (
        binding["run_id"] != common.require_run_id(control["run_id"], "control run ID")
        or binding["run_attempt"] != control["run_attempt"]
        or binding["artifact_name"]
        != f"production-orphan-lock-finalization-{control['run_id']}-1"
        or authority["signer_workflow"] != f"{common.REPOSITORY}/{WORKFLOW_PATH}"
        or authority["signer_digest"] != control["workflow_sha"]
        or authority["source_digest"] != control["workflow_sha"]
        or authority["source_ref"] != "refs/heads/main"
        or authority["runner_environment"] != "github-hosted"
        or authority["provenance_predicate_type"]
        != finalizer.PROVENANCE_PREDICATE_TYPE
        or authority["policy_predicate_type"] != finalizer.PREDICATE_TYPE
        or embedded_preauthorization["actor_provenance"] != finalizer.ACTOR_PROVENANCE
        or embedded_preauthorization["authorization_scope"] != finalizer.AUTHORIZATION_SCOPE
    ):
        common.fail("release preauthorization identity differs")
    for key in ("signer_digest", "source_digest"):
        common.require_sha1(authority[key], f"release preauthorization {key}")
    for key in (
        "provenance_verification_sha256", "policy_verification_sha256",
    ):
        common.require_sha256(authority[key], f"release preauthorization {key}")
    common.require_sha256(
        embedded_preauthorization["typed_confirmation_sha256"],
        "release typed confirmation hash",
    )
    request = copy.deepcopy(receipt["branch_release"])
    root = request.pop("root_acquire_intent", None)
    if type(root) is not dict:
        common.fail("release root acquire intent is missing")
    request["preauthorization_sha256"] = binding["sha256"]
    validate_release_request(request)
    _root_acquire_intent(root, "release root acquire intent")
    resolution = common.exact_keys(
        receipt["resolution"],
        {
            "closure_kind", "reconciliation_sha256", "closure_receipt_sha256",
            "orphan_rollback_sha256", "phase_state_sha256",
        },
        "release resolution",
    )
    if resolution["closure_kind"] not in finalizer.CLOSURE_KINDS:
        common.fail("release closure kind differs")
    for key in ("reconciliation_sha256", "closure_receipt_sha256"):
        common.require_sha256(resolution[key], f"release {key}")
    for key in ("orphan_rollback_sha256", "phase_state_sha256"):
        if resolution[key] is not None:
            common.require_sha256(resolution[key], f"release {key}")
    closure = resolution["closure_kind"]
    if closure == finalizer.CLOSURE_NO_MUTATION:
        if (
            resolution["closure_receipt_sha256"]
            != resolution["reconciliation_sha256"]
            or resolution["orphan_rollback_sha256"] is not None
            or resolution["phase_state_sha256"] is not None
        ):
            common.fail("release no-mutation closure differs")
    elif closure == finalizer.CLOSURE_RECONCILIATION:
        if (
            resolution["closure_receipt_sha256"]
            != resolution["reconciliation_sha256"]
            or resolution["orphan_rollback_sha256"] is not None
            or resolution["phase_state_sha256"] is None
        ):
            common.fail("release reconciliation closure differs")
    elif (
        resolution["orphan_rollback_sha256"] is None
        or resolution["phase_state_sha256"] is None
        or resolution["closure_receipt_sha256"]
        != resolution["orphan_rollback_sha256"]
    ):
        common.fail("release orphan-rollback closure differs")
    if receipt["gates"] != {
        "preauthorization_authenticated": True,
        "main_unchanged": True,
        "rule_identity_unchanged": True,
        "one_shot_mutation": True,
        "post_read_confirmed": True,
        "lock_released": True,
    }:
        common.fail("orphan-lock release gates are incomplete")
    source = finalizer.validate_finalization_authorization(preauthorization)
    source_authority = finalizer.validate_attested_artifact_authority(
        preauthorization_authority,
        subject=source,
        workflow_path=WORKFLOW_PATH,
        predicate_type=finalizer.PREDICATE_TYPE,
        artifact_name=f"production-orphan-lock-finalization-{source['control']['run_id']}-1",
        label="release receipt source preauthorization",
    )
    source_resolution = source["resolution"]
    expected_resolution = {
        "closure_kind": source_resolution["closure_kind"],
        "reconciliation_sha256": source_resolution["reconciliation_sha256"],
        "closure_receipt_sha256": source_resolution["closure_receipt_sha256"],
        "orphan_rollback_sha256": source_resolution["orphan_rollback_sha256"],
        "phase_state_sha256": source_resolution["phase_state_sha256"],
    }
    if (
        source_authority != authority
        or source_authority["binding"]["sha256"]
        != common.sha256_bytes(common.canonical_file_bytes(source))
        or source["control"]["workflow_sha"] != control["workflow_sha"]
        or str(source["control"]["run_id"]) != str(control["run_id"])
        or source["control"]["run_attempt"] != control["run_attempt"]
        or source["orphan"]["current_main_sha"]
        != receipt["branch_release"]["main_sha"]
        or source["orphan"]["rule_id"] != receipt["branch_release"]["rule_id"]
        or source["orphan"]["rule_identity_sha256"]
        != receipt["branch_release"]["rule_identity_sha256"]
        or _root_acquire_intent(
            source["orphan"]["root_acquire_intent"], "source root acquire intent"
        )
        != root
        or expected_resolution != resolution
        or source["break_glass"]["actor_provenance"]
        != embedded_preauthorization["actor_provenance"]
        or source["break_glass"]["authorization_scope"]
        != embedded_preauthorization["authorization_scope"]
        or source["break_glass"]["typed_confirmation_sha256"]
        != embedded_preauthorization["typed_confirmation_sha256"]
    ):
        common.fail("release receipt is cross-spliced from its preauthorization")
    common.sanitize_public(receipt, allowed_keys=("completed_at",))
    return receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Confirm an exactly authorized orphan main-lock release"
    )
    commands = parser.add_subparsers(dest="command", required=True)
    confirm = commands.add_parser("confirm")
    for name in (
        "release-request", "preauthorization", "preauthorization-authority",
        "release-schema", "controller-sha256", "workflow-sha",
        "workflow-run-id", "runner-temp", "output",
    ):
        confirm.add_argument(f"--{name}", required=True)
    confirm.add_argument("--workflow-run-attempt", required=True, type=int)
    validate = commands.add_parser("validate")
    validate.add_argument("--receipt", required=True)
    validate.add_argument("--sha256", required=True)
    validate.add_argument("--preauthorization", required=True)
    validate.add_argument("--preauthorization-authority", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    if args.command == "validate":
        path = Path(args.receipt)
        receipt = common.load_json(path, "orphan-lock release receipt")
        if common.sha256_bytes(path.read_bytes()) != common.require_sha256(
            args.sha256, "expected release receipt hash"
        ):
            common.fail("release receipt exact-file hash differs")
        validate_release_receipt(
            receipt,
            preauthorization=common.load_json(
                Path(args.preauthorization), "release source preauthorization"
            ),
            preauthorization_authority=common.load_json(
                Path(args.preauthorization_authority),
                "release source preauthorization authority",
            ),
        )
        return 0
    schema_path = Path(args.release_schema)
    common.load_json(schema_path, "orphan-lock release schema")
    receipt = build_release_receipt(
        request=common.load_json(Path(args.release_request), "orphan-lock release request"),
        preauthorization=common.load_json(
            Path(args.preauthorization), "orphan-lock finalization authorization"
        ),
        preauthorization_authority=common.load_json(
            Path(args.preauthorization_authority),
            "orphan-lock finalization attestation authority",
        ),
        control=_control(
            workflow_sha=args.workflow_sha,
            workflow_run_id=args.workflow_run_id,
            workflow_run_attempt=args.workflow_run_attempt,
            release_schema_sha256=common.sha256_bytes(schema_path.read_bytes()),
            controller_sha256=args.controller_sha256,
        ),
        completed_at=dt.datetime.now(dt.timezone.utc),
    )
    common.write_canonical_output(Path(args.output), receipt, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production orphan-lock release confirmation failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
