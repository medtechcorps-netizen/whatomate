#!/usr/bin/env python3
"""Apply one exact ReReply production phase with a single DigitalOcean PUT."""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import http.client
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_release as common
import observe_production_recovery as recovery_control


WORKFLOW_PATH = ".github/workflows/apply-production-phase.yml"
AUTHORITY = "production-phase-apply-receipt"
ENDPOINT_LABELS = [
    "app-health", "app-ready", "meta-live", "meta-ready", "gmail-live", "gmail-ready",
]
POLL_LIMIT = 90
POLL_SECONDS = 10


def _response_status(response: Any) -> int:
    status = getattr(response, "status", None)
    return status if status is not None else response.getcode()


class ProductionAppClient:
    """Capability-limited Apps client: explicit GETs and one exact PUT only."""

    def __init__(self, app_id: str, token: str, *, opener: Any | None = None) -> None:
        self.app_id = common.require_uuid(app_id, "production app identity")
        if type(token) is not str or len(token) < 20 or any(ch in token for ch in "\r\n\x00"):
            common.fail("production apply token is invalid")
        self._token = token
        self._opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), common.RejectRedirects()
        )
        self.request_log: list[tuple[str, str]] = []
        self.mutation_attempted = False
        self.mutation_ambiguous = False

    @property
    def app_path(self) -> str:
        return f"/v2/apps/{self.app_id}"

    def deployment_path(self, deployment_id: str) -> str:
        return f"/v2/apps/{self.app_id}/deployments/{common.require_uuid(deployment_id, 'deployment identity')}"

    def _url(self, path: str) -> str:
        if path != self.app_path and re_full_deployment_path(path, self.app_id) is False:
            common.fail("provider path is outside the exact app allowlist")
        url = common.API_ORIGIN + path
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "https" or parsed.hostname != "api.digitalocean.com"
            or parsed.port not in (None, 443) or parsed.username is not None
            or parsed.password is not None or parsed.query or parsed.fragment
        ):
            common.fail("provider URL is outside the exact HTTPS origin")
        return url

    def _decode(self, response: Any, url: str, expected: set[int]) -> Any:
        if response.geturl() != url:
            common.fail("provider response URL differs from the exact request")
        if _response_status(response) not in expected:
            common.fail("provider returned an unexpected status")
        content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
        if content_type != "application/json":
            common.fail("provider response content type differs")
        raw = response.read(common.MAX_JSON_BYTES + 1)
        if not raw or len(raw) > common.MAX_JSON_BYTES:
            common.fail("provider response size differs")
        return common.loads_strict(raw)

    def _get(self, path: str, label: str) -> Any:
        url = self._url(path)
        request = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._token}",
                "User-Agent": "rereply-production-apply/1",
            },
        )
        try:
            with self._opener.open(request, timeout=20) as response:
                value = self._decode(response, url, {200})
        except common.ReleaseError:
            raise
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError) as exc:
            raise common.ReleaseError("provider GET failed") from exc
        self.request_log.append(("GET", label))
        return value

    def get_app(self) -> Any:
        return self._get(self.app_path, "app")

    def get_deployment(self, deployment_id: str) -> Any:
        return self._get(self.deployment_path(deployment_id), "deployment")

    def put_app_once(self, spec: Mapping[str, Any]) -> Any:
        if self.mutation_attempted:
            common.fail("a second production mutation was blocked")
        self.mutation_attempted = True
        body = {"spec": copy.deepcopy(spec), "update_all_source_versions": False}
        raw = common.canonical_payload_bytes(body)
        url = self._url(self.app_path)
        request = urllib.request.Request(
            url,
            data=raw,
            method="PUT",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._token}",
                "Content-Type": "application/json",
                "Content-Length": str(len(raw)),
                "User-Agent": "rereply-production-apply/1",
            },
        )
        self.request_log.append(("PUT", "app"))
        try:
            with self._opener.open(request, timeout=30) as response:
                try:
                    return self._decode(response, url, {200})
                except common.ReleaseError as exc:
                    self.mutation_ambiguous = True
                    raise common.AmbiguousMutation(
                        "provider update response was not authoritative"
                    ) from exc
        except urllib.error.HTTPError as exc:
            # Only responses that prove the provider rejected the request before
            # application may be treated as definitive. A 408 is explicitly
            # ambiguous after bytes have been sent and must be reconciled with
            # GETs; the PUT is never repeated.
            if exc.code in {400, 401, 403, 404, 405, 409, 415, 422}:
                raise common.ReleaseError("provider rejected the exact app update") from exc
            self.mutation_ambiguous = True
            raise common.AmbiguousMutation("provider update outcome is ambiguous") from exc
        except common.AmbiguousMutation:
            raise
        except common.ReleaseError as exc:
            self.mutation_ambiguous = True
            raise common.AmbiguousMutation(
                "provider update outcome is ambiguous"
            ) from exc
        except (urllib.error.URLError, http.client.HTTPException, TimeoutError, OSError) as exc:
            self.mutation_ambiguous = True
            raise common.AmbiguousMutation("provider update outcome is ambiguous") from exc

    def scrub(self) -> None:
        self._token = ""


def re_full_deployment_path(path: str, app_id: str) -> bool:
    prefix = f"/v2/apps/{app_id}/deployments/"
    return path.startswith(prefix) and common.UUID_RE.fullmatch(path[len(prefix):]) is not None


def _app_object(value: Any) -> dict[str, Any]:
    if type(value) is not dict or type(value.get("app")) is not dict:
        common.fail("provider app response is malformed")
    return value["app"]


def _deployment_object(value: Any) -> dict[str, Any]:
    if type(value) is not dict or type(value.get("deployment")) is not dict:
        common.fail("provider deployment response is malformed")
    return value["deployment"]


def _deployment_id(value: Any, label: str) -> str | None:
    if value is None:
        return None
    if type(value) is not dict:
        common.fail(f"{label} is malformed")
    return common.require_uuid(value.get("id"), f"{label} identity")


def _active_id(app: Mapping[str, Any]) -> str:
    active = app.get("active_deployment")
    if type(active) is not dict or active.get("phase") != "ACTIVE":
        common.fail("production active deployment is not stable")
    return common.require_uuid(active.get("id"), "active deployment identity")


def _no_transition(app: Mapping[str, Any]) -> None:
    for key in ("in_progress_deployment", "pending_deployment", "pinned_deployment"):
        if app.get(key) is not None:
            common.fail("another production deployment is pending")


def provider_snapshot(
    app_response: Any,
    deployment_response: Any,
    app_id: str,
    *,
    require_stable: bool = True,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    app = _app_object(app_response)
    deployment = _deployment_object(deployment_response)
    if common.require_uuid(app.get("id"), "app identity") != app_id:
        common.fail("production app identity differs")
    active_id = _active_id(app)
    if common.require_uuid(deployment.get("id"), "deployment identity") != active_id:
        common.fail("active deployment identity differs")
    if deployment.get("phase") != "ACTIVE":
        common.fail("active deployment is not ACTIVE")
    if require_stable:
        _no_transition(app)
    live_spec = app.get("spec")
    active_spec = deployment.get("spec")
    if type(live_spec) is not dict or type(active_spec) is not dict or live_spec != active_spec:
        common.fail("live and active production specs differ")
    mode = common.source_mode(live_spec)
    images = common.sanitized_image_records(common.extract_image_digests(live_spec)) if mode == "digest-images" else []
    updated_at = common.exact_string(app.get("updated_at"), "app updated_at")
    ingress = common.exact_string(app.get("default_ingress"), "app default ingress")
    public = {
        "app_identity_sha256": common.sha256_bytes(app_id.encode("utf-8")),
        "default_ingress_sha256": common.sha256_bytes(ingress.encode("utf-8")),
        "app_updated_at_sha256": common.sha256_bytes(updated_at.encode("utf-8")),
        "active_deployment_identity_sha256": common.sha256_bytes(active_id.encode("utf-8")),
        "canonical_spec_sha256": common.sha256_value(live_spec),
        "environment_values_sha256": common.environment_value_fingerprint(live_spec),
        "non_source_projection_sha256": common.non_source_fingerprint(live_spec),
        "source_mode": mode,
        "images": images,
    }
    return public, live_spec, deployment


def observe_stable(client: ProductionAppClient) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    first_app = client.get_app()
    first_id = _active_id(_app_object(first_app))
    first_deployment = client.get_deployment(first_id)
    second_app = client.get_app()
    second_id = _active_id(_app_object(second_app))
    second_deployment = client.get_deployment(second_id)
    first = provider_snapshot(first_app, first_deployment, client.app_id)
    second = provider_snapshot(second_app, second_deployment, client.app_id)
    if first != second or first_id != second_id:
        common.fail("production changed between the two exact reads")
    return first


def _deployment_candidate(app: Mapping[str, Any], desired_spec: Mapping[str, Any]) -> str | None:
    candidates: list[str] = []
    for key in ("in_progress_deployment", "pending_deployment", "active_deployment"):
        value = app.get(key)
        if type(value) is not dict:
            continue
        candidate_spec = value.get("spec")
        if candidate_spec == desired_spec:
            candidates.append(common.require_uuid(value.get("id"), f"{key} identity"))
        elif key == "active_deployment" and app.get("spec") == desired_spec:
            candidates.append(common.require_uuid(value.get("id"), "active deployment identity"))
    unique = set(candidates)
    if len(unique) > 1:
        common.fail("multiple candidate deployments match the exact mutation")
    return next(iter(unique)) if unique else None


def _migration_succeeded(deployment: Mapping[str, Any]) -> bool:
    jobs = deployment.get("jobs")
    if type(jobs) is not list:
        common.fail("deployment job inventory is malformed")
    matches = [item for item in jobs if type(item) is dict and item.get("name") == "rereply-rls-migrate"]
    if len(matches) != 1 or matches[0].get("phase") != "SUCCEEDED":
        common.fail("production migration job did not succeed exactly once")
    return True


def reconcile_until_active(
    client: ProductionAppClient,
    desired_spec: Mapping[str, Any],
    *,
    sleeper: Callable[[float], None] = time.sleep,
    poll_limit: int = POLL_LIMIT,
) -> tuple[dict[str, Any], dict[str, Any], bool]:
    candidate_id: str | None = None
    for attempt in range(poll_limit):
        app_response = client.get_app()
        app = _app_object(app_response)
        observed = _deployment_candidate(app, desired_spec)
        if observed is not None:
            if candidate_id is not None and candidate_id != observed:
                common.fail("candidate deployment identity changed during reconciliation")
            candidate_id = observed
        if candidate_id is not None:
            deployment_response = client.get_deployment(candidate_id)
            deployment = _deployment_object(deployment_response)
            phase = deployment.get("phase")
            if phase in {"ERROR", "CANCELED"}:
                common.fail("production deployment reached a terminal failure")
            active = app.get("active_deployment")
            if (
                phase == "ACTIVE" and type(active) is dict
                and active.get("id") == candidate_id and app.get("spec") == desired_spec
                and deployment.get("spec") == desired_spec
            ):
                _no_transition(app)
                _migration_succeeded(deployment)
                return app_response, deployment_response, client.mutation_ambiguous
        if attempt + 1 < poll_limit:
            sleeper(POLL_SECONDS)
    common.fail("production update could not be reconciled before the deadline")


def validate_recovery(
    value: Any,
    expected_sha256: str,
    now: dt.datetime,
    *,
    contract_path: Path | None = None,
    expected_control: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    recovery = common.exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "issued_at", "expires_at",
            "control", "authorities", "target", "postgresql", "valkey", "provider",
            "gates",
        },
        "recovery readiness",
    )
    if (
        recovery["schema_version"] != 2
        or recovery["authority"] != "production-recovery-readiness"
        or recovery["repository"] != common.REPOSITORY
    ):
        common.fail("recovery readiness authority differs")
    control = common.exact_keys(
        recovery["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "contract_sha256", "controller_sha256",
        },
        "recovery readiness control",
    )
    common.require_sha1(control["workflow_sha"], "recovery workflow SHA")
    common.require_run_id(control["run_id"], "recovery run ID")
    common.exact_int(control["run_attempt"], "recovery run attempt", 1, 1)
    if (
        control["workflow_path"]
        != ".github/workflows/verify-production-recovery-readiness.yml"
        or control["runner_environment"] != "github-hosted"
    ):
        common.fail("recovery workflow identity differs")
    common.require_sha256(control["contract_sha256"], "recovery contract hash")
    common.require_sha256(control["controller_sha256"], "recovery controller hash")
    if expected_control is not None:
        checked_expected_control = common.exact_keys(
            expected_control,
            {
                "workflow_sha", "workflow_path", "run_id", "run_attempt",
                "runner_environment", "contract_sha256", "controller_sha256",
            },
            "expected recovery readiness control",
        )
        common.require_sha1(
            checked_expected_control["workflow_sha"],
            "expected recovery workflow SHA",
        )
        common.require_run_id(
            checked_expected_control["run_id"], "expected recovery run ID"
        )
        common.exact_int(
            checked_expected_control["run_attempt"],
            "expected recovery run attempt",
            1,
            1,
        )
        for key in ("contract_sha256", "controller_sha256"):
            common.require_sha256(
                checked_expected_control[key], f"expected recovery {key}"
            )
        if (
            checked_expected_control["workflow_path"]
            != ".github/workflows/verify-production-recovery-readiness.yml"
            or checked_expected_control["runner_environment"] != "github-hosted"
            or control != checked_expected_control
        ):
            common.fail("recovery readiness control differs from signing authority")
    recovery_controller_path = Path(__file__).resolve().with_name(
        "observe_production_recovery.py"
    )
    if control["controller_sha256"] != common.sha256_bytes(
        recovery_controller_path.read_bytes()
    ):
        common.fail("recovery evidence is not bound to the current recovery controller")
    contract_path = contract_path or Path(__file__).resolve().with_name("production-app-contract.json")
    contract_raw = contract_path.read_bytes()
    try:
        contract = common.loads_strict(contract_raw.decode("utf-8"))
    except UnicodeError as exc:
        raise common.ReleaseError("production app contract is malformed") from exc
    contract_sha256 = common.sha256_bytes(contract_raw)
    if control["contract_sha256"] != contract_sha256:
        common.fail("recovery evidence is not bound to the current production contract")
    contract_databases = recovery_control.contract_database_bindings(contract)
    common.validate_fresh_window(
        recovery["issued_at"], recovery["expires_at"], now,
        maximum_age_seconds=common.MAX_RECOVERY_AGE_SECONDS,
        label="recovery readiness",
    )
    authorities = common.exact_keys(
        recovery["authorities"], {"production_plan", "valkey_fork"},
        "recovery readiness authorities",
    )
    production_plan = common.exact_keys(
        authorities["production_plan"], {"run_id", "run_attempt", "sha256"},
        "recovery production plan authority",
    )
    production_plan = {
        "run_id": common.require_run_id(
            production_plan["run_id"], "recovery production plan run ID"
        ),
        "run_attempt": common.exact_int(
            production_plan["run_attempt"], "recovery production plan run attempt", 1, 1
        ),
        "sha256": common.require_sha256(
            production_plan["sha256"], "recovery production plan hash"
        ),
    }
    valkey_fork_authority = common.exact_keys(
        authorities["valkey_fork"],
        {"run_id", "run_attempt", "sha256", "request_sha256", "receipt_sha256"},
        "recovery Valkey fork authority",
    )
    valkey_fork_authority = {
        "run_id": common.require_run_id(
            valkey_fork_authority["run_id"], "Valkey fork run ID"
        ),
        "run_attempt": common.exact_int(
            valkey_fork_authority["run_attempt"], "Valkey fork run attempt", 1, 1
        ),
        "sha256": common.require_sha256(
            valkey_fork_authority["sha256"], "Valkey fork exact-file hash"
        ),
        "request_sha256": common.require_sha256(
            valkey_fork_authority["request_sha256"], "Valkey fork request hash"
        ),
        "receipt_sha256": common.require_sha256(
            valkey_fork_authority["receipt_sha256"], "Valkey fork receipt hash"
        ),
    }
    target = common.exact_keys(
        recovery["target"],
        {
            "descriptor_sha256", "contract_sha256", "postgresql_identity_sha256",
            "valkey_identity_sha256", "valkey_recovery_identity_sha256",
            "identity_projection_sha256", "postgresql_cluster_sha256",
            "valkey_cluster_sha256", "region_sha256",
        },
        "recovery target authority",
    )
    for key in target:
        common.require_sha256(target[key], f"recovery target {key}")
    if (
        target["contract_sha256"] != contract_sha256
        or target["postgresql_cluster_sha256"] != contract_databases["postgresql_cluster_sha256"]
        or target["valkey_cluster_sha256"] != contract_databases["valkey_cluster_sha256"]
        or target["region_sha256"] != contract_databases["region_sha256"]
    ):
        common.fail("recovery database contract binding differs")
    expected_projection = common.sha256_value(
        {
            "postgresql_identity_sha256": target["postgresql_identity_sha256"],
            "valkey_identity_sha256": target["valkey_identity_sha256"],
            "valkey_recovery_identity_sha256": target["valkey_recovery_identity_sha256"],
        }
    )
    if (
        target["identity_projection_sha256"] != expected_projection
        or target["descriptor_sha256"] != expected_projection
    ):
        common.fail("recovery database identity projection differs")
    postgresql = common.exact_keys(
        recovery["postgresql"],
        {
            "identity_sha256", "observation_sha256", "status", "engine", "version",
            "region_sha256", "fresh_backup", "backup_identity_sha256",
            "backup_inventory_sha256", "point_in_time_restore_ready",
            "production_cluster_sha256",
        },
        "PostgreSQL recovery evidence",
    )
    for key in ("identity_sha256", "observation_sha256", "region_sha256", "backup_identity_sha256", "backup_inventory_sha256", "production_cluster_sha256"):
        common.require_sha256(postgresql[key], f"PostgreSQL recovery {key}")
    if (
        postgresql["identity_sha256"] != target["postgresql_identity_sha256"]
        or postgresql["production_cluster_sha256"] != target["postgresql_cluster_sha256"]
        or postgresql["region_sha256"] != target["region_sha256"]
        or postgresql["status"] != "online"
        or postgresql["engine"] != "postgresql"
        or postgresql["version"] != contract_databases["postgresql_version"]
        or postgresql["fresh_backup"] is not True
        or postgresql["point_in_time_restore_ready"] is not True
    ):
        common.fail("PostgreSQL recovery binding differs")
    common.exact_string(postgresql["version"], "PostgreSQL recovery version")
    valkey = common.exact_keys(
        recovery["valkey"],
        {
            "identity_sha256", "recovery_identity_sha256", "source_observation_sha256",
            "recovery_observation_sha256", "status", "recovery_status", "version",
            "recovery_version", "region_sha256", "recovery_region_sha256",
            "source_topology_sha256", "recovery_topology_sha256", "persistence",
            "recovery_persistence", "recovery_is_distinct", "recovery_is_fresh",
            "topology_equal", "production_cluster_sha256", "provider_fork",
        },
        "Valkey recovery evidence",
    )
    for key in (
        "identity_sha256", "recovery_identity_sha256", "source_observation_sha256",
        "recovery_observation_sha256", "production_cluster_sha256", "region_sha256",
        "recovery_region_sha256", "source_topology_sha256", "recovery_topology_sha256",
    ):
        common.require_sha256(valkey[key], f"Valkey recovery {key}")
    if (
        valkey["identity_sha256"] != target["valkey_identity_sha256"]
        or valkey["recovery_identity_sha256"] != target["valkey_recovery_identity_sha256"]
        or valkey["production_cluster_sha256"] != target["valkey_cluster_sha256"]
        or valkey["status"] != "online"
        or valkey["recovery_status"] != "online"
        or valkey["version"] != contract_databases["valkey_version"]
        or valkey["recovery_version"] != contract_databases["valkey_version"]
        or valkey["identity_sha256"] == valkey["recovery_identity_sha256"]
        or valkey["region_sha256"] != target["region_sha256"]
        or valkey["recovery_region_sha256"] != target["region_sha256"]
        or valkey["source_topology_sha256"] != valkey["recovery_topology_sha256"]
        or valkey["persistence"] != "rdb"
        or valkey["recovery_persistence"] != "rdb"
        or any(valkey[key] is not True for key in ("recovery_is_distinct", "recovery_is_fresh", "topology_equal"))
    ):
        common.fail("Valkey recovery binding differs")
    provider_fork = common.exact_keys(
        valkey["provider_fork"],
        {
            "authority", "source_identity_sha256", "recovery_identity_sha256",
            "request_sha256", "receipt_sha256", "source_config_sha256",
            "recovery_config_sha256", "source_firewall_sha256",
            "recovery_firewall_sha256", "fork_name_sha256",
            "fork_created_at_sha256", "provider_copy_contract", "stable_read_count",
            "request_attempt_count", "mutation_ambiguous_reconciled",
            "source_firewall_unchanged", "source_firewall_exact_app",
            "recovery_firewall_exact_source_app",
            "recovery_restricted_to_exact_production_app",
        },
        "Valkey provider fork proof",
    )
    for key in (
        "source_identity_sha256", "recovery_identity_sha256", "request_sha256",
        "receipt_sha256", "source_config_sha256", "recovery_config_sha256",
        "source_firewall_sha256", "recovery_firewall_sha256", "fork_name_sha256",
        "fork_created_at_sha256",
    ):
        common.require_sha256(provider_fork[key], f"Valkey provider fork {key}")
    if (
        provider_fork["authority"] != "production-valkey-recovery-fork-v2"
        or provider_fork["source_identity_sha256"] != valkey["identity_sha256"]
        or provider_fork["recovery_identity_sha256"]
        != valkey["recovery_identity_sha256"]
        or provider_fork["request_sha256"]
        != valkey_fork_authority["request_sha256"]
        or provider_fork["receipt_sha256"]
        != valkey_fork_authority["receipt_sha256"]
        or valkey_fork_authority["sha256"]
        != valkey_fork_authority["receipt_sha256"]
        or provider_fork["source_config_sha256"]
        != provider_fork["recovery_config_sha256"]
        or provider_fork["provider_copy_contract"]
        != "digitalocean-valkey-latest-transaction-data-and-configuration"
        or common.exact_int(
            provider_fork["stable_read_count"], "Valkey provider stable read count", 2, 2
        ) != 2
        or common.exact_int(
            provider_fork["request_attempt_count"], "Valkey fork request attempt count", 1, 1
        ) != 1
        or common.exact_bool(
            provider_fork["mutation_ambiguous_reconciled"],
            "Valkey fork ambiguity reconciliation",
        ) is not False
        or provider_fork["source_firewall_unchanged"] is not True
        or provider_fork["source_firewall_exact_app"] is not True
        or provider_fork["recovery_firewall_exact_source_app"] is not True
        or provider_fork["recovery_restricted_to_exact_production_app"] is not True
    ):
        common.fail("Valkey provider fork authority differs")
    provider = common.exact_keys(
        recovery["provider"],
        {"http_methods_used", "http_request_count", "http_endpoint_labels", "mutation_request_count"},
        "recovery provider ledger",
    )
    if recovery["gates"] != {
        "postgresql_ready": True,
        "valkey_ready": True,
        "double_read_equal": True,
        "mutation_free": True,
        "provider_fork_bound": True,
        "recovery_restricted_to_exact_production_app": True,
    }:
        common.fail("recovery readiness gates are incomplete")
    stable_round = [
        "postgres-cluster", "postgres-backups", "valkey-cluster", "valkey-config",
        "valkey-source-firewall", "valkey-recovery-cluster",
        "valkey-recovery-config", "valkey-recovery-firewall",
    ]
    endpoint_labels = ["valkey-recovery-discovery", *stable_round, *stable_round]
    if (
        provider["http_methods_used"] != ["GET"]
        or provider["mutation_request_count"] != 0
        or common.exact_int(
            provider["http_request_count"], "recovery provider request count", 17, 17
        ) != 17
        or provider["http_endpoint_labels"] != endpoint_labels
    ):
        common.fail("recovery readiness is not mutation-free")
    exact_hash = common.require_sha256(expected_sha256, "recovery exact-file hash")
    if common.sha256_bytes(common.canonical_file_bytes(recovery)) != exact_hash:
        common.fail("recovery exact-file hash differs")
    common.sanitize_public(recovery)
    return recovery


def _require_recovery_plan_authority(
    recovery: Mapping[str, Any], authority: Mapping[str, Any]
) -> None:
    supplied = common.exact_keys(
        recovery["authorities"]["production_plan"],
        {"run_id", "run_attempt", "sha256"},
        "recovery production plan authority",
    )
    actual = {
        "run_id": common.require_run_id(
            supplied["run_id"], "recovery production plan run ID"
        ),
        "run_attempt": common.exact_int(
            supplied["run_attempt"], "recovery production plan run attempt", 1, 1
        ),
        "sha256": common.require_sha256(
            supplied["sha256"], "recovery production plan hash"
        ),
    }
    expected = {
        "run_id": common.require_run_id(authority.get("run_id"), "production plan run ID"),
        "run_attempt": common.exact_int(
            authority.get("run_attempt"), "production plan run attempt", 1, 1
        ),
        "sha256": common.require_sha256(
            authority.get("sha256"), "production plan exact-file hash"
        ),
    }
    if actual != expected:
        common.fail("recovery production plan authority differs")


def _plan_images(plan: Mapping[str, Any], rollout: Mapping[str, Any]) -> tuple[str, str, str, dict[str, str]]:
    transition = common.exact_keys(plan.get("transition"), {"operation", "from", "to", "ordinal"}, "production plan transition")
    if transition["operation"] != "activate":
        common.fail("production plan operation differs")
    target = common.validate_phase(transition["to"], "target phase")
    source = common.exact_string(transition["from"], "source phase")
    if source != common.PREDECESSOR[target] or transition["ordinal"] != common.PHASES.index(target) + 1:
        common.fail("production activation sequence differs")
    target_object = plan.get("target")
    if type(target_object) is not dict or target_object.get("phase") != target:
        common.fail("production plan target differs")
    source_sha = common.require_sha1(target_object.get("source_sha"), "target source SHA")
    digests = common.phase_images_from_rollout(rollout, target)
    expected_records = [
        {
            "component": component,
            "repository": f"ghcr.io/medtechcorps-netizen/rereply-release-{component}",
            "digest": digests[component],
            "subject": f"ghcr.io/medtechcorps-netizen/rereply-release-{component}@{digests[component]}",
        }
        for component in ("web", "meta-relay", "gmail-relay")
    ]
    if target_object.get("images") != expected_records:
        common.fail("production plan image authority differs")
    migration = target_object.get("migration")
    if type(migration) is not dict or migration.get("digest") != digests["web"]:
        common.fail("production plan migration binding differs")
    return source, target, source_sha, digests


def _match_plan_observation(plan: Mapping[str, Any], before: Mapping[str, Any]) -> None:
    observation = plan.get("provider_observation")
    if type(observation) is not dict:
        common.fail("production plan observation is malformed")
    mappings = {
        "app_identity_sha256": "app_identity_sha256",
        "default_ingress_sha256": "default_ingress_sha256",
        "app_updated_at_sha256": "app_updated_at_sha256",
        "active_deployment_identity_sha256": "active_deployment_identity_sha256",
        "live_canonical_spec_sha256": "canonical_spec_sha256",
        "environment_values_sha256": "environment_values_sha256",
        "non_source_projection_sha256": "non_source_projection_sha256",
    }
    for plan_key, state_key in mappings.items():
        if observation.get(plan_key) != before[state_key]:
            common.fail("live provider state differs from the signed plan")
    if observation.get("live_active_equal") is not True or observation.get("predecessor_match") is not True:
        common.fail("production plan equality proof is incomplete")


def _validate_predecessor(
    predecessor: Mapping[str, Any] | None,
    predecessor_sha256: str | None,
    source_phase: str,
    before: Mapping[str, Any],
    signed_predecessor_sha256: str,
) -> None:
    if source_phase == "genesis":
        if (
            predecessor is not None
            or predecessor_sha256 is not None
            or before["source_mode"] != "legacy-git"
        ):
            common.fail("baseline genesis state differs")
        common.require_sha256(signed_predecessor_sha256, "signed genesis state hash")
        return
    if predecessor is None or predecessor_sha256 is None:
        common.fail("production predecessor state is required")
    state = common.validate_phase_state(predecessor)
    if common.sha256_bytes(common.canonical_file_bytes(state)) != common.require_sha256(predecessor_sha256, "predecessor hash"):
        common.fail("predecessor exact-file hash differs")
    if state["lineage"]["phase"] != source_phase or not common.provider_states_share_semantic_lineage(
        state["provider_state"], before, allow_legacy=False
    ):
        common.fail("live provider state differs from the signed predecessor")


def _authority_binding(value: Any, label: str) -> dict[str, Any]:
    binding = common.exact_keys(value, {"run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"}, label)
    return {
        "run_id": common.require_run_id(binding["run_id"], f"{label} run ID"),
        "run_attempt": common.exact_int(binding["run_attempt"], f"{label} run attempt", 1, 1),
        "artifact_id": common.require_run_id(binding["artifact_id"], f"{label} artifact ID"),
        "artifact_digest": common.require_digest(binding["artifact_digest"], f"{label} artifact digest"),
        "sha256": common.require_sha256(binding["sha256"], f"{label} file hash"),
    }


def validate_evidence_request(value: Any) -> dict[str, Any]:
    evidence = common.exact_keys(
        value,
        {"production_plan", "recovery", "rollout_plan_sha256", "predecessor_state_sha256"},
        "apply evidence request",
    )
    predecessor = common.require_sha256(
        evidence["predecessor_state_sha256"], "requested predecessor state hash"
    )
    return {
        "production_plan": _authority_binding(evidence["production_plan"], "production plan authority"),
        "recovery": _authority_binding(evidence["recovery"], "recovery authority"),
        "rollout_plan_sha256": common.require_sha256(evidence["rollout_plan_sha256"], "rollout plan hash"),
        "predecessor_state_sha256": predecessor,
    }


def _validate_plan_control(
    plan: Mapping[str, Any],
    workflow_control: Mapping[str, Any],
    authority: Mapping[str, Any],
    release_policy_sha256: str,
    change_schema_sha256: str,
) -> None:
    control = common.exact_keys(
        plan.get("control"),
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "contract_sha256", "release_policy_sha256",
            "change_schema_sha256", "verifier_sha256",
        },
        "production plan control",
    )
    if (
        common.require_sha1(control["workflow_sha"], "plan workflow SHA")
        != common.require_sha1(workflow_control["workflow_sha"], "apply workflow SHA")
        or control["workflow_path"] != ".github/workflows/plan-production-rollout.yml"
        or control["runner_environment"] != "github-hosted"
        or common.require_run_id(control["run_id"], "plan run ID")
        != authority["run_id"]
        or common.exact_int(control["run_attempt"], "plan run attempt", 1, 1)
        != authority["run_attempt"]
        or common.require_sha256(
            control["release_policy_sha256"], "plan release policy hash"
        ) != common.require_sha256(release_policy_sha256, "apply release policy hash")
        or common.require_sha256(
            control["change_schema_sha256"], "plan change schema hash"
        ) != common.require_sha256(change_schema_sha256, "apply change schema hash")
    ):
        common.fail("production plan control authority differs")
    common.require_sha256(control["contract_sha256"], "plan contract hash")
    common.require_sha256(control["verifier_sha256"], "plan verifier hash")


def _clock_value(clock: Callable[[], dt.datetime]) -> dt.datetime:
    value = clock()
    if (
        not isinstance(value, dt.datetime)
        or value.tzinfo is None
        or value.utcoffset() is None
    ):
        common.fail("production mutation clock is invalid")
    return value.astimezone(dt.timezone.utc)


def require_fresh_immediately_before_mutation(
    *,
    recovery: Mapping[str, Any],
    clock: Callable[[], dt.datetime],
    plan: Mapping[str, Any] | None = None,
) -> dt.datetime:
    checked = _clock_value(clock)
    if plan is not None:
        common.validate_fresh_window(
            plan.get("issued_at"),
            plan.get("expires_at"),
            checked,
            maximum_age_seconds=common.MAX_PLAN_AGE_SECONDS,
            label="production plan immediately before mutation",
        )
    common.validate_fresh_window(
        recovery.get("issued_at"),
        recovery.get("expires_at"),
        checked,
        maximum_age_seconds=common.MAX_RECOVERY_AGE_SECONDS,
        label="recovery readiness immediately before mutation",
    )
    return checked


def _full_binding(value: Mapping[str, Any], artifact_name: str) -> dict[str, Any]:
    binding = dict(value)
    binding["artifact_name"] = artifact_name
    return common.validate_full_artifact_binding(binding, "mutation intent artifact authority")


def _plan_before_state(
    plan: Mapping[str, Any], predecessor: Mapping[str, Any] | None
) -> dict[str, Any]:
    observation = plan.get("provider_observation")
    if type(observation) is not dict:
        common.fail("production plan observation is malformed")
    mode = observation.get("source_mode")
    if mode == "legacy-git":
        images: list[dict[str, str]] = []
    elif mode == "digest-images":
        if predecessor is None:
            common.fail("digest-image mutation intent requires a predecessor")
        predecessor = common.validate_phase_state(predecessor)
        images = copy.deepcopy(predecessor["provider_state"]["images"])
    else:
        common.fail("production plan source mode differs")
    before = {
        "app_identity_sha256": observation.get("app_identity_sha256"),
        "default_ingress_sha256": observation.get("default_ingress_sha256"),
        "app_updated_at_sha256": observation.get("app_updated_at_sha256"),
        "active_deployment_identity_sha256": observation.get(
            "active_deployment_identity_sha256"
        ),
        "canonical_spec_sha256": observation.get("live_canonical_spec_sha256"),
        "environment_values_sha256": observation.get("environment_values_sha256"),
        "non_source_projection_sha256": observation.get(
            "non_source_projection_sha256"
        ),
        "source_mode": mode,
        "images": images,
    }
    return common._validate_public_provider_state(
        before, "planned production before state", allow_legacy=True
    )


def _desired_projection(
    *,
    canonical_spec_sha256: str,
    before: Mapping[str, Any],
    target_digests: Mapping[str, str],
) -> dict[str, Any]:
    desired = {
        "canonical_spec_sha256": common.require_sha256(
            canonical_spec_sha256, "desired candidate hash"
        ),
        "environment_values_sha256": before["environment_values_sha256"],
        "non_source_projection_sha256": before["non_source_projection_sha256"],
        "source_mode": "digest-images",
        "images": common.sanitized_image_records(target_digests),
        "migration_job": "rereply-rls-migrate",
        "migration_digest": common.require_digest(
            target_digests["web"], "desired migration image"
        ),
    }
    return common._validate_desired_projection(desired, "planned desired state")


def prepare_apply_mutation_intent(
    *,
    control: Mapping[str, Any],
    plan: Mapping[str, Any],
    rollout: Mapping[str, Any],
    rollout_sha256: str,
    recovery: Mapping[str, Any],
    recovery_sha256: str,
    predecessor: Mapping[str, Any] | None,
    predecessor_sha256: str | None,
    authorities: Mapping[str, Any],
    lock_authority: Mapping[str, Any],
    release_policy_sha256: str,
    change_schema_sha256: str,
    mutation_intent_schema_sha256: str,
    controller_sha256: str,
    route_contract_sha256: str,
    now: dt.datetime,
) -> dict[str, Any]:
    """Build the sanitized authority in a job with no provider mutation token."""
    checked = _clock_value(lambda: now)
    if plan.get("schema_version") != 2 or plan.get("authority") != "observation-only-production-plan":
        common.fail("production plan authority differs")
    common.validate_fresh_window(
        plan.get("issued_at"), plan.get("expires_at"), checked,
        maximum_age_seconds=common.MAX_PLAN_AGE_SECONDS,
        label="production plan for mutation intent",
    )
    _validate_plan_control(
        plan, control, authorities["production_plan"],
        release_policy_sha256, change_schema_sha256,
    )
    source_phase, target_phase, source_sha, target_digests = _plan_images(plan, rollout)
    if common.sha256_bytes(common.canonical_file_bytes(rollout)) != common.require_sha256(
        rollout_sha256, "rollout plan hash"
    ):
        common.fail("rollout plan exact-file hash differs")
    if rollout_sha256 != authorities["rollout_plan_sha256"]:
        common.fail("rollout authority differs")
    plan_hash = common.sha256_bytes(common.canonical_file_bytes(plan))
    if plan_hash != authorities["production_plan"]["sha256"]:
        common.fail("production plan authority differs")
    recovery = validate_recovery(recovery, recovery_sha256, checked)
    if recovery_sha256 != authorities["recovery"]["sha256"]:
        common.fail("recovery authority differs")
    _require_recovery_plan_authority(recovery, authorities["production_plan"])
    before = _plan_before_state(plan, predecessor)
    authoritative_predecessor_hash = authorities["predecessor_state_sha256"]
    _validate_predecessor(
        predecessor, predecessor_sha256, source_phase, before,
        authoritative_predecessor_hash,
    )
    candidate_hash = common.require_sha256(
        plan.get("target", {}).get("credential_neutral_logical_candidate_sha256"),
        "signed candidate hash",
    )
    desired = _desired_projection(
        canonical_spec_sha256=candidate_hash,
        before=before,
        target_digests=target_digests,
    )
    prepared_at = common.format_timestamp(checked)
    intent_deadline = min(
        checked + dt.timedelta(seconds=common.MAX_PLAN_AGE_SECONDS),
        common.require_timestamp(plan["expires_at"], "production plan expiry"),
        common.require_timestamp(recovery["expires_at"], "recovery readiness expiry"),
    )
    expires_at = common.format_timestamp(intent_deadline)
    predecessor_authority = plan["predecessor_authority"]
    predecessor_binding = {
        "kind": predecessor_authority["kind"],
        "run_id": predecessor_authority["run_id"],
        "run_attempt": predecessor_authority["run_attempt"],
        "artifact_id": predecessor_authority["artifact_id"],
        "artifact_name": (
            None if predecessor_authority["kind"] == "genesis"
            else f"production-phase-state-{predecessor_authority['run_id']}-{predecessor_authority['run_attempt']}"
        ),
        "artifact_digest": predecessor_authority["artifact_digest"],
        "sha256": predecessor_authority["state_sha256"],
    }
    rollout_authority = plan["rollout_authority"]
    intent_control = {
        **dict(control),
        "release_policy_sha256": common.require_sha256(
            release_policy_sha256, "release policy hash"
        ),
        "change_schema_sha256": common.require_sha256(
            change_schema_sha256, "phase schema hash"
        ),
        "mutation_intent_schema_sha256": common.require_sha256(
            mutation_intent_schema_sha256, "mutation intent schema hash"
        ),
        "controller_sha256": common.require_sha256(
            controller_sha256, "apply controller hash"
        ),
    }
    lineage = {
        "event_sequence": int(predecessor_authority["event_sequence"]) + 1,
        "phase_ordinal": common.PHASES.index(target_phase) + 1,
        "operation": "activate",
        "from": source_phase,
        "to": target_phase,
        "predecessor_kind": predecessor_authority["kind"],
        "predecessor_state_sha256": authoritative_predecessor_hash,
        "phase": target_phase,
        "phase_source_sha": source_sha,
    }
    before_sha = common.sha256_value(before)
    desired_sha = common.sha256_value(desired)
    mutation = {
        "http_method": "PUT",
        "endpoint_label": "app",
        "update_all_source_versions": False,
        "before_sha256": before_sha,
        "desired_sha256": desired_sha,
        "mutation_fingerprint_sha256": common.sha256_value(
            {
                "before_sha256": before_sha,
                "desired_sha256": desired_sha,
                "http_method": "PUT",
                "endpoint_label": "app",
                "update_all_source_versions": False,
            }
        ),
    }
    intent = {
        "schema_version": 2,
        "authority": "production-mutation-intent",
        "repository": common.REPOSITORY,
        "prepared_at": prepared_at,
        "expires_at": expires_at,
        "control": intent_control,
        "operation": "activate",
        "lineage": lineage,
        "authorities": {
            "rollout_plan_sha256": rollout_sha256,
            "rollout_authority": _full_binding(
                {
                    "run_id": rollout_authority["run_id"],
                    "run_attempt": rollout_authority["run_attempt"],
                    "artifact_id": rollout_authority["artifact_id"],
                    "artifact_digest": rollout_authority["artifact_digest"],
                    "sha256": rollout_sha256,
                },
                f"exact-four-phase-rollout-{rollout_authority['run_id']}-{rollout_authority['run_attempt']}",
            ),
            "production_plan": _full_binding(
                authorities["production_plan"],
                f"verified-production-plan-{authorities['production_plan']['run_id']}-{authorities['production_plan']['run_attempt']}",
            ),
            "recovery": _full_binding(
                authorities["recovery"],
                f"production-recovery-readiness-{authorities['recovery']['run_id']}-{authorities['recovery']['run_attempt']}",
            ),
            "predecessor_state": predecessor_binding,
        },
        "lock": copy.deepcopy(lock_authority),
        "before": before,
        "desired": desired,
        "mutation": mutation,
        "rollback": copy.deepcopy(common.ROLLBACK_FLOORS[target_phase]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": ENDPOINT_LABELS,
            "route_contract_sha256": common.require_sha256(
                route_contract_sha256, "route contract hash"
            ),
        },
    }
    return common.validate_mutation_intent(intent, now=checked)


def validate_intent_authority(value: Any, label: str = "mutation intent authority") -> dict[str, Any]:
    return common.validate_full_artifact_binding(value, label)


def apply_change(
    *,
    target: Mapping[str, str],
    control: Mapping[str, Any],
    token: str,
    plan: Mapping[str, Any],
    plan_sha256: str,
    rollout: Mapping[str, Any],
    rollout_sha256: str,
    recovery: Mapping[str, Any],
    recovery_sha256: str,
    predecessor: Mapping[str, Any] | None,
    predecessor_sha256: str | None,
    authorities: Mapping[str, Any],
    release_policy_sha256: str,
    change_schema_sha256: str,
    controller_sha256: str,
    route_contract_sha256: str | None,
    mutation_intent: Mapping[str, Any] | None = None,
    mutation_intent_sha256: str | None = None,
    mutation_intent_authority: Mapping[str, Any] | None = None,
    main_lock_proof: Mapping[str, Any] | None = None,
    main_lock_proof_sha256: str | None = None,
    main_lock_proof_authority: Mapping[str, Any] | None = None,
    now: dt.datetime | None = None,
    clock: Callable[[], dt.datetime] | None = None,
    opener: Any | None = None,
    sleeper: Callable[[float], None] = time.sleep,
    poll_limit: int = POLL_LIMIT,
) -> dict[str, Any]:
    time_source = clock or (
        (lambda fixed=now: fixed)
        if now is not None
        else (lambda: dt.datetime.now(dt.timezone.utc))
    )
    checked = _clock_value(time_source)
    if mutation_intent is None or mutation_intent_sha256 is None or mutation_intent_authority is None:
        common.fail("a durable signed mutation intent is required")
    mutation_intent = common.validate_mutation_intent(mutation_intent, now=checked)
    mutation_intent_hash = common.require_sha256(
        mutation_intent_sha256, "mutation intent exact-file hash"
    )
    if common.sha256_bytes(common.canonical_file_bytes(mutation_intent)) != mutation_intent_hash:
        common.fail("mutation intent exact-file hash differs")
    mutation_intent_authority = validate_intent_authority(mutation_intent_authority)
    expected_intent_name = (
        f"production-mutation-intent-apply-{mutation_intent_authority['run_id']}-1"
    )
    if (
        mutation_intent_authority["sha256"] != mutation_intent_hash
        or mutation_intent_authority["artifact_name"] != expected_intent_name
    ):
        common.fail("mutation intent artifact authority differs")
    intent_control = mutation_intent["control"]
    if (
        intent_control["workflow_sha"] != control.get("workflow_sha")
        or str(intent_control["run_id"]) != str(control.get("run_id"))
        or intent_control["run_attempt"] != control.get("run_attempt")
        or str(intent_control["run_id"]) != str(mutation_intent_authority["run_id"])
        or intent_control["run_attempt"] != mutation_intent_authority["run_attempt"]
        or intent_control["controller_sha256"]
        != common.require_sha256(controller_sha256, "apply controller hash")
    ):
        common.fail("mutation intent control authority differs")
    if main_lock_proof is None or main_lock_proof_sha256 is None or main_lock_proof_authority is None:
        common.fail("a signed post-lock proof is required before production mutation")
    main_lock_proof = common.validate_main_lock_proof(
        main_lock_proof, mutation_intent=mutation_intent, now=checked
    )
    proof_hash = common.require_sha256(
        main_lock_proof_sha256, "main lock proof exact-file hash"
    )
    proof_authority = common.validate_full_artifact_binding(
        main_lock_proof_authority, "main lock proof artifact authority"
    )
    if (
        common.sha256_bytes(common.canonical_file_bytes(main_lock_proof)) != proof_hash
        or proof_authority["sha256"] != proof_hash
        or proof_authority["artifact_name"]
        != f"production-main-lock-proof-apply-{proof_authority['run_id']}-1"
        or str(proof_authority["run_id"]) != str(control.get("run_id"))
        or proof_authority["run_attempt"] != control.get("run_attempt")
        or main_lock_proof["mutation_intent"] != mutation_intent_authority
    ):
        common.fail("main lock proof authority differs")
    target = common.validate_target_descriptor(dict(target))
    reviewed_route_hash = common.public_route_contract_sha256(target["default_ingress"])
    if route_contract_sha256 is not None and route_contract_sha256 != reviewed_route_hash:
        common.fail("canary route contract differs from the protected ingress")
    common.validate_fresh_window(plan.get("issued_at"), plan.get("expires_at"), checked, maximum_age_seconds=common.MAX_PLAN_AGE_SECONDS, label="production plan")
    if (
        plan.get("schema_version") != 2
        or plan.get("authority") != "observation-only-production-plan"
        or plan.get("repository") != common.REPOSITORY
    ):
        common.fail("production plan authority differs")
    _validate_plan_control(
        plan,
        control,
        authorities["production_plan"],
        release_policy_sha256,
        change_schema_sha256,
    )
    source_phase, target_phase, source_sha, target_digests = _plan_images(plan, rollout)
    if plan.get("rollback") != common.ROLLBACK_FLOORS[target_phase]:
        common.fail("production plan rollback floor differs")
    recovery = validate_recovery(recovery, recovery_sha256, checked)
    _require_recovery_plan_authority(recovery, authorities["production_plan"])
    if (
        recovery["control"]["contract_sha256"] != plan["control"]["contract_sha256"]
        or
        recovery["control"]["workflow_sha"] != control["workflow_sha"]
        or recovery["control"]["run_id"] != authorities["recovery"]["run_id"]
        or recovery["control"]["run_attempt"] != authorities["recovery"]["run_attempt"]
    ):
        common.fail("recovery artifact control authority differs")
    if common.sha256_bytes(common.canonical_file_bytes(plan)) != common.require_sha256(plan_sha256, "production plan hash"):
        common.fail("production plan exact-file hash differs")
    if common.sha256_bytes(common.canonical_file_bytes(rollout)) != common.require_sha256(rollout_sha256, "rollout plan hash"):
        common.fail("rollout plan exact-file hash differs")
    if rollout_sha256 != authorities["rollout_plan_sha256"]:
        common.fail("rollout evidence request differs")
    if plan_sha256 != authorities["production_plan"]["sha256"] or recovery_sha256 != authorities["recovery"]["sha256"]:
        common.fail("apply evidence artifact binding differs")
    authoritative_predecessor_hash = authorities["predecessor_state_sha256"]
    client = ProductionAppClient(target["app_id"], token, opener=opener)
    try:
        before, before_spec, _before_deployment = observe_stable(client)
        if before["default_ingress_sha256"] != common.sha256_bytes(
            target["default_ingress"].encode("utf-8")
        ):
            common.fail("protected default ingress differs from production")
        _match_plan_observation(plan, before)
        predecessor_authority = plan.get("predecessor_authority")
        if type(predecessor_authority) is not dict:
            common.fail("production predecessor authority is malformed")
        signed_predecessor_hash = common.require_sha256(
            predecessor_authority.get("state_sha256"), "signed predecessor state hash"
        )
        if signed_predecessor_hash != authoritative_predecessor_hash:
            common.fail("production predecessor hash differs")
        if source_phase != "genesis" and predecessor_sha256 != authoritative_predecessor_hash:
            common.fail("apply predecessor file hash differs")
        _validate_predecessor(
            predecessor,
            predecessor_sha256,
            source_phase,
            before,
            signed_predecessor_hash,
        )
        desired = common.set_phase_images(before_spec, target_digests)
        common.require_exact_image_change(before_spec, desired)
        candidate_hash = plan.get("target", {}).get("credential_neutral_logical_candidate_sha256")
        if common.sha256_value(desired) != common.require_sha256(candidate_hash, "signed candidate hash"):
            common.fail("exact desired spec differs from the signed plan")
        desired_projection = _desired_projection(
            canonical_spec_sha256=common.sha256_value(desired),
            before=before,
            target_digests=target_digests,
        )
        if (
            mutation_intent["operation"] != "activate"
            or mutation_intent["before"] != before
            or mutation_intent["desired"] != desired_projection
            or mutation_intent["authorities"]["rollout_plan_sha256"] != rollout_sha256
            or mutation_intent["authorities"]["production_plan"]["sha256"] != plan_sha256
            or mutation_intent["authorities"]["recovery"]["sha256"] != recovery_sha256
            or mutation_intent["lineage"]["predecessor_state_sha256"]
            != authoritative_predecessor_hash
            or mutation_intent["canary"]["route_contract_sha256"] != reviewed_route_hash
        ):
            common.fail("durable mutation intent differs from the exact apply candidate")
        # Last read-only CAS immediately before the one permitted PUT.
        cas_before, cas_spec, _ = observe_stable(client)
        if cas_before != before or cas_spec != before_spec:
            common.fail("production changed immediately before mutation")
        mutation_checked = require_fresh_immediately_before_mutation(
            plan=plan,
            recovery=recovery,
            clock=time_source,
        )
        recovery = validate_recovery(recovery, recovery_sha256, mutation_checked)
        _require_recovery_plan_authority(recovery, authorities["production_plan"])
        common.validate_mutation_intent(mutation_intent, now=mutation_checked)
        mutation_fingerprint = mutation_intent["mutation"]["mutation_fingerprint_sha256"]
        try:
            client.put_app_once(desired)
        except common.AmbiguousMutation:
            pass
        active_app, active_deployment, ambiguous = reconcile_until_active(
            client, desired, sleeper=sleeper, poll_limit=poll_limit
        )
        after, after_spec, after_deployment = provider_snapshot(
            active_app, active_deployment, client.app_id
        )
        final_first, final_spec, final_deployment = observe_stable(client)
        if after != final_first or after_spec != final_spec or after_deployment != final_deployment:
            common.fail("production changed during final double-read")
        if common.extract_image_digests(after_spec) != dict(target_digests):
            common.fail("active production image tuple differs")
        if before["environment_values_sha256"] != after["environment_values_sha256"]:
            common.fail("production environment changed")
        if before["non_source_projection_sha256"] != after["non_source_projection_sha256"]:
            common.fail("production non-source topology changed")
        _migration_succeeded(final_deployment)
        put_count = sum(method == "PUT" for method, _ in client.request_log)
        if put_count != 1 or any(method not in {"GET", "PUT"} for method, _ in client.request_log):
            common.fail("provider mutation ledger differs")
        completed = common.format_timestamp(
            checked if now is not None else dt.datetime.now(dt.timezone.utc)
        )
        predecessor_sequence = (
            0 if predecessor is None else common.exact_int(
                predecessor["lineage"]["event_sequence"], "predecessor event sequence", 1
            )
        )
        receipt = {
            "schema_version": 1,
            "authority": AUTHORITY,
            "repository": common.REPOSITORY,
            "completed_at": completed,
            "control": {
                **dict(control),
                "release_policy_sha256": common.require_sha256(release_policy_sha256, "release policy hash"),
                "change_schema_sha256": common.require_sha256(change_schema_sha256, "phase schema hash"),
                "controller_sha256": common.require_sha256(controller_sha256, "apply controller hash"),
            },
            "lineage": {
                "event_sequence": predecessor_sequence + 1,
                "phase_ordinal": common.PHASES.index(target_phase) + 1,
                "operation": "activate",
                "from": source_phase,
                "to": target_phase,
                "predecessor_kind": "genesis" if source_phase == "genesis" else "phase-state",
                "predecessor_state_sha256": authoritative_predecessor_hash,
                "phase": target_phase,
                "phase_source_sha": source_sha,
            },
            "authorities": {
                "rollout_plan_sha256": rollout_sha256,
                "production_plan": dict(authorities["production_plan"]),
                "recovery": dict(authorities["recovery"]),
                "mutation_intent": dict(mutation_intent_authority),
                "main_lock_proof": dict(proof_authority),
            },
            "provider_transition": {
                "http_methods_used": ["GET", "PUT"],
                "http_request_count": len(client.request_log),
                "mutation_request_count": 1,
                "endpoint_labels": ["app", "deployment"],
                "mutation_fingerprint_sha256": mutation_fingerprint,
                "ambiguous_reconciled": ambiguous,
            },
            "before": before,
            "after": after,
            "gates": {"deployment_succeeded": True, "migration_succeeded": True},
            "rollback": common.ROLLBACK_FLOORS[target_phase],
            "canary": {
                "required": True,
                "completed": False,
                "endpoint_labels": ENDPOINT_LABELS,
                "route_contract_sha256": reviewed_route_hash,
            },
        }
        common.validate_apply_receipt(receipt)
        common.sanitize_public(receipt, private_values=tuple(target.values()))
        return receipt
    finally:
        client.scrub()


def _add_common_inputs(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--evidence-request", required=True)
    parser.add_argument("--production-plan", required=True)
    parser.add_argument("--rollout-plan", required=True)
    parser.add_argument("--recovery", required=True)
    parser.add_argument("--predecessor-state")
    parser.add_argument("--release-policy-sha256", required=True)
    parser.add_argument("--change-schema-sha256", required=True)
    parser.add_argument("--mutation-intent-schema-sha256", required=True)
    parser.add_argument("--controller-sha256", required=True)
    parser.add_argument("--workflow-sha", required=True)
    parser.add_argument("--workflow-run-id", required=True)
    parser.add_argument("--workflow-run-attempt", required=True, type=int)
    parser.add_argument("--runner-temp", required=True)
    parser.add_argument("--output", required=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Apply one exact production phase")
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser(
        "prepare-intent", description="Prepare a mutation authority without a provider token"
    )
    _add_common_inputs(prepare)
    prepare.add_argument("--lock-authority", required=True)
    prepare.add_argument("--route-contract-sha256", required=True)
    execute = commands.add_parser(
        "execute", description="Execute one exact PUT bound to a signed mutation intent"
    )
    _add_common_inputs(execute)
    execute.add_argument("--target", required=True)
    execute.add_argument("--mutation-intent", required=True)
    execute.add_argument("--mutation-intent-authority", required=True)
    execute.add_argument("--main-lock-proof", required=True)
    execute.add_argument("--main-lock-proof-authority", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    evidence = validate_evidence_request(common.load_json(Path(args.evidence_request), "apply evidence request"))
    plan_path = Path(args.production_plan)
    rollout_path = Path(args.rollout_plan)
    recovery_path = Path(args.recovery)
    plan = common.load_json(plan_path, "production plan")
    rollout = common.load_json(rollout_path, "rollout plan")
    recovery = common.load_json(recovery_path, "recovery readiness")
    predecessor_path = Path(args.predecessor_state) if args.predecessor_state else None
    predecessor = common.load_json(predecessor_path, "predecessor phase state") if predecessor_path else None
    control = {
        "workflow_sha": common.require_sha1(args.workflow_sha, "workflow SHA"),
        "workflow_path": WORKFLOW_PATH,
        "run_id": common.require_run_id(args.workflow_run_id, "workflow run ID"),
        "run_attempt": common.exact_int(args.workflow_run_attempt, "workflow run attempt", 1, 1),
        "runner_environment": "github-hosted",
    }
    if args.command == "prepare-intent":
        lock_authority = common.load_json(Path(args.lock_authority), "branch lock authority")
        intent = prepare_apply_mutation_intent(
            control=control,
            plan=plan,
            rollout=rollout,
            rollout_sha256=common.sha256_bytes(rollout_path.read_bytes()),
            recovery=recovery,
            recovery_sha256=common.sha256_bytes(recovery_path.read_bytes()),
            predecessor=predecessor,
            predecessor_sha256=(
                common.sha256_bytes(predecessor_path.read_bytes()) if predecessor_path else None
            ),
            authorities=evidence,
            lock_authority=lock_authority,
            release_policy_sha256=args.release_policy_sha256,
            change_schema_sha256=args.change_schema_sha256,
            mutation_intent_schema_sha256=args.mutation_intent_schema_sha256,
            controller_sha256=args.controller_sha256,
            route_contract_sha256=args.route_contract_sha256,
            now=dt.datetime.now(dt.timezone.utc),
        )
        common.write_canonical_output(Path(args.output), intent, Path(args.runner_temp))
        return 0
    target = common.loads_strict(args.target)
    intent_path = Path(args.mutation_intent)
    intent_authority = common.load_json(
        Path(args.mutation_intent_authority), "mutation intent authority"
    )
    intent_value = common.load_json(intent_path, "production mutation intent")
    proof_path = Path(args.main_lock_proof)
    proof_value = common.load_json(proof_path, "production main lock proof")
    proof_authority = common.load_json(
        Path(args.main_lock_proof_authority), "main lock proof authority"
    )
    if (
        type(intent_value) is not dict
        or type(intent_value.get("control")) is not dict
        or intent_value["control"].get("mutation_intent_schema_sha256")
        != common.require_sha256(
            args.mutation_intent_schema_sha256, "current mutation intent schema hash"
        )
    ):
        common.fail("mutation intent is not bound to the current schema")
    token = os.environ.pop("DO_PRODUCTION_APPLY_TOKEN", "")
    receipt = apply_change(
        target=target,
        control=control,
        token=token,
        plan=plan,
        plan_sha256=common.sha256_bytes(plan_path.read_bytes()),
        rollout=rollout,
        rollout_sha256=common.sha256_bytes(rollout_path.read_bytes()),
        recovery=recovery,
        recovery_sha256=common.sha256_bytes(recovery_path.read_bytes()),
        predecessor=predecessor,
        predecessor_sha256=(common.sha256_bytes(predecessor_path.read_bytes()) if predecessor_path else None),
        authorities=evidence,
        release_policy_sha256=args.release_policy_sha256,
        change_schema_sha256=args.change_schema_sha256,
        controller_sha256=args.controller_sha256,
        route_contract_sha256=None,
        mutation_intent=intent_value,
        mutation_intent_sha256=common.sha256_bytes(intent_path.read_bytes()),
        mutation_intent_authority=intent_authority,
        main_lock_proof=proof_value,
        main_lock_proof_sha256=common.sha256_bytes(proof_path.read_bytes()),
        main_lock_proof_authority=proof_authority,
    )
    del token, target
    common.write_canonical_output(Path(args.output), receipt, Path(args.runner_temp))
    return 0


def main() -> int:
    try:
        return run_cli()
    except common.ReleaseError as exc:
        print(f"production apply failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
