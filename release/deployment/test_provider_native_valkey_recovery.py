from __future__ import annotations

import copy
import datetime as dt
import json
import subprocess
import sys
import unittest
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))
import provider_native_valkey_recovery as fork
import verify_production_release as common


NOW = dt.datetime(2026, 8, 30, 0, 0, 0, tzinfo=dt.timezone.utc)
SOURCE_ID = "11111111-1111-4111-8111-111111111111"
POSTGRES_ID = "22222222-2222-4222-8222-222222222222"
FORK_ID = "33333333-3333-4333-8333-333333333333"
VPC_ID = "44444444-4444-4444-8444-444444444444"
RULE_ID = "55555555-5555-4555-8555-555555555555"
APP_ID = "66666666-6666-4666-8666-666666666666"
FORK_RULE_ID = "77777777-7777-4777-8777-777777777777"
SOURCE_NAME = "synthetic-valkey-source"
TARGET = {"postgres_cluster_id": POSTGRES_ID, "valkey_cluster_id": SOURCE_ID}
PHASE = "baseline"


def contract() -> dict[str, Any]:
    return {
        "provider": {
            "app_id_sha256": common.sha256_bytes(APP_ID.encode("utf-8")),
        },
        "expected_topology": {
            "region": "sgp",
            "vpc_id_sha256": common.sha256_bytes(VPC_ID.encode("utf-8")),
            "databases": [
                {
                    "engine": "PG",
                    "version": "17",
                    "production": True,
                    "name_sha256": common.sha256_bytes(b"synthetic-pg-binding"),
                    "cluster_sha256": common.sha256_bytes(b"synthetic-pg-source"),
                },
                {
                    "engine": "VALKEY",
                    "version": "8",
                    "production": True,
                    "name_sha256": common.sha256_bytes(b"synthetic-valkey-binding"),
                    "cluster_sha256": common.sha256_bytes(
                        SOURCE_NAME.encode("utf-8")
                    ),
                },
            ],
        }
    }


def control(
    workflow_path: str = fork.PREPARE_WORKFLOW_PATH, run_id: str = "7001"
) -> dict[str, Any]:
    value = contract()
    result = {
        "workflow_sha": "a" * 40,
        "workflow_path": workflow_path,
        "run_id": run_id,
        "run_attempt": 1,
        "runner_environment": "github-hosted",
        "rollout_plan_sha256": "b" * 64,
        "contract_sha256": common.sha256_bytes(common.canonical_file_bytes(value)),
        "controller_sha256": "c" * 64,
    }
    if workflow_path == fork.CLEANUP_WORKFLOW_PATH:
        result.update(
            {
                "authority_workflow_sha": "a" * 40,
                "authority_controller_sha256": "c" * 64,
            }
        )
    return result


def database(
    identity: str,
    name: str,
    *,
    status: str = "online",
    created_at: str = "2026-08-30T00:00:00Z",
) -> dict[str, Any]:
    return {
        "id": identity,
        "name": name,
        "engine": "valkey",
        "version": "8",
        "region": "sgp1",
        "size": "db-s-1vcpu-1gb",
        "num_nodes": 1,
        "status": status,
        "created_at": created_at,
        "private_network_uuid": VPC_ID,
        "storage_size_mib": 10240,
        # Credentials may be returned by broader tokens; they must never enter
        # a projection or public receipt.
        "connection": {
            "host": "synthetic.invalid",
            "port": 25061,
            "user": "doadmin",
            "password": "private-test-password",
        },
    }


def envelope(value: dict[str, Any]) -> dict[str, Any]:
    return {"database": copy.deepcopy(value)}


CONFIG = {
    "config": {
        "valkey_persistence": "rdb",
        "valkey_maxmemory_policy": "allkeys-lru",
    }
}
SOURCE_FIREWALL = {
    "rules": [
        {
            "type": "app",
            "value": APP_ID,
            "cluster_uuid": SOURCE_ID,
            "uuid": RULE_ID,
            "created_at": "2026-08-01T00:00:00Z",
            "description": "synthetic recovery application",
        }
    ]
}
FORK_FIREWALL = {
    "rules": [
        {
            "type": "app",
            "value": APP_ID,
            "cluster_uuid": FORK_ID,
            "uuid": FORK_RULE_ID,
            "created_at": "2026-08-30T00:00:00Z",
            "description": "synthetic application",
        }
    ]
}
EMPTY_FIREWALL = {"rules": []}


class FakeTransport:
    def __init__(self, steps: list[tuple[str, str, object]]) -> None:
        self.steps = list(steps)
        self.calls: list[tuple[str, str, bytes | None]] = []

    def request(
        self, method: str, path: str, body: bytes | None = None
    ) -> fork.APIResult:
        self.calls.append((method, path, body))
        if not self.steps:
            raise AssertionError(f"unexpected provider request {method} {path}")
        expected_method, expected_path, result = self.steps.pop(0)
        if (method, path) != (expected_method, expected_path):
            raise AssertionError(
                f"expected {expected_method} {expected_path}, got {method} {path}"
            )
        if isinstance(result, BaseException):
            raise result
        if not isinstance(result, fork.APIResult):
            raise AssertionError("fake result is invalid")
        return result


class MethodBoundTransport:
    def __init__(self, shared: FakeTransport, methods: set[str]) -> None:
        self.shared = shared
        self.methods = methods
        self.calls: list[tuple[str, str, bytes | None]] = []

    def request(
        self, method: str, path: str, body: bytes | None = None
    ) -> fork.APIResult:
        if method not in self.methods:
            raise AssertionError("provider capability received a forbidden method")
        self.calls.append((method, path, body))
        return self.shared.request(method, path, body)


class FakeCapabilities:
    def __init__(self, steps: list[tuple[str, str, object]]) -> None:
        self.shared = FakeTransport(steps)
        self.read = MethodBoundTransport(self.shared, {"GET"})
        self.mutation = MethodBoundTransport(self.shared, {"POST", "DELETE"})

    @property
    def calls(self) -> list[tuple[str, str, bytes | None]]:
        return self.shared.calls

    @property
    def steps(self) -> list[tuple[str, str, object]]:
        return self.shared.steps


class FakeHTTPResponse:
    def __init__(
        self,
        *,
        status: int,
        url: str,
        content_type: str,
        raw: bytes,
    ) -> None:
        self.status = status
        self._url = url
        self.headers = {"Content-Type": content_type}
        self._raw = raw

    def __enter__(self) -> "FakeHTTPResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def geturl(self) -> str:
        return self._url

    def read(self, _maximum: int) -> bytes:
        return self._raw


class FakeOpener:
    def __init__(self, response: FakeHTTPResponse) -> None:
        self.response = response
        self.calls = 0

    def open(self, _request: object, timeout: int) -> FakeHTTPResponse:
        self.calls += 1
        if timeout != 20:
            raise AssertionError("provider timeout differs")
        return self.response


def ok(value: object) -> fork.APIResult:
    return fork.APIResult(200, copy.deepcopy(value))


def create_steps(fork_name: str) -> list[tuple[str, str, object]]:
    source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
    recovery = database(FORK_ID, fork_name)
    return [
        ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
        ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(CONFIG)),
        ("GET", f"/v2/databases/{SOURCE_ID}/firewall", ok(SOURCE_FIREWALL)),
        ("GET", fork.LIST_PATH, ok({"databases": [source], "meta": {"total": 1}})),
        ("POST", fork.DATABASES_PATH, fork.APIResult(201, envelope(recovery))),
        ("GET", f"/v2/databases/{FORK_ID}", ok(envelope(recovery))),
        ("GET", f"/v2/databases/{FORK_ID}/config", ok(CONFIG)),
        ("GET", f"/v2/databases/{FORK_ID}/firewall", ok(FORK_FIREWALL)),
        ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
        ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(CONFIG)),
        ("GET", f"/v2/databases/{SOURCE_ID}/firewall", ok(SOURCE_FIREWALL)),
    ]


def source_read_steps(
    *,
    source: dict[str, Any] | None = None,
    config: dict[str, Any] | None = None,
    firewall: dict[str, Any] | None = None,
) -> list[tuple[str, str, object]]:
    source = source or database(
        SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
    )
    return [
        ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
        ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(config or CONFIG)),
        (
            "GET",
            f"/v2/databases/{SOURCE_ID}/firewall",
            ok(firewall or SOURCE_FIREWALL),
        ),
    ]


def inventory_result(*databases: dict[str, Any]) -> fork.APIResult:
    return ok(
        {"databases": list(databases), "meta": {"total": len(databases)}}
    )


def cleanup_round_steps(
    *databases: dict[str, Any],
    source: dict[str, Any] | None = None,
    config: dict[str, Any] | None = None,
    firewall: dict[str, Any] | None = None,
) -> list[tuple[str, str, object]]:
    return source_read_steps(
        source=source, config=config, firewall=firewall
    ) + [("GET", fork.LIST_PATH, inventory_result(*databases))]


class ProviderNativeValkeyRecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.control = control()
        self.readiness_control = control(fork.READINESS_WORKFLOW_PATH, "7002")
        self.cleanup_control = control(fork.CLEANUP_WORKFLOW_PATH, "7003")
        self.contract = contract()
        self.contract_sha = self.control["contract_sha256"]
        self.fork_name = fork.deterministic_fork_name(PHASE, self.control)
        self.terminal_cleanup_evidence = {
            "mode": "terminal",
            "authority_set_sha256": "d" * 64,
        }
        self.quarantine_cleanup_evidence = {
            "mode": "quarantine",
            "authority_set_sha256": "e" * 64,
        }
        self.pre_mutation_cleanup_evidence = {
            "mode": "pre-mutation-failure",
            "apply_failure": {
                "run_id": "7010",
                "run_attempt": 1,
                "job_inventory_sha256": "1" * 64,
                "artifact_inventory_sha256": "2" * 64,
            },
        }
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        self.intent_transport = FakeTransport(
            [
                ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
                ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(CONFIG)),
                ("GET", f"/v2/databases/{SOURCE_ID}/firewall", ok(SOURCE_FIREWALL)),
                ("GET", fork.LIST_PATH, ok({"databases": [source], "meta": {"total": 1}})),
            ]
        )
        self.intent = fork.build_create_intent(
            target=TARGET,
            control=self.control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            transport=self.intent_transport,
            now=NOW,
        )
        self.intent_sha = common.sha256_bytes(
            common.canonical_file_bytes(self.intent)
        )

    def test_inventory_accepts_absent_optional_meta_only_below_page_ceiling(
        self,
    ) -> None:
        def unique_records(count: int) -> list[dict[str, Any]]:
            return [
                database(
                    f"{index:08x}-1111-4111-8111-{index:012x}",
                    f"synthetic-valkey-{index:03d}",
                    created_at="2025-01-01T00:00:00Z",
                )
                for index in range(1, count + 1)
            ]

        five = unique_records(5)
        accepted_responses = (
            {"databases": five},
            {"databases": five, "links": {}},
            {"databases": five, "links": {"pages": {}}},
            {"databases": five, "meta": {"total": 5}},
            {"databases": unique_records(fork.LIST_PAGE_SIZE - 1)},
        )
        for response in accepted_responses:
            with self.subTest(response=response):
                session = fork.ProviderSession(
                    FakeTransport([("GET", fork.LIST_PATH, ok(response))])
                )
                inventory = fork._list_inventory(session)
                self.assertEqual(len(inventory), len(response["databases"]))
                self.assertEqual(
                    [item["id"] for item in inventory],
                    [item["id"] for item in response["databases"]],
                )
                self.assertTrue(all("connection" not in item for item in inventory))
                self.assertEqual(
                    session.ledger, [("GET", "valkey-recovery-discovery")]
                )

        source = five[0]
        malformed_responses = (
            {"databases": unique_records(fork.LIST_PAGE_SIZE)},
            {
                "databases": unique_records(fork.LIST_PAGE_SIZE + 1),
                "meta": {"total": fork.LIST_PAGE_SIZE + 1},
            },
            {
                "databases": [source],
                "links": {"pages": {"next": "https://example.invalid/page/2"}},
            },
            {"databases": [source], "links": {"pages": {"last": 2}}},
            {"databases": [source], "links": {"pages": {"prev": 1}}},
            {"databases": [source], "links": {"other": {}}},
            {"databases": [source], "links": None},
            {"databases": [source], "links": []},
            {"databases": [source], "links": {"pages": None}},
            {"databases": [source], "links": {"pages": []}},
            {"databases": [source], "meta": None},
            {"databases": [source], "meta": []},
            {"databases": [source], "meta": {}},
            {"databases": [source], "meta": {"total": True}},
            {"databases": [source], "meta": {"total": -1}},
            {
                "databases": [source],
                "meta": {"total": fork.LIST_PAGE_SIZE + 1},
            },
            {"databases": [source], "meta": {"total": 2}},
            {
                "databases": [
                    source,
                    {**unique_records(2)[1], "id": source["id"]},
                ],
                "meta": {"total": 2},
            },
            {
                "databases": [
                    source,
                    {**unique_records(2)[1], "name": source["name"]},
                ],
                "meta": {"total": 2},
            },
        )
        for response in malformed_responses:
            with self.subTest(response=response):
                rejected = fork.ProviderSession(
                    FakeTransport(
                        [("GET", fork.LIST_PATH, ok(response))]
                    )
                )
                with self.assertRaises(common.ReleaseError):
                    fork._list_inventory(rejected)

    def create_receipt(self) -> tuple[dict[str, Any], FakeCapabilities]:
        transport = FakeCapabilities(create_steps(self.fork_name))
        receipt = fork.create_or_reconcile(
            target=TARGET,
            control=self.control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_intent=self.intent,
            create_intent_sha256=self.intent_sha,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW,
            poll_limit=2,
        )
        return receipt, transport

    def terminal_delete_receipt(
        self,
    ) -> tuple[dict[str, Any], dict[str, Any], str, FakeCapabilities]:
        create_receipt, _ = self.create_receipt()
        create_receipt_sha = common.sha256_bytes(
            common.canonical_file_bytes(create_receipt)
        )
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                ),
                ("DELETE", f"/v2/databases/{FORK_ID}", fork.APIResult(204, None)),
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source),
                ),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=create_receipt,
            create_receipt_sha256=create_receipt_sha,
            cleanup_mode="terminal",
            cleanup_evidence=self.terminal_cleanup_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW,
            poll_limit=2,
        )
        return deleted, create_receipt, create_receipt_sha, transport

    def test_isolated_cli_exposes_fixed_subcommands(self) -> None:
        result = subprocess.run(
            [
                sys.executable,
                "-I",
                "-S",
                "-B",
                str(Path(fork.__file__).resolve()),
                "--help",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for command in (
            "prepare-intent",
            "create-or-reconcile",
            "validate-create-receipt",
            "observe-readiness",
            "delete-or-reconcile",
            "validate-delete-receipt",
        ):
            self.assertIn(command, result.stdout)
        delete_help = subprocess.run(
            [
                sys.executable,
                "-I",
                "-S",
                "-B",
                str(Path(fork.__file__).resolve()),
                "delete-or-reconcile",
                "--help",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(delete_help.returncode, 0, delete_help.stderr)
        self.assertIn("--cleanup-mode", delete_help.stdout)
        self.assertIn("--cleanup-evidence", delete_help.stdout)
        self.assertIn("pre-mutation-failure", delete_help.stdout)
        gate_help = subprocess.run(
            [
                sys.executable,
                "-I",
                "-S",
                "-B",
                str(Path(fork.__file__).resolve()),
                "validate-create-receipt",
                "--help",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(gate_help.returncode, 0, gate_help.stderr)
        for option in (
            "--contract",
            "--intent",
            "--intent-sha256",
            "--receipt",
            "--receipt-sha256",
        ):
            self.assertIn(option, gate_help.stdout)
        delete_gate_help = subprocess.run(
            [
                sys.executable,
                "-I",
                "-S",
                "-B",
                str(Path(fork.__file__).resolve()),
                "validate-delete-receipt",
                "--help",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(delete_gate_help.returncode, 0, delete_gate_help.stderr)
        for option in (
            "--delete-receipt",
            "--delete-receipt-sha256",
            "--create-receipt",
            "--create-intent",
            "--cleanup-mode",
            "--cleanup-evidence",
        ):
            self.assertIn(option, delete_gate_help.stdout)

    def test_cleanup_uses_current_code_with_exact_ancestor_authority_binding(self) -> None:
        current_contract = copy.deepcopy(self.contract)
        current_contract["provider"]["app_id_sha256"] = "f" * 64
        current_contract_sha = common.sha256_bytes(
            common.canonical_file_bytes(current_contract)
        )
        self.assertNotEqual(
            current_contract_sha,
            self.contract_sha,
        )
        self.cleanup_control["workflow_sha"] = "d" * 40
        self.cleanup_control["controller_sha256"] = "e" * 64
        deleted, create_receipt, create_receipt_sha, _ = (
            self.terminal_delete_receipt()
        )
        self.assertEqual(deleted["control"]["workflow_sha"], "d" * 40)
        self.assertEqual(
            deleted["control"]["authority_workflow_sha"], "a" * 40
        )
        self.assertEqual(
            deleted["control"]["authority_controller_sha256"], "c" * 64
        )
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        self.assertEqual(
            fork.validate_delete_receipt(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
            ),
            deleted,
        )

        current_contract_control = copy.deepcopy(self.cleanup_control)
        current_contract_control["contract_sha256"] = current_contract_sha
        transport = FakeCapabilities([])
        with self.assertRaises(common.ReleaseError):
            fork.delete_or_reconcile(
                target=TARGET,
                control=current_contract_control,
                phase=PHASE,
                contract=current_contract,
                contract_file_sha256=current_contract_sha,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
            )
        self.assertEqual(transport.calls, [])

        for field, invalid in (
            ("authority_workflow_sha", "f" * 40),
            ("authority_controller_sha256", "f" * 64),
            ("rollout_plan_sha256", "f" * 64),
        ):
            with self.subTest(field=field):
                wrong = copy.deepcopy(self.cleanup_control)
                wrong[field] = invalid
                transport = FakeCapabilities([])
                with self.assertRaises(common.ReleaseError):
                    fork.delete_or_reconcile(
                        target=TARGET,
                        control=wrong,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        create_receipt=create_receipt,
                        create_receipt_sha256=create_receipt_sha,
                        cleanup_mode="terminal",
                        cleanup_evidence=self.terminal_cleanup_evidence,
                        read_transport=transport.read,
                        mutation_transport=transport.mutation,
                        now=NOW,
                    )
                self.assertEqual(transport.calls, [])

    def test_checked_in_contract_binds_raw_file_and_cluster_name_hash(self) -> None:
        path = Path(fork.__file__).resolve().with_name("production-app-contract.json")
        value, exact_hash = fork._load_contract(path)
        self.assertEqual(exact_hash, common.sha256_bytes(path.read_bytes()))
        binding = fork.contract_binding(value, exact_hash, exact_hash)
        valkey = next(
            item
            for item in value["expected_topology"]["databases"]
            if item["engine"] == "VALKEY"
        )
        self.assertEqual(binding["source_name_sha256"], valkey["cluster_sha256"])
        self.assertNotEqual(binding["source_name_sha256"], valkey["name_sha256"])
        self.assertEqual(
            binding["app_id_sha256"], value["provider"]["app_id_sha256"]
        )

    def test_intent_is_sanitized_and_binds_exact_latest_state_request(self) -> None:
        self.assertEqual(self.intent["authority"], fork.INTENT_AUTHORITY)
        self.assertEqual(
            self.intent["target"]["descriptor_sha256"],
            fork.target_descriptor_sha256(TARGET),
        )
        self.assertTrue(self.intent["request"]["spec"]["backup_created_at_omitted"])
        rendered = json.dumps(self.intent, sort_keys=True)
        for private in (
            SOURCE_ID,
            POSTGRES_ID,
            VPC_ID,
            APP_ID,
            SOURCE_NAME,
            self.fork_name,
        ):
            self.assertNotIn(private, rendered)
        self.assertEqual(
            [call[0] for call in self.intent_transport.calls], ["GET"] * 4
        )

    def test_database_get_response_must_match_requested_path_identity(self) -> None:
        transport = FakeTransport(
            [
                (
                    "GET",
                    f"/v2/databases/{SOURCE_ID}",
                    ok(envelope(database(FORK_ID, SOURCE_NAME))),
                )
            ]
        )
        with self.assertRaises(common.ReleaseError):
            fork._get_database(
                fork.ProviderSession(transport), SOURCE_ID, "synthetic-database"
            )

    def test_source_firewall_must_be_one_exact_production_app_rule(self) -> None:
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        wrong_app = "88888888-8888-4888-8888-888888888888"
        cases = {
            "empty": EMPTY_FIREWALL,
            "arbitrary": {"rules": [{"type": "tag", "value": "synthetic"}]},
            "multi": {
                "rules": [
                    {"type": "app", "value": APP_ID},
                    {"type": "app", "value": wrong_app},
                ]
            },
            "ip": {"rules": [{"type": "ip_addr", "value": "203.0.113.7"}]},
            "wrong-app": {"rules": [{"type": "app", "value": wrong_app}]},
        }
        for label, firewall in cases.items():
            with self.subTest(label=label):
                transport = FakeTransport(
                    [
                        ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
                        ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(CONFIG)),
                        (
                            "GET",
                            f"/v2/databases/{SOURCE_ID}/firewall",
                            ok(firewall),
                        ),
                    ]
                )
                with self.assertRaises(common.ReleaseError):
                    fork.build_create_intent(
                        target=TARGET,
                        control=self.control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        transport=transport,
                        now=NOW,
                    )
                self.assertTrue(all(call[0] == "GET" for call in transport.calls))

    def test_clean_201_creates_one_sanitized_authoritative_receipt(self) -> None:
        receipt, transport = self.create_receipt()
        self.assertEqual(receipt["authority"], fork.CREATE_AUTHORITY)
        self.assertEqual(receipt["result"]["outcome"], "created")
        self.assertFalse(receipt["result"]["mutation_ambiguous_reconciled"])
        self.assertEqual(receipt["provider"]["mutation_request_count"], 1)
        self.assertEqual(sum(call[0] == "POST" for call in transport.calls), 1)
        self.assertEqual({call[0] for call in transport.read.calls}, {"GET"})
        self.assertEqual(
            [call[0] for call in transport.mutation.calls], ["POST"]
        )
        post = next(call for call in transport.calls if call[0] == "POST")
        body = common.loads_strict(post[2] or b"")
        self.assertEqual(body["backup_restore"], {"database_name": SOURCE_NAME})
        self.assertNotIn("backup_created_at", body["backup_restore"])
        self.assertEqual(body["rules"], [{"type": "app", "value": APP_ID}])
        self.assertEqual(
            self.intent["request"]["spec"]["firewall_policy_sha256"],
            common.sha256_value(
                [
                    {
                        "type": "app",
                        "value_sha256": common.sha256_bytes(
                            APP_ID.encode("utf-8")
                        ),
                    }
                ]
            ),
        )
        self.assertTrue(receipt["gates"]["source_firewall_exact_app"])
        self.assertTrue(
            receipt["gates"]["recovery_firewall_exact_source_app"]
        )
        self.assertTrue(
            receipt["gates"]["recovery_restricted_to_exact_production_app"]
        )
        self.assertNotEqual(
            receipt["target"]["source_firewall_sha256"],
            receipt["result"]["recovery_firewall_sha256"],
            "per-cluster provider metadata must remain hash-bound independently",
        )
        rendered = json.dumps(receipt, sort_keys=True)
        for private in (
            SOURCE_ID,
            POSTGRES_ID,
            FORK_ID,
            VPC_ID,
            APP_ID,
            SOURCE_NAME,
            self.fork_name,
            "private-test-password",
        ):
            self.assertNotIn(private, rendered)

    def test_fork_firewall_allows_only_exact_source_app_policy(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        wrong_app = "88888888-8888-4888-8888-888888888888"
        cases = {
            "empty": EMPTY_FIREWALL,
            "arbitrary": {"rules": [{"type": "tag", "value": "synthetic"}]},
            "multi": {
                "rules": [
                    {"type": "app", "value": APP_ID},
                    {"type": "app", "value": wrong_app},
                ]
            },
            "ip": {"rules": [{"type": "ip_addr", "value": "203.0.113.7"}]},
            "wrong-app": {"rules": [{"type": "app", "value": wrong_app}]},
        }
        for label, firewall in cases.items():
            with self.subTest(label=label):
                steps = self.readiness_steps()[:7]
                steps[-1] = (
                    "GET",
                    f"/v2/databases/{FORK_ID}/firewall",
                    ok(firewall),
                )
                with self.assertRaises(common.ReleaseError):
                    fork.validate_create_receipt_live(
                        receipt,
                        exact_sha256=receipt_sha,
                        target=TARGET,
                        control=self.control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        create_intent=self.intent,
                        create_intent_sha256=self.intent_sha,
                        read_transport=FakeTransport(steps),
                        now=NOW,
                    )

    def test_live_create_gate_binds_intent_control_request_and_provider_result(
        self,
    ) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        validated = fork.validate_create_receipt_live(
            receipt,
            exact_sha256=receipt_sha,
            target=TARGET,
            control=self.control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_intent=self.intent,
            create_intent_sha256=self.intent_sha,
            read_transport=FakeTransport(self.readiness_steps()[:7]),
            now=NOW,
        )
        self.assertEqual(validated, receipt)

        pre_wire_mutations = (
            ("control", "controller_sha256"),
            ("control", "contract_sha256"),
            ("target", "source_config_sha256"),
            ("request", "request_sha256"),
        )
        for section, key in pre_wire_mutations:
            with self.subTest(pre_wire=f"{section}.{key}"):
                mutant = copy.deepcopy(receipt)
                original = mutant[section][key]
                mutant[section][key] = "d" * 64
                if mutant[section][key] == original:
                    mutant[section][key] = "e" * 64
                with self.assertRaises(common.ReleaseError):
                    fork.validate_create_receipt(
                        mutant,
                        exact_sha256=common.sha256_bytes(
                            common.canonical_file_bytes(mutant)
                        ),
                        target=TARGET,
                        phase=PHASE,
                        now=NOW,
                        create_intent=self.intent,
                        create_intent_sha256=self.intent_sha,
                        current_control=self.control,
                    )

        for key in (
            "recovery_identity_sha256",
            "fork_name_sha256",
            "fork_created_at_sha256",
            "recovery_observation_sha256",
            "recovery_topology_sha256",
            "recovery_config_sha256",
            "recovery_firewall_sha256",
        ):
            with self.subTest(dynamic_result=key):
                mutant = copy.deepcopy(receipt)
                original = mutant["result"][key]
                mutant["result"][key] = "d" * 64
                if mutant["result"][key] == original:
                    mutant["result"][key] = "e" * 64
                with self.assertRaises(common.ReleaseError):
                    fork.validate_create_receipt_live(
                        mutant,
                        exact_sha256=common.sha256_bytes(
                            common.canonical_file_bytes(mutant)
                        ),
                        target=TARGET,
                        control=self.control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        create_intent=self.intent,
                        create_intent_sha256=self.intent_sha,
                        read_transport=FakeTransport(self.readiness_steps()[:7]),
                        now=NOW,
                    )

    def test_live_create_gate_allows_spent_intent_after_valid_one_shot_wire(
        self,
    ) -> None:
        receipt, _ = self.create_receipt()
        gate_time = NOW + dt.timedelta(
            seconds=fork.MAX_INTENT_AGE_SECONDS + 1
        )
        self.assertEqual(
            fork.validate_create_receipt_live(
                receipt,
                exact_sha256=common.sha256_bytes(
                    common.canonical_file_bytes(receipt)
                ),
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                read_transport=FakeTransport(self.readiness_steps()[:7]),
                now=gate_time,
            ),
            receipt,
        )

    def test_create_receipt_rejects_more_than_maximum_ready_polls(self) -> None:
        receipt, _ = self.create_receipt()
        mutant = copy.deepcopy(receipt)
        labels = mutant["provider"]["endpoint_labels"]
        labels[-5:-5] = ["valkey-recovery-cluster-ready"] * fork.MAX_PROVIDER_POLLS
        mutant["provider"]["http_request_count"] = len(labels)
        with self.assertRaises(common.ReleaseError):
            fork.validate_create_receipt(
                mutant,
                exact_sha256=common.sha256_bytes(
                    common.canonical_file_bytes(mutant)
                ),
                target=TARGET,
                phase=PHASE,
                now=NOW,
            )

    def test_mutation_never_occurs_before_exact_intent_validation(self) -> None:
        transport = FakeCapabilities([])
        tampered = copy.deepcopy(self.intent)
        tampered["request"]["request_sha256"] = "d" * 64
        with self.assertRaises(common.ReleaseError):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=tampered,
                create_intent_sha256=self.intent_sha,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
            )
        self.assertEqual(transport.calls, [])

    def test_firewall_stripped_request_never_reaches_post(self) -> None:
        mutant = copy.deepcopy(self.intent)
        request = fork._create_request(
            {
                "database": database(
                    SOURCE_ID,
                    SOURCE_NAME,
                    created_at="2025-01-01T00:00:00Z",
                ),
                "firewall_app_id": APP_ID,
            },
            self.fork_name,
        )
        request.pop("rules")
        mutant["request"]["request_sha256"] = common.sha256_bytes(
            common.canonical_payload_bytes(request)
        )
        mutant["operation_id_sha256"] = common.sha256_value(
            {
                "phase": PHASE,
                "workflow_sha": self.control["workflow_sha"],
                "run_id": self.control["run_id"],
                "run_attempt": self.control["run_attempt"],
                "fork_name_sha256": mutant["request"]["spec"][
                    "fork_name_sha256"
                ],
                "request_sha256": mutant["request"]["request_sha256"],
            }
        )
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        transport = FakeCapabilities(
            source_read_steps()
            + [("GET", fork.LIST_PATH, inventory_result(source))]
        )
        with self.assertRaises(common.ReleaseError):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=mutant,
                create_intent_sha256=common.sha256_bytes(
                    common.canonical_file_bytes(mutant)
                ),
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
            )
        self.assertEqual(transport.mutation.calls, [])

    def test_ambiguous_create_is_discovered_but_never_authoritative(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        steps = create_steps(self.fork_name)[:4]
        steps.extend(
            [
                ("POST", fork.DATABASES_PATH, fork.MutationAmbiguous("timeout")),
                (
                    "GET",
                    fork.LIST_PATH,
                    ok({"databases": [source, recovery], "meta": {"total": 2}}),
                ),
            ]
        )
        transport = FakeCapabilities(steps)
        with self.assertRaises(fork.MutationAmbiguous):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
                poll_limit=2,
            )
        self.assertEqual(sum(call[0] == "POST" for call in transport.calls), 1)

    def test_malformed_clean_201_is_reconciled_then_quarantined(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        steps = create_steps(self.fork_name)[:4]
        steps.extend(
            [
                (
                    "POST",
                    fork.DATABASES_PATH,
                    fork.APIResult(201, {"database": {"id": FORK_ID}}),
                ),
                (
                    "GET",
                    fork.LIST_PATH,
                    ok({"databases": [source, recovery], "meta": {"total": 2}}),
                ),
            ]
        )
        transport = FakeCapabilities(steps)
        with self.assertRaises(fork.MutationAmbiguous):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
                poll_limit=2,
            )
        self.assertEqual(sum(call[0] == "POST" for call in transport.calls), 1)

    def test_wrong_spec_clean_201_is_quarantined_without_retry(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        wrong = database(FORK_ID, self.fork_name)
        wrong["size"] = "db-s-2vcpu-4gb"
        steps = create_steps(self.fork_name)[:4]
        steps.extend(
            [
                ("POST", fork.DATABASES_PATH, fork.APIResult(201, envelope(wrong))),
                (
                    "GET",
                    fork.LIST_PATH,
                    ok({"databases": [source, wrong], "meta": {"total": 2}}),
                ),
            ]
        )
        transport = FakeCapabilities(steps)
        with self.assertRaises(fork.MutationAmbiguous):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
                poll_limit=2,
            )
        self.assertEqual(sum(call[0] == "POST" for call in transport.calls), 1)

    def test_post_create_observation_failure_is_quarantined_without_retry(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        steps = create_steps(self.fork_name)[:5]
        steps.extend(
            [
                (
                    "GET",
                    f"/v2/databases/{FORK_ID}",
                    common.ReleaseError("truncated post-create observation"),
                ),
                (
                    "GET",
                    fork.LIST_PATH,
                    ok({"databases": [source, recovery], "meta": {"total": 2}}),
                ),
            ]
        )
        transport = FakeCapabilities(steps)
        with self.assertRaises(fork.MutationAmbiguous):
            fork.create_or_reconcile(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
                poll_limit=2,
            )
        self.assertEqual(sum(call[0] == "POST" for call in transport.calls), 1)
        self.assertEqual(transport.steps, [])

    def readiness_steps(
        self, *, second_firewall: dict[str, Any] | None = None
    ) -> list[tuple[str, str, object]]:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        steps: list[tuple[str, str, object]] = [
            (
                "GET",
                fork.LIST_PATH,
                ok({"databases": [source, recovery], "meta": {"total": 2}}),
            )
        ]
        for index in range(2):
            steps.extend(
                [
                    ("GET", f"/v2/databases/{SOURCE_ID}", ok(envelope(source))),
                    ("GET", f"/v2/databases/{SOURCE_ID}/config", ok(CONFIG)),
                    ("GET", f"/v2/databases/{SOURCE_ID}/firewall", ok(SOURCE_FIREWALL)),
                    ("GET", f"/v2/databases/{FORK_ID}", ok(envelope(recovery))),
                    ("GET", f"/v2/databases/{FORK_ID}/config", ok(CONFIG)),
                    (
                        "GET",
                        f"/v2/databases/{FORK_ID}/firewall",
                        ok(
                            second_firewall
                            if index == 1 and second_firewall
                            else FORK_FIREWALL
                        ),
                    ),
                ]
            )
        return steps

    def test_readiness_is_two_stable_get_rounds_and_exact_v2_proof(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        transport = FakeTransport(self.readiness_steps())
        observation = fork.observe_readiness(
            target=TARGET,
            control=self.readiness_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
            transport=transport,
            now=NOW,
        )
        proof = observation["provider_fork"]
        self.assertEqual(set(proof), fork.PROVIDER_FORK_KEYS)
        self.assertEqual(proof["authority"], fork.READINESS_AUTHORITY)
        self.assertEqual(proof["receipt_sha256"], receipt_sha)
        self.assertEqual(proof["stable_read_count"], 2)
        self.assertTrue(proof["source_firewall_unchanged"])
        self.assertTrue(proof["source_firewall_exact_app"])
        self.assertTrue(proof["recovery_firewall_exact_source_app"])
        self.assertTrue(
            proof["recovery_restricted_to_exact_production_app"]
        )
        self.assertEqual(observation["provider"]["http_request_count"], 13)
        self.assertEqual(observation["provider"]["http_methods_used"], ["GET"])

    def test_readiness_rejects_firewall_drift_between_rounds(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        with self.assertRaises(common.ReleaseError):
            fork.observe_readiness(
                target=TARGET,
                control=self.readiness_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_receipt=receipt,
                create_receipt_sha256=receipt_sha,
                transport=FakeTransport(
                    self.readiness_steps(second_firewall=SOURCE_FIREWALL)
                ),
                now=NOW,
            )

    def test_readiness_requires_its_own_workflow_and_embedded_prepare_control(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        transport = FakeTransport([])
        with self.assertRaises(common.ReleaseError):
            fork.observe_readiness(
                target=TARGET,
                control=self.control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_receipt=receipt,
                create_receipt_sha256=receipt_sha,
                transport=transport,
                now=NOW,
            )
        self.assertEqual(transport.calls, [])

    def test_delete_uses_one_receipt_bound_mutation_and_reconciles_absence(self) -> None:
        deleted, receipt, receipt_sha, transport = (
            self.terminal_delete_receipt()
        )
        self.assertEqual(deleted["result"]["outcome"], "deleted")
        self.assertEqual(deleted["result"]["deletion_request_attempt_count"], 1)
        self.assertEqual(deleted["result"]["stable_absence_read_count"], 2)
        self.assertEqual(deleted["result"]["source_stable_read_count"], 2)
        for key in fork.CREATE_TARGET_KEYS:
            self.assertEqual(deleted["target"][key], receipt["target"][key])
        self.assertTrue(deleted["gates"]["source_ready"])
        self.assertTrue(deleted["gates"]["source_stable"])
        self.assertTrue(deleted["gates"]["source_firewall_exact_app"])
        self.assertNotIn(APP_ID, json.dumps(deleted, sort_keys=True))
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)
        self.assertEqual({call[0] for call in transport.read.calls}, {"GET"})
        self.assertEqual(
            [call[0] for call in transport.mutation.calls], ["DELETE"]
        )
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        self.assertEqual(
            fork.validate_delete_receipt(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                create_receipt=receipt,
                create_receipt_sha256=receipt_sha,
            ),
            deleted,
        )
        mutants: list[dict[str, Any]] = []
        for path, replacement in (
            (("control", "run_id"), "7999"),
            (("result", "stable_absence_read_count"), 1),
            (("result", "outcome"), "already-absent"),
            (("provider", "mutation_request_count"), 0),
            (("gates", "fork_absent"), False),
            (("gates", "source_firewall_exact_app"), False),
            (("target", "create_authority"), "unbound"),
            (("target", "create_authority_sha256"), "f" * 64),
            (("target", "fork_name_sha256"), "f" * 64),
            (("target", "recovery_identity_sha256"), "f" * 64),
            (("target", "cleanup_mode"), "quarantine"),
            (("target", "cleanup_authority_sha256"), "f" * 64),
        ):
            mutant = copy.deepcopy(deleted)
            mutant[path[0]][path[1]] = replacement
            mutants.append(mutant)
        missing_read = copy.deepcopy(deleted)
        missing_read["provider"]["endpoint_labels"].pop()
        missing_read["provider"]["http_request_count"] -= 1
        mutants.append(missing_read)
        for mutant in mutants:
            with self.assertRaises(common.ReleaseError):
                fork.validate_delete_receipt(
                    mutant,
                    exact_sha256=common.sha256_bytes(
                        common.canonical_file_bytes(mutant)
                    ),
                    target=TARGET,
                    control=self.cleanup_control,
                    phase=PHASE,
                    now=NOW,
                    cleanup_mode="terminal",
                    cleanup_evidence=self.terminal_cleanup_evidence,
                    create_receipt=receipt,
                    create_receipt_sha256=receipt_sha,
                )

        tampered_evidence = copy.deepcopy(self.terminal_cleanup_evidence)
        tampered_evidence["authority_set_sha256"] = "f" * 64
        with self.assertRaises(common.ReleaseError):
            fork.validate_delete_receipt(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=tampered_evidence,
                create_receipt=receipt,
                create_receipt_sha256=receipt_sha,
            )

    def test_delete_rejects_source_drift_before_any_mutation(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        base = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        offline = copy.deepcopy(base)
        offline["status"] = "maintenance"
        topology = copy.deepcopy(base)
        topology["size"] = "db-s-2vcpu-4gb"
        version = copy.deepcopy(base)
        version["version"] = "7"
        region = copy.deepcopy(base)
        region["region"] = "nyc3"
        config_drift = copy.deepcopy(CONFIG)
        config_drift["config"]["valkey_maxmemory_policy"] = "volatile-lru"
        wrong_firewall = {
            "rules": [
                {
                    "type": "app",
                    "value": "88888888-8888-4888-8888-888888888888",
                }
            ]
        }
        cases = {
            "missing": [
                (
                    "GET",
                    f"/v2/databases/{SOURCE_ID}",
                    fork.APIResult(404, None),
                )
            ],
            "offline": source_read_steps(source=offline),
            "config": source_read_steps(config=config_drift),
            "firewall": source_read_steps(firewall=wrong_firewall),
            "topology": source_read_steps(source=topology),
            "version": source_read_steps(source=version),
            "region": source_read_steps(source=region),
        }
        for label, steps in cases.items():
            with self.subTest(label=label):
                transport = FakeCapabilities(steps)
                with self.assertRaises(common.ReleaseError):
                    fork.delete_or_reconcile(
                        target=TARGET,
                        control=self.cleanup_control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        create_receipt=receipt,
                        create_receipt_sha256=receipt_sha,
                        cleanup_mode="terminal",
                        cleanup_evidence=self.terminal_cleanup_evidence,
                        read_transport=transport.read,
                        mutation_transport=transport.mutation,
                        now=NOW,
                    )
                self.assertEqual(transport.mutation.calls, [])

    def test_live_delete_gate_requires_two_independent_get_only_absence_reads(
        self,
    ) -> None:
        deleted, create_receipt, create_receipt_sha, _ = (
            self.terminal_delete_receipt()
        )
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        shared = FakeTransport(
            cleanup_round_steps(source) + cleanup_round_steps(source)
        )
        read_transport = MethodBoundTransport(shared, {"GET"})
        sleeps: list[float] = []
        self.assertEqual(
            fork.validate_delete_receipt_live(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=read_transport,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
                sleeper=sleeps.append,
            ),
            deleted,
        )
        self.assertEqual([call[0] for call in read_transport.calls], ["GET"] * 8)
        self.assertEqual(sleeps, [2])
        self.assertEqual(shared.steps, [])

    def test_live_delete_gate_rejects_stale_reappearing_or_renamed_fork(
        self,
    ) -> None:
        deleted, create_receipt, create_receipt_sha, _ = (
            self.terminal_delete_receipt()
        )
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        recovery = database(FORK_ID, self.fork_name)
        renamed = database(FORK_ID, "synthetic-renamed-recovery")

        def response(*items: dict[str, Any]) -> fork.APIResult:
            return ok({"databases": list(items), "meta": {"total": len(items)}})

        cases = (
            ("stale", [response(source, recovery)]),
            ("reappeared", [response(source), response(source, recovery)]),
            ("renamed", [response(source, renamed)]),
            (
                "one-read",
                [response(source), common.ReleaseError("second read unavailable")],
            ),
            (
                "paginated",
                [ok({"databases": [source], "meta": {"total": 2}})],
            ),
            ("duplicate", [response(source, source)]),
            (
                "malformed",
                [ok({"databases": [{"id": SOURCE_ID}], "meta": {"total": 1}})],
            ),
        )
        for label, results in cases:
            with self.subTest(label=label):
                steps: list[tuple[str, str, object]] = []
                for result in results:
                    steps.extend(source_read_steps())
                    steps.append(("GET", fork.LIST_PATH, result))
                read_transport = MethodBoundTransport(
                    FakeTransport(steps), {"GET"}
                )
                with self.assertRaises((common.ReleaseError, AssertionError)):
                    fork.validate_delete_receipt_live(
                        deleted,
                        exact_sha256=deleted_sha,
                        target=TARGET,
                        control=self.cleanup_control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        now=NOW,
                        cleanup_mode="terminal",
                        cleanup_evidence=self.terminal_cleanup_evidence,
                        read_transport=read_transport,
                        create_receipt=create_receipt,
                        create_receipt_sha256=create_receipt_sha,
                    )
                self.assertTrue(all(call[0] == "GET" for call in read_transport.calls))

    def test_live_delete_gate_rejects_source_safety_drift(self) -> None:
        deleted, create_receipt, create_receipt_sha, _ = (
            self.terminal_delete_receipt()
        )
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        config_drift = copy.deepcopy(CONFIG)
        config_drift["config"]["valkey_maxmemory_policy"] = "volatile-lru"
        steps = cleanup_round_steps(source)
        steps.extend(cleanup_round_steps(source, config=config_drift))
        transport = MethodBoundTransport(FakeTransport(steps), {"GET"})
        with self.assertRaises(common.ReleaseError):
            fork.validate_delete_receipt_live(
                deleted,
                exact_sha256=common.sha256_bytes(
                    common.canonical_file_bytes(deleted)
                ),
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=transport,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
            )
        self.assertTrue(all(call[0] == "GET" for call in transport.calls))

    def test_live_delete_gate_rejects_current_control_and_cross_authority_drift(
        self,
    ) -> None:
        deleted, create_receipt, create_receipt_sha, _ = (
            self.terminal_delete_receipt()
        )
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        wrong_control = copy.deepcopy(self.cleanup_control)
        wrong_control["run_id"] = "7999"
        calls = FakeTransport([])
        with self.assertRaises(common.ReleaseError):
            fork.validate_delete_receipt_live(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=wrong_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=MethodBoundTransport(calls, {"GET"}),
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
            )
        self.assertEqual(calls.calls, [])

        with self.assertRaises(common.ReleaseError):
            fork.validate_delete_receipt_live(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                now=NOW,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=MethodBoundTransport(calls, {"GET"}),
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
            )
        self.assertEqual(calls.calls, [])

    def test_cleanup_mode_must_match_evidence_and_create_authority(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        cases = (
            (
                "receipt-quarantine",
                {
                    "create_receipt": receipt,
                    "create_receipt_sha256": receipt_sha,
                },
                "quarantine",
                self.quarantine_cleanup_evidence,
            ),
            (
                "intent-terminal",
                {
                    "create_intent": self.intent,
                    "create_intent_sha256": self.intent_sha,
                },
                "terminal",
                self.terminal_cleanup_evidence,
            ),
            (
                "intent-pre-mutation-failure",
                {
                    "create_intent": self.intent,
                    "create_intent_sha256": self.intent_sha,
                },
                "pre-mutation-failure",
                self.pre_mutation_cleanup_evidence,
            ),
            (
                "evidence-mode",
                {
                    "create_receipt": receipt,
                    "create_receipt_sha256": receipt_sha,
                },
                "terminal",
                self.quarantine_cleanup_evidence,
            ),
        )
        for label, authority, mode, evidence in cases:
            with self.subTest(label=label):
                transport = FakeCapabilities([])
                with self.assertRaises(common.ReleaseError):
                    fork.delete_or_reconcile(
                        target=TARGET,
                        control=self.cleanup_control,
                        phase=PHASE,
                        contract=self.contract,
                        contract_file_sha256=self.contract_sha,
                        cleanup_mode=mode,
                        cleanup_evidence=evidence,
                        read_transport=transport.read,
                        mutation_transport=transport.mutation,
                        now=NOW,
                        **authority,
                    )
                self.assertEqual(transport.calls, [])

    def test_pre_mutation_failure_cleanup_is_receipt_backed_and_exactly_bound(
        self,
    ) -> None:
        create_receipt, _ = self.create_receipt()
        create_receipt_sha = common.sha256_bytes(
            common.canonical_file_bytes(create_receipt)
        )
        source = database(
            SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z"
        )
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                ("GET", fork.LIST_PATH, inventory_result(source, recovery)),
                ("DELETE", f"/v2/databases/{FORK_ID}", fork.APIResult(204, None)),
                ("GET", fork.LIST_PATH, inventory_result(source)),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=create_receipt,
            create_receipt_sha256=create_receipt_sha,
            cleanup_mode="pre-mutation-failure",
            cleanup_evidence=self.pre_mutation_cleanup_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW + dt.timedelta(days=2),
            poll_limit=2,
        )
        self.assertEqual(deleted["target"]["cleanup_mode"], "pre-mutation-failure")
        self.assertEqual(
            deleted["target"]["cleanup_authority_sha256"],
            common.sha256_bytes(
                common.canonical_file_bytes(self.pre_mutation_cleanup_evidence)
            ),
        )
        self.assertEqual(deleted["result"]["outcome"], "deleted")
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        self.assertEqual(
            fork.validate_delete_receipt(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                now=NOW + dt.timedelta(days=2),
                cleanup_mode="pre-mutation-failure",
                cleanup_evidence=self.pre_mutation_cleanup_evidence,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
            ),
            deleted,
        )
        tampered = copy.deepcopy(self.pre_mutation_cleanup_evidence)
        tampered["apply_failure"]["job_inventory_sha256"] = "3" * 64
        with self.assertRaises(common.ReleaseError):
            fork.validate_delete_receipt(
                deleted,
                exact_sha256=deleted_sha,
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                now=NOW + dt.timedelta(days=2),
                cleanup_mode="pre-mutation-failure",
                cleanup_evidence=tampered,
                create_receipt=create_receipt,
                create_receipt_sha256=create_receipt_sha,
            )

    def test_ambiguous_delete_is_get_reconciled_without_second_delete(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                ),
                ("DELETE", f"/v2/databases/{FORK_ID}", fork.MutationAmbiguous("eof")),
                ("GET", fork.LIST_PATH, inventory_result(source)),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
            cleanup_mode="terminal",
            cleanup_evidence=self.terminal_cleanup_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW,
            poll_limit=2,
        )
        self.assertEqual(deleted["result"]["outcome"], "ambiguous-reconciled")
        self.assertTrue(deleted["result"]["mutation_ambiguous_reconciled"])
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)
        deleted_sha = common.sha256_bytes(common.canonical_file_bytes(deleted))
        fork.validate_delete_receipt(
            deleted,
            exact_sha256=deleted_sha,
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            now=NOW,
            cleanup_mode="terminal",
            cleanup_evidence=self.terminal_cleanup_evidence,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
        )

    def test_malformed_clean_delete_response_is_get_reconciled(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                ),
                (
                    "DELETE",
                    f"/v2/databases/{FORK_ID}",
                    fork.APIResult(204, {"unexpected": True}),
                ),
                ("GET", fork.LIST_PATH, inventory_result(source)),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
            cleanup_mode="terminal",
            cleanup_evidence=self.terminal_cleanup_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW,
            poll_limit=2,
        )
        self.assertEqual(deleted["result"]["outcome"], "ambiguous-reconciled")
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)

    def test_post_delete_observation_failure_is_quarantined_without_retry(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                ),
                ("DELETE", f"/v2/databases/{FORK_ID}", fork.APIResult(204, None)),
                (
                    "GET",
                    fork.LIST_PATH,
                    common.ReleaseError("truncated post-delete observation"),
                ),
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source),
                ),
            ]
        )
        with self.assertRaises(fork.MutationAmbiguous):
            fork.delete_or_reconcile(
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_receipt=receipt,
                create_receipt_sha256=receipt_sha,
                cleanup_mode="terminal",
                cleanup_evidence=self.terminal_cleanup_evidence,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW,
                poll_limit=2,
            )
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)
        self.assertEqual(transport.steps, [])

    def test_intent_only_cleanup_removes_ambiguous_create_candidate(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                ),
                ("DELETE", f"/v2/databases/{FORK_ID}", fork.APIResult(204, None)),
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source),
                ),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_intent=self.intent,
            create_intent_sha256=self.intent_sha,
            cleanup_mode="quarantine",
            cleanup_evidence=self.quarantine_cleanup_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW + dt.timedelta(hours=1),
            poll_limit=2,
        )
        self.assertEqual(deleted["target"]["create_authority"], "create-intent")
        self.assertEqual(deleted["result"]["outcome"], "deleted")
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 1)
        fork.validate_delete_receipt(
            deleted,
            exact_sha256=common.sha256_bytes(common.canonical_file_bytes(deleted)),
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            now=NOW + dt.timedelta(hours=1),
            cleanup_mode="quarantine",
            cleanup_evidence=self.quarantine_cleanup_evidence,
            create_intent=self.intent,
            create_intent_sha256=self.intent_sha,
        )

    def test_intent_cleanup_rejects_exact_name_with_unbound_spec(self) -> None:
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        recovery = database(FORK_ID, self.fork_name)
        recovery["size"] = "db-s-2vcpu-4gb"
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source, recovery),
                )
            ]
        )
        with self.assertRaises(common.ReleaseError):
            fork.delete_or_reconcile(
                target=TARGET,
                control=self.cleanup_control,
                phase=PHASE,
                contract=self.contract,
                contract_file_sha256=self.contract_sha,
                create_intent=self.intent,
                create_intent_sha256=self.intent_sha,
                cleanup_mode="quarantine",
                cleanup_evidence=self.quarantine_cleanup_evidence,
                read_transport=transport.read,
                mutation_transport=transport.mutation,
                now=NOW + dt.timedelta(hours=1),
            )
        self.assertEqual(sum(call[0] == "DELETE" for call in transport.calls), 0)

    def test_rate_limited_mutation_is_ambiguous_not_retryable(self) -> None:
        with self.assertRaises(fork.MutationAmbiguous):
            fork.DigitalOceanTransport._decode_response(
                "POST", 429, {"Content-Type": "application/json"}, b"{}"
            )

    def test_post_wire_response_validation_is_always_ambiguous(self) -> None:
        exact_url = common.API_ORIGIN + fork.DATABASES_PATH
        cases = (
            ("redirect", "https://example.invalid/v2/databases", "application/json", b"{}"),
            ("content-type", exact_url, "text/plain", b"{}"),
            ("truncated-json", exact_url, "application/json", b"{"),
        )
        for label, url, content_type, raw in cases:
            with self.subTest(label=label):
                opener = FakeOpener(
                    FakeHTTPResponse(
                        status=201,
                        url=url,
                        content_type=content_type,
                        raw=raw,
                    )
                )
                transport = fork.DigitalOceanTransport("x" * 32, opener=opener)
                with self.assertRaises(fork.MutationAmbiguous):
                    transport.request("POST", fork.DATABASES_PATH, b"{}")
                self.assertEqual(opener.calls, 1)

        delete_url = common.API_ORIGIN + f"/v2/databases/{FORK_ID}"
        opener = FakeOpener(
            FakeHTTPResponse(
                status=204,
                url=delete_url,
                content_type="application/json",
                raw=b"unexpected",
            )
        )
        transport = fork.DigitalOceanTransport("x" * 32, opener=opener)
        with self.assertRaises(fork.MutationAmbiguous):
            transport.request("DELETE", f"/v2/databases/{FORK_ID}")
        self.assertEqual(opener.calls, 1)

    def test_expired_create_receipt_remains_valid_for_cleanup_only(self) -> None:
        receipt, _ = self.create_receipt()
        receipt_sha = common.sha256_bytes(common.canonical_file_bytes(receipt))
        no_mutation_evidence = {
            "mode": "no-mutation",
            "authority_set_sha256": "f" * 64,
        }
        source = database(SOURCE_ID, SOURCE_NAME, created_at="2025-01-01T00:00:00Z")
        transport = FakeCapabilities(
            source_read_steps()
            + [
                (
                    "GET",
                    fork.LIST_PATH,
                    inventory_result(source),
                ),
            ]
            + cleanup_round_steps(source)
            + cleanup_round_steps(source)
        )
        deleted = fork.delete_or_reconcile(
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            contract=self.contract,
            contract_file_sha256=self.contract_sha,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
            cleanup_mode="no-mutation",
            cleanup_evidence=no_mutation_evidence,
            read_transport=transport.read,
            mutation_transport=transport.mutation,
            now=NOW + dt.timedelta(days=2),
        )
        self.assertEqual(deleted["result"]["outcome"], "already-absent")
        self.assertEqual(deleted["result"]["deletion_request_attempt_count"], 0)
        fork.validate_delete_receipt(
            deleted,
            exact_sha256=common.sha256_bytes(common.canonical_file_bytes(deleted)),
            target=TARGET,
            control=self.cleanup_control,
            phase=PHASE,
            now=NOW + dt.timedelta(days=2),
            cleanup_mode="no-mutation",
            cleanup_evidence=no_mutation_evidence,
            create_receipt=receipt,
            create_receipt_sha256=receipt_sha,
        )


if __name__ == "__main__":
    unittest.main()
