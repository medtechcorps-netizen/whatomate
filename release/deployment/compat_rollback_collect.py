"""Preparatory all-head Actions/source collection, not rollback activation.

Every head owns its immutable workflow bytes. Raw API envelopes and both actual
verified subjects are retained for pure replay. One caller-owned OperationBudget
covers Git, Actions, verification and teardown; this collector closes that phase.
The future caller must resolve authenticated source before/after this phase under
the same outer allowance. Neither a SHA nor these returned data types authorize
deployment, prove current main, or establish deleted-run coverage.
"""
from dataclasses import dataclass, replace
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re

import compat_adapter as adapter
import compat_collect as normal
import compat_envelope as envelope
import compat_evidence as evidence
import compat_rollback_history as history
import record_transport as transport
import ship_common as common
import source_git


_need = envelope._need
_GETS = (re.compile(re.escape(normal.API) +
    rf'/actions/workflows/{normal._ID}/runs\?branch=main&event=(?:push|workflow_dispatch)'
    rf'&per_page=100&page={normal._PAGE}\Z'), *normal._GETS[:1], *normal._GETS[2:])


def _hash(raw):
    return hashlib.sha256(raw).hexdigest()


def _now(clock):
    value = clock()
    _need(type(value) is datetime and value.tzinfo is timezone.utc and value.fold == 0,
          'rollback-collect-clock')
    return value


class _Capture:
    """Private transcript over the exact shared budget, without stream echo."""
    def __init__(self, budget, gh, git, scratch):
        self.budget, self.gh, self.git = budget, str(gh), str(git)
        self.scratch, self.identity = scratch, normal._directory(scratch)
        self.commands = self.captured_bytes = 0
        self.transcript = hashlib.sha256()

    def __call__(self, argv, **options):
        _need(type(argv) is list and argv and argv[0] in (self.gh, self.git),
              'rollback-collect-executable')
        _need(normal._directory(self.scratch) == self.identity, 'rollback-collect-directory-changed')
        result = self.budget.capture(argv, **options)
        _need(normal._directory(self.scratch) == self.identity, 'rollback-collect-directory-changed')
        self.commands += 1
        self.captured_bytes += len(result.stdout) + len(result.stderr)
        self.transcript.update(common.canonical_file_bytes({
            'command': self.commands, 'role': 'git' if argv[0] == self.git else 'gh',
            'returncode': result.returncode,
            'stdout_bytes': len(result.stdout), 'stderr_bytes': len(result.stderr),
            'stdout_sha256': _hash(result.stdout), 'stderr_sha256': _hash(result.stderr)}))
        return result


class _RollbackActionsGh(normal.ActionsGh):
    """Internally constructed fixed all-head variant of the existing client."""
    def _allows_get(self, path):
        return any(rule.fullmatch(path) for rule in _GETS)

    def _history_pair(self, meta, attempt, scratch):
        _need(self._scope is None, 'collect-verification-scope')
        self._downloads.add(meta.id)
        raw = self._run(['api', '--method', 'GET',
            normal.API + f'/actions/artifacts/{meta.id}/zip', '--allow-escape-sequences'],
            code='gh-failed:collect')
        _need(len(raw) == meta.size, 'rollback-collect-artifact-size')
        self._scope = attempt, scratch
        try:
            location = adapter.ArtifactLocation(meta.id, meta.name, attempt.run_id,
                                                attempt.run_attempt, attempt.sha, False)
            return adapter.verify_history_pair(self, raw, location, attempt, scratch)
        finally:
            self._scope = None


_project = envelope._project_fields


_canonical = envelope._canonical_projection


_run = envelope._project_run


_job = envelope._project_job


_check = envelope._project_check


_artifact = envelope._project_artifact


def _inventory(client, wall_clock):
    meta = client.get(normal.API + '/actions/workflows/compat.yml')
    _need(type(meta) is dict and meta.get('path') == evidence.WORKFLOW
          and meta.get('name') == 'Compatibility' and meta.get('state') == 'active',
          'collect-workflow')
    wid = envelope._integer(meta.get('id'))
    runs, numbers, total = {}, set(), 0
    for event in ('push', 'workflow_dispatch'):
        rows = normal._pages(client, normal.API +
            f'/actions/workflows/{wid}/runs?branch=main&event={event}',
            'workflow_runs', history.MAX_ATTEMPTS)
        observed = _now(wall_clock)
        for row in rows:
            _need(type(row.get('head_sha')) is str
                  and common.SHA1_RE.fullmatch(row['head_sha']) is not None,
                  'rollback-collect-source')
            source = envelope.WorkflowObservation(row['head_sha'], None)
            parsed = envelope._run_meta(row, source, wid, observed)
            latest = envelope._integer(parsed['attempt'], history.MAX_ATTEMPTS)
            _need(parsed['event'] == event and parsed['id'] not in runs
                  and parsed['number'] not in numbers, 'collect-run-duplicate')
            total += latest
            _need(total <= history.MAX_ATTEMPTS, 'collect-attempt-bound')
            numbers.add(parsed['number'])
            runs[parsed['id']] = _run(row, latest)
    return {'workflow': _project(meta, {'id', 'path', 'name', 'state'}),
            'runs': [runs[key] for key in sorted(runs)]}


def _sources(reader, producer_sha, runs):
    heads = {producer_sha, *(row['head_sha'] for row in runs)}
    _need(len(heads) <= history.MAX_HEADS, 'rollback-collect-head-bound')
    observations, size = [], 0
    for sha in sorted(heads):
        _need(reader.output(reader.repo, ['cat-file', '-t', sha], code='unused') == b'commit\n',
              'rollback-collect-source')
        raw = reader.read_blob(sha, evidence.WORKFLOW)
        source = envelope.WorkflowObservation(sha, raw)
        envelope.validate_workflow(source)
        if sha == producer_sha:
            _need(raw is not None and _hash(raw) == normal.WORKFLOW_SHA256,
                  'rollback-collect-workflow-source')
        size += 0 if raw is None else len(raw)
        _need(size <= history.MAX_RAW_BYTES, 'rollback-history-bound')
        observations.append(source)
    return tuple(observations), size


@dataclass(frozen=True, slots=True)
class _Snapshot:
    inventory: bytes
    workflows: tuple
    rows: tuple


def _snapshot(client, reader, producer_sha, wall_clock):
    inventory = _inventory(client, wall_clock)
    workflows, size = _sources(reader, producer_sha, inventory['runs'])
    by_sha = {row.sha: row for row in workflows}
    rows = []
    for latest in inventory['runs']:
        rid, last = latest['id'], latest['latest_run_attempt']
        artifacts = sorted((_artifact(item) for item in normal._pages(client,
            normal.API + f'/actions/runs/{rid}/artifacts?', 'artifacts', 3 * last)),
            key=lambda item: item['id'])
        for number in range(1, last + 1):
            run = _run(client.get(normal.API + f'/actions/runs/{rid}/attempts/{number}'), last)
            _need(all(run[key] == latest[key] for key in ('id', 'head_sha', 'workflow_id',
                'run_number', 'event', 'created_at')) and run['run_attempt'] == number,
                'collect-attempt')
            _need(number != last or run == latest, 'collect-latest-changed')
            jobs = sorted((_job(item) for item in normal._pages(client,
                normal.API + f'/actions/runs/{rid}/attempts/{number}/jobs?', 'jobs', 4)),
                key=lambda item: item['id'])
            suite = envelope._integer(run['check_suite_id'])
            checks = sorted((_check(item) for item in normal._pages(client,
                normal.API + f'/check-suites/{suite}/check-runs?filter=all',
                'check_runs', 4 * last)), key=lambda item: item['id'])
            value = {'version': 1, 'run': run, 'jobs': jobs, 'checks': checks, 'artifacts': artifacts}
            envelope._tree(value)
            raw = _canonical(value, min(envelope.MAX_RAW_BYTES, history.MAX_RAW_BYTES - size))
            _need(len(raw) <= envelope.MAX_RAW_BYTES and size + len(raw) <= history.MAX_RAW_BYTES,
                  'rollback-history-bound')
            size += len(raw)
            facts = envelope.EnvelopeFacts(raw)
            replayed = envelope.replay(by_sha[run['head_sha']], facts, observed_at=_now(wall_clock))
            rows.append(history.RollbackAttempt(facts, replayed.attempt.measurement))
    snapshot = _Snapshot(common.canonical_file_bytes(inventory), workflows, tuple(rows))
    # Validate cross-run identities, exact per-run inventory and every attempt
    # before any artifact can be treated as authenticated history.
    history.validate_history(_history(producer_sha, snapshot, snapshot.rows, _now(wall_clock),
        _hash(snapshot.inventory), 'complete-visible'), producer_sha=producer_sha)
    return snapshot


def _history(producer_sha, snapshot, rows, observed, observation, enumeration):
    return history.RollbackHistory(producer_sha, evidence.REPOSITORY, evidence.WORKFLOW,
        observation, observed, snapshot.workflows, tuple(rows), enumeration, 'unavailable')


def _commit(snapshot):
    if snapshot is None:
        return None
    return {'inventory': _hash(snapshot.inventory),
        'workflows': [{'sha': item.sha, 'raw': None if item.raw is None else _hash(item.raw)}
                      for item in snapshot.workflows],
        'envelopes': [_hash(item.envelope.raw) for item in snapshot.rows]}


def _pair_commit(pair):
    if type(pair) is evidence.MissingEvidence:
        return pair.value
    return {'result': _hash(pair.result), 'context': _hash(pair.context),
            'result_verification': pair.result_subject.verification_reference,
            'context_verification': pair.context_subject.verification_reference}


def _paths(repo, gh, git, config, scratch):
    normal._directory(repo)
    normal._directory(config, empty=True)
    normal._directory(scratch, empty=True)
    repo_path, config_path, scratch_path = repo.resolve(), config.resolve(), scratch.resolve()
    _need(config_path != scratch_path and not config_path.is_relative_to(scratch_path)
          and not scratch_path.is_relative_to(config_path)
          and not config_path.is_relative_to(repo_path) and not scratch_path.is_relative_to(repo_path),
          'rollback-collect-directory')
    for path in (gh, git):
        _need(isinstance(path, Path) and path.is_absolute() and path.is_file()
              and not path.is_symlink() and not getattr(path, 'is_junction', lambda: False)(),
              'rollback-collect-executable')
    _need(gh.resolve() != git.resolve(), 'rollback-collect-executable')


def _collect(producer_sha, repo_dir, *, gh_binary, git_binary, token, config_dir,
             scratch, budget, wall_clock, base_env):
    _need(type(budget) is transport.OperationBudget, 'rollback-collect-budget')
    try:
        budget.check()
        _need(type(producer_sha) is str and common.SHA1_RE.fullmatch(producer_sha) is not None,
              'rollback-collect-source')
        _need(type(token) is str and 0 < len(token) <= 8192
              and not any(char in token for char in '\r\n\0'), 'rollback-collect-token')
        _paths(repo_dir, gh_binary, git_binary, config_dir, scratch)
        start = _now(wall_clock)
        capture = _Capture(budget, gh_binary.resolve(), git_binary.resolve(), scratch)
        client = _RollbackActionsGh(str(gh_binary.resolve()), token, config_dir,
            seconds=min(600, budget.remaining()), base_env=base_env,
            capture=capture, clock=budget.clock)
        client._used = True
        reader = source_git.ImmutableGit(repo_dir, git_binary,
            limits=source_git.Limits(seconds=min(600, budget.remaining())),
            capture=capture, clock=budget.clock, base_env=base_env)
        with reader.operation():
            first = _snapshot(client, reader, producer_sha, wall_clock)
            by_sha = {item.sha: item for item in first.workflows}
            rows, issues = [], set()
            size = sum(len(item.raw) for item in first.workflows if item.raw is not None)
            size += sum(len(item.envelope.raw) for item in first.rows)
            for row in first.rows:
                value = common.loads_strict(row.envelope.raw)
                replayed = envelope.replay(by_sha[value['run']['head_sha']], row.envelope,
                                           observed_at=_now(wall_clock))
                attempt = replayed.attempt
                name = f'compat-result-{attempt.sha}-{attempt.run_attempt}'
                meta = next((item for item in replayed.artifacts if item.name == name), None)
                measurement = row.measurement
                if meta is not None:
                    if meta.expired:
                        measurement = evidence.MissingEvidence.EXPIRED
                    else:
                        try:
                            _need(attempt.started_at is not None and attempt.started_at <= meta.created
                                  and (attempt.completed_at is None or meta.updated <= attempt.completed_at),
                                  'collect-artifact-period')
                            measurement = client._history_pair(meta, attempt, scratch)
                        except evidence.CompatRefusal:
                            # A spent shared allowance is never softened into
                            # an older recoverable artifact-verification issue.
                            budget.check()
                            measurement = evidence.MissingEvidence.UNKNOWN
                            issues.add('artifact-unverified')
                if type(measurement) is evidence.AuthenticatedPair:
                    size += len(measurement.result) + len(measurement.context)
                    _need(size <= history.MAX_RAW_BYTES, 'rollback-history-bound')
                rows.append(replace(row, measurement=measurement))
            second, enumeration = None, 'complete-visible'
            try:
                second = _snapshot(client, reader, producer_sha, wall_clock)
                if first != second:
                    enumeration = 'incomplete'
                    issues.add('inventory-changed')
            except evidence.CompatRefusal:
                budget.check()
                _need(not reader.failed, 'rollback-collect-source')
                enumeration = 'incomplete'
                issues.add('inventory-unavailable')
        # Includes the source operation's final repository checks in the hash
        # and allowance; no deferred IO remains when the result is returned.
        budget.check()
        _need(normal._directory(scratch) == capture.identity and not any(scratch.iterdir()),
              'rollback-collect-directory-changed')
        end = _now(wall_clock)
        _need(start <= end, 'rollback-collect-clock')
        transcript = capture.transcript.hexdigest()
        observation = _hash(common.canonical_file_bytes({
            'domain': 'rollback-collection-v1', 'producer_sha': producer_sha,
            'begin': _commit(first), 'end': _commit(second),
            'measurements': [_pair_commit(row.measurement) for row in rows],
            'started': start.isoformat(), 'observed': end.isoformat(),
            'commands': capture.commands, 'bytes': capture.captured_bytes,
            'transcript': transcript}))
        result = history.CollectedRollbackHistory(_history(producer_sha, first, rows, end,
            observation, enumeration), tuple(sorted(issues)), capture.commands,
            capture.captured_bytes, transcript)
        history.validate_collected(result)
        budget.check()
        return result
    except evidence.CompatRefusal:
        budget.failed = True
        raise
    except (common.ReleaseError, OSError, ValueError, TypeError, KeyError, AttributeError,
            RecursionError, OverflowError, UnicodeError):
        budget.failed = True
        raise evidence.CompatRefusal('compat-stale', 'rollback-collect-invalid') from None
    finally:
        budget.close()


def collect_rollback_history(producer_sha: str, repo_dir: Path, *, gh_binary: Path,
        git_binary: Path, token: str, config_dir: Path, scratch: Path,
        budget: transport.OperationBudget) -> history.CollectedRollbackHistory:
    """Read all visible heads using one already-open, caller-owned allowance.

    config_dir/scratch must be fresh private directories outside the repository.
    The SHA is only a restrictive input; current-main and signed release/source
    authority must come from the surrounding concrete resolver before effects.
    No arbitrary client, callback, raw workflow or supplied proof is accepted.
    """
    return _collect(producer_sha, repo_dir, gh_binary=gh_binary, git_binary=git_binary,
        token=token, config_dir=config_dir, scratch=scratch, budget=budget,
        wall_clock=lambda: datetime.now(timezone.utc), base_env=os.environ)
