"""Private source preparation through real parsers and controlled transports.

No Git/HTTP subprocess, crypto authority, observation fixture or database is
used. The small declared PG17 row blueprint is the independently reviewed
source template. Full expectations are derived before source collection.
"""
import copy
from dataclasses import FrozenInstanceError
import hashlib
import json
from pathlib import Path
import unittest
from unittest import mock

import nullable_profile as nullable
import record_policy as policy
import record_resolver as resolver
import record_transport as rt
import release_record as records
import ship_common as common
import test_record_resolver as source
import test_ship_support as support


ROOT = Path(__file__).resolve().parents[2]
PREFIX = "internal/dbcatalog/profiles/nullable-column-v1/"
BEFORE = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"
AFTER = "a8f15ea159883aacdeecd3c55100f3b589f10ca18708ba113a393805cbf9948e"
DESCRIPTOR = "0d20f14c144d0230d4afb2b98b9bf106bc08577c69160af41dc014d8ebafa946"
OPERATION = "c775d31243e85e933598b06d3828cc329460902b27d1f0f1a9b2b62a022424d2"
SLOTS = ("before", "candidate_baseline", "candidate_probe", "descriptor", "operation", "after")


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def wire(value, *, sort=True):
    text = json.dumps(value, ensure_ascii=False, sort_keys=sort,
                      separators=(",", ":"), allow_nan=False)
    for char, escaped in (("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(char, escaped)
    return (text + "\n").encode()


def catalog_bytes(value):
    rows = []
    for item in sorted(value["objects"], key=lambda row: (row["kind"], row["identity"])):
        row = {"kind": item["kind"], "identity": item["identity"]}
        if item.get("parent"):
            row["parent"] = item["parent"]
        row["attributes"] = dict(sorted(item["attributes"].items()))
        if item.get("missing"):
            row["missing"] = item["missing"]
        if item != row:
            raise AssertionError("unexpected independent source object")
        rows.append(row)
    return wire({"version": 0, "objects": rows}, sort=False)


def declared_sources():
    """Replay sealed production derivation 6a417430, never fixture graphs.

    First reconstruct golden17ae and production1c85; then independently match
    the old production type5eb8 before deriving the new expected type55cd.
    These controlled source declarations are not a general PostgreSQL compiler.
    """
    raw = (ROOT / "internal/dbcatalog/golden/catalog.json").read_bytes()
    profile_raw = (ROOT / "internal/dbcatalog/golden/production-v0.json").read_bytes()
    if digest(raw) != "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8":
        raise AssertionError("independent golden changed")
    if digest(profile_raw) != "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d":
        raise AssertionError("independent production profile changed")
    golden, profile = json.loads(raw), json.loads(profile_raw)
    if catalog_bytes(golden) != raw or len(golden["objects"]) != 4432:
        raise AssertionError("independent golden framing changed")
    fields = [[1, "id", "uuid", True, ""],
        [2, "created_at", "timestamp with time zone", False, ""],
        [3, "updated_at", "timestamp with time zone", False, ""],
        [4, "deleted_at", "timestamp with time zone", False, ""],
        [5, "reseller_id", "uuid", False, ""],
        [6, "name", "character varying(255)", True, '"default"'],
        [7, "slug", "character varying(100)", True, '"default"'],
        [8, "settings", "jsonb", False, ""]]
    definition = ["c", "C", False, "-", "", -1, False, "d", "x",
        "record_in(cstring,oid,integer)", "record_out(record)", "-", fields, []]
    pg = lambda value: json.dumps(value, ensure_ascii=False, separators=(", ", ": ")).encode()
    if digest(pg(definition)) != "63697f813c0cfa2b9ae24e6d604bda9d64b9851720c11a9963ba059b824bb1ea":
        raise AssertionError("reviewed old application blueprint mismatch")
    added_column = {"kind": "columns", "identity": "organizations.extra",
        "parent": "organizations", "attributes": {"acl": "", "collation_sha256": digest(b""),
            "default_sha256": digest(b""), "generated": "", "identity": "", "not_null": "false",
            "position": "9", "type_sha256": digest(b"bigint")}}
    # Independent wrong-profile negative: derive application-order target from
    # the original source golden, not the actual AD observation fixture.
    app_definition, app_catalog = copy.deepcopy(definition), copy.deepcopy(golden)
    app_definition[12].append([9, "extra", "bigint", False, ""])
    for row in app_catalog["objects"]:
        if (row["kind"], row["identity"]) == ("types", "organizations"):
            row["attributes"]["definition_sha256"] = digest(pg(app_definition))
    app_catalog["objects"].append(copy.deepcopy(added_column))
    application_after = catalog_bytes(app_catalog)
    if digest(application_after) != "1a06e19765347268a83a80288f1d054b43caa72ae625f4c2e9b8a94a1d92ee77":
        raise AssertionError("independent wrong-profile negative changed")
    production = copy.deepcopy(golden)
    rows = {(obj["kind"], obj["identity"]): obj for obj in production["objects"]}
    seen = set()
    for delta in profile["delta"]:
        key = delta["kind"], delta["identity"]
        if key in seen or delta["disposition"] != "fix-bootstrap":
            raise AssertionError("invalid source delta")
        seen.add(key)
        attr = "position" if key[0] == "columns" else "definition_sha256"
        if key[0] not in ("columns", "types") or set(delta["attributes"]) != {attr}:
            raise AssertionError("unexpected source delta shape")
        pair = delta["attributes"][attr]
        if rows[key]["attributes"][attr] != pair["golden"]:
            raise AssertionError("source delta lost golden binding")
        rows[key]["attributes"][attr] = pair["production"]
    before = catalog_bytes(production)
    if (len(seen) != 72 or digest(before) != BEFORE
            or before != (ROOT / (PREFIX + "catalog-v0.json")).read_bytes()):
        raise AssertionError("independent full production reconstruction failed")
    orders = [item["columns"] for item in profile["bootstrap_column_order"]
              if item["table"] == "organizations"]
    by_name = {field[1]: field for field in fields}
    if len(orders) != 1 or len(orders[0]) != 8 or set(orders[0]) != set(by_name):
        raise AssertionError("source organization order mismatch")
    definition[12] = []
    for position, name in enumerate(orders[0], 1):
        field = copy.deepcopy(by_name[name])
        field[0] = position
        column = rows["columns", "organizations." + name]["attributes"]
        if (column["position"] != str(position) or column["type_sha256"] != digest(field[2].encode())
                or column["not_null"] != str(field[3]).lower()
                or column["collation_sha256"] != digest(("pg_catalog.default" if field[4] else "").encode())
                or any(column[key] != "" for key in ("identity", "generated", "acl"))):
            raise AssertionError("source column does not match row blueprint")
        definition[12].append(field)
    old_type = digest(pg(definition))
    if (old_type != "5eb86115e47f40c97e52705ae5134b2358e24fb98ee2725c859df61bc5148df4"
            or old_type != rows["types", "organizations"]["attributes"]["definition_sha256"]):
        raise AssertionError("old production type must match before deriving target")
    definition[12].append([9, "extra", "bigint", False, ""])
    new_type = digest(pg(definition))
    if new_type != "55cdece8b65005d806f3239642c621a3913fdfe1d8856140aff424a4879fb5ea":
        raise AssertionError("independent new production type changed")
    rows["types", "organizations"]["attributes"]["definition_sha256"] = new_type
    production["objects"].append(added_column)
    after = catalog_bytes(production)
    operation = wire({"column": "extra", "default": None, "format_version": 1,
        "id": "add_extra", "kind": "add_nullable_column", "nullable": True,
        "relation": "organizations", "type": "pg_catalog.int8"})
    def ref(role, name, body):
        return {"role": role, "path": PREFIX + name, "bytes": len(body), "sha256": digest(body)}
    descriptor = wire({"format_version": 1, "profile": "nullable-int8-no-default-pg17-observation2",
        "postgres_major": 17, "observation_version": 2,
        "default_acl_contract_sha256": "6f51817c3516988a08b66193fedbb00a375bf8a23459c297593af3f675b37c53",
        "renderer": "nullable-column-v1",
        "operation": {**ref("candidate", "operation-v1.json", operation), "id": "add_extra",
            "statement_sha256": digest(b'ALTER TABLE ONLY "public"."organizations" ADD COLUMN "extra" pg_catalog.int8;\n')},
        "before": {**ref("retained_origin", "catalog-v0.json", before), "catalog_wire_version": 0,
            "catalog_version": 0, "min_reader": 0, "catalog_sha256": digest(before)},
        "after": {**ref("candidate", "catalog-v1.json", after), "catalog_wire_version": 0,
            "catalog_version": 1, "min_reader": 0, "catalog_sha256": digest(after)}})
    if (digest(after), digest(operation), digest(descriptor)) != (AFTER, OPERATION, DESCRIPTOR):
        raise AssertionError("sealed source-only derivation changed")
    if operation != (ROOT / (PREFIX + "probe-operation.json")).read_bytes():
        raise AssertionError("fixed probe differs from declared operation")
    return ({"catalog-v0.json": before, "probe-operation.json": operation,
             "transition-v1.json": descriptor, "operation-v1.json": operation, "catalog-v1.json": after},
            application_after)


class NullableResolverTests(unittest.TestCase):
    # Only reuse controlled command-edge fixtures. Real signature parsers,
    # source transport, classifier, full-chain policy and AG binder stay active.
    setUp = source.ResolverTests.setUp
    add = source.ResolverTests.add
    profile = source.ForwardResolverTests.profile
    candidate = source.ForwardResolverTests.candidate
    WORKFLOW = source.ForwardResolverTests.WORKFLOW
    MAIN = source.ForwardResolverTests.MAIN
    RECIPE = source.ForwardResolverTests.RECIPE

    @classmethod
    def setUpClass(cls):
        source.ResolverTests.setUpClass.__func__(cls)
        cls.declared, cls.application_after = declared_sources()

    def install(self, sha, *, plan=True):
        for path, raw in self.declared.items():
            if plan or path in ("catalog-v0.json", "probe-operation.json"):
                self.world.files[sha, PREFIX + path] = raw

    def ordinary(self, *, legacy=False, candidate=None):
        old = self.add(version=1 if legacy else 2)
        self.profile(old["sha"])
        self.install(old["sha"], plan=False)
        sha = self.candidate() if candidate is None else self.candidate(candidate)
        self.install(sha)
        return old, sha

    def prepare(self, sha, *, normal=False, overrides=None):
        self.assertIsNone(self.client, "each controlled operation is fresh")
        config, work = self.root / "config", self.root / "work"
        config.mkdir()
        work.mkdir()
        budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
        self.client = rt.ReleaseGh(self.gh, support.GH_TOKEN, config, work, budget=budget,
                                  base_env={"SHIP_DO_TOKEN": "private"})
        if overrides is not None:
            options = dict(selected_tag=None, candidate_sha=sha, forward=True, nullable_profile=True)
            options.update(overrides)
            return resolver._resolve(self.client, self.repo, self.git, **options)
        function = resolver.resolve_forward if normal else resolver._resolve_nullable_profile
        return function(self.client, self.repo, self.git, sha)

    def refused(self, sha, **kwargs):
        with self.assertRaises(rt.TransportRefusal) as caught:
            self.prepare(sha, **kwargs)
        self.assertRegex(str(caught.exception), r"^record-chain-invalid:transport-resolver(?:-[a-z]+)?$")
        self.assertNotIn("private", str(caught.exception))
        self.assertTrue(self.client.budget.failed)
        self.assertTrue(self.client.budget.closed)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())
        count = len(self.world.trace)
        with self.assertRaises(rt.TransportRefusal):
            self.client.capture_snapshot()
        self.assertEqual(count, len(self.world.trace))

    def fresh(self):
        case = type(self)()
        case.setUp()
        self.addCleanup(case.doCleanups)
        return case

    def test_full_source_binding_is_not_active_target_or_measurement(self):
        old, sha = self.ordinary()
        result = self.prepare(sha)
        self.assertIs(type(result), resolver.NullableProfilePreparation)
        self.assertEqual(result.unavailable, ())
        self.assertIs(type(result.bound), nullable.BoundNullableProfile)
        self.assertEqual(result.before_lookup_sha, old["sha"])
        self.assertEqual(result.before_provenance, "live-origin")
        self.assertEqual(result.planned_target, nullable.CatalogCoordinates(1, 0, AFTER))
        self.assertEqual(result.forward.candidate.catalog.catalog_version, 0)
        self.assertEqual(result.forward.resolved.latest.live.catalog.catalog_sha256, BEFORE)
        self.assertEqual(result.forward.pending.candidate_files, ())
        self.assertEqual(result.forward.resolved.latest, result.forward.resolved.selected)
        self.assertNotEqual(sha, result.forward.resolved.inventory.main_sha)
        self.assertEqual(json.loads(result.bound._origin)["manifest_sha256"],
                         result.forward.resolved.latest.live.origin_manifest_sha256)
        self.assertTrue(self.client.budget.closed)
        self.assertFalse(self.client.budget.failed)

    def test_source_target_is_derived_before_collection_and_diff_is_exact(self):
        before, after = (json.loads(self.declared[key]) for key in ("catalog-v0.json", "catalog-v1.json"))
        left = {(row["kind"], row["identity"]): row for row in before["objects"]}
        right = {(row["kind"], row["identity"]): row for row in after["objects"]}
        self.assertEqual(sorted(right.keys() - left.keys()), [("columns", "organizations.extra")])
        self.assertEqual([key for key in left if left[key] != right[key]], [("types", "organizations")])
        self.assertEqual(left["types", "_organizations"], right["types", "_organizations"])
        self.assertEqual((len(left), len(right)), (4432, 4433))
        self.assertEqual(sum(len(self.declared[name]) for name in (
            "catalog-v0.json", "catalog-v0.json", "probe-operation.json", "transition-v1.json",
            "operation-v1.json", "catalog-v1.json")), 5288652)

    def test_rollback_keeps_origin_candidate_and_actual_web_distinct(self):
        previous = self.add(version=1)
        self.profile(previous["sha"])
        origin = self.add()
        self.profile(origin["sha"])
        self.install(origin["sha"], plan=False)
        self.add(kind="rollback", target=previous)
        sha = self.candidate()
        self.install(sha)
        result = self.prepare(sha)
        self.assertEqual(len({origin["sha"], previous["sha"], sha}), 3)
        self.assertEqual(result.before_lookup_sha, origin["sha"])
        self.assertEqual(result.material.before_catalog.source_sha, origin["sha"])
        self.assertEqual(result.forward.previous_web_source_sha, previous["sha"])
        self.assertEqual(result.forward.resolved.latest.record_sha, previous["sha"])
        self.assertEqual(json.loads(result.bound._origin)["release"], origin["release"])

    def test_same_origin_candidate_still_retains_six_role_slots(self):
        old = self.add()
        self.profile(old["sha"])
        self.install(old["sha"])
        result = self.prepare(old["sha"])
        self.assertEqual(tuple(row.slot for row in result.observations), SLOTS)
        self.assertEqual({row.source_sha for row in result.observations}, {old["sha"]})
        self.assertEqual(result.observations[0].raw, result.observations[1].raw)
        self.assertEqual(sum(len(row.raw) for row in result.observations), 5288652)
        self.assertIsNotNone(result.bound)

    def test_legacy_missing_before_does_not_copy_candidate_baseline(self):
        old, sha = self.ordinary(legacy=True)
        del self.world.files[old["sha"], PREFIX + "catalog-v0.json"]
        result = self.prepare(sha)
        self.assertEqual(result.unavailable, ("legacy-live-unavailable", "origin-profile-missing"))
        self.assertIsNone(result.observations[0].raw)
        self.assertIsNotNone(result.observations[1].raw)
        self.assertIsNone(result.material)
        self.assertIsNone(result.bound)
        self.assertIsNone(result.forward.resolved.latest.live)
        self.assertEqual(result.before_provenance, "legacy-record-only")

    def test_legacy_complete_material_remains_without_live_authority(self):
        old, sha = self.ordinary(legacy=True)
        result = self.prepare(sha)
        self.assertEqual(result.unavailable, ("legacy-live-unavailable",))
        self.assertIs(type(result.material), nullable.ProfileMaterial)
        self.assertIsNone(result.bound)
        self.assertIsNone(result.forward.resolved.chain.baseline)
        self.assertIsNone(result.forward.resolved.latest.live)
        self.assertEqual(result.before_lookup_sha, old["sha"])

    def test_repeated_v1_candidate_keeps_required_complete_precedence(self):
        old = self.add(version=1)
        self.profile(old["sha"])
        self.install(old["sha"])
        result = self.prepare(old["sha"])
        matches = [row for row in result.forward.resolved.chain.sources if row.sha == old["sha"]]
        self.assertEqual(matches, [result.forward.candidate])
        self.assertEqual(matches[0].mode, "reviewed-v0")
        self.assertEqual(result.unavailable, ("legacy-live-unavailable",))
        self.assertIsNone(result.bound)

    def test_missing_required_active_contract_never_falls_back_to_v1(self):
        old = self.add(version=1)
        self.profile(old["sha"])
        self.install(old["sha"])
        del self.world.files[old["sha"], policy.HISTORY]
        self.refused(old["sha"])

    def test_all_plan_absent_and_all_profile_absent_are_explicit(self):
        for remove_baseline in (False, True):
            with self.subTest(remove_baseline=remove_baseline):
                case = self.fresh()
                old, sha = case.ordinary(legacy=True)
                for name in ("transition-v1.json", "operation-v1.json", "catalog-v1.json"):
                    del case.world.files[sha, PREFIX + name]
                del case.world.files[old["sha"], PREFIX + "catalog-v0.json"]
                if remove_baseline:
                    for name in ("catalog-v0.json", "probe-operation.json"):
                        del case.world.files[sha, PREFIX + name]
                result = case.prepare(sha)
                self.assertEqual(result.unavailable,
                    ("legacy-live-unavailable", "origin-profile-missing", "candidate-plan-missing"))
                self.assertIsNone(result.planned_target)
                self.assertIsNone(result.material)
                self.assertIsNone(result.bound)

    def test_v2_missing_origin_is_unavailable_not_synthesized(self):
        old, sha = self.ordinary()
        del self.world.files[old["sha"], PREFIX + "catalog-v0.json"]
        result = self.prepare(sha)
        self.assertEqual(result.unavailable, ("origin-profile-missing",))
        self.assertIsNone(result.material)
        self.assertIsNone(result.bound)
        self.assertIsNotNone(result.forward.resolved.latest.live)

    def test_partial_plan_or_incomplete_probe_pair_refuses(self):
        for missing in (("transition-v1.json",), ("operation-v1.json",), ("catalog-v1.json",),
                        ("catalog-v0.json",), ("probe-operation.json",),
                        ("catalog-v0.json", "probe-operation.json"),
                        ("transition-v1.json", "operation-v1.json", "catalog-v1.json", "probe-operation.json")):
            with self.subTest(missing=missing):
                case = self.fresh()
                _old, sha = case.ordinary()
                for name in missing:
                    del case.world.files[sha, PREFIX + name]
                case.refused(sha)

    def test_every_present_slot_is_checked_on_unavailable_legacy_route(self):
        for name in self.declared:
            with self.subTest(name=name):
                case = self.fresh()
                old, sha = case.ordinary(legacy=True)
                del case.world.files[old["sha"], PREFIX + "catalog-v0.json"]
                case.world.files[sha, PREFIX + name] = b"private-malformed\n"
                case.refused(sha)
        case = self.fresh()
        old, sha = case.ordinary(legacy=True)
        case.world.files[old["sha"], PREFIX + "catalog-v0.json"] = b""
        case.refused(sha)

    def test_absent_plan_does_not_hide_malformed_baseline_or_probe(self):
        for name in ("catalog-v0.json", "probe-operation.json"):
            case = self.fresh()
            _old, sha = case.ordinary()
            for plan in ("transition-v1.json", "operation-v1.json", "catalog-v1.json"):
                del case.world.files[sha, PREFIX + plan]
            case.world.files[sha, PREFIX + name] += b" "
            case.refused(sha)

    def test_canonical_descriptor_role_path_hash_and_coordinates_cannot_select_reads(self):
        variants = (
            lambda doc: doc["before"].update(role="candidate"),
            lambda doc: doc["operation"].update(path="private/arbitrary.json"),
            lambda doc: doc["after"].update(sha256="0" * 64),
            lambda doc: doc["after"].update(catalog_version=0),
            lambda doc: doc["after"].update(catalog_version=True),
            lambda doc: doc.update(default_acl_contract_sha256="f" * 64),
        )
        for change in variants:
            case = self.fresh()
            _old, sha = case.ordinary(legacy=True)
            value = json.loads(case.declared["transition-v1.json"])
            change(value)
            case.world.files[sha, PREFIX + "transition-v1.json"] = wire(value)
            case.refused(sha)
            self.assertFalse(any(args[-1] == "private/arbitrary.json" for args, _ in case.world.trace))

    def test_swapped_catalogs_and_wrong_type_hash_refuse_with_coherent_json(self):
        for variant in ("swap", "old-type", "no-op", "application"):
            case = self.fresh()
            old, sha = case.ordinary()
            after = json.loads(case.declared["catalog-v1.json"])
            if variant == "swap":
                case.world.files[old["sha"], PREFIX + "catalog-v0.json"] = case.declared["catalog-v1.json"]
                body = case.declared["catalog-v0.json"]
            elif variant == "no-op":
                body = case.declared["catalog-v0.json"]
            elif variant == "application":
                body = case.application_after
            else:
                for row in after["objects"]:
                    if (row["kind"], row["identity"]) == ("types", "organizations"):
                        row["attributes"]["definition_sha256"] = "5eb86115e47f40c97e52705ae5134b2358e24fb98ee2725c859df61bc5148df4"
                body = catalog_bytes(after)
            case.world.files[sha, PREFIX + "catalog-v1.json"] = body
            case.refused(sha)

    def test_operation_multiple_or_changed_shape_and_noncanonical_documents_refuse(self):
        for body in (b"", b"{}\n", self.declared["operation-v1.json"] + b" ",
                     wire([json.loads(self.declared["operation-v1.json"])]),
                     self.declared["operation-v1.json"].replace(b'"extra"', b'"other"')):
            case = self.fresh()
            _old, sha = case.ordinary()
            case.world.files[sha, PREFIX + "operation-v1.json"] = body
            case.refused(sha)

    def test_fixed_regular_blob_modes_and_tree_shape_are_required(self):
        for mode in (b"120000", b"160000", b"duplicate"):
            case = self.fresh()
            _old, sha = case.ordinary()
            original = case.world.git
            def altered(args, original=original, mode=mode, sha=sha):
                result = original(args)
                if args == ["ls-tree", "-z", sha, "--", PREFIX + "operation-v1.json"]:
                    body = result.stdout * 2 if mode == b"duplicate" else result.stdout.replace(b"100644", mode, 1)
                    return rt.runtime.Capture(0, body, b"")
                return result
            case.world.git = altered
            case.refused(sha)

    def test_fixed_individual_caps_refuse_complete_oversized_blobs(self):
        for name, size in (("catalog-v1.json", (4 << 20) + 1),
                           ("transition-v1.json", (16 << 10) + 1),
                           ("operation-v1.json", (2 << 10) + 1)):
            case = self.fresh()
            _old, sha = case.ordinary()
            case.world.files[sha, PREFIX + name] = b" " * size
            case.refused(sha)

    def test_all_six_slots_count_before_decoding_even_for_duplicate_identity(self):
        old = self.add()
        self.profile(old["sha"])
        self.install(old["sha"])
        # Each individual blob fits. Two identical 4MiB baseline roles plus
        # the probe exceed aggregate8MiB; parsing must not begin at all.
        self.world.files[old["sha"], PREFIX + "catalog-v0.json"] = b" " * (4 << 20)
        with mock.patch.object(nullable, "_catalog", side_effect=AssertionError("decode before bound")) as parser:
            self.refused(old["sha"])
        parser.assert_not_called()

    def test_planned_source_coordinates_never_change_active_catalog_acceptance(self):
        _old, sha = self.ordinary()
        active = json.loads(self.world.files[sha, policy.HISTORY])
        self.assertEqual(active, [{"version": 0, "class": "baseline", "min_reader_version": 0}])
        planned = json.loads(self.world.files[sha, PREFIX + "transition-v1.json"])["after"]
        self.assertEqual((planned["catalog_version"], planned["min_reader"]), (1, 0))
        # A valid history extension attempts to promote planned coordinates to
        # active ones. Its grammar is valid; reviewed_v0 must still reject it.
        active.append({"version": planned["catalog_version"], "class": "expand",
                       "min_reader_version": planned["min_reader"], "apply_engine": True})
        self.assertIsNone(policy.schema_change._history(active))
        self.world.files[sha, policy.HISTORY] = wire(active)
        self.refused(sha)

    def test_source_ancestry_still_required_for_origin_and_latest(self):
        old, sha = self.ordinary()
        self.world.bad_pairs.add((old["sha"], sha))
        self.refused(sha)

    def test_invalid_candidate_or_incompatible_private_route_refuses_before_source(self):
        for changes in ({"candidate_sha": True}, {"candidate_sha": "main"},
                        {"admission": True}, {"rollback": True}, {"rollback_proof": True},
                        {"forward": False}, {"selected_tag": common.BOOTSTRAP_TAG}):
            case = self.fresh()
            _old, sha = case.ordinary()
            case.refused(sha, overrides=changes)
            self.assertFalse(any(args[0] == str(case.git) for args, _ in case.world.trace))

    def test_normal_forward_adds_no_nullable_slot_reads_and_keeps_original_result(self):
        _old, sha = self.ordinary()
        result = self.prepare(sha, normal=True)
        self.assertIs(type(result), resolver.ForwardResolution)
        self.assertFalse(hasattr(result, "planned_target"))
        # Source classification may probe its one fixed declaration. The
        # ordinary route still must not collect any of the six profile slots.
        rows = [(args[-3], args[-1]) for args, _ in self.world.trace
                if "ls-tree" in args and args[-1].startswith(PREFIX)]
        self.assertEqual(rows, [(sha, PREFIX + "source-shape-v1.json")])

    def test_source_preparation_preserves_real_classifier_blockers(self):
        _old, sha = self.ordinary()
        self.world.files[sha, self.MAIN] += b"\nfunc runOther() {}\n"
        # A stale source closure remains a real classifier reason; this source
        # preparation route neither calls guard nor upgrades it to permission.
        self.world.files[sha, self.MAIN] = self.world.files[sha, self.MAIN].replace(b"println(1)", b"println(2)")
        result = self.prepare(sha)
        self.assertIn("migration-contract-stale", result.forward.classification.reasons)
        self.assertIsNotNone(result.bound)

    def test_final_git_preflight_rejects_drift_after_last_fixed_blob(self):
        _old, sha = self.ordinary()
        loaded = False
        original = self.world.git
        def drift(args):
            nonlocal loaded
            result = original(args)
            if args == ["ls-tree", "-z", sha, "--", PREFIX + "catalog-v1.json"]:
                loaded = True
            if loaded and args == ["rev-parse", "--is-shallow-repository"]:
                return rt.runtime.Capture(0, b"true\n", b"")
            return result
        self.world.git = drift
        self.refused(sha)
        self.assertTrue(loaded)

    def test_final_github_snapshot_refuses_main_or_record_drift(self):
        for what in ("main", "record"):
            case = self.fresh()
            _old, sha = case.ordinary()
            def drift(args, case=case, sha=sha, what=what):
                if args[-5:] == ["ls-tree", "-z", sha, "--", PREFIX + "catalog-v1.json"]:
                    if what == "main":
                        case.world.main = "b" * 40
                    else:
                        case.world.fake.releases[-1]["assets"][0]["size"] += 1
            case.world.after = drift
            case.refused(sha)
            self.assertEqual(sum("--show-toplevel" in args for args, _ in case.world.trace), 2)

    def test_last_read_and_final_recheck_share_original_clock(self):
        for boundary in ("last-read", "recheck"):
            case = self.fresh()
            _old, sha = case.ordinary()
            loaded = False
            def expire(args, case=case, sha=sha, boundary=boundary):
                nonlocal loaded
                if args[-5:] == ["ls-tree", "-z", sha, "--", PREFIX + "catalog-v1.json"]:
                    loaded = True
                    if boundary == "last-read":
                        case.clock.now = 701
                if loaded and boundary == "recheck" and args[0] == str(case.gh):
                    case.clock.now = 701
            case.world.after = expire
            case.refused(sha)

    def test_shared_outer_command_and_byte_caps_are_not_reset_for_profile(self):
        for field, maximum in (("commands", rt.MAX_COMMANDS), ("captured_bytes", rt.MAX_BYTES)):
            case = self.fresh()
            old, sha = case.ordinary()
            def exhaust(args, case=case, old=old, field=field, maximum=maximum):
                if args[-5:] == ["ls-tree", "-z", old["sha"], "--", PREFIX + "catalog-v0.json"]:
                    setattr(case.client.budget, field, maximum)
            case.world.after = exhaust
            case.refused(sha)

    def test_source_command_error_is_masked_and_never_unavailable(self):
        old, sha = self.ordinary(legacy=True)
        def fail(args):
            if args[-5:] == ["ls-tree", "-z", old["sha"], "--", PREFIX + "catalog-v0.json"]:
                raise RuntimeError("private-token arbitrary-path source error")
        self.world.before = fail
        self.refused(sha)

    def test_no_extra_allowance_is_granted_after_profile_reads(self):
        _old, sha = self.ordinary()
        after_source = False
        def advance(args):
            nonlocal after_source
            if args[-5:] == ["ls-tree", "-z", sha, "--", PREFIX + "catalog-v1.json"]:
                self.clock.now = 500
                after_source = True
        self.world.after = advance
        self.prepare(sha)
        self.assertTrue(after_source)
        # The original deadline is 700; only 200 seconds remain, including
        # each transport's existing cleanup reserve, for the final recheck.
        last_read = next(i for i, (args, _) in enumerate(self.world.trace)
            if args[-5:] == ["ls-tree", "-z", sha, "--", PREFIX + "catalog-v1.json"])
        final = [options for args, options in self.world.trace[last_read + 1:]
                 if args[0] == str(self.gh)]
        self.assertTrue(final)
        self.assertTrue(all(options["timeout"] < 200 for options in final))

    def test_collection_order_is_fixed_and_final_checks_follow_all_six_reads(self):
        old, sha = self.ordinary()
        declaration = PREFIX + "source-shape-v1.json"
        def advance(args):
            if args[-5:] == ["ls-tree", "-z", sha, "--", declaration]:
                self.clock.now = 500
        self.world.after = advance
        result = self.prepare(sha)
        rows = [(i, args[-3], args[-1]) for i, (args, _) in enumerate(self.world.trace)
                if "ls-tree" in args and args[-1].startswith(PREFIX)]
        expected = [(row.source_sha, row.path) for row in result.observations]
        self.assertEqual(len(expected), 6)
        # Check the whole prefix inventory, not a filter that could hide an
        # extra read: one classifier declaration, then exactly six data slots.
        self.assertEqual([(source_sha, path) for _i, source_sha, path in rows],
                         [(sha, declaration), *expected])
        self.assertEqual(len(rows), 7)
        tops = [i for i, (args, _) in enumerate(self.world.trace) if "--show-toplevel" in args]
        self.assertLess(tops[0], rows[0][0])
        self.assertLess(rows[-1][0], tops[-1])
        self.assertTrue(any(i > tops[-1] and args[0] == str(self.gh)
                            for i, (args, _) in enumerate(self.world.trace)))
        later = [options for _args, options in self.world.trace[rows[0][0] + 1:]]
        self.assertTrue(later)
        # The operation started with deadline700. The classifier probe must
        # consume the same allowance as profile reads and final Git/GH checks.
        self.assertTrue(all(options["timeout"] < 200 for options in later))
        self.assertEqual(result.observations[0].source_sha, old["sha"])

    def test_projection_is_deeply_immutable_and_raw_bytes_are_not_repr(self):
        _old, sha = self.ordinary()
        result = self.prepare(sha)
        with self.assertRaises(FrozenInstanceError):
            result.bound = None
        with self.assertRaises(FrozenInstanceError):
            result.observations[0].raw = b"{}"
        self.assertIs(type(result.observations), tuple)
        self.assertTrue(all(type(row.raw) is bytes for row in result.observations))
        self.assertNotIn('"objects"', repr(result.observations[0]))
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())


if __name__ == "__main__":
    unittest.main()
