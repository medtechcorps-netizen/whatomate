#!/usr/bin/env python3

from __future__ import annotations

import copy
import datetime as dt
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import authorize_production_main_lock_release as auth
import reconcile_production_main_lock_release as reconcile
import test_authorize_production_main_lock_release as fixtures
import verify_production_release as common


HASH = "d" * 64
ISSUED = fixtures.ISSUED


def attested(binding: dict, workflow: str, predicate: str, sha: str) -> dict:
    return {
        "binding": copy.deepcopy(binding),
        "signer_workflow": f"{common.REPOSITORY}/{workflow}",
        "signer_digest": sha,
        "source_digest": sha,
        "source_ref": "refs/heads/main",
        "runner_environment": "github-hosted",
        "provenance_predicate_type": "https://slsa.dev/provenance/v1",
        "policy_predicate_type": predicate,
        "provenance_verification_sha256": "e" * 64,
        "policy_verification_sha256": "f" * 64,
    }


def context() -> tuple[dict, dict, dict]:
    auth_request, receipt = fixtures.request_and_receipt()
    authorization = auth.build_authorization(auth_request, receipt, now=ISSUED)
    run_id = authorization["control"]["run_id"]
    source_jobs = []
    for index, name in enumerate(
        [*auth.WORKFLOWS["apply"]["jobs"], "Release exact production apply main lock"],
        start=1,
    ):
        job_id = (
            authorization["jobs"][index - 1]["job_id"]
            if index <= len(authorization["jobs"])
            else str(5000 + index)
        )
        source_jobs.append(
            {
                "job_id": job_id,
                "name": name,
                "status": "completed",
                "conclusion": "failure" if index == 9 else "success",
                "started_at": "2026-08-27T01:02:00Z" if index < 9 else "2026-08-27T01:03:00Z",
                "completed_at": "2026-08-27T01:02:30Z" if index < 9 else "2026-08-27T01:03:01Z",
            }
        )
    step_names = [
        "Set up job",
        "Check out exact protected controls",
        "Install pinned unlock verification tools",
        "Download exact signed apply release authorization",
        "Download exact signed apply receipt for release",
        "Authenticate exact apply pre-unlock authority",
        "Release the exact authorized apply main lock",
        "Post Check out exact protected controls",
        "Complete job",
    ]
    steps = []
    for index, name in enumerate(step_names, start=1):
        conclusion = "success" if index <= 6 else ("failure" if index == 7 else "skipped")
        steps.append(
            {
                # GitHub reserves hidden runner-step numbers before post steps.
                "number": index if index <= 7 else index + 2,
                "name": name,
                "status": "completed",
                "conclusion": conclusion,
                "started_at": None if index >= 8 else "2026-08-27T01:03:00Z",
                "completed_at": None if index >= 8 else "2026-08-27T01:03:01Z",
            }
        )
    auth_binding = fixtures.binding(
        run_id,
        "2999",
        f"production-main-lock-release-authorization-apply-{run_id}-1",
        common.sha256_bytes(common.canonical_file_bytes(authorization)),
    )
    artifacts = {**copy.deepcopy(authorization["artifacts"]), "release_authorization": auth_binding}
    source = {
        "workflow_sha": authorization["control"]["workflow_sha"],
        "workflow_path": auth.WORKFLOWS["apply"]["path"],
        "workflow_name": "Apply Production Phase",
        "run_id": run_id,
        "run_attempt": 1,
        "event": "workflow_dispatch",
        "head_branch": "main",
        "status": "completed",
        "conclusion": "failure",
        "jobs": source_jobs,
        "job_inventory_sha256": common.sha256_value(source_jobs),
        "authorization_job_inventory_sha256": common.sha256_value(authorization["jobs"]),
        "unlock_steps": steps,
        "unlock_step_inventory_sha256": common.sha256_value(steps),
        "artifacts": artifacts,
        "artifact_inventory_sha256": common.sha256_value(artifacts),
    }
    control = {
        "workflow_sha": authorization["control"]["workflow_sha"],
        "workflow_path": reconcile.WORKFLOW_PATH,
        "run_id": "9001",
        "run_attempt": 1,
        "runner_environment": "github-hosted",
        "assertion_schema_sha256": HASH,
        "reconciliation_schema_sha256": "c" * 64,
        "controller_sha256": "b" * 64,
    }
    assertion_request = {
        "operation": "apply",
        "issued_at": "2026-08-27T01:03:02Z",
        "control": control,
        "source": source,
        "authorization_attestations": attested(
            auth_binding,
            auth.WORKFLOWS["apply"]["path"],
            auth.PREDICATE_TYPE,
            authorization["control"]["workflow_sha"],
        ),
        "receipt_attestations": attested(
            artifacts["signed_receipt"],
            auth.WORKFLOWS["apply"]["path"],
            auth.WORKFLOWS["apply"]["receipt_predicate"],
            authorization["control"]["workflow_sha"],
        ),
    }
    return assertion_request, authorization, receipt


def reconciliation_context() -> tuple[dict, dict, dict]:
    request, authorization, receipt = context()
    assertion = reconcile.build_assertion(
        request,
        authorization=authorization,
        receipt=receipt,
        now=dt.datetime(2026, 8, 27, 1, 3, 3, 123456, tzinfo=dt.timezone.utc),
    )
    assertion_binding = fixtures.binding(
        "9001",
        "9002",
        "production-main-lock-release-failure-assertion-9001-1",
        common.sha256_bytes(common.canonical_file_bytes(assertion)),
    )
    assertion_authority = attested(
        assertion_binding,
        reconcile.WORKFLOW_PATH,
        reconcile.ASSERTION_PREDICATE,
        assertion["control"]["workflow_sha"],
    )
    observations = []
    for round_number, second in enumerate((4, 5, 6), start=1):
        observations.append(
            {
                "round": round_number,
                "observed_at": f"2026-08-27T01:03:0{second}Z",
                "main_sha": assertion["control"]["workflow_sha"],
                "rule_id": assertion["branch"]["rule_id"],
                "lock_branch": False,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
                "http_method": "POST",
                "api_operation": "graphql-query",
            }
        )
    reconciliation_request = {
        "completed_at": "2026-08-27T01:03:07Z",
        "control": copy.deepcopy(assertion["control"]),
        "observations": observations,
        "request_ledger": [
            item
            for round_number in range(1, 4)
            for item in (
                {"round": round_number, "label": "main-ref-before", "http_method": "GET", "api_operation": "rest-read"},
                {"round": round_number, "label": "branch-rule", "http_method": "POST", "api_operation": "graphql-query"},
                {"round": round_number, "label": "main-ref-after", "http_method": "GET", "api_operation": "rest-read"},
            )
        ],
        "http_request_count": 9,
        "graphql_query_count": 3,
        "branch_mutation_request_count": 0,
        "mutation_text_present": False,
    }
    return reconciliation_request, assertion, assertion_authority, receipt


class MainLockReleaseReconciliationTests(unittest.TestCase):
    def test_exact_failed_unlock_reconciles_and_pairs_receipt(self) -> None:
        request, assertion, authority, receipt = reconciliation_context()
        value = reconcile.build_reconciliation(
            request,
            assertion=assertion,
            assertion_authority=authority,
            now=dt.datetime(2026, 8, 27, 1, 3, 7, 999999, tzinfo=dt.timezone.utc),
        )
        self.assertIs(reconcile.validate_pair(value, receipt), value)

    def test_prewrite_step_failure_never_authorizes_observation(self) -> None:
        request, authorization, receipt = context()
        request["source"]["unlock_steps"][5]["conclusion"] = "failure"
        request["source"]["unlock_step_inventory_sha256"] = common.sha256_value(
            request["source"]["unlock_steps"]
        )
        with self.assertRaises(common.ReleaseError):
            reconcile.build_assertion(request, authorization=authorization, receipt=receipt, now=dt.datetime(2026, 8, 27, 1, 3, 3, tzinfo=dt.timezone.utc))

    def test_signed_preunlock_job_id_cross_splice_fails_closed(self) -> None:
        request, authorization, receipt = context()
        request["source"]["jobs"][0]["job_id"] = "999999"
        request["source"]["job_inventory_sha256"] = common.sha256_value(
            request["source"]["jobs"]
        )
        with self.assertRaises(common.ReleaseError):
            reconcile.build_assertion(request, authorization=authorization, receipt=receipt)

    def test_unlock_must_start_inside_exclusive_authorization_window(self) -> None:
        request, authorization, receipt = context()
        request["source"]["unlock_steps"][6]["started_at"] = authorization["expires_at"]
        request["source"]["unlock_step_inventory_sha256"] = common.sha256_value(request["source"]["unlock_steps"])
        with self.assertRaises(common.ReleaseError):
            reconcile.build_assertion(request, authorization=authorization, receipt=receipt)

    def test_successful_write_then_post_failure_is_recoverable(self) -> None:
        request, authorization, receipt = context()
        request["source"]["unlock_steps"][6]["conclusion"] = "success"
        request["source"]["unlock_steps"][7].update(
            {
                "conclusion": "failure",
                "started_at": "2026-08-27T01:03:02Z",
                "completed_at": "2026-08-27T01:03:03Z",
            }
        )
        request["source"]["unlock_step_inventory_sha256"] = common.sha256_value(
            request["source"]["unlock_steps"]
        )
        value = reconcile.build_assertion(
            request,
            authorization=authorization,
            receipt=receipt,
            now=dt.datetime(2026, 8, 27, 1, 3, 4, tzinfo=dt.timezone.utc),
        )
        self.assertEqual(value["source"]["unlock_steps"][6]["conclusion"], "success")

    def test_noncontiguous_post_step_numbers_are_required_to_increase(self) -> None:
        request, authorization, receipt = context()
        request["source"]["unlock_steps"][7]["number"] = request["source"]["unlock_steps"][6]["number"]
        request["source"]["unlock_step_inventory_sha256"] = common.sha256_value(
            request["source"]["unlock_steps"]
        )
        with self.assertRaises(common.ReleaseError):
            reconcile.build_assertion(request, authorization=authorization, receipt=receipt)

    def test_three_read_projection_mismatch_fails_closed(self) -> None:
        request, assertion, authority, _ = reconciliation_context()
        request["observations"][1]["main_sha"] = "0" * 40
        with self.assertRaises(common.ReleaseError):
            reconcile.build_reconciliation(request, assertion=assertion, assertion_authority=authority)

    def test_incomplete_read_only_ledger_fails_closed(self) -> None:
        request, assertion, authority, _ = reconciliation_context()
        request["request_ledger"].pop()
        with self.assertRaises(common.ReleaseError):
            reconcile.build_reconciliation(
                request, assertion=assertion, assertion_authority=authority
            )

    def test_terminal_source_name_and_verification_hashes_are_strict(self) -> None:
        request, assertion, authority, receipt = reconciliation_context()
        value = reconcile.build_reconciliation(
            request, assertion=assertion, assertion_authority=authority
        )
        mutations = {
            "workflow name": lambda candidate: candidate["source"].__setitem__(
                "workflow_name", "Rollback Production Phase"
            ),
            "attestation verification hash": lambda candidate: candidate[
                "authorities"
            ]["original_receipt"].__setitem__(
                "policy_verification_sha256", "not-a-sha256"
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                candidate = copy.deepcopy(value)
                mutate(candidate)
                with self.assertRaises(common.ReleaseError):
                    reconcile.validate_pair(candidate, receipt)

    def test_assertion_expiry_is_exclusive_with_fractional_clock(self) -> None:
        request, authorization, receipt = context()
        assertion = reconcile.build_assertion(request, authorization=authorization, receipt=receipt)
        expires = common.require_timestamp(assertion["expires_at"], "expiry")
        for checked in (expires, expires + dt.timedelta(microseconds=1)):
            with self.assertRaises(common.ReleaseError):
                reconcile.validate_assertion(assertion, now=checked)

    def test_stale_failed_source_cannot_be_replayed(self) -> None:
        request, authorization, receipt = context()
        request["issued_at"] = "2026-08-27T01:33:01Z"
        with self.assertRaises(common.ReleaseError):
            reconcile.build_assertion(request, authorization=authorization, receipt=receipt)


if __name__ == "__main__":
    unittest.main()
