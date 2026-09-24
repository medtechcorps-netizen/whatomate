#!/usr/bin/env python3
"""Protected one-shot recovery of the exact existing CRM canary driver.

Public validation and check never receive a mutation credential. Recovery may
perform one existing-app PUT and, only after exact ACTIVE/health checks, one
canary-secret addition. No create, redeploy, retry, cleanup, logs or credential
GET route exists. All runtime specs and private inputs remain in memory.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
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
    from . import recover_production_crm_canary_driver as policy
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import recover_production_crm_canary_driver as policy

boot, common, fixture = policy.boot, policy.common, policy.boot.fixture
WORKFLOW = ".github/workflows/recover-production-crm-canary-driver.yml"
KIND = "production-crm-canary-driver-existing-recovery-v1"
AUTH_KEYS = policy.AUTH_KEYS | {"mode", "binding_sha256", "check_run_id", "check_operation_id",
                               "production_history_sha256"}
BINDING_EXCLUDED = {"kind", "mode", "operation_id", "issued_at", "expires_at", "effects",
                    "binding_sha256", "check_run_id", "check_operation_id"}
CHECK_EFFECTS = {**policy.EFFECTS, "ledger_table_initialization": False}
RECOVER_EFFECTS = {**policy.EFFECTS, "app_updates": 1, "deployments": 1, "secret_updates": 1,
                   "ledger_table_initialization": True}
PRIVATE_NAMES = frozenset({"CRM_CANARY_FIXTURE_INPUT_JSON", "CRM_CANARY_DRIVER_BOOTSTRAP_JSON",
                           "DO_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_DRIVER_RECOVERY_UPDATE_TOKEN",
                           "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"})
CHECK_PRIVATE_NAMES = PRIVATE_NAMES - {"DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"}
NONTERMINAL = frozenset({"PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING"})
ACTIVE_STATUSES = frozenset({"queued", "in_progress", "waiting", "pending", "requested"})
require = policy.require
PHASE_RANK = {"PENDING_BUILD": 0, "BUILDING": 1, "PENDING_DEPLOY": 2, "DEPLOYING": 3, "ACTIVE": 4}
STAGES = frozenset({"AUTHORIZATION", "ORIGIN_AUTHORITY", "FIXTURE_AUTHORITY", "IMAGE_AUTHORITY",
    "PRIVATE_REHYDRATION", "PROVIDER_PRESTATE", "UPDATE_APP", "DEPLOYMENT_READBACK", "HEALTH", "CANARY_INSTALL", "POSTSTATE"})
CURRENT_STAGE = "AUTHORIZATION"
AUTHORIZATION_DIAGNOSTICS = frozenset({
    "PUBLIC_AUTHORITY", "PRIVATE_INPUT_PRESENCE", "CURRENT_GUARD_TOKEN",
    "CURRENT_GUARD_HEAD", "CURRENT_GUARD_PROTECTION", "CURRENT_GUARD_HISTORY",
    "CURRENT_GUARD_OWN_JOB",
    "DESCRIPTOR_VALIDATION", "PINNED_GH_ACQUISITION", "PRE_RUN_ONCE_SETUP",
    "RUN_ONCE_AUTHORIZATION",
})

# These are exact messages emitted by the checked-in planner's provider_state
# path. Never publish the exception, received field, value, key or hash. The
# closed codes only locate the acceptance predicate that rejected the REST
# envelopes; they do not relax that predicate or infer a provider root cause.
_PROVIDER_PLAN_REJECTIONS = {
    "provider app response is malformed": "PRODUCTION_PROVIDER_APP_ENVELOPE",
    "provider deployment response is malformed": "PRODUCTION_PROVIDER_DEPLOYMENT_ENVELOPE",
    "observed app ID differs": "PRODUCTION_PROVIDER_APP_IDENTITY",
    "observed app spec is malformed": "PRODUCTION_PROVIDER_APP_SPEC_SHAPE",
    "observed app name differs": "PRODUCTION_PROVIDER_APP_NAME",
    "observed provider default ingress differs": "PRODUCTION_PROVIDER_DEFAULT_INGRESS",
    "production ingress is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "production ingress rule is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "production ingress must use exact prefix matches": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "ingress rule must have exactly one destination": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "ingress component destination is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "ingress preserve_path_prefix must be boolean": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "ingress redirect destination is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "redirect authority match is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "active deployment is missing": "PRODUCTION_PROVIDER_ACTIVE_MISSING",
    "active deployment differs from the signed predecessor": "PRODUCTION_PROVIDER_ACTIVE_IDENTITY",
    "active deployment is not ACTIVE": "PRODUCTION_PROVIDER_ACTIVE_PHASE",
    "provider reports a pending, in-progress, or pinned deployment": "PRODUCTION_PROVIDER_NOT_IDLE",
    "deployment response ID differs from the active deployment": "PRODUCTION_PROVIDER_DEPLOYMENT_IDENTITY",
    "deployment response is not ACTIVE": "PRODUCTION_PROVIDER_DEPLOYMENT_PHASE",
    "provider response is missing an app spec": "PRODUCTION_PROVIDER_DEPLOYMENT_SPEC_SHAPE",
    "embedded active spec differs from the live spec": "PRODUCTION_PROVIDER_EMBEDDED_SPEC_EQUALITY",
    "live and active deployment specs differ": "PRODUCTION_PROVIDER_LIVE_SPEC_EQUALITY",
    "raw production spec differs from the signed predecessor": "PRODUCTION_PROVIDER_RAW_SPEC_DIGEST",
    "production environment values differ from the signed predecessor": "PRODUCTION_PROVIDER_ENVIRONMENT_DIGEST",
    "production non-source projection differs from the signed predecessor": "PRODUCTION_PROVIDER_NON_SOURCE_DIGEST",
    "production spec app name differs": "PRODUCTION_PROVIDER_TOPOLOGY_NAME",
    "production region differs": "PRODUCTION_PROVIDER_TOPOLOGY_REGION",
    "production VPC binding is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_VPC",
    "production VPC binding differs": "PRODUCTION_PROVIDER_TOPOLOGY_VPC",
    "production ingress differs": "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "production domains are malformed": "PRODUCTION_PROVIDER_TOPOLOGY_DOMAINS",
    "production domain entry is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_DOMAINS",
    "production domains differ": "PRODUCTION_PROVIDER_TOPOLOGY_DOMAINS",
    "production database bindings differ": "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES",
    "production database entry is malformed": "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES",
    "environment list is malformed": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "environment entry is malformed": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "duplicate environment key": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "environment scope is outside the reviewed contract": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "environment value is missing": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "environment type is outside the reviewed contract": "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE",
    "database production flag must be boolean": "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES",
    "observed app updated_at is not UTC": "PRODUCTION_PROVIDER_APP_UPDATED_AT",
    "migration job kind differs": "PRODUCTION_PROVIDER_SOURCE_AUTHORITY",
    "migration job run command differs": "PRODUCTION_PROVIDER_SOURCE_AUTHORITY",
    "genesis predecessor unexpectedly contains image authority": "PRODUCTION_PROVIDER_SOURCE_AUTHORITY",
    "phase predecessor image authority is missing": "PRODUCTION_PROVIDER_SOURCE_AUTHORITY",
    "predecessor source mode differs": "PRODUCTION_PROVIDER_SOURCE_AUTHORITY",
}

# The planner constructs these messages from only these reviewed, fixed labels
# and suffixes. This expands an exact-match table, not a prefix/regex matcher
# over untrusted text; any other label, component or message is UNCLASSIFIED.
for _label, _code in (
    ("observed app ID", "PRODUCTION_PROVIDER_APP_IDENTITY"),
    ("observed app updated_at", "PRODUCTION_PROVIDER_APP_UPDATED_AT"),
    ("active deployment ID", "PRODUCTION_PROVIDER_ACTIVE_IDENTITY"),
    ("deployment response ID", "PRODUCTION_PROVIDER_DEPLOYMENT_IDENTITY"),
):
    for _suffix in (" is invalid", " contains control characters", " format is invalid"):
        _PROVIDER_PLAN_REJECTIONS[_label + _suffix] = _code

for _label, _code in (
    ("environment key", "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE"),
    ("production domain", "PRODUCTION_PROVIDER_TOPOLOGY_DOMAINS"),
    ("database binding name", "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES"),
    ("database cluster name", "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES"),
    ("database engine", "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES"),
    ("database version", "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES"),
    ("ingress path prefix", "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS"),
    ("ingress component", "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS"),
    ("redirect match authority", "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS"),
    ("redirect authority", "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS"),
):
    for _suffix in (" is invalid", " contains control characters"):
        _PROVIDER_PLAN_REJECTIONS[_label + _suffix] = _code

for _collection in ("services", "jobs", "workers", "static_sites", "functions"):
    for _message in (f"spec {_collection} must be an array",
                     f"spec {_collection} entry is malformed",
                     f"duplicate spec component in {_collection}"):
        _PROVIDER_PLAN_REJECTIONS[_message] = "PRODUCTION_PROVIDER_NON_SOURCE_STRUCTURE"
    for _suffix in (" is invalid", " contains control characters"):
        _PROVIDER_PLAN_REJECTIONS[f"spec {_collection} component name" + _suffix] = (
            "PRODUCTION_PROVIDER_NON_SOURCE_STRUCTURE")
    _PROVIDER_PLAN_REJECTIONS[f"spec {_collection} component set differs"] = (
        "PRODUCTION_PROVIDER_NON_SOURCE_STRUCTURE")
for _collection in ("services", "jobs"):
    _PROVIDER_PLAN_REJECTIONS[f"production {_collection} component set differs"] = (
        "PRODUCTION_PROVIDER_SOURCE_AUTHORITY")
for _collection in ("workers", "static_sites", "functions"):
    _PROVIDER_PLAN_REJECTIONS[f"unexpected production {_collection} component"] = (
        "PRODUCTION_PROVIDER_TOPOLOGY_COMPONENTS")

for _name in ("omnitech-web", "meta-relay", "gmail-relay", "rereply-rls-migrate"):
    for _message in (f"digest image source selector is not exact for {_name}",
                     f"digest image component retains a Dockerfile for {_name}",
                     f"digest image source for {_name} keys differ",
                     f"digest image source differs for {_name}",
                     f"HTTP port differs for {_name}", f"health path differs for {_name}"):
        _PROVIDER_PLAN_REJECTIONS[_message] = "PRODUCTION_PROVIDER_SOURCE_AUTHORITY"


def mark_stage(stage: str) -> None:
    global CURRENT_STAGE
    require(stage in STAGES)
    CURRENT_STAGE = stage


class AuthorizationRejected(common.ReleaseError):
    """A reviewed source-literal boundary, never an exception or input value."""
    def __init__(self, code: str):
        require(type(code) is str and code in AUTHORIZATION_DIAGNOSTICS)
        super().__init__("existing-driver authorization rejected")
        self.code = code


@contextmanager
def authorization_diagnostic(code: str):
    require(type(code) is str and code in AUTHORIZATION_DIAGNOSTICS)
    try:
        yield
    except Exception as error:
        if type(error) is AuthorizationRejected:
            inner = vars(error).get("code")
            if type(inner) is str and inner in AUTHORIZATION_DIAGNOSTICS:
                raise
        raise AuthorizationRejected(code) from None


class UpdateRejected(common.ReleaseError):
    def __init__(self, status: int | None = None):
        super().__init__("existing driver update rejected")
        self.status = status if type(status) is int and 100 <= status <= 599 else None


def _timestamp(value: Any) -> dt.datetime:
    parsed = common.require_timestamp(value, "recovery timestamp")
    require(common.format_timestamp(parsed) == value)
    return parsed


def binding_hash(value: Any) -> str:
    policy._json_tree(value)
    common.exact_keys(value, AUTH_KEYS, "recovery authority")
    return common.sha256_value({key: item for key, item in value.items() if key not in BINDING_EXCLUDED})


def run_title(value: dict[str, Any]) -> str:
    require(value["mode"] in ("check", "recover"))
    return ("Check" if value["mode"] == "check" else "Recover") + " existing CRM driver " \
        + value["operation_id"] + " " + value["binding_sha256"]


def inspection_authorization(value: dict[str, Any]) -> dict[str, Any]:
    # This internal view permits the read-only kernel only. It is never used as
    # operational authority, persisted or reported as the actual input packet.
    result = {key: copy.deepcopy(value[key]) for key in policy.AUTH_KEYS}
    result["kind"] = "production-crm-canary-driver-existing-inspection-v1"
    result["effects"] = copy.deepcopy(policy.EFFECTS)
    return result


def validate_authorization(value: Any, *, control_sha: str, now: dt.datetime) -> dict[str, Any]:
    policy._json_tree(value)
    a = common.exact_keys(value, AUTH_KEYS, "recovery authority")
    require(a["kind"] == KIND and a["mode"] in ("check", "recover"))
    require(type(a["effects"]) is dict
            and a["effects"] == (CHECK_EFFECTS if a["mode"] == "check" else RECOVER_EFFECTS)
            and all(type(a["effects"].get(key)) is int for key in policy.EFFECTS)
            and type(a["effects"].get("ledger_table_initialization")) is bool)
    policy.validate_recovery_authorization(inspection_authorization(a), control_sha=control_sha, now=now)
    for key in ("issued_at", "expires_at"):
        require(common.format_timestamp(common.require_timestamp(a[key], "authority time")) == a[key])
    require(common.require_sha256(a["binding_sha256"], "recovery binding") == binding_hash(a))
    common.require_sha256(a["production_history_sha256"], "production history")
    if a["mode"] == "check":
        require(a["check_run_id"] is None and a["check_operation_id"] is None)
    else:
        common.require_run_id(a["check_run_id"], "predecessor check")
        common.require_uuid(a["check_operation_id"], "predecessor operation")
        require(a["check_operation_id"] != a["operation_id"])
    return copy.deepcopy(a)


def _target_expected(original: Any, descriptor: Any, expected: Any, a: Any) -> dict[str, Any]:
    # Use the same reviewed planner to construct the expected new shape. The
    # artificial ciphertext is local fixed data and never a provider submission.
    opaque = copy.deepcopy(expected)
    for row in opaque["services"][0]["envs"]:
        if row["type"] == "SECRET":
            row["value"] = "EV[synthetic-policy-placeholder]"
    return policy.target_update_plan(original, descriptor, expected, a["target_driver_evidence"],
                                     opaque, control_sha=a["control_sha"])


def validate_update_response(value: Any, before: Any, expected_target: Any, *, issued_at: str,
                             now: dt.datetime | None = None) -> str:
    """Require identity and exact associated pending deployment on this PUT."""
    policy._json_tree(value)
    require(type(value) is dict and type(value.get("app")) is dict)
    app = value["app"]
    old = before["app"]
    require(app.get("id") == old["id"] and app.get("owner_uuid") == old["owner_uuid"]
            and app.get("created_at") == old["created_at"])
    pending = app.get("pending_deployment")
    require(type(pending) is dict and pending.get("phase") in NONTERMINAL)
    identity = common.require_uuid(pending.get("id"), "updated deployment")
    require(identity not in {row["id"] for row in before["deployments"]["deployments"]})
    require(all(app.get(key) is None or app.get(key) == {} for key in
                ("active_deployment", "in_progress_deployment", "pinned_deployment")))
    created = _timestamp(pending.get("created_at"))
    require(created >= _timestamp(issued_at)
            and created <= (now if now is not None else dt.datetime.now(dt.timezone.utc))
            and _timestamp(old.get("updated_at")) <= _timestamp(app.get("updated_at"))
                <= (now if now is not None else dt.datetime.now(dt.timezone.utc)))
    app_spec = policy._exact_spec(app.get("spec"), expected_target)
    pending_spec = policy._exact_spec(pending.get("spec"), expected_target)
    require(common.canonical_payload_bytes(app_spec) == common.canonical_payload_bytes(pending_spec))
    old_values = {row["key"]: row["value"] for row in old["spec"]["services"][0]["envs"]}
    for row in app_spec["services"][0]["envs"]:
        if row["type"] == "SECRET" and row["key"] not in policy.LOGIN_KEYS:
            require(row["value"] == old_values[row["key"]])
        elif row["key"] in policy.LOGIN_KEYS:
            require(row["value"] != old_values[row["key"]])
    return identity


def validate_poststate(value: Any, before: Any, expected_target: Any, deployment_id: str,
                       *, canary_after: bool = False) -> tuple[str, dict[str, Any]]:
    """Validate one readback; ERROR/unknown or any unrelated delta fails closed."""
    policy._json_tree(value)
    common.exact_keys(value, policy.SNAPSHOT_KEYS, "recovery poststate")
    old_app, app = before["app"], value["app"]
    require(type(app) is dict and app.get("id") == old_app["id"]
            and app.get("owner_uuid") == old_app["owner_uuid"]
            and app.get("created_at") == old_app["created_at"])
    for key in ("account", "apps", "firewall", "fixture_environment_sha256", "production_state_sha256"):
        # App inventory contains metadata only in the collector, not changing specs.
        require(common.canonical_payload_bytes(value[key]) == common.canonical_payload_bytes(before[key]))
    if not canary_after:
        require(value["canary_environment"] == before["canary_environment"])
    deps = policy._complete_inventory(value["deployments"], "deployments", 3)
    old_deps = {row["id"]: row for row in before["deployments"]["deployments"]}
    require({row["id"] for row in deps} == set(old_deps) | {deployment_id})
    for row in deps:
        if row["id"] in old_deps:
            require(common.canonical_payload_bytes(row) == common.canonical_payload_bytes(old_deps[row["id"]]))
    selected = next(row for row in deps if row["id"] == deployment_id)
    phase = selected.get("phase")
    require(phase in NONTERMINAL | {"ACTIVE"})
    normalized = policy._exact_spec(app.get("spec"), expected_target)
    dep_spec = policy._exact_spec(selected.get("spec"), expected_target)
    require(common.canonical_payload_bytes(normalized) == common.canonical_payload_bytes(dep_spec))
    old_env = {row["key"]: row["value"] for row in old_app["spec"]["services"][0]["envs"]}
    for row in normalized["services"][0]["envs"]:
        if row["type"] == "SECRET" and row["key"] not in policy.LOGIN_KEYS:
            require(row["value"] == old_env[row["key"]])
        elif row["key"] in policy.LOGIN_KEYS:
            require(row["value"] != old_env[row["key"]])
    # Preserve original provider routing/features even though the strict public
    # projection treats exactly recognized representations as equivalent.
    for spec in (app["spec"], selected["spec"]):
        for key in ("ingress", "features"):
            require((key in spec) == (key in old_app["spec"])
                    and spec.get(key) == old_app["spec"].get(key))
    coherent = True
    for key in ("active_deployment", "pending_deployment", "in_progress_deployment", "pinned_deployment"):
        slot = app.get(key)
        require(slot is None or type(slot) is dict)
        if slot:
            require(slot.get("id") == deployment_id and slot.get("phase") in PHASE_RANK
                    and PHASE_RANK[slot["phase"]] <= PHASE_RANK[phase])
            coherent = coherent and slot["phase"] == phase
    if phase == "ACTIVE":
        coherent = coherent and (app.get("active_deployment") or {}).get("id") == deployment_id \
            and not any(app.get(key) for key in ("pending_deployment", "in_progress_deployment", "pinned_deployment"))
    require(_timestamp(app.get("updated_at")) >= _timestamp(old_app.get("updated_at")))
    for key in ("created_at", "updated_at"):
        _timestamp(selected.get(key))
    require(_timestamp(selected["created_at"]) <= _timestamp(selected["updated_at"]))
    stable = {"app_updated_at": app["updated_at"], "deployment_created_at": selected["created_at"],
              "deployment_updated_at": selected["updated_at"], "spec": normalized,
              "default_ingress": app.get("default_ingress"), "phase": phase}
    return phase if coherent else "NOT_READY", stable


def run_once(a: Any, original: Any, descriptor: Any, protected: Any, *, provider: Any,
             reader: Any, authenticate_origin: Any, authenticate_fixture: Any,
             authenticate_image: Any, current_guard: Any, rehydrate: Any, transport: Any,
             writer: Any = None, now: Any = lambda: dt.datetime.now(dt.timezone.utc),
             sleep: Any = time.sleep, health: Any = boot.health) -> dict[str, Any]:
    """All injected boundaries are offline-test seams; CLI supplies fixed adapters."""
    mark_stage("AUTHORIZATION")
    with authorization_diagnostic("RUN_ONCE_AUTHORIZATION"):
        a = validate_authorization(a, control_sha=a["control_sha"], now=now())
        require((a["mode"] == "check" and writer is None and not hasattr(provider, "update"))
                or (a["mode"] == "recover" and writer is not None and hasattr(provider, "update")))
        with authorization_diagnostic("CURRENT_GUARD_HISTORY"):
            current_guard()
    mark_stage("ORIGIN_AUTHORITY")
    run, artifacts = authenticate_origin()
    d = policy.validate_origin(original, descriptor, run, artifacts)
    require(a["operation_id"] != original["operation_id"])
    mark_stage("FIXTURE_AUTHORITY")
    raw = authenticate_fixture()
    require(type(raw) is bytes and common.sha256_bytes(raw) == original["fixture_evidence"]["result_sha256"])
    result = fixture.validate_terminal_result(common.loads_strict(raw))
    require(result["fixture_descriptor_sha256"] == d["fixture_descriptor_sha256"])
    mark_stage("IMAGE_AUTHORITY")
    authenticate_image()
    mark_stage("PRIVATE_REHYDRATION")
    request = {key: result[key] for key in fixture.REQUEST_KEYS}
    checked = fixture.validate_protected_input(protected, request)
    reconstructed = rehydrate(request, checked, result, transport)
    expected = policy.historical_expected_spec(original, d, checked, reconstructed)
    mark_stage("PROVIDER_PRESTATE")
    with policy.prestate_diagnostic("CANARY_ABSENCE"):
        reader.require_absent()
    with policy.prestate_diagnostic("FIRST_SNAPSHOT"):
        first = provider.snapshot()
    sleep(3)
    with policy.prestate_diagnostic("PRESTATE_GUARD"):
        current_guard()
    with policy.prestate_diagnostic("SECOND_SNAPSHOT"):
        second = provider.snapshot()
    report = policy.inspect_pair(inspection_authorization(a), original, d, run, artifacts,
        first, second, control_sha=a["control_sha"], expected_spec=expected, now=now())
    with policy.prestate_diagnostic("PRESTATE_AUTHORIZATION"):
        validate_authorization(a, control_sha=a["control_sha"], now=now())
    with policy.prestate_diagnostic("PRESTATE_GUARD"):
        current_guard()
    if a["mode"] == "check":
        return {**report, "state": "existing-driver-check-complete",
                "authorization_sha256": common.sha256_value(a), "binding_sha256": a["binding_sha256"]}
    target = _target_expected(original, d, expected, a)
    plan = policy.target_update_plan(original, d, expected, a["target_driver_evidence"],
                                     second["app"]["spec"], control_sha=a["control_sha"])
    reader.require_absent()
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    final_prestate = provider.snapshot()
    require(policy._snapshot(inspection_authorization(a), d, final_prestate, expected)
            == policy._snapshot(inspection_authorization(a), d, second, expected))
    try:
        mark_stage("UPDATE_APP")
        sent_at = common.format_timestamp(now())
        response = provider.update(plan)
        identity = validate_update_response(response, second, target, issued_at=sent_at, now=now())
        stable = None
        latest = None
        previous_rank = -1
        for _ in range(80):
            mark_stage("DEPLOYMENT_READBACK")
            current_guard()
            validate_authorization(a, control_sha=a["control_sha"], now=now())
            latest = provider.observe(identity)
            phase, current = validate_poststate(latest, second, target, identity)
            require(PHASE_RANK[current["phase"]] >= previous_rank
                    and _timestamp(current["deployment_created_at"]) >= _timestamp(sent_at)
                    and _timestamp(current["deployment_updated_at"]) <= now()
                    and _timestamp(current["app_updated_at"]) <= now())
            previous_rank = PHASE_RANK[current["phase"]]
            if phase == "ACTIVE":
                mark_stage("HEALTH")
                origin = boot.driver_origin(latest["app"], d["plan"]["app_name"])
                health(origin)
                if stable is not None:
                    require(common.canonical_payload_bytes(stable) == common.canonical_payload_bytes(current))
                    break
                stable = current
            else:
                require(stable is None)
            sleep(10)
        else:
            require(False)
        current_guard()
        validate_authorization(a, control_sha=a["control_sha"], now=now())
        reader.require_absent()
        mark_stage("CANARY_INSTALL")
        writer.install({"schema_version": 1, "url": origin + "/v1/execute",
            "driver_version_sha256": a["target_driver_evidence"]["driver_version_sha256"],
            "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
            "hmac_key_base64": d["hmac_key_base64"]})
        current_guard()
        mark_stage("POSTSTATE")
        validate_authorization(a, control_sha=a["control_sha"], now=now())
        final = provider.observe(identity)
        phase, final_stable = validate_poststate(final, second, target, identity, canary_after=True)
        require(phase == "ACTIVE" and common.canonical_payload_bytes(final_stable)
                == common.canonical_payload_bytes(stable))
        env = copy.deepcopy(final["canary_environment"])
        additions = [row for row in env["secrets"] if row["name"] == boot.SECRET_NAME]
        require(len(additions) == 1)
        common.exact_keys(additions[0], {"name", "created_at", "updated_at"}, "canary addition metadata")
        require(_timestamp(sent_at) <= _timestamp(additions[0]["created_at"])
                <= _timestamp(additions[0]["updated_at"]) <= now())
        env["secrets"] = [row for row in env["secrets"] if row["name"] != boot.SECRET_NAME]
        require(env == second["canary_environment"])
        return {"schema_version": 1, "kind": "production-crm-canary-driver-recovery-receipt-v1",
                "state": "existing-driver-recovery-complete", "control_sha": a["control_sha"],
                "operation_id": a["operation_id"], "authorization_sha256": common.sha256_value(a),
                "binding_sha256": a["binding_sha256"], "origin_authorization_sha256": policy.ORIGIN_PACKET_SHA256,
                "origin_run_id": policy.ORIGIN_RUN, "check_run_id": a["check_run_id"],
                "app_id_sha256": policy.IDENTITY_PINS["app_id_sha256"],
                "deployment_id_sha256": policy._hash_id(identity), "driver_evidence": a["target_driver_evidence"],
                "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
                "canary_environment_sha256": common.sha256_value(final["canary_environment"]),
                "observed_at": common.format_timestamp(now()), "effects": copy.deepcopy(RECOVER_EFFECTS),
                "health_observations": 2, "synthetic_execution_performed": False}
    except Exception as error:
        # Exactly one bounded observation attempt, never a mutation retry. It
        # cannot turn an ambiguous original response into execution permission.
        try:
            provider.reconcile()
        except Exception:
            pass
        if type(error) is UpdateRejected:
            raise error from None
        raise common.ReleaseError("existing driver recovery stopped") from None


def failure_report(error: Exception) -> dict[str, Any]:
    stage = CURRENT_STAGE if type(CURRENT_STAGE) is str and CURRENT_STAGE in STAGES else "AUTHORIZATION"
    result = {"schema_version": 1, "state": "existing-driver-recovery-stopped",
              "code": "RECOVERY_STOPPED_RECONCILE_ONLY", "stage": stage, "retry_authorized": False}
    if stage == "PROVIDER_PRESTATE" and type(error) is policy.PrestateRejected:
        code = vars(error).get("code")
        if type(code) is str and code in policy.PRESTATE_DIAGNOSTICS:
            result["diagnostic_code"] = code
    if stage == "AUTHORIZATION" and type(error) is AuthorizationRejected:
        code = vars(error).get("code")
        if type(code) is str and code in AUTHORIZATION_DIAGNOSTICS:
            result["diagnostic_code"] = code
    if type(error) is UpdateRejected and type(error.status) is int and 100 <= error.status <= 599:
        result["http_status"] = error.status
    return result


FIXTURE_SECRET_NAMES = frozenset({"CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "CRM_CANARY_FIXTURE_AUTHORITY_JSON",
    "CRM_CANARY_FIXTURE_INPUT_JSON", "CRM_CANARY_FIXTURE_INVERSE_AUTHORITY_JSON", "DO_DRIVER_BOOTSTRAP_CREATE_TOKEN",
    "DO_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_PRODUCTION_FIXTURE_READ_TOKEN", "DO_PRODUCTION_FIXTURE_UPDATE_TOKEN",
    "GH_CANARY_ENVIRONMENT_WRITE_TOKEN", "GH_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_DRIVER_RECOVERY_UPDATE_TOKEN"})


def fixture_metadata(api: Any) -> dict[str, Any]:
    path = fixture.API_PREFIX + "/environments/rereply-production-crm-fixture"
    env = api.get(path)
    require(env.get("name") == "rereply-production-crm-fixture"
            and env.get("deployment_branch_policy") == {"protected_branches": False, "custom_branch_policies": True})
    rules = env.get("protection_rules")
    require(type(rules) is list and len([row for row in rules if row.get("type") == "required_reviewers"
                                        and row.get("reviewers")]) == 1)
    branches = api.pages(path + "/deployment-branch-policies", "branch_policies")
    require(branches["total_count"] == 1 and branches["branch_policies"][0].get("name") == "main"
            and branches["branch_policies"][0].get("type") == "branch")
    result = {"environment": {key: env.get(key) for key in
        ("id", "name", "protection_rules", "deployment_branch_policy")},
        "branch_policies": branches["branch_policies"]}
    for kind in ("secrets", "variables"):
        inventory = api.get(path + "/" + kind + "?per_page=100&page=1")
        count = common.exact_int(inventory.get("total_count"), "fixture metadata count", 0, 100)
        rows = inventory.get(kind)
        require(type(rows) is list and len(rows) == count)
        for row in rows:
            common.exact_keys(row, {"name", "created_at", "updated_at"} | ({"value"} if kind == "variables" else set()),
                              "fixture metadata")
            require(type(row["name"]) is str)
            for key in ("created_at", "updated_at"):
                common.require_timestamp(row[key], "fixture metadata time")
        require(len({row["name"] for row in rows}) == count)
        result[kind] = sorted(rows, key=lambda row: row["name"])
    require({row["name"] for row in result["secrets"]} == FIXTURE_SECRET_NAMES and result["variables"] == [])
    return result


def _deployment_row(value: Any) -> dict[str, Any]:
    require(type(value) is dict)
    keys = ("id", "phase", "created_at", "updated_at", "spec")
    require(all(key in value for key in keys))
    return {key: copy.deepcopy(value[key]) for key in keys}


class ProviderRead:
    """Bounded selected-app, production, inventory and firewall GETs only."""
    def __init__(self, root: Path, a: Any, descriptor: Any, read_token: str, api: Any, reader: Any,
                 *, now: Any = lambda: dt.datetime.now(dt.timezone.utc)):
        try:
            from . import verify_production_plan as planner
        except ImportError:
            import verify_production_plan as planner
        require(not any(key in os.environ for key in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR")))
        self.__token = fixture._secret(read_token)
        self.__opener = fixture._opener()
        self.a, self.d, self.api, self.reader, self.now, self.planner = a, descriptor, api, reader, now, planner
        self.contract = planner.validate_contract(common.load_json(root / "release/deployment/production-app-contract.json",
                                                                    "production contract", canonical=False))
        self.app_id = None
        self.production_id = None
        self.driver_deployment_ids: set[str] = set()
        self.production_deployment_ids: set[str] = set()
        self.new_deployment_id = None
        self.__reconciled = False

    def _get(self, path: str) -> Any:
        require(type(path) is str and not any(part in path for part in policy._FORBIDDEN_ROUTES))
        fixed = {"/v2/account", "/v2/apps?per_page=200&page=1",
                 "/v2/apps/regions", "/v2/apps/tiers/instance_sizes/" + self.d["plan"]["instance_size_slug"],
                 "/v2/databases/" + self.d["plan"]["ledger"]["cluster_id"] + "/firewall"}
        for identity in (self.app_id, self.production_id):
            if identity:
                fixed.add("/v2/apps/" + identity)
                fixed.add("/v2/apps/" + identity + "/deployments?per_page=200&page=1")
                selected_ids = self.driver_deployment_ids if identity == self.app_id else self.production_deployment_ids
                fixed.update("/v2/apps/" + identity + "/deployments/" + dep for dep in selected_ids)
        require(path in fixed)
        raw = fixture._wire(self.__opener, common.API_ORIGIN + path, method="GET",
            headers={"Authorization": "Bearer " + self.__token, "Accept": "application/json"}, maximum=boot.MAX_PUBLIC)
        value = common.loads_strict(raw)
        policy._json_tree(value)
        return value

    def _inventory(self, identity: str, count: int) -> list[dict[str, Any]]:
        raw = self._get("/v2/apps/" + identity + "/deployments?per_page=200&page=1")
        return policy._complete_inventory(raw, "deployments", count)

    def _production(self, summary: Any) -> str:
        with policy.prestate_diagnostic("PRODUCTION_APP_READ"):
            app = self._get("/v2/apps/" + self.production_id).get("app")
        with policy.prestate_diagnostic("PRODUCTION_ACTIVE_IDENTITY"):
            require(type(app) is dict and type(app.get("active_deployment")) is dict)
            active = common.require_uuid(app["active_deployment"].get("id"), "production active")
        self.production_deployment_ids.add(active)
        with policy.prestate_diagnostic("PRODUCTION_DEPLOYMENT_READ"):
            dep = self._get("/v2/apps/" + self.production_id + "/deployments/" + active).get("deployment")
        with policy.prestate_diagnostic("PRODUCTION_TARGET_DESCRIPTOR"):
            target = self.planner.normalize_target_descriptor(common.canonical_payload_bytes({"app_id": self.production_id,
                                                           "default_ingress": app.get("default_ingress")}).decode(), self.contract)
        with policy.prestate_diagnostic("PRODUCTION_PREDECESSOR"):
            expected, images = self.planner.predecessor_provider_expectation(self.contract, {}, None)
        with policy.prestate_diagnostic("PRODUCTION_PROVIDER_VALIDATION"):
            try:
                state, _ = self.planner.provider_state({"app": app}, {"deployment": dep}, self.contract, target, expected, images)
            except self.planner.PlanError as error:
                code = "PRODUCTION_PROVIDER_UNCLASSIFIED"
                # Only an exact planner exception with one exact built-in string
                # may select a fixed source-literal code. Never format error.
                if type(error) is self.planner.PlanError:
                    args = error.args
                    if type(args) is tuple and len(args) == 1 and type(args[0]) is str:
                        code = _PROVIDER_PLAN_REJECTIONS.get(args[0], code)
                raise policy.PrestateRejected(code) from None
        with policy.prestate_diagnostic("PRODUCTION_STATE_DIGEST"):
            require(common.sha256_value(state) == self.a["production_state_sha256"])
        with policy.prestate_diagnostic("PRODUCTION_HISTORY_READ"):
            history = self._inventory(self.production_id, 164)
        with policy.prestate_diagnostic("PRODUCTION_HISTORY_POLICY"):
            records = []
            for row in history:
                require(row.get("phase") in {"ACTIVE", "SUPERSEDED", "CANCELED", "ERROR"})
                require(common.require_timestamp(row.get("created_at"), "production history time")
                    <= common.require_timestamp(dep.get("created_at"), "production active time"))
                common.require_timestamp(row.get("updated_at"), "production history update")
                records.append({key: row[key] for key in ("id", "created_at", "updated_at", "phase")})
            require({row["id"] for row in records if row["phase"] == "ACTIVE"} == {active})
            require(common.sha256_value(sorted(records, key=lambda row: row["id"])) == self.a["production_history_sha256"])
        return common.sha256_value(state)

    def _snapshot(self, deployment_id: str | None = None) -> dict[str, Any]:
        with policy.prestate_diagnostic("SIZE_READ"):
            size = self._get("/v2/apps/tiers/instance_sizes/" + self.d["plan"]["instance_size_slug"]).get("instance_size")
        with policy.prestate_diagnostic("SIZE_POLICY"):
            require(type(size) is dict and size.get("slug") == self.d["plan"]["instance_size_slug"]
                and size.get("usd_per_month") == self.d["plan"]["monthly_usd"])
        with policy.prestate_diagnostic("REGIONS_READ"):
            regions = self._get("/v2/apps/regions").get("regions")
        with policy.prestate_diagnostic("REGIONS_POLICY"):
            require(type(regions) is list and len([row for row in regions if type(row) is dict
            and row.get("slug") == self.d["plan"]["region"] and row.get("disabled", False) is False]) == 1)
        with policy.prestate_diagnostic("APPS_READ"):
            apps_raw = self._get("/v2/apps?per_page=200&page=1")
        with policy.prestate_diagnostic("APPS_INVENTORY"):
            apps = policy._complete_inventory(apps_raw, "apps", 3)
        with policy.prestate_diagnostic("APP_SELECTION"):
            matches = [row for row in apps if policy._hash_id(row["id"]) == policy.IDENTITY_PINS["app_id_sha256"]]
            productions = [row for row in apps if policy._hash_id(row["id"]) == self.contract["provider"]["app_id_sha256"]]
            require(len(matches) == 1 and len(productions) == 1 and matches[0]["id"] != productions[0]["id"])
            if self.app_id is not None:
                require(self.app_id == matches[0]["id"] and self.production_id == productions[0]["id"])
        self.app_id, self.production_id = matches[0]["id"], productions[0]["id"]
        with policy.prestate_diagnostic("APP_IDLE"):
            for row in apps:
                if row["id"] != self.app_id or deployment_id is None:
                    require(not any(row.get(key) for key in ("pending_deployment", "in_progress_deployment", "pinned_deployment")))
        with policy.prestate_diagnostic("DRIVER_HISTORY_READ"):
            inventory = self._inventory(self.app_id, 3 if deployment_id is not None else 2)
        with policy.prestate_diagnostic("DRIVER_HISTORY_POLICY"):
            ids = {row["id"] for row in inventory}
            old_ids = ids if deployment_id is None else ids - {deployment_id}
            require({policy._hash_id(identity) for identity in old_ids} == {
            policy.IDENTITY_PINS["failed_deployment_id_sha256"], policy.IDENTITY_PINS["redeployed_deployment_id_sha256"]})
            if deployment_id is not None:
                require(deployment_id in ids and self.new_deployment_id == deployment_id)
        self.driver_deployment_ids.update(ids)
        with policy.prestate_diagnostic("DRIVER_APP_READ"):
            app = self._get("/v2/apps/" + self.app_id).get("app")
        deps = []
        for row in sorted(inventory, key=lambda item: item["id"]):
            with policy.prestate_diagnostic("DRIVER_DEPLOYMENT_READ"):
                full = self._get("/v2/apps/" + self.app_id + "/deployments/" + row["id"]).get("deployment")
            with policy.prestate_diagnostic("DRIVER_DEPLOYMENT_POLICY"):
                require(full.get("id") == row["id"])
                if deployment_id == row["id"]:
                    require(row.get("phase") in PHASE_RANK and full.get("phase") in PHASE_RANK
                        and PHASE_RANK[full["phase"]] >= PHASE_RANK[row["phase"]])
                else:
                    require(full.get("phase") == row.get("phase"))
                deps.append(_deployment_row(full))
        production = self._production(productions[0])
        with policy.prestate_diagnostic("FIXTURE_METADATA_READ"):
            metadata = fixture_metadata(self.api)
        with policy.prestate_diagnostic("FIXTURE_METADATA_BINDING"):
            require(common.sha256_value(metadata) == self.a["fixture_environment_sha256"])
        app_rows = [{"id": row["id"], "spec": {"name": row["spec"]["name"]}} for row in apps]
        observed_at = common.format_timestamp(self.now())
        with policy.prestate_diagnostic("ACCOUNT_READ"):
            account = self._get("/v2/account").get("account")
        with policy.prestate_diagnostic("FIREWALL_READ"):
            firewall = self._get("/v2/databases/" + self.d["plan"]["ledger"]["cluster_id"] + "/firewall")
        with policy.prestate_diagnostic("CANARY_METADATA_READ"):
            canary = self.reader.snapshot()
        return {"observed_at": observed_at, "account": account,
                "apps": {"apps": sorted(app_rows, key=lambda row: row["id"]), "meta": {"total": 3}, "links": {}},
                "app": app, "deployments": {"deployments": deps, "meta": {"total": len(deps)}, "links": {}},
                "firewall": firewall,
                "canary_environment": canary, "fixture_environment_sha256": common.sha256_value(metadata),
                "production_state_sha256": production}

    def snapshot(self) -> dict[str, Any]:
        require(self.new_deployment_id is None)
        return self._snapshot()

    def observe(self, deployment_id: str) -> dict[str, Any]:
        require(self.new_deployment_id == common.require_uuid(deployment_id, "recovery observation"))
        return self._snapshot(deployment_id)

    def reconcile(self) -> None:
        require(not self.__reconciled and self.app_id is not None)
        self.__reconciled = True
        # Do not select a deployment or adopt state after an ambiguous write.
        self._get("/v2/apps/" + self.app_id)
        self._get("/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1")


class ProviderUpdate(ProviderRead):
    def __init__(self, *args: Any, update_token: str, **kwargs: Any):
        super().__init__(*args, **kwargs)
        self.__update_token = fixture._secret(update_token)
        self.attempted = False

    def update(self, spec: Any) -> Any:
        policy._json_tree(spec)
        require(not self.attempted and self.app_id is not None and self.new_deployment_id is None)
        require(policy._hash_id(self.app_id) == policy.IDENTITY_PINS["app_id_sha256"])
        body = common.canonical_payload_bytes({"spec": spec})
        require(len(body) <= boot.MAX_PUBLIC)
        self.attempted = True  # Burn BEFORE transport; timeout is consumed too.
        try:
            request = urllib.request.Request(common.API_ORIGIN + "/v2/apps/" + self.app_id, data=body, method="PUT",
                headers={"Authorization": "Bearer " + self.__update_token, "Accept": "application/json",
                         "Content-Type": "application/json"})
            with fixture._opener().open(request, timeout=30) as response:
                if response.status != 200:
                    raise UpdateRejected(response.status)
                raw = response.read(boot.MAX_PUBLIC + 1)
                require(len(raw) <= boot.MAX_PUBLIC)
        except urllib.error.HTTPError as error:
            status = error.code
            error.close()
            raise UpdateRejected(status) from None
        except UpdateRejected:
            raise
        except Exception:
            raise UpdateRejected() from None
        value = common.loads_strict(raw)
        policy._json_tree(value)
        app = value.get("app") if type(value) is dict else None
        pending = app.get("pending_deployment") if type(app) is dict else None
        require(type(pending) is dict)
        # Identity is provisional until run_once validates the complete response.
        self.new_deployment_id = common.require_uuid(pending.get("id"), "recovery deployment response")
        self.driver_deployment_ids.add(self.new_deployment_id)
        return value


JOB_NAMES = {"authority": "Verify exact existing-driver recovery authority",
             "check": "Check existing CRM driver without writes",
             "recover": "Recover exact existing CRM driver once",
             "gate": "Exact existing CRM driver recovery gate"}


def _run_identity(run: Any, *, control_sha: str, workflow_id: int, title: str | None = None) -> str:
    policy._json_tree(run)
    require(type(run) is dict and type(run.get("repository")) is dict)
    identity = common.require_run_id(run.get("id"), "recovery run")
    require(type(run.get("workflow_id")) is int and run["workflow_id"] == workflow_id
            and run.get("head_sha") == control_sha and run.get("head_branch") == "main"
            and run.get("path") == WORKFLOW and run.get("event") == "workflow_dispatch"
            and type(run.get("run_attempt")) is int and run["run_attempt"] == 1
            and "previous_attempt_url" in run and run["previous_attempt_url"] is None
            and run["repository"].get("full_name") == common.REPOSITORY)
    if title is not None:
        require(run.get("display_title") == title)
    else:
        require(type(run.get("display_title")) is str and re.fullmatch(
            r"(?:Check|Recover) existing CRM driver [0-9a-f-]{36} [0-9a-f]{64}", run["display_title"]))
    return identity


def _jobs(api: Any, run_id: str, *, mode: str) -> list[dict[str, Any]]:
    require(mode in ("check", "recover"))
    result = api.pages(fixture.API_PREFIX + "/actions/runs/" + run_id + "/attempts/1/jobs", "jobs")
    rows = result["jobs"]
    require(result["total_count"] == len(rows) and 0 < len(rows) <= 4
            and len({row.get("name") for row in rows}) == len(rows))
    for row in rows:
        require(row.get("name") in JOB_NAMES.values() and type(row.get("run_attempt")) is int
                and row["run_attempt"] == 1)
        common.require_run_id(row.get("id"), "recovery job identity")
        require(common.require_run_id(row.get("run_id"), "recovery job run") == run_id)
        opposite = "recover" if mode == "check" else "check"
        if row.get("status") == "completed" and row.get("conclusion") == "skipped":
            require(row["name"] == JOB_NAMES[opposite] and row.get("steps") == [])
        if row.get("started_at") is not None:
            _timestamp(row["started_at"])
        if row.get("completed_at") is not None:
            require(row.get("started_at") is not None)
            started, completed = _timestamp(row["started_at"]), _timestamp(row["completed_at"])
            if completed < started:
                # GitHub can report a one-second clock inversion for a job
                # skipped by the selected mode. It cannot represent a write.
                require(row["name"] == JOB_NAMES[opposite]
                        and row.get("status") == "completed" and row.get("conclusion") == "skipped"
                        and row.get("steps") == []
                        and started - completed <= dt.timedelta(seconds=1))
    return rows


def _artifact_zero(api: Any, run_id: str) -> None:
    require(api.pages(fixture.API_PREFIX + "/actions/runs/" + run_id + "/artifacts", "artifacts")
            == {"total_count": 0, "artifacts": []})


# This is quarantined historical evidence, NEVER a passing predecessor. A
# control repair must not erase burned history or generically admit other heads.
FAILED_CHECK_ID = "35643414394"
FAILED_CHECK_CONTROL = "f4b94fdc82a8c2914c356e2644f7632be4e4851e"
FAILED_CHECK_TITLE = ("Check existing CRM driver 627c314e-14c9-4750-a832-926956bc2324 "
                      "205f27d01a8eb98f8e44c404158f1c83aecd3ca1cb616f78dde94c685a52a080")
FAILED_CHECK_TIMES = {"created_at": "2026-09-21T19:13:05Z", "updated_at": "2026-09-21T19:16:53Z"}
FAILED_CHECK_JOBS = {
    "authority": (106477986928, "success", "2026-09-21T19:13:09Z", "2026-09-21T19:13:20Z"),
    "check": (106478062554, "failure", "2026-09-21T19:15:36Z", "2026-09-21T19:16:46Z"),
    "recover": (106478064026, "skipped", "2026-09-21T19:13:20Z", "2026-09-21T19:13:20Z"),
    "gate": (106479272066, "failure", "2026-09-21T19:16:49Z", "2026-09-21T19:16:52Z"),
}

FAILED_CHECK_2_ID = "35785166926"
FAILED_CHECK_2_CONTROL = "2edb431f7665f725657b1c947b2d9c7bd85340f5"
FAILED_CHECK_2_TITLE = ("Check existing CRM driver bbcffd0e-4c11-47bb-84a6-ca0d5a810d5f "
                        "413f503e9ee1bfe52c97e1a1890fa0a9f774d9ddf72cfaa55345708aae786020")
FAILED_CHECK_2_TIMES = {"created_at": "2026-09-22T21:12:15Z", "updated_at": "2026-09-22T21:21:10Z"}
FAILED_CHECK_2_JOBS = {
    "authority": (106940102160, "success", "2026-09-22T21:12:19Z", "2026-09-22T21:12:31Z"),
    "check": (106940191697, "failure", "2026-09-22T21:20:54Z", "2026-09-22T21:21:03Z"),
    "recover": (106940193638, "skipped", "2026-09-22T21:12:32Z", "2026-09-22T21:12:31Z"),
    "gate": (106943141218, "failure", "2026-09-22T21:21:06Z", "2026-09-22T21:21:09Z"),
}

FAILED_CHECK_3_ID = "35828881657"
FAILED_CHECK_3_CONTROL = "92d1e96964511008e5d5fee12ba8377cb7d6ad01"
FAILED_CHECK_3_TITLE = ("Check existing CRM driver 52b20524-7165-4d3e-af9b-24ff09f59951 "
                        "59e2847c542c356b35b7bcaab8262e575ee041318cada021ca0ac6aaf78241dc")
FAILED_CHECK_3_TIMES = {"created_at": "2026-09-23T06:53:14Z", "updated_at": "2026-09-23T06:55:52Z"}
FAILED_CHECK_3_JOBS = {
    "authority": (107076655961, "success", "2026-09-23T06:53:18Z", "2026-09-23T06:53:31Z"),
    "check": (107076718516, "failure", "2026-09-23T06:54:14Z", "2026-09-23T06:55:48Z"),
    "recover": (107076719437, "skipped", "2026-09-23T06:53:32Z", "2026-09-23T06:53:31Z"),
    "gate": (107077304221, "failure", "2026-09-23T06:55:50Z", "2026-09-23T06:55:52Z"),
}
FAILED_CHECK_4_ID = "35923321107"
FAILED_CHECK_4_CONTROL = "5f1b0851ca3fbefceef307b001e031ad51dcab97"
FAILED_CHECK_4_TITLE = ("Check existing CRM driver edd940e0-a992-47cb-a591-755e6a461697 "
                        "b791b9bd4a88befffc98ddaa3e025f4e9f76e4060e4889d74a928599b98adcdd")
FAILED_CHECK_4_TIMES = {"created_at": "2026-09-23T21:34:49Z", "updated_at": "2026-09-23T21:36:59Z"}
FAILED_CHECK_4_JOBS = {
    "authority": (107392335979, "success", "2026-09-23T21:34:53Z", "2026-09-23T21:35:03Z"),
    "check": (107392401976, "failure", "2026-09-23T21:36:40Z", "2026-09-23T21:36:53Z"),
    "recover": (107392403404, "skipped", "2026-09-23T21:35:03Z", "2026-09-23T21:35:03Z"),
    "gate": (107393024946, "failure", "2026-09-23T21:36:55Z", "2026-09-23T21:36:58Z"),
}
FAILED_CHECK_5_ID = "35982003681"
FAILED_CHECK_5_CONTROL = "b28e6728c13b3980a03c7451a96b5ea4dda6589f"
FAILED_CHECK_5_TITLE = ("Check existing CRM driver 759dedd9-8ba4-4351-a483-d6265256c444 "
                        "6b614a1ad5d772ec37c0a9c01be9023a9f64947f22f8174f7328a99821c67e2b")
FAILED_CHECK_5_TIMES = {"created_at": "2026-09-24T09:33:57Z", "updated_at": "2026-09-24T09:38:52Z"}
FAILED_CHECK_5_JOBS = {
    "authority": (107575873518, "success", "2026-09-24T09:34:01Z", "2026-09-24T09:34:12Z"),
    "check": (107575942127, "failure", "2026-09-24T09:38:03Z", "2026-09-24T09:38:47Z"),
    "recover": (107575944449, "skipped", "2026-09-24T09:34:12Z", "2026-09-24T09:34:12Z"),
    "gate": (107577447061, "failure", "2026-09-24T09:38:50Z", "2026-09-24T09:38:52Z"),
}
FAILED_CHECK_6_ID = "35984481354"
FAILED_CHECK_6_CONTROL = "b28e6728c13b3980a03c7451a96b5ea4dda6589f"
FAILED_CHECK_6_TITLE = ("Check existing CRM driver 74e258c3-f76b-431c-b5e3-530e0b9b13ce "
                        "0f8bdcaac1d448bd9bab05756e8882ce7f8ce358a93eb649817fdab19d4b7604")
FAILED_CHECK_6_TIMES = {"created_at": "2026-09-24T09:58:30Z", "updated_at": "2026-09-24T10:07:32Z"}
FAILED_CHECK_6_JOBS = {
    "authority": (107583843073, "success", "2026-09-24T09:58:34Z", "2026-09-24T09:58:48Z"),
    "check": (107583932101, "failure", "2026-09-24T10:05:08Z", "2026-09-24T10:07:27Z"),
    "recover": (107583933904, "skipped", "2026-09-24T09:58:49Z", "2026-09-24T09:58:48Z"),
    "gate": (107586722596, "failure", "2026-09-24T10:07:29Z", "2026-09-24T10:07:32Z"),
}
FAILED_CHECKS = {
    FAILED_CHECK_ID: (FAILED_CHECK_CONTROL, FAILED_CHECK_TITLE, FAILED_CHECK_TIMES, FAILED_CHECK_JOBS),
    FAILED_CHECK_2_ID: (FAILED_CHECK_2_CONTROL, FAILED_CHECK_2_TITLE, FAILED_CHECK_2_TIMES, FAILED_CHECK_2_JOBS),
    FAILED_CHECK_3_ID: (FAILED_CHECK_3_CONTROL, FAILED_CHECK_3_TITLE, FAILED_CHECK_3_TIMES, FAILED_CHECK_3_JOBS),
    FAILED_CHECK_4_ID: (FAILED_CHECK_4_CONTROL, FAILED_CHECK_4_TITLE, FAILED_CHECK_4_TIMES, FAILED_CHECK_4_JOBS),
    FAILED_CHECK_5_ID: (FAILED_CHECK_5_CONTROL, FAILED_CHECK_5_TITLE, FAILED_CHECK_5_TIMES, FAILED_CHECK_5_JOBS),
    FAILED_CHECK_6_ID: (FAILED_CHECK_6_CONTROL, FAILED_CHECK_6_TITLE, FAILED_CHECK_6_TIMES, FAILED_CHECK_6_JOBS),
}


def _quarantined_failed_check(api: Any, item: Any, workflow_id: int, failed_id: str) -> None:
    """Authenticate only the frozen failed read-only run, including no writes."""
    require(type(workflow_id) is int and workflow_id == 363575080)
    require(failed_id in FAILED_CHECKS)
    control, title, times, jobs_expected = FAILED_CHECKS[failed_id]
    latest = api.get(fixture.API_PREFIX + "/actions/runs/" + failed_id)
    for run in (item, latest):
        require(_run_identity(run, control_sha=control, workflow_id=workflow_id,
                              title=title) == failed_id)
        require(run.get("status") == "completed" and run.get("conclusion") == "failure"
                and all(run.get(key) == value for key, value in times.items()))
    jobs = _jobs(api, failed_id, mode="check")
    require(len(jobs) == 4)
    by_name = {row["name"]: row for row in jobs}
    for key, (identity, conclusion, started, completed) in jobs_expected.items():
        row = by_name[JOB_NAMES[key]]
        require(row["id"] == identity and row.get("status") == "completed"
                and row.get("conclusion") == conclusion and row.get("started_at") == started
                and row.get("completed_at") == completed)
        if key == "recover":
            expected_steps = []
        elif key == "gate":
            expected_steps = [("Set up job", "success"), ("Require exactly the selected recovery mode", "failure"),
                              ("Complete job", "success")]
        else:
            middle = "Validate public authority without private credentials" if key == "authority" \
                else "Rehydrate and inspect only inside the protected boundary"
            expected_steps = [("Set up job", "success"), ("Check out exact protected controls", "success"),
                              (middle, conclusion), ("Post Check out exact protected controls", "success"),
                              ("Complete job", "success")]
        steps = row.get("steps")
        require(type(steps) is list and all(type(step) is dict and step.get("status") == "completed" for step in steps)
                and [(step.get("name"), step.get("conclusion")) for step in steps] == expected_steps)
    _artifact_zero(api, failed_id)


def current_guard(api: Any, root: Path, a: Any, *, now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> str:
    """Current hosted identity, complete burned history and predecessor binding."""
    with authorization_diagnostic("CURRENT_GUARD_HEAD"):
        validate_authorization(a, control_sha=a["control_sha"], now=now())
        sha = fixture._current_guard(api, root, workflow=WORKFLOW)
        require(sha == a["control_sha"] and os.environ.get("RECOVERY_MODE") == a["mode"])
        job_key = os.environ.get("GITHUB_JOB")
        require(job_key in ("authority", a["mode"]))
        branch = api.get(fixture.API_PREFIX + "/branches/main")
        require(branch.get("protected") is True and branch.get("commit", {}).get("sha") == sha)
    if job_key != "authority":
        # The public github.token has no administration-read grant. Full
        # protection is checked by the existing private read token before any
        # fixture login or provider action, never by broadening public authority.
        with authorization_diagnostic("CURRENT_GUARD_PROTECTION"):
            protection = api.get(fixture.API_PREFIX + "/branches/main/protection")
            required = protection.get("required_status_checks")
            contexts = {"test", "lint", "build", "security", "e2e", "tenant-isolation"}
            require(protection.get("enforce_admins", {}).get("enabled") is True
                    and type(required) is dict and required.get("strict") is True
                    and set(required.get("contexts", [])) == contexts
                    and type(required.get("checks")) is list and len(required["checks"]) == 6
                    and {row.get("context") for row in required["checks"]} == contexts
                    and all(type(row.get("app_id")) is int and row["app_id"] == 15368 for row in required["checks"])
                    and protection.get("required_conversation_resolution", {}).get("enabled") is True
                    and protection.get("allow_force_pushes", {}).get("enabled") is False
                    and protection.get("allow_deletions", {}).get("enabled") is False)
            reviews = protection.get("required_pull_request_reviews")
            require(type(reviews) is dict and reviews.get("dismiss_stale_reviews") is True
                    and reviews.get("require_code_owner_reviews") is False
                    and reviews.get("require_last_push_approval") is False
                    and type(reviews.get("required_approving_review_count")) is int
                    and reviews["required_approving_review_count"] == 0
                    and protection.get("restrictions") is None)
            # GitHub omits this optional field when no bypass is configured; some
            # API representations emit null. Any populated allowance fails closed.
            bypass = reviews.get("bypass_pull_request_allowances")
            require(bypass is None or (type(bypass) is dict and set(bypass) == {"users", "teams", "apps"}
                    and all(type(bypass[key]) is list and bypass[key] == [] for key in ("users", "teams", "apps"))))
    endpoint = fixture.API_PREFIX + "/actions/workflows/" + WORKFLOW.rsplit("/", 1)[1]
    workflow = api.get(endpoint)
    require(type(workflow.get("id")) is int and workflow["id"] > 0
            and workflow.get("path") == WORKFLOW and workflow.get("state") == "active")
    inventory = api.pages(endpoint + "/runs", "workflow_runs")
    require(inventory["total_count"] == len(inventory["workflow_runs"]) and inventory["total_count"] > 0)
    current_id = common.require_run_id(os.environ.get("GITHUB_RUN_ID"), "current recovery run")
    seen = set()
    predecessor = None
    current = None
    for item in inventory["workflow_runs"]:
        policy._json_tree(item)
        require(type(item) is dict)
        failed_id = common.require_run_id(item.get("id"), "history identity")
        if failed_id in FAILED_CHECKS:
            require(failed_id not in seen and current_id != failed_id
                    and a["check_run_id"] != failed_id)
            _quarantined_failed_check(api, item, workflow["id"], failed_id)
            seen.add(failed_id)
            continue
        rid = _run_identity(item, control_sha=sha, workflow_id=workflow["id"])
        require(rid not in seen)
        seen.add(rid)
        if rid == current_id:
            _run_identity(item, control_sha=sha, workflow_id=workflow["id"], title=run_title(a))
            require(item.get("status") in ACTIVE_STATUSES and item.get("conclusion") is None)
            current = item
        else:
            # Every previous recover, including a failure/cancellation, consumes
            # this incident. It can never be another retry or new-control escape.
            require(item["display_title"].startswith("Check existing CRM driver ")
                    and item.get("status") == "completed")
            if rid == a["check_run_id"]:
                predecessor = item
    require(current is not None and set(FAILED_CHECKS).issubset(seen))
    with authorization_diagnostic("CURRENT_GUARD_OWN_JOB"):
        created = common.require_timestamp(current.get("created_at"), "recovery run creation")
        require(common.require_timestamp(a["issued_at"], "authority issue") <= created
                < common.require_timestamp(a["expires_at"], "authority expiry") and created <= now())
        _artifact_zero(api, current_id)
        jobs = _jobs(api, current_id, mode=a["mode"])
        own = [row for row in jobs if row["name"] == JOB_NAMES[job_key]]
        require(len(own) == 1 and own[0].get("status") == "in_progress" and own[0].get("conclusion") is None)
        if job_key != "authority":
            authority = [row for row in jobs if row["name"] == JOB_NAMES["authority"]]
            require(len(authority) == 1 and authority[0].get("status") == "completed"
                    and authority[0].get("conclusion") == "success")
    if a["mode"] == "recover":
        require(predecessor is not None and a["check_run_id"] != current_id)
        check_title = "Check existing CRM driver " + a["check_operation_id"] + " " + a["binding_sha256"]
        _run_identity(predecessor, control_sha=sha, workflow_id=workflow["id"], title=check_title)
        require(predecessor.get("status") == "completed" and predecessor.get("conclusion") == "success"
                and created > common.require_timestamp(predecessor.get("updated_at"), "check completion")
                and now() - dt.timedelta(hours=2) <= common.require_timestamp(predecessor.get("created_at"), "check creation"))
        latest = api.get(fixture.API_PREFIX + "/actions/runs/" + a["check_run_id"])
        _run_identity(latest, control_sha=sha, workflow_id=workflow["id"], title=check_title)
        require(latest.get("status") == "completed" and latest.get("conclusion") == "success")
        require(all(latest.get(key) == predecessor.get(key) for key in ("created_at", "updated_at")))
        check_jobs = _jobs(api, a["check_run_id"], mode="check")
        require(len(check_jobs) == 4)
        for row in check_jobs:
            conclusion = "skipped" if row["name"] == JOB_NAMES["recover"] else "success"
            require(row.get("status") == "completed" and row.get("conclusion") == conclusion)
            if conclusion == "success":
                require(type(row.get("steps")) is list and row["steps"]
                        and all(step.get("status") == "completed" and step.get("conclusion") == "success"
                                for step in row["steps"]))
        _artifact_zero(api, a["check_run_id"])
    for status in ACTIVE_STATUSES:
        active = api.pages(fixture.API_PREFIX + "/actions/runs?status=" + status, "workflow_runs")
        require(all(str(row.get("id")) == current_id for row in active["workflow_runs"]))
    return sha


def authenticate_origin(api: Any) -> tuple[dict[str, Any], dict[str, Any]]:
    path = fixture.API_PREFIX + "/actions/runs/" + policy.ORIGIN_RUN
    run = api.get(path)
    artifacts = api.pages(path + "/artifacts", "artifacts")
    jobs = api.pages(path + "/attempts/1/jobs", "jobs")
    expected = {"Verify exact one-time driver setup authority": "success",
        "Check exact private CRM driver prerequisites without writes": "skipped",
        "Install exact isolated CRM canary driver once": "failure",
        "Exact private CRM canary driver setup gate": "failure"}
    require(jobs["total_count"] == 4 and len(jobs["jobs"]) == 4
            and {row.get("name") for row in jobs["jobs"]} == set(expected))
    for row in jobs["jobs"]:
        require(type(row.get("run_attempt")) is int and row["run_attempt"] == 1
                and row.get("status") == "completed" and row.get("conclusion") == expected[row["name"]])
    return run, artifacts


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("validate", "check", "recover"))
    parser.add_argument("--control-root", required=True, type=Path)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args(argv)
    try:
        mark_stage("AUTHORIZATION")
        # Pop ALL private inputs, including both mutation tokens, before any
        # current-guard git command or public-evidence subprocess can run.
        private = {key: os.environ.pop(key, None) for key in PRIVATE_NAMES}
        with authorization_diagnostic("PRIVATE_INPUT_PRESENCE"):
            require(os.environ.get("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN") is None)
        with authorization_diagnostic("PUBLIC_AUTHORITY"):
            mode = os.environ.get("RECOVERY_MODE")
            require(mode in ("check", "recover") and (args.command == "validate" or args.command == mode))
            raw = os.environ.get("RECOVERY_AUTHORIZATION_JSON")
            origin_raw = os.environ.get("ORIGIN_AUTHORIZATION_JSON")
            require(type(raw) is str and len(raw.encode()) <= 32768
                    and type(origin_raw) is str and len(origin_raw.encode()) <= 32768)
            sha = common.require_sha1(os.environ.get("CONTROL_SHA"), "recovery control")
            a = validate_authorization(common.loads_strict(raw), control_sha=sha, now=dt.datetime.now(dt.timezone.utc))
            require(a["mode"] == mode and common.canonical_payload_bytes(a) == raw.encode())
            original = common.loads_strict(origin_raw)
            common.exact_keys(original, boot.AUTH_KEYS, "historical packet")
            require(common.canonical_payload_bytes(original) == origin_raw.encode()
                    and common.sha256_value(original) == policy.ORIGIN_PACKET_SHA256
                    and original["control_sha"] == policy.ORIGIN_CONTROL)
        with authorization_diagnostic("PRIVATE_INPUT_PRESENCE"):
            if args.command == "validate":
                require(all(value is None for value in private.values()) and args.output_dir is None)
            else:
                required = CHECK_PRIVATE_NAMES if mode == "check" else PRIVATE_NAMES
                require(all(type(private[key]) is str and private[key] for key in required)
                        and all(private[key] is None for key in PRIVATE_NAMES - required))
                require((mode == "recover") == (args.output_dir is not None))
        with authorization_diagnostic("CURRENT_GUARD_HEAD"):
            root = args.control_root.resolve(strict=True)
        with authorization_diagnostic("CURRENT_GUARD_TOKEN"):
            token = fixture._secret(os.environ.get("GH_TOKEN"))
            api = boot.GitHubRead(token)
        with authorization_diagnostic("CURRENT_GUARD_HISTORY"):
            current_guard(api, root, a)
        if args.command == "validate":
            return_record = {"schema_version": 1, "state": "existing-driver-public-authority-validated",
                             "authorization_sha256": common.sha256_value(a), "binding_sha256": a["binding_sha256"]}
        else:
            with authorization_diagnostic("DESCRIPTOR_VALIDATION"):
                require(len(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"].encode()) <= 32768
                        and len(private["CRM_CANARY_FIXTURE_INPUT_JSON"].encode()) <= fixture.MAX_BODY_BYTES)
                d = boot.validate_descriptor(common.loads_strict(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"]), original)
                protected = common.loads_strict(private["CRM_CANARY_FIXTURE_INPUT_JSON"])
            with authorization_diagnostic("PRE_RUN_ONCE_SETUP"):
                if args.output_dir is not None:
                    output = args.output_dir.resolve()
                    require(output.parent == Path(os.environ["RUNNER_TEMP"]).resolve(strict=True) and not output.exists())
                try:
                    from . import launch_production_prerequisites as prerequisites
                except ImportError:
                    import launch_production_prerequisites as prerequisites
            with authorization_diagnostic("PINNED_GH_ACQUISITION"):
                gh = fixture._pinned_gh()
            with authorization_diagnostic("PRE_RUN_ONCE_SETUP"):
                evidence = prerequisites.GitHubEvidence(root, transport=prerequisites.GitHubGetOnly(gh=str(gh)))
                reader = boot.EnvironmentReader(token, d["plan"]["github_environment_sha256"])
                provider_args = (root, a, d, private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"], api, reader)
                provider = ProviderRead(*provider_args) if mode == "check" else ProviderUpdate(
                    *provider_args, update_token=private["DO_DRIVER_RECOVERY_UPDATE_TOKEN"])
                writer = None if mode == "check" else boot.EnvironmentWriter(
                    private["GH_CANARY_ENVIRONMENT_WRITE_TOKEN"], gh, d["plan"]["github_environment_sha256"])
                transport = boot.ReadOnlyProductTransport(protected["credentials"]["meta_access_token"])
            return_record = run_once(a, original, d, protected, provider=provider, reader=reader, writer=writer,
                authenticate_origin=lambda: authenticate_origin(api),
                authenticate_fixture=lambda: evidence.authenticate_public_fixture(
                    common.canonical_payload_bytes(original["fixture_evidence"]), sha),
                authenticate_image=lambda: boot.authenticate_driver(api, root, gh, a["target_driver_evidence"], sha,
                                                                    gh_token=token),
                current_guard=lambda: current_guard(api, root, a), rehydrate=fixture.rehydrate,
                transport=transport)
            if mode == "recover":
                output.mkdir(mode=0o700, parents=False, exist_ok=False)
                receipt = common.canonical_file_bytes(return_record)
                (output / "receipt.json").write_bytes(receipt)
                (output / "receipt.sha256").write_text(common.sha256_bytes(receipt) + "\n", encoding="ascii")
        print(common.canonical_payload_bytes(return_record).decode())
        return 0
    except Exception as error:
        print(common.canonical_payload_bytes(failure_report(error)).decode(), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
