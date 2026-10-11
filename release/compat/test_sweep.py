"""Synthetic HTTP contract tests only. No socket, server, Docker or credentials."""
from dataclasses import replace
import hashlib
import hmac
import json
from pathlib import Path
import sys
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import sweep as s

NS = "rereply-compat-0123456789ab"
ORG = "10000000-0000-4000-8000-000000000001"
ACCOUNT = "10000000-0000-4000-8000-000000000002"
CHANNEL = "10000000-0000-4000-8000-000000000003"
WALL = 1_790_000_000
NONCE = "0123456789abcdef0123456789abcdef"


def fixture():
    return s.Fixture(NS, "http://127.0.0.1:8080", "http://127.0.0.1:8090", ORG, ACCOUNT, CHANNEL,
                     NS + "-wa", "999123456789012", NS + "-client@example.test", "a" * 48, "b" * 48)


class World:
    """Models the actual documented envelopes, independent of adapter projections."""
    def __init__(self):
        self.f = fixture()
        self.next_id = 10
        self.contacts, self.conversations, self.messages = {}, {}, {}
        self.templates, self.flows = {}, {}
        self.settings = {"enabled": False, "ai_enabled": False, "greeting_message": "original", "session_timeout_minutes": 37}
        self.entries, self.calls, self.pages = [], [], []
        self.mutate = lambda method, path, result: result
        self.intercept = lambda method, path, body: None

    def uid(self):
        self.next_id += 1
        return "20000000-0000-4000-8000-" + str(self.next_id).zfill(12)

    def add(self, **row):
        self.entries.append({"seq": len(self.entries) + 1, **row})

    def transport(self, stub=False):
        world = self
        class Fake:
            origin = world.f.stub_origin if stub else world.f.product_origin
            def request(self, method, path, headers, body, timeout):
                world.calls.append((method, path, headers.copy(), body, timeout))
                override = world.intercept(method, path, body)
                if override is not None:
                    return override
                value = json.loads(body) if body else None
                if stub:
                    assert "Cookie" not in headers and "X-CSRF-Token" not in headers
                    mac = "|".join((method, path, headers["X-Stub-Timestamp"], headers["X-Stub-Nonce"], hashlib.sha256(body).hexdigest()))
                    assert headers["X-Stub-Mac"] == hmac.new(world.f.control_key.encode(), mac.encode(), hashlib.sha256).hexdigest()
                    assert headers["X-Stub-Timestamp"] == str(WALL)
                    data = world.stub(method, path, value)
                    response = data
                    cookies = ()
                else:
                    assert headers["X-Organization-ID"] == ORG
                    if path != "/api/auth/login":
                        assert headers["X-CSRF-Token"] == "csrf_token"
                        assert "whm_access=access_token" in headers["Cookie"]
                    data = world.product(method, path, value)
                    response = {"data": data}
                    cookies = (("Set-Cookie", "whm_access=access_token; HttpOnly; Path=/"),
                               ("Set-Cookie", "whm_csrf=csrf_token; Path=/"),
                               ("Set-Cookie", "whm_refresh=private_refresh; Path=/api/auth/refresh")) if path == "/api/auth/login" else ()
                response = world.mutate(method, path, response)
                return s.Reply(200, cookies, json.dumps(response).encode())
        return Fake()

    def client(self):
        client = s.Sweep(self.f, product=self.transport(), stub=self.transport(True), wall=lambda: WALL, sleep=lambda _: None)
        client.login()
        return client

    def product(self, method, path, value):
        if path == "/api/auth/login":
            assert value == {"email": self.f.email, "password": self.f.password}
            return {"authenticated": True}
        if path == "/api/me":
            return {"email": self.f.email, "organization_id": ORG, "is_super_admin": False}
        if path == "/api/chatbot/settings":
            if method == "PUT":
                assert set(value) == {"greeting_message"}
                self.settings.update(value)
            return {"settings": dict(self.settings), "stats": {}}
        if path == "/api/contacts" and method == "POST":
            assert set(value) == {"phone_number", "profile_name", "whatsapp_account"}
            ident = self.uid()
            self.contacts[ident] = {"id": ident, **value}
            return dict(self.contacts[ident])
        if path.startswith("/api/contacts/"):
            return dict(self.contacts[path.rsplit("/", 1)[1]])
        if path.startswith("/api/conversations?"):
            phone = path.rsplit("=", 1)[1]
            return {"conversations": [v for v in self.conversations.values() if v["contact"]["phone_number"] == phone]}
        if path.startswith("/api/conversations/"):
            ident = path.split("/")[3]
            if path.endswith("/legacy-whatsapp-replies"):
                assert set(value) == {"idempotency_key", "type", "content"}
                assert value["type"] == "text" and set(value["content"]) == {"body"}
                text = value["content"]["body"]
                msg = {"id": self.uid(), "message_type": "text", "direction": "outgoing", "content": text,
                       "organization_id": ORG, "contact_id": self.conversations[ident]["contact_id"],
                       "whatsapp_account": self.f.account_name, "inbox_conversation_id": ident}
                self.messages[ident].append({"message": msg})
                self.add(kind="graph", route="messages", method="POST", status=200, message_type="text",
                         phone_number_id=self.f.phone_number_id, to=self.conversations[ident]["contact"]["phone_number"],
                         text_sha256=hashlib.sha256(text.encode()).hexdigest())
                return {"message": msg}
            return {"messages": self.messages[ident], "total": len(self.messages[ident]), "page": 1, "limit": 100}
        if path == "/api/templates" and method == "POST":
            assert value["category"] == "UTILITY" and value["header_type"] == "NONE"
            ident = self.uid()
            self.templates[ident] = {"id": ident, "status": "DRAFT", **value}
            return dict(self.templates[ident])
        if path.startswith("/api/templates/"):
            return dict(self.templates[path.rsplit("/", 1)[1]])
        if path == "/api/chatbot/flows" and method == "POST":
            assert value["on_complete_action"] == "none"
            assert {node["type"] for node in value["graph"]["nodes"]} == {"start", "end"}
            ident = self.uid()
            self.flows[ident] = {"id": ident, "organization_id": ORG, **value, "is_enabled": value["enabled"]}
            return {"id": ident, "message": "Flow created successfully"}
        if path.startswith("/api/chatbot/flows/"):
            return dict(self.flows[path.rsplit("/", 1)[1]])
        raise AssertionError("unexpected synthetic route")

    def stub(self, method, path, value):
        if path.startswith("/_control/journal?"):
            if self.pages:
                return self.pages.pop(0)
            cursor = int(path.split("after=")[1].split("&")[0])
            return {"entries": [dict(e) for e in self.entries if e["seq"] > cursor][:1000], "latest": len(self.entries)}
        assert path == "/_control/inbound" and method == "POST"
        assert value["phone_number_id"] == self.f.phone_number_id and value["type"] == "text"
        contacts = [c for c in self.contacts.values() if c["phone_number"] == value["from"]]
        assert len(contacts) == 1
        contact = contacts[0]
        ident = self.uid()
        self.conversations[ident] = {"id": ident, "organization_id": ORG, "channel_account_id": CHANNEL,
            "contact_id": contact["id"], "channel": "whatsapp", "external_conversation_id": "legacy-contact:" + contact["id"],
            "service_window_ends_at": "2099-01-01T00:00:00Z", "contact": {**contact, "organization_id": ORG},
            "channel_account": {"id": CHANNEL, "organization_id": ORG, "provider": "meta_legacy", "status": "active",
                                "channel": "whatsapp", "has_credentials": False,
                                "external_account_id": "legacy-account:" + ACCOUNT,
                                "capabilities": {"legacy_text_reply_endpoint": True, "text": True, "replies": True, "service_window": True},
                                "config": {"legacy_read_only": True, "outbound_enabled": False, "reply_route": "chat"}},
            "contact_identity": {"organization_id": ORG, "contact_id": contact["id"], "channel_account_id": CHANNEL,
                                 "channel": "whatsapp", "external_id": "legacy-contact:" + contact["id"],
                                 "address": value["from"], "normalized_address": value["from"], "display_name": value["profile_name"],
                                 "is_primary": True, "is_verified": True}}
        self.messages[ident] = [{"message": {"id": self.uid(), "message_type": "text", "direction": "incoming",
                                 "organization_id": ORG, "contact_id": contact["id"], "whatsapp_account": self.f.account_name,
                                 "inbox_conversation_id": ident,
                                 "content": value["text"], "whatsapp_message_id": value["message_id"]}}]
        self.add(kind="webhook", route="inbound", status=200, phone_number_id=self.f.phone_number_id, message_id=value["message_id"])
        return {"callback_status": 200}


class SweepTests(unittest.TestCase):
    def test_successful_stub_receipts_survive_read_budget_reset_without_credentials(self):
        world=World();client=world.client();record=client.write(NONCE)
        before=client.stub_receipts();client.read(record)
        self.assertEqual(client.stub_receipts(),before)
        self.assertEqual(len(before['windows']),4)
        self.assertEqual([v['method']+' '+v['route'] for v in before['calls']],
                         ['GET journal','POST inbound','GET journal','GET journal','GET journal'])
        before['calls'].clear()
        self.assertEqual(len(client.stub_receipts()['calls']),5)
        for private in (world.f.password,world.f.control_key,NONCE):
            self.assertNotIn(private,json.dumps(client.stub_receipts()))

    def test_rejected_fixed_endpoint_and_uncompleted_calls_do_not_get_receipts(self):
        world=World();client=world.client();client._begin()
        world.stub=lambda method,path,value: {'entries':[],'latest':0}
        self.assertEqual(client._rejected(0),([],0))
        self.assertEqual(world.calls[-1][1],'/_control/rejected?after=0&limit=1000')
        receipt=client.stub_receipts()
        world.intercept=lambda *args: s.Reply(403,(),b'{}')
        with self.assertRaises(s.SemanticFailure):client._rejected(0)
        self.assertEqual(client.stub_receipts(),receipt)

    def test_two_versions_write_read_reverse_and_exact_settings_patch(self):
        world = World()
        candidate = world.client()
        cand = candidate.write(NONCE)
        previous = world.client()
        self.assertEqual(previous.read(cand), {k: True for k in s.proof.SWEEP_CASES})
        old = previous.write("abcdef0123456789abcdef0123456789")
        self.assertEqual(world.client().read(old), {k: True for k in s.proof.SWEEP_CASES})
        self.assertNotEqual(cand.contact_id, old.contact_id)
        self.assertEqual(world.settings["session_timeout_minutes"], 37)
        self.assertFalse(world.settings["enabled"])
        self.assertFalse(world.settings["ai_enabled"])
        self.assertNotIn(ORG, repr(cand))
        self.assertNotIn(world.f.password, repr(world.f))
        self.assertFalse(any("private_refresh" in headers.get("Cookie", "") for _, _, headers, *_ in world.calls))

    def test_actual_handler_payload_shapes_are_pinned_in_repository(self):
        root = Path(__file__).resolve().parents[2]
        legacy = (root / "internal/handlers/legacy_whatsapp_reply.go").read_text()
        self.assertIn('json:"idempotency_key"', legacy)
        self.assertIn('json:"body"', legacy)
        chatbot = (root / "internal/handlers/chatbot.go").read_text()
        self.assertIn('"settings": settingsResp', chatbot)
        self.assertIn('GreetingMessage              *string', chatbot)
        self.assertIn('json:"is_enabled"', (root / "internal/models/chatbot.go").read_text())

    def test_external_and_cross_run_origins_refuse_before_request(self):
        for origin in ("https://rereply.app", "http://localhost:8080", "http://example.test:8080",
                       "http://127.0.0.1:8080/", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080?q=secret",
                       "http://rereply-compat-ffffffffffff-web:8080"):
            with self.subTest(origin=origin), self.assertRaises(s.proof.EvidenceError):
                replace(fixture(), product_origin=origin)
        self.assertEqual(s.synthetic_origin(f"http://{NS}-web:8080", NS, "web"), f"http://{NS}-web:8080")
        self.assertEqual(s.synthetic_origin("http://[::1]:8080", NS, "web"), "http://[::1]:8080")

    def test_fixture_secret_and_identity_validation(self):
        for change in ({"namespace": "customer"}, {"email": "person@example.test"}, {"password": "real-secret"},
                       {"organization_id": "invalid"}, {"phone_number_id": "60123456789"}, {"account_name": "other"}):
            with self.subTest(change=tuple(change)), self.assertRaises(s.proof.EvidenceError):
                replace(fixture(), **change)

    def test_wrong_login_org_or_super_admin_refuses(self):
        for change in ({"organization_id": ACCOUNT}, {"is_super_admin": True}, {"email": "other@example.test"}):
            world = World()
            world.mutate = lambda m, p, r: {"data": {**r["data"], **change}} if p == "/api/me" else r
            with self.assertRaises(s.SemanticFailure):
                world.client()
            self.assertEqual(len(world.calls), 2)

    def test_missing_auth_cookie_prevents_any_sweep(self):
        world = World()
        world.intercept = lambda m, p, b: s.Reply(200, (), b'{"data":{}}')
        with self.assertRaises(s.proof.EvidenceError):
            world.client()
        self.assertEqual(len(world.calls), 1)

    def test_disabled_automation_required_before_any_write(self):
        for key in ("enabled", "ai_enabled"):
            world = World()
            client = world.client()
            world.calls.clear()
            world.settings[key] = True
            with self.assertRaises(s.SemanticFailure):
                client.write(NONCE)
            self.assertTrue(all(method == "GET" for method, *_ in world.calls))

    def test_each_retained_case_detects_independent_drift(self):
        mutations = {
            "contacts": lambda w, r: w.contacts[r.contact_id].update(profile_name="changed"),
            "inbound": lambda w, r: w.messages[r.conversation_id][0]["message"].update(content="changed"),
            "outbound": lambda w, r: w.messages[r.conversation_id][1]["message"].update(content="changed"),
            "template": lambda w, r: w.templates[r.template_id].update(body_content="changed"),
            "flow": lambda w, r: w.flows[r.flow_id].update(description="changed"),
            "settings": lambda w, r: w.settings.update(greeting_message="changed"),
        }
        for case, mutate in mutations.items():
            with self.subTest(case=case):
                world = World()
                record = world.client().write(NONCE)
                mutate(world, record)
                client = world.client()
                world.calls.clear()
                actual = client.read(record)
                self.assertEqual(actual, {key: key != case for key in s.proof.SWEEP_CASES})
                self.assertEqual(len(world.calls), 7)
                self.assertEqual(world.calls[-1][1], "/api/chatbot/settings")
                self.assertTrue(all(method == "GET" for method, *_ in world.calls))
                self.assertFalse(client._failed)
                self.assertFalse(s.proof._sweep({"execution": {"status": "completed", "exit_code": 0, "failure": None},
                                                "cases": actual}))

    def test_all_six_false_are_complete_repeatable_observations(self):
        world = World(); record = world.client().write(NONCE)
        world.contacts[record.contact_id]["profile_name"] = "changed"
        for row in world.messages[record.conversation_id]: row["message"]["content"] = "changed"
        world.templates[record.template_id]["body_content"] = "changed"
        world.flows[record.flow_id]["trigger_keywords"] = []
        world.settings["greeting_message"] = "changed"
        client = world.client(); world.calls.clear()
        for _ in range(2):
            self.assertEqual(client.read(record), dict.fromkeys(s.proof.SWEEP_CASES, False))
        self.assertEqual(len(world.calls), 14)
        self.assertTrue(all(method == "GET" for method, *_ in world.calls))
        self.assertEqual(client.stub_receipts(), {"calls": [], "windows": []})

    def test_message_absence_requires_complete_validated_page(self):
        world = World(); record = world.client().write(NONCE)
        world.messages[record.conversation_id].pop(0)
        actual = world.client().read(record)
        self.assertFalse(actual["inbound"]); self.assertTrue(actual["outbound"])
        for metadata in ({"total": 2}, {"total": True}, {"page": 2}, {"page": True}, {"limit": 99}, {"total": None}, {"has_more": True}):
            world.mutate = lambda m, p, r: {"data": {**r["data"], **metadata}} if "/messages?" in p else r
            client = world.client()
            with self.assertRaisesRegex(s.proof.EvidenceError, "sweep-message-page-incomplete"):
                client.read(record)
            self.assertTrue(client._failed)

    def test_identity_safety_and_provider_bindings_stay_strict(self):
        mutations = [lambda w, r: w.contacts[r.contact_id].update(id=ACCOUNT),
                     lambda w, r: w.contacts[r.contact_id].update(whatsapp_account="other"),
                     lambda w, r: w.messages[r.conversation_id][0]["message"].update(whatsapp_message_id="other"),
                     lambda w, r: w.messages[r.conversation_id][1]["message"].update(direction="incoming"),
                     lambda w, r: w.templates[r.template_id].update(status="APPROVED"),
                     lambda w, r: w.flows[r.flow_id].update(graph={}),
                     lambda w, r: w.flows[r.flow_id].update(on_complete_action="handoff"),
                     lambda w, r: w.settings.update(enabled=True),
                     lambda w, r: w.settings.update(ai_enabled=True),
                     lambda w, r: w.conversations.clear()]
        for mutate in mutations:
            world = World(); record = world.client().write(NONCE); mutate(world, record)
            client = world.client()
            with self.assertRaises(s.proof.EvidenceError): client.read(record)
            self.assertTrue(client._failed)

    def test_mirrored_display_mismatch_is_false_but_ownership_is_not(self):
        for part, key in (("contact", "profile_name"), ("contact_identity", "display_name")):
            world = World(); record = world.client().write(NONCE)
            world.conversations[record.conversation_id][part][key] = "changed"
            self.assertFalse(world.client().read(record)["contacts"])

    def test_late_failure_never_returns_partial_false_cases(self):
        for status in (401, 403, 404, 500, 302):
            world = World(); record = world.client().write(NONCE)
            world.contacts[record.contact_id]["profile_name"] = "changed"
            world.intercept = lambda m, p, b: s.Reply(status, (), b'private') if p == "/api/chatbot/settings" else None
            client = world.client()
            with self.assertRaises(s.proof.EvidenceError) as caught: client.read(record)
            self.assertNotIn("private", str(caught.exception)); self.assertTrue(client._failed)
            count = len(world.calls)
            with self.assertRaises(s.proof.EvidenceError): client.read(record)
            self.assertEqual(len(world.calls), count)

    def test_malformed_later_field_is_not_hidden_by_earlier_false(self):
        for change in ({"name": "changed", "body_content": None}, {"name": "changed", "body_content": "x" * 8193}):
            world = World(); record = world.client().write(NONCE)
            world.templates[record.template_id].update(change)
            with self.assertRaisesRegex(s.proof.EvidenceError, "sweep-response-invalid"):
                world.client().read(record)
        world = World(); record = world.client().write(NONCE)
        world.flows[record.flow_id].update(name="changed", trigger_keywords=["ok", None])
        with self.assertRaises(s.proof.EvidenceError): world.client().read(record)

    def test_write_precheck_cannot_discard_complete_false_result(self):
        world = World(); client = world.client()
        world.mutate = lambda m, p, r: {"data": {**r["data"], "profile_name": "changed"}} if p.startswith("/api/contacts/") else r
        with self.assertRaises(s.SemanticFailure): client.write(NONCE)
        self.assertTrue(client._failed)
        self.assertEqual(world.calls[-1][1], "/api/chatbot/settings")
        count = len(world.calls)
        with self.assertRaises(s.proof.EvidenceError): client.write(NONCE)
        self.assertEqual(len(world.calls), count)

    def test_record_foreign_binding_and_path_injection_refuse(self):
        world = World()
        record = world.client().write(NONCE)
        for bad in (replace(record, fixture_binding="0" * 64), replace(record, contact_id="../secret"),
                    replace(record, nonce="invalid")):
            client = world.client()
            before = len(world.calls)
            with self.assertRaises(s.proof.EvidenceError):
                client.read(bad)
            self.assertEqual(len(world.calls), before)

    def test_duplicate_or_foreign_conversation_and_messages_refuse(self):
        for case in ("duplicate-conversation", "foreign-account", "duplicate-message"):
            with self.subTest(case=case):
                world = World()
                record = world.client().write(NONCE)
                if case == "duplicate-conversation":
                    world.conversations["duplicate"] = dict(world.conversations[record.conversation_id])
                elif case == "foreign-account":
                    world.conversations[record.conversation_id]["channel_account"]["external_account_id"] = "legacy-account:" + ORG
                elif case == "duplicate-message":
                    world.messages[record.conversation_id].append(world.messages[record.conversation_id][0])
                with self.assertRaises(s.SemanticFailure):
                    world.client().read(record)

    def test_mirror_and_message_tenant_binding_is_not_inferred_from_body(self):
        for case in ("identity", "message-tenant", "message-contact", "message-conversation", "account-capability"):
            with self.subTest(case=case):
                world = World()
                record = world.client().write(NONCE)
                if case == "identity":
                    world.conversations[record.conversation_id]["contact_identity"]["organization_id"] = ACCOUNT
                elif case == "account-capability":
                    world.conversations[record.conversation_id]["channel_account"]["capabilities"]["legacy_text_reply_endpoint"] = False
                else:
                    key = {"message-tenant": "organization_id", "message-contact": "contact_id", "message-conversation": "inbox_conversation_id"}[case]
                    world.messages[record.conversation_id][0]["message"][key] = ACCOUNT
                with self.assertRaises(s.SemanticFailure):
                    world.client().read(record)

    def test_ambiguous_post_never_retried_and_instance_poisoned(self):
        world = World()
        client = world.client()
        def fail(method, path, body):
            if path == "/api/contacts":
                raise OSError("password-host-secret")
        world.intercept = fail
        with self.assertRaises(s.proof.InfrastructureFailure) as caught:
            client.write(NONCE)
        self.assertNotIn("secret", str(caught.exception))
        with self.assertRaises(s.proof.EvidenceError):
            client.write(NONCE)
        self.assertEqual(sum(path == "/api/contacts" for _, path, *_ in world.calls), 1)

    def test_duplicate_write_nonce_refuses_before_mutation(self):
        world = World()
        client = world.client()
        client.write(NONCE)
        before = len(world.calls)
        with self.assertRaises(s.proof.EvidenceError):
            client.write(NONCE)
        self.assertEqual(len(world.calls), before)

    def test_missing_and_duplicate_inbound_journal_and_wrong_send_refuse(self):
        for case in ("missing", "duplicate", "wrong-send"):
            world = World()
            client = world.client()
            def mutate(method, path, result):
                if path.startswith("/_control/journal?"):
                    result = json.loads(json.dumps(result))
                    entries = result["entries"]
                    if case == "missing":
                        for e in entries:
                            if e.get("kind") == "webhook": e["message_id"] = "wrong"
                    if case == "duplicate" and entries:
                        e = dict(entries[-1]); e["seq"] += 1
                        entries.append(e); result["latest"] += 1
                    if case == "wrong-send":
                        for e in entries:
                            if e.get("kind") == "graph": e["text_sha256"] = "0" * 64
                return result
            world.mutate = mutate
            with self.subTest(case=case), self.assertRaises(s.proof.EvidenceError):
                client.write(NONCE)

    def test_journal_pagination_continuity_and_reset_refusal(self):
        world = World()
        client = world.client()
        client._begin()
        world.pages = [{"entries": [{"seq": 8}], "latest": 9}, {"entries": [{"seq": 9}], "latest": 9}]
        self.assertEqual(client._journal(7)[1], 9)
        for bad in ({"entries": [{"seq": 9}], "latest": 9}, {"entries": [], "latest": 8},
                    {"entries": [], "latest": 0}, {"entries": [{"seq": True}], "latest": 8}):
            world.pages = [bad]
            with self.assertRaises(s.proof.EvidenceError):
                client._journal(7)

    def test_truncated_journal_is_baseline_only_not_measured_evidence(self):
        world = World()
        client = world.client()
        client._begin()
        retained = {"entries": [{"seq": 8}, {"seq": 9}], "latest": 9}
        world.pages = [retained]
        self.assertEqual(client._journal(0, discard_baseline=True)[1], 9)
        world.pages = [retained]
        with self.assertRaises(s.proof.EvidenceError):
            client._journal(0)
        world.pages = [{"entries": [{"seq": 8}, {"seq": 10}], "latest": 10}]
        with self.assertRaises(s.proof.EvidenceError):
            client._journal(0, discard_baseline=True)
        with self.assertRaises(s.proof.EvidenceError):
            client._journal(7, discard_baseline=True)
        world.pages = [{"entries": [{"seq": 1}, {"seq": 2}], "latest": 2}]
        self.assertEqual(client._journal(0)[1], 2)

    def test_expired_service_window_prevents_outbound_post(self):
        world = World()
        client = world.client()
        def mutate(method, path, result):
            if path.startswith("/api/conversations?"):
                for row in result["data"]["conversations"]:
                    row["service_window_ends_at"] = "2000-01-01T00:00:00Z"
            return result
        world.mutate = mutate
        with self.assertRaises(s.SemanticFailure):
            client.write(NONCE)
        self.assertFalse(any(path.endswith("legacy-whatsapp-replies") for _, path, *_ in world.calls))

    def test_response_limits_redirects_duplicate_json_and_redacted_errors(self):
        replies = [s.Reply(302, (("Location", "https://secret.example/"),), b"secret"),
                   s.Reply(500, (), b"password secret"), s.Reply(200, (), b"x" * (s.MAX_BODY + 1)),
                   s.Reply(200, (), b'{"data":{},"data":{"password":"secret"}}'), s.Reply(200, (), b"invalid secret"),
                   s.Reply(200, (), b'{"data":{"ignored":NaN}}')]
        for reply in replies:
            world = World()
            world.intercept = lambda m, p, b: reply
            with self.assertRaises(s.proof.EvidenceError) as caught:
                world.client()
            self.assertNotIn("secret", str(caught.exception))
            self.assertEqual(len(world.calls), 1)

    def test_get_polling_is_bounded_without_mutation_retry(self):
        world = World()
        client = world.client()
        world.mutate = lambda m, p, r: {"data": {"conversations": []}} if p.startswith("/api/conversations?") else r
        with self.assertRaises(s.SemanticFailure):
            client.write(NONCE)
        self.assertEqual(sum(path.startswith("/api/conversations?") for _, path, *_ in world.calls), 40)
        self.assertEqual(sum(path == "/_control/inbound" for _, path, *_ in world.calls), 1)

    def test_total_deadline_prevents_a_later_mutation(self):
        world = World()
        now = [0]
        client = s.Sweep(world.f, product=world.transport(), stub=world.transport(True), clock=lambda: now[0], wall=lambda: WALL)
        client.login()
        def advance(method, path, result):
            if path == "/api/chatbot/settings": now[0] = 181
            return result
        world.mutate = advance
        with self.assertRaises(s.proof.InfrastructureFailure):
            client.write(NONCE)
        self.assertFalse(any(path == "/api/contacts" for _, path, *_ in world.calls))

    def test_http_transport_no_redirect_follow_or_ambient_proxy(self):
        calls = []
        class Response:
            status = 302
            def getheaders(self): return [("Location", "https://secret.example/")]
            def read(self, size):
                self.size = size
                return b"private"
        class Connection:
            sock = None
            def __init__(self, host, port, timeout): calls.append((host, port))
            def connect(self): pass
            def request(self, *args, **kwargs): calls.append(args)
            def getresponse(self): return Response()
            def close(self): pass
        with patch.object(s.http.client, "HTTPConnection", Connection):
            result = s.HTTP(fixture().product_origin, NS, "web").request("GET", "/api/me", {}, b"", 1)
        self.assertEqual(result.status, 302)
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], ("127.0.0.1", 8080))

    def test_http_wall_deadline_poison_and_late_dns_sends_no_request(self):
        calls = []
        class Connection:
            sock = None
            def __init__(self, *args, **kwargs): pass
            def connect(self): time.sleep(0.03)
            def request(self, *args, **kwargs): calls.append("sent")
            def close(self): pass
        with patch.object(s.http.client, "HTTPConnection", Connection):
            transport = s.HTTP(fixture().product_origin, NS, "web")
            with self.assertRaises(s.proof.InfrastructureFailure):
                transport.request("POST", "/api/contacts", {}, b"{}", 0.005)
            with self.assertRaises(s.proof.EvidenceError):
                transport.request("POST", "/api/contacts", {}, b"{}", 1)
            time.sleep(0.04)
        self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
