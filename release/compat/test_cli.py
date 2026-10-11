import contextlib
import copy
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import cli
import test_proof


SHA = "a" * 40
OLD = "b" * 40
LATEST = "c" * 40
TAG = "prod-20261010T234723Z-bbbbbbbb"
LATEST_TAG = "prod-20261011T234723Z-cccccccc"
DIGEST = "sha256:" + "d" * 64


def environment(event="push", ref="refs/heads/main", attempt="1"):
    return {"GITHUB_REPOSITORY": cli.common.REPOSITORY, "GITHUB_SHA": SHA,
            "GITHUB_WORKFLOW_SHA": SHA, "GITHUB_WORKFLOW_REF": cli._WORKFLOW + ref,
            "GITHUB_EVENT_NAME": event, "GITHUB_REF": ref,
            "GITHUB_RUN_ID": "123456", "GITHUB_RUN_ATTEMPT": attempt,
            "RUNNER_ENVIRONMENT": "github-hosted"}


def snapshot(ctx):
    old = {"release": TAG, "sha": OLD, "web_digest": DIGEST}
    latest = dict(old) if ctx.target is None else {"release": LATEST_TAG, "sha": LATEST, "web_digest": "sha256:" + "e" * 64}
    return {"version": 1, "sha": ctx.sha, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
            "requested_rollback_target": ctx.target, "latest": latest, "selected": old,
            "preview": {"kind": "none", "dormant": False, "blocked": False}}


def result(ctx):
    value = cli.proof.evaluate_proof(test_proof.observations())
    value.update(sha=ctx.sha, run_id=ctx.run_id, run_attempt=ctx.attempt,
                 previous_release=TAG, previous_web_digest=DIGEST)
    return value


class ContextTests(unittest.TestCase):
    def test_events_and_reruns_accept_actual_checked_out_identity(self):
        for event, ref in (("push", "refs/heads/main"), ("pull_request", "refs/pull/267/merge"),
                           ("workflow_dispatch", "refs/heads/codex/compat-fixture")):
            with self.subTest(event=event):
                value = cli.context(environment(event, ref, "2"))
                self.assertEqual(value.sha, SHA)
                self.assertEqual(value.attempt, 2)
                self.assertEqual(cli.proof.artifact_name(value.sha, value.attempt), f"compat-result-{SHA}-2")

    def test_invalid_context_and_shell_like_target_refuse(self):
        invalid = [("GITHUB_REPOSITORY", "elsewhere/repo"), ("RUNNER_ENVIRONMENT", "self-hosted"),
                   ("RUNNER_DEBUG", "1"), ("GITHUB_SHA", "A" * 40), ("GITHUB_RUN_ID", "01"),
                   ("GITHUB_RUN_ID", "10000000000000000"), ("GITHUB_RUN_ATTEMPT", "0"),
                   ("GITHUB_RUN_ATTEMPT", "1\n"), ("GITHUB_EVENT_NAME", "pull_request_target"),
                   ("GITHUB_REF", "refs/heads/other"), ("GITHUB_WORKFLOW_SHA", OLD),
                   ("GITHUB_WORKFLOW_REF", cli._WORKFLOW + "refs/heads/other"),
                   ("COMPAT_ROLLBACK_TARGET", TAG)]
        for key, item in invalid:
            env = environment(); env[key] = item
            with self.subTest(key=key, item=item), self.assertRaises(cli.proof.EvidenceError):
                cli.context(env)
        for item in ("$(whoami)", TAG + "\n", "prod-0000;command"):
            env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = item
            with self.assertRaises(cli.proof.EvidenceError): cli.context(env)

    def test_explicit_rollback_has_bounded_grammar(self):
        for item in (TAG, "prod-0000"):
            env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = item
            self.assertEqual(cli.context(env).target, item)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.ctx = cli.context(environment())

    def test_snapshot_context_selection_and_shape_refuse_mutations(self):
        original = snapshot(self.ctx)
        self.assertIs(cli.validate_snapshot(original, self.ctx), original)
        mutations = [lambda v: v.update(version=True), lambda v: v.update(sha=OLD),
                     lambda v: v.update(run_id=True), lambda v: v.update(run_attempt=2),
                     lambda v: v.update(extra=True), lambda v: v.update(requested_rollback_target=TAG),
                     lambda v: v["selected"].update(sha=LATEST),
                     lambda v: v["selected"].update(web_digest="tag:latest"),
                     lambda v: v["preview"].update(kind="contract"),
                     lambda v: v["preview"].update(dormant=True),
                     lambda v: v["preview"].update(blocked=0)]
        for mutation in mutations:
            changed = copy.deepcopy(original); mutation(changed)
            with self.assertRaises(cli.proof.EvidenceError): cli.validate_snapshot(changed, self.ctx)

    def test_single_chain_freezes_latest_and_explicit_selection(self):
        env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = TAG
        ctx = cli.context(env)
        old = SimpleNamespace(release=TAG, sha=OLD, images={"web": DIGEST})
        latest = SimpleNamespace(release=LATEST_TAG, sha=LATEST, images={"web": "sha256:" + "e" * 64})
        chain = cli.records.Chain((old, latest)); client = SimpleNamespace(work_dir=Path("/private"))
        source = mock.Mock()
        with mock.patch.object(cli.images, "_verification_transport", return_value=contextlib.nullcontext()), \
             mock.patch.object(cli.images, "_Source", return_value=source), \
             mock.patch.object(cli.records, "load_bootstrap", return_value="bootstrap"), \
             mock.patch.object(cli.records, "resolve_chain", return_value=chain) as resolve, \
             mock.patch.object(cli.records, "verify_images") as verify, \
             mock.patch.object(cli.schema_change, "classify", return_value=SimpleNamespace(kind="data", dormant=True, reasons=())) as classify:
            value = cli.preview(Path("/repo"), ctx, client)
        self.assertEqual(resolve.call_count, 1)
        self.assertEqual(verify.call_args_list, [mock.call(client, latest.images, LATEST, "bootstrap"),
                                                mock.call(client, old.images, OLD, "bootstrap")])
        classify.assert_called_once_with(Path("/repo"), LATEST, SHA, mode="forward")
        self.assertEqual(value["latest"]["sha"], LATEST)
        self.assertEqual(value["selected"]["sha"], OLD)

    def test_unknown_target_or_signature_failure_has_no_snapshot(self):
        env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = TAG
        ctx = cli.context(env)
        latest = SimpleNamespace(release=LATEST_TAG, sha=LATEST, images={"web": DIGEST})
        client = SimpleNamespace(work_dir=Path("/private"))
        with mock.patch.object(cli.images, "_verification_transport", return_value=contextlib.nullcontext()), \
             mock.patch.object(cli.images, "_Source"), mock.patch.object(cli.records, "load_bootstrap"), \
             mock.patch.object(cli.records, "resolve_chain", return_value=cli.records.Chain((latest,))), \
             mock.patch.object(cli.records, "verify_images") as verify:
            with self.assertRaises(cli.proof.EvidenceError): cli.preview(Path("/repo"), ctx, client)
            verify.assert_not_called()


class RunTests(unittest.TestCase):
    def setUp(self):
        self.ctx = cli.context(environment(attempt="2"))
        self.snap = snapshot(self.ctx)
        self.raw = cli.proof.result_bytes(result(self.ctx))

    def test_selected_tag_sha_and_locations_pass_to_real_verifier(self):
        client = object()
        options = dict(docker_binary="/docker", config_dir=Path("/config"),
                       image_work_dir=Path("/images"), buildx_binary=Path("/buildx"))
        with mock.patch.object(cli.compat, "run_experiment", return_value=self.raw) as run:
            self.assertEqual(cli.execute(Path("/repo"), self.ctx, client, self.snap, **options), self.raw)
        run.assert_called_once_with(Path("/repo"), SHA, self.ctx.run_id, 2, client, **options,
            rollback_target=TAG, expected_previous_sha=OLD)

    def test_result_bindings_noncanonical_and_extra_fields_refuse(self):
        mutations = [lambda v: v.update(sha=OLD), lambda v: v.update(run_attempt=1),
                     lambda v: v.update(run_id=7), lambda v: v.update(previous_release=LATEST_TAG),
                     lambda v: v.update(previous_web_digest="sha256:" + "e" * 64),
                     lambda v: v.update(hostname="must-never-publish")]
        for mutation in mutations:
            value = result(self.ctx); mutation(value)
            with self.assertRaises(cli.proof.EvidenceError):
                cli.validate_result(cli.canonical(value), self.snap, self.ctx)
        with self.assertRaises(cli.proof.EvidenceError): cli.validate_result(b" " + self.raw, self.snap, self.ctx)
        with self.assertRaises(cli.common.ReleaseError):
            cli.validate_result(self.raw.replace(b'"sha":', b'"sha":"' + OLD.encode() + b'","sha":'), self.snap, self.ctx)

    def test_semantic_false_is_actual_valid_evidence_but_fails_acceptance(self):
        value = result(self.ctx); value["old_server_ok"]["read"] = False
        parsed = cli.validate_result(cli.proof.result_bytes(value), self.snap, self.ctx)
        self.assertFalse(cli.accepted(parsed))
        self.assertTrue(cli.accepted(result(self.ctx)))

    def test_blocked_preview_stops_before_experiment(self):
        self.snap["preview"]["blocked"] = True
        with mock.patch.object(cli.compat, "run_experiment") as run:
            with self.assertRaises(cli.proof.EvidenceError):
                cli.execute(Path("/repo"), self.ctx, object(), self.snap, docker_binary="/docker",
                    config_dir=Path("/config"), image_work_dir=Path("/images"), buildx_binary=Path("/buildx"))
        run.assert_not_called()


class FileAndEntryTests(unittest.TestCase):
    def test_default_enabled_preview_uses_bound_resolution_and_canonical_output(self):
        for blocked in (False, True):
            with self.subTest(blocked=blocked), tempfile.TemporaryDirectory() as temp:
                root = Path(temp).resolve(); repo = root / "repo"; runner = root / "runner"
                repo.mkdir(); runner.mkdir()
                env = environment("pull_request", "refs/pull/267/merge", "2")
                env["RUNNER_TEMP"] = str(runner)
                ctx = cli.context(env)
                entry = SimpleNamespace(release=TAG, sha=OLD, images={"web": DIGEST})
                client = SimpleNamespace(work_dir=runner / "private-chain")
                source = mock.Mock()
                client_environments = []
                def client_for(work, actual_env):
                    client_environments.append(dict(actual_env))
                    return client
                with mock.patch.dict(cli.os.environ, env, clear=True), \
                     mock.patch.object(Path, "cwd", return_value=repo), \
                     mock.patch.object(cli, "_client", side_effect=client_for) as make_client, \
                     mock.patch.object(cli.images, "_verification_transport", return_value=contextlib.nullcontext()), \
                     mock.patch.object(cli.images, "_Source", return_value=source) as make_source, \
                     mock.patch.object(cli.records, "load_bootstrap", return_value="bootstrap"), \
                     mock.patch.object(cli.records, "resolve_chain", return_value=cli.records.Chain((entry,))) as resolve, \
                     mock.patch.object(cli.records, "verify_images") as verify, \
                     mock.patch.object(cli.schema_change, "classify", return_value=SimpleNamespace(
                         kind="none", dormant=False, reasons=("synthetic-block",) if blocked else ())), \
                     contextlib.redirect_stdout(io.StringIO()) as stdout, \
                     contextlib.redirect_stderr(io.StringIO()) as stderr:
                    status = cli.main(["preview"])
                work = runner / ("compat-preview-" + ctx.suffix)
                expected = snapshot(ctx); expected["preview"]["blocked"] = blocked
                self.assertEqual(status, int(blocked))
                self.assertEqual((work / "public" / "resolution.json").read_bytes(), cli.canonical(expected))
                self.assertEqual(stdout.getvalue(), ""); self.assertEqual(stderr.getvalue(), "")
                make_client.assert_called_once()
                self.assertEqual(make_client.call_args.args[0], work)
                self.assertEqual(client_environments, [env])
                make_source.assert_called_once_with(repo, SHA, mock.ANY)
                self.assertEqual(source.clean.call_count, 2)
                resolve.assert_called_once_with(client, repo, bootstrap="bootstrap", work_dir=client.work_dir)
                verify.assert_called_once_with(client, entry.images, OLD, "bootstrap")

    def test_client_version_setup_is_five_seconds_and_restores_transport(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            for name in ("gh-config", "gh-work"):
                (root / name).mkdir()
            binary = root / "gh"; binary.touch()
            env = {"COMPAT_GH_BIN": str(binary), "GH_TOKEN": "synthetic-not-a-credential"}
            with mock.patch.object(cli.runtime, "bounded_command",
                 return_value=cli.runtime.Capture(0, b"gh version 2.98.0 (2026-10-08)\n", b"")) as capture:
                client = cli._client(root, env)
                self.assertIs(client.capture, capture)
                self.assertEqual(capture.call_args.args[0], [str(binary), "--version"])
                self.assertEqual(capture.call_args.kwargs["timeout"], 5)

    def test_fresh_private_directories_and_create_only_bounded_public_file(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve(); repo = root / "repo"; scratch = root / "scratch"
            repo.mkdir(); scratch.mkdir(); ctx = cli.context(environment())
            work = cli._scratch(repo, ctx, "run", {"RUNNER_TEMP": str(scratch)})
            target = work / "public" / "compat-result.json"
            cli._write_public(target, b"{}\n")
            self.assertEqual(cli._read_public(target), b"{}\n")
            with self.assertRaises(cli.proof.EvidenceError): cli._write_public(target, b"{}\n")
            with self.assertRaises(cli.proof.EvidenceError):
                cli._scratch(repo, ctx, "run", {"RUNNER_TEMP": str(scratch)})
            with self.assertRaises(cli.proof.EvidenceError):
                cli._scratch(repo, ctx, "run", {"RUNNER_TEMP": str(repo)})
            with self.assertRaises(cli.proof.EvidenceError): cli._write_public(work / "public" / "huge", b"x" * 16385)

    def test_disabled_main_has_no_side_effect_or_private_error_echo(self):
        with mock.patch.object(cli.images, "_READY_FOR_EXECUTION", False), \
             mock.patch.object(cli, "_scratch") as scratch, \
             mock.patch.object(cli, "_client") as client, contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertEqual(cli.main(["run"]), 1)
        self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n")
        scratch.assert_not_called(); client.assert_not_called()

    def test_bad_or_extra_arguments_never_echo_input_or_exit_parser(self):
        for args in ([], ["accidental-private-token"], ["run", "private/path"], ["--help"], [None], "run"):
            with contextlib.redirect_stderr(io.StringIO()) as stderr, \
                 contextlib.redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(cli.main(args), 1)
            self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n")
            self.assertEqual(stdout.getvalue(), "")

    def test_aggregator_requires_both_real_job_outcomes_before_result_read(self):
        env = environment(); env.update(COMPAT_PREVIEW_OUTCOME="success", COMPAT_RUN_OUTCOME="failure")
        ctx = cli.context(env)
        with tempfile.TemporaryDirectory() as temp:
            env["RUNNER_TEMP"] = str(Path(temp).resolve())
            with mock.patch.dict(cli.os.environ, env, clear=True), \
                 mock.patch.object(cli.images, "_READY_FOR_EXECUTION", True), \
                 mock.patch.object(cli, "_read_public", return_value=cli.canonical(snapshot(ctx))) as read, \
                 mock.patch.object(cli, "_write_public") as write, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(cli.main(["check"]), 1)
            self.assertEqual(read.call_count, 1)
            write.assert_not_called()

    def test_aggregator_preserves_semantic_false_before_failing_gate(self):
        env = environment(); env.update(COMPAT_PREVIEW_OUTCOME="success", COMPAT_RUN_OUTCOME="success")
        ctx = cli.context(env); value = result(ctx); value["old_server_ok"]["reverse"] = False
        raw = cli.proof.result_bytes(value)
        with tempfile.TemporaryDirectory() as temp:
            env["RUNNER_TEMP"] = str(Path(temp).resolve())
            output = Path(temp).resolve() / "step-output"; output.touch()
            env["GITHUB_OUTPUT"] = str(output)
            with mock.patch.dict(cli.os.environ, env, clear=True), \
                 mock.patch.object(cli.images, "_READY_FOR_EXECUTION", True), \
                 mock.patch.object(cli, "_read_public", side_effect=[cli.canonical(snapshot(ctx)), raw]), \
                 mock.patch.object(cli, "_scratch", return_value=Path(temp).resolve()) as scratch, \
                 mock.patch.object(cli, "_write_public") as write:
                self.assertEqual(cli.main(["check"]), 1)
            self.assertEqual(scratch.call_args.args[2], "check")
            self.assertEqual(write.call_args.args[1], raw)
            self.assertEqual(output.read_text(), "complete=true\n")

    def test_runner_does_not_write_result_on_infrastructure_exception(self):
        env = environment(); ctx = cli.context(env)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve(); env["RUNNER_TEMP"] = str(root)
            for name in ("docker", "buildx"):
                (root / name).touch(); env["COMPAT_" + ("DOCKER" if name == "docker" else "BUILDX") + "_BIN"] = str(root / name)
            with mock.patch.dict(cli.os.environ, env, clear=True), \
                 mock.patch.object(cli.images, "_READY_FOR_EXECUTION", True), \
                 mock.patch.object(cli, "_read_public", return_value=cli.canonical(snapshot(ctx))), \
                 mock.patch.object(cli, "_scratch", return_value=root), mock.patch.object(cli, "_client"), \
                 mock.patch.object(cli, "execute", side_effect=cli.proof.InfrastructureFailure("private-token-should-not-print")), \
                 mock.patch.object(cli, "_write_public") as write, contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(cli.main(["run"]), 1)
            write.assert_not_called()
            self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n")

    def test_partial_public_write_never_marks_complete(self):
        env = environment(); env.update(COMPAT_PREVIEW_OUTCOME="success", COMPAT_RUN_OUTCOME="success")
        ctx = cli.context(env); raw = cli.proof.result_bytes(result(ctx))
        with tempfile.TemporaryDirectory() as temp:
            env["RUNNER_TEMP"] = str(Path(temp).resolve())
            with mock.patch.dict(cli.os.environ, env, clear=True), \
                 mock.patch.object(cli.images, "_READY_FOR_EXECUTION", True), \
                 mock.patch.object(cli, "_read_public", side_effect=[cli.canonical(snapshot(ctx)), raw]), \
                 mock.patch.object(cli, "_scratch", return_value=Path(temp).resolve()), \
                 mock.patch.object(cli, "_write_public", side_effect=OSError("private")), \
                 mock.patch.object(cli, "_mark_complete") as mark, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(cli.main(["check"]), 1)
            mark.assert_not_called()


if __name__ == "__main__":
    unittest.main()
