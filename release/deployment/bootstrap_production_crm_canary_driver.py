#!/usr/bin/env python3
"""One protected-control driver bootstrap; never a production rollout.

Only a later separately approved main workflow may use ``install``. The public
packet binds all choices and the protected descriptor; missing choices are not
defaults. There is no retry/resume/update/delete or database/user/fixture create
path. An ambiguous create or secret installation requires read-only review.

Managed database credentials are resolved only by App Platform's fixed bindable
expression. This controller never requests a database connection or user record.
The sole allowed ledger write is the existing driver's table initialization.
"""
from __future__ import annotations

import argparse
import base64
import copy
import datetime as dt
import hashlib
import http.client
import io
import ipaddress
import os
from pathlib import Path
import re
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from typing import Any

try:
    from . import provision_production_crm_canary_fixture as fixture
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import provision_production_crm_canary_fixture as fixture

common = fixture.common
WORKFLOW = ".github/workflows/bootstrap-production-crm-canary-driver.yml"
PUBLISHER = ".github/workflows/publish-attest-production-crm-canary-driver.yml"
ENVIRONMENT = "rereply-production-canary"
SECRET_NAME = "CRM_CANARY_SYNTHETIC_DRIVER_JSON"
IMAGE = "ghcr.io/medtechcorps-netizen/rereply-crm-canary-driver"
PREDICATE = "https://rereply.app/attestations/production-crm-canary-driver-image/v1"
LEDGER_EXPRESSION = (
    "postgresql://${ledger.USERNAME}:${ledger.PASSWORD}@${ledger.HOSTNAME}:"
    "${ledger.PORT}/${ledger.DATABASE}?sslmode=require&uselibpqcompat=true"
)
EFFECTS = {
    "new_driver_apps": 1, "new_driver_instances": 1,
    "existing_ledger_app_admissions": 1, "ledger_table_initialization": True,
    "github_environment": ENVIRONMENT, "github_secret": SECRET_NAME,
    "github_secret_installations": 1, "production_app_updates": 0,
    "database_creations": 0, "database_user_creations": 0,
    "fixture_mutations": 0, "synthetic_executions": 0,
    "credential_redacted_selected_ledger_metadata_read": True,
}
AUTH_KEYS = {"schema_version", "kind", "control_sha", "operation_id", "issued_at",
             "expires_at", "protected_descriptor_sha256", "plan_sha256", "fixture_evidence",
             "driver_evidence", "effects"}
PLAN_KEYS = {"provider_account_uuid", "app_name", "region", "instance_size_slug",
             "instance_count", "monthly_usd", "ledger", "github_environment_sha256"}
LEDGER_KEYS = {"cluster_id", "cluster_name", "database", "user", "version",
               "firewall_sha256", "dedicated_synthetic_ledger"}
DESCRIPTOR_KEYS = {"schema_version", "control_sha", "operation_id",
                   "public_packet_sha256", "fixture_descriptor_sha256",
                   "hmac_key_base64", "scope_review", "plan"}
FIXTURE_EVIDENCE_KEYS = {"control_sha", "run_id", "artifact_id", "artifact_digest", "result_sha256"}
DRIVER_EVIDENCE_KEYS = {"control_sha", "run_id", "artifact_id", "artifact_digest",
                        "digest", "driver_version_sha256"}
SCOPE_REVIEW = {
    "read": ["account:read", "actions:read", "app:read", "database:read", "regions:read", "sizes:read"],
    # App creation resolves the existing db_user through this credential only.
    # Never add credential-view to READ or use CREATE for database GET requests.
    "create": ["actions:read", "app:create", "app:read", "database:read",
               "database:update", "database:view_credentials", "regions:read", "sizes:read"],
    "github": ["environments:write"],
    "github_read": ["actions:read", "administration:read", "attestations:read", "contents:read", "environments:read"],
    "ledger_user": "existing-dedicated-database-table-initialization-only",
}
PRIVATE_NAMES = (
    "CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "CRM_CANARY_FIXTURE_INPUT_JSON",
    "DO_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_DRIVER_BOOTSTRAP_CREATE_TOKEN",
    "GH_CANARY_ENVIRONMENT_WRITE_TOKEN",
)
MODE_ENV = "BOOTSTRAP_MODE"
MODES = frozenset({"check", "install"})
RUN_TITLES = {
    "check": "Check exact private CRM driver prerequisites",
    "install": "Install exact private CRM canary driver",
}
CHECK_PRIVATE_NAMES = PRIVATE_NAMES[:3]
STAGES = frozenset({
    "INITIALIZATION", "MODE", "AUTHORIZATION", "PRIVATE_INPUTS", "CURRENT_AUTHORITY",
    "PINNED_CLI", "PUBLIC_EVIDENCE_READER", "ADAPTERS", "ENVIRONMENT_PRESTATE",
    "FIXTURE_AUTHENTICATION", "IMAGE_AUTHENTICATION", "FIXTURE_INPUT_VALIDATION",
    "PROVIDER_ACCOUNT", "PROVIDER_SIZE", "PROVIDER_REGION", "PROVIDER_LEDGER_METADATA",
    "PROVIDER_LEDGER_DATABASE", "PROVIDER_FIREWALL", "PROVIDER_APPS",
    "FIXTURE_REHYDRATION", "RUNTIME_SPEC", "SECOND_CAS", "PROPOSE_APP", "PROPOSAL_POSTSTATE", "CHECK_COMPLETE",
    "CREATE_APP", "OBSERVE_CREATED", "HEALTH", "STABLE_HEALTH",
    "INSTALL_ENVIRONMENT", "FINAL_AUTHORITY", "RECEIPT_WRITE",
})
_diagnostic_stage = "INITIALIZATION"
LABEL = re.compile(r"^[a-z][a-z0-9-]{1,62}$")
APP_NAME = re.compile(r"^[a-z][a-z0-9-]{0,30}[a-z0-9]$")
DB_LABEL = re.compile(r"^[a-z][a-z0-9_]{1,62}$")
# The real attested publisher archive contains a ~21 MiB SBOM bundle and
# ~16 MiB SPDX document. Keep a bounded aggregate that admits that exact shape.
MAX_PUBLIC = 64 * 1024 * 1024
CONNECTION_NAMES = ("connection", "private_connection", "standby_connection", "standby_private_connection")
CONNECTION_FIELDS = frozenset({"uri", "database", "host", "port", "user", "password", "ssl"})
CONNECTION_OPTIONAL_FIELDS = frozenset({"protocol", "application_ports"})
LEDGER_METADATA_CODES = frozenset({"LEDGER_CONNECTION_SHAPE_REJECTED", "LEDGER_CONNECTION_CREDENTIALS_REJECTED",
    "LEDGER_URI_USERINFO_REJECTED", "LEDGER_URI_NONCANONICAL_REJECTED",
    "LEDGER_RESPONSE_TYPE_REJECTED", "LEDGER_DATABASE_TYPE_REJECTED", "LEDGER_CLUSTER_ID_REJECTED",
    "LEDGER_CONNECTION_TYPE_REJECTED", "LEDGER_CONNECTION_FIELDS_REJECTED",
    "LEDGER_URI_TYPE_REJECTED", "LEDGER_URI_LENGTH_REJECTED", "LEDGER_HOST_REJECTED",
    "LEDGER_PORT_REJECTED", "LEDGER_DATABASE_LABEL_REJECTED", "LEDGER_SSL_FLAG_REJECTED",
    "LEDGER_PROTOCOL_TYPE_REJECTED", "LEDGER_PROTOCOL_VALUE_REJECTED",
    "LEDGER_APPLICATION_PORTS_TYPE_REJECTED", "LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED"})
LEDGER_HOST = re.compile(r"(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+db\.ondigitalocean\.com")
MAX_LEDGER_URI = 1024
MAX_PROPOSAL_BYTES = 256 * 1024
PROPOSAL_URL = common.API_ORIGIN + "/v2/apps/propose"
PROPOSAL_CODES = frozenset({
    "PROPOSAL_HTTP_REJECTED", "PROPOSAL_REDIRECT_REJECTED", "PROPOSAL_TRANSPORT_FAILED",
    "PROPOSAL_RESPONSE_BOUND_REJECTED", "PROPOSAL_RESPONSE_SHAPE_REJECTED",
    "PROPOSAL_NAME_UNAVAILABLE", "PROPOSAL_RESPONSE_IDENTITY_REJECTED",
})
# These are mentions, not causal conclusions. Never copy a received field or
# message, or emit indexes, lengths, hashes, request IDs or normalized specs.
PROPOSAL_FIELDS = {
    "spec.name": "APP_NAME", "spec.region": "REGION",
    "spec.databases": "DATABASE_BINDING", "spec.services": "SERVICE",
    "instance_size_slug": "INSTANCE_SIZE", "instance_count": "INSTANCE_COUNT",
    "health_check": "HEALTH_CHECK", "deploy_on_push": "DEPLOY_ON_PUSH",
    "image.digest": "IMAGE_DIGEST", "image.registry": "IMAGE_REGISTRY",
    "image.repository": "IMAGE_REPOSITORY", "http_port": "HTTP_PORT",
    "envs": "ENVIRONMENT", "routes": "ROUTES", "cluster_name": "LEDGER_CLUSTER",
    "db_name": "LEDGER_DATABASE", "db_user": "LEDGER_USER",
    "CRM_CANARY_HMAC_KEY_BASE64": "ENV_HMAC",
    "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON": "ENV_FIXTURE_DESCRIPTOR",
    "CRM_CANARY_KLINIK_LOGIN_JSON": "ENV_KLINIK_LOGIN",
    "CRM_CANARY_NON_KLINIK_LOGIN_JSON": "ENV_NON_KLINIK_LOGIN",
    "CRM_CANARY_META_APP_SECRET": "ENV_META_SECRET",
    "CRM_CANARY_LEDGER_DATABASE_URL": "ENV_LEDGER_BINDING",
    "CRM_CANARY_DRIVER_VERSION_SHA256": "ENV_DRIVER_VERSION",
}
PROPOSAL_FIELD_CODES = frozenset(PROPOSAL_FIELDS.values())
CREATE_URL = common.API_ORIGIN + "/v2/apps"
MAX_CREATE_ERROR_BYTES = 256 * 1024
CREATE_CODES = frozenset({
    "CREATE_REQUEST_REJECTED", "CREATE_HTTP_REJECTED", "CREATE_REDIRECT_REJECTED",
    "CREATE_TRANSPORT_AMBIGUOUS", "CREATE_RESPONSE_BOUND_AMBIGUOUS",
    "CREATE_RESPONSE_SHAPE_AMBIGUOUS", "CREATE_RESPONSE_IDENTITY_AMBIGUOUS",
})
# The same reviewed literal mentions, never an interpretation of the cause.
CREATE_FIELDS = dict(PROPOSAL_FIELDS)
CREATE_FIELD_CODES = frozenset(CREATE_FIELDS.values())


def require(ok: bool, message: str) -> None:
    if not ok:
        common.fail(message)


def mark_stage(name: str) -> None:
    global _diagnostic_stage
    require(name in STAGES, "diagnostic stage differs")
    _diagnostic_stage = name


class LedgerCredentialResponseRejected(common.ReleaseError):
    def __init__(self, *, uri: bool):
        super().__init__("ledger response contains forbidden credentials")
        self.code = "LEDGER_URI_RESPONSE_REJECTED" if uri else "LEDGER_CREDENTIAL_RESPONSE_REJECTED"


class LedgerMetadataRejected(common.ReleaseError):
    def __init__(self, code: str):
        require(code in LEDGER_METADATA_CODES, "ledger diagnostic code differs")
        super().__init__("ledger metadata compatibility rejected")
        self.code = code


class ProposalRejected(common.ReleaseError):
    def __init__(self, code: str, http_status: int | None = None, field_mentions: tuple[str, ...] = ()):
        super().__init__("app spec proposal rejected")
        self.code, self.http_status, self.field_mentions = code, http_status, field_mentions


class CreateRejected(common.ReleaseError):
    def __init__(self, code: str, http_status: int | None = None, field_mentions: tuple[str, ...] = ()):
        super().__init__("app creation did not complete cleanly; read-only reconciliation required")
        self.code, self.http_status, self.field_mentions = code, http_status, field_mentions


def failure_report(error: Exception) -> dict[str, Any]:
    """Only reviewed constants escape; never format upstream/private objects."""
    if type(error) is CreateRejected:
        code = getattr(error, "code", None)
        status = getattr(error, "http_status", None)
        fields = getattr(error, "field_mentions", None)
        if (type(code) is str and code in CREATE_CODES
                and (status is None or type(status) is int and 100 <= status <= 599)
                and type(fields) is tuple and len(fields) <= len(CREATE_FIELD_CODES)
                and all(type(v) is str and v in CREATE_FIELD_CODES for v in fields)
                and fields == tuple(sorted(set(fields)))):
            return {"schema_version": 1, "outcome": "ERROR", "stage": "CREATE_APP", "code": code,
                    "create": {"http_status": status, "field_mentions": list(fields)}}
    if type(error) is ProposalRejected:
        # Revalidate even our exception: injected/modified attributes must not
        # become a side channel around the closed diagnostic vocabulary.
        code = getattr(error, "code", None)
        status = getattr(error, "http_status", None)
        fields = getattr(error, "field_mentions", None)
        if (type(code) is str and code in PROPOSAL_CODES
                and (status is None or type(status) is int and 100 <= status <= 599)
                and type(fields) is tuple and len(fields) <= len(PROPOSAL_FIELD_CODES)
                and all(type(v) is str and v in PROPOSAL_FIELD_CODES for v in fields)
                and fields == tuple(sorted(set(fields)))):
            return {"schema_version": 1, "outcome": "ERROR", "stage": "PROPOSE_APP", "code": code,
                    "proposal": {"http_status": status, "field_mentions": list(fields)}}
    code = "BOOTSTRAP_CHECK_FAILED"
    if isinstance(error, common.ReleaseError):
        try:
            message = str(error)
        except Exception:
            message = ""
        if type(error) is LedgerMetadataRejected and error.code in LEDGER_METADATA_CODES:
            code = error.code
        elif (type(error) is LedgerCredentialResponseRejected
                and error.code in ("LEDGER_URI_RESPONSE_REJECTED", "LEDGER_CREDENTIAL_RESPONSE_REJECTED")):
            code = error.code
        elif message == "ledger response contains forbidden credentials":
            code = "LEDGER_CREDENTIAL_RESPONSE_REJECTED"
        elif re.fullmatch(r"bounded HTTP operation failed: status [1-5][0-9]{2}", message):
            status = int(message.rsplit(" ", 1)[1])
            code = ({401: "HTTP_UNAUTHORIZED", 403: "HTTP_FORBIDDEN",
                     404: "HTTP_NOT_FOUND", 429: "HTTP_RATE_LIMITED"}.get(status)
                    or ("HTTP_SERVER_ERROR" if status >= 500 else "HTTP_REJECTED"))
    return {"schema_version": 1, "outcome": "ERROR",
            "stage": _diagnostic_stage if _diagnostic_stage in STAGES else "INITIALIZATION",
            "code": code}


def public_packet_hash(authorization: dict[str, Any]) -> str:
    """Two-pass binding, not a recursive hash: public choices first, private next."""
    return common.sha256_value({k: v for k, v in authorization.items()
                                if k != "protected_descriptor_sha256"})


def validate_authorization(value: Any, *, control_sha: str, now: dt.datetime) -> dict[str, Any]:
    a = common.exact_keys(value, AUTH_KEYS, "bootstrap authorization")
    require(type(a["schema_version"]) is int and a["schema_version"] == 1
            and a["kind"] == "production-crm-canary-driver-bootstrap-v1",
            "bootstrap authorization schema differs")
    require(common.require_sha1(a["control_sha"], "control") == control_sha, "bootstrap control differs")
    common.require_uuid(a["operation_id"], "bootstrap operation")
    common.require_sha256(a["protected_descriptor_sha256"], "protected descriptor digest")
    issued = common.require_timestamp(a["issued_at"], "bootstrap issue time")
    expires = common.require_timestamp(a["expires_at"], "bootstrap expiry")
    require(issued <= now < expires and dt.timedelta(0) < expires - issued <= dt.timedelta(hours=2),
            "bootstrap authorization window differs")
    require(a["effects"] == EFFECTS, "bootstrap effects differ")
    common.require_sha256(a["plan_sha256"], "protected resource plan")
    for field, keys in (("fixture_evidence", FIXTURE_EVIDENCE_KEYS),
                        ("driver_evidence", DRIVER_EVIDENCE_KEYS)):
        e = common.exact_keys(a[field], keys, "bootstrap evidence")
        common.require_sha1(e["control_sha"], "evidence control")
        for key in ("run_id", "artifact_id"):
            common.require_run_id(e[key], "evidence identity")
        common.require_digest(e["artifact_digest"], "evidence artifact")
    common.require_sha256(a["fixture_evidence"]["result_sha256"], "fixture result")
    common.require_digest(a["driver_evidence"]["digest"], "driver image")
    common.require_sha256(a["driver_evidence"]["driver_version_sha256"], "driver input manifest")
    return copy.deepcopy(a)


def validate_plan(value: Any) -> dict[str, Any]:
    p = common.exact_keys(value, PLAN_KEYS, "bootstrap plan")
    common.require_uuid(p["provider_account_uuid"], "provider account")
    common.require_sha256(p["github_environment_sha256"], "reviewed environment metadata")
    require(common.exact_string(p["app_name"], "driver app", APP_NAME).startswith("rereply-canary-driver-"),
            "driver app namespace differs")
    common.exact_string(p["region"], "driver region", LABEL)
    common.exact_string(p["instance_size_slug"], "driver size", re.compile(r"^[a-z0-9.-]{1,64}$"))
    require(type(p["instance_count"]) is int and p["instance_count"] == 1, "driver instance count differs")
    common.exact_string(p["monthly_usd"], "driver monthly cost", re.compile(r"^[1-9][0-9]{0,3}\.[0-9]{2}$"))
    ledger = common.exact_keys(p["ledger"], LEDGER_KEYS, "existing ledger")
    common.require_uuid(ledger["cluster_id"], "ledger cluster")
    common.exact_string(ledger["cluster_name"], "ledger cluster name", LABEL)
    common.exact_string(ledger["database"], "ledger database", DB_LABEL)
    require(ledger["user"] == "crm_canary_driver", "dedicated ledger role differs")
    require(ledger["dedicated_synthetic_ledger"] is True, "dedicated ledger review missing")
    common.exact_string(ledger["version"], "ledger version", re.compile(r"^[1-9][0-9]$"))
    common.require_sha256(ledger["firewall_sha256"], "ledger firewall digest")
    return copy.deepcopy(p)


def validate_descriptor(value: Any, authorization: dict[str, Any]) -> dict[str, Any]:
    d = common.exact_keys(value, DESCRIPTOR_KEYS, "protected bootstrap descriptor")
    require(type(d["schema_version"]) is int and d["schema_version"] == 1
            and d["control_sha"] == authorization["control_sha"]
            and d["operation_id"] == authorization["operation_id"]
            and d["public_packet_sha256"] == public_packet_hash(authorization)
            and common.sha256_value(d) == authorization["protected_descriptor_sha256"],
            "protected bootstrap packet binding differs")
    common.require_sha256(d["fixture_descriptor_sha256"], "fixture runtime binding")
    require(common.sha256_value(validate_plan(d["plan"])) == authorization["plan_sha256"],
            "protected resource plan binding differs")
    require(d["scope_review"] == SCOPE_REVIEW, "purpose-scoped credential review missing")
    try:
        key = base64.b64decode(d["hmac_key_base64"], validate=True)
        require(len(key) == 32 and base64.b64encode(key).decode() == d["hmac_key_base64"],
                "bootstrap HMAC key differs")
    except (ValueError, TypeError):
        common.fail("bootstrap HMAC key differs")
    return copy.deepcopy(d)


def require_unique_run(api: Any, control_sha: str, run_id: str, mode: str = "install") -> None:
    """Complete history permits read-only checks, but only one install per control.

    Fixed titles derive solely from the reviewed workflow's explicit mode choice.
    Every run must be attempt one. Unknown titles, deleted history, or reruns are
    not recovery mechanisms; a prior install burns this control regardless of its
    conclusion. Checks must complete before another check or install may proceed.
    """
    require(mode in MODES, "bootstrap mode differs")
    path = fixture.API_PREFIX + "/actions/workflows/" + WORKFLOW.rsplit("/", 1)[1]
    workflow = api.get(path)
    identity = workflow.get("id")
    require(type(identity) is int and identity > 0 and workflow.get("path") == WORKFLOW,
            "bootstrap workflow identity differs")
    result = api.pages(path + "/runs?head_sha=" + control_sha, "workflow_runs")
    runs = result["workflow_runs"]
    require(type(runs) is list and result["total_count"] == len(runs) and 0 < len(runs) <= 10000,
            "bootstrap run inventory differs")
    ids, installs, current = set(), [], []
    for run in runs:
        rid = common.require_run_id(run.get("id"), "bootstrap history run")
        require(rid not in ids and run.get("workflow_id") == identity
                and run.get("head_sha") == control_sha and run.get("head_branch") == "main"
                and run.get("event") == "workflow_dispatch" and run.get("path") == WORKFLOW
                and type(run.get("run_attempt")) is int and run["run_attempt"] == 1
                and run.get("display_title") in RUN_TITLES.values(), "bootstrap run inventory differs")
        ids.add(rid)
        if run["display_title"] == RUN_TITLES["install"]:
            installs.append(rid)
        if rid == run_id:
            current.append(run)
            require(run["display_title"] == RUN_TITLES[mode]
                    and run.get("status") in ("queued", "in_progress", "waiting", "pending", "requested")
                    and run.get("conclusion") is None, "bootstrap current mode or status differs")
        else:
            require(run["display_title"] == RUN_TITLES["check"] and run.get("status") == "completed"
                    and run.get("conclusion") in ("success", "failure", "cancelled", "timed_out",
                        "action_required", "neutral", "skipped", "startup_failure", "stale"),
                    "bootstrap prior run is not a terminal check")
    require(len(current) == 1 and installs == ([run_id] if mode == "install" else []),
            "bootstrap install authority was already consumed")


def runtime_spec(a: dict[str, Any], d: dict[str, Any], protected: dict[str, Any],
                 driver_descriptor: dict[str, Any]) -> dict[str, Any]:
    p, ledger = d["plan"], d["plan"]["ledger"]
    require(common.sha256_value(driver_descriptor) == d["fixture_descriptor_sha256"],
            "rehydrated fixture binding differs")
    values = {
        "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON": common.canonical_payload_bytes(driver_descriptor).decode(),
        "CRM_CANARY_KLINIK_LOGIN_JSON": common.canonical_payload_bytes({
            "email": protected["registration"]["klinik_email"],
            "password": protected["credentials"]["klinik_password"]}).decode(),
        "CRM_CANARY_NON_KLINIK_LOGIN_JSON": common.canonical_payload_bytes({
            "email": protected["registration"]["non_klinik_email"],
            "password": protected["credentials"]["non_klinik_password"]}).decode(),
        "CRM_CANARY_META_APP_SECRET": protected["credentials"]["meta_app_secret"],
        "CRM_CANARY_HMAC_KEY_BASE64": d["hmac_key_base64"],
    }
    return {
        "name": p["app_name"], "region": p["region"],
        "databases": [{"name": "ledger", "engine": "PG", "production": True,
                       "cluster_name": ledger["cluster_name"], "db_name": ledger["database"],
                       "db_user": ledger["user"], "version": ledger["version"]}],
        "services": [{"name": "driver", "instance_count": 1,
                      "instance_size_slug": p["instance_size_slug"], "http_port": 8080,
                      "image": {"registry_type": "GHCR", "registry": "medtechcorps-netizen",
                                "repository": "rereply-crm-canary-driver",
                                "digest": a["driver_evidence"]["digest"],
                                "deploy_on_push": {"enabled": False}},
                      "health_check": {"http_path": "/healthz", "port": 8080},
                      "routes": [{"path": "/"}],
                      "envs": [{"key": key, "value": value, "scope": "RUN_TIME", "type": "SECRET"}
                               for key, value in sorted(values.items())] + [
                          {"key": "CRM_CANARY_DRIVER_VERSION_SHA256", "value": a["driver_evidence"]["driver_version_sha256"],
                           "scope": "RUN_TIME", "type": "GENERAL"},
                          {"key": "CRM_CANARY_LEDGER_DATABASE_URL", "value": LEDGER_EXPRESSION,
                           "scope": "RUN_TIME", "type": "GENERAL"}]}],
    }


def firewall_projection(raw: Any) -> list[dict[str, str]]:
    require(type(raw) is dict and type(raw.get("rules")) is list and 0 < len(raw["rules"]) <= 100,
            "ledger trusted sources are not restricted")
    rules = []
    for rule in raw["rules"]:
        require(type(rule) is dict and set(rule) <= {"uuid", "cluster_uuid", "type", "value", "created_at"},
                "ledger firewall rule fields differ")
        kind, value = rule.get("type"), rule.get("value")
        require(kind in ("app", "ip_addr") and type(value) is str, "ledger firewall rule kind differs")
        if kind == "app":
            common.require_uuid(value, "trusted app")
        else:
            network = ipaddress.ip_network(value, strict=False)
            require(network.prefixlen == network.max_prefixlen and network.is_global,
                    "ledger trusted IP is not one public address")
        rules.append({"type": kind, "value": value})
    require(len({(r["type"], r["value"]) for r in rules}) == len(rules), "duplicate ledger trusted source")
    return sorted(rules, key=lambda r: (r["type"], r["value"]))


def public_receipt(a: dict[str, Any], app_id: str, deployment_id: str, driver_url: str) -> dict[str, Any]:
    """Only fixed labels, IDs and high-entropy/public source hashes escape."""
    return {"schema_version": 1, "kind": "production-crm-canary-driver-bootstrap-result-v1",
            "control_sha": a["control_sha"], "authorization_sha256": common.sha256_value(a),
            "operation_id": a["operation_id"], "app_id_sha256": common.sha256_bytes(app_id.encode()),
            "deployment_id_sha256": common.sha256_bytes(deployment_id.encode()),
            "driver_url_sha256": common.sha256_bytes(driver_url.encode()),
            "driver_version_sha256": a["driver_evidence"]["driver_version_sha256"],
            "image_digest": a["driver_evidence"]["digest"],
            "fixture_result_sha256": a["fixture_evidence"]["result_sha256"],
            "state": "active-health-and-environment-install-observed",
            "synthetic_execution": "not-performed"}


def subprocess_environment(*, token: str | None = None) -> dict[str, str]:
    # No provider/private descriptor, GH_DEBUG, proxy, git config injection or
    # user CLI configuration is inherited by a child process.
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
           "GH_HOST": "github.com", "GH_PROMPT_DISABLED": "1",
           "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}
    for name in ("SYSTEMROOT", "WINDIR", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"):
        if name in os.environ:
            env[name] = os.environ[name]
    if os.environ.get("RUNNER_TEMP"):
        env["HOME"] = os.environ["RUNNER_TEMP"]
        env["GH_CONFIG_DIR"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-empty-gh-config")
        env["XDG_STATE_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-state")
        env["XDG_CACHE_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-cache")
        env["XDG_CONFIG_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-config")
    if token is not None:
        env["GH_TOKEN"] = fixture._secret(token)
    return env


class GitHubRead(fixture.GitHubRead):
    """Permit only the exact SHA comparison syntax the producer rejects.

    The pinned producer's generic '..' rejection also rejects GitHub's valid
    comparison separator. Do not alter that producer or broaden arbitrary paths.
    """
    def __init__(self, token: str):
        super().__init__(token)
        self.__comparison_token = fixture._secret(token)

    def get(self, path: str) -> Any:
        if re.fullmatch(re.escape(fixture.API_PREFIX) + r"/compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40}", path):
            return common.loads_strict(fixture._wire(self.opener, "https://api.github.com" + path,
                headers={"Authorization": "Bearer " + self.__comparison_token,
                         "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}, maximum=MAX_PUBLIC))
        return super().get(path)


def driver_manifest(root: Path) -> bytes:
    proc = subprocess.run(["git", "-C", str(root), "ls-tree", "-rz", "--full-tree", "HEAD", "--",
                           "docker/crm-canary-driver.Dockerfile", "frontend/package.json",
                           "frontend/package-lock.json", "frontend/canary-driver"],
                          env=subprocess_environment(), capture_output=True, check=False, timeout=30)
    require(proc.returncode == 0 and len(proc.stdout) < 256 * 1024, "driver tree read failed")
    rows = []
    for entry in proc.stdout.split(b"\0"):
        if not entry:
            continue
        match = re.fullmatch(rb"(100644|100755) blob ([0-9a-f]{40})\t([A-Za-z0-9._/-]+)", entry)
        require(match is not None, "driver source tree differs")
        mode, blob, raw_path = (item.decode() for item in match.groups())
        require(".." not in raw_path and not raw_path.startswith("/"), "driver source path differs")
        path = root / raw_path
        require(path.is_file() and not path.is_symlink(), "driver source type differs")
        raw = path.read_bytes()
        require(hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == blob,
                "driver checkout bytes differ")
        rows.append((raw_path, f"{mode}\t{blob}\t{common.sha256_bytes(raw)}\t{raw_path}\n"))
    names = [name for name, _ in rows]
    require(len(names) >= 4 and len(set(names)) == len(names)
            and {"docker/crm-canary-driver.Dockerfile", "frontend/package.json", "frontend/package-lock.json"}
                <= set(names), "driver input inventory differs")
    return "".join(row for _, row in sorted(rows)).encode()


def publisher_files(raw: bytes) -> dict[str, bytes]:
    expected = {"driver-inputs.tsv", "driver-predicate.json", "image-inspect.json", "image.json",
                "provenance.bundle.json", "remote-descriptor.json", "sbom.bundle.json", "sbom.spdx.json",
                "scan.json", "secret-report.json", "source-binding.bundle.json", "unit-test.txt",
                "vulnerability-report.json"}
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        records = archive.infolist()
        require(len(records) == len(expected) and {r.filename for r in records} == expected,
                "publisher artifact file inventory differs")
        require(sum(r.file_size for r in records) <= MAX_PUBLIC
                and all(not r.is_dir() and not (r.flag_bits & 1)
                        and ((r.external_attr >> 16) & 0o170000) != 0o120000 for r in records),
                "publisher artifact bounds differ")
        return {r.filename: archive.read(r) for r in records}


def authenticate_driver(api: Any, root: Path, gh: Path, evidence: dict[str, Any], current: str,
                        *, gh_token: str) -> None:
    sha, run_id = evidence["control_sha"], str(evidence["run_id"])
    prefix = fixture.API_PREFIX
    comparison = api.get(prefix + "/compare/" + sha + "..." + current)
    require(comparison.get("status") in ("ahead", "identical")
            and comparison.get("merge_base_commit", {}).get("sha") == sha,
            "publisher control is not protected ancestry")
    old = api.get(prefix + "/contents/" + PUBLISHER + "?ref=" + sha)
    new = api.get(prefix + "/contents/" + PUBLISHER + "?ref=" + current)
    require(common.require_sha1(old.get("sha"), "publisher workflow") == new.get("sha"),
            "publisher workflow changed after evidence")
    run = api.get(prefix + "/actions/runs/" + run_id)
    require(str(run.get("id")) == run_id and run.get("head_sha") == sha
            and run.get("head_branch") == "main" and run.get("path") == PUBLISHER
            and run.get("event") == "workflow_dispatch" and run.get("run_attempt") == 1
            and run.get("status") == "completed" and run.get("conclusion") == "success"
            and run.get("repository", {}).get("full_name") == common.REPOSITORY,
            "publisher run is not exact successful authority")
    jobs = api.pages(prefix + "/actions/runs/" + run_id + "/attempts/1/jobs", "jobs")
    names = {"Verify protected-main Test authority", "Build and publish exact CRM canary driver",
             "Scan, SBOM, unit-test, and inspect exact driver", "Attest exact CRM canary driver",
             "Verify exact attestations and anonymous digest pull", "Exact production CRM canary driver image gate"}
    require(jobs["total_count"] == 6 and len(jobs["jobs"]) == 6
            and {j["name"] for j in jobs["jobs"]} == names
            and all(j.get("conclusion") == "success" for j in jobs["jobs"]),
            "publisher job inventory differs")
    path = prefix + "/actions/runs/" + run_id + "/artifacts"
    inventory = api.pages(path, "artifacts")
    expected = {kind + "-crm-canary-driver-" + run_id + "-1"
                for kind in ("attested", "image", "scanned", "verified")}
    require(inventory["total_count"] == 4 and len(inventory["artifacts"]) == 4
            and {a["name"] for a in inventory["artifacts"]} == expected,
            "publisher artifact inventory differs")
    for artifact in inventory["artifacts"]:
        fixture._artifact_record(artifact, artifact["name"], run_id, sha,
                                 common.require_digest(artifact.get("digest"), "publisher artifact digest"))
    artifact = next(a for a in inventory["artifacts"] if a["name"].startswith("attested-"))
    require(str(artifact["id"]) == str(evidence["artifact_id"])
            and artifact["digest"] == evidence["artifact_digest"], "publisher artifact selection differs")
    files = publisher_files(api.artifact(str(artifact["id"]), evidence["artifact_digest"]))
    manifest = driver_manifest(root)
    require(files["driver-inputs.tsv"] == manifest
            and common.sha256_bytes(manifest) == evidence["driver_version_sha256"],
            "published driver inputs differ from current control")
    policy = common.loads_strict(files["driver-predicate.json"])
    require(policy.get("schema_version") == 1 and policy.get("repository") == common.REPOSITORY
            and policy.get("workflow_path") == PUBLISHER and policy.get("workflow_sha") == sha
            and str(policy.get("run_id")) == run_id and policy.get("run_attempt") == 1
            and policy.get("driver_version_sha256") == evidence["driver_version_sha256"]
            and policy.get("subject") == {"image": IMAGE, "digest": evidence["digest"], "platform": "linux/amd64"},
            "publisher signed policy binding differs")
    identity = common.loads_strict(files["image.json"])
    require(policy.get("image_identity") == identity and identity.get("control_sha") == sha
            and identity.get("digest") == evidence["digest"] and identity.get("image") == IMAGE
            and identity.get("platform") == "linux/amd64" and identity.get("tag_is_authority") is False
            and identity.get("driver_version_sha256") == evidence["driver_version_sha256"]
            and "sha256:" + common.sha256_bytes(files["remote-descriptor.json"]) == evidence["digest"]
            and policy.get("verification") == common.loads_strict(files["scan.json"]),
            "publisher image identity differs")
    flags = ["--repo", common.REPOSITORY, "--signer-workflow", common.REPOSITORY + "/" + PUBLISHER,
             "--signer-digest", sha, "--source-digest", sha, "--source-ref", "refs/heads/main",
             "--deny-self-hosted-runners", "--format", "json"]
    with tempfile.TemporaryDirectory(prefix="driver-public-evidence-", dir=os.environ["RUNNER_TEMP"]) as temp:
        for bundle, predicate, expected_policy in (
                ("provenance.bundle.json", "https://slsa.dev/provenance/v1", None),
                ("sbom.bundle.json", "https://spdx.dev/Document/v2.3", common.loads_strict(files["sbom.spdx.json"])),
                ("source-binding.bundle.json", PREDICATE, policy)):
            target = Path(temp) / bundle
            target.write_bytes(files[bundle])  # Public signed evidence only.
            result = subprocess.run([str(gh), "attestation", "verify", "oci://" + IMAGE + "@" + evidence["digest"],
                                     *flags, "--bundle", str(target), "--predicate-type", predicate],
                                    env=subprocess_environment(token=gh_token), capture_output=True,
                                    check=False, timeout=120)
            require(result.returncode == 0 and len(result.stdout) <= MAX_PUBLIC,
                    "driver signature verification failed")
            verified = common.loads_strict(result.stdout)
            require(type(verified) is list and 0 < len(verified) <= 100,
                    "driver signature verification empty")
            if expected_policy is not None:
                require(any(v.get("verificationResult", {}).get("statement", {}).get("predicate") == expected_policy
                            for v in verified), "driver signed predicate differs")
    time.sleep(2)
    require(inventory == api.pages(path, "artifacts"), "publisher artifact inventory changed")
    require(api.get(prefix + "/actions/runs/" + run_id).get("run_attempt") == 1,
            "publisher attempt changed")


def reject_database_credentials(value: Any) -> None:
    """Defense in depth, not token-scope introspection or scope proof.

    Only the independently reviewed database:read credential may reach this
    endpoint. A mis-scoped response fails without printing or persisting it.
    """
    if type(value) is dict:
        for key, child in value.items():
            if any(word in key.lower() for word in ("password", "secret", "token")) or key.lower() in (
                    "uri", "url", "connection_string"):
                if child not in (None, ""):
                    raise LedgerCredentialResponseRejected(uri=key.lower() in ("uri", "url", "connection_string"))
            reject_database_credentials(child)
    elif type(value) is list:
        for child in value:
            reject_database_credentials(child)


def project_ledger_metadata(value: Any, cluster_id: str) -> dict[str, Any]:
    """Drop only validated, unused connections from the exact selected cluster.

    DigitalOcean's documented connection URI is assembled from sibling fields;
    its schema does not promise an empty URI without view_credentials. Admit
    only literal reconstruction with zero credential characters, never a parsed
    or redacted credential. These URIs are not authority or network selectors.
    DigitalOcean OpenAPI a06aa8a675a047af2db0865f51b7e81e2e972a23,
    specification/resources/databases/models/database_connection.yml, lists
    seven fields. Godo 22aca23972064d6a96366a8c19ae847b30a6b267,
    databases.go DatabaseConnection, also declares protocol:string and
    application_ports:map[string]uint32 (introduced for Kafka). DigitalOcean's
    published PostgreSQL metadata example uses the exact protocol "postgresql":
    https://www.digitalocean.com/community/tutorials/how-to-collect-scrapable-metrics-for-managed-dbaas-on-digitalocean
    Admit that literal plus absent/null/empty protocol, never arbitrary strings;
    application_ports remains absent/null/empty-map only. PostgreSQL documents
    postgres:// and postgresql:// as URI aliases in libpq-connect.html's
    LIBPQ-CONNSTRING-URIS section. Neither source proves the live payload value;
    both aliases still require literal zero-credential sibling reconstruction.
    """
    def check(ok: bool, code: str) -> None:
        if not ok:
            raise LedgerMetadataRejected(code)

    # Fixed predicate names only: never include received keys, values or lengths.
    # Preserve the original guard order; validate SDK extensions before dropping.
    check(type(value) is dict, "LEDGER_RESPONSE_TYPE_REJECTED")
    check(type(value.get("database")) is dict, "LEDGER_DATABASE_TYPE_REJECTED")
    database = value["database"]
    check(database.get("id") == common.require_uuid(cluster_id, "selected ledger cluster"),
          "LEDGER_CLUSTER_ID_REJECTED")
    projected = copy.deepcopy(value)
    # Keep every object present during the full recursive guard. Only the four
    # exact string URI slots are quarantined for stricter validation below;
    # unknown URI locations and later nested secrets cannot disappear in a drop.
    for name in CONNECTION_NAMES:
        connection = projected["database"].get(name)
        if type(connection) is dict and type(connection.get("uri")) is str:
            connection["uri"] = ""
    reject_database_credentials(projected)
    for name in CONNECTION_NAMES:
        connection = database.get(name)
        if connection is None:
            continue
        check(type(connection) is dict, "LEDGER_CONNECTION_TYPE_REJECTED")
        check(set(connection) <= CONNECTION_FIELDS | CONNECTION_OPTIONAL_FIELDS,
              "LEDGER_CONNECTION_FIELDS_REJECTED")
        check(all(connection.get(key) in (None, "") for key in ("user", "password")),
              "LEDGER_CONNECTION_CREDENTIALS_REJECTED")
        # These unused shared-SDK fields never influence connections, provider
        # requests, the runtime spec or ledger authority. Only the documented
        # PostgreSQL protocol literal can carry nonempty metadata content.
        # Do not use generic falsiness: False, 0, [] and wrong-type empties fail.
        protocol = connection.get("protocol")
        check(protocol is None or type(protocol) is str, "LEDGER_PROTOCOL_TYPE_REJECTED")
        check(protocol is None or protocol in ("", "postgresql"), "LEDGER_PROTOCOL_VALUE_REJECTED")
        application_ports = connection.get("application_ports")
        check(application_ports is None or type(application_ports) is dict,
              "LEDGER_APPLICATION_PORTS_TYPE_REJECTED")
        check(application_ports is None or not application_ports,
              "LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED")
        uri = connection.get("uri")
        if uri in (None, ""):
            continue  # Preserve the already allowed missing/redacted-empty case.
        check(type(uri) is str, "LEDGER_URI_TYPE_REJECTED")
        check(0 < len(uri) <= MAX_LEDGER_URI, "LEDGER_URI_LENGTH_REJECTED")
        host, port, db = connection.get("host"), connection.get("port"), connection.get("database")
        check(type(host) is str and 0 < len(host) <= 253 and LEDGER_HOST.fullmatch(host) is not None,
              "LEDGER_HOST_REJECTED")
        check(type(port) is int and 1 <= port <= 65535, "LEDGER_PORT_REJECTED")
        check(type(db) is str and DB_LABEL.fullmatch(db) is not None, "LEDGER_DATABASE_LABEL_REJECTED")
        check(connection.get("ssl") is True, "LEDGER_SSL_FLAG_REJECTED")
        # Empty userinfo punctuation carries no credential bits. Do not use a
        # URL parser, percent decoding, normalization or any redaction marker.
        schemes = ("postgres://", "postgresql://")
        expected = {f"{scheme}{prefix}{host}:{port}/{db}?sslmode=require"
                    for scheme in schemes for prefix in ("", "@", ":@")}
        if uri not in expected:
            scheme = next((candidate for candidate in schemes if uri.startswith(candidate)), None)
            authority = uri[len(scheme):].split("/", 1)[0] if scheme is not None else ""
            nonempty_userinfo = "@" in authority and (authority.count("@") != 1 or authority.split("@", 1)[0] not in ("", ":"))
            raise LedgerMetadataRejected("LEDGER_URI_USERINFO_REJECTED" if nonempty_userinfo
                                         else "LEDGER_URI_NONCANONICAL_REJECTED")
    for name in CONNECTION_NAMES:
        projected["database"].pop(name, None)
    return projected


class _ProposalNoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req: Any, fp: Any, code: int, msg: Any, headers: Any, newurl: Any) -> None:
        raise ProposalRejected("PROPOSAL_REDIRECT_REJECTED", code) from None


class AppSpecProposer:
    """One non-creating validation POST, with only the app:read credential.

    Kept separate from the GET-only provider and the installation adapter.
    DigitalOcean apps/propose validates a spec; no existing app ID is supplied.
    Private request/response bytes live only in memory and are never output.
    """
    def __init__(self, read_token: str):
        require(not any(name in os.environ for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR")),
                "proposal TLS environment override prohibited")
        self.__read_token = fixture._secret(read_token)
        context = ssl.create_default_context()
        require(context.check_hostname and context.verify_mode == ssl.CERT_REQUIRED
                and context.keylog_filename is None, "proposal TLS configuration differs")
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            _ProposalNoRedirect(), urllib.request.HTTPSHandler(context=context))
        self.attempted = False

    def propose(self, spec: dict[str, Any]) -> None:
        require(not self.attempted, "app spec proposal already attempted")
        self.attempted = True  # Uncertain transport is consumed, never retried.
        body = common.canonical_payload_bytes({"spec": spec})
        require(len(body) <= MAX_PROPOSAL_BYTES, "app spec proposal request exceeds bound")
        request = urllib.request.Request(PROPOSAL_URL, data=body, method="POST", headers={
            "Authorization": "Bearer " + self.__read_token, "Content-Type": "application/json",
            "Accept": "application/json"})
        status = None
        try:
            try:
                response = self.opener.open(request, timeout=30)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                status = response.getcode()
                if type(status) is not int or not 100 <= status <= 599:
                    raise ProposalRejected("PROPOSAL_RESPONSE_SHAPE_REJECTED")
                if response.geturl() != PROPOSAL_URL or 300 <= status < 400:
                    raise ProposalRejected("PROPOSAL_REDIRECT_REJECTED", status)
                raw = response.read(MAX_PROPOSAL_BYTES + 1)
            if type(raw) is not bytes or len(raw) > MAX_PROPOSAL_BYTES:
                raise ProposalRejected("PROPOSAL_RESPONSE_BOUND_REJECTED", status)
            try:
                value = common.loads_strict(raw)
            except Exception:
                raise ProposalRejected("PROPOSAL_RESPONSE_SHAPE_REJECTED", status) from None
            if status != 200:
                message = value.get("message") if type(value) is dict else None
                mentions = ()
                if type(message) is str:
                    mentions = tuple(sorted({code for literal, code in PROPOSAL_FIELDS.items()
                        if re.search(r"(?<![A-Za-z0-9_])" + re.escape(literal) + r"(?![A-Za-z0-9_])", message)}))
                raise ProposalRejected("PROPOSAL_HTTP_REJECTED", status, mentions)
            if type(value) is not dict or type(value.get("spec")) is not dict:
                raise ProposalRejected("PROPOSAL_RESPONSE_SHAPE_REJECTED", status)
            if value.get("app_name_available") is not True:
                raise ProposalRejected("PROPOSAL_NAME_UNAVAILABLE", status)
            returned = value["spec"]
            services = returned.get("services")
            if (returned.get("name") != spec["name"] or type(services) is not list or len(services) != 1
                    or type(services[0]) is not dict or services[0].get("name") != spec["services"][0]["name"]
                    or type(services[0].get("image")) is not dict
                    or services[0]["image"].get("digest") != spec["services"][0]["image"]["digest"]):
                raise ProposalRejected("PROPOSAL_RESPONSE_IDENTITY_REJECTED", status)
        except ProposalRejected:
            raise
        except Exception:
            raise ProposalRejected("PROPOSAL_TRANSPORT_FAILED", status) from None


def expected_app_owner_uuid(account: dict[str, Any], plan: dict[str, Any]) -> str:
    """The UUID a created driver app must be owned by.

    DigitalOcean sets a created app's `owner_uuid` to the account's *team* UUID,
    while `/v2/account` reports the authenticated *user* UUID that the plan
    pins. The ownership unit is therefore the team whenever the account response
    carries one, and the pinned account UUID otherwise.
    """
    team = account.get("team")
    if team is None:
        return plan["provider_account_uuid"]
    require(type(team) is dict, "provider team shape differs")
    team_uuid = team.get("uuid")
    if team_uuid is None:
        return plan["provider_account_uuid"]
    return common.require_uuid(team_uuid, "provider team")


def planned_owner_uuid(provider: Any) -> str:
    """The owner a created app must carry, once the account has been read.

    `preflight` records the ownership unit; before it runs - as in the
    create-only diagnostics - the pinned account UUID is the only known unit.
    """
    owner = getattr(provider, "owner_uuid", None)
    return provider.plan["provider_account_uuid"] if owner is None else owner


class ReadOnlyProvider:
    """Fixed GET allowlist only; no creation method or mutation credential."""
    def __init__(self, plan: dict[str, Any], read_token: str):
        self.plan = copy.deepcopy(plan)
        self.__read_token = fixture._secret(read_token)
        self.opener = fixture._opener()
        self.app_id: str | None = None
        self.deployment_id: str | None = None
        self.owner_uuid: str | None = None

    def get(self, path: str) -> Any:
        ledger = self.plan["ledger"]
        fixed = {"/v2/account", "/v2/apps/regions",
                 "/v2/apps/tiers/instance_sizes/" + self.plan["instance_size_slug"],
                 "/v2/databases/" + ledger["cluster_id"],
                 "/v2/databases/" + ledger["cluster_id"] + "/firewall",
                 "/v2/databases/" + ledger["cluster_id"] + "/dbs/" + ledger["database"]}
        if self.app_id is not None:
            fixed.add("/v2/apps/" + self.app_id)
            if self.deployment_id is not None:
                fixed.add("/v2/apps/" + self.app_id + "/deployments/" + self.deployment_id)
        require(path in fixed or re.fullmatch(r"/v2/apps\?page=[1-9][0-9]?&per_page=200", path) is not None,
                "bootstrap provider read route differs")
        raw = fixture._wire(self.opener, common.API_ORIGIN + path,
                            headers={"Authorization": "Bearer " + self.__read_token, "Accept": "application/json"},
                            maximum=MAX_PUBLIC)
        value = common.loads_strict(raw)
        if path == "/v2/databases/" + ledger["cluster_id"]:
            return project_ledger_metadata(value, ledger["cluster_id"])
        if path.startswith("/v2/databases/"):
            reject_database_credentials(value)
        return value

    def apps(self) -> list[dict[str, str]]:
        rows, total = [], None
        for page in range(1, 100):
            value = self.get(f"/v2/apps?page={page}&per_page=200")
            observed = common.exact_int(value.get("meta", {}).get("total"), "app inventory total", 0, 19800)
            chunk = value.get("apps")
            require(type(chunk) is list and len(chunk) <= 200 and total in (None, observed),
                    "app inventory moved")
            total = observed
            for app in chunk:
                rows.append({"id": common.require_uuid(app.get("id"), "app inventory identity"),
                             "name": common.exact_string(app.get("spec", {}).get("name"), "app inventory name", LABEL)})
            require(len(rows) <= total and len({r["id"] for r in rows}) == len(rows), "app inventory incomplete")
            if len(rows) == total:
                require(not value.get("links", {}).get("pages", {}).get("next"), "excess app page")
                return sorted(rows, key=lambda row: row["id"])
            require(len(chunk) > 0, "app inventory missing page")
        common.fail("app inventory exceeds bound")

    def preflight(self) -> tuple[list[dict[str, str]], list[dict[str, str]]]:
        p, ledger = self.plan, self.plan["ledger"]
        mark_stage("PROVIDER_ACCOUNT")
        account = self.get("/v2/account").get("account", {})
        require(account.get("uuid") == p["provider_account_uuid"] and account.get("status") == "active",
                "provider account differs")
        self.owner_uuid = expected_app_owner_uuid(account, p)
        mark_stage("PROVIDER_SIZE")
        size = self.get("/v2/apps/tiers/instance_sizes/" + p["instance_size_slug"]).get("instance_size", {})
        require(size.get("slug") == p["instance_size_slug"] and size.get("usd_per_month") == p["monthly_usd"],
                "driver size or current monthly cost differs")
        mark_stage("PROVIDER_REGION")
        regions = self.get("/v2/apps/regions").get("regions")
        require(type(regions) is list, "app region inventory missing")
        matches = [r for r in regions if r.get("slug") == p["region"] and r.get("disabled", False) is False]
        require(len(matches) == 1, "driver region differs")
        mark_stage("PROVIDER_LEDGER_METADATA")
        cluster = self.get("/v2/databases/" + ledger["cluster_id"]).get("database", {})
        require(cluster.get("id") == ledger["cluster_id"] and cluster.get("name") == ledger["cluster_name"]
                and cluster.get("engine") == "pg" and cluster.get("version") == ledger["version"]
                and cluster.get("status") == "online" and cluster.get("region") in matches[0].get("data_centers", []),
                "selected existing ledger metadata differs")
        mark_stage("PROVIDER_LEDGER_DATABASE")
        require(self.get("/v2/databases/" + ledger["cluster_id"] + "/dbs/" + ledger["database"])
                .get("db") == {"name": ledger["database"]}, "selected ledger database is not existing")
        # User existence/least privilege is independently reviewed in the
        # protected packet. No user-list, user-detail, credential or SQL API.
        mark_stage("PROVIDER_FIREWALL")
        rules = firewall_projection(self.get("/v2/databases/" + ledger["cluster_id"] + "/firewall"))
        require(common.sha256_value(rules) == ledger["firewall_sha256"], "ledger firewall prestate differs")
        mark_stage("PROVIDER_APPS")
        apps = self.apps()
        require(all(app["name"] != p["app_name"] for app in apps), "driver app already exists")
        return apps, rules


class _CreateNoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req: Any, fp: Any, code: int, msg: Any, headers: Any, newurl: Any) -> None:
        # urllib turns this into HTTPError; create then closes it without reading
        # its body. Never construct a redirected request containing the token.
        return None


def _create_opener() -> Any:
    require(not any(name in os.environ for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR")),
            "create TLS environment override prohibited")
    context = ssl.create_default_context()
    require(context.check_hostname and context.verify_mode == ssl.CERT_REQUIRED
            and context.keylog_filename is None, "create TLS configuration differs")
    return urllib.request.build_opener(urllib.request.ProxyHandler({}),
        _CreateNoRedirect(), urllib.request.HTTPSHandler(context=context))


class Provider(ReadOnlyProvider):
    """Install-only adapter: one app POST; no DB write/user API or app update."""
    def __init__(self, plan: dict[str, Any], read_token: str, create_token: str):
        # Reject TLS environment overrides before either opener initializes TLS.
        self.create_opener = _create_opener()
        super().__init__(plan, read_token)
        self.__create_token = fixture._secret(create_token)
        require(read_token != self.__create_token, "provider credential roles are not separate")
        self.created = False

    def create(self, spec: dict[str, Any]) -> tuple[str, str, dict[str, Any]]:
        require(not self.created, "app creation was already attempted")
        self.created = True  # Burn before transport, including timeout/HTTP error.
        try:
            request = urllib.request.Request(CREATE_URL, method="POST",
                headers={"Authorization": "Bearer " + self.__create_token,
                         "Accept": "application/json", "Content-Type": "application/json"},
                data=common.canonical_payload_bytes({"spec": spec}))
        except Exception:
            raise CreateRejected("CREATE_REQUEST_REJECTED") from None
        status = None
        try:
            try:
                response = self.create_opener.open(request, timeout=30)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                observed_status = response.getcode()
                if type(observed_status) is not int or not 100 <= observed_status <= 599:
                    raise CreateRejected("CREATE_RESPONSE_SHAPE_AMBIGUOUS")
                status = observed_status
                if response.geturl() != CREATE_URL or 300 <= status < 400:
                    raise CreateRejected("CREATE_REDIRECT_REJECTED", status)
                # Preserve the original success status/size acceptance. Only
                # rejected-response diagnostics use the smaller in-memory bound.
                accepted_status = status in (200, 201, 202, 204)
                maximum = MAX_PUBLIC if accepted_status else MAX_CREATE_ERROR_BYTES
                raw = response.read(maximum + 1)
            if type(raw) is not bytes or len(raw) > maximum:
                raise CreateRejected("CREATE_RESPONSE_BOUND_AMBIGUOUS", status)
            try:
                value = common.loads_strict(raw)
            except Exception:
                raise CreateRejected("CREATE_RESPONSE_SHAPE_AMBIGUOUS", status) from None
            if not accepted_status:
                message = value.get("message") if type(value) is dict else None
                mentions = ()
                if type(message) is str:
                    mentions = tuple(sorted({code for literal, code in CREATE_FIELDS.items()
                        if re.search(r"(?<![A-Za-z0-9_])" + re.escape(literal) + r"(?![A-Za-z0-9_])", message)}))
                raise CreateRejected("CREATE_HTTP_REJECTED", status, mentions)
            if type(value) is not dict or type(value.get("app", {})) is not dict:
                raise CreateRejected("CREATE_RESPONSE_SHAPE_AMBIGUOUS", status)
            app = value.get("app", {})
            try:
                self.app_id = common.require_uuid(app.get("id"), "created driver app")
                # Capture this create's associated deployment; never select a
                # later latest deployment to recover an ambiguous response.
                self.deployment_id = common.require_uuid(app.get("pending_deployment", {}).get("id"), "created driver deployment")
                require(app.get("owner_uuid") == planned_owner_uuid(self), "created driver owner differs")
            except Exception:
                raise CreateRejected("CREATE_RESPONSE_IDENTITY_AMBIGUOUS", status) from None
            return self.app_id, self.deployment_id, app
        except CreateRejected:
            raise
        except Exception:
            # A known HTTP status survives a later read/close/transport error.
            # Every attempted create is consumed; only later GET reconciliation
            # is allowed, never a retry, secret installation, update or cleanup.
            raise CreateRejected("CREATE_TRANSPORT_AMBIGUOUS", status) from None


def spec_projection(actual: Any, expected: dict[str, Any]) -> dict[str, Any]:
    """Strict public shape plus opaque initial secret ciphertext equality.

    The provider is trusted to store submitted secrets. Readback proves stable
    ciphertext/spec identity, not independent decryption or image measurement.
    """
    observed = copy.deepcopy(actual)
    require(type(observed) is dict, "driver spec missing")
    # Only explicitly benign empty API defaults may be added.
    for key in ("alerts", "domains", "envs", "features", "jobs", "workers", "static_sites", "functions"):
        if observed.get(key) == []:
            observed.pop(key)
    require(set(observed) == set(expected), "driver spec fields differ")
    require(type(observed.get("services")) is list and len(observed["services"]) == 1,
            "driver service inventory differs")
    service = observed["services"][0]
    expected_service = expected["services"][0]
    require(set(service) == set(expected_service), "driver service fields differ")
    envs = service.get("envs")
    require(type(envs) is list and len(envs) == len(expected_service["envs"]), "runtime variable inventory differs")
    by_key = {e.get("key"): e for e in envs if type(e) is dict}
    require(len(by_key) == len(envs), "duplicate runtime variable")
    for env in expected_service["envs"]:
        actual_env = by_key.get(env["key"])
        if type(actual_env) is dict and env["type"] == "GENERAL" and "type" not in actual_env:
            actual_env["type"] = "GENERAL"
        require(type(actual_env) is dict and set(actual_env) == set(env)
                and all(actual_env[k] == env[k] for k in ("key", "scope", "type")),
                "runtime variable custody differs")
        value = actual_env["value"]
        if env["type"] == "SECRET":
            require(type(value) is str and re.fullmatch(r"EV\[[^\r\n]{8,262144}\]", value),
                    "runtime variable storage differs")
        else:
            require(value == env["value"], "public runtime version differs")
    projection = copy.deepcopy(observed)
    projection["services"][0]["envs"] = copy.deepcopy(expected_service["envs"])
    require(projection == expected, "driver spec policy differs")
    service["envs"] = sorted(envs, key=lambda item: item["key"])
    return observed


def driver_origin(app: dict[str, Any], app_name: str) -> str:
    ingress = app.get("default_ingress")
    require(type(ingress) is str, "driver default domain missing")
    parsed = urllib.parse.urlsplit(ingress)
    host = parsed.hostname
    require(parsed.scheme == "https" and parsed.netloc == host and not parsed.path
            and not parsed.query and not parsed.fragment and type(host) is str
            and re.fullmatch(re.escape(app_name) + r"-[a-z0-9]+\.ondigitalocean\.app", host),
            "driver default domain differs")
    origin = "https://" + host
    require(app.get("live_url") in (origin, origin + "/") and app.get("live_domain") == host,
            "driver live URL differs")
    return origin


def health(origin: str) -> None:
    parsed = urllib.parse.urlsplit(origin)
    require(parsed.scheme == "https" and parsed.netloc == parsed.hostname and not parsed.path
            and not parsed.query and not parsed.fragment and parsed.hostname.endswith(".ondigitalocean.app"),
            "driver health origin differs")
    addresses = sorted({row[4][0] for row in socket.getaddrinfo(parsed.hostname, 443, type=socket.SOCK_STREAM)})
    require(0 < len(addresses) <= 16 and all(ipaddress.ip_address(ip).is_global for ip in addresses),
            "driver health address is not public")
    connection = http.client.HTTPSConnection(parsed.hostname, timeout=15, context=ssl.create_default_context())
    try:
        # Pin the already checked address while retaining hostname certificate
        # verification/SNI. Never send auth, follow redirects or call execute.
        connection.sock = connection._context.wrap_socket(
            socket.create_connection((addresses[0], 443), timeout=15), server_hostname=parsed.hostname)
        connection.request("GET", "/healthz", headers={"Host": parsed.hostname, "Accept": "*/*"})
        response = connection.getresponse()
        require(response.status == 204 and response.read(1) == b"", "driver health did not pass")
    finally:
        connection.close()


class EnvironmentReader:
    """Environment metadata checks only; no secret installation method."""
    def __init__(self, token: str, expected_metadata_sha256: str):
        self.api = fixture.GitHubRead(fixture._secret(token))
        self.expected_metadata_sha256 = common.require_sha256(expected_metadata_sha256, "environment metadata digest")
        self.initial: Any = None

    def snapshot(self) -> dict[str, Any]:
        path = fixture.API_PREFIX + "/environments/" + ENVIRONMENT
        env = self.api.get(path)
        require(env.get("name") == ENVIRONMENT
                and env.get("deployment_branch_policy") == {"protected_branches": False, "custom_branch_policies": True},
                "canary environment protection differs")
        policies = self.api.pages(path + "/deployment-branch-policies", "branch_policies")
        require(policies["total_count"] == 1 and len(policies["branch_policies"]) == 1
                and policies["branch_policies"][0].get("name") == "main"
                and policies["branch_policies"][0].get("type") == "branch", "canary environment is not main-only")
        snapshot = {"environment": {key: env.get(key) for key in (
            "id", "name", "protection_rules", "deployment_branch_policy")},
            "branch_policies": policies["branch_policies"]}
        for kind in ("secrets", "variables"):
            rows, total = [], None
            for page in range(1, 11):
                result = self.api.get(path + f"/{kind}?per_page=100&page={page}")
                count = common.exact_int(result.get("total_count"), "environment metadata count", 0, 1000)
                chunk = result.get(kind)
                require(total in (None, count) and type(chunk) is list and len(chunk) <= 100,
                        "environment metadata inventory differs")
                total = count
                rows.extend(chunk)
                for row in rows:
                    common.exact_keys(row, {"name", "created_at", "updated_at"} | ({"value"} if kind == "variables" else set()),
                                      "environment metadata record")
                names = [row["name"] for row in rows]
                require(len(names) == len(set(names)) and all(type(name) is str for name in names)
                        and len(rows) <= total, "environment metadata is ambiguous")
                if len(rows) == total:
                    break
                require(chunk, "environment metadata inventory incomplete")
            else:
                common.fail("environment metadata inventory exceeds bound")
            snapshot[kind] = sorted(rows, key=lambda row: row["name"])
        return snapshot

    def require_absent(self) -> None:
        value = self.snapshot()
        require({row["name"] for row in value["secrets"]}
                == {"CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY"},
                "canary preexisting secret names differ")
        require(common.sha256_value(value) == self.expected_metadata_sha256,
                "reviewed environment metadata differs")
        if self.initial is None:
            self.initial = value
        require(value == self.initial, "environment metadata changed before installation")


class EnvironmentWriter(EnvironmentReader):
    def __init__(self, token: str, gh: Path, expected_metadata_sha256: str):
        super().__init__(token, expected_metadata_sha256)
        self.__token = fixture._secret(token)
        self.gh = gh
        self.attempted = False

    def install(self, config: dict[str, Any]) -> None:
        require(not self.attempted, "environment installation was already attempted")
        common.exact_keys(config, {"schema_version", "url", "driver_version_sha256", "fixture_descriptor_sha256", "hmac_key_base64"},
                          "driver configuration")
        self.require_absent()
        public_key_path = fixture.API_PREFIX + "/environments/" + ENVIRONMENT + "/secrets/public-key"
        public_key = self.api.get(public_key_path)
        common.exact_keys(public_key, {"key_id", "key"}, "environment public key")
        require(type(public_key["key_id"]) is str and re.fullmatch(r"[0-9]+", public_key["key_id"])
                and len(base64.b64decode(public_key["key"], validate=True)) == 32, "environment public key differs")
        # Pinned gh v2.98.0 setSecret returns BEFORE putEnvSecret when
        # --no-store is set. It seals stdin locally; no mutation is delegated.
        # github.com/cli/cli/blob/v2.98.0/pkg/cmd/secret/set/set.go#L328-L330
        result = subprocess.run([str(self.gh), "secret", "set", SECRET_NAME, "--env", ENVIRONMENT,
                                 "--repo", common.REPOSITORY, "--no-store"],
                                input=common.canonical_payload_bytes(config), env=subprocess_environment(token=self.__token),
                                capture_output=True, timeout=90, check=False)
        require(result.returncode == 0 and len(result.stdout) <= 16384, "environment secret sealing failed")
        encrypted = result.stdout.strip().decode("ascii")
        require(len(base64.b64decode(encrypted, validate=True)) == len(common.canonical_payload_bytes(config)) + 48,
                "environment sealed value length differs")
        require(self.api.get(public_key_path) == public_key, "environment encryption key changed")
        self.require_absent()
        self.attempted = True  # Burn before exactly one explicit, nonretry PUT.
        fixture._wire(fixture._opener(), "https://api.github.com" + fixture.API_PREFIX
                      + "/environments/" + ENVIRONMENT + "/secrets/" + SECRET_NAME, method="PUT",
                      headers={"Authorization": "Bearer " + self.__token, "Accept": "application/vnd.github+json",
                               "Content-Type": "application/json", "X-GitHub-Api-Version": "2022-11-28"},
                      body=common.canonical_payload_bytes({"key_id": public_key["key_id"], "encrypted_value": encrypted}),
                      maximum=4096)
        after = self.snapshot()
        additions = [row for row in after["secrets"] if row["name"] == SECRET_NAME]
        require(len(additions) == 1, "environment installation metadata absent")
        after["secrets"] = [row for row in after["secrets"] if row["name"] != SECRET_NAME]
        require(after == self.initial, "environment metadata changed during installation")
        # GitHub intentionally provides no secret-value readback. Successful
        # encrypted write plus metadata is not a runtime synthetic probe.


def observe_created(provider: Any, spec: dict[str, Any], initial_spec: dict[str, Any],
                    before_apps: list[dict[str, str]], before_rules: list[dict[str, str]]) -> str | None:
    app_id, deployment_id = provider.app_id, provider.deployment_id
    app = provider.get("/v2/apps/" + app_id).get("app", {})
    dep = provider.get("/v2/apps/" + app_id + "/deployments/" + deployment_id).get("deployment", {})
    require(app.get("id") == app_id
            and app.get("owner_uuid") == planned_owner_uuid(provider)
            and dep.get("id") == deployment_id, "created driver identity changed")
    require(spec_projection(app.get("spec"), spec) == initial_spec, "created driver spec changed")
    phase = dep.get("phase")
    require(phase in ("PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING", "ACTIVE"),
            "created driver deployment did not progress safely")
    if phase != "ACTIVE":
        return None
    require(app.get("active_deployment", {}).get("id") == deployment_id
            and spec_projection(dep.get("spec"), spec) == initial_spec,
            "active driver deployment differs")
    for key in ("in_progress_deployment", "pending_deployment", "pinned_deployment"):
        value = app.get(key)
        require(not value or (value.get("id") == deployment_id and value.get("phase") == "ACTIVE"),
                "competing driver deployment exists")
    expected_apps = sorted(before_apps + [{"id": app_id, "name": provider.plan["app_name"]}], key=lambda a: a["id"])
    require(provider.apps() == expected_apps, "app inventory after creation differs")
    rules = firewall_projection(provider.get("/v2/databases/" + provider.plan["ledger"]["cluster_id"] + "/firewall"))
    expected_rules = sorted(before_rules + [{"type": "app", "value": app_id}], key=lambda r: (r["type"], r["value"]))
    require(rules == expected_rules, "automatic ledger admission was not exactly observed")
    return driver_origin(app, provider.plan["app_name"])


class ReadOnlyProductTransport:
    """Authentication sessions and resource GETs only; no fixture writes."""
    def __init__(self, meta_token: str):
        self.__transport = fixture.ProductHTTP(meta_token)

    def login(self, email: str, password: str) -> str:
        return self.__transport.login(email, password)

    def request(self, method: str, path: str, body: Any = None, *, session: Any = None,
                organization_id: str | None = None, headers: Any = None, graph: bool = False) -> Any:
        require(method == "GET" and body is None, "bootstrap fixture mutation prohibited")
        return self.__transport.request(method, path, None, session=session,
            organization_id=organization_id, headers=headers, graph=graph)


def preflight(a: dict[str, Any], d: dict[str, Any], protected: Any, *, provider: Any, reader: Any,
              authenticate_fixture: Any, authenticate_image: Any, current_guard: Any,
              rehydrate: Any, transport: Any,
              now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> tuple[dict[str, Any], Any, Any]:
    """The entire shared pre-create path; the private spec never becomes output.

    Injected boundaries are tests only. Both CLI modes bind the same read-only
    evidence and fixture path, including the second provider/environment CAS.
    """
    mark_stage("AUTHORIZATION")
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    mark_stage("PRIVATE_INPUTS")
    validate_descriptor(d, a)
    mark_stage("CURRENT_AUTHORITY")
    current_guard()
    mark_stage("ENVIRONMENT_PRESTATE")
    reader.require_absent()
    # Genuine signature/artifact checks must finish before any fixture login.
    mark_stage("FIXTURE_AUTHENTICATION")
    raw = authenticate_fixture()
    require(common.sha256_bytes(raw) == a["fixture_evidence"]["result_sha256"], "authenticated fixture file differs")
    result = fixture.validate_terminal_result(common.loads_strict(raw))
    require(result["fixture_descriptor_sha256"] == d["fixture_descriptor_sha256"], "fixture runtime authority differs")
    mark_stage("IMAGE_AUTHENTICATION")
    authenticate_image()
    mark_stage("FIXTURE_INPUT_VALIDATION")
    request = {key: result[key] for key in fixture.REQUEST_KEYS}
    checked = fixture.validate_protected_input(protected, request)
    before_apps, before_rules = provider.preflight()
    mark_stage("FIXTURE_REHYDRATION")
    reconstructed = rehydrate(request, checked, result, transport)
    mark_stage("RUNTIME_SPEC")
    spec = runtime_spec(a, d, checked, reconstructed)
    mark_stage("CURRENT_AUTHORITY")
    current_guard()
    mark_stage("AUTHORIZATION")
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    mark_stage("ENVIRONMENT_PRESTATE")
    reader.require_absent()
    repeated_prestate = provider.preflight()
    mark_stage("SECOND_CAS")
    require(repeated_prestate == (before_apps, before_rules), "driver prestate changed before creation")
    mark_stage("CURRENT_AUTHORITY")
    current_guard()
    mark_stage("AUTHORIZATION")
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    return spec, before_apps, before_rules


def check_once(a: dict[str, Any], d: dict[str, Any], protected: Any, *, provider: Any, reader: Any, proposer: Any,
               authenticate_fixture: Any, authenticate_image: Any, current_guard: Any,
               rehydrate: Any, transport: Any,
               now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> dict[str, Any]:
    require(not hasattr(provider, "create") and not hasattr(reader, "install")
            and not any(hasattr(proposer, name) for name in ("create", "install", "update", "delete", "get")),
            "check received mutation-capable adapters")
    spec, before_apps, before_rules = preflight(a, d, protected, provider=provider, reader=reader,
        authenticate_fixture=authenticate_fixture, authenticate_image=authenticate_image,
        current_guard=current_guard, rehydrate=rehydrate, transport=transport, now=now)
    try:
        mark_stage("PROPOSE_APP")
        proposer.propose(spec)
    finally:
        # Reconcile even on rejection, timeout or malformed output. A proposal
        # result cannot override state/authority drift, and never falls through
        # to installation. These are the same narrowly scoped read adapters.
        repeated_prestate = provider.preflight()
        mark_stage("PROPOSAL_POSTSTATE")
        require(repeated_prestate == (before_apps, before_rules), "driver prestate changed during proposal")
        mark_stage("ENVIRONMENT_PRESTATE")
        reader.require_absent()
        mark_stage("CURRENT_AUTHORITY")
        current_guard()
        mark_stage("AUTHORIZATION")
        validate_authorization(a, control_sha=a["control_sha"], now=now())
    mark_stage("CHECK_COMPLETE")
    return {"schema_version": 1, "state": "private-driver-prerequisites-verified", "mutation_performed": False,
            "app_spec_proposal": "accepted"}


def install_once(a: dict[str, Any], d: dict[str, Any], protected: Any, *, provider: Any, writer: Any,
                 authenticate_fixture: Any, authenticate_image: Any, current_guard: Any,
                 rehydrate: Any, transport: Any, sleep: Any = time.sleep, probe: Any = health,
                 now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> dict[str, Any]:
    """One mutation sequence, preceded by exactly the check mode's preflight."""
    spec, before_apps, before_rules = preflight(a, d, protected, provider=provider, reader=writer,
        authenticate_fixture=authenticate_fixture, authenticate_image=authenticate_image,
        current_guard=current_guard, rehydrate=rehydrate, transport=transport, now=now)
    mark_stage("CREATE_APP")
    _, _, app = provider.create(spec)
    initial = spec_projection(app.get("spec"), spec)
    origin = None
    for _ in range(30):
        mark_stage("AUTHORIZATION")
        validate_authorization(a, control_sha=a["control_sha"], now=now())
        mark_stage("OBSERVE_CREATED")
        origin = observe_created(provider, spec, initial, before_apps, before_rules)
        if origin is not None:
            break
        sleep(10)
    require(origin is not None, "driver did not become ACTIVE within bound")
    mark_stage("HEALTH")
    probe(origin)
    sleep(30)
    mark_stage("CURRENT_AUTHORITY")
    current_guard()
    mark_stage("AUTHORIZATION")
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    mark_stage("STABLE_HEALTH")
    require(observe_created(provider, spec, initial, before_apps, before_rules) == origin,
            "driver was not stably ACTIVE")
    probe(origin)
    mark_stage("CURRENT_AUTHORITY")
    current_guard()
    mark_stage("AUTHORIZATION")
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    mark_stage("INSTALL_ENVIRONMENT")
    writer.install({"schema_version": 1, "url": origin + "/v1/execute",
                    "driver_version_sha256": a["driver_evidence"]["driver_version_sha256"],
                    "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
                    "hmac_key_base64": d["hmac_key_base64"]})
    mark_stage("FINAL_AUTHORITY")
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    return public_receipt(a, provider.app_id, provider.deployment_id, origin)


def current_guard(api: Any, root: Path, mode: str = "install") -> str:
    sha = fixture._current_guard(api, root, workflow=WORKFLOW)
    branch = api.get(fixture.API_PREFIX + "/branches/main")
    require(branch.get("protected") is True and branch.get("commit", {}).get("sha") == sha,
            "bootstrap main is not protected")
    require_unique_run(api, sha, common.require_run_id(os.environ.get("GITHUB_RUN_ID"), "bootstrap run"), mode)
    return sha


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("validate", "check", "install"))
    parser.add_argument("--control-root", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args(argv)
    try:
        mark_stage("INITIALIZATION")
        # Remove private values before any subprocess/imported adapter can run.
        private = {name: os.environ.pop(name, None) for name in PRIVATE_NAMES}
        mark_stage("MODE")
        mode = os.environ.get(MODE_ENV)
        require(mode in MODES and (args.command == "validate" or args.command == mode),
                "bootstrap command mode differs")
        mark_stage("AUTHORIZATION")
        raw = os.environ.get("BOOTSTRAP_AUTHORIZATION_JSON")
        require(type(raw) is str and len(raw.encode()) <= 32768, "public authorization missing")
        sha = common.require_sha1(os.environ.get("CONTROL_SHA"), "bootstrap control")
        a = validate_authorization(common.loads_strict(raw), control_sha=sha, now=dt.datetime.now(dt.timezone.utc))
        require(raw.encode() == common.canonical_payload_bytes(a), "public authorization is not canonical")
        token = fixture._secret(os.environ.get("GH_TOKEN"))
        mark_stage("PRIVATE_INPUTS")
        if args.command in MODES:
            required_names = CHECK_PRIVATE_NAMES if mode == "check" else PRIVATE_NAMES
            require(all(type(private[n]) is str and private[n] for n in required_names), "protected setup inputs missing")
            if mode == "check":
                require(all(private[n] is None for n in PRIVATE_NAMES if n not in CHECK_PRIVATE_NAMES),
                        "check received mutation credentials")
            require(len(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"].encode()) <= 32768
                    and len(private["CRM_CANARY_FIXTURE_INPUT_JSON"].encode()) <= fixture.MAX_BODY_BYTES,
                    "protected setup input exceeds bound")
            d = validate_descriptor(common.loads_strict(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"]), a)
            protected = common.loads_strict(private["CRM_CANARY_FIXTURE_INPUT_JSON"])
            if mode == "install":
                require(args.output_dir is not None, "public receipt output missing")
                output = args.output_dir.resolve()
                runner_temp = Path(os.environ["RUNNER_TEMP"]).resolve(strict=True)
                require(output.parent == runner_temp and not output.exists(), "public receipt location differs")
            else:
                require(args.output_dir is None, "check cannot write a public receipt")
        else:
            require(not any(private.values()) and args.output_dir is None, "public validation received private authority")
        root = args.control_root.resolve(strict=True)
        api = GitHubRead(token)
        mark_stage("CURRENT_AUTHORITY")
        current_guard(api, root, mode)
        if args.command == "validate":
            print(common.canonical_payload_bytes({"state": "public-authority-validated",
                                                 "authorization_sha256": common.sha256_value(a)}).decode())
            return 0
        # Import the new public verifier, not the pinned producer's
        # verify_fixture_result wrapper (which requires an existing runtime).
        try:
            from . import launch_production_prerequisites as prerequisites
        except ImportError:
            import launch_production_prerequisites as prerequisites
        mark_stage("PINNED_CLI")
        gh = fixture._pinned_gh()
        mark_stage("PUBLIC_EVIDENCE_READER")
        evidence = prerequisites.GitHubEvidence(root, transport=prerequisites.GitHubGetOnly(gh=str(gh)))
        mark_stage("ADAPTERS")
        transport = ReadOnlyProductTransport(protected["credentials"]["meta_access_token"])
        shared = dict(
            authenticate_fixture=lambda: evidence.authenticate_public_fixture(common.canonical_payload_bytes(a["fixture_evidence"]), sha),
            authenticate_image=lambda: authenticate_driver(api, root, gh, a["driver_evidence"], sha, gh_token=token),
            current_guard=lambda: current_guard(api, root, mode), rehydrate=fixture.rehydrate, transport=transport)
        if mode == "check":
            provider = ReadOnlyProvider(d["plan"], private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"])
            reader = EnvironmentReader(token, d["plan"]["github_environment_sha256"])
            proposer = AppSpecProposer(private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"])
            checked = check_once(a, d, protected, provider=provider, reader=reader, proposer=proposer, **shared)
            print(common.canonical_payload_bytes(checked).decode())
            return 0
        provider = Provider(d["plan"], private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"], private["DO_DRIVER_BOOTSTRAP_CREATE_TOKEN"])
        writer = EnvironmentWriter(private["GH_CANARY_ENVIRONMENT_WRITE_TOKEN"], gh, d["plan"]["github_environment_sha256"])
        receipt = install_once(a, d, protected, provider=provider, writer=writer, **shared)
        mark_stage("RECEIPT_WRITE")
        output.mkdir(mode=0o700, parents=False, exist_ok=False)
        receipt_bytes = common.canonical_file_bytes(receipt)
        (output / "receipt.json").write_bytes(receipt_bytes)
        (output / "receipt.sha256").write_text(common.sha256_bytes(receipt_bytes) + "\n", encoding="ascii")
        print(common.canonical_payload_bytes(receipt).decode())
        return 0
    except Exception as error:
        # Never include exception/provider/CLI text: it may contain private data.
        print(common.canonical_payload_bytes(failure_report(error)).decode(), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
