from __future__ import annotations

import copy
import datetime as dt
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any, Callable, Mapping


sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_release as release


LOCKED = {
    "lock_branch": True,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
UNLOCKED = {
    "lock_branch": False,
    "is_admin_enforced": True,
    "lock_allows_fetch_and_merge": False,
}
ENDPOINT_LABELS = [
    "app-health",
    "app-ready",
    "meta-live",
    "meta-ready",
    "gmail-live",
    "gmail-ready",
]


def sha(character: str) -> str:
    return character * 64


def digest(character: str) -> str:
    return "sha256:" + character * 64


def binding(
    *,
    run_id: str,
    artifact_id: str,
    artifact_name: str,
    file_sha256: str,
    digest_character: str,
) -> dict[str, Any]:
    return {
        "run_id": run_id,
        "run_attempt": 1,
        "artifact_id": artifact_id,
        "artifact_name": artifact_name,
        "artifact_digest": digest(digest_character),
        "sha256": file_sha256,
    }


def image_records(character: str) -> list[dict[str, str]]:
    return release.sanitized_image_records(
        {
            "web": digest(character),
            "meta-relay": digest(character),
            "gmail-relay": digest(character),
        }
    )


def legacy_before() -> dict[str, Any]:
    return {
        "app_identity_sha256": sha("1"),
        "default_ingress_sha256": sha("2"),
        "app_updated_at_sha256": sha("3"),
        "active_deployment_identity_sha256": sha("4"),
        "canonical_spec_sha256": sha("5"),
        "environment_values_sha256": sha("6"),
        "non_source_projection_sha256": sha("7"),
        "source_mode": "legacy-git",
        "images": [],
    }


def digest_before(character: str = "8") -> dict[str, Any]:
    value = legacy_before()
    value["source_mode"] = "digest-images"
    value["images"] = image_records(character)
    return value


def desired_projection(character: str = "9") -> dict[str, Any]:
    return {
        "canonical_spec_sha256": sha("a"),
        "environment_values_sha256": sha("6"),
        "non_source_projection_sha256": sha("7"),
        "source_mode": "digest-images",
        "images": image_records(character),
        "migration_job": "rereply-rls-migrate",
        "migration_digest": digest(character),
    }


def mutation_request(before: Mapping[str, Any], desired: Mapping[str, Any]) -> dict[str, Any]:
    before_hash = release.sha256_value(before)
    desired_hash = release.sha256_value(desired)
    return {
        "http_method": "PUT",
        "endpoint_label": "app",
        "update_all_source_versions": False,
        "before_sha256": before_hash,
        "desired_sha256": desired_hash,
        "mutation_fingerprint_sha256": release.sha256_value(
            {
                "before_sha256": before_hash,
                "desired_sha256": desired_hash,
                "http_method": "PUT",
                "endpoint_label": "app",
                "update_all_source_versions": False,
            }
        ),
    }


def fresh_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0)


def make_intent(
    proof_operation: str,
    *,
    now: dt.datetime,
) -> dict[str, Any]:
    if proof_operation not in {"apply", "rollback", "orphan-rollback"}:
        raise AssertionError("unsupported fixture operation")
    is_apply = proof_operation == "apply"
    is_inherited = proof_operation == "orphan-rollback"
    operation = "activate" if is_apply else "rollback"
    workflow_path = release.LOCK_PROOF_WORKFLOWS[proof_operation]
    run_id = {"apply": "101", "rollback": "501", "orphan-rollback": "601"}[
        proof_operation
    ]
    workflow_sha = {
        "apply": "a" * 40,
        "rollback": "b" * 40,
        "orphan-rollback": "c" * 40,
    }[proof_operation]
    before = legacy_before() if is_apply else digest_before("8")
    desired = desired_projection("9")
    current = {
        "kind": "apply-receipt",
        **binding(
            run_id="401",
            artifact_id="1401",
            artifact_name="production-phase-apply-401-1",
            file_sha256=sha("b"),
            digest_character="b",
        ),
    }
    if is_apply:
        lineage = {
            "event_sequence": 1,
            "phase_ordinal": 1,
            "operation": "activate",
            "from": "genesis",
            "to": "baseline",
            "predecessor_kind": "genesis",
            "predecessor_state_sha256": sha("0"),
            "phase": "baseline",
            "phase_source_sha": "d" * 40,
        }
        authorities: dict[str, Any] = {
            "rollout_plan_sha256": sha("1"),
            "rollout_authority": binding(
                run_id="91",
                artifact_id="191",
                artifact_name="exact-four-phase-rollout-91-1",
                file_sha256=sha("1"),
                digest_character="1",
            ),
            "production_plan": binding(
                run_id="92",
                artifact_id="192",
                artifact_name="verified-production-plan-92-1",
                file_sha256=sha("2"),
                digest_character="2",
            ),
            "recovery": binding(
                run_id="93",
                artifact_id="193",
                artifact_name="production-recovery-readiness-93-1",
                file_sha256=sha("3"),
                digest_character="3",
            ),
            "predecessor_state": {
                "kind": "genesis",
                "run_id": None,
                "run_attempt": None,
                "artifact_id": None,
                "artifact_name": None,
                "artifact_digest": None,
                "sha256": sha("0"),
            },
        }
    else:
        lineage = {
            "event_sequence": 3,
            "phase_ordinal": 1,
            "operation": "rollback",
            "from": "bridge",
            "to": "baseline",
            "predecessor_kind": "apply-receipt",
            "predecessor_state_sha256": current["sha256"],
            "phase": "baseline",
            "phase_source_sha": "d" * 40,
        }
        authorities = {
            "rollout_plan_sha256": sha("1"),
            "current_state": current,
            "target_state": binding(
                run_id="402",
                artifact_id="1402",
                artifact_name="production-phase-state-baseline-402-1",
                file_sha256=sha("c"),
                digest_character="c",
            ),
            "recovery": binding(
                run_id="403",
                artifact_id="1403",
                artifact_name="production-recovery-readiness-403-1",
                file_sha256=sha("d"),
                digest_character="d",
            ),
            "target_authority": {"production_plan_sha256": sha("e")},
        }
    rule_id = "BPR_kwDO_rereply_lock_01"
    root_run_id = "101" if is_inherited else run_id
    lock = {
        "mode": "planned",
        "strategy": "inherit" if is_inherited else "acquire",
        "branch": "main",
        "rule_id": rule_id,
        "rule_identity_sha256": release.sha256_bytes(rule_id.encode("utf-8")),
        "expected_pre_lock": copy.deepcopy(LOCKED if is_inherited else UNLOCKED),
        "expected_post_lock": copy.deepcopy(LOCKED),
        "root_acquire_intent": binding(
            run_id=root_run_id,
            artifact_id="1901",
            artifact_name=f"production-main-lock-acquire-{root_run_id}-1",
            file_sha256=sha("4"),
            digest_character="4",
        ),
        "owner_operation": "apply" if is_inherited else proof_operation,
        "owner_run_id": root_run_id if is_inherited else run_id,
        "owner_run_attempt": 1,
        "owner_control_sha": "a" * 40 if is_inherited else workflow_sha,
        "owner_intent_sha256": sha("f") if is_inherited else None,
    }
    prepared = now - dt.timedelta(seconds=30)
    expires = now + dt.timedelta(minutes=10)
    intent = {
        "schema_version": 2,
        "authority": "production-mutation-intent",
        "repository": release.REPOSITORY,
        "prepared_at": release.format_timestamp(prepared),
        "expires_at": release.format_timestamp(expires),
        "control": {
            "workflow_sha": workflow_sha,
            "workflow_path": workflow_path,
            "run_id": run_id,
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": sha("5"),
            "change_schema_sha256": sha("6"),
            "mutation_intent_schema_sha256": sha("7"),
            "controller_sha256": sha("8"),
        },
        "operation": operation,
        "lineage": lineage,
        "authorities": authorities,
        "lock": lock,
        "before": before,
        "desired": desired,
        "mutation": mutation_request(before, desired),
        "rollback": copy.deepcopy(release.ROLLBACK_FLOORS["baseline"]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": copy.deepcopy(ENDPOINT_LABELS),
            "route_contract_sha256": sha("9"),
        },
    }
    return release.validate_mutation_intent(intent, now=now)


def intent_binding(intent: Mapping[str, Any], proof_operation: str) -> dict[str, Any]:
    run_id = str(intent["control"]["run_id"])
    return binding(
        run_id=run_id,
        artifact_id=str(2000 + int(run_id)),
        artifact_name=f"production-mutation-intent-{proof_operation}-{run_id}-1",
        file_sha256=release.sha256_bytes(release.canonical_file_bytes(intent)),
        digest_character="a",
    )


def proof_request(intent: Mapping[str, Any], proof_operation: str) -> dict[str, Any]:
    inherited = intent["lock"]["strategy"] == "inherit"
    return {
        "operation": proof_operation,
        "main_sha": intent["control"]["workflow_sha"],
        "rule_id": intent["lock"]["rule_id"],
        "rule_identity_sha256": intent["lock"]["rule_identity_sha256"],
        "pre_lock": copy.deepcopy(intent["lock"]["expected_pre_lock"]),
        "post_lock": copy.deepcopy(intent["lock"]["expected_post_lock"]),
        "http_methods_used": ["POST"],
        "graphql_operations_used": (
            ["query"] if inherited else ["query", "mutation", "query"]
        ),
        "mutation_request_count": 0 if inherited else 1,
        "outcome": "already-locked-inherited" if inherited else "applied",
        "read_confirmed": True,
    }


def proof_control(intent: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "workflow_sha": intent["control"]["workflow_sha"],
        "workflow_path": intent["control"]["workflow_path"],
        "run_id": intent["control"]["run_id"],
        "run_attempt": intent["control"]["run_attempt"],
        "runner_environment": "github-hosted",
    }


def make_proof(
    proof_operation: str,
    *,
    now: dt.datetime,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any], dict[str, Any]]:
    intent = make_intent(proof_operation, now=now)
    authority = intent_binding(intent, proof_operation)
    request = proof_request(intent, proof_operation)
    proof = release.build_main_lock_proof(
        request=request,
        mutation_intent=intent,
        mutation_intent_binding=authority,
        control=proof_control(intent),
        now=now,
    )
    return proof, intent, authority, request


def write_canonical(path: Path, value: Any) -> None:
    path.write_bytes(release.canonical_file_bytes(value))


class ProductionMainLockProofRuntimeTests(unittest.TestCase):
    def test_build_and_validate_all_lock_strategies(self) -> None:
        now = fresh_now()
        expected = {
            "apply": ("acquire", 1, "applied"),
            "rollback": ("acquire", 1, "applied"),
            "orphan-rollback": ("inherit", 0, "already-locked-inherited"),
        }
        for operation, (strategy, count, outcome) in expected.items():
            with self.subTest(operation=operation):
                proof, intent, _authority, _request = make_proof(operation, now=now)
                self.assertIs(
                    release.validate_main_lock_proof(
                        proof, mutation_intent=intent, now=now
                    ),
                    proof,
                )
                self.assertEqual(proof["operation"], operation)
                self.assertEqual(proof["branch"]["strategy"], strategy)
                self.assertEqual(
                    proof["acquisition"]["mutation_request_count"], count
                )
                self.assertEqual(proof["acquisition"]["outcome"], outcome)
                self.assertTrue(proof["acquisition"]["read_confirmed"])

    def test_acquire_accepts_ambiguous_reconciled_only_with_exact_ledger(self) -> None:
        now = fresh_now()
        intent = make_intent("rollback", now=now)
        request = proof_request(intent, "rollback")
        request["outcome"] = "ambiguous-reconciled"
        proof = release.build_main_lock_proof(
            request=request,
            mutation_intent=intent,
            mutation_intent_binding=intent_binding(intent, "rollback"),
            control=proof_control(intent),
            now=now,
        )
        release.validate_main_lock_proof(proof, mutation_intent=intent, now=now)

    def test_tampered_proof_bindings_and_lock_evidence_fail_closed(self) -> None:
        now = fresh_now()
        proof, intent, _authority, _request = make_proof("apply", now=now)
        mutations: dict[str, Callable[[dict[str, Any]], None]] = {
            "intent hash": lambda value: value["mutation_intent"].__setitem__(
                "sha256", sha("0")
            ),
            "intent artifact": lambda value: value["mutation_intent"].__setitem__(
                "artifact_name", "production-mutation-intent-rollback-101-1"
            ),
            "root": lambda value: value["root_acquire_intent"].__setitem__(
                "sha256", sha("0")
            ),
            "rule": lambda value: value["branch"].__setitem__(
                "rule_id", "BPR_kwDO_rereply_other"
            ),
            "rule identity": lambda value: value["branch"].__setitem__(
                "rule_identity_sha256", sha("0")
            ),
            "main": lambda value: value["branch"].__setitem__(
                "main_sha", "0" * 40
            ),
            "run": lambda value: value["control"].__setitem__("run_id", "102"),
            "count": lambda value: value["acquisition"].__setitem__(
                "mutation_request_count", 0
            ),
            "outcome": lambda value: value["acquisition"].__setitem__(
                "outcome", "already-locked-inherited"
            ),
            "fingerprint": lambda value: value["acquisition"].__setitem__(
                "mutation_fingerprint_sha256", sha("0")
            ),
            "pre lock": lambda value: value["branch"]["pre_lock"].__setitem__(
                "lock_branch", True
            ),
            "post lock": lambda value: value["branch"]["post_lock"].__setitem__(
                "lock_branch", False
            ),
            "timestamp before intent": lambda value: value.__setitem__(
                "created_at", "2020-01-01T00:00:00Z"
            ),
            "timestamp after intent": lambda value: value.__setitem__(
                "created_at", "2030-01-01T00:00:00Z"
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                candidate = copy.deepcopy(proof)
                mutate(candidate)
                with self.assertRaises(release.ReleaseError):
                    release.validate_main_lock_proof(
                        candidate, mutation_intent=intent, now=now
                    )

    def test_inherited_lock_cannot_claim_acquire_semantics(self) -> None:
        now = fresh_now()
        proof, intent, _authority, _request = make_proof(
            "orphan-rollback", now=now
        )
        for mutate in (
            lambda value: value["branch"].__setitem__("strategy", "acquire"),
            lambda value: value["acquisition"].__setitem__(
                "graphql_operations_used", ["query", "mutation", "query"]
            ),
            lambda value: value["acquisition"].__setitem__(
                "mutation_request_count", 1
            ),
            lambda value: value["acquisition"].__setitem__("outcome", "applied"),
        ):
            candidate = copy.deepcopy(proof)
            mutate(candidate)
            with self.assertRaises(release.ReleaseError):
                release.validate_main_lock_proof(
                    candidate, mutation_intent=intent, now=now
                )

    def test_isolated_cli_builds_and_validates_each_operation(self) -> None:
        script = str(Path(release.__file__).resolve())
        environment = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1"}
        for operation in ("apply", "rollback", "orphan-rollback"):
            with self.subTest(operation=operation), tempfile.TemporaryDirectory() as raw:
                temporary = Path(raw)
                now = fresh_now()
                intent = make_intent(operation, now=now)
                authority = intent_binding(intent, operation)
                request = proof_request(intent, operation)
                request_path = temporary / "request.json"
                intent_path = temporary / "intent.json"
                authority_path = temporary / "authority.json"
                proof_path = temporary / "proof.json"
                write_canonical(request_path, request)
                write_canonical(intent_path, intent)
                write_canonical(authority_path, authority)
                build = subprocess.run(
                    [
                        sys.executable,
                        "-I",
                        "-S",
                        "-B",
                        script,
                        "build-main-lock-proof",
                        "--request",
                        str(request_path),
                        "--mutation-intent",
                        str(intent_path),
                        "--mutation-intent-authority",
                        str(authority_path),
                        "--workflow-path",
                        intent["control"]["workflow_path"],
                        "--workflow-sha",
                        intent["control"]["workflow_sha"],
                        "--workflow-run-id",
                        intent["control"]["run_id"],
                        "--workflow-run-attempt",
                        "1",
                        "--runner-temp",
                        str(temporary),
                        "--output",
                        str(proof_path),
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=environment,
                )
                self.assertEqual(build.returncode, 0, build.stderr)
                proof_hash = release.sha256_bytes(proof_path.read_bytes())
                validate = subprocess.run(
                    [
                        sys.executable,
                        "-I",
                        "-S",
                        "-B",
                        script,
                        "validate-main-lock-proof",
                        "--proof",
                        str(proof_path),
                        "--proof-sha256",
                        proof_hash,
                        "--mutation-intent",
                        str(intent_path),
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=environment,
                )
                self.assertEqual(validate.returncode, 0, validate.stderr)

    def test_isolated_cli_rejects_tampering_and_filename_bearing_hash(self) -> None:
        script = str(Path(release.__file__).resolve())
        environment = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1"}
        with tempfile.TemporaryDirectory() as raw:
            temporary = Path(raw)
            now = fresh_now()
            proof, intent, _authority, _request = make_proof("apply", now=now)
            intent_path = temporary / "intent.json"
            proof_path = temporary / "proof.json"
            write_canonical(intent_path, intent)
            write_canonical(proof_path, proof)
            proof_hash = release.sha256_bytes(proof_path.read_bytes())
            command = [
                sys.executable,
                "-I",
                "-S",
                "-B",
                script,
                "validate-main-lock-proof",
                "--proof",
                str(proof_path),
                "--proof-sha256",
                f"{proof_hash}  production-main-lock-proof.json",
                "--mutation-intent",
                str(intent_path),
            ]
            filename_hash = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(filename_hash.returncode, 2)

            tampered = copy.deepcopy(proof)
            tampered["root_acquire_intent"]["sha256"] = sha("0")
            tampered_path = temporary / "tampered.json"
            write_canonical(tampered_path, tampered)
            tampered_hash = release.sha256_bytes(tampered_path.read_bytes())
            command[command.index(str(proof_path))] = str(tampered_path)
            command[command.index(f"{proof_hash}  production-main-lock-proof.json")] = (
                tampered_hash
            )
            invalid = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(invalid.returncode, 2)


if __name__ == "__main__":
    unittest.main()
