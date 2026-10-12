from __future__ import annotations

import copy
import hashlib
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record as records
import ship_common as common
import test_ship_support as support


SOURCE = "1" * 40
PREVIOUS = "prod-20261004T120000Z-22222222"
KNOWN_CATALOG = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"


def v1(kind="promote"):
    return support.manifest(
        kind=kind, sha=SOURCE, images=support.NEW, previous=PREVIOUS,
        deployed_at="2026-10-05T12:00:00Z",
        rolled_back_from=PREVIOUS if kind == "rollback" else None,
    )


def v2(kind="promote", **changes):
    value = v1(kind)
    value.update(
        schema_version=2, db_change="none", dormant=False, reversible=True,
        old_server_ok=True, catalog_version=0, min_reader_version=0,
        catalog_sha256=KNOWN_CATALOG, data_steps_sha256="a" * 64,
        seeds_sha256="b" * 64, compat_run_id=38100000001, compat="ok",
        staging="passed", fixed_forward_from=None,
    )
    if kind != "promote":
        value.update(compat="bypassed", compat_run_id=None, old_server_ok="unproven", staging="bypassed")
    value.update(changes)
    return value


def manual_entry(value, *, db_change=False):
    # Deliberately bypass manifest_entry to exercise the public legacy guards.
    return records.Entry(
        value["release"], value["kind"], value["sha"], dict(value["images"]),
        hashlib.sha256(common.canonical_file_bytes(value)).hexdigest(),
        value["deployed_at"], value["previous"], value["rolled_back_from"],
        db_change, copy.deepcopy(value),
    )


class ManifestV2Tests(unittest.TestCase):
    def refused(self, value):
        with self.assertRaisesRegex(common.ReleaseError, "^record-invalid$"):
            records.validate_manifest(value)

    def test_all_structural_classes_and_dormancy(self):
        for kind, dormant in (("none", False), ("expand", False), ("expand", True),
                              ("data", False), ("data", True), ("contract", False)):
            with self.subTest(kind=kind, dormant=dormant):
                value = v2(db_change=kind, dormant=dormant)
                self.assertEqual(records.validate_manifest(value), value)
                self.assertEqual(records.manifest_facts(value).db_change, kind)
        for changes in ({"db_change": "none", "dormant": True},
                        {"db_change": "contract", "dormant": True}, {"db_change": "blocked"}):
            self.refused(v2(**changes))

    def test_parser_preserves_negative_claims_without_authorizing_them(self):
        value = v2(db_change="contract", reversible=False, old_server_ok=False,
                   catalog_version=3, min_reader_version=2)
        facts = records.manifest_facts(value)
        self.assertFalse(facts.reversible)
        self.assertFalse(facts.old_server_ok)
        self.assertEqual(facts.catalog.min_reader_version, 2)
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
            records.manifest_entry(value, common.canonical_file_bytes(value))

    def test_schemas_are_closed_and_disjoint(self):
        for key in records.MANIFEST_V2_KEYS:
            value = v2()
            del value[key]
            with self.subTest(missing=key):
                self.refused(value)
        for extra in ("target_release", "compat_run_attempt", "reader_version", "live_contract_origin"):
            self.refused(v2(**{extra: PREVIOUS}))
        self.refused(dict(v1(), dormant=False))
        self.refused(v2(schema_version=1))

    def test_versions_are_exact_integers(self):
        for version in (True, False, 1.0, 2.0, "1", "2", None, [], 0, 3):
            for value in (v1(), v2()):
                with self.subTest(version=version, shape=value["schema_version"]):
                    self.refused(dict(value, schema_version=version))
        for value in (None, [], (), "private-host.invalid"):
            self.refused(value)

    def test_exact_boolean_and_class_types(self):
        for key in ("dormant", "reversible", "old_server_ok"):
            for value in (0, 1, "false", None, []):
                with self.subTest(key=key, value=value):
                    self.refused(v2(**{key: value}))
        for value in (False, [], {}, "NONE", "data (dormant)"):
            self.refused(v2(db_change=value))

    def test_version_ranges_and_reader_floor(self):
        records.validate_manifest(v2(catalog_version=2_147_483_647, min_reader_version=2_147_483_647))
        for key in ("catalog_version", "min_reader_version"):
            for value in (True, -1, 0.0, "0", 2_147_483_648, None):
                with self.subTest(key=key, value=value):
                    self.refused(v2(**{key: value}))
        self.refused(v2(catalog_version=2, min_reader_version=3))

    def test_digest_fields_are_not_urls_or_source_names(self):
        for key in ("catalog_sha256", "data_steps_sha256", "seeds_sha256"):
            for value in ("A" * 64, "a" * 63, "sha256:" + "a" * 64, None, 7,
                          "https://private-host.invalid/secret"):
                with self.subTest(key=key, value=value):
                    self.refused(v2(**{key: value}))

    def test_compat_run_is_integer_or_explicitly_absent(self):
        for run in (True, False, "38100000001", 0, -1, 1.0, None, 10**15):
            self.refused(v2(compat_run_id=run))
        records.validate_manifest(v2(compat_run_id=10**15 - 1))
        self.refused(v2(compat="ok", old_server_ok="unproven"))
        for compat in ("unavailable", "OK", None, []):
            self.refused(v2(compat=compat))

    def test_forward_bypass_requires_no_change_unproven_and_no_run(self):
        value = v2(compat="bypassed", compat_run_id=None, old_server_ok="unproven", staging="bypassed")
        records.validate_manifest(value)
        self.assertIsNone(records.manifest_facts(value).old_server_ok)
        for key, bad in (("db_change", "data"), ("dormant", True), ("reversible", False),
                         ("old_server_ok", True), ("old_server_ok", False),
                         ("compat_run_id", 1), ("staging", "passed")):
            with self.subTest(key=key, value=bad):
                self.refused(dict(value, **{key: bad}))

    def test_image_only_rollback_and_restore_do_not_claim_an_experiment(self):
        for kind in ("rollback", "restore"):
            value = v2(kind)
            facts = records.manifest_facts(value)
            self.assertEqual(facts.compat, "bypassed")
            self.assertIsNone(facts.compat_run_id)
            self.assertIsNone(facts.old_server_ok)
            self.assertTrue(facts.reversible)
            for key, bad in (("db_change", "data"), ("dormant", True),
                             ("reversible", False), ("staging", "passed")):
                with self.subTest(kind=kind, key=key):
                    self.refused(dict(value, **{key: bad}))

    def test_rollback_can_record_qualified_experiment_but_restore_cannot(self):
        value = v2("rollback", compat="ok", compat_run_id=38100000001, old_server_ok=True)
        records.validate_manifest(value)
        self.refused(dict(value, old_server_ok=False))
        self.refused(v2("restore", compat="ok", compat_run_id=38100000001, old_server_ok=True))

    def test_staging_and_fix_forward_do_not_gain_new_modes(self):
        for staging in (True, None, "skipped", "Passed", []):
            self.refused(v2(staging=staging))
        for fixed in (False, "", PREVIOUS, {}, []):
            self.refused(v2(fixed_forward_from=fixed))

    def test_common_identity_rules_still_apply_to_v2(self):
        for changes in (
            {"kind": "bootstrap"}, {"release": "prod-20261005T120001Z-11111111"},
            {"sha": "2" * 40}, {"images": {"web": support.NEW["web"]}},
            {"schema_guard": "waived"}, {"previous": "latest"},
            {"built_at": "2026-10-05T13:00:00Z"}, {"reconciled": True},
            {"rolled_back_from": PREVIOUS}, {"signer": {"workflow": ".github/workflows/other.yml", "workflow_sha": SOURCE}},
            {"fingerprints": {"environment_values_sha256": "bad", "non_source_projection_sha256": "d" * 64}},
        ):
            with self.subTest(changes=changes):
                self.refused(v2(**changes))
        self.refused(v2("rollback", built_at="2026-10-05T12:00:00Z"))
        self.refused(v2("rollback", rolled_back_from="prod-20261003T120000Z-22222222"))
        self.refused(v2("restore", rolled_back_from=PREVIOUS))

    def test_manifest_size_limit_remains_enforced(self):
        with mock.patch.object(records, "MAX_MANIFEST_BYTES", 64):
            self.refused(v2())


class DocumentFactsTests(unittest.TestCase):
    def test_v1_facts_leave_unsigned_evidence_absent_and_bytes_unchanged(self):
        value = v1()
        before = common.canonical_file_bytes(value)
        facts = records.manifest_facts(value)
        self.assertEqual((facts.schema_version, facts.db_change, facts.dormant, facts.reversible),
                         (1, "none", False, True))
        self.assertEqual((facts.old_server_ok, facts.catalog, facts.compat, facts.compat_run_id, facts.staging),
                         (None, None, None, None, None))
        self.assertEqual(common.canonical_file_bytes(value), before)

    def test_v2_facts_are_deeply_immutable_and_detached(self):
        value = v2()
        facts = records.manifest_facts(value)
        with self.assertRaises(AttributeError):
            facts.reversible = False
        with self.assertRaises(AttributeError):
            facts.catalog.catalog_version = 9
        value["catalog_sha256"] = "c" * 64
        value["reversible"] = False
        self.assertTrue(facts.reversible)
        self.assertEqual(facts.catalog.catalog_sha256, KNOWN_CATALOG)
        self.assertIsInstance(hash(facts), int)

    def test_parser_does_not_invent_source_reader_or_origin_authority(self):
        # Valid wire syntax may contain an unqualified future version/hash.
        # It remains a claim, not a known source, measured catalog or reader.
        value = v2(catalog_version=17, min_reader_version=12, catalog_sha256="f" * 64)
        with mock.patch.object(records, "git", side_effect=AssertionError("unexpected source read")):
            facts = records.manifest_facts(value)
        self.assertEqual(facts.catalog.catalog_version, 17)
        self.assertEqual(facts.catalog.catalog_sha256, "f" * 64)
        self.assertFalse(hasattr(facts, "reader_version"))
        self.assertFalse(hasattr(facts, "origin_release"))

    def test_invalid_document_cannot_produce_facts(self):
        with self.assertRaisesRegex(common.ReleaseError, "^record-invalid$"):
            records.manifest_facts(v2(compat="ok", compat_run_id=None))

    def test_v1_writer_output_is_byte_identical_and_not_auto_upgraded(self):
        value = v1()
        fields = dict(value)
        fields["workflow_sha"] = SOURCE
        fields.update(fields.pop("fingerprints"))
        expected = common.canonical_file_bytes(value)
        self.assertEqual(records.build_manifest(fields), expected)
        fields.update(schema_version=2, db_change="data", reversible=False)
        self.assertEqual(records.build_manifest(fields), expected)


class LegacyExecutionBoundaryTests(unittest.TestCase):
    def bootstrap(self):
        return records.bootstrap_entry(support.bootstrap_record("b" * 40))

    def test_valid_v2_cannot_become_legacy_entry(self):
        for kind in ("promote", "rollback", "restore"):
            value = v2(kind)
            records.validate_manifest(value)
            with self.subTest(kind=kind), self.assertRaisesRegex(
                common.ReleaseError, "^record-chain-invalid:policy-unavailable$"
            ):
                records.manifest_entry(value, common.canonical_file_bytes(value))

    def test_manually_assembled_none_chain_cannot_bypass_any_execution_boundary(self):
        initial = self.bootstrap()
        forged = manual_entry(v2(), db_change=False)
        legacy = manual_entry(v1())
        # Check all positions, including a v2 target with zero intermediates.
        for entries in ((initial, forged), (forged, legacy), (initial, forged, legacy)):
            chain = records.Chain(entries)
            for target in (entries[0], entries[-1]):
                with self.subTest(entries=len(entries), target=target.release):
                    with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
                        records.rollback_plan(chain, target.release)
                    with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
                        records.decide_rollback_case(chain.latest.images, chain, target)
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
                records.require_link(entries, legacy, None)
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
            records.require_link((initial,), forged, None)
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
            records.decide_rollback_case(legacy.images, records.Chain((initial, legacy)), forged)

    def test_even_attested_v2_chain_refuses_until_policy_adapter_exists(self):
        value = v2(previous="prod-0000")
        fake = support.FakeGh()
        fake.add_record(value)
        with tempfile.TemporaryDirectory() as temp:
            client = records.Gh("/fake/gh", support.GH_TOKEN, common.REPOSITORY,
                                expected_version=support.GH_VERSION, runner=fake, config_dir=temp)
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
                records.resolve_chain(client, None, bootstrap=support.bootstrap_record("b" * 40),
                                      work_dir=Path(temp) / "chain")
        self.assertTrue(any(argv[1:3] == ["attestation", "verify"] for argv, _env in fake.calls))
        self.assertEqual(fake.writes, [])

    def test_raw_canonical_and_signature_checks_are_not_bypassed_by_v2(self):
        value = v2(previous="prod-0000")
        raw = common.canonical_file_bytes(value)
        for mode, expected in (("unsigned", "attestation"), ("noncanonical", "manifest"), ("duplicate", "manifest")):
            fake = support.FakeGh()
            changed = (raw + b" " if mode == "noncanonical" else
                       raw.replace(b'"schema_version":2', b'"schema_version":2,"schema_version":2')
                       if mode == "duplicate" else raw)
            fake.add_record(value, raw=changed, attested=mode != "unsigned")
            with tempfile.TemporaryDirectory() as temp:
                client = records.Gh("/fake/gh", support.GH_TOKEN, common.REPOSITORY,
                                    expected_version=support.GH_VERSION, runner=fake, config_dir=temp)
                with self.subTest(mode=mode), self.assertRaisesRegex(
                    common.ReleaseError, "^record-chain-invalid:" + expected + "$"
                ):
                    records.resolve_chain(client, None, bootstrap=support.bootstrap_record("b" * 40),
                                          work_dir=Path(temp) / "chain")


if __name__ == "__main__":
    unittest.main()
