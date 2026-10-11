"""Private controller protocol tests; fake sweeps/HTTP only, no network/DB."""
from dataclasses import asdict, replace
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import controller as c
import test_sweep

BUSINESS = "999987654321098"


def fixture():
    value = test_sweep.fixture()
    return replace(value, product_origin=f"http://{value.namespace}-web:8080",
                   stub_origin=f"http://{value.namespace}-graph:8090")


def request(operation, **extra):
    return {"version": 1, "operation": operation, "fixture": asdict(fixture()), **extra}


def raw(value):
    return json.dumps(value).encode()


def receipts(operation, window=None):
    """Synthetic completion receipts for fake-controller protocol tests only."""
    windows, calls = [], []
    if operation == 'setup':
        calls = [{'method':m,'route':'accounts','status':200} for m in ('POST','GET')]
    elif operation in {'journal','rejected'}:
        windows = [{'ring':operation, **{k:window[k] for k in ('after','cursor','discard_baseline','requests')}}]
        calls = [{'method':'GET','route':operation,'status':200} for _ in range(window['requests'])]
    elif operation == 'write':
        windows = [{'ring':'journal','after':0,'cursor':0,'discard_baseline':i in (0,2),'requests':1} for i in range(4)]
        calls = [{'method':'GET','route':'journal','status':200}, {'method':'POST','route':'inbound','status':200}]
        calls.extend({'method':'GET','route':'journal','status':200} for _ in range(3))
    return {'calls':calls,'windows':windows}


def record():
    return c.sweep.RecordSet(c.sweep.Sweep(fixture())._binding(), test_sweep.NONCE,
                            *(f"20000000-0000-4000-8000-{n:012d}" for n in range(1, 7)))


class FakeSweep:
    def __init__(self):
        self.calls = []
        self.flags = {key: True for key in c.sweep.proof.SWEEP_CASES}
        self.retained = record()
        self.failure = None

    def factory(self, value):
        self.calls.append(("fixture", value))
        return self

    def login(self):
        self.calls.append(("login",))
        if self.failure:
            raise self.failure

    def write(self, nonce):
        self.calls.append(("write", nonce))
        return self.retained

    def stub_receipts(self):
        return receipts('write' if any(v[0] == 'write' for v in self.calls) else 'read')

    def read(self, retained):
        self.calls.append(("read", retained))
        return self.flags


class SetupWorld(test_sweep.World):
    """Real Sweep transport with independently checked HMAC and Graph shape."""
    def __init__(self):
        super().__init__()
        self.f = fixture()
        self.accounts = []

    def product(self, method, path, value):
        raise AssertionError("setup must not contact product")

    def stub(self, method, path, value):
        assert path == "/_control/accounts"
        if method == "POST":
            assert value == {"business_account_id": BUSINESS, "phone_number_id": self.f.phone_number_id,
                             "display_phone_number": self.f.phone_number_id, "verified_name": self.f.namespace}
            self.accounts = [dict(value)]
            return {"ok": True}
        assert method == "GET" and value is None
        return {"accounts": self.accounts}

    def factory(self, value):
        return c.sweep.Sweep(value, product=self.transport(), stub=self.transport(True),
                             wall=lambda: test_sweep.WALL, sleep=lambda _: None)


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.fake = FakeSweep()

    def handle(self, value):
        return json.loads(c.handle(raw(value), sweep_factory=self.fake.factory))

    def test_pure_preflight_performs_no_sweep_or_http_operations(self):
        values = [request("setup", business_account_id=BUSINESS), request("readiness"), request("write", nonce=test_sweep.NONCE),
                  request("read", record=asdict(record()))]
        with patch.object(c.sweep.Sweep, "login", side_effect=AssertionError("no login")), \
                patch.object(c.sweep.Sweep, "write", side_effect=AssertionError("no write")), \
                patch.object(c.sweep.Sweep, "read", side_effect=AssertionError("no read")), \
                patch.object(c.sweep.HTTP, "request", side_effect=AssertionError("no HTTP")):
            for value in values:
                self.assertEqual(c.parse_request(raw(value)), value)
            bad = request("readiness")
            bad["fixture"]["product_origin"] = "https://example.com"
            with self.assertRaisesRegex(c.sweep.proof.EvidenceError, "controller-input-invalid"):
                c.parse_request(raw(bad))

    def test_write_returns_private_record_and_actual_read_flags(self):
        self.fake.flags["flow"] = False
        value = self.handle(request("write", nonce=test_sweep.NONCE))
        self.assertEqual(value, {"version": 1, "status": "ok", "operation": "write",
                                "record": asdict(record()), "cases": self.fake.flags,
                                'stub_receipts':receipts('write')})
        self.assertEqual([x[0] for x in self.fake.calls], ["fixture", "login", "write", "read"])
        self.assertNotIn("password", json.dumps(value))

    def test_journal_uses_real_signed_bounded_pages_without_login_or_cases(self):
        world = SetupWorld()
        world.stub = lambda method, path, value: {"entries": [{"seq": 8}], "latest": 8}
        value = json.loads(c.handle(raw(request("journal", after=7, discard_baseline=False)), sweep_factory=world.factory))
        window = {"after": 7, "cursor": 8, "discard_baseline": False, "entries": [{"seq": 8}], "requests": 1}
        self.assertEqual(value, {"version": 1, "status": "ok", "operation": "journal", "journal":window,
                                'stub_receipts':receipts('journal',window)})
        self.assertEqual([(r[0], r[1]) for r in world.calls], [("GET", "/_control/journal?after=7&limit=1000")])
        self.assertNotIn("cases", value)
        self.assertNotIn(fixture().control_key, json.dumps(value))

    def test_journal_explicit_truncated_baseline_allowed_measured_zero_refused(self):
        for baseline, expected in ((True, "ok"), (False, "error")):
            world = SetupWorld()
            world.stub = lambda method, path, value: {"entries": [{"seq": 50}], "latest": 50}
            result = json.loads(c.handle(raw(request("journal", after=0, discard_baseline=baseline)), sweep_factory=world.factory))
            self.assertEqual(result["status"], expected)
            self.assertEqual(len(world.calls), 1)

    def test_journal_multpage_counts_requests_and_refuses_missing_cursor(self):
        world = SetupWorld()
        pages = iter([{"entries": [{"seq": 2}], "latest": 3},
                      {"entries": [{"seq": 3}, {"seq": 4}], "latest": 4}])
        world.stub = lambda method, path, value: next(pages)
        result = json.loads(c.handle(raw(request("journal", after=1, discard_baseline=False)), sweep_factory=world.factory))
        self.assertEqual(result["journal"]["requests"], 2)
        self.assertEqual([r[1] for r in world.calls], ["/_control/journal?after=1&limit=1000", "/_control/journal?after=2&limit=1000"])
        result["journal"]["cursor"] = 5
        with self.assertRaises(c.sweep.proof.EvidenceError):
            c.journal_result(result["journal"], after=1, discard_baseline=False)

    def test_journal_request_boundaries_are_pure_and_no_arbitrary_routes(self):
        variants = [request("journal", after=x, discard_baseline=False) for x in (-1, True, 2**64, "0")]
        variants += [request("journal", after=1, discard_baseline=True), request("journal", after=0),
                     request("journal", after=0, discard_baseline=1),
                     request("journal", after=0, discard_baseline=False, path="/_control/reset")]
        for value in variants:
            with self.assertRaises(c.sweep.proof.EvidenceError): c.parse_request(raw(value))
        self.assertFalse(self.fake.calls)

    def test_rejected_is_fixed_signed_ring_with_no_discard_or_arbitrary_endpoint(self):
        world=SetupWorld()
        world.stub=lambda method,path,value: {'entries':[],'latest':0}
        result=json.loads(c.handle(raw(request('rejected',after=0,discard_baseline=False)),sweep_factory=world.factory))
        self.assertEqual(result['status'],'ok')
        self.assertEqual(result['rejected'],{'after':0,'cursor':0,'discard_baseline':False,'entries':[],'requests':1})
        self.assertEqual(world.calls[0][1],'/_control/rejected?after=0&limit=1000')
        self.assertEqual(result['stub_receipts'],receipts('rejected',result['rejected']))
        for change in ({'discard_baseline':True},{'after':True},{'path':'/_control/reset'}):
            with self.assertRaises(c.sweep.proof.EvidenceError):
                c.parse_request(raw({**request('rejected',after=0,discard_baseline=False),**change}))

    def test_receipt_shape_counts_windows_and_success_status_cannot_be_forged(self):
        import copy
        base={'stub_receipts':receipts('write')}
        changes=[lambda x:x.pop('windows'), lambda x:x['calls'].pop(),
                 lambda x:x['calls'][0].update(status=True),lambda x:x['calls'][0].update(route='reset'),
                 lambda x:x['windows'][0].update(requests=2),lambda x:x['windows'][1].update(after=4),
                 lambda x:x['windows'][0].update(extra='secret')]
        for change in changes:
            value=copy.deepcopy(base);change(value['stub_receipts'])
            with self.assertRaises(c.sweep.proof.EvidenceError):c.validate_receipts(request('write',nonce=test_sweep.NONCE),value)

    def test_read_uses_retained_record_without_write(self):
        value = self.handle(request("read", record=asdict(record())))
        self.assertEqual(value["cases"], self.fake.flags)
        self.assertNotIn("record", value)
        self.assertEqual([x[0] for x in self.fake.calls], ["fixture", "login", "read"])

    def test_actual_sweep_fake_http_private_write_read_roundtrip(self):
        world = test_sweep.World()
        world.f = fixture()
        def factory(value):
            return c.sweep.Sweep(value, product=world.transport(), stub=world.transport(True),
                                 wall=lambda: test_sweep.WALL, sleep=lambda _: None)
        written = json.loads(c.handle(raw(request("write", nonce=test_sweep.NONCE)), sweep_factory=factory))
        self.assertEqual(written["status"], "ok")
        read = json.loads(c.handle(raw(request("read", record=written["record"])), sweep_factory=factory))
        self.assertEqual(read["cases"], {key: True for key in c.sweep.proof.SWEEP_CASES})
        self.assertEqual(len(world.contacts), 1)

    def test_actual_read_value_mismatch_completes_but_late_auth_failure_does_not(self):
        world = test_sweep.World(); world.f = fixture()
        def factory(value):
            return c.sweep.Sweep(value, product=world.transport(), stub=world.transport(True),
                                 wall=lambda: test_sweep.WALL, sleep=lambda _: None)
        retained = world.client().write(test_sweep.NONCE)
        world.templates[retained.template_id]["body_content"] = "changed"
        frame = raw(request("read", record=asdict(retained)))
        result = json.loads(c.handle(frame, sweep_factory=factory))
        self.assertEqual(result, {"version": 1, "status": "ok", "operation": "read",
                                  "cases": {key: key != "template" for key in c.sweep.proof.SWEEP_CASES},
                                  "stub_receipts": receipts("read")})
        world.intercept = lambda m, p, b: c.sweep.Reply(401, (), b'private') if p == "/api/chatbot/settings" else None
        failed = json.loads(c.handle(frame, sweep_factory=factory))
        self.assertEqual(failed, {"version": 1, "status": "error", "failure": "semantic"})
        self.assertNotIn("cases", failed)
        for private in (fixture().password, fixture().control_key, retained.contact_id, "private"):
            self.assertNotIn(private, json.dumps(failed))

    def test_readiness_fixed_gets_no_login_and_no_sweep_flags(self):
        calls = []
        class HTTP:
            def __init__(self, origin, namespace, role):
                self.origin = origin
                calls.append((origin, namespace, role))
            def request(self, *args):
                calls.append(args)
                return c.sweep.Reply(200, (), b"{}")
        value = json.loads(c.handle(raw(request("readiness")), http_factory=HTTP,
                                    sweep_factory=self.fake.factory))
        self.assertEqual(value["ready"], {"web": True, "graph": True})
        self.assertNotIn("cases", value)
        self.assertFalse(self.fake.calls)
        self.assertEqual([calls[x][0:2] for x in (1, 3)], [("GET", "/ready"), ("GET", "/readyz")])
        self.assertTrue(all(calls[x][2:4] == ({"Accept": "application/json"}, b"") for x in (1, 3)))
        self.assertTrue(all(0 < calls[x][4] <= 15 for x in (1, 3)))

    def test_readiness_non200_or_exception_never_retries(self):
        for response in (c.sweep.Reply(302, (), b"secret"), c.sweep.Reply(503, (), b"secret"),
                         RuntimeError("raw secret password")):
            with self.subTest(response=type(response).__name__):
                calls = []
                class HTTP:
                    def __init__(self, origin, namespace, role): self.origin = origin
                    def request(self, *args):
                        calls.append(args)
                        if isinstance(response, Exception): raise response
                        return response
                value = c.handle(raw(request("readiness")), http_factory=HTTP)
                self.assertEqual(json.loads(value), {"version": 1, "status": "error", "failure": "infrastructure"})
                self.assertEqual(len(calls), 1)
                self.assertNotIn(b"secret", value)

    def test_fixture_rejects_external_loopback_foreign_alias_ports_and_local_namespace(self):
        changes = [{"product_origin": x} for x in ("https://example.com", "http://127.0.0.1:8080",
                   "http://rereply-compat-ffffffffffff-web:8080", fixture().product_origin + "/",
                   fixture().product_origin.replace(":8080", ":8081"))]
        changes += [{"stub_origin": "http://127.0.0.1:8090"}, {"namespace": "rereply-local-0123456789ab"}]
        for delta in changes:
            value = request("write", nonce=test_sweep.NONCE)
            value["fixture"].update(delta)
            self.assertEqual(self.handle(value)["failure"], "evidence")
        self.assertFalse(self.fake.calls)

    def test_setup_real_signed_transport_post_then_bound_get_only(self):
        world = SetupWorld()
        value = json.loads(c.handle(raw(request("setup", business_account_id=BUSINESS)), sweep_factory=world.factory))
        self.assertEqual(value, {"version": 1, "status": "ok", "operation": "setup", "registered": True,
                                'stub_receipts':receipts('setup')})
        self.assertEqual([(x[0], x[1]) for x in world.calls],
                         [("POST", "/_control/accounts"), ("GET", "/_control/accounts")])
        self.assertTrue(all(0 < x[4] <= 45 for x in world.calls))
        self.assertNotEqual(world.calls[0][2]["X-Stub-Nonce"], world.calls[1][2]["X-Stub-Nonce"])
        self.assertNotIn(fixture().control_key, json.dumps(value))
        self.assertNotIn(fixture().password, json.dumps(value))
        self.assertNotIn("cases", value)

    def test_setup_schema_and_synthetic_business_identity_refuse_before_io(self):
        values = [request("setup"), request("setup", business_account_id=BUSINESS, payload={}),
                  request("setup", business_account_id=BUSINESS, url=fixture().stub_origin),
                  request("setup", business_account_id=BUSINESS, display_phone_number="private")]
        values += [request("setup", business_account_id=x) for x in
                   (None, True, 999987654321098, "", "123456789012345", BUSINESS + " ", fixture().phone_number_id)]
        for value in values:
            with self.subTest(value=value.get("business_account_id")):
                self.assertEqual(self.handle(value), {"version": 1, "status": "error", "failure": "evidence"})
        self.assertFalse(self.fake.calls)

    def test_setup_requires_exact_ack_without_retry_or_read_after_failure(self):
        for reply in ({"ok": False}, {"ok": 1}, {"ok": True, "extra": "secret"}, {}):
            world = SetupWorld()
            world.mutate = lambda method, path, value: reply
            value = c.handle(raw(request("setup", business_account_id=BUSINESS)), sweep_factory=world.factory)
            self.assertEqual(json.loads(value)["failure"], "evidence")
            self.assertEqual(len(world.calls), 1)
            self.assertNotIn(b"secret", value)

    def test_setup_readback_missing_duplicate_foreign_or_changed_rows_refuse(self):
        expected = {"business_account_id": BUSINESS, "phone_number_id": fixture().phone_number_id,
                    "display_phone_number": fixture().phone_number_id, "verified_name": fixture().namespace}
        variants = [{}, {"accounts": []}, {"accounts": [expected, expected]},
                    {"accounts": [dict(expected, business_account_id="999000000000001")]},
                    {"accounts": [dict(expected, phone_number_id="999000000000002")]},
                    {"accounts": [dict(expected, verified_name="foreign")]},
                    {"accounts": [dict(expected, display_phone_number="999000000000003")]},
                    {"accounts": [dict(expected, extra="secret")]},
                    {"accounts": [expected], "extra": "secret"}]
        for reply in variants:
            world = SetupWorld()
            world.mutate = lambda method, path, value: reply if method == "GET" else value
            value = c.handle(raw(request("setup", business_account_id=BUSINESS)), sweep_factory=world.factory)
            self.assertEqual(json.loads(value)["failure"], "evidence")
            self.assertEqual(len(world.calls), 2)
            self.assertNotIn(b"secret", value)

    def test_setup_ambiguous_http_or_malformed_response_never_retries_or_leaks(self):
        responses = [(c.sweep.Reply(302, (), b"secret"), "infrastructure"),
                     (c.sweep.Reply(403, (), b"secret"), "semantic"),
                     (c.sweep.Reply(200, (), b'{"ok":true,"ok":true}'), "evidence"),
                     (c.sweep.Reply(200, (), b'{"ok":NaN}'), "evidence"),
                     (c.sweep.Reply(200, (), b"x" * (c.sweep.MAX_BODY + 1)), "evidence"),
                     (RuntimeError("raw secret control key"), "infrastructure")]
        for response, failure in responses:
            world = SetupWorld()
            def intercept(method, path, body):
                if isinstance(response, Exception):
                    raise response
                return response
            world.intercept = intercept
            value = c.handle(raw(request("setup", business_account_id=BUSINESS)), sweep_factory=world.factory)
            self.assertEqual(json.loads(value), {"version": 1, "status": "error", "failure": failure})
            self.assertEqual(len(world.calls), 1)
            self.assertNotIn(b"secret", value)

    def test_closed_schema_missing_extra_and_wrong_types_before_io(self):
        base = request("write", nonce=test_sweep.NONCE)
        values = [dict(base, version=True), dict(base, version=2), dict(base, operation="delete"),
                  dict(base, nonce="bad"), dict(base, extra="secret"), dict(base, operation=[])]
        for key in base:
            values.append({k: v for k, v in base.items() if k != key})
        for key in base["fixture"]:
            value = request("write", nonce=test_sweep.NONCE)
            del value["fixture"][key]
            values.append(value)
        for value in (dict(base["fixture"], extra="secret"), dict(base["fixture"], password=3)):
            values.append(dict(base, fixture=value))
        for value in values:
            self.assertEqual(self.handle(value)["failure"], "evidence")
        self.assertFalse(self.fake.calls)

    def test_duplicate_nan_invalid_utf8_deep_and_oversized_frames(self):
        values = [b'{"version":1,"version":1}', b'{"version":NaN}', b'\xff', b'[]', b'',
                  b'[' * 2000 + b']' * 2000, b' ' * (c.MAX_FRAME + 1), b'{}{}',
                  b'{"fixture":{"password":"a","password":"b"}}']
        for value in values:
            self.assertEqual(json.loads(c.handle(value, sweep_factory=self.fake.factory))["failure"], "evidence")
        self.assertFalse(self.fake.calls)

    def test_retained_record_schema_binding_nonce_uuid_refuse_before_login(self):
        base = asdict(record())
        variants = [dict(base, fixture_binding="f" * 64), dict(base, nonce="x" * 32),
                    dict(base, contact_id="secret"), dict(base, extra="secret")]
        variants.extend({k: v for k, v in base.items() if k != missing} for missing in base)
        for value in variants:
            self.assertEqual(self.handle(request("read", record=value))["failure"], "evidence")
        self.assertFalse(self.fake.calls)

    def test_write_invalid_record_never_returns_partial_private_response(self):
        self.fake.retained = replace(record(), nonce="f" * 32)
        value = self.handle(request("write", nonce=test_sweep.NONCE))
        self.assertEqual(value, {"version": 1, "status": "error", "failure": "evidence"})
        self.assertNotIn("read", [x[0] for x in self.fake.calls])

    def test_missing_or_nonboolean_sweep_flags_refuse(self):
        for flags in ({}, {key: 1 for key in c.sweep.proof.SWEEP_CASES}, {**self.fake.flags, "extra": True}):
            self.fake.flags = flags
            self.assertEqual(self.handle(request("read", record=asdict(record())))["failure"], "evidence")

    def test_raw_errors_map_to_fixed_classes_no_secret_or_traceback(self):
        for error, expected in ((c.sweep.SemanticFailure("raw secret semantic"), "semantic"),
                                (c.sweep.proof.InfrastructureFailure("raw secret http"), "infrastructure"),
                                (c.sweep.proof.EvidenceError("raw secret evidence"), "evidence"),
                                (RuntimeError("raw secret password"), "infrastructure")):
            self.fake.failure = error
            value = self.handle(request("read", record=asdict(record())))
            self.assertEqual(value, {"version": 1, "status": "error", "failure": expected})
        self.assertEqual(sum(x[0] == "login" for x in self.fake.calls), 4)

    def test_binary_stdio_one_frame_bound_and_no_cli_arguments(self):
        class Input(io.BytesIO):
            def read(self, size=-1):
                self.requested = size
                return super().read(size)
        stdin, stdout = Input(raw(request("read", record=asdict(record())))), io.BytesIO()
        self.assertEqual(c.run(stdin, stdout, sweep_factory=self.fake.factory), 0)
        self.assertEqual(stdin.requested, c.MAX_FRAME + 1)
        self.assertEqual(len(stdout.getvalue().splitlines()), 1)
        stdin, stdout = Input(b"private secret"), io.BytesIO()
        self.assertEqual(c.run(stdin, stdout, argv=("secret",)), 1)
        self.assertEqual(stdin.tell(), 0)
        self.assertNotIn(b"secret", stdout.getvalue())


if __name__ == "__main__":
    unittest.main()
