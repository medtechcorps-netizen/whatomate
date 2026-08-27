from __future__ import annotations

import copy
import datetime as dt
import json
import subprocess
import sys
import unittest
from pathlib import Path
from typing import Any, Mapping


sys.path.insert(0, str(Path(__file__).resolve().parent))
import reconcile_production_orphan as reconcile
import verify_production_release as common


APP_ID = "11111111-1111-4111-8111-111111111111"
DEPLOYMENT_ID = "22222222-2222-4222-8222-222222222222"
DEFAULT_INGRESS = "https://crm.example.invalid"
APPLY_CONTROL_SHA = "a" * 40
RECONCILE_CONTROL_SHA = APPLY_CONTROL_SHA
NOW = dt.datetime(2026, 8, 27, 0, 5, 0, tzinfo=dt.timezone.utc)
REQUEST_LOG = [
    ("GET", "app"),
    ("GET", "deployment"),
    ("GET", "app"),
    ("GET", "deployment"),
]


def sha(character: str) -> str:
    return character * 64


def digest(character: str) -> str:
    return "sha256:" + character * 64


def artifact_binding(
    *,
    run_id: str,
    artifact_id: str,
    artifact_name: str,
    file_sha256: str,
    digest_character: str = "d",
) -> dict[str, Any]:
    return {
        "run_id": run_id,
        "run_attempt": 1,
        "artifact_id": artifact_id,
        "artifact_name": artifact_name,
        "artifact_digest": digest(digest_character),
        "sha256": file_sha256,
    }


def receipt_binding(binding: Mapping[str, Any]) -> dict[str, Any]:
    return {
        key: value
        for key, value in binding.items()
        if key != "artifact_name"
    }


def image_records(character: str) -> list[dict[str, str]]:
    return common.sanitized_image_records(
        {
            "web": digest(character),
            "meta-relay": digest(character),
            "gmail-relay": digest(character),
        }
    )


def before_state() -> dict[str, Any]:
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


def desired_projection() -> dict[str, Any]:
    return {
        "canonical_spec_sha256": sha("8"),
        "environment_values_sha256": sha("6"),
        "non_source_projection_sha256": sha("7"),
        "source_mode": "digest-images",
        "images": image_records("9"),
        "migration_job": "rereply-rls-migrate",
        "migration_digest": digest("9"),
    }


def desired_public_state() -> dict[str, Any]:
    before = before_state()
    desired = desired_projection()
    return {
        "app_identity_sha256": before["app_identity_sha256"],
        "default_ingress_sha256": before["default_ingress_sha256"],
        "app_updated_at_sha256": sha("a"),
        "active_deployment_identity_sha256": sha("b"),
        "canonical_spec_sha256": desired["canonical_spec_sha256"],
        "environment_values_sha256": desired["environment_values_sha256"],
        "non_source_projection_sha256": desired["non_source_projection_sha256"],
        "source_mode": desired["source_mode"],
        "images": copy.deepcopy(desired["images"]),
    }


def valid_intent() -> dict[str, Any]:
    before = before_state()
    desired = desired_projection()
    before_hash = common.sha256_value(before)
    desired_hash = common.sha256_value(desired)
    rule_id = "BPR_kwDO_rereply_01"
    intent = {
        "schema_version": 2,
        "authority": "production-mutation-intent",
        "repository": common.REPOSITORY,
        "prepared_at": "2026-08-27T00:00:00Z",
        "expires_at": "2026-08-27T00:15:00Z",
        "control": {
            "workflow_sha": APPLY_CONTROL_SHA,
            "workflow_path": ".github/workflows/apply-production-phase.yml",
            "run_id": "101",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": sha("c"),
            "change_schema_sha256": sha("d"),
            "mutation_intent_schema_sha256": sha("e"),
            "controller_sha256": sha("f"),
        },
        "operation": "activate",
        "lineage": {
            "event_sequence": 1,
            "phase_ordinal": 1,
            "operation": "activate",
            "from": "genesis",
            "to": "baseline",
            "predecessor_kind": "genesis",
            "predecessor_state_sha256": sha("0"),
            "phase": "baseline",
            "phase_source_sha": "1" * 40,
        },
        "authorities": {
            "rollout_plan_sha256": sha("1"),
            "rollout_authority": artifact_binding(
                run_id="91",
                artifact_id="191",
                artifact_name="exact-four-phase-rollout-91-1",
                file_sha256=sha("1"),
                digest_character="1",
            ),
            "production_plan": artifact_binding(
                run_id="92",
                artifact_id="192",
                artifact_name="verified-production-plan-92-1",
                file_sha256=sha("2"),
                digest_character="2",
            ),
            "recovery": artifact_binding(
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
        },
        "lock": {
            "mode": "planned",
            "strategy": "acquire",
            "branch": "main",
            "rule_id": rule_id,
            "rule_identity_sha256": common.sha256_bytes(rule_id.encode("utf-8")),
            "expected_pre_lock": {
                "lock_branch": False,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "expected_post_lock": {
                "lock_branch": True,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "root_acquire_intent": artifact_binding(
                run_id="101",
                artifact_id="194",
                artifact_name="production-main-lock-acquire-101-1",
                file_sha256=sha("4"),
                digest_character="4",
            ),
            "owner_operation": "apply",
            "owner_run_id": "101",
            "owner_run_attempt": 1,
            "owner_control_sha": APPLY_CONTROL_SHA,
            "owner_intent_sha256": None,
        },
        "before": before,
        "desired": desired,
        "mutation": {
            "http_method": "PUT",
            "endpoint_label": "app",
            "update_all_source_versions": False,
            "before_sha256": before_hash,
            "desired_sha256": desired_hash,
            "mutation_fingerprint_sha256": common.sha256_value(
                {
                    "before_sha256": before_hash,
                    "desired_sha256": desired_hash,
                    "http_method": "PUT",
                    "endpoint_label": "app",
                    "update_all_source_versions": False,
                }
            ),
        },
        "rollback": copy.deepcopy(common.ROLLBACK_FLOORS["baseline"]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": copy.deepcopy(reconcile.apply_control.ENDPOINT_LABELS),
            "route_contract_sha256": sha("5"),
        },
    }
    return common.validate_mutation_intent(intent)


def reconciliation_control() -> dict[str, Any]:
    return {
        "workflow_sha": RECONCILE_CONTROL_SHA,
        "workflow_path": reconcile.WORKFLOW_PATH,
        "run_id": "201",
        "run_attempt": 1,
        "runner_environment": "github-hosted",
        "release_policy_sha256": sha("c"),
        "change_schema_sha256": sha("d"),
        "mutation_intent_schema_sha256": sha("e"),
        "reconciliation_schema_sha256": sha("6"),
        "controller_sha256": sha("7"),
    }


def simple_reconciliation_control() -> dict[str, Any]:
    control = reconciliation_control()
    return {
        key: control[key]
        for key in ("workflow_sha", "workflow_path", "run_id", "run_attempt")
    }


def intent_binding(intent: Mapping[str, Any]) -> dict[str, Any]:
    return artifact_binding(
        run_id="101",
        artifact_id="301",
        artifact_name="production-mutation-intent-apply-101-1",
        file_sha256=common.sha256_bytes(common.canonical_file_bytes(intent)),
        digest_character="5",
    )


def lock_assertion_request(intent: Mapping[str, Any]) -> dict[str, Any]:
    exact_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
    provider_jobs = {
        ".github/workflows/apply-production-phase.yml": "Apply exact production phase",
        ".github/workflows/rollback-production-phase.yml": "Roll back exact production phase",
        ".github/workflows/rollback-production-orphan.yml": "Roll back exact production orphan",
    }
    return {
        "original_workflow_path": intent["control"]["workflow_path"],
        "original_control_sha": intent["control"]["workflow_sha"],
        "original_run_id": intent["control"]["run_id"],
        "original_run_attempt": intent["control"]["run_attempt"],
        "rule_id": intent["lock"]["rule_id"],
        "current_main_sha": RECONCILE_CONTROL_SHA,
        "typed_confirmation": (
            f"RECONCILE LOCKED PRODUCTION {intent['control']['run_id']} {exact_hash}"
        ),
        "original_provider_job": {
            "job_id": "901",
            "job_name": provider_jobs[intent["control"]["workflow_path"]],
            "status": "completed",
            "conclusion": "skipped",
            "started_at": common.format_timestamp(NOW),
            "completed_at": common.format_timestamp(NOW),
            "steps": [],
        },
    }


def valid_lock_assertion(
    intent: Mapping[str, Any], *, provider_job: Mapping[str, Any] | None = None
) -> dict[str, Any]:
    exact_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
    request = lock_assertion_request(intent)
    if provider_job is not None:
        request["original_provider_job"] = copy.deepcopy(provider_job)
    return reconcile.build_lock_assertion(
        request=request,
        intent=intent,
        intent_sha256=exact_hash,
        control=simple_reconciliation_control(),
        now=NOW,
    )


def lock_assertion_binding(assertion: Mapping[str, Any]) -> dict[str, Any]:
    return artifact_binding(
        run_id="201",
        artifact_id="302",
        artifact_name="production-main-lock-assertion-201-1",
        file_sha256=common.sha256_bytes(common.canonical_file_bytes(assertion)),
        digest_character="6",
    )


def observed(
    public: Mapping[str, Any],
    *,
    transitions: list[dict[str, Any]] | None = None,
    migration_succeeded: bool,
    app_spec_sha256: str | None = None,
) -> dict[str, Any]:
    return {
        "public": copy.deepcopy(public),
        "app_spec_sha256": app_spec_sha256 or public["canonical_spec_sha256"],
        "transitions": copy.deepcopy(transitions or []),
        "migration_succeeded": migration_succeeded,
    }


def original_apply_receipt(
    intent: Mapping[str, Any], after: Mapping[str, Any]
) -> tuple[dict[str, Any], dict[str, Any]]:
    exact_intent_binding = intent_binding(intent)
    receipt = {
        "schema_version": 1,
        "authority": "production-phase-apply-receipt",
        "repository": common.REPOSITORY,
        "completed_at": "2026-08-27T00:03:00Z",
        "control": {
            "workflow_sha": APPLY_CONTROL_SHA,
            "workflow_path": ".github/workflows/apply-production-phase.yml",
            "run_id": "301",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": sha("c"),
            "change_schema_sha256": sha("d"),
            "controller_sha256": sha("f"),
        },
        "lineage": copy.deepcopy(intent["lineage"]),
        "authorities": {
            "rollout_plan_sha256": intent["authorities"]["rollout_plan_sha256"],
            "production_plan": receipt_binding(intent["authorities"]["production_plan"]),
            "recovery": receipt_binding(intent["authorities"]["recovery"]),
            "mutation_intent": exact_intent_binding,
            "main_lock_proof": artifact_binding(
                run_id="301",
                artifact_id="398",
                artifact_name="production-main-lock-proof-apply-301-1",
                file_sha256=sha("e"),
                digest_character="e",
            ),
        },
        "provider_transition": {
            "http_methods_used": ["GET", "PUT"],
            "http_request_count": 11,
            "mutation_request_count": 1,
            "endpoint_labels": ["app", "deployment"],
            "mutation_fingerprint_sha256": intent["mutation"][
                "mutation_fingerprint_sha256"
            ],
            "ambiguous_reconciled": True,
        },
        "before": copy.deepcopy(intent["before"]),
        "after": copy.deepcopy(after),
        "gates": {"deployment_succeeded": True, "migration_succeeded": True},
        "rollback": copy.deepcopy(intent["rollback"]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": copy.deepcopy(intent["canary"]["endpoint_labels"]),
            "route_contract_sha256": intent["canary"]["route_contract_sha256"],
        },
    }
    common.validate_apply_receipt(receipt)
    binding = artifact_binding(
        run_id="301",
        artifact_id="303",
        artifact_name="production-phase-apply-301-1",
        file_sha256=common.sha256_bytes(common.canonical_file_bytes(receipt)),
        digest_character="7",
    )
    return receipt, binding


def build_receipt(
    *,
    public: Mapping[str, Any],
    transitions: list[dict[str, Any]] | None = None,
    migration_succeeded: bool,
    with_original_receipt: bool = False,
    provider_job: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    intent = valid_intent()
    assertion = valid_lock_assertion(intent, provider_job=provider_job)
    original: dict[str, Any] | None = None
    original_binding: dict[str, Any] | None = None
    if with_original_receipt:
        original, original_binding = original_apply_receipt(intent, public)
    return reconcile.build_reconciliation_receipt(
        control=reconciliation_control(),
        intent=intent,
        intent_binding=intent_binding(intent),
        lock_assertion=assertion,
        lock_assertion_binding=lock_assertion_binding(assertion),
        observed=observed(
            public,
            transitions=transitions,
            migration_succeeded=migration_succeeded,
        ),
        request_log=REQUEST_LOG,
        original_receipt=original,
        original_receipt_binding=original_binding,
        completed_at=NOW,
    )


def digest_spec(character: str = "9") -> dict[str, Any]:
    def image(repository: str) -> dict[str, str]:
        return {
            "registry_type": "GHCR",
            "registry": "ghcr.io",
            "repository": repository,
            "digest": digest(character),
        }

    return {
        "name": "rereply",
        "region": "sgp",
        "vpc": {"id": "private-vpc"},
        "envs": [
            {
                "key": "SAFE",
                "value": "EV[private]",
                "type": "SECRET",
                "scope": "RUN_TIME",
            }
        ],
        "services": [
            {
                "name": "omnitech-web",
                "image": image("medtechcorps-netizen/rereply-release-web"),
                "envs": [],
            },
            {
                "name": "meta-relay",
                "image": image("medtechcorps-netizen/rereply-release-meta-relay"),
                "envs": [],
            },
            {
                "name": "gmail-relay",
                "image": image("medtechcorps-netizen/rereply-release-gmail-relay"),
                "envs": [],
            },
        ],
        "jobs": [
            {
                "name": "rereply-rls-migrate",
                "image": image("medtechcorps-netizen/rereply-release-web"),
                "envs": [],
            }
        ],
        "ingress": {
            "rules": [
                {
                    "match": {"path": {"prefix": "/"}},
                    "component": {"name": "omnitech-web"},
                }
            ]
        },
        "domains": [{"domain": "crm.example.invalid", "type": "PRIMARY"}],
        "databases": [],
    }


def app_response(spec: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "app": {
            "id": APP_ID,
            "updated_at": "2026-08-27T00:04:00Z",
            "default_ingress": DEFAULT_INGRESS,
            "spec": copy.deepcopy(spec),
            "active_deployment": {"id": DEPLOYMENT_ID, "phase": "ACTIVE"},
            "in_progress_deployment": None,
            "pending_deployment": None,
            "pinned_deployment": None,
        }
    }


def deployment_response(spec: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "deployment": {
            "id": DEPLOYMENT_ID,
            "phase": "ACTIVE",
            "spec": copy.deepcopy(spec),
            "jobs": [{"name": "rereply-rls-migrate", "phase": "SUCCEEDED"}],
        }
    }


class FakeResponse:
    def __init__(self, value: Any, url: str) -> None:
        self.raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.url = url
        self.status = 200
        self.headers = {"Content-Type": "application/json"}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def getcode(self) -> int:
        return self.status

    def read(self, amount: int) -> bytes:
        return self.raw[:amount]


class QueueOpener:
    def __init__(self, values: list[Any]) -> None:
        self.values = list(values)
        self.requests: list[Any] = []

    def open(self, request: Any, timeout: int) -> FakeResponse:
        self.requests.append(request)
        return FakeResponse(self.values.pop(0), request.full_url)


class ReconcileProductionOrphanTests(unittest.TestCase):
    def test_isolated_direct_entrypoint_resolves_sibling_controls(self) -> None:
        result = subprocess.run(
            [
                sys.executable,
                "-I",
                "-S",
                "-B",
                str(Path(reconcile.__file__).resolve()),
                "--help",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("GET-only production orphan reconciliation", result.stdout)

    def test_mutation_intent_expiry_is_exclusive_at_boundary_and_fraction(self) -> None:
        intent = valid_intent()
        expires = common.require_timestamp(intent["expires_at"], "test intent expiry")
        for checked in (expires, expires + dt.timedelta(microseconds=1)):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    common.validate_mutation_intent(intent, now=checked)

    def test_build_and_validate_v2_lock_assertion_and_wrong_phrase(self) -> None:
        intent = valid_intent()
        intent_hash = common.sha256_bytes(common.canonical_file_bytes(intent))
        assertion = valid_lock_assertion(intent)
        self.assertEqual(assertion["mutation_intent_sha256"], intent_hash)
        self.assertEqual(assertion["current_main_sha"], RECONCILE_CONTROL_SHA)
        reconcile.validate_lock_assertion(assertion, intent=intent)

        wrong = lock_assertion_request(intent)
        wrong["typed_confirmation"] += " WRONG"
        with self.assertRaises(common.ReleaseError):
            reconcile.build_lock_assertion(
                request=wrong,
                intent=intent,
                intent_sha256=intent_hash,
                control=simple_reconciliation_control(),
                now=NOW,
            )

        stale = copy.deepcopy(assertion)
        stale["created_at"] = common.format_timestamp(
            NOW - dt.timedelta(seconds=reconcile.MAX_ASSERTION_AGE_SECONDS + 1)
        )
        with self.assertRaises(common.ReleaseError):
            reconcile.validate_lock_assertion(stale, intent=intent, now=NOW)

    def test_no_mutation_requires_hash_bound_provider_job_never_started_proof(self) -> None:
        intent = valid_intent()
        request = lock_assertion_request(intent)["original_provider_job"]
        self.assertTrue(valid_lock_assertion(intent)["original_provider_job"]["never_started"])

        skipped_steps = copy.deepcopy(request)
        skipped_steps["steps"] = [
            {
                "number": 1,
                "name": "Apply with one isolated app-update capability",
                "status": "completed",
                "conclusion": "skipped",
            }
        ]
        self.assertTrue(
            valid_lock_assertion(
                intent, provider_job=skipped_steps
            )["original_provider_job"]["never_started"]
        )

        started = copy.deepcopy(request)
        started["conclusion"] = "failure"
        started["steps"] = [
            {
                "number": 1,
                "name": "Apply with one isolated app-update capability",
                "status": "completed",
                "conclusion": "failure",
            }
        ]
        assertion = valid_lock_assertion(intent, provider_job=started)
        self.assertFalse(assertion["original_provider_job"]["never_started"])
        receipt = build_receipt(
            public=before_state(), migration_succeeded=False, provider_job=started
        )
        self.assertEqual(receipt["classification"]["outcome"], "indeterminate")

        duplicate = copy.deepcopy(skipped_steps)
        duplicate["steps"].append(copy.deepcopy(duplicate["steps"][0]))
        with self.assertRaises(common.ReleaseError):
            valid_lock_assertion(intent, provider_job=duplicate)

        forged = copy.deepcopy(assertion)
        forged["original_provider_job"]["never_started"] = True
        with self.assertRaises(common.ReleaseError):
            reconcile.validate_lock_assertion(forged, intent=intent)

    def test_no_mutation_allows_only_pre_attested_timestamp_metadata_drift(self) -> None:
        timestamp_only = before_state()
        timestamp_only["app_updated_at_sha256"] = sha("a")
        receipt = build_receipt(
            public=timestamp_only, migration_succeeded=False
        )
        self.assertEqual(receipt["classification"]["outcome"], "no-mutation")
        self.assertNotEqual(receipt["before"], receipt["after"])
        self.assertEqual(
            {
                key
                for key in receipt["before"]
                if receipt["before"][key] != receipt["after"][key]
            },
            {"app_updated_at_sha256"},
        )
        common.validate_reconciliation_receipt(receipt)

        started = lock_assertion_request(valid_intent())["original_provider_job"]
        started["conclusion"] = "failure"
        started["steps"] = [
            {
                "number": 1,
                "name": "Apply with one isolated app-update capability",
                "status": "completed",
                "conclusion": "failure",
            }
        ]
        started_receipt = build_receipt(
            public=timestamp_only,
            migration_succeeded=False,
            provider_job=started,
        )
        self.assertEqual(
            started_receipt["classification"]["outcome"], "indeterminate"
        )

        mutations = {
            "app identity": lambda state: state.__setitem__(
                "app_identity_sha256", sha("a")
            ),
            "default ingress": lambda state: state.__setitem__(
                "default_ingress_sha256", sha("a")
            ),
            "active deployment": lambda state: state.__setitem__(
                "active_deployment_identity_sha256", sha("a")
            ),
            "canonical spec": lambda state: state.__setitem__(
                "canonical_spec_sha256", sha("a")
            ),
            "environment": lambda state: state.__setitem__(
                "environment_values_sha256", sha("a")
            ),
            "non-source": lambda state: state.__setitem__(
                "non_source_projection_sha256", sha("a")
            ),
            "source mode and images": lambda state: state.update(
                {"source_mode": "digest-images", "images": image_records("a")}
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                drifted = copy.deepcopy(timestamp_only)
                mutate(drifted)
                drifted_receipt = build_receipt(
                    public=drifted, migration_succeeded=False
                )
                self.assertEqual(
                    drifted_receipt["classification"]["outcome"],
                    "indeterminate",
                )

    def test_all_five_outcomes_are_classified_exactly(self) -> None:
        committed = build_receipt(
            public=desired_public_state(), migration_succeeded=True
        )
        already = build_receipt(
            public=desired_public_state(),
            migration_succeeded=True,
            with_original_receipt=True,
        )
        no_mutation = build_receipt(
            public=before_state(), migration_succeeded=False
        )
        pending = build_receipt(
            public=before_state(),
            transitions=[
                {"canonical_spec_sha256": desired_projection()["canonical_spec_sha256"]}
            ],
            migration_succeeded=False,
        )
        drift = desired_public_state()
        drift["canonical_spec_sha256"] = sha("f")
        indeterminate = build_receipt(public=drift, migration_succeeded=False)
        self.assertEqual(
            [
                receipt["classification"]["outcome"]
                for receipt in (
                    committed,
                    already,
                    no_mutation,
                    pending,
                    indeterminate,
                )
            ],
            [
                "committed",
                "already-receipted",
                "no-mutation",
                "pending",
                "indeterminate",
            ],
        )
        self.assertEqual(
            [receipt["classification"]["canary_eligible"] for receipt in (
                committed, already, no_mutation, pending, indeterminate
            )],
            [True, True, False, False, False],
        )

    def test_v1_without_an_exact_signed_receipt_is_rejected(self) -> None:
        receipt = build_receipt(
            public=desired_public_state(), migration_succeeded=True
        )
        receipt["intent"]["schema_version"] = 1
        self.assertIsNone(receipt["authorities"]["original_receipt"])
        with self.assertRaises(common.ReleaseError):
            common.validate_reconciliation_receipt(receipt)

    def test_committed_requires_exact_desired_stability_and_migration(self) -> None:
        committed = build_receipt(
            public=desired_public_state(), migration_succeeded=True
        )
        self.assertEqual(committed["classification"]["outcome"], "committed")

        desired_drift = copy.deepcopy(committed)
        desired_drift["after"]["canonical_spec_sha256"] = sha("f")
        with self.assertRaises(common.ReleaseError):
            common.validate_reconciliation_receipt(desired_drift)

        unstable = copy.deepcopy(committed)
        unstable["provider_observation"]["double_read_equal"] = False
        with self.assertRaises(common.ReleaseError):
            common.validate_reconciliation_receipt(unstable)

        without_migration = build_receipt(
            public=desired_public_state(), migration_succeeded=False
        )
        self.assertEqual(
            without_migration["classification"]["outcome"], "indeterminate"
        )

        transitioning = build_receipt(
            public=desired_public_state(),
            transitions=[
                {"canonical_spec_sha256": desired_projection()["canonical_spec_sha256"]}
            ],
            migration_succeeded=True,
        )
        self.assertEqual(transitioning["classification"]["outcome"], "pending")

    def test_app_spec_and_active_deployment_spec_must_match(self) -> None:
        intent = valid_intent()
        assertion = valid_lock_assertion(intent)
        mismatched = observed(
            desired_public_state(),
            migration_succeeded=True,
            app_spec_sha256=sha("f"),
        )
        with self.assertRaisesRegex(
            common.ReleaseError, "app spec differs from the active deployment spec"
        ):
            reconcile.build_reconciliation_receipt(
                control=reconciliation_control(),
                intent=intent,
                intent_binding=intent_binding(intent),
                lock_assertion=assertion,
                lock_assertion_binding=lock_assertion_binding(assertion),
                observed=mismatched,
                request_log=REQUEST_LOG,
                original_receipt=None,
                original_receipt_binding=None,
                completed_at=NOW,
            )

        app_spec = digest_spec("8")
        active_spec = digest_spec("9")
        opener = QueueOpener(
            [app_response(app_spec), deployment_response(active_spec)]
        )
        client = reconcile.ReadOnlyProductionClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaisesRegex(
            common.ReleaseError, "app spec differs from the active deployment spec"
        ):
            reconcile.observe_twice(client)

    def test_noncommitted_outcomes_cannot_create_a_phase_state(self) -> None:
        drift = desired_public_state()
        drift["canonical_spec_sha256"] = sha("f")
        cases = (
            build_receipt(public=before_state(), migration_succeeded=False),
            build_receipt(
                public=before_state(),
                transitions=[
                    {
                        "canonical_spec_sha256": desired_projection()[
                            "canonical_spec_sha256"
                        ]
                    }
                ],
                migration_succeeded=False,
            ),
            build_receipt(public=drift, migration_succeeded=False),
        )
        for receipt in cases:
            with self.subTest(outcome=receipt["classification"]["outcome"]):
                self.assertFalse(receipt["canary"]["required"])
                self.assertFalse(receipt["canary"]["eligible"])
                receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
                with self.assertRaises(common.ReleaseError):
                    common.build_phase_state(
                        receipt,
                        change_receipt_sha256=receipt_hash,
                        canary_sha256=sha("8"),
                        control={
                            "workflow_sha": RECONCILE_CONTROL_SHA,
                            "workflow_path": (
                                ".github/workflows/verify-production-crm-canary.yml"
                            ),
                            "run_id": "401",
                            "run_attempt": 1,
                            "runner_environment": "github-hosted",
                            "release_policy_sha256": sha("c"),
                            "change_schema_sha256": sha("d"),
                        },
                        completed_at="2026-08-27T00:06:00Z",
                    )

    def test_read_only_client_has_no_put_and_exact_four_get_ledger(self) -> None:
        spec = digest_spec()
        opener = QueueOpener(
            [
                app_response(spec),
                deployment_response(spec),
                app_response(spec),
                deployment_response(spec),
            ]
        )
        client = reconcile.ReadOnlyProductionClient(
            APP_ID, "t" * 24, opener=opener
        )
        self.assertFalse(hasattr(client, "put"))
        self.assertFalse(hasattr(client, "put_app_once"))
        observation = reconcile.observe_twice(client)
        self.assertEqual(client.request_log, REQUEST_LOG)
        self.assertEqual([request.method for request in opener.requests], ["GET"] * 4)
        self.assertTrue(observation["migration_succeeded"])
        client.scrub()
        self.assertEqual(client._token, "")

        receipt = build_receipt(
            public=desired_public_state(), transitions=[], migration_succeeded=True
        )
        self.assertEqual(receipt["provider_observation"]["http_request_count"], 4)
        receipt["provider_observation"]["http_request_count"] = 5
        with self.assertRaises(common.ReleaseError):
            common.validate_reconciliation_receipt(receipt)

        first_app = app_response(spec)
        second_app = app_response(spec)
        second_app["app"]["updated_at"] = "2026-08-27T00:04:01Z"
        opener = QueueOpener(
            [
                first_app,
                deployment_response(spec),
                second_app,
                deployment_response(spec),
            ]
        )
        client = reconcile.ReadOnlyProductionClient(
            APP_ID, "t" * 24, opener=opener
        )
        with self.assertRaisesRegex(
            common.ReleaseError,
            "production changed between reconciliation observations",
        ):
            reconcile.observe_twice(client)
        self.assertEqual([request.method for request in opener.requests], ["GET"] * 4)

    def test_artifact_name_and_hash_cross_splices_are_rejected(self) -> None:
        intent = valid_intent()
        assertion = valid_lock_assertion(intent)
        base = {
            "control": reconciliation_control(),
            "intent": intent,
            "intent_binding": intent_binding(intent),
            "lock_assertion": assertion,
            "lock_assertion_binding": lock_assertion_binding(assertion),
            "observed": observed(
                desired_public_state(), migration_succeeded=True
            ),
            "request_log": REQUEST_LOG,
            "original_receipt": None,
            "original_receipt_binding": None,
            "completed_at": NOW,
        }
        mutations = (
            lambda value: value["intent_binding"].__setitem__(
                "artifact_name", "production-mutation-intent-rollback-101-1"
            ),
            lambda value: value["intent_binding"].__setitem__("sha256", sha("f")),
            lambda value: value["lock_assertion_binding"].__setitem__(
                "artifact_name", "production-main-lock-assertion-999-1"
            ),
            lambda value: value["lock_assertion_binding"].__setitem__(
                "sha256", sha("f")
            ),
        )
        for mutate in mutations:
            arguments = copy.deepcopy(base)
            mutate(arguments)
            with self.assertRaises(common.ReleaseError):
                reconcile.build_reconciliation_receipt(**arguments)

    def test_secret_sanitizer_override_is_narrow(self) -> None:
        common.sanitize_public(
            {"created_at": "2026-08-27T00:05:00Z"},
            allowed_keys=("created_at",),
        )
        for value in (
            {"token": "not-public"},
            {"safe": "dop_v1_not-public"},
            {"created_at": "2026-08-27T00:05:00Z", "password": "not-public"},
        ):
            with self.assertRaises(common.ReleaseError):
                common.sanitize_public(value, allowed_keys=("created_at",))


if __name__ == "__main__":
    unittest.main()
