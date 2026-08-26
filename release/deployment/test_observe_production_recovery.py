from __future__ import annotations

import copy
import base64
import datetime as dt
import decimal
import hashlib
import hmac
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import observe_production_recovery as recovery
import apply_production_change as apply
import verify_production_release as common


NOW = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
TARGET = {
    "postgres_cluster_id": "11111111-1111-4111-8111-111111111111",
    "valkey_cluster_id": "22222222-2222-4222-8222-222222222222",
    "valkey_recovery_cluster_id": "33333333-3333-4333-8333-333333333333",
}
CONTROL = {
    "workflow_sha": "a" * 40,
    "workflow_path": recovery.WORKFLOW_PATH,
    "run_id": "101",
    "run_attempt": 1,
    "runner_environment": "github-hosted",
}
DATABASE_NAMES = {
    "postgres": "test-postgres-cluster",
    "valkey": "test-valkey-cluster",
    "recovery": "test-valkey-recovery",
}
DATABASE_HOSTS = {
    "postgres": "test-postgres.db.ondigitalocean.com",
    "valkey": "test-valkey.db.ondigitalocean.com",
    "recovery": "test-valkey-recovery.db.ondigitalocean.com",
}
HMAC_KEY = b"k" * 32
HMAC_KEY_B64 = base64.b64encode(HMAC_KEY).decode("ascii")
PRIVATE_NETWORK_UUID = "44444444-4444-4444-8444-444444444444"


def database(identity: str, engine: str, created_at: str, name: str, host: str) -> dict[str, object]:
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
        "postgres-cluster": database(TARGET["postgres_cluster_id"], "pg", "2025-01-01T00:00:00Z", DATABASE_NAMES["postgres"], DATABASE_HOSTS["postgres"]),
        "postgres-backups": {"backups": [{"created_at": "2026-08-26T23:30:00Z", "size_gigabytes": 1}]},
        "valkey-cluster": database(TARGET["valkey_cluster_id"], "valkey", "2025-01-01T00:00:00Z", DATABASE_NAMES["valkey"], DATABASE_HOSTS["valkey"]),
        "valkey-config": {"config": {"redis_persistence": "rdb"}},
        "valkey-recovery-cluster": database(TARGET["valkey_recovery_cluster_id"], "valkey", "2026-08-26T23:00:00Z", DATABASE_NAMES["recovery"], DATABASE_HOSTS["recovery"]),
        "valkey-recovery-config": {"config": {"redis_persistence": "rdb"}},
    }


def contract() -> dict[str, object]:
    return {
        "expected_topology": {
            "region": "sgp",
            "databases": [
                {
                    "engine": "PG", "version": "17", "production": True,
                    "name_sha256": common.sha256_bytes(b"postgres-binding"),
                    "cluster_sha256": common.sha256_bytes(DATABASE_NAMES["postgres"].encode("utf-8")),
                },
                {
                    "engine": "VALKEY", "version": "8", "production": True,
                    "name_sha256": common.sha256_bytes(b"valkey-binding"),
                    "cluster_sha256": common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
                },
            ]
        }
    }


def connection(kind: str) -> dict[str, object]:
    return {
        "host": DATABASE_HOSTS[kind], "port": 25061, "server_name": DATABASE_HOSTS[kind],
        "username": "sentinel-reader", "password": "read-only-password",
    }


def marker(issued_at: str = "2026-08-26T22:55:00Z") -> bytes:
    payload = {
        "authority": recovery.SENTINEL_AUTHORITY,
        "issued_at": issued_at,
        "nonce": "a" * 64,
        "source_identity_sha256": common.sha256_bytes(TARGET["valkey_cluster_id"].encode("utf-8")),
        "source_cluster_sha256": common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
    }
    payload["hmac_sha256"] = hmac.new(
        HMAC_KEY, common.canonical_payload_bytes(payload), hashlib.sha256
    ).hexdigest()
    return common.canonical_payload_bytes(payload)


def sentinel_proof() -> dict[str, object]:
    raw = marker()
    return {
        "authority": recovery.SENTINEL_AUTHORITY,
        "marker_key_sha256": common.sha256_bytes(recovery.SENTINEL_KEY.encode("utf-8")),
        "marker_sha256": common.sha256_bytes(raw),
        "source_recovery_equal": True,
        "live_read_count": 2,
        "issued_at_sha256": common.sha256_bytes(b"2026-08-26T22:55:00Z"),
        "source_endpoint_sha256": common.sha256_value({"host": DATABASE_HOSTS["valkey"], "port": 25061}),
        "recovery_endpoint_sha256": common.sha256_value({"host": DATABASE_HOSTS["recovery"], "port": 25061}),
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
        labels = [
            "postgres-cluster", "postgres-backups", "valkey-cluster", "valkey-config",
            "valkey-recovery-cluster", "valkey-recovery-config",
        ]
        return [("GET", label) for label in labels] * 2

    def test_valkey_request_writer_completes_short_writes_and_rejects_zero(self) -> None:
        class Writer:
            def __init__(self, maximum: int) -> None:
                self.maximum = maximum
                self.value = bytearray()

            def write(self, value: bytes) -> int:
                amount = min(self.maximum, len(value))
                self.value.extend(value[:amount])
                return amount

        partial = Writer(3)
        recovery._write_exact(partial, b"exact-request")
        self.assertEqual(bytes(partial.value), b"exact-request")
        with self.assertRaises(common.ReleaseError):
            recovery._write_exact(Writer(0), b"blocked")

    def test_provider_parser_preserves_fractional_size_but_rejects_unsafe_json(self) -> None:
        value = recovery.loads_provider_json(
            b'{"backups":[{"size_gigabytes":0.03364864}]}'
        )
        self.assertEqual(
            value["backups"][0]["size_gigabytes"],
            decimal.Decimal("0.03364864"),
        )
        for raw in (b'{"x":1,"x":2}', b'{"x":NaN}', b'{"x":Infinity}'):
            with self.subTest(raw=raw):
                with self.assertRaises(common.ReleaseError):
                    recovery.loads_provider_json(raw)

    def build(
        self,
        value: dict[str, object] | None = None,
        ledger: list[tuple[str, str]] | None = None,
        contract_sha256: str = "b" * 64,
    ) -> dict[str, object]:
        first = value or observations()
        return recovery.build_readiness(
            target=TARGET,
            control=CONTROL,
            first=first,
            second=copy.deepcopy(first),
            request_log=self.ledger() if ledger is None else ledger,
            now=NOW,
            contract_sha256=contract_sha256,
            controller_sha256="c" * 64,
            contract_databases=recovery.contract_database_bindings(contract()),
            sentinel_proof=sentinel_proof(),
        )

    def test_builds_sanitized_double_read_readiness(self) -> None:
        value = self.build()
        self.assertEqual(value["provider"]["http_request_count"], 12)
        self.assertTrue(value["gates"]["postgresql_ready"])
        self.assertTrue(value["gates"]["valkey_ready"])
        encoded = common.canonical_file_bytes(value)
        for private in TARGET.values():
            self.assertNotIn(private.encode("ascii"), encoded)

    def test_single_six_get_pass_is_rejected(self) -> None:
        with self.assertRaises(common.ReleaseError):
            self.build(ledger=self.ledger()[:6])

    def test_stale_backup_non_rdb_and_stale_fork_fail_closed(self) -> None:
        cases = []
        stale_backup = observations()
        stale_backup["postgres-backups"]["backups"][0]["created_at"] = "2026-08-20T00:00:00Z"
        cases.append(stale_backup)
        non_rdb = observations()
        non_rdb["valkey-config"]["config"]["redis_persistence"] = "off"
        cases.append(non_rdb)
        stale_fork = observations()
        stale_fork["valkey-recovery-cluster"]["database"]["created_at"] = "2026-08-20T00:00:00Z"
        cases.append(stale_fork)
        for value in cases:
            with self.subTest(value=value):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=value)

    def test_backup_size_and_in_progress_backup_fail_closed(self) -> None:
        cases = []
        for size in (
            None,
            0,
            -1,
            True,
            "1",
            decimal.Decimal("NaN"),
            decimal.Decimal("Infinity"),
        ):
            value = observations()
            if size is None:
                del value["postgres-backups"]["backups"][0]["size_gigabytes"]
            else:
                value["postgres-backups"]["backups"][0]["size_gigabytes"] = size
            cases.append(value)
        in_progress = observations()
        in_progress["postgres-backups"]["backup_progress"] = {
            "created_at": "2026-08-26T23:59:00Z"
        }
        cases.append(in_progress)
        for value in cases:
            with self.subTest(value=value):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=value)

    def test_exact_client_disables_mutation_and_uses_only_allowlisted_urls(self) -> None:
        target = common.validate_target_descriptor(TARGET, recovery=True)
        probe = recovery.DatabaseReadClient(target, "t" * 24, opener=FakeOpener([]))
        values = observations()
        values["postgres-backups"]["backups"][0]["size_gigabytes"] = 0.03364864
        queue = [(values[label], common.API_ORIGIN + path) for label, path in probe.paths.items()] * 2
        opener = FakeOpener(queue)
        result = recovery.observe(
            target=target,
            control=CONTROL,
            token="t" * 24,
            contract_sha256="b" * 64,
            controller_sha256="c" * 64,
            contract=contract(),
            source_sentinel_connection=connection("valkey"),
            recovery_sentinel_connection=connection("recovery"),
            sentinel_hmac_key_b64=HMAC_KEY_B64,
            now=NOW,
            opener=opener,
            sentinel_reader=lambda _connection: marker(),
        )
        self.assertEqual(len(opener.requests), 12)
        self.assertEqual({request.method for request in opener.requests}, {"GET"})
        self.assertEqual(result["provider"]["mutation_request_count"], 0)
        encoded = common.canonical_file_bytes(result)
        for private in (
            "sentinel-reader", "read-only-password", HMAC_KEY_B64,
            DATABASE_HOSTS["valkey"], DATABASE_HOSTS["recovery"],
            PRIVATE_NETWORK_UUID,
        ):
            self.assertNotIn(private.encode("utf-8"), encoded)

    def test_observation_change_and_provider_403_fail_closed(self) -> None:
        first = observations()
        second = copy.deepcopy(first)
        second["valkey-config"]["config"]["redis_persistence"] = "off"
        with self.assertRaises(common.ReleaseError):
            recovery.build_readiness(
                target=TARGET,
                control=CONTROL,
                first=first,
                second=second,
                request_log=self.ledger(),
                now=NOW,
                contract_sha256="b" * 64,
                controller_sha256="c" * 64,
                contract_databases=recovery.contract_database_bindings(contract()),
                sentinel_proof=sentinel_proof(),
            )

    def test_unrelated_healthy_production_clusters_fail_contract_binding(self) -> None:
        for label in ("postgres-cluster", "valkey-cluster"):
            unrelated = observations()
            unrelated[label]["database"]["name"] = "healthy-but-unrelated"
            with self.subTest(label=label):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=unrelated)

    def test_provider_database_versions_must_match_the_exact_contract(self) -> None:
        for label in (
            "postgres-cluster",
            "valkey-cluster",
            "valkey-recovery-cluster",
        ):
            drifted = observations()
            drifted[label]["database"]["version"] = "unexpected-version"
            with self.subTest(label=label):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=drifted)

    def test_provider_regions_and_valkey_recovery_capacity_must_match(self) -> None:
        unmapped = contract()
        unmapped["expected_topology"]["region"] = "unreviewed"
        with self.assertRaises(common.ReleaseError):
            recovery.contract_database_bindings(unmapped)
        no_additional_storage = observations()
        no_additional_storage["valkey-cluster"]["database"]["storage_size_mib"] = None
        no_additional_storage["valkey-recovery-cluster"]["database"]["storage_size_mib"] = None
        self.build(value=no_additional_storage)
        for label in (
            "postgres-cluster",
            "valkey-cluster",
            "valkey-recovery-cluster",
        ):
            drifted = observations()
            drifted[label]["database"]["region"] = "nyc3"
            with self.subTest(region_label=label):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=drifted)
        mutations = {
            "size": "db-s-2vcpu-4gb",
            "num_nodes": 2,
            "private_network_uuid": "55555555-5555-4555-8555-555555555555",
            "storage_size_mib": 20480,
        }
        for key, changed in mutations.items():
            drifted = observations()
            drifted["valkey-recovery-cluster"]["database"][key] = changed
            with self.subTest(topology_key=key):
                with self.assertRaises(common.ReleaseError):
                    self.build(value=drifted)

    def test_fresh_empty_or_unrelated_recovery_cluster_fails_live_sentinel(self) -> None:
        values = observations()
        source = recovery._database(
            values["valkey-cluster"], TARGET["valkey_cluster_id"], {"valkey"}, "Valkey",
            expected_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
        )
        fork = recovery._database(
            values["valkey-recovery-cluster"], TARGET["valkey_recovery_cluster_id"], {"valkey"}, "Valkey recovery fork"
        )
        for recovery_value in (b"", marker().replace(b'"nonce":"a', b'"nonce":"b', 1)):
            reads = iter([marker(), recovery_value])
            with self.subTest(recovery_value=recovery_value):
                with self.assertRaises(common.ReleaseError):
                    recovery.build_live_sentinel_proof(
                        source_connection=connection("valkey"), recovery_connection=connection("recovery"),
                        source_database=source, recovery_database=fork,
                        source_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
                        hmac_key_b64=HMAC_KEY_B64, now=NOW, reader=lambda _connection: next(reads),
                    )

    def test_post_fork_or_invalid_hmac_sentinel_fails_closed(self) -> None:
        values = observations()
        source = recovery._database(
            values["valkey-cluster"], TARGET["valkey_cluster_id"], {"valkey"}, "Valkey",
            expected_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
        )
        fork = recovery._database(
            values["valkey-recovery-cluster"], TARGET["valkey_recovery_cluster_id"], {"valkey"}, "Valkey recovery fork"
        )
        bad_hmac = common.loads_strict(marker())
        bad_hmac["hmac_sha256"] = "0" * 64
        for raw in (
            marker("2026-08-26T23:00:00Z"),
            marker("2026-08-26T23:30:00Z"),
            common.canonical_payload_bytes(bad_hmac),
        ):
            with self.subTest(raw=raw):
                with self.assertRaises(common.ReleaseError):
                    recovery.build_live_sentinel_proof(
                        source_connection=connection("valkey"), recovery_connection=connection("recovery"),
                        source_database=source, recovery_database=fork,
                        source_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
                        hmac_key_b64=HMAC_KEY_B64, now=NOW, reader=lambda _connection, value=raw: value,
                    )

    def test_sentinel_endpoint_and_hmac_key_must_be_protected_and_provider_bound(self) -> None:
        values = observations()
        source = recovery._database(
            values["valkey-cluster"], TARGET["valkey_cluster_id"], {"valkey"}, "Valkey",
            expected_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
        )
        fork = recovery._database(
            values["valkey-recovery-cluster"], TARGET["valkey_recovery_cluster_id"], {"valkey"}, "Valkey recovery fork"
        )
        unrelated = connection("valkey")
        unrelated["host"] = unrelated["server_name"] = "unrelated.db.ondigitalocean.com"
        cases = (
            (unrelated, connection("recovery"), HMAC_KEY_B64),
            (connection("valkey"), connection("recovery"), ""),
        )
        for source_connection, recovery_connection, key in cases:
            with self.subTest(source_connection=source_connection, key=bool(key)):
                with self.assertRaises(common.ReleaseError):
                    recovery.build_live_sentinel_proof(
                        source_connection=source_connection, recovery_connection=recovery_connection,
                        source_database=source, recovery_database=fork,
                        source_cluster_sha256=common.sha256_bytes(DATABASE_NAMES["valkey"].encode("utf-8")),
                        hmac_key_b64=key, now=NOW, reader=lambda _connection: marker(),
                    )
        same_fork = copy.deepcopy(fork)
        same_fork["connection_endpoints"] = set(source["connection_endpoints"])
        with self.assertRaises(common.ReleaseError):
            recovery.build_live_sentinel_proof(
                source_connection=connection("valkey"),
                recovery_connection=connection("valkey"),
                source_database=source,
                recovery_database=same_fork,
                source_cluster_sha256=common.sha256_bytes(
                    DATABASE_NAMES["valkey"].encode("utf-8")
                ),
                hmac_key_b64=HMAC_KEY_B64,
                now=NOW,
                reader=lambda _connection: marker(),
            )

    def test_apply_and_rollback_shared_validator_bind_exact_contract_descriptor_and_sentinel(self) -> None:
        contract_raw = common.canonical_file_bytes(contract())
        contract_hash = common.sha256_bytes(contract_raw)
        value = self.build(contract_sha256=contract_hash)
        value_hash = common.sha256_bytes(common.canonical_file_bytes(value))
        with tempfile.TemporaryDirectory() as temporary:
            contract_path = Path(temporary) / "production-app-contract.json"
            contract_path.write_bytes(contract_raw)
            apply.validate_recovery(value, value_hash, NOW, contract_path=contract_path)
            cases = []
            descriptor = copy.deepcopy(value)
            descriptor["target"]["descriptor_sha256"] = "0" * 64
            cases.append(descriptor)
            identity = copy.deepcopy(value)
            identity["target"]["valkey_identity_sha256"] = "1" * 64
            cases.append(identity)
            sentinel = copy.deepcopy(value)
            sentinel["valkey"]["live_recovery_sentinel"]["source_recovery_equal"] = False
            cases.append(sentinel)
            marker_key = copy.deepcopy(value)
            marker_key["valkey"]["live_recovery_sentinel"]["marker_key_sha256"] = "2" * 64
            cases.append(marker_key)
            sentinel_endpoint = copy.deepcopy(value)
            sentinel_endpoint["valkey"]["live_recovery_sentinel"]["recovery_endpoint_sha256"] = (
                sentinel_endpoint["valkey"]["live_recovery_sentinel"]["source_endpoint_sha256"]
            )
            cases.append(sentinel_endpoint)
            postgres_version = copy.deepcopy(value)
            postgres_version["postgresql"]["version"] = "unexpected-version"
            cases.append(postgres_version)
            valkey_version = copy.deepcopy(value)
            valkey_version["valkey"]["version"] = "unexpected-version"
            cases.append(valkey_version)
            region = copy.deepcopy(value)
            region["valkey"]["recovery_region_sha256"] = "3" * 64
            cases.append(region)
            topology = copy.deepcopy(value)
            topology["valkey"]["recovery_topology_sha256"] = "4" * 64
            cases.append(topology)
            for tampered in cases:
                with self.subTest(tampered=tampered):
                    with self.assertRaises(common.ReleaseError):
                        apply.validate_recovery(
                            tampered,
                            common.sha256_bytes(common.canonical_file_bytes(tampered)),
                            NOW,
                            contract_path=contract_path,
                        )

    def test_recovery_workflow_keeps_protected_descriptors_out_of_argv(self) -> None:
        workflow = (Path(__file__).resolve().parents[2] / ".github" / "workflows" / "verify-production-recovery-readiness.yml").read_text(encoding="utf-8")
        self.assertNotIn('--target "$TARGET_JSON"', workflow)
        self.assertIn('DO_PRODUCTION_DATABASE_TARGET_JSON="$TARGET_JSON"', workflow)
        self.assertIn("DO_PRODUCTION_VALKEY_SENTINEL_SOURCE_JSON", workflow)
        self.assertIn("a.validate_recovery", workflow)


if __name__ == "__main__":
    unittest.main()
