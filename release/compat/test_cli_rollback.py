"""Private rollback producer with actual source/engine/proof, fake service edges.

Captured signatures and immutable source responses are synthetic. The tests do
not authenticate a real release, run Docker, or authorize an operational gate.
"""
from dataclasses import replace
import copy
import hashlib
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cli
import test_compat as engine_tests
import test_compat_evidence as pair_tests
import test_compat_adapter as adapter_tests


class RollbackCLITests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        engine_tests.RollbackExperimentTests.setUpClass()

    def setUp(self):
        # Reuse the complete source fixture and controlled owned services, not
        # its runner or favorable evaluator result. Every CLI run resolves anew.
        engine_tests.RollbackExperimentTests.setUp(self)
        self.clients = []
        self.ctx = cli.Context(engine_tests.SHA, 12345, 2,
            self.resolution.resolved.selected.release, 'workflow_dispatch', 'refs/heads/main')

    def client(self):
        root = self.case.root / ('cli-client-' + str(len(self.clients)))
        root.mkdir(); config, work = root / 'config', root / 'work'
        config.mkdir(); work.mkdir()
        budget = cli.record_transport.OperationBudget(self.deadline.end,
            capture=self.case.world, clock=self.deadline.clock)
        client = cli.record_transport.ReleaseGh(self.case.gh, engine_tests.tr.support.GH_TOKEN,
            config, work, budget=budget, base_env={})
        self.clients.append(client)
        self.addCleanup(budget.close)
        return client

    def preview(self):
        client = self.client()
        result = cli._preview_rollback(self.case.repo, self.ctx, client,
            git_binary=self.case.git, deadline=self.deadline)
        self.assertTrue(client.budget.closed)
        return result

    def execute(self, snapshot):
        client, end = self.client(), self.deadline.end
        source_before = len(self.case.world.trace)
        def build(repo, resolution, binary, config, work, deadline, *, buildx_binary):
            self.assertIs(deadline, self.deadline)
            self.assertEqual(deadline.end, end)
            self.assertGreater(len(self.case.world.trace), source_before)
            self.assertTrue(client.budget.closed)
            self.assertEqual(resolution.requirements, self.resolution.requirements)
            self.w.event('admission.rollback')
            return self.admission
        with mock.patch.object(cli.images, '_admit_rollback_resolved', side_effect=build), \
             mock.patch.object(cli.images, '_admit_resolved', side_effect=AssertionError('normal admission')), \
             mock.patch.object(cli.runtime, 'verified_previous', side_effect=AssertionError('legacy verification')), \
             mock.patch.object(cli.runtime, 'bounded_command',
                               side_effect=lambda *a, **kw: self.w.owner.raw_capture(*a, **kw)):
            return cli._execute_rollback(self.case.repo, self.ctx, client, snapshot,
                git_binary=self.case.git, deadline=self.deadline, docker_binary='docker',
                config_dir=self.p, image_work_dir=self.p, buildx_binary=None)

    def measured(self, raw, snapshot):
        context = cli.canonical(cli.rollback_evidence_context(raw, snapshot, self.ctx))
        self.assertEqual(cli.validate_rollback_evidence(context, raw, snapshot, self.ctx), json.loads(context))
        original = pair_tests.pack(raw, context, self.ctx.run_id, self.ctx.attempt)
        attempt = pair_tests.attempt(self.ctx.run_id, rerun=self.ctx.attempt,
            measurement=original, event='workflow_dispatch',
            conclusion='success' if cli.accepted(json.loads(raw)) else 'failure')
        # The real fixed Gh verifier consumes synthetic certificate/statement
        # transcripts for these actual producer bytes; no favorable validator
        # is replaced. This is still synthetic, not hosted authentication.
        with adapter_tests.harness(attempt) as (root, client, fake, row):
            pair = adapter_tests.adapter.verify_rollback_pair(client, adapter_tests.archive(original),
                adapter_tests.location(row), row, root)
            self.assertEqual(len(fake.calls), 3)  # Pinned version, then both subjects.
            self.assertEqual([Path(args[3]).name for args, _ in fake.calls[1:]],
                             ['compat-result.json', 'compat-context.json'])
        self.assertEqual(pair.result, raw); self.assertEqual(pair.context, context)
        measurement = cli.compat_evidence.parse_authenticated_rollback_pair(pair, attempt)
        return measurement, pair, attempt

    def test_preview_derives_distinct_H_target_live_origin_and_complete_span(self):
        snap = self.preview()
        self.assertEqual(snap['version'], 1)
        self.assertEqual(snap['purpose'], 'rollback-v0')
        self.assertEqual(snap['sha'], engine_tests.SHA)
        self.assertEqual(snap['selected']['release'], self.ctx.target)
        self.assertEqual(snap['selected']['sha'], '1' * 40)
        self.assertEqual(snap['selected_web_source_sha'], self.case.boot['sha'])
        self.assertEqual(snap['latest']['sha'], '4' * 40)
        self.assertEqual(snap['live_web_source_sha'], '4' * 40)
        self.assertEqual(snap['database_base']['source_sha'], '3' * 40)
        self.assertEqual(snap['span']['records'], len(self.resolution.requirements.span))
        self.assertEqual(snap['span']['non_none'], 2)
        self.assertEqual(snap['span']['sha256'], self.resolution.requirements.span_sha256)
        self.assertEqual(snap['source_material_sha256'], cli.record_resolver.rollback_material_digest(self.resolution))
        self.assertEqual(snap['preview'], {'kind': 'none', 'dormant': False, 'blocked': False})
        self.assertEqual(self.w.trace, [])
        self.assertFalse(hasattr(self.w, 'owner'))
        self.assertIsNone(cli.records._GIT_OUTPUT_TRANSPORT.get())

    def test_fresh_source_to_real_engine_to_context4_preserves_all_roles_and_cleanup(self):
        snap = self.preview(); raw = self.execute(snap)
        measured, pair, _ = self.measured(raw, snap)
        self.assertTrue(measured.positive)
        self.assertEqual(measured.binding.producer_sha, self.ctx.sha)
        self.assertEqual(measured.binding.selected_web_source_sha, self.case.boot['sha'])
        self.assertEqual(measured.binding.live_web_source_sha, '4' * 40)
        self.assertEqual(measured.binding.database_base.source_sha, '3' * 40)
        self.assertEqual(measured.result_sha256, hashlib.sha256(raw).hexdigest())
        self.assertEqual(measured.context_sha256, hashlib.sha256(pair.context).hexdigest())
        self.assertLessEqual(len(pair.context), 4096)
        self.assertEqual([name for name, _ in self.w.image_roles], [
            'web.previous', 'compat-fwd', 'compat-fwd', 'web.candidate', 'compat-n1-rw',
            'compat-n1-ro', 'web.previous', 'web.candidate'])
        self.assertEqual(self.w.trace[-4:], ['ledger.seal', 'catalog.final', 'manager.close', 'stack.cleanup'])
        self.assertTrue(self.w.owner._closed); self.assertTrue(self.w.manager._closed)
        self.assertTrue(all(client.budget.closed for client in self.clients))
        self.assertEqual(self.deadline.end, self.started + 2400)
        self.assertEqual(self.w.clock.now, self.started + 180)  # Three constructors plus actual quiet interval.

    def test_complete_actual_read_false_is_signed_shape_evidence_with_real_reverse(self):
        self.w.false_case = 'read'
        snap = self.preview(); raw = self.execute(snap)
        measured, _, _ = self.measured(raw, snap)
        result = json.loads(raw)
        self.assertFalse(measured.positive)
        self.assertFalse(result['old_server_ok']['read'])
        self.assertFalse(result['old_server_ok']['reverse'])
        self.assertTrue(result['reversible'])
        self.assertEqual(self.w.trace.count('controller.read'), 2)
        self.assertIn('pg.control', self.w.trace)
        self.assertTrue(self.w.owner._closed)

    def test_complete_measured_catalog_mismatch_retains_actual_hash_not_base(self):
        old = cli.compat.catalog.OwnedCatalog
        class ChangedFinalCatalog(old):
            def observe(inner, inputs):
                super().observe(inputs)
                return inner.observation('e' * 64)
        snap = self.preview()
        with mock.patch.object(cli.compat.catalog, 'OwnedCatalog', ChangedFinalCatalog):
            raw = self.execute(snap)
        measured, _, _ = self.measured(raw, snap)
        self.assertEqual(measured.catalog_sha256, 'e' * 64)
        self.assertFalse(measured.positive)
        self.assertFalse(json.loads(raw)['old_server_ok']['reverse'])
        self.assertNotEqual(measured.catalog_sha256, measured.binding.database_base.catalog.catalog_sha256)
        self.assertEqual(json.loads(raw)['catalog_sha256'], 'e' * 64)
        self.assertTrue(self.w.owner._closed)

    def test_actual_terminal_target_panic_is_H_bound_false_not_fabricated_reverse(self):
        self.w.terminal_old_boot = True
        snap = self.preview(); raw = self.execute(snap)
        measured, _, _ = self.measured(raw, snap)
        self.assertFalse(measured.positive); self.assertFalse(any(measured.servers))
        self.assertEqual(json.loads(raw)['sha'], self.ctx.sha)
        self.assertEqual(self.w.trace.count('controller.read'), 0)
        self.assertEqual(self.w.trace.count('web.candidate'), 1)
        self.assertIn('pg.control', self.w.trace)
        self.assertTrue(self.w.owner._closed)

    def test_cleanup_refusal_never_returns_even_after_complete_measurement(self):
        snap = self.preview(); self.w.cleanup_fail = True
        with self.assertRaisesRegex(cli.proof.EvidenceError, 'cleanup'):
            self.execute(snap)
        self.assertIn('catalog.final', self.w.trace)
        self.assertEqual(self.w.trace[-1], 'stack.cleanup')

    def test_incomplete_late_capture_never_becomes_false_result(self):
        snap = self.preview(); self.w.false_case = 'read'; self.w.fail = 'catalog.final'
        with self.assertRaises(cli.proof.InfrastructureFailure):
            self.execute(snap)
        self.assertTrue(self.w.owner._closed)

    def test_fresh_latest_change_refuses_before_build_or_owned_resources(self):
        snap = self.preview()
        latest = self.case.add()
        self.case.profile(latest['sha'])
        with self.assertRaisesRegex(cli.proof.EvidenceError, 'cli-resolution-changed'):
            self.execute(snap)
        self.assertEqual(self.w.trace, [])
        self.assertTrue(self.clients[-1].budget.closed)

    def test_fresh_recipe_change_refuses_instead_of_using_old_commitment(self):
        snap = self.preview()
        self.case.world.files[self.case.boot['sha'], self.case.RECIPE] += b'# changed captured source\n'
        with self.assertRaisesRegex(cli.proof.EvidenceError, 'cli-resolution-changed'):
            self.execute(snap)
        self.assertEqual(self.w.trace, [])

    def test_fresh_main_change_never_relabels_control(self):
        snap = self.preview()
        self.case.world.main = 'b' * 40
        with self.assertRaises((cli.common.ReleaseError, cli.proof.EvidenceError)):
            self.execute(snap)
        self.assertEqual(self.w.trace, [])

    def test_validly_shaped_snapshot_tampering_still_requires_fresh_complete_equality(self):
        original = self.preview()
        for path, value in [(('latest', 'manifest_sha256'), 'f' * 64),
                            (('selected', 'web_digest'), 'sha256:' + 'f' * 64),
                            (('span', 'sha256'), 'f' * 64),
                            (('source_material_sha256',), 'f' * 64),
                            (('live_web_source_sha',), 'f' * 40)]:
            snap = copy.deepcopy(original)
            target = snap if len(path) == 1 else snap[path[0]]
            target[path[-1]] = value
            with self.subTest(path=path), self.assertRaisesRegex(cli.proof.EvidenceError, 'cli-resolution-changed'):
                self.execute(snap)
        self.assertEqual(self.w.trace, [])

    def test_snapshot_rejects_wrong_protocol_type_count_and_unknown_fields(self):
        original = self.preview()
        bad = []
        for key, value in [('version', True), ('purpose', 'normal'), ('run_id', True),
                           ('run_attempt', 0), ('requested_rollback_target', None), ('extra', 'PRIVATE')]:
            snap = copy.deepcopy(original); snap[key] = value; bad.append(snap)
        for key, value in [('records', True), ('records', 502), ('non_none', 1), ('non_none', 999)]:
            snap = copy.deepcopy(original); snap['span'][key] = value; bad.append(snap)
        for key, value in [('blocked', 0), ('dormant', 0), ('kind', 'data')]:
            snap = copy.deepcopy(original); snap['preview'][key] = value; bad.append(snap)
        snap = copy.deepcopy(original); del snap['database_base']; bad.append(snap)
        for snap in bad:
            with self.subTest(snapshot=snap), self.assertRaises(cli.proof.EvidenceError):
                cli.validate_rollback_snapshot(snap, self.ctx)
        self.assertEqual(self.w.trace, [])

    def test_dispatch_main_explicit_target_and_exact_numeric_context_are_required(self):
        snap = self.preview()
        for changes in ({'event': 'push'}, {'ref': 'refs/pull/9/merge'}, {'target': None},
                        {'run_id': True}, {'attempt': True}, {'attempt': 10000}, {'sha': 'bad'}):
            with self.subTest(changes=changes), self.assertRaises(cli.proof.EvidenceError):
                cli.validate_rollback_snapshot(snap, replace(self.ctx, **changes))

    def test_result_identity_and_canonical_bytes_are_bound_without_false_flag_rewrite(self):
        snap = self.preview(); raw = self.execute(snap); original = json.loads(raw)
        for key, value in [('sha', 'b' * 40), ('run_id', 12346), ('run_attempt', 3),
                           ('previous_release', snap['latest']['release']),
                           ('previous_web_digest', snap['latest']['web_digest']),
                           ('classification', {'kind': 'data', 'dormant': True})]:
            changed = copy.deepcopy(original); changed[key] = value
            with self.subTest(key=key), self.assertRaises(cli.proof.EvidenceError):
                cli.validate_rollback_result(cli.proof.result_bytes(changed), snap, self.ctx)
        for changed in (raw.rstrip(b'\n'), raw + b' ', raw + b'\n', b'X' * 16385):
            with self.subTest(raw=changed[:20]), self.assertRaises((cli.common.ReleaseError, cli.proof.EvidenceError)):
                cli.validate_rollback_result(changed, snap, self.ctx)

    def test_context_requires_complete_canonical_lf_hash_and_fixed_framing(self):
        snap = self.preview(); raw = self.execute(snap)
        context = cli.canonical(cli.rollback_evidence_context(raw, snap, self.ctx))
        self.assertLessEqual(len(context), 4096)
        altered = json.loads(context); altered['result_sha256'] = 'f' * 64
        extra = json.loads(context); extra['extra'] = 'PRIVATE'
        duplicate = context.replace(b'{', b'{"version":4,', 1)
        for value in (context.rstrip(b'\n'), context + b' ', cli.canonical(altered),
                      cli.canonical(extra), duplicate, b'X' * 4097):
            with self.subTest(raw=value[:20]), self.assertRaises((cli.common.ReleaseError, cli.proof.EvidenceError)):
                cli.validate_rollback_evidence(value, raw, snap, self.ctx)

    def test_new_namespace_cannot_enter_normal_parser_or_public_multispan_policy(self):
        snap = self.preview(); raw = self.execute(snap)
        _, pair, attempt = self.measured(raw, snap)
        with self.assertRaises(cli.proof.EvidenceError):
            cli.validate_snapshot(snap, self.ctx)
        with self.assertRaises(cli.compat_evidence.CompatRefusal):
            cli.compat_evidence.parse_authenticated_pair(pair, attempt)
        with self.assertRaisesRegex(cli.record_resolver.policy.PolicyRefusal, 'rollback-qualification-unavailable'):
            cli.record_resolver.policy.rollback_plan(self.resolution.resolved.chain, self.ctx.target)

    def test_expired_original_deadline_refuses_before_another_source_operation(self):
        client = self.client(); calls = len(self.case.world.trace)
        self.w.clock.now = self.deadline.end
        with self.assertRaises(cli.proof.InfrastructureFailure):
            cli._preview_rollback(self.case.repo, self.ctx, client,
                git_binary=self.case.git, deadline=self.deadline)
        self.assertEqual(len(self.case.world.trace), calls)
        self.assertEqual(self.w.trace, [])
        with self.assertRaisesRegex(cli.record_transport.TransportRefusal, 'transport-deadline'):
            client.budget.close()
        self.assertTrue(client.budget.closed)


if __name__ == '__main__':
    unittest.main()
