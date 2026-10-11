"""Synthetic date-boundary policy checks; no real drill, clock or provider I/O."""
from dataclasses import FrozenInstanceError
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import unittest

import forward_gates as gates


NOW = datetime(2026, 10, 11, 8, 0, 0, tzinfo=timezone.utc)


def stamp(value):
    return value.strftime("%Y-%m-%dT%H:%M:%SZ")


def raw(first=NOW, second=NOW, **extra):
    value = {"version": 1, "staging_restore": stamp(first) if first is not None else None,
             "production_fork": stamp(second) if second is not None else None}
    value.update(extra)
    return json.dumps(value).encode()


def check(data=None, **changes):
    args = dict(direction="forward", kind="expand", dormant=False, now=NOW)
    args.update(changes)
    return gates.require_restore_drills(data, **args)


class RestoreDrillTests(unittest.TestCase):
    def test_all_active_forward_classes_need_both_dates(self):
        for kind in ("expand", "data", "contract"):
            with self.subTest(kind=kind):
                result = check(raw(), kind=kind)
                self.assertTrue(result.required)
                self.assertEqual((result.staging_restore, result.production_fork), (NOW, NOW))
                self.assertEqual(result.source_sha256, hashlib.sha256(raw()).hexdigest())

    def test_exact_ninety_days_is_current(self):
        old = NOW - timedelta(days=90)
        self.assertEqual(check(raw(old, old)).staging_restore, old)

    def test_one_second_expired_either_drill_refuses(self):
        old = NOW - timedelta(days=90, seconds=1)
        for first, second in ((old, NOW), (NOW, old)):
            with self.subTest(first=first, second=second):
                with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-stale"):
                    check(raw(first, second))

    def test_future_date_either_drill_refuses(self):
        future = NOW + timedelta(seconds=1)
        for first, second in ((future, NOW), (NOW, future)):
            with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-future"):
                check(raw(first, second))

    def test_missing_file_and_partial_inventory_refuse(self):
        for data in (None, raw(None, NOW), raw(NOW, None), raw(None, None)):
            with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-missing"):
                check(data)

    def test_committed_initial_record_does_not_claim_completed_drills(self):
        data = Path(__file__).with_name("drills.json").read_bytes()
        self.assertEqual(json.loads(data), {"version": 1, "staging_restore": None, "production_fork": None})
        with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-missing"):
            check(data)

    def test_none_and_dormant_forward_do_not_require_restore_drills(self):
        for kind, dormant in (("none", False), ("expand", True), ("data", True)):
            result = check(kind=kind, dormant=dormant)
            self.assertFalse(result.required)
            self.assertIsNone(result.source_sha256)

    def test_rollback_and_restore_do_not_select_forward_gate(self):
        for direction in ("rollback", "restore"):
            for kind in ("none", "expand", "data", "contract"):
                self.assertFalse(check(direction=direction, kind=kind).required)

    def test_exempt_path_does_not_parse_unneeded_record(self):
        self.assertFalse(check(b"private-invalid", direction="rollback").required)
        self.assertFalse(check(b"private-invalid", kind="none").required)

    def test_clock_and_class_inputs_are_closed(self):
        for changes in ({"now": NOW.replace(tzinfo=None)}, {"now": NOW.astimezone(timezone(timedelta(hours=8)))},
                        {"now": "2026-10-11"}, {"direction": "stage"}, {"direction": True},
                        {"kind": "blocked"}, {"dormant": 1}, {"kind": "none", "dormant": True},
                        {"kind": "contract", "dormant": True}):
            with self.subTest(changes=changes):
                with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-input-invalid"):
                    check(raw(), **changes)

    def test_strict_version_and_inventory(self):
        for data in (raw(version=True), raw(version=1.0), raw(version=2), raw(extra="private"),
                     b'{"version":1,"staging_restore":null}', b'[]', b'null'):
            with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-invalid"):
                check(data)

    def test_invalid_timestamp_and_public_error(self):
        for value in ("2026-02-30T00:00:00Z", "2026-10-11", "2026-10-11T08:00:00+00:00",
                      "2026-10-11T08:00:00.000Z", "private token", True, 1):
            data = json.loads(raw())
            data["staging_restore"] = value
            with self.assertRaises(gates.DrillRefusal) as caught:
                check(json.dumps(data).encode())
            self.assertEqual(str(caught.exception), "restore-drills-invalid")

    def test_duplicate_json_keys_and_byte_bounds(self):
        for data in (b'{"version":1,"version":1}', b'\xff', b'x' * 4097, b'', "not bytes"):
            with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-invalid"):
                check(data)

    def test_actual_clock_not_truncated_at_expiry(self):
        old = NOW - timedelta(days=90)
        with self.assertRaisesRegex(gates.DrillRefusal, "restore-drills-stale"):
            check(raw(old, old), now=NOW + timedelta(microseconds=1))

    def test_receipt_immutable_and_does_not_grant_other_gates(self):
        result = check(raw())
        with self.assertRaises(FrozenInstanceError):
            result.required = False
        self.assertFalse(hasattr(result, "allow_deploy"))


class SourceObservationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import test_compat_request as concrete
        concrete.source.ForwardResolverTests.setUpClass()
        cls.resolution = concrete.resolution()

    def policy_projection(self, data, kind='expand', dormant=False):
        # Direct-rule policy cases only: no claim that the operational bridge
        # admits active changes or that a synthetic timestamp proves a drill.
        from dataclasses import replace
        value = self.resolution
        return replace(value, audit=replace(value.audit,
            drills=replace(value.audit.drills, raw=data)),
            classification=value.classification._replace(kind=kind, dormant=dormant))

    def test_actual_resolver_audit_exempt_with_null_or_missing_or_invalid_dates(self):
        for raw_bytes in (None, raw(None, None), b'not-json'):
            for kind, dormant in (('none', False), ('data', True)):
                result = gates.observe_forward_drills(self.policy_projection(raw_bytes, kind, dormant), now=NOW)
                self.assertEqual(result, gates.DrillGate(False, NOW, None, None, None))

    def test_full_audit_partition_and_exact_candidate_still_required_when_exempt(self):
        from dataclasses import replace
        value = self.resolution
        for audit in (None, replace(value.audit, candidate_sha='f' * 40),
                      replace(value.audit, changes=value.audit.changes[:-1]),
                      replace(value.audit, drills=replace(value.audit.drills, path='PRIVATE'))):
            with self.assertRaisesRegex(gates.DrillRefusal, '^restore-drills-input-invalid$'):
                gates.observe_forward_drills(replace(value, audit=audit), now=NOW)

    def test_direct_policy_source_hash_and_original_time_replay(self):
        data = raw(NOW - timedelta(days=90), NOW)
        value = self.policy_projection(data)
        observation = gates.observe_forward_drills(value, now=NOW)
        self.assertEqual(observation.source_sha256, hashlib.sha256(data).hexdigest())
        self.assertEqual(gates.validate_preserved_drills(value, observation), observation)
        with self.assertRaisesRegex(gates.DrillRefusal, 'restore-drills-stale'):
            gates.observe_forward_drills(value, now=NOW + timedelta(microseconds=1))
        # No retry clock is accepted: the stored observation is still valid.
        self.assertEqual(gates.validate_preserved_drills(value, observation), observation)

    def test_receipt_mutations_and_source_date_change_refuse(self):
        from dataclasses import replace
        value = self.policy_projection(raw())
        original = gates.observe_forward_drills(value, now=NOW)
        for changed in (replace(original, required=1), replace(original, required=False),
                        replace(original, source_sha256='f' * 64),
                        replace(original, staging_restore=None),
                        replace(original, production_fork=NOW - timedelta(seconds=1)),
                        replace(original, observed_at=NOW.replace(tzinfo=None))):
            with self.assertRaises(gates.DrillRefusal): gates.validate_preserved_drills(value, changed)
        with self.assertRaisesRegex(gates.DrillRefusal, 'restore-drills-observation-invalid'):
            gates.validate_preserved_drills(self.policy_projection(raw(NOW - timedelta(seconds=1), NOW)), original)

    def test_direct_active_policy_keeps_closed_age_and_missing_rules(self):
        for data, code in ((None, 'missing'), (raw(None, NOW), 'missing'), (b'PRIVATE', 'invalid'),
                           (raw(NOW - timedelta(days=91), NOW), 'stale'),
                           (raw(NOW + timedelta(seconds=1), NOW), 'future')):
            with self.assertRaisesRegex(gates.DrillRefusal, 'restore-drills-' + code):
                gates.observe_forward_drills(self.policy_projection(data), now=NOW)


if __name__ == "__main__":
    unittest.main()
