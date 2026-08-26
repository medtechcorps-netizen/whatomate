#!/usr/bin/env python3
"""Build and verify a sanitized, observation-only production rollout plan.

The token-bearing command in this module performs four exact DigitalOcean GET
requests (two identical snapshots). It has no provider mutation primitive and
never serializes a live, deployment, or candidate app spec.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import os
import re
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Mapping, NoReturn, Sequence


PHASES = ["baseline", "bridge", "backend", "ui"]
COMPONENT_COLLECTIONS = ("services", "jobs")
EMPTY_COMPONENT_COLLECTIONS = ("workers", "static_sites", "functions")
SOURCE_FIELDS = ("git", "dockerfile_path", "image")
SOURCE_SELECTORS = ("git", "github", "gitlab", "bitbucket", "image")
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
UUID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
RUN_ID_RE = re.compile(r"^[1-9][0-9]{0,14}$")
TIMESTAMP_RE = re.compile(
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z$"
)
MAX_JSON_BYTES = 8 * 1024 * 1024
MAX_PLAN_BYTES = 128 * 1024
EXPECTED_INPUT_KEYS = {
    "control_sha",
    "rollout_run_id",
    "rollout_run_attempt",
    "capsule_artifact_id",
    "capsule_artifact_digest",
    "rollout_plan_sha256",
}
PRODUCTION_APP_NAME = "rereply"
PRODUCTION_APP_ID_SHA256 = (
    "c8c3b97aa3face41a6c2d1357807fa979e8a9bf2db25fff3f31b98dfa2019a31"
)
PRODUCTION_DEFAULT_INGRESS_SHA256 = (
    "05ab4f90194ad37c6926138e9aafbd49c73aa75d08da92b0b1309bfce207cfa8"
)
BOOTSTRAP_UPDATED_AT_SHA256 = (
    "c849930716929c0f80937eb213f3505c104c4eca913fc03934b8ea6f9a73f3df"
)
BOOTSTRAP_DEPLOYMENT_ID_SHA256 = (
    "439ecb65c0e711036d39b26a4d38b91f555dad32325dbdd86544230743d3cb0f"
)
BOOTSTRAP_SOURCE_SHA = "974bb998f6d4c94ce750a92bf23f4550f8e45a2f"
BOOTSTRAP_CANONICAL_SPEC_SHA256 = (
    "a93a507a5affd82b4e00636812e4c444d31c9027bc31c19bd075f2e4d580b07e"
)
BOOTSTRAP_ENVIRONMENT_SHA256 = (
    "3b675a9ca2279c465a102e8256cbf8242a46c456e6898d593e9cde3d0e2361c7"
)
BOOTSTRAP_NON_SOURCE_SHA256 = (
    "4f31183fb7f305a2a36422fdf722f90bed1eadc69fd90026928f522edf9a419b"
)
PRODUCTION_VPC_ID_SHA256 = (
    "aaaf98cef6beb658509d644dc8c56b559a38f79344739cd0edd1442070ec207e"
)
PRODUCTION_DATABASE_INVENTORY = {
    (
        "PG",
        "17",
        True,
        "5a08acdc0eed3e363e1aa4e0bba55a523bff63fec1680f3f63b1f46e834289c2",
        "28248f27ae9c2545b42173bd7f06fffce87e1b22db6d51d1721fb3318a871472",
    ),
    (
        "VALKEY",
        "8",
        True,
        "92acd7905c06aeb9db2bb0b6a71b739bfc6032ca8a2a5fb7473e8afd4a1c87e3",
        "affc1fc72bb50fa5467e5ba5bd07f5b2bfc7c448bed946c17bbb4078aee1db97",
    ),
}


class PlanError(RuntimeError):
    """A fail-closed release-plan validation error."""


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(
        self,
        req: urllib.request.Request,
        fp: Any,
        code: int,
        msg: str,
        headers: Any,
        newurl: str,
    ) -> None:
        raise PlanError("provider redirect rejected")


def fail(message: str) -> NoReturn:
    raise PlanError(message)


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            fail("duplicate JSON key rejected")
        result[key] = value
    return result


def reject_float(raw: str) -> NoReturn:
    fail("floating-point JSON number rejected")


def reject_nonfinite(raw: str) -> NoReturn:
    fail("non-finite JSON number rejected")


def loads_strict(raw: str) -> Any:
    try:
        return json.loads(
            raw,
            object_pairs_hook=reject_duplicate_keys,
            parse_float=reject_float,
            parse_constant=reject_nonfinite,
        )
    except (json.JSONDecodeError, UnicodeError) as exc:
        raise PlanError("malformed JSON rejected") from exc


def read_regular_file(path: Path, label: str, maximum_bytes: int) -> bytes:
    if maximum_bytes <= 0:
        fail(f"{label} byte budget is invalid")
    if path.is_symlink():
        fail(f"{label} must be a regular non-symlink file")
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0)
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        try:
            metadata = os.fstat(descriptor)
            if not stat.S_ISREG(metadata.st_mode) or not 0 < metadata.st_size <= maximum_bytes:
                fail(f"{label} has an invalid size or type")
            with os.fdopen(descriptor, "rb") as handle:
                descriptor = -1
                raw = handle.read(maximum_bytes + 1)
        finally:
            if descriptor >= 0:
                os.close(descriptor)
    except PlanError:
        raise
    except OSError as exc:
        raise PlanError(f"{label} could not be read securely") from exc
    if not raw or len(raw) > maximum_bytes:
        fail(f"{label} has an invalid size")
    return raw


def load_json_document(
    path: Path, label: str, *, canonical: bool = False
) -> tuple[Any, bytes]:
    try:
        raw = read_regular_file(path, label, MAX_JSON_BYTES)
        text = raw.decode("utf-8")
    except UnicodeError as exc:
        raise PlanError(f"{label} could not be read") from exc
    value = loads_strict(text)
    if canonical and raw != canonical_file_bytes(value):
        fail(f"{label} is not canonical JSON")
    return value, raw


def load_json(path: Path, label: str, *, canonical: bool = False) -> Any:
    value, _ = load_json_document(path, label, canonical=canonical)
    return value


def load_json_and_hash(
    path: Path, label: str, *, canonical: bool = False
) -> tuple[Any, str]:
    value, raw = load_json_document(path, label, canonical=canonical)
    return value, sha256_bytes(raw)


def canonical_payload_bytes(value: Any) -> bytes:
    """Canonical UTF-8 payload used for provider-state fingerprints.

    Provider fingerprints intentionally exclude a presentation newline. Release
    artifacts use :func:`canonical_file_bytes` instead and are hashed as exact
    files, including their required trailing newline.
    """

    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise PlanError("value cannot be serialized canonically") from exc


def canonical_file_bytes(value: Any) -> bytes:
    """Canonical JSON release artifact with one trailing newline."""

    try:
        encoded = json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=True,
            separators=(",", ":"),
            sort_keys=True,
        )
    except (TypeError, ValueError) as exc:
        raise PlanError("value cannot be serialized canonically") from exc
    return (encoded + "\n").encode("utf-8")


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha256_value(value: Any) -> str:
    return sha256_bytes(canonical_payload_bytes(value))


def sha256_file(path: Path) -> str:
    return sha256_bytes(read_regular_file(path, "control file", MAX_JSON_BYTES))


def write_secure_bytes(path: Path, raw: bytes, label: str) -> None:
    if path.exists() or path.is_symlink():
        fail(f"{label} output already exists")
    if path.parent.is_symlink() or not path.parent.is_dir():
        fail(f"{label} parent is not a secure directory")
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
            path.unlink(missing_ok=True)
        except OSError:
            pass
        raise
    mode = stat.S_IMODE(path.stat().st_mode)
    if os.name == "posix" and mode & 0o077:
        fail(f"{label} permissions are too broad")


def write_canonical(path: Path, value: Any) -> None:
    raw = canonical_file_bytes(value)
    if len(raw) > MAX_PLAN_BYTES:
        fail("sanitized plan exceeds its byte budget")
    write_secure_bytes(path, raw, "canonical JSON")


def write_plan_artifacts(directory: Path, plan: Mapping[str, Any]) -> tuple[Path, Path]:
    plan_path = directory / "production-plan.json"
    hash_path = directory / "production-plan.sha256"
    raw = canonical_file_bytes(plan)
    if len(raw) > MAX_PLAN_BYTES:
        fail("sanitized plan exceeds its byte budget")
    digest = sha256_bytes(raw)
    try:
        write_secure_bytes(plan_path, raw, "production plan")
        write_secure_bytes(
            hash_path,
            (digest + "\n").encode("ascii"),
            "production plan hash",
        )
    except BaseException:
        plan_path.unlink(missing_ok=True)
        hash_path.unlink(missing_ok=True)
        raise
    return plan_path, hash_path


def prepare_runner_output_directory(path: Path) -> Path:
    runner_temp_raw = os.environ.get("RUNNER_TEMP")
    if not runner_temp_raw:
        fail("RUNNER_TEMP is required for provider observation")
    runner_temp = Path(runner_temp_raw)
    if not runner_temp.is_absolute() or runner_temp.is_symlink() or not runner_temp.is_dir():
        fail("RUNNER_TEMP is not a secure absolute directory")
    try:
        runner_root = runner_temp.resolve(strict=True)
        requested_parent = path.parent.resolve(strict=True)
    except OSError as exc:
        raise PlanError("provider output directory parent is invalid") from exc
    if requested_parent != runner_root or path.name != "rereply-production-plan":
        fail("provider output directory is outside the fixed runner path")
    if path.exists() or path.is_symlink():
        fail("provider output directory already exists")
    path.mkdir(mode=0o700)
    resolved = path.resolve(strict=True)
    if resolved.parent != runner_root or resolved.name != "rereply-production-plan":
        fail("provider output directory escaped the runner path")
    if os.name == "posix" and stat.S_IMODE(path.stat().st_mode) != 0o700:
        fail("provider output directory permissions differ")
    return path


def exact_keys(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    if type(value) is not dict or set(value) != expected:
        fail(f"{label} keys differ")
    return value


def exact_string(
    value: Any,
    label: str,
    pattern: re.Pattern[str] | None = None,
    *,
    maximum: int = 512,
) -> str:
    if type(value) is not str or not value or len(value) > maximum:
        fail(f"{label} is invalid")
    if "\n" in value or "\r" in value or "\x00" in value:
        fail(f"{label} contains control characters")
    if pattern is not None and pattern.fullmatch(value) is None:
        fail(f"{label} format is invalid")
    return value


def exact_int(value: Any, label: str, minimum: int = 0, maximum: int = 2_147_483_647) -> int:
    if type(value) is not int or not minimum <= value <= maximum:
        fail(f"{label} is invalid")
    return value


def exact_bool(value: Any, label: str) -> bool:
    if type(value) is not bool:
        fail(f"{label} must be boolean")
    return value


def optional_object(value: Any, label: str) -> dict[str, Any] | None:
    if value is None:
        return None
    if type(value) is not dict:
        fail(f"{label} is malformed")
    return value


def require_sha1(value: Any, label: str) -> str:
    return exact_string(value, label, SHA1_RE, maximum=40)


def require_sha256(value: Any, label: str) -> str:
    return exact_string(value, label, SHA256_RE, maximum=64)


def require_digest(value: Any, label: str) -> str:
    return exact_string(value, label, DIGEST_RE, maximum=71)


def require_uuid(value: Any, label: str) -> str:
    return exact_string(value, label, UUID_RE, maximum=36)


def require_run_id(value: Any, label: str) -> str:
    return exact_string(value, label, RUN_ID_RE, maximum=20)


def require_timestamp(value: Any, label: str) -> str:
    raw = exact_string(value, label, TIMESTAMP_RE, maximum=40)
    try:
        parsed = dt.datetime.fromisoformat(raw[:-1] + "+00:00")
    except ValueError as exc:
        raise PlanError(f"{label} is invalid") from exc
    if parsed.tzinfo != dt.timezone.utc:
        fail(f"{label} is not UTC")
    return raw


def component_index(spec: Mapping[str, Any], collection: str) -> dict[str, dict[str, Any]]:
    raw = spec.get(collection, [])
    if type(raw) is not list:
        fail(f"spec {collection} must be an array")
    result: dict[str, dict[str, Any]] = {}
    for item in raw:
        if type(item) is not dict:
            fail(f"spec {collection} entry is malformed")
        name = exact_string(item.get("name"), f"spec {collection} component name")
        if name in result:
            fail(f"duplicate spec component in {collection}")
        result[name] = item
    return result


def normalized_env_type(item: Mapping[str, Any]) -> str:
    value = item.get("type", "GENERAL")
    if value not in {"GENERAL", "SECRET"}:
        fail("environment type is outside the reviewed contract")
    return value


def environment_value_inventory(spec: Mapping[str, Any]) -> tuple[str, int, int]:
    records: list[list[str]] = []

    def collect(collection: str, component: str, raw: Any) -> None:
        if raw is None:
            raw = []
        if type(raw) is not list:
            fail("environment list is malformed")
        seen: set[str] = set()
        for item in raw:
            if type(item) is not dict:
                fail("environment entry is malformed")
            key = exact_string(item.get("key"), "environment key")
            if key in seen:
                fail("duplicate environment key")
            seen.add(key)
            scope = item.get("scope", "RUN_TIME")
            if scope != "RUN_TIME":
                fail("environment scope is outside the reviewed contract")
            value = item.get("value")
            if type(value) is not str:
                fail("environment value is missing")
            records.append(
                [
                    collection,
                    component,
                    key,
                    scope,
                    normalized_env_type(item),
                    sha256_bytes(value.encode("utf-8")),
                ]
            )

    collect("app", "app", spec.get("envs", []))
    for collection in (*COMPONENT_COLLECTIONS, *EMPTY_COMPONENT_COLLECTIONS):
        for name, component in component_index(spec, collection).items():
            collect(collection, name, component.get("envs", []))
    secret_count = sum(1 for item in records if item[4] == "SECRET")
    return sha256_value(sorted(records)), len(records), secret_count


def environment_value_fingerprint(spec: Mapping[str, Any]) -> str:
    return environment_value_inventory(spec)[0]


def strip_component_sources(spec: Mapping[str, Any], contract: Mapping[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(spec)
    by_collection: dict[str, set[str]] = {name: set() for name in COMPONENT_COLLECTIONS}
    for component in contract["components"]:
        by_collection[component["collection"]].add(component["app_name"])
    for collection in COMPONENT_COLLECTIONS:
        indexed = component_index(result, collection)
        if set(indexed) != by_collection[collection]:
            fail(f"spec {collection} component set differs")
        for component in indexed.values():
            for field in SOURCE_FIELDS:
                component.pop(field, None)
    return result


def non_source_fingerprint(spec: Mapping[str, Any], contract: Mapping[str, Any]) -> str:
    return sha256_value(strip_component_sources(spec, contract))


def validate_contract(value: Any) -> dict[str, Any]:
    contract = exact_keys(
        value,
        {
            "schema_version",
            "repository",
            "authority",
            "workflow",
            "provider",
            "bootstrap_state",
            "components",
            "logical_source_transform",
            "expected_topology",
            "plan",
            "artifact",
            "sanitization",
            "security",
        },
        "production app contract",
    )
    exact_int(contract["schema_version"], "contract schema version", 1, 1)
    if contract["repository"] != "medtechcorps-netizen/whatomate":
        fail("contract repository differs")
    if contract["authority"] != "observation-only":
        fail("contract authority differs")

    workflow = exact_keys(
        contract["workflow"],
        {"path", "environment", "concurrency_group"},
        "contract workflow",
    )
    if workflow != {
        "path": ".github/workflows/plan-production-rollout.yml",
        "environment": "rereply-production-plan",
        "concurrency_group": "rereply-production",
    }:
        fail("contract workflow identity differs")

    provider = exact_keys(
        contract["provider"],
        {
            "name",
            "api_origin",
            "app_id_sha256",
            "app_name",
            "default_ingress_sha256",
            "allowed_http_methods",
            "allowed_path_templates",
            "required_token_scopes",
        },
        "contract provider",
    )
    if provider["name"] != "digitalocean-app-platform":
        fail("contract provider differs")
    if provider["api_origin"] != "https://api.digitalocean.com":
        fail("provider origin differs")
    if require_sha256(provider["app_id_sha256"], "provider app ID hash") != (
        PRODUCTION_APP_ID_SHA256
    ):
        fail("provider app ID hash differs")
    if exact_string(provider["app_name"], "provider app name") != PRODUCTION_APP_NAME:
        fail("provider app name differs")
    if require_sha256(
        provider["default_ingress_sha256"], "provider default ingress hash"
    ) != PRODUCTION_DEFAULT_INGRESS_SHA256:
        fail("provider default ingress hash differs")
    if provider["allowed_http_methods"] != ["GET"]:
        fail("provider method allowlist is not GET-only")
    if provider["allowed_path_templates"] != [
        "/v2/apps/{app_id}",
        "/v2/apps/{app_id}/deployments/{active_deployment_id}",
    ]:
        fail("provider path-template allowlist differs")
    if provider["required_token_scopes"] != [
        "app:read",
        "regions:read",
        "sizes:read",
        "actions:read",
    ]:
        fail("provider read-only token scopes differ")

    bootstrap = exact_keys(
        contract["bootstrap_state"],
        {
            "app_updated_at_sha256",
            "active_deployment_id_sha256",
            "canonical_spec_sha256",
            "environment_values_sha256",
            "non_source_projection_sha256",
            "source_mode",
            "source_sha",
            "next_phase",
        },
        "bootstrap state",
    )
    if require_sha256(
        bootstrap["app_updated_at_sha256"], "bootstrap app timestamp hash"
    ) != BOOTSTRAP_UPDATED_AT_SHA256:
        fail("bootstrap app timestamp hash differs")
    if require_sha256(
        bootstrap["active_deployment_id_sha256"],
        "bootstrap active deployment ID hash",
    ) != BOOTSTRAP_DEPLOYMENT_ID_SHA256:
        fail("bootstrap active deployment hash differs")
    for key in (
        "canonical_spec_sha256",
        "environment_values_sha256",
        "non_source_projection_sha256",
    ):
        require_sha256(bootstrap[key], f"bootstrap {key}")
    if bootstrap["canonical_spec_sha256"] != BOOTSTRAP_CANONICAL_SPEC_SHA256:
        fail("bootstrap raw spec hash differs")
    if bootstrap["environment_values_sha256"] != BOOTSTRAP_ENVIRONMENT_SHA256:
        fail("bootstrap environment hash differs")
    if bootstrap["non_source_projection_sha256"] != BOOTSTRAP_NON_SOURCE_SHA256:
        fail("bootstrap non-source hash differs")
    if bootstrap["source_mode"] != "legacy-git":
        fail("bootstrap source mode differs")
    if require_sha1(bootstrap["source_sha"], "bootstrap source SHA") != BOOTSTRAP_SOURCE_SHA:
        fail("bootstrap source SHA differs")
    if bootstrap["next_phase"] != "baseline":
        fail("bootstrap next phase differs")

    components = contract["components"]
    if type(components) is not list or len(components) != 4:
        fail("contract must contain four components")
    expected_order = [
        ("services", "omnitech-web", "web", "docker/Dockerfile", 8080, "/ready"),
        (
            "services",
            "meta-relay",
            "meta-relay",
            "docker/meta-relay.Dockerfile",
            8081,
            "/readyz",
        ),
        (
            "services",
            "gmail-relay",
            "gmail-relay",
            "docker/gmail-relay.Dockerfile",
            8082,
            "/readyz",
        ),
        ("jobs", "rereply-rls-migrate", "web", "docker/Dockerfile", None, None),
    ]
    for index, expected in enumerate(expected_order):
        component = components[index]
        common = {
            "collection",
            "app_name",
            "release_component",
            "image_repository",
            "legacy_repo_clone_url",
            "legacy_branch",
            "legacy_dockerfile_path",
        }
        if expected[0] == "services":
            component = exact_keys(
                component,
                common | {"http_port", "health_path"},
                f"contract component {expected[1]}",
            )
            if exact_int(
                component["http_port"], f"{expected[1]} HTTP port", 1, 65535
            ) != expected[4]:
                fail(f"{expected[1]} HTTP port differs")
            if exact_string(
                component["health_path"], f"{expected[1]} health path"
            ) != expected[5]:
                fail(f"{expected[1]} health path differs")
        else:
            component = exact_keys(
                component,
                common | {"kind", "run_command"},
                f"contract component {expected[1]}",
            )
            if component["kind"] != "PRE_DEPLOY":
                fail("migration job kind differs")
            if component["run_command"] != "./rereply rls-migrate -config config.toml":
                fail("migration run command differs")
        if (
            component["collection"],
            component["app_name"],
            component["release_component"],
        ) != expected[:3]:
            fail("contract component order or identity differs")
        if component["legacy_repo_clone_url"] != (
            "https://github.com/medtechcorps-netizen/whatomate.git"
        ):
            fail("legacy component repository differs")
        if component["legacy_branch"] != "main":
            fail("legacy component branch differs")
        if component["legacy_dockerfile_path"] != expected[3]:
            fail(f"legacy Dockerfile differs for {expected[1]}")
        expected_repository = (
            f"medtechcorps-netizen/rereply-release-{component['release_component']}"
        )
        if component["image_repository"] != expected_repository:
            fail("release image repository differs")

    transform = exact_keys(
        contract["logical_source_transform"],
        {
            "registry_type",
            "registry",
            "remove_fields",
            "add_field",
            "forbidden_image_fields",
            "migration_binding",
        },
        "logical source transform",
    )
    if transform != {
        "registry_type": "GHCR",
        "registry": "ghcr.io",
        "remove_fields": ["git", "dockerfile_path"],
        "add_field": "image",
        "forbidden_image_fields": ["tag", "deploy_on_push", "registry_credentials"],
        "migration_binding": "same-web-image-digest",
    }:
        fail("logical source transform differs")

    topology = exact_keys(
        contract["expected_topology"],
        {"region", "vpc_id_sha256", "ingress", "domains", "databases"},
        "expected topology",
    )
    if topology["region"] != "sgp":
        fail("production region differs")
    if (
        require_sha256(topology["vpc_id_sha256"], "VPC ID hash")
        != PRODUCTION_VPC_ID_SHA256
    ):
        fail("VPC contract differs")
    expected_ingress = [
        {
            "kind": "component",
            "path_prefix": "/gmail-relay",
            "component": "gmail-relay",
            "preserve_path_prefix": False,
        },
        {
            "kind": "component",
            "path_prefix": "/meta-relay",
            "component": "meta-relay",
            "preserve_path_prefix": False,
        },
        {
            "kind": "component",
            "path_prefix": "/",
            "component": "omnitech-web",
            "preserve_path_prefix": False,
        },
        {
            "kind": "redirect",
            "path_prefix": "/",
            "match_authority": "rereply.app",
            "redirect_authority": "app.rereply.app",
            "scheme": "https",
            "redirect_code": 308,
        },
    ]
    if topology["ingress"] != expected_ingress:
        fail("ingress contract differs")
    if topology["domains"] != ["app.rereply.app", "rereply.app"]:
        fail("domain contract differs")
    databases = topology["databases"]
    if type(databases) is not list or len(databases) != 2:
        fail("database contract differs")
    for item in databases:
        item = exact_keys(
            item,
            {"engine", "version", "production", "name_sha256", "cluster_sha256"},
            "database contract entry",
        )
        if item["engine"] not in {"PG", "VALKEY"}:
            fail("database engine differs")
        exact_string(item["version"], "database version", maximum=16)
        if exact_bool(item["production"], "database production flag") is not True:
            fail("database must remain production")
        require_sha256(item["name_sha256"], "database name hash")
        require_sha256(item["cluster_sha256"], "database cluster hash")
    if {
        (
            item["engine"],
            item["version"],
            item["production"],
            item["name_sha256"],
            item["cluster_sha256"],
        )
        for item in databases
    } != PRODUCTION_DATABASE_INVENTORY:
        fail("database contract inventory differs")

    plan = exact_keys(
        contract["plan"],
        {"maximum_age_seconds", "phase_order"},
        "plan policy",
    )
    exact_int(plan["maximum_age_seconds"], "plan maximum age", 60, 1800)
    if plan["phase_order"] != PHASES:
        fail("plan phase order differs")

    artifact = exact_keys(
        contract["artifact"], {"files", "maximum_bytes"}, "artifact policy"
    )
    if artifact["files"] != ["production-plan.json", "production-plan.sha256"]:
        fail("plan artifact inventory differs")
    exact_int(artifact["maximum_bytes"], "artifact maximum bytes", 1024, MAX_PLAN_BYTES)

    sanitization = exact_keys(
        contract["sanitization"],
        {"forbidden_keys", "forbidden_string_prefixes"},
        "sanitization policy",
    )
    if sanitization["forbidden_keys"] != [
        "envs",
        "value",
        "registry_credentials",
        "access_token",
        "authorization",
        "app_id",
        "active_deployment_id",
        "app_updated_at",
        "default_ingress",
        "http_paths_used",
        "spec",
    ]:
        fail("sanitization forbidden-key policy differs")
    if sanitization["forbidden_string_prefixes"] != [
        "EV[",
        "dop_v1_",
        "doo_v1_",
        "ghp_",
        "github_pat_",
        "-----BEGIN ",
    ]:
        fail("sanitization prefix policy differs")

    security = exact_keys(
        contract["security"],
        {
            "token_environment",
            "target_environment",
            "forbidden_ambient_environment",
        },
        "security policy",
    )
    if security["token_environment"] != "DO_PRODUCTION_READ_TOKEN":
        fail("read token environment differs")
    if security["target_environment"] != "DO_PRODUCTION_TARGET_JSON":
        fail("protected target environment differs")
    if security["forbidden_ambient_environment"] != [
        "DIGITALOCEAN_ACCESS_TOKEN",
        "DO_ACCESS_TOKEN",
        "DO_TOKEN",
        "GHCR_PRODUCTION_PULL_TOKEN",
    ]:
        fail("forbidden ambient environment policy differs")
    return contract


def normalize_target_descriptor(
    raw: str, contract: Mapping[str, Any]
) -> dict[str, str]:
    value = exact_keys(
        loads_strict(raw),
        {"app_id", "active_deployment_id", "app_updated_at", "default_ingress"},
        "protected production target descriptor",
    )
    app_id = require_uuid(value["app_id"], "protected app ID")
    active_deployment_id = require_uuid(
        value["active_deployment_id"], "protected active deployment ID"
    )
    app_updated_at = require_timestamp(
        value["app_updated_at"], "protected app timestamp"
    )
    default_ingress = exact_string(
        value["default_ingress"], "protected default ingress", maximum=512
    )
    parsed = urllib.parse.urlsplit(default_ingress)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.port not in (None, 443)
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
    ):
        fail("protected default ingress is not an exact HTTPS origin")

    expected_hashes = {
        "app_id": contract["provider"]["app_id_sha256"],
        "active_deployment_id": contract["bootstrap_state"][
            "active_deployment_id_sha256"
        ],
        "app_updated_at": contract["bootstrap_state"]["app_updated_at_sha256"],
        "default_ingress": contract["provider"]["default_ingress_sha256"],
    }
    normalized = {
        "app_id": app_id,
        "active_deployment_id": active_deployment_id,
        "app_updated_at": app_updated_at,
        "default_ingress": default_ingress,
    }
    for key, expected in expected_hashes.items():
        if sha256_bytes(normalized[key].encode("utf-8")) != expected:
            fail("protected production target identity differs")
    return normalized


def provider_paths(
    contract: Mapping[str, Any], target: Mapping[str, str]
) -> tuple[str, str]:
    templates = contract["provider"]["allowed_path_templates"]
    app_path = templates[0].replace("{app_id}", target["app_id"])
    deployment_path = (
        templates[1]
        .replace("{app_id}", target["app_id"])
        .replace("{active_deployment_id}", target["active_deployment_id"])
    )
    if "{" in app_path or "}" in app_path or "{" in deployment_path or "}" in deployment_path:
        fail("provider path template was not resolved exactly")
    return app_path, deployment_path


def normalize_input(raw: str, control_sha: str) -> dict[str, Any]:
    require_sha1(control_sha, "workflow control SHA")
    value = exact_keys(loads_strict(raw), EXPECTED_INPUT_KEYS, "plan input")
    if require_sha1(value["control_sha"], "input control SHA") != control_sha:
        fail("input control SHA differs from workflow SHA")
    result = {
        "control_sha": control_sha,
        "rollout_run_id": require_run_id(value["rollout_run_id"], "rollout run ID"),
        "rollout_run_attempt": exact_int(
            value["rollout_run_attempt"], "rollout run attempt", 1
        ),
        "capsule_artifact_id": require_run_id(
            value["capsule_artifact_id"], "capsule artifact ID"
        ),
        "capsule_artifact_digest": require_digest(
            value["capsule_artifact_digest"], "capsule artifact digest"
        ),
        "rollout_plan_sha256": require_sha256(
            value["rollout_plan_sha256"], "rollout plan SHA-256"
        ),
    }
    return result


def normalized_ingress(spec: Mapping[str, Any]) -> list[dict[str, Any]]:
    ingress = spec.get("ingress")
    if type(ingress) is not dict or type(ingress.get("rules")) is not list:
        fail("production ingress is malformed")
    result: list[dict[str, Any]] = []
    for rule in ingress["rules"]:
        if type(rule) is not dict or type(rule.get("match")) is not dict:
            fail("production ingress rule is malformed")
        match = rule["match"]
        path = match.get("path")
        if type(path) is not dict or set(path) != {"prefix"}:
            fail("production ingress must use exact prefix matches")
        prefix = exact_string(path["prefix"], "ingress path prefix")
        component = rule.get("component")
        redirect = rule.get("redirect")
        if (component is None) == (redirect is None):
            fail("ingress rule must have exactly one destination")
        if component is not None:
            if type(component) is not dict:
                fail("ingress component destination is malformed")
            preserve = component.get("preserve_path_prefix", False)
            exact_bool(preserve, "ingress preserve_path_prefix")
            result.append(
                {
                    "kind": "component",
                    "path_prefix": prefix,
                    "component": exact_string(component.get("name"), "ingress component"),
                    "preserve_path_prefix": preserve,
                }
            )
            continue

        if type(redirect) is not dict:
            fail("ingress redirect destination is malformed")
        authority_match = match.get("authority")
        if type(authority_match) is not dict or set(authority_match) != {"exact"}:
            fail("redirect authority match is malformed")
        result.append(
            {
                "kind": "redirect",
                "path_prefix": prefix,
                "match_authority": exact_string(
                    authority_match["exact"], "redirect match authority"
                ),
                "redirect_authority": exact_string(
                    redirect.get("authority"), "redirect authority"
                ),
                "scheme": redirect.get("scheme", "https"),
                "redirect_code": redirect.get("redirect_code", 302),
            }
        )
    return result


def validate_topology(spec: Mapping[str, Any], contract: Mapping[str, Any]) -> None:
    topology = contract["expected_topology"]
    if spec.get("name") != contract["provider"]["app_name"]:
        fail("production spec app name differs")
    if spec.get("region") != topology["region"]:
        fail("production region differs")
    for collection in EMPTY_COMPONENT_COLLECTIONS:
        if spec.get(collection, []) not in (None, []):
            fail(f"unexpected production {collection} component")

    vpc = spec.get("vpc")
    if type(vpc) is not dict or type(vpc.get("id")) is not str:
        fail("production VPC binding is malformed")
    if sha256_bytes(vpc["id"].encode("utf-8")) != topology["vpc_id_sha256"]:
        fail("production VPC binding differs")

    if normalized_ingress(spec) != topology["ingress"]:
        fail("production ingress differs")

    domains = spec.get("domains")
    if type(domains) is not list:
        fail("production domains are malformed")
    domain_names = []
    for domain in domains:
        if type(domain) is not dict:
            fail("production domain entry is malformed")
        domain_names.append(exact_string(domain.get("domain"), "production domain"))
    if sorted(domain_names) != topology["domains"] or len(set(domain_names)) != 2:
        fail("production domains differ")

    databases = spec.get("databases")
    if type(databases) is not list or len(databases) != 2:
        fail("production database bindings differ")
    observed_databases = []
    for database in databases:
        if type(database) is not dict:
            fail("production database entry is malformed")
        name = exact_string(database.get("name"), "database binding name")
        cluster = exact_string(database.get("cluster_name"), "database cluster name")
        observed_databases.append(
            {
                "engine": exact_string(database.get("engine"), "database engine"),
                "version": exact_string(database.get("version"), "database version"),
                "production": exact_bool(database.get("production"), "database production flag"),
                "name_sha256": sha256_bytes(name.encode("utf-8")),
                "cluster_sha256": sha256_bytes(cluster.encode("utf-8")),
            }
        )
    if sorted(observed_databases, key=lambda item: item["engine"]) != sorted(
        topology["databases"], key=lambda item: item["engine"]
    ):
        fail("production database bindings differ")


def validate_legacy_component_sources(
    spec: Mapping[str, Any], contract: Mapping[str, Any]
) -> None:
    expected_by_collection: dict[str, dict[str, Mapping[str, Any]]] = {
        collection: {} for collection in COMPONENT_COLLECTIONS
    }
    for item in contract["components"]:
        expected_by_collection[item["collection"]][item["app_name"]] = item
    for collection in COMPONENT_COLLECTIONS:
        observed = component_index(spec, collection)
        if set(observed) != set(expected_by_collection[collection]):
            fail(f"production {collection} component set differs")
        for name, component in observed.items():
            expected = expected_by_collection[collection][name]
            if set(component).intersection(SOURCE_SELECTORS) != {"git"}:
                fail(f"legacy source selector is not exact for {name}")
            git = component.get("git")
            if type(git) is not dict:
                fail(f"legacy source is missing for {name}")
            if set(git) not in (
                {"repo_clone_url", "branch"},
                {"repo_clone_url", "branch", "deploy_on_push"},
            ):
                fail(f"legacy git source envelope differs for {name}")
            if git.get("repo_clone_url") != expected["legacy_repo_clone_url"]:
                fail(f"legacy repository differs for {name}")
            if git.get("branch") != expected["legacy_branch"]:
                fail(f"legacy branch differs for {name}")
            if git.get("deploy_on_push", False) is not False:
                fail(f"legacy deploy-on-push is enabled for {name}")
            if component.get("dockerfile_path") != expected["legacy_dockerfile_path"]:
                fail(f"legacy Dockerfile differs for {name}")
            if "image" in component:
                fail(f"legacy component already contains an image source: {name}")
            if collection == "services":
                if component.get("http_port") != expected["http_port"]:
                    fail(f"HTTP port differs for {name}")
                health = component.get("health_check")
                if type(health) is not dict or health.get("http_path") != expected["health_path"]:
                    fail(f"health path differs for {name}")
            else:
                if component.get("kind") != expected["kind"]:
                    fail("migration job kind differs")
                if component.get("run_command") != expected["run_command"]:
                    fail("migration job run command differs")


def validate_deployment_sources(
    deployment: Mapping[str, Any], contract: Mapping[str, Any]
) -> None:
    source_sha = contract["bootstrap_state"]["source_sha"]
    expected_by_collection: dict[str, set[str]] = {
        collection: set() for collection in COMPONENT_COLLECTIONS
    }
    for item in contract["components"]:
        expected_by_collection[item["collection"]].add(item["app_name"])
    for collection in COMPONENT_COLLECTIONS:
        raw = deployment.get(collection, [])
        if type(raw) is not list:
            fail(f"deployment {collection} source list is malformed")
        observed: dict[str, str] = {}
        for item in raw:
            if type(item) is not dict:
                fail(f"deployment {collection} source entry is malformed")
            name = exact_string(item.get("name"), f"deployment {collection} name")
            if name in observed:
                fail("duplicate deployment component source")
            observed[name] = require_sha1(
                item.get("source_commit_hash"), f"deployment source SHA for {name}"
            )
        if set(observed) != expected_by_collection[collection]:
            fail(f"deployment {collection} source set differs")
        if set(observed.values()) != {source_sha}:
            fail(f"deployment {collection} source SHA differs")


def validate_rollout_plan(
    value: Any,
    contract: Mapping[str, Any],
    normalized_input: Mapping[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    if type(value) is not dict:
        fail("rollout plan is malformed")
    if value.get("schema_version") != 1 or value.get("authority") != "digest-only":
        fail("rollout plan authority differs")
    if value.get("repository") != contract["repository"]:
        fail("rollout plan repository differs")
    if value.get("activation_order") != PHASES:
        fail("rollout activation order differs")
    control = value.get("control")
    if type(control) is not dict:
        fail("rollout control is malformed")
    if control.get("workflow_sha") != normalized_input["control_sha"]:
        fail("rollout control SHA differs")
    if require_run_id(control.get("run_id"), "rollout plan run ID") != normalized_input[
        "rollout_run_id"
    ]:
        fail("rollout run ID differs")
    if exact_int(control.get("run_attempt"), "rollout plan run attempt", 1) != normalized_input[
        "rollout_run_attempt"
    ]:
        fail("rollout run attempt differs")
    phases = value.get("phases")
    if type(phases) is not list or len(phases) != 4:
        fail("rollout phases differ")
    if [item.get("phase") if type(item) is dict else None for item in phases] != PHASES:
        fail("rollout phase order differs")
    target_phase = contract["bootstrap_state"]["next_phase"]
    target = phases[PHASES.index(target_phase)]
    source = target.get("source")
    if type(source) is not dict or source.get("commit") != contract["bootstrap_state"]["source_sha"]:
        fail("baseline rollout source differs from live source")
    images = target.get("images")
    if type(images) is not list or len(images) != 3:
        fail("baseline rollout image set differs")
    expected_images = {
        item["release_component"]: item["image_repository"]
        for item in contract["components"]
    }
    observed_images: dict[str, dict[str, str]] = {}
    for image in images:
        if type(image) is not dict:
            fail("rollout image is malformed")
        component = exact_string(image.get("component"), "rollout image component")
        if component in observed_images:
            fail("duplicate rollout image component")
        digest = require_digest(image.get("digest"), "rollout image digest")
        expected_repository = expected_images.get(component)
        if expected_repository is None:
            fail("unexpected rollout image component")
        expected_full = f"ghcr.io/{expected_repository}"
        if image.get("image") != expected_full or image.get("tag_is_authority") is not False:
            fail("rollout image authority differs")
        observed_images[component] = {
            "repository": expected_repository,
            "digest": digest,
            "subject": f"{expected_full}@{digest}",
        }
    if set(observed_images) != set(expected_images):
        fail("rollout image component set differs")
    migration = target.get("migration")
    if type(migration) is not dict or migration.get("digest") != observed_images["web"]["digest"]:
        fail("rollout migration/web digest binding differs")
    rollback = target.get("rollback")
    if rollback != {"allowed_targets": [], "forbidden_targets": []}:
        fail("baseline rollback floor differs")
    return target, observed_images


def build_logical_candidate(
    live_spec: Mapping[str, Any],
    contract: Mapping[str, Any],
    images: Mapping[str, Mapping[str, str]],
) -> dict[str, Any]:
    candidate = copy.deepcopy(live_spec)
    for expected in contract["components"]:
        component = component_index(candidate, expected["collection"])[expected["app_name"]]
        if set(component).intersection(SOURCE_SELECTORS) != {"git"}:
            fail("live source selector is not exact")
        if set(SOURCE_FIELDS).intersection(component) != {"git", "dockerfile_path"}:
            fail("live source envelope is not exact")
        component.pop("git")
        component.pop("dockerfile_path")
        release_component = expected["release_component"]
        image = images[release_component]
        component["image"] = {
            "registry_type": contract["logical_source_transform"]["registry_type"],
            "registry": contract["logical_source_transform"]["registry"],
            "repository": image["repository"],
            "digest": image["digest"],
        }
        if set(component["image"]).intersection(
            contract["logical_source_transform"]["forbidden_image_fields"]
        ):
            fail("logical candidate contains a mutable or credentialed image field")
    live_projection = strip_component_sources(live_spec, contract)
    candidate_projection = strip_component_sources(candidate, contract)
    if live_projection != candidate_projection:
        fail("logical candidate changed a non-source production field")
    return candidate


def provider_state(
    app_response: Any,
    deployment_response: Any,
    contract: Mapping[str, Any],
    target: Mapping[str, str],
) -> tuple[dict[str, Any], dict[str, Any]]:
    if type(app_response) is not dict or type(app_response.get("app")) is not dict:
        fail("provider app response is malformed")
    if type(deployment_response) is not dict or type(deployment_response.get("deployment")) is not dict:
        fail("provider deployment response is malformed")
    app = app_response["app"]
    deployment = deployment_response["deployment"]
    provider = contract["provider"]
    bootstrap = contract["bootstrap_state"]

    if require_uuid(app.get("id"), "observed app ID") != target["app_id"]:
        fail("observed app ID differs")
    observed_spec = app.get("spec")
    if type(observed_spec) is not dict:
        fail("observed app spec is malformed")
    if observed_spec.get("name") != provider["app_name"]:
        fail("observed app name differs")
    if app.get("default_ingress") != target["default_ingress"]:
        fail("observed provider default ingress differs")
    updated_at = require_timestamp(app.get("updated_at"), "observed app updated_at")
    if updated_at != target["app_updated_at"]:
        fail("observed app updated_at differs from the reviewed bootstrap")

    active = app.get("active_deployment")
    if type(active) is not dict:
        fail("active deployment is missing")
    active_id = require_uuid(active.get("id"), "active deployment ID")
    if active_id != target["active_deployment_id"]:
        fail("active deployment differs from the reviewed bootstrap")
    if active.get("phase") != "ACTIVE":
        fail("active deployment is not ACTIVE")
    for field in ("in_progress_deployment", "pending_deployment", "pinned_deployment"):
        if app.get(field) is not None:
            fail("provider reports a pending, in-progress, or pinned deployment")

    if require_uuid(deployment.get("id"), "deployment response ID") != active_id:
        fail("deployment response ID differs from the active deployment")
    if deployment.get("phase") != "ACTIVE":
        fail("deployment response is not ACTIVE")
    live_spec = observed_spec
    active_spec = deployment.get("spec")
    if type(live_spec) is not dict or type(active_spec) is not dict:
        fail("provider response is missing an app spec")
    embedded_active_spec = active.get("spec")
    if embedded_active_spec is not None and embedded_active_spec != live_spec:
        fail("embedded active spec differs from the live spec")
    if live_spec != active_spec:
        fail("live and active deployment specs differ")

    canonical_spec_sha256 = sha256_value(live_spec)
    environment_sha256 = environment_value_fingerprint(live_spec)
    non_source_sha256 = non_source_fingerprint(live_spec, contract)
    if canonical_spec_sha256 != bootstrap["canonical_spec_sha256"]:
        fail("raw production spec differs from the reviewed bootstrap")
    if environment_sha256 != bootstrap["environment_values_sha256"]:
        fail("production environment values differ from the reviewed bootstrap")
    if non_source_sha256 != bootstrap["non_source_projection_sha256"]:
        fail("production non-source projection differs from the reviewed bootstrap")

    validate_topology(live_spec, contract)
    validate_legacy_component_sources(live_spec, contract)
    validate_deployment_sources(deployment, contract)

    state = {
        "app_identity_sha256": provider["app_id_sha256"],
        "app_name": provider["app_name"],
        "default_ingress_sha256": provider["default_ingress_sha256"],
        "app_updated_at_sha256": bootstrap["app_updated_at_sha256"],
        "active_deployment_identity_sha256": bootstrap[
            "active_deployment_id_sha256"
        ],
        "active_phase": "ACTIVE",
        "in_progress_deployment": False,
        "pending_deployment": False,
        "pinned_deployment": False,
        "live_canonical_spec_sha256": canonical_spec_sha256,
        "active_canonical_spec_sha256": sha256_value(active_spec),
        "environment_values_sha256": environment_sha256,
        "non_source_projection_sha256": non_source_sha256,
        "live_active_equal": True,
        "bootstrap_match": True,
    }
    return state, copy.deepcopy(live_spec)


def sanitize_plan(
    value: Any,
    contract: Mapping[str, Any],
    *,
    private_values: Sequence[str] = (),
) -> None:
    forbidden_keys = set(contract["sanitization"]["forbidden_keys"])
    prefixes = tuple(contract["sanitization"]["forbidden_string_prefixes"])
    if any(type(item) is not str or not item for item in private_values):
        fail("private target scrub list is malformed")

    def inspect(item: Any) -> None:
        if type(item) is dict:
            for key, child in item.items():
                if type(key) is not str or key.lower() in forbidden_keys:
                    fail("sanitized plan contains a forbidden key")
                inspect(child)
        elif type(item) is list:
            for child in item:
                inspect(child)
        elif type(item) is str:
            if any(private in item for private in private_values):
                fail("sanitized plan contains a private production target value")
            if any(item.startswith(prefix) for prefix in prefixes):
                fail("sanitized plan contains a credential or ciphertext prefix")
            if "\n" in item or "\r" in item or "\x00" in item:
                fail("sanitized plan contains a control character")
        elif item is not None and type(item) not in {int, bool}:
            fail("sanitized plan contains an unsupported value type")

    inspect(value)
    raw = canonical_file_bytes(value)
    if len(raw) > contract["artifact"]["maximum_bytes"]:
        fail("sanitized plan exceeds the contract byte budget")
    lower = raw.lower()
    for prefix in prefixes:
        if prefix.lower().encode("utf-8") in lower:
            fail("sanitized plan contains forbidden content")


def issue_window(now: dt.datetime, maximum_age_seconds: int) -> tuple[str, str]:
    if now.tzinfo is None or now.utcoffset() != dt.timedelta(0):
        fail("plan clock must be UTC")
    normalized = now.replace(microsecond=0)
    expires = normalized + dt.timedelta(seconds=maximum_age_seconds)
    return (
        normalized.isoformat().replace("+00:00", "Z"),
        expires.isoformat().replace("+00:00", "Z"),
    )


def target_image_records(
    contract: Mapping[str, Any], images: Mapping[str, Mapping[str, str]]
) -> list[dict[str, str]]:
    result = []
    seen: set[str] = set()
    for component in contract["components"]:
        release_component = component["release_component"]
        if release_component in seen:
            continue
        seen.add(release_component)
        image = images[release_component]
        result.append(
            {
                "component": release_component,
                "repository": f"ghcr.io/{image['repository']}",
                "digest": image["digest"],
                "subject": image["subject"],
            }
        )
    return result


def source_mutation_records(contract: Mapping[str, Any]) -> list[str]:
    return [
        f"{item['collection']}:{item['app_name']}:git+dockerfile_path->image"
        for item in contract["components"]
    ]


def build_plan(
    *,
    contract: Mapping[str, Any],
    contract_sha256: str,
    verifier_sha256: str,
    normalized_input: Mapping[str, Any],
    target_descriptor: Mapping[str, str],
    rollout_plan: Mapping[str, Any],
    first_app_response: Any,
    first_deployment_response: Any,
    second_app_response: Any,
    second_deployment_response: Any,
    workflow_run_id: str,
    workflow_run_attempt: int,
    request_log: Sequence[tuple[str, str]],
    now: dt.datetime,
) -> dict[str, Any]:
    target_phase, images = validate_rollout_plan(rollout_plan, contract, normalized_input)
    first_state, live_spec = provider_state(
        first_app_response, first_deployment_response, contract, target_descriptor
    )
    second_state, second_spec = provider_state(
        second_app_response, second_deployment_response, contract, target_descriptor
    )
    if first_state != second_state or live_spec != second_spec:
        fail("production changed between the two observations")

    expected_app_path, expected_deployment_path = provider_paths(
        contract, target_descriptor
    )
    expected_log = [
        ("GET", expected_app_path),
        ("GET", expected_deployment_path),
        ("GET", expected_app_path),
        ("GET", expected_deployment_path),
    ]
    if list(request_log) != expected_log:
        fail("provider request ledger differs from the exact GET-only contract")

    candidate = build_logical_candidate(live_spec, contract, images)
    candidate_projection = non_source_fingerprint(candidate, contract)
    if candidate_projection != first_state["non_source_projection_sha256"]:
        fail("logical candidate does not preserve the non-source projection")
    migration_digest = images["web"]["digest"]
    issued_at, expires_at = issue_window(now, contract["plan"]["maximum_age_seconds"])

    plan = {
        "schema_version": 1,
        "authority": "observation-only-production-plan",
        "repository": contract["repository"],
        "issued_at": issued_at,
        "expires_at": expires_at,
        "control": {
            "workflow_sha": normalized_input["control_sha"],
            "workflow_path": contract["workflow"]["path"],
            "run_id": require_run_id(workflow_run_id, "plan workflow run ID"),
            "run_attempt": exact_int(workflow_run_attempt, "plan workflow run attempt", 1),
            "runner_environment": "github-hosted",
            "contract_sha256": require_sha256(
                contract_sha256, "production contract exact-file hash"
            ),
            "verifier_sha256": require_sha256(
                verifier_sha256, "production verifier exact-file hash"
            ),
        },
        "rollout_authority": {
            "run_id": normalized_input["rollout_run_id"],
            "run_attempt": normalized_input["rollout_run_attempt"],
            "artifact_id": normalized_input["capsule_artifact_id"],
            "artifact_digest": normalized_input["capsule_artifact_digest"],
            "rollout_plan_sha256": normalized_input["rollout_plan_sha256"],
            "target_phase": target_phase["phase"],
            "target_source_sha": target_phase["source"]["commit"],
        },
        "provider_observation": {
            **first_state,
            "provider": contract["provider"]["name"],
            "http_methods_used": ["GET"],
            "http_request_count": 4,
            "http_endpoint_labels": ["app", "active-deployment"],
            "mutation_request_count": 0,
        },
        "target": {
            "phase": target_phase["phase"],
            "images": target_image_records(contract, images),
            "migration": {
                "job": "rereply-rls-migrate",
                "binding": contract["logical_source_transform"]["migration_binding"],
                "digest": migration_digest,
            },
            "credential_neutral_logical_candidate_sha256": sha256_value(candidate),
        },
        "preservation": {
            "non_source_projection_sha256": candidate_projection,
            "non_source_equal": True,
            "environment_equal": True,
            "ingress_equal": True,
            "domains_equal": True,
            "databases_equal": True,
            "runtime_shape_equal": True,
            "source_mutations": source_mutation_records(contract),
        },
        "provider_validation": {
            "spec_validation_performed": False,
            "proposal_performed": False,
            "mutation_performed": False,
            "deployment_authority": False,
        },
        "rollback": target_phase["rollback"],
    }
    sanitize_plan(plan, contract, private_values=tuple(target_descriptor.values()))
    return plan


def validate_plan(
    plan: Any,
    contract: Mapping[str, Any],
    contract_sha256: str,
    verifier_sha256: str,
    rollout_plan: Mapping[str, Any],
    rollout_plan_sha256: str,
    *,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    plan = exact_keys(
        plan,
        {
            "schema_version",
            "authority",
            "repository",
            "issued_at",
            "expires_at",
            "control",
            "rollout_authority",
            "provider_observation",
            "target",
            "preservation",
            "provider_validation",
            "rollback",
        },
        "production plan",
    )
    exact_int(plan["schema_version"], "plan schema version", 1, 1)
    if plan["authority"] != "observation-only-production-plan":
        fail("plan authority differs")
    if plan["repository"] != contract["repository"]:
        fail("plan repository differs")
    issued = require_timestamp(plan["issued_at"], "plan issued_at")
    expires = require_timestamp(plan["expires_at"], "plan expires_at")
    issued_dt = dt.datetime.fromisoformat(issued[:-1] + "+00:00")
    expires_dt = dt.datetime.fromisoformat(expires[:-1] + "+00:00")
    if int((expires_dt - issued_dt).total_seconds()) != contract["plan"]["maximum_age_seconds"]:
        fail("plan expiry window differs")
    if now is not None:
        if now.tzinfo is None or now.utcoffset() != dt.timedelta(0):
            fail("plan verification clock must be UTC")
        checked_at = now.replace(microsecond=0)
        if checked_at < issued_dt or checked_at > expires_dt:
            fail("production plan is not currently valid")

    control = exact_keys(
        plan["control"],
        {
            "workflow_sha",
            "workflow_path",
            "run_id",
            "run_attempt",
            "runner_environment",
            "contract_sha256",
            "verifier_sha256",
        },
        "plan control",
    )
    control_sha = require_sha1(control["workflow_sha"], "plan control SHA")
    if control["workflow_path"] != contract["workflow"]["path"]:
        fail("plan workflow path differs")
    require_run_id(control["run_id"], "plan run ID")
    exact_int(control["run_attempt"], "plan run attempt", 1)
    if control["runner_environment"] != "github-hosted":
        fail("plan runner environment differs")
    if control["contract_sha256"] != require_sha256(
        contract_sha256, "production contract exact-file hash"
    ):
        fail("plan contract hash differs")
    if control["verifier_sha256"] != require_sha256(
        verifier_sha256, "production verifier exact-file hash"
    ):
        fail("plan verifier hash differs")

    authority = exact_keys(
        plan["rollout_authority"],
        {
            "run_id",
            "run_attempt",
            "artifact_id",
            "artifact_digest",
            "rollout_plan_sha256",
            "target_phase",
            "target_source_sha",
        },
        "rollout authority",
    )
    normalized = {
        "control_sha": control_sha,
        "rollout_run_id": require_run_id(authority["run_id"], "rollout run ID"),
        "rollout_run_attempt": exact_int(
            authority["run_attempt"], "rollout run attempt", 1
        ),
        "capsule_artifact_id": require_run_id(
            authority["artifact_id"], "rollout artifact ID"
        ),
        "capsule_artifact_digest": require_digest(
            authority["artifact_digest"], "rollout artifact digest"
        ),
        "rollout_plan_sha256": require_sha256(
            authority["rollout_plan_sha256"], "rollout plan hash"
        ),
    }
    if normalized["rollout_plan_sha256"] != require_sha256(
        rollout_plan_sha256, "rollout plan file hash"
    ):
        fail("rollout plan content hash differs")
    target_phase, images = validate_rollout_plan(rollout_plan, contract, normalized)
    if authority["target_phase"] != target_phase["phase"]:
        fail("plan target phase differs")
    if authority["target_source_sha"] != target_phase["source"]["commit"]:
        fail("plan target source differs")

    observation = exact_keys(
        plan["provider_observation"],
        {
            "app_identity_sha256",
            "app_name",
            "default_ingress_sha256",
            "app_updated_at_sha256",
            "active_deployment_identity_sha256",
            "active_phase",
            "in_progress_deployment",
            "pending_deployment",
            "pinned_deployment",
            "live_canonical_spec_sha256",
            "active_canonical_spec_sha256",
            "environment_values_sha256",
            "non_source_projection_sha256",
            "live_active_equal",
            "bootstrap_match",
            "provider",
            "http_methods_used",
            "http_request_count",
            "http_endpoint_labels",
            "mutation_request_count",
        },
        "provider observation",
    )
    bootstrap = contract["bootstrap_state"]
    if observation["provider"] != contract["provider"]["name"]:
        fail("observed provider differs")
    if observation["app_identity_sha256"] != contract["provider"]["app_id_sha256"]:
        fail("observed app identity hash differs")
    if observation["app_name"] != contract["provider"]["app_name"]:
        fail("observed app name differs")
    if observation["default_ingress_sha256"] != contract["provider"][
        "default_ingress_sha256"
    ]:
        fail("observed default ingress hash differs")
    if observation["app_updated_at_sha256"] != bootstrap["app_updated_at_sha256"]:
        fail("observed app timestamp hash differs")
    if observation["active_deployment_identity_sha256"] != bootstrap[
        "active_deployment_id_sha256"
    ]:
        fail("observed active deployment hash differs")
    if observation["active_phase"] != "ACTIVE":
        fail("observed active phase differs")
    for key in (
        "in_progress_deployment",
        "pending_deployment",
        "pinned_deployment",
    ):
        if exact_bool(observation[key], f"observation {key}") is not False:
            fail("observation contains an active deployment transition")
    for key in ("live_active_equal", "bootstrap_match"):
        if exact_bool(observation[key], f"observation {key}") is not True:
            fail("observation equality proof is false")
    if observation["live_canonical_spec_sha256"] != bootstrap["canonical_spec_sha256"]:
        fail("observed live spec hash differs")
    if observation["active_canonical_spec_sha256"] != bootstrap["canonical_spec_sha256"]:
        fail("observed active spec hash differs")
    if observation["environment_values_sha256"] != bootstrap["environment_values_sha256"]:
        fail("observed environment fingerprint differs")
    if observation["non_source_projection_sha256"] != bootstrap[
        "non_source_projection_sha256"
    ]:
        fail("observed non-source fingerprint differs")
    if observation["http_methods_used"] != ["GET"]:
        fail("provider observation is not GET-only")
    exact_int(observation["http_request_count"], "provider request count", 4, 4)
    exact_int(observation["mutation_request_count"], "mutation request count", 0, 0)
    if observation["http_endpoint_labels"] != ["app", "active-deployment"]:
        fail("provider observation endpoint labels differ")

    target = exact_keys(
        plan["target"],
        {"phase", "images", "migration", "credential_neutral_logical_candidate_sha256"},
        "plan target",
    )
    if target["phase"] != target_phase["phase"]:
        fail("plan target phase differs")
    if target["images"] != target_image_records(contract, images):
        fail("plan target images differ")
    require_sha256(
        target["credential_neutral_logical_candidate_sha256"],
        "logical candidate hash",
    )
    migration = exact_keys(
        target["migration"], {"job", "binding", "digest"}, "target migration"
    )
    if migration != {
        "job": "rereply-rls-migrate",
        "binding": "same-web-image-digest",
        "digest": images["web"]["digest"],
    }:
        fail("target migration binding differs")

    preservation = exact_keys(
        plan["preservation"],
        {
            "non_source_projection_sha256",
            "non_source_equal",
            "environment_equal",
            "ingress_equal",
            "domains_equal",
            "databases_equal",
            "runtime_shape_equal",
            "source_mutations",
        },
        "plan preservation",
    )
    if preservation["non_source_projection_sha256"] != bootstrap[
        "non_source_projection_sha256"
    ]:
        fail("preserved non-source hash differs")
    for key in (
        "non_source_equal",
        "environment_equal",
        "ingress_equal",
        "domains_equal",
        "databases_equal",
        "runtime_shape_equal",
    ):
        if exact_bool(preservation[key], f"preservation {key}") is not True:
            fail("plan preservation proof is false")
    if preservation["source_mutations"] != source_mutation_records(contract):
        fail("source mutation set differs")

    if plan["provider_validation"] != {
        "spec_validation_performed": False,
        "proposal_performed": False,
        "mutation_performed": False,
        "deployment_authority": False,
    }:
        fail("provider validation boundary differs")
    if plan["rollback"] != {"allowed_targets": [], "forbidden_targets": []}:
        fail("baseline rollback floor differs")
    sanitize_plan(plan, contract)
    return plan


class ProviderClient:
    """Exact-path, GET-only DigitalOcean client with no proxy or redirect use."""

    def __init__(
        self,
        contract: Mapping[str, Any],
        target: Mapping[str, str],
        token: str,
        *,
        opener: Any | None = None,
    ) -> None:
        if type(token) is not str or len(token) < 20 or any(ch in token for ch in "\r\n\x00"):
            fail("read-only provider token is invalid")
        self.contract = contract
        self.allowed_paths = set(provider_paths(contract, target))
        self.token = token
        self.opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), RejectRedirects()
        )
        self.request_log: list[tuple[str, str]] = []

    def allowed_path(self, path: str) -> bool:
        return path in self.allowed_paths

    def get_json(self, path: str) -> Any:
        if not self.allowed_path(path):
            fail("provider path is outside the exact allowlist")
        origin = self.contract["provider"]["api_origin"]
        url = origin + path
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "https"
            or parsed.hostname != "api.digitalocean.com"
            or parsed.port not in (None, 443)
            or parsed.username is not None
            or parsed.password is not None
            or parsed.query
            or parsed.fragment
        ):
            fail("provider URL is outside the exact HTTPS origin")
        request = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self.token}",
                "User-Agent": "rereply-production-plan/1",
            },
        )
        try:
            with self.opener.open(request, timeout=20) as response:
                final_url = response.geturl()
                if final_url != url:
                    fail("provider response URL differs from the exact request")
                status = getattr(response, "status", None)
                if status is None:
                    status = response.getcode()
                if status != 200:
                    fail("provider returned a non-success status")
                content_type = (
                    response.headers.get("Content-Type", "")
                    .split(";", 1)[0]
                    .strip()
                    .lower()
                )
                if content_type != "application/json":
                    fail("provider returned an unexpected content type")
                raw = response.read(MAX_JSON_BYTES + 1)
        except PlanError:
            raise
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError) as exc:
            raise PlanError("provider observation failed") from exc
        if not raw or len(raw) > MAX_JSON_BYTES:
            fail("provider response has an invalid size")
        try:
            parsed = loads_strict(raw.decode("utf-8"))
        except UnicodeError as exc:
            raise PlanError("provider response is not UTF-8 JSON") from exc
        self.request_log.append(("GET", path))
        return parsed


def observe(
    *,
    contract: Mapping[str, Any],
    contract_sha256: str,
    verifier_sha256: str,
    normalized_input: Mapping[str, Any],
    target_descriptor: Mapping[str, str],
    rollout_plan: Mapping[str, Any],
    rollout_plan_sha256: str,
    workflow_run_id: str,
    workflow_run_attempt: int,
    token: str,
    now: dt.datetime | None = None,
    opener: Any | None = None,
) -> dict[str, Any]:
    for name in contract["security"]["forbidden_ambient_environment"]:
        if os.environ.get(name):
            fail("a forbidden ambient production credential is present")
    client = ProviderClient(contract, target_descriptor, token, opener=opener)
    app_path, deployment_path = provider_paths(contract, target_descriptor)
    first_app = client.get_json(app_path)
    if type(first_app) is not dict or type(first_app.get("app")) is not dict:
        fail("provider app response is malformed")
    first_active = first_app["app"].get("active_deployment")
    if type(first_active) is not dict:
        fail("active deployment is missing")
    active_id = require_uuid(first_active.get("id"), "active deployment ID")
    if active_id != target_descriptor["active_deployment_id"]:
        fail("active deployment differs from the protected target")
    first_deployment = client.get_json(deployment_path)
    second_app = client.get_json(app_path)
    second_deployment = client.get_json(deployment_path)
    plan = build_plan(
        contract=contract,
        contract_sha256=contract_sha256,
        verifier_sha256=verifier_sha256,
        normalized_input=normalized_input,
        target_descriptor=target_descriptor,
        rollout_plan=rollout_plan,
        first_app_response=first_app,
        first_deployment_response=first_deployment,
        second_app_response=second_app,
        second_deployment_response=second_deployment,
        workflow_run_id=workflow_run_id,
        workflow_run_attempt=workflow_run_attempt,
        request_log=client.request_log,
        now=now or dt.datetime.now(dt.timezone.utc),
    )
    checked_at = now or dt.datetime.now(dt.timezone.utc)
    validate_plan(
        plan,
        contract,
        contract_sha256,
        verifier_sha256,
        rollout_plan,
        rollout_plan_sha256,
        now=checked_at,
    )
    return plan


def load_normalized_input(path: Path, control_sha: str) -> dict[str, Any]:
    value = load_json(path, "normalized production-plan input", canonical=True)
    return normalize_input(
        json.dumps(value, allow_nan=False, ensure_ascii=True, separators=(",", ":")),
        control_sha,
    )


def load_rollout_plan_for_observation(
    path: Path,
    contract: Mapping[str, Any],
    normalized_input: Mapping[str, Any],
) -> tuple[dict[str, Any], str]:
    value, digest = load_json_and_hash(path, "rollout plan", canonical=True)
    if type(value) is not dict:
        fail("rollout plan is malformed")
    if digest != normalized_input["rollout_plan_sha256"]:
        fail("rollout plan exact-file hash differs")
    validate_rollout_plan(value, contract, normalized_input)
    return value, digest


def trusted_verifier_hash(path: Path) -> str:
    executing = Path(__file__)
    if path.is_symlink() or not path.is_file() or executing.is_symlink():
        fail("production verifier must be a regular non-symlink file")
    try:
        if path.resolve(strict=True) != executing.resolve(strict=True):
            fail("production verifier path differs from the executing verifier")
        raw = read_regular_file(path, "production verifier", MAX_JSON_BYTES)
    except OSError as exc:
        raise PlanError("production verifier could not be read") from exc
    return sha256_bytes(raw)


def verify_github_runtime(
    contract: Mapping[str, Any],
    control_sha: str,
    workflow_run_id: str,
    workflow_run_attempt: int,
) -> None:
    expected = {
        "GITHUB_REPOSITORY": contract["repository"],
        "GITHUB_REF": "refs/heads/main",
        "GITHUB_SHA": control_sha,
        "GITHUB_WORKFLOW_SHA": control_sha,
        "GITHUB_WORKFLOW_REF": (
            f"{contract['repository']}/{contract['workflow']['path']}@refs/heads/main"
        ),
        "GITHUB_EVENT_NAME": "workflow_dispatch",
        "GITHUB_RUN_ID": workflow_run_id,
        "GITHUB_RUN_ATTEMPT": str(workflow_run_attempt),
        "RUNNER_ENVIRONMENT": "github-hosted",
        "RUNNER_OS": "Linux",
    }
    for name, value in expected.items():
        if os.environ.get(name) != value:
            fail("GitHub runtime authority differs from the exact plan contract")


def validate_hash_sidecar(digest: str, hash_path: Path) -> str:
    require_sha256(digest, "production plan exact-file hash")
    raw = read_regular_file(hash_path, "production plan hash", 65)
    if len(raw) != 65:
        fail("production plan hash file has an invalid size")
    if raw != (digest + "\n").encode("ascii"):
        fail("production plan exact-file hash differs")
    return digest


def command_validate_contract(args: argparse.Namespace) -> None:
    validate_contract(load_json(args.contract, "production app contract"))


def command_normalize_input(args: argparse.Namespace) -> None:
    normalized = normalize_input(args.raw, args.control_sha)
    write_canonical(args.output, normalized)


def command_observe(args: argparse.Namespace) -> None:
    contract_value, contract_hash = load_json_and_hash(
        args.contract, "production app contract"
    )
    contract = validate_contract(contract_value)
    verifier_hash = trusted_verifier_hash(args.verifier)
    normalized = load_normalized_input(args.normalized_input, args.control_sha)
    rollout_plan, rollout_hash = load_rollout_plan_for_observation(
        args.rollout_plan, contract, normalized
    )
    require_run_id(args.workflow_run_id, "production-plan workflow run ID")
    exact_int(args.workflow_run_attempt, "production-plan workflow run attempt", 1)
    verify_github_runtime(
        contract,
        args.control_sha,
        args.workflow_run_id,
        args.workflow_run_attempt,
    )

    token_name = contract["security"]["token_environment"]
    target_name = contract["security"]["target_environment"]
    token = os.environ.pop(token_name, None)
    if token is None:
        fail("the dedicated read-only provider token is unavailable")
    target_raw = os.environ.pop(target_name, None)
    if target_raw is None:
        fail("the protected production target descriptor is unavailable")
    target_descriptor = normalize_target_descriptor(target_raw, contract)

    output_dir = prepare_runner_output_directory(args.output_dir)
    plan_path = output_dir / "production-plan.json"
    hash_path = output_dir / "production-plan.sha256"
    try:
        checked_at = dt.datetime.now(dt.timezone.utc)
        plan = observe(
            contract=contract,
            contract_sha256=contract_hash,
            verifier_sha256=verifier_hash,
            normalized_input=normalized,
            target_descriptor=target_descriptor,
            rollout_plan=rollout_plan,
            rollout_plan_sha256=rollout_hash,
            workflow_run_id=args.workflow_run_id,
            workflow_run_attempt=args.workflow_run_attempt,
            token=token,
            now=checked_at,
        )
        write_plan_artifacts(output_dir, plan)
        loaded_plan, plan_hash = load_json_and_hash(
            plan_path, "production plan", canonical=True
        )
        validate_hash_sidecar(plan_hash, hash_path)
        validate_plan(
            loaded_plan,
            contract,
            contract_hash,
            verifier_hash,
            rollout_plan,
            rollout_hash,
            now=dt.datetime.now(dt.timezone.utc),
        )
    except BaseException:
        plan_path.unlink(missing_ok=True)
        hash_path.unlink(missing_ok=True)
        try:
            output_dir.rmdir()
        except OSError:
            pass
        raise


def command_verify_plan(args: argparse.Namespace) -> None:
    contract_value, contract_hash = load_json_and_hash(
        args.contract, "production app contract"
    )
    contract = validate_contract(contract_value)
    verifier_hash = trusted_verifier_hash(args.verifier)
    plan, plan_hash = load_json_and_hash(args.plan, "production plan", canonical=True)
    rollout_plan, rollout_hash = load_json_and_hash(
        args.rollout_plan, "rollout plan", canonical=True
    )
    validate_hash_sidecar(plan_hash, args.sha256)
    validate_plan(
        plan,
        contract,
        contract_hash,
        verifier_hash,
        rollout_plan,
        rollout_hash,
        now=dt.datetime.now(dt.timezone.utc),
    )


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subparsers = result.add_subparsers(dest="command", required=True)

    contract_parser = subparsers.add_parser("validate-contract")
    contract_parser.add_argument("--contract", type=Path, required=True)
    contract_parser.set_defaults(handler=command_validate_contract)

    input_parser = subparsers.add_parser("normalize-input")
    input_parser.add_argument("--raw", required=True)
    input_parser.add_argument("--control-sha", required=True)
    input_parser.add_argument("--output", type=Path, required=True)
    input_parser.set_defaults(handler=command_normalize_input)

    observe_parser = subparsers.add_parser("observe")
    observe_parser.add_argument("--contract", type=Path, required=True)
    observe_parser.add_argument("--verifier", type=Path, required=True)
    observe_parser.add_argument("--normalized-input", type=Path, required=True)
    observe_parser.add_argument("--rollout-plan", type=Path, required=True)
    observe_parser.add_argument("--control-sha", required=True)
    observe_parser.add_argument("--workflow-run-id", required=True)
    observe_parser.add_argument("--workflow-run-attempt", type=int, required=True)
    observe_parser.add_argument("--output-dir", type=Path, required=True)
    observe_parser.set_defaults(handler=command_observe)

    verify_parser = subparsers.add_parser("verify-plan")
    verify_parser.add_argument("--contract", type=Path, required=True)
    verify_parser.add_argument("--verifier", type=Path, required=True)
    verify_parser.add_argument("--rollout-plan", type=Path, required=True)
    verify_parser.add_argument("--plan", type=Path, required=True)
    verify_parser.add_argument("--sha256", type=Path, required=True)
    verify_parser.set_defaults(handler=command_verify_plan)
    return result


def main() -> int:
    args = parser().parse_args()
    try:
        args.handler(args)
    except PlanError as exc:
        print(f"production plan rejected: {exc}", file=sys.stderr)
        return 1
    except OSError:
        print("production plan rejected: secure file operation failed", file=sys.stderr)
        return 1
    except Exception:
        print("production plan rejected: internal validation failure", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
