"""Real preflight composition with synthetic command edges, never live tools."""
from dataclasses import FrozenInstanceError
from pathlib import Path
import unittest
from unittest import mock

import ship_compat as ship
import test_compat_collect as actions
import test_compat_evidence as fixtures
import test_compat_request as bridge_tests
import test_record_resolver as source


class PreflightTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source.ForwardResolverTests.setUpClass()

    def setUp(self):
        self.scenario()

    def scenario(self, *, legacy=False, dormant=False):
        self.case = source.ForwardResolverTests()
        self.case.setUp()
        self.addCleanup(self.case.doCleanups)
        if legacy:
            self.old = self.case.add(version=1, sha=source.policy.V0_RELEASED_SOURCE)
            self.case.profile(self.old['sha'])
            for path in (source.policy.PROFILE, source.policy.OVERLAY):
                del self.case.world.files[self.old['sha'], path]
        else:
            self.old = self.case.add(version=1, images=self.case.boot['images'])
            self.origin = self.case.add()
            self.case.profile(self.origin['sha'])
            self.case.add(kind='rollback', target=self.old)
            self.case.world.files[self.case.boot['sha'], self.case.RECIPE] = b'FROM scratch\n'
        self.case.candidate(fixtures.SHA)
        if dormant:
            self.case.world.files[fixtures.SHA, source.policy.SEEDS] += b'\n'
        # Setup projection binds synthetic artifact bytes. The operation below
        # must reconstruct its own clients, resolutions and qualified receipt.
        self.expected = self.case.forward(fixtures.SHA)
        self.world = actions.World()
        self.world.add()
        bridge_tests.bind_pairs(self.world, self.expected)
        self.case.clock.now = 100.0
        self.calls = []
        self.phase = None
        self.after = lambda phase, argv: None
        self.scratch = self.case.root / 'preflight'
        self.deadline = 1000.0

    def capture(self, argv, **options):
        config = options['env'].get('GH_CONFIG_DIR', '')
        parts = Path(config).parts
        for name in ('source-before', 'actions', 'source-after'):
            if name in parts:
                self.phase = name
        self.calls.append((self.phase, list(argv), dict(options)))
        if self.phase == 'actions':
            raw = self.world.capture(argv, **options)
            result = ship.runtime.Capture(raw.returncode, raw.stdout, raw.stderr)
        else:
            result = self.case.world(argv, **options)
        self.case.clock.now += 0.001
        self.after(self.phase, argv)
        return result

    def run_preflight(self, *, wall_clock=lambda: actions.NOW, **changes):
        options = dict(gh_binary=self.case.gh, git_binary=self.case.git,
            token=source.support.GH_TOKEN, scratch_dir=self.scratch, deadline=self.deadline,
            capture=self.capture, clock=self.case.clock, wall_clock=wall_clock,
            base_env={'SHIP_DO_TOKEN':'PRIVATE-provider-token', 'GH_TOKEN':'PRIVATE-old-token',
                      'GIT_CONFIG_COUNT':'999', 'PATH':'/synthetic-system'})
        options.update(changes)
        return ship._preflight(self.case.repo, fixtures.SHA, **options)

    def refuse(self, code=None, **options):
        with self.assertRaises((ship.evidence.CompatRefusal, ship.common.ReleaseError)) as caught:
            self.run_preflight(**options)
        self.assertNotIn('PRIVATE', str(caught.exception))
        if code is not None:
            self.assertEqual(caught.exception.code, code)
        self.assertIsNone(source.records._GIT_OUTPUT_TRANSPORT.get())
        return caught.exception

    def rebind(self, *, server_false=None, migration_false=None, stale=False, foreign=False):
        latest, base = bridge_tests.identities(self.expected)
        pair = fixtures.pair(latest=latest, base=base,
            web_source=self.expected.previous_web_source_sha,
            server_false=server_false, migration_false=migration_false)
        pair = fixtures.edit_pair(pair, result=lambda row: row.update(catalog_sha256=source.policy.V0_MEASUREMENT))
        if stale:
            def change(row):
                row['latest']['manifest_sha256'] = 'f' * 64
                row['selected']['manifest_sha256'] = 'f' * 64
            pair = fixtures.edit_pair(pair, context=change)
        if foreign:
            pair = fixtures.edit_pair(pair, context=lambda row: row.update(sha='f' * 40))
        identity = self.world.artifacts[100][0]['id']
        raw = actions.signature.archive(pair)
        self.world.pairs[100, 1] = pair
        self.world.archives[identity] = raw, (100, 1)
        self.world.artifacts[100][0]['size_in_bytes'] = len(raw)

    def test_concrete_normal_operation_preserves_distinct_sources_and_original_budget(self):
        result = self.run_preflight()
        self.assertIs(type(result), ship.NormalPreflight)
        self.assertEqual(result.resolution, self.expected)
        self.assertEqual(result.qualified.candidate_sha, fixtures.SHA)
        self.assertEqual(len({result.qualified.previous.sha,
                             result.qualified.database_base.source_sha,
                             result.qualified.previous_web_source_sha}), 3)
        self.assertEqual(result.qualified.observation_id, result.collected.history.observation_id)
        self.assertEqual(result.command_count, len(self.calls))
        self.assertGreater(result.captured_bytes, result.collected.captured_bytes)
        versions = [(phase, opts) for phase, argv, opts in self.calls if argv[1:] == ['--version']]
        self.assertEqual([phase for phase, _ in versions], ['source-before', 'actions', 'source-after'])
        self.assertTrue(all(opts['timeout'] <= 30 for _, opts in versions))
        self.assertTrue(result.qualified.forward and result.qualified.reversible)
        with self.assertRaises(FrozenInstanceError): result.command_count = 0
        for _, argv, options in self.calls:
            self.assertIn(argv[0], (str(self.case.gh), str(self.case.git)))
            self.assertNotIn('SHIP_DO_TOKEN', options['env'])
            self.assertNotIn('PRIVATE-old-token', options['env'].values())
            self.assertGreater(options['timeout'], 0)
        self.assertNotIn('PRIVATE', repr(result))

    def test_current_e001_legacy_has_no_invented_live_contract(self):
        self.scenario(legacy=True)
        result = self.run_preflight()
        self.assertIsNone(result.resolution.resolved.latest.live)
        self.assertEqual(result.qualified.database_base, ship.evidence.DatabaseBase(
            'legacy-source-only', source.policy.V0_RELEASED_SOURCE, None, None))

    def test_real_dormant_data_source_qualifies_without_changing_policy(self):
        self.scenario(dormant=True)
        result = self.run_preflight()
        self.assertEqual(result.qualified.classification, ship.evidence.Classification('data', True))

    def test_current_main_mismatch_stops_before_actions(self):
        self.case.world.main = 'f' * 40
        self.assertEqual(self.refuse().detail, 'preflight-main-changed')
        self.assertEqual({p for p, _, _ in self.calls}, {'source-before'})

    def change_after_collection(self, change):
        reads = 0
        def hook(path, value):
            nonlocal reads
            if path.endswith('/actions/workflows/compat.yml'):
                reads += 1
                if reads == 2:
                    change()
            return value
        self.world.hook = hook

    def test_main_drift_after_collection_prevents_qualification(self):
        self.change_after_collection(lambda: setattr(self.case.world, 'main', 'f' * 40))
        self.assertEqual(self.refuse().detail, 'preflight-source-changed')
        self.assertIn('source-after', [p for p, _, _ in self.calls])

    def test_new_record_with_same_images_is_still_source_drift(self):
        self.change_after_collection(lambda: self.case.add(kind='restore'))
        self.assertEqual(self.refuse().detail, 'preflight-source-changed')

    def test_previous_recipe_changed_under_same_synthetic_sha_refuses_full_projection(self):
        self.change_after_collection(lambda: self.case.world.files.__setitem__(
            (self.case.boot['sha'], self.case.RECIPE), b'FROM scratch\n# changed\n'))
        self.assertEqual(self.refuse().detail, 'preflight-source-changed')

    def test_classifier_only_drift_is_not_hidden_by_same_catalog_or_images(self):
        def mutate():
            self.case.world.files[fixtures.SHA, source.policy.SEEDS] += b'\n'
        self.change_after_collection(mutate)
        self.assertEqual(self.refuse().detail, 'preflight-source-changed')

    def test_missing_history_preserves_policy_missing_refusal(self):
        self.world.runs.clear()
        self.refuse('compat-missing')
        self.assertIn('source-after', [p for p, _, _ in self.calls])

    def test_stale_authenticated_context_preserves_stale_refusal(self):
        self.rebind(stale=True)
        self.refuse('compat-stale')

    def test_foreign_context_cannot_become_a_receipt(self):
        self.rebind(foreign=True)
        self.refuse('compat-stale')

    def test_measured_old_server_false_keeps_specific_policy_code(self):
        self.rebind(server_false='reverse')
        self.refuse('compat-old-server-incompatible')

    def test_measured_migration_false_keeps_fix_forward_refusal(self):
        self.rebind(migration_false='rw')
        self.refuse('irreversible-needs-fix-forward-mode')

    def test_incomplete_inventory_cannot_gain_a_receipt(self):
        def hook(path, value):
            if path.endswith('/actions/workflows/compat.yml') and self.world.workflow_reads == 2:
                value['id'] += 1
            return value
        self.world.hook = hook
        self.refuse('compat-stale')

    def test_source_constructor_version_time_consumes_original_clock(self):
        self.deadline = 110
        self.after = lambda phase, argv: setattr(self.case.clock, 'now', 111)
        self.refuse()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0][1][1:], ['--version'])

    def test_actions_constructor_cannot_reset_original_deadline(self):
        self.deadline = 110
        def expire(phase, argv):
            if phase == 'actions' and argv[1:] == ['--version']:
                self.case.clock.now = 111
        self.after = expire
        self.refuse()
        last_phase, last_args, options = self.calls[-1]
        self.assertEqual((last_phase, last_args[1:]), ('actions', ['--version']))
        self.assertLess(options['timeout'], 10)
        self.assertNotIn('source-after', [p for p, _, _ in self.calls])

    def test_final_source_version_time_is_not_a_new_allowance(self):
        self.deadline = 110
        def expire(phase, argv):
            if phase == 'source-after' and argv[1:] == ['--version']:
                self.case.clock.now = 111
        self.after = expire
        self.refuse()
        self.assertEqual(self.calls[-1][0], 'source-after')
        self.assertEqual(self.calls[-1][1][1:], ['--version'])

    def test_exhaustion_at_collection_end_stops_before_final_source(self):
        calls = 0
        def wall():
            nonlocal calls
            calls += 1
            if calls == 4:
                self.case.clock.now = self.deadline
            return actions.NOW
        self.refuse(wall_clock=wall)
        self.assertNotIn('source-after', [p for p, _, _ in self.calls])

    def test_expiration_during_final_policy_cannot_return_a_projection(self):
        qualify = ship.bridge.qualify
        def expire(*args):
            result = qualify(*args)
            self.case.clock.now = self.deadline
            return result
        with mock.patch.object(ship.bridge, 'qualify', side_effect=expire):
            self.assertEqual(self.refuse().detail, 'preflight-deadline')

    def test_scratch_replay_refuses_before_any_new_command(self):
        self.run_preflight()
        count = len(self.calls)
        self.assertEqual(self.refuse().detail, 'preflight-scratch')
        self.assertEqual(len(self.calls), count)

    def test_private_capture_errors_never_become_public_text(self):
        def fail(*args, **kwargs):
            raise ValueError('PRIVATE credential-bearing driver text')
        self.refuse(capture=fail)
        self.assertEqual(self.calls, [])

    def test_nonzero_private_streams_never_become_public_text(self):
        def fail(*args, **kwargs):
            return ship.runtime.Capture(1, b'PRIVATE stdout', b'PRIVATE credential-bearing stderr')
        self.refuse(capture=fail)
        self.assertEqual(self.calls, [])

    def test_binary_links_and_nonabsolute_paths_refuse_before_capture(self):
        original = Path.is_symlink
        with mock.patch.object(Path, 'is_symlink', lambda p: p == self.case.gh or original(p)):
            self.assertEqual(self.refuse().detail, 'preflight-path')
        self.assertEqual(self.refuse(gh_binary=Path('gh')).detail, 'preflight-path')
        self.assertEqual(self.calls, [])

    def test_replaced_scratch_is_not_silently_used(self):
        def replace_root(phase, argv):
            self.scratch.rename(self.case.root / 'retained-original')
            self.scratch.mkdir()
        self.after = replace_root
        self.refuse()
        self.assertEqual(len(self.calls), 1)

    def test_public_api_has_no_evidence_or_client_override(self):
        options = dict(gh_binary=self.case.gh, git_binary=self.case.git,
                       token=source.support.GH_TOKEN, scratch_dir=self.scratch, deadline=self.deadline)
        for key in ('client', 'resolution', 'collected', 'qualified', 'capture', 'clock', 'wall_clock'):
            with self.subTest(key=key), self.assertRaises(TypeError):
                ship.preflight(self.case.repo, fixtures.SHA, **options, **{key:object()})
        self.assertFalse(self.scratch.exists())


class AggregateBudgetTests(unittest.TestCase):
    def setUp(self):
        self.clock = source.transport_tests.Clock()
        self.calls = []

    def budget(self, deadline=1000, capture=None):
        return ship._Budget(deadline, capture=capture or self.capture, clock=self.clock)

    def capture(self, argv, **options):
        self.calls.append((argv, options))
        return ship.runtime.Capture(0, b'a' * options['stdout_limit'], b'b' * options['stderr_limit'])

    def bound(self, budget):
        # Transport grammar is covered by real adapters above. These unit
        # cases isolate cumulative allowances without allocating large streams.
        budget._paths = ((Path('/root'), True, None), (Path('/gh'), False, None), (Path('/git'), False, None))
        return mock.patch.object(budget, 'check', side_effect=budget.remaining)

    def test_absolute_maximum_and_backwards_or_nonfinite_clock_refuse(self):
        budget = self.budget(99999)
        self.assertEqual(budget.end, 1000)
        for value in (99, float('nan'), float('inf'), True):
            budget = self.budget()
            self.clock.now = value
            with self.assertRaises(ship.evidence.CompatRefusal): budget.remaining()
            self.clock.now = 100
        for end in (100, True, float('nan'), float('inf')):
            with self.assertRaises(ship.evidence.CompatRefusal): self.budget(end)

    def test_summed_stream_allowance_and_cleanup_tail_are_shared(self):
        budget = self.budget(110)
        budget.captured_bytes = ship.MAX_BYTES - 100
        with self.bound(budget):
            result = budget.capture([str(Path('/gh')), '--version'], env={}, timeout=60, stdout_limit=90, stderr_limit=20)
            self.assertEqual((len(result.stdout), len(result.stderr)), (80, 20))
            self.assertEqual(self.calls[0][1]['timeout'], 10 - ship.CLEANUP_RESERVE)
            self.assertEqual(budget.captured_bytes, ship.MAX_BYTES)
            with self.assertRaises(ship.evidence.CompatRefusal):
                budget.capture([str(Path('/git')), 'fixed-read'], env={}, timeout=60, stdout_limit=1, stderr_limit=1)
        self.assertEqual(len(self.calls), 1)

    def test_command_cap_persists_across_transports(self):
        budget = self.budget()
        budget.commands = ship.MAX_COMMANDS - 1
        with self.bound(budget):
            budget.capture([str(Path('/gh')), '--version'], env={}, timeout=1, stdout_limit=1, stderr_limit=1)
            with self.assertRaises(ship.evidence.CompatRefusal):
                budget.capture([str(Path('/git')), 'fixed-read'], env={}, timeout=1, stdout_limit=1, stderr_limit=1)
        self.assertEqual(len(self.calls), 1)

    def test_overrun_malformed_capture_and_concurrency_poison_budget(self):
        for capture in (lambda *a, **k: ship.runtime.Capture(0, b'oversized', b''),
                        lambda *a, **k: object()):
            budget = self.budget(capture=capture)
            with self.bound(budget), self.assertRaises(ship.evidence.CompatRefusal):
                budget.capture([str(Path('/gh')), '--version'], env={}, timeout=1, stdout_limit=1, stderr_limit=1)
            self.assertTrue(budget.failed)
        budget = self.budget()
        budget._lock.acquire()
        try:
            with self.bound(budget), self.assertRaises(ship.evidence.CompatRefusal):
                budget.capture([str(Path('/gh')), '--version'], env={}, timeout=1, stdout_limit=1, stderr_limit=1)
        finally:
            budget._lock.release()
        self.assertTrue(budget.failed)


if __name__ == '__main__':
    unittest.main()
