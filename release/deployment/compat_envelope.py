"""Pure, bounded replay of fixed workflow and API envelope facts.

These data are not signatures or collection capabilities. No source, clock,
network or subprocess read occurs here. Unknown workflow bytes yield no window.
"""
from dataclasses import dataclass, field
from datetime import datetime, timezone
import hashlib
import json
import re
from types import SimpleNamespace

import compat_evidence as evidence
import ship_common as common

WORKFLOW_SHA256 = "73c655c829fa36a109dfee621f1efa4529cf89ffa112599a8ff1bb8391b7eff9"
P0_WORKFLOW_SHA256 = "56005fe2015e8385e6ecfc621a403c06a534ed9a5712e5ec4ec4f9c70407b941"
W0_WORKFLOW_SHA256 = "0f8ae9f3079d4a3c0749721e5100d6df60842b4811059aee02a2ee4d8ced4f6b"
REPOSITORY_ID = 1297344686
API = "/repos/" + evidence.REPOSITORY
WEB = "https://github.com/" + evidence.REPOSITORY
API_URL = "https://api.github.com" + API
_ID = r"[1-9][0-9]{0,15}"
_ACTIVE = {"queued", "in_progress", "waiting", "pending", "requested"}
_CONCLUSIONS = {"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale"}
_TIME = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?Z\Z")
# Preserve the moved predicate's exact AST without importing an operational adapter.
adapter = SimpleNamespace(MAX_ARCHIVE=64 * 1024)
MAX_RAW_BYTES = 4 * 1024 * 1024

def _need(ok, detail):
    if not ok:
        raise evidence.CompatRefusal("compat-stale", detail)

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
        cancelled = attempt["status"] == "completed" and attempt["conclusion"] == "cancelled"
        _need(row.get("steps") == [] and (name in ("compat-run", "compat-attest") or cancelled), "collect-skipped-job")
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
    executed = (start, end) if terminal_known and conclusion != "skipped" else None
    return typed, eligible, terminal_known, executed, (created, start, end, steps)

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


@dataclass(frozen=True, slots=True)
class WorkflowObservation:
    sha: str
    raw: bytes | None = field(repr=False)


@dataclass(frozen=True, slots=True)
class EnvelopeFacts:
    raw: bytes = field(repr=False)


@dataclass(frozen=True, slots=True)
class ArtifactFact:
    id: int
    name: str
    size: int
    expired: bool
    created: datetime
    updated: datetime
    expires: datetime


@dataclass(frozen=True, slots=True)
class ReplayedEnvelope:
    """Derived facts only; callers cannot supply these to qualification."""
    attempt: evidence.Attempt
    workflow_id: int
    created_at: datetime
    updated_at: datetime
    suite: int
    job_ids: tuple[int, ...]
    check_ids: tuple[int, ...]
    listed_checks: tuple[tuple[int, bytes], ...]
    artifacts: tuple[ArtifactFact, ...]
    artifacts_raw: bytes


_RUN_KEYS = frozenset(('id', 'name', 'repository', 'head_repository', 'head_sha',
    'head_branch', 'event', 'path', 'workflow_id', 'run_number', 'run_attempt',
    'latest_run_attempt', 'check_suite_id', 'status', 'conclusion', 'created_at',
    'run_started_at', 'updated_at', 'url', 'html_url'))
_JOB_KEYS = frozenset(('id', 'name', 'run_id', 'run_attempt', 'head_sha', 'status',
    'conclusion', 'created_at', 'started_at', 'completed_at', 'steps', 'url',
    'html_url', 'check_run_url'))
_STEP_KEYS = frozenset(('number', 'name', 'status', 'conclusion', 'started_at', 'completed_at'))
_CHECK_KEYS = frozenset(('id', 'name', 'app', 'check_suite', 'head_sha', 'status',
    'conclusion', 'started_at', 'completed_at', 'url', 'html_url', 'details_url'))
_ARTIFACT_KEYS = frozenset(('id', 'name', 'size_in_bytes', 'expired', 'created_at',
    'updated_at', 'expires_at', 'workflow_run', 'url', 'archive_download_url'))


def _keys(value, keys):
    _need(type(value) is dict and set(value) == keys, 'envelope-keys')


def _tree(value, depth=0, count=None):
    if count is None:
        count = [0]
    count[0] += 1
    _need(depth <= 8 and count[0] <= 100000, 'envelope-bound')
    if type(value) is dict:
        for key, item in value.items():
            _need(type(key) is str and len(key) <= 1024, 'envelope-scalar')
            _tree(item, depth + 1, count)
    elif type(value) is list:
        for item in value:
            _tree(item, depth + 1, count)
    else:
        _need(value is None or type(value) in (str, int, bool), 'envelope-scalar')
        if type(value) is str:
            _need(len(value) <= 1024, 'envelope-scalar')
        if type(value) is int:
            _need(0 <= value <= 10**16 - 1, 'envelope-scalar')


def _ordered(rows, maximum):
    _need(type(rows) is list and len(rows) <= maximum, 'envelope-bound')
    ids = []
    for row in rows:
        _need(type(row) is dict, 'envelope-keys')
        ids.append(_integer(row.get('id')))
    _need(ids == sorted(set(ids)), 'envelope-order')


def validate_workflow(value):
    _need(type(value) is WorkflowObservation and type(value.sha) is str
          and re.fullmatch(r'[0-9a-f]{40}', value.sha) is not None
          and (value.raw is None or (type(value.raw) is bytes and len(value.raw) <= MAX_RAW_BYTES)),
          'envelope-workflow')
    return value


def _reviewed_steps(value):
    validate_workflow(value)
    if value.raw is None or hashlib.sha256(value.raw).hexdigest() not in (W0_WORKFLOW_SHA256, P0_WORKFLOW_SHA256, WORKFLOW_SHA256):
        return None
    steps, job = {}, None
    for line in value.raw.decode('ascii').splitlines():
        if re.fullmatch(r'  (release-preview|compat-run|compat|attest):', line):
            job = line.strip()[:-1]
            if job == 'attest':
                job = 'compat-attest'
            steps[job] = []
        elif job and line.startswith('      - name: '):
            steps[job].append(line[len('      - name: '):])
    _need(set(steps) == set(evidence.JOBS), 'envelope-workflow')
    return steps


def _shape(value, workflow, observed):
    _keys(value, {'version', 'run', 'jobs', 'checks', 'artifacts'})
    _need(type(value['version']) is int and value['version'] == 1, 'envelope-version')
    raw = value['run']
    _keys(raw, _RUN_KEYS)
    for name in ('repository', 'head_repository'):
        _keys(raw[name], {'id', 'full_name'})
    wid = _integer(raw['workflow_id'])
    run = _run_meta(raw, workflow, wid, observed)
    latest = _integer(raw['latest_run_attempt'], evidence.MAX_ATTEMPTS)
    _need(run['attempt'] <= latest, 'envelope-attempt')
    _ordered(value['jobs'], 4)
    _ordered(value['checks'], 4 * latest)
    _ordered(value['artifacts'], 3 * latest)
    checks = {}
    for check in value['checks']:
        _keys(check, _CHECK_KEYS)
        _keys(check['app'], {'id'}); _keys(check['check_suite'], {'id'})
        cid = _integer(check['id'])
        _need(type(check['app']['id']) is int and check['app']['id'] == 15368
              and type(check['check_suite']['id']) is int and check['check_suite']['id'] == run['suite']
              and check['head_sha'] == workflow.sha and type(check['name']) is str
              and 0 < len(check['name']) <= 256
              and check['url'] == API_URL + f'/check-runs/{cid}', 'envelope-check')
        _state(check)
        for name in ('started_at', 'completed_at'):
            if check[name] is not None:
                _date(check[name], observed)
        checks[cid] = check
    for job in value['jobs']:
        _keys(job, _JOB_KEYS)
        jid = _integer(job['id'])
        _need(type(job['name']) is str and 0 < len(job['name']) <= 256
              and type(job['run_id']) is int and job['run_id'] == run['id']
              and type(job['run_attempt']) is int and job['run_attempt'] == run['attempt']
              and job['head_sha'] == workflow.sha
              and job['url'] == API_URL + f'/actions/jobs/{jid}'
              and job['html_url'] == WEB + f"/actions/runs/{run['id']}/job/{jid}", 'envelope-job')
        _date(job['created_at'], observed)
        for name in ('started_at', 'completed_at'):
            if job[name] is not None:
                _date(job[name], observed)
        _state(job)
        match = re.fullmatch(re.escape(API_URL) + rf'/check-runs/({_ID})', job['check_run_url'])
        _need(match is not None and int(match[1]) in checks, 'envelope-check')
        check = checks[int(match[1])]
        _need(all(check[name] == job[name] for name in ('name', 'status', 'conclusion', 'started_at', 'completed_at'))
              and check['html_url'] == check['details_url'] == job['html_url'], 'envelope-check')
        _need(type(job['steps']) is list and len(job['steps']) <= 100, 'envelope-bound')
        previous, names = 0, set()
        for step in job['steps']:
            _keys(step, _STEP_KEYS)
            number = _integer(step['number'], 100)
            _need(number > previous and type(step['name']) is str and 0 < len(step['name']) <= 256
                  and step['name'] not in names, 'envelope-step')
            previous = number; names.add(step['name'])
            _state(step)
            for name in ('started_at', 'completed_at'):
                if step[name] is not None:
                    _date(step[name], observed)
    artifacts = []
    for raw_artifact in value['artifacts']:
        _keys(raw_artifact, _ARTIFACT_KEYS)
        _keys(raw_artifact['workflow_run'], {'id', 'repository_id', 'head_repository_id', 'head_sha', 'head_branch'})
        item = _artifact(raw_artifact, {**run, 'attempt': latest}, workflow.sha, observed)
        artifacts.append(ArtifactFact(**item))
    _need(len({row.name for row in artifacts}) == len(artifacts), 'envelope-artifact')
    return run, latest, wid, checks, tuple(artifacts)


def replay(workflow, facts, *, observed_at):
    """Rebuild the actual window; never accept caller-supplied eligibility."""
    try:
        validate_workflow(workflow)
        _need(type(observed_at) is datetime and observed_at.tzinfo is timezone.utc
              and observed_at.fold == 0, 'envelope-clock')
        _need(type(facts) is EnvelopeFacts and type(facts.raw) is bytes
              and 0 < len(facts.raw) <= MAX_RAW_BYTES, 'envelope-bound')
        value = common.loads_strict(facts.raw)
        _tree(value)
        _need(common.canonical_file_bytes(value) == facts.raw, 'envelope-canonical')
        run, latest, wid, checks, artifacts = _shape(value, workflow, observed_at)
        expected = _reviewed_steps(workflow)
        jobs, names, intervals, known = [], set(), [], expected is not None
        job_ids, check_ids = [], []
        for raw in value['jobs']:
            job_ids.append(raw['id']); check_ids.append(int(raw['check_run_url'].rsplit('/', 1)[1]))
            _need(raw['name'] not in names, 'envelope-job-duplicate')
            names.add(raw['name'])
            if expected is not None:
                typed, eligible, terminal, executed, _ = _job(raw, checks, run, workflow.sha, expected, observed_at)
                known &= terminal
                if eligible or typed.conclusion == 'skipped':
                    jobs.append(typed)
                if executed is not None:
                    intervals.append(executed)
        _need(len(set(check_ids)) == len(check_ids), 'envelope-check-duplicate')
        known &= names == set(evidence.JOBS) and run['status'] == 'completed' and bool(intervals)
        completion = max(end for _, end in intervals) if known else None
        window = evidence.ExecutionWindow(min(start for start, _ in intervals), completion, True) if known and len(jobs) == 4 else None
        attempt = evidence.Attempt(evidence.REPOSITORY, evidence.WORKFLOW, workflow.sha, workflow.sha,
            run['event'], evidence.REF, run['id'], run['number'], run['attempt'], latest,
            run['status'], run['conclusion'], run['created'] if run['attempt'] == 1 else None,
            run['started'], completion, tuple(sorted(jobs, key=lambda j:j.name)),
            evidence.MissingEvidence.ACTIVE if run['status'] != 'completed' else evidence.MissingEvidence.MISSING,
            execution_window=window)
        evidence._attempt(attempt, workflow.sha, observed_at)
        return ReplayedEnvelope(attempt, wid, run['created'], run['updated'], run['suite'],
            tuple(job_ids), tuple(check_ids),
            tuple((c['id'], common.canonical_file_bytes(c)) for c in value['checks']),
            artifacts, common.canonical_file_bytes(value['artifacts']))
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, ValueError, TypeError, KeyError, AttributeError,
            RecursionError, OverflowError, UnicodeError):
        raise evidence.CompatRefusal('compat-stale', 'envelope-invalid') from None


# Fixed lossless API projections shared by both concrete collectors.
def _project_fields(value, keys):
    _need(type(value) is dict and keys <= value.keys(), 'rollback-collect-field')
    return {key: value[key] for key in sorted(keys)}


def _canonical_projection(value, maximum):
    """Bound the canonical retained buffer as it is encoded, including LF."""
    output = bytearray()
    for part in json.JSONEncoder(sort_keys=True, separators=(',', ':'),
                                 ensure_ascii=True, allow_nan=False).iterencode(value):
        raw = part.encode('ascii')
        _need(len(output) + len(raw) + 1 <= maximum, 'rollback-history-bound')
        output.extend(raw)
    output.extend(b'\n')
    return bytes(output)


def _project_run(value, latest):
    row = _project_fields(value, _RUN_KEYS - {'latest_run_attempt'})
    row['latest_run_attempt'] = latest
    for key in ('repository', 'head_repository'):
        row[key] = _project_fields(row[key], {'id', 'full_name'})
    return row


def _project_job(value):
    row = _project_fields(value, _JOB_KEYS)
    _need(type(row['steps']) is list and len(row['steps']) <= 100,
          'rollback-collect-steps')
    row['steps'] = [_project_fields(step, _STEP_KEYS) for step in row['steps']]
    return row


def _project_check(value):
    row = _project_fields(value, _CHECK_KEYS)
    row['app'] = _project_fields(row['app'], {'id'})
    row['check_suite'] = _project_fields(row['check_suite'], {'id'})
    return row


def _project_artifact(value):
    row = _project_fields(value, _ARTIFACT_KEYS)
    row['workflow_run'] = _project_fields(row['workflow_run'],
        {'id', 'repository_id', 'head_repository_id', 'head_sha', 'head_branch'})
    return row
