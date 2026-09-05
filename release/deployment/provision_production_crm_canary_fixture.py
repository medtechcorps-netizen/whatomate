#!/usr/bin/env python3
"""Bounded product API fixture provisioning; all effects require an injected gate.

Credential custody and durable registration belong to the protected controller.
This module never persists a login, descriptor, product response, or credential.
The source-policy check here is static; the injected gate must independently
recheck the live provider source before every mutation.
"""

from __future__ import annotations

import base64
import copy
import hashlib
import hmac
import re
import secrets
import sys
from pathlib import Path
from typing import Any
from urllib.parse import urlencode

try:
    from . import verify_production_release as common
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import verify_production_release as common

PRODUCT_ORIGIN = "https://app.rereply.app"
EXPECTED_SOURCE_SHA = "974bb998f6d4c94ce750a92bf23f4550f8e45a2f"
REQUEST_KEYS = {"schema_version", "control_sha", "operation_sha256", "descriptor_sha256"}
PROTECTED_INPUT_KEYS = {"descriptor", "credentials", "registration"}
DESCRIPTOR_KEYS = {
    "schema_version", "product_origin", "fixture_namespace", "source_sha",
    "super_admin_id", "super_admin_home_org_id", "reseller_id",
    "controlled_email_domain", "klinik", "non_klinik", "meta", "conversations",
}
ORG_KEYS = {"organization_name", "full_name", "plan"}
PLAN_KEYS = {
    "plan_id", "plan_code", "plan_name", "vertical", "entitlements_sha256",
    "price_id", "price_code", "currency", "unit_amount_minor", "setup_amount_minor",
    "interval", "interval_count", "tax_behavior",
}
META_KEYS = {"app_id", "config_id", "business_account_id", "phone_number_id",
             "display_phone_number", "account_name", "api_version"}
CONVERSATION_KEYS = {"display_name", "sender_wa_id", "wamid", "timestamp", "body"}
CREDENTIAL_KEYS = {"super_admin_login", "klinik_password", "non_klinik_password",
                   "meta_access_token", "meta_app_secret", "meta_webhook_verify_token"}
REGISTRATION_KEYS = {"klinik_email", "non_klinik_email"}
PUBLIC_STAGES = (
    "create_klinik_org", "license_klinik", "create_klinik_user",
    "create_non_klinik_org", "license_non_klinik", "create_non_klinik_user",
    "configure_meta", "create_account", "webhook_a", "webhook_b", "clear_b",
)
MAX_PAGES = 100
MAX_BODY_BYTES = 128 * 1024
DIGITS = re.compile(r"^[1-9][0-9]{5,31}$")
LABEL = re.compile(r"^[A-Za-z0-9][A-Za-z0-9 ._-]{0,95}$")
DOMAIN = re.compile(r"^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$")
AGENT_PERMISSIONS = frozenset((
    "accounts:read", "chat:read", "chat:write", "contacts:read", "tags:read",
    "analytics.agents:read", "transfers:read", "transfers:write", "transfers:pickup",
    "canned_responses:read", "call_transfers:read", "call_transfers:write",
    "outgoing_calls:read", "outgoing_calls:write", "crm.pipelines:read",
    "crm.leads:read", "crm.leads:write", "crm.automations:read", "tasks:read",
    "tasks:write", "bookings:read", "bookings:write", "booking.settings:read",
    "packages:read", "credits:read", "payments:read", "copilot:read", "copilot:execute",
    "channel_accounts:read", "conversations:read", "conversations:write",
))


def expected_origin_slots() -> list[dict[str, Any]]:
    return [{"stage": stage, "wrapper_upper_bound": 1,
             "nested_upper_bound": 1 if stage == "create_account" else 0}
            for stage in PUBLIC_STAGES]


def _require(condition: bool, label: str) -> None:
    if not condition:
        common.fail(label)


def _schema(value: Any, keys: set[str], label: str) -> dict[str, Any]:
    return common.exact_keys(value, keys, label)


def _secret(value: Any) -> str:
    _require(type(value) is str and 12 <= len(value) <= 4096
             and not any(ord(c) < 33 or ord(c) > 126 for c in value),
             "protected credential is invalid")
    return value


def generate_registration(domain: str) -> dict[str, str]:
    """Call once at durable protected registration, never during resume.

    Independent 256-bit local parts make collision with historical accounts
    negligible; live API absence is not a claim about soft-deleted accounts.
    The caller must persist the resulting registration before any fixture POST.
    """
    common.exact_string(domain, "controlled email domain", DOMAIN)
    return {
        role + "_email": prefix + base64.b32encode(secrets.token_bytes(32)).decode("ascii").rstrip("=").lower() + "@" + domain
        for role, prefix in (("klinik", "k-"), ("non_klinik", "n-"))
    }


def validate_request(value: Any) -> dict[str, Any]:
    req = _schema(value, REQUEST_KEYS, "fixture request")
    _require(type(req["schema_version"]) is int and req["schema_version"] == 1,
             "fixture request schema differs")
    common.require_sha1(req["control_sha"], "control SHA")
    for field in ("operation_sha256", "descriptor_sha256"):
        common.require_sha256(req[field], "request digest")
    return copy.deepcopy(req)


def validate_protected_input(value: Any, request: Any) -> dict[str, Any]:
    req = validate_request(request)
    protected = _schema(value, PROTECTED_INPUT_KEYS, "protected input")
    d = _schema(protected["descriptor"], DESCRIPTOR_KEYS, "selector descriptor")
    _require(type(d["schema_version"]) is int and d["schema_version"] == 1,
             "selector schema differs")
    _require(d["product_origin"] == PRODUCT_ORIGIN and d["fixture_namespace"] == "rereply-canary"
             and d["source_sha"] == EXPECTED_SOURCE_SHA, "product source policy differs")
    for field in ("super_admin_id", "super_admin_home_org_id", "reseller_id"):
        common.require_uuid(d[field], "protected identity")
    domain = common.exact_string(d["controlled_email_domain"], "email domain", DOMAIN)
    _require(len(domain) <= 190, "email domain is too long")
    for role in ("klinik", "non_klinik"):
        org = _schema(d[role], ORG_KEYS, "organization selector")
        for field in ("organization_name", "full_name"):
            label = common.exact_string(org[field], "fixture label", LABEL)
            _require(label.lower().startswith("rereply-canary-"), "fixture namespace differs")
        plan = _schema(org["plan"], PLAN_KEYS, "plan selector")
        for field in ("plan_id", "price_id"):
            common.require_uuid(plan[field], "catalog identity")
        common.require_sha256(plan["entitlements_sha256"], "entitlement digest")
        for field in ("plan_code", "plan_name", "vertical", "price_code", "currency", "tax_behavior"):
            common.exact_string(plan[field], "catalog selector", LABEL)
        _require(plan["interval"] in ("month", "year"), "price interval differs")
        common.exact_int(plan["interval_count"], "price interval count", 1, 12)
        for field in ("unit_amount_minor", "setup_amount_minor"):
            common.exact_int(plan[field], "price amount", 0)
    _require(d["klinik"]["organization_name"] != d["non_klinik"]["organization_name"],
             "fixture organizations are not distinct")
    meta = _schema(d["meta"], META_KEYS, "Meta selector")
    for field in ("app_id", "config_id", "business_account_id", "phone_number_id", "display_phone_number"):
        common.exact_string(meta[field], "Meta identity", DIGITS)
    common.exact_string(meta["api_version"], "Meta version", re.compile(r"^v[1-9][0-9]\.0$"))
    name = common.exact_string(meta["account_name"], "account name", LABEL)
    _require(name.lower().startswith("rereply-canary-"), "account namespace differs")
    conversations = _schema(d["conversations"], {"a", "b"}, "conversation selectors")
    for fixture in conversations.values():
        _schema(fixture, CONVERSATION_KEYS, "conversation selector")
        name = common.exact_string(fixture["display_name"], "display name", LABEL)
        _require(name.lower().startswith("rereply-canary-"), "contact namespace differs")
        common.exact_string(fixture["sender_wa_id"], "sender identity", DIGITS)
        common.exact_string(fixture["wamid"], "message identity", re.compile(r"^[A-Za-z0-9._:-]{1,128}$"))
        common.exact_string(fixture["timestamp"], "message timestamp", re.compile(r"^[1-9][0-9]{9}$"))
        common.exact_string(fixture["body"], "synthetic body", re.compile(r"^rereply-canary-[\x20-\x7e]{1,240}$"))
    for field in ("display_name", "sender_wa_id", "wamid"):
        _require(conversations["a"][field] != conversations["b"][field], "fixtures are not distinct")
    credentials = _schema(protected["credentials"], CREDENTIAL_KEYS, "credential custody")
    login = _schema(credentials["super_admin_login"], {"email", "password"}, "admin login")
    common.exact_string(login["email"], "admin login email", re.compile(r"^[^\s@]+@[^\s@]+\.[^\s@]+$"))
    _secret(login["password"])
    for field in CREDENTIAL_KEYS - {"super_admin_login"}:
        _secret(credentials[field])
    _require(credentials["klinik_password"] != credentials["non_klinik_password"], "fixture credentials are not distinct")
    registration = _schema(protected["registration"], REGISTRATION_KEYS, "issued registration")
    for role, prefix in (("klinik", "k-"), ("non_klinik", "n-")):
        common.exact_string(registration[role + "_email"], "issued fixture email",
                            re.compile(r"^" + prefix + r"[a-z2-7]{51}[aq]@" + re.escape(domain) + r"$"))
    _require(common.sha256_value(d) == req["descriptor_sha256"], "selector digest differs")
    _require(len(common.canonical_payload_bytes(protected)) <= MAX_BODY_BYTES, "protected input is too large")
    return copy.deepcopy(protected)


class ProductProvisioner:
    """Injected transport returns decoded envelopes, never logs response bodies.

    transport.login(email,password) returns an opaque session. Authentication is
    separate from fixture effects and is not replayed automatically. request uses
    (method,path,body,session=,organization_id=,headers=,graph=). Body is a dict.
    gate.once sees a redacted canonical policy, never low-entropy credential hashes.
    Its callback returns only validated, known nonsecret response projections.
    """

    def __init__(self, request: Any, protected_input: Any, transport: Any, gate: Any):
        self.request = validate_request(request)
        self.protected = validate_protected_input(protected_input, self.request)
        self.d = self.protected["descriptor"]
        self.c = self.protected["credentials"]
        self.registration = self.protected["registration"]
        self.transport, self.gate = transport, gate
        self.stages: list[dict[str, Any]] = []
        self.admin = None
        self.failed = False
        self.started = False

    def _request(self, method: str, path: str, body: Any = None, *, session: Any = None,
                 org: str | None = None, graph: bool = False, headers: Any = None) -> Any:
        try:
            result = self.transport.request(method, path, body, session=session,
                                            organization_id=org, headers=headers, graph=graph)
            _require(type(result) is dict, "response shape differs")
            if graph:
                return result
            _require(set(result) <= {"status", "data"} and result.get("status") == "success"
                     and "data" in result, "product response envelope differs")
            return result["data"]
        except Exception:
            self.failed = True
            raise common.ReleaseError("fixture transport did not complete cleanly") from None

    def _get(self, path: str, *, org: str | None = None, session: Any = None, graph: bool = False) -> Any:
        return self._request("GET", path, org=org, session=self.admin if session is None else session, graph=graph)

    def _effect(self, stage: str, method: str, path: str, actual: dict[str, Any],
                projection: Any, *, org: str | None = None, session: Any = None,
                policy: dict[str, Any] | None = None, nested_budget: int = 0,
                headers: Any = None) -> Any:
        _require(not self.failed and stage in PUBLIC_STAGES and stage not in {v["stage"] for v in self.stages},
                 "fixture mutation is not eligible")
        public_body = {"organization_sha256": common.sha256_value(org),
                       "body": actual if policy is None else policy}
        raw = common.canonical_payload_bytes(public_body)
        def send() -> Any:
            result = self._request(method, path, actual, session=self.admin if session is None else session,
                                   org=org, headers=headers)
            return projection(result)
        try:
            result = self.gate.once(stage, method, path, raw, send, nested_budget=nested_budget)
        except Exception:
            self.failed = True
            raise common.AmbiguousMutation("fixture mutation requires protected reconciliation") from None
        self.stages.append({"stage": stage, "request_sha256": common.sha256_bytes(raw),
                            "response_sha256": common.sha256_value(result),
                            "wrapper_upper_bound": 1, "nested_upper_bound": nested_budget})
        return result

    def _pages(self, path: str, key: str, *, org: str, session: Any = None) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        seen: set[str] = set()
        total = None
        for page in range(1, MAX_PAGES + 1):
            data = self._get(path + ("&" if "?" in path else "?") + urlencode({"page": page, "limit": 100}),
                             org=org, session=session)
            _require(type(data) is dict and key in data and type(data[key]) is list,
                     "paginated response differs")
            observed = common.exact_int(data.get("total"), "inventory total", 0, MAX_PAGES * 100)
            _require(data.get("page") == page and data.get("limit") == 100 and
                     (total is None or total == observed), "pagination changed")
            total = observed
            chunk = data[key]
            _require(len(chunk) <= 100 and (chunk or len(rows) == total), "pagination is incomplete")
            for row in chunk:
                _require(type(row) is dict, "inventory row differs")
                if key == "messages":
                    _require(set(row) == {"message", "parts"} and type(row["message"]) is dict
                             and (row["parts"] is None or type(row["parts"]) is list),
                             "message wrapper differs")
                    row = row["message"]
                identity = common.require_uuid(row.get("id"), "inventory identity")
                _require(identity not in seen, "duplicate inventory identity")
                seen.add(identity)
                rows.append(row)
            _require(len(rows) <= total, "inventory exceeds total")
            if len(rows) == total:
                return rows
        common.fail("inventory page bound exceeded")

    @staticmethod
    def _list(data: Any, key: str) -> list[dict[str, Any]]:
        _require(type(data) is dict and type(data.get(key)) is list, "inventory shape differs")
        rows = data[key]
        identities = [common.require_uuid(r.get("id"), "inventory identity")
                      for r in rows if type(r) is dict]
        _require(len(rows) == len(identities) == len(set(identities)), "inventory identities differ")
        return rows

    def _assert_tenant(self, org: str, *, session: Any = None) -> None:
        data = self._get("/api/organizations/current", org=org, session=session)
        _require(type(data) is dict and data.get("id") == org, "selected organization differs")

    def _plan(self, org: str, wanted: dict[str, Any]) -> None:
        rows = self._list(self._get(f"/api/admin/organizations/{org}/product/plans"), "plans")
        matches = [v for v in rows if v["id"] == wanted["plan_id"]]
        _require(len(matches) == 1, "selected plan is unavailable")
        plan = matches[0]
        _require(all(plan.get(k) == wanted[w] for k, w in
                     (("code", "plan_code"), ("name", "plan_name"), ("vertical", "vertical")))
                 and plan.get("status") == "active" and
                 common.sha256_value(plan.get("entitlements")) == wanted["entitlements_sha256"],
                 "selected plan content differs")
        prices = plan.get("prices")
        _require(type(prices) is list and len({v.get("id") for v in prices}) == len(prices),
                 "price inventory differs")
        matches = [v for v in prices if v.get("id") == wanted["price_id"]]
        _require(len(matches) == 1, "selected price is unavailable")
        price = matches[0]
        _require(price.get("assignable") is True and price.get("code") == wanted["price_code"]
                 and all(price.get(k) == wanted[k] for k in
                         ("currency", "unit_amount_minor", "setup_amount_minor", "interval", "interval_count", "tax_behavior")),
                 "selected price content differs")

    def _subscription(self, data: Any, wanted: dict[str, Any]) -> dict[str, Any]:
        _require(type(data) is dict and data.get("plan_id") == wanted["plan_id"]
                 and data.get("plan_price_id") == wanted["price_id"] and
                 data.get("plan_code") == wanted["plan_code"] and data.get("status") == "active"
                 and data.get("provider") == "manual", "subscription differs")
        return {"id": common.require_uuid(data.get("id"), "subscription identity"),
                "plan_id": wanted["plan_id"], "plan_price_id": wanted["price_id"], "status": "active"}

    def _create_org_and_user(self, role: str) -> tuple[str, Any]:
        wanted = self.d[role]
        def org_projection(data: Any) -> dict[str, Any]:
            _require(type(data) is dict and data.get("name") == wanted["organization_name"]
                     and data.get("reseller_id") == self.d["reseller_id"], "created organization differs")
            return {"id": common.require_uuid(data.get("id"), "created organization"), "name": wanted["organization_name"]}
        result = self._effect("create_" + role + "_org", "POST", "/api/organizations",
                              {"name": wanted["organization_name"], "reseller_id": self.d["reseller_id"]}, org_projection)
        org = result["id"]
        self._assert_tenant(org)
        self._plan(org, wanted["plan"])
        path = f"/api/admin/organizations/{org}/subscription"
        self._effect("license_" + role, "PUT", path,
                     {"plan_id": wanted["plan"]["plan_id"], "plan_price_id": wanted["plan"]["price_id"],
                      "status": "active", "manual_reference": "rereply-canary-" + self.request["operation_sha256"]},
                     lambda data: self._subscription(data, wanted["plan"]))
        self._subscription(self._get(path), wanted["plan"])
        roles = self._pages("/api/roles", "roles", org=org)
        agents = [v for v in roles if v.get("name") == "agent"]
        _require(len(agents) == 1, "agent role is not unique")
        agent = agents[0]
        perms = agent.get("permissions")
        _require(agent.get("is_system") is True and agent.get("is_default") is True
                 and type(perms) is list and len(perms) == len(set(perms))
                 and set(perms) == AGENT_PERMISSIONS, "agent policy differs")
        email = self.registration[role + "_email"]
        body = {"email": email, "password": self.c[role + "_password"],
                "full_name": wanted["full_name"], "role_id": agent["id"]}
        def user_projection(data: Any) -> dict[str, Any]:
            _require(type(data) is dict and data.get("email") == email and
                     data.get("full_name") == wanted["full_name"] and
                     data.get("organization_id") == org and data.get("role_id") == agent["id"] and
                     data.get("is_active") is True and data.get("is_super_admin") is False,
                     "created user differs")
            return {"id": common.require_uuid(data.get("id"), "created user"), "role_id": agent["id"], "organization_id": org}
        user = self._effect("create_" + role + "_user", "POST", "/api/users", body, user_projection,
                            org=org, policy={**body, "password": "custody:" + role + "_password"})
        members = self._pages("/api/organizations/members", "members", org=org)
        matches = [v for v in members if v.get("user_id") == user["id"]]
        _require(len(matches) == 1 and matches[0].get("organization_id") == org
                 and matches[0].get("role_id") == agent["id"] and matches[0].get("is_active") is True,
                 "fixture membership is not proven")
        session = self.transport.login(email, self.c[role + "_password"])
        me = self._get("/api/me", session=session)
        _require(me.get("id") == user["id"] and me.get("organization_id") == org
                 and me.get("is_super_admin") is False and me.get("is_active") is True,
                 "fixture login identity differs")
        self._assert_tenant(org, session=session)
        return org, session

    def _integration(self, data: Any, org: str) -> dict[str, Any]:
        meta = self.d["meta"]
        _require(type(data) is dict and data.get("provider") == "meta" and data.get("enabled") is True
                 and data.get("configured") is True, "Meta configuration differs")
        config, credentials = data.get("config", {}), data.get("credentials", {})
        _require(config.get("app_id") == meta["app_id"] and config.get("config_id") == meta["config_id"]
                 and config.get("api_version") == meta["api_version"] and
                 config.get("management_mode") == "workspace" and
                 config.get("webhook_callback_path") == "/api/webhook?workspace=" + org and
                 data.get("status") in ("configured", "connected") and
                 data.get("oauth", {}).get("available") is True and
                 all(type(credentials.get(k)) is dict and credentials[k].get("configured") is True
                     and credentials[k].get("source") == "workspace"
                     for k in ("app_secret", "webhook_verify_token")), "Meta binding differs")
        return {"provider": "meta", "configured": True, "config": {k: config[k] for k in ("app_id", "config_id", "api_version")}}

    def _account(self, data: Any) -> dict[str, Any]:
        meta = self.d["meta"]
        _require(type(data) is dict and data.get("name") == meta["account_name"] and
                 data.get("phone_id") == meta["phone_number_id"] and data.get("business_id") == meta["business_account_id"]
                 and data.get("api_version") == meta["api_version"] and data.get("status") == "active"
                 and data.get("has_access_token") is True and not data.get("warning") and
                 all(data.get(k) is False for k in ("auto_read_receipt", "business_calling_enabled", "is_default_incoming", "is_default_outgoing")),
                 "account readiness differs")
        return {"id": common.require_uuid(data.get("id"), "account identity"), "status": "active"}

    def _graph_phone(self) -> None:
        meta = self.d["meta"]
        cursor,seen,phones,matches = None,set(),set(),0
        for unused in range(MAX_PAGES):
            path = ("/" + meta["api_version"] + "/" + meta["business_account_id"]
                    + "/phone_numbers?fields=id,display_phone_number,verified_name,quality_rating&limit=100")
            if cursor is not None:
                path += "&after=" + cursor
            data = self._get(path,graph=True)
            _require(type(data.get("data")) is list,"Meta phone inventory differs")
            for phone in data["data"]:
                identity = common.exact_string(phone.get("id"),"Meta phone",DIGITS)
                _require(identity not in phones,"duplicate Meta phone")
                phones.add(identity)
                if identity != meta["phone_number_id"]:
                    continue
                display = phone.get("display_phone_number")
                _require(type(display) is str and re.fullmatch(r"\+?[0-9 ()-]{6,48}",display) is not None
                         and re.sub(r"[^0-9]","",display) == meta["display_phone_number"],"Meta display phone differs")
                matches += 1
            paging = data.get("paging",{})
            if not paging.get("next"):
                _require(matches == 1,"exact WABA phone binding missing")
                return
            cursor = common.exact_string(paging.get("cursors",{}).get("after"),"Meta phone cursor",
                                         re.compile(r"^[A-Za-z0-9_-]{1,512}$"))
            _require(cursor not in seen,"Meta phone cursor repeats")
            seen.add(cursor)
        common.fail("Meta phone pagination bound exceeded")

    def _graph_subscription(self) -> None:
        meta = self.d["meta"]
        cursor = None
        seen = set()
        matches = 0
        for unused in range(MAX_PAGES):
            query = {"limit": 100}
            if cursor is not None:
                query["after"] = cursor
            data = self._get("/" + meta["api_version"] + "/" + meta["business_account_id"] + "/subscribed_apps?" + urlencode(query), graph=True)
            _require(type(data.get("data")) is list, "Graph subscription inventory differs")
            for item in data["data"]:
                identity = item.get("whatsapp_business_api_data", {}).get("id")
                common.exact_string(identity, "subscribed app", DIGITS)
                matches += identity == meta["app_id"]
            paging = data.get("paging", {})
            if not paging.get("next"):
                _require(matches == 1, "dedicated Meta subscription is not unique")
                return
            cursor = paging.get("cursors", {}).get("after")
            common.exact_string(cursor, "Graph cursor", re.compile(r"^[A-Za-z0-9_-]{1,512}$"))
            _require(cursor not in seen, "Graph pagination repeats")
            seen.add(cursor)
        common.fail("Graph pagination bound exceeded")

    def _conversation(self, rows: list[dict[str, Any]], selector: dict[str, Any], org: str, account: str) -> dict[str, Any]:
        matches = [v for v in rows if v.get("contact", {}).get("phone_number") == selector["sender_wa_id"]]
        _require(len(matches) == 1, "synthetic conversation is not unique")
        row = matches[0]
        contact, shadow, identity = row.get("contact", {}), row.get("channel_account", {}), row.get("contact_identity", {})
        contact_id = common.require_uuid(row.get("contact_id"), "fixture contact")
        shadow_id = common.require_uuid(row.get("channel_account_id"), "fixture shadow")
        external = "legacy-contact:" + contact_id
        _require(row.get("organization_id") == org and row.get("channel") == "whatsapp" and
                 row.get("external_conversation_id") == external and contact.get("id") == contact_id and
                 contact.get("organization_id") == org and contact.get("profile_name") == selector["display_name"] and
                 contact.get("whatsapp_account") == self.d["meta"]["account_name"] and
                 shadow.get("id") == shadow_id and shadow.get("organization_id") == org and
                 shadow.get("channel") == "whatsapp" and shadow.get("provider") == "meta_legacy" and
                 shadow.get("external_account_id") == "legacy-account:" + account and shadow.get("status") == "active" and
                 shadow.get("has_credentials") is False and
                 all(shadow.get("config", {}).get(k) == v for k, v in
                     {"legacy_read_only": True, "outbound_enabled": False, "reply_route": "chat"}.items()) and
                 all(identity.get(k) == v for k, v in {"organization_id": org, "contact_id": contact_id,
                     "channel_account_id": shadow_id, "channel": "whatsapp", "external_id": external,
                     "address": selector["sender_wa_id"], "normalized_address": selector["sender_wa_id"],
                     "display_name": selector["display_name"], "is_primary": True, "is_verified": True}.items()),
                 "synthetic conversation binding differs")
        common.exact_int(row.get("unread_count"), "fixture unread", 0)
        return {"conversation_id": common.require_uuid(row.get("id"), "fixture conversation"),
                "contact_id": contact_id, "display_name": selector["display_name"],
                "sender_wa_id": selector["sender_wa_id"], "channel_account_id": shadow_id,
                "unread_count": row["unread_count"]}

    def _webhook(self, which: str, org: str) -> None:
        f, m = self.d["conversations"][which], self.d["meta"]
        body = {"object": "whatsapp_business_account", "entry": [{"id": m["business_account_id"],
            "changes": [{"field": "messages", "value": {"messaging_product": "whatsapp",
                "metadata": {"display_phone_number": m["display_phone_number"], "phone_number_id": m["phone_number_id"]},
                "contacts": [{"profile": {"name": f["display_name"]}, "wa_id": f["sender_wa_id"]}],
                "messages": [{"from": f["sender_wa_id"], "id": f["wamid"], "timestamp": f["timestamp"],
                              "text": {"body": f["body"]}, "type": "text"}]}}]}]}
        signature = "sha256=" + hmac.new(self.c["meta_app_secret"].encode("ascii"),
                                         common.canonical_payload_bytes(body), hashlib.sha256).hexdigest()
        def accepted(data: Any) -> dict[str, str]:
            _require(data == {"status": "ok"}, "webhook acknowledgement differs")
            return {"status": "ok"}
        self._effect("webhook_" + which, "POST", "/api/webhook?" + urlencode({"workspace": org}), body,
                     accepted, session=False, headers={"X-Hub-Signature-256": signature})

    def _seed_message(self, selector: dict[str, Any], fixture: dict[str, Any], org: str, session: Any,
                      *, incomplete_ok: bool = False, read: bool = False) -> bool:
        rows = self._pages("/api/conversations/" + fixture["conversation_id"] + "/messages",
                           "messages", org=org, session=session)
        if not rows and incomplete_ok:
            return False
        _require(len(rows) == 1, "seed message inventory differs")
        message = rows[0]
        _require(all(message.get(key) == value for key, value in {
            "organization_id": org, "contact_id": fixture["contact_id"],
            "inbox_conversation_id": fixture["conversation_id"],
            "whatsapp_account": self.d["meta"]["account_name"], "whatsapp_message_id": selector["wamid"],
            "direction": "incoming", "message_type": "text", "content": selector["body"],
            "status": "read" if read else "received", "is_reply": False, "error_message": "",
        }.items()) and message.get("sent_by_user_id") is None,
                 "seed message binding differs")
        return True

    def _settled_fixtures(self, org: str, account: str, session: Any) -> dict[str, Any]:
        # Webhook acknowledgement can precede the best-effort inbox mirror.
        # Only missing expected rows may wait; wrong/excess records fail immediately.
        for attempt in range(31):
            rows = self._pages("/api/conversations", "conversations", org=org, session=session)
            _require(len(rows) <= 2, "fixture conversation inventory excessive")
            fixtures = {}
            for row in rows:
                keys = [k for k in ("a", "b") if row.get("contact", {}).get("phone_number")
                        == self.d["conversations"][k]["sender_wa_id"]]
                _require(len(keys) == 1 and keys[0] not in fixtures, "unexpected fixture conversation")
                key = keys[0]
                fixtures[key] = self._conversation([row], self.d["conversations"][key], org, account)
            ready = [self._seed_message(self.d["conversations"][k], f, org, session, incomplete_ok=True)
                     for k, f in fixtures.items()]
            if len(fixtures) == 2 and all(ready):
                return fixtures
            if attempt < 30:
                time.sleep(2)
        common.fail("expected fixture rows did not settle; webhook replay prohibited")

    def provision(self) -> tuple[dict[str, Any], dict[str, Any]]:
        _require(not self.started, "provisioning cannot be repeated")
        self.started = True
        try:
            login = self.c["super_admin_login"]
            self.admin = self.transport.login(login["email"], login["password"])
            me = self._get("/api/me")
            _require(me.get("id") == self.d["super_admin_id"] and
                     me.get("organization_id") == self.d["super_admin_home_org_id"] and
                     me.get("is_super_admin") is True and me.get("is_active") is True,
                     "provisioning principal differs")
            portfolios = self._list(self._get("/api/resellers"), "resellers")
            matches = [v for v in portfolios if v["id"] == self.d["reseller_id"]]
            _require(len(matches) == 1 and matches[0].get("status") == "active", "portfolio differs")
            portfolio = matches[0]
            _require(common.exact_int(portfolio.get("organization_count"), "organization count") + 2 <=
                     common.exact_int(portfolio.get("max_organizations"), "organization limit"), "portfolio has no fixture capacity")
            orgs = self._list(self._get("/api/organizations"), "organizations")
            for org in orgs:
                _require(not str(org.get("name", "")).lower().startswith("rereply-canary")
                         and not str(org.get("slug", "")).lower().startswith("rereply-canary"), "reserved organization namespace exists")
                self._assert_tenant(org["id"])
                for user in self._pages("/api/users", "users", org=org["id"]):
                    _require(user.get("email") not in self.registration.values(), "issued login collides with a live user")
                for account in self._list(self._get("/api/accounts", org=org["id"]), "accounts"):
                    _require(account.get("phone_id") != self.d["meta"]["phone_number_id"] and
                             account.get("business_id") != self.d["meta"]["business_account_id"], "dedicated Meta identity is already bound")
            klinik, klinik_session = self._create_org_and_user("klinik")
            other, unused_session = self._create_org_and_user("non_klinik")
            _require(klinik != other, "generated organizations are not distinct")
            meta = self.d["meta"]
            body = {"enabled": True, "config": {"app_id": meta["app_id"], "config_id": meta["config_id"]},
                    "credentials": {"app_secret": self.c["meta_app_secret"], "webhook_verify_token": self.c["meta_webhook_verify_token"]}}
            policy = {**body, "credentials": {"app_secret": "custody:meta_app_secret", "webhook_verify_token": "custody:meta_webhook_verify_token"}}
            self._effect("configure_meta", "PUT", "/api/integrations/meta", body,
                         lambda data:self._integration(data,klinik), org=klinik, policy=policy)
            integrations = self._get("/api/integrations", org=klinik).get("integrations")
            _require(type(integrations) is list, "integration inventory differs")
            matches = [v for v in integrations if v.get("provider") == "meta"]
            _require(len(matches) == 1, "Meta integration is not unique")
            self._integration(matches[0],klinik)
            self._graph_phone()
            account_body = {"name": meta["account_name"], "phone_id": meta["phone_number_id"],
                            "business_id": meta["business_account_id"], "api_version": meta["api_version"],
                            "access_token": self.c["meta_access_token"], "auto_read_receipt": False,
                            "business_calling_enabled": False, "is_default_incoming": False, "is_default_outgoing": False}
            account = self._effect("create_account", "POST", "/api/accounts", account_body, self._account,
                                   org=klinik, policy={**account_body, "access_token": "custody:meta_access_token"}, nested_budget=1)["id"]
            accounts = self._list(self._get("/api/accounts", org=klinik), "accounts")
            _require(len(accounts) == 1 and self._account(accounts[0])["id"] == account, "account inventory differs")
            self._account(self._get("/api/accounts/" + account, org=klinik))
            self._graph_subscription()
            for which in ("a", "b"):
                self._webhook(which, klinik)
            fixtures = self._settled_fixtures(klinik,account,klinik_session)
            _require(fixtures["a"]["channel_account_id"] == fixtures["b"]["channel_account_id"] and
                     fixtures["a"]["contact_id"] != fixtures["b"]["contact_id"], "generated fixture identities differ")
            def read_projection(data: Any) -> dict[str, bool]:
                _require(type(data) is dict and data.get("provider_synced") is False
                         and data.get("legacy_state_synced") is True, "fixture read state differs")
                return {"provider_synced": False, "legacy_state_synced": True}
            self._effect("clear_b", "POST", "/api/conversations/" + fixtures["b"]["conversation_id"] + "/read", {},
                         read_projection, org=klinik, session=klinik_session)
            rows = self._pages("/api/conversations", "conversations", org=klinik, session=klinik_session)
            _require(len(rows) == 2, "settled fixture inventory differs")
            b = self._conversation(rows, self.d["conversations"]["b"], klinik, account)
            _require(b["conversation_id"] == fixtures["b"]["conversation_id"] and b["unread_count"] == 0,
                     "fixture B unread is not clear")
            for key in ("a", "b"):
                self._seed_message(self.d["conversations"][key], fixtures[key], klinik, klinik_session, read=key == "b")
            driver = {"schema_version": 1, "product_origin": PRODUCT_ORIGIN, "fixture_namespace": "rereply-canary",
                      "klinik": {"organization_id": klinik,
                                 "conversations": {k: {field: value for field, value in f.items()
                                                       if field not in ("channel_account_id", "unread_count")}
                                                   for k, f in fixtures.items()},
                                 "meta": {"business_account_id": meta["business_account_id"], "phone_number_id": meta["phone_number_id"],
                                          "display_phone_number": meta["display_phone_number"], "channel_account_id": fixtures["a"]["channel_account_id"],
                                          "legacy_account_id": account, "legacy_account_name": meta["account_name"]}},
                      "non_klinik": {"organization_id": other}}
            _require(tuple(s["stage"] for s in self.stages) == PUBLIC_STAGES, "fixture stage inventory differs")
            return driver, {"schema_version": 1, "kind": "crm-canary-fixture-provisioning", **{k: self.request[k] for k in
                            ("control_sha", "operation_sha256", "descriptor_sha256")},
                            "fixture_descriptor_sha256": common.sha256_value(driver), "stages": copy.deepcopy(self.stages),
                            "state": "fixture_rows_verified"}
        except Exception:
            self.failed = True
            raise common.ReleaseError("fixture provisioning stopped; protected reconciliation is required") from None

    def rehydrate(self, terminal_receipt: Any) -> dict[str, Any]:
        """Reconstruct protected fixture data with authentication and GETs only.

        Caller must independently verify the original receipt attestation and
        run/artifact provenance. No missing object can be repaired on this path.
        """
        _require(not self.started, "controller instance was already used")
        self.started = True
        try:
            receipt = _schema(terminal_receipt, {"schema_version", "kind", "control_sha", "operation_sha256",
                "descriptor_sha256", "fixture_descriptor_sha256", "stages", "state"}, "terminal receipt")
            _require(receipt["schema_version"] == 1 and receipt["kind"] == "crm-canary-fixture-provisioning"
                     and receipt["state"] == "fixture_rows_verified" and
                     all(receipt[k] == self.request[k] for k in ("control_sha", "operation_sha256", "descriptor_sha256")),
                     "terminal fixture authority differs")
            common.require_sha256(receipt["fixture_descriptor_sha256"], "fixture digest")
            _require(type(receipt["stages"]) is list and len(receipt["stages"]) == len(PUBLIC_STAGES), "terminal stages differ")
            for stage, slot in zip(receipt["stages"], expected_origin_slots()):
                _schema(stage, {"stage", "wrapper_upper_bound", "nested_upper_bound", "request_sha256", "response_sha256"}, "terminal stage")
                _require(all(stage[k] == slot[k] for k in slot), "terminal stage policy differs")
                common.require_sha256(stage["request_sha256"], "terminal request digest")
                common.require_sha256(stage["response_sha256"], "terminal response digest")
            login = self.c["super_admin_login"]
            self.admin = self.transport.login(login["email"], login["password"])
            me = self._get("/api/me")
            _require(me.get("id") == self.d["super_admin_id"] and me.get("is_super_admin") is True and
                     me.get("is_active") is True and me.get("organization_id") == self.d["super_admin_home_org_id"],
                     "rehydration principal differs")
            orgs = self._list(self._get("/api/organizations"), "organizations")
            reserved = [v for v in orgs if str(v.get("name", "")).lower().startswith("rereply-canary")
                        or str(v.get("slug", "")).lower().startswith("rereply-canary")]
            _require(len(reserved) == 2, "reserved fixture inventory differs")
            found = {}
            for role in ("klinik", "non_klinik"):
                wanted = self.d[role]
                matches = [v for v in reserved if v.get("name") == wanted["organization_name"] and v.get("reseller_id") == self.d["reseller_id"]]
                _require(len(matches) == 1, "rehydration organization differs")
                org = matches[0]["id"]
                self._assert_tenant(org)
                self._plan(org, wanted["plan"])
                self._subscription(self._get(f"/api/admin/organizations/{org}/subscription"), wanted["plan"])
                agents = [v for v in self._pages("/api/roles", "roles", org=org) if v.get("name") == "agent"]
                _require(len(agents) == 1 and agents[0].get("is_system") is True and agents[0].get("is_default") is True and
                         type(agents[0].get("permissions")) is list and
                         len(agents[0]["permissions"]) == len(AGENT_PERMISSIONS) and
                         set(agents[0]["permissions"]) == AGENT_PERMISSIONS, "rehydration role differs")
                users = self._pages("/api/users", "users", org=org)
                matches = [v for v in users if v.get("email") == self.registration[role + "_email"]]
                _require(len(matches) == 1 and matches[0].get("full_name") == wanted["full_name"] and
                         matches[0].get("organization_id") == org and matches[0].get("is_active") is True and
                         matches[0].get("is_super_admin") is False and matches[0].get("role_id") == agents[0]["id"],
                         "rehydration login binding differs")
                user_id = matches[0]["id"]
                original_user = next(s for s in receipt["stages"] if s["stage"] == "create_" + role + "_user")
                _require(common.sha256_value({"id":user_id,"role_id":agents[0]["id"],"organization_id":org})
                         == original_user["response_sha256"],"original fixture principal differs")
                members = [v for v in self._pages("/api/organizations/members", "members", org=org) if v.get("user_id") == user_id]
                _require(len(members) == 1 and members[0].get("organization_id") == org and members[0].get("role_id") == agents[0]["id"]
                         and members[0].get("is_active") is True, "rehydration membership differs")
                session = self.transport.login(self.registration[role + "_email"], self.c[role + "_password"])
                me = self._get("/api/me", session=session)
                _require(me.get("id") == user_id and me.get("organization_id") == org and
                         me.get("is_super_admin") is False and me.get("is_active") is True, "rehydration session differs")
                self._assert_tenant(org, session=session)
                found[role] = (org, session)
            klinik, session = found["klinik"]
            other = found["non_klinik"][0]
            _require(klinik != other, "rehydrated organizations are not distinct")
            integrations = self._get("/api/integrations", org=klinik).get("integrations")
            _require(type(integrations) is list, "rehydrated integrations differ")
            metas = [v for v in integrations if v.get("provider") == "meta"]
            _require(len(metas) == 1, "rehydrated Meta is not unique")
            self._integration(metas[0],klinik)
            self._graph_phone()
            accounts = self._list(self._get("/api/accounts", org=klinik), "accounts")
            _require(len(accounts) == 1, "rehydrated account inventory differs")
            account = self._account(accounts[0])["id"]
            self._account(self._get("/api/accounts/" + account, org=klinik))
            self._graph_subscription()
            rows = self._pages("/api/conversations", "conversations", org=klinik, session=session)
            _require(len(rows) == 2, "rehydrated conversation inventory differs")
            fixtures = {k: self._conversation(rows, self.d["conversations"][k], klinik, account) for k in ("a", "b")}
            for key in ("a", "b"):
                self._seed_message(self.d["conversations"][key], fixtures[key], klinik, session, read=key == "b")
            _require(fixtures["b"]["unread_count"] == 0 and fixtures["a"]["channel_account_id"] == fixtures["b"]["channel_account_id"]
                     and fixtures["a"]["contact_id"] != fixtures["b"]["contact_id"], "rehydrated fixture state differs")
            meta = self.d["meta"]
            driver = {"schema_version": 1, "product_origin": PRODUCT_ORIGIN, "fixture_namespace": "rereply-canary",
                      "klinik": {"organization_id": klinik, "conversations": {
                          k: {field: value for field, value in f.items() if field not in ("channel_account_id", "unread_count")}
                          for k, f in fixtures.items()}, "meta": {
                              "business_account_id": meta["business_account_id"], "phone_number_id": meta["phone_number_id"],
                              "display_phone_number": meta["display_phone_number"], "channel_account_id": fixtures["a"]["channel_account_id"],
                              "legacy_account_id": account, "legacy_account_name": meta["account_name"]}},
                      "non_klinik": {"organization_id": other}}
            _require(common.sha256_value(driver) == receipt["fixture_descriptor_sha256"], "rehydrated descriptor digest differs")
            return driver
        except Exception:
            self.failed = True
            raise common.ReleaseError("fixture rehydration did not prove the original descriptor") from None


def rehydrate(original_public_request: Any, protected_input: Any, terminal_receipt: Any, transport: Any) -> dict[str, Any]:
    """Read-only entry point; the caller verifies original signed receipt authority."""
    checked = validate_terminal_result(terminal_receipt)
    product = ProductProvisioner(original_public_request, protected_input, transport, None)
    _require(common.sha256_value(product.registration) == checked["registration_sha256"],
             "original signed login registration differs")
    receipt = {k: checked[k] for k in ("schema_version", "kind", "control_sha", "operation_sha256",
               "descriptor_sha256", "fixture_descriptor_sha256", "stages")}
    receipt["state"] = "fixture_rows_verified"
    return product.rehydrate(receipt)


# CLAIM_ADAPTER_AND_CLI

# Runtime authority below is separate from the injected product policy above.
import argparse
import datetime as dt
import http.cookiejar
import io
import json
import os
import ssl
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

WORKFLOW_PATH = ".github/workflows/provision-production-crm-canary-fixture.yml"
CLEANUP_WORKFLOW_PATH = ".github/workflows/cleanup-production-crm-canary-fixture.yml"
EXECUTOR_JOB = "Execute exact CRM canary fixture bootstrap"
WORKFLOW_JOB_NAMES = (
    "Prepare and attest CRM canary fixture intent", "Test exact fixture claim adapter",
    EXECUTOR_JOB, "Reconcile exact CRM canary fixture state", "Exact CRM fixture control gate",
)
UPLOAD_COMMIT = "ea165f8d65b6e75b540449e92b4886f43607fa02"
UPLOAD_BUNDLE_SHA256 = "0165b8a75330f3228f2c7a234b4ff8a107b9139c2b519147f4c8e9fe99b262d8"
UPLOAD_BUNDLE_BLOB = "89238fa3eb49937ea82c5d82006ee1fc6c6abaae"
CLAIM_CONFLICT_EXIT = 73
# Probe-only classifier for the exact pinned core.setFailed/CreateArtifact path.
# All child streams are suppressed; only this reserved exit status is observable.
# Unexpected native exits (including 73) are normalized to failure by the hook.
CLAIM_PROBE_WRAPPER = r'''
const exact = '::error::Failed to CreateArtifact: Received non-retryable error: Failed request: (409) Conflict: an artifact with this name already exists on the workflow run\n';
let matches = 0, bad = false, pending = '';
const line = value => {
  if (value === exact) matches++;
  else if (value.includes('::error')) bad = true;
};
const sink = stderr => (chunk, encoding, callback) => {
  const value = Buffer.isBuffer(chunk) ? chunk.toString('utf8') : String(chunk);
  if (Buffer.byteLength(value, 'utf8') > 8192 || (stderr && value.length)) bad = true;
  else if (!bad) {
    pending += value;
    let end;
    while ((end = pending.indexOf('\n')) !== -1) {
      const complete = pending.slice(0, end + 1);
      if (Buffer.byteLength(complete, 'utf8') > 8192) bad = true;
      else line(complete);
      pending = pending.slice(end + 1);
    }
    if (Buffer.byteLength(pending, 'utf8') > 8192) { bad = true; pending = ''; }
  }
  const done = typeof encoding === 'function' ? encoding : callback;
  if (typeof done === 'function') done();
  return true;
};
process.stdout.write = sink(false);
process.stderr.write = sink(true);
process.on('exit', code => {
  if (pending) line(pending);
  process.exitCode = code === 1 && matches === 1 && !bad ? 73 :
    code === 0 && matches === 0 && !bad ? 0 : 1;
});
require(process.argv[1]);
'''


class ClaimConflict(common.ReleaseError):
    """Probe-only exact backend duplicate rejection; never a send permission."""
INTENT_PREDICATE = "https://rereply.app/attestations/crm-canary-fixture-intent/v1"
STAGES = PUBLIC_STAGES + ("append_klinik_allowlist",)
INTENT_KEYS = {
    "schema_version", "kind", "control_sha", "origin_run_id", "origin_run_attempt",
    "executor_job", "workflow_path", "workflow_sha256", "controller_sha256",
    "upload_bundle_sha256", "request", "slots", "issued_at", "expires_at",
}
ORIGIN_DESCRIPTOR_KEYS = {"run_id", "artifact_id", "artifact_digest", "intent_sha256"}
MAX_DOWNLOAD = 64 * 1024 * 1024
API_PREFIX = "/repos/" + common.REPOSITORY
ALLOWLIST_KEY = "WHATOMATE_LEGACY_WHATSAPP_REPLY__ALLOWED_ORGANIZATION_IDS"
ENABLE_KEY = "WHATOMATE_LEGACY_WHATSAPP_REPLY__ENABLED"


def stage_slots() -> list[dict[str, Any]]:
    return [{"stage": s, "wrapper_upper_bound": 1,
             "nested_upper_bound": int(s == "create_account")} for s in STAGES]


def validate_origin_intent(value: Any, *, now: dt.datetime | None = None) -> dict[str, Any]:
    v = _schema(value, INTENT_KEYS, "origin intent")
    _require(type(v["schema_version"]) is int and v["schema_version"] == 1
             and v["kind"] == "crm-canary-fixture-intent", "origin schema differs")
    common.require_sha1(v["control_sha"], "origin control")
    common.require_run_id(v["origin_run_id"], "origin run")
    _require(type(v["origin_run_attempt"]) is int and v["origin_run_attempt"] == 1
             and v["executor_job"] == EXECUTOR_JOB and v["workflow_path"] == WORKFLOW_PATH,
             "origin execution identity differs")
    for key in ("workflow_sha256", "controller_sha256", "upload_bundle_sha256"):
        common.require_sha256(v[key], "origin content pin")
    _require(v["upload_bundle_sha256"] == UPLOAD_BUNDLE_SHA256, "claim bundle differs")
    req = validate_request(v["request"])
    _require(req["control_sha"] == v["control_sha"] and v["slots"] == stage_slots(),
             "origin request or slot policy differs")
    issued = common.require_timestamp(v["issued_at"], "origin issue time")
    expires = common.require_timestamp(v["expires_at"], "origin expiry")
    _require(common.format_timestamp(issued) == v["issued_at"]
             and common.format_timestamp(expires) == v["expires_at"], "origin clock encoding differs")
    for slot in v["slots"]:
        _require(type(slot["wrapper_upper_bound"]) is int and type(slot["nested_upper_bound"]) is int,
                 "slot number encoding differs")
    _require(0 < (expires-issued).total_seconds() <= 86400, "origin lifetime differs")
    if now is not None:
        _require(issued <= now < expires, "origin is not currently valid")
    return copy.deepcopy(v)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:
        raise common.ReleaseError("redirect prohibited")


class _CaptureRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:
        return None


def _opener(cookie_jar: Any = None) -> Any:
    handlers = [urllib.request.ProxyHandler({}), _NoRedirect(),
                urllib.request.HTTPSHandler(context=ssl.create_default_context())]
    if cookie_jar is not None:
        handlers.append(urllib.request.HTTPCookieProcessor(cookie_jar))
    return urllib.request.build_opener(*handlers)


def _wire(opener: Any, url: str, *, method: str = "GET", headers: Any = None,
          body: bytes | None = None, maximum: int = MAX_BODY_BYTES) -> bytes:
    """Exactly one application attempt. No automatic redirects or POST retries."""
    try:
        request = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
        with opener.open(request, timeout=30) as response:
            _require(response.status in (200, 201, 202, 204), "HTTP result differs")
            raw = response.read(maximum + 1)
            _require(len(raw) <= maximum, "HTTP response exceeds bound")
            return raw
    except Exception:
        raise common.ReleaseError("bounded HTTP operation failed") from None


class ProductHTTP:
    """Fixed product and Graph origins; opaque per-login cookies stay in memory."""
    def __init__(self, meta_token: str):
        self.__meta_token = _secret(meta_token)
        self.__anonymous = _opener()
        self.__sessions: dict[str, tuple[Any, Any]] = {}

    def __repr__(self) -> str:
        return "<ProductHTTP:redacted>"

    def login(self, email: str, password: str) -> str:
        jar = http.cookiejar.CookieJar()
        op = _opener(jar)
        raw = _wire(op, PRODUCT_ORIGIN + "/api/auth/login", method="POST",
                    headers={"Content-Type":"application/json", "Origin":PRODUCT_ORIGIN},
                    body=common.canonical_payload_bytes({"email":email,"password":password}))
        data = common.loads_strict(raw)
        _require(type(data) is dict and data.get("status") == "success", "login failed")
        tokens = {c.name:c.value for c in jar if c.name in ("whm_access", "whm_csrf")}
        _require(set(tokens) == {"whm_access", "whm_csrf"}, "login cookies absent")
        handle = secrets.token_hex(32)
        self.__sessions[handle] = (op, jar)
        return handle

    def request(self, method: str, path: str, body: Any = None, *, session: Any = None,
                organization_id: str | None = None, headers: Any = None, graph: bool = False) -> Any:
        _require(method in ("GET","POST","PUT"), "product method differs")
        _require(type(path) is str and path.startswith("/") and not path.startswith("//")
                 and not any(c in path for c in ("#", "\r", "\n", "\\", "\x00"))
                 and ".." not in urllib.parse.unquote(path), "product path differs")
        h = dict(headers or {})
        _require(set(h) <= {"X-Hub-Signature-256"}, "caller header differs")
        if graph:
            _require(method == "GET",
                     "Graph mutation prohibited")
            _require(re.fullmatch(r"/v[1-9][0-9]\.0/[1-9][0-9]*/subscribed_apps\?limit=100(?:&after=[A-Za-z0-9_-]{1,512})?", path)
                     is not None or re.fullmatch(r"/v[1-9][0-9]\.0/[1-9][0-9]*/phone_numbers\?fields=id,display_phone_number,verified_name,quality_rating&limit=100(?:&after=[A-Za-z0-9_-]{1,512})?",path)
                     is not None, "Graph path differs")
            h["Authorization"] = "Bearer " + self.__meta_token
            return common.loads_strict(_wire(self.__anonymous, "https://graph.facebook.com"+path, headers=h))
        _require(path.startswith("/api/"), "product route differs")
        op = self.__anonymous
        if session is False:
            _require(method == "POST" and re.fullmatch(r"/api/webhook\?workspace=[0-9a-f-]{36}",path)
                     is not None and set(h) == {"X-Hub-Signature-256"}, "anonymous fixture boundary differs")
        elif session is not None:
            _require(session in self.__sessions, "unknown product session")
            op, jar = self.__sessions[session]
            if method != "GET":
                csrf = [c.value for c in jar if c.name == "whm_csrf"]
                _require(len(csrf) == 1, "CSRF cookie differs")
                h["X-CSRF-Token"] = csrf[0]
        if organization_id is not None:
            h["X-Organization-ID"] = common.require_uuid(organization_id, "tenant")
        h["Origin"] = PRODUCT_ORIGIN
        if body is not None:
            h["Content-Type"] = "application/json"
        raw = _wire(op, PRODUCT_ORIGIN + path, method=method, headers=h,
                    body=None if body is None else common.canonical_payload_bytes(body))
        # The public webhook uses a deliberately smaller acknowledgement envelope.
        if path.startswith("/api/webhook?"):
            result = common.loads_strict(raw)
            if result == {"status":"ok"}:
                return {"status":"success","data":result}
        return common.loads_strict(raw)


class GitHubRead:
    def __init__(self, token: str):
        self.__token = _secret(token)
        self.opener = _opener()

    def get(self, path: str) -> Any:
        _require(path.startswith(API_PREFIX+"/") and "\\" not in path
                 and ".." not in path and "#" not in path, "GitHub route differs")
        raw = _wire(self.opener, "https://api.github.com"+path,
                    headers={"Authorization":"Bearer "+self.__token,
                             "Accept":"application/vnd.github+json",
                             "X-GitHub-Api-Version":"2022-11-28"},
                    maximum=MAX_DOWNLOAD)
        return common.loads_strict(raw)

    def pages(self, path: str, key: str) -> dict[str, Any]:
        rows: list[Any] = []
        total = None
        for page in range(1, 101):
            v = self.get(path+("&" if "?" in path else "?")+"per_page=100&page="+str(page))
            _require(type(v) is dict and type(v.get(key)) is list, "GitHub inventory differs")
            count = common.exact_int(v.get("total_count"), "GitHub total", 0, 10000)
            _require(total in (None, count), "GitHub inventory moved")
            total = count
            _require(len(v[key]) <= 100 and (v[key] or len(rows) == count), "GitHub page incomplete")
            rows.extend(v[key])
            _require(len(rows) <= count, "GitHub inventory excess")
            if len(rows) == count:
                _require(len({r["id"] for r in rows}) == len(rows), "GitHub duplicate records")
                return {"total_count":count,key:rows}
        common.fail("GitHub inventory exceeds bound")

    def artifact(self, artifact_id: str, expected_digest: str) -> bytes:
        common.require_run_id(artifact_id, "artifact ID")
        common.require_digest(expected_digest, "artifact digest")
        req = urllib.request.Request("https://api.github.com"+API_PREFIX+"/actions/artifacts/"+artifact_id+"/zip",
                                     headers={"Authorization":"Bearer "+self.__token,
                                              "Accept":"application/vnd.github+json"})
        try:
            urllib.request.build_opener(urllib.request.ProxyHandler({}), _CaptureRedirect()).open(req, timeout=30)
        except urllib.error.HTTPError as e:
            _require(e.code == 302, "artifact redirect absent")
            location = e.headers.get("Location", "")
            e.close()
        except Exception:
            raise common.ReleaseError("artifact download failed") from None
        else:
            common.fail("unexpected artifact response")
        u = urllib.parse.urlsplit(location)
        _require(u.scheme == "https" and u.port in (None,443) and not u.username and not u.password
                 and not u.fragment and type(u.hostname) is str
                 and (u.hostname.endswith(".blob.core.windows.net")
                      or u.hostname.endswith(".githubusercontent.com")), "artifact host differs")
        raw = _wire(_opener(), location, maximum=MAX_DOWNLOAD)
        _require("sha256:"+common.sha256_bytes(raw) == expected_digest, "artifact archive digest differs")
        return raw


def _extract_exact(raw: bytes, expected: set[str]) -> dict[str, bytes]:
    try:
        with zipfile.ZipFile(io.BytesIO(raw)) as archive:
            info = archive.infolist()
            _require(len(info) == len(expected) and {i.filename for i in info} == expected,
                     "artifact file inventory differs")
            _require(all(not i.is_dir() and i.file_size <= MAX_BODY_BYTES
                         and not (i.external_attr >> 16) & 0o170000 == 0o120000 for i in info),
                     "artifact record type differs")
            return {i.filename: archive.read(i) for i in info}
    except Exception:
        raise common.ReleaseError("artifact contents invalid") from None


def _current_guard(api: GitHubRead, root: Path, *, workflow: str = WORKFLOW_PATH) -> str:
    sha = common.require_sha1(os.environ.get("CONTROL_SHA"), "control SHA")
    _require(os.environ.get("GITHUB_REPOSITORY") == common.REPOSITORY
             and os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch"
             and os.environ.get("GITHUB_REF") == "refs/heads/main"
             and os.environ.get("REF_PROTECTED") == "true"
             and os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted"
             and os.environ.get("GITHUB_RUN_ATTEMPT") == "1"
             and os.environ.get("WORKFLOW_SHA") == sha
             and os.environ.get("GITHUB_WORKFLOW_REF") == common.REPOSITORY+"/"+workflow+"@refs/heads/main",
             "protected workload identity differs")
    proc = subprocess.run(["git","-C",str(root),"rev-parse","HEAD"],capture_output=True,check=False)
    _require(proc.returncode == 0 and proc.stdout.decode().strip() == sha, "checkout differs")
    clean = subprocess.run(["git","-C",str(root),"status","--porcelain=v1","-z","--untracked-files=all"],
                           capture_output=True,check=False)
    _require(clean.returncode == 0 and not clean.stdout,"control checkout is not clean")
    _require(api.get(API_PREFIX+"/git/ref/heads/main")["object"]["sha"] == sha, "main moved")
    run_id = common.require_run_id(os.environ.get("GITHUB_RUN_ID"), "current run")
    run = api.get(API_PREFIX+"/actions/runs/"+run_id)
    _require(run["run_attempt"] == 1 and run["head_sha"] == sha and run["head_branch"] == "main"
             and run["event"] == "workflow_dispatch" and run["path"] == workflow,
             "current run differs")
    return sha


def _write_public(directory: Path, stem: str, value: Any) -> None:
    directory.mkdir(mode=0o700, parents=False, exist_ok=False)
    raw = common.canonical_file_bytes(value)
    _require(len(raw) <= MAX_BODY_BYTES, "public record exceeds bound")
    for name, data in ((stem+".json",raw),(stem+".sha256",(common.sha256_bytes(raw)+"\n").encode())):
        with (directory/name).open("xb") as f:
            f.write(data)


def _parse_action_outputs(raw: str) -> dict[str, str]:
    """Exact Actions file-command parser; duplicates/replayed files cannot pass."""
    lines = raw.splitlines()
    out: dict[str,str] = {}
    i = 0
    while i < len(lines):
        line = lines[i]
        i += 1
        if "<<" in line:
            key, delimiter = line.split("<<",1)
            _require(bool(delimiter) and i+1 < len(lines) and lines[i+1] == delimiter,
                     "uploader output framing differs")
            value = lines[i]
            i += 2
        else:
            _require("=" in line, "uploader output syntax differs")
            key,value = line.split("=",1)
        _require(key not in out, "duplicate uploader output")
        out[key] = value
    _require(set(out) == {"artifact-id","artifact-digest","artifact-url"}, "uploader output keys differ")
    common.require_run_id(out["artifact-id"], "uploaded artifact")
    common.require_sha256(out["artifact-digest"], "uploaded digest")
    return out


class ClaimGate:
    """Durably burn possible-attempt bounds, then issue an ephemeral one-use permit.

    Only successful fresh create/finalize in this living process permits a send.
    The pinned artifact client may retry claim RPCs. It never overwrites/deletes.
    Provider/product requests themselves are not retried. Artifact administration
    remains a trusted boundary: its runtime bearer also has deletion capability.
    """
    def __init__(self, intent: Any, api: GitHubRead, bundle: Path, node: str,
                 before_effect: Any, *, claim_test: bool = False):
        self.intent = validate_origin_intent(intent, now=dt.datetime.now(dt.timezone.utc))
        self.api, self.bundle, self.node, self.before_effect = api,bundle,node,before_effect
        self.claim_test = claim_test
        self.used: set[str] = set()
        self.position = 0
        self.records: list[dict[str,Any]] = []
        raw = bundle.read_bytes()
        _require(len(raw) == 5051718 and common.sha256_bytes(raw) == UPLOAD_BUNDLE_SHA256,
                 "uploader executable differs")
        _require(hashlib.sha1(b"blob "+str(len(raw)).encode()+b"\x00"+raw).hexdigest() == UPLOAD_BUNDLE_BLOB,
                 "uploader Git blob differs")
        _require(os.path.isabs(node) and Path(node).is_file(), "Node runtime absent")
        _require(os.environ.get("GITHUB_RUN_ID") == intent["origin_run_id"]
                 and os.environ.get("GITHUB_RUN_ATTEMPT") == "1",
                 "claim is not in original execution")
        _require(os.environ.get("GITHUB_JOB") == ("claim-test" if claim_test else "execute"),
                 "claim job key differs")
        jobs = api.pages(API_PREFIX+"/actions/runs/"+intent["origin_run_id"]+"/attempts/1/jobs","jobs")["jobs"]
        name = WORKFLOW_JOB_NAMES[1] if claim_test else EXECUTOR_JOB
        matching = [j for j in jobs if j["name"] == name and j["status"] == "in_progress"]
        _require(len(matching) == 1 and matching[0]["run_attempt"] == 1, "executor job is not unique")
        self.job_id = common.require_run_id(str(matching[0]["id"]), "executor job ID")

    def _fresh_claim(self, record: dict[str,Any]) -> dict[str,str]:
        # No caller-supplied output file. The child inherits no product credentials.
        with tempfile.TemporaryDirectory(prefix="fixture-claim-",dir=os.environ["RUNNER_TEMP"]) as tmp:
            root = Path(tmp)
            payload = root/"payload"
            payload.mkdir(mode=0o700)
            (payload/"claim.json").write_bytes(common.canonical_file_bytes(record))
            output = root/"output"
            with output.open("xb") as f:
                os.fchmod(f.fileno(),0o600)
            allowed = {"PATH","HOME","LANG","RUNNER_TEMP","GITHUB_REPOSITORY","GITHUB_RUN_ID",
                       "GITHUB_RUN_ATTEMPT","GITHUB_JOB","GITHUB_WORKSPACE","ACTIONS_RUNTIME_TOKEN",
                       "ACTIONS_RUNTIME_URL","ACTIONS_RESULTS_URL","GITHUB_RETENTION_DAYS"}
            env = {k:v for k,v in os.environ.items() if k in allowed}
            for k in ("ACTIONS_RUNTIME_TOKEN","ACTIONS_RESULTS_URL"):
                _require(bool(env.get(k)), "JavaScript action runtime authority missing")
            env.update({
                "GITHUB_OUTPUT":str(output), "INPUT_NAME":record["artifact_name"],
                "INPUT_PATH":str(payload/"claim.json"), "INPUT_IF-NO-FILES-FOUND":"error",
                "INPUT_OVERWRITE":"false", "INPUT_INCLUDE-HIDDEN-FILES":"false",
                "INPUT_COMPRESSION-LEVEL":"0", "INPUT_RETENTION-DAYS":"90",
            })
            command = ([self.node,"-e",CLAIM_PROBE_WRAPPER,str(self.bundle)] if self.claim_test
                       else [self.node,str(self.bundle)])
            result = subprocess.run(command,env=env,stdin=subprocess.DEVNULL,
                                    stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,
                                    timeout=120,check=False)
            _require(output.is_file() and not output.is_symlink() and output.stat().st_size <= 8192,
                     "claim output invalid")
            if self.claim_test and result.returncode == CLAIM_CONFLICT_EXIT:
                _require(output.stat().st_size == 0,"conflict also produced successful output")
                raise ClaimConflict("exact CreateArtifact duplicate rejected")
            _require(result.returncode == 0, "claim finalization did not return fresh success")
            fields = _parse_action_outputs(output.read_text("utf-8"))
            _require(fields["artifact-url"] == "https://github.com/"+common.REPOSITORY+
                     "/actions/runs/"+self.intent["origin_run_id"]+"/artifacts/"+fields["artifact-id"],
                     "claim output URL differs")
            return fields

    def once(self, stage: str, method: str, path: str, canonical_body: bytes,
             callback: Any, *, nested_budget: int = 0) -> Any:
        expected_stages = ("adapter_probe",) if self.claim_test else STAGES
        _require(stage not in self.used and self.position < len(expected_stages)
                 and stage == expected_stages[self.position], "claim slot is not eligible")
        _require(method in ("POST","PUT") and nested_budget == int(stage == "create_account"),
                 "claim budget differs")
        _require(type(canonical_body) is bytes and len(canonical_body) <= MAX_BODY_BYTES,
                 "request policy differs")
        self.before_effect()
        # Mark used before starting upload; even a local exception is fail-closed.
        self.used.add(stage)
        name = "crm-canary-fixture-burn-"+self.intent["origin_run_id"]+"-1-"+stage
        record = {"schema_version":1,"kind":"crm-canary-fixture-burn",
                  "control_sha":self.intent["control_sha"],"origin_run_id":self.intent["origin_run_id"],
                  "origin_run_attempt":1,"executor_job_id":self.job_id,
                  "operation_sha256":self.intent["request"]["operation_sha256"],
                  "intent_sha256":common.sha256_bytes(common.canonical_file_bytes(self.intent)),
                  "stage":stage,"artifact_name":name,"method":method,
                  "route_sha256":common.sha256_bytes(path.encode()),
                  "request_sha256":common.sha256_bytes(canonical_body),
                  "wrapper_upper_bound":1,"nested_upper_bound":nested_budget}
        fields = self._fresh_claim(record)
        # A retrieved artifact is evidence FOR the fresh return, never a permit.
        meta = self.api.get(API_PREFIX+"/actions/artifacts/"+fields["artifact-id"])
        _artifact_record(meta,name,self.intent["origin_run_id"],self.intent["control_sha"],
                         "sha256:"+fields["artifact-digest"])
        files = _extract_exact(self.api.artifact(fields["artifact-id"],meta["digest"]),{"claim.json"})
        _require(files["claim.json"] == common.canonical_file_bytes(record), "burn record differs")
        self.before_effect()
        self.position += 1  # consume the in-memory permit BEFORE invoking callback
        self.records.append({**record,"artifact_id":fields["artifact-id"],"artifact_digest":meta["digest"]})
        try:
            return callback()
        except Exception:
            raise common.AmbiguousMutation("issued fixture effect requires quarantine") from None


def _artifact_record(value: Any, name: str, run: str, sha: str, digest: str) -> None:
    _require(type(value) is dict and value.get("name") == name and value.get("digest") == digest
             and value.get("expired") is False and type(value.get("size_in_bytes")) is int
             and 0 < value["size_in_bytes"] <= MAX_DOWNLOAD, "artifact metadata differs")
    binding = value.get("workflow_run",{})
    _require(str(binding.get("id")) == run and binding.get("head_sha") == sha
             and binding.get("head_branch") == "main", "artifact origin differs")
    expires = common.require_timestamp(value.get("expires_at"), "artifact expiry")
    _require(expires > dt.datetime.now(dt.timezone.utc), "artifact expired")


def _pinned_gh() -> Path:
    directory = Path(os.environ["RUNNER_TEMP"])/"fixture-gh"
    binary = directory/"gh"
    if binary.exists():
        common.fail("unexpected existing evidence executable")
    directory.mkdir(mode=0o700)
    url = "https://github.com/cli/cli/releases/download/v2.98.0/gh_2.98.0_linux_amd64.tar.gz"
    # Public checksum-pinned tool download has no credential and may follow HTTPS.
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with op.open(url,timeout=60) as r:
        raw = r.read(MAX_DOWNLOAD+1)
    _require(common.sha256_bytes(raw) == "3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de",
             "evidence executable checksum differs")
    with tarfile.open(fileobj=io.BytesIO(raw),mode="r:gz") as archive:
        member = archive.getmember("gh_2.98.0_linux_amd64/bin/gh")
        _require(member.isfile() and 0 < member.size <= MAX_DOWNLOAD, "evidence executable invalid")
        f = archive.extractfile(member)
        _require(f is not None, "evidence executable missing")
        binary.write_bytes(f.read())
        binary.chmod(0o700)
    return binary


def _verify_intent_attestations(path: Path, intent: Any, gh: Path, *,
                                policy_type: str = INTENT_PREDICATE) -> tuple[Any,Any]:
    env = {k:v for k,v in os.environ.items() if k in {"PATH","HOME","GH_TOKEN","RUNNER_TEMP","LANG"}}
    flags = ["--repo",common.REPOSITORY,"--signer-workflow",common.REPOSITORY+"/"+WORKFLOW_PATH,
             "--signer-digest",intent["control_sha"],"--source-digest",intent["control_sha"],
             "--source-ref","refs/heads/main","--deny-self-hosted-runners","--format","json"]
    results = []
    for predicate in ("https://slsa.dev/provenance/v1",policy_type):
        p = subprocess.run([str(gh),"attestation","verify",str(path),*flags,"--predicate-type",predicate],
                           env=env,capture_output=True,timeout=120,check=False)
        _require(p.returncode == 0 and len(p.stdout) <= MAX_DOWNLOAD, "intent signature verification failed")
        result = common.loads_strict(p.stdout)
        _require(type(result) is list and len(result) > 0, "intent verification empty")
        if predicate == policy_type:
            _require(any(x.get("verificationResult",{}).get("statement",{}).get("predicate") == intent for x in result),
                     "signed intent policy differs")
        results.append(result)
    return tuple(results)


def acquire_origin(api: GitHubRead, descriptor: Any, root: Path, *, gh: Path,
                   current: bool = False) -> tuple[dict[str,Any],dict[str,Any],Any,Any]:
    d = _schema(descriptor,ORIGIN_DESCRIPTOR_KEYS,"origin descriptor")
    run_id = common.require_run_id(d["run_id"],"original run")
    artifact_id = common.require_run_id(d["artifact_id"],"intent artifact")
    digest = common.require_digest(d["artifact_digest"],"intent artifact digest")
    common.require_sha256(d["intent_sha256"],"intent content digest")
    run = api.get(API_PREFIX+"/actions/runs/"+run_id)
    attempt = api.get(API_PREFIX+"/actions/runs/"+run_id+"/attempts/1")
    for r in (run,attempt):
        _require(r.get("run_attempt") == 1 and r.get("head_branch") == "main"
                 and r.get("event") == "workflow_dispatch" and r.get("path") == WORKFLOW_PATH
                 and r.get("head_sha") == os.environ["CONTROL_SHA"], "original run differs")
        if not current:
            _require(r.get("status") == "completed", "origin still active")
    meta = api.get(API_PREFIX+"/actions/artifacts/"+artifact_id)
    _artifact_record(meta,"crm-canary-fixture-intent-"+run_id+"-1",run_id,
                     os.environ["CONTROL_SHA"],digest)
    files = _extract_exact(api.artifact(artifact_id,digest),{"intent.json","intent.sha256"})
    _require(common.sha256_bytes(files["intent.json"]) == d["intent_sha256"]
             and files["intent.sha256"] == (d["intent_sha256"]+"\n").encode(), "intent sidecar differs")
    intent = validate_origin_intent(common.loads_strict(files["intent.json"]))
    _require(intent["origin_run_id"] == run_id and intent["control_sha"] == os.environ["CONTROL_SHA"]
             and files["intent.json"] == common.canonical_file_bytes(intent), "origin content differs")
    with tempfile.TemporaryDirectory(prefix="fixture-intent-",dir=os.environ["RUNNER_TEMP"]) as tmp:
        p = Path(tmp)/"intent.json"
        p.write_bytes(files["intent.json"])
        provenance,policy = _verify_intent_attestations(p,intent,gh)
    evidence = {"run":run,"attempt":attempt,
                "jobs":api.pages(API_PREFIX+"/actions/runs/"+run_id+"/attempts/1/jobs","jobs"),
                "artifacts":api.pages(API_PREFIX+"/actions/runs/"+run_id+"/artifacts","artifacts")}
    return intent,evidence,provenance,policy

def expected_origin_slots() -> list[dict[str,Any]]:
    return stage_slots()


def _claim_test_authority(api: GitHubRead, run_id: str, sha: str) -> None:
    common.require_run_id(run_id,"claim test run")
    run = api.get(API_PREFIX+"/actions/runs/"+run_id)
    _require(run.get("status") == "completed" and run.get("conclusion") == "success"
             and run.get("run_attempt") == 1 and run.get("head_sha") == sha
             and run.get("head_branch") == "main" and run.get("event") == "workflow_dispatch"
             and run.get("path") == WORKFLOW_PATH, "claim-only hosted proof absent")
    jobs = api.pages(API_PREFIX+"/actions/runs/"+run_id+"/attempts/1/jobs","jobs")["jobs"]
    _require(sorted(j["name"] for j in jobs) == sorted(WORKFLOW_JOB_NAMES), "claim-test jobs differ")
    results = {j["name"]:j["conclusion"] for j in jobs}
    _require(results == {WORKFLOW_JOB_NAMES[0]:"success",WORKFLOW_JOB_NAMES[1]:"success",
                         EXECUTOR_JOB:"skipped",WORKFLOW_JOB_NAMES[3]:"skipped",WORKFLOW_JOB_NAMES[4]:"success"},
             "claim test was not harmless")
    artifacts = api.pages(API_PREFIX+"/actions/runs/"+run_id+"/artifacts","artifacts")["artifacts"]
    names = {"crm-canary-fixture-intent-"+run_id+"-1",
             "crm-canary-fixture-burn-"+run_id+"-1-adapter_probe"}
    _require(len(artifacts) == 2 and {a["name"] for a in artifacts} == names,
             "claim-test evidence inventory differs")
    for a in artifacts:
        _artifact_record(a,a["name"],run_id,sha,a["digest"])


def validate_execution_authority(authority: Any, intent: Any, protected: Any,
                                 descriptor: Any, api: GitHubRead) -> dict[str,Any]:
    keys = {"schema_version","origin","execution_job_key","request_sha256","registration_sha256",
            "claim_test_run_id","provider_target","provider_prestate_sha256","expires_at"}
    a = _schema(authority,keys,"protected execution authority")
    _require(type(a["schema_version"]) is int and a["schema_version"] == 1
             and a["origin"] == descriptor and a["execution_job_key"] == "execute",
             "protected origin differs")
    _require(a["origin"]["run_id"] == os.environ.get("GITHUB_RUN_ID")
             and a["request_sha256"] == common.sha256_value(intent["request"])
             and a["registration_sha256"] == common.sha256_value(protected["registration"]),
             "protected operation or login registration differs")
    common.require_sha256(a["provider_prestate_sha256"],"provider prestate hash")
    expires = common.require_timestamp(a["expires_at"],"execution approval expiry")
    now = dt.datetime.now(dt.timezone.utc)
    _require(now < expires <= common.require_timestamp(intent["expires_at"],"intent expiry"),
             "execution approval expired")
    _claim_test_authority(api,a["claim_test_run_id"],intent["control_sha"])
    environment = api.get(API_PREFIX+"/environments/rereply-production-crm-fixture")
    rules = environment.get("protection_rules",[])
    _require(len([r for r in rules if r.get("type") == "required_reviewers" and r.get("reviewers")])
             == 1, "required environment approval missing")
    policy = environment.get("deployment_branch_policy",{})
    _require(policy == {"protected_branches":False,"custom_branch_policies":True},
             "environment branch policy differs")
    branches = api.get(API_PREFIX+"/environments/rereply-production-crm-fixture/deployment-branch-policies")
    _require(branches.get("total_count") == 1 and len(branches.get("branch_policies",[])) == 1
             and branches["branch_policies"][0].get("name") == "main"
             and branches["branch_policies"][0].get("type") == "branch",
             "fixture environment is not main-only")
    return copy.deepcopy(a)


class ProviderFixture:
    """One full-spec allowlist append; no app creation, DB API, or secret writer."""
    def __init__(self, root: Path, authority: Any, read_token: str, update_token: str):
        try:
            from . import verify_production_plan as planner
        except ImportError:
            import verify_production_plan as planner
        self.planner = planner
        self.contract = common.load_json(root/"release/deployment/production-app-contract.json","contract",canonical=False)
        self.target = planner.normalize_target_descriptor(
            common.canonical_payload_bytes(authority["provider_target"]).decode(),self.contract)
        self.expected,_ = planner.predecessor_provider_expectation(self.contract,{},None)
        self.__read_token,self.__update_token = _secret(read_token),_secret(update_token)
        self.opener = _opener()
        self.prestate_hash = authority["provider_prestate_sha256"]
        self.initial_spec: dict[str,Any] | None = None
        self.last_state: Any = None

    def _get(self,path: str) -> Any:
        prefix = "/v2/apps/"+self.target["app_id"]
        _require(path == prefix or re.fullmatch(re.escape(prefix)+r"/deployments/(?:[0-9a-f-]{36})",path)
                 is not None or re.fullmatch(re.escape(prefix)+r"/deployments\?page=[1-9][0-9]*&per_page=200",path)
                 is not None,
                 "provider GET path differs")
        return common.loads_strict(_wire(self.opener,common.API_ORIGIN+path,
                    headers={"Authorization":"Bearer "+self.__read_token},maximum=common.MAX_JSON_BYTES))

    def current(self) -> tuple[Any,Any,Any]:
        path = "/v2/apps/"+self.target["app_id"]
        app = self._get(path)
        active = common.require_uuid(app.get("app",{}).get("active_deployment",{}).get("id"),"active deployment")
        deployment = self._get(path+"/deployments/"+active)
        return app,deployment,path

    def terminal_inventory(self, active: str) -> list[dict[str,str]]:
        """Complete metadata projection only; never return historical specs."""
        path = "/v2/apps/"+self.target["app_id"]+"/deployments"
        rows,seen,total = [],set(),None
        for page in range(1,MAX_PAGES+1):
            data = self._get(path+f"?page={page}&per_page=200")
            observed = common.exact_int(data.get("meta",{}).get("total"),"deployment total",1,MAX_PAGES*200)
            _require(total is None or total == observed,"deployment inventory moved")
            total = observed
            chunk = data.get("deployments")
            _require(type(chunk) is list and 0 < len(chunk) <= 200,"deployment page incomplete")
            for record in chunk:
                identity = common.require_uuid(record.get("id"),"deployment identity")
                phase = record.get("phase")
                _require(identity not in seen and phase in ("ACTIVE","SUPERSEDED","ERROR","CANCELED"),
                         "duplicate, unknown, or nonterminal deployment")
                seen.add(identity)
                rows.append({"id":identity,"phase":phase})
            _require(len(rows) <= total,"excess deployment records")
            next_url = data.get("links",{}).get("pages",{}).get("next")
            if not next_url:
                _require(len(rows) == total and {r["id"] for r in rows if r["phase"] == "ACTIVE"} == {active},
                         "complete active deployment inventory differs")
                return sorted(rows,key=lambda r:r["id"])
            parsed = urllib.parse.urlsplit(next_url)
            _require(parsed.scheme == "https" and parsed.netloc == "api.digitalocean.com"
                     and parsed.path == path and not parsed.fragment
                     and urllib.parse.parse_qs(parsed.query,strict_parsing=True)
                         == {"page":[str(page+1)],"per_page":["200"]},
                     "deployment pagination target differs")
        common.fail("deployment page bound exceeded")

    def before_effect(self) -> None:
        app,dep,_ = self.current()
        state,spec = self.planner.provider_state(app,dep,self.contract,self.target,self.expected,None)
        _require(common.sha256_value(state) == self.prestate_hash, "provider prestate changed")
        if self.initial_spec is None:
            self.initial_spec = spec
        _require(spec == self.initial_spec, "provider full spec moved")
        self.allowlist_control(spec)  # prove append is possible before any fixture mutation
        self.terminal_inventory(dep["deployment"]["id"])
        self.last_state = state

    @staticmethod
    def allowlist_control(spec: Any) -> dict[str,Any]:
        _require(type(spec) is dict,"allowlist spec differs")
        matches: list[dict[str,Any]] = []
        enabled: list[dict[str,Any]] = []
        # Component overrides and inherited app-level duplicates are never ignored.
        for collection in (None,"services","workers","jobs"):
            blocks = [spec] if collection is None else spec.get(collection,[])
            for block in blocks:
                for env in block.get("envs",[]):
                    if env.get("key") == ALLOWLIST_KEY:
                        _require(collection == "services" and block.get("name") == "omnitech-web",
                                 "allowlist shadow or placement differs")
                        matches.append(env)
                    if env.get("key") == ENABLE_KEY:
                        _require(collection == "services" and block.get("name") == "omnitech-web",
                                 "enablement shadow differs")
                        enabled.append(env)
        _require(len(matches) == len(enabled) == 1,"allowlist controls missing or duplicated")
        _require(enabled[0].get("value") == "true" and enabled[0].get("type") in (None,"GENERAL"),
                 "legacy reply is not explicitly enabled")
        entry = matches[0]
        _require(entry.get("type") in (None,"GENERAL") and type(entry.get("value")) is str,
                 "allowlist is not an observable general value")
        previous = entry["value"].split(",")
        _require(previous and len(previous) == len(set(previous)), "allowlist prefix differs")
        for value in previous:
            common.require_uuid(value,"allowlist member")
        return entry

    @staticmethod
    def appended_spec(spec: Any, klinik: str, other: str) -> dict[str,Any]:
        common.require_uuid(klinik,"Klinik")
        common.require_uuid(other,"non-Klinik")
        _require(klinik != other,"allowlist target differs")
        result = copy.deepcopy(spec)
        entry = ProviderFixture.allowlist_control(result)
        previous = entry["value"].split(",")
        _require(klinik not in previous and other not in previous,"fixture already admitted")
        entry["value"] += ","+klinik
        return result

    def append(self,gate: ClaimGate,klinik: str,other: str) -> dict[str,Any]:
        self.before_effect()
        before = copy.deepcopy(self.initial_spec)
        after = self.appended_spec(before,klinik,other)
        old_deployment = self.last_state["active_deployment_identity_sha256"]
        path = "/v2/apps/"+self.target["app_id"]
        policy = common.canonical_payload_bytes({
            "before_spec_sha256":common.sha256_value(before),"after_spec_sha256":common.sha256_value(after),
            "klinik_sha256":common.sha256_value(klinik),"non_klinik_sha256":common.sha256_value(other)})
        def send() -> None:
            # Response may carry encrypted provider values; it is never logged or published.
            _wire(self.opener,common.API_ORIGIN+path,method="PUT",
                  headers={"Authorization":"Bearer "+self.__update_token,"Content-Type":"application/json"},
                  body=common.canonical_payload_bytes({"spec":after}),maximum=common.MAX_JSON_BYTES)
        gate.once("append_klinik_allowlist","PUT",path,policy,send)
        settled = None
        deadline = time.monotonic()+1200
        while time.monotonic() < deadline:
            app,dep,_ = self.current()
            a,d = app["app"],dep["deployment"]
            _require(a["spec"] == after,"allowlist full-spec readback differs")
            if a.get("in_progress_deployment") is None and a.get("pending_deployment") is None:
                _require(a.get("pinned_deployment") is None and d.get("phase") == "ACTIVE"
                         and d.get("spec") == after,"replacement deployment failed")
                _require(common.sha256_bytes(d["id"].encode()) != old_deployment,
                         "replacement deployment did not materialize")
                self.planner.validate_legacy_component_sources(after,self.contract)
                self.planner.validate_legacy_deployment_sources(d,self.contract)
                inventory = self.terminal_inventory(d["id"])
                snapshot = {"spec_sha256":common.sha256_value(after),
                            "deployment_sha256":common.sha256_bytes(d["id"].encode()),
                            "app_updated_at_sha256":common.sha256_bytes(a["updated_at"].encode())}
                stable = (snapshot,inventory)
                if settled == stable:
                    self._health()
                    # Health polling must not hide a subsequent provider move.
                    final_app,final_dep,_ = self.current()
                    _require(final_app == app and final_dep == dep and
                             self.terminal_inventory(d["id"]) == inventory,"provider moved during readiness")
                    return {"state":"allowlist_deployment_verified","provider":snapshot,
                            "release_evidence_invalidated":True,"contract_rebaseline_required":True}
                settled = stable
            time.sleep(3)
        common.fail("replacement deployment did not settle")

    def _health(self) -> None:
        paths = (("/ready",200),("/meta-relay/readyz",204),("/gmail-relay/readyz",204))
        for _ in range(2):
            for path,status in paths:
                try:
                    with _opener().open(PRODUCT_ORIGIN+path,timeout=20) as response:
                        _require(response.status == status,"replacement readiness failed")
                except Exception:
                    raise common.ReleaseError("replacement readiness failed") from None
            time.sleep(3)


def _intent_from_current(api: GitHubRead) -> tuple[dict[str,str],dict[str,bytes]]:
    run = common.require_run_id(os.environ["GITHUB_RUN_ID"],"run")
    identity = common.require_run_id(os.environ["INTENT_ARTIFACT_ID"],"intent artifact")
    digest = "sha256:"+common.require_sha256(os.environ["INTENT_ARTIFACT_DIGEST"],"intent digest")
    meta = api.get(API_PREFIX+"/actions/artifacts/"+identity)
    _artifact_record(meta,"crm-canary-fixture-intent-"+run+"-1",run,os.environ["CONTROL_SHA"],digest)
    files = _extract_exact(api.artifact(identity,digest),{"intent.json","intent.sha256"})
    value = {"run_id":run,"artifact_id":identity,"artifact_digest":digest,
             "intent_sha256":common.sha256_bytes(files["intent.json"])}
    return value,files


def _require_intent_code(intent: Any,root: Path) -> None:
    _require(intent["controller_sha256"] == common.sha256_bytes(Path(__file__).read_bytes())
             and intent["workflow_sha256"] == common.sha256_bytes((root/WORKFLOW_PATH).read_bytes()),
             "signed controller or workflow content differs")


def _prepare_intent(api: GitHubRead,root: Path,output: Path) -> None:
    sha = _current_guard(api,root)
    req = validate_request(common.loads_strict(os.environ["REQUEST_JSON"]))
    _require(req["control_sha"] == sha,"request control differs")
    mode = os.environ.get("FIXTURE_MODE")
    _require(mode in ("claim-test","execute","reconcile"),"mode differs")
    _require(bool(os.environ.get("ORIGIN_EVIDENCE_JSON")) == (mode == "reconcile"),
             "origin descriptor mode differs")
    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    intent = {"schema_version":1,"kind":"crm-canary-fixture-intent","control_sha":sha,
              "origin_run_id":os.environ["GITHUB_RUN_ID"],"origin_run_attempt":1,
              "executor_job":EXECUTOR_JOB,"workflow_path":WORKFLOW_PATH,
              "workflow_sha256":common.sha256_bytes((root/WORKFLOW_PATH).read_bytes()),
              "controller_sha256":common.sha256_bytes(Path(__file__).read_bytes()),
              "upload_bundle_sha256":UPLOAD_BUNDLE_SHA256,"request":req,"slots":stage_slots(),
              "issued_at":common.format_timestamp(now),
              "expires_at":common.format_timestamp(now+dt.timedelta(hours=24))}
    validate_origin_intent(intent)
    _write_public(output,"intent",intent)


def _execute(api: GitHubRead,root: Path,bundle: Path,output: Path,gh: Path) -> None:
    _current_guard(api,root)
    descriptor,_ = _intent_from_current(api)
    intent,_,_,_ = acquire_origin(api,descriptor,root,gh=gh,current=True)
    _require_intent_code(intent,root)
    validate_origin_intent(intent,now=dt.datetime.now(dt.timezone.utc))
    _require(intent["request"] == validate_request(common.loads_strict(os.environ["REQUEST_JSON"])),
             "dispatch request differs from intent")
    protected = validate_protected_input(common.loads_strict(os.environ.pop("CRM_CANARY_FIXTURE_INPUT_JSON")),
                                         intent["request"])
    authority = validate_execution_authority(
        common.loads_strict(os.environ.pop("CRM_CANARY_FIXTURE_AUTHORITY_JSON")),
        intent,protected,descriptor,api)
    provider = ProviderFixture(root,authority,os.environ.pop("DO_PRODUCTION_FIXTURE_READ_TOKEN"),
                               os.environ.pop("DO_PRODUCTION_FIXTURE_UPDATE_TOKEN"))
    def guard() -> None:
        _current_guard(api,root)
        provider.before_effect()
        # Sample after network guards, including the post-finalization guard.
        _require_effect_window(intent,authority,dt.datetime.now(dt.timezone.utc))
    gate = ClaimGate(intent,api,bundle,os.environ["CLAIM_NODE"],guard)
    guard()
    transport = ProductHTTP(protected["credentials"]["meta_access_token"])
    product = ProductProvisioner(intent["request"],protected,transport,gate)
    driver,result = product.provision()
    terminal = provider.append(gate,driver["klinik"]["organization_id"],driver["non_klinik"]["organization_id"])
    _require(gate.position == len(STAGES),"incomplete issued-effect inventory")
    result["state"] = terminal["state"]
    result.update({key:value for key,value in terminal.items() if key != "state"})
    result["origin"] = descriptor
    result["burns"] = gate.records
    result["registration_sha256"] = authority["registration_sha256"]
    validate_terminal_result(result)
    _current_guard(api,root)
    _write_public(output,"result",result)


def _require_effect_window(intent: Any, authority: Any, now: dt.datetime) -> None:
    validate_origin_intent(intent,now=now)
    _require(now < common.require_timestamp(authority["expires_at"], "execution approval expiry"),
             "protected execution approval expired")


def _hosted_claim_test(api: GitHubRead,root: Path,bundle: Path,gh: Path) -> None:
    _current_guard(api,root)
    descriptor,_ = _intent_from_current(api)
    intent,_,_,_ = acquire_origin(api,descriptor,root,gh=gh,current=True)
    _require_intent_code(intent,root)
    gate = ClaimGate(intent,api,bundle,os.environ["CLAIM_NODE"],lambda:_current_guard(api,root),claim_test=True)
    called = []
    gate.once("adapter_probe","POST","/claim-test-no-network",b'{"synthetic":true}',lambda:called.append(True))
    _require(called == [True],"claim-only permit differs")
    # A separate new controller process-state contender still hits the same backend name.
    contender = ClaimGate(intent,api,bundle,os.environ["CLAIM_NODE"],lambda:_current_guard(api,root),claim_test=True)
    try:
        contender.once("adapter_probe","POST","/claim-test-no-network",b'{"synthetic":true}',lambda:called.append(True))
    except ClaimConflict:
        pass
    else:
        common.fail("artifact backend accepted duplicate claim")
    _require(called == [True],"duplicate claim invoked callback")
    original = gate.records[0]
    meta = api.get(API_PREFIX+"/actions/artifacts/"+original["artifact_id"])
    _artifact_record(meta,original["artifact_name"],intent["origin_run_id"],intent["control_sha"],
                     original["artifact_digest"])
    files = _extract_exact(api.artifact(original["artifact_id"],original["artifact_digest"]),{"claim.json"})
    payload = {k:v for k,v in original.items() if k not in ("artifact_id","artifact_digest")}
    _require(files["claim.json"] == common.canonical_file_bytes(payload),"original claim changed during probe")



RESULT_KEYS = {"schema_version","kind","control_sha","operation_sha256","descriptor_sha256",
               "fixture_descriptor_sha256","registration_sha256","stages","state","provider",
               "release_evidence_invalidated","contract_rebaseline_required","origin","burns"}


def validate_terminal_result(result: Any) -> dict[str,Any]:
    r = _schema(result,RESULT_KEYS,"fixture terminal result")
    _require(type(r["schema_version"]) is int and r["schema_version"] == 1
             and r["kind"] == "crm-canary-fixture-provisioning"
             and r["state"] == "allowlist_deployment_verified"
             and r["release_evidence_invalidated"] is True and r["contract_rebaseline_required"] is True,
             "fixture terminal status differs")
    common.require_sha1(r["control_sha"],"fixture control")
    for key in ("operation_sha256","descriptor_sha256","fixture_descriptor_sha256","registration_sha256"):
        common.require_sha256(r[key],"fixture binding")
    origin = _schema(r["origin"],ORIGIN_DESCRIPTOR_KEYS,"fixture origin")
    for k in ("run_id","artifact_id"):
        common.require_run_id(origin[k],"fixture origin identity")
    common.require_sha256(origin["intent_sha256"],"fixture origin hash")
    common.require_digest(origin["artifact_digest"],"fixture origin digest")
    provider = _schema(r["provider"],{"spec_sha256","deployment_sha256","app_updated_at_sha256"},"fixture provider proof")
    for value in provider.values():
        common.require_sha256(value,"provider binding")
    _require(type(r["stages"]) is list and len(r["stages"]) == len(PUBLIC_STAGES)
             and type(r["burns"]) is list and len(r["burns"]) == len(STAGES),"fixture effect inventory differs")
    for stage,expected in zip(r["stages"],stage_slots()):
        _schema(stage,{"stage","request_sha256","response_sha256","wrapper_upper_bound","nested_upper_bound"},"fixture stage")
        _require(all(stage[k] == expected[k] for k in expected),"fixture stage vector differs")
        for k in ("request_sha256","response_sha256"):
            common.require_sha256(stage[k],"stage digest")
    ids=set()
    keys={"schema_version","kind","control_sha","origin_run_id","origin_run_attempt","executor_job_id",
          "operation_sha256","intent_sha256","stage","artifact_name","method","route_sha256",
          "request_sha256","wrapper_upper_bound","nested_upper_bound","artifact_id","artifact_digest"}
    for burn,expected in zip(r["burns"],stage_slots()):
        _schema(burn,keys,"fixture burn")
        _require(all(burn[k] == expected[k] for k in expected)
                 and burn["schema_version"] == 1 and burn["kind"] == "crm-canary-fixture-burn"
                 and burn["control_sha"] == r["control_sha"] and burn["origin_run_id"] == origin["run_id"]
                 and burn["origin_run_attempt"] == 1 and burn["operation_sha256"] == r["operation_sha256"]
                 and burn["intent_sha256"] == origin["intent_sha256"]
                 and burn["artifact_name"] == "crm-canary-fixture-burn-"+origin["run_id"]+"-1-"+expected["stage"],
                 "fixture burn origin differs")
        for k in ("artifact_id","executor_job_id"):
            common.require_run_id(burn[k],"burn ID")
        _require(burn["artifact_id"] not in ids,"duplicate burn artifact")
        ids.add(burn["artifact_id"])
        common.require_digest(burn["artifact_digest"],"burn digest")
        for k in ("route_sha256","request_sha256"):
            common.require_sha256(burn[k],"burn policy")
        _require(burn["method"] == ("PUT" if expected["stage"] in
                 ("license_klinik","license_non_klinik","configure_meta","append_klinik_allowlist") else "POST"),
                 "burn method differs")
    for stage,burn in zip(r["stages"],r["burns"]):
        _require(stage["request_sha256"] == burn["request_sha256"],"stage burn request differs")
    _require(len({b["executor_job_id"] for b in r["burns"]}) == 1,"burn executor differs")
    return copy.deepcopy(r)


def verify_fixture_result(api: GitHubRead,root: Path,gh: Path) -> dict[str,Any]:
    d = _schema(common.loads_strict(os.environ["FIXTURE_EVIDENCE_JSON"]),
                {"control_sha","run_id","artifact_id","artifact_digest","result_sha256"},"fixture result descriptor")
    common.require_sha1(d["control_sha"],"fixture source")
    current = os.environ["CONTROL_SHA"]
    compare = api.get(API_PREFIX+"/compare/"+d["control_sha"]+"..."+current)
    _require(compare.get("status") in ("ahead","identical")
             and compare.get("merge_base_commit",{}).get("sha") == d["control_sha"],"fixture control is not protected ancestry")
    # Only the separately reviewed contract rebaseline and release evidence may follow.
    for path in (WORKFLOW_PATH,"release/deployment/provision_production_crm_canary_fixture.py"):
        old=api.get(API_PREFIX+"/contents/"+path+"?ref="+d["control_sha"])
        new=api.get(API_PREFIX+"/contents/"+path+"?ref="+current)
        _require(old.get("sha") == new.get("sha"),"fixture producer changed after receipt")
    run_id=common.require_run_id(d["run_id"],"fixture run")
    run=api.get(API_PREFIX+"/actions/runs/"+run_id)
    _require(run.get("head_sha") == d["control_sha"] and run.get("head_branch") == "main"
             and run.get("path") == WORKFLOW_PATH and run.get("event") == "workflow_dispatch"
             and run.get("run_attempt") == 1 and run.get("status") == "completed"
             and run.get("conclusion") == "success","fixture run not successful")
    jobs=api.pages(API_PREFIX+"/actions/runs/"+run_id+"/attempts/1/jobs","jobs")["jobs"]
    _require(len(jobs) == 5 and {j["name"]:j["conclusion"] for j in jobs} == {
        WORKFLOW_JOB_NAMES[0]:"success",WORKFLOW_JOB_NAMES[1]:"skipped",EXECUTOR_JOB:"success",
        WORKFLOW_JOB_NAMES[3]:"skipped",WORKFLOW_JOB_NAMES[4]:"success"},
        "fixture job inventory differs")
    identity=common.require_run_id(d["artifact_id"],"fixture result artifact")
    digest=common.require_digest(d["artifact_digest"],"fixture result digest")
    common.require_sha256(d["result_sha256"],"fixture content digest")
    meta=api.get(API_PREFIX+"/actions/artifacts/"+identity)
    _artifact_record(meta,"crm-canary-fixture-result-"+run_id+"-1",run_id,d["control_sha"],digest)
    files=_extract_exact(api.artifact(identity,digest),{"result.json","result.sha256"})
    _require(common.sha256_bytes(files["result.json"]) == d["result_sha256"]
             and files["result.sha256"] == (d["result_sha256"]+"\n").encode(),"fixture content differs")
    result=validate_terminal_result(common.loads_strict(files["result.json"]))
    _require(result["control_sha"] == d["control_sha"] and result["origin"]["run_id"] == run_id,
             "fixture result authority differs")
    with tempfile.TemporaryDirectory(prefix="fixture-result-",dir=os.environ["RUNNER_TEMP"]) as tmp:
        p=Path(tmp)/"result.json";p.write_bytes(files["result.json"])
        _verify_intent_attestations(p,result,gh,
                    policy_type="https://rereply.app/attestations/crm-canary-fixture-result/v1")
    inventory=api.pages(API_PREFIX+"/actions/runs/"+run_id+"/artifacts","artifacts")
    expected={b["artifact_name"]:(b["artifact_id"],b["artifact_digest"]) for b in result["burns"]}
    expected["crm-canary-fixture-result-"+run_id+"-1"]=(identity,digest)
    expected["crm-canary-fixture-intent-"+run_id+"-1"]=(result["origin"]["artifact_id"],result["origin"]["artifact_digest"])
    _require(inventory["total_count"] == 14 and {a["name"] for a in inventory["artifacts"]} == set(expected),
             "fixture exact artifact inventory differs")
    for a in inventory["artifacts"]:
        i,h=expected[a["name"]]
        _require(str(a["id"]) == i,"fixture artifact ID differs")
        _artifact_record(a,a["name"],run_id,d["control_sha"],h)
    time.sleep(2)
    _require(inventory == api.pages(API_PREFIX+"/actions/runs/"+run_id+"/artifacts","artifacts"),
             "fixture artifact inventory changed")
    driver=_schema(common.loads_strict(os.environ["CRM_CANARY_SYNTHETIC_DRIVER_JSON"]),
                   {"schema_version","url","driver_version_sha256","fixture_descriptor_sha256","hmac_key_base64"},
                   "synthetic driver")
    _require(driver["fixture_descriptor_sha256"] == result["fixture_descriptor_sha256"],
             "runtime fixture does not match signed provision receipt")
    return result


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Exact CRM fixture lifecycle controls")
    parser.add_argument("command",choices=["prepare-intent","claim-test","execute","reconcile","acquire-origin","verify-fixture-result"])
    parser.add_argument("--control-root",type=Path,required=True)
    parser.add_argument("--output-dir",type=Path)
    parser.add_argument("--upload-bundle",type=Path)
    args = parser.parse_args(argv)
    try:
        root = args.control_root.resolve(strict=True)
        api = GitHubRead(os.environ["GH_TOKEN"])
        if args.command == "prepare-intent":
            _require(args.output_dir is not None,"intent output missing")
            _prepare_intent(api,root,args.output_dir)
        else:
            workflow = (CLEANUP_WORKFLOW_PATH if args.command == "acquire-origin" else
                        ".github/workflows/verify-production-crm-canary.yml" if args.command == "verify-fixture-result" else WORKFLOW_PATH)
            _current_guard(api,root,workflow=workflow)
            gh = _pinned_gh()
            if args.command == "verify-fixture-result":
                verify_fixture_result(api,root,gh)
            elif args.command == "execute":
                _require(args.upload_bundle is not None and args.output_dir is not None,"execution paths missing")
                _execute(api,root,args.upload_bundle.resolve(strict=True),args.output_dir,gh)
            elif args.command == "claim-test":
                _require(args.upload_bundle is not None,"claim bundle missing")
                _hosted_claim_test(api,root,args.upload_bundle.resolve(strict=True),gh)
            else:
                descriptor = _schema(common.loads_strict(os.environ["ORIGIN_EVIDENCE_JSON"]),
                                     ORIGIN_DESCRIPTOR_KEYS,"origin descriptor")
                intent,evidence,provenance,policy = acquire_origin(api,descriptor,root,gh=gh)
                _require(args.output_dir is not None,"origin output missing")
                args.output_dir.mkdir(mode=0o700,parents=False,exist_ok=False)
                for name,value in (("intent",intent),("evidence",evidence),
                                   ("provenance",provenance),("policy",policy),("descriptor",descriptor)):
                    (args.output_dir/(name+".json")).write_bytes(common.canonical_file_bytes(value))
                if args.command == "reconcile":
                    # This job only re-acquires evidence. It cannot mint a new permit.
                    from cleanup_production_crm_canary_fixture import build_report
                    report = build_report(intent,evidence,provenance,policy,
                        now=dt.datetime.now(dt.timezone.utc),expected_control_sha=os.environ["CONTROL_SHA"],
                        expected_origin_run_id=descriptor["run_id"],expected_intent_sha256=descriptor["intent_sha256"],
                        expected_origin_artifact_id=descriptor["artifact_id"],expected_origin_artifact_digest=descriptor["artifact_digest"])
                    (args.output_dir/"reconciliation.json").write_bytes(common.canonical_file_bytes(report))
        return 0
    except Exception:
        print("fixture control stopped; no repeat execution is authorized",file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
