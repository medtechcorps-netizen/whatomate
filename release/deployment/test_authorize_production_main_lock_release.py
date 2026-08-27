#!/usr/bin/env python3

from __future__ import annotations

import copy
import datetime as dt
import json
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import authorize_production_main_lock_release as authorization
import test_verify_production_release as release_fixtures
import verify_production_release as common


SHA = "a" * 40
HASH = "b" * 64
ISSUED = dt.datetime(2026, 8, 27, 1, 2, 3, tzinfo=dt.timezone.utc)


def binding(run_id: str, artifact_id: str, name: str, sha256: str = HASH) -> dict:
    return {
        "run_id": run_id,
        "run_attempt": 1,
        "artifact_id": artifact_id,
        "artifact_name": name,
        "artifact_digest": "sha256:" + "c" * 64,
        "sha256": sha256,
    }


def request_and_receipt(operation: str = "apply") -> tuple[dict, dict]:
    receipt = release_fixtures.apply_receipt()
    if operation == "rollback":
        receipt["authority"] = "production-phase-rollback-receipt"
        receipt["control"]["workflow_path"] = ".github/workflows/rollback-production-phase.yml"
        receipt["lineage"].update(
            {"event_sequence": 2, "operation": "rollback", "from": "bridge", "to": "baseline"}
        )
        receipt["lineage"]["phase"] = "baseline"
        receipt["rollback"] = common.ROLLBACK_FLOORS["baseline"]
    run_id = str(receipt["control"]["run_id"])
    receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
    policy = authorization.WORKFLOWS[operation]
    jobs = []
    for index, name in enumerate(policy["jobs"], start=1):
        jobs.append(
            {
                "job_id": str(1000 + index),
                "name": name,
                "status": "in_progress" if index == 8 else "completed",
                "conclusion": None if index == 8 else "success",
            }
        )
    artifacts = {
        "acquire_intent": binding(run_id, "2001", f"production-main-lock-{operation}-{run_id}-1"),
        "mutation_intent": binding(run_id, "2002", f"production-mutation-intent-{operation}-{run_id}-1"),
        "main_lock_proof": binding(run_id, "2003", f"production-main-lock-proof-{operation}-{run_id}-1"),
        "unsigned_receipt": binding(run_id, "2004", f"unsigned-production-phase-{operation}-{run_id}-1", receipt_hash),
        "signed_receipt": binding(run_id, "2005", f"production-phase-{operation}-{run_id}-1", receipt_hash),
    }
    rule_id = "QnJhbmNoUHJvdGVjdGlvblJ1bGUx"
    request = {
        "operation": operation,
        "issued_at": ISSUED.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "control": {
            "workflow_sha": receipt["control"]["workflow_sha"],
            "workflow_path": policy["path"],
            "run_id": run_id,
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "authorization_schema_sha256": HASH,
            "controller_sha256": "d" * 64,
        },
        "jobs": jobs,
        "artifacts": artifacts,
        "branch": {
            "main_sha": receipt["control"]["workflow_sha"],
            "rule_id": rule_id,
            "rule_identity_sha256": common.sha256_bytes(rule_id.encode("utf-8")),
            "lock_branch": True,
            "is_admin_enforced": True,
            "lock_allows_fetch_and_merge": False,
        },
        "receipt_attestations": {
            "signer_workflow": f"{common.REPOSITORY}/{policy['path']}",
            "signer_digest": receipt["control"]["workflow_sha"],
            "source_digest": receipt["control"]["workflow_sha"],
            "source_ref": "refs/heads/main",
            "runner_environment": "github-hosted",
            "provenance_predicate_type": "https://slsa.dev/provenance/v1",
            "policy_predicate_type": policy["receipt_predicate"],
            "provenance_verification_sha256": "e" * 64,
            "policy_verification_sha256": "f" * 64,
        },
    }
    return request, receipt


class ReleaseAuthorizationTests(unittest.TestCase):
    def test_builds_exact_apply_authorization(self) -> None:
        request, receipt = request_and_receipt()
        value = authorization.build_authorization(
            request, receipt, now=ISSUED + dt.timedelta(seconds=1, microseconds=1)
        )
        self.assertEqual(value["expires_at"], "2026-08-27T01:12:03Z")
        self.assertIs(
            authorization.validate_authorization(
                value, receipt=receipt, now=ISSUED + dt.timedelta(seconds=599, microseconds=999999)
            ),
            value,
        )

    def test_expiry_is_exclusive_and_preserves_fractional_clock(self) -> None:
        request, receipt = request_and_receipt()
        value = authorization.build_authorization(request, receipt, now=ISSUED)
        for checked in (
            ISSUED + dt.timedelta(seconds=600),
            ISSUED + dt.timedelta(seconds=600, microseconds=1),
        ):
            with self.assertRaises(common.ReleaseError):
                authorization.validate_authorization(value, receipt=receipt, now=checked)

    def test_cross_splice_and_job_drift_fail_closed(self) -> None:
        request, receipt = request_and_receipt()
        for mutate in (
            lambda value: value["jobs"][0].__setitem__("conclusion", "failure"),
            lambda value: value["artifacts"]["signed_receipt"].__setitem__("sha256", "0" * 64),
            lambda value: value["branch"].__setitem__("lock_branch", False),
        ):
            candidate = copy.deepcopy(request)
            mutate(candidate)
            with self.assertRaises(common.ReleaseError):
                authorization.build_authorization(candidate, receipt, now=ISSUED)

    def test_schema_accepts_subject_and_rejects_extra_key(self) -> None:
        try:
            import jsonschema
        except ImportError:  # pragma: no cover
            self.skipTest("jsonschema is unavailable")
        request, receipt = request_and_receipt()
        value = authorization.build_authorization(request, receipt, now=ISSUED)
        schema = json.loads(
            (ROOT / "release/deployment/production-main-lock-release-authorization.schema.json").read_text(encoding="utf-8")
        )
        jsonschema.Draft202012Validator(schema).validate(value)
        value["unexpected"] = True
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.Draft202012Validator(schema).validate(value)


if __name__ == "__main__":
    unittest.main()
