"""Complete controlled source declarations, never observed graph fixtures.

The reviewed PG17 blueprint first matches the existing golden preimage hash.
Only then is the declared added slot derived. This is not a Go/DDL compiler or
an actual engine qualification. Source and production catalog orders differ.
"""
import copy
from dataclasses import FrozenInstanceError
import hashlib
import json
from pathlib import Path
import unittest
from unittest import mock

import schema_nullable as sn

ROOT = Path(__file__).resolve().parents[2]
BASE, HEAD = "1" * 40, "2" * 40
EMPTY = hashlib.sha256(b"").hexdigest()
GOLDEN = "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8"
PRODUCTION = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def wire(value, *, sort=True):
    text = json.dumps(value, sort_keys=sort, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    for char, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(char, escaped)
    return (text + "\n").encode()


def pg(value):
    return json.dumps(value, ensure_ascii=False, separators=(", ", ": "))


def catalog_bytes(value, *, version=None):
    rows = []
    for item in sorted(value["objects"], key=lambda r: (r["kind"], r["identity"])):
        row = {"kind": item["kind"], "identity": item["identity"]}
        if item.get("parent"):
            row["parent"] = item["parent"]
        row["attributes"] = dict(sorted(item["attributes"].items()))
        rows.append(row)
    return wire({"version": value["version"] if version is None else version, "objects": rows}, sort=False)


def bind_documents(declaration, before, after):
    result = copy.deepcopy(declaration)
    for label, raw in (("before", before), ("after", after)):
        value = json.loads(raw)
        result[label].update(bytes=len(raw), sha256=sha(raw), catalog_version=value["version"],
                             catalog_sha256=sha(catalog_bytes(value, version=0)))
    return result


def declared_catalogs(*, production=False):
    raw = (ROOT / "internal/dbcatalog/golden/catalog.json").read_bytes()
    if sha(raw) != GOLDEN:
        raise AssertionError("independent source golden changed")
    old = json.loads(raw)
    if catalog_bytes(old) != raw or len(old["objects"]) != 4432:
        raise AssertionError("independent source framing changed")
    fields = [[1, "id", "uuid", True, ""], [2, "created_at", "timestamp with time zone", False, ""],
        [3, "updated_at", "timestamp with time zone", False, ""], [4, "deleted_at", "timestamp with time zone", False, ""],
        [5, "reseller_id", "uuid", False, ""], [6, "name", "character varying(255)", True, '"default"'],
        [7, "slug", "character varying(100)", True, '"default"'], [8, "settings", "jsonb", False, ""]]
    row_type = ["c", "C", False, "-", "", -1, False, "d", "x",
        "record_in(cstring,oid,integer)", "record_out(record)", "-", fields, []]
    array_type = ["b", "A", False, "-", "", -1, False, "d", "x",
        "array_in(cstring,oid,integer)", "array_out(anyarray)", "public.organizations", [], []]
    rows = {(r["kind"], r["identity"]): r for r in old["objects"]}
    for name, definition, expected in (
            ("organizations", row_type, "63697f813c0cfa2b9ae24e6d604bda9d64b9851720c11a9963ba059b824bb1ea"),
            ("_organizations", array_type, "cd5dec2e31c91f2bec82a645cf7bcc1ebc7f2acf82ea54b64b9e400dc9767197")):
        if sha(pg(definition).encode()) != expected or rows["types", name]["attributes"]["definition_sha256"] != expected:
            raise AssertionError("independent old type blueprint mismatch")
    if production:
        profile_raw = (ROOT / "internal/dbcatalog/golden/production-v0.json").read_bytes()
        if sha(profile_raw) != "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d":
            raise AssertionError("independent production profile changed")
        seen = set()
        for delta in json.loads(profile_raw)["delta"]:
            key = delta["kind"], delta["identity"]
            attr = "position" if key[0] == "columns" else "definition_sha256"
            if (key in seen or key[0] not in ("columns", "types") or delta["disposition"] != "fix-bootstrap"
                    or set(delta["attributes"]) != {attr}):
                raise AssertionError("independent profile delta changed")
            seen.add(key)
            pair = delta["attributes"][attr]
            if set(pair) != {"golden", "production"} or rows[key]["attributes"][attr] != pair["golden"]:
                raise AssertionError("independent profile old-value mismatch")
            rows[key]["attributes"][attr] = pair["production"]
        if len(seen) != 72:
            raise AssertionError("independent profile inventory changed")
        fields.sort(key=lambda field: int(rows["columns", "organizations." + field[1]]["attributes"]["position"]))
        for number, field in enumerate(fields, 1):
            field[0] = number
        if (sha(pg(row_type).encode()) != "5eb86115e47f40c97e52705ae5134b2358e24fb98ee2725c859df61bc5148df4"
                or sha(catalog_bytes(old)) != PRODUCTION):
            raise AssertionError("independent production order mismatch")
    slots = []
    names = {"uuid": ("uuid", -1), "timestamp with time zone": ("timestamptz", -1),
             "character varying(255)": ("varchar", 259), "character varying(100)": ("varchar", 104), "jsonb": ("jsonb", -1)}
    for number, name, type_text, not_null, collation in fields:
        attrs = rows["columns", "organizations." + name]["attributes"]
        if (attrs["position"] != str(number) or attrs["type_sha256"] != sha(type_text.encode())
                or attrs["not_null"] != str(not_null).lower()):
            raise AssertionError("independent source column mismatch")
        type_name, typmod = names[type_text]
        slots.append({"number": number, "name": name, "type_namespace": "pg_catalog", "type_name": type_name,
            "type_modifier": typmod, "type_text": type_text, "not_null": not_null, "dropped": False,
            "local": True, "inherited_count": 0, "default_present": attrs["default_sha256"] != EMPTY,
            "identity": attrs["identity"], "generated": attrs["generated"], "acl": attrs["acl"],
            "collation_namespace": "pg_catalog" if collation else "", "collation_name": "default" if collation else ""})
    new = copy.deepcopy(old)
    new["version"] = 1
    new_type = copy.deepcopy(row_type)
    new_type[12].append([9, "extra", "bigint", False, ""])
    for item in new["objects"]:
        if (item["kind"], item["identity"]) == ("types", "organizations"):
            item["attributes"]["definition_sha256"] = sha(pg(new_type).encode())
    new["objects"].append({"kind": "columns", "identity": "organizations.extra", "parent": "organizations",
        "attributes": {"position": "9", "type_sha256": sha(b"bigint"), "not_null": "false", "default_sha256": EMPTY,
                       "identity": "", "generated": "", "collation_sha256": EMPTY, "acl": ""}})
    expected = ("a8f15ea159883aacdeecd3c55100f3b589f10ca18708ba113a393805cbf9948e" if production
                else "1a06e19765347268a83a80288f1d054b43caa72ae625f4c2e9b8a94a1d92ee77")
    if sha(catalog_bytes(new, version=0)) != expected:
        raise AssertionError("independent target mismatch")
    extra = {"number": 9, "name": "extra", "type_namespace": "pg_catalog", "type_name": "int8", "type_modifier": -1,
        "type_text": "bigint", "not_null": False, "dropped": False, "local": True, "inherited_count": 0,
        "default_present": False, "identity": "", "generated": "", "collation_namespace": "", "collation_name": "", "acl": ""}
    before, after = catalog_bytes(old), catalog_bytes(new)
    declaration = {"format_version": 1, "postgres_major": 17, "purpose": "nullable-source-shape",
        "operation": {"format_version": 1, "id": "add_extra", "kind": "add_nullable_column", "relation": "organizations",
                      "column": "extra", "type": "pg_catalog.int8", "nullable": True, "default": None}}
    for label, role, definition, inventory in (("before", "base", row_type, slots), ("after", "head", new_type, slots + [extra])):
        declaration[label] = {"role": role, "path": "internal/dbcatalog/golden/catalog.json", "row_definition_json": pg(definition),
            "array_definition_json": pg(array_type), "slots": inventory}
    return before, after, bind_documents(declaration, before, after)


class NullableSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.app = declared_catalogs()
        cls.production = declared_catalogs(production=True)

    def inputs(self, production=False):
        before, after, declaration = self.production if production else self.app
        return before, after, copy.deepcopy(declaration)

    def parse(self, before, after, declaration, **kwargs):
        return sn.classify_source(wire(declaration), before, after, base_sha=kwargs.get("base_sha", BASE), head_sha=kwargs.get("head_sha", HEAD))

    def refused(self, before, after, declaration, **kwargs):
        with self.assertRaisesRegex(sn.NullableSourceRefusal, "^nullable-source-invalid$"):
            self.parse(before, after, declaration, **kwargs)

    def test_complete_independent_source_and_production_pairs(self):
        for production in (False, True):
            with self.subTest(production=production):
                before, after, declaration = self.inputs(production)
                result = self.parse(before, after, declaration)
                self.assertEqual([(r.kind, r.identity) for r in result.explained], [("columns", "organizations.extra"), ("types", "organizations")])
                self.assertEqual(result.residual, ())
                self.assertEqual((result.base_sha, result.head_sha), (BASE, HEAD))
                self.assertEqual(result.statement_sha256, sha(b'ALTER TABLE ONLY "public"."organizations" ADD COLUMN "extra" pg_catalog.int8;\n'))

    def test_document_version_and_measurement_wire_are_distinct(self):
        before, after, declaration = self.inputs()
        self.assertEqual(declaration["after"]["catalog_version"], 1)
        self.assertNotEqual(declaration["after"]["sha256"], declaration["after"]["catalog_sha256"])
        self.parse(before, after, declaration)
        declaration["after"]["catalog_sha256"] = sha(after)
        self.refused(before, after, declaration)

    def test_residual_unrelated_type_not_explained(self):
        before, after, declaration = self.inputs()
        value = json.loads(after)
        row = next(r for r in value["objects"] if r["kind"] == "types" and r["identity"] == "users")
        row["attributes"]["definition_sha256"] = "a" * 64
        after = catalog_bytes(value)
        result = self.parse(before, after, bind_documents(declaration, before, after))
        self.assertEqual([(r.kind, r.identity) for r in result.residual], [("types", "users")])

    def test_source_identity_strict(self):
        for key in ("base_sha", "head_sha"):
            for value in (None, True, "MAIN", "1" * 39, "A" * 40):
                with self.subTest(key=key, value=value):
                    self.refused(*self.inputs(), **{key: value})

    def test_one_source_cannot_claim_two_different_catalogs(self):
        self.refused(*self.inputs(), head_sha=BASE)

    def test_closed_top_level_fields_and_numeric_types(self):
        for key, value in (("format_version", True), ("format_version", 2), ("postgres_major", True),
                           ("postgres_major", 16), ("purpose", "observation"), ("extra", False)):
            before, after, declaration = self.inputs()
            declaration[key] = value
            with self.subTest(key=key, value=value): self.refused(before, after, declaration)
        for key in self.inputs()[2]:
            before, after, declaration = self.inputs(); del declaration[key]
            with self.subTest(missing=key): self.refused(before, after, declaration)

    def test_endpoint_roles_paths_sizes_hashes_and_versions(self):
        for side in ("before", "after"):
            for key, value in (("role", "observation"), ("path", "../catalog.json"), ("bytes", True), ("bytes", 1),
                               ("sha256", "f" * 64), ("catalog_sha256", "0" * 64), ("catalog_version", True), ("catalog_version", 9)):
                before, after, declaration = self.inputs(); declaration[side][key] = value
                with self.subTest(side=side, key=key): self.refused(before, after, declaration)

    def test_swapped_complete_endpoints_and_profile_mismatch(self):
        before, after, declaration = self.inputs()
        self.refused(after, before, declaration)
        pbefore, pafter, _ = self.inputs(True)
        self.refused(pbefore, pafter, bind_documents(declaration, pbefore, pafter))

    def test_noop_complete_after_is_not_a_transition(self):
        before, _, declaration = self.inputs()
        declaration["after"] = copy.deepcopy(declaration["before"]); declaration["after"]["role"] = "head"
        self.refused(before, before, bind_documents(declaration, before, before))

    def test_wrong_or_nonnullable_operation(self):
        for key, value in (("relation", "users"), ("column", "other"), ("type", "pg_catalog.text"),
                           ("nullable", False), ("nullable", 1), ("default", 1), ("kind", "rename"), ("extra", [])):
            before, after, declaration = self.inputs(); declaration["operation"][key] = value
            with self.subTest(key=key): self.refused(before, after, declaration)

    def test_missing_duplicate_and_reordered_slots(self):
        for side in ("before", "after"):
            for mode in ("missing", "duplicate", "reverse"):
                before, after, declaration = self.inputs(); slots = declaration[side]["slots"]
                if mode == "missing": slots.pop(0)
                elif mode == "duplicate": slots[1] = copy.deepcopy(slots[0])
                else: slots.reverse()
                with self.subTest(side=side, mode=mode): self.refused(before, after, declaration)

    def test_slot_catalog_bindings(self):
        for key, value in (("number", True), ("name", "other"), ("type_text", "integer"), ("not_null", False),
                           ("default_present", False), ("dropped", True), ("local", False), ("inherited_count", 1),
                           ("identity", "a"), ("generated", "s"), ("collation_name", "default"), ("acl", "OTHER>PUBLIC:USAGE:false")):
            before, after, declaration = self.inputs(); declaration["before"]["slots"][0][key] = value
            with self.subTest(key=key): self.refused(before, after, declaration)

    def test_new_slot_fixed_shape(self):
        for key, value in (("type_namespace", "public"), ("type_name", "oid"), ("type_modifier", 4), ("not_null", True),
                           ("default_present", True), ("identity", "d"), ("generated", "s")):
            before, after, declaration = self.inputs(); declaration["after"]["slots"][-1][key] = value
            with self.subTest(key=key): self.refused(before, after, declaration)

    def test_preimages_not_opaque_hash_claims(self):
        for side in ("before", "after"):
            for key in ("row_definition_json", "array_definition_json"):
                before, after, declaration = self.inputs(); declaration[side][key] += " "
                with self.subTest(side=side, key=key): self.refused(before, after, declaration)

    def test_coherent_wrong_array_element_refuses(self):
        before, after, declaration = self.inputs()
        values = [json.loads(before), json.loads(after)]
        for side, value in zip(("before", "after"), values):
            definition = json.loads(declaration[side]["array_definition_json"]); definition[11] = "public.users"
            declaration[side]["array_definition_json"] = pg(definition)
            next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("types", "_organizations"))["attributes"]["definition_sha256"] = sha(pg(definition).encode())
        before, after = map(catalog_bytes, values)
        self.refused(before, after, bind_documents(declaration, before, after))

    def test_coherent_row_header_change_refuses(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        definition = json.loads(declaration["after"]["row_definition_json"]); definition[7] = "i"
        declaration["after"]["row_definition_json"] = pg(definition)
        next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("types", "organizations"))["attributes"]["definition_sha256"] = sha(pg(definition).encode())
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_old_column_change_with_coherent_document_refuses(self):
        for attr in ("default_sha256", "type_sha256", "collation_sha256"):
            before, after, declaration = self.inputs(); value = json.loads(after)
            next(r for r in value["objects"] if r["identity"] == "organizations.name")["attributes"][attr] = "f" * 64
            after = catalog_bytes(value)
            with self.subTest(attr=attr): self.refused(before, after, bind_documents(declaration, before, after))

    def test_row_array_and_relation_security_changes_refuse(self):
        for kind, name in (("types", "organizations"), ("types", "_organizations"), ("relations", "organizations")):
            before, after, declaration = self.inputs(); value = json.loads(after)
            next(r for r in value["objects"] if (r["kind"], r["identity"]) == (kind, name))["attributes"]["acl"] = ""
            after = catalog_bytes(value)
            with self.subTest(name=name): self.refused(before, after, bind_documents(declaration, before, after))

    def test_complete_catalog_malformed_and_missing_objects(self):
        for mutation in ("orphan", "missing-array", "missing-column", "duplicate", "unknown-attribute"):
            before, after, declaration = self.inputs(); value = json.loads(after)
            if mutation == "orphan": next(r for r in value["objects"] if r["identity"] == "organizations.extra")["parent"] = "absent"
            elif mutation == "missing-array": value["objects"] = [r for r in value["objects"] if r["identity"] != "_organizations"]
            elif mutation == "missing-column": value["objects"] = [r for r in value["objects"] if r["identity"] != "organizations.id"]
            elif mutation == "duplicate": value["objects"].append(copy.deepcopy(value["objects"][0]))
            else: value["objects"][0]["attributes"]["unknown"] = "x"
            after = catalog_bytes(value)
            with self.subTest(mutation=mutation): self.refused(before, after, bind_documents(declaration, before, after))

    def test_numeric_spelling_and_order_do_not_hide_in_document_hash(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        next(r for r in value["objects"] if r["identity"] == "organizations.extra")["attributes"]["position"] = "09"
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_canonical_declaration_duplicate_keys_and_bad_encoding(self):
        before, after, declaration = self.inputs()
        for raw in (wire(declaration).rstrip(), b'{}\n', b'\xff', b'{"format_version":1,"format_version":1}\n', b'{"format_version":NaN}\n'):
            with self.subTest(raw=raw[:25]), self.assertRaises(sn.NullableSourceRefusal):
                sn.classify_source(raw, before, after, base_sha=BASE, head_sha=HEAD)

    def test_every_input_bound_before_decode(self):
        before, after, declaration = self.inputs()
        for index in range(3):
            raws = [wire(declaration), before, after]; raws[index] = b" " * (sn.MAX_DOCUMENT+1)
            with self.subTest(index=index), mock.patch.object(sn.wire, "_load", side_effect=AssertionError("decoded overcap")):
                with self.assertRaises(sn.NullableSourceRefusal): sn.classify_source(*raws, base_sha=BASE, head_sha=HEAD)

    def test_aggregate_counts_repeated_full_slots_before_decode(self):
        raw = b" " * (3 << 20)
        with mock.patch.object(sn.wire, "_load", side_effect=AssertionError("decoded aggregate")):
            with self.assertRaises(sn.NullableSourceRefusal): sn.classify_source(raw, raw, raw, base_sha=BASE, head_sha=HEAD)

    def test_complete_coherently_bound_documents_over_aggregate_cap(self):
        before, after, declaration = self.inputs()
        before = b" " * (sn.MAX_DOCUMENT - len(before)) + before
        after = b" " * (sn.MAX_DOCUMENT - len(after)) + after
        declaration = bind_documents(declaration, before, after)
        self.assertEqual((len(before), len(after)), (sn.MAX_DOCUMENT, sn.MAX_DOCUMENT))
        # Complete valid catalog JSON is permitted to have ordinary whitespace;
        # the two catalogs plus their bound declaration exceed the total cap.
        with mock.patch.object(sn.wire, "_load", side_effect=AssertionError("decoded full overcap")):
            with self.assertRaises(sn.NullableSourceRefusal):
                sn.classify_source(wire(declaration), before, after, base_sha=BASE, head_sha=HEAD)

    def test_output_and_inputs_cannot_be_used_as_authority(self):
        before, after, declaration = self.inputs(); result = self.parse(before, after, declaration)
        with self.assertRaises(FrozenInstanceError): result.residual = ()
        declaration["after"]["slots"][-1]["not_null"] = True
        self.refused(before, after, declaration)
        self.assertEqual(result.after_sha256, sha(after))
        with self.assertRaises(sn.NullableSourceRefusal): sn.classify_source(result, before, after, base_sha=BASE, head_sha=HEAD)

    def test_second_added_column_is_not_explained(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        column = copy.deepcopy(next(r for r in value["objects"] if r["identity"] == "organizations.extra"))
        column["identity"] = "organizations.other"; column["attributes"]["position"] = "10"; value["objects"].append(column)
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_no_input_content_in_errors(self):
        before, after, declaration = self.inputs(); declaration["secret-private-marker"] = "private-value"
        with self.assertRaises(sn.NullableSourceRefusal) as caught: self.parse(before, after, declaration)
        self.assertEqual(str(caught.exception), "nullable-source-invalid")


# Source-derived expectations. The exact typed-literal expression template was
# precommitted and then matched all18 PG17 cases (independent review3c669119).
# No observation/fixture graph is read to construct these complete catalogs.
CONSTANT_DECIMALS = ("-9223372036854775808", "-2147483649", "-2147483648", "-1", "0", "1",
                     "2147483647", "2147483648", "9223372036854775807")


def constant_catalogs(*, production=False, decimal="1", nullable=True):
    before, after, declaration = declared_catalogs(production=production)
    return constant_variant(before, after, declaration, decimal=decimal, nullable=nullable)


def constant_variant(before, after, declaration, *, decimal="1", nullable=True):
    # Both inputs come from the independently checked source-only blueprint.
    # This transformation defines expected source intent, never measured output.
    declaration = copy.deepcopy(declaration)
    value = json.loads(after)
    expression = "'" + decimal + "'::bigint"
    definition = json.loads(declaration["after"]["row_definition_json"])
    definition[12][-1][3] = not nullable
    declaration["after"]["row_definition_json"] = pg(definition)
    declaration["after"]["slots"][-1].update(not_null=not nullable, default_present=True)
    for row in value["objects"]:
        if (row["kind"], row["identity"]) == ("types", "organizations"):
            row["attributes"]["definition_sha256"] = sha(pg(definition).encode())
        if (row["kind"], row["identity"]) == ("columns", "organizations.extra"):
            row["attributes"].update(not_null=str(not nullable).lower(), default_sha256=sha(expression.encode()))
    declaration["purpose"] = "int8-constant-column-source-shape"
    declaration["operation"].update(kind="add_int8_constant_column", nullable=nullable,
                                      default={"kind": "int8-literal", "decimal": decimal})
    declaration["default_expression"] = expression
    after = catalog_bytes(value)
    return before, after, bind_documents(declaration, before, after)


class ConstantSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.app = declared_catalogs()
        cls.production = declared_catalogs(production=True)

    def inputs(self, *, production=False, decimal="1", nullable=True):
        return constant_variant(*(self.production if production else self.app), decimal=decimal, nullable=nullable)

    def parse(self, before, after, declaration, **kwargs):
        return sn.classify_constant_source(wire(declaration), before, after,
            base_sha=kwargs.get("base_sha", BASE), head_sha=kwargs.get("head_sha", HEAD))

    def refused(self, before, after, declaration, **kwargs):
        with self.assertRaisesRegex(sn.ConstantSourceRefusal, "^constant-source-invalid$"):
            self.parse(before, after, declaration, **kwargs)

    def test_all18_prequalified_literal_and_nullability_vectors(self):
        for decimal in CONSTANT_DECIMALS:
            for nullable in (True, False):
                with self.subTest(decimal=decimal, nullable=nullable):
                    before, after, declaration = self.inputs(decimal=decimal, nullable=nullable)
                    result = self.parse(before, after, declaration)
                    expected = ('ALTER TABLE ONLY "public"."organizations" ADD COLUMN "extra" pg_catalog.int8 DEFAULT '
                        + "'" + decimal + "'::pg_catalog.int8" + ("" if nullable else " NOT NULL") + ";\n").encode()
                    self.assertEqual(result.statement_sha256, sha(expected))
                    self.assertEqual(result.operation_sha256, sha(wire(declaration["operation"])))
                    self.assertEqual(result.residual, ())
                    self.assertEqual([(x.kind, x.identity) for x in result.explained],
                                     [("columns", "organizations.extra"), ("types", "organizations")])

    def test_production_order_independently_derived_before_target(self):
        for nullable in (True, False):
            before, after, declaration = self.inputs(production=True, decimal="-1", nullable=nullable)
            self.assertEqual(sha(before), PRODUCTION)
            self.assertEqual(self.parse(before, after, declaration).before_sha256, PRODUCTION)

    def test_source_identity_exact_and_distinct(self):
        for key in ("base_sha", "head_sha"):
            for value in (None, True, "main", "a" * 39, "A" * 40):
                with self.subTest(key=key, value=value): self.refused(*self.inputs(), **{key: value})
        self.refused(*self.inputs(), head_sha=BASE)

    def test_closed_top_level_version_purpose_and_no_privilege_fields(self):
        for key, value in (("format_version", True), ("format_version", 2), ("postgres_major", 18),
                           ("postgres_major", True), ("purpose", "nullable-source-shape"), ("approved", True)):
            before, after, declaration = self.inputs(); declaration[key] = value
            with self.subTest(key=key, value=value): self.refused(before, after, declaration)
        for key in self.inputs()[2]:
            before, after, declaration = self.inputs(); del declaration[key]
            with self.subTest(missing=key): self.refused(before, after, declaration)

    def test_exact_operation_shape_no_alternate_sql(self):
        for key, value in (("format_version", True), ("id", "bad id"), ("kind", "add_nullable_column"),
                ("relation", "users"), ("column", "other"), ("type", "pg_catalog.int4"), ("nullable", 1),
                ("nullable", None), ("sql", "ALTER TABLE private"), ("concurrently", True)):
            before, after, declaration = self.inputs(); declaration["operation"][key] = value
            with self.subTest(key=key, value=value): self.refused(before, after, declaration)
        for key in self.inputs()[2]["operation"]:
            before, after, declaration = self.inputs(); del declaration["operation"][key]
            with self.subTest(missing=key): self.refused(before, after, declaration)

    def test_literal_grammar_rejects_expressions_and_noncanonical_numbers(self):
        for value in (None, True, 1, 1.0, "", "+1", "-0", "01", "-01", " 1", "1 ", "1\n", "1.0", "1e0",
                      "\uff11", "NULL", "1+0", "nextval('x')", "'1'", "1::int8", "9223372036854775808",
                      "-9223372036854775809", "9" * 100):
            before, after, declaration = self.inputs(); declaration["operation"]["default"]["decimal"] = value
            with self.subTest(value=value): self.refused(before, after, declaration)

    def test_default_object_requires_exact_literal_tag_and_fields(self):
        for default in (None, True, "1", {}, {"kind": "expression", "decimal": "1"},
                        {"kind": "int8-literal"}, {"kind": "int8-literal", "decimal": "1", "sql": "1"}):
            before, after, declaration = self.inputs(); declaration["operation"]["default"] = default
            with self.subTest(default=default): self.refused(before, after, declaration)

    def test_coherently_hashed_alternate_expression_is_not_a_constant_preimage(self):
        for expression in ("1::bigint", "'1'::pg_catalog.int8", "'1'::bigint ", "'1'::bigint\n", "(1 + 0)", "NULL::bigint"):
            before, after, declaration = self.inputs(); value = json.loads(after)
            declaration["default_expression"] = expression
            next(r for r in value["objects"] if r["identity"] == "organizations.extra")["attributes"]["default_sha256"] = sha(expression.encode())
            after = catalog_bytes(value)
            with self.subTest(expression=expression): self.refused(before, after, bind_documents(declaration, before, after))

    def test_declared_decimal_must_match_expression_not_only_type(self):
        before, after, declaration = self.inputs(); declaration["operation"]["default"]["decimal"] = "2"
        self.refused(before, after, declaration)

    def test_missing_or_wrong_default_catalog_hash_refuses(self):
        for digest in (EMPTY, sha(b"'2'::bigint"), "f" * 64):
            before, after, declaration = self.inputs(); value = json.loads(after)
            next(r for r in value["objects"] if r["identity"] == "organizations.extra")["attributes"]["default_sha256"] = digest
            after = catalog_bytes(value)
            with self.subTest(digest=digest): self.refused(before, after, bind_documents(declaration, before, after))

    def test_nullable_intent_must_match_complete_slot_column_and_row(self):
        for nullable in (True, False):
            before, after, declaration = self.inputs(nullable=nullable)
            declaration["operation"]["nullable"] = not nullable
            with self.subTest(nullable=nullable): self.refused(before, after, declaration)

    def test_endpoint_identity_length_and_measurement_version_are_bound(self):
        for side in ("before", "after"):
            for key, value in (("role", "observation"), ("path", "../catalog"), ("bytes", True), ("bytes", 1),
                               ("sha256", "f" * 64), ("catalog_sha256", "0" * 64), ("catalog_version", True), ("catalog_version", 8)):
                before, after, declaration = self.inputs(); declaration[side][key] = value
                with self.subTest(side=side, key=key): self.refused(before, after, declaration)
        before, after, declaration = self.inputs()
        self.assertNotEqual(declaration["after"]["sha256"], declaration["after"]["catalog_sha256"])
        self.parse(before, after, declaration)

    def test_swapped_profile_and_noop_endpoints_refuse(self):
        before, after, declaration = self.inputs(); self.refused(after, before, declaration)
        pbefore, pafter, _ = self.inputs(production=True)
        self.refused(pbefore, pafter, bind_documents(declaration, pbefore, pafter))
        declaration["after"] = copy.deepcopy(declaration["before"]); declaration["after"]["role"] = "head"
        self.refused(before, before, bind_documents(declaration, before, before))

    def test_complete_slot_inventory_and_fixed_new_shape(self):
        for mode in ("missing", "duplicate", "reverse", "default-absent", "identity", "generated", "type"):
            before, after, declaration = self.inputs(); slots = declaration["after"]["slots"]
            if mode == "missing": slots.pop(0)
            elif mode == "duplicate": slots[1] = copy.deepcopy(slots[0])
            elif mode == "reverse": slots.reverse()
            elif mode == "default-absent": slots[-1]["default_present"] = False
            elif mode == "identity": slots[-1]["identity"] = "a"
            elif mode == "generated": slots[-1]["generated"] = "s"
            else: slots[-1]["type_name"] = "int4"
            with self.subTest(mode=mode): self.refused(before, after, declaration)

    def test_existing_default_change_is_never_absorbed(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        next(r for r in value["objects"] if r["identity"] == "organizations.id")["attributes"]["default_sha256"] = sha(b"private()")
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_existing_nullability_change_even_coherent_preimage_refuses(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        definition = json.loads(declaration["after"]["row_definition_json"]); definition[12][0][3] = False
        declaration["after"]["row_definition_json"] = pg(definition)
        declaration["after"]["slots"][0]["not_null"] = False
        next(r for r in value["objects"] if r["identity"] == "organizations.id")["attributes"]["not_null"] = "false"
        next(r for r in value["objects"] if r["identity"] == "organizations" and r["kind"] == "types")["attributes"]["definition_sha256"] = sha(pg(definition).encode())
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_security_and_opaque_type_changes_not_explained(self):
        for kind, name in (("relations", "organizations"), ("types", "organizations"), ("types", "_organizations")):
            before, after, declaration = self.inputs(); value = json.loads(after)
            next(r for r in value["objects"] if (r["kind"], r["identity"]) == (kind, name))["attributes"]["acl"] = ""
            after = catalog_bytes(value)
            with self.subTest(kind=kind, name=name): self.refused(before, after, bind_documents(declaration, before, after))

    def test_wrong_array_preimage_coherently_bound_refuses(self):
        before, after, declaration = self.inputs(); values = [json.loads(before), json.loads(after)]
        for side, value in zip(("before", "after"), values):
            definition = json.loads(declaration[side]["array_definition_json"]); definition[11] = "public.users"
            declaration[side]["array_definition_json"] = pg(definition)
            next(r for r in value["objects"] if r["identity"] == "_organizations")["attributes"]["definition_sha256"] = sha(pg(definition).encode())
        before, after = map(catalog_bytes, values)
        self.refused(before, after, bind_documents(declaration, before, after))

    def test_unrelated_changed_object_is_complete_residual(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        next(r for r in value["objects"] if r["identity"] == "users" and r["kind"] == "types")["attributes"]["definition_sha256"] = "f" * 64
        after = catalog_bytes(value); result = self.parse(before, after, bind_documents(declaration, before, after))
        self.assertEqual([(r.kind, r.identity) for r in result.residual], [("types", "users")])

    def test_malformed_complete_catalogs_and_declaration_canonicality(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        value["objects"].append(copy.deepcopy(value["objects"][0])); after = catalog_bytes(value)
        self.refused(before, after, bind_documents(declaration, before, after))
        before, after, declaration = self.inputs()
        for raw in (wire(declaration).rstrip(), b'{}\n', b'\xff', b'{"format_version":1,"format_version":1}\n'):
            with self.subTest(raw=raw[:20]), self.assertRaises(sn.ConstantSourceRefusal):
                sn.classify_constant_source(raw, before, after, base_sha=BASE, head_sha=HEAD)

    def test_each_document_cap_before_decoding(self):
        before, after, declaration = self.inputs()
        for index in range(3):
            raws = [wire(declaration), before, after]; raws[index] = b" " * (sn.MAX_DOCUMENT + 1)
            with self.subTest(index=index), mock.patch.object(sn.wire, "_load", side_effect=AssertionError("overcap decoded")):
                with self.assertRaises(sn.ConstantSourceRefusal): sn.classify_constant_source(*raws, base_sha=BASE, head_sha=HEAD)

    def test_full_bound_documents_over_shared_cap_refuse_before_decode(self):
        before, after, declaration = self.inputs()
        before = b" " * (sn.MAX_DOCUMENT - len(before)) + before
        after = b" " * (sn.MAX_DOCUMENT - len(after)) + after
        declaration = bind_documents(declaration, before, after)
        with mock.patch.object(sn.wire, "_load", side_effect=AssertionError("overcap decoded")):
            with self.assertRaises(sn.ConstantSourceRefusal):
                sn.classify_constant_source(wire(declaration), before, after, base_sha=BASE, head_sha=HEAD)

    def test_input_mutation_and_forged_output_are_not_authority(self):
        before, after, declaration = self.inputs(); result = self.parse(before, after, declaration)
        with self.assertRaises(FrozenInstanceError): result.residual = ()
        declaration["operation"]["default"]["decimal"] = "2"; self.refused(before, after, declaration)
        self.assertEqual(result.after_sha256, sha(after))
        with self.assertRaises(sn.ConstantSourceRefusal): sn.classify_constant_source(result, before, after, base_sha=BASE, head_sha=HEAD)

    def test_no_input_content_in_error(self):
        before, after, declaration = self.inputs(); declaration["private-marker"] = "sensitive-value"
        with self.assertRaises(sn.ConstantSourceRefusal) as caught: self.parse(before, after, declaration)
        self.assertEqual(str(caught.exception), "constant-source-invalid")

    def test_second_added_column_or_operation_refuses(self):
        before, after, declaration = self.inputs(); declaration["operation"] = [declaration["operation"]]
        self.refused(before, after, declaration)
        before, after, declaration = self.inputs(); value = json.loads(after)
        column = copy.deepcopy(next(r for r in value["objects"] if r["identity"] == "organizations.extra"))
        column["identity"] = "organizations.other"; column["attributes"]["position"] = "10"; value["objects"].append(column)
        after = catalog_bytes(value); self.refused(before, after, bind_documents(declaration, before, after))

    def test_normal_nullable_and_constant_parsers_never_cross_accept(self):
        before, after, declaration = self.inputs()
        with self.assertRaises(sn.NullableSourceRefusal): sn.classify_source(wire(declaration), before, after, base_sha=BASE, head_sha=HEAD)
        before, after, declaration = self.app
        with self.assertRaises(sn.ConstantSourceRefusal): sn.classify_constant_source(wire(declaration), before, after, base_sha=BASE, head_sha=HEAD)


if __name__ == "__main__":
    unittest.main()
