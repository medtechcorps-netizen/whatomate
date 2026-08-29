#!/usr/bin/env python3
"""Observe provider-native PostgreSQL and Valkey recovery readiness without mutation."""

from __future__ import annotations

import argparse
import datetime as dt
import decimal
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import provider_native_valkey_recovery as fork_control
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/verify-production-recovery-readiness.yml"
FORK_WORKFLOW_PATH = fork_control.PREPARE_WORKFLOW_PATH
AUTHORITY = "production-recovery-readiness"
FORK_RECEIPT_AUTHORITY = fork_control.CREATE_AUTHORITY
FORK_PROOF_AUTHORITY = fork_control.READINESS_AUTHORITY
PROVIDER_COPY_CONTRACT = fork_control.PROVIDER_COPY_CONTRACT
MAX_BACKUP_AGE = dt.timedelta(hours=36)
MAX_FORK_AGE = dt.timedelta(hours=24)
APP_TO_DATABASE_REGION = {"sgp": "sgp1"}
FORK_NAME_RE = re.compile(
    r"rereply-recovery-(?:baseline|bridge|backend|ui)-[0-9a-f]{8}-[1-9][0-9]{0,14}-1"
)
PHASES = {"baseline", "bridge", "backend", "ui"}
STABLE_LABELS = [
    "postgres-cluster",
    "postgres-backups",
    "valkey-cluster",
    "valkey-config",
    "valkey-source-firewall",
    "valkey-recovery-cluster",
    "valkey-recovery-config",
    "valkey-recovery-firewall",
]
DISCOVERY_LABEL = "valkey-recovery-discovery"


def _provider_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            common.fail("provider JSON contains a duplicate key")
        value[key] = item
    return value


def _reject_provider_constant(raw: str) -> None:
    del raw
    common.fail("provider JSON contains a non-finite number")


def loads_provider_json(raw: bytes) -> Any:
    try:
        return json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_provider_pairs,
            parse_float=decimal.Decimal,
            parse_constant=_reject_provider_constant,
        )
    except common.ReleaseError:
        raise
    except (
        UnicodeError,
        json.JSONDecodeError,
        TypeError,
        ValueError,
        decimal.InvalidOperation,
    ) as exc:
        raise common.ReleaseError("provider JSON is malformed") from exc


def _provider_projection(value: Any) -> Any:
    if type(value) is dict:
        return [
            "object",
            [[key, _provider_projection(value[key])] for key in sorted(value)],
        ]
    if type(value) is list:
        return ["array", [_provider_projection(item) for item in value]]
    if type(value) is str:
        return ["string", value]
    if type(value) is bool:
        return ["boolean", value]
    if value is None:
        return ["null"]
    if type(value) is int:
        return ["integer", value]
    if type(value) is decimal.Decimal:
        digits = value.as_tuple().digits
        exponent = value.as_tuple().exponent
        if not value.is_finite() or len(digits) > 100 or abs(exponent) > 1000:
            common.fail("provider decimal is outside the canonical bound")
        return ["decimal", str(value)]
    common.fail("provider JSON contains an unsupported value")


def _provider_sha256(value: Any) -> str:
    return common.sha256_value(_provider_projection(value))


def _identity_projection(target: Mapping[str, str]) -> dict[str, str]:
    return {
        "postgresql_identity_sha256": common.sha256_bytes(
            target["postgres_cluster_id"].encode("utf-8")
        ),
        "valkey_identity_sha256": common.sha256_bytes(
            target["valkey_cluster_id"].encode("utf-8")
        ),
    }


def _stable_target_descriptor_sha256(target: Mapping[str, str]) -> str:
    return fork_control.target_descriptor_sha256(target)


def _database(
    value: Any,
    expected_id: str,
    expected_engine: set[str],
    label: str,
    *,
    expected_cluster_sha256: str | None = None,
    expected_region: str | None = None,
    expected_version: str | None = None,
) -> dict[str, Any]:
    database = fork_control._database_envelope(value, label)
    identity = database["id"]
    if identity != expected_id:
        common.fail(f"{label} identity differs")
    identity_sha256 = database["safe"]["identity_sha256"]
    status = database["status"]
    if status != "online":
        common.fail(f"{label} is not online")
    raw_engine = database["engine"]
    if raw_engine not in expected_engine:
        common.fail(f"{label} engine differs")
    engine = "postgresql" if raw_engine in {"pg", "postgres", "postgresql"} else "valkey"
    version = database["version"]
    region = database["region"]
    if expected_version is not None and version != common.exact_string(
        expected_version, f"{label} contract version"
    ):
        common.fail(f"{label} version differs from the production contract")
    if expected_region is not None and region != common.exact_string(
        expected_region, f"{label} contract region"
    ):
        common.fail(f"{label} region differs from the production contract")
    created_at_raw = database["created_at"]
    created_at = common.require_timestamp(created_at_raw, f"{label} created_at")
    name = database["name"]
    name_sha256 = database["safe"]["name_sha256"]
    if expected_cluster_sha256 is not None and name_sha256 != common.require_sha256(
        expected_cluster_sha256, f"{label} production cluster hash"
    ):
        common.fail(f"{label} is not the contract-bound production cluster")
    return {
        "identity": identity,
        "identity_sha256": identity_sha256,
        "status": status,
        "engine": engine,
        "version": version,
        "region": region,
        "created_at": created_at,
        "created_at_raw": created_at_raw,
        "name": name,
        "name_sha256": name_sha256,
        "observation_sha256": database["observation_sha256"],
        "topology_sha256": database["topology_sha256"],
    }


def _config(value: Any, label: str) -> dict[str, Any]:
    _config_value, config_sha256 = fork_control._config_projection(value, label)
    return {"persistence": "rdb", "sha256": config_sha256}


def _firewall(value: Any, label: str) -> dict[str, Any]:
    projection = fork_control._firewall_projection(value, label)
    return {
        "count": len(projection["policy"]),
        "sha256": projection["sha256"],
        "policy": projection["policy"],
        "policy_sha256": projection["policy_sha256"],
    }


def _fresh_backup(value: Any, now: dt.datetime) -> dict[str, Any]:
    if (
        type(value) is not dict
        or "backup_progress" in value
        or type(value.get("backups")) is not list
        or not value["backups"]
    ):
        common.fail("PostgreSQL backup inventory is empty")
    timestamps: list[tuple[dt.datetime, Any]] = []
    for item in value["backups"]:
        if type(item) is not dict:
            common.fail("PostgreSQL backup inventory is malformed")
        size = item.get("size_gigabytes")
        size_valid = (
            (type(size) is int and size > 0)
            or (type(size) is decimal.Decimal and size.is_finite() and size > 0)
        )
        if not size_valid:
            common.fail("PostgreSQL backup size is missing or invalid")
        timestamps.append(
            (
                common.require_timestamp(item.get("created_at"), "PostgreSQL backup created_at"),
                item,
            )
        )
    newest, record = max(timestamps, key=lambda pair: pair[0])
    checked = now.replace(microsecond=0)
    if newest > checked or checked - newest > MAX_BACKUP_AGE:
        common.fail("PostgreSQL backup is stale or future-dated")
    return {
        "fresh": True,
        "newest_identity_sha256": _provider_sha256(record),
        "inventory_sha256": _provider_sha256(value["backups"]),
    }


def contract_database_bindings(contract: Any) -> dict[str, str]:
    if (
        type(contract) is not dict
        or type(contract.get("expected_topology")) is not dict
        or type(contract.get("provider")) is not dict
    ):
        common.fail("production contract topology is malformed")
    topology = contract["expected_topology"]
    app_region = common.exact_string(topology.get("region"), "production contract region")
    if app_region not in APP_TO_DATABASE_REGION:
        common.fail("production contract has no reviewed database region mapping")
    region = APP_TO_DATABASE_REGION[app_region]
    databases = topology.get("databases")
    if type(databases) is not list or len(databases) != 2:
        common.fail("production contract database inventory differs")
    output: dict[str, dict[str, str]] = {}
    for item in databases:
        item = common.exact_keys(
            item,
            {"engine", "version", "production", "name_sha256", "cluster_sha256"},
            "production contract database",
        )
        engine = common.exact_string(item["engine"], "production contract database engine")
        if engine not in {"PG", "VALKEY"} or engine in output or item["production"] is not True:
            common.fail("production contract database authority differs")
        output[engine] = {
            "version": common.exact_string(
                item["version"], "production contract database version"
            ),
            "cluster_sha256": common.require_sha256(
                item["cluster_sha256"], "production database cluster hash"
            ),
        }
        common.require_sha256(item["name_sha256"], "production database binding hash")
    if set(output) != {"PG", "VALKEY"}:
        common.fail("production contract database engines differ")
    return {
        "app_id_sha256": common.require_sha256(
            contract["provider"].get("app_id_sha256"),
            "production app identity hash",
        ),
        "region": region,
        "region_sha256": common.sha256_bytes(region.encode("utf-8")),
        "postgresql_cluster_sha256": output["PG"]["cluster_sha256"],
        "postgresql_version": output["PG"]["version"],
        "valkey_cluster_sha256": output["VALKEY"]["cluster_sha256"],
        "valkey_version": output["VALKEY"]["version"],
    }


class DatabaseReadClient:
    """GET-only client that discovers one exact restricted fork then performs two stable rounds."""

    def __init__(
        self,
        target: Mapping[str, str],
        token: str,
        *,
        opener: Any | None = None,
    ) -> None:
        if type(token) is not str or len(token) < 20 or any(ch in token for ch in "\r\n\x00"):
            common.fail("database read token is invalid")
        self.target = dict(target)
        self._token = token
        self._opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), common.RejectRedirects()
        )
        self.request_log: list[tuple[str, str]] = []
        self.paths: dict[str, str] = {}
        self.recovery_fork_id = ""
        self.recovery_fork_name = ""

    def _get(self, label: str, path: str) -> Any:
        url = common.API_ORIGIN + path
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "https"
            or parsed.hostname != "api.digitalocean.com"
            or parsed.port not in (None, 443)
            or parsed.username is not None
            or parsed.password is not None
            or parsed.fragment
            or parsed.query not in {"", "page=1&per_page=200"}
        ):
            common.fail("database URL is outside the exact provider origin")
        request = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._token}",
                "User-Agent": "rereply-production-recovery/2",
            },
        )
        try:
            with self._opener.open(request, timeout=20) as response:
                if response.geturl() != url:
                    common.fail("database response URL differs")
                status = getattr(response, "status", None)
                if status is None:
                    status = response.getcode()
                if status != 200:
                    common.fail("database observation returned non-success")
                content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
                if content_type != "application/json":
                    common.fail("database observation content type differs")
                raw = response.read(common.MAX_JSON_BYTES + 1)
        except common.ReleaseError:
            raise
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError) as exc:
            raise common.ReleaseError("database observation failed") from exc
        if not raw or len(raw) > common.MAX_JSON_BYTES:
            common.fail("database observation size differs")
        self.request_log.append(("GET", label))
        return loads_provider_json(raw)

    def discover_fork(self, exact_name: str) -> str:
        if not FORK_NAME_RE.fullmatch(exact_name) or len(exact_name) > 63:
            common.fail("Valkey recovery fork name differs")
        value = self._get(DISCOVERY_LABEL, "/v2/databases?page=1&per_page=200")
        if type(value) is not dict or type(value.get("databases")) is not list:
            common.fail("database discovery response is malformed")
        databases = value["databases"]
        meta = value.get("meta")
        if (
            type(meta) is not dict
            or common.exact_int(meta.get("total"), "database discovery total", 0, 200)
            != len(databases)
        ):
            common.fail("database discovery inventory is incomplete")
        matches: list[dict[str, str]] = []
        for item in databases:
            if type(item) is not dict:
                common.fail("database discovery record is malformed")
            name = common.exact_string(item.get("name"), "database discovery name")
            identity = common.require_uuid(item.get("id"), "database discovery identity")
            if name == exact_name:
                matches.append({"id": identity, "name": name})
        if len(matches) != 1:
            common.fail("exact Valkey recovery fork discovery is not unique")
        fork = matches[0]
        if fork["id"] in self.target.values():
            common.fail("Valkey recovery fork is not distinct")
        self.recovery_fork_id = fork["id"]
        self.recovery_fork_name = fork["name"]
        postgres = self.target["postgres_cluster_id"]
        source = self.target["valkey_cluster_id"]
        recovery = self.recovery_fork_id
        self.paths = {
            "postgres-cluster": f"/v2/databases/{postgres}",
            "postgres-backups": f"/v2/databases/{postgres}/backups?page=1&per_page=200",
            "valkey-cluster": f"/v2/databases/{source}",
            "valkey-config": f"/v2/databases/{source}/config",
            "valkey-source-firewall": f"/v2/databases/{source}/firewall",
            "valkey-recovery-cluster": f"/v2/databases/{recovery}",
            "valkey-recovery-config": f"/v2/databases/{recovery}/config",
            "valkey-recovery-firewall": f"/v2/databases/{recovery}/firewall",
        }
        if list(self.paths) != STABLE_LABELS or len(set(self.paths.values())) != 8:
            common.fail("database endpoint inventory differs")
        return self.recovery_fork_id

    def get_label(self, label: str) -> Any:
        if label not in self.paths:
            common.fail("database endpoint label is outside the exact allowlist")
        return self._get(label, self.paths[label])


def _production_plan_authority(
    plan: Mapping[str, Any],
    authority: Mapping[str, Any],
    *,
    exact_sha256: str,
    control_sha: str,
) -> dict[str, Any]:
    supplied = common.exact_keys(
        dict(authority), {"run_id", "run_attempt", "sha256"},
        "production plan authority",
    )
    normalized = {
        "run_id": common.require_run_id(supplied["run_id"], "production plan run ID"),
        "run_attempt": common.exact_int(
            supplied["run_attempt"], "production plan run attempt", 1, 1
        ),
        "sha256": common.require_sha256(supplied["sha256"], "production plan hash"),
    }
    if normalized["sha256"] != common.require_sha256(
        exact_sha256, "production plan exact-file hash"
    ):
        common.fail("production plan exact-file authority differs")
    if (
        type(plan) is not dict
        or plan.get("schema_version") != 2
        or plan.get("authority") != "observation-only-production-plan"
        or plan.get("repository") != common.REPOSITORY
        or type(plan.get("control")) is not dict
        or plan["control"].get("workflow_sha") != control_sha
        or common.require_run_id(plan["control"].get("run_id"), "production plan control run ID")
        != normalized["run_id"]
        or common.exact_int(
            plan["control"].get("run_attempt"), "production plan control attempt", 1, 1
        )
        != normalized["run_attempt"]
    ):
        common.fail("production plan authority differs")
    return normalized


def _plan_phase_and_rollout(plan: Mapping[str, Any]) -> tuple[str, str]:
    transition = common.exact_keys(
        plan.get("transition"), {"operation", "from", "to", "ordinal"},
        "production plan transition",
    )
    if transition["operation"] != "activate":
        common.fail("production plan is not an activation")
    phase = common.exact_string(transition["to"], "production plan target phase")
    if phase not in PHASES:
        common.fail("production plan target phase differs")
    target = plan.get("target")
    if type(target) is not dict or target.get("phase") != phase:
        common.fail("production plan target phase binding differs")
    rollout = plan.get("rollout_authority")
    if type(rollout) is not dict:
        common.fail("production plan rollout authority is malformed")
    return phase, common.require_sha256(
        rollout.get("rollout_plan_sha256"), "production rollout plan hash"
    )


def _fork_name(receipt: Mapping[str, Any]) -> str:
    phase = common.exact_string(receipt.get("phase"), "Valkey fork phase")
    control = receipt.get("control")
    if phase not in PHASES or type(control) is not dict:
        common.fail("Valkey fork phase authority differs")
    workflow_sha = common.require_sha1(control.get("workflow_sha"), "Valkey fork workflow SHA")
    run_id = common.require_run_id(control.get("run_id"), "Valkey fork run ID")
    run_attempt = common.exact_int(
        control.get("run_attempt"), "Valkey fork run attempt", 1, 1
    )
    name = fork_control.deterministic_fork_name(phase, control)
    if not FORK_NAME_RE.fullmatch(name) or len(name) > 63:
        common.fail("Valkey recovery fork name differs")
    return name


def validate_fork_receipt(
    receipt: Mapping[str, Any],
    *,
    receipt_sha256: str,
    fork_authority: Mapping[str, Any],
    plan: Mapping[str, Any],
    plan_authority: Mapping[str, Any],
    control_sha: str,
    contract_sha256: str,
    target: Mapping[str, str],
    now: dt.datetime,
) -> dict[str, Any]:
    receipt = common.exact_keys(
        dict(receipt),
        {
            "schema_version", "authority", "repository", "issued_at", "expires_at",
            "phase", "control", "target", "request", "result", "provider", "gates",
        },
        "Valkey fork receipt",
    )
    exact_receipt_sha = common.require_sha256(
        receipt_sha256, "Valkey fork receipt exact-file hash"
    )
    if common.sha256_bytes(common.canonical_file_bytes(receipt)) != exact_receipt_sha:
        common.fail("Valkey fork receipt exact-file hash differs")
    if (
        receipt["schema_version"] != 2
        or receipt["authority"] != FORK_RECEIPT_AUTHORITY
        or receipt["repository"] != common.REPOSITORY
    ):
        common.fail("Valkey fork receipt authority differs")
    common.validate_fresh_window(
        receipt["issued_at"], receipt["expires_at"], now,
        maximum_age_seconds=int(MAX_FORK_AGE.total_seconds()),
        label="Valkey fork receipt",
    )
    normalized_plan = _production_plan_authority(
        plan, plan_authority, exact_sha256=plan_authority["sha256"], control_sha=control_sha
    )
    phase, rollout_plan_sha256 = _plan_phase_and_rollout(plan)
    if receipt["phase"] != phase:
        common.fail("Valkey fork receipt phase differs from the production plan")
    fork_control.validate_create_receipt(
        receipt,
        exact_sha256=exact_receipt_sha,
        target=target,
        phase=phase,
        now=now,
    )
    supplied_fork = common.exact_keys(
        dict(fork_authority), {"run_id", "run_attempt", "sha256"},
        "Valkey fork run authority",
    )
    normalized_fork = {
        "run_id": common.require_run_id(supplied_fork["run_id"], "Valkey fork run ID"),
        "run_attempt": common.exact_int(
            supplied_fork["run_attempt"], "Valkey fork run attempt", 1, 1
        ),
        "sha256": common.require_sha256(supplied_fork["sha256"], "Valkey fork file hash"),
    }
    if normalized_fork["sha256"] != exact_receipt_sha:
        common.fail("Valkey fork receipt run authority differs")
    control = common.exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "rollout_plan_sha256", "contract_sha256",
            "controller_sha256",
        },
        "Valkey fork receipt control",
    )
    if (
        common.require_sha1(control["workflow_sha"], "Valkey fork workflow SHA")
        != control_sha
        or control["workflow_path"] != FORK_WORKFLOW_PATH
        or common.require_run_id(control["run_id"], "Valkey fork control run ID")
        != normalized_fork["run_id"]
        or common.exact_int(
            control["run_attempt"], "Valkey fork control run attempt", 1, 1
        )
        != normalized_fork["run_attempt"]
        or control["runner_environment"] != "github-hosted"
        or common.require_sha256(
            control["rollout_plan_sha256"], "Valkey fork rollout plan hash"
        )
        != rollout_plan_sha256
        or common.require_sha256(
            control["contract_sha256"], "Valkey fork contract hash"
        )
        != contract_sha256
        or common.require_sha256(
            control["controller_sha256"], "Valkey fork controller hash"
        )
        != common.sha256_bytes(Path(fork_control.__file__).resolve().read_bytes())
    ):
        common.fail("Valkey fork receipt control authority differs")
    receipt_target = common.exact_keys(
        receipt["target"],
        {
            "descriptor_sha256", "source_identity_sha256", "source_name_sha256",
            "source_observation_sha256", "source_topology_sha256",
            "source_config_sha256", "source_firewall_sha256",
        },
        "Valkey fork target",
    )
    for key, value in receipt_target.items():
        common.require_sha256(value, f"Valkey fork target {key}")
    identity = _identity_projection(target)
    if (
        receipt_target["descriptor_sha256"] != _stable_target_descriptor_sha256(target)
        or receipt_target["source_identity_sha256"] != identity["valkey_identity_sha256"]
    ):
        common.fail("Valkey fork protected target binding differs")
    request = common.exact_keys(
        receipt["request"],
        {
            "method", "endpoint_label", "request_sha256", "request_attempt_count",
            "provider_copy_contract",
        },
        "Valkey fork request",
    )
    if (
        request["method"] != "POST"
        or request["endpoint_label"] != "database-clusters"
        or common.exact_int(
            request["request_attempt_count"], "Valkey fork request attempt count", 1, 1
        )
        != 1
        or request["provider_copy_contract"] != PROVIDER_COPY_CONTRACT
    ):
        common.fail("Valkey fork request authority differs")
    request_sha256 = common.require_sha256(
        request["request_sha256"], "Valkey fork request hash"
    )
    result = common.exact_keys(
        receipt["result"],
        {
            "outcome", "recovery_identity_sha256", "fork_name_sha256",
            "fork_created_at_sha256", "recovery_observation_sha256",
            "recovery_topology_sha256", "recovery_config_sha256",
            "recovery_firewall_sha256", "mutation_ambiguous_reconciled",
        },
        "Valkey fork result",
    )
    for key in result:
        if key not in {"outcome", "mutation_ambiguous_reconciled"}:
            common.require_sha256(result[key], f"Valkey fork result {key}")
    if (
        result["outcome"] != "created"
        or common.exact_bool(
            result["mutation_ambiguous_reconciled"],
            "Valkey fork ambiguity reconciliation",
        )
        is not False
    ):
        common.fail("ambiguous Valkey fork creation cannot authorize recovery")
    expected_name = _fork_name(receipt)
    if result["fork_name_sha256"] != common.sha256_bytes(expected_name.encode("utf-8")):
        common.fail("Valkey fork deterministic name binding differs")
    if receipt["gates"] != {
        "source_ready": True,
        "fork_ready": True,
        "source_stable": True,
        "source_firewall_exact_app": True,
        "recovery_firewall_exact_source_app": True,
        "recovery_restricted_to_exact_production_app": True,
        "exact_single_mutation": True,
    }:
        common.fail("Valkey fork receipt gates are incomplete")
    if type(receipt["provider"]) is not dict:
        common.fail("Valkey fork provider ledger is malformed")
    common.sanitize_public(receipt)
    return {
        "phase": phase,
        "fork_name": expected_name,
        "production_plan": normalized_plan,
        "valkey_fork": {
            "run_id": normalized_fork["run_id"],
            "run_attempt": normalized_fork["run_attempt"],
            "sha256": exact_receipt_sha,
            "request_sha256": request_sha256,
            "receipt_sha256": exact_receipt_sha,
        },
        "target": dict(receipt_target),
        "result": dict(result),
        "request": dict(request),
    }


def build_readiness(
    *,
    target: Mapping[str, str],
    control: Mapping[str, Any],
    first: Mapping[str, Any],
    second: Mapping[str, Any],
    request_log: Sequence[tuple[str, str]],
    now: dt.datetime,
    contract_sha256: str,
    controller_sha256: str,
    contract_databases: Mapping[str, str],
    fork_evidence: Mapping[str, Any],
    recovery_id: str,
    recovery_name: str,
) -> dict[str, Any]:
    if dict(first) != dict(second):
        common.fail("database recovery state changed between observations")
    expected_log = [("GET", DISCOVERY_LABEL)] + [
        ("GET", label) for label in STABLE_LABELS
    ] * 2
    if list(request_log) != expected_log:
        common.fail("database request ledger differs")
    contract_databases = common.exact_keys(
        dict(contract_databases),
        {
            "app_id_sha256", "region", "region_sha256", "postgresql_cluster_sha256",
            "postgresql_version", "valkey_cluster_sha256", "valkey_version",
        },
        "production contract database bindings",
    )
    contract_region = common.exact_string(
        contract_databases["region"], "production database region"
    )
    contract_region_sha256 = common.require_sha256(
        contract_databases["region_sha256"], "production database region hash"
    )
    if contract_region_sha256 != common.sha256_bytes(contract_region.encode("utf-8")):
        common.fail("production database region authority differs")
    postgres_cluster_sha256 = common.require_sha256(
        contract_databases["postgresql_cluster_sha256"],
        "production PostgreSQL cluster hash",
    )
    valkey_cluster_sha256 = common.require_sha256(
        contract_databases["valkey_cluster_sha256"], "production Valkey cluster hash"
    )
    postgres = _database(
        first["postgres-cluster"], target["postgres_cluster_id"],
        {"pg", "postgres", "postgresql"}, "PostgreSQL",
        expected_cluster_sha256=postgres_cluster_sha256,
        expected_region=contract_region,
        expected_version=contract_databases["postgresql_version"],
    )
    backup = _fresh_backup(first["postgres-backups"], now)
    source = _database(
        first["valkey-cluster"], target["valkey_cluster_id"], {"redis", "valkey"},
        "Valkey", expected_cluster_sha256=valkey_cluster_sha256,
        expected_region=contract_region,
        expected_version=contract_databases["valkey_version"],
    )
    recovery = _database(
        first["valkey-recovery-cluster"], recovery_id, {"redis", "valkey"},
        "Valkey recovery fork", expected_region=contract_region,
        expected_version=contract_databases["valkey_version"],
    )
    source_config = _config(first["valkey-config"], "Valkey")
    recovery_config = _config(first["valkey-recovery-config"], "Valkey recovery fork")
    source_firewall = _firewall(first["valkey-source-firewall"], "Valkey")
    recovery_firewall = _firewall(
        first["valkey-recovery-firewall"], "Valkey recovery fork"
    )
    if (
        source["identity"] == recovery["identity"]
        or source["topology_sha256"] != recovery["topology_sha256"]
        or source_config["sha256"] != recovery_config["sha256"]
        or source_firewall["count"] != 1
        or recovery_firewall["count"] != 1
        or source_firewall["policy"]
        != [{"type": "app", "value_sha256": contract_databases["app_id_sha256"]}]
        or recovery_firewall["policy"] != source_firewall["policy"]
        or recovery_firewall["policy_sha256"]
        != source_firewall["policy_sha256"]
    ):
        common.fail(
            "Valkey recovery fork is not an exact provider copy restricted to the production app"
        )
    checked = now.replace(microsecond=0)
    if recovery["created_at"] > checked or checked - recovery["created_at"] > MAX_FORK_AGE:
        common.fail("Valkey recovery fork is stale or future-dated")
    if recovery["name"] != recovery_name:
        common.fail("Valkey recovery fork discovery name differs")
    receipt_target = fork_evidence["target"]
    receipt_result = fork_evidence["result"]
    if (
        receipt_target["source_identity_sha256"] != source["identity_sha256"]
        or receipt_target["source_name_sha256"] != source["name_sha256"]
        or receipt_target["source_observation_sha256"] != source["observation_sha256"]
        or receipt_target["source_topology_sha256"] != source["topology_sha256"]
        or receipt_target["source_config_sha256"] != source_config["sha256"]
        or receipt_target["source_firewall_sha256"] != source_firewall["sha256"]
        or receipt_result["recovery_identity_sha256"] != recovery["identity_sha256"]
        or receipt_result["fork_name_sha256"] != recovery["name_sha256"]
        or receipt_result["fork_created_at_sha256"]
        != common.sha256_bytes(recovery["created_at_raw"].encode("utf-8"))
        or receipt_result["recovery_observation_sha256"]
        != recovery["observation_sha256"]
        or receipt_result["recovery_topology_sha256"] != recovery["topology_sha256"]
        or receipt_result["recovery_config_sha256"] != recovery_config["sha256"]
        or receipt_result["recovery_firewall_sha256"] != recovery_firewall["sha256"]
    ):
        common.fail("Valkey provider fork receipt differs from stable observation")
    stable_identity = _identity_projection(target)
    recovery_identity_sha256 = recovery["identity_sha256"]
    identity_projection_sha256 = common.sha256_value(
        {
            **stable_identity,
            "valkey_recovery_identity_sha256": recovery_identity_sha256,
        }
    )
    issued_at = common.format_timestamp(checked)
    expires_at = common.format_timestamp(
        checked + dt.timedelta(seconds=common.MAX_RECOVERY_AGE_SECONDS)
    )
    result = {
        "schema_version": 2,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": issued_at,
        "expires_at": expires_at,
        "control": {
            **dict(control),
            "contract_sha256": common.require_sha256(
                contract_sha256, "recovery contract hash"
            ),
            "controller_sha256": common.require_sha256(
                controller_sha256, "recovery controller hash"
            ),
        },
        "authorities": {
            "production_plan": dict(fork_evidence["production_plan"]),
            "valkey_fork": dict(fork_evidence["valkey_fork"]),
        },
        "target": {
            "descriptor_sha256": identity_projection_sha256,
            "contract_sha256": common.require_sha256(
                contract_sha256, "recovery contract hash"
            ),
            **stable_identity,
            "valkey_recovery_identity_sha256": recovery_identity_sha256,
            "identity_projection_sha256": identity_projection_sha256,
            "postgresql_cluster_sha256": postgres_cluster_sha256,
            "valkey_cluster_sha256": valkey_cluster_sha256,
            "region_sha256": contract_region_sha256,
        },
        "postgresql": {
            "identity_sha256": postgres["identity_sha256"],
            "observation_sha256": postgres["observation_sha256"],
            "status": "online",
            "engine": "postgresql",
            "version": postgres["version"],
            "region_sha256": common.sha256_bytes(postgres["region"].encode("utf-8")),
            "fresh_backup": backup["fresh"],
            "backup_identity_sha256": backup["newest_identity_sha256"],
            "backup_inventory_sha256": backup["inventory_sha256"],
            "point_in_time_restore_ready": True,
            "production_cluster_sha256": postgres_cluster_sha256,
        },
        "valkey": {
            "identity_sha256": source["identity_sha256"],
            "recovery_identity_sha256": recovery_identity_sha256,
            "source_observation_sha256": source["observation_sha256"],
            "recovery_observation_sha256": recovery["observation_sha256"],
            "status": "online",
            "recovery_status": "online",
            "version": source["version"],
            "recovery_version": recovery["version"],
            "region_sha256": common.sha256_bytes(source["region"].encode("utf-8")),
            "recovery_region_sha256": common.sha256_bytes(
                recovery["region"].encode("utf-8")
            ),
            "source_topology_sha256": source["topology_sha256"],
            "recovery_topology_sha256": recovery["topology_sha256"],
            "persistence": source_config["persistence"],
            "recovery_persistence": recovery_config["persistence"],
            "recovery_is_distinct": True,
            "recovery_is_fresh": True,
            "topology_equal": True,
            "production_cluster_sha256": valkey_cluster_sha256,
            "provider_fork": {
                "authority": FORK_PROOF_AUTHORITY,
                "source_identity_sha256": source["identity_sha256"],
                "recovery_identity_sha256": recovery_identity_sha256,
                "request_sha256": fork_evidence["valkey_fork"]["request_sha256"],
                "receipt_sha256": fork_evidence["valkey_fork"]["receipt_sha256"],
                "source_config_sha256": source_config["sha256"],
                "recovery_config_sha256": recovery_config["sha256"],
                "source_firewall_sha256": source_firewall["sha256"],
                "recovery_firewall_sha256": recovery_firewall["sha256"],
                "fork_name_sha256": recovery["name_sha256"],
                "fork_created_at_sha256": common.sha256_bytes(
                    recovery["created_at_raw"].encode("utf-8")
                ),
                "provider_copy_contract": PROVIDER_COPY_CONTRACT,
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
            "http_endpoint_labels": [DISCOVERY_LABEL] + STABLE_LABELS * 2,
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
    common.sanitize_public(
        result,
        private_values=tuple(target.values()) + (recovery_id, recovery_name),
    )
    return result


def observe(
    *,
    target: Mapping[str, str],
    control: Mapping[str, Any],
    token: str,
    contract_sha256: str,
    controller_sha256: str,
    contract: Mapping[str, Any],
    production_plan: Mapping[str, Any],
    production_plan_authority: Mapping[str, Any],
    fork_receipt: Mapping[str, Any],
    fork_receipt_sha256: str,
    fork_authority: Mapping[str, Any],
    now: dt.datetime | None = None,
    opener: Any | None = None,
) -> dict[str, Any]:
    for forbidden in (
        "DIGITALOCEAN_ACCESS_TOKEN", "DO_ACCESS_TOKEN", "DO_TOKEN",
        "DO_PRODUCTION_APPLY_TOKEN", "DO_PRODUCTION_DATABASE_FORK_TOKEN",
        "DO_PRODUCTION_DATABASE_CREATE_TOKEN",
        "DO_PRODUCTION_DATABASE_DELETE_TOKEN",
    ):
        if os.environ.get(forbidden):
            common.fail("a forbidden ambient production credential is present")
    target = common.validate_target_descriptor(dict(target), recovery=True)
    checked = (now or dt.datetime.now(dt.timezone.utc)).replace(microsecond=0)
    databases = contract_database_bindings(contract)
    fork_evidence = validate_fork_receipt(
        fork_receipt,
        receipt_sha256=fork_receipt_sha256,
        fork_authority=fork_authority,
        plan=production_plan,
        plan_authority=production_plan_authority,
        control_sha=control["workflow_sha"],
        contract_sha256=contract_sha256,
        target=target,
        now=checked,
    )
    client = DatabaseReadClient(target, token, opener=opener)
    recovery_id = client.discover_fork(fork_evidence["fork_name"])
    first = {label: client.get_label(label) for label in STABLE_LABELS}
    second = {label: client.get_label(label) for label in STABLE_LABELS}
    result = build_readiness(
        target=target,
        control=control,
        first=first,
        second=second,
        request_log=client.request_log,
        now=checked,
        contract_sha256=contract_sha256,
        controller_sha256=controller_sha256,
        contract_databases=databases,
        fork_evidence=fork_evidence,
        recovery_id=recovery_id,
        recovery_name=client.recovery_fork_name,
    )
    common.sanitize_public(
        result,
        private_values=tuple(target.values()) + (recovery_id, client.recovery_fork_name),
    )
    return result


def _load_exact(path: Path, expected_sha256: str, label: str) -> dict[str, Any]:
    if path.is_symlink() or not path.is_file():
        common.fail(f"{label} is not a regular file")
    raw = path.read_bytes()
    if not raw or len(raw) > common.MAX_JSON_BYTES:
        common.fail(f"{label} has an invalid size")
    value = common.loads_strict(raw)
    if (
        raw != common.canonical_file_bytes(value)
        or common.sha256_bytes(raw) != common.require_sha256(expected_sha256, f"{label} hash")
    ):
        common.fail(f"{label} exact-file encoding differs")
    return value


def _load_raw_bound(path: Path, expected_sha256: str, label: str) -> dict[str, Any]:
    if path.is_symlink() or not path.is_file():
        common.fail(f"{label} is not a regular file")
    raw = path.read_bytes()
    if not raw or len(raw) > common.MAX_JSON_BYTES:
        common.fail(f"{label} has an invalid size")
    value = common.loads_strict(raw)
    if common.sha256_bytes(raw) != common.require_sha256(expected_sha256, f"{label} hash"):
        common.fail(f"{label} exact-file hash differs")
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Observe production recovery readiness")
    parser.add_argument("--contract", required=True)
    parser.add_argument("--production-plan", required=True)
    parser.add_argument("--production-plan-sha256", required=True)
    parser.add_argument("--production-plan-run-id", required=True)
    parser.add_argument("--production-plan-run-attempt", required=True, type=int)
    parser.add_argument("--fork-receipt", required=True)
    parser.add_argument("--fork-receipt-sha256", required=True)
    parser.add_argument("--fork-run-id", required=True)
    parser.add_argument("--fork-run-attempt", required=True, type=int)
    parser.add_argument("--output", required=True)
    parser.add_argument("--contract-sha256", required=True)
    parser.add_argument("--controller-sha256", required=True)
    parser.add_argument("--workflow-sha", required=True)
    parser.add_argument("--workflow-run-id", required=True)
    parser.add_argument("--workflow-run-attempt", required=True, type=int)
    parser.add_argument("--runner-temp", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    target_raw = os.environ.pop("DO_PRODUCTION_DATABASE_TARGET_JSON", "")
    target = common.loads_strict(target_raw)
    del target_raw
    contract_path = Path(args.contract)
    contract = _load_raw_bound(
        contract_path, args.contract_sha256, "production app contract"
    )
    production_plan = _load_exact(
        Path(args.production_plan), args.production_plan_sha256, "production plan"
    )
    fork_receipt = _load_exact(
        Path(args.fork_receipt), args.fork_receipt_sha256, "Valkey fork receipt"
    )
    control = {
        "workflow_sha": common.require_sha1(args.workflow_sha, "workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(args.workflow_run_id, "workflow run ID"),
        "run_attempt": common.exact_int(
            args.workflow_run_attempt, "workflow run attempt", 1, 1
        ),
        "runner_environment": "github-hosted",
    }
    token = os.environ.pop("DO_PRODUCTION_DATABASE_READ_TOKEN", "")
    result = observe(
        target=target,
        control=control,
        token=token,
        contract_sha256=args.contract_sha256,
        controller_sha256=args.controller_sha256,
        contract=contract,
        production_plan=production_plan,
        production_plan_authority={
            "run_id": args.production_plan_run_id,
            "run_attempt": args.production_plan_run_attempt,
            "sha256": args.production_plan_sha256,
        },
        fork_receipt=fork_receipt,
        fork_receipt_sha256=args.fork_receipt_sha256,
        fork_authority={
            "run_id": args.fork_run_id,
            "run_attempt": args.fork_run_attempt,
            "sha256": args.fork_receipt_sha256,
        },
    )
    del token, target, production_plan, fork_receipt
    common.write_canonical_output(Path(args.output), result, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"recovery readiness failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
