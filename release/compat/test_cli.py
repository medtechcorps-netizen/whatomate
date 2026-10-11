import contextlib
import copy
import io
import json
import hashlib
from pathlib import Path
import tempfile
import types
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
    old = {"release": TAG, "manifest_sha256": "1" * 64, "sha": OLD, "web_digest": DIGEST}
    latest = dict(old) if ctx.target is None else {"release": LATEST_TAG, "manifest_sha256": "2" * 64, "sha": LATEST, "web_digest": "sha256:" + "e" * 64}
    return {"version": 3, "sha": ctx.sha, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
            "requested_rollback_target": ctx.target, "latest": latest, "selected": old,
            "database_base": {"mode": "legacy-source-only", "source_sha": latest["sha"],
                              "origin": None, "catalog": None},
            "selected_web_source_sha": OLD,
            "preview": {"kind": "none", "dormant": False, "blocked": False}}


def resolved(ctx, *, blocked=False):
    """Synthetic projection for CLI wiring tests; not authentication evidence."""
    r, p = cli.record_resolver, cli.record_resolver.policy
    snap = snapshot(ctx)
    def selection(name, index):
        row = snap[name]
        images = tuple((key, row["web_digest"] if key == "web" else "sha256:" + "8" * 64)
                       for key in cli.common.COMPONENTS)
        return r.ReleaseSelection(index, row["release"], row["sha"], row["manifest_sha256"],
            images, row["sha"], p.legacy_reader0(row["sha"]), None)
    latest = selection("latest", 1)
    selected = latest if ctx.target is None else selection("selected", 0)
    chain = r.ResolvedChain(None, None, (), latest, selected)
    classification = cli.schema_change.PolicyClassification("none", False,
        ("synthetic-block",) if blocked else (), (), (), ())
    return r.ForwardResolution(chain, p.legacy_reader0(ctx.sha), classification,
        b"synthetic", "0" * 64, selected.actual_web_source_sha, latest.record_sha, None, None)


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
        ctx = cli.context(env); resolution = resolved(ctx)
        client = object(); deadline = cli.images._Deadline(); binary = Path("/git")
        with mock.patch.object(cli.record_resolver, "resolve_forward", return_value=resolution) as resolve:
            value = cli.preview(Path("/repo"), ctx, client, git_binary=binary, deadline=deadline)
        resolve.assert_called_once_with(client, Path("/repo"), binary, SHA, selected_tag=TAG)
        self.assertEqual(value["latest"]["sha"], LATEST)
        self.assertEqual(value["selected"]["sha"], OLD)
        self.assertEqual(value["version"], 3)
        self.assertEqual(value["selected"]["manifest_sha256"], "1" * 64)
        self.assertEqual(value["database_base"]["source_sha"], LATEST)
        self.assertEqual(value["selected_web_source_sha"], OLD)
        self.assertTrue(value["preview"]["blocked"])  # Dedicated target proof is not the normal lane.

    def test_unknown_target_or_signature_failure_has_no_snapshot(self):
        env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = TAG
        ctx = cli.context(env)
        with mock.patch.object(cli.record_resolver, "resolve_forward", side_effect=cli.common.ReleaseError("synthetic")):
            with self.assertRaises(cli.common.ReleaseError):
                cli.preview(Path("/repo"), ctx, object(), git_binary=Path("/git"), deadline=cli.images._Deadline())


class RunTests(unittest.TestCase):
    def setUp(self):
        self.ctx = cli.context(environment(attempt="2"))
        self.snap = snapshot(self.ctx)
        self.raw = cli.proof.result_bytes(result(self.ctx))

    def test_fresh_full_resolution_and_same_deadline_pass_to_engine(self):
        client = object(); resolution = resolved(self.ctx); deadline = cli.images._Deadline()
        options = dict(docker_binary="/docker", config_dir=Path("/config"),
                       image_work_dir=Path("/images"), buildx_binary=Path("/buildx"))
        with mock.patch.object(cli.record_resolver, "resolve_forward", return_value=resolution) as resolve, \
             mock.patch.object(cli.compat, "_run_resolved_experiment", return_value=self.raw, create=True) as run:
            self.assertEqual(cli.execute(Path("/repo"), self.ctx, client, self.snap,
                git_binary=Path("/git"), deadline=deadline, **options), self.raw)
        resolve.assert_called_once_with(client, Path("/repo"), Path("/git"), SHA)
        run.assert_called_once_with(Path("/repo"), resolution, self.ctx.run_id, 2, deadline=deadline, **options)

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
                    config_dir=Path("/config"), image_work_dir=Path("/images"), buildx_binary=Path("/buildx"),
                    git_binary=Path("/git"), deadline=cli.images._Deadline())
        run.assert_not_called()


class FileAndEntryTests(unittest.TestCase):
    def test_default_enabled_preview_uses_bound_resolution_and_canonical_output(self):
        for blocked in (False, True):
            with self.subTest(blocked=blocked), tempfile.TemporaryDirectory() as temp:
                root = Path(temp).resolve(); repo = root / "repo"; runner = root / "runner"
                repo.mkdir(); runner.mkdir()
                env = environment("pull_request", "refs/pull/267/merge", "2")
                env["RUNNER_TEMP"] = str(runner)
                ctx = cli.context(env); resolution = resolved(ctx, blocked=blocked); client = object()
                client_environments = []; deadlines = []
                def client_for(work, actual_env, deadline):
                    client_environments.append(dict(actual_env)); deadlines.append(deadline)
                    self.assertLessEqual(deadline.remaining(), 600)
                    return client
                with mock.patch.dict(cli.os.environ, env, clear=True), \
                     mock.patch.object(Path, "cwd", return_value=repo), \
                     mock.patch.object(cli, "_git_binary", return_value=Path("/git")), \
                     mock.patch.object(cli, "_client", side_effect=client_for) as make_client, \
                     mock.patch.object(cli.record_resolver, "resolve_forward", return_value=resolution) as resolve, \
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
                self.assertIs(make_client.call_args.args[2], deadlines[0])
                resolve.assert_called_once_with(client, repo, Path("/git"), SHA, selected_tag=None)

    def test_preview_complete_chain_and_classification_share_original_six_hundred_seconds(self):
        # Synthetic elapsed work crosses the old cap. Real deadline, client and
        # OperationBudget are used; only external capture/resolution are faked.
        for elapsed, expires in ((320, False), (601, True)):
            with self.subTest(elapsed=elapsed), tempfile.TemporaryDirectory() as temp:
                root = Path(temp).resolve(); repo = root / "repo"; runner = root / "runner"
                repo.mkdir(); runner.mkdir(); binary = root / "gh"; binary.touch()
                env = environment("pull_request", "refs/pull/269/merge")
                env.update(RUNNER_TEMP=str(runner), COMPAT_GH_BIN=str(binary), GH_TOKEN="synthetic-token")
                ctx = cli.context(env); clock = [100.0]; deadlines = []; calls = []
                original_init, original_scratch = cli.images._Deadline.__init__, cli._scratch
                def init(deadline):
                    original_init(deadline, clock=lambda: clock[0]); deadlines.append(deadline)
                def scratch(*args):
                    clock[0] += 30  # Local setup also consumes the original deadline.
                    return original_scratch(*args)
                def capture(argv, **options):
                    calls.append((argv, options)); clock[0] += 2
                    return cli.runtime.Capture(0, b"gh version 2.98.0 (2026-10-08)\n", b"")
                def resolve(client, actual_repo, git_binary, sha, *, selected_tag):
                    self.assertEqual((actual_repo, sha, selected_tag), (repo, ctx.sha, None))
                    self.assertEqual(clock[0], 132)
                    self.assertEqual(client.budget._deadline, 700)
                    self.assertEqual(client.budget.remaining(), 568)
                    clock[0] = 100 + elapsed
                    # Even a source boundary returning after expiry must not
                    # publish. preview's final original-deadline check owns it.
                    if not expires: client.budget.close()
                    return resolved(ctx)
                with mock.patch.dict(cli.os.environ, env, clear=True), \
                     mock.patch.object(Path, "cwd", return_value=repo), \
                     mock.patch.object(cli.images._Deadline, "__init__", init), \
                     mock.patch.object(cli, "_scratch", side_effect=scratch), \
                     mock.patch.object(cli, "_git_binary", return_value=Path("/synthetic-git")), \
                     mock.patch.object(cli.runtime, "bounded_command", side_effect=capture), \
                     mock.patch.object(cli.record_resolver, "resolve_forward", side_effect=resolve), \
                     mock.patch.object(cli, "_mark_complete") as marker, \
                     contextlib.redirect_stdout(io.StringIO()) as stdout, \
                     contextlib.redirect_stderr(io.StringIO()) as stderr:
                    status = cli.main(["preview"])
                self.assertEqual(status, int(expires)); self.assertEqual(len(deadlines), 1)
                self.assertEqual(deadlines[0].end, 700)
                self.assertEqual(len(calls), 1); self.assertEqual(calls[0][1]["timeout"], 5)
                target = runner / ("compat-preview-" + ctx.suffix) / "public/resolution.json"
                self.assertEqual(target.exists(), not expires); marker.assert_not_called()
                self.assertEqual(stdout.getvalue(), "")
                if expires: self.assertIn("admission-deadline", stderr.getvalue())
                else: self.assertEqual(target.read_bytes(), cli.canonical(snapshot(ctx)))

    def test_client_version_setup_is_inside_shared_deadline_and_five_seconds(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            for name in ("gh-config", "gh-work"):
                (root / name).mkdir()
            binary = root / "gh"; binary.touch()
            env = {"COMPAT_GH_BIN": str(binary), "GH_TOKEN": "synthetic-not-a-credential"}
            deadline = cli.images._Deadline()
            with mock.patch.object(cli.runtime, "bounded_command",
                 return_value=cli.runtime.Capture(0, b"gh version 2.98.0 (2026-10-08)\n", b"")) as capture:
                client = cli._client(root, env, deadline)
                self.assertIs(type(client), cli.record_transport.ReleaseGh)
                self.assertIs(client.budget.clock, deadline.clock)
                self.assertEqual(client.budget.commands, 1)
                self.assertLessEqual(client.budget._deadline, deadline.end)
                self.assertEqual(capture.call_args.args[0], [str(binary), "--version"])
                self.assertEqual(capture.call_args.kwargs["timeout"], 5)
                client.budget.close()

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
        self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n"
            "compatibility-diagnostic: phase=cli-entry family=evidence code=cli-experiment-disabled; chain=complete\n")
        scratch.assert_not_called(); client.assert_not_called()

    def test_bad_or_extra_arguments_never_echo_input_or_exit_parser(self):
        for args in ([], ["accidental-private-token"], ["run", "private/path"], ["--help"], [None], "run"):
            with contextlib.redirect_stderr(io.StringIO()) as stderr, \
                 contextlib.redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(cli.main(args), 1)
            self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n"
                "compatibility-diagnostic: phase=cli-entry family=evidence code=cli-command-invalid; chain=complete\n")
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
                 mock.patch.object(cli, "_git_binary", return_value=Path("/synthetic-git")), \
                 mock.patch.object(cli, "execute", side_effect=cli.proof.InfrastructureFailure("private-token-should-not-print")), \
                 mock.patch.object(cli, "_write_public") as write, contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(cli.main(["run"]), 1)
            write.assert_not_called()
            self.assertEqual(stderr.getvalue(), "compatibility-entrypoint-refused\n"
                "compatibility-diagnostic: phase=cli-entry family=infrastructure code=unknown; chain=complete\n")

    def test_safe_diagnostic_never_publishes_a_result_or_formats_private_errors(self):
        class PrivateError(cli.proof.InfrastructureFailure):
            def __str__(self):
                raise AssertionError("private exception text must not be formatted")
        cases = [
            (cli.proof.EvidenceError("admission-credential-helper-available"),
             "compatibility-entrypoint-refused: admission-credential-helper-available\n"),
            (cli.proof.InfrastructureFailure("admission-builder-ownership-unresolved"),
             "compatibility-entrypoint-refused: admission-builder-ownership-unresolved\n"),
            (PrivateError("admission-private-secret"), "compatibility-entrypoint-refused\n"),
            (PrivateError("admission-build-failed", "private-token"), "compatibility-entrypoint-refused\n"),
            (ValueError("admission-build-failed"), "compatibility-entrypoint-refused\n"),
        ]
        env = environment(); ctx = cli.context(env)
        for error, expected in cases:
            with self.subTest(kind=type(error).__name__), tempfile.TemporaryDirectory() as temp:
                root = Path(temp).resolve(); env["RUNNER_TEMP"] = str(root)
                for name in ("docker", "buildx"):
                    (root / name).touch()
                    env["COMPAT_" + ("DOCKER" if name == "docker" else "BUILDX") + "_BIN"] = str(root / name)
                with mock.patch.dict(cli.os.environ, env, clear=True), \
                     mock.patch.object(cli, "_read_public", return_value=cli.canonical(snapshot(ctx))), \
                     mock.patch.object(cli, "_scratch", return_value=root), mock.patch.object(cli, "_client"), \
                     mock.patch.object(cli, "_git_binary", return_value=Path("/synthetic-git")), \
                 mock.patch.object(cli, "execute", side_effect=error), \
                     mock.patch.object(cli, "_write_public") as write, \
                     mock.patch.object(cli, "_mark_complete") as mark, \
                     contextlib.redirect_stderr(io.StringIO()) as stderr:
                    self.assertEqual(cli.main(["run"]), 1)
                write.assert_not_called(); mark.assert_not_called()
                lines = stderr.getvalue().splitlines()
                self.assertEqual(lines[0] + "\n", expected)
                self.assertEqual(len(lines), 2)
                self.assertTrue(lines[1].startswith("compatibility-diagnostic: phase=cli-entry family="))
                self.assertLess(len(lines[1]), 1024)
                self.assertNotIn("private", lines[1])

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


class EvidenceContextTests(unittest.TestCase):
    def setUp(self):
        self.ctx = cli.context(environment(attempt="2"))
        self.snap = snapshot(self.ctx)
        self.raw = cli.proof.result_bytes(result(self.ctx))

    def test_positive_and_false_context_bind_exact_unchanged_result_bytes(self):
        for negative in (False, True):
            value = result(self.ctx)
            if negative:
                value["old_migrate_ok"]["ro"] = False
                value["reversible"] = False
            raw = cli.proof.result_bytes(value)
            bound = cli.evidence_context(raw, self.snap, self.ctx)
            self.assertEqual(bound, {"version": 2, "repository": cli.common.REPOSITORY,
                "workflow_path": ".github/workflows/compat.yml", "workflow_sha": SHA,
                "event": "push", "ref": "refs/heads/main", "sha": SHA,
                "run_id": self.ctx.run_id, "run_attempt": 2, "requested_rollback_target": None,
                "latest": self.snap["latest"], "selected": self.snap["selected"],
                "preview": self.snap["preview"], "database_base": self.snap["database_base"],
                "selected_web_source_sha": self.snap["selected_web_source_sha"],
                "result_sha256": hashlib.sha256(raw).hexdigest()})
            encoded = cli.canonical(bound)
            self.assertLess(len(encoded), 4096)
            self.assertEqual(cli.validate_evidence(encoded, raw, self.snap, self.ctx), bound)
            self.assertEqual(raw, cli.proof.result_bytes(value))
            self.assertNotIn("result_sha256", value)
            self.assertEqual(cli.accepted(value), not negative)

    def test_every_context_field_and_nested_identity_is_rebound(self):
        original = cli.evidence_context(self.raw, self.snap, self.ctx)
        changes = [(key, "foreign") for key in original]
        changes += [("version", True), ("run_id", True), ("run_attempt", True),
                    ("event", "workflow_dispatch"), ("ref", "refs/heads/other"),
                    ("sha", OLD), ("workflow_sha", OLD), ("requested_rollback_target", TAG)]
        for key, value in changes:
            altered = copy.deepcopy(original); altered[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(cli.proof.EvidenceError):
                cli.validate_evidence(cli.canonical(altered), self.raw, self.snap, self.ctx)
        for key in original:
            altered = copy.deepcopy(original); del altered[key]
            with self.subTest(missing=key), self.assertRaises(cli.proof.EvidenceError):
                cli.validate_evidence(cli.canonical(altered), self.raw, self.snap, self.ctx)
        for which in ("latest", "selected"):
            for key in original[which]:
                altered = copy.deepcopy(original); altered[which][key] = "f" * 64
                with self.subTest(which=which, key=key), self.assertRaises(cli.proof.EvidenceError):
                    cli.validate_evidence(cli.canonical(altered), self.raw, self.snap, self.ctx)
        altered = copy.deepcopy(original); altered["extra"] = False
        with self.assertRaises(cli.proof.EvidenceError):
            cli.validate_evidence(cli.canonical(altered), self.raw, self.snap, self.ctx)
        for key in ("kind", "dormant", "blocked"):
            altered = copy.deepcopy(original); altered["preview"][key] = "bad"
            with self.assertRaises(cli.proof.EvidenceError):
                cli.validate_evidence(cli.canonical(altered), self.raw, self.snap, self.ctx)

    def test_strict_context_bytes_and_digest_do_not_accept_equivalent_or_foreign_input(self):
        raw = cli.canonical(cli.evidence_context(self.raw, self.snap, self.ctx))
        cases = [b" " + raw, raw[:-1], raw + b" " * 4096, b"\xff", b'{"version":1,"version":1}',
                 b'{"value":NaN}', b'{"value":Infinity}', b'[]\n', raw.replace(b'"version":2', b'"version":2.0')]
        for case in cases:
            with self.subTest(raw=case[:32]), self.assertRaises((cli.proof.EvidenceError, cli.common.ReleaseError)):
                cli.validate_evidence(case, self.raw, self.snap, self.ctx)
        changed = result(self.ctx); changed["old_server_ok"]["read"] = False
        with self.assertRaises(cli.proof.EvidenceError):
            cli.validate_evidence(raw, cli.proof.result_bytes(changed), self.snap, self.ctx)

    def test_resolution_v1_or_missing_manifest_hash_cannot_be_upgraded_implicitly(self):
        for mutate in (lambda s: s.update(version=1),
                       lambda s: s["latest"].pop("manifest_sha256"),
                       lambda s: s["selected"].update(manifest_sha256=True),
                       lambda s: s["selected"].update(manifest_sha256="A" * 64)):
            snap = copy.deepcopy(self.snap); mutate(snap)
            with self.assertRaises(cli.proof.EvidenceError): cli.validate_snapshot(snap, self.ctx)

    def test_normal_classification_mismatch_and_explicit_target_refuse(self):
        value = result(self.ctx); value["classification"] = {"kind": "data", "dormant": True}
        with self.assertRaises(cli.proof.EvidenceError):
            cli.evidence_context(cli.proof.result_bytes(value), self.snap, self.ctx)
        for target in (TAG, "prod-0000"):
            env = environment("workflow_dispatch"); env["COMPAT_ROLLBACK_TARGET"] = target
            ctx = cli.context(env); snap = snapshot(ctx); snap["selected"]["release"] = target
            value = result(ctx); value["previous_release"] = target
            with self.assertRaisesRegex(cli.proof.EvidenceError, "cli-target-proof-unsupported"):
                cli.evidence_context(cli.proof.result_bytes(value), snap, ctx)

    def test_events_and_attempts_are_independently_rebound_without_new_capabilities(self):
        for event, ref in (("push", "refs/heads/main"), ("pull_request", "refs/pull/268/merge"),
                           ("workflow_dispatch", "refs/heads/codex/fixture")):
            env = environment(event, ref, "3"); ctx = cli.context(env); snap = snapshot(ctx)
            raw = cli.proof.result_bytes(result(ctx)); bound = cli.evidence_context(raw, snap, ctx)
            self.assertEqual((bound["event"], bound["ref"], bound["run_attempt"]), (event, ref, 3))
            with self.assertRaises(cli.proof.EvidenceError):
                cli.validate_evidence(cli.canonical(bound), self.raw, self.snap, self.ctx)


class EvidenceEntryTests(unittest.TestCase):
    @contextlib.contextmanager
    def workspace(self, negative=False):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve(); repo = root / "repo"; repo.mkdir()
            runner = root / "runner"; runner.mkdir(); inputs = runner / "compat-input"; inputs.mkdir()
            output = runner / "step-output"; output.touch()
            env = environment(attempt="2")
            env.update(RUNNER_TEMP=str(runner), GITHUB_OUTPUT=str(output),
                       COMPAT_PREVIEW_OUTCOME="success", COMPAT_RUN_OUTCOME="success")
            ctx = cli.context(env); snap = snapshot(ctx); value = result(ctx)
            if negative: value["old_server_ok"]["reverse"] = False
            raw = cli.proof.result_bytes(value)
            (inputs / "resolution.json").write_bytes(cli.canonical(snap))
            (inputs / "compat-result.json").write_bytes(raw)
            with mock.patch.dict(cli.os.environ, env, clear=True), mock.patch.object(Path, "cwd", return_value=repo), \
                 mock.patch.object(cli, "_client", side_effect=AssertionError("no client")), \
                 mock.patch.object(cli.compat, "run_experiment", side_effect=AssertionError("no experiment")), \
                 mock.patch.object(cli.records, "resolve_chain", side_effect=AssertionError("no moving latest")), \
                 contextlib.redirect_stdout(io.StringIO()) as stdout, contextlib.redirect_stderr(io.StringIO()) as stderr:
                yield runner, inputs, output, ctx, snap, raw, stdout, stderr

    def test_real_pair_publication_and_independent_revalidation_preserve_negative_verdict(self):
        for negative in (False, True):
            with self.subTest(negative=negative), self.workspace(negative) as state:
                runner, inputs, output, ctx, snap, raw, stdout, stderr = state
                self.assertEqual(cli.main(["check"]), int(negative))
                self.assertEqual(output.read_text(), "complete=true\n")
                public = runner / ("compat-check-" + ctx.suffix) / "public"
                self.assertEqual({p.name for p in public.iterdir()}, {"compat-result.json", "compat-context.json"})
                self.assertEqual((public / "compat-result.json").read_bytes(), raw)
                context_raw = (public / "compat-context.json").read_bytes()
                cli.validate_evidence(context_raw, raw, snap, ctx)
                (inputs / "compat-context.json").write_bytes(context_raw); output.write_bytes(b"")
                self.assertEqual(cli.main(["validate-evidence"]), 0)
                checked = runner / ("compat-validate-evidence-" + ctx.suffix) / "public"
                self.assertEqual({p.name: p.read_bytes() for p in checked.iterdir()},
                                 {p.name: p.read_bytes() for p in public.iterdir()})
                self.assertEqual(output.read_text(), "complete=true\n")
                self.assertEqual(cli.accepted(json.loads(raw)), not negative)
                self.assertEqual(stdout.getvalue(), ""); self.assertEqual(stderr.getvalue(), "")

    def test_signer_refuses_missing_extra_tampered_and_wrong_attempt_evidence(self):
        for case in ("missing", "extra", "wrong-hash", "changed-result", "wrong-attempt", "oversize"):
            with self.subTest(case=case), self.workspace() as state:
                runner, inputs, output, ctx, snap, raw, _, _ = state
                bound = cli.evidence_context(raw, snap, ctx)
                if case == "wrong-hash": bound["result_sha256"] = "0" * 64
                if case == "wrong-attempt": bound["run_attempt"] = 1
                if case != "missing": (inputs / "compat-context.json").write_bytes(cli.canonical(bound))
                if case == "extra": (inputs / "unexpected.json").write_bytes(b"{}")
                if case == "oversize": (inputs / "compat-context.json").write_bytes(b" " * 4097)
                if case == "changed-result":
                    value = result(ctx); value["old_server_ok"]["workers"] = False
                    (inputs / "compat-result.json").write_bytes(cli.proof.result_bytes(value))
                self.assertEqual(cli.main(["validate-evidence"]), 1)
                self.assertEqual(output.read_bytes(), b"")
                self.assertFalse((runner / ("compat-validate-evidence-" + ctx.suffix)).exists())

    def test_context_write_or_readback_failure_never_marks_complete(self):
        for case in ("partial-context", "tampered-context", "tampered-result"):
            with self.subTest(case=case), self.workspace() as state:
                runner, _, output, ctx, _, _, _, _ = state
                real_write = cli._write_public
                def write(path, raw):
                    if path.name == "compat-context.json":
                        if case == "partial-context":
                            path.write_bytes(raw[:1]); raise OSError("private detail")
                        real_write(path, raw)
                        if case == "tampered-context": path.write_bytes(raw[:-1])
                        if case == "tampered-result": (path.parent / "compat-result.json").write_bytes(b"{}\n")
                    else: real_write(path, raw)
                with mock.patch.object(cli, "_write_public", side_effect=write):
                    self.assertEqual(cli.main(["check"]), 1)
                self.assertEqual(output.read_bytes(), b"")
                self.assertTrue((runner / ("compat-check-" + ctx.suffix) / "public/compat-result.json").exists())

    def test_evidence_path_guards_refuse_link_or_junction_before_read(self):
        with self.workspace() as state:
            _, inputs, output, ctx, snap, raw, _, _ = state
            target = inputs / "compat-context.json"
            target.write_bytes(cli.canonical(cli.evidence_context(raw, snap, ctx)))
            original = Path.is_symlink
            with mock.patch.object(Path, "is_symlink", lambda p: p == target or original(p)):
                self.assertEqual(cli.main(["validate-evidence"]), 1)
            self.assertEqual(output.read_bytes(), b"")
            with mock.patch.object(Path, "is_junction", lambda p: p == inputs, create=True):
                self.assertEqual(cli.main(["validate-evidence"]), 1)
            self.assertEqual(output.read_bytes(), b"")


class DiagnosticTests(unittest.TestCase):
    def test_source_and_transport_fixed_codes_require_exact_owners(self):
        owners = ((cli.record_resolver.source_git.SourceGitRefusal,
                   ("git-failed:source-deadline", "git-failed:source-budget",
                    "git-failed:source-capture", "git-failed:source-command")),
                  (cli.record_transport.TransportRefusal,
                   ("record-chain-invalid:transport-deadline", "record-chain-invalid:transport-bound",
                    "record-chain-invalid:transport-capture", "record-chain-invalid:transport-resolver")))
        for owner, codes in owners:
            for code in codes:
                self.assertEqual(cli._diagnostic_code(owner(code)), code)
                self.assertEqual(cli._failure_message(owner(code)), "compatibility-entrypoint-refused")
                self.assertEqual(cli._diagnostic_code(cli.common.ReleaseError(code)), "unknown")
                self.assertEqual(cli._diagnostic_code(cli.proof.EvidenceError(code)), "unknown")
            class Foreign(owner): pass
            self.assertEqual(cli._diagnostic_code(Foreign(codes[0])), "unknown")
            for code in (codes[0]+":private/path", "git-failed:source-private-token"):
                text = cli._failure_diagnostic(owner(code))
                self.assertIn("code=unknown", text); self.assertNotIn("private", text)

    def test_real_budget_failure_reports_fixed_phase_without_capture_or_raw_text(self):
        clock = [100.0]
        def private_capture(*args, **kwargs):
            raise OSError("private token path command output")
        budget = cli.record_transport.OperationBudget(700, capture=private_capture, clock=lambda:clock[0])
        try:
            budget.capture(["private-command"], env={}, timeout=30, stdout_limit=128, stderr_limit=128)
        except cli.record_transport.TransportRefusal as error:
            text = cli._failure_diagnostic(error)
        else: self.fail("capture must refuse")
        self.assertIn("phase=release-capture family=release code=record-chain-invalid:transport-capture", text)
        self.assertNotIn("private", text)
        clock[0] = 100
        budget = cli.record_transport.OperationBudget(700, capture=private_capture, clock=lambda:clock[0])
        clock[0] = 700
        try: budget.remaining()
        except cli.record_transport.TransportRefusal as error:
            self.assertIn("phase=release-budget family=release code=record-chain-invalid:transport-deadline",
                          cli._failure_diagnostic(error))
        else: self.fail("deadline must refuse")

    def test_runtime_and_cli_codes_are_type_approved_only(self):
        for code in ("network", "pull", "capture", "container", "process", "boot-timeout",
                     "release-verification-failed", "cli-path-invalid", "cli-token-unavailable"):
            with self.subTest(code=code):
                error = cli.proof.InfrastructureFailure(code)
                self.assertIn("code=" + code + ";", cli._failure_diagnostic(error))
                self.assertEqual(cli._diagnostic_code(ValueError(code)), "unknown")
                self.assertEqual(cli._diagnostic_code(cli.common.ReleaseError(code)), "unknown")
        for code in ("record-chain-invalid:attestation", "attestation-unverified:image",
                     "gh-failed:version", "input-invalid:json"):
            self.assertEqual(cli._diagnostic_code(cli.common.ReleaseError(code)), code)
            self.assertEqual(cli._diagnostic_code(cli.proof.EvidenceError(code)), "unknown")

    def test_suppressed_real_verifier_context_is_visible_without_private_text(self):
        with tempfile.TemporaryDirectory() as temp:
            client = object.__new__(cli.runtime.ReadOnlyGh)
            client.work_dir = Path(temp)
            with mock.patch.object(cli.records, "load_bootstrap",
                                   side_effect=cli.common.ReleaseError("record-chain-invalid:bootstrap")):
                try:
                    cli.runtime.verified_previous(client, Path(temp))
                except cli.proof.EvidenceError as error:
                    self.assertTrue(error.__suppress_context__)
                    text = cli._failure_diagnostic(error)
                else:
                    self.fail("the verifier must refuse")
        self.assertIn("phase=release-verify family=evidence code=release-verification-failed", text)
        self.assertIn("family=release code=record-chain-invalid:bootstrap", text)
        self.assertNotIn(temp, text)

    def test_real_owned_pull_and_inspect_phases_survive_generic_transport(self):
        with tempfile.TemporaryDirectory() as temp:
            docker = cli.runtime.Docker("synthetic-docker", Path(temp).resolve(),
                capture=lambda *args, **kwargs: cli.runtime.Capture(1, b"", b"private"))
            for method, phase, code in ((docker.pull, "docker-pull", "pull"),
                                        (docker.inspect, "docker-inspect", "container")):
                try:
                    method(cli.runtime.POSTGRES)
                except cli.proof.InfrastructureFailure as error:
                    self.assertIn("phase=" + phase + " family=infrastructure code=" + code,
                                  cli._failure_diagnostic(error))
                else:
                    self.fail("nonzero synthetic transport must refuse")

    def test_exact_code_identity_not_equal_code_or_name_and_frame_bound(self):
        clone = types.FunctionType(cli.context.__code__.replace(), cli.context.__globals__)
        self.assertEqual(clone.__code__, cli.context.__code__)
        self.assertIsNot(clone.__code__, cli.context.__code__)
        for function, expected in ((cli.context, "cli-context"), (clone, "unknown")):
            try:
                function({})
            except cli.proof.EvidenceError as error:
                self.assertEqual(cli._diagnostic_phase(error), expected)
        def deep(count):
            if count:
                return deep(count - 1)
            cli.context({})
        try:
            deep(70)
        except cli.proof.EvidenceError as error:
            self.assertEqual(cli._diagnostic_phase(error), "trace-limit")

    def test_hostile_exception_slots_strings_and_unknown_suffix_never_render(self):
        class Hostile(cli.proof.InfrastructureFailure):
            def __getattribute__(self, name):
                if name in {"args", "__cause__", "__context__", "__traceback__"}:
                    raise AssertionError("custom exception property used")
                return super().__getattribute__(name)
            def __str__(self):
                raise AssertionError("private text rendered")
            def __repr__(self):
                raise AssertionError("private text rendered")
        class String(str):
            def __hash__(self):
                raise AssertionError("untrusted string hashed")
        cases = (Hostile("/private/token"), Hostile("pull", "secret"), Hostile(String("pull")),
                 cli.common.ReleaseError("record-chain-invalid:private-token"),
                 OSError("/private/path"), Exception("pull"))
        for error in cases:
            text = cli._failure_diagnostic(error)
            self.assertIn("code=unknown", text)
            self.assertNotIn("private", text)
            self.assertNotIn("secret", text)
            self.assertLess(len(text), 1024)
        self.assertEqual(cli._failure_message(Hostile("admission-build-failed")),
                         "compatibility-entrypoint-refused: admission-build-failed")

    def test_chain_cycle_limit_and_explicit_cause_precedence_are_bounded(self):
        error = cli.proof.EvidenceError("release-verification-failed")
        error.__context__ = error
        self.assertTrue(cli._failure_diagnostic(error).endswith("chain=cycle"))
        for _ in range(8):
            outer = cli.common.ReleaseError("record-chain-invalid:attestation")
            outer.__context__ = error
            error = outer
        text = cli._failure_diagnostic(error)
        self.assertEqual(text.count("phase="), 4)
        self.assertTrue(text.endswith("chain=limit")); self.assertLess(len(text), 1024)
        error.__cause__ = cli.proof.InfrastructureFailure("network")
        error.__context__ = ValueError("private")
        text = cli._failure_diagnostic(error)
        self.assertIn("family=infrastructure code=network", text)
        self.assertNotIn("family=value", text)

    def test_entry_refusal_reports_known_suppressed_cause_without_result_or_marker(self):
        env = environment(); ctx = cli.context(env)
        def fail(*args, **kwargs):
            try:
                raise cli.common.ReleaseError("attestation-unverified:record")
            except cli.common.ReleaseError:
                raise cli.proof.EvidenceError("release-verification-failed") from None
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve(); env["RUNNER_TEMP"] = str(root)
            for name in ("docker", "buildx"):
                (root / name).touch()
                env["COMPAT_" + ("DOCKER" if name == "docker" else "BUILDX") + "_BIN"] = str(root / name)
            with mock.patch.dict(cli.os.environ, env, clear=True), \
                 mock.patch.object(cli, "_read_public", return_value=cli.canonical(snapshot(ctx))), \
                 mock.patch.object(cli, "_scratch", return_value=root), mock.patch.object(cli, "_client"), \
                 mock.patch.object(cli, "_git_binary", return_value=Path("/synthetic-git")), \
                 mock.patch.object(cli, "execute", side_effect=fail), \
                 mock.patch.object(cli, "_write_public") as write, mock.patch.object(cli, "_mark_complete") as mark, \
                 contextlib.redirect_stderr(io.StringIO()) as stderr, contextlib.redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(cli.main(["run"]), 1)
            write.assert_not_called(); mark.assert_not_called()
            self.assertFalse((root / "public" / "compat-result.json").exists())
        self.assertEqual(stdout.getvalue(), "")
        self.assertEqual(stderr.getvalue().splitlines()[0], "compatibility-entrypoint-refused")
        self.assertIn("code=release-verification-failed", stderr.getvalue())
        self.assertIn("family=release code=attestation-unverified:record", stderr.getvalue())
        self.assertLess(len(stderr.getvalue()), 1024)


class BuildDiagnosticTests(unittest.TestCase):
    def test_exact_carrier_private_marker_projection_and_bounded_cause_walk(self):
        private=b'/private/token PRIVATE_SQL Password=secret https://secret.example/'
        error=cli.images._BuildFailure('candidate',cli.runtime.Capture(2,private,b'failed to solve: '+private))
        value=cli._build_failure_diagnostic(error)
        self.assertTrue(value.startswith('compatibility-build-diagnostic: '))
        data=json.loads(value.split(': ',1)[1])
        self.assertEqual(data['role'],'candidate');self.assertEqual(data['exit_code'],2)
        self.assertEqual(data['families'],['buildkit']);self.assertIsNone(data['cleanup_complete'])
        for word in ('private','token','PRIVATE_SQL','Password','secret','https'):
            self.assertNotIn(word,value)
        self.assertLess(len(value),1024)
        outer=cli.proof.InfrastructureFailure('admission-builder-ownership-unresolved')
        outer.__cause__=error
        self.assertEqual(cli._build_failure_diagnostic(outer),value)
        for _ in range(4):
            parent=ValueError('private');parent.__context__=outer;outer=parent
        self.assertIsNone(cli._build_failure_diagnostic(outer))
        outer.__context__=outer
        self.assertIsNone(cli._build_failure_diagnostic(outer))
        class Foreign(cli.images._BuildFailure):
            def _public(self):raise AssertionError('arbitrary accessor')
        self.assertIsNone(cli._build_failure_diagnostic(Foreign('graph',cli.runtime.Capture(1,b'',b''))))
        error._snapshot=('private',)
        self.assertIsNone(cli._build_failure_diagnostic(error))

    def test_build_refusal_and_cleanup_cause_emit_facts_without_public_result(self):
        env=environment();ctx=cli.context(env)
        for cleanup in (True,False):
            error=cli.images._BuildFailure('fixture',cli.runtime.Capture(17,b'private-source',b'npm ERR! private'))
            cli.images._BuildFailure._finish_cleanup(error,cleanup)
            if not cleanup:
                outer=cli.proof.InfrastructureFailure('admission-builder-ownership-unresolved')
                outer.__cause__=error
            else:outer=error
            with self.subTest(cleanup=cleanup),tempfile.TemporaryDirectory() as temp:
                root=Path(temp).resolve();env['RUNNER_TEMP']=str(root)
                for name in ('docker','buildx'):
                    (root/name).touch()
                    env['COMPAT_'+('DOCKER' if name=='docker' else 'BUILDX')+'_BIN']=str(root/name)
                with mock.patch.dict(cli.os.environ,env,clear=True), \
                     mock.patch.object(cli,'_read_public',return_value=cli.canonical(snapshot(ctx))), \
                     mock.patch.object(cli,'_scratch',return_value=root),mock.patch.object(cli,'_client'), \
                     mock.patch.object(cli,'_git_binary',return_value=Path('/synthetic-git')), \
                     mock.patch.object(cli,'execute',side_effect=outer), \
                     mock.patch.object(cli,'_write_public') as write,mock.patch.object(cli,'_mark_complete') as mark, \
                     contextlib.redirect_stderr(io.StringIO()) as stderr,contextlib.redirect_stdout(io.StringIO()) as stdout:
                    self.assertEqual(cli.main(['run']),1)
                write.assert_not_called();mark.assert_not_called()
                self.assertFalse((root/'public'/'compat-result.json').exists())
            lines=stderr.getvalue().splitlines();self.assertEqual(len(lines),3)
            self.assertEqual(lines[0],cli._failure_message(outer))
            data=json.loads(lines[2].split(': ',1)[1])
            self.assertEqual((data['role'],data['exit_code'],data['cleanup_complete']),('fixture',17,cleanup))
            self.assertEqual(data['stdout_bytes'],14);self.assertEqual(data['families'],['package'])
            self.assertNotIn('private',stderr.getvalue());self.assertEqual(stdout.getvalue(),'')
            self.assertLess(len(lines[2]),1024)


if __name__ == "__main__":
    unittest.main()
