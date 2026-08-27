#!/usr/bin/env python3
"""Certify an unlocked orphan main rule after an incomplete finalizer run.

This controller is observation-only. It has no network client, token handling, or
branch/provider mutation method. A separate workflow performs two identical
read-only observation rounds and supplies only sanitized evidence. The resulting
authority deliberately does not claim how many mutations the interrupted run
sent or that a release receipt existed.
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


WORKFLOW_PATH = ".github/workflows/reconcile-production-orphan-lock-release.yml"
WORKFLOW_NAME = "Reconcile Production Orphan Lock Release"
ASSERTION_AUTHORITY = "production-orphan-lock-release-assertion"
ASSERTION_PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-lock-release-assertion/v1"
)
RECONCILIATION_AUTHORITY = "production-orphan-lock-release-reconciliation"
RECONCILIATION_PREDICATE_TYPE = (
    "https://rereply.app/attestations/production-orphan-lock-release-reconciliation/v1"
)
ACTOR_PROVENANCE = (
    "single-operator-break-glass-assertion-not-cryptographic-actor-provenance"
)
AUTHORIZATION_SCOPE = "observe-orphan-lock-release-only"
ASSERTION_MAX_AGE_SECONDS = 600
SOURCE_MAX_AGE_DAYS = 30
OUTCOME = "observed-unlocked-after-incomplete-finalizer"
RECEIPT_TRUTH_CLASSES = {
    "preauthorization-only",
    "unsigned-unattested",
    "attested-receipt-upload-incomplete",
}

FINALIZER_JOBS = [
    "Authenticate exact production orphan finalization authority",
    "Prepare and attest exact production orphan lock finalization",
    "Release exact production orphan main lock",
    "Exact production orphan lock release receipt gate",
]
RELEASE_JOB = FINALIZER_JOBS[2]
GATE_JOB = FINALIZER_JOBS[3]
PREAUTH_PREFIX = "production-orphan-lock-finalization"
UNSIGNED_PREFIX = "unsigned-production-orphan-lock-release"
SIGNED_PREFIX = "production-orphan-lock-release"

EXPECTED_LOCKED = {
    "lock_branch": True,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
EXPECTED_UNLOCKED = {
    "lock_branch": False,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
REQUEST_LEDGER = [
    {"round": 1, "label": "main-ref", "http_method": "GET", "api_operation": "rest-read"},
    {"round": 1, "label": "branch-rule", "http_method": "POST", "api_operation": "graphql-query"},
    {"round": 2, "label": "main-ref", "http_method": "GET", "api_operation": "rest-read"},
    {"round": 2, "label": "branch-rule", "http_method": "POST", "api_operation": "graphql-query"},
]
RULE_ID_RE = re.compile(r"^[A-Za-z0-9_+/=-]{1,512}$")


def _binding(value: Any, label: str) -> dict[str, Any]:
    binding = copy.deepcopy(common.validate_full_artifact_binding(value, label))
    # JSON/API producers sometimes represent these IDs numerically.  Keep the
    # signed representation deterministic everywhere this controller emits it.
    binding["run_id"] = str(binding["run_id"])
    binding["artifact_id"] = str(binding["artifact_id"])
    return binding


def _rule_id(value: Any, label: str) -> str:
    rule_id = common.exact_string(value, label)
    if RULE_ID_RE.fullmatch(rule_id) is None:
        common.fail(f"{label} differs")
    return rule_id


def _validate_attested_authority_shape(value: Any, label: str) -> dict[str, Any]:
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
    normalized = copy.deepcopy(dict(authority))
    normalized["binding"] = _binding(authority["binding"], f"{label} binding")
    common.require_sha1(authority["signer_digest"], f"{label} signer digest")
    common.require_sha1(authority["source_digest"], f"{label} source digest")
    common.exact_string(authority["signer_workflow"], f"{label} signer workflow")
    common.exact_string(authority["source_ref"], f"{label} source ref")
    common.exact_string(authority["runner_environment"], f"{label} runner")
    common.exact_string(
        authority["provenance_predicate_type"], f"{label} provenance predicate"
    )
    common.exact_string(authority["policy_predicate_type"], f"{label} policy predicate")
    for key in ("provenance_verification_sha256", "policy_verification_sha256"):
        common.require_sha256(authority[key], f"{label} {key}")
    return normalized


def _job(value: Any, *, expected_name: str) -> dict[str, Any]:
    job = common.exact_keys(
        value,
        {"job_id", "name", "status", "conclusion", "started_at", "completed_at"},
        f"original finalizer job {expected_name}",
    )
    job_id = common.require_run_id(job["job_id"], f"{expected_name} job ID")
    if job["name"] != expected_name or job["status"] != "completed":
        common.fail("original finalizer job identity differs")
    conclusion = common.exact_string(job["conclusion"], f"{expected_name} conclusion")
    started = common.require_timestamp(job["started_at"], f"{expected_name} started_at")
    completed = common.require_timestamp(
        job["completed_at"], f"{expected_name} completed_at"
    )
    if completed < started:
        common.fail("original finalizer job timing differs")
    normalized = copy.deepcopy(dict(job))
    normalized["job_id"] = job_id
    return normalized


def _receipt_truth(
    value: Any,
    *,
    artifacts: Sequence[Mapping[str, Any]],
    label: str,
) -> dict[str, Any]:
    truth = common.exact_keys(
        value,
        {
            "observed_at", "classification", "source_artifact_inventory_sha256",
            "signed_release_artifact_present_at_observation",
            "unsigned_receipt_binding", "provenance_match_count",
            "policy_match_count", "provenance_query_sha256",
            "policy_query_sha256", "provenance_verification_sha256",
            "policy_verification_sha256",
        },
        label,
    )
    common.require_timestamp(truth["observed_at"], f"{label} observation time")
    classification = common.exact_string(truth["classification"], f"{label} classification")
    if classification not in RECEIPT_TRUTH_CLASSES:
        common.fail(f"{label} classification differs")
    common.require_sha256(
        truth["source_artifact_inventory_sha256"], f"{label} inventory hash"
    )
    if truth["source_artifact_inventory_sha256"] != common.sha256_value(list(artifacts)):
        common.fail(f"{label} inventory hash differs")
    if common.exact_bool(
        truth["signed_release_artifact_present_at_observation"],
        f"{label} signed artifact flag",
    ) is not False:
        common.fail(f"{label} signed artifact unexpectedly exists")
    provenance_count = common.exact_int(
        truth["provenance_match_count"], f"{label} provenance count", 0, 1
    )
    policy_count = common.exact_int(
        truth["policy_match_count"], f"{label} policy count", 0, 1
    )
    common.require_sha256(truth["provenance_query_sha256"], f"{label} provenance query")
    common.require_sha256(truth["policy_query_sha256"], f"{label} policy query")
    unsigned = None
    if truth["unsigned_receipt_binding"] is not None:
        unsigned = _binding(truth["unsigned_receipt_binding"], f"{label} unsigned receipt")
    unsigned_artifacts = [
        item["binding"] for item in artifacts
        if item.get("kind") == "unsigned-release-receipt"
    ]
    if len(unsigned_artifacts) > 1:
        common.fail(f"{label} unsigned receipt inventory differs")
    expected_unsigned = unsigned_artifacts[0] if unsigned_artifacts else None
    if unsigned != expected_unsigned:
        common.fail(f"{label} unsigned receipt binding differs")
    provenance_verification = truth["provenance_verification_sha256"]
    policy_verification = truth["policy_verification_sha256"]
    if provenance_verification is not None:
        common.require_sha256(provenance_verification, f"{label} provenance verification")
    if policy_verification is not None:
        common.require_sha256(policy_verification, f"{label} policy verification")
    expected = {
        "preauthorization-only": (False, 0, 0, None, None),
        "unsigned-unattested": (True, 0, 0, None, None),
        "attested-receipt-upload-incomplete": (True, 1, 1, "sha", "sha"),
    }[classification]
    actual = (
        unsigned is not None,
        provenance_count,
        policy_count,
        None if provenance_verification is None else "sha",
        None if policy_verification is None else "sha",
    )
    if actual != expected:
        common.fail(f"{label} attestation classification differs")
    normalized = copy.deepcopy(dict(truth))
    normalized["unsigned_receipt_binding"] = unsigned
    return normalized


def _same_receipt_truth(left: Mapping[str, Any], right: Mapping[str, Any]) -> bool:
    ignored = {
        "observed_at", "provenance_query_sha256", "policy_query_sha256",
        "provenance_verification_sha256", "policy_verification_sha256",
    }
    return {key: value for key, value in left.items() if key not in ignored} == {
        key: value for key, value in right.items() if key not in ignored
    }


def _require_truth_window(
    truth: Mapping[str, Any], *, not_before: dt.datetime, not_after: dt.datetime, label: str
) -> None:
    observed = common.require_timestamp(truth["observed_at"], f"{label} observed_at")
    if observed < not_before or observed > not_after:
        common.fail(f"{label} observation time differs")


def _original_finalizer(value: Any, *, preauthorization_binding: Mapping[str, Any]) -> dict[str, Any]:
    source = common.exact_keys(
        value,
        {
            "workflow_sha", "workflow_path", "workflow_name", "run_id",
            "run_attempt", "event", "head_branch", "status", "conclusion",
            "jobs", "job_inventory_sha256", "artifacts",
            "artifact_inventory_sha256", "receipt_truth",
            "static_max_branch_mutations",
        },
        "original incomplete finalizer",
    )
    common.require_sha1(source["workflow_sha"], "original finalizer control SHA")
    if (
        source["workflow_path"] != finalizer.WORKFLOW_PATH
        or source["workflow_name"] != "Finalize Production Orphan Lock"
        or source["event"] != "workflow_dispatch"
        or source["head_branch"] != "main"
        or source["status"] != "completed"
        or source["conclusion"] not in {"failure", "cancelled", "timed_out"}
    ):
        common.fail("original incomplete finalizer run identity differs")
    run_id = common.require_run_id(source["run_id"], "original finalizer run ID")
    common.exact_int(source["run_attempt"], "original finalizer attempt", 1, 1)
    if type(source["jobs"]) is not list or len(source["jobs"]) != len(FINALIZER_JOBS):
        common.fail("original finalizer job inventory differs")
    jobs = [
        _job(item, expected_name=expected)
        for item, expected in zip(source["jobs"], FINALIZER_JOBS, strict=True)
    ]
    for index in (0, 1):
        if jobs[index]["conclusion"] != "success":
            common.fail("original finalizer prerequisite job did not succeed")
    if jobs[2]["conclusion"] not in {"success", "failure", "cancelled", "timed_out"}:
        common.fail("original finalizer release job conclusion differs")
    if jobs[3]["conclusion"] not in {"failure", "cancelled", "timed_out", "skipped"}:
        common.fail("original finalizer receipt gate unexpectedly succeeded")
    if source["job_inventory_sha256"] != common.sha256_value(jobs):
        common.fail("original finalizer job inventory hash differs")
    artifacts = source["artifacts"]
    if type(artifacts) is not list or len(artifacts) not in {1, 2}:
        common.fail("original finalizer artifact inventory differs")
    normalized_artifacts: list[dict[str, Any]] = []
    seen: set[str] = set()
    for index, item in enumerate(artifacts):
        artifact = common.exact_keys(
            item, {"kind", "binding"}, "original finalizer artifact"
        )
        kind = common.exact_string(artifact["kind"], "original artifact kind")
        if kind in seen or kind not in {"preauthorization", "unsigned-release-receipt"}:
            common.fail("original finalizer artifact kind differs")
        seen.add(kind)
        item_binding = _binding(artifact["binding"], f"original {kind} artifact")
        expected_name = (
            f"{PREAUTH_PREFIX}-{run_id}-1"
            if kind == "preauthorization"
            else f"{UNSIGNED_PREFIX}-{run_id}-1"
        )
        if (
            item_binding["run_id"] != run_id
            or item_binding["run_attempt"] != 1
            or item_binding["artifact_name"] != expected_name
        ):
            common.fail("original finalizer artifact identity differs")
        normalized_artifacts.append({"kind": kind, "binding": item_binding})
        if index == 0 and kind != "preauthorization":
            common.fail("preauthorization must be the first original artifact")
    if (
        "preauthorization" not in seen
        or normalized_artifacts[0]["binding"] != dict(preauthorization_binding)
        or source["artifact_inventory_sha256"]
        != common.sha256_value(normalized_artifacts)
        or common.exact_int(
            source["static_max_branch_mutations"],
            "original static mutation maximum",
            1,
            1,
        )
        != 1
    ):
        common.fail("original finalizer artifact or mutation authority differs")
    normalized = copy.deepcopy(dict(source))
    normalized["run_id"] = run_id
    normalized["jobs"] = jobs
    normalized["artifacts"] = normalized_artifacts
    normalized["receipt_truth"] = _receipt_truth(
        source["receipt_truth"],
        artifacts=normalized_artifacts,
        label="original finalizer receipt truth",
    )
    return normalized


def _assertion_control(
    *, workflow_sha: Any, workflow_run_id: Any, workflow_run_attempt: Any,
    assertion_schema_sha256: Any, controller_sha256: Any,
) -> dict[str, Any]:
    return {
        "workflow_sha": common.require_sha1(workflow_sha, "reconcile workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(workflow_run_id, "reconcile run ID"),
        "run_attempt": common.exact_int(workflow_run_attempt, "reconcile attempt", 1, 1),
        "runner_environment": "github-hosted",
        "assertion_schema_sha256": common.require_sha256(
            assertion_schema_sha256, "release assertion schema hash"
        ),
        "controller_sha256": common.require_sha256(
            controller_sha256, "release reconciler controller hash"
        ),
    }


def build_assertion(
    *, request: Any, preauthorization: Any, preauthorization_authority: Any,
    control: Mapping[str, Any], created_at: dt.datetime,
) -> dict[str, Any]:
    preauthorization = finalizer.validate_finalization_authorization(preauthorization)
    prepared = common.require_timestamp(
        preauthorization["prepared_at"], "source preauthorization preparation"
    )
    checked = created_at.astimezone(dt.timezone.utc).replace(microsecond=0)
    if checked < prepared or checked - prepared > dt.timedelta(days=SOURCE_MAX_AGE_DAYS):
        common.fail("source preauthorization is outside artifact retention")
    preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauthorization))
    preauth_authority = finalizer.validate_attested_artifact_authority(
        preauthorization_authority,
        subject=preauthorization,
        workflow_path=finalizer.WORKFLOW_PATH,
        predicate_type=finalizer.PREDICATE_TYPE,
        artifact_name=(
            f"{PREAUTH_PREFIX}-{preauthorization['control']['run_id']}-1"
        ),
        label="release reconciliation preauthorization authority",
    )
    request = common.exact_keys(
        request,
        {
            "actor_provenance", "authorization_scope", "current_main_sha",
            "rule_id", "rule_identity_sha256", "preauthorization_sha256",
            "typed_confirmation_sha256", "original_finalizer",
        },
        "release reconciliation assertion request",
    )
    orphan = preauthorization["orphan"]
    phrase = (
        f"RECONCILE UNLOCKED PRODUCTION "
        f"{preauthorization['control']['run_id']} {preauth_hash}"
    )
    expected_confirmation = common.sha256_bytes(phrase.encode("utf-8"))
    if (
        request["actor_provenance"] != ACTOR_PROVENANCE
        or request["authorization_scope"] != AUTHORIZATION_SCOPE
        or request["current_main_sha"] != orphan["current_main_sha"]
        or request["rule_id"] != orphan["rule_id"]
        or request["rule_identity_sha256"] != orphan["rule_identity_sha256"]
        or request["preauthorization_sha256"] != preauth_hash
        or request["typed_confirmation_sha256"] != expected_confirmation
    ):
        common.fail("release reconciliation break-glass request differs")
    original = _original_finalizer(
        request["original_finalizer"],
        preauthorization_binding=preauth_authority["binding"],
    )
    latest_source_job = max(
        common.require_timestamp(job["completed_at"], "source finalizer completion")
        for job in original["jobs"]
    )
    _require_truth_window(
        original["receipt_truth"],
        not_before=latest_source_job,
        not_after=checked,
        label="source receipt truth",
    )
    control = copy.deepcopy(dict(control))
    if (
        control["workflow_path"] != WORKFLOW_PATH
        or control["runner_environment"] != "github-hosted"
        or control["workflow_sha"] != orphan["current_main_sha"]
        or original["workflow_sha"] != orphan["current_main_sha"]
        or original["run_id"] != preauthorization["control"]["run_id"]
        or original["run_attempt"] != preauthorization["control"]["run_attempt"]
    ):
        common.fail("release reconciliation control or source differs")
    root = _binding(orphan["root_acquire_intent"], "release reconciliation lock root")
    assertion = {
        "schema_version": 1,
        "authority": ASSERTION_AUTHORITY,
        "repository": common.REPOSITORY,
        "created_at": common.format_timestamp(checked),
        "expires_at": common.format_timestamp(
            checked + dt.timedelta(seconds=ASSERTION_MAX_AGE_SECONDS)
        ),
        "control": control,
        "actor_provenance": ACTOR_PROVENANCE,
        "authorization_scope": AUTHORIZATION_SCOPE,
        "typed_confirmation_sha256": expected_confirmation,
        "preauthorization": copy.deepcopy(preauth_authority),
        "orphan": {
            "main_sha": orphan["current_main_sha"],
            "rule_id": orphan["rule_id"],
            "rule_identity_sha256": orphan["rule_identity_sha256"],
            "root_acquire_intent": root,
            "preauthorization_sha256": preauth_hash,
        },
        "original_finalizer": original,
        "observation_contract": {
            "ordered_requests": copy.deepcopy(REQUEST_LEDGER),
            "http_request_count": 4,
            "graphql_query_count": 2,
            "branch_mutation_request_count": 0,
            "observation_rounds": 2,
            "required_projection": copy.deepcopy(EXPECTED_UNLOCKED),
        },
    }
    return validate_assertion(assertion, now=checked)


def validate_assertion(value: Any, *, now: dt.datetime | None = None) -> dict[str, Any]:
    assertion = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "created_at", "expires_at",
            "control", "actor_provenance", "authorization_scope",
            "typed_confirmation_sha256", "preauthorization", "orphan",
            "original_finalizer", "observation_contract",
        },
        "orphan-lock release reconciliation assertion",
    )
    common.exact_int(assertion["schema_version"], "release assertion schema", 1, 1)
    if (
        assertion["authority"] != ASSERTION_AUTHORITY
        or assertion["repository"] != common.REPOSITORY
        or assertion["actor_provenance"] != ACTOR_PROVENANCE
        or assertion["authorization_scope"] != AUTHORIZATION_SCOPE
    ):
        common.fail("release reconciliation assertion identity differs")
    created = common.require_timestamp(assertion["created_at"], "release assertion creation")
    expires = common.require_timestamp(assertion["expires_at"], "release assertion expiry")
    if expires <= created or (expires - created).total_seconds() != ASSERTION_MAX_AGE_SECONDS:
        common.fail("release reconciliation assertion window differs")
    if now is not None:
        if now.tzinfo is None or now.utcoffset() is None:
            common.fail("release reconciliation assertion clock is invalid")
        checked = now.astimezone(dt.timezone.utc)
        if checked < created or checked >= expires:
            common.fail("release reconciliation assertion is stale or future-dated")
    control = common.exact_keys(
        assertion["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "assertion_schema_sha256", "controller_sha256",
        },
        "release reconciliation assertion control",
    )
    common.require_sha1(control["workflow_sha"], "release assertion control SHA")
    common.require_run_id(control["run_id"], "release assertion run ID")
    common.exact_int(control["run_attempt"], "release assertion attempt", 1, 1)
    if control["workflow_path"] != WORKFLOW_PATH or control["runner_environment"] != "github-hosted":
        common.fail("release assertion workflow differs")
    common.require_sha256(control["assertion_schema_sha256"], "assertion schema hash")
    common.require_sha256(control["controller_sha256"], "assertion controller hash")
    common.require_sha256(assertion["typed_confirmation_sha256"], "assertion confirmation hash")
    preauth = _validate_attested_authority_shape(
        assertion["preauthorization"], "release assertion preauthorization"
    )
    if (
        preauth["signer_workflow"] != f"{common.REPOSITORY}/{finalizer.WORKFLOW_PATH}"
        or preauth["signer_digest"] != control["workflow_sha"]
        or preauth["source_digest"] != control["workflow_sha"]
        or preauth["source_ref"] != "refs/heads/main"
        or preauth["runner_environment"] != "github-hosted"
        or preauth["provenance_predicate_type"] != finalizer.PROVENANCE_PREDICATE_TYPE
        or preauth["policy_predicate_type"] != finalizer.PREDICATE_TYPE
    ):
        common.fail("release assertion preauthorization authority differs")
    orphan = common.exact_keys(
        assertion["orphan"],
        {
            "main_sha", "rule_id", "rule_identity_sha256", "root_acquire_intent",
            "preauthorization_sha256",
        },
        "release assertion orphan",
    )
    common.require_sha1(orphan["main_sha"], "release assertion main SHA")
    rule_id = _rule_id(orphan["rule_id"], "release assertion rule ID")
    if common.sha256_bytes(rule_id.encode()) != common.require_sha256(
        orphan["rule_identity_sha256"], "release assertion rule hash"
    ):
        common.fail("release assertion rule hash differs")
    root = _binding(orphan["root_acquire_intent"], "release assertion root lock")
    expected_roots = {
        f"production-main-lock-apply-{root['run_id']}-1",
        f"production-main-lock-rollback-{root['run_id']}-1",
    }
    if root["artifact_name"] not in expected_roots:
        common.fail("release assertion root lock identity differs")
    if (
        orphan["preauthorization_sha256"] != preauth["binding"]["sha256"]
        or orphan["main_sha"] != control["workflow_sha"]
    ):
        common.fail("release assertion orphan authority differs")
    original = _original_finalizer(
        assertion["original_finalizer"], preauthorization_binding=preauth["binding"]
    )
    latest_source_job = max(
        common.require_timestamp(job["completed_at"], "source finalizer completion")
        for job in original["jobs"]
    )
    _require_truth_window(
        original["receipt_truth"],
        not_before=latest_source_job,
        not_after=created,
        label="source receipt truth",
    )
    if original["workflow_sha"] != control["workflow_sha"]:
        common.fail("release assertion original control differs")
    contract = common.exact_keys(
        assertion["observation_contract"],
        {
            "ordered_requests", "http_request_count", "graphql_query_count",
            "branch_mutation_request_count", "observation_rounds",
            "required_projection",
        },
        "release observation contract",
    )
    if (
        contract["ordered_requests"] != REQUEST_LEDGER
        or contract["http_request_count"] != 4
        or contract["graphql_query_count"] != 2
        or contract["branch_mutation_request_count"] != 0
        or contract["observation_rounds"] != 2
        or contract["required_projection"] != EXPECTED_UNLOCKED
    ):
        common.fail("release observation contract differs")
    common.sanitize_public(assertion, allowed_keys=("created_at",))
    return assertion


def _observation_round(value: Any, *, expected_round: int) -> dict[str, Any]:
    observed = common.exact_keys(
        value, {"round", "main_sha", "rule"}, "release observation round"
    )
    if common.exact_int(observed["round"], "observation round", expected_round, expected_round) != expected_round:
        common.fail("release observation round differs")
    common.require_sha1(observed["main_sha"], "observed main SHA")
    rule = common.exact_keys(
        observed["rule"],
        {
            "rule_id", "rule_identity_sha256", "pattern", "lock_branch",
            "is_admin_enforced", "lock_allows_fetch_and_merge",
        },
        "observed branch rule",
    )
    rule_id = _rule_id(rule["rule_id"], "observed rule ID")
    if (
        common.sha256_bytes(rule_id.encode())
        != common.require_sha256(rule["rule_identity_sha256"], "observed rule hash")
        or rule["pattern"] != "main"
    ):
        common.fail("observed rule identity differs")
    for key in ("lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge"):
        common.exact_bool(rule[key], f"observed rule {key}")
    return copy.deepcopy(dict(observed))


def validate_observation_request(
    value: Any, *, source_artifacts: Sequence[Mapping[str, Any]]
) -> dict[str, Any]:
    request = common.exact_keys(
        value,
        {
            "ordered_requests", "http_request_count", "graphql_query_count",
            "branch_mutation_request_count", "mutation_text_present",
            "observation_rounds", "double_read_equal", "read_confirmed",
            "source_receipt_truth",
        },
        "orphan-lock release observation request",
    )
    if (
        request["ordered_requests"] != REQUEST_LEDGER
        or common.exact_int(request["http_request_count"], "observation request count", 4, 4) != 4
        or common.exact_int(request["graphql_query_count"], "GraphQL query count", 2, 2) != 2
        or common.exact_int(request["branch_mutation_request_count"], "branch mutation count", 0, 0) != 0
        or common.exact_bool(request["mutation_text_present"], "mutation text flag") is not False
        or common.exact_bool(request["double_read_equal"], "double-read equality") is not True
        or common.exact_bool(request["read_confirmed"], "read confirmation") is not True
        or type(request["observation_rounds"]) is not list
        or len(request["observation_rounds"]) != 2
    ):
        common.fail("orphan-lock release observation ledger differs")
    rounds = [
        _observation_round(item, expected_round=index)
        for index, item in enumerate(request["observation_rounds"], start=1)
    ]
    if {key: value for key, value in rounds[0].items() if key != "round"} != {
        key: value for key, value in rounds[1].items() if key != "round"
    }:
        common.fail("orphan-lock release observation rounds differ")
    normalized = copy.deepcopy(dict(request))
    normalized["observation_rounds"] = rounds
    normalized["source_receipt_truth"] = _receipt_truth(
        request["source_receipt_truth"],
        artifacts=source_artifacts,
        label="observation source receipt truth",
    )
    common.sanitize_public(normalized)
    return normalized


def _reconciliation_control(
    *, workflow_sha: Any, workflow_run_id: Any, workflow_run_attempt: Any,
    assertion_schema_sha256: Any, reconciliation_schema_sha256: Any,
    controller_sha256: Any,
) -> dict[str, Any]:
    control = _assertion_control(
        workflow_sha=workflow_sha,
        workflow_run_id=workflow_run_id,
        workflow_run_attempt=workflow_run_attempt,
        assertion_schema_sha256=assertion_schema_sha256,
        controller_sha256=controller_sha256,
    )
    control["reconciliation_schema_sha256"] = common.require_sha256(
        reconciliation_schema_sha256, "release reconciliation schema hash"
    )
    return control


def build_reconciliation(
    *, observation_request: Any, assertion: Any, assertion_authority: Any,
    preauthorization: Any, preauthorization_authority: Any,
    control: Mapping[str, Any], completed_at: dt.datetime,
) -> dict[str, Any]:
    assertion = validate_assertion(assertion, now=completed_at)
    preauthorization = finalizer.validate_finalization_authorization(preauthorization)
    preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauthorization))
    preauth_authority = finalizer.validate_attested_artifact_authority(
        preauthorization_authority,
        subject=preauthorization,
        workflow_path=finalizer.WORKFLOW_PATH,
        predicate_type=finalizer.PREDICATE_TYPE,
        artifact_name=f"{PREAUTH_PREFIX}-{preauthorization['control']['run_id']}-1",
        label="release reconciliation preauthorization authority",
    )
    assertion_authority = finalizer.validate_attested_artifact_authority(
        assertion_authority,
        subject=assertion,
        workflow_path=WORKFLOW_PATH,
        predicate_type=ASSERTION_PREDICATE_TYPE,
        artifact_name=f"production-orphan-lock-release-assertion-{assertion['control']['run_id']}-1",
        label="release reconciliation assertion authority",
    )
    control = copy.deepcopy(dict(control))
    if (
        control["workflow_path"] != WORKFLOW_PATH
        or control["workflow_sha"] != assertion["control"]["workflow_sha"]
        or str(control["run_id"]) != str(assertion["control"]["run_id"])
        or control["run_attempt"] != assertion["control"]["run_attempt"]
        or control["assertion_schema_sha256"]
        != assertion["control"]["assertion_schema_sha256"]
        or control["controller_sha256"] != assertion["control"]["controller_sha256"]
        or preauth_hash != assertion["orphan"]["preauthorization_sha256"]
        or preauth_authority != assertion["preauthorization"]
    ):
        common.fail("release reconciliation authority chain differs")
    observation = validate_observation_request(
        observation_request,
        source_artifacts=assertion["original_finalizer"]["artifacts"],
    )
    if not _same_receipt_truth(
        observation["source_receipt_truth"],
        assertion["original_finalizer"]["receipt_truth"],
    ):
        common.fail("release reconciliation source receipt truth changed")
    _require_truth_window(
        observation["source_receipt_truth"],
        not_before=common.require_timestamp(assertion["created_at"], "assertion creation"),
        not_after=completed_at,
        label="observed source receipt truth",
    )
    round_one = observation["observation_rounds"][0]
    orphan = assertion["orphan"]
    rule = round_one["rule"]
    if (
        round_one["main_sha"] != orphan["main_sha"]
        or rule["rule_id"] != orphan["rule_id"]
        or rule["rule_identity_sha256"] != orphan["rule_identity_sha256"]
        or {
            "lock_branch": rule["lock_branch"],
            "is_admin_enforced": rule["is_admin_enforced"],
            "lock_allows_fetch_and_merge": rule["lock_allows_fetch_and_merge"],
        }
        != EXPECTED_UNLOCKED
    ):
        common.fail("release reconciliation live authority differs")
    receipt = {
        "schema_version": 1,
        "authority": RECONCILIATION_AUTHORITY,
        "repository": common.REPOSITORY,
        "completed_at": common.format_timestamp(completed_at),
        "control": control,
        "assertion": copy.deepcopy(assertion_authority),
        "preauthorization": copy.deepcopy(preauth_authority),
        "classification": {
            "outcome": OUTCOME,
            "terminal": True,
            "signed_release_artifact_present_at_observation": False,
            "source_receipt_classification": observation[
                "source_receipt_truth"
            ]["classification"],
            "mutation_count_observed": None,
            "original_static_max_branch_mutations": 1,
        },
        "orphan": copy.deepcopy(orphan),
        "original_finalizer": copy.deepcopy(assertion["original_finalizer"]),
        "observation": observation,
        "gates": {
            "assertion_authenticated": True,
            "preauthorization_authenticated": True,
            "main_unchanged": True,
            "rule_identity_unchanged": True,
            "double_read_complete": True,
            "branch_mutation_absent": True,
            "lock_observed_released": True,
        },
        "execution_boundary": {
            "controller_network_access": False,
            "provider_network_access": False,
            "branch_mutation_request_count": 0,
            "provider_token_present": False,
            "branch_write_token_present": False,
            "observation_only": True,
        },
    }
    return validate_reconciliation(
        receipt,
        assertion=assertion,
        assertion_authority=assertion_authority,
        preauthorization=preauthorization,
        preauthorization_authority=preauth_authority,
    )


def validate_reconciliation(
    value: Any,
    *,
    assertion: Any,
    assertion_authority: Any,
    preauthorization: Any,
    preauthorization_authority: Any,
) -> dict[str, Any]:
    receipt = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "completed_at", "control",
            "assertion", "preauthorization", "classification", "orphan",
            "original_finalizer", "observation", "gates", "execution_boundary",
        },
        "production orphan-lock release reconciliation",
    )
    common.exact_int(receipt["schema_version"], "release reconciliation schema", 1, 1)
    if receipt["authority"] != RECONCILIATION_AUTHORITY or receipt["repository"] != common.REPOSITORY:
        common.fail("release reconciliation identity differs")
    completed_at = common.require_timestamp(
        receipt["completed_at"], "release reconciliation completion"
    )
    control = common.exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "assertion_schema_sha256",
            "controller_sha256", "reconciliation_schema_sha256",
        },
        "release reconciliation control",
    )
    common.require_sha1(control["workflow_sha"], "release reconciliation control SHA")
    common.require_run_id(control["run_id"], "release reconciliation run ID")
    common.exact_int(control["run_attempt"], "release reconciliation attempt", 1, 1)
    if control["workflow_path"] != WORKFLOW_PATH or control["runner_environment"] != "github-hosted":
        common.fail("release reconciliation workflow differs")
    for key in ("assertion_schema_sha256", "controller_sha256", "reconciliation_schema_sha256"):
        common.require_sha256(control[key], f"release reconciliation {key}")
    embedded_assertion_authority = _validate_attested_authority_shape(
        receipt["assertion"], "release reconciliation assertion"
    )
    embedded_preauth_authority = _validate_attested_authority_shape(
        receipt["preauthorization"], "release reconciliation preauthorization"
    )
    source_assertion = validate_assertion(assertion, now=completed_at)
    source_preauthorization = finalizer.validate_finalization_authorization(
        preauthorization
    )
    source_assertion_authority = finalizer.validate_attested_artifact_authority(
        assertion_authority,
        subject=source_assertion,
        workflow_path=WORKFLOW_PATH,
        predicate_type=ASSERTION_PREDICATE_TYPE,
        artifact_name=(
            f"production-orphan-lock-release-assertion-"
            f"{source_assertion['control']['run_id']}-1"
        ),
        label="release reconciliation source assertion authority",
    )
    source_preauth_authority = finalizer.validate_attested_artifact_authority(
        preauthorization_authority,
        subject=source_preauthorization,
        workflow_path=finalizer.WORKFLOW_PATH,
        predicate_type=finalizer.PREDICATE_TYPE,
        artifact_name=(
            f"{PREAUTH_PREFIX}-{source_preauthorization['control']['run_id']}-1"
        ),
        label="release reconciliation source preauthorization authority",
    )
    if (
        embedded_assertion_authority != source_assertion_authority
        or embedded_preauth_authority != source_preauth_authority
        or source_assertion["preauthorization"] != source_preauth_authority
        or source_assertion["orphan"]["preauthorization_sha256"]
        != common.sha256_bytes(
            common.canonical_file_bytes(source_preauthorization)
        )
        or source_assertion["orphan"]["root_acquire_intent"]
        != source_preauthorization["orphan"]["root_acquire_intent"]
        or source_assertion["orphan"]["main_sha"]
        != source_preauthorization["orphan"]["current_main_sha"]
        or source_assertion["orphan"]["rule_id"]
        != source_preauthorization["orphan"]["rule_id"]
        or source_assertion["orphan"]["rule_identity_sha256"]
        != source_preauthorization["orphan"]["rule_identity_sha256"]
    ):
        common.fail("release reconciliation source authority chain differs")
    assertion = embedded_assertion_authority
    preauth = embedded_preauth_authority
    if (
        assertion["signer_workflow"] != f"{common.REPOSITORY}/{WORKFLOW_PATH}"
        or assertion["signer_digest"] != control["workflow_sha"]
        or assertion["source_digest"] != control["workflow_sha"]
        or assertion["source_ref"] != "refs/heads/main"
        or assertion["runner_environment"] != "github-hosted"
        or assertion["provenance_predicate_type"] != finalizer.PROVENANCE_PREDICATE_TYPE
        or assertion["policy_predicate_type"] != ASSERTION_PREDICATE_TYPE
        or assertion["binding"]["run_id"] != control["run_id"]
        or assertion["binding"]["run_attempt"] != control["run_attempt"]
        or assertion["binding"]["artifact_name"]
        != f"production-orphan-lock-release-assertion-{control['run_id']}-1"
        or preauth["signer_workflow"]
        != f"{common.REPOSITORY}/{finalizer.WORKFLOW_PATH}"
        or preauth["signer_digest"] != control["workflow_sha"]
        or preauth["source_digest"] != control["workflow_sha"]
        or preauth["source_ref"] != "refs/heads/main"
        or preauth["runner_environment"] != "github-hosted"
        or preauth["provenance_predicate_type"] != finalizer.PROVENANCE_PREDICATE_TYPE
        or preauth["policy_predicate_type"] != finalizer.PREDICATE_TYPE
    ):
        common.fail("release reconciliation attested authority differs")
    classification = common.exact_keys(
        receipt["classification"],
        {
            "outcome", "terminal", "signed_release_artifact_present_at_observation",
            "source_receipt_classification",
            "mutation_count_observed", "original_static_max_branch_mutations",
        },
        "release reconciliation classification",
    )
    expected_classification = receipt["observation"]["source_receipt_truth"][
        "classification"
    ] if type(receipt["observation"]) is dict and type(receipt["observation"].get("source_receipt_truth")) is dict else None
    if classification != {
        "outcome": OUTCOME,
        "terminal": True,
        "signed_release_artifact_present_at_observation": False,
        "source_receipt_classification": expected_classification,
        "mutation_count_observed": None,
        "original_static_max_branch_mutations": 1,
    }:
        common.fail("release reconciliation classification differs")
    orphan = common.exact_keys(
        receipt["orphan"],
        {
            "main_sha", "rule_id", "rule_identity_sha256", "root_acquire_intent",
            "preauthorization_sha256",
        },
        "release reconciliation orphan",
    )
    common.require_sha1(orphan["main_sha"], "release reconciliation main SHA")
    rule_id = _rule_id(orphan["rule_id"], "release reconciliation rule ID")
    if common.sha256_bytes(rule_id.encode()) != common.require_sha256(
        orphan["rule_identity_sha256"], "release reconciliation rule hash"
    ):
        common.fail("release reconciliation rule hash differs")
    root = _binding(orphan["root_acquire_intent"], "release reconciliation root")
    if root["artifact_name"] not in {
        f"production-main-lock-apply-{root['run_id']}-1",
        f"production-main-lock-rollback-{root['run_id']}-1",
    }:
        common.fail("release reconciliation root identity differs")
    if orphan["preauthorization_sha256"] != preauth["binding"]["sha256"]:
        common.fail("release reconciliation preauthorization hash differs")
    original = _original_finalizer(
        receipt["original_finalizer"], preauthorization_binding=preauth["binding"]
    )
    if (
        original["workflow_sha"] != control["workflow_sha"]
        or receipt["orphan"] != source_assertion["orphan"]
        or receipt["original_finalizer"] != source_assertion["original_finalizer"]
        or control["workflow_sha"] != source_assertion["control"]["workflow_sha"]
        or str(control["run_id"]) != str(source_assertion["control"]["run_id"])
        or control["run_attempt"] != source_assertion["control"]["run_attempt"]
        or control["assertion_schema_sha256"]
        != source_assertion["control"]["assertion_schema_sha256"]
        or control["controller_sha256"]
        != source_assertion["control"]["controller_sha256"]
    ):
        common.fail("release reconciliation source control differs")
    observation = validate_observation_request(
        receipt["observation"], source_artifacts=original["artifacts"]
    )
    if not _same_receipt_truth(
        observation["source_receipt_truth"], original["receipt_truth"]
    ):
        common.fail("release reconciliation source receipt truth changed")
    _require_truth_window(
        observation["source_receipt_truth"],
        not_before=common.require_timestamp(source_assertion["created_at"], "assertion creation"),
        not_after=completed_at,
        label="observed source receipt truth",
    )
    observed = observation["observation_rounds"][0]
    if (
        observed["main_sha"] != orphan["main_sha"]
        or observed["rule"]["rule_id"] != orphan["rule_id"]
        or observed["rule"]["rule_identity_sha256"] != orphan["rule_identity_sha256"]
        or {
            "lock_branch": observed["rule"]["lock_branch"],
            "is_admin_enforced": observed["rule"]["is_admin_enforced"],
            "lock_allows_fetch_and_merge": observed["rule"]["lock_allows_fetch_and_merge"],
        }
        != EXPECTED_UNLOCKED
    ):
        common.fail("release reconciliation observed authority differs")
    if receipt["gates"] != {
        "assertion_authenticated": True,
        "preauthorization_authenticated": True,
        "main_unchanged": True,
        "rule_identity_unchanged": True,
        "double_read_complete": True,
        "branch_mutation_absent": True,
        "lock_observed_released": True,
    }:
        common.fail("release reconciliation gates differ")
    if receipt["execution_boundary"] != {
        "controller_network_access": False,
        "provider_network_access": False,
        "branch_mutation_request_count": 0,
        "provider_token_present": False,
        "branch_write_token_present": False,
        "observation_only": True,
    }:
        common.fail("release reconciliation execution boundary differs")
    common.sanitize_public(receipt, allowed_keys=("completed_at",))
    return receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Observation-only orphan lock-release reconciliation")
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare-assertion")
    for name in (
        "request", "preauthorization", "preauthorization-authority",
        "assertion-schema", "controller-sha256", "workflow-sha",
        "workflow-run-id", "runner-temp", "output",
    ):
        prepare.add_argument(f"--{name}", required=True)
    prepare.add_argument("--workflow-run-attempt", required=True, type=int)
    reconcile = commands.add_parser("reconcile")
    for name in (
        "observation-request", "assertion", "assertion-authority",
        "preauthorization", "preauthorization-authority", "assertion-schema",
        "reconciliation-schema", "controller-sha256", "workflow-sha",
        "workflow-run-id", "runner-temp", "output",
    ):
        reconcile.add_argument(f"--{name}", required=True)
    reconcile.add_argument("--workflow-run-attempt", required=True, type=int)
    validate_assertion_parser = commands.add_parser("validate-assertion")
    validate_assertion_parser.add_argument("--assertion", required=True)
    validate_assertion_parser.add_argument("--sha256", required=True)
    validate_reconciliation_parser = commands.add_parser("validate-reconciliation")
    validate_reconciliation_parser.add_argument("--reconciliation", required=True)
    validate_reconciliation_parser.add_argument("--sha256", required=True)
    for name in (
        "assertion", "assertion-authority", "preauthorization",
        "preauthorization-authority",
    ):
        validate_reconciliation_parser.add_argument(f"--{name}", required=True)
    return parser


def _validate_file(path: Path, expected_sha256: str, label: str, validator: Any) -> int:
    value = common.load_json(path, label)
    if common.sha256_bytes(path.read_bytes()) != common.require_sha256(
        expected_sha256, f"{label} expected hash"
    ):
        common.fail(f"{label} exact-file hash differs")
    validator(value)
    return 0


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    if args.command == "validate-assertion":
        return _validate_file(
            Path(args.assertion), args.sha256, "release reconciliation assertion",
            validate_assertion,
        )
    if args.command == "validate-reconciliation":
        path = Path(args.reconciliation)
        value = common.load_json(path, "release reconciliation receipt")
        if common.sha256_bytes(path.read_bytes()) != common.require_sha256(
            args.sha256, "release reconciliation receipt expected hash"
        ):
            common.fail("release reconciliation receipt exact-file hash differs")
        validate_reconciliation(
            value,
            assertion=common.load_json(Path(args.assertion), "release assertion"),
            assertion_authority=common.load_json(
                Path(args.assertion_authority), "release assertion authority"
            ),
            preauthorization=common.load_json(
                Path(args.preauthorization), "orphan-lock preauthorization"
            ),
            preauthorization_authority=common.load_json(
                Path(args.preauthorization_authority), "preauthorization authority"
            ),
        )
        return 0
    assertion_schema = Path(args.assertion_schema)
    common.load_json(assertion_schema, "release assertion schema")
    controller_sha256 = common.require_sha256(args.controller_sha256, "controller hash")
    if args.command == "prepare-assertion":
        assertion = build_assertion(
            request=common.load_json(Path(args.request), "release assertion request"),
            preauthorization=common.load_json(
                Path(args.preauthorization), "orphan-lock preauthorization"
            ),
            preauthorization_authority=common.load_json(
                Path(args.preauthorization_authority), "preauthorization authority"
            ),
            control=_assertion_control(
                workflow_sha=args.workflow_sha,
                workflow_run_id=args.workflow_run_id,
                workflow_run_attempt=args.workflow_run_attempt,
                assertion_schema_sha256=common.sha256_bytes(assertion_schema.read_bytes()),
                controller_sha256=controller_sha256,
            ),
            created_at=dt.datetime.now(dt.timezone.utc),
        )
        common.write_canonical_output(Path(args.output), assertion, Path(args.runner_temp))
        return 0
    reconciliation_schema = Path(args.reconciliation_schema)
    common.load_json(reconciliation_schema, "release reconciliation schema")
    receipt = build_reconciliation(
        observation_request=common.load_json(
            Path(args.observation_request), "release observation request"
        ),
        assertion=common.load_json(Path(args.assertion), "release assertion"),
        assertion_authority=common.load_json(
            Path(args.assertion_authority), "release assertion authority"
        ),
        preauthorization=common.load_json(
            Path(args.preauthorization), "orphan-lock preauthorization"
        ),
        preauthorization_authority=common.load_json(
            Path(args.preauthorization_authority), "preauthorization authority"
        ),
        control=_reconciliation_control(
            workflow_sha=args.workflow_sha,
            workflow_run_id=args.workflow_run_id,
            workflow_run_attempt=args.workflow_run_attempt,
            assertion_schema_sha256=common.sha256_bytes(assertion_schema.read_bytes()),
            reconciliation_schema_sha256=common.sha256_bytes(
                reconciliation_schema.read_bytes()
            ),
            controller_sha256=controller_sha256,
        ),
        completed_at=dt.datetime.now(dt.timezone.utc),
    )
    common.write_canonical_output(Path(args.output), receipt, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production orphan lock-release reconciliation failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
