"""Fresh execution policy; synthetic facts are not cryptographic authority."""
from dataclasses import replace
from datetime import timedelta, timezone
import unittest

import compat_evidence as evidence
import test_compat_evidence as fixture


class ExecutionWindowTests(unittest.TestCase):
    def qualify(self, *rows):
        return evidence.require_compat(fixture.request(), fixture.history(*rows))

    def reject(self, *rows, detail=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            self.qualify(*rows)
        if detail:
            self.assertEqual(caught.exception.detail, detail)
        return caught.exception

    def rerun(self):
        first = fixture.attempt(latest_attempt=2, minute=1, conclusion="failure",
                                measurement=evidence.MissingEvidence.MISSING)
        second = replace(fixture.attempt(rerun=2, minute=20), requested_at=None)
        return first, second

    def test_fresh_rerun_with_unknown_human_request_time_qualifies(self):
        first, second = self.rerun()
        result = self.qualify(first, second)
        self.assertEqual((result.run_id, result.run_attempt), (100, 2))
        self.assertIsNone(result.requested_at)
        self.assertEqual(result.started_at, second.started_at)
        self.assertEqual(result.execution_window, second.execution_window)

    def test_processing_and_actual_execution_starts_stay_distinct(self):
        row = fixture.attempt()
        window = replace(row.execution_window, started_at=row.started_at + timedelta(seconds=30))
        result = self.qualify(replace(row, requested_at=None, execution_window=window))
        self.assertEqual(result.started_at, row.started_at)
        self.assertEqual(result.execution_window.started_at, window.started_at)

    def test_earlier_failure_can_finish_after_queueing_but_before_fresh_execution(self):
        row = fixture.attempt(minute=20)
        row = replace(row, requested_at=fixture.T0)
        old = fixture.attempt(run=200, number=20, minute=10, conclusion="failure",
                              measurement=evidence.MissingEvidence.MISSING)
        self.assertEqual(self.qualify(old, row).run_id, row.run_id)

    def test_higher_number_earlier_negative_can_be_superseded_by_fresh_rerun(self):
        first, second = self.rerun()
        negative = fixture.attempt(run=200, number=20, minute=10, conclusion="failure",
                                   measurement=fixture.pair(200, server_false="read"))
        self.assertEqual(self.qualify(negative, second, first).run_attempt, 2)

    def test_later_older_number_rerun_negative_still_vetoes(self):
        first = fixture.attempt(run=99, number=9, latest_attempt=2, minute=1)
        later = fixture.attempt(run=99, number=9, rerun=2, minute=20, conclusion="failure",
                                measurement=fixture.pair(99, 2, server_false="workers"))
        later = replace(later, requested_at=None)
        self.assertEqual(self.reject(first, fixture.attempt(), later).code,
                         "compat-old-server-incompatible")

    def test_equal_boundary_and_overlap_do_not_supersede_incomplete(self):
        current = fixture.attempt(minute=20)
        for minute in (19, 20, 21):
            old = fixture.attempt(run=99, number=9, minute=minute, conclusion="failure",
                                  measurement=evidence.MissingEvidence.EXPIRED)
            with self.subTest(minute=minute):
                self.reject(old, current, detail="later-or-overlapping-incomplete")

    def test_missing_or_reused_window_has_no_old_timestamp_fallback(self):
        row = fixture.attempt()
        for window in (None, replace(row.execution_window, fresh=False)):
            with self.subTest(window=window):
                self.reject(replace(row, execution_window=window))
                old = fixture.attempt(run=99, number=9, minute=1, conclusion="failure",
                                      measurement=evidence.MissingEvidence.UNKNOWN)
                old_window = None if window is None else replace(old.execution_window, fresh=False)
                self.reject(replace(old, execution_window=old_window), row,
                            detail="later-or-unknown-timing")

    def test_known_cancelled_work_and_skipped_dependants_can_be_superseded(self):
        old = fixture.attempt(run=99, number=9, minute=1, conclusion="cancelled",
                              measurement=evidence.MissingEvidence.MISSING)
        old = replace(old, requested_at=None, jobs=tuple(
            replace(j, conclusion="cancelled" if i == 0 else "skipped")
            for i, j in enumerate(old.jobs)))
        self.assertEqual(self.qualify(old, fixture.attempt()).run_id, 100)

    def test_window_requires_all_terminal_jobs_and_actual_work(self):
        row = fixture.attempt()
        cases = [replace(row, jobs=row.jobs[:-1]),
                 replace(row, jobs=tuple(replace(j, conclusion="skipped") for j in row.jobs)),
                 replace(row, status="in_progress", conclusion=None, completed_at=None),
                 replace(row, jobs=(replace(row.jobs[0], status="in_progress", conclusion=None),) + row.jobs[1:])]
        for item in cases:
            with self.subTest(item=item): self.reject(item)

    def test_window_shape_timing_and_observer_bounds_are_strict(self):
        row = fixture.attempt(); window = row.execution_window
        cases = [True, {}, replace(window, fresh=1),
                 replace(window, started_at=window.started_at.replace(tzinfo=None)),
                 replace(window, started_at=window.started_at.replace(tzinfo=timezone(timedelta(hours=1)))),
                 replace(window, started_at=row.started_at - timedelta(seconds=1)),
                 replace(window, completed_at=row.completed_at + timedelta(seconds=1)),
                 replace(window, started_at=window.completed_at + timedelta(seconds=1)),
                 replace(window, completed_at=fixture.T0 + timedelta(days=2))]
        for item in cases:
            with self.subTest(item=item): self.reject(replace(row, execution_window=item))

    def test_same_run_processing_start_cannot_reverse_attempt_order(self):
        first = replace(fixture.attempt(latest_attempt=2, minute=20), requested_at=None)
        second = replace(fixture.attempt(rerun=2, minute=1), requested_at=None)
        self.reject(first, second, detail="history-attempt-order")

    def test_ordered_processing_cannot_hide_reversed_fresh_execution(self):
        for first_span, second_span in (((20, 21), (10, 11)), ((10, 21), (15, 20))):
            rows = []
            for number, span in enumerate((first_span, second_span), 1):
                window = evidence.ExecutionWindow(
                    fixture.T0 + timedelta(minutes=span[0]),
                    fixture.T0 + timedelta(minutes=span[1]), True)
                row = fixture.attempt(rerun=number, latest_attempt=2, minute=number)
                rows.append(replace(row, requested_at=None, completed_at=window.completed_at,
                                    execution_window=window))
            with self.subTest(first_span=first_span, second_span=second_span):
                self.reject(*rows, detail="history-window-order")

    def test_concurrent_positive_keeps_canonical_run_number_selection(self):
        high = fixture.attempt(run=200, number=20, minute=10)
        low = replace(fixture.attempt(run=99, number=9, minute=10), requested_at=None)
        self.assertEqual(self.qualify(low, high).run_id, 200)

    def test_normal_recovery_does_not_erase_any_bypass_negative(self):
        first, second = self.rerun()
        negative = fixture.attempt(run=99, number=9, minute=5, conclusion="failure",
                                   measurement=fixture.pair(99, server_false="read"))
        history = fixture.history(first, negative, second)
        self.assertEqual(evidence.require_compat(fixture.request(), history).run_attempt, 2)
        result = evidence.assess_bypass_history(fixture.SHA, history)
        self.assertEqual(result.state, evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)
        self.assertEqual(result.negatives[0].run_id, 99)


if __name__ == "__main__":
    unittest.main()
