"""Complete actual-capture fixture and deliberate semantic corruption tests."""
import copy
import hashlib
import json
from pathlib import Path
import unittest

import catalog_semantics as semantic

FIXTURE = Path(__file__).resolve().parents[2] / "internal/dbcatalog/testdata/nullable-column-v1.json"


def wire(value):
    text = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    for char, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(char, escaped)
    return (text + "\n").encode("utf-8")


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def reseal(value):
    """Keep a mutated whole catalog honest; rejection must be semantic."""
    rows = []
    for item in sorted(value["snapshot"]["objects"], key=lambda r: (r["kind"], r["identity"])):
        row = {"kind": item["kind"], "identity": item["identity"]}
        if item.get("parent"):
            row["parent"] = item["parent"]
        row["attributes"] = dict(sorted(item["attributes"].items()))
        rows.append(row)
    text = json.dumps({"version": 0, "objects": rows}, separators=(",", ":"), ensure_ascii=False)
    for char, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(char, escaped)
    value["snapshot"]["objects"] = rows
    value["snapshot"]["sha256"] = sha((text + "\n").encode("utf-8"))
    value["graph"]["catalog_sha256"] = value["snapshot"]["sha256"]


class OperationTests(unittest.TestCase):
    def operation(self):
        return {"format_version": 1, "id": "add-next", "kind": "add_nullable_column",
                "relation": "sample", "column": "next", "type": "pg_catalog.int8",
                "nullable": True, "default": None}

    def test_exact_renderer(self):
        self.assertEqual(semantic.render_statement(semantic.parse_operation(wire(self.operation()))),
            b'ALTER TABLE ONLY "public"."sample" ADD COLUMN "next" pg_catalog.int8;\n')

    def test_closed_operation(self):
        for key, value in (("format_version", True), ("type", "bigint"), ("default", "NULL"),
                ("nullable", 1), ("relation", "public.sample"), ("column", "x\"; DROP TABLE y;--"),
                ("id", "caf\u00e9"), ("kind", "execute_sql"), ("extra", False)):
            with self.subTest(key=key):
                operation = self.operation()
                operation[key] = value
                with self.assertRaises(semantic.SemanticRefusal):
                    semantic.parse_operation(wire(operation))

    def test_bad_json_and_bounds(self):
        original = wire(self.operation())
        for raw in (original[:-1], b" " + original, original + b"{}", original.replace(b'"default":null',
                b'"default":null,"default":null'), original.replace(b'"format_version":1',
                b'"format_version":1.0'), b"[" * 1000 + b"]" * 1000, b"x" * 2049):
            with self.subTest(raw=raw[:30]):
                with self.assertRaises(semantic.SemanticRefusal):
                    semantic.parse_operation(raw)

    def test_direct_model_construction_is_not_validation(self):
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.render_statement(semantic.NullableColumnOperationV1(b"{}\n"))
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.canonical_observation(semantic.NullableObservationV1(b"{}\n"))


class ActualCaptureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Missing fixture is an error, never a skipped or handcrafted positive.
        cls.fixture = json.loads(FIXTURE.read_bytes())
        if set(cls.fixture) != {"format_version", "operation", "before", "after", "explanation"}:
            raise AssertionError("unexpected actual-capture fixture schema")
        if cls.fixture["format_version"] != 1:
            raise AssertionError("unexpected actual-capture fixture version")

    def pair(self):
        return copy.deepcopy(self.fixture["before"]), copy.deepcopy(self.fixture["after"])

    def explain(self, before, after):
        return semantic.explain_transition(semantic.parse_operation(wire(self.fixture["operation"])),
            semantic.parse_observation(wire(before)), semantic.parse_observation(wire(after)))

    def refused(self, before, after):
        with self.assertRaises(semantic.SemanticRefusal):
            self.explain(before, after)

    def test_actual_complete_capture_matches_go_explanation(self):
        before, after = self.pair()
        actual = semantic.canonical_explanation(self.explain(before, after))
        self.assertEqual(actual, wire(self.fixture["explanation"]))
        self.assertEqual(len(self.fixture["explanation"]["explained"]), 2)
        self.assertNotIn("compatible", json.loads(actual))
        self.assertTrue(any("\\u003e" in wire(v).decode("utf-8") for v in (before, after)))
        for value in (before, after):
            parsed = semantic.parse_observation(wire(value))
            self.assertEqual(semantic.canonical_observation(parsed), wire(value))

    def test_new_column_semantic_boundaries(self):
        mutations = (("type_namespace", "public"), ("type_name", "int4"), ("type_modifier", 0),
            ("type_text", "integer"), ("not_null", True), ("default_present", True),
            ("dropped", True), ("local", False), ("inherited_count", 1), ("number", True),
            ("identity", "a"), ("generated", "s"), ("collation_name", "default"),
            ("acl", "MIGRATION_OWNER>RUNTIME:SELECT:false"))
        for key, value in mutations:
            with self.subTest(key=key):
                before, after = self.pair()
                after["graph"]["slots"][-1][key] = value
                self.refused(before, after)

    def test_relation_and_dependency_boundaries(self):
        for key, value in (("relkind", "p"), ("persistence", "t"), ("is_partition", True),
                ("parents", [{"kind": "relations", "identity": "parent", "parent": ""}]),
                ("children", ["other"]), ("of_type", "typed")):
            with self.subTest(key=key):
                before, after = self.pair()
                after["graph"]["relation"][key] = value
                self.refused(before, after)
        for surface, link in (("array_type", "element"), ("row_type", "array"), ("row_type", "relation")):
            before, after = self.pair()
            after["graph"][surface][link]["identity"] = "unrelated"
            self.refused(before, after)

    def test_full_catalog_old_object_changes_not_swallowed(self):
        # These are independently resealed full catalogs, not broken digests.
        for kind, key, value in (("relations", "rls", "false"), ("relations", "owner", "RUNTIME"),
                ("columns", "default_sha256", "a" * 64), ("indexes", "valid", "false"),
                ("constraints", "validated", "false"), ("sequences", "increment", "2")):
            with self.subTest(kind=kind, key=key):
                before, after = self.pair()
                rows = [r for r in after["snapshot"]["objects"] if r["kind"] == kind]
                self.assertTrue(rows, "actual fixture must contain " + kind)
                row = rows[0]
                if row["attributes"][key] == value:
                    value = "true" if value == "false" else "false"
                row["attributes"][key] = value
                reseal(after)
                self.refused(before, after)

    def test_unchanged_column_or_row_type_cannot_hide_missing_companion(self):
        before, after = self.pair()
        after["graph"]["row_type"] = copy.deepcopy(before["graph"]["row_type"])
        self.refused(before, after)
        before, after = self.pair()
        after["graph"]["slots"] = copy.deepcopy(before["graph"]["slots"])
        self.refused(before, after)

    def test_security_changes_and_nonempty_default_acls_refuse(self):
        for key in ("database_acl_sha256", "public_schema_acl_sha256"):
            before, after = self.pair()
            after["graph"]["security"][key] = "f" * 64
            self.refused(before, after)
        for graph_key, count_key in (("default_acl_count", "default_acls"),
                ("other_acl_entries", "other_acl_entries"), ("other_policy_roles", "other_policy_roles")):
            before, after = self.pair()
            for value in (before, after):
                value["graph"]["security"][graph_key] = 1
                value["snapshot"]["count_only"][count_key] = 1
            self.refused(before, after)

    def test_complete_inventory_required(self):
        for variant in ("missing", "duplicate", "unknown", "order", "seeds"):
            before, after = self.pair()
            if variant == "missing":
                after["snapshot"]["objects"].pop()
                reseal(after)
            elif variant == "duplicate":
                after["snapshot"]["objects"].append(after["snapshot"]["objects"][0])
                reseal(after)
            elif variant == "unknown":
                after["snapshot"]["unknown"] = [{"kind": "functions", "count": 1}]
            elif variant == "order":
                after["snapshot"]["objects"].reverse()
            else:
                after["snapshot"]["seeds"] = []
            with self.subTest(variant=variant):
                self.refused(before, after)

    def test_definition_tampering_and_outer_version(self):
        for surface in ("row_type", "array_type"):
            before, after = self.pair()
            after["graph"][surface]["definition_json"] += " "
            self.refused(before, after)
        for key, value in (("format_version", True), ("postgres_major", 14)):
            before, after = self.pair()
            after[key] = value
            self.refused(before, after)


if __name__ == "__main__":
    unittest.main()
