"""Synthetic byte custody/binding checks; no source, SQL or provider access."""
from dataclasses import FrozenInstanceError, replace
import json
import unittest
from unittest import mock

import pending_source as source
import test_pending_plan as fixture

CANDIDATE = "a" * 40
PREVIOUS = "b" * 40


def blobs(files):
    return tuple(source.PendingBlob(path, raw) for path, raw in sorted(files.items()))


class PendingSourceTests(unittest.TestCase):
    def setUp(self):
        self.args = fixture.fixture()
        self.active = {key: self.args[key] for key in ("active_catalog", "active_globals", "active_history")}
        self.files = blobs(self.args["files"])
        self.material = source.PendingMaterial(CANDIDATE, PREVIOUS, self.files, ())

    def replay(self, material=None, **changes):
        args = dict(candidate_sha=CANDIDATE, previous_sha=PREVIOUS, **self.active)
        args.update(changes)
        return source.replay(self.material if material is None else material, **args)

    def refuse(self, material, code=None, **changes):
        with self.assertRaises(source.PendingSourceRefusal) as caught:
            self.replay(material, **changes)
        self.assertNotIn("PRIVATE", str(caught.exception))
        if code: self.assertEqual(str(caught.exception), "pending-source-invalid:" + code)

    def test_new_bundle_binds_all_source_files_and_keeps_active_separate(self):
        value = self.replay()
        self.assertEqual(value.plan, source.plan.parse_bundle(**self.args))
        self.assertEqual(value.binding.target_version, 1)
        self.assertEqual((value.binding.candidate_sha, value.binding.previous_sha), (CANDIDATE, PREVIOUS))
        self.assertEqual(value.binding.candidate_files,
            tuple((row.path, source.plan.digest(row.raw), len(row.raw)) for row in self.files))
        self.assertEqual(dict(value.binding.active_artifacts),
            {key: source.plan.digest(raw) for key, raw in self.active.items()})
        self.assertNotEqual(value.binding.candidate_digest, value.binding.previous_digest)
        self.assertEqual(value.binding.previous_files, ())
        with self.assertRaises(FrozenInstanceError): value.binding.target_version = 2

    def test_complete_empty_directories_differ_from_missing_historical_material(self):
        value = self.replay(replace(self.material, candidate_files=()))
        self.assertIsNone(value.plan)
        self.assertIsNone(value.binding.target_version)
        self.assertEqual(value.binding.candidate_digest, value.binding.previous_digest)
        for material in (None, {}, object()):
            with self.assertRaises(source.PendingSourceRefusal):
                source.replay(material, candidate_sha=CANDIDATE, previous_sha=PREVIOUS, **self.active)

    def test_identical_existing_bundle_has_equal_digests_and_exact_plan(self):
        value = self.replay(replace(self.material, previous_files=self.files))
        self.assertEqual(value.binding.candidate_digest, value.binding.previous_digest)
        self.assertEqual(value.binding.candidate_files, value.binding.previous_files)
        self.assertEqual(value.plan, self.replay().plan)

    def test_existing_bundle_cannot_be_removed_or_replaced(self):
        material = replace(self.material, previous_files=self.files)
        self.refuse(replace(material, candidate_files=()), "removed")
        files = dict(self.args["files"])
        plan = json.loads(files[source.plan.PLAN]); plan["steps"][0]["source_sha256"] = "f" * 64
        files[source.plan.PLAN] = source.plan.canonical(plan)
        # Both independently parse, but immutable prior continuity must refuse.
        source.plan.parse_bundle(**dict(self.args, files=files))
        self.refuse(replace(material, candidate_files=blobs(files)), "bundle")

    def test_source_sha_roles_cannot_be_interchanged_or_fabricated(self):
        for material in (replace(self.material, candidate_sha=PREVIOUS),
                         replace(self.material, previous_sha=CANDIDATE),
                         replace(self.material, candidate_sha="PRIVATE")):
            self.refuse(material, "source")
        self.refuse(self.material, "source", candidate_sha=True)
        self.refuse(self.material, "source", previous_sha="short")

    def test_same_revision_requires_identical_observed_directories(self):
        material = replace(self.material, previous_sha=CANDIDATE)
        self.refuse(material, "same-source", previous_sha=CANDIDATE)
        value = self.replay(replace(material, previous_files=self.files), previous_sha=CANDIDATE)
        self.assertEqual(value.binding.candidate_digest, value.binding.previous_digest)

    def test_no_raw_file_is_optional_duplicate_unsorted_or_extra(self):
        for files in (self.files[:2], self.files + self.files[:1], self.files[::-1],
                      (self.files[0],) * 3, list(self.files),
                      (replace(self.files[0], path=source.plan.PREFIX + "extra.json"), *self.files[1:])):
            self.refuse(replace(self.material, candidate_files=files))

    def test_unsafe_paths_or_non_bytes_refuse_before_parser(self):
        for changes in ({"path": "../PRIVATE"}, {"path": source.plan.PREFIX + "catalog-v0.json"},
                        {"raw": "PRIVATE"}, {"raw": b""}, {"raw": bytearray(b"PRIVATE")}):
            self.refuse(replace(self.material, candidate_files=(replace(self.files[0], **changes), *self.files[1:])))

    def test_malformed_or_noncanonical_pending_bytes_refuse(self):
        for raw in (b"PRIVATE", self.files[0].raw + b" "):
            self.refuse(replace(self.material,
                candidate_files=(replace(self.files[0], raw=raw), *self.files[1:])), "bundle")

    def test_active_binding_changes_refuse_and_never_use_pending_as_live(self):
        for name, raw in self.active.items():
            self.refuse(self.material, "bundle", **{name: raw + b"\n"})
        self.refuse(self.material, "bundle", active_catalog=self.files[0].raw)

    def test_file_and_aggregate_caps_include_repeated_occurrences(self):
        length = max(len(row.raw) for row in self.files)
        with mock.patch.object(source, "MAX_FILE_BYTES", length - 1): self.refuse(self.material)
        total = sum(map(len, self.active.values())) + sum(len(row.raw) for row in self.files)
        with mock.patch.object(source, "MAX_MATERIAL_BYTES", total): self.replay()
        with mock.patch.object(source, "MAX_MATERIAL_BYTES", total - 1): self.refuse(self.material, "bound")
        with mock.patch.object(source, "MAX_MATERIAL_BYTES", total):
            self.refuse(replace(self.material, previous_files=self.files), "bound")

    def test_empty_pending_still_requires_bounded_explicit_active_bytes(self):
        material = replace(self.material, candidate_files=())
        for raw in (None, b"", "PRIVATE"):
            self.refuse(material, "bound", active_catalog=raw)


if __name__ == "__main__":
    unittest.main()
