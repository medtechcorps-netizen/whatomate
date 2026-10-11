"""Pure compatibility qualification over trusted, externally collected facts.

These immutable types are data contracts, not cryptographic capabilities. The
future adapter must verify signatures, actual run/attempt metadata, source and
fresh latest-record identity before constructing them. This module performs
no verification command, network, Git, clock or provider I/O. Missing coverage
cannot establish that a deleted run never contained a negative result.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from enum import Enum
import hashlib
import importlib.util
from pathlib import Path
import re
import sys

import ship_common as common
import pending_binding as pending_wire
from pending_source import PendingBinding

# Import only the pure proof module under a distinct name; importing the
# producer CLI would also import operational image/runtime modules. Loading
# trusted Python source here is ordinary module import, not evidence I/O.
_SPEC = importlib.util.spec_from_file_location(
    "_rereply_compat_measurement_proof", Path(__file__).resolve().parents[1] / "compat/proof.py")
_proof = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _proof
_SPEC.loader.exec_module(_proof)

REPOSITORY = common.REPOSITORY
WORKFLOW = ".github/workflows/compat.yml"
REF = "refs/heads/main"
SIGNER = f"https://github.com/{REPOSITORY}/{WORKFLOW}@{REF}"
ISSUER = "https://token.actions.githubusercontent.com"
PREDICATE = "https://slsa.dev/provenance/v1"
JOBS = ("release-preview", "compat-run", "compat", "compat-attest")
MIGRATIONS = ("rw", "ro", "negative_control")
SERVERS = ("boot", "read", "write", "reverse", "workers")
CATALOG_PATHS = tuple(sorted((
    "internal/dbcatalog/golden/catalog.json", "internal/dbcatalog/golden/global_tables.json",
    "internal/dbcatalog/golden/history.json", "internal/dbcatalog/golden/production-v0.json",
    "internal/dbcatalog/shape/production-v0.sql")))
MAX_ATTEMPTS = 500
MAX_EVIDENCE_BYTES = 12 * 1024 * 1024
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
_ACTIVE = frozenset(("queued", "in_progress", "waiting", "pending", "requested"))
_CONCLUSIONS = frozenset(("success", "failure", "cancelled", "skipped", "timed_out", "action_required", "neutral", "stale"))


class CompatRefusal(Exception):
    """Only module-owned constant code/detail strings are emitted."""
    def __init__(self, code: str, detail: str):
        self.code, self.detail = code, detail
        super().__init__(code + ":" + detail)


def _need(ok, detail="invalid-evidence", code="compat-stale"):
    if not ok:
        raise CompatRefusal(code, detail)


def _string(value, pattern):
    return type(value) is str and pattern.fullmatch(value) is not None


def _number(value, maximum=10**16-1):
    return type(value) is int and 1 <= value <= maximum


@dataclass(frozen=True, slots=True)
class ReleaseIdentity:
    release: str
    manifest_sha256: str
    sha: str
    web_digest: str


@dataclass(frozen=True, slots=True)
class Classification:
    kind: str
    dormant: bool
    reasons: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class CatalogBlob:
    path: str
    sha256: str


@dataclass(frozen=True, slots=True)
class CatalogTuple:
    catalog_version: int
    min_reader_version: int
    catalog_sha256: str
    data_steps_sha256: str
    seeds_sha256: str


@dataclass(frozen=True, slots=True)
class OriginIdentity:
    release: str
    manifest_sha256: str


@dataclass(frozen=True, slots=True)
class DatabaseBase:
    """Adapter-supplied origin data, never proof of source or live row state."""
    mode: str
    source_sha: str
    origin: OriginIdentity | None
    catalog: CatalogTuple | None


@dataclass(frozen=True, slots=True)
class SourceFacts:
    sha: str
    base: ReleaseIdentity
    classification: Classification
    expected_after_catalog_sha256: str
    catalog_authority: tuple[CatalogBlob, ...]
    database_base: DatabaseBase
    previous_web_source_sha: str
    pending_binding: PendingBinding | None = None  # Historical context2 custody only.


@dataclass(frozen=True, slots=True)
class NormalRequest:
    source: SourceFacts
    latest: ReleaseIdentity
    observation_id: str
    mode: str = "normal"


@dataclass(frozen=True, slots=True)
class VerifiedSubject:
    subject_name: str
    subject_sha256: str
    predicate_type: str
    issuer: str
    signer_uri: str
    signer_sha: str
    source_sha: str
    source_ref: str
    runner: str
    run_id: int
    run_attempt: int
    verification_reference: str


@dataclass(frozen=True, slots=True)
class ArtifactFile:
    name: str
    sha256: str
    kind: str = "regular"


@dataclass(frozen=True, slots=True)
class Artifact:
    artifact_id: int
    name: str
    run_id: int
    run_attempt: int
    sha: str
    expired: bool
    files: tuple[ArtifactFile, ...]


@dataclass(frozen=True, slots=True, repr=False)
class AuthenticatedPair:
    result: bytes
    context: bytes
    result_subject: VerifiedSubject
    context_subject: VerifiedSubject
    artifact: Artifact


class MissingEvidence(Enum):
    ACTIVE = "active"
    NEVER_STARTED = "never-started-proven"
    STARTED_INCOMPLETE = "started-incomplete"
    EXPIRED = "artifact-expired"
    SIGNING_FAILED = "signing-failed"
    MISSING = "missing"
    UNKNOWN = "unknown"
    UNQUALIFIED = "unqualified"


@dataclass(frozen=True, slots=True)
class Job:
    name: str
    job_id: int
    check_run_id: int
    app_id: int
    run_id: int
    run_attempt: int
    sha: str
    status: str
    conclusion: str | None


@dataclass(frozen=True, slots=True)
class ExecutionWindow:
    """Actual accounted job execution, supplied by the metadata adapter.

    Neither an attempt number nor run_started_at alone proves this window.
    These are data facts, not a signature or a collector capability.
    """
    started_at: datetime
    completed_at: datetime
    fresh: bool


@dataclass(frozen=True, slots=True, repr=False)
class Attempt:
    repository: str
    workflow_path: str
    sha: str
    workflow_sha: str
    event: str
    ref: str
    run_id: int
    run_number: int
    run_attempt: int
    latest_run_attempt: int
    status: str
    conclusion: str | None
    requested_at: datetime | None
    started_at: datetime | None
    completed_at: datetime | None
    jobs: tuple[Job, ...]
    measurement: AuthenticatedPair | MissingEvidence
    execution_window: ExecutionWindow | None = None


@dataclass(frozen=True, slots=True, repr=False)
class VisibleHistory:
    sha: str
    repository: str
    workflow_path: str
    observation_id: str
    observed_at: datetime
    attempts: tuple[Attempt, ...]
    enumeration: str = "complete-visible"
    whole_run_deletion_coverage: str = "unavailable"


@dataclass(frozen=True, slots=True)
class BoundMeasurement:
    latest: ReleaseIdentity
    selected: ReleaseIdentity
    requested_target: str | None
    classification: Classification
    preview: Classification
    catalog_sha256: str
    forward: bool
    migrations: tuple[bool, ...]
    servers: tuple[bool, ...]
    reversible: bool
    result_sha256: str
    context_sha256: str
    context_version: int
    database_base: DatabaseBase | None
    selected_web_source_sha: str | None
    pending_binding: PendingBinding | None = None

    @property
    def positive(self):
        # Context1 flags remain actual measurements for negative auditing;
        # protocol eligibility is a separate qualification predicate.
        return self.forward and self.reversible and all(self.migrations) and all(self.servers)


@dataclass(frozen=True, slots=True)
class QualifiedCompat:
    candidate_sha: str
    previous: ReleaseIdentity
    run_id: int
    run_number: int
    run_attempt: int
    requested_at: datetime | None
    started_at: datetime
    completed_at: datetime
    artifact_id: int
    artifact_name: str
    result_sha256: str
    context_sha256: str
    signer_uri: str
    workflow_sha: str
    classification: Classification
    catalog_sha256: str
    catalog_authority: tuple[CatalogBlob, ...]
    forward: bool
    old_migrate_ok: tuple[bool, ...]
    old_server_ok: tuple[bool, ...]
    reversible: bool
    observation_id: str
    execution_window: ExecutionWindow
    database_base: DatabaseBase
    previous_web_source_sha: str
    pending_binding: PendingBinding | None = None

    @property
    def old_server(self):
        return all(self.old_server_ok)


class BypassState(Enum):
    VETO_AUTHENTICATED_NEGATIVE = "veto-authenticated-negative"
    UNAVAILABLE_HISTORY_COVERAGE = "unavailable-history-coverage"


@dataclass(frozen=True, slots=True)
class NegativeReference:
    run_id: int
    run_attempt: int
    result_sha256: str
    context_sha256: str


@dataclass(frozen=True, slots=True)
class BypassHistory:
    state: BypassState
    negatives: tuple[NegativeReference, ...]


def _release(value):
    _need(type(value) is ReleaseIdentity, "release-shape")
    try:
        _proof.validate_rollback_target(value.release)
    except _proof.EvidenceError:
        raise CompatRefusal("compat-stale", "release-shape") from None
    _need(_string(value.manifest_sha256, _HASH) and _string(value.sha, _SHA)
          and _string(value.web_digest, _DIGEST), "release-shape")
    return value


def _release_dict(value):
    _need(type(value) is dict and set(value) == {"release", "manifest_sha256", "sha", "web_digest"}, "release-shape")
    return _release(ReleaseIdentity(**value))


def _database_base(value, latest):
    _need(type(value) is DatabaseBase and type(value.mode) is str
          and _string(value.source_sha, _SHA), "database-base-shape")
    if value.mode == "legacy-source-only":
        _need(value.origin is None and value.catalog is None and value.source_sha == latest.sha,
              "database-base-legacy")
    else:
        _need(value.mode == "retained-live-v2", "database-base-mode")
        origin, catalog = value.origin, value.catalog
        _need(type(origin) is OriginIdentity and type(origin.release) is str
              and common.RECORD_TAG_RE.fullmatch(origin.release) is not None
              and _string(origin.manifest_sha256, _HASH), "database-base-origin")
        _need(type(catalog) is CatalogTuple
              and all(type(v) is int and 0 <= v <= 2_147_483_647
                      for v in (catalog.catalog_version, catalog.min_reader_version))
              and catalog.min_reader_version <= catalog.catalog_version
              and all(_string(v, _HASH) for v in
                      (catalog.catalog_sha256, catalog.data_steps_sha256, catalog.seeds_sha256)),
              "database-base-catalog")
    return value


def _database_base_dict(value, latest):
    _need(type(value) is dict and set(value) == {"mode", "source_sha", "origin", "catalog"},
          "database-base-shape")
    origin, catalog = value["origin"], value["catalog"]
    if origin is not None:
        _need(type(origin) is dict and set(origin) == {"release", "manifest_sha256"}, "database-base-origin")
        origin = OriginIdentity(**origin)
    if catalog is not None:
        _need(type(catalog) is dict and set(catalog) ==
              {"catalog_version", "min_reader_version", "catalog_sha256", "data_steps_sha256", "seeds_sha256"},
              "database-base-catalog")
        catalog = CatalogTuple(**catalog)
    return _database_base(DatabaseBase(value["mode"], value["source_sha"], origin, catalog), latest)


def _time(value):
    return type(value) is datetime and value.utcoffset() == timedelta(0)


def _state(status, conclusion):
    return (type(status) is str and ((status == "completed" and type(conclusion) is str and conclusion in _CONCLUSIONS)
            or (status in _ACTIVE and conclusion is None)))


def _attempt(value, sha, observed_at):
    _need(type(value) is Attempt, "attempt-shape")
    _need(value.repository == REPOSITORY and value.workflow_path == WORKFLOW and value.sha == sha
          and value.workflow_sha == sha and value.event in ("push", "workflow_dispatch") and value.ref == REF,
          "attempt-identity")
    _need(_number(value.run_id) and _number(value.run_number) and _number(value.run_attempt, 9999)
          and _number(value.latest_run_attempt, 9999) and value.run_attempt <= value.latest_run_attempt,
          "attempt-identity")
    _need(_state(value.status, value.conclusion), "attempt-state")
    times = (value.requested_at, value.started_at, value.completed_at)
    _need(all(t is None or (_time(t) and t <= observed_at) for t in times), "attempt-timing")
    known = [t for t in times if t is not None]
    _need(known == sorted(known) and (value.status == "completed" or value.completed_at is None), "attempt-timing")
    _need(type(value.jobs) is tuple and len(value.jobs) <= 4, "job-shape")
    names, identifiers, checks = set(), set(), set()
    for job in value.jobs:
        _need(type(job) is Job, "job-shape")
        _need(type(job.name) is str and job.name in JOBS and job.name not in names
              and _number(job.job_id) and job.job_id not in identifiers
              and _number(job.check_run_id) and job.check_run_id not in checks,
              "job-identity")
        _need(type(job.app_id) is int and job.app_id == 15368 and job.run_id == value.run_id
              and type(job.run_id) is int and job.run_attempt == value.run_attempt and type(job.run_attempt) is int
              and job.sha == sha and _state(job.status, job.conclusion), "job-identity")
        names.add(job.name); identifiers.add(job.job_id); checks.add(job.check_run_id)
    _need(type(value.measurement) in (AuthenticatedPair, MissingEvidence), "measurement-shape")
    window = value.execution_window
    _need(window is None or type(window) is ExecutionWindow, "execution-window-shape")
    if window is not None:
        _need(type(window.fresh) is bool and _time(window.started_at) and _time(window.completed_at)
              and window.started_at <= window.completed_at <= observed_at, "execution-window-timing")
        _need(value.status == "completed" and value.started_at is not None and value.completed_at is not None
              and value.started_at <= window.started_at and window.completed_at == value.completed_at
              and names == set(JOBS) and all(j.status == "completed" for j in value.jobs)
              and any(j.conclusion != "skipped" for j in value.jobs), "execution-window-incomplete")


def _history(value, sha, observation_id=None):
    _need(_string(sha, _SHA) and type(value) is VisibleHistory, "history-shape")
    _need(value.sha == sha and value.repository == REPOSITORY and value.workflow_path == WORKFLOW,
          "history-identity")
    _need(_string(value.observation_id, _HASH) and (observation_id is None or value.observation_id == observation_id),
          "history-observation")
    _need(_time(value.observed_at) and type(value.attempts) is tuple and len(value.attempts) <= MAX_ATTEMPTS,
          "history-bound")
    _need(value.enumeration in ("complete-visible", "incomplete")
          and value.whole_run_deletion_coverage == "unavailable", "history-coverage")
    keys, runs, numbers, total = set(), {}, {}, 0
    for item in value.attempts:
        _attempt(item, sha, value.observed_at)
        key = (item.run_id, item.run_attempt)
        _need(key not in keys, "duplicate-attempt"); keys.add(key)
        identity = (item.run_number, item.latest_run_attempt, item.event)
        _need(item.run_id not in runs or runs[item.run_id] == identity, "run-identity")
        _need(item.run_number not in numbers or numbers[item.run_number] == item.run_id, "run-identity")
        runs[item.run_id] = identity; numbers[item.run_number] = item.run_id
        if type(item.measurement) is AuthenticatedPair:
            pair = item.measurement
            _need(type(pair.result) is bytes and type(pair.context) is bytes, "pair-shape")
            _need(0 < len(pair.result) <= 16384 and 0 < len(pair.context) <= 4096, "pair-bound")
            total += len(pair.result) + len(pair.context)
    _need(total <= MAX_EVIDENCE_BYTES, "history-bound")
    if value.enumeration == "complete-visible":
        for run_id, (_, latest, _) in runs.items():
            attempts = sorted((a for a in value.attempts if a.run_id == run_id), key=lambda a: a.run_attempt)
            _need({a.run_attempt for a in attempts} == set(range(1, latest+1)),
                  "history-attempt-gap")
            requested = [a.requested_at for a in attempts if a.requested_at is not None]
            _need(requested == sorted(requested), "history-attempt-order")
            started = [a.started_at for a in attempts if a.started_at is not None]
            _need(started == sorted(started), "history-attempt-order")
            windows = [a.execution_window for a in attempts
                       if a.execution_window is not None and a.execution_window.fresh]
            for moments in ([w.started_at for w in windows], [w.completed_at for w in windows]):
                _need(moments == sorted(moments), "history-window-order")


def _request_common(value):
    _need(type(value) is NormalRequest and value.mode == "normal", "request-shape")
    _need(type(value.source) is SourceFacts and _string(value.observation_id, _HASH), "request-shape")
    source = value.source
    _need(_string(source.sha, _SHA) and _release(source.base) == _release(value.latest), "source-base")
    c = source.classification
    _need(type(c) is Classification and type(c.reasons) is tuple and not c.reasons,
          "classification-blocked")
    _need(type(c.kind) is str and type(c.dormant) is bool, "classification-unsupported")
    _need(_string(source.expected_after_catalog_sha256, _HASH), "catalog-expectation")
    _need(type(source.catalog_authority) is tuple and len(source.catalog_authority) == len(CATALOG_PATHS)
          and all(type(b) is CatalogBlob and _string(b.sha256, _HASH) for b in source.catalog_authority)
          and tuple(b.path for b in source.catalog_authority) == CATALOG_PATHS, "catalog-authority")
    _database_base(source.database_base, value.latest)
    _need(_string(source.previous_web_source_sha, _SHA), "previous-web-source")


def _pending(value, candidate_sha, previous_sha, authority):
    active = tuple((name, next(row.sha256 for row in authority if row.path == path))
        for name, path in (("active_catalog", "internal/dbcatalog/golden/catalog.json"),
                           ("active_globals", "internal/dbcatalog/golden/global_tables.json"),
                           ("active_history", "internal/dbcatalog/golden/history.json")))
    try:
        return pending_wire.validate_against(value, candidate_sha=candidate_sha,
            previous_sha=previous_sha, active_artifacts=active)
    except pending_wire.PendingBindingRefusal:
        raise CompatRefusal("compat-stale", "pending-binding") from None


def _request(value):
    _request_common(value)
    source, c = value.source, value.source.classification
    binding = _pending(source.pending_binding, source.sha, value.latest.sha, source.catalog_authority)
    _need((c.kind == "none" and not c.dormant) or (c.kind == "data" and c.dormant)
          or (c.kind == "expand" and c.dormant and binding.target_version is not None),
          "classification-unsupported")


def _request_context2_custody(value):
    """Exact historical data validation, never a current predeploy fallback."""
    _request_common(value)
    c = value.source.classification
    _need(value.source.pending_binding is None
          and ((c.kind == "none" and not c.dormant) or (c.kind == "data" and c.dormant)),
          "context2-custody-only")


def _subject(value, name, raw, attempt):
    _need(type(value) is VerifiedSubject, "subject-unverified")
    _need(value.subject_name == name and value.subject_sha256 == hashlib.sha256(raw).hexdigest()
          and value.predicate_type == PREDICATE and value.issuer == ISSUER and value.signer_uri == SIGNER
          and value.signer_sha == attempt.sha and value.source_sha == attempt.sha and value.source_ref == REF
          and value.runner == "github-hosted" and type(value.run_id) is int and value.run_id == attempt.run_id
          and type(value.run_attempt) is int and value.run_attempt == attempt.run_attempt
          and _string(value.verification_reference, _HASH), "subject-binding")


def parse_authenticated_pair(pair: AuthenticatedPair, attempt: Attempt) -> BoundMeasurement:
    """Validate a pair whose cryptographic verification is the caller's duty."""
    try:
        _need(type(attempt) is Attempt and _string(attempt.sha, _SHA)
              and _number(attempt.run_id) and _number(attempt.run_number)
              and _number(attempt.run_attempt, 9999), "attempt-shape")
        _need(type(pair) is AuthenticatedPair and type(pair.result) is bytes and type(pair.context) is bytes
              and 0 < len(pair.result) <= 16384 and 0 < len(pair.context) <= 4096, "pair-bound")
        _subject(pair.result_subject, "compat-result.json", pair.result, attempt)
        _subject(pair.context_subject, "compat-context.json", pair.context, attempt)
        artifact = pair.artifact
        _need(type(artifact) is Artifact and _number(artifact.artifact_id) and artifact.expired is False
              and artifact.sha == attempt.sha and type(artifact.run_id) is int and artifact.run_id == attempt.run_id
              and type(artifact.run_attempt) is int and artifact.run_attempt == attempt.run_attempt
              and artifact.name == _proof.artifact_name(attempt.sha, attempt.run_attempt), "artifact-binding")
        _need(type(artifact.files) is tuple and len(artifact.files) == 2
              and all(type(f) is ArtifactFile and f.kind == "regular" for f in artifact.files), "artifact-inventory")
        files = {f.name: f.sha256 for f in artifact.files}
        _need(files == {"compat-result.json": hashlib.sha256(pair.result).hexdigest(),
                        "compat-context.json": hashlib.sha256(pair.context).hexdigest()}, "artifact-inventory")
        result = common.loads_strict(pair.result)
        _need(_proof.result_bytes(result) == pair.result, "result-noncanonical")
        context = common.loads_strict(pair.context)
        keys = {"version", "repository", "workflow_path", "workflow_sha", "event", "ref", "sha", "run_id",
                "run_attempt", "requested_rollback_target", "latest", "selected", "preview", "result_sha256"}
        _need(type(context) is dict and type(context.get("version")) is int
              and context["version"] in (1, 2, 3), "context-version")
        version = context["version"]
        if version in (2, 3):
            keys |= {"database_base", "selected_web_source_sha"}
        if version == 3:
            keys.add("pending")
        _need(type(context) is dict and set(context) == keys and common.canonical_file_bytes(context) == pair.context,
              "context-shape")
        _need(context["repository"] == REPOSITORY
              and context["workflow_path"] == WORKFLOW and context["workflow_sha"] == attempt.sha
              and context["sha"] == attempt.sha and context["ref"] == REF and attempt.ref == REF
              and context["event"] == attempt.event and attempt.event in ("push", "workflow_dispatch")
              and attempt.repository == REPOSITORY and attempt.workflow_path == WORKFLOW
              and attempt.workflow_sha == attempt.sha, "context-identity")
        for name, expected in (("run_id", attempt.run_id), ("run_attempt", attempt.run_attempt)):
            _need(type(context[name]) is int and context[name] == expected and result[name] == expected, "context-attempt")
        _need(result["sha"] == attempt.sha and context["result_sha256"] == hashlib.sha256(pair.result).hexdigest(),
              "context-result")
        latest, selected = _release_dict(context["latest"]), _release_dict(context["selected"])
        target = context["requested_rollback_target"]
        if target is None:
            _need(selected == latest, "context-selection")
        else:
            _proof.validate_rollback_target(target)
            _need(attempt.event == "workflow_dispatch" and selected.release == target, "context-selection")
        database_base = actual_source = pending = None
        if version in (2, 3):
            # This protocol defines normal forward evidence only. Context1
            # explicit-target observations remain parseable for audit, not a
            # newly invented exact-target/live rollback qualification.
            _need(target is None, "context2-normal-only")
            database_base = _database_base_dict(context["database_base"], latest)
            actual_source = context["selected_web_source_sha"]
            _need(_string(actual_source, _SHA), "previous-web-source")
        if version == 3:
            pending = pending_wire.parse_wire(context["pending"])
            pending_wire.validate_against(pending, candidate_sha=attempt.sha,
                previous_sha=latest.sha, active_artifacts=pending.active_artifacts)
        _need(result["previous_release"] == selected.release and result["previous_web_digest"] == selected.web_digest,
              "result-selection")
        preview = context["preview"]
        _need(type(preview) is dict and set(preview) == {"kind", "dormant", "blocked"}
              and type(preview["dormant"]) is bool and preview["blocked"] is False
              and ((preview["kind"] == "none" and not preview["dormant"])
                   or (preview["kind"] == "data" and preview["dormant"])
                   or (version == 3 and preview["kind"] == "expand" and preview["dormant"]
                       and pending.target_version is not None)), "context-preview")
        classification = Classification(**result["classification"])
        preview_class = Classification(preview["kind"], preview["dormant"])
        _need(target is not None or classification == preview_class, "context-classification")
        return BoundMeasurement(latest, selected, target, classification, preview_class, result["catalog_sha256"],
            result["forward"], tuple(result["old_migrate_ok"][key] for key in MIGRATIONS),
            tuple(result["old_server_ok"][key] for key in SERVERS), result["reversible"],
            hashlib.sha256(pair.result).hexdigest(), hashlib.sha256(pair.context).hexdigest(),
            version, database_base, actual_source, pending)
    except (common.ReleaseError, _proof.EvidenceError, ValueError, TypeError, KeyError, RecursionError):
        raise CompatRefusal("compat-stale", "pair-invalid") from None


def _measurements(history):
    result = []
    for attempt in history.attempts:
        measured = None
        if type(attempt.measurement) is AuthenticatedPair:
            try:
                measured = parse_authenticated_pair(attempt.measurement, attempt)
            except CompatRefusal:
                pass  # Unknown evidence, never a favorable or semantic-false flag.
        result.append((attempt, measured))
    return result


def _identity_current(measured, request):
    return (measured is not None and measured.requested_target is None
            and measured.latest == measured.selected == request.latest
            and measured.classification == measured.preview == request.source.classification
            and measured.catalog_sha256 == request.source.expected_after_catalog_sha256)


def _current(measured, request):
    return (_identity_current(measured, request) and measured.context_version == 3
            and measured.database_base == request.source.database_base
            and measured.selected_web_source_sha == request.source.previous_web_source_sha
            and measured.pending_binding == request.source.pending_binding)


def _current_context2_custody(measured, request):
    return (_identity_current(measured, request) and measured.context_version == 2
            and measured.database_base == request.source.database_base
            and measured.selected_web_source_sha == request.source.previous_web_source_sha
            and measured.pending_binding is None and request.source.pending_binding is None)


def _negative_current(measured, request):
    # Old actual false flags retain their audit meaning. Old positives never
    # gain current protocol eligibility or fabricated pending observations.
    return (_current(measured, request) or (_identity_current(measured, request)
        and (measured.context_version == 1 or (measured.context_version == 2
            and measured.database_base == request.source.database_base
            and measured.selected_web_source_sha == request.source.previous_web_source_sha))))


def _successful(attempt):
    return (attempt.status == "completed" and attempt.conclusion == "success"
            and attempt.execution_window is not None and attempt.execution_window.fresh
            and {j.name for j in attempt.jobs} == set(JOBS)
            and all(j.status == "completed" and j.conclusion == "success" for j in attempt.jobs))


def _negative(measured):
    if not all(measured.servers):
        raise CompatRefusal("compat-old-server-incompatible", "authenticated-negative")
    if not measured.reversible or not all(measured.migrations):
        raise CompatRefusal("irreversible-needs-fix-forward-mode", "authenticated-negative")
    raise CompatRefusal("compat-stale", "forward-false")


def require_compat(request: NormalRequest, history: VisibleHistory) -> QualifiedCompat:
    _request(request)
    return _qualify(request, history, _current)


def _require_context2_custody(request: NormalRequest, history: VisibleHistory) -> QualifiedCompat:
    """Replay old same-run custody only; external original execution checks apply.

    The closed codec selects this private path for original versions1..4.
    Types and this function are not custody or signature capabilities.
    """
    _request_context2_custody(request)
    return _qualify(request, history, _current_context2_custody)


def _qualify(request, history, current):
    _history(history, request.source.sha, request.observation_id)
    _need(history.enumeration == "complete-visible", "history-incomplete")
    rows = _measurements(history)
    positives = [(a, m) for a, m in rows if current(m, request) and m.positive and _successful(a)]
    if not positives:
        negatives = [(a, m) for a, m in rows if not (m is None or m.positive)
                     and _negative_current(m, request)]
        if negatives:
            _negative(max(negatives, key=lambda row: (row[0].run_number, row[0].run_attempt))[1])
        raise CompatRefusal("compat-missing" if not rows else "compat-stale", "no-qualified-positive")
    selected, measured = max(positives, key=lambda row: (row[0].run_number, row[0].run_attempt))
    for other, observation in rows:
        if other is selected:
            continue
        # Even a larger run_number failure may predate a fresh older-run rerun.
        # Conversely an older run's rerun may execute later. Actual interval
        # evidence, not list order or IDs, controls the negative/incomplete veto.
        window = other.execution_window
        earlier = (window is not None and window.fresh
                   and window.completed_at < selected.execution_window.started_at)
        if earlier:
            continue
        _need(window is not None and window.fresh,
              "later-or-unknown-timing")
        if observation is not None and not observation.positive:
            _negative(observation)
        _need(current(observation, request) and observation.positive and _successful(other),
              "later-or-overlapping-incomplete")
    pair = selected.measurement
    return QualifiedCompat(request.source.sha, request.latest, selected.run_id, selected.run_number,
        selected.run_attempt, selected.requested_at, selected.started_at, selected.completed_at,
        pair.artifact.artifact_id, pair.artifact.name, measured.result_sha256, measured.context_sha256,
        pair.result_subject.signer_uri, pair.result_subject.signer_sha, measured.classification,
        measured.catalog_sha256, request.source.catalog_authority, measured.forward, measured.migrations,
        measured.servers, measured.reversible, request.observation_id, selected.execution_window,
        measured.database_base, measured.selected_web_source_sha, measured.pending_binding)


def assess_bypass_history(sha: str, history: VisibleHistory) -> BypassHistory:
    _history(history, sha)
    negatives = tuple(sorted((NegativeReference(a.run_id, a.run_attempt, m.result_sha256, m.context_sha256)
                              for a, m in _measurements(history) if m is not None and not m.positive),
                             key=lambda n: (n.run_id, n.run_attempt)))
    return BypassHistory(BypassState.VETO_AUTHENTICATED_NEGATIVE if negatives
                         else BypassState.UNAVAILABLE_HISTORY_COVERAGE, negatives)
