"""Pure synthetic policy cases; no signatures, Git commands or deployment."""
from __future__ import annotations

import copy
from dataclasses import FrozenInstanceError, replace
import hashlib
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import record_policy as p
import release_record as records
import ship_common as common
import test_ship_support as support

ROOT = Path(__file__).resolve().parents[2]


def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()


def current(sha="1" * 40):
    return p.reviewed_v0_source(sha, tuple(p.SourceBlob(name, (ROOT/name).read_bytes()) for name in p.SOURCE_PATHS))


def future(sha, version=1, minimum=0, *, kind="expand", prior=None):
    """Synthetic policy facts, deliberately not a supported source decoder."""
    base = prior or current()
    history = base.history
    if version > history[-1].version:
        history += (p.HistoryVersion(version, minimum, kind),)
    artifacts = dict(base.artifacts)
    for name in (p.GOLDEN, p.HISTORY):
        artifacts[name] = digest(name + str(version) + str(minimum))
    catalog = records.DocumentCatalog(version, minimum, digest("catalog" + str(version)),
                                      artifacts[p.STEPS], artifacts[p.SEEDS])
    return p.SourceFacts(sha, version, catalog, tuple(sorted(artifacts.items())), history, "synthetic-policy")


class Scenario:
    def __init__(self):
        raw = records.BOOTSTRAP_PATH.read_bytes()
        self.documents = [common.loads_strict(raw)]
        boot = self.documents[0]
        self.sources = {boot["sha"]: p.legacy_reader0(boot["sha"])}
        self.ancestors = []
        self.web_sources = [boot["legacy_signer"]["source_commit"]]

    def add(self, source, *, version=2, kind="promote", target=None, catalog=None, **changes):
        previous = self.documents[-1]
        images = {name: "sha256:" + digest(source.sha+name) for name in common.COMPONENTS}
        if target is not None:
            images = target["images"]
        if kind == "restore":
            images = previous["images"]
        value = support.manifest(kind=kind, sha=source.sha, images=images, previous=previous["release"],
            deployed_at=f"2026-10-03T12:{len(self.documents):02d}:00Z",
            rolled_back_from=previous["release"] if kind == "rollback" else None)
        if version == 2:
            if catalog is None and kind != "promote" and previous["schema_version"] == 2:
                catalog = records.DocumentCatalog(*(previous[key] for key in records.DocumentCatalog._fields))
            catalog = catalog or source.catalog
            if catalog is None:
                catalog = current().catalog
            value.update(schema_version=2, db_change="none", dormant=False, reversible=True,
                         old_server_ok=True, compat_run_id=38100000001, compat="ok", staging="passed",
                         fixed_forward_from=None, **catalog._asdict())
            if kind != "promote":
                value.update(old_server_ok="unproven", compat_run_id=None, compat="bypassed", staging="bypassed")
        value.update(changes)
        self.documents.append(value)
        self.sources[source.sha] = source
        self.web_sources.append(self.documents[0]["sha"] if images["web"] == self.documents[0]["images"]["web"] else source.sha)
        if kind == "promote" and previous["sha"] != source.sha:
            pair = (previous["sha"], source.sha)
            if pair not in self.ancestors:
                self.ancestors.append(pair)
        return value

    def inputs(self):
        result = []
        for row, web in zip(self.documents, self.web_sources):
            raw = common.canonical_file_bytes(row)
            result.append(p.RecordInput(raw, hashlib.sha256(raw).hexdigest(), web))
        return tuple(result)

    def normalize(self, *, baseline=None):
        return p.normalize_chain(self.inputs(), tuple(self.sources.values()), tuple(self.ancestors), baseline=baseline)


class SourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.blobs = tuple(p.SourceBlob(name, (ROOT/name).read_bytes()) for name in p.SOURCE_PATHS)

    def test_v0_measurement_is_distinct_from_file_hash_and_seed_registry_are_actual_bytes(self):
        source = p.reviewed_v0_source("1" * 40, self.blobs)
        hashes = dict(source.artifacts)
        self.assertEqual(source.catalog.catalog_sha256, p.V0_MEASUREMENT)
        self.assertNotEqual(source.catalog.catalog_sha256, hashes[p.GOLDEN])
        self.assertEqual(source.catalog.data_steps_sha256, hashes[p.STEPS])
        self.assertEqual(source.catalog.seeds_sha256, hashes[p.SEEDS])
        self.assertEqual(source.reader_version, 0)
        with self.assertRaises(FrozenInstanceError):source.sha = "2" * 40

    def test_every_reviewed_pin_refuses_modified_or_missing_bytes(self):
        for name, _ in p.V0_PINS:
            changed = tuple(p.SourceBlob(blob.path, blob.raw+b" ") if blob.path == name else blob for blob in self.blobs)
            missing = tuple(blob for blob in self.blobs if blob.path != name)
            with self.subTest(path=name), self.assertRaisesRegex(p.PolicyRefusal, "record-source-unsupported"):
                p.reviewed_v0_source("1" * 40, changed)
            with self.assertRaises(p.PolicyRefusal):p.reviewed_v0_source("1" * 40, missing)

    def test_registry_and_seed_structure_and_order_are_validated_without_substitute_hashes(self):
        for name, invalid in ((p.STEPS, b'{}'), (p.SEEDS, b'[]')):
            changed = tuple(p.SourceBlob(blob.path, invalid) if blob.path == name else blob for blob in self.blobs)
            with self.subTest(path=name), self.assertRaisesRegex(p.PolicyRefusal, "record-source-invalid"):
                p.reviewed_v0_source("1" * 40, changed)
        for blobs in (tuple(reversed(self.blobs)), self.blobs[:-1]+(self.blobs[0],)):
            with self.assertRaises(p.PolicyRefusal):p.reviewed_v0_source("1" * 40, blobs)
        for sha in (True, "private-host.invalid", "a" * 39):
            with self.assertRaisesRegex(p.PolicyRefusal, "record-source-invalid"):
                p.reviewed_v0_source(sha, self.blobs)

    def test_legacy_reader_is_explicit_zero_with_no_fabricated_tuple(self):
        source = p.legacy_reader0("1" * 40)
        self.assertEqual(source.reader_version, 0)
        self.assertIsNone(source.catalog)
        self.assertEqual((source.artifacts, source.history), ((), ()))

    def test_synthetic_future_facts_do_not_claim_runtime_decoder_support(self):
        scenario = Scenario()
        scenario.add(future("1" * 40), db_change="expand")
        chain = scenario.normalize()
        decision = p.rollback_plan(chain, chain.entries[-1].release)
        self.assertFalse(chain.runtime_source_supported)
        self.assertFalse(decision.runtime_source_supported)
        bad = replace(scenario.sources["1" * 40], mode="reviewed-v0")
        scenario.sources[bad.sha] = bad
        with self.assertRaisesRegex(p.PolicyRefusal, "record-source-unsupported"):scenario.normalize()


class NormalizationTests(unittest.TestCase):
    def test_v1_prefix_preserves_raw_history_without_catalog_or_new_authority(self):
        scenario = Scenario()
        scenario.add(p.legacy_reader0("1" * 40), version=1)
        scenario.add(p.legacy_reader0("2" * 40), version=1)
        chain = scenario.normalize()
        self.assertEqual(tuple(entry.record for entry in chain.entries), scenario.inputs())
        for entry in chain.entries:
            self.assertEqual((entry.facts.db_change, entry.facts.dormant, entry.facts.reversible), ("none", False, True))
            self.assertIsNone(entry.live)
            self.assertIsNone(entry.facts.catalog)
        with self.assertRaisesRegex(p.PolicyRefusal, "legacy-live-unavailable"):
            p.rollback_plan(chain, chain.entries[1].release)

    def test_first_v2_forward_establishes_candidate_tuple_and_rejects_later_v1(self):
        scenario = Scenario();scenario.add(p.legacy_reader0("1" * 40), version=1)
        promoted = scenario.add(current("2" * 40))
        chain = scenario.normalize()
        live = chain.entries[-1].live
        self.assertEqual(live.origin_release, promoted["release"])
        self.assertEqual(live.origin_source.sha, promoted["sha"])
        self.assertEqual(live.catalog, current().catalog)
        scenario.add(p.legacy_reader0("3" * 40), version=1)
        with self.assertRaisesRegex(p.PolicyRefusal, "v1-after-v2"):scenario.normalize()

    def test_document_catalog_claim_requires_exact_source_contract(self):
        scenario = Scenario();scenario.add(current())
        original = copy.deepcopy(scenario.documents[-1])
        for key in records.DocumentCatalog._fields:
            scenario.documents[-1] = copy.deepcopy(original)
            scenario.documents[-1][key] = 1 if key in ("catalog_version", "min_reader_version") else "f" * 64
            if key == "min_reader_version":scenario.documents[-1]["catalog_version"] = 1
            with self.subTest(field=key), self.assertRaises(p.PolicyRefusal):scenario.normalize()

    def test_first_v2_image_only_requires_explicit_complete_latest_legacy_baseline(self):
        scenario = Scenario()
        target = scenario.add(current("1" * 40), version=1)
        previous = scenario.add(current("2" * 40), version=1)
        scenario.add(current("1" * 40), kind="rollback", target=target)
        with self.assertRaisesRegex(p.PolicyRefusal, "legacy-live-unavailable"):scenario.normalize()
        record = scenario.inputs()[-2]
        baseline = p.LegacyBaseline(previous["release"], record.manifest_sha256, previous["sha"])
        chain = scenario.normalize(baseline=baseline)
        self.assertEqual(chain.entries[-1].live.origin_release, previous["release"])
        self.assertEqual(chain.entries[-1].live.origin_source.sha, "2" * 40)
        scenario.sources["2" * 40] = p.legacy_reader0("2" * 40)
        with self.assertRaisesRegex(p.PolicyRefusal, "legacy-baseline"):scenario.normalize(baseline=baseline)

    def test_baseline_cannot_borrow_future_source_or_bind_earlier_legacy_record(self):
        scenario = Scenario();first = scenario.add(current("1" * 40), version=1)
        latest = scenario.add(current("2" * 40), version=1)
        for baseline in (p.LegacyBaseline(first["release"], scenario.inputs()[1].manifest_sha256, first["sha"]),
                         p.LegacyBaseline(latest["release"], scenario.inputs()[2].manifest_sha256, first["sha"]),
                         p.LegacyBaseline(latest["release"], "a" * 64, latest["sha"])):
            with self.subTest(baseline=baseline), self.assertRaisesRegex(p.PolicyRefusal, "legacy-baseline"):
                scenario.normalize(baseline=baseline)

    def test_rollback_and_restore_retain_live_tuple_origin_instead_of_old_image_contract(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        expanded = scenario.add(future("2" * 40), db_change="expand")
        rolled = scenario.add(current("1" * 40), kind="rollback", target=target)
        scenario.add(current("1" * 40), kind="restore")
        chain = scenario.normalize()
        for entry in chain.entries[-2:]:
            self.assertEqual(entry.live.origin_release, expanded["release"])
            self.assertEqual(entry.live.catalog.catalog_version, 1)
            self.assertEqual(entry.reader.reader_version, 0)
        self.assertEqual(chain.entries[-2].record.raw, scenario.inputs()[-2].raw)
        self.assertEqual(chain.entries[-2].release, rolled["release"])

    def test_every_inherited_live_field_is_checked(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40));scenario.add(current("1" * 40), kind="rollback", target=target)
        original = copy.deepcopy(scenario.documents[-1])
        for key in records.DocumentCatalog._fields:
            scenario.documents[-1] = copy.deepcopy(original)
            scenario.documents[-1][key] = 1 if key in ("catalog_version", "min_reader_version") else "f" * 64
            if key == "min_reader_version":scenario.documents[-1]["catalog_version"] = 1
            with self.subTest(field=key), self.assertRaisesRegex(p.PolicyRefusal, "inherited-contract"):
                scenario.normalize()

    def test_forward_after_rollback_compares_with_retained_origin(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        v1 = future("2" * 40);scenario.add(v1, db_change="expand")
        scenario.add(current("1" * 40), kind="rollback", target=target)
        scenario.add(replace(v1, sha="3" * 40))
        self.assertEqual(scenario.normalize().entries[-1].live.catalog.catalog_version, 1)
        scenario.sources["3" * 40] = current("3" * 40)
        scenario.documents[-1].update(scenario.sources["3" * 40].catalog._asdict())
        with self.assertRaisesRegex(p.PolicyRefusal, "catalog-regression"):scenario.normalize()

    def test_legacy_bootstrap_web_digest_uses_authenticated_legacy_source_not_record_sha(self):
        scenario = Scenario();source = current("1" * 40)
        row = scenario.add(source, target=scenario.documents[0])
        chain = scenario.normalize()
        self.assertNotEqual(chain.entries[-1].reader.sha, row["sha"])
        self.assertEqual(chain.entries[-1].reader.sha, scenario.documents[0]["sha"])
        scenario.web_sources[-1] = row["sha"]
        with self.assertRaisesRegex(p.PolicyRefusal, "image-source"):scenario.normalize()

    def test_new_missing_metadata_cannot_create_legacy_reader_authority(self):
        scenario = Scenario();scenario.add(current("1" * 40))
        row = scenario.add(p.legacy_reader0("2" * 40), catalog=current().catalog)
        with self.assertRaisesRegex(p.PolicyRefusal, "legacy-reader-unbound"):scenario.normalize()
        self.assertEqual(row["schema_version"], 2)

    def test_canonical_bytes_hash_link_time_ancestry_and_bootstrap_are_required(self):
        scenario = Scenario();scenario.add(current())
        inputs = scenario.inputs();sources = tuple(scenario.sources.values());ancestors = tuple(scenario.ancestors)
        wrong_raw = inputs[-1].raw+b" "
        for replacement in (replace(inputs[-1], manifest_sha256="a" * 64),
                            p.RecordInput(wrong_raw, hashlib.sha256(wrong_raw).hexdigest(), inputs[-1].web_source_sha)):
            with self.assertRaises(p.PolicyRefusal):p.normalize_chain(inputs[:-1]+(replacement,), sources, ancestors)
        with self.assertRaisesRegex(p.PolicyRefusal, "ancestry"):p.normalize_chain(inputs, sources, ())
        damaged = copy.deepcopy(scenario.documents[0]);damaged["sha"] = "f" * 40
        raw = common.canonical_file_bytes(damaged)
        with self.assertRaisesRegex(p.PolicyRefusal, "bootstrap"):
            p.normalize_chain((p.RecordInput(raw, hashlib.sha256(raw).hexdigest(), "f" * 40),)+inputs[1:], sources, ancestors)
        original = scenario.documents[-1]
        scenario.documents[-1] = dict(original, previous="prod-20261003T110000Z-11111111")
        with self.assertRaisesRegex(p.PolicyRefusal, "link"):scenario.normalize()
        scenario.documents[-1] = original
        later = scenario.add(current("2" * 40))
        later.update(release="prod-20261003T110000Z-22222222", deployed_at="2026-10-03T11:00:00Z",
                     built_at="2026-10-03T11:00:00Z")
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-link"):scenario.normalize()

    def test_history_prefix_same_version_and_reader_floor_rules(self):
        scenario = Scenario();scenario.add(current("1" * 40))
        v1 = future("2" * 40, minimum=1, kind="contract")
        scenario.add(v1, db_change="contract")
        scenario.add(replace(v1, sha="3" * 40))
        self.assertEqual(scenario.normalize().entries[-1].live.catalog.min_reader_version, 1)
        changed = replace(v1, sha="3" * 40, catalog=v1.catalog._replace(catalog_sha256="f" * 64))
        scenario.sources[changed.sha] = changed;scenario.documents[-1].update(changed.catalog._asdict())
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-version"):scenario.normalize()
        scenario.sources[changed.sha] = replace(v1, sha="3" * 40)
        scenario.documents[-1].update(v1.catalog._asdict())
        scenario.documents[2]["db_change"] = "expand"
        with self.assertRaisesRegex(p.PolicyRefusal, "reader-floor"):scenario.normalize()

    def test_historical_replay_does_not_fetch_old_artifacts_or_reconstruct_duplicate_target(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data")
        scenario.add(current("3" * 40), db_change="data", dormant=True)
        scenario.add(current("1" * 40), kind="rollback", target=target, compat="ok",
                     compat_run_id=38100000002, old_server_ok=True)
        with patch.object(records.Gh, "verify_file", side_effect=AssertionError("no network")), \
             patch.object(records, "git_output", side_effect=AssertionError("no Git")):
            chain = scenario.normalize()
        self.assertEqual(len(chain.entries), 5)
        self.assertEqual(chain.entries[-1].facts.compat_run_id, 38100000002)
        with self.assertRaisesRegex(p.PolicyRefusal, "qualification-unavailable"):
            p.rollback_plan(chain, target["release"])

    def test_history_rewrite_duplicate_records_and_missing_image_match_refuse(self):
        scenario = Scenario();scenario.add(current("1" * 40))
        expanded = future("2" * 40);scenario.add(expanded, db_change="expand")
        changed = future("3" * 40, version=2, prior=expanded)
        changed = replace(changed, history=(changed.history[0], p.HistoryVersion(1, 0, "contract"), changed.history[2]))
        scenario.add(changed, db_change="expand")
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-history"):scenario.normalize()
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40))
        scenario.documents.append(copy.deepcopy(scenario.documents[-1]));scenario.web_sources.append("2" * 40)
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-duplicate"):scenario.normalize()
        scenario.documents.pop();scenario.web_sources.pop()
        rollback = scenario.add(current("1" * 40), kind="rollback", target=target)
        rollback["images"]["meta-relay"] = "sha256:"+"0" * 64
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-link"):scenario.normalize()

    def test_source_fact_shapes_are_data_and_refuse_malformed_claims_with_fixed_codes(self):
        scenario = Scenario();scenario.add(current())
        original = scenario.sources["1" * 40]
        for changed in (replace(original, reader_version=True), replace(original, artifacts=list(original.artifacts)),
                        replace(original, history=(p.HistoryVersion(0, True, "baseline"),)),
                        replace(original, mode="verified=true private-host.invalid"),
                        replace(original, catalog=original.catalog._replace(seeds_sha256="a" * 64))):
            scenario.sources[original.sha] = changed
            with self.subTest(mode=changed.mode), self.assertRaises(p.PolicyRefusal) as caught:scenario.normalize()
            self.assertNotIn("private", str(caught.exception))
        scenario.sources[original.sha] = original
        sources = tuple(scenario.sources.values())
        with self.assertRaisesRegex(p.PolicyRefusal, "source-duplicate"):
            p.normalize_chain(scenario.inputs(), sources+(original,), tuple(scenario.ancestors))


class RollbackTests(unittest.TestCase):
    def test_zero_and_one_change_preserve_live_contract_and_honest_image_only_facts(self):
        for change in ("none", "data"):
            scenario = Scenario();target = scenario.add(current("1" * 40))
            scenario.add(current("2" * 40), db_change=change)
            chain = scenario.normalize();result = p.rollback_plan(chain, target["release"])
            self.assertEqual(result.non_none_count, int(change != "none"))
            self.assertIs(result.live, chain.entries[-1].live)
            self.assertEqual(result.image_only_facts, records.ManifestFacts(2, "none", False, True,
                None, result.live.catalog, "bypassed", None, "bypassed"))
            self.assertEqual(result.intermediates, chain.entries[2:])

    def test_duplicate_images_keep_exact_tag_span_including_dormant_changes(self):
        scenario = Scenario();first = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data", dormant=True)
        scenario.add(current("3" * 40), db_change="data")
        duplicate = scenario.add(current("1" * 40), kind="rollback", target=first)
        scenario.add(current("1" * 40), kind="restore")
        chain = scenario.normalize()
        with self.assertRaisesRegex(p.PolicyRefusal, "qualification-unavailable"):
            p.rollback_plan(chain, first["release"])
        later = p.rollback_plan(chain, duplicate["release"])
        self.assertEqual(later.non_none_count, 0)
        self.assertEqual(tuple(entry.kind for entry in later.intermediates), ("restore",))
        self.assertEqual(len(chain.entries), 6)

    def test_irreversible_intermediate_remains_barrier_after_rollback_and_restore(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        irreversible = scenario.add(current("2" * 40), db_change="data", reversible=False)
        scenario.add(current("1" * 40), kind="rollback", target=target)
        scenario.add(current("1" * 40), kind="restore")
        chain = scenario.normalize()
        with self.assertRaisesRegex(p.PolicyRefusal, "irreversible-span"):p.rollback_plan(chain, target["release"])
        result = p.rollback_plan(chain, irreversible["release"])
        self.assertEqual(result.non_none_count, 0)  # Target itself is not crossed.

    def test_reader_comes_from_old_image_source_not_inherited_live_tuple(self):
        scenario = Scenario();old = scenario.add(current("1" * 40))
        expanded = future("2" * 40);scenario.add(expanded, db_change="expand")
        old_again = scenario.add(current("1" * 40), kind="rollback", target=old)
        contracted = future("3" * 40, version=2, minimum=1, kind="contract", prior=expanded)
        scenario.add(contracted, db_change="contract")
        chain = scenario.normalize()
        self.assertEqual(chain.entries[-2].live.catalog.catalog_version, 1)
        self.assertEqual(chain.entries[-2].reader.reader_version, 0)
        with self.assertRaisesRegex(p.PolicyRefusal, "reader-too-old"):
            p.rollback_plan(chain, old_again["release"])

    def test_unknown_target_incomplete_and_manually_rewritten_chain_refuse(self):
        scenario = Scenario();scenario.add(current())
        chain = scenario.normalize()
        for target in ("latest", "prod-20261003T120200Z-99999999", None):
            with self.assertRaisesRegex(p.PolicyRefusal, "rollback-target-invalid"):p.rollback_plan(chain, target)
        altered = replace(chain.entries[-1], facts=chain.entries[-1].facts._replace(reversible=False))
        with self.assertRaisesRegex(p.PolicyRefusal, "chain-changed"):
            p.rollback_plan(replace(chain, entries=chain.entries[:-1]+(altered,)), chain.entries[-1].release)
        with self.assertRaises(p.PolicyRefusal):p.normalize_chain(scenario.inputs()[1:], tuple(scenario.sources.values()), ())

    def test_latest_target_empty_span_does_not_decide_restore_or_touch_legacy_entrypoints(self):
        scenario = Scenario();scenario.add(current())
        chain = scenario.normalize();latest = chain.entries[-1]
        result = p.rollback_plan(chain, latest.release)
        self.assertEqual(result.intermediates, ())
        self.assertFalse(hasattr(result, "action"))
        with self.assertRaisesRegex(common.ReleaseError, "policy-unavailable"):
            records.manifest_entry(scenario.documents[-1], scenario.inputs()[-1].raw)

    def test_mutating_original_document_does_not_change_returned_facts_or_raw(self):
        scenario = Scenario();scenario.add(current())
        chain = scenario.normalize();saved = chain.entries[-1].record.raw
        scenario.documents[-1]["images"]["web"] = "private-invalid"
        scenario.documents[-1]["catalog_sha256"] = "bad"
        self.assertEqual(chain.entries[-1].record.raw, saved)
        self.assertEqual(p.rollback_plan(chain, chain.entries[-1].release).live.catalog.catalog_sha256, p.V0_MEASUREMENT)


class RollbackRequirementsTests(unittest.TestCase):
    def test_multiple_dormant_changes_are_described_without_authorizing_rollback(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data", dormant=True)
        scenario.add(current("3" * 40), db_change="expand", dormant=True)
        chain = scenario.normalize()
        required = p._rollback_requirements(chain, target["release"])
        self.assertIs(type(required), p.RollbackRequirements)
        self.assertEqual(required.non_none_count, 2)
        self.assertTrue(required.runtime_source_supported)
        self.assertIs(required.live, chain.entries[-1].live)
        self.assertFalse(hasattr(required, "image_only_facts"))
        self.assertNotIsInstance(required, p.RollbackDecision)
        with self.assertRaisesRegex(p.PolicyRefusal, "rollback-qualification-unavailable"):
            p.rollback_plan(chain, target["release"])

    def test_inclusive_span_keeps_none_rollback_restore_and_duplicate_image_tags(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data", dormant=True)
        scenario.add(current("3" * 40))
        scenario.add(current("4" * 40), db_change="data", dormant=True)
        duplicate = scenario.add(current("1" * 40), kind="rollback", target=target)
        scenario.add(current("1" * 40), kind="restore")
        chain = scenario.normalize()
        early = p._rollback_requirements(chain, target["release"])
        later = p._rollback_requirements(chain, duplicate["release"])
        self.assertEqual(tuple(entry.kind for entry in early.intermediates),
                         ("promote", "promote", "promote", "rollback", "restore"))
        self.assertEqual(early.span, tuple((entry.release, entry.record.manifest_sha256)
                                         for entry in chain.entries[1:]))
        self.assertEqual((early.non_none_count, later.non_none_count), (2, 0))
        self.assertEqual(early.target.images, later.target.images)
        self.assertEqual(later.span, early.span[-2:])
        self.assertNotEqual(early.span_sha256, later.span_sha256)
        self.assertEqual(p.rollback_plan(chain, duplicate["release"]).intermediates,
                         later.intermediates)

    def test_latest_target_has_one_bound_row_but_no_crossed_changes(self):
        scenario = Scenario();latest = scenario.add(current(), db_change="data", dormant=True)
        chain = scenario.normalize()
        required = p._rollback_requirements(chain, latest["release"])
        self.assertEqual(required.intermediates, ())
        self.assertEqual(required.non_none_count, 0)
        self.assertEqual(required.span, ((latest["release"], chain.inputs[-1].manifest_sha256),))
        self.assertRegex(required.span_sha256, r"^[0-9a-f]{64}$")
        self.assertFalse(hasattr(required, "action"))

    def test_intermediate_manifest_change_binds_even_when_class_and_images_are_equal(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        middle = scenario.add(current("2" * 40))
        scenario.add(current("3" * 40))
        before = p._rollback_requirements(scenario.normalize(), target["release"])
        middle["compat_run_id"] += 1
        after = p._rollback_requirements(scenario.normalize(), target["release"])
        self.assertEqual(before.non_none_count, after.non_none_count)
        self.assertEqual(before.intermediates[0].images, after.intermediates[0].images)
        self.assertEqual(before.span[0], after.span[0])
        self.assertEqual(before.span[-1], after.span[-1])
        self.assertNotEqual(before.span[1][1], after.span[1][1])
        self.assertNotEqual(before.span_sha256, after.span_sha256)

    def test_forged_entries_omission_order_and_raw_hash_cannot_supply_span(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data", dormant=True)
        scenario.add(current("3" * 40))
        chain = scenario.normalize()
        changed = replace(chain.entries[2], facts=chain.entries[2].facts._replace(db_change="none", dormant=False))
        mutations = (
            replace(chain, entries=chain.entries[:2]+chain.entries[3:]),
            replace(chain, entries=chain.entries[:2]+tuple(reversed(chain.entries[2:]))),
            replace(chain, entries=chain.entries[:2]+(changed,)+chain.entries[3:]),
            replace(chain, inputs=chain.inputs[:2]+chain.inputs[3:]),
            replace(chain, inputs=chain.inputs[:-1]+(replace(chain.inputs[-1], manifest_sha256="a" * 64),)),
        )
        for changed_chain in mutations:
            with self.subTest(chain=changed_chain.entries), self.assertRaises(p.PolicyRefusal):
                p._rollback_requirements(changed_chain, target["release"])

    def test_missing_live_and_invalid_exact_tag_still_refuse(self):
        scenario = Scenario();target = scenario.add(p.legacy_reader0("1" * 40), version=1)
        with self.assertRaisesRegex(p.PolicyRefusal, "record-policy-legacy-live-unavailable"):
            p._rollback_requirements(scenario.normalize(), target["release"])
        scenario.add(current("2" * 40))
        chain = scenario.normalize()
        for tag in (None, "latest", "prod-20261003T120200Z-99999999"):
            with self.subTest(tag=tag), self.assertRaisesRegex(p.PolicyRefusal, "rollback-target-invalid"):
                p._rollback_requirements(chain, tag)

    def test_irreversible_check_precedes_reader_floor_and_count_hold(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        scenario.add(current("2" * 40), db_change="data", reversible=False)
        scenario.add(future("3" * 40, minimum=1, kind="contract"), db_change="contract")
        chain = scenario.normalize()
        for calculation in (p._rollback_requirements, p.rollback_plan):
            with self.subTest(calculation=calculation.__name__), self.assertRaisesRegex(
                    p.PolicyRefusal, "rollback-irreversible-span"):
                calculation(chain, target["release"])
        scenario.documents[2]["reversible"] = True
        chain = scenario.normalize()
        for calculation in (p._rollback_requirements, p.rollback_plan):
            with self.subTest(calculation=calculation.__name__), self.assertRaisesRegex(
                    p.PolicyRefusal, "rollback-reader-too-old"):
                calculation(chain, target["release"])

    def test_synthetic_future_requirements_do_not_claim_source_runtime_support(self):
        scenario = Scenario();target = scenario.add(current("1" * 40))
        expanded = future("2" * 40)
        scenario.add(expanded, db_change="expand")
        scenario.add(future("3" * 40, version=2, prior=expanded), db_change="expand")
        chain = scenario.normalize()
        required = p._rollback_requirements(chain, target["release"])
        self.assertFalse(required.runtime_source_supported)
        self.assertEqual(required.non_none_count, 2)
        self.assertEqual(required.live.catalog.catalog_version, 2)
        with self.assertRaisesRegex(p.PolicyRefusal, "rollback-qualification-unavailable"):
            p.rollback_plan(chain, target["release"])

    def test_actual_legacy_web_reader_stays_separate_from_record_and_live_origin(self):
        scenario = Scenario()
        target = scenario.add(current("1" * 40), target=scenario.documents[0])
        origin = scenario.add(current("2" * 40), db_change="data", dormant=True)
        chain = scenario.normalize()
        required = p._rollback_requirements(chain, target["release"])
        self.assertNotEqual(required.target.reader.sha, required.target.sha)
        self.assertEqual(required.target.reader.sha, scenario.documents[0]["sha"])
        self.assertEqual(required.live.origin_release, origin["release"])
        self.assertEqual(required.live.origin_source.sha, origin["sha"])
        self.assertEqual(required.span[0], (target["release"], chain.inputs[1].manifest_sha256))

    def test_preparation_data_are_frozen_and_never_accepted_as_a_decision_input(self):
        scenario = Scenario();target = scenario.add(current())
        chain = scenario.normalize()
        required = p._rollback_requirements(chain, target["release"])
        with self.assertRaises(FrozenInstanceError):required.non_none_count = 0
        with self.assertRaises(TypeError):required.span[0] = (target["release"], "a" * 64)
        with self.assertRaises(p.PolicyRefusal):p.rollback_plan(required, target["release"])
        with self.assertRaises(p.PolicyRefusal):p._rollback_requirements(required, target["release"])


if __name__ == "__main__":
    unittest.main()
