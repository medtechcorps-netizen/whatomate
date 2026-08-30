from __future__ import annotations

import copy
import datetime as dt
import io
import json
import os
import re
import sys
import tempfile
import unittest
from collections import Counter
from pathlib import Path
from contextlib import redirect_stderr, redirect_stdout
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_plan as verifier


ROOT = Path(__file__).resolve().parents[2]
CONTRACT_PATH = ROOT / "release" / "deployment" / "production-app-contract.json"
POLICY_PATH = ROOT / "release" / "deployment" / "production-release-policy.json"
SCHEMA_PATH = ROOT / "release" / "deployment" / "production-change.schema.json"
VERIFIER_PATH = ROOT / "release" / "deployment" / "verify_production_plan.py"
WORKFLOW_PATH = ROOT / ".github" / "workflows" / "plan-production-rollout.yml"
IMAGE_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "build-attest-exact-release-images.yml"
APPLY_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "apply-production-phase.yml"
ROLLBACK_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "rollback-production-phase.yml"
CANARY_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "verify-production-crm-canary.yml"
TEST_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "test.yml"
CONTROL_SHA = "f" * 40
NOW = dt.datetime(2026, 8, 26, 12, 0, 0, tzinfo=dt.timezone.utc)
TEST_TARGET = {
    "app_id": "11111111-1111-4111-8111-111111111111",
    "default_ingress": "https://private-target.invalid",
}
ACTIVE_DEPLOYMENT_ID = "22222222-2222-4222-8222-222222222222"
APP_UPDATED_AT = "2026-08-26T11:11:11Z"


def digest(label: str) -> str:
    return "sha256:" + verifier.sha256_bytes(label.encode("utf-8"))


def fake_spec() -> dict[str, object]:
    repository = "https://github.com/medtechcorps-netizen/whatomate.git"
    return {
        "name": verifier.PRODUCTION_APP_NAME,
        "region": "sgp",
        "vpc": {"id": "test-vpc-binding"},
        "envs": [
            {
                "key": "APP_PUBLIC_MODE",
                "scope": "RUN_TIME",
                "type": "GENERAL",
                "value": "enabled",
            }
        ],
        "services": [
            {
                "name": "omnitech-web",
                "git": {"repo_clone_url": repository, "branch": "main"},
                "dockerfile_path": "docker/Dockerfile",
                "http_port": 8080,
                "health_check": {"http_path": "/ready"},
                "instance_count": 1,
                "instance_size_slug": "professional-xs",
                "envs": [
                    {
                        "key": "WHATOMATE_APP__ENCRYPTION_KEY",
                        "scope": "RUN_TIME",
                        "type": "SECRET",
                        "value": "EV[test-ciphertext-web]",
                    }
                ],
            },
            {
                "name": "meta-relay",
                "git": {"repo_clone_url": repository, "branch": "main"},
                "dockerfile_path": "docker/meta-relay.Dockerfile",
                "http_port": 8081,
                "health_check": {"http_path": "/readyz"},
                "instance_count": 1,
                "instance_size_slug": "professional-xs",
                "envs": [],
            },
            {
                "name": "gmail-relay",
                "git": {"repo_clone_url": repository, "branch": "main"},
                "dockerfile_path": "docker/gmail-relay.Dockerfile",
                "http_port": 8082,
                "health_check": {"http_path": "/readyz"},
                "instance_count": 1,
                "instance_size_slug": "professional-xs",
                "envs": [],
            },
        ],
        "jobs": [
            {
                "name": "rereply-rls-migrate",
                "git": {"repo_clone_url": repository, "branch": "main"},
                "dockerfile_path": "docker/Dockerfile",
                "kind": "PRE_DEPLOY",
                "run_command": "./rereply rls-migrate -config config.toml",
                "envs": [],
            }
        ],
        "ingress": {
            "rules": [
                {
                    "match": {"path": {"prefix": "/gmail-relay"}},
                    "component": {"name": "gmail-relay"},
                },
                {
                    "match": {"path": {"prefix": "/meta-relay"}},
                    "component": {"name": "meta-relay"},
                },
                {
                    "match": {"path": {"prefix": "/"}},
                    "component": {"name": "omnitech-web"},
                },
                {
                    "match": {
                        "path": {"prefix": "/"},
                        "authority": {"exact": "rereply.app"},
                    },
                    "redirect": {
                        "authority": "app.rereply.app",
                        "scheme": "https",
                        "redirect_code": 308,
                    },
                },
            ]
        },
        "domains": [
            {"domain": "rereply.app", "type": "ALIAS"},
            {"domain": "app.rereply.app", "type": "PRIMARY"},
        ],
        "databases": [
            {
                "name": "test-postgres-binding",
                "cluster_name": "test-postgres-cluster",
                "engine": "PG",
                "version": "17",
                "production": True,
            },
            {
                "name": "test-valkey-binding",
                "cluster_name": "test-valkey-cluster",
                "engine": "VALKEY",
                "version": "8",
                "production": True,
            },
        ],
    }


def database_inventory(spec: dict[str, object]) -> set[tuple[object, ...]]:
    return {
        (
            item["engine"],
            item["version"],
            item["production"],
            verifier.sha256_bytes(item["name"].encode("utf-8")),
            verifier.sha256_bytes(item["cluster_name"].encode("utf-8")),
        )
        for item in spec["databases"]
    }


def rollout_plan() -> dict[str, object]:
    phases = []
    for index, phase in enumerate(verifier.PHASES):
        source_sha = verifier.BOOTSTRAP_SOURCE_SHA if phase == "baseline" else (
            f"{index + 1:x}" * 40
        )
        images = []
        for component in ("web", "meta-relay", "gmail-relay"):
            repository = f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
            label = component if phase == "baseline" else f"{phase}-{component}"
            images.append(
                {
                    "component": component,
                    "image": repository,
                    "digest": digest(label),
                    "tag_is_authority": False,
                }
            )
        phases.append(
            {
                "phase": phase,
                "source": {"commit": source_sha},
                "images": images,
                "migration": {"digest": images[0]["digest"]},
                "rollback": copy.deepcopy(verifier.ROLLBACK_FLOORS[phase]),
            }
        )
    return {
        "schema_version": 1,
        "authority": "digest-only",
        "repository": "medtechcorps-netizen/whatomate",
        "control": {
            "workflow_sha": CONTROL_SHA,
            "run_id": "101",
            "run_attempt": 1,
        },
        "activation_order": verifier.PHASES,
        "phases": phases,
    }


def phase_images(
    rollout: dict[str, object], phase: str, contract: dict[str, object]
) -> list[dict[str, str]]:
    selected = next(item for item in rollout["phases"] if item["phase"] == phase)
    observed: dict[str, dict[str, str]] = {}
    for image in selected["images"]:
        repository = image["image"].removeprefix("ghcr.io/")
        observed[image["component"]] = {
            "repository": repository,
            "digest": image["digest"],
            "subject": f"{image['image']}@{image['digest']}",
        }
    return verifier.target_image_records(contract, observed)


class FakeResponse:
    def __init__(self, value: object, url: str, *, status: int = 200) -> None:
        self.raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.url = url
        self.status = status
        self.headers = {"Content-Type": "application/json; charset=utf-8"}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def read(self, amount: int) -> bytes:
        return self.raw[:amount]


class FakeOpener:
    def __init__(self, values: list[object], urls: list[str]) -> None:
        self.responses = [
            FakeResponse(value, url) for value, url in zip(values, urls, strict=True)
        ]
        self.requests = []

    def open(self, request: object, timeout: int) -> FakeResponse:
        if timeout != 20:
            raise AssertionError("unexpected timeout")
        if request.method != "GET" or request.data is not None:
            raise AssertionError("provider request is not GET-only")
        self.requests.append(request)
        if not self.responses:
            raise AssertionError("unexpected provider request")
        return self.responses.pop(0)


class ProductionPlanTests(unittest.TestCase):
    def setUp(self) -> None:
        self.spec = fake_spec()
        self.target = dict(TEST_TARGET)
        self.contract = json.loads(CONTRACT_PATH.read_text(encoding="utf-8"))
        self.policy = verifier.validate_release_policy(
            json.loads(POLICY_PATH.read_text(encoding="utf-8"))
        )
        self.schema = verifier.validate_change_schema(
            json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
        )
        self.policy_hash = verifier.sha256_bytes(POLICY_PATH.read_bytes())
        self.schema_hash = verifier.sha256_bytes(SCHEMA_PATH.read_bytes())
        target_hashes = {
            key: verifier.sha256_bytes(value.encode("utf-8"))
            for key, value in self.target.items()
        }
        self.contract["provider"]["app_id_sha256"] = target_hashes["app_id"]
        self.contract["provider"]["default_ingress_sha256"] = target_hashes[
            "default_ingress"
        ]
        active_deployment_hash = verifier.sha256_bytes(
            ACTIVE_DEPLOYMENT_ID.encode("utf-8")
        )
        self.contract["bootstrap_state"]["active_deployment_id_sha256"] = (
            active_deployment_hash
        )
        vpc_hash = verifier.sha256_bytes(self.spec["vpc"]["id"].encode("utf-8"))
        db_inventory = database_inventory(self.spec)
        self.contract["expected_topology"]["vpc_id_sha256"] = vpc_hash
        db_by_engine = {item[0]: item for item in db_inventory}
        for item in self.contract["expected_topology"]["databases"]:
            observed = db_by_engine[item["engine"]]
            item["version"] = observed[1]
            item["production"] = observed[2]
            item["name_sha256"] = observed[3]
            item["cluster_sha256"] = observed[4]

        canonical_hash = verifier.sha256_value(self.spec)
        environment_hash = verifier.environment_value_fingerprint(self.spec)
        non_source_hash = verifier.non_source_fingerprint(self.spec, self.contract)
        self.contract["bootstrap_state"]["canonical_spec_sha256"] = canonical_hash
        self.contract["bootstrap_state"]["environment_values_sha256"] = environment_hash
        self.contract["bootstrap_state"]["non_source_projection_sha256"] = non_source_hash
        self.contract["bootstrap_state"]["genesis_state_sha256"] = (
            verifier.genesis_state_sha256(self.contract)
        )

        patches = [
            mock.patch.object(verifier, "PRODUCTION_VPC_ID_SHA256", vpc_hash),
            mock.patch.object(verifier, "PRODUCTION_DATABASE_INVENTORY", db_inventory),
            mock.patch.object(
                verifier, "BOOTSTRAP_CANONICAL_SPEC_SHA256", canonical_hash
            ),
            mock.patch.object(
                verifier, "BOOTSTRAP_ENVIRONMENT_SHA256", environment_hash
            ),
            mock.patch.object(
                verifier, "PRODUCTION_APP_ID_SHA256", target_hashes["app_id"]
            ),
            mock.patch.object(
                verifier,
                "PRODUCTION_DEFAULT_INGRESS_SHA256",
                target_hashes["default_ingress"],
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_DEPLOYMENT_ID_SHA256",
                active_deployment_hash,
            ),
            mock.patch.object(
                verifier, "BOOTSTRAP_NON_SOURCE_SHA256", non_source_hash
            ),
        ]
        for patcher in patches:
            patcher.start()
            self.addCleanup(patcher.stop)
        self.contract = verifier.validate_contract(
            self.contract, self.policy, self.schema
        )

        self.rollout = rollout_plan()
        self.rollout_hash = verifier.sha256_bytes(
            verifier.canonical_file_bytes(self.rollout)
        )
        self.normalized = verifier.normalize_input(
            json.dumps(
                {
                    "control_sha": CONTROL_SHA,
                    "rollout_run_id": "101",
                    "rollout_run_attempt": 1,
                    "capsule_artifact_id": "202",
                    "capsule_artifact_digest": digest("capsule"),
                    "rollout_plan_sha256": self.rollout_hash,
                    "predecessor": None,
                },
                separators=(",", ":"),
            ),
            CONTROL_SHA,
        )

    def responses(self) -> tuple[dict[str, object], dict[str, object]]:
        app = {
            "app": {
                "id": self.target["app_id"],
                "updated_at": APP_UPDATED_AT,
                "default_ingress": self.target["default_ingress"],
                "spec": copy.deepcopy(self.spec),
                "active_deployment": {
                    "id": ACTIVE_DEPLOYMENT_ID,
                    "phase": "ACTIVE",
                },
            }
        }
        deployment = {
            "deployment": {
                "id": ACTIVE_DEPLOYMENT_ID,
                "phase": "ACTIVE",
                "spec": copy.deepcopy(self.spec),
                "services": [
                    {
                        "name": item["name"],
                        "source_commit_hash": verifier.BOOTSTRAP_SOURCE_SHA,
                    }
                    for item in self.spec["services"]
                ],
                "jobs": [
                    {
                        "name": item["name"],
                        "source_commit_hash": verifier.BOOTSTRAP_SOURCE_SHA,
                    }
                    for item in self.spec["jobs"]
                ],
            }
        }
        return app, deployment

    def build(self, *, second_app: dict[str, object] | None = None) -> dict[str, object]:
        app, deployment = self.responses()
        second = copy.deepcopy(second_app if second_app is not None else app)
        app_path, deployment_path = verifier.provider_paths(
            self.contract, self.target, ACTIVE_DEPLOYMENT_ID
        )
        return verifier.build_plan(
            contract=self.contract,
            contract_sha256="a" * 64,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            verifier_sha256="b" * 64,
            normalized_input=self.normalized,
            target_descriptor=self.target,
            rollout_plan=self.rollout,
            predecessor_state=None,
            first_app_response=app,
            first_deployment_response=deployment,
            second_app_response=second,
            second_deployment_response=copy.deepcopy(deployment),
            workflow_run_id="303",
            workflow_run_attempt=1,
            request_log=[
                ("GET", app_path),
                ("GET", deployment_path),
                ("GET", app_path),
                ("GET", deployment_path),
            ],
            now=NOW,
        )

    def validate(self, plan: dict[str, object], *, now: dt.datetime = NOW) -> None:
        verifier.validate_plan(
            plan,
            self.contract,
            "a" * 64,
            self.policy,
            self.policy_hash,
            self.schema_hash,
            "b" * 64,
            self.rollout,
            self.rollout_hash,
            None,
            now=now,
        )

    def phase_state(
        self,
        phase: str,
        *,
        event_sequence: int | None = None,
        operation: str = "activate",
        source_phase: str | None = None,
        predecessor_kind: str | None = None,
    ) -> dict[str, object]:
        ordinal = verifier.PHASES.index(phase) + 1
        if event_sequence is None:
            event_sequence = ordinal
        if source_phase is None:
            source_phase = (
                "genesis" if phase == "baseline" else verifier.PHASES[ordinal - 2]
            )
        if predecessor_kind is None:
            predecessor_kind = (
                "apply-receipt" if operation == "activate" else "rollback-receipt"
            )
        selected = self.rollout["phases"][ordinal - 1]
        receipt_sha256 = verifier.sha256_bytes(
            f"{operation}:{source_phase}:{phase}:{event_sequence}".encode("utf-8")
        )
        return {
            "schema_version": 1,
            "authority": "production-phase-state",
            "repository": self.contract["repository"],
            "completed_at": "2026-08-26T11:55:00Z",
            "control": {
                "workflow_sha": CONTROL_SHA,
                "workflow_path": self.policy["phase_state"]["workflow_path"],
                "run_id": "401",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": self.policy_hash,
                "change_schema_sha256": self.schema_hash,
            },
            "lineage": {
                "event_sequence": event_sequence,
                "phase_ordinal": ordinal,
                "operation": operation,
                "from": source_phase,
                "to": phase,
                "predecessor_kind": predecessor_kind,
                "predecessor_state_sha256": receipt_sha256,
                "phase": phase,
                "phase_source_sha": selected["source"]["commit"],
            },
            "provider_state": {
                "app_identity_sha256": self.contract["provider"]["app_id_sha256"],
                "default_ingress_sha256": self.contract["provider"][
                    "default_ingress_sha256"
                ],
                "app_updated_at_sha256": verifier.sha256_bytes(
                    f"updated:{phase}:{event_sequence}".encode("utf-8")
                ),
                "active_deployment_identity_sha256": verifier.sha256_bytes(
                    f"deployment:{phase}:{event_sequence}".encode("utf-8")
                ),
                "canonical_spec_sha256": verifier.sha256_bytes(
                    f"spec:{phase}:{event_sequence}".encode("utf-8")
                ),
                "environment_values_sha256": self.contract["bootstrap_state"][
                    "environment_values_sha256"
                ],
                "non_source_projection_sha256": self.contract["bootstrap_state"][
                    "non_source_projection_sha256"
                ],
                "source_mode": "digest-images",
                "images": phase_images(self.rollout, phase, self.contract),
            },
            "evidence": {
                "rollout_plan_sha256": self.rollout_hash,
                "production_plan_sha256": verifier.sha256_bytes(
                    f"plan:{phase}:{event_sequence}".encode("utf-8")
                ),
                "recovery_sha256": verifier.sha256_bytes(
                    f"recovery:{phase}:{event_sequence}".encode("utf-8")
                ),
                "change_receipt_sha256": receipt_sha256,
                "canary_sha256": verifier.sha256_bytes(
                    f"canary:{phase}:{event_sequence}".encode("utf-8")
                ),
            },
            "gates": {
                "deployment_succeeded": True,
                "migration_succeeded": True,
                "canary_succeeded": True,
            },
            "rollback": copy.deepcopy(verifier.ROLLBACK_FLOORS[phase]),
        }

    def input_for_state(
        self, state: dict[str, object]
    ) -> tuple[dict[str, object], str]:
        state_sha256 = verifier.sha256_bytes(verifier.canonical_file_bytes(state))
        normalized = copy.deepcopy(self.normalized)
        normalized["predecessor"] = {
            "run_id": "401",
            "run_attempt": 1,
            "artifact_id": "402",
            "artifact_digest": digest("phase-state-artifact"),
            "state_sha256": state_sha256,
        }
        return normalized, state_sha256

    def test_change_schema_deep_constraints_fail_closed(self) -> None:
        mutations = {
            "receipt kind allowlist": lambda schema: schema["$defs"]["phaseState"][
                "properties"
            ]["lineage"]["properties"]["predecessor_kind"]["enum"].append(
                "phase-state"
            ),
            "operation receipt binding": lambda schema: schema["$defs"][
                "phaseState"
            ]["properties"]["lineage"]["allOf"][0]["then"]["properties"][
                "predecessor_kind"
            ].__setitem__("const", "rollback-receipt"),
            "image component binding": lambda schema: schema["$defs"][
                "imageWeb"
            ]["properties"]["component"].__setitem__("const", "meta-relay"),
            "image repository binding": lambda schema: schema["$defs"][
                "imageWeb"
            ]["properties"]["repository"].__setitem__(
                "const",
                "ghcr.io/medtechcorps-netizen/rereply-release-meta-relay",
            ),
            "image subject pattern": lambda schema: schema["$defs"][
                "imageWeb"
            ]["properties"]["subject"].__setitem__("pattern", ".*"),
            "image digest equality template": lambda schema: schema["$defs"][
                "imageWeb"
            ].__setitem__(
                "x-rereply-subject-template",
                "ghcr.io/medtechcorps-netizen/rereply-release-web@{other}",
            ),
            "component uniqueness": lambda schema: schema["$defs"]["phaseState"][
                "properties"
            ]["provider_state"]["properties"]["images"].__setitem__(
                "uniqueItems", False
            ),
            "component order": lambda schema: schema["$defs"]["phaseState"][
                "properties"
            ]["provider_state"]["properties"]["images"]["prefixItems"].reverse(),
            "closed tuple": lambda schema: schema["$defs"]["phaseState"][
                "properties"
            ]["provider_state"]["properties"]["images"].__setitem__("items", {}),
            "rollback phase binding": lambda schema: schema["$defs"]["phaseState"][
                "allOf"
            ][1]["then"]["properties"]["rollback"]["const"].__setitem__(
                "allowed_targets", []
            ),
            "success gate": lambda schema: schema["$defs"]["phaseState"][
                "properties"
            ]["gates"]["properties"]["canary_succeeded"].__setitem__(
                "const", False
            ),
            "digest primitive": lambda schema: schema["$defs"][
                "digest"
            ].__setitem__("pattern", "^sha256:.*$"),
            "phase-state schema version": lambda schema: schema["$defs"][
                "phaseState"
            ]["properties"]["schema_version"]["enum"].append(3),
            "artifact binding closedness": lambda schema: schema["$defs"][
                "artifactBinding"
            ].__setitem__("additionalProperties", True),
            "v2 paired binding requirement": lambda schema: schema["$defs"][
                "phaseState"
            ]["allOf"][5]["then"]["properties"]["evidence"]["required"].pop(),
            "v1 reconciled kind exclusion": lambda schema: schema["$defs"][
                "phaseState"
            ]["allOf"][4]["then"]["properties"]["lineage"]["allOf"][0][
                "then"
            ]["properties"]["predecessor_kind"]["enum"].append(
                "apply-reconciled-receipt"
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                schema = copy.deepcopy(self.schema)
                mutate(schema)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_change_schema(schema)

    def test_provider_fingerprint_canonicalization_excludes_file_newline(self) -> None:
        value = {"z": "é", "a": [2, 1]}
        payload = verifier.canonical_payload_bytes(value)
        artifact = verifier.canonical_file_bytes(value)
        self.assertFalse(payload.endswith(b"\n"))
        self.assertTrue(artifact.endswith(b"\n"))
        self.assertNotEqual(verifier.sha256_bytes(payload), verifier.sha256_bytes(artifact))

    def test_valid_observation_is_sanitized_and_deterministic(self) -> None:
        first = self.build()
        second = self.build()
        self.assertEqual(first, second)
        encoded = verifier.canonical_file_bytes(first)
        self.assertNotIn(b"EV[", encoded)
        self.assertNotIn(b'"envs"', encoded)
        self.assertNotIn(b"test-ciphertext", encoded)
        for private_value in self.target.values():
            self.assertNotIn(private_value.encode("utf-8"), encoded)
        self.assertNotIn(ACTIVE_DEPLOYMENT_ID.encode("utf-8"), encoded)
        self.assertNotIn(APP_UPDATED_AT.encode("utf-8"), encoded)
        self.assertFalse(first["provider_validation"]["mutation_performed"])
        self.assertFalse(first["provider_validation"]["deployment_authority"])
        self.assertEqual(first["provider_observation"]["http_request_count"], 4)
        self.assertEqual(
            first["provider_observation"]["app_updated_at_sha256"],
            verifier.sha256_bytes(APP_UPDATED_AT.encode("utf-8")),
        )
        self.validate(first)

    def test_protected_target_descriptor_is_hash_bound_and_never_public(self) -> None:
        normalized = verifier.normalize_target_descriptor(
            json.dumps(self.target, separators=(",", ":")), self.contract
        )
        self.assertEqual(normalized, self.target)
        for key in self.target:
            with self.subTest(key=key):
                tampered = dict(self.target)
                if key == "app_id":
                    tampered[key] = "33333333-3333-4333-8333-333333333333"
                else:
                    tampered[key] = "https://other-target.invalid"
                with self.assertRaises(verifier.PlanError):
                    verifier.normalize_target_descriptor(
                        json.dumps(tampered, separators=(",", ":")), self.contract
                    )
        leaked = self.build()
        leaked["target"]["canary"] = self.target["default_ingress"]
        with self.assertRaises(verifier.PlanError):
            verifier.sanitize_plan(
                leaked, self.contract, private_values=tuple(self.target.values())
            )

    def test_second_snapshot_drift_fails_closed(self) -> None:
        app, _ = self.responses()
        app["app"]["active_deployment"]["phase"] = "DEPLOYING"
        with self.assertRaises(verifier.PlanError):
            self.build(second_app=app)

        app, _ = self.responses()
        app["app"]["updated_at"] = "2026-08-27T05:45:23Z"
        with self.assertRaisesRegex(
            verifier.PlanError, "production changed between the two observations"
        ):
            self.build(second_app=app)

    def test_app_updated_at_is_observation_not_predecessor_lineage(self) -> None:
        genesis_expectation, genesis_images = (
            verifier.predecessor_provider_expectation(
                self.contract, self.rollout, None
            )
        )
        self.assertNotIn("app_updated_at_sha256", genesis_expectation)
        self.assertIsNone(genesis_images)

        predecessor = self.phase_state("baseline")
        _, expected_images = verifier.rollout_phase(
            self.rollout, self.contract, "baseline"
        )
        live_spec = verifier.build_logical_candidate(
            self.spec, self.contract, expected_images, "legacy-git"
        )
        predecessor_provider = predecessor["provider_state"]
        predecessor_provider["active_deployment_identity_sha256"] = (
            verifier.sha256_bytes(ACTIVE_DEPLOYMENT_ID.encode("utf-8"))
        )
        predecessor_provider["canonical_spec_sha256"] = verifier.sha256_value(
            live_spec
        )
        predecessor_provider["environment_values_sha256"] = (
            verifier.environment_value_fingerprint(live_spec)
        )
        predecessor_provider["non_source_projection_sha256"] = (
            verifier.non_source_fingerprint(live_spec, self.contract)
        )
        historical_timestamp_hash = predecessor_provider[
            "app_updated_at_sha256"
        ]
        expected_state, phase_images_value = (
            verifier.predecessor_provider_expectation(
                self.contract, self.rollout, predecessor
            )
        )
        self.assertNotIn("app_updated_at_sha256", expected_state)

        app, deployment = self.responses()
        app["app"]["spec"] = copy.deepcopy(live_spec)
        app["app"]["updated_at"] = "2026-08-27T05:45:22Z"
        deployment["deployment"] = {
            "id": ACTIVE_DEPLOYMENT_ID,
            "phase": "ACTIVE",
            "spec": copy.deepcopy(live_spec),
        }
        observed, _ = verifier.provider_state(
            app,
            deployment,
            self.contract,
            self.target,
            expected_state,
            phase_images_value,
        )
        self.assertNotEqual(
            observed["app_updated_at_sha256"], historical_timestamp_hash
        )
        self.assertTrue(observed["predecessor_match"])

    def test_timestamp_hash_remains_strict_public_evidence(self) -> None:
        for bad_hash in ("f" * 63, "g" * 64):
            with self.subTest(bad_hash=bad_hash):
                plan = self.build()
                plan["provider_observation"]["app_updated_at_sha256"] = bad_hash
                with self.assertRaises(verifier.PlanError):
                    self.validate(plan)

        for mutate in (
            lambda state: state["provider_state"].pop(
                "app_updated_at_sha256"
            ),
            lambda state: state["provider_state"].__setitem__(
                "app_updated_at_sha256", "not-a-hash"
            ),
        ):
            state = self.phase_state("baseline")
            mutate(state)
            normalized, _ = self.input_for_state(state)
            with self.assertRaises(verifier.PlanError):
                verifier.validate_phase_state(
                    state,
                    self.contract,
                    self.policy,
                    normalized,
                    self.rollout,
                    self.policy_hash,
                    self.schema_hash,
                )

    def test_live_mutations_fail_closed(self) -> None:
        mutations = {
            "malformed timestamp": lambda app: app["app"].__setitem__(
                "updated_at", "not-a-timestamp"
            ),
            "default ingress": lambda app: app["app"].__setitem__(
                "default_ingress", "https://unexpected.invalid"
            ),
            "pending deployment": lambda app: app["app"].__setitem__(
                "pending_deployment", {"id": "x"}
            ),
            "environment": lambda app: app["app"]["spec"]["envs"][0].__setitem__(
                "value", "changed"
            ),
            "ingress": lambda app: app["app"]["spec"]["ingress"]["rules"].pop(),
            "domain": lambda app: app["app"]["spec"]["domains"].pop(),
            "source selector": lambda app: app["app"]["spec"]["services"][0].__setitem__(
                "github", {"repo": "unexpected"}
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                app, deployment = self.responses()
                mutate(app)
                expected_state, expected_images = verifier.predecessor_provider_expectation(
                    self.contract, self.rollout, None
                )
                with self.assertRaises(verifier.PlanError):
                    verifier.provider_state(
                        app,
                        deployment,
                        self.contract,
                        self.target,
                        expected_state,
                        expected_images,
                    )

    def test_logical_candidate_changes_only_four_source_envelopes(self) -> None:
        _, images, _, _ = verifier.validate_rollout_plan(
            self.rollout,
            self.contract,
            self.normalized,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            predecessor_state=None,
        )
        candidate = verifier.build_logical_candidate(
            self.spec, self.contract, images, "legacy-git"
        )
        self.assertEqual(
            verifier.strip_component_sources(candidate, self.contract),
            verifier.strip_component_sources(self.spec, self.contract),
        )
        for item in self.contract["components"]:
            component = verifier.component_index(candidate, item["collection"])[
                item["app_name"]
            ]
            self.assertNotIn("git", component)
            self.assertNotIn("dockerfile_path", component)
            self.assertEqual(set(component["image"]), {"registry_type", "registry", "repository", "digest"})

    def test_expired_or_future_plan_is_rejected(self) -> None:
        plan = self.build()
        for checked_at in (
            NOW - dt.timedelta(seconds=1),
            NOW + dt.timedelta(seconds=self.contract["plan"]["maximum_age_seconds"] + 1),
        ):
            with self.subTest(checked_at=checked_at):
                with self.assertRaises(verifier.PlanError):
                    self.validate(plan, now=checked_at)

    def test_strict_input_rejects_duplicates_floats_booleans_and_long_ids(self) -> None:
        samples = [
            '{"control_sha":"' + CONTROL_SHA + '","control_sha":"' + CONTROL_SHA + '"}',
            json.dumps({**self.normalized, "rollout_run_attempt": 1.5}),
            json.dumps({**self.normalized, "rollout_run_attempt": True}),
            json.dumps({**self.normalized, "rollout_run_id": "1" * 16}),
        ]
        for raw in samples:
            with self.subTest(raw=raw[:80]):
                with self.assertRaises(verifier.PlanError):
                    verifier.normalize_input(raw, CONTROL_SHA)

    def test_rollout_exact_file_hash_is_checked_before_provider_access(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "rollout-plan.json"
            path.write_bytes(verifier.canonical_file_bytes(self.rollout))
            bad_input = dict(self.normalized)
            bad_input["rollout_plan_sha256"] = "0" * 64
            with self.assertRaises(verifier.PlanError):
                verifier.load_rollout_plan_for_observation(
                    path,
                    self.contract,
                    bad_input,
                    policy=self.policy,
                    policy_sha256=self.policy_hash,
                    schema_sha256=self.schema_hash,
                    predecessor_state=None,
                )

    def test_provider_client_uses_only_two_exact_get_paths(self) -> None:
        app, deployment = self.responses()
        app_path, deployment_path = verifier.provider_paths(
            self.contract, self.target, ACTIVE_DEPLOYMENT_ID
        )
        origin = self.contract["provider"]["api_origin"]
        opener = FakeOpener(
            [app, deployment, app, deployment],
            [origin + app_path, origin + deployment_path, origin + app_path, origin + deployment_path],
        )
        with mock.patch.dict(os.environ, {}, clear=True):
            plan = verifier.observe(
                contract=self.contract,
                contract_sha256="a" * 64,
                policy=self.policy,
                policy_sha256=self.policy_hash,
                schema_sha256=self.schema_hash,
                verifier_sha256="b" * 64,
                normalized_input=self.normalized,
                target_descriptor=self.target,
                rollout_plan=self.rollout,
                rollout_plan_sha256=self.rollout_hash,
                predecessor_state=None,
                workflow_run_id="303",
                workflow_run_attempt=1,
                token="read-only-test-token-123456",
                now=NOW,
                opener=opener,
            )
        self.assertEqual(len(opener.requests), 4)
        self.assertEqual(
            plan["provider_observation"]["http_endpoint_labels"],
            ["app", "active-deployment"],
        )
        client = verifier.ProviderClient(
            self.contract,
            self.target,
            "read-only-test-token-123456",
            opener=opener,
        )
        with self.assertRaises(verifier.PlanError):
            client.get_json("/v2/apps")
        with self.assertRaises(verifier.PlanError):
            client.get_json(
                f"{app_path}/deployments/00000000-0000-4000-8000-000000000000"
            )

    def test_exact_two_file_artifact_and_fixed_runner_directory(self) -> None:
        plan = self.build()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            output = root / "rereply-production-plan"
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}, clear=True):
                verifier.prepare_runner_output_directory(output)
                plan_path, hash_path = verifier.write_plan_artifacts(output, plan)
            self.assertEqual(sorted(path.name for path in output.iterdir()), [
                "production-plan.json",
                "production-plan.sha256",
            ])
            _, plan_hash = verifier.load_json_and_hash(
                plan_path, "production plan", canonical=True
            )
            self.assertEqual(verifier.validate_hash_sidecar(plan_hash, hash_path), plan_hash)

    def test_output_escape_existing_directory_and_tampered_hash_fail(self) -> None:
        plan = self.build()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}, clear=True):
                with self.assertRaises(verifier.PlanError):
                    verifier.prepare_runner_output_directory(root / "wrong-name")
                output = verifier.prepare_runner_output_directory(
                    root / "rereply-production-plan"
                )
                with self.assertRaises(verifier.PlanError):
                    verifier.prepare_runner_output_directory(output)
                plan_path, hash_path = verifier.write_plan_artifacts(output, plan)
            hash_path.write_text("0" * 64 + "\n", encoding="ascii")
            _, plan_hash = verifier.load_json_and_hash(
                plan_path, "production plan", canonical=True
            )
            with self.assertRaises(verifier.PlanError):
                verifier.validate_hash_sidecar(plan_hash, hash_path)

    def test_forbidden_ambient_credential_stops_before_network(self) -> None:
        app, deployment = self.responses()
        app_path, deployment_path = verifier.provider_paths(
            self.contract, self.target, ACTIVE_DEPLOYMENT_ID
        )
        origin = self.contract["provider"]["api_origin"]
        opener = FakeOpener(
            [app, deployment, app, deployment],
            [origin + app_path, origin + deployment_path, origin + app_path, origin + deployment_path],
        )
        with mock.patch.dict(os.environ, {"DO_TOKEN": "write-capable-canary"}, clear=True):
            with self.assertRaises(verifier.PlanError):
                verifier.observe(
                    contract=self.contract,
                    contract_sha256="a" * 64,
                    policy=self.policy,
                    policy_sha256=self.policy_hash,
                    schema_sha256=self.schema_hash,
                    verifier_sha256="b" * 64,
                    normalized_input=self.normalized,
                    target_descriptor=self.target,
                    rollout_plan=self.rollout,
                    rollout_plan_sha256=self.rollout_hash,
                    predecessor_state=None,
                    workflow_run_id="303",
                    workflow_run_attempt=1,
                    token="read-only-test-token-123456",
                    now=NOW,
                    opener=opener,
                )
        self.assertEqual(opener.requests, [])

    def test_redirect_changed_url_and_malformed_body_fail_without_leaking(self) -> None:
        client = verifier.ProviderClient(
            self.contract,
            self.target,
            "read-only-test-token-123456",
            opener=FakeOpener(
                [{"sentinel": "EV[never-log-this]"}],
                ["https://api.digitalocean.com/unexpected"],
            ),
        )
        stdout = io.StringIO()
        stderr = io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            with self.assertRaises(verifier.PlanError) as captured:
                client.get_json(
                    verifier.provider_paths(
                        self.contract, self.target, ACTIVE_DEPLOYMENT_ID
                    )[0]
                )
        combined = stdout.getvalue() + stderr.getvalue() + str(captured.exception)
        self.assertNotIn("never-log-this", combined)
        with self.assertRaises(verifier.PlanError):
            verifier.RejectRedirects().redirect_request(
                None, None, 302, "redirect", {}, "https://example.invalid"
            )

    def test_sanitizer_rejects_ciphertext_tokens_and_raw_specs(self) -> None:
        plan = self.build()
        canaries = [
            ("credential prefix", lambda value: value["target"].__setitem__("canary", "EV[secret]")),
            ("raw spec key", lambda value: value.__setitem__("spec", {})),
            ("raw env key", lambda value: value.__setitem__("envs", [])),
            (
                "raw target key",
                lambda value: value["provider_observation"].__setitem__(
                    "app_id", self.target["app_id"]
                ),
            ),
            ("PEM", lambda value: value["target"].__setitem__("canary", "-----BEGIN PRIVATE KEY-----")),
        ]
        for label, mutate in canaries:
            with self.subTest(label=label):
                candidate = copy.deepcopy(plan)
                mutate(candidate)
                with self.assertRaises(verifier.PlanError):
                    verifier.sanitize_plan(candidate, self.contract)

    def test_runtime_authority_and_verifier_path_are_exact(self) -> None:
        runtime = {
            "GITHUB_REPOSITORY": self.contract["repository"],
            "GITHUB_REF": "refs/heads/main",
            "GITHUB_SHA": CONTROL_SHA,
            "GITHUB_WORKFLOW_SHA": CONTROL_SHA,
            "GITHUB_WORKFLOW_REF": (
                f"{self.contract['repository']}/{self.contract['workflow']['path']}@refs/heads/main"
            ),
            "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_RUN_ID": "303",
            "GITHUB_RUN_ATTEMPT": "1",
            "RUNNER_ENVIRONMENT": "github-hosted",
            "RUNNER_OS": "Linux",
        }
        with mock.patch.dict(os.environ, runtime, clear=True):
            verifier.verify_github_runtime(self.contract, CONTROL_SHA, "303", 1)
        runtime["GITHUB_REF"] = "refs/heads/feature"
        with mock.patch.dict(os.environ, runtime, clear=True):
            with self.assertRaises(verifier.PlanError):
                verifier.verify_github_runtime(self.contract, CONTROL_SHA, "303", 1)
        with tempfile.TemporaryDirectory() as temporary:
            other = Path(temporary) / "other.py"
            other.write_text("pass\n", encoding="utf-8")
            with self.assertRaises(verifier.PlanError):
                verifier.trusted_verifier_hash(other)

    def test_genesis_authority_selects_only_baseline(self) -> None:
        target, _images, transition, predecessor = verifier.validate_rollout_plan(
            self.rollout,
            self.contract,
            self.normalized,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            predecessor_state=None,
        )
        self.assertEqual(target["phase"], "baseline")
        self.assertEqual(
            transition,
            {
                "operation": "activate",
                "from": "genesis",
                "to": "baseline",
                "ordinal": 1,
            },
        )
        self.assertEqual(predecessor["event_sequence"], 0)
        self.assertEqual(predecessor["phase_ordinal"], 0)
        self.assertEqual(
            predecessor["state_sha256"],
            self.contract["bootstrap_state"]["genesis_state_sha256"],
        )

    def test_each_signed_phase_authorizes_only_the_next_activation(self) -> None:
        for current_phase, next_phase in zip(
            verifier.PHASES[:-1], verifier.PHASES[1:]
        ):
            with self.subTest(current=current_phase, target=next_phase):
                state = self.phase_state(current_phase)
                normalized, _ = self.input_for_state(state)
                target, _images, transition, predecessor = (
                    verifier.validate_rollout_plan(
                        self.rollout,
                        self.contract,
                        normalized,
                        policy=self.policy,
                        policy_sha256=self.policy_hash,
                        schema_sha256=self.schema_hash,
                        predecessor_state=state,
                    )
                )
                self.assertEqual(target["phase"], next_phase)
                self.assertEqual(transition["from"], current_phase)
                self.assertEqual(transition["to"], next_phase)
                self.assertEqual(
                    predecessor["event_sequence"],
                    state["lineage"]["event_sequence"],
                )
                self.assertEqual(
                    predecessor["phase_ordinal"],
                    state["lineage"]["phase_ordinal"],
                )

    def test_rollback_state_can_reauthorize_the_next_legal_activation(self) -> None:
        state = self.phase_state(
            "backend",
            event_sequence=7,
            operation="rollback",
            source_phase="ui",
            predecessor_kind="rollback-receipt",
        )
        normalized, _ = self.input_for_state(state)
        target, _images, transition, predecessor = verifier.validate_rollout_plan(
            self.rollout,
            self.contract,
            normalized,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            predecessor_state=state,
        )
        self.assertEqual(target["phase"], "ui")
        self.assertEqual(transition["ordinal"], 4)
        self.assertEqual(predecessor["event_sequence"], 7)
        self.assertEqual(predecessor["phase_ordinal"], 3)

    def test_terminal_ui_and_invalid_phase_lineage_fail_closed(self) -> None:
        ui = self.phase_state("ui")
        normalized, _ = self.input_for_state(ui)
        with self.assertRaises(verifier.PlanError):
            verifier.validate_rollout_plan(
                self.rollout,
                self.contract,
                normalized,
                policy=self.policy,
                policy_sha256=self.policy_hash,
                schema_sha256=self.schema_hash,
                predecessor_state=ui,
            )

        mutations = {
            "phase ordinal": lambda state: state["lineage"].__setitem__(
                "phase_ordinal", 4
            ),
            "activation skip": lambda state: state["lineage"].__setitem__(
                "from", "genesis"
            ),
            "receipt kind": lambda state: state["lineage"].__setitem__(
                "predecessor_kind", "rollback-receipt"
            ),
            "receipt binding": lambda state: state["evidence"].__setitem__(
                "change_receipt_sha256", "0" * 64
            ),
            "rollback floor": lambda state: state.__setitem__(
                "rollback", copy.deepcopy(verifier.ROLLBACK_FLOORS["ui"])
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                state = self.phase_state("bridge")
                mutate(state)
                normalized, _ = self.input_for_state(state)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_phase_state(
                        state,
                        self.contract,
                        self.policy,
                        normalized,
                        self.rollout,
                        self.policy_hash,
                        self.schema_hash,
                    )

    def test_phase_state_image_identity_and_digest_cross_bindings_fail_closed(self) -> None:
        def duplicate_component(state: dict[str, object]) -> None:
            images = state["provider_state"]["images"]
            images[1] = copy.deepcopy(images[0])

        def wrong_repository(state: dict[str, object]) -> None:
            state["provider_state"]["images"][0]["repository"] = (
                "ghcr.io/medtechcorps-netizen/rereply-release-meta-relay"
            )

        def wrong_subject_component(state: dict[str, object]) -> None:
            image = state["provider_state"]["images"][0]
            image["subject"] = (
                "ghcr.io/medtechcorps-netizen/rereply-release-meta-relay@"
                + image["digest"]
            )

        def subject_digest_mismatch(state: dict[str, object]) -> None:
            image = state["provider_state"]["images"][0]
            image["subject"] = image["repository"] + "@" + digest(
                "different-subject-digest"
            )

        def digest_subject_mismatch(state: dict[str, object]) -> None:
            state["provider_state"]["images"][0]["digest"] = digest(
                "different-digest-field"
            )

        def reorder_components(state: dict[str, object]) -> None:
            state["provider_state"]["images"][0:2] = reversed(
                state["provider_state"]["images"][0:2]
            )

        for label, mutate in {
            "duplicate component": duplicate_component,
            "component repository": wrong_repository,
            "component subject": wrong_subject_component,
            "subject digest": subject_digest_mismatch,
            "digest subject": digest_subject_mismatch,
            "component order": reorder_components,
        }.items():
            with self.subTest(label=label):
                state = self.phase_state("bridge")
                mutate(state)
                normalized, _ = self.input_for_state(state)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_phase_state(
                        state,
                        self.contract,
                        self.policy,
                        normalized,
                        self.rollout,
                        self.policy_hash,
                        self.schema_hash,
                    )

    def test_final_state_accepts_only_the_receipt_kind_for_its_operation(self) -> None:
        cases = (
            (
                "activate",
                "bridge",
                "baseline",
                {"apply-receipt", "reconciliation-receipt"},
            ),
            (
                "rollback",
                "backend",
                "ui",
                {
                    "rollback-receipt",
                    "orphan-rollback-receipt",
                    "reconciliation-receipt",
                },
            ),
        )
        all_kinds = {
            "genesis",
            "phase-state",
            "apply-receipt",
            "apply-reconciled-receipt",
            "rollback-receipt",
            "rollback-reconciled-receipt",
            "orphan-rollback-receipt",
            "reconciliation-receipt",
        }
        for operation, phase, source, accepted_kinds in cases:
            for accepted in sorted(accepted_kinds):
                with self.subTest(operation=operation, accepted=accepted):
                    valid = self.phase_state(
                        phase,
                        event_sequence=7,
                        operation=operation,
                        source_phase=source,
                        predecessor_kind=accepted,
                    )
                    normalized, _ = self.input_for_state(valid)
                    self.assertEqual(
                        verifier.validate_phase_state(
                            valid,
                            self.contract,
                            self.policy,
                            normalized,
                            self.rollout,
                            self.policy_hash,
                            self.schema_hash,
                        ),
                        valid,
                    )
            for rejected in sorted(all_kinds - accepted_kinds):
                with self.subTest(operation=operation, rejected=rejected):
                    invalid = self.phase_state(
                        phase,
                        event_sequence=7,
                        operation=operation,
                        source_phase=source,
                        predecessor_kind=rejected,
                    )
                    invalid_normalized, _ = self.input_for_state(invalid)
                    with self.assertRaises(verifier.PlanError):
                        verifier.validate_phase_state(
                            invalid,
                            self.contract,
                            self.policy,
                            invalid_normalized,
                            self.rollout,
                            self.policy_hash,
                            self.schema_hash,
                        )

    def test_predecessor_exact_file_and_sidecar_are_bound(self) -> None:
        state = self.phase_state("baseline")
        normalized, state_sha256 = self.input_for_state(state)
        with tempfile.TemporaryDirectory() as temporary:
            state_path = Path(temporary) / "production-phase-state.json"
            hash_path = Path(temporary) / "production-phase-state.sha256"
            state_path.write_bytes(verifier.canonical_file_bytes(state))
            hash_path.write_bytes((state_sha256 + "\n").encode("ascii"))
            self.assertEqual(
                verifier.load_predecessor_state(
                    normalized, state_path, hash_path, self.policy
                ),
                state,
            )
            hash_path.write_bytes(("0" * 64 + "\n").encode("ascii"))
            with self.assertRaises(verifier.PlanError):
                verifier.load_predecessor_state(
                    normalized, state_path, hash_path, self.policy
                )

    def test_cross_module_final_phase_state_contract(self) -> None:
        import verify_production_release as release_verifier

        direct = self.phase_state("bridge")
        normalized, _ = self.input_for_state(direct)
        self.assertEqual(
            verifier.validate_phase_state(
                direct,
                self.contract,
                self.policy,
                normalized,
                self.rollout,
                self.policy_hash,
                self.schema_hash,
            ),
            direct,
        )
        self.assertEqual(release_verifier.validate_phase_state(direct), direct)

        reconciled = self.phase_state(
            "bridge", predecessor_kind="apply-reconciled-receipt"
        )
        reconciled["schema_version"] = 2
        receipt_hash = reconciled["evidence"]["change_receipt_sha256"]
        reconciled["evidence"].update(
            {
                "change_receipt_binding": {
                    "run_id": "701",
                    "run_attempt": 1,
                    "artifact_id": "702",
                    "artifact_name": "production-phase-apply-701-1",
                    "artifact_digest": digest("reconciled-apply-artifact"),
                    "sha256": receipt_hash,
                },
                "main_lock_release_reconciliation_binding": {
                    "run_id": "703",
                    "run_attempt": 1,
                    "artifact_id": "704",
                    "artifact_name": (
                        "production-main-lock-release-reconciliation-703-1"
                    ),
                    "artifact_digest": digest("main-lock-reconciliation-artifact"),
                    "sha256": verifier.sha256_bytes(
                        b"main-lock-release-reconciliation"
                    ),
                },
            }
        )
        reconciled_normalized, _ = self.input_for_state(reconciled)
        self.assertEqual(
            verifier.validate_phase_state(
                reconciled,
                self.contract,
                self.policy,
                reconciled_normalized,
                self.rollout,
                self.policy_hash,
                self.schema_hash,
            ),
            reconciled,
        )
        self.assertEqual(
            release_verifier.validate_phase_state(reconciled), reconciled
        )

        for label, mutate in {
            "operation-kind mismatch": lambda state: state["lineage"].__setitem__(
                "predecessor_kind", "rollback-reconciled-receipt"
            ),
            "original receipt splice": lambda state: state["evidence"][
                "change_receipt_binding"
            ].__setitem__("sha256", "0" * 64),
            "release reconciliation splice": lambda state: state["evidence"][
                "main_lock_release_reconciliation_binding"
            ].__setitem__(
                "artifact_name",
                "production-main-lock-release-reconciliation-999-1",
            ),
        }.items():
            with self.subTest(label=label):
                invalid = copy.deepcopy(reconciled)
                mutate(invalid)
                invalid_normalized, _ = self.input_for_state(invalid)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_phase_state(
                        invalid,
                        self.contract,
                        self.policy,
                        invalid_normalized,
                        self.rollout,
                        self.policy_hash,
                        self.schema_hash,
                    )
                with self.assertRaises(release_verifier.ReleaseError):
                    release_verifier.validate_phase_state(invalid)

        rollback_reconciled = self.phase_state(
            "backend",
            event_sequence=7,
            operation="rollback",
            source_phase="ui",
            predecessor_kind="rollback-reconciled-receipt",
        )
        rollback_reconciled["schema_version"] = 2
        rollback_receipt_hash = rollback_reconciled["evidence"][
            "change_receipt_sha256"
        ]
        rollback_reconciled["evidence"].update(
            {
                "change_receipt_binding": {
                    "run_id": "801",
                    "run_attempt": 1,
                    "artifact_id": "802",
                    "artifact_name": "production-phase-rollback-801-1",
                    "artifact_digest": digest("reconciled-rollback-artifact"),
                    "sha256": rollback_receipt_hash,
                },
                "main_lock_release_reconciliation_binding": {
                    "run_id": "803",
                    "run_attempt": 1,
                    "artifact_id": "804",
                    "artifact_name": (
                        "production-main-lock-release-reconciliation-803-1"
                    ),
                    "artifact_digest": digest(
                        "rollback-main-lock-reconciliation-artifact"
                    ),
                    "sha256": verifier.sha256_bytes(
                        b"rollback-main-lock-release-reconciliation"
                    ),
                },
            }
        )
        rollback_normalized, _ = self.input_for_state(rollback_reconciled)
        self.assertEqual(
            verifier.validate_phase_state(
                rollback_reconciled,
                self.contract,
                self.policy,
                rollback_normalized,
                self.rollout,
                self.policy_hash,
                self.schema_hash,
            ),
            rollback_reconciled,
        )
        self.assertEqual(
            release_verifier.validate_phase_state(rollback_reconciled),
            rollback_reconciled,
        )
        wrong_rollback_receipt_name = copy.deepcopy(rollback_reconciled)
        wrong_rollback_receipt_name["evidence"]["change_receipt_binding"][
            "artifact_name"
        ] = "production-phase-apply-801-1"
        wrong_rollback_normalized, _ = self.input_for_state(
            wrong_rollback_receipt_name
        )
        with self.assertRaises(verifier.PlanError):
            verifier.validate_phase_state(
                wrong_rollback_receipt_name,
                self.contract,
                self.policy,
                wrong_rollback_normalized,
                self.rollout,
                self.policy_hash,
                self.schema_hash,
            )
        with self.assertRaises(release_verifier.ReleaseError):
            release_verifier.validate_phase_state(wrong_rollback_receipt_name)

    def test_phase_state_artifact_name_is_identical_across_producer_and_consumers(self) -> None:
        planner = WORKFLOW_PATH.read_text(encoding="utf-8")
        apply = APPLY_WORKFLOW_PATH.read_text(encoding="utf-8")
        rollback = ROLLBACK_WORKFLOW_PATH.read_text(encoding="utf-8")
        canary = CANARY_WORKFLOW_PATH.read_text(encoding="utf-8")

        # The phase is cryptographically bound inside the state.  Keeping it out of
        # the artifact name gives every producer and consumer one unambiguous rule.
        self.assertIn(
            "name: production-phase-state-${{ github.run_id }}-${{ github.run_attempt }}",
            canary,
        )
        self.assertIn(
            'expected_name="production-phase-state-${predecessor_run_id}-${predecessor_run_attempt}"',
            planner,
        )
        self.assertIn(
            r'predecessor_name=production-phase-state-{pred[\"run_id\"]}-{pred[\"run_attempt\"]}',
            apply,
        )
        self.assertIn(
            "`production-phase-state-${value.target_state.run_id}-1`",
            rollback,
        )
        for workflow in (planner, apply, rollback, canary):
            self.assertNotRegex(
                workflow,
                r"production-phase-state-(?:\$\{\{?\s*)?(?:phase|target_phase|current_phase)[-}]",
            )

    def test_anonymous_digest_pullability_is_transitively_and_currently_proved(self) -> None:
        workflow = WORKFLOW_PATH.read_text(encoding="utf-8")
        image_workflow = IMAGE_WORKFLOW_PATH.read_text(encoding="utf-8")
        pullability = self.contract["logical_source_transform"][
            "anonymous_pullability"
        ]
        self.assertEqual(
            pullability,
            {
                "mode": "anonymous-exact-digest",
                "release_image_workflow_path": (
                    ".github/workflows/build-attest-exact-release-images.yml"
                ),
                "release_image_gate_job_name": "Exact release image gate",
                "release_image_proof_step_name": (
                    "Require anonymous pullability of every exact release digest"
                ),
                "plan_recheck_step_name": (
                    "Require current anonymous pullability of every target digest"
                ),
                "fresh_docker_config_required": True,
                "registry_credentials_allowed": False,
            },
        )
        self.assertIs(
            self.policy["planning"]["anonymous_target_pullability_required"], True
        )

        self.assertIn(
            "      - name: Require anonymous pullability of every exact release digest\n",
            image_workflow,
        )
        image_pull = image_workflow.split(
            "      - name: Require anonymous pullability of every exact release digest\n",
            1,
        )[1].split("\n      - name:", 1)[0]
        self.assertNotIn("\n        if:", image_pull)
        self.assertIn("DOCKER_CONFIG:", image_pull)
        self.assertIn("unset GH_TOKEN GITHUB_TOKEN CR_PAT", image_pull)
        self.assertIn("docker pull --platform linux/amd64", image_pull)
        self.assertNotIn("docker login", image_pull.lower())
        image_gate = image_workflow.split("\n  gate:\n", 1)[1]
        self.assertIn("    name: Exact release image gate", image_gate)
        self.assertIn("      - verify_set", image_gate)
        self.assertIn('"$VERIFY_SET_RESULT"; do', image_gate)
        self.assertIn('if [[ "$result" != success ]]', image_gate)

        self.assertIn(
            "  RELEASE_IMAGE_WORKFLOW_PATH: .github/workflows/build-attest-exact-release-images.yml",
            workflow,
        )
        self.assertIn("  RELEASE_IMAGE_GATE_NAME: Exact release image gate", workflow)
        self.assertIn('"$RELEASE_IMAGE_WORKFLOW_PATH"', workflow)
        self.assertEqual(workflow.count("anonymous_image_gates_checked"), 4)
        self.assertIn('[[ "$anonymous_image_gates_checked" -eq 4 ]]', workflow)
        self.assertIn(
            "      - name: Require current anonymous pullability of every target digest\n",
            workflow,
        )
        self.assertLess(
            workflow.index(
                "      - name: Require current anonymous pullability of every target digest\n"
            ),
            workflow.index(
                "      - name: Observe exact production state using the dedicated GET-only controller\n"
            ),
        )
        observe_job = workflow.split("\n  observe:\n", 1)[1].split(
            "\n  attest:\n", 1
        )[0]
        self.assertIn("    timeout-minutes: 45", observe_job)
        self.assertEqual(self.contract["plan"]["maximum_age_seconds"], 900)
        live_pull = workflow.split(
            "      - name: Require current anonymous pullability of every target digest\n",
            1,
        )[1].split("\n      - name:", 1)[0]
        self.assertIn("env -i", live_pull)
        self.assertIn("DOCKER_CONFIG=", live_pull)
        self.assertIn("/usr/bin/docker pull --platform linux/amd64", live_pull)
        self.assertIn("unset GH_TOKEN GITHUB_TOKEN CR_PAT", live_pull)
        self.assertNotIn("docker login", live_pull.lower())
        self.assertNotIn("packages: read", workflow)

    def test_predecessor_validation_exports_only_the_exact_target_images(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            normalized_path = root / "normalized.json"
            rollout_path = root / "rollout.json"
            contract_path = root / "contract.json"
            output_path = root / "target-images.json"
            normalized_path.write_bytes(verifier.canonical_file_bytes(self.normalized))
            rollout_path.write_bytes(verifier.canonical_file_bytes(self.rollout))
            contract_path.write_bytes(verifier.canonical_file_bytes(self.contract))

            verifier.command_validate_predecessor(
                SimpleNamespace(
                    contract=contract_path,
                    policy=POLICY_PATH,
                    schema=SCHEMA_PATH,
                    normalized_input=normalized_path,
                    control_sha=CONTROL_SHA,
                    rollout_plan=rollout_path,
                    predecessor_state=None,
                    predecessor_sha256=None,
                    target_images_output=output_path,
                )
            )
            exported = json.loads(output_path.read_text(encoding="utf-8"))
            self.assertEqual(set(exported), {"schema_version", "phase", "images"})
            self.assertEqual(exported["schema_version"], 1)
            self.assertEqual(exported["phase"], "baseline")
            self.assertEqual(
                exported["images"],
                phase_images(self.rollout, "baseline", self.contract),
            )
            self.assertEqual(
                output_path.read_bytes(), verifier.canonical_file_bytes(exported)
            )

    def test_workflow_is_manual_observation_only_and_capability_separated(self) -> None:
        workflow = WORKFLOW_PATH.read_text(encoding="utf-8")
        trigger = workflow.split("\non:\n", 1)[1].split("\n# This workflow", 1)[0]
        self.assertIn("workflow_dispatch:", trigger)
        self.assertEqual(trigger.count("type: string"), 1)
        for forbidden_trigger in ("push:", "pull_request:", "schedule:", "workflow_call:"):
            self.assertNotIn(forbidden_trigger, trigger)

        self.assertIn("group: rereply-production", workflow)
        self.assertIn('[[ "$REF_PROTECTED" == "true" ]]', workflow)
        self.assertIn("environment: rereply-production-plan", workflow)
        self.assertIn(
            "https://rereply.app/attestations/observation-only-production-plan/v2",
            workflow,
        )
        self.assertIn("production-release-policy.json", workflow)
        self.assertIn("production-change.schema.json", workflow)
        self.assertIn("verify-production-crm-canary.yml", workflow)
        self.assertIn("production-phase-state.json", workflow)
        self.assertNotRegex(workflow, r"(?m)^\s*environment:\s*production\s*$")
        self.assertEqual(workflow.count("${{ secrets.DO_PRODUCTION_READ_TOKEN }}"), 1)
        self.assertEqual(workflow.count("${{ secrets.DO_PRODUCTION_TARGET_JSON }}"), 1)
        self.assertNotIn("deployments: write", workflow)
        self.assertNotIn("packages: write", workflow)
        self.assertEqual(workflow.count("id-token: write"), 1)
        self.assertEqual(workflow.count("attestations: write"), 1)

        observe = workflow.split("\n  observe:\n", 1)[1].split("\n  attest:\n", 1)[0]
        self.assertIn("actions: read", observe)
        self.assertIn("contents: read", observe)
        self.assertNotIn("id-token: write", observe)
        self.assertNotIn("attestations: write", observe)
        self.assertNotIn("actions/upload-artifact@", observe)
        controller = observe.split(
            "      - name: Observe exact production state using the dedicated GET-only controller\n",
            1,
        )[1].split("\n      - name:", 1)[0]
        self.assertIn("env -i", controller)
        self.assertIn("/usr/bin/python3 -I -S -B", controller)
        self.assertIn('DO_PRODUCTION_TARGET_JSON="$DO_PRODUCTION_TARGET_JSON"', controller)
        self.assertIn(
            "unset DO_PRODUCTION_READ_TOKEN DO_PRODUCTION_TARGET_JSON", controller
        )
        self.assertIn("require_control_blob \"$PRODUCTION_PLAN_VERIFIER_PATH\"", controller)
        self.assertIn("require_control_blob \"$PRODUCTION_CONTRACT_PATH\"", controller)
        for command in ("curl ", "wget ", "gh api", "doctl", "terraform", "docker ", "kubectl", "/propose"):
            self.assertNotIn(command, controller)

        attest = workflow.split("\n  attest:\n", 1)[1].split("\n  gate:\n", 1)[0]
        self.assertEqual(workflow.count("require_control_blob() {"), 4)
        self.assertEqual(attest.count("require_control_blob() {"), 2)
        self.assertIn("require_control_blob \"$PRODUCTION_PLAN_VERIFIER_PATH\"", attest)
        self.assertEqual(attest.count("verify-plan \\"), 2)
        self.assertIn('[[ "$embedded_plan_run_id" == "$CURRENT_RUN_ID" ]]', attest)
        self.assertIn(
            '[[ "$embedded_plan_run_attempt" == "$CURRENT_RUN_ATTEMPT" ]]', attest
        )

        gate = workflow.split("\n  gate:\n", 1)[1]
        self.assertIn("actions: read", gate)
        self.assertIn("deployments: read", gate)
        self.assertNotIn("actions: write", gate)
        self.assertNotIn("deployments: write", gate)
        self.assertEqual(workflow.count("latest_upstream_attempt="), 5)
        self.assertEqual(workflow.count('[[ "$upstream_checked" -eq 8 ]]'), 5)
        self.assertEqual(workflow.count("latest_plan_attempt="), 4)
        self.assertEqual(workflow.count("latest_predecessor_attempt="), 5)
        self.assertEqual(workflow.count("latest_successful_predecessor="), 4)
        self.assertEqual(workflow.count("latest_successful_id="), 1)
        self.assertGreaterEqual(
            workflow.count('[[ "$CURRENT_RUN_ATTEMPT" == "$AUTHORITY_PLAN_RUN_ATTEMPT" ]]'),
            4,
        )

        uses = re.findall(r"(?m)^\s*uses:\s*([^\s#]+)", workflow)
        self.assertEqual(
            Counter(uses),
            Counter(
                {
                    "actions/checkout@11d5960a326750d5838078e36cf38b85af677262": 3,
                    "actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093": 2,
                    "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02": 2,
                    "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6": 2,
                }
            ),
        )
        for action in uses:
            with self.subTest(action=action):
                self.assertRegex(action, r"^[^@]+@[0-9a-f]{40}$")

        lower = workflow.lower()
        for mutation in (
            "doctl apps update",
            "create-deployment",
            "--force-rebuild",
            "update_all_source_versions",
            "post /v2/apps",
            "put /v2/apps",
            "patch /v2/apps",
            "delete /v2/apps",
        ):
            self.assertNotIn(mutation, lower)
        protected_ci = TEST_WORKFLOW_PATH.read_text(encoding="utf-8")
        self.assertIn("release/deployment/verify_production_plan.py", protected_ci)
        self.assertIn("release/deployment/test_verify_production_plan.py", protected_ci)


class ReviewedProductionContractTests(unittest.TestCase):
    def test_reviewed_contract_is_valid_against_unpatched_constants(self) -> None:
        production_contract = verifier.load_json(CONTRACT_PATH, "contract")
        verifier.validate_contract(production_contract)
        self.assertNotIn(
            "app_updated_at_sha256", production_contract["bootstrap_state"]
        )
        self.assertEqual(
            production_contract["bootstrap_state"]["genesis_state_sha256"],
            "c43ed05bc18c2be1c23ab42c85918a821390d39230573780835522481cddcb5d",
        )
        self.assertEqual(
            verifier.genesis_state_sha256(production_contract),
            production_contract["bootstrap_state"]["genesis_state_sha256"],
        )


if __name__ == "__main__":
    unittest.main()
