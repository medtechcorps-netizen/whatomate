"""Read-only policy and private update-plan kernel for the ONE existing CRM driver.

This is deliberately not an installer, hosted entry point, or recovery executor.
It has no create/update/redeploy/firewall/secret-write implementation. The
separate companion runner must authenticate the original public evidence, reconstruct the
private expected spec using the preserved descriptor, authenticate the separate
target image, check protected-main and production state, and supply two genuine
snapshots to ``inspect_pair``. A validated plan is not execution authority.

Old authorization is historical lineage ONLY. Fresh, short-lived zero-effect
authorization permits inspection, never revives the burned CREATE authority.
All provider objects and secret ciphertexts remain in memory. Only closed codes
and public authority identities leave this module; no received message is copied.
"""
from __future__ import annotations

import copy
from contextlib import contextmanager
import datetime as dt
import os
from typing import Any

try:
    from . import bootstrap_production_crm_canary_driver as boot
except ImportError:
    import bootstrap_production_crm_canary_driver as boot

common = boot.common
ORIGIN_CONTROL = "eae99c5d192e7d0ea83c5e1df4dff3fa2b2b6bb1"
ORIGIN_RUN = "35587339019"
ORIGIN_PACKET_SHA256 = "6b20d285b74bbdbebbe2440a74e0d81c2bcf7291bfa054a55afecc8e51753d0a"
ORIGIN_WORKFLOW_ID = 362262514
ORIGIN_CREATED_AT = "2026-09-21T10:10:42Z"
APP_UPDATED_AT = "2026-09-21T11:58:48Z"
ORIGINAL_DEPLOYMENT_TIMES = {"created_at": "2026-09-21T10:14:36Z",
                             "updated_at": "2026-09-21T10:17:57Z"}
REDEPLOYMENT_TIMES = {"created_at": "2026-09-21T11:55:18Z",
                      "updated_at": "2026-09-21T11:58:47Z"}
LEDGER_ID_SHA256 = "1b3708c8e3cbd0911236eba4809fbf72bf1744d9a40eb5fd9335fa5552c5b153"
IDENTITY_PINS = {
    "app_id_sha256": "0b7b25e1e7cb74a342896bd9fb9fcc82c6c8f9f74aa121cb0e78d555e67b396d",
    "failed_deployment_id_sha256": "77a281f02984ce092c5192a2e4de043ab6efce0c297e4ba4bd9e5dbe1ac7d9ac",
    "redeployed_deployment_id_sha256": "5e348b9e9a7f43f70e6699f3533bb78b49ef0c0c0c90101b1f383b51ea1f03a1",
    "provider_user_id_sha256": "6d610b21ebe872c7773a56520374ed027cfead4fb48d33af0e993e48a61df601",
    "provider_team_id_sha256": "92035fae7ba26dd2d0f86734ff5950f6b07a10d3dc60e9006df75b8b369a1e4b",
}
EFFECTS = {
    "app_creations": 0, "app_updates": 0, "deployments": 0,
    "firewall_updates": 0, "secret_updates": 0, "synthetic_executions": 0,
}
AUTH_KEYS = {
    "schema_version", "kind", "control_sha", "operation_id", "issued_at", "expires_at",
    "origin_authorization_sha256", "identities", "apps_inventory_sha256",
    "production_state_sha256", "fixture_environment_sha256", "firewall_sha256",
    "driver_metadata_sha256", "target_driver_evidence", "effects",
}
SNAPSHOT_KEYS = {
    "observed_at", "account", "apps", "app", "deployments", "firewall",
    "canary_environment", "fixture_environment_sha256", "production_state_sha256",
}
BLOCKERS = frozenset({"DEPLOYMENT_ERROR", "CANARY_CONFIGURATION_MISSING"})
MAX_PAIR_INTERVAL = dt.timedelta(minutes=2)
MIN_PAIR_INTERVAL = dt.timedelta(seconds=3)
_FORBIDDEN_ROUTES = ("/users", "/credentials", "/logs", "/exec")
LOGIN_KEYS = frozenset({"CRM_CANARY_KLINIK_LOGIN_JSON", "CRM_CANARY_NON_KLINIK_LOGIN_JSON"})
PRESTATE_DIAGNOSTICS = frozenset({
    # Runner-owned boundaries. Values are source literals, never wire content.
    "CANARY_ABSENCE", "FIRST_SNAPSHOT", "SECOND_SNAPSHOT", "PRESTATE_GUARD", "PRESTATE_AUTHORIZATION",
    "SIZE_READ", "SIZE_POLICY", "REGIONS_READ", "REGIONS_POLICY", "APPS_READ", "APPS_INVENTORY",
    "APP_SELECTION", "APP_IDLE", "DRIVER_HISTORY_READ", "DRIVER_HISTORY_POLICY", "DRIVER_APP_READ",
    "DRIVER_DEPLOYMENT_READ", "DRIVER_DEPLOYMENT_POLICY", "PRODUCTION_APP_READ", "PRODUCTION_ACTIVE_IDENTITY",
    "PRODUCTION_DEPLOYMENT_READ", "PRODUCTION_STATE", "PRODUCTION_TARGET_DESCRIPTOR",
    "PRODUCTION_PREDECESSOR", "PRODUCTION_PROVIDER_VALIDATION", "PRODUCTION_STATE_DIGEST",
    # Exact, source-literal planner failures only. Unknown/hostile messages
    # remain unclassified; no provider value or exception text is reported.
    "PRODUCTION_PROVIDER_APP_ENVELOPE", "PRODUCTION_PROVIDER_DEPLOYMENT_ENVELOPE",
    "PRODUCTION_PROVIDER_APP_IDENTITY", "PRODUCTION_PROVIDER_APP_SPEC_SHAPE",
    "PRODUCTION_PROVIDER_APP_NAME", "PRODUCTION_PROVIDER_DEFAULT_INGRESS",
    "PRODUCTION_PROVIDER_ACTIVE_MISSING", "PRODUCTION_PROVIDER_ACTIVE_IDENTITY",
    "PRODUCTION_PROVIDER_ACTIVE_PHASE", "PRODUCTION_PROVIDER_NOT_IDLE",
    "PRODUCTION_PROVIDER_DEPLOYMENT_IDENTITY", "PRODUCTION_PROVIDER_DEPLOYMENT_PHASE",
    "PRODUCTION_PROVIDER_DEPLOYMENT_SPEC_SHAPE", "PRODUCTION_PROVIDER_EMBEDDED_SPEC_EQUALITY",
    "PRODUCTION_PROVIDER_LIVE_SPEC_EQUALITY", "PRODUCTION_PROVIDER_RAW_SPEC_DIGEST",
    "PRODUCTION_PROVIDER_ENVIRONMENT_DIGEST", "PRODUCTION_PROVIDER_NON_SOURCE_DIGEST",
    "PRODUCTION_PROVIDER_TOPOLOGY_NAME", "PRODUCTION_PROVIDER_TOPOLOGY_REGION",
    "PRODUCTION_PROVIDER_TOPOLOGY_VPC", "PRODUCTION_PROVIDER_TOPOLOGY_INGRESS",
    "PRODUCTION_PROVIDER_TOPOLOGY_DOMAINS", "PRODUCTION_PROVIDER_TOPOLOGY_DATABASES",
    "PRODUCTION_PROVIDER_TOPOLOGY_COMPONENTS", "PRODUCTION_PROVIDER_APP_UPDATED_AT",
    "PRODUCTION_PROVIDER_ENVIRONMENT_STRUCTURE", "PRODUCTION_PROVIDER_NON_SOURCE_STRUCTURE",
    "PRODUCTION_PROVIDER_SOURCE_AUTHORITY", "PRODUCTION_PROVIDER_UNCLASSIFIED",
    "PRODUCTION_HISTORY_READ", "PRODUCTION_HISTORY_POLICY",
    "FIXTURE_METADATA_READ", "FIXTURE_METADATA_BINDING", "ACCOUNT_READ", "FIREWALL_READ", "CANARY_METADATA_READ",
    # Pure policy boundaries. None identifies a received key, value or object.
    "INSPECTION_JSON", "INSPECTION_AUTHORIZATION", "INSPECTION_ORIGIN", "INSPECTION_OPERATION",
    "EXPECTED_RECONSTRUCTION", "SNAPSHOT_SHAPE", "OBSERVATION_WINDOW", "SNAPSHOT_BINDINGS",
    "ACCOUNT_OWNER", "APP_IDENTITY", "APP_INVENTORY_BINDING", "APP_NAME_BINDING", "DEPLOYMENT_IDENTITIES",
    "DRIVER_METADATA_BINDING", "APP_DEPLOYMENT_SLOTS", "EXPECTED_APP_NAME", "APP_SPEC", "DEPLOYMENT_SPECS",
    "DEPLOYMENT_SPEC_EQUALITY", "FIREWALL_PROJECTION", "LEDGER_IDENTITY", "FIREWALL_CLUSTER_BINDING",
    "FIREWALL_ADMISSION_BINDING", "FIREWALL_ORIGINAL_BINDING", "CANARY_METADATA_BINDING", "CANARY_ABSENT_POLICY",
    "SNAPSHOT_STABLE_PROJECTION", "SPEC_JSON", "SPEC_STRUCTURE", "SPEC_PROJECTION", "SPEC_PUBLIC_EQUALITY",
    "PLAN_JSON", "PLAN_ORIGIN", "PLAN_DESCRIPTOR", "PLAN_EXPECTED_RECONSTRUCTION", "PLAN_TARGET_EVIDENCE",
    "PLAN_TARGET_DISTINCT", "PLAN_OBSERVED_SPEC", "PLAN_SECRET_INVENTORY", "PLAN_LOGIN_SCHEMA",
    "PLAN_APPLY_ALLOWED_VALUES", "PLAN_LOGIN_COPY", "PLAN_TARGET_SPEC", "PLAN_SECRET_PRESERVATION",
    "PLAN_EXACT_DELTA", "PAIR_PLAN_ORDER", "PAIR_TIME", "PAIR_STABILITY",
})


def require(ok: bool) -> None:
    # A single fixed rejection keeps upstream/private values out of exceptions.
    if not ok:
        raise common.ReleaseError("existing-driver recovery inspection rejected")


class PrestateRejected(common.ReleaseError):
    """Only a reviewed, content-free code may leave a diagnostic boundary."""
    def __init__(self, code: str):
        require(type(code) is str and code in PRESTATE_DIAGNOSTICS)
        super().__init__("existing-driver prestate rejected")
        self.code = code


@contextmanager
def prestate_diagnostic(code: str):
    """Annotate failure without formatting, copying or inspecting its payload.

    Preserve a valid exact inner diagnostic; unknown/subclass/hostile errors
    become the outer source-literal code. This changes no acceptance predicate.
    """
    require(type(code) is str and code in PRESTATE_DIAGNOSTICS)
    try:
        yield
    except Exception as error:
        if type(error) is PrestateRejected:
            inner = error.__dict__.get("code")
            if type(inner) is str and inner in PRESTATE_DIAGNOSTICS:
                raise
        raise PrestateRejected(code) from None


def _json_tree(value: Any) -> None:
    """Reject foreign objects before equality, formatting, copying or traversal.

    Wire data is already strict JSON, but the pure API also rejects hostile test
    doubles/caller objects without invoking their magic methods. Bound depth and
    nodes, including cycles, while preserving ordinary finite numeric values
    for the later exact schema checks.
    """
    visited = 0

    def walk(item: Any, depth: int) -> None:
        nonlocal visited
        visited += 1
        require(depth <= 48 and visited <= 100000)
        kind = type(item)
        if kind is dict:
            require(all(type(key) is str for key in item))
            for child in item.values():
                walk(child, depth + 1)
        elif kind is list:
            for child in item:
                walk(child, depth + 1)
        elif kind is float:
            require(item == item and abs(item) != float("inf"))
        else:
            require(kind in (str, int, bool, type(None)))

    walk(value, 0)


def _hash_id(value: Any) -> str:
    common.require_uuid(value, "recovery identity")
    return common.sha256_bytes(value.encode("ascii"))


def validate_target_driver_evidence(value: Any, *, control_sha: str) -> dict[str, Any]:
    """Validate choices only; the protected runner must authenticate evidence.

    Require publication at this exact current control. This pure function does
    not prove signatures, ancestry, current attempts, artifact custody or CI.
    """
    _json_tree(value)
    require(type(control_sha) is str)
    common.require_sha1(control_sha, "target current control")
    evidence = common.exact_keys(value, boot.DRIVER_EVIDENCE_KEYS, "target driver evidence")
    require(common.require_sha1(evidence["control_sha"], "target control") == control_sha)
    for key in ("run_id", "artifact_id"):
        common.require_run_id(evidence[key], "target evidence identity")
    common.require_digest(evidence["artifact_digest"], "target artifact")
    common.require_digest(evidence["digest"], "target image")
    common.require_sha256(evidence["driver_version_sha256"], "target input manifest")
    return copy.deepcopy(evidence)


def validate_recovery_authorization(value: Any, *, control_sha: str,
                                    now: dt.datetime) -> dict[str, Any]:
    _json_tree(value)
    require(type(control_sha) is str and type(now) is dt.datetime
            and type(now.tzinfo) is dt.timezone)
    common.require_sha1(control_sha, "recovery current control")
    a = common.exact_keys(value, AUTH_KEYS, "recovery inspection authority")
    require(type(a["schema_version"]) is int and a["schema_version"] == 1)
    require(a["kind"] == "production-crm-canary-driver-existing-inspection-v1")
    require(common.require_sha1(a["control_sha"], "recovery control") == control_sha)
    common.require_uuid(a["operation_id"], "recovery operation")
    issued = common.require_timestamp(a["issued_at"], "recovery issue time")
    expires = common.require_timestamp(a["expires_at"], "recovery expiry")
    require(issued <= now < expires and dt.timedelta(0) < expires - issued <= dt.timedelta(hours=2))
    require(a["origin_authorization_sha256"] == ORIGIN_PACKET_SHA256)
    require(common.exact_keys(a["identities"], set(IDENTITY_PINS), "recovery identities") == IDENTITY_PINS)
    require(common.exact_keys(a["effects"], set(EFFECTS), "recovery effects") == EFFECTS
            and all(type(v) is int for v in a["effects"].values()))
    for name in ("apps_inventory_sha256", "production_state_sha256", "fixture_environment_sha256",
                 "firewall_sha256", "driver_metadata_sha256"):
        common.require_sha256(a[name], "recovery snapshot binding")
    validate_target_driver_evidence(a["target_driver_evidence"], control_sha=control_sha)
    return copy.deepcopy(a)


def validate_origin(original: Any, descriptor: Any, run: Any, artifacts: Any) -> dict[str, Any]:
    """Validate the immutable historical packet; never substitute a fake 'now'.

    The exact original public packet hash is frozen in reviewed source. Its
    window must have covered the exact historical run, not the present. It is
    never passed to install/check/preflight and cannot authorize a new action.
    """
    for value in (original, descriptor, run, artifacts):
        _json_tree(value)
    original = common.exact_keys(original, boot.AUTH_KEYS, "historical bootstrap packet")
    require(common.sha256_value(original) == ORIGIN_PACKET_SHA256)
    require(original["control_sha"] == ORIGIN_CONTROL)
    require(type(run) is dict and type(run.get("repository")) is dict
            and str(run.get("id")) == ORIGIN_RUN)
    require(type(run.get("workflow_id")) is int and run["workflow_id"] == ORIGIN_WORKFLOW_ID
            and run.get("path") == boot.WORKFLOW)
    require(run.get("head_sha") == ORIGIN_CONTROL and run.get("head_branch") == "main"
            and run.get("event") == "workflow_dispatch"
            and type(run.get("run_attempt")) is int and run["run_attempt"] == 1
            and run.get("previous_attempt_url") is None
            and "previous_attempt_url" in run
            and run.get("display_title") == boot.RUN_TITLES["install"]
            and run.get("status") == "completed" and run.get("conclusion") == "failure"
            and run.get("repository", {}).get("full_name") == common.REPOSITORY
            and run.get("created_at") == ORIGIN_CREATED_AT)
    issued = common.require_timestamp(original["issued_at"], "historical issue time")
    expires = common.require_timestamp(original["expires_at"], "historical expiry")
    created = common.require_timestamp(run["created_at"], "historical run time")
    require(issued <= created < expires and dt.timedelta(0) < expires - issued <= dt.timedelta(hours=2))
    common.exact_keys(artifacts, {"total_count", "artifacts"}, "historical artifact inventory")
    require(type(artifacts["total_count"]) is int and artifacts == {"total_count": 0, "artifacts": []})
    # Includes the unchanged HMAC, plan, original control/operation and scope
    # review. Declarative scope_review is NOT proof of any token's actual scope.
    return boot.validate_descriptor(descriptor, original)


def validate_account_owner(account: Any, app: Any, descriptor: dict[str, Any]) -> None:
    for value in (account, app, descriptor):
        _json_tree(value)
    require(type(account) is dict and account.get("status") == "active" and type(app) is dict)
    team = account.get("team")
    require(type(team) is dict)
    require(_hash_id(account.get("uuid")) == IDENTITY_PINS["provider_user_id_sha256"]
            and account.get("uuid") == descriptor["plan"]["provider_account_uuid"])
    require(_hash_id(team.get("uuid")) == IDENTITY_PINS["provider_team_id_sha256"]
            and app.get("owner_uuid") == team["uuid"])
    # This equality is a separately reviewed observation for this one app/team,
    # not a universal interpretation of DigitalOcean's owner_uuid schema.


class ExistingAppReader:
    """Fixed GET-only provider adapter for exact, previously reconciled IDs.

    Callers may resolve IDs from a bounded list in memory, but BOTH selected UUID
    hashes must match the frozen origin identities. Names/latest are not authority.
    This adapter never reads database user/connection credentials or app logs.
    """
    def __init__(self, read_token: str, app_id: str, deployment_id: str, ledger_id: str,
                 *, redeployed_deployment_id: str):
        require(_hash_id(app_id) == IDENTITY_PINS["app_id_sha256"]
                and _hash_id(deployment_id) == IDENTITY_PINS["failed_deployment_id_sha256"]
                and _hash_id(redeployed_deployment_id) == IDENTITY_PINS["redeployed_deployment_id_sha256"]
                and deployment_id != redeployed_deployment_id)
        require(_hash_id(ledger_id) == LEDGER_ID_SHA256)
        require(not any(name in os.environ for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR")))
        self.__token = boot.fixture._secret(read_token)
        self.__opener = boot.fixture._opener()
        self.__routes = frozenset({
            "/v2/account", "/v2/apps/" + app_id,
            "/v2/apps/" + app_id + "/deployments/" + deployment_id,
            "/v2/apps/" + app_id + "/deployments/" + redeployed_deployment_id,
            "/v2/apps/" + app_id + "/deployments?per_page=200&page=1",
            "/v2/apps?per_page=200&page=1",
            "/v2/databases/" + ledger_id + "/firewall",
        })

    def get(self, path: str) -> Any:
        require(type(path) is str and path in self.__routes
                and not any(part in path for part in _FORBIDDEN_ROUTES))
        raw = boot.fixture._wire(self.__opener, common.API_ORIGIN + path, method="GET",
            headers={"Authorization": "Bearer " + self.__token, "Accept": "application/json"},
            maximum=boot.MAX_PUBLIC)
        return common.loads_strict(raw)


def _complete_inventory(value: Any, key: str, count: int) -> list[dict[str, Any]]:
    require(type(value) is dict and type(value.get(key)) is list
            and type(value.get("meta")) is dict
            and type(value["meta"].get("total")) is int
            and value["meta"]["total"] == count and len(value[key]) == count)
    links = value.get("links", {})
    require(type(links) is dict)
    pages = links.get("pages", {})
    require(type(pages) is dict and ("next" not in pages or pages["next"] is None))
    rows = value[key]
    require(all(type(row) is dict for row in rows))
    ids = [common.require_uuid(row.get("id"), "recovery inventory ID") for row in rows]
    require(len(set(ids)) == count)
    return rows


def _driver_metadata(app: Any, deployments: Any) -> dict[str, Any]:
    """Closed public identity/timestamp binding, never spec/ciphertext hashes."""
    _json_tree(app)
    _json_tree(deployments)
    require(type(app) is dict and type(deployments) is list and len(deployments) == 2)

    def row(value: Any) -> dict[str, str]:
        require(type(value) is dict)
        identity = common.require_uuid(value.get("id"), "driver metadata ID")
        created = common.require_timestamp(value.get("created_at"), "driver creation time")
        updated = common.require_timestamp(value.get("updated_at"), "driver update time")
        require(created <= updated
                and common.format_timestamp(created) == value["created_at"]
                and common.format_timestamp(updated) == value["updated_at"])
        return {"id": identity, "created_at": value["created_at"], "updated_at": value["updated_at"]}

    app_row = row(app)
    require(_hash_id(app_row["id"]) == IDENTITY_PINS["app_id_sha256"]
            and app_row["updated_at"] == APP_UPDATED_AT)
    rows = sorted([row(value) for value in deployments], key=lambda value: value["id"])
    require(len({value["id"] for value in rows}) == 2)
    expected = {IDENTITY_PINS["failed_deployment_id_sha256"]: ORIGINAL_DEPLOYMENT_TIMES,
                IDENTITY_PINS["redeployed_deployment_id_sha256"]: REDEPLOYMENT_TIMES}
    for value in rows:
        identity = _hash_id(value["id"])
        require(identity in expected
                and {key: value[key] for key in ("created_at", "updated_at")} == expected[identity])
    return {"app": app_row, "deployments": rows}


def _private_login(raw: Any, *, versioned: bool) -> dict[str, Any]:
    """Parse one private canonical login with the exact historical/target shape."""
    require(type(raw) is str and len(raw.encode()) <= boot.fixture.MAX_BODY_BYTES)
    value = common.loads_strict(raw)
    require(common.canonical_payload_bytes(value).decode() == raw)
    common.exact_keys(value, {"email", "password", "schema_version"} if versioned
                      else {"email", "password"}, "private login")
    require(all(type(value[key]) is str and value[key] for key in ("email", "password")))
    if versioned:
        require(type(value["schema_version"]) is int and value["schema_version"] == 1)
    return value


def historical_expected_spec(original: Any, descriptor: Any, protected: Any,
                             reconstructed_fixture: Any) -> dict[str, Any]:
    """Reconstruct ONLY this original control's unversioned private login shape.

    The current generator correctly includes schema_version=1. The exact pinned
    incident did not. Convert only those two members in memory; do not pretend
    the corrected current generator describes the historical submitted bytes.
    Fixture authentication/rehydration remains a protected-runner obligation.
    """
    for value in (original, descriptor, protected, reconstructed_fixture):
        _json_tree(value)
    common.exact_keys(original, boot.AUTH_KEYS, "historical bootstrap packet")
    require(common.sha256_value(original) == ORIGIN_PACKET_SHA256
            and original.get("control_sha") == ORIGIN_CONTROL)
    d = boot.validate_descriptor(descriptor, original)
    result = boot.runtime_spec(original, d, protected, reconstructed_fixture)
    logins = [row for row in result["services"][0]["envs"] if row["key"] in LOGIN_KEYS]
    require(len(logins) == 2 and {row["key"] for row in logins} == LOGIN_KEYS)
    for row in logins:
        value = _private_login(row["value"], versioned=True)
        del value["schema_version"]
        row["value"] = common.canonical_payload_bytes(value).decode()
        _private_login(row["value"], versioned=False)
    return result


def _validate_expected_spec(original: dict[str, Any], descriptor: dict[str, Any], expected: Any) -> None:
    """Do not let a caller weaken the pinned plan through an arbitrary spec.

    Reconstruct the entire shape with the pinned historical wrapper. The
    fixed HMAC and fixture descriptor must match original custody; only the
    fixture login passwords/Meta secret still rely on authenticated private
    rehydration by the future protected runner, never provider decryption.
    """
    require(type(expected) is dict and type(expected.get("services")) is list
            and len(expected["services"]) == 1 and type(expected["services"][0]) is dict)
    service = expected["services"][0]
    require(type(service.get("envs")) is list and len(service["envs"]) == 7)
    values = {}
    for item in service["envs"]:
        require(type(item) is dict and type(item.get("key")) is str
                and type(item.get("value")) is str and item["key"] not in values)
        values[item["key"]] = item["value"]
    require(set(values) == {
        "CRM_CANARY_HMAC_KEY_BASE64", "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON",
        "CRM_CANARY_KLINIK_LOGIN_JSON", "CRM_CANARY_NON_KLINIK_LOGIN_JSON",
        "CRM_CANARY_META_APP_SECRET", "CRM_CANARY_LEDGER_DATABASE_URL",
        "CRM_CANARY_DRIVER_VERSION_SHA256"})
    require(values["CRM_CANARY_HMAC_KEY_BASE64"] == descriptor["hmac_key_base64"])
    parsed = {}
    for key in ("CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "CRM_CANARY_KLINIK_LOGIN_JSON",
                "CRM_CANARY_NON_KLINIK_LOGIN_JSON"):
        raw = values[key]
        require(len(raw.encode()) <= boot.fixture.MAX_BODY_BYTES)
        parsed[key] = common.loads_strict(raw)
        require(common.canonical_payload_bytes(parsed[key]).decode() == raw)
    login_a = _private_login(values["CRM_CANARY_KLINIK_LOGIN_JSON"], versioned=False)
    login_b = _private_login(values["CRM_CANARY_NON_KLINIK_LOGIN_JSON"], versioned=False)
    protected = {"registration": {"klinik_email": login_a["email"], "non_klinik_email": login_b["email"]},
                 "credentials": {"klinik_password": login_a["password"], "non_klinik_password": login_b["password"],
                                 "meta_app_secret": values["CRM_CANARY_META_APP_SECRET"]}}
    regenerated = historical_expected_spec(original, descriptor, protected,
                                            parsed["CRM_CANARY_FIXTURE_DESCRIPTOR_JSON"])
    # Canonical bytes distinguish integer 1 from boolean true; plain dict
    # equality would incorrectly treat these provider resource values as equal.
    require(common.canonical_payload_bytes(expected) == common.canonical_payload_bytes(regenerated))


def _exact_spec(actual: Any, expected: dict[str, Any]) -> dict[str, Any]:
    with prestate_diagnostic("SPEC_JSON"):
        _json_tree(actual)
        _json_tree(expected)
    with prestate_diagnostic("SPEC_STRUCTURE"):
        require(type(actual) is dict and type(actual.get("services")) is list
                and len(actual["services"]) == 1 and type(actual["services"][0]) is dict)
    # The shared reviewed bootstrap projection recognizes only the exact
    # provider ingress and inert image-only feature; no generic stripping.
    with prestate_diagnostic("SPEC_PROJECTION"):
        normalized = boot.spec_projection(actual, expected)
    with prestate_diagnostic("SPEC_PUBLIC_EQUALITY"):
        public = copy.deepcopy(normalized)
        wanted = copy.deepcopy(expected)
        expected_envs = {row["key"]: row for row in wanted["services"][0]["envs"]}
        for row in public["services"][0]["envs"]:
            if row["type"] == "SECRET":
                row["value"] = expected_envs[row["key"]]["value"]
        wanted["services"][0]["envs"] = sorted(expected_envs.values(), key=lambda row: row["key"])
        require(common.canonical_payload_bytes(public) == common.canonical_payload_bytes(wanted))
    return normalized


def target_update_plan(original: Any, descriptor: Any, expected_spec: Any,
                       target_driver_evidence: Any, observed_app_spec: Any,
                       *, control_sha: str) -> dict[str, Any]:
    """Return a PRIVATE in-memory spec proposal, never a write or public report.

    The original packet/HMAC remain immutable. Exactly four values may change:
    image digest, GENERAL driver version, and the two fixture-derived login
    SECRET values corrected with schema_version=1. The other three ciphertexts
    and provider routing/features remain byte-for-byte equivalent in the copy.
    Evidence authenticity/current authority must be established by a future
    protected runner before it may act on this plan. This is not that runner.
    """
    with prestate_diagnostic("PLAN_JSON"):
        for value in (original, descriptor, expected_spec, target_driver_evidence, observed_app_spec):
            _json_tree(value)
    with prestate_diagnostic("PLAN_ORIGIN"):
        common.exact_keys(original, boot.AUTH_KEYS, "historical bootstrap packet")
        require(common.sha256_value(original) == ORIGIN_PACKET_SHA256
                and original.get("control_sha") == ORIGIN_CONTROL)
    with prestate_diagnostic("PLAN_DESCRIPTOR"):
        d = boot.validate_descriptor(descriptor, original)
    with prestate_diagnostic("PLAN_EXPECTED_RECONSTRUCTION"):
        _validate_expected_spec(original, d, expected_spec)
    with prestate_diagnostic("PLAN_TARGET_EVIDENCE"):
        target = validate_target_driver_evidence(target_driver_evidence, control_sha=control_sha)
    with prestate_diagnostic("PLAN_TARGET_DISTINCT"):
        require(target["digest"] != original["driver_evidence"]["digest"]
                and target["driver_version_sha256"] != original["driver_evidence"]["driver_version_sha256"])
    with prestate_diagnostic("PLAN_OBSERVED_SPEC"):
        stored = _exact_spec(observed_app_spec, expected_spec)
    with prestate_diagnostic("PLAN_SECRET_INVENTORY"):
        before_secrets = {row["key"]: row["value"] for row in stored["services"][0]["envs"]
                          if row["type"] == "SECRET"}
        require(len(before_secrets) == 5)
    with prestate_diagnostic("PLAN_LOGIN_SCHEMA"):
        expected_envs = {row["key"]: row for row in expected_spec["services"][0]["envs"]}
        corrected_logins = {}
        for key in LOGIN_KEYS:
            historical = _private_login(expected_envs[key]["value"], versioned=False)
            corrected = {**historical, "schema_version": 1}
            raw = common.canonical_payload_bytes(corrected).decode()
            _private_login(raw, versioned=True)
            require({name: value for name, value in corrected.items() if name != "schema_version"}
                    == historical)
            corrected_logins[key] = raw
    with prestate_diagnostic("PLAN_APPLY_ALLOWED_VALUES"):
        proposal = copy.deepcopy(observed_app_spec)
        expected_target = copy.deepcopy(expected_spec)
        for spec in (proposal, expected_target):
            service = spec["services"][0]
            service["image"]["digest"] = target["digest"]
            versions = [row for row in service["envs"]
                        if row["key"] == "CRM_CANARY_DRIVER_VERSION_SHA256"]
            require(len(versions) == 1 and versions[0].get("type", "GENERAL") == "GENERAL")
            versions[0]["value"] = target["driver_version_sha256"]
            for row in service["envs"]:
                if row["key"] in LOGIN_KEYS:
                    require(row.get("type") == "SECRET")
                    row["value"] = corrected_logins[row["key"]]
    # spec_projection validates provider ciphertext readback, not an outgoing
    # mixed plaintext/ciphertext update. Check the two new private bytes exactly,
    # then use the OLD opaque login ciphertexts in a separate structural copy.
    # This never decrypts provider values or claims future readback equality.
    with prestate_diagnostic("PLAN_LOGIN_COPY"):
        structural = copy.deepcopy(proposal)
        for row in structural["services"][0]["envs"]:
            if row["key"] in LOGIN_KEYS:
                require(row["value"] == corrected_logins[row["key"]])
                row["value"] = before_secrets[row["key"]]
    with prestate_diagnostic("PLAN_TARGET_SPEC"):
        projected_target = _exact_spec(structural, expected_target)
    with prestate_diagnostic("PLAN_SECRET_PRESERVATION"):
        after_secrets = {row["key"]: row["value"] for row in projected_target["services"][0]["envs"]
                         if row["type"] == "SECRET"}
        require(before_secrets == after_secrets)
    # Check the exact allowed delta independently of public-value projection.
    with prestate_diagnostic("PLAN_EXACT_DELTA"):
        reverted = copy.deepcopy(proposal)
        reverted["services"][0]["image"]["digest"] = observed_app_spec["services"][0]["image"]["digest"]
        old_values = {row["key"]: row["value"] for row in observed_app_spec["services"][0]["envs"]}
        for row in reverted["services"][0]["envs"]:
            if row["key"] in LOGIN_KEYS or row["key"] == "CRM_CANARY_DRIVER_VERSION_SHA256":
                row["value"] = old_values[row["key"]]
        require(common.canonical_payload_bytes(reverted) == common.canonical_payload_bytes(observed_app_spec))
    return proposal


def _snapshot(a: dict[str, Any], d: dict[str, Any], value: Any,
              expected_spec: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
    with prestate_diagnostic("SNAPSHOT_SHAPE"):
        s = common.exact_keys(value, SNAPSHOT_KEYS, "existing driver snapshot")
    with prestate_diagnostic("OBSERVATION_WINDOW"):
        observed = common.require_timestamp(s["observed_at"], "recovery observation")
        issued = common.require_timestamp(a["issued_at"], "recovery issue time")
        expires = common.require_timestamp(a["expires_at"], "recovery expiry")
        require(issued <= observed < expires)
    with prestate_diagnostic("SNAPSHOT_BINDINGS"):
        require(s["production_state_sha256"] == a["production_state_sha256"]
                and s["fixture_environment_sha256"] == a["fixture_environment_sha256"])
    app = s["app"]
    with prestate_diagnostic("ACCOUNT_OWNER"):
        validate_account_owner(s["account"], app, d)
    with prestate_diagnostic("APP_IDENTITY"):
        require(_hash_id(app.get("id")) == IDENTITY_PINS["app_id_sha256"])
    with prestate_diagnostic("APP_INVENTORY_BINDING"):
        apps = _complete_inventory(s["apps"], "apps", 3)
        require(all(type(row.get("spec")) is dict for row in apps))
        app_rows = sorted([{"id": row["id"], "name": row.get("spec", {}).get("name")} for row in apps],
                          key=lambda row: row["id"])
        require(all(type(row["name"]) is str for row in app_rows))
        require(common.sha256_value(app_rows) == a["apps_inventory_sha256"])
    with prestate_diagnostic("APP_NAME_BINDING"):
        require(sum(row["id"] == app["id"] and row["name"] == d["plan"]["app_name"]
                    for row in app_rows) == 1)
        require(sum(row["name"].startswith("rereply-canary-driver-") for row in app_rows) == 1)
    with prestate_diagnostic("DEPLOYMENT_IDENTITIES"):
        deps = _complete_inventory(s["deployments"], "deployments", 2)
        expected_deployments = {IDENTITY_PINS["failed_deployment_id_sha256"],
                                IDENTITY_PINS["redeployed_deployment_id_sha256"]}
        require(len(expected_deployments) == 2
                and {_hash_id(dep["id"]) for dep in deps} == expected_deployments
                and all(dep.get("phase") == "ERROR" for dep in deps))
    with prestate_diagnostic("DRIVER_METADATA_BINDING"):
        driver_metadata = _driver_metadata(app, deps)
        require(common.sha256_value(driver_metadata) == a["driver_metadata_sha256"])
    with prestate_diagnostic("APP_DEPLOYMENT_SLOTS"):
        require(all(app.get(key) is None or type(app[key]) is dict and app[key] == {} for key in
                    ("active_deployment", "pending_deployment", "in_progress_deployment", "pinned_deployment")))
    with prestate_diagnostic("EXPECTED_APP_NAME"):
        require(expected_spec.get("name") == d["plan"]["app_name"])
    with prestate_diagnostic("APP_SPEC"):
        app_spec = _exact_spec(app.get("spec"), expected_spec)
    with prestate_diagnostic("DEPLOYMENT_SPECS"):
        dep_specs = {dep["id"]: _exact_spec(dep.get("spec"), expected_spec) for dep in deps}
    with prestate_diagnostic("DEPLOYMENT_SPEC_EQUALITY"):
        require(all(common.canonical_payload_bytes(app_spec) == common.canonical_payload_bytes(spec)
                    for spec in dep_specs.values()))
    with prestate_diagnostic("FIREWALL_PROJECTION"):
        rules = boot.firewall_projection(s["firewall"])
    with prestate_diagnostic("LEDGER_IDENTITY"):
        require(_hash_id(d["plan"]["ledger"]["cluster_id"]) == LEDGER_ID_SHA256)
    with prestate_diagnostic("FIREWALL_CLUSTER_BINDING"):
        require(all("cluster_uuid" not in row or row["cluster_uuid"] == d["plan"]["ledger"]["cluster_id"]
                    for row in s["firewall"]["rules"]))
    with prestate_diagnostic("FIREWALL_ADMISSION_BINDING"):
        admission = {"type": "app", "value": app["id"]}
        require(sum(rule == admission for rule in rules) == 1
                and common.sha256_value(rules) == a["firewall_sha256"])
    with prestate_diagnostic("FIREWALL_ORIGINAL_BINDING"):
        original_rules = [rule for rule in rules if rule != admission]
        require(common.sha256_value(original_rules) == d["plan"]["ledger"]["firewall_sha256"])
    env = s["canary_environment"]
    with prestate_diagnostic("CANARY_METADATA_BINDING"):
        require(type(env) is dict and common.sha256_value(env) == d["plan"]["github_environment_sha256"])
    with prestate_diagnostic("CANARY_ABSENT_POLICY"):
        require(type(env.get("secrets")) is list and env.get("variables") == []
                and {row.get("name") for row in env["secrets"]}
                    == {"CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY"}
                and len(env["secrets"]) == 2)
    # Compare opaque ciphertexts ONLY in memory, never emit private fingerprints.
    with prestate_diagnostic("SNAPSHOT_STABLE_PROJECTION"):
        stable = {"account_user": s["account"]["uuid"], "account_team": s["account"]["team"]["uuid"],
                  "owner": app["owner_uuid"], "apps": app_rows, "app_id": app["id"],
                  "deployment_ids": sorted(dep_specs), "app_spec": app_spec, "phase": "ERROR",
                  "driver_metadata": driver_metadata,
                  "routing_representation": {"app": "ingress" in app["spec"],
                                             "deployments": {dep["id"]: "ingress" in dep["spec"] for dep in deps}},
                  "feature_representation": {"app": ("features" in app["spec"], app["spec"].get("features")),
                                             "deployments": {dep["id"]: ("features" in dep["spec"], dep["spec"].get("features")) for dep in deps}},
                  "firewall": rules, "canary_environment": env,
                  "production_state_sha256": s["production_state_sha256"],
                  "fixture_environment_sha256": s["fixture_environment_sha256"]}
    return stable, sorted(BLOCKERS)


def inspect_pair(authorization: Any, original: Any, descriptor: Any, origin_run: Any,
                 origin_artifacts: Any, first: Any, second: Any, *, control_sha: str,
                 expected_spec: dict[str, Any], now: dt.datetime) -> dict[str, Any]:
    """Validate two stable read-only snapshots; NEVER return execution readiness.

    expected_spec must be reconstructed from authenticated fixture evidence
    with historical_expected_spec(original, same_descriptor, protected, rehydrated).
    This kernel does not replace the hosted evidence verification/current guards.
    """
    with prestate_diagnostic("INSPECTION_JSON"):
        for value in (authorization, original, descriptor, origin_run, origin_artifacts, first, second, expected_spec):
            _json_tree(value)
    with prestate_diagnostic("INSPECTION_AUTHORIZATION"):
        a = validate_recovery_authorization(authorization, control_sha=control_sha, now=now)
    with prestate_diagnostic("INSPECTION_ORIGIN"):
        d = validate_origin(original, descriptor, origin_run, origin_artifacts)
    with prestate_diagnostic("INSPECTION_OPERATION"):
        require(a["operation_id"] != original["operation_id"])
    with prestate_diagnostic("EXPECTED_RECONSTRUCTION"):
        _validate_expected_spec(original, d, expected_spec)
    before, blockers = _snapshot(a, d, first, expected_spec)
    after, after_blockers = _snapshot(a, d, second, expected_spec)
    first_plan = target_update_plan(original, d, expected_spec, a["target_driver_evidence"],
                                    first["app"]["spec"], control_sha=control_sha)
    second_plan = target_update_plan(original, d, expected_spec, a["target_driver_evidence"],
                                     second["app"]["spec"], control_sha=control_sha)
    # Environment ordering is not authority, while the private returned plan
    # must preserve each original representation. Compare sorted copies only.
    with prestate_diagnostic("PAIR_PLAN_ORDER"):
        comparable_plans = []
        for plan in (first_plan, second_plan):
            comparable = copy.deepcopy(plan)
            comparable["services"][0]["envs"].sort(key=lambda row: row["key"])
            comparable_plans.append(comparable)
    with prestate_diagnostic("PAIR_TIME"):
        first_time = common.require_timestamp(first["observed_at"], "first recovery observation")
        second_time = common.require_timestamp(second["observed_at"], "second recovery observation")
        require(MIN_PAIR_INTERVAL <= second_time - first_time <= MAX_PAIR_INTERVAL
                and now - MAX_PAIR_INTERVAL <= second_time <= now)
    with prestate_diagnostic("PAIR_STABILITY"):
        require(before == after and blockers == after_blockers
                and common.canonical_payload_bytes(comparable_plans[0])
                    == common.canonical_payload_bytes(comparable_plans[1]))
    return {"schema_version": 1, "state": "existing-driver-inspection-complete",
            "authorization_sha256": common.sha256_value(a), "origin_run_id": ORIGIN_RUN,
            "app_id_sha256": IDENTITY_PINS["app_id_sha256"],
            "failed_deployment_id_sha256": IDENTITY_PINS["failed_deployment_id_sha256"],
            "redeployed_deployment_id_sha256": IDENTITY_PINS["redeployed_deployment_id_sha256"],
            "deployment_phase": "ERROR", "blockers": blockers,
            "mutation_performed": False, "ready_for_finalize": False, "ready_for_operation": False,
            "plan_validation": "image-version-login-schema-only",
            "planned_image_digest": a["target_driver_evidence"]["digest"],
            "planned_driver_version_sha256": a["target_driver_evidence"]["driver_version_sha256"],
            "historical_configuration_incompatibility": "SCHEMA_VERSION_MISSING",
            "historical_configuration_evidence": "source-construction-only",
            "historical_create_cause": "not-proven", "deployment_failure_cause": "not-proven"}


def failure_report(_error: Exception) -> dict[str, Any]:
    # No exception formatting, upstream reason/code/message, hashes or lengths.
    return {"schema_version": 1, "state": "existing-driver-inspection-rejected",
            "code": "RECOVERY_INSPECTION_REJECTED", "mutation_performed": False,
            "ready_for_finalize": False, "ready_for_operation": False}
