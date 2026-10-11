"""Pending-context3 and exact historical custody with synthetic verified pairs.

Signatures/source commands are fake-capture fixtures, never external authority.
Current source classification remains gated elsewhere until atomic activation.
"""
from dataclasses import replace
import copy
import json
import unittest

import compat_evidence as e
import compat_request as bridge
import pending_source as ps
import test_compat_evidence as f
import test_compat_request as rf
import test_pending_plan as plan_fixture


def raw_material(value, *, retained=False):
    """Full existing active catalog plus synthetic PR22-shaped pending metadata."""
    args = plan_fixture.fixture()
    active = {row.path: row.raw for row in value.catalog_material.candidate_blobs}
    keys = (("active_catalog", rf.source.policy.GOLDEN), ("active_globals", rf.source.policy.GLOBALS),
            ("active_history", rf.source.policy.HISTORY))
    args.update({key: active[path] for key, path in keys})
    name = ps.plan.PREFIX + "catalog-v1.json"
    target = json.loads(args["files"][name])
    target["objects"] = [*json.loads(args["active_catalog"])["objects"],
        *(row for row in target["objects"] if row["identity"] not in ("organizations", "organizations.id"))]
    globals_ = [*json.loads(args["active_globals"]),
        *(row for row in json.loads(args["files"][ps.plan.GLOBALS]) if row["name"] != "organizations")]
    args["files"][name] = ps.plan.canonical(target)
    args["files"][ps.plan.GLOBALS] = ps.plan.canonical(globals_)
    plan = json.loads(args["files"][ps.plan.PLAN])
    plan.update({key + "_file_sha256": ps.plan.digest(args[key]) for key, _ in keys})
    plan["globals_file_sha256"] = ps.plan.digest(args["files"][ps.plan.GLOBALS])
    plan["entries"][0].update(catalog_file_sha256=ps.plan.digest(args["files"][name]),
        globals_sha256=ps.plan.digest(args["files"][ps.plan.GLOBALS]))
    args["files"][ps.plan.PLAN] = ps.plan.canonical(plan)
    files = tuple(ps.PendingBlob(path, raw) for path, raw in sorted(args["files"].items()))
    return ps.PendingMaterial(value.candidate.sha, value.resolved.latest.record_sha, files, files if retained else ())


class PendingProtocolTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        args = plan_fixture.fixture()
        files = tuple(ps.PendingBlob(path, raw) for path, raw in sorted(args["files"].items()))
        cls.present = ps.replay(ps.PendingMaterial(f.SHA, f.LATEST.sha, files, ()),
            candidate_sha=f.SHA, previous_sha=f.LATEST.sha,
            **{key: args[key] for key in ("active_catalog", "active_globals", "active_history")}).binding

    def request(self, binding=None, classification=f.CLASS):
        if binding is None:
            return f.request(classification)
        paths = {"active_catalog": "catalog.json", "active_globals": "global_tables.json", "active_history": "history.json"}
        hashes = {"internal/dbcatalog/golden/" + paths[name]: digest for name, digest in binding.active_artifacts}
        req = f.request(classification, pending=binding)
        return replace(req, source=replace(req.source,
            catalog_authority=tuple(replace(row, sha256=hashes.get(row.path, row.sha256)) for row in req.source.catalog_authority)))

    def refuse(self, request, rows, code="compat-stale", *, private=False):
        with self.assertRaises(e.CompatRefusal) as caught:
            (e._require_context2_custody if private else e.require_compat)(request, f.history(*rows))
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn("PRIVATE", str(caught.exception))
        return caught.exception

    def test_current_observed_empty_preserves_none_and_dormant_data(self):
        for classification in (f.CLASS, e.Classification("data", True)):
            req = self.request(classification=classification)
            row = f.attempt(measurement=f.pair(classification=classification))
            got = e.require_compat(req, f.history(row))
            self.assertEqual(got.pending_binding, req.source.pending_binding)
            self.assertEqual(got.pending_binding.candidate_files, ())
            self.assertEqual(got.classification, classification)

    def test_present_plan_does_not_override_none_or_data_precedence(self):
        for classification in (f.CLASS, e.Classification("data", True), e.Classification("expand", True)):
            row = f.attempt(measurement=f.pair(classification=classification, pending=self.present))
            got = e.require_compat(self.request(self.present, classification), f.history(row))
            self.assertEqual(got.classification, classification)
            self.assertEqual(got.pending_binding, self.present)

    def test_expand_needs_present_binding_and_all_active_classes_refuse(self):
        for classification in (e.Classification("expand", True), e.Classification("expand", False),
                               e.Classification("contract", False), e.Classification("data", False)):
            self.refuse(f.request(classification), (f.attempt(),))
        self.refuse(self.request(self.present, e.Classification("expand", False)), (f.attempt(),))

    def test_context3_missing_null_extra_and_malformed_pending_never_parse(self):
        for mutate in (lambda c: c.pop("pending"), lambda c: c.update(pending=None),
                       lambda c: c["pending"].update(extra="PRIVATE"),
                       lambda c: c["pending"].update(format_version=True),
                       lambda c: c["pending"].update(target_version=True),
                       lambda c: c["pending"].update(candidate_digest="f" * 64)):
            pair = f.edit_pair(f.pair(), context=mutate)
            with self.assertRaises(e.CompatRefusal): e.parse_authenticated_pair(pair, f.attempt())
            self.assertEqual(e.assess_bypass_history(f.SHA, f.history(f.attempt(measurement=pair))).negatives, ())

    def test_pending_candidate_and_latest_sources_are_bound_separately(self):
        for field, value in (("candidate_sha", f.OTHER), ("previous_sha", f.SHA), ("previous_sha", "9" * 40)):
            pair = f.edit_pair(f.pair(), context=lambda c, field=field, value=value: c["pending"].update({field: value}))
            with self.assertRaises(e.CompatRefusal): e.parse_authenticated_pair(pair, f.attempt())

    def test_active_hashes_compare_with_request_authority_not_live_catalog_hash(self):
        binding = replace(f.pending_binding(), active_artifacts=(("active_catalog", "7" * 64), *f.pending_binding().active_artifacts[1:]))
        row = f.attempt(measurement=f.pair(pending=binding))
        self.assertTrue(e.parse_authenticated_pair(row.measurement, row).positive)
        self.refuse(f.request(), (row,))
        self.refuse(f.request(pending=binding), (row,))

    def test_current_request_missing_binding_is_not_observed_absence(self):
        for value in (None, {}, e.pending_wire.to_wire(f.pending_binding())):
            self.refuse(f.request(pending=value), (f.attempt(),))

    def test_historical_positives_never_satisfy_new_current_request(self):
        for version in (1, 2):
            row = f.attempt(measurement=f.pair(version=version))
            measured = e.parse_authenticated_pair(row.measurement, row)
            self.assertTrue(measured.positive)
            self.assertIsNone(measured.pending_binding)
            self.refuse(f.request(), (row,))
            self.assertEqual(e.assess_bypass_history(f.SHA, f.history(row)).negatives, ())

    def test_each_old_protocol_negative_keeps_specific_no_positive_refusal(self):
        for version in (1, 2):
            for options, code in ((dict(server_false="write"), "compat-old-server-incompatible"),
                                  (dict(migration_false="rw"), "irreversible-needs-fix-forward-mode"),
                                  (dict(forward=False), "compat-stale")):
                row = f.attempt(measurement=f.pair(version=version, **options), conclusion="failure")
                self.refuse(f.request(), (row,), code)
                self.assertEqual(e.assess_bypass_history(f.SHA, f.history(row)).state,
                                 e.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_old_context2_wrong_origin_cannot_claim_specific_current_negative(self):
        row = f.attempt(measurement=f.pair(version=2, web_source="9" * 40, server_false="read"), conclusion="failure")
        self.assertEqual(self.refuse(f.request(), (row,)).detail, "no-qualified-positive")
        self.assertEqual(e.assess_bypass_history(f.SHA, f.history(row)).state, e.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_later_legacy_positives_unknowns_and_actual_negatives_keep_policy(self):
        for version in (1, 2):
            later = f.attempt(run=101, number=11, minute=20, measurement=f.pair(101, version=version))
            self.refuse(f.request(), (f.attempt(), later))
            negative = replace(later, measurement=f.pair(101, version=version, server_false="boot"), conclusion="failure")
            self.refuse(f.request(), (f.attempt(), negative), "compat-old-server-incompatible")
            self.refuse(f.request(), (f.attempt(), replace(negative, execution_window=None)))

    def test_earlier_completed_legacy_negative_recovery_preserves_permanent_bypass_veto(self):
        for version in (1, 2):
            old = f.attempt(run=99, number=9, minute=1,
                measurement=f.pair(99, version=version, server_false="workers"), conclusion="failure")
            history = f.history(old, f.attempt())
            self.assertEqual(e.require_compat(f.request(), history).run_id, 100)
            self.assertEqual(e.assess_bypass_history(f.SHA, history).state, e.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_historical_private_qualifier_is_exact_context2_without_pending(self):
        for classification in (f.CLASS, e.Classification("data", True)):
            req = f.request(classification, pending=None)
            row = f.attempt(measurement=f.pair(version=2, classification=classification))
            got = e._require_context2_custody(req, f.history(row))
            self.assertIsNone(got.pending_binding)
            self.assertEqual(got.classification, classification)
            self.refuse(req, (row,))
        self.refuse(f.request(), (f.attempt(measurement=f.pair(version=2)),), private=True)
        for version in (1, 3):
            self.refuse(f.request(pending=None), (f.attempt(measurement=f.pair(version=version)),), private=True)
        self.refuse(f.request(e.Classification("expand", True), pending=None), (f.attempt(),), private=True)

    def test_old_wire_never_accepts_new_fields_or_implicit_upgrade(self):
        for version in (1, 2):
            old = f.pair(version=version)
            for mutate in (lambda c: c.update(pending=e.pending_wire.to_wire(f.pending_binding())),
                           lambda c: c.update(version=3)):
                with self.assertRaises(e.CompatRefusal): e.parse_authenticated_pair(f.edit_pair(old, context=mutate), f.attempt())
        for version in (1, 2):
            with self.assertRaises(e.CompatRefusal):
                e.parse_authenticated_pair(f.edit_pair(f.pair(), context=lambda c: c.update(version=version)), f.attempt())

    def test_private_historical_timing_and_actual_false_flags_are_not_bypassed(self):
        req = f.request(pending=None)
        old = f.attempt(measurement=f.pair(version=2))
        later = f.attempt(run=101, number=11, minute=20,
            measurement=f.pair(101, version=2, server_false="read"), conclusion="failure")
        self.refuse(req, (old, later), "compat-old-server-incompatible", private=True)
        self.refuse(req, (replace(old, execution_window=None),), private=True)


class PendingRequestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rf.source.ForwardResolverTests.setUpClass()
        cls.absent = rf.resolution()
        cls.absent_history = rf.collected(cls.absent)
        # This is the reviewed future classifier projection in a synthetic
        # fixture only. The real classifier's unsupported marker is not removed.
        cls.present = replace(cls.absent, pending=raw_material(cls.absent),
            classification=cls.absent.classification._replace(kind="expand", dormant=True))
        cls.present_history = rf.collected(cls.present)
        cls.legacy_history = rf.collected(cls.absent, version=2)

    def refuse(self, value, history=None, *, private=False, code="compat-stale"):
        with self.assertRaises(e.CompatRefusal) as caught:
            (bridge._qualify_context2_custody if private else bridge.qualify)(value,
                self.present_history if history is None else history)
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn("PRIVATE", str(caught.exception))

    def test_concrete_full_material_and_actual_pair_verifier_compose(self):
        result = bridge.qualify(self.present, self.present_history)
        self.assertEqual(result.pending_binding, rf.pending(self.present))
        self.assertEqual(result.classification, e.Classification("expand", True))
        self.assertEqual(result.pending_binding.previous_sha, self.present.resolved.latest.record_sha)
        self.assertEqual(result.catalog_sha256, rf.source.policy.V0_MEASUREMENT)
        self.assertNotEqual(result.pending_binding.target_version, self.present.candidate.catalog.catalog_version)
        pair = self.present_history.history.attempts[0].measurement
        self.assertLess(len(pair.context), 4096)

    def test_absence_still_requires_concrete_material_and_context3(self):
        self.assertEqual(bridge.qualify(self.absent, self.absent_history).pending_binding.candidate_files, ())
        self.refuse(replace(self.absent, pending=None), self.absent_history)
        self.refuse(self.absent, self.legacy_history)

    def test_pending_source_roles_cannot_use_database_origin_or_actual_image(self):
        value = self.present
        for sha in (value.database_base_source, value.previous_web_source_sha, value.candidate.sha):
            self.refuse(replace(value, pending=replace(value.pending, previous_sha=sha)))
        self.refuse(replace(value, pending=replace(value.pending, candidate_sha=value.resolved.latest.record_sha)))

    def test_raw_bytes_are_replayed_not_replaced_by_signed_binding_or_parsed_plan(self):
        value = self.present
        self.refuse(replace(value, pending=rf.pending(value)))
        files = list(value.pending.candidate_files)
        index = next(i for i, row in enumerate(files) if row.path == ps.plan.PLAN)
        doc = json.loads(files[index].raw); doc["steps"] = []
        files[index] = replace(files[index], raw=ps.plan.canonical(doc))
        self.refuse(replace(value, pending=replace(value.pending, candidate_files=tuple(files))))

    def test_structurally_valid_nonexpand_plan_is_not_current_source_support(self):
        for plan_kind in ("data", "contract"):
            files = list(self.present.pending.candidate_files)
            index = next(i for i, row in enumerate(files) if row.path == ps.plan.PLAN)
            document = json.loads(files[index].raw)
            document["entries"][0]["class"] = plan_kind
            if plan_kind == "contract": document["entries"][0]["min_reader_version"] = 1
            files[index] = replace(files[index], raw=ps.plan.canonical(document))
            for kind, dormant in (("none", False), ("data", True)):
                value = replace(self.present,
                    pending=replace(self.present.pending, candidate_files=tuple(files)),
                    classification=self.present.classification._replace(kind=kind, dormant=dormant))
                # Raw parser accepts these metadata classes. The consumer's
                # supported pending subset must refuse before making a request.
                self.assertEqual(rf.pending(value).target_version, 1)
                with self.assertRaises(e.CompatRefusal): bridge.build_request(value, self.absent_history)

    def test_missing_extra_removed_or_changed_pending_inventory_refuses(self):
        value = self.present; pending = value.pending
        changes = [replace(pending, candidate_files=pending.candidate_files[:2]),
                   replace(pending, candidate_files=pending.candidate_files + (ps.PendingBlob("PRIVATE", b"PRIVATE"),)),
                   replace(pending, candidate_files=(), previous_files=pending.candidate_files),
                   replace(pending, candidate_files=(replace(pending.candidate_files[0], raw=b"PRIVATE"), *pending.candidate_files[1:]))]
        for changed in changes: self.refuse(replace(value, pending=changed))

    def test_retained_plan_none_and_data_keep_original_source_class(self):
        for kind, dormant in (("none", False), ("data", True)):
            value = replace(self.present, pending=replace(self.present.pending,
                previous_files=self.present.pending.candidate_files),
                classification=self.present.classification._replace(kind=kind, dormant=dormant))
            history = rf.collected(value)
            self.assertEqual(bridge.qualify(value, history).classification, e.Classification(kind, dormant))

    def test_producer_blocker_unknown_requirements_and_active_classes_are_not_cleared(self):
        c = self.present.classification
        for changed in (c._replace(requirements=c.requirements + ("producer-class-unsupported",)),
                        c._replace(requirements=c.requirements + ("future-format",)),
                        c._replace(reasons=("PRIVATE",)), c._replace(dormant=False), c._replace(kind="contract")):
            self.refuse(replace(self.present, classification=changed))

    def test_private_original_context2_replay_needs_historical_absence(self):
        original = replace(self.absent, pending=None)
        got = bridge._qualify_context2_custody(original, self.legacy_history)
        self.assertIsNone(got.pending_binding)
        self.refuse(self.absent, self.legacy_history, private=True)
        self.refuse(original, self.absent_history, private=True)
        self.refuse(original, self.legacy_history)
        self.refuse(replace(original, classification=original.classification._replace(kind="expand", dormant=True)),
                    self.legacy_history, private=True)


if __name__ == "__main__":
    unittest.main()
