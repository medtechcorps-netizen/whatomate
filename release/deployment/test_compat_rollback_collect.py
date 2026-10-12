"""Actual bounded Git/GH classes with synthetic command responses only.

The fixture never launches Git, GH, Docker or a service. Real fixed verifiers,
raw-envelope replay, source grammar and all-head qualification remain in use.
"""
from contextlib import contextmanager
import copy
from dataclasses import replace
from datetime import timedelta
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import compat_rollback_collect as collect
import compat_rollback_history as history
import compat_envelope as envelope
import compat_evidence as evidence
import record_transport as transport
import test_compat_collect as actions
import test_compat_rollback_history as histories
import test_compat_rollback as rollback
import test_compat_adapter as signature
import test_record_resolver as source_fixtures
import test_compat_rollback_request as request_fixtures
import test_ship_support as support

NOW = actions.NOW
WORKFLOW = actions.WORKFLOW


class World(actions.World):
    def __init__(self, binding=None):
        super().__init__()
        self.binding = rollback.binding() if binding is None else binding
        self.workflow_bytes = {self.binding.producer_sha: WORKFLOW}
        self.trace = []
        self.before = lambda argv: None
        self.git_hook = lambda args, result: result

    def add_row(self, row):
        value = json.loads(row.envelope.raw)
        run = dict(value['run'])
        run.pop('latest_run_attempt')
        key = run['id'], run['run_attempt']
        self.attempts[key] = run
        self.jobs[key] = value['jobs']
        self.checks[run['check_suite_id']] = value['checks']
        current = self.runs.get(run['id'])
        if current is None or current['run_attempt'] < run['run_attempt']:
            self.runs[run['id']] = run
        self.artifacts.setdefault(run['id'], [])
        self.workflow_bytes.setdefault(run['head_sha'], WORKFLOW)
        if type(row.measurement) is evidence.AuthenticatedPair:
            meta = next(item for item in value['artifacts']
                        if item['name'] == f"compat-result-{run['head_sha']}-{run['run_attempt']}")
            raw = signature.archive(row.measurement)
            meta['size_in_bytes'] = len(raw)
            self.artifacts[run['id']].append(meta)
            self.archives[meta['id']] = raw, key
            self.pairs[key] = row.measurement
            attempt = envelope.replay(envelope.WorkflowObservation(run['head_sha'], WORKFLOW),
                row.envelope, observed_at=NOW).attempt
            self.signers[key] = signature.SyntheticGh(attempt)
        return row

    def add(self, **kwargs):
        return self.add_row(histories.row(self.binding, **kwargs))

    def capture_both(self, argv, **options):
        argv = list(argv)
        self.trace.append((argv, dict(options)))
        self.before(argv)
        if argv[0] == str(self.git_binary):
            self.clock.value += 0.001
            args = argv[argv.index('advice.graftFileDeprecated=false') + 1:]
            result = self.git_hook(args, self.source.git(args))
        else:
            completed = super().capture(argv, **options)
            result = transport.runtime.Capture(completed.returncode, completed.stdout, completed.stderr)
        return result


@contextmanager
def harness(world=None, *, seconds=300):
    world = World() if world is None else world
    if not world.runs:
        world.add()
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary).resolve()
        repo, config, scratch = root/'repo', root/'config', root/'scratch'
        (repo/'.git').mkdir(parents=True)
        config.mkdir(); scratch.mkdir()
        gh, git = root/'gh.exe', root/'git.exe'
        gh.write_bytes(b'nonexecutable fixture'); git.write_bytes(b'nonexecutable fixture')
        world.git_binary = git
        world.source = source_fixtures.ControlledWorld(repo, git, world.clock)
        world.source.commits.update(world.workflow_bytes)
        world.source.files.update({(sha, evidence.WORKFLOW): raw
            for sha, raw in world.workflow_bytes.items() if raw is not None})
        budget = transport.OperationBudget(world.clock.value + seconds,
            capture=world.capture_both, clock=world.clock)
        options = dict(gh_binary=gh, git_binary=git, token=support.GH_TOKEN,
            config_dir=config, scratch=scratch, budget=budget, wall_clock=lambda: NOW, base_env={})
        yield world, repo, options
        assert not any(scratch.iterdir()), 'Verifier-owned directories leaked'


def execute(world, repo, options):
    return collect._collect(world.binding.producer_sha, repo, **options)


def qualify(world, result):
    request = history.RollbackRequest(world.binding, WORKFLOW, result.history.observation_id)
    return history.require_rollback_compat(request, result.history)


class RollbackCollectorTests(unittest.TestCase):
    def refusal(self, world, repo, options):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            execute(world, repo, options)
        self.assertEqual(caught.exception.code, 'compat-stale')
        self.assertNotIn('PRIVATE', str(caught.exception))
        self.assertTrue(options['budget'].closed)
        self.assertTrue(options['budget'].failed)
        return caught.exception

    def test_actual_source_transport_verifier_and_qualifier_without_supplied_facts(self):
        with harness() as (world, repo, options):
            result = execute(world, repo, options)
            proof = qualify(world, result)
            self.assertEqual(proof.binding, world.binding)
            self.assertEqual(result.history.enumeration, 'complete-visible')
            self.assertEqual(result.history.whole_run_deletion_coverage, 'unavailable')
            self.assertEqual(result.command_count, len(world.trace))
            self.assertEqual(result.command_count, options['budget'].commands)
            self.assertEqual(result.captured_bytes, options['budget'].captured_bytes)
            self.assertTrue(options['budget'].closed)
            self.assertFalse(options['budget'].failed)
            verifies = [args for args, _ in world.trace if args[1:3] == ['attestation','verify']]
            self.assertEqual(len(verifies), 2)
            self.assertEqual(world.workflow_reads, 2)
            queries = [args[4] for args, _ in world.trace if args[1:4] == ['api','--method','GET']]
            self.assertTrue(any('/runs?branch=main&event=push&' in path for path in queries))
            self.assertTrue(any('/runs?branch=main&event=workflow_dispatch&' in path for path in queries))
            self.assertFalse(any('head_sha=' in path for path in queries))
            trees = [args for args, _ in world.trace if 'ls-tree' in args]
            self.assertEqual(len(trees), 2)
            self.assertTrue(all(args[-1] == evidence.WORKFLOW for args in trees))
            self.assertIsNone(collect.source_git.records._GIT_OUTPUT_TRANSPORT.get())

    def test_all_heads_keep_older_normal_and_rollback_negatives_and_real_signatures(self):
        world = World()
        world.add(run=101, number=11, minute=1, sha='b'*40, normal=True, server_false='read')
        world.add(run=102, number=12, minute=3, sha='c'*40, migration_false='rw')
        world.add(minute=10)
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(qualify(world, result).run_id, 100)
            self.assertEqual({row.sha for row in result.history.workflows},
                             {world.binding.producer_sha, 'b'*40, 'c'*40})
            self.assertEqual(len(result.history.attempts), 3)
            self.assertTrue(all(type(row.measurement) is evidence.AuthenticatedPair
                                for row in result.history.attempts))
            self.assertEqual(sum(args[1:3] == ['attestation','verify'] for args, _ in world.trace), 6)
            self.assertEqual(sum('ls-tree' in args for args, _ in world.trace), 6)

    def test_later_and_overlap_foreign_head_cannot_be_filtered_out(self):
        for minute in (10, 20):
            world = World(); world.add()
            world.add(run=101, number=9, minute=minute, sha='b'*40, server_false='workers')
            with self.subTest(minute=minute), harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                with self.assertRaises(evidence.CompatRefusal): qualify(world, result)
                self.assertEqual(len(result.history.attempts), 2)

    def test_own_unknown_workflow_and_proven_absence_do_not_borrow_H_steps(self):
        for raw in (None, b'', WORKFLOW + b'# unsupported\n'):
            world = World(); world.add()
            world.add(run=101, number=9, minute=1, sha='b'*40)
            world.workflow_bytes['b'*40] = raw
            with self.subTest(absent=raw is None), harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                actual = next(row for row in result.history.workflows if row.sha == 'b'*40)
                self.assertEqual(actual.raw, raw)
                foreign = next(row for row in result.history.attempts
                    if json.loads(row.envelope.raw)['run']['head_sha'] == 'b'*40)
                self.assertIsNone(envelope.replay(actual, foreign.envelope,
                    observed_at=NOW).attempt.execution_window)
                with self.assertRaises(evidence.CompatRefusal): qualify(world, result)

    def test_complete_negative_and_actual_final_catalog_are_retained(self):
        for fields, expected in (({'server_false':'workers'}, 'compat-old-server-incompatible'),
                ({'migration_false':'rw'}, 'irreversible-needs-fix-forward-mode'),
                ({'forward':False, 'catalog_sha256':'9'*64}, 'compat-stale')):
            world = World(); world.add(**fields)
            with self.subTest(fields=fields), harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                row = result.history.attempts[0]
                attempt = envelope.replay(result.history.workflows[0], row.envelope, observed_at=NOW).attempt
                measured = evidence.parse_authenticated_rollback_pair(row.measurement, attempt)
                self.assertFalse(measured.positive)
                self.assertEqual(json.loads(row.measurement.result)['catalog_sha256'],
                    fields.get('catalog_sha256', world.binding.database_base.catalog.catalog_sha256))
                with self.assertRaises(evidence.CompatRefusal) as caught: qualify(world, result)
                self.assertEqual(caught.exception.code, expected)

    def test_bad_signature_only_older_known_work_may_be_superseded(self):
        world = World(); world.add(run=101, number=9, minute=1); world.add()
        world.signers[101,1].fail_name = 'compat-context.json'
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(result.issues, ('artifact-unverified',))
            self.assertEqual(qualify(world, result).run_id, 100)
            self.assertEqual(sum(row.measurement is evidence.MissingEvidence.UNKNOWN
                                 for row in result.history.attempts), 1)
            downloads = [args[4] for args, _ in world.trace if args[1:4] == ['api','--method','GET']
                         and args[4].endswith('/zip')]
            self.assertEqual(len(downloads), len(set(downloads)))

    def test_expired_or_missing_artifact_never_becomes_a_receipt(self):
        for mode in ('expired', 'missing'):
            world = World(); world.add()
            if mode == 'expired': world.artifacts[100][0]['expired'] = True
            else: world.artifacts[100] = []
            with self.subTest(mode=mode), harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                self.assertIs(type(result.history.attempts[0].measurement), evidence.MissingEvidence)
                self.assertFalse(any(args[1:3] == ['attestation','verify'] for args, _ in world.trace))
                with self.assertRaises(evidence.CompatRefusal): qualify(world, result)

    def test_run_scoped_name_source_period_and_download_length_are_bound(self):
        for mode in ('name', 'source', 'period', 'size'):
            world = World(); world.add()
            meta = world.artifacts[100][0]
            if mode == 'name': meta['name'] = 'compat-result-' + 'b'*40 + '-1'
            elif mode == 'source': meta['workflow_run']['head_sha'] = 'b'*40
            elif mode == 'period': meta['created_at'] = world.runs[100]['created_at']
            else: meta['size_in_bytes'] += 1
            with self.subTest(mode=mode), harness(world) as (world, repo, options):
                if mode in ('name', 'source'):
                    self.refusal(world, repo, options)
                else:
                    result = execute(world, repo, options)
                    self.assertIs(result.history.attempts[0].measurement, evidence.MissingEvidence.UNKNOWN)
                    with self.assertRaises(evidence.CompatRefusal): qualify(world, result)

    def test_every_attempt_and_fresh_rerun_uses_actual_execution(self):
        world = World()
        world.add(attempt=1, latest=2, minute=1, server_false='workers')
        world.add(attempt=2, latest=2, minute=1)
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(qualify(world, result).run_attempt, 2)
            self.assertIsNone(qualify(world, result).requested_at)
            self.assertEqual(len(result.history.attempts), 2)

    def test_reused_job_attempt_label_does_not_create_fresh_work(self):
        world = World(); world.add(attempt=1, latest=2, minute=1)
        world.add(attempt=2, latest=2, minute=1)
        old = world.jobs[100,1][0]
        world.set_job(100,2,old['name'], started_at=old['started_at'],
                      completed_at=old['completed_at'], steps=copy.deepcopy(old['steps']))
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            with self.assertRaises(evidence.CompatRefusal): qualify(world, result)

    def test_global_attempt_cap_is_not_reset_per_event_or_head(self):
        world = World(); world.add(event='push'); world.add(run=101, number=11, sha='b'*40)
        world.runs[100]['run_attempt'] = 250; world.runs[101]['run_attempt'] = 251
        with harness(world) as (world, repo, options):
            self.refusal(world, repo, options)
            self.assertFalse(any('ls-tree' in args for args, _ in world.trace))

    def test_full_page_is_followed_by_empty_sentinel_and_all_runs_preserved(self):
        world = World()
        for index in range(100):
            world.add(run=100+index, number=10+index, minute=10+index,
                      missing=evidence.MissingEvidence.MISSING)
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(len(result.history.attempts), 100)
            pages = [args[4] for args, _ in world.trace if args[1:4] == ['api','--method','GET']
                     and '/runs?branch=' in args[4] and 'event=workflow_dispatch' in args[4]]
            self.assertEqual(sum(path.endswith('page=2') for path in pages), 2)
            self.assertEqual(result.history.enumeration, 'complete-visible')

    def test_duplicates_gaps_missing_fields_and_check_coverage_refuse(self):
        mutations = [lambda w: w.jobs[100,1].append(copy.deepcopy(w.jobs[100,1][0])),
            lambda w: w.jobs[100,1][0].pop('created_at'),
            lambda w: w.checks[w.runs[100]['check_suite_id']].pop(),
            lambda w: w.artifacts[100].append(copy.deepcopy(w.artifacts[100][0])),
            lambda w: w.runs.__setitem__(100, {**w.runs[100], 'run_attempt': 2})]
        for mutation in mutations:
            world = World(); world.add(); mutation(world)
            with self.subTest(mutation=mutation), harness(world) as (world, repo, options):
                self.refusal(world, repo, options)

    def test_changed_second_inventory_cannot_be_complete(self):
        for mode in ('run', 'artifact', 'job', 'removed'):
            world = World(); world.add(); changed = [False]
            def hook(path, value):
                if world.workflow_reads == 2 and not changed[0]:
                    changed[0] = True
                    if mode == 'run': world.runs[100]['updated_at'] = actions.stamp(NOW)
                    elif mode == 'artifact': world.artifacts[100][0]['expires_at'] = actions.stamp(NOW+timedelta(days=5))
                    elif mode == 'job': world.jobs[100,1][0]['steps'][1]['name'] = 'unreviewed'
                    else: world.runs.clear()
                return value
            world.hook = hook
            with self.subTest(mode=mode), harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                self.assertEqual(result.history.enumeration, 'incomplete')
                self.assertTrue(set(result.issues) & {'inventory-changed','inventory-unavailable'})
                with self.assertRaises(evidence.CompatRefusal): qualify(world, result)

    def test_second_transport_unavailable_retains_only_incomplete_first_capture(self):
        world = World(); world.add()
        def broken(path, value):
            if world.workflow_reads == 2: return {'PRIVATE': 'not the workflow schema'}
            return value
        world.hook = broken
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(result.history.enumeration, 'incomplete')
            self.assertEqual(result.issues, ('inventory-unavailable',))

    def test_source_missing_commit_nonregular_truncated_and_H_absent_refuse(self):
        for mode in ('commit', 'mode', 'truncated', 'H-absent'):
            with self.subTest(mode=mode), harness() as (world, repo, options):
                if mode == 'commit': world.source.commits.clear()
                elif mode == 'H-absent': world.source.files.clear()
                else:
                    def mutate(args, result):
                        if mode == 'mode' and args[:2] == ['ls-tree','-z']:
                            return replace(result, stdout=result.stdout.replace(b'100644', b'120000'))
                        if mode == 'truncated' and args[:2] == ['cat-file','blob']:
                            return replace(result, stdout=result.stdout[:-1])
                        return result
                    world.git_hook = mutate
                self.refusal(world, repo, options)

    def test_source_reread_change_or_late_repository_drift_refuses(self):
        for mode in ('source', 'metadata'):
            with self.subTest(mode=mode), harness() as (world, repo, options):
                def change(argv):
                    if world.workflow_reads >= 2:
                        if mode == 'source':
                            world.source.files[world.binding.producer_sha, evidence.WORKFLOW] = WORKFLOW+b'\n'
                        else:
                            (repo/'.git/shallow').write_bytes(b'PRIVATE')
                world.before = change
                # H source drift may return an incomplete inventory; a changed
                # repository invalidates the source operation itself.
                if mode == 'metadata': self.refusal(world, repo, options)
                else:
                    result = execute(world, repo, options)
                    self.assertEqual(result.history.enumeration, 'incomplete')

    def test_budget_expiring_inside_verification_is_not_artifact_unknown(self):
        with harness() as (world, repo, options):
            def expire(argv):
                if argv[1:3] == ['attestation','verify']: world.clock.value += 601
            world.before = expire
            self.refusal(world, repo, options)
            self.assertLessEqual(sum(args[1:3] == ['attestation','verify'] for args, _ in world.trace), 1)

    def test_combined_Git_GH_command_and_stream_allowance_is_not_reset(self):
        for mode in ('commands', 'bytes'):
            with self.subTest(mode=mode), harness() as (world, repo, options):
                if mode == 'commands': options['budget'].commands = transport.MAX_COMMANDS - 2
                else: options['budget'].captured_bytes = transport.MAX_BYTES - 65537
                self.refusal(world, repo, options)
                self.assertLessEqual(len(world.trace), 2)

    def test_raw_history_cap_charges_sources_and_full_repeated_envelopes(self):
        world = World(); first = world.add(); world.add(run=101, number=11, minute=20)
        bound = len(WORKFLOW) + len(first.envelope.raw) + 64
        with harness(world) as (world, repo, options), mock.patch.object(history, 'MAX_RAW_BYTES', bound):
            self.refusal(world, repo, options)
            self.assertFalse(any(args[1:3] == ['attestation','verify'] for args, _ in world.trace))

    def test_private_paths_and_public_inputs_refuse_without_commands(self):
        with harness() as (world, repo, options):
            unsafe = repo/'inside'; unsafe.mkdir()
            options['scratch'] = unsafe
            self.refusal(world, repo, options)
            self.assertEqual(world.trace, [])
        with harness() as (world, repo, options):
            options['token'] = 'PRIVATE\nTOKEN'
            self.refusal(world, repo, options)
            self.assertEqual(world.trace, [])

    def test_observation_id_is_derived_and_binds_complete_measurements(self):
        ids = []
        for negative in (False, True):
            world = World(); world.add(server_false='read' if negative else None)
            with harness(world) as (world, repo, options):
                result = execute(world, repo, options)
                self.assertRegex(result.history.observation_id, r'^[0-9a-f]{64}$')
                self.assertRegex(result.transcript_sha256, r'^[0-9a-f]{64}$')
                self.assertNotIn(support.GH_TOKEN, repr(result))
                ids.append(result.history.observation_id)
        self.assertNotEqual(*ids)

    def test_concrete_resolve_collect_fresh_resolve_bridge_keeps_public_guard(self):
        request_fixtures.sources.RollbackProofResolverTests.setUpClass()
        before = request_fixtures.source_resolution()
        binding = request_fixtures.producer_binding(before)
        world = World(binding); world.add()
        with harness(world) as (world, repo, options):
            result = execute(world, repo, options)
        after = request_fixtures.source_resolution()
        self.assertEqual(before, after)
        qualified = request_fixtures.bridge.qualify_rollback(after, result)
        self.assertEqual(qualified.binding, binding)
        with self.assertRaisesRegex(request_fixtures.bridge.policy.PolicyRefusal,
                                    'rollback-qualification-unavailable'):
            request_fixtures.bridge.policy.rollback_plan(after.resolved.chain, binding.selected.release)
        with self.assertRaises(evidence.CompatRefusal):
            request_fixtures.bridge.qualify_rollback(replace(after,
                workflow=after.workflow+b'PRIVATE'), result)


if __name__ == '__main__':
    unittest.main()


class SharedProjectionTests(unittest.TestCase):
    def test_all_six_original_rollback_helpers_are_exact_shared_aliases(self):
        for local, shared in (('_project', '_project_fields'), ('_canonical', '_canonical_projection'),
                ('_run', '_project_run'), ('_job', '_project_job'), ('_check', '_project_check'),
                ('_artifact', '_project_artifact')):
            self.assertIs(getattr(collect, local), getattr(envelope, shared))

    def test_fixed_projection_reproduces_complete_original_envelope(self):
        raw = json.loads(histories.row().envelope.raw)
        rebuilt = {'version':1, 'run':collect._run(raw['run'], raw['run']['latest_run_attempt']),
            'jobs':[collect._job(row) for row in raw['jobs']],
            'checks':[collect._check(row) for row in raw['checks']],
            'artifacts':[collect._artifact(row) for row in raw['artifacts']]}
        self.assertEqual(collect._canonical(rebuilt, envelope.MAX_RAW_BYTES), histories.row().envelope.raw)
        for name in ('jobs', 'checks', 'artifacts'):
            changed = copy.deepcopy(raw); changed[name][0].pop('id')
            projector = {'jobs':collect._job, 'checks':collect._check, 'artifacts':collect._artifact}[name]
            with self.subTest(name=name), self.assertRaises(evidence.CompatRefusal):
                projector(changed[name][0])

    def test_canonical_shared_buffer_limit_includes_terminal_lf(self):
        raw = collect._canonical({'a':1}, 8)
        self.assertEqual(raw, b'{"a":1}\n')
        with self.assertRaises(evidence.CompatRefusal): collect._canonical({'a':1}, 7)

    def test_rollback_own_w0_source_window_survives_current_pin_transition(self):
        with mock.patch.object(envelope, 'WORKFLOW_SHA256', 'f'*64), harness() as (world, repo, options):
            result = execute(world, repo, options)
            self.assertEqual(qualify(world, result).run_id, 100)
            self.assertEqual(len([a for a, _ in world.trace if a[1:3] == ['attestation', 'verify']]), 2)
