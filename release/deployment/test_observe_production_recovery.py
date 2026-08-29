from __future__ import annotations

import copy
import datetime as dt
import decimal
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import apply_production_change as apply
import observe_production_recovery as recovery
import verify_production_release as common


NOW = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
TARGET = {
    "postgres_cluster_id": "11111111-1111-4111-8111-111111111111",
    "valkey_cluster_id": "22222222-2222-4222-8222-222222222222",
}
RECOVERY_ID = "33333333-3333-4333-8333-333333333333"
PRIVATE_NETWORK_UUID = "44444444-4444-4444-8444-444444444444"
SOURCE_RULE_UUID = "55555555-5555-4555-8555-555555555555"
RECOVERY_RULE_UUID = "66666666-6666-4666-8666-666666666666"
APP_ID = "synthetic-app"
CONTROL = {
    "workflow_sha": "a" * 40,
    "workflow_path": recovery.WORKFLOW_PATH,
    "run_id": "303",
    "run_attempt": 1,
    "runner_environment": "github-hosted",
}
FORK_RUN_ID = "202"
PLAN_RUN_ID = "101"
FORK_NAME = "rereply-recovery-baseline-aaaaaaaa-202-1"
DATABASE_NAMES = {
    "postgres": "test-postgres-cluster",
    "valkey": "test-valkey-cluster",
}
DATABASE_HOSTS = {
    "postgres": "test-postgres.db.ondigitalocean.com",
    "valkey": "test-valkey.db.ondigitalocean.com",
    "recovery": "test-valkey-recovery.db.ondigitalocean.com",
}


def database(
    identity: str,
    engine: str,
    created_at: str,
    name: str,
    host: str,
) -> dict[str, object]:
    return {
        "database": {
            "id": identity,
            "status": "online",
            "engine": engine,
            "version": "17" if engine == "pg" else "8",
            "region": "sgp1",
            "created_at": created_at,
            "name": name,
            "size": "db-s-1vcpu-1gb",
            "num_nodes": 1,
            "private_network_uuid": PRIVATE_NETWORK_UUID,
            "storage_size_mib": 10240,
            "connection": {"host": host, "port": 25061},
        }
    }


def observations() -> dict[str, object]:
    return {
        "postgres-cluster": database(
            TARGET["postgres_cluster_id"], "pg", "2025-01-01T00:00:00Z",
            DATABASE_NAMES["postgres"], DATABASE_HOSTS["postgres"],
        ),
        "postgres-backups": {
            "backups": [{"created_at": "2026-08-26T23:30:00Z", "size_gigabytes": 1}]
        },
        "valkey-cluster": database(
            TARGET["valkey_cluster_id"], "valkey", "2025-01-01T00:00:00Z",
            DATABASE_NAMES["valkey"], DATABASE_HOSTS["valkey"],
        ),
        "valkey-config": {"config": {"redis_persistence": "rdb", "maxmemory_policy": "allkeys-lru"}},
        "valkey-source-firewall": {
            "rules": [
                {
                    "type": "app",
                    "value": APP_ID,
                    "uuid": SOURCE_RULE_UUID,
                    "description": "source app rule",
                }
            ]
        },
        "valkey-recovery-cluster": database(
            RECOVERY_ID, "valkey", "2026-08-26T23:00:00Z", FORK_NAME,
            DATABASE_HOSTS["recovery"],
        ),
        "valkey-recovery-config": {
            "config": {"redis_persistence": "rdb", "maxmemory_policy": "allkeys-lru"}
        },
        "valkey-recovery-firewall": {
            "rules": [
                {
                    "type": "app",
                    "value": APP_ID,
                    "uuid": RECOVERY_RULE_UUID,
                    "description": "provider-created fork rule",
                }
            ]
        },
    }


def discovery(*, duplicate: bool = False) -> dict[str, object]:
    records = [
        {"id": TARGET["postgres_cluster_id"], "name": DATABASE_NAMES["postgres"]},
        {"id": TARGET["valkey_cluster_id"], "name": DATABASE_NAMES["valkey"]},
        {"id": RECOVERY_ID, "name": FORK_NAME},
    ]
    if duplicate:
        records.append(
            {"id": "66666666-6666-4666-8666-666666666666", "name": FORK_NAME}
        )
    return {"databases": records, "links": {}, "meta": {"total": len(records)}}


def contract() -> dict[str, object]:
    return {
        "provider": {
            "app_id_sha256": common.sha256_bytes(APP_ID.encode("utf-8")),
        },
        "expected_topology": {
            "region": "sgp",
            "databases": [
                {
                    "engine": "PG", "version": "17", "production": True,
                    "name_sha256": common.sha256_bytes(b"postgres-app-binding"),
                    "cluster_sha256": common.sha256_bytes(
                        DATABASE_NAMES["postgres"].encode("utf-8")
                    ),
                },
                {
                    "engine": "VALKEY", "version": "8", "production": True,
                    "name_sha256": common.sha256_bytes(b"valkey-app-binding"),
                    "cluster_sha256": common.sha256_bytes(
                        DATABASE_NAMES["valkey"].encode("utf-8")
                    ),
                },
            ],
        }
    }


def production_plan() -> dict[str, object]:
    return {
        "schema_version": 2,
        "authority": "observation-only-production-plan",
        "repository": common.REPOSITORY,
        "control": {
            "workflow_sha": CONTROL["workflow_sha"],
            "run_id": PLAN_RUN_ID,
            "run_attempt": 1,
        },
        "rollout_authority": {"rollout_plan_sha256": "d" * 64},
        "transition": {"operation": "activate", "from": None, "to": "baseline", "ordinal": 1},
        "target": {"phase": "baseline"},
    }


def plan_authority(plan: dict[str, object] | None = None) -> dict[str, object]:
    plan = plan or production_plan()
    return {
        "run_id": PLAN_RUN_ID,
        "run_attempt": 1,
        "sha256": common.sha256_bytes(common.canonical_file_bytes(plan)),
    }


def receipt(
    values: dict[str, object] | None = None,
    *,
    contract_sha256: str = "b" * 64,
) -> dict[str, object]:
    values = values or observations()
    source = recovery._database(
        values["valkey-cluster"], TARGET["valkey_cluster_id"], {"valkey"}, "Valkey"
    )
    fork = recovery._database(
        values["valkey-recovery-cluster"], RECOVERY_ID, {"valkey"}, "Valkey recovery fork"
    )
    source_config = recovery._config(values["valkey-config"], "Valkey")
    fork_config = recovery._config(values["valkey-recovery-config"], "Valkey recovery fork")
    source_firewall = recovery._firewall(values["valkey-source-firewall"], "Valkey")
    fork_firewall = recovery._firewall(
        values["valkey-recovery-firewall"], "Valkey recovery fork"
    )
    return {
        "schema_version": 2,
        "authority": recovery.FORK_RECEIPT_AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": "2026-08-26T23:50:00Z",
        "expires_at": "2026-08-27T00:10:00Z",
        "phase": "baseline",
        "control": {
            "workflow_sha": CONTROL["workflow_sha"],
            "workflow_path": recovery.FORK_WORKFLOW_PATH,
            "run_id": FORK_RUN_ID,
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "rollout_plan_sha256": "d" * 64,
            "contract_sha256": contract_sha256,
            "controller_sha256": common.sha256_bytes(
                Path(recovery.fork_control.__file__).resolve().read_bytes()
            ),
        },
        "target": {
            "descriptor_sha256": recovery._stable_target_descriptor_sha256(TARGET),
            "source_identity_sha256": source["identity_sha256"],
            "source_name_sha256": source["name_sha256"],
            "source_observation_sha256": source["observation_sha256"],
            "source_topology_sha256": source["topology_sha256"],
            "source_config_sha256": source_config["sha256"],
            "source_firewall_sha256": source_firewall["sha256"],
        },
        "request": {
            "method": "POST",
            "endpoint_label": "database-clusters",
            "request_sha256": "f" * 64,
            "request_attempt_count": 1,
            "provider_copy_contract": recovery.PROVIDER_COPY_CONTRACT,
        },
        "result": {
            "outcome": "created",
            "recovery_identity_sha256": fork["identity_sha256"],
            "fork_name_sha256": fork["name_sha256"],
            "fork_created_at_sha256": common.sha256_bytes(
                fork["created_at_raw"].encode("utf-8")
            ),
            "recovery_observation_sha256": fork["observation_sha256"],
            "recovery_topology_sha256": fork["topology_sha256"],
            "recovery_config_sha256": fork_config["sha256"],
            "recovery_firewall_sha256": fork_firewall["sha256"],
            "mutation_ambiguous_reconciled": False,
        },
        "provider": {
            "http_methods_used": ["GET", "POST"],
            "http_request_count": 11,
            "endpoint_labels": [
                "valkey-cluster",
                "valkey-config",
                "valkey-source-firewall",
                "valkey-recovery-discovery",
                "create-valkey-recovery-fork",
                "valkey-recovery-cluster-ready",
                "valkey-recovery-config-ready",
                "valkey-recovery-firewall-ready",
                "valkey-cluster-post-create",
                "valkey-config-post-create",
                "valkey-source-firewall-post-create",
            ],
            "mutation_request_count": 1,
        },
        "gates": {
            "source_ready": True,
            "fork_ready": True,
            "source_stable": True,
            "source_firewall_exact_app": True,
            "recovery_firewall_exact_source_app": True,
            "recovery_restricted_to_exact_production_app": True,
            "exact_single_mutation": True,
        },
    }


def receipt_authority(value: dict[str, object]) -> dict[str, object]:
    return {
        "run_id": FORK_RUN_ID,
        "run_attempt": 1,
        "sha256": common.sha256_bytes(common.canonical_file_bytes(value)),
    }


class FakeResponse:
    def __init__(self, value: object, url: str, *, content_type: str = "application/json") -> None:
        self.raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.url = url
        self.status = 200
        self.headers = {"Content-Type": content_type}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def read(self, amount: int) -> bytes:
        return self.raw[:amount]


class FakeOpener:
    def __init__(self, values: list[tuple[object, str]]) -> None:
        self.values = list(values)
        self.requests: list[object] = []

    def open(self, request: object, timeout: int) -> FakeResponse:
        if timeout != 20 or request.method != "GET" or request.data is not None:
            raise AssertionError("recovery observer attempted a non-exact GET")
        self.requests.append(request)
        if not self.values:
            raise AssertionError("unexpected extra provider read")
        value, url = self.values.pop(0)
        return FakeResponse(value, url)


class RecoveryReadinessTests(unittest.TestCase):
    def test_isolated_direct_entrypoint_resolves_only_sibling_controls(self) -> None:
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(Path(recovery.__file__).resolve()), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def ledger(self) -> list[tuple[str, str]]:
        return [("GET", recovery.DISCOVERY_LABEL)] + [
            ("GET", label) for label in recovery.STABLE_LABELS
        ] * 2

    def fork_evidence(
        self,
        values: dict[str, object] | None = None,
        fork_receipt: dict[str, object] | None = None,
        *,
        contract_sha256: str = "b" * 64,
    ) -> dict[str, object]:
        values = values or observations()
        plan = production_plan()
        evidence = fork_receipt or receipt(values, contract_sha256=contract_sha256)
        authority = receipt_authority(evidence)
        return recovery.validate_fork_receipt(
            evidence,
            receipt_sha256=authority["sha256"],
            fork_authority=authority,
            plan=plan,
            plan_authority=plan_authority(plan),
            control_sha=CONTROL["workflow_sha"],
            contract_sha256=contract_sha256,
            target=TARGET,
            now=NOW,
        )

    def build(
        self,
        values: dict[str, object] | None = None,
        *,
        first: dict[str, object] | None = None,
        second: dict[str, object] | None = None,
        ledger: list[tuple[str, str]] | None = None,
        fork_receipt: dict[str, object] | None = None,
        contract_sha256: str = "b" * 64,
    ) -> dict[str, object]:
        values = values or observations()
        return recovery.build_readiness(
            target=TARGET,
            control=CONTROL,
            first=first or values,
            second=second or copy.deepcopy(values),
            request_log=self.ledger() if ledger is None else ledger,
            now=NOW,
            contract_sha256=contract_sha256,
            controller_sha256=common.sha256_bytes(
                Path(recovery.__file__).resolve().read_bytes()
            ),
            contract_databases=recovery.contract_database_bindings(contract()),
            fork_evidence=self.fork_evidence(
                values, fork_receipt, contract_sha256=contract_sha256
            ),
            recovery_id=RECOVERY_ID,
            recovery_name=FORK_NAME,
        )

    def test_provider_parser_preserves_decimal_and_rejects_unsafe_json(self) -> None:
        value = recovery.loads_provider_json(
            b'{"backups":[{"size_gigabytes":0.03364864}]}'
        )
        self.assertEqual(
            value["backups"][0]["size_gigabytes"], decimal.Decimal("0.03364864")
        )
        for raw in (b'{"x":1,"x":2}', b'{"x":NaN}', b'{"x":Infinity}'):
            with self.subTest(raw=raw):
                with self.assertRaises(common.ReleaseError):
                    recovery.loads_provider_json(raw)

    def test_contract_cluster_name_is_distinct_from_app_binding_and_raw_uuid(self) -> None:
        source_contract = contract()
        bindings = recovery.contract_database_bindings(contract())
        self.assertEqual(
            bindings["valkey_cluster_sha256"],
            common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
        )
        self.assertNotEqual(
            bindings["valkey_cluster_sha256"],
            common.sha256_bytes(TARGET["valkey_cluster_id"].encode("utf-8")),
        )
        self.assertNotEqual(
            bindings["valkey_cluster_sha256"],
            source_contract["expected_topology"]["databases"][1]["name_sha256"],
        )
        wrong_name = observations()
        wrong_name["valkey-cluster"]["database"]["name"] = "unrelated-valkey-name"
        with self.assertRaises(common.ReleaseError):
            self.build(values=wrong_name)
        wrong_identity = observations()
        wrong_identity["valkey-cluster"]["database"]["id"] = (
            "77777777-7777-4777-8777-777777777777"
        )
        with self.assertRaises(common.ReleaseError):
            self.build(values=wrong_identity)

    def test_pretty_checked_in_contract_is_raw_hash_bound_but_artifacts_stay_canonical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            pretty_path = root / "production-app-contract.json"
            pretty_raw = json.dumps(contract(), indent=2).encode("utf-8") + b"\n"
            pretty_path.write_bytes(pretty_raw)
            self.assertEqual(
                recovery._load_raw_bound(
                    pretty_path,
                    common.sha256_bytes(pretty_raw),
                    "production app contract",
                ),
                contract(),
            )
            with self.assertRaises(common.ReleaseError):
                recovery._load_exact(
                    pretty_path,
                    common.sha256_bytes(pretty_raw),
                    "production plan",
                )
            with self.assertRaises(common.ReleaseError):
                recovery._load_raw_bound(
                    pretty_path,
                    "0" * 64,
                    "production app contract",
                )

    def test_builds_sanitized_v2_double_read_readiness(self) -> None:
        value = self.build()
        self.assertEqual(value["schema_version"], 2)
        self.assertEqual(value["provider"]["http_request_count"], 17)
        self.assertEqual(
            value["provider"]["http_endpoint_labels"],
            [recovery.DISCOVERY_LABEL] + recovery.STABLE_LABELS * 2,
        )
        self.assertTrue(value["gates"]["provider_fork_bound"])
        self.assertTrue(
            value["valkey"]["provider_fork"][
                "recovery_restricted_to_exact_production_app"
            ]
        )
        self.assertNotEqual(
            value["valkey"]["provider_fork"]["source_firewall_sha256"],
            value["valkey"]["provider_fork"]["recovery_firewall_sha256"],
        )
        encoded = common.canonical_file_bytes(value)
        for private in (
            *TARGET.values(), RECOVERY_ID, FORK_NAME, *DATABASE_HOSTS.values(),
            PRIVATE_NETWORK_UUID, SOURCE_RULE_UUID, RECOVERY_RULE_UUID, APP_ID,
        ):
            self.assertNotIn(private.encode("utf-8"), encoded)

    def test_exact_client_discovers_restricted_fork_then_performs_only_17_gets(self) -> None:
        values = observations()
        client = recovery.DatabaseReadClient(TARGET, "t" * 24, opener=FakeOpener([]))
        queue: list[tuple[object, str]] = [
            (discovery(), common.API_ORIGIN + "/v2/databases?page=1&per_page=200")
        ]
        # Populate paths through an isolated discovery client using the same fixture.
        discovery_opener = FakeOpener(queue)
        client = recovery.DatabaseReadClient(TARGET, "t" * 24, opener=discovery_opener)
        self.assertEqual(client.discover_fork(FORK_NAME), RECOVERY_ID)
        stable_queue = [
            (values[label], common.API_ORIGIN + client.paths[label])
            for label in recovery.STABLE_LABELS
        ] * 2
        client._opener = FakeOpener(stable_queue)
        first = {label: client.get_label(label) for label in recovery.STABLE_LABELS}
        second = {label: client.get_label(label) for label in recovery.STABLE_LABELS}
        self.assertEqual(first, second)
        self.assertEqual(client.request_log, self.ledger())

    def test_observe_binds_plan_receipt_discovery_and_hides_fork_identity(self) -> None:
        values = observations()
        fork_receipt = receipt(values)
        fork_auth = receipt_authority(fork_receipt)
        plan = production_plan()
        probe = recovery.DatabaseReadClient(TARGET, "t" * 24, opener=FakeOpener([]))
        # Mirror the deterministic paths without doing a real read.
        probe.recovery_fork_id = RECOVERY_ID
        probe.recovery_fork_name = FORK_NAME
        postgres = TARGET["postgres_cluster_id"]
        source = TARGET["valkey_cluster_id"]
        probe.paths = {
            "postgres-cluster": f"/v2/databases/{postgres}",
            "postgres-backups": f"/v2/databases/{postgres}/backups?page=1&per_page=200",
            "valkey-cluster": f"/v2/databases/{source}",
            "valkey-config": f"/v2/databases/{source}/config",
            "valkey-source-firewall": f"/v2/databases/{source}/firewall",
            "valkey-recovery-cluster": f"/v2/databases/{RECOVERY_ID}",
            "valkey-recovery-config": f"/v2/databases/{RECOVERY_ID}/config",
            "valkey-recovery-firewall": f"/v2/databases/{RECOVERY_ID}/firewall",
        }
        queue = [
            (discovery(), common.API_ORIGIN + "/v2/databases?page=1&per_page=200")
        ] + [
            (values[label], common.API_ORIGIN + probe.paths[label])
            for label in recovery.STABLE_LABELS
        ] * 2
        opener = FakeOpener(queue)
        result = recovery.observe(
            target=TARGET,
            control=CONTROL,
            token="t" * 24,
            contract_sha256="b" * 64,
            controller_sha256="c" * 64,
            contract=contract(),
            production_plan=plan,
            production_plan_authority=plan_authority(plan),
            fork_receipt=fork_receipt,
            fork_receipt_sha256=fork_auth["sha256"],
            fork_authority=fork_auth,
            now=NOW,
            opener=opener,
        )
        self.assertEqual(len(opener.requests), 17)
        self.assertEqual({request.method for request in opener.requests}, {"GET"})
        self.assertEqual(result["provider"]["mutation_request_count"], 0)
        self.assertEqual(
            result["authorities"]["production_plan"], plan_authority(plan)
        )
        self.assertEqual(result["authorities"]["valkey_fork"]["sha256"], fork_auth["sha256"])

    def test_discovery_is_exact_unique_and_complete(self) -> None:
        for value in (
            {"databases": [], "meta": {"total": 0}},
            discovery(duplicate=True),
            {**discovery(), "meta": {"total": 4}},
        ):
            with self.subTest(value=value):
                opener = FakeOpener(
                    [(value, common.API_ORIGIN + "/v2/databases?page=1&per_page=200")]
                )
                client = recovery.DatabaseReadClient(TARGET, "t" * 24, opener=opener)
                with self.assertRaises(common.ReleaseError):
                    client.discover_fork(FORK_NAME)

    def test_double_read_and_exact_ledger_fail_closed(self) -> None:
        first = observations()
        second = copy.deepcopy(first)
        second["valkey-config"]["config"]["maxmemory_policy"] = "noeviction"
        with self.assertRaises(common.ReleaseError):
            self.build(first=first, second=second)
        with self.assertRaises(common.ReleaseError):
            self.build(ledger=self.ledger()[:-1])

    def test_receipt_v1_ambiguity_phase_and_run_drift_fail_closed(self) -> None:
        cases: list[dict[str, object]] = []
        legacy = receipt()
        legacy["schema_version"] = 1
        cases.append(legacy)
        ambiguous = receipt()
        ambiguous["result"]["outcome"] = "reconciled"
        ambiguous["result"]["mutation_ambiguous_reconciled"] = True
        cases.append(ambiguous)
        wrong_phase = receipt()
        wrong_phase["phase"] = "ui"
        cases.append(wrong_phase)
        wrong_path = receipt()
        wrong_path["control"]["workflow_path"] = ".github/workflows/untrusted.yml"
        cases.append(wrong_path)
        wrong_controller = receipt()
        wrong_controller["control"]["controller_sha256"] = "0" * 64
        cases.append(wrong_controller)
        wrong_descriptor = receipt()
        wrong_descriptor["target"]["descriptor_sha256"] = "0" * 64
        cases.append(wrong_descriptor)
        for value in cases:
            with self.subTest(value=value):
                with self.assertRaises(common.ReleaseError):
                    self.fork_evidence(fork_receipt=value)

    def test_receipt_must_match_stable_source_and_fork_observation(self) -> None:
        base = receipt()
        for section, key in (
            ("target", "source_observation_sha256"),
            ("target", "source_config_sha256"),
            ("target", "source_firewall_sha256"),
            ("result", "recovery_observation_sha256"),
            ("result", "recovery_topology_sha256"),
            ("result", "recovery_config_sha256"),
            ("result", "recovery_firewall_sha256"),
        ):
            changed = copy.deepcopy(base)
            changed[section][key] = "0" * 64
            with self.subTest(section=section, key=key):
                with self.assertRaises(common.ReleaseError):
                    self.build(fork_receipt=changed)

    def test_non_rdb_topology_firewall_policy_and_stale_fork_fail(self) -> None:
        cases: list[dict[str, object]] = []
        non_rdb = observations()
        non_rdb["valkey-recovery-config"]["config"]["redis_persistence"] = "off"
        cases.append(non_rdb)
        topology = observations()
        topology["valkey-recovery-cluster"]["database"]["size"] = "db-s-2vcpu-4gb"
        cases.append(topology)
        wrong_app = observations()
        wrong_app["valkey-recovery-firewall"]["rules"][0]["value"] = "other-app"
        cases.append(wrong_app)
        public = observations()
        public["valkey-recovery-firewall"]["rules"] = []
        cases.append(public)
        multiple = observations()
        multiple["valkey-recovery-firewall"]["rules"].append(
            {"type": "ip_addr", "value": "192.0.2.10"}
        )
        cases.append(multiple)
        stale = observations()
        stale["valkey-recovery-cluster"]["database"]["created_at"] = "2026-08-20T00:00:00Z"
        cases.append(stale)
        for value in cases:
            with self.subTest(value=value):
                with self.assertRaises(common.ReleaseError):
                    self.build(values=value)

    def test_backup_size_age_and_progress_fail_closed(self) -> None:
        cases: list[dict[str, object]] = []
        for size in (None, 0, -1, True, "1", decimal.Decimal("NaN")):
            value = observations()
            if size is None:
                del value["postgres-backups"]["backups"][0]["size_gigabytes"]
            else:
                value["postgres-backups"]["backups"][0]["size_gigabytes"] = size
            cases.append(value)
        stale = observations()
        stale["postgres-backups"]["backups"][0]["created_at"] = "2026-08-20T00:00:00Z"
        cases.append(stale)
        progress = observations()
        progress["postgres-backups"]["backup_progress"] = {"created_at": "2026-08-26T23:59:00Z"}
        cases.append(progress)
        for value in cases:
            with self.subTest(value=value):
                with self.assertRaises(common.ReleaseError):
                    self.build(values=value)

    def test_apply_shared_validator_accepts_v2_and_rejects_v1(self) -> None:
        contract_raw = common.canonical_file_bytes(contract())
        contract_hash = common.sha256_bytes(contract_raw)
        value = self.build(contract_sha256=contract_hash)
        value_hash = common.sha256_bytes(common.canonical_file_bytes(value))
        with tempfile.TemporaryDirectory() as temporary:
            contract_path = Path(temporary) / "production-app-contract.json"
            contract_path.write_bytes(contract_raw)
            apply.validate_recovery(value, value_hash, NOW, contract_path=contract_path)
            legacy = copy.deepcopy(value)
            legacy["schema_version"] = 1
            with self.assertRaises(common.ReleaseError):
                apply.validate_recovery(
                    legacy,
                    common.sha256_bytes(common.canonical_file_bytes(legacy)),
                    NOW,
                    contract_path=contract_path,
                )

    def test_legacy_sentinel_surface_is_removed(self) -> None:
        source = Path(recovery.__file__).read_text(encoding="utf-8")
        for forbidden in (
            "SENTINEL_KEY", "VALKEY_SENTINEL", "read_valkey_sentinel", "socket.",
            "ssl.", "hmac.", "source_sentinel_connection", "recovery_sentinel_connection",
        ):
            self.assertNotIn(forbidden, source)
        descriptor = common.validate_target_descriptor(TARGET, recovery=True)
        self.assertEqual(descriptor, TARGET)


if __name__ == "__main__":
    unittest.main()
