#!/usr/bin/env python3
"""The six public health probes that gate every ship.yml deploy and rollback.

The endpoint set and statuses are verify_production_crm_canary.py:114-121.
The HTTPS client (public addresses only, TLS verified, no redirects, bounded
body and time) is copied from verify_production_crm_canary.py:586-726. The
probe origin is the live app's default ingress, bound to the committed hash
in ship-target.json: probing that ingress is the reviewed route contract
(apply_production_change.py:1371-1374), so no public-origin secret is needed.
"""

from __future__ import annotations

import ipaddress
import re
import socket
import ssl
import sys
import time
import urllib.parse
from http.client import HTTPResponse
from pathlib import Path
from typing import Any, Callable, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common


HEALTH = (
    ("app-health", "/health", 200),
    ("app-ready", "/ready", 200),
    ("meta-live", "/meta-relay/livez", 204),
    ("meta-ready", "/meta-relay/readyz", 204),
    ("gmail-live", "/gmail-relay/livez", 204),
    ("gmail-ready", "/gmail-relay/readyz", 204),
)
MAX_HEALTH_BODY_BYTES = 4096
SOCKET_TIMEOUT_SECONDS = 20
ROUNDS = 6
ROUND_DELAY_SECONDS = 10
USER_AGENT = "ReReply-Ship-Smoke/1"


def canonical_origin(default_ingress: Any, expected_sha256: str) -> tuple[str, str]:
    """Return (origin, host) when the exact ingress string matches the
    committed hash and is a canonical HTTPS origin (validate_target_descriptor,
    verify_production_release.py:417-433)."""
    if type(default_ingress) is not str or common.sha256_text(default_ingress) != expected_sha256:
        common.fail("topology-differs:default-ingress")
    parsed = urllib.parse.urlsplit(default_ingress)
    try:
        port = parsed.port
    except ValueError as exc:
        raise common.ReleaseError("topology-differs:default-ingress") from exc
    if (
        parsed.scheme != "https"
        or parsed.username is not None
        or parsed.password is not None
        or port not in (None, 443)
        or not parsed.hostname
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        common.fail("topology-differs:default-ingress")
    host = parsed.hostname.lower()
    origin = f"https://{host}"
    if default_ingress != origin:
        common.fail("topology-differs:default-ingress")
    return origin, host


def validate_https_url(raw: Any) -> tuple[str, str, int]:
    value = common.exact_string(raw, "smoke-failed:url")
    if len(value) > 2048:
        common.fail("smoke-failed:url")
    parsed = urllib.parse.urlsplit(value)
    try:
        port = parsed.port
    except ValueError as exc:
        raise common.ReleaseError("smoke-failed:url") from exc
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or port not in (None, 443)
        or not parsed.path.startswith("/")
        or "//" in parsed.path
        or (parsed.path.endswith("/") and parsed.path != "/")
    ):
        common.fail("smoke-failed:url")
    try:
        host = parsed.hostname.encode("idna").decode("ascii").lower()
    except UnicodeError as exc:
        raise common.ReleaseError("smoke-failed:url") from exc
    if host in {"localhost", "localhost.localdomain"} or host.endswith(".local"):
        common.fail("smoke-failed:url")
    return host, parsed.path, 443


def resolve_public_addresses(
    host: str, port: int, *, getaddrinfo: Callable[..., Any] = socket.getaddrinfo
) -> list[str]:
    try:
        records = getaddrinfo(host, port, type=socket.SOCK_STREAM)
    except OSError as exc:
        raise common.ReleaseError("smoke-failed:dns") from exc
    addresses: set[str] = set()
    for record in records:
        raw = record[4][0]
        try:
            address = ipaddress.ip_address(raw)
        except ValueError as exc:
            raise common.ReleaseError("smoke-failed:dns") from exc
        if not address.is_global:
            common.fail("smoke-failed:non-public-address")
        addresses.add(address.compressed)
    if not addresses:
        common.fail("smoke-failed:dns")
    return sorted(addresses)


def secure_https_request(
    url: str,
    *,
    method: str = "GET",
    maximum_body_bytes: int = MAX_HEALTH_BODY_BYTES,
    socket_timeout_seconds: int = SOCKET_TIMEOUT_SECONDS,
    getaddrinfo: Callable[..., Any] = socket.getaddrinfo,
    create_connection: Callable[..., Any] = socket.create_connection,
    context_factory: Callable[[], Any] = ssl.create_default_context,
    monotonic: Callable[[], float] = time.monotonic,
) -> tuple[int, Mapping[str, str], bytes]:
    if type(socket_timeout_seconds) is not int or not 1 <= socket_timeout_seconds <= 60:
        common.fail("internal-error:smoke-timeout")
    if method not in {"GET", "HEAD"}:
        common.fail("internal-error:smoke-method")
    host, path, port = validate_https_url(url)
    addresses = resolve_public_addresses(host, port, getaddrinfo=getaddrinfo)
    deadline = monotonic() + socket_timeout_seconds

    def remaining_time() -> float:
        remaining = deadline - monotonic()
        if remaining <= 0:
            common.fail("smoke-failed:deadline")
        return remaining

    request_headers = {
        "Host": host,
        "User-Agent": USER_AGENT,
        "Accept": "application/json",
        "Connection": "close",
    }
    lines = [f"{method} {path} HTTP/1.1"]
    lines.extend(f"{key}: {value}" for key, value in request_headers.items())
    request = ("\r\n".join(lines) + "\r\n\r\n").encode("ascii")
    context = context_factory()
    last_error: BaseException | None = None
    for address in addresses:
        raw_socket: Any = None
        tls_socket: Any = None
        try:
            raw_socket = create_connection((address, port), timeout=min(15, remaining_time()))
            raw_socket.settimeout(remaining_time())
            tls_socket = context.wrap_socket(raw_socket, server_hostname=host)
            tls_socket.settimeout(remaining_time())
            tls_socket.sendall(request)
            response = HTTPResponse(tls_socket)
            tls_socket.settimeout(remaining_time())
            response.begin()
            if response.getheader("Location") is not None:
                common.fail("smoke-failed:redirect")
            content_length = response.getheader("Content-Length")
            if content_length is not None:
                if re.fullmatch(r"[0-9]{1,12}", content_length) is None:
                    common.fail("smoke-failed:length")
                if int(content_length) > maximum_body_bytes:
                    common.fail("smoke-failed:length")
            chunks: list[bytes] = []
            total = 0
            while True:
                tls_socket.settimeout(remaining_time())
                chunk = response.read(min(65_536, maximum_body_bytes + 1 - total))
                if not chunk:
                    break
                total += len(chunk)
                if total > maximum_body_bytes:
                    common.fail("smoke-failed:length")
                chunks.append(chunk)
            return (
                response.status,
                {key.lower(): value for key, value in response.getheaders()},
                b"".join(chunks),
            )
        except common.ReleaseError:
            raise
        except (OSError, ssl.SSLError, ValueError) as exc:
            last_error = exc
        except Exception as exc:  # http.client parse errors on a hostile peer
            last_error = exc
        finally:
            if tls_socket is not None:
                tls_socket.close()
            elif raw_socket is not None:
                raw_socket.close()
    raise common.ReleaseError("smoke-failed:connect") from last_error


def run_health(
    origin: str,
    *,
    request: Callable[..., tuple[int, Mapping[str, str], bytes]] = secure_https_request,
    rounds: int = ROUNDS,
    delay: float = ROUND_DELAY_SECONDS,
    sleeper: Callable[[float], None] = time.sleep,
) -> dict[str, str]:
    """Pass only when all six probes return their exact status in one round."""
    if type(origin) is not str or not origin.startswith("https://") or origin.endswith("/"):
        common.fail("internal-error:smoke-origin")
    for attempt in range(rounds):
        passed = 0
        for _label, path, expected in HEALTH:
            try:
                status, _headers, _body = request(origin + path)
            except common.ReleaseError:
                break
            if status != expected:
                break
            passed += 1
        if passed == len(HEALTH):
            return {"health": f"{passed}/{len(HEALTH)}"}
        if attempt + 1 < rounds:
            sleeper(delay)
    common.fail("smoke-failed")
