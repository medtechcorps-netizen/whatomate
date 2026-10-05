"""Semantic tests for the CI workflows that branch protection relies on.

They replace the retired suite's byte pins on test.yml. Each check states one
property of the whole workflow set, passes on the checked-in files, and has
negative cases that break the property and must make the checker fail:

1. each required status check is reported by one job whose id and name equal
   it, and the E2E shards stay e2e-shard-1 to e2e-shard-4;
2. no other job in any workflow can report one of those names;
3. CI runs on every push to and pull request for main with no path filter, no
   workflow uses pull_request_target or workflow_run, every action is pinned
   to a full commit, every service image to a digest (PostgreSQL to the
   reviewed 17 image), and no checkout persists credentials;
4. main push runs never cancel each other, and the CI timeouts hold;
5. only ship.yml names a deployment environment or reads a secret other than
   GITHUB_TOKEN, and each environment belongs to exactly the job mapped here;
6. actionlint checks every workflow;
7. the Test workflow runs the release/deployment suite (this file included)
   in a job its aggregator needs;
8. each CI aggregator runs even when a job fails, needs every other job of its
   workflow and fails unless each one succeeded;
9. the frontend audit keeps only the dated, dev-only braces exception and
   fails closed on everything else;
10. every job of a CI workflow other than its aggregator runs unconditionally;
11. lint runs the pinned golangci-lint, build builds every package, and
    security runs govulncheck, rejects ambient Trivy suppression files right
    before its scans, and fails on any CRITICAL or HIGH finding in each image
    it builds, with no exception file.
12. the Test workflow runs Stage 1's schema guard (schema_change.py) on every
    pull request, from its merge base with main, and on every push to main,
    from the first parent, right after a full-history checkout, in a job the
    test aggregator needs. Its embedded script runs against a synthetic
    repository: it must refuse exactly the guarded changes, judge a pull
    request only by its own changes, and fail closed on anything unexpected.
13. the Test workflow proves Part A's staging database bootstrap
    (release/staging/bootstrap) in a required job: on DigitalOcean's role
    shape it builds the database, this commit's rls-migrate accepts it twice
    with the schema and data unchanged, a second bootstrap is refused, the
    server serves a login on it, and an owner role that the doadmin-like role
    created is refused; every step runs unconditionally and fails closed;
14. no release image and no workflow that builds or tests one references the
    staging code (release/staging, the graph stub): the Dockerfiles under
    docker/ outside docker/staging/, ship.yml, e2e-tests.yml, and every Test
    job except staging-bootstrap.

The workflows are read with test_ship_workflow's YAML-subset reader plus the
folded scalars the CI workflows use; when PyYAML happens to be importable the
parse is cross-checked.
"""

from __future__ import annotations

import atexit
import datetime as dt
import functools
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any, Callable

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_ship_support import TempRepo
from test_ship_workflow import MiniYaml


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"

# Branch protection's required status checks on main, each with the one job
# (workflow file, job id) that reports it.
REQUIRED_CONTEXTS = {
    "test": ("test.yml", "test"),
    "lint": ("test.yml", "lint"),
    "build": ("test.yml", "build"),
    "security": ("test.yml", "security"),
    "e2e": ("e2e-tests.yml", "e2e"),
    "tenant-isolation": ("test.yml", "tenant-isolation"),
}
E2E_SHARDS = [1, 2, 3, 4]
# Each CI workflow's aggregator and its concurrency group prefix.
AGGREGATORS = {"test.yml": "test", "e2e-tests.yml": "e2e"}
CI_PREFIXES = {"test.yml": "test", "e2e-tests.yml": "e2e"}
CI_TRIGGERS = {"push": {"branches": ["main"]}, "pull_request": {"branches": ["main"]}}
FORBIDDEN_TRIGGERS = ("pull_request_target", "workflow_run")
# Every job that names a deployment environment, and the environment it names.
ENVIRONMENTS = {("ship.yml", "production"): "production"}
SECRET_WORKFLOWS = {"ship.yml"}
PG17_IMAGE = "postgres:17@sha256:e38411452a464af89e5adadb8d223bf53b898d47d6ef918b2d58c08707350449"
POSTGRES_JOBS = (("test.yml", "tenant-isolation"), ("test.yml", "go-race"), ("test.yml", "staging-bootstrap"),
                 ("e2e-tests.yml", "e2e-shard"))
PINNED_USES = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}")
DIGEST_IMAGE = re.compile(r"[a-z0-9./_-]+(?::[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}")
TENANT_TEST = "go test -mod=readonly -v -timeout 45m ./internal/database -run '^TestTenantRLS_'"
RACE_TEST = '-- -mod=readonly -race -p 1 -timeout 150m -coverprofile=coverage.out "${packages[@]}"'
ACTIONLINT_INSTALL = "go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7"
# Exactly the test discovery the release tests job runs. A new release test
# directory is added here and to the job together.
RELEASE_TEST_COMMANDS = ("python3 -B -m unittest discover -s release/deployment -p 'test_*.py' -v",)
GOLANGCI_LINT = {"version": "v2.11.4"}
GO_BUILD = "go build -mod=readonly -v ./..."
GOVULNCHECK = ("go install golang.org/x/vuln/cmd/govulncheck@v1.7.0", "GOFLAGS=-mod=readonly govulncheck ./...")
AMBIENT_TRIVY_POLICY = [
    "set -euo pipefail",
    "for path in .trivyignore .trivyignore.yaml trivy.yaml trivy.yml; do",
    '[[ ! -e "$path" && ! -L "$path" ]]',
    "done",
]
# Each image the security job builds and scans, with its Dockerfile, and the
# scan settings every one of them gets. Scanning other images or applying an
# exception file is a reviewed edit of these lines.
SCANNED_IMAGES = {
    "rereply:ci": "docker/Dockerfile",
    "rereply-meta-relay:ci": "docker/meta-relay.Dockerfile",
    "rereply-gmail-relay:ci": "docker/gmail-relay.Dockerfile",
}
TRIVY_SCAN = {"format": "table", "exit-code": "1", "vuln-type": "os,library", "severity": "CRITICAL,HIGH"}
# #213: GHSA-vfj7-8cjw-p6xm, reviewed to 2026-12-31. A renewal or a new
# exception is a reviewed edit of this line.
BRACES_EXCEPTION = ("braces", "stack-exhaustion denial of service", "2026-12-31")


class WorkflowYaml(MiniYaml):
    """MiniYaml plus the folded scalars the CI workflows use for service
    options. More-indented lines inside a folded scalar stay outside the
    contract."""

    def value(self, rest: str, indent: int) -> Any:
        if rest in {">", ">-"}:
            return self.folded(indent, keep_last=rest == ">")
        return super().value(rest, indent)

    def folded(self, indent: int, *, keep_last: bool) -> str:
        text = ""
        for line in self.literal(indent, keep_last=False).split("\n"):
            if line[:1] in {" ", "\t"}:
                raise ValueError("more-indented folded lines are outside the contract")
            if not line:
                text += "\n"
            elif text and not text.endswith("\n"):
                text += " " + line
            else:
                text += line
        return text + "\n" if keep_last and text else text


def read_sources() -> dict[str, str]:
    sources: dict[str, str] = {}
    for path in sorted(WORKFLOWS.iterdir()):
        if path.suffix not in {".yml", ".yaml"}:
            continue
        if path.is_symlink() or not path.is_file():
            raise AssertionError(f"workflow is not a regular file: {path.name}")
        sources[path.name] = path.read_text(encoding="utf-8")
    return sources


SOURCES = read_sources()


def parse(sources: dict[str, str]) -> dict[str, dict[str, Any]]:
    return {name: WorkflowYaml(text).parse() for name, text in sources.items()}


def jobs(doc: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return doc["jobs"]


def needs(job: dict[str, Any]) -> list[str]:
    value = job.get("needs", [])
    return [value] if type(value) is str else list(value)


def run_lines(step: dict[str, Any]) -> list[str]:
    return [line.strip() for line in str(step.get("run", "")).splitlines()
            if line.strip() and not line.strip().startswith("#")]


def step_named(job: dict[str, Any], name: str) -> dict[str, Any]:
    matches = [item for item in job.get("steps", []) if item.get("name") == name]
    if len(matches) != 1:
        raise AssertionError(f"step {name!r} is missing or not unique")
    return matches[0]


def heredoc(run: str, marker: str) -> str:
    opener = f"<<'{marker}'\n"
    if run.count(opener) != 1:
        raise AssertionError(f"expected one {marker} heredoc")
    body, closed, _ = run.split(opener, 1)[1].partition(f"\n{marker}\n")
    if not closed:
        raise AssertionError(f"the {marker} heredoc is not closed")
    return body + "\n"


def run_python(script: str, *args: str, files: dict[str, str] | None = None,
               env: dict[str, str] | None = None) -> int:
    """Run a workflow's embedded Python the way its step does, in a scratch directory."""
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        (root / "policy.py").write_text(script, encoding="utf-8")
        for name, text in (files or {}).items():
            (root / name).write_text(text, encoding="utf-8")
        return subprocess.run(
            [sys.executable, "-I", "-S", "policy.py", *args], cwd=root, env={**os.environ, **(env or {})},
            capture_output=True, text=True, timeout=60,
        ).returncode


def name_pattern(name: str) -> re.Pattern[str]:
    # An expression in a job name can render as any text.
    return re.compile(".*".join(re.escape(part) for part in re.split(r"\$\{\{.*?\}\}", name)))


def expected_concurrency(prefix: str) -> dict[str, str]:
    # Pull request runs supersede each other; every other run (each push to
    # main) has its own group, so a later merge never cancels or replaces the
    # push run that ship.py's require_ci_green needs for an earlier commit.
    return {
        "group": prefix + "-${{ github.event_name == 'pull_request' && "
        "format('pr-{0}', github.event.pull_request.number) || "
        "format('run-{0}', github.run_id) }}",
        "cancel-in-progress": "${{ github.event_name == 'pull_request' }}",
    }


# --------------------------------------------------------------------------
# The checks. Each takes the workflow sources by file name and raises
# AssertionError when its property does not hold.
# --------------------------------------------------------------------------


def assert_required_contexts(sources: dict[str, str]) -> None:
    docs = parse(sources)
    for context, (workflow, job_id) in REQUIRED_CONTEXTS.items():
        job = jobs(docs[workflow]).get(job_id)
        if job is None or job.get("name") != context:
            raise AssertionError(f"required check {context} is not reported by job {job_id} of {workflow}")
    shard = jobs(docs["e2e-tests.yml"]).get("e2e-shard")
    if shard is None or shard.get("name") != "e2e-shard-${{ matrix.shard }}":
        raise AssertionError("the E2E shards are not named e2e-shard-N")
    if shard.get("strategy", {}).get("matrix") != {"shard": E2E_SHARDS}:
        raise AssertionError("the E2E shard matrix differs")
    split = "--shard=${{ matrix.shard }}/" + str(len(E2E_SHARDS))
    if not any(split in line for item in shard.get("steps", []) for line in run_lines(item)):
        raise AssertionError("the E2E shards do not split the suite by the matrix")


def assert_context_names_unique(sources: dict[str, str]) -> None:
    designated = set(REQUIRED_CONTEXTS.values()) | {("e2e-tests.yml", "e2e-shard")}
    reserved = [*REQUIRED_CONTEXTS, *(f"e2e-shard-{shard}" for shard in E2E_SHARDS)]
    for workflow, doc in parse(sources).items():
        for job_id, job in jobs(doc).items():
            if (workflow, job_id) in designated:
                continue
            pattern = name_pattern(str(job.get("name", job_id)))
            clashes = [name for name in reserved if pattern.fullmatch(name)]
            if clashes:
                raise AssertionError(f"{workflow} job {job_id} can report the required check {clashes[0]}")


def assert_triggers_and_pins(sources: dict[str, str]) -> None:
    docs = parse(sources)
    for workflow in CI_PREFIXES:
        if docs[workflow].get("on") != CI_TRIGGERS:
            raise AssertionError(f"{workflow} does not run on every push to and pull request for main")
    for workflow, doc in docs.items():
        on = doc.get("on")
        events = set(on) if type(on) in (dict, list) else {on}
        active = "\n".join(line for line in sources[workflow].splitlines() if not line.lstrip().startswith("#"))
        for event in FORBIDDEN_TRIGGERS:
            if event in events or re.search(rf"\b{event}\b", active):
                raise AssertionError(f"{workflow} uses {event}")
        uses = [job["uses"] for job in jobs(doc).values() if "uses" in job]
        uses += [item["uses"] for job in jobs(doc).values() for item in job.get("steps", []) if "uses" in item]
        if len(uses) != len(re.findall(r"(?m)^[ \t]*(?:-[ \t]+)?uses[ \t]*:", sources[workflow])):
            raise AssertionError(f"{workflow} has a uses: the reader did not see")
        for value in uses:
            if not PINNED_USES.fullmatch(str(value)):
                raise AssertionError(f"{workflow} uses {value} without a full commit pin")
        for job_id, job in jobs(doc).items():
            images = [service if type(service) is str else service.get("image")
                      for service in job.get("services", {}).values()]
            if "container" in job:
                images.append(job["container"] if type(job["container"]) is str else job["container"].get("image"))
            for image in images:
                if not DIGEST_IMAGE.fullmatch(str(image)) or (str(image).startswith("postgres") and image != PG17_IMAGE):
                    raise AssertionError(f"{workflow} job {job_id} runs an unpinned or unreviewed image {image}")
            for item in job.get("steps", []):
                if (str(item.get("uses", "")).startswith("actions/checkout@")
                        and item.get("with", {}).get("persist-credentials") is not False):
                    raise AssertionError(f"{workflow} job {job_id} persists checkout credentials")
    for workflow, job_id in POSTGRES_JOBS:
        if jobs(docs[workflow])[job_id].get("services", {}).get("postgres", {}).get("image") != PG17_IMAGE:
            raise AssertionError(f"{workflow} job {job_id} does not test against the reviewed PostgreSQL 17")


def assert_concurrency_and_timeouts(sources: dict[str, str]) -> None:
    docs = parse(sources)
    for workflow, prefix in CI_PREFIXES.items():
        if docs[workflow].get("concurrency") != expected_concurrency(prefix):
            raise AssertionError(f"{workflow} lets main push runs cancel each other")
        for job_id, job in jobs(docs[workflow]).items():
            if "concurrency" in job:
                raise AssertionError(f"{workflow} job {job_id} has its own concurrency group")
    test_jobs = jobs(docs["test.yml"])
    tenant = test_jobs["tenant-isolation"]
    if tenant.get("timeout-minutes") != 60:
        raise AssertionError("tenant-isolation has no explicit 60-minute job timeout")
    if [line for item in tenant.get("steps", []) for line in run_lines(item) if line.startswith("go test")] != [TENANT_TEST]:
        raise AssertionError("tenant-isolation does not run its tests with the 45-minute timeout")
    race = test_jobs["go-race"]
    # The per-package cap stays below the job's ceiling (GitHub's 360 minutes).
    if race.get("timeout-minutes") != 360 or RACE_TEST not in run_lines(step_named(race, "Run tests")):
        raise AssertionError("go-race does not run with the 150-minute package timeout under a 360-minute job")


def assert_environments_and_secrets(sources: dict[str, str]) -> None:
    docs = parse(sources)
    found: dict[tuple[str, str], Any] = {}
    for workflow, doc in docs.items():
        for job_id, job in jobs(doc).items():
            if "environment" in job:
                value = job["environment"]
                found[(workflow, job_id)] = value.get("name") if type(value) is dict else value
            if "secrets" in job and workflow not in SECRET_WORKFLOWS:
                raise AssertionError(f"{workflow} job {job_id} passes secrets")
        declared = len(re.findall(r"(?m)^[ \t]+environment[ \t]*:", sources[workflow]))
        if declared != sum(key[0] == workflow for key in found):
            raise AssertionError(f"{workflow} names an environment outside a job")
    if found != ENVIRONMENTS:
        raise AssertionError(f"the job environments differ: {sorted(found.items())}")
    for workflow, source in sources.items():
        if workflow in SECRET_WORKFLOWS:
            continue
        for expression in re.findall(r"\$\{\{(.*?)\}\}", source, flags=re.S):
            for match in re.finditer(r"\bsecrets\b(\.[A-Za-z_][A-Za-z0-9_]*)?", expression):
                if match.group(1) != ".GITHUB_TOKEN":
                    raise AssertionError(f"{workflow} reads a secret other than GITHUB_TOKEN")


def assert_workflow_lint(sources: dict[str, str]) -> None:
    lint = jobs(parse(sources)["test.yml"])["lint"]
    lines = [line for item in lint.get("steps", []) if "if" not in item for line in run_lines(item)]
    # With no arguments actionlint checks every file under .github/workflows.
    if ACTIONLINT_INSTALL not in lines or "actionlint" not in lines:
        raise AssertionError("the lint job does not run the pinned actionlint over every workflow")


def assert_release_tests(sources: dict[str, str]) -> None:
    test_jobs = jobs(parse(sources)["test.yml"])
    runners = [(job_id, item) for job_id, job in test_jobs.items() for item in job.get("steps", [])
               if any("unittest discover" in line for line in run_lines(item))]
    if len(runners) != 1:
        raise AssertionError(f"expected one step running the release tests, found {len(runners)}")
    job_id, item = runners[0]
    if job_id not in needs(test_jobs["test"]):
        raise AssertionError(f"the release tests job {job_id} is not in the test aggregator's needs")
    if "if" in item or "if" in test_jobs[job_id] or "working-directory" in item:
        raise AssertionError("the release tests are conditional or run elsewhere")
    if run_lines(item) != ["set -euo pipefail", *RELEASE_TEST_COMMANDS]:
        raise AssertionError("the release tests job does not run exactly the release test discovery")


def aggregator_script_requires_every_result(run: str, variable: str) -> bool:
    lines = run_lines({"run": run})
    if lines[:2] != ["set -euo pipefail", "python3 - <<'NEEDS'"] or lines[-1] != "NEEDS":
        return False
    script = heredoc(run, "NEEDS")

    def passes(value: str) -> bool:
        return run_python(script, env={variable: value}) == 0

    def results(*values: str) -> str:
        return json.dumps({f"job-{index}": {"result": value, "outputs": {}} for index, value in enumerate(values)})

    return (passes(results("success", "success"))
            and not any(passes(results("success", value)) for value in ("failure", "cancelled", "skipped"))
            and not passes("{}") and not passes("[]"))


def checked_results(job: dict[str, Any]) -> set[str]:
    """The needed jobs whose results some unconditional step of ``job`` requires to be success."""
    checked: set[str] = set()
    for item in job.get("steps", []):
        run = str(item.get("run", ""))
        if "if" in item or "||" in run:
            continue
        for variable, value in item.get("env", {}).items():
            if value == "${{ toJSON(needs) }}":
                if aggregator_script_requires_every_result(run, variable):
                    checked.update(needs(job))
                continue
            match = re.fullmatch(r"\$\{\{ needs\.([A-Za-z0-9_-]+)\.result \}\}", str(value))
            if match and re.search(r'"\$%s" ==? "success"' % re.escape(variable), run):
                checked.add(match.group(1))
    return checked


def assert_aggregators(sources: dict[str, str]) -> None:
    docs = parse(sources)
    for workflow in CI_PREFIXES:
        for job_id, job in jobs(docs[workflow]).items():
            # A tolerated job or step reports success to the aggregator.
            if "continue-on-error" in job or any("continue-on-error" in item for item in job.get("steps", [])):
                raise AssertionError(f"{workflow} job {job_id} tolerates failure")
    for workflow, aggregator in AGGREGATORS.items():
        workflow_jobs = jobs(docs[workflow])
        job = workflow_jobs[aggregator]
        others = sorted(job_id for job_id in workflow_jobs if job_id != aggregator)
        # Skipped because a needed job failed, a required check counts as passing.
        if job.get("if") not in ("always()", "${{ always() }}"):
            raise AssertionError(f"{workflow} {aggregator} does not run when a job it needs fails")
        if sorted(needs(job)) != others:
            raise AssertionError(f"{workflow} {aggregator} does not need exactly every other job")
        if sorted(checked_results(job)) != others:
            raise AssertionError(f"{workflow} {aggregator} does not require every other job to succeed")


def assert_unconditional_jobs(sources: dict[str, str]) -> None:
    docs = parse(sources)
    for workflow, aggregator in AGGREGATORS.items():
        for job_id, job in jobs(docs[workflow]).items():
            # A skipped job's own check counts as passing in branch protection.
            if job_id != aggregator and "if" in job:
                raise AssertionError(f"{workflow} job {job_id} is conditional")


def text_values(mapping: dict[str, Any]) -> dict[str, str]:
    return {key: str(value) for key, value in mapping.items()}


def assert_lint_build_and_scans(sources: dict[str, str]) -> None:
    test_jobs = jobs(parse(sources)["test.yml"])
    linters = [item for item in test_jobs["lint"].get("steps", [])
               if str(item.get("uses", "")).startswith("golangci/golangci-lint-action@")]
    if len(linters) != 1 or "if" in linters[0] or text_values(linters[0].get("with", {})) != GOLANGCI_LINT:
        raise AssertionError("the lint job does not run the pinned golangci-lint")
    if GO_BUILD not in [line for item in test_jobs["build"].get("steps", []) if "if" not in item
                        for line in run_lines(item)]:
        raise AssertionError("the build job does not build every package")
    security = test_jobs["security"].get("steps", [])
    lines = [line for item in security if "if" not in item for line in run_lines(item)]
    if any(command not in lines for command in GOVULNCHECK):
        raise AssertionError("the security job does not run govulncheck over every package")
    built = {}
    for line in lines:
        match = re.fullmatch(r"docker build -f (\S+) -t (\S+) \.", line)
        if match:
            built[match.group(2)] = match.group(1)
    rejections = [index for index, item in enumerate(security)
                  if "if" not in item and run_lines(item) == AMBIENT_TRIVY_POLICY]
    scans = [index for index, item in enumerate(security)
             if str(item.get("uses", "")).startswith("aquasecurity/trivy-action@")]
    # Nothing runs between the rejection and the scans that could add a policy file.
    if len(rejections) != 1 or scans != list(range(rejections[0] + 1, rejections[0] + 1 + len(scans))):
        raise AssertionError("the security job does not reject ambient Trivy policy right before its scans")
    scanned = {}
    for index in scans:
        settings = text_values(security[index].get("with", {}))
        image = settings.pop("image-ref", "")
        if "if" in security[index] or settings != TRIVY_SCAN:
            raise AssertionError(f"the scan of {image} does not fail on every CRITICAL or HIGH finding")
        scanned[image] = built.get(image)
    if scanned != SCANNED_IMAGES:
        raise AssertionError(f"the security job does not scan exactly the images it builds: {sorted(scanned.items())}")


AUDIT_LOCK = {"packages": {
    "": {"name": "fixture"},
    "node_modules/runtime-lib": {"version": "1.0.0"},
    "node_modules/build-tool": {"version": "1.0.0", "dev": True},
}}


def audit_report(**vulnerabilities: dict) -> dict:
    return {"auditReportVersion": 2, "vulnerabilities": vulnerabilities, "metadata": {"vulnerabilities": {}}}


def advisory(package: str, severity: str) -> dict:
    return {"name": package, "severity": severity, "via": [{
        "name": package, "title": "synthetic advisory", "severity": severity}]}


def braces_report(title: str = "braces vulnerable to stack-exhaustion denial of service through deeply "
                  "nested patterns", severity: str = "high") -> dict:
    return audit_report(braces={"name": "braces", "severity": severity, "via": [{
        "name": "braces", "title": title, "severity": severity}]},
        micromatch={"name": "micromatch", "severity": severity, "via": ["braces"]})


def braces_lock(dev: bool) -> dict:
    row = {"version": "3.0.3", "dev": True} if dev else {"version": "3.0.3"}
    return {"packages": {**AUDIT_LOCK["packages"], "node_modules/braces": row}}


def audit_run(sources: dict[str, str]) -> str:
    return step_named(jobs(parse(sources)["test.yml"])["security"], "Audit frontend dependencies")["run"]


def run_audit_policy(run: str, report: object, rc: int, lock: dict) -> int:
    """Run the step's embedded audit policy against a synthetic npm audit report."""
    return run_python(heredoc(run, "AUDIT"), "report.json", str(rc), files={
        "package-lock.json": json.dumps(lock),
        "report.json": report if type(report) is str else json.dumps(report),
    })


def assert_frontend_audit_policy(sources: dict[str, str]) -> None:
    run = audit_run(sources)
    if "|| audit_rc=$?" not in run or "|| true" in run:
        raise AssertionError("the frontend audit's exit status is not captured fail-closed")
    if re.findall(r"(?m)^ALLOWED = (.*)$", heredoc(run, "AUDIT")) != ['{("%s", "%s", "%s")}' % BRACES_EXCEPTION]:
        raise AssertionError("the audit exceptions differ from the reviewed braces exception")
    if dt.date.fromisoformat(BRACES_EXCEPTION[2]) < dt.date.today():
        raise AssertionError(f"the braces exception lapsed on {BRACES_EXCEPTION[2]}; review it")
    if run_audit_policy(run, braces_report(), 1, braces_lock(True)) != 0:
        raise AssertionError("the dev-only braces advisory is no longer allowed")
    if run_audit_policy(run, braces_report(), 1, braces_lock(False)) == 0:
        raise AssertionError("a braces advisory that reaches production is allowed")


# The schema guard job: a 10-minute job of exactly two steps, a full-history
# checkout of the commit under test (GitHub's merge commit on a pull request)
# and the embedded script, in the shell's default directory and environment.
SCHEMA_GUARD_TIMEOUT = 10
SCHEMA_GUARD_CHECKOUT = {"fetch-depth": 0, "persist-credentials": False}
SCHEMA_GUARD_OPENER = ["set -euo pipefail", "python3 -B - <<'GUARD'"]
SCHEMA_GUARD_MESSAGE = "guarded database code changed; it cannot be promoted in Part A; split or revert"
GUARD_HANDLERS = "package handlers\n\nfunc send() {}\n"
GUARD_MODELS = {"internal/models/models.go": "package models\n\ntype Note struct{}\n"}
GUARD_REPLY = {"internal/handlers/messages.go": GUARD_HANDLERS + "\nfunc reply() {}\n"}
# Each scenario names the commit checked out, where origin/main points
# ("head": that same commit, as on a push run; None: missing), the event, and
# whether the guard must refuse. Checking out GitHub's merge commit, a pull
# request is judged against the main commit it was merged onto. The first
# seven are the ones a broken script most often gets wrong, so a mutant fails
# after a few runs.
GUARD_SCENARIOS: dict[str, tuple[str, str | None, str | None, bool]] = {
    "pull request editing internal/models": ("models", "main", "pull_request", True),
    "pull request changing handlers only": ("handlers", "main", "pull_request", False),
    "push to main merging an internal/models edit": ("push-models", "head", "push", True),
    "workflow_dispatch run": ("handlers", "main", "workflow_dispatch", True),
    "pull request without origin/main": ("handlers", None, "pull_request", True),
    "pull request behind a main that moved on with a guarded change": ("handlers", "main-moved", "pull_request", False),
    "pull request whose guarded commit is not its last": ("models-then-handlers", "main", "pull_request", True),
    "pull request beside a tag named origin/main at its head": ("models", "main", "pull_request", True),
    "pull request adding an AutoMigrate call to internal/handlers": ("auto-migrate", "main", "pull_request", True),
    "pull request changing only a test under internal/database": ("database-test", "main", "pull_request", False),
    "pull request changing only release/staging": ("staging", "main", "pull_request", False),
    "pull request equal to main": ("main", "main", "pull_request", False),
    "pull request adding a non-ASCII path under internal/models": ("non-ascii", "main", "pull_request", True),
    "clean merge commit onto a main that moved on": ("merge-handlers", "main-moved", "pull_request", False),
    "guarded merge commit onto a main that moved on": ("merge-models", "main-moved", "pull_request", True),
    "push to main merging handlers only": ("push-handlers", "head", "push", False),
    "push of a commit without a parent": ("root", "head", "push", True),
    "run without an event name": ("handlers", "main", None, True),
}
# Scenarios whose checkout also has a tag named origin/main, at this commit:
# the short name would resolve to the tag instead of the remote branch.
GUARD_TAGS = {"pull request beside a tag named origin/main at its head": "models"}


class GuardFixture:
    """A synthetic repository with this directory's release modules and the
    commits the schema guard scenarios check out: pull request branches off
    main, GitHub-style merge commits, a main that moved on with a guarded
    change before the guard existed, and a root commit."""

    def __init__(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        atexit.register(self.directory.cleanup)
        root = Path(self.directory.name)
        self.summary = root / "step-summary.md"
        self.repo = repo = TempRepo(root / "repo")
        modules = {f"release/deployment/{path.name}": path.read_text(encoding="utf-8")
                   for path in sorted(HERE.glob("*.py")) if not path.name.startswith("test_")}
        main = repo.commit({
            **modules,
            "internal/database/postgres.go": "package database\n",
            "internal/models/models.go": "package models\n",
            "internal/handlers/messages.go": GUARD_HANDLERS,
            "README.md": "readme\n",
        }, "main")

        def branch(*changes: dict[str, str]) -> str:
            repo.checkout(main)
            for files in changes:
                repo.commit(files, "pull request")
            return repo.head()

        handlers = branch(GUARD_REPLY)
        models = branch(GUARD_MODELS)
        moved = branch({"internal/database/postgres.go": "package database\n\n// landed before the guard\n"})
        self.commits = {
            "main": main,
            "handlers": handlers,
            "models": models,
            "auto-migrate": branch({"internal/handlers/messages.go":
                                    GUARD_HANDLERS + "\nfunc up() { db.AutoMigrate(&models.Note{}) }\n"}),
            "database-test": branch({"internal/database/postgres_test.go": "package database\n"}),
            "staging": branch({"release/staging/bootstrap/main.go": "package main\n\nfunc main() { db.AutoMigrate(&Note{}) }\n"}),
            "models-then-handlers": branch(GUARD_MODELS, GUARD_REPLY),
            "non-ascii": branch({"internal/models/caf\u00e9.go": "package models\n"}),
            "main-moved": moved,
            "merge-handlers": self.merge(moved, handlers),
            "merge-models": self.merge(moved, models),
            "push-handlers": self.merge(main, handlers),
            "push-models": self.merge(main, models),
            "root": repo.git("commit-tree", "-m", "root", main + "^{tree}").strip(),
        }

    def merge(self, onto: str, branch: str) -> str:
        """A merge commit whose first parent is ``onto``, as GitHub makes."""
        self.repo.checkout(onto)
        self.repo.git("merge", "--quiet", "--no-ff", "-m", "merge", branch)
        return self.repo.head()

    def point(self, ref: str, sha: str | None) -> None:
        if sha is not None:
            self.repo.git("update-ref", ref, sha)
        elif self.repo.git("for-each-ref", ref).strip():
            self.repo.git("update-ref", "-d", ref)

    def run(self, script: str, head: str, main: str | None, event: str | None,
            tag: str | None = None) -> tuple[int, str, str]:
        """Run the step's script the way the step does, from the checkout."""
        sha = self.commits[head]
        self.repo.checkout(sha)
        self.point("refs/remotes/origin/main", None if main is None else sha if main == "head" else self.commits[main])
        self.point("refs/tags/origin/main", None if tag is None else self.commits[tag])
        self.summary.write_text("", encoding="utf-8")
        # The release tests themselves run in Actions: none of that run's
        # GITHUB_* variables (its event, its step summary) may leak in.
        env = {key: value for key, value in os.environ.items() if not key.upper().startswith("GITHUB_")}
        env["GITHUB_STEP_SUMMARY"] = str(self.summary)
        if event is not None:
            env["GITHUB_EVENT_NAME"] = event
        result = subprocess.run(
            [sys.executable, "-B", "-I", "-"], input=script, cwd=self.repo.root, env=env,
            capture_output=True, text=True, timeout=120,
        )
        return result.returncode, result.stdout, self.summary.read_text(encoding="utf-8")


@functools.lru_cache(maxsize=None)
def guard_fixture() -> GuardFixture:
    return GuardFixture()


@functools.lru_cache(maxsize=None)
def run_guard(script: str, scenario: str) -> tuple[int, str, str]:
    head, main, event, _refused = GUARD_SCENARIOS[scenario]
    return guard_fixture().run(script, head, main, event, GUARD_TAGS.get(scenario))


def schema_guard_script(sources: dict[str, str]) -> str:
    docs = parse(sources)
    steps = [(workflow, job_id, item) for workflow, doc in docs.items()
             for job_id, job in jobs(doc).items() for item in job.get("steps", [])
             if "schema_change" in str(item.get("run", ""))]
    if len(steps) != 1 or steps[0][0] != "test.yml":
        raise AssertionError(f"expected one Test step running the schema guard, found {len(steps)}")
    _workflow, job_id, item = steps[0]
    test_jobs = jobs(docs["test.yml"])
    job = test_jobs[job_id]
    if job_id not in needs(test_jobs["test"]):
        raise AssertionError(f"the schema guard job {job_id} is not in the test aggregator's needs")
    if "if" in job or job.get("timeout-minutes") != SCHEMA_GUARD_TIMEOUT:
        raise AssertionError("the schema guard job is conditional or has no 10-minute timeout")
    steps_run = job.get("steps", [])
    checkout = steps_run[0] if steps_run else {}
    # Anything between the two (a git checkout, a reset, a fetch) or another
    # ref changes which commits the guard compares.
    if (len(steps_run) != 2 or steps_run[1] is not item
            or not str(checkout.get("uses", "")).startswith("actions/checkout@")
            or checkout.get("with") != SCHEMA_GUARD_CHECKOUT or "if" in checkout):
        raise AssertionError("the schema guard does not run right after a full-history checkout of the commit under test")
    if any(key in item for key in ("if", "working-directory", "shell", "env", "continue-on-error")):
        raise AssertionError("the schema guard step is conditional or runs elsewhere or with another environment")
    lines = run_lines(item)
    if lines[:2] != SCHEMA_GUARD_OPENER or lines[-1] != "GUARD":
        raise AssertionError("the schema guard step does not run exactly its embedded script")
    return heredoc(str(item["run"]), "GUARD")


def assert_schema_guard(sources: dict[str, str]) -> None:
    script = schema_guard_script(sources)
    for scenario, (_head, _main, _event, refused) in GUARD_SCENARIOS.items():
        if (run_guard(script, scenario)[0] != 0) != refused:
            raise AssertionError(f"the schema guard {'passes' if refused else 'refuses'} a {scenario}")


# The staging bootstrap job (Part A PR 4): its steps in order, each with the
# lines it must run in this order. The lines pin what the proof is (the
# DigitalOcean role shape, two verify-only rls-migrate runs compared with the
# bootstrap snapshot, the refusals and their exit status), not how the rest of
# each script is written.
STAGING_BOOTSTRAP_JOB = "staging-bootstrap"
STAGING_BOOTSTRAP_TIMEOUT = 20
STAGING_BOOTSTRAP_SETUP = ("actions/checkout@", "actions/setup-go@")
RLS_MIGRATE = '"$dir/rereply" rls-migrate -config "$dir/migrate.toml"'
# Any failure after the server starts prints its log.
SERVER_LOG_TRAP = ("trap 'status=$?; kill \"$server\" 2>/dev/null || true; "
                   "if [[ \"$status\" -ne 0 ]]; then cat \"$dir/server.log\"; fi' EXIT")
STAGING_BOOTSTRAP_STEPS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("Create the DigitalOcean role shape", (
        "set -euo pipefail",
        "umask 077",
        "CREATE ROLE doadmin LOGIN PASSWORD '$owner_password' NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS;",
        "CREATE DATABASE rereply OWNER doadmin;",
        "SET ROLE doadmin;",
        "CREATE ROLE rereply_app LOGIN PASSWORD '$runtime_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;",
        "CREATE ROLE rereply_owner LOGIN PASSWORD '$negative_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;",
        "RESET ROLE;",
        "CREATE DATABASE rereply_negative OWNER rereply_owner;",
        'test "$memberships" = "doadmin>rereply_app:t,doadmin>rereply_owner:t"',
        'test "$owners" = "rereply:doadmin,rereply_negative:rereply_owner"',
        'environment = "test"',
        "rls_enabled = true",
    )),
    ("Build the bootstrap tool and the server", (
        "set -euo pipefail",
        'go build -mod=readonly -o "$RUNNER_TEMP/staging-bootstrap/bootstrap" ./release/staging/bootstrap',
        'go build -mod=readonly -o "$RUNNER_TEMP/staging-bootstrap/rereply" ./cmd/whatomate',
    )),
    ("Bootstrap the staging database", (
        "set -euo pipefail",
        '"$RUNNER_TEMP/staging-bootstrap/bootstrap" -config "$RUNNER_TEMP/staging-bootstrap/migrate.toml"',
    )),
    ("Accept the database with rls-migrate twice and refuse a second bootstrap", (
        "set -euo pipefail",
        'cmp "$dir/schema-bootstrap.sql" "$dir/schema-$1.sql"',
        'cmp "$dir/data-bootstrap.sql" "$dir/data-$1.sql"',
        "snapshot bootstrap",
        RLS_MIGRATE,
        "snapshot rls-migrate-1",
        "unchanged rls-migrate-1",
        RLS_MIGRATE,
        "snapshot rls-migrate-2",
        "unchanged rls-migrate-2",
        "status=0",
        '"$dir/bootstrap" -config "$dir/migrate.toml" 2> "$dir/second.err" || status=$?',
        'test "$status" -eq 2',
        "grep -q '^staging-bootstrap: refusing: public schema is not empty ' \"$dir/second.err\"",
        "snapshot second-bootstrap",
        "unchanged second-bootstrap",
    )),
    ("Serve, log in and read the current user", (
        "set -euo pipefail",
        '"$dir/rereply" server -config "$dir/server.toml" > "$dir/server.log" 2>&1 &',
        SERVER_LOG_TRAP,
        "grep -q 'PostgreSQL tenant RLS verified' \"$dir/server.log\"",
        'test "$(code --cookie-jar "$dir/cookies" --header \'Content-Type: application/json\' --data "@$dir/login.json" '
        'http://127.0.0.1:8080/api/auth/login)" = 200',
        'test "$(code --cookie "$dir/cookies" http://127.0.0.1:8080/api/me)" = 200',
    )),
    ("Refuse an owner that the doadmin-like role created", (
        "set -euo pipefail",
        "status=0",
        '"$dir/bootstrap" -config "$dir/negative.toml" 2> "$dir/negative.err" || status=$?',
        'test "$status" -eq 2',
        "grep -qx 'staging-bootstrap: refusing: membership preflight: roles other than the migration owner are "
        "members of the owner=1 runtime=1' \"$dir/negative.err\"",
        'test "$objects" = 0',
    )),
)
# Ways a line can swallow a failure; "|| status=$?" is how a step captures an
# expected refusal, which a later test of $status then requires. EXIT trap
# lines are cleanup and exempt.
TOLERATED_FAILURE = re.compile(r"\|\|\s*(?:true\b|:|exit 0\b)|\bset \+[a-z]*[eo]\b")


def ordered_subsequence(lines: list[str], required: tuple[str, ...]) -> bool:
    position = 0
    for line in lines:
        if position < len(required) and line == required[position]:
            position += 1
    return position == len(required)


def assert_staging_bootstrap(sources: dict[str, str]) -> None:
    test_jobs = jobs(parse(sources)["test.yml"])
    job = test_jobs.get(STAGING_BOOTSTRAP_JOB)
    if job is None or job.get("name") != STAGING_BOOTSTRAP_JOB:
        raise AssertionError("the Test workflow has no staging-bootstrap job")
    if STAGING_BOOTSTRAP_JOB not in needs(test_jobs["test"]):
        raise AssertionError("the staging-bootstrap job is not in the test aggregator's needs")
    if job.get("timeout-minutes") != STAGING_BOOTSTRAP_TIMEOUT or job.get("permissions") != {"contents": "read"}:
        raise AssertionError("the staging-bootstrap job has no 20-minute timeout or more than read access")
    if sorted(job.get("services", {})) != ["postgres", "redis"]:
        raise AssertionError("the staging-bootstrap job does not run on its PostgreSQL and Redis services")
    steps = job.get("steps", [])
    setup = [str(item.get("uses", "")) for item in steps[:len(STAGING_BOOTSTRAP_SETUP)]]
    if len(setup) != len(STAGING_BOOTSTRAP_SETUP) or not all(
            value.startswith(prefix) for value, prefix in zip(setup, STAGING_BOOTSTRAP_SETUP)):
        raise AssertionError("the staging-bootstrap job does not start with checkout and Go setup")
    proof = steps[len(STAGING_BOOTSTRAP_SETUP):]
    if [item.get("name") for item in proof] != [name for name, _ in STAGING_BOOTSTRAP_STEPS]:
        raise AssertionError("the staging-bootstrap steps differ: %s" % [item.get("name") for item in proof])
    for item in steps:
        if any(key in item for key in ("if", "continue-on-error", "shell", "working-directory")):
            raise AssertionError(f"staging-bootstrap step {item.get('name')!r} is conditional or runs elsewhere")
    for item, (name, required) in zip(proof, STAGING_BOOTSTRAP_STEPS):
        lines = run_lines(item)
        if lines[:1] != ["set -euo pipefail"]:
            raise AssertionError(f"staging-bootstrap step {name!r} does not fail on the first error")
        if not ordered_subsequence(lines, required):
            raise AssertionError(f"staging-bootstrap step {name!r} does not run its proof in order")
        # An EXIT trap's cleanup may ignore a server that already stopped.
        tolerated = [line for line in lines if TOLERATED_FAILURE.search(line) and not line.startswith("trap ")]
        if tolerated:
            raise AssertionError(f"staging-bootstrap step {name!r} tolerates a failure: {tolerated[0]}")


# Staging code: the bootstrap tool, the graph stub and anything else under
# release/staging. In the workflows that build or test the release images,
# only the staging-bootstrap job may build or run it. Workflows that exist for
# staging code (the planned staging-images.yml and canary-local.yml) are not
# in this list.
STAGING_REFERENCE = re.compile(r"release/staging|graph-stub|graphstub")
STAGING_REFERENCE_WORKFLOWS = ("ship.yml", "test.yml", "e2e-tests.yml")
STAGING_REFERENCE_JOBS = {("test.yml", STAGING_BOOTSTRAP_JOB)}


def text_nodes(value: Any) -> list[str]:
    if type(value) is dict:
        return [text for key, child in value.items() for text in (str(key), *text_nodes(child))]
    if type(value) is list:
        return [text for child in value for text in text_nodes(child)]
    return [str(value)]


def release_dockerfiles() -> dict[str, str]:
    files = {}
    for path in sorted((ROOT / "docker").rglob("*")):
        relative = path.relative_to(ROOT).as_posix()
        if path.is_file() and "Dockerfile" in path.name and not relative.startswith("docker/staging/"):
            files[relative] = path.read_text(encoding="utf-8")
    return files


def assert_staging_placement(sources: dict[str, str], dockerfiles: dict[str, str] | None = None) -> None:
    docs = parse(sources)
    for workflow in STAGING_REFERENCE_WORKFLOWS:
        doc = dict(docs[workflow])
        doc["jobs"] = {job_id: job for job_id, job in jobs(doc).items()
                       if (workflow, job_id) not in STAGING_REFERENCE_JOBS}
        for job_id, job in doc["jobs"].items():
            if any(STAGING_REFERENCE.search(text) for text in text_nodes(job)):
                raise AssertionError(f"{workflow} job {job_id} references staging code")
        if any(STAGING_REFERENCE.search(text) for text in text_nodes({k: v for k, v in doc.items() if k != "jobs"})):
            raise AssertionError(f"{workflow} references staging code outside its jobs")
    files = release_dockerfiles() if dockerfiles is None else dockerfiles
    if not any(name.startswith("docker/release/") for name in files) or "docker/Dockerfile" not in files:
        raise AssertionError("the release Dockerfiles were not found")
    for name, text in files.items():
        if STAGING_REFERENCE.search(text):
            raise AssertionError(f"{name} references staging code")


CHECKS: tuple[tuple[str, Callable[[dict[str, str]], None]], ...] = (
    ("required-contexts", assert_required_contexts),
    ("unique-context-names", assert_context_names_unique),
    ("triggers-and-pins", assert_triggers_and_pins),
    ("concurrency-and-timeouts", assert_concurrency_and_timeouts),
    ("environments-and-secrets", assert_environments_and_secrets),
    ("workflow-lint", assert_workflow_lint),
    ("release-tests", assert_release_tests),
    ("aggregators", assert_aggregators),
    ("braces-audit", assert_frontend_audit_policy),
    ("unconditional-jobs", assert_unconditional_jobs),
    ("lint-build-and-scans", assert_lint_build_and_scans),
    ("staging-bootstrap", assert_staging_bootstrap),
    ("staging-placement", assert_staging_placement),
    # Last: it runs the guard script, so earlier checks fail a mutant first.
    ("schema-guard", assert_schema_guard),
)


def check_ci_workflows(sources: dict[str, str]) -> None:
    """Every check in order; the error names the first one that fails."""
    try:
        parse(sources)
    except (ValueError, KeyError, IndexError) as error:
        raise AssertionError(f"parse: {error}") from None
    for label, check in CHECKS:
        try:
            check(sources)
        except (AssertionError, KeyError, TypeError, AttributeError) as error:
            raise AssertionError(f"{label}: {error}") from None


def replaced(workflow: str, old: str, new: str) -> dict[str, str]:
    if SOURCES[workflow].count(old) != 1:
        raise AssertionError(f"mutation anchor is not unique in {workflow}: {old!r}")
    return {**SOURCES, workflow: SOURCES[workflow].replace(old, new, 1)}


def added(workflow: str, text: str) -> dict[str, str]:
    if workflow in SOURCES:
        raise AssertionError(f"{workflow} already exists")
    return {**SOURCES, workflow: text}


def extra_workflow(on: str, job: str) -> str:
    return f"""name: Extra

on: {on}

permissions:
  contents: read

jobs:
  {job}:
    runs-on: ubuntu-24.04
    steps:
      - run: "true"
"""

AGGREGATOR_STEP = "      - name: Require every Test job to succeed\n"
GMAIL_SCAN = ("      - name: Scan Gmail relay container\n"
              "        uses: aquasecurity/trivy-action@a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0\n"
              "        with:\n          image-ref: rereply-gmail-relay:ci\n          format: table\n"
              "          exit-code: \"1\"\n          vuln-type: os,library\n          severity: CRITICAL,HIGH\n")
RELEASE_TESTS_STEP = "      - name: Test the Release workflow, its modules and the CI workflows\n"
GUARD_STEP = "      - name: Refuse guarded database changes\n"
GUARD_JOB = "  schema-guard:\n    name: schema-guard\n"
GUARD_CHECKOUT = ("      contents: read\n    steps:\n      - name: Checkout repository\n"
                  "        uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4\n"
                  "        with:\n          fetch-depth: 0\n")
GUARD_PR_BASE = 'base = commit("merge-base", "refs/remotes/origin/main", head)'
GUARD_PUSH_BASE = 'base = commit("rev-parse", "--verify", head + "^1^{commit}")'


def without_schema_guard_job() -> dict[str, str]:
    source = SOURCES["test.yml"]
    start, end = source.find(GUARD_JOB), source.find("  tenant-isolation:\n")
    if source.count(GUARD_JOB) != 1 or not 0 <= start < end:
        raise AssertionError("the schema guard job is not where the mutation expects it")
    mutated = source[:start] + source[end:]
    if mutated.count("      - schema-guard\n") != 1:
        raise AssertionError("the aggregator does not need the schema guard once")
    return {**SOURCES, "test.yml": mutated.replace("      - schema-guard\n", "", 1)}


STAGING_JOB = "  staging-bootstrap:\n    name: staging-bootstrap\n"
STAGING_HEAD = ("    runs-on: ubuntu-24.04\n    timeout-minutes: 20\n    permissions:\n      contents: read\n\n"
                "    services:\n      postgres:\n")
NEGATIVE_STEP = "      - name: Refuse an owner that the doadmin-like role created\n"


def without_negative_case() -> dict[str, str]:
    source = SOURCES["test.yml"]
    start, end = source.find(NEGATIVE_STEP), source.find("  go-race:\n")
    if source.count(NEGATIVE_STEP) != 1 or not 0 <= start < end:
        raise AssertionError("the negative case is not where the mutation expects it")
    return {**SOURCES, "test.yml": source[:start] + source[end:]}


# Each case breaks one property; the checker must fail on the named check.
NEGATIVE_CASES: dict[str, tuple[str, Callable[[], dict[str, str]]]] = {
    # 1. Required check names.
    "rename a required job's name": ("required-contexts", lambda: replaced(
        "test.yml", "  lint:\n    name: lint\n", "  lint:\n    name: golangci-lint\n")),
    "rename a required job's id": ("required-contexts", lambda: replaced(
        "test.yml", "  build:\n    name: build\n", "  compile:\n    name: build\n")),
    "rename the e2e aggregator": ("required-contexts", lambda: replaced(
        "e2e-tests.yml", "  e2e:\n    name: e2e\n", "  e2e:\n    name: e2e-all\n")),
    "rename the e2e shards": ("required-contexts", lambda: replaced(
        "e2e-tests.yml", "name: e2e-shard-${{ matrix.shard }}", "name: shard-${{ matrix.shard }}")),
    "shrink the shard matrix": ("required-contexts", lambda: replaced(
        "e2e-tests.yml", "shard: [1, 2, 3, 4]", "shard: [1, 2, 3]")),
    # 2. No other job can report a required name.
    "name a new job test in a new workflow": ("unique-context-names", lambda: added(
        "extra.yml", extra_workflow("[push]", "test"))),
    "name a new job test in the e2e workflow": ("unique-context-names", lambda: replaced(
        "e2e-tests.yml", "  e2e:\n    name: e2e\n",
        "  smoke:\n    name: test\n    runs-on: ubuntu-latest\n    steps:\n      - run: \"true\"\n\n"
        "  e2e:\n    name: e2e\n")),
    "name a Release job lint": ("unique-context-names", lambda: replaced(
        "ship.yml", "    name: Release plan\n", "    name: lint\n")),
    "name a Release job by expression only": ("unique-context-names", lambda: replaced(
        "ship.yml", "    name: Release image (${{ matrix.component }})\n", "    name: ${{ matrix.component }}\n")),
    # 3. Triggers, pins and checkouts.
    "add pull_request_target to Test": ("triggers-and-pins", lambda: replaced(
        "test.yml", "  pull_request:\n    branches:\n      - main\n",
        "  pull_request:\n    branches:\n      - main\n  pull_request_target:\n    branches:\n      - main\n")),
    "add workflow_run to Release": ("triggers-and-pins", lambda: replaced(
        "ship.yml", "on:\n  workflow_dispatch:\n",
        "on:\n  workflow_run:\n    workflows: [Test]\n    types: [completed]\n  workflow_dispatch:\n")),
    "add pull_request_target to Release": ("triggers-and-pins", lambda: replaced(
        "ship.yml", "on:\n  workflow_dispatch:\n", "on:\n  pull_request_target:\n  workflow_dispatch:\n")),
    "add a workflow on pull_request_target": ("triggers-and-pins", lambda: added(
        "extra.yml", extra_workflow("[push, pull_request_target]", "extra"))),
    "add a workflow on workflow_run": ("triggers-and-pins", lambda: added(
        "extra.yml", extra_workflow("workflow_run", "extra"))),
    "add a path filter to Test": ("triggers-and-pins", lambda: replaced(
        "test.yml", "  pull_request:\n    branches:\n      - main\n",
        "  pull_request:\n    branches:\n      - main\n    paths:\n      - internal/**\n")),
    "add a path filter to E2E": ("triggers-and-pins", lambda: replaced(
        "e2e-tests.yml", "  push:\n    branches: [main]\n", "  push:\n    branches: [main]\n    paths-ignore: [docs/**]\n")),
    "unpin a uses in Test": ("triggers-and-pins", lambda: replaced(
        "test.yml", "golangci/golangci-lint-action@9fae48acfc02a90574d7c304a1758ef9895495fa # v7",
        "golangci/golangci-lint-action@v7")),
    "unpin a uses in E2E": ("triggers-and-pins", lambda: replaced(
        "e2e-tests.yml", "- uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4",
        "- uses: actions/checkout@main")),
    "unpin a uses in Release": ("triggers-and-pins", lambda: replaced(
        "ship.yml", "docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8 # v6",
        "docker/build-push-action@v6")),
    "unpin a service image": ("triggers-and-pins", lambda: replaced(
        "e2e-tests.yml", "redis:7@sha256:91d0f7e8c748ec7a4c2b4fb2c4f84edab794dd91d01e095e38dc906db9d684ab", "redis:7")),
    "test against another PostgreSQL": ("triggers-and-pins", lambda: replaced(
        "e2e-tests.yml", PG17_IMAGE, "postgres:16@sha256:" + "0" * 64)),
    "persist checkout credentials": ("triggers-and-pins", lambda: replaced(
        "e2e-tests.yml", "          persist-credentials: false\n", "          persist-credentials: true\n")),
    # 4. Concurrency and timeouts.
    "revert the Test concurrency group": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "format('run-{0}', github.run_id)", "github.ref")),
    "revert the E2E concurrency group": ("concurrency-and-timeouts", lambda: replaced(
        "e2e-tests.yml", "format('run-{0}', github.run_id)", "github.ref")),
    "cancel Test main runs in progress": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n", "  cancel-in-progress: true\n")),
    "cancel E2E main runs in progress": ("concurrency-and-timeouts", lambda: replaced(
        "e2e-tests.yml", "  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n",
        "  cancel-in-progress: true\n")),
    "add a job concurrency group": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "  build:\n    name: build\n",
        "  build:\n    name: build\n    concurrency:\n      group: build\n      cancel-in-progress: true\n")),
    "drop the tenant-isolation job timeout": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "    name: tenant-isolation\n    runs-on: ubuntu-24.04\n    timeout-minutes: 60\n",
        "    name: tenant-isolation\n    runs-on: ubuntu-24.04\n")),
    "drop the tenant-isolation test timeout": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "go test -mod=readonly -v -timeout 45m ./internal/database", "go test -mod=readonly -v ./internal/database")),
    "shorten the go-race package timeout": ("concurrency-and-timeouts", lambda: replaced(
        "test.yml", "-race -p 1 -timeout 150m ", "-race -p 1 -timeout 60m ")),
    # 5. Environments and secrets.
    "add an environment outside ship.yml": ("environments-and-secrets", lambda: replaced(
        "test.yml", "  lint:\n    name: lint\n", "  lint:\n    name: lint\n    environment: production\n")),
    "add a secret outside ship.yml": ("environments-and-secrets", lambda: replaced(
        "e2e-tests.yml", "          CI: true\n", "          CI: true\n          META_TOKEN: ${{ secrets.META_TOKEN }}\n")),
    "read every secret outside ship.yml": ("environments-and-secrets", lambda: replaced(
        "e2e-tests.yml", "          CI: true\n", "          CI: true\n          ALL: ${{ toJSON(secrets) }}\n")),
    "inherit secrets outside ship.yml": ("environments-and-secrets", lambda: replaced(
        "test.yml", "  lint:\n    name: lint\n", "  lint:\n    name: lint\n    secrets: inherit\n")),
    "map the production job to the wrong environment": ("environments-and-secrets", lambda: replaced(
        "ship.yml", "    environment: production\n", "    environment: staging\n")),
    "map the production job to the wrong environment by object": ("environments-and-secrets", lambda: replaced(
        "ship.yml", "    environment: production\n", "    environment:\n      name: staging\n")),
    "give another Release job an environment": ("environments-and-secrets", lambda: replaced(
        "ship.yml", "    name: Release record\n", "    name: Release record\n    environment: production\n")),
    # 6. actionlint.
    "lint only one workflow": ("workflow-lint", lambda: replaced(
        "test.yml", "          actionlint\n", "          actionlint .github/workflows/test.yml\n")),
    # 7. The release tests.
    "remove the release/deployment discover command": ("release-tests", lambda: replaced(
        "test.yml", "          python3 -B -m unittest discover -s release/deployment -p 'test_*.py' -v\n", "")),
    "retarget the release tests": ("release-tests", lambda: replaced(
        "test.yml", "discover -s release/deployment -p", "discover -s release/canary -p")),
    "make the release tests conditional": ("release-tests", lambda: replaced(
        "test.yml", RELEASE_TESTS_STEP, RELEASE_TESTS_STEP + "        if: ${{ github.event_name == 'push' }}\n")),
    "drop the release tests from the aggregator": ("release-tests", lambda: replaced(
        "test.yml", "      - release-tests\n", "")),
    # 8. Aggregators.
    "drop a job from the test aggregator": ("aggregators", lambda: replaced("test.yml", "      - go-race\n", "")),
    "add a job the e2e aggregator does not need": ("aggregators", lambda: replaced(
        "e2e-tests.yml", "  e2e:\n    name: e2e\n",
        "  e2e-lint:\n    name: e2e-lint\n    runs-on: ubuntu-latest\n    steps:\n      - run: \"true\"\n\n"
        "  e2e:\n    name: e2e\n")),
    "skip the test aggregator when a job fails": ("aggregators", lambda: replaced(
        "test.yml", "    if: ${{ always() }}\n", "")),
    "skip the e2e aggregator when a shard fails": ("aggregators", lambda: replaced(
        "e2e-tests.yml", "  e2e:\n    name: e2e\n    if: always()\n", "  e2e:\n    name: e2e\n")),
    "make the aggregator check conditional": ("aggregators", lambda: replaced(
        "test.yml", AGGREGATOR_STEP, AGGREGATOR_STEP + "        if: ${{ false }}\n")),
    "ignore cancelled and skipped jobs": ("aggregators", lambda: replaced(
        "test.yml", '              if result != "success":\n', '              if result == "failure":\n')),
    "pass with no job results": ("aggregators", lambda: replaced(
        "test.yml", "          if type(needs) is not dict or not needs:\n", "          if type(needs) is not dict:\n")),
    "tolerate a failed e2e shard": ("aggregators", lambda: replaced(
        "e2e-tests.yml", 'run: test "$SHARD_RESULT" = "success"', 'run: test "$SHARD_RESULT" = "success" || true')),
    "tolerate a failed job": ("aggregators", lambda: replaced(
        "test.yml", "  build:\n    name: build\n    runs-on: ubuntu-24.04\n",
        "  build:\n    name: build\n    runs-on: ubuntu-24.04\n    continue-on-error: true\n")),
    # 9. The braces exception.
    "expire the braces date": ("braces-audit", lambda: replaced("test.yml", '"2026-12-31")}', '"2000-01-01")}')),
    "add a second audit exception": ("braces-audit", lambda: replaced(
        "test.yml", '"2026-12-31")}', '"2026-12-31"), ("esbuild", "request forgery", "2026-12-31")}')),
    "tolerate a failed npm audit": ("braces-audit", lambda: replaced(
        "test.yml", "|| audit_rc=$?", "|| true")),
    # 10. Unconditional jobs.
    "run security only on push": ("unconditional-jobs", lambda: replaced(
        "test.yml", "  security:\n    name: security\n",
        "  security:\n    name: security\n    if: github.event_name == 'push'\n")),
    "skip tenant-isolation on pull requests": ("unconditional-jobs", lambda: replaced(
        "test.yml", "  tenant-isolation:\n    name: tenant-isolation\n",
        "  tenant-isolation:\n    name: tenant-isolation\n    if: ${{ github.event_name != 'pull_request' }}\n")),
    "run the e2e shards only on push": ("unconditional-jobs", lambda: replaced(
        "e2e-tests.yml", "    name: e2e-shard-${{ matrix.shard }}\n",
        "    name: e2e-shard-${{ matrix.shard }}\n    if: github.event_name == 'push'\n")),
    # 11. Lint, build and the security scans.
    "drop golangci-lint": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "      - name: Run golangci-lint\n"
        "        uses: golangci/golangci-lint-action@9fae48acfc02a90574d7c304a1758ef9895495fa # v7\n"
        "        with:\n          version: v2.11.4\n\n", "")),
    "change the golangci-lint version": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "          version: v2.11.4\n", "          version: latest\n")),
    "replace go build with true": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "        run: go build -mod=readonly -v ./...\n", '        run: "true"\n')),
    "drop govulncheck": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "          GOFLAGS=-mod=readonly govulncheck ./...\n", "")),
    "tolerate govulncheck findings": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "govulncheck ./...\n", "govulncheck ./... || true\n")),
    "accept ambient Trivy policy": ("lint-build-and-scans", lambda: replaced(
        "test.yml", '            [[ ! -e "$path" && ! -L "$path" ]]\n', "            true\n")),
    "write a Trivy policy after the rejection": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "      - name: Scan production container\n",
        "      - name: Write policy\n        run: touch trivy.yaml\n\n      - name: Scan production container\n")),
    "let a scan pass on findings": ("lint-build-and-scans", lambda: replaced(
        "test.yml", GMAIL_SCAN, GMAIL_SCAN.replace('exit-code: "1"', 'exit-code: "0"'))),
    "scan for CRITICAL only": ("lint-build-and-scans", lambda: replaced(
        "test.yml", GMAIL_SCAN, GMAIL_SCAN.replace("severity: CRITICAL,HIGH", "severity: CRITICAL"))),
    "give a scan an exception file": ("lint-build-and-scans", lambda: replaced(
        "test.yml", GMAIL_SCAN, GMAIL_SCAN + "          trivyignores: release/deployment/ship.trivyignore\n")),
    "make a scan conditional": ("lint-build-and-scans", lambda: replaced(
        "test.yml", GMAIL_SCAN, GMAIL_SCAN.replace("        uses: ", "        if: ${{ false }}\n        uses: "))),
    "drop a scan": ("lint-build-and-scans", lambda: replaced("test.yml", GMAIL_SCAN, "")),
    "scan an image built from another Dockerfile": ("lint-build-and-scans", lambda: replaced(
        "test.yml", "docker build -f docker/gmail-relay.Dockerfile -t rereply-gmail-relay:ci .",
        "docker build -f docker/meta-relay.Dockerfile -t rereply-gmail-relay:ci .")),
    # 12. The schema guard. Dropping or skipping its job trips the generic
    # aggregator and unconditional-job checks first.
    "drop the schema guard job and its need": ("schema-guard", without_schema_guard_job),
    "drop the schema guard from the aggregator": ("aggregators", lambda: replaced(
        "test.yml", "      - schema-guard\n", "")),
    "run the schema guard only on pull requests": ("unconditional-jobs", lambda: replaced(
        "test.yml", GUARD_JOB, GUARD_JOB + "    if: github.event_name == 'pull_request'\n")),
    "make the schema guard step conditional": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_STEP, GUARD_STEP + "        if: ${{ github.event_name == 'pull_request' }}\n")),
    "tolerate a refusing schema guard": ("schema-guard", lambda: replaced(
        "test.yml", "python3 -B - <<'GUARD'\n", "python3 -B - <<'GUARD' || true\n")),
    "drop the schema guard timeout": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_JOB + "    runs-on: ubuntu-24.04\n    timeout-minutes: 10\n",
        GUARD_JOB + "    runs-on: ubuntu-24.04\n")),
    "check out a shallow clone for the schema guard": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_CHECKOUT, GUARD_CHECKOUT.replace("          fetch-depth: 0\n", ""))),
    "check out the pull request base for the schema guard": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_CHECKOUT, GUARD_CHECKOUT + "          ref: ${{ github.event.pull_request.base.sha }}\n")),
    "move HEAD before the schema guard": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_STEP,
        "      - name: Compare with main\n        run: git checkout --quiet origin/main\n\n" + GUARD_STEP)),
    "give the schema guard step an environment": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_STEP, GUARD_STEP + "        env:\n          PYTHONPATH: release\n")),
    "run the schema guard in another directory": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_STEP, GUARD_STEP + "        working-directory: release\n")),
    "compare a pull request with main's tip": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_PR_BASE, 'base = commit("rev-parse", "--verify", "refs/remotes/origin/main^{commit}")')),
    "compare a pull request with whatever origin/main names": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_PR_BASE, 'base = commit("merge-base", "origin/main", head)')),
    "compare a pull request with its first parent": ("schema-guard", lambda: replaced(
        "test.yml", GUARD_PR_BASE, GUARD_PUSH_BASE)),
    "compare a push with itself": ("schema-guard", lambda: replaced("test.yml", GUARD_PUSH_BASE, "base = head")),
    "check a push like a pull request": ("schema-guard", lambda: replaced(
        "test.yml", 'if event == "pull_request":', 'if event in ("pull_request", "push"):')),
    "skip the check": ("schema-guard", lambda: replaced(
        "test.yml", "reasons = schema_change.check(REPO, base, head)", "reasons = ()")),
    "pass a refused change": ("schema-guard", lambda: replaced(
        "test.yml", "          sys.exit(1)\n          GUARD\n", "          sys.exit(0)\n          GUARD\n")),
    "pass an unknown event": ("schema-guard", lambda: replaced(
        "test.yml", 'common.fail("context-invalid:event")', "sys.exit(0)")),
    "pass when git fails": ("schema-guard", lambda: replaced(
        "test.yml", '"schema guard failed closed: %s" % error)\n              sys.exit(1)\n',
        '"schema guard failed closed: %s" % error)\n              sys.exit(0)\n')),
    # 13. The staging bootstrap proof. Dropping the job or making it
    # conditional trips the generic aggregator and unconditional-job checks.
    "drop the staging bootstrap from the aggregator": ("aggregators", lambda: replaced(
        "test.yml", "      - staging-bootstrap\n", "")),
    "run the staging bootstrap only on push": ("unconditional-jobs", lambda: replaced(
        "test.yml", STAGING_JOB, STAGING_JOB + "    if: github.event_name == 'push'\n")),
    "prove the staging bootstrap on another PostgreSQL": ("triggers-and-pins", lambda: replaced(
        "test.yml", STAGING_JOB + STAGING_HEAD + "        image: " + PG17_IMAGE,
        STAGING_JOB + STAGING_HEAD + "        image: postgres:16@sha256:" + "0" * 64)),
    "drop the staging bootstrap timeout": ("staging-bootstrap", lambda: replaced(
        "test.yml", STAGING_JOB + "    runs-on: ubuntu-24.04\n    timeout-minutes: 20\n",
        STAGING_JOB + "    runs-on: ubuntu-24.04\n")),
    "give the staging bootstrap write access": ("staging-bootstrap", lambda: replaced(
        "test.yml", STAGING_JOB + "    runs-on: ubuntu-24.04\n    timeout-minutes: 20\n    permissions:\n      contents: read\n",
        STAGING_JOB + "    runs-on: ubuntu-24.04\n    timeout-minutes: 20\n    permissions:\n      contents: write\n")),
    "make doadmin a superuser": ("staging-bootstrap", lambda: replaced(
        "test.yml", "PASSWORD '$owner_password' NOSUPERUSER CREATEDB", "PASSWORD '$owner_password' SUPERUSER CREATEDB")),
    "create rereply_app without doadmin": ("staging-bootstrap", lambda: replaced(
        "test.yml", "          SET ROLE doadmin;\n", "")),
    "skip the role shape assertion": ("staging-bootstrap", lambda: replaced(
        "test.yml", '          test "$memberships" = "doadmin>rereply_app:t,doadmin>rereply_owner:t"\n', "")),
    "run rls-migrate once": ("staging-bootstrap", lambda: replaced(
        "test.yml", "          snapshot rls-migrate-2\n          unchanged rls-migrate-2\n", "")),
    "tolerate a failing rls-migrate": ("staging-bootstrap", lambda: replaced(
        "test.yml", "          " + RLS_MIGRATE + "\n          snapshot rls-migrate-1\n",
        "          " + RLS_MIGRATE + " || true\n          snapshot rls-migrate-1\n")),
    "stop comparing the data": ("staging-bootstrap", lambda: replaced(
        "test.yml", '            cmp "$dir/data-bootstrap.sql" "$dir/data-$1.sql"\n', "")),
    "accept any second-bootstrap status": ("staging-bootstrap", lambda: replaced(
        "test.yml", '          test "$status" -eq 2\n          grep -q \'^staging-bootstrap', "          grep -q '^staging-bootstrap")),
    "tolerate a failure outside the pinned lines": ("staging-bootstrap", lambda: replaced(
        "test.yml", '          cat "$dir/second.err"\n', '          cat "$dir/second.err" || true\n')),
    "stop printing the server log on failure": ("staging-bootstrap", lambda: replaced(
        "test.yml", SERVER_LOG_TRAP, "trap 'kill \"$server\" 2>/dev/null || true' EXIT")),
    "skip the login": ("staging-bootstrap", lambda: replaced(
        "test.yml", '          test "$(code --cookie "$dir/cookies" http://127.0.0.1:8080/api/me)" = 200\n', "")),
    "drop the negative case": ("staging-bootstrap", without_negative_case),
    "make the negative case conditional": ("staging-bootstrap", lambda: replaced(
        "test.yml", NEGATIVE_STEP, NEGATIVE_STEP + "        if: ${{ github.event_name == 'push' }}\n")),
    "stop failing on the first error": ("staging-bootstrap", lambda: replaced(
        "test.yml", "      - name: Bootstrap the staging database\n        run: |\n          set -euo pipefail\n",
        "      - name: Bootstrap the staging database\n        run: |\n          set -uo pipefail\n")),
    # 14. Staging code stays out of the release images and their workflows.
    "build the bootstrap tool in the build job": ("staging-placement", lambda: replaced(
        "test.yml", "        run: go build -mod=readonly -v ./...\n",
        "        run: |\n          go build -mod=readonly -v ./...\n          go build -o tool ./release/staging/bootstrap\n")),
    "run the graph stub in the E2E shards": ("staging-placement", lambda: replaced(
        "e2e-tests.yml", "      - name: Build backend\n        run: go build -o rereply ./cmd/whatomate\n",
        "      - name: Build backend\n        run: go build -o rereply ./cmd/whatomate ./release/staging/graphstub/cmd/graph-stub\n")),
    "point a Release job at the stub": ("staging-placement", lambda: replaced(
        "ship.yml", "    name: Release plan\n", "    name: Release plan\n    env:\n      GRAPH_BASE: http://graph-stub:8090\n")),
}


class ParserTests(unittest.TestCase):
    def test_folded_scalars(self) -> None:
        self.assertEqual(WorkflowYaml("a: >-\n  x y\n  z\n\n  w\nb: 1\n").parse(), {"a": "x y z\nw", "b": 1})
        self.assertEqual(WorkflowYaml("a: >\n  x\n  y\n").parse(), {"a": "x y\n"})
        with self.assertRaises(ValueError):
            WorkflowYaml("a: >-\n  x\n    y\n").parse()

    def test_every_workflow_parses(self) -> None:
        self.assertGreaterEqual(set(SOURCES), {"ship.yml", "test.yml", "e2e-tests.yml"})
        docs = parse(SOURCES)
        for name, doc in docs.items():
            with self.subTest(workflow=name):
                self.assertTrue(jobs(doc))
        self.assertEqual(jobs(docs["test.yml"])["go-race"]["services"]["postgres"]["options"],
                         "--health-cmd pg_isready --health-interval 10s --health-timeout 5s --health-retries 5")

    def test_optional_pyyaml_cross_check(self) -> None:
        try:
            import yaml  # type: ignore[import-not-found]
        except ImportError:
            self.skipTest("PyYAML is not installed")

        # BaseLoader keeps every scalar a string (no YAML 1.1 "on" or
        # base-60 surprises), so compare against the reader's values as text.
        def text(value: Any) -> Any:
            if type(value) is dict:
                return {key: text(child) for key, child in value.items()}
            if type(value) is list:
                return [text(child) for child in value]
            if type(value) is bool:
                return "true" if value else "false"
            return "" if value is None else str(value)

        for name, source in SOURCES.items():
            with self.subTest(workflow=name):
                self.assertEqual(yaml.load(source, Loader=yaml.BaseLoader), text(WorkflowYaml(source).parse()))


class CiWorkflowTests(unittest.TestCase):
    def test_checked_in_workflows_pass_every_check(self) -> None:
        check_ci_workflows(SOURCES)

    def test_required_contexts_and_e2e_shard_names(self) -> None:
        docs = parse(SOURCES)
        for context, (workflow, job_id) in REQUIRED_CONTEXTS.items():
            with self.subTest(context=context):
                self.assertEqual(job_id, context)
                self.assertEqual(jobs(docs[workflow])[job_id]["name"], context)
        shard = jobs(docs["e2e-tests.yml"])["e2e-shard"]
        self.assertEqual([name_pattern(shard["name"]).fullmatch(f"e2e-shard-{n}") is not None for n in E2E_SHARDS],
                         [True] * len(E2E_SHARDS))

    def test_ci_main_push_runs_never_cancel_each_other(self) -> None:
        for workflow, prefix in CI_PREFIXES.items():
            self.assertEqual(parse(SOURCES)[workflow]["concurrency"], expected_concurrency(prefix))
            for old, new in (
                ("format('run-{0}', github.run_id)", "github.ref"),
                ("format('run-{0}', github.run_id)", "github.sha"),
                ("cancel-in-progress: ${{ github.event_name == 'pull_request' }}", "cancel-in-progress: true"),
            ):
                with self.subTest(workflow=workflow, mutation=new):
                    with self.assertRaisesRegex(AssertionError, "cancel each other"):
                        assert_concurrency_and_timeouts(replaced(workflow, old, new))

    def test_harmless_changes_pass(self) -> None:
        # The checks do not simply fail on any edit.
        assert_environments_and_secrets(replaced(
            "e2e-tests.yml", "          CI: true\n", "          CI: true\n          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n"))
        assert_environments_and_secrets(replaced(
            "ship.yml", "    environment: production\n", "    environment:\n      name: production\n"))
        check_ci_workflows(added("extra.yml", extra_workflow("[push]", "extra")))
        check_ci_workflows(replaced(
            "test.yml", "      - name: Build\n        run: go build -mod=readonly -v ./...\n",
            "      - name: Build\n        run: go build -mod=readonly -v ./...\n\n"
            "      - name: Vet\n        run: go vet -mod=readonly ./...\n"))
        check_ci_workflows(replaced("test.yml", 'split or revert"', 'split it or revert it"'))

    def test_every_negative_case_fails_its_check(self) -> None:
        labels = {label for label, _ in CHECKS}
        self.assertEqual({label for label, _ in NEGATIVE_CASES.values()}, labels)
        for case, (label, mutate) in NEGATIVE_CASES.items():
            with self.subTest(case=case):
                mutant = mutate()
                self.assertNotEqual(mutant, SOURCES)
                with self.assertRaisesRegex(AssertionError, rf"^{re.escape(label)}: "):
                    check_ci_workflows(mutant)


class AggregatorScriptTests(unittest.TestCase):
    def test_generic_aggregator_requires_every_result_to_be_success(self) -> None:
        step = step_named(jobs(parse(SOURCES)["test.yml"])["test"], "Require every Test job to succeed")
        self.assertEqual(step["env"], {"NEEDS_JSON": "${{ toJSON(needs) }}"})
        script = heredoc(step["run"], "NEEDS")
        cases = {
            "all success": ({"a": {"result": "success"}, "b": {"result": "success", "outputs": {}}}, 0),
            "one failure": ({"a": {"result": "success"}, "b": {"result": "failure"}}, 1),
            "one cancelled": ({"a": {"result": "cancelled"}, "b": {"result": "success"}}, 1),
            "one skipped": ({"a": {"result": "success"}, "b": {"result": "skipped"}}, 1),
            "no result": ({"a": {"result": "success"}, "b": {}}, 1),
            "not an object": ({"a": {"result": "success"}, "b": "success"}, 1),
            "no jobs": ({}, 1),
            "a list": ([], 1),
            "not json": ("success", 1),
        }
        for name, (value, expected) in cases.items():
            with self.subTest(case=name):
                raw = value if type(value) is str else json.dumps(value)
                self.assertEqual(run_python(script, env={"NEEDS_JSON": raw}) != 0, expected == 1)


class SchemaGuardScriptTests(unittest.TestCase):
    """The schema guard step's embedded script, run the way the step runs it."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.script = schema_guard_script(SOURCES)
        cls.commits = guard_fixture().commits

    def run_scenario(self, scenario: str) -> tuple[int, list[str], str]:
        code, stdout, summary = run_guard(self.script, scenario)
        return code, stdout.splitlines(), summary

    def assert_clean(self, scenario: str, base: str, head: str) -> None:
        event = GUARD_SCENARIOS[scenario][2]
        self.assertEqual(self.run_scenario(scenario), (
            0, [f"schema guard: {event}, {self.commits[base]}..{self.commits[head]}", "schema guard: clean"], ""))

    def assert_refused(self, scenario: str, base: str, head: str, reasons: list[str]) -> None:
        event = GUARD_SCENARIOS[scenario][2]
        self.assertEqual(self.run_scenario(scenario), (1, [
            f"schema guard: {event}, {self.commits[base]}..{self.commits[head]}",
            SCHEMA_GUARD_MESSAGE, *(f"  {reason}" for reason in reasons),
        ], f"### Schema guard refused\n\n{SCHEMA_GUARD_MESSAGE}\n\n" + "".join(f"- {reason}\n" for reason in reasons)))

    def test_every_scenario_has_its_outcome(self) -> None:
        for scenario, (_head, _main, _event, refused) in GUARD_SCENARIOS.items():
            with self.subTest(scenario=scenario):
                self.assertEqual(self.run_scenario(scenario)[0], 1 if refused else 0)

    def test_pull_requests_against_main(self) -> None:
        self.assert_clean("pull request equal to main", "main", "main")
        self.assert_clean("pull request changing handlers only", "main", "handlers")
        self.assert_clean("pull request changing only a test under internal/database", "main", "database-test")
        self.assert_clean("pull request changing only release/staging", "main", "staging")
        self.assert_refused("pull request editing internal/models", "main", "models",
                            ["guarded-tree:internal/models/models.go"])
        self.assert_refused("pull request adding an AutoMigrate call to internal/handlers", "main", "auto-migrate",
                            ["migration-call:internal/handlers/messages.go"])
        self.assert_refused("pull request whose guarded commit is not its last", "main", "models-then-handlers",
                            ["guarded-tree:internal/models/models.go"])
        # A tag named origin/main does not move the base off the remote branch.
        self.assert_refused("pull request beside a tag named origin/main at its head", "main", "models",
                            ["guarded-tree:internal/models/models.go"])

    def test_only_the_pull_requests_own_changes_count(self) -> None:
        # main took a guarded change after these pull requests branched off.
        self.assert_clean("pull request behind a main that moved on with a guarded change", "main", "handlers")
        self.assert_clean("clean merge commit onto a main that moved on", "main-moved", "merge-handlers")
        self.assert_refused("guarded merge commit onto a main that moved on", "main-moved", "merge-models",
                            ["guarded-tree:internal/models/models.go"])

    def test_pushes_to_main_against_the_first_parent(self) -> None:
        self.assert_clean("push to main merging handlers only", "main", "push-handlers")
        self.assert_refused("push to main merging an internal/models edit", "main", "push-models",
                            ["guarded-tree:internal/models/models.go"])

    def test_only_public_reasons_are_printed(self) -> None:
        self.assert_refused("pull request adding a non-ASCII path under internal/models", "main", "non-ascii",
                            ["(path withheld)"])

    def test_fails_closed(self) -> None:
        cases = {
            "push of a commit without a parent": "git-failed:schema-guard",
            "pull request without origin/main": "git-failed:schema-guard",
            "workflow_dispatch run": "context-invalid:event",
            "run without an event name": "context-invalid:event",
        }
        for scenario, code in cases.items():
            with self.subTest(scenario=scenario):
                self.assertEqual(self.run_scenario(scenario), (1, [f"schema guard failed closed: {code}"], ""))


class FrontendAuditPolicyTests(unittest.TestCase):
    """#213's dated braces exception, ported from the retired suite."""

    def test_frontend_audit_policy_fails_closed(self) -> None:
        gate = audit_run(SOURCES)
        self.assertIn("|| audit_rc=$?", gate)
        self.assertNotIn("|| true", gate)
        cases = {
            "clean": (audit_report(), 0, 0),
            "moderate only": (audit_report(**{"build-tool": advisory("build-tool", "moderate")}), 0, 0),
            "high production": (audit_report(**{"runtime-lib": advisory("runtime-lib", "high")}), 1, 1),
            "critical production": (audit_report(**{"runtime-lib": advisory("runtime-lib", "critical")}), 1, 1),
            "high dev-only": (audit_report(**{"build-tool": advisory("build-tool", "high")}), 1, 1),
            "registry error": ({"error": {"code": "E503", "summary": "unavailable"}}, 1, 1),
            "error beside an empty report": (dict(audit_report(), error={"code": "E500"}), 1, 1),
            "error object with exit 0": ({"error": {"code": "E503", "summary": "unavailable"}}, 0, 1),
            "error beside an empty report with exit 0": (dict(audit_report(), error={"code": "E500"}), 0, 1),
            "missing vulnerabilities": ({"auditReportVersion": 2, "metadata": {}}, 0, 1),
            "empty vulnerabilities without metadata": ({"auditReportVersion": 2, "vulnerabilities": {}}, 0, 1),
            "old report version": (dict(audit_report(), auditReportVersion=1), 0, 1),
            "npm crashed": (audit_report(), 2, 1),
            "exit 1 without findings": (audit_report(), 1, 1),
            "exit 0 with a high finding": (audit_report(**{"runtime-lib": advisory("runtime-lib", "high")}), 0, 1),
            "not json": ("npm ERR! network", 1, 1),
            "not an object": ([], 0, 1),
        }
        for name, (body, rc, expected) in cases.items():
            with self.subTest(case=name):
                returned = run_audit_policy(gate, body, rc, AUDIT_LOCK)
                self.assertEqual(returned != 0, expected == 1, returned)

    def test_braces_exception_is_exact_dated_and_dev_only(self) -> None:
        gate = audit_run(SOURCES)
        self.assertRegex(gate, r'(?m)^ALLOWED = \{\("braces", "stack-exhaustion denial of service", "2026-12-31"\)\}$')
        expired = gate.replace('"2026-12-31")}', '"2000-01-01")}', 1)
        without = re.sub(r"(?m)^ALLOWED = .*$", "ALLOWED = set()", gate, count=1)
        self.assertNotEqual(expired, gate)
        self.assertNotEqual(without, gate)
        cases = {
            "braces dev-only": (gate, braces_report(), braces_lock(True), 0),
            "braces critical dev-only": (gate, braces_report(severity="critical"), braces_lock(True), 0),
            "braces reaching production": (gate, braces_report(), braces_lock(False), 1),
            "braces other advisory": (gate, braces_report(title="braces prototype pollution"), braces_lock(True), 1),
            "braces after review date": (expired, braces_report(), braces_lock(True), 1),
            "no exception": (without, braces_report(), braces_lock(True), 1),
        }
        for name, (run, body, lock, expected) in cases.items():
            with self.subTest(case=name):
                returned = run_audit_policy(run, body, 1, lock)
                self.assertEqual(returned != 0, expected == 1, returned)


class StagingPlacementTests(unittest.TestCase):
    """The release Dockerfiles half of property 14, on synthetic files."""

    def test_checked_in_dockerfiles_are_found_and_clean(self) -> None:
        files = release_dockerfiles()
        self.assertIn("docker/Dockerfile", files)
        self.assertIn("docker/release/web.Dockerfile", files)
        self.assertFalse(any(name.startswith("docker/staging/") for name in files))
        assert_staging_placement(SOURCES)

    def test_a_release_dockerfile_referencing_staging_code_fails(self) -> None:
        files = release_dockerfiles()
        for name, line in (
            ("docker/release/web.Dockerfile", "RUN go build -o /out/bootstrap ./release/staging/bootstrap\n"),
            ("docker/Dockerfile", "COPY --from=stub /graph-stub /usr/local/bin/graph-stub\n"),
            ("docker/meta-relay.Dockerfile", "COPY release/staging/graphstub/ ./graphstub/\n"),
        ):
            with self.subTest(file=name):
                with self.assertRaisesRegex(AssertionError, re.escape(name)):
                    assert_staging_placement(SOURCES, {**files, name: files[name] + line})

    def test_missing_release_dockerfiles_fail(self) -> None:
        with self.assertRaisesRegex(AssertionError, "release Dockerfiles were not found"):
            assert_staging_placement(SOURCES, {})

    def test_the_staging_bootstrap_job_may_reference_staging_code(self) -> None:
        job = jobs(parse(SOURCES)["test.yml"])[STAGING_BOOTSTRAP_JOB]
        self.assertTrue(any(STAGING_REFERENCE.search(text) for text in text_nodes(job)))


class StagingBootstrapJobTests(unittest.TestCase):
    def test_the_proof_runs_rls_migrate_twice_between_snapshots(self) -> None:
        job = jobs(parse(SOURCES)["test.yml"])[STAGING_BOOTSTRAP_JOB]
        accept = step_named(job, "Accept the database with rls-migrate twice and refuse a second bootstrap")
        self.assertEqual(run_lines(accept).count(RLS_MIGRATE), 2)

    def test_tolerated_failures_are_recognised(self) -> None:
        for line in ("make || true", "make ||:", "make || exit 0", "set +e", "set +euo pipefail"):
            with self.subTest(line=line):
                self.assertIsNotNone(TOLERATED_FAILURE.search(line))
        for line in ('"$dir/bootstrap" -config x 2> err || status=$?', "set -euo pipefail", "truest"):
            with self.subTest(line=line):
                self.assertIsNone(TOLERATED_FAILURE.search(line))


if __name__ == "__main__":
    unittest.main()
