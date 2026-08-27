from __future__ import annotations

import copy
import datetime as dt
import json
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import finalize_production_orphan_lock as finalizer
import reconcile_production_orphan_lock_release as release_reconcile
import test_finalize_production_orphan_lock as fixtures
import verify_production_release as common


NOW = dt.datetime(2026, 8, 27, 0, 8, 0, tzinfo=dt.timezone.utc)
COMPLETED = dt.datetime(2026, 8, 27, 0, 9, 0, tzinfo=dt.timezone.utc)
CONTROL_SHA = "a" * 40


def _job(job_id: int, name: str, conclusion: str) -> dict[str, object]:
    return {
        "job_id": str(job_id),
        "name": name,
        "status": "completed",
        "conclusion": conclusion,
        "started_at": "2026-08-27T00:07:00Z",
        "completed_at": "2026-08-27T00:07:30Z",
    }


def _original(
    preauth_authority: dict[str, object],
    classification: str = "preauthorization-only",
) -> dict[str, object]:
    run_id = str(preauth_authority["binding"]["run_id"])
    jobs = [
        _job(901, release_reconcile.FINALIZER_JOBS[0], "success"),
        _job(902, release_reconcile.FINALIZER_JOBS[1], "success"),
        _job(903, release_reconcile.FINALIZER_JOBS[2], "success"),
        _job(904, release_reconcile.FINALIZER_JOBS[3], "failure"),
    ]
    artifacts = [
        {"kind": "preauthorization", "binding": preauth_authority["binding"]}
    ]
    unsigned_binding = None
    if classification != "preauthorization-only":
        unsigned_binding = {
            "run_id": run_id,
            "run_attempt": 1,
            "artifact_id": "992",
            "artifact_name": f"unsigned-production-orphan-lock-release-{run_id}-1",
            "artifact_digest": "sha256:" + "8" * 64,
            "sha256": "9" * 64,
        }
        artifacts.append(
            {"kind": "unsigned-release-receipt", "binding": unsigned_binding}
        )
    inventory_sha = common.sha256_value(artifacts)
    return {
        "workflow_sha": CONTROL_SHA,
        "workflow_path": finalizer.WORKFLOW_PATH,
        "workflow_name": "Finalize Production Orphan Lock",
        "run_id": run_id,
        "run_attempt": 1,
        "event": "workflow_dispatch",
        "head_branch": "main",
        "status": "completed",
        "conclusion": "failure",
        "jobs": jobs,
        "job_inventory_sha256": common.sha256_value(jobs),
        "artifacts": artifacts,
        "artifact_inventory_sha256": inventory_sha,
        "receipt_truth": {
            "observed_at": "2026-08-27T00:07:45Z",
            "classification": classification,
            "source_artifact_inventory_sha256": inventory_sha,
            "signed_release_artifact_present_at_observation": False,
            "unsigned_receipt_binding": unsigned_binding,
            "provenance_match_count": 1 if classification.startswith("attested-") else 0,
            "policy_match_count": 1 if classification.startswith("attested-") else 0,
            "provenance_query_sha256": "4" * 64,
            "policy_query_sha256": "5" * 64,
            "provenance_verification_sha256": "6" * 64 if classification.startswith("attested-") else None,
            "policy_verification_sha256": "7" * 64 if classification.startswith("attested-") else None,
        },
        "static_max_branch_mutations": 1,
    }


def _context(
    classification: str = "preauthorization-only",
) -> dict[str, object]:
    preauth = finalizer.build_finalization_authorization(**fixtures.chain("committed"))
    run_id = str(preauth["control"]["run_id"])
    preauth_authority = fixtures.attested(
        preauth,
        workflow_path=finalizer.WORKFLOW_PATH,
        predicate=finalizer.PREDICATE_TYPE,
        artifact_name=f"production-orphan-lock-finalization-{run_id}-1",
        artifact_id="990",
    )
    preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauth))
    orphan = preauth["orphan"]
    phrase = f"RECONCILE UNLOCKED PRODUCTION {run_id} {preauth_hash}"
    request = {
        "actor_provenance": release_reconcile.ACTOR_PROVENANCE,
        "authorization_scope": release_reconcile.AUTHORIZATION_SCOPE,
        "current_main_sha": orphan["current_main_sha"],
        "rule_id": orphan["rule_id"],
        "rule_identity_sha256": orphan["rule_identity_sha256"],
        "preauthorization_sha256": preauth_hash,
        "typed_confirmation_sha256": common.sha256_bytes(phrase.encode()),
        "original_finalizer": _original(preauth_authority, classification),
    }
    control = release_reconcile._assertion_control(
        workflow_sha=CONTROL_SHA,
        workflow_run_id="601",
        workflow_run_attempt=1,
        assertion_schema_sha256="1" * 64,
        controller_sha256="2" * 64,
    )
    assertion = release_reconcile.build_assertion(
        request=request,
        preauthorization=preauth,
        preauthorization_authority=preauth_authority,
        control=control,
        created_at=NOW,
    )
    assertion_authority = fixtures.attested(
        assertion,
        workflow_path=release_reconcile.WORKFLOW_PATH,
        predicate=release_reconcile.ASSERTION_PREDICATE_TYPE,
        artifact_name="production-orphan-lock-release-assertion-601-1",
        artifact_id="991",
    )
    rule = {
        "rule_id": orphan["rule_id"],
        "rule_identity_sha256": orphan["rule_identity_sha256"],
        "pattern": "main",
        **release_reconcile.EXPECTED_UNLOCKED,
    }
    observation = {
        "ordered_requests": copy.deepcopy(release_reconcile.REQUEST_LEDGER),
        "http_request_count": 4,
        "graphql_query_count": 2,
        "branch_mutation_request_count": 0,
        "mutation_text_present": False,
        "observation_rounds": [
            {"round": 1, "main_sha": orphan["current_main_sha"], "rule": rule},
            {"round": 2, "main_sha": orphan["current_main_sha"], "rule": copy.deepcopy(rule)},
        ],
        "double_read_equal": True,
        "read_confirmed": True,
        "source_receipt_truth": {
            **copy.deepcopy(assertion["original_finalizer"]["receipt_truth"]),
            "observed_at": "2026-08-27T00:08:45Z",
            "provenance_query_sha256": "6" * 64,
            "policy_query_sha256": "7" * 64,
        },
    }
    receipt_control = release_reconcile._reconciliation_control(
        workflow_sha=CONTROL_SHA,
        workflow_run_id="601",
        workflow_run_attempt=1,
        assertion_schema_sha256="1" * 64,
        reconciliation_schema_sha256="3" * 64,
        controller_sha256="2" * 64,
    )
    return {
        "observation_request": observation,
        "assertion": assertion,
        "assertion_authority": assertion_authority,
        "preauthorization": preauth,
        "preauthorization_authority": preauth_authority,
        "control": receipt_control,
        "completed_at": COMPLETED,
    }


class OrphanLockReleaseReconciliationTests(unittest.TestCase):
    def test_valid_two_round_observation_builds_terminal_receipt(self) -> None:
        context = _context()
        receipt = release_reconcile.build_reconciliation(**context)
        self.assertEqual(
            receipt["classification"]["outcome"], release_reconcile.OUTCOME
        )
        self.assertIsNone(receipt["classification"]["mutation_count_observed"])
        self.assertEqual(
            receipt["classification"]["source_receipt_classification"],
            "preauthorization-only",
        )
        release_reconcile.validate_reconciliation(
            receipt,
            assertion=context["assertion"],
            assertion_authority=context["assertion_authority"],
            preauthorization=context["preauthorization"],
            preauthorization_authority=context["preauthorization_authority"],
        )

    def test_assertion_expiry_is_exclusive_at_boundary_and_fraction(self) -> None:
        assertion = _context()["assertion"]
        expires = common.require_timestamp(
            assertion["expires_at"], "test release assertion expiry"
        )
        for checked in (expires, expires + dt.timedelta(microseconds=1)):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    release_reconcile.validate_assertion(assertion, now=checked)

    def test_all_three_source_truth_classes_are_exact(self) -> None:
        for classification in (
            "preauthorization-only",
            "unsigned-unattested",
            "attested-receipt-upload-incomplete",
        ):
            context = _context(classification)
            receipt = release_reconcile.build_reconciliation(**context)
            self.assertEqual(
                receipt["classification"]["source_receipt_classification"],
                classification,
            )

    def test_partial_duplicate_or_spliced_attestation_truth_fails(self) -> None:
        for mutate in (
            lambda truth: truth.__setitem__("provenance_match_count", 1),
            lambda truth: truth.__setitem__("policy_match_count", 1),
            lambda truth: truth.__setitem__("provenance_match_count", 2),
            lambda truth: truth.__setitem__("unsigned_receipt_binding", None),
        ):
            context = _context("unsigned-unattested")
            mutate(context["assertion"]["original_finalizer"]["receipt_truth"])
            with self.assertRaises(common.ReleaseError):
                release_reconcile.build_reconciliation(**context)

    def test_source_truth_timestamp_must_follow_source_and_precede_receipt(self) -> None:
        context = _context()
        context["assertion"]["original_finalizer"]["receipt_truth"][
            "observed_at"
        ] = "2026-08-27T00:07:00Z"
        with self.assertRaises(common.ReleaseError):
            release_reconcile.build_reconciliation(**context)
        context = _context()
        context["observation_request"]["source_receipt_truth"][
            "observed_at"
        ] = "2026-08-27T00:09:01Z"
        with self.assertRaises(common.ReleaseError):
            release_reconcile.build_reconciliation(**context)

    def test_observation_rejects_mutation_text_extra_or_reordered_requests(self) -> None:
        for mutate in (
            lambda value: value.__setitem__("mutation_text_present", True),
            lambda value: value["ordered_requests"].append(
                copy.deepcopy(release_reconcile.REQUEST_LEDGER[0])
            ),
            lambda value: value["ordered_requests"].reverse(),
        ):
            context = _context()
            mutate(context["observation_request"])
            with self.assertRaises(common.ReleaseError):
                release_reconcile.build_reconciliation(**context)

    def test_observation_rejects_single_or_differing_rounds(self) -> None:
        for mutate in (
            lambda value: value["observation_rounds"].pop(),
            lambda value: value["observation_rounds"][1].__setitem__(
                "main_sha", "f" * 40
            ),
        ):
            context = _context()
            mutate(context["observation_request"])
            with self.assertRaises(common.ReleaseError):
                release_reconcile.build_reconciliation(**context)

    def test_locked_or_moved_live_state_is_indeterminate(self) -> None:
        for field, value in (("lock_branch", True), ("rule_id", "different")):
            context = _context()
            for observed in context["observation_request"]["observation_rounds"]:
                observed["rule"][field] = value
                if field == "rule_id":
                    observed["rule"]["rule_identity_sha256"] = common.sha256_bytes(
                        value.encode()
                    )
            with self.assertRaises(common.ReleaseError):
                release_reconcile.build_reconciliation(**context)

    def test_original_finalizer_must_have_no_signed_receipt(self) -> None:
        context = _context()
        context["assertion"]["original_finalizer"]["receipt_truth"][
            "signed_release_artifact_present_at_observation"
        ] = True
        with self.assertRaises(common.ReleaseError):
            release_reconcile.build_reconciliation(**context)

    def test_receipt_validation_rejects_source_assertion_splice(self) -> None:
        context = _context()
        receipt = release_reconcile.build_reconciliation(**context)
        other = _context()
        other["assertion"]["typed_confirmation_sha256"] = "f" * 64
        with self.assertRaises(common.ReleaseError):
            release_reconcile.validate_reconciliation(
                receipt,
                assertion=other["assertion"],
                assertion_authority=context["assertion_authority"],
                preauthorization=context["preauthorization"],
                preauthorization_authority=context["preauthorization_authority"],
            )

    def test_receipt_validation_rejects_source_preauthorization_splice(self) -> None:
        context = _context()
        receipt = release_reconcile.build_reconciliation(**context)
        other = copy.deepcopy(context["preauthorization"])
        other["orphan"]["rule_id"] = "different"
        other["orphan"]["rule_identity_sha256"] = common.sha256_bytes(b"different")
        with self.assertRaises(common.ReleaseError):
            release_reconcile.validate_reconciliation(
                receipt,
                assertion=context["assertion"],
                assertion_authority=context["assertion_authority"],
                preauthorization=other,
                preauthorization_authority=context["preauthorization_authority"],
            )

    def test_unknown_keys_and_invalid_rule_identifier_fail_closed(self) -> None:
        context = _context()
        context["observation_request"]["extra"] = True
        with self.assertRaises(common.ReleaseError):
            release_reconcile.build_reconciliation(**context)
        context = _context()
        context["observation_request"]["observation_rounds"][0]["rule"][
            "rule_id"
        ] = "bad rule id"
        with self.assertRaises(common.ReleaseError):
            release_reconcile.build_reconciliation(**context)

    def test_controller_isolated_help_and_no_network_client(self) -> None:
        controller = Path(release_reconcile.__file__)
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(controller), "--help"],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        source = controller.read_text(encoding="utf-8")
        for forbidden in (
            "import requests", "import urllib", "import http.client",
            "GITHUB_TOKEN", "GH_TOKEN", "lockBranch: false",
        ):
            self.assertNotIn(forbidden, source)

    def test_schemas_are_strict_stdlib_structures(self) -> None:
        for filename in (
            "production-orphan-lock-release-assertion.schema.json",
            "production-orphan-lock-release-reconciliation.schema.json",
        ):
            schema = json.loads(Path(__file__).with_name(filename).read_text())
            self.assertFalse(schema["additionalProperties"])
            self.assertEqual(len(schema["required"]), len(set(schema["required"])))


if __name__ == "__main__":
    unittest.main()
