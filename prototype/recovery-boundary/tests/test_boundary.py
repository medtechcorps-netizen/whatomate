from __future__ import annotations

import copy
import hashlib
import ipaddress
import re
import socket
import struct
import unittest
import urllib.request
from pathlib import Path
from unittest import mock

from fault_oracle import BoundaryStateOracle, FaultLedger, OracleError, SIDE_EFFECTS
from schema_tools import (
    HASH_FIELDS,
    LEDGER_PUBLICATION_FIELDS,
    LEDGER_PUBLICATION_REQUEST_FIELDS,
    LEDGER_PUBLICATION_RESULT_FIELDS,
    LIFECYCLE_EFFECTS,
    MAX_PUBLIC_JSON_BYTES,
    OUTER_CONTINUITY_FIELDS,
    OBSERVER_CLEANUP_LIFECYCLE_REQUEST_FIELDS,
    OBSERVER_CLEANUP_LIFECYCLE_RESULT_FIELDS,
    ValidationError,
    assert_schema_is_closed,
    base64url_encode,
    canonical_bytes,
    canonical_record_sha256,
    domain_separated_message,
    ed25519_public_key_from_seed,
    ed25519_sign,
    ed25519_verify,
    signed_record_payload,
    strict_json_loads,
    validate_evidence,
    validate_recovery_chain,
    validate_recovery_continuity,
)


ROOT = Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas"
FIXTURES = ROOT / "fixtures"
WORKFLOWS = ROOT / "workflows"
DOCS = ROOT / "docs"

VALID_FIXTURES = {
    "writer-receipt.valid.json": "writer-receipt.schema.json",
    "observer-receipt.valid.json": "observer-receipt.schema.json",
    "lifecycle-receipt.valid.json": "lifecycle-receipt.schema.json",
    "domain-separation.valid.json": "domain-separation.schema.json",
    "recovery-admission-authorization.valid.json": "recovery-admission-authorization.schema.json",
    "recovery-admission.valid.json": "recovery-admission.schema.json",
    "recovery-boundary-continuity.valid.json": "recovery-boundary-continuity.schema.json",
    "writer-authorization-publication.valid.json": "ledger-publication.schema.json",
    "observer-admission-publication.valid.json": "ledger-publication.schema.json",
    "observer-evidence-publication.valid.json": "ledger-publication.schema.json",
    "observer-admission-lifecycle.valid.json": "observer-admission-lifecycle.schema.json",
    "observer-cleanup-lifecycle.valid.json": "observer-cleanup-lifecycle.schema.json",
}

EXPECTED_TREE_FILES = {
    "PATH_BLOBS.sha256",
    "README.md",
    "cmd/observer-authority/main.go",
    "cmd/observer-broker/main.go",
    "cmd/writer-authority/main.go",
    "cmd/writer-broker/main.go",
    "docker/observer-authority.Dockerfile",
    "docker/observer-broker.Dockerfile",
    "docker/writer-authority.Dockerfile",
    "docker/writer-broker.Dockerfile",
    "docs/gate-c-resource-ceiling.md",
    "docs/state-machine-runbook.md",
    "docs/threat-model.md",
    "fixtures/domain-separation.noncanonical.json",
    "fixtures/domain-separation.valid.json",
    "fixtures/lifecycle-receipt.invalid-writer-present.json",
    "fixtures/lifecycle-receipt.valid.json",
    "fixtures/observer-receipt.invalid-source-read.json",
    "fixtures/observer-receipt.valid.json",
    "fixtures/observer-admission-publication.valid.json",
    "fixtures/observer-admission-lifecycle.valid.json",
    "fixtures/observer-cleanup-lifecycle.valid.json",
    "fixtures/observer-evidence-publication.valid.json",
    "fixtures/recovery-admission-authorization.valid.json",
    "fixtures/recovery-admission.valid.json",
    "fixtures/recovery-boundary-continuity.valid.json",
    "fixtures/writer-receipt.invalid-extra.json",
    "fixtures/writer-receipt.valid.json",
    "fixtures/writer-authorization-publication.valid.json",
    "internal/model/boundary.go",
    "internal/model/durable_effect.go",
    "internal/model/oracle.go",
    "internal/model/oracle_test.go",
    "internal/model/observer_lifecycle.go",
    "internal/model/observer_lifecycle_test.go",
    "internal/model/protocol_test.go",
    "internal/model/protocol.go",
    "internal/model/sanitize.go",
    "internal/model/sanitize_test.go",
    "internal/model/types.go",
    "internal/model/writer_lifecycle_provider.go",
    "internal/model/writer_lifecycle_provider_test.go",
    "internal/protocol/canonical.go",
    "internal/protocol/canonical_test.go",
    "internal/protocol/ledger.go",
    "internal/protocol/ledger_test.go",
    "internal/protocol/no_network_test.go",
    "internal/protocol/oidc.go",
    "internal/protocol/oidc_test.go",
    "internal/protocol/signature.go",
    "internal/rolecmd/rolecmd.go",
    "internal/rolecmd/rolecmd_test.go",
    "schemas/domain-separation.schema.json",
    "schemas/lifecycle-receipt.schema.json",
    "schemas/ledger-publication.schema.json",
    "schemas/observer-receipt.schema.json",
    "schemas/observer-admission-lifecycle.schema.json",
    "schemas/observer-cleanup-lifecycle.schema.json",
    "schemas/recovery-admission-authorization.schema.json",
    "schemas/recovery-admission.schema.json",
    "schemas/recovery-boundary-continuity.schema.json",
    "schemas/writer-receipt.schema.json",
    "tests/fault_oracle.py",
    "tests/schema_tools.py",
    "tests/test_boundary.py",
    "verify_gate_a.py",
    "workflows/build.tmpl",
    "workflows/cleanup.tmpl",
    "workflows/exercise.tmpl",
}


def load_json(path: Path):
    return strict_json_loads(path.read_bytes())


def iter_items(value):
    if isinstance(value, dict):
        for key, child in value.items():
            yield key, child
            yield from iter_items(child)
    elif isinstance(value, list):
        for child in value:
            yield from iter_items(child)


def terminal_evidence_digest(value):
    evidence = {
        "provider_fact_source": value["provider_fact_source"],
        "provider_read_one_at": value["provider_read_one_at"],
        "provider_read_two_at": value["provider_read_two_at"],
        "minimum_provider_read_separation_seconds": value["minimum_provider_read_separation_seconds"],
        "writer_terminal_proof": value["writer_terminal_proof"],
    }
    return hashlib.sha256(canonical_bytes(evidence)).hexdigest()


_ORIGINAL_SOCKET = socket.socket
_ORIGINAL_CONNECT = socket.socket.connect
_ORIGINAL_CONNECT_EX = socket.socket.connect_ex
_ORIGINAL_SENDTO = socket.socket.sendto


def _require_loopback_address(address):
    if not isinstance(address, tuple) or not address:
        raise AssertionError("external DNS/network is forbidden in boundary tests")
    try:
        candidate = ipaddress.ip_address(address[0])
    except ValueError as exc:
        raise AssertionError("external DNS/network is forbidden in boundary tests") from exc
    if not candidate.is_loopback:
        raise AssertionError("external DNS/network is forbidden in boundary tests")


def _loopback_only_connect(instance, address):
    _require_loopback_address(address)
    return _ORIGINAL_CONNECT(instance, address)


def _loopback_only_connect_ex(instance, address):
    _require_loopback_address(address)
    return _ORIGINAL_CONNECT_EX(instance, address)


def _loopback_only_sendto(instance, data, *args):
    if not args:
        raise AssertionError("external DNS/network is forbidden in boundary tests")
    _require_loopback_address(args[-1])
    return _ORIGINAL_SENDTO(instance, data, *args)


class RecoveryBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        denial = AssertionError("external DNS/network is forbidden in boundary tests")
        cls._network_patches = [
            mock.patch.object(_ORIGINAL_SOCKET, "connect", new=_loopback_only_connect),
            mock.patch.object(_ORIGINAL_SOCKET, "connect_ex", new=_loopback_only_connect_ex),
            mock.patch.object(_ORIGINAL_SOCKET, "sendto", new=_loopback_only_sendto),
            mock.patch.object(socket, "getaddrinfo", side_effect=denial),
            mock.patch.object(socket, "gethostbyname", side_effect=denial),
            mock.patch.object(socket, "gethostbyname_ex", side_effect=denial),
            mock.patch.object(socket, "gethostbyaddr", side_effect=denial),
            mock.patch.object(socket, "create_connection", side_effect=denial),
            mock.patch.object(urllib.request, "urlopen", side_effect=denial),
        ]
        for patcher in cls._network_patches:
            patcher.start()

    @classmethod
    def tearDownClass(cls):
        for patcher in reversed(cls._network_patches):
            patcher.stop()

    def schema(self, name: str):
        return load_json(SCHEMAS / name)

    def assert_semantically_rejected(self, value, schema_name: str):
        with self.assertRaises(ValidationError):
            validate_evidence(value, self.schema(schema_name))

    def recovery_chain(self):
        names = (
            "domain-separation.valid.json",
            "writer-receipt.valid.json",
            "lifecycle-receipt.valid.json",
            "writer-authorization-publication.valid.json",
            "recovery-admission-authorization.valid.json",
            "observer-admission-publication.valid.json",
            "recovery-admission.valid.json",
            "observer-admission-lifecycle.valid.json",
            "observer-receipt.valid.json",
            "observer-evidence-publication.valid.json",
            "observer-cleanup-lifecycle.valid.json",
            "recovery-boundary-continuity.valid.json",
        )
        return tuple(load_json(FIXTURES / name) for name in names)

    @staticmethod
    def resign_publication(value, role):
        domain = {
            "writer-authorization": "ledger-publication/writer-authorization/v2",
            "observer-admission": "ledger-publication/observer-admission/v2",
            "observer-evidence": "ledger-publication/observer-evidence/v2",
        }[value["publication_kind"]]
        seed = bytes(range(32)) if role == "writer" else bytes(range(32, 64))
        signature = ed25519_sign(
            seed,
            domain_separated_message(
                domain, signed_record_payload(value, "signature")
            ),
        )
        value["signature"] = base64url_encode(signature)
        value["signature_sha256"] = hashlib.sha256(signature).hexdigest()

    @classmethod
    def rebind_publication(cls, value, role):
        value["publication_request_sha256"] = hashlib.sha256(canonical_bytes({
            field: value[field] for field in LEDGER_PUBLICATION_REQUEST_FIELDS
        })).hexdigest()
        value["publication_result_sha256"] = hashlib.sha256(canonical_bytes({
            field: value[field] for field in LEDGER_PUBLICATION_RESULT_FIELDS
        })).hexdigest()
        cls.resign_publication(value, role)

    @classmethod
    def rebind_observer_evidence_publication(cls, chain):
        publication = chain[9]
        observer = chain[8]
        observer["publication_record_sha256"] = canonical_record_sha256(publication)
        signature = ed25519_sign(
            bytes(range(32, 64)),
            domain_separated_message(
                "observer-recovery-receipt/v2",
                signed_record_payload(observer, "signature"),
            ),
        )
        observer["signature"] = base64url_encode(signature)
        observer["signature_sha256"] = hashlib.sha256(signature).hexdigest()
        cleanup = chain[10]
        cleanup["bound_publication_record_sha256"] = canonical_record_sha256(
            publication
        )
        cleanup["observer_receipt_sha256"] = canonical_record_sha256(observer)
        cleanup["observer_evidence_sha256"] = observer["evidence_sha256"]
        cleanup["lifecycle_request_sha256"] = hashlib.sha256(canonical_bytes({
            field: cleanup[field]
            for field in OBSERVER_CLEANUP_LIFECYCLE_REQUEST_FIELDS
        })).hexdigest()
        cleanup["lifecycle_result_sha256"] = hashlib.sha256(canonical_bytes({
            field: cleanup[field]
            for field in OBSERVER_CLEANUP_LIFECYCLE_RESULT_FIELDS
        })).hexdigest()
        cleanup_signature = ed25519_sign(
            bytes(range(32, 64)),
            domain_separated_message(
                "observer-cleanup-lifecycle/v2",
                signed_record_payload(cleanup, "signature"),
            ),
        )
        cleanup["signature"] = base64url_encode(cleanup_signature)
        cleanup["signature_sha256"] = hashlib.sha256(cleanup_signature).hexdigest()
        outer = chain[11]
        outer["observer_evidence_publication_record_sha256"] = canonical_record_sha256(
            publication
        )
        outer["observer_receipt_sha256"] = canonical_record_sha256(observer)
        outer["observer_cleanup_lifecycle_sha256"] = canonical_record_sha256(
            cleanup
        )
        outer["outer_continuity_sha256"] = hashlib.sha256(canonical_bytes({
            field: outer[field] for field in OUTER_CONTINUITY_FIELDS
        })).hexdigest()

    def test_whole_subtree_exact_path_manifest_and_regular_files(self):
        actual = {
            path.relative_to(ROOT).as_posix()
            for path in ROOT.rglob("*")
            if path.is_file()
        }
        self.assertEqual(actual, EXPECTED_TREE_FILES)
        for relative in EXPECTED_TREE_FILES:
            path = ROOT / relative
            self.assertFalse(path.is_symlink(), relative)
            self.assertEqual(path.stat().st_nlink, 1, relative)
            data = path.read_bytes()
            self.assertNotIn(b"\x00", data, relative)
            data.decode("utf-8", errors="strict")

    def test_no_generated_bytecode_or_cache(self):
        generated = [
            path for path in ROOT.rglob("*")
            if path.name in {"__pycache__", ".pytest_cache", ".mypy_cache"}
            or path.suffix in {".pyc", ".pyo"}
        ]
        self.assertEqual(generated, [])

    def test_workflow_templates_are_inert_and_non_discoverable(self):
        templates = sorted(WORKFLOWS.iterdir())
        self.assertEqual([path.name for path in templates], ["build.tmpl", "cleanup.tmpl", "exercise.tmpl"])
        forbidden = (
            "workflow_dispatch", "pull_request", "schedule:", "id-token", "secrets.",
            "http://", "https://", "curl ", "wget ", "doctl", "gh api",
            "invoke-webrequest", "docker ", "kubectl", "terraform",
        )
        for path in templates:
            source = path.read_text(encoding="utf-8")
            lowered = source.lower()
            self.assertEqual(source.count("on: []"), 1, path)
            self.assertEqual(source.count("if: ${{ false }}"), 1, path)
            permission_block = source.split("permissions:", 1)[1].split("concurrency:", 1)[0]
            self.assertNotIn("write", permission_block.lower())
            for fragment in forbidden:
                self.assertNotIn(fragment, lowered, (path, fragment))

    def test_inert_docker_and_command_surfaces(self):
        dockerfiles = sorted((ROOT / "docker").glob("*.Dockerfile"))
        self.assertEqual(len(dockerfiles), 4)
        for path in dockerfiles:
            source = path.read_text(encoding="utf-8")
            self.assertRegex(source, r"(?m)^FROM .*@sha256:[0-9a-f]{64} AS builder$")
            self.assertIn("GOPROXY=off", source)
            self.assertIn("\nFROM scratch\n", source)
            self.assertIn("\nUSER 65532:65532\n", source)
            self.assertIn('ENTRYPOINT ["/runtime"]', source)
            self.assertNotIn("CMD", source)
        rolecmd = (ROOT / "internal/rolecmd/rolecmd.go").read_text(encoding="utf-8")
        self.assertIn('"--describe"', rolecmd)
        self.assertNotIn("net/http", rolecmd)

    def test_schemas_are_closed_and_use_only_supported_subset(self):
        schema_paths = sorted(SCHEMAS.glob("*.schema.json"))
        self.assertEqual(len(schema_paths), 10)
        for path in schema_paths:
            assert_schema_is_closed(load_json(path))
        publication_schema = self.schema("ledger-publication.schema.json")
        self.assertEqual(
            set(publication_schema["required"]), set(LEDGER_PUBLICATION_FIELDS)
        )

    def test_valid_fixtures_validate_semantically_and_are_canonical(self):
        for fixture_name, schema_name in VALID_FIXTURES.items():
            fixture_path = FIXTURES / fixture_name
            raw = fixture_path.read_bytes()
            self.assertTrue(raw.endswith(b"\n"), fixture_path)
            self.assertNotIn(b"\n", raw[:-1], fixture_path)
            value = strict_json_loads(raw[:-1], require_canonical=True)
            validate_evidence(value, self.schema(schema_name))
            self.assertEqual(raw[:-1], canonical_bytes(value), fixture_path)

    def test_schema_semantic_and_canonicalization_negative_fixtures(self):
        self.assert_semantically_rejected(
            load_json(FIXTURES / "writer-receipt.invalid-extra.json"),
            "writer-receipt.schema.json",
        )
        self.assert_semantically_rejected(
            load_json(FIXTURES / "observer-receipt.invalid-source-read.json"),
            "observer-receipt.schema.json",
        )
        self.assert_semantically_rejected(
            load_json(FIXTURES / "lifecycle-receipt.invalid-writer-present.json"),
            "lifecycle-receipt.schema.json",
        )
        raw = (FIXTURES / "domain-separation.noncanonical.json").read_bytes().rstrip(b"\n")
        value = strict_json_loads(raw)
        validate_evidence(value, self.schema("domain-separation.schema.json"))
        with self.assertRaises(ValidationError):
            strict_json_loads(raw, require_canonical=True)

    def test_strict_json_rejects_duplicates_floats_unicode_and_oversize(self):
        self.assertEqual(strict_json_loads(b'{"a":1}', require_canonical=True), {"a": 1})
        negatives = (
            b'{"a":1,"a":2}',
            b'{"a":1.0}',
            b'{"a":NaN}',
            b'{"a":"\\u00e9"}',
            '{"a":"é"}'.encode("utf-8"),
            b'{ "a":1}',
        )
        for raw in negatives:
            with self.subTest(raw=raw):
                with self.assertRaises(ValidationError):
                    strict_json_loads(raw, require_canonical=True)
        oversized = b'{"a":"' + (b"x" * MAX_PUBLIC_JSON_BYTES) + b'"}'
        with self.assertRaises(ValidationError):
            strict_json_loads(oversized)

    def test_universal_semantic_validator_rejects_required_mutations(self):
        writer = load_json(FIXTURES / "writer-receipt.valid.json")
        observer = load_json(FIXTURES / "observer-receipt.valid.json")
        lifecycle = load_json(FIXTURES / "lifecycle-receipt.valid.json")
        domain = load_json(FIXTURES / "domain-separation.valid.json")

        for value, schema_name in (
            (writer, "writer-receipt.schema.json"),
            (observer, "observer-receipt.schema.json"),
            (lifecycle, "lifecycle-receipt.schema.json"),
        ):
            mutant = copy.deepcopy(value)
            mutant["authorization_sha256"] = mutant["operation_sha256"]
            self.assert_semantically_rejected(mutant, schema_name)

        for field in ("authority_app_sha256", "broker_app_sha256", "ledger_sha256", "root_key_sha256"):
            mutant = copy.deepcopy(domain)
            mutant["observer"][field] = mutant["writer"][field]
            self.assert_semantically_rejected(mutant, "domain-separation.schema.json")

        true_fields = (
            "broker_deleted", "direct_get_absent", "delete_action_terminal",
            "app_inventory_pagination_complete",
            "deployment_inventory_pagination_complete",
            "provider_operation_inventory_pagination_complete",
            "full_redeploy_complete",
            "old_instance_grace_elapsed", "leaf_revoked", "capability_revoked",
            "mtls_revoked", "wrapping_key_revoked", "binding_absent",
            "credential_absent", "firewall_restored",
        )
        zero_fields = (
            "app_inventory_count", "deployment_inventory_count",
            "nonterminal_deployment_count", "rollback_capable_deployment_count",
            "nonterminal_provider_operation_count",
        )
        for field in true_fields + zero_fields:
            mutant = copy.deepcopy(lifecycle)
            proof = mutant["writer_terminal_proof"]
            proof[field] = False if field in true_fields else 1
            mutant["terminal_evidence_sha256"] = terminal_evidence_digest(mutant)
            self.assert_semantically_rejected(mutant, "lifecycle-receipt.schema.json")

        for mutation in (
            {"terminal_result": "success", "reconciliation": "reconciled-zero-terminal"},
            {"terminal_result": "quarantined", "reconciliation": "reconciled-one"},
            {"provider_read_two_at": "2099-01-01T00:00:01Z"},
            {"provider_read_two_at": "2098-12-31T23:59:59Z"},
            {"issued_at": "2099-02-30T00:00:03Z"},
            {"expires_at": "2099-01-01T00:00:03Z"},
            {"terminal_evidence_sha256": "9" * 64},
        ):
            mutant = copy.deepcopy(lifecycle)
            mutant.update(mutation)
            self.assert_semantically_rejected(mutant, "lifecycle-receipt.schema.json")

        mutant = copy.deepcopy(lifecycle)
        mutant["writer_terminal_proof"]["firewall_read_one_sha256"] = "0" * 64
        mutant["writer_terminal_proof"]["firewall_read_two_sha256"] = "0" * 64
        mutant["terminal_evidence_sha256"] = terminal_evidence_digest(mutant)
        self.assert_semantically_rejected(mutant, "lifecycle-receipt.schema.json")

        for field in (
            "original_firewall_sha256", "firewall_read_one_sha256",
            "firewall_read_two_sha256", "original_projection_sha256",
            "projection_read_one_sha256", "projection_read_two_sha256",
            "app_inventory_sha256", "deployment_inventory_sha256",
            "provider_operation_inventory_sha256",
            "action_ledger_read_one_sha256", "action_ledger_read_two_sha256",
        ):
            mutant = copy.deepcopy(lifecycle)
            mutant["writer_terminal_proof"][field] = "ab" * 32
            mutant["terminal_evidence_sha256"] = terminal_evidence_digest(mutant)
            self.assert_semantically_rejected(mutant, "lifecycle-receipt.schema.json")

        mutant = copy.deepcopy(lifecycle)
        mutant["writer_terminal_proof"]["delete_action_sha256"] = "0" * 64
        mutant["terminal_evidence_sha256"] = terminal_evidence_digest(mutant)
        self.assert_semantically_rejected(mutant, "lifecycle-receipt.schema.json")

        for mutation in (
            {"recovery_read_two_at": "2099-01-01T00:00:01Z"},
            {"recovery_read_two_at": "2099-01-01T00:02:00Z"},
            {"issued_at": "2099-01-01T00:00:01Z"},
        ):
            mutant = copy.deepcopy(observer)
            mutant.update(mutation)
            self.assert_semantically_rejected(mutant, "observer-receipt.schema.json")

    def test_recovery_admission_publications_and_outer_continuity_are_exact(self):
        authorization = load_json(FIXTURES / "recovery-admission-authorization.valid.json")
        admission = load_json(FIXTURES / "recovery-admission.valid.json")
        outer = load_json(FIXTURES / "recovery-boundary-continuity.valid.json")
        validate_recovery_continuity(authorization, admission, outer)

        # Each published domain is independently closed and binds its exact
        # signature, durable publication request/result, and completion record.
        for field in (
            "claims_sha256", "publication_request_sha256",
            "writer_signature_sha256", "publication_result_sha256",
            "publication_record_sha256", "authorization_sha256",
        ):
            mutant = copy.deepcopy(authorization)
            mutant[field] = "ab" * 32
            self.assert_semantically_rejected(
                mutant, "recovery-admission-authorization.schema.json"
            )
        mutant = copy.deepcopy(authorization)
        mutant["writer_signature"] = "A" + mutant["writer_signature"][1:]
        self.assert_semantically_rejected(
            mutant, "recovery-admission-authorization.schema.json"
        )
        mutant = copy.deepcopy(authorization)
        mutant["publication_completed_at"] = "2099-01-01T00:00:05Z"
        self.assert_semantically_rejected(
            mutant, "recovery-admission-authorization.schema.json"
        )
        for field in (
            "authorization_sha256", "continuity_sha256", "admission_request_sha256",
            "admission_claims_sha256", "publication_request_sha256",
            "observer_signature_sha256", "publication_result_sha256",
            "publication_record_sha256", "admission_sha256",
        ):
            mutant = copy.deepcopy(admission)
            mutant[field] = "ac" * 32
            self.assert_semantically_rejected(mutant, "recovery-admission.schema.json")
        mutant = copy.deepcopy(admission)
        mutant["observer_signature"] = "A" + mutant["observer_signature"][1:]
        self.assert_semantically_rejected(mutant, "recovery-admission.schema.json")
        mutant = copy.deepcopy(admission)
        mutant["publication_completed_at"] = "2099-01-01T00:00:07Z"
        self.assert_semantically_rejected(mutant, "recovery-admission.schema.json")

        # Recomputing the outer self-digest cannot legitimize substitution of
        # any writer/observer publication or shared fork/terminal binding.
        for field in (
            "terminal_receipt_sha256", "fork_request_sha256", "fork_result_sha256",
            "authorization_claims_sha256", "authorization_sha256",
            "admission_request_sha256", "admission_claims_sha256", "admission_sha256",
            "writer_ledger_sha256", "writer_root_sha256", "writer_oracle_sha256",
            "writer_signature_sha256",
            "writer_authorization_publication_record_sha256",
            "observer_ledger_sha256", "observer_root_sha256", "observer_oracle_sha256",
            "observer_signature_sha256",
            "observer_admission_publication_record_sha256",
        ):
            mutant = copy.deepcopy(outer)
            mutant[field] = "ad" * 32
            mutant["outer_continuity_sha256"] = hashlib.sha256(canonical_bytes({
                name: mutant[name] for name in OUTER_CONTINUITY_FIELDS
            })).hexdigest()
            with self.assertRaises(ValidationError, msg=field):
                validate_recovery_continuity(authorization, admission, mutant)
        for field in (
            "writer_authorization_publication_completed_at",
            "observer_admission_publication_completed_at",
        ):
            mutant = copy.deepcopy(outer)
            mutant[field] = "2099-01-01T00:00:59Z"
            mutant["outer_continuity_sha256"] = hashlib.sha256(canonical_bytes({
                name: mutant[name] for name in OUTER_CONTINUITY_FIELDS
            })).hexdigest()
            with self.assertRaises(ValidationError, msg=field):
                validate_recovery_continuity(authorization, admission, mutant)

    def test_full_chain_uses_exact_records_real_role_keys_and_temporal_order(self):
        domain = load_json(FIXTURES / "domain-separation.valid.json")
        writer = load_json(FIXTURES / "writer-receipt.valid.json")
        lifecycle = load_json(FIXTURES / "lifecycle-receipt.valid.json")
        writer_publication = load_json(FIXTURES / "writer-authorization-publication.valid.json")
        authorization = load_json(FIXTURES / "recovery-admission-authorization.valid.json")
        admission_publication = load_json(FIXTURES / "observer-admission-publication.valid.json")
        admission = load_json(FIXTURES / "recovery-admission.valid.json")
        admission_lifecycle = load_json(FIXTURES / "observer-admission-lifecycle.valid.json")
        observer = load_json(FIXTURES / "observer-receipt.valid.json")
        evidence_publication = load_json(FIXTURES / "observer-evidence-publication.valid.json")
        cleanup_lifecycle = load_json(FIXTURES / "observer-cleanup-lifecycle.valid.json")
        outer = load_json(FIXTURES / "recovery-boundary-continuity.valid.json")
        validate_recovery_chain(
            domain, writer, lifecycle, writer_publication, authorization,
            admission_publication, admission, admission_lifecycle, observer,
            evidence_publication, cleanup_lifecycle, outer,
        )

        self.assertEqual(outer["domain_record_sha256"], canonical_record_sha256(domain))
        self.assertEqual(outer["writer_receipt_sha256"], canonical_record_sha256(writer))
        self.assertEqual(outer["terminal_receipt_sha256"], canonical_record_sha256(lifecycle))
        self.assertEqual(
            outer["authorization_record_sha256"], canonical_record_sha256(authorization)
        )
        self.assertEqual(outer["admission_record_sha256"], canonical_record_sha256(admission))
        self.assertEqual(outer["observer_receipt_sha256"], canonical_record_sha256(observer))

        # A role-key swap, even if all public domain fields are changed
        # together, is rejected before it can authorize a receipt.
        mutant_domain = copy.deepcopy(domain)
        mutant_domain["observer"]["signing_public_key"] = domain["writer"]["signing_public_key"]
        mutant_domain["observer"]["root_key_sha256"] = domain["writer"]["root_key_sha256"]
        mutant_domain["observer"]["signing_kid"] = "synthetic-observer-" + domain["writer"]["root_key_sha256"][:16]
        with self.assertRaises(ValidationError):
            validate_recovery_chain(
                mutant_domain, writer, lifecycle, writer_publication, authorization,
                admission_publication, admission, admission_lifecycle, observer,
                evidence_publication, cleanup_lifecycle, outer,
            )
        for field, replacement in (
            ("signing_kid", "synthetic-writer-56475aa75463474d"),
            ("signing_public_key", "B" + domain["writer"]["signing_public_key"][1:]),
        ):
            mutant_domain = copy.deepcopy(domain)
            mutant_domain["writer"][field] = replacement
            with self.assertRaises(ValidationError, msg=field):
                validate_recovery_chain(
                    mutant_domain, writer, lifecycle, writer_publication, authorization,
                    admission_publication, admission, admission_lifecycle, observer,
                    evidence_publication, cleanup_lifecycle, outer,
                )

        # A signature copied across roles is not a valid signature under the
        # observer trust root or observer domain-separated payload.
        mutant_observer = copy.deepcopy(observer)
        mutant_observer["signature"] = writer["signature"]
        mutant_observer["signature_sha256"] = writer["signature_sha256"]
        mutant_outer = copy.deepcopy(outer)
        mutant_outer["observer_receipt_sha256"] = canonical_record_sha256(mutant_observer)
        mutant_outer["outer_continuity_sha256"] = hashlib.sha256(canonical_bytes({
            name: mutant_outer[name] for name in OUTER_CONTINUITY_FIELDS
        })).hexdigest()
        with self.assertRaises(ValidationError):
            validate_recovery_chain(
                domain, writer, lifecycle, writer_publication, authorization,
                admission_publication, admission, admission_lifecycle, mutant_observer,
                evidence_publication, cleanup_lifecycle, mutant_outer,
            )

        for field in ("read_one_marker_sha256", "read_two_marker_sha256"):
            mutant_observer = copy.deepcopy(observer)
            mutant_observer[field] = "ab" * 32
            with self.assertRaises(ValidationError, msg=field):
                validate_recovery_chain(
                    domain, writer, lifecycle, writer_publication, authorization,
                    admission_publication, admission, admission_lifecycle,
                    mutant_observer, evidence_publication, cleanup_lifecycle, outer,
                )

        # Exclusive lifetime: equality with expiry is already expired.
        mutant_observer = copy.deepcopy(observer)
        mutant_observer["issued_at"] = mutant_observer["expires_at"]
        with self.assertRaises(ValidationError):
            validate_recovery_chain(
                domain, writer, lifecycle, writer_publication, authorization,
                admission_publication, admission, admission_lifecycle,
                mutant_observer, evidence_publication, cleanup_lifecycle, outer,
            )

    def test_publication_completion_records_are_closed_signed_and_chain_bound(self):
        chain = list(self.recovery_chain())
        schema = self.schema("ledger-publication.schema.json")
        publications = (
            (3, "writer"),
            (5, "observer"),
            (9, "observer"),
        )
        for index, role in publications:
            publication = chain[index]
            validate_evidence(publication, schema)

            mutant = copy.deepcopy(publication)
            mutant["unexpected_field"] = "synthetic-unknown-field"
            self.assert_semantically_rejected(mutant, "ledger-publication.schema.json")
            mutant = copy.deepcopy(publication)
            del mutant["published_object_sha256"]
            self.assert_semantically_rejected(mutant, "ledger-publication.schema.json")
            for field, replacement in (
                ("issue_count", 2),
                ("state", "issued-ambiguous"),
                ("publication_request_sha256", "ab" * 32),
                ("publication_result_sha256", "ac" * 32),
                ("completed_at", publication["requested_at"]),
                ("expires_at", publication["completed_at"]),
            ):
                mutant = copy.deepcopy(publication)
                mutant[field] = replacement
                self.assert_semantically_rejected(
                    mutant, "ledger-publication.schema.json"
                )

            # A record can be independently well-formed and genuinely signed
            # yet still be unusable if its exact completion was never bound by
            # the signed owner and outer chain.
            mutant_chain = copy.deepcopy(chain)
            unpublished = mutant_chain[index]
            unpublished["completed_at"] = {
                3: "2099-01-01T00:00:05Z",
                5: "2099-01-01T00:00:07Z",
                9: "2099-01-01T00:00:13Z",
            }[index]
            self.rebind_publication(unpublished, role)
            validate_evidence(unpublished, schema)
            with self.assertRaises(ValidationError):
                validate_recovery_chain(*mutant_chain)

            for field in (
                "ledger_sha256", "root_key_sha256", "signing_kid",
                "generation", "phase", "operation_body_sha256",
                "published_object_sha256",
            ):
                mutant_chain = copy.deepcopy(chain)
                substituted = mutant_chain[index]
                substituted[field] = (
                    2 if field == "generation"
                    else "bridge" if field == "phase"
                    else f"synthetic-{role}-deadbeefdeadbeef" if field == "signing_kid"
                    else "ad" * 32
                )
                self.rebind_publication(substituted, role)
                validate_evidence(substituted, schema)
                with self.assertRaises(ValidationError, msg=(index, field)):
                    validate_recovery_chain(*mutant_chain)

        # Fully cascade the schema-invalid observer publication through its
        # signed owner and outer record. Without mandatory full-chain closure
        # and constants, both mutants would otherwise be cryptographically
        # consistent and accepted.
        for field, replacement in (
            ("issue_count", 2),
            ("unexpected_field", "synthetic-unknown-field"),
        ):
            mutant_chain = copy.deepcopy(chain)
            invalid_completion = mutant_chain[9]
            invalid_completion[field] = replacement
            if field == "issue_count":
                self.rebind_publication(invalid_completion, "observer")
            else:
                self.resign_publication(invalid_completion, "observer")
            self.rebind_observer_evidence_publication(mutant_chain)
            with self.assertRaises(ValidationError, msg=field):
                validate_recovery_chain(*mutant_chain)

        # A signature made by the other domain key is not authority even when
        # the record retains the expected observer KID and all exact content.
        mutant_chain = copy.deepcopy(chain)
        cross_key = mutant_chain[5]
        self.resign_publication(cross_key, "writer")
        with self.assertRaises(ValidationError):
            validate_recovery_chain(*mutant_chain)

        # The publication fixtures themselves are canonical and duplicate-key
        # hostile, rather than relying on the enclosing records to detect it.
        raw = (FIXTURES / "writer-authorization-publication.valid.json").read_bytes()
        duplicate = raw.replace(
            b'{"authority_domain":"writer"',
            b'{"authority_domain":"writer","authority_domain":"writer"',
            1,
        )
        with self.assertRaises(ValidationError):
            strict_json_loads(duplicate)

    def test_observer_lifecycles_are_closed_signed_and_chain_bound(self):
        chain = list(self.recovery_chain())
        cases = (
            (7, "observer-admission-lifecycle.schema.json", True),
            (10, "observer-cleanup-lifecycle.schema.json", False),
        )
        for index, schema_name, admitted in cases:
            lifecycle = chain[index]
            validate_evidence(lifecycle, self.schema(schema_name))
            self.assertFalse(lifecycle["source_trust_present"])
            self.assertFalse(lifecycle["writer_material_present"])
            for field in (
                "recovery_trust_present", "observer_binding_present",
                "observer_credential_present", "observer_leaf_present",
                "observer_capability_present", "observer_mtls_present",
            ):
                self.assertIs(lifecycle[field], admitted)
            for field, replacement in (
                ("unexpected_field", "synthetic-unknown-field"),
                ("issue_count", 2),
                ("source_trust_present", True),
                ("writer_material_present", True),
                ("bound_publication_record_sha256", "ab" * 32),
                ("root_key_sha256", "ac" * 32),
                ("signing_kid", "synthetic-observer-deadbeefdeadbeef"),
            ):
                mutant = copy.deepcopy(lifecycle)
                mutant[field] = replacement
                self.assert_semantically_rejected(mutant, schema_name)
                mutant_chain = copy.deepcopy(chain)
                mutant_chain[index] = mutant
                with self.assertRaises(ValidationError, msg=(index, field)):
                    validate_recovery_chain(*mutant_chain)

        # Every complete input is closed at the authoritative entry point,
        # including the unsigned outer continuity record and nested objects.
        for index in range(len(chain)):
            mutant = copy.deepcopy(chain)
            mutant[index]["unexpected_field"] = "synthetic-unknown-field"
            with self.assertRaises(ValidationError, msg=index):
                validate_recovery_chain(*mutant)
        mutant = copy.deepcopy(chain)
        mutant[0]["observer"]["unexpected_field"] = "synthetic-unknown-field"
        with self.assertRaises(ValidationError):
            validate_recovery_chain(*mutant)
        mutant = copy.deepcopy(chain)
        mutant[2]["writer_terminal_proof"]["unexpected_field"] = (
            "synthetic-unknown-field"
        )
        with self.assertRaises(ValidationError):
            validate_recovery_chain(*mutant)

        raw_outer = (FIXTURES / "recovery-boundary-continuity.valid.json").read_bytes()
        duplicate_outer = raw_outer.replace(
            b'{"admission_claims_sha256"',
            b'{"admission_claims_sha256":"' + (b"ab" * 32)
            + b'","admission_claims_sha256"',
            1,
        )
        with self.assertRaises(ValidationError):
            strict_json_loads(duplicate_outer)

        # Fully re-sign and rebind a malformed lifecycle value. The
        # authoritative entry point must apply the exact closed schema, not
        # merely accept a cryptographically self-consistent field set.
        malformed_chain = copy.deepcopy(chain)
        malformed_cleanup = malformed_chain[10]
        malformed_cleanup["provider_operation_inventory_sha256"] = "malformed"
        malformed_cleanup["lifecycle_result_sha256"] = hashlib.sha256(
            canonical_bytes({
                field: malformed_cleanup[field]
                for field in OBSERVER_CLEANUP_LIFECYCLE_RESULT_FIELDS
            })
        ).hexdigest()
        malformed_signature = ed25519_sign(
            bytes(range(32, 64)),
            domain_separated_message(
                "observer-cleanup-lifecycle/v2",
                signed_record_payload(malformed_cleanup, "signature"),
            ),
        )
        malformed_cleanup["signature"] = base64url_encode(malformed_signature)
        malformed_cleanup["signature_sha256"] = hashlib.sha256(
            malformed_signature
        ).hexdigest()
        malformed_outer = malformed_chain[11]
        malformed_outer["observer_cleanup_lifecycle_sha256"] = (
            canonical_record_sha256(malformed_cleanup)
        )
        malformed_outer["outer_continuity_sha256"] = hashlib.sha256(
            canonical_bytes({
                field: malformed_outer[field]
                for field in OUTER_CONTINUITY_FIELDS
            })
        ).hexdigest()
        with self.assertRaisesRegex(ValidationError, "pattern mismatch"):
            validate_recovery_chain(*malformed_chain)

    def test_full_chain_rejects_every_temporal_boundary_move(self):
        chain = list(self.recovery_chain())
        mutations = (
            (2, "provider_read_one_at", "2098-12-31T23:59:59Z"),
            (2, "provider_read_one_at", "2099-01-01T00:00:00Z"),
            (2, "provider_read_two_at", "2099-01-01T00:00:04Z"),
            (3, "requested_at", "2099-01-01T00:00:02Z"),
            (3, "completed_at", "2099-01-01T00:00:05Z"),
            (5, "requested_at", "2099-01-01T00:00:04Z"),
            (5, "completed_at", "2099-01-01T00:00:07Z"),
            (7, "requested_at", "2099-01-01T00:00:05Z"),
            (7, "completed_at", "2099-01-01T00:00:08Z"),
            (8, "recovery_read_one_at", "2099-01-01T00:00:07Z"),
            (8, "recovery_read_two_at", "2099-01-01T00:00:11Z"),
            (9, "requested_at", "2099-01-01T00:00:09Z"),
            (9, "completed_at", "2099-01-01T00:00:13Z"),
            (10, "requested_at", "2099-01-01T00:00:12Z"),
            (10, "completed_at", "2099-01-01T00:00:13Z"),
        )
        for index, field, replacement in mutations:
            mutant = copy.deepcopy(chain)
            mutant[index][field] = replacement
            with self.assertRaises(ValidationError, msg=(index, field)):
                validate_recovery_chain(*mutant)

        # The signed evidence publication remains invalid if its owner is
        # issued at or after the publication record's exclusive expiry, even
        # when every dependent canonical digest and signature is rebound.
        mutant = copy.deepcopy(chain)
        evidence_publication = mutant[9]
        evidence_publication["expires_at"] = "2099-01-01T00:00:12Z"
        self.resign_publication(evidence_publication, "observer")
        observer = mutant[8]
        observer["issued_at"] = "2099-01-01T00:00:12Z"
        observer["expires_at"] = "2099-01-01T00:05:12Z"
        self.rebind_observer_evidence_publication(mutant)
        with self.assertRaises(ValidationError):
            validate_recovery_chain(*mutant)

    def test_cross_language_domain_separated_known_answer_uses_real_ed25519(self):
        # Synthetic signing material is deliberately test-code-local rather
        # than exposed as a public evidence fixture.
        domain = b"writer-receipt/v1"
        payload = (
            b'{"marker_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
            b'aaaaaaaaaaaaaaaa","operation_id":"synthetic-operation-001"}'
        )
        message = (
            b"rereply-recovery-boundary/signature/v1\x00"
            + struct.pack(">H", len(domain)) + domain
            + struct.pack(">Q", len(payload)) + payload
        )
        self.assertEqual(
            hashlib.sha256(message).hexdigest(),
            "d95bc4eeb1c883cfc65d0217cac27f2f47525a65a3c1714989d567ebf893df9a",
        )
        seed = bytes(range(32))
        signature = bytes.fromhex(
            "d39cfe5bc3588d3938ee8c755fc9c01f87cfd2966a3e35c5844490bc3ee98dde"
            "e5ac7c74ffb8e09655a14e7c156a748a1f3be44943406fef4409e89f33fab00e"
        )
        public_key = ed25519_public_key_from_seed(seed)
        self.assertEqual(ed25519_sign(seed, message), signature)
        self.assertTrue(ed25519_verify(public_key, message, signature))
        self.assertFalse(ed25519_verify(public_key, message + b"!", signature))
        changed = bytearray(signature)
        changed[0] ^= 1
        self.assertFalse(ed25519_verify(public_key, message, bytes(changed)))

    def test_receipts_bind_separate_control_runtime_workflow_config_image_and_spec(self):
        for fixture_name in (
            "writer-receipt.valid.json", "observer-receipt.valid.json",
            "lifecycle-receipt.valid.json",
        ):
            value = load_json(FIXTURES / fixture_name)
            hashes = [value[field] for field in HASH_FIELDS]
            self.assertEqual(len(hashes), 6)
            self.assertEqual(len(set(hashes)), 6)

    def test_observer_is_recovery_only_and_reads_exactly_twice(self):
        observer = load_json(FIXTURES / "observer-receipt.valid.json")
        writer = load_json(FIXTURES / "writer-receipt.valid.json")
        self.assertEqual(observer["source_read_count"], 0)
        self.assertEqual(observer["recovery_read_count"], 2)
        self.assertEqual(observer["read_sequence"], ["recovery", "recovery"])
        self.assertEqual(observer["minimum_read_separation_seconds"], 2)
        self.assertNotEqual(observer["recovery_read_one_at"], observer["recovery_read_two_at"])
        self.assertFalse(observer["writer_receipt_seen"])
        self.assertFalse(observer["expected_marker_hash_seen"])
        self.assertFalse(observer["raw_marker_returned"])
        self.assertEqual(observer["fixed_key_sha256"], writer["fixed_key_sha256"])
        self.assertEqual(observer["marker_sha256"], writer["marker_sha256"])

        oracle = BoundaryStateOracle()
        for target in (
            "WRITER_ADMITTED", "MARKER_COMMITTED", "WRITER_REVOKING",
            "WRITER_DELETED", "SOURCE_STABLE", "FORK_ISSUED",
            "FORK_RECONCILED", "RECOVERY_ADMISSION_AUTHORIZED",
            "RECOVERY_ADMISSION_PUBLISHED", "OBSERVER_ADMITTED",
        ):
            oracle.transition(target)
        oracle.read_recovery()
        oracle.read_recovery()
        self.assertEqual((oracle.recovery_reads, oracle.source_reads), (2, 0))
        with self.assertRaises(OracleError):
            oracle.read_recovery()
        with self.assertRaises(OracleError):
            oracle.read_source()

    def test_observer_admission_cannot_skip_either_durable_publication_state(self):
        oracle = BoundaryStateOracle()
        for target in (
            "WRITER_ADMITTED", "MARKER_COMMITTED", "WRITER_REVOKING",
            "WRITER_DELETED", "SOURCE_STABLE", "FORK_ISSUED",
            "FORK_RECONCILED",
        ):
            oracle.transition(target)
        with self.assertRaisesRegex(OracleError, "invalid transition"):
            oracle.transition("OBSERVER_ADMITTED")
        with self.assertRaisesRegex(OracleError, "invalid transition"):
            oracle.transition("RECOVERY_ADMISSION_PUBLISHED")
        oracle.transition("RECOVERY_ADMISSION_AUTHORIZED")
        with self.assertRaisesRegex(OracleError, "invalid transition"):
            oracle.transition("OBSERVER_ADMITTED")
        oracle.transition("RECOVERY_ADMISSION_PUBLISHED")
        oracle.transition("OBSERVER_ADMITTED")

    def test_writer_deletion_is_a_hard_fork_guard(self):
        oracle = BoundaryStateOracle()
        oracle.transition("WRITER_ADMITTED")
        oracle.transition("MARKER_COMMITTED")
        with self.assertRaises(OracleError):
            oracle.transition("FORK_ISSUED")
        oracle.transition("WRITER_REVOKING")
        with self.assertRaises(OracleError):
            oracle.transition("SOURCE_STABLE")
        oracle.transition("WRITER_DELETED")
        oracle.transition("SOURCE_STABLE")
        oracle.transition("FORK_ISSUED")
        proof = load_json(FIXTURES / "lifecycle-receipt.valid.json")["writer_terminal_proof"]
        self.assertTrue(proof["broker_deleted"])
        self.assertTrue(proof["direct_get_absent"])
        self.assertTrue(proof["app_inventory_pagination_complete"])
        self.assertTrue(proof["deployment_inventory_pagination_complete"])
        self.assertTrue(proof["provider_operation_inventory_pagination_complete"])
        self.assertEqual(proof["app_inventory_count"], 0)
        self.assertEqual(proof["deployment_inventory_count"], 0)
        self.assertEqual(proof["nonterminal_provider_operation_count"], 0)
        self.assertEqual(proof["rollback_capable_deployment_count"], 0)
        self.assertTrue(proof["delete_action_terminal"])
        self.assertTrue(proof["old_instance_grace_elapsed"])
        self.assertEqual(
            proof["provider_operation_inventory_sha256"],
            proof["action_ledger_read_one_sha256"],
        )
        self.assertEqual(
            proof["action_ledger_read_one_sha256"],
            proof["action_ledger_read_two_sha256"],
        )

    def test_authorization_is_fresh_and_separate_from_operation_digest(self):
        for name in (
            "writer-receipt.valid.json", "observer-receipt.valid.json",
            "lifecycle-receipt.valid.json",
        ):
            value = load_json(FIXTURES / name)
            self.assertNotEqual(value["operation_sha256"], value["authorization_sha256"])
        ledger = FaultLedger()
        self.assertIsNone(ledger.authorize(
            "synthetic-operation", 1, "operation-digest", "fresh-auth-one"
        ))
        ledger.issue_once(
            "synthetic-operation", 1, "operation-digest", "marker-cas-v2",
            "marker-request-digest", "effect-auth-one",
        )
        self.assertEqual(
            ledger.reconcile(
                "synthetic-operation", 1, "operation-digest", "marker-cas-v2",
                "marker-request-digest", "marker-observation-digest", 1,
                "reconcile-auth-one",
            ),
            "reconciled-one",
        )
        ledger.commit_response(
            "synthetic-operation", 1, "operation-digest", "marker-cas-v2",
            "marker-request-digest", b"canonical-response",
        )
        self.assertEqual(
            ledger.authorize(
                "synthetic-operation", 1, "operation-digest", "fresh-auth-two"
            ),
            b"canonical-response",
        )
        with self.assertRaises(OracleError):
            ledger.authorize(
                "synthetic-operation", 1, "operation-digest", "fresh-auth-one"
            )
        with self.assertRaises(OracleError):
            ledger.authorize(
                "synthetic-operation", 2, "operation-digest", "fresh-auth-three"
            )
        self.assertTrue(ledger.is_quarantined("synthetic-operation"))
        # Authentication is consumed before either operation-level semantic
        # check.  Replaying an envelope after the semantic failure must report
        # replay, not get another chance to supply a repaired body.
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.authorize(
                "synthetic-operation", 1, "operation-digest", "fresh-auth-three"
            )

        with self.assertRaisesRegex(OracleError, "authorization digest"):
            ledger.authorize("new-operation", 1, "same-digest", "same-digest")
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.authorize(
                "new-operation", 1, "repaired-operation-digest", "same-digest"
            )

    def test_status_is_fresh_read_only_and_digest_mismatch_quarantines_durably(self):
        ledger = FaultLedger()
        ledger.authorize("operation", 1, "body-a", "mutation-auth-1")
        ledger.issue_once(
            "operation", 1, "body-a", "fork-post", "fork-request-a", "effect-auth-1"
        )
        ledger.reconcile(
            "operation", 1, "body-a", "fork-post", "fork-request-a",
            "fork-observation-one", 1, "reconcile-auth-1",
        )
        ledger.commit_response(
            "operation", 1, "body-a", "fork-post", "fork-request-a",
            b"signed-terminal-response",
        )
        self.assertEqual(
            ledger.status(
                "operation", 1, "body-a", "fork-post", "fork-request-a",
                "status-auth-1",
            ),
            ("reconciled-one", b"signed-terminal-response"),
        )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.status(
                "operation", 1, "body-a", "fork-post", "fork-request-a",
                "status-auth-1",
            )
        with self.assertRaises(TypeError):
            ledger.status("operation", 1, "body-a", "fork-post", "fork-request-a")
        with self.assertRaisesRegex(OracleError, "authorization digest"):
            ledger.status(
                "operation", 1, "body-a", "fork-post", "fork-request-a", "body-a"
            )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.status(
                "operation", 1, "body-a", "fork-post", "fork-request-a", "body-a"
            )

        for collision in ("fork-request-a", "fork-observation-one"):
            isolated = FaultLedger()
            isolated.authorize("status-collision", 1, "status-body", "status-create")
            isolated.issue_once(
                "status-collision", 1, "status-body", "fork-post",
                "fork-request-a", "status-issue",
            )
            isolated.reconcile(
                "status-collision", 1, "status-body", "fork-post",
                "fork-request-a", "fork-observation-one", 1,
                "status-reconcile",
            )
            with self.assertRaisesRegex(OracleError, "every bound digest"):
                isolated.status(
                    "status-collision", 1, "status-body", "fork-post",
                    "fork-request-a", collision,
                )
            self.assertTrue(isolated.is_quarantined("status-collision"))

        # Status cannot create a missing operation, and its authentication is
        # still consumed before that semantic lookup.
        with self.assertRaisesRegex(OracleError, "unknown operation"):
            ledger.status(
                "missing", 1, "body", "fork-post", "fork-request-missing",
                "status-auth-missing",
            )
        self.assertFalse(ledger.has_operation("missing"))
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.authorize("missing", 1, "body", "status-auth-missing")

        # Looking up a missing effect cannot create or issue it, and also
        # consumes its authentication envelope before the lookup fails.
        with self.assertRaisesRegex(OracleError, "unknown effect"):
            ledger.status(
                "operation", 1, "body-a", "cleanup-delete", "cleanup-request",
                "status-auth-unknown-effect",
            )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.issue_once(
                "operation", 1, "body-a", "cleanup-delete", "cleanup-request",
                "status-auth-unknown-effect",
            )

        # Reusing the stable operation identifier with another immutable body
        # is a durable quarantine. A later correct body cannot recover it.
        with self.assertRaisesRegex(OracleError, "quarantined"):
            ledger.status(
                "operation", 1, "body-b", "fork-post", "fork-request-a",
                "status-auth-mismatch",
            )
        self.assertTrue(ledger.is_quarantined("operation"))
        with self.assertRaisesRegex(OracleError, "quarantined"):
            ledger.status(
                "operation", 1, "body-a", "fork-post", "fork-request-a",
                "status-auth-after-quarantine",
            )
        with self.assertRaisesRegex(OracleError, "quarantined"):
            ledger.commit_response(
                "operation", 1, "body-a", "fork-post", "fork-request-a",
                b"rewritten-response",
            )

    def test_effect_request_and_reconciliation_records_are_immutable(self):
        ledger = FaultLedger()
        ledger.authorize("operation", 7, "body-seven", "authorize-seven")
        self.assertEqual(
            ledger.issue_once(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "issue-seven-one",
            ),
            "issued-ambiguous",
        )
        # Exact request replay is idempotent only with a fresh authentication
        # envelope and never increments the single issue.
        self.assertEqual(
            ledger.issue_once(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "issue-seven-two",
            ),
            "issued-ambiguous",
        )
        self.assertEqual(ledger.issue_count("operation", "fork-post"), 1)
        self.assertEqual(
            ledger.reconcile(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "observation-seven", 1, "reconcile-seven-one",
            ),
            "reconciled-one",
        )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            ledger.reconcile(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "observation-seven", 1, "reconcile-seven-one",
            )
        with self.assertRaises(TypeError):
            ledger.reconcile(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "observation-seven", 1,
            )
        self.assertEqual(
            ledger.reconcile(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "observation-seven", 1, "reconcile-seven-two",
            ),
            "reconciled-one",
        )
        with self.assertRaisesRegex(OracleError, "outcome rewrite"):
            ledger.reconcile(
                "operation", 7, "body-seven", "fork-post", "fork-request-seven",
                "observation-rewritten", 0, "reconcile-seven-three",
            )
        self.assertTrue(ledger.is_quarantined("operation"))

        for effect, request, expected in (
            ("fork-post", "request-a", "effect request rewrite"),
            ("cleanup-delete", "request-shared", "reused across effects"),
        ):
            isolated = FaultLedger()
            isolated.authorize("isolated", 1, "isolated-body", "isolated-authorize")
            isolated.issue_once(
                "isolated", 1, "isolated-body", "fork-post", "request-shared",
                "isolated-issue-one",
            )
            with self.assertRaisesRegex(OracleError, expected):
                isolated.issue_once(
                    "isolated", 1, "isolated-body", effect, request,
                    "isolated-issue-two",
                )
            self.assertTrue(isolated.is_quarantined("isolated"))

        # A single authentication envelope can authorize exactly one ledger
        # method, never two effects or an effect followed by status.
        isolated = FaultLedger()
        isolated.authorize("auth-bound", 1, "auth-bound-body", "auth-bound-create")
        isolated.issue_once(
            "auth-bound", 1, "auth-bound-body", "fork-post", "auth-bound-fork",
            "one-effect-auth",
        )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            isolated.issue_once(
                "auth-bound", 1, "auth-bound-body", "cleanup-delete",
                "auth-bound-cleanup", "one-effect-auth",
            )
        with self.assertRaisesRegex(OracleError, "authorization replay"):
            isolated.status(
                "auth-bound", 1, "auth-bound-body", "fork-post",
                "auth-bound-fork", "one-effect-auth",
            )

        # Reconciliation and status bind the exact effect request digest; a
        # fresh envelope cannot repair request drift after it is observed.
        for method in ("reconcile", "status"):
            isolated = FaultLedger()
            isolated.authorize("request-bound", 1, "request-body", "request-create")
            isolated.issue_once(
                "request-bound", 1, "request-body", "fork-post", "request-original",
                "request-issue",
            )
            with self.assertRaisesRegex(OracleError, "request mismatch"):
                if method == "reconcile":
                    isolated.reconcile(
                        "request-bound", 1, "request-body", "fork-post",
                        "request-drift", "observation-drift", 1,
                        "request-reconcile",
                    )
                else:
                    isolated.status(
                        "request-bound", 1, "request-body", "fork-post",
                        "request-drift", "request-status",
                    )
            self.assertTrue(isolated.is_quarantined("request-bound"))

        for generation, body, expected in (
            (2, "identity-body", "generation/body mismatch"),
            (1, "identity-body-drift", "generation/body mismatch"),
        ):
            isolated = FaultLedger()
            isolated.authorize("identity", 1, "identity-body", "identity-create")
            isolated.issue_once(
                "identity", 1, "identity-body", "fork-post", "identity-request",
                "identity-issue",
            )
            with self.assertRaisesRegex(OracleError, expected):
                isolated.reconcile(
                    "identity", generation, body, "fork-post", "identity-request",
                    "identity-observation", 1,
                    f"identity-reconcile-{generation}-{body}",
                )
            self.assertTrue(isolated.is_quarantined("identity"))

        isolated = FaultLedger()
        isolated.authorize("negative", 1, "negative-body", "negative-create")
        isolated.issue_once(
            "negative", 1, "negative-body", "fork-post", "negative-request",
            "negative-issue",
        )
        with self.assertRaisesRegex(OracleError, "negative reconciliation"):
            isolated.reconcile(
                "negative", 1, "negative-body", "fork-post", "negative-request",
                "negative-observation", -1, "negative-reconcile",
            )
        self.assertTrue(isolated.is_quarantined("negative"))

        isolated = FaultLedger()
        isolated.authorize("response", 1, "response-body", "response-create")
        with self.assertRaisesRegex(OracleError, "lacks an issued effect"):
            isolated.commit_response(
                "response", 1, "response-body", "fork-post",
                "response-request", b"response-one",
            )
        self.assertTrue(isolated.is_quarantined("response"))

        isolated = FaultLedger()
        isolated.authorize("response", 1, "response-body", "response-create")
        isolated.issue_once(
            "response", 1, "response-body", "fork-post", "response-request",
            "response-issue",
        )
        isolated.reconcile(
            "response", 1, "response-body", "fork-post", "response-request",
            "response-observation", 1, "response-reconcile",
        )
        isolated.commit_response(
            "response", 1, "response-body", "fork-post", "response-request",
            b"response-one",
        )
        isolated.commit_response(
            "response", 1, "response-body", "fork-post", "response-request",
            b"response-one",
        )
        with self.assertRaisesRegex(OracleError, "terminal response changed"):
            isolated.commit_response(
                "response", 1, "response-body", "fork-post",
                "response-request", b"response-two",
            )
        self.assertTrue(isolated.is_quarantined("response"))

        # Every endpoint burns a fresh envelope and rejects equality with any
        # body/request/observation digest it is asked to bind.
        for collision in ("collision-body", "collision-request"):
            isolated = FaultLedger()
            isolated.authorize(
                "collision", 1, "collision-body", "collision-create"
            )
            with self.assertRaisesRegex(OracleError, "every bound digest"):
                isolated.issue_once(
                    "collision", 1, "collision-body", "fork-post",
                    "collision-request", collision,
                )
            with self.assertRaisesRegex(OracleError, "authorization replay"):
                isolated.issue_once(
                    "collision", 1, "collision-body", "fork-post",
                    "collision-request", collision,
                )

        for collision in (
            "reconcile-body", "reconcile-request", "reconcile-observation",
        ):
            isolated = FaultLedger()
            isolated.authorize(
                "reconcile-collision", 1, "reconcile-body",
                "reconcile-create",
            )
            isolated.issue_once(
                "reconcile-collision", 1, "reconcile-body", "fork-post",
                "reconcile-request", "reconcile-issue",
            )
            with self.assertRaisesRegex(OracleError, "every bound digest"):
                isolated.reconcile(
                    "reconcile-collision", 1, "reconcile-body", "fork-post",
                    "reconcile-request", "reconcile-observation", 1, collision,
                )
            with self.assertRaisesRegex(OracleError, "authorization replay"):
                isolated.reconcile(
                    "reconcile-collision", 1, "reconcile-body", "fork-post",
                    "reconcile-request", "reconcile-observation", 1, collision,
                )

        # A later endpoint cannot use any digest already committed by an
        # earlier endpoint as its fresh authentication envelope.
        for endpoint in ("authorize", "issue"):
            isolated = FaultLedger()
            isolated.authorize(
                "historical-collision", 1, "historical-body", "historical-create"
            )
            isolated.issue_once(
                "historical-collision", 1, "historical-body", "fork-post",
                "historical-request", "historical-issue",
            )
            isolated.reconcile(
                "historical-collision", 1, "historical-body", "fork-post",
                "historical-request", "historical-observation", 1,
                "historical-reconcile",
            )
            with self.assertRaisesRegex(OracleError, "every bound digest"):
                if endpoint == "authorize":
                    isolated.authorize(
                        "historical-collision", 1, "historical-body",
                        "historical-observation",
                    )
                else:
                    isolated.issue_once(
                        "historical-collision", 1, "historical-body",
                        "cleanup-delete", "historical-cleanup",
                        "historical-observation",
                    )
            self.assertTrue(isolated.is_quarantined("historical-collision"))

        isolated = FaultLedger()
        isolated.authorize("ambiguous", 1, "ambiguous-body", "ambiguous-create")
        isolated.issue_once(
            "ambiguous", 1, "ambiguous-body", "fork-post",
            "ambiguous-request", "ambiguous-issue",
        )
        with self.assertRaisesRegex(OracleError, "reconciled-one"):
            isolated.commit_response(
                "ambiguous", 1, "ambiguous-body", "fork-post",
                "ambiguous-request", b"must-not-commit",
            )
        self.assertTrue(isolated.is_quarantined("ambiguous"))

    def test_every_exact_low_level_side_effect_has_ambiguity_reconciliation(self):
        self.assertEqual(tuple(SIDE_EFFECTS), tuple(LIFECYCLE_EFFECTS))
        model_source = (ROOT / "internal" / "model" / "types.go").read_text(encoding="utf-8")
        model_effects = tuple(re.findall(
            r'^\s*Effect\w+\s+Effect\s*=\s*"([^"]+)"$',
            model_source,
            re.MULTILINE,
        ))
        self.assertIn("evidence-observe", model_effects)
        model_mutating_effects = tuple(
            effect for effect in model_effects if effect != "evidence-observe"
        )
        self.assertEqual(model_mutating_effects, tuple(SIDE_EFFECTS))
        self.assertNotIn("evidence-observe", LIFECYCLE_EFFECTS)
        lifecycle_schema = self.schema("lifecycle-receipt.schema.json")
        self.assertEqual(
            tuple(lifecycle_schema["properties"]["side_effect"]["enum"]),
            tuple(SIDE_EFFECTS),
        )
        for index, side_effect in enumerate(SIDE_EFFECTS):
            for observed, expected in (
                (0, "reconciled-zero-terminal"),
                (1, "reconciled-one"),
                (2, "reconciled-multiple-terminal"),
            ):
                ledger = FaultLedger()
                operation = f"operation-{index}-{observed}"
                body = f"digest-{index}-{observed}"
                request = f"request-{index}-{observed}"
                ledger.authorize(
                    operation, 1, body, f"authorization-{index}-{observed}"
                )
                ledger.issue_once(
                    operation, 1, body, side_effect, request,
                    f"issue-authorization-{index}-{observed}",
                )
                self.assertEqual(
                    ledger.reconcile(
                        operation, 1, body, side_effect, request,
                        f"observation-{index}-{observed}", observed,
                        f"reconcile-authorization-{index}-{observed}",
                    ),
                    expected,
                )
                self.assertEqual(ledger.issue_count(operation, side_effect), 1)
                if observed == 1:
                    self.assertEqual(
                        ledger.issue_once(
                            operation, 1, body, side_effect, request,
                            f"idempotent-issue-authorization-{index}-{observed}",
                        ),
                        expected,
                    )
                    ledger.commit_response(
                        operation, 1, body, side_effect, request,
                        b"synthetic-terminal-response",
                    )
                else:
                    self.assertTrue(ledger.is_quarantined(operation))
                    with self.assertRaisesRegex(OracleError, "quarantined"):
                        ledger.issue_once(
                            operation, 1, body, side_effect, request,
                            f"idempotent-issue-authorization-{index}-{observed}",
                        )
                    with self.assertRaisesRegex(OracleError, "quarantined"):
                        ledger.commit_response(
                            operation, 1, body, side_effect, request,
                            b"must-not-commit",
                        )

    def test_authority_domains_have_four_apps_two_ledgers_two_roots_all_distinct(self):
        evidence = load_json(FIXTURES / "domain-separation.valid.json")
        categories = ("authority_app_sha256", "broker_app_sha256", "ledger_sha256", "root_key_sha256")
        identities = [evidence[domain][key] for domain in ("writer", "observer") for key in categories]
        self.assertEqual(len(identities), 8)
        self.assertEqual(len(set(identities)), 8)
        self.assertNotEqual(evidence["writer"]["signing_kid"], evidence["observer"]["signing_kid"])
        self.assertNotEqual(
            evidence["writer"]["signing_public_key"],
            evidence["observer"]["signing_public_key"],
        )

    def test_public_fixtures_protect_raw_identifiers(self):
        forbidden_keys = {
            "app_id", "cluster_id", "database_id", "deployment_id", "endpoint",
            "hostname", "host", "url", "uri", "credential", "password", "token",
            "uuid", "marker",
        }
        uuid_pattern = re.compile(
            r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
            r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b"
        )
        for fixture_name in VALID_FIXTURES:
            value = load_json(FIXTURES / fixture_name)
            for key, child in iter_items(value):
                self.assertNotIn(key.lower(), forbidden_keys, (fixture_name, key))
                if isinstance(child, str):
                    self.assertNotIn("://", child, fixture_name)
                    self.assertIsNone(uuid_pattern.search(child), fixture_name)

    def test_gate_c_only_claims_and_numeric_resource_ceiling(self):
        for fixture_name in VALID_FIXTURES:
            value = load_json(FIXTURES / fixture_name)
            self.assertEqual(value["claim_scope"], "gate-c-nonproduction-only")
            self.assertTrue(value["non_authoritative"])
        ceiling = (DOCS / "gate-c-resource-ceiling.md").read_text(encoding="utf-8")
        for exact_line in (
            "MAX_PROJECTS=1", "MAX_PROVIDER_TOKENS=12", "MAX_PROVIDER_ACTIONS=64",
            "MAX_APP_DEPLOYMENTS=12", "MAX_APP_REDEPLOYMENTS=4", "MAX_REPLICAS_PER_APP=2",
            "MAX_SCALE_EVENTS=4", "MAX_AUTHORITY_APPS=2", "MAX_BROKER_APPS=2",
            "MAX_LEDGER_CLUSTERS=2", "MAX_SIGNING_ROOTS=2",
            "MAX_SOURCE_VALKEY_CLUSTERS=1", "MAX_RECOVERY_VALKEY_FORKS=1",
            "MAX_CUSTOM_DOMAINS=0",
        ):
            self.assertIn(exact_line, ceiling)

    def test_docs_contain_required_fail_closed_boundaries(self):
        text = "\n".join(path.read_text(encoding="utf-8") for path in sorted(DOCS.glob("*.md"))).lower()
        for phrase in (
            "observer has recovery-only reach", "exactly two reads",
            "writer deletion is mandatory", "fresh authorization", "zero/one/multiple",
            "raw identifiers remain in protected descriptors", "pairwise distinct",
            "gate-c nonproduction only", "cleanup authority is unconditional",
            "explicit user risk acceptance", "protected controller", "two ledgers",
            "two signing roots", "ephemeral writer broker",
        ):
            self.assertIn(phrase, text)
        self.assertNotIn("production-ready", text)
        self.assertNotIn("production-safe", text)

    def test_docs_preserve_two_step_recovery_admission_authority(self):
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        runbook = (DOCS / "state-machine-runbook.md").read_text(encoding="utf-8")
        threat_model = (DOCS / "threat-model.md").read_text(encoding="utf-8")
        authorization_state = runbook.index("| `RECOVERY_ADMISSION_AUTHORIZED` |")
        publication_state = runbook.index("| `RECOVERY_ADMISSION_PUBLISHED` |")
        observer_state = runbook.index("| `OBSERVER_ADMITTED` |")
        self.assertLess(authorization_state, publication_state)
        self.assertLess(publication_state, observer_state)
        for source in (readme, runbook, threat_model):
            self.assertIn("RecoveryAdmissionAuthorization", source)
            self.assertIn("writer ledger", source)
            self.assertIn("observer authority", source)
            self.assertIn("observer ledger", source)
            self.assertIn("status reconciliation", source)
        normalized_readme = " ".join(readme.split())
        self.assertIn("It is not an observer signature or admission.", normalized_readme)
        self.assertIn("both the signature and the matching completed ledger record", normalized_readme)
        self.assertIn("A signed but unpublished receipt is invalid.", threat_model)
        self.assertIn("These are two distinct authorized effects, ledgers, and failure\n", runbook)

    def test_tests_make_external_network_impossible(self):
        for host in ("127.0.0.1", "::1"):
            self.assertTrue(ipaddress.ip_address(host).is_loopback)
        with self.assertRaisesRegex(AssertionError, "external DNS/network"):
            socket.getaddrinfo("external.invalid", 443)
        with self.assertRaisesRegex(AssertionError, "external DNS/network"):
            urllib.request.urlopen("data:text/plain,inert")
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as stream:
            with self.assertRaisesRegex(AssertionError, "external DNS/network"):
                stream.connect((str(ipaddress.IPv4Address(0xC6336401)), 443))
            with self.assertRaisesRegex(AssertionError, "external DNS/network"):
                stream.connect_ex((str(ipaddress.IPv4Address(0xCB007101)), 443))
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as datagram:
            with self.assertRaisesRegex(AssertionError, "external DNS/network"):
                datagram.sendto(b"synthetic", (str(ipaddress.IPv4Address(0xC0000201)), 53))

    def test_gate_a_checker_rejects_adversarial_workflow_language_and_text_surfaces(self):
        import_util = __import__("importlib.util", fromlist=["util"])
        verifier_path = ROOT / "verify_gate_a.py"
        spec = import_util.spec_from_file_location("gate_a_checker_for_tests", verifier_path)
        verifier = import_util.module_from_spec(spec)
        spec.loader.exec_module(verifier)

        workflow_path = ROOT / "workflows" / "build.tmpl"
        workflow = workflow_path.read_text(encoding="utf-8")
        verifier.check_workflows([(workflow_path, workflow)])
        workflow_mutants = (
            workflow.replace("on: []", "on: []\non: []", 1),
            workflow.replace("on: []", '"on": []', 1),
            workflow.replace("on: []", "on : []", 1),
            workflow.replace(
                "permissions:\n  contents: read",
                'permissions:\n  contents: read\n  "id-token": write',
                1,
            ),
            workflow + "\n  inert:\n    if: ${{ false }}\n    runs-on: ubuntu-latest\n",
        )
        relative = workflow_path.relative_to(ROOT).as_posix()
        for mutant in workflow_mutants:
            with self.subTest(workflow_sha256=hashlib.sha256(mutant.encode()).hexdigest()):
                with mock.patch.dict(
                    verifier.EXPECTED_WORKFLOW_SHA256,
                    {relative: hashlib.sha256(mutant.encode()).hexdigest()},
                ):
                    with self.assertRaises(AssertionError):
                        verifier.check_workflows([(workflow_path, mutant)])

        go_probe = ROOT / "internal" / "model" / "network_probe.go"
        with self.assertRaisesRegex(AssertionError, "unapproved Go import"):
            verifier.check_language_surface(
                go_probe,
                'package model\nimport mail "net/smtp"\nvar _ = mail.SendMail\n',
            )
        python_probe = ROOT / "tests" / "fault_oracle.py"
        for source in ("import socket as wire\n", "import urllib.request as transport\n"):
            with self.assertRaises(AssertionError):
                verifier.check_language_surface(python_probe, source)

        text_probe = ROOT / "fixtures" / "synthetic-probe.json"
        forbidden_values = (
            "dop_" + "v1_" + ("A" * 24),
            "https://" + "example.com/path",
            str(ipaddress.IPv4Address(0xC6336401)),
            "12345678-1234-" + "4234-9234-123456789abc",
        )
        for value in forbidden_values:
            with self.subTest(forbidden=value):
                with self.assertRaises(AssertionError):
                    verifier.check_text(text_probe, value)

        selector_probe = ROOT / "docs" / "synthetic-probe.md"
        for value in (
            "[2001" + ":db8::1]",
            "ftp" + "://synthetic.invalid/object",
            "synthetic.invalid" + ":6379",
        ):
            with self.subTest(selector=value):
                with self.assertRaises(AssertionError):
                    verifier.check_text(selector_probe, value)

        for fixture_text in (
            '{"signing_' + 'seed":"synthetic-label"}',
            '{"ed25519_' + 'private_material":"synthetic-label"}',
            '{"secret_' + 'key_bytes":"synthetic-label"}',
        ):
            with self.subTest(private_fixture=fixture_text):
                with self.assertRaises(AssertionError):
                    verifier.check_text(text_probe, fixture_text)

        bare_hostname_fixture = (
            '{"semantic_label":"provider' + '.example"}'
        )
        with self.assertRaisesRegex(AssertionError, "bare hostname-shaped"):
            verifier.check_text(text_probe, bare_hostname_fixture)
        absolute_hostname_fixture = (
            '{"semantic_label":"provider' + '.example."}'
        )
        with self.assertRaisesRegex(AssertionError, "bare hostname-shaped"):
            verifier.check_text(text_probe, absolute_hostname_fixture)
        verifier.check_text(
            text_probe, '{"semantic_label":"synthetic' + '.invalid"}'
        )

    def test_gate_a_checker_rejects_static_python_literal_reconstruction(self):
        import_util = __import__("importlib.util", fromlist=["util"])
        verifier_path = ROOT / "verify_gate_a.py"
        spec = import_util.spec_from_file_location(
            "gate_a_static_literal_checker_for_tests", verifier_path
        )
        verifier = import_util.module_from_spec(spec)
        spec.loader.exec_module(verifier)

        verifier.check_text(
            verifier_path, verifier_path.read_text(encoding="utf-8")
        )

        protected = "synthetic-protected-boundary-value-729"
        protected_sha256 = hashlib.sha256(
            protected.casefold().encode("utf-8")
        ).hexdigest()
        prefix = "synthetic-protected-"
        suffix = "boundary-value-729"
        python_probe = ROOT / "tests" / "fault_oracle.py"
        sources = {
            "direct": f'value = "{protected}"\n',
            "adjacent": f'value = "{prefix}" "{suffix}"\n',
            "split": (
                "value = (\n"
                f'    "{prefix}"\n'
                f'    "{suffix}"\n'
                ")\n"
            ),
            "concatenated": f'value = "{prefix}" + "{suffix}"\n',
            "formatted_value": (
                f'value = f"{{\'{prefix}\'}}{{\'{suffix}\'}}"\n'
            ),
            "joined": (
                f'value = "".join(["{prefix}", "{suffix}"])\n'
            ),
            "percent_formatted": (
                f'value = "%s%s" % ("{prefix}", "{suffix}")\n'
            ),
            "method_formatted": (
                f'value = "{{}}{{}}".format("{prefix}", "{suffix}")\n'
            ),
            "propagated_names": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "value = prefix + suffix\n"
            ),
            "named_list_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = [prefix, suffix]\n"
                'value = "".join(parts)\n'
            ),
            "named_tuple_percent": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = (prefix, suffix)\n"
                'value = "%s%s" % parts\n'
            ),
            "named_mapping_percent": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"left": prefix, "right": suffix}\n'
                'value = "%(left)s%(right)s" % parts\n'
            ),
            "literal_mapping_percent": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "%(left)s%(right)s" % '
                '{"left": prefix, "right": suffix}\n'
            ),
            "starred_method_format": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = (prefix, suffix)\n"
                'value = "{}{}".format(*parts)\n'
            ),
            "augmented_assignment": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "value = prefix\n"
                "value += suffix\n"
            ),
            "tuple_unpack": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "left, right = (prefix, suffix)\n"
                "value = left + right\n"
            ),
            "constant_branch": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "value = (prefix if True else \"unused\") + suffix\n"
            ),
            "literal_slice": (
                f'value = ("x" + "{prefix}" + "{suffix}" + "y")[1:-1]\n'
            ),
            "generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(part for part in [prefix, suffix])\n'
            ),
            "if_then_augmented_assignment": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "if True:\n"
                "    value = prefix\n"
                "value += suffix\n"
            ),
            "named_list_concatenation_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = [prefix] + [suffix]\n"
                'value = "".join(parts)\n'
            ),
            "named_tuple_concatenation_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = (prefix,) + (suffix,)\n"
                'value = "".join(parts)\n'
            ),
            "unpack_named_sequence": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = (prefix, suffix)\n"
                "left, right = parts\n"
                "value = left + right\n"
            ),
            "nested_tuple_unpack": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = ((prefix,), (suffix,))\n"
                "((left,), (right,)) = parts\n"
                "value = left + right\n"
            ),
            "starred_target_unpack": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = (prefix, suffix)\n"
                "left, *right = parts\n"
                'value = left + "".join(right)\n'
            ),
            "starred_sequence_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "tail = [suffix]\n"
                "parts = [prefix, *tail]\n"
                'value = "".join(parts)\n'
            ),
            "named_slice_bounds": (
                f'wrapped = "x" + "{prefix}" + "{suffix}" + "y"\n'
                "start = 1\n"
                "end = -1\n"
                "value = wrapped[start:end]\n"
            ),
            "filtered_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(part for part in [prefix, suffix] if True)\n'
            ),
            "identity_slice_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(part[:] for part in [prefix, suffix])\n'
            ),
            "format_kwargs_mapping": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"left": prefix, "right": suffix}\n'
                'value = "{left}{right}".format(**parts)\n'
            ),
            "format_map_mapping": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"left": prefix, "right": suffix}\n'
                'value = "{left}{right}".format_map(parts)\n'
            ),
            "format_map_dict_constructor": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "{left}{right}".format_map('
                'dict(left=prefix, right=suffix))\n'
            ),
            "format_map_dict_pairs": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "{left}{right}".format_map(dict(['
                '("left", prefix), ("right", suffix)]))\n'
            ),
            "format_map_dict_union": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"left": prefix} | {"right": suffix}\n'
                'value = "{left}{right}".format_map(parts)\n'
            ),
            "dict_starred_static_pairs": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'pairs = [("left", prefix), ("right", suffix)]\n'
                'parts = dict([*pairs])\n'
                'value = "%(left)s%(right)s" % parts\n'
            ),
            "dict_named_concatenated_pairs": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'left_pairs = [("left", prefix)]\n'
                'right_pairs = [("right", suffix)]\n'
                "pairs = left_pairs + right_pairs\n"
                'value = "%(left)s%(right)s" % dict(pairs)\n'
            ),
            "named_mapping_subscript": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"left": prefix, "right": suffix}\n'
                'value = parts["left"] + parts["right"]\n'
            ),
            "literal_mapping_subscript": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = {"left": prefix}["left"] + '
                '{"right": suffix}["right"]\n'
            ),
            "direct_walrus_concatenation": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = (left := prefix) + (right := suffix)\n'
            ),
            "mapping_walrus_subscript": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = ((parts := {"left": prefix, "right": suffix})'
                '["left"] + parts["right"])\n'
            ),
            "nested_mapping_subscript": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"outer": {"left": prefix, "right": suffix}}\n'
                'value = parts["outer"]["left"] + '
                'parts["outer"]["right"]\n'
            ),
            "nested_mapping_percent": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"outer": {"left": prefix, "right": suffix}}\n'
                'value = "%(left)s%(right)s" % parts["outer"]\n'
            ),
            "nested_mapping_fstring": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'parts = {"outer": {"left": prefix, "right": suffix}}\n'
                'value = f"{parts[\'outer\'][\'left\']}'
                '{parts[\'outer\'][\'right\']}"\n'
            ),
            "positional_mapping_format": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'left = {"part": prefix}\n'
                'right = {"part": suffix}\n'
                'value = "{0[part]}{1[part]}".format(left, right)\n'
            ),
            "boolop_concatenation": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = (prefix or "") + (suffix or "")\n'
            ),
            "unbound_str_format": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = str.format("{}{}", prefix, suffix)\n'
            ),
            "two_level_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join('
                'part for row in [[prefix], [suffix]] for part in row)\n'
            ),
            "named_rows_two_level_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "rows = [[prefix], [suffix]]\n"
                'value = "".join('
                'part for row in rows for part in row)\n'
            ),
            "concatenated_named_rows_two_level_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "rows = [[prefix]] + [[suffix]]\n"
                'value = "".join('
                'part for row in rows for part in row)\n'
            ),
            "sliced_named_rows_two_level_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'all_rows = [[prefix], [suffix], ["unused"]]\n'
                "rows = all_rows[:2]\n"
                'value = "".join('
                'part for row in rows for part in row)\n'
            ),
            "conditional_named_rows_two_level_generator_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "rows = [[prefix], [suffix]] if True else []\n"
                'value = "".join('
                'part for row in rows for part in row)\n'
            ),
            "named_integer_multiplication": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "one = 1\n"
                "value = prefix * one + suffix\n"
            ),
            "list_augmented_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = [prefix]\n"
                "parts += [suffix]\n"
                'value = "".join(parts)\n'
            ),
            "list_self_reassignment_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = [prefix]\n"
                "parts = parts + [suffix]\n"
                'value = "".join(parts)\n'
            ),
            "sequence_multiplication_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "parts = [prefix] * 1 + [suffix] * 1\n"
                'value = "".join(parts)\n'
            ),
            "integer_subtraction_sequence_multiplication_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "one = 2 - 1\n"
                "parts = one * [prefix] + [suffix]\n"
                'value = "".join(parts)\n'
            ),
            "module_constants_in_function": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "def probe():\n"
                "    return prefix + suffix\n"
            ),
            "closure_constants": (
                "def outer():\n"
                f'    prefix = "{prefix}"\n'
                f'    suffix = "{suffix}"\n'
                "    def inner():\n"
                "        return prefix + suffix\n"
            ),
            "module_constants_in_class": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "class Probe:\n"
                "    value = prefix + suffix\n"
            ),
            "default_expression_uses_parent_constants": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "def probe(value=prefix + suffix):\n"
                "    return value\n"
            ),
            "positional_parameter_defaults": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "def probe(left=prefix, right=suffix):\n"
                "    return left + right\n"
            ),
            "keyword_only_parameter_defaults": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "def probe(*, left=prefix, right=suffix):\n"
                "    return left + right\n"
            ),
            "lambda_parameter_defaults": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "probe = lambda left=prefix, right=suffix: left + right\n"
            ),
            "mixed_outer_parameter_defaults": (
                "def outer():\n"
                f'    prefix = "{prefix}"\n'
                f'    suffix = "{suffix}"\n'
                "    async def probe(left=prefix, *, right=suffix):\n"
                "        return left + right\n"
            ),
            "single_placeholder_replace": (
                f'value = ("{prefix}" + "PLACE").replace('
                f'"PLACE", "{suffix}")\n'
            ),
            "map_str_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(map(str, (prefix, suffix)))\n'
            ),
            "filter_none_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(filter(None, (prefix, suffix)))\n'
            ),
            "reversed_join": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                'value = "".join(reversed((suffix, prefix)))\n'
            ),
            "decorator_uses_parent_constants": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "@decorate(prefix + suffix)\n"
                "def probe():\n"
                "    pass\n"
            ),
            "tuple_target_comprehension": (
                f'prefix = "{prefix}"\n'
                f'suffix = "{suffix}"\n'
                "rows = [(prefix,), (suffix,)]\n"
                'value = "".join(part for (part,) in rows)\n'
            ),
        }
        self.assertEqual(len(sources), len(set(sources.values())))
        for label, source in sources.items():
            if label != "direct":
                self.assertNotIn(protected, source)
            with self.subTest(label=label):
                with mock.patch.object(
                    verifier,
                    "FORBIDDEN_CANDIDATE_SHA256",
                    frozenset({protected_sha256}),
                ):
                    with self.assertRaisesRegex(
                        AssertionError, "known protected candidate"
                    ):
                        verifier.check_text(python_probe, source)

        excessive_alternatives = "\n".join(
            f'value = "synthetic-fragment-{index}"' for index in range(300)
        ) + "\nprobe = value\n"
        with mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
        ):
            with self.assertRaisesRegex(
                AssertionError, "static Python .* exceeded value bound"
            ):
                verifier.check_text(python_probe, excessive_alternatives)

    def test_gate_a_checker_rejects_cross_language_reconstruction_and_punctuation(self):
        import_util = __import__("importlib.util", fromlist=["util"])
        verifier_path = ROOT / "verify_gate_a.py"
        spec = import_util.spec_from_file_location(
            "gate_a_cross_language_checker_for_tests", verifier_path
        )
        verifier = import_util.module_from_spec(spec)
        spec.loader.exec_module(verifier)

        protected = "synthetic-protected-boundary-value-729"
        protected_sha256 = hashlib.sha256(
            protected.casefold().encode("utf-8")
        ).hexdigest()
        prefix = "synthetic-protected-"
        suffix = "boundary-value-729"
        ansi_hex = "".join(f"\\x{ord(char):02x}" for char in protected)
        ansi_octal = "".join(f"\\{ord(char):03o}" for char in protected)
        ansi_unicode = "".join(f"\\u{ord(char):04x}" for char in protected)
        probes = {
            "go_literal_concatenation": (
                ROOT / "internal" / "model" / "protocol.go",
                f'const value = "{prefix}" + "{suffix}"\n',
            ),
            "go_named_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "const (\n"
                    f'    left = "{prefix}"\n'
                    f'    right = "{suffix}"\n'
                    "    value = left + right\n"
                    ")\n"
                ),
            ),
            "go_multiline_constant": (
                ROOT / "internal" / "model" / "protocol.go",
                f'const value = "{prefix}" +\n    "{suffix}"\n',
            ),
            "go_parenthesized_multiline_constant": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "const value = (\n"
                    f'    "{prefix}" +\n'
                    f'    "{suffix}"\n'
                    ")\n"
                ),
            ),
            "go_multi_name_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left, right = "{prefix}", "{suffix}"\n'
                    "const value = left + right\n"
                ),
            ),
            "go_semicolon_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"; const right = "{suffix}"; '
                    "const value = left + right\n"
                ),
            ),
            "go_semicolon_constant_block": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "const ("
                    f'left = "{prefix}"; right = "{suffix}"; '
                    "value = left + right)\n"
                ),
            ),
            "go_string_conversion": (
                ROOT / "internal" / "model" / "protocol.go",
                f'const value = string("{prefix}") + "{suffix}"\n',
            ),
            "go_initialized_variable": (
                ROOT / "internal" / "model" / "protocol.go",
                f'var value = "{prefix}" + "{suffix}"\n',
            ),
            "go_short_declaration_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "func probe() string {\n"
                    f'    left := "{prefix}"\n'
                    f'    right := "{suffix}"\n'
                    "    value := left + right\n"
                    "    return value\n"
                    "}\n"
                ),
            ),
            "go_return_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "func probe() string { return left + right }\n"
                ),
            ),
            "go_call_argument_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "func probe() { sink(left + right) }\n"
                ),
            ),
            "go_same_line_short_declarations": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "func probe() { "
                    f'left := "{prefix}"; right := "{suffix}"; '
                    "sink(left + right) }\n"
                ),
            ),
            "go_ordinary_assignment_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "func probe() { var value string; "
                    "value = left + right; sink(value) }\n"
                ),
            ),
            "go_struct_literal_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "var value = Thing{Value: left + right}\n"
                ),
            ),
            "go_array_literal_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "var value = [1]string{left + right}\n"
                ),
            ),
            "go_switch_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    "func probe() { switch value { case left + right: } }\n"
                ),
            ),
            "go_if_initializer_expression": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const left = "{prefix}"\n'
                    f'const right = "{suffix}"\n'
                    'func probe() { if value := left + right; value != "" {} }\n'
                ),
            ),
            "go_named_string_type_conversion": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "type Label string\n"
                    f'const left Label = "{prefix}"\n'
                    f'var right Label = "{suffix}"\n'
                    "var value = Label(left) + Label(right)\n"
                ),
            ),
            "go_positional_struct_members": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const P = "{prefix}"; const S = "{suffix}"; '
                    "func probe() { value := struct{ A, B string }{P, S}; "
                    "sink(value.A + value.B) }\n"
                ),
            ),
            "go_array_members": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const P = "{prefix}"; const S = "{suffix}"; '
                    "func probe() { value := [2]string{P, S}; "
                    "sink(value[0] + value[1]) }\n"
                ),
            ),
            "go_map_members": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const P = "{prefix}"; const S = "{suffix}"; '
                    'func probe() { value := map[string]string{"a": P, "b": S}; '
                    'sink(value["a"] + value["b"]) }\n'
                ),
            ),
            "go_multiline_slice_members": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const P = "{prefix}"; const S = "{suffix}"\n'
                    "func probe() { value := []string{\nP,\nS,\n}; "
                    "sink(value[0] + value[1]) }\n"
                ),
            ),
            "go_string_alias_conversion": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    "type Label = string\n"
                    f'const P = "{prefix}"; const S = "{suffix}"\n'
                    "var value = Label(P) + Label(S)\n"
                ),
            ),
            "go_fmt_sprintf_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const p = "{prefix}"\n'
                    f'const s = "{suffix}"\n'
                    'var value = fmt.Sprintf("%s%s", p, s)\n'
                ),
            ),
            "go_nested_fmt_sprintf_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const p = "{prefix}"\n'
                    f'const s = "{suffix}"\n'
                    'var value = fmt.Sprintf("%s", '
                    'fmt.Sprintf("%s%s", p, s))\n'
                ),
            ),
            "go_strings_join_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const p = "{prefix}"\n'
                    f'const s = "{suffix}"\n'
                    'var value = strings.Join([]string{p, s}, "")\n'
                ),
            ),
            "go_nested_strings_join_constants": (
                ROOT / "internal" / "model" / "protocol.go",
                (
                    f'const p = "{prefix}"\n'
                    f'const s = "{suffix}"\n'
                    'var value = strings.Join([]string{'
                    'strings.Join([]string{p, s}, "")}, "")\n'
                ),
            ),
            "json_unicode_escape": (
                ROOT / "docs" / "synthetic-probe.json",
                '{"value":"synthetic-protected-\\u0062oundary-value-729"}\n',
            ),
            "template_hex_escape": (
                ROOT / "workflows" / "exercise.tmpl",
                'value: "synthetic-protected-\\x62oundary-value-729"\n',
            ),
            "shell_adjacent_literals": (
                ROOT / "workflows" / "exercise.tmpl",
                f"value='{prefix}'\"{suffix}\"\n",
            ),
            "shell_line_continuation": (
                ROOT / "workflows" / "exercise.tmpl",
                f"value='{prefix}'\\\n'{suffix}'\n",
            ),
            "shell_ansi_c_literals": (
                ROOT / "workflows" / "exercise.tmpl",
                f"value=$'{prefix}'$'{suffix}'\n",
            ),
            "shell_quoted_unquoted_literals": (
                ROOT / "workflows" / "exercise.tmpl",
                f"value='{prefix}'{suffix}\n",
            ),
            "shell_locale_quoted_literals": (
                ROOT / "workflows" / "exercise.tmpl",
                f'value=$"{prefix}"$"{suffix}"\n',
            ),
            "shell_ansi_c_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' $'{prefix}'$'{suffix}'\n",
            ),
            "shell_quoted_unquoted_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' '{prefix}'{suffix}\n",
            ),
            "shell_unquoted_backslash_escape": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' {prefix}\\{suffix}\n",
            ),
            "shell_ansi_c_hex_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' $'{ansi_hex}'\n",
            ),
            "shell_ansi_c_octal_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' $'{ansi_octal}'\n",
            ),
            "shell_ansi_c_unicode_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                f"printf '%s\\n' $'{ansi_unicode}'\n",
            ),
            "shell_variable_assignment": (
                ROOT / "workflows" / "exercise.tmpl",
                (
                    f"p='{prefix}'\n"
                    f"s='{suffix}'\n"
                    'value="$p$s"\n'
                ),
            ),
            "shell_variable_command_argument": (
                ROOT / "workflows" / "exercise.tmpl",
                (
                    f"p='{prefix}'\n"
                    f"s='{suffix}'\n"
                    'printf \'%s\\n\' "${p}${s}"\n'
                ),
            ),
        }
        for label, (path, source) in probes.items():
            self.assertNotIn(protected, source)
            with self.subTest(label=label):
                with mock.patch.object(
                    verifier,
                    "FORBIDDEN_CANDIDATE_SHA256",
                    frozenset({protected_sha256}),
                ):
                    with self.assertRaisesRegex(
                        AssertionError, "known protected candidate"
                    ):
                        verifier.check_text(path, source)

        nonmatching = {
            "go_fmt_separator": (
                ROOT / "internal" / "model" / "protocol.go",
                f'const p = "{prefix}"; const s = "{suffix}"; '
                'var value = fmt.Sprintf("%s-%s", p, s)\n',
            ),
            "go_join_separator": (
                ROOT / "internal" / "model" / "protocol.go",
                f'const p = "{prefix}"; const s = "{suffix}"; '
                'var value = strings.Join([]string{p, s}, "-")\n',
            ),
            "shell_single_quoted_variables": (
                ROOT / "workflows" / "exercise.tmpl",
                f"p='{prefix}'\ns='{suffix}'\nvalue='$p$s'\n",
            ),
            "shell_undefined_variable": (
                ROOT / "workflows" / "exercise.tmpl",
                f"s='{suffix}'\nvalue=\"${{missing}}${{s}}\"\n",
            ),
            "shell_parameter_operator": (
                ROOT / "workflows" / "exercise.tmpl",
                f"p='{prefix}'\ns='{suffix}'\nvalue=\"${{p:-x}}${{s}}\"\n",
            ),
        }
        for label, (path, source) in nonmatching.items():
            with self.subTest(nonmatching=label), mock.patch.object(
                verifier,
                "FORBIDDEN_CANDIDATE_SHA256",
                frozenset({protected_sha256}),
            ):
                verifier.check_text(path, source)

        punctuation_probe = ROOT / "docs" / "gate-a.md"
        for punctuation in (".", ":", ".,", ":" + ":", ":" + "."):
            with self.subTest(punctuation=punctuation):
                with mock.patch.object(
                    verifier,
                    "FORBIDDEN_CANDIDATE_SHA256",
                    frozenset({protected_sha256}),
                ):
                    with self.assertRaisesRegex(
                        AssertionError, "known protected candidate"
                    ):
                        verifier.check_text(
                            punctuation_probe, protected + punctuation
                        )

        for masked in (
            protected + ".mask",
            protected + ":mask",
            protected + "-mask",
            protected + "_mask",
            "mask." + protected,
            "mask:" + protected,
            "mask-" + protected,
            "mask_" + protected,
            "x" * 300 + protected + "y" * 300,
        ):
            with self.subTest(masked_token=True):
                with mock.patch.object(
                    verifier,
                    "FORBIDDEN_CANDIDATE_SHA256",
                    frozenset({protected_sha256}),
                ), mock.patch.object(
                    verifier,
                    "FORBIDDEN_CANDIDATE_LENGTHS",
                    frozenset({len(protected)}),
                ):
                    with self.assertRaisesRegex(
                        AssertionError, "known protected candidate"
                    ):
                        verifier.check_text(punctuation_probe, masked)

    def test_gate_a_candidate_scan_streams_and_enforces_work_budgets(self):
        import_util = __import__("importlib.util", fromlist=["util"])
        verifier_path = ROOT / "verify_gate_a.py"
        spec = import_util.spec_from_file_location(
            "gate_a_streaming_candidate_checker_for_tests", verifier_path
        )
        verifier = import_util.module_from_spec(spec)
        spec.loader.exec_module(verifier)
        probe = ROOT / "docs" / "gate-a.md"

        self.assertFalse(hasattr(verifier, "protected_candidate_values"))
        with mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
        ), mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_LENGTHS", frozenset({11})
        ), mock.patch.object(
            verifier, "MAX_PROTECTED_HASH_WINDOWS", 64
        ):
            with self.assertRaisesRegex(AssertionError, "scan exceeded work bound"):
                verifier.check_text(probe, "x" * 200)

        with mock.patch.object(verifier, "MAX_PROTECTED_FILE_BYTES", 128):
            with self.assertRaisesRegex(AssertionError, "file exceeded byte bound"):
                verifier.check_text(probe, "x" * 129)

        with mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
        ), mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_LENGTHS", frozenset()
        ), mock.patch.object(
            verifier, "MAX_PROTECTED_TOKENS_PER_FILE", 1
        ):
            with self.assertRaisesRegex(AssertionError, "scan exceeded token bound"):
                verifier.check_text(probe, "alpha beta")

        go_probe = ROOT / "internal" / "model" / "protocol.go"
        with mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
        ), mock.patch.object(verifier, "MAX_GO_STATIC_WINDOWS", 1):
            with self.assertRaisesRegex(
                AssertionError, "Go expression analysis exceeded work bound"
            ):
                verifier.check_text(
                    go_probe,
                    'const a = "a"\nconst b = "b"\nvar value = a + b + a\n',
                )

        with mock.patch.object(
            verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
        ), mock.patch.object(verifier, "MAX_GO_STATIC_WINDOW_TOKENS", 4):
            with self.assertRaisesRegex(
                AssertionError, "Go expression exceeded token-window bound"
            ):
                verifier.check_text(
                    go_probe,
                    'const value = "a" + "b" + "c"\n',
                )

        python_probe = ROOT / "tests" / "fault_oracle.py"
        allocation_probes = (
            'value = "x" * 100000000\n',
            'value = "%100000000s" % "x"\n',
            'value = "{:100000000}".format("x")\n',
            'value = f"{\'x\':100000000}"\n',
        )
        for source in allocation_probes:
            with self.subTest(allocation_preflight=source):
                with mock.patch.object(
                    verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
                ):
                    with self.assertRaisesRegex(
                        AssertionError, "allocation bound"
                    ):
                        verifier.check_text(python_probe, source)

        bounded_call_probes = (
            'value = "aPLACE".replace("PLACE", "0123456789")\n',
            'value = "".join(map(str, ("12345", "67890")))\n',
        )
        for source in bounded_call_probes:
            with self.subTest(call_allocation_preflight=source), mock.patch.object(
                verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
            ), mock.patch.object(verifier, "MAX_STATIC_STRING_LENGTH", 8):
                with self.assertRaisesRegex(
                    AssertionError, "(?:allocation|length) bound"
                ):
                    verifier.check_text(python_probe, source)

        bounded_go_call_probes = (
            (
                'var a = fmt.Sprintf("%s%s", "a", "b")\n'
                'var b = fmt.Sprintf("%s%s", "c", "d")\n'
            ),
            (
                'var a = fmt.Sprintf("%s%s", "a", "b")\n'
                'var b = strings.Join([]string{"c", "d"}, "")\n'
            ),
            (
                'var value = fmt.Sprintf("%s", '
                'strings.Join([]string{"a", "b"}, ""))\n'
            ),
        )
        for source in bounded_go_call_probes:
            with self.subTest(go_call_work_bound=source), mock.patch.object(
                verifier, "FORBIDDEN_CANDIDATE_SHA256", frozenset()
            ), mock.patch.object(verifier, "MAX_GO_STATIC_WINDOWS", 1):
                with self.assertRaisesRegex(
                    AssertionError, "Go expression analysis exceeded work bound"
                ):
                    verifier.check_text(go_probe, source)


if __name__ == "__main__":
    unittest.main()
