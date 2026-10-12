"""Real ship/gate/codec/writer composition with synthetic command/provider I/O.

No gate, qualifier, source facts, writer, image verifier or codec is stubbed.
Only command captures, time, provider/health transports and commit-summary text
are synthetic. This suite complements the historical post-gate provider tests.
"""
import base64
from contextlib import ExitStack
from datetime import timedelta
import hashlib
import io
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest import mock

import ship
import ship_flow as flow
import test_ship_compat as fixtures
import test_ship_support as support
import test_record_writer as writer_fixture
import test_ship_rollback_compat as rollback_fixture
import test_ship_outage_compat as outage_fixture

c = ship.common


class CallerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fixtures.PreflightTests.setUpClass()

    def setUp(self):
        self.fixture = fixtures.PreflightTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.case = self.fixture.case
        root = self.case.root
        self.now = fixtures.actions.NOW
        self.sha = fixtures.fixtures.SHA
        self.images = dict(writer_fixture.images(self.fixture.expected).images)
        self.gh = self.case.world.fake
        self.gh.ci_green(self.sha)
        self.gh.attest_ship_images(self.images, self.sha)
        self.env = support.context_env(self.sha, root)
        self.env['SHIP_GH_BIN'] = str(self.case.gh)
        self.env['SHIP_MODE'] = 'promote'
        self.env['SHIP_TARGET_RELEASE'] = ''
        latest = self.fixture.expected.resolved.latest
        self.env.update(PLAN_LATEST_RELEASE=latest.release,
            PLAN_LATEST_MANIFEST_SHA256=latest.manifest_sha256,
            PLAN_TARGET_RELEASE='', PLAN_TARGET_MANIFEST_SHA256='')
        payload, digest = support.candidate_b64(self.images, self.sha, self.now - timedelta(hours=1))
        self.env.update(CANDIDATE_B64=payload, CANDIDATE_SHA256=digest)
        self.env.update(support.staging_evidence(c.loads_strict(base64.b64decode(payload)), digest,
            previous_sha=latest.actual_web_source_sha))
        self.target = root / 'target.json'; self.target.write_bytes(support.canonical(support.make_target()))
        self.pins = root / 'staging-pins.json'
        self.pins.write_bytes(support.canonical({'schema_version': 1, 'profile': 'staging',
            'team_uuid_sha256': 'd' * 64, 'app_id_sha256': 'e' * 64}))
        self.policy = root / 'ship.trivyignore'; self.policy.write_bytes(b'# no exceptions\n')
        self.do = support.FakeDO(spec=support.make_spec(dict(latest.images)), backups=[{
            'created_at': c.format_timestamp(self.now - timedelta(hours=2)), 'size_gigabytes': 0.03}])
        self.https = support.FakeHttps()
        self.output = io.StringIO()
        self.sleeps = []
        self.deps = ship.Deps(clock=lambda: self.now, sleeper=self.sleeps.append,
            opener=self.do, gh_runner=self.gh, https_request=self.https, stdout=self.output,
            repo_dir=self.case.repo, target_path=self.target, staging_pins_path=self.pins,
            trivy_policy_path=self.policy, poll_limit=8, smoke_rounds=2, smoke_delay=0)

    def run_command(self, *args):
        # These are the actual public preflight and record adapters. Both GH
        # clients instantiate and validate normally; only their processes stop
        # at the controlled stdout/stderr boundary.
        with ExitStack() as stack:
            stack.enter_context(mock.patch.object(flow.shutil, 'which', return_value=str(self.case.git)))
            clock_module = SimpleNamespace(monotonic=self.case.clock)
            for module in (ship, flow, flow.ship_compat, flow.ship_records,
                           flow.ship_rollback_compat, flow.ship_outage_compat):
                stack.enter_context(mock.patch.object(module, 'time', clock_module))
            # Keep the real datetime type and all concrete policy checks. Only
            # the existing private wall-clock seam receives deterministic UTC.
            original_preflight = flow.ship_compat._preflight
            def preflight(*args, **kwargs):
                kwargs['wall_clock'] = lambda: self.now
                return original_preflight(*args, **kwargs)
            stack.enter_context(mock.patch.object(flow.ship_compat, '_preflight', side_effect=preflight))
            original_outage = flow.ship_outage_compat._preflight
            def outage(*args, **kwargs):
                kwargs['wall_clock'] = lambda: self.now
                return original_outage(*args, **kwargs)
            stack.enter_context(mock.patch.object(flow.ship_outage_compat, '_preflight', side_effect=outage))
            stack.enter_context(mock.patch.object(flow.ship_compat.runtime, 'bounded_command', self.fixture.capture))
            stack.enter_context(mock.patch.object(ship, 'first_parent_commits', return_value=(0, [])))
            return ship.main(list(args), self.env, self.deps)

    def production(self, mode='promote'):
        self.env.update(SHIP_MODE=mode, SHIP_DO_TOKEN=support.DO_TOKEN,
            SHIP_TARGET_JSON=c.canonical_payload_bytes({'schema_version': 1,
                'app_id': support.APP_ID, 'postgres_cluster_id': support.PG_ID}).decode())
        return self.run_command('production')

    def outputs(self):
        return support.read_outputs(Path(self.env['GITHUB_OUTPUT']))

    def to_record(self):
        outputs = self.outputs()
        self.env.update(('REC_' + key.upper(), value) for key, value in outputs.items())
        return outputs

    def test_plan_and_production_each_capture_fresh_complete_evidence_then_write_v2(self):
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        calls = len(self.fixture.calls)
        plan_time = self.now
        self.now += timedelta(seconds=1)
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertGreater(len(self.fixture.calls), 2 * calls - 5)
        self.assertEqual(self.do.put_count(), 1)
        self.assertEqual(len(self.https.calls), 6)
        outputs = self.to_record()
        custody = flow.record_handoff.decode(
            {key: outputs[key] for key in flow.record_handoff.OUTPUT_KEYS},
            candidate_sha=self.sha, workflow_sha=self.sha,
            run_id=int(self.env['GITHUB_RUN_ID']), record_attempt=1)
        self.assertEqual(custody.drills.observed_at, self.now)
        self.assertGreater(custody.drills.observed_at, plan_time)
        self.assertFalse(custody.drills.required)
        self.assertEqual(outputs['handoff_version'], '6')
        self.assertIsNone(custody.contract_observation)
        self.assertIs(type(custody.resolution.pending), flow.record_resolver.pending_source.PendingMaterial)
        self.assertEqual(custody.resolution.pending, self.fixture.expected.pending)
        self.assertEqual(outputs['previous'], self.fixture.expected.resolved.latest.release)
        self.assertTrue(all(key in outputs for key in flow.record_handoff.OUTPUT_KEYS))
        self.assertNotIn('SHIP_DO_TOKEN', self.env)
        for _, _, options in self.fixture.calls:
            self.assertNotIn('SHIP_DO_TOKEN', options['env'])
            self.assertNotIn('SHIP_TARGET_JSON', options['env'])
        # Current main can advance while the record job starts. It cannot be
        # substituted into the original compatibility capture.
        self.case.world.main = 'f' * 40
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        path = Path(self.env['RUNNER_TEMP']) / 'ship-record' / 'release-manifest.json'
        raw = path.read_bytes(); document = c.loads_strict(raw)
        self.assertEqual((document['schema_version'], document['sha'], document['compat'], document['staging']),
                         (2, self.sha, 'ok', 'passed'))
        self.assertEqual(document['catalog_sha256'], fixtures.source.policy.V0_MEASUREMENT)
        # Model the separate attestation action at its signed-output boundary.
        self.gh.attested_files[hashlib.sha256(raw).hexdigest()] = self.sha
        self.assertEqual(self.run_command('record', '--stage', 'publish'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.run_command('record', '--stage', 'verify'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)
        self.assertEqual(self.gh.assets[document['release']], raw)
        # Record-only rerun after artifacts are gone must reuse exact published
        # bytes. No compatibility Actions requests may occur in this route.
        self.env['GITHUB_RUN_ATTEMPT'] = '2'
        for key in flow.record_handoff.OUTPUT_KEYS:
            self.env['REC_' + key.upper()] = ''
        self.fixture.world.capture = mock.Mock(side_effect=AssertionError('expired Actions'))
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(path.read_bytes(), raw)
        self.assertEqual(self.outputs()['state'], 'published')

    def test_stale_main_refuses_before_provider_client_or_success_outputs(self):
        self.case.world.main = 'f' * 40
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertEqual(self.do.requests, [])
        self.assertEqual(self.outputs(), {})
        self.assertIn('record-chain-invalid:transport-resolver', self.output.getvalue())

    def test_signed_old_server_failure_refuses_before_provider_client(self):
        self.fixture.rebind(server_false='read')
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertEqual(self.outputs(), {})

    def test_size_refusal_occurs_before_any_provider_io(self):
        with mock.patch.object(flow.record_handoff, 'MAX_ENCODED', 8192), \
                mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('record-invalid:handoff', self.output.getvalue())
        self.assertEqual(self.outputs(), {})

    def test_dry_run_still_performs_full_gate_without_put_or_handoff_emission(self):
        self.assertEqual(self.production('dry-run'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 0)
        self.assertTrue(self.do.requests)
        self.assertEqual(self.outputs(), {})

    def test_changed_chain_after_plan_refuses_before_provider(self):
        self.env['PLAN_LATEST_MANIFEST_SHA256'] = 'f' * 64
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('latest-changed-since-plan', self.output.getvalue())

    def refused_in_plan_and_production(self, code, *, mode='promote'):
        self.env['SHIP_MODE'] = mode
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.run_command('plan'), ship.EXIT_REFUSED, self.output.getvalue())
            self.assertEqual(self.production(mode), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertEqual(self.do.requests, [])
        self.assertEqual(self.outputs(), {})
        self.assertIn(code, self.output.getvalue())

    def test_missing_compatibility_refuses_both_actual_admission_points(self):
        self.fixture.world.runs.clear()
        self.refused_in_plan_and_production('candidate-invalid:compat-missing')

    def test_foreign_compatibility_refuses_both_actual_admission_points(self):
        self.fixture.rebind(foreign=True)
        self.refused_in_plan_and_production('candidate-invalid:compat-stale')

    def test_irreversible_migration_preserves_specific_refusal_before_provider(self):
        # Change only the measured outcome on the already exact source/context3
        # pair. The generic preflight fixture's rebind helper builds a fresh
        # synthetic request and does not carry this real pending binding.
        world = self.fixture.world
        def irreversible(row):
            row['old_migrate_ok']['rw'] = False
            row['reversible'] = False
        pair = fixtures.fixtures.edit_pair(world.pairs[100, 1], result=irreversible)
        identity = world.artifacts[100][0]['id']
        raw = fixtures.actions.signature.archive(pair)
        world.pairs[100, 1] = pair
        world.archives[identity] = raw, (100, 1)
        world.artifacts[100][0]['size_in_bytes'] = len(raw)
        self.refused_in_plan_and_production('candidate-invalid:irreversible-needs-fix-forward-mode')

    def test_dormant_source_with_matching_green_proof_cannot_bypass_staging(self):
        self.case.world.files[self.sha, fixtures.source.policy.SEEDS] += b'\n'
        world = self.fixture.world
        pair = fixtures.fixtures.edit_pair(world.pairs[100, 1],
            result=lambda row: row.update(classification={'kind': 'data', 'dormant': True}),
            context=lambda row: row.update(preview={'kind': 'data', 'dormant': True, 'blocked': False}))
        identity = world.artifacts[100][0]['id']
        raw = fixtures.actions.signature.archive(pair)
        world.pairs[100, 1] = pair
        world.archives[identity] = raw, (100, 1)
        world.artifacts[100][0]['size_in_bytes'] = len(raw)
        absent_consumer = self.case.root / 'absent-consumer.json'
        self.assertFalse(absent_consumer.exists())
        with mock.patch.object(flow.ship_outage_compat.loader, '_POLICY_PATH', absent_consumer):
            self.refused_in_plan_and_production('staging-bypass', mode='promote-without-staging')

    def image_only(self, *, restore):
        selected = (self.fixture.expected.resolved.latest if restore else
                    flow.record_resolver._selection(self.fixture.expected.resolved.chain, 2))
        if restore:
            self.do = support.FakeDO(spec=support.make_spec(self.images), backups=self.do.backups)
            self.deps.opener = self.do
        self.env.update(SHIP_MODE='rollback', SHIP_TARGET_RELEASE=selected.release,
            PLAN_TARGET_RELEASE=selected.release, PLAN_TARGET_MANIFEST_SHA256=selected.manifest_sha256)
        # Image-only policy does not invoke the normal Actions proof producer.
        self.fixture.world.capture = mock.Mock(side_effect=AssertionError('normal proof used for rollback'))
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.production('rollback'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)
        outputs = self.to_record()
        self.assertEqual(outputs['kind'], 'restore' if restore else 'rollback')
        self.assertFalse(any(key.startswith('handoff_') for key in outputs))
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        path = Path(self.env['RUNNER_TEMP']) / 'ship-record' / 'release-manifest.json'
        raw = path.read_bytes(); doc = c.loads_strict(raw)
        self.assertEqual(doc['sha'], selected.record_sha)
        self.assertEqual(doc['images'], dict(selected.images))
        self.assertEqual((doc['schema_version'], doc['db_change'], doc['compat'], doc['old_server_ok']),
                         (2, 'none', 'bypassed', 'unproven'))
        self.assertEqual(doc['catalog_sha256'], self.fixture.expected.resolved.latest.live.catalog.catalog_sha256)
        self.gh.attested_files[hashlib.sha256(raw).hexdigest()] = self.sha
        self.assertEqual(self.run_command('record', '--stage', 'publish'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.run_command('record', '--stage', 'verify'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)

    def test_mixed_chain_rollback_keeps_live_origin_through_actual_caller_and_writer(self):
        self.image_only(restore=False)

    def test_mixed_chain_restore_records_current_live_contract_without_normal_proof(self):
        self.image_only(restore=True)


class QualifiedRollbackCallerTests(unittest.TestCase):
    """Full source/verifier/custody/writer route, with only external I/O faked."""
    @classmethod
    def setUpClass(cls):
        rollback_fixture.RollbackPreflightTests.setUpClass()

    def setUp(self):
        self.fixture = rollback_fixture.RollbackPreflightTests()
        self.fixture.setUp(); self.addCleanup(self.fixture.doCleanups)
        self.case = self.fixture.case
        root = self.case.root
        self.now = rollback_fixture.actions.NOW
        self.sha = self.fixture.producer
        self.gh = self.case.world.fake
        self.env = support.context_env(self.sha, root)
        self.env['SHIP_GH_BIN'] = str(self.case.gh)
        selected = self.fixture.expected.resolved.selected
        latest = self.fixture.expected.resolved.latest
        self.env.update(SHIP_MODE='rollback', SHIP_TARGET_RELEASE=selected.release,
            PLAN_LATEST_RELEASE=latest.release, PLAN_LATEST_MANIFEST_SHA256=latest.manifest_sha256,
            PLAN_TARGET_RELEASE=selected.release, PLAN_TARGET_MANIFEST_SHA256=selected.manifest_sha256)
        self.target = root / 'target.json'; self.target.write_bytes(support.canonical(support.make_target()))
        self.policy = root / 'ship.trivyignore'; self.policy.write_bytes(b'# no exceptions\n')
        self.do = support.FakeDO(spec=support.make_spec(dict(latest.images)), backups=[{
            'created_at': c.format_timestamp(self.now - timedelta(hours=2)), 'size_gigabytes': 0.03}])
        self.https = support.FakeHttps()
        self.output = io.StringIO(); self.sleeps = []
        self.deps = ship.Deps(clock=lambda: self.now, sleeper=self.sleeps.append,
            opener=self.do, gh_runner=self.gh, https_request=self.https, stdout=self.output,
            repo_dir=self.case.repo, target_path=self.target, trivy_policy_path=self.policy,
            poll_limit=8, smoke_rounds=2, smoke_delay=0)

    outputs = CallerTests.outputs
    to_record = CallerTests.to_record

    def run_command(self, *args):
        for sha, raw in self.fixture.world.workflow_bytes.items():
            self.case.world.commits.add(sha)
            if raw is not None:
                self.case.world.files[sha, flow.ship_rollback_compat.evidence.WORKFLOW] = raw
        with ExitStack() as stack:
            stack.enter_context(mock.patch.object(flow.shutil, 'which', return_value=str(self.case.git)))
            clock_module = SimpleNamespace(monotonic=self.case.clock)
            for module in (ship, flow, flow.ship_compat, flow.ship_records, flow.ship_rollback_compat):
                stack.enter_context(mock.patch.object(module, 'time', clock_module))
            original = flow.ship_rollback_compat._preflight
            def preflight(*args, **kwargs):
                kwargs['wall_clock'] = lambda: self.now
                return original(*args, **kwargs)
            stack.enter_context(mock.patch.object(flow.ship_rollback_compat, '_preflight', side_effect=preflight))
            stack.enter_context(mock.patch.object(flow.ship_rollback_compat.runtime, 'bounded_command', self.fixture.capture))
            return ship.main(list(args), self.env, self.deps)

    def production(self):
        return CallerTests.production(self, 'rollback')

    def record_raw(self):
        return (Path(self.env['RUNNER_TEMP']) / 'ship-record' / 'release-manifest.json').read_bytes()

    def test_plan_production_and_retention_independent_record_use_complete_dedicated_proof(self):
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        previous_reads = self.fixture.world.workflow_reads
        self.now += timedelta(seconds=1)
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.fixture.world.workflow_reads, previous_reads + 2)
        self.assertEqual(self.do.put_count(), 1)
        self.assertEqual(len(self.https.calls), 6)
        outputs = self.to_record()
        self.assertEqual(outputs['handoff_version'], '7')
        original = self.fixture.expected
        handoff = flow.record_handoff.decode_rollback(
            {key: outputs[key] for key in flow.record_handoff.OUTPUT_KEYS}, producer_sha=self.sha,
            target_release=original.resolved.selected.release, workflow_sha=self.sha,
            run_id=int(self.env['GITHUB_RUN_ID']), record_attempt=1)
        self.assertEqual(handoff.preflight.resolution, original)
        self.assertEqual(handoff.preflight.checked_at, self.now)
        self.assertEqual(handoff.preflight.collected.history.attempts[0].measurement.context,
                         self.fixture.world.pairs[100, 1].context)
        # Record publication may happen after main moves and artifact retention
        # ends. Only preserved original custody and a fresh exact parent apply.
        self.case.world.main = 'f' * 40
        self.now += timedelta(days=400)
        self.fixture.world.capture = mock.Mock(side_effect=AssertionError('expired Actions'))
        self.env['GITHUB_RUN_ATTEMPT'] = '2'
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        raw = self.record_raw(); doc = c.loads_strict(raw)
        self.assertEqual((doc['schema_version'], doc['kind'], doc['sha'], doc['compat'], doc['old_server_ok']),
                         (2, 'rollback', original.resolved.selected.record_sha, 'ok', True))
        self.assertEqual(doc['compat_run_id'], 100)
        self.assertEqual(doc['images'], dict(original.resolved.selected.images))
        self.assertEqual(doc['previous'], original.resolved.latest.release)
        self.assertEqual(doc['rolled_back_from'], original.resolved.latest.release)
        self.assertEqual({key: doc[key] for key in original.requirements.live.catalog._fields},
                         original.requirements.live.catalog._asdict())
        self.assertIsNone(doc['fixed_forward_from'])
        self.assertNotEqual(doc['sha'], self.sha)
        self.gh.attested_files[hashlib.sha256(raw).hexdigest()] = self.sha
        self.assertEqual(self.run_command('record', '--stage', 'publish'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.run_command('record', '--stage', 'verify'), ship.EXIT_OK, self.output.getvalue())
        self.env['GITHUB_RUN_ATTEMPT'] = '3'
        for key in flow.record_handoff.OUTPUT_KEYS:
            self.env['REC_' + key.upper()] = ''
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.record_raw(), raw)
        self.assertEqual(self.outputs()['state'], 'published')
        self.fixture.world.capture.assert_not_called()
        self.assertEqual(self.do.put_count(), 1)

    def test_reconcile_success_carries_same_complete_custody_without_put(self):
        selected = self.fixture.expected.resolved.selected
        self.do = support.FakeDO(spec=support.make_spec(dict(selected.images)), backups=self.do.backups)
        self.deps.opener = self.do
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        outputs = self.to_record()
        self.assertEqual((outputs['reconciled'], outputs['handoff_version']), ('true', '7'))
        self.assertEqual(self.do.put_count(), 0)
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        doc = c.loads_strict(self.record_raw())
        self.assertTrue(doc['reconciled'])
        self.assertEqual((doc['compat'], doc['old_server_ok']), ('ok', True))

    def test_missing_or_actual_negative_refuses_before_any_provider_construction(self):
        for mode in ('missing', 'negative'):
            with self.subTest(mode=mode):
                if mode == 'missing': self.fixture.world.runs.clear()
                else: self.fixture.replace_history(server_false='read')
                with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
                    self.assertEqual(self.run_command('plan'), ship.EXIT_REFUSED, self.output.getvalue())
                    self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
                client.assert_not_called()
        self.assertEqual(self.do.requests, [])
        self.assertFalse(any(key.startswith('handoff_') for key in self.outputs()))

    def test_successful_plan_does_not_cover_new_negative_at_production(self):
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        self.fixture.replace_history(server_false='workers')
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('candidate-invalid:compat-old-server-incompatible', self.output.getvalue())

    def test_later_unknown_foreign_head_cannot_be_filtered_out(self):
        self.fixture.world.add(run=101, number=11, minute=30, sha='e' * 40,
                               missing=flow.compat_evidence.MissingEvidence.MISSING)
        self.fixture.world.workflow_bytes['e' * 40] = None
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()

    def test_custody_size_failure_occurs_before_provider_and_emits_no_handoff(self):
        with mock.patch.object(flow.record_handoff, 'MAX_ENCODED', 8192), \
                mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('record-invalid:handoff', self.output.getvalue())
        self.assertEqual(self.outputs(), {})

    def test_expiry_after_custody_preparation_refuses_before_provider(self):
        original = flow.record_handoff.encode_rollback
        def encode(value):
            encoded = original(value)
            self.now = value.preflight.artifact_expires_at
            return encoded
        with mock.patch.object(flow.record_handoff, 'encode_rollback', side_effect=encode), \
                mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('candidate-invalid:compat-stale', self.output.getvalue())

    def test_real_default_clock_retains_fractional_same_second_before_provider(self):
        self.now = self.now.replace(microsecond=900000)
        actual_dt = ship.dt
        clock_source = SimpleNamespace(datetime=SimpleNamespace(now=lambda timezone: self.now),
            timezone=actual_dt.timezone, timedelta=actual_dt.timedelta)
        with mock.patch.object(ship, 'dt', clock_source):
            self.deps.clock = ship.Deps().clock
            self.assertEqual(self.deps.clock(), self.now)
            self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
            outputs = self.to_record()
            original = flow.record_handoff.decode_rollback(
                {key: outputs[key] for key in flow.record_handoff.OUTPUT_KEYS}, producer_sha=self.sha,
                target_release=self.fixture.target['release'], workflow_sha=self.sha,
                run_id=int(self.env['GITHUB_RUN_ID']), record_attempt=1)
        self.assertEqual(original.preflight.checked_at, self.now)
        self.assertEqual(outputs['deployed_at'], c.format_timestamp(self.now))
        self.assertEqual(self.do.put_count(), 1)

    def test_backward_fractional_clock_after_custody_refuses_before_provider(self):
        self.now = self.now.replace(microsecond=900000)
        original = flow.record_handoff.encode_rollback
        def encode(value):
            encoded = original(value)
            self.now = value.preflight.checked_at - timedelta(microseconds=1)
            return encoded
        with mock.patch.object(flow.record_handoff, 'encode_rollback', side_effect=encode), \
                mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called()
        self.assertIn('candidate-invalid:compat-stale', self.output.getvalue())

    def test_one_microsecond_before_expiry_passes_exact_provider_boundary(self):
        original = flow.record_handoff.encode_rollback
        def encode(value):
            encoded = original(value)
            self.now = value.preflight.artifact_expires_at - timedelta(microseconds=1)
            self.do.backups = [{'created_at': c.format_timestamp(self.now - timedelta(hours=2)),
                                'size_gigabytes': 0.03}]
            return encoded
        with mock.patch.object(flow.record_handoff, 'encode_rollback', side_effect=encode):
            self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)
        self.assertEqual(len(self.https.calls), 6)
        self.assertEqual(self.outputs()['handoff_version'], '7')

    def test_changed_target_custody_or_parent_refuses_record_without_another_put(self):
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        outputs = self.to_record()
        original_target = self.env['SHIP_TARGET_RELEASE']
        self.env['SHIP_TARGET_RELEASE'] = self.fixture.expected.resolved.chain.entries[2].release
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_REFUSED, self.output.getvalue())
        self.env['SHIP_TARGET_RELEASE'] = original_target
        self.env['REC_HANDOFF_01'] = ''
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_REFUSED, self.output.getvalue())
        self.env['REC_HANDOFF_01'] = outputs['handoff_01']
        self.case.add(kind='restore')
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_REFUSED, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)


class OutageCallerTests(unittest.TestCase):
    """Concrete outage gate, writer and provider ordering with synthetic I/O."""
    outputs = CallerTests.outputs
    to_record = CallerTests.to_record

    def setUp(self):
        self.configure()

    def configure(self, *, unavailable=True):
        self.case = outage_fixture.outage_case(self, unavailable=unavailable)
        self.now, self.sha = self.case.now, self.case.sha
        self.images = dict(writer_fixture.images(self.case.expected).images)
        self.gh = self.case.world.fake
        self.gh.ci_green(self.sha); self.gh.attest_ship_images(self.images, self.sha)
        self.env = support.context_env(self.sha, self.case.root)
        latest = self.case.expected.resolved.latest
        self.env.update(SHIP_GH_BIN=str(self.case.gh), SHIP_MODE='promote-without-staging',
            SHIP_TARGET_RELEASE='', PLAN_LATEST_RELEASE=latest.release,
            PLAN_LATEST_MANIFEST_SHA256=latest.manifest_sha256,
            PLAN_TARGET_RELEASE='', PLAN_TARGET_MANIFEST_SHA256='')
        payload, digest = support.candidate_b64(self.images, self.sha, self.now-timedelta(hours=1))
        self.env.update(CANDIDATE_B64=payload, CANDIDATE_SHA256=digest)
        self.target = self.case.root/'target.json'
        self.target.write_bytes(support.canonical(support.make_target()))
        self.policy = self.case.root/'ship.trivyignore'; self.policy.write_bytes(b'# no exceptions\n')
        self.do = support.FakeDO(spec=support.make_spec(dict(latest.images)), backups=[{
            'created_at': c.format_timestamp(self.now-timedelta(hours=2)), 'size_gigabytes': 0.03}])
        self.https = support.FakeHttps(); self.output = io.StringIO(); self.sleeps = []
        self.deps = ship.Deps(clock=lambda: self.now, sleeper=self.sleeps.append, opener=self.do,
            gh_runner=self.gh, https_request=self.https, stdout=self.output,
            repo_dir=self.case.repo, target_path=self.target, trivy_policy_path=self.policy,
            poll_limit=8, smoke_rounds=2, smoke_delay=0)

    def run_command(self, *args):
        def capture(argv, **options):
            parts = Path(options['env'].get('GH_CONFIG_DIR', '')).parts
            if any(phase in parts for phase in ('source-before', 'source-after', 'actions', 'negative-index')):
                return self.case.capture(argv, **options)
            self.case.clock.now += 0.001
            return self.case.world(argv, **options)
        with ExitStack() as stack:
            stack.enter_context(self.case.installed())
            stack.enter_context(mock.patch.object(flow.shutil, 'which', return_value=str(self.case.git)))
            for module in (ship, flow, flow.ship_compat, flow.ship_records,
                           flow.ship_rollback_compat, flow.ship_outage_compat):
                stack.enter_context(mock.patch.object(module, 'time', SimpleNamespace(monotonic=self.case.clock)))
            for module in (flow.ship_compat, flow.ship_outage_compat):
                original = module._preflight
                def preflight(*args, _original=original, **kwargs):
                    kwargs['wall_clock'] = lambda: self.now
                    return _original(*args, **kwargs)
                stack.enter_context(mock.patch.object(module, '_preflight', side_effect=preflight))
            stack.enter_context(mock.patch.object(flow.ship_compat.runtime, 'bounded_command', capture))
            stack.enter_context(mock.patch.object(ship, 'first_parent_commits', return_value=(0, [])))
            return ship.main(list(args), self.env, self.deps)

    def production(self):
        self.env.update(SHIP_DO_TOKEN=support.DO_TOKEN, SHIP_TARGET_JSON=c.canonical_payload_bytes({
            'schema_version': 1, 'app_id': support.APP_ID, 'postgres_cluster_id': support.PG_ID}).decode())
        return self.run_command('production')

    def refused_before_provider(self):
        before_outputs = self.outputs()
        with mock.patch.object(ship.do_app, 'DOAppClient', side_effect=AssertionError('provider constructed')) as client:
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.output.getvalue())
        client.assert_not_called(); self.assertEqual(self.do.requests, []); self.assertEqual(self.outputs(), before_outputs)

    def negative_refs(self):
        negative = flow.ship_outage_compat.negative
        endpoint = negative.API+'/git/matching-refs/tags/'+negative.PREFIX+self.sha+'-'
        self.case.negative_world.overrides['GET', endpoint] = flow.ship_outage_compat.runtime.Capture(
            0, b'[{"ref":"refs/tags/synthetic-negative"}]\n', b'')

    def record_cycle(self):
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        plan_calls = len(self.case.calls); self.now += timedelta(seconds=1)
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertGreater(len(self.case.calls), plan_calls)
        self.assertEqual(self.do.put_count(), 1); self.assertEqual(len(self.https.calls), 6)
        outputs = self.to_record()
        self.assertEqual(outputs['handoff_version'], '8')
        custody = flow.record_handoff.decode_outage(
            {key: outputs[key] for key in flow.record_handoff.OUTPUT_KEYS}, candidate_sha=self.sha,
            workflow_sha=self.sha, run_id=int(self.env['GITHUB_RUN_ID']), record_attempt=1)
        self.assertEqual(custody.preflight.checked_at, self.now)
        self.assertNotIn('SHIP_DO_TOKEN', self.env)
        self.case.world.main = 'f'*40
        # Original raw custody is sufficient even after installed policy has gone.
        self.case.policy_path.unlink()
        self.now += timedelta(days=2)
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        path = Path(self.env['RUNNER_TEMP'])/'ship-record/release-manifest.json'
        raw = path.read_bytes(); doc = c.loads_strict(raw)
        self.assertEqual((doc['schema_version'], doc['db_change'], doc['dormant'], doc['reversible']),
                         (2, 'none', False, True))
        self.assertEqual((doc['compat'], doc['compat_run_id'], doc['old_server_ok'], doc['staging']),
                         ('bypassed', None, 'unproven', 'bypassed'))
        self.assertEqual(doc['catalog_sha256'], self.case.expected.resolved.latest.live.catalog.catalog_sha256)
        self.gh.attested_files[hashlib.sha256(raw).hexdigest()] = self.sha
        self.assertEqual(self.run_command('record', '--stage', 'publish'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.run_command('record', '--stage', 'verify'), ship.EXIT_OK, self.output.getvalue())
        self.env['GITHUB_RUN_ATTEMPT'] = '2'
        for key in flow.record_handoff.OUTPUT_KEYS: self.env['REC_'+key.upper()] = ''
        with mock.patch.object(flow.record_handoff, 'decode_outage', side_effect=AssertionError('old custody')):
            self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(path.read_bytes(), raw); self.assertEqual(self.outputs()['state'], 'published')
        self.assertEqual(self.do.put_count(), 1)

    def test_503_plan_production_and_record_keep_original_evidence_and_unproven_facts(self):
        self.record_cycle()
        self.assertFalse(any(phase == 'actions' for phase, _, _ in self.case.calls))

    def test_missing_plan_production_and_record_keep_full_history(self):
        self.configure(unavailable=False); self.record_cycle()
        self.assertTrue(any(phase == 'actions' for phase, _, _ in self.case.calls))

    def test_nonempty_protected_refs_refuse_before_provider(self):
        self.negative_refs()
        self.refused_before_provider()

    def test_failed_service_response_is_not_an_outage_waiver(self):
        self.case.negative_world.service = flow.ship_outage_compat.runtime.Capture(
            1, outage_fixture.index_fixture.http(401), b'PRIVATE')
        self.refused_before_provider()
        self.assertNotIn('PRIVATE', self.output.getvalue())

    def test_missing_coverage_refuses_before_provider(self):
        del self.case.world.files[self.sha, 'release/compat/negative_index.py']
        self.refused_before_provider()

    def test_current_normal_success_survives_uncovered_publisher_source(self):
        self.case.negative_world.service = flow.ship_outage_compat.runtime.Capture(
            0, outage_fixture.index_fixture.http(200), b'')
        del self.case.world.files[self.sha, 'release/compat/negative_index.py']
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1); self.assertEqual(self.outputs()['handoff_version'], '6')

    def test_successful_plan_cannot_cover_a_new_protected_negative_ref(self):
        self.assertEqual(self.run_command('plan'), ship.EXIT_OK, self.output.getvalue())
        self.negative_refs()
        self.refused_before_provider()

    def test_handoff_capacity_failure_occurs_before_provider(self):
        with mock.patch.object(flow.record_handoff, 'MAX_ENCODED', 8192):
            self.refused_before_provider()
        self.assertIn('record-invalid:handoff', self.output.getvalue())

    def mutate_after_encoding(self, change):
        original = flow.record_handoff.encode_outage
        def encode(value):
            outputs = original(value); change(value.preflight); return outputs
        return mock.patch.object(flow.record_handoff, 'encode_outage', side_effect=encode)

    def test_wall_clock_expiry_after_encoding_refuses_before_provider(self):
        with self.mutate_after_encoding(lambda proof: setattr(self, 'now', proof.valid_until)):
            self.refused_before_provider()
        self.assertIn('candidate-invalid:compat-stale', self.output.getvalue())

    def test_original_monotonic_deadline_cannot_be_renewed_after_encoding(self):
        with self.mutate_after_encoding(lambda proof: setattr(self.case.clock, 'now', 10000.0)):
            self.refused_before_provider()
        self.assertIn('candidate-invalid:compat-stale', self.output.getvalue())

    def test_backward_fractional_clock_after_encoding_refuses_before_provider(self):
        self.now = self.now.replace(microsecond=900000)
        with self.mutate_after_encoding(lambda proof: setattr(self, 'now', proof.checked_at-timedelta(microseconds=1))):
            self.refused_before_provider()

    def test_one_microsecond_before_expiry_passes_and_keeps_whole_second_completion(self):
        with self.mutate_after_encoding(lambda proof: setattr(self, 'now', proof.valid_until-timedelta(microseconds=1))):
            self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1); self.assertEqual(self.outputs()['handoff_version'], '8')
        self.assertEqual(self.outputs()['deployed_at'], c.format_timestamp(self.now))

    def test_fractional_default_clock_is_preserved_at_provider_boundary(self):
        self.now = self.now.replace(microsecond=900000)
        actual_dt = ship.dt
        with mock.patch.object(ship, 'dt', SimpleNamespace(datetime=SimpleNamespace(now=lambda timezone: self.now),
                timezone=actual_dt.timezone, timedelta=actual_dt.timedelta)):
            self.deps.clock = ship.Deps().clock
            self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        outputs = self.to_record()
        value = flow.record_handoff.decode_outage(
            {key: outputs[key] for key in flow.record_handoff.OUTPUT_KEYS}, candidate_sha=self.sha,
            workflow_sha=self.sha, run_id=int(self.env['GITHUB_RUN_ID']), record_attempt=1)
        self.assertEqual(value.preflight.checked_at, self.now)

    def test_tampered_custody_or_new_parent_cannot_write_record(self):
        self.assertEqual(self.production(), ship.EXIT_OK, self.output.getvalue())
        outputs = self.to_record(); self.env['REC_HANDOFF_01'] = ''
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_REFUSED, self.output.getvalue())
        self.env['REC_HANDOFF_01'] = outputs['handoff_01']
        self.case.source.add(kind='restore')
        self.assertEqual(self.run_command('record', '--stage', 'manifest'), ship.EXIT_REFUSED, self.output.getvalue())
        self.assertEqual(self.do.put_count(), 1)


if __name__ == '__main__':
    unittest.main()
