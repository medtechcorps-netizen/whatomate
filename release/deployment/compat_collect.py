"""Bounded visible Actions collection, not ship activation or deletion coverage.

Only the reviewed paired producer is supported. Rerun request time is unknown:
its signed evidence is retained, but the current pure consumer cannot qualify
a rerun positive. A later explicit execution-interval policy must lift that
preparatory limitation; run_started_at is never called a human request time.
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
import release_record as records
import ship_common as common

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "compat"))
import runtime

WORKFLOW_SHA256 = "dab7e577a153983e717bffe6881cdb72d1d5113cf2076230048cfa7ea595424c"
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


def _need(ok, detail):
    if not ok:
        raise evidence.CompatRefusal("compat-stale", detail)


def _hash(raw):
    return hashlib.sha256(raw).hexdigest()


def _integer(value, maximum=10**16-1, minimum=1):
    _need(type(value) is int and minimum <= value <= maximum, "collect-integer")
    return value


def _date(value, observed=None):
    _need(type(value) is str and _TIME.fullmatch(value) is not None, "collect-time")
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise evidence.CompatRefusal("compat-stale", "collect-time") from None
    _need(observed is None or result <= observed, "collect-time")
    return result


def _state(row):
    status, conclusion = row.get("status"), row.get("conclusion")
    _need(type(status) is str and ((status in _ACTIVE and conclusion is None)
          or (status == "completed" and type(conclusion) is str and conclusion in _CONCLUSIONS)), "collect-state")
    return status, conclusion


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


def _repository(value):
    _need(type(value) is dict and type(value.get("id")) is int and value["id"] == REPOSITORY_ID
          and value.get("full_name") == evidence.REPOSITORY, "collect-repository")


def _run_meta(row, request, workflow, observed):
    _need(type(row) is dict, "collect-run")
    _repository(row.get("repository")); _repository(row.get("head_repository"))
    rid = _integer(row.get("id"))
    _need(row.get("head_sha") == request.sha and row.get("head_branch") == "main"
          and row.get("event") in ("push", "workflow_dispatch") and row.get("path") == evidence.WORKFLOW
          and type(row.get("workflow_id")) is int and row["workflow_id"] == workflow
          and row.get("name") == "Compatibility" and row.get("url") == API_URL + f"/actions/runs/{rid}"
          and row.get("html_url") == WEB + f"/actions/runs/{rid}", "collect-run-identity")
    status, conclusion = _state(row)
    created = _date(row.get("created_at"), observed)
    started = _date(row["run_started_at"], observed) if row.get("run_started_at") is not None else None
    updated = _date(row.get("updated_at"), observed)
    _need(created <= updated and (started is None or created <= started <= updated), "collect-run-time")
    return {"id":rid, "number":_integer(row.get("run_number")),
        "attempt":_integer(row.get("run_attempt"), evidence.MAX_ATTEMPTS),
        "suite":_integer(row.get("check_suite_id")), "event":row["event"],
        "status":status, "conclusion":conclusion, "created":created, "started":started, "updated":updated}


def _steps(rows, expected, job_start, job_end, observed):
    _need(type(rows) is list and len(rows) <= len(expected) + 3, "collect-steps")
    source_order, seen, previous, positive = [], set(), 0, True
    for step in rows:
        _need(type(step) is dict, "collect-steps")
        number = _integer(step.get("number"), 100)
        name = step.get("name")
        _need(type(name) is str and name not in seen and number > previous
              and name in (*expected, "Set up job", "Complete job", "Post Check out the exact candidate"), "collect-step-identity")
        seen.add(name); previous = number
        if name in expected: source_order.append(name)
        status, conclusion = _state(step)
        start = _date(step["started_at"], observed) if step.get("started_at") is not None else None
        end = _date(step["completed_at"], observed) if step.get("completed_at") is not None else None
        if conclusion == "skipped":
            _need((start is None and end is None) or (start is not None and end == start), "collect-step-time")
            positive = False
        elif status in _ACTIVE and start is None and end is None:
            positive = False
        else:
            _need(start is not None and job_start is not None and job_start <= start
                  and (end is None or start <= end) and (job_end is None or (end is not None and end <= job_end)), "collect-step-time")
            positive &= status == "completed" and conclusion == "success"
    _need(source_order == [name for name in expected if name in seen], "collect-step-order")
    complete = source_order == expected and all(s.get("status") == "completed" for s in rows)
    return complete, positive, tuple((s["name"], s["number"], s["status"], s["conclusion"], s.get("started_at"), s.get("completed_at")) for s in rows)


def _job(row, checks, attempt, sha, expected, observed):
    jid = _integer(row.get("id")); name = row.get("name")
    _need(type(name) is str and name in evidence.JOBS, "collect-job")
    rid, a = attempt["id"], attempt["attempt"]
    check_url = row.get("check_run_url")
    match = re.fullmatch(re.escape(API_URL) + rf"/check-runs/({_ID})", check_url) if type(check_url) is str else None
    _need(match is not None, "collect-check-url")
    cid = int(match[1]); check = checks.get(cid)
    _need(type(check) is dict and row.get("run_id") == rid and type(row.get("run_id")) is int
          and row.get("run_attempt") == a and type(row.get("run_attempt")) is int and row.get("head_sha") == sha
          and row.get("url") == API_URL + f"/actions/jobs/{jid}"
          and row.get("html_url") == WEB + f"/actions/runs/{rid}/job/{jid}", "collect-job-identity")
    status, conclusion = _state(row)
    _need(type(check.get("app")) is dict and type(check["app"].get("id")) is int and check["app"]["id"] == 15368
          and type(check.get("check_suite")) is dict and type(check["check_suite"].get("id")) is int
          and check["check_suite"]["id"] == attempt["suite"] and check.get("head_sha") == sha
          and check.get("name") == name and _state(check) == (status, conclusion)
          and check.get("html_url") == row["html_url"] and check.get("details_url") == row["html_url"]
          and check.get("url") == check_url, "collect-check-identity")
    _need(all(check.get(k) == row.get(k) for k in ("started_at", "completed_at")), "collect-check-time")
    created = _date(row.get("created_at"), observed)
    start = _date(row["started_at"], observed) if row.get("started_at") is not None else None
    end = _date(row["completed_at"], observed) if row.get("completed_at") is not None else None
    fresh = attempt["started"] is not None and start is not None and attempt["started"] <= created <= start
    if conclusion == "skipped":
        _need(row.get("steps") == [] and name in ("compat-run", "compat-attest"), "collect-skipped-job")
        # A recognized skipped dependent does no work. Its synthetic interval
        # is never success evidence or the completion envelope's endpoint.
        complete, positive, steps, fresh = True, False, (), False
    else:
        _need(end is None or (start is not None and start <= end), "collect-job-time")
        complete, positive, steps = _steps(row.get("steps"), expected[name], start, end, observed)
        fresh &= status != "completed" or end is not None
    typed = evidence.Job(name, jid, cid, 15368, rid, a, sha, status, conclusion)
    eligible = fresh and (conclusion != "success" or positive)
    terminal_known = status == "completed" and complete and (conclusion == "skipped" or fresh)
    return typed, eligible, terminal_known, end if conclusion != "skipped" else None, (created, start, end, steps)


def _artifact(row, run, sha, observed):
    identity = _integer(row.get("id")); name = row.get("name")
    _need(type(name) is str, "collect-artifact")
    match = re.fullmatch(rf"compat-(?:resolution|measured)-{sha}-{run['id']}-([1-9][0-9]*)|compat-result-{sha}-([1-9][0-9]*)", name)
    _need(match is not None and 1 <= int(match[1] or match[2]) <= run["attempt"], "collect-artifact-name")
    _integer(row.get("size_in_bytes"), adapter.MAX_ARCHIVE)
    _need(type(row.get("expired")) is bool and type(row.get("workflow_run")) is dict, "collect-artifact")
    wr = row["workflow_run"]
    _need(all(type(wr.get(k)) is int and wr[k] == v for k,v in
              (("id",run["id"]),("repository_id",REPOSITORY_ID),("head_repository_id",REPOSITORY_ID)))
          and wr.get("head_branch") == "main" and wr.get("head_sha") == sha, "collect-artifact-run")
    created = _date(row.get("created_at"), observed); updated = _date(row.get("updated_at"), observed)
    expires = _date(row.get("expires_at"))
    _need(run["created"] <= created <= updated and created <= expires
          and (row["expired"] or observed < expires)
          and row.get("url") == API_URL + f"/actions/artifacts/{identity}"
          and row.get("archive_download_url") == API_URL + f"/actions/artifacts/{identity}/zip", "collect-artifact-time")
    # Optional API digest is deliberately not interpreted as a ZIP digest.
    return {"id":identity,"name":name,"size":row["size_in_bytes"],"expired":row["expired"],
            "created":created,"updated":updated,"expires":expires}


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
            jobs, details, names, known, ends = [], [], set(), True, []
            for job in raw_jobs:
                typed, eligible, terminal, end, detail = _job(job, checks, run, request.sha, steps, observed)
                _need(typed.name not in names and typed.job_id not in all_jobs and typed.check_run_id not in all_checks, "collect-job-duplicate")
                names.add(typed.name); all_jobs.add(typed.job_id); all_checks.add(typed.check_run_id)
                known &= terminal
                if eligible or typed.conclusion == "skipped": jobs.append(typed)
                if end is not None: ends.append(end)
                details.append((typed, eligible, terminal, detail))
            known &= names == set(evidence.JOBS) and run["status"] == "completed" and bool(ends)
            completion = max(ends) if known else None
            attempt = evidence.Attempt(evidence.REPOSITORY, evidence.WORKFLOW, request.sha, request.sha,
                run["event"], evidence.REF, rid, run["number"], number, latest["attempt"], run["status"],
                run["conclusion"], run["created"] if number == 1 else None, run["started"], completion,
                tuple(sorted(jobs,key=lambda j:j.name)), evidence.MissingEvidence.ACTIVE if run["status"] != "completed" else evidence.MissingEvidence.MISSING)
            attempts.append(attempt)
            projections.append((run, tuple(sorted(details,key=lambda d:d[0].name))))
    _need(all_checks == listed_checks, "collect-check-inventory")
    return attempts, (wid, runs, projections, artifacts), artifacts


def _safe_projection(value):
    if isinstance(value, datetime): return value.isoformat()
    if type(value) is evidence.Job: return tuple(getattr(value,k) for k in value.__slots__)
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
