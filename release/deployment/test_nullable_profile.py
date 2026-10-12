"""Independent source blueprint composed with the required actual AD capture.

O/H identities are controlled policy inputs, not Git authentication. Expected
catalogs are built BEFORE loading the observation, never copied from its graph.
"""
import copy
from dataclasses import FrozenInstanceError, replace
import hashlib
import json
from pathlib import Path
import unittest

import catalog_semantics as wire_format
import nullable_profile as profile

ROOT = Path(__file__).resolve().parents[2]
GOLDEN = ROOT / "internal/dbcatalog/golden/catalog.json"
FIXTURE = ROOT / "internal/dbcatalog/testdata/nullable-column-v2.json"
GOLDEN_SHA = "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8"
FIXTURE_SHA = "78ecf998236b22f60ccadbe202ef5eea9f337d74e08f45fef47e5f22817650f2"
OLD_TYPE_SHA = "63697f813c0cfa2b9ae24e6d604bda9d64b9851720c11a9963ba059b824bb1ea"
PRODUCTION_SHA = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"
O, H = "1" * 40, "2" * 40


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def wire(value, *, sorted_keys=True):
    text = json.dumps(value, ensure_ascii=False, sort_keys=sorted_keys,
                      separators=(",", ":"), allow_nan=False)
    for char, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(char, escaped)
    return (text + "\n").encode()


def catalog_bytes(value):
    # Independent implementation of the reviewed Go struct serialization.
    rows = []
    for item in sorted(value["objects"], key=lambda r: (r["kind"], r["identity"])):
        row = {"kind": item["kind"], "identity": item["identity"]}
        if item.get("parent"):
            row["parent"] = item["parent"]
        row["attributes"] = dict(sorted(item["attributes"].items()))
        rows.append(row)
    return wire({"version": value["version"], "objects": rows}, sorted_keys=False)


def source_catalogs(raw):
    """Reviewed source-only blueprint ae14e72e; no observation parameter.

    Model BaseModel/Organization order + queries.go 14-slot query and explicit
    PG17 composite header are checked against the older opaque golden hash.
    This is a controlled test declaration, not a general model/DDL compiler.
    """
    if sha(raw) != GOLDEN_SHA:
        raise AssertionError("independent source golden changed")
    before = json.loads(raw)
    if catalog_bytes(before) != raw:
        raise AssertionError("source golden not canonical")
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
    pg_text = lambda v: json.dumps(v, ensure_ascii=False, separators=(", ", ": ")).encode()
    rows = {(o["kind"], o["identity"]): o for o in before["objects"]}
    if sha(pg_text(definition)) != OLD_TYPE_SHA or (
            rows["types", "organizations"]["attributes"]["definition_sha256"] != OLD_TYPE_SHA):
        raise AssertionError("independent old row blueprint mismatch")
    for position, name, type_text, not_null, collation in fields:
        attributes = rows["columns", "organizations." + name]["attributes"]
        if (attributes["position"] != str(position)
                or attributes["type_sha256"] != sha(type_text.encode())
                or attributes["not_null"] != str(not_null).lower()
                or attributes["collation_sha256"] != sha(("pg_catalog.default" if collation else "").encode())):
            raise AssertionError("independent source column mismatch")
    definition[12].append([9, "extra", "bigint", False, ""])
    after = copy.deepcopy(before)
    for row in after["objects"]:
        if (row["kind"], row["identity"]) == ("types", "organizations"):
            row["attributes"]["definition_sha256"] = sha(pg_text(definition))
    after["objects"].append({"kind": "columns", "identity": "organizations.extra",
        "parent": "organizations", "attributes": {"acl": "", "collation_sha256": sha(b""),
            "default_sha256": sha(b""), "generated": "", "identity": "", "not_null": "false",
            "position": "9", "type_sha256": sha(b"bigint")}})
    return raw, catalog_bytes(after)


def declared_operation():
    return {"format_version": 1, "id": "add_extra", "kind": "add_nullable_column",
            "relation": "organizations", "column": "extra", "type": "pg_catalog.int8",
            "nullable": True, "default": None}


def material(before, after, *, operation=None):
    operation = wire(declared_operation()) if operation is None else operation
    old = profile.CatalogCoordinates(0, 0, sha(before))
    new = profile.CatalogCoordinates(1, 0, sha(after))
    origin = profile.RetainedOrigin("prod-20261012T010203Z-12345678", "3" * 64, O, old)
    candidate = profile.CandidateSource(H, new)
    source_op = profile.SourceBlob(H, "profiles/operation.json", operation)
    source_before = profile.SourceBlob(O, "catalog/shape.json", before)
    source_after = profile.SourceBlob(H, "catalog/shape.json", after)
    def ref(blob, role):
        return {"role": role, "path": blob.path, "bytes": len(blob.raw), "sha256": sha(blob.raw)}
    descriptor = {"format_version": 1, "profile": profile.PROFILE, "postgres_major": 17,
        "observation_version": 2, "default_acl_contract_sha256": profile.DEFAULT_ACL_SHA256,
        "renderer": "nullable-column-v1",
        "operation": {**ref(source_op, "candidate"), "id": "add_extra",
            "statement_sha256": sha(b'ALTER TABLE ONLY "public"."organizations" ADD COLUMN "extra" pg_catalog.int8;\n')},
        "before": {**ref(source_before, "retained_origin"), "catalog_wire_version": 0,
            "catalog_version": old.version, "min_reader": old.min_reader, "catalog_sha256": old.catalog_sha256},
        "after": {**ref(source_after, "candidate"), "catalog_wire_version": 0,
            "catalog_version": new.version, "min_reader": new.min_reader, "catalog_sha256": new.catalog_sha256}}
    result = profile.ProfileMaterial(profile.SourceBlob(H, "profiles/nullable.json", wire(descriptor)),
                                     source_op, source_before, source_after)
    return origin, candidate, result


def descriptor_change(inputs, function):
    origin, candidate, sources = inputs
    descriptor = json.loads(sources.profile.raw)
    function(descriptor)
    return origin, candidate, replace(sources, profile=replace(sources.profile, raw=wire(descriptor)))


def reseal_observation(value):
    # Negative tests keep hashes honest without changing checker behavior.
    raw = catalog_bytes({"version": 0, "objects": value["snapshot"]["objects"]})
    value["snapshot"]["objects"] = json.loads(raw)["objects"]
    value["snapshot"]["sha256"] = sha(raw)
    value["graph"]["catalog_sha256"] = sha(raw)


def forged(holder, **changes):
    copy_ = object.__new__(type(holder))
    for key in holder.__slots__:
        object.__setattr__(copy_, key, changes.get(key, getattr(holder, key)))
    return copy_


class ProfileTests(unittest.TestCase):
    measurements = {}

    @classmethod
    def setUpClass(cls):
        # Establish both expectations first. Missing files fail, never skip.
        cls.source_before, cls.source_after = source_catalogs(GOLDEN.read_bytes())
        raw = FIXTURE.read_bytes()
        if sha(raw) != FIXTURE_SHA:
            raise AssertionError("actual AD fixture changed")
        cls.fixture = json.loads(raw)
        if (set(cls.fixture) != {"format_version", "operation", "before", "after", "explanation"}
                or type(cls.fixture["format_version"]) is not int or cls.fixture["format_version"] != 2
                or wire(cls.fixture) != raw or cls.fixture["operation"] != declared_operation()):
            raise AssertionError("actual AD fixture contract changed")
        cls.observed_before, cls.observed_after = (wire(cls.fixture[k]) for k in ("before", "after"))

    def inputs(self):
        return material(self.source_before, self.source_after)

    def bound(self, inputs=None):
        return profile.bind_source_profile(*(self.inputs() if inputs is None else inputs))

    def explain(self, bound=None, before=None, after=None):
        return profile.explain_profile(self.bound() if bound is None else bound,
            before=self.observed_before if before is None else before,
            after=self.observed_after if after is None else after)

    def refused(self, function, *args, code=None, **kwargs):
        with self.assertRaises(profile.ProfileRefusal) as caught:
            function(*args, **kwargs)
        if code is not None:
            self.assertEqual(caught.exception.code, "nullable-profile:" + code)
        self.assertRegex(str(caught.exception), r"^nullable-profile:[a-z]+$")

    def test_independent_blueprint_and_complete_actual_composition(self):
        before, after = json.loads(self.source_before), json.loads(self.source_after)
        self.assertEqual(len(before["objects"]), 4432)
        self.assertEqual(len(after["objects"]), 4433)
        old = {(r["kind"], r["identity"]): r for r in before["objects"]}
        new = {(r["kind"], r["identity"]): r for r in after["objects"]}
        self.assertEqual(set(new) - set(old), {("columns", "organizations.extra")})
        self.assertEqual([key for key in old if old[key] != new[key]], [("types", "organizations")])
        result = profile.canonical_explanation(self.explain())
        value = json.loads(result)
        self.assertEqual(set(value), {"format_version", "purpose", "profile", "origin", "candidate", "source", "semantic"})
        self.assertEqual(value["semantic"], self.fixture["explanation"])
        self.assertEqual(value["origin"]["catalog_sha256"], GOLDEN_SHA)
        self.assertEqual(value["candidate"]["catalog_sha256"], sha(self.source_after))
        self.assertNotEqual(GOLDEN_SHA, PRODUCTION_SHA)
        self.assertEqual(value["source"]["profile"]["source_sha"], H)
        self.assertEqual(value["source"]["before_catalog"]["source_sha"], O)
        self.assertEqual(value["source"]["operation"]["sha256"], value["semantic"]["operation_sha256"])
        self.assertNotEqual(value["source"]["profile"]["sha256"], GOLDEN_SHA)
        self.assertNotIn("compatible", value)
        source = self.inputs()[2]
        sizes = {name: len(getattr(source, name).raw) for name in ("profile", "operation", "before_catalog", "after_catalog")}
        sizes.update(before=len(self.observed_before), after=len(self.observed_after))
        self.assertLessEqual(sum(sizes.values()), profile.MAX_TOTAL)
        self.measurements["complete_positive"] = {"slots": sizes, "aggregate_bytes": sum(sizes.values()),
                                                   "result_bytes": len(result)}

    def test_binding_has_no_observation_input_and_is_not_semantic_success(self):
        # A coherent no-op source contract can be bound, but cannot explain the
        # independent real after-state as that expected source or as one add.
        bound = self.bound(material(self.source_before, self.source_before))
        self.refused(self.explain, bound, code="expected")
        self.refused(self.explain, self.bound(), self.observed_after, self.observed_before)

    def test_source_identity_and_role_swaps(self):
        origin, candidate, source = self.inputs()
        for slot in ("profile", "operation", "before_catalog", "after_catalog"):
            with self.subTest(slot=slot):
                bad = replace(source, **{slot: replace(getattr(source, slot), source_sha="4" * 40)})
                self.refused(profile.bind_source_profile, origin, candidate, bad, code="source")
        self.refused(profile.bind_source_profile, origin, candidate,
            replace(source, before_catalog=source.after_catalog, after_catalog=source.before_catalog))
        for slot in ("before", "after", "operation"):
            self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(),
                lambda d, slot=slot: d[slot].update(role="image_source")))

    def test_exact_paths_lengths_and_hashes(self):
        for key, value in (("path", "elsewhere.json"), ("bytes", True), ("bytes", 0),
                           ("bytes", 1), ("sha256", "0" * 64), ("sha256", "A" * 64)):
            for slot in ("operation", "before", "after"):
                with self.subTest(key=key, slot=slot):
                    self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(),
                        lambda d, slot=slot, key=key, value=value: d[slot].update({key: value})))
        origin, candidate, source = self.inputs()
        for path in ("/abs", "a//b", "a/../b", "./a", "a\\b", "C:/a", "a\nsecret", "caf\u00e9", "a/", "a" * 257):
            self.refused(profile.bind_source_profile, origin, candidate,
                replace(source, profile=replace(source.profile, path=path)), code="source")
        self.refused(profile.bind_source_profile, origin, candidate,
            replace(source, operation=replace(source.operation, path=source.profile.path)), code="source")

    def test_retained_coordinates_are_independent_and_bool_is_not_int(self):
        origin, candidate, source = self.inputs()
        for coordinates in (replace(origin.catalog, version=True), replace(origin.catalog, min_reader=True),
                replace(origin.catalog, version=-1), replace(origin.catalog, min_reader=1),
                replace(origin.catalog, version=2**63), replace(origin.catalog, catalog_sha256=PRODUCTION_SHA)):
            self.refused(profile.bind_source_profile, replace(origin, catalog=coordinates), candidate, source)
        for value in ("", "prod-0000", "secret\npath", "prod-20261012T010203Z-1234567X"):
            self.refused(profile.bind_source_profile, replace(origin, release=value), candidate, source)
        self.refused(profile.bind_source_profile, replace(origin, manifest_sha256="bad"), candidate, source)
        self.refused(profile.bind_source_profile, origin, replace(candidate, source_sha=True), source)
        for slot in ("before", "after"):
            for key in ("catalog_wire_version", "catalog_version", "min_reader"):
                self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(),
                    lambda d, slot=slot, key=key: d[slot].update({key: True})))

    def test_fixed_profile_renderer_acl_and_closed_version(self):
        for key, value in (("format_version", True), ("format_version", 2), ("profile", "other"),
                ("postgres_major", 18), ("observation_version", True), ("observation_version", 1),
                ("renderer", "arbitrary-sql"), ("default_acl_contract_sha256", "0" * 64)):
            self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(),
                lambda d, key=key, value=value: d.update({key: value})))
        self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(), lambda d: d.update(extra=True)))
        self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(), lambda d: d.pop("before")))
        self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(), lambda d: d["operation"].update(id="other")))
        self.refused(profile.bind_source_profile, *descriptor_change(self.inputs(),
            lambda d: d["operation"].update(statement_sha256="0" * 64)))

    def test_operation_is_single_nullable_no_default_and_fixed_rendering(self):
        for change in ({"default": "1"}, {"nullable": False}, {"type": "pg_catalog.text"},
                       {"kind": "add_index_concurrently"}, {"column": "extra;DROP"},
                       {"format_version": True}, {"extra": 1}):
            operation = declared_operation()
            operation.update(change)
            self.refused(profile.bind_source_profile,
                         *material(self.source_before, self.source_after, operation=wire(operation)))
        self.refused(profile.bind_source_profile, *material(self.source_before, self.source_after,
                     operation=wire([declared_operation(), declared_operation()])))

    def test_noncanonical_duplicate_unknown_and_invalid_json_refuse(self):
        origin, candidate, source = self.inputs()
        raw = source.profile.raw
        variants = (raw[:-1], b" " + raw, b'{"format_version":1,' + raw[1:],
                    raw.replace(b'"format_version":1', b'"format_version":1.0'),
                    b"\xff", b'{"secret":"sensitive-path"}', b"[" * 30 + b"]" * 30)
        for bad in variants:
            self.refused(profile.bind_source_profile, origin, candidate,
                         replace(source, profile=replace(source.profile, raw=bad)))
        # Catalog uses struct order, not sorted descriptor serialization.
        wrong_order = wire(json.loads(self.source_before))
        self.assertNotEqual(wrong_order, self.source_before)
        self.refused(profile.bind_source_profile, *material(wrong_order, self.source_after))

    def test_complete_catalog_grammar_parent_order_and_numeric_spelling(self):
        def mutate(label):
            value = json.loads(self.source_before)
            child = next(r for r in value["objects"] if r["kind"] == "columns")
            if label == "orphan": child["parent"] = "absent"
            elif label == "empty-parent": next(r for r in value["objects"] if r["kind"] == "types")["parent"] = ""
            elif label == "missing": child["missing"] = True
            elif label == "unknown-attr": child["attributes"]["x"] = "1"
            elif label == "missing-attr": child["attributes"].pop("acl")
            elif label == "dup": value["objects"].append(copy.deepcopy(child))
            elif label == "version": value["version"] = True
            elif label == "negative-zero": child["attributes"]["position"] = "-0"
            elif label == "large-int": child["attributes"]["position"] = str(2**63)
            # Preserve intentional malformed fields; only Go-style JSON shape
            # ordering here, never catalog_bytes which drops unknown members.
            return wire(value, sorted_keys=False)
        for label in ("orphan", "empty-parent", "missing", "unknown-attr", "missing-attr",
                      "dup", "version", "negative-zero", "large-int"):
            with self.subTest(label=label):
                self.refused(profile.bind_source_profile, *material(mutate(label), self.source_after))
        value = json.loads(self.source_before)
        value["objects"].reverse()
        self.refused(profile.bind_source_profile, *material(wire(value, sorted_keys=False), self.source_after))

    def test_wrong_complete_expected_catalog_with_coherent_hashes_refuses(self):
        for side in ("before", "after"):
            expected = json.loads(self.source_before if side == "before" else self.source_after)
            row = next(r for r in expected["objects"] if r["kind"] == "relations" and r["identity"] != "organizations")
            row["attributes"]["force_rls"] = "false" if row["attributes"]["force_rls"] == "true" else "true"
            wrong = catalog_bytes(expected)
            inputs = material(wrong if side == "before" else self.source_before,
                              wrong if side == "after" else self.source_after)
            self.refused(self.explain, self.bound(inputs), code="expected")

    def test_full_observed_catalog_and_security_not_just_hashes(self):
        for label in ("unrelated", "ownership", "unknown", "acl", "reciprocal", "slots"):
            before, after = copy.deepcopy(self.fixture["before"]), copy.deepcopy(self.fixture["after"])
            if label == "unrelated":
                row = next(r for r in after["snapshot"]["objects"] if r["kind"] == "relations" and r["identity"] != "organizations")
                row["attributes"]["force_rls"] = "false" if row["attributes"]["force_rls"] == "true" else "true"
                reseal_observation(after)
            elif label == "ownership": after["snapshot"]["ownership"]["migration_owner_owns_database"] = False
            elif label == "unknown": after["snapshot"]["unknown"] = [{"kind": "columns", "parent": "organizations", "count": 1}]
            elif label == "acl": after["graph"]["security"]["default_acl_rows"][0]["acl"][0]["grantable"] = True
            elif label == "reciprocal": after["graph"]["row_type"]["array"]["identity"] = "missing"
            elif label == "slots": after["graph"]["slots"][-1]["default_present"] = True
            with self.subTest(label=label):
                self.refused(self.explain, self.bound(), wire(before), wire(after))

    def test_manual_holders_and_post_binding_corruption_are_revalidated(self):
        self.refused(profile.BoundNullableProfile)
        self.refused(profile.NullableProfileExplanation)
        self.refused(profile.explain_profile, object.__new__(profile.BoundNullableProfile),
                     before=self.observed_before, after=self.observed_after)
        bound = self.bound()
        for key in bound.__slots__:
            self.refused(self.explain, forged(bound, **{key: b"sensitive-bad-input"}))
        descriptor = json.loads(bound._profile)
        descriptor["default_acl_contract_sha256"] = "0" * 64
        self.refused(self.explain, forged(bound, _profile=wire(descriptor)))
        result = self.explain(bound)
        favorable = json.loads(result._raw)
        favorable["semantic"]["after_catalog_sha256"] = "0" * 64
        self.refused(profile.canonical_explanation, forged(result, _raw=wire(favorable)), code="result")
        self.refused(profile.canonical_explanation, forged(result, _before=self.observed_after))
        self.refused(profile.canonical_explanation, object.__new__(profile.NullableProfileExplanation))
        self.refused(profile.canonical_explanation, favorable)

    def test_input_mutation_has_no_shared_nested_state(self):
        inputs = self.inputs()
        descriptor = json.loads(inputs[2].profile.raw)
        bound = self.bound(inputs)
        descriptor["renderer"] = "changed-after-bind"
        with self.assertRaises(FrozenInstanceError): inputs[0].catalog.version = 9
        with self.assertRaises(FrozenInstanceError): bound._profile = b"changed"
        self.assertEqual(json.loads(profile.canonical_explanation(self.explain(bound)))["profile"], profile.PROFILE)
        origin, candidate, source = inputs
        for mutable in (bytearray(source.operation.raw), memoryview(source.operation.raw)):
            self.refused(profile.bind_source_profile, origin, candidate,
                         replace(source, operation=replace(source.operation, raw=mutable)), code="bound")

    def test_per_slot_caps_reject_before_decoding_and_no_favorable_override(self):
        origin, candidate, source = self.inputs()
        for slot, limit in (("profile", profile.MAX_DESCRIPTOR), ("operation", 2048),
                            ("before_catalog", profile.MAX_DOCUMENT), ("after_catalog", profile.MAX_DOCUMENT)):
            self.refused(profile.bind_source_profile, origin, candidate,
                replace(source, **{slot: replace(getattr(source, slot), raw=b"x" * (limit + 1))}), code="bound")
        self.refused(self.explain, self.bound(), b"x" * (profile.MAX_DOCUMENT + 1), self.observed_after, code="bound")
        with self.assertRaises(TypeError):
            profile.explain_profile(self.bound(), before=self.observed_before,
                                    after=self.observed_after, compatible=True)

    def test_complete_overcap_documents_are_counted_in_every_role(self):
        # Every catalog is complete and below 4 MiB; real additional type
        # declarations make the source pair fit 8 MiB but source+observations
        # exceed it. No padding strings or deduplicated role accounting.
        def enlarged(raw):
            value = json.loads(raw)
            template = copy.deepcopy(next(r for r in value["objects"] if r["kind"] == "types"))
            for number in range(3000):
                row = copy.deepcopy(template)
                row["identity"] = "zz_profile_bound_" + str(number)
                value["objects"].append(row)
            return catalog_bytes(value)
        before, after = enlarged(self.source_before), enlarged(self.source_after)
        self.assertLessEqual(len(before), profile.MAX_DOCUMENT)
        self.assertLessEqual(len(after), profile.MAX_DOCUMENT)
        bound = self.bound(material(before, after))
        raw_total = sum(len(getattr(bound, key)) for key in ("_profile", "_operation", "_before", "_after"))
        self.assertLessEqual(raw_total, profile.MAX_TOTAL)
        self.assertGreater(raw_total + len(self.observed_before) + len(self.observed_after), profile.MAX_TOTAL)
        self.refused(self.explain, bound, code="bound")
        self.measurements["complete_overcap"] = {"source_bytes": raw_total,
            "with_observations_bytes": raw_total + len(self.observed_before) + len(self.observed_after),
            "expected_refusal": "nullable-profile:bound"}
        # The same immutable byte object in two roles is charged twice, even
        # before the smaller complete documents fail the source identity check.
        self.refused(self.explain, bound, self.observed_after, self.observed_after, code="bound")

    def test_error_does_not_echo_raw_paths_or_payloads(self):
        origin, candidate, source = self.inputs()
        marker = b"password=do-not-disclose-private-path"
        self.refused(profile.bind_source_profile, origin, candidate,
                     replace(source, profile=replace(source.profile, raw=marker)))

    def test_exact_types_not_subclasses_or_mutable_identity_objects(self):
        origin, candidate, source = self.inputs()
        class ForeignOrigin(profile.RetainedOrigin):
            pass
        self.refused(profile.bind_source_profile,
            ForeignOrigin(origin.release, origin.manifest_sha256, origin.source_sha, origin.catalog),
            candidate, source, code="identity")
        self.refused(profile.bind_source_profile, origin, candidate,
            {key: getattr(source, key) for key in source.__slots__}, code="source")
        self.refused(profile.bind_source_profile, replace(origin, catalog={"version": 0}), candidate, source)
        self.refused(profile.bind_source_profile, origin, candidate,
            replace(source, profile=replace(source.profile, source_sha="2" * 39)))

    def test_canonical_encoder_uses_go_escaping_and_holders_bind_metadata(self):
        # Paths are ASCII closed. Even legal relative punctuation must match
        # the exact blob path and cannot be laundered through holder metadata.
        bound = self.bound()
        sources = json.loads(bound._sources)
        sources["before_catalog"]["source_sha"] = H
        self.refused(self.explain, forged(bound, _sources=wire(sources)), code="source")
        sources = json.loads(bound._sources)
        sources["profile"]["bytes"] = True
        self.refused(self.explain, forged(bound, _sources=wire(sources)), code="source")
        after = json.loads(self.source_after)
        row = next(r for r in after["objects"] if r["kind"] == "types")
        self.assertIn("\\u003e", catalog_bytes(after).decode())
        self.assertIn(">", row["attributes"]["acl"])
        unescaped = catalog_bytes(after).replace(b"\\u003e", b">")
        self.refused(profile.bind_source_profile, *material(self.source_before, unescaped))


if __name__ == "__main__":
    unittest.main()
