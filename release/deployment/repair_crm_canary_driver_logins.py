#!/usr/bin/env python3
"""Protected repair of the existing CRM canary driver: its two logins, then image rolls.

Login repair (plan / apply; completed 2026-09-30). Before it, the existing
driver app had never become ACTIVE. Its original installation
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

Image roll (roll-plan / roll-apply) moves the ACTIVE driver from a reviewed
PRIOR image to NEW publisher evidence given as a public dispatch input and
authenticated by the unchanged bootstrap authenticate_driver (protected
ancestry, unchanged publisher workflow, exact jobs and artifacts, current
driver-input manifest, three attestation verifications). The change set is
exactly the image digest and the CRM_CANARY_DRIVER_VERSION_SHA256 value; every
SECRET ciphertext is carried byte-identical, so the fixture input is never read.
roll-apply sends one PUT, awaits ACTIVE plus two /healthz 204 observations on
the unchanged origin, then replaces CRM_CANARY_SYNTHETIC_DRIVER_JSON, whose
only changed field is driver_version_sha256. A driver already ACTIVE at NEW is
a continuation: no PUT, health is re-proved and the configuration re-replaced.

Deliberately not inherited from the historical recovery kernel: production-app
raw spec digests, exact deployment counts and timestamps, and frozen failed-run
tables. The production app is never read or written here. Every run is gated by
the fixture environment's required reviewer. The repository is public, so every
report is content-free: no identifier, hostname, spec or value is ever printed.
"""
from __future__ import annotations

import argparse
import base64
import copy
import datetime as dt
import os
from pathlib import Path
import re
import subprocess
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
ROLL_KIND = "production-crm-canary-driver-image-roll-v1"
# Reviewed driver pins a roll may start from, oldest first. It starts with the
# image the 2026-09-30 repair made ACTIVE. Each later driver-fix PR appends the
# NEW evidence of the last completed roll; entries are never edited or removed.
PRIOR_DRIVER_EVIDENCE = (TARGET_DRIVER_EVIDENCE,)
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
# Only roll modes accept a deployment replaced by a later ACTIVE one.
ROLL_HISTORY_PHASES = FAILED_PHASES | {"SUPERSEDED"}
PHASE_RANK = {"PENDING_BUILD": 0, "BUILDING": 1, "PENDING_DEPLOY": 2, "DEPLOYING": 3, "ACTIVE": 4}
NONTERMINAL = frozenset(PHASE_RANK) - {"ACTIVE"}
MAX_DEPLOYMENTS = 50
POLL_LIMIT = 150
POLL_SECONDS = 10
HEALTH_LIMIT = 30
READ_MISSES = 3
REPLACE_READS = 3
MAX_EVIDENCE_BYTES = 4096
CANARY_BASE_NAMES = frozenset({"CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY"})
PRIVATE_NAMES = frozenset({FIXTURE_INPUT_NAME, "CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "DO_DRIVER_BOOTSTRAP_READ_TOKEN",
                           "DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"})
PLAN_PRIVATE_NAMES = PRIVATE_NAMES - {"DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"}
# Roll modes never receive the fixture input: logins are carried as ciphertext.
ROLL_PLAN_PRIVATE_NAMES = PLAN_PRIVATE_NAMES - {FIXTURE_INPUT_NAME}
ROLL_APPLY_PRIVATE_NAMES = PRIVATE_NAMES - {FIXTURE_INPUT_NAME}
ROLL_MODES = ("roll-plan", "roll-apply")
MODES = ("plan", "apply") + ROLL_MODES
REQUIRED_PRIVATE_NAMES = {"plan": PLAN_PRIVATE_NAMES, "apply": PRIVATE_NAMES,
                          "roll-plan": ROLL_PLAN_PRIVATE_NAMES, "roll-apply": ROLL_APPLY_PRIVATE_NAMES}
WRITING_MODES = ("apply", "roll-apply")
# Runner-provided OIDC request material is only for the later attest steps.
INHERITED_CREDENTIALS = ("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL")
STAGES = frozenset({"AUTHORIZATION", "CURRENT_CONTROL", "ORIGIN_AUTHORITY", "FIXTURE_CUSTODY",
                    "IMAGE_AUTHORITY", "CANARY_ENVIRONMENT", "DRIVER_PRESTATE", "PLAN", "PRE_UPDATE",
                    "UPDATE_APP", "DEPLOYMENT_READBACK", "HEALTH", "CANARY_INSTALL", "CANARY_REPLACE",
                    "POSTSTATE"})
CODES = frozenset({"MODE", "PRIVATE_INPUTS", "OUTPUT", "ORIGIN_PACKET", "FIXTURE_INPUT_CHANGED",
                   "LOGIN_SHAPE", "APPS_INVENTORY", "DRIVER_SELECTION", "DRIVER_IDENTITY",
                   "DRIVER_NOT_IDLE", "DRIVER_HISTORY", "SPEC_IDENTITY", "SPEC_SERVICE", "SPEC_IMAGE",
                   "SPEC_ENV", "SPEC_DATABASE", "PLAN_DELTA", "PRESTATE_MOVED", "UPDATE_REJECTED",
                   "UPDATE_RESPONSE", "DEPLOYMENT_ERROR", "DEPLOYMENT_CANCELED", "DEPLOYMENT_PHASE",
                   "DEPLOYMENT_SPEC", "DEPLOYMENT_TIMEOUT", "HEALTH", "POSTSTATE", "TOKEN_VIEW",
                   "ACTIVE_SPEC", "CANARY_STATE", "WRITER_PREFLIGHT", "DRIVER_EVIDENCE", "DRIVER_PINS",
                   "DRIVER_ORIGIN", "CANARY_REPLACE"})
STAGE = "AUTHORIZATION"
# A rejected update reports only its HTTP status, the provider's short error id
# and which of these fixed words its message contains; never the message itself.
PROVIDER_ERROR_ID = re.compile(r"[a-z_]{1,64}")
MESSAGE_KEYWORDS = ("credential", "database", "db_user", "decrypt", "encrypt", "forbidden", "invalid",
                    "permission", "project", "scope", "secret", "spec", "token", "unauthorized")
MAX_ERROR_BODY = 4096


class Stopped(common.ReleaseError):
    """Carries only a fixed source-literal code, never a received message."""
    def __init__(self, code: str, detail: dict[str, Any] | None = None):
        super().__init__("driver login repair stopped")
        self.code = code if code in CODES else None
        self.detail = detail if type(detail) is dict else None


def rejection_detail(status: Any, body: Any) -> dict[str, Any]:
    """Content-free classification of a provider rejection."""
    error_id, keywords = "unrecognized", []
    try:
        value = common.loads_strict(body) if type(body) is bytes and 0 < len(body) <= MAX_ERROR_BODY else None
    except Exception:
        value = None
    if type(value) is dict:
        if type(value.get("id")) is str and PROVIDER_ERROR_ID.fullmatch(value["id"]):
            error_id = value["id"]
        if type(value.get("message")) is str:
            message = value["message"].lower()
            keywords = [word for word in MESSAGE_KEYWORDS if word in message]
    return {"http_status": status if type(status) is int and 100 <= status <= 599 else None,
            "provider_error_id": error_id, "message_keywords": keywords}


def spec_fingerprints(spec: Any, prefix: str) -> dict[str, str]:
    """Whole-spec digest and a digest with every SECRET value blanked.

    Comparing these with an out-of-band full-access read separates a different
    secret representation from a different public spec, without revealing either.
    """
    public = copy.deepcopy(spec)
    for service in (public.get("services") or []) if type(public) is dict else []:
        for row in (service.get("envs") or []) if type(service) is dict else []:
            if type(row) is dict and row.get("type") == "SECRET":
                row["value"] = ""
    return {prefix + "_spec_sha256": common.sha256_value(spec), prefix + "_public_spec_sha256": common.sha256_value(public)}


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


def validate_spec(spec: Any, d: dict[str, Any], evidence: dict[str, Any] | None = None) -> None:
    """The reviewed driver spec shape at one evidence's image digest and version."""
    evidence = TARGET_DRIVER_EVIDENCE if evidence is None else evidence
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
          and image.get("digest") == evidence["digest"], "SPEC_IMAGE")
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
    check(values[VERSION_KEY] == evidence["driver_version_sha256"]
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
    identities, active_id = driver_slots_and_history(app, deployments, FAILED_PHASES)
    validate_spec(app.get("spec"), d)
    return identities, active_id


def driver_slots_and_history(app: Any, deployments: Any, history_phases: frozenset[str]) -> tuple[set[str], str | None]:
    """Pinned identity, no busy slot, and a complete history whose every
    non-ACTIVE deployment is in history_phases and keeps both pinned failures."""
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
        check(row.get("phase") == "ACTIVE" if identity == active_id else row.get("phase") in history_phases,
              "DRIVER_HISTORY")
        identities.add(identity)
    check(len(identities) == len(rows) and (active_id is None or active_id in identities)
          and {policy.IDENTITY_PINS["failed_deployment_id_sha256"], policy.IDENTITY_PINS["redeployed_deployment_id_sha256"]}
          <= {common.sha256_bytes(i.encode("ascii")) for i in identities}, "DRIVER_HISTORY")
    return identities, active_id


def spec_pins(spec: Any) -> tuple[Any, Any]:
    """The spec's image digest and version value, read without trusting its shape."""
    check(type(spec) is dict and type(spec.get("services")) is list and len(spec["services"]) == 1
          and type(spec["services"][0]) is dict, "SPEC_SERVICE")
    image, envs = spec["services"][0].get("image"), spec["services"][0].get("envs")
    check(type(image) is dict, "SPEC_IMAGE")
    check(type(envs) is list and all(type(row) is dict for row in envs), "SPEC_ENV")
    versions = [row.get("value") for row in envs if row.get("key") == VERSION_KEY]
    check(len(versions) == 1, "SPEC_ENV")
    return image.get("digest"), versions[0]


def roll_pins(spec: Any, new: dict[str, Any]) -> dict[str, Any]:
    """The one reviewed evidence the spec runs: NEW or exactly one PRIOR entry."""
    digest, version = spec_pins(spec)
    matches = [evidence for evidence in (new, *PRIOR_DRIVER_EVIDENCE)
               if evidence["digest"] == digest and evidence["driver_version_sha256"] == version]
    check(len(matches) == 1, "DRIVER_PINS")
    return matches[0]


def validate_roll_state(app: Any, deployments: Any, d: dict[str, Any],
                        new: dict[str, Any]) -> tuple[set[str], str, dict[str, Any] | None]:
    """ACTIVE on exactly one deployment at a reviewed PRIOR pin or at NEW.

    History may also contain SUPERSEDED deployments. Returns the deployment
    identities, the ACTIVE identity and the matched PRIOR evidence, or None when
    the driver already runs NEW (a continuation).
    """
    identities, active_id = driver_slots_and_history(app, deployments, ROLL_HISTORY_PHASES)
    check(active_id is not None, "DRIVER_NOT_IDLE")
    pins = roll_pins(app.get("spec"), new)
    validate_spec(app.get("spec"), d, pins)
    return identities, active_id, None if pins is new else pins


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


def roll_paths(spec: dict[str, Any]) -> list[str]:
    """The only two paths a roll may change, in diff_paths order."""
    index = [i for i, row in enumerate(spec["services"][0]["envs"]) if row.get("key") == VERSION_KEY]
    check(len(index) == 1, "PLAN_DELTA")
    return ["spec.services[0].envs[" + str(index[0]) + "].value", "spec.services[0].image.digest"]


def require_roll_carried(before: dict[str, Any], after: Any, code: str, new: dict[str, Any]) -> None:
    """Exactly the image digest and the version value differ, both at NEW.

    Every other byte of the spec is carried, including all five SECRET
    ciphertexts, which must be byte-identical rather than re-encrypted.
    """
    check(type(after) is dict and type(after.get("services")) is list and len(after["services"]) == 1
          and type(after["services"][0]) is dict and type(after["services"][0].get("envs")) is list
          and type(after["services"][0].get("image")) is dict, code)
    old = {row["key"]: (index, row) for index, row in enumerate(before["services"][0]["envs"])}
    rows = {row.get("key"): (index, row) for index, row in enumerate(after["services"][0]["envs"])
            if type(row) is dict}
    check(set(rows) == set(old) and len(rows) == len(after["services"][0]["envs"]), code)
    check(all(rows[key] == old[key] for key in SECRET_KEYS), code)
    index, row = rows[VERSION_KEY]
    check(index == old[VERSION_KEY][0] and row.get("value") == new["driver_version_sha256"]
          and after["services"][0]["image"].get("digest") == new["digest"], code)
    check(diff_paths(before, after) == roll_paths(before), code)


def build_roll_proposal(spec: dict[str, Any], new: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
    """Current spec with only the image digest and the version value moved to NEW."""
    proposal = copy.deepcopy(spec)
    service = proposal["services"][0]
    paths = roll_paths(spec)
    rows = [row for row in service["envs"] if row["key"] == VERSION_KEY]
    check(service["image"]["digest"] != new["digest"]
          and rows[0]["value"] != new["driver_version_sha256"], "PLAN_DELTA")
    service["image"]["digest"] = new["digest"]
    rows[0]["value"] = new["driver_version_sha256"]
    check(diff_paths(spec, proposal) == paths, "PLAN_DELTA")
    require_roll_carried(spec, proposal, "PLAN_DELTA", new)
    return proposal, paths


def validate_roll_update_response(value: Any, before: dict[str, Any], known: set[str], active_id: str,
                                  new: dict[str, Any]) -> str:
    """The prior deployment stays ACTIVE and exactly one new deployment is pending."""
    check(type(value) is dict and type(value.get("app")) is dict, "UPDATE_RESPONSE")
    app = value["app"]
    check(all(app.get(key) == before.get(key) for key in ("id", "owner_uuid", "created_at")), "UPDATE_RESPONSE")
    pending = app.get("pending_deployment")
    check(type(pending) is dict and pending.get("phase") in NONTERMINAL, "UPDATE_RESPONSE")
    identity = common.require_uuid(pending.get("id"), "roll deployment")
    active = app.get("active_deployment")
    check(identity not in known and type(active) is dict and active.get("id") == active_id
          and not app.get("pinned_deployment"), "UPDATE_RESPONSE")
    require_roll_carried(before["spec"], app.get("spec"), "UPDATE_RESPONSE", new)
    if "spec" in pending:
        require_roll_carried(before["spec"], pending["spec"], "UPDATE_RESPONSE", new)
    return identity


def await_roll_active(provider: Any, before: dict[str, Any], identity: str, active_id: str,
                      new: dict[str, Any], *, sleep: Any = time.sleep) -> dict[str, Any]:
    """Monotone phases to ACTIVE; until then the prior deployment stays ACTIVE."""
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
        require_roll_carried(before["spec"], app.get("spec"), "DEPLOYMENT_SPEC", new)
        require_roll_carried(before["spec"], deployment.get("spec"), "DEPLOYMENT_SPEC", new)
        active = app.get("active_deployment")
        check(type(active) is dict and active.get("id") in (active_id, identity), "DEPLOYMENT_PHASE")
        if phase == "ACTIVE" and active.get("id") == identity \
                and not app.get("pending_deployment") and not app.get("in_progress_deployment"):
            return app
        sleep(POLL_SECONDS)
    raise Stopped("DEPLOYMENT_TIMEOUT")


def synthetic_config(origin: str, d: dict[str, Any], evidence: dict[str, Any] | None = None) -> dict[str, Any]:
    evidence = TARGET_DRIVER_EVIDENCE if evidence is None else evidence
    return {"schema_version": 1, "url": origin + "/v1/execute",
            "driver_version_sha256": evidence["driver_version_sha256"],
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


def require_active_spec(provider: Any, app: dict[str, Any], active_id: str, d: dict[str, Any],
                        evidence: dict[str, Any] | None = None) -> None:
    """The ACTIVE deployment itself runs the reviewed spec, not just the app record."""
    deployment = provider.deployment(active_id)
    check(type(deployment) is dict and deployment.get("id") == active_id and deployment.get("phase") == "ACTIVE"
          and common.canonical_payload_bytes(deployment.get("spec")) == common.canonical_payload_bytes(app["spec"]),
          "ACTIVE_SPEC")
    validate_spec(deployment["spec"], d, evidence)


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


def metadata_time(value: Any, code: str) -> dt.datetime:
    try:
        return common.require_timestamp(value, "environment metadata time")
    except common.ReleaseError:
        raise Stopped(code) from None


def without_driver_secret(snapshot: dict[str, Any]) -> dict[str, Any]:
    base = copy.deepcopy(snapshot)
    base["secrets"] = [row for row in base["secrets"] if row["name"] != boot.SECRET_NAME]
    return base


class EnvironmentReplacer(boot.EnvironmentReader):
    """Replaces exactly CRM_CANARY_SYNTHETIC_DRIVER_JSON once; never creates it.

    Follows the bootstrap EnvironmentWriter install pattern: environment
    metadata reads through the write token, local sealing with the pinned gh
    --no-store, one burned PUT, then metadata readback. The gh path is the one
    already fetched for image authority; fetching it again would fail closed.
    """
    def __init__(self, token: str, gh: Path, expected_metadata_sha256: str):
        super().__init__(token, expected_metadata_sha256)
        self.__token = fixture._secret(token)
        self.gh = gh
        self.attempted = False
        self.prestate: dict[str, Any] | None = None
        self.sleep = time.sleep

    def __repr__(self) -> str:
        return "<EnvironmentReplacer:redacted>"

    def require_present(self) -> dict[str, Any]:
        """The reviewed base pin plus exactly the driver secret, unchanged since first read."""
        value = self.snapshot()
        check({row["name"] for row in value["secrets"]} == CANARY_BASE_NAMES | {boot.SECRET_NAME}
              and common.sha256_value(without_driver_secret(value)) == self.expected_metadata_sha256, "CANARY_STATE")
        if self.prestate is None:
            self.prestate = value
        check(value == self.prestate, "CANARY_STATE")
        return value

    def public_key(self) -> dict[str, Any]:
        key = self.api.get(fixture.API_PREFIX + "/environments/" + boot.ENVIRONMENT + "/secrets/public-key")
        check(type(key) is dict and set(key) == {"key_id", "key"} and type(key["key_id"]) is str
              and re.fullmatch(r"[0-9]+", key["key_id"]) and type(key["key"]) is str, "WRITER_PREFLIGHT")
        try:
            check(len(base64.b64decode(key["key"], validate=True)) == 32, "WRITER_PREFLIGHT")
        except ValueError:
            raise Stopped("WRITER_PREFLIGHT") from None
        return key

    def preflight(self) -> None:
        """Prove BEFORE the PUT that the write token reads the environment and key."""
        self.require_present()
        self.public_key()

    def replace(self, config: dict[str, Any]) -> None:
        check(not self.attempted and type(config) is dict and set(config) == {
            "schema_version", "url", "driver_version_sha256", "fixture_descriptor_sha256", "hmac_key_base64"},
            "CANARY_REPLACE")
        before = self.require_present()
        row = [item for item in before["secrets"] if item["name"] == boot.SECRET_NAME][0]
        previous = metadata_time(row["updated_at"], "CANARY_STATE")
        key = self.public_key()
        payload = common.canonical_payload_bytes(config)
        # Pinned gh v2.98.0 returns BEFORE putEnvSecret when --no-store is set:
        # it seals stdin locally and delegates no mutation (see bootstrap install).
        result = subprocess.run([str(self.gh), "secret", "set", boot.SECRET_NAME, "--env", boot.ENVIRONMENT,
                                 "--repo", common.REPOSITORY, "--no-store"],
                                input=payload, env=boot.subprocess_environment(token=self.__token),
                                capture_output=True, timeout=90, check=False)
        check(result.returncode == 0 and len(result.stdout) <= 16384, "CANARY_REPLACE")
        try:
            encrypted = result.stdout.strip().decode("ascii")
            check(len(base64.b64decode(encrypted, validate=True)) == len(payload) + 48, "CANARY_REPLACE")
        except ValueError:
            raise Stopped("CANARY_REPLACE") from None
        check(self.public_key() == key, "CANARY_REPLACE")
        self.require_present()
        self.attempted = True  # Burn before exactly one explicit, nonretry PUT.
        fixture._wire(fixture._opener(), "https://api.github.com" + fixture.API_PREFIX
                      + "/environments/" + boot.ENVIRONMENT + "/secrets/" + boot.SECRET_NAME, method="PUT",
                      headers={"Authorization": "Bearer " + self.__token, "Accept": "application/vnd.github+json",
                               "Content-Type": "application/json", "X-GitHub-Api-Version": "2022-11-28"},
                      body=common.canonical_payload_bytes({"key_id": key["key_id"], "encrypted_value": encrypted}),
                      maximum=4096)
        # An overwrite keeps created_at and moves updated_at; nothing else changes.
        # GitHub provides no secret-value readback, so this is metadata only.
        for _ in range(REPLACE_READS):
            after = self.snapshot()
            rows = [item for item in after["secrets"] if item["name"] == boot.SECRET_NAME]
            check(len(rows) == 1 and rows[0]["created_at"] == row["created_at"]
                  and without_driver_secret(after) == without_driver_secret(before), "CANARY_REPLACE")
            if metadata_time(rows[0]["updated_at"], "CANARY_REPLACE") > previous:
                return
            self.sleep(2)
        raise Stopped("CANARY_REPLACE")


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
                if response.status != 200:
                    raise Stopped("UPDATE_REJECTED", rejection_detail(response.status, None))
                raw = response.read(boot.MAX_PUBLIC + 1)
        except urllib.error.HTTPError as error:
            try:
                body = error.read(MAX_ERROR_BODY + 1)
            except Exception:
                body = None
            status = error.code
            error.close()
            raise Stopped("UPDATE_REJECTED", rejection_detail(status, body)) from None
        except Stopped:
            raise
        except Exception:
            raise Stopped("UPDATE_REJECTED", {"http_status": None, "provider_error_id": "transport",
                                              "message_keywords": []}) from None
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


def load_driver_evidence(raw: Any) -> dict[str, Any]:
    """Exact canonical public publisher evidence for the NEW image.

    Format only; boot.authenticate_driver proves it. NEW must differ from every
    reviewed PRIOR pin in both its image digest and its driver version.
    """
    check(type(raw) is str and 0 < len(raw.encode()) <= MAX_EVIDENCE_BYTES, "DRIVER_EVIDENCE")
    try:
        value = common.loads_strict(raw)
        common.exact_keys(value, boot.DRIVER_EVIDENCE_KEYS, "driver evidence")
        check(all(type(value[key]) is str for key in boot.DRIVER_EVIDENCE_KEYS)
              and common.canonical_payload_bytes(value) == raw.encode(), "DRIVER_EVIDENCE")
        common.require_sha1(value["control_sha"], "driver evidence control")
        common.require_run_id(value["run_id"], "driver evidence run")
        common.require_run_id(value["artifact_id"], "driver evidence artifact")
        common.require_digest(value["artifact_digest"], "driver evidence artifact digest")
        common.require_digest(value["digest"], "driver evidence image")
        common.require_sha256(value["driver_version_sha256"], "driver evidence version")
    except Stopped:
        raise
    except Exception:
        raise Stopped("DRIVER_EVIDENCE") from None
    check(all(value["digest"] != prior["digest"] and value["driver_version_sha256"] != prior["driver_version_sha256"]
              for prior in PRIOR_DRIVER_EVIDENCE), "DRIVER_EVIDENCE")
    return value


def prestate_origin(app: Any, app_name: str) -> str:
    try:
        return boot.driver_origin(app, app_name)
    except Exception:
        raise Stopped("DRIVER_ORIGIN") from None


def failure_report(error: Exception, *, roll: bool = False) -> dict[str, Any]:
    stage = STAGE if type(STAGE) is str and STAGE in STAGES else "AUTHORIZATION"
    state = "driver-image-roll-stopped" if roll is True else "driver-login-repair-stopped"
    result = {"schema_version": 1, "state": state, "stage": stage, "retry_authorized": False}
    code = vars(error).get("code") if type(error) is Stopped else None
    if type(code) is str and code in CODES:
        result["code"] = code
    detail = vars(error).get("detail") if type(error) is Stopped else None
    if code == "UPDATE_REJECTED" and type(detail) is dict:
        # Re-validate every field; anything unexpected is dropped, never copied.
        status = detail.get("http_status")
        if status is None or (type(status) is int and 100 <= status <= 599):
            result["http_status"] = status
        error_id = detail.get("provider_error_id")
        if type(error_id) is str and (PROVIDER_ERROR_ID.fullmatch(error_id) or error_id == "unrecognized"):
            result["provider_error_id"] = error_id
        words = detail.get("message_keywords")
        if type(words) is list and all(type(w) is str and w in MESSAGE_KEYWORDS for w in words):
            result["message_keywords"] = sorted(set(words))
        for key in ("update_view_spec_sha256", "update_view_public_spec_sha256"):
            if policy._is_sha256_hex(detail.get(key)):
                result[key] = detail[key]
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
              "canary_configuration_present": canary == "present",
              **spec_fingerprints(seen["spec"], "read_view")}
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
        try:
            response = provider.update(proposal)
        except Stopped as error:
            if error.code == "UPDATE_REJECTED" and error.detail is not None:
                error.detail.update(spec_fingerprints(before["spec"], "update_view"))
            raise
        identity = validate_update_response(response, before, known, logins)
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


def run_roll(mode: str, root: Path, private: dict[str, Any], gh_token: str, origin_raw: Any, evidence_raw: Any,
             output: Path | None, *, now: Any = lambda: dt.datetime.now(dt.timezone.utc),
             sleep: Any = None) -> dict[str, Any]:
    """roll-plan / roll-apply. Never reads CRM_CANARY_FIXTURE_INPUT_JSON."""
    check(mode in ROLL_MODES, "MODE")
    sleep = time.sleep if sleep is None else sleep
    original = load_origin(origin_raw)
    new = load_driver_evidence(evidence_raw)
    api = boot.GitHubRead(fixture._secret(gh_token))
    mark("CURRENT_CONTROL")
    sha = fixture._current_guard(api, root, workflow=WORKFLOW)
    mark("ORIGIN_AUTHORITY")
    descriptor_raw = private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"]
    check(len(descriptor_raw.encode()) <= 32768, "PRIVATE_INPUTS")
    origin_run, origin_artifacts = recovery.authenticate_origin(api)
    d = policy.validate_origin(original, common.loads_strict(descriptor_raw), origin_run, origin_artifacts)
    mark("IMAGE_AUTHORITY")
    # Exactly once per run: a second fetch fails closed on the existing binary,
    # so the replacer reuses this path.
    gh = fixture._pinned_gh()
    # Protected ancestry rather than control equality: a later fix outside the
    # driver inputs and the publisher workflow does not need a re-publish.
    boot.authenticate_driver(api, root, gh, new, sha, gh_token=gh_token)
    mark("CANARY_ENVIRONMENT")
    reader = boot.EnvironmentReader(gh_token, d["plan"]["github_environment_sha256"])
    check(canary_state(reader) == "present", "CANARY_STATE")
    mark("DRIVER_PRESTATE")
    viewer = DriverProvider(private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"], allow_update=False)
    viewer.select(d["plan"]["app_name"])
    seen = viewer.app()
    policy.validate_account_owner(viewer.account(), seen, d)
    known, active_id, prior = validate_roll_state(seen, viewer.deployments(), d, new)
    require_active_spec(viewer, seen, active_id, d, new if prior is None else prior)
    origin = prestate_origin(seen, d["plan"]["app_name"])
    mark("PLAN")
    proposal, paths = build_roll_proposal(seen["spec"], new) if prior is not None else (None, [])
    report = {"schema_version": 1, "kind": ROLL_KIND, "control_sha": sha,
              "app_id_sha256": policy.IDENTITY_PINS["app_id_sha256"],
              "prior_driver_evidence": copy.deepcopy(prior), "driver_evidence": copy.deepcopy(new),
              "changed_spec_paths": paths, "changed_env_keys": [VERSION_KEY] if prior is not None else [],
              "carried_secret_keys": sorted(SECRET_KEYS), "deployments": len(known),
              "driver_already_at_new": prior is None, "canary_configuration_present": True,
              **spec_fingerprints(seen["spec"], "read_view")}
    if mode == "roll-plan":
        return {**report, "state": "driver-image-roll-planned"}
    mark("PRE_UPDATE")
    check(fixture._current_guard(api, root, workflow=WORKFLOW) == sha, "PRESTATE_MOVED")
    check(canary_state(reader) == "present", "PRESTATE_MOVED")
    provider = DriverProvider(private["DO_DRIVER_RECOVERY_UPDATE_TOKEN"], allow_update=True)
    provider.select(d["plan"]["app_name"])
    check(provider.app_id == viewer.app_id, "TOKEN_VIEW")
    before = provider.app()
    compare_views(before, seen)
    check(driver_projection(viewer.app()) == driver_projection(seen)
          and validate_roll_state(before, provider.deployments(), d, new) == (known, active_id, prior),
          "PRESTATE_MOVED")
    replacer = EnvironmentReplacer(private["GH_CANARY_ENVIRONMENT_WRITE_TOKEN"], gh,
                                   d["plan"]["github_environment_sha256"])
    replacer.sleep = sleep
    replacer.preflight()
    if prior is not None:
        mark("UPDATE_APP")
        try:
            response = provider.update(proposal)
        except Stopped as error:
            if error.code == "UPDATE_REJECTED" and error.detail is not None:
                error.detail.update(spec_fingerprints(before["spec"], "update_view"))
            raise
        identity = validate_roll_update_response(response, before, known, active_id, new)
        mark("DEPLOYMENT_READBACK")
        require_active_spec(provider, await_roll_active(provider, before, identity, active_id, new, sleep=sleep),
                            identity, d, new)
    else:
        identity = active_id  # Continuation: already ACTIVE at NEW, so no PUT.

    def require_running(app: Any, code: str) -> None:
        check(type(app) is dict and (app.get("active_deployment") or {}).get("id") == identity
              and not app.get("pending_deployment") and not app.get("in_progress_deployment"), code)
        if prior is not None:
            require_roll_carried(before["spec"], app.get("spec"), code, new)
        else:
            check(common.canonical_payload_bytes(app.get("spec")) == common.canonical_payload_bytes(before["spec"]), code)

    mark("HEALTH")
    check(await_health(provider, d["plan"]["app_name"], sleep=sleep) == origin, "DRIVER_ORIGIN")
    mark("CANARY_REPLACE")
    check(fixture._current_guard(api, root, workflow=WORKFLOW) == sha, "PRESTATE_MOVED")
    require_running(provider.app(), "PRESTATE_MOVED")
    # Rebuilt from the same descriptor and origin: only driver_version_sha256 moves.
    replacer.replace(synthetic_config(origin, d, new))
    mark("POSTSTATE")
    misses, final = [0], None
    while final is None:
        final = tolerant(provider.app, misses)
        if final is None:
            sleep(POLL_SECONDS)
    require_running(final, "POSTSTATE")
    return {**report, "state": "driver-image-roll-complete",
            "deployment_id_sha256": common.sha256_bytes(identity.encode("ascii")),
            "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
            "canary_configuration_replaced": True, "health_observations": 2,
            "synthetic_execution_performed": False, "observed_at": common.format_timestamp(now())}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=MODES)
    parser.add_argument("--control-root", required=True, type=Path)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args(argv)
    mode = None
    try:
        mark("AUTHORIZATION")
        # Pop every private input before any subprocess can inherit it.
        private = {key: os.environ.pop(key, None) for key in PRIVATE_NAMES}
        gh_token = os.environ.pop("GH_TOKEN", None)
        for key in INHERITED_CREDENTIALS:
            os.environ.pop(key, None)
        mode = os.environ.get("REPAIR_MODE")
        check(mode in MODES and args.command == mode, "MODE")
        required = REQUIRED_PRIVATE_NAMES[mode]
        check(all(type(private[key]) is str and private[key] for key in required)
              and all(private[key] is None for key in PRIVATE_NAMES - required)
              and type(gh_token) is str and os.environ.get("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN") is None,
              "PRIVATE_INPUTS")
        # Public input: required for a roll, and empty (the dispatch default) otherwise.
        evidence_raw = os.environ.get("DRIVER_EVIDENCE_JSON")
        check((type(evidence_raw) is str and evidence_raw != "") if mode in ROLL_MODES
              else (evidence_raw in (None, "")), "DRIVER_EVIDENCE")
        check((mode in WRITING_MODES) == (args.output_dir is not None), "OUTPUT")
        output = None
        if args.output_dir is not None:
            output = args.output_dir.resolve()
            check(output.parent == Path(os.environ["RUNNER_TEMP"]).resolve(strict=True) and not output.exists(),
                  "OUTPUT")
        if mode in ROLL_MODES:
            record = run_roll(mode, args.control_root.resolve(strict=True), private, gh_token,
                              os.environ.get("ORIGIN_AUTHORIZATION_JSON"), evidence_raw, output)
        else:
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
        print(common.canonical_payload_bytes(failure_report(error, roll=mode in ROLL_MODES)).decode(), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
