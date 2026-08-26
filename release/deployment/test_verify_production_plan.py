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
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_plan as verifier


ROOT = Path(__file__).resolve().parents[2]
CONTRACT_PATH = ROOT / "release" / "deployment" / "production-app-contract.json"
VERIFIER_PATH = ROOT / "release" / "deployment" / "verify_production_plan.py"
WORKFLOW_PATH = ROOT / ".github" / "workflows" / "plan-production-rollout.yml"
TEST_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "test.yml"
CONTROL_SHA = "f" * 40
NOW = dt.datetime(2026, 8, 26, 12, 0, 0, tzinfo=dt.timezone.utc)
TEST_TARGET = {
    "app_id": "11111111-1111-4111-8111-111111111111",
    "active_deployment_id": "22222222-2222-4222-8222-222222222222",
    "app_updated_at": "2026-08-26T11:11:11Z",
    "default_ingress": "https://private-target.invalid",
}


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
    images = []
    for component in ("web", "meta-relay", "gmail-relay"):
        repository = f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
        images.append(
            {
                "component": component,
                "image": repository,
                "digest": digest(component),
                "tag_is_authority": False,
            }
        )
    baseline = {
        "phase": "baseline",
        "source": {"commit": verifier.BOOTSTRAP_SOURCE_SHA},
        "images": images,
        "migration": {"digest": digest("web")},
        "rollback": {"allowed_targets": [], "forbidden_targets": []},
    }
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
        "phases": [
            baseline,
            {"phase": "bridge"},
            {"phase": "backend"},
            {"phase": "ui"},
        ],
    }


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
        target_hashes = {
            key: verifier.sha256_bytes(value.encode("utf-8"))
            for key, value in self.target.items()
        }
        self.contract["provider"]["app_id_sha256"] = target_hashes["app_id"]
        self.contract["provider"]["default_ingress_sha256"] = target_hashes[
            "default_ingress"
        ]
        self.contract["bootstrap_state"]["active_deployment_id_sha256"] = (
            target_hashes["active_deployment_id"]
        )
        self.contract["bootstrap_state"]["app_updated_at_sha256"] = target_hashes[
            "app_updated_at"
        ]
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
                target_hashes["active_deployment_id"],
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_UPDATED_AT_SHA256",
                target_hashes["app_updated_at"],
            ),
            mock.patch.object(
                verifier, "BOOTSTRAP_NON_SOURCE_SHA256", non_source_hash
            ),
        ]
        for patcher in patches:
            patcher.start()
            self.addCleanup(patcher.stop)
        self.contract = verifier.validate_contract(self.contract)

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
                },
                separators=(",", ":"),
            ),
            CONTROL_SHA,
        )

    def responses(self) -> tuple[dict[str, object], dict[str, object]]:
        app = {
            "app": {
                "id": self.target["app_id"],
                "updated_at": self.target["app_updated_at"],
                "default_ingress": self.target["default_ingress"],
                "spec": copy.deepcopy(self.spec),
                "active_deployment": {
                    "id": self.target["active_deployment_id"],
                    "phase": "ACTIVE",
                },
            }
        }
        deployment = {
            "deployment": {
                "id": self.target["active_deployment_id"],
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
        app_path, deployment_path = verifier.provider_paths(self.contract, self.target)
        return verifier.build_plan(
            contract=self.contract,
            contract_sha256="a" * 64,
            verifier_sha256="b" * 64,
            normalized_input=self.normalized,
            target_descriptor=self.target,
            rollout_plan=self.rollout,
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

    def test_reviewed_contract_is_valid(self) -> None:
        production_contract = verifier.load_json(CONTRACT_PATH, "contract")
        with (
            mock.patch.object(
                verifier,
                "PRODUCTION_APP_ID_SHA256",
                production_contract["provider"]["app_id_sha256"],
            ),
            mock.patch.object(
                verifier,
                "PRODUCTION_DEFAULT_INGRESS_SHA256",
                production_contract["provider"]["default_ingress_sha256"],
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_DEPLOYMENT_ID_SHA256",
                production_contract["bootstrap_state"][
                    "active_deployment_id_sha256"
                ],
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_UPDATED_AT_SHA256",
                production_contract["bootstrap_state"]["app_updated_at_sha256"],
            ),
            mock.patch.object(
                verifier,
                "PRODUCTION_VPC_ID_SHA256",
                "aaaf98cef6beb658509d644dc8c56b559a38f79344739cd0edd1442070ec207e",
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_CANONICAL_SPEC_SHA256",
                "a93a507a5affd82b4e00636812e4c444d31c9027bc31c19bd075f2e4d580b07e",
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_ENVIRONMENT_SHA256",
                "3b675a9ca2279c465a102e8256cbf8242a46c456e6898d593e9cde3d0e2361c7",
            ),
            mock.patch.object(
                verifier,
                "BOOTSTRAP_NON_SOURCE_SHA256",
                "4f31183fb7f305a2a36422fdf722f90bed1eadc69fd90026928f522edf9a419b",
            ),
            mock.patch.object(
                verifier,
                "PRODUCTION_DATABASE_INVENTORY",
                {
                    (
                        "PG",
                        "17",
                        True,
                        "5a08acdc0eed3e363e1aa4e0bba55a523bff63fec1680f3f63b1f46e834289c2",
                        "28248f27ae9c2545b42173bd7f06fffce87e1b22db6d51d1721fb3318a871472",
                    ),
                    (
                        "VALKEY",
                        "8",
                        True,
                        "92acd7905c06aeb9db2bb0b6a71b739bfc6032ca8a2a5fb7473e8afd4a1c87e3",
                        "affc1fc72bb50fa5467e5ba5bd07f5b2bfc7c448bed946c17bbb4078aee1db97",
                    ),
                },
            ),
        ):
            verifier.validate_contract(production_contract)

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
        self.assertFalse(first["provider_validation"]["mutation_performed"])
        self.assertFalse(first["provider_validation"]["deployment_authority"])
        self.assertEqual(first["provider_observation"]["http_request_count"], 4)
        verifier.validate_plan(
            first,
            self.contract,
            "a" * 64,
            "b" * 64,
            self.rollout,
            self.rollout_hash,
            now=NOW,
        )

    def test_protected_target_descriptor_is_hash_bound_and_never_public(self) -> None:
        normalized = verifier.normalize_target_descriptor(
            json.dumps(self.target, separators=(",", ":")), self.contract
        )
        self.assertEqual(normalized, self.target)
        for key in self.target:
            with self.subTest(key=key):
                tampered = dict(self.target)
                if key in {"app_id", "active_deployment_id"}:
                    tampered[key] = "33333333-3333-4333-8333-333333333333"
                elif key == "app_updated_at":
                    tampered[key] = "2026-08-26T11:11:12Z"
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

    def test_live_mutations_fail_closed(self) -> None:
        mutations = {
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
                with self.assertRaises(verifier.PlanError):
                    verifier.provider_state(app, deployment, self.contract, self.target)

    def test_logical_candidate_changes_only_four_source_envelopes(self) -> None:
        _, images = verifier.validate_rollout_plan(
            self.rollout, self.contract, self.normalized
        )
        candidate = verifier.build_logical_candidate(self.spec, self.contract, images)
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
                    verifier.validate_plan(
                        plan,
                        self.contract,
                        "a" * 64,
                        "b" * 64,
                        self.rollout,
                        self.rollout_hash,
                        now=checked_at,
                    )

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
                verifier.load_rollout_plan_for_observation(path, self.contract, bad_input)

    def test_provider_client_uses_only_two_exact_get_paths(self) -> None:
        app, deployment = self.responses()
        app_path, deployment_path = verifier.provider_paths(self.contract, self.target)
        origin = self.contract["provider"]["api_origin"]
        opener = FakeOpener(
            [app, deployment, app, deployment],
            [origin + app_path, origin + deployment_path, origin + app_path, origin + deployment_path],
        )
        with mock.patch.dict(os.environ, {}, clear=True):
            plan = verifier.observe(
                contract=self.contract,
                contract_sha256="a" * 64,
                verifier_sha256="b" * 64,
                normalized_input=self.normalized,
                target_descriptor=self.target,
                rollout_plan=self.rollout,
                rollout_plan_sha256=self.rollout_hash,
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
        app_path, deployment_path = verifier.provider_paths(self.contract, self.target)
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
                    verifier_sha256="b" * 64,
                    normalized_input=self.normalized,
                    target_descriptor=self.target,
                    rollout_plan=self.rollout,
                    rollout_plan_sha256=self.rollout_hash,
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
                client.get_json(verifier.provider_paths(self.contract, self.target)[0])
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


if __name__ == "__main__":
    unittest.main()
