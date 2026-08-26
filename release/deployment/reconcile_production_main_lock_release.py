#!/usr/bin/env python3
"""Certify a normal apply/rollback lock release after its unlock runner died.

This controller is offline and mutation-free.  It accepts only an exact
attempt-one source run where every pre-unlock job succeeded and the unlock job
is the sole incomplete terminal job.  Three identical read-only branch
observations are required before a reconciliation can be signed.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import os
import re
import stat
import sys
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import authorize_production_main_lock_release as release_authorization
import rollback_production_change as rollback_control
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/reconcile-production-main-lock-release.yml"
WORKFLOW_NAME = "Reconcile Production Main Lock Release"
ASSERTION_AUTHORITY = "production-main-lock-release-failure-assertion"
ASSERTION_PREDICATE = "https://rereply.app/attestations/production-main-lock-release-failure-assertion/v1"
RECONCILIATION_AUTHORITY = "production-main-lock-release-reconciliation"
RECONCILIATION_PREDICATE = "https://rereply.app/attestations/production-main-lock-release-reconciliation/v1"
AUTHORIZATION_PREDICATE = release_authorization.PREDICATE_TYPE
PROVENANCE_PREDICATE = "https://slsa.dev/provenance/v1"
MAX_ASSERTION_AGE_SECONDS = 600
MAX_SOURCE_RECOVERY_AGE_SECONDS = 1800
TERMINAL_FAILURES = {"failure", "cancelled", "timed_out"}
UNLOCK_STEP_TERMINALS = {"success", *TERMINAL_FAILURES}
RULE_ID_RE = re.compile(r"^[A-Za-z0-9_+/=-]{1,512}$")


def _binding(value: Any, label: str) -> dict[str, Any]:
    binding = copy.deepcopy(common.validate_full_artifact_binding(value, label))
    binding["run_id"] = str(binding["run_id"])
    binding["artifact_id"] = str(binding["artifact_id"])
    return binding


def _attested(
    value: Any,
    *,
    label: str,
    expected_workflow: str,
    expected_sha: str,
    expected_predicate: str,
    expected_binding: Mapping[str, Any],
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
    binding = _binding(authority["binding"], f"{label} binding")
    if (
        binding != dict(expected_binding)
        or authority["signer_workflow"] != f"{common.REPOSITORY}/{expected_workflow}"
        or authority["signer_digest"] != expected_sha
        or authority["source_digest"] != expected_sha
        or authority["source_ref"] != "refs/heads/main"
        or authority["runner_environment"] != "github-hosted"
        or authority["provenance_predicate_type"] != PROVENANCE_PREDICATE
        or authority["policy_predicate_type"] != expected_predicate
    ):
        common.fail(f"{label} differs")
    for key in ("provenance_verification_sha256", "policy_verification_sha256"):
        common.require_sha256(authority[key], f"{label} {key}")
    normalized = copy.deepcopy(dict(authority))
    normalized["binding"] = binding
    return normalized


def _control(value: Any, label: str) -> dict[str, Any]:
    control = common.exact_keys(
        value,
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "assertion_schema_sha256",
            "reconciliation_schema_sha256", "controller_sha256",
        },
        label,
    )
    normalized = {
        "workflow_sha": common.require_sha1(control["workflow_sha"], f"{label} SHA"),
        "workflow_path": common.exact_string(control["workflow_path"], f"{label} path"),
        "run_id": common.require_run_id(control["run_id"], f"{label} run ID"),
        "run_attempt": common.exact_int(control["run_attempt"], f"{label} attempt", 1, 1),
        "runner_environment": common.exact_string(control["runner_environment"], f"{label} runner"),
        "assertion_schema_sha256": common.require_sha256(control["assertion_schema_sha256"], f"{label} assertion schema"),
        "reconciliation_schema_sha256": common.require_sha256(control["reconciliation_schema_sha256"], f"{label} reconciliation schema"),
        "controller_sha256": common.require_sha256(control["controller_sha256"], f"{label} controller"),
    }
    if normalized["workflow_path"] != WORKFLOW_PATH or normalized["runner_environment"] != "github-hosted":
        common.fail(f"{label} workflow identity differs")
    return normalized


def _job(value: Any, expected_name: str) -> dict[str, Any]:
    job = common.exact_keys(
        value,
        {"job_id", "name", "status", "conclusion", "started_at", "completed_at"},
        f"source job {expected_name}",
    )
    if job["name"] != expected_name or job["status"] != "completed":
        common.fail("source job identity differs")
    started = common.require_timestamp(job["started_at"], f"{expected_name} start")
    completed = common.require_timestamp(job["completed_at"], f"{expected_name} completion")
    if completed < started:
        common.fail("source job timing differs")
    normalized = copy.deepcopy(dict(job))
    normalized["job_id"] = common.require_run_id(job["job_id"], f"{expected_name} job ID")
    normalized["conclusion"] = common.exact_string(job["conclusion"], f"{expected_name} conclusion")
    return normalized


def _receipt(operation: str, value: Any) -> dict[str, Any]:
    try:
        if operation == "apply":
            return common.validate_apply_receipt(value)
        return rollback_control.validate_rollback_receipt(value)
    except Exception as exc:
        raise common.ReleaseError("source receipt validation failed") from exc


def _unlock_steps(
    value: Any,
    *,
    operation: str,
    authorization: Mapping[str, Any] | None = None,
) -> list[dict[str, Any]]:
    """Validate the exact unlock job step lineage returned by the Jobs API.

    GitHub assigns non-contiguous step numbers once post actions are included,
    so the signed inventory binds exact names and ordering while requiring only
    strictly increasing positive numbers.  A committed/read-back unlock can be
    followed by a runner or post-action failure, therefore the write step may
    itself be successful as long as a later synthetic step explains the failed
    source job.
    """

    expected_steps = [
        "Set up job",
        "Check out exact protected controls",
        "Install pinned unlock verification tools",
        f"Download exact signed {operation} release authorization",
        f"Download exact signed {operation} receipt for release",
        f"Authenticate exact {operation} pre-unlock authority",
        f"Release the exact authorized {operation} main lock",
        "Post Check out exact protected controls",
        "Complete job",
    ]
    if type(value) is not list or len(value) != len(expected_steps):
        common.fail("unlock step inventory differs")

    normalized: list[dict[str, Any]] = []
    prior_number = 0
    for item, expected_name in zip(value, expected_steps, strict=True):
        step = common.exact_keys(
            item,
            {"number", "name", "status", "conclusion", "started_at", "completed_at"},
            f"unlock step {expected_name}",
        )
        number = common.exact_int(step["number"], f"{expected_name} number", 1, 1000)
        if number <= prior_number or step["name"] != expected_name:
            common.fail("unlock step identity differs")
        prior_number = number
        if step["status"] != "completed":
            common.fail("unlock step status differs")
        started = None
        completed = None
        if step["started_at"] is not None:
            started = common.require_timestamp(step["started_at"], f"{expected_name} started_at")
        if step["completed_at"] is not None:
            completed = common.require_timestamp(step["completed_at"], f"{expected_name} completed_at")
        if started is not None and completed is not None and completed < started:
            common.fail("unlock step timing differs")
        normalized.append(copy.deepcopy(dict(step)))

    # Setup through authentication are the exact pre-write lineage.  All must
    # have completed successfully and carry real timing evidence.
    for step in normalized[:6]:
        if (
            step["conclusion"] != "success"
            or step["started_at"] is None
            or step["completed_at"] is None
        ):
            common.fail("unlock pre-write step did not succeed")

    release_step = normalized[6]
    if release_step["conclusion"] not in UNLOCK_STEP_TERMINALS or release_step["started_at"] is None:
        common.fail("unlock write step did not start")
    if release_step["conclusion"] == "success" and release_step["completed_at"] is None:
        common.fail("successful unlock write step has no completion evidence")

    # Post-checkout and Complete-job are synthetic runner steps.  They can be
    # successful, failed, cancelled, or skipped.  If the write step succeeded,
    # at least one later step must be non-success to explain the failed job.
    for step in normalized[7:]:
        if step["conclusion"] not in {"success", "failure", "cancelled", "skipped", "timed_out"}:
            common.fail("unlock synthetic step conclusion differs")
        if step["conclusion"] == "success" and (
            step["started_at"] is None or step["completed_at"] is None
        ):
            common.fail("successful unlock synthetic step lacks timing evidence")
    if release_step["conclusion"] == "success" and all(
        step["conclusion"] == "success" for step in normalized[7:]
    ):
        common.fail("failed unlock job has no terminal failure step")

    if authorization is not None:
        issued = common.require_timestamp(
            authorization["issued_at"], "release authorization issued_at"
        )
        expires = common.require_timestamp(
            authorization["expires_at"], "release authorization expires_at"
        )
        release_started = common.require_timestamp(
            release_step["started_at"], "unlock write step start"
        )
        if release_started < issued or release_started >= expires:
            common.fail("unlock write step started outside authorization window")
    return normalized


def _source(
    value: Any,
    *,
    operation: str,
    authorization: Mapping[str, Any],
    receipt: Mapping[str, Any],
) -> dict[str, Any]:
    source = common.exact_keys(
        value,
        {
            "workflow_sha", "workflow_path", "workflow_name", "run_id",
            "run_attempt", "event", "head_branch", "status", "conclusion",
            "jobs", "job_inventory_sha256", "artifacts",
            "artifact_inventory_sha256", "unlock_steps",
            "unlock_step_inventory_sha256", "authorization_job_inventory_sha256",
        },
        "failed normal release source",
    )
    policy = release_authorization.WORKFLOWS[operation]
    run_id = common.require_run_id(source["run_id"], "source run ID")
    if (
        source["workflow_sha"] != authorization["control"]["workflow_sha"]
        or source["workflow_path"] != policy["path"]
        or source["workflow_name"] != ("Apply Production Phase" if operation == "apply" else "Rollback Production Phase")
        or source["run_attempt"] != 1
        or source["event"] != "workflow_dispatch"
        or source["head_branch"] != "main"
        or source["status"] != "completed"
        or source["conclusion"] not in TERMINAL_FAILURES
        or run_id != authorization["control"]["run_id"]
    ):
        common.fail("failed source run identity differs")
    expected_jobs = [*policy["jobs"], f"Release exact production {operation} main lock"]
    if type(source["jobs"]) is not list or len(source["jobs"]) != 9:
        common.fail("failed source job inventory differs")
    jobs = [
        _job(item, expected)
        for item, expected in zip(source["jobs"], expected_jobs, strict=True)
    ]
    if any(item["conclusion"] != "success" for item in jobs[:-1]):
        common.fail("a pre-unlock source job did not succeed")
    if jobs[-1]["conclusion"] not in TERMINAL_FAILURES:
        common.fail("unlock is not the sole incomplete terminal job")
    if source["job_inventory_sha256"] != common.sha256_value(jobs):
        common.fail("failed source job inventory hash differs")
    signed_jobs = authorization["jobs"]
    if source["authorization_job_inventory_sha256"] != common.sha256_value(signed_jobs):
        common.fail("release authorization job inventory hash differs")
    if any(
        actual["job_id"] != signed["job_id"] or actual["name"] != signed["name"]
        for actual, signed in zip(jobs[:-1], signed_jobs, strict=True)
    ):
        common.fail("release authorization job cross-splice detected")

    unlock_steps = _unlock_steps(
        source["unlock_steps"], operation=operation, authorization=authorization
    )
    if source["unlock_step_inventory_sha256"] != common.sha256_value(unlock_steps):
        common.fail("unlock step inventory hash differs")

    artifacts = common.exact_keys(
        source["artifacts"],
        {
            "acquire_intent", "mutation_intent", "main_lock_proof",
            "unsigned_receipt", "signed_receipt", "release_authorization",
        },
        "failed source artifact inventory",
    )
    normalized_artifacts = {
        key: _binding(item, f"failed source {key}") for key, item in artifacts.items()
    }
    for key, expected in authorization["artifacts"].items():
        if normalized_artifacts[key] != expected:
            common.fail("failed source artifact cross-splice detected")
    expected_auth = normalized_artifacts["release_authorization"]
    if (
        expected_auth["run_id"] != run_id
        or expected_auth["artifact_name"]
        != f"production-main-lock-release-authorization-{operation}-{run_id}-1"
        or expected_auth["sha256"]
        != common.sha256_bytes(common.canonical_file_bytes(authorization))
    ):
        common.fail("failed source release authorization binding differs")
    expected_receipt = normalized_artifacts["signed_receipt"]
    if expected_receipt["sha256"] != common.sha256_bytes(common.canonical_file_bytes(receipt)):
        common.fail("failed source receipt binding differs")
    if source["artifact_inventory_sha256"] != common.sha256_value(normalized_artifacts):
        common.fail("failed source artifact inventory hash differs")
    normalized = copy.deepcopy(dict(source))
    normalized["run_id"] = run_id
    normalized["jobs"] = jobs
    normalized["unlock_steps"] = unlock_steps
    normalized["artifacts"] = normalized_artifacts
    return normalized


def build_assertion(
    request: Any,
    *,
    authorization: Any,
    receipt: Any,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    request = common.exact_keys(
        request,
        {"operation", "issued_at", "control", "source", "authorization_attestations", "receipt_attestations"},
        "release-failure assertion request",
    )
    operation = common.exact_string(request["operation"], "release operation")
    if operation not in release_authorization.WORKFLOWS:
        common.fail("release operation differs")
    issued = common.require_timestamp(request["issued_at"], "assertion issue time")
    expires = issued + dt.timedelta(seconds=MAX_ASSERTION_AGE_SECONDS)
    if now is not None:
        checked = now.astimezone(dt.timezone.utc)
        if issued > checked or checked >= expires:
            common.fail("release-failure assertion is expired")
    control = _control(request["control"], "assertion control")
    receipt = _receipt(operation, receipt)
    release_authorization.validate_authorization(authorization, receipt=receipt)
    source = _source(request["source"], operation=operation, authorization=authorization, receipt=receipt)
    source_completed = common.require_timestamp(
        source["jobs"][-1]["completed_at"], "failed unlock job completion"
    )
    if issued < source_completed or issued >= source_completed + dt.timedelta(
        seconds=MAX_SOURCE_RECOVERY_AGE_SECONDS
    ):
        common.fail("failed normal release is outside the recovery window")
    auth_binding = source["artifacts"]["release_authorization"]
    receipt_binding = source["artifacts"]["signed_receipt"]
    auth_authority = _attested(
        request["authorization_attestations"],
        label="release authorization attestations",
        expected_workflow=source["workflow_path"],
        expected_sha=source["workflow_sha"],
        expected_predicate=AUTHORIZATION_PREDICATE,
        expected_binding=auth_binding,
    )
    receipt_authority = _attested(
        request["receipt_attestations"],
        label="original receipt attestations",
        expected_workflow=source["workflow_path"],
        expected_sha=source["workflow_sha"],
        expected_predicate=release_authorization.WORKFLOWS[operation]["receipt_predicate"],
        expected_binding=receipt_binding,
    )
    if control["workflow_sha"] != source["workflow_sha"]:
        common.fail("assertion main differs from failed source")
    assertion = {
        "schema_version": 1,
        "authority": ASSERTION_AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": request["issued_at"],
        "expires_at": expires.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "state": "failed-source-unlock-observation-authorized",
        "operation": operation,
        "control": control,
        "source": source,
        "branch": copy.deepcopy(authorization["branch"]),
        "authorities": {
            "release_authorization": auth_authority,
            "original_receipt": receipt_authority,
        },
        "authorization_scope": "observe-normal-main-lock-release-only",
        "static_max_branch_mutations": 0,
    }
    common.sanitize_public(assertion)
    return assertion


def validate_assertion(value: Any, *, now: dt.datetime | None = None) -> dict[str, Any]:
    assertion = common.exact_keys(
        value,
        {"schema_version", "authority", "repository", "issued_at", "expires_at", "state", "operation", "control", "source", "branch", "authorities", "authorization_scope", "static_max_branch_mutations"},
        "release-failure assertion",
    )
    if (
        assertion["schema_version"] != 1
        or assertion["authority"] != ASSERTION_AUTHORITY
        or assertion["repository"] != common.REPOSITORY
        or assertion["state"] != "failed-source-unlock-observation-authorized"
        or assertion["authorization_scope"] != "observe-normal-main-lock-release-only"
        or assertion["static_max_branch_mutations"] != 0
    ):
        common.fail("release-failure assertion identity differs")
    issued = common.require_timestamp(assertion["issued_at"], "assertion issue time")
    expires = common.require_timestamp(assertion["expires_at"], "assertion expiry time")
    if expires != issued + dt.timedelta(seconds=MAX_ASSERTION_AGE_SECONDS):
        common.fail("assertion expiry differs")
    if now is not None:
        checked = now.astimezone(dt.timezone.utc)
        if issued > checked or checked >= expires:
            common.fail("release-failure assertion is expired")
    _control(assertion["control"], "assertion control")
    operation = assertion["operation"]
    if operation not in release_authorization.WORKFLOWS:
        common.fail("assertion operation differs")
    source = assertion["source"]
    if type(source) is not dict or source.get("conclusion") not in TERMINAL_FAILURES:
        common.fail("assertion source differs")
    jobs = source.get("jobs")
    if type(jobs) is not list or len(jobs) != 9:
        common.fail("assertion source job inventory differs")
    source_completed = common.require_timestamp(
        jobs[-1]["completed_at"], "assertion failed unlock job completion"
    )
    if issued < source_completed or issued >= source_completed + dt.timedelta(
        seconds=MAX_SOURCE_RECOVERY_AGE_SECONDS
    ):
        common.fail("assertion failed source recovery window differs")
    branch = assertion["branch"]
    if (
        type(branch) is not dict
        or branch.get("main_sha") != assertion["control"]["workflow_sha"]
        or branch.get("lock_branch") is not True
        or branch.get("is_admin_enforced") is not True
        or branch.get("lock_allows_fetch_and_merge") is not False
        or RULE_ID_RE.fullmatch(str(branch.get("rule_id", ""))) is None
    ):
        common.fail("assertion branch differs")
    authorities = common.exact_keys(assertion["authorities"], {"release_authorization", "original_receipt"}, "assertion authorities")
    for label, item in authorities.items():
        common.validate_full_artifact_binding(item["binding"], f"assertion {label}")
    common.sanitize_public(assertion)
    return assertion


def _observation(value: Any, *, round_number: int, assertion: Mapping[str, Any]) -> dict[str, Any]:
    item = common.exact_keys(
        value,
        {"round", "observed_at", "main_sha", "rule_id", "lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge", "http_method", "api_operation"},
        f"release observation {round_number}",
    )
    common.exact_int(item["round"], "release observation round", round_number, round_number)
    observed = common.require_timestamp(item["observed_at"], "release observation time")
    if (
        item["main_sha"] != assertion["control"]["workflow_sha"]
        or item["rule_id"] != assertion["branch"]["rule_id"]
        or item["lock_branch"] is not False
        or item["is_admin_enforced"] is not True
        or item["lock_allows_fetch_and_merge"] is not False
        or item["http_method"] != "POST"
        or item["api_operation"] != "graphql-query"
    ):
        common.fail("release observation differs")
    issued = common.require_timestamp(assertion["issued_at"], "assertion issue time")
    expires = common.require_timestamp(assertion["expires_at"], "assertion expiry time")
    if observed < issued or observed >= expires:
        common.fail("release observation is outside assertion authority")
    return copy.deepcopy(dict(item))


def build_reconciliation(
    request: Any,
    *,
    assertion: Any,
    assertion_authority: Any,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    request = common.exact_keys(
        request,
        {
            "completed_at", "control", "observations", "request_ledger",
            "http_request_count", "graphql_query_count",
            "branch_mutation_request_count", "mutation_text_present",
        },
        "lock-release reconciliation request",
    )
    assertion = validate_assertion(assertion, now=now)
    control = _control(request["control"], "reconciliation control")
    if control != assertion["control"]:
        common.fail("reconciliation control differs from assertion")
    completed = common.require_timestamp(request["completed_at"], "reconciliation completion time")
    expires = common.require_timestamp(assertion["expires_at"], "assertion expiry time")
    if now is not None:
        checked = now.astimezone(dt.timezone.utc)
        if completed > checked or checked >= expires:
            common.fail("reconciliation completed outside authority")
    if type(request["observations"]) is not list or len(request["observations"]) != 3:
        common.fail("reconciliation observation inventory differs")
    observations = [
        _observation(item, round_number=index, assertion=assertion)
        for index, item in enumerate(request["observations"], start=1)
    ]
    if not (observations[0]["observed_at"] <= observations[1]["observed_at"] <= observations[2]["observed_at"] <= request["completed_at"]):
        common.fail("reconciliation observation ordering differs")
    projection_keys = {"round", "observed_at"}
    projections = [{k: v for k, v in item.items() if k not in projection_keys} for item in observations]
    if not (projections[0] == projections[1] == projections[2]):
        common.fail("reconciliation branch reads differ")
    expected_ledger = [
        item
        for round_number in range(1, 4)
        for item in (
            {"round": round_number, "label": "main-ref-before", "http_method": "GET", "api_operation": "rest-read"},
            {"round": round_number, "label": "branch-rule", "http_method": "POST", "api_operation": "graphql-query"},
            {"round": round_number, "label": "main-ref-after", "http_method": "GET", "api_operation": "rest-read"},
        )
    ]
    if (
        request["request_ledger"] != expected_ledger
        or request["http_request_count"] != 9
        or request["graphql_query_count"] != 3
        or request["branch_mutation_request_count"] != 0
        or request["mutation_text_present"] is not False
    ):
        common.fail("reconciliation read-only request ledger differs")
    assertion_binding = _binding(assertion_authority["binding"], "assertion authority binding")
    expected_assertion_name = f"production-main-lock-release-failure-assertion-{control['run_id']}-1"
    if (
        assertion_binding["run_id"] != control["run_id"]
        or assertion_binding["artifact_name"] != expected_assertion_name
        or assertion_binding["sha256"] != common.sha256_bytes(common.canonical_file_bytes(assertion))
    ):
        common.fail("reconciliation assertion binding differs")
    normalized_assertion_authority = _attested(
        assertion_authority,
        label="signed failure assertion",
        expected_workflow=WORKFLOW_PATH,
        expected_sha=control["workflow_sha"],
        expected_predicate=ASSERTION_PREDICATE,
        expected_binding=assertion_binding,
    )
    receipt = {
        "schema_version": 1,
        "authority": RECONCILIATION_AUTHORITY,
        "repository": common.REPOSITORY,
        "completed_at": request["completed_at"],
        "state": "observed-unlocked-after-incomplete-normal-release",
        "operation": assertion["operation"],
        "control": control,
        "source": copy.deepcopy(assertion["source"]),
        "branch": {
            "main_sha": assertion["branch"]["main_sha"],
            "rule_id": assertion["branch"]["rule_id"],
            "rule_identity_sha256": assertion["branch"]["rule_identity_sha256"],
            "expected_before": {
                "lock_branch": True,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "observed_after": {
                "lock_branch": False,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
        },
        "authorities": {
            "failure_assertion": normalized_assertion_authority,
            "release_authorization": copy.deepcopy(assertion["authorities"]["release_authorization"]),
            "original_receipt": copy.deepcopy(assertion["authorities"]["original_receipt"]),
        },
        "observations": observations,
        "request_ledger": copy.deepcopy(request["request_ledger"]),
        "http_request_count": request["http_request_count"],
        "graphql_query_count": request["graphql_query_count"],
        "branch_mutation_request_count": request["branch_mutation_request_count"],
        "mutation_text_present": request["mutation_text_present"],
        "gates": {
            "source_attempt_one": True,
            "source_terminal_failure": True,
            "pre_unlock_jobs_succeeded": True,
            "unlock_sole_incomplete_terminal_job": True,
            "exact_artifacts_verified": True,
            "dual_attested_release_authorization_verified": True,
            "dual_attested_original_receipt_verified": True,
            "main_unchanged": True,
            "unique_rule_observed_unlocked": True,
            "read_only": True,
        },
        "mutation_request_count": 0,
        "canary_eligible": True,
    }
    common.sanitize_public(receipt)
    return receipt


def validate_reconciliation(value: Any) -> dict[str, Any]:
    receipt = common.exact_keys(
        value,
        {"schema_version", "authority", "repository", "completed_at", "state", "operation", "control", "source", "branch", "authorities", "observations", "request_ledger", "http_request_count", "graphql_query_count", "branch_mutation_request_count", "mutation_text_present", "gates", "mutation_request_count", "canary_eligible"},
        "normal lock-release reconciliation",
    )
    if (
        receipt["schema_version"] != 1
        or receipt["authority"] != RECONCILIATION_AUTHORITY
        or receipt["repository"] != common.REPOSITORY
        or receipt["state"] != "observed-unlocked-after-incomplete-normal-release"
        or receipt["operation"] not in release_authorization.WORKFLOWS
        or receipt["mutation_request_count"] != 0
        or receipt["canary_eligible"] is not True
    ):
        common.fail("normal lock-release reconciliation identity differs")
    control = _control(receipt["control"], "reconciliation control")
    completed = common.require_timestamp(receipt["completed_at"], "reconciliation completion")
    source = receipt["source"]
    operation = receipt["operation"]
    policy = release_authorization.WORKFLOWS[operation]
    expected_job_names = [*policy["jobs"], f"Release exact production {operation} main lock"]
    if (
        type(source) is not dict
        or set(source) != {
            "workflow_sha", "workflow_path", "workflow_name", "run_id", "run_attempt",
            "event", "head_branch", "status", "conclusion", "jobs",
            "job_inventory_sha256", "authorization_job_inventory_sha256",
            "unlock_steps", "unlock_step_inventory_sha256",
            "artifacts", "artifact_inventory_sha256",
        }
        or source["workflow_sha"] != control["workflow_sha"]
        or source["workflow_path"] != policy["path"]
        or source["workflow_name"]
        != ("Apply Production Phase" if operation == "apply" else "Rollback Production Phase")
        or source["run_attempt"] != 1
        or source["event"] != "workflow_dispatch"
        or source["head_branch"] != "main"
        or source["status"] != "completed"
        or source["conclusion"] not in TERMINAL_FAILURES
    ):
        common.fail("reconciliation source identity differs")
    if type(source["jobs"]) is not list or len(source["jobs"]) != 9:
        common.fail("reconciliation source jobs differ")
    jobs = [_job(item, name) for item, name in zip(source["jobs"], expected_job_names, strict=True)]
    if any(item["conclusion"] != "success" for item in jobs[:-1]) or jobs[-1]["conclusion"] not in TERMINAL_FAILURES:
        common.fail("reconciliation source job conclusions differ")
    if source["job_inventory_sha256"] != common.sha256_value(jobs):
        common.fail("reconciliation job inventory hash differs")
    common.require_sha256(
        source["authorization_job_inventory_sha256"],
        "reconciliation authorization job inventory hash",
    )
    artifacts = common.exact_keys(source["artifacts"], {"acquire_intent", "mutation_intent", "main_lock_proof", "unsigned_receipt", "signed_receipt", "release_authorization"}, "reconciliation source artifacts")
    artifacts = {key: _binding(item, f"reconciliation source {key}") for key, item in artifacts.items()}
    if source["artifact_inventory_sha256"] != common.sha256_value(artifacts):
        common.fail("reconciliation artifact inventory hash differs")
    unlock_steps = _unlock_steps(source["unlock_steps"], operation=operation)
    if source["unlock_step_inventory_sha256"] != common.sha256_value(unlock_steps):
        common.fail("reconciliation unlock step inventory differs")

    branch = common.exact_keys(receipt["branch"], {"main_sha", "rule_id", "rule_identity_sha256", "expected_before", "observed_after"}, "reconciliation branch")
    rule_id = common.exact_string(branch["rule_id"], "reconciliation rule ID")
    if (
        RULE_ID_RE.fullmatch(rule_id) is None
        or branch["main_sha"] != control["workflow_sha"]
        or branch["rule_identity_sha256"] != common.sha256_bytes(rule_id.encode("utf-8"))
        or branch["expected_before"] != {"lock_branch": True, "is_admin_enforced": True, "lock_allows_fetch_and_merge": False}
        or branch["observed_after"] != {"lock_branch": False, "is_admin_enforced": True, "lock_allows_fetch_and_merge": False}
    ):
        common.fail("reconciliation branch projection differs")
    if type(receipt["observations"]) is not list or len(receipt["observations"]) != 3:
        common.fail("reconciliation observations differ")
    observations: list[dict[str, Any]] = []
    for index, item in enumerate(receipt["observations"], start=1):
        normalized = common.exact_keys(item, {"round", "observed_at", "main_sha", "rule_id", "lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge", "http_method", "api_operation"}, f"reconciliation observation {index}")
        if (
            normalized["round"] != index
            or normalized["main_sha"] != control["workflow_sha"]
            or normalized["rule_id"] != rule_id
            or normalized["lock_branch"] is not False
            or normalized["is_admin_enforced"] is not True
            or normalized["lock_allows_fetch_and_merge"] is not False
            or normalized["http_method"] != "POST"
            or normalized["api_operation"] != "graphql-query"
        ):
            common.fail("reconciliation observation projection differs")
        common.require_timestamp(normalized["observed_at"], f"reconciliation observation {index} time")
        observations.append(dict(normalized))
    ignored = {"round", "observed_at"}
    if not all({k: v for k, v in item.items() if k not in ignored} == {k: v for k, v in observations[0].items() if k not in ignored} for item in observations[1:]):
        common.fail("reconciliation observations are not identical")
    times = [common.require_timestamp(item["observed_at"], "observation time") for item in observations]
    if not (times[0] <= times[1] <= times[2] <= completed):
        common.fail("reconciliation observation order differs")
    expected_ledger = [
        item
        for round_number in range(1, 4)
        for item in (
            {"round": round_number, "label": "main-ref-before", "http_method": "GET", "api_operation": "rest-read"},
            {"round": round_number, "label": "branch-rule", "http_method": "POST", "api_operation": "graphql-query"},
            {"round": round_number, "label": "main-ref-after", "http_method": "GET", "api_operation": "rest-read"},
        )
    ]
    if (
        receipt["request_ledger"] != expected_ledger
        or receipt["http_request_count"] != 9
        or receipt["graphql_query_count"] != 3
        or receipt["branch_mutation_request_count"] != 0
        or receipt["mutation_text_present"] is not False
    ):
        common.fail("reconciliation request ledger differs")
    if receipt["gates"] != {
        "source_attempt_one": True,
        "source_terminal_failure": True,
        "pre_unlock_jobs_succeeded": True,
        "unlock_sole_incomplete_terminal_job": True,
        "exact_artifacts_verified": True,
        "dual_attested_release_authorization_verified": True,
        "dual_attested_original_receipt_verified": True,
        "main_unchanged": True,
        "unique_rule_observed_unlocked": True,
        "read_only": True,
    }:
        common.fail("reconciliation gates differ")
    authorities = common.exact_keys(receipt["authorities"], {"failure_assertion", "release_authorization", "original_receipt"}, "reconciliation authorities")
    expected_authorities = {
        "failure_assertion": (WORKFLOW_PATH, ASSERTION_PREDICATE),
        "release_authorization": (policy["path"], AUTHORIZATION_PREDICATE),
        "original_receipt": (policy["path"], policy["receipt_predicate"]),
    }
    for label, authority in authorities.items():
        authority = common.exact_keys(authority, {"binding", "signer_workflow", "signer_digest", "source_digest", "source_ref", "runner_environment", "provenance_predicate_type", "policy_predicate_type", "provenance_verification_sha256", "policy_verification_sha256"}, f"reconciliation {label}")
        binding = _binding(authority["binding"], f"reconciliation {label} binding")
        workflow_path, predicate = expected_authorities[label]
        if (
            authority["signer_workflow"] != f"{common.REPOSITORY}/{workflow_path}"
            or authority["signer_digest"] != control["workflow_sha"]
            or authority["source_digest"] != control["workflow_sha"]
            or authority["source_ref"] != "refs/heads/main"
            or authority["runner_environment"] != "github-hosted"
            or authority["provenance_predicate_type"] != PROVENANCE_PREDICATE
            or authority["policy_predicate_type"] != predicate
        ):
            common.fail("reconciliation attested authority differs")
        common.require_sha256(
            authority["provenance_verification_sha256"],
            f"reconciliation {label} provenance verification hash",
        )
        common.require_sha256(
            authority["policy_verification_sha256"],
            f"reconciliation {label} policy verification hash",
        )
        if label == "release_authorization" and binding != artifacts["release_authorization"]:
            common.fail("reconciliation release authorization is cross-spliced")
        if label == "original_receipt" and binding != artifacts["signed_receipt"]:
            common.fail("reconciliation original receipt is cross-spliced")
    common.sanitize_public(receipt)
    return receipt


def validate_pair(reconciliation: Any, receipt_value: Any) -> dict[str, Any]:
    reconciliation = validate_reconciliation(reconciliation)
    operation = reconciliation["operation"]
    receipt = _receipt(operation, receipt_value)
    binding = _binding(reconciliation["authorities"]["original_receipt"]["binding"], "paired original receipt")
    if (
        binding["sha256"] != common.sha256_bytes(common.canonical_file_bytes(receipt))
        or binding != reconciliation["source"]["artifacts"]["signed_receipt"]
        or receipt["control"]["workflow_sha"] != reconciliation["source"]["workflow_sha"]
        or str(receipt["control"]["run_id"]) != reconciliation["source"]["run_id"]
        or receipt["control"]["run_attempt"] != 1
    ):
        common.fail("normal lock-release reconciliation is not paired to receipt")
    return reconciliation


def _write_pair(path: Path, value: Mapping[str, Any]) -> None:
    runner_temp = Path(os.environ.get("RUNNER_TEMP", ""))
    digest = common.write_canonical_output(path, value, runner_temp)
    sidecar = path.with_suffix(".sha256")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(sidecar, flags, 0o600)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            common.fail("reconciliation sidecar differs")
        with os.fdopen(descriptor, "wb", closefd=False) as stream:
            stream.write((digest + "\n").encode("ascii"))
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(descriptor)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)
    assertion = commands.add_parser("build-assertion")
    assertion.add_argument("--request", type=Path, required=True)
    assertion.add_argument("--authorization", type=Path, required=True)
    assertion.add_argument("--receipt", type=Path, required=True)
    assertion.add_argument("--output", type=Path, required=True)
    validate_assert = commands.add_parser("validate-assertion")
    validate_assert.add_argument("--assertion", type=Path, required=True)
    reconcile = commands.add_parser("build-reconciliation")
    reconcile.add_argument("--request", type=Path, required=True)
    reconcile.add_argument("--assertion", type=Path, required=True)
    reconcile.add_argument("--assertion-authority", type=Path, required=True)
    reconcile.add_argument("--output", type=Path, required=True)
    validate_reconcile = commands.add_parser("validate-reconciliation")
    validate_reconcile.add_argument("--reconciliation", type=Path, required=True)
    validate_reconcile.add_argument("--receipt", type=Path)
    return root


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = parser().parse_args(arguments)
    now = dt.datetime.now(dt.timezone.utc)
    if args.command == "build-assertion":
        value = build_assertion(
            common.load_json(args.request, "assertion request"),
            authorization=common.load_json(args.authorization, "release authorization"),
            receipt=common.load_json(args.receipt, "original receipt"),
            now=now,
        )
        _write_pair(args.output, value)
    elif args.command == "validate-assertion":
        validate_assertion(
            common.load_json(args.assertion, "release assertion"), now=now
        )
    elif args.command == "build-reconciliation":
        value = build_reconciliation(
            common.load_json(args.request, "reconciliation request"),
            assertion=common.load_json(args.assertion, "release assertion"),
            assertion_authority=common.load_json(args.assertion_authority, "assertion authority"),
            now=now,
        )
        _write_pair(args.output, value)
    else:
        value = common.load_json(args.reconciliation, "release reconciliation")
        if args.receipt is None:
            validate_reconciliation(value)
        else:
            validate_pair(value, common.load_json(args.receipt, "original receipt"))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"main lock release reconciliation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
