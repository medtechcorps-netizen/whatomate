#!/usr/bin/env python3
"""Classify fixture closure from already acquired, verified public evidence.

This command has no HTTP, product, provider, credential, or deletion client.
The protected launcher must successfully run ``gh attestation verify`` with
the exact signer/source flags before supplying its JSON results. The checks
below bind those verified statements to the supplied intent and GitHub run.
Missing evidence is an error, never a permission to repeat a mutation.
"""

from __future__ import annotations

import argparse
import datetime as dt
import sys
from pathlib import Path
from typing import Any

try:
    from . import verify_production_release as common
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/provision-production-crm-canary-fixture.yml"
INTENT_PREDICATE = "https://rereply.app/attestations/crm-canary-fixture-intent/v1"
PROVENANCE_PREDICATE = "https://slsa.dev/provenance/v1"
EXECUTOR_JOB = "Execute exact CRM canary fixture bootstrap"
INTENT_JOB = "Prepare and attest CRM canary fixture intent"
JOB_NAMES = (
    INTENT_JOB,
    "Test exact fixture claim adapter",
    EXECUTOR_JOB,
    "Reconcile exact CRM canary fixture state",
    "Exact CRM fixture control gate",
)
TERMINAL = {"success", "failure", "cancelled", "skipped", "timed_out", "neutral", "action_required", "stale", "startup_failure"}
INTENT_KEYS = {
    "schema_version", "kind", "control_sha", "origin_run_id", "origin_run_attempt",
    "executor_job", "workflow_path", "workflow_sha256", "controller_sha256",
    "upload_bundle_sha256", "request", "slots", "issued_at", "expires_at",
}
REPORT_KEYS = {
    "schema_version", "kind", "control_sha", "origin_run_id", "origin_run_attempt",
    "origin_intent_sha256", "operation_sha256", "classification",
    "effects_possible_upper_bound", "evidence_sha256", "requires_separate_inverse",
    "no_live_mutation",
}


def _require(condition: bool, label: str) -> None:
    if not condition:
        common.fail(label)


def _object(value: Any, label: str) -> dict[str, Any]:
    _require(type(value) is dict, label)
    return value


def _timestamp(value: Any, label: str) -> dt.datetime:
    parsed = common.require_timestamp(value, label)
    _require(common.format_timestamp(parsed) == value, label)
    return parsed


def _positive_id(value: Any, label: str) -> str:
    return common.require_run_id(value, label)


def _expected_slots() -> list[dict[str, Any]]:
    # This shared fixed construction is code authority, never a CLI argument.
    try:
        from .provision_production_crm_canary_fixture import expected_origin_slots
    except ImportError:
        from provision_production_crm_canary_fixture import expected_origin_slots
    return expected_origin_slots()


def validate_origin_intent(value: Any, *, now: dt.datetime) -> dict[str, Any]:
    intent = common.exact_keys(value, INTENT_KEYS, "fixture origin intent")
    _require(type(intent["schema_version"]) is int and intent["schema_version"] == 1, "fixture intent schema differs")
    _require(intent["kind"] == "crm-canary-fixture-intent", "fixture intent kind differs")
    common.require_sha1(intent["control_sha"], "fixture control")
    _require(type(intent["origin_run_id"]) is str, "fixture origin run differs")
    _positive_id(intent["origin_run_id"], "fixture origin run")
    common.exact_int(intent["origin_run_attempt"], "fixture origin attempt", 1, 1)
    _require(intent["executor_job"] == EXECUTOR_JOB and intent["workflow_path"] == WORKFLOW_PATH, "fixture executor authority differs")
    for name in ("workflow_sha256", "controller_sha256", "upload_bundle_sha256"):
        common.require_sha256(intent[name], "fixture source binding")
    request = common.exact_keys(intent["request"], {"schema_version", "control_sha", "operation_sha256", "descriptor_sha256"}, "fixture request")
    _require(type(request["schema_version"]) is int and request["schema_version"] == 1, "fixture request schema differs")
    _require(request["control_sha"] == intent["control_sha"], "fixture request control differs")
    for name in ("operation_sha256", "descriptor_sha256"):
        common.require_sha256(request[name], "fixture request binding")
    _require(type(intent["slots"]) is list and bool(intent["slots"]), "fixture slots differ")
    for slot in intent["slots"]:
        common.exact_keys(slot, {"stage", "wrapper_upper_bound", "nested_upper_bound"}, "fixture slot")
        common.exact_string(slot["stage"], "fixture stage")
        common.exact_int(slot["wrapper_upper_bound"], "fixture wrapper ceiling", 1, 1)
        common.exact_int(slot["nested_upper_bound"], "fixture nested ceiling", 0, 1)
    _require(intent["slots"] == _expected_slots(), "fixture fixed slot inventory differs")
    _timestamp(intent["issued_at"], "fixture issued time")
    _timestamp(intent["expires_at"], "fixture expiry time")
    common.validate_fresh_window(intent["issued_at"], intent["expires_at"], now,
                                 maximum_age_seconds=86_400, label="fixture intent")
    return intent


def _statements(value: Any, predicate_type: str, subject_sha256: str) -> list[dict[str, Any]]:
    _require(type(value) is list and 0 < len(value) <= 100, "fixture verification results differ")
    matched = []
    for item in value:
        result = _object(_object(item, "fixture verification entry differs").get("verificationResult"), "fixture verification result differs")
        statement = common.exact_keys(result.get("statement"), {"_type", "subject", "predicateType", "predicate"}, "fixture verified statement")
        _require(statement["_type"] == "https://in-toto.io/Statement/v1", "fixture statement type differs")
        subjects = statement["subject"]
        _require(type(subjects) is list and len(subjects) == 1, "fixture verified subject inventory differs")
        subject = common.exact_keys(subjects[0], {"name", "digest"}, "fixture verified subject")
        common.exact_string(subject["name"], "fixture verified subject name")
        common.exact_keys(subject["digest"], {"sha256"}, "fixture subject digest")
        common.require_sha256(subject["digest"]["sha256"], "fixture verified digest")
        if statement["predicateType"] == predicate_type and subject["digest"]["sha256"] == subject_sha256:
            matched.append(statement)
    _require(bool(matched), "fixture verified subject binding differs")
    return matched


def validate_verified_provenance(intent: dict[str, Any], provenance: Any, policy_verification: Any) -> None:
    subject_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
    policies = _statements(policy_verification, INTENT_PREDICATE, subject_hash)
    _require(any(statement["predicate"] == intent for statement in policies), "fixture verified policy differs")
    statements = _statements(provenance, PROVENANCE_PREDICATE, subject_hash)
    for statement in statements:
        predicate = _object(statement["predicate"], "fixture provenance predicate differs")
        definition = _object(predicate.get("buildDefinition"), "fixture provenance definition differs")
        external = _object(definition.get("externalParameters"), "fixture provenance parameters differ")
        workflow = _object(external.get("workflow"), "fixture provenance workflow differs")
        details = _object(predicate.get("runDetails"), "fixture provenance run differs")
        builder = _object(details.get("builder"), "fixture provenance builder differs")
        metadata = _object(details.get("metadata"), "fixture provenance metadata differs")
        dependencies = definition.get("resolvedDependencies")
        _require(type(dependencies) is list and bool(dependencies), "fixture provenance sources differ")
        source = f"git+https://github.com/{common.REPOSITORY}@refs/heads/main"
        matches = [item for item in dependencies if type(item) is dict and item.get("uri") == source
                   and item.get("digest") == {"gitCommit": intent["control_sha"]}]
        if (definition.get("buildType") == "https://actions.github.io/buildtypes/workflow/v1"
                and workflow.get("repository") == f"https://github.com/{common.REPOSITORY}"
                and workflow.get("path") == WORKFLOW_PATH and workflow.get("ref") == "refs/heads/main"
                and builder.get("id") == "https://github.com/actions/runner/github-hosted"
                and metadata.get("invocationId") == f"https://github.com/{common.REPOSITORY}/actions/runs/{intent['origin_run_id']}/attempts/1"
                and len(matches) == 1):
            return
    common.fail("fixture verified provenance authority differs")


def _run(value: Any, intent: dict[str, Any]) -> dict[str, Any]:
    run = _object(value, "fixture origin run differs")
    _require(_positive_id(run.get("id"), "fixture run identity") == intent["origin_run_id"], "fixture run identity differs")
    common.exact_int(run.get("run_attempt"), "fixture run attempt", 1, 1)
    _require(run.get("head_sha") == intent["control_sha"] and run.get("head_branch") == "main"
             and run.get("event") == "workflow_dispatch" and run.get("path") == WORKFLOW_PATH
             and run.get("status") == "completed" and run.get("conclusion") in TERMINAL,
             "fixture terminal run authority differs")
    _require("previous_attempt_url" in run and run["previous_attempt_url"] is None, "fixture previous attempt differs")
    repository = _object(run.get("repository"), "fixture run repository differs")
    _require(repository.get("full_name") == common.REPOSITORY, "fixture run repository differs")
    return run


def _jobs(value: Any, intent: dict[str, Any]) -> dict[str, dict[str, Any]]:
    envelope = common.exact_keys(value, {"total_count", "jobs"}, "fixture job envelope")
    common.exact_int(envelope["total_count"], "fixture job count", len(JOB_NAMES), len(JOB_NAMES))
    records = envelope["jobs"]
    _require(type(records) is list and len(records) == len(JOB_NAMES), "fixture job inventory incomplete")
    names, identities = {}, set()
    for job in records:
        _object(job, "fixture job differs")
        identity = _positive_id(job.get("id"), "fixture job identity")
        _require(identity not in identities, "fixture duplicate job identity")
        identities.add(identity)
        _require(_positive_id(job.get("run_id"), "fixture job run") == intent["origin_run_id"], "fixture job run differs")
        common.exact_int(job.get("run_attempt"), "fixture job attempt", 1, 1)
        _require(job.get("head_sha") == intent["control_sha"], "fixture job control differs")
        name = job.get("name")
        _require(type(name) is str and name in JOB_NAMES and name not in names, "fixture job names differ")
        _require(job.get("status") == "completed" and job.get("conclusion") in TERMINAL, "fixture nonterminal job")
        steps = job.get("steps")
        _require(type(steps) is list and len(steps) <= 100, "fixture job steps differ")
        numbers = set()
        for step in steps:
            _object(step, "fixture job step differs")
            number = common.exact_int(step.get("number"), "fixture step number", 1, 1000)
            _require(number not in numbers, "fixture duplicate step")
            numbers.add(number)
            common.exact_string(step.get("name"), "fixture step name")
            _require(step.get("status") == "completed" and step.get("conclusion") in TERMINAL, "fixture nonterminal step")
        _require(job["conclusion"] != "skipped" or not steps, "fixture skipped job has steps")
        names[name] = job
    _require(set(names) == set(JOB_NAMES) and names[INTENT_JOB]["conclusion"] == "success", "fixture intent job authority differs")
    return names


def _artifacts(value: Any, intent: dict[str, Any], now: dt.datetime) -> tuple[bool, dict[str, Any]]:
    envelope = common.exact_keys(value, {"total_count", "artifacts"}, "fixture artifact envelope")
    records = envelope["artifacts"]
    common.exact_int(envelope["total_count"], "fixture artifact count", 1, len(intent["slots"]) + 3)
    _require(type(records) is list and len(records) == envelope["total_count"], "fixture artifact inventory incomplete")
    suffix = f"{intent['origin_run_id']}-1"
    intent_name = f"crm-canary-fixture-intent-{suffix}"
    burns = {f"crm-canary-fixture-burn-{suffix}-{slot['stage']}" for slot in intent["slots"]}
    expected = burns | {intent_name, f"crm-canary-fixture-result-{suffix}",
                       f"crm-canary-fixture-burn-{suffix}-adapter_probe"}
    names, identities = set(), set()
    origin_artifact, burned = None, False
    for artifact in records:
        _object(artifact, "fixture artifact differs")
        identity = _positive_id(artifact.get("id"), "fixture artifact identity")
        name = artifact.get("name")
        _require(type(name) is str and name in expected and name not in names and identity not in identities, "fixture artifact names or identities differ")
        identities.add(identity)
        names.add(name)
        common.exact_int(artifact.get("size_in_bytes"), "fixture artifact size", 1, 1_048_576)
        _require(artifact.get("expired") is False, "fixture artifact expired")
        common.require_digest(artifact.get("digest"), "fixture artifact API digest")
        created = _timestamp(artifact.get("created_at"), "fixture artifact created time")
        expires = _timestamp(artifact.get("expires_at"), "fixture artifact expiry")
        _require(created <= now < expires and created < expires, "fixture artifact time differs")
        binding = _object(artifact.get("workflow_run"), "fixture artifact run differs")
        _require(_positive_id(binding.get("id"), "fixture artifact run") == intent["origin_run_id"]
                 and binding.get("head_sha") == intent["control_sha"] and binding.get("head_branch") == "main",
                 "fixture artifact authority differs")
        burned = burned or name in burns
        if name == intent_name:
            origin_artifact = artifact
    _require(origin_artifact is not None, "fixture origin artifact unavailable")
    return burned, origin_artifact


def build_report(intent: Any, evidence: Any, provenance: Any, policy_verification: Any,
                 *, now: dt.datetime, expected_control_sha: str, expected_origin_run_id: str,
                 expected_intent_sha256: str, expected_origin_artifact_id: str,
                 expected_origin_artifact_digest: str) -> dict[str, Any]:
    common.require_sha1(expected_control_sha, "fixture expected control")
    _positive_id(expected_origin_run_id, "fixture expected origin")
    common.require_sha256(expected_intent_sha256, "fixture expected intent")
    _positive_id(expected_origin_artifact_id, "fixture expected artifact")
    common.require_digest(expected_origin_artifact_digest, "fixture expected artifact digest")
    checked = validate_origin_intent(intent, now=now)
    _require(checked["control_sha"] == expected_control_sha and checked["origin_run_id"] == expected_origin_run_id
             and common.sha256_bytes(common.canonical_file_bytes(checked)) == expected_intent_sha256,
             "fixture origin anchor differs")
    validate_verified_provenance(checked, provenance, policy_verification)
    inventory = common.exact_keys(evidence, {"run", "attempt", "jobs", "artifacts"}, "fixture evidence envelope")
    latest, attempt = _run(inventory["run"], checked), _run(inventory["attempt"], checked)
    _require(latest["conclusion"] == attempt["conclusion"], "fixture latest attempt differs")
    jobs = _jobs(inventory["jobs"], checked)
    burned, origin = _artifacts(inventory["artifacts"], checked, now)
    _require(str(origin["id"]) == expected_origin_artifact_id and origin["digest"] == expected_origin_artifact_digest,
             "fixture artifact anchor differs")
    executor = jobs[EXECUTOR_JOB]
    safe_abort = executor["conclusion"] == "skipped" and executor["steps"] == [] and not burned
    upper_bound = 0 if safe_abort else sum(slot["wrapper_upper_bound"] + slot["nested_upper_bound"] for slot in checked["slots"])
    report = {
        "schema_version": 1, "kind": "crm-canary-fixture-cleanup",
        "control_sha": checked["control_sha"], "origin_run_id": checked["origin_run_id"], "origin_run_attempt": 1,
        "origin_intent_sha256": expected_intent_sha256, "operation_sha256": checked["request"]["operation_sha256"],
        "classification": "aborted_before_effect" if safe_abort else "quarantined",
        "effects_possible_upper_bound": upper_bound,
        "evidence_sha256": common.sha256_value({"github": inventory, "provenance": provenance, "policy_verification": policy_verification}),
        "requires_separate_inverse": not safe_abort, "no_live_mutation": True,
    }
    common.exact_keys(report, REPORT_KEYS, "fixture cleanup report")
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Classify fixture closure without live mutation")
    parser.add_argument("command", choices=["classify"])
    for name in ("intent", "evidence", "provenance", "policy-verification", "output-dir"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args(argv)
    try:
        import os
        descriptor = common.load_json(args.intent.parent / "descriptor.json", "origin descriptor")
        result = build_report(
            common.load_json(args.intent, "fixture intent"),
            common.load_json(args.evidence, "fixture GitHub evidence", canonical=False),
            common.load_json(args.provenance, "fixture provenance verification", canonical=False),
            common.load_json(args.policy_verification, "fixture policy verification", canonical=False),
            now=dt.datetime.now(dt.timezone.utc), expected_control_sha=os.environ["GITHUB_SHA"],
            expected_origin_run_id=descriptor["run_id"], expected_intent_sha256=descriptor["intent_sha256"],
            expected_origin_artifact_id=descriptor["artifact_id"],
            expected_origin_artifact_digest=descriptor["artifact_digest"],
        )
        args.output_dir.mkdir(mode=0o700, exist_ok=False)
        raw = common.canonical_file_bytes(result)
        digest = common.sha256_bytes(raw)
        (args.output_dir / "cleanup.json").write_bytes(raw)
        (args.output_dir / "cleanup.sha256").write_text(digest + "\n", encoding="ascii")
    except (common.ReleaseError, OSError, ValueError, TypeError, KeyError, ImportError):
        print("fixture cleanup evidence rejected", file=sys.stderr)
        return 1
    print("fixture cleanup report sha256:" + digest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
