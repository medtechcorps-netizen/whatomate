from __future__ import annotations

import base64
import copy
import datetime as dt
import hashlib
import hmac
import importlib.util
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("verify_production_crm_canary.py")
SPEC = importlib.util.spec_from_file_location("verify_production_crm_canary", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
canary = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(canary)

release = canary.load_control_module(
    MODULE_PATH.parents[2],
    "release/deployment/verify_production_release.py",
    "verify_production_release",
)


def digest(character: str) -> str:
    return character * 64


def image(component: str, character: str) -> dict[str, str]:
    repository = f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
    value = f"sha256:{digest(character)}"
    return {
        "component": component,
        "repository": repository,
        "digest": value,
        "subject": f"{repository}@{value}",
    }


def provider_state(*, legacy: bool, changed: bool = False) -> dict[str, object]:
    return {
        "app_identity_sha256": digest("a"),
        "default_ingress_sha256": digest("b"),
        "app_updated_at_sha256": digest("d" if changed else "c"),
        "active_deployment_identity_sha256": digest("f" if changed else "e"),
        "canonical_spec_sha256": digest("2" if changed else "1"),
        "environment_values_sha256": digest("3"),
        "non_source_projection_sha256": digest("4"),
        "source_mode": "legacy-git" if legacy else "digest-images",
        "images": [] if legacy else [image("web", "5"), image("meta-relay", "6"), image("gmail-relay", "7")],
    }


def binding(seed: int, character: str) -> dict[str, object]:
    return {
        "run_id": str(seed),
        "run_attempt": 1,
        "artifact_id": str(seed + 100),
        "artifact_digest": f"sha256:{digest(character)}",
        "sha256": digest(character),
    }


def full_binding(seed: int, character: str, name: str) -> dict[str, object]:
    return {**binding(seed, character), "artifact_name": name}


def receipt(phase: str = "baseline", route_hash: str | None = None) -> dict[str, object]:
    index = canary.PHASES.index(phase)
    predecessor = "genesis" if index == 0 else canary.PHASES[index - 1]
    floors = {
        "baseline": {"allowed_targets": [], "forbidden_targets": []},
        "bridge": {"allowed_targets": ["baseline"], "forbidden_targets": []},
        "backend": {"allowed_targets": ["bridge"], "forbidden_targets": ["baseline"]},
        "ui": {"allowed_targets": ["backend", "bridge"], "forbidden_targets": ["baseline"]},
    }
    return {
        "schema_version": 1,
        "authority": "production-phase-apply-receipt",
        "repository": canary.REPOSITORY,
        "completed_at": "2026-08-27T01:00:00Z",
        "control": {
            "workflow_sha": "a" * 40,
            "workflow_path": canary.RECEIPT_KINDS["apply"]["workflow_path"],
            "run_id": "1001",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": digest("8"),
            "change_schema_sha256": digest("9"),
            "controller_sha256": digest("a"),
        },
        "lineage": {
            "event_sequence": index + 1,
            "phase_ordinal": index + 1,
            "operation": "activate",
            "predecessor_state_sha256": digest("b"),
            "predecessor_kind": "genesis" if index == 0 else "phase-state",
            "from": predecessor,
            "to": phase,
            "phase": phase,
            "phase_source_sha": "c" * 40,
        },
        "authorities": {
            "rollout_plan_sha256": digest("c"),
            "production_plan": binding(2001, "d"),
            "recovery": binding(3001, "e"),
            "mutation_intent": full_binding(
                1001, "f", "production-mutation-intent-apply-1001-1"
            ),
            "main_lock_proof": full_binding(
                1001, "e", "production-main-lock-proof-apply-1001-1"
            ),
        },
        "provider_transition": {
            "http_methods_used": ["GET", "PUT"],
            "http_request_count": 11,
            "mutation_request_count": 1,
            "endpoint_labels": ["app", "deployment"],
            "mutation_fingerprint_sha256": digest("f"),
            "ambiguous_reconciled": False,
        },
        "before": provider_state(legacy=phase == "baseline"),
        "after": provider_state(legacy=False, changed=True),
        "gates": {"deployment_succeeded": True, "migration_succeeded": True},
        "rollback": floors[phase],
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": list(canary.HEALTH_STATUSES),
            "route_contract_sha256": route_hash or digest("0"),
        },
    }


def descriptor(receipt_hash: str = digest("0")) -> dict[str, object]:
    return {
        "control_sha": "a" * 40,
        "receipt_kind": "apply",
        "run_id": "1001",
        "run_attempt": 1,
        "artifact_id": "1002",
        "artifact_digest": f"sha256:{digest('1')}",
        "receipt_sha256": receipt_hash,
    }


def reconciled_descriptor(
    receipt_hash: str = digest("0"), reconciliation_hash: str = digest("2")
) -> dict[str, object]:
    value = descriptor(receipt_hash)
    value["receipt_kind"] = "apply-reconciled"
    value["release_reconciliation"] = {
        "run_id": "9001",
        "run_attempt": 1,
        "artifact_id": "9002",
        "artifact_digest": f"sha256:{digest('3')}",
        "sha256": reconciliation_hash,
    }
    return value


def rollback_receipt() -> dict[str, object]:
    value = receipt("backend")
    value["authority"] = "production-phase-rollback-receipt"
    value["control"]["workflow_path"] = canary.RECEIPT_KINDS["rollback"]["workflow_path"]  # type: ignore[index]
    value["lineage"] = {
        "event_sequence": 5,
        "phase_ordinal": 3,
        "operation": "rollback",
        "from": "ui",
        "to": "backend",
        "predecessor_kind": "phase-state",
        "predecessor_state_sha256": digest("b"),
        "phase": "backend",
        "phase_source_sha": "c" * 40,
    }
    value["authorities"] = {
        "rollout_plan_sha256": digest("c"),
        "current_state": {"kind": "phase-state", **binding(2001, "b")},
        "target_state": binding(3001, "c"),
        "recovery": binding(4001, "d"),
        "mutation_intent": full_binding(
            1001, "f", "production-mutation-intent-rollback-1001-1"
        ),
        "main_lock_proof": full_binding(
            1001, "e", "production-main-lock-proof-rollback-1001-1"
        ),
    }
    value["target_authority"] = {"production_plan_sha256": digest("e")}
    value["before"] = provider_state(legacy=False)
    value["after"] = provider_state(legacy=False, changed=True)
    return value


class StrictInputTests(unittest.TestCase):
    def test_duplicate_json_key_is_rejected(self) -> None:
        with self.assertRaises(canary.CanaryError):
            canary.loads_strict('{"a":1,"a":2}')

    def test_apply_descriptor_is_exact_and_control_bound(self) -> None:
        value = descriptor()
        self.assertEqual(canary.validate_release_descriptor(value, "a" * 40)["run_id"], "1001")
        extra = dict(value, extra=True)
        with self.assertRaises(canary.CanaryError):
            canary.validate_release_descriptor(extra, "a" * 40)
        with self.assertRaises(canary.CanaryError):
            canary.validate_release_descriptor(value, "b" * 40)

    def test_reconciled_descriptor_requires_exact_paired_binding(self) -> None:
        value = reconciled_descriptor()
        normalized = canary.validate_release_descriptor(value, "a" * 40)
        self.assertEqual(normalized["release_reconciliation"]["run_id"], "9001")
        missing = copy.deepcopy(value)
        missing.pop("release_reconciliation")
        with self.assertRaises(canary.CanaryError):
            canary.validate_release_descriptor(missing, "a" * 40)
        direct = descriptor()
        direct["release_reconciliation"] = value["release_reconciliation"]
        with self.assertRaises(canary.CanaryError):
            canary.validate_release_descriptor(direct, "a" * 40)

    def test_public_targets_are_exact_https_and_hash_bound(self) -> None:
        endpoints = {
            label: f"https://crm.example.com/canary/{label}" for label in canary.HEALTH_STATUSES
        }
        normalized, route_hash = canary.validate_targets({"schema_version": 1, "endpoints": endpoints})
        self.assertEqual(normalized, endpoints)
        self.assertRegex(route_hash, r"^[0-9a-f]{64}$")
        private = copy.deepcopy(endpoints)
        private["app-health"] = "http://127.0.0.1/health"
        with self.assertRaises(canary.CanaryError):
            canary.validate_targets({"schema_version": 1, "endpoints": private})

    def test_dns_resolution_rejects_private_answers(self) -> None:
        answer = [(2, 1, 6, "", ("127.0.0.1", 443))]
        with mock.patch.object(canary.socket, "getaddrinfo", return_value=answer):
            with self.assertRaises(canary.CanaryError):
                canary.resolve_public_addresses("crm.example.com", 443)


class DriverTests(unittest.TestCase):
    def driver_config(self) -> dict[str, object]:
        return {
            "url": "https://synthetic.example.com/run/ui",
            "driver_version_sha256": digest("a"),
            "fixture_descriptor_sha256": digest("f"),
            "driver_config_sha256": digest("d"),
            "hmac_key": b"k" * 32,
        }

    def response(
        self,
        nonce: str,
        observed_at: str,
        checks: dict[str, bool] | None = None,
        *,
        execution_count: int = 1,
        fixture_descriptor_sha256: str | None = None,
    ) -> bytes:
        value: dict[str, object] = {
            "schema_version": 1,
            "authority": "rereply-controlled-synthetic-crm-result",
            "phase": "ui",
            "nonce": nonce,
            "idempotency_key": nonce,
            "change_receipt_sha256": digest("c"),
            "driver_version_sha256": digest("a"),
            "fixture_descriptor_sha256": (
                fixture_descriptor_sha256 or digest("f")
            ),
            "observed_at": observed_at,
            "execution_count": execution_count,
            "checks": checks or {name: True for name in canary.UI_CHECKS},
        }
        value["hmac_sha256"] = hmac.new(
            b"k" * 32, canary.canonical_payload_bytes(value), hashlib.sha256
        ).hexdigest()
        return canary.canonical_payload_bytes(value)

    def test_request_hmac_has_exact_cross_language_timestamp_vector(self) -> None:
        key = bytes(range(32))
        timestamp = "2026-08-30T01:02:03Z"
        body = b'{"schema_version":1}'
        self.assertEqual(
            hmac.new(
                key,
                canary.driver_request_hmac_payload(timestamp, body),
                hashlib.sha256,
            ).hexdigest(),
            "cb84422660014e889aa1de606126338f4eab813db4bc5355c8ec29cb7e239a37",
        )

    def test_ui_driver_requires_nonce_hmac_freshness_and_every_check(self) -> None:
        nonce = digest("b")
        now = dt.datetime(2026, 8, 27, 1, 0, tzinfo=dt.timezone.utc)
        response = self.response(nonce, "2026-08-27T01:00:00Z")
        with mock.patch.object(
            canary,
            "secure_https_request",
            return_value=(200, {"content-type": "application/json"}, response),
        ) as request:
            result, driver = canary.run_ui_driver(
                self.driver_config(), control_sha="a" * 40,
                change_receipt_sha256=digest("c"), nonce=nonce, now=now,
            )
        self.assertEqual(result, {name: True for name in canary.UI_CHECKS})
        self.assertEqual(driver["driver_version_sha256"], digest("a"))
        self.assertEqual(driver["fixture_descriptor_sha256"], digest("f"))
        self.assertFalse(request.call_args.kwargs["retry_addresses"])
        self.assertEqual(
            request.call_args.kwargs["socket_timeout_seconds"],
            canary.DRIVER_SOCKET_TIMEOUT_SECONDS,
        )
        sent = canary.loads_strict(request.call_args.kwargs["body"])
        self.assertEqual(sent["idempotency_key"], nonce)
        self.assertEqual(sent["change_receipt_sha256"], digest("c"))
        self.assertEqual(sent["fixture_descriptor_sha256"], digest("f"))
        headers = request.call_args.kwargs["headers"]
        self.assertNotIn("Authorization", headers)
        self.assertRegex(headers["X-ReReply-Canary-Signature"], r"^[0-9a-f]{64}$")
        self.assertEqual(
            headers["X-ReReply-Canary-Signature"],
            hmac.new(
                b"k" * 32,
                canary.driver_request_hmac_payload(
                    headers["X-ReReply-Canary-Timestamp"],
                    request.call_args.kwargs["body"],
                ),
                hashlib.sha256,
            ).hexdigest(),
        )

        failed = {name: True for name in canary.UI_CHECKS}
        failed["cross_organization_send_denied"] = False
        response = self.response(nonce, "2026-08-27T01:00:00Z", failed)
        with mock.patch.object(
            canary,
            "secure_https_request",
            return_value=(200, {"content-type": "application/json"}, response),
        ):
            with self.assertRaises(canary.CanaryError):
                canary.run_ui_driver(
                    self.driver_config(), control_sha="a" * 40,
                    change_receipt_sha256=digest("c"), nonce=nonce, now=now,
                )

        mismatched_fixture = self.response(
            nonce,
            "2026-08-27T01:00:00Z",
            fixture_descriptor_sha256=digest("e"),
        )
        with mock.patch.object(
            canary,
            "secure_https_request",
            return_value=(
                200,
                {"content-type": "application/json"},
                mismatched_fixture,
            ),
        ):
            with self.assertRaises(canary.CanaryError):
                canary.run_ui_driver(
                    self.driver_config(),
                    control_sha="a" * 40,
                    change_receipt_sha256=digest("c"),
                    nonce=nonce,
                    now=now,
                )

        replayed = self.response(
            nonce, "2026-08-27T01:00:00Z", execution_count=2
        )
        with mock.patch.object(
            canary,
            "secure_https_request",
            return_value=(200, {"content-type": "application/json"}, replayed),
        ):
            with self.assertRaises(canary.CanaryError):
                canary.run_ui_driver(
                    self.driver_config(), control_sha="a" * 40,
                    change_receipt_sha256=digest("c"), nonce=nonce, now=now,
                )

    def test_driver_timeout_is_bounded_and_health_probes_keep_the_default(self) -> None:
        endpoints = {
            label: f"https://crm.example.com/canary/{label}"
            for label in canary.HEALTH_STATUSES
        }
        with mock.patch.object(
            canary,
            "secure_https_request",
            side_effect=[
                (status, {"content-type": "application/json"}, b"{}")
                for status in [
                    canary.HEALTH_STATUSES[label]
                    for label in sorted(canary.HEALTH_STATUSES)
                ]
            ],
        ) as request:
            self.assertEqual(
                canary.run_health_probes(endpoints),
                {label: True for label in sorted(canary.HEALTH_STATUSES)},
            )
        self.assertEqual(request.call_count, len(canary.HEALTH_STATUSES))
        for call in request.call_args_list:
            self.assertEqual(
                call.kwargs["socket_timeout_seconds"],
                canary.DEFAULT_SOCKET_TIMEOUT_SECONDS,
            )

        self.assertLess(
            canary.DRIVER_SOCKET_TIMEOUT_SECONDS,
            canary.MAX_DRIVER_CLOCK_SKEW_SECONDS,
        )
        with self.assertRaises(canary.CanaryError):
            canary.secure_https_request(
                "https://crm.example.com/health",
                method="GET",
                maximum_body_bytes=1,
                socket_timeout_seconds=canary.DRIVER_SOCKET_TIMEOUT_SECONDS + 1,
            )

    def test_https_deadline_is_monotonic_across_headers_and_bounded_reads(self) -> None:
        class FakeSocket:
            def __init__(self) -> None:
                self.timeouts: list[float] = []
                self.closed = False

            def settimeout(self, value: float) -> None:
                self.timeouts.append(value)

            def sendall(self, _value: bytes) -> None:
                return None

            def close(self) -> None:
                self.closed = True

        class FakeContext:
            def __init__(self, tls_socket: FakeSocket) -> None:
                self.tls_socket = tls_socket

            def wrap_socket(self, _raw: FakeSocket, *, server_hostname: str) -> FakeSocket:
                self.server_hostname = server_hostname
                return self.tls_socket

        class FakeResponse:
            status = 200

            def __init__(self, chunks: list[bytes], on_read=None) -> None:
                self.chunks = list(chunks)
                self.read_sizes: list[int] = []
                self.on_read = on_read

            def begin(self) -> None:
                return None

            def getheader(self, _name: str):
                return None

            def read(self, size: int) -> bytes:
                self.read_sizes.append(size)
                if self.on_read is not None:
                    self.on_read(len(self.read_sizes))
                return self.chunks.pop(0)

            def getheaders(self) -> list[tuple[str, str]]:
                return [("Content-Type", "application/json")]

        raw_socket = FakeSocket()
        tls_socket = FakeSocket()
        response = FakeResponse([b"ab", b"cd", b""])
        ticks = iter([0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0])
        with (
            mock.patch.object(canary, "resolve_public_addresses", return_value=["203.0.114.10"]),
            mock.patch.object(canary.time, "monotonic", side_effect=lambda: next(ticks)),
            mock.patch.object(canary.socket, "create_connection", return_value=raw_socket) as connect,
            mock.patch.object(canary.ssl, "create_default_context", return_value=FakeContext(tls_socket)),
            mock.patch.object(canary, "HTTPResponse", return_value=response),
        ):
            status, headers, payload = canary.secure_https_request(
                "https://synthetic.example.com/run/ui",
                method="GET",
                maximum_body_bytes=4,
                retry_addresses=False,
                socket_timeout_seconds=20,
            )
        self.assertEqual((status, headers, payload), (200, {"content-type": "application/json"}, b"abcd"))
        self.assertEqual(response.read_sizes, [5, 3, 1])
        self.assertLess(connect.call_args.kwargs["timeout"], 20)
        self.assertTrue(all(0 < value < 20 for value in tls_socket.timeouts))
        self.assertEqual(tls_socket.timeouts, sorted(tls_socket.timeouts, reverse=True))

        deadline_clock = {"now": 0.0}
        expired_response = FakeResponse(
            [b"a", b""],
            on_read=lambda count: deadline_clock.update(now=21.0) if count == 1 else None,
        )
        with (
            mock.patch.object(canary, "resolve_public_addresses", return_value=["203.0.114.10"]),
            mock.patch.object(canary.time, "monotonic", side_effect=lambda: deadline_clock["now"]),
            mock.patch.object(canary.socket, "create_connection", return_value=FakeSocket()),
            mock.patch.object(canary.ssl, "create_default_context", return_value=FakeContext(FakeSocket())),
            mock.patch.object(canary, "HTTPResponse", return_value=expired_response),
        ):
            with self.assertRaises(canary.CanaryError):
                canary.secure_https_request(
                    "https://synthetic.example.com/run/ui",
                    method="GET",
                    maximum_body_bytes=4,
                    retry_addresses=False,
                    socket_timeout_seconds=20,
                )

    def test_driver_config_requires_a_32_byte_key_and_exact_version(self) -> None:
        value = {
            "schema_version": 1,
            "url": "https://synthetic.example.com/run/ui",
            "driver_version_sha256": digest("a"),
            "fixture_descriptor_sha256": digest("f"),
            "hmac_key_base64": base64.b64encode(b"k" * 32).decode("ascii"),
        }
        validated = canary.validate_driver_config(value)
        self.assertEqual(validated["hmac_key"], b"k" * 32)
        self.assertEqual(validated["fixture_descriptor_sha256"], digest("f"))
        invalid_fixture = dict(value)
        invalid_fixture["fixture_descriptor_sha256"] = "not-a-hash"
        with self.assertRaises(canary.CanaryError):
            canary.validate_driver_config(invalid_fixture)
        value["hmac_key_base64"] = base64.b64encode(b"short").decode("ascii")
        with self.assertRaises(canary.CanaryError):
            canary.validate_driver_config(value)


class EvidenceTests(unittest.TestCase):
    def test_canary_requires_exact_apply_and_rollback_unlock_jobs(self) -> None:
        workflow = (
            MODULE_PATH.parents[2]
            / ".github"
            / "workflows"
            / "verify-production-crm-canary.yml"
        ).read_text(encoding="utf-8")
        self.assertIn(
            'expected_jobs=\'["Acquire exact production apply main lock","Apply exact production phase","Attest exact production apply main lock proof","Authenticate exact production apply authority","Authorize exact production apply main lock release","Exact production apply receipt gate","Prepare and attest exact production mutation intent","Prepare exact production apply pre-lock authority","Release exact production apply main lock"]\'',
            workflow,
        )
        self.assertIn(
            'expected_jobs=\'["Acquire exact production rollback main lock","Attest exact production rollback main lock proof","Authenticate exact production rollback authority","Authorize exact production rollback main lock release","Exact production rollback receipt gate","Prepare and attest exact production rollback mutation intent","Prepare exact production rollback pre-lock authority","Release exact production rollback main lock","Roll back exact production phase"]\'',
            workflow,
        )
        self.assertIn(
            'expected_artifact_prefixes=\'["production-main-lock-apply","production-mutation-intent-apply","production-main-lock-proof-apply","unsigned-production-phase-apply","production-phase-apply","production-main-lock-release-authorization-apply"]\'',
            workflow,
        )
        self.assertIn(
            'expected_artifact_prefixes=\'["production-main-lock-rollback","production-mutation-intent-rollback","production-main-lock-proof-rollback","unsigned-production-phase-rollback","production-phase-rollback","production-main-lock-release-authorization-rollback"]\'',
            workflow,
        )
        self.assertIn(
            'expected_jobs=\'["Attest exact production orphan rollback main lock proof","Authenticate exact production orphan rollback authority","Exact production orphan rollback receipt gate","Prepare and attest exact production orphan rollback mutation intent","Roll back exact production orphan"]\'',
            workflow,
        )
        self.assertIn(
            'expected_artifact_prefixes=\'["production-mutation-intent-orphan-rollback","production-main-lock-proof-orphan-rollback","unsigned-production-orphan-rollback","production-orphan-rollback"]\'',
            workflow,
        )
        self.assertIn(
            ".total_count == ($expected_names | length)",
            workflow,
        )
        self.assertIn(
            "artifact_digest: ${{ steps.digest.outputs.artifact_digest }}",
            workflow,
        )
        self.assertIn(
            "phase_state_artifact_digest: ${{ steps.digest.outputs.artifact_digest }}",
            workflow,
        )
        self.assertEqual(
            workflow.count('[[ "$RAW_ARTIFACT_DIGEST" =~ ^[0-9a-f]{64}$ ]]'),
            2,
        )
        self.assertEqual(
            workflow.count("printf 'artifact_digest=sha256:%s\\n'"),
            2,
        )

    def test_receipt_sidecar_is_raw_hash_only_and_rejects_filename_form(self) -> None:
        with tempfile.TemporaryDirectory() as raw_root:
            root = Path(raw_root)
            subject = root / "production-phase-apply-receipt.json"
            sidecar = root / "production-phase-apply-receipt.sha256"
            subject.write_bytes(b'{}\n')
            expected = hashlib.sha256(subject.read_bytes()).hexdigest()
            sidecar.write_bytes((expected + "\n").encode("ascii"))
            canary.validate_sidecar(sidecar, subject, expected, "release receipt")

            sidecar.write_bytes(
                f"{expected}  production-phase-apply-receipt.json\n".encode("ascii")
            )
            with self.assertRaises(canary.CanaryError):
                canary.validate_sidecar(sidecar, subject, expected, "release receipt")

    def test_release_assertions_reject_environment_or_topology_drift(self) -> None:
        value = receipt()
        self.assertTrue(all(canary.receipt_release_assertions(value).values()))
        value["after"]["environment_values_sha256"] = digest("e")  # type: ignore[index]
        with self.assertRaises(canary.CanaryError):
            canary.receipt_release_assertions(value)

    def test_canary_requires_all_ui_checks_only_for_ui(self) -> None:
        health = {key: True for key in canary.HEALTH_STATUSES}
        baseline = receipt("baseline")
        value = canary.build_canary(
            baseline,
            descriptor(),
            health,
            None,
            None,
            route_contract_sha256=digest("0"),
            control_sha="a" * 40,
            run_id="4001",
            run_attempt=1,
            completed_at="2026-08-27T01:00:00Z",
        )
        self.assertEqual(value["assertions"]["crm"], {"required": False, "checks": {}})
        with self.assertRaises(canary.CanaryError):
            canary.build_canary(
                receipt("ui"), descriptor(), health, None, None,
                route_contract_sha256=digest("0"),
                control_sha="a" * 40, run_id="4001", run_attempt=1,
                completed_at="2026-08-27T01:00:00Z",
            )

    def test_two_file_artifact_hashes_exact_canonical_file(self) -> None:
        with tempfile.TemporaryDirectory() as raw_root:
            root = Path(raw_root)
            output = root / "out"
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}, clear=False):
                canary.ensure_output_directory(output)
                value = {"schema_version": 1, "passed": True}
                digest_value = canary.write_two_file_artifact(output, "production-crm-canary", value)
            self.assertEqual(
                digest_value,
                hashlib.sha256((json.dumps(value, separators=(",", ":"), sort_keys=True) + "\n").encode("ascii")).hexdigest(),
            )
            self.assertEqual((output / "production-crm-canary.sha256").read_text(), digest_value + "\n")

    def test_shared_phase_state_builder_binds_receipt_and_canary(self) -> None:
        value = receipt("baseline")
        raw = canary.canonical_file_bytes(value)
        receipt_hash = hashlib.sha256(raw).hexdigest()
        state = canary.build_phase_state(
            value,
            descriptor(receipt_hash),
            release=release,
            canary_sha256=digest("f"),
            control_sha="a" * 40,
            run_id="4001",
            run_attempt=1,
            completed_at="2026-08-27T01:02:00Z",
            policy_sha256=value["control"]["release_policy_sha256"],  # type: ignore[index]
            schema_sha256=value["control"]["change_schema_sha256"],  # type: ignore[index]
        )
        self.assertEqual(state["evidence"]["change_receipt_sha256"], receipt_hash)
        self.assertEqual(state["evidence"]["canary_sha256"], digest("f"))
        self.assertEqual(state["control"]["workflow_path"], canary.WORKFLOW_PATH)

    def test_reconciled_phase_state_binds_original_receipt_and_reconciliation(self) -> None:
        value = receipt("baseline")
        receipt_hash = hashlib.sha256(canary.canonical_file_bytes(value)).hexdigest()
        evidence = canary.validate_release_descriptor(
            reconciled_descriptor(receipt_hash), "a" * 40
        )
        state = canary.build_phase_state(
            value,
            evidence,
            release=release,
            canary_sha256=digest("f"),
            control_sha="a" * 40,
            run_id="6001",
            run_attempt=1,
            completed_at="2026-08-27T01:02:00Z",
            policy_sha256=value["control"]["release_policy_sha256"],  # type: ignore[index]
            schema_sha256=value["control"]["change_schema_sha256"],  # type: ignore[index]
        )
        self.assertEqual(state["schema_version"], 2)
        self.assertEqual(
            state["lineage"]["predecessor_kind"], "apply-reconciled-receipt"
        )
        self.assertEqual(
            state["evidence"]["change_receipt_binding"]["sha256"], receipt_hash
        )
        self.assertEqual(
            state["evidence"]["main_lock_release_reconciliation_binding"]["sha256"],
            digest("2"),
        )

    def test_shared_phase_state_builder_accepts_only_strict_rollback_receipt(self) -> None:
        value = rollback_receipt()
        receipt_hash = hashlib.sha256(canary.canonical_file_bytes(value)).hexdigest()
        evidence = descriptor(receipt_hash)
        evidence["receipt_kind"] = "rollback"
        canary.load_control_module(
            MODULE_PATH.parents[2],
            "release/deployment/rollback_production_change.py",
            "rollback_production_change",
        )
        state = canary.build_phase_state(
            value,
            evidence,
            release=release,
            canary_sha256=digest("f"),
            control_sha="a" * 40,
            run_id="5001",
            run_attempt=1,
            completed_at="2026-08-27T01:02:00Z",
            policy_sha256=value["control"]["release_policy_sha256"],  # type: ignore[index]
            schema_sha256=value["control"]["change_schema_sha256"],  # type: ignore[index]
        )
        self.assertEqual(state["lineage"]["predecessor_kind"], "rollback-receipt")
        self.assertEqual(state["lineage"]["phase"], "backend")
        self.assertEqual(state["evidence"]["change_receipt_sha256"], receipt_hash)

    def test_reconciled_rollback_preloads_canonical_module_in_clean_process(self) -> None:
        previous = sys.modules.pop("rollback_production_change", None)
        try:
            shared_release = canary.load_phase_state_release_module(
                MODULE_PATH.parents[2], "rollback-reconciled"
            )
            self.assertIn("rollback_production_change", sys.modules)
            value = rollback_receipt()
            receipt_hash = hashlib.sha256(
                canary.canonical_file_bytes(value)
            ).hexdigest()
            evidence = descriptor(receipt_hash)
            evidence["receipt_kind"] = "rollback-reconciled"
            evidence["release_reconciliation"] = {
                "run_id": "9001",
                "run_attempt": 1,
                "artifact_id": "9002",
                "artifact_digest": f"sha256:{digest('3')}",
                "sha256": digest("2"),
            }
            evidence = canary.validate_release_descriptor(evidence, "a" * 40)
            state = canary.build_phase_state(
                value,
                evidence,
                release=shared_release,
                canary_sha256=digest("f"),
                control_sha="a" * 40,
                run_id="5002",
                run_attempt=1,
                completed_at="2026-08-27T01:03:00Z",
                policy_sha256=value["control"]["release_policy_sha256"],  # type: ignore[index]
                schema_sha256=value["control"]["change_schema_sha256"],  # type: ignore[index]
            )
            self.assertEqual(state["schema_version"], 2)
            self.assertEqual(
                state["lineage"]["predecessor_kind"],
                "rollback-reconciled-receipt",
            )
            shared_release.validate_phase_state(state)
        finally:
            sys.modules.pop("rollback_production_change", None)
            if previous is not None:
                sys.modules["rollback_production_change"] = previous


if __name__ == "__main__":
    unittest.main()
