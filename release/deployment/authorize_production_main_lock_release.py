#!/usr/bin/env python3
"""Build a credential-free, attested authority for one normal main-lock release.

The controller is deliberately offline.  It validates the exact successful
pre-unlock job/artifact inventory and the already-attested production receipt,
then emits the only subject that the token-isolated unlock job may consume.
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
import rollback_production_change as rollback_control
import verify_production_release as common


WORKFLOWS = {
    "apply": {
        "path": ".github/workflows/apply-production-phase.yml",
        "receipt_authority": "production-phase-apply-receipt",
        "receipt_prefix": "production-phase-apply",
        "receipt_predicate": "https://rereply.app/attestations/production-phase-apply-receipt/v1",
        "validator": common.validate_apply_receipt,
        "jobs": [
            "Authenticate exact production apply authority",
            "Prepare exact production apply pre-lock authority",
            "Prepare and attest exact production mutation intent",
            "Acquire exact production apply main lock",
            "Attest exact production apply main lock proof",
            "Apply exact production phase",
            "Exact production apply receipt gate",
            "Authorize exact production apply main lock release",
        ],
    },
    "rollback": {
        "path": ".github/workflows/rollback-production-phase.yml",
        "receipt_authority": "production-phase-rollback-receipt",
        "receipt_prefix": "production-phase-rollback",
        "receipt_predicate": "https://rereply.app/attestations/production-phase-rollback-receipt/v1",
        "validator": rollback_control.validate_rollback_receipt,
        "jobs": [
            "Authenticate exact production rollback authority",
            "Prepare exact production rollback pre-lock authority",
            "Prepare and attest exact production rollback mutation intent",
            "Acquire exact production rollback main lock",
            "Attest exact production rollback main lock proof",
            "Roll back exact production phase",
            "Exact production rollback receipt gate",
            "Authorize exact production rollback main lock release",
        ],
    },
}
WORKFLOW_PATHS = {item["path"] for item in WORKFLOWS.values()}
AUTHORITY = "production-main-lock-release-authorization"
STATE = "pre-unlock-authorized"
PREDICATE_TYPE = "https://rereply.app/attestations/production-main-lock-release-authorization/v1"
PROVENANCE_PREDICATE_TYPE = "https://slsa.dev/provenance/v1"
MAX_AGE_SECONDS = 600
RULE_ID_RE = re.compile(r"^[A-Za-z0-9_+/=-]{1,512}$")


def _binding(value: Any, label: str) -> dict[str, Any]:
    binding = copy.deepcopy(common.validate_full_artifact_binding(value, label))
    binding["run_id"] = str(binding["run_id"])
    binding["artifact_id"] = str(binding["artifact_id"])
    return binding


def _job(value: Any, label: str) -> dict[str, Any]:
    job = common.exact_keys(
        value, {"job_id", "name", "status", "conclusion"}, label
    )
    normalized = {
        "job_id": common.require_run_id(job["job_id"], f"{label} ID"),
        "name": common.exact_string(job["name"], f"{label} name"),
        "status": common.exact_string(job["status"], f"{label} status"),
        "conclusion": job["conclusion"],
    }
    if normalized["conclusion"] is not None:
        normalized["conclusion"] = common.exact_string(
            normalized["conclusion"], f"{label} conclusion"
        )
    return normalized


def _control(value: Any, operation: str) -> dict[str, Any]:
    control = common.exact_keys(
        value,
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "authorization_schema_sha256",
            "controller_sha256",
        },
        "release authorization control",
    )
    expected = WORKFLOWS[operation]
    normalized = {
        "workflow_sha": common.require_sha1(
            control["workflow_sha"], "authorization workflow SHA"
        ),
        "workflow_path": common.exact_string(
            control["workflow_path"], "authorization workflow path"
        ),
        "run_id": common.require_run_id(control["run_id"], "authorization run ID"),
        "run_attempt": common.exact_int(
            control["run_attempt"], "authorization run attempt", 1, 1
        ),
        "runner_environment": common.exact_string(
            control["runner_environment"], "authorization runner"
        ),
        "authorization_schema_sha256": common.require_sha256(
            control["authorization_schema_sha256"], "authorization schema hash"
        ),
        "controller_sha256": common.require_sha256(
            control["controller_sha256"], "authorization controller hash"
        ),
    }
    if (
        normalized["workflow_path"] != expected["path"]
        or normalized["runner_environment"] != "github-hosted"
    ):
        common.fail("release authorization workflow identity differs")
    return normalized


def _receipt_authority(
    value: Any,
    *,
    operation: str,
    receipt: Mapping[str, Any],
    binding: Mapping[str, Any],
) -> dict[str, Any]:
    authority = common.exact_keys(
        value,
        {
            "signer_workflow", "signer_digest", "source_digest", "source_ref",
            "runner_environment", "provenance_predicate_type",
            "policy_predicate_type", "provenance_verification_sha256",
            "policy_verification_sha256",
        },
        "receipt attestation authority",
    )
    policy = WORKFLOWS[operation]
    receipt_control = receipt["control"]
    expected_signer = f"{common.REPOSITORY}/{policy['path']}"
    if (
        authority["signer_workflow"] != expected_signer
        or authority["signer_digest"] != receipt_control["workflow_sha"]
        or authority["source_digest"] != receipt_control["workflow_sha"]
        or authority["source_ref"] != "refs/heads/main"
        or authority["runner_environment"] != "github-hosted"
        or authority["provenance_predicate_type"] != PROVENANCE_PREDICATE_TYPE
        or authority["policy_predicate_type"] != policy["receipt_predicate"]
        or binding["sha256"] != common.sha256_bytes(common.canonical_file_bytes(receipt))
    ):
        common.fail("receipt attestation authority differs")
    for key in ("signer_digest", "source_digest"):
        common.require_sha1(authority[key], f"receipt authority {key}")
    for key in ("provenance_verification_sha256", "policy_verification_sha256"):
        common.require_sha256(authority[key], f"receipt authority {key}")
    return copy.deepcopy(dict(authority))


def build_authorization(
    request: Any,
    receipt_value: Any,
    *,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    request = common.exact_keys(
        request,
        {
            "operation", "issued_at", "control", "jobs", "artifacts",
            "branch", "receipt_attestations",
        },
        "release authorization request",
    )
    operation = common.exact_string(request["operation"], "release operation")
    if operation not in WORKFLOWS:
        common.fail("release operation differs")
    policy = WORKFLOWS[operation]
    issued = common.require_timestamp(request["issued_at"], "authorization issue time")
    if now is not None:
        checked = now.astimezone(dt.timezone.utc)
        expires = issued + dt.timedelta(seconds=MAX_AGE_SECONDS)
        if issued > checked or checked >= expires:
            common.fail("release authorization issue time is stale")
    control = _control(request["control"], operation)

    if type(request["jobs"]) is not list or len(request["jobs"]) != 8:
        common.fail("release authorization job inventory differs")
    jobs = [_job(item, f"release job {index}") for index, item in enumerate(request["jobs"])]
    if [item["name"] for item in jobs] != policy["jobs"]:
        common.fail("release authorization job names differ")
    for item in jobs[:-1]:
        if item["status"] != "completed" or item["conclusion"] != "success":
            common.fail("release authorization prerequisite did not succeed")
    if jobs[-1]["status"] != "in_progress" or jobs[-1]["conclusion"] is not None:
        common.fail("release authorization job is not current")

    artifacts = common.exact_keys(
        request["artifacts"],
        {
            "acquire_intent", "mutation_intent", "main_lock_proof",
            "unsigned_receipt", "signed_receipt",
        },
        "release authorization artifacts",
    )
    normalized_artifacts = {
        key: _binding(value, f"release authorization {key}")
        for key, value in artifacts.items()
    }
    run_id = control["run_id"]
    expected_names = {
        "acquire_intent": f"production-main-lock-{operation}-{run_id}-1",
        "mutation_intent": f"production-mutation-intent-{operation}-{run_id}-1",
        "main_lock_proof": f"production-main-lock-proof-{operation}-{run_id}-1",
        "unsigned_receipt": f"unsigned-production-phase-{operation}-{run_id}-1",
        "signed_receipt": f"production-phase-{operation}-{run_id}-1",
    }
    for key, binding in normalized_artifacts.items():
        if (
            binding["run_id"] != run_id
            or binding["run_attempt"] != 1
            or binding["artifact_name"] != expected_names[key]
        ):
            common.fail("release authorization artifact identity differs")

    validator = policy["validator"]
    try:
        receipt = validator(receipt_value)
    except Exception as exc:
        raise common.ReleaseError("release receipt validation failed") from exc
    receipt_control = receipt["control"]
    if (
        receipt.get("authority") != policy["receipt_authority"]
        or receipt_control["workflow_sha"] != control["workflow_sha"]
        or receipt_control["workflow_path"] != control["workflow_path"]
        or str(receipt_control["run_id"]) != run_id
        or receipt_control["run_attempt"] != 1
    ):
        common.fail("release receipt control differs")
    receipt_authority = _receipt_authority(
        request["receipt_attestations"],
        operation=operation,
        receipt=receipt,
        binding=normalized_artifacts["signed_receipt"],
    )

    branch = common.exact_keys(
        request["branch"],
        {
            "main_sha", "rule_id", "rule_identity_sha256", "lock_branch",
            "is_admin_enforced", "lock_allows_fetch_and_merge",
        },
        "release authorization branch",
    )
    rule_id = common.exact_string(branch["rule_id"], "release rule ID")
    if RULE_ID_RE.fullmatch(rule_id) is None:
        common.fail("release rule ID differs")
    normalized_branch = {
        "main_sha": common.require_sha1(branch["main_sha"], "release main SHA"),
        "rule_id": rule_id,
        "rule_identity_sha256": common.require_sha256(
            branch["rule_identity_sha256"], "release rule identity hash"
        ),
        "lock_branch": branch["lock_branch"],
        "is_admin_enforced": branch["is_admin_enforced"],
        "lock_allows_fetch_and_merge": branch["lock_allows_fetch_and_merge"],
    }
    if (
        normalized_branch["main_sha"] != control["workflow_sha"]
        or normalized_branch["rule_identity_sha256"]
        != common.sha256_bytes(rule_id.encode("utf-8"))
        or normalized_branch["lock_branch"] is not True
        or normalized_branch["is_admin_enforced"] is not True
        or normalized_branch["lock_allows_fetch_and_merge"] is not False
    ):
        common.fail("release authorization branch differs")

    authorization = {
        "schema_version": 1,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": request["issued_at"],
        "expires_at": (
            issued + dt.timedelta(seconds=MAX_AGE_SECONDS)
        ).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "state": STATE,
        "operation": operation,
        "control": control,
        "branch": normalized_branch,
        "jobs": jobs,
        "job_inventory_sha256": common.sha256_value(jobs),
        "artifacts": normalized_artifacts,
        "artifact_inventory_sha256": common.sha256_value(normalized_artifacts),
        "receipt_attestations": receipt_authority,
        "authorization_scope": "release-exact-normal-main-lock-only",
        "static_max_branch_mutations": 1,
    }
    common.sanitize_public(authorization)
    return authorization


def validate_authorization(
    value: Any,
    *,
    receipt: Any | None = None,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    authorization = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "issued_at", "expires_at", "state",
            "operation", "control", "branch", "jobs", "job_inventory_sha256",
            "artifacts", "artifact_inventory_sha256", "receipt_attestations",
            "authorization_scope", "static_max_branch_mutations",
        },
        "main lock release authorization",
    )
    if (
        authorization["schema_version"] != 1
        or authorization["authority"] != AUTHORITY
        or authorization["repository"] != common.REPOSITORY
        or authorization["state"] != STATE
        or authorization["authorization_scope"]
        != "release-exact-normal-main-lock-only"
        or authorization["static_max_branch_mutations"] != 1
    ):
        common.fail("main lock release authorization identity differs")
    operation = authorization["operation"]
    if operation not in WORKFLOWS:
        common.fail("main lock release authorization operation differs")
    request = {
        key: copy.deepcopy(authorization[key])
        for key in (
            "operation", "issued_at", "control", "jobs", "artifacts", "branch",
            "receipt_attestations",
        )
    }
    if receipt is None:
        # Structural validation without the receipt still verifies both stable
        # inventory hashes; cross-artifact validation requires the receipt.
        issued = common.require_timestamp(request["issued_at"], "authorization issue time")
        if now is not None:
            checked = now.astimezone(dt.timezone.utc)
            expires = common.require_timestamp(
                authorization["expires_at"], "authorization expiry time"
            )
            if (
                expires != issued + dt.timedelta(seconds=MAX_AGE_SECONDS)
                or issued > checked
                or checked >= expires
            ):
                common.fail("release authorization issue time is stale")
        _control(request["control"], operation)
        jobs = [_job(item, f"release job {index}") for index, item in enumerate(request["jobs"])]
        if [item["name"] for item in jobs] != WORKFLOWS[operation]["jobs"]:
            common.fail("release authorization job names differ")
        for item in jobs[:-1]:
            if item["status"] != "completed" or item["conclusion"] != "success":
                common.fail("release authorization prerequisite did not succeed")
        if jobs[-1]["status"] != "in_progress" or jobs[-1]["conclusion"] is not None:
            common.fail("release authorization job is not current")
        artifacts = {
            key: _binding(item, f"authorization {key}")
            for key, item in common.exact_keys(
                request["artifacts"],
                {"acquire_intent", "mutation_intent", "main_lock_proof", "unsigned_receipt", "signed_receipt"},
                "authorization artifacts",
            ).items()
        }
        if authorization["job_inventory_sha256"] != common.sha256_value(jobs):
            common.fail("authorization job inventory hash differs")
        if authorization["artifact_inventory_sha256"] != common.sha256_value(artifacts):
            common.fail("authorization artifact inventory hash differs")
        common.sanitize_public(authorization)
        return authorization
    rebuilt = build_authorization(request, receipt, now=now)
    if rebuilt != authorization:
        common.fail("main lock release authorization differs")
    return authorization


def _load(path: Path, label: str) -> Any:
    return common.load_json(path, label)


def _write_pair(output: Path, value: Mapping[str, Any]) -> None:
    runner_temp = Path(os.environ.get("RUNNER_TEMP", ""))
    digest = common.write_canonical_output(output, value, runner_temp)
    sidecar = output.with_suffix(".sha256")
    if sidecar.parent.resolve(strict=True) != runner_temp.resolve(strict=True):
        common.fail("authorization sidecar escaped RUNNER_TEMP")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(sidecar, flags, 0o600)
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            common.fail("authorization sidecar is not a single regular file")
        with os.fdopen(descriptor, "wb", closefd=False) as stream:
            stream.write((digest + "\n").encode("ascii"))
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(descriptor)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build")
    build.add_argument("--request", type=Path, required=True)
    build.add_argument("--receipt", type=Path, required=True)
    build.add_argument("--output", type=Path, required=True)
    validate = commands.add_parser("validate")
    validate.add_argument("--authorization", type=Path, required=True)
    validate.add_argument("--receipt", type=Path, required=True)
    validate.add_argument("--sha256")
    return root


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = parser().parse_args(arguments)
    if args.command == "build":
        value = build_authorization(
            _load(args.request, "authorization request"),
            _load(args.receipt, "production receipt"),
            now=dt.datetime.now(dt.timezone.utc),
        )
        _write_pair(args.output, value)
    else:
        value = _load(args.authorization, "release authorization")
        if args.sha256 is not None and common.sha256_bytes(
            common.canonical_file_bytes(value)
        ) != common.require_sha256(args.sha256, "authorization hash"):
            common.fail("authorization exact-file hash differs")
        receipt = _load(args.receipt, "production receipt")
        validate_authorization(
            value,
            receipt=receipt,
            now=dt.datetime.now(dt.timezone.utc),
        )
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"release authorization failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
