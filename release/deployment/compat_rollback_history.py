"""Pure all-head rollback evidence selection, never operational authority.

Source and signature collection remain external. Every execution window is
replayed from retained closed API facts and that head's exact workflow bytes.
No transport, clock, source read, callback or rollback policy is invoked here.
"""
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
import re

import compat_envelope as envelope
import compat_evidence as evidence
import ship_common as common

WorkflowObservation = envelope.WorkflowObservation
EnvelopeFacts = envelope.EnvelopeFacts
MAX_ATTEMPTS = 500
MAX_HEADS = 501
MAX_RAW_BYTES = 12 * 1024 * 1024
MAX_COMMANDS = 6000
MAX_CAPTURED_BYTES = 128 * 1024 * 1024


@dataclass(frozen=True, slots=True)
class RollbackAttempt:
    envelope: EnvelopeFacts = field(repr=False)
    measurement: evidence.AuthenticatedPair | evidence.MissingEvidence = field(repr=False)


@dataclass(frozen=True, slots=True, repr=False)
class RollbackHistory:
    producer_sha: str
    repository: str
    workflow_path: str
    observation_id: str
    observed_at: datetime
    workflows: tuple[WorkflowObservation, ...]
    attempts: tuple[RollbackAttempt, ...]
    enumeration: str = 'complete-visible'
    whole_run_deletion_coverage: str = 'unavailable'


@dataclass(frozen=True, slots=True)
class CollectedRollbackHistory:
    history: RollbackHistory = field(repr=False)
    issues: tuple[str, ...]
    command_count: int
    captured_bytes: int
    transcript_sha256: str


@dataclass(frozen=True, slots=True)
class RollbackRequest:
    binding: evidence.RollbackBinding
    workflow: bytes = field(repr=False)
    observation_id: str


@dataclass(frozen=True, slots=True)
class QualifiedRollback:
    """Selected data, not a RollbackDecision or a transferable capability."""
    binding: evidence.RollbackBinding
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
    catalog_sha256: str
    forward: bool
    migrations: tuple[bool, ...]
    servers: tuple[bool, ...]
    reversible: bool
    observation_id: str
    execution_window: evidence.ExecutionWindow


def _need(ok, detail):
    if not ok:
        raise evidence.CompatRefusal('compat-stale', detail)


def _hash(value):
    return type(value) is str and re.fullmatch(r'[0-9a-f]{64}', value) is not None


def _sha(value):
    return type(value) is str and re.fullmatch(r'[0-9a-f]{40}', value) is not None


def _raw_bound(history):
    total = 0
    for source in history.workflows:
        envelope.validate_workflow(source)
        if source.raw is not None:
            total += len(source.raw)
    for row in history.attempts:
        _need(type(row) is RollbackAttempt and type(row.envelope) is EnvelopeFacts
              and type(row.envelope.raw) is bytes and 0 < len(row.envelope.raw) <= envelope.MAX_RAW_BYTES,
              'rollback-history-envelope')
        total += len(row.envelope.raw)
        pair = row.measurement
        _need(type(pair) in (evidence.AuthenticatedPair, evidence.MissingEvidence), 'rollback-history-measurement')
        if type(pair) is evidence.AuthenticatedPair:
            _need(type(pair.result) is bytes and 0 < len(pair.result) <= 16384
                  and type(pair.context) is bytes and 0 < len(pair.context) <= 4096,
                  'rollback-history-pair-bound')
            total += len(pair.result) + len(pair.context)
    _need(total <= MAX_RAW_BYTES, 'rollback-history-bound')


def _pair_inventory(pair, replayed):
    if type(pair) is not evidence.AuthenticatedPair:
        return
    artifact = pair.artifact
    attempt = replayed.attempt
    _need(type(artifact) is evidence.Artifact, 'rollback-history-artifact')
    expected = f'compat-result-{attempt.sha}-{attempt.run_attempt}'
    metas = [item for item in replayed.artifacts if item.name == expected]
    _need(len(metas) == 1, 'rollback-history-artifact')
    meta = metas[0]
    _need(type(artifact.artifact_id) is int and artifact.artifact_id == meta.id
          and artifact.name == meta.name and artifact.expired is False and not meta.expired
          and artifact.sha == attempt.sha and type(artifact.run_id) is int and artifact.run_id == attempt.run_id
          and type(artifact.run_attempt) is int and artifact.run_attempt == attempt.run_attempt
          and attempt.started_at is not None and attempt.started_at <= meta.created
          and (attempt.completed_at is None or meta.updated <= attempt.completed_at), 'rollback-history-artifact')


def _replay_history(history, *, producer_sha, observation_id=None):
    _need(type(history) is RollbackHistory and _sha(producer_sha)
          and history.producer_sha == producer_sha
          and history.repository == evidence.REPOSITORY and history.workflow_path == evidence.WORKFLOW,
          'rollback-history-identity')
    _need(_hash(history.observation_id) and (observation_id is None or
          (_hash(observation_id) and history.observation_id == observation_id)), 'rollback-history-observation')
    _need(type(history.observed_at) is datetime and history.observed_at.tzinfo is timezone.utc
          and history.observed_at.fold == 0, 'rollback-history-clock')
    _need(type(history.workflows) is tuple and 1 <= len(history.workflows) <= MAX_HEADS
          and type(history.attempts) is tuple and len(history.attempts) <= MAX_ATTEMPTS,
          'rollback-history-bound')
    _need(type(history.enumeration) is str and history.enumeration in ('complete-visible', 'incomplete')
          and history.whole_run_deletion_coverage == 'unavailable', 'rollback-history-coverage')
    _raw_bound(history)
    shas = [item.sha for item in history.workflows]
    _need(shas == sorted(set(shas)), 'rollback-history-workflows')
    sources = {item.sha:item for item in history.workflows}
    heads, rows, identities, runs, numbers = {producer_sha}, [], set(), {}, {}
    jobs, referenced, listed, suites, artifact_ids = set(), set(), {}, {}, {}
    workflow_ids = set()
    for row in history.attempts:
        value = common.loads_strict(row.envelope.raw)
        _need(type(value) is dict and type(value.get('run')) is dict, 'rollback-history-envelope')
        sha = value['run'].get('head_sha')
        _need(_sha(sha) and sha in sources, 'rollback-history-workflows')
        heads.add(sha)
        result = envelope.replay(sources[sha], row.envelope, observed_at=history.observed_at)
        attempt = replace(result.attempt, measurement=row.measurement)
        evidence._attempt(attempt, sha, history.observed_at)
        key = attempt.run_id, attempt.run_attempt
        _need(key not in identities, 'rollback-history-duplicate'); identities.add(key)
        identity = (sha, attempt.run_number, attempt.latest_run_attempt, attempt.event, result.created_at)
        _need(attempt.run_id not in runs or runs[attempt.run_id][0] == identity, 'rollback-history-run')
        _need(attempt.run_number not in numbers or numbers[attempt.run_number] == attempt.run_id,
              'rollback-history-run')
        numbers[attempt.run_number] = attempt.run_id
        if attempt.run_id in runs:
            _need(runs[attempt.run_id][1] == result.artifacts_raw, 'rollback-history-artifacts-changed')
        runs[attempt.run_id] = identity, result.artifacts_raw
        workflow_ids.add(result.workflow_id)
        _need(len(workflow_ids) == 1, 'rollback-history-workflow-id')
        for jid in result.job_ids:
            _need(jid not in jobs, 'rollback-history-job-duplicate'); jobs.add(jid)
        for cid in result.check_ids:
            _need(cid not in referenced, 'rollback-history-check-duplicate'); referenced.add(cid)
        suite = attempt.run_id, sha, result.listed_checks
        _need(result.suite not in suites or suites[result.suite] == suite, 'rollback-history-suite-changed')
        suites[result.suite] = suite
        for cid, raw in result.listed_checks:
            check = attempt.run_id, sha, result.suite, raw
            _need(cid not in listed or listed[cid] == check, 'rollback-history-check-changed')
            listed[cid] = check
        for meta in result.artifacts:
            _need(meta.id not in artifact_ids or artifact_ids[meta.id] == (attempt.run_id, meta),
                  'rollback-history-artifact-duplicate')
            artifact_ids[meta.id] = attempt.run_id, meta
        _pair_inventory(row.measurement, result)
        rows.append((attempt, result))
    _need(heads == set(sources), 'rollback-history-workflows')
    _need(sum(value[0][2] for value in runs.values()) <= MAX_ATTEMPTS, 'rollback-history-bound')
    _need(referenced == set(listed), 'rollback-history-check-inventory')
    if history.enumeration == 'complete-visible':
        for rid, (identity, _) in runs.items():
            attempts = sorted((a for a, _ in rows if a.run_id == rid), key=lambda a:a.run_attempt)
            _need([a.run_attempt for a in attempts] == list(range(1, identity[2] + 1)), 'rollback-history-gap')
            for moments in ([a.requested_at for a in attempts if a.requested_at is not None],
                            [a.started_at for a in attempts if a.started_at is not None],
                            [a.execution_window.started_at for a in attempts if a.execution_window is not None],
                            [a.execution_window.completed_at for a in attempts if a.execution_window is not None]):
                _need(moments == sorted(moments), 'rollback-history-order')
    return tuple(a for a, _ in rows)


def _fixed(function, *args, **kwargs):
    try:
        return function(*args, **kwargs)
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, ValueError, TypeError, KeyError, AttributeError,
            RecursionError, OverflowError, UnicodeError):
        raise evidence.CompatRefusal('compat-stale', 'rollback-history-invalid') from None


def validate_history(history, *, producer_sha, observation_id=None):
    _fixed(_replay_history, history, producer_sha=producer_sha, observation_id=observation_id)
    return history


def _collected(value):
    _need(type(value) is CollectedRollbackHistory and type(value.history) is RollbackHistory,
          'rollback-collected-shape')
    validate_history(value.history, producer_sha=value.history.producer_sha,
                     observation_id=value.history.observation_id)
    allowed = {'artifact-unverified', 'inventory-changed', 'inventory-unavailable', 'collection-deadline'}
    _need(type(value.issues) is tuple and all(type(i) is str and i in allowed for i in value.issues)
          and len(set(value.issues)) == len(value.issues)
          and type(value.command_count) is int and 0 < value.command_count <= MAX_COMMANDS
          and type(value.captured_bytes) is int and 0 <= value.captured_bytes <= MAX_CAPTURED_BYTES
          and _hash(value.transcript_sha256), 'rollback-collected-metadata')
    fatal = {'inventory-changed', 'inventory-unavailable', 'collection-deadline'}
    _need(not fatal.intersection(value.issues) or value.history.enumeration == 'incomplete',
          'rollback-collected-coverage')
    return value


def validate_collected(collected):
    return _fixed(_collected, collected)


def _request(request):
    _need(type(request) is RollbackRequest, 'rollback-request-shape')
    evidence.validate_rollback_binding(request.binding)
    _need(type(request.workflow) is bytes and 0 < len(request.workflow) <= envelope.MAX_RAW_BYTES
          and _hash(request.observation_id), 'rollback-request-shape')
    _need(envelope._reviewed_steps(WorkflowObservation(request.binding.producer_sha, request.workflow)) is not None,
          'rollback-request-workflow')
    return request


def validate_request(request):
    return _fixed(_request, request)


def _measurement(attempt):
    if type(attempt.measurement) is not evidence.AuthenticatedPair:
        return None
    try:
        return evidence.parse_authenticated_rollback_pair(attempt.measurement, attempt)
    except evidence.CompatRefusal:
        try:
            return evidence.parse_authenticated_pair(attempt.measurement, attempt)
        except evidence.CompatRefusal:
            return None


def _exact(measured, request):
    return type(measured) is evidence.BoundRollbackMeasurement and measured.binding == request.binding


def _qualify(request, history):
    validate_request(request)
    attempts = _replay_history(history, producer_sha=request.binding.producer_sha,
                              observation_id=request.observation_id)
    _need(history.enumeration == 'complete-visible', 'rollback-history-incomplete')
    source = next(row for row in history.workflows if row.sha == history.producer_sha)
    _need(source.raw == request.workflow, 'rollback-request-workflow')
    rows = tuple((a, _measurement(a)) for a in attempts)
    def positive(attempt, measured):
        return (attempt.sha == request.binding.producer_sha and _exact(measured, request)
            and measured.positive and measured.catalog_sha256 == request.binding.database_base.catalog.catalog_sha256
            and evidence._successful(attempt))
    positives = [(a, m) for a, m in rows if positive(a, m)]
    if not positives:
        negatives = [(a, m) for a, m in rows if a.sha == request.binding.producer_sha
                     and _exact(m, request) and not m.positive]
        if negatives:
            evidence._negative(max(negatives, key=lambda row:(row[0].run_number, row[0].run_attempt))[1])
        raise evidence.CompatRefusal('compat-missing' if not rows else 'compat-stale', 'rollback-no-qualified-positive')
    selected, measured = max(positives, key=lambda row:(row[0].run_number, row[0].run_attempt))
    for other, observation in rows:
        if other is selected:
            continue
        window = other.execution_window
        if window is not None and window.fresh and window.completed_at < selected.execution_window.started_at:
            continue
        _need(window is not None and window.fresh, 'rollback-later-or-unknown-timing')
        if other.sha == request.binding.producer_sha and _exact(observation, request) and not observation.positive:
            evidence._negative(observation)
        # Preserve normal selection semantics only for another complete exact-H,
        # exact-binding positive. Foreign or differently bound work never fits.
        _need(positive(other, observation), 'rollback-later-or-overlapping-evidence')
    pair = selected.measurement
    return QualifiedRollback(request.binding, selected.run_id, selected.run_number, selected.run_attempt,
        selected.requested_at, selected.started_at, selected.completed_at,
        pair.artifact.artifact_id, pair.artifact.name, measured.result_sha256, measured.context_sha256,
        pair.result_subject.signer_uri, pair.result_subject.signer_sha, measured.catalog_sha256,
        measured.forward, measured.migrations, measured.servers, measured.reversible,
        request.observation_id, selected.execution_window)


def require_rollback_compat(request, history):
    return _fixed(_qualify, request, history)
