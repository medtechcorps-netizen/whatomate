"""Complete source blueprints and adversarial planned-index declarations.

The old application/production hashes are checked by the existing independent
source fixture. New index intent follows the fixed catalog query array; these
tests neither execute CREATE INDEX nor establish concurrency at runtime.
"""
import copy
from dataclasses import FrozenInstanceError
import json
import unittest
from unittest import mock

import schema_index as si
import test_ship_schema_nullable as columns

BASE, HEAD = columns.BASE, columns.HEAD
wire, sha, catalog_bytes = columns.wire, columns.sha, columns.catalog_bytes
RELATION, COLUMN, INDEX = "agent_transfers", "escalation_level", "ix_source_escalation"


def bind_documents(declaration, before, after):
    return columns.bind_documents(declaration, before, after)


def add_index(before, after, *, relation=RELATION, column=COLUMN, index=INDEX):
    """Add one declared index to an independently supplied complete source pair."""
    target = json.loads(after)
    target["version"] = max(json.loads(before)["version"] + 1, target["version"])
    definition = columns.pg(["btree", 1, 1, False, True, False, "",
                            [[1, column, "pg_catalog", "int8_ops", "", "", 0]]])
    target["objects"].append({"kind": "indexes", "identity": relation + "." + index,
        "parent": relation, "attributes": {"definition_sha256": sha(definition.encode()),
        "valid": "true", "ready": "true", "live": "true", "unique": "false", "primary": "false", "exclusion": "false"}})
    raw = catalog_bytes(target)
    declaration = {"format_version": 1, "postgres_major": 17, "purpose": "concurrent-index-source-shape",
        "operation": {"format_version": 1, "id": "add_escalation_index", "kind": "create_nonunique_index_concurrently",
            "schema": "public", "relation": relation, "index": index, "column": column,
            "method": "btree", "operator_class": "pg_catalog.int8_ops", "order": "asc", "nulls": "last",
            "concurrently": True, "unique": False, "include": [], "predicate": None},
        "index_definition_json": definition,
        "before": {"role": "base", "path": si.CATALOG_PATH},
        "after": {"role": "head", "path": si.CATALOG_PATH}}
    return raw, bind_documents(declaration, before, raw)


def declared_catalogs(*, production=False):
    before, _unused_column_target, _unused_column_declaration = columns.declared_catalogs(production=production)
    after, declaration = add_index(before, before)
    return before, after, declaration


def compact_pair(full):
    """Keep a structurally closed source subset for cheap malformed-wire cases."""
    before, _after, _declaration = full
    value = json.loads(before)
    value["objects"] = [row for row in value["objects"]
        if (row["kind"] == "relations" and row["identity"] == RELATION)
        or (row["kind"] == "types" and row["identity"] in (RELATION, "_" + RELATION))
        or (row["kind"] == "columns" and row.get("parent") == RELATION)]
    raw = catalog_bytes(value)
    after, declaration = add_index(raw, raw)
    return raw, after, declaration


class IndexSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.app = declared_catalogs()
        cls.production = declared_catalogs(production=True)
        cls.small = compact_pair(cls.app)

    def inputs(self, *, full=False, production=False):
        return copy.deepcopy(self.production if production else self.app if full else self.small)

    def parse(self, before, after, declaration, **changes):
        return si.classify_source(wire(declaration), before, after,
            base_sha=changes.get("base_sha", BASE), head_sha=changes.get("head_sha", HEAD))

    def refused(self, before, after, declaration, **changes):
        with self.assertRaisesRegex(si.IndexSourceRefusal, "^index-source-invalid$"):
            self.parse(before, after, declaration, **changes)

    def object_change(self, pair, kind, identity, changes, *, both=False):
        before, after, declaration = pair
        values = [json.loads(before), json.loads(after)]
        for value in values if both else values[1:]:
            item = next(row for row in value["objects"] if (row["kind"], row["identity"]) == (kind, identity))
            item["attributes"].update(changes)
        before, after = map(catalog_bytes, values)
        return before, after, bind_documents(declaration, before, after)

    def definition_change(self, pair, change):
        before, after, declaration = pair
        value = json.loads(declaration["index_definition_json"])
        change(value)
        declaration["index_definition_json"] = columns.pg(value)
        pair = self.object_change((before, after, declaration), "indexes", RELATION + "." + INDEX,
            {"definition_sha256": sha(declaration["index_definition_json"].encode())})
        return pair

    def test_full_independent_application_and_production_catalogs(self):
        for production in (False, True):
            with self.subTest(production=production):
                before, after, declaration = self.inputs(full=True, production=production)
                value = self.parse(before, after, declaration)
                self.assertEqual(len(json.loads(before)["objects"]), 4432)
                self.assertEqual(len(json.loads(after)["objects"]), 4433)
                self.assertEqual(sha(before), columns.PRODUCTION if production else columns.GOLDEN)
                self.assertEqual([(r.kind, r.identity, r.parent) for r in value.explained],
                    [("indexes", RELATION + "." + INDEX, RELATION)])
                self.assertEqual(value.residual, ())
                self.assertEqual((value.base_sha, value.head_sha), (BASE, HEAD))
                expected = b'CREATE INDEX CONCURRENTLY "ix_source_escalation" ON "public"."agent_transfers" USING btree ("escalation_level" pg_catalog.int8_ops ASC NULLS LAST);\n'
                self.assertEqual(value.statement_sha256, sha(expected))
                self.assertEqual(value.operation_sha256, sha(wire(declaration["operation"])))

    def test_source_versions_and_normalized_catalog_hash_are_distinct(self):
        before, after, declaration = self.inputs()
        self.assertEqual(declaration["after"]["catalog_version"], 1)
        self.assertNotEqual(declaration["after"]["sha256"], declaration["after"]["catalog_sha256"])
        declaration["after"]["catalog_sha256"] = sha(after)
        self.refused(before, after, declaration)

    def test_output_frozen_and_no_runtime_or_authority_fields(self):
        value = self.parse(*self.inputs())
        with self.assertRaises(FrozenInstanceError):
            value.residual = ()
        self.assertEqual(tuple(value.__dataclass_fields__), tuple(si.nullable.SourceNullableDelta.__dataclass_fields__))
        for name in ("concurrent", "executed", "verified", "observation", "old_server_ok"):
            self.assertFalse(hasattr(value, name))
        with self.assertRaisesRegex(si.IndexSourceRefusal, "^index-source-invalid$"):
            si.classify_source(value, *self.inputs()[:2], base_sha=BASE, head_sha=HEAD)

    def test_source_sha_exact_and_distinct(self):
        for name in ("base_sha", "head_sha"):
            for value in (True, None, "MAIN", "a" * 39, "A" * 40):
                with self.subTest(name=name, value=value):
                    self.refused(*self.inputs(), **{name: value})
        self.refused(*self.inputs(), head_sha=BASE)

    def test_top_level_and_endpoint_closed_wire(self):
        for key, value in (("format_version", True), ("format_version", 2), ("postgres_major", True),
                           ("postgres_major", 16), ("purpose", "observed"), ("verified", True)):
            with self.subTest(key=key, value=value):
                before, after, declaration = self.inputs(); declaration[key] = value
                self.refused(before, after, declaration)
        for role in ("before", "after"):
            for key, value in (("role", "candidate"), ("path", "../catalog.json"), ("bytes", True),
                ("bytes", 1), ("sha256", "f" * 64), ("catalog_version", True),
                ("catalog_version", 9), ("catalog_sha256", "f" * 64), ("extra", "PRIVATE")):
                with self.subTest(role=role, key=key):
                    before, after, declaration = self.inputs(); declaration[role][key] = value
                    self.refused(before, after, declaration)

    def test_operation_requires_exact_concurrent_nonunique_closed_subset(self):
        variants = {"format_version": (True, 2), "id": ("", "../bad", "x" * 65),
            "kind": ("create_index",), "schema": ("private",), "relation": ("a.b",),
            "index": ('x";DROP',), "column": ("escalation_level + 1",), "method": ("hash",),
            "operator_class": ("public.int8_ops",), "order": ("desc",), "nulls": ("first",),
            "concurrently": (False, 1, "true"), "unique": (True, 0), "include": (["id"], None),
            "predicate": ("TRUE", False), "statement": ("CREATE INDEX CONCURRENTLY ...",),
            "tablespace": ("pg_default",), "if_not_exists": (True,), "options": ([],)}
        for key, values in variants.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    before, after, declaration = self.inputs(); declaration["operation"][key] = value
                    self.refused(before, after, declaration)
        before, after, declaration = self.inputs(); del declaration["operation"]["concurrently"]
        self.refused(before, after, declaration)

    def test_definition_preimage_is_raw_hash_bound(self):
        before, after, declaration = self.inputs()
        declaration["index_definition_json"] = json.dumps(json.loads(declaration["index_definition_json"]), separators=(",", ":"))
        self.refused(before, after, declaration)
        pair = self.object_change((before, after, declaration), "indexes", RELATION + "." + INDEX,
            {"definition_sha256": sha(declaration["index_definition_json"].encode())})
        self.parse(*pair)

    def test_definition_semantics_reject_even_with_recomputed_hash(self):
        for position, value in ((0, "hash"), (1, 2), (2, 0), (3, True), (4, False),
                                (5, True), (6, "escalation_level > 0")):
            with self.subTest(position=position):
                self.refused(*self.definition_change(self.inputs(), lambda row: row.__setitem__(position, value)))
        for position, value in ((0, 2), (1, "(escalation_level + 1)"), (2, "public"),
                                (3, "int4_ops"), (4, "pg_catalog"), (5, "default"), (6, 1)):
            with self.subTest(key_position=position):
                self.refused(*self.definition_change(self.inputs(), lambda row: row[7][0].__setitem__(position, value)))
        self.refused(*self.definition_change(self.inputs(), lambda row: row[7].append(list(row[7][0]))))
        self.refused(*self.definition_change(self.inputs(), lambda row: row[7][0].append(0)))

    def test_definition_boolean_integer_confusion_refuses(self):
        for position in (1, 2):
            self.refused(*self.definition_change(self.inputs(), lambda row: row.__setitem__(position, True)))
        for position in (3, 4, 5):
            self.refused(*self.definition_change(self.inputs(), lambda row: row.__setitem__(position, int(row[position]))))
        for position in (0, 6):
            self.refused(*self.definition_change(self.inputs(), lambda row: row[7][0].__setitem__(position, bool(row[7][0][position]))))

    def test_key_quote_is_only_exact_identifier_spelling(self):
        self.parse(*self.definition_change(self.inputs(), lambda row: row[7][0].__setitem__(1, '"' + COLUMN + '"')))
        for key in ("other", '"' + COLUMN + '" DESC', COLUMN + "::bigint", "public." + COLUMN, " " + COLUMN):
            self.refused(*self.definition_change(self.inputs(), lambda row: row[7][0].__setitem__(1, key)))

    def test_all_index_flags_are_closed(self):
        for key, value in (("valid", "false"), ("ready", "false"), ("live", "false"),
                           ("unique", "true"), ("primary", "true"), ("exclusion", "true")):
            with self.subTest(key=key):
                self.refused(*self.object_change(self.inputs(), "indexes", RELATION + "." + INDEX, {key: value}))

    def test_existing_index_cannot_be_explained_as_new(self):
        before, after, declaration = self.inputs()
        old, new = json.loads(before), json.loads(after)
        item = next(row for row in new["objects"] if row["kind"] == "indexes")
        old["objects"].append(copy.deepcopy(item))
        before = catalog_bytes(old)
        self.refused(before, after, bind_documents(declaration, before, after))

    def test_removed_old_index_is_residual_even_with_valid_new_index(self):
        before, after, declaration = self.inputs(full=True)
        value = json.loads(after)
        removed = next(row for row in json.loads(before)["objects"] if row["kind"] == "indexes")
        value["objects"].remove(removed)
        after = catalog_bytes(value)
        result = self.parse(before, after, bind_documents(declaration, before, after))
        self.assertEqual([(r.kind, r.identity, r.parent) for r in result.residual],
                         [("indexes", removed["identity"], removed["parent"])])

    def test_new_index_namespace_is_global_across_parents(self):
        for kind in ("relations", "indexes", "sequences"):
            for endpoint in ("before", "after"):
                with self.subTest(kind=kind, endpoint=endpoint):
                    before, after, declaration = self.inputs()
                    value = json.loads(before if endpoint == "before" else after)
                    if kind == "relations":
                        item = copy.deepcopy(next(row for row in value["objects"] if row["kind"] == "relations"))
                        item["identity"] = INDEX
                    elif kind == "indexes":
                        item = copy.deepcopy(next(row for row in json.loads(after)["objects"] if row["kind"] == "indexes"))
                        parent = copy.deepcopy(next(row for row in value["objects"] if row["kind"] == "relations"))
                        parent["identity"] = "other_table"; value["objects"].append(parent)
                        item.update(identity="other_table." + INDEX, parent="other_table")
                    else:
                        item = {"kind": "sequences", "identity": INDEX, "attributes": {
                            "type_sha256": sha(b"bigint"), "start": "1", "increment": "1", "min": "1",
                            "max": "9223372036854775807", "cache": "1", "cycle": "false",
                            "owner": "MIGRATION_OWNER", "acl": "", "owned_by_sha256": sha(b"[]")}}
                    value["objects"].append(item)
                    if endpoint == "before": before = catalog_bytes(value)
                    else: after = catalog_bytes(value)
                    self.refused(before, after, bind_documents(declaration, before, after))

    def test_composite_namespace_collision_preserves_scalar_and_array_distinction(self):
        for type_kind, endpoints in (("c", (0, 1)), ("c", (1,)), ("b", (0, 1)), ("e", (0, 1))):
            with self.subTest(type_kind=type_kind, endpoints=endpoints):
                before, after, declaration = self.inputs()
                values = [json.loads(before), json.loads(after)]
                # Closed source facts isolate the namespace rule; no fixture
                # claims to capture or execute a PostgreSQL type creation.
                item = copy.deepcopy(next(row for row in values[0]["objects"]
                    if row["kind"] == "types" and row["identity"] == RELATION))
                item["identity"] = INDEX
                item["attributes"]["type"] = type_kind
                for endpoint in endpoints:
                    values[endpoint]["objects"].append(copy.deepcopy(item))
                before, after = map(catalog_bytes, values)
                declaration = bind_documents(declaration, before, after)
                if type_kind == "c":
                    self.refused(before, after, declaration)
                else:
                    value = self.parse(before, after, declaration)
                    self.assertEqual([(ref.kind, ref.identity) for ref in value.explained],
                                     [("indexes", RELATION + "." + INDEX)])
                    self.assertEqual(value.residual, ())

    def test_parent_must_be_existing_ordinary_unchanged_relation(self):
        for change in ({"relkind": "p"}, {"partition_key_sha256": "a" * 64},
                       {"partition_bound_sha256": "a" * 64}, {"parents_sha256": "a" * 64}):
            self.refused(*self.object_change(self.inputs(), "relations", RELATION, change, both=True))
        self.refused(*self.object_change(self.inputs(), "relations", RELATION, {"owner": "OTHER"}))

    def test_key_must_preexist_with_unchanged_supported_attributes(self):
        for change in ({"type_sha256": sha(b"integer")}, {"collation_sha256": sha(b"pg_catalog.default")},
                       {"identity": "a"}, {"generated": "s"}, {"position": "0"}, {"position": "1025"}):
            self.refused(*self.object_change(self.inputs(), "columns", RELATION + "." + COLUMN, change, both=True))
        for change in ({"not_null": "true"}, {"default_sha256": "a" * 64}, {"acl": "MIGRATION_OWNER>PUBLIC:SELECT:false"}):
            self.refused(*self.object_change(self.inputs(), "columns", RELATION + "." + COLUMN, change))
        before, after, declaration = self.inputs(); value = json.loads(before)
        value["objects"] = [r for r in value["objects"] if r["identity"] != RELATION + "." + COLUMN]
        before = catalog_bytes(value)
        self.refused(before, after, bind_documents(declaration, before, after))

    def test_same_parent_column_and_rowtype_changes_remain_residual(self):
        before, after, declaration = self.inputs(); value = json.loads(after)
        item = copy.deepcopy(next(row for row in value["objects"] if row["identity"] == RELATION + "." + COLUMN))
        item["identity"] = RELATION + ".new_value"; item["attributes"]["position"] = "99"
        value["objects"].append(item)
        next(row for row in value["objects"] if row["kind"] == "types" and row["identity"] == RELATION)["attributes"]["definition_sha256"] = "b" * 64
        after = catalog_bytes(value)
        result = self.parse(before, after, bind_documents(declaration, before, after))
        self.assertEqual([(r.kind, r.identity) for r in result.residual],
                         [("columns", RELATION + ".new_value"), ("types", RELATION)])

    def test_nonkey_security_and_unrelated_changes_are_never_explained(self):
        before, after, declaration = self.inputs(full=True)
        pair = self.object_change((before, after, declaration), "columns", RELATION + ".id",
                                 {"acl": "MIGRATION_OWNER>PUBLIC:SELECT:false"})
        pair = self.object_change(pair, "types", "users", {"definition_sha256": "a" * 64})
        value = self.parse(*pair)
        self.assertEqual([(r.kind, r.identity) for r in value.residual],
                         [("columns", RELATION + ".id"), ("types", "users")])

    def test_independent_column_and_index_declarations_share_full_catalogs(self):
        before, column_after, declaration = columns.declared_catalogs()
        after, index_declaration = add_index(before, column_after)
        column_declaration = columns.bind_documents(declaration, before, after)
        column = si.nullable.classify_source(wire(column_declaration), before, after, base_sha=BASE, head_sha=HEAD)
        index = self.parse(before, after, index_declaration)
        self.assertFalse(set(column.explained) & set(index.explained))
        self.assertEqual(set(column.residual), set(index.explained))
        self.assertEqual(set(index.residual), set(column.explained))

    def test_catalog_duplicate_order_parent_and_unknown_attributes(self):
        for mode in ("duplicate", "reorder", "parent", "extra"):
            before, after, declaration = self.inputs(); value = json.loads(after)
            if mode == "duplicate": value["objects"].append(copy.deepcopy(value["objects"][0]))
            elif mode == "reorder": value["objects"].reverse()
            elif mode == "parent": next(r for r in value["objects"] if r["kind"] == "indexes")["parent"] = "missing"
            else: value["objects"][0]["attributes"]["unknown"] = "true"
            after = wire(value, sort=False) if mode == "reorder" else catalog_bytes(value)
            with self.subTest(mode=mode):
                self.refused(before, after, bind_documents(declaration, before, after))

    def test_document_type_canonical_duplicate_and_numeric_guards(self):
        before, after, declaration = self.inputs(); raw = wire(declaration)
        raws = [raw[:-1], b' ' + raw, raw + b'\n', raw.replace(b'"format_version":1', b'"format_version":1,"format_version":1', 1),
                raw.replace(b'"postgres_major":17', b'"postgres_major":-0'),
                raw.replace(b'"postgres_major":17', b'"postgres_major":17.0'),
                raw.replace(b'"postgres_major":17', b'"postgres_major":9223372036854775808'), b'\xff', bytearray(raw), None]
        for invalid in raws:
            with self.subTest(kind=type(invalid).__name__), self.assertRaisesRegex(si.IndexSourceRefusal, "^index-source-invalid$"):
                si.classify_source(invalid, before, after, base_sha=BASE, head_sha=HEAD)

    def test_definition_depth_control_and_extra_structure_refuse(self):
        for raw in ('[' * 25 + '0' + ']' * 25, '[]', '{}', '"PRIVATE\\n"', '["\\ufffd"]'):
            before, after, declaration = self.inputs(); declaration["index_definition_json"] = raw
            self.refused(before, after, declaration)

    def test_every_raw_role_and_combined_cap_precede_decoding(self):
        before, after, declaration = self.inputs(); raw = wire(declaration)
        cases = [(b'x' * (si.MAX_DECLARATION + 1), before, after),
                 (raw, b'x' * (si.MAX_DOCUMENT + 1), after),
                 (raw, before, b'x' * (si.MAX_DOCUMENT + 1)),
                 (raw, b'x' * si.MAX_DOCUMENT, b'x' * si.MAX_DOCUMENT)]
        with mock.patch.object(si.wire, '_load', side_effect=AssertionError('decode must not run')):
            for args in cases:
                with self.subTest(lengths=tuple(map(len, args))), self.assertRaisesRegex(si.IndexSourceRefusal, "^index-source-invalid$"):
                    si.classify_source(*args, base_sha=BASE, head_sha=HEAD)

    def test_definition_and_operation_bounds_are_not_silently_truncated(self):
        before, after, declaration = self.inputs()
        declaration["index_definition_json"] = 'x' * (si.MAX_DEFINITION + 1)
        self.refused(before, after, declaration)
        before, after, declaration = self.inputs(); declaration["operation"]["id"] = 'x' * si.MAX_OPERATION
        self.refused(before, after, declaration)


if __name__ == "__main__":
    unittest.main()
