"""Shared wire-parser parity; fixtures are data, never release authority."""
from dataclasses import FrozenInstanceError, replace
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import cli
import public_evidence as public
import test_cli as normal_fixture
import test_compat_rollback as rollback_fixture


_SHARED = (
    "need", "canonical", "Context", "context", "regular_path", "validate_snapshot",
    "validate_result", "evidence_context", "validate_evidence", "accepted",
    "_rollback_context", "validate_rollback_snapshot", "validate_rollback_result",
    "rollback_evidence_context", "validate_rollback_evidence",
)


def rollback_documents(**options):
    pair = rollback_fixture.pair(**options)
    context = json.loads(pair.context)
    snapshot = {key: value for key, value in context.items() if key not in {
        "repository", "workflow_path", "workflow_sha", "event", "ref", "result_sha256"}}
    snapshot["version"] = 1
    ctx = public.Context(context["sha"], context["run_id"], context["run_attempt"],
                         context["requested_rollback_target"], context["event"], context["ref"])
    return ctx, snapshot, pair.result, pair.context


class SharedBindingTests(unittest.TestCase):
    def test_cli_exports_identical_objects_and_frozen_context(self):
        for name in _SHARED:
            with self.subTest(name=name):
                self.assertIs(getattr(cli, name), getattr(public, name))
        for name in ("_SHA", "_MAX_PUBLIC", "_MAX_CONTEXT", "_WORKFLOW_PATH", "_WORKFLOW"):
            self.assertIs(getattr(cli, name), getattr(public, name))
        ctx = public.context(normal_fixture.environment())
        self.assertIs(type(ctx), cli.Context)
        self.assertEqual(ctx.suffix, f"{ctx.sha}-{ctx.run_id}-{ctx.attempt}")
        with self.assertRaises(FrozenInstanceError):
            ctx.sha = "f" * 40

    def test_active_artifacts_are_exact_original_three_source_pins(self):
        catalog = cli.compat.catalog
        expected = (
            ("active_catalog", "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8"),
            ("active_globals", "c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3"),
            ("active_history", "9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513"),
        )
        self.assertEqual(public.PENDING_ACTIVE_ARTIFACTS, expected)
        self.assertIs(catalog.PENDING_ACTIVE_ARTIFACTS, public.PENDING_ACTIVE_ARTIFACTS)
        paths = (catalog._GOLDEN, "internal/dbcatalog/golden/global_tables.json",
                 "internal/dbcatalog/golden/history.json")
        self.assertEqual(tuple(value for _, value in expected), tuple(catalog._PINS[path] for path in paths))

    def test_cold_import_has_no_cli_engine_source_or_transport_dependency(self):
        # Known module search roots are supplied by this import test only. The
        # publisher's separate cold main test must establish its own path setup.
        child = r'''
import importlib, json, sys
from pathlib import Path
root = Path(sys.argv[1])
blocked = {"cli", "compat", "catalog", "images", "postgres", "processes", "observation",
           "runtime", "record_resolver", "record_transport", "schema_change", "ship_outage_compat",
           "record_handoff", "compat_negative_transport", "negative_index"}
assert not blocked.intersection(sys.modules)
class RejectOperational:
    def find_spec(self, fullname, path=None, target=None):
        if fullname.split(".")[0] in blocked:
            raise AssertionError("operational import")
sys.meta_path.insert(0, RejectOperational())
def audit(event, args):
    if event == "subprocess.Popen" or event == "os.system" or event.startswith("socket."):
        raise AssertionError("external effect")
sys.addaudithook(audit)
sys.path[:0] = [str(root), str(root.parent / "deployment")]
module = importlib.import_module("public_evidence")
assert Path(module.__file__).resolve() == (root / "public_evidence.py").resolve()
assert not blocked.intersection(sys.modules)
assert module.canonical({"accepted": False}) == b'{"accepted":false}\n'
assert module._MAX_PUBLIC == 16384 and module._MAX_CONTEXT == 4096
assert module._SHA.fullmatch("a" * 40) and not module._SHA.fullmatch("a" * 40 + "\\Z")
print(json.dumps({"public": True, "proof": "proof" in sys.modules,
                  "authenticated_parser": "compat_evidence" in sys.modules,
                  "dynamic_proof": "_rereply_compat_measurement_proof" in sys.modules}, sort_keys=True))
'''
        env = {key: os.environ[key] for key in ("SYSTEMROOT", "WINDIR", "TEMP", "TMP") if key in os.environ}
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, "-I", "-S", "-B", "-c", child,
                                     str(Path(__file__).resolve().parent)],
                                    cwd=directory, env=env, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20, check=False)
        self.assertLessEqual(len(result.stdout) + len(result.stderr), 8192)
        self.assertEqual(result.returncode, 0, result.stderr[:8192].decode("utf-8", "replace"))
        self.assertEqual(result.stderr, b"")
        self.assertEqual(json.loads(result.stdout), {
            "public": True, "proof": True, "authenticated_parser": True, "dynamic_proof": True})

    def test_regular_path_retains_explicit_local_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            item = root / "public.json"
            item.write_bytes(b"{}\n")
            self.assertIs(public.regular_path(item), item)
            self.assertIs(public.regular_path(root, directory=True), root)
            for path, folder in ((Path("relative"), False), (root / "absent", False),
                                 (root, False), (item, True)):
                with self.subTest(path=path, directory=folder), self.assertRaises(public.proof.EvidenceError):
                    public.regular_path(path, directory=folder)

    def test_shared_frames_keep_fixed_sanitized_cli_diagnostic_phases(self):
        ctx = public.context(normal_fixture.environment())
        snap = normal_fixture.snapshot(ctx)
        rctx, rsnap, _, _ = rollback_documents()
        cases = (
            (lambda: public.context({}), "cli-context"),
            (lambda: public.regular_path(Path("PRIVATE-relative")), "cli-path"),
            (lambda: public.validate_snapshot({}, ctx), "cli-snapshot"),
            (lambda: public.validate_result(b"", snap, ctx), "cli-result"),
            (lambda: public._rollback_context(replace(rctx, ref="refs/heads/PRIVATE")), "cli-context"),
            (lambda: public.validate_rollback_snapshot({}, rctx), "cli-snapshot"),
            (lambda: public.validate_rollback_result(b"", rsnap, rctx), "cli-result"),
            (lambda: public.validate_rollback_evidence(b"", b"", rsnap, rctx), "cli-result"),
        )
        for action, phase in cases:
            with self.subTest(phase=phase):
                try:
                    action()
                except public.proof.EvidenceError as error:
                    diagnostic = cli._failure_diagnostic(error)
                    self.assertEqual(cli._diagnostic_phase(error), phase)
                    self.assertIn("phase=" + phase + " family=evidence", diagnostic)
                    self.assertNotIn("PRIVATE", diagnostic + cli._failure_message(error))
                else:
                    self.fail("invalid public evidence accepted")


class NormalPublicTests(unittest.TestCase):
    def setUp(self):
        self.ctx = public.context(normal_fixture.environment())
        self.snapshot = normal_fixture.snapshot(self.ctx)
        self.result = normal_fixture.result(self.ctx)
        self.raw = public.proof.result_bytes(self.result)

    def assert_refused(self, action):
        with self.assertRaises((public.proof.EvidenceError, public.common.ReleaseError)):
            action()

    def test_canonical_roundtrip_is_exact_and_does_not_change_inputs(self):
        before = copy.deepcopy(self.snapshot)
        self.assertIs(public.validate_snapshot(self.snapshot, self.ctx), self.snapshot)
        self.assertEqual(public.validate_result(self.raw, self.snapshot, self.ctx), self.result)
        context = public.evidence_context(self.raw, self.snapshot, self.ctx)
        raw = public.canonical(context)
        self.assertEqual((self.snapshot["version"], context["version"]), (4, 3))
        self.assertEqual(public.validate_evidence(raw, self.raw, self.snapshot, self.ctx), context)
        self.assertEqual(self.snapshot, before)
        self.assertEqual(self.raw, public.proof.result_bytes(self.result))
        self.assertTrue(public.accepted(self.result))

    def test_every_completed_false_flag_survives_without_becoming_accepted(self):
        variants = []
        for key in self.result["old_server_ok"]:
            value = copy.deepcopy(self.result); value["old_server_ok"][key] = False; variants.append(value)
        for key in ("rw", "ro"):
            value = copy.deepcopy(self.result); value["old_migrate_ok"][key] = False
            value["reversible"] = False; variants.append(value)
        value = copy.deepcopy(self.result); value["forward"] = False; variants.append(value)
        for value in variants:
            with self.subTest(value=value):
                raw = public.proof.result_bytes(value)
                measured = public.validate_result(raw, self.snapshot, self.ctx)
                context = public.canonical(public.evidence_context(raw, self.snapshot, self.ctx))
                public.validate_evidence(context, raw, self.snapshot, self.ctx)
                self.assertEqual(measured, value)
                self.assertFalse(public.accepted(measured))

    def test_unproven_negative_control_is_not_a_completed_false_result(self):
        value = copy.deepcopy(self.result)
        value["old_migrate_ok"]["negative_control"] = False; value["reversible"] = False
        self.assert_refused(lambda: public.validate_result(public.canonical(value), self.snapshot, self.ctx))
        self.assert_refused(lambda: public.accepted(value))

    def test_snapshot_is_closed_and_exactly_binds_boolean_integer_fields(self):
        mutations = (
            lambda v: v.update(version=True), lambda v: v.update(run_id=True),
            lambda v: v.update(run_attempt=True), lambda v: v.update(extra="PRIVATE"),
            lambda v: v.pop("pending"), lambda v: v["preview"].update(blocked=0),
            lambda v: v["preview"].update(dormant=0),
        )
        for mutate in mutations:
            value = copy.deepcopy(self.snapshot); mutate(value)
            self.assert_refused(lambda: public.validate_snapshot(value, self.ctx))

    def test_snapshot_rebinds_source_run_attempt_selection_and_pending(self):
        mutations = (
            lambda v: v.update(sha="f" * 40), lambda v: v.update(run_id=self.ctx.run_id + 1),
            lambda v: v.update(run_attempt=self.ctx.attempt + 1),
            lambda v: v.update(requested_rollback_target=normal_fixture.TAG),
            lambda v: v["selected"].update(web_digest="sha256:" + "0" * 64),
            lambda v: v["pending"].update(candidate_sha="f" * 40),
            lambda v: v["pending"].update(previous_sha="f" * 40),
            lambda v: v["pending"]["active_artifacts"][0].__setitem__(1, "0" * 64),
        )
        for mutate in mutations:
            value = copy.deepcopy(self.snapshot); mutate(value)
            self.assert_refused(lambda: public.validate_snapshot(value, self.ctx))

    def test_result_identity_previous_digest_and_classification_are_not_rebound(self):
        changes = ({"sha": "f" * 40}, {"run_id": self.ctx.run_id + 1},
                   {"run_attempt": self.ctx.attempt + 1},
                   {"previous_release": normal_fixture.LATEST_TAG},
                   {"previous_web_digest": "sha256:" + "0" * 64},
                   {"classification": {"kind": "data", "dormant": True}})
        for change in changes:
            value = copy.deepcopy(self.result); value.update(change)
            self.assert_refused(lambda: public.validate_result(public.canonical(value), self.snapshot, self.ctx))

    def test_noncanonical_duplicate_and_overbound_bytes_refuse(self):
        spaced = json.dumps(self.result, sort_keys=True, indent=1).encode() + b"\n"
        duplicate = self.raw[:-2] + b',"run_id":123456}\n'
        for raw in (self.raw[:-1], self.raw + b"\n", spaced, duplicate,
                    b"x" * (public._MAX_PUBLIC + 1)):
            self.assert_refused(lambda: public.validate_result(raw, self.snapshot, self.ctx))
        context = public.canonical(public.evidence_context(self.raw, self.snapshot, self.ctx))
        for raw in (context[:-1], context + b"\n", b"x" * (public._MAX_CONTEXT + 1)):
            self.assert_refused(lambda: public.validate_evidence(raw, self.raw, self.snapshot, self.ctx))

    def test_context_hash_and_every_closed_identity_remain_bound(self):
        context = public.evidence_context(self.raw, self.snapshot, self.ctx)
        changes = ({"result_sha256": "0" * 64}, {"workflow_sha": "f" * 40},
                   {"event": "workflow_dispatch"}, {"ref": "refs/heads/other"},
                   {"run_id": True}, {"extra": "PRIVATE"}, {"version": 4})
        for change in changes:
            value = copy.deepcopy(context); value.update(change)
            self.assert_refused(lambda: public.validate_evidence(
                public.canonical(value), self.raw, self.snapshot, self.ctx))

    def test_blocked_preview_never_creates_a_context(self):
        value = copy.deepcopy(self.snapshot); value["preview"]["blocked"] = True
        self.assert_refused(lambda: public.validate_result(self.raw, value, self.ctx))
        self.assert_refused(lambda: public.evidence_context(self.raw, value, self.ctx))


class RollbackPublicTests(unittest.TestCase):
    def setUp(self):
        self.ctx, self.snapshot, self.raw, self.context = rollback_documents()

    def assert_refused(self, action):
        with self.assertRaises((public.proof.EvidenceError, public.common.ReleaseError)):
            action()

    def test_exact_roundtrip_keeps_H_target_live_and_origin_separate(self):
        value = public.validate_rollback_result(self.raw, self.snapshot, self.ctx)
        self.assertEqual(public.canonical(public.rollback_evidence_context(
            self.raw, self.snapshot, self.ctx)), self.context)
        self.assertEqual(public.validate_rollback_evidence(
            self.context, self.raw, self.snapshot, self.ctx), json.loads(self.context))
        roles = (self.ctx.sha, self.snapshot["selected"]["sha"], self.snapshot["latest"]["sha"],
                 self.snapshot["selected_web_source_sha"], self.snapshot["live_web_source_sha"],
                 self.snapshot["database_base"]["source_sha"])
        self.assertEqual(len(set(roles)), 6)
        self.assertEqual(value["sha"], self.ctx.sha)
        self.assertEqual(value["previous_release"], self.ctx.target)
        self.assertTrue(public.accepted(value))

    def test_false_measurements_and_actual_catalog_mismatch_are_retained(self):
        variants = [{"server_false": key} for key in public.compat_evidence.SERVERS]
        variants += [{"migration_false": key} for key in ("rw", "ro")]
        variants += [{"forward": False}, {"server_false": "reverse", "catalog_sha256": "0" * 64}]
        for options in variants:
            with self.subTest(options=options):
                ctx, snapshot, raw, context = rollback_documents(**options)
                value = public.validate_rollback_result(raw, snapshot, ctx)
                public.validate_rollback_evidence(context, raw, snapshot, ctx)
                self.assertFalse(public.accepted(value))
                if "catalog_sha256" in options:
                    self.assertEqual(value["catalog_sha256"], "0" * 64)
                    self.assertNotEqual(value["catalog_sha256"], snapshot["database_base"]["catalog"]["catalog_sha256"])

    def test_rollback_requires_exact_typed_main_dispatch_context(self):
        for change in ({"event": "push"}, {"ref": "refs/heads/other"}, {"target": None},
                       {"sha": "PRIVATE"}, {"run_id": True}, {"attempt": True}):
            ctx = replace(self.ctx, **change)
            self.assert_refused(lambda: public.validate_rollback_snapshot(self.snapshot, ctx))
        class ContextSubclass(public.Context):
            pass
        subclass = ContextSubclass(self.ctx.sha, self.ctx.run_id, self.ctx.attempt,
                                   self.ctx.target, self.ctx.event, self.ctx.ref)
        self.assert_refused(lambda: public._rollback_context(subclass))

    def test_snapshot_refuses_wrong_target_source_span_or_bool_fields(self):
        mutations = (
            lambda v: v.update(version=True), lambda v: v.update(run_id=True),
            lambda v: v.update(run_attempt=True), lambda v: v.update(extra="PRIVATE"),
            lambda v: v.update(sha=v["live_web_source_sha"]),
            lambda v: v.update(requested_rollback_target=v["latest"]["release"]),
            lambda v: v["span"].update(non_none=1), lambda v: v["span"].update(records=True),
            lambda v: v.update(source_material_sha256="PRIVATE"),
            lambda v: v["preview"].update(blocked=0),
        )
        for mutate in mutations:
            value = copy.deepcopy(self.snapshot); mutate(value)
            self.assert_refused(lambda: public.validate_rollback_snapshot(value, self.ctx))

    def test_result_cannot_substitute_live_image_or_another_producer(self):
        for change in ({"sha": self.snapshot["live_web_source_sha"]},
                       {"previous_release": self.snapshot["latest"]["release"]},
                       {"previous_web_digest": self.snapshot["latest"]["web_digest"]},
                       {"run_attempt": self.ctx.attempt + 1}):
            value = json.loads(self.raw); value.update(change)
            self.assert_refused(lambda: public.validate_rollback_result(
                public.canonical(value), self.snapshot, self.ctx))

    def test_rollback_context_digest_and_canonical_framing_are_exact(self):
        value = json.loads(self.context); value["result_sha256"] = "0" * 64
        for raw in (public.canonical(value), self.context[:-1], self.context + b"\n",
                    b"x" * (public._MAX_CONTEXT + 1)):
            self.assert_refused(lambda: public.validate_rollback_evidence(
                raw, self.raw, self.snapshot, self.ctx))

    def test_normal_and_rollback_snapshots_and_contexts_are_not_interchangeable(self):
        ctx = public.context(normal_fixture.environment())
        snapshot = normal_fixture.snapshot(ctx)
        raw = public.proof.result_bytes(normal_fixture.result(ctx))
        context = public.canonical(public.evidence_context(raw, snapshot, ctx))
        self.assert_refused(lambda: public.validate_snapshot(self.snapshot, ctx))
        self.assert_refused(lambda: public.validate_rollback_snapshot(snapshot, self.ctx))
        self.assert_refused(lambda: public.validate_evidence(self.context, raw, snapshot, ctx))
        self.assert_refused(lambda: public.validate_rollback_evidence(context, self.raw, self.snapshot, self.ctx))


if __name__ == "__main__":
    unittest.main()
