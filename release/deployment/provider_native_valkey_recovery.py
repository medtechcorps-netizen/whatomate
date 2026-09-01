#!/usr/bin/env python3
"""Create, attest, observe, and remove a provider-native Valkey recovery fork.

The controller deliberately never connects to Valkey and never requests database
credentials.  A fork is identified by a deterministic operation-scoped name,
but public evidence contains only hashes of provider identifiers and names.

Mutation rules are intentionally small and fail closed:

* one POST may create the fork; an ambiguous result is reconciled with GETs;
* one DELETE may remove the receipt-bound fork; ambiguity is reconciled with GETs;
* neither mutation is automatically repeated;
* readiness consists only of provider GETs and two identical complete reads.
"""

from __future__ import annotations

import argparse
import datetime as dt
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Mapping, NoReturn, Protocol, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_release as common


PREPARE_WORKFLOW_PATH = (
    ".github/workflows/prepare-production-valkey-recovery-fork.yml"
)
READINESS_WORKFLOW_PATH = ".github/workflows/verify-production-recovery-readiness.yml"
CLEANUP_WORKFLOW_PATH = (
    ".github/workflows/cleanup-production-valkey-recovery-fork.yml"
)
CREATE_AUTHORITY = "production-valkey-recovery-fork-create-receipt-v2"
INTENT_AUTHORITY = "production-valkey-recovery-fork-create-intent-v2"
READINESS_AUTHORITY = "production-valkey-recovery-fork-v2"
DELETE_AUTHORITY = "production-valkey-recovery-fork-delete-receipt-v2"
PROVIDER_COPY_CONTRACT = (
    "digitalocean-valkey-latest-transaction-data-and-configuration"
)
MAX_FORK_TTL_SECONDS = 24 * 60 * 60
MAX_INTENT_AGE_SECONDS = 15 * 60
MAX_PROVIDER_POLLS = 40
PROVIDER_POLL_SECONDS = 15
LIST_PAGE_SIZE = 200
LIST_PATH = f"/v2/databases?page=1&per_page={LIST_PAGE_SIZE}"
DATABASES_PATH = "/v2/databases"
APP_TO_DATABASE_REGION = {"sgp": "sgp1"}
CLUSTER_NAME_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$")
_MISSING = object()
SIZE_RE = re.compile(r"^db-[a-z0-9-]{3,120}$")

CREATE_CONTROL_KEYS = {
    "workflow_sha",
    "workflow_path",
    "run_id",
    "run_attempt",
    "runner_environment",
    "rollout_plan_sha256",
    "contract_sha256",
    "controller_sha256",
}
CLEANUP_CONTROL_KEYS = CREATE_CONTROL_KEYS | {
    "authority_workflow_sha",
    "authority_controller_sha256",
}
CREATE_TARGET_KEYS = {
    "descriptor_sha256",
    "source_identity_sha256",
    "source_name_sha256",
    "source_observation_sha256",
    "source_topology_sha256",
    "source_config_sha256",
    "source_firewall_sha256",
}
CREATE_REQUEST_KEYS = {
    "method",
    "endpoint_label",
    "request_sha256",
    "request_attempt_count",
    "provider_copy_contract",
}
CREATE_RESULT_KEYS = {
    "outcome",
    "recovery_identity_sha256",
    "fork_name_sha256",
    "fork_created_at_sha256",
    "recovery_observation_sha256",
    "recovery_topology_sha256",
    "recovery_config_sha256",
    "recovery_firewall_sha256",
    "mutation_ambiguous_reconciled",
}
PROVIDER_KEYS = {
    "http_methods_used",
    "http_request_count",
    "endpoint_labels",
    "mutation_request_count",
}
CREATE_GATE_KEYS = {
    "source_ready",
    "fork_ready",
    "source_stable",
    "source_firewall_exact_app",
    "recovery_firewall_exact_source_app",
    "recovery_restricted_to_exact_production_app",
    "exact_single_mutation",
}
PROVIDER_FORK_KEYS = {
    "authority",
    "source_identity_sha256",
    "recovery_identity_sha256",
    "request_sha256",
    "receipt_sha256",
    "source_config_sha256",
    "recovery_config_sha256",
    "source_firewall_sha256",
    "recovery_firewall_sha256",
    "fork_name_sha256",
    "fork_created_at_sha256",
    "provider_copy_contract",
    "stable_read_count",
    "request_attempt_count",
    "mutation_ambiguous_reconciled",
    "source_firewall_unchanged",
    "source_firewall_exact_app",
    "recovery_firewall_exact_source_app",
    "recovery_restricted_to_exact_production_app",
}
DELETE_TARGET_KEYS = {
    "descriptor_sha256",
    "source_identity_sha256",
    "source_name_sha256",
    "source_observation_sha256",
    "source_topology_sha256",
    "source_config_sha256",
    "source_firewall_sha256",
    "recovery_identity_sha256",
    "fork_name_sha256",
    "create_authority",
    "create_authority_sha256",
    "cleanup_mode",
    "cleanup_authority_sha256",
}
DELETE_RESULT_KEYS = {
    "outcome",
    "deletion_request_attempt_count",
    "mutation_ambiguous_reconciled",
    "stable_absence_read_count",
    "source_stable_read_count",
    "fork_absent",
}
DELETE_GATE_KEYS = {
    "authority_bound",
    "exact_single_or_zero_mutation",
    "source_ready",
    "source_stable",
    "source_firewall_exact_app",
    "fork_absent",
}
INTENT_REQUEST_KEYS = {
    "method",
    "endpoint_label",
    "request_sha256",
    "provider_copy_contract",
    "spec",
}
INTENT_SPEC_KEYS = {
    "engine",
    "version",
    "region_sha256",
    "size",
    "num_nodes",
    "storage_size_mib",
    "source_name_sha256",
    "private_network_uuid_sha256",
    "firewall_policy_sha256",
    "fork_name_sha256",
    "backup_created_at_omitted",
}
CLEANUP_MODES = {
    "terminal",
    "never-started",
    "pre-mutation-failure",
    "no-mutation",
    "quarantine",
}


class MutationAmbiguous(common.AmbiguousMutation):
    """The one permitted mutation may have reached DigitalOcean."""


class MutationRejected(common.ReleaseError):
    """DigitalOcean definitively rejected a mutation before committing it."""


@dataclass(frozen=True)
class APIResult:
    status: int
    value: Any | None


class Transport(Protocol):
    def request(self, method: str, path: str, body: bytes | None = None) -> APIResult:
        """Perform exactly one HTTP request with no retry."""


def _fail(message: str) -> NoReturn:
    common.fail(message)


def _checked_clock(now: dt.datetime, label: str) -> dt.datetime:
    if (
        not isinstance(now, dt.datetime)
        or now.tzinfo is None
        or now.utcoffset() is None
        or now.microsecond
    ):
        _fail(f"{label} clock is not second-precision UTC")
    return now.astimezone(dt.timezone.utc)


def _validate_provider_path(method: str, path: str) -> None:
    if method == "POST" and path == DATABASES_PATH:
        return
    if method == "GET" and path == LIST_PATH:
        return
    if re.fullmatch(
        r"/v2/databases/[0-9a-f-]{36}(?:/(?:config|firewall))?", path
    ) is not None:
        if method in {"GET", "DELETE"} and not (
            method == "DELETE" and path.endswith(("/config", "/firewall"))
        ):
            return
    _fail("provider request is outside the exact database allowlist")


class DigitalOceanTransport:
    """Minimal HTTPS transport with redirects, proxies, and retries disabled."""

    def __init__(self, token: str, *, opener: Any | None = None) -> None:
        if (
            type(token) is not str
            or len(token) < 20
            or any(ch in token for ch in "\r\n\x00")
        ):
            _fail("database token is invalid")
        self._token = token
        self._opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), common.RejectRedirects()
        )

    def request(self, method: str, path: str, body: bytes | None = None) -> APIResult:
        if method not in {"GET", "POST", "DELETE"}:
            _fail("provider HTTP method differs")
        _validate_provider_path(method, path)
        if (method == "POST") != (body is not None):
            _fail("provider request body differs")
        if body is not None:
            if not body or len(body) > common.MAX_JSON_BYTES:
                _fail("provider request body size differs")
            value = common.loads_strict(body)
            if body != common.canonical_payload_bytes(value):
                _fail("provider mutation body is not canonical")
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
            _fail("provider URL differs")
        headers = {
            "Accept": "application/json",
            "Authorization": f"Bearer {self._token}",
            "User-Agent": "rereply-provider-native-valkey-recovery/2",
        }
        if body is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            url, data=body, method=method, headers=headers
        )
        try:
            with self._opener.open(request, timeout=20) as response:
                if response.geturl() != url:
                    _fail("provider response URL differs")
                status = getattr(response, "status", None)
                if status is None:
                    status = response.getcode()
                raw = response.read(common.MAX_JSON_BYTES + 1)
                return self._decode_response(method, int(status), response.headers, raw)
        except MutationRejected:
            raise
        except common.ReleaseError as exc:
            if method in {"POST", "DELETE"}:
                raise MutationAmbiguous(
                    "provider mutation response could not prove its outcome"
                ) from exc
            raise
        except urllib.error.HTTPError as exc:
            status = int(exc.code)
            if method in {"POST", "DELETE"} and (
                status < 400 or status in {408, 429} or status >= 500
            ):
                raise MutationAmbiguous("provider mutation result is ambiguous") from exc
            if status == 404 and method in {"GET", "DELETE"}:
                return APIResult(status=404, value=None)
            if method in {"POST", "DELETE"} and 400 <= status < 500:
                raise MutationRejected("provider definitively rejected mutation") from exc
            raise common.ReleaseError("provider request failed closed") from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            if method in {"POST", "DELETE"}:
                raise MutationAmbiguous("provider mutation result is ambiguous") from exc
            raise common.ReleaseError("provider observation failed") from exc
        except Exception as exc:
            if method in {"POST", "DELETE"}:
                raise MutationAmbiguous(
                    "provider mutation response could not prove its outcome"
                ) from exc
            raise common.ReleaseError("provider observation failed") from exc

    @staticmethod
    def _decode_response(
        method: str, status: int, headers: Mapping[str, Any], raw: bytes
    ) -> APIResult:
        expected = {"GET": {200}, "POST": {201}, "DELETE": {204}}[method]
        if status not in expected:
            if method in {"POST", "DELETE"} and (
                status in {408, 429} or status >= 500
            ):
                raise MutationAmbiguous("provider mutation result is ambiguous")
            if status == 404 and method in {"GET", "DELETE"}:
                return APIResult(status=404, value=None)
            if method in {"POST", "DELETE"} and 400 <= status < 500:
                raise MutationRejected("provider definitively rejected mutation")
            _fail("provider response status differs")
        if status == 204:
            if raw:
                _fail("provider empty response contains a body")
            return APIResult(status=status, value=None)
        if not raw or len(raw) > common.MAX_JSON_BYTES:
            _fail("provider response size differs")
        content_type = str(headers.get("Content-Type", ""))
        if content_type.split(";", 1)[0].strip().lower() != "application/json":
            _fail("provider response content type differs")
        return APIResult(status=status, value=common.loads_strict(raw))

    def scrub(self) -> None:
        self._token = ""


class ProviderSession:
    """Attach semantic labels to an injected no-retry transport."""

    def __init__(self, transport: Transport) -> None:
        self.transport = transport
        self.ledger: list[tuple[str, str]] = []

    def call(
        self, label: str, method: str, path: str, body: bytes | None = None
    ) -> APIResult:
        self.ledger.append((method, label))
        return self.transport.request(method, path, body)


class _MutationTrackingTransport:
    """Record whether the sole mutation call may have crossed the wire."""

    def __init__(self, transport: Transport) -> None:
        self.transport = transport
        self.mutation_attempted = False

    def request(
        self, method: str, path: str, body: bytes | None = None
    ) -> APIResult:
        if method in {"POST", "DELETE"}:
            if self.mutation_attempted:
                _fail("provider mutation was attempted more than once")
            self.mutation_attempted = True
        return self.transport.request(method, path, body)


def validate_target_descriptor(value: Any) -> dict[str, str]:
    descriptor = common.exact_keys(
        value,
        {"postgres_cluster_id", "valkey_cluster_id"},
        "protected recovery target descriptor",
    )
    postgres = common.require_uuid(
        descriptor["postgres_cluster_id"], "target PostgreSQL identity"
    )
    valkey = common.require_uuid(
        descriptor["valkey_cluster_id"], "target Valkey identity"
    )
    if postgres == valkey:
        _fail("production database identities are not distinct")
    return {"postgres_cluster_id": postgres, "valkey_cluster_id": valkey}


def target_descriptor_sha256(target: Mapping[str, Any]) -> str:
    checked = validate_target_descriptor(dict(target))
    return common.sha256_value(
        {
            "postgresql_identity_sha256": common.sha256_bytes(
                checked["postgres_cluster_id"].encode("utf-8")
            ),
            "valkey_identity_sha256": common.sha256_bytes(
                checked["valkey_cluster_id"].encode("utf-8")
            ),
        }
    )


def validate_control(value: Any, *, workflow_path: str) -> dict[str, Any]:
    expected_keys = (
        CLEANUP_CONTROL_KEYS
        if workflow_path == CLEANUP_WORKFLOW_PATH
        else CREATE_CONTROL_KEYS
    )
    control = common.exact_keys(value, expected_keys, "fork control")
    checked = {
        "workflow_sha": common.require_sha1(control["workflow_sha"], "workflow SHA"),
        "workflow_path": common.exact_string(
            control["workflow_path"], "workflow path"
        ),
        "run_id": common.require_run_id(control["run_id"], "workflow run ID"),
        "run_attempt": common.exact_int(
            control["run_attempt"], "workflow run attempt", 1, 1
        ),
        "runner_environment": common.exact_string(
            control["runner_environment"], "runner environment"
        ),
        "rollout_plan_sha256": common.require_sha256(
            control["rollout_plan_sha256"], "rollout plan hash"
        ),
        "contract_sha256": common.require_sha256(
            control["contract_sha256"], "production contract hash"
        ),
        "controller_sha256": common.require_sha256(
            control["controller_sha256"], "fork controller hash"
        ),
    }
    if workflow_path == CLEANUP_WORKFLOW_PATH:
        checked["authority_workflow_sha"] = common.require_sha1(
            control["authority_workflow_sha"], "fork authority workflow SHA"
        )
        checked["authority_controller_sha256"] = common.require_sha256(
            control["authority_controller_sha256"],
            "fork authority controller hash",
        )
    if (
        checked["workflow_path"] != workflow_path
        or checked["runner_environment"] != "github-hosted"
    ):
        _fail("fork workflow identity differs")
    return checked


def _require_same_release_binding(
    current: Mapping[str, Any], prepare: Mapping[str, Any]
) -> None:
    for key in (
        "workflow_sha",
        "runner_environment",
        "rollout_plan_sha256",
        "contract_sha256",
        "controller_sha256",
    ):
        if current[key] != prepare[key]:
            _fail("fork workflow release binding differs")


def _require_cleanup_release_binding(
    current: Mapping[str, Any], prepare: Mapping[str, Any]
) -> None:
    if (
        current["authority_workflow_sha"] != prepare["workflow_sha"]
        or current["authority_controller_sha256"]
        != prepare["controller_sha256"]
    ):
        _fail("fork cleanup authority control binding differs")
    for key in (
        "runner_environment",
        "rollout_plan_sha256",
        "contract_sha256",
    ):
        if current[key] != prepare[key]:
            _fail("fork cleanup release binding differs")


def contract_binding(
    contract: Any, exact_sha256: str, observed_file_sha256: str
) -> dict[str, str]:
    expected_sha = common.require_sha256(exact_sha256, "production contract hash")
    if (
        common.require_sha256(
            observed_file_sha256, "observed production contract hash"
        )
        != expected_sha
    ):
        _fail("production contract exact hash differs")
    if (
        type(contract) is not dict
        or type(contract.get("expected_topology")) is not dict
        or type(contract.get("provider")) is not dict
    ):
        _fail("production contract topology is malformed")
    topology = contract["expected_topology"]
    app_region = common.exact_string(topology.get("region"), "production app region")
    region = APP_TO_DATABASE_REGION.get(app_region)
    if region is None:
        _fail("production database region mapping is unavailable")
    vpc_hash = common.require_sha256(
        topology.get("vpc_id_sha256"), "production VPC hash"
    )
    databases = topology.get("databases")
    if type(databases) is not list:
        _fail("production database inventory is malformed")
    valkey: dict[str, Any] | None = None
    for item in databases:
        item = common.exact_keys(
            item,
            {"engine", "version", "production", "name_sha256", "cluster_sha256"},
            "production database binding",
        )
        if item["engine"] == "VALKEY":
            if valkey is not None:
                _fail("production Valkey binding is duplicated")
            valkey = item
    if valkey is None or valkey["production"] is not True:
        _fail("production Valkey binding is missing")
    version = common.exact_string(valkey["version"], "production Valkey version")
    if version != "8":
        _fail("production Valkey version differs")
    return {
        "contract_sha256": expected_sha,
        "app_id_sha256": common.require_sha256(
            contract["provider"].get("app_id_sha256"),
            "production app identity hash",
        ),
        "source_name_sha256": common.require_sha256(
            valkey["cluster_sha256"], "production Valkey cluster-name hash"
        ),
        "region": region,
        "region_sha256": common.sha256_bytes(region.encode("utf-8")),
        "version": version,
        "private_network_uuid_sha256": vpc_hash,
    }


def deterministic_fork_name(phase: str, control: Mapping[str, Any]) -> str:
    phase = common.validate_phase(phase, "recovery phase")
    checked = validate_control(
        dict(control), workflow_path=PREPARE_WORKFLOW_PATH
    )
    value = (
        f"rereply-recovery-{phase}-{checked['workflow_sha'][:8]}-"
        f"{checked['run_id']}-{checked['run_attempt']}"
    )
    if len(value) > 63 or CLUSTER_NAME_RE.fullmatch(value) is None:
        _fail("deterministic recovery fork name is invalid")
    return value


def _database_record(value: Any, label: str) -> dict[str, Any]:
    if type(value) is not dict:
        _fail(f"{label} is malformed")
    identity = common.require_uuid(value.get("id"), f"{label} identity")
    name = common.exact_string(value.get("name"), f"{label} name", CLUSTER_NAME_RE)
    engine = common.exact_string(value.get("engine"), f"{label} engine").lower()
    version = common.exact_string(value.get("version"), f"{label} version")
    region = common.exact_string(value.get("region"), f"{label} region")
    size = common.exact_string(value.get("size"), f"{label} size", SIZE_RE)
    nodes = common.exact_int(value.get("num_nodes"), f"{label} node count", 1, 10)
    status = common.exact_string(value.get("status"), f"{label} status").lower()
    created_raw = common.exact_string(value.get("created_at"), f"{label} created_at")
    common.require_timestamp(created_raw, f"{label} created_at")
    private_network_uuid = common.require_uuid(
        value.get("private_network_uuid"), f"{label} private network identity"
    )
    storage = value.get("storage_size_mib")
    if storage is not None:
        storage = common.exact_int(storage, f"{label} storage", 1, 1_000_000_000)
    safe = {
        "identity_sha256": common.sha256_bytes(identity.encode("utf-8")),
        "name_sha256": common.sha256_bytes(name.encode("utf-8")),
        "status": status,
        "engine": engine,
        "version": version,
        "region_sha256": common.sha256_bytes(region.encode("utf-8")),
        "size": size,
        "num_nodes": nodes,
        "private_network_uuid_sha256": common.sha256_bytes(
            private_network_uuid.encode("utf-8")
        ),
        "created_at_sha256": common.sha256_bytes(created_raw.encode("utf-8")),
        "storage_size_mib": storage,
    }
    topology = {
        key: safe[key]
        for key in (
            "engine",
            "version",
            "region_sha256",
            "size",
            "num_nodes",
            "private_network_uuid_sha256",
            "storage_size_mib",
        )
    }
    return {
        "id": identity,
        "name": name,
        "engine": engine,
        "version": version,
        "region": region,
        "size": size,
        "num_nodes": nodes,
        "status": status,
        "created_at": created_raw,
        "private_network_uuid": private_network_uuid,
        "storage_size_mib": storage,
        "safe": safe,
        "observation_sha256": common.sha256_value(safe),
        "topology_sha256": common.sha256_value(topology),
    }


def _database_envelope(value: Any, label: str) -> dict[str, Any]:
    if type(value) is not dict or type(value.get("database")) is not dict:
        _fail(f"{label} response is malformed")
    return _database_record(value["database"], label)


def _validate_source(
    database: Mapping[str, Any], target: Mapping[str, str], binding: Mapping[str, str]
) -> None:
    if (
        database["id"] != target["valkey_cluster_id"]
        or database["engine"] != "valkey"
        or database["version"] != binding["version"]
        or database["region"] != binding["region"]
        or database["status"] != "online"
        or database["safe"]["name_sha256"] != binding["source_name_sha256"]
        or database["safe"]["private_network_uuid_sha256"]
        != binding["private_network_uuid_sha256"]
    ):
        _fail("production Valkey source differs from the exact contract")


def _validate_fork(
    database: Mapping[str, Any], source: Mapping[str, Any], fork_name: str, *, online: bool
) -> None:
    allowed_status = {"online"} if online else {"creating", "forking", "online"}
    if (
        database["id"] == source["id"]
        or database["name"] != fork_name
        or database["engine"] != "valkey"
        or database["status"] not in allowed_status
        or database["topology_sha256"] != source["topology_sha256"]
    ):
        _fail("Valkey recovery fork differs from the exact source topology")


def _config_projection(value: Any, label: str) -> tuple[dict[str, Any], str]:
    if type(value) is not dict or type(value.get("config")) is not dict:
        _fail(f"{label} configuration is malformed")
    config = value["config"]
    persistence = config.get("valkey_persistence")
    if persistence is None:
        persistence = config.get("redis_persistence")
    if persistence != "rdb":
        _fail(f"{label} persistence is not forkable RDB")
    return config, common.sha256_value(config)


def _firewall_projection(value: Any, label: str) -> dict[str, Any]:
    if type(value) is not dict or type(value.get("rules")) is not list:
        _fail(f"{label} firewall is malformed")
    rules = value["rules"]
    normalized: list[dict[str, Any]] = []
    semantic: list[dict[str, Any]] = []
    policy: list[dict[str, str]] = []
    raw_rules: list[dict[str, str]] = []
    allowed = {"cluster_uuid", "created_at", "description", "type", "uuid", "value"}
    for rule in rules:
        if type(rule) is not dict:
            _fail(f"{label} firewall rule is malformed")
        if not set(rule).issubset(allowed) or not {"type", "value"}.issubset(rule):
            _fail(f"{label} firewall rule keys differ")
        kind = common.exact_string(rule["type"], f"{label} firewall rule type")
        if kind not in {"droplet", "k8s", "ip_addr", "tag", "app"}:
            _fail(f"{label} firewall rule type differs")
        raw_value = common.exact_string(rule["value"], f"{label} firewall rule value")
        projection: dict[str, Any] = {
            "type": kind,
            "value_sha256": common.sha256_bytes(raw_value.encode("utf-8")),
            "cluster_identity_sha256": None,
            "rule_identity_sha256": None,
            "created_at_sha256": None,
            "description_sha256": None,
        }
        policy.append(
            {"type": kind, "value_sha256": projection["value_sha256"]}
        )
        raw_rules.append({"type": kind, "value": raw_value})
        if rule.get("cluster_uuid") is not None:
            projection["cluster_identity_sha256"] = common.sha256_bytes(
                common.require_uuid(
                    rule["cluster_uuid"], f"{label} firewall cluster identity"
                ).encode("utf-8")
            )
        if rule.get("uuid") is not None:
            projection["rule_identity_sha256"] = common.sha256_bytes(
                common.require_uuid(
                    rule["uuid"], f"{label} firewall rule identity"
                ).encode("utf-8")
            )
        if rule.get("created_at") is not None:
            created = common.exact_string(
                rule["created_at"], f"{label} firewall rule created_at"
            )
            common.require_timestamp(created, f"{label} firewall rule created_at")
            projection["created_at_sha256"] = common.sha256_bytes(
                created.encode("utf-8")
            )
        if rule.get("description") is not None:
            description = common.exact_string(
                rule["description"], f"{label} firewall rule description"
            )
            projection["description_sha256"] = common.sha256_bytes(
                description.encode("utf-8")
            )
        semantic.append(
            {
                "type": projection["type"],
                "value_sha256": projection["value_sha256"],
                "description_sha256": projection["description_sha256"],
            }
        )
        normalized.append(projection)
    normalized.sort(key=common.canonical_payload_bytes)
    semantic.sort(key=common.canonical_payload_bytes)
    policy.sort(key=common.canonical_payload_bytes)
    raw_rules.sort(key=common.canonical_payload_bytes)
    return {
        "rules": normalized,
        "sha256": common.sha256_value(normalized),
        "semantic": semantic,
        "semantic_sha256": common.sha256_value(semantic),
        "policy": policy,
        "policy_sha256": common.sha256_value(policy),
        "raw_rules": raw_rules,
    }


def _require_exact_app_firewall(
    firewall: Mapping[str, Any], expected_app_id_sha256: str, label: str
) -> str:
    expected_hash = common.require_sha256(
        expected_app_id_sha256, f"{label} application identity hash"
    )
    expected_policy = [{"type": "app", "value_sha256": expected_hash}]
    if firewall.get("policy") != expected_policy:
        _fail(f"{label} firewall is not exactly the production application rule")
    raw_rules = firewall.get("raw_rules")
    if (
        type(raw_rules) is not list
        or len(raw_rules) != 1
        or type(raw_rules[0]) is not dict
        or set(raw_rules[0]) != {"type", "value"}
        or raw_rules[0]["type"] != "app"
        or common.sha256_bytes(raw_rules[0]["value"].encode("utf-8"))
        != expected_hash
    ):
        _fail(f"{label} firewall application rule differs")
    return raw_rules[0]["value"]


def _complete_database_collection(value: Any) -> list[Any]:
    if type(value) is not dict:
        _fail("database inventory response differs")
    databases = value.get("databases", _MISSING)
    if type(databases) is not list or len(databases) > LIST_PAGE_SIZE:
        _fail("database inventory is malformed or incomplete")

    links = value.get("links", _MISSING)
    if links is not _MISSING:
        if type(links) is not dict or set(links) - {"pages"}:
            _fail("database inventory pagination metadata is malformed")
        pages = links.get("pages", _MISSING)
        if pages is not _MISSING and (type(pages) is not dict or pages):
            _fail("database inventory has unconsumed pagination metadata")

    meta = value.get("meta", _MISSING)
    if meta is _MISSING:
        # DigitalOcean may omit the optional collection metadata when the
        # complete result fits on the requested page.  A full page without a
        # total remains ambiguous and therefore fails closed.
        if len(databases) >= LIST_PAGE_SIZE:
            _fail("database inventory completeness metadata is missing")
    else:
        if type(meta) is not dict or type(meta.get("total", _MISSING)) is not int:
            _fail("database inventory completeness metadata is malformed")
        total = common.exact_int(
            meta["total"], "database inventory total", 0, LIST_PAGE_SIZE
        )
        if total != len(databases):
            _fail("database inventory is paginated or incomplete")
    return databases


def _list_inventory(
    session: ProviderSession, *, label: str = "valkey-recovery-discovery"
) -> list[dict[str, Any]]:
    result = session.call(label, "GET", LIST_PATH)
    if result.status != 200:
        _fail("database inventory response differs")
    databases = _complete_database_collection(result.value)
    identities: set[str] = set()
    names: set[str] = set()
    inventory: list[dict[str, Any]] = []
    for item in databases:
        database = _database_record(item, "database inventory item")
        if database["id"] in identities or database["name"] in names:
            _fail("database inventory contains duplicate identities or names")
        identities.add(database["id"])
        names.add(database["name"])
        inventory.append(database)
    return inventory


def _list_named(
    session: ProviderSession, name: str, *, label: str = "valkey-recovery-discovery"
) -> list[dict[str, Any]]:
    matches = [
        database
        for database in _list_inventory(session, label=label)
        if database["name"] == name
    ]
    if len(matches) > 1:
        _fail("deterministic recovery fork name is not unique")
    return matches


def _get_database(
    session: ProviderSession, identity: str, label: str
) -> dict[str, Any] | None:
    identity = common.require_uuid(identity, f"{label} identity")
    result = session.call(label, "GET", f"/v2/databases/{identity}")
    if result.status == 404:
        return None
    if result.status != 200:
        _fail(f"{label} response status differs")
    database = _database_envelope(result.value, label)
    if database["id"] != identity:
        _fail(f"{label} response identity differs from its request path")
    return database


def _get_config(session: ProviderSession, identity: str, label: str) -> tuple[dict[str, Any], str]:
    result = session.call(label, "GET", f"/v2/databases/{identity}/config")
    if result.status != 200:
        _fail(f"{label} response status differs")
    return _config_projection(result.value, label)


def _get_firewall(
    session: ProviderSession, identity: str, label: str
) -> dict[str, Any]:
    result = session.call(label, "GET", f"/v2/databases/{identity}/firewall")
    if result.status != 200:
        _fail(f"{label} response status differs")
    return _firewall_projection(result.value, label)


def _source_bundle(
    session: ProviderSession,
    target: Mapping[str, str],
    binding: Mapping[str, str],
    *,
    suffix: str = "",
) -> dict[str, Any]:
    identity = target["valkey_cluster_id"]
    database = _get_database(session, identity, f"valkey-cluster{suffix}")
    if database is None:
        _fail("production Valkey source is missing")
    _validate_source(database, target, binding)
    config, config_hash = _get_config(
        session, identity, f"valkey-config{suffix}"
    )
    firewall = _get_firewall(
        session, identity, f"valkey-source-firewall{suffix}"
    )
    firewall_app_id = _require_exact_app_firewall(
        firewall, binding["app_id_sha256"], "production Valkey source"
    )
    return {
        "database": database,
        "config": config,
        "config_sha256": config_hash,
        "firewall": firewall["rules"],
        "firewall_sha256": firewall["sha256"],
        "firewall_semantic": firewall["semantic"],
        "firewall_semantic_sha256": firewall["semantic_sha256"],
        "firewall_policy": firewall["policy"],
        "firewall_policy_sha256": firewall["policy_sha256"],
        "firewall_app_id": firewall_app_id,
    }


def _fork_bundle(
    session: ProviderSession,
    identity: str,
    source: Mapping[str, Any],
    fork_name: str,
    *,
    suffix: str = "",
) -> dict[str, Any]:
    database = _get_database(session, identity, f"valkey-recovery-cluster{suffix}")
    if database is None:
        _fail("Valkey recovery fork is missing")
    _validate_fork(database, source["database"], fork_name, online=True)
    config, config_hash = _get_config(
        session, identity, f"valkey-recovery-config{suffix}"
    )
    firewall = _get_firewall(
        session, identity, f"valkey-recovery-firewall{suffix}"
    )
    _require_exact_app_firewall(
        firewall,
        source["firewall_policy"][0]["value_sha256"],
        "Valkey recovery fork",
    )
    if firewall["policy"] != source["firewall_policy"]:
        _fail("Valkey recovery fork firewall differs from source application policy")
    return {
        "database": database,
        "config": config,
        "config_sha256": config_hash,
        "firewall": firewall["rules"],
        "firewall_sha256": firewall["sha256"],
        "firewall_semantic": firewall["semantic"],
        "firewall_semantic_sha256": firewall["semantic_sha256"],
        "firewall_policy": firewall["policy"],
        "firewall_policy_sha256": firewall["policy_sha256"],
    }


def _source_authority_projection(
    target: Mapping[str, Any], source: Mapping[str, Any]
) -> dict[str, str]:
    database = source["database"]
    return {
        "descriptor_sha256": target_descriptor_sha256(target),
        "source_identity_sha256": database["safe"]["identity_sha256"],
        "source_name_sha256": database["safe"]["name_sha256"],
        "source_observation_sha256": database["observation_sha256"],
        "source_topology_sha256": database["topology_sha256"],
        "source_config_sha256": source["config_sha256"],
        "source_firewall_sha256": source["firewall_sha256"],
    }


def _create_request(source: Mapping[str, Any], fork_name: str) -> dict[str, Any]:
    database = source["database"]
    request: dict[str, Any] = {
        "name": fork_name,
        "engine": "valkey",
        "version": database["version"],
        "region": database["region"],
        "size": database["size"],
        "num_nodes": database["num_nodes"],
        "private_network_uuid": database["private_network_uuid"],
        "backup_restore": {"database_name": database["name"]},
        "rules": [{"type": "app", "value": source["firewall_app_id"]}],
    }
    if database["storage_size_mib"] is not None:
        request["storage_size_mib"] = database["storage_size_mib"]
    if "backup_created_at" in request["backup_restore"]:
        _fail("Valkey fork request must use latest provider state")
    return request


def _intent_request_spec(
    source: Mapping[str, Any], fork_name: str
) -> dict[str, Any]:
    database = source["database"]
    return {
        "engine": "valkey",
        "version": database["version"],
        "region_sha256": common.sha256_bytes(database["region"].encode("utf-8")),
        "size": database["size"],
        "num_nodes": database["num_nodes"],
        "storage_size_mib": database["storage_size_mib"],
        "source_name_sha256": database["safe"]["name_sha256"],
        "private_network_uuid_sha256": database["safe"][
            "private_network_uuid_sha256"
        ],
        "firewall_policy_sha256": source["firewall_policy_sha256"],
        "fork_name_sha256": common.sha256_bytes(fork_name.encode("utf-8")),
        "backup_created_at_omitted": True,
    }


def build_create_intent(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    transport: Transport,
    now: dt.datetime,
) -> dict[str, Any]:
    """Build the immutable hash-only authority that must exist before POST."""

    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=PREPARE_WORKFLOW_PATH
    )
    phase = common.validate_phase(phase, "recovery phase")
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    fork_name = deterministic_fork_name(phase, checked_control)
    session = ProviderSession(transport)
    source = _source_bundle(session, checked_target, binding)
    if _list_named(session, fork_name):
        _fail("deterministic recovery fork already exists before intent")
    request_value = _create_request(source, fork_name)
    request_hash = common.sha256_bytes(common.canonical_payload_bytes(request_value))
    issued = _checked_clock(now, "Valkey fork create intent")
    operation_projection = {
        "phase": phase,
        "workflow_sha": checked_control["workflow_sha"],
        "run_id": checked_control["run_id"],
        "run_attempt": checked_control["run_attempt"],
        "fork_name_sha256": common.sha256_bytes(fork_name.encode("utf-8")),
        "request_sha256": request_hash,
    }
    intent = {
        "schema_version": 2,
        "authority": INTENT_AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": common.format_timestamp(issued),
        "expires_at": common.format_timestamp(
            issued + dt.timedelta(seconds=MAX_INTENT_AGE_SECONDS)
        ),
        "phase": phase,
        "operation_id_sha256": common.sha256_value(operation_projection),
        "control": checked_control,
        "target": {
            "descriptor_sha256": target_descriptor_sha256(checked_target),
            "source_identity_sha256": source["database"]["safe"][
                "identity_sha256"
            ],
            "source_name_sha256": source["database"]["safe"]["name_sha256"],
            "source_observation_sha256": source["database"][
                "observation_sha256"
            ],
            "source_topology_sha256": source["database"]["topology_sha256"],
            "source_config_sha256": source["config_sha256"],
            "source_firewall_sha256": source["firewall_sha256"],
        },
        "request": {
            "method": "POST",
            "endpoint_label": "database-clusters",
            "request_sha256": request_hash,
            "provider_copy_contract": PROVIDER_COPY_CONTRACT,
            "spec": _intent_request_spec(source, fork_name),
        },
        "provider": _provider_ledger(session, 0),
        "gates": {
            "source_ready": True,
            "source_firewall_exact_app": True,
            "fork_absent": True,
            "mutation_free": True,
        },
    }
    common.sanitize_public(
        intent,
        private_values=(
            checked_target["postgres_cluster_id"],
            checked_target["valkey_cluster_id"],
            source["database"]["name"],
            source["database"]["private_network_uuid"],
            source["firewall_app_id"],
            fork_name,
        ),
        allowed_keys=("spec",),
    )
    return intent


def validate_create_intent(
    value: Any,
    *,
    exact_sha256: str,
    target: Mapping[str, Any],
    control: Mapping[str, Any] | None,
    phase: str,
    now: dt.datetime,
    allow_expired: bool = False,
) -> dict[str, Any]:
    intent = common.exact_keys(
        value,
        {
            "schema_version",
            "authority",
            "repository",
            "issued_at",
            "expires_at",
            "phase",
            "operation_id_sha256",
            "control",
            "target",
            "request",
            "provider",
            "gates",
        },
        "Valkey fork create intent",
    )
    phase = common.validate_phase(phase, "recovery phase")
    if (
        intent["schema_version"] != 2
        or intent["authority"] != INTENT_AUTHORITY
        or intent["repository"] != common.REPOSITORY
        or intent["phase"] != phase
    ):
        _fail("Valkey fork create intent authority differs")
    checked_target = validate_target_descriptor(dict(target))
    embedded_control = validate_control(
        intent["control"], workflow_path=PREPARE_WORKFLOW_PATH
    )
    if control is not None and embedded_control != validate_control(
        dict(control), workflow_path=PREPARE_WORKFLOW_PATH
    ):
        _fail("Valkey fork create intent control differs")
    if allow_expired:
        issued = common.require_timestamp(
            intent["issued_at"], "Valkey fork create intent issued_at"
        )
        expires = common.require_timestamp(
            intent["expires_at"], "Valkey fork create intent expires_at"
        )
        if (
            expires <= issued
            or (expires - issued).total_seconds() > MAX_INTENT_AGE_SECONDS
            or _checked_clock(now, "Valkey fork create intent") < issued
        ):
            _fail("Valkey fork create intent validity window differs")
    else:
        common.validate_fresh_window(
            intent["issued_at"],
            intent["expires_at"],
            now,
            maximum_age_seconds=MAX_INTENT_AGE_SECONDS,
            label="Valkey fork create intent",
        )
    operation_hash = common.require_sha256(
        intent["operation_id_sha256"], "fork operation ID hash"
    )
    target_value = common.exact_keys(
        intent["target"], CREATE_TARGET_KEYS, "Valkey fork intent target"
    )
    for key in CREATE_TARGET_KEYS:
        common.require_sha256(target_value[key], f"Valkey fork intent target {key}")
    if (
        target_value["descriptor_sha256"] != target_descriptor_sha256(checked_target)
        or target_value["source_identity_sha256"]
        != common.sha256_bytes(checked_target["valkey_cluster_id"].encode("utf-8"))
    ):
        _fail("Valkey fork intent target differs")
    request = common.exact_keys(
        intent["request"], INTENT_REQUEST_KEYS, "Valkey fork intent request"
    )
    if (
        request["method"] != "POST"
        or request["endpoint_label"] != "database-clusters"
        or request["provider_copy_contract"] != PROVIDER_COPY_CONTRACT
    ):
        _fail("Valkey fork intent request differs")
    common.require_sha256(request["request_sha256"], "Valkey fork intent request hash")
    spec = common.exact_keys(
        request["spec"], INTENT_SPEC_KEYS, "Valkey fork intent request spec"
    )
    if (
        spec["engine"] != "valkey"
        or spec["backup_created_at_omitted"] is not True
        or common.exact_int(spec["num_nodes"], "fork intent node count", 1, 10)
        != spec["num_nodes"]
    ):
        _fail("Valkey fork intent request spec differs")
    for key in (
        "region_sha256",
        "source_name_sha256",
        "private_network_uuid_sha256",
        "firewall_policy_sha256",
        "fork_name_sha256",
    ):
        common.require_sha256(spec[key], f"Valkey fork intent spec {key}")
    common.exact_string(spec["version"], "fork intent version")
    common.exact_string(spec["size"], "fork intent size", SIZE_RE)
    if spec["storage_size_mib"] is not None:
        common.exact_int(spec["storage_size_mib"], "fork intent storage", 1, 1_000_000_000)
    expected_name = deterministic_fork_name(phase, embedded_control)
    if spec["fork_name_sha256"] != common.sha256_bytes(
        expected_name.encode("utf-8")
    ):
        _fail("Valkey fork intent deterministic name differs")
    expected_operation_hash = common.sha256_value(
        {
            "phase": phase,
            "workflow_sha": embedded_control["workflow_sha"],
            "run_id": embedded_control["run_id"],
            "run_attempt": embedded_control["run_attempt"],
            "fork_name_sha256": spec["fork_name_sha256"],
            "request_sha256": request["request_sha256"],
        }
    )
    if operation_hash != expected_operation_hash:
        _fail("Valkey fork intent operation binding differs")
    provider = common.exact_keys(
        intent["provider"], PROVIDER_KEYS, "Valkey fork intent provider ledger"
    )
    if type(provider["endpoint_labels"]) is not list:
        _fail("Valkey fork intent provider ledger differs")
    if (
        provider["http_methods_used"] != ["GET"]
        or provider["mutation_request_count"] != 0
        or provider["http_request_count"] != len(provider["endpoint_labels"])
        or provider["endpoint_labels"]
        != [
            "valkey-cluster",
            "valkey-config",
            "valkey-source-firewall",
            "valkey-recovery-discovery",
        ]
    ):
        _fail("Valkey fork intent is not mutation-free")
    if intent["gates"] != {
        "source_ready": True,
        "source_firewall_exact_app": True,
        "fork_absent": True,
        "mutation_free": True,
    }:
        _fail("Valkey fork intent gates are incomplete")
    exact_hash = common.require_sha256(exact_sha256, "Valkey fork intent hash")
    if common.sha256_bytes(common.canonical_file_bytes(intent)) != exact_hash:
        _fail("Valkey fork intent exact-file hash differs")
    common.sanitize_public(intent, allowed_keys=("spec",))
    return intent


def _wait_for_online_fork(
    session: ProviderSession,
    identity: str,
    source: Mapping[str, Any],
    fork_name: str,
    *,
    sleeper: Callable[[float], None],
    poll_limit: int,
) -> dict[str, Any]:
    common.exact_int(poll_limit, "fork poll limit", 1, MAX_PROVIDER_POLLS)
    for attempt in range(poll_limit):
        database = _get_database(
            session, identity, "valkey-recovery-cluster-ready"
        )
        if database is None:
            _fail("created Valkey recovery fork disappeared")
        _validate_fork(database, source, fork_name, online=False)
        if database["status"] == "online":
            return database
        if attempt + 1 < poll_limit:
            sleeper(PROVIDER_POLL_SECONDS)
    _fail("Valkey recovery fork did not become online")


def _reconcile_ambiguous_create(
    session: ProviderSession,
    source: Mapping[str, Any],
    fork_name: str,
    *,
    sleeper: Callable[[float], None],
    poll_limit: int,
) -> dict[str, Any]:
    common.exact_int(poll_limit, "fork reconcile limit", 1, MAX_PROVIDER_POLLS)
    for attempt in range(poll_limit):
        matches = _list_named(session, fork_name)
        if len(matches) == 1:
            _validate_fork(matches[0], source, fork_name, online=False)
            return matches[0]
        if attempt + 1 < poll_limit:
            sleeper(PROVIDER_POLL_SECONDS)
    _fail("ambiguous Valkey fork creation did not reconcile to exactly one fork")


def _provider_ledger(session: ProviderSession, mutation_count: int) -> dict[str, Any]:
    methods: list[str] = []
    for method, _ in session.ledger:
        if method not in methods:
            methods.append(method)
    return {
        "http_methods_used": methods,
        "http_request_count": len(session.ledger),
        "endpoint_labels": [label for _, label in session.ledger],
        "mutation_request_count": mutation_count,
    }


def _create_or_reconcile_impl(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    create_intent: Mapping[str, Any],
    create_intent_sha256: str,
    read_transport: Transport,
    mutation_transport: Transport,
    now: dt.datetime,
    sleeper: Callable[[float], None] = lambda _: None,
    poll_limit: int = MAX_PROVIDER_POLLS,
) -> dict[str, Any]:
    """Perform at most one fork POST and reconcile an ambiguous result by GET."""

    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=PREPARE_WORKFLOW_PATH
    )
    phase = common.validate_phase(phase, "recovery phase")
    intent = validate_create_intent(
        create_intent,
        exact_sha256=create_intent_sha256,
        target=checked_target,
        control=checked_control,
        phase=phase,
        now=now,
    )
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    checked_now = _checked_clock(now, "Valkey fork creation")
    fork_name = deterministic_fork_name(phase, checked_control)
    session = ProviderSession(read_transport)
    source_before = _source_bundle(session, checked_target, binding)
    if _list_named(session, fork_name):
        _fail("deterministic recovery fork already exists before its one mutation")
    request_value = _create_request(source_before, fork_name)
    request_body = common.canonical_payload_bytes(request_value)
    request_hash = common.sha256_bytes(request_body)
    current_target = {
        "descriptor_sha256": target_descriptor_sha256(checked_target),
        "source_identity_sha256": source_before["database"]["safe"][
            "identity_sha256"
        ],
        "source_name_sha256": source_before["database"]["safe"]["name_sha256"],
        "source_observation_sha256": source_before["database"]["observation_sha256"],
        "source_topology_sha256": source_before["database"]["topology_sha256"],
        "source_config_sha256": source_before["config_sha256"],
        "source_firewall_sha256": source_before["firewall_sha256"],
    }
    if (
        intent["target"] != current_target
        or intent["request"]["request_sha256"] != request_hash
        or intent["request"]["spec"]
        != _intent_request_spec(source_before, fork_name)
    ):
        _fail("live fork request differs from immutable pre-wire intent")
    session.ledger.append(("POST", "create-valkey-recovery-fork"))
    try:
        result = mutation_transport.request("POST", DATABASES_PATH, request_body)
        if result.status != 201:
            _fail("Valkey fork create response status differs")
        fork = _database_envelope(result.value, "created Valkey recovery fork")
        _validate_fork(fork, source_before["database"], fork_name, online=False)
    except MutationRejected:
        raise
    except Exception as exc:
        try:
            _reconcile_ambiguous_create(
                session,
                source_before["database"],
                fork_name,
                sleeper=sleeper,
                poll_limit=poll_limit,
            )
        except Exception as reconcile_exc:
            raise MutationAmbiguous(
                "Valkey fork creation is quarantined; GET reconciliation failed"
            ) from reconcile_exc
        raise MutationAmbiguous(
            "ambiguous Valkey fork is quarantined and cannot authorize recovery"
        ) from exc
    fork = _wait_for_online_fork(
        session,
        fork["id"],
        source_before["database"],
        fork_name,
        sleeper=sleeper,
        poll_limit=poll_limit,
    )
    fork_created = common.require_timestamp(
        fork["created_at"], "created Valkey recovery fork created_at"
    )
    intent_issued = common.require_timestamp(
        intent["issued_at"], "Valkey fork create intent issued_at"
    )
    intent_expires = common.require_timestamp(
        intent["expires_at"], "Valkey fork create intent expires_at"
    )
    if fork_created < intent_issued or fork_created >= intent_expires:
        _fail("created Valkey recovery fork is outside the exact intent window")
    fork_config, fork_config_hash = _get_config(
        session, fork["id"], "valkey-recovery-config-ready"
    )
    fork_firewall = _get_firewall(
        session, fork["id"], "valkey-recovery-firewall-ready"
    )
    _require_exact_app_firewall(
        fork_firewall,
        binding["app_id_sha256"],
        "Valkey recovery fork",
    )
    source_after = _source_bundle(
        session, checked_target, binding, suffix="-post-create"
    )
    if (
        source_before["database"]["observation_sha256"]
        != source_after["database"]["observation_sha256"]
        or source_before["config_sha256"] != source_after["config_sha256"]
        or source_before["firewall_sha256"] != source_after["firewall_sha256"]
    ):
        _fail("production Valkey source changed during fork creation")
    if (
        source_before["config"] != fork_config
        or source_before["database"]["topology_sha256"] != fork["topology_sha256"]
        or source_before["firewall_policy"] != fork_firewall["policy"]
    ):
        _fail(
            "provider fork is not an exact nonpublic copy of source configuration"
        )
    issued_at = common.format_timestamp(checked_now)
    expires_at = common.format_timestamp(
        checked_now + dt.timedelta(seconds=MAX_FORK_TTL_SECONDS)
    )
    private_values = (
        checked_target["postgres_cluster_id"],
        checked_target["valkey_cluster_id"],
        source_before["database"]["name"],
        source_before["database"]["private_network_uuid"],
        source_before["firewall_app_id"],
        fork["id"],
        fork_name,
    )
    receipt = {
        "schema_version": 2,
        "authority": CREATE_AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": issued_at,
        "expires_at": expires_at,
        "phase": phase,
        "control": checked_control,
        "target": {
            "descriptor_sha256": target_descriptor_sha256(checked_target),
            "source_identity_sha256": source_before["database"]["safe"][
                "identity_sha256"
            ],
            "source_name_sha256": source_before["database"]["safe"][
                "name_sha256"
            ],
            "source_observation_sha256": source_before["database"][
                "observation_sha256"
            ],
            "source_topology_sha256": source_before["database"][
                "topology_sha256"
            ],
            "source_config_sha256": source_before["config_sha256"],
            "source_firewall_sha256": source_before["firewall_sha256"],
        },
        "request": {
            "method": "POST",
            "endpoint_label": "database-clusters",
            "request_sha256": request_hash,
            "request_attempt_count": 1,
            "provider_copy_contract": PROVIDER_COPY_CONTRACT,
        },
        "result": {
            "outcome": "created",
            "recovery_identity_sha256": fork["safe"]["identity_sha256"],
            "fork_name_sha256": fork["safe"]["name_sha256"],
            "fork_created_at_sha256": fork["safe"]["created_at_sha256"],
            "recovery_observation_sha256": fork["observation_sha256"],
            "recovery_topology_sha256": fork["topology_sha256"],
            "recovery_config_sha256": fork_config_hash,
            "recovery_firewall_sha256": fork_firewall["sha256"],
            "mutation_ambiguous_reconciled": False,
        },
        "provider": _provider_ledger(session, 1),
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
    common.sanitize_public(receipt, private_values=private_values)
    return receipt


def create_or_reconcile(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    create_intent: Mapping[str, Any],
    create_intent_sha256: str,
    read_transport: Transport,
    mutation_transport: Transport,
    now: dt.datetime,
    sleeper: Callable[[float], None] = lambda _: None,
    poll_limit: int = MAX_PROVIDER_POLLS,
) -> dict[str, Any]:
    """Create once; quarantine every uncertainty after the POST may cross wire."""

    if read_transport is mutation_transport:
        _fail("provider read and create capabilities are not separated")
    tracked = _MutationTrackingTransport(mutation_transport)
    try:
        return _create_or_reconcile_impl(
            target=target,
            control=control,
            phase=phase,
            contract=contract,
            contract_file_sha256=contract_file_sha256,
            create_intent=create_intent,
            create_intent_sha256=create_intent_sha256,
            read_transport=read_transport,
            mutation_transport=tracked,
            now=now,
            sleeper=sleeper,
            poll_limit=poll_limit,
        )
    except (MutationRejected, MutationAmbiguous):
        raise
    except Exception as exc:
        if not tracked.mutation_attempted:
            raise
        try:
            prepare_control = validate_control(
                dict(control), workflow_path=PREPARE_WORKFLOW_PATH
            )
            fork_name = deterministic_fork_name(
                common.validate_phase(phase, "recovery phase"), prepare_control
            )
            _list_named(ProviderSession(read_transport), fork_name)
        except Exception as reconcile_exc:
            raise MutationAmbiguous(
                "post-create verification is quarantined; GET reconciliation failed"
            ) from reconcile_exc
        raise MutationAmbiguous(
            "post-create verification failed; fork is quarantined"
        ) from exc


def validate_create_receipt(
    value: Any,
    *,
    exact_sha256: str,
    target: Mapping[str, Any],
    phase: str,
    now: dt.datetime,
    allow_expired: bool = False,
    allow_expired_intent: bool = False,
    create_intent: Mapping[str, Any] | None = None,
    create_intent_sha256: str | None = None,
    current_control: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    receipt = common.exact_keys(
        value,
        {
            "schema_version",
            "authority",
            "repository",
            "issued_at",
            "expires_at",
            "phase",
            "control",
            "target",
            "request",
            "result",
            "provider",
            "gates",
        },
        "Valkey fork create receipt",
    )
    if (
        receipt["schema_version"] != 2
        or receipt["authority"] != CREATE_AUTHORITY
        or receipt["repository"] != common.REPOSITORY
        or receipt["phase"] != common.validate_phase(phase, "recovery phase")
    ):
        _fail("Valkey fork create receipt authority differs")
    validate_control(
        receipt["control"], workflow_path=PREPARE_WORKFLOW_PATH
    )
    if allow_expired:
        issued = common.require_timestamp(
            receipt["issued_at"], "Valkey fork create receipt issued_at"
        )
        expires = common.require_timestamp(
            receipt["expires_at"], "Valkey fork create receipt expires_at"
        )
        if (
            expires <= issued
            or (expires - issued).total_seconds() > MAX_FORK_TTL_SECONDS
            or _checked_clock(now, "Valkey fork create receipt") < issued
        ):
            _fail("Valkey fork create receipt validity window differs")
    else:
        common.validate_fresh_window(
            receipt["issued_at"],
            receipt["expires_at"],
            now,
            maximum_age_seconds=MAX_FORK_TTL_SECONDS,
            label="Valkey fork create receipt",
        )
    checked_target = validate_target_descriptor(dict(target))
    target_value = common.exact_keys(
        receipt["target"], CREATE_TARGET_KEYS, "Valkey fork receipt target"
    )
    for key in CREATE_TARGET_KEYS:
        common.require_sha256(target_value[key], f"Valkey fork receipt target {key}")
    if (
        target_value["descriptor_sha256"] != target_descriptor_sha256(checked_target)
        or target_value["source_identity_sha256"]
        != common.sha256_bytes(checked_target["valkey_cluster_id"].encode("utf-8"))
    ):
        _fail("Valkey fork receipt target descriptor differs")
    request = common.exact_keys(
        receipt["request"], CREATE_REQUEST_KEYS, "Valkey fork request receipt"
    )
    if (
        request["method"] != "POST"
        or request["endpoint_label"] != "database-clusters"
        or request["provider_copy_contract"] != PROVIDER_COPY_CONTRACT
        or common.exact_int(
            request["request_attempt_count"], "fork request attempt count", 1, 1
        )
        != 1
    ):
        _fail("Valkey fork request receipt differs")
    common.require_sha256(request["request_sha256"], "Valkey fork request hash")
    result = common.exact_keys(
        receipt["result"], CREATE_RESULT_KEYS, "Valkey fork result receipt"
    )
    if result["outcome"] != "created":
        _fail("Valkey fork result differs")
    for key in CREATE_RESULT_KEYS - {"outcome", "mutation_ambiguous_reconciled"}:
        common.require_sha256(result[key], f"Valkey fork result {key}")
    ambiguous = common.exact_bool(
        result["mutation_ambiguous_reconciled"], "fork ambiguity result"
    )
    prepare_control = validate_control(
        receipt["control"], workflow_path=PREPARE_WORKFLOW_PATH
    )
    intent_binding = (
        create_intent is not None
        or create_intent_sha256 is not None
        or current_control is not None
    )
    if intent_binding:
        if (
            type(create_intent) is not dict
            or type(create_intent_sha256) is not str
            or type(current_control) is not dict
        ):
            _fail("Valkey fork receipt pre-wire authority is incomplete")
        checked_current = validate_control(
            dict(current_control), workflow_path=PREPARE_WORKFLOW_PATH
        )
        signed_intent = validate_create_intent(
            create_intent,
            exact_sha256=create_intent_sha256,
            target=checked_target,
            control=checked_current,
            phase=receipt["phase"],
            now=now,
            allow_expired=allow_expired or allow_expired_intent,
        )
        intent_request = signed_intent["request"]
        intent_issued = common.require_timestamp(
            signed_intent["issued_at"], "Valkey fork create intent issued_at"
        )
        intent_expires = common.require_timestamp(
            signed_intent["expires_at"], "Valkey fork create intent expires_at"
        )
        receipt_issued = common.require_timestamp(
            receipt["issued_at"], "Valkey fork create receipt issued_at"
        )
        if (
            prepare_control != signed_intent["control"]
            or prepare_control != checked_current
            or target_value != signed_intent["target"]
            or request["method"] != intent_request["method"]
            or request["endpoint_label"] != intent_request["endpoint_label"]
            or request["request_sha256"] != intent_request["request_sha256"]
            or request["provider_copy_contract"]
            != intent_request["provider_copy_contract"]
            or receipt_issued < intent_issued
            or receipt_issued >= intent_expires
        ):
            _fail("Valkey fork receipt differs from signed pre-wire authority")
    expected_name = deterministic_fork_name(receipt["phase"], prepare_control)
    if (
        ambiguous is not False
        or result["fork_name_sha256"]
        != common.sha256_bytes(expected_name.encode("utf-8"))
    ):
        _fail("Valkey fork ambiguity result differs")
    provider = common.exact_keys(
        receipt["provider"], PROVIDER_KEYS, "Valkey fork provider ledger"
    )
    if type(provider["endpoint_labels"]) is not list or not all(
        type(item) is str for item in provider["endpoint_labels"]
    ):
        _fail("Valkey fork provider ledger differs")
    labels = provider["endpoint_labels"]
    expected_prefix = [
        "valkey-cluster",
        "valkey-config",
        "valkey-source-firewall",
        "valkey-recovery-discovery",
        "create-valkey-recovery-fork",
    ]
    expected_suffix = [
        "valkey-recovery-config-ready",
        "valkey-recovery-firewall-ready",
        "valkey-cluster-post-create",
        "valkey-config-post-create",
        "valkey-source-firewall-post-create",
    ]
    ready_labels = labels[len(expected_prefix) : -len(expected_suffix)]
    if (
        provider["http_methods_used"] != ["GET", "POST"]
        or common.exact_int(
            provider["mutation_request_count"], "fork mutation count", 1, 1
        )
        != 1
        or common.exact_int(
            provider["http_request_count"],
            "fork HTTP request count",
            len(expected_prefix) + 1 + len(expected_suffix),
            len(expected_prefix) + MAX_PROVIDER_POLLS + len(expected_suffix),
        )
        != len(labels)
        or labels[: len(expected_prefix)] != expected_prefix
        or labels[-len(expected_suffix) :] != expected_suffix
        or not 1 <= len(ready_labels) <= MAX_PROVIDER_POLLS
        or any(label != "valkey-recovery-cluster-ready" for label in ready_labels)
    ):
        _fail("Valkey fork provider ledger differs")
    gates = common.exact_keys(
        receipt["gates"], CREATE_GATE_KEYS, "Valkey fork gates"
    )
    if any(gates[key] is not True for key in CREATE_GATE_KEYS):
        _fail("Valkey fork gates are incomplete")
    exact_hash = common.require_sha256(exact_sha256, "Valkey fork receipt hash")
    if common.sha256_bytes(common.canonical_file_bytes(receipt)) != exact_hash:
        _fail("Valkey fork receipt exact-file hash differs")
    common.sanitize_public(receipt)
    return receipt


def validate_create_receipt_live(
    value: Any,
    *,
    exact_sha256: str,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    create_intent: Mapping[str, Any],
    create_intent_sha256: str,
    read_transport: Transport,
    now: dt.datetime,
) -> dict[str, Any]:
    """Pre-sign gate: bind intent fields and independently GET all dynamic facts."""

    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=PREPARE_WORKFLOW_PATH
    )
    checked_phase = common.validate_phase(phase, "recovery phase")
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    receipt = validate_create_receipt(
        value,
        exact_sha256=exact_sha256,
        target=checked_target,
        phase=checked_phase,
        now=now,
        allow_expired_intent=True,
        create_intent=create_intent,
        create_intent_sha256=create_intent_sha256,
        current_control=checked_control,
    )
    intent = validate_create_intent(
        create_intent,
        exact_sha256=create_intent_sha256,
        target=checked_target,
        control=checked_control,
        phase=checked_phase,
        now=now,
        allow_expired=True,
    )
    fork_name = deterministic_fork_name(checked_phase, checked_control)
    session = ProviderSession(read_transport)
    matches = _list_named(session, fork_name)
    if len(matches) != 1:
        _fail("create receipt fork is not uniquely discoverable at its live gate")
    source = _source_bundle(session, checked_target, binding)
    fork = _fork_bundle(session, matches[0]["id"], source, fork_name)
    source_database = source["database"]
    fork_database = fork["database"]
    expected_target = {
        "descriptor_sha256": target_descriptor_sha256(checked_target),
        "source_identity_sha256": source_database["safe"]["identity_sha256"],
        "source_name_sha256": source_database["safe"]["name_sha256"],
        "source_observation_sha256": source_database["observation_sha256"],
        "source_topology_sha256": source_database["topology_sha256"],
        "source_config_sha256": source["config_sha256"],
        "source_firewall_sha256": source["firewall_sha256"],
    }
    expected_result = {
        "outcome": "created",
        "recovery_identity_sha256": fork_database["safe"]["identity_sha256"],
        "fork_name_sha256": fork_database["safe"]["name_sha256"],
        "fork_created_at_sha256": fork_database["safe"]["created_at_sha256"],
        "recovery_observation_sha256": fork_database["observation_sha256"],
        "recovery_topology_sha256": fork_database["topology_sha256"],
        "recovery_config_sha256": fork["config_sha256"],
        "recovery_firewall_sha256": fork["firewall_sha256"],
        "mutation_ambiguous_reconciled": False,
    }
    if receipt["target"] != expected_target or receipt["result"] != expected_result:
        _fail("create receipt differs from independent live provider observation")
    if (
        source_database["topology_sha256"] != fork_database["topology_sha256"]
        or source["config"] != fork["config"]
        or source["firewall_policy"] != fork["firewall_policy"]
    ):
        _fail(
            "live create receipt fork is not an exact nonpublic provider copy"
        )
    created = common.require_timestamp(
        fork_database["created_at"], "live create receipt fork created_at"
    )
    intent_issued = common.require_timestamp(
        intent["issued_at"], "Valkey fork create intent issued_at"
    )
    intent_expires = common.require_timestamp(
        intent["expires_at"], "Valkey fork create intent expires_at"
    )
    checked_now = _checked_clock(now, "Valkey fork live create receipt gate")
    if (
        created < intent_issued
        or created >= intent_expires
        or created > checked_now
        or checked_now - created > dt.timedelta(seconds=MAX_FORK_TTL_SECONDS)
    ):
        _fail("live create receipt fork timestamp differs")
    if session.ledger != [
        ("GET", "valkey-recovery-discovery"),
        ("GET", "valkey-cluster"),
        ("GET", "valkey-config"),
        ("GET", "valkey-source-firewall"),
        ("GET", "valkey-recovery-cluster"),
        ("GET", "valkey-recovery-config"),
        ("GET", "valkey-recovery-firewall"),
    ]:
        _fail("live create receipt provider ledger differs")
    return receipt


def observe_readiness(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    create_receipt: Mapping[str, Any],
    create_receipt_sha256: str,
    transport: Transport,
    now: dt.datetime,
    sleeper: Callable[[float], None] = lambda _: None,
) -> dict[str, Any]:
    """Return a hash-only v2 provider-fork proof from two exact GET rounds."""

    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=READINESS_WORKFLOW_PATH
    )
    phase = common.validate_phase(phase, "recovery phase")
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    receipt = validate_create_receipt(
        create_receipt,
        exact_sha256=create_receipt_sha256,
        target=checked_target,
        phase=phase,
        now=now,
    )
    prepare_control = validate_control(
        receipt["control"], workflow_path=PREPARE_WORKFLOW_PATH
    )
    _require_same_release_binding(checked_control, prepare_control)
    fork_name = deterministic_fork_name(phase, prepare_control)
    session = ProviderSession(transport)
    matches = _list_named(session, fork_name)
    if len(matches) != 1:
        _fail("receipt-bound Valkey recovery fork is not uniquely discoverable")
    recovery_id = matches[0]["id"]
    if matches[0]["safe"]["identity_sha256"] != receipt["result"][
        "recovery_identity_sha256"
    ]:
        _fail("discovered Valkey recovery fork identity differs from receipt")
    rounds: list[dict[str, Any]] = []
    for index in range(2):
        source = _source_bundle(session, checked_target, binding)
        fork = _fork_bundle(
            session, recovery_id, source, fork_name
        )
        rounds.append({"source": source, "fork": fork})
        if index == 0:
            sleeper(2)
    def stable_projection(value: Mapping[str, Any]) -> dict[str, Any]:
        return {
            "source_observation_sha256": value["source"]["database"][
                "observation_sha256"
            ],
            "source_topology_sha256": value["source"]["database"][
                "topology_sha256"
            ],
            "source_config_sha256": value["source"]["config_sha256"],
            "source_firewall_sha256": value["source"]["firewall_sha256"],
            "recovery_observation_sha256": value["fork"]["database"][
                "observation_sha256"
            ],
            "recovery_topology_sha256": value["fork"]["database"][
                "topology_sha256"
            ],
            "recovery_config_sha256": value["fork"]["config_sha256"],
            "recovery_firewall_sha256": value["fork"]["firewall_sha256"],
        }
    first = stable_projection(rounds[0])
    second = stable_projection(rounds[1])
    if first != second:
        _fail("Valkey provider state changed between readiness observations")
    source = rounds[0]["source"]
    fork = rounds[0]["fork"]
    if (
        first["source_observation_sha256"]
        != receipt["target"]["source_observation_sha256"]
        or first["source_topology_sha256"]
        != receipt["target"]["source_topology_sha256"]
        or first["source_config_sha256"]
        != receipt["target"]["source_config_sha256"]
        or first["source_firewall_sha256"]
        != receipt["target"]["source_firewall_sha256"]
        or first["recovery_observation_sha256"]
        != receipt["result"]["recovery_observation_sha256"]
        or first["recovery_topology_sha256"]
        != receipt["result"]["recovery_topology_sha256"]
        or first["recovery_config_sha256"]
        != receipt["result"]["recovery_config_sha256"]
        or first["recovery_firewall_sha256"]
        != receipt["result"]["recovery_firewall_sha256"]
    ):
        _fail("Valkey readiness observation differs from fork receipt")
    if (
        source["database"]["topology_sha256"]
        != fork["database"]["topology_sha256"]
        or source["config"] != fork["config"]
        or source["firewall_policy"] != fork["firewall_policy"]
    ):
        _fail("Valkey recovery fork is not an exact nonpublic provider copy")
    created = common.require_timestamp(
        fork["database"]["created_at"], "Valkey recovery fork created_at"
    )
    checked_now = _checked_clock(now, "Valkey fork readiness")
    if created > checked_now or checked_now - created > dt.timedelta(
        seconds=MAX_FORK_TTL_SECONDS
    ):
        _fail("Valkey recovery fork is stale or future-dated")
    proof = {
        "authority": READINESS_AUTHORITY,
        "source_identity_sha256": source["database"]["safe"]["identity_sha256"],
        "recovery_identity_sha256": fork["database"]["safe"][
            "identity_sha256"
        ],
        "request_sha256": receipt["request"]["request_sha256"],
        "receipt_sha256": common.require_sha256(
            create_receipt_sha256, "Valkey fork receipt hash"
        ),
        "source_config_sha256": source["config_sha256"],
        "recovery_config_sha256": fork["config_sha256"],
        "source_firewall_sha256": source["firewall_sha256"],
        "recovery_firewall_sha256": fork["firewall_sha256"],
        "fork_name_sha256": fork["database"]["safe"]["name_sha256"],
        "fork_created_at_sha256": fork["database"]["safe"][
            "created_at_sha256"
        ],
        "provider_copy_contract": PROVIDER_COPY_CONTRACT,
        "stable_read_count": 2,
        "request_attempt_count": receipt["request"]["request_attempt_count"],
        "mutation_ambiguous_reconciled": receipt["result"][
            "mutation_ambiguous_reconciled"
        ],
        "source_firewall_unchanged": True,
        "source_firewall_exact_app": True,
        "recovery_firewall_exact_source_app": True,
        "recovery_restricted_to_exact_production_app": True,
    }
    common.exact_keys(proof, PROVIDER_FORK_KEYS, "Valkey provider fork proof")
    public = {
        "schema_version": 2,
        "authority": "production-valkey-recovery-fork-observation-v2",
        "repository": common.REPOSITORY,
        "issued_at": common.format_timestamp(checked_now),
        "phase": phase,
        "control": checked_control,
        "provider_fork": proof,
        "provider": _provider_ledger(session, 0),
    }
    private_values = (
        checked_target["postgres_cluster_id"],
        checked_target["valkey_cluster_id"],
        source["database"]["name"],
        source["database"]["private_network_uuid"],
        recovery_id,
        fork_name,
    )
    common.sanitize_public(public, private_values=private_values)
    return public


def _require_intent_cleanup_candidate(
    candidate: Mapping[str, Any], intent: Mapping[str, Any]
) -> None:
    spec = intent["request"]["spec"]
    observed = {
        "engine": candidate["engine"],
        "version": candidate["version"],
        "region_sha256": candidate["safe"]["region_sha256"],
        "size": candidate["size"],
        "num_nodes": candidate["num_nodes"],
        "storage_size_mib": candidate["storage_size_mib"],
        "source_name_sha256": spec["source_name_sha256"],
        "private_network_uuid_sha256": candidate["safe"][
            "private_network_uuid_sha256"
        ],
        "firewall_policy_sha256": spec["firewall_policy_sha256"],
        "fork_name_sha256": candidate["safe"]["name_sha256"],
        "backup_created_at_omitted": True,
    }
    if observed != spec:
        _fail("cleanup candidate differs from the exact create intent")
    created = common.require_timestamp(
        candidate["created_at"], "cleanup candidate created_at"
    )
    issued = common.require_timestamp(
        intent["issued_at"], "Valkey fork create intent issued_at"
    )
    expires = common.require_timestamp(
        intent["expires_at"], "Valkey fork create intent expires_at"
    )
    if created < issued or created >= expires:
        _fail("cleanup candidate was not created within the exact intent window")


def cleanup_authority_binding(
    cleanup_evidence: Any, cleanup_mode: str
) -> tuple[str, str]:
    mode = common.exact_string(cleanup_mode, "Valkey fork cleanup mode")
    if mode not in CLEANUP_MODES:
        _fail("Valkey fork cleanup mode differs")
    if type(cleanup_evidence) is not dict:
        _fail("Valkey fork cleanup evidence is not an object")
    evidence_mode = common.exact_string(
        cleanup_evidence.get("mode"), "Valkey fork cleanup evidence mode"
    )
    if evidence_mode != mode:
        _fail("Valkey fork cleanup evidence mode differs")
    return mode, common.sha256_bytes(common.canonical_file_bytes(cleanup_evidence))


def _delete_or_reconcile_impl(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    read_transport: Transport,
    mutation_transport: Transport,
    now: dt.datetime,
    cleanup_mode: str,
    cleanup_evidence: Mapping[str, Any],
    create_receipt: Mapping[str, Any] | None = None,
    create_receipt_sha256: str | None = None,
    create_intent: Mapping[str, Any] | None = None,
    create_intent_sha256: str | None = None,
    sleeper: Callable[[float], None] = lambda _: None,
    poll_limit: int = MAX_PROVIDER_POLLS,
) -> dict[str, Any]:
    """Delete one exact fork bound to a receipt or failed-create intent."""

    receipt_mode = create_receipt is not None or create_receipt_sha256 is not None
    intent_mode = create_intent is not None or create_intent_sha256 is not None
    if receipt_mode == intent_mode:
        _fail("cleanup requires exactly one create receipt or create intent authority")
    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=CLEANUP_WORKFLOW_PATH
    )
    phase = common.validate_phase(phase, "recovery phase")
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    checked_cleanup_mode, cleanup_authority_sha256 = cleanup_authority_binding(
        cleanup_evidence, cleanup_mode
    )
    recovery_hash: str | None
    if receipt_mode:
        if type(create_receipt) is not dict or type(create_receipt_sha256) is not str:
            _fail("cleanup create receipt authority is incomplete")
        authority = validate_create_receipt(
            create_receipt,
            exact_sha256=create_receipt_sha256,
            target=checked_target,
            phase=phase,
            now=now,
            allow_expired=True,
        )
        prepare_control = validate_control(
            authority["control"], workflow_path=PREPARE_WORKFLOW_PATH
        )
        expected_name_hash = authority["result"]["fork_name_sha256"]
        recovery_hash = authority["result"]["recovery_identity_sha256"]
        authority_kind = "create-receipt"
        authority_sha = common.require_sha256(
            create_receipt_sha256, "Valkey fork receipt hash"
        )
    else:
        if type(create_intent) is not dict or type(create_intent_sha256) is not str:
            _fail("cleanup create intent authority is incomplete")
        authority = validate_create_intent(
            create_intent,
            exact_sha256=create_intent_sha256,
            target=checked_target,
            control=None,
            phase=phase,
            now=now,
            allow_expired=True,
        )
        prepare_control = validate_control(
            authority["control"], workflow_path=PREPARE_WORKFLOW_PATH
        )
        expected_name_hash = authority["request"]["spec"]["fork_name_sha256"]
        recovery_hash = None
        authority_kind = "create-intent"
        authority_sha = common.require_sha256(
            create_intent_sha256, "Valkey fork intent hash"
        )
    if (
        checked_cleanup_mode
        in {"terminal", "never-started", "pre-mutation-failure", "no-mutation"}
    ) != receipt_mode:
        _fail("cleanup mode differs from its create authority")
    _require_cleanup_release_binding(checked_control, prepare_control)
    fork_name = deterministic_fork_name(phase, prepare_control)
    if common.sha256_bytes(fork_name.encode("utf-8")) != expected_name_hash:
        _fail("cleanup deterministic fork name differs from create authority")
    session = ProviderSession(read_transport)
    source_before = _source_bundle(
        session, checked_target, binding, suffix="-pre-delete"
    )
    source_authority = _source_authority_projection(checked_target, source_before)
    if source_authority != authority["target"]:
        _fail("production Valkey source differs before recovery cleanup")
    matches = _list_named(session, fork_name)
    mutation_count = 0
    ambiguous = False
    outcome = "already-absent"
    first_absence_observed = not matches
    if matches:
        fork = matches[0]
        if fork["id"] in checked_target.values():
            _fail("cleanup candidate overlaps a protected production database")
        if receipt_mode:
            if (
                fork["safe"]["identity_sha256"] != recovery_hash
                or fork["safe"]["name_sha256"] != expected_name_hash
            ):
                _fail("cleanup candidate differs from the exact create receipt")
        else:
            _require_intent_cleanup_candidate(fork, authority)
            recovery_hash = fork["safe"]["identity_sha256"]
        mutation_count = 1
        session.ledger.append(("DELETE", "delete-valkey-recovery-fork"))
        try:
            result = mutation_transport.request(
                "DELETE", f"/v2/databases/{fork['id']}"
            )
            if result.status not in {204, 404} or result.value is not None:
                _fail("Valkey recovery fork delete response differs")
        except MutationRejected:
            raise
        except Exception:
            ambiguous = True
        common.exact_int(poll_limit, "delete reconcile limit", 1, MAX_PROVIDER_POLLS)
        for attempt in range(poll_limit):
            if not _list_named(session, fork_name):
                first_absence_observed = True
                outcome = "ambiguous-reconciled" if ambiguous else "deleted"
                break
            if attempt + 1 < poll_limit:
                sleeper(PROVIDER_POLL_SECONDS)
        else:
            _fail("Valkey recovery fork deletion did not reconcile to absence")
    if not first_absence_observed:
        _fail("Valkey recovery fork absence was not observed")
    for index in range(2):
        source_round = _source_bundle(
            session, checked_target, binding, suffix=f"-cleanup-{index + 1}"
        )
        if (
            _source_authority_projection(checked_target, source_round)
            != source_authority
        ):
            _fail("production Valkey source changed during recovery cleanup")
        if _list_named(
            session,
            fork_name,
            label=f"valkey-recovery-cleanup-absence-{index + 1}",
        ):
            _fail("Valkey recovery fork reappeared after deletion")
        if index == 0:
            sleeper(2)
    checked_now = _checked_clock(now, "Valkey fork cleanup")
    result = {
        "schema_version": 2,
        "authority": DELETE_AUTHORITY,
        "repository": common.REPOSITORY,
        "issued_at": common.format_timestamp(checked_now),
        "phase": phase,
        "control": checked_control,
        "target": {
            **source_authority,
            "recovery_identity_sha256": recovery_hash if receipt_mode else None,
            "fork_name_sha256": expected_name_hash,
            "create_authority": authority_kind,
            "create_authority_sha256": authority_sha,
            "cleanup_mode": checked_cleanup_mode,
            "cleanup_authority_sha256": cleanup_authority_sha256,
        },
        "result": {
            "outcome": outcome,
            "deletion_request_attempt_count": mutation_count,
            "mutation_ambiguous_reconciled": ambiguous,
            "stable_absence_read_count": 2,
            "source_stable_read_count": 2,
            "fork_absent": True,
        },
        "provider": _provider_ledger(session, mutation_count),
        "gates": {
            "authority_bound": True,
            "exact_single_or_zero_mutation": True,
            "source_ready": True,
            "source_stable": True,
            "source_firewall_exact_app": True,
            "fork_absent": True,
        },
    }
    common.sanitize_public(
        result,
        private_values=(
            checked_target["postgres_cluster_id"],
            checked_target["valkey_cluster_id"],
            source_before["firewall_app_id"],
            fork_name,
        ),
    )
    return result


def delete_or_reconcile(
    *,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    read_transport: Transport,
    mutation_transport: Transport,
    now: dt.datetime,
    cleanup_mode: str,
    cleanup_evidence: Mapping[str, Any],
    create_receipt: Mapping[str, Any] | None = None,
    create_receipt_sha256: str | None = None,
    create_intent: Mapping[str, Any] | None = None,
    create_intent_sha256: str | None = None,
    sleeper: Callable[[float], None] = lambda _: None,
    poll_limit: int = MAX_PROVIDER_POLLS,
) -> dict[str, Any]:
    """Delete once; quarantine every uncertainty after the DELETE may cross wire."""

    if read_transport is mutation_transport:
        _fail("provider read and delete capabilities are not separated")
    tracked = _MutationTrackingTransport(mutation_transport)
    try:
        return _delete_or_reconcile_impl(
            target=target,
            control=control,
            phase=phase,
            contract=contract,
            contract_file_sha256=contract_file_sha256,
            read_transport=read_transport,
            mutation_transport=tracked,
            now=now,
            cleanup_mode=cleanup_mode,
            cleanup_evidence=cleanup_evidence,
            create_receipt=create_receipt,
            create_receipt_sha256=create_receipt_sha256,
            create_intent=create_intent,
            create_intent_sha256=create_intent_sha256,
            sleeper=sleeper,
            poll_limit=poll_limit,
        )
    except (MutationRejected, MutationAmbiguous):
        raise
    except Exception as exc:
        if not tracked.mutation_attempted:
            raise
        try:
            authority = create_receipt if create_receipt is not None else create_intent
            if type(authority) is not dict:
                _fail("cleanup create authority is unavailable")
            prepare_control = validate_control(
                authority["control"], workflow_path=PREPARE_WORKFLOW_PATH
            )
            fork_name = deterministic_fork_name(
                common.validate_phase(phase, "recovery phase"), prepare_control
            )
            _list_named(ProviderSession(read_transport), fork_name)
        except Exception as reconcile_exc:
            raise MutationAmbiguous(
                "post-delete verification is quarantined; GET reconciliation failed"
            ) from reconcile_exc
        raise MutationAmbiguous(
            "post-delete verification failed; cleanup remains quarantined"
        ) from exc


def validate_delete_receipt(
    value: Any,
    *,
    exact_sha256: str,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    now: dt.datetime,
    cleanup_mode: str,
    cleanup_evidence: Mapping[str, Any],
    create_receipt: Mapping[str, Any] | None = None,
    create_receipt_sha256: str | None = None,
    create_intent: Mapping[str, Any] | None = None,
    create_intent_sha256: str | None = None,
) -> dict[str, Any]:
    """Validate the exact cleanup receipt before its policy attestation."""

    receipt = common.exact_keys(
        value,
        {
            "schema_version",
            "authority",
            "repository",
            "issued_at",
            "phase",
            "control",
            "target",
            "result",
            "provider",
            "gates",
        },
        "Valkey fork delete receipt",
    )
    checked_phase = common.validate_phase(phase, "recovery phase")
    if (
        receipt["schema_version"] != 2
        or receipt["authority"] != DELETE_AUTHORITY
        or receipt["repository"] != common.REPOSITORY
        or receipt["phase"] != checked_phase
    ):
        _fail("Valkey fork delete receipt authority differs")
    checked_control = validate_control(
        dict(control), workflow_path=CLEANUP_WORKFLOW_PATH
    )
    if (
        validate_control(
            receipt["control"], workflow_path=CLEANUP_WORKFLOW_PATH
        )
        != checked_control
    ):
        _fail("Valkey fork delete receipt control differs")
    issued = common.require_timestamp(
        receipt["issued_at"], "Valkey fork delete receipt issued_at"
    )
    checked_now = _checked_clock(now, "Valkey fork delete receipt")
    if issued > checked_now or checked_now - issued > dt.timedelta(
        seconds=MAX_INTENT_AGE_SECONDS
    ):
        _fail("Valkey fork delete receipt is stale or future-dated")
    checked_target = validate_target_descriptor(dict(target))
    checked_cleanup_mode, cleanup_authority_sha256 = cleanup_authority_binding(
        cleanup_evidence, cleanup_mode
    )
    receipt_mode = create_receipt is not None or create_receipt_sha256 is not None
    intent_mode = create_intent is not None or create_intent_sha256 is not None
    if receipt_mode == intent_mode:
        _fail(
            "Valkey fork delete receipt validation requires exactly one create authority"
        )
    if receipt_mode:
        if type(create_receipt) is not dict or type(create_receipt_sha256) is not str:
            _fail("cleanup create receipt authority is incomplete")
        create_authority_value = validate_create_receipt(
            create_receipt,
            exact_sha256=create_receipt_sha256,
            target=checked_target,
            phase=checked_phase,
            now=now,
            allow_expired=True,
        )
        prepare_control = validate_control(
            create_authority_value["control"], workflow_path=PREPARE_WORKFLOW_PATH
        )
        expected_create_kind = "create-receipt"
        expected_create_sha = common.require_sha256(
            create_receipt_sha256, "Valkey fork receipt hash"
        )
        expected_recovery_identity = create_authority_value["result"][
            "recovery_identity_sha256"
        ]
        expected_fork_name = create_authority_value["result"]["fork_name_sha256"]
    else:
        if type(create_intent) is not dict or type(create_intent_sha256) is not str:
            _fail("cleanup create intent authority is incomplete")
        create_authority_value = validate_create_intent(
            create_intent,
            exact_sha256=create_intent_sha256,
            target=checked_target,
            control=None,
            phase=checked_phase,
            now=now,
            allow_expired=True,
        )
        prepare_control = validate_control(
            create_authority_value["control"], workflow_path=PREPARE_WORKFLOW_PATH
        )
        expected_create_kind = "create-intent"
        expected_create_sha = common.require_sha256(
            create_intent_sha256, "Valkey fork intent hash"
        )
        expected_recovery_identity = None
        expected_fork_name = create_authority_value["request"]["spec"][
            "fork_name_sha256"
        ]
    _require_cleanup_release_binding(checked_control, prepare_control)
    deterministic_name_hash = common.sha256_bytes(
        deterministic_fork_name(checked_phase, prepare_control).encode("utf-8")
    )
    if expected_fork_name != deterministic_name_hash:
        _fail("cleanup deterministic fork name differs from create authority")
    target_value = common.exact_keys(
        receipt["target"], DELETE_TARGET_KEYS, "Valkey fork delete target"
    )
    for key in (
        "descriptor_sha256",
        "source_identity_sha256",
        "source_name_sha256",
        "source_observation_sha256",
        "source_topology_sha256",
        "source_config_sha256",
        "source_firewall_sha256",
        "fork_name_sha256",
        "create_authority_sha256",
        "cleanup_authority_sha256",
    ):
        common.require_sha256(target_value[key], f"Valkey fork delete target {key}")
    recovery_identity = target_value["recovery_identity_sha256"]
    if recovery_identity is not None:
        common.require_sha256(
            recovery_identity, "Valkey fork delete recovery identity hash"
        )
    create_authority = common.exact_string(
        target_value["create_authority"], "Valkey fork delete create authority"
    )
    receipt_cleanup_mode = common.exact_string(
        target_value["cleanup_mode"], "Valkey fork delete cleanup mode"
    )
    if (
        any(
            target_value[key] != create_authority_value["target"][key]
            for key in CREATE_TARGET_KEYS
        )
        or create_authority != expected_create_kind
        or target_value["create_authority_sha256"] != expected_create_sha
        or target_value["fork_name_sha256"] != deterministic_name_hash
        or recovery_identity != expected_recovery_identity
        or receipt_cleanup_mode != checked_cleanup_mode
        or target_value["cleanup_authority_sha256"] != cleanup_authority_sha256
        or (
            checked_cleanup_mode
            in {"terminal", "never-started", "pre-mutation-failure", "no-mutation"}
        ) != (create_authority == "create-receipt")
    ):
        _fail("Valkey fork delete target authority differs")
    result = common.exact_keys(
        receipt["result"], DELETE_RESULT_KEYS, "Valkey fork delete result"
    )
    attempts = common.exact_int(
        result["deletion_request_attempt_count"],
        "Valkey fork deletion request count",
        0,
        1,
    )
    ambiguous = common.exact_bool(
        result["mutation_ambiguous_reconciled"],
        "Valkey fork deletion ambiguity",
    )
    stable_reads = common.exact_int(
        result["stable_absence_read_count"],
        "Valkey fork stable absence read count",
        2,
        2,
    )
    source_stable_reads = common.exact_int(
        result["source_stable_read_count"],
        "Valkey source stable read count",
        2,
        2,
    )
    expected_result = {
        "already-absent": (0, False),
        "deleted": (1, False),
        "ambiguous-reconciled": (1, True),
    }
    outcome = common.exact_string(result["outcome"], "Valkey fork delete outcome")
    if (
        outcome not in expected_result
        or expected_result[outcome] != (attempts, ambiguous)
        or stable_reads != 2
        or source_stable_reads != 2
        or result["fork_absent"] is not True
    ):
        _fail("Valkey fork delete result differs")
    provider = common.exact_keys(
        receipt["provider"], PROVIDER_KEYS, "Valkey fork delete provider ledger"
    )
    labels = provider["endpoint_labels"]
    if type(labels) is not list or not all(type(item) is str for item in labels):
        _fail("Valkey fork delete provider ledger differs")
    expected_methods = ["GET"] if attempts == 0 else ["GET", "DELETE"]
    expected_prefix = [
        "valkey-cluster-pre-delete",
        "valkey-config-pre-delete",
        "valkey-source-firewall-pre-delete",
        "valkey-recovery-discovery",
    ]
    expected_suffix = [
        "valkey-cluster-cleanup-1",
        "valkey-config-cleanup-1",
        "valkey-source-firewall-cleanup-1",
        "valkey-recovery-cleanup-absence-1",
        "valkey-cluster-cleanup-2",
        "valkey-config-cleanup-2",
        "valkey-source-firewall-cleanup-2",
        "valkey-recovery-cleanup-absence-2",
    ]
    if attempts == 0:
        labels_valid = labels == expected_prefix + expected_suffix
    else:
        poll_labels = labels[len(expected_prefix) + 1 : -len(expected_suffix)]
        labels_valid = (
            len(labels)
            >= len(expected_prefix) + 1 + 1 + len(expected_suffix)
            and labels[: len(expected_prefix)] == expected_prefix
            and labels[len(expected_prefix)] == "delete-valkey-recovery-fork"
            and labels[-len(expected_suffix) :] == expected_suffix
            and 1 <= len(poll_labels) <= MAX_PROVIDER_POLLS
            and all(
                label == "valkey-recovery-discovery" for label in poll_labels
            )
        )
    if (
        provider["http_methods_used"] != expected_methods
        or common.exact_int(
            provider["mutation_request_count"],
            "Valkey fork delete mutation count",
            0,
            1,
        )
        != attempts
        or common.exact_int(
            provider["http_request_count"],
            "Valkey fork delete HTTP request count",
            len(expected_prefix) + len(expected_suffix),
            len(expected_prefix) + 1 + MAX_PROVIDER_POLLS + len(expected_suffix),
        )
        != len(labels)
        or not labels_valid
    ):
        _fail("Valkey fork delete provider ledger differs")
    gates = common.exact_keys(
        receipt["gates"], DELETE_GATE_KEYS, "Valkey fork delete gates"
    )
    if any(gates[key] is not True for key in DELETE_GATE_KEYS):
        _fail("Valkey fork delete gates are incomplete")
    exact_hash = common.require_sha256(exact_sha256, "Valkey fork delete receipt hash")
    if common.sha256_bytes(common.canonical_file_bytes(receipt)) != exact_hash:
        _fail("Valkey fork delete receipt exact-file hash differs")
    common.sanitize_public(receipt)
    return receipt


def validate_delete_receipt_live(
    value: Any,
    *,
    exact_sha256: str,
    target: Mapping[str, Any],
    control: Mapping[str, Any],
    phase: str,
    contract: Mapping[str, Any],
    contract_file_sha256: str,
    now: dt.datetime,
    cleanup_mode: str,
    cleanup_evidence: Mapping[str, Any],
    read_transport: Transport,
    create_receipt: Mapping[str, Any] | None = None,
    create_receipt_sha256: str | None = None,
    create_intent: Mapping[str, Any] | None = None,
    create_intent_sha256: str | None = None,
    sleeper: Callable[[float], None] = lambda _: None,
) -> dict[str, Any]:
    """Pre-sign gate: independently prove receipt-bound fork absence twice."""

    receipt = validate_delete_receipt(
        value,
        exact_sha256=exact_sha256,
        target=target,
        control=control,
        phase=phase,
        now=now,
        cleanup_mode=cleanup_mode,
        cleanup_evidence=cleanup_evidence,
        create_receipt=create_receipt,
        create_receipt_sha256=create_receipt_sha256,
        create_intent=create_intent,
        create_intent_sha256=create_intent_sha256,
    )
    checked_target = validate_target_descriptor(dict(target))
    checked_control = validate_control(
        dict(control), workflow_path=CLEANUP_WORKFLOW_PATH
    )
    binding = contract_binding(
        contract, checked_control["contract_sha256"], contract_file_sha256
    )
    authority = create_receipt if create_receipt is not None else create_intent
    if type(authority) is not dict:
        _fail("cleanup live gate create authority is unavailable")
    prepare_control = validate_control(
        authority["control"], workflow_path=PREPARE_WORKFLOW_PATH
    )
    fork_name = deterministic_fork_name(
        common.validate_phase(phase, "recovery phase"), prepare_control
    )
    expected_recovery_identity = receipt["target"]["recovery_identity_sha256"]
    expected_source = {
        key: receipt["target"][key] for key in CREATE_TARGET_KEYS
    }
    session = ProviderSession(read_transport)
    for index in range(2):
        source = _source_bundle(
            session,
            checked_target,
            binding,
            suffix=f"-cleanup-gate-{index + 1}",
        )
        if _source_authority_projection(checked_target, source) != expected_source:
            _fail("production Valkey source differs at cleanup signing gate")
        inventory = _list_inventory(
            session, label=f"valkey-recovery-cleanup-absence-{index + 1}"
        )
        if any(database["name"] == fork_name for database in inventory):
            _fail("receipt-bound Valkey recovery fork remains at cleanup gate")
        if expected_recovery_identity is not None and any(
            database["safe"]["identity_sha256"] == expected_recovery_identity
            for database in inventory
        ):
            _fail("receipt-bound Valkey recovery identity survives under another name")
        if index == 0:
            sleeper(2)
    if session.ledger != [
        ("GET", "valkey-cluster-cleanup-gate-1"),
        ("GET", "valkey-config-cleanup-gate-1"),
        ("GET", "valkey-source-firewall-cleanup-gate-1"),
        ("GET", "valkey-recovery-cleanup-absence-1"),
        ("GET", "valkey-cluster-cleanup-gate-2"),
        ("GET", "valkey-config-cleanup-gate-2"),
        ("GET", "valkey-source-firewall-cleanup-gate-2"),
        ("GET", "valkey-recovery-cleanup-absence-2"),
    ]:
        _fail("cleanup live gate provider ledger differs")
    return receipt


def _load_canonical(path: Path, label: str) -> Any:
    return common.load_json(path, label, canonical=True)


def _load_contract(path: Path) -> tuple[dict[str, Any], str]:
    if path.is_symlink() or not path.is_file():
        _fail("production contract is not a regular file")
    raw = path.read_bytes()
    if not raw or len(raw) > common.MAX_JSON_BYTES:
        _fail("production contract has an invalid size")
    value = common.loads_strict(raw)
    if type(value) is not dict:
        _fail("production contract is malformed")
    return value, common.sha256_bytes(raw)


def _token(name: str) -> str:
    value = os.environ.pop(name, "")
    if not value:
        _fail(f"{name} is unavailable")
    return value


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    for name in (
        "prepare-intent",
        "create-or-reconcile",
        "validate-create-receipt",
        "observe-readiness",
        "delete-or-reconcile",
        "validate-delete-receipt",
    ):
        item = commands.add_parser(name)
        item.add_argument("--target", required=True)
        item.add_argument("--control", required=True)
        item.add_argument("--phase", required=True, choices=common.PHASES)
        item.add_argument("--output", required=True)
        item.add_argument("--runner-temp", required=True)
        if name in {
            "prepare-intent",
            "create-or-reconcile",
            "validate-create-receipt",
        }:
            item.add_argument("--contract", required=True)
            if name == "create-or-reconcile":
                item.add_argument("--intent", required=True)
                item.add_argument("--intent-sha256", required=True)
            elif name == "validate-create-receipt":
                item.add_argument("--intent", required=True)
                item.add_argument("--intent-sha256", required=True)
                item.add_argument("--receipt", required=True)
                item.add_argument("--receipt-sha256", required=True)
        elif name == "observe-readiness":
            item.add_argument("--receipt", required=True)
            item.add_argument("--receipt-sha256", required=True)
            item.add_argument("--contract", required=True)
        elif name == "validate-delete-receipt":
            item.add_argument("--contract", required=True)
            item.add_argument("--delete-receipt", required=True)
            item.add_argument("--delete-receipt-sha256", required=True)
            authority = item.add_mutually_exclusive_group(required=True)
            authority.add_argument("--create-receipt")
            authority.add_argument("--create-intent")
            item.add_argument("--create-receipt-sha256")
            item.add_argument("--create-intent-sha256")
            item.add_argument(
                "--cleanup-mode", required=True, choices=sorted(CLEANUP_MODES)
            )
            item.add_argument("--cleanup-evidence", required=True)
        else:
            item.add_argument("--contract", required=True)
            authority = item.add_mutually_exclusive_group(required=True)
            authority.add_argument("--receipt")
            authority.add_argument("--intent")
            item.add_argument("--receipt-sha256")
            item.add_argument("--intent-sha256")
            item.add_argument(
                "--cleanup-mode", required=True, choices=sorted(CLEANUP_MODES)
            )
            item.add_argument("--cleanup-evidence", required=True)
    args = parser.parse_args(argv)
    target = _load_canonical(Path(args.target), "recovery target descriptor")
    control = _load_canonical(Path(args.control), "fork control")
    if (
        type(control) is not dict
        or common.require_sha256(
            control.get("controller_sha256"), "fork controller hash"
        )
        != common.sha256_bytes(Path(__file__).read_bytes())
    ):
        _fail("fork controller exact-file hash differs")
    output = Path(args.output)
    runner_temp = Path(args.runner_temp)
    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    contract_value: dict[str, Any] | None = None
    contract_sha256: str | None = None
    if args.command in {
        "prepare-intent",
        "create-or-reconcile",
        "validate-create-receipt",
        "observe-readiness",
        "delete-or-reconcile",
        "validate-delete-receipt",
    }:
        contract_value, contract_sha256 = _load_contract(Path(args.contract))
    transports: list[DigitalOceanTransport] = []
    try:
        read_transport = DigitalOceanTransport(
            _token("DO_PRODUCTION_DATABASE_READ_TOKEN")
        )
        transports.append(read_transport)
        mutation_transport: DigitalOceanTransport | None = None
        if args.command == "create-or-reconcile":
            mutation_transport = DigitalOceanTransport(
                _token("DO_PRODUCTION_DATABASE_CREATE_TOKEN")
            )
            transports.append(mutation_transport)
        elif args.command == "delete-or-reconcile":
            mutation_transport = DigitalOceanTransport(
                _token("DO_PRODUCTION_DATABASE_DELETE_TOKEN")
            )
            transports.append(mutation_transport)
        if args.command == "prepare-intent":
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            value = build_create_intent(
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                transport=read_transport,
                now=now,
            )
        elif args.command == "create-or-reconcile":
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            if mutation_transport is None:
                _fail("provider create capability is unavailable")
            value = create_or_reconcile(
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                create_intent=_load_canonical(Path(args.intent), "fork create intent"),
                create_intent_sha256=args.intent_sha256,
                read_transport=read_transport,
                mutation_transport=mutation_transport,
                now=now,
                sleeper=lambda seconds: __import__("time").sleep(seconds),
            )
        elif args.command == "validate-create-receipt":
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            value = validate_create_receipt_live(
                _load_canonical(Path(args.receipt), "fork create receipt"),
                exact_sha256=args.receipt_sha256,
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                create_intent=_load_canonical(
                    Path(args.intent), "fork create intent"
                ),
                create_intent_sha256=args.intent_sha256,
                read_transport=read_transport,
                now=now,
            )
        elif args.command == "observe-readiness":
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            value = observe_readiness(
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                create_receipt=_load_canonical(Path(args.receipt), "fork create receipt"),
                create_receipt_sha256=args.receipt_sha256,
                transport=read_transport,
                now=now,
                sleeper=lambda seconds: __import__("time").sleep(seconds),
            )
        elif args.command == "validate-delete-receipt":
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            value = validate_delete_receipt_live(
                _load_canonical(
                    Path(args.delete_receipt), "fork delete receipt"
                ),
                exact_sha256=args.delete_receipt_sha256,
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                now=now,
                cleanup_mode=args.cleanup_mode,
                cleanup_evidence=_load_canonical(
                    Path(args.cleanup_evidence), "fork cleanup evidence"
                ),
                read_transport=read_transport,
                create_receipt=(
                    _load_canonical(
                        Path(args.create_receipt), "fork create receipt"
                    )
                    if args.create_receipt
                    else None
                ),
                create_receipt_sha256=args.create_receipt_sha256,
                create_intent=(
                    _load_canonical(
                        Path(args.create_intent), "fork create intent"
                    )
                    if args.create_intent
                    else None
                ),
                create_intent_sha256=args.create_intent_sha256,
                sleeper=lambda seconds: __import__("time").sleep(seconds),
            )
        else:
            if contract_value is None or contract_sha256 is None:
                _fail("production contract was not loaded")
            if mutation_transport is None:
                _fail("provider delete capability is unavailable")
            value = delete_or_reconcile(
                target=target,
                control=control,
                phase=args.phase,
                contract=contract_value,
                contract_file_sha256=contract_sha256,
                create_receipt=(
                    _load_canonical(Path(args.receipt), "fork create receipt")
                    if args.receipt
                    else None
                ),
                create_receipt_sha256=args.receipt_sha256,
                create_intent=(
                    _load_canonical(Path(args.intent), "fork create intent")
                    if args.intent
                    else None
                ),
                create_intent_sha256=args.intent_sha256,
                cleanup_mode=args.cleanup_mode,
                cleanup_evidence=_load_canonical(
                    Path(args.cleanup_evidence), "fork cleanup evidence"
                ),
                read_transport=read_transport,
                mutation_transport=mutation_transport,
                now=now,
                sleeper=lambda seconds: __import__("time").sleep(seconds),
            )
        common.write_canonical_output(output, value, runner_temp)
    finally:
        for transport in transports:
            transport.scrub()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
