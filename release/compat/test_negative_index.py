"""Controlled local subjects/captures; no installed setup or hosted authority."""
from dataclasses import FrozenInstanceError, replace
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import negative_index as publisher
import cli
import compat_negative_index as policy
import compat_negative_transport as transport
import test_compat_negative_index as policy_tests
import test_cli as normal_tests
import test_cli_resolution as source_tests
import test_cli_rollback as rollback_source_tests
import test_compat_rollback as rollback_tests


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def http(status, value):
    reason = {200: "OK", 201: "Created", 404: "Not Found", 500: "Internal Server Error"}[status]
    return (f"HTTP/2.0 {status} {reason}\r\nContent-Type: application/json; charset=utf-8\r\n\r\n"
            .encode("ascii") + cli.canonical(value))


class Clock:
    def __init__(self):
        self.now = 100.0

    def __call__(self):
        return self.now


class Capture:
    """A strict command boundary, not a replacement validator or publisher."""
    def __init__(self, clock):
        self.clock = clock
        self.calls = []
        self.request = None
        self.present = False
        self.fail_post = False
        self.fail_readback = False
        self.wrong_sha = False
        self.rule_changed = False
        self.rules = 0
        self.after = lambda argv: None

    def ref(self):
        name = policy.marker_ref(self.request)
        sha = "f" * 40 if self.wrong_sha else self.request.producer_sha
        api = "https://api.github.com/repos/" + cli.common.REPOSITORY
        return {"ref": name, "node_id": "REF_synthetic", "url": api + "/git/" + name,
                "object": {"type": "commit", "sha": sha, "url": api + "/git/commits/" + sha}}

    def __call__(self, argv, *, env, timeout, stdout_limit, stderr_limit):
        self.calls.append((tuple(argv), dict(env), timeout, stdout_limit, stderr_limit))
        rc, raw = 0, b""
        if argv[1:] == ["--version"]:
            raw = b"gh version 2.98.0 (synthetic)\n"
        else:
            assert argv[1:4] == ["api", "--hostname", "github.com"], argv
            assert argv[4] == "--method", argv
            method, path = argv[5:7]
            base = "/repos/" + cli.common.REPOSITORY
            if method == "GET" and path == base:
                raw = cli.canonical({"id": 1297344686, "full_name": cli.common.REPOSITORY})
            elif method == "GET" and "/rulesets/" in path:
                self.rules += 1
                raw = policy_tests.rule_response()
                if self.rule_changed and self.rules > 1:
                    value = json.loads(raw); value["enforcement"] = "disabled"
                    raw = cli.canonical(value)
            elif method == "GET" and path.startswith(base + "/git/ref/"):
                assert self.request is not None
                assert path == base + "/git/ref/" + policy.marker_ref(self.request)[5:]
                if self.present and not self.fail_readback:
                    raw = http(200, self.ref())
                else:
                    rc, raw = 1, http(404, {"message": "Not Found", "status": "404"})
            elif method == "POST" and path == base + "/git/refs":
                assert self.request is not None
                fields = [argv[index + 1] for index, item in enumerate(argv) if item == "--raw-field"]
                assert fields == ["ref=" + policy.marker_ref(self.request),
                                  "sha=" + self.request.producer_sha]
                assert not self.present
                self.present = True
                rc, raw = ((1, http(500, {"message": "PRIVATE ambiguous"})) if self.fail_post
                           else (0, http(201, self.ref())))
            else:
                raise AssertionError("unexpected controlled command")
        self.after(argv)
        return cli.runtime.Capture(rc, raw, b"")


class PublisherTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.inputs = self.root / "compat-input"; self.inputs.mkdir()
        self.binary = self.root / "gh"; self.binary.write_bytes(b"controlled executable")
        self.clock = Clock()
        self.capture = Capture(self.clock)
        self.budget = cli.record_transport.OperationBudget(700, capture=self.capture, clock=self.clock)
        self.addCleanup(self.budget.close)
        self.setup = policy_tests.setup_bytes()
        install = mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", self.setup)
        install.start(); self.addCleanup(install.stop)
        self.env = normal_tests.environment()
        self.env.update(RUNNER_TEMP=str(self.root), GH_TOKEN="synthetic-not-a-credential",
                        COMPAT_GH_BIN=str(self.binary), COMPAT_PREVIEW_OUTCOME="success",
                        COMPAT_RUN_OUTCOME="success")
        self.ctx = cli.context(self.env)
        self.snapshot = normal_tests.snapshot(self.ctx)
        self.value = normal_tests.result(self.ctx)
        self.install()

    def install(self, snapshot=None, raw=None, context=None, ctx=None):
        if ctx is not None:
            self.ctx = ctx
            self.env.update(GITHUB_SHA=ctx.sha, GITHUB_WORKFLOW_SHA=ctx.sha,
                GITHUB_EVENT_NAME=ctx.event, GITHUB_REF=ctx.ref,
                GITHUB_WORKFLOW_REF=cli._WORKFLOW + ctx.ref,
                GITHUB_RUN_ID=str(ctx.run_id), GITHUB_RUN_ATTEMPT=str(ctx.attempt),
                COMPAT_ROLLBACK_TARGET=ctx.target or "")
        if snapshot is not None:
            self.snapshot = snapshot
        if raw is None:
            raw = cli.proof.result_bytes(self.value)
        if context is None:
            maker = cli.evidence_context if self.ctx.target is None else cli.rollback_evidence_context
            context = cli.canonical(maker(raw, self.snapshot, self.ctx))
        self.public = self.root / ("compat-validate-evidence-" + self.ctx.suffix) / "public"
        self.public.mkdir(parents=True, exist_ok=True)
        (self.inputs / "resolution.json").write_bytes(cli.canonical(self.snapshot))
        for path in (self.inputs, self.public):
            (path / "compat-result.json").write_bytes(raw)
            (path / "compat-context.json").write_bytes(context)
        self.raw, self.context_raw = raw, context
        self.capture.request = policy.PublicationRequest(self.ctx.sha, self.ctx.run_id, self.ctx.attempt,
            digest(raw), digest(context), "normal3" if self.ctx.target is None else "rollback4")

    def negative(self, flag="read"):
        self.value["old_server_ok"][flag] = False
        self.install()

    def rollback(self, **options):
        pair = rollback_tests.pair(**options)
        context = json.loads(pair.context)
        snapshot = {key: value for key, value in context.items() if key not in {
            "repository", "workflow_path", "workflow_sha", "event", "ref", "result_sha256"}}
        snapshot["version"] = 1
        ctx = cli.Context(context["sha"], context["run_id"], context["run_attempt"],
            context["requested_rollback_target"], context["event"], context["ref"])
        self.install(snapshot, pair.result, pair.context, ctx)
        return pair

    def run_publish(self):
        with mock.patch.dict(os.environ, self.env, clear=True):
            return publisher.publish_from_environment(budget=self.budget)

    def refuse(self):
        with self.assertRaises(policy.NegativeIndexRefusal) as caught:
            self.run_publish()
        self.assertEqual(str(caught.exception), "negative-index-invalid")
        self.assertNotIn("PRIVATE", str(caught.exception))
        return caught.exception

    def posts(self):
        return [row for row in self.capture.calls if "POST" in row[0]]

    def test_none_configuration_precedes_arguments_environment_clock_and_imports(self):
        with mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", None), \
             mock.patch.object(publisher, "_operational_modules", side_effect=AssertionError("imports")), \
             mock.patch.object(publisher.time, "monotonic", side_effect=AssertionError("clock")), \
             mock.patch.object(publisher.os, "environ", object()):
            output = io.StringIO()
            with contextlib.redirect_stderr(output):
                self.assertEqual(publisher.main(object()), 1)
            self.assertEqual(output.getvalue(), "negative-index-unconfigured\n")
            with self.assertRaisesRegex(policy.NegativeIndexRefusal, "negative-index-unconfigured"):
                publisher.publish_from_environment(budget=object())
        self.assertEqual(self.capture.calls, [])

    def test_positive_has_exact_receipt_and_no_token_binary_scratch_or_transport(self):
        self.env.pop("GH_TOKEN"); self.env.pop("COMPAT_GH_BIN")
        with mock.patch.object(transport, "open_publisher", side_effect=AssertionError("transport")):
            result = self.run_publish()
        self.assertEqual(type(result), policy.NoIntentRequired)
        self.assertEqual(result.request, self.capture.request)
        self.assertEqual(result.setup_sha256, digest(self.setup))
        self.assertEqual(self.capture.calls, [])
        self.assertFalse((self.root / ("compat-negative-index-" + self.ctx.suffix)).exists())
        self.assertFalse(self.budget.closed)
        with self.assertRaises(FrozenInstanceError): result.setup_sha256 = "0" * 64

    def test_cross_api_birthtime_and_ctime_are_distinct_with_exact_fallback(self):
        path = (11, 22, 33206, 31, 100, 10, 1, 10)
        handle = (*path[:5], 20, *path[6:])
        self.assertTrue(publisher._same_file_identity(path, handle))
        for index in (0, 1, 2, 3, 4, 6, 7):
            with self.subTest(field=index):
                changed = list(handle); changed[index] += 1
                self.assertFalse(publisher._same_file_identity(path, tuple(changed)))
        for left_birth, right_birth in ((None, None), (10, None), (None, 10)):
            with self.subTest(birthtimes=(left_birth, right_birth)):
                left, right = (*path[:7], left_birth), (*handle[:7], right_birth)
                self.assertFalse(publisher._same_file_identity(left, right))
                self.assertTrue(publisher._same_file_identity(left, (*right[:5], 10, *right[6:])))

    def test_actual_file_change_during_read_refuses_before_validation_or_capture(self):
        original = os.fstat
        calls = []
        path = self.inputs / "resolution.json"
        raw = path.read_bytes()
        def changed(descriptor):
            calls.append(descriptor)
            if len(calls) == 2:
                path.write_bytes(raw + b" ")
            return original(descriptor)
        with mock.patch.object(publisher.os, "fstat", changed), \
             mock.patch.object(publisher, "_validate", side_effect=AssertionError("validator reached")) as validate:
            self.refuse()
        validate.assert_not_called()
        self.assertEqual(len(calls), 2)
        self.assertEqual(path.read_bytes(), raw + b" ")
        self.assertTrue(self.budget.failed)
        self.assertEqual(self.capture.calls, [])

    def test_negative_uses_real_factory_capture_and_exact_two_raw_fields(self):
        self.negative()
        result = self.run_publish()
        self.assertEqual(type(result), policy.IntentPersisted)
        self.assertEqual(result.request, self.capture.request)
        self.assertEqual(result.marker_ref, policy.marker_ref(self.capture.request))
        self.assertEqual(result.setup_sha256, digest(self.setup))
        self.assertEqual(len(self.posts()), 1)
        self.assertEqual(self.capture.rules, 2)
        self.assertFalse(self.budget.closed)
        for argv, env, timeout, _, _ in self.capture.calls:
            self.assertNotIn("--input", argv)
            self.assertEqual(env["GH_HOST"], "github.com")
            self.assertLessEqual(timeout, 30)
            self.assertNotIn("GITHUB_OUTPUT", env)

    def test_existing_exact_marker_is_idempotent_without_post(self):
        self.negative(); self.capture.present = True
        result = self.run_publish()
        self.assertEqual(result.marker_ref, policy.marker_ref(self.capture.request))
        self.assertEqual(self.posts(), [])

    def test_wrong_existing_marker_object_refuses_without_post(self):
        self.negative(); self.capture.present = True; self.capture.wrong_sha = True
        self.refuse(); self.assertEqual(self.posts(), [])

    def test_ambiguous_create_is_terminal_without_retry_or_receipt(self):
        self.negative(); self.capture.fail_post = True
        self.refuse(); self.assertEqual(len(self.posts()), 1)
        self.assertTrue(self.capture.present)
        self.assertTrue(self.budget.failed)

    def test_missing_readback_and_changed_rule_refuse_without_deleting_intent(self):
        self.negative(); self.capture.fail_readback = True
        self.refuse(); self.assertEqual(len(self.posts()), 1)
        self.assertTrue(self.capture.present)

    def test_final_rule_change_refuses_even_after_exact_created_ref(self):
        self.negative(); self.capture.rule_changed = True
        self.refuse(); self.assertTrue(self.capture.present)
        self.assertEqual(len(self.posts()), 1)

    def test_rollback_positive_keeps_all_source_roles_and_uses_H(self):
        self.rollback()
        result = self.run_publish()
        self.assertEqual(result.request.protocol, "rollback4")
        self.assertEqual(result.request.producer_sha, self.ctx.sha)
        self.assertNotEqual(self.ctx.sha, self.snapshot["live_web_source_sha"])
        self.assertNotEqual(self.ctx.sha, self.snapshot["selected_web_source_sha"])
        self.assertEqual(self.capture.calls, [])

    def test_rollback_negative_marker_is_H_not_WL_or_target(self):
        self.rollback(server_false="workers")
        result = self.run_publish()
        self.assertEqual(result.request.producer_sha, self.ctx.sha)
        self.assertTrue(result.marker_ref.startswith("refs/tags/rereply-compat-negative-v1-" + self.ctx.sha + "-"))
        self.assertEqual(len(self.posts()), 1)

    def test_each_complete_semantic_false_is_published(self):
        # Each subcase owns a new scratch, client and original allowance.
        variants = [("forward", None), ("old_migrate_ok", "rw"), ("old_migrate_ok", "ro")]
        variants += [("old_server_ok", key) for key in ("boot", "read", "write", "reverse", "workers")]
        for name, key in variants:
            with self.subTest(name=name, key=key):
                temporary = tempfile.TemporaryDirectory(); self.addCleanup(temporary.cleanup)
                self.root = Path(temporary.name).resolve()
                self.inputs = self.root / "compat-input"; self.inputs.mkdir()
                self.env["RUNNER_TEMP"] = str(self.root)
                self.value = normal_tests.result(self.ctx)
                if key is None: self.value[name] = False
                else: self.value[name][key] = False
                self.value["reversible"] = all(self.value["old_migrate_ok"].values())
                self.capture = Capture(self.clock)
                self.budget = cli.record_transport.OperationBudget(700, capture=self.capture, clock=self.clock)
                self.addCleanup(self.budget.close)
                self.install()
                result = self.run_publish()
                self.assertEqual(type(result), policy.IntentPersisted)
                self.assertEqual(len(self.posts()), 1)

    def test_unproven_negative_control_is_refusal_not_intent(self):
        value = copy.deepcopy(self.value)
        value["old_migrate_ok"]["negative_control"] = False; value["reversible"] = False
        (self.public / "compat-result.json").write_bytes(cli.canonical(value))
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_actual_final_catalog_mismatch_hash_is_never_replaced(self):
        pair = self.rollback(server_false="reverse", catalog_sha256="9" * 64)
        result = self.run_publish()
        self.assertEqual(result.request.result_sha256, digest(pair.result))
        self.assertNotEqual(json.loads(pair.result)["catalog_sha256"],
                            self.snapshot["database_base"]["catalog"]["catalog_sha256"])

    def test_downloaded_evidence_does_not_supply_marker_bytes(self):
        self.negative()
        positive = cli.proof.result_bytes(normal_tests.result(self.ctx))
        (self.inputs / "compat-result.json").write_bytes(positive)
        (self.inputs / "compat-context.json").write_bytes(cli.canonical(
            cli.evidence_context(positive, self.snapshot, self.ctx)))
        result = self.run_publish()
        self.assertEqual(result.request.result_sha256, digest(self.raw))
        self.assertNotEqual(result.request.result_sha256, digest(positive))

    def test_positive_actual_subjects_ignore_negative_downloaded_copy(self):
        value = copy.deepcopy(self.value); value["forward"] = False
        raw = cli.proof.result_bytes(value)
        (self.inputs / "compat-result.json").write_bytes(raw)
        (self.inputs / "compat-context.json").write_bytes(cli.canonical(cli.evidence_context(raw, self.snapshot, self.ctx)))
        self.assertEqual(type(self.run_publish()), policy.NoIntentRequired)
        self.assertEqual(self.capture.calls, [])

    def test_only_downloaded_evidence_without_actual_signed_subject_refuses(self):
        (self.public / "compat-result.json").unlink()
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_input_and_public_extra_inventory_refuse_before_network(self):
        (self.inputs / "extra.json").write_bytes(b"PRIVATE")
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_extra_attested_public_file_refuses_before_network(self):
        (self.public / "extra.json").write_bytes(b"PRIVATE")
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_directory_subject_and_relative_root_refuse(self):
        path = self.public / "compat-result.json"; path.unlink(); path.mkdir()
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_relative_runner_root_refuses(self):
        self.env["RUNNER_TEMP"] = "relative"
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_actual_hardlinked_signed_subject_refuses_before_network(self):
        alias = self.root / "outside-public-alias"
        os.link(self.public / "compat-result.json", alias)
        self.assertEqual((self.public / "compat-result.json").stat().st_nlink, 2)
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_actual_hardlinked_resolution_refuses_before_network(self):
        alias = self.root / "outside-input-alias"
        os.link(self.inputs / "resolution.json", alias)
        self.assertEqual((self.inputs / "resolution.json").stat().st_nlink, 2)
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_link_reported_on_subject_parent_refuses_before_network(self):
        # Controlled filesystem metadata edge; no platform symlink privilege.
        original = Path.is_symlink
        parent = self.public.parent
        def linked(path):
            return path == parent or original(path)
        with mock.patch.object(Path, "is_symlink", linked):
            self.refuse()
        self.assertEqual(self.capture.calls, [])

    def test_regular_file_permission_change_after_capture_refuses(self):
        self.negative()
        original = publisher._identity
        path = self.public / "compat-result.json"
        def identity(value, cli_module, *, directory=False):
            observed = original(value, cli_module, directory=directory)
            if value == path and self.capture.rules == 2:
                observed = (*observed[:2], observed[2] ^ 0o200, *observed[3:])
            return observed
        with mock.patch.object(publisher, "_identity", identity):
            self.refuse()
        self.assertTrue(self.capture.present)

    def test_each_file_cap_is_checked_before_json_decode(self):
        # Test the bounded reader itself for all roles without copying huge fixtures.
        for name, limit in (("resolution.json", 16384), ("compat-result.json", 16384), ("compat-context.json", 4096)):
            with self.subTest(name=name):
                path = self.root / name; path.write_bytes(b" " * (limit + 1))
                with self.assertRaises(policy.NegativeIndexRefusal):
                    publisher._read(path, limit, cli, self.budget)

    def test_noncanonical_snapshot_refuses_before_network(self):
        (self.inputs / "resolution.json").write_bytes(json.dumps(self.snapshot, indent=2).encode() + b"\n")
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_duplicate_snapshot_key_refuses_before_network(self):
        raw = cli.canonical(self.snapshot)
        (self.inputs / "resolution.json").write_bytes(b'{"version":4,' + raw[1:])
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_wrong_context_digest_is_not_repaired(self):
        value = json.loads(self.context_raw); value["result_sha256"] = "0" * 64
        (self.public / "compat-context.json").write_bytes(cli.canonical(value))
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_cross_protocol_subjects_refuse_without_fallback(self):
        pair = rollback_tests.pair()
        (self.public / "compat-context.json").write_bytes(pair.context)
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_wrong_snapshot_and_result_run_identity_refuse(self):
        value = copy.deepcopy(self.snapshot); value["run_attempt"] += 1
        (self.inputs / "resolution.json").write_bytes(cli.canonical(value))
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_wrong_result_H_with_rebound_context_digest_refuses(self):
        value = copy.deepcopy(self.value); value["sha"] = "f" * 40
        raw = cli.proof.result_bytes(value)
        context = json.loads(self.context_raw); context["result_sha256"] = digest(raw)
        (self.public / "compat-result.json").write_bytes(raw)
        (self.public / "compat-context.json").write_bytes(cli.canonical(context))
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_wrong_rollback_target_refuses_before_client(self):
        self.rollback(server_false="read")
        self.env["COMPAT_ROLLBACK_TARGET"] = "prod-0000"
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_pull_request_and_nonmain_dispatch_are_not_publishers(self):
        self.env.update(GITHUB_EVENT_NAME="pull_request", GITHUB_REF="refs/pull/269/merge",
                        GITHUB_WORKFLOW_REF=cli._WORKFLOW + "refs/pull/269/merge")
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_nonmain_dispatch_refuses_before_files(self):
        self.env.update(GITHUB_EVENT_NAME="workflow_dispatch", GITHUB_REF="refs/heads/other",
                        GITHUB_WORKFLOW_REF=cli._WORKFLOW + "refs/heads/other")
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_failed_cancelled_upstream_cannot_be_signed_negative(self):
        self.negative(); self.env["COMPAT_RUN_OUTCOME"] = "cancelled"
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_target_on_push_refuses_before_client(self):
        self.env["COMPAT_ROLLBACK_TARGET"] = normal_tests.TAG
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_negative_requires_real_token_and_absolute_binary(self):
        self.negative(); self.env["GH_TOKEN"] = "PRIVATE\ninvalid"
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_scratch_replay_is_refused_before_version_or_post(self):
        self.negative(); (self.root / ("compat-negative-index-" + self.ctx.suffix)).mkdir()
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_original_expired_budget_refuses_before_version(self):
        self.negative(); self.clock.now = 700
        self.refuse(); self.assertEqual(self.capture.calls, [])

    def test_version_time_consumes_original_allowance(self):
        self.negative()
        self.capture.after = lambda argv: setattr(self.clock, "now", 700)
        self.refuse(); self.assertEqual(len(self.capture.calls), 1)
        self.assertEqual(self.posts(), [])

    def test_final_subject_mutation_leaves_marker_but_no_receipt(self):
        self.negative()
        def after(argv):
            if self.capture.rules == 2:
                (self.public / "compat-result.json").write_bytes(self.raw + b" ")
        self.capture.after = after
        self.refuse(); self.assertTrue(self.capture.present)
        self.assertEqual(len(self.posts()), 1)

    def test_identical_bytes_replaced_inode_are_refused(self):
        self.negative()
        def after(argv):
            if self.capture.rules == 2:
                path = self.public / "compat-result.json"
                replacement = self.public / "replacement"
                replacement.write_bytes(self.raw)
                os.replace(replacement, path)
        self.capture.after = after
        self.refuse(); self.assertTrue(self.capture.present)

    def test_budget_expires_after_last_capture_and_before_receipt(self):
        self.negative()
        def after(argv):
            if self.capture.rules == 2: self.clock.now = 700
        self.capture.after = after
        self.refuse(); self.assertTrue(self.capture.present)

    def test_forged_transport_receipt_cannot_replace_derived_request(self):
        self.negative()
        real = transport.NegativePublisher.publish_intent
        def changed(client, request):
            receipt = real(client, request)
            return replace(receipt, request=replace(request, producer_sha="f" * 40))
        with mock.patch.object(transport.NegativePublisher, "publish_intent", changed):
            self.refuse()
        self.assertTrue(self.capture.present)

    def test_forged_receipt_boolean_attempt_is_not_equal_integer_authority(self):
        self.negative()
        real = transport.NegativePublisher.publish_intent
        def changed(client, request):
            receipt = real(client, request)
            return replace(receipt, request=replace(request, run_attempt=True))
        with mock.patch.object(transport.NegativePublisher, "publish_intent", changed):
            self.refuse()
        self.assertTrue(self.capture.present)

    def test_cancellation_poisons_allowance_and_returns_no_receipt(self):
        self.negative()
        def cancelled(argv):
            raise KeyboardInterrupt()
        self.capture.after = cancelled
        with self.assertRaises(KeyboardInterrupt): self.run_publish()
        self.assertTrue(self.budget.failed)
        self.assertEqual(self.posts(), [])

    def test_main_positive_has_no_output_readiness_or_subject_write(self):
        output = self.root / "outputs"; output.write_bytes(b"original\n")
        self.env["GITHUB_OUTPUT"] = str(output)
        before = (self.public / "compat-result.json").read_bytes()
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.dict(os.environ, self.env, clear=True), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(publisher.main([]), 0)
        self.assertEqual(stdout.getvalue(), ""); self.assertEqual(stderr.getvalue(), "")
        self.assertEqual(output.read_bytes(), b"original\n")
        self.assertEqual((self.public / "compat-result.json").read_bytes(), before)

    def test_main_unknown_failure_is_fixed_and_never_leaks_input(self):
        self.env["GITHUB_SHA"] = "PRIVATEinvalid"
        stderr = io.StringIO()
        with mock.patch.dict(os.environ, self.env, clear=True), contextlib.redirect_stderr(stderr):
            self.assertEqual(publisher.main([]), 1)
        self.assertEqual(stderr.getvalue(), "negative-index-publisher-refused\n")

    def test_main_closure_failure_cannot_return_zero(self):
        def bad_close(budget):
            budget.closed = True
            raise RuntimeError("PRIVATE close")
        stderr = io.StringIO()
        with mock.patch.dict(os.environ, self.env, clear=True), \
             mock.patch.object(cli.record_transport.OperationBudget, "close", bad_close), \
             contextlib.redirect_stderr(stderr):
            self.assertEqual(publisher.main([]), 1)
        self.assertEqual(stderr.getvalue(), "negative-index-publisher-refused\n")

    def test_concrete_normal_resolver_snapshot_crosses_real_publisher_validators(self):
        source_tests.ResolutionTests.setUpClass()
        case = source_tests.ResolutionTests(); case.setUp(); self.addCleanup(case.doCleanups)
        snapshot = case.preview(); raw = case.raw(snapshot, negative=True)
        context = cli.canonical(cli.evidence_context(raw, snapshot, case.ctx))
        self.install(snapshot, raw, context, case.ctx)
        result = self.run_publish()
        self.assertEqual(result.request.result_sha256, digest(raw))
        self.assertEqual(result.request.protocol, "normal3")
        self.assertNotEqual(snapshot["database_base"]["source_sha"], snapshot["selected_web_source_sha"])

    def test_concrete_rollback_source_engine_and_verifier_pair_crosses_publisher(self):
        rollback_source_tests.RollbackCLITests.setUpClass()
        case = rollback_source_tests.RollbackCLITests(); case.setUp(); self.addCleanup(case.doCleanups)
        case.w.false_case = "read"
        snapshot = case.preview(); raw = case.execute(snapshot)
        measured, pair, _ = case.measured(raw, snapshot)
        self.assertFalse(measured.positive)
        self.install(snapshot, pair.result, pair.context, case.ctx)
        result = self.run_publish()
        self.assertEqual(result.request.producer_sha, case.ctx.sha)
        self.assertEqual(result.request.protocol, "rollback4")
        self.assertEqual(result.request.context_sha256, digest(pair.context))


if __name__ == "__main__":
    unittest.main()
