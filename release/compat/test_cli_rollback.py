"""Private rollback producer with actual source/engine/proof, fake service edges.

Captured signatures and immutable source responses are synthetic. The tests do
not authenticate a real release, run Docker, or authorize an operational gate.
"""
from dataclasses import replace
import contextlib
import copy
import hashlib
import io
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
import test_cli as normal_tests


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
        self.assertEqual(self.deadline.end, self.started + 4500)
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


class PublicRollbackCLITests(unittest.TestCase):
    """Public files/commands with real source, engine, proof and verifier code.

    Only the clock, capture, build and owned-service boundaries are controlled.
    Synthetic certificates and service evidence are not hosted qualification.
    Existing private-helper tests above remain independently discoverable.
    """
    @classmethod
    def setUpClass(cls):
        RollbackCLITests.setUpClass()

    def setUp(self):
        RollbackCLITests.setUp(self)
        self.runner = self.case.root / 'runner'
        self.runner.mkdir()
        self.inputs = self.runner / 'compat-input'
        self.inputs.mkdir()
        self.output = self.runner / 'step-output'
        self.output.touch()
        self.docker, self.plugin = self.case.root / 'docker.exe', self.case.root / 'buildx.exe'
        self.docker.write_bytes(b'controlled boundary'); self.plugin.write_bytes(b'controlled boundary')
        self.env = normal_tests.environment('workflow_dispatch', attempt='2')
        self.env.update(GITHUB_SHA=self.ctx.sha, GITHUB_WORKFLOW_SHA=self.ctx.sha,
            GITHUB_RUN_ID=str(self.ctx.run_id), COMPAT_ROLLBACK_TARGET=self.ctx.target,
            RUNNER_TEMP=str(self.runner), GITHUB_OUTPUT=str(self.output),
            COMPAT_PREVIEW_OUTCOME='success', COMPAT_RUN_OUTCOME='success',
            COMPAT_GIT_BIN=str(self.case.git), COMPAT_GH_BIN=str(self.case.gh),
            COMPAT_DOCKER_BIN=str(self.docker), COMPAT_BUILDX_BIN=str(self.plugin),
            GH_TOKEN=engine_tests.tr.support.GH_TOKEN)
        self.public_clients, self.public_deadlines, self.admissions = [], [], []

    def public(self, command, **changes):
        env = {**self.env, **changes}
        self.output.write_bytes(b'')
        init, make_client = cli.images._Deadline.__init__, cli._client
        def clock_init(deadline):
            init(deadline, clock=self.w.clock)
            self.public_deadlines.append((deadline, self.w.clock.now))
        def client(work, actual_env, deadline):
            self.assertIs(deadline, self.public_deadlines[-1][0])
            value = make_client(work, actual_env, deadline)
            self.public_clients.append(value)
            return value
        def capture(argv, **options):
            if argv[0] in (str(self.case.gh), str(self.case.git)):
                return self.case.world(argv, **options)
            return self.w.owner.raw_capture(argv, **options)
        def build(repo, resolution, binary, config, work, deadline, *, buildx_binary):
            self.assertIs(deadline, self.public_deadlines[-1][0])
            self.assertEqual(deadline.end, self.public_deadlines[-1][1] + 4500)
            self.assertEqual(resolution, self.resolution)
            self.assertTrue(self.public_clients[-1].budget.closed)
            self.assertIsNone(cli.records._GIT_OUTPUT_TRANSPORT.get())
            self.admissions.append((resolution, deadline.end))
            self.w.event('admission.rollback')
            return self.admission
        with mock.patch.dict(cli.os.environ, env, clear=True), \
             mock.patch.object(Path, 'cwd', return_value=self.case.repo), \
             mock.patch.object(cli.images._Deadline, '__init__', clock_init), \
             mock.patch.object(cli, '_client', side_effect=client), \
             mock.patch.object(cli.runtime, 'bounded_command', side_effect=capture), \
             mock.patch.object(cli.images, '_admit_rollback_resolved', side_effect=build), \
             mock.patch.object(cli.images, '_admit_resolved', side_effect=AssertionError('normal admission')), \
             mock.patch.object(cli, 'preview', side_effect=AssertionError('normal preview fallback')), \
             mock.patch.object(cli, 'execute', side_effect=AssertionError('normal execution fallback')), \
             mock.patch.object(cli, '_write_evidence', side_effect=AssertionError('normal writer fallback')), \
             contextlib.redirect_stdout(io.StringIO()) as stdout, \
             contextlib.redirect_stderr(io.StringIO()) as stderr:
            status = cli.main([command])
        self.last_stdout, self.last_stderr = stdout.getvalue(), stderr.getvalue()
        self.assertNotIn('PRIVATE', self.last_stderr)
        self.assertNotIn(engine_tests.tr.support.GH_TOKEN, self.last_stderr)
        self.assertTrue(all(client.budget.closed for client in self.public_clients))
        self.assertIsNone(cli.records._GIT_OUTPUT_TRANSPORT.get())
        return status

    def directory(self, command):
        return self.runner / ('compat-' + command + '-' + self.ctx.suffix) / 'public'

    def stage_preview(self):
        self.assertEqual(self.public('preview'), 0, self.last_stderr)
        raw = (self.directory('preview') / 'resolution.json').read_bytes()
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertEqual(self.public_deadlines[-1][0].end, self.public_deadlines[-1][1] + 600)
        (self.inputs / 'resolution.json').write_bytes(raw)
        return json.loads(raw)

    def stage_run(self):
        snapshot = self.stage_preview()
        self.assertEqual(self.public('run'), 0, self.last_stderr)
        raw = (self.directory('run') / 'compat-result.json').read_bytes()
        (self.inputs / 'compat-result.json').write_bytes(raw)
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertEqual(len(self.admissions), 1)
        self.assertEqual(self.w.trace[-4:], ['ledger.seal', 'catalog.final', 'manager.close', 'stack.cleanup'])
        return snapshot, raw

    def published(self, negative=False):
        snapshot, raw = self.stage_run()
        calls = len(self.case.world.trace)
        self.assertEqual(self.public('check'), int(negative), self.last_stderr)
        self.assertEqual(self.output.read_text(), 'complete=true\n')
        directory = self.directory('check')
        self.assertEqual({p.name for p in directory.iterdir()}, {'compat-result.json', 'compat-context.json'})
        self.assertEqual((directory / 'compat-result.json').read_bytes(), raw)
        context_raw = (directory / 'compat-context.json').read_bytes()
        self.assertEqual(json.loads(context_raw)['version'], 4)
        self.assertEqual(json.loads(context_raw)['result_sha256'], hashlib.sha256(raw).hexdigest())
        (self.inputs / 'compat-context.json').write_bytes(context_raw)
        self.assertEqual(self.public('validate-evidence'), 0, self.last_stderr)
        self.assertEqual(self.output.read_text(), 'complete=true\n')
        self.assertEqual({p.name: p.read_bytes() for p in self.directory('validate-evidence').iterdir()},
                         {p.name: p.read_bytes() for p in directory.iterdir()})
        self.assertEqual(len(self.case.world.trace), calls)  # No source/engine work in either file-only command.
        measurement, pair, attempt = RollbackCLITests.measured(self, raw, snapshot)
        self.assertEqual(pair.context, context_raw)
        self.assertEqual(measurement.positive, not negative)
        return measurement, pair, attempt

    def refuse(self, command, **changes):
        self.assertEqual(self.public(command, **changes), 1)
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertFalse((self.directory(command) / 'compat-result.json').exists())

    def test_public_positive_crosses_all_commands_and_both_real_subject_verifiers(self):
        measured, pair, _ = self.published()
        self.assertEqual(measured.binding.producer_sha, self.ctx.sha)
        self.assertEqual(measured.binding.database_base.source_sha, '3' * 40)
        self.assertEqual(measured.binding.selected_web_source_sha, self.case.boot['sha'])
        self.assertEqual(measured.binding.live_web_source_sha, '4' * 40)
        self.assertLessEqual(len(pair.context), 4096)
        self.assertEqual(self.last_stderr, '')
        with self.assertRaisesRegex(cli.record_resolver.policy.PolicyRefusal, 'rollback-qualification-unavailable'):
            cli.record_resolver.policy.rollback_plan(self.resolution.resolved.chain, self.ctx.target)

    def test_public_actual_old_migration_false_is_complete_signable_and_failed_check(self):
        self.w.codes['compat-n1-rw'] = 1
        measured, pair, _ = self.published(negative=True)
        self.assertFalse(json.loads(pair.result)['old_migrate_ok']['rw'])
        self.assertFalse(measured.reversible)

    def test_public_actual_read_and_reverse_false_are_complete_signable_and_failed_check(self):
        self.w.false_case = 'read'
        _, pair, _ = self.published(negative=True)
        value = json.loads(pair.result)
        self.assertFalse(value['old_server_ok']['read']); self.assertFalse(value['old_server_ok']['reverse'])
        self.assertTrue(value['reversible'])

    def test_public_actual_worker_false_is_complete_signable_and_failed_check(self):
        self.w.worker_panic = 1
        _, pair, _ = self.published(negative=True)
        self.assertFalse(json.loads(pair.result)['old_server_ok']['workers'])

    def test_public_actual_catalog_false_retains_measured_hash(self):
        original = cli.compat.catalog.OwnedCatalog
        class ChangedFinal(original):
            def observe(inner, inputs):
                super().observe(inputs)
                return inner.observation('e' * 64)
        with mock.patch.object(cli.compat.catalog, 'OwnedCatalog', ChangedFinal):
            measured, pair, _ = self.published(negative=True)
        self.assertEqual(measured.catalog_sha256, 'e' * 64)
        self.assertEqual(json.loads(pair.result)['catalog_sha256'], 'e' * 64)
        self.assertNotEqual(measured.catalog_sha256, measured.binding.database_base.catalog.catalog_sha256)

    def test_public_terminal_target_panic_is_H_bound_measured_negative(self):
        self.w.terminal_old_boot = True
        measured, _, _ = self.published(negative=True)
        self.assertFalse(any(measured.servers))
        self.assertEqual(self.w.trace.count('controller.read'), 0)

    def test_cleanup_failure_does_not_publish_or_mark_complete(self):
        self.stage_preview(); self.w.cleanup_fail = True
        self.refuse('run')
        self.assertEqual(self.w.trace[-1], 'stack.cleanup')

    def test_incomplete_capture_does_not_publish_a_semantic_negative(self):
        self.stage_preview(); self.w.false_case = 'read'; self.w.fail = 'catalog.final'
        self.refuse('run')
        self.assertTrue(self.w.owner._closed)

    def test_target_rejected_on_push_PR_or_nonmain_dispatch_before_any_effect(self):
        calls = len(self.case.world.trace)
        for event, ref in (('push', 'refs/heads/main'), ('pull_request', 'refs/pull/9/merge'),
                           ('workflow_dispatch', 'refs/heads/other')):
            for command in ('preview', 'run', 'check', 'validate-evidence'):
                with self.subTest(event=event, command=command):
                    self.refuse(command, GITHUB_EVENT_NAME=event, GITHUB_REF=ref,
                                GITHUB_WORKFLOW_REF=cli._WORKFLOW + ref)
        self.assertEqual(len(self.case.world.trace), calls)
        self.assertEqual(self.public_deadlines, [])
        self.assertEqual(set(self.runner.iterdir()), {self.inputs, self.output})

    def test_disabled_target_route_precedes_context_and_source(self):
        calls = len(self.case.world.trace)
        with mock.patch.object(cli.images, '_READY_FOR_EXECUTION', False):
            self.refuse('preview', GITHUB_SHA='PRIVATE invalid')
        self.assertEqual(len(self.case.world.trace), calls)
        self.assertEqual(self.public_deadlines, [])

    def test_wrong_workflow_identity_refuses_before_client(self):
        calls = len(self.case.world.trace)
        self.refuse('preview', GITHUB_WORKFLOW_SHA='b' * 40)
        self.assertEqual(len(self.case.world.trace), calls)
        self.assertEqual(self.public_deadlines, [])

    def test_foreign_run_snapshot_refuses_before_fresh_source(self):
        self.stage_preview(); calls = len(self.case.world.trace)
        self.refuse('run', GITHUB_RUN_ID='12346')
        self.assertEqual(len(self.case.world.trace), calls)

    def test_foreign_attempt_snapshot_refuses_before_fresh_source(self):
        self.stage_preview(); calls = len(self.case.world.trace)
        self.refuse('run', GITHUB_RUN_ATTEMPT='3')
        self.assertEqual(len(self.case.world.trace), calls)

    def test_fresh_latest_drift_refuses_before_image_admission(self):
        self.stage_preview(); latest = self.case.add(); self.case.profile(latest['sha'])
        self.refuse('run'); self.assertEqual(self.admissions, [])
        self.assertEqual(self.w.trace, [])

    def test_fresh_main_drift_refuses_before_image_admission(self):
        self.stage_preview(); self.case.world.main = 'b' * 40
        self.refuse('run'); self.assertEqual(self.admissions, [])

    def test_fresh_endpoint_recipe_drift_refuses_before_image_admission(self):
        self.stage_preview()
        self.case.world.files[self.case.boot['sha'], self.case.RECIPE] += b'# changed source\n'
        self.refuse('run'); self.assertEqual(self.admissions, [])

    def test_span_commitment_tamper_refuses_after_real_fresh_resolution(self):
        snapshot = self.stage_preview(); calls = len(self.case.world.trace)
        snapshot['span']['sha256'] = 'f' * 64
        (self.inputs / 'resolution.json').write_bytes(cli.canonical(snapshot))
        self.refuse('run'); self.assertEqual(self.admissions, [])
        self.assertGreater(len(self.case.world.trace), calls)

    def test_source_material_commitment_tamper_refuses_after_real_fresh_resolution(self):
        snapshot = self.stage_preview(); snapshot['source_material_sha256'] = 'f' * 64
        (self.inputs / 'resolution.json').write_bytes(cli.canonical(snapshot))
        self.refuse('run'); self.assertEqual(self.admissions, [])

    def test_unknown_target_is_not_a_normal_preview_fallback(self):
        self.refuse('preview', COMPAT_ROLLBACK_TARGET='prod-20261011T000000Z-ffffffff')
        self.assertFalse((self.directory('preview') / 'resolution.json').exists())
        self.assertEqual(self.admissions, [])

    def test_zero_change_target_does_not_create_a_proof_or_admission(self):
        self.refuse('preview', COMPAT_ROLLBACK_TARGET=self.case.documents[4]['release'])
        self.assertFalse((self.directory('preview') / 'resolution.json').exists())
        self.assertEqual(self.admissions, [])

    def test_one_change_target_does_not_create_a_proof_or_admission(self):
        self.refuse('preview', COMPAT_ROLLBACK_TARGET=self.case.documents[3]['release'])
        self.assertFalse((self.directory('preview') / 'resolution.json').exists())
        self.assertEqual(self.admissions, [])

    def test_all_v1_history_does_not_invent_retained_live_authority(self):
        case = engine_tests.trb.RollbackProofResolverTests()
        case.setUp(); self.addCleanup(case.doCleanups)
        target = case.add(version=1); latest = case.add(version=1)
        for source in (target['sha'], latest['sha'], self.ctx.sha):
            case.profile(source)
        self.case = case
        self.env.update(COMPAT_GIT_BIN=str(case.git), COMPAT_GH_BIN=str(case.gh),
                        COMPAT_ROLLBACK_TARGET=target['release'])
        self.refuse('preview'); self.assertEqual(self.admissions, [])
        self.assertFalse((self.directory('preview') / 'resolution.json').exists())

    def test_unequal_seven_file_source_material_refuses_before_engine(self):
        self.case.world.files[self.ctx.sha, cli.record_resolver.policy.STEPS] += b'\n'
        self.refuse('preview'); self.assertEqual(self.admissions, [])

    def test_constructor_work_consumes_original_preview_deadline(self):
        def elapsed(argv):
            if argv == [str(self.case.gh), '--version']:
                self.w.clock.sleep(601)
        self.case.world.before = elapsed
        self.refuse('preview')
        self.assertEqual(self.public_deadlines[0][0].end, self.public_deadlines[0][1] + 600)
        self.assertEqual(self.admissions, [])

    def test_run_constructor_cannot_reset_original_deadline(self):
        self.stage_preview()
        def elapsed(argv):
            if argv == [str(self.case.gh), '--version']:
                self.w.clock.sleep(4501)
        self.case.world.before = elapsed
        self.refuse('run'); self.assertEqual(self.admissions, [])
        self.assertEqual(self.public_deadlines[-1][0].end, self.public_deadlines[-1][1] + 4500)

    def test_normal_snapshot_does_not_select_normal_parser_for_explicit_target(self):
        snapshot = normal_tests.snapshot(self.ctx)
        (self.inputs / 'resolution.json').write_bytes(cli.canonical(snapshot))
        calls = len(self.case.world.trace)
        self.refuse('run'); self.assertEqual(len(self.case.world.trace), calls)

    def test_rollback_snapshot_cannot_select_lane_when_target_absent(self):
        self.stage_preview(); calls = len(self.case.world.trace)
        self.refuse('run', COMPAT_ROLLBACK_TARGET='')
        self.assertEqual(len(self.case.world.trace), calls)

    def test_upstream_failure_or_cancel_never_publishes_complete_measurement(self):
        self.stage_run()
        for outcome in ('failure', 'cancelled', 'skipped', ''):
            with self.subTest(outcome=outcome):
                self.refuse('check', COMPAT_RUN_OUTCOME=outcome)

    def test_partial_context_write_never_marks_complete(self):
        self.stage_run(); original = cli._write_public
        def partial(path, raw):
            if path.name == 'compat-context.json':
                path.write_bytes(raw[:1]); raise OSError('PRIVATE partial capture')
            return original(path, raw)
        with mock.patch.object(cli, '_write_public', side_effect=partial):
            self.assertEqual(self.public('check'), 1)
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertEqual((self.directory('check') / 'compat-context.json').read_bytes(), b'{')

    def test_readback_tamper_never_marks_complete(self):
        self.stage_run(); original = cli._write_public
        def tamper(path, raw):
            original(path, raw)
            if path.name == 'compat-context.json':
                (path.parent / 'compat-result.json').write_bytes(b'{}\n')
        with mock.patch.object(cli, '_write_public', side_effect=tamper):
            self.assertEqual(self.public('check'), 1)
        self.assertEqual(self.output.read_bytes(), b'')

    def test_extra_public_file_never_marks_complete(self):
        self.stage_run(); original = cli._write_public
        def extra(path, raw):
            original(path, raw)
            if path.name == 'compat-context.json':
                (path.parent / 'extra.json').write_bytes(b'{}\n')
        with mock.patch.object(cli, '_write_public', side_effect=extra):
            self.assertEqual(self.public('check'), 1)
        self.assertEqual(self.output.read_bytes(), b'')

    def test_signer_rejects_missing_foreign_or_noncanonical_context_without_readiness(self):
        snapshot, raw = self.stage_run()
        context_raw = cli.canonical(cli.rollback_evidence_context(raw, snapshot, self.ctx))
        path = self.inputs / 'compat-context.json'
        self.refuse('validate-evidence')
        for changes in ({'version': 3}, {'result_sha256': 'f' * 64}, {'run_attempt': 3}, {'extra': True}):
            value = json.loads(context_raw); value.update(changes)
            path.write_bytes(cli.canonical(value))
            with self.subTest(changes=changes): self.refuse('validate-evidence')
        for data in (context_raw[:-1], context_raw + b' ', b' ' * 4097):
            path.write_bytes(data); self.refuse('validate-evidence')
        path.write_bytes(context_raw); (self.inputs / 'extra.json').write_bytes(b'{}\n')
        self.refuse('validate-evidence')

    def test_signed_public_bytes_require_both_exact_subjects_and_invocation(self):
        _, pair, attempt = self.published()
        for mutation in ('missing', 'digest', 'attempt'):
            with self.subTest(mutation=mutation), adapter_tests.harness(attempt) as (root, client, fake, row):
                archive = adapter_tests.archive(pair)
                if mutation == 'missing':
                    archive = adapter_tests.archive(pair, [('compat-result.json', pair.result)])
                else:
                    def mutate(value, path):
                        if path.name != 'compat-context.json': return
                        if mutation == 'digest':
                            value[0]['verificationResult']['statement']['subject'][0]['digest']['sha256'] = 'f' * 64
                        else:
                            value[0]['verificationResult']['signature']['certificate']['runInvocationURI'] += '9'
                    fake.mutate = mutate
                with self.assertRaises(cli.compat_evidence.CompatRefusal):
                    adapter_tests.adapter.verify_rollback_pair(client, archive, adapter_tests.location(row), row, root)

    def test_same_attempt_scratch_replay_refuses_before_another_source_read(self):
        self.stage_preview(); calls = len(self.case.world.trace)
        self.assertEqual(self.public('preview'), 1)
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertEqual(len(self.case.world.trace), calls)


if __name__ == '__main__':
    unittest.main()
