"""Private, one-request controller protocol for an already owned synthetic stack.

Input and successful retained records are private in-memory transport, never a
public compatibility artifact. The caller provisions the fixture, owns the
internal Docker network and bounds the process lifetime. Setup registers only
that synthetic fixture in owned Graph before measured sweeps. No database
provisioning, classification, Docker access, retry or complete proof exists here.
"""
from __future__ import annotations

from dataclasses import asdict, fields
import json
from pathlib import Path
import re
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
import sweep

MAX_FRAME = 256 * 1024
_FIXTURE_FIELDS = frozenset(f.name for f in fields(sweep.Fixture))
_RECORD_FIELDS = frozenset(f.name for f in fields(sweep.RecordSet))
_ID_FIELDS = _RECORD_FIELDS - {"fixture_binding", "nonce"}


def _need(condition):
    if not condition:
        raise sweep.proof.EvidenceError("controller-input-invalid")


def _encode(value):
    raw = json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8") + b"\n"
    _need(len(raw) <= MAX_FRAME)
    return raw


def _failure(kind):
    return _encode({"version": 1, "status": "error", "failure": kind})


def _parse(raw):
    _need(type(raw) is bytes and 0 < len(raw) <= MAX_FRAME)

    def pairs(items):
        result = {}
        for key, value in items:
            _need(key not in result)
            result[key] = value
        return result

    def constant(_value):
        raise ValueError

    try:
        value = json.loads(raw.decode("utf-8", "strict"), object_pairs_hook=pairs, parse_constant=constant)
    except (ValueError, UnicodeError, RecursionError):
        raise sweep.proof.EvidenceError("controller-input-invalid") from None
    _need(type(value) is dict)
    return value


def _fixture(value):
    _need(type(value) is dict and set(value) == _FIXTURE_FIELDS)
    _need(all(type(v) is str for v in value.values()))
    namespace = value["namespace"]
    _need(re.fullmatch(r"rereply-compat-[0-9a-f]{12}", namespace) is not None)
    # The generic sweep also supports loopback. This controller deliberately
    # accepts only its two exact owned-network aliases and fixed service ports.
    _need(value["product_origin"] == f"http://{namespace}-web:8080"
          and value["stub_origin"] == f"http://{namespace}-graph:8090")
    return sweep.Fixture(**value)


def _record(value, fixture):
    _need(type(value) is dict and set(value) == _RECORD_FIELDS)
    _need(all(type(v) is str for v in value.values()))
    _need(re.fullmatch(r"[0-9a-f]{64}", value["fixture_binding"]) is not None
          and sweep._NONCE.fullmatch(value["nonce"]) is not None)
    for key in _ID_FIELDS:
        sweep._uuid(value[key])
    record = sweep.RecordSet(**value)
    # Sweep and HTTP constructors perform no I/O. Reuse the existing binding
    # implementation before Docker creation without copying its algorithm or
    # accepting a record for another fixture before login.
    _need(record.fixture_binding == sweep.Sweep(fixture)._binding())
    return record


def _flags(value):
    _need(type(value) is dict and set(value) == sweep.proof.SWEEP_CASES
          and all(type(v) is bool for v in value.values()))
    return dict(value)


def _readiness(fixture, http_factory, clock):
    deadline = clock() + 30
    flags = {}
    for role, origin, path in (("web", fixture.product_origin, "/ready"),
                               ("graph", fixture.stub_origin, "/readyz")):
        left = deadline - clock()
        if left <= 0:
            raise sweep.proof.InfrastructureFailure("controller-readiness-incomplete")
        transport = http_factory(origin, fixture.namespace, role)
        _need(transport.origin == origin)
        reply = transport.request("GET", path, {"Accept": "application/json"}, b"", min(15, left))
        _need(type(reply) is sweep.Reply and type(reply.status) is int
              and type(reply.body) is bytes and len(reply.body) <= sweep.MAX_BODY)
        if reply.status != 200 or clock() > deadline:
            raise sweep.proof.InfrastructureFailure("controller-readiness-incomplete")
        flags[role] = True
    return flags


def _setup(fixture, business_account_id, sweep_factory):
    # Reuse the existing bounded transport and HMAC implementation. These two
    # control requests provision a fixture; they are never measured sweep or
    # worker evidence. No login, arbitrary payload, endpoint or retry is used.
    account = {"business_account_id": business_account_id,
               "phone_number_id": fixture.phone_number_id,
               "display_phone_number": fixture.phone_number_id,
               "verified_name": fixture.namespace}
    client = sweep_factory(fixture)
    client._begin()
    acknowledged = client._call("POST", "/_control/accounts", account, stub=True)
    _need(type(acknowledged) is dict and set(acknowledged) == {"ok"}
          and acknowledged["ok"] is True)
    observed = client._call("GET", "/_control/accounts", stub=True)
    # OwnedStack starts Graph without accounts. Admit precisely our one account,
    # including its complete returned shape; missing/foreign/duplicate rows fail.
    _need(type(observed) is dict and set(observed) == {"accounts"}
          and observed["accounts"] == [account])
    return True


def parse_request(raw: bytes) -> dict:
    """Pure preflight for the owner, before creating any controller resource.

    Raises fixed-code EvidenceError on an invalid frame. The returned dict is
    still private. The owner must additionally bind its namespace to its own
    stack before passing these same bytes to the controller process.
    """
    request = _parse(raw)
    _need(type(request.get("version")) is int and request["version"] == 1)
    operation = request.get("operation")
    _need(type(operation) is str and operation in {"setup", "readiness", "write", "read"})
    keys = {"version", "operation", "fixture"}
    if operation == "setup":
        keys.add("business_account_id")
    elif operation == "write":
        keys.add("nonce")
    elif operation == "read":
        keys.add("record")
    _need(set(request) == keys)
    fixture = _fixture(request["fixture"])
    if operation == "setup":
        _need(type(request["business_account_id"]) is str
              and sweep._DIGITS.fullmatch(request["business_account_id"]) is not None
              and request["business_account_id"] != fixture.phone_number_id)
    elif operation == "write":
        _need(type(request["nonce"]) is str and sweep._NONCE.fullmatch(request["nonce"]) is not None)
    elif operation == "read":
        _record(request["record"], fixture)
    return request


def handle(raw: bytes, *, sweep_factory=sweep.Sweep, http_factory=sweep.HTTP,
           clock=time.monotonic) -> bytes:
    """One bounded private frame; fixed failures never retain exception text.

    The owning process adapter must enforce a wall deadline: setup 100s,
    readiness 30s, write 600s and read 400s. Setup uses two <=45s requests;
    Sweep retains its existing per-operation budgets.
    A success response is protocol completion, not compatibility qualification.
    """
    try:
        request = parse_request(raw)
        operation = request["operation"]
        fixture = sweep.Fixture(**request["fixture"])
        nonce, record = None, None
        if operation == "write":
            nonce = request["nonce"]
        elif operation == "read":
            record = sweep.RecordSet(**request["record"])
        response = {"version": 1, "status": "ok", "operation": operation}
        if operation == "setup":
            response["registered"] = _setup(fixture, request["business_account_id"], sweep_factory)
        elif operation == "readiness":
            response["ready"] = _readiness(fixture, http_factory, clock)
        else:
            client = sweep_factory(fixture)
            client.login()
            if operation == "write":
                record = client.write(nonce)
                _need(type(record) is sweep.RecordSet)
                _record(asdict(record), fixture)
                _need(record.nonce == nonce)
                response["record"] = asdict(record)
            # Sweep.write already checks retained values internally but returns
            # only a RecordSet. Obtain the flags from actual Sweep.read instead
            # of manufacturing six true values from a successful return.
            response["cases"] = _flags(client.read(record))
        return _encode(response)
    except sweep.SemanticFailure:
        return _failure("semantic")
    except sweep.proof.InfrastructureFailure:
        return _failure("infrastructure")
    except sweep.proof.EvidenceError:
        return _failure("evidence")
    except Exception:
        return _failure("infrastructure")


def run(stdin, stdout, *, argv=(), **adapters) -> int:
    """No CLI arguments or secrets in argv; caller closes stdin after one frame."""
    try:
        response = _failure("evidence") if argv else handle(stdin.read(MAX_FRAME + 1), **adapters)
    except Exception:
        response = _failure("infrastructure")
    try:
        stdout.write(response)
        stdout.flush()
    except Exception:
        return 1
    return 0 if json.loads(response)["status"] == "ok" else 1


if __name__ == "__main__":
    raise SystemExit(run(sys.stdin.buffer, sys.stdout.buffer, argv=sys.argv[1:]))
