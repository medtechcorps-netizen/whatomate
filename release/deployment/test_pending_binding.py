"""Pure pending wire behavior and synthetic full-context capacity; no authority."""
from dataclasses import FrozenInstanceError, replace
from pathlib import Path
import copy
import hashlib
import json
import unittest
from unittest import mock

import pending_binding as wire
import pending_source as source
import test_pending_plan as fixture

CANDIDATE = "a" * 40
PREVIOUS = "b" * 40


def blobs(files):
    return tuple(source.PendingBlob(path, raw) for path, raw in sorted(files.items()))


def replay(args, *, retained=False, absent=False):
    files = () if absent else blobs(args["files"])
    material = source.PendingMaterial(CANDIDATE, PREVIOUS, files, files if retained else ())
    return source.replay(material, candidate_sha=CANDIDATE, previous_sha=PREVIOUS,
        **{name: args[name] for name in ("active_catalog", "active_globals", "active_history")})


def context(binding):
    """Representative future context3 only; not an activated producer fixture."""
    latest = dict(release="prod-20261011T120000Z-bbbbbbbb", manifest_sha256="c" * 64,
                  sha=PREVIOUS, web_digest="sha256:" + "d" * 64)
    return dict(version=3, repository="medtechcorps-netizen/whatomate",
        workflow_path=".github/workflows/compat.yml", workflow_sha=CANDIDATE,
        run_id=9999999999999999, run_attempt=9999, event="workflow_dispatch",
        ref="refs/heads/main", sha=CANDIDATE, latest=latest, selected=dict(latest),
        selected_web_source_sha="9" * 40, requested_rollback_target=None,
        preview=dict(kind="expand", dormant=True, blocked=False), result_sha256="1" * 64,
        database_base=dict(mode="retained-live-v2", source_sha="e" * 40,
            origin=dict(release="prod-20261010T120000Z-eeeeeeee", manifest_sha256="f" * 64),
            catalog=dict(catalog_version=0, min_reader_version=0, catalog_sha256="2" * 64,
                         data_steps_sha256="3" * 64, seeds_sha256="4" * 64)),
        pending=wire.to_wire(binding))


class BindingTests(unittest.TestCase):
    def setUp(self):
        self.args = fixture.fixture()
        self.binding = replay(self.args).binding
        self.value = wire.to_wire(self.binding)

    def refuse(self, value):
        with self.assertRaises(wire.PendingBindingRefusal) as caught:
            wire.parse_wire(value)
        self.assertTrue(str(caught.exception).startswith("pending-binding-invalid:"))
        self.assertNotIn("PRIVATE", str(caught.exception))

    def test_replayed_introduction_retained_and_absent_roundtrip(self):
        for options in ({}, {"retained": True}, {"absent": True}):
            with self.subTest(options=options):
                actual = replay(self.args, **options).binding
                encoded = source.plan.canonical(wire.to_wire(actual))
                decoded = wire.parse_wire(json.loads(encoded))
                self.assertEqual(decoded, actual)
                self.assertEqual(source.plan.canonical(wire.to_wire(decoded)), encoded)

    def test_empty_is_observed_data_and_null_is_not_an_alternative(self):
        empty = replay(self.args, absent=True).binding
        self.assertEqual(empty.candidate_digest, hashlib.sha256(b"[]\n").hexdigest())
        self.assertEqual(empty.candidate_digest, empty.previous_digest)
        self.assertEqual(wire.parse_wire(wire.to_wire(empty)).candidate_files, ())
        self.assertIsNone(empty.target_version)
        for value in (None, {}, [], False): self.refuse(value)

    def test_output_is_immutable_and_wire_arrays_are_detached(self):
        decoded = wire.parse_wire(self.value)
        self.value["candidate_files"][0][1] = "f" * 64
        self.value["active_artifacts"].clear()
        self.assertEqual(decoded, self.binding)
        with self.assertRaises(FrozenInstanceError): decoded.target_version = 2
        with self.assertRaises(TypeError): decoded.candidate_files[0][1] = "f" * 64
        wire.to_wire(decoded)["candidate_files"].clear()
        self.assertEqual(len(decoded.candidate_files), 3)

    def test_exact_top_level_fields_and_plain_dict(self):
        class Pretend(dict): pass
        for key in self.value:
            missing = copy.deepcopy(self.value); del missing[key]
            self.refuse(missing)
        self.refuse(dict(self.value, PRIVATE=True))
        self.refuse(Pretend(self.value))
        value = dict(self.value); value[1] = value.pop("format_version")
        self.refuse(value)

    def test_versions_and_source_hashes_use_exact_types(self):
        for value in (True, 1.0, "1", 0, 2, None):
            self.refuse(dict(self.value, format_version=value))
        for field in ("candidate_sha", "previous_sha"):
            for value in (True, "A" * 40, "a" * 39, "a" * 41, "PRIVATE", "a" * 40 + "\n"):
                self.refuse(dict(self.value, **{field: value}))

    def test_active_inventory_is_exact_order_and_hash_vocabulary(self):
        rows = self.value["active_artifacts"]
        for changed in (rows[::-1], rows[:2], rows + rows[:1], [rows[0]] * 3,
                        [["PRIVATE", "a" * 64], *rows[1:]],
                        [[rows[0][0], "A" * 64], *rows[1:]],
                        [[rows[0][0], True], *rows[1:]]):
            self.refuse(dict(self.value, active_artifacts=changed))

    def test_wire_containers_do_not_accept_tuple_or_object_coercions(self):
        for field in ("active_artifacts", "candidate_files", "previous_files"):
            self.refuse(dict(self.value, **{field: tuple(self.value[field])}))
        value = copy.deepcopy(self.value); value["candidate_files"][0] = tuple(value["candidate_files"][0])
        self.refuse(value)
        value = copy.deepcopy(self.value); value["active_artifacts"][0] = tuple(value["active_artifacts"][0])
        self.refuse(value)

    def test_inventory_must_be_complete_unique_and_sorted(self):
        rows = self.value["candidate_files"]
        for changed in (rows[:1], rows[:2], rows + rows[:1], rows[::-1], [rows[0]] * 3,
                        [rows[0][:2], *rows[1:]], [[*rows[0], True], *rows[1:]]):
            self.refuse(dict(self.value, candidate_files=changed))

    def test_fixed_paths_reject_traversal_extra_versions_and_leading_zero(self):
        for path in ("../PRIVATE", source.plan.PREFIX + "../catalog-v1.json",
                     source.plan.PREFIX + "catalog-v0.json", source.plan.PREFIX + "catalog-v01.json",
                     source.plan.PREFIX + "catalog-v2147483648.json", source.plan.PREFIX + "catalog-v99999999999.json",
                     source.plan.PREFIX + "PRIVATE.json", self.value["candidate_files"][0][0] + "\n"):
            value = copy.deepcopy(self.value); value["candidate_files"][0][0] = path
            self.refuse(value)
        value = copy.deepcopy(self.value)
        value["candidate_files"][1][0] = source.plan.PREFIX + "catalog-v2.json"
        value["candidate_files"].sort()
        self.refuse(value)

    def test_lengths_and_target_never_coerce_booleans_or_floats(self):
        for length in (True, False, 1.0, "1", 0, -1, source.MAX_FILE_BYTES + 1):
            value = copy.deepcopy(self.value); value["candidate_files"][0][2] = length
            self.refuse(value)
        for target in (True, 1.0, "1", None, 0, 2):
            self.refuse(dict(self.value, target_version=target))
        empty = wire.to_wire(replay(self.args, absent=True).binding)
        for target in (False, 0, 1): self.refuse(dict(empty, target_version=target))

    def test_every_declared_hash_and_inventory_digest_is_checked(self):
        for field in ("candidate_digest", "previous_digest"):
            for digest in (True, "PRIVATE", "A" * 64, "a" * 63, "a" * 65, "f" * 64):
                self.refuse(dict(self.value, **{field: digest}))
        for position in range(3):
            value = copy.deepcopy(self.value); value["candidate_files"][position][1] = "PRIVATE"
            self.refuse(value)
            value = copy.deepcopy(self.value); value["candidate_files"][position][2] += 1
            self.refuse(value)
        value = dict(self.value, candidate_digest=hashlib.sha256(
            source.plan.canonical(self.value["candidate_files"]).rstrip(b"\n")).hexdigest())
        self.refuse(value)

    def test_aggregate_cap_charges_both_inventories(self):
        value = wire.to_wire(replay(self.args, retained=True).binding)
        length = sum(row[2] for row in value["candidate_files"])
        with mock.patch.object(source, "MAX_MATERIAL_BYTES", length * 2): wire.parse_wire(value)
        with mock.patch.object(source, "MAX_MATERIAL_BYTES", length * 2 - 1): self.refuse(value)
        with mock.patch.object(source, "MAX_FILE_BYTES", max(row[2] for row in value["candidate_files"]) - 1):
            self.refuse(value)

    def test_previous_present_cannot_be_removed_or_replaced_even_with_valid_digests(self):
        value = wire.to_wire(replay(self.args, retained=True).binding)
        value["candidate_files"] = []
        value["candidate_digest"] = source.plan.digest(b"[]\n")
        value["target_version"] = None
        self.refuse(value)
        value = wire.to_wire(replay(self.args, retained=True).binding)
        value["candidate_files"][0][1] = "f" * 64
        value["candidate_digest"] = source.plan.digest(source.plan.canonical(value["candidate_files"]))
        self.refuse(value)

    def test_same_source_requires_identical_inventories(self):
        self.refuse(dict(self.value, previous_sha=CANDIDATE))
        binding = replace(replay(self.args, retained=True).binding, previous_sha=CANDIDATE)
        self.assertEqual(wire.parse_wire(wire.to_wire(binding)), binding)

    def test_binding_projection_does_not_trust_manually_assembled_dataclass_fields(self):
        for binding in (None, self.value, replace(self.binding, candidate_files=list(self.binding.candidate_files)),
                        replace(self.binding, active_artifacts=list(self.binding.active_artifacts)),
                        replace(self.binding, target_version=True), replace(self.binding, candidate_digest="f" * 64)):
            with self.assertRaises(wire.PendingBindingRefusal): wire.to_wire(binding)

    def test_validate_binds_latest_record_role_and_exact_active_authority(self):
        args = dict(candidate_sha=CANDIDATE, previous_sha=PREVIOUS, active_artifacts=self.binding.active_artifacts)
        self.assertIs(wire.validate_against(self.binding, **args), self.binding)
        for changed in (dict(candidate_sha=PREVIOUS), dict(previous_sha=CANDIDATE),
                        dict(previous_sha="e" * 40), dict(previous_sha="9" * 40),
                        dict(candidate_sha=True), dict(active_artifacts=list(self.binding.active_artifacts)),
                        dict(active_artifacts=(("active_catalog", "f" * 64), *self.binding.active_artifacts[1:]))):
            with self.assertRaises(wire.PendingBindingRefusal): wire.validate_against(self.binding, **dict(args, **changed))

    def test_structural_hashes_do_not_grant_raw_plan_semantics(self):
        files = dict(self.args["files"])
        plan = json.loads(files[source.plan.PLAN]); plan["steps"] = []
        files[source.plan.PLAN] = source.plan.canonical(plan)
        rows = tuple((p, source.plan.digest(raw), len(raw)) for p, raw in sorted(files.items()))
        binding = replace(self.binding, candidate_files=rows,
            candidate_digest=source.plan.digest(source.plan.canonical(rows)))
        self.assertEqual(wire.parse_wire(wire.to_wire(binding)), binding)
        with self.assertRaises(source.PendingSourceRefusal): replay(dict(self.args, files=files))

    def test_helpers_do_not_read_raw_files_or_invoke_replay(self):
        # Inputs are already materialized. Wire validation needs no raw parser
        # or operational dependency and cannot accidentally grant its result.
        with mock.patch("builtins.open", side_effect=AssertionError("unexpected IO")), \
             mock.patch.object(source, "replay", side_effect=AssertionError("unexpected replay")):
            decoded = wire.parse_wire(wire.to_wire(self.binding))
            wire.validate_against(decoded, candidate_sha=CANDIDATE, previous_sha=PREVIOUS,
                                  active_artifacts=self.binding.active_artifacts)


class FullContextCapacityTests(unittest.TestCase):
    measurements = {}

    @classmethod
    def setUpClass(cls):
        args = fixture.fixture()
        root = Path(__file__).resolve().parents[2] / "internal" / "dbcatalog" / "golden"
        cls.active_paths = {key: root / name for key, name in (
            ("active_catalog", "catalog.json"), ("active_globals", "global_tables.json"), ("active_history", "history.json"))}
        cls.original = {key: path.read_bytes() for key, path in cls.active_paths.items()}
        target_path = source.plan.PREFIX + "catalog-v1.json"
        target = json.loads(args["files"][target_path])
        target["objects"] = [*json.loads(cls.original["active_catalog"])["objects"],
            *(row for row in target["objects"] if row["identity"] not in ("organizations", "organizations.id"))]
        globals_ = [*json.loads(cls.original["active_globals"]),
            *(row for row in json.loads(args["files"][source.plan.GLOBALS]) if row["name"] != "organizations")]
        args.update(cls.original)
        args["files"][target_path] = source.plan.canonical(target)
        args["files"][source.plan.GLOBALS] = source.plan.canonical(globals_)
        plan = json.loads(args["files"][source.plan.PLAN])
        plan.update({key + "_file_sha256": source.plan.digest(raw) for key, raw in cls.original.items()})
        plan["globals_file_sha256"] = source.plan.digest(args["files"][source.plan.GLOBALS])
        plan["entries"][0].update(catalog_file_sha256=source.plan.digest(args["files"][target_path]),
            globals_sha256=source.plan.digest(args["files"][source.plan.GLOBALS]))
        args["files"][source.plan.PLAN] = source.plan.canonical(plan)
        cls.args = args

    def test_real_full_catalog_bundle_context_stays_below_4096(self):
        for name, options in (("introduction", {}), ("retained", {"retained": True}), ("absent", {"absent": True})):
            with self.subTest(name=name):
                projection = replay(self.args, **options)
                if projection.plan is not None:
                    self.assertGreater(len(projection.plan.entries[0].objects), 4000)
                raw = source.plan.canonical(context(projection.binding))
                self.assertLess(len(raw), 4096)
                self.assertEqual(wire.parse_wire(json.loads(raw)["pending"]), projection.binding)
                type(self).measurements[name] = dict(context_bytes=len(raw),
                    binding_bytes=len(source.plan.canonical(wire.to_wire(projection.binding))),
                    target_objects=len(projection.plan.entries[0].objects) if projection.plan else 0,
                    pending_bytes=sum(row[2] for row in projection.binding.candidate_files))
        self.assertEqual({key: path.read_bytes() for key, path in self.active_paths.items()}, self.original)


if __name__ == "__main__":
    unittest.main()
