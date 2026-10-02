#!/usr/bin/env python3
"""Shared fail-closed primitives for the Release workflow (.github/workflows/ship.yml).

Copied and trimmed from verify_production_release.py (:154-337, :376-405 and
:764-800) so that the Release modules never import the old release-control
lanes, which Stage 2 deletes. Standard library only; every module runs under
``python3 -I -S -B``.

Every failure is a ``ReleaseError`` whose message is a constant reason code
(``<reason>`` or ``<reason>:<detail>``). A message never contains a provider
value, an identifier, a hostname or an environment value.
"""

from __future__ import annotations

import datetime as dt
import decimal
import hashlib
import json
import os
import re
from pathlib import Path
from typing import Any, Iterable, Mapping, NoReturn, Sequence, TextIO


REPOSITORY = "medtechcorps-netizen/whatomate"
SHIP_WORKFLOW_PATH = ".github/workflows/ship.yml"
SHIP_WORKFLOW_REF = f"{REPOSITORY}/{SHIP_WORKFLOW_PATH}@refs/heads/main"
API_ORIGIN = "https://api.digitalocean.com"
COMPONENTS = ("web", "meta-relay", "gmail-relay")
IMAGE_REPOSITORY = {
    component: f"ghcr.io/medtechcorps-netizen/rereply-release-{component}"
    for component in COMPONENTS
}
# verify_production_release.py:125-130: the four App Platform image bindings.
SPEC_BINDINGS = (
    ("services", "omnitech-web", "web", "medtechcorps-netizen/rereply-release-web"),
    ("services", "meta-relay", "meta-relay", "medtechcorps-netizen/rereply-release-meta-relay"),
    ("services", "gmail-relay", "gmail-relay", "medtechcorps-netizen/rereply-release-gmail-relay"),
    ("jobs", "rereply-rls-migrate", "web", "medtechcorps-netizen/rereply-release-web"),
)
PRE_DEPLOY_JOB = "rereply-rls-migrate"
RECORD_TAG_RE = re.compile(r"^prod-([0-9]{8}T[0-9]{6}Z)-([0-9a-f]{8})$")
BOOTSTRAP_TAG = "prod-0000"
# production-app-contract.json security.forbidden_ambient_environment, plus the
# DigitalOcean CLI's own token variable.
FORBIDDEN_AMBIENT = (
    "DIGITALOCEAN_ACCESS_TOKEN",
    "DIGITALOCEAN_TOKEN",
    "DO_ACCESS_TOKEN",
    "DO_TOKEN",
    "GHCR_PRODUCTION_PULL_TOKEN",
)
SECRET_ENV_NAMES = frozenset({"SHIP_DO_TOKEN", "SHIP_TARGET_JSON"})
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
UUID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
# Any 8-4-4-4-12 hex group anywhere in a string, whatever its version nibble.
ANY_UUID_RE = re.compile(
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
)
RUN_ID_RE = re.compile(r"^[1-9][0-9]{0,14}$")
CODE_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*(?::[A-Za-z0-9#._/-]+)*$")
OUTPUT_KEY_RE = re.compile(r"^[a-z][a-z0-9_]{0,63}$")
OUTPUT_VALUE_RE = re.compile(r"^[A-Za-z0-9:._/+=-]{0,8192}$")
MAX_JSON_BYTES = 4 * 1024 * 1024
CREDENTIAL_MARKERS = (
    "EV[",
    "dop_v1_",
    "doo_v1_",
    "dor_v1_",
    "ghp_",
    "github_pat_",
    "-----BEGIN ",
)
# The App Platform default hostname suffix, assembled so that no committed
# file contains a DigitalOcean hostname fragment.
FORBIDDEN_PUBLIC_SUBSTRINGS = ("api.digitalocean.com", "ondigitalocean" + ".app")
FORBIDDEN_PUBLIC_KEYS = frozenset(
    {
        "spec", "envs", "value", "authorization", "access_token", "token",
        "app_id", "active_deployment_id", "postgres_cluster_id", "valkey_cluster_id",
        "valkey_recovery_cluster_id", "default_ingress", "updated_at", "created_at",
        "request_path", "url", "host", "ip", "connection", "user", "password",
    }
)
ENV_ALLOWLIST = ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "RUNNER_TEMP")
# Only so that local Windows test runs can start git; never set on a runner.
WINDOWS_ENV_ALLOWLIST = ("SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE")
# Public reason codes. The detail after ':' is always a code constant.
REASONS = frozenset(
    {
        "app-identity-mismatch", "artifact-invalid", "attestation-unverified",
        "backup-stale", "candidate-invalid", "candidate-stale", "cas-changed",
        "ci-not-green", "context-invalid", "deployment-error",
        "dockerfile-pin-invalid", "downgrade-refused", "drift",
        "dry-run-client-cannot-mutate", "forbidden-image-field", "gh-failed",
        "git-failed", "image-contract-differs", "input-invalid", "internal-error",
        "latest-changed-since-plan", "latest-changed-since-production", "manual",
        "nothing-to-roll-back", "old-lane-active", "output-unsafe",
        "post-deploy-guard", "provider-ambiguous", "provider-get-failed",
        "provider-invalid", "provider-rejected", "reconcile-failed",
        "reconcile-timeout", "record-chain-invalid", "record-invalid",
        "rollback-failed", "rollback-precondition-failed",
        "rollback-target-invalid", "schema-change-blocked",
        "second-mutation-blocked", "smoke-failed", "target-invalid",
        "topology-differs", "trivy-exception-invalid", "vpc-missing-or-differs",
    }
)


class ReleaseError(RuntimeError):
    """An intentionally content-free failure; ``str()`` is its reason code."""

    def __init__(self, code: str) -> None:
        if type(code) is not str or CODE_RE.fullmatch(code) is None or len(code) > 200:
            code = "internal-error:invalid-reason-code"
        elif code.split(":", 1)[0] not in REASONS:
            code = "internal-error:unknown-reason-code"
        super().__init__(code)
        self.code = code

    @property
    def reason(self) -> str:
        return self.code.split(":", 1)[0]


class AmbiguousMutation(ReleaseError):
    """The single PUT may have reached the provider; reconcile with GETs only."""


def fail(code: str) -> NoReturn:
    raise ReleaseError(code)


def _reject_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            fail("input-invalid:json-duplicate-key")
        value[key] = item
    return value


def _reject_number(raw: str) -> NoReturn:
    del raw
    fail("input-invalid:json-number")


def loads_strict(raw: str | bytes, *, decimals: bool = False, code: str = "input-invalid:json") -> Any:
    """Strict JSON: no duplicate keys, no NaN/Infinity, floats only as Decimal.

    ``decimals=True`` is used for provider database endpoints, whose sizes are
    fractional (observe_production_recovery.py:51-82); everything else rejects
    floating point outright.
    """
    if isinstance(raw, bytes):
        try:
            raw = raw.decode("utf-8")
        except UnicodeError as exc:
            raise ReleaseError(code) from exc
    try:
        return json.loads(
            raw,
            object_pairs_hook=_reject_pairs,
            parse_float=decimal.Decimal if decimals else _reject_number,
            parse_constant=_reject_number,
        )
    except ReleaseError as exc:
        raise ReleaseError(code) from exc
    except (json.JSONDecodeError, TypeError, ValueError, decimal.InvalidOperation, RecursionError) as exc:
        raise ReleaseError(code) from exc


def canonical_payload_bytes(value: Any) -> bytes:
    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=True,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("ascii")
    except (TypeError, ValueError) as exc:
        raise ReleaseError("internal-error:not-canonical-json") from exc


def canonical_file_bytes(value: Any) -> bytes:
    return canonical_payload_bytes(value) + b"\n"


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha256_text(raw: str) -> str:
    return sha256_bytes(raw.encode("utf-8"))


def sha256_value(value: Any) -> str:
    return sha256_bytes(canonical_payload_bytes(value))


def exact_keys(value: Any, expected: Iterable[str], code: str) -> dict[str, Any]:
    if type(value) is not dict or set(value) != set(expected):
        fail(code)
    return value


def exact_string(value: Any, code: str, pattern: re.Pattern[str] | None = None) -> str:
    if type(value) is not str or not value or len(value) > 4096:
        fail(code)
    if any(ch in value for ch in "\r\n\x00"):
        fail(code)
    if pattern is not None and pattern.fullmatch(value) is None:
        fail(code)
    return value


def exact_int(value: Any, code: str, minimum: int = 0, maximum: int = 2_147_483_647) -> int:
    if type(value) is not int or value < minimum or value > maximum:
        fail(code)
    return value


def exact_bool(value: Any, code: str) -> bool:
    if type(value) is not bool:
        fail(code)
    return value


def require_sha1(value: Any, code: str) -> str:
    return exact_string(value, code, SHA1_RE)


def require_sha256(value: Any, code: str) -> str:
    return exact_string(value, code, SHA256_RE)


def require_digest(value: Any, code: str) -> str:
    return exact_string(value, code, DIGEST_RE)


def require_uuid(value: Any, code: str) -> str:
    return exact_string(value, code, UUID_RE)


def require_run_id(value: Any, code: str) -> str:
    if type(value) is int:
        value = str(value)
    return exact_string(value, code, RUN_ID_RE)


def require_timestamp(value: Any, code: str) -> dt.datetime:
    raw = exact_string(value, code)
    if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", raw) is None:
        fail(code)
    try:
        parsed = dt.datetime.fromisoformat(raw[:-1] + "+00:00")
    except ValueError as exc:
        raise ReleaseError(code) from exc
    return parsed


def format_timestamp(value: dt.datetime) -> str:
    if not isinstance(value, dt.datetime) or value.tzinfo is None or value.utcoffset() != dt.timedelta(0):
        fail("internal-error:clock-not-utc")
    return value.replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def compact_timestamp(value: str) -> str:
    """2026-10-02T15:11:28Z -> 20261002T151128Z (the record tag form)."""
    require_timestamp(value, "internal-error:timestamp")
    return value.replace("-", "").replace(":", "")


def utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0)


def checked_clock(value: Any) -> dt.datetime:
    if not isinstance(value, dt.datetime) or value.tzinfo is None or value.utcoffset() is None:
        fail("internal-error:clock")
    return value.astimezone(dt.timezone.utc).replace(microsecond=0)


def load_json(
    path: Path,
    code: str,
    *,
    canonical: bool = True,
    maximum: int = MAX_JSON_BYTES,
    decimals: bool = False,
) -> Any:
    if path.is_symlink() or not path.is_file():
        fail(code)
    raw = path.read_bytes()
    if not raw or len(raw) > maximum:
        fail(code)
    value = loads_strict(raw, decimals=decimals, code=code)
    if canonical and raw != canonical_file_bytes(value):
        fail(code)
    return value


def _string_is_public(item: str, private: Sequence[str]) -> bool:
    lowered = item.lower()
    if any(marker in item for marker in CREDENTIAL_MARKERS):
        return False
    if any(secret and secret in item for secret in private):
        return False
    if any(fragment in lowered for fragment in FORBIDDEN_PUBLIC_SUBSTRINGS):
        return False
    if ANY_UUID_RE.search(item):
        return False
    return True


def sanitize_public(
    value: Any,
    *,
    private: Sequence[str] = (),
    allowed_keys: Sequence[str] = (),
) -> None:
    """verify_production_release.py:764-800, plus ``dor_v1_`` and the App
    Platform hostname suffix, and with UUIDs refused anywhere in a string."""
    secrets = tuple(item for item in private if type(item) is str and item)
    allowed = set(allowed_keys)

    def walk(item: Any) -> None:
        if type(item) is dict:
            for key, child in item.items():
                if type(key) is not str:
                    fail("output-unsafe:key")
                if key.lower() in FORBIDDEN_PUBLIC_KEYS and key not in allowed:
                    fail("output-unsafe:key")
                if not _string_is_public(key, secrets):
                    fail("output-unsafe:key")
                walk(child)
        elif type(item) is list:
            for child in item:
                walk(child)
        elif type(item) is str:
            if not _string_is_public(item, secrets):
                fail("output-unsafe:value")
        elif item is None or type(item) in (bool, int):
            return
        else:
            fail("output-unsafe:type")

    walk(value)


def require_public_text(text: Any, private: Sequence[str] = ()) -> str:
    """Free text that may be printed: ASCII, single line, nothing private."""
    if type(text) is not str or len(text) > 8192:
        fail("output-unsafe:text")
    if any(ord(ch) < 0x20 or ord(ch) > 0x7E for ch in text):
        fail("output-unsafe:text")
    if not _string_is_public(text, tuple(item for item in private if type(item) is str and item)):
        fail("output-unsafe:text")
    return text


class Output:
    """The only way the Release modules print, write step outputs or summaries."""

    def __init__(
        self,
        *,
        stdout: TextIO,
        summary_path: str | None = None,
        output_path: str | None = None,
    ) -> None:
        self._stdout = stdout
        self._summary_path = summary_path or None
        self._output_path = output_path or None
        self._private: list[str] = []
        self._masked: set[str] = set()

    @property
    def private(self) -> tuple[str, ...]:
        return tuple(self._private)

    def add_private(self, *values: Any) -> None:
        for value in values:
            if type(value) is str and value and value not in self._private:
                self._private.append(value)

    def add_mask(self, value: Any) -> None:
        """Print ``::add-mask::`` once per value, before the value is used."""
        if type(value) is not str or not value:
            return
        if any(ch in value for ch in "\r\n\x00"):
            fail("output-unsafe:mask")
        self.add_private(value)
        if value in self._masked:
            return
        self._masked.add(value)
        self._stdout.write(f"::add-mask::{value}\n")
        self._stdout.flush()

    def text(self, line: str) -> None:
        require_public_text(line, self._private)
        self._stdout.write(line + "\n")
        self._stdout.flush()

    def emit(self, value: Mapping[str, Any]) -> None:
        sanitize_public(value, private=self._private)
        self._stdout.write(canonical_payload_bytes(value).decode("ascii") + "\n")
        self._stdout.flush()

    def summary(self, lines: Sequence[str]) -> None:
        checked = [require_public_text(line, self._private) for line in lines]
        if self._summary_path is None:
            return
        with open(self._summary_path, "a", encoding="ascii", newline="\n") as handle:
            handle.write("\n".join(checked) + "\n")

    def set_outputs(self, mapping: Mapping[str, Any]) -> None:
        lines: list[str] = []
        for key, value in mapping.items():
            if type(key) is not str or OUTPUT_KEY_RE.fullmatch(key) is None:
                fail("output-unsafe:output-key")
            if type(value) is not str or OUTPUT_VALUE_RE.fullmatch(value) is None:
                fail("output-unsafe:output-value")
            if value:
                require_public_text(value, self._private)
            lines.append(f"{key}={value}")
        if self._output_path is None:
            return
        with open(self._output_path, "a", encoding="ascii", newline="\n") as handle:
            handle.write("".join(line + "\n" for line in lines))


def scrubbed_env(extra: Mapping[str, str] | None = None, *, base: Mapping[str, str] | None = None) -> dict[str, str]:
    """A subprocess environment that can never carry the deploy token."""
    source = os.environ if base is None else base
    names = ENV_ALLOWLIST + (WINDOWS_ENV_ALLOWLIST if os.name == "nt" else ())
    env = {name: source[name] for name in names if name in source and type(source[name]) is str}
    for key, value in (extra or {}).items():
        if key in SECRET_ENV_NAMES or type(key) is not str or type(value) is not str:
            fail("internal-error:subprocess-environment")
        env[key] = value
    return env


def validate_context(env: Mapping[str, str], *, allow_rerun: bool = False) -> dict[str, Any]:
    """verify_production_release.py:376-405 adapted to ship.yml."""
    if env.get("GITHUB_REPOSITORY") != REPOSITORY:
        fail("context-invalid:repository")
    if env.get("GITHUB_REF") != "refs/heads/main" or env.get("GITHUB_REF_PROTECTED") != "true":
        fail("context-invalid:ref")
    if env.get("GITHUB_EVENT_NAME") != "workflow_dispatch":
        fail("context-invalid:event")
    if env.get("GITHUB_WORKFLOW_REF") != SHIP_WORKFLOW_REF:
        fail("context-invalid:workflow-ref")
    workflow_sha = require_sha1(env.get("GITHUB_WORKFLOW_SHA"), "context-invalid:workflow-sha")
    if require_sha1(env.get("GITHUB_SHA"), "context-invalid:sha") != workflow_sha:
        fail("context-invalid:workflow-sha")
    if env.get("RUNNER_ENVIRONMENT") != "github-hosted":
        fail("context-invalid:runner")
    attempt = env.get("GITHUB_RUN_ATTEMPT")
    if type(attempt) is not str or re.fullmatch(r"[1-9][0-9]{0,3}", attempt) is None:
        fail("context-invalid:run-attempt")
    if attempt != "1" and not allow_rerun:
        fail("context-invalid:run-attempt")
    if env.get("RUNNER_DEBUG") == "1":
        fail("context-invalid:runner-debug")
    return {
        "sha": workflow_sha,
        "workflow_sha": workflow_sha,
        "run_id": require_run_id(env.get("GITHUB_RUN_ID"), "context-invalid:run-id"),
        "run_attempt": int(attempt),
    }
