"""Fail-closed production launch prerequisites and genuine read-only evidence.

The CLI only audits/checks, never dispatches or mutates. audit-public permits an
explicitly trusted local candidate for source compatibility review but cannot
provide launch or bootstrap authority. Normal mode requires a clean checkout of
current protected main before loading any controller. Only public receipts and
secret NAME metadata are read. Provider parity is explicit, sanitized and GET-only.
"""

from __future__ import annotations

import hashlib
import io
import json
import re
import zipfile
from dataclasses import dataclass
from typing import Mapping, Protocol, Sequence

REPOSITORY = "medtechcorps-netizen/whatomate"
PHASES = ("baseline", "bridge", "backend", "ui")
CANARY_ENVIRONMENT = "rereply-production-canary"
CANARY_PATH = ".github/workflows/verify-production-crm-canary.yml"
FIXTURE_PATH = ".github/workflows/provision-production-crm-canary-fixture.yml"
FIXTURE_CONTROLLER = "release/deployment/provision_production_crm_canary_fixture.py"
SLSA_PREDICATE = "https://slsa.dev/provenance/v1"
STATE_PREDICATE = "https://rereply.app/attestations/production-phase-state/v1"
FIXTURE_PREDICATE = "https://rereply.app/attestations/crm-canary-fixture-result/v1"
REQUIRED_CANARY_SECRET_NAMES = frozenset({
    "CRM_CANARY_PUBLIC_TARGETS_JSON", "CRM_CANARY_SYNTHETIC_DRIVER_JSON",
})
# Include queued and environment-approval waits, not just running jobs.
BLOCKING_STATUSES = frozenset({"queued", "waiting", "pending", "in_progress", "requested"})
CONTROLLED_WORKFLOW_PATHS = frozenset(
    ".github/workflows/" + name for name in (
        "aggregate-exact-four-phase-rollout.yml", "apply-production-phase.yml",
        "build-attest-exact-release-images.yml", "cleanup-production-crm-canary-fixture.yml",
        "cleanup-production-valkey-recovery-fork.yml", "deploy-production.yml",
        "finalize-production-orphan-lock.yml", "inverse-production-crm-canary-fixture.yml",
        "plan-production-rollout.yml", "prepare-production-valkey-recovery-fork.yml",
        "provision-production-crm-canary-fixture.yml", "reconcile-production-main-lock-release.yml",
        "reconcile-production-orphan-lock-release.yml", "reconcile-production-orphan.yml",
        "rollback-production-orphan.yml", "rollback-production-phase.yml",
        "validate-exact-release-source.yml", "verify-production-crm-canary.yml",
        "verify-production-recovery-readiness.yml",
    )
)


class LaunchBlocked(RuntimeError):
    """Only constant, sanitized reason codes may be included in messages."""


class LaunchNotQuiescent(LaunchBlocked):
    """Only individually valid full pairs that changed, not an authority failure."""


def require(condition: bool, code: str) -> None:
    if not condition:
        raise LaunchBlocked(code)


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate-json-key")
        result[key] = value
    return result


def _invalid_constant(_value):
    raise LaunchBlocked("nonstandard-json-constant")


def public_json(data: bytes | None, limit: int = 131072) -> dict:
    require(isinstance(data, bytes) and 0 < len(data) <= limit, "public-evidence-missing-or-oversized")
    try:
        value = json.loads(data, object_pairs_hook=_pairs, parse_constant=_invalid_constant)
    except (ValueError, UnicodeError):
        raise LaunchBlocked("invalid-public-evidence-json") from None
    require(type(value) is dict, "public-evidence-not-object")
    return value


def _hex(value, length: int) -> bool:
    return type(value) is str and re.fullmatch(r"[0-9a-f]{" + str(length) + r"}", value) is not None


def _id(value) -> bool:
    return type(value) is str and re.fullmatch(r"[1-9][0-9]{0,14}", value) is not None


def descriptor(data: bytes | None, *, fixture: bool) -> dict:
    value = public_json(data, 16384)
    digest_key = "result_sha256" if fixture else "state_sha256"
    expected = {"run_id", "artifact_id", "artifact_digest", digest_key}
    expected.add("control_sha" if fixture else "run_attempt")
    require(set(value) == expected, "public-descriptor-keys-differ")
    require(_id(value["run_id"]) and _id(value["artifact_id"]), "public-descriptor-id-invalid")
    require(_hex(value[digest_key], 64), "public-descriptor-hash-invalid")
    require(type(value["artifact_digest"]) is str
            and value["artifact_digest"].startswith("sha256:")
            and _hex(value["artifact_digest"][7:], 64), "public-descriptor-artifact-digest-invalid")
    if fixture:
        require(_hex(value["control_sha"], 40), "fixture-origin-control-invalid")
    else:
        require(type(value["run_attempt"]) is int and value["run_attempt"] >= 1,
                "predecessor-attempt-invalid")
    return value


@dataclass(frozen=True)
class LaunchContext:
    control_sha: str
    phase: str
    fixture_descriptor: bytes | None
    predecessor_descriptor: bytes | None = None
    predecessor_state: bytes | None = None


@dataclass(frozen=True)
class RunInventory:
    runs: Sequence[Mapping]
    statuses_queried: frozenset[str]
    complete: bool


@dataclass(frozen=True)
class SecretMetadata:
    environment: str
    names: frozenset[str]
    protected_environment_verified: bool
    complete: bool


@dataclass(frozen=True)
class DispatchIdentity:
    workflow_id: int
    workflow_path: str
    control_sha: str
    expected_title: str


def materialized_dispatch_run(before_ids: frozenset[int], runs: Sequence[Mapping],
                              expected: DispatchIdentity) -> int | None:
    """Adopt one exact new attempt, never the first unseen/foreign run."""
    require(type(expected.workflow_id) is int and expected.workflow_id > 0
            and expected.workflow_path in CONTROLLED_WORKFLOW_PATHS
            and _hex(expected.control_sha, 40) and type(expected.expected_title) is str
            and 0 < len(expected.expected_title) <= 256, "dispatch-expectation-invalid")
    require(all(type(run.get("id")) is int and run["id"] > 0 for run in runs)
            and len({run["id"] for run in runs}) == len(runs), "dispatch-run-inventory-invalid")
    new = [run for run in runs if run["id"] not in before_ids]
    if not new:
        return None
    require(len(new) == 1, "dispatch-materialization-ambiguous")
    run = new[0]
    require(run.get("workflow_id") == expected.workflow_id and run.get("path") == expected.workflow_path
            and run.get("repository", {}).get("full_name") == REPOSITORY
            and run.get("event") == "workflow_dispatch" and run.get("head_branch") == "main"
            and run.get("head_sha") == expected.control_sha and type(run.get("run_attempt")) is int
            and run["run_attempt"] == 1 and run.get("previous_attempt_url") is None
            and run.get("display_title") == expected.expected_title,
            "dispatch-materialization-foreign-or-misbound")
    return run["id"]


class ReadOnlyEvidence(Protocol):
    """Mandatory adapter contract, implemented by the genuine read-only reader.

    GET lists must be exhaustively paginated (including runs in every requested
    status). Resolve workflow IDs from their exact paths at current main. Include
    EVERY workflow sharing rereply-production, not just known paths below.
    API errors must not be converted to empty lists or successful proof results.
    Secret access is metadata-only, scoped to the protected canary environment.

    fixture_public_authority must enforce the reviewed fixture verifier's public
    ancestry/producer compatibility, terminal-result schema, exact five-job and
    fourteen-artifact inventories, latest attempt, protected producer run, and
    stable inventory checks. It must NOT fetch or parse driver-secret contents.
    verify_attestation must run real cryptographic verification binding repository,
    signer path/digest, source digest/ref main, deny-self-hosted, exact subject hash,
    predicate type and (for custom predicates) equality to the public JSON subject.
    """

    def current_main_sha(self) -> str: ...
    def worktree_sha(self) -> str: ...
    def workflow_identities(self) -> Mapping[int, str]: ...
    def active_runs(self, statuses: frozenset[str]) -> RunInventory: ...
    def canary_secret_metadata(self) -> SecretMetadata: ...
    def fixture_public_authority(self, descriptor: Mapping, current_control: str) -> bool: ...
    def artifact_metadata(self, artifact_id: str) -> Mapping: ...
    def artifact_archive(self, artifact_id: str) -> bytes: ...
    def run_metadata(self, run_id: str, attempt: int | None = None) -> Mapping: ...
    def run_jobs(self, run_id: str, attempt: int) -> Sequence[Mapping]: ...
    def latest_successful_state_run_id(self, control_sha: str) -> str | None: ...
    def verify_attestation(self, subject: bytes, predicate: str,
                           signer_path: str, control_sha: str) -> bool: ...


def _workflow_identities(reader: ReadOnlyEvidence) -> Mapping[int, str]:
    identities = reader.workflow_identities()
    require(bool(identities) and all(type(k) is int and k > 0
            and type(v) is str and v.startswith(".github/workflows/")
            for k, v in identities.items()), "workflow-identity-inventory-invalid")
    require(len(set(identities.values())) == len(identities), "workflow-identity-inventory-ambiguous")
    require(CONTROLLED_WORKFLOW_PATHS <= set(identities.values()), "workflow-identity-inventory-incomplete")
    return identities


def _production_lock_busy(reader: ReadOnlyEvidence) -> bool:
    identities = _workflow_identities(reader)
    inventory = reader.active_runs(BLOCKING_STATUSES)
    require(inventory.complete is True and BLOCKING_STATUSES <= inventory.statuses_queried,
            "active-run-inventory-incomplete")
    paths = set(identities.values())
    for run in inventory.runs:
        if run.get("status") == "completed":
            continue
        path, workflow_id = run.get("path"), run.get("workflow_id")
        if path in paths or workflow_id in identities:
            # Identity disagreement is itself a blocking observation.
            return True
        require(run.get("status") in BLOCKING_STATUSES, "active-run-status-unrecognized")
        require(type(path) is str and type(workflow_id) is int,
                "active-run-identity-missing")
    return False


def production_lock_busy(reader: ReadOnlyEvidence) -> bool:
    try:
        return _production_lock_busy(reader)
    except LaunchBlocked:
        raise
    except Exception:
        raise LaunchBlocked("readonly-lock-inventory-failed") from None


def require_current_control(reader: ReadOnlyEvidence, control_sha: str) -> None:
    try:
        require(_hex(control_sha, 40), "control-sha-invalid")
        require(reader.current_main_sha() == control_sha, "main-control-mismatch")
        require(reader.worktree_sha() == control_sha, "worktree-control-mismatch")
    except LaunchBlocked:
        raise
    except Exception:
        raise LaunchBlocked("readonly-control-inventory-failed") from None


def _artifact(reader: ReadOnlyEvidence, d: Mapping, name: str, filename: str,
              hash_key: str) -> bytes:
    meta = reader.artifact_metadata(d["artifact_id"])
    require(str(meta.get("id")) == d["artifact_id"] and meta.get("name") == name
            and meta.get("digest") == d["artifact_digest"] and meta.get("expired") is False
            and str(meta.get("workflow_run", {}).get("id")) == d["run_id"],
            "artifact-metadata-binding-failed")
    blob = reader.artifact_archive(d["artifact_id"])
    require(isinstance(blob, bytes) and 0 < len(blob) <= 1048576, "artifact-archive-size-invalid")
    require("sha256:" + hashlib.sha256(blob).hexdigest() == d["artifact_digest"],
            "artifact-archive-digest-mismatch")
    try:
        with zipfile.ZipFile(io.BytesIO(blob)) as archive:
            names = archive.namelist()
            sidecar = filename.removesuffix(".json") + ".sha256"
            require(len(names) == 2 and set(names) == {filename, sidecar}, "artifact-file-inventory-differs")
            require(all(not entry.is_dir() and (entry.external_attr >> 16) & 0o170000 in (0, 0o100000)
                        for entry in archive.infolist()), "artifact-nonregular-entry")
            require(0 < archive.getinfo(filename).file_size <= 131072
                    and archive.getinfo(sidecar).file_size == 65, "artifact-public-file-size-invalid")
            subject = archive.read(filename)
            require(hashlib.sha256(subject).hexdigest() == d[hash_key]
                    and archive.read(sidecar) == (d[hash_key] + "\n").encode(),
                    "artifact-exact-file-hash-mismatch")
    except (ValueError, KeyError, zipfile.BadZipFile, RuntimeError):
        raise LaunchBlocked("artifact-archive-invalid") from None
    return subject


def _signatures(reader: ReadOnlyEvidence, subject: bytes, path: str,
                control: str, custom_predicate: str) -> None:
    for predicate in (SLSA_PREDICATE, custom_predicate):
        require(reader.verify_attestation(subject, predicate, path, control) is True,
                "public-artifact-signature-verification-failed")


def _fixture(reader: ReadOnlyEvidence, d: Mapping, control: str) -> bytes:
    require(reader.fixture_public_authority(d, control) is True, "fixture-public-authority-failed")
    subject = _artifact(reader, d, "crm-canary-fixture-result-" + d["run_id"] + "-1",
                        "result.json", "result_sha256")
    _signatures(reader, subject, FIXTURE_PATH, d["control_sha"], FIXTURE_PREDICATE)
    result = public_json(subject)
    require(result.get("kind") == "crm-canary-fixture-provisioning"
            and result.get("state") == "allowlist_deployment_verified"
            and result.get("control_sha") == d["control_sha"]
            and result.get("origin", {}).get("run_id") == d["run_id"]
            and _hex(result.get("fixture_descriptor_sha256"), 64), "fixture-terminal-result-invalid")
    return subject


def _predecessor(reader: ReadOnlyEvidence, context: LaunchContext, d: Mapping) -> None:
    current = context.control_sha
    previous = PHASES[PHASES.index(context.phase) - 1]
    identities = _workflow_identities(reader)
    workflow_id = next(k for k, v in identities.items() if v == CANARY_PATH)
    for attempt in (None, d["run_attempt"]):
        run = reader.run_metadata(d["run_id"], attempt)
        require(str(run.get("id")) == d["run_id"] and run.get("run_attempt") == d["run_attempt"]
                and run.get("workflow_id") == workflow_id and run.get("path") == CANARY_PATH
                and run.get("repository", {}).get("full_name") == REPOSITORY
                and run.get("event") == "workflow_dispatch" and run.get("head_branch") == "main"
                and run.get("head_sha") == current and run.get("status") == "completed"
                and run.get("conclusion") == "success", "predecessor-run-authority-failed")
    require(reader.latest_successful_state_run_id(current) == d["run_id"],
            "predecessor-is-not-latest-successful-state")
    gates = [job for job in reader.run_jobs(d["run_id"], d["run_attempt"])
             if job.get("name") == "Exact production phase gate"]
    require(len(gates) == 1 and gates[0].get("run_attempt") == d["run_attempt"]
            and gates[0].get("status") == "completed" and gates[0].get("conclusion") == "success"
            and gates[0].get("runner_name") is not None, "predecessor-gate-not-successful")
    subject = _artifact(reader, d, "production-phase-state-" + d["run_id"] + "-" + str(d["run_attempt"]),
                        "production-phase-state.json", "state_sha256")
    require(subject == context.predecessor_state, "local-predecessor-bytes-differ")
    _signatures(reader, subject, CANARY_PATH, current, STATE_PREDICATE)
    state = public_json(subject)
    lineage, state_control = state.get("lineage", {}), state.get("control", {})
    require(state.get("authority") == "production-phase-state" and state.get("repository") == REPOSITORY
            and state_control.get("workflow_sha") == current and state_control.get("workflow_path") == CANARY_PATH
            and state_control.get("run_id") == d["run_id"] and type(state_control.get("run_attempt")) is int
            and state_control.get("run_attempt") == d["run_attempt"]
            and state_control.get("runner_environment") == "github-hosted"
            and lineage.get("phase") == previous and lineage.get("to") == previous
            and type(lineage.get("phase_ordinal")) is int
            and lineage.get("phase_ordinal") == PHASES.index(previous) + 1
            and all(state.get("gates", {}).get(key) is True for key in
                    ("deployment_succeeded", "migration_succeeded", "canary_succeeded")),
            "predecessor-state-control-or-lineage-failed")


def require_launch_ready(reader: ReadOnlyEvidence | None, context: LaunchContext) -> None:
    """One initial gate for ALL future phases; no mutation capability is accepted."""
    require(reader is not None, "explicit-reviewed-readonly-evidence-adapter-required")
    require(context.phase in PHASES, "phase-invalid")
    try:
        # Reject absent future UI requirements before any remote read or dispatch.
        fixture = descriptor(context.fixture_descriptor, fixture=True)
        predecessor = None
        if context.phase != "baseline":
            predecessor = descriptor(context.predecessor_descriptor, fixture=False)
            require(context.predecessor_state is not None, "predecessor-state-required")
        else:
            require(context.predecessor_descriptor is None and context.predecessor_state is None,
                    "genesis-must-not-supply-predecessor")
        require_current_control(reader, context.control_sha)
        metadata = reader.canary_secret_metadata()
        require(metadata.environment == CANARY_ENVIRONMENT and metadata.complete is True
                and metadata.protected_environment_verified is True,
                "protected-canary-secret-metadata-unavailable")
        require(REQUIRED_CANARY_SECRET_NAMES <= metadata.names, "protected-ui-driver-secret-metadata-missing")
        require(not production_lock_busy(reader), "production-lock-conflict")
        _fixture(reader, fixture, context.control_sha)
        if predecessor is not None:
            _predecessor(reader, context, predecessor)
        require_current_control(reader, context.control_sha)
    except LaunchBlocked:
        raise
    except Exception:
        # Never echo API exceptions or untrusted evidence/secret content.
        raise LaunchBlocked("readonly-evidence-read-or-verification-failed") from None


import argparse
import base64
import hashlib
import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import types
from pathlib import Path
from urllib.parse import urlencode, urlsplit, parse_qs

guard = sys.modules[__name__]

PREFIX = "/repos/" + guard.REPOSITORY
MAX_RESPONSE = 8 * 1024 * 1024
PUBLIC_SOURCE_PATHS = {guard.FIXTURE_PATH, guard.FIXTURE_CONTROLLER}


def github_subprocess_environment(*, include_auth=True):
    """Preserve CLI authentication/runtime only; never forward provider inputs."""
    allowed = {"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP",
               "USERPROFILE", "HOME", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
               "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN"}
    if not include_auth:
        allowed -= {"GH_TOKEN", "GITHUB_TOKEN"}
    # Iterate names first: excluded provider values are never accessed here.
    env = {key: os.environ[key] for key in os.environ if key.upper() in allowed}
    env.update({"GH_HOST": "github.com", "GH_REPO": guard.REPOSITORY,
                "GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0", "GH_PAGER": "cat"})
    return env


def _json(raw):
    try:
        return json.loads(raw, object_pairs_hook=guard._pairs, parse_constant=guard._invalid_constant)
    except (ValueError, UnicodeError):
        raise guard.LaunchBlocked("github-json-invalid") from None


class GitHubGetOnly:
    """Explicit GET allowlist, bounded pagination and zero retry behavior."""

    def __init__(self, *, gh="gh", run=subprocess.run):
        self.gh = gh
        self._run = run
        self.last_operation = "not-started"

    def command(self, args, *, maximum=MAX_RESPONSE, timeout=120):
        if len(args) >= 3 and args[1:3] == ["attestation", "verify"]:
            self.last_operation = "public-attestation-verification"
        try:
            result = self._run(args, capture_output=True, timeout=timeout, check=False,
                               env=github_subprocess_environment(include_auth=args[0] == self.gh))
        except Exception:
            raise guard.LaunchBlocked("readonly-command-unavailable") from None
        guard.require(result.returncode == 0 and isinstance(result.stdout, bytes)
                      and len(result.stdout) <= maximum, "readonly-command-failed")
        return result.stdout

    @staticmethod
    def allowed(path):
        guard.require(type(path) is str and path.startswith(PREFIX + "/")
                      and not any(c in path for c in ("\\", "#", "\r", "\n")),
                      "github-route-denied")
        parsed = urlsplit(path)
        relative = parsed.path[len(PREFIX):]
        allowed = (
            r"/git/ref/heads/main", r"/branches/main(?:/protection)?",
            r"/actions/workflows(?:/[A-Za-z0-9_.-]+(?:/runs)?)?",
            r"/actions/runs(?:/[1-9][0-9]*(?:/attempts/[1-9][0-9]*)?(?:/(?:jobs|artifacts))?)?",
            r"/actions/artifacts/[1-9][0-9]*(?:/zip)?",
            r"/compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40}",
            r"/environments/rereply-production-canary(?:/(?:secrets|deployment-branch-policies))?",
            r"/contents/\.github/workflows(?:/[A-Za-z0-9_.-]+\.ya?ml)?",
            r"/contents/release/deployment/provision_production_crm_canary_fixture\.py",
        )
        guard.require(any(re.fullmatch(pattern, relative) for pattern in allowed), "github-route-denied")
        query = parse_qs(parsed.query, strict_parsing=True)
        guard.require(set(query) <= {"page", "per_page", "ref", "branch", "event", "status", "head_sha"}
                      and all(len(values) == 1 for values in query.values()), "github-query-denied")

    def raw_get(self, path, maximum=MAX_RESPONSE):
        self.allowed(path)
        self.last_operation = "GET " + re.sub(r"[0-9]{5,}", "[id]", path[len(PREFIX):].split("?")[0])
        return self.command([self.gh, "api", "--hostname", "github.com", "--method", "GET", path], maximum=maximum)

    def get(self, path):
        return _json(self.raw_get(path))

    def pages(self, path, key):
        rows, total = [], None
        for page in range(1, 101):
            value = self.get(path + ("&" if "?" in path else "?") + urlencode({"per_page": 100, "page": page}))
            count = value.get("total_count") if type(value) is dict else None
            guard.require(type(count) is int and 0 <= count <= 10000
                          and total in (None, count) and type(value.get(key)) is list,
                          "github-inventory-invalid-or-moving")
            total = count
            entries = value[key]
            guard.require(len(entries) <= 100 and (entries or len(rows) == count), "github-page-incomplete")
            rows.extend(entries)
            guard.require(len(rows) <= total, "github-inventory-excess")
            if len(rows) == total:
                # Secret metadata has names, not record IDs.
                identity = "name" if key == "secrets" else "id"
                guard.require(all(type(row) is dict and identity in row for row in rows)
                              and len({row[identity] for row in rows}) == len(rows), "github-inventory-duplicates")
                return {"total_count": total, key: rows}
        raise guard.LaunchBlocked("github-inventory-exceeds-bound")


class GitHubEvidence:
    def __init__(self, control_root: Path, *, candidate_audit=False, transport=None, sleep=time.sleep):
        self.root = control_root.resolve(strict=True)
        self.candidate_audit = candidate_audit
        self.api = transport or GitHubGetOnly()
        self.sleep = sleep
        self._identities = None
        self._identity_control = None
        self._parity = None
        self._read_private_inputs = None
        if not candidate_audit:
            # Authenticate the checkout BEFORE executing any local verifier.
            guard.require(self.worktree_sha() == self.current_main_sha(), "launch-checkout-is-not-current-main")
            self.require_protected_main()
        self._fixture = self._load_fixture_verifier()

    def _load_fixture_verifier(self):
        path = self.root / guard.FIXTURE_CONTROLLER
        guard.require(path.is_file(), "reviewed-fixture-verifier-missing")
        # Importing defines validators only; never call main/execute/rehydrate or
        # verify_fixture_result (the latter accesses the private driver secret).
        package_name = "rereply_launch_control_" + hashlib.sha256(str(self.root).encode()).hexdigest()[:16]
        package = types.ModuleType(package_name)
        package.__path__ = [str(path.parent)]
        sys.modules[package_name] = package
        spec = importlib.util.spec_from_file_location(package_name + ".provision_production_crm_canary_fixture", path)
        guard.require(spec is not None and spec.loader is not None, "fixture-verifier-import-unavailable")
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        guard.require(callable(getattr(module, "_verify_fixture_producer_compatibility", None)),
                      "reviewed-fixture-compatibility-helper-missing")
        return module

    def current_main_sha(self):
        ref = self.api.get(PREFIX + "/git/ref/heads/main")
        sha = ref.get("object", {}).get("sha")
        guard.require(guard._hex(sha, 40), "protected-main-sha-invalid")
        return sha

    def _head(self):
        value = self.api.command(["git", "-C", str(self.root), "rev-parse", "HEAD"], maximum=256).decode().strip()
        guard.require(guard._hex(value, 40), "worktree-head-invalid")
        return value

    def worktree_sha(self):
        guard.require(not self.candidate_audit, "candidate-audit-is-not-launch-authority")
        status = self.api.command(["git", "-C", str(self.root), "status", "--porcelain", "--untracked-files=all"], maximum=65536)
        guard.require(not status.strip(), "launch-checkout-has-uncommitted-changes")
        return self._head()

    def require_provider_parity(self, context):
        """Explicit live read-only parity; audit mode cannot enter this path."""
        guard.require_current_control(self, context.control_sha)
        if self._parity is None:
            path = self.root / "release/deployment/sanitized_provider_parity.py"
            name = "rereply_launch_provider_" + hashlib.sha256(str(self.root).encode()).hexdigest()[:16]
            spec = importlib.util.spec_from_file_location(name, path)
            guard.require(spec is not None and spec.loader is not None, "provider-parity-loader-unavailable")
            module = importlib.util.module_from_spec(spec)
            sys.modules[name] = module
            spec.loader.exec_module(module)
            self._parity = module
            self._read_private_inputs = module.make_environment_private_input_reader()
        try:
            return self._parity.require_provider_parity(
                worktree=self.root, control_sha=context.control_sha, phase=context.phase,
                authenticate_predecessor=lambda: self.authenticate_predecessor(context),
                read_private_inputs=self._read_private_inputs)
        except self._parity.ProviderNotQuiescent:
            raise guard.LaunchNotQuiescent("provider-not-yet-quiescent") from None

    def require_protected_main(self):
        branch = self.api.get(PREFIX + "/branches/main")
        protection = self.api.get(PREFIX + "/branches/main/protection")
        guard.require(branch.get("protected") is True
                      and protection.get("enforce_admins", {}).get("enabled") is True
                      and type(protection.get("required_status_checks")) is dict,
                      "protected-main-metadata-invalid")

    def source(self, path, control):
        guard.require(path in PUBLIC_SOURCE_PATHS or re.fullmatch(r"\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml", path),
                      "public-source-path-denied")
        guard.require(guard._hex(control, 40), "public-source-control-invalid")
        return self.api.get(PREFIX + "/contents/" + path + "?" + urlencode({"ref": control}))

    @staticmethod
    def source_bytes(record):
        guard.require(record.get("encoding") == "base64" and type(record.get("content")) is str
                      and len(record["content"]) <= 500000, "public-source-encoding-invalid")
        try:
            source = base64.b64decode(record["content"].replace("\n", ""), validate=True)
        except ValueError:
            raise guard.LaunchBlocked("public-source-base64-invalid") from None
        guard.require(len(source) <= 350000 and hashlib.sha1(b"blob " + str(len(source)).encode() + b"\0" + source).hexdigest()
                      == record.get("sha"), "public-source-blob-mismatch")
        return source

    def _candidate_source(self, path):
        guard.require(self.candidate_audit and path in PUBLIC_SOURCE_PATHS, "candidate-source-not-authorized")
        source = (self.root / path).read_bytes().replace(b"\r\n", b"\n")
        return {"encoding": "base64", "content": base64.b64encode(source).decode(),
                "sha": hashlib.sha1(b"blob " + str(len(source)).encode() + b"\0" + source).hexdigest()}

    def workflow_identities(self):
        current = self.current_main_sha()
        if self._identities is not None and self._identity_control == current:
            return self._identities
        workflows = self.api.pages(PREFIX + "/actions/workflows", "workflows")["workflows"]
        current_files = self.api.get(PREFIX + "/contents/.github/workflows?" + urlencode({"ref": current}))
        guard.require(type(current_files) is list and all(type(row) is dict and row.get("type") == "file"
                      and type(row.get("path")) is str for row in current_files), "workflow-source-inventory-invalid")
        current_paths = {row["path"] for row in current_files}
        identities = {}
        for workflow in workflows:
            path = workflow.get("path", "")
            guard.require(type(workflow.get("id")) is int and workflow["id"] > 0
                          and re.fullmatch(r"\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml", path) is not None,
                          "workflow-metadata-path-invalid")
            if path not in current_paths:
                # Historical workflows stay in the API after deletion. Never GET
                # a missing source and never assume an active retired run is safe.
                identities[workflow["id"]] = path
                continue
            source = self.source_bytes(self.source(path, current)).decode("utf-8")
            # Production concurrency is a literal block in this repository.
            # Unknown syntax involving this group is rejected, never omitted.
            match = re.search(r"(?m)^concurrency:\s*\n((?:[ \t]+[^\n]*\n|\n)+)", source)
            group = re.search(r"(?m)^  group: *['\"]?(rereply-production)['\"]? *$", match.group(1)) if match else None
            if group:
                identities[workflow["id"]] = path
            elif path in guard.CONTROLLED_WORKFLOW_PATHS:
                raise guard.LaunchBlocked("production-concurrency-shape-unrecognized")
            elif re.search(r"(?m)^concurrency:", source):
                # Unknown/dynamic concurrency is conservatively lock-relevant.
                # Environment names mentioning production are not lock groups.
                identities[workflow["id"]] = path
        guard.require(guard.CONTROLLED_WORKFLOW_PATHS <= set(identities.values()), "production-workflow-inventory-incomplete")
        self._identities, self._identity_control = identities, current
        return identities

    def active_runs(self, statuses):
        guard.require(statuses == guard.BLOCKING_STATUSES, "lock-status-policy-differs")
        by_id = {}
        for status in sorted(statuses):
            records = self.api.pages(PREFIX + "/actions/runs?" + urlencode({"status": status}), "workflow_runs")
            for run in records["workflow_runs"]:
                previous = by_id.get(run.get("id"))
                guard.require(previous is None or previous == run, "active-run-inventory-moved")
                by_id[run["id"]] = run
        return guard.RunInventory(tuple(by_id.values()), guard.BLOCKING_STATUSES, True)

    def authenticate_predecessor(self, context):
        """Fresh authenticated bytes for the in-memory provider parity checker."""
        guard.require_current_control(self, context.control_sha)
        if context.phase == "baseline":
            guard.require(context.predecessor_descriptor is None and context.predecessor_state is None,
                          "genesis-predecessor-not-empty")
            return None
        guard._predecessor(self, context, guard.descriptor(context.predecessor_descriptor, fixture=False))
        guard.require_current_control(self, context.control_sha)
        return context.predecessor_state

    def canary_secret_metadata(self):
        self.require_protected_main()
        env = self.api.get(PREFIX + "/environments/" + guard.CANARY_ENVIRONMENT)
        guard.require(env.get("name") == guard.CANARY_ENVIRONMENT, "canary-environment-metadata-invalid")
        policy = env.get("deployment_branch_policy") or {}
        protected = policy.get("protected_branches") is True
        if policy.get("custom_branch_policies") is True:
            policies = self.api.pages(PREFIX + "/environments/" + guard.CANARY_ENVIRONMENT + "/deployment-branch-policies", "branch_policies")
            protected = policies["total_count"] == 1 and policies["branch_policies"][0].get("name") == "main"
            protected = protected and policies["branch_policies"][0].get("type", "branch") == "branch"
        secrets = self.api.pages(PREFIX + "/environments/" + guard.CANARY_ENVIRONMENT + "/secrets", "secrets")
        # Project NAME metadata only. Values never enter the protocol or output.
        names = frozenset(record["name"] for record in secrets["secrets"])
        guard.require(all(type(name) is str and re.fullmatch(r"[A-Z0-9_]+", name) for name in names), "secret-name-metadata-invalid")
        return guard.SecretMetadata(guard.CANARY_ENVIRONMENT, names, protected, True)

    def artifact_metadata(self, artifact_id):
        guard.require(guard._id(artifact_id), "artifact-id-invalid")
        return self.api.get(PREFIX + "/actions/artifacts/" + artifact_id)

    def artifact_archive(self, artifact_id):
        guard.require(guard._id(artifact_id), "artifact-id-invalid")
        return self.api.raw_get(PREFIX + "/actions/artifacts/" + artifact_id + "/zip", maximum=1048576)

    def run_metadata(self, run_id, attempt=None):
        guard.require(guard._id(run_id), "run-id-invalid")
        guard.require(attempt is None or (type(attempt) is int and attempt > 0), "run-attempt-invalid")
        path = PREFIX + "/actions/runs/" + run_id
        return self.api.get(path if attempt is None else path + "/attempts/" + str(attempt))

    def dispatch_identity(self, workflow, fields, control_sha):
        path = ".github/workflows/" + workflow
        guard.require(path in guard.CONTROLLED_WORKFLOW_PATHS, "dispatch-workflow-not-controlled")
        identities = self.workflow_identities()
        matches = [identity for identity, recorded_path in identities.items() if recorded_path == path]
        guard.require(len(matches) == 1, "dispatch-workflow-identity-ambiguous")
        source = self.source_bytes(self.source(path, control_sha)).decode("utf-8")
        names = re.findall(r"(?m)^run-name: ([^\n]+)$", source)
        guard.require(len(names) == 1, "dispatch-run-name-not-explicit")
        guard.require(len(fields) % 2 == 0 and all(fields[i] == "-f" for i in range(0, len(fields), 2)),
                      "dispatch-field-shape-invalid")
        inputs = {}
        for field in fields[1::2]:
            guard.require(type(field) is str and "=" in field, "dispatch-field-shape-invalid")
            key, value = field.split("=", 1)
            guard.require(key not in inputs, "dispatch-field-duplicate")
            inputs[key] = value
        title = names[0]
        if "${{ inputs.phase }}" in title:
            guard.require(inputs.get("phase") in guard.PHASES, "dispatch-phase-missing")
            title = title.replace("${{ inputs.phase }}", inputs["phase"])
        guard.require("${{" not in title and not title.startswith(("'", '"')), "dispatch-run-name-expression-unreviewed")
        return guard.DispatchIdentity(matches[0], path, control_sha, title)

    def materialization_runs(self, expected):
        guard.require(isinstance(expected, guard.DispatchIdentity), "dispatch-expectation-invalid")
        query = urlencode({"branch": "main", "event": "workflow_dispatch", "head_sha": expected.control_sha})
        return self.api.pages(PREFIX + "/actions/workflows/" + str(expected.workflow_id) + "/runs?" + query,
                              "workflow_runs")["workflow_runs"]

    def run_jobs(self, run_id, attempt):
        guard.require(guard._id(run_id) and type(attempt) is int and attempt > 0, "job-run-binding-invalid")
        return self.api.pages(PREFIX + "/actions/runs/" + run_id + "/attempts/" + str(attempt) + "/jobs", "jobs")["jobs"]

    def latest_successful_state_run_id(self, control_sha):
        guard.require(guard._hex(control_sha, 40), "state-control-invalid")
        query = urlencode({"branch": "main", "event": "workflow_dispatch", "status": "success", "head_sha": control_sha})
        runs = self.api.pages(PREFIX + "/actions/workflows/verify-production-crm-canary.yml/runs?" + query, "workflow_runs")["workflow_runs"]
        matches = [run for run in runs if run.get("path") == guard.CANARY_PATH and run.get("head_sha") == control_sha
                   and run.get("status") == "completed" and run.get("conclusion") == "success"]
        if not matches:
            return None
        latest = max(matches, key=lambda run: (run["created_at"], run["id"]))
        return str(latest["id"])

    def verify_attestation(self, subject, predicate, signer_path, control_sha):
        guard.require(predicate in (guard.SLSA_PREDICATE, guard.STATE_PREDICATE, guard.FIXTURE_PREDICATE)
                      and signer_path in (guard.CANARY_PATH, guard.FIXTURE_PATH)
                      and guard._hex(control_sha, 40), "signature-policy-invalid")
        parsed = guard.public_json(subject)
        # The only persisted inputs are bounded, content-free PUBLIC receipts.
        with tempfile.TemporaryDirectory(prefix="rereply-public-attestation-") as temporary:
            target = Path(temporary) / "public-receipt.json"
            target.write_bytes(subject)
            command = [self.api.gh, "attestation", "verify", str(target), "--repo", guard.REPOSITORY,
                "--signer-workflow", guard.REPOSITORY + "/" + signer_path,
                "--signer-digest", control_sha, "--source-digest", control_sha,
                "--source-ref", "refs/heads/main", "--deny-self-hosted-runners", "--format", "json",
                "--predicate-type", predicate]
            result = _json(self.api.command(command))
        guard.require(type(result) is list and bool(result), "signature-verification-empty")
        expected_hash = hashlib.sha256(subject).hexdigest()
        valid = []
        for entry in result:
            statement = entry.get("verificationResult", {}).get("statement", {})
            subject_matches = any(item.get("digest", {}).get("sha256") == expected_hash for item in statement.get("subject", []))
            valid.append(subject_matches and statement.get("predicateType") == predicate
                         and (predicate == guard.SLSA_PREDICATE or statement.get("predicate") == parsed))
        guard.require(any(valid), "signed-public-predicate-or-subject-differs")
        return True

    def fixture_public_authority(self, descriptor, current_control):
        d, f = descriptor, self._fixture
        self.require_protected_main()
        guard.require(self.current_main_sha() == current_control, "fixture-current-main-moved")
        compare = self.api.get(PREFIX + "/compare/" + d["control_sha"] + "..." + current_control)
        guard.require(compare.get("status") in ("ahead", "identical")
                      and compare.get("merge_base_commit", {}).get("sha") == d["control_sha"], "fixture-origin-not-protected-ancestry")
        for path in (guard.FIXTURE_PATH, guard.FIXTURE_CONTROLLER):
            old = self.source(path, d["control_sha"])
            new = self._candidate_source(path) if self.candidate_audit else self.source(path, current_control)
            if not self.candidate_audit:
                guard.require(self.source_bytes(new) == (self.root / path).read_bytes().replace(b"\r\n", b"\n"),
                              "current-producer-source-differs-from-checkout")
            f._verify_fixture_producer_compatibility(path, old, new, d)
        workflow = self.api.get(PREFIX + "/actions/workflows/provision-production-crm-canary-fixture.yml")
        guard.require(type(workflow.get("id")) is int and workflow["id"] > 0
                      and workflow.get("path") == guard.FIXTURE_PATH, "fixture-workflow-identity-invalid")
        for attempt in (None, 1):
            run = self.run_metadata(d["run_id"], attempt)
            guard.require(str(run.get("id")) == d["run_id"] and run.get("head_sha") == d["control_sha"]
                          and run.get("head_branch") == "main" and run.get("path") == guard.FIXTURE_PATH
                          and run.get("workflow_id") == workflow["id"]
                          and run.get("repository", {}).get("full_name") == guard.REPOSITORY
                          and run.get("event") == "workflow_dispatch" and run.get("run_attempt") == 1
                          and run.get("status") == "completed" and run.get("conclusion") == "success",
                          "fixture-exact-run-not-successful")
        jobs = self.run_jobs(d["run_id"], 1)
        expected_jobs = {f.WORKFLOW_JOB_NAMES[0]: "success", f.WORKFLOW_JOB_NAMES[1]: "skipped",
                         f.EXECUTOR_JOB: "success", f.WORKFLOW_JOB_NAMES[3]: "skipped", f.WORKFLOW_JOB_NAMES[4]: "success"}
        guard.require(len(jobs) == 5 and {job.get("name"): job.get("conclusion") for job in jobs} == expected_jobs
                      and all(job.get("status") == "completed" and job.get("run_attempt") == 1 for job in jobs),
                      "fixture-job-inventory-differs")
        meta = self.artifact_metadata(d["artifact_id"])
        f._artifact_record(meta, "crm-canary-fixture-result-" + d["run_id"] + "-1", d["run_id"], d["control_sha"], d["artifact_digest"])
        subject = guard._artifact(self, d, meta["name"], "result.json", "result_sha256")
        result = f.validate_terminal_result(guard.public_json(subject))
        guard.require(result["control_sha"] == d["control_sha"] and result["origin"]["run_id"] == d["run_id"],
                      "fixture-terminal-result-origin-differs")
        expected = {burn["artifact_name"]: (burn["artifact_id"], burn["artifact_digest"]) for burn in result["burns"]}
        expected[meta["name"]] = (d["artifact_id"], d["artifact_digest"])
        expected["crm-canary-fixture-intent-" + d["run_id"] + "-1"] = (result["origin"]["artifact_id"], result["origin"]["artifact_digest"])
        endpoint = PREFIX + "/actions/runs/" + d["run_id"] + "/artifacts"
        inventory = self.api.pages(endpoint, "artifacts")
        guard.require(inventory["total_count"] == 14 and {a["name"] for a in inventory["artifacts"]} == set(expected),
                      "fixture-artifact-inventory-differs")
        for artifact in inventory["artifacts"]:
            identity, digest = expected[artifact["name"]]
            guard.require(str(artifact["id"]) == identity, "fixture-artifact-id-differs")
            f._artifact_record(artifact, artifact["name"], d["run_id"], d["control_sha"], digest)
        self.sleep(2)
        guard.require(inventory == self.api.pages(endpoint, "artifacts"), "fixture-artifact-inventory-moved")
        guard.require(self.run_metadata(d["run_id"]).get("run_attempt") == 1
                      and self.current_main_sha() == current_control, "fixture-latest-attempt-or-main-moved")
        return True

    def authenticate_public_fixture(self, descriptor_bytes, expected_control_sha):
        """Authenticate and return exact public signed result bytes; no secrets.

        For launch/bootstrap authority normal clean-current-main mode is required.
        Candidate audit is intentionally excluded from this public authority API.
        """
        guard.require_current_control(self, expected_control_sha)
        subject = guard._fixture(self, guard.descriptor(descriptor_bytes, fixture=True), expected_control_sha)
        guard.require_current_control(self, expected_control_sha)
        return subject

    def audit_public(self, fixture_descriptor, predecessor_descriptor=None, predecessor_state=None):
        """Real metadata/signatures; no secret contents, provider access or permit."""
        current = self.current_main_sha()
        guard.require(self._head() == current, "candidate-base-is-not-current-main")
        d = guard.descriptor(fixture_descriptor, fixture=True)
        guard._fixture(self, d, current)
        if predecessor_descriptor is not None:
            state = guard.public_json(predecessor_state)
            previous = state.get("lineage", {}).get("phase")
            guard.require(previous in guard.PHASES[:-1], "audit-predecessor-phase-invalid")
            context = guard.LaunchContext(current, guard.PHASES[guard.PHASES.index(previous) + 1], fixture_descriptor,
                                          predecessor_descriptor, predecessor_state)
            guard._predecessor(self, context, guard.descriptor(predecessor_descriptor, fixture=False))
        guard.require(self.current_main_sha() == current, "public-audit-main-moved")
        return {"public_fixture_verified": True, "predecessor_verified": predecessor_descriptor is not None,
                "candidate_audit": self.candidate_audit, "launch_authorized": False}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("audit-public", "check-launch"))
    parser.add_argument("--control-root", required=True, type=Path)
    parser.add_argument("--fixture-evidence", required=True, type=Path)
    parser.add_argument("--phase", choices=guard.PHASES, default="baseline")
    parser.add_argument("--predecessor-evidence", type=Path)
    parser.add_argument("--predecessor-state", type=Path)
    args = parser.parse_args(argv)
    try:
        adapter = GitHubEvidence(args.control_root, candidate_audit=args.command == "audit-public")
        fixture = args.fixture_evidence.read_bytes()
        predecessor = args.predecessor_evidence.read_bytes() if args.predecessor_evidence else None
        state = args.predecessor_state.read_bytes() if args.predecessor_state else None
        if args.command == "audit-public":
            outcome = adapter.audit_public(fixture, predecessor, state)
        else:
            context = guard.LaunchContext(adapter.current_main_sha(), args.phase, fixture, predecessor, state)
            guard.require_launch_ready(adapter, context)
            adapter.require_provider_parity(context)
            outcome = {"launch_prerequisites_verified": True, "launch_authorized": False}
        print(json.dumps(outcome, sort_keys=True))
        return 0
    except Exception as exc:
        code = str(exc) if isinstance(exc, guard.LaunchBlocked) else "public-evidence-verification-failed"
        operation = adapter.api.last_operation if "adapter" in locals() else "initialization"
        print("READ-ONLY CHECK BLOCKED: " + code + " (" + operation + ")")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
