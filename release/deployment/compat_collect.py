"""Bounded visible Actions collection, not ship activation or deletion coverage.

Only the reviewed paired producer is supported. Rerun request time remains
unknown. Complete current-attempt job intervals establish execution windows;
run_started_at remains a processing boundary, never a human request time.
"""
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
import hashlib
import math
from pathlib import Path
import re
import sys
import time

import compat_adapter as adapter
import compat_evidence as evidence
import compat_envelope as envelope
import release_record as records
import ship_common as common

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "compat"))
import runtime

WORKFLOW_SHA256 = "73c655c829fa36a109dfee621f1efa4529cf89ffa112599a8ff1bb8391b7eff9"
REPOSITORY_ID = 1297344686
API = "/repos/" + evidence.REPOSITORY
WEB = "https://github.com/" + evidence.REPOSITORY
API_URL = "https://api.github.com" + API
HEADERS = ("-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28")
MAX_COMMANDS = 6000
MAX_BYTES = 128 * 1024 * 1024
# bounded_command reaps its child for <=2s and joins two readers for <=0.1s
# each. That capture tail stays inside this operation's remaining deadline.
CLEANUP_RESERVE = 2.25
STDERR_LIMIT = 65536
PAGE = 100
_ID = r"[1-9][0-9]{0,15}"
_PAGE = r"[1-9][0-9]?"
_SHA = r"[0-9a-f]{40}"
_ACTIVE = {"queued", "in_progress", "waiting", "pending", "requested"}
_CONCLUSIONS = {"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale"}
_TIME = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?Z\Z")
_GETS = tuple(re.compile(re.escape(API) + p + r"\Z") for p in (
    r"/actions/workflows/compat\.yml",
    rf"/actions/workflows/{_ID}/runs\?head_sha={_SHA}&branch=main&event=(?:push|workflow_dispatch)&per_page=100&page={_PAGE}",
    rf"/actions/runs/{_ID}/attempts/{_ID}",
    rf"/actions/runs/{_ID}/attempts/{_ID}/jobs\?per_page=100&page={_PAGE}",
    rf"/check-suites/{_ID}/check-runs\?filter=all&per_page=100&page={_PAGE}",
    rf"/actions/runs/{_ID}/artifacts\?per_page=100&page={_PAGE}",
))


_need = envelope._need


def _hash(raw):
    return hashlib.sha256(raw).hexdigest()


_integer = envelope._integer


_date = envelope._date


_state = envelope._state


def _directory(path, empty=False):
    _need(isinstance(path, Path) and path.is_absolute(), "collect-directory")
    for part in (path, *path.parents):
        _need(part.is_dir() and not part.is_symlink() and not getattr(part, "is_junction", lambda: False)(), "collect-directory")
    _need(not empty or not any(path.iterdir()), "collect-directory")
    info = path.stat()
    return info.st_dev, info.st_ino


@dataclass(frozen=True, slots=True)
class CollectionRequest:
    """Source data, not an authenticated-source capability.

    The upstream immutable-source adapter must bind workflow_bytes to sha.
    Matching the reviewed bytes here does not prove that commit's Git tree.
    """
    sha: str
    workflow_bytes: bytes = field(repr=False)


@dataclass(frozen=True, slots=True)
class CollectedHistory:
    """API/signature facts; candidate workflow Git binding remains upstream."""
    history: evidence.VisibleHistory = field(repr=False)
    issues: tuple[str, ...]
    command_count: int
    captured_bytes: int
    transcript_sha256: str


class ActionsGh(records.Gh):
    """Fixed read endpoints and the inherited exact two-subject verifier only.

    Each instance is one collection budget and may collect only once. Injected
    capture/clock are test seams, never accepted verified JSON. No subprocess
    or network call occurs on import; construction checks the actual CLI version.
    """
    def __init__(self, binary, token, config_dir, *, seconds=300, base_env=None,
                 capture=runtime.bounded_command, clock=time.monotonic):
        _need(type(seconds) in (int, float) and math.isfinite(seconds) and 0 < seconds <= 600, "collect-budget")
        self._clock, self._capture = clock, capture
        self._last = clock()
        _need(type(self._last) in (int, float) and math.isfinite(self._last), "collect-clock")
        self._deadline = self._last + seconds
        self._commands, self._bytes, self._transcript = 0, 0, hashlib.sha256()
        self._failure_category = None
        self._scope = self._verification_args = None
        self._downloads, self._used_downloads = set(), set()
        self._used = False
        self._config = config_dir
        try:
            self._config_identity = _directory(config_dir, empty=True)
            super().__init__(binary, token, evidence.REPOSITORY, expected_version="2.98.0",
                             config_dir=str(config_dir), base_env=base_env)
        except (OSError, common.ReleaseError):
            raise evidence.CompatRefusal("compat-stale", "collect-client") from None

    def _note_failure(self, category):
        # Local diagnostic only; no reason, evidence or policy changes.
        if self._failure_category is None:
            self._failure_category = category

    def _remaining(self, maximum):
        now = self._clock()
        valid = type(now) in (int, float) and math.isfinite(now) and self._last <= now < self._deadline
        if not valid:
            self._note_failure("deadline")
        _need(valid, "collect-deadline")
        self._last = now
        return min(maximum, self._deadline - now)

    def _allows_get(self, path):
        """Fixed normal grammar; purpose-specific collectors supply fixed alternatives."""
        return any(p.fullmatch(path) for p in _GETS)

    def _run(self, args, *, code):
        args = list(args)
        limit, duration = 4 * 1024 * 1024, 30
        allowed = args == ["--version"]
        if len(args) == 8 and args[:3] == ["api", "--method", "GET"] and tuple(args[4:]) == HEADERS:
            allowed = self._allows_get(args[3])
        if args == self._verification_args and self._scope is not None:
            allowed, limit, duration = True, adapter.MAX_VERIFICATION, 60
        if len(args) == 5 and args[:3] == ["api", "--method", "GET"] and args[4] == "--allow-escape-sequences":
            match = re.fullmatch(re.escape(API) + rf"/actions/artifacts/({_ID})/zip", args[3])
            if match:
                identity = int(match[1])
                allowed = identity in self._downloads and identity not in self._used_downloads
                if allowed:
                    self._used_downloads.add(identity)
                    limit = adapter.MAX_ARCHIVE
        _need(allowed, "collect-command")
        remaining_bytes = MAX_BYTES - self._bytes
        _need(self._commands < MAX_COMMANDS and remaining_bytes > STDERR_LIMIT, "collect-bound")
        try:
            _need(_directory(self._config) == self._config_identity, "collect-directory-changed")
            timeout = self._remaining(duration + CLEANUP_RESERVE) - CLEANUP_RESERVE
            if timeout <= 0:
                self._note_failure("deadline")
            _need(timeout > 0, "collect-deadline")
            stdout_limit = min(limit, remaining_bytes - STDERR_LIMIT)
            self._commands += 1
            env = dict(self._env)
            env["GH_HOST"] = "github.com"
            try:
                result = self._capture([self.gh_bin, *args], env=env, timeout=timeout,
                    stdout_limit=stdout_limit, stderr_limit=STDERR_LIMIT)
            except Exception:
                self._note_failure("capture")
                raise
            self._remaining(duration)
            _need(_directory(self._config) == self._config_identity, "collect-directory-changed")
            valid = (type(result.returncode) is int and type(result.stdout) is bytes and type(result.stderr) is bytes
                     and len(result.stdout) <= stdout_limit and len(result.stderr) <= STDERR_LIMIT)
            if not valid:
                self._note_failure("capture")
            _need(valid, "collect-capture")
            self._bytes += len(result.stdout) + len(result.stderr)
            _need(self._bytes <= MAX_BYTES, "collect-bound")
            self._transcript.update(common.canonical_file_bytes({"command": self._commands,
                "returncode": result.returncode, "stdout": _hash(result.stdout), "stderr": _hash(result.stderr)}))
            if result.returncode != 0:
                self._note_failure("command-nonzero")
            _need(result.returncode == 0, "collect-transport")
            return result.stdout
        except (OSError, ValueError, runtime.proof.InfrastructureFailure, runtime.proof.EvidenceError):
            raise evidence.CompatRefusal("compat-stale", "collect-transport") from None

    def get(self, path):
        try:
            return self._json(["api", "--method", "GET", path, *HEADERS], code="gh-failed:collect")
        except (common.ReleaseError, ValueError, TypeError, RecursionError):
            raise evidence.CompatRefusal("compat-stale", "collect-json") from None

    def _verify(self, subject, flags, identity, *, code):
        _need(self._scope is not None and self._verification_args is None, "collect-verification-scope")
        attempt, scratch = self._scope
        path = Path(subject)
        expected = self.signer_flags(evidence.WORKFLOW, attempt.sha, attempt.sha, evidence.PREDICATE)
        expected += ["--cert-oidc-issuer", evidence.ISSUER]
        _need(path.is_absolute() and path.name in adapter.FILES and path.parent.parent == scratch
              and path.parent.name.startswith("compat-verify-") and list(flags) == expected
              and identity == self.signer_identity(evidence.WORKFLOW, attempt.sha, attempt.sha), "collect-verification-scope")
        self._verification_args = ["attestation", "verify", subject, *flags, "--format", "json"]
        try:
            return super()._verify(subject, flags, identity, code=code)
        except common.ReleaseError:
            self._note_failure("signature")
            raise
        finally:
            self._verification_args = None

    def _pair(self, meta, attempt, scratch):
        _need(self._scope is None, "collect-verification-scope")
        self._downloads.add(meta["id"])
        raw = self._run(["api", "--method", "GET", API + f"/actions/artifacts/{meta['id']}/zip",
                         "--allow-escape-sequences"], code="gh-failed:collect")
        self._scope = attempt, scratch
        try:
            location = adapter.ArtifactLocation(meta["id"], meta["name"], attempt.run_id,
                                                attempt.run_attempt, attempt.sha, False)
            return adapter.verify_pair(self, raw, location, attempt, scratch)
        finally:
            self._scope = None


def _pages(client, base, key, maximum):
    output, ids, total = [], set(), None
    # A full final page is followed by one bounded empty sentinel.
    for page in range(1, maximum // PAGE + 2):
        value = client.get(base + ("" if base.endswith("?") else "&") + f"per_page=100&page={page}")
        _need(type(value) is dict and type(value.get(key)) is list, "collect-page")
        count = _integer(value.get("total_count"), maximum, 0)
        _need(total is None or total == count, "collect-page-changed")
        total = count
        rows = value[key]
        _need(len(rows) <= PAGE, "collect-page")
        for row in rows:
            _need(type(row) is dict, "collect-page")
            identity = _integer(row.get("id"))
            _need(identity not in ids, "collect-duplicate")
            ids.add(identity); output.append(row)
        _need(len(output) <= total, "collect-page-count")
        if len(rows) < PAGE:
            _need(len(output) == total, "collect-page-count")
            return output
    raise evidence.CompatRefusal("compat-stale", "collect-page-bound")


def _workflow(request):
    _need(type(request) is CollectionRequest and type(request.sha) is str
          and re.fullmatch(_SHA, request.sha) is not None and type(request.workflow_bytes) is bytes
          and _hash(request.workflow_bytes) == WORKFLOW_SHA256, "collect-workflow-source")
    # Parsing is restricted to these exact reviewed bytes, not generic YAML.
    steps, job = {}, None
    for line in request.workflow_bytes.decode("ascii").splitlines():
        if re.fullmatch(r"  (release-preview|compat-run|compat|attest):", line):
            job = line.strip()[:-1]
            if job == "attest": job = "compat-attest"
            steps[job] = []
        elif job and line.startswith("      - name: "):
            steps[job].append(line[len("      - name: "):])
    _need(set(steps) == set(evidence.JOBS), "collect-workflow-source")
    return steps


_repository = envelope._repository


_run_meta = envelope._run_meta


_steps = envelope._steps


_job = envelope._job


_artifact = envelope._artifact


def _snapshot(client, request, steps, observed, *, retain=False, retention_limit=None):
    if retain:
        if retention_limit is None:
            retention_limit = MAX_OUTAGE_RAW_BYTES
        _need(type(retention_limit) is int and 0 < retention_limit <= MAX_OUTAGE_RAW_BYTES,
              'outage-history-bound')
    workflow = client.get(API + "/actions/workflows/compat.yml")
    _need(type(workflow) is dict and workflow.get("path") == evidence.WORKFLOW
          and workflow.get("name") == "Compatibility" and workflow.get("state") == "active", "collect-workflow")
    wid = _integer(workflow.get("id"))
    runs, total, numbers = {}, 0, set()
    if retain:
        raw_runs, raw_attempts, retained_bytes = {}, [], 0
    for event in ("push", "workflow_dispatch"):
        raw = _pages(client, API + f"/actions/workflows/{wid}/runs?head_sha={request.sha}&branch=main&event={event}", "workflow_runs", evidence.MAX_ATTEMPTS)
        for row in raw:
            run = _run_meta(row, request, wid, observed)
            _need(run["event"] == event and run["id"] not in runs and run["number"] not in numbers, "collect-run-duplicate")
            runs[run["id"]] = run; numbers.add(run["number"]); total += run["attempt"]
            if retain:
                raw_runs[run["id"]] = envelope._project_run(row, run["attempt"])
            _need(total <= evidence.MAX_ATTEMPTS, "collect-attempt-bound")
    attempts, projections, artifacts, all_jobs, all_checks, listed_checks = [], [], {}, set(), set(), set()
    for rid, latest in sorted(runs.items()):
        raw_artifacts = _pages(client, API + f"/actions/runs/{rid}/artifacts?", "artifacts", 3*latest["attempt"])
        metas = [_artifact(a, latest, request.sha, observed) for a in raw_artifacts]
        _need(len({a["name"] for a in metas}) == len(metas), "collect-artifact-duplicate")
        artifacts[rid] = sorted(metas, key=lambda a:a["id"])
        for number in range(1, latest["attempt"]+1):
            row = client.get(API + f"/actions/runs/{rid}/attempts/{number}")
            run = _run_meta(row, request, wid, observed)
            if retain:
                raw_run = envelope._project_run(row, latest["attempt"])
            _need(run["id"] == rid and run["attempt"] == number and run["number"] == latest["number"]
                  and run["event"] == latest["event"] and run["created"] == latest["created"], "collect-attempt")
            if number == latest["attempt"]:
                _need(run == latest, "collect-latest-changed")
            raw_jobs = _pages(client, API + f"/actions/runs/{rid}/attempts/{number}/jobs?", "jobs", 4)
            raw_checks = _pages(client, API + f"/check-suites/{run['suite']}/check-runs?filter=all", "check_runs", 4*latest["attempt"])
            checks = {c["id"]:c for c in raw_checks}
            listed_checks.update(checks)
            jobs, details, names, known, intervals = [], [], set(), True, []
            for job in raw_jobs:
                typed, eligible, terminal, executed, detail = _job(job, checks, run, request.sha, steps, observed)
                _need(typed.name not in names and typed.job_id not in all_jobs and typed.check_run_id not in all_checks, "collect-job-duplicate")
                names.add(typed.name); all_jobs.add(typed.job_id); all_checks.add(typed.check_run_id)
                known &= terminal
                if eligible or typed.conclusion == "skipped": jobs.append(typed)
                if executed is not None: intervals.append(executed)
                details.append((typed, eligible, terminal, detail))
            known &= names == set(evidence.JOBS) and run["status"] == "completed" and bool(intervals)
            completion = max(end for _, end in intervals) if known else None
            window = evidence.ExecutionWindow(min(start for start, _ in intervals), completion, True) if known and len(jobs) == 4 else None
            attempt = evidence.Attempt(evidence.REPOSITORY, evidence.WORKFLOW, request.sha, request.sha,
                run["event"], evidence.REF, rid, run["number"], number, latest["attempt"], run["status"],
                run["conclusion"], run["created"] if number == 1 else None, run["started"], completion,
                tuple(sorted(jobs,key=lambda j:j.name)), evidence.MissingEvidence.ACTIVE if run["status"] != "completed" else evidence.MissingEvidence.MISSING,
                execution_window=window)
            attempts.append(attempt)
            projections.append((run, tuple(sorted(details,key=lambda d:d[0].name)), window))
            if retain:
                raw = envelope._canonical_projection({'version': 1, 'run': raw_run,
                    'jobs': sorted((envelope._project_job(j) for j in raw_jobs), key=lambda j:j['id']),
                    'checks': sorted((envelope._project_check(j) for j in raw_checks), key=lambda j:j['id']),
                    'artifacts': sorted((envelope._project_artifact(j) for j in raw_artifacts), key=lambda j:j['id'])},
                    min(envelope.MAX_RAW_BYTES, retention_limit - retained_bytes))
                retained_bytes += len(raw)
                _need(retained_bytes <= retention_limit, 'outage-history-bound')
                raw_attempts.append(envelope.EnvelopeFacts(raw))
    _need(all_checks == listed_checks, "collect-check-inventory")
    if retain:
        raw = envelope._canonical_projection([raw_runs[rid] for rid in sorted(raw_runs)],
            min(envelope.MAX_RAW_BYTES, retention_limit - retained_bytes))
        _need(retained_bytes + len(raw) <= retention_limit, 'outage-history-bound')
        return attempts, (wid, runs, projections, artifacts), artifacts, OutageHistorySnapshot(
            wid, raw, tuple(raw_attempts), observed)
    return attempts, (wid, runs, projections, artifacts), artifacts


def _safe_projection(value):
    if isinstance(value, datetime): return value.isoformat()
    if type(value) is evidence.Job: return tuple(getattr(value,k) for k in value.__slots__)
    if type(value) is evidence.ExecutionWindow: return [_safe_projection(value.started_at), _safe_projection(value.completed_at), value.fresh]
    if type(value) in (tuple,list): return [_safe_projection(v) for v in value]
    if type(value) is dict: return {str(k):_safe_projection(v) for k,v in value.items()}
    return value


def _collect_history(request, client, scratch, *, wall_clock, retain=False):
    """Collect once; no policy grant and no claim against deleted whole runs."""
    steps = _workflow(request)
    _need(type(client) is ActionsGh and not client._used, "collect-client-reuse")
    client._used = True
    try:
        _directory(scratch)
        start = wall_clock()
        _need(type(start) is datetime and start.tzinfo == timezone.utc, "collect-clock")
        if retain:
            attempts, first, artifacts, before = _snapshot(client, request, steps, wall_clock(), retain=True)
        else:
            attempts, first, artifacts = _snapshot(client, request, steps, wall_clock())
        issues = []
        for i, attempt in enumerate(attempts):
            name = f"compat-result-{request.sha}-{attempt.run_attempt}"
            meta = next((m for m in artifacts[attempt.run_id] if m["name"] == name), None)
            measurement = attempt.measurement
            if meta is not None:
                if meta["expired"]:
                    measurement = evidence.MissingEvidence.EXPIRED
                else:
                    try:
                        _need(attempt.started_at is not None and attempt.started_at <= meta["created"]
                              and (attempt.completed_at is None or meta["updated"] <= attempt.completed_at), "collect-artifact-period")
                        measurement = client._pair(meta, attempt, scratch)
                    except evidence.CompatRefusal:
                        measurement = evidence.MissingEvidence.UNKNOWN
                        issues.append("artifact-unverified")
            attempts[i] = replace(attempt, measurement=measurement)
        enumeration, second = "complete-visible", None
        if retain:
            after = None
        try:
            if retain:
                used = len(before.runs) + sum(len(row.raw) for row in before.attempts)
                used += sum(len(row.measurement.result) + len(row.measurement.context) for row in attempts
                            if type(row.measurement) is evidence.AuthenticatedPair)
                _, second, _, after = _snapshot(client, request, steps, wall_clock(), retain=True,
                    retention_limit=MAX_OUTAGE_RAW_BYTES - used)
            else:
                _, second, _ = _snapshot(client, request, steps, wall_clock())
            if second != first:
                enumeration = "incomplete"; issues.append("inventory-changed")
        except evidence.CompatRefusal:
            enumeration = "incomplete"; issues.append("inventory-unavailable")
        end = wall_clock()
        _need(type(end) is datetime and end.tzinfo == timezone.utc and start <= end, "collect-clock")
        try:
            client._remaining(1)
        except evidence.CompatRefusal:
            enumeration = "incomplete"; issues.append("collection-deadline")
        transcript = client._transcript.hexdigest()
        observation = _hash(common.canonical_file_bytes(_safe_projection(
            {"begin":first,"end":second,"transcript":transcript,"started":start,"observed":end})))
        history = evidence.VisibleHistory(request.sha, evidence.REPOSITORY, evidence.WORKFLOW,
            observation, end, tuple(attempts), enumeration)
        evidence._history(history, request.sha)
        normal = CollectedHistory(history, tuple(sorted(set(issues))), client._commands, client._bytes, transcript)
        if retain:
            _need(after is not None, 'outage-history-incomplete')
            return validate_outage_history(CollectedOutageHistory(normal, start, before, after),
                                           workflow=request.workflow_bytes, producer_sha=request.sha)
        return normal
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, OSError, ValueError, TypeError, KeyError, AttributeError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "collect-invalid") from None


# Raw retention is a sibling of normal collection, not a new coverage verdict.
MAX_OUTAGE_RAW_BYTES = 12 * 1024 * 1024


@dataclass(frozen=True, slots=True)
class OutageHistorySnapshot:
    workflow_id: int
    runs: bytes = field(repr=False)
    attempts: tuple[envelope.EnvelopeFacts, ...]
    observed_at: datetime


@dataclass(frozen=True, slots=True)
class CollectedOutageHistory:
    normal: CollectedHistory
    started_at: datetime
    before: OutageHistorySnapshot
    after: OutageHistorySnapshot


def collect_history(request, client, scratch, *, wall_clock=lambda:datetime.now(timezone.utc)):
    """Original normal collector; raw outage custody is never fabricated here."""
    return _collect_history(request, client, scratch, wall_clock=wall_clock)


def collect_outage_history(request, client, scratch, *, wall_clock=lambda:datetime.now(timezone.utc)):
    """Retain the same two API snapshots, without another client or query pass."""
    return _collect_history(request, client, scratch, wall_clock=wall_clock, retain=True)


def _outage_utc(value):
    _need(type(value) is datetime and value.tzinfo is timezone.utc and value.fold == 0,
          'outage-history-clock')


def _outage_bound(value):
    _need(type(value) is CollectedOutageHistory and type(value.normal) is CollectedHistory
          and type(value.normal.history) is evidence.VisibleHistory, 'outage-history-shape')
    total = 0
    for snapshot in (value.before, value.after):
        _need(type(snapshot) is OutageHistorySnapshot and type(snapshot.runs) is bytes
              and 0 < len(snapshot.runs) <= envelope.MAX_RAW_BYTES
              and type(snapshot.attempts) is tuple and len(snapshot.attempts) <= evidence.MAX_ATTEMPTS,
              'outage-history-bound')
        total += len(snapshot.runs)
        for item in snapshot.attempts:
            _need(type(item) is envelope.EnvelopeFacts and type(item.raw) is bytes
                  and 0 < len(item.raw) <= envelope.MAX_RAW_BYTES, 'outage-history-bound')
            total += len(item.raw)
    _need(type(value.normal.history.attempts) is tuple
          and len(value.normal.history.attempts) <= evidence.MAX_ATTEMPTS, 'outage-history-bound')
    for attempt in value.normal.history.attempts:
        _need(type(attempt) is evidence.Attempt, 'outage-history-shape')
        if type(attempt.measurement) is evidence.AuthenticatedPair:
            pair = attempt.measurement
            _need(type(pair.result) is bytes and 0 < len(pair.result) <= 16384
                  and type(pair.context) is bytes and 0 < len(pair.context) <= 4096,
                  'outage-history-bound')
            total += len(pair.result) + len(pair.context)
    _need(total <= MAX_OUTAGE_RAW_BYTES, 'outage-history-bound')


def _outage_snapshot(snapshot, workflow):
    """Reconstruct the exact old normal projection from complete raw facts."""
    _outage_utc(snapshot.observed_at)
    wid = _integer(snapshot.workflow_id)
    raw_runs = common.loads_strict(snapshot.runs)
    envelope._tree(raw_runs)
    _need(common.canonical_file_bytes(raw_runs) == snapshot.runs, 'outage-history-canonical')
    envelope._ordered(raw_runs, evidence.MAX_ATTEMPTS)
    runs, numbers, total, listings = {}, set(), 0, {}
    for raw in raw_runs:
        envelope._keys(raw, envelope._RUN_KEYS)
        for name in ('repository', 'head_repository'):
            envelope._keys(raw[name], {'id', 'full_name'})
        run = _run_meta(raw, workflow, wid, snapshot.observed_at)
        _need(type(raw['latest_run_attempt']) is int and raw['latest_run_attempt'] == run['attempt']
              and run['number'] not in numbers, 'outage-history-runs')
        numbers.add(run['number']); total += run['attempt']
        runs[run['id']], listings[run['id']] = run, raw
    _need(total <= evidence.MAX_ATTEMPTS and total == len(snapshot.attempts), 'outage-history-attempts')
    expected_keys = [(rid, number) for rid, row in runs.items() for number in range(1, row['attempt'] + 1)]
    steps = envelope._reviewed_steps(workflow)
    _need(steps is not None, 'outage-history-workflow')
    projections, replayed, artifacts = [], [], {}
    jobs, referenced, listed, suites, artifact_ids = set(), set(), {}, {}, {}
    artifact_raw = {}
    for facts, key in zip(snapshot.attempts, expected_keys, strict=True):
        result = envelope.replay(workflow, facts, observed_at=snapshot.observed_at)
        attempt = result.attempt
        _need((attempt.run_id, attempt.run_attempt) == key and result.workflow_id == wid,
              'outage-history-attempts')
        value = common.loads_strict(facts.raw)
        run = _run_meta(value['run'], workflow, wid, snapshot.observed_at)
        latest = runs[attempt.run_id]
        _need(attempt.latest_run_attempt == latest['attempt']
              and run['number'] == latest['number'] and run['event'] == latest['event']
              and run['created'] == latest['created'], 'outage-history-run')
        if attempt.run_attempt == latest['attempt']:
            _need(value['run'] == listings[attempt.run_id], 'outage-history-latest')
        for jid in result.job_ids:
            _need(jid not in jobs, 'outage-history-job-duplicate'); jobs.add(jid)
        for cid in result.check_ids:
            _need(cid not in referenced, 'outage-history-check-duplicate'); referenced.add(cid)
        suite = attempt.run_id, result.listed_checks
        _need(result.suite not in suites or suites[result.suite] == suite, 'outage-history-suite')
        suites[result.suite] = suite
        for cid, raw in result.listed_checks:
            check = attempt.run_id, result.suite, raw
            _need(cid not in listed or listed[cid] == check, 'outage-history-checks')
            listed[cid] = check
        _need(attempt.run_id not in artifact_raw or artifact_raw[attempt.run_id] == result.artifacts_raw,
              'outage-history-artifacts')
        artifact_raw[attempt.run_id] = result.artifacts_raw
        for meta in result.artifacts:
            _need(meta.id not in artifact_ids or artifact_ids[meta.id] == (attempt.run_id, meta),
                  'outage-history-artifact-duplicate')
            artifact_ids[meta.id] = attempt.run_id, meta
        artifacts[attempt.run_id] = [dict(id=a.id, name=a.name, size=a.size, expired=a.expired,
            created=a.created, updated=a.updated, expires=a.expires) for a in result.artifacts]
        checks = {row['id']:row for row in value['checks']}
        details = []
        for row in value['jobs']:
            typed, eligible, terminal, _, detail = _job(row, checks, run, workflow.sha, steps, snapshot.observed_at)
            details.append((typed, eligible, terminal, detail))
        projections.append((run, tuple(sorted(details, key=lambda d:d[0].name)), attempt.execution_window))
        replayed.append(result)
    _need(referenced == set(listed), 'outage-history-check-inventory')
    return tuple(replayed), (wid, runs, projections, artifacts)


def _outage_pair(attempt, replayed):
    pair = attempt.measurement
    meta = next((row for row in replayed.artifacts
                 if row.name == f'compat-result-{attempt.sha}-{attempt.run_attempt}'), None)
    if type(pair) is evidence.AuthenticatedPair:
        _need(meta is not None and not meta.expired and type(pair.artifact) is evidence.Artifact
              and type(pair.artifact.artifact_id) is int and pair.artifact.artifact_id == meta.id
              and attempt.started_at is not None and attempt.started_at <= meta.created
              and (attempt.completed_at is None or meta.updated <= attempt.completed_at), 'outage-history-pair')
        evidence.parse_authenticated_pair(pair, attempt)
    elif meta is None:
        _need(pair is replayed.attempt.measurement, 'outage-history-missing')
    elif meta.expired:
        _need(pair is evidence.MissingEvidence.EXPIRED, 'outage-history-expired')
    else:
        _need(pair is evidence.MissingEvidence.UNKNOWN, 'outage-history-unverified')


def _outage_history(value, workflow, producer_sha):
    _outage_bound(value)
    observation = envelope.WorkflowObservation(producer_sha, workflow)
    envelope.validate_workflow(observation)
    _need(envelope._reviewed_steps(observation) is not None, 'outage-history-workflow')
    normal, history = value.normal, value.normal.history
    for moment in (value.started_at, value.before.observed_at, value.after.observed_at, history.observed_at):
        _outage_utc(moment)
    _need(value.started_at <= value.before.observed_at <= value.after.observed_at <= history.observed_at,
          'outage-history-clock')
    _need(type(normal.issues) is tuple and normal.issues in ((), ('artifact-unverified',))
          and type(normal.command_count) is int and 0 < normal.command_count <= MAX_COMMANDS
          and type(normal.captured_bytes) is int and 0 <= normal.captured_bytes <= MAX_BYTES
          and type(normal.transcript_sha256) is str and re.fullmatch(r'[0-9a-f]{64}', normal.transcript_sha256),
          'outage-history-metadata')
    evidence._history(history, producer_sha)
    _need(history.enumeration == 'complete-visible'
          and value.before.workflow_id == value.after.workflow_id
          and value.before.runs == value.after.runs
          and value.before.attempts == value.after.attempts, 'outage-history-incomplete')
    before, first = _outage_snapshot(value.before, observation)
    after, second = _outage_snapshot(value.after, observation)
    _need(first == second and len(history.attempts) == len(before), 'outage-history-incomplete')
    unknown = False
    for actual, derived, final in zip(history.attempts, before, after, strict=True):
        for moment in (actual.requested_at, actual.started_at, actual.completed_at):
            if moment is not None:
                _outage_utc(moment)
        if actual.execution_window is not None:
            _outage_utc(actual.execution_window.started_at); _outage_utc(actual.execution_window.completed_at)
        _need(replace(actual, measurement=derived.attempt.measurement) == derived.attempt
              and derived.attempt == final.attempt, 'outage-history-derived')
        _outage_pair(actual, derived)
        unknown |= actual.measurement is evidence.MissingEvidence.UNKNOWN
    _need(unknown == ('artifact-unverified' in normal.issues), 'outage-history-metadata')
    observation_id = _hash(common.canonical_file_bytes(_safe_projection({'begin':first, 'end':second,
        'transcript':normal.transcript_sha256, 'started':value.started_at, 'observed':history.observed_at})))
    _need(history.observation_id == observation_id, 'outage-history-observation')
    return value


def _outage_safe(value, workflow, producer_sha):
    try:
        return _outage_history(value, workflow, producer_sha)
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, ValueError, TypeError, KeyError, AttributeError,
            RecursionError, OverflowError, UnicodeError):
        raise evidence.CompatRefusal('compat-stale', 'outage-history-invalid') from None


def validate_outage_history(collected, *, workflow, producer_sha):
    _workflow(CollectionRequest(producer_sha, workflow))
    return _outage_safe(collected, workflow, producer_sha)


def _validate_outage_history_custody(collected, *, workflow, producer_sha):
    """Only fixed W0/current original-custody replay, not source coverage."""
    return _outage_safe(collected, workflow, producer_sha)


def _missing_expired(request, collected):
    evidence._request(request)
    history = collected.normal.history
    _need(request.observation_id == history.observation_id and request.source.sha == history.sha
          and not collected.normal.issues, 'outage-history-request')
    missing = not history.attempts
    for attempt in history.attempts:
        window = attempt.execution_window
        _need(attempt.status == 'completed' and window is not None and window.fresh,
              'outage-history-unqualified')
        if type(attempt.measurement) is evidence.AuthenticatedPair:
            measured = evidence.parse_authenticated_pair(attempt.measurement, attempt)
            if not measured.positive:
                if evidence._negative_current(measured, request):
                    evidence._negative(measured)
                raise evidence.CompatRefusal('compat-stale', 'outage-authenticated-negative')
            _need(evidence._current(measured, request) and evidence._successful(attempt),
                  'outage-history-positive-unqualified')
        else:
            _need(attempt.measurement in (evidence.MissingEvidence.MISSING, evidence.MissingEvidence.EXPIRED),
                  'outage-history-unavailable')
            missing = True
    _need(missing, 'outage-history-no-missing-evidence')
    return collected


def require_missing_expired(request, collected, *, workflow):
    evidence._request(request)
    validate_outage_history(collected, workflow=workflow, producer_sha=request.source.sha)
    return _missing_expired(request, collected)


def _require_missing_expired_custody(request, collected, *, workflow):
    evidence._request(request)
    _validate_outage_history_custody(collected, workflow=workflow, producer_sha=request.source.sha)
    return _missing_expired(request, collected)
