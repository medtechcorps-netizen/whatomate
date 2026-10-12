"""Origin-aware pure policy with synthetic signed-byte adapter receipts.

No test here grants cryptographic authority or activates the current producer.
The prior source and retained live origin deliberately differ in these cases.
"""
from dataclasses import asdict, FrozenInstanceError, replace
import json
import unittest

import compat_evidence as e
import test_compat_evidence as f


ORIGIN = e.OriginIdentity("prod-20261011T000000Z-cccccccc", "3" * 64)
CATALOG = e.CatalogTuple(0, 0, "4" * 64, "5" * 64, "6" * 64)
BASE = e.DatabaseBase("retained-live-v2", "c" * 40, ORIGIN, CATALOG)
WEB = "9" * 40  # Actual image source is neither manifest source nor live origin.


class OriginTests(unittest.TestCase):
    def row(self, **kwargs):
        return f.attempt(measurement=f.pair(base=BASE, web_source=WEB, **kwargs))

    def req(self, **kwargs):
        return f.request(base=BASE, web_source=WEB, **kwargs)

    def refuse(self, rows, request=None, code="compat-stale"):
        with self.assertRaises(e.CompatRefusal) as caught:
            e.require_compat(request or self.req(), f.history(*rows))
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn("PRIVATE", str(caught.exception))

    def parse_refuses(self, pair):
        with self.assertRaises(e.CompatRefusal) as caught:
            e.parse_authenticated_pair(pair, f.attempt())
        self.assertNotIn("PRIVATE", str(caught.exception))

    def test_retained_origin_actual_image_and_manifest_identity_stay_distinct(self):
        row = self.row()
        result = e.require_compat(self.req(), f.history(row))
        self.assertEqual(result.previous, f.LATEST)
        self.assertEqual(result.database_base, BASE)
        self.assertEqual(result.previous_web_source_sha, WEB)
        self.assertEqual(result.candidate_sha, f.SHA)
        self.assertEqual(result.result_sha256, f.digest(row.measurement.result))
        self.assertEqual(result.catalog_sha256, f.CATALOG)  # Actual after, not the retained tuple's hash.
        for obj, field in ((result, "database_base"), (BASE, "source_sha"), (ORIGIN, "release"),
                           (CATALOG, "catalog_version")):
            with self.assertRaises(FrozenInstanceError):
                setattr(obj, field, None)

    def test_same_as_origin_candidate_uses_explicit_plan_without_image_inference(self):
        base = replace(BASE, source_sha=f.SHA)
        row = f.attempt(measurement=f.pair(base=base, web_source=WEB))
        result = e.require_compat(f.request(base=base, web_source=WEB), f.history(row))
        self.assertEqual(result.classification, f.CLASS)
        self.assertNotEqual(result.database_base.source_sha, result.previous_web_source_sha)
        self.assertNotEqual(result.previous.sha, result.previous_web_source_sha)
        # A purported image-derived plan is not silently substituted.
        self.refuse((row,), f.request(base=replace(base, source_sha=WEB), web_source=WEB))

    def test_every_retained_origin_tuple_field_is_compared_to_trusted_request(self):
        mutations = [replace(BASE, source_sha="d" * 40),
                     replace(BASE, origin=replace(ORIGIN, release="prod-20261011T000001Z-cccccccc")),
                     replace(BASE, origin=replace(ORIGIN, manifest_sha256="7" * 64))]
        for name, value in (("catalog_version", 1), ("catalog_sha256", "7" * 64),
                            ("data_steps_sha256", "7" * 64), ("seeds_sha256", "7" * 64)):
            mutations.append(replace(BASE, catalog=replace(CATALOG, **{name: value})))
        # Independently change a valid reader floor rather than failing shape.
        one = replace(BASE, catalog=replace(CATALOG, catalog_version=1))
        row = f.attempt(measurement=f.pair(base=one, web_source=WEB))
        self.refuse((row,), f.request(base=replace(one, catalog=replace(one.catalog, min_reader_version=1)), web_source=WEB))
        for base in mutations:
            with self.subTest(base=base): self.refuse((self.row(),), f.request(base=base, web_source=WEB))

    def test_actual_web_source_cannot_be_replaced_with_record_or_origin_source(self):
        for source in (f.LATEST.sha, BASE.source_sha, "d" * 40):
            with self.subTest(source=source): self.refuse((self.row(),), f.request(base=BASE, web_source=source))
        changed = f.edit_pair(self.row().measurement, context=lambda c: c.update(selected_web_source_sha=f.LATEST.sha))
        self.refuse((f.attempt(measurement=changed),))

    def test_legacy_source_only_remains_null_live_not_a_baseline_adapter(self):
        row = f.attempt(measurement=f.pair(web_source=WEB))
        result = e.require_compat(f.request(web_source=WEB), f.history(row))
        self.assertEqual(result.database_base.source_sha, f.LATEST.sha)
        self.assertIsNone(result.database_base.origin)
        self.assertIsNone(result.database_base.catalog)
        self.refuse((row,))  # A retained-origin request cannot use that evidence.

    def test_missing_or_invalid_request_provenance_refuses(self):
        req = self.req()
        for field, value in (("database_base", None), ("database_base", asdict(BASE)),
                             ("previous_web_source_sha", None), ("previous_web_source_sha", "PRIVATE")):
            with self.subTest(field=field, value=value):
                self.refuse((self.row(),), replace(req, source=replace(req.source, **{field: value})))

    def test_closed_context_nested_shapes_and_types_refuse(self):
        original = self.row().measurement
        mutations = [lambda c: c.pop("database_base"), lambda c: c.pop("selected_web_source_sha"),
                     lambda c: c.update(database_base=None), lambda c: c.update(selected_web_source_sha=True),
                     lambda c: c.update(selected_web_source_sha="PRIVATE"),
                     lambda c: c["database_base"].update(extra="PRIVATE"),
                     lambda c: c["database_base"].pop("origin"),
                     lambda c: c["database_base"].update(source_sha="PRIVATE"),
                     lambda c: c["database_base"].update(mode="future"),
                     lambda c: c["database_base"].update(origin=None),
                     lambda c: c["database_base"].update(catalog=None),
                     lambda c: c["database_base"]["origin"].update(extra="PRIVATE"),
                     lambda c: c["database_base"]["origin"].update(release="prod-0000"),
                     lambda c: c["database_base"]["origin"].update(manifest_sha256="PRIVATE"),
                     lambda c: c["database_base"]["catalog"].update(extra="PRIVATE"),
                     lambda c: c["database_base"]["catalog"].pop("seeds_sha256")]
        for field in ("catalog_version", "min_reader_version"):
            for value in (True, -1, 2**31, 0.0, "0"):
                mutations.append(lambda c, field=field, value=value: c["database_base"]["catalog"].update({field:value}))
        mutations.append(lambda c: c["database_base"]["catalog"].update(min_reader_version=1))
        for field in ("catalog_sha256", "data_steps_sha256", "seeds_sha256"):
            mutations.append(lambda c, field=field: c["database_base"]["catalog"].update({field:"PRIVATE"}))
        for index, change in enumerate(mutations):
            with self.subTest(index=index): self.parse_refuses(f.edit_pair(original, context=change))

    def test_legacy_mode_cannot_carry_or_infer_origin_tuple(self):
        original = f.pair()
        for change in (lambda c: c["database_base"].update(origin=asdict(ORIGIN)),
                       lambda c: c["database_base"].update(catalog=asdict(CATALOG)),
                       lambda c: c["database_base"].update(source_sha=WEB)):
            self.parse_refuses(f.edit_pair(original, context=change))

    def test_context1_positive_is_measured_but_never_new_normal_qualification(self):
        row = f.attempt(measurement=f.pair(version=1))
        measured = e.parse_authenticated_pair(row.measurement, row)
        self.assertTrue(measured.positive)
        self.assertEqual(measured.context_version, 1)
        self.assertIsNone(measured.database_base)
        self.assertIsNone(measured.selected_web_source_sha)
        self.refuse((row,), f.request())
        self.assertEqual(e.assess_bypass_history(f.SHA, f.history(row)).negatives, ())

    def test_context1_each_actual_negative_keeps_hard_refusal_and_bypass_veto(self):
        cases = [(dict(server_false=name), "compat-old-server-incompatible") for name in e.SERVERS]
        cases += [(dict(migration_false=name), "irreversible-needs-fix-forward-mode") for name in ("rw", "ro")]
        cases += [(dict(forward=False), "compat-stale")]
        for options, code in cases:
            with self.subTest(options=options):
                row = f.attempt(measurement=f.pair(version=1, **options), conclusion="failure")
                self.refuse((row,), f.request(), code)
                decision = e.assess_bypass_history(f.SHA, f.history(row))
                self.assertEqual(decision.state, e.BypassState.VETO_AUTHENTICATED_NEGATIVE)
                self.assertEqual(decision.negatives[0].result_sha256, f.digest(row.measurement.result))

    def test_context1_nested_new_fields_and_fake_upgrade_refuse(self):
        legacy = f.pair(version=1)
        for change in (lambda c: c.update(database_base=asdict(BASE)),
                       lambda c: c.update(selected_web_source_sha=WEB), lambda c: c.update(version=2)):
            self.parse_refuses(f.edit_pair(legacy, context=change))
        self.parse_refuses(f.edit_pair(self.row().measurement, context=lambda c: c.update(version=1)))

    def test_later_legacy_positive_is_incomplete_not_fake_negative(self):
        old_protocol = f.attempt(run=101, number=11, minute=20, measurement=f.pair(101, version=1))
        self.refuse((self.row(), old_protocol))
        self.assertEqual(e.assess_bypass_history(f.SHA, f.history(self.row(), old_protocol)).negatives, ())

    def test_later_or_unknown_context1_negative_still_vetoes(self):
        old_protocol = f.attempt(run=101, number=11, minute=20,
            measurement=f.pair(101, version=1, server_false="workers"), conclusion="failure")
        self.refuse((self.row(), old_protocol), code="compat-old-server-incompatible")
        self.refuse((self.row(), replace(old_protocol, execution_window=None)))

    def test_earlier_context1_failure_recovery_never_erases_bypass_veto(self):
        old_protocol = f.attempt(run=99, number=9, minute=1,
            measurement=f.pair(99, version=1, server_false="workers"), conclusion="failure")
        rows = f.history(old_protocol, self.row())
        self.assertEqual(e.require_compat(self.req(), rows).run_id, 100)
        self.assertEqual(e.assess_bypass_history(f.SHA, rows).state, e.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_origin_mismatched_later_positive_still_blocks(self):
        later = f.attempt(run=101, number=11, minute=20,
            measurement=f.pair(101, base=replace(BASE, source_sha="d" * 40), web_source=WEB))
        self.refuse((self.row(), later))

    def test_context3_does_not_introduce_rollback_or_new_result_sha_semantics(self):
        target = replace(f.LATEST, release="prod-20261009T000000Z-bbbbbbbb")
        pair = f.pair(event="workflow_dispatch", target=target.release, selected=target, base=BASE, web_source=WEB)
        row = f.attempt(event="workflow_dispatch", measurement=pair)
        with self.assertRaises(e.CompatRefusal): e.parse_authenticated_pair(pair, row)
        self.parse_refuses(f.edit_pair(self.row().measurement, result=lambda r: r.update(sha=BASE.source_sha)))

    def test_wire_and_trusted_data_are_not_cryptographic_capabilities(self):
        original = self.row().measurement
        changed = f.edit_pair(original, context=lambda c: c["database_base"].update(source_sha="d" * 40))
        self.parse_refuses(replace(changed, context_subject=original.context_subject))
        # Even a separately authenticated different claim cannot match the
        # trusted operation's source facts merely by retaining all true flags.
        self.refuse((f.attempt(measurement=changed),))

    def test_legacy_explicit_target_negative_remains_audit_only(self):
        target = replace(f.LATEST, release="prod-0000")
        pair = f.pair(event="workflow_dispatch", target=target.release, selected=target,
                      version=1, server_false="read")
        row = f.attempt(event="workflow_dispatch", measurement=pair, conclusion="failure")
        self.assertEqual(e.parse_authenticated_pair(pair, row).requested_target, "prod-0000")
        self.assertEqual(e.assess_bypass_history(f.SHA, f.history(row)).state,
                         e.BypassState.VETO_AUTHENTICATED_NEGATIVE)


if __name__ == "__main__":
    unittest.main()
