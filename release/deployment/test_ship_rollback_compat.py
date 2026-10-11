"""Real source/Actions/verifier/bridge composition at synthetic command edges.

No Git, GH, provider or service process is launched. Test setup derives artifact
bindings with the actual resolver and private producer; the preflight must then
independently reconstruct everything through its own three concrete phases.
"""
from dataclasses import FrozenInstanceError
from datetime import timedelta
import copy
from pathlib import Path
import unittest
from unittest import mock

import ship_rollback_compat as ship
import test_compat_collect as actions
import test_compat_rollback_collect as collector
import test_compat_rollback_request as requests
import test_record_resolver_rollback as source


class RollbackPreflightTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source.RollbackProofResolverTests.setUpClass()

    def setUp(self):
        self.scenario()

    def scenario(self, *, change_count=2, irreversible=False, prepare=True):
        self.case = source.RollbackProofResolverTests()
        self.case.setUp()
        self.addCleanup(self.case.doCleanups)
        self.target = self.case.add(version=1, sha='1' * 40, images=self.case.boot['images'])
        self.live_image = self.case.add(version=1, sha='4' * 40)
        self.case.add(sha='2' * 40, db_change='data' if change_count >= 1 else 'none',
                      dormant=change_count >= 1, reversible=not irreversible)
        self.origin = self.case.add(sha='3' * 40,
            db_change='expand' if change_count >= 2 else 'none', dormant=change_count >= 2)
        self.case.add(kind='rollback', target=self.live_image)
        self.producer = self.case.world.main
        for sha in (self.case.boot['sha'], self.live_image['sha'], self.producer):
            self.case.profile(sha)
        self.case.world.files[self.case.boot['sha'], self.case.RECIPE] += b'# target recipe\n'
        self.case.world.files[self.live_image['sha'], self.case.RECIPE] += b'# live recipe\n'
        self.expected = (self.case.proof(self.producer, self.target['release']) if prepare else None)
        binding = requests.producer_binding(self.expected) if prepare else None
        self.world = collector.World(binding)
        self.world.add()
        self.case.clock.now = 100.0
        self.calls, self.phase = [], None
        self.after = lambda phase, argv: None
        self.scratch = self.case.root / 'preflight'
        self.deadline = 1000.0

    def capture(self, argv, **options):
        argv = list(argv)
        parts = Path(options['env'].get('GH_CONFIG_DIR', '')).parts
        for phase in ('source-before', 'actions', 'source-after'):
            if phase in parts:
                self.phase = phase
        self.calls.append((self.phase, argv, dict(options)))
        if self.phase == 'actions' and argv[0] == str(self.case.gh):
            raw = self.world.capture(argv, **options)
            result = ship.runtime.Capture(raw.returncode, raw.stdout, raw.stderr)
        else:
            # Includes the collector's actual ImmutableGit reads of all heads.
            result = self.case.world(argv, **options)
        self.case.clock.now += 0.001
        self.after(self.phase, argv)
        return result

    def run_preflight(self, *, wall_clock=lambda: actions.NOW, **changes):
        for sha, raw in self.world.workflow_bytes.items():
            self.case.world.commits.add(sha)
            if raw is not None:
                self.case.world.files[sha, ship.evidence.WORKFLOW] = raw
            else:
                self.case.world.files.pop((sha, ship.evidence.WORKFLOW), None)
        options = dict(gh_binary=self.case.gh, git_binary=self.case.git,
            token=source.fixtures.support.GH_TOKEN, scratch_dir=self.scratch,
            deadline=self.deadline, capture=self.capture, clock=self.case.clock,
            wall_clock=wall_clock, base_env={'SHIP_DO_TOKEN': 'PRIVATE-provider',
                'GH_TOKEN': 'PRIVATE-stale', 'GIT_CONFIG_COUNT': '999'})
        options.update(changes)
        return ship._preflight(self.case.repo, self.producer, self.target['release'], **options)

    def refuse(self, *, code=None, **changes):
        with self.assertRaises((ship.evidence.CompatRefusal, ship.common.ReleaseError,
                               ship.resolver.policy.PolicyRefusal)) as caught:
            self.run_preflight(**changes)
        self.assertNotIn('PRIVATE', str(caught.exception))
        if code is not None:
            self.assertEqual(caught.exception.code, code)
        self.assertIsNone(source.records._GIT_OUTPUT_TRANSPORT.get())
        return caught.exception

    def replace_history(self, **changes):
        self.world = collector.World(self.world.binding)
        self.world.add(**changes)

    def change_after_collection(self, change):
        def hook(path, value):
            if path.endswith('/actions/workflows/compat.yml') and self.world.workflow_reads == 2:
                change()
            return value
        self.world.hook = hook

    def test_actual_three_phase_composition_retains_distinct_sources_and_public_hold(self):
        result = self.run_preflight()
        self.assertIs(type(result), ship.RollbackPreflight)
        self.assertEqual(result.resolution, self.expected)
        self.assertEqual(result.qualified.binding, self.world.binding)
        self.assertEqual(result.qualified.observation_id, result.collected.history.observation_id)
        self.assertEqual(result.command_count, len(self.calls))
        self.assertGreater(result.captured_bytes, result.collected.captured_bytes)
        self.assertEqual(result.checked_at, actions.NOW)
        self.assertGreater(result.artifact_expires_at, result.checked_at)
        binding = result.qualified.binding
        self.assertEqual(len({binding.producer_sha, binding.database_base.source_sha,
            binding.live_web_source_sha, binding.selected_web_source_sha}), 4)
        self.assertEqual(binding.span.non_none, 2)
        self.assertEqual(binding.span.records, 5)
        self.assertEqual(result.collected.history.whole_run_deletion_coverage, 'unavailable')
        versions = [phase for phase, argv, _ in self.calls if argv[1:] == ['--version']]
        self.assertEqual(versions, ['source-before', 'actions', 'source-after'])
        verifies = [args for phase, args, _ in self.calls
                    if phase == 'actions' and args[1:3] == ['attestation', 'verify']]
        self.assertEqual(len(verifies), 2)
        self.assertTrue(all('--cert-oidc-issuer' in args and '--deny-self-hosted-runners' in args
                            for args in verifies))
        self.assertEqual(self.world.workflow_reads, 2)
        self.assertTrue(any(phase == 'actions' and argv[0] == str(self.case.git)
                            for phase, argv, _ in self.calls))
        self.assertFalse(any('graphql' in argv for _, argv, _ in self.calls))
        self.assertFalse(any((self.scratch / 'actions' / 'work').iterdir()))
        with self.assertRaisesRegex(ship.resolver.policy.PolicyRefusal, 'rollback-qualification-unavailable'):
            ship.resolver.policy.rollback_plan(result.resolution.resolved.chain, self.target['release'])
        with self.assertRaises(FrozenInstanceError):
            result.command_count = 0
        self.assertNotIn('PRIVATE', repr(result))
        for _, argv, options in self.calls:
            self.assertIn(argv[0], (str(self.case.gh), str(self.case.git)))
            self.assertNotIn('SHIP_DO_TOKEN', options['env'])
            self.assertNotIn('PRIVATE-stale', options['env'].values())

    def test_three_distinct_closed_phase_budgets_share_one_outer_capture(self):
        operations, outers = [], []
        original = ship.transport.OperationBudget.__init__
        original_outer = ship.normal._Budget.__init__
        def operation(obj, *args, **kwargs):
            original(obj, *args, **kwargs)
            operations.append(obj)
        def outer(obj, *args, **kwargs):
            original_outer(obj, *args, **kwargs)
            outers.append(obj)
        with mock.patch.object(ship.transport.OperationBudget, '__init__', operation), \
                mock.patch.object(ship.normal._Budget, '__init__', outer):
            result = self.run_preflight()
        self.assertEqual(len(operations), 3)
        self.assertEqual(len(outers), 1)
        self.assertEqual(len({id(o) for o in operations}), 3)
        self.assertTrue(all(o.closed and not o.failed for o in operations))
        self.assertTrue(outers[0].closed and not outers[0].failed)
        self.assertTrue(all(o._capture.__self__ is outers[0] for o in operations))
        self.assertTrue(all(o._deadline <= outers[0].end for o in operations))
        self.assertEqual(sum(o.commands for o in operations), result.command_count)
        self.assertEqual(sum(o.captured_bytes for o in operations), result.captured_bytes)

    def test_new_latest_same_images_still_invalidates_entire_source(self):
        self.change_after_collection(lambda: self.case.add(kind='restore'))
        self.assertEqual(self.refuse().detail, 'rollback-preflight-source-changed')

    def test_recipe_and_workflow_source_drift_are_not_hidden_by_signed_image_equality(self):
        for mode in ('target', 'live', 'workflow'):
            with self.subTest(mode=mode):
                self.scenario()
                def change():
                    sha, path = ((self.producer, ship.evidence.WORKFLOW) if mode == 'workflow' else
                        (self.case.boot['sha'] if mode == 'target' else self.live_image['sha'], self.case.RECIPE))
                    self.case.world.files[sha, path] += b'# changed immutable source\n'
                self.change_after_collection(change)
                self.assertEqual(self.refuse().detail, 'rollback-preflight-source-changed')

    def test_main_or_origin_material_change_refuses_after_collection(self):
        for mode in ('main', 'origin'):
            with self.subTest(mode=mode):
                self.scenario()
                def change():
                    if mode == 'main':
                        self.case.world.main = 'f' * 40
                    else:
                        key = self.origin['sha'], ship.resolver.policy.SEEDS
                        self.case.world.files[key] += b'\n'
                self.change_after_collection(change)
                self.refuse()
                self.assertIn('source-after', [phase for phase, _, _ in self.calls])

    def test_source_policy_refusals_stop_before_any_actions(self):
        for mode in ('zero', 'one', 'irreversible', 'material', 'main'):
            with self.subTest(mode=mode):
                self.scenario(change_count=0 if mode == 'zero' else 1 if mode == 'one' else 2,
                              irreversible=mode == 'irreversible', prepare=False)
                if mode == 'material':
                    self.case.world.files[self.origin['sha'], ship.resolver.policy.SEEDS] += b'\n'
                elif mode == 'main':
                    self.case.world.main = 'f' * 40
                exc = self.refuse()
                self.assertNotIn('actions', [phase for phase, _, _ in self.calls])
                if mode in ('zero', 'one'):
                    self.assertEqual(str(exc), 'rollback-proof-not-required')
                elif mode == 'irreversible':
                    self.assertEqual(str(exc), 'rollback-irreversible-span')

    def test_exact_target_tag_never_substitutes_same_image_latest(self):
        self.target = self.live_image
        self.refuse(code='compat-stale')
        self.assertIn('source-after', [phase for phase, _, _ in self.calls])

    def test_authenticated_semantic_negatives_keep_specific_refusals(self):
        for change, code in (({'server_false': 'workers'}, 'compat-old-server-incompatible'),
                ({'migration_false': 'rw'}, 'irreversible-needs-fix-forward-mode'),
                ({'catalog_sha256': 'f' * 64}, 'compat-stale')):
            with self.subTest(change=change):
                self.scenario(); self.replace_history(**change)
                self.refuse(code=code)

    def test_missing_and_expired_artifact_do_not_become_a_receipt(self):
        for mode in ('empty', 'expired', 'missing'):
            with self.subTest(mode=mode):
                self.scenario()
                if mode == 'empty': self.world.runs.clear()
                elif mode == 'expired': self.world.artifacts[100][0]['expired'] = True
                else: self.world.artifacts[100] = []
                self.refuse(code='compat-missing' if mode == 'empty' else 'compat-stale')

    def test_later_or_unknown_foreign_head_is_not_filtered_out(self):
        for mode in ('later', 'unknown'):
            with self.subTest(mode=mode):
                self.scenario()
                self.world.add(sha='f' * 40, run=200, number=20, minute=60 if mode == 'later' else 1,
                               server_false='workers')
                if mode == 'unknown': self.world.workflow_bytes['f' * 40] = b'unknown workflow\n'
                self.refuse(code='compat-stale')

    def test_older_known_negative_or_unverified_can_be_superseded_without_erasing_it(self):
        for mode in ('negative', 'unverified'):
            with self.subTest(mode=mode):
                self.scenario()
                self.world = collector.World(self.world.binding)
                self.world.add(run=100, number=10, minute=1, server_false='workers')
                self.world.add(run=200, number=20, minute=60)
                if mode == 'unverified':
                    self.world.archives[10001] = (b'not a zip', (100, 1))
                    self.world.artifacts[100][0]['size_in_bytes'] = len(b'not a zip')
                result = self.run_preflight()
                self.assertEqual(result.qualified.run_id, 200)
                self.assertEqual(len(result.collected.history.attempts), 2)
                if mode == 'unverified': self.assertIn('artifact-unverified', result.collected.issues)

    def test_second_inventory_drift_refuses_complete_qualification(self):
        def hook(path, value):
            if path.endswith('/actions/workflows/compat.yml') and self.world.workflow_reads == 2:
                value['id'] += 1
            return value
        self.world.hook = hook
        self.refuse(code='compat-stale')

    def test_foreign_certificate_attempt_and_reused_job_times_do_not_qualify(self):
        self.world.sign_mutate = lambda value, path: value[0]['verificationResult']['signature']['certificate'].update(
            runInvocationURI=f'https://github.com/{ship.evidence.REPOSITORY}/actions/runs/100/attempts/2')
        self.refuse(code='compat-stale')
        self.scenario()
        self.world = collector.World(self.world.binding)
        self.world.add(attempt=1, latest=2, minute=1)
        self.world.add(attempt=2, latest=2, minute=1)
        old = self.world.jobs[100, 1][0]
        self.world.set_job(100, 2, old['name'], started_at=old['started_at'],
            completed_at=old['completed_at'], steps=copy.deepcopy(old['steps']))
        self.refuse(code='compat-stale')

    def test_original_deadline_covers_every_phase_constructor(self):
        for selected in ('source-before', 'actions', 'source-after'):
            with self.subTest(phase=selected):
                self.scenario(); self.deadline = 110.0
                def expire(phase, argv):
                    if phase == selected and argv[1:] == ['--version']:
                        self.case.clock.now = 111.0
                self.after = expire
                self.refuse()
                self.assertEqual(self.calls[-1][0], selected)
                self.assertEqual(self.calls[-1][1][1:], ['--version'])
                self.assertLess(self.calls[-1][2]['timeout'], 10)

    def test_phase_change_cannot_renew_summed_command_or_byte_allowance(self):
        for mode in ('commands', 'bytes'):
            with self.subTest(mode=mode):
                self.scenario()
                outer = []
                original = ship.normal._Budget.__init__
                def remember(obj, *args, **kwargs):
                    original(obj, *args, **kwargs); outer.append(obj)
                def spent(phase, argv):
                    if phase == 'actions' and argv[1:] == ['--version']:
                        if mode == 'commands': outer[0].commands = ship.normal.MAX_COMMANDS
                        else: outer[0].captured_bytes = ship.normal.MAX_BYTES
                self.after = spent
                with mock.patch.object(ship.normal._Budget, '__init__', remember):
                    self.refuse()
                self.assertTrue(outer[0].failed and outer[0].closed)
                self.assertNotIn('source-after', [phase for phase, _, _ in self.calls])

    def test_artifact_expiring_during_final_source_read_refuses_without_new_observation(self):
        self.world.artifacts[100][0]['expires_at'] = actions.stamp(actions.NOW + timedelta(seconds=1))
        def clock():
            return actions.NOW + (timedelta(seconds=2) if self.phase == 'source-after' else timedelta())
        self.assertEqual(self.refuse(wall_clock=clock).detail, 'rollback-preflight-artifact-expired')
        self.assertEqual(self.world.workflow_reads, 2)

    def test_final_time_retains_original_history_instead_of_relabeling_it(self):
        def clock():
            return actions.NOW + (timedelta(seconds=2) if self.phase == 'source-after' else timedelta())
        result = self.run_preflight(wall_clock=clock)
        self.assertEqual(result.collected.history.observed_at, actions.NOW)
        self.assertEqual(result.checked_at, actions.NOW + timedelta(seconds=2))
        self.assertEqual(result.qualified.observation_id, result.collected.history.observation_id)

    def test_full_entry_collection_final_clock_chain_is_required(self):
        for mode in ('entry-after-collection', 'final-before-collection', 'naive', 'folded'):
            with self.subTest(mode=mode):
                self.scenario()
                def clock():
                    if mode == 'entry-after-collection' and self.phase is None:
                        return actions.NOW + timedelta(seconds=1)
                    if self.phase == 'source-after':
                        if mode == 'naive': return actions.NOW.replace(tzinfo=None)
                        if mode == 'folded': return actions.NOW.replace(fold=1)
                        return actions.NOW + timedelta(seconds=2 if mode == 'entry-after-collection' else -1)
                    return actions.NOW
                self.assertEqual(self.refuse(wall_clock=clock).detail, 'rollback-preflight-clock')

    def test_clock_exhaustion_during_final_check_cannot_return_a_result(self):
        def clock():
            if self.phase == 'source-after': self.case.clock.now = self.deadline
            return actions.NOW
        self.refuse(wall_clock=clock)

    def test_existing_or_replaced_scratch_is_refused(self):
        self.run_preflight()
        calls = len(self.calls)
        self.refuse()
        self.assertEqual(len(self.calls), calls)
        self.scenario()
        def change(phase, argv):
            self.scratch.rename(self.case.root / 'retained-original')
            self.scratch.mkdir()
        self.after = change
        self.refuse()
        self.assertEqual(len(self.calls), 1)

    def test_links_relative_binaries_and_private_capture_failure_are_safe(self):
        original = Path.is_symlink
        with mock.patch.object(Path, 'is_symlink', lambda p: p == self.case.gh or original(p)):
            self.refuse()
        self.refuse(gh_binary=Path('gh'))
        self.assertEqual(self.calls, [])
        def fail(*args, **kwargs): raise ValueError('PRIVATE driver credentials')
        self.refuse(capture=fail)
        self.assertEqual(self.calls, [])

    def test_public_boundary_has_no_receipt_or_callback_inputs(self):
        options = dict(gh_binary=self.case.gh, git_binary=self.case.git,
            token=source.fixtures.support.GH_TOKEN, scratch_dir=self.scratch, deadline=self.deadline)
        for name in ('client', 'resolution', 'collected', 'qualified', 'capture', 'clock', 'wall_clock'):
            with self.subTest(name=name), self.assertRaises(TypeError):
                ship.preflight(self.case.repo, self.producer, self.target['release'],
                               **options, **{name: object()})
        self.assertEqual(self.calls, [])


if __name__ == '__main__':
    unittest.main()
