#!/usr/bin/env python3
"""Fail-closed primitives for the ReReply production release lanes.

This module deliberately exposes only the exact operations needed by the
recovery, phase-apply, and phase-rollback controllers.  It is dependency-free
so a pinned repository blob can run with ``python -I -S -B``.  Public evidence
contains hashes and semantic labels only; provider identifiers and live specs
must never be written to an artifact.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import math
import os
import re
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Iterable, Mapping, NoReturn, Sequence


REPOSITORY = "medtechcorps-netizen/whatomate"
API_ORIGIN = "https://api.digitalocean.com"
PHASES = ("baseline", "bridge", "backend", "ui")
PREDECESSOR = {
    "baseline": "genesis",
    "bridge": "baseline",
    "backend": "bridge",
    "ui": "backend",
}
ROLLBACK_FLOORS = {
    "baseline": {"allowed_targets": [], "forbidden_targets": []},
    "bridge": {"allowed_targets": ["baseline"], "forbidden_targets": []},
    "backend": {"allowed_targets": ["bridge"], "forbidden_targets": ["baseline"]},
    "ui": {"allowed_targets": ["backend", "bridge"], "forbidden_targets": ["baseline"]},
}
COMPONENTS = (
    ("services", "omnitech-web", "web", "medtechcorps-netizen/rereply-release-web"),
    ("services", "meta-relay", "meta-relay", "medtechcorps-netizen/rereply-release-meta-relay"),
    ("services", "gmail-relay", "gmail-relay", "medtechcorps-netizen/rereply-release-gmail-relay"),
    ("jobs", "rereply-rls-migrate", "web", "medtechcorps-netizen/rereply-release-web"),
)
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
UUID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
RUN_ID_RE = re.compile(r"^[1-9][0-9]{0,14}$")
MAX_JSON_BYTES = 4 * 1024 * 1024
MAX_EVIDENCE_BYTES = 256 * 1024
MAX_PLAN_AGE_SECONDS = 900
MAX_RECOVERY_AGE_SECONDS = 900


class ReleaseError(RuntimeError):
    """An intentionally content-free release-control failure."""


class AmbiguousMutation(ReleaseError):
    """A single mutation may have reached the provider."""


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:
        del args, kwargs
        raise ReleaseError("provider redirects are forbidden")


def fail(message: str) -> NoReturn:
    raise ReleaseError(message)


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
            raise ReleaseError("JSON is not UTF-8") from exc
    try:
        return json.loads(
            raw,
            object_pairs_hook=_reject_pairs,
            parse_float=_reject_number,
            parse_constant=_reject_number,
        )
    except ReleaseError:
        raise
    except (json.JSONDecodeError, TypeError, ValueError) as exc:
        raise ReleaseError("JSON is malformed") from exc


def canonical_payload_bytes(value: Any) -> bytes:
    try:
        encoded = json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=True,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("ascii")
    except (TypeError, ValueError) as exc:
        raise ReleaseError("value is not canonical JSON") from exc
    return encoded


def canonical_file_bytes(value: Any) -> bytes:
    return canonical_payload_bytes(value) + b"\n"


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha256_value(value: Any) -> str:
    return sha256_bytes(canonical_payload_bytes(value))


def exact_keys(value: Any, expected: Iterable[str], label: str) -> dict[str, Any]:
    expected_set = set(expected)
    if type(value) is not dict or set(value) != expected_set:
        fail(f"{label} keys differ")
    return value


def exact_string(value: Any, label: str, pattern: re.Pattern[str] | None = None) -> str:
    if type(value) is not str or not value or len(value) > 4096:
        fail(f"{label} is invalid")
    if any(ch in value for ch in "\r\n\x00"):
        fail(f"{label} contains forbidden characters")
    if pattern is not None and pattern.fullmatch(value) is None:
        fail(f"{label} has an invalid format")
    return value


def exact_int(value: Any, label: str, minimum: int = 0, maximum: int = 2_147_483_647) -> int:
    if type(value) is not int or value < minimum or value > maximum:
        fail(f"{label} is invalid")
    return value


def exact_bool(value: Any, label: str) -> bool:
    if type(value) is not bool:
        fail(f"{label} is invalid")
    return value


def require_sha1(value: Any, label: str) -> str:
    return exact_string(value, label, SHA1_RE)


def require_sha256(value: Any, label: str) -> str:
    return exact_string(value, label, SHA256_RE)


def require_digest(value: Any, label: str) -> str:
    return exact_string(value, label, DIGEST_RE)


def require_uuid(value: Any, label: str) -> str:
    return exact_string(value, label, UUID_RE)


def require_run_id(value: Any, label: str) -> str:
    if type(value) is int:
        value = str(value)
    return exact_string(value, label, RUN_ID_RE)


def require_timestamp(value: Any, label: str) -> dt.datetime:
    raw = exact_string(value, label)
    if not raw.endswith("Z"):
        fail(f"{label} must be UTC")
    try:
        parsed = dt.datetime.fromisoformat(raw[:-1] + "+00:00")
    except ValueError as exc:
        raise ReleaseError(f"{label} is invalid") from exc
    if parsed.utcoffset() != dt.timedelta(0) or parsed.microsecond:
        fail(f"{label} must be second-precision UTC")
    return parsed


def format_timestamp(value: dt.datetime) -> str:
    if value.tzinfo is None or value.utcoffset() != dt.timedelta(0):
        fail("clock must be UTC")
    return value.replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def validate_fresh_window(
    issued_at: Any,
    expires_at: Any,
    now: dt.datetime,
    *,
    maximum_age_seconds: int,
    label: str,
) -> None:
    issued = require_timestamp(issued_at, f"{label} issued_at")
    expires = require_timestamp(expires_at, f"{label} expires_at")
    if expires <= issued or (expires - issued).total_seconds() > maximum_age_seconds:
        fail(f"{label} validity window differs")
    if (
        not isinstance(now, dt.datetime)
        or now.tzinfo is None
        or now.utcoffset() is None
    ):
        fail(f"{label} clock is invalid")
    checked = now.astimezone(dt.timezone.utc)
    if checked < issued or checked >= expires:
        fail(f"{label} is stale or future-dated")


def load_json(path: Path, label: str, *, canonical: bool = True, maximum: int = MAX_JSON_BYTES) -> Any:
    if path.is_symlink() or not path.is_file():
        fail(f"{label} is not a regular file")
    raw = path.read_bytes()
    if not raw or len(raw) > maximum:
        fail(f"{label} has an invalid size")
    value = loads_strict(raw)
    if canonical and raw != canonical_file_bytes(value):
        fail(f"{label} is not canonical JSON")
    return value


def _ensure_output_path(path: Path, runner_temp: Path) -> Path:
    if runner_temp.is_symlink() or not runner_temp.is_dir():
        fail("RUNNER_TEMP is not a real directory")
    root = runner_temp.resolve(strict=True)
    if path.parent.resolve(strict=True) != root or path.name in {"", ".", ".."}:
        fail("output path must be a direct RUNNER_TEMP child")
    if path.exists() or path.is_symlink():
        fail("output already exists")
    return root / path.name


def write_canonical_output(path: Path, value: Any, runner_temp: Path) -> str:
    destination = _ensure_output_path(path, runner_temp)
    raw = canonical_file_bytes(value)
    if len(raw) > MAX_EVIDENCE_BYTES:
        fail("sanitized evidence is too large")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(destination, flags, 0o600)
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            fail("reserved output is not a single regular file")
        with os.fdopen(descriptor, "wb", closefd=False) as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        after = os.stat(destination, follow_symlinks=False)
        if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            fail("output identity changed")
    finally:
        os.close(descriptor)
    return sha256_bytes(raw)


def validate_control_identity(
    *,
    repository: str,
    ref: str,
    ref_protected: str,
    event_name: str,
    workflow_sha: str,
    dispatch_sha: str,
    workflow_ref: str,
    workflow_path: str,
    run_id: str,
    run_attempt: int,
    runner_environment: str,
) -> dict[str, Any]:
    if repository != REPOSITORY or ref != "refs/heads/main" or ref_protected != "true":
        fail("workflow is not running from exact protected main")
    if event_name != "workflow_dispatch" or runner_environment != "github-hosted":
        fail("workflow trigger or runner authority differs")
    control_sha = require_sha1(workflow_sha, "workflow SHA")
    if control_sha != require_sha1(dispatch_sha, "dispatch SHA"):
        fail("workflow and dispatch SHA differ")
    if workflow_ref != f"{REPOSITORY}/{workflow_path}@refs/heads/main":
        fail("workflow ref differs")
    return {
        "workflow_sha": control_sha,
        "workflow_path": workflow_path,
        "run_id": require_run_id(run_id, "workflow run ID"),
        "run_attempt": exact_int(run_attempt, "workflow run attempt", 1, 1),
        "runner_environment": "github-hosted",
    }


def validate_target_descriptor(value: Any, *, recovery: bool = False) -> dict[str, str]:
    keys = (
        {"postgres_cluster_id", "valkey_cluster_id", "valkey_recovery_cluster_id"}
        if recovery
        else {"app_id", "default_ingress"}
    )
    descriptor = exact_keys(value, keys, "protected target descriptor")
    if recovery:
        return {key: require_uuid(descriptor[key], f"target {key}") for key in sorted(keys)}
    app_id = require_uuid(descriptor["app_id"], "target app_id")
    ingress = exact_string(descriptor["default_ingress"], "target default_ingress")
    parsed = urllib.parse.urlsplit(ingress)
    if (
        parsed.scheme != "https"
        or parsed.username is not None
        or parsed.password is not None
        or parsed.port not in (None, 443)
        or not parsed.hostname
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        fail("target default ingress is not a canonical HTTPS origin")
    canonical_ingress = f"https://{parsed.hostname.lower()}"
    if ingress != canonical_ingress:
        fail("target default ingress is not canonical")
    return {"app_id": app_id, "default_ingress": ingress}


def target_descriptor_hash(value: Mapping[str, str]) -> str:
    return sha256_value(dict(value))


def public_route_contract_sha256(default_ingress: str) -> str:
    ingress = validate_target_descriptor(
        {
            "app_id": "00000000-0000-4000-8000-000000000000",
            "default_ingress": default_ingress,
        }
    )["default_ingress"]
    endpoints = {
        "app-health": f"{ingress}/health",
        "app-ready": f"{ingress}/ready",
        "meta-live": f"{ingress}/meta-relay/livez",
        "meta-ready": f"{ingress}/meta-relay/readyz",
        "gmail-live": f"{ingress}/gmail-relay/livez",
        "gmail-ready": f"{ingress}/gmail-relay/readyz",
    }
    return sha256_value({"schema_version": 1, "endpoints": endpoints})


def validate_phase(value: Any, label: str = "phase") -> str:
    phase = exact_string(value, label)
    if phase not in PHASES:
        fail(f"{label} differs")
    return phase


def phase_images_from_rollout(rollout: Any, phase: str) -> dict[str, str]:
    if type(rollout) is not dict or rollout.get("activation_order") != list(PHASES):
        fail("rollout authority is malformed")
    phases = rollout.get("phases")
    if type(phases) is not list or [item.get("phase") for item in phases if type(item) is dict] != list(PHASES):
        fail("rollout phase order differs")
    selected = phases[PHASES.index(validate_phase(phase))]
    images = selected.get("images")
    if type(images) is not list or len(images) != 3:
        fail("rollout image set differs")
    output: dict[str, str] = {}
    for item in images:
        item = exact_keys(
            item,
            {
                "component", "image", "digest", "platform", "tag",
                "tag_is_authority", "dockerfile", "dockerfile_sha256",
            },
            "rollout image",
        )
        component = exact_string(item["component"], "rollout image component")
        if component not in {"web", "meta-relay", "gmail-relay"} or component in output:
            fail("rollout image component differs")
        expected = f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
        if item["image"] != expected or item["platform"] != "linux/amd64" or item["tag_is_authority"] is not False:
            fail("rollout image authority differs")
        output[component] = require_digest(item["digest"], "rollout image digest")
    migration = selected.get("migration")
    if type(migration) is not dict or migration.get("digest") != output["web"]:
        fail("migration/web digest binding differs")
    if selected.get("rollback") != ROLLBACK_FLOORS[phase]:
        fail("rollout rollback floor differs")
    return output


def component_map(spec: Mapping[str, Any], collection: str) -> dict[str, dict[str, Any]]:
    values = spec.get(collection)
    if type(values) is not list:
        fail(f"spec {collection} is malformed")
    output: dict[str, dict[str, Any]] = {}
    for value in values:
        if type(value) is not dict:
            fail(f"spec {collection} item is malformed")
        name = exact_string(value.get("name"), f"spec {collection} name")
        if name in output:
            fail(f"spec {collection} contains duplicate names")
        output[name] = value
    return output


def extract_image_digests(spec: Mapping[str, Any]) -> dict[str, str]:
    output: dict[str, str] = {}
    for collection, name, release_component, repository in COMPONENTS:
        item = component_map(spec, collection).get(name)
        if item is None:
            fail("required production component is missing")
        image = exact_keys(
            item.get("image"),
            {"registry_type", "registry", "repository", "digest"},
            f"{name} image selector",
        )
        if image != {
            "registry_type": "GHCR",
            "registry": "ghcr.io",
            "repository": repository,
            "digest": image["digest"],
        }:
            fail(f"{name} image repository differs")
        digest = require_digest(image["digest"], f"{name} digest")
        prior = output.get(release_component)
        if prior is not None and prior != digest:
            fail("migration/web digest binding differs")
        output[release_component] = digest
    if set(output) != {"web", "meta-relay", "gmail-relay"}:
        fail("production image set differs")
    return output


def set_phase_images(spec: Mapping[str, Any], digests: Mapping[str, str]) -> dict[str, Any]:
    if set(digests) != {"web", "meta-relay", "gmail-relay"}:
        fail("target image set differs")
    desired = copy.deepcopy(spec)
    for collection, name, release_component, repository in COMPONENTS:
        item = component_map(desired, collection).get(name)
        if item is None:
            fail("required production component is missing")
        current_source_fields = set(item).intersection({"git", "github", "gitlab", "image"})
        if current_source_fields not in ({"git"}, {"image"}):
            fail(f"{name} has an ambiguous source selector")
        if current_source_fields == {"git"}:
            if "dockerfile_path" not in item:
                fail(f"{name} legacy source is incomplete")
            item.pop("git")
            item.pop("dockerfile_path")
        else:
            exact_keys(
                item["image"],
                {"registry_type", "registry", "repository", "digest"},
                f"{name} image selector",
            )
        item["image"] = {
            "registry_type": "GHCR",
            "registry": "ghcr.io",
            "repository": repository,
            "digest": require_digest(digests[release_component], f"{name} target digest"),
        }
    require_exact_image_change(spec, desired)
    return desired


def changed_leaf_pointers(left: Any, right: Any, prefix: str = "") -> list[str]:
    if type(left) is not type(right):
        return [prefix or "/"]
    if type(left) is dict:
        pointers: list[str] = []
        for key in sorted(set(left) | set(right)):
            escaped = key.replace("~", "~0").replace("/", "~1")
            child = f"{prefix}/{escaped}"
            if key not in left or key not in right:
                pointers.append(child)
            else:
                pointers.extend(changed_leaf_pointers(left[key], right[key], child))
        return pointers
    if type(left) is list:
        if len(left) != len(right):
            return [prefix or "/"]
        pointers = []
        for index, (before, after) in enumerate(zip(left, right, strict=True)):
            pointers.extend(changed_leaf_pointers(before, after, f"{prefix}/{index}"))
        return pointers
    return [] if left == right else [prefix or "/"]


def _component_index(spec: Mapping[str, Any], collection: str, name: str) -> int:
    values = spec.get(collection)
    if type(values) is not list:
        fail("spec collection is malformed")
    matches = [index for index, item in enumerate(values) if type(item) is dict and item.get("name") == name]
    if len(matches) != 1:
        fail("component selector is not unique")
    return matches[0]


def require_exact_image_change(before: Mapping[str, Any], after: Mapping[str, Any]) -> list[str]:
    allowed: set[str] = set()
    for collection, name, _component, _repository in COMPONENTS:
        index = _component_index(before, collection, name)
        if _component_index(after, collection, name) != index:
            fail("component ordering changed")
        before_item = before[collection][index]
        after_item = after[collection][index]
        source = set(before_item).intersection({"git", "github", "gitlab", "image"})
        if source == {"git"}:
            allowed.update(
                {
                    f"/{collection}/{index}/git",
                    f"/{collection}/{index}/dockerfile_path",
                    f"/{collection}/{index}/image",
                }
            )
        elif source == {"image"}:
            allowed.add(f"/{collection}/{index}/image/digest")
        else:
            fail("source selector is ambiguous")
    changed = set(changed_leaf_pointers(before, after))
    if changed != allowed:
        fail("candidate differs outside the four exact image bindings")
    return sorted(changed)


def environment_value_fingerprint(spec: Mapping[str, Any]) -> str:
    entries: list[dict[str, Any]] = []
    for scope in ("app", "services", "workers", "jobs", "static_sites", "functions"):
        owners = [spec] if scope == "app" else spec.get(scope, [])
        if type(owners) is not list:
            fail("environment container is malformed")
        for owner in owners:
            if type(owner) is not dict:
                fail("environment owner is malformed")
            owner_name = "app" if scope == "app" else exact_string(owner.get("name"), "environment owner name")
            envs = owner.get("envs", [])
            if type(envs) is not list:
                fail("environment list is malformed")
            for env in envs:
                if type(env) is not dict:
                    fail("environment entry is malformed")
                entries.append(
                    {
                        "scope": scope,
                        "owner": owner_name,
                        "key": env.get("key"),
                        "value_sha256": sha256_bytes(str(env.get("value", "")).encode("utf-8")),
                        "type": env.get("type"),
                        "run_scope": env.get("scope"),
                    }
                )
    return sha256_value(sorted(entries, key=lambda item: (str(item["scope"]), str(item["owner"]), str(item["key"]))))


def strip_image_sources(spec: Mapping[str, Any]) -> dict[str, Any]:
    value = copy.deepcopy(spec)
    for collection, name, _component, _repository in COMPONENTS:
        item = component_map(value, collection)[name]
        for key in ("git", "github", "gitlab", "dockerfile_path", "image"):
            item.pop(key, None)
    return value


def non_source_fingerprint(spec: Mapping[str, Any]) -> str:
    return sha256_value(strip_image_sources(spec))


def source_mode(spec: Mapping[str, Any]) -> str:
    modes: set[str] = set()
    for collection, name, _component, _repository in COMPONENTS:
        item = component_map(spec, collection)[name]
        selected = set(item).intersection({"git", "github", "gitlab", "image"})
        if selected == {"git"} and "dockerfile_path" in item:
            modes.add("legacy-git")
        elif selected == {"image"} and "dockerfile_path" not in item:
            modes.add("digest-images")
        else:
            fail("component source mode is ambiguous")
    if len(modes) != 1:
        fail("component source modes differ")
    return modes.pop()


def sanitized_image_records(digests: Mapping[str, str]) -> list[dict[str, str]]:
    return [
        {
            "component": component,
            "repository": f"ghcr.io/medtechcorps-netizen/rereply-release-{component}",
            "digest": require_digest(digests[component], "image digest"),
            "subject": f"ghcr.io/medtechcorps-netizen/rereply-release-{component}@{require_digest(digests[component], 'image digest')}",
        }
        for component in ("web", "meta-relay", "gmail-relay")
    ]


def validate_rollback_transition(current: str, target: str) -> None:
    current = validate_phase(current, "current phase")
    target = validate_phase(target, "rollback target phase")
    floor = ROLLBACK_FLOORS[current]
    if target not in floor["allowed_targets"] or target in floor["forbidden_targets"]:
        fail("rollback target violates the signed floor")


def sanitize_public(
    value: Any,
    *,
    private_values: Sequence[str] = (),
    allowed_keys: Sequence[str] = (),
) -> None:
    forbidden_keys = {
        "spec", "envs", "value", "authorization", "access_token", "token",
        "app_id", "active_deployment_id", "postgres_cluster_id", "valkey_cluster_id",
        "valkey_recovery_cluster_id", "default_ingress", "updated_at", "created_at",
        "request_path", "url", "host", "ip", "connection", "user", "password",
    }
    prefixes = ("EV[", "dop_v1_", "doo_v1_", "ghp_", "github_pat_", "-----BEGIN ")
    private = tuple(item for item in private_values if item)
    allowed = set(allowed_keys)

    def walk(item: Any) -> None:
        if type(item) is dict:
            for key, child in item.items():
                if str(key).lower() in forbidden_keys and str(key) not in allowed:
                    fail("public evidence contains a forbidden key")
                walk(child)
        elif type(item) is list:
            for child in item:
                walk(child)
        elif type(item) is str:
            lowered = item.lower()
            if any(item.startswith(prefix) for prefix in prefixes):
                fail("public evidence contains credential material")
            if any(secret in item for secret in private):
                fail("public evidence contains protected provider identity")
            if "api.digitalocean.com" in lowered or UUID_RE.search(lowered):
                fail("public evidence contains provider topology")
        elif type(item) is float or (isinstance(item, float) and not math.isfinite(item)):
            fail("public evidence contains floating point data")

    walk(value)


def _validate_public_provider_state(value: Any, label: str, *, allow_legacy: bool) -> dict[str, Any]:
    state = exact_keys(
        value,
        {
            "app_identity_sha256", "default_ingress_sha256", "app_updated_at_sha256",
            "active_deployment_identity_sha256", "canonical_spec_sha256",
            "environment_values_sha256", "non_source_projection_sha256",
            "source_mode", "images",
        },
        label,
    )
    for key in (
        "app_identity_sha256", "default_ingress_sha256", "app_updated_at_sha256",
        "active_deployment_identity_sha256", "canonical_spec_sha256",
        "environment_values_sha256", "non_source_projection_sha256",
    ):
        require_sha256(state[key], f"{label} {key}")
    allowed_modes = {"digest-images", "legacy-git"} if allow_legacy else {"digest-images"}
    if state["source_mode"] not in allowed_modes:
        fail(f"{label} source mode differs")
    images = state["images"]
    expected_count = 0 if state["source_mode"] == "legacy-git" else 3
    if type(images) is not list or len(images) != expected_count:
        fail(f"{label} image set differs")
    seen: set[str] = set()
    for item in images:
        image = exact_keys(item, {"component", "repository", "digest", "subject"}, f"{label} image")
        component = exact_string(image["component"], f"{label} image component")
        if component not in {"web", "meta-relay", "gmail-relay"} or component in seen:
            fail(f"{label} image component differs")
        seen.add(component)
        repository = f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
        digest = require_digest(image["digest"], f"{label} image digest")
        if image["repository"] != repository or image["subject"] != f"{repository}@{digest}":
            fail(f"{label} image subject differs")
    return state


def provider_states_share_semantic_lineage(
    left: Any, right: Any, *, allow_legacy: bool
) -> bool:
    """Compare complete provider states, excluding only app_updated_at_sha256."""

    left_state = _validate_public_provider_state(
        left, "left provider lineage state", allow_legacy=allow_legacy
    )
    right_state = _validate_public_provider_state(
        right, "right provider lineage state", allow_legacy=allow_legacy
    )
    return {
        key: copy.deepcopy(value)
        for key, value in left_state.items()
        if key != "app_updated_at_sha256"
    } == {
        key: copy.deepcopy(value)
        for key, value in right_state.items()
        if key != "app_updated_at_sha256"
    }


def _validate_artifact_binding(value: Any, label: str) -> dict[str, Any]:
    binding = exact_keys(value, {"run_id", "run_attempt", "artifact_id", "artifact_digest", "sha256"}, label)
    require_run_id(binding["run_id"], f"{label} run ID")
    exact_int(binding["run_attempt"], f"{label} run attempt", 1, 1)
    require_run_id(binding["artifact_id"], f"{label} artifact ID")
    require_digest(binding["artifact_digest"], f"{label} artifact digest")
    require_sha256(binding["sha256"], f"{label} file hash")
    return binding


def validate_full_artifact_binding(value: Any, label: str) -> dict[str, Any]:
    """Validate an immutable Actions artifact coordinate including its exact name."""
    binding = exact_keys(
        value,
        {
            "run_id", "run_attempt", "artifact_id", "artifact_name",
            "artifact_digest", "sha256",
        },
        label,
    )
    require_run_id(binding["run_id"], f"{label} run ID")
    exact_int(binding["run_attempt"], f"{label} run attempt", 1, 1)
    require_run_id(binding["artifact_id"], f"{label} artifact ID")
    exact_string(binding["artifact_name"], f"{label} artifact name")
    require_digest(binding["artifact_digest"], f"{label} artifact digest")
    require_sha256(binding["sha256"], f"{label} file hash")
    return binding


def _validate_intent_control(value: Any, operation: str) -> dict[str, Any]:
    control = exact_keys(
        value,
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "release_policy_sha256",
            "change_schema_sha256", "mutation_intent_schema_sha256",
            "controller_sha256",
        },
        "mutation intent control",
    )
    require_sha1(control["workflow_sha"], "mutation intent workflow SHA")
    expected_paths = (
        {".github/workflows/apply-production-phase.yml"}
        if operation == "activate"
        else {
            ".github/workflows/rollback-production-phase.yml",
            ".github/workflows/rollback-production-orphan.yml",
        }
    )
    if (
        control["workflow_path"] not in expected_paths
        or control["runner_environment"] != "github-hosted"
    ):
        fail("mutation intent workflow identity differs")
    require_run_id(control["run_id"], "mutation intent run ID")
    exact_int(control["run_attempt"], "mutation intent run attempt", 1, 1)
    for key in (
        "release_policy_sha256", "change_schema_sha256",
        "mutation_intent_schema_sha256", "controller_sha256",
    ):
        require_sha256(control[key], f"mutation intent {key}")
    return control


def _validate_intent_lineage(value: Any, operation: str) -> dict[str, Any]:
    lineage = exact_keys(
        value,
        {
            "event_sequence", "phase_ordinal", "operation", "from", "to",
            "predecessor_kind", "predecessor_state_sha256", "phase",
            "phase_source_sha",
        },
        "mutation intent lineage",
    )
    target = validate_phase(lineage["to"], "mutation intent target phase")
    source = exact_string(lineage["from"], "mutation intent source phase")
    sequence = exact_int(lineage["event_sequence"], "mutation intent event sequence", 1)
    ordinal = exact_int(lineage["phase_ordinal"], "mutation intent phase ordinal", 1, 4)
    if lineage["operation"] != operation or lineage["phase"] != target:
        fail("mutation intent lineage operation differs")
    if ordinal != PHASES.index(target) + 1:
        fail("mutation intent phase ordinal differs")
    if operation == "activate":
        if (
            source != PREDECESSOR[target]
            or (source == "genesis" and sequence != 1)
            or (source != "genesis" and sequence < 2)
        ):
            fail("mutation intent activation lineage differs")
        expected_kind = "genesis" if source == "genesis" else "phase-state"
        if lineage["predecessor_kind"] != expected_kind:
            fail("mutation intent activation predecessor differs")
    else:
        validate_rollback_transition(source, target)
        if sequence < 2 or lineage["predecessor_kind"] not in {
            "phase-state", "apply-receipt", "reconciliation-receipt",
        }:
            fail("mutation intent rollback predecessor differs")
    require_sha256(lineage["predecessor_state_sha256"], "mutation intent predecessor hash")
    require_sha1(lineage["phase_source_sha"], "mutation intent phase source SHA")
    return lineage


def _validate_predecessor_binding(value: Any, label: str) -> dict[str, Any]:
    binding = exact_keys(
        value,
        {
            "kind", "run_id", "run_attempt", "artifact_id", "artifact_name",
            "artifact_digest", "sha256",
        },
        label,
    )
    kind = exact_string(binding["kind"], f"{label} kind")
    require_sha256(binding["sha256"], f"{label} hash")
    if kind == "genesis":
        for key in ("run_id", "run_attempt", "artifact_id", "artifact_name", "artifact_digest"):
            if binding[key] is not None:
                fail(f"{label} genesis coordinate differs")
    elif kind in {"phase-state", "apply-receipt", "reconciliation-receipt"}:
        validate_full_artifact_binding(
            {key: binding[key] for key in (
                "run_id", "run_attempt", "artifact_id", "artifact_name",
                "artifact_digest", "sha256",
            )},
            label,
        )
    else:
        fail(f"{label} kind differs")
    return binding


def validate_lock_authority(value: Any, *, operation: str, control: Mapping[str, Any]) -> dict[str, Any]:
    lock = exact_keys(
        value,
        {
            "mode", "strategy", "branch", "rule_id", "rule_identity_sha256",
            "expected_pre_lock", "expected_post_lock",
            "root_acquire_intent", "owner_operation", "owner_run_id",
            "owner_run_attempt", "owner_control_sha", "owner_intent_sha256",
        },
        "mutation lock authority",
    )
    if lock["mode"] != "planned" or lock["strategy"] not in {"acquire", "inherit"} or lock["branch"] != "main":
        fail("mutation lock mode differs")
    exact_string(lock["rule_id"], "mutation lock rule ID")
    if sha256_bytes(lock["rule_id"].encode("utf-8")) != require_sha256(
        lock["rule_identity_sha256"], "mutation lock rule hash"
    ):
        fail("mutation lock rule hash differs")
    pre = exact_keys(
        lock["expected_pre_lock"],
        {"lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge"},
        "mutation expected pre-lock projection",
    )
    post = exact_keys(
        lock["expected_post_lock"],
        {"lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge"},
        "mutation expected post-lock projection",
    )
    if (
        pre["is_admin_enforced"] is not True
        or pre["lock_allows_fetch_and_merge"] is not False
        or post != {
            "lock_branch": True,
            "is_admin_enforced": True,
            "lock_allows_fetch_and_merge": False,
        }
    ):
        fail("mutation branch lock plan is not fail-closed")
    validate_full_artifact_binding(lock["root_acquire_intent"], "root lock acquire intent")
    if lock["owner_operation"] not in {"apply", "rollback"}:
        fail("mutation lock owner operation differs")
    require_run_id(lock["owner_run_id"], "mutation lock owner run ID")
    exact_int(lock["owner_run_attempt"], "mutation lock owner run attempt", 1, 1)
    require_sha1(lock["owner_control_sha"], "mutation lock owner control SHA")
    if lock["strategy"] == "acquire":
        expected_owner = "apply" if operation == "activate" else "rollback"
        if (
            pre["lock_branch"] is not False
            or
            lock["owner_operation"] != expected_owner
            or str(lock["owner_run_id"]) != str(control["run_id"])
            or lock["owner_run_attempt"] != control["run_attempt"]
            or lock["owner_control_sha"] != control["workflow_sha"]
            or lock["owner_intent_sha256"] is not None
        ):
            fail("new mutation lock ownership differs")
    else:
        require_sha256(lock["owner_intent_sha256"], "inherited owner mutation intent hash")
        if operation != "rollback" or pre["lock_branch"] is not True:
            fail("only rollback may inherit an orphan lock")
    return lock


def _validate_desired_projection(value: Any, label: str) -> dict[str, Any]:
    desired = exact_keys(
        value,
        {
            "canonical_spec_sha256", "environment_values_sha256",
            "non_source_projection_sha256", "source_mode", "images",
            "migration_job", "migration_digest",
        },
        label,
    )
    for key in (
        "canonical_spec_sha256", "environment_values_sha256",
        "non_source_projection_sha256",
    ):
        require_sha256(desired[key], f"{label} {key}")
    if desired["source_mode"] != "digest-images" or desired["migration_job"] != "rereply-rls-migrate":
        fail(f"{label} source or migration authority differs")
    if type(desired["images"]) is not list or len(desired["images"]) != 3:
        fail(f"{label} image inventory differs")
    image_state = {
        "app_identity_sha256": "0" * 64,
        "default_ingress_sha256": "0" * 64,
        "app_updated_at_sha256": "0" * 64,
        "active_deployment_identity_sha256": "0" * 64,
        "canonical_spec_sha256": desired["canonical_spec_sha256"],
        "environment_values_sha256": desired["environment_values_sha256"],
        "non_source_projection_sha256": desired["non_source_projection_sha256"],
        "source_mode": "digest-images",
        "images": desired["images"],
    }
    _validate_public_provider_state(image_state, label, allow_legacy=False)
    migration = require_digest(desired["migration_digest"], f"{label} migration digest")
    web = next(item for item in desired["images"] if item["component"] == "web")
    if migration != web["digest"]:
        fail(f"{label} migration image differs")
    return desired


def validate_mutation_intent(value: Any, *, now: dt.datetime | None = None) -> dict[str, Any]:
    """Validate the durable, sanitized authority that must exist before a PUT."""
    intent = exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "prepared_at",
            "expires_at", "control", "operation", "lineage", "authorities",
            "lock", "before", "desired", "mutation", "rollback", "canary",
        },
        "production mutation intent",
    )
    exact_int(intent["schema_version"], "mutation intent schema", 2, 2)
    if intent["authority"] != "production-mutation-intent" or intent["repository"] != REPOSITORY:
        fail("mutation intent authority differs")
    operation = exact_string(intent["operation"], "mutation intent operation")
    if operation not in {"activate", "rollback"}:
        fail("mutation intent operation differs")
    prepared = require_timestamp(intent["prepared_at"], "mutation intent preparation time")
    expires = require_timestamp(intent["expires_at"], "mutation intent expiry")
    if expires <= prepared or (expires - prepared).total_seconds() > MAX_PLAN_AGE_SECONDS:
        fail("mutation intent validity window differs")
    if now is not None:
        if now.tzinfo is None or now.utcoffset() is None:
            fail("mutation intent clock is invalid")
        checked = now.astimezone(dt.timezone.utc)
        if checked < prepared or checked >= expires:
            fail("mutation intent is not currently valid")
    control = _validate_intent_control(intent["control"], operation)
    lineage = _validate_intent_lineage(intent["lineage"], operation)
    authorities = intent["authorities"]
    if operation == "activate":
        authorities = exact_keys(
            authorities,
            {
                "rollout_plan_sha256", "rollout_authority", "production_plan",
                "recovery", "predecessor_state",
            },
            "apply mutation authorities",
        )
        require_sha256(authorities["rollout_plan_sha256"], "intent rollout plan hash")
        for key in ("rollout_authority", "production_plan", "recovery"):
            validate_full_artifact_binding(authorities[key], f"intent {key}")
        predecessor = _validate_predecessor_binding(
            authorities["predecessor_state"], "intent predecessor state"
        )
        if (
            predecessor["kind"] != lineage["predecessor_kind"]
            or predecessor["sha256"] != lineage["predecessor_state_sha256"]
        ):
            fail("intent predecessor authority differs")
    else:
        authorities = exact_keys(
            authorities,
            {
                "rollout_plan_sha256", "current_state", "target_state",
                "recovery", "target_authority",
            },
            "rollback mutation authorities",
        )
        require_sha256(authorities["rollout_plan_sha256"], "intent rollout plan hash")
        _validate_predecessor_binding(authorities["current_state"], "intent current state")
        validate_full_artifact_binding(authorities["target_state"], "intent target state")
        validate_full_artifact_binding(authorities["recovery"], "intent recovery")
        target_authority = exact_keys(
            authorities["target_authority"], {"production_plan_sha256"},
            "intent rollback target authority",
        )
        require_sha256(target_authority["production_plan_sha256"], "intent target plan hash")
    lock = validate_lock_authority(intent["lock"], operation=operation, control=control)
    if operation == "rollback":
        expected_path = (
            ".github/workflows/rollback-production-orphan.yml"
            if lock["strategy"] == "inherit"
            else ".github/workflows/rollback-production-phase.yml"
        )
        if control["workflow_path"] != expected_path:
            fail("rollback workflow does not match its lock authority")
    before = _validate_public_provider_state(
        intent["before"], "mutation intent before", allow_legacy=operation == "activate"
    )
    desired = _validate_desired_projection(intent["desired"], "mutation intent desired")
    for key in ("environment_values_sha256", "non_source_projection_sha256"):
        if before[key] != desired[key]:
            fail("mutation intent does not preserve production state")
    if before["canonical_spec_sha256"] == desired["canonical_spec_sha256"]:
        fail("mutation intent is not a source change")
    mutation = exact_keys(
        intent["mutation"],
        {
            "http_method", "endpoint_label", "update_all_source_versions",
            "before_sha256", "desired_sha256", "mutation_fingerprint_sha256",
        },
        "mutation intent request",
    )
    if (
        mutation["http_method"] != "PUT"
        or mutation["endpoint_label"] != "app"
        or mutation["update_all_source_versions"] is not False
        or mutation["before_sha256"] != sha256_value(before)
        or mutation["desired_sha256"] != sha256_value(desired)
    ):
        fail("mutation intent request binding differs")
    expected_fingerprint = sha256_value(
        {
            "before_sha256": mutation["before_sha256"],
            "desired_sha256": mutation["desired_sha256"],
            "http_method": "PUT",
            "endpoint_label": "app",
            "update_all_source_versions": False,
        }
    )
    if mutation["mutation_fingerprint_sha256"] != expected_fingerprint:
        fail("mutation intent fingerprint differs")
    target = lineage["to"]
    if intent["rollback"] != ROLLBACK_FLOORS[target]:
        fail("mutation intent rollback floor differs")
    canary = exact_keys(
        intent["canary"],
        {"required", "completed", "endpoint_labels", "route_contract_sha256"},
        "mutation intent canary",
    )
    if (
        canary["required"] is not True
        or canary["completed"] is not False
        or canary["endpoint_labels"] != [
            "app-health", "app-ready", "meta-live", "meta-ready",
            "gmail-live", "gmail-ready",
        ]
    ):
        fail("mutation intent canary contract differs")
    require_sha256(canary["route_contract_sha256"], "mutation intent route hash")
    sanitize_public(intent)
    return intent


LOCK_PROOF_WORKFLOWS = {
    "apply": ".github/workflows/apply-production-phase.yml",
    "rollback": ".github/workflows/rollback-production-phase.yml",
    "orphan-rollback": ".github/workflows/rollback-production-orphan.yml",
}


def _validate_lock_projection(value: Any, label: str) -> dict[str, Any]:
    projection = exact_keys(
        value,
        {"lock_branch", "is_admin_enforced", "lock_allows_fetch_and_merge"},
        label,
    )
    for key in projection:
        exact_bool(projection[key], f"{label} {key}")
    if (
        projection["is_admin_enforced"] is not True
        or projection["lock_allows_fetch_and_merge"] is not False
    ):
        fail(f"{label} is not fail-closed")
    return projection


def validate_main_lock_proof(
    value: Any,
    *,
    mutation_intent: Mapping[str, Any],
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    """Validate the separately signed post-lock authority required before PUT."""
    intent = validate_mutation_intent(mutation_intent, now=now)
    proof = exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "created_at", "control",
            "operation", "mutation_intent", "root_acquire_intent", "branch",
            "acquisition",
        },
        "production main lock proof",
    )
    exact_int(proof["schema_version"], "main lock proof schema", 1, 1)
    if proof["authority"] != "production-main-lock-proof" or proof["repository"] != REPOSITORY:
        fail("main lock proof authority differs")
    created = require_timestamp(proof["created_at"], "main lock proof creation time")
    if created < require_timestamp(intent["prepared_at"], "intent preparation time") or created > require_timestamp(intent["expires_at"], "intent expiry"):
        fail("main lock proof is outside the mutation intent validity window")
    if now is not None:
        checked = now.astimezone(dt.timezone.utc).replace(microsecond=0)
        if created > checked or checked > require_timestamp(intent["expires_at"], "intent expiry"):
            fail("main lock proof is stale or future-dated")
    operation = exact_string(proof["operation"], "main lock proof operation")
    if operation not in LOCK_PROOF_WORKFLOWS:
        fail("main lock proof operation differs")
    expected_operation = "apply" if intent["operation"] == "activate" else (
        "orphan-rollback" if intent["lock"]["strategy"] == "inherit" else "rollback"
    )
    if operation != expected_operation:
        fail("main lock proof operation differs from the mutation intent")
    control = exact_keys(
        proof["control"],
        {"workflow_sha", "workflow_path", "run_id", "run_attempt", "runner_environment"},
        "main lock proof control",
    )
    require_sha1(control["workflow_sha"], "main lock proof workflow SHA")
    require_run_id(control["run_id"], "main lock proof run ID")
    exact_int(control["run_attempt"], "main lock proof run attempt", 1, 1)
    if (
        control["workflow_path"] != LOCK_PROOF_WORKFLOWS[operation]
        or control["workflow_path"] != intent["control"]["workflow_path"]
        or control["workflow_sha"] != intent["control"]["workflow_sha"]
        or str(control["run_id"]) != str(intent["control"]["run_id"])
        or control["run_attempt"] != intent["control"]["run_attempt"]
        or control["runner_environment"] != "github-hosted"
    ):
        fail("main lock proof control differs from the mutation intent")
    intent_binding = validate_full_artifact_binding(
        proof["mutation_intent"], "main lock proof mutation intent"
    )
    intent_hash = sha256_bytes(canonical_file_bytes(intent))
    expected_intent_name = f"production-mutation-intent-{operation}-{intent_binding['run_id']}-1"
    if (
        intent_binding["sha256"] != intent_hash
        or intent_binding["artifact_name"] != expected_intent_name
        or str(intent_binding["run_id"]) != str(control["run_id"])
        or intent_binding["run_attempt"] != control["run_attempt"]
    ):
        fail("main lock proof mutation intent binding differs")
    root = validate_full_artifact_binding(
        proof["root_acquire_intent"], "main lock proof root acquire intent"
    )
    if root != intent["lock"]["root_acquire_intent"]:
        fail("main lock proof root acquire intent differs")
    branch = exact_keys(
        proof["branch"],
        {
            "strategy", "main_sha", "rule_id", "rule_identity_sha256",
            "pre_lock", "post_lock",
        },
        "main lock proof branch",
    )
    pre = _validate_lock_projection(branch["pre_lock"], "main lock proof pre-lock")
    post = _validate_lock_projection(branch["post_lock"], "main lock proof post-lock")
    if (
        branch["strategy"] != intent["lock"]["strategy"]
        or branch["main_sha"] != control["workflow_sha"]
        or branch["rule_id"] != intent["lock"]["rule_id"]
        or branch["rule_identity_sha256"] != intent["lock"]["rule_identity_sha256"]
        or pre != intent["lock"]["expected_pre_lock"]
        or post != intent["lock"]["expected_post_lock"]
        or post["lock_branch"] is not True
    ):
        fail("main lock proof branch projection differs from the mutation intent")
    acquisition = exact_keys(
        proof["acquisition"],
        {
            "http_methods_used", "graphql_operations_used", "mutation_request_count",
            "outcome", "mutation_fingerprint_sha256", "read_confirmed",
        },
        "main lock proof acquisition",
    )
    if acquisition["http_methods_used"] != ["POST"] or acquisition["read_confirmed"] is not True:
        fail("main lock proof observation ledger differs")
    strategy = branch["strategy"]
    if strategy == "acquire":
        expected_graphql = ["query", "mutation", "query"]
        expected_outcomes = {"applied", "ambiguous-reconciled"}
        expected_count = 1
    else:
        expected_graphql = ["query"]
        expected_outcomes = {"already-locked-inherited"}
        expected_count = 0
    if (
        acquisition["graphql_operations_used"] != expected_graphql
        or acquisition["outcome"] not in expected_outcomes
        or acquisition["mutation_request_count"] != expected_count
    ):
        fail("main lock proof acquisition semantics differ")
    fingerprint_payload = {
        "intent_sha256": intent_hash,
        "root_acquire_intent_sha256": root["sha256"],
        "main_sha": branch["main_sha"],
        "rule_identity_sha256": branch["rule_identity_sha256"],
        "strategy": strategy,
        "pre_lock": pre,
        "post_lock": post,
        "graphql_operations_used": acquisition["graphql_operations_used"],
        "mutation_request_count": acquisition["mutation_request_count"],
        "outcome": acquisition["outcome"],
        "read_confirmed": True,
    }
    if acquisition["mutation_fingerprint_sha256"] != sha256_value(fingerprint_payload):
        fail("main lock proof mutation fingerprint differs")
    sanitize_public(proof, allowed_keys=("created_at",))
    return proof


def build_main_lock_proof(
    *,
    request: Mapping[str, Any],
    mutation_intent: Mapping[str, Any],
    mutation_intent_binding: Mapping[str, Any],
    control: Mapping[str, Any],
    now: dt.datetime,
) -> dict[str, Any]:
    intent = validate_mutation_intent(mutation_intent, now=now)
    request = exact_keys(
        request,
        {
            "operation", "main_sha", "rule_id", "rule_identity_sha256",
            "pre_lock", "post_lock", "http_methods_used",
            "graphql_operations_used", "mutation_request_count", "outcome",
            "read_confirmed",
        },
        "main lock proof request",
    )
    root = copy.deepcopy(intent["lock"]["root_acquire_intent"])
    fingerprint_payload = {
        "intent_sha256": sha256_bytes(canonical_file_bytes(intent)),
        "root_acquire_intent_sha256": root["sha256"],
        "main_sha": request["main_sha"],
        "rule_identity_sha256": request["rule_identity_sha256"],
        "strategy": intent["lock"]["strategy"],
        "pre_lock": request["pre_lock"],
        "post_lock": request["post_lock"],
        "graphql_operations_used": request["graphql_operations_used"],
        "mutation_request_count": request["mutation_request_count"],
        "outcome": request["outcome"],
        "read_confirmed": request["read_confirmed"],
    }
    proof = {
        "schema_version": 1,
        "authority": "production-main-lock-proof",
        "repository": REPOSITORY,
        "created_at": format_timestamp(now),
        "control": dict(control),
        "operation": request["operation"],
        "mutation_intent": dict(mutation_intent_binding),
        "root_acquire_intent": root,
        "branch": {
            "strategy": intent["lock"]["strategy"],
            "main_sha": request["main_sha"],
            "rule_id": request["rule_id"],
            "rule_identity_sha256": request["rule_identity_sha256"],
            "pre_lock": copy.deepcopy(request["pre_lock"]),
            "post_lock": copy.deepcopy(request["post_lock"]),
        },
        "acquisition": {
            "http_methods_used": copy.deepcopy(request["http_methods_used"]),
            "graphql_operations_used": copy.deepcopy(request["graphql_operations_used"]),
            "mutation_request_count": request["mutation_request_count"],
            "outcome": request["outcome"],
            "mutation_fingerprint_sha256": sha256_value(fingerprint_payload),
            "read_confirmed": request["read_confirmed"],
        },
    }
    return validate_main_lock_proof(proof, mutation_intent=intent, now=now)


RECONCILIATION_OUTCOMES = {
    "committed": {
        "terminal": True,
        "canary_eligible": True,
        "original_receipt_present": False,
        "reason": "desired-active-without-receipt",
    },
    "already-receipted": {
        "terminal": True,
        "canary_eligible": True,
        "original_receipt_present": True,
        "reason": "desired-active-with-signed-receipt",
    },
    "no-mutation": {
        "terminal": True,
        "canary_eligible": False,
        "original_receipt_present": False,
        "reason": "exact-before-no-provider-transition",
    },
    "pending": {
        "terminal": False,
        "canary_eligible": False,
        "original_receipt_present": False,
        "reason": "desired-provider-transition-pending",
    },
    "indeterminate": {
        "terminal": False,
        "canary_eligible": False,
        "reason": "provider-state-indeterminate",
    },
}


ORIGINAL_PROVIDER_JOBS = {
    ".github/workflows/apply-production-phase.yml": (
        "Apply exact production phase",
        "Apply with one isolated app-update capability",
    ),
    ".github/workflows/rollback-production-phase.yml": (
        "Roll back exact production phase",
        "Roll back with one isolated app-update capability",
    ),
    ".github/workflows/rollback-production-orphan.yml": (
        "Roll back exact production orphan",
        "Roll back with one inherited-lock app-update capability",
    ),
}
JOB_CONCLUSIONS = {
    "action_required", "cancelled", "failure", "neutral", "skipped",
    "stale", "success", "timed_out",
}


def project_original_provider_job(value: Any, *, workflow_path: str) -> dict[str, Any]:
    """Reduce an exact GitHub job response to signed, content-free evidence.

    The raw timing and step inventory are accepted only inside the tokenless
    assertion builder.  Public evidence carries their canonical hashes and a
    fail-closed `never_started` result; no runner identity or raw timestamp is
    retained.
    """
    if workflow_path not in ORIGINAL_PROVIDER_JOBS:
        fail("original provider workflow differs")
    expected_job, expected_step = ORIGINAL_PROVIDER_JOBS[workflow_path]
    job = exact_keys(
        value,
        {
            "job_id", "job_name", "status", "conclusion", "started_at",
            "completed_at", "steps",
        },
        "original provider job response",
    )
    job_id = require_run_id(job["job_id"], "original provider job ID")
    if job["job_name"] != expected_job or job["status"] != "completed":
        fail("original provider job identity or status differs")
    conclusion = exact_string(job["conclusion"], "original provider job conclusion")
    if conclusion not in JOB_CONCLUSIONS:
        fail("original provider job conclusion differs")
    timing: dict[str, Any] = {}
    for key in ("started_at", "completed_at"):
        raw = job[key]
        if raw is not None:
            require_timestamp(raw, f"original provider job {key}")
        timing[key] = raw
    steps = job["steps"]
    if type(steps) is not list or len(steps) > 100:
        fail("original provider job step inventory differs")
    normalized_steps: list[dict[str, Any]] = []
    numbers: set[int] = set()
    names: set[str] = set()
    for raw in steps:
        step = exact_keys(
            raw, {"number", "name", "status", "conclusion"},
            "original provider job step",
        )
        number = exact_int(step["number"], "original provider step number", 1, 1000)
        name = exact_string(step["name"], "original provider step name")
        status = exact_string(step["status"], "original provider step status")
        if status not in {"queued", "in_progress", "completed"}:
            fail("original provider step status differs")
        step_conclusion = step["conclusion"]
        if step_conclusion is not None:
            step_conclusion = exact_string(
                step_conclusion, "original provider step conclusion"
            )
            if step_conclusion not in JOB_CONCLUSIONS:
                fail("original provider step conclusion differs")
        if number in numbers or name in names:
            fail("original provider job step inventory is ambiguous")
        numbers.add(number)
        names.add(name)
        normalized_steps.append(
            {
                "number": number,
                "name": name,
                "status": status,
                "conclusion": step_conclusion,
            }
        )
    provider_steps = [step for step in normalized_steps if step["name"] == expected_step]
    if len(provider_steps) > 1:
        fail("original provider capability step is ambiguous")
    provider_step = provider_steps[0] if provider_steps else None
    all_steps_skipped = all(
        step["status"] == "completed" and step["conclusion"] == "skipped"
        for step in normalized_steps
    )
    never_started = bool(
        conclusion == "skipped"
        and all_steps_skipped
        and (
            not normalized_steps
            or (
                provider_step is not None
                and provider_step["status"] == "completed"
                and provider_step["conclusion"] == "skipped"
            )
        )
    )
    evidence = {
        "job_id": job_id,
        "job_name": expected_job,
        "status": "completed",
        "conclusion": conclusion,
        "timing_sha256": sha256_value(timing),
        "step_inventory_sha256": sha256_value(normalized_steps),
        "step_count": len(normalized_steps),
        "all_steps_skipped": all_steps_skipped,
        "provider_step_name": expected_step,
        "provider_step_status": provider_step["status"] if provider_step else None,
        "provider_step_conclusion": provider_step["conclusion"] if provider_step else None,
        "never_started": never_started,
    }
    return validate_original_provider_job(evidence, workflow_path=workflow_path)


def validate_original_provider_job(value: Any, *, workflow_path: str) -> dict[str, Any]:
    if workflow_path not in ORIGINAL_PROVIDER_JOBS:
        fail("original provider workflow differs")
    expected_job, expected_step = ORIGINAL_PROVIDER_JOBS[workflow_path]
    evidence = exact_keys(
        value,
        {
            "job_id", "job_name", "status", "conclusion", "timing_sha256",
            "step_inventory_sha256", "step_count", "all_steps_skipped",
            "provider_step_name", "provider_step_status",
            "provider_step_conclusion", "never_started",
        },
        "original provider job evidence",
    )
    require_run_id(evidence["job_id"], "original provider job ID")
    if evidence["job_name"] != expected_job or evidence["status"] != "completed":
        fail("original provider job evidence identity differs")
    if evidence["conclusion"] not in JOB_CONCLUSIONS:
        fail("original provider job evidence conclusion differs")
    require_sha256(evidence["timing_sha256"], "original provider timing hash")
    require_sha256(evidence["step_inventory_sha256"], "original provider step hash")
    step_count = exact_int(evidence["step_count"], "original provider step count", 0, 100)
    all_skipped = exact_bool(
        evidence["all_steps_skipped"], "original provider all-skipped result"
    )
    if evidence["provider_step_name"] != expected_step:
        fail("original provider capability step identity differs")
    provider_status = evidence["provider_step_status"]
    provider_conclusion = evidence["provider_step_conclusion"]
    if (provider_status is None) is not (provider_conclusion is None):
        fail("original provider capability step result is incomplete")
    if provider_status is not None:
        if provider_status not in {"queued", "in_progress", "completed"}:
            fail("original provider capability step status differs")
        if provider_conclusion not in JOB_CONCLUSIONS:
            fail("original provider capability step conclusion differs")
    never_started = exact_bool(evidence["never_started"], "original provider never-started result")
    expected_never_started = bool(
        evidence["conclusion"] == "skipped"
        and all_skipped
        and (
            (step_count == 0 and provider_status is None)
            or (
                step_count > 0
                and provider_status == "completed"
                and provider_conclusion == "skipped"
            )
        )
    )
    if never_started is not expected_never_started:
        fail("original provider never-started result differs")
    if step_count == 0 and provider_status is not None:
        fail("empty original provider step inventory contains a capability result")
    sanitize_public(evidence)
    return evidence


def _validate_reconciliation_original_receipt(value: Any) -> dict[str, Any] | None:
    if value is None:
        return None
    receipt = exact_keys(
        value, {"kind", "workflow_path", "binding"},
        "reconciliation original receipt authority",
    )
    paths = {
        "apply": ".github/workflows/apply-production-phase.yml",
        "rollback": ".github/workflows/rollback-production-phase.yml",
        "orphan-rollback": ".github/workflows/rollback-production-orphan.yml",
    }
    if receipt["kind"] not in paths or receipt["workflow_path"] != paths[receipt["kind"]]:
        fail("reconciliation original receipt signer differs")
    validate_full_artifact_binding(receipt["binding"], "reconciliation original receipt")
    return receipt


def validate_reconciliation_receipt(value: Any) -> dict[str, Any]:
    """Validate a GET-only orphan classification; it never implies an unlock."""
    receipt = exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "completed_at",
            "control", "intent", "lock_assertion", "lineage", "authorities",
            "classification", "provider_observation", "before", "desired",
            "after", "gates", "rollback", "canary",
        },
        "production orphan reconciliation receipt",
    )
    exact_int(receipt["schema_version"], "reconciliation schema", 1, 1)
    if (
        receipt["authority"] != "production-orphan-reconciliation-receipt"
        or receipt["repository"] != REPOSITORY
    ):
        fail("reconciliation receipt authority differs")
    require_timestamp(receipt["completed_at"], "reconciliation completion time")
    control = exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt",
            "runner_environment", "release_policy_sha256",
            "change_schema_sha256", "mutation_intent_schema_sha256",
            "reconciliation_schema_sha256", "controller_sha256",
        },
        "reconciliation control",
    )
    require_sha1(control["workflow_sha"], "reconciliation workflow SHA")
    if (
        control["workflow_path"] != ".github/workflows/reconcile-production-orphan.yml"
        or control["runner_environment"] != "github-hosted"
    ):
        fail("reconciliation workflow identity differs")
    require_run_id(control["run_id"], "reconciliation run ID")
    exact_int(control["run_attempt"], "reconciliation run attempt", 1, 1)
    for key in (
        "release_policy_sha256", "change_schema_sha256",
        "mutation_intent_schema_sha256", "reconciliation_schema_sha256",
        "controller_sha256",
    ):
        require_sha256(control[key], f"reconciliation {key}")
    intent = exact_keys(
        receipt["intent"],
        {"schema_version", "operation", "workflow_path", "binding", "lock"},
        "reconciliation mutation intent authority",
    )
    exact_int(intent["schema_version"], "reconciliation intent schema", 1, 2)
    if intent["operation"] not in {"activate", "rollback"}:
        fail("reconciliation intent operation differs")
    allowed_paths = {
        "activate": {".github/workflows/apply-production-phase.yml"},
        "rollback": {
            ".github/workflows/rollback-production-phase.yml",
            ".github/workflows/rollback-production-orphan.yml",
        },
    }
    if intent["workflow_path"] not in allowed_paths[intent["operation"]]:
        fail("reconciliation intent workflow differs")
    validate_full_artifact_binding(intent["binding"], "reconciliation mutation intent")
    assertion = exact_keys(
        receipt["lock_assertion"],
        {
            "authority", "actor_provenance", "original_workflow_path",
            "original_control_sha", "original_run_id", "original_run_attempt",
            "rule_id", "rule_identity_sha256", "current_main_sha",
            "mutation_intent_sha256", "typed_confirmation_sha256",
            "original_provider_job", "binding",
        },
        "reconciliation lock assertion",
    )
    if (
        assertion["authority"] != "production-main-lock-ownership-assertion"
        or assertion["actor_provenance"]
        != "single-operator-assertion-not-audit-log"
        or assertion["original_workflow_path"] != intent["workflow_path"]
    ):
        fail("reconciliation lock assertion authority differs")
    require_sha1(assertion["original_control_sha"], "lock assertion control SHA")
    require_run_id(assertion["original_run_id"], "lock assertion original run ID")
    exact_int(assertion["original_run_attempt"], "lock assertion original attempt", 1, 1)
    exact_string(assertion["rule_id"], "lock assertion rule ID")
    if sha256_bytes(assertion["rule_id"].encode("utf-8")) != require_sha256(
        assertion["rule_identity_sha256"], "lock assertion rule hash"
    ):
        fail("lock assertion rule hash differs")
    require_sha1(assertion["current_main_sha"], "lock assertion current main SHA")
    require_sha256(assertion["mutation_intent_sha256"], "lock assertion intent hash")
    require_sha256(assertion["typed_confirmation_sha256"], "typed confirmation hash")
    provider_job = validate_original_provider_job(
        assertion["original_provider_job"], workflow_path=intent["workflow_path"]
    )
    validate_full_artifact_binding(assertion["binding"], "lock assertion artifact")
    if (
        str(assertion["original_run_id"]) != str(intent["binding"]["run_id"])
        or assertion["original_run_attempt"] != intent["binding"]["run_attempt"]
        or assertion["mutation_intent_sha256"] != intent["binding"]["sha256"]
        or assertion["original_control_sha"] != control["workflow_sha"]
        or assertion["current_main_sha"] != control["workflow_sha"]
    ):
        fail("lock assertion does not bind the exact mutation intent")
    operation = intent["operation"]
    lock = validate_lock_authority(
        intent["lock"],
        operation=operation,
        control={
            "workflow_sha": assertion["original_control_sha"],
            "run_id": assertion["original_run_id"],
            "run_attempt": assertion["original_run_attempt"],
        },
    )
    if (
        lock["rule_id"] != assertion["rule_id"]
        or lock["rule_identity_sha256"] != assertion["rule_identity_sha256"]
    ):
        fail("reconciliation intent lock differs from the asserted rule")
    expected_intent_path = (
        ".github/workflows/apply-production-phase.yml"
        if operation == "activate"
        else (
            ".github/workflows/rollback-production-orphan.yml"
            if lock["strategy"] == "inherit"
            else ".github/workflows/rollback-production-phase.yml"
        )
    )
    if intent["workflow_path"] != expected_intent_path:
        fail("reconciliation intent path differs from its lock plan")
    lineage = _validate_intent_lineage(receipt["lineage"], operation)
    authorities = exact_keys(
        receipt["authorities"], {"upstream", "original_receipt"},
        "reconciliation authorities",
    )
    original = _validate_reconciliation_original_receipt(authorities["original_receipt"])
    upstream = authorities["upstream"]
    if operation == "activate":
        upstream = exact_keys(
            upstream,
            {
                "rollout_plan_sha256", "rollout_authority", "production_plan",
                "recovery", "predecessor_state",
            },
            "reconciliation apply upstream authorities",
        )
        require_sha256(upstream["rollout_plan_sha256"], "reconciliation rollout hash")
        for key in ("rollout_authority", "production_plan", "recovery"):
            validate_full_artifact_binding(upstream[key], f"reconciliation {key}")
        predecessor = _validate_predecessor_binding(
            upstream["predecessor_state"], "reconciliation predecessor"
        )
        if (
            predecessor["kind"] != lineage["predecessor_kind"]
            or predecessor["sha256"] != lineage["predecessor_state_sha256"]
        ):
            fail("reconciliation apply predecessor differs")
    else:
        upstream = exact_keys(
            upstream,
            {
                "rollout_plan_sha256", "current_state", "target_state",
                "recovery", "target_authority",
            },
            "reconciliation rollback upstream authorities",
        )
        require_sha256(upstream["rollout_plan_sha256"], "reconciliation rollout hash")
        _validate_predecessor_binding(upstream["current_state"], "reconciliation current state")
        validate_full_artifact_binding(upstream["target_state"], "reconciliation target state")
        validate_full_artifact_binding(upstream["recovery"], "reconciliation recovery")
        target_authority = exact_keys(
            upstream["target_authority"], {"production_plan_sha256"},
            "reconciliation target authority",
        )
        require_sha256(target_authority["production_plan_sha256"], "reconciliation target plan hash")
    classification = exact_keys(
        receipt["classification"],
        {
            "outcome", "terminal", "canary_eligible",
            "original_receipt_present", "reason",
        },
        "reconciliation classification",
    )
    outcome = exact_string(classification["outcome"], "reconciliation outcome")
    if outcome not in RECONCILIATION_OUTCOMES:
        fail("reconciliation outcome differs")
    expected = RECONCILIATION_OUTCOMES[outcome]
    for key, expected_value in expected.items():
        if classification[key] != expected_value:
            fail("reconciliation classification semantics differ")
    if classification["original_receipt_present"] is not (original is not None):
        fail("reconciliation original receipt presence differs")
    if intent["schema_version"] == 1 and outcome != "already-receipted":
        fail("legacy mutation evidence requires an exact signed receipt")
    observation = exact_keys(
        receipt["provider_observation"],
        {
            "http_methods_used", "http_request_count", "mutation_request_count",
            "endpoint_labels", "observation_rounds", "double_read_equal",
            "app_spec_matches_active_deployment", "transition_absent",
            "migration_succeeded",
        },
        "reconciliation provider observation",
    )
    if (
        observation["http_methods_used"] != ["GET"]
        or observation["mutation_request_count"] != 0
        or observation["endpoint_labels"] != ["app", "deployment"]
        or observation["observation_rounds"] != 2
        or observation["double_read_equal"] is not True
        or observation["app_spec_matches_active_deployment"] is not True
    ):
        fail("reconciliation provider ledger is not GET-only and complete")
    exact_int(observation["http_request_count"], "reconciliation request count", 4, 4)
    exact_bool(observation["transition_absent"], "reconciliation transition proof")
    exact_bool(observation["migration_succeeded"], "reconciliation migration proof")
    before = _validate_public_provider_state(
        receipt["before"], "reconciliation before", allow_legacy=operation == "activate"
    )
    desired = _validate_desired_projection(receipt["desired"], "reconciliation desired")
    after = _validate_public_provider_state(
        receipt["after"], "reconciliation after", allow_legacy=operation == "activate"
    )
    desired_match = (
        after["canonical_spec_sha256"] == desired["canonical_spec_sha256"]
        and after["environment_values_sha256"] == desired["environment_values_sha256"]
        and after["non_source_projection_sha256"] == desired["non_source_projection_sha256"]
        and after["source_mode"] == "digest-images"
        and after["images"] == desired["images"]
        and after["app_identity_sha256"] == before["app_identity_sha256"]
        and after["default_ingress_sha256"] == before["default_ingress_sha256"]
    )
    if outcome in {"committed", "already-receipted"}:
        if (
            not desired_match
            or observation["transition_absent"] is not True
            or observation["migration_succeeded"] is not True
        ):
            fail("reconciled desired state is incomplete")
    elif outcome == "no-mutation":
        if (
            not provider_states_share_semantic_lineage(
                after, before, allow_legacy=operation == "activate"
            )
            or observation["transition_absent"] is not True
            or provider_job["never_started"] is not True
        ):
            fail("reconciliation no-mutation proof differs")
    elif outcome == "pending":
        if observation["transition_absent"] is not False:
            fail("reconciliation pending proof differs")
    gates = exact_keys(
        receipt["gates"],
        {
            "artifacts_authenticated", "main_unchanged", "lock_owned",
            "get_only", "double_read_complete",
            "app_spec_matches_active_deployment", "deployment_succeeded",
            "migration_succeeded",
        },
        "reconciliation gates",
    )
    for key in (
        "artifacts_authenticated", "main_unchanged", "lock_owned",
        "get_only", "double_read_complete", "app_spec_matches_active_deployment",
    ):
        if gates[key] is not True:
            fail("reconciliation authority gate is incomplete")
    should_succeed = outcome in {"committed", "already-receipted"}
    if (
        gates["deployment_succeeded"] is not should_succeed
        or gates["migration_succeeded"] is not should_succeed
    ):
        fail("reconciliation deployment gates differ")
    target = lineage["to"]
    if receipt["rollback"] != ROLLBACK_FLOORS[target]:
        fail("reconciliation rollback floor differs")
    canary = exact_keys(
        receipt["canary"],
        {
            "required", "eligible", "completed", "endpoint_labels",
            "route_contract_sha256",
        },
        "reconciliation canary contract",
    )
    if (
        canary["required"] is not should_succeed
        or canary["eligible"] is not should_succeed
        or canary["completed"] is not False
        or canary["endpoint_labels"] != [
            "app-health", "app-ready", "meta-live", "meta-ready",
            "gmail-live", "gmail-ready",
        ]
    ):
        fail("reconciliation canary eligibility differs")
    require_sha256(canary["route_contract_sha256"], "reconciliation route hash")
    sanitize_public(receipt)
    return receipt


def validate_apply_receipt(value: Any) -> dict[str, Any]:
    receipt = exact_keys(
        value,
        {
            "schema_version", "authority", "repository", "completed_at", "control",
            "lineage", "authorities", "provider_transition", "before", "after",
            "gates", "rollback", "canary",
        },
        "production apply receipt",
    )
    exact_int(receipt["schema_version"], "apply receipt schema", 1, 1)
    if receipt["authority"] != "production-phase-apply-receipt" or receipt["repository"] != REPOSITORY:
        fail("apply receipt authority differs")
    require_timestamp(receipt["completed_at"], "apply completion time")
    control = exact_keys(
        receipt["control"],
        {
            "workflow_sha", "workflow_path", "run_id", "run_attempt", "runner_environment",
            "release_policy_sha256", "change_schema_sha256", "controller_sha256",
        },
        "apply receipt control",
    )
    require_sha1(control["workflow_sha"], "apply workflow SHA")
    if control["workflow_path"] != ".github/workflows/apply-production-phase.yml":
        fail("apply workflow path differs")
    require_run_id(control["run_id"], "apply run ID")
    exact_int(control["run_attempt"], "apply run attempt", 1, 1)
    if control["runner_environment"] != "github-hosted":
        fail("apply runner differs")
    for key in ("release_policy_sha256", "change_schema_sha256", "controller_sha256"):
        require_sha256(control[key], f"apply {key}")
    lineage = exact_keys(
        receipt["lineage"],
        {
            "event_sequence", "phase_ordinal", "operation", "from", "to", "predecessor_kind",
            "predecessor_state_sha256", "phase", "phase_source_sha",
        },
        "apply lineage",
    )
    target = validate_phase(lineage["to"], "apply target phase")
    source = exact_string(lineage["from"], "apply source phase")
    sequence = exact_int(lineage["event_sequence"], "apply event sequence", 1)
    ordinal = exact_int(lineage["phase_ordinal"], "apply phase ordinal", 1, 4)
    if (
        lineage["operation"] != "activate"
        or lineage["phase"] != target
        or source != PREDECESSOR[target]
        or ordinal != PHASES.index(target) + 1
        or (source == "genesis" and sequence != 1)
        or (source != "genesis" and sequence < 2)
    ):
        fail("apply lineage sequence differs")
    expected_kind = "genesis" if source == "genesis" else "phase-state"
    if lineage["predecessor_kind"] != expected_kind:
        fail("apply predecessor kind differs")
    predecessor_hash = lineage["predecessor_state_sha256"]
    require_sha256(predecessor_hash, "apply predecessor state hash")
    require_sha1(lineage["phase_source_sha"], "apply phase source SHA")
    authorities = exact_keys(
        receipt["authorities"],
        {
            "rollout_plan_sha256", "production_plan", "recovery",
            "mutation_intent", "main_lock_proof",
        },
        "apply authorities",
    )
    require_sha256(authorities["rollout_plan_sha256"], "apply rollout plan hash")
    _validate_artifact_binding(authorities["production_plan"], "apply production plan authority")
    _validate_artifact_binding(authorities["recovery"], "apply recovery authority")
    validate_full_artifact_binding(authorities["mutation_intent"], "apply mutation intent authority")
    validate_full_artifact_binding(authorities["main_lock_proof"], "apply main lock proof authority")
    transition = exact_keys(
        receipt["provider_transition"],
        {
            "http_methods_used", "http_request_count", "mutation_request_count",
            "endpoint_labels", "mutation_fingerprint_sha256", "ambiguous_reconciled",
        },
        "provider transition",
    )
    if transition["http_methods_used"] != ["GET", "PUT"]:
        fail("apply provider methods differ")
    exact_int(transition["http_request_count"], "apply request count", 11, 10_000)
    exact_int(transition["mutation_request_count"], "apply mutation count", 1, 1)
    if transition["endpoint_labels"] != ["app", "deployment"]:
        fail("apply endpoint labels differ")
    require_sha256(transition["mutation_fingerprint_sha256"], "apply mutation fingerprint")
    exact_bool(transition["ambiguous_reconciled"], "apply ambiguous reconciliation")
    before = _validate_public_provider_state(receipt["before"], "apply before state", allow_legacy=True)
    after = _validate_public_provider_state(receipt["after"], "apply after state", allow_legacy=False)
    for key in ("app_identity_sha256", "default_ingress_sha256", "environment_values_sha256", "non_source_projection_sha256"):
        if before[key] != after[key]:
            fail("apply receipt does not preserve production state")
    if receipt["gates"] != {"deployment_succeeded": True, "migration_succeeded": True}:
        fail("apply gates are incomplete")
    if receipt["rollback"] != ROLLBACK_FLOORS[target]:
        fail("apply rollback floor differs")
    canary = exact_keys(
        receipt["canary"],
        {"required", "completed", "endpoint_labels", "route_contract_sha256"},
        "apply canary requirement",
    )
    if canary["required"] is not True or canary["completed"] is not False:
        fail("apply receipt improperly claims canary completion")
    if canary["endpoint_labels"] != [
        "app-health", "app-ready", "meta-live", "meta-ready", "gmail-live", "gmail-ready",
    ]:
        fail("apply canary endpoint labels differ")
    require_sha256(canary["route_contract_sha256"], "apply route contract hash")
    sanitize_public(receipt)
    return receipt


def build_phase_state(
    receipt: Any,
    *,
    change_receipt_sha256: str,
    canary_sha256: str,
    control: Mapping[str, Any],
    completed_at: str,
    change_receipt_binding: Mapping[str, Any] | None = None,
    main_lock_release_reconciliation_binding: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    authority = receipt.get("authority") if type(receipt) is dict else None
    if authority == "production-phase-apply-receipt":
        receipt = validate_apply_receipt(receipt)
        receipt_kind = "apply-receipt"
    elif authority == "production-phase-rollback-receipt":
        from rollback_production_change import validate_rollback_receipt

        receipt = validate_rollback_receipt(receipt)
        receipt_kind = "rollback-receipt"
    elif authority == "production-orphan-reconciliation-receipt":
        receipt = validate_reconciliation_receipt(receipt)
        if receipt["classification"]["outcome"] not in {
            "committed", "already-receipted",
        } or receipt["classification"]["canary_eligible"] is not True:
            fail("orphan reconciliation is not eligible for a phase state")
        receipt_kind = "reconciliation-receipt"
    elif authority == "production-orphan-rollback-receipt":
        from rollback_production_change import validate_orphan_rollback_receipt

        receipt = validate_orphan_rollback_receipt(receipt)
        receipt_kind = "orphan-rollback-receipt"
    else:
        fail("change receipt authority differs")
    receipt_hash = require_sha256(
        change_receipt_sha256, "change receipt exact-file hash"
    )
    if sha256_bytes(canonical_file_bytes(receipt)) != receipt_hash:
        fail("change receipt exact-file hash differs")
    canary_hash = require_sha256(canary_sha256, "canary evidence hash")
    target = receipt["lineage"]["to"]
    state_control = exact_keys(
        dict(control),
        {"workflow_sha", "workflow_path", "run_id", "run_attempt", "runner_environment", "release_policy_sha256", "change_schema_sha256"},
        "phase state control",
    )
    if state_control["release_policy_sha256"] != receipt["control"]["release_policy_sha256"] or state_control["change_schema_sha256"] != receipt["control"]["change_schema_sha256"]:
        fail("phase state control hashes differ from apply")
    lineage = copy.deepcopy(receipt["lineage"])
    reconciled = (
        change_receipt_binding is not None
        or main_lock_release_reconciliation_binding is not None
    )
    if reconciled and (
        change_receipt_binding is None
        or main_lock_release_reconciliation_binding is None
        or receipt_kind not in {"apply-receipt", "rollback-receipt"}
    ):
        fail("normal lock-release reconciliation bindings are incomplete")
    if reconciled:
        receipt_binding = copy.deepcopy(
            validate_full_artifact_binding(
                dict(change_receipt_binding), "phase state original receipt binding"
            )
        )
        release_binding = copy.deepcopy(
            validate_full_artifact_binding(
                dict(main_lock_release_reconciliation_binding),
                "phase state main-lock release reconciliation binding",
            )
        )
        if (
            receipt_binding["sha256"] != receipt_hash
            or str(receipt_binding["run_id"]) != str(receipt["control"]["run_id"])
            or receipt_binding["run_attempt"] != 1
            or receipt_binding["artifact_name"]
            != (
                f"production-phase-{receipt_kind.removesuffix('-receipt')}-"
                f"{receipt_binding['run_id']}-1"
            )
            or release_binding["artifact_name"]
            != f"production-main-lock-release-reconciliation-{release_binding['run_id']}-1"
        ):
            fail("normal lock-release reconciliation binding differs")
        lineage["predecessor_kind"] = f"{receipt_kind.removesuffix('-receipt')}-reconciled-receipt"
    else:
        receipt_binding = None
        release_binding = None
        lineage["predecessor_kind"] = receipt_kind
    lineage["predecessor_state_sha256"] = receipt_hash
    if receipt_kind == "apply-receipt":
        rollout_hash = receipt["authorities"]["rollout_plan_sha256"]
        production_plan_hash = receipt["authorities"]["production_plan"]["sha256"]
        recovery_hash = receipt["authorities"]["recovery"]["sha256"]
    elif receipt_kind in {"rollback-receipt", "orphan-rollback-receipt"}:
        rollout_hash = receipt["authorities"]["rollout_plan_sha256"]
        production_plan_hash = receipt["target_authority"]["production_plan_sha256"]
        recovery_hash = receipt["authorities"]["recovery"]["sha256"]
    else:
        upstream = receipt["authorities"]["upstream"]
        rollout_hash = upstream["rollout_plan_sha256"]
        if receipt["lineage"]["operation"] == "activate":
            production_plan_hash = upstream["production_plan"]["sha256"]
        else:
            production_plan_hash = upstream["target_authority"]["production_plan_sha256"]
        recovery_hash = upstream["recovery"]["sha256"]
    evidence = {
        "rollout_plan_sha256": rollout_hash,
        "production_plan_sha256": production_plan_hash,
        "recovery_sha256": recovery_hash,
        "change_receipt_sha256": receipt_hash,
        "canary_sha256": canary_hash,
    }
    if reconciled:
        evidence["change_receipt_binding"] = receipt_binding
        evidence["main_lock_release_reconciliation_binding"] = release_binding
    state = {
        "schema_version": 2 if reconciled else 1,
        "authority": "production-phase-state",
        "repository": REPOSITORY,
        "completed_at": completed_at,
        "control": state_control,
        "lineage": lineage,
        "provider_state": copy.deepcopy(receipt["after"]),
        "evidence": evidence,
        "gates": {
            "deployment_succeeded": True,
            "migration_succeeded": True,
            "canary_succeeded": True,
        },
        "rollback": copy.deepcopy(receipt["rollback"]),
    }
    validate_phase_state(state)
    return state


def validate_phase_state(value: Any, *, now: dt.datetime | None = None) -> dict[str, Any]:
    state = exact_keys(
        value,
        {"schema_version", "authority", "repository", "completed_at", "control", "lineage", "provider_state", "evidence", "gates", "rollback"},
        "production phase state",
    )
    version = exact_int(state["schema_version"], "phase state schema", 1, 2)
    if state["authority"] != "production-phase-state" or state["repository"] != REPOSITORY:
        fail("phase state authority differs")
    completed = require_timestamp(state["completed_at"], "phase completion time")
    if now is not None:
        checked = now.replace(microsecond=0)
        if completed > checked or checked - completed > dt.timedelta(days=30):
            fail("phase state time is invalid")
    control = exact_keys(
        state["control"],
        {"workflow_sha", "workflow_path", "run_id", "run_attempt", "runner_environment", "release_policy_sha256", "change_schema_sha256"},
        "phase state control",
    )
    require_sha1(control["workflow_sha"], "phase state workflow SHA")
    if control["workflow_path"] != ".github/workflows/verify-production-crm-canary.yml":
        fail("phase state workflow differs")
    require_run_id(control["run_id"], "phase state run ID")
    exact_int(control["run_attempt"], "phase state run attempt", 1, 1)
    if control["runner_environment"] != "github-hosted":
        fail("phase state runner differs")
    require_sha256(control["release_policy_sha256"], "phase state policy hash")
    require_sha256(control["change_schema_sha256"], "phase state schema hash")
    lineage = exact_keys(
        state["lineage"],
        {
            "event_sequence", "phase_ordinal", "operation", "from", "to", "predecessor_kind",
            "predecessor_state_sha256", "phase", "phase_source_sha",
        },
        "phase state lineage",
    )
    phase = validate_phase(lineage["phase"], "phase state phase")
    sequence = exact_int(lineage["event_sequence"], "phase state event sequence", 1)
    ordinal = exact_int(lineage["phase_ordinal"], "phase state ordinal", 1, 4)
    source = exact_string(lineage["from"], "phase state source")
    target = validate_phase(lineage["to"], "phase state target")
    operation = exact_string(lineage["operation"], "phase state operation")
    if phase != target or ordinal != PHASES.index(target) + 1:
        fail("phase state target differs")
    if operation == "activate":
        if (
            source != PREDECESSOR[target]
            or (source == "genesis" and sequence != 1)
            or (source != "genesis" and sequence < 2)
        ):
            fail("phase activation lineage differs")
        expected_kinds = {"apply-receipt", "apply-reconciled-receipt", "reconciliation-receipt"}
    elif operation == "rollback":
        validate_rollback_transition(source, target)
        if sequence < 2:
            fail("rollback lineage sequence differs")
        expected_kinds = {
            "rollback-receipt", "rollback-reconciled-receipt", "orphan-rollback-receipt",
            "reconciliation-receipt",
        }
    else:
        fail("phase state operation differs")
    if lineage["predecessor_kind"] not in expected_kinds:
        fail("phase state predecessor kind differs")
    predecessor_hash = lineage["predecessor_state_sha256"]
    require_sha256(predecessor_hash, "predecessor state hash")
    require_sha1(lineage["phase_source_sha"], "phase source SHA")
    _validate_public_provider_state(
        state["provider_state"], "phase provider state", allow_legacy=False
    )
    evidence_keys = {
        "rollout_plan_sha256", "production_plan_sha256", "recovery_sha256",
        "change_receipt_sha256", "canary_sha256",
    }
    reconciled_kind = lineage["predecessor_kind"] in {
        "apply-reconciled-receipt", "rollback-reconciled-receipt"
    }
    if version == 2 and reconciled_kind:
        evidence_keys |= {
            "change_receipt_binding",
            "main_lock_release_reconciliation_binding",
        }
    elif version != 1 or reconciled_kind:
        fail("phase state reconciliation schema differs")
    evidence = exact_keys(state["evidence"], evidence_keys, "phase state evidence")
    for key in ("rollout_plan_sha256", "production_plan_sha256", "recovery_sha256", "change_receipt_sha256", "canary_sha256"):
        item = evidence[key]
        require_sha256(item, f"phase state {key}")
    if evidence["change_receipt_sha256"] != lineage["predecessor_state_sha256"]:
        fail("phase state receipt lineage differs from evidence")
    if reconciled_kind:
        receipt_binding = validate_full_artifact_binding(
            evidence["change_receipt_binding"], "phase state original receipt binding"
        )
        release_binding = validate_full_artifact_binding(
            evidence["main_lock_release_reconciliation_binding"],
            "phase state main-lock release reconciliation binding",
        )
        expected_receipt_prefix = (
            "production-phase-apply"
            if operation == "activate"
            else "production-phase-rollback"
        )
        if (
            receipt_binding["sha256"] != evidence["change_receipt_sha256"]
            or receipt_binding["artifact_name"]
            != f"{expected_receipt_prefix}-{receipt_binding['run_id']}-1"
            or release_binding["artifact_name"]
            != f"production-main-lock-release-reconciliation-{release_binding['run_id']}-1"
        ):
            fail("phase state paired reconciliation bindings differ")
    gates = exact_keys(state["gates"], {"deployment_succeeded", "migration_succeeded", "canary_succeeded"}, "phase gates")
    if gates != {"deployment_succeeded": True, "migration_succeeded": True, "canary_succeeded": True}:
        fail("phase state gates are incomplete")
    if state["rollback"] != ROLLBACK_FLOORS[phase]:
        fail("phase state rollback floor differs")
    sanitize_public(state)
    return state


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Verify fail-closed production release evidence")
    sub = parser.add_subparsers(dest="command", required=True)
    state = sub.add_parser("validate-phase-state")
    state.add_argument("--state", required=True)
    state.add_argument("--expected-sha256", required=True)
    lock_build = sub.add_parser("build-main-lock-proof")
    lock_build.add_argument("--request", required=True)
    lock_build.add_argument("--mutation-intent", required=True)
    lock_build.add_argument("--mutation-intent-authority", required=True)
    lock_build.add_argument(
        "--workflow-path", required=True, choices=tuple(LOCK_PROOF_WORKFLOWS.values())
    )
    lock_build.add_argument("--workflow-sha", required=True)
    lock_build.add_argument("--workflow-run-id", required=True)
    lock_build.add_argument("--workflow-run-attempt", required=True, type=int)
    lock_build.add_argument("--runner-temp", required=True)
    lock_build.add_argument("--output", required=True)
    lock_validate = sub.add_parser("validate-main-lock-proof")
    lock_validate.add_argument("--proof", required=True)
    lock_validate.add_argument("--proof-sha256", required=True)
    lock_validate.add_argument("--mutation-intent", required=True)
    return parser


def run_cli(arguments: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(arguments)
    if args.command == "validate-phase-state":
        path = Path(args.state)
        value = load_json(path, "production phase state")
        if sha256_bytes(path.read_bytes()) != require_sha256(args.expected_sha256, "expected phase state hash"):
            fail("phase state exact-file hash differs")
        validate_phase_state(value)
        return 0
    if args.command == "build-main-lock-proof":
        operation = next(
            name for name, path in LOCK_PROOF_WORKFLOWS.items()
            if path == args.workflow_path
        )
        proof = build_main_lock_proof(
            request=load_json(Path(args.request), "main lock proof request"),
            mutation_intent=load_json(Path(args.mutation_intent), "production mutation intent"),
            mutation_intent_binding=load_json(
                Path(args.mutation_intent_authority), "mutation intent artifact authority"
            ),
            control={
                "workflow_sha": require_sha1(args.workflow_sha, "workflow SHA"),
                "workflow_path": args.workflow_path,
                "run_id": require_run_id(args.workflow_run_id, "workflow run ID"),
                "run_attempt": exact_int(args.workflow_run_attempt, "workflow attempt", 1, 1),
                "runner_environment": "github-hosted",
            },
            now=dt.datetime.now(dt.timezone.utc),
        )
        if proof["operation"] != operation:
            fail("main lock proof request operation differs from the workflow path")
        write_canonical_output(Path(args.output), proof, Path(args.runner_temp))
        return 0
    proof_path = Path(args.proof)
    proof = load_json(proof_path, "production main lock proof")
    if sha256_bytes(proof_path.read_bytes()) != require_sha256(
        args.proof_sha256, "main lock proof exact-file hash"
    ):
        fail("main lock proof exact-file hash differs")
    validate_main_lock_proof(
        proof,
        mutation_intent=load_json(Path(args.mutation_intent), "production mutation intent"),
    )
    return 0
    fail("unknown command")


def main() -> int:
    try:
        return run_cli()
    except ReleaseError as exc:
        print(f"release verification failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
