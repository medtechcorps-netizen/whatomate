#!/usr/bin/env python3
"""Fail-closed verifier for ReReply's four-phase rollout evidence.

The workflow intentionally keeps policy parsing, ZIP extraction, and plan
validation in a small standard-library-only program that is also exercised by
protected CI.  It never talks to GitHub or a cloud provider and performs no
deployment or package mutation.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import sys
import zipfile
from pathlib import Path, PurePosixPath
from typing import Any


PHASES = ["baseline", "bridge", "backend", "ui"]
COMPONENTS = ["gmail-relay", "meta-relay", "web"]
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
# GitHub exposes IDs as JSON numbers. Keep every string-form ID within the
# exact-integer range shared by jq and JavaScript before converting it with
# --argjson for REST response comparisons.
RUN_ID_RE = re.compile(r"^[1-9][0-9]{0,14}$")
TAG_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,127}$")
MAX_ARCHIVE_FILES = 32
MAX_ARCHIVE_COMPRESSED_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_FILE_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_UNCOMPRESSED_BYTES = 256 * 1024 * 1024
MAX_COMPRESSION_RATIO = 100
MAX_DIRECTORY_COMPRESSED_BYTES = 64
STREAM_CHUNK_BYTES = 1024 * 1024
SCHEMA_DRAFT = "https://json-schema.org/draft/2020-12/schema"
SCHEMA_ID = "https://rereply.app/schemas/exact-four-phase-rollout-plan-v1.json"
SCHEMA_TITLE = "ReReply exact four-phase rollout plan"

EXPECTED_JOBS = [
    "Aggregate the exact three-digest release set",
    "Attest gmail-relay",
    "Attest meta-relay",
    "Attest web",
    "Build and publish gmail-relay",
    "Build and publish meta-relay",
    "Build and publish web",
    "Exact release image gate",
    "Scan, SBOM, and verify gmail-relay",
    "Scan, SBOM, and verify meta-relay",
    "Scan, SBOM, and verify web",
    "Verify attestations for gmail-relay",
    "Verify attestations for meta-relay",
    "Verify attestations for web",
    "Verify exact release-set attestations",
    "Verify protected-main authority and exact validation",
]

ARTIFACT_INVENTORIES = {
    "image": ["image.json", "remote-descriptor.json"],
    "scanned": [
        "image-inspect.json",
        "image.json",
        "remote-descriptor.json",
        "sbom.spdx.json",
        "scan.json",
        "secret-report.json",
        "vulnerability-report.json",
    ],
    "attested": [
        "attestation-record.json",
        "exact-source-predicate.json",
        "image-inspect.json",
        "image.json",
        "provenance.bundle.json",
        "remote-descriptor.json",
        "sbom.bundle.json",
        "sbom.spdx.json",
        "scan.json",
        "secret-report.json",
        "source-binding.bundle.json",
        "vulnerability-report.json",
    ],
    "verified": [
        "exact-source-predicate.json",
        "image-inspect.json",
        "image.json",
        "remote-descriptor.json",
        "sbom.spdx.json",
        "scan.json",
        "secret-report.json",
        "verified-provenance.json",
        "verified-sbom.json",
        "verified-source-binding.json",
        "vulnerability-report.json",
    ],
    "release-set": [
        "release-set-provenance.bundle.json",
        "release-set-source-binding.bundle.json",
        "release-set.json",
        "release-set.sha256",
    ],
    "verified-release-set": [
        "release-set.json",
        "release-set.sha256",
        "verified-release-set-binding.json",
        "verified-release-set-provenance.json",
    ],
}

ROLLBACK = {
    "baseline": {"allowed_targets": [], "forbidden_targets": []},
    "bridge": {"allowed_targets": ["baseline"], "forbidden_targets": []},
    "backend": {
        "allowed_targets": ["bridge"],
        "forbidden_targets": ["baseline"],
    },
    "ui": {
        "allowed_targets": ["backend", "bridge"],
        "forbidden_targets": ["baseline"],
    },
}

MIGRATION = {
    "component": "web",
    "binding": "same-image-digest",
    "entrypoint": ["./rereply"],
    "arguments": ["rls-migrate", "-config", "config.toml"],
}

EXPECTED_PHASE_SOURCES = {
    "baseline": {
        "source_sha": "ad580e949f264a67032ad004f2995d0199af84c9",
        "root_tree": "feb8096aa3c9e89296295cb866709601b84b75e2",
        "frontend_tree": "0f16215d6bab3496e23d8a1e0d4d2c048ad4ba74",
        "internal_tree": "f70860bcee374d507b7e6443f4f21df2855b217f",
    },
    "bridge": {
        "source_sha": "66b9351a5e7767cb7450e17cb6362990b4fc4f6f",
        "root_tree": "349735041a41afd12c3754631963097da2323adf",
        "frontend_tree": "0f16215d6bab3496e23d8a1e0d4d2c048ad4ba74",
        "internal_tree": "32763888c556305e8aee180183c92f36ddf5d195",
    },
    "backend": {
        "source_sha": "022cce50d96dff0991a742abec579bd6bc25963a",
        "root_tree": "0ec920b9a5842d3a37cc6b506e7d5172c99068b6",
        "frontend_tree": "0f16215d6bab3496e23d8a1e0d4d2c048ad4ba74",
        "internal_tree": "494d3957ff3559375f406886be45646049ec9378",
    },
    "ui": {
        "source_sha": "f69f45fbb60962e7f7c679fbb6c1e5a2b391b455",
        "root_tree": "844d742a232c6b3b92b0522c1c96dba98321bca7",
        "frontend_tree": "f1cdd8186e2b724dc4bba41081578b2b003a6910",
        "internal_tree": "a1da97143c17f3d02e47250269d00f238ac0e38c",
    },
}

EXPECTED_COMPONENTS = {
    "gmail-relay": {
        "image": "ghcr.io/medtechcorps-netizen/rereply-release-gmail-relay",
        "dockerfile": "docker/release/gmail-relay.Dockerfile",
        "dockerfile_sha256": "f0c2ac09e585b3504123c1a5d336658dca5a52a0124a04aace202fb61a98a098",
        "user": "relay",
        "working_dir": "/app",
        "entrypoint": ["/app/gmail-relay"],
        "cmd": None,
        "port": "8082/tcp",
        "smoke": "binary",
    },
    "meta-relay": {
        "image": "ghcr.io/medtechcorps-netizen/rereply-release-meta-relay",
        "dockerfile": "docker/release/meta-relay.Dockerfile",
        "dockerfile_sha256": "38a6dcceaa252750f4ffd016002687423f062b15d8b47f6478af2185b628eb25",
        "user": "relay",
        "working_dir": "/app",
        "entrypoint": ["/app/meta-relay"],
        "cmd": None,
        "port": "8081/tcp",
        "smoke": "binary",
    },
    "web": {
        "image": "ghcr.io/medtechcorps-netizen/rereply-release-web",
        "dockerfile": "docker/release/web.Dockerfile",
        "dockerfile_sha256": "f441cbd45867a32a509af11c246f2c4e6d81104db99b728d7ccd865c7f9ad381",
        "user": "rereply",
        "working_dir": "/app",
        "entrypoint": ["./rereply"],
        "cmd": ["server", "-config", "config.toml"],
        "port": "8080/tcp",
        "smoke": "version",
    },
}

EXPECTED_MATERIALS = {
    "ubuntu_snapshot": {
        "id": "20260824T000000Z",
        "packages": {
            "ca-certificates": "20260601~24.04.1",
            "espeak-ng": "1.51+dfsg-12build1",
            "ffmpeg": "7:6.1.1-3ubuntu5",
            "opus-tools": "0.2-1build3",
            "tzdata": "2026c-0ubuntu0.24.04.1",
        },
        "dockerfile": "docker/release/web.Dockerfile",
        "stage": None,
    },
    "base_images": [
        {
            "name": "docker.io/library/node:22-alpine",
            "digest": "sha256:76789712cd1ae89a1225eac9077010d68987a423588042dac30446f502f1858c",
            "uses": [
                {"dockerfile": "docker/release/web.Dockerfile", "stage": "frontend-builder"},
                {"dockerfile": "docker/release/web.Dockerfile", "stage": "piper-download"},
            ],
        },
        {
            "name": "docker.io/library/golang:1.26.6-alpine",
            "digest": "sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae",
            "uses": [
                {"dockerfile": "docker/release/gmail-relay.Dockerfile", "stage": "builder"},
                {"dockerfile": "docker/release/meta-relay.Dockerfile", "stage": "builder"},
                {"dockerfile": "docker/release/web.Dockerfile", "stage": "builder"},
            ],
        },
        {
            "name": "docker.io/library/ubuntu:24.04",
            "digest": "sha256:1e0a86e57d247923571b75e0aaf48a1449cf8c543d51fb3e07a4a7d7bfa79316",
            "uses": [{"dockerfile": "docker/release/web.Dockerfile", "stage": None}],
        },
    ],
    "scratch_stages": [
        {"dockerfile": "docker/release/gmail-relay.Dockerfile", "stage": None},
        {"dockerfile": "docker/release/meta-relay.Dockerfile", "stage": None},
    ],
    "direct_downloads": [
        {
            "name": "piper_linux_x86_64.tar.gz",
            "url": "https://github.com/rhasspy/piper/releases/download/2023.11.14-2/piper_linux_x86_64.tar.gz",
            "sha256": "a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992",
            "dockerfile": "docker/release/web.Dockerfile",
            "stage": "piper-download",
            "destination": "/tmp/piper.tar.gz",
            "runtime_path": None,
            "runtime_mode": None,
        },
        {
            "name": "en_US-lessac-medium.onnx",
            "url": "https://huggingface.co/rhasspy/piper-voices/resolve/39ab474be869e9181350af6a65e4953eef67aaa0/en/en_US/lessac/medium/en_US-lessac-medium.onnx",
            "sha256": "5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f",
            "dockerfile": "docker/release/web.Dockerfile",
            "stage": "piper-download",
            "destination": "/tmp/piper-models/en_US-lessac-medium.onnx",
            "runtime_path": "/opt/piper/models/en_US-lessac-medium.onnx",
            "runtime_mode": "0644",
        },
        {
            "name": "en_US-lessac-medium.onnx.json",
            "url": "https://huggingface.co/rhasspy/piper-voices/resolve/39ab474be869e9181350af6a65e4953eef67aaa0/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json",
            "sha256": "efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0",
            "dockerfile": "docker/release/web.Dockerfile",
            "stage": "piper-download",
            "destination": "/tmp/piper-models/en_US-lessac-medium.onnx.json",
            "runtime_path": "/opt/piper/models/en_US-lessac-medium.onnx.json",
            "runtime_mode": "0644",
        },
    ],
}


class EvidenceError(ValueError):
    """Raised for any non-canonical or unauthorized evidence."""


def fail(message: str) -> None:
    raise EvidenceError(message)


def require(condition: bool, message: str) -> None:
    if not condition:
        fail(message)


def require_exact_int(value: Any, label: str, minimum: int | None = None, maximum: int | None = None) -> int:
    require(type(value) is int, f"{label} must be an integer")
    if minimum is not None:
        require(value >= minimum, f"{label} is below its minimum")
    if maximum is not None:
        require(value <= maximum, f"{label} exceeds its maximum")
    return value


def validate_json_value(value: Any, label: str = "JSON value") -> None:
    if value is None or type(value) in {bool, int, str}:
        return
    if type(value) is list:
        for index, item in enumerate(value):
            validate_json_value(item, f"{label}[{index}]")
        return
    if type(value) is dict:
        for key, item in value.items():
            require(type(key) is str, f"{label} contains a non-string object key")
            validate_json_value(item, f"{label}.{key}")
        return
    if type(value) is float:
        fail(f"{label} contains a forbidden floating-point number")
    fail(f"{label} contains a non-JSON value of type {type(value).__name__}")


def strict_json_equal(left: Any, right: Any) -> bool:
    if type(left) is not type(right):
        return False
    if type(left) is list:
        return len(left) == len(right) and all(
            strict_json_equal(left_item, right_item)
            for left_item, right_item in zip(left, right)
        )
    if type(left) is dict:
        return set(left) == set(right) and all(
            strict_json_equal(left[key], right[key]) for key in left
        )
    return left == right


def strict_json_loads(raw: str, label: str) -> Any:
    def reject_float(token: str) -> Any:
        raise EvidenceError(f"{label} contains forbidden floating-point number {token!r}")

    def reject_constant(token: str) -> Any:
        raise EvidenceError(f"{label} contains forbidden non-finite number {token!r}")

    def build_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise EvidenceError(f"{label} contains duplicate object key {key!r}")
            result[key] = value
        return result

    try:
        value = json.loads(
            raw,
            object_pairs_hook=build_object,
            parse_float=reject_float,
            parse_constant=reject_constant,
        )
    except EvidenceError:
        raise
    except json.JSONDecodeError as exc:
        raise EvidenceError(f"{label} is not valid JSON") from exc
    validate_json_value(value, label)
    return value


def canonical_json_bytes(value: Any) -> bytes:
    validate_json_value(value, "canonical JSON")
    try:
        encoded = json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
            allow_nan=False,
        )
    except (TypeError, ValueError) as exc:
        raise EvidenceError("value cannot be represented as canonical JSON") from exc
    return (encoded + "\n").encode("utf-8")


def exact_keys(value: Any, keys: set[str], label: str) -> dict[str, Any]:
    require(type(value) is dict, f"{label} must be an object")
    require(set(value) == keys, f"{label} keys differ: {sorted(value)}")
    return value


def real_regular_file(path: Path, label: str) -> Path:
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError as exc:
        raise EvidenceError(f"{label} is missing: {path}") from exc
    require(stat.S_ISREG(mode), f"{label} must be a regular non-symlink file: {path}")
    return path


def real_directory(path: Path, label: str) -> Path:
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError as exc:
        raise EvidenceError(f"{label} is missing: {path}") from exc
    require(stat.S_ISDIR(mode), f"{label} must be a real directory: {path}")
    return path


def load_json(path: Path, label: str) -> Any:
    real_regular_file(path, label)
    try:
        raw = path.read_text(encoding="utf-8")
    except UnicodeDecodeError as exc:
        raise EvidenceError(f"{label} is not valid UTF-8 JSON: {path}") from exc
    return strict_json_loads(raw, f"{label}: {path}")


def dump_canonical(value: Any, path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json_bytes(value))


def sha256_file(path: Path) -> str:
    real_regular_file(path, "hashed file")
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require_sha1(value: Any, label: str) -> str:
    require(isinstance(value, str) and SHA1_RE.fullmatch(value) is not None, f"invalid {label}")
    return value


def require_sha256(value: Any, label: str) -> str:
    require(
        isinstance(value, str) and SHA256_RE.fullmatch(value) is not None,
        f"invalid {label}",
    )
    return value


def require_digest(value: Any, label: str) -> str:
    require(
        isinstance(value, str) and DIGEST_RE.fullmatch(value) is not None,
        f"invalid {label}",
    )
    return value


def require_run_id(value: Any, label: str) -> str:
    require(
        isinstance(value, str) and RUN_ID_RE.fullmatch(value) is not None,
        f"invalid {label}",
    )
    return value


def require_attempt(value: Any, label: str) -> int:
    return require_exact_int(value, label, 1, 2_147_483_647)


SCHEMA_KEYWORDS = {
    "$schema",
    "$id",
    "$ref",
    "$defs",
    "title",
    "type",
    "const",
    "enum",
    "pattern",
    "minimum",
    "maximum",
    "minLength",
    "maxLength",
    "minItems",
    "maxItems",
    "uniqueItems",
    "prefixItems",
    "items",
    "required",
    "properties",
    "additionalProperties",
}
EXPECTED_SCHEMA_PROPERTIES = {
    "schema_version",
    "authority",
    "repository",
    "control",
    "activation_order",
    "baseline_forbidden_after_activation",
    "phases",
}
EXPECTED_SCHEMA_DEFS = {
    "sha1",
    "sha256",
    "digest",
    "runId",
    "attempt",
    "control",
    "source",
    "validation",
    "imageBuild",
    "artifact",
    "image",
    "migration",
    "rollback",
    "phase",
}


def _schema_ref(root: dict[str, Any], reference: Any, path: str) -> dict[str, Any]:
    require(
        isinstance(reference, str)
        and re.fullmatch(r"#/\$defs/[A-Za-z][A-Za-z0-9]*", reference) is not None,
        f"{path} has a non-local or malformed $ref",
    )
    name = reference.rsplit("/", 1)[1]
    require(name in root["$defs"], f"{path} references missing definition {name!r}")
    target = root["$defs"][name]
    require(type(target) is dict, f"{path} reference target must be an object")
    return target


def _validate_schema_node(node: Any, root: dict[str, Any], path: str, *, root_node: bool = False) -> None:
    require(type(node) is dict, f"schema node {path} must be an object")
    require(set(node) <= SCHEMA_KEYWORDS, f"schema node {path} uses unsupported keywords")
    if not root_node:
        require(
            not ({"$schema", "$id", "$defs", "title"} & set(node)),
            f"schema metadata is only allowed at the root: {path}",
        )
    if "$ref" in node:
        require(set(node) == {"$ref"}, f"$ref siblings are forbidden at {path}")
        _schema_ref(root, node["$ref"], path)
        return

    if "type" in node:
        require(
            node["type"] in {"object", "array", "string", "integer"},
            f"unsupported schema type at {path}",
        )
    if "const" in node:
        validate_json_value(node["const"], f"schema const {path}")
    if "enum" in node:
        enum = node["enum"]
        require(type(enum) is list and enum, f"schema enum at {path} must be nonempty")
        for item in enum:
            validate_json_value(item, f"schema enum {path}")
        for index, item in enumerate(enum):
            require(
                not any(strict_json_equal(item, prior) for prior in enum[:index]),
                f"schema enum at {path} contains duplicates",
            )
    if "pattern" in node:
        require(type(node["pattern"]) is str, f"schema pattern at {path} must be a string")
        try:
            re.compile(node["pattern"])
        except re.error as exc:
            raise EvidenceError(f"schema pattern at {path} is invalid") from exc

    for keyword in ("minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"):
        if keyword in node:
            require_exact_int(node[keyword], f"schema {keyword} at {path}", 0)
    if "minimum" in node and "maximum" in node:
        require(node["minimum"] <= node["maximum"], f"schema integer bounds invert at {path}")
    if "minLength" in node and "maxLength" in node:
        require(node["minLength"] <= node["maxLength"], f"schema string bounds invert at {path}")
    if "minItems" in node and "maxItems" in node:
        require(node["minItems"] <= node["maxItems"], f"schema array bounds invert at {path}")
    if "uniqueItems" in node:
        require(type(node["uniqueItems"]) is bool, f"schema uniqueItems at {path} must be boolean")

    if node.get("type") == "object":
        require(node.get("additionalProperties") is False, f"object schema must fail closed at {path}")
        properties = node.get("properties")
        required = node.get("required")
        require(type(properties) is dict, f"object schema properties missing at {path}")
        require(
            type(required) is list
            and all(type(item) is str for item in required)
            and len(required) == len(set(required)),
            f"object schema required list is invalid at {path}",
        )
        require(set(required) == set(properties), f"object schema must require every property at {path}")
        for name, child in properties.items():
            require(type(name) is str and name, f"invalid property name at {path}")
            _validate_schema_node(child, root, f"{path}.properties.{name}")
    else:
        require(
            not ({"properties", "required", "additionalProperties"} & set(node)),
            f"object-only schema keywords used at {path}",
        )

    if node.get("type") == "array":
        if "prefixItems" in node:
            require(type(node["prefixItems"]) is list, f"prefixItems must be an array at {path}")
            for index, child in enumerate(node["prefixItems"]):
                _validate_schema_node(child, root, f"{path}.prefixItems[{index}]")
        if "items" in node:
            items = node["items"]
            require(items is False or type(items) is dict, f"items is invalid at {path}")
            if type(items) is dict:
                _validate_schema_node(items, root, f"{path}.items")
    else:
        require(
            not ({"minItems", "maxItems", "uniqueItems", "prefixItems", "items"} & set(node)),
            f"array-only schema keywords used at {path}",
        )
    if node.get("type") != "string":
        require(
            not ({"pattern", "minLength", "maxLength"} & set(node)),
            f"string-only schema keywords used at {path}",
        )
    if node.get("type") != "integer":
        require(
            not ({"minimum", "maximum"} & set(node)),
            f"integer-only schema keywords used at {path}",
        )

    if "$defs" in node:
        require(root_node and type(node["$defs"]) is dict, f"$defs is invalid at {path}")
        for name, child in node["$defs"].items():
            require(
                re.fullmatch(r"[A-Za-z][A-Za-z0-9]*", name) is not None,
                f"invalid definition name {name!r}",
            )
            _validate_schema_node(child, root, f"$.$defs.{name}")


def validate_plan_schema(schema: Any) -> dict[str, Any]:
    schema = exact_keys(
        schema,
        {"$schema", "$id", "title", "type", "additionalProperties", "required", "properties", "$defs"},
        "rollout plan schema",
    )
    require(schema["$schema"] == SCHEMA_DRAFT, "rollout plan schema draft differs")
    require(schema["$id"] == SCHEMA_ID, "rollout plan schema ID differs")
    require(schema["title"] == SCHEMA_TITLE, "rollout plan schema title differs")
    require(schema["type"] == "object", "rollout plan schema root type differs")
    require(schema["additionalProperties"] is False, "rollout plan schema root is not fail-closed")
    require(
        schema["required"]
        == [
            "schema_version",
            "authority",
            "repository",
            "control",
            "activation_order",
            "baseline_forbidden_after_activation",
            "phases",
        ],
        "rollout plan schema root required policy differs",
    )
    require(set(schema["properties"]) == EXPECTED_SCHEMA_PROPERTIES, "schema root properties differ")
    require(set(schema["$defs"]) == EXPECTED_SCHEMA_DEFS, "schema definition set differs")
    require(
        strict_json_equal(schema["properties"]["schema_version"], {"const": 1})
        and strict_json_equal(schema["properties"]["authority"], {"const": "digest-only"})
        and strict_json_equal(
            schema["properties"]["repository"], {"const": "medtechcorps-netizen/whatomate"}
        )
        and strict_json_equal(schema["properties"]["activation_order"], {"const": PHASES})
        and strict_json_equal(
            schema["properties"]["baseline_forbidden_after_activation"], {"const": "backend"}
        ),
        "schema root authority constants differ",
    )
    _validate_schema_node(schema, schema, "$", root_node=True)
    return schema


def validate_json_schema(
    value: Any,
    schema: dict[str, Any],
    root: dict[str, Any] | None = None,
    path: str = "$",
    depth: int = 0,
) -> None:
    require(depth <= 100, f"schema reference depth exceeded at {path}")
    if root is None:
        root = schema
    if "$ref" in schema:
        validate_json_schema(value, _schema_ref(root, schema["$ref"], path), root, path, depth + 1)
        return

    expected_type = schema.get("type")
    if expected_type == "object":
        require(type(value) is dict, f"{path} must be an object")
    elif expected_type == "array":
        require(type(value) is list, f"{path} must be an array")
    elif expected_type == "string":
        require(type(value) is str, f"{path} must be a string")
    elif expected_type == "integer":
        require(type(value) is int, f"{path} must be an integer")

    if "const" in schema:
        require(strict_json_equal(value, schema["const"]), f"{path} differs from schema const")
    if "enum" in schema:
        require(
            any(strict_json_equal(value, candidate) for candidate in schema["enum"]),
            f"{path} is not in the schema enum",
        )
    if type(value) is str:
        if "pattern" in schema:
            require(re.search(schema["pattern"], value) is not None, f"{path} does not match pattern")
        if "minLength" in schema:
            require(len(value) >= schema["minLength"], f"{path} is shorter than minLength")
        if "maxLength" in schema:
            require(len(value) <= schema["maxLength"], f"{path} exceeds maxLength")
    if type(value) is int:
        if "minimum" in schema:
            require(value >= schema["minimum"], f"{path} is below minimum")
        if "maximum" in schema:
            require(value <= schema["maximum"], f"{path} exceeds maximum")
    if type(value) is dict:
        required = schema.get("required", [])
        for key in required:
            require(key in value, f"{path} is missing required property {key!r}")
        properties = schema.get("properties", {})
        if schema.get("additionalProperties") is False:
            require(set(value) <= set(properties), f"{path} contains an additional property")
        for key, child in properties.items():
            if key in value:
                validate_json_schema(value[key], child, root, f"{path}.{key}", depth + 1)
    if type(value) is list:
        if "minItems" in schema:
            require(len(value) >= schema["minItems"], f"{path} has fewer than minItems")
        if "maxItems" in schema:
            require(len(value) <= schema["maxItems"], f"{path} exceeds maxItems")
        if schema.get("uniqueItems") is True:
            for index, item in enumerate(value):
                require(
                    not any(strict_json_equal(item, prior) for prior in value[:index]),
                    f"{path} violates uniqueItems",
                )
        prefix = schema.get("prefixItems", [])
        for index, child in enumerate(prefix[: len(value)]):
            validate_json_schema(value[index], child, root, f"{path}[{index}]", depth + 1)
        if len(value) > len(prefix):
            items = schema.get("items")
            require(items is not False, f"{path} contains items forbidden after prefixItems")
            if type(items) is dict:
                for index in range(len(prefix), len(value)):
                    validate_json_schema(value[index], items, root, f"{path}[{index}]", depth + 1)


def validate_contract(contract: Any) -> dict[str, Any]:
    contract = exact_keys(
        contract,
        {
            "schema_version",
            "repository",
            "authority",
            "workflows",
            "predicates",
            "phase_order",
            "rollback",
            "migration",
            "image_workflow",
            "validation_workflow",
            "capsule",
        },
        "contract",
    )
    require_exact_int(contract["schema_version"], "contract schema version", 1, 1)
    require(contract["repository"] == "medtechcorps-netizen/whatomate", "repository differs")
    require(contract["authority"] == "digest-only", "authority must be digest-only")
    require(
        contract["workflows"]
        == {
            "rollout": ".github/workflows/aggregate-exact-four-phase-rollout.yml",
            "image": ".github/workflows/build-attest-exact-release-images.yml",
            "validation": ".github/workflows/validate-exact-release-source.yml",
        },
        "workflow paths differ",
    )
    require(
        contract["predicates"]
        == {
            "release_set": "https://rereply.app/attestations/exact-release-set/v1",
            "rollout_plan": "https://rereply.app/attestations/exact-four-phase-rollout/v1",
        },
        "predicate types differ",
    )
    require(contract["phase_order"] == PHASES, "phase order differs")
    require(contract["rollback"] == ROLLBACK, "rollback floors differ")
    require(contract["migration"] == MIGRATION, "migration contract differs")

    image_workflow = exact_keys(
        contract["image_workflow"],
        {"gate_job_name", "expected_jobs", "artifact_inventories"},
        "image_workflow",
    )
    require(image_workflow["gate_job_name"] == "Exact release image gate", "image gate differs")
    require(image_workflow["expected_jobs"] == EXPECTED_JOBS, "exact 16-job set differs")
    require(
        image_workflow["artifact_inventories"] == ARTIFACT_INVENTORIES,
        "artifact inventories differ",
    )
    require(
        contract["validation_workflow"] == {"gate_job_name": "Exact source validation gate"},
        "validation gate differs",
    )
    capsule = exact_keys(
        contract["capsule"], {"retention_days", "root_files", "phase_files"}, "capsule contract"
    )
    require_exact_int(capsule["retention_days"], "capsule retention days", 90, 90)
    require(
        capsule
        == {
            "retention_days": 90,
            "root_files": [
                "rollout-plan-policy.bundle.json",
                "rollout-plan-provenance.bundle.json",
                "rollout-plan.json",
                "rollout-plan.sha256",
            ],
            "phase_files": [
                "release-set-provenance.bundle.json",
                "release-set-source-binding.bundle.json",
                "release-set.json",
                "release-set.sha256",
            ],
        },
        "capsule contract differs",
    )
    return contract


def validate_manifest(manifest: Any) -> dict[str, Any]:
    manifest = exact_keys(
        manifest,
        {"schema_version", "repository", "validation", "phases", "release"},
        "source manifest",
    )
    require_exact_int(manifest["schema_version"], "source manifest schema version", 1, 1)
    require(manifest["repository"] == "medtechcorps-netizen/whatomate", "manifest repository differs")
    require(
        manifest["validation"]
        == {
            "workflow_path": ".github/workflows/validate-exact-release-source.yml",
            "gate_job_name": "Exact source validation gate",
        },
        "manifest validation authority differs",
    )
    phases = exact_keys(manifest["phases"], set(PHASES), "manifest phases")
    for phase in PHASES:
        entry = exact_keys(
            phases[phase],
            {"source_sha", "root_tree", "frontend_tree", "internal_tree"},
            f"manifest phase {phase}",
        )
        for key, value in entry.items():
            require_sha1(value, f"manifest {phase}.{key}")
        require(
            strict_json_equal(entry, EXPECTED_PHASE_SOURCES[phase]),
            f"manifest phase authority differs: {phase}",
        )
    release = exact_keys(manifest["release"], {"platform", "components", "materials"}, "release")
    require(release["platform"] == "linux/amd64", "release platform differs")
    components = exact_keys(release["components"], set(COMPONENTS), "release components")
    for component in COMPONENTS:
        entry = exact_keys(
            components[component],
            {
                "image",
                "dockerfile",
                "dockerfile_sha256",
                "user",
                "working_dir",
                "entrypoint",
                "cmd",
                "port",
                "smoke",
            },
            f"component {component}",
        )
        require(
            type(entry["image"]) is str
            and entry["image"] == f"ghcr.io/medtechcorps-netizen/rereply-release-{component}",
            f"component image differs: {component}",
        )
        require(
            type(entry["dockerfile"]) is str
            and entry["dockerfile"] == f"docker/release/{component}.Dockerfile",
            f"Dockerfile differs: {component}",
        )
        require_sha256(entry["dockerfile_sha256"], f"Dockerfile hash {component}")
        require(
            type(entry["user"]) is str
            and re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", entry["user"]) is not None,
            f"component user is invalid: {component}",
        )
        require(entry["working_dir"] == "/app", f"component working directory differs: {component}")
        require(
            type(entry["entrypoint"]) is list
            and entry["entrypoint"]
            and all(type(item) is str and item for item in entry["entrypoint"]),
            f"component entrypoint is invalid: {component}",
        )
        require(
            entry["cmd"] is None
            or (
                type(entry["cmd"]) is list
                and entry["cmd"]
                and all(type(item) is str and item for item in entry["cmd"])
            ),
            f"component command is invalid: {component}",
        )
        require(
            type(entry["port"]) is str
            and re.fullmatch(r"[1-9][0-9]{0,4}/tcp", entry["port"]) is not None
            and int(entry["port"].split("/", 1)[0]) <= 65535,
            f"component port is invalid: {component}",
        )
        require(entry["smoke"] in {"binary", "version"}, f"component smoke mode is invalid: {component}")
        require(
            strict_json_equal(entry, EXPECTED_COMPONENTS[component]),
            f"component release authority differs: {component}",
        )

    materials = exact_keys(
        release["materials"],
        {"ubuntu_snapshot", "base_images", "scratch_stages", "direct_downloads"},
        "release materials",
    )
    snapshot = exact_keys(
        materials["ubuntu_snapshot"], {"id", "packages", "dockerfile", "stage"}, "Ubuntu snapshot"
    )
    require(
        type(snapshot["id"]) is str
        and re.fullmatch(r"20[0-9]{6}T[0-9]{6}Z", snapshot["id"]) is not None,
        "Ubuntu snapshot ID is invalid",
    )
    packages = exact_keys(
        snapshot["packages"], set(EXPECTED_MATERIALS["ubuntu_snapshot"]["packages"]), "Ubuntu packages"
    )
    for package, version in packages.items():
        require(
            type(version) is str
            and version
            and re.fullmatch(r"[A-Za-z0-9.+:~_-]+", version) is not None,
            f"Ubuntu package version is invalid: {package}",
        )
    require(snapshot["dockerfile"] == "docker/release/web.Dockerfile", "Ubuntu Dockerfile differs")
    require(snapshot["stage"] is None, "Ubuntu snapshot stage must be null")

    base_images = materials["base_images"]
    require(type(base_images) is list and len(base_images) == 3, "base image set must contain three entries")
    for index, base_image in enumerate(base_images):
        base_image = exact_keys(base_image, {"name", "digest", "uses"}, f"base image {index}")
        require(
            type(base_image["name"]) is str
            and re.fullmatch(r"docker\.io/library/[a-z0-9._-]+:[A-Za-z0-9._-]+", base_image["name"])
            is not None,
            f"base image name is invalid at index {index}",
        )
        require_digest(base_image["digest"], f"base image digest {index}")
        uses = base_image["uses"]
        require(type(uses) is list and uses, f"base image uses are invalid at index {index}")
        for use_index, use in enumerate(uses):
            use = exact_keys(use, {"dockerfile", "stage"}, f"base image {index} use {use_index}")
            require(
                use["dockerfile"] in {entry["dockerfile"] for entry in EXPECTED_COMPONENTS.values()},
                f"base image use Dockerfile differs at {index}/{use_index}",
            )
            require(
                use["stage"] is None
                or (
                    type(use["stage"]) is str
                    and re.fullmatch(r"[a-z][a-z0-9-]*", use["stage"]) is not None
                ),
                f"base image stage is invalid at {index}/{use_index}",
            )

    scratch_stages = materials["scratch_stages"]
    require(type(scratch_stages) is list and len(scratch_stages) == 2, "scratch stage set differs")
    for index, scratch in enumerate(scratch_stages):
        scratch = exact_keys(scratch, {"dockerfile", "stage"}, f"scratch stage {index}")
        require(
            scratch["dockerfile"] in {entry["dockerfile"] for entry in EXPECTED_COMPONENTS.values()},
            f"scratch Dockerfile differs at index {index}",
        )
        require(scratch["stage"] is None, f"scratch stage selector must be null at index {index}")

    downloads = materials["direct_downloads"]
    require(type(downloads) is list and len(downloads) == 3, "direct download set differs")
    for index, download in enumerate(downloads):
        download = exact_keys(
            download,
            {
                "name",
                "url",
                "sha256",
                "dockerfile",
                "stage",
                "destination",
                "runtime_path",
                "runtime_mode",
            },
            f"direct download {index}",
        )
        require(
            type(download["name"]) is str
            and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", download["name"]) is not None,
            f"direct download name is invalid at index {index}",
        )
        require(
            type(download["url"]) is str
            and re.fullmatch(r"https://(github\.com|huggingface\.co)/[^\s]+", download["url"])
            is not None,
            f"direct download URL is invalid at index {index}",
        )
        require_sha256(download["sha256"], f"direct download SHA-256 {index}")
        require(download["dockerfile"] == "docker/release/web.Dockerfile", f"download Dockerfile differs at {index}")
        require(
            type(download["stage"]) is str
            and re.fullmatch(r"[a-z][a-z0-9-]*", download["stage"]) is not None,
            f"direct download stage is invalid at index {index}",
        )
        require(
            type(download["destination"]) is str
            and download["destination"].startswith("/")
            and ".." not in PurePosixPath(download["destination"]).parts,
            f"direct download destination is invalid at index {index}",
        )
        require(
            download["runtime_path"] is None
            or (
                type(download["runtime_path"]) is str
                and download["runtime_path"].startswith("/")
                and ".." not in PurePosixPath(download["runtime_path"]).parts
            ),
            f"direct download runtime path is invalid at index {index}",
        )
        require(
            download["runtime_mode"] is None
            or (
                type(download["runtime_mode"]) is str
                and re.fullmatch(r"0[0-7]{3}", download["runtime_mode"]) is not None
            ),
            f"direct download runtime mode is invalid at index {index}",
        )
    require(strict_json_equal(materials, EXPECTED_MATERIALS), "release material authority differs")
    return manifest


def normalize_input(raw: str, expected_control_sha: str) -> dict[str, Any]:
    require_sha1(expected_control_sha, "expected control SHA")
    value = strict_json_loads(raw, "phase_evidence_json")
    value = exact_keys(value, {"control_sha", "phases"}, "phase evidence")
    require(value["control_sha"] == expected_control_sha, "input control SHA differs from workflow SHA")
    phases = value["phases"]
    require(type(phases) is list and len(phases) == 4, "input must contain exactly four phases")
    normalized = []
    run_ids: set[str] = set()
    artifact_ids: set[str] = set()
    for index, expected_phase in enumerate(PHASES):
        item = exact_keys(
            phases[index],
            {
                "phase",
                "image_run_id",
                "image_run_attempt",
                "release_set_artifact_id",
                "release_set_sha256",
            },
            f"input phase {index}",
        )
        require(item["phase"] == expected_phase, f"phase {index} must be {expected_phase}")
        run_id = require_run_id(item["image_run_id"], f"{expected_phase} image run ID")
        attempt = require_attempt(item["image_run_attempt"], f"{expected_phase} image run attempt")
        artifact_id = require_run_id(
            item["release_set_artifact_id"], f"{expected_phase} release-set artifact ID"
        )
        release_hash = require_sha256(
            item["release_set_sha256"], f"{expected_phase} release-set SHA-256"
        )
        require(run_id not in run_ids, "image run IDs must be unique")
        require(artifact_id not in artifact_ids, "release-set artifact IDs must be unique")
        run_ids.add(run_id)
        artifact_ids.add(artifact_id)
        normalized.append(
            {
                "phase": expected_phase,
                "image_run_id": run_id,
                "image_run_attempt": attempt,
                "release_set_artifact_id": artifact_id,
                "release_set_sha256": release_hash,
            }
        )
    return {"control_sha": expected_control_sha, "phases": normalized}


def artifact_name_map(phase: str, run_id: str, run_attempt: int) -> dict[str, str]:
    require(phase in PHASES, "invalid artifact phase")
    require_run_id(run_id, "artifact run ID")
    require_attempt(run_attempt, "artifact run attempt")
    result: dict[str, str] = {}
    for component in COMPONENTS:
        for kind in ("image", "scanned", "attested", "verified"):
            result[f"{kind}-{phase}-{component}-{run_id}-{run_attempt}"] = kind
    result[f"release-set-{phase}-{run_id}-{run_attempt}"] = "release-set"
    result[f"verified-release-set-{phase}-{run_id}-{run_attempt}"] = "verified-release-set"
    require(len(result) == 14, "internal artifact-name set is not exactly 14")
    return result


def canonical_zip_name(info: zipfile.ZipInfo, label: str) -> str:
    name = info.filename
    require(type(name) is str and name, f"{label} ZIP member name is empty")
    require("\\" not in name and "\x00" not in name, f"unsafe {label} ZIP filename")
    require((info.flag_bits & 0x1) == 0, f"encrypted {label} ZIP member is forbidden: {name}")
    if info.is_dir():
        require(name.endswith("/") and not name.endswith("//"), f"non-canonical directory name: {name}")
        bare_name = name[:-1]
        pure = PurePosixPath(bare_name)
        require(
            bare_name
            and not pure.is_absolute()
            and pure.as_posix() == bare_name
            and all(part not in {"", ".", ".."} for part in pure.parts),
            f"non-canonical or escaping directory name: {name}",
        )
        file_type = stat.S_IFMT((info.external_attr >> 16) & 0xFFFF)
        require(file_type in {0, stat.S_IFDIR}, f"directory ZIP member has unsafe mode: {name}")
        require(info.file_size == 0 and info.CRC == 0, f"directory ZIP member is not empty: {name}")
        require(
            0 <= info.compress_size <= MAX_DIRECTORY_COMPRESSED_BYTES,
            f"directory ZIP member compressed metadata is excessive: {name}",
        )
        return name

    require(not name.endswith("/"), f"file ZIP member has a directory suffix: {name}")
    pure = PurePosixPath(name)
    require(
        not pure.is_absolute()
        and pure.as_posix() == name
        and all(part not in {"", ".", ".."} for part in pure.parts),
        f"non-canonical or escaping {label} ZIP path: {name}",
    )
    file_type = stat.S_IFMT((info.external_attr >> 16) & 0xFFFF)
    require(file_type in {0, stat.S_IFREG}, f"non-regular {label} ZIP member is forbidden: {name}")
    require(0 < info.file_size <= MAX_ARCHIVE_FILE_BYTES, f"{label} ZIP member size is invalid: {name}")
    require(info.compress_size > 0, f"{label} ZIP member compressed size is invalid: {name}")
    require(
        info.file_size <= info.compress_size * MAX_COMPRESSION_RATIO,
        f"{label} ZIP member compression ratio exceeds {MAX_COMPRESSION_RATIO}:1: {name}",
    )
    return name


def stream_zip_members(
    bundle: zipfile.ZipFile,
    infos: list[zipfile.ZipInfo],
    output: Path | None,
    label: str,
) -> None:
    if output is not None:
        if output.exists():
            real_directory(output, f"{label} extraction target")
            require(not any(output.iterdir()), f"{label} extraction target must be empty")
        else:
            output.mkdir(parents=True)

    aggregate_bytes = 0
    for info in infos:
        destination = output / PurePosixPath(info.filename) if output is not None else None
        if destination is not None:
            destination.parent.mkdir(parents=True, exist_ok=True)
            target = destination.open("xb")
        else:
            target = None
        member_bytes = 0
        try:
            with bundle.open(info, "r") as source:
                while True:
                    chunk = source.read(STREAM_CHUNK_BYTES)
                    if not chunk:
                        break
                    require(type(chunk) is bytes, f"{label} ZIP reader returned non-bytes")
                    member_bytes += len(chunk)
                    aggregate_bytes += len(chunk)
                    require(
                        member_bytes <= info.file_size and member_bytes <= MAX_ARCHIVE_FILE_BYTES,
                        f"{label} ZIP member exceeded its streaming byte budget: {info.filename}",
                    )
                    require(
                        aggregate_bytes <= MAX_ARCHIVE_UNCOMPRESSED_BYTES,
                        f"{label} ZIP exceeded its aggregate streaming byte budget",
                    )
                    if target is not None:
                        target.write(chunk)
        finally:
            if target is not None:
                target.close()
        require(member_bytes == info.file_size and member_bytes > 0, f"{label} ZIP member length differs: {info.filename}")
        if destination is not None:
            os.chmod(destination, 0o600)


def inspect_archive(archive: Path, kind: str, contract: dict[str, Any], output: Path | None) -> list[str]:
    real_regular_file(archive, "artifact archive")
    require(0 < archive.stat().st_size <= MAX_ARCHIVE_COMPRESSED_BYTES, "artifact ZIP size is invalid")
    require(kind in ARTIFACT_INVENTORIES, f"unknown artifact kind: {kind}")
    expected = sorted(contract["image_workflow"]["artifact_inventories"][kind])
    with zipfile.ZipFile(archive) as bundle:
        infos = bundle.infolist()
        require(0 < len(infos) <= MAX_ARCHIVE_FILES, "artifact ZIP entry count is invalid")
        names: list[str] = []
        total_size = 0
        for info in infos:
            require(not info.is_dir(), f"directory entry is forbidden: {info.filename}")
            name = canonical_zip_name(info, "artifact")
            require(len(PurePosixPath(name).parts) == 1, f"nested artifact ZIP path is forbidden: {name}")
            total_size += info.file_size
            require(total_size <= MAX_ARCHIVE_UNCOMPRESSED_BYTES, "artifact ZIP is too large")
            names.append(name)
        require(len(names) == len(set(names)), "duplicate ZIP member is forbidden")
        require(sorted(names) == expected, f"{kind} artifact inventory differs: {sorted(names)}")
        stream_zip_members(bundle, infos, output, "artifact")
    return expected


def inspect_capsule_archive(
    archive: Path, contract: dict[str, Any], output: Path | None
) -> list[str]:
    real_regular_file(archive, "capsule archive")
    require(0 < archive.stat().st_size <= MAX_ARCHIVE_COMPRESSED_BYTES, "capsule ZIP size is invalid")
    expected = capsule_paths(contract)
    expected_directories = {"phases/", *(f"phases/{phase}/" for phase in PHASES)}
    with zipfile.ZipFile(archive) as bundle:
        infos = bundle.infolist()
        require(0 < len(infos) <= 32, "capsule ZIP entry count is invalid")
        files: list[zipfile.ZipInfo] = []
        names: list[str] = []
        entry_names: list[str] = []
        total_size = 0
        for info in infos:
            name = canonical_zip_name(info, "capsule")
            entry_names.append(name)
            if info.is_dir():
                require(
                    name in expected_directories,
                    f"unexpected capsule directory: {name}",
                )
                continue
            total_size += info.file_size
            require(total_size <= MAX_ARCHIVE_UNCOMPRESSED_BYTES, "capsule ZIP is too large")
            names.append(name)
            files.append(info)
        require(
            len(entry_names) == len(set(entry_names)),
            "duplicate capsule ZIP member is forbidden",
        )
        require(sorted(names) == expected, f"capsule ZIP inventory differs: {sorted(names)}")
        stream_zip_members(bundle, files, output, "capsule")
    return expected


def strict_release_set(
    release_set_path: Path,
    release_hash_path: Path,
    manifest: dict[str, Any],
    phase_input: dict[str, Any],
    control_sha: str,
    source_manifest_sha256: str,
) -> dict[str, Any]:
    release_set = exact_keys(
        load_json(release_set_path, "release set"),
        {"schema_version", "authority", "phase", "source", "validation", "builder", "images"},
        "release set",
    )
    phase = phase_input["phase"]
    run_id = phase_input["image_run_id"]
    run_attempt = phase_input["image_run_attempt"]
    expected_hash = phase_input["release_set_sha256"]
    actual_hash = sha256_file(release_set_path)
    require(actual_hash == expected_hash, f"release-set SHA-256 differs for {phase}")
    real_regular_file(release_hash_path, "release-set hash file")
    require(
        release_hash_path.read_bytes() == (expected_hash + "\n").encode("ascii"),
        f"non-canonical release-set hash file for {phase}",
    )
    require_exact_int(release_set["schema_version"], "release-set schema version", 1, 1)
    require(release_set["authority"] == "digest-only", "release-set authority differs")
    require(release_set["phase"] == phase, "release-set phase differs")

    source = exact_keys(
        release_set["source"],
        {"repository", "commit", "root_tree", "frontend_tree", "internal_tree", "manifest_sha256"},
        f"{phase} source",
    )
    expected_source = manifest["phases"][phase]
    require(
        source
        == {
            "repository": manifest["repository"],
            "commit": expected_source["source_sha"],
            "root_tree": expected_source["root_tree"],
            "frontend_tree": expected_source["frontend_tree"],
            "internal_tree": expected_source["internal_tree"],
            "manifest_sha256": source_manifest_sha256,
        },
        f"release-set source differs for {phase}",
    )
    validation = exact_keys(
        release_set["validation"], {"run_id", "run_attempt", "run_url"}, f"{phase} validation"
    )
    validation_run_id = require_run_id(validation["run_id"], f"{phase} validation run ID")
    require_attempt(validation["run_attempt"], f"{phase} validation attempt")
    require(
        validation["run_url"]
        == f"https://github.com/medtechcorps-netizen/whatomate/actions/runs/{validation_run_id}",
        f"validation URL differs for {phase}",
    )
    builder = exact_keys(
        release_set["builder"],
        {"workflow_sha", "run_id", "run_attempt", "runner_environment"},
        f"{phase} builder",
    )
    require(
        builder
        == {
            "workflow_sha": control_sha,
            "run_id": run_id,
            "run_attempt": str(run_attempt),
            "runner_environment": "github-hosted",
        },
        f"release-set builder differs for {phase}",
    )

    images = release_set["images"]
    require(type(images) is list and len(images) == 3, f"{phase} must contain three images")
    require([item.get("component") for item in images] == COMPONENTS, f"{phase} image order differs")
    digests: set[str] = set()
    expected_tag = (
        f"{phase}-{source['commit'][:12]}-control-{control_sha[:12]}-run-{run_id}-{run_attempt}"
    )
    for image in images:
        image = exact_keys(
            image,
            {
                "component",
                "image",
                "digest",
                "platform",
                "tag",
                "tag_is_authority",
                "dockerfile",
                "dockerfile_sha256",
            },
            f"{phase} image",
        )
        component = image["component"]
        require(component in COMPONENTS, f"unknown component in {phase}")
        expected_component = manifest["release"]["components"][component]
        require(image["image"] == expected_component["image"], f"image repository differs: {phase}/{component}")
        digest = require_digest(image["digest"], f"image digest {phase}/{component}")
        require(digest not in digests, f"duplicate image digest in {phase}")
        digests.add(digest)
        require(image["platform"] == "linux/amd64", "image platform differs")
        require(image["tag"] == expected_tag and TAG_RE.fullmatch(image["tag"]), "non-canonical audit tag")
        require(image["tag_is_authority"] is False, "tag cannot be authority")
        require(image["dockerfile"] == expected_component["dockerfile"], "Dockerfile differs")
        require(
            image["dockerfile_sha256"] == expected_component["dockerfile_sha256"],
            "Dockerfile hash differs",
        )
    return release_set


def validate_artifact_records(
    records: Any, phase: str, run_id: str, run_attempt: int, release_artifact_id: str
) -> list[dict[str, Any]]:
    require(type(records) is list and len(records) == 14, f"{phase} must record exactly 14 artifacts")
    expected_names = artifact_name_map(phase, run_id, run_attempt)
    result = []
    ids: set[str] = set()
    names: set[str] = set()
    for record in records:
        record = exact_keys(
            record,
            {"artifact_id", "name", "archive_digest", "archive_sha256", "size_in_bytes", "inventory"},
            f"{phase} artifact record",
        )
        artifact_id = require_run_id(record["artifact_id"], "artifact ID")
        name = record["name"]
        require(isinstance(name, str) and name in expected_names, f"unexpected artifact name: {name!r}")
        digest = require_digest(record["archive_digest"], f"archive digest {name}")
        archive_hash = require_sha256(record["archive_sha256"], f"archive SHA-256 {name}")
        require(digest == f"sha256:{archive_hash}", f"archive digest/hash mismatch: {name}")
        require_exact_int(record["size_in_bytes"], f"artifact size: {name}", 1, MAX_ARCHIVE_FILE_BYTES)
        require(
            record["inventory"] == ARTIFACT_INVENTORIES[expected_names[name]],
            f"artifact inventory differs: {name}",
        )
        require(artifact_id not in ids and name not in names, f"duplicate artifact record: {name}")
        ids.add(artifact_id)
        names.add(name)
        result.append(record)
    require(names == set(expected_names), f"exact artifact-name set differs for {phase}")
    release_name = f"release-set-{phase}-{run_id}-{run_attempt}"
    release_record = next(item for item in result if item["name"] == release_name)
    require(
        release_record["artifact_id"] == release_artifact_id,
        f"release-set artifact ID differs for {phase}",
    )
    return sorted(result, key=lambda item: item["name"])


def build_plan(
    normalized_input: dict[str, Any],
    artifact_evidence: dict[str, Any],
    release_root: Path,
    manifest: dict[str, Any],
    contract: dict[str, Any],
    control_sha: str,
    workflow_run_id: str,
    workflow_run_attempt: int,
    contract_path: Path,
    manifest_path: Path,
    schema_path: Path,
    verifier_path: Path,
) -> dict[str, Any]:
    require_run_id(workflow_run_id, "rollout workflow run ID")
    require_attempt(workflow_run_attempt, "rollout workflow run attempt")
    artifact_evidence = exact_keys(artifact_evidence, {"schema_version", "phases"}, "artifact evidence")
    require_exact_int(artifact_evidence["schema_version"], "artifact evidence schema version", 1, 1)
    evidence_phases = artifact_evidence["phases"]
    require(type(evidence_phases) is list and len(evidence_phases) == 4, "artifact evidence phase count differs")
    plan_phases = []
    for ordinal, phase in enumerate(PHASES):
        phase_input = normalized_input["phases"][ordinal]
        require(phase_input["phase"] == phase, "normalized input order differs")
        evidence = exact_keys(evidence_phases[ordinal], {"phase", "artifacts"}, f"{phase} artifact evidence")
        require(evidence["phase"] == phase, "artifact evidence order differs")
        records = validate_artifact_records(
            evidence["artifacts"],
            phase,
            phase_input["image_run_id"],
            phase_input["image_run_attempt"],
            phase_input["release_set_artifact_id"],
        )
        phase_dir = release_root / phase
        real_directory(phase_dir, f"{phase} release-set directory")
        release_set = strict_release_set(
            phase_dir / "release-set.json",
            phase_dir / "release-set.sha256",
            manifest,
            phase_input,
            control_sha,
            sha256_file(manifest_path),
        )
        web = next(image for image in release_set["images"] if image["component"] == "web")
        migration = {
            "component": "web",
            "image": web["image"],
            "digest": web["digest"],
            "subject": f"{web['image']}@{web['digest']}",
            "binding": "same-image-digest",
            "entrypoint": MIGRATION["entrypoint"],
            "arguments": MIGRATION["arguments"],
        }
        plan_phases.append(
            {
                "phase": phase,
                "ordinal": ordinal,
                "source": release_set["source"],
                "validation": release_set["validation"],
                "image_build": {
                    "run_id": phase_input["image_run_id"],
                    "run_attempt": phase_input["image_run_attempt"],
                    "release_set_artifact_id": phase_input["release_set_artifact_id"],
                    "release_set_sha256": phase_input["release_set_sha256"],
                },
                "input_artifacts": records,
                "images": release_set["images"],
                "migration": migration,
                "rollback": ROLLBACK[phase],
            }
        )
    plan = {
        "schema_version": 1,
        "authority": "digest-only",
        "repository": contract["repository"],
        "control": {
            "workflow_sha": control_sha,
            "workflow_path": contract["workflows"]["rollout"],
            "run_id": workflow_run_id,
            "run_attempt": workflow_run_attempt,
            "runner_environment": "github-hosted",
            "contract_sha256": sha256_file(contract_path),
            "source_manifest_sha256": sha256_file(manifest_path),
            "plan_schema_sha256": sha256_file(schema_path),
            "verifier_sha256": sha256_file(verifier_path),
        },
        "activation_order": PHASES,
        "baseline_forbidden_after_activation": "backend",
        "phases": plan_phases,
    }
    validate_plan(
        plan,
        release_root,
        manifest,
        contract,
        contract_path,
        manifest_path,
        schema_path,
        verifier_path,
    )
    return plan


def validate_plan(
    plan: Any,
    release_root: Path,
    manifest: dict[str, Any],
    contract: dict[str, Any],
    contract_path: Path,
    manifest_path: Path,
    schema_path: Path,
    verifier_path: Path,
) -> dict[str, Any]:
    schema = validate_plan_schema(load_json(schema_path, "rollout plan schema"))
    validate_json_schema(plan, schema)
    plan = exact_keys(
        plan,
        {
            "schema_version",
            "authority",
            "repository",
            "control",
            "activation_order",
            "baseline_forbidden_after_activation",
            "phases",
        },
        "rollout plan",
    )
    require_exact_int(plan["schema_version"], "plan schema version", 1, 1)
    require(plan["authority"] == "digest-only", "plan authority must be digest-only")
    require(plan["repository"] == contract["repository"], "plan repository differs")
    require(plan["activation_order"] == PHASES, "activation order differs")
    require(plan["baseline_forbidden_after_activation"] == "backend", "baseline floor differs")
    control = exact_keys(
        plan["control"],
        {
            "workflow_sha",
            "workflow_path",
            "run_id",
            "run_attempt",
            "runner_environment",
            "contract_sha256",
            "source_manifest_sha256",
            "plan_schema_sha256",
            "verifier_sha256",
        },
        "plan control",
    )
    require_sha1(control["workflow_sha"], "plan workflow SHA")
    require(control["workflow_path"] == contract["workflows"]["rollout"], "plan workflow path differs")
    require_run_id(control["run_id"], "plan run ID")
    require_attempt(control["run_attempt"], "plan run attempt")
    require(control["runner_environment"] == "github-hosted", "plan runner is not GitHub-hosted")
    require(control["contract_sha256"] == sha256_file(contract_path), "contract hash differs")
    require(control["source_manifest_sha256"] == sha256_file(manifest_path), "manifest hash differs")
    require(control["plan_schema_sha256"] == sha256_file(schema_path), "schema hash differs")
    require(control["verifier_sha256"] == sha256_file(verifier_path), "verifier hash differs")

    phases = plan["phases"]
    require(type(phases) is list and len(phases) == 4, "plan phase count differs")
    all_artifact_ids: set[str] = set()
    all_image_run_ids: set[str] = set()
    all_validation_run_ids: set[str] = set()
    for ordinal, phase in enumerate(PHASES):
        item = exact_keys(
            phases[ordinal],
            {
                "phase",
                "ordinal",
                "source",
                "validation",
                "image_build",
                "input_artifacts",
                "images",
                "migration",
                "rollback",
            },
            f"plan phase {phase}",
        )
        require_exact_int(item["ordinal"], f"{phase} ordinal", ordinal, ordinal)
        require(item["phase"] == phase, "plan phase order differs")
        image_build = exact_keys(
            item["image_build"],
            {"run_id", "run_attempt", "release_set_artifact_id", "release_set_sha256"},
            f"{phase} image build",
        )
        run_id = require_run_id(image_build["run_id"], f"{phase} image run ID")
        run_attempt = require_attempt(image_build["run_attempt"], f"{phase} image run attempt")
        release_artifact_id = require_run_id(
            image_build["release_set_artifact_id"], f"{phase} release artifact ID"
        )
        require_sha256(image_build["release_set_sha256"], f"{phase} release hash")
        require(run_id not in all_image_run_ids, "plan image run IDs are not unique")
        all_image_run_ids.add(run_id)
        records = validate_artifact_records(
            item["input_artifacts"], phase, run_id, run_attempt, release_artifact_id
        )
        for record in records:
            require(record["artifact_id"] not in all_artifact_ids, "artifact IDs repeat across phases")
            all_artifact_ids.add(record["artifact_id"])

        phase_input = {
            "phase": phase,
            "image_run_id": run_id,
            "image_run_attempt": run_attempt,
            "release_set_artifact_id": release_artifact_id,
            "release_set_sha256": image_build["release_set_sha256"],
        }
        release_set = strict_release_set(
            release_root / phase / "release-set.json",
            release_root / phase / "release-set.sha256",
            manifest,
            phase_input,
            control["workflow_sha"],
            sha256_file(manifest_path),
        )
        require(item["source"] == release_set["source"], f"plan source differs for {phase}")
        require(item["validation"] == release_set["validation"], f"plan validation differs for {phase}")
        validation_run_id = require_run_id(
            release_set["validation"]["run_id"], f"{phase} validation run ID"
        )
        require(validation_run_id not in all_validation_run_ids, "validation run IDs repeat across phases")
        all_validation_run_ids.add(validation_run_id)
        require(item["images"] == release_set["images"], f"plan images differ for {phase}")
        require(item["rollback"] == ROLLBACK[phase], f"rollback floor differs for {phase}")
        web = next(image for image in item["images"] if image["component"] == "web")
        expected_migration = {
            "component": "web",
            "image": web["image"],
            "digest": web["digest"],
            "subject": f"{web['image']}@{web['digest']}",
            "binding": "same-image-digest",
            "entrypoint": MIGRATION["entrypoint"],
            "arguments": MIGRATION["arguments"],
        }
        require(item["migration"] == expected_migration, f"migration/web digest binding differs for {phase}")
    return plan


def capsule_paths(contract: dict[str, Any]) -> list[str]:
    paths = list(contract["capsule"]["root_files"])
    for phase in PHASES:
        paths.extend(f"phases/{phase}/{name}" for name in contract["capsule"]["phase_files"])
    require(len(paths) == 20 and len(set(paths)) == 20, "capsule inventory must be exactly 20 files")
    return sorted(paths)


def validate_capsule(
    root: Path,
    contract_path: Path,
    manifest_path: Path,
    schema_path: Path,
    verifier_path: Path,
) -> dict[str, Any]:
    real_directory(root, "capsule")
    contract = validate_contract(load_json(contract_path, "rollout contract"))
    manifest = validate_manifest(load_json(manifest_path, "source manifest"))
    expected = capsule_paths(contract)
    actual: list[str] = []
    total_size = 0
    for candidate in root.rglob("*"):
        relative = candidate.relative_to(root).as_posix()
        mode = candidate.lstat().st_mode
        if stat.S_ISDIR(mode):
            continue
        require(stat.S_ISREG(mode), f"capsule contains a symlink or special entry: {relative}")
        file_size = candidate.stat().st_size
        require(0 < file_size <= MAX_ARCHIVE_FILE_BYTES, f"capsule file size is invalid: {relative}")
        total_size += file_size
        require(total_size <= MAX_ARCHIVE_UNCOMPRESSED_BYTES, "capsule exceeds aggregate byte budget")
        actual.append(relative)
    require(sorted(actual) == expected, f"capsule inventory differs: {sorted(actual)}")

    plan_path = root / "rollout-plan.json"
    plan_hash = sha256_file(plan_path)
    require(
        (root / "rollout-plan.sha256").read_bytes() == (plan_hash + "\n").encode("ascii"),
        "rollout-plan hash file differs",
    )
    plan = load_json(plan_path, "rollout plan")
    require(plan_path.read_bytes() == canonical_json_bytes(plan), "rollout-plan.json is not canonical")
    return validate_plan(
        plan,
        root / "phases",
        manifest,
        contract,
        contract_path,
        manifest_path,
        schema_path,
        verifier_path,
    )


def command_normalize_input(args: argparse.Namespace) -> None:
    result = normalize_input(args.raw, args.control_sha)
    dump_canonical(result, args.output)


def command_validate_contract(args: argparse.Namespace) -> None:
    validate_contract(load_json(args.contract, "rollout contract"))
    validate_manifest(load_json(args.manifest, "source manifest"))
    validate_plan_schema(load_json(args.schema, "rollout plan schema"))


def command_inspect_archive(args: argparse.Namespace) -> None:
    contract = validate_contract(load_json(args.contract, "rollout contract"))
    inventory = inspect_archive(args.archive, args.kind, contract, args.output)
    print(json.dumps(inventory, separators=(",", ":")))


def command_inspect_capsule_archive(args: argparse.Namespace) -> None:
    contract = validate_contract(load_json(args.contract, "rollout contract"))
    inventory = inspect_capsule_archive(args.archive, contract, args.output)
    print(json.dumps(inventory, separators=(",", ":")))


def command_build_plan(args: argparse.Namespace) -> None:
    contract = validate_contract(load_json(args.contract, "rollout contract"))
    manifest = validate_manifest(load_json(args.manifest, "source manifest"))
    normalized = load_json(args.normalized_input, "normalized phase input")
    normalized = normalize_input(json.dumps(normalized), args.control_sha)
    artifacts = load_json(args.artifact_evidence, "artifact evidence")
    plan = build_plan(
        normalized,
        artifacts,
        args.release_root,
        manifest,
        contract,
        args.control_sha,
        args.workflow_run_id,
        args.workflow_run_attempt,
        args.contract,
        args.manifest,
        args.schema,
        args.verifier,
    )
    dump_canonical(plan, args.output)


def command_validate_capsule(args: argparse.Namespace) -> None:
    validate_capsule(args.root, args.contract, args.manifest, args.schema, args.verifier)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subparsers = result.add_subparsers(dest="command", required=True)

    validate_contract_parser = subparsers.add_parser("validate-contract")
    validate_contract_parser.add_argument("--contract", type=Path, required=True)
    validate_contract_parser.add_argument("--manifest", type=Path, required=True)
    validate_contract_parser.add_argument("--schema", type=Path, required=True)
    validate_contract_parser.set_defaults(handler=command_validate_contract)

    input_parser = subparsers.add_parser("normalize-input")
    input_parser.add_argument("--raw", required=True)
    input_parser.add_argument("--control-sha", required=True)
    input_parser.add_argument("--output", type=Path, required=True)
    input_parser.set_defaults(handler=command_normalize_input)

    archive_parser = subparsers.add_parser("inspect-archive")
    archive_parser.add_argument("--archive", type=Path, required=True)
    archive_parser.add_argument("--kind", required=True)
    archive_parser.add_argument("--contract", type=Path, required=True)
    archive_parser.add_argument("--output", type=Path)
    archive_parser.set_defaults(handler=command_inspect_archive)

    capsule_archive_parser = subparsers.add_parser("inspect-capsule-archive")
    capsule_archive_parser.add_argument("--archive", type=Path, required=True)
    capsule_archive_parser.add_argument("--contract", type=Path, required=True)
    capsule_archive_parser.add_argument("--output", type=Path)
    capsule_archive_parser.set_defaults(handler=command_inspect_capsule_archive)

    plan_parser = subparsers.add_parser("build-plan")
    plan_parser.add_argument("--normalized-input", type=Path, required=True)
    plan_parser.add_argument("--artifact-evidence", type=Path, required=True)
    plan_parser.add_argument("--release-root", type=Path, required=True)
    plan_parser.add_argument("--manifest", type=Path, required=True)
    plan_parser.add_argument("--contract", type=Path, required=True)
    plan_parser.add_argument("--schema", type=Path, required=True)
    plan_parser.add_argument("--verifier", type=Path, required=True)
    plan_parser.add_argument("--control-sha", required=True)
    plan_parser.add_argument("--workflow-run-id", required=True)
    plan_parser.add_argument("--workflow-run-attempt", type=int, required=True)
    plan_parser.add_argument("--output", type=Path, required=True)
    plan_parser.set_defaults(handler=command_build_plan)

    capsule_parser = subparsers.add_parser("validate-capsule")
    capsule_parser.add_argument("--root", type=Path, required=True)
    capsule_parser.add_argument("--manifest", type=Path, required=True)
    capsule_parser.add_argument("--contract", type=Path, required=True)
    capsule_parser.add_argument("--schema", type=Path, required=True)
    capsule_parser.add_argument("--verifier", type=Path, required=True)
    capsule_parser.set_defaults(handler=command_validate_capsule)
    return result


def main() -> int:
    args = parser().parse_args()
    try:
        args.handler(args)
    except (EvidenceError, OSError, zipfile.BadZipFile) as exc:
        print(f"rollout evidence rejected: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
