"""Closed API/verified-byte fixtures; no network or cryptographic authority."""
import copy
from dataclasses import FrozenInstanceError, replace
from datetime import timedelta, timezone
import hashlib
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat_envelope as envelope
import compat_rollback_history as history_policy
import compat_evidence as evidence
import test_compat_collect as collected_fixtures
import test_compat_evidence as fixture
import test_compat_rollback as rollback_fixtures

WORKFLOW = collected_fixtures.WORKFLOW
NOW = collected_fixtures.NOW


def _rebind(pair, sha, run, attempt, artifact_id):
    result, context = json.loads(pair.result), json.loads(pair.context)
    result['sha'] = sha
    context['sha'] = context['workflow_sha'] = sha
    result_raw = fixture.canonical(result)
    context['result_sha256'] = fixture.digest(result_raw)
    context_raw = fixture.canonical(context)
    subjects = [replace(subject, signer_sha=sha, source_sha=sha, run_id=run,
        run_attempt=attempt, subject_sha256=fixture.digest(raw))
        for subject, raw in ((pair.result_subject, result_raw), (pair.context_subject, context_raw))]
    artifact = replace(pair.artifact, artifact_id=artifact_id, name=f'compat-result-{sha}-{attempt}',
        run_id=run, run_attempt=attempt, sha=sha,
        files=(evidence.ArtifactFile('compat-result.json', fixture.digest(result_raw)),
               evidence.ArtifactFile('compat-context.json', fixture.digest(context_raw))))
    return evidence.AuthenticatedPair(result_raw, context_raw, *subjects, artifact)


def row(binding=None, *, run=100, number=10, attempt=1, latest=None, minute=10,
        sha=None, server_false=None, migration_false=None, forward=True,
        catalog_sha256=None, missing=None, normal=False, event='workflow_dispatch'):
    """Build actual-shaped closed facts from the existing controlled API world."""
    binding = rollback_fixtures.binding() if binding is None else binding
    sha = binding.producer_sha if sha is None else sha
    latest = attempt if latest is None else latest
    world = collected_fixtures.World()
    world.add(run, number, latest, minute, event=event,
              negative=server_false is not None or migration_false is not None or not forward)
    run_row = copy.deepcopy(world.attempts[run, attempt])
    run_row['head_sha'], run_row['latest_run_attempt'] = sha, latest
    jobs = copy.deepcopy(world.jobs[run, attempt])
    checks = copy.deepcopy(world.checks[run_row['check_suite_id']])
    for item in (*jobs, *checks):
        item['head_sha'] = sha
    artifacts = copy.deepcopy(world.artifacts[run])
    for item in artifacts:
        item['workflow_run']['head_sha'] = sha
        item['name'] = item['name'].replace(fixture.SHA, sha)
        item['size_in_bytes'] = 4096
    raw = fixture.canonical({'version':1, 'run':run_row,
        'jobs':sorted(jobs, key=lambda v:v['id']), 'checks':sorted(checks, key=lambda v:v['id']),
        'artifacts':sorted(artifacts, key=lambda v:v['id'])})
    if missing is not None:
        pair = missing
    else:
        if normal:
            pair = fixture.pair(run, attempt, version=1, event=event, server_false=server_false)
        else:
            pair = rollback_fixtures.pair(run, attempt, binding=replace(binding, producer_sha=sha),
                server_false=server_false, migration_false=migration_false, forward=forward,
                catalog_sha256=catalog_sha256)
        pair = _rebind(pair, sha, run, attempt, run * 100 + attempt)
    return history_policy.RollbackAttempt(envelope.EnvelopeFacts(raw), pair)


def history(binding=None, *rows, workflows=None, attempts=None):
    binding = rollback_fixtures.binding() if binding is None else binding
    if attempts is not None:
        rows = attempts
    elif not rows:
        rows = (row(binding),)
    if workflows is None:
        heads = {binding.producer_sha, *(json.loads(r.envelope.raw)['run']['head_sha'] for r in rows)}
        workflows = tuple(envelope.WorkflowObservation(sha, WORKFLOW) for sha in sorted(heads))
    return history_policy.RollbackHistory(binding.producer_sha, evidence.REPOSITORY, evidence.WORKFLOW,
        fixture.OBSERVATION, NOW, workflows, tuple(rows))


def request(binding=None):
    binding = rollback_fixtures.binding() if binding is None else binding
    return history_policy.RollbackRequest(binding, WORKFLOW, fixture.OBSERVATION)


def changed(value, callback):
    raw = json.loads(value.envelope.raw)
    callback(raw)
    return replace(value, envelope=envelope.EnvelopeFacts(fixture.canonical(raw)))


class RollbackHistoryTests(unittest.TestCase):
    def qualify(self, value=None, expected=None):
        return history_policy.require_rollback_compat(request() if expected is None else expected,
                                                       history() if value is None else value)

    def refuse(self, value, code='compat-stale', expected=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            self.qualify(value, expected)
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn('PRIVATE', str(caught.exception))
        return caught.exception

    def test_positive_replays_complete_raw_envelope_and_binds_exact_subjects(self):
        value = history()
        qualified = self.qualify(value)
        replayed = envelope.replay(value.workflows[0], value.attempts[0].envelope, observed_at=NOW)
        self.assertIs(type(qualified), history_policy.QualifiedRollback)
        self.assertEqual(qualified.binding, request().binding)
        self.assertEqual(qualified.execution_window, replayed.attempt.execution_window)
        self.assertEqual(qualified.result_sha256, fixture.digest(value.attempts[0].measurement.result))
        self.assertEqual(qualified.context_sha256, fixture.digest(value.attempts[0].measurement.context))
        self.assertEqual(qualified.artifact_id, 10001)
        self.assertEqual(qualified.workflow_sha, value.producer_sha)
        with self.assertRaises(FrozenInstanceError): qualified.run_id = 0

    def test_exact_current_negative_preserves_specific_refusal(self):
        for field in evidence.SERVERS:
            self.refuse(history(None, row(server_false=field)), 'compat-old-server-incompatible')
        for field in ('rw', 'ro'):
            self.refuse(history(None, row(migration_false=field)), 'irreversible-needs-fix-forward-mode')
        self.refuse(history(None, row(forward=False)))

    def test_foreign_wrong_target_and_normal_negatives_do_not_claim_requested_failure(self):
        for foreign in (row(run=101, number=11, minute=20, sha='b'*40, server_false='read'),
                        row(replace(request().binding, source_material_sha256='0'*64),
                            run=101, number=11, minute=20, migration_false='rw'),
                        row(run=101, number=11, minute=20, normal=True, server_false='read')):
            self.refuse(history(None, row(), foreign))

    def test_reviewed_foreign_earlier_negative_unknown_or_positive_can_be_superseded(self):
        for options in ({'server_false':'workers'}, {'missing':evidence.MissingEvidence.UNKNOWN}, {}):
            old = row(run=101, number=20, minute=1, sha='b'*40, **options)
            qualified = self.qualify(history(None, old, row()))
            self.assertEqual(qualified.run_id, 100)

    def test_foreign_later_overlapping_equal_start_or_unknown_always_vetoes(self):
        for minute in (10, 11, 20):
            self.refuse(history(None, row(), row(run=101, number=9, minute=minute, sha='b'*40)))
        raw = row(run=101, number=9, minute=1, sha='b'*40)
        self.refuse(history(None, row(), raw, workflows=(
            envelope.WorkflowObservation(fixture.SHA, WORKFLOW), envelope.WorkflowObservation('b'*40, None))))

    def test_unknown_or_changed_own_workflow_never_borrows_current_steps(self):
        old = row(run=101, number=9, minute=1, sha='b'*40)
        for raw in (None, b'', WORKFLOW+b'\n', b'PRIVATE'):
            value = history(None, old, row(), workflows=(
                envelope.WorkflowObservation(fixture.SHA, WORKFLOW), envelope.WorkflowObservation('b'*40, raw)))
            replayed = envelope.replay(value.workflows[1], old.envelope, observed_at=NOW)
            self.assertIsNone(replayed.attempt.execution_window)
            self.assertEqual(replayed.attempt.jobs, ())
            self.refuse(value)
        self.refuse(replace(history(), workflows=(envelope.WorkflowObservation(fixture.SHA, b'PRIVATE'),)))

    def test_only_exact_current_context4_positive_can_be_selected(self):
        self.refuse(history(None, row(normal=True)))
        self.refuse(history(None, row(sha='b'*40)))
        for binding in (replace(request().binding, source_material_sha256='0'*64),
                        replace(request().binding, selected_web_source_sha='0'*40),
                        replace(request().binding, span=replace(request().binding.span, sha256='0'*64))):
            self.refuse(history(None, row(binding)))
        self.refuse(history(None, row(catalog_sha256='0'*64)))

    def test_fresh_rerun_supersedes_higher_number_but_uses_actual_execution(self):
        first = row(attempt=1, latest=2, minute=1, missing=evidence.MissingEvidence.UNKNOWN)
        second = row(attempt=2, latest=2, minute=1)
        old = row(run=101, number=20, minute=2, server_false='read')
        qualified = self.qualify(history(None, old, second, first))
        self.assertEqual((qualified.run_id, qualified.run_attempt), (100, 2))
        self.assertIsNone(qualified.requested_at)

    def test_exact_h_matching_positive_overlap_preserves_normal_selection_rule(self):
        lower = row(run=101, number=9, minute=10)
        qualified = self.qualify(history(None, lower, row()))
        self.assertEqual(qualified.run_id, 100)
        self.refuse(history(None, replace(lower, measurement=evidence.MissingEvidence.UNKNOWN), row()))

    def test_missing_reused_or_partial_step_evidence_cannot_make_fresh_window(self):
        for label, change in (
            ('job-created-before-execution',
             lambda r:r['jobs'][0].update(created_at=collected_fixtures.stamp(fixture.T0))),
            ('missing-source-declared-step', lambda r:r['jobs'][0]['steps'].pop(1)),
            ('missing-job', lambda r:r['jobs'].pop()),
        ):
            with self.subTest(mutation=label):
                self.refuse(history(None, changed(row(), change)))

    def test_no_work_only_attempt_is_not_a_supersedable_window(self):
        def no_work(raw):
            raw['run'].update(conclusion='cancelled')
            for job in raw['jobs']:
                job.update(conclusion='skipped', steps=[], started_at=None, completed_at=None)
            for check in raw['checks']:
                check.update(conclusion='skipped', started_at=None, completed_at=None)
        old = changed(row(run=101, number=9, minute=1, missing=evidence.MissingEvidence.MISSING), no_work)
        self.refuse(history(None, old, row()))

    def test_closed_raw_json_types_keys_canonical_and_depth_are_required(self):
        original = row()
        baseline = history(None, original)
        for raw in (original.envelope.raw+b'\n', original.envelope.raw.replace(b'"version":1', b'"version":true'),
                    original.envelope.raw.replace(b'"version":1', b'"version":1,"version":1'),
                    b'{"version":NaN}', b'{"PRIVATE":'+b'['*20+b'0'+b']'*20+b'}'):
            self.refuse(replace(baseline, attempts=(replace(original, envelope=envelope.EnvelopeFacts(raw)),)))
        for change in (lambda r:r.update(window=True), lambda r:r['run'].update(sha='PRIVATE'),
                       lambda r:r['checks'][0]['app'].update(unknown=1),
                       lambda r:r['jobs'][0]['steps'][0].update(reviewed=True),
                       lambda r:r['artifacts'][0]['workflow_run'].update(unknown=0)):
            self.refuse(history(None, changed(original, change)))

    def test_raw_caller_window_cannot_override_bad_derived_evidence(self):
        self.refuse(history(None, changed(row(), lambda r:r.update(execution_window={
            'fresh':True, 'started_at':'2026-01-01T00:00:00Z', 'completed_at':'2026-01-01T01:00:00Z'}))))
        self.refuse(history(None, changed(row(), lambda r:r['jobs'][0].update(eligible=True))))

    def test_exact_job_check_source_publisher_and_time_bindings_are_mandatory(self):
        for change in (lambda r:r['checks'][0]['app'].update(id=1),
                       lambda r:r['checks'][0].update(head_sha='b'*40),
                       lambda r:r['jobs'][0].update(run_attempt=2),
                       lambda r:r['checks'][0].update(details_url='PRIVATE'),
                       lambda r:r['checks'][0].update(completed_at=None),
                       lambda r:r['run'].update(head_branch='other'),
                       lambda r:r['run'].update(event='pull_request')):
            self.refuse(history(None, changed(row(), change)))

    def test_global_job_check_artifact_ids_and_complete_listing_are_not_per_head(self):
        def reused_job(raw):
            job, check = raw['jobs'][0], raw['checks'][0]
            job['id'] = 1000100
            job['url'] = envelope.API_URL + '/actions/jobs/1000100'
            job['html_url'] = envelope.WEB + '/actions/runs/101/job/1000100'
            check['html_url'] = check['details_url'] = job['html_url']
        def unused_check(raw):
            check = {**raw['checks'][-1], 'id':99999999,
                     'url':envelope.API_URL+'/check-runs/99999999'}
            raw['checks'].append(check)
        for change in (
            reused_job,
            lambda r:r['checks'].pop(),
            lambda r:r['artifacts'][0].update(id=10001),
            unused_check,
        ):
            self.refuse(history(None, row(), changed(row(run=101, number=11, sha='b'*40), change)))

    def test_exact_500_attempt_and_501_source_boundary_counts_both_events(self):
        rows = []
        original = json.loads(row(missing=evidence.MissingEvidence.MISSING).envelope.raw)
        for i in range(500):
            raw = copy.deepcopy(original)
            rid, sha = 1000+i, f'{i+1:040x}'
            raw['run'].update(id=rid, run_number=i+1, check_suite_id=rid,
                head_sha=sha, event='push' if i%2 else 'workflow_dispatch',
                url=envelope.API_URL+f'/actions/runs/{rid}', html_url=envelope.WEB+f'/actions/runs/{rid}')
            raw.update(jobs=[], checks=[], artifacts=[])
            rows.append(history_policy.RollbackAttempt(envelope.EnvelopeFacts(fixture.canonical(raw)),
                        evidence.MissingEvidence.MISSING))
        sources = tuple(envelope.WorkflowObservation(sha, WORKFLOW if sha == fixture.SHA else None)
                        for sha in sorted({fixture.SHA, *(f'{i+1:040x}' for i in range(500))}))
        value = history(None, *rows, workflows=sources)
        self.assertIs(history_policy.validate_history(value, producer_sha=fixture.SHA), value)
        self.assertEqual(len(value.workflows), 501)
        self.refuse(value)
        self.refuse(replace(value, attempts=(*value.attempts, value.attempts[0])))

    def test_duplicate_gapped_or_rebound_runs_and_global_attempt_total_refuse(self):
        value = row()
        self.refuse(history(None, value, value))
        self.refuse(history(None, row(attempt=2, latest=2)))
        self.refuse(history(None, row(), row(run=101, number=10)))
        self.refuse(history(None, changed(row(), lambda r:r['run'].update(latest_run_attempt=500)),
                                  changed(row(run=101, number=11), lambda r:r['run'].update(latest_run_attempt=2))))
        self.refuse(replace(history(), attempts=(value,)*501))

    def test_repeated_run_and_suite_custody_must_be_identical(self):
        first, second = row(attempt=1, latest=2), row(attempt=2, latest=2)
        self.qualify(history(None, first, second))
        second = changed(second, lambda r:r['artifacts'][0].update(size_in_bytes=4097))
        self.refuse(history(None, first, second))

    def test_workflow_inventory_is_exact_sorted_unique_and_includes_h(self):
        value = history()
        for workflows in ((), value.workflows*2,
                          value.workflows+(envelope.WorkflowObservation('b'*40,WORKFLOW),),
                          (envelope.WorkflowObservation('b'*40,WORKFLOW),)):
            self.refuse(replace(value, workflows=workflows))
        foreign = row(run=101, number=9, minute=1, sha='b'*40)
        value = history(None, foreign, row())
        self.refuse(replace(value, workflows=tuple(reversed(value.workflows))))

    def test_raw_bound_includes_workflows_envelopes_and_both_paired_files(self):
        rows = [row(run=101+i, number=20+i, minute=1, sha=f'{i+1:040x}',
                    missing=evidence.MissingEvidence.UNKNOWN) for i in range(3)]
        value = history(None, row(), *rows)
        sources = tuple(replace(s, raw=b'x'*(4*1024*1024)) if s.sha != fixture.SHA else s for s in value.workflows)
        self.refuse(replace(value, workflows=sources))
        measured = value.attempts[0].measurement
        self.refuse(history(None, replace(row(), measurement=replace(measured, context=b'x'*4097))))
        baseline = history()
        self.refuse(replace(baseline, attempts=(replace(baseline.attempts[0],
            envelope=envelope.EnvelopeFacts(b'x'*(4*1024*1024+1))),)))

    def test_artifact_inventory_period_expiry_and_measured_subject_are_bound(self):
        for change in (lambda r:r['artifacts'].clear(),
                       lambda r:r['artifacts'][0].update(expired=True),
                       lambda r:r['artifacts'][0]['workflow_run'].update(head_sha='b'*40),
                       lambda r:r['artifacts'][0].update(created_at=r['run']['created_at'])):
            self.refuse(history(None, changed(row(), change)))
        original = row()
        self.refuse(history(None, replace(original, measurement=replace(original.measurement,
            context_subject=replace(original.measurement.context_subject, signer_sha='b'*40)))))

    def test_collected_metadata_and_history_coverage_are_not_authority_flags(self):
        value = history_policy.CollectedRollbackHistory(history(), (), 1, 0, 'a'*64)
        self.assertIs(history_policy.validate_collected(value), value)
        for item in (replace(value, command_count=True), replace(value, command_count=6001),
                     replace(value, captured_bytes=128*1024*1024+1), replace(value, issues=('PRIVATE',))):
            with self.assertRaises(evidence.CompatRefusal): history_policy.validate_collected(item)
        self.refuse(replace(history(), enumeration='incomplete'))
        self.refuse(replace(history(), whole_run_deletion_coverage='complete'))
        self.refuse(replace(history(), observed_at=NOW.replace(tzinfo=timezone(timedelta(hours=1)))))
        self.refuse(replace(history(), observed_at=NOW.replace(fold=1)))

    def test_fatal_collection_issues_cannot_be_hidden_by_complete_enumeration(self):
        value = history_policy.CollectedRollbackHistory(history(), (), 1, 0, 'a'*64)
        for issue in ('inventory-changed', 'inventory-unavailable', 'collection-deadline'):
            bad = replace(value, issues=(issue,))
            with self.assertRaises(evidence.CompatRefusal): history_policy.validate_collected(bad)
            incomplete = replace(bad, history=replace(bad.history, enumeration='incomplete'))
            self.assertIs(history_policy.validate_collected(incomplete), incomplete)
            self.refuse(incomplete.history)
        self.assertIs(history_policy.validate_collected(replace(value, issues=('artifact-unverified',))).history,
                      value.history)

    def test_missing_empty_history_and_normal_qualifier_are_distinct(self):
        self.refuse(history(attempts=()), 'compat-missing')
        with self.assertRaises(evidence.CompatRefusal):
            evidence.require_compat(fixture.request(), history())
        self.refuse(fixture.history(fixture.attempt()))

    def test_pure_qualification_does_not_read_clock_source_or_transport(self):
        value = history()
        with mock.patch('builtins.open', side_effect=AssertionError('I/O')), \
             mock.patch('subprocess.run', side_effect=AssertionError('command')), \
             mock.patch('time.monotonic', side_effect=AssertionError('clock')):
            self.assertEqual(self.qualify(value).run_id, 100)


if __name__ == '__main__':
    unittest.main()


class OwnWorkflowReplayTests(unittest.TestCase):
    def test_real_w0_raw_workflow_stays_known_after_current_pin_changes(self):
        facts = row().envelope
        source = envelope.WorkflowObservation(rollback_fixtures.binding().producer_sha, WORKFLOW)
        before = envelope.replay(source, facts, observed_at=NOW)
        with mock.patch.object(envelope, 'WORKFLOW_SHA256', 'f'*64):
            after = envelope.replay(source, facts, observed_at=NOW)
        self.assertEqual(after, before)
        self.assertIsNotNone(after.attempt.execution_window)

    def test_unknown_workflow_never_borrows_historical_steps(self):
        facts = row().envelope
        sha = rollback_fixtures.binding().producer_sha
        for raw in (None, WORKFLOW+b'\n'):
            with self.subTest(raw_is_none=raw is None):
                result = envelope.replay(envelope.WorkflowObservation(sha, raw), facts, observed_at=NOW)
                self.assertIsNone(result.attempt.execution_window)
                self.assertEqual(result.attempt.jobs, ())
