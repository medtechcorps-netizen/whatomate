"""Strict, offline evidence validation helpers for the synthetic prototype."""

from __future__ import annotations

import hashlib
import json
import re
import base64
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping


class ValidationError(ValueError):
    """Raised when evidence violates syntax, schema, or semantic policy."""


SUPPORTED_KEYS = {
    "title", "description", "type", "additionalProperties", "required",
    "properties", "items", "minItems", "maxItems", "uniqueItems",
    "pattern", "minLength", "maxLength", "minimum", "maximum", "enum",
    "const",
}
MAX_PUBLIC_JSON_BYTES = 16_384
SCHEMA_ROOT = Path(__file__).resolve().parents[1] / "schemas"
RFC3339_SECONDS = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
HASH_FIELDS = (
    "control_sha256", "runtime_source_sha256", "workflow_definition_sha256",
    "config_sha256", "image_sha256", "app_spec_sha256",
)
LIFECYCLE_EFFECTS = (
    "authority-app-create", "authority-app-update", "authority-app-delete",
    "broker-app-create", "broker-app-update", "full-redeploy", "broker-app-delete",
    "trusted-source-add", "trusted-source-delete", "binding-install",
    "binding-remove", "credential-install", "credential-remove", "leaf-issue",
    "leaf-revoke", "capability-issue", "capability-revoke", "mtls-issue",
    "mtls-revoke", "wrapping-key-revoke", "marker-cas-v2", "fork-post",
    "evidence-publish", "cleanup-delete",
)
RECOVERY_AUTHORIZATION_CLAIM_FIELDS = (
    "generation", "phase", "domain_record_sha256", "operation_body_sha256",
    "source_target_sha256", "projection_sha256", "writer_receipt_sha256",
    "terminal_receipt_sha256", "fork_request_sha256", "fork_result_sha256",
    "fork_proof_sha256", "recovery_target_sha256", "fork_provider_sha256",
    "writer_authority_sha256", "writer_broker_sha256", "writer_ledger_sha256",
    "writer_root_sha256", "writer_signing_kid",
    "writer_oracle_sha256", "observer_authority_sha256",
    "observer_broker_sha256", "observer_ledger_sha256",
    "observer_root_sha256", "observer_oracle_sha256", "boundary_sha256",
)
RECOVERY_ADMISSION_REQUEST_FIELDS = (
    "generation", "phase", "domain_record_sha256", "operation_body_sha256",
    "source_target_sha256", "projection_sha256", "writer_receipt_sha256",
    "terminal_receipt_sha256", "fork_request_sha256", "fork_result_sha256",
    "fork_proof_sha256", "recovery_target_sha256", "fork_provider_sha256",
    "boundary_sha256", "authorization_sha256", "continuity_sha256",
    "observer_authority_sha256", "observer_broker_sha256",
    "observer_ledger_sha256", "observer_root_sha256", "observer_oracle_sha256",
    "observer_signing_kid",
)
OUTER_CONTINUITY_FIELDS = (
    "generation", "phase", "domain_record_sha256", "operation_body_sha256",
    "source_target_sha256", "projection_sha256", "writer_receipt_sha256",
    "terminal_receipt_sha256", "fork_request_sha256", "fork_result_sha256",
    "fork_proof_sha256", "recovery_target_sha256", "fork_provider_sha256",
    "boundary_sha256", "authorization_claims_sha256", "authorization_sha256",
    "authorization_record_sha256", "admission_request_sha256",
    "admission_claims_sha256", "admission_sha256", "admission_record_sha256",
    "continuity_sha256", "observer_receipt_sha256", "marker_sha256",
    "fixed_key_sha256", "writer_ledger_sha256", "writer_root_sha256",
    "writer_oracle_sha256", "writer_signature_sha256",
    "writer_authorization_publication_record_sha256",
    "writer_authorization_publication_completed_at", "observer_ledger_sha256",
    "observer_root_sha256", "observer_oracle_sha256",
    "observer_signature_sha256", "observer_admission_publication_record_sha256",
    "observer_admission_publication_completed_at",
    "observer_evidence_publication_record_sha256",
    "observer_evidence_publication_completed_at",
    "observer_admission_lifecycle_sha256",
    "observer_cleanup_lifecycle_sha256",
)

DOMAIN_SEPARATION_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind", "writer",
    "observer",
))
DOMAIN_ROLE_FIELDS = frozenset((
    "semantic_label", "authority_app_sha256", "broker_app_sha256",
    "ledger_sha256", "root_key_sha256", "signing_kid", "signing_public_key",
))
WRITER_RECEIPT_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "generation", "phase", "domain_record_sha256",
    "source_target_sha256", "recovery_target_sha256", "operation_sha256",
    "authorization_sha256", "control_sha256", "runtime_source_sha256",
    "workflow_definition_sha256", "config_sha256", "image_sha256",
    "app_spec_sha256", "authority_app_sha256", "broker_app_sha256",
    "ledger_sha256", "root_key_sha256", "signing_kid", "fixed_key_sha256",
    "marker_sha256", "command", "write_issue_count", "readback_count",
    "reconciliation", "issued_at", "expires_at", "signature",
    "signature_sha256",
))
TERMINAL_LIFECYCLE_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind", "generation",
    "phase", "domain_record_sha256", "writer_receipt_sha256",
    "source_target_sha256", "recovery_target_sha256", "side_effect",
    "operation_sha256", "authorization_sha256", "control_sha256",
    "runtime_source_sha256", "workflow_definition_sha256", "config_sha256",
    "image_sha256", "app_spec_sha256", "controller_sha256",
    "root_key_sha256", "signing_kid", "before_inventory_sha256",
    "after_inventory_sha256", "issue_count", "reconciliation",
    "terminal_result", "provider_fact_source", "provider_read_one_at",
    "provider_read_two_at", "minimum_provider_read_separation_seconds",
    "terminal_evidence_sha256", "writer_terminal_proof", "issued_at",
    "expires_at", "signature", "signature_sha256",
))
WRITER_TERMINAL_PROOF_FIELDS = frozenset((
    "broker_deleted", "direct_get_absent", "app_inventory_pagination_complete",
    "app_inventory_count", "app_inventory_sha256",
    "deployment_inventory_pagination_complete", "deployment_inventory_count",
    "deployment_inventory_sha256",
    "provider_operation_inventory_pagination_complete",
    "provider_operation_inventory_count", "nonterminal_provider_operation_count",
    "provider_operation_inventory_sha256", "nonterminal_deployment_count",
    "rollback_capable_deployment_count", "delete_action_terminal",
    "delete_action_sha256", "full_redeploy_complete",
    "old_instance_grace_elapsed", "leaf_revoked", "capability_revoked",
    "mtls_revoked", "wrapping_key_revoked", "binding_absent",
    "credential_absent", "firewall_restored", "original_firewall_sha256",
    "firewall_read_one_sha256", "firewall_read_two_sha256",
    "original_projection_sha256", "projection_read_one_sha256",
    "projection_read_two_sha256", "action_ledger_read_one_sha256",
    "action_ledger_read_two_sha256",
))
AUTHORIZATION_RECORD_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "generation", "phase", "domain_record_sha256",
    "operation_body_sha256", "source_target_sha256", "projection_sha256",
    "writer_receipt_sha256", "terminal_receipt_sha256", "fork_request_sha256",
    "fork_result_sha256", "fork_proof_sha256", "recovery_target_sha256",
    "fork_provider_sha256", "writer_authority_sha256", "writer_broker_sha256",
    "writer_ledger_sha256", "writer_root_sha256", "writer_signing_kid",
    "writer_oracle_sha256", "observer_authority_sha256",
    "observer_broker_sha256", "observer_ledger_sha256", "observer_root_sha256",
    "observer_oracle_sha256", "boundary_sha256", "claims_sha256",
    "publication_request_sha256", "writer_signature", "writer_signature_sha256",
    "publication_result_sha256", "publication_record_sha256",
    "publication_completed_at", "authorization_sha256", "issued_at",
    "expires_at",
))
ADMISSION_RECORD_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "generation", "phase", "domain_record_sha256",
    "operation_body_sha256", "source_target_sha256", "projection_sha256",
    "writer_receipt_sha256", "terminal_receipt_sha256", "fork_request_sha256",
    "fork_result_sha256", "fork_proof_sha256", "recovery_target_sha256",
    "fork_provider_sha256", "boundary_sha256", "authorization_sha256",
    "continuity_sha256", "observer_authority_sha256", "observer_broker_sha256",
    "observer_ledger_sha256", "observer_root_sha256", "observer_oracle_sha256",
    "observer_signing_kid", "admission_request_sha256",
    "admission_claims_sha256", "publication_request_sha256",
    "observer_signature", "observer_signature_sha256",
    "publication_result_sha256", "publication_record_sha256",
    "publication_completed_at", "admission_sha256", "issued_at", "expires_at",
))
OBSERVER_RECEIPT_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "generation", "phase", "domain_record_sha256",
    "source_target_sha256", "recovery_target_sha256", "operation_sha256",
    "authorization_sha256", "admission_sha256", "control_sha256",
    "runtime_source_sha256", "workflow_definition_sha256", "config_sha256",
    "image_sha256", "app_spec_sha256", "authority_app_sha256",
    "broker_app_sha256", "ledger_sha256", "root_key_sha256", "signing_kid",
    "fixed_key_sha256", "marker_sha256", "read_one_marker_sha256",
    "read_two_marker_sha256", "source_read_count", "recovery_read_count",
    "read_sequence", "recovery_read_one_at", "recovery_read_two_at",
    "minimum_read_separation_seconds", "writer_receipt_seen",
    "expected_marker_hash_seen", "raw_marker_returned", "evidence_sha256",
    "publication_record_sha256", "publication_completed_at", "issued_at",
    "expires_at", "signature", "signature_sha256",
))
OUTER_RECORD_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    *OUTER_CONTINUITY_FIELDS, "outer_continuity_sha256",
))

OBSERVER_LIFECYCLE_REQUEST_FIELDS = (
    "authority_domain", "lifecycle_kind", "generation", "phase",
    "domain_record_sha256", "operation_body_sha256", "source_target_sha256",
    "recovery_target_sha256",
    "authorization_sha256", "admission_sha256", "admission_record_sha256",
    "authority_app_sha256",
    "broker_app_sha256", "ledger_sha256", "root_key_sha256", "signing_kid",
    "bound_publication_record_sha256", "requested_at",
)
OBSERVER_CLEANUP_LIFECYCLE_REQUEST_FIELDS = (
    *OBSERVER_LIFECYCLE_REQUEST_FIELDS,
    "observer_receipt_sha256", "observer_evidence_sha256",
)
OBSERVER_ADMISSION_LIFECYCLE_RESULT_FIELDS = (
    "lifecycle_request_sha256", "issue_count", "state", "source_trust_present",
    "writer_material_present", "recovery_trust_present",
    "observer_binding_present", "observer_credential_present",
    "observer_leaf_present", "observer_capability_present",
    "observer_mtls_present", "completed_at",
)
OBSERVER_CLEANUP_LIFECYCLE_RESULT_FIELDS = (
    "lifecycle_request_sha256", "issue_count", "state", "source_trust_present",
    "writer_material_present", "recovery_trust_present",
    "observer_binding_present", "observer_credential_present",
    "observer_leaf_present", "observer_capability_present",
    "observer_mtls_present", "direct_get_absent", "delete_action_terminal",
    "old_instance_grace_elapsed", "app_inventory_pagination_complete",
    "app_inventory_count", "app_inventory_sha256",
    "deployment_inventory_pagination_complete", "deployment_inventory_count",
    "deployment_inventory_sha256",
    "provider_operation_inventory_pagination_complete",
    "nonterminal_provider_operation_count", "provider_operation_inventory_sha256",
    "completed_at",
)
OBSERVER_ADMISSION_LIFECYCLE_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "lifecycle_kind", "generation", "phase",
    "domain_record_sha256", "operation_body_sha256", "source_target_sha256",
    "recovery_target_sha256",
    "authorization_sha256", "admission_sha256", "admission_record_sha256",
    "authority_app_sha256",
    "broker_app_sha256", "ledger_sha256", "root_key_sha256", "signing_kid",
    "bound_publication_record_sha256", "lifecycle_request_sha256",
    "lifecycle_result_sha256", "issue_count", "state", "source_trust_present",
    "writer_material_present", "recovery_trust_present",
    "observer_binding_present", "observer_credential_present",
    "observer_leaf_present", "observer_capability_present",
    "observer_mtls_present", "requested_at", "completed_at", "issued_at",
    "expires_at", "signature", "signature_sha256",
))
OBSERVER_CLEANUP_LIFECYCLE_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "lifecycle_kind", "generation", "phase",
    "domain_record_sha256", "operation_body_sha256", "source_target_sha256",
    "recovery_target_sha256",
    "authorization_sha256", "admission_sha256", "admission_record_sha256",
    "observer_receipt_sha256", "observer_evidence_sha256", "authority_app_sha256",
    "broker_app_sha256", "ledger_sha256", "root_key_sha256", "signing_kid",
    "bound_publication_record_sha256", "lifecycle_request_sha256",
    "lifecycle_result_sha256", "issue_count", "state", "source_trust_present",
    "writer_material_present", "recovery_trust_present",
    "observer_binding_present", "observer_credential_present",
    "observer_leaf_present", "observer_capability_present",
    "observer_mtls_present", "direct_get_absent", "delete_action_terminal",
    "old_instance_grace_elapsed", "app_inventory_pagination_complete",
    "app_inventory_count", "app_inventory_sha256",
    "deployment_inventory_pagination_complete", "deployment_inventory_count",
    "deployment_inventory_sha256",
    "provider_operation_inventory_pagination_complete",
    "nonterminal_provider_operation_count", "provider_operation_inventory_sha256",
    "requested_at", "completed_at", "issued_at", "expires_at", "signature",
    "signature_sha256",
))

OBSERVER_EVIDENCE_FIELDS = (
    "generation", "phase", "domain_record_sha256", "source_target_sha256",
    "recovery_target_sha256", "operation_sha256", "authorization_sha256",
    "admission_sha256", "control_sha256", "runtime_source_sha256",
    "workflow_definition_sha256", "config_sha256", "image_sha256",
    "app_spec_sha256", "authority_app_sha256", "broker_app_sha256",
    "ledger_sha256", "root_key_sha256", "signing_kid", "fixed_key_sha256",
    "marker_sha256", "read_one_marker_sha256", "read_two_marker_sha256",
    "source_read_count", "recovery_read_count", "read_sequence",
    "recovery_read_one_at", "recovery_read_two_at",
    "minimum_read_separation_seconds", "writer_receipt_seen",
    "expected_marker_hash_seen", "raw_marker_returned",
)

LEDGER_PUBLICATION_REQUEST_FIELDS = (
    "authority_domain", "publication_kind", "generation", "phase",
    "domain_record_sha256", "operation_body_sha256", "ledger_sha256",
    "root_key_sha256", "signing_kid", "published_object_sha256",
    "requested_at",
)
LEDGER_PUBLICATION_RESULT_FIELDS = (
    "publication_request_sha256", "published_object_sha256", "issue_count",
    "state", "completed_at",
)
LEDGER_PUBLICATION_FIELDS = frozenset((
    "schema_version", "claim_scope", "non_authoritative", "kind",
    "authority_domain", "publication_kind", "generation", "phase",
    "domain_record_sha256", "operation_body_sha256", "ledger_sha256",
    "root_key_sha256", "signing_kid", "published_object_sha256",
    "publication_request_sha256", "publication_result_sha256", "issue_count",
    "state", "requested_at", "completed_at", "expires_at", "signature",
    "signature_sha256",
))

SIGNED_RECORD_DOMAINS = {
    "writer-marker-receipt": "writer-marker-receipt/v2",
    "lifecycle-side-effect-receipt": "writer-terminal-lifecycle/v2",
    "recovery-admission-authorization": "recovery-admission-authorization/v2",
    "recovery-admission": "recovery-admission/v2",
    "observer-recovery-receipt": "observer-recovery-receipt/v2",
    "observer-admission-lifecycle": "observer-admission-lifecycle/v2",
    "observer-cleanup-lifecycle": "observer-cleanup-lifecycle/v2",
}
LEDGER_PUBLICATION_DOMAINS = {
    "writer-authorization": "ledger-publication/writer-authorization/v2",
    "observer-admission": "ledger-publication/observer-admission/v2",
    "observer-evidence": "ledger-publication/observer-evidence/v2",
}
TRUSTED_SYNTHETIC_SIGNERS = {
    "writer": {
        "kid": "synthetic-writer-56475aa75463474c",
        "public_key": "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg",
    },
    "observer": {
        "kid": "synthetic-observer-24f6ed6acbfe1009",
        "public_key": "Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc",
    },
}


def canonical_bytes(value: Any) -> bytes:
    """Return the sole accepted ASCII public-evidence JSON encoding."""

    _reject_non_ascii(value)
    return json.dumps(
        value, ensure_ascii=True, allow_nan=False, sort_keys=True, separators=(",", ":")
    ).encode("ascii")


def canonical_record_sha256(value: Mapping[str, Any]) -> str:
    """Digest the complete closed record, including its real signature."""

    return hashlib.sha256(canonical_bytes(value)).hexdigest()


def base64url_encode(value: bytes) -> str:
    """Return canonical unpadded base64url for synthetic public evidence."""

    return base64.urlsafe_b64encode(value).decode("ascii").rstrip("=")


def domain_separated_message(domain: str, payload: bytes) -> bytes:
    """Frame a canonical payload exactly as the Go cross-language KAT does."""

    domain_bytes = domain.encode("ascii", errors="strict")
    if not domain_bytes or len(domain_bytes) > 0xFFFF:
        raise ValidationError("signature domain length is invalid")
    return (
        b"rereply-recovery-boundary/signature/v1\x00"
        + len(domain_bytes).to_bytes(2, "big") + domain_bytes
        + len(payload).to_bytes(8, "big") + payload
    )


def signed_record_payload(value: Mapping[str, Any], signature_field: str) -> bytes:
    """Return the exact non-circular canonical payload for a signed record."""

    digest_field = (
        "writer_signature_sha256" if signature_field == "writer_signature"
        else "observer_signature_sha256" if signature_field == "observer_signature"
        else "signature_sha256"
    )
    excluded = {signature_field, digest_field}
    # The semantic authorization/admission digest remains the sole circular
    # field. Publication completion records are independent signed records,
    # so their exact request/result/digest can be covered here.
    if value.get("kind") == "recovery-admission-authorization":
        excluded.add("authorization_sha256")
    elif value.get("kind") == "recovery-admission":
        excluded.add("admission_sha256")
    return canonical_bytes({key: child for key, child in value.items() if key not in excluded})


def _reject_non_ascii(value: Any, path: str = "$") -> None:
    if isinstance(value, str):
        if any(ord(char) < 0x20 or ord(char) > 0x7E for char in value):
            raise ValidationError(f"{path}: public JSON must be printable ASCII")
    elif isinstance(value, dict):
        for key, child in value.items():
            _reject_non_ascii(key, f"{path}.<key>")
            _reject_non_ascii(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _reject_non_ascii(child, f"{path}[{index}]")


def strict_json_loads(
    raw: bytes | str,
    *,
    require_canonical: bool = False,
    max_bytes: int = MAX_PUBLIC_JSON_BYTES,
) -> Any:
    """Parse bounded JSON, rejecting duplicates, floats, constants, and Unicode."""

    data = raw.encode("utf-8") if isinstance(raw, str) else raw
    if len(data) > max_bytes:
        raise ValidationError("$: public JSON exceeds size limit")
    try:
        text = data.decode("utf-8", errors="strict")
    except UnicodeDecodeError as exc:
        raise ValidationError("$: public JSON is not UTF-8") from exc

    def pairs_hook(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise ValidationError(f"$: duplicate object key {key!r}")
            result[key] = value
        return result

    def reject_float(value: str) -> Any:
        raise ValidationError(f"$: floating-point number forbidden: {value}")

    def reject_constant(value: str) -> Any:
        raise ValidationError(f"$: non-finite number forbidden: {value}")

    try:
        value = json.loads(
            text,
            object_pairs_hook=pairs_hook,
            parse_float=reject_float,
            parse_constant=reject_constant,
        )
    except ValidationError:
        raise
    except (json.JSONDecodeError, UnicodeError) as exc:
        raise ValidationError("$: malformed JSON") from exc
    _reject_non_ascii(value)
    if require_canonical and data != canonical_bytes(value):
        raise ValidationError("$: noncanonical JSON encoding")
    return value


def assert_schema_is_closed(schema: Mapping[str, Any], path: str = "$") -> None:
    unknown = set(schema) - SUPPORTED_KEYS
    if unknown:
        raise AssertionError(f"{path}: unsupported schema keys: {sorted(unknown)}")
    schema_type = schema.get("type")
    if schema_type == "object":
        if schema.get("additionalProperties") is not False:
            raise AssertionError(f"{path}: object is not closed")
        properties = schema.get("properties")
        required = schema.get("required")
        if not isinstance(properties, dict) or not isinstance(required, list):
            raise AssertionError(f"{path}: object properties/required missing")
        if set(properties) != set(required) or len(required) != len(set(required)):
            raise AssertionError(f"{path}: every property must be required exactly once")
        for name, child in properties.items():
            if not isinstance(child, dict):
                raise AssertionError(f"{path}.{name}: property schema is not an object")
            assert_schema_is_closed(child, f"{path}.{name}")
    elif schema_type == "array":
        items = schema.get("items")
        if not isinstance(items, dict):
            raise AssertionError(f"{path}: array items schema missing")
        if schema.get("minItems") != schema.get("maxItems"):
            raise AssertionError(f"{path}: public arrays must have exact cardinality")
        assert_schema_is_closed(items, f"{path}[]")


def _check_type(value: Any, expected: str, path: str) -> None:
    valid = {
        "object": isinstance(value, dict), "array": isinstance(value, list),
        "string": isinstance(value, str),
        "integer": isinstance(value, int) and not isinstance(value, bool),
        "boolean": isinstance(value, bool),
    }.get(expected)
    if valid is None:
        raise AssertionError(f"{path}: unsupported type in schema: {expected!r}")
    if not valid:
        raise ValidationError(f"{path}: expected {expected}")


def validate(value: Any, schema: Mapping[str, Any], path: str = "$") -> None:
    """Validate an instance against the deliberately small strict subset."""

    expected_type = schema.get("type")
    if expected_type is not None:
        _check_type(value, expected_type, path)
    if "const" in schema and value != schema["const"]:
        raise ValidationError(f"{path}: const mismatch")
    if "enum" in schema and value not in schema["enum"]:
        raise ValidationError(f"{path}: enum mismatch")
    if expected_type == "object":
        properties = schema["properties"]
        missing = [name for name in schema["required"] if name not in value]
        if missing:
            raise ValidationError(f"{path}: missing properties {missing}")
        extra = set(value) - set(properties)
        if schema.get("additionalProperties") is False and extra:
            raise ValidationError(f"{path}: unexpected properties {sorted(extra)}")
        for name, child_schema in properties.items():
            if name in value:
                validate(value[name], child_schema, f"{path}.{name}")
    if expected_type == "array":
        if len(value) < schema.get("minItems", 0):
            raise ValidationError(f"{path}: too few items")
        if "maxItems" in schema and len(value) > schema["maxItems"]:
            raise ValidationError(f"{path}: too many items")
        if schema.get("uniqueItems"):
            encoded = [canonical_bytes(item) for item in value]
            if len(encoded) != len(set(encoded)):
                raise ValidationError(f"{path}: duplicate items")
        for index, item in enumerate(value):
            validate(item, schema["items"], f"{path}[{index}]")
    if expected_type == "string":
        if len(value) < schema.get("minLength", 0):
            raise ValidationError(f"{path}: string too short")
        if "maxLength" in schema and len(value) > schema["maxLength"]:
            raise ValidationError(f"{path}: string too long")
        if "pattern" in schema and re.fullmatch(schema["pattern"], value) is None:
            raise ValidationError(f"{path}: pattern mismatch")
    if expected_type == "integer":
        if value < schema.get("minimum", value):
            raise ValidationError(f"{path}: below minimum")
        if value > schema.get("maximum", value):
            raise ValidationError(f"{path}: above maximum")


def _timestamp(value: str, path: str) -> datetime:
    if not RFC3339_SECONDS.fullmatch(value):
        raise ValidationError(f"{path}: timestamp must be whole-second UTC RFC3339")
    try:
        return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except ValueError as exc:
        raise ValidationError(f"{path}: invalid timestamp") from exc


def _validate_receipt_common(value: Mapping[str, Any]) -> tuple[datetime, datetime]:
    if value["operation_sha256"] == value["authorization_sha256"]:
        raise ValidationError("$: authorization digest must differ from operation digest")
    for field in HASH_FIELDS:
        if value[field] == "0" * 64:
            raise ValidationError(f"$.{field}: zero digest forbidden")
    issued = _timestamp(value["issued_at"], "$.issued_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    lifetime = (expires - issued).total_seconds()
    if lifetime <= 0 or lifetime > 300:
        raise ValidationError("$: receipt lifetime must be in (0,300] seconds")
    return issued, expires


def _validate_publication_times(value: Mapping[str, Any]) -> None:
    issued = _timestamp(value["issued_at"], "$.issued_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    lifetime = (expires - issued).total_seconds()
    if lifetime <= 0 or lifetime > 300:
        raise ValidationError("$: publication lifetime must be in (0,300] seconds")


def _digest_fields(value: Mapping[str, Any], fields: tuple[str, ...]) -> str:
    return hashlib.sha256(canonical_bytes({field: value[field] for field in fields})).hexdigest()


def _signature_digest(value: str, path: str) -> str:
    try:
        raw = base64.urlsafe_b64decode(value + "==")
    except (ValueError, TypeError) as exc:
        raise ValidationError(f"{path}: malformed base64url signature") from exc
    if len(raw) != 64 or base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=") != value:
        raise ValidationError(f"{path}: noncanonical Ed25519 signature encoding")
    return hashlib.sha256(raw).hexdigest()


def _decode_public_key(value: str, path: str) -> bytes:
    try:
        raw = base64.urlsafe_b64decode(value + "=")
    except (ValueError, TypeError) as exc:
        raise ValidationError(f"{path}: malformed base64url public key") from exc
    if len(raw) != 32 or base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=") != value:
        raise ValidationError(f"{path}: noncanonical Ed25519 public key encoding")
    return raw


def _signature_bytes(value: str, path: str) -> bytes:
    _signature_digest(value, path)
    return base64.urlsafe_b64decode(value + "==")


def _validate_signature_digest(
    value: Mapping[str, Any], signature_field: str, digest_field: str
) -> None:
    if value[digest_field] != _signature_digest(value[signature_field], f"$.{signature_field}"):
        raise ValidationError(f"$: {signature_field} digest mismatch")


def _verify_trusted_record_signature(
    value: Mapping[str, Any],
    *,
    role: str,
    public_key: bytes,
    expected_kid: str,
    signature_field: str,
    kid_field: str,
) -> None:
    kind = value["kind"]
    domain = (
        LEDGER_PUBLICATION_DOMAINS.get(value.get("publication_kind"))
        if kind == "ledger-publication-completion"
        else SIGNED_RECORD_DOMAINS.get(kind)
    )
    if domain is None:
        raise ValidationError("$: signed record kind has no domain separation")
    if value[kid_field] != expected_kid:
        raise ValidationError(f"$: {role} signing KID mismatch")
    signature = _signature_bytes(value[signature_field], f"$.{signature_field}")
    message = domain_separated_message(domain, signed_record_payload(value, signature_field))
    if not ed25519_verify(public_key, message, signature):
        raise ValidationError(f"$: invalid {role} Ed25519 signature")


def _fork_completion_digest(value: Mapping[str, Any]) -> str:
    payload = (
        b"fork-completion/v2\x00"
        + value["terminal_receipt_sha256"].encode("ascii")
        + b"\x00" + value["fork_request_sha256"].encode("ascii")
        + b"\x00" + value["fork_result_sha256"].encode("ascii")
    )
    return hashlib.sha256(payload).hexdigest()


def _validate_fork_binding(value: Mapping[str, Any]) -> None:
    if value["fork_proof_sha256"] != _fork_completion_digest(value):
        raise ValidationError("$: fork proof does not bind terminal/request/result")


def _require_exact_fields(
    value: Mapping[str, Any], expected: frozenset[str], label: str
) -> None:
    if not isinstance(value, dict):
        raise ValidationError(f"$: {label} must be an exact JSON object")
    actual = set(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unexpected = sorted(actual - expected)
        raise ValidationError(
            f"$: {label} fields mismatch; missing={missing}, unexpected={unexpected}"
        )


def _require_record_identity(
    value: Mapping[str, Any], kind: str, authority_domain: str | None = None
) -> None:
    if (
        type(value["schema_version"]) is not int
        or value["schema_version"] != 2
        or value["claim_scope"] != "gate-c-nonproduction-only"
        or value["non_authoritative"] is not True
        or value["kind"] != kind
    ):
        raise ValidationError(f"$: {kind} identity/constants mismatch")
    if authority_domain is not None and value["authority_domain"] != authority_domain:
        raise ValidationError(f"$: {kind} authority domain mismatch")


def _validate_ledger_publication(value: Mapping[str, Any]) -> None:
    _require_exact_fields(value, LEDGER_PUBLICATION_FIELDS, "ledger publication")
    if (
        type(value["schema_version"]) is not int
        or value["schema_version"] != 2
        or value["claim_scope"] != "gate-c-nonproduction-only"
        or value["non_authoritative"] is not True
        or value["kind"] != "ledger-publication-completion"
    ):
        raise ValidationError("$: ledger publication identity/constants mismatch")
    if type(value["generation"]) is not int or not 1 <= value["generation"] <= 4:
        raise ValidationError("$: ledger publication generation is invalid")
    if value["phase"] not in {"baseline", "bridge", "backend", "ui"}:
        raise ValidationError("$: ledger publication phase is invalid")
    if type(value["issue_count"]) is not int or value["issue_count"] != 1:
        raise ValidationError("$: ledger publication must contain exactly one issue")
    if value["state"] != "completed":
        raise ValidationError("$: ledger publication is not completed")
    for field in ("requested_at", "completed_at", "expires_at"):
        if not isinstance(value[field], str):
            raise ValidationError(f"$.{field}: ledger publication timestamp is malformed")
    for field in (
        "domain_record_sha256", "operation_body_sha256", "ledger_sha256",
        "root_key_sha256", "published_object_sha256",
        "publication_request_sha256", "publication_result_sha256",
        "signature_sha256",
    ):
        if not isinstance(value[field], str) or re.fullmatch(r"[0-9a-f]{64}", value[field]) is None:
            raise ValidationError(f"$.{field}: ledger publication digest is malformed")
        if value[field] == "0" * 64:
            raise ValidationError(f"$.{field}: zero digest forbidden")
    if (
        not isinstance(value["signing_kid"], str)
        or re.fullmatch(r"synthetic-(writer|observer)-[0-9a-f]{16}", value["signing_kid"])
        is None
    ):
        raise ValidationError("$.signing_kid: ledger publication KID is malformed")
    expected_domain = {
        "writer-authorization": "writer",
        "observer-admission": "observer",
        "observer-evidence": "observer",
    }.get(value["publication_kind"])
    if value["authority_domain"] != expected_domain:
        raise ValidationError("$: publication kind/authority domain mismatch")
    requested = _timestamp(value["requested_at"], "$.requested_at")
    completed = _timestamp(value["completed_at"], "$.completed_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    if not requested < completed < expires:
        raise ValidationError("$: publication chronology must be strictly ordered")
    if (expires - requested).total_seconds() > 300:
        raise ValidationError("$: publication lifetime exceeds 300 seconds")
    if value["publication_request_sha256"] != _digest_fields(
        value, LEDGER_PUBLICATION_REQUEST_FIELDS
    ):
        raise ValidationError("$: ledger publication request digest mismatch")
    if value["publication_result_sha256"] != _digest_fields(
        value, LEDGER_PUBLICATION_RESULT_FIELDS
    ):
        raise ValidationError("$: ledger publication result digest mismatch")
    _validate_signature_digest(value, "signature", "signature_sha256")


def _validate_observer_lifecycle(value: Mapping[str, Any]) -> tuple[datetime, datetime]:
    kind = value.get("kind")
    if kind == "observer-admission-lifecycle":
        expected_fields = OBSERVER_ADMISSION_LIFECYCLE_FIELDS
        lifecycle_kind = "observer-admission"
        result_fields = OBSERVER_ADMISSION_LIFECYCLE_RESULT_FIELDS
        request_fields = OBSERVER_LIFECYCLE_REQUEST_FIELDS
        expected_presence = True
    elif kind == "observer-cleanup-lifecycle":
        expected_fields = OBSERVER_CLEANUP_LIFECYCLE_FIELDS
        lifecycle_kind = "observer-cleanup"
        result_fields = OBSERVER_CLEANUP_LIFECYCLE_RESULT_FIELDS
        request_fields = OBSERVER_CLEANUP_LIFECYCLE_REQUEST_FIELDS
        expected_presence = False
    else:
        raise ValidationError("$: unsupported observer lifecycle kind")
    _require_exact_fields(value, expected_fields, lifecycle_kind)
    _require_record_identity(value, kind, "observer")
    if value["lifecycle_kind"] != lifecycle_kind:
        raise ValidationError("$: observer lifecycle semantic kind mismatch")
    if type(value["generation"]) is not int or not 1 <= value["generation"] <= 4:
        raise ValidationError("$: observer lifecycle generation is invalid")
    if value["phase"] not in {"baseline", "bridge", "backend", "ui"}:
        raise ValidationError("$: observer lifecycle phase is invalid")
    if type(value["issue_count"]) is not int or value["issue_count"] != 1:
        raise ValidationError("$: observer lifecycle must contain exactly one issue")
    if value["state"] != "completed":
        raise ValidationError("$: observer lifecycle is not completed")
    if value["source_trust_present"] is not False:
        raise ValidationError("$: observer lifecycle must never contain source trust")
    if value["writer_material_present"] is not False:
        raise ValidationError("$: observer lifecycle must never contain writer material")
    for field in (
        "recovery_trust_present", "observer_binding_present",
        "observer_credential_present", "observer_leaf_present",
        "observer_capability_present", "observer_mtls_present",
    ):
        if value[field] is not expected_presence:
            raise ValidationError(f"$.{field}: observer lifecycle state mismatch")
    if value["lifecycle_request_sha256"] != _digest_fields(
        value, request_fields
    ):
        raise ValidationError("$: observer lifecycle request digest mismatch")
    if value["lifecycle_result_sha256"] != _digest_fields(value, result_fields):
        raise ValidationError("$: observer lifecycle result digest mismatch")
    requested = _timestamp(value["requested_at"], "$.requested_at")
    completed = _timestamp(value["completed_at"], "$.completed_at")
    issued = _timestamp(value["issued_at"], "$.issued_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    if not requested < completed <= issued < expires:
        raise ValidationError("$: observer lifecycle chronology is invalid")
    if (expires - issued).total_seconds() > 300:
        raise ValidationError("$: observer lifecycle lifetime exceeds 300 seconds")
    _validate_signature_digest(value, "signature", "signature_sha256")
    if kind == "observer-cleanup-lifecycle":
        for field in (
            "direct_get_absent", "delete_action_terminal",
            "old_instance_grace_elapsed", "app_inventory_pagination_complete",
            "deployment_inventory_pagination_complete",
            "provider_operation_inventory_pagination_complete",
        ):
            if value[field] is not True:
                raise ValidationError(f"$.{field}: observer cleanup proof is incomplete")
        for field in (
            "app_inventory_count", "deployment_inventory_count",
            "nonterminal_provider_operation_count",
        ):
            if value[field] != 0:
                raise ValidationError(f"$.{field}: observer cleanup inventory is nonempty")
        empty_inventory = hashlib.sha256(canonical_bytes([])).hexdigest()
        if value["app_inventory_sha256"] != empty_inventory:
            raise ValidationError("$: observer cleanup app inventory digest mismatch")
        if value["deployment_inventory_sha256"] != empty_inventory:
            raise ValidationError("$: observer cleanup deployment inventory digest mismatch")
    return issued, expires


def _validate_recovery_admission_authorization(value: Mapping[str, Any]) -> None:
    _validate_publication_times(value)
    _validate_fork_binding(value)
    separated = (
        value["writer_authority_sha256"], value["writer_broker_sha256"],
        value["writer_ledger_sha256"], value["writer_root_sha256"],
        value["writer_oracle_sha256"], value["observer_authority_sha256"],
        value["observer_broker_sha256"], value["observer_ledger_sha256"],
        value["observer_root_sha256"], value["observer_oracle_sha256"],
    )
    if len(set(separated)) != len(separated):
        raise ValidationError("$: writer/observer app, broker, ledger, root, and store identities must differ")
    if value["claims_sha256"] != _digest_fields(value, RECOVERY_AUTHORIZATION_CLAIM_FIELDS):
        raise ValidationError("$: authorization claims digest mismatch")
    _validate_signature_digest(value, "writer_signature", "writer_signature_sha256")
    completed = _timestamp(value["publication_completed_at"], "$.publication_completed_at")
    issued = _timestamp(value["issued_at"], "$.issued_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    if not completed <= issued < expires:
        raise ValidationError("$: writer publication completion chronology mismatch")
    expected_authorization = _digest_fields(value, (
        "claims_sha256", "publication_request_sha256", "publication_result_sha256",
        "publication_record_sha256", "publication_completed_at",
        "writer_signature_sha256",
    ))
    if value["authorization_sha256"] != expected_authorization:
        raise ValidationError("$: authorization digest mismatch")


def _validate_recovery_admission(value: Mapping[str, Any]) -> None:
    _validate_publication_times(value)
    _validate_fork_binding(value)
    expected_continuity = _digest_fields(value, (
        "authorization_sha256", "operation_body_sha256", "fork_proof_sha256",
        "recovery_target_sha256",
    ))
    if value["continuity_sha256"] != expected_continuity:
        raise ValidationError("$: observer continuity digest mismatch")
    if value["admission_request_sha256"] != _digest_fields(value, RECOVERY_ADMISSION_REQUEST_FIELDS):
        raise ValidationError("$: observer admission request digest mismatch")
    expected_claims = _digest_fields(value, (
        "admission_request_sha256", "authorization_sha256",
        "continuity_sha256", "observer_oracle_sha256",
    ))
    if value["admission_claims_sha256"] != expected_claims:
        raise ValidationError("$: observer admission claims digest mismatch")
    _validate_signature_digest(value, "observer_signature", "observer_signature_sha256")
    completed = _timestamp(value["publication_completed_at"], "$.publication_completed_at")
    issued = _timestamp(value["issued_at"], "$.issued_at")
    expires = _timestamp(value["expires_at"], "$.expires_at")
    if not completed <= issued < expires:
        raise ValidationError("$: observer admission publication chronology mismatch")
    expected_admission = _digest_fields(value, (
        "admission_claims_sha256", "publication_request_sha256",
        "publication_result_sha256", "observer_signature_sha256",
        "publication_record_sha256", "publication_completed_at",
        "authorization_sha256", "continuity_sha256",
    ))
    if value["admission_sha256"] != expected_admission:
        raise ValidationError("$: admission digest mismatch")


def _validate_outer_recovery_continuity(value: Mapping[str, Any]) -> None:
    _validate_fork_binding(value)
    expected_continuity = _digest_fields(value, (
        "authorization_sha256", "operation_body_sha256", "fork_proof_sha256",
        "recovery_target_sha256",
    ))
    if value["continuity_sha256"] != expected_continuity:
        raise ValidationError("$: outer observer continuity digest mismatch")
    for writer, observer in (
        ("writer_ledger_sha256", "observer_ledger_sha256"),
        ("writer_root_sha256", "observer_root_sha256"),
        ("writer_oracle_sha256", "observer_oracle_sha256"),
        ("writer_signature_sha256", "observer_signature_sha256"),
        ("writer_authorization_publication_record_sha256", "observer_admission_publication_record_sha256"),
    ):
        if value[writer] == value[observer]:
            raise ValidationError("$: writer and observer continuity domains must differ")
    if value["outer_continuity_sha256"] != _digest_fields(value, OUTER_CONTINUITY_FIELDS):
        raise ValidationError("$: outer continuity digest mismatch")


def validate_recovery_continuity(
    authorization: Mapping[str, Any],
    admission: Mapping[str, Any],
    outer: Mapping[str, Any],
) -> None:
    """Cross-bind the separately published writer, observer, and outer records."""

    _validate_recovery_admission_authorization(authorization)
    _validate_recovery_admission(admission)
    _validate_outer_recovery_continuity(outer)
    shared = (
        "generation", "phase", "domain_record_sha256", "operation_body_sha256",
        "source_target_sha256", "projection_sha256", "writer_receipt_sha256",
        "terminal_receipt_sha256", "fork_request_sha256", "fork_result_sha256",
        "fork_proof_sha256", "recovery_target_sha256", "fork_provider_sha256",
        "boundary_sha256",
    )
    for field in shared:
        if authorization[field] != admission[field] or admission[field] != outer[field]:
            raise ValidationError(f"$.{field}: recovery continuity mismatch")
    exact_links = {
        "authorization_claims_sha256": authorization["claims_sha256"],
        "authorization_sha256": authorization["authorization_sha256"],
        "admission_request_sha256": admission["admission_request_sha256"],
        "admission_claims_sha256": admission["admission_claims_sha256"],
        "admission_sha256": admission["admission_sha256"],
        "continuity_sha256": admission["continuity_sha256"],
        "writer_ledger_sha256": authorization["writer_ledger_sha256"],
        "writer_root_sha256": authorization["writer_root_sha256"],
        "writer_oracle_sha256": authorization["writer_oracle_sha256"],
        "writer_signature_sha256": authorization["writer_signature_sha256"],
        "writer_authorization_publication_record_sha256": authorization["publication_record_sha256"],
        "writer_authorization_publication_completed_at": authorization["publication_completed_at"],
        "observer_ledger_sha256": admission["observer_ledger_sha256"],
        "observer_root_sha256": admission["observer_root_sha256"],
        "observer_oracle_sha256": admission["observer_oracle_sha256"],
        "observer_signature_sha256": admission["observer_signature_sha256"],
        "observer_admission_publication_record_sha256": admission["publication_record_sha256"],
        "observer_admission_publication_completed_at": admission["publication_completed_at"],
    }
    for field, expected in exact_links.items():
        if outer[field] != expected:
            raise ValidationError(f"$.{field}: outer recovery binding mismatch")
    for field in (
        "observer_authority_sha256", "observer_broker_sha256",
        "observer_ledger_sha256", "observer_root_sha256", "observer_oracle_sha256",
    ):
        if authorization[field] != admission[field]:
            raise ValidationError(f"$.{field}: observer target substitution")


def validate_recovery_chain(
    domain_record: Mapping[str, Any],
    writer_receipt: Mapping[str, Any],
    terminal_lifecycle: Mapping[str, Any],
    writer_authorization_publication: Mapping[str, Any],
    authorization: Mapping[str, Any],
    observer_admission_publication: Mapping[str, Any],
    admission: Mapping[str, Any],
    observer_admission_lifecycle: Mapping[str, Any],
    observer_receipt: Mapping[str, Any],
    observer_evidence_publication: Mapping[str, Any],
    observer_cleanup_lifecycle: Mapping[str, Any],
    outer: Mapping[str, Any],
) -> None:
    """Validate the complete synthetic writer-to-recovery evidence chain.

    This function deliberately accepts complete closed records rather than a
    bag of caller-supplied hashes.  It binds their canonical digests, trusted
    role-separated keys, exact identities/targets, marker equality, and the
    full terminal/publication/read temporal sequence.
    """

    # The authoritative entry point independently applies every exact closed
    # schema. Field-set checks alone cannot enforce string patterns, integer
    # types, enums, or nested constraints after a caller re-signs malformed
    # content.
    schema_bindings = (
        (domain_record, "domain-separation.schema.json"),
        (writer_receipt, "writer-receipt.schema.json"),
        (terminal_lifecycle, "lifecycle-receipt.schema.json"),
        (writer_authorization_publication, "ledger-publication.schema.json"),
        (authorization, "recovery-admission-authorization.schema.json"),
        (observer_admission_publication, "ledger-publication.schema.json"),
        (admission, "recovery-admission.schema.json"),
        (observer_admission_lifecycle, "observer-admission-lifecycle.schema.json"),
        (observer_receipt, "observer-receipt.schema.json"),
        (observer_evidence_publication, "ledger-publication.schema.json"),
        (observer_cleanup_lifecycle, "observer-cleanup-lifecycle.schema.json"),
        (outer, "recovery-boundary-continuity.schema.json"),
    )
    for record, schema_name in schema_bindings:
        schema = strict_json_loads((SCHEMA_ROOT / schema_name).read_bytes())
        assert_schema_is_closed(schema)
        validate(record, schema)

    # Keep explicit field closure at the semantic boundary as defense in
    # depth and to make exact nested record ownership evident here.
    _require_exact_fields(domain_record, DOMAIN_SEPARATION_FIELDS, "authority domain")
    for role in ("writer", "observer"):
        _require_exact_fields(domain_record[role], DOMAIN_ROLE_FIELDS, f"{role} domain")
    _require_exact_fields(writer_receipt, WRITER_RECEIPT_FIELDS, "writer receipt")
    _require_exact_fields(
        terminal_lifecycle, TERMINAL_LIFECYCLE_FIELDS, "terminal lifecycle"
    )
    _require_exact_fields(
        terminal_lifecycle["writer_terminal_proof"],
        WRITER_TERMINAL_PROOF_FIELDS,
        "writer terminal proof",
    )
    _require_exact_fields(
        writer_authorization_publication,
        LEDGER_PUBLICATION_FIELDS,
        "writer authorization publication",
    )
    _require_exact_fields(authorization, AUTHORIZATION_RECORD_FIELDS, "authorization")
    _require_exact_fields(
        observer_admission_publication,
        LEDGER_PUBLICATION_FIELDS,
        "observer admission publication",
    )
    _require_exact_fields(admission, ADMISSION_RECORD_FIELDS, "admission")
    _require_exact_fields(
        observer_admission_lifecycle,
        OBSERVER_ADMISSION_LIFECYCLE_FIELDS,
        "observer admission lifecycle",
    )
    _require_exact_fields(observer_receipt, OBSERVER_RECEIPT_FIELDS, "observer receipt")
    _require_exact_fields(
        observer_evidence_publication,
        LEDGER_PUBLICATION_FIELDS,
        "observer evidence publication",
    )
    _require_exact_fields(
        observer_cleanup_lifecycle,
        OBSERVER_CLEANUP_LIFECYCLE_FIELDS,
        "observer cleanup lifecycle",
    )
    _require_exact_fields(outer, OUTER_RECORD_FIELDS, "outer continuity")
    _require_record_identity(domain_record, "authority-domain-separation")
    _require_record_identity(writer_receipt, "writer-marker-receipt", "writer")
    _require_record_identity(terminal_lifecycle, "lifecycle-side-effect-receipt")
    _require_record_identity(
        authorization, "recovery-admission-authorization", "writer"
    )
    _require_record_identity(admission, "recovery-admission", "observer")
    _require_record_identity(observer_receipt, "observer-recovery-receipt", "observer")
    _require_record_identity(outer, "recovery-boundary-continuity")
    for record in (
        domain_record, writer_receipt, terminal_lifecycle,
        writer_authorization_publication, authorization,
        observer_admission_publication, admission, observer_admission_lifecycle,
        observer_receipt, observer_evidence_publication,
        observer_cleanup_lifecycle, outer,
    ):
        _reject_non_ascii(record)
        _reject_zero_hashes(record)

    _validate_domain_separation(domain_record)
    writer_issued, writer_expires = _validate_receipt_common(writer_receipt)
    terminal_issued, terminal_expires = _validate_receipt_common(terminal_lifecycle)
    observer_issued, observer_expires = _validate_receipt_common(observer_receipt)
    admission_lifecycle_issued, admission_lifecycle_expires = (
        _validate_observer_lifecycle(observer_admission_lifecycle)
    )
    cleanup_lifecycle_issued, cleanup_lifecycle_expires = (
        _validate_observer_lifecycle(observer_cleanup_lifecycle)
    )
    _validate_signature_digest(writer_receipt, "signature", "signature_sha256")
    _validate_signature_digest(terminal_lifecycle, "signature", "signature_sha256")
    _validate_signature_digest(observer_receipt, "signature", "signature_sha256")
    for publication in (
        writer_authorization_publication,
        observer_admission_publication,
        observer_evidence_publication,
    ):
        _validate_ledger_publication(publication)
    _validate_lifecycle(terminal_lifecycle, terminal_issued)
    _validate_observer(observer_receipt, observer_issued)
    validate_recovery_continuity(authorization, admission, outer)
    if terminal_lifecycle["side_effect"] != "fork-post":
        raise ValidationError("$: recovery chain requires exact terminal fork-post lifecycle")
    if terminal_lifecycle["terminal_result"] != "success":
        raise ValidationError("$: recovery chain requires successful terminal lifecycle")

    domain_digest = canonical_record_sha256(domain_record)
    writer_digest = canonical_record_sha256(writer_receipt)
    terminal_digest = canonical_record_sha256(terminal_lifecycle)
    writer_publication_digest = canonical_record_sha256(writer_authorization_publication)
    authorization_digest = canonical_record_sha256(authorization)
    admission_publication_digest = canonical_record_sha256(observer_admission_publication)
    admission_digest = canonical_record_sha256(admission)
    admission_lifecycle_digest = canonical_record_sha256(observer_admission_lifecycle)
    observer_digest = canonical_record_sha256(observer_receipt)
    evidence_publication_digest = canonical_record_sha256(observer_evidence_publication)
    cleanup_lifecycle_digest = canonical_record_sha256(observer_cleanup_lifecycle)

    for name, value in (
        ("writer receipt", writer_receipt), ("terminal lifecycle", terminal_lifecycle),
        ("writer authorization publication", writer_authorization_publication),
        ("authorization", authorization), ("admission", admission),
        ("observer admission publication", observer_admission_publication),
        ("observer admission lifecycle", observer_admission_lifecycle),
        ("observer receipt", observer_receipt),
        ("observer evidence publication", observer_evidence_publication),
        ("observer cleanup lifecycle", observer_cleanup_lifecycle),
        ("outer continuity", outer),
    ):
        if value["domain_record_sha256"] != domain_digest:
            raise ValidationError(f"$: {name} does not bind exact authority-domain record")

    expected_records = {
        "writer_receipt_sha256": writer_digest,
        "terminal_receipt_sha256": terminal_digest,
        "authorization_record_sha256": authorization_digest,
        "admission_record_sha256": admission_digest,
        "observer_receipt_sha256": observer_digest,
        "observer_admission_lifecycle_sha256": admission_lifecycle_digest,
        "observer_cleanup_lifecycle_sha256": cleanup_lifecycle_digest,
    }
    if terminal_lifecycle["writer_receipt_sha256"] != writer_digest:
        raise ValidationError("$: terminal lifecycle does not bind writer receipt")
    for record in (authorization, admission, outer):
        if record["writer_receipt_sha256"] != writer_digest:
            raise ValidationError("$: publication chain does not bind writer receipt")
        if record["terminal_receipt_sha256"] != terminal_digest:
            raise ValidationError("$: publication chain does not bind terminal lifecycle")
    for field, expected in expected_records.items():
        if outer[field] != expected:
            raise ValidationError(f"$.{field}: outer canonical record digest mismatch")

    publication_specs = (
        (
            writer_authorization_publication, "writer-authorization",
            authorization["claims_sha256"], writer_publication_digest,
            authorization, "writer_authorization",
        ),
        (
            observer_admission_publication, "observer-admission",
            admission["admission_claims_sha256"], admission_publication_digest,
            admission, "observer_admission",
        ),
        (
            observer_evidence_publication, "observer-evidence",
            observer_receipt["evidence_sha256"], evidence_publication_digest,
            observer_receipt, "observer_evidence",
        ),
    )
    if len({writer_publication_digest, admission_publication_digest, evidence_publication_digest}) != 3:
        raise ValidationError("$: publication completion records must be distinct")
    for publication, publication_kind, published_object, record_digest, owner, outer_prefix in publication_specs:
        if publication["publication_kind"] != publication_kind:
            raise ValidationError("$: wrong ledger publication kind")
        if publication["published_object_sha256"] != published_object:
            raise ValidationError("$: ledger publication binds wrong object")
        if owner["publication_record_sha256"] != record_digest:
            raise ValidationError("$: signed owner does not bind exact publication record")
        if owner["publication_completed_at"] != publication["completed_at"]:
            raise ValidationError("$: signed owner publication completion mismatch")
        if outer[f"{outer_prefix}_publication_record_sha256"] != record_digest:
            raise ValidationError("$: outer continuity publication record mismatch")
        if outer[f"{outer_prefix}_publication_completed_at"] != publication["completed_at"]:
            raise ValidationError("$: outer continuity publication chronology mismatch")
    for publication, owner in (
        (writer_authorization_publication, authorization),
        (observer_admission_publication, admission),
    ):
        if owner["publication_request_sha256"] != publication["publication_request_sha256"]:
            raise ValidationError("$: signed owner publication request mismatch")
        if owner["publication_result_sha256"] != publication["publication_result_sha256"]:
            raise ValidationError("$: signed owner publication result mismatch")

    lifecycle_links = (
        (
            observer_admission_lifecycle,
            admission_publication_digest,
            "observer_admission_lifecycle_sha256",
            admission_lifecycle_digest,
        ),
        (
            observer_cleanup_lifecycle,
            evidence_publication_digest,
            "observer_cleanup_lifecycle_sha256",
            cleanup_lifecycle_digest,
        ),
    )
    for lifecycle, publication_digest, outer_field, lifecycle_digest in lifecycle_links:
        if lifecycle["bound_publication_record_sha256"] != publication_digest:
            raise ValidationError("$: observer lifecycle publication binding mismatch")
        if lifecycle["authorization_sha256"] != authorization["authorization_sha256"]:
            raise ValidationError("$: observer lifecycle authorization mismatch")
        if lifecycle["admission_sha256"] != admission["admission_sha256"]:
            raise ValidationError("$: observer lifecycle admission mismatch")
        if lifecycle["admission_record_sha256"] != admission_digest:
            raise ValidationError("$: observer lifecycle admission record mismatch")
        if outer[outer_field] != lifecycle_digest:
            raise ValidationError("$: outer observer lifecycle digest mismatch")
    if observer_cleanup_lifecycle["observer_receipt_sha256"] != observer_digest:
        raise ValidationError("$: observer cleanup receipt binding mismatch")
    if (
        observer_cleanup_lifecycle["observer_evidence_sha256"]
        != observer_receipt["evidence_sha256"]
    ):
        raise ValidationError("$: observer cleanup evidence binding mismatch")

    common = (
        "generation", "phase", "source_target_sha256", "recovery_target_sha256",
    )
    for field in common:
        values = (
            writer_receipt[field], terminal_lifecycle[field], authorization[field],
            admission[field], observer_admission_lifecycle[field],
            observer_receipt[field], observer_cleanup_lifecycle[field], outer[field],
        )
        if len(set(values)) != 1:
            raise ValidationError(f"$.{field}: full-chain identity mismatch")
    for publication in (
        writer_authorization_publication,
        observer_admission_publication,
        observer_evidence_publication,
    ):
        if publication["generation"] != outer["generation"]:
            raise ValidationError("$: publication generation mismatch")
        if publication["phase"] != outer["phase"]:
            raise ValidationError("$: publication phase mismatch")
        if publication["operation_body_sha256"] != outer["operation_body_sha256"]:
            raise ValidationError("$: publication operation body mismatch")
    for record in (writer_receipt, terminal_lifecycle, observer_receipt):
        if record["operation_sha256"] != outer["operation_body_sha256"]:
            raise ValidationError("$: receipt operation digest does not bind canonical operation body")
    for lifecycle in (observer_admission_lifecycle, observer_cleanup_lifecycle):
        if lifecycle["operation_body_sha256"] != outer["operation_body_sha256"]:
            raise ValidationError("$: observer lifecycle operation body mismatch")
    if len({writer_receipt["fixed_key_sha256"], observer_receipt["fixed_key_sha256"], outer["fixed_key_sha256"]}) != 1:
        raise ValidationError("$: fixed-key digest mismatch")
    if len({writer_receipt["marker_sha256"], observer_receipt["marker_sha256"], outer["marker_sha256"]}) != 1:
        raise ValidationError("$: writer/read-one/read-two marker hashes differ")
    if observer_receipt["read_one_marker_sha256"] != writer_receipt["marker_sha256"]:
        raise ValidationError("$: first observer read differs from writer marker")
    if observer_receipt["read_two_marker_sha256"] != writer_receipt["marker_sha256"]:
        raise ValidationError("$: second observer read differs from writer marker")
    if observer_receipt["authorization_sha256"] != authorization["authorization_sha256"]:
        raise ValidationError("$: observer receipt authorization mismatch")
    if observer_receipt["admission_sha256"] != admission["admission_sha256"]:
        raise ValidationError("$: observer receipt admission mismatch")

    writer_domain = domain_record["writer"]
    observer_domain = domain_record["observer"]
    identity_links = (
        (writer_receipt, writer_domain, "authority_app_sha256", "authority_app_sha256"),
        (writer_receipt, writer_domain, "broker_app_sha256", "broker_app_sha256"),
        (writer_receipt, writer_domain, "ledger_sha256", "ledger_sha256"),
        (writer_receipt, writer_domain, "root_key_sha256", "root_key_sha256"),
        (authorization, writer_domain, "writer_authority_sha256", "authority_app_sha256"),
        (authorization, writer_domain, "writer_broker_sha256", "broker_app_sha256"),
        (authorization, writer_domain, "writer_ledger_sha256", "ledger_sha256"),
        (authorization, writer_domain, "writer_root_sha256", "root_key_sha256"),
        (writer_authorization_publication, writer_domain, "ledger_sha256", "ledger_sha256"),
        (writer_authorization_publication, writer_domain, "root_key_sha256", "root_key_sha256"),
        (observer_receipt, observer_domain, "authority_app_sha256", "authority_app_sha256"),
        (observer_receipt, observer_domain, "broker_app_sha256", "broker_app_sha256"),
        (observer_receipt, observer_domain, "ledger_sha256", "ledger_sha256"),
        (observer_receipt, observer_domain, "root_key_sha256", "root_key_sha256"),
        (admission, observer_domain, "observer_authority_sha256", "authority_app_sha256"),
        (admission, observer_domain, "observer_broker_sha256", "broker_app_sha256"),
        (admission, observer_domain, "observer_ledger_sha256", "ledger_sha256"),
        (admission, observer_domain, "observer_root_sha256", "root_key_sha256"),
        (observer_admission_publication, observer_domain, "ledger_sha256", "ledger_sha256"),
        (observer_admission_publication, observer_domain, "root_key_sha256", "root_key_sha256"),
        (observer_evidence_publication, observer_domain, "ledger_sha256", "ledger_sha256"),
        (observer_evidence_publication, observer_domain, "root_key_sha256", "root_key_sha256"),
        (observer_admission_lifecycle, observer_domain, "authority_app_sha256", "authority_app_sha256"),
        (observer_admission_lifecycle, observer_domain, "broker_app_sha256", "broker_app_sha256"),
        (observer_admission_lifecycle, observer_domain, "ledger_sha256", "ledger_sha256"),
        (observer_admission_lifecycle, observer_domain, "root_key_sha256", "root_key_sha256"),
        (observer_cleanup_lifecycle, observer_domain, "authority_app_sha256", "authority_app_sha256"),
        (observer_cleanup_lifecycle, observer_domain, "broker_app_sha256", "broker_app_sha256"),
        (observer_cleanup_lifecycle, observer_domain, "ledger_sha256", "ledger_sha256"),
        (observer_cleanup_lifecycle, observer_domain, "root_key_sha256", "root_key_sha256"),
    )
    for record, trusted, record_field, trusted_field in identity_links:
        if record[record_field] != trusted[trusted_field]:
            raise ValidationError(f"$.{record_field}: record/domain identity mismatch")
    if terminal_lifecycle["controller_sha256"] != writer_domain["authority_app_sha256"]:
        raise ValidationError("$: terminal controller is not writer authority app")
    if terminal_lifecycle["root_key_sha256"] != writer_domain["root_key_sha256"]:
        raise ValidationError("$: terminal proof is not writer-root signed")
    for publication, trusted in (
        (writer_authorization_publication, writer_domain),
        (observer_admission_publication, observer_domain),
        (observer_evidence_publication, observer_domain),
    ):
        if publication["signing_kid"] != trusted["signing_kid"]:
            raise ValidationError("$: publication signing KID/domain mismatch")

    writer_key = _decode_public_key(writer_domain["signing_public_key"], "$.writer.signing_public_key")
    observer_key = _decode_public_key(observer_domain["signing_public_key"], "$.observer.signing_public_key")
    for value, signature_field, kid_field in (
        (writer_receipt, "signature", "signing_kid"),
        (terminal_lifecycle, "signature", "signing_kid"),
        (writer_authorization_publication, "signature", "signing_kid"),
        (authorization, "writer_signature", "writer_signing_kid"),
    ):
        _verify_trusted_record_signature(
            value, role="writer", public_key=writer_key,
            expected_kid=writer_domain["signing_kid"],
            signature_field=signature_field, kid_field=kid_field,
        )
    for value, signature_field, kid_field in (
        (admission, "observer_signature", "observer_signing_kid"),
        (observer_admission_publication, "signature", "signing_kid"),
        (observer_admission_lifecycle, "signature", "signing_kid"),
        (observer_receipt, "signature", "signing_kid"),
        (observer_evidence_publication, "signature", "signing_kid"),
        (observer_cleanup_lifecycle, "signature", "signing_kid"),
    ):
        _verify_trusted_record_signature(
            value, role="observer", public_key=observer_key,
            expected_kid=observer_domain["signing_kid"],
            signature_field=signature_field, kid_field=kid_field,
        )

    lifecycle_read_one = _timestamp(
        terminal_lifecycle["provider_read_one_at"], "$.terminal_lifecycle.provider_read_one_at"
    )
    lifecycle_read_two = _timestamp(
        terminal_lifecycle["provider_read_two_at"], "$.terminal_lifecycle.provider_read_two_at"
    )
    writer_publication_requested = _timestamp(
        writer_authorization_publication["requested_at"], "$.writer_publication.requested_at"
    )
    writer_publication_completed = _timestamp(
        writer_authorization_publication["completed_at"], "$.writer_publication.completed_at"
    )
    writer_publication_expires = _timestamp(
        writer_authorization_publication["expires_at"], "$.writer_publication.expires_at"
    )
    authorization_issued = _timestamp(authorization["issued_at"], "$.authorization.issued_at")
    authorization_expires = _timestamp(authorization["expires_at"], "$.authorization.expires_at")
    admission_publication_requested = _timestamp(
        observer_admission_publication["requested_at"], "$.admission_publication.requested_at"
    )
    admission_publication_completed = _timestamp(
        observer_admission_publication["completed_at"], "$.admission_publication.completed_at"
    )
    admission_publication_expires = _timestamp(
        observer_admission_publication["expires_at"], "$.admission_publication.expires_at"
    )
    admission_issued = _timestamp(admission["issued_at"], "$.admission.issued_at")
    admission_expires = _timestamp(admission["expires_at"], "$.admission.expires_at")
    admission_lifecycle_requested = _timestamp(
        observer_admission_lifecycle["requested_at"],
        "$.observer_admission_lifecycle.requested_at",
    )
    admission_lifecycle_completed = _timestamp(
        observer_admission_lifecycle["completed_at"],
        "$.observer_admission_lifecycle.completed_at",
    )
    read_one = _timestamp(observer_receipt["recovery_read_one_at"], "$.observer_receipt.recovery_read_one_at")
    read_two = _timestamp(observer_receipt["recovery_read_two_at"], "$.observer_receipt.recovery_read_two_at")
    evidence_publication_requested = _timestamp(
        observer_evidence_publication["requested_at"], "$.evidence_publication.requested_at"
    )
    evidence_publication_completed = _timestamp(
        observer_evidence_publication["completed_at"], "$.evidence_publication.completed_at"
    )
    evidence_publication_expires = _timestamp(
        observer_evidence_publication["expires_at"], "$.evidence_publication.expires_at"
    )
    cleanup_lifecycle_requested = _timestamp(
        observer_cleanup_lifecycle["requested_at"],
        "$.observer_cleanup_lifecycle.requested_at",
    )
    cleanup_lifecycle_completed = _timestamp(
        observer_cleanup_lifecycle["completed_at"],
        "$.observer_cleanup_lifecycle.completed_at",
    )
    if not (
        writer_issued < lifecycle_read_one < lifecycle_read_two <= terminal_issued
        <= writer_publication_requested < writer_publication_completed <= authorization_issued
        <= admission_publication_requested < admission_publication_completed <= admission_issued
        <= admission_lifecycle_requested < admission_lifecycle_completed
        <= admission_lifecycle_issued < read_one < read_two
        <= evidence_publication_requested < evidence_publication_completed
        <= observer_issued < cleanup_lifecycle_requested
        < cleanup_lifecycle_completed <= cleanup_lifecycle_issued
    ):
        raise ValidationError("$: terminal/publication/read evidence order is invalid")
    # All authority consumption points use exclusive upper bounds.  Equality
    # with an expiry is expired, never a successful boundary case.
    if not writer_publication_completed < terminal_expires:
        raise ValidationError("$: terminal lifecycle expired before writer publication")
    if not terminal_issued < writer_expires:
        raise ValidationError("$: writer receipt expired before terminal proof")
    if not writer_publication_completed < writer_expires:
        raise ValidationError("$: writer receipt expired before authorization publication")
    if not writer_publication_completed < writer_publication_expires:
        raise ValidationError("$: writer publication record expired at completion")
    if not authorization_issued < writer_publication_expires:
        raise ValidationError("$: writer publication expired before authorization issuance")
    if not admission_publication_completed < authorization_expires:
        raise ValidationError("$: writer authorization expired before observer admission")
    if not admission_publication_completed < admission_publication_expires:
        raise ValidationError("$: observer admission publication expired at completion")
    if not admission_issued < admission_publication_expires:
        raise ValidationError("$: observer admission publication expired before admission issuance")
    if not admission_lifecycle_issued < admission_expires:
        raise ValidationError("$: observer admission expired before lifecycle completion")
    if not admission_lifecycle_issued < admission_lifecycle_expires:
        raise ValidationError("$: observer admission lifecycle exclusive lifetime violated")
    if not evidence_publication_completed < admission_expires:
        raise ValidationError("$: observer admission expired before evidence completion")
    if not evidence_publication_completed < evidence_publication_expires:
        raise ValidationError("$: observer evidence publication expired at completion")
    if not observer_issued < evidence_publication_expires:
        raise ValidationError("$: observer evidence publication expired before receipt issuance")
    if not cleanup_lifecycle_issued < observer_expires:
        raise ValidationError("$: observer receipt expired before cleanup completion")
    if not cleanup_lifecycle_issued < cleanup_lifecycle_expires:
        raise ValidationError("$: observer cleanup lifecycle exclusive lifetime violated")
    if not writer_issued < writer_expires or not observer_issued < observer_expires:
        raise ValidationError("$: receipt exclusive lifetime violated")


def _validate_domain_separation(value: Mapping[str, Any]) -> None:
    fields = ("authority_app_sha256", "broker_app_sha256", "ledger_sha256", "root_key_sha256")
    identities = [value[domain][field] for domain in ("writer", "observer") for field in fields]
    if len(identities) != 8 or len(set(identities)) != 8:
        raise ValidationError("$: four apps, two ledgers, and two roots must be pairwise distinct")
    keys: list[bytes] = []
    kids: list[str] = []
    for role in ("writer", "observer"):
        record = value[role]
        trusted = TRUSTED_SYNTHETIC_SIGNERS[role]
        if record["signing_kid"] != trusted["kid"]:
            raise ValidationError(f"$.{role}: untrusted signing KID")
        if record["signing_public_key"] != trusted["public_key"]:
            raise ValidationError(f"$.{role}: untrusted signing public key")
        public_key = _decode_public_key(record["signing_public_key"], f"$.{role}.signing_public_key")
        root_digest = hashlib.sha256(public_key).hexdigest()
        if record["root_key_sha256"] != root_digest:
            raise ValidationError(f"$.{role}: root digest does not bind trusted public key")
        expected_kid = f"synthetic-{role}-{root_digest[:16]}"
        if record["signing_kid"] != expected_kid:
            raise ValidationError(f"$.{role}: signing KID does not bind role and public key")
        keys.append(public_key)
        kids.append(record["signing_kid"])
    if len(set(keys)) != 2 or len(set(kids)) != 2:
        raise ValidationError("$: writer and observer trusted keys/KIDs must differ")


def _reject_zero_hashes(value: Any, path: str = "$") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if key.endswith("_sha256") and child == "0" * 64:
                raise ValidationError(f"{path}.{key}: zero digest forbidden")
            _reject_zero_hashes(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _reject_zero_hashes(child, f"{path}[{index}]")


def _validate_observer(value: Mapping[str, Any], issued: datetime) -> None:
    first = _timestamp(value["recovery_read_one_at"], "$.recovery_read_one_at")
    second = _timestamp(value["recovery_read_two_at"], "$.recovery_read_two_at")
    separation = (second - first).total_seconds()
    if separation < value["minimum_read_separation_seconds"] or separation > 60:
        raise ValidationError("$: observer reads are not within the bounded separation")
    if second > issued:
        raise ValidationError("$: observer receipt issued before the second recovery read")
    if value["evidence_sha256"] != _digest_fields(value, OBSERVER_EVIDENCE_FIELDS):
        raise ValidationError("$: observer evidence digest mismatch")
    publication_completed = _timestamp(
        value["publication_completed_at"], "$.publication_completed_at"
    )
    if not second <= publication_completed <= issued:
        raise ValidationError("$: observer evidence publication chronology mismatch")


def _validate_lifecycle(value: Mapping[str, Any], issued: datetime) -> None:
    if value["side_effect"] not in LIFECYCLE_EFFECTS:
        raise ValidationError("$.side_effect: not an exact low-level effect")
    first = _timestamp(value["provider_read_one_at"], "$.provider_read_one_at")
    second = _timestamp(value["provider_read_two_at"], "$.provider_read_two_at")
    separation = (second - first).total_seconds()
    if separation < value["minimum_provider_read_separation_seconds"] or separation > 60:
        raise ValidationError("$: provider reads are not within the bounded separation")
    if second > issued:
        raise ValidationError("$: lifecycle receipt issued before the second provider read")
    successful = value["terminal_result"] == "success"
    reconciled = value["reconciliation"]
    if successful != (reconciled in {"not-ambiguous", "reconciled-one"}):
        raise ValidationError("$: terminal result and reconciliation disagree")
    proof = value["writer_terminal_proof"]
    terminal_evidence = {
        "provider_fact_source": value["provider_fact_source"],
        "provider_read_one_at": value["provider_read_one_at"],
        "provider_read_two_at": value["provider_read_two_at"],
        "minimum_provider_read_separation_seconds": value["minimum_provider_read_separation_seconds"],
        "writer_terminal_proof": proof,
    }
    expected_digest = hashlib.sha256(canonical_bytes(terminal_evidence)).hexdigest()
    if value["terminal_evidence_sha256"] != expected_digest:
        raise ValidationError("$: terminal evidence digest does not bind canonical proof")
    for fields in (
        ("original_firewall_sha256", "firewall_read_one_sha256", "firewall_read_two_sha256"),
        ("original_projection_sha256", "projection_read_one_sha256", "projection_read_two_sha256"),
        ("provider_operation_inventory_sha256", "action_ledger_read_one_sha256", "action_ledger_read_two_sha256"),
    ):
        if len({proof[field] for field in fields}) != 1:
            raise ValidationError("$: terminal complete delayed-read projections differ")
    if value["side_effect"] == "fork-post" and successful:
        required_true = (
            "broker_deleted", "direct_get_absent", "delete_action_terminal",
            "app_inventory_pagination_complete",
            "deployment_inventory_pagination_complete",
            "provider_operation_inventory_pagination_complete",
            "full_redeploy_complete",
            "old_instance_grace_elapsed", "leaf_revoked", "capability_revoked",
            "mtls_revoked", "wrapping_key_revoked", "binding_absent",
            "credential_absent", "firewall_restored",
        )
        required_zero = (
            "app_inventory_count", "deployment_inventory_count",
            "nonterminal_deployment_count", "rollback_capable_deployment_count",
            "nonterminal_provider_operation_count",
        )
        if not all(proof[name] is True for name in required_true):
            raise ValidationError("$: successful fork lacks complete writer-deleted proof")
        if not all(proof[name] == 0 for name in required_zero):
            raise ValidationError("$: successful fork retains writer app/deployment state")
        empty_inventory = hashlib.sha256(canonical_bytes([])).hexdigest()
        if proof["app_inventory_sha256"] != empty_inventory:
            raise ValidationError("$: empty complete app inventory digest mismatch")
        if proof["deployment_inventory_sha256"] != empty_inventory:
            raise ValidationError("$: empty complete deployment inventory digest mismatch")


def validate_evidence(value: Any, schema: Mapping[str, Any]) -> None:
    """Universally enforce schema plus kind-specific semantic invariants."""

    validate(value, schema)
    _reject_non_ascii(value)
    _reject_zero_hashes(value)
    kind = value.get("kind") if isinstance(value, dict) else None
    if kind == "authority-domain-separation":
        _validate_domain_separation(value)
        return
    if kind == "recovery-admission-authorization":
        _validate_recovery_admission_authorization(value)
        return
    if kind == "recovery-admission":
        _validate_recovery_admission(value)
        return
    if kind == "recovery-boundary-continuity":
        _validate_outer_recovery_continuity(value)
        return
    if kind == "ledger-publication-completion":
        _validate_ledger_publication(value)
        return
    if kind in {"observer-admission-lifecycle", "observer-cleanup-lifecycle"}:
        _validate_observer_lifecycle(value)
        return
    if kind not in {"writer-marker-receipt", "observer-recovery-receipt", "lifecycle-side-effect-receipt"}:
        raise ValidationError("$.kind: unsupported evidence kind")
    issued, _ = _validate_receipt_common(value)
    _validate_signature_digest(value, "signature", "signature_sha256")
    if kind == "observer-recovery-receipt":
        _validate_observer(value, issued)
    elif kind == "lifecycle-side-effect-receipt":
        _validate_lifecycle(value, issued)


# Minimal RFC 8032 Ed25519 verifier, implemented with Python stdlib integers.
_P = 2**255 - 19
_L = 2**252 + 27742317777372353535851937790883648493
_D = (-121665 * pow(121666, _P - 2, _P)) % _P
_I = pow(2, (_P - 1) // 4, _P)


def _recover_x(y: int, sign: int) -> int:
    x2 = ((y * y - 1) * pow(_D * y * y + 1, _P - 2, _P)) % _P
    x = pow(x2, (_P + 3) // 8, _P)
    if (x * x - x2) % _P:
        x = (x * _I) % _P
    if (x * x - x2) % _P:
        raise ValidationError("invalid Ed25519 point")
    return _P - x if (x & 1) != sign else x


def _decode_point(encoded: bytes) -> tuple[int, int]:
    if len(encoded) != 32:
        raise ValidationError("invalid Ed25519 point length")
    raw = int.from_bytes(encoded, "little")
    y = raw & ((1 << 255) - 1)
    if y >= _P:
        raise ValidationError("noncanonical Ed25519 point")
    point = (_recover_x(y, raw >> 255), y)
    if (point[1] * point[1] - point[0] * point[0] - 1 - _D * point[0] * point[0] * point[1] * point[1]) % _P:
        raise ValidationError("invalid Ed25519 point")
    return point


def _point_add(left: tuple[int, int], right: tuple[int, int]) -> tuple[int, int]:
    x1, y1 = left
    x2, y2 = right
    product = (_D * x1 * x2 * y1 * y2) % _P
    return (
        ((x1 * y2 + x2 * y1) * pow(1 + product, _P - 2, _P)) % _P,
        ((y1 * y2 + x1 * x2) * pow(1 - product, _P - 2, _P)) % _P,
    )


def _scalar_mult(point: tuple[int, int], scalar: int) -> tuple[int, int]:
    result = (0, 1)
    addend = point
    while scalar:
        if scalar & 1:
            result = _point_add(result, addend)
        addend = _point_add(addend, addend)
        scalar >>= 1
    return result


_BASE_Y = (4 * pow(5, _P - 2, _P)) % _P
_BASE = (_recover_x(_BASE_Y, 0), _BASE_Y)


def _encode_point(point: tuple[int, int]) -> bytes:
    x, y = point
    return (y | ((x & 1) << 255)).to_bytes(32, "little")


def ed25519_public_key_from_seed(seed: bytes) -> bytes:
    if len(seed) != 32:
        raise ValidationError("Ed25519 seed must be 32 bytes")
    digest = hashlib.sha512(seed).digest()
    scalar_bytes = bytes([digest[0] & 248]) + digest[1:31] + bytes([(digest[31] & 63) | 64])
    return _encode_point(_scalar_mult(_BASE, int.from_bytes(scalar_bytes, "little")))


def ed25519_sign(seed: bytes, message: bytes) -> bytes:
    """Create a deterministic RFC 8032 signature for synthetic fixtures."""

    if len(seed) != 32:
        raise ValidationError("Ed25519 seed must be 32 bytes")
    digest = hashlib.sha512(seed).digest()
    scalar_bytes = bytes([digest[0] & 248]) + digest[1:31] + bytes([(digest[31] & 63) | 64])
    scalar = int.from_bytes(scalar_bytes, "little")
    prefix = digest[32:]
    public_key = _encode_point(_scalar_mult(_BASE, scalar))
    nonce = int.from_bytes(hashlib.sha512(prefix + message).digest(), "little") % _L
    encoded_r = _encode_point(_scalar_mult(_BASE, nonce))
    challenge = int.from_bytes(hashlib.sha512(encoded_r + public_key + message).digest(), "little") % _L
    response = (nonce + challenge * scalar) % _L
    return encoded_r + response.to_bytes(32, "little")


def ed25519_verify(public_key: bytes, message: bytes, signature: bytes) -> bool:
    """Strictly verify one Ed25519 signature without external dependencies."""

    if len(public_key) != 32 or len(signature) != 64:
        return False
    scalar = int.from_bytes(signature[32:], "little")
    if scalar >= _L:
        return False
    try:
        public_point = _decode_point(public_key)
        r_point = _decode_point(signature[:32])
    except ValidationError:
        return False
    if _scalar_mult(public_point, 8) == (0, 1) or _scalar_mult(r_point, 8) == (0, 1):
        return False
    if _scalar_mult(public_point, _L) != (0, 1) or _scalar_mult(r_point, _L) != (0, 1):
        return False
    challenge = int.from_bytes(hashlib.sha512(signature[:32] + public_key + message).digest(), "little") % _L
    return _scalar_mult(_BASE, scalar) == _point_add(r_point, _scalar_mult(public_point, challenge))
