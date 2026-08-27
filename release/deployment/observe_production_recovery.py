#!/usr/bin/env python3
"""Observe PostgreSQL and Valkey production recovery readiness without mutation."""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import decimal
import hashlib
import hmac
import json
import os
import re
import socket
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_release as common


WORKFLOW_PATH = ".github/workflows/verify-production-recovery-readiness.yml"
AUTHORITY = "production-recovery-readiness"
MAX_BACKUP_AGE = dt.timedelta(hours=36)
MAX_FORK_AGE = dt.timedelta(hours=24)
MAX_SENTINEL_AGE = dt.timedelta(hours=24)
SENTINEL_KEY = "rereply:recovery:sentinel:v1"
SENTINEL_AUTHORITY = "rereply-valkey-recovery-sentinel-v1"
HOST_RE = re.compile(r"^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
APP_TO_DATABASE_REGION = {"sgp": "sgp1"}


class DatabaseReadClient:
    """Exact-path GET-only client for the three protected database identities."""

    def __init__(
        self,
        target: Mapping[str, str],
        token: str,
        *,
        opener: Any | None = None,
    ) -> None:
        if type(token) is not str or len(token) < 20 or any(ch in token for ch in "\r\n\x00"):
            common.fail("database read token is invalid")
        postgres = target["postgres_cluster_id"]
        valkey = target["valkey_cluster_id"]
        fork = target["valkey_recovery_cluster_id"]
        self.paths = {
            "postgres-cluster": f"/v2/databases/{postgres}",
            "postgres-backups": f"/v2/databases/{postgres}/backups?page=1&per_page=200",
            "valkey-cluster": f"/v2/databases/{valkey}",
            "valkey-config": f"/v2/databases/{valkey}/config",
            "valkey-recovery-cluster": f"/v2/databases/{fork}",
            "valkey-recovery-config": f"/v2/databases/{fork}/config",
        }
        if len(set(self.paths.values())) != 6 or valkey == fork:
            common.fail("recovery target identities are not distinct")
        self._token = token
        self._opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), common.RejectRedirects()
        )
        self.request_log: list[tuple[str, str]] = []

    def get_label(self, label: str) -> Any:
        if label not in self.paths:
            common.fail("database endpoint label is outside the exact allowlist")
        path = self.paths[label]
        url = common.API_ORIGIN + path
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "https"
            or parsed.hostname != "api.digitalocean.com"
            or parsed.port not in (None, 443)
            or parsed.username is not None
            or parsed.password is not None
            or parsed.fragment
            or (parsed.query not in {"", "page=1&per_page=200"})
        ):
            common.fail("database URL is outside the exact provider origin")
        request = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._token}",
                "User-Agent": "rereply-production-recovery/1",
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
        value = loads_provider_json(raw)
        self.request_log.append(("GET", label))
        return value


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
    if type(value) is not dict or type(value.get("database")) is not dict:
        common.fail(f"{label} response is malformed")
    database = value["database"]
    if common.require_uuid(database.get("id"), f"{label} identity") != expected_id:
        common.fail(f"{label} identity differs")
    status = common.exact_string(database.get("status"), f"{label} status").lower()
    if status != "online":
        common.fail(f"{label} is not online")
    engine = common.exact_string(database.get("engine"), f"{label} engine").lower()
    if engine not in expected_engine:
        common.fail(f"{label} engine differs")
    version = common.exact_string(database.get("version"), f"{label} version")
    if expected_version is not None and version != common.exact_string(
        expected_version, f"{label} contract version"
    ):
        common.fail(f"{label} version differs from the production contract")
    region = common.exact_string(database.get("region"), f"{label} region")
    if expected_region is not None and region != common.exact_string(
        expected_region, f"{label} contract region"
    ):
        common.fail(f"{label} region differs from the production contract")
    created_at = common.require_timestamp(database.get("created_at"), f"{label} created_at")
    name = common.exact_string(database.get("name"), f"{label} name")
    name_sha256 = common.sha256_bytes(name.encode("utf-8"))
    if expected_cluster_sha256 is not None and name_sha256 != common.require_sha256(
        expected_cluster_sha256, f"{label} production cluster hash"
    ):
        common.fail(f"{label} is not the contract-bound production cluster")
    size = common.exact_string(database.get("size"), f"{label} size")
    num_nodes = common.exact_int(database.get("num_nodes"), f"{label} node count", 1, 100)
    private_network_uuid = common.require_uuid(
        database.get("private_network_uuid"), f"{label} private network identity"
    )
    raw_storage_size_mib = database.get("storage_size_mib")
    storage_size_mib = (
        None
        if raw_storage_size_mib is None
        else common.exact_int(
            raw_storage_size_mib,
            f"{label} storage size",
            1,
            1_000_000_000,
        )
    )
    topology_projection = {
        "engine": "postgresql" if engine in {"pg", "postgres", "postgresql"} else "valkey",
        "version": version,
        "region": region,
        "size": size,
        "num_nodes": num_nodes,
        "private_network_uuid_sha256": common.sha256_bytes(
            private_network_uuid.encode("utf-8")
        ),
        "storage_size_mib": storage_size_mib,
    }
    return {
        "id": expected_id,
        "status": status,
        "engine": "postgresql" if engine in {"pg", "postgres", "postgresql"} else "valkey",
        "version": version,
        "region": region,
        "created_at": created_at,
        "name_sha256": name_sha256,
        "raw_sha256": _provider_sha256(database),
        "topology_sha256": common.sha256_value(topology_projection),
        "connection_endpoints": _connection_endpoints(database, label),
    }


def _connection_endpoints(database: Mapping[str, Any], label: str) -> set[tuple[str, int]]:
    endpoints: set[tuple[str, int]] = set()
    for key in ("connection", "private_connection"):
        value = database.get(key)
        if value is None:
            continue
        if type(value) is not dict:
            common.fail(f"{label} {key} is malformed")
        host = common.exact_string(value.get("host"), f"{label} {key} host").lower()
        port = common.exact_int(value.get("port"), f"{label} {key} port", 1, 65535)
        endpoints.add((host, port))
    if not endpoints:
        common.fail(f"{label} has no provider-bound connection endpoint")
    return endpoints


def contract_database_bindings(contract: Any) -> dict[str, str]:
    if type(contract) is not dict or type(contract.get("expected_topology")) is not dict:
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
        version = common.exact_string(item["version"], "production contract database version")
        common.require_sha256(item["name_sha256"], "production database binding hash")
        output[engine] = {
            "version": version,
            "cluster_sha256": common.require_sha256(
                item["cluster_sha256"], "production database cluster hash"
            ),
        }
    if set(output) != {"PG", "VALKEY"}:
        common.fail("production contract database engines differ")
    return {
        "region": region,
        "region_sha256": common.sha256_bytes(region.encode("utf-8")),
        "postgresql_cluster_sha256": output["PG"]["cluster_sha256"],
        "postgresql_version": output["PG"]["version"],
        "valkey_cluster_sha256": output["VALKEY"]["cluster_sha256"],
        "valkey_version": output["VALKEY"]["version"],
    }


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
        text = raw.decode("utf-8")
        return json.loads(
            text,
            object_pairs_hook=_provider_pairs,
            parse_float=decimal.Decimal,
            parse_constant=_reject_provider_constant,
        )
    except common.ReleaseError:
        raise
    except (UnicodeError, json.JSONDecodeError, TypeError, ValueError, decimal.InvalidOperation) as exc:
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


def _sentinel_connection(value: Any, label: str) -> dict[str, Any]:
    item = common.exact_keys(
        value, {"host", "port", "username", "password", "server_name"}, label
    )
    host = common.exact_string(item["host"], f"{label} host").lower()
    server_name = common.exact_string(item["server_name"], f"{label} server name").lower()
    if not HOST_RE.fullmatch(host) or server_name != host:
        common.fail(f"{label} host is not a canonical TLS name")
    username = common.exact_string(item["username"], f"{label} username")
    password = common.exact_string(item["password"], f"{label} password")
    if len(password) > 1024 or any(character in username + password for character in "\r\n\x00"):
        common.fail(f"{label} credential is malformed")
    return {
        "host": host,
        "port": common.exact_int(item["port"], f"{label} port", 1, 65535),
        "username": username,
        "password": password,
        "server_name": server_name,
    }


def _resp_command(*parts: str) -> bytes:
    encoded = [part.encode("utf-8") for part in parts]
    return (
        f"*{len(encoded)}\r\n".encode("ascii")
        + b"".join(f"${len(part)}\r\n".encode("ascii") + part + b"\r\n" for part in encoded)
    )


def _read_resp_line(handle: Any, maximum: int = 4096) -> bytes:
    line = handle.readline(maximum + 1)
    if not line.endswith(b"\r\n") or len(line) > maximum:
        common.fail("Valkey sentinel response is malformed")
    return line[:-2]


def _read_exact(handle: Any, length: int) -> bytes:
    chunks: list[bytes] = []
    remaining = length
    while remaining:
        chunk = handle.read(remaining)
        if not chunk:
            common.fail("Valkey sentinel response is truncated")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def _write_exact(handle: Any, value: bytes) -> None:
    offset = 0
    while offset < len(value):
        written = handle.write(value[offset:])
        if type(written) is not int or written < 1 or written > len(value) - offset:
            common.fail("Valkey sentinel request write was incomplete")
        offset += written


def read_valkey_sentinel(connection: Mapping[str, Any]) -> bytes:
    """Perform exactly AUTH and one GET over provider-bound TLS; never retry."""
    try:
        with socket.create_connection((connection["host"], connection["port"]), timeout=10) as raw:
            context = ssl.create_default_context()
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            with context.wrap_socket(raw, server_hostname=connection["server_name"]) as tls:
                tls.settimeout(10)
                handle = tls.makefile("rwb", buffering=0)
                _write_exact(
                    handle,
                    _resp_command("AUTH", connection["username"], connection["password"]),
                )
                if _read_resp_line(handle) != b"+OK":
                    common.fail("Valkey sentinel authentication failed")
                _write_exact(handle, _resp_command("GET", SENTINEL_KEY))
                header = _read_resp_line(handle)
                if header == b"$-1":
                    common.fail("Valkey recovery sentinel is missing")
                if not header.startswith(b"$") or not header[1:].isdigit():
                    common.fail("Valkey sentinel response type differs")
                length = int(header[1:])
                if length < 1 or length > 4096:
                    common.fail("Valkey sentinel response size differs")
                value = _read_exact(handle, length)
                if _read_exact(handle, 2) != b"\r\n":
                    common.fail("Valkey sentinel response is truncated")
                return value
    except common.ReleaseError:
        raise
    except (OSError, ssl.SSLError, TimeoutError) as exc:
        raise common.ReleaseError("Valkey sentinel live read failed") from exc


def _sentinel_hmac_key(value: str) -> bytes:
    if type(value) is not str or len(value) > 256 or any(ch in value for ch in "\r\n\x00"):
        common.fail("Valkey sentinel HMAC key is malformed")
    try:
        key = base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as exc:
        raise common.ReleaseError("Valkey sentinel HMAC key is malformed") from exc
    if len(key) != 32:
        common.fail("Valkey sentinel HMAC key length differs")
    return key


def validate_live_sentinel(
    *,
    source_raw: bytes,
    recovery_raw: bytes,
    source_identity_sha256: str,
    source_cluster_sha256: str,
    recovery_created_at: dt.datetime,
    now: dt.datetime,
    hmac_key: bytes,
) -> dict[str, Any]:
    if not source_raw or not hmac.compare_digest(source_raw, recovery_raw):
        common.fail("Valkey source and recovery sentinel values differ")
    marker = common.loads_strict(source_raw)
    marker = common.exact_keys(
        marker,
        {"authority", "issued_at", "nonce", "source_identity_sha256", "source_cluster_sha256", "hmac_sha256"},
        "Valkey recovery sentinel",
    )
    if marker["authority"] != SENTINEL_AUTHORITY:
        common.fail("Valkey recovery sentinel authority differs")
    issued = common.require_timestamp(marker["issued_at"], "Valkey recovery sentinel issued_at")
    checked = now.replace(microsecond=0)
    if issued >= recovery_created_at or issued > checked or checked - issued > MAX_SENTINEL_AGE:
        common.fail("Valkey recovery sentinel is stale, future-dated, or post-fork")
    if marker["source_identity_sha256"] != source_identity_sha256 or marker["source_cluster_sha256"] != source_cluster_sha256:
        common.fail("Valkey recovery sentinel source authority differs")
    nonce = common.exact_string(marker["nonce"], "Valkey recovery sentinel nonce")
    if re.fullmatch(r"[0-9a-f]{64}", nonce) is None:
        common.fail("Valkey recovery sentinel nonce differs")
    supplied = common.require_sha256(marker["hmac_sha256"], "Valkey recovery sentinel HMAC")
    payload = {key: marker[key] for key in marker if key != "hmac_sha256"}
    expected = hmac.new(hmac_key, common.canonical_payload_bytes(payload), hashlib.sha256).hexdigest()
    if not hmac.compare_digest(supplied, expected):
        common.fail("Valkey recovery sentinel HMAC differs")
    return {
        "authority": SENTINEL_AUTHORITY,
        "marker_key_sha256": common.sha256_bytes(SENTINEL_KEY.encode("utf-8")),
        "marker_sha256": common.sha256_bytes(source_raw),
        "source_recovery_equal": True,
        "live_read_count": 2,
        "issued_at_sha256": common.sha256_bytes(marker["issued_at"].encode("utf-8")),
    }


def build_live_sentinel_proof(
    *,
    source_connection: Mapping[str, Any],
    recovery_connection: Mapping[str, Any],
    source_database: Mapping[str, Any],
    recovery_database: Mapping[str, Any],
    source_cluster_sha256: str,
    hmac_key_b64: str,
    now: dt.datetime,
    reader: Any = read_valkey_sentinel,
) -> dict[str, Any]:
    source = _sentinel_connection(source_connection, "Valkey source sentinel connection")
    recovery = _sentinel_connection(recovery_connection, "Valkey recovery sentinel connection")
    try:
        if (source["host"], source["port"]) not in source_database["connection_endpoints"]:
            common.fail("Valkey source sentinel endpoint is not provider-bound")
        if (recovery["host"], recovery["port"]) not in recovery_database["connection_endpoints"]:
            common.fail("Valkey recovery sentinel endpoint is not provider-bound")
        source_endpoint = {"host": source["host"], "port": source["port"]}
        recovery_endpoint = {"host": recovery["host"], "port": recovery["port"]}
        if source_endpoint == recovery_endpoint:
            common.fail("Valkey source and recovery sentinel endpoints are not distinct")
        hmac_key = _sentinel_hmac_key(hmac_key_b64)
        source_raw = reader(source)
        recovery_raw = reader(recovery)
        proof = validate_live_sentinel(
            source_raw=source_raw,
            recovery_raw=recovery_raw,
            source_identity_sha256=common.sha256_bytes(source_database["id"].encode("utf-8")),
            source_cluster_sha256=source_cluster_sha256,
            recovery_created_at=recovery_database["created_at"],
            now=now,
            hmac_key=hmac_key,
        )
        proof["source_endpoint_sha256"] = common.sha256_value(source_endpoint)
        proof["recovery_endpoint_sha256"] = common.sha256_value(recovery_endpoint)
        return proof
    finally:
        for item in (source, recovery):
            item["username"] = ""
            item["password"] = ""


def _persistence(value: Any, label: str) -> str:
    if type(value) is not dict or type(value.get("config")) is not dict:
        common.fail(f"{label} config response is malformed")
    config = value["config"]
    allowed = config.get("redis_persistence")
    if allowed is None:
        allowed = config.get("valkey_persistence")
    if allowed != "rdb":
        common.fail(f"{label} persistence is not restorable RDB")
    return "rdb"


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
        if type(size) is int:
            size_valid = size > 0
        elif type(size) is decimal.Decimal:
            size_valid = size.is_finite() and size > 0
        else:
            size_valid = False
        if not size_valid:
            common.fail("PostgreSQL backup size is missing or invalid")
        timestamps.append((common.require_timestamp(item.get("created_at"), "PostgreSQL backup created_at"), item))
    newest, record = max(timestamps, key=lambda pair: pair[0])
    checked = now.replace(microsecond=0)
    if newest > checked or checked - newest > MAX_BACKUP_AGE:
        common.fail("PostgreSQL backup is stale or future-dated")
    return {
        "fresh": True,
        "newest_identity_sha256": _provider_sha256(record),
        "inventory_sha256": _provider_sha256(value["backups"]),
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
    sentinel_proof: Mapping[str, Any],
) -> dict[str, Any]:
    if first != second:
        common.fail("database recovery state changed between observations")
    expected_labels = [
        "postgres-cluster", "postgres-backups", "valkey-cluster", "valkey-config",
        "valkey-recovery-cluster", "valkey-recovery-config",
    ]
    expected = [("GET", label) for label in expected_labels] * 2
    if list(request_log) != expected:
        common.fail("database request ledger differs")
    contract_databases = common.exact_keys(
        dict(contract_databases),
        {
            "region", "region_sha256",
            "postgresql_cluster_sha256", "postgresql_version",
            "valkey_cluster_sha256", "valkey_version",
        },
        "production contract database bindings",
    )
    postgres_cluster_hash = common.require_sha256(
        contract_databases["postgresql_cluster_sha256"], "production PostgreSQL cluster hash"
    )
    valkey_cluster_hash = common.require_sha256(
        contract_databases["valkey_cluster_sha256"], "production Valkey cluster hash"
    )
    postgres_version = common.exact_string(
        contract_databases["postgresql_version"], "production PostgreSQL version"
    )
    valkey_version = common.exact_string(
        contract_databases["valkey_version"], "production Valkey version"
    )
    contract_region = common.exact_string(
        contract_databases["region"], "production database region"
    )
    contract_region_sha256 = common.require_sha256(
        contract_databases["region_sha256"], "production database region hash"
    )
    if contract_region_sha256 != common.sha256_bytes(contract_region.encode("utf-8")):
        common.fail("production database region authority differs")
    postgres = _database(
        first["postgres-cluster"],
        target["postgres_cluster_id"],
        {"pg", "postgres", "postgresql"},
        "PostgreSQL",
        expected_cluster_sha256=postgres_cluster_hash,
        expected_region=contract_region,
        expected_version=postgres_version,
    )
    backup = _fresh_backup(first["postgres-backups"], now)
    valkey = _database(
        first["valkey-cluster"],
        target["valkey_cluster_id"],
        {"redis", "valkey"},
        "Valkey",
        expected_cluster_sha256=valkey_cluster_hash,
        expected_region=contract_region,
        expected_version=valkey_version,
    )
    fork = _database(
        first["valkey-recovery-cluster"],
        target["valkey_recovery_cluster_id"],
        {"redis", "valkey"},
        "Valkey recovery fork",
        expected_region=contract_region,
        expected_version=valkey_version,
    )
    source_persistence = _persistence(first["valkey-config"], "Valkey")
    fork_persistence = _persistence(first["valkey-recovery-config"], "Valkey recovery fork")
    if (
        (valkey["engine"], valkey["version"], valkey["region"])
        != (fork["engine"], fork["version"], fork["region"])
        or valkey["topology_sha256"] != fork["topology_sha256"]
    ):
        common.fail("Valkey recovery fork topology differs")
    checked = now.replace(microsecond=0)
    if fork["created_at"] > checked or checked - fork["created_at"] > MAX_FORK_AGE:
        common.fail("Valkey recovery fork is stale or future-dated")
    sentinel = common.exact_keys(
        dict(sentinel_proof),
        {
            "authority", "marker_key_sha256", "marker_sha256", "source_recovery_equal",
            "live_read_count", "issued_at_sha256", "source_endpoint_sha256",
            "recovery_endpoint_sha256",
        },
        "Valkey live recovery sentinel proof",
    )
    if (
        sentinel["authority"] != SENTINEL_AUTHORITY
        or sentinel["source_recovery_equal"] is not True
        or sentinel["live_read_count"] != 2
    ):
        common.fail("Valkey live recovery sentinel proof differs")
    for key in (
        "marker_key_sha256", "marker_sha256", "issued_at_sha256",
        "source_endpoint_sha256", "recovery_endpoint_sha256",
    ):
        common.require_sha256(sentinel[key], f"Valkey sentinel {key}")
    postgres_identity_hash = common.sha256_bytes(target["postgres_cluster_id"].encode("utf-8"))
    valkey_identity_hash = common.sha256_bytes(target["valkey_cluster_id"].encode("utf-8"))
    recovery_identity_hash = common.sha256_bytes(
        target["valkey_recovery_cluster_id"].encode("utf-8")
    )
    identity_projection_hash = common.sha256_value(
        {
            "postgresql_identity_sha256": postgres_identity_hash,
            "valkey_identity_sha256": valkey_identity_hash,
            "valkey_recovery_identity_sha256": recovery_identity_hash,
        }
    )
    # Hash the already-sanitized identity projection. This remains a binding to
    # all three protected IDs while allowing downstream controllers to verify
    # the descriptor without receiving those IDs.
    descriptor_hash = identity_projection_hash
    issued_at = common.format_timestamp(checked)
    expires_at = common.format_timestamp(checked + dt.timedelta(seconds=common.MAX_RECOVERY_AGE_SECONDS))
    result = {
        "schema_version": 1,
        "authority": AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": issued_at,
        "expires_at": expires_at,
        "control": {
            **dict(control),
            "contract_sha256": common.require_sha256(contract_sha256, "recovery contract hash"),
            "controller_sha256": common.require_sha256(controller_sha256, "recovery controller hash"),
        },
        "target": {
            "descriptor_sha256": descriptor_hash,
            "contract_sha256": common.require_sha256(contract_sha256, "recovery contract hash"),
            "postgresql_identity_sha256": postgres_identity_hash,
            "valkey_identity_sha256": valkey_identity_hash,
            "valkey_recovery_identity_sha256": recovery_identity_hash,
            "identity_projection_sha256": identity_projection_hash,
            "postgresql_cluster_sha256": postgres_cluster_hash,
            "valkey_cluster_sha256": valkey_cluster_hash,
            "region_sha256": contract_region_sha256,
        },
        "postgresql": {
            "identity_sha256": common.sha256_bytes(target["postgres_cluster_id"].encode("utf-8")),
            "observation_sha256": postgres["raw_sha256"],
            "status": "online",
            "engine": "postgresql",
            "version": postgres["version"],
            "region_sha256": common.sha256_bytes(postgres["region"].encode("utf-8")),
            "fresh_backup": backup["fresh"],
            "backup_identity_sha256": backup["newest_identity_sha256"],
            "backup_inventory_sha256": backup["inventory_sha256"],
            "point_in_time_restore_ready": True,
            "production_cluster_sha256": postgres_cluster_hash,
        },
        "valkey": {
            "identity_sha256": common.sha256_bytes(target["valkey_cluster_id"].encode("utf-8")),
            "recovery_identity_sha256": common.sha256_bytes(target["valkey_recovery_cluster_id"].encode("utf-8")),
            "source_observation_sha256": valkey["raw_sha256"],
            "recovery_observation_sha256": fork["raw_sha256"],
            "status": "online",
            "recovery_status": "online",
            "version": valkey["version"],
            "recovery_version": fork["version"],
            "region_sha256": common.sha256_bytes(valkey["region"].encode("utf-8")),
            "recovery_region_sha256": common.sha256_bytes(
                fork["region"].encode("utf-8")
            ),
            "source_topology_sha256": valkey["topology_sha256"],
            "recovery_topology_sha256": fork["topology_sha256"],
            "persistence": source_persistence,
            "recovery_persistence": fork_persistence,
            "recovery_is_distinct": True,
            "recovery_is_fresh": True,
            "topology_equal": True,
            "production_cluster_sha256": valkey_cluster_hash,
            "live_recovery_sentinel": sentinel,
        },
        "provider": {
            "http_methods_used": ["GET"],
            "http_request_count": 12,
            "http_endpoint_labels": expected_labels,
            "mutation_request_count": 0,
        },
        "gates": {
            "postgresql_ready": True,
            "valkey_ready": True,
            "double_read_equal": True,
            "mutation_free": True,
        },
    }
    common.sanitize_public(result, private_values=tuple(target.values()))
    return result


def observe(
    *,
    target: Mapping[str, str],
    control: Mapping[str, Any],
    token: str,
    contract_sha256: str,
    controller_sha256: str,
    contract: Mapping[str, Any],
    source_sentinel_connection: Mapping[str, Any],
    recovery_sentinel_connection: Mapping[str, Any],
    sentinel_hmac_key_b64: str,
    now: dt.datetime | None = None,
    opener: Any | None = None,
    sentinel_reader: Any = read_valkey_sentinel,
) -> dict[str, Any]:
    for forbidden in ("DIGITALOCEAN_ACCESS_TOKEN", "DO_ACCESS_TOKEN", "DO_TOKEN", "DO_PRODUCTION_APPLY_TOKEN"):
        if os.environ.get(forbidden):
            common.fail("a forbidden ambient production credential is present")
    target = common.validate_target_descriptor(dict(target), recovery=True)
    databases = contract_database_bindings(contract)
    # Validate every protected sentinel secret before the first network read.
    for raw_connection, label in (
        (source_sentinel_connection, "Valkey source sentinel connection"),
        (recovery_sentinel_connection, "Valkey recovery sentinel connection"),
    ):
        checked_connection = _sentinel_connection(raw_connection, label)
        checked_connection["username"] = ""
        checked_connection["password"] = ""
    _sentinel_hmac_key(sentinel_hmac_key_b64)
    client = DatabaseReadClient(target, token, opener=opener)
    labels = list(client.paths)
    first = {label: client.get_label(label) for label in labels}
    second = {label: client.get_label(label) for label in labels}
    if first != second:
        common.fail("database recovery state changed between observations")
    checked = (now or dt.datetime.now(dt.timezone.utc)).replace(microsecond=0)
    valkey = _database(
        first["valkey-cluster"],
        target["valkey_cluster_id"],
        {"redis", "valkey"},
        "Valkey",
        expected_cluster_sha256=databases["valkey_cluster_sha256"],
        expected_region=databases["region"],
        expected_version=databases["valkey_version"],
    )
    fork = _database(
        first["valkey-recovery-cluster"],
        target["valkey_recovery_cluster_id"],
        {"redis", "valkey"},
        "Valkey recovery fork",
        expected_region=databases["region"],
        expected_version=databases["valkey_version"],
    )
    sentinel_proof = build_live_sentinel_proof(
        source_connection=source_sentinel_connection,
        recovery_connection=recovery_sentinel_connection,
        source_database=valkey,
        recovery_database=fork,
        source_cluster_sha256=databases["valkey_cluster_sha256"],
        hmac_key_b64=sentinel_hmac_key_b64,
        now=checked,
        reader=sentinel_reader,
    )
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
        sentinel_proof=sentinel_proof,
    )
    protected_values = tuple(target.values()) + tuple(
        item
        for connection in (source_sentinel_connection, recovery_sentinel_connection)
        for item in connection.values()
        if type(item) is str
    ) + (sentinel_hmac_key_b64,)
    common.sanitize_public(result, private_values=protected_values)
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Observe production recovery readiness")
    parser.add_argument("--contract", required=True)
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
    source_raw = os.environ.pop("DO_PRODUCTION_VALKEY_SENTINEL_SOURCE_JSON", "")
    recovery_raw = os.environ.pop("DO_PRODUCTION_VALKEY_SENTINEL_RECOVERY_JSON", "")
    sentinel_hmac_key_b64 = os.environ.pop("DO_PRODUCTION_VALKEY_SENTINEL_HMAC_KEY_B64", "")
    target = common.loads_strict(target_raw)
    source_connection = common.loads_strict(source_raw)
    recovery_connection = common.loads_strict(recovery_raw)
    del target_raw, source_raw, recovery_raw
    contract_path = Path(args.contract)
    contract = common.load_json(contract_path, "production app contract")
    contract_sha256 = common.sha256_bytes(contract_path.read_bytes())
    if contract_sha256 != common.require_sha256(args.contract_sha256, "recovery contract hash"):
        common.fail("production contract exact-file hash differs")
    control = {
        "workflow_sha": common.require_sha1(args.workflow_sha, "workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(args.workflow_run_id, "workflow run ID"),
        "run_attempt": common.exact_int(args.workflow_run_attempt, "workflow run attempt", 1, 1),
        "runner_environment": "github-hosted",
    }
    token = os.environ.pop("DO_PRODUCTION_DATABASE_READ_TOKEN", "")
    result = observe(
        target=target,
        control=control,
        token=token,
        contract_sha256=contract_sha256,
        controller_sha256=args.controller_sha256,
        contract=contract,
        source_sentinel_connection=source_connection,
        recovery_sentinel_connection=recovery_connection,
        sentinel_hmac_key_b64=sentinel_hmac_key_b64,
    )
    del token, target, source_connection, recovery_connection, sentinel_hmac_key_b64
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
