#!/usr/bin/env python3
"""Tokenless, content-free CRM production canary verifier.

The observer reads only an allowlisted sanitized change receipt plus protected synthetic
canary configuration.  It never receives a DigitalOcean token, provider app
identifier, customer message, screenshot, browser trace, or full response
body.  The finalizer is a separate credential-free process that turns the
verified receipt and canary result into the next signed phase-state subject.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import hmac
import importlib.util
import ipaddress
import json
import os
import re
import socket
import ssl
import stat
import sys
import zipfile
from http.client import HTTPResponse
from pathlib import Path, PurePosixPath
from types import ModuleType
from typing import Any, Iterable, Mapping, NoReturn, Sequence
from urllib.parse import urlsplit


REPOSITORY = "medtechcorps-netizen/whatomate"
WORKFLOW_PATH = ".github/workflows/verify-production-crm-canary.yml"
WORKFLOW_NAME = "Verify Production CRM Canary"
RECEIPT_KINDS = {
    "apply": {
        "workflow_path": ".github/workflows/apply-production-phase.yml",
        "workflow_name": "Apply Production Phase",
        "gate_job": "Exact production apply receipt gate",
        "artifact_prefix": "production-phase-apply",
        "stem": "production-phase-apply-receipt",
        "authority": "production-phase-apply-receipt",
        "verifier_path": "release/deployment/verify_production_release.py",
        "validator": "validate_apply_receipt",
    },
    "rollback": {
        "workflow_path": ".github/workflows/rollback-production-phase.yml",
        "workflow_name": "Rollback Production Phase",
        "gate_job": "Exact production rollback receipt gate",
        "artifact_prefix": "production-phase-rollback",
        "stem": "production-phase-rollback-receipt",
        "authority": "production-phase-rollback-receipt",
        "verifier_path": "release/deployment/rollback_production_change.py",
        "validator": "validate_rollback_receipt",
    },
    "apply-reconciled": {
        "workflow_path": ".github/workflows/apply-production-phase.yml",
        "workflow_name": "Apply Production Phase",
        "gate_job": "Exact production apply receipt gate",
        "artifact_prefix": "production-phase-apply",
        "stem": "production-phase-apply-receipt",
        "authority": "production-phase-apply-receipt",
        "verifier_path": "release/deployment/verify_production_release.py",
        "validator": "validate_apply_receipt",
        "reconciliation_required": True,
        "operation": "apply",
    },
    "rollback-reconciled": {
        "workflow_path": ".github/workflows/rollback-production-phase.yml",
        "workflow_name": "Rollback Production Phase",
        "gate_job": "Exact production rollback receipt gate",
        "artifact_prefix": "production-phase-rollback",
        "stem": "production-phase-rollback-receipt",
        "authority": "production-phase-rollback-receipt",
        "verifier_path": "release/deployment/rollback_production_change.py",
        "validator": "validate_rollback_receipt",
        "reconciliation_required": True,
        "operation": "rollback",
    },
    "reconciliation": {
        "workflow_path": ".github/workflows/reconcile-production-orphan.yml",
        "workflow_name": "Reconcile Production Orphan",
        "gate_job": "Exact production orphan reconciliation gate",
        "artifact_prefix": "production-orphan-reconciliation",
        "stem": "production-orphan-reconciliation",
        "authority": "production-orphan-reconciliation-receipt",
        "verifier_path": "release/deployment/verify_production_release.py",
        "validator": "validate_reconciliation_receipt",
    },
    "orphan-rollback": {
        "workflow_path": ".github/workflows/rollback-production-orphan.yml",
        "workflow_name": "Rollback Production Orphan",
        "gate_job": "Exact production orphan rollback receipt gate",
        "artifact_prefix": "production-orphan-rollback",
        "stem": "production-orphan-rollback-receipt",
        "authority": "production-orphan-rollback-receipt",
        "verifier_path": "release/deployment/rollback_production_change.py",
        "validator": "validate_orphan_rollback_receipt",
    },
}
CANARY_ARTIFACT_FILES = (
    "production-crm-canary.json",
    "production-crm-canary.sha256",
)
PHASE_STATE_FILES = (
    "production-phase-state.json",
    "production-phase-state.sha256",
)
PHASES = ("baseline", "bridge", "backend", "ui")
HEALTH_STATUSES = {
    "app-health": 200,
    "app-ready": 200,
    "meta-live": 204,
    "meta-ready": 204,
    "gmail-live": 204,
    "gmail-ready": 204,
}
UI_CHECKS = (
    "klinik_whatsapp_outbound",
    "klinik_whatsapp_inbound",
    "omnichannel_outbound_realtime_without_reload",
    "omnichannel_inbound_realtime_without_reload",
    "navbar_unread_increment",
    "navbar_unread_clear",
    "omnichannel_conversation_switch_autoscroll",
    "omnichannel_late_layout_autoscroll",
    "native_chat_realtime_without_reload",
    "native_chat_conversation_switch_autoscroll",
    "native_chat_late_layout_autoscroll",
    "non_klinik_send_denied",
    "cross_organization_send_denied",
)
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
RUN_ID_RE = re.compile(r"^[1-9][0-9]{0,14}$")
MAX_JSON_BYTES = 131_072
MAX_ARCHIVE_BYTES = 524_288
MAX_HEALTH_BODY_BYTES = 4096
MAX_DRIVER_BODY_BYTES = 65_536
MAX_DRIVER_CLOCK_SKEW_SECONDS = 300


class CanaryError(RuntimeError):
    """A deliberately content-free canary failure."""


def fail(message: str) -> NoReturn:
    raise CanaryError(message)


def _reject_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            fail("JSON contains a duplicate key")
        value[key] = item
    return value


def _reject_number(raw: str) -> NoReturn:
    del raw
    fail("JSON floating-point and non-finite numbers are forbidden")


def loads_strict(raw: str | bytes) -> Any:
    if isinstance(raw, bytes):
        try:
            raw = raw.decode("utf-8")
        except UnicodeError as exc:
            raise CanaryError("JSON is not UTF-8") from exc
    try:
        return json.loads(
            raw,
            object_pairs_hook=_reject_pairs,
            parse_float=_reject_number,
            parse_constant=_reject_number,
        )
    except CanaryError:
        raise
    except (json.JSONDecodeError, TypeError, ValueError) as exc:
        raise CanaryError("JSON is malformed") from exc


def canonical_payload_bytes(value: Any) -> bytes:
    try:
        return json.dumps(
            value,
            ensure_ascii=True,
            allow_nan=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("ascii")
    except (TypeError, ValueError) as exc:
        raise CanaryError("value is not canonical JSON") from exc


def canonical_file_bytes(value: Any) -> bytes:
    return canonical_payload_bytes(value) + b"\n"


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def exact_keys(value: Any, expected: Iterable[str], label: str) -> dict[str, Any]:
    expected_set = set(expected)
    if type(value) is not dict or set(value) != expected_set:
        fail(f"{label} keys differ")
    return value


def exact_string(
    value: Any,
    label: str,
    pattern: re.Pattern[str] | None = None,
    *,
    maximum: int = 4096,
) -> str:
    if type(value) is not str or not value or len(value) > maximum:
        fail(f"{label} is invalid")
    if any(character in value for character in "\r\n\x00"):
        fail(f"{label} contains forbidden characters")
    if pattern is not None and pattern.fullmatch(value) is None:
        fail(f"{label} has an invalid format")
    return value


def exact_int(value: Any, label: str, minimum: int = 1) -> int:
    if type(value) is not int or value < minimum or value > 2_147_483_647:
        fail(f"{label} is invalid")
    return value


def load_canonical(path: Path, label: str, maximum: int = MAX_JSON_BYTES) -> Any:
    if path.is_symlink() or not path.is_file():
        fail(f"{label} is not a regular file")
    raw = path.read_bytes()
    if not raw or len(raw) > maximum:
        fail(f"{label} has an invalid size")
    value = loads_strict(raw)
    if raw != canonical_file_bytes(value):
        fail(f"{label} is not canonical JSON")
    return value


def validate_sidecar(path: Path, subject: Path, expected: str, label: str) -> None:
    if path.is_symlink() or not path.is_file():
        fail(f"{label} hash is not a regular file")
    raw = path.read_bytes()
    if raw != (expected + "\n").encode("ascii"):
        fail(f"{label} hash sidecar differs")
    if sha256_bytes(subject.read_bytes()) != expected:
        fail(f"{label} exact-file hash differs")


def ensure_output_directory(path: Path) -> Path:
    runner_temp_raw = os.environ.get("RUNNER_TEMP")
    if not runner_temp_raw:
        fail("RUNNER_TEMP is required")
    runner_temp = Path(runner_temp_raw)
    if runner_temp.is_symlink() or not runner_temp.is_dir():
        fail("RUNNER_TEMP is not a real directory")
    root = runner_temp.resolve(strict=True)
    parent = path.parent.resolve(strict=True)
    if parent != root or path.name in {"", ".", ".."}:
        fail("output directory must be a direct RUNNER_TEMP child")
    if path.exists() or path.is_symlink():
        fail("output directory already exists")
    path.mkdir(mode=0o700)
    if path.resolve(strict=True).parent != root:
        fail("output directory escaped RUNNER_TEMP")
    return path


def write_exclusive(path: Path, raw: bytes, label: str) -> None:
    if len(raw) == 0 or len(raw) > MAX_JSON_BYTES:
        fail(f"{label} output has an invalid size")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, 0o600)
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISREG(opened.st_mode):
            fail(f"{label} output is not a regular file")
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
    except BaseException:
        try:
            os.close(descriptor)
        except OSError:
            pass
        path.unlink(missing_ok=True)
        raise


def write_two_file_artifact(directory: Path, stem: str, value: Mapping[str, Any]) -> str:
    raw = canonical_file_bytes(value)
    digest = sha256_bytes(raw)
    subject = directory / f"{stem}.json"
    sidecar = directory / f"{stem}.sha256"
    try:
        write_exclusive(subject, raw, stem)
        write_exclusive(sidecar, (digest + "\n").encode("ascii"), f"{stem} hash")
    except BaseException:
        subject.unlink(missing_ok=True)
        sidecar.unlink(missing_ok=True)
        raise
    return digest


def validate_release_descriptor(value: Any, expected_control_sha: str) -> dict[str, Any]:
    if type(value) is not dict:
        fail("release evidence descriptor keys differ")
    kind = exact_string(value.get("receipt_kind"), "receipt kind")
    reconciled = kind in {"apply-reconciled", "rollback-reconciled"}
    expected_keys = {
        "control_sha", "receipt_kind", "run_id", "run_attempt", "artifact_id",
        "artifact_digest", "receipt_sha256",
    }
    if reconciled:
        expected_keys.add("release_reconciliation")
    descriptor = exact_keys(
        value,
        expected_keys,
        "release evidence descriptor",
    )
    normalized = {
        "control_sha": exact_string(descriptor["control_sha"], "control SHA", SHA1_RE),
        "receipt_kind": exact_string(descriptor["receipt_kind"], "receipt kind"),
        "run_id": exact_string(str(descriptor["run_id"]), "receipt run ID", RUN_ID_RE),
        "run_attempt": exact_int(descriptor["run_attempt"], "receipt run attempt"),
        "artifact_id": exact_string(str(descriptor["artifact_id"]), "receipt artifact ID", RUN_ID_RE),
        "artifact_digest": exact_string(
            descriptor["artifact_digest"], "receipt artifact digest", DIGEST_RE
        ),
        "receipt_sha256": exact_string(
            descriptor["receipt_sha256"], "release receipt hash", SHA256_RE
        ),
    }
    if normalized["receipt_kind"] not in RECEIPT_KINDS:
        fail("release receipt kind is not allowlisted")
    if reconciled:
        item = exact_keys(
            descriptor["release_reconciliation"],
            {"run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"},
            "main-lock release reconciliation descriptor",
        )
        normalized["release_reconciliation"] = {
            "run_id": exact_string(str(item["run_id"]), "reconciliation run ID", RUN_ID_RE),
            "run_attempt": exact_int(item["run_attempt"], "reconciliation run attempt"),
            "artifact_id": exact_string(str(item["artifact_id"]), "reconciliation artifact ID", RUN_ID_RE),
            "artifact_digest": exact_string(item["artifact_digest"], "reconciliation artifact digest", DIGEST_RE),
            "sha256": exact_string(item["sha256"], "reconciliation exact-file hash", SHA256_RE),
        }
        if normalized["release_reconciliation"]["run_attempt"] != 1:
            fail("reconciliation run attempt differs")
    expected = exact_string(expected_control_sha, "expected control SHA", SHA1_RE)
    if normalized["control_sha"] != expected:
        fail("release evidence control SHA differs")
    return normalized


def load_control_module(control_root: Path, relative_path: str, module_name: str) -> ModuleType:
    path = control_root / relative_path
    if path.is_symlink() or not path.is_file():
        fail("shared production verifier is unavailable")
    spec = importlib.util.spec_from_file_location(module_name, path)
    if spec is None or spec.loader is None:
        fail("shared production verifier cannot be loaded")
    module = importlib.util.module_from_spec(spec)
    deployment_path = str(path.parent.resolve(strict=True))
    sys.modules[module_name] = module
    sys.path.insert(0, deployment_path)
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(module_name, None)
        raise
    finally:
        if sys.path[0] == deployment_path:
            del sys.path[0]
    return module


def load_phase_state_release_module(
    control_root: Path, receipt_kind: str
) -> ModuleType:
    """Load the shared phase-state builder and its canonical rollback dependency."""

    release = load_control_module(
        control_root,
        "release/deployment/verify_production_release.py",
        "verify_production_release",
    )
    if receipt_kind in {"rollback", "rollback-reconciled", "orphan-rollback"}:
        load_control_module(
            control_root,
            "release/deployment/rollback_production_change.py",
            "rollback_production_change",
        )
    return release


def validate_receipt(
    receipt_path: Path,
    sidecar_path: Path,
    descriptor: Mapping[str, Any],
    control_root: Path,
    reconciliation_path: Path | None = None,
    reconciliation_sidecar_path: Path | None = None,
) -> dict[str, Any]:
    receipt = load_canonical(receipt_path, "release receipt")
    receipt_digest = sha256_bytes(receipt_path.read_bytes())
    if receipt_digest != descriptor["receipt_sha256"]:
        fail("release receipt hash differs from dispatch authority")
    validate_sidecar(sidecar_path, receipt_path, receipt_digest, "release receipt")

    kind = descriptor["receipt_kind"]
    policy = RECEIPT_KINDS[kind]
    verifier = load_control_module(
        control_root, str(policy["verifier_path"]), f"rereply_{kind}_receipt_verifier"
    )
    validator = getattr(verifier, str(policy["validator"]), None)
    if not callable(validator):
        fail("shared release receipt validator is unavailable")
    try:
        validated = validator(receipt)
    except Exception as exc:
        raise CanaryError("shared release receipt validation failed") from exc
    if type(validated) is not dict:
        fail("shared release receipt validator returned an invalid value")

    control = validated.get("control")
    if type(control) is not dict:
        fail("release receipt control is invalid")
    if control.get("workflow_sha") != descriptor["control_sha"]:
        fail("release receipt control SHA differs")
    if control.get("workflow_path") != policy["workflow_path"]:
        fail("release receipt workflow path differs")
    if str(control.get("run_id")) != descriptor["run_id"]:
        fail("release receipt run ID differs")
    if control.get("run_attempt") != descriptor["run_attempt"]:
        fail("release receipt run attempt differs")
    reconciled = bool(policy.get("reconciliation_required"))
    if reconciled:
        if reconciliation_path is None or reconciliation_sidecar_path is None:
            fail("signed main-lock release reconciliation is required")
        reconciliation = load_canonical(
            reconciliation_path, "main-lock release reconciliation"
        )
        binding = descriptor["release_reconciliation"]
        reconciliation_digest = sha256_bytes(reconciliation_path.read_bytes())
        if reconciliation_digest != binding["sha256"]:
            fail("main-lock release reconciliation hash differs")
        validate_sidecar(
            reconciliation_sidecar_path,
            reconciliation_path,
            reconciliation_digest,
            "main-lock release reconciliation",
        )
        reconciler = load_control_module(
            control_root,
            "release/deployment/reconcile_production_main_lock_release.py",
            "rereply_main_lock_release_reconciler",
        )
        pair_validator = getattr(reconciler, "validate_pair", None)
        if not callable(pair_validator):
            fail("main-lock release reconciliation validator is unavailable")
        try:
            validated_reconciliation = pair_validator(reconciliation, validated)
        except Exception as exc:
            raise CanaryError("main-lock release reconciliation pairing failed") from exc
        if (
            validated_reconciliation.get("operation") != policy["operation"]
            or validated_reconciliation.get("control", {}).get("workflow_sha")
            != descriptor["control_sha"]
            or str(validated_reconciliation.get("control", {}).get("run_id"))
            != binding["run_id"]
            or validated_reconciliation.get("control", {}).get("run_attempt") != 1
        ):
            fail("main-lock release reconciliation control differs")
    elif reconciliation_path is not None or reconciliation_sidecar_path is not None:
        fail("main-lock release reconciliation is forbidden for direct receipts")
    return validated


def reconciliation_files() -> tuple[str, str]:
    return (
        "production-main-lock-release-reconciliation.json",
        "production-main-lock-release-reconciliation.sha256",
    )


def validate_reconciliation_archive(
    archive: Path, descriptor: Mapping[str, Any], output: Path
) -> None:
    binding = descriptor.get("release_reconciliation")
    if type(binding) is not dict:
        fail("main-lock release reconciliation descriptor is missing")
    if archive.is_symlink() or not archive.is_file():
        fail("main-lock release reconciliation archive is not a regular file")
    raw = archive.read_bytes()
    if not raw or len(raw) > MAX_ARCHIVE_BYTES:
        fail("main-lock release reconciliation archive has an invalid size")
    if f"sha256:{sha256_bytes(raw)}" != binding["artifact_digest"]:
        fail("main-lock release reconciliation archive API digest differs")
    output = ensure_output_directory(output)
    expected = reconciliation_files()
    with zipfile.ZipFile(archive) as bundle:
        infos = bundle.infolist()
        if len(infos) != 2 or sorted(item.filename for item in infos) != sorted(expected):
            fail("main-lock release reconciliation artifact inventory differs")
        for info in infos:
            path = PurePosixPath(info.filename)
            if (
                info.filename != path.as_posix()
                or path.is_absolute()
                or len(path.parts) != 1
                or info.is_dir()
                or info.file_size <= 0
                or info.file_size > MAX_JSON_BYTES
            ):
                fail("main-lock release reconciliation artifact entry is invalid")
            mode = (info.external_attr >> 16) & 0xFFFF
            if mode and not stat.S_ISREG(mode):
                fail("main-lock release reconciliation artifact entry is not regular")
            write_exclusive(output / info.filename, bundle.read(info.filename), "main-lock release reconciliation artifact")


def receipt_files(descriptor: Mapping[str, Any]) -> tuple[str, str]:
    stem = str(RECEIPT_KINDS[str(descriptor["receipt_kind"])]["stem"])
    return (f"{stem}.json", f"{stem}.sha256")


def validate_archive(archive: Path, descriptor: Mapping[str, Any], output: Path) -> None:
    if archive.is_symlink() or not archive.is_file():
        fail("release receipt archive is not a regular file")
    raw = archive.read_bytes()
    if not raw or len(raw) > MAX_ARCHIVE_BYTES:
        fail("release receipt archive has an invalid size")
    if f"sha256:{sha256_bytes(raw)}" != descriptor["artifact_digest"]:
        fail("release receipt archive API digest differs")
    output = ensure_output_directory(output)
    expected_files = receipt_files(descriptor)
    with zipfile.ZipFile(archive) as bundle:
        infos = bundle.infolist()
        if len(infos) != len(expected_files):
            fail("release receipt artifact inventory differs")
        names: list[str] = []
        for info in infos:
            name = info.filename
            path = PurePosixPath(name)
            if (
                name != path.as_posix()
                or path.is_absolute()
                or len(path.parts) != 1
                or name not in expected_files
                or info.is_dir()
                or info.file_size <= 0
                or info.file_size > MAX_JSON_BYTES
                or info.compress_size > MAX_ARCHIVE_BYTES
            ):
                fail("release receipt artifact entry is invalid")
            mode = (info.external_attr >> 16) & 0xFFFF
            if mode and not stat.S_ISREG(mode):
                fail("release receipt artifact entry is not a regular file")
            names.append(name)
        if sorted(names) != sorted(expected_files):
            fail("release receipt artifact inventory differs")
        for name in expected_files:
            payload = bundle.read(name)
            write_exclusive(output / name, payload, "release receipt artifact")


def validate_https_url(raw: Any, label: str) -> tuple[str, str, int]:
    value = exact_string(raw, label, maximum=2048)
    parsed = urlsplit(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.port not in (None, 443)
        or not parsed.path.startswith("/")
        or "//" in parsed.path
        or parsed.path.endswith("/") and parsed.path != "/"
    ):
        fail(f"{label} is not an exact HTTPS origin/path")
    try:
        host = parsed.hostname.encode("idna").decode("ascii").lower()
    except UnicodeError as exc:
        raise CanaryError(f"{label} hostname is invalid") from exc
    if host in {"localhost", "localhost.localdomain"} or host.endswith(".local"):
        fail(f"{label} hostname is forbidden")
    return host, parsed.path, 443


def resolve_public_addresses(host: str, port: int) -> list[str]:
    try:
        records = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    except OSError as exc:
        raise CanaryError("canary target DNS resolution failed") from exc
    addresses: set[str] = set()
    for record in records:
        raw = record[4][0]
        try:
            address = ipaddress.ip_address(raw)
        except ValueError as exc:
            raise CanaryError("canary target DNS answer is invalid") from exc
        if not address.is_global:
            fail("canary target resolved to a non-public address")
        addresses.add(address.compressed)
    if not addresses:
        fail("canary target has no public address")
    return sorted(addresses)


def secure_https_request(
    url: str,
    *,
    method: str,
    headers: Mapping[str, str] | None = None,
    body: bytes | None = None,
    maximum_body_bytes: int,
    retry_addresses: bool = True,
) -> tuple[int, Mapping[str, str], bytes]:
    host, path, port = validate_https_url(url, "canary target")
    addresses = resolve_public_addresses(host, port)
    request_headers = {
        "Host": host,
        "User-Agent": "ReReply-Production-Canary/1",
        "Accept": "application/json",
        "Connection": "close",
    }
    if headers:
        for key, value in headers.items():
            if not re.fullmatch(r"[A-Za-z0-9-]{1,64}", key) or any(
                character in value for character in "\r\n\x00"
            ):
                fail("canary request header is invalid")
            request_headers[key] = value
    if body is not None:
        request_headers["Content-Length"] = str(len(body))
    lines = [f"{method} {path} HTTP/1.1"]
    lines.extend(f"{key}: {value}" for key, value in request_headers.items())
    request = ("\r\n".join(lines) + "\r\n\r\n").encode("ascii") + (body or b"")

    context = ssl.create_default_context()
    last_error: OSError | ssl.SSLError | None = None
    candidates = addresses if retry_addresses else addresses[:1]
    for address in candidates:
        raw_socket: socket.socket | None = None
        tls_socket: ssl.SSLSocket | None = None
        try:
            raw_socket = socket.create_connection((address, port), timeout=15)
            tls_socket = context.wrap_socket(raw_socket, server_hostname=host)
            tls_socket.settimeout(20)
            tls_socket.sendall(request)
            response = HTTPResponse(tls_socket)
            response.begin()
            location = response.getheader("Location")
            if location is not None:
                fail("canary target redirects are forbidden")
            content_length = response.getheader("Content-Length")
            if content_length is not None:
                try:
                    declared = int(content_length, 10)
                except ValueError as exc:
                    raise CanaryError("canary response length is invalid") from exc
                if declared < 0 or declared > maximum_body_bytes:
                    fail("canary response is too large")
            payload = response.read(maximum_body_bytes + 1)
            if len(payload) > maximum_body_bytes:
                fail("canary response is too large")
            return response.status, {key.lower(): value for key, value in response.getheaders()}, payload
        except CanaryError:
            raise
        except (OSError, ssl.SSLError) as exc:
            last_error = exc
        finally:
            if tls_socket is not None:
                tls_socket.close()
            elif raw_socket is not None:
                raw_socket.close()
    raise CanaryError("canary HTTPS request failed") from last_error


def validate_targets(value: Any) -> tuple[dict[str, str], str]:
    config = exact_keys(value, {"schema_version", "endpoints"}, "public target config")
    if config["schema_version"] != 1:
        fail("public target config schema differs")
    endpoints = exact_keys(config["endpoints"], HEALTH_STATUSES, "public target endpoints")
    normalized: dict[str, str] = {}
    origins: set[str] = set()
    for label in sorted(HEALTH_STATUSES):
        raw = exact_string(endpoints[label], f"{label} URL", maximum=2048)
        host, path, _ = validate_https_url(raw, f"{label} URL")
        origins.add(host)
        normalized[label] = f"https://{host}{path}"
    if len(origins) != 1:
        fail("public canary endpoints must share one reviewed origin")
    contract = {"schema_version": 1, "endpoints": normalized}
    return normalized, sha256_bytes(canonical_payload_bytes(contract))


def parse_utc_timestamp(value: Any, label: str) -> dt.datetime:
    raw = exact_string(value, label)
    if not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", raw):
        fail(f"{label} is invalid")
    try:
        return dt.datetime.fromisoformat(raw[:-1] + "+00:00")
    except ValueError as exc:
        raise CanaryError(f"{label} is invalid") from exc


def run_health_probes(endpoints: Mapping[str, str]) -> dict[str, bool]:
    checks: dict[str, bool] = {}
    for label in sorted(HEALTH_STATUSES):
        status, _headers, _body = secure_https_request(
            endpoints[label],
            method="GET",
            maximum_body_bytes=MAX_HEALTH_BODY_BYTES,
        )
        if status != HEALTH_STATUSES[label]:
            fail("a production health endpoint returned an unexpected status")
        checks[label] = True
    return checks


def validate_driver_config(value: Any) -> dict[str, Any]:
    config = exact_keys(
        value,
        {"schema_version", "url", "driver_version_sha256", "hmac_key_base64"},
        "synthetic driver config",
    )
    if config["schema_version"] != 1:
        fail("synthetic driver config schema differs")
    url = exact_string(config["url"], "synthetic driver URL", maximum=2048)
    host, path, _ = validate_https_url(url, "synthetic driver URL")
    version = exact_string(config["driver_version_sha256"], "synthetic driver version", SHA256_RE)
    encoded_key = exact_string(config["hmac_key_base64"], "synthetic driver HMAC key", maximum=256)
    try:
        key = base64.b64decode(encoded_key, validate=True)
    except (ValueError, TypeError) as exc:
        raise CanaryError("synthetic driver HMAC key is invalid") from exc
    if len(key) != 32:
        fail("synthetic driver HMAC key length differs")
    normalized_url = f"https://{host}{path}"
    public_binding = {
        "schema_version": 1,
        "url": normalized_url,
        "driver_version_sha256": version,
    }
    return {
        "url": normalized_url,
        "driver_version_sha256": version,
        "driver_config_sha256": sha256_bytes(canonical_payload_bytes(public_binding)),
        "hmac_key": key,
    }


def run_ui_driver(
    config: Mapping[str, Any],
    *,
    control_sha: str,
    change_receipt_sha256: str,
    nonce: str,
    now: dt.datetime,
) -> tuple[dict[str, bool], dict[str, str]]:
    request_timestamp = now.astimezone(dt.timezone.utc).replace(microsecond=0).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    request_value = {
        "schema_version": 1,
        "authority": "rereply-controlled-synthetic-crm-request",
        "phase": "ui",
        "nonce": exact_string(nonce, "canary nonce", SHA256_RE),
        "idempotency_key": nonce,
        "control_sha": exact_string(control_sha, "control SHA", SHA1_RE),
        "change_receipt_sha256": exact_string(
            change_receipt_sha256, "change receipt hash", SHA256_RE
        ),
        "driver_version_sha256": config["driver_version_sha256"],
    }
    request_body = canonical_payload_bytes(request_value)
    request_signature = hmac.new(
        config["hmac_key"], request_body, hashlib.sha256
    ).hexdigest()
    status, headers, response_body = secure_https_request(
        config["url"],
        method="POST",
        headers={
            "Content-Type": "application/json",
            "X-ReReply-Canary-Timestamp": request_timestamp,
            "X-ReReply-Canary-Signature": request_signature,
        },
        body=request_body,
        maximum_body_bytes=MAX_DRIVER_BODY_BYTES,
        retry_addresses=False,
    )
    if status != 200:
        fail("synthetic CRM driver returned an unexpected status")
    content_type = headers.get("content-type", "").split(";", 1)[0].strip().lower()
    if content_type != "application/json":
        fail("synthetic CRM driver content type differs")
    response = exact_keys(
        loads_strict(response_body),
        {
            "schema_version",
            "authority",
            "phase",
            "nonce",
            "idempotency_key",
            "change_receipt_sha256",
            "driver_version_sha256",
            "observed_at",
            "execution_count",
            "checks",
            "hmac_sha256",
        },
        "synthetic CRM result",
    )
    if response["schema_version"] != 1 or response["authority"] != "rereply-controlled-synthetic-crm-result":
        fail("synthetic CRM result authority differs")
    if (
        response["phase"] != "ui"
        or response["nonce"] != nonce
        or response["idempotency_key"] != nonce
        or response["execution_count"] != 1
        or response["change_receipt_sha256"] != change_receipt_sha256
    ):
        fail("synthetic CRM result request binding differs")
    if response["driver_version_sha256"] != config["driver_version_sha256"]:
        fail("synthetic CRM driver version differs")
    observed_at = parse_utc_timestamp(response["observed_at"], "synthetic CRM observed_at")
    checked_at = now.astimezone(dt.timezone.utc).replace(microsecond=0)
    if abs((checked_at - observed_at).total_seconds()) > MAX_DRIVER_CLOCK_SKEW_SECONDS:
        fail("synthetic CRM result is stale or future-dated")
    signature = exact_string(response["hmac_sha256"], "synthetic CRM HMAC", SHA256_RE)
    signed = dict(response)
    del signed["hmac_sha256"]
    expected_signature = hmac.new(
        config["hmac_key"], canonical_payload_bytes(signed), hashlib.sha256
    ).hexdigest()
    if not hmac.compare_digest(signature, expected_signature):
        fail("synthetic CRM result authentication failed")
    checks = exact_keys(response["checks"], UI_CHECKS, "synthetic CRM checks")
    if any(value is not True for value in checks.values()):
        fail("one or more synthetic CRM checks failed")
    return (
        {key: True for key in UI_CHECKS},
        {
            "driver_version_sha256": config["driver_version_sha256"],
            "driver_config_sha256": config["driver_config_sha256"],
        },
    )


def receipt_phase(receipt: Mapping[str, Any]) -> str:
    lineage = receipt.get("lineage")
    if type(lineage) is not dict:
        fail("release receipt lineage is invalid")
    phase = lineage.get("to", lineage.get("phase"))
    if phase not in PHASES:
        fail("release receipt phase is invalid")
    return phase


def receipt_canary_contract(receipt: Mapping[str, Any]) -> str:
    value = receipt.get("canary")
    if type(value) is not dict or value.get("required") is not True or value.get("completed") is not False:
        fail("release receipt canary handoff differs")
    if value.get("endpoint_labels") != list(HEALTH_STATUSES):
        fail("release receipt endpoint label order differs")
    return exact_string(value.get("route_contract_sha256"), "route contract hash", SHA256_RE)


def receipt_release_assertions(receipt: Mapping[str, Any]) -> dict[str, bool]:
    before = receipt.get("before")
    after = receipt.get("after")
    gates = receipt.get("gates")
    transition = receipt.get("provider_transition")
    reconciliation = receipt.get("provider_observation")
    if not all(type(value) is dict for value in (before, after, gates)):
        fail("release receipt assertions are incomplete")
    if receipt.get("authority") == "production-orphan-reconciliation-receipt":
        classification = receipt.get("classification")
        if (
            type(classification) is not dict
            or classification.get("outcome") not in {"committed", "already-receipted"}
            or classification.get("canary_eligible") is not True
            or type(reconciliation) is not dict
            or reconciliation.get("http_methods_used") != ["GET"]
            or reconciliation.get("mutation_request_count") != 0
            or reconciliation.get("transition_absent") is not True
            or reconciliation.get("migration_succeeded") is not True
            or gates.get("deployment_succeeded") is not True
            or gates.get("migration_succeeded") is not True
        ):
            fail("orphan reconciliation is not eligible for canary")
    else:
        if gates != {"deployment_succeeded": True, "migration_succeeded": True}:
            fail("release receipt gates differ")
        if type(transition) is not dict or type(transition.get("ambiguous_reconciled")) is not bool:
            fail("apply transition reconciliation is invalid")
    preserved_hashes = (
        "app_identity_sha256",
        "default_ingress_sha256",
        "environment_values_sha256",
        "non_source_projection_sha256",
    )
    for key in preserved_hashes:
        if before.get(key) != after.get(key) or not isinstance(after.get(key), str):
            fail("release receipt reports production compatibility drift")
    after_images = after.get("images")
    if type(after_images) is not list or len(after_images) != 3:
        fail("release receipt image set differs")
    return {
        "digest_sources_verified": True,
        "topology_preserved": True,
        "routes_preserved": True,
        "environment_preserved": True,
        "migration_succeeded": True,
    }


def build_canary(
    receipt: Mapping[str, Any],
    descriptor: Mapping[str, Any],
    health: Mapping[str, bool],
    ui_checks: Mapping[str, bool] | None,
    driver_binding: Mapping[str, str] | None,
    *,
    route_contract_sha256: str,
    control_sha: str,
    run_id: str,
    run_attempt: int,
    completed_at: str,
) -> dict[str, Any]:
    phase = receipt_phase(receipt)
    if phase == "ui" and (ui_checks is None or driver_binding is None):
        fail("UI phase synthetic evidence is required")
    if phase != "ui" and (ui_checks is not None or driver_binding is not None):
        fail("synthetic CRM evidence is only valid for the UI phase")
    lineage = {
        "event_sequence": receipt["lineage"]["event_sequence"],
        "phase_ordinal": receipt["lineage"]["phase_ordinal"],
        "operation": receipt["lineage"]["operation"],
        "from": receipt["lineage"]["from"],
        "to": receipt["lineage"]["to"],
        "phase": phase,
        "phase_source_sha": receipt["lineage"]["phase_source_sha"],
        "receipt_kind": descriptor["receipt_kind"],
        "change_receipt_sha256": descriptor["receipt_sha256"],
    }
    if "release_reconciliation" in descriptor:
        lineage["main_lock_release_reconciliation_sha256"] = descriptor[
            "release_reconciliation"
        ]["sha256"]
    return {
        "schema_version": 1,
        "authority": "production-crm-canary",
        "repository": REPOSITORY,
        "completed_at": completed_at,
        "control": {
            "workflow_sha": control_sha,
            "workflow_path": WORKFLOW_PATH,
            "run_id": run_id,
            "run_attempt": run_attempt,
            "runner_environment": "github-hosted",
        },
        "lineage": lineage,
        "bindings": {
            "route_contract_sha256": exact_string(
                route_contract_sha256, "route contract hash", SHA256_RE
            ),
            "synthetic_driver": (
                {}
                if driver_binding is None
                else {
                    "driver_version_sha256": exact_string(
                        driver_binding["driver_version_sha256"], "driver version", SHA256_RE
                    ),
                    "driver_config_sha256": exact_string(
                        driver_binding["driver_config_sha256"], "driver config hash", SHA256_RE
                    ),
                }
            ),
        },
        "assertions": {
            "health": {key: health[key] for key in sorted(HEALTH_STATUSES)},
            "release": receipt_release_assertions(receipt),
            "crm": {
                "required": phase == "ui",
                "checks": {} if ui_checks is None else {key: ui_checks[key] for key in UI_CHECKS},
            },
        },
        "automatic_advance": False,
        "automatic_rollback": False,
    }


def build_phase_state(
    receipt: Mapping[str, Any],
    descriptor: Mapping[str, Any],
    *,
    release: ModuleType,
    canary_sha256: str,
    control_sha: str,
    run_id: str,
    run_attempt: int,
    completed_at: str,
    policy_sha256: str,
    schema_sha256: str,
) -> dict[str, Any]:
    builder = getattr(release, "build_phase_state", None)
    if not callable(builder):
        fail("shared phase-state builder is unavailable")
    try:
        extra_bindings: dict[str, Any] = {}
        if "release_reconciliation" in descriptor:
            policy = RECEIPT_KINDS[str(descriptor["receipt_kind"])]
            reconciliation = descriptor["release_reconciliation"]
            extra_bindings = {
                "change_receipt_binding": {
                    "run_id": descriptor["run_id"],
                    "run_attempt": descriptor["run_attempt"],
                    "artifact_id": descriptor["artifact_id"],
                    "artifact_name": f"{policy['artifact_prefix']}-{descriptor['run_id']}-1",
                    "artifact_digest": descriptor["artifact_digest"],
                    "sha256": descriptor["receipt_sha256"],
                },
                "main_lock_release_reconciliation_binding": {
                    "run_id": reconciliation["run_id"],
                    "run_attempt": reconciliation["run_attempt"],
                    "artifact_id": reconciliation["artifact_id"],
                    "artifact_name": f"production-main-lock-release-reconciliation-{reconciliation['run_id']}-1",
                    "artifact_digest": reconciliation["artifact_digest"],
                    "sha256": reconciliation["sha256"],
                },
            }
        state = builder(
            receipt,
            change_receipt_sha256=descriptor["receipt_sha256"],
            canary_sha256=canary_sha256,
            control={
                "workflow_sha": control_sha,
                "workflow_path": WORKFLOW_PATH,
                "run_id": run_id,
                "run_attempt": run_attempt,
                "runner_environment": "github-hosted",
                "release_policy_sha256": policy_sha256,
                "change_schema_sha256": schema_sha256,
            },
            completed_at=completed_at,
            **extra_bindings,
        )
    except Exception as exc:
        raise CanaryError("shared phase-state construction failed") from exc
    if type(state) is not dict:
        fail("shared phase-state builder returned an invalid value")
    return state


def now_utc() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0)


def reconciliation_paths(args: argparse.Namespace) -> tuple[Path | None, Path | None]:
    return (
        getattr(args, "reconciliation", None),
        getattr(args, "reconciliation_sha256", None),
    )


def normalize_input(args: argparse.Namespace) -> None:
    raw = os.environ.get(args.input_env)
    if raw is None or len(raw.encode("utf-8")) > MAX_JSON_BYTES:
        fail("release evidence input is missing or too large")
    descriptor = validate_release_descriptor(loads_strict(raw), args.expected_control_sha)
    output = ensure_output_directory(args.output_dir)
    write_exclusive(output / "release-evidence.json", canonical_file_bytes(descriptor), "release evidence")


def probe(args: argparse.Namespace) -> None:
    descriptor = validate_release_descriptor(
        load_canonical(args.descriptor, "release evidence descriptor"), args.expected_control_sha
    )
    reconciliation, reconciliation_sha256 = reconciliation_paths(args)
    receipt = validate_receipt(
        args.receipt,
        args.receipt_sha256,
        descriptor,
        args.control_root,
        reconciliation,
        reconciliation_sha256,
    )
    target_raw = os.environ.get(args.targets_env)
    if target_raw is None or len(target_raw.encode("utf-8")) > MAX_JSON_BYTES:
        fail("public target config is missing or too large")
    endpoints, route_contract = validate_targets(loads_strict(target_raw))
    if route_contract != receipt_canary_contract(receipt):
        fail("public route contract differs from the release receipt")
    health = run_health_probes(endpoints)

    checked_at = now_utc()
    ui_checks: dict[str, bool] | None = None
    driver_binding: dict[str, str] | None = None
    if receipt_phase(receipt) == "ui":
        driver_raw = os.environ.get(args.driver_env)
        if driver_raw is None or len(driver_raw.encode("utf-8")) > MAX_JSON_BYTES:
            fail("synthetic driver config is missing or too large")
        driver = validate_driver_config(loads_strict(driver_raw))
        nonce = hashlib.sha256(os.urandom(64)).hexdigest()
        ui_checks, driver_binding = run_ui_driver(
            driver,
            control_sha=args.expected_control_sha,
            change_receipt_sha256=descriptor["receipt_sha256"],
            nonce=nonce,
            now=checked_at,
        )
    elif os.environ.get(args.driver_env):
        fail("synthetic driver credentials must not be exposed before the UI phase")

    completed_at = checked_at.strftime("%Y-%m-%dT%H:%M:%SZ")
    canary = build_canary(
        receipt,
        descriptor,
        health,
        ui_checks,
        driver_binding,
        route_contract_sha256=route_contract,
        control_sha=args.expected_control_sha,
        run_id=args.run_id,
        run_attempt=args.run_attempt,
        completed_at=completed_at,
    )
    output = ensure_output_directory(args.output_dir)
    write_two_file_artifact(output, "production-crm-canary", canary)


def inspect_receipt(args: argparse.Namespace) -> None:
    descriptor = validate_release_descriptor(
        load_canonical(args.descriptor, "release evidence descriptor"), args.expected_control_sha
    )
    reconciliation, reconciliation_sha256 = reconciliation_paths(args)
    receipt = validate_receipt(
        args.receipt,
        args.receipt_sha256,
        descriptor,
        args.control_root,
        reconciliation,
        reconciliation_sha256,
    )
    receipt_canary_contract(receipt)
    print(receipt_phase(receipt))


def finalize(args: argparse.Namespace) -> None:
    descriptor = validate_release_descriptor(
        load_canonical(args.descriptor, "release evidence descriptor"), args.expected_control_sha
    )
    reconciliation, reconciliation_sha256 = reconciliation_paths(args)
    receipt = validate_receipt(
        args.receipt,
        args.receipt_sha256,
        descriptor,
        args.control_root,
        reconciliation,
        reconciliation_sha256,
    )
    canary = load_canonical(args.canary, "CRM canary")
    canary_digest = sha256_bytes(args.canary.read_bytes())
    validate_sidecar(args.canary_sha256, args.canary, canary_digest, "CRM canary")
    exact_keys(
        canary,
        {
            "schema_version",
            "authority",
            "repository",
            "completed_at",
            "control",
            "lineage",
            "bindings",
            "assertions",
            "automatic_advance",
            "automatic_rollback",
        },
        "CRM canary",
    )
    if (
        canary["schema_version"] != 1
        or canary["authority"] != "production-crm-canary"
        or canary["repository"] != REPOSITORY
        or canary["automatic_advance"] is not False
        or canary["automatic_rollback"] is not False
    ):
        fail("CRM canary authority differs")
    if canary["control"] != {
        "workflow_sha": args.expected_control_sha,
        "workflow_path": WORKFLOW_PATH,
        "run_id": args.run_id,
        "run_attempt": args.run_attempt,
        "runner_environment": "github-hosted",
    }:
        fail("CRM canary control binding differs")
    expected_lineage = {
        "event_sequence": receipt["lineage"]["event_sequence"],
        "phase_ordinal": receipt["lineage"]["phase_ordinal"],
        "operation": receipt["lineage"]["operation"],
        "from": receipt["lineage"]["from"],
        "to": receipt["lineage"]["to"],
        "phase": receipt_phase(receipt),
        "phase_source_sha": receipt["lineage"]["phase_source_sha"],
        "receipt_kind": descriptor["receipt_kind"],
        "change_receipt_sha256": descriptor["receipt_sha256"],
    }
    if "release_reconciliation" in descriptor:
        expected_lineage["main_lock_release_reconciliation_sha256"] = descriptor[
            "release_reconciliation"
        ]["sha256"]
    if canary["lineage"] != expected_lineage:
        fail("CRM canary lineage differs")
    bindings = exact_keys(
        canary["bindings"], {"route_contract_sha256", "synthetic_driver"}, "CRM bindings"
    )
    if bindings["route_contract_sha256"] != receipt_canary_contract(receipt):
        fail("CRM canary route binding differs")
    expected_driver = bindings["synthetic_driver"]
    if receipt_phase(receipt) == "ui":
        expected_driver = exact_keys(
            expected_driver,
            {"driver_version_sha256", "driver_config_sha256"},
            "CRM driver binding",
        )
        for key in ("driver_version_sha256", "driver_config_sha256"):
            exact_string(expected_driver[key], f"CRM {key}", SHA256_RE)
    elif expected_driver != {}:
        fail("CRM driver binding is forbidden before UI")
    assertions = exact_keys(canary["assertions"], {"health", "release", "crm"}, "CRM assertions")
    if assertions["health"] != {key: True for key in sorted(HEALTH_STATUSES)}:
        fail("CRM canary health assertions differ")
    if assertions["release"] != receipt_release_assertions(receipt):
        fail("CRM canary release assertions differ")
    expected_crm = (
        {"required": True, "checks": {key: True for key in UI_CHECKS}}
        if receipt_phase(receipt) == "ui"
        else {"required": False, "checks": {}}
    )
    if assertions["crm"] != expected_crm:
        fail("CRM canary synthetic assertions differ")

    if args.policy.is_symlink() or not args.policy.is_file():
        fail("production release policy is not a regular file")
    if args.schema.is_symlink() or not args.schema.is_file():
        fail("production change schema is not a regular file")
    policy_raw = args.policy.read_bytes()
    schema_raw = args.schema.read_bytes()
    if not policy_raw or not schema_raw or len(policy_raw) > MAX_JSON_BYTES or len(schema_raw) > MAX_JSON_BYTES:
        fail("production policy inputs are empty")
    policy_sha256 = sha256_bytes(policy_raw)
    schema_sha256 = sha256_bytes(schema_raw)
    completed_at = now_utc().strftime("%Y-%m-%dT%H:%M:%SZ")
    release = load_phase_state_release_module(
        args.control_root, descriptor["receipt_kind"]
    )
    state = build_phase_state(
        receipt,
        descriptor,
        release=release,
        canary_sha256=canary_digest,
        control_sha=args.expected_control_sha,
        run_id=args.run_id,
        run_attempt=args.run_attempt,
        completed_at=completed_at,
        policy_sha256=policy_sha256,
        schema_sha256=schema_sha256,
    )
    try:
        release.validate_phase_state(state, now=now_utc())
    except Exception as exc:
        raise CanaryError("final production phase state validation failed") from exc
    output = ensure_output_directory(args.output_dir)
    write_two_file_artifact(output, "production-phase-state", state)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)

    normalize = commands.add_parser("normalize-input")
    normalize.add_argument("--input-env", required=True)
    normalize.add_argument("--expected-control-sha", required=True)
    normalize.add_argument("--output-dir", type=Path, required=True)

    extract = commands.add_parser("extract-receipt")
    extract.add_argument("--archive", type=Path, required=True)
    extract.add_argument("--descriptor", type=Path, required=True)
    extract.add_argument("--expected-control-sha", required=True)
    extract.add_argument("--output-dir", type=Path, required=True)

    extract_reconciliation = commands.add_parser("extract-reconciliation")
    extract_reconciliation.add_argument("--archive", type=Path, required=True)
    extract_reconciliation.add_argument("--descriptor", type=Path, required=True)
    extract_reconciliation.add_argument("--expected-control-sha", required=True)
    extract_reconciliation.add_argument("--output-dir", type=Path, required=True)

    inspect = commands.add_parser("inspect-receipt")
    inspect.add_argument("--descriptor", type=Path, required=True)
    inspect.add_argument("--receipt", type=Path, required=True)
    inspect.add_argument("--receipt-sha256", type=Path, required=True)
    inspect.add_argument("--control-root", type=Path, required=True)
    inspect.add_argument("--expected-control-sha", required=True)
    inspect.add_argument("--reconciliation", type=Path)
    inspect.add_argument("--reconciliation-sha256", type=Path)

    observe = commands.add_parser("probe")
    observe.add_argument("--descriptor", type=Path, required=True)
    observe.add_argument("--receipt", type=Path, required=True)
    observe.add_argument("--receipt-sha256", type=Path, required=True)
    observe.add_argument("--control-root", type=Path, required=True)
    observe.add_argument("--expected-control-sha", required=True)
    observe.add_argument("--targets-env", required=True)
    observe.add_argument("--driver-env", required=True)
    observe.add_argument("--run-id", required=True)
    observe.add_argument("--run-attempt", type=int, required=True)
    observe.add_argument("--output-dir", type=Path, required=True)
    observe.add_argument("--reconciliation", type=Path)
    observe.add_argument("--reconciliation-sha256", type=Path)

    finish = commands.add_parser("finalize")
    finish.add_argument("--descriptor", type=Path, required=True)
    finish.add_argument("--receipt", type=Path, required=True)
    finish.add_argument("--receipt-sha256", type=Path, required=True)
    finish.add_argument("--canary", type=Path, required=True)
    finish.add_argument("--canary-sha256", type=Path, required=True)
    finish.add_argument("--control-root", type=Path, required=True)
    finish.add_argument("--policy", type=Path, required=True)
    finish.add_argument("--schema", type=Path, required=True)
    finish.add_argument("--expected-control-sha", required=True)
    finish.add_argument("--run-id", required=True)
    finish.add_argument("--run-attempt", type=int, required=True)
    finish.add_argument("--output-dir", type=Path, required=True)
    finish.add_argument("--reconciliation", type=Path)
    finish.add_argument("--reconciliation-sha256", type=Path)
    return root


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = parser().parse_args(arguments)
    if args.command == "normalize-input":
        normalize_input(args)
    elif args.command == "extract-receipt":
        descriptor = validate_release_descriptor(
            load_canonical(args.descriptor, "release evidence descriptor"), args.expected_control_sha
        )
        validate_archive(args.archive, descriptor, args.output_dir)
    elif args.command == "extract-reconciliation":
        descriptor = validate_release_descriptor(
            load_canonical(args.descriptor, "release evidence descriptor"),
            args.expected_control_sha,
        )
        validate_reconciliation_archive(args.archive, descriptor, args.output_dir)
    elif args.command == "inspect-receipt":
        inspect_receipt(args)
    elif args.command == "probe":
        probe(args)
    elif args.command == "finalize":
        finalize(args)
    else:  # pragma: no cover - argparse prevents this branch.
        fail("unsupported command")
    return 0


def main() -> int:
    try:
        return run_cli()
    except CanaryError as exc:
        print(f"canary verification failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
