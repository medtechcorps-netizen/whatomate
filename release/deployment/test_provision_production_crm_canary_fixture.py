from __future__ import annotations

import copy
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import unittest
import warnings
from unittest import mock
from urllib.parse import parse_qs, urlsplit

try:
    from . import provision_production_crm_canary_fixture as fixture
except ImportError:
    import provision_production_crm_canary_fixture as fixture

common = fixture.common


def uid(n):
    return f"{n:08x}-1111-4111-8111-111111111111"


def inputs():
    def plan(n):
        return {"plan_id": uid(n), "plan_code": "fixture-" + str(n), "plan_name": "Fixture " + str(n),
                "vertical": "healthcare" if n == 20 else "general", "entitlements_sha256": common.sha256_value({}),
                "price_id": uid(n + 2), "price_code": "monthly", "currency": "MYR", "unit_amount_minor": 0,
                "setup_amount_minor": 0, "interval": "month", "interval_count": 1, "tax_behavior": "inclusive"}
    d = {"schema_version": 1, "product_origin": fixture.PRODUCT_ORIGIN,
         "fixture_namespace": "rereply-canary", "source_sha": fixture.EXPECTED_SOURCE_SHA,
         "super_admin_id": uid(1), "super_admin_home_org_id": uid(2), "reseller_id": uid(3),
         "controlled_email_domain": "fixtures.example.test",
         "klinik": {"organization_name": "rereply-canary-klinik", "full_name": "rereply-canary-klinik-agent", "plan": plan(20)},
         "non_klinik": {"organization_name": "rereply-canary-non-klinik", "full_name": "rereply-canary-other-agent", "plan": plan(21)},
         "meta": {"app_id": "100001", "config_id": "100002", "business_account_id": "100003", "phone_number_id": "100004",
                  "display_phone_number": "100005", "account_name": "rereply-canary-account", "api_version": "v21.0"},
         "conversations": {key: {"display_name": "rereply-canary-" + key, "sender_wa_id": str(200000 + n),
                                 "wamid": "wamid.fixture." + key, "timestamp": "1790000000", "body": "rereply-canary-seed-" + key}
                           for key, n in (("a", 1), ("b", 2))}}
    credentials = {"super_admin_login": {"email": "admin@example.test", "password": "SuperAdminPassword123"},
                   "klinik_password": "KlinikPassword123", "non_klinik_password": "OtherPassword123",
                   "meta_access_token": "SyntheticMetaToken123", "meta_app_secret": "SyntheticAppSecret123",
                   "meta_webhook_verify_token": "SyntheticVerifyToken123"}
    with mock.patch.object(fixture.secrets, "token_bytes", side_effect=[b"a" * 32, b"b" * 32]):
        registration = fixture.generate_registration(d["controlled_email_domain"])
    protected = {"descriptor": d, "credentials": credentials, "registration": registration}
    request = {"schema_version": 1, "control_sha": "a" * 40, "operation_sha256": "b" * 64,
               "descriptor_sha256": common.sha256_value(d)}
    return request, protected


class FakeGate:
    def __init__(self):
        self.calls = []

    def once(self, stage, method, path, body, call, nested_budget=0):
        self.calls.append((stage, method, path, body, nested_budget))
        return call()


def terminal_receipt(base, protected):
    """Synthetic final signed-payload shape; no signature/provenance claim."""
    result = copy.deepcopy(base)
    result.update(state="allowlist_deployment_verified",release_evidence_invalidated=True,
        contract_rebaseline_required=True,registration_sha256=common.sha256_value(protected["registration"]),
        provider={"spec_sha256":"a"*64,"deployment_sha256":"b"*64,"app_updated_at_sha256":"c"*64},
        origin={"run_id":"12345","artifact_id":"71","artifact_digest":"sha256:"+"a"*64,"intent_sha256":"d"*64},burns=[])
    for index,slot in enumerate(fixture.stage_slots()):
        result["burns"].append({**slot,"schema_version":1,"kind":"crm-canary-fixture-burn",
            "control_sha":base["control_sha"],"origin_run_id":"12345","origin_run_attempt":1,
            "executor_job_id":"54321","operation_sha256":base["operation_sha256"],"intent_sha256":"d"*64,
            "artifact_name":"crm-canary-fixture-burn-12345-1-"+slot["stage"],
            "method":"PUT" if slot["stage"] in ("license_klinik","license_non_klinik","configure_meta","append_klinik_allowlist") else "POST",
            "route_sha256":"e"*64,"request_sha256":base["stages"][index]["request_sha256"] if index < 11 else "f"*64,
            "artifact_id":str(100+index),"artifact_digest":"sha256:"+"a"*64})
    return fixture.validate_terminal_result(result)


class FakeTransport:
    def __init__(self, protected):
        self.p = protected
        self.d = protected["descriptor"]
        self.orgs = [{"id": uid(2), "name": "Home", "slug": "home", "reseller_id": uid(3)}]
        self.users = {}
        self.accounts = []
        self.rows = []
        self.messages = {}
        self.subscriptions = {}
        self.calls = []
        self.sessions = {}
        self.meta = None
        self.next_org = 100

    def login(self, email, password):
        if email == self.p["credentials"]["super_admin_login"]["email"]:
            return "admin"
        matches = [v for v in self.users.values() if v["email"] == email]
        if len(matches) != 1:
            raise ValueError("login failed")
        return matches[0]["id"]

    def request(self, method, path, body=None, *, session=None, organization_id=None, headers=None, graph=False):
        self.calls.append((method, path, copy.deepcopy(body), session, organization_id, headers, graph))
        route, query = urlsplit(path).path, parse_qs(urlsplit(path).query)
        if graph:
            if route.endswith("/phone_numbers"):
                return {"data": [{"id": self.d["meta"]["phone_number_id"],
                                  "display_phone_number": "+" + self.d["meta"]["display_phone_number"]}]}
            return {"data": [{"whatsapp_business_api_data": {"id": "100001"}}]}
        def result(data):
            return {"status": "success", "data": copy.deepcopy(data)}
        def page(key, rows):
            number = int(query.get("page", ["1"])[0])
            return result({key: rows[(number - 1) * 100:number * 100], "total": len(rows), "page": number, "limit": 100})
        if route == "/api/me":
            if session == "admin":
                return result({"id": uid(1), "organization_id": uid(2), "is_super_admin": True, "is_active": True})
            return result(self.users[session])
        if route == "/api/resellers":
            return result({"resellers": [{"id": uid(3), "status": "active", "organization_count": 1, "max_organizations": 20}]})
        if route == "/api/organizations/current":
            return result({"id": organization_id})
        if route == "/api/organizations":
            if method == "GET":
                return result({"organizations": self.orgs})
            self.next_org += 1
            item = {"id": uid(self.next_org), "name": body["name"], "slug": body["name"], "reseller_id": body["reseller_id"]}
            self.orgs.append(item)
            return result(item)
        if route.endswith("/product/plans"):
            org = next(v for v in self.orgs if v["id"] == route.split("/")[4])
            role = "klinik" if org["name"] == self.d["klinik"]["organization_name"] else "non_klinik"
            wanted = self.d[role]["plan"]
            price = {k: wanted[k] for k in ("currency", "unit_amount_minor", "setup_amount_minor", "interval", "interval_count", "tax_behavior")}
            price.update({"id": wanted["price_id"], "code": wanted["price_code"], "assignable": True})
            return result({"plans": [{"id": wanted["plan_id"], "code": wanted["plan_code"], "name": wanted["plan_name"],
                                      "vertical": wanted["vertical"], "status": "active", "entitlements": {}, "prices": [price]}]})
        if route.endswith("/subscription"):
            if method == "PUT":
                wanted = next(self.d[k]["plan"] for k in ("klinik", "non_klinik") if self.d[k]["plan"]["plan_id"] == body["plan_id"])
                self.subscriptions[route] = {"id": uid(200 + len(self.subscriptions)), "plan_id": body["plan_id"],
                                             "plan_price_id": body["plan_price_id"], "plan_code": wanted["plan_code"], "provider": "manual", "status": "active"}
            return result(self.subscriptions[route])
        if route == "/api/roles":
            return page("roles", [{"id": uid(300 if organization_id == uid(101) else 301), "name": "agent",
                                   "is_system": True, "is_default": True, "permissions": sorted(fixture.AGENT_PERMISSIONS)}])
        if route == "/api/users":
            if method == "GET":
                return page("users", [v for v in self.users.values() if v["organization_id"] == organization_id])
            identity = uid(400 + len(self.users))
            self.users[identity] = {k: body[k] for k in ("email", "full_name", "role_id")}
            self.users[identity].update({"id": identity, "organization_id": organization_id, "is_active": True, "is_super_admin": False})
            return result(self.users[identity])
        if route == "/api/organizations/members":
            return page("members", [{"id": uid(500 + n), "user_id": v["id"], "organization_id": organization_id,
                                     "role_id": v["role_id"], "is_active": True} for n, v in enumerate(self.users.values()) if v["organization_id"] == organization_id])
        if route == "/api/integrations/meta":
            self.meta = {"provider": "meta", "enabled": True, "configured": True,
                         "status": "configured", "oauth": {"available": True},
                         "config": {**body["config"], "api_version": "v21.0", "management_mode": "workspace",
                                    "webhook_callback_path": "/api/webhook?workspace=" + organization_id},
                         "credentials": {k: {"configured": True, "source": "workspace"}
                                         for k in ("app_secret", "webhook_verify_token")}}
            return result(self.meta)
        if route == "/api/integrations":
            return result({"integrations": [self.meta]})
        if route == "/api/accounts":
            if method == "GET":
                return result({"accounts": self.accounts if organization_id == uid(101) else []})
            account = {k: v for k, v in body.items() if k != "access_token"}
            account.update({"id": uid(600), "status": "active", "has_access_token": True})
            self.accounts.append(account)
            return result(account)
        if route.startswith("/api/accounts/"):
            return result(self.accounts[0])
        if route == "/api/webhook":
            value = body["entry"][0]["changes"][0]["value"]
            index = len(self.rows)
            contact, conversation, shadow = uid(700 + index), uid(800 + index), uid(900)
            sender = value["contacts"][0]["wa_id"]
            name = value["contacts"][0]["profile"]["name"]
            external = "legacy-contact:" + contact
            seed = value["messages"][0]
            self.messages[conversation] = [{"id": uid(1000 + index), "organization_id": uid(101),
                "contact_id": contact, "inbox_conversation_id": conversation,
                "whatsapp_account": self.d["meta"]["account_name"], "whatsapp_message_id": seed["id"],
                "direction": "incoming", "message_type": "text", "content": seed["text"]["body"],
                "status": "received", "is_reply": False, "error_message": ""}]
            self.rows.append({"id": conversation, "organization_id": uid(101), "channel_account_id": shadow,
                "contact_id": contact, "channel": "whatsapp", "external_conversation_id": external, "unread_count": 1,
                "contact": {"id": contact, "organization_id": uid(101), "phone_number": sender, "profile_name": name,
                            "whatsapp_account": self.d["meta"]["account_name"]},
                "channel_account": {"id": shadow, "organization_id": uid(101), "channel": "whatsapp", "provider": "meta_legacy",
                    "name": "WhatsApp " + self.d["meta"]["account_name"] + " [" + uid(600) + "]",
                    "external_account_id": "legacy-account:" + uid(600), "status": "active", "has_credentials": False,
                    "capabilities": {"text": True, "replies": True, "service_window": True, "legacy_text_reply_endpoint": True},
                    "config": {"legacy_read_only": True, "outbound_enabled": False, "reply_route": "chat"}},
                "contact_identity": {"organization_id": uid(101), "contact_id": contact, "channel_account_id": shadow,
                    "channel": "whatsapp", "external_id": external, "address": sender, "normalized_address": sender,
                    "display_name": name, "is_primary": True, "is_verified": True}})
            return result({"status": "ok"})
        if route == "/api/conversations":
            return page("conversations", self.rows)
        if route.startswith("/api/conversations/") and route.endswith("/messages"):
            return page("messages", [{"message": item, "parts": None}
                                     for item in self.messages[route.split("/")[3]]])
        if route.endswith("/read"):
            self.rows[1]["unread_count"] = 0
            self.messages[self.rows[1]["id"]][0]["status"] = "read"
            return result({"read_at": "2026-09-06T00:00:00Z", "provider_synced": False, "legacy_state_synced": True})
        raise AssertionError("unexpected synthetic route")


class TestProductProvisioner(unittest.TestCase):
    def setUp(self):
        self.request, self.protected = inputs()
        self.transport = FakeTransport(self.protected)
        self.gate = FakeGate()

    def controller(self):
        return fixture.ProductProvisioner(self.request, self.protected, self.transport, self.gate)

    def test_happy_path_exact_effect_inventory_and_no_secrets_in_receipt_or_gate(self):
        descriptor, receipt = self.controller().provision()
        self.assertEqual(receipt["fixture_descriptor_sha256"], common.sha256_value(descriptor))
        self.assertEqual([s["stage"] for s in receipt["stages"]], list(fixture.PUBLIC_STAGES))
        self.assertEqual(sum(s["nested_upper_bound"] for s in receipt["stages"]), 1)
        self.assertEqual(receipt["state"], "fixture_rows_verified")
        evidence = json.dumps(receipt) + str(self.gate.calls)
        for key, value in self.protected["credentials"].items():
            self.assertNotIn(value["password"] if key == "super_admin_login" else value, evidence)
        account = next(c for c in self.transport.calls if c[:2] == ("POST", "/api/accounts"))[2]
        for key in ("is_default_incoming", "is_default_outgoing", "auto_read_receipt", "business_calling_enabled"):
            self.assertIs(account[key], False)
        self.assertFalse(any("subscribe" in c[1] or "webhook-override" in c[1] for c in self.transport.calls if c[0] != "GET"))

    def test_unknown_key_source_and_selector_digest_are_rejected_before_authentication(self):
        for mutate in (lambda p: p.update(extra=1), lambda p: p["descriptor"].update(extra=1),
                       lambda p: p["credentials"].update(extra="hidden"),
                       lambda p: p["descriptor"].update(source_sha="f" * 40)):
            candidate = copy.deepcopy(self.protected)
            mutate(candidate)
            with self.assertRaises(common.ReleaseError):
                fixture.ProductProvisioner(self.request, candidate, self.transport, self.gate)
        self.assertEqual(self.transport.calls, [])

    def test_authority_registration_is_not_regenerated_during_provision(self):
        with mock.patch.object(fixture.secrets, "token_bytes", side_effect=AssertionError("must not regenerate")):
            self.controller().provision()
        self.protected["registration"]["klinik_email"] = "chosen@example.test"
        with self.assertRaises(common.ReleaseError):
            self.controller()

    def test_wrong_tenant_and_reserved_namespace_stop_before_effect(self):
        original = self.transport.request
        def wrong(*args, **kwargs):
            result = original(*args, **kwargs)
            if args[1] == "/api/organizations/current":
                result["data"]["id"] = uid(999)
            return result
        self.transport.request = wrong
        with self.assertRaises(common.ReleaseError):
            self.controller().provision()
        self.assertEqual(self.gate.calls, [])
        self.transport.request = original
        self.transport.orgs[0]["name"] = "rereply-canary-existing"
        with self.assertRaises(common.ReleaseError):
            self.controller().provision()
        self.assertEqual(self.gate.calls, [])

    def test_ambiguous_account_creation_stops_without_any_recovery_post(self):
        original = self.transport.request
        def ambiguous(method, path, *args, **kwargs):
            if (method, path) == ("POST", "/api/accounts"):
                raise TimeoutError(self.protected["credentials"]["meta_access_token"])
            return original(method, path, *args, **kwargs)
        self.transport.request = ambiguous
        controller = self.controller()
        with self.assertRaises(common.ReleaseError) as error:
            controller.provision()
        self.assertNotIn(self.protected["credentials"]["meta_access_token"], str(error.exception))
        self.assertEqual(self.gate.calls[-1][0], "create_account")
        with self.assertRaises(common.ReleaseError):
            controller.provision()
        self.assertEqual(len(self.gate.calls), 8)

    def test_complete_pagination_and_duplicate_or_total_drift_rejection(self):
        controller = self.controller()
        rows = [{"id": uid(1000 + i)} for i in range(101)]
        def pages(method, path, body, **kwargs):
            page = int(parse_qs(urlsplit(path).query)["page"][0])
            return {"status": "success", "data": {"rows": rows[(page-1)*100:page*100], "total": 101, "page": page, "limit": 100}}
        self.transport.request = pages
        self.assertEqual(len(controller._pages("/api/synthetic", "rows", org=uid(2))), 101)
        rows[-1] = rows[0]
        with self.assertRaises(common.ReleaseError):
            controller._pages("/api/synthetic", "rows", org=uid(2))

    def test_account_default_or_tenant_binding_drift_is_rejected(self):
        original = self.transport.request
        for field, value in (("is_default_incoming", True), ("status", "subscription_failed")):
            self.setUp()
            original = self.transport.request
            def changed(method, path, *args, **kwargs):
                result = original(method, path, *args, **kwargs)
                if (method, path) == ("POST", "/api/accounts"):
                    result["data"][field] = value
                return result
            self.transport.request = changed
            with self.assertRaises(common.ReleaseError):
                self.controller().provision()
            self.assertEqual(self.gate.calls[-1][0], "create_account")

    def test_rehydrate_has_no_effects_and_requires_original_descriptor_digest(self):
        descriptor, receipt = self.controller().provision()
        receipt = terminal_receipt(receipt,self.protected)
        before = len(self.transport.calls)
        result = fixture.rehydrate(self.request, self.protected, receipt, self.transport)
        self.assertEqual(result, descriptor)
        self.assertTrue(all(v[0] == "GET" for v in self.transport.calls[before:]))
        wrong = copy.deepcopy(receipt)
        wrong["fixture_descriptor_sha256"] = "f" * 64
        with self.assertRaises(common.ReleaseError):
            fixture.rehydrate(self.request, self.protected, wrong, self.transport)

    def test_rehydrate_rejects_either_replacement_login_before_authentication(self):
        descriptor,base = self.controller().provision()
        receipt = terminal_receipt(base,self.protected)
        for role in ("klinik","non_klinik"):
            changed = copy.deepcopy(self.protected)
            changed["registration"][role+"_email"] = ("k-" if role == "klinik" else "n-")+"c"*51+"a@fixtures.example.test"
            fixture.validate_protected_input(changed,self.request)
            user = next(v for v in self.transport.users.values()
                        if v["email"] == self.protected["registration"][role+"_email"])
            alternate = copy.deepcopy(user);alternate.update(id=uid(1999),email=changed["registration"][role+"_email"])
            self.transport.users[uid(1999)] = alternate
            with mock.patch.object(self.transport,"login") as login, self.assertRaisesRegex(
                    common.ReleaseError,"^original signed login registration differs$"):
                fixture.rehydrate(self.request,changed,receipt,self.transport)
            login.assert_not_called()
            del self.transport.users[uid(1999)]
        with self.assertRaises(common.ReleaseError):
            fixture.rehydrate(self.request,self.protected,base,self.transport)

    def test_real_http_webhook_is_signed_anonymous_and_invalid_handles_fail(self):
        transport = fixture.ProductHTTP(self.protected["credentials"]["meta_access_token"])
        controller = fixture.ProductProvisioner(self.request,self.protected,transport,self.gate)
        controller.admin = "unused-admin-handle"
        with mock.patch.object(fixture,"_wire",return_value=b'{"status":"ok"}') as wire:
            controller._webhook("a",uid(101))
            wire.assert_called_once()
            args,kwargs = wire.call_args
            self.assertFalse(any(isinstance(h,fixture.urllib.request.HTTPCookieProcessor) for h in args[0].handlers))
            self.assertEqual(args[1],fixture.PRODUCT_ORIGIN+"/api/webhook?workspace="+uid(101))
            self.assertFalse(set(kwargs["headers"]) & {"Cookie","X-CSRF-Token","Authorization"})
            expected = "sha256="+fixture.hmac.new(self.protected["credentials"]["meta_app_secret"].encode(),
                                                kwargs["body"],hashlib.sha256).hexdigest()
            self.assertEqual(kwargs["headers"]["X-Hub-Signature-256"],expected)
            with self.assertRaises(common.ReleaseError):
                transport.request("POST","/api/accounts",{},session="invalid")
            with self.assertRaises(common.ReleaseError):
                transport.request("POST","/api/accounts",{},session=False)
            wire.assert_called_once()

    def test_rehydrate_rejects_replaced_principal_even_with_original_email(self):
        for role in ("klinik","non_klinik"):
            self.setUp()
            _,base = self.controller().provision()
            receipt = terminal_receipt(base,self.protected)
            old = next(v for v in self.transport.users.values()
                       if v["email"] == self.protected["registration"][role+"_email"])
            replacement = copy.deepcopy(old);replacement["id"] = uid(1999)
            del self.transport.users[old["id"]]
            self.transport.users[uid(1999)] = replacement
            with mock.patch.object(self.transport,"login",wraps=self.transport.login) as login:
                with self.assertRaises(common.ReleaseError):
                    fixture.rehydrate(self.request,self.protected,receipt,self.transport)
                self.assertNotIn(mock.call(replacement["email"],self.protected["credentials"][role+"_password"]),
                                 login.call_args_list)

    def test_driver_schema_and_canonical_hash_match_existing_node_consumer(self):
        descriptor, receipt = self.controller().provision()
        node = shutil.which("node")
        self.assertIsNotNone(node, "Node is required for driver descriptor compatibility")
        root = Path(__file__).resolve().parents[2]
        runner = (root / "frontend/canary-driver/runner.mjs").as_uri()
        driver = (root / "frontend/canary-driver/index.mjs").as_uri()
        script = "import {validateFixtureDescriptor} from " + json.dumps(runner) + ";import {canonicalJson} from " + json.dumps(driver) + ";import fs from 'node:fs';import {createHash} from 'node:crypto';let d=validateFixtureDescriptor(JSON.parse(fs.readFileSync(0,'utf8')));process.stdout.write(createHash('sha256').update(canonicalJson(d)).digest('hex'));"
        result = subprocess.run([node, "--input-type=module", "-e", script], input=json.dumps(descriptor), text=True, capture_output=True, timeout=30, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, receipt["fixture_descriptor_sha256"])

    def test_pending_mirror_waits_only_by_get_without_webhook_replay(self):
        original = self.transport.request
        pending = [True]
        def delayed(method,path,*args,**kwargs):
            result = original(method,path,*args,**kwargs)
            if urlsplit(path).path == "/api/conversations" and pending:
                pending.pop()
                result["data"].update(conversations=[],total=0)
            return result
        self.transport.request = delayed
        with mock.patch.object(fixture.time,"sleep") as pause:
            self.controller().provision()
        pause.assert_called_once_with(2)
        self.assertEqual(sum(c[0] == "POST" and c[1].startswith("/api/webhook?")
                             for c in self.transport.calls),2)

    def test_seed_message_wrong_extra_and_flat_wrapper_fail_closed(self):
        for kind in ("wrong_wamid","outgoing","extra","flat"):
            self.setUp()
            original = self.transport.request
            def changed(method,path,*args,**kwargs):
                result = original(method,path,*args,**kwargs)
                if urlsplit(path).path.endswith("/messages"):
                    data = result["data"]
                    if kind == "wrong_wamid": data["messages"][0]["message"]["whatsapp_message_id"] = "wrong"
                    if kind == "outgoing": data["messages"][0]["message"]["direction"] = "outgoing"
                    if kind == "extra":
                        row = copy.deepcopy(data["messages"][0]);row["message"]["id"] = uid(1900)
                        data["messages"].append(row);data["total"] = 2
                    if kind == "flat": data["messages"] = [data["messages"][0]["message"]]
                return result
            self.transport.request = changed
            with self.subTest(kind=kind), self.assertRaises(common.ReleaseError):
                self.controller().provision()
            self.assertNotIn("clear_b",[c[0] for c in self.gate.calls])

    def test_degraded_or_shadow_meta_rejected_before_account(self):
        for mutate in (lambda d:d.update(status="degraded"),
                       lambda d:d["oauth"].update(available=False),
                       lambda d:d["credentials"]["app_secret"].update(source="platform"),
                       lambda d:d["config"].update(webhook_callback_path="/api/webhook")):
            self.setUp();original = self.transport.request
            def changed(method,path,*args,**kwargs):
                result = original(method,path,*args,**kwargs)
                if (method,path) == ("PUT","/api/integrations/meta"): mutate(result["data"])
                return result
            self.transport.request = changed
            with self.assertRaises(common.ReleaseError): self.controller().provision()
            self.assertNotIn("create_account",[c[0] for c in self.gate.calls])

    def test_phone_binding_mismatch_never_creates_account(self):
        original = self.transport.request
        def changed(method,path,*args,**kwargs):
            result = original(method,path,*args,**kwargs)
            if urlsplit(path).path.endswith("/phone_numbers"):
                result["data"][0]["display_phone_number"] = "999999"
            return result
        self.transport.request = changed
        with self.assertRaises(common.ReleaseError): self.controller().provision()
        self.assertNotIn("create_account",[c[0] for c in self.gate.calls])


# CLAIM_ADAPTER_TESTS

import datetime as dt
import os
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor


def origin_intent():
    request, _ = inputs()
    return {"schema_version":1,"kind":"crm-canary-fixture-intent",
            "control_sha":request["control_sha"],"origin_run_id":"12345","origin_run_attempt":1,
            "executor_job":fixture.EXECUTOR_JOB,"workflow_path":fixture.WORKFLOW_PATH,
            "workflow_sha256":"b"*64,"controller_sha256":"c"*64,
            "upload_bundle_sha256":fixture.UPLOAD_BUNDLE_SHA256,"request":request,
            "slots":fixture.stage_slots(),"issued_at":"2026-09-05T00:00:00Z",
            "expires_at":"2026-09-06T00:00:00Z"}


class MemoryClaims:
    """Fault oracle only, never the production durable backend."""
    def __init__(self):
        self.records = {}
        self.lock = threading.Lock()
        self.fail = None

    def issue(self, record):
        with self.lock:
            name = record["artifact_name"]
            if name in self.records:
                raise common.ReleaseError("conflict")
            self.records[name] = copy.deepcopy(record)
        if self.fail == "after_finalize":
            raise common.ReleaseError("response lost")
        return {"artifact-id":"77","artifact-digest":"a"*64,
                "artifact-url":"unused-in-injected-oracle"}

    def get(self, path):
        record = next(iter(self.records.values()))
        return {"id":77,"name":record["artifact_name"],"digest":"sha256:"+"a"*64,
                "size_in_bytes":1024,"expired":False,"expires_at":"2099-01-01T00:00:00Z",
                "workflow_run":{"id":12345,"head_sha":record["control_sha"],"head_branch":"main"}}

    def artifact(self, identity, digest):
        record = next(iter(self.records.values()))
        output = fixture.io.BytesIO()
        with fixture.zipfile.ZipFile(output,"w") as archive:
            archive.writestr("claim.json",common.canonical_file_bytes(record))
        return output.getvalue()


def test_gate(backend):
    gate = object.__new__(fixture.ClaimGate)
    gate.intent = origin_intent()
    gate.api = backend
    gate.claim_test = False
    gate.used,gate.position,gate.records = set(),0,[]
    gate.job_id = "54321"
    gate.before_effect = lambda:None
    gate._fresh_claim = backend.issue
    return gate


class TestClaimAdapter(unittest.TestCase):
    def test_probe_wrapper_signals_only_exact_backend_conflict(self):
        node = shutil.which("node")
        self.assertIsNotNone(node)
        annotation = "::error::Failed to CreateArtifact: Received non-retryable error: Failed request: (409) Conflict: an artifact with this name already exists on the workflow run\n"
        error = "process.stdout.write("+json.dumps(annotation)+");"
        cases = [
            (error+"process.exitCode=1;",fixture.CLAIM_CONFLICT_EXIT),
            (error+"process.exitCode=0;",1),
            (error+error+"process.exitCode=1;",1),
            (error+"console.error('different');process.exitCode=1;",1),
            (error+"process.stdout.write('x'.repeat(8193));process.exitCode=1;",1),
            (error+"process.stdout.write('\\u00e9'.repeat(5000));process.exitCode=1;",1),
            (error+"process.stdout.write('::er');process.stdout.write('ror::different\\n');process.exitCode=1;",1),
            (error+"process.stdout.write('::er');process.stdout.write('ror::different');process.exitCode=1;",1),
            ("process.stdout.write("+json.dumps(annotation[:7])+");process.stdout.write("+
             json.dumps(annotation[7:])+");process.exitCode=1;",fixture.CLAIM_CONFLICT_EXIT),
            (error+"process.stdout.write('x'.repeat(5000));process.stdout.write('x'.repeat(5000));process.exitCode=1;",1),
            ("process.exit(73);",1),
            (error.replace("CreateArtifact","FinalizeArtifact")+"process.exitCode=1;",1),
            (error.replace("(409) Conflict","(500) Error")+"process.exitCode=1;",1),
            ("console.log('harmless progress');",0),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            bundle = Path(tmp)/"probe.cjs"
            for source,expected in cases:
                bundle.write_text(source,encoding="utf-8")
                process = subprocess.run([node,"-e",fixture.CLAIM_PROBE_WRAPPER,str(bundle)],
                                         capture_output=True,timeout=10,check=False)
                self.assertEqual(process.returncode,expected)
                self.assertEqual(process.stdout,b"")
                self.assertEqual(process.stderr,b"")

    def test_hosted_probe_does_not_accept_guard_or_transport_failure(self):
        for failure in ("duplicate","guard","transport"):
            backend = MemoryClaims()
            first,second = test_gate(backend),test_gate(backend)
            first.claim_test = second.claim_test = True
            def reject(): raise common.ReleaseError("unrelated failure")
            if failure == "guard": second.before_effect = reject
            def fresh(record):
                if failure == "duplicate": raise fixture.ClaimConflict("verified duplicate")
                raise common.ReleaseError("transport failure")
            second._fresh_claim = fresh
            with mock.patch.object(fixture,"_current_guard"), \
                 mock.patch.object(fixture,"_intent_from_current",return_value=({},None)), \
                 mock.patch.object(fixture,"acquire_origin",return_value=(origin_intent(),{},[],[])), \
                 mock.patch.object(fixture,"_require_intent_code"), \
                 mock.patch.object(fixture,"ClaimGate",side_effect=[first,second]), \
                 mock.patch.dict(os.environ,{"CLAIM_NODE":"synthetic"}):
                if failure == "duplicate":
                    fixture._hosted_claim_test(backend,Path("."),Path("unused"),Path("unused"))
                else:
                    with self.assertRaises(common.ReleaseError):
                        fixture._hosted_claim_test(backend,Path("."),Path("unused"),Path("unused"))

    def test_conflict_signal_with_success_fields_is_not_duplicate_proof(self):
        gate = test_gate(MemoryClaims())
        gate.node,gate.bundle,gate.claim_test = "/synthetic/node",Path("synthetic-upload.js"),True
        with tempfile.TemporaryDirectory() as tmp:
            env = {"RUNNER_TEMP":tmp,"ACTIONS_RUNTIME_TOKEN":"runtime-only","ACTIONS_RESULTS_URL":"https://results.example.test/"}
            def conflict(command,**kwargs):
                Path(kwargs["env"]["GITHUB_OUTPUT"]).write_text("artifact-id=77\n",encoding="utf-8")
                return mock.Mock(returncode=fixture.CLAIM_CONFLICT_EXIT)
            with mock.patch.dict(os.environ,env,clear=True), mock.patch.object(fixture.os,"fchmod",create=True), \
                 mock.patch.object(fixture.subprocess,"run",side_effect=conflict):
                with self.assertRaises(common.ReleaseError) as caught:
                    fixture.ClaimGate._fresh_claim(gate,{"artifact_name":"synthetic"})
                self.assertNotIsInstance(caught.exception,fixture.ClaimConflict)

    def test_fresh_child_environment_excludes_application_credentials(self):
        gate = test_gate(MemoryClaims())
        gate.node,gate.bundle = "/synthetic/node",Path("synthetic-upload.js")
        record = {"artifact_name":"synthetic-burn"}
        with tempfile.TemporaryDirectory() as tmp:
            env = {"RUNNER_TEMP":tmp,"ACTIONS_RUNTIME_TOKEN":"runtime-only",
                   "ACTIONS_RESULTS_URL":"https://results.example.test/", "GH_TOKEN":"never-forward",
                   "CRM_CANARY_FIXTURE_INPUT_JSON":"never-forward", "DO_PRODUCTION_FIXTURE_UPDATE_TOKEN":"never-forward"}
            def launch(command,**kwargs):
                child = kwargs["env"]
                self.assertFalse(set(child) & {"GH_TOKEN","CRM_CANARY_FIXTURE_INPUT_JSON","DO_PRODUCTION_FIXTURE_UPDATE_TOKEN"})
                self.assertEqual(child["INPUT_OVERWRITE"],"false")
                self.assertEqual(kwargs["stdout"],subprocess.DEVNULL)
                self.assertEqual(kwargs["stderr"],subprocess.DEVNULL)
                Path(child["GITHUB_OUTPUT"]).write_text("artifact-id=77\nartifact-digest="+"a"*64+
                    "\nartifact-url=https://github.com/"+common.REPOSITORY+"/actions/runs/12345/artifacts/77\n",encoding="utf-8")
                return mock.Mock(returncode=0)
            with mock.patch.dict(os.environ,env,clear=True), mock.patch.object(fixture.os,"fchmod",create=True), \
                 mock.patch.object(fixture.subprocess,"run",side_effect=launch):
                self.assertEqual(fixture.ClaimGate._fresh_claim(gate,record)["artifact-id"],"77")
            with mock.patch.dict(os.environ,env,clear=True), mock.patch.object(fixture.os,"fchmod",create=True), \
                 mock.patch.object(fixture.subprocess,"run",return_value=mock.Mock(returncode=1)):
                with self.assertRaises(common.ReleaseError): fixture.ClaimGate._fresh_claim(gate,record)

    def test_provider_complete_history_rejects_active_conflict_and_bad_pages(self):
        provider = object.__new__(fixture.ProviderFixture)
        provider.target = {"app_id":uid(9000)}
        good = {"deployments":[{"id":uid(9001),"phase":"ACTIVE"}],"meta":{"total":1},"links":{"pages":{}}}
        provider._get = lambda path:copy.deepcopy(good)
        self.assertEqual(provider.terminal_inventory(uid(9001)),good["deployments"])
        for mutate in (lambda d:d["deployments"][0].update(phase="PENDING_BUILD"),
                       lambda d:d["meta"].update(total=2),
                       lambda d:d["links"]["pages"].update(next="https://example.test/?page=2"),
                       lambda d:d["deployments"].append(d["deployments"][0])):
            candidate = copy.deepcopy(good);mutate(candidate)
            provider._get = lambda path:copy.deepcopy(candidate)
            with self.assertRaises(common.ReleaseError): provider.terminal_inventory(uid(9001))

    def test_approval_expiring_during_claim_cannot_send(self):
        intent = origin_intent()
        authority = {"expires_at": "2026-09-05T01:00:00Z"}
        for second in (0, 1):
            with self.subTest(after_expiry_seconds=second):
                gate = test_gate(MemoryClaims())
                clocks = iter((dt.datetime(2026,9,5,0,59,59,tzinfo=dt.timezone.utc),
                               dt.datetime(2026,9,5,1,0,second,tzinfo=dt.timezone.utc)))
                gate.before_effect = lambda:fixture._require_effect_window(intent,authority,next(clocks))
                with self.assertRaises(common.ReleaseError):
                    gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:self.fail("sent"))
                self.assertEqual(gate.position,0)
                self.assertEqual(len(gate.api.records),1)

    def test_two_contenders_get_at_most_one_send(self):
        backend = MemoryClaims()
        sent = []
        def run():
            gate = test_gate(backend)
            try:
                gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:sent.append(1))
                return True
            except common.ReleaseError:
                return False
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda _:run(),range(2)))
        self.assertEqual(sum(results),1)
        self.assertEqual(sent,[1])

    def test_lost_finalization_and_restart_cannot_send(self):
        backend = MemoryClaims()
        backend.fail = "after_finalize"
        sent = []
        for _ in range(2):
            with self.assertRaises(common.ReleaseError):
                test_gate(backend).once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:sent.append(1))
        self.assertEqual(sent,[])
        self.assertEqual(len(backend.records),1)

    def test_readback_failure_and_parent_crash_never_reissue(self):
        backend = MemoryClaims()
        gate = test_gate(backend)
        with mock.patch.object(backend,"artifact",side_effect=common.ReleaseError("missing")):
            with self.assertRaises(common.ReleaseError):
                gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:self.fail("sent"))
        with self.assertRaises(common.ReleaseError):
            test_gate(backend).once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:self.fail("resent"))

    def test_permit_consumed_before_callback_and_duplicate_rejected(self):
        backend = MemoryClaims()
        gate = test_gate(backend)
        def callback():
            self.assertEqual(gate.position,1)
            raise ValueError("synthetic transport EOF")
        with self.assertRaises(common.AmbiguousMutation):
            gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",callback)
        with self.assertRaises(common.ReleaseError):
            gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:self.fail("resent"))

    def test_exact_sequence_and_nested_budget(self):
        gate = test_gate(MemoryClaims())
        for stage,budget in (("create_account",1),(fixture.STAGES[0],1),("extra",0)):
            with self.assertRaises(common.ReleaseError):
                gate.once(stage,"POST","/api/organizations",b"{}",lambda:self.fail("sent"),nested_budget=budget)
        self.assertEqual(gate.used,set())

    def test_expired_or_wrong_artifact_records_fail(self):
        backend = MemoryClaims()
        gate = test_gate(backend)
        for field,value in (("expired",True),("name","unexpected"),("digest","sha256:"+"f"*64)):
            with self.subTest(field=field):
                backend.records.clear()
                gate = test_gate(backend)
                original = backend.get
                def altered(path):
                    result = original(path)
                    result[field] = value
                    return result
                with mock.patch.object(backend,"get",side_effect=altered):
                    with self.assertRaises(common.ReleaseError):
                        gate.once(fixture.STAGES[0],"POST","/api/organizations",b"{}",lambda:self.fail("sent"))

    def test_outputs_require_unique_exact_fresh_fields(self):
        raw = "artifact-id<<unique\n77\nunique\nartifact-digest="+"a"*64+"\nartifact-url=https://example.test/a\n"
        self.assertEqual(fixture._parse_action_outputs(raw)["artifact-id"],"77")
        for mutated in (raw+"artifact-id=77\n",raw.replace("unique\nartifact","wrong\nartifact"),
                        raw.replace("artifact-digest","extra"),"",raw.replace("77","0")):
            with self.assertRaises(common.ReleaseError):
                fixture._parse_action_outputs(mutated)

    def test_origin_slots_attempt_and_lifetime_are_exact(self):
        intent = origin_intent()
        self.assertEqual(fixture.validate_origin_intent(intent),intent)
        for key,value in (("origin_run_attempt",2),("executor_job","other"),
                          ("upload_bundle_sha256","f"*64),("slots",intent["slots"][:-1])):
            changed = copy.deepcopy(intent);changed[key]=value
            with self.assertRaises(common.ReleaseError):
                fixture.validate_origin_intent(changed)
        with self.assertRaises(common.ReleaseError):
            fixture.validate_origin_intent(intent,now=dt.datetime(2026,9,7,tzinfo=dt.timezone.utc))

    def test_exact_archive_cannot_include_extra_or_duplicate_files(self):
        for names in (["claim.json","other"],["claim.json","claim.json"],["../claim.json"]):
            output=fixture.io.BytesIO()
            with warnings.catch_warnings(), fixture.zipfile.ZipFile(output,"w") as archive:
                warnings.filterwarnings("ignore", message="Duplicate name: 'claim.json'", category=UserWarning)
                for name in names:
                    archive.writestr(name,"{}")
            with self.assertRaises(common.ReleaseError):
                fixture._extract_exact(output.getvalue(),{"claim.json"})

    def test_allowlist_append_preserves_entire_spec_and_rejects_shadows(self):
        spec={"name":"synthetic","services":[{"name":"omnitech-web","envs":[
            {"key":fixture.ALLOWLIST_KEY,"value":uid(9001),"scope":"RUN_TIME"},
            {"key":fixture.ENABLE_KEY,"value":"true","scope":"RUN_TIME"}],
            "image":{"digest":"sha256:"+"a"*64}}]}
        result=fixture.ProviderFixture.appended_spec(spec,uid(9002),uid(9003))
        self.assertEqual(result["services"][0]["envs"][0]["value"],uid(9001)+","+uid(9002))
        reverted=copy.deepcopy(result)
        reverted["services"][0]["envs"][0]["value"]=uid(9001)
        self.assertEqual(reverted,spec)
        for change in (lambda x:x.update(envs=[{"key":fixture.ALLOWLIST_KEY,"value":uid(9001)}]),
                       lambda x:x["services"][0]["envs"][0].update(type="SECRET"),
                       lambda x:x["services"][0]["envs"][0].update(value=uid(9001)+","+uid(9002))):
            mutant=copy.deepcopy(spec);change(mutant)
            with self.assertRaises(common.ReleaseError):
                fixture.ProviderFixture.appended_spec(mutant,uid(9002),uid(9003))
