"""Controlled native captures only; no hosted capability or database claims."""
import ast
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import qualify_negative_index as q
import public_evidence as public
import negative_index as publisher
import compat_negative_index as policy
import compat_negative_transport as transport
import record_transport as source
import test_compat_negative_index as fixtures
from test_ci_workflows import WorkflowYaml

H = "a" * 40
ROOT = Path(__file__).resolve().parents[2]
PATH = ROOT / ".github/workflows/compat-negative-index-qualification.yml"


def environment(root, repo, binary, output):
    return {"GITHUB_REPOSITORY": policy.REPOSITORY, "RUNNER_ENVIRONMENT": "github-hosted",
        "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
        "GITHUB_REF_PROTECTED": "true", "GITHUB_SHA": H, "GITHUB_WORKFLOW_SHA": H,
        "GITHUB_WORKFLOW_REF": policy.REPOSITORY + "/" + q.WORKFLOW + "@refs/heads/main",
        "GITHUB_RUN_ID": "123456789", "GITHUB_RUN_ATTEMPT": "1", "GH_TOKEN": "synthetic-token",
        "RUNNER_TEMP": str(root), "GITHUB_WORKSPACE": str(repo), "COMPAT_GH_BIN": str(binary),
        "GITHUB_OUTPUT": str(output)}


def http(status, value):
    return fixtures.http(status, q.canonical(value))


def ref_value(name, sha):
    return {"ref": name, "node_id": "REF_synthetic", "url": "https://api.github.com" + policy.API + "/git/" + name,
        "object": {"type": "commit", "sha": sha,
                   "url": "https://api.github.com" + policy.API + "/git/commits/" + sha}}


class Capture:
    def __init__(self):
        self.calls = []
        self.refs = {"refs/heads/main": H}
        self.mode = None
        self.positive_commands = None
        self.on_call = lambda argv: None

    def __call__(self, argv, *, env, timeout, stdout_limit, stderr_limit):
        self.calls.append(list(argv))
        self.on_call(argv)
        code, raw, err = 0, b"", b""
        if "-C" in argv:
            assert "GH_TOKEN" not in env
            args = argv[argv.index("-C") + 2:]
            if args == ["rev-parse", "--verify", "HEAD"]:
                raw = (H + "\n").encode()
            elif args == ["merge-base", q.Q, H]:
                raw = (("b" * 40 if self.mode == "ancestry" else q.Q) + "\n").encode()
            elif args == ["status", "--porcelain=v1", "--untracked-files=all"]:
                raw = b" M injected.py\n" if self.mode == "dirty" else b""
            else:
                raise AssertionError(args)
        elif argv[1:] == ["--version"]:
            assert env["GH_TOKEN"] == "synthetic-token"
            raw = b"gh version 2.98.0 (controlled)\n"
        else:
            assert env["GH_TOKEN"] == "synthetic-token"
            assert argv[1:5] == ["api", "--hostname", "github.com", "--method"]
            assert tuple(argv[argv.index("-H"):argv.index("-H") + 4]) == transport.HEADERS
            method, path = argv[5:7]
            fields = {argv[i + 1].split("=", 1)[0]: argv[i + 1].split("=", 1)[1]
                      for i, item in enumerate(argv) if item in ("--raw-field", "--field")}
            if method == "GET" and path == policy.API:
                raw = q.canonical({"id": policy.REPOSITORY_ID, "full_name": policy.REPOSITORY})
            elif method == "GET" and path == policy.API + "/rulesets/12345":
                raw = fixtures.rule_response()
            elif method == "GET" and path == policy.API + "/actions/runs/123456789":
                raw = http(200, {"id": 123456789, "run_attempt": 1,
                    "head_sha": "f" * 40 if self.mode == "foreign-run" else H,
                    "head_branch": "main", "event": "workflow_dispatch", "path": q.WORKFLOW,
                    "repository": {"id": policy.REPOSITORY_ID, "full_name": policy.REPOSITORY}})
            elif method == "GET" and path.startswith(policy.API + "/git/ref/"):
                name = "refs/" + path.split("/git/ref/", 1)[1]
                if name in self.refs:
                    raw = http(200, ref_value(name, self.refs[name]))
                else:
                    code, raw = 1, http(404, {"message": "Not Found", "status": "404"})
            elif method == "POST" and path == policy.API + "/git/refs":
                assert set(fields) == {"ref", "sha"} and fields["ref"] not in self.refs
                self.refs[fields["ref"]] = fields["sha"]
                raw = http(201, ref_value(fields["ref"], fields["sha"]))
                if self.mode == "ambiguous-post" or (self.mode == "ambiguous-marker" and fields["sha"] == q.Q):
                    code, raw = 1, http(502, {"message": "Unknown outcome"})
            elif method in ("PATCH", "DELETE"):
                name = "refs/" + path.split("/git/refs/", 1)[1]
                assert name == f"refs/tags/{policy.PREFIX}probe-123456789-1-permissions"
                assert self.refs[name] == H
                if method == "PATCH":
                    assert fields == {"sha": q.Q, "force": "true"}
                else:
                    assert fields == {}
                reason = "Cannot update this ref" if method == "PATCH" else "Cannot delete this ref"
                code, raw = 1, http(422, {"message": "Repository rule violations found\n\n" + reason + "\n"})
                if self.mode == "auth-denial":
                    raw = http(403, {"message": "Resource not accessible by integration"})
                elif self.mode == "generic-422":
                    raw = http(422, {"message": "Validation Failed"})
                elif self.mode == "update-succeeded" and method == "PATCH":
                    self.refs[name] = q.Q
                    code, raw = 0, http(200, ref_value(name, q.Q))
                elif self.mode == "delete-succeeded" and method == "DELETE":
                    del self.refs[name]
                    code, raw = 0, b"HTTP/2.0 204 No Content\r\n\r\n"
                elif self.mode == "changed-after-denial":
                    self.refs[name] = q.Q
            else:
                raise AssertionError((method, path, fields))
        assert len(raw) <= stdout_limit and len(err) <= stderr_limit
        return source.runtime.Capture(code, raw, err)


class QualificationTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.repo = self.root / "repo"
        self.repo.mkdir()
        fake_file = self.repo / "release/compat/qualify_negative_index.py"
        fake_file.parent.mkdir(parents=True)
        fake_file.write_bytes(b"# controlled source path\n")
        self.temp = self.root / "runner"
        self.temp.mkdir()
        self.git = self.root / "git"
        self.git.write_bytes(b"controlled-git")
        self.gh = self.root / "gh"
        self.gh.write_bytes(b"controlled-gh")
        self.github_output = self.root / "github-output"
        self.github_output.write_bytes(b"")
        self.env = environment(self.temp, self.repo, self.gh, self.github_output)
        self.capture = Capture()
        for patcher in (mock.patch.dict(os.environ, self.env, clear=True),
                        mock.patch.object(q, "__file__", str(fake_file)),
                        mock.patch.object(q, "GIT", self.git),
                        mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", fixtures.setup_bytes()),
                        mock.patch.object(source.runtime, "bounded_command", self.capture)):
            patcher.start()
            self.addCleanup(patcher.stop)

    @property
    def out(self):
        return self.temp / f"compat-negative-qualification-{H}-123456789-1"

    def main(self):
        error = io.StringIO()
        with contextlib.redirect_stderr(error):
            code = q.main([])
        self.assertEqual(error.getvalue(), "" if code == 0 else q._REFUSED + "\n")
        return code

    def test_real_validator_and_real_publisher_composition_with_one_token(self):
        self.assertEqual(self.main(), 0)
        self.assertEqual(self.github_output.read_bytes(), b"complete=true\n")
        result = json.loads((self.out / "result.json").read_bytes())
        self.assertEqual(result["commands"], 29)
        self.assertLessEqual(result["captured_bytes"], q.MAX_BYTES)
        positive = json.loads((self.out / "public/qualification-positive.json").read_bytes())
        negative = json.loads((self.out / "public/qualification-negative.json").read_bytes())
        for value in (positive, negative):
            self.assertIs(value["synthetic"], True)
            self.assertIs(value["database_measurement"], False)
            self.assertEqual(value["actual"]["sha"], H)
            self.assertEqual(value["synthetic_source"], q.Q)
            self.assertEqual(value["protocol"], "normal3")
        self.assertIsNone(positive["marker_ref"])
        self.assertFalse(negative["synthetic_accepted"])
        self.assertEqual(self.capture.refs[negative["marker_ref"]], q.Q)
        self.assertNotIn(policy.PREFIX + H, repr(self.capture.refs))
        writes = [row for row in self.capture.calls if "--method" in row and row[5] in ("POST", "PATCH", "DELETE")]
        self.assertEqual([row[5] for row in writes], ["POST", "PATCH", "DELETE", "POST"])
        self.assertTrue(all("synthetic-token" not in repr(row) for row in self.capture.calls))
        self.assertEqual(os.environ["GITHUB_SHA"], H)

    def test_absent_setup_has_zero_files_native_and_operational_import_effects(self):
        with mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", None), \
             mock.patch.object(q, "modules", side_effect=AssertionError("operational import")):
            self.assertEqual(self.main(), 1)
        self.assertEqual(self.capture.calls, [])
        self.assertEqual(list(self.temp.iterdir()), [])

    def test_real_context_no_ref_or_attempt_or_workflow_overrides(self):
        for key, value in (("GITHUB_REF", "refs/heads/topic"), ("GITHUB_RUN_ATTEMPT", "2"),
                           ("GITHUB_SHA", q.Q), ("GITHUB_EVENT_NAME", "push"),
                           ("GITHUB_WORKFLOW_SHA", "b" * 40), ("GITHUB_REF_PROTECTED", "false"),
                           ("GITHUB_WORKFLOW_REF", policy.REPOSITORY + "/.github/workflows/compat.yml@refs/heads/main")):
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: value}):
                self.assertEqual(self.main(), 1)
        self.assertEqual(self.capture.calls, [])

    def test_existing_attempt_directory_never_changes(self):
        self.out.mkdir()
        (self.out / "original").write_bytes(b"preserve")
        self.assertEqual(self.main(), 1)
        self.assertEqual([x.name for x in self.out.iterdir()], ["original"])
        self.assertEqual(self.capture.calls, [])

    def test_nonancestor_refuses_before_gh(self):
        self.capture.mode = "ancestry"
        self.assertEqual(self.main(), 1)
        self.assertEqual(len(self.capture.calls), 2)
        self.assertTrue(all("-C" in row for row in self.capture.calls))

    def test_dirty_source_refuses_before_gh(self):
        self.capture.mode = "dirty"
        self.assertEqual(self.main(), 1)
        self.assertTrue(all("-C" in row for row in self.capture.calls))

    def test_consumer_must_be_absent_no_placeholder_authority(self):
        destination = self.repo / "release/deployment/compat-negative-consumer-v1.json"
        destination.parent.mkdir()
        destination.write_bytes(b"null\n")
        self.assertEqual(self.main(), 1)
        self.assertEqual(self.capture.calls, [])

    def test_wrong_actual_run_stops_before_mutation(self):
        self.capture.mode = "foreign-run"
        self.assertEqual(self.main(), 1)
        self.assertFalse(any("--method" in x and x[5] != "GET" for x in self.capture.calls))

    def test_existing_probe_is_not_adopted(self):
        self.capture.refs[q.actual_context(self.env).probe] = H
        self.assertEqual(self.main(), 1)
        self.assertFalse(any("--method" in x and x[5] == "POST" for x in self.capture.calls))

    def test_generic_auth_denial_cannot_qualify_rule_protection(self):
        self.capture.mode = "auth-denial"
        self.assertEqual(self.main(), 1)
        self.assertFalse(any("--method" in x and x[5] == "DELETE" for x in self.capture.calls))
        self.assertEqual(self.github_output.read_bytes(), b"")

    def test_generic_422_cannot_qualify_rule_protection(self):
        self.capture.mode = "generic-422"
        self.assertEqual(self.main(), 1)
        self.assertEqual(self.github_output.read_bytes(), b"")

    def test_successful_update_is_terminal_without_repair(self):
        self.capture.mode = "update-succeeded"
        self.assertEqual(self.main(), 1)
        self.assertEqual(self.capture.refs[q.actual_context(self.env).probe], q.Q)
        self.assertFalse(any("--method" in x and x[5] == "DELETE" for x in self.capture.calls))
        self.assertFalse((self.out / "public").exists())

    def test_successful_delete_is_terminal_without_repair(self):
        self.capture.mode = "delete-succeeded"
        self.assertEqual(self.main(), 1)
        self.assertNotIn(q.actual_context(self.env).probe, self.capture.refs)
        self.assertEqual(sum("--method" in x and x[5] == "POST" for x in self.capture.calls), 1)

    def test_denial_with_changed_readback_is_not_success(self):
        self.capture.mode = "changed-after-denial"
        self.assertEqual(self.main(), 1)
        self.assertFalse(any("--method" in x and x[5] == "DELETE" for x in self.capture.calls))

    def test_ambiguous_create_retains_raw_response_and_never_retries(self):
        self.capture.mode = "ambiguous-post"
        self.assertEqual(self.main(), 1)
        self.assertEqual(sum("--method" in x and x[5] == "POST" for x in self.capture.calls), 1)
        failure = json.loads((self.out / "failure.json").read_bytes())
        self.assertFalse(failure["repair_or_retry"])
        last = failure["commands"][-1]
        self.assertIn(b"Unknown outcome", (self.out / "receipts" / f"{last['number']:02d}.stdout").read_bytes())

    def test_final_main_movement_refuses_before_public_statements(self):
        def move(argv):
            if "--method" in argv and argv[5] == "POST" and any(q.Q in arg for arg in argv):
                self.capture.refs["refs/heads/main"] = "e" * 40
        self.capture.on_call = move
        self.assertEqual(self.main(), 1)
        self.assertFalse((self.out / "public").exists())
        self.assertEqual(self.github_output.read_bytes(), b"")

    def test_real_negative_publisher_ambiguous_post_never_becomes_signed_success(self):
        self.capture.mode = "ambiguous-marker"
        self.assertEqual(self.main(), 1)
        writes = [x for x in self.capture.calls if "--method" in x and x[5] == "POST"]
        self.assertEqual(len(writes), 2)
        self.assertIn("sha=" + q.Q, writes[-1])
        self.assertEqual(self.github_output.read_bytes(), b"")
        self.assertFalse((self.out / "public").exists())
        self.assertEqual(sum(sha == q.Q for sha in self.capture.refs.values()), 1)

    def test_aggregate_call_cap_stops_original_operation_without_new_allowance(self):
        with mock.patch.object(q, "MAX_CALLS", 16):
            self.assertEqual(self.main(), 1)
        self.assertEqual(len(self.capture.calls), 16)
        self.assertEqual(self.github_output.read_bytes(), b"")
        self.assertFalse(any(x.startswith("refs/tags/" + policy.PREFIX + q.Q) for x in self.capture.refs))

    def test_tool_changed_after_create_refuses_no_repair(self):
        def change(argv):
            if "--method" in argv and argv[5] == "POST":
                self.gh.write_bytes(b"changed-binary")
        self.capture.on_call = change
        self.assertEqual(self.main(), 1)
        self.assertEqual(sum("--method" in x and x[5] == "POST" for x in self.capture.calls), 1)

    def test_synthetic_negative_diff_is_only_one_boolean(self):
        ctx = public.Context(q.Q, 123456789, 1, None, "workflow_dispatch", "refs/heads/main")
        before = q.synthetic(ctx, negative=False, cli=public)
        after = q.synthetic(ctx, negative=True, cli=public)
        self.assertEqual(before[0], after[0])
        one, two = json.loads(before[1]), json.loads(after[1])
        one["old_server_ok"]["boot"] = False
        self.assertEqual(one, two)
        snapshot = json.loads(after[0])
        self.assertFalse(public.accepted(public.validate_result(after[1], snapshot, ctx)))
        public.validate_evidence(after[2], after[1], snapshot, ctx)

    def test_wrong_context_cannot_reach_transport(self):
        real = q.synthetic
        def corrupt(ctx, *, negative, cli):
            one, two, three = real(ctx, negative=negative, cli=cli)
            value = json.loads(three)
            value["sha"] = H
            return one, two, q.canonical(value)
        with mock.patch.object(q, "synthetic", corrupt):
            self.assertEqual(self.main(), 1)
        self.assertTrue(all("-C" in row for row in self.capture.calls))

    def test_labelled_statement_is_not_a_normal_compatibility_result(self):
        self.assertEqual(self.main(), 0)
        statement = (self.out / "public/qualification-negative.json").read_bytes()
        ctx = public.Context(q.Q, 123456789, 1, None, "workflow_dispatch", "refs/heads/main")
        snapshot = json.loads(q.synthetic(ctx, negative=True, cli=public)[0])
        with self.assertRaises(public.proof.EvidenceError):
            public.validate_result(statement, snapshot, ctx)


class BoundaryTests(unittest.TestCase):
    def test_denial_requires_exact_operation_status_message_and_json_types(self):
        ref = f"refs/tags/{policy.PREFIX}probe-123-1-permissions"
        good = {"message": "Repository rule violations found\n\nCannot update this ref\n"}
        q.denied(source.runtime.Capture(1, http(422, good), b"gh bounded error"), operation="update", ref=ref)
        for code, status, value in ((0, 422, good), (1, 403, good),
            (1, 422, {"message": good["message"], "status": 422}),
            (1, 422, {"message": good["message"].replace("update", "delete")}),
            (1, 422, {"message": "Validation Failed"}),
            (1, 422, {"message": good["message"], "documentation_url": "https://evil.invalid"})):
            with self.subTest(value=value), self.assertRaises(Exception):
                q.denied(source.runtime.Capture(code, http(status, value), b""), operation="update", ref=ref)

    def test_recorder_returns_original_object_and_preserves_nonzero_bytes(self):
        with tempfile.TemporaryDirectory() as name:
            recorder = q.Recorder(Path(name), source.runtime)
            result = source.runtime.Capture(1, b"bounded refusal", b"private error")
            with mock.patch.object(source.runtime, "bounded_command", return_value=result) as capture:
                found = recorder(["gh", "api"], env={}, timeout=1, stdout_limit=100, stderr_limit=100)
            self.assertIs(found, result)
            capture.assert_called_once()
            self.assertEqual((Path(name) / "01.stdout").read_bytes(), result.stdout)
            self.assertEqual(recorder.rows[0]["stderr_sha256"], q.digest(result.stderr))
            self.assertNotIn("private error", repr(recorder.rows))

    def test_recorder_enforces_call_and_aggregate_caps_before_native_capture(self):
        with tempfile.TemporaryDirectory() as name:
            recorder = q.Recorder(Path(name), source.runtime)
            for field, value in (("rows", [{}] * q.MAX_CALLS), ("total", q.MAX_BYTES - 1)):
                recorder.rows, recorder.total = [], 0
                setattr(recorder, field, value)
                with mock.patch.object(source.runtime, "bounded_command", side_effect=AssertionError("native")), \
                     self.assertRaises(q.Refusal):
                    recorder(["gh"], env={}, timeout=1, stdout_limit=100, stderr_limit=100)

    def test_deadline_failure_uses_original_budget_and_no_native_call(self):
        now = [100.0]
        budget = source.OperationBudget(101.0, clock=lambda: now[0], capture=lambda *a, **k: self.fail("native"))
        now[0] = 101.0
        with self.assertRaises(source.TransportRefusal):
            budget.capture(["gh"], env={}, timeout=1, stdout_limit=100, stderr_limit=100)
        self.assertTrue(budget.failed)


CHECKOUT = "actions/checkout@11d5960a326750d5838078e36cf38b85af677262"
ATTEST = "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6"
UPLOAD = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
SUFFIX = "${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}"
BASE = "${{ runner.temp }}/compat-negative-qualification-" + SUFFIX


def workflow_check(value):
    assert set(value) == {"name", "on", "permissions", "concurrency", "env", "jobs"}
    assert value["on"] == {"workflow_dispatch": None}
    assert value["permissions"] == {}
    assert value["concurrency"] == {"group": "compat-negative-index-qualification", "cancel-in-progress": False}
    assert set(value["jobs"]) == {"qualify"}
    job = value["jobs"]["qualify"]
    assert set(job) == {"name", "if", "runs-on", "timeout-minutes", "permissions", "steps"}
    assert job["if"] == "${{ github.repository == 'medtechcorps-netizen/whatomate' && github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && github.run_attempt == 1 }}"
    assert job["permissions"] == {"contents": "write", "actions": "read", "attestations": "write", "id-token": "write"}
    assert job["runs-on"] == "ubuntu-24.04" and job["timeout-minutes"] == 18
    steps = job["steps"]
    assert [x.get("id") for x in steps] == ["checkout", "install-gh", "qualify", "sign-positive", "sign-negative", "diagnostic", None]
    assert all("continue-on-error" not in x for x in steps)
    for step, keys in zip(steps, (
        {"name", "id", "timeout-minutes", "uses", "with"},
        {"name", "id", "timeout-minutes", "run"},
        {"name", "id", "timeout-minutes", "env", "run"},
        {"name", "id", "if", "timeout-minutes", "uses", "with"},
        {"name", "id", "if", "timeout-minutes", "uses", "with"},
        {"name", "id", "if", "timeout-minutes", "uses", "with"},
        {"name", "if", "env", "run"})):
        assert set(step) == keys
    assert steps[0]["uses"] == CHECKOUT
    assert steps[0]["with"] == {"ref": "${{ github.sha }}", "fetch-depth": 0, "persist-credentials": False}
    assert steps[0]["timeout-minutes"] == steps[1]["timeout-minutes"] == 1
    assert value["env"] == {"PINNED_GH_VERSION": "2.98.0",
        "PINNED_GH_URL": "https://github.com/cli/cli/releases/download/v2.98.0/gh_2.98.0_linux_amd64.tar.gz",
        "PINNED_GH_SHA256": "3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de"}
    install = steps[1]["run"]
    assert "--connect-timeout 10 --max-time 20" in install and "--retry" not in install
    assert install.index("sha256sum --check --strict") < install.index("tar -xzf")
    assert "--proto '=https' --proto-redir '=https' --tlsv1.2" in install
    assert steps[2]["run"] == "/usr/bin/python3 -I -S -B release/compat/qualify_negative_index.py"
    assert steps[2]["env"] == {"GH_TOKEN": "${{ github.token }}"} and steps[2]["timeout-minutes"] == 11
    gate = "!cancelled() && steps.qualify.outcome == 'success' && steps.qualify.outputs.complete == 'true'"
    for i, label in ((3, "positive"), (4, "negative")):
        step = steps[i]
        assert step["uses"] == ATTEST and step["timeout-minutes"] == 1
        assert step["if"] == "${{ " + gate + (" && steps.sign-positive.outcome == 'success'" if i == 4 else "") + " }}"
        assert step["with"] == {"subject-path": BASE + "/public/qualification-" + label + ".json",
            "predicate-type": q.PREDICATE, "predicate-path": BASE + "/public/predicate-" + label + ".json",
            "push-to-registry": False, "github-token": "${{ github.token }}"}
    assert steps[5]["uses"] == UPLOAD and steps[5]["if"] == "${{ always() }}" and steps[5]["timeout-minutes"] == 1
    assert steps[5]["with"] == {"name": "negative-index-qualification-" + SUFFIX,
        "path": "\n".join(BASE + x for x in ("/public/", "/receipts/", "/result.json", "/failure.json")) + "\n",
        "if-no-files-found": "error", "retention-days": 7}
    assert steps[6]["if"] == "${{ always() }}"
    assert steps[6]["env"] == {"CHECKOUT": "${{ steps.checkout.outcome }}", "INSTALL": "${{ steps.install-gh.outcome }}",
        "QUALIFY": "${{ steps.qualify.outcome }}", "POSITIVE": "${{ steps.sign-positive.outcome }}",
        "NEGATIVE": "${{ steps.sign-negative.outcome }}", "DIAGNOSTIC": "${{ steps.diagnostic.outcome }}",
        "COMPLETE": "${{ steps.qualify.outputs.complete }}"}
    assert "if os.environ.get('COMPLETE') != 'true' or any(os.environ.get(name) != 'success' for name in names):" in steps[6]["run"]
    assert "('CHECKOUT', 'INSTALL', 'QUALIFY', 'POSITIVE', 'NEGATIVE', 'DIAGNOSTIC')" in steps[6]["run"]
    assert "secrets." not in repr(value) and "compat-result-" not in repr(value)


class WorkflowTests(unittest.TestCase):
    def document(self):
        return WorkflowYaml(PATH.read_text(encoding="utf-8")).parse()

    def test_exact_dedicated_workflow_contract(self):
        workflow_check(self.document())

    def test_authority_order_subject_and_token_mutants_refused(self):
        mutations = [lambda v: v["on"].update(push={}),
            lambda v: v["on"].update(workflow_dispatch={"inputs": {"sha": {"type": "string"}}}),
            lambda v: v["jobs"]["qualify"].update(**{"if": "${{ always() }}"}),
            lambda v: v["jobs"]["qualify"]["permissions"].update(administration="write"),
            lambda v: v["jobs"]["qualify"]["steps"][0]["with"].update(ref="main"),
            lambda v: v["jobs"]["qualify"]["steps"][0]["with"].update(**{"persist-credentials": True}),
            lambda v: v["jobs"]["qualify"]["steps"][2]["env"].update(GH_TOKEN="${{ secrets.ADMIN_TOKEN }}"),
            lambda v: v["jobs"]["qualify"]["steps"][2].update(**{"continue-on-error": True}),
            lambda v: v["jobs"]["qualify"]["steps"][3].update(**{"if": "${{ always() }}"}),
            lambda v: v["jobs"]["qualify"]["steps"][4].update(**{"if": "${{ steps.qualify.outcome == 'success' }}"}),
            lambda v: v["jobs"]["qualify"]["steps"][3]["with"].update(**{"subject-path": BASE + "/positive/compat-result.json"}),
            lambda v: v["jobs"]["qualify"]["steps"][4]["with"].update(**{"predicate-type": "https://slsa.dev/provenance/v1"}),
            lambda v: v["jobs"]["qualify"]["steps"][5]["with"].update(name="compat-result-" + SUFFIX),
            lambda v: v["jobs"]["qualify"]["steps"][6]["env"].pop("NEGATIVE"),
            lambda v: v["jobs"]["qualify"]["steps"].reverse()]
        for i, mutate in enumerate(mutations):
            with self.subTest(index=i):
                value = copy.deepcopy(self.document())
                mutate(value)
                with self.assertRaises(AssertionError):
                    workflow_check(value)

    def test_production_has_no_test_imports_or_provider_adoption_commands(self):
        tree = ast.parse(Path(q.__file__).read_bytes())
        names = [name.name for node in ast.walk(tree) if isinstance(node, ast.Import) for name in node.names]
        names += [node.module or "" for node in ast.walk(tree) if isinstance(node, ast.ImportFrom)]
        self.assertFalse(any(name.startswith("test_") for name in names))
        self.assertFalse(set(names) & {"ship", "ship_flow", "record_writer", "docker"})


if __name__ == "__main__":
    unittest.main()
