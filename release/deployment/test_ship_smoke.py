from __future__ import annotations

import io
import socket
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import smoke
import test_ship_support as support


class FakeSocket:
    """A connected TLS socket whose peer replies with fixed bytes."""

    def __init__(self, response: bytes) -> None:
        self.response = response
        self.sent = b""
        self.closed = False

    def settimeout(self, value: float) -> None:
        del value

    def sendall(self, data: bytes) -> None:
        self.sent += data

    def makefile(self, mode: str, *args: object, **kwargs: object) -> io.BytesIO:
        del mode, args, kwargs
        return io.BytesIO(self.response)

    def close(self) -> None:
        self.closed = True


class FakeContext:
    def __init__(self, sock: FakeSocket) -> None:
        self.sock = sock
        self.server_hostname: str | None = None

    def wrap_socket(self, raw: object, server_hostname: str) -> FakeSocket:
        del raw
        self.server_hostname = server_hostname
        return self.sock


def addresses(*values: str):
    def getaddrinfo(host: str, port: int, type: int = 0):
        del host, type
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (value, port)) for value in values]
    return getaddrinfo


class OriginTests(unittest.TestCase):
    def test_origin_must_match_the_committed_hash(self) -> None:
        origin, host = smoke.canonical_origin(support.INGRESS, support.sha256_text(support.INGRESS))
        self.assertEqual(origin, support.INGRESS)
        self.assertEqual(host, "rereply-fake.example.invalid")
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:default-ingress$"):
            smoke.canonical_origin(support.INGRESS, "0" * 64)

    def test_non_canonical_origins_are_refused_even_with_a_matching_hash(self) -> None:
        for value in (
            "http://rereply-fake.example.invalid",
            "https://rereply-fake.example.invalid/",
            "https://RErePLY-fake.example.invalid",
            "https://user@rereply-fake.example.invalid",
            "https://rereply-fake.example.invalid:8443",
            "https://rereply-fake.example.invalid/path",
            "https://rereply-fake.example.invalid?x=1",
        ):
            with self.subTest(value=value), self.assertRaisesRegex(common.ReleaseError, "^topology-differs:default-ingress$"):
                smoke.canonical_origin(value, support.sha256_text(value))


class HealthTests(unittest.TestCase):
    def test_six_probes_with_exact_statuses(self) -> None:
        fake = support.FakeHttps()
        self.assertEqual(smoke.run_health(support.INGRESS, request=fake, rounds=1, delay=0, sleeper=lambda _: None), {"health": "6/6"})
        self.assertEqual(sorted(url[len(support.INGRESS):] for url in fake.calls),
                         sorted(path for _label, path, _status in smoke.HEALTH))

    def test_wrong_status_fails_after_all_rounds(self) -> None:
        calls: list[float] = []

        def request(url: str, **kwargs: object) -> tuple[int, dict, bytes]:
            del kwargs
            return (200 if url.endswith("/meta-relay/livez") else support.FakeHttps().expected[url[len(support.INGRESS):]]), {}, b""

        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed$"):
            smoke.run_health(support.INGRESS, request=request, rounds=3, delay=7, sleeper=calls.append)
        self.assertEqual(calls, [7, 7])

    def test_retry_rounds_recover(self) -> None:
        fake = support.FakeHttps(script=[False, False, True])
        self.assertEqual(smoke.run_health(support.INGRESS, request=fake, rounds=3, delay=0, sleeper=lambda _: None), {"health": "6/6"})

    def test_probe_errors_count_as_failures(self) -> None:
        def request(url: str, **kwargs: object) -> tuple[int, dict, bytes]:
            raise common.ReleaseError("smoke-failed:connect")

        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed$"):
            smoke.run_health(support.INGRESS, request=request, rounds=2, delay=0, sleeper=lambda _: None)


class SecureRequestTests(unittest.TestCase):
    def request(self, response: bytes, *, ips: tuple[str, ...] = ("93.184.216.34",), url: str | None = None):
        sock = FakeSocket(response)
        context = FakeContext(sock)
        connections: list[tuple] = []

        def create_connection(address: tuple, timeout: float) -> FakeSocket:
            del timeout
            connections.append(address)
            return sock

        result = smoke.secure_https_request(
            url or support.INGRESS + "/health",
            getaddrinfo=addresses(*ips),
            create_connection=create_connection,
            context_factory=lambda: context,
        )
        return result, sock, context, connections

    def test_public_address_tls_and_bounded_body(self) -> None:
        (status, headers, body), sock, context, connections = self.request(
            b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
        )
        self.assertEqual((status, body), (200, b"ok"))
        self.assertEqual(context.server_hostname, "rereply-fake.example.invalid")
        self.assertEqual(connections, [("93.184.216.34", 443)])
        self.assertIn(b"GET /health HTTP/1.1\r\nHost: rereply-fake.example.invalid\r\n", sock.sent)
        self.assertIn(b"User-Agent: ReReply-Ship-Smoke/1", sock.sent)
        self.assertTrue(sock.closed)
        self.assertEqual(headers["content-length"], "2")

    def test_non_public_addresses_are_refused(self) -> None:
        for ip in ("10.0.0.1", "127.0.0.1", "169.254.169.254", "192.168.1.1", "::1"):
            with self.subTest(ip=ip), self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:non-public-address$"):
                self.request(b"HTTP/1.1 200 OK\r\n\r\n", ips=("93.184.216.34", ip))

    def test_redirects_and_oversized_bodies_are_refused(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:redirect$"):
            self.request(b"HTTP/1.1 302 Found\r\nLocation: https://example.invalid/\r\nContent-Length: 0\r\n\r\n")
        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:length$"):
            self.request(b"HTTP/1.1 200 OK\r\nContent-Length: 999999\r\n\r\n")
        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:length$"):
            self.request(b"HTTP/1.1 200 OK\r\n\r\n" + b"x" * 5000)

    def test_url_validation(self) -> None:
        for url in ("http://example.invalid/health", "https://localhost/health", "https://example.invalid/a//b",
                    "https://example.invalid/health?x=1", "https://u:p@example.invalid/health", "https://x.local/health"):
            with self.subTest(url=url), self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:url$"):
                smoke.validate_https_url(url)

    def test_dns_failure(self) -> None:
        def failing(host: str, port: int, type: int = 0):
            raise OSError("no dns")

        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:dns$"):
            smoke.resolve_public_addresses("example.invalid", 443, getaddrinfo=failing)


if __name__ == "__main__":
    unittest.main()
