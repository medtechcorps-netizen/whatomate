from __future__ import annotations

import base64
import copy
import datetime as dt
import hashlib
import io
import json
import os
import re
import subprocess
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
SOURCE_MANIFEST_PATH = ROOT / "release" / "exact-sources.json"
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
                "name": "test-postgres-runtime-binding",
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
        source_sha = {
            "baseline": verifier.BASELINE_TARGET_SOURCE_SHA,
            "ui": verifier.UI_TARGET_SOURCE_SHA,
        }.get(phase, f"{index + 1:x}" * 40)
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
                "source": {
                    "repository": "medtechcorps-netizen/whatomate",
                    "commit": source_sha,
                    "root_tree": f"{index + 5:x}" * 40,
                    "frontend_tree": "a" * 40,
                    "internal_tree": f"{index + 9:x}" * 40,
                    "manifest_sha256": "b" * 64,
                },
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


def digest_source_spec() -> dict[str, object]:
    """The reviewed production app with digest-image sources instead of git."""
    spec = fake_spec()
    records = {record["component"]: record for record in verifier.BOOTSTRAP_IMAGES}
    for collection in ("services", "jobs"):
        for component in spec[collection]:
            component.pop("git", None)
            component.pop("dockerfile_path", None)
            release_component = (
                "web"
                if component["name"] in {"omnitech-web", "rereply-rls-migrate"}
                else component["name"]
            )
            record = records[release_component]
            component["image"] = {
                "registry_type": "GHCR",
                "registry": "ghcr.io",
                "repository": record["repository"].removeprefix("ghcr.io/"),
                "digest": record["digest"],
            }
    return spec


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
        # The synthetic production app is git-sourced, so the fixture models the
        # legacy bootstrap shape explicitly.
        self.contract["bootstrap_state"]["source_mode"] = "legacy-git"
        for key in ("images", "live_phase", "live_evidence"):
            self.contract["bootstrap_state"].pop(key, None)
        vpc_hash = verifier.sha256_bytes(self.spec["vpc"]["id"].encode("utf-8"))
        db_inventory = database_inventory(self.spec)
        self.contract["expected_topology"]["vpc_id_sha256"] = vpc_hash
        self.contract["expected_topology"]["databases"] = [
            {
                "engine": engine,
                "version": version,
                "production": production,
                "name_sha256": name_sha256,
                "cluster_sha256": cluster_sha256,
            }
            for engine, version, production, name_sha256, cluster_sha256 in sorted(
                db_inventory
            )
        ]

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
            # The synthetic production app is still git-sourced, so the fixture
            # pins the bootstrap's mode to the legacy lineage it models.
            mock.patch.object(verifier, "BOOTSTRAP_SOURCE_MODE", "legacy-git"),
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
        def rollback_constraint(schema: dict[str, object], phase: str) -> dict[str, object]:
            matches = []
            for condition in schema["$defs"]["phaseState"]["allOf"]:
                try:
                    anchored_phase = condition["if"]["properties"]["lineage"][
                        "properties"
                    ]["phase"]["const"]
                    constraint = condition["then"]["properties"]["rollback"]
                except (KeyError, TypeError):
                    continue
                if anchored_phase == phase:
                    matches.append(constraint)
            self.assertEqual(len(matches), 1)
            return matches[0]

        def duplicate_bridge_variant(schema: dict[str, object]) -> None:
            variants = rollback_constraint(schema, "bridge")["oneOf"]
            variants[1] = copy.deepcopy(variants[0])

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
            "non-bridge rollback phase binding": lambda schema: rollback_constraint(
                schema, "baseline"
            )["const"]["allowed_targets"].append(
                "baseline"
            ),
            "bridge rollback variant missing": lambda schema: rollback_constraint(
                schema, "bridge"
            )["oneOf"].pop(),
            "bridge rollback variant extra": lambda schema: rollback_constraint(
                schema, "bridge"
            )["oneOf"].append(
                {"const": copy.deepcopy(verifier.ROLLBACK_FLOORS["baseline"])}
            ),
            "bridge rollback variant altered": lambda schema: rollback_constraint(
                schema, "bridge"
            )["oneOf"][1]["const"]["forbidden_targets"].__setitem__(0, "bridge"),
            "bridge rollback variant dead": duplicate_bridge_variant,
            "bridge rollback variants swapped": lambda schema: rollback_constraint(
                schema, "bridge"
            )["oneOf"].reverse(),
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

    def test_database_alias_topology_is_exact_and_order_independent(self) -> None:
        verifier.validate_topology(self.spec, self.contract)

        reordered = copy.deepcopy(self.spec)
        reordered["databases"][0:2] = reversed(reordered["databases"][0:2])
        verifier.validate_topology(reordered, self.contract)

        def replace_runtime_with_second_valkey(spec: dict[str, object]) -> None:
            duplicate = copy.deepcopy(spec["databases"][2])
            duplicate["name"] = "test-second-valkey-binding"
            spec["databases"][1] = duplicate

        def drift_both_pg_versions(spec: dict[str, object]) -> None:
            spec["databases"][0]["version"] = "16"
            spec["databases"][1]["version"] = "16"

        def drift_both_pg_clusters(spec: dict[str, object]) -> None:
            spec["databases"][0]["cluster_name"] = "test-other-postgres-cluster"
            spec["databases"][1]["cluster_name"] = "test-other-postgres-cluster"

        mutations = {
            "missing runtime alias": lambda spec: spec["databases"].pop(1),
            "extra alias": lambda spec: spec["databases"].append(
                copy.deepcopy(spec["databases"][1])
            ),
            "duplicate alias name": lambda spec: spec["databases"][1].__setitem__(
                "name", spec["databases"][0]["name"]
            ),
            "different PG cluster": lambda spec: spec["databases"][1].__setitem__(
                "cluster_name", "test-other-postgres-cluster"
            ),
            "different PG version": lambda spec: spec["databases"][1].__setitem__(
                "version", "16"
            ),
            "both PG versions drift": drift_both_pg_versions,
            "both PG clusters drift": drift_both_pg_clusters,
            "non-production PG alias": lambda spec: spec["databases"][1].__setitem__(
                "production", False
            ),
            "Valkey version drift": lambda spec: spec["databases"][2].__setitem__(
                "version", "7"
            ),
            "Valkey cluster drift": lambda spec: spec["databases"][2].__setitem__(
                "cluster_name", "test-other-valkey-cluster"
            ),
            "Valkey name drift": lambda spec: spec["databases"][2].__setitem__(
                "name", "test-other-valkey-binding"
            ),
            "physical clusters overlap": lambda spec: spec["databases"][
                2
            ].__setitem__("cluster_name", spec["databases"][0]["cluster_name"]),
            "cross-engine binding name collision": lambda spec: spec["databases"][
                2
            ].__setitem__("name", spec["databases"][0]["name"]),
            "duplicate Valkey binding": replace_runtime_with_second_valkey,
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                candidate = copy.deepcopy(self.spec)
                mutate(candidate)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_topology(candidate, self.contract)

        reordered_contract = copy.deepcopy(self.contract)
        reordered_contract["expected_topology"]["databases"].reverse()
        verifier.validate_contract(
            reordered_contract,
            policy_value=self.policy,
            schema_value=self.schema,
        )

        def replace_contract_runtime_with_second_valkey(
            value: dict[str, object],
        ) -> None:
            duplicate = copy.deepcopy(value["expected_topology"]["databases"][2])
            duplicate["name_sha256"] = "8" * 64
            value["expected_topology"]["databases"][1] = duplicate

        def drift_both_contract_pg_versions(value: dict[str, object]) -> None:
            value["expected_topology"]["databases"][0]["version"] = "16"
            value["expected_topology"]["databases"][1]["version"] = "16"

        def drift_both_contract_pg_clusters(value: dict[str, object]) -> None:
            value["expected_topology"]["databases"][0]["cluster_sha256"] = "7" * 64
            value["expected_topology"]["databases"][1]["cluster_sha256"] = "7" * 64

        contract_mutations = {
            "missing contract runtime alias": lambda value: value[
                "expected_topology"
            ]["databases"].pop(1),
            "extra contract alias": lambda value: value["expected_topology"][
                "databases"
            ].append(copy.deepcopy(value["expected_topology"]["databases"][1])),
            "duplicate contract alias name": lambda value: value[
                "expected_topology"
            ]["databases"][1].__setitem__(
                "name_sha256",
                value["expected_topology"]["databases"][0]["name_sha256"],
            ),
            "different contract PG cluster": lambda value: value[
                "expected_topology"
            ]["databases"][1].__setitem__("cluster_sha256", "9" * 64),
            "different contract PG version": lambda value: value[
                "expected_topology"
            ]["databases"][1].__setitem__("version", "16"),
            "both contract PG versions drift": drift_both_contract_pg_versions,
            "both contract PG clusters drift": drift_both_contract_pg_clusters,
            "unique contract alias name drift": lambda value: value[
                "expected_topology"
            ]["databases"][1].__setitem__("name_sha256", "6" * 64),
            "non-production contract PG alias": lambda value: value[
                "expected_topology"
            ]["databases"][1].__setitem__("production", False),
            "contract Valkey version drift": lambda value: value[
                "expected_topology"
            ]["databases"][2].__setitem__("version", "7"),
            "contract Valkey cluster drift": lambda value: value[
                "expected_topology"
            ]["databases"][2].__setitem__("cluster_sha256", "5" * 64),
            "contract physical clusters overlap": lambda value: value[
                "expected_topology"
            ]["databases"][2].__setitem__(
                "cluster_sha256",
                value["expected_topology"]["databases"][0]["cluster_sha256"],
            ),
            "duplicate contract Valkey binding": (
                replace_contract_runtime_with_second_valkey
            ),
        }
        for label, mutate in contract_mutations.items():
            with self.subTest(label=label):
                candidate = copy.deepcopy(self.contract)
                mutate(candidate)
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_contract(
                        candidate,
                        policy_value=self.policy,
                        schema_value=self.schema,
                    )

        malformed_contract = copy.deepcopy(self.contract)
        malformed_contract["expected_topology"]["databases"][0]["unexpected"] = True
        with self.assertRaises(verifier.PlanError):
            verifier.validate_contract(
                malformed_contract,
                policy_value=self.policy,
                schema_value=self.schema,
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
        source_manifest = json.loads(SOURCE_MANIFEST_PATH.read_text(encoding="utf-8"))
        self.assertEqual(
            source_manifest["phases"]["baseline"]["source_sha"],
            verifier.BASELINE_TARGET_SOURCE_SHA,
        )
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
            target["source"]["commit"], verifier.BASELINE_TARGET_SOURCE_SHA
        )
        # The synthetic fixture models the legacy bootstrap shape; the reviewed
        # production contract is re-baselined onto image authority and is
        # asserted separately in ReviewedProductionContractTests.
        self.assertEqual(verifier.BOOTSTRAP_SOURCE_MODE, "legacy-git")
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

    def test_genesis_rejects_a_phase_state_predecessor(self) -> None:
        with self.assertRaisesRegex(
            verifier.PlanError, "genesis input unexpectedly contains a phase state"
        ):
            verifier.validate_rollout_plan(
                self.rollout,
                self.contract,
                self.normalized,
                policy=self.policy,
                policy_sha256=self.policy_hash,
                schema_sha256=self.schema_hash,
                predecessor_state=self.phase_state("baseline"),
            )

    def test_database_phase_sources_are_independently_allocated(self) -> None:
        for key in ("commit", "root_tree", "internal_tree"):
            with self.subTest(key=key):
                rollout = copy.deepcopy(self.rollout)
                rollout["phases"][1]["source"][key] = rollout["phases"][0][
                    "source"
                ][key]
                with self.assertRaisesRegex(
                    verifier.PlanError,
                    rf"database phase source allocation is not unique: {key}",
                ):
                    verifier.validate_rollout_plan(
                        rollout,
                        self.contract,
                        self.normalized,
                        policy=self.policy,
                        policy_sha256=self.policy_hash,
                        schema_sha256=self.schema_hash,
                        predecessor_state=None,
                    )

        rollout = copy.deepcopy(self.rollout)
        rollout["phases"][3]["source"]["manifest_sha256"] = "c" * 64
        with self.assertRaisesRegex(
            verifier.PlanError,
            "rollout source manifest authority differs across phases",
        ):
            verifier.validate_rollout_plan(
                rollout,
                self.contract,
                self.normalized,
                policy=self.policy,
                policy_sha256=self.policy_hash,
                schema_sha256=self.schema_hash,
                predecessor_state=None,
            )

    def test_effective_rollback_floor_is_strict_and_canonical(self) -> None:
        self.assertEqual(
            verifier.validate_effective_rollback_floor(
                copy.deepcopy(verifier.ROLLBACK_FLOORS["bridge"]),
                "bridge",
                "test rollback floor",
            ),
            verifier.ROLLBACK_FLOORS["bridge"],
        )
        self.assertEqual(
            verifier.validate_effective_rollback_floor(
                copy.deepcopy(verifier.HARDENED_BRIDGE_ROLLBACK_FLOOR),
                "bridge",
                "test rollback floor",
            ),
            verifier.HARDENED_BRIDGE_ROLLBACK_FLOOR,
        )
        invalid = {
            "missing key": {"allowed_targets": []},
            "extra key": {
                "allowed_targets": [],
                "forbidden_targets": ["baseline"],
                "source_phase": "backend",
            },
            "allowed not list": {
                "allowed_targets": "baseline",
                "forbidden_targets": [],
            },
            "forbidden not list": {
                "allowed_targets": [],
                "forbidden_targets": ("baseline",),
            },
            "unknown phase": {
                "allowed_targets": [],
                "forbidden_targets": ["genesis"],
            },
            "duplicate": {
                "allowed_targets": ["baseline", "baseline"],
                "forbidden_targets": [],
            },
            "overlap": {
                "allowed_targets": ["baseline"],
                "forbidden_targets": ["baseline"],
            },
            "noncanonical order": {
                "allowed_targets": ["bridge", "backend"],
                "forbidden_targets": ["baseline"],
            },
            "unreviewed canonical floor": {
                "allowed_targets": [],
                "forbidden_targets": ["bridge"],
            },
        }
        for label, rollback in invalid.items():
            with self.subTest(label=label):
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_effective_rollback_floor(
                        rollback,
                        "bridge" if label != "noncanonical order" else "ui",
                        "test rollback floor",
                    )

    def test_hardened_bridge_state_advances_to_backend_intrinsic_floor(self) -> None:
        state = self.phase_state(
            "bridge",
            event_sequence=7,
            operation="rollback",
            source_phase="backend",
            predecessor_kind="rollback-receipt",
        )
        state["rollback"] = copy.deepcopy(
            verifier.HARDENED_BRIDGE_ROLLBACK_FLOOR
        )

        _, bridge_images = verifier.rollout_phase(
            self.rollout, self.contract, "bridge"
        )
        live_spec = verifier.build_logical_candidate(
            self.spec,
            self.contract,
            bridge_images,
            "legacy-git",
        )
        provider = state["provider_state"]
        provider["active_deployment_identity_sha256"] = verifier.sha256_bytes(
            ACTIVE_DEPLOYMENT_ID.encode("utf-8")
        )
        provider["canonical_spec_sha256"] = verifier.sha256_value(live_spec)
        provider["environment_values_sha256"] = (
            verifier.environment_value_fingerprint(live_spec)
        )
        provider["non_source_projection_sha256"] = verifier.non_source_fingerprint(
            live_spec, self.contract
        )

        normalized, _ = self.input_for_state(state)
        target, _images, transition, _predecessor = verifier.validate_rollout_plan(
            self.rollout,
            self.contract,
            normalized,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            predecessor_state=state,
        )
        self.assertEqual(transition["from"], "bridge")
        self.assertEqual(transition["operation"], "activate")
        self.assertEqual(transition["ordinal"], 3)
        self.assertEqual(target["phase"], "backend")
        self.assertEqual(target["rollback"], verifier.ROLLBACK_FLOORS["backend"])

        app, deployment = self.responses()
        app["app"]["spec"] = copy.deepcopy(live_spec)
        deployment["deployment"]["spec"] = copy.deepcopy(live_spec)
        app_path, deployment_path = verifier.provider_paths(
            self.contract, self.target, ACTIVE_DEPLOYMENT_ID
        )
        plan = verifier.build_plan(
            contract=self.contract,
            contract_sha256="a" * 64,
            policy=self.policy,
            policy_sha256=self.policy_hash,
            schema_sha256=self.schema_hash,
            verifier_sha256="b" * 64,
            normalized_input=normalized,
            target_descriptor=self.target,
            rollout_plan=self.rollout,
            predecessor_state=state,
            first_app_response=app,
            first_deployment_response=deployment,
            second_app_response=copy.deepcopy(app),
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
        self.assertEqual(plan["target"]["phase"], "backend")
        self.assertEqual(plan["rollback"], verifier.ROLLBACK_FLOORS["backend"])
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
            state,
            now=NOW,
        )

    def test_hardened_bridge_floor_is_provenance_bound_and_fail_closed(self) -> None:
        activation = self.phase_state("bridge")
        activation["rollback"] = copy.deepcopy(
            verifier.HARDENED_BRIDGE_ROLLBACK_FLOOR
        )
        activation_normalized, _ = self.input_for_state(activation)
        with self.assertRaisesRegex(
            verifier.PlanError,
            "phase-state rollback floor provenance differs",
        ):
            verifier.validate_phase_state(
                activation,
                self.contract,
                self.policy,
                activation_normalized,
                self.rollout,
                self.policy_hash,
                self.schema_hash,
            )

        mutations = {
            "overlap": {
                "allowed_targets": ["baseline"],
                "forbidden_targets": ["baseline"],
            },
            "duplicate forbidden": {
                "allowed_targets": [],
                "forbidden_targets": ["baseline", "baseline"],
            },
            "forbidden not list": {
                "allowed_targets": [],
                "forbidden_targets": "baseline",
            },
            "unknown phase": {
                "allowed_targets": [],
                "forbidden_targets": ["genesis"],
            },
            "extra key": {
                "allowed_targets": [],
                "forbidden_targets": ["baseline"],
                "source_phase": "backend",
            },
            "noncanonical order": {
                "allowed_targets": [],
                "forbidden_targets": ["bridge", "baseline"],
            },
        }
        for label, rollback in mutations.items():
            with self.subTest(label=label):
                state = self.phase_state(
                    "bridge",
                    event_sequence=7,
                    operation="rollback",
                    source_phase="backend",
                    predecessor_kind="rollback-receipt",
                )
                state["rollback"] = rollback
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
            'predecessor_name=production-phase-state-{}-{}".format(pred["run_id"],pred["run_attempt"])',
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

    def test_isolated_predecessor_cli_all_transitions_and_required_output(self) -> None:
        # Unlike the in-process synthetic provider tests, run a fresh isolated
        # Python process from a different cwd. No provider observation or
        # credential is needed by this command. The real reviewed contract (with
        # its real compiled pins) re-entered at the live ui phase, so its genesis
        # targets ui and no signed state below ui is a predecessor. A synthetic
        # legacy-git contract, which enters at baseline, keeps every predecessor
        # transition and the CLI's exact-file sidecar binding covered end to end:
        # it runs the same verifier file through an isolated wrapper that sets
        # only the synthetic fixture's pinned provider and bootstrap constants.
        legacy = copy.deepcopy(self.contract)
        legacy_pins = {name: getattr(verifier, name) for name in (
            "PRODUCTION_VPC_ID_SHA256", "PRODUCTION_DATABASE_INVENTORY", "BOOTSTRAP_SOURCE_MODE",
            "BOOTSTRAP_CANONICAL_SPEC_SHA256", "BOOTSTRAP_ENVIRONMENT_SHA256", "PRODUCTION_APP_ID_SHA256",
            "PRODUCTION_DEFAULT_INGRESS_SHA256", "BOOTSTRAP_DEPLOYMENT_ID_SHA256", "BOOTSTRAP_NON_SOURCE_SHA256",
        )}
        legacy_pins["PRODUCTION_DATABASE_INVENTORY"] = sorted(
            list(item) for item in legacy_pins["PRODUCTION_DATABASE_INVENTORY"]
        )
        wrapper_source = (
            "import importlib.util, json, sys\n"
            "verifier_path, pins_path = sys.argv[1], sys.argv[2]\n"
            "spec = importlib.util.spec_from_file_location('verify_production_plan', verifier_path)\n"
            "module = importlib.util.module_from_spec(spec)\n"
            "sys.modules[spec.name] = module\n"
            "spec.loader.exec_module(module)\n"
            "pins = json.load(open(pins_path, encoding='utf-8'))\n"
            "pins['PRODUCTION_DATABASE_INVENTORY'] = {tuple(item) for item in pins['PRODUCTION_DATABASE_INVENTORY']}\n"
            "for name, value in pins.items():\n"
            "    assert hasattr(module, name), name\n"
            "    setattr(module, name, value)\n"
            "sys.argv = [verifier_path, *sys.argv[3:]]\n"
            "raise SystemExit(module.main())\n"
        )
        reviewed = json.loads(CONTRACT_PATH.read_text(encoding="utf-8"))
        env = {key: os.environ[key] for key in ("SYSTEMROOT", "WINDIR", "TEMP", "TMP") if key in os.environ}
        for label, contract in (("reviewed", reviewed), ("legacy", legacy)):
            for phase in ("genesis", "baseline", "bridge", "backend"):
                with self.subTest(contract=label, predecessor=phase), \
                        tempfile.TemporaryDirectory(prefix="predecessor-cli-") as name:
                    root = Path(name)
                    self.contract = contract
                    command = [sys.executable, "-I", "-S", "-B", str(VERIFIER_PATH)]
                    contract_path = CONTRACT_PATH
                    if label == "legacy":
                        contract_path = root / "contract.json"
                        contract_path.write_bytes(verifier.canonical_file_bytes(contract))
                        (root / "pins.json").write_text(json.dumps(legacy_pins), encoding="utf-8")
                        (root / "run_verifier.py").write_text(wrapper_source, encoding="utf-8")
                        command = [sys.executable, "-I", "-S", "-B", str(root / "run_verifier.py"),
                                   str(VERIFIER_PATH), str(root / "pins.json")]
                    state = None if phase == "genesis" else self.phase_state(phase)
                    normalized = self.normalized if state is None else self.input_for_state(state)[0]
                    (root / "normalized.json").write_bytes(verifier.canonical_file_bytes(normalized))
                    (root / "rollout.json").write_bytes(verifier.canonical_file_bytes(self.rollout))
                    args = command + [
                        "validate-predecessor",
                        "--contract", str(contract_path), "--policy", str(POLICY_PATH),
                        "--schema", str(SCHEMA_PATH), "--normalized-input", str(root / "normalized.json"),
                        "--rollout-plan", str(root / "rollout.json"), "--control-sha", CONTROL_SHA]
                    if state is not None:
                        raw = verifier.canonical_file_bytes(state)
                        (root / "state.json").write_bytes(raw)
                        (root / "state.sha256").write_bytes((verifier.sha256_bytes(raw) + "\n").encode("ascii"))
                        args += ["--predecessor-state", str(root / "state.json"),
                                 "--predecessor-sha256", str(root / "state.sha256")]
                    args += ["--target-images-output", str(root / "target-images.json")]
                    result = subprocess.run(args, cwd=root, env=env, capture_output=True, text=True, timeout=15)
                    missing_output = subprocess.run(args[:-2], cwd=root, env=env,
                                                    capture_output=True, text=True, timeout=15)
                    self.assertEqual(missing_output.returncode, 2)
                    self.assertIn("--target-images-output", missing_output.stderr)
                    if label == "reviewed" and state is not None:
                        # No signed state below the reviewed live ui phase can be
                        # a predecessor of this genesis epoch.
                        self.assertEqual(result.returncode, 1)
                        self.assertIn("phase state precedes the reviewed live phase", result.stderr)
                        self.assertFalse((root / "target-images.json").exists())
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        if state is None:
                            target = verifier.genesis_target_phase(contract)
                            self.assertEqual(target, "ui" if label == "reviewed" else "baseline")
                        else:
                            target = verifier.PHASES[verifier.PHASES.index(phase) + 1]
                        exported = json.loads((root / "target-images.json").read_text(encoding="utf-8"))
                        self.assertEqual(exported["phase"], target)
                        self.assertEqual(exported["images"], phase_images(self.rollout, target, contract))
                    if state is not None:
                        # The exact-file sidecar is bound before any lineage rule.
                        (root / "state.sha256").write_bytes(("0" * 64 + "\n").encode("ascii"))
                        tampered_output = root / "tampered-target-images.json"
                        tampered_args = args[:-1] + [str(tampered_output)]
                        tampered = subprocess.run(tampered_args, cwd=root, env=env,
                                                  capture_output=True, text=True, timeout=15)
                        self.assertNotEqual(tampered.returncode, 0)
                        self.assertIn("production phase-state predecessor exact-file hash differs", tampered.stderr)
                        self.assertFalse(tampered_output.exists())

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
        self.assertIn("  release-controls:\n", protected_ci)
        self.assertIn("      - release-controls\n", protected_ci)
        self.assertIn("python3 -B -m unittest discover -s release/deployment -p 'test_*.py' -v", protected_ci)


GENESIS_PIN = "b7892b2caaaf66ef132791b19c2a69dc200b40197a1420f4aae0b207ef0ae793"
LIVE_EVIDENCE_DIR = ROOT / "release" / "deployment" / "live-evidence"
REPOSITORY_URL = "https://github.com/medtechcorps-netizen/whatomate"
SLSA_PREDICATE = "https://slsa.dev/provenance/v1"
SIGSTORE_BUNDLE_MEDIA_TYPE = "application/vnd.dev.sigstore.bundle.v0.3+json"
IN_TOTO_STATEMENT_TYPE = "https://in-toto.io/Statement/v1"
# Every committed evidence document kind: its exact-file name prefix, signed
# subject file name, producing workflow and custom attestation predicate.
EVIDENCE_AUTHORITIES = {
    "production-phase-apply-receipt": {
        "prefix": "production-phase-apply-receipt",
        "subject": "production-phase-apply-receipt.json",
        "workflow_path": ".github/workflows/apply-production-phase.yml",
        "predicate_type": "https://rereply.app/attestations/production-phase-apply-receipt/v1",
    },
    "production-phase-state": {
        "prefix": "production-phase-state",
        "subject": "production-phase-state.json",
        "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
        "predicate_type": "https://rereply.app/attestations/production-phase-state/v1",
    },
}


def reentry_fixture(test: unittest.TestCase, live_phase: object) -> "ProductionPlanTests":
    """The synthetic plan fixture as a digest bootstrap re-entered at live_phase."""
    base = legacy_fixture(test)
    bootstrap = base.contract["bootstrap_state"]
    bootstrap["source_mode"] = "digest-images"
    bootstrap["images"] = copy.deepcopy(verifier.BOOTSTRAP_IMAGES)
    bootstrap["live_phase"] = live_phase
    bootstrap["live_evidence"] = {**copy.deepcopy(verifier.BOOTSTRAP_LIVE_EVIDENCE), "phase": live_phase}
    bootstrap["genesis_state_sha256"] = verifier.genesis_state_sha256(base.contract)
    return base


def legacy_fixture(test: unittest.TestCase) -> "ProductionPlanTests":
    """The synthetic legacy-git plan fixture; its genesis enters at baseline."""
    base = ProductionPlanTests()
    try:
        base.setUp()
    except BaseException:
        base.doCleanups()
        raise
    test.addCleanup(base.doCleanups)
    return base


def _certificate_pem(der: bytes) -> str:
    body = base64.b64encode(der).decode("ascii")
    lines = [body[index:index + 64] for index in range(0, len(body), 64)]
    return "-----BEGIN CERTIFICATE-----\n" + "\n".join(lines) + "\n-----END CERTIFICATE-----\n"


def attestation_statement(raw: bytes, *, subject: str, subject_sha256: str,
                          control: dict[str, object]) -> dict[str, object]:
    """Offline binding of one committed Sigstore bundle to its committed subject.

    This is not cryptographic verification: the signature, Fulcio chain and
    Rekor inclusion were verified online with `gh attestation verify` (recorded
    in docs/crm-production-release-control.md). It proves that the committed
    bundle is the one over these exact bytes: the in-toto subject, the Rekor
    entry's payload hash, signature and certificate, and the signer identity
    the certificate carries.
    """
    import verify_production_release as release

    bundle = release.loads_strict(raw)
    if type(bundle) is not dict or set(bundle) != {"mediaType", "verificationMaterial", "dsseEnvelope"}:
        raise AssertionError("attestation bundle shape differs")
    if bundle["mediaType"] != SIGSTORE_BUNDLE_MEDIA_TYPE:
        raise AssertionError("attestation bundle media type differs")
    envelope = bundle["dsseEnvelope"]
    if (type(envelope) is not dict or set(envelope) != {"payload", "payloadType", "signatures"}
            or envelope["payloadType"] != "application/vnd.in-toto+json"
            or type(envelope["signatures"]) is not list or len(envelope["signatures"]) != 1
            or set(envelope["signatures"][0]) != {"sig"} or not envelope["signatures"][0]["sig"]):
        raise AssertionError("attestation envelope differs")
    payload = base64.b64decode(envelope["payload"], validate=True)
    statement = release.loads_strict(payload)
    if type(statement) is not dict or set(statement) != {"_type", "subject", "predicateType", "predicate"}:
        raise AssertionError("attestation statement shape differs")
    if statement["_type"] != IN_TOTO_STATEMENT_TYPE:
        raise AssertionError("attestation statement type differs")
    if statement["subject"] != [{"name": subject, "digest": {"sha256": subject_sha256}}]:
        raise AssertionError("attestation subject differs")
    material = bundle["verificationMaterial"]
    if (type(material) is not dict
            or set(material) != {"tlogEntries", "timestampVerificationData", "certificate"}
            or type(material["tlogEntries"]) is not list or len(material["tlogEntries"]) != 1
            or set(material["certificate"]) != {"rawBytes"}):
        raise AssertionError("attestation verification material differs")
    certificate = base64.b64decode(material["certificate"]["rawBytes"], validate=True)
    entry = material["tlogEntries"][0]
    body = release.loads_strict(base64.b64decode(entry["canonicalizedBody"], validate=True))
    if (entry["kindVersion"] != {"kind": "dsse", "version": "0.0.1"}
            or body.get("kind") != "dsse" or body.get("apiVersion") != "0.0.1"
            or body["spec"]["payloadHash"] != {"algorithm": "sha256", "value": hashlib.sha256(payload).hexdigest()}
            or body["spec"]["signatures"] != [{
                "signature": envelope["signatures"][0]["sig"],
                "verifier": base64.b64encode(_certificate_pem(certificate).encode("ascii")).decode("ascii"),
            }]):
        raise AssertionError("attestation transparency-log entry differs")
    workflow = control["workflow_path"]
    identity = (
        f"{REPOSITORY_URL}/{workflow}@refs/heads/main",
        str(control["workflow_sha"]),
        "refs/heads/main",
        "workflow_dispatch",
        "github-hosted",
        f"{REPOSITORY_URL}/actions/runs/{control['run_id']}/attempts/{control['run_attempt']}",
    )
    if not all(value.encode("ascii") in certificate for value in identity):
        raise AssertionError("attestation signer identity differs")
    return statement


def classify_live_evidence(directory: Path) -> dict[str, dict[str, object]]:
    """Validate every committed live-evidence file; an unreviewed file fails closed.

    Each evidence document is a canonical apply receipt or phase state named by
    its own control run, with an exact-hash sidecar and exactly two attestation
    bundles over its exact bytes: SLSA provenance and its custom predicate.
    """
    import verify_production_release as release

    documents: dict[str, bytes] = {}
    sidecars: dict[str, bytes] = {}
    bundles: dict[str, bytes] = {}
    for path in sorted(directory.iterdir()):
        if path.is_symlink() or not path.is_file():
            raise AssertionError(f"live evidence is not a regular file: {path.name}")
        if path.name.endswith(".sigstore.json"):
            bundles[path.name] = path.read_bytes()
        elif path.name.endswith(".sha256"):
            sidecars[path.name] = path.read_bytes()
        elif path.name.endswith(".json"):
            documents[path.name] = path.read_bytes()
        else:
            raise AssertionError(f"unreviewed live-evidence file: {path.name}")
    result: dict[str, dict[str, object]] = {}
    for name, raw in documents.items():
        value = release.loads_strict(raw)
        if raw != release.canonical_file_bytes(value):
            raise AssertionError(f"live evidence is not canonical: {name}")
        authority = value.get("authority") if type(value) is dict else None
        if authority == "production-phase-apply-receipt":
            release.validate_apply_receipt(value)
        elif authority == "production-phase-state":
            release.validate_phase_state(value)
        else:
            raise AssertionError(f"live evidence authority differs: {name}")
        reviewed = EVIDENCE_AUTHORITIES[authority]
        control = value["control"]
        stem = f"{reviewed['prefix']}-{control['run_id']}-{control['run_attempt']}"
        if name != stem + ".json" or control["workflow_path"] != reviewed["workflow_path"]:
            raise AssertionError(f"live evidence name or producer differs: {name}")
        digest = hashlib.sha256(raw).hexdigest()
        if sidecars.pop(stem + ".sha256", None) != (digest + "\n").encode("ascii"):
            raise AssertionError(f"live evidence sidecar differs: {name}")
        statements: dict[str, dict[str, object]] = {}
        for bundle_name in sorted(item for item in bundles if item.startswith(stem + ".")):
            statement = attestation_statement(bundles.pop(bundle_name), subject=reviewed["subject"],
                                              subject_sha256=digest, control=control)
            if statement["predicateType"] in statements:
                raise AssertionError(f"live evidence attestation is duplicated: {bundle_name}")
            statements[statement["predicateType"]] = statement
        if set(statements) != {SLSA_PREDICATE, reviewed["predicate_type"]}:
            if set(statements) - {SLSA_PREDICATE, reviewed["predicate_type"]}:
                raise AssertionError(f"attestation predicate type differs: {name}")
            raise AssertionError(f"live evidence attestation bundles differ: {name}")
        custom = statements[reviewed["predicate_type"]]["predicate"]
        if custom != value or release.canonical_file_bytes(custom) != raw:
            raise AssertionError(f"attested predicate differs from the committed evidence: {name}")
        provenance = statements[SLSA_PREDICATE]["predicate"]
        definition, run = provenance["buildDefinition"], provenance["runDetails"]
        if (definition["externalParameters"]["workflow"] != {
                "ref": "refs/heads/main", "repository": REPOSITORY_URL, "path": reviewed["workflow_path"]}
                or definition["resolvedDependencies"][0]["digest"] != {"gitCommit": control["workflow_sha"]}
                or definition["internalParameters"]["github"]["event_name"] != "workflow_dispatch"
                or definition["internalParameters"]["github"]["runner_environment"] != "github-hosted"
                or run["builder"]["id"] != f"{REPOSITORY_URL}/{reviewed['workflow_path']}@refs/heads/main"
                or run["metadata"]["invocationId"]
                != f"{REPOSITORY_URL}/actions/runs/{control['run_id']}/attempts/{control['run_attempt']}"):
            raise AssertionError(f"attested provenance differs: {name}")
        result[name] = {
            "raw": raw, "value": value, "sha256": digest, "authority": authority,
            "phase": value["lineage"]["phase"], "statements": statements,
        }
    if sidecars or bundles:
        raise AssertionError(f"orphaned live-evidence files: {sorted(sidecars) + sorted(bundles)}")
    return result


def require_live_phase_floor(documents: dict[str, dict[str, object]]) -> int:
    """The live phase never falls below the highest phase committed evidence records."""
    if not documents:
        raise AssertionError("no committed live evidence")
    floor = max(verifier.PHASES.index(document["phase"]) for document in documents.values())
    if verifier.PHASES.index(verifier.BOOTSTRAP_LIVE_PHASE) < floor:
        raise AssertionError("bootstrap live phase is below the committed live-evidence floor")
    if any(verifier.PHASES.index(phase) < floor for phase in verifier.GENESIS_ENTRY_PHASES):
        raise AssertionError("a digest genesis entry is below the committed live-evidence floor")
    return floor


def bind_live_evidence(evidence: dict[str, object], bootstrap: dict[str, object],
                       documents: dict[str, dict[str, object]]) -> dict[str, object]:
    """Bind the contract's static live evidence to committed bytes, per kind."""
    import verify_production_release as release

    phase = bootstrap["live_phase"]
    verifier.validate_live_evidence(copy.deepcopy(evidence), phase)
    kind = evidence["kind"]
    if kind == "accepted-unsigned-apply-receipt":
        name = f"production-phase-apply-receipt-{evidence['run_id']}-{evidence['run_attempt']}.json"
        document = documents.get(name)
        if document is None or document["authority"] != "production-phase-apply-receipt":
            raise AssertionError("committed apply receipt is missing")
        if document["sha256"] != evidence["receipt_sha256"]:
            raise AssertionError("committed apply receipt hash differs")
        receipt = document["value"]
        lineage, observed = receipt["lineage"], receipt["after"]
        if lineage["predecessor_state_sha256"] != evidence["receipt_predecessor_state_sha256"]:
            raise AssertionError("committed apply receipt predecessor differs")
    elif kind == "signed-phase-state":
        name = f"production-phase-state-{evidence['run_id']}-{evidence['run_attempt']}.json"
        document = documents.get(name)
        if document is None or document["authority"] != "production-phase-state":
            raise AssertionError("committed phase state is missing")
        if document["sha256"] != evidence["phase_state_sha256"]:
            raise AssertionError("committed phase state hash differs")
        state = release.validate_phase_state(copy.deepcopy(document["value"]))
        lineage, observed = state["lineage"], state["provider_state"]
        if (state["evidence"]["change_receipt_sha256"] != evidence["change_receipt_sha256"]
                or state["evidence"]["canary_sha256"] != evidence["canary_sha256"]
                or lineage["predecessor_state_sha256"] != evidence["change_receipt_sha256"]):
            raise AssertionError("committed phase state evidence differs")
        receipts = [item for item in documents.values()
                    if item["sha256"] == evidence["change_receipt_sha256"]]
        if len(receipts) != 1:
            raise AssertionError("committed change receipt is missing")
        change = receipts[0]["value"]
        if (change["lineage"]["predecessor_state_sha256"] != evidence["receipt_predecessor_state_sha256"]
                or change["after"] != observed):
            raise AssertionError("committed change receipt differs from the phase state")
    else:
        raise AssertionError("live evidence kind differs")
    control = document["value"]["control"]
    if (control["workflow_sha"], control["workflow_path"], control["run_id"], control["run_attempt"]) != (
            evidence["control_sha"], evidence["workflow_path"], evidence["run_id"], evidence["run_attempt"]):
        raise AssertionError("committed live evidence control differs")
    if (lineage["phase"], lineage["to"]) != (phase, phase) or evidence["phase"] != phase:
        raise AssertionError("committed live evidence phase differs")
    if lineage["phase_source_sha"] != evidence["phase_source_sha"]:
        raise AssertionError("committed live evidence source differs")
    if observed["active_deployment_identity_sha256"] != bootstrap["active_deployment_id_sha256"] or any(
            observed[key] != bootstrap[key] for key in (
                "canonical_spec_sha256", "environment_values_sha256",
                "non_source_projection_sha256", "source_mode", "images")):
        raise AssertionError("committed live evidence provider state differs from the bootstrap")
    if document["value"]["rollback"] != verifier.ROLLBACK_FLOORS[phase]:
        raise AssertionError("committed live evidence rollback floor differs")
    return document


class GenesisReentryTests(unittest.TestCase):
    def plan(self, base, *, state=None, normalized=None, policy=None, rollout=None):
        return verifier.validate_rollout_plan(
            rollout or base.rollout,
            base.contract,
            normalized or base.normalized,
            policy=policy or base.policy,
            policy_sha256=base.policy_hash,
            schema_sha256=base.schema_hash,
            predecessor_state=state,
        )

    def test_genesis_enters_exactly_at_the_live_phase(self) -> None:
        # A digest bootstrap enters at its live phase (ui); only the legacy-git
        # bootstrap, which predates every phase, enters at baseline.
        self.assertEqual(verifier.GENESIS_ENTRY_PHASES, ("ui",))
        for live, base in (("ui", reentry_fixture(self, "ui")), ("baseline", legacy_fixture(self))):
            with self.subTest(live=live, mode=base.contract["bootstrap_state"]["source_mode"]):
                self.assertEqual(verifier.genesis_target_phase(base.contract), live)
                target, _images, transition, predecessor = self.plan(base)
                ordinal = verifier.PHASES.index(live) + 1
                self.assertEqual(target["phase"], live)
                self.assertEqual(transition, {"operation": "activate", "from": "genesis", "to": live, "ordinal": ordinal})
                self.assertEqual(predecessor, {
                    "kind": "genesis", "event_sequence": 0, "phase_ordinal": 0, "phase": "genesis",
                    "state_sha256": verifier.genesis_state_sha256(base.contract),
                    "run_id": None, "run_attempt": None, "artifact_id": None, "artifact_digest": None,
                })
                self.assertEqual(target["rollback"], verifier.ROLLBACK_FLOORS[live])

    def test_a_live_phase_outside_the_reviewed_entries_fails_closed(self) -> None:
        # Baseline is no digest-mode entry: production has run ui.
        for live in ("baseline", "bridge", "backend", "genesis", None, 4):
            with self.subTest(live=live):
                base = reentry_fixture(self, live)
                with self.assertRaisesRegex(verifier.PlanError, "bootstrap live phase is not a reviewed genesis entry"):
                    self.plan(base)
        base = reentry_fixture(self, "ui")
        del base.contract["bootstrap_state"]["live_phase"]
        with self.assertRaisesRegex(verifier.PlanError, "bootstrap live state is incomplete"):
            self.plan(base)
        legacy = legacy_fixture(self)
        legacy.contract["bootstrap_state"]["live_phase"] = "baseline"
        with self.assertRaisesRegex(verifier.PlanError, "bootstrap source mode differs"):
            verifier.genesis_target_phase(legacy.contract)

    def test_a_digest_bootstrap_never_re_enters_at_baseline(self) -> None:
        # Even a PR that moved BOOTSTRAP_LIVE_PHASE together with the contract
        # cannot re-enter a digest bootstrap below ui without also widening
        # GENESIS_ENTRY_PHASES, which the committed live-evidence floor forbids.
        import apply_production_change as apply

        contract = verifier.load_json(CONTRACT_PATH, "contract")
        for lower in ("baseline", "bridge", "backend"):
            with self.subTest(live=lower):
                tampered = copy.deepcopy(contract)
                bootstrap = tampered["bootstrap_state"]
                bootstrap["live_phase"] = lower
                bootstrap["live_evidence"]["phase"] = lower
                bootstrap["genesis_state_sha256"] = verifier.genesis_state_sha256(tampered)
                with mock.patch.object(verifier, "BOOTSTRAP_LIVE_PHASE", lower), \
                        mock.patch.object(verifier, "BOOTSTRAP_LIVE_EVIDENCE", bootstrap["live_evidence"]):
                    with self.assertRaisesRegex(verifier.PlanError, "bootstrap live phase differs"):
                        verifier.validate_contract(copy.deepcopy(tampered))
                    with self.assertRaisesRegex(verifier.PlanError, "bootstrap live phase is not a reviewed genesis entry"):
                        verifier.genesis_target_phase(tampered)
                with self.assertRaisesRegex(apply.common.ReleaseError, "live phase"):
                    apply.contract_genesis_phase(tampered)
        with mock.patch.object(verifier, "GENESIS_ENTRY_PHASES", ("baseline", "ui")):
            with self.assertRaisesRegex(AssertionError, "below the committed live-evidence floor"):
                require_live_phase_floor(classify_live_evidence(LIVE_EVIDENCE_DIR))

    def test_plan_transition_must_be_a_reviewed_policy_edge(self) -> None:
        base = reentry_fixture(self, "ui")
        policy = copy.deepcopy(base.policy)
        policy["activation_transitions"] = [
            edge for edge in policy["activation_transitions"]
            if edge != {"from": "genesis", "to": "ui", "ordinal": 4}
        ]
        with self.assertRaisesRegex(verifier.PlanError, "production activation transition differs"):
            self.plan(base, policy=policy)

    def test_genesis_reentry_source_is_pinned_to_the_reviewed_ui_source(self) -> None:
        manifest = json.loads(SOURCE_MANIFEST_PATH.read_text(encoding="utf-8"))
        self.assertEqual(manifest["phases"]["ui"]["source_sha"], verifier.UI_TARGET_SOURCE_SHA)
        base = reentry_fixture(self, "ui")
        rollout = copy.deepcopy(base.rollout)
        rollout["phases"][3]["source"]["commit"] = "9" * 40
        with self.assertRaisesRegex(verifier.PlanError, "genesis rollout source differs from the reviewed target"):
            self.plan(base, rollout=rollout)

    def test_live_ui_rejects_every_predecessor_below_the_live_phase(self) -> None:
        base = reentry_fixture(self, "ui")
        states = [
            base.phase_state("baseline"),
            base.phase_state("bridge"),
            base.phase_state("backend"),
            base.phase_state("backend", event_sequence=7, operation="rollback", source_phase="ui", predecessor_kind="rollback-receipt"),
            base.phase_state("bridge", event_sequence=7, operation="rollback", source_phase="ui", predecessor_kind="rollback-receipt"),
        ]
        for state in states:
            with self.subTest(phase=state["lineage"]["phase"], operation=state["lineage"]["operation"]):
                normalized, _ = base.input_for_state(state)
                with self.assertRaisesRegex(verifier.PlanError, "phase state precedes the reviewed live phase"):
                    self.plan(base, state=state, normalized=normalized)

    def test_signed_reentry_state_is_valid_and_terminal(self) -> None:
        base = reentry_fixture(self, "ui")
        state = base.phase_state("ui", event_sequence=1, source_phase="genesis")
        normalized, _ = base.input_for_state(state)
        verifier.validate_phase_state(state, base.contract, base.policy, normalized, base.rollout, base.policy_hash, base.schema_hash)
        with self.assertRaisesRegex(verifier.PlanError, "the signed UI phase is terminal"):
            self.plan(base, state=state, normalized=normalized)
        for label, kwargs, message in (
            ("genesis at sequence 2", {"event_sequence": 2, "source_phase": "genesis"}, "non-initial phase state cannot start from genesis"),
            ("linear backend edge", {"event_sequence": 4, "source_phase": "backend"}, "phase-state activation edge differs"),
            ("backend at sequence 1", {"event_sequence": 1, "source_phase": "backend"}, "phase-state activation edge differs"),
        ):
            with self.subTest(label=label):
                mutated = base.phase_state("ui", **kwargs)
                normalized, _ = base.input_for_state(mutated)
                with self.assertRaisesRegex(verifier.PlanError, message):
                    verifier.validate_phase_state(mutated, base.contract, base.policy, normalized, base.rollout, base.policy_hash, base.schema_hash)

    def test_legacy_entry_still_rejects_a_genesis_ui_state(self) -> None:
        base = legacy_fixture(self)
        state = base.phase_state("ui", event_sequence=1, source_phase="genesis")
        normalized, _ = base.input_for_state(state)
        with self.assertRaisesRegex(verifier.PlanError, "phase-state activation edge differs"):
            verifier.validate_phase_state(state, base.contract, base.policy, normalized, base.rollout, base.policy_hash, base.schema_hash)

    def test_policy_admits_exactly_the_reviewed_genesis_edges(self) -> None:
        policy = json.loads(POLICY_PATH.read_text(encoding="utf-8"))
        verifier.validate_release_policy(policy)
        self.assertEqual(
            [edge for edge in policy["activation_transitions"] if edge["from"] == "genesis"],
            [{"from": "genesis", "to": "baseline", "ordinal": 1}, {"from": "genesis", "to": "ui", "ordinal": 4}],
        )
        self.assertEqual(POLICY_PATH.read_bytes(), verifier.canonical_file_bytes(policy))
        mutations = {
            "genesis->bridge": lambda edges: edges.append({"from": "genesis", "to": "bridge", "ordinal": 2}),
            "genesis->backend": lambda edges: edges.append({"from": "genesis", "to": "backend", "ordinal": 3}),
            "second genesis->baseline": lambda edges: edges.append({"from": "genesis", "to": "baseline", "ordinal": 1}),
            "genesis->ui ordinal 1": lambda edges: edges.__setitem__(4, {"from": "genesis", "to": "ui", "ordinal": 1}),
            "ui->baseline": lambda edges: edges.append({"from": "ui", "to": "baseline", "ordinal": 1}),
            "backend->bridge": lambda edges: edges.append({"from": "backend", "to": "bridge", "ordinal": 2}),
            "missing re-entry": lambda edges: edges.pop(4),
            "missing genesis->baseline": lambda edges: edges.pop(0),
            "reordered": lambda edges: edges.reverse(),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                tampered = copy.deepcopy(policy)
                mutate(tampered["activation_transitions"])
                with self.assertRaisesRegex(verifier.PlanError, "activation transitions differ"):
                    verifier.validate_release_policy(tampered)

    def test_digest_genesis_hash_binds_images_live_phase_and_evidence(self) -> None:
        contract = verifier.load_json(CONTRACT_PATH, "contract")
        original = verifier.genesis_state_sha256(contract)
        bootstrap = contract["bootstrap_state"]
        for key, value in (
            ("live_phase", "baseline"),
            ("live_evidence", {**bootstrap["live_evidence"], "run_id": "1"}),
            ("images", list(reversed(bootstrap["images"]))),
        ):
            with self.subTest(key=key):
                tampered = copy.deepcopy(contract)
                tampered["bootstrap_state"][key] = value
                self.assertNotEqual(verifier.genesis_state_sha256(tampered), original)
        missing = copy.deepcopy(contract)
        del missing["bootstrap_state"]["live_evidence"]
        with self.assertRaisesRegex(verifier.PlanError, "bootstrap live state is incomplete"):
            verifier.genesis_state_sha256(missing)
        legacy = copy.deepcopy(contract)
        legacy_bootstrap = legacy["bootstrap_state"]
        legacy_bootstrap["source_mode"] = "legacy-git"
        for key in ("images", "live_phase", "live_evidence"):
            legacy_bootstrap.pop(key)
        self.assertEqual(verifier.genesis_state_sha256(legacy), verifier.sha256_value({
            "kind": "genesis", "event_sequence": 0, "phase_ordinal": 0, "phase": "genesis",
            "app_identity_sha256": legacy["provider"]["app_id_sha256"],
            "default_ingress_sha256": legacy["provider"]["default_ingress_sha256"],
            "active_deployment_identity_sha256": legacy_bootstrap["active_deployment_id_sha256"],
            "canonical_spec_sha256": legacy_bootstrap["canonical_spec_sha256"],
            "environment_values_sha256": legacy_bootstrap["environment_values_sha256"],
            "non_source_projection_sha256": legacy_bootstrap["non_source_projection_sha256"],
            "source_mode": "legacy-git", "source_sha": legacy_bootstrap["source_sha"],
        }))
        self.assertEqual(verifier.genesis_target_phase(legacy), "baseline")

    def test_bootstrap_live_phase_and_evidence_are_pinned(self) -> None:
        contract = verifier.load_json(CONTRACT_PATH, "contract")
        verifier.validate_contract(copy.deepcopy(contract))

        def reseal(value):
            value["bootstrap_state"]["genesis_state_sha256"] = verifier.genesis_state_sha256(value)
            return value

        for phase in ("baseline", "bridge", "backend", "genesis"):
            with self.subTest(live_phase=phase):
                tampered = copy.deepcopy(contract)
                tampered["bootstrap_state"]["live_phase"] = phase
                tampered["bootstrap_state"]["live_evidence"]["phase"] = phase
                with self.assertRaisesRegex(verifier.PlanError, "bootstrap live phase differs"):
                    verifier.validate_contract(reseal(tampered))
        cases = (
            ("receipt hash", lambda e: e.__setitem__("receipt_sha256", "0" * 64), "bootstrap live-state evidence differs"),
            ("evidence phase", lambda e: e.__setitem__("phase", "backend"), "bootstrap live-state evidence phase differs"),
            ("evidence kind", lambda e: e.__setitem__("kind", "owner-smoke-test"), "bootstrap live-state evidence kind differs"),
            ("extra key", lambda e: e.__setitem__("url", "https://example.invalid"), "bootstrap live-state evidence keys differ"),
            ("workflow", lambda e: e.__setitem__("workflow_path", ".github/workflows/deploy-production.yml"), "bootstrap live-state evidence authority differs"),
        )
        for label, mutate, message in cases:
            with self.subTest(case=label):
                tampered = copy.deepcopy(contract)
                mutate(tampered["bootstrap_state"]["live_evidence"])
                with self.assertRaisesRegex(verifier.PlanError, message):
                    verifier.validate_contract(reseal(tampered))
        for key in ("live_phase", "live_evidence"):
            with self.subTest(missing=key):
                tampered = copy.deepcopy(contract)
                del tampered["bootstrap_state"][key]
                with self.assertRaisesRegex(verifier.PlanError, "bootstrap state keys differ"):
                    verifier.validate_contract(tampered)
        stale = copy.deepcopy(contract)
        stale["bootstrap_state"]["genesis_state_sha256"] = "0" * 64
        with self.assertRaisesRegex(verifier.PlanError, "bootstrap genesis state hash differs"):
            verifier.validate_contract(stale)

    def test_live_evidence_shapes_are_exact(self) -> None:
        verifier.validate_live_evidence(copy.deepcopy(verifier.BOOTSTRAP_LIVE_EVIDENCE), "ui")
        signed = {
            "kind": "signed-phase-state", "phase": "ui",
            "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
            "control_sha": "a" * 40, "run_id": "123", "run_attempt": 1, "artifact_id": "456",
            "artifact_name": "production-phase-state-123-1", "artifact_digest": "sha256:" + "b" * 64,
            "predicate_type": "https://rereply.app/attestations/production-phase-state/v1",
            "phase_state_sha256": "c" * 64, "phase_source_sha": "d" * 40, "change_receipt_sha256": "e" * 64,
            "receipt_predecessor_state_sha256": "1" * 64, "canary_sha256": "2" * 64,
        }
        verifier.validate_live_evidence(copy.deepcopy(signed), "ui")
        self.assertEqual(set(verifier.LIVE_EVIDENCE_SHAPES["signed-phase-state"]["keys"]), set(signed))
        for label, key, value in (
            ("artifact name", "artifact_name", "production-phase-state-999-1"),
            ("attempt", "run_attempt", 2),
            ("predicate", "predicate_type", "https://slsa.dev/provenance/v1"),
            ("hash", "phase_state_sha256", "z" * 64),
            ("receipt predecessor", "receipt_predecessor_state_sha256", "z" * 64),
            ("canary", "canary_sha256", "z" * 64),
            ("receipt kind key", "receipt_sha256", "c" * 64),
        ):
            with self.subTest(case=label):
                tampered = {**copy.deepcopy(signed), key: value}
                with self.assertRaises(verifier.PlanError):
                    verifier.validate_live_evidence(tampered, "ui")
        for missing in ("receipt_predecessor_state_sha256", "canary_sha256"):
            with self.subTest(missing=missing), self.assertRaisesRegex(
                    verifier.PlanError, "bootstrap live-state evidence keys differ"):
                verifier.validate_live_evidence({k: v for k, v in signed.items() if k != missing}, "ui")
        with self.assertRaises(verifier.PlanError):
            verifier.validate_live_evidence(copy.deepcopy(signed), "backend")

    def test_live_evidence_matches_the_committed_attested_receipt(self) -> None:
        documents = classify_live_evidence(LIVE_EVIDENCE_DIR)
        bootstrap = verifier.load_json(CONTRACT_PATH, "contract")["bootstrap_state"]
        self.assertEqual(bootstrap["live_evidence"], verifier.BOOTSTRAP_LIVE_EVIDENCE)
        document = bind_live_evidence(verifier.BOOTSTRAP_LIVE_EVIDENCE, bootstrap, documents)
        evidence = verifier.BOOTSTRAP_LIVE_EVIDENCE
        self.assertEqual(document["sha256"], evidence["receipt_sha256"])
        self.assertEqual(document["value"]["lineage"]["phase"], verifier.BOOTSTRAP_LIVE_PHASE)
        for key, mutation, message in (
            ("receipt_sha256", "0" * 64, "committed apply receipt hash differs"),
            ("receipt_predecessor_state_sha256", "0" * 64, "committed apply receipt predecessor differs"),
            ("phase_source_sha", "0" * 40, "committed live evidence source differs"),
            ("control_sha", "0" * 40, "committed live evidence control differs"),
            ("run_id", "36773451427", "committed apply receipt is missing"),
        ):
            with self.subTest(evidence=key):
                tampered = {**copy.deepcopy(evidence), key: mutation}
                if key == "run_id":
                    tampered["artifact_name"] = f"production-phase-apply-{mutation}-1"
                with self.assertRaisesRegex(AssertionError, message):
                    bind_live_evidence(tampered, bootstrap, documents)
        for key in ("active_deployment_id_sha256", "canonical_spec_sha256", "images"):
            with self.subTest(bootstrap=key):
                tampered = copy.deepcopy(bootstrap)
                tampered[key] = "0" * 64 if key != "images" else list(reversed(tampered["images"]))
                with self.assertRaisesRegex(AssertionError, "provider state differs from the bootstrap"):
                    bind_live_evidence(evidence, tampered, documents)

    def test_attestation_bundles_bind_the_committed_receipt(self) -> None:
        import verify_production_release as release

        evidence = verifier.BOOTSTRAP_LIVE_EVIDENCE
        stem = f"production-phase-apply-receipt-{evidence['run_id']}-{evidence['run_attempt']}"
        documents = classify_live_evidence(LIVE_EVIDENCE_DIR)
        self.assertEqual(sorted(path.name for path in LIVE_EVIDENCE_DIR.iterdir()), [
            stem + ".json",
            stem + ".predicate-receipt-v1.sigstore.json",
            stem + ".predicate-slsa-provenance-v1.sigstore.json",
            stem + ".sha256",
        ])
        document = documents[stem + ".json"]
        self.assertEqual(set(document["statements"]), {SLSA_PREDICATE, evidence["predicate_type"]})
        for statement in document["statements"].values():
            self.assertEqual(statement["subject"], [{
                "name": "production-phase-apply-receipt.json",
                "digest": {"sha256": evidence["receipt_sha256"]},
            }])
        custom = document["statements"][evidence["predicate_type"]]["predicate"]
        self.assertEqual(release.canonical_file_bytes(custom), document["raw"])

        def tamper(*edits):
            with tempfile.TemporaryDirectory(prefix="live-evidence-") as temporary:
                directory = Path(temporary)
                for path in LIVE_EVIDENCE_DIR.iterdir():
                    (directory / path.name).write_bytes(path.read_bytes())
                for name, edit in edits:
                    target = directory / name
                    if edit is None:
                        target.unlink()
                    elif name.endswith(".sigstore.json"):
                        bundle = json.loads(target.read_bytes())
                        edit(bundle)
                        target.write_bytes(json.dumps(bundle).encode("utf-8"))
                    else:
                        target.write_bytes(edit(target.read_bytes()))
                classify_live_evidence(directory)

        def rekor(bundle, change):
            entry = bundle["verificationMaterial"]["tlogEntries"][0]
            body = json.loads(base64.b64decode(entry["canonicalizedBody"]))
            change(body)
            entry["canonicalizedBody"] = base64.b64encode(json.dumps(body).encode("utf-8")).decode("ascii")

        def restate(change, *, consistent_log=False):
            def edit(bundle):
                envelope = bundle["dsseEnvelope"]
                statement = json.loads(base64.b64decode(envelope["payload"]))
                change(statement)
                payload = json.dumps(statement).encode("utf-8")
                envelope["payload"] = base64.b64encode(payload).decode("ascii")
                if consistent_log:
                    # Isolate the predicate binding from the log-entry check.
                    rekor(bundle, lambda body: body["spec"]["payloadHash"].update(
                        value=hashlib.sha256(payload).hexdigest()))
            return edit

        def recanonicalized_receipt(raw):
            value = json.loads(raw)
            value["completed_at"] = "2026-09-30T20:39:18Z"
            return release.canonical_file_bytes(value)

        receipt_raw = (LIVE_EVIDENCE_DIR / (stem + ".json")).read_bytes()
        self.assertNotEqual(recanonicalized_receipt(receipt_raw), receipt_raw)
        receipt_bundle = stem + ".predicate-receipt-v1.sigstore.json"
        slsa_bundle = stem + ".predicate-slsa-provenance-v1.sigstore.json"
        cases = (
            ("subject digest", [(receipt_bundle,
             restate(lambda s: s["subject"][0]["digest"].update(sha256="0" * 64)))], "attestation subject differs"),
            ("subject name", [(slsa_bundle,
             restate(lambda s: s["subject"][0].update(name="other.json")))], "attestation subject differs"),
            ("predicate content without a log entry", [(receipt_bundle,
             restate(lambda s: s["predicate"]["lineage"].update(phase="backend")))],
             "transparency-log entry differs"),
            ("predicate content", [(receipt_bundle, restate(
             lambda s: s["predicate"].update(completed_at="2026-09-30T20:39:18Z"), consistent_log=True))],
             "attested predicate differs from the committed evidence"),
            ("rekor payload hash", [(slsa_bundle, lambda b: rekor(
             b, lambda body: body["spec"]["payloadHash"].update(value="0" * 64)))],
             "transparency-log entry differs"),
            ("rekor signature", [(slsa_bundle, lambda b: rekor(
             b, lambda body: body["spec"]["signatures"][0].update(signature="AAAA")))],
             "transparency-log entry differs"),
            ("missing bundle", [(slsa_bundle, None)], "attestation bundles differ"),
            ("media type", [(receipt_bundle, lambda b: b.update(mediaType="application/json"))], "media type differs"),
            ("sidecar", [(stem + ".sha256", lambda raw: ("0" * 64 + "\n").encode("ascii"))], "sidecar differs"),
            ("receipt not canonical", [(stem + ".json", lambda raw: raw[:-1] + b" \n")], "not canonical"),
            ("receipt changed with a matching sidecar", [
                (stem + ".json", recanonicalized_receipt),
                (stem + ".sha256", lambda raw: (hashlib.sha256(recanonicalized_receipt(receipt_raw)).hexdigest()
                                                 + "\n").encode("ascii")),
             ], "attestation subject differs"),
        )
        for label, edits, message in cases:
            with self.subTest(case=label), self.assertRaisesRegex(AssertionError, message):
                tamper(*edits)
        with tempfile.TemporaryDirectory(prefix="live-evidence-") as temporary:
            directory = Path(temporary)
            for path in LIVE_EVIDENCE_DIR.iterdir():
                (directory / path.name).write_bytes(path.read_bytes())
            (directory / "notes.md").write_text("unreviewed\n", encoding="utf-8")
            with self.assertRaisesRegex(AssertionError, "unreviewed live-evidence file"):
                classify_live_evidence(directory)
            (directory / "notes.md").unlink()
            (directory / (stem + ".predicate-extra.sigstore.json")).write_bytes(
                (LIVE_EVIDENCE_DIR / slsa_bundle).read_bytes())
            with self.assertRaisesRegex(AssertionError, "duplicated"):
                classify_live_evidence(directory)

    def test_committed_live_evidence_sets_a_monotonic_live_phase_floor(self) -> None:
        documents = classify_live_evidence(LIVE_EVIDENCE_DIR)
        floor = require_live_phase_floor(documents)
        self.assertEqual(verifier.PHASES[floor], "ui")
        self.assertGreaterEqual(verifier.PHASES.index(verifier.BOOTSTRAP_LIVE_PHASE), floor)
        for lower in verifier.PHASES[:floor]:
            with self.subTest(live_phase=lower), mock.patch.object(verifier, "BOOTSTRAP_LIVE_PHASE", lower):
                with self.assertRaisesRegex(AssertionError, "below the committed live-evidence floor"):
                    require_live_phase_floor(documents)

    def test_signed_phase_state_evidence_binds_committed_state_bytes(self) -> None:
        # The next rebaseline (onto the signed ui' state) is data-only; this is
        # the same binder exercised on a synthetic signed re-entry.
        import test_verify_production_release as release_fixtures
        import verify_production_release as release

        receipt = release_fixtures.reentry_receipt()
        receipt_raw = release.canonical_file_bytes(receipt)
        receipt_hash = hashlib.sha256(receipt_raw).hexdigest()
        state = release.build_phase_state(
            receipt, change_receipt_sha256=receipt_hash, canary_sha256="9" * 64,
            control=release_fixtures.STATE_CONTROL, completed_at="2026-08-27T00:01:00Z",
        )
        state_raw = release.canonical_file_bytes(state)
        control = state["control"]
        documents = {
            f"production-phase-apply-receipt-{receipt['control']['run_id']}-1.json": {
                "raw": receipt_raw, "value": receipt, "sha256": receipt_hash,
                "authority": "production-phase-apply-receipt", "phase": "ui", "statements": {},
            },
            f"production-phase-state-{control['run_id']}-1.json": {
                "raw": state_raw, "value": state, "sha256": hashlib.sha256(state_raw).hexdigest(),
                "authority": "production-phase-state", "phase": "ui", "statements": {},
            },
        }
        evidence = {
            "kind": "signed-phase-state", "phase": "ui",
            "workflow_path": control["workflow_path"], "control_sha": control["workflow_sha"],
            "run_id": control["run_id"], "run_attempt": 1, "artifact_id": "402",
            "artifact_name": f"production-phase-state-{control['run_id']}-1",
            "artifact_digest": "sha256:" + "4" * 64,
            "predicate_type": "https://rereply.app/attestations/production-phase-state/v1",
            "phase_state_sha256": hashlib.sha256(state_raw).hexdigest(),
            "phase_source_sha": state["lineage"]["phase_source_sha"],
            "change_receipt_sha256": receipt_hash,
            "receipt_predecessor_state_sha256": receipt["lineage"]["predecessor_state_sha256"],
            "canary_sha256": "9" * 64,
        }
        provider = state["provider_state"]
        bootstrap = {
            "active_deployment_id_sha256": provider["active_deployment_identity_sha256"],
            **{key: copy.deepcopy(provider[key]) for key in (
                "canonical_spec_sha256", "environment_values_sha256",
                "non_source_projection_sha256", "source_mode", "images")},
            "live_phase": "ui",
        }
        self.assertIs(bind_live_evidence(evidence, bootstrap, documents)["value"], state)
        for key, value, message in (
            ("phase_state_sha256", "0" * 64, "committed phase state hash differs"),
            ("canary_sha256", "0" * 64, "committed phase state evidence differs"),
            ("change_receipt_sha256", "0" * 64, "committed phase state evidence differs"),
            ("receipt_predecessor_state_sha256", "0" * 64, "committed change receipt differs"),
            ("phase_source_sha", "0" * 40, "committed live evidence source differs"),
        ):
            with self.subTest(evidence=key), self.assertRaisesRegex(AssertionError, message):
                bind_live_evidence({**evidence, key: value}, bootstrap, documents)
        with self.assertRaisesRegex(AssertionError, "committed change receipt is missing"):
            bind_live_evidence(evidence, bootstrap, {
                name: item for name, item in documents.items() if item["authority"] != "production-phase-apply-receipt"})
        with self.assertRaisesRegex(AssertionError, "provider state differs from the bootstrap"):
            bind_live_evidence(evidence, {**bootstrap, "active_deployment_id_sha256": "0" * 64}, documents)
        with self.assertRaisesRegex(verifier.PlanError, "phase differs"):
            bind_live_evidence(evidence, {**bootstrap, "live_phase": "backend"}, documents)

    def test_runtime_controls_never_read_the_committed_live_evidence(self) -> None:
        runtime = [path for path in (ROOT / "release").rglob("*.py") if not path.name.startswith("test_")]
        runtime += sorted((ROOT / ".github" / "workflows").glob("*.yml"))
        for path in runtime:
            with self.subTest(path=path.name):
                self.assertNotIn("live-evidence", path.read_text(encoding="utf-8"))
        with mock.patch.object(verifier.urllib.request, "build_opener", side_effect=AssertionError("network forbidden")), \
                mock.patch.object(verifier.urllib.request, "urlopen", side_effect=AssertionError("network forbidden")):
            verifier.validate_contract(verifier.load_json(CONTRACT_PATH, "contract"))

    def test_committed_live_evidence_is_never_line_ending_converted(self) -> None:
        attributes = (ROOT / ".gitattributes").read_text(encoding="utf-8").splitlines()
        self.assertIn("release/deployment/live-evidence/** -text", attributes)
        for path in LIVE_EVIDENCE_DIR.iterdir():
            with self.subTest(path=path.name):
                self.assertNotIn(b"\r", path.read_bytes())

    def test_runbook_records_the_reentry_rules(self) -> None:
        runbook = (ROOT / "docs" / "crm-production-release-control.md").read_text(encoding="utf-8")
        section = runbook.split("## Genesis re-entry at the accepted live phase (2026-10-01)\n", 1)[1]
        section = section.split("\n## ", 1)[0]
        evidence = verifier.BOOTSTRAP_LIVE_EVIDENCE
        for required in (
            "### Required pre-merge verification (recorded)",
            "`gh attestation verify`",
            evidence["predicate_type"],
            "https://slsa.dev/provenance/v1",
            f"Run {evidence['run_id']} is a",
            "The owner accepted the unsigned c4cdac90 ui on\n2026-09-30 after a manual smoke test.",
            "### Live-phase floor induction",
            "**no governed rollback after ui'**",
            "every future release must rebaseline again",
            "Every live-evidence file stays committed",
            "`do not relaunch; triage it (runbook F14)`",
            "`main-branch-locked-by-an-earlier-apply`",
            "Never relaunch for F4",
            "Data-only rebaseline checklist:",
            "LiveFloorInductionTests",
        ):
            self.assertIn(required, section)
        for incident in range(1, 15):
            self.assertIn(f"- **F{incident}. ", section)
        genesis = verifier.genesis_state_sha256(verifier.load_json(CONTRACT_PATH, "contract"))
        self.assertIn(f"`{genesis[:8]}`", section)
        for value in (evidence["receipt_sha256"], evidence["receipt_predecessor_state_sha256"],
                      evidence["phase_source_sha"], verifier.UI_TARGET_SOURCE_SHA):
            self.assertIn(f"`{value[:8]}`", section)

    def test_rollback_floors_only_point_below_their_phase(self) -> None:
        for phase, floor in verifier.ROLLBACK_FLOORS.items():
            below = set(verifier.PHASES[: verifier.PHASES.index(phase)])
            self.assertLessEqual(set(floor["allowed_targets"]), below)
            self.assertLessEqual(set(floor["forbidden_targets"]), below)
        for live in (verifier.PHASES[0], *verifier.GENESIS_ENTRY_PHASES):
            self.assertNotIn(live, verifier.ROLLBACK_FLOORS[live]["forbidden_targets"])


class ReviewedProductionContractTests(unittest.TestCase):
    def test_digest_bootstrap_authority_is_pinned_and_enforced(self) -> None:
        contract = verifier.validate_contract(
            verifier.load_json(CONTRACT_PATH, "contract"),
            verifier.load_json(POLICY_PATH, "policy"),
            verifier.load_json(SCHEMA_PATH, "schema"),
        )
        expected_state, images = verifier.predecessor_provider_expectation(
            contract, rollout_plan(), None
        )
        self.assertEqual(expected_state["source_mode"], "digest-images")
        self.assertEqual(set(images), {"web", "meta-relay", "gmail-relay"})
        for record in verifier.BOOTSTRAP_IMAGES:
            self.assertEqual(images[record["component"]]["digest"], record["digest"])
            self.assertEqual(
                images[record["component"]]["repository"],
                record["repository"].removeprefix("ghcr.io/"),
            )
            self.assertEqual(
                images[record["component"]]["subject"], record["subject"]
            )

        spec = digest_source_spec()
        verifier.validate_digest_component_sources(spec, contract, images)
        tampered = copy.deepcopy(spec)
        web = next(item for item in tampered["services"] if item["name"] == "omnitech-web")
        web["image"]["digest"] = digest("tampered")
        with self.assertRaises(verifier.PlanError):
            verifier.validate_digest_component_sources(tampered, contract, images)

    def test_bootstrap_image_authority_is_pinned_to_the_reviewed_constant(self) -> None:
        contract = verifier.load_json(CONTRACT_PATH, "contract")
        contract["bootstrap_state"]["images"] = copy.deepcopy(
            verifier.BOOTSTRAP_IMAGES
        )
        contract["bootstrap_state"]["images"][0]["digest"] = digest("tampered")
        with self.assertRaisesRegex(
            verifier.PlanError, "bootstrap image authority differs"
        ):
            verifier.validate_contract(contract)

    def test_reviewed_contract_is_valid_against_unpatched_constants(self) -> None:
        production_contract = verifier.load_json(CONTRACT_PATH, "contract")
        verifier.validate_contract(production_contract)
        self.assertNotIn(
            "app_updated_at_sha256", production_contract["bootstrap_state"]
        )
        self.assertEqual(
            production_contract["bootstrap_state"]["genesis_state_sha256"],
            GENESIS_PIN,
        )
        self.assertEqual(
            verifier.genesis_state_sha256(production_contract),
            production_contract["bootstrap_state"]["genesis_state_sha256"],
        )
        # 2026-09-18: production was re-baselined onto the already-applied
        # baseline phase, so the bootstrap is pinned to image authority rather
        # than to the retired legacy git sources.
        bootstrap = production_contract["bootstrap_state"]
        self.assertEqual(bootstrap["source_mode"], "digest-images")
        self.assertEqual(bootstrap["source_mode"], verifier.BOOTSTRAP_SOURCE_MODE)
        self.assertEqual(
            bootstrap["source_sha"], "4f65abeb1c03c8fca018aa92cc987fae25ef4000"
        )
        self.assertEqual(bootstrap["source_sha"], verifier.BOOTSTRAP_SOURCE_SHA)
        self.assertNotEqual(bootstrap["source_sha"], verifier.BASELINE_TARGET_SOURCE_SHA)
        # 2026-10-01: re-entered at the accepted live ui phase.
        self.assertEqual(bootstrap["live_phase"], "ui")
        self.assertEqual(bootstrap["live_phase"], verifier.BOOTSTRAP_LIVE_PHASE)
        self.assertEqual(bootstrap["live_evidence"], verifier.BOOTSTRAP_LIVE_EVIDENCE)
        self.assertEqual(verifier.genesis_target_phase(production_contract), "ui")
        self.assertEqual(bootstrap["images"], verifier.BOOTSTRAP_IMAGES)
        self.assertEqual(
            [record["component"] for record in bootstrap["images"]],
            ["web", "meta-relay", "gmail-relay"],
        )
        self.assertEqual(
            bootstrap["active_deployment_id_sha256"],
            verifier.BOOTSTRAP_DEPLOYMENT_ID_SHA256,
        )
        # The reviewed contract is canonical on disk so that the strict
        # reconciliation loader accepts the same bytes the plan hashes.
        self.assertEqual(
            CONTRACT_PATH.read_bytes(),
            verifier.canonical_file_bytes(production_contract),
        )


if __name__ == "__main__":
    unittest.main()
