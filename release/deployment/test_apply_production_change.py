from __future__ import annotations

import copy
import datetime as dt
import http.client
import inspect
import json
import subprocess
import sys
import tempfile
import unittest
import urllib.error
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import apply_production_change as apply
import verify_production_release as common
import verify_production_plan as planner


APP_ID = "11111111-1111-4111-8111-111111111111"
OLD_DEPLOYMENT = "22222222-2222-4222-8222-222222222222"
NEW_DEPLOYMENT = "33333333-3333-4333-8333-333333333333"


def digest(character: str) -> str:
    return "sha256:" + character * 64


def digest_spec(character: str = "1") -> dict[str, object]:
    return {
        "name": "rereply",
        "region": "sgp",
        "vpc": {"id": "private-vpc"},
        "envs": [{"key": "SAFE", "value": "EV[private]", "type": "SECRET", "scope": "RUN_TIME"}],
        "services": [
            {"name": "omnitech-web", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-web", "digest": digest(character)}, "envs": []},
            {"name": "meta-relay", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-meta-relay", "digest": digest(character)}, "envs": []},
            {"name": "gmail-relay", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-gmail-relay", "digest": digest(character)}, "envs": []},
        ],
        "jobs": [
            {"name": "rereply-rls-migrate", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-web", "digest": digest(character)}, "envs": []},
        ],
        "ingress": {"rules": [{"match": {"path": {"prefix": "/"}}, "component": {"name": "omnitech-web"}}]},
        "domains": [{"domain": "example.invalid", "type": "PRIMARY"}],
        "databases": [],
    }


def environment_spec() -> dict[str, object]:
    spec = digest_spec()
    spec["envs"] = [
        {"key": "Z_APP", "value": "", "type": "GENERAL"},
        {
            "key": "A_APP",
            "value": "app-secret",
            "type": "SECRET",
            "scope": "RUN_TIME",
        },
    ]
    for index, service in enumerate(spec["services"]):
        service["envs"] = [
            {
                "key": f"SERVICE_{index}",
                "value": f"service-value-{index}",
                "scope": "RUN_TIME",
            }
        ]
    spec["jobs"][0]["envs"] = [
        {"key": "JOB_SECRET", "value": "job-secret", "type": "SECRET"}
    ]
    spec["workers"] = [
        {
            "name": "wørker-a",
            "envs": [{"key": "WORKER_值", "value": "välue"}],
        }
    ]
    spec["static_sites"] = [
        {
            "name": "static-a",
            "envs": [
                {
                    "key": "STATIC_SECRET",
                    "value": "static-secret",
                    "type": "SECRET",
                }
            ],
        }
    ]
    spec["functions"] = [
        {
            "name": "function-a",
            "envs": [{"key": "FUNCTION_VALUE", "value": "function-value"}],
        }
    ]
    return spec


def legacy_spec() -> dict[str, object]:
    spec = digest_spec()
    paths = {
        "omnitech-web": "docker/Dockerfile",
        "meta-relay": "docker/meta-relay.Dockerfile",
        "gmail-relay": "docker/gmail-relay.Dockerfile",
        "rereply-rls-migrate": "docker/Dockerfile",
    }
    for collection in ("services", "jobs"):
        for component in spec[collection]:
            component.pop("image")
            component["git"] = {"repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git", "branch": "main"}
            component["dockerfile_path"] = paths[component["name"]]
    return spec


def app_response(spec: dict[str, object], deployment_id: str, *, pinned: bool = False) -> dict[str, object]:
    return {
        "app": {
            "id": APP_ID,
            "updated_at": "2026-08-27T00:01:00Z",
            "default_ingress": "https://example.invalid",
            "spec": copy.deepcopy(spec),
            "active_deployment": {"id": deployment_id, "phase": "ACTIVE", "spec": copy.deepcopy(spec)},
            "in_progress_deployment": None,
            "pending_deployment": None,
            "pinned_deployment": ({"id": deployment_id} if pinned else None),
        }
    }


def deployment_response(spec: dict[str, object], deployment_id: str) -> dict[str, object]:
    return {
        "deployment": {
            "id": deployment_id,
            "phase": "ACTIVE",
            "spec": copy.deepcopy(spec),
            "jobs": [{"name": "rereply-rls-migrate", "phase": "SUCCEEDED"}],
        }
    }


def recovery_contract() -> dict[str, object]:
    return {
        "provider": {"app_id_sha256": "d" * 64},
        "expected_topology": {
            "region": "sgp",
            "databases": [
                {
                    "engine": "PG",
                    "version": "17",
                    "production": True,
                    "name_sha256": "1" * 64,
                    "cluster_sha256": "2" * 64,
                },
                {
                    "engine": "PG",
                    "version": "17",
                    "production": True,
                    "name_sha256": "5" * 64,
                    "cluster_sha256": "2" * 64,
                },
                {
                    "engine": "VALKEY",
                    "version": "8",
                    "production": True,
                    "name_sha256": "3" * 64,
                    "cluster_sha256": "4" * 64,
                },
            ],
        }
    }


def recovery_readiness(
    contract_sha256: str, now: dt.datetime
) -> dict[str, object]:
    postgresql_identity = "5" * 64
    valkey_identity = "6" * 64
    recovery_identity = "7" * 64
    identity_projection = common.sha256_value(
        {
            "postgresql_identity_sha256": postgresql_identity,
            "valkey_identity_sha256": valkey_identity,
            "valkey_recovery_identity_sha256": recovery_identity,
        }
    )
    plan_sha256 = "8" * 64
    request_sha256 = "9" * 64
    receipt_sha256 = "a" * 64
    topology_sha256 = "b" * 64
    config_sha256 = "c" * 64
    stable_round = [
        "postgres-cluster", "postgres-backups", "valkey-cluster", "valkey-config",
        "valkey-source-firewall", "valkey-recovery-cluster",
        "valkey-recovery-config", "valkey-recovery-firewall",
    ]
    return {
        "schema_version": 2,
        "authority": "production-recovery-readiness",
        "repository": common.REPOSITORY,
        "issued_at": common.format_timestamp(now),
        "expires_at": common.format_timestamp(now + dt.timedelta(minutes=5)),
        "control": {
            "workflow_sha": "d" * 40,
            "workflow_path": ".github/workflows/verify-production-recovery-readiness.yml",
            "run_id": "401",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "contract_sha256": contract_sha256,
            "controller_sha256": common.sha256_bytes(
                Path(apply.__file__).resolve().with_name(
                    "observe_production_recovery.py"
                ).read_bytes()
            ),
        },
        "authorities": {
            "production_plan": {
                "run_id": "201",
                "run_attempt": 1,
                "sha256": plan_sha256,
            },
            "valkey_fork": {
                "run_id": "301",
                "run_attempt": 1,
                "sha256": receipt_sha256,
                "request_sha256": request_sha256,
                "receipt_sha256": receipt_sha256,
            },
        },
        "target": {
            "descriptor_sha256": identity_projection,
            "contract_sha256": contract_sha256,
            "postgresql_identity_sha256": postgresql_identity,
            "valkey_identity_sha256": valkey_identity,
            "valkey_recovery_identity_sha256": recovery_identity,
            "identity_projection_sha256": identity_projection,
            "postgresql_cluster_sha256": "2" * 64,
            "valkey_cluster_sha256": "4" * 64,
            "region_sha256": common.sha256_bytes(b"sgp1"),
        },
        "postgresql": {
            "identity_sha256": postgresql_identity,
            "observation_sha256": "0" * 64,
            "status": "online",
            "engine": "postgresql",
            "version": "17",
            "region_sha256": common.sha256_bytes(b"sgp1"),
            "fresh_backup": True,
            "backup_identity_sha256": "1" * 64,
            "backup_inventory_sha256": "2" * 64,
            "point_in_time_restore_ready": True,
            "production_cluster_sha256": "2" * 64,
        },
        "valkey": {
            "identity_sha256": valkey_identity,
            "recovery_identity_sha256": recovery_identity,
            "source_observation_sha256": "3" * 64,
            "recovery_observation_sha256": "4" * 64,
            "status": "online",
            "recovery_status": "online",
            "version": "8",
            "recovery_version": "8",
            "region_sha256": common.sha256_bytes(b"sgp1"),
            "recovery_region_sha256": common.sha256_bytes(b"sgp1"),
            "source_topology_sha256": topology_sha256,
            "recovery_topology_sha256": topology_sha256,
            "persistence": "rdb",
            "recovery_persistence": "rdb",
            "recovery_is_distinct": True,
            "recovery_is_fresh": True,
            "topology_equal": True,
            "production_cluster_sha256": "4" * 64,
            "provider_fork": {
                "authority": "production-valkey-recovery-fork-v2",
                "source_identity_sha256": valkey_identity,
                "recovery_identity_sha256": recovery_identity,
                "request_sha256": request_sha256,
                "receipt_sha256": receipt_sha256,
                "source_config_sha256": config_sha256,
                "recovery_config_sha256": config_sha256,
                "source_firewall_sha256": "5" * 64,
                "recovery_firewall_sha256": "6" * 64,
                "fork_name_sha256": "7" * 64,
                "fork_created_at_sha256": "8" * 64,
                "provider_copy_contract": (
                    "digitalocean-valkey-latest-transaction-data-and-configuration"
                ),
                "stable_read_count": 2,
                "request_attempt_count": 1,
                "mutation_ambiguous_reconciled": False,
                "source_firewall_unchanged": True,
                "source_firewall_exact_app": True,
                "recovery_firewall_exact_source_app": True,
                "recovery_restricted_to_exact_production_app": True,
            },
        },
        "provider": {
            "http_methods_used": ["GET"],
            "http_request_count": 17,
            "http_endpoint_labels": [
                "valkey-recovery-discovery", *stable_round, *stable_round,
            ],
            "mutation_request_count": 0,
        },
        "gates": {
            "postgresql_ready": True,
            "valkey_ready": True,
            "double_read_equal": True,
            "mutation_free": True,
            "provider_fork_bound": True,
            "recovery_restricted_to_exact_production_app": True,
        },
    }


class FakeResponse:
    def __init__(self, value: object, url: str, *, content_type: str = "application/json", status: int = 200) -> None:
        self.raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.url = url
        self.status = status
        self.headers = {"Content-Type": content_type}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def getcode(self) -> int:
        return self.status

    def read(self, amount: int) -> bytes:
        return self.raw[:amount]


class IncompleteResponse(FakeResponse):
    def read(self, amount: int) -> bytes:
        raise http.client.IncompleteRead(b'{"app":')


class QueueOpener:
    def __init__(self, values: list[object]) -> None:
        self.values = list(values)
        self.requests: list[object] = []

    def open(self, request: object, timeout: int) -> FakeResponse:
        self.requests.append(request)
        value = self.values.pop(0)
        if isinstance(value, BaseException):
            raise value
        if isinstance(value, FakeResponse):
            return value
        return FakeResponse(value, request.full_url)


class ApplyControllerTests(unittest.TestCase):
    def test_isolated_direct_entrypoint_resolves_only_sibling_controls(self) -> None:
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(Path(apply.__file__).resolve()), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_provider_native_recovery_v2_binds_exact_plan_and_fork(self) -> None:
        now = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        contract_raw = common.canonical_file_bytes(recovery_contract())
        with tempfile.TemporaryDirectory() as temporary:
            contract_path = Path(temporary) / "production-app-contract.json"
            contract_path.write_bytes(contract_raw)
            value = recovery_readiness(common.sha256_bytes(contract_raw), now)
            value_sha256 = common.sha256_bytes(common.canonical_file_bytes(value))
            expected_control = copy.deepcopy(value["control"])
            validated = apply.validate_recovery(
                value,
                value_sha256,
                now,
                contract_path=contract_path,
                expected_control=expected_control,
            )
            self.assertEqual(validated["schema_version"], 2)
            self.assertEqual(
                validated["valkey"]["provider_fork"]["stable_read_count"], 2
            )
            apply._require_recovery_plan_authority(
                validated,
                {"run_id": "201", "run_attempt": 1, "sha256": "8" * 64},
            )
            with self.assertRaises(common.ReleaseError):
                apply._require_recovery_plan_authority(
                    validated,
                    {"run_id": "201", "run_attempt": 1, "sha256": "9" * 64},
                )
            for label, key, replacement in (
                ("workflow-sha", "workflow_sha", "e" * 40),
                ("run-id", "run_id", "999"),
            ):
                tampered = copy.deepcopy(value)
                tampered["control"][key] = replacement
                with self.subTest(control=label):
                    with self.assertRaises(common.ReleaseError):
                        apply.validate_recovery(
                            tampered,
                            common.sha256_bytes(common.canonical_file_bytes(tampered)),
                            now,
                            contract_path=contract_path,
                            expected_control=expected_control,
                        )

    def test_recovery_accepts_raw_hash_bound_checked_in_pretty_contract(self) -> None:
        now = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        contract_path = Path(apply.__file__).resolve().with_name(
            "production-app-contract.json"
        )
        contract_raw = contract_path.read_bytes()
        value = recovery_readiness(common.sha256_bytes(contract_raw), now)
        bindings = apply.recovery_control.contract_database_bindings(
            common.loads_strict(contract_raw.decode("utf-8"))
        )
        value["target"]["postgresql_cluster_sha256"] = bindings[
            "postgresql_cluster_sha256"
        ]
        value["target"]["valkey_cluster_sha256"] = bindings[
            "valkey_cluster_sha256"
        ]
        value["target"]["region_sha256"] = bindings["region_sha256"]
        value["postgresql"]["production_cluster_sha256"] = bindings[
            "postgresql_cluster_sha256"
        ]
        value["postgresql"]["version"] = bindings["postgresql_version"]
        value["postgresql"]["region_sha256"] = bindings["region_sha256"]
        value["valkey"]["production_cluster_sha256"] = bindings[
            "valkey_cluster_sha256"
        ]
        value["valkey"]["version"] = bindings["valkey_version"]
        value["valkey"]["recovery_version"] = bindings["valkey_version"]
        value["valkey"]["region_sha256"] = bindings["region_sha256"]
        value["valkey"]["recovery_region_sha256"] = bindings["region_sha256"]
        self.assertEqual(
            apply.validate_recovery(value, common.sha256_bytes(common.canonical_file_bytes(value)), now)[
                "schema_version"
            ],
            2,
        )

    def test_recovery_v1_and_provider_fork_drift_fail_closed(self) -> None:
        now = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        contract_raw = common.canonical_file_bytes(recovery_contract())
        mutations: list[tuple[str, tuple[str, ...], object]] = [
            ("v1", ("schema_version",), 1),
            ("controller-drift", ("control", "controller_sha256"), "e" * 64),
            ("fork-attempt", ("authorities", "valkey_fork", "run_attempt"), 2),
            ("fork-file-sha", ("authorities", "valkey_fork", "sha256"), "f" * 64),
            ("fork-receipt", ("valkey", "provider_fork", "receipt_sha256"), "0" * 64),
            ("same-fork", ("valkey", "recovery_identity_sha256"), "6" * 64),
            ("offline-fork", ("valkey", "recovery_status"), "creating"),
            ("version-drift", ("valkey", "recovery_version"), "7"),
            ("non-rdb", ("valkey", "recovery_persistence"), "off"),
            ("topology-drift", ("valkey", "recovery_topology_sha256"), "0" * 64),
            ("stale-fork", ("valkey", "recovery_is_fresh"), False),
            ("config-drift", ("valkey", "provider_fork", "recovery_config_sha256"), "0" * 64),
            ("source-firewall-drift", ("valkey", "provider_fork", "source_firewall_unchanged"), False),
            ("source-firewall-not-app", ("valkey", "provider_fork", "source_firewall_exact_app"), False),
            ("fork-firewall-drift", ("valkey", "provider_fork", "recovery_firewall_exact_source_app"), False),
            ("fork-not-production-app", ("valkey", "provider_fork", "recovery_restricted_to_exact_production_app"), False),
            ("single-read", ("valkey", "provider_fork", "stable_read_count"), 1),
            ("repeated-create", ("valkey", "provider_fork", "request_attempt_count"), 2),
            ("ambiguous-create", ("valkey", "provider_fork", "mutation_ambiguous_reconciled"), True),
            ("provider-mutation", ("provider", "mutation_request_count"), 1),
            ("missing-get", ("provider", "http_request_count"), 16),
            ("incomplete-gate", ("gates", "recovery_restricted_to_exact_production_app"), False),
        ]
        with tempfile.TemporaryDirectory() as temporary:
            contract_path = Path(temporary) / "production-app-contract.json"
            contract_path.write_bytes(contract_raw)
            for label, path, replacement in mutations:
                tampered = recovery_readiness(common.sha256_bytes(contract_raw), now)
                cursor: dict[str, object] = tampered
                for key in path[:-1]:
                    cursor = cursor[key]  # type: ignore[assignment,index]
                cursor[path[-1]] = replacement
                with self.subTest(label=label):
                    with self.assertRaises(common.ReleaseError):
                        apply.validate_recovery(
                            tampered,
                            common.sha256_bytes(common.canonical_file_bytes(tampered)),
                            now,
                            contract_path=contract_path,
                        )

    def test_recovery_readiness_expiry_is_fail_closed(self) -> None:
        issued = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        checked = issued + dt.timedelta(minutes=6)
        contract_raw = common.canonical_file_bytes(recovery_contract())
        with tempfile.TemporaryDirectory() as temporary:
            contract_path = Path(temporary) / "production-app-contract.json"
            contract_path.write_bytes(contract_raw)
            value = recovery_readiness(common.sha256_bytes(contract_raw), issued)
            with self.assertRaises(common.ReleaseError):
                apply.validate_recovery(
                    value,
                    common.sha256_bytes(common.canonical_file_bytes(value)),
                    checked,
                    contract_path=contract_path,
                )

    def test_plan_and_recovery_expiring_during_preflight_block_before_put(self) -> None:
        initial = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        expired = initial + dt.timedelta(minutes=6)
        plan = {
            "issued_at": common.format_timestamp(initial),
            "expires_at": common.format_timestamp(initial + dt.timedelta(minutes=5)),
        }
        recovery = {
            "issued_at": common.format_timestamp(initial),
            "expires_at": common.format_timestamp(initial + dt.timedelta(minutes=5)),
        }
        moments = iter((initial, expired))
        clock = lambda: next(moments)
        self.assertEqual(apply._clock_value(clock), initial)
        with self.assertRaises(common.ReleaseError):
            apply.require_fresh_immediately_before_mutation(
                plan=plan, recovery=recovery, clock=clock
            )
        source = inspect.getsource(apply.apply_change)
        self.assertLess(source.index("cas_before"), source.index("require_fresh_immediately_before_mutation"))
        self.assertLess(source.index("require_fresh_immediately_before_mutation"), source.index("put_app_once"))
        self.assertEqual(source.count("validate_recovery("), 2)

    def test_expiry_is_exclusive_and_fractional_clock_is_not_truncated(self) -> None:
        issued = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        expires = issued + dt.timedelta(minutes=5)
        authority = {
            "issued_at": common.format_timestamp(issued),
            "expires_at": common.format_timestamp(expires),
        }
        fractional = expires + dt.timedelta(microseconds=999_999)
        self.assertEqual(apply._clock_value(lambda: fractional), fractional)
        for checked in (expires, fractional):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    apply.require_fresh_immediately_before_mutation(
                        plan=authority, recovery=authority, clock=lambda: checked
                    )

    def test_plan_control_must_match_exact_artifact_and_current_controls(self) -> None:
        authority = {
            "run_id": "101",
            "run_attempt": 1,
            "artifact_id": "201",
            "artifact_digest": digest("1"),
            "sha256": "2" * 64,
        }
        plan = {
            "control": {
                "workflow_sha": "a" * 40,
                "workflow_path": ".github/workflows/plan-production-rollout.yml",
                "run_id": "101",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "contract_sha256": "3" * 64,
                "release_policy_sha256": "4" * 64,
                "change_schema_sha256": "5" * 64,
                "verifier_sha256": "6" * 64,
            }
        }
        apply._validate_plan_control(
            plan, {"workflow_sha": "a" * 40}, authority, "4" * 64, "5" * 64
        )
        tampered = copy.deepcopy(plan)
        tampered["control"]["run_id"] = "102"
        with self.assertRaises(common.ReleaseError):
            apply._validate_plan_control(
                tampered,
                {"workflow_sha": "a" * 40},
                authority,
                "4" * 64,
                "5" * 64,
            )

    def test_exact_image_update_preserves_every_non_source_leaf(self) -> None:
        before = legacy_spec()
        desired = common.set_phase_images(
            before,
            {"web": digest("1"), "meta-relay": digest("2"), "gmail-relay": digest("3")},
        )
        changes = common.require_exact_image_change(before, desired)
        self.assertEqual(len(changes), 12)
        self.assertEqual(common.non_source_fingerprint(before), common.non_source_fingerprint(desired))
        self.assertEqual(common.environment_value_fingerprint(before), common.environment_value_fingerprint(desired))
        self.assertEqual(common.extract_image_digests(desired)["web"], digest("1"))
        self.assertEqual(common.extract_image_digests(desired)["meta-relay"], digest("2"))

    def test_environment_fingerprint_matches_planner_inventory_and_apply_snapshot(
        self,
    ) -> None:
        spec = environment_spec()
        expected_records = [
            [
                "app",
                "app",
                "Z_APP",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes(b""),
            ],
            [
                "app",
                "app",
                "A_APP",
                "RUN_TIME",
                "SECRET",
                common.sha256_bytes(b"app-secret"),
            ],
            [
                "services",
                "omnitech-web",
                "SERVICE_0",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes(b"service-value-0"),
            ],
            [
                "services",
                "meta-relay",
                "SERVICE_1",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes(b"service-value-1"),
            ],
            [
                "services",
                "gmail-relay",
                "SERVICE_2",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes(b"service-value-2"),
            ],
            [
                "jobs",
                "rereply-rls-migrate",
                "JOB_SECRET",
                "RUN_TIME",
                "SECRET",
                common.sha256_bytes(b"job-secret"),
            ],
            [
                "workers",
                "wørker-a",
                "WORKER_值",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes("välue".encode("utf-8")),
            ],
            [
                "static_sites",
                "static-a",
                "STATIC_SECRET",
                "RUN_TIME",
                "SECRET",
                common.sha256_bytes(b"static-secret"),
            ],
            [
                "functions",
                "function-a",
                "FUNCTION_VALUE",
                "RUN_TIME",
                "GENERAL",
                common.sha256_bytes(b"function-value"),
            ],
        ]
        expected_hash = planner.sha256_value(sorted(expected_records))
        self.assertEqual(
            planner.environment_value_inventory(spec), (expected_hash, 9, 3)
        )
        self.assertEqual(common.environment_value_fingerprint(spec), expected_hash)

        snapshot, _live_spec, _deployment = apply.provider_snapshot(
            app_response(spec, OLD_DEPLOYMENT),
            deployment_response(spec, OLD_DEPLOYMENT),
            APP_ID,
        )
        self.assertEqual(snapshot["environment_values_sha256"], expected_hash)

    def test_environment_fingerprint_ordering_and_empty_defaults_match_planner(
        self,
    ) -> None:
        spec = environment_spec()
        expected = planner.environment_value_fingerprint(spec)
        reordered = copy.deepcopy(spec)
        reordered["envs"].reverse()
        for collection in (
            "services",
            "jobs",
            "workers",
            "static_sites",
            "functions",
        ):
            reordered[collection].reverse()
            for component in reordered[collection]:
                component["envs"].reverse()
        self.assertEqual(planner.environment_value_fingerprint(reordered), expected)
        self.assertEqual(common.environment_value_fingerprint(reordered), expected)

        nullable = copy.deepcopy(reordered)
        nullable["functions"][0]["envs"] = None
        self.assertEqual(
            common.environment_value_fingerprint(nullable),
            planner.environment_value_fingerprint(nullable),
        )
        snapshot, _live_spec, _deployment = apply.provider_snapshot(
            app_response(reordered, OLD_DEPLOYMENT),
            deployment_response(reordered, OLD_DEPLOYMENT),
            APP_ID,
        )
        self.assertEqual(snapshot["environment_values_sha256"], expected)

    def test_environment_fingerprint_rejects_planner_invalid_apply_snapshots(
        self,
    ) -> None:
        mutations = (
            (
                "malformed component collection",
                lambda value: value.__setitem__("workers", {}),
            ),
            (
                "malformed component",
                lambda value: value["workers"].__setitem__(0, "worker-a"),
            ),
            (
                "missing component name",
                lambda value: value["workers"][0].pop("name"),
            ),
            (
                "oversize component name",
                lambda value: value["workers"][0].__setitem__("name", "n" * 513),
            ),
            (
                "duplicate component name",
                lambda value: value["workers"].append(
                    copy.deepcopy(value["workers"][0])
                ),
            ),
            (
                "malformed environment list",
                lambda value: value["workers"][0].__setitem__("envs", {}),
            ),
            (
                "malformed environment entry",
                lambda value: value["workers"][0]["envs"].__setitem__(0, "env"),
            ),
            (
                "missing environment key",
                lambda value: value["workers"][0]["envs"][0].pop("key"),
            ),
            (
                "oversize environment key",
                lambda value: value["workers"][0]["envs"][0].__setitem__(
                    "key", "k" * 513
                ),
            ),
            (
                "environment key control character",
                lambda value: value["workers"][0]["envs"][0].__setitem__(
                    "key", "BAD\nKEY"
                ),
            ),
            (
                "duplicate environment key",
                lambda value: value["workers"][0]["envs"].append(
                    copy.deepcopy(value["workers"][0]["envs"][0])
                ),
            ),
            (
                "unsupported environment scope",
                lambda value: value["workers"][0]["envs"][0].__setitem__(
                    "scope", "BUILD_TIME"
                ),
            ),
            (
                "unsupported environment type",
                lambda value: value["workers"][0]["envs"][0].__setitem__(
                    "type", "ENCRYPTED"
                ),
            ),
            (
                "missing environment value",
                lambda value: value["workers"][0]["envs"][0].pop("value"),
            ),
            (
                "non-string environment value",
                lambda value: value["workers"][0]["envs"][0].__setitem__(
                    "value", 1
                ),
            ),
        )
        for label, mutate in mutations:
            spec = environment_spec()
            mutate(spec)
            with self.subTest(label=label, consumer="planner"):
                with self.assertRaises(planner.PlanError):
                    planner.environment_value_fingerprint(spec)
            with self.subTest(label=label, consumer="release"):
                with self.assertRaises(common.ReleaseError):
                    common.environment_value_fingerprint(spec)
            with self.subTest(label=label, consumer="apply snapshot"):
                with self.assertRaises(common.ReleaseError):
                    apply.provider_snapshot(
                        app_response(spec, OLD_DEPLOYMENT),
                        deployment_response(spec, OLD_DEPLOYMENT),
                        APP_ID,
                    )

    def test_digest_phase_changes_exactly_four_digest_leaves(self) -> None:
        before = digest_spec("1")
        desired = common.set_phase_images(before, {key: digest("2") for key in ("web", "meta-relay", "gmail-relay")})
        changes = common.require_exact_image_change(before, desired)
        self.assertEqual(len(changes), 4)
        self.assertTrue(all(pointer.endswith("/image/digest") for pointer in changes))

    def test_sent_put_with_malformed_200_is_ambiguous_and_reconciles_get_only(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                FakeResponse({"app": {}}, app_url, content_type="text/plain"),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        app_value, deployment_value, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual(app_value["app"]["spec"], desired)
        self.assertEqual(deployment_value["deployment"]["phase"], "ACTIVE")
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_http_408_after_put_is_ambiguous_and_never_retries_mutation(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                urllib.error.HTTPError(app_url, 408, "timeout", {}, None),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        _, _, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_truncated_put_response_is_ambiguous_and_reconciles_get_only(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                IncompleteResponse({"app": {}}, app_url),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        _, _, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_pinned_or_pending_deployment_blocks_before_mutation(self) -> None:
        spec = digest_spec()
        for key, value in (("pinned_deployment", {"id": OLD_DEPLOYMENT}), ("pending_deployment", {"id": OLD_DEPLOYMENT})):
            app_value = app_response(spec, OLD_DEPLOYMENT)
            app_value["app"][key] = value
            opener = QueueOpener(
                [
                    app_value,
                    deployment_response(spec, OLD_DEPLOYMENT),
                    copy.deepcopy(app_value),
                    deployment_response(spec, OLD_DEPLOYMENT),
                ]
            )
            client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
            with self.subTest(key=key):
                with self.assertRaises(common.ReleaseError):
                    apply.observe_stable(client)
                self.assertFalse(any(request.method == "PUT" for request in opener.requests))

    def test_timestamp_drift_is_allowed_only_for_predecessor_lineage(self) -> None:
        import test_verify_production_release as release_fixtures

        receipt = release_fixtures.apply_receipt()
        receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
        state = common.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="9" * 64,
            control={
                "workflow_sha": "a" * 40,
                "workflow_path": (
                    ".github/workflows/verify-production-crm-canary.yml"
                ),
                "run_id": "401",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": "b" * 64,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:01:00Z",
        )
        state_hash = common.sha256_bytes(common.canonical_file_bytes(state))
        live = copy.deepcopy(state["provider_state"])
        live["app_updated_at_sha256"] = "9" * 64
        apply._validate_predecessor(
            state, state_hash, "baseline", live, state_hash
        )

        for key in (
            "app_identity_sha256",
            "default_ingress_sha256",
            "active_deployment_identity_sha256",
            "canonical_spec_sha256",
            "environment_values_sha256",
            "non_source_projection_sha256",
        ):
            with self.subTest(key=key):
                drifted = copy.deepcopy(live)
                drifted[key] = "0" * 64
                with self.assertRaises(common.ReleaseError):
                    apply._validate_predecessor(
                        state, state_hash, "baseline", drifted, state_hash
                    )

        image_drift = copy.deepcopy(live)
        image = image_drift["images"][0]
        image["digest"] = digest("9")
        image["subject"] = image["repository"] + "@" + digest("9")
        with self.assertRaises(common.ReleaseError):
            apply._validate_predecessor(
                state, state_hash, "baseline", image_drift, state_hash
            )

        observation = {
            "provider_observation": {
                "app_identity_sha256": live["app_identity_sha256"],
                "default_ingress_sha256": live["default_ingress_sha256"],
                "app_updated_at_sha256": live["app_updated_at_sha256"],
                "active_deployment_identity_sha256": live[
                    "active_deployment_identity_sha256"
                ],
                "live_canonical_spec_sha256": live[
                    "canonical_spec_sha256"
                ],
                "environment_values_sha256": live[
                    "environment_values_sha256"
                ],
                "non_source_projection_sha256": live[
                    "non_source_projection_sha256"
                ],
                "live_active_equal": True,
                "predecessor_match": True,
            }
        }
        apply._match_plan_observation(observation, live)
        observation["provider_observation"]["app_updated_at_sha256"] = "8" * 64
        with self.assertRaises(common.ReleaseError):
            apply._match_plan_observation(observation, live)

    def test_live_timestamp_change_between_apply_reads_fails_closed(self) -> None:
        spec = digest_spec()
        first_app = app_response(spec, OLD_DEPLOYMENT)
        second_app = app_response(spec, OLD_DEPLOYMENT)
        second_app["app"]["updated_at"] = "2026-08-27T00:01:01Z"
        opener = QueueOpener(
            [
                first_app,
                deployment_response(spec, OLD_DEPLOYMENT),
                second_app,
                deployment_response(spec, OLD_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaisesRegex(
            common.ReleaseError, "production changed between the two exact reads"
        ):
            apply.observe_stable(client)
        self.assertEqual([request.method for request in opener.requests], ["GET"] * 4)

    def test_baseline_uses_non_null_genesis_authority_without_file(self) -> None:
        before = {
            "source_mode": "legacy-git",
        }
        genesis = "a" * 64
        apply._validate_predecessor(None, None, "genesis", before, genesis)
        with self.assertRaises(common.ReleaseError):
            apply._validate_predecessor(None, None, "genesis", before, "not-a-hash")
        request = apply.validate_evidence_request(
            {
                "production_plan": {"run_id": "1", "run_attempt": 1, "artifact_id": "2", "artifact_digest": "sha256:" + "1" * 64, "sha256": "2" * 64},
                "recovery": {"run_id": "3", "run_attempt": 1, "artifact_id": "4", "artifact_digest": "sha256:" + "3" * 64, "sha256": "4" * 64},
                "rollout_plan_sha256": "5" * 64,
                "predecessor_state_sha256": genesis,
            }
        )
        self.assertEqual(request["predecessor_state_sha256"], genesis)


if __name__ == "__main__":
    unittest.main()
