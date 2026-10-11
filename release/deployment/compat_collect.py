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

WORKFLOW_SHA256 = "0f8ae9f3079d4a3c0749721e5100d6df60842b4811059aee02a2ee4d8ced4f6b"
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

    def _remaining(self, maximum):
        now = self._clock()
        _need(type(now) in (int, float) and math.isfinite(now) and self._last <= now < self._deadline, "collect-deadline")
        self._last = now
        return min(maximum, self._deadline - now)

    def _run(self, args, *, code):
        args = list(args)
        limit, duration = 4 * 1024 * 1024, 30
        allowed = args == ["--version"]
        if len(args) == 8 and args[:3] == ["api", "--method", "GET"] and tuple(args[4:]) == HEADERS:
            allowed = any(p.fullmatch(args[3]) for p in _GETS)
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
            _need(timeout > 0, "collect-deadline")
            stdout_limit = min(limit, remaining_bytes - STDERR_LIMIT)
            self._commands += 1
            env = dict(self._env)
            env["GH_HOST"] = "github.com"
            result = self._capture([self.gh_bin, *args], env=env, timeout=timeout,
                stdout_limit=stdout_limit, stderr_limit=STDERR_LIMIT)
            self._remaining(duration)
            _need(_directory(self._config) == self._config_identity, "collect-directory-changed")
            _need(type(result.returncode) is int and type(result.stdout) is bytes and type(result.stderr) is bytes
                  and len(result.stdout) <= stdout_limit and len(result.stderr) <= STDERR_LIMIT, "collect-capture")
            self._bytes += len(result.stdout) + len(result.stderr)
            _need(self._bytes <= MAX_BYTES, "collect-bound")
            self._transcript.update(common.canonical_file_bytes({"command": self._commands,
                "returncode": result.returncode, "stdout": _hash(result.stdout), "stderr": _hash(result.stderr)}))
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


def _snapshot(client, request, steps, observed):
    workflow = client.get(API + "/actions/workflows/compat.yml")
    _need(type(workflow) is dict and workflow.get("path") == evidence.WORKFLOW
          and workflow.get("name") == "Compatibility" and workflow.get("state") == "active", "collect-workflow")
    wid = _integer(workflow.get("id"))
    runs, total, numbers = {}, 0, set()
    for event in ("push", "workflow_dispatch"):
        raw = _pages(client, API + f"/actions/workflows/{wid}/runs?head_sha={request.sha}&branch=main&event={event}", "workflow_runs", evidence.MAX_ATTEMPTS)
        for row in raw:
            run = _run_meta(row, request, wid, observed)
            _need(run["event"] == event and run["id"] not in runs and run["number"] not in numbers, "collect-run-duplicate")
            runs[run["id"]] = run; numbers.add(run["number"]); total += run["attempt"]
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
    _need(all_checks == listed_checks, "collect-check-inventory")
    return attempts, (wid, runs, projections, artifacts), artifacts


def _safe_projection(value):
    if isinstance(value, datetime): return value.isoformat()
    if type(value) is evidence.Job: return tuple(getattr(value,k) for k in value.__slots__)
    if type(value) is evidence.ExecutionWindow: return [_safe_projection(value.started_at), _safe_projection(value.completed_at), value.fresh]
    if type(value) in (tuple,list): return [_safe_projection(v) for v in value]
    if type(value) is dict: return {str(k):_safe_projection(v) for k,v in value.items()}
    return value


def collect_history(request, client, scratch, *, wall_clock=lambda:datetime.now(timezone.utc)):
    """Collect once; no policy grant and no claim against deleted whole runs."""
    steps = _workflow(request)
    _need(type(client) is ActionsGh and not client._used, "collect-client-reuse")
    client._used = True
    try:
        _directory(scratch)
        start = wall_clock()
        _need(type(start) is datetime and start.tzinfo == timezone.utc, "collect-clock")
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
        try:
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
        return CollectedHistory(history, tuple(sorted(set(issues))), client._commands, client._bytes, transcript)
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, OSError, ValueError, TypeError, KeyError, AttributeError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "collect-invalid") from None
