"""Normal collection's narrowly witnessed first-attempt timestamp split.

Use the existing synthetic transport, real envelope predicates and signature
validators; these fixtures never launch GitHub CLI or contact a service.
"""
import copy
from datetime import timedelta
import unittest

import compat_collect as collect
import compat_envelope as envelope
import compat_evidence as evidence
import test_compat_collect as actions


def split_world(**kwargs):
    world = actions.World()
    world.add(**kwargs)
    listing = copy.deepcopy(world.runs[100])
    listing['run_started_at'] = listing['created_at']
    attempt = copy.deepcopy(listing)
    attempt['created_at'] = actions.stamp(
        collect._date(listing['created_at']) + timedelta(seconds=1))
    world.runs[100] = listing
    world.attempts[100, listing['run_attempt']] = attempt
    return world


class NormalAttemptClockTests(unittest.TestCase):
    def test_exact_split_qualifies_with_two_signatures_and_actual_job_window(self):
        world = split_world()
        originals = copy.deepcopy((world.runs, world.attempts, world.jobs, world.checks))
        with actions.harness(world) as (w, client, scratch):
            result = actions.execute(client, scratch)
            qualified = actions.qualify(result)
            attempt = result.history.attempts[0]
            later = collect._date(w.attempts[100, 1]['created_at'])
            self.assertEqual(qualified.run_id, 100)
            self.assertEqual(attempt.requested_at, collect._date(w.runs[100]['created_at']))
            self.assertEqual(attempt.requested_at + timedelta(seconds=1), later)
            self.assertEqual(attempt.started_at, later)
            self.assertEqual(attempt.execution_window, evidence.ExecutionWindow(
                min(collect._date(j['started_at']) for j in w.jobs[100, 1]),
                max(collect._date(j['completed_at']) for j in w.jobs[100, 1]), True))
            self.assertEqual(qualified.execution_window, attempt.execution_window)
            self.assertEqual(result.history.enumeration, 'complete-visible')
            self.assertEqual(w.workflow_reads, 2)
            self.assertEqual(len([a for a, _ in w.calls if a[1:3] == ['attestation', 'verify']]), 2)
        self.assertEqual((world.runs, world.attempts, world.jobs, world.checks), originals)

    def test_snapshot_keeps_both_raw_run_boundaries(self):
        world = split_world()
        with actions.harness(world) as (w, client, _):
            attempts, snapshot, _ = collect._snapshot(client, actions.REQUEST, w.steps, actions.NOW)
            _, listing, projections, _ = snapshot
            raw_attempt = projections[0][0]
            self.assertEqual(listing[100]['created'], collect._date(w.runs[100]['created_at']))
            self.assertEqual(raw_attempt['created'], collect._date(w.attempts[100, 1]['created_at']))
            self.assertEqual(raw_attempt['started'], collect._date(w.attempts[100, 1]['run_started_at']))
            self.assertLess(raw_attempt['started'], raw_attempt['created'])
            self.assertNotEqual(listing[100]['created'], raw_attempt['created'])
            self.assertEqual(attempts[0].started_at, raw_attempt['created'])

    def test_ordered_push_dispatch_and_rerun_keep_their_original_start(self):
        for kwargs in ({}, {'event': 'workflow_dispatch'}, {'attempts': 2}):
            world = actions.World()
            world.add(**kwargs)
            with self.subTest(kwargs=kwargs), actions.harness(world) as (w, client, scratch):
                result = actions.execute(client, scratch)
                for attempt in result.history.attempts:
                    raw = w.attempts[100, attempt.run_attempt]
                    self.assertEqual(attempt.started_at, collect._date(raw['run_started_at']))
                self.assertEqual(actions.qualify(result).run_attempt, kwargs.get('attempts', 1))

    def test_only_the_exact_whole_second_first_successful_push_pair_is_admitted(self):
        for mode in ('two-seconds', 'fractional', 'direction', 'list-inverted',
                     'list-not-started-at-creation', 'updated', 'identity',
                     'dispatch', 'rerun', 'active', 'failed'):
            kwargs = {'event': 'workflow_dispatch'} if mode == 'dispatch' else {}
            if mode == 'rerun':
                kwargs = {'attempts': 2}
            world = split_world(**kwargs)
            listing = world.runs[100]
            attempt = world.attempts[100, listing['run_attempt']]
            created = collect._date(listing['created_at'])
            if mode == 'two-seconds':
                attempt['created_at'] = actions.stamp(created + timedelta(seconds=2))
            elif mode == 'fractional':
                for row in (listing, attempt):
                    for key in ('created_at', 'run_started_at'):
                        row[key] = actions.stamp(collect._date(row[key]) + timedelta(microseconds=500000))
            elif mode == 'direction':
                attempt['created_at'] = actions.stamp(created - timedelta(seconds=1))
            elif mode == 'list-inverted':
                listing['created_at'] = attempt['created_at']
            elif mode == 'list-not-started-at-creation':
                listing['created_at'] = actions.stamp(created - timedelta(seconds=1))
            elif mode == 'updated':
                attempt['updated_at'] = actions.stamp(collect._date(attempt['updated_at']) + timedelta(seconds=1))
            elif mode == 'identity':
                attempt['head_sha'] = 'f' * 40
            elif mode in ('active', 'failed'):
                for row in (listing, attempt):
                    row.update(status='in_progress' if mode == 'active' else 'completed',
                               conclusion=None if mode == 'active' else 'failure')
            with self.subTest(mode=mode), actions.harness(world) as (w, client, scratch):
                with self.assertRaises(evidence.CompatRefusal):
                    actions.execute(client, scratch)
                self.assertFalse(any('/zip' in arg for argv, _ in w.calls for arg in argv))

    def test_later_creation_is_a_lower_bound_for_every_real_job(self):
        for name in evidence.JOBS:
            world = split_world()
            world.set_job(100, 1, name, created_at=world.runs[100]['created_at'])
            with self.subTest(job=name), actions.harness(world) as (_, client, scratch):
                with self.assertRaisesRegex(evidence.CompatRefusal, 'collect-attempt-clock-jobs'):
                    actions.execute(client, scratch)

    def test_all_four_successful_jobs_steps_and_matching_checks_are_required(self):
        for mode in ('missing', 'skipped', 'failed', 'missing-step', 'check-time', 'check-owner'):
            world = split_world()
            if mode == 'missing':
                world.jobs[100, 1].pop()
                world.checks[100001].pop()
            elif mode == 'skipped':
                world.set_job(100, 1, 'compat-attest', conclusion='skipped', steps=[])
            elif mode == 'failed':
                world.set_job(100, 1, 'compat', conclusion='failure')
            elif mode == 'missing-step':
                world.jobs[100, 1][0]['steps'].pop(1)
            elif mode == 'check-time':
                world.checks[100001][0]['started_at'] = world.runs[100]['created_at']
            else:
                world.checks[100001][0]['app']['id'] = 1
            with self.subTest(mode=mode), actions.harness(world) as (_, client, scratch):
                with self.assertRaises(evidence.CompatRefusal):
                    actions.execute(client, scratch)

    def test_either_endpoint_change_between_snapshots_prevents_qualification(self):
        for endpoint in ('listing', 'attempt', 'both'):
            world = split_world()
            def change(path, value):
                if world.workflow_reads == 2:
                    rows = []
                    if endpoint in ('listing', 'both') and 'workflow_runs' in value:
                        rows = value['workflow_runs']
                    elif endpoint in ('attempt', 'both') and path.endswith('/attempts/1'):
                        rows = [value]
                    for row in rows:
                        for key in ('created_at', 'run_started_at', 'updated_at'):
                            row[key] = actions.stamp(collect._date(row[key]) + timedelta(seconds=1))
                return value
            world.hook = change
            with self.subTest(endpoint=endpoint), actions.harness(world) as (_, client, scratch):
                result = actions.execute(client, scratch)
                self.assertEqual(result.history.enumeration, 'incomplete')
                self.assertIsInstance(result.history.attempts[0].measurement, evidence.AuthenticatedPair)
                with self.assertRaises(evidence.CompatRefusal):
                    actions.qualify(result)

    def test_split_does_not_replace_signatures_or_complete_history(self):
        for mode in ('signature', 'missing-artifact', 'later-negative'):
            world = split_world()
            if mode == 'signature':
                world.signers[100, 1].fail_name = 'compat-context.json'
            elif mode == 'missing-artifact':
                world.artifacts[100].clear()
            else:
                world.add(200, 20, minute=60, negative=True)
            with self.subTest(mode=mode), actions.harness(world) as (_, client, scratch):
                result = actions.execute(client, scratch)
                with self.assertRaises(evidence.CompatRefusal):
                    actions.qualify(result)

    def test_shared_default_outage_and_rollback_envelope_replay_remain_strict(self):
        world = split_world()
        row = world.attempts[100, 1]
        with self.assertRaisesRegex(evidence.CompatRefusal, 'collect-run-time'):
            envelope._run_meta(row, actions.REQUEST, 900, actions.NOW)
        with actions.harness(world) as (_, client, scratch):
            with self.assertRaisesRegex(evidence.CompatRefusal, 'collect-run-time'):
                collect.collect_outage_history(actions.REQUEST, client, scratch, wall_clock=lambda: actions.NOW)
        raw = envelope._canonical_projection({
            'version': 1, 'run': envelope._project_run(row, 1),
            'jobs': [envelope._project_job(j) for j in world.jobs[100, 1]],
            'checks': [envelope._project_check(c) for c in world.checks[100001]],
            'artifacts': [envelope._project_artifact(a) for a in world.artifacts[100]],
        }, envelope.MAX_RAW_BYTES)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'collect-run-time'):
            envelope.replay(envelope.WorkflowObservation(actions.REQUEST.sha, actions.WORKFLOW),
                            envelope.EnvelopeFacts(raw), observed_at=actions.NOW)


if __name__ == '__main__':
    unittest.main()
