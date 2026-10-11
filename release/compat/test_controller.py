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


def fixture():
    value = test_sweep.fixture()
    return replace(value, product_origin=f"http://{value.namespace}-web:8080",
                   stub_origin=f"http://{value.namespace}-graph:8090")


def request(operation, **extra):
    return {"version": 1, "operation": operation, "fixture": asdict(fixture()), **extra}


def raw(value):
    return json.dumps(value).encode()


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

    def read(self, retained):
        self.calls.append(("read", retained))
        return self.flags


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.fake = FakeSweep()

    def handle(self, value):
        return json.loads(c.handle(raw(value), sweep_factory=self.fake.factory))

    def test_pure_preflight_performs_no_sweep_or_http_operations(self):
        values = [request("readiness"), request("write", nonce=test_sweep.NONCE),
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
                                "record": asdict(record()), "cases": self.fake.flags})
        self.assertEqual([x[0] for x in self.fake.calls], ["fixture", "login", "write", "read"])
        self.assertNotIn("password", json.dumps(value))

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
