#!/usr/bin/env python3
"""Protected one-purpose repair of the existing CRM canary driver's two logins.

The existing driver app has never become ACTIVE. Its original installation
(control eae99c5d) wrote CRM_CANARY_KLINIK_LOGIN_JSON and
CRM_CANARY_NON_KLINIK_LOGIN_JSON as {email,password}; the unchanged driver
requires {email,password,schema_version:1} and exits at startup CONFIG. The
2026-09-29 owner-authorized update already moved the image and GENERAL version
to the attested publisher evidence pinned below, so exactly the two login values
remain wrong. Their plaintext exists only in CRM_CANARY_FIXTURE_INPUT_JSON.

plan:  read-only, and holds only the read-only DigitalOcean token. Authenticates
       origin lineage, fixture custody, the image evidence against this control
       and the driver app, then proves that the change set is exactly the two
       login values. No provider or GitHub write.
apply: the same checks, then a pre-mutation proof that the update token sees
       byte-for-byte the same driver spec as the read token and that the canary
       write token can read its environment; then one PUT carrying every other
       value unchanged, ACTIVE plus two /healthz 204 observations, then one
       installation of CRM_CANARY_SYNTHETIC_DRIVER_JSON. Within a run there is
       no retry, create, redeploy or delete; any further attempt is a separate,
       separately approved dispatch.

Continuation: if the driver is already ACTIVE at the pinned image (only possible
with corrected logins, which the startup CONFIG stage enforces) the PUT is
skipped; if the canary configuration is also already installed, the run only
re-proves health and writes its receipt. A stop after the PUT is therefore
completed by a later approved apply rather than requiring manual cleanup.

Deliberately not inherited from the historical recovery kernel: production-app
raw spec digests, exact deployment counts and timestamps, and frozen failed-run
tables. The production app is never read or written here. Every run is gated by
the fixture environment's required reviewer. The repository is public, so every
report is content-free: no identifier, hostname, spec or value is ever printed.
"""
from __future__ import annotations

import argparse
import copy
import datetime as dt
import os
from pathlib import Path
import re
import sys
import time
import urllib.error
import urllib.request
from typing import Any

try:
    from . import run_existing_crm_canary_driver_recovery as recovery
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import run_existing_crm_canary_driver_recovery as recovery

policy = recovery.policy
boot, common, fixture = policy.boot, policy.common, policy.boot.fixture

WORKFLOW = ".github/workflows/repair-production-crm-canary-driver-logins.yml"
KIND = "production-crm-canary-driver-login-repair-v1"
# Publisher run 36583000843 at fa62f72c. The 2026-09-29 update installed exactly
# this digest and version; authenticate_driver re-proves at the current control
# that the driver inputs are unchanged, so no new image is published or deployed.
TARGET_DRIVER_EVIDENCE = {
    "artifact_digest": "sha256:48fd50178f11892dbc03caee6ea30bbb5cfef5d5ee5480bf016d8795d9be0fe6",
    "artifact_id": "11041211487",
    "control_sha": "fa62f72c4d0e94ef2283b5f839715ac4bde6f8b0",
    "digest": "sha256:dbed5ddc60ea4f57729f0b0cd84c480f1a38fa88a9bd53a3561f5dd5a8655d7c",
    "driver_version_sha256": "9b87716e51624e0b39f37b14c049e82fa9849ea9100ded36d0724e3fc2da0f11",
    "run_id": "36583000843",
}
# The fixture input has not been written since it was provisioned, so it is the
# same custody the original installation read on 2026-09-21.
FIXTURE_INPUT_NAME = "CRM_CANARY_FIXTURE_INPUT_JSON"
FIXTURE_INPUT_UPDATED_AT = "2026-09-14T17:38:09Z"
LOGIN_KEYS = policy.LOGIN_KEYS
LOGIN_SOURCES = {"CRM_CANARY_KLINIK_LOGIN_JSON": "klinik", "CRM_CANARY_NON_KLINIK_LOGIN_JSON": "non_klinik"}
VERSION_KEY = "CRM_CANARY_DRIVER_VERSION_SHA256"
LEDGER_KEY = "CRM_CANARY_LEDGER_DATABASE_URL"
SECRET_KEYS = LOGIN_KEYS | {"CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "CRM_CANARY_HMAC_KEY_BASE64",
                            "CRM_CANARY_META_APP_SECRET"}
ENV_KEYS = SECRET_KEYS | {VERSION_KEY, LEDGER_KEY}
CIPHERTEXT = re.compile(r"EV\[[!-Z^-~]{8,8192}\]")
SLOTS = ("active_deployment", "pending_deployment", "in_progress_deployment", "pinned_deployment")
FAILED_PHASES = frozenset({"ERROR", "CANCELED"})
PHASE_RANK = {"PENDING_BUILD": 0, "BUILDING": 1, "PENDING_DEPLOY": 2, "DEPLOYING": 3, "ACTIVE": 4}
NONTERMINAL = frozenset(PHASE_RANK) - {"ACTIVE"}
MAX_DEPLOYMENTS = 50
POLL_LIMIT = 150
POLL_SECONDS = 10
HEALTH_LIMIT = 30
READ_MISSES = 3
CANARY_BASE_NAMES = frozenset({"CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY"})
PRIVATE_NAMES = frozenset({FIXTURE_INPUT_NAME, "CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "DO_DRIVER_BOOTSTRAP_READ_TOKEN",
                           "DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"})
PLAN_PRIVATE_NAMES = PRIVATE_NAMES - {"DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"}
# Runner-provided OIDC request material is only for the later attest steps.
INHERITED_CREDENTIALS = ("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL")
STAGES = frozenset({"AUTHORIZATION", "CURRENT_CONTROL", "ORIGIN_AUTHORITY", "FIXTURE_CUSTODY",
                    "IMAGE_AUTHORITY", "CANARY_ENVIRONMENT", "DRIVER_PRESTATE", "PLAN", "PRE_UPDATE",
                    "UPDATE_APP", "DEPLOYMENT_READBACK", "HEALTH", "CANARY_INSTALL", "POSTSTATE"})
CODES = frozenset({"MODE", "PRIVATE_INPUTS", "OUTPUT", "ORIGIN_PACKET", "FIXTURE_INPUT_CHANGED",
                   "LOGIN_SHAPE", "APPS_INVENTORY", "DRIVER_SELECTION", "DRIVER_IDENTITY",
                   "DRIVER_NOT_IDLE", "DRIVER_HISTORY", "SPEC_IDENTITY", "SPEC_SERVICE", "SPEC_IMAGE",
                   "SPEC_ENV", "SPEC_DATABASE", "PLAN_DELTA", "PRESTATE_MOVED", "UPDATE_REJECTED",
                   "UPDATE_RESPONSE", "DEPLOYMENT_ERROR", "DEPLOYMENT_CANCELED", "DEPLOYMENT_PHASE",
                   "DEPLOYMENT_SPEC", "DEPLOYMENT_TIMEOUT", "HEALTH", "POSTSTATE", "TOKEN_VIEW",
                   "ACTIVE_SPEC", "CANARY_STATE", "WRITER_PREFLIGHT"})
STAGE = "AUTHORIZATION"


class Stopped(common.ReleaseError):
    """Carries only a fixed source-literal code, never a received message."""
    def __init__(self, code: str):
        super().__init__("driver login repair stopped")
        self.code = code if code in CODES else None


def check(ok: Any, code: str) -> None:
    if not ok:
        raise Stopped(code)


def mark(stage: str) -> None:
    global STAGE
    if stage not in STAGES:
        raise Stopped("MODE")
    STAGE = stage


def diff_paths(left: Any, right: Any, path: str = "spec") -> list[str]:
    if type(left) is not type(right):
        return [path]
    if type(left) is dict:
        out: list[str] = []
        for key in sorted(set(left) | set(right)):
            if key not in left or key not in right:
                out.append(path + "." + key)
            else:
                out.extend(diff_paths(left[key], right[key], path + "." + key))
        return out
    if type(left) is list:
        if len(left) != len(right):
            return [path]
        out = []
        for index, (a, b) in enumerate(zip(left, right)):
            out.extend(diff_paths(a, b, path + "[" + str(index) + "]"))
        return out
    return [] if left == right else [path]


def login_value(protected: Any, source: str) -> str:
    """Exactly the corrected current-generator shape (bootstrap runtime_spec)."""
    check(type(protected) is dict and type(protected.get("registration")) is dict
          and type(protected.get("credentials")) is dict, "LOGIN_SHAPE")
    email = protected["registration"].get(source + "_email")
    password = protected["credentials"].get(source + "_password")
    # The driver's own acceptance rules (frontend/canary-driver/runner.mjs validateLogin).
    check(type(email) is str and re.fullmatch(r"[!-~]{1,254}", email) and "@" in email
          and type(password) is str and re.fullmatch(r"[ -~]{8,256}", password), "LOGIN_SHAPE")
    raw = common.canonical_payload_bytes({"schema_version": 1, "email": email, "password": password}).decode()
    policy._private_login(raw, versioned=True)
    return raw


def corrected_logins(protected: Any) -> dict[str, str]:
    return {key: login_value(protected, source) for key, source in LOGIN_SOURCES.items()}


def select_driver(apps: Any, app_name: str) -> str:
    """Complete bounded inventory; exactly one app with the pinned identity."""
    check(type(apps) is dict and type(apps.get("apps")) is list, "APPS_INVENTORY")
    rows = apps["apps"]
    meta, links = apps.get("meta"), apps.get("links") or {}
    check(type(meta) is dict and meta.get("total") == len(rows) and 0 < len(rows) <= 200
          and type(links) is dict and not (links.get("pages") or {}).get("next"), "APPS_INVENTORY")
    identities = [row.get("id") for row in rows if type(row) is dict]
    check(len(identities) == len(rows) and all(type(i) is str for i in identities)
          and len(set(identities)) == len(identities), "APPS_INVENTORY")
    matches = [row for row in rows if common.sha256_bytes(row["id"].encode("ascii"))
               == policy.IDENTITY_PINS["app_id_sha256"]]
    check(len(matches) == 1 and type(matches[0].get("spec")) is dict
          and matches[0]["spec"].get("name") == app_name, "DRIVER_SELECTION")
    return common.require_uuid(matches[0]["id"], "driver app")


def validate_spec(spec: Any, d: dict[str, Any]) -> None:
    p, ledger = d["plan"], d["plan"]["ledger"]
    check(type(spec) is dict and spec.get("name") == p["app_name"] and spec.get("region") == p["region"]
          and not any(spec.get(key) for key in ("workers", "jobs", "static_sites", "functions")), "SPEC_IDENTITY")
    services = spec.get("services")
    check(type(services) is list and len(services) == 1 and type(services[0]) is dict, "SPEC_SERVICE")
    service = services[0]
    check(service.get("name") == "driver" and service.get("instance_count") == 1
          and service.get("instance_size_slug") == p["instance_size_slug"] and service.get("http_port") == 8080
          and service.get("health_check") == {"http_path": "/healthz", "port": 8080}, "SPEC_SERVICE")
    image = service.get("image")
    check(type(image) is dict and image.get("registry_type") == "GHCR"
          and image.get("registry") == "medtechcorps-netizen" and image.get("repository") == "rereply-crm-canary-driver"
          and image.get("digest") == TARGET_DRIVER_EVIDENCE["digest"], "SPEC_IMAGE")
    envs = service.get("envs")
    check(type(envs) is list and all(type(row) is dict and type(row.get("key")) is str for row in envs)
          and len(envs) == len(ENV_KEYS) and {row["key"] for row in envs} == ENV_KEYS, "SPEC_ENV")
    for row in envs:
        check(row.get("scope") == "RUN_TIME" and type(row.get("value")) is str, "SPEC_ENV")
        if row["key"] in SECRET_KEYS:
            check(row.get("type") == "SECRET" and CIPHERTEXT.fullmatch(row["value"]), "SPEC_ENV")
        else:
            check(row.get("type", "GENERAL") == "GENERAL", "SPEC_ENV")
    values = {row["key"]: row["value"] for row in envs}
    check(values[VERSION_KEY] == TARGET_DRIVER_EVIDENCE["driver_version_sha256"]
          and values[LEDGER_KEY] == boot.LEDGER_EXPRESSION, "SPEC_ENV")
    databases = spec.get("databases")
    check(type(databases) is list and len(databases) == 1 and type(databases[0]) is dict
          and {key: databases[0].get(key) for key in ("name", "engine", "cluster_name", "db_name", "db_user")}
          == {"name": "ledger", "engine": "PG", "cluster_name": ledger["cluster_name"],
              "db_name": ledger["database"], "db_user": ledger["user"]}, "SPEC_DATABASE")


def validate_driver_state(app: Any, deployments: Any, d: dict[str, Any]) -> tuple[set[str], str | None]:
    """Either idle with only failed history, or ACTIVE on exactly one deployment.

    Returns the deployment identities and the ACTIVE identity, if any. ACTIVE at
    the pinned image and version implies corrected logins: the driver's startup
    CONFIG stage rejects the historical unversioned shape before it can listen.
    """
    check(type(app) is dict and type(app.get("id")) is str and type(app.get("owner_uuid")) is str
          and common.sha256_bytes(app["id"].encode("ascii")) == policy.IDENTITY_PINS["app_id_sha256"], "DRIVER_IDENTITY")
    common.require_uuid(app["owner_uuid"], "driver owner")
    check(not any(app.get(slot) for slot in SLOTS if slot != "active_deployment"), "DRIVER_NOT_IDLE")
    active = app.get("active_deployment")
    check(not active or (type(active) is dict and active.get("phase") == "ACTIVE"), "DRIVER_NOT_IDLE")
    active_id = common.require_uuid(active.get("id"), "driver active deployment") if active else None
    check(type(deployments) is dict and type(deployments.get("deployments")) is list, "DRIVER_HISTORY")
    rows = deployments["deployments"]
    meta, links = deployments.get("meta"), deployments.get("links") or {}
    check(type(meta) is dict and meta.get("total") == len(rows) and 0 < len(rows) <= MAX_DEPLOYMENTS
          and type(links) is dict and not (links.get("pages") or {}).get("next"), "DRIVER_HISTORY")
    identities = set()
    for row in rows:
        check(type(row) is dict, "DRIVER_HISTORY")
        identity = common.require_uuid(row.get("id"), "driver deployment")
        check(row.get("phase") == "ACTIVE" if identity == active_id else row.get("phase") in FAILED_PHASES,
              "DRIVER_HISTORY")
        identities.add(identity)
    check(len(identities) == len(rows) and (active_id is None or active_id in identities)
          and {policy.IDENTITY_PINS["failed_deployment_id_sha256"], policy.IDENTITY_PINS["redeployed_deployment_id_sha256"]}
          <= {common.sha256_bytes(i.encode("ascii")) for i in identities}, "DRIVER_HISTORY")
    validate_spec(app.get("spec"), d)
    return identities, active_id


def build_proposal(spec: dict[str, Any], logins: dict[str, str]) -> dict[str, Any]:
    """Current spec with exactly the two login values replaced; all else carried."""
    check(set(logins) == LOGIN_KEYS, "PLAN_DELTA")
    proposal = copy.deepcopy(spec)
    expected = []
    for index, row in enumerate(proposal["services"][0]["envs"]):
        if row["key"] in LOGIN_KEYS:
            check(logins[row["key"]] != row["value"], "PLAN_DELTA")
            row["value"] = logins[row["key"]]
            expected.append("spec.services[0].envs[" + str(index) + "].value")
    check(len(expected) == 2 and diff_paths(spec, proposal) == expected, "PLAN_DELTA")
    return proposal


def require_carried(before: dict[str, Any], after: Any, code: str, sent: dict[str, str]) -> None:
    """Only the two login values may differ: each is a NEW provider ciphertext or
    exactly the corrected value sent. Every other byte of the spec is carried."""
    check(type(after) is dict and type(after.get("services")) is list and len(after["services"]) == 1
          and type(after["services"][0]) is dict and type(after["services"][0].get("envs")) is list, code)
    old = {row["key"]: (index, row) for index, row in enumerate(before["services"][0]["envs"])}
    new = {row.get("key"): (index, row) for index, row in enumerate(after["services"][0]["envs"])
           if type(row) is dict}
    check(set(new) == set(old) and len(new) == len(after["services"][0]["envs"]) and set(sent) == LOGIN_KEYS, code)
    allowed = []
    for key in LOGIN_KEYS:
        (index, before_row), (after_index, after_row) = old[key], new[key]
        value = after_row.get("value")
        check(index == after_index and after_row.get("type") == "SECRET" and type(value) is str
              and value != before_row["value"] and (CIPHERTEXT.fullmatch(value) or value == sent[key]), code)
        allowed.append("spec.services[0].envs[" + str(index) + "].value")
    check(set(diff_paths(before, after)) <= set(allowed), code)


def validate_update_response(value: Any, before: dict[str, Any], known: set[str], sent: dict[str, str]) -> str:
    check(type(value) is dict and type(value.get("app")) is dict, "UPDATE_RESPONSE")
    app = value["app"]
    check(all(app.get(key) == before.get(key) for key in ("id", "owner_uuid", "created_at")), "UPDATE_RESPONSE")
    pending = app.get("pending_deployment")
    check(type(pending) is dict and pending.get("phase") in NONTERMINAL, "UPDATE_RESPONSE")
    identity = common.require_uuid(pending.get("id"), "repair deployment")
    check(identity not in known and not app.get("active_deployment") and not app.get("pinned_deployment"),
          "UPDATE_RESPONSE")
    require_carried(before["spec"], app.get("spec"), "UPDATE_RESPONSE", sent)
    if "spec" in pending:
        require_carried(before["spec"], pending["spec"], "UPDATE_RESPONSE", sent)
    return identity


def await_health(provider: Any, app_name: str, *, sleep: Any = time.sleep,
                 health: Any = None) -> str:
    """Two consecutive unauthenticated /healthz 204s on the app's default origin."""
    health = health or boot.health
    successes = 0
    for _ in range(HEALTH_LIMIT):
        try:
            origin = boot.driver_origin(provider.app(), app_name)
            health(origin)
            successes += 1
        except Exception:
            # The default domain and its DNS can trail the first ACTIVE deployment.
            successes = 0
        if successes == 2:
            return origin
        sleep(POLL_SECONDS if successes == 0 else 5)
    raise Stopped("HEALTH")


def tolerant(read: Any, misses: list[int]) -> Any:
    """One provider GET; a bounded run of consecutive transport misses is tolerated."""
    try:
        value = read()
    except Stopped:
        raise
    except Exception:
        misses[0] += 1
        check(misses[0] < READ_MISSES, "DEPLOYMENT_PHASE")
        return None
    misses[0] = 0
    return value


def await_active(provider: Any, before: dict[str, Any], identity: str, sent: dict[str, str], *,
                 sleep: Any = time.sleep) -> dict[str, Any]:
    rank = -1
    misses = [0]
    for _ in range(POLL_LIMIT):
        app = tolerant(provider.app, misses)
        deployment = tolerant(lambda: provider.deployment(identity), misses) if app is not None else None
        if app is None or deployment is None:
            sleep(POLL_SECONDS)
            continue
        check(type(deployment) is dict and deployment.get("id") == identity, "DEPLOYMENT_PHASE")
        phase = deployment.get("phase")
        if phase == "ERROR":
            raise Stopped("DEPLOYMENT_ERROR")
        if phase in ("CANCELED", "SUPERSEDED"):
            raise Stopped("DEPLOYMENT_CANCELED")
        check(phase in PHASE_RANK and PHASE_RANK[phase] >= rank, "DEPLOYMENT_PHASE")
        rank = PHASE_RANK[phase]
        check(type(app) is dict and app.get("id") == before["id"], "DEPLOYMENT_PHASE")
        require_carried(before["spec"], app.get("spec"), "DEPLOYMENT_SPEC", sent)
        require_carried(before["spec"], deployment.get("spec"), "DEPLOYMENT_SPEC", sent)
        active = app.get("active_deployment")
        if phase == "ACTIVE" and type(active) is dict and active.get("id") == identity \
                and not app.get("pending_deployment") and not app.get("in_progress_deployment"):
            return app
        sleep(POLL_SECONDS)
    raise Stopped("DEPLOYMENT_TIMEOUT")


def synthetic_config(origin: str, d: dict[str, Any]) -> dict[str, Any]:
    return {"schema_version": 1, "url": origin + "/v1/execute",
            "driver_version_sha256": TARGET_DRIVER_EVIDENCE["driver_version_sha256"],
            "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
            "hmac_key_base64": d["hmac_key_base64"]}


def driver_projection(app: Any) -> bytes:
    """Identity, slots and spec; excludes provider bookkeeping such as updated_at."""
    check(type(app) is dict, "PRESTATE_MOVED")
    return common.canonical_payload_bytes({key: app.get(key) for key in ("id", "owner_uuid", "created_at", "spec", *SLOTS)})


def compare_views(update_view: Any, read_view: Any) -> None:
    """The PUT carries what the update token reads; it must be what was reviewed."""
    check(type(update_view) is dict and type(read_view) is dict
          and driver_projection(update_view) == driver_projection(read_view), "TOKEN_VIEW")


def require_active_spec(provider: Any, app: dict[str, Any], active_id: str, d: dict[str, Any]) -> None:
    """The ACTIVE deployment itself runs the reviewed spec, not just the app record."""
    deployment = provider.deployment(active_id)
    check(type(deployment) is dict and deployment.get("id") == active_id and deployment.get("phase") == "ACTIVE"
          and common.canonical_payload_bytes(deployment.get("spec")) == common.canonical_payload_bytes(app["spec"]),
          "ACTIVE_SPEC")
    validate_spec(deployment["spec"], d)


def canary_state(reader: Any) -> str:
    """'absent' (reviewed pin) or 'present' (pin plus exactly the driver secret)."""
    snapshot = reader.snapshot()
    names = {row["name"] for row in snapshot["secrets"]}
    if names == CANARY_BASE_NAMES:
        reader.require_absent()
        return "absent"
    check(names == CANARY_BASE_NAMES | {boot.SECRET_NAME}, "CANARY_STATE")
    base = copy.deepcopy(snapshot)
    base["secrets"] = [row for row in base["secrets"] if row["name"] != boot.SECRET_NAME]
    check(common.sha256_value(base) == reader.expected_metadata_sha256, "CANARY_STATE")
    return "present"


def writer_preflight(writer: Any) -> None:
    """Prove the canary write token reads its environment and key BEFORE the PUT."""
    writer.require_absent()
    key = writer.api.get(fixture.API_PREFIX + "/environments/" + boot.ENVIRONMENT + "/secrets/public-key")
    check(type(key) is dict and set(key) == {"key_id", "key"} and type(key["key_id"]) is str
          and re.fullmatch(r"[0-9]+", key["key_id"]) and type(key["key"]) is str, "WRITER_PREFLIGHT")


class DriverProvider:
    """Fixed selected-driver GETs; one PUT only when constructed for apply."""
    def __init__(self, token: str, *, allow_update: bool):
        self.__token = fixture._secret(token)
        self.__opener = fixture._opener()
        self.allow_update = allow_update is True
        self.app_id: str | None = None
        self.attempted = False

    def __repr__(self) -> str:
        return "<DriverProvider:redacted>"

    def _get(self, path: str) -> Any:
        # /v2/account is read only through the read token (the update token has
        # app scopes only); the pinned identity hash and name select the driver.
        fixed = {"/v2/account", "/v2/apps?per_page=200&page=1"}
        if self.app_id is not None:
            fixed |= {"/v2/apps/" + self.app_id, "/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1"}
        check(path in fixed or (self.app_id is not None and re.fullmatch(
            re.escape("/v2/apps/" + self.app_id + "/deployments/") + r"[0-9a-f-]{36}", path)), "DRIVER_SELECTION")
        raw = fixture._wire(self.__opener, common.API_ORIGIN + path, method="GET",
                            headers={"Authorization": "Bearer " + self.__token, "Accept": "application/json"},
                            maximum=boot.MAX_PUBLIC)
        value = common.loads_strict(raw)
        policy._json_tree(value)
        return value

    def select(self, app_name: str) -> None:
        check(self.app_id is None, "DRIVER_SELECTION")
        self.app_id = select_driver(self._get("/v2/apps?per_page=200&page=1"), app_name)

    def account(self) -> Any:
        check(not self.allow_update, "DRIVER_SELECTION")
        return self._get("/v2/account").get("account")

    def app(self) -> Any:
        return self._get("/v2/apps/" + str(self.app_id)).get("app")

    def deployments(self) -> Any:
        return self._get("/v2/apps/" + str(self.app_id) + "/deployments?per_page=200&page=1")

    def deployment(self, identity: str) -> Any:
        return self._get("/v2/apps/" + str(self.app_id) + "/deployments/"
                         + common.require_uuid(identity, "repair deployment")).get("deployment")

    def update(self, spec: dict[str, Any]) -> Any:
        policy._json_tree(spec)
        check(self.allow_update and not self.attempted and self.app_id is not None
              and common.sha256_bytes(self.app_id.encode("ascii")) == policy.IDENTITY_PINS["app_id_sha256"],
              "UPDATE_REJECTED")
        body = common.canonical_payload_bytes({"spec": spec})
        check(len(body) <= boot.MAX_PUBLIC, "UPDATE_REJECTED")
        self.attempted = True  # Burn BEFORE transport; a timeout is consumed too.
        try:
            request = urllib.request.Request(common.API_ORIGIN + "/v2/apps/" + self.app_id, data=body, method="PUT",
                headers={"Authorization": "Bearer " + self.__token, "Accept": "application/json",
                         "Content-Type": "application/json"})
            with self.__opener.open(request, timeout=60) as response:
                check(response.status == 200, "UPDATE_REJECTED")
                raw = response.read(boot.MAX_PUBLIC + 1)
        except urllib.error.HTTPError as error:
            error.close()
            raise Stopped("UPDATE_REJECTED") from None
        except Stopped:
            raise
        except Exception:
            raise Stopped("UPDATE_REJECTED") from None
        check(len(raw) <= boot.MAX_PUBLIC, "UPDATE_REJECTED")
        value = common.loads_strict(raw)
        policy._json_tree(value)
        return value


def fixture_custody(api: Any) -> None:
    metadata = recovery.fixture_metadata(api)
    rows = [row for row in metadata["secrets"] if row["name"] == FIXTURE_INPUT_NAME]
    check(len(rows) == 1 and rows[0]["updated_at"] == FIXTURE_INPUT_UPDATED_AT, "FIXTURE_INPUT_CHANGED")


def load_origin(raw: Any) -> dict[str, Any]:
    check(type(raw) is str and 0 < len(raw.encode()) <= 32768, "ORIGIN_PACKET")
    original = common.loads_strict(raw)
    common.exact_keys(original, boot.AUTH_KEYS, "historical packet")
    check(common.canonical_payload_bytes(original) == raw.encode()
          and common.sha256_value(original) == policy.ORIGIN_PACKET_SHA256
          and original["control_sha"] == policy.ORIGIN_CONTROL, "ORIGIN_PACKET")
    return original


def failure_report(error: Exception) -> dict[str, Any]:
    stage = STAGE if type(STAGE) is str and STAGE in STAGES else "AUTHORIZATION"
    result = {"schema_version": 1, "state": "driver-login-repair-stopped", "stage": stage, "retry_authorized": False}
    code = vars(error).get("code") if type(error) is Stopped else None
    if type(code) is str and code in CODES:
        result["code"] = code
    return result


def run(mode: str, root: Path, private: dict[str, Any], gh_token: str, origin_raw: Any,
        output: Path | None, *, now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> dict[str, Any]:
    original = load_origin(origin_raw)
    api = boot.GitHubRead(fixture._secret(gh_token))
    mark("CURRENT_CONTROL")
    sha = fixture._current_guard(api, root, workflow=WORKFLOW)
    mark("ORIGIN_AUTHORITY")
    descriptor_raw = private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"]
    check(len(descriptor_raw.encode()) <= 32768, "PRIVATE_INPUTS")
    origin_run, origin_artifacts = recovery.authenticate_origin(api)
    d = policy.validate_origin(original, common.loads_strict(descriptor_raw), origin_run, origin_artifacts)
    mark("FIXTURE_CUSTODY")
    fixture_custody(api)
    check(len(private[FIXTURE_INPUT_NAME].encode()) <= fixture.MAX_BODY_BYTES, "PRIVATE_INPUTS")
    logins = corrected_logins(common.loads_strict(private[FIXTURE_INPUT_NAME]))
    mark("IMAGE_AUTHORITY")
    gh = fixture._pinned_gh()
    boot.authenticate_driver(api, root, gh, TARGET_DRIVER_EVIDENCE, sha, gh_token=gh_token)
    mark("CANARY_ENVIRONMENT")
    reader = boot.EnvironmentReader(gh_token, d["plan"]["github_environment_sha256"])
    canary = canary_state(reader)
    mark("DRIVER_PRESTATE")
    viewer = DriverProvider(private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"], allow_update=False)
    viewer.select(d["plan"]["app_name"])
    seen = viewer.app()
    policy.validate_account_owner(viewer.account(), seen, d)
    known, active_id = validate_driver_state(seen, viewer.deployments(), d)
    if active_id is not None:
        require_active_spec(viewer, seen, active_id, d)
    # The canary configuration can only be present after a healthy driver.
    check(canary == "absent" or active_id is not None, "CANARY_STATE")
    mark("PLAN")
    # Only an idle, never-started driver needs the update. An ACTIVE driver at the
    # pinned image already runs corrected logins (see validate_driver_state).
    proposal = build_proposal(seen["spec"], logins) if active_id is None else None
    report = {"schema_version": 1, "kind": KIND, "control_sha": sha,
              "app_id_sha256": policy.IDENTITY_PINS["app_id_sha256"],
              "driver_evidence": copy.deepcopy(TARGET_DRIVER_EVIDENCE),
              "changed_env_keys": sorted(LOGIN_KEYS) if active_id is None else [],
              "carried_secret_keys": sorted(SECRET_KEYS - LOGIN_KEYS),
              "deployments": len(known), "driver_already_active": active_id is not None,
              "canary_configuration_present": canary == "present"}
    if mode == "plan":
        return {**report, "state": "driver-login-repair-planned"}
    mark("PRE_UPDATE")
    check(fixture._current_guard(api, root, workflow=WORKFLOW) == sha, "PRESTATE_MOVED")
    check(canary_state(reader) == canary, "PRESTATE_MOVED")
    provider = DriverProvider(private["DO_DRIVER_RECOVERY_UPDATE_TOKEN"], allow_update=True)
    provider.select(d["plan"]["app_name"])
    check(provider.app_id == viewer.app_id, "TOKEN_VIEW")
    before = provider.app()
    compare_views(before, seen)
    check(driver_projection(viewer.app()) == driver_projection(seen)
          and validate_driver_state(before, provider.deployments(), d) == (known, active_id), "PRESTATE_MOVED")
    writer = None
    if canary == "absent":
        writer = boot.EnvironmentWriter(private["GH_CANARY_ENVIRONMENT_WRITE_TOKEN"], gh,
                                        d["plan"]["github_environment_sha256"])
        writer_preflight(writer)
    if active_id is None:
        mark("UPDATE_APP")
        identity = validate_update_response(provider.update(proposal), before, known, logins)
        mark("DEPLOYMENT_READBACK")
        await_active(provider, before, identity, logins)
    else:
        identity = active_id

    def require_running(app: Any, code: str) -> None:
        check(type(app) is dict and (app.get("active_deployment") or {}).get("id") == identity
              and not app.get("pending_deployment") and not app.get("in_progress_deployment"), code)
        if active_id is None:
            require_carried(before["spec"], app.get("spec"), code, logins)
        else:
            check(common.canonical_payload_bytes(app.get("spec")) == common.canonical_payload_bytes(before["spec"]), code)

    mark("HEALTH")
    origin = await_health(provider, d["plan"]["app_name"])
    if writer is not None:
        mark("CANARY_INSTALL")
        check(fixture._current_guard(api, root, workflow=WORKFLOW) == sha, "PRESTATE_MOVED")
        require_running(provider.app(), "PRESTATE_MOVED")
        writer.install(synthetic_config(origin, d))
    mark("POSTSTATE")
    misses, final = [0], None
    while final is None:
        final = tolerant(provider.app, misses)
        if final is None:
            time.sleep(POLL_SECONDS)
    require_running(final, "POSTSTATE")
    return {**report, "state": "driver-login-repair-complete",
            "deployment_id_sha256": common.sha256_bytes(identity.encode("ascii")),
            "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
            "canary_configuration_installed_by_this_run": writer is not None,
            "health_observations": 2, "synthetic_execution_performed": False,
            "observed_at": common.format_timestamp(now())}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "apply"))
    parser.add_argument("--control-root", required=True, type=Path)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args(argv)
    try:
        mark("AUTHORIZATION")
        # Pop every private input before any subprocess can inherit it.
        private = {key: os.environ.pop(key, None) for key in PRIVATE_NAMES}
        gh_token = os.environ.pop("GH_TOKEN", None)
        for key in INHERITED_CREDENTIALS:
            os.environ.pop(key, None)
        mode = os.environ.get("REPAIR_MODE")
        check(mode in ("plan", "apply") and args.command == mode, "MODE")
        required = PLAN_PRIVATE_NAMES if mode == "plan" else PRIVATE_NAMES
        check(all(type(private[key]) is str and private[key] for key in required)
              and all(private[key] is None for key in PRIVATE_NAMES - required)
              and type(gh_token) is str and os.environ.get("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN") is None,
              "PRIVATE_INPUTS")
        check((mode == "apply") == (args.output_dir is not None), "OUTPUT")
        output = None
        if args.output_dir is not None:
            output = args.output_dir.resolve()
            check(output.parent == Path(os.environ["RUNNER_TEMP"]).resolve(strict=True) and not output.exists(),
                  "OUTPUT")
        record = run(mode, args.control_root.resolve(strict=True), private, gh_token,
                     os.environ.get("ORIGIN_AUTHORIZATION_JSON"), output)
        if output is not None:
            output.mkdir(mode=0o700, parents=False, exist_ok=False)
            receipt = common.canonical_file_bytes(record)
            (output / "receipt.json").write_bytes(receipt)
            (output / "receipt.sha256").write_text(common.sha256_bytes(receipt) + "\n", encoding="ascii")
        print(common.canonical_payload_bytes(record).decode())
        return 0
    except Exception as error:
        print(common.canonical_payload_bytes(failure_report(error)).decode(), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
