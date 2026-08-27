from __future__ import annotations

import copy
import datetime as dt
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import confirm_production_orphan_lock_release as release
import finalize_production_orphan_lock as finalizer
import test_finalize_production_orphan_lock as prefixtures
import verify_production_release as common


NOW = dt.datetime(2026, 8, 27, 0, 8, 0, tzinfo=dt.timezone.utc)
CONTROL_SHA = "a" * 40
RUN_ID = "811"
RULE_ID = "QnJhbmNoUHJvdGVjdGlvblJ1bGU6MQ=="


def binding(
    *, run_id: str = RUN_ID, artifact_name: str, character: str
) -> dict[str, object]:
    return {
        "run_id": run_id,
        "run_attempt": 1,
        "artifact_id": str(int(run_id) + 100),
        "artifact_name": artifact_name,
        "artifact_digest": "sha256:" + character * 64,
        "sha256": character * 64,
    }


def root_binding() -> dict[str, object]:
    return binding(
        run_id="700",
        artifact_name="production-main-lock-apply-700-1",
        character="b",
    )


def preauthorization() -> dict[str, object]:
    return {
        "schema_version": 1,
        "authority": finalizer.AUTHORITY,
        "repository": common.REPOSITORY,
        "prepared_at": "2026-08-27T00:05:00Z",
        "expires_at": "2026-08-27T00:15:00Z",
        "state": finalizer.STATE,
        "control": {
            "workflow_sha": CONTROL_SHA,
            "workflow_path": release.WORKFLOW_PATH,
            "run_id": RUN_ID,
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": "1" * 64,
            "change_schema_sha256": "2" * 64,
            "mutation_intent_schema_sha256": "3" * 64,
            "reconciliation_schema_sha256": "4" * 64,
            "finalization_schema_sha256": "5" * 64,
            "controller_sha256": "6" * 64,
        },
        "break_glass": {
            "actor_provenance": finalizer.ACTOR_PROVENANCE,
            "authorization_scope": finalizer.AUTHORIZATION_SCOPE,
            "typed_confirmation_sha256": "7" * 64,
        },
        "orphan": {
            "branch": "main",
            "current_main_sha": CONTROL_SHA,
            "rule_id": RULE_ID,
            "rule_identity_sha256": common.sha256_bytes(RULE_ID.encode()),
            "root_acquire_intent": root_binding(),
            "original_operation": "activate",
            "original_workflow_path": ".github/workflows/apply-production-phase.yml",
            "original_control_sha": CONTROL_SHA,
            "original_run_id": "700",
            "original_run_attempt": 1,
            "mutation_intent_schema_version": 2,
        },
        "resolution": {
            "closure_kind": finalizer.CLOSURE_RECONCILIATION,
            "outcome": "committed",
            "terminal": True,
            "reconciliation_sha256": "8" * 64,
            "closure_receipt_sha256": "8" * 64,
            "orphan_rollback_sha256": None,
            "phase_state_sha256": "9" * 64,
            "provider_job_never_started": False,
            "canary_certified": True,
        },
        "authorities": {
            "mutation_intent": {},
            "lock_assertion": {},
            "reconciliation": {},
            "orphan_rollback_intent": None,
            "orphan_rollback": None,
            "phase_state": {},
        },
        "branch_action": {
            "action": "release-main-lock",
            "expected_pre_release": copy.deepcopy(release.EXPECTED_PRE_RELEASE),
            "authorized_post_release": copy.deepcopy(release.EXPECTED_POST_RELEASE),
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


def request(preauthorization_hash: str, *, ambiguous: bool = False) -> dict[str, object]:
    value: dict[str, object] = {
        "preauthorization_sha256": preauthorization_hash,
        "main_sha": CONTROL_SHA,
        "rule_id": RULE_ID,
        "rule_identity_sha256": common.sha256_bytes(RULE_ID.encode()),
        "http_methods_used": ["POST"],
        "graphql_operations_used": ["query", "mutation", "query"],
        "mutation_request_count": 1,
        "outcome": "ambiguous-reconciled" if ambiguous else "applied",
        "mutation_response_received": not ambiguous,
        "read_confirmed": True,
        "pre_release": copy.deepcopy(release.EXPECTED_PRE_RELEASE),
        "post_release": copy.deepcopy(release.EXPECTED_POST_RELEASE),
        "mutation_fingerprint_sha256": "0" * 64,
    }
    value["mutation_fingerprint_sha256"] = release._release_fingerprint(value)
    return value


def authority(preauth: dict[str, object]) -> dict[str, object]:
    file_hash = common.sha256_bytes(common.canonical_file_bytes(preauth))
    return {
        "binding": {
            **binding(
                artifact_name=f"production-orphan-lock-finalization-{RUN_ID}-1",
                character="c",
            ),
            "sha256": file_hash,
        },
        "signer_workflow": f"{common.REPOSITORY}/{release.WORKFLOW_PATH}",
        "signer_digest": CONTROL_SHA,
        "source_digest": CONTROL_SHA,
        "source_ref": "refs/heads/main",
        "runner_environment": "github-hosted",
        "provenance_predicate_type": finalizer.PROVENANCE_PREDICATE_TYPE,
        "policy_predicate_type": finalizer.PREDICATE_TYPE,
        "provenance_verification_sha256": "d" * 64,
        "policy_verification_sha256": "e" * 64,
    }


def control() -> dict[str, object]:
    return {
        "workflow_sha": CONTROL_SHA,
        "workflow_path": release.WORKFLOW_PATH,
        "run_id": RUN_ID,
        "run_attempt": 1,
        "runner_environment": "github-hosted",
        "release_schema_sha256": "f" * 64,
        "controller_sha256": "1" * 64,
    }


def built_receipt(*, ambiguous: bool = False) -> dict[str, object]:
    preauth = preauthorization()
    preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauth))
    with (
        mock.patch.object(
            finalizer,
            "validate_finalization_authorization",
            side_effect=lambda value, now=None: value,
        ),
        mock.patch.object(
            finalizer,
            "validate_attested_artifact_authority",
            side_effect=lambda value, **_kwargs: value,
        ),
    ):
        return release.build_release_receipt(
            request=request(preauth_hash, ambiguous=ambiguous),
            preauthorization=preauth,
            preauthorization_authority=authority(preauth),
            control=control(),
            completed_at=NOW,
        )


def real_receipt_context(
    *, ambiguous: bool = False,
) -> tuple[dict[str, object], dict[str, object], dict[str, object]]:
    arguments = prefixtures.chain("committed")
    source = finalizer.build_finalization_authorization(**arguments)
    source_hash = common.sha256_bytes(common.canonical_file_bytes(source))
    source_run = str(source["control"]["run_id"])
    source_authority = prefixtures.attested(
        source,
        workflow_path=finalizer.WORKFLOW_PATH,
        predicate=finalizer.PREDICATE_TYPE,
        artifact_name=f"production-orphan-lock-finalization-{source_run}-1",
        artifact_id="991",
    )
    release_request = request(source_hash, ambiguous=ambiguous)
    release_request["main_sha"] = source["orphan"]["current_main_sha"]
    release_request["rule_id"] = source["orphan"]["rule_id"]
    release_request["rule_identity_sha256"] = source["orphan"][
        "rule_identity_sha256"
    ]
    release_request["mutation_fingerprint_sha256"] = release._release_fingerprint(
        release_request
    )
    release_control = control()
    release_control["workflow_sha"] = source["control"]["workflow_sha"]
    release_control["run_id"] = source_run
    receipt = release.build_release_receipt(
        request=release_request,
        preauthorization=source,
        preauthorization_authority=source_authority,
        control=release_control,
        completed_at=NOW,
    )
    return receipt, source, source_authority


class ProductionOrphanLockReleaseTests(unittest.TestCase):
    def test_applied_and_response_loss_receipts_validate(self) -> None:
        for ambiguous in (False, True):
            with self.subTest(ambiguous=ambiguous):
                receipt, source, source_authority = real_receipt_context(
                    ambiguous=ambiguous
                )
                release.validate_release_receipt(
                    receipt,
                    preauthorization=source,
                    preauthorization_authority=source_authority,
                )

    def test_second_mutation_and_moved_authority_fail_closed(self) -> None:
        preauth = preauthorization()
        preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauth))
        for field, value in (
            ("mutation_request_count", 2),
            ("main_sha", "b" * 40),
            ("rule_id", "different-rule"),
        ):
            candidate = request(preauth_hash)
            candidate[field] = value
            candidate["mutation_fingerprint_sha256"] = release._release_fingerprint(candidate)
            with self.subTest(field=field), self.assertRaises(common.ReleaseError):
                with (
                    mock.patch.object(
                        finalizer,
                        "validate_finalization_authorization",
                        side_effect=lambda item, now=None: item,
                    ),
                    mock.patch.object(
                        finalizer,
                        "validate_attested_artifact_authority",
                        side_effect=lambda item, **_kwargs: item,
                    ),
                ):
                    release.build_release_receipt(
                        request=candidate,
                        preauthorization=preauth,
                        preauthorization_authority=authority(preauth),
                        control=control(),
                        completed_at=NOW,
                    )

    def test_wrong_preauthorization_hash_and_root_fail_closed(self) -> None:
        preauth = preauthorization()
        preauth_hash = common.sha256_bytes(common.canonical_file_bytes(preauth))
        candidate = request(preauth_hash)
        candidate["preauthorization_sha256"] = "0" * 64
        with (
            mock.patch.object(
                finalizer,
                "validate_finalization_authorization",
                side_effect=lambda item, now=None: item,
            ),
            mock.patch.object(
                finalizer,
                "validate_attested_artifact_authority",
                side_effect=lambda item, **_kwargs: item,
            ),
            self.assertRaises(common.ReleaseError),
        ):
            release.build_release_receipt(
                request=candidate,
                preauthorization=preauth,
                preauthorization_authority=authority(preauth),
                control=control(),
                completed_at=NOW,
            )
        preauth["orphan"]["root_acquire_intent"]["artifact_name"] = "wrong"
        candidate = request(common.sha256_bytes(common.canonical_file_bytes(preauth)))
        with (
            mock.patch.object(
                finalizer,
                "validate_finalization_authorization",
                side_effect=lambda item, now=None: item,
            ),
            mock.patch.object(
                finalizer,
                "validate_attested_artifact_authority",
                side_effect=lambda item, **_kwargs: item,
            ),
            self.assertRaises(common.ReleaseError),
        ):
            release.build_release_receipt(
                request=candidate,
                preauthorization=preauth,
                preauthorization_authority=authority(preauth),
                control=control(),
                completed_at=NOW,
            )

    def test_wrong_closure_and_extra_key_fail_closed(self) -> None:
        receipt, source, source_authority = real_receipt_context()
        receipt["resolution"]["closure_kind"] = finalizer.CLOSURE_NO_MUTATION
        with self.assertRaises(common.ReleaseError):
            release.validate_release_receipt(
                receipt,
                preauthorization=source,
                preauthorization_authority=source_authority,
            )
        receipt, source, source_authority = real_receipt_context()
        receipt["unexpected"] = True
        with self.assertRaises(common.ReleaseError):
            release.validate_release_receipt(
                receipt,
                preauthorization=source,
                preauthorization_authority=source_authority,
            )

    def test_standalone_validator_rejects_root_and_closure_splices(self) -> None:
        receipt, source, source_authority = real_receipt_context()
        receipt["branch_release"]["root_acquire_intent"] = binding(
            run_id="999",
            artifact_name="production-main-lock-rollback-999-1",
            character="f",
        )
        with self.assertRaises(common.ReleaseError):
            release.validate_release_receipt(
                receipt,
                preauthorization=source,
                preauthorization_authority=source_authority,
            )
        receipt, source, source_authority = real_receipt_context()
        reconciliation = receipt["resolution"]["reconciliation_sha256"]
        receipt["resolution"] = {
            "closure_kind": finalizer.CLOSURE_NO_MUTATION,
            "reconciliation_sha256": reconciliation,
            "closure_receipt_sha256": reconciliation,
            "orphan_rollback_sha256": None,
            "phase_state_sha256": None,
        }
        with self.assertRaises(common.ReleaseError):
            release.validate_release_receipt(
                receipt,
                preauthorization=source,
                preauthorization_authority=source_authority,
            )

    def test_preauthorization_alone_cannot_claim_release_success(self) -> None:
        _receipt, source, source_authority = real_receipt_context()
        with self.assertRaises(common.ReleaseError):
            release.validate_release_receipt(
                source,
                preauthorization=source,
                preauthorization_authority=source_authority,
            )

    def test_duplicate_json_key_is_rejected(self) -> None:
        with self.assertRaises(common.ReleaseError):
            common.loads_strict(b'{"state":"confirmed-released","state":"other"}')

    def test_schema_is_closed_and_exact(self) -> None:
        path = Path(__file__).with_name("production-orphan-lock-release.schema.json")
        schema = json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(
            schema["$id"],
            "https://rereply.app/schemas/production-orphan-lock-release-v1.json",
        )
        for node in _walk(schema):
            if type(node) is dict and node.get("type") == "object":
                self.assertIs(node.get("additionalProperties"), False)
                self.assertEqual(
                    set(node.get("required", [])), set(node.get("properties", {}))
                )

    def test_isolated_entrypoint_help(self) -> None:
        script = Path(__file__).with_name("confirm_production_orphan_lock_release.py")
        completed = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(script), "--help"],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)


def _walk(value: object) -> list[object]:
    output = [value]
    if type(value) is dict:
        for child in value.values():
            output.extend(_walk(child))
    elif type(value) is list:
        for child in value:
            output.extend(_walk(child))
    return output


if __name__ == "__main__":
    unittest.main()
