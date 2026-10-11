"""Synthetic six-case API adapter, not provisioning or a compatibility runner.

Fixtures and RecordSet values are private, in-memory run data. The caller owns
the isolated stack, synthetic accounts/entitlements/legacy-reply allowlist,
stable configuration across versions, and process/worker/SQL evidence. No I/O
occurs at import. No POST/PUT is retried, even after an ambiguous transport error.
Completed retained reads preserve bounded value mismatches as false cases.
Identity, runtime safety and incomplete evidence still raise; an unrun case is
never manufactured for the future semantic-blocked runner contract.
"""
from __future__ import annotations

from dataclasses import dataclass
import datetime
import hashlib
import hmac
import http.client
from http.cookies import SimpleCookie
import json
from pathlib import Path
import re
import secrets
import socket
import sys
import threading
import time
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof

MAX_BODY = 262_144
_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\Z")
_NS = re.compile(r"rereply-(?:compat|local)-[0-9a-f]{12}\Z")
_DIGITS = re.compile(r"999[0-9]{12}\Z")
_NONCE = re.compile(r"[0-9a-f]{32}\Z")


def _need(ok: bool, code: str = "sweep-input-invalid") -> None:
    if not ok:
        raise proof.EvidenceError(code)


class SemanticFailure(proof.EvidenceError):
    """A fixed-code API/retained-value mismatch; never a complete proof result."""


def _same(ok: bool) -> None:
    if not ok:
        raise SemanticFailure("sweep-value-differs")


def _obj(value):
    _need(type(value) is dict, "sweep-response-invalid")
    return value


def _text_equal(value, expected):
    # Wrong/missing types are incomplete response evidence, not a completed
    # value comparison. Neither private value enters errors or public output.
    _need(type(value) is str and len(value) <= 8192, "sweep-response-invalid")
    return value == expected


def _text_fields(value, expected):
    # Eager validation: an early false must not hide a later malformed field.
    comparisons = [_text_equal(value.get(key), wanted) for key, wanted in expected.items()]
    return all(comparisons)


def _uuid(value):
    _need(type(value) is str and bool(_UUID.fullmatch(value)), "sweep-identity-invalid")
    return value


def _json(value) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def _parse(raw):
    def constant(_):
        raise ValueError
    def pairs(items):
        result = {}
        for key, value in items:
            _need(key not in result, "sweep-response-invalid")
            result[key] = value
        return result
    try:
        return _obj(json.loads(raw, object_pairs_hook=pairs, parse_constant=constant))
    except (ValueError, UnicodeError, RecursionError):
        raise proof.EvidenceError("sweep-response-invalid") from None


def synthetic_origin(value: str, namespace: str, role: str) -> str:
    """Only explicit loopback or this run's exact Docker DNS alias; no proxy."""
    _need(type(value) is str and type(namespace) is str and bool(_NS.fullmatch(namespace)))
    _need(role in {"web", "graph"})
    try:
        url = urlsplit(value)
        _need(url.scheme == "http" and not url.username and not url.password and not url.path
              and not url.query and not url.fragment and url.hostname in {"127.0.0.1", "::1", namespace + "-" + role}
              and url.port is not None and 1024 <= url.port <= 65535)
        host = "[::1]" if url.hostname == "::1" else url.hostname
        _need(value == f"http://{host}:{url.port}")
    except ValueError:
        raise proof.EvidenceError("sweep-input-invalid") from None
    return value


@dataclass(frozen=True, repr=False)
class Fixture:
    namespace: str
    product_origin: str
    stub_origin: str
    organization_id: str
    account_id: str
    channel_account_id: str
    account_name: str
    phone_number_id: str
    email: str
    password: str
    control_key: str

    def __post_init__(self):
        synthetic_origin(self.product_origin, self.namespace, "web")
        synthetic_origin(self.stub_origin, self.namespace, "graph")
        _need(self.product_origin != self.stub_origin)
        for value in (self.organization_id, self.account_id, self.channel_account_id):
            _uuid(value)
        _need(self.account_name == self.namespace + "-wa")
        _need(type(self.phone_number_id) is str and bool(_DIGITS.fullmatch(self.phone_number_id)))
        _need(self.email == self.namespace + "-client@example.test")
        for value in (self.password, self.control_key):
            _need(type(value) is str and bool(re.fullmatch(r"[0-9a-f]{48,128}", value)))


@dataclass(frozen=True, repr=False)
class Reply:
    status: int
    headers: tuple[tuple[str, str], ...]
    body: bytes


class HTTP:
    """Fixed-origin HTTP/1 transport with no redirects/proxies or ambient auth.

    A daemon performs one bounded request. A wall deadline closes/shuts down the
    owned socket and poisons the transport: no follow-up writes after ambiguity.
    OS DNS lookup itself is not cancellable; internal-alias resolution may finish
    later, but the cancellation check precedes sending HTTP bytes. Stack/network
    ownership and DNS isolation remain the caller's responsibility.
    """
    def __init__(self, origin: str, namespace: str, role: str):
        self.origin = synthetic_origin(origin, namespace, role)
        self._url = urlsplit(origin)
        self._poisoned = False

    def request(self, method, path, headers, body, timeout) -> Reply:
        _need(not self._poisoned and method in {"GET", "POST", "PUT"})
        _need(type(path) is str and path.startswith("/") and not path.startswith("//")
              and not any(c in path for c in "\r\n#") and len(path) <= 1024)
        _need(type(body) is bytes and len(body) <= MAX_BODY and 0 < timeout <= 45)
        connection = http.client.HTTPConnection(self._url.hostname, self._url.port, timeout=timeout)
        cancelled = threading.Event()
        done = threading.Event()
        result = []
        owned_socket = []

        def capture():
            try:
                connection.connect()
                connection.auto_open = 0  # A concurrent deadline-close must never reconnect a POST.
                owned_socket.append(connection.sock)
                if cancelled.is_set():
                    return
                connection.request(method, path, body=body or None, headers=headers)
                response = connection.getresponse()
                pairs = tuple(response.getheaders())
                if sum(len(k) + len(v) for k, v in pairs) > 32_768:
                    return
                raw = response.read(MAX_BODY + 1)
                if len(raw) <= MAX_BODY:
                    result.append(Reply(response.status, pairs, raw))
            except Exception:
                pass  # Never retain exception text, headers, URL or credentials.
            finally:
                connection.close()
                done.set()

        thread = threading.Thread(target=capture, daemon=True)
        try:
            thread.start()
        except Exception:
            self._poisoned = True
            connection.close()
            raise proof.InfrastructureFailure("sweep-http-incomplete") from None
        if not done.wait(timeout) or len(result) != 1:
            self._poisoned = True
            cancelled.set()
            for stream in owned_socket:
                try:
                    stream.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            connection.close()
            raise proof.InfrastructureFailure("sweep-http-incomplete")
        return result[0]


@dataclass(frozen=True, repr=False)
class RecordSet:
    """Retain privately across server replacement; never a public artifact.

    Expected projections are generated from the fixture/nonce, not copied from
    server responses. Only validated newly observed UUIDs enter this record.
    """
    fixture_binding: str
    nonce: str
    contact_id: str
    conversation_id: str
    inbound_id: str
    outbound_id: str
    template_id: str
    flow_id: str


class Sweep:
    def __init__(self, fixture: Fixture, *, product=None, stub=None, clock=time.monotonic,
                 wall=time.time, sleep=time.sleep):
        _need(type(fixture) is Fixture)
        self._f = fixture
        self._product = product if product is not None else HTTP(fixture.product_origin, fixture.namespace, "web")
        self._stub = stub if stub is not None else HTTP(fixture.stub_origin, fixture.namespace, "graph")
        # Injected transports implement the same private fixed-origin capability.
        _need(self._product.origin == fixture.product_origin and self._stub.origin == fixture.stub_origin)
        self._clock, self._wall, self._sleep = clock, wall, sleep
        self._cookies = {}
        self._logged_in = False
        self._failed = False
        self._nonces = set()
        self._deadline = 0.0
        self._requests = 0
        self._stub_calls = []
        self._stub_windows = []

    def _begin(self):
        _need(not self._failed, "sweep-instance-failed")
        self._deadline, self._requests = self._clock() + 180, 0

    def _call(self, method, path, data=None, *, stub=False):
        self._requests += 1
        left = self._deadline - self._clock()
        if left <= 0 or self._requests > 256:
            raise proof.InfrastructureFailure("sweep-budget-exhausted")
        body = b"" if data is None else _json(data)
        _need(len(body) <= MAX_BODY)
        headers = {"Content-Type": "application/json", "Accept": "application/json"}
        if stub:
            _need(path.startswith("/_control/"))
            timestamp, nonce = str(int(self._wall())), secrets.token_hex(24)
            signature = "|".join((method, path, timestamp, nonce, hashlib.sha256(body).hexdigest()))
            headers.update({"X-Stub-Timestamp": timestamp, "X-Stub-Nonce": nonce,
                            "X-Stub-Mac": hmac.new(self._f.control_key.encode(), signature.encode(), hashlib.sha256).hexdigest()})
        else:
            _need(path.startswith("/api/"))
            headers["X-Organization-ID"] = self._f.organization_id
            if self._cookies:
                headers["Cookie"] = "; ".join(k + "=" + v for k, v in sorted(self._cookies.items()))
                headers["X-CSRF-Token"] = self._cookies["whm_csrf"]
        try:
            reply = (self._stub if stub else self._product).request(method, path, headers, body, min(45, left))
        except Exception:
            raise proof.InfrastructureFailure("sweep-http-incomplete") from None
        _need(type(reply) is Reply and type(reply.status) is int and 100 <= reply.status <= 599
              and type(reply.body) is bytes and len(reply.body) <= MAX_BODY
              and type(reply.headers) is tuple and len(reply.headers) <= 100
              and all(type(v) is tuple and len(v) == 2 and all(type(x) is str for x in v) for v in reply.headers),
              "sweep-response-invalid")
        if 300 <= reply.status < 400:
            raise proof.InfrastructureFailure("sweep-redirect-refused")
        if reply.status < 200 or reply.status >= 300:
            raise SemanticFailure("sweep-http-status")
        if not stub:
            for key, value in reply.headers:
                if key.lower() == "set-cookie":
                    try:
                        parsed = SimpleCookie()
                        parsed.load(value)
                        for name, morsel in parsed.items():
                            _need(name in {"whm_access", "whm_refresh", "whm_csrf"})
                            _need(bool(re.fullmatch(r"[A-Za-z0-9_.~-]{1,8192}", morsel.value)))
                            # This adapter never refreshes and must not send the
                            # narrow /api/auth/refresh cookie to other API routes.
                            if name != "whm_refresh":
                                self._cookies[name] = morsel.value
                    except Exception:
                        raise proof.EvidenceError("sweep-cookie-invalid") from None
        value = _parse(reply.body)
        if stub:
            # Private completion receipts, not inferred from later journal
            # contents. Never retain headers, MACs, tokens or request bodies.
            route = path.split('?', 1)[0].removeprefix('/_control/')
            _need(route in {'accounts', 'inbound', 'journal', 'rejected'}
                  and len(self._stub_calls) < 256, 'sweep-receipt-invalid')
            self._stub_calls.append({'method': method, 'route': route, 'status': reply.status})
            return value
        _need("data" in value, "sweep-response-invalid")
        return _obj(value["data"])

    def login(self):
        self._begin()
        try:
            _need(not self._logged_in)
            self._call("POST", "/api/auth/login", {"email": self._f.email, "password": self._f.password})
            _need({"whm_access", "whm_csrf"} <= self._cookies.keys(), "sweep-cookie-missing")
            me = self._call("GET", "/api/me")
            _same(me.get("organization_id") == self._f.organization_id and me.get("is_super_admin") is False
                  and me.get("email") == self._f.email)
            self._logged_in = True
        except Exception:
            self._failed = True
            raise

    def _binding(self):
        return hashlib.sha256(_json([self._f.namespace, self._f.organization_id, self._f.account_id,
                                     self._f.channel_account_id, self._f.account_name, self._f.phone_number_id])).hexdigest()

    def _expected(self, nonce):
        _need(type(nonce) is str and bool(_NONCE.fullmatch(nonce)))
        label = self._f.namespace + "-" + nonce
        return {"phone": "999" + str(int(nonce, 16) % 10**12).zfill(12), "name": label,
                "inbound": label + " inbound", "outbound": label + " outbound",
                "wamid": "wamid.local." + nonce, "template": "compat_" + nonce,
                "flow": label, "settings": label + " greeting"}

    @staticmethod
    def _graph():
        return {"version": 2, "entry_node": "start", "nodes": [{"id": "start", "type": "start"},
                {"id": "end", "type": "end"}], "edges": [{"from": "start", "to": "end", "condition": "default"}]}

    def stub_receipts(self):
        """Detached private receipts across login/write/read budget resets."""
        _need(not self._failed, 'sweep-instance-failed')
        return {'calls': [dict(v) for v in self._stub_calls],
                'windows': [dict(v) for v in self._stub_windows]}

    def _journal(self, after, *, discard_baseline=False):
        return self._ring('journal', after, discard_baseline=discard_baseline)

    def _rejected(self, after):
        return self._ring('rejected', after, discard_baseline=False)

    def _ring(self, ring, after, *, discard_baseline):
        # A cursor-discard baseline may start at the ring's first retained row.
        # Measured windows require every sequence, including when after is zero.
        _need(ring in {'journal', 'rejected'} and type(after) is int and 0 <= after < 2**64
              and type(discard_baseline) is bool and (not discard_baseline or (after == 0 and ring == 'journal')))
        rows, cursor = [], after
        for page_number in range(1, 21):
            page = self._call("GET", f"/_control/{ring}?after={cursor}&limit=1000", stub=True)
            entries, latest = page.get("entries"), page.get("latest")
            _need(set(page) == {'entries', 'latest'} and type(entries) is list and len(entries) <= 1000
                  and type(latest) is int and cursor <= latest < 2**64,
                  "sweep-journal-invalid")
            _need(bool(entries) or latest == cursor, "sweep-journal-gap")
            for entry in entries:
                entry = _obj(entry)
                seq = entry.get("seq")
                _need(type(seq) is int and cursor < seq <= latest
                      and (seq == cursor + 1 or (discard_baseline and cursor == 0 and not rows)),
                      "sweep-journal-gap")
                rows.append(entry)
                cursor = seq
            if cursor == latest:
                _need(len(self._stub_windows) < 8, 'sweep-receipt-invalid')
                self._stub_windows.append({'ring': ring, 'after': after, 'cursor': cursor,
                    'discard_baseline': discard_baseline, 'requests': page_number})
                return rows, cursor
        raise proof.InfrastructureFailure("sweep-journal-unsettled")

    def _conversation(self, expected, contact_id, conversation_id=None, *, wait=False, retained_values=False):
        for attempt in range(40 if wait else 1):
            values = self._call("GET", "/api/conversations?limit=100&search=" + expected["phone"]).get("conversations")
            _need(type(values) is list and len(values) <= 100, "sweep-response-invalid")
            matches = [v for v in values if type(v) is dict and v.get("contact_id") == contact_id]
            if matches:
                _same(len(matches) == 1)
                value = matches[0]
                ident = _uuid(value.get("id"))
                _same(conversation_id is None or ident == conversation_id)
                _same(value.get("organization_id") == self._f.organization_id
                      and value.get("channel_account_id") == self._f.channel_account_id
                      and value.get("channel") == "whatsapp"
                      and value.get("external_conversation_id") == "legacy-contact:" + contact_id)
                contact, account = _obj(value.get("contact")), _obj(value.get("channel_account"))
                _same(contact.get("id") == contact_id and contact.get("phone_number") == expected["phone"]
                      and contact.get("whatsapp_account") == self._f.account_name
                      and contact.get("organization_id") == self._f.organization_id)
                _same(account.get("id") == self._f.channel_account_id and account.get("organization_id") == self._f.organization_id
                      and account.get("channel") == "whatsapp" and account.get("has_credentials") is False
                      and account.get("provider") == "meta_legacy" and account.get("status") == "active"
                      and account.get("external_account_id") == "legacy-account:" + self._f.account_id
                      and _obj(account.get("capabilities")).get("legacy_text_reply_endpoint") is True)
                caps, config, identity = _obj(account.get("capabilities")), _obj(account.get("config")), _obj(value.get("contact_identity"))
                _same(all(caps.get(k) is True for k in ("text", "replies", "service_window"))
                      and config.get("legacy_read_only") is True and config.get("outbound_enabled") is False
                      and config.get("reply_route") == "chat")
                _same(all(identity.get(k) == v for k, v in {"organization_id": self._f.organization_id,
                      "contact_id": contact_id, "channel_account_id": self._f.channel_account_id, "channel": "whatsapp",
                      "external_id": "legacy-contact:" + contact_id, "address": expected["phone"],
                      "normalized_address": expected["phone"]}.items())
                      and identity.get("is_primary") is True and identity.get("is_verified") is True)
                names = [_text_equal(contact.get("profile_name"), expected["name"]),
                         _text_equal(identity.get("display_name"), expected["name"])]
                if not retained_values:
                    _same(all(names))
                return value
            if wait and attempt < 39:
                self._sleep(0.25)
        raise SemanticFailure("sweep-conversation-missing")

    def _message(self, conversation, contact_id, text, direction, *, wamid=None, retained=None, wait=False):
        for attempt in range(40 if wait else 1):
            rows = self._call("GET", f"/api/conversations/{conversation}/messages?limit=100").get("messages")
            _need(type(rows) is list and len(rows) <= 100, "sweep-response-invalid")
            messages = [_obj(v).get("message", v) for v in rows]
            matches = [v for v in messages if type(v) is dict and v.get("content") == text and v.get("direction") == direction]
            if matches:
                _same(len(matches) == 1)
                message = matches[0]
                ident = _uuid(message.get("id"))
                _same(message.get("message_type") == "text" and message.get("organization_id") == self._f.organization_id
                      and message.get("contact_id") == contact_id and message.get("whatsapp_account") == self._f.account_name
                      and message.get("inbox_conversation_id") == conversation
                      and (wamid is None or message.get("whatsapp_message_id") == wamid)
                      and (retained is None or ident == retained))
                return ident
            if wait and attempt < 39:
                self._sleep(0.25)
        raise SemanticFailure("sweep-message-missing")

    def _settings(self):
        value = _obj(self._call("GET", "/api/chatbot/settings").get("settings"))
        _same(value.get("enabled") is False and value.get("ai_enabled") is False)
        return value

    def _retained_message(self, record, ident, text, direction, *, wamid=None):
        response = self._call("GET", f"/api/conversations/{record.conversation_id}/messages?limit=100")
        rows = response.get("messages")
        _need(set(response) == {"messages", "page", "limit", "total"}
              and type(rows) is list and len(rows) <= 100
              and type(response.get("page")) is int and response["page"] == 1
              and type(response.get("limit")) is int and response["limit"] == 100
              and type(response.get("total")) is int and response["total"] == len(rows),
              "sweep-message-page-incomplete")
        messages = []
        seen = set()
        for row in rows:
            row = _obj(row)
            message = _obj(row.get("message", row))
            key = _uuid(message.get("id"))
            _same(key not in seen)
            seen.add(key)
            _same(message.get("organization_id") == self._f.organization_id
                  and message.get("contact_id") == record.contact_id
                  and message.get("whatsapp_account") == self._f.account_name
                  and message.get("inbox_conversation_id") == record.conversation_id)
            messages.append(message)
        # Select by the retained identity, not by the expected content: changed
        # text is observable. Absence is false only after complete enumeration.
        matches = [message for message in messages if message["id"] == ident]
        if not matches:
            return False
        message = matches[0]
        _same(message.get("direction") == direction and message.get("message_type") == "text"
              and (wamid is None or message.get("whatsapp_message_id") == wamid))
        return _text_equal(message.get("content"), text)

    def write(self, nonce: str) -> RecordSet:
        self._begin()
        try:
            _need(self._logged_in and nonce not in self._nonces)
            expected = self._expected(nonce)
            self._nonces.add(nonce)  # Consume before the first possible mutation.
            self._settings()  # Do not enable or erase any preprovisioned setting.
            contact = self._call("POST", "/api/contacts", {"phone_number": expected["phone"],
                "profile_name": expected["name"], "whatsapp_account": self._f.account_name})
            contact_id = _uuid(contact.get("id"))
            _, cursor = self._journal(0, discard_baseline=True)
            delivered = self._call("POST", "/_control/inbound", {"phone_number_id": self._f.phone_number_id,
                "from": expected["phone"], "profile_name": expected["name"], "type": "text",
                "text": expected["inbound"], "message_id": expected["wamid"], "timestamp": int(self._wall())}, stub=True)
            _same(delivered.get("callback_status") == 200)
            entries, _ = self._journal(cursor)
            matching = [v for v in entries if v.get("kind") == "webhook" and v.get("message_id") == expected["wamid"]]
            _same(len(matching) == 1 and matching[0].get("status") == 200
                  and matching[0].get("phone_number_id") == self._f.phone_number_id)
            conversation = self._conversation(expected, contact_id, wait=True)
            conversation_id = conversation["id"]
            inbound = self._message(conversation_id, contact_id, expected["inbound"], "incoming", wamid=expected["wamid"], wait=True)
            try:
                window = datetime.datetime.fromisoformat(conversation["service_window_ends_at"].replace("Z", "+00:00"))
                _same(window.tzinfo is not None and window.timestamp() > self._wall())
            except (ValueError, TypeError, KeyError, AttributeError):
                raise SemanticFailure("sweep-service-window-invalid") from None
            _, cursor = self._journal(0, discard_baseline=True)
            self._call("POST", f"/api/conversations/{conversation_id}/legacy-whatsapp-replies",
                       {"idempotency_key": nonce, "type": "text", "content": {"body": expected["outbound"]}})
            outbound = self._message(conversation_id, contact_id, expected["outbound"], "outgoing", wait=True)
            entries, _ = self._journal(cursor)
            sends = [v for v in entries if v.get("kind") == "graph" and v.get("route") == "messages" and v.get("method") == "POST"]
            _same(len(sends) == 1 and sends[0].get("status") == 200 and sends[0].get("message_type") == "text"
                  and sends[0].get("phone_number_id") == self._f.phone_number_id and sends[0].get("to") == expected["phone"]
                  and sends[0].get("text_sha256") == hashlib.sha256(expected["outbound"].encode()).hexdigest())
            template = self._call("POST", "/api/templates", {"whatsapp_account": self._f.account_name,
                "name": expected["template"], "language": "en_US", "category": "UTILITY", "header_type": "NONE",
                "body_content": expected["name"]})
            # The enabled graph is inert (start -> end, no actions); the global
            # chatbot remains disabled. Explicit true also avoids GORM's true
            # default silently replacing a false zero value on create.
            flow = self._call("POST", "/api/chatbot/flows", {"name": expected["flow"], "description": expected["name"],
                "trigger_keywords": [expected["template"]], "on_complete_action": "none", "enabled": True, "graph": self._graph()})
            self._call("PUT", "/api/chatbot/settings", {"greeting_message": expected["settings"]})
            record = RecordSet(self._binding(), nonce, contact_id, conversation_id, inbound, outbound,
                               _uuid(template.get("id")), _uuid(flow.get("id")))
            # A write cannot discard an early readback mismatch and later return
            # a favorable RecordSet. Keep this precheck strict and same-budget.
            _same(all(self._read(record).values()))
            return record
        except Exception:
            self._failed = True
            raise

    def _read(self, record):
        _need(self._logged_in and type(record) is RecordSet and record.fixture_binding == self._binding())
        for value in (record.contact_id, record.conversation_id, record.inbound_id, record.outbound_id, record.template_id, record.flow_id):
            _uuid(value)
        expected = self._expected(record.nonce)
        contact = self._call("GET", "/api/contacts/" + record.contact_id)
        _same(contact.get("id") == record.contact_id and contact.get("phone_number") == expected["phone"]
              and contact.get("whatsapp_account") == self._f.account_name)
        contact_name = _text_equal(contact.get("profile_name"), expected["name"])
        conversation = self._conversation(expected, record.contact_id, record.conversation_id, retained_values=True)
        names = [_text_equal(conversation["contact"]["profile_name"], expected["name"]),
                 _text_equal(conversation["contact_identity"]["display_name"], expected["name"])]
        inbound = self._retained_message(record, record.inbound_id, expected["inbound"], "incoming", wamid=expected["wamid"])
        outbound = self._retained_message(record, record.outbound_id, expected["outbound"], "outgoing")
        template = self._call("GET", "/api/templates/" + record.template_id)
        _same(template.get("id") == record.template_id and template.get("whatsapp_account") == self._f.account_name
              and template.get("status") == "DRAFT")
        template_values = _text_fields(template, {"name": expected["template"], "language": "en_US", "category": "UTILITY",
                                                 "header_type": "NONE", "body_content": expected["name"]})
        flow = self._call("GET", "/api/chatbot/flows/" + record.flow_id)
        _same(all(flow.get(k) == v for k, v in {"id": record.flow_id, "organization_id": self._f.organization_id,
              "on_complete_action": "none", "is_enabled": True, "graph": self._graph()}.items()))
        flow_values = _text_fields(flow, {"name": expected["flow"], "description": expected["name"]})
        keywords = flow.get("trigger_keywords")
        _need(type(keywords) is list and len(keywords) <= 100, "sweep-response-invalid")
        for keyword in keywords:
            _text_equal(keyword, "")  # Validate every item; equality is below.
        settings = _text_equal(self._settings().get("greeting_message"), expected["settings"])
        # No partial result escapes if any later GET, identity or safety check
        # failed. All six booleans describe completed private observations.
        return {"contacts": contact_name and all(names), "inbound": inbound, "outbound": outbound,
                "template": template_values, "flow": flow_values and keywords == [expected["template"]],
                "settings": settings}

    def read(self, record: RecordSet) -> dict[str, bool]:
        self._begin()
        try:
            return self._read(record)
        except Exception:
            self._failed = True
            raise
