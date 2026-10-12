"""Synthetic verified-byte boundaries; no cryptography or rollback authority."""
from dataclasses import FrozenInstanceError, replace
import json
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat_evidence as evidence
import test_compat_evidence as fixture


def binding():
    """Distinct producer, signed records, actual image sources and DB origin."""
    return evidence.RollbackBinding(fixture.SHA, fixture.LATEST,
        evidence.ReleaseIdentity("prod-20261009T010203Z-cccccccc", "3" * 64,
                                 "c" * 40, "sha256:" + "4" * 64),
        evidence.DatabaseBase("retained-live-v2", "d" * 40,
            evidence.OriginIdentity("prod-20261008T010203Z-dddddddd", "5" * 64),
            evidence.CatalogTuple(0, 0, fixture.CATALOG, "6" * 64, "7" * 64)),
        "e" * 40, "f" * 40, evidence.RollbackSpan(4, 2, "8" * 64), "9" * 64)


_DEFAULT_BINDING = binding()


def pair(run=100, attempt=1, *, binding=None, server_false=None,
         migration_false=None, forward=True, catalog_sha256=None):
    """Reuse the normal synthetic receipt factory, with closed context4 bytes."""
    value = _DEFAULT_BINDING if binding is None else binding
    wire = evidence.rollback_binding_wire(value)
    result = {"sha": value.producer_sha, "previous_release": value.selected.release,
        "previous_web_digest": value.selected.web_digest,
        "classification": {"kind": "none", "dormant": False}, "forward": forward,
        "old_migrate_ok": {key: key != migration_false for key in evidence.MIGRATIONS},
        "old_server_ok": {key: key != server_false for key in evidence.SERVERS},
        "reversible": migration_false is None,
        "catalog_sha256": value.database_base.catalog.catalog_sha256 if catalog_sha256 is None else catalog_sha256,
        "run_id": run, "run_attempt": attempt}
    context = {key: value for key, value in wire.items() if key != "producer_sha"}
    context.update(version=4, purpose="rollback-v0", repository=evidence.REPOSITORY,
        workflow_path=evidence.WORKFLOW, workflow_sha=value.producer_sha,
        event="workflow_dispatch", ref=evidence.REF, sha=value.producer_sha,
        run_id=run, run_attempt=attempt, requested_rollback_target=value.selected.release,
        preview={"kind": "none", "dormant": False, "blocked": False},
        result_sha256=fixture.digest(fixture.canonical(result)))
    return fixture.pack(result, context, run, attempt)


def attempt(run=100, number=10, rerun=1, *, measurement=None, conclusion="success"):
    return fixture.attempt(run, number, rerun, event="workflow_dispatch",
        measurement=pair(run, rerun) if measurement is None else measurement, conclusion=conclusion)


class RollbackParserTests(unittest.TestCase):
    def parse(self, value=None, row=None):
        value = pair() if value is None else value
        return evidence.parse_authenticated_rollback_pair(value, attempt(measurement=value) if row is None else row)

    def refuse(self, value, row=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            self.parse(value, row)
        self.assertEqual(caught.exception.code, "compat-stale")
        self.assertNotIn("PRIVATE", str(caught.exception))
        return caught.exception

    def changed(self, *, context=None, result=None):
        value = pair()
        result_value, context_value = json.loads(value.result), json.loads(value.context)
        if result is not None: result(result_value)
        context_value["result_sha256"] = fixture.digest(fixture.canonical(result_value))
        if context is not None: context(context_value)
        return fixture.pack(result_value, context_value, result_value["run_id"], result_value["run_attempt"])

    def test_complete_measurement_retains_all_distinct_identities_and_exact_subjects(self):
        value = pair()
        result = self.parse(value)
        self.assertIs(type(result), evidence.BoundRollbackMeasurement)
        self.assertEqual(result.binding, binding())
        self.assertEqual(len({result.binding.producer_sha, result.binding.latest.sha,
            result.binding.selected.sha, result.binding.database_base.source_sha,
            result.binding.selected_web_source_sha, result.binding.live_web_source_sha}), 6)
        self.assertEqual(result.result_sha256, fixture.digest(value.result))
        self.assertEqual(result.context_sha256, fixture.digest(value.context))
        self.assertTrue(result.positive)
        self.assertNotIsInstance(result, evidence.QualifiedCompat)
        with self.assertRaises(FrozenInstanceError): result.forward = False
        with self.assertRaises(FrozenInstanceError): result.binding.span.records = 5

    def test_binding_wire_roundtrip_is_closed_and_does_not_alias_frozen_data(self):
        value = binding()
        wire = evidence.rollback_binding_wire(value)
        self.assertEqual(evidence.parse_rollback_binding(wire), value)
        wire["span"]["records"] = 5
        wire["database_base"]["catalog"]["catalog_sha256"] = "0" * 64
        self.assertEqual(value.span.records, 4)
        self.assertEqual(value.database_base.catalog.catalog_sha256, fixture.CATALOG)
        with self.assertRaises(evidence.CompatRefusal): evidence.validate_rollback_binding(wire)

    def test_duplicate_images_and_actual_sources_are_not_collapsed_or_refused(self):
        value = binding()
        value = replace(value, latest=replace(value.latest, web_digest=value.selected.web_digest),
                        live_web_source_sha=value.selected_web_source_sha)
        measured = self.parse(pair(binding=value))
        self.assertEqual(measured.binding.span, value.span)
        self.assertNotEqual(measured.binding.latest.release, measured.binding.selected.release)
        self.assertEqual(measured.binding.latest.web_digest, measured.binding.selected.web_digest)

    def test_every_completed_server_false_remains_a_bound_negative_even_on_failed_run(self):
        for flag in evidence.SERVERS:
            with self.subTest(flag=flag):
                value = pair(server_false=flag)
                measured = self.parse(value, attempt(measurement=value, conclusion="failure"))
                self.assertFalse(measured.positive)
                self.assertEqual(measured.servers, tuple(key != flag for key in evidence.SERVERS))
                self.assertTrue(measured.forward and measured.reversible)

    def test_completed_migration_and_forward_false_are_not_infrastructure_or_success(self):
        for flag in ("rw", "ro"):
            with self.subTest(flag=flag):
                measured = self.parse(pair(migration_false=flag))
                self.assertFalse(measured.positive or measured.reversible)
                self.assertEqual(measured.migrations, tuple(key != flag for key in evidence.MIGRATIONS))
        self.assertFalse(self.parse(pair(forward=False)).positive)
        self.refuse(pair(migration_false="negative_control"))
        self.refuse(self.changed(result=lambda r: r.update(reversible=False)))

    def test_actual_final_catalog_mismatch_is_retained_separate_from_expected_base(self):
        measured = self.parse(pair(server_false="reverse", catalog_sha256="0" * 64))
        self.assertEqual(measured.catalog_sha256, "0" * 64)
        self.assertEqual(measured.binding.database_base.catalog.catalog_sha256, fixture.CATALOG)
        self.assertFalse(measured.positive)

    def test_normal_contexts_cannot_parse_as_rollback_and_context4_cannot_qualify_normal(self):
        for version in (1, 2, 3):
            with self.subTest(version=version): self.refuse(fixture.pair(version=version, event="workflow_dispatch"))
        target = binding().selected
        self.refuse(fixture.pair(version=1, event="workflow_dispatch", selected=target, target=target.release))
        value = pair()
        with self.assertRaises(evidence.CompatRefusal): evidence.parse_authenticated_pair(value, attempt())
        with self.assertRaises(evidence.CompatRefusal): evidence.require_compat(fixture.request(), fixture.history(attempt()))

    def test_normal_historical_negative_and_current_positive_semantics_remain_available(self):
        for version in (1, 2, 3):
            with self.subTest(version=version):
                value = fixture.pair(version=version, server_false="workers")
                measured = evidence.parse_authenticated_pair(value, fixture.attempt(measurement=value))
                self.assertFalse(measured.positive)
                self.assertEqual(measured.context_version, version)
        self.assertIsInstance(evidence.require_compat(fixture.request(), fixture.history(fixture.attempt())),
                              evidence.QualifiedCompat)

    def test_only_explicit_main_dispatch_matching_all_producer_identities_is_accepted(self):
        for key, wrong in (("repository", "PRIVATE"), ("workflow_path", "PRIVATE"),
                           ("workflow_sha", "0" * 40), ("sha", "0" * 40),
                           ("event", "push"), ("ref", "refs/pull/1/merge")):
            with self.subTest(key=key): self.refuse(self.changed(context=lambda c: c.update({key: wrong})))
        for key, wrong in (("event", "push"), ("ref", "refs/heads/PRIVATE"),
                           ("repository", "PRIVATE"), ("workflow_path", "PRIVATE"),
                           ("workflow_sha", "0" * 40), ("sha", "0" * 40)):
            with self.subTest(attempt=key): self.refuse(pair(), replace(attempt(), **{key: wrong}))

    def test_run_attempt_and_result_hash_bindings_are_exact_not_boolean_aliases(self):
        for key in ("run_id", "run_attempt"):
            for wrong in (True, 0, 101, "1"):
                with self.subTest(key=key, wrong=wrong):
                    self.refuse(self.changed(context=lambda c: c.update({key: wrong})))
        self.refuse(self.changed(context=lambda c: c.update(result_sha256="0" * 64)))
        self.refuse(self.changed(result=lambda r: r.update(sha="0" * 40)))
        for row in (replace(attempt(), run_attempt=10_000), replace(attempt(), run_number=True),
                    replace(attempt(), run_id=10**16)):
            self.refuse(pair(), row)

    def test_selected_target_and_result_must_match_without_latest_substitution(self):
        for wrong in (None, "", "PRIVATE", fixture.LATEST.release):
            with self.subTest(wrong=wrong):
                self.refuse(self.changed(context=lambda c: c.update(requested_rollback_target=wrong)))
        self.refuse(self.changed(result=lambda r: r.update(previous_release=fixture.LATEST.release)))
        self.refuse(self.changed(result=lambda r: r.update(previous_web_digest="sha256:" + "0" * 64)))
        self.refuse(self.changed(context=lambda c: c["selected"].update(release=fixture.LATEST.release)))

    def test_fixed_none_preview_and_result_classification_cannot_authorize_other_classes(self):
        for wrong in ({"kind": "data", "dormant": True, "blocked": False},
                      {"kind": "none", "dormant": False, "blocked": True},
                      {"kind": "none", "dormant": 0, "blocked": False},
                      {"kind": "none", "dormant": False, "blocked": 0},
                      {"kind": "none", "dormant": False, "blocked": False, "extra": 1}):
            with self.subTest(preview=wrong): self.refuse(self.changed(context=lambda c: c.update(preview=wrong)))
        self.refuse(self.changed(result=lambda r: r.update(classification={"kind": "data", "dormant": True})))

    def test_context_purpose_version_keysets_and_absent_pending_are_closed(self):
        for key in json.loads(pair().context):
            with self.subTest(missing=key): self.refuse(self.changed(context=lambda c: c.pop(key)))
        for key in ("pending", "records", "authorization", "PRIVATE"):
            with self.subTest(extra=key): self.refuse(self.changed(context=lambda c: c.update({key: None})))
        for wrong in (True, 1, 2, 3, 5, "4"):
            with self.subTest(version=wrong): self.refuse(self.changed(context=lambda c: c.update(version=wrong)))
        self.refuse(self.changed(context=lambda c: c.update(purpose="normal")))

    def test_nested_binding_keysets_reject_missing_extra_and_non_object_values(self):
        wire = evidence.rollback_binding_wire(binding())
        paths = [(), ("latest",), ("selected",), ("database_base",),
                 ("database_base", "catalog"), ("database_base", "origin"), ("span",)]
        for path in paths:
            original = wire
            for key in path: original = original[key]
            for key in tuple(original) + ("PRIVATE",):
                with self.subTest(path=path, key=key):
                    altered = json.loads(json.dumps(wire)); target = altered
                    for part in path: target = target[part]
                    if key in target: del target[key]
                    else: target[key] = None
                    with self.assertRaises(evidence.CompatRefusal): evidence.parse_rollback_binding(altered)
        for key in wire:
            with self.subTest(nonobject=key):
                altered = dict(wire); altered[key] = None
                with self.assertRaises(evidence.CompatRefusal): evidence.parse_rollback_binding(altered)

    def test_span_requires_inclusive_multispan_counts_and_exact_hash(self):
        for key, wrong in (("version", True), ("version", 2), ("records", True),
                           ("records", 2), ("records", 502), ("records", "4"),
                           ("non_none", True), ("non_none", 1), ("non_none", 4),
                           ("sha256", "PRIVATE"), ("sha256", "a" * 63)):
            with self.subTest(key=key, wrong=wrong):
                self.refuse(self.changed(context=lambda c: c["span"].update({key: wrong})))

    def test_database_base_cannot_invent_legacy_or_future_live_tuple(self):
        base = binding().database_base
        for changed in (evidence.DatabaseBase("legacy-source-only", fixture.LATEST.sha, None, None),
                        replace(base, origin=None), replace(base, catalog=None),
                        replace(base, catalog=replace(base.catalog, catalog_version=1)),
                        replace(base, catalog=replace(base.catalog, min_reader_version=1)),
                        replace(base, catalog=replace(base.catalog, catalog_version=False)),
                        replace(base, source_sha="PRIVATE")):
            with self.subTest(base=changed):
                with self.assertRaises(evidence.CompatRefusal):
                    evidence.validate_rollback_binding(replace(binding(), database_base=changed))

    def test_source_and_manifest_hashes_are_typed_signed_facts_not_authority_inferred_here(self):
        original = binding()
        changes = (replace(original, selected_web_source_sha="0" * 40),
                   replace(original, live_web_source_sha="0" * 40),
                   replace(original, latest=replace(original.latest, manifest_sha256="0" * 64)),
                   replace(original, selected=replace(original.selected, manifest_sha256="0" * 64)),
                   replace(original, database_base=replace(original.database_base, source_sha="0" * 40)),
                   replace(original, source_material_sha256="0" * 64),
                   replace(original, span=replace(original.span, sha256="0" * 64)))
        for changed in changes:
            with self.subTest(changed=changed):
                self.assertEqual(self.parse(pair(binding=changed)).binding, changed)
                self.assertNotEqual(changed, original)
        for key in ("selected_web_source_sha", "live_web_source_sha", "source_material_sha256"):
            with self.subTest(malformed=key): self.refuse(self.changed(context=lambda c: c.update({key: "PRIVATE"})))

    def test_both_exact_signed_subjects_and_hosted_workflow_claims_are_required(self):
        value = pair()
        for field in ("result_subject", "context_subject"):
            subject = getattr(value, field)
            for key, wrong in (("subject_name", "PRIVATE"), ("subject_sha256", "0" * 64),
                               ("predicate_type", "PRIVATE"), ("issuer", "PRIVATE"),
                               ("signer_uri", "PRIVATE"), ("signer_sha", "0" * 40),
                               ("source_sha", "0" * 40), ("source_ref", "refs/pull/1/merge"),
                               ("runner", "self-hosted"), ("run_id", 99), ("run_attempt", 2),
                               ("verification_reference", "PRIVATE")):
                with self.subTest(subject=field, key=key):
                    self.refuse(replace(value, **{field: replace(subject, **{key: wrong})}))
            self.refuse(replace(value, **{field: None}))
        self.refuse(replace(value, result_subject=value.context_subject, context_subject=value.result_subject))

    def test_artifact_inventory_is_exact_complete_regular_and_same_attempt(self):
        value = pair(); artifact = value.artifact
        changed = [replace(artifact, expired=True), replace(artifact, name="PRIVATE"),
                   replace(artifact, run_id=99), replace(artifact, run_attempt=2),
                   replace(artifact, sha="0" * 40), replace(artifact, artifact_id=True),
                   replace(artifact, files=artifact.files[:1]),
                   replace(artifact, files=(artifact.files[0], artifact.files[0])),
                   replace(artifact, files=(replace(artifact.files[0], kind="symlink"), artifact.files[1])),
                   replace(artifact, files=(replace(artifact.files[0], sha256="0" * 64), artifact.files[1]))]
        for item in changed:
            with self.subTest(artifact=item): self.refuse(replace(value, artifact=item))

    def test_canonical_json_bytes_duplicate_keys_and_lf_are_not_normalized(self):
        value = pair()
        altered_contexts = (value.context.rstrip(b"\n"), value.context + b"\n",
            value.context.replace(b'"version":4', b'"version":4,"version":4'),
            value.context.replace(b'"version":4', b'"version":NaN'),
            value.context.replace(b'{', b'{ ', 1), b'\xff', b'[]\n')
        for raw in altered_contexts:
            with self.subTest(raw=raw[:24]): self.refuse(fixture.pack(value.result, raw))
        for raw in (value.result.rstrip(b"\n"), value.result + b"\n", b'{}\n'):
            self.refuse(fixture.pack(raw, value.context))

    def test_result_missing_flags_or_nonboolean_values_refuse_instead_of_filling(self):
        for group, keys in (("old_migrate_ok", evidence.MIGRATIONS), ("old_server_ok", evidence.SERVERS)):
            for key in keys:
                with self.subTest(group=group, key=key):
                    self.refuse(self.changed(result=lambda r: r[group].pop(key)))
                    self.refuse(self.changed(result=lambda r: r[group].update({key: 1})))

    def test_maximal_closed_fixed_width_context_fits_unchanged_cap_without_omission(self):
        bounded = replace(binding(), span=evidence.RollbackSpan(501, 500, "8" * 64))
        value = pair(10**16-1, 9999, binding=bounded)
        value = replace(value, artifact=replace(value.artifact, artifact_id=1))
        measured = self.parse(value, attempt(10**16-1, 10**16-1, 9999, measurement=value))
        self.assertEqual(measured.binding, bounded)
        self.assertLessEqual(len(value.context), 2006)
        self.assertLess(len(value.context), 4096)
        self.assertEqual(set(json.loads(value.context)), set(json.loads(pair().context)))
        self.refuse(fixture.pack(value.result, value.context + b" " * (4097 - len(value.context)),
                                 10**16-1, 9999), attempt(10**16-1, 10**16-1, 9999, measurement=value))
        ordinary = pair()
        self.refuse(fixture.pack(ordinary.result + b" " * (16385 - len(ordinary.result)), ordinary.context))


if __name__ == "__main__":
    unittest.main()
