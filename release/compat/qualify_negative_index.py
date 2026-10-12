"""Dedicated hosted token/order qualification; never compatibility evidence.

Only this main-only qualification workflow may call this entry. Q is a fixed
pre-producer ancestor reserved outside every future consumer lineage. Synthetic
normal3 inputs exercise public validation and the actual publisher transport;
only separately labelled qualification statements are signing subjects.
"""
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import time

sys.path.insert(0, str(Path(__file__).parent))
sys.path.insert(0, str(Path(__file__).parent.parent / "deployment"))
import compat_negative_index as policy

Q = "12519b7f2ad020f1aceb2a17432a686a9f87ae6d"
WORKFLOW = ".github/workflows/compat-negative-index-qualification.yml"
PREDICATE = "https://rereply.dev/attestations/negative-index-qualification/v1"
FIXTURE = "synthetic-normal3-token-order-v1"
MAX_SECONDS = 600
MAX_CALLS = 32
MAX_BYTES = 4 << 20
GIT = Path("/usr/bin/git")
STDOUT = 80 << 10
STDERR = 16 << 10
_REFUSED = "negative-index-qualification-refused"
_SHA = re.compile(r"[0-9a-f]{40}\Z")


class Refusal(ValueError):
    def __init__(self):
        super().__init__(_REFUSED)


def need(value):
    if not value:
        raise Refusal()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"),
                       ensure_ascii=True, allow_nan=False) + "\n").encode("ascii")


def modules():
    import negative_index
    import public_evidence
    import compat_negative_transport
    import record_transport
    return negative_index, public_evidence, compat_negative_transport, record_transport


@dataclass(frozen=True, slots=True)
class Actual:
    sha: str
    run: int
    attempt: int

    @property
    def suffix(self):
        return f"{self.sha}-{self.run}-{self.attempt}"

    @property
    def probe(self):
        return f"refs/tags/{policy.PREFIX}probe-{self.run}-{self.attempt}-permissions"


def actual_context(env):
    need(env.get("GITHUB_REPOSITORY") == policy.REPOSITORY
         and env.get("RUNNER_ENVIRONMENT") == "github-hosted"
         and env.get("RUNNER_DEBUG") != "1"
         and env.get("GITHUB_EVENT_NAME") == "workflow_dispatch"
         and env.get("GITHUB_REF") == "refs/heads/main"
         and env.get("GITHUB_REF_PROTECTED") == "true")
    sha, run = env.get("GITHUB_SHA"), env.get("GITHUB_RUN_ID")
    need(type(sha) is str and _SHA.fullmatch(sha) and sha != Q
         and env.get("GITHUB_WORKFLOW_SHA") == sha
         and env.get("GITHUB_WORKFLOW_REF") == policy.REPOSITORY + "/" + WORKFLOW + "@refs/heads/main"
         and type(run) is str and re.fullmatch(r"[1-9][0-9]{0,15}", run)
         and int(run) < 10**16 and env.get("GITHUB_RUN_ATTEMPT") == "1"
         and not env.get("COMPAT_ROLLBACK_TARGET"))
    return Actual(sha, int(run), 1)


def _write(path, raw):
    need(type(raw) is bytes and len(raw) <= MAX_BYTES)
    with path.open("xb") as stream:
        stream.write(raw)
    need(path.read_bytes() == raw)


class Recorder:
    """One fixed tightening wrapper; unchanged completed captures go upstream."""
    def __init__(self, directory, runtime):
        self.directory, self.runtime = directory, runtime
        self.rows, self.total = [], 0

    def __call__(self, argv, *, env, timeout, stdout_limit, stderr_limit):
        need(len(self.rows) < MAX_CALLS and self.total + stderr_limit < MAX_BYTES)
        limit = min(stdout_limit, MAX_BYTES - self.total - stderr_limit)
        number = len(self.rows) + 1
        row = {"number": number, "argv": list(argv), "complete": False}
        self.rows.append(row)
        # The original bounded process/tree implementation owns containment.
        # This wrapper never retries, rewrites arguments or interprets success.
        result = self.runtime.bounded_command(argv, env=env, timeout=timeout,
            stdout_limit=limit, stderr_limit=stderr_limit)
        need(type(result) is self.runtime.Capture and type(result.returncode) is int
             and type(result.stdout) is bytes and type(result.stderr) is bytes
             and len(result.stdout) <= limit and len(result.stderr) <= stderr_limit)
        self.total += len(result.stdout) + len(result.stderr)
        _write(self.directory / f"{number:02d}.stdout", result.stdout)
        row.update(complete=True, returncode=result.returncode,
            stdout_bytes=len(result.stdout), stdout_sha256=digest(result.stdout),
            stderr_bytes=len(result.stderr), stderr_sha256=digest(result.stderr))
        return result


def synthetic(ctx, *, negative, cli):
    """Explicit invented public data; no source, image or database authority."""
    empty = digest(canonical([]))
    old = {"release": "prod-0000", "manifest_sha256": digest(b"synthetic-manifest"),
           "sha": Q, "web_digest": "sha256:" + digest(b"synthetic-web")}
    snapshot = {"version": 4, "sha": Q, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
        "requested_rollback_target": None, "latest": old, "selected": dict(old),
        "database_base": {"mode": "legacy-source-only", "source_sha": Q, "origin": None, "catalog": None},
        "selected_web_source_sha": Q,
        "pending": {"format_version": 1, "candidate_sha": Q, "previous_sha": Q,
            "candidate_digest": empty, "previous_digest": empty,
            "active_artifacts": [list(row) for row in cli.PENDING_ACTIVE_ARTIFACTS],
            "candidate_files": [], "previous_files": [], "target_version": None},
        "preview": {"kind": "none", "dormant": False, "blocked": False}}
    value = {"sha": Q, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
        "previous_release": old["release"], "previous_web_digest": old["web_digest"],
        "classification": {"kind": "none", "dormant": False}, "forward": True,
        "old_migrate_ok": {"rw": True, "ro": True, "negative_control": True},
        "old_server_ok": {"boot": not negative, "read": True, "write": True, "reverse": True, "workers": True},
        "reversible": True, "catalog_sha256": digest(b"synthetic-no-database-measurement")}
    raw = cli.proof.result_bytes(value)
    context = cli.canonical(cli.evidence_context(raw, snapshot, ctx))
    return cli.canonical(snapshot), raw, context


def _stage_case(root, ctx, raw):
    root.mkdir(mode=0o700)
    inputs = root / "compat-input"
    inputs.mkdir(mode=0o700)
    public = root / ("compat-validate-evidence-" + ctx.suffix) / "public"
    public.parent.mkdir(mode=0o700)
    public.mkdir(mode=0o700)
    for name, data in zip(("resolution.json", "compat-result.json", "compat-context.json"), raw):
        _write(inputs / name, data)
    for name, data in zip(("compat-result.json", "compat-context.json"), raw[1:]):
        _write(public / name, data)


def _frame(result, status, *, success, expected_created_location=None):
    found, body, headers = policy._http_frame(result.stdout, expected_created_location=expected_created_location)
    need(found == status and result.returncode == (0 if success else 1)
         and (not result.stderr if success else True)
         and headers.get(b"content-type", b"").lower() in
             (b"application/json", b"application/json; charset=utf-8"))
    return policy._load(body, 64 << 10), body


def absent(result):
    value, _ = _frame(result, 404, success=False)
    need(type(value) is dict and set(value) <= {"message", "documentation_url", "status"}
         and value.get("message") == "Not Found"
         and value.get("status", "404") == "404"
         and value.get("documentation_url", "https://docs.github.com/rest/git/refs#get-a-reference") ==
             "https://docs.github.com/rest/git/refs#get-a-reference")


def denied(result, *, operation, ref):
    """Precommitted, unqualified REST grammar; generic 403/422 never proves it."""
    need(operation in ("update", "delete") and type(ref) is str
         and re.fullmatch(re.escape("refs/tags/" + policy.PREFIX) + r"probe-[1-9][0-9]{0,15}-1-permissions", ref))
    value, _ = _frame(result, 422, success=False)
    reason = "Cannot update this ref" if operation == "update" else "Cannot delete this ref"
    messages = {"Repository rule violations found\n\n" + reason + "\n",
                "Repository rule violations found\n\n- " + reason + "\n",
                "Repository rule violations found for " + ref + ".\n\n- " + reason + "\n"}
    suffix = "update-a-reference" if operation == "update" else "delete-a-reference"
    need(type(value) is dict and set(value) <= {"message", "documentation_url", "status"}
         and value.get("message") in messages and value.get("status", "422") == "422"
         and value.get("documentation_url", "https://docs.github.com/rest/git/refs#" + suffix) ==
             "https://docs.github.com/rest/git/refs#" + suffix)


class Qualification:
    """Fixed command graph; no selectable repository, SHA, ref or operation."""
    def __init__(self, actual, root, recorder, budget, setup):
        self.actual, self.root, self.recorder, self.budget, self.setup = actual, root, recorder, budget, setup
        self.publisher, self.cli, self.transport, self.source = modules()
        need(type(budget) is self.source.OperationBudget)
        self.repo = self.cli.regular_path(Path(os.environ.get("GITHUB_WORKSPACE", "")), directory=True)
        need(self.repo == Path(__file__).absolute().parents[2])
        self.git = self.cli.regular_path(GIT)
        self.gh = self.cli.regular_path(Path(os.environ.get("COMPAT_GH_BIN", "")))
        self.git_env = self.cli.common.scrubbed_env({"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C",
            "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}, base=os.environ)
        self.pins = tuple(self._pin(path) for path in (self.git, self.gh))
        self.client = None

    def _pin(self, path):
        self.budget.check()
        before = self.publisher._identity(path, self.cli)
        need(0 < before[3] <= 256 << 20)
        h, size = hashlib.sha256(), 0
        with path.open("rb") as stream:
            while block := stream.read(1 << 20):
                self.budget.check()
                size += len(block)
                need(size <= 256 << 20)
                h.update(block)
        need(size == before[3] and self.publisher._identity(path, self.cli) == before)
        return before, h.hexdigest()

    def _git(self, args):
        result = self.budget.capture([str(self.git), "--no-replace-objects", "--no-optional-locks", "-c", "core.fsmonitor=false",
            "-C", str(self.repo), *args], env=self.git_env,
            timeout=30, stdout_limit=64 << 10, stderr_limit=STDERR)
        need(result.returncode == 0 and not result.stderr)
        return result.stdout

    def source_check(self):
        consumer = self.repo / "release/deployment/compat-negative-consumer-v1.json"
        need(not consumer.exists() and not consumer.is_symlink())
        need(self._git(["rev-parse", "--verify", "HEAD"]) == (self.actual.sha + "\n").encode())
        need(self._git(["merge-base", Q, self.actual.sha]) == (Q + "\n").encode())
        need(self._git(["status", "--porcelain=v1", "--untracked-files=all"]) == b"")
        need(tuple(self._pin(path) for path in (self.git, self.gh)) == self.pins)

    def _api(self, method, path, fields=()):
        self.client._check()
        args = self.client._args(method, path, include=True)
        for flag, value in fields:
            args.extend((flag, value))
        result = self.budget.capture([str(self.gh), *args], env=dict(self.client._env),
            timeout=30, stdout_limit=STDOUT, stderr_limit=STDERR)
        self.client._check()
        return result

    def ref(self, name, sha):
        _, body = _frame(self._api("GET", policy.API + "/git/ref/" + name[5:]), 200, success=True)
        return self.transport.NegativePublisher._ref_body(body, name, sha)

    def identities(self):
        self.ref("refs/heads/main", self.actual.sha)
        value, _ = _frame(self._api("GET", policy.API + "/actions/runs/" + str(self.actual.run)), 200, success=True)
        need(type(value) is dict and type(value.get("id")) is int and value["id"] == self.actual.run
             and type(value.get("run_attempt")) is int and value["run_attempt"] == 1
             and value.get("head_sha") == self.actual.sha and value.get("head_branch") == "main"
             and value.get("event") == "workflow_dispatch" and value.get("path") == WORKFLOW
             and type(value.get("repository")) is dict
             and type(value["repository"].get("id")) is int
             and value["repository"]["id"] == policy.REPOSITORY_ID
             and value["repository"].get("full_name") == policy.REPOSITORY)

    def probe(self):
        name = self.actual.probe
        path = policy.API + "/git/refs/" + name[5:]
        absent(self._api("GET", policy.API + "/git/ref/" + name[5:]))
        _, body = _frame(self._api("POST", policy.API + "/git/refs",
            (("--raw-field", "ref=" + name), ("--raw-field", "sha=" + self.actual.sha))), 201, success=True,
            expected_created_location="https://api.github.com" + policy.API + "/git/" + name)
        created = self.transport.NegativePublisher._ref_body(body, name, self.actual.sha)
        need(self.ref(name, self.actual.sha) == created)
        denied(self._api("PATCH", path, (("--raw-field", "sha=" + Q), ("--field", "force=true"))),
               operation="update", ref=name)
        need(self.ref(name, self.actual.sha) == created)
        denied(self._api("DELETE", path), operation="delete", ref=name)
        need(self.ref(name, self.actual.sha) == created)
        return created

    def run(self):
        self.source_check()
        ctx = self.cli.Context(Q, self.actual.run, 1, None, "workflow_dispatch", "refs/heads/main")
        cases = []
        for label, negative in (("positive", False), ("negative", True)):
            case_root = self.root / label
            _stage_case(case_root, ctx, synthetic(ctx, negative=negative, cli=self.cli))
            subjects = self.publisher._subjects(case_root, ctx, self.cli, self.budget)
            request, positive = self.publisher._validate(subjects, ctx, self.cli)
            need(positive is (not negative) and request.producer_sha == Q and request.protocol == "normal3")
            self.publisher._reread(subjects, case_root, ctx, self.cli, self.budget, request, positive)
            cases.append((label, case_root, subjects, request, positive))
        positive_before = self.budget.commands
        positive_receipt = policy.NoIntentRequired(cases[0][3], self.setup.sha256)
        self.publisher._reread(cases[0][2], cases[0][1], ctx, self.cli, self.budget, positive_receipt.request, True)
        need(self.budget.commands == positive_before)
        config = self.root / "gh-config"
        config.mkdir(mode=0o700)
        self.client = self.transport.open_publisher(self.gh, os.environ.get("GH_TOKEN"), config,
            budget=self.budget, base_env=self.cli.common.scrubbed_env(base=os.environ))
        need(type(self.client) is self.transport.NegativePublisher and self.client.budget is self.budget
             and self.client.setup == self.setup)
        rule = self.client.read_rule()
        self.identities()
        self.probe()
        request = cases[1][3]
        marker = policy.marker_ref(request)
        absent(self._api("GET", policy.API + "/git/ref/" + marker[5:]))
        receipt = self.client.publish_intent(request)
        need(type(receipt) is policy.IntentPersisted and receipt.request == request
             and receipt.setup_sha256 == self.setup.sha256 and receipt.marker_ref == marker)
        for _, case_root, subjects, bound, positive in cases:
            self.publisher._reread(subjects, case_root, ctx, self.cli, self.budget, bound, positive)
        need(self.client.read_rule() == rule)
        self.ref(self.actual.probe, self.actual.sha)
        self.ref(marker, Q)
        self.identities()
        self.source_check()
        need(self.budget.commands == len(self.recorder.rows) <= MAX_CALLS
             and self.budget.captured_bytes == self.recorder.total <= MAX_BYTES)
        self.budget.check()
        return cases, receipt, digest(rule)


def _statements(actual, setup, cases, receipt, rule_hash, trace):
    result = {}
    for label, _, subjects, request, positive in cases:
        result[label] = {"format_version": 1, "purpose": "negative-index-qualification",
            "synthetic": True, "database_measurement": False, "fixture": FIXTURE,
            "actual": {"repository": policy.REPOSITORY, "workflow": WORKFLOW, "sha": actual.sha,
                       "run_id": actual.run, "run_attempt": actual.attempt, "ref": "refs/heads/main"},
            "synthetic_source": Q, "protocol": "normal3", "case": label,
            "synthetic_accepted": positive, "setup_sha256": setup.sha256, "rule_sha256": rule_hash,
            "snapshot_sha256": digest(subjects.snapshot), "result_sha256": request.result_sha256,
            "context_sha256": request.context_sha256, "permission_probe_ref": actual.probe,
            "marker_ref": None if positive else receipt.marker_ref,
            "command_receipt_sha256": digest(trace),
            "claims": ["ordinary-token-create", "rule-denied-update", "rule-denied-delete",
                       "synthetic-public-validation", "intent-before-qualification-signing"],
            "limitations": ["not-normal-compatibility", "not-database-proof", "not-rollback4-qualification",
                            "not-consumer-activation", "not-lifetime-admin-continuity"]}
    return result


def main(argv=None):
    budget = recorder = output = None
    try:
        setup = policy.load_producer_setup()  # No absent-setup operational effects.
        need((sys.argv[1:] if argv is None else argv) == [])
        actual = actual_context(os.environ)
        publisher, cli, _, source = modules()
        deadline = time.monotonic() + MAX_SECONDS
        parent = cli.regular_path(Path(os.environ.get("RUNNER_TEMP", "")), directory=True)
        selected = parent / ("compat-negative-qualification-" + actual.suffix)
        selected.mkdir(mode=0o700)  # No resume or overwrite of an earlier attempt.
        output = selected
        private = output / "receipts"
        private.mkdir(mode=0o700)
        recorder = Recorder(private, source.runtime)
        budget = source.OperationBudget(deadline, capture=recorder)
        operation = Qualification(actual, output, recorder, budget, setup)
        cases, receipt, rule_hash = operation.run()
        trace = canonical({"commands": recorder.rows, "captured_bytes": recorder.total})
        _write(private / "commands.json", trace)
        statements = _statements(actual, setup, cases, receipt, rule_hash, trace)
        public = output / "public"
        public.mkdir(mode=0o700)
        for label, value in statements.items():
            raw = canonical(value)
            _write(public / ("qualification-" + label + ".json"), raw)
            # Custom predicate repeats the closed labelled statement, never the
            # synthetic normal result/context as an attestation subject.
            _write(public / ("predicate-" + label + ".json"), raw)
        for label, value in statements.items():
            for prefix in ("qualification-", "predicate-"):
                need(publisher._read(public / (prefix + label + ".json"), 16384, cli, budget) == canonical(value))
        budget.close()
        result = canonical({"status": "QUALIFIED_SYNTHETIC_TOKEN_ORDER_ONLY", "commands": budget.commands,
                            "captured_bytes": budget.captured_bytes, "sha": actual.sha,
                            "synthetic_source": Q, "setup_sha256": setup.sha256})
        _write(output / "result.json", result)
        target = cli.regular_path(Path(os.environ.get("GITHUB_OUTPUT", "")))
        need(target.stat().st_nlink == 1)
        with target.open("a", encoding="ascii", newline="\n") as stream:
            stream.write("complete=true\n")
        return 0
    except Exception:
        if budget is not None:
            budget.failed = True
        if output is not None and output.is_dir():
            try:
                _write(output / "failure.json", canonical({"status": "REFUSED_NO_SIGNING",
                    "commands": recorder.rows if recorder is not None else [],
                    "captured_bytes": recorder.total if recorder is not None else 0,
                    "repair_or_retry": False}))
            except Exception:
                pass
        print(_REFUSED, file=sys.stderr)
        return 1
    finally:
        if budget is not None:
            try:
                budget.close()
            except Exception:
                pass


if __name__ == "__main__":
    raise SystemExit(main())
