"""Actual application capture parity and complete default-ACL refusal boundaries."""
import copy
import json
from pathlib import Path
import unittest

import catalog_semantics as v1
import catalog_semantics_v2 as semantic
from test_catalog_semantics import reseal, sha, wire

FIXTURE = Path(__file__).resolve().parents[2] / "internal/dbcatalog/testdata/nullable-column-v2.json"
CONTRACT_HASH = "6f51817c3516988a08b66193fedbb00a375bf8a23459c297593af3f675b37c53"


class ActualApplicationCaptureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # A missing actual PG17 capture is a failure, never a fabricated positive.
        raw = FIXTURE.read_bytes()
        cls.fixture = json.loads(raw)
        if set(cls.fixture) != {"format_version", "operation", "before", "after", "explanation"}:
            raise AssertionError("unexpected actual-capture fixture schema")
        if type(cls.fixture["format_version"]) is not int or cls.fixture["format_version"] != 2:
            raise AssertionError("unexpected actual-capture fixture version")
        if raw != wire(cls.fixture):
            raise AssertionError("actual capture is not canonical")

    def pair(self):
        return copy.deepcopy(self.fixture["before"]), copy.deepcopy(self.fixture["after"])

    def explain(self, before, after):
        return semantic.explain_transition(semantic.parse_operation(wire(self.fixture["operation"])),
            semantic.parse_observation(wire(before)), semantic.parse_observation(wire(after)))

    def refused(self, before, after):
        with self.assertRaises(semantic.SemanticRefusal):
            self.explain(before, after)

    def set_rows(self, value, rows):
        value["graph"]["security"]["default_acl_rows"] = rows
        value["graph"]["security"]["default_acl_count"] = len(rows)
        value["snapshot"]["count_only"]["default_acls"] = len(rows)

    def test_actual_complete_application_capture_matches_go(self):
        before, after = self.pair()
        self.assertEqual(self.fixture["operation"]["relation"], "organizations")
        self.assertEqual(self.fixture["operation"]["column"], "extra")
        self.assertEqual(semantic.render_statement(semantic.parse_operation(wire(self.fixture["operation"]))),
            b'ALTER TABLE ONLY "public"."organizations" ADD COLUMN "extra" pg_catalog.int8;\n')
        result = semantic.canonical_explanation(self.explain(before, after))
        self.assertEqual(result, wire(self.fixture["explanation"]))
        explanation = json.loads(result)
        self.assertEqual(explanation["default_acl_contract_sha256"], CONTRACT_HASH)
        self.assertEqual(explanation["before_observation_sha256"], sha(wire(before)))
        self.assertEqual(explanation["after_observation_sha256"], sha(wire(after)))
        self.assertEqual([r["kind"] for r in explanation["explained"]], ["columns", "types"])
        self.assertNotIn("compatible", explanation)
        expected = []
        for kind, privileges in (("S", ("SELECT", "USAGE")),
                ("r", ("DELETE", "INSERT", "SELECT", "UPDATE"))):
            expected.append({"owner": "MIGRATION_OWNER", "namespace": "public", "object_type": kind,
                "acl": [{"grantor": "MIGRATION_OWNER", "grantee": "RUNTIME", "privilege": privilege,
                         "grantable": False} for privilege in privileges]})
        for value in (before, after):
            self.assertGreater(len(value["snapshot"]["objects"]), 100)
            self.assertEqual(value["snapshot"]["count_only"]["default_acls"], 2)
            self.assertEqual(value["graph"]["security"]["default_acl_count"], 2)
            self.assertEqual(value["graph"]["security"]["default_acl_rows"], expected)
            self.assertEqual(semantic.canonical_observation(semantic.parse_observation(wire(value))), wire(value))

    def test_recognized_same_count_drift_cannot_be_explained(self):
        # Each is structurally valid and has honest unchanged counts, including
        # when the same unexpected profile is present at both endpoints.
        for variant in ("privilege", "grant_option", "grantor", "owner", "global", "public"):
            for both in (False, True):
                with self.subTest(variant=variant, both=both):
                    before, after = self.pair()
                    for value in ((before, after) if both else (after,)):
                        rows = value["graph"]["security"]["default_acl_rows"]
                        row = rows[0]
                        if variant == "privilege":
                            row["acl"][0]["privilege"] = "UPDATE"
                        elif variant == "grant_option":
                            row["acl"][0]["grantable"] = True
                        elif variant == "grantor":
                            row["acl"][0]["grantor"] = "RUNTIME"
                        elif variant == "owner":
                            row["owner"] = "RUNTIME"
                        elif variant == "global":
                            row["namespace"] = None
                        else:
                            row["acl"][0]["grantee"] = "PUBLIC"
                        row["acl"].sort(key=lambda r: (r["grantor"], r["grantee"], r["privilege"]))
                        rows.sort(key=lambda r: (r["owner"], r["namespace"] or "", r["object_type"]))
                        semantic.parse_observation(wire(value))
                    self.refused(before, after)

    def test_other_recognized_kinds_are_diagnostic_only(self):
        for kind, privilege, namespace in (("f", "EXECUTE", "public"), ("T", "USAGE", "public"),
                ("n", "CREATE", None), ("r", "MAINTAIN", "public")):
            with self.subTest(kind=kind):
                before, after = self.pair()
                for value in (before, after):
                    self.set_rows(value, [{"owner": "MIGRATION_OWNER", "namespace": namespace,
                        "object_type": kind, "acl": [{"grantor": "MIGRATION_OWNER", "grantee": "PUBLIC",
                        "privilege": privilege, "grantable": False}]}])
                    semantic.parse_observation(wire(value))
                self.refused(before, after)

    def test_empty_rows_and_absent_rows_remain_distinct_and_refuse_profile(self):
        before, after = self.pair()
        empty = copy.deepcopy(before)
        empty_rows = empty["graph"]["security"]["default_acl_rows"]
        for row in empty_rows:
            row["acl"] = []
        absent = copy.deepcopy(before)
        self.set_rows(absent, [])
        self.assertNotEqual(semantic.canonical_observation(semantic.parse_observation(wire(empty))),
            semantic.canonical_observation(semantic.parse_observation(wire(absent))))
        for rows in ([], empty_rows, empty_rows[:1]):
            before, after = self.pair()
            for value in (before, after):
                self.set_rows(value, copy.deepcopy(rows))
                semantic.parse_observation(wire(value))
            self.refused(before, after)

    def test_closed_rows_and_principals(self):
        mutations = (("owner", "unrecognized_role"), ("owner", None), ("namespace", "private"),
            ("namespace", 0), ("object_type", "x"), ("object_type", []), ("object_type", "n"),
            ("acl", None), ("acl", [None]), ("oid", 123))
        for key, value in mutations:
            with self.subTest(key=key, value=value):
                before, after = self.pair()
                after["graph"]["security"]["default_acl_rows"][0][key] = value
                self.refused(before, after)
        for key in ("owner", "namespace", "object_type", "acl"):
            before, after = self.pair()
            del after["graph"]["security"]["default_acl_rows"][0][key]
            self.refused(before, after)

    def test_closed_tuples_types_privileges_and_order(self):
        for key, value in (("grantor", "PUBLIC"), ("grantor", None), ("grantee", "other"),
                ("grantee", []), ("privilege", "EXECUTE"), ("privilege", "select"),
                ("privilege", 1), ("grantable", 1), ("grantable", "false"), ("extra", False)):
            with self.subTest(key=key, value=value):
                before, after = self.pair()
                after["graph"]["security"]["default_acl_rows"][0]["acl"][0][key] = value
                self.refused(before, after)
        for variant in ("row_order", "row_duplicate", "tuple_order", "tuple_duplicate", "grant_option_duplicate"):
            with self.subTest(variant=variant):
                before, after = self.pair()
                rows = after["graph"]["security"]["default_acl_rows"]
                if variant == "row_order":
                    rows.reverse()
                elif variant == "row_duplicate":
                    self.set_rows(after, [rows[0], rows[0]])
                elif variant == "tuple_order":
                    rows[0]["acl"].reverse()
                else:
                    duplicate = copy.deepcopy(rows[0]["acl"][0])
                    duplicate["grantable"] = variant == "grant_option_duplicate"
                    rows[0]["acl"].insert(1, duplicate)
                self.refused(before, after)

    def test_counter_equality_cannot_replace_complete_rows(self):
        for variant in ("snapshot", "graph", "missing_rows", "null_rows", "boolean_count", "unknown_acl", "unknown_policy"):
            with self.subTest(variant=variant):
                before, after = self.pair()
                security = after["graph"]["security"]
                if variant == "snapshot":
                    after["snapshot"]["count_only"]["default_acls"] = 0
                elif variant == "graph":
                    security["default_acl_count"] = 0
                elif variant == "missing_rows":
                    del security["default_acl_rows"]
                elif variant == "null_rows":
                    security["default_acl_rows"] = None
                elif variant == "boolean_count":
                    security["default_acl_count"] = True
                else:
                    key = "other_acl_entries" if variant == "unknown_acl" else "other_policy_roles"
                    security[key] = after["snapshot"]["count_only"][key] = 1
                self.refused(before, after)

    def test_row_tuple_and_observation_bounds(self):
        before, after = self.pair()
        row = after["graph"]["security"]["default_acl_rows"][0]
        self.set_rows(after, [copy.deepcopy(row) for _ in range(33)])
        self.refused(before, after)
        before, after = self.pair()
        row = after["graph"]["security"]["default_acl_rows"][0]
        row["acl"] = [copy.deepcopy(row["acl"][0]) for _ in range(1025)]
        self.refused(before, after)
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.parse_observation(b" " * (4 * 1024 * 1024 + 1))

    def test_complete_catalog_and_unrelated_changes_remain_binding(self):
        for kind, key, replacement in (("relations", "rls", "false"), ("columns", "default_sha256", "a" * 64),
                ("indexes", "valid", "false"), ("constraints", "validated", "false")):
            with self.subTest(kind=kind):
                before, after = self.pair()
                matches = [r for r in after["snapshot"]["objects"] if r["kind"] == kind]
                self.assertTrue(matches, "complete application capture must contain " + kind)
                row = matches[0]
                row["attributes"][key] = replacement if row["attributes"][key] != replacement else "changed"
                reseal(after)
                self.refused(before, after)
        for variant in ("missing", "unknown", "unknown_snapshot_key", "acl_hash", "column", "row_type", "array_link"):
            before, after = self.pair()
            if variant == "missing":
                after["snapshot"]["objects"].pop()
                reseal(after)
            elif variant == "unknown":
                after["snapshot"]["unknown"] = [{"kind": "functions", "count": 1}]
            elif variant == "unknown_snapshot_key":
                after["snapshot"]["seeds"] = []
            elif variant == "acl_hash":
                after["graph"]["security"]["database_acl_sha256"] = "f" * 64
            elif variant == "column":
                after["graph"]["slots"] = copy.deepcopy(before["graph"]["slots"])
            elif variant == "row_type":
                after["graph"]["row_type"] = copy.deepcopy(before["graph"]["row_type"])
            else:
                after["graph"]["array_type"]["element"]["identity"] = "unrelated"
            with self.subTest(variant=variant):
                self.refused(before, after)

    def test_nullable_operation_semantics_remain_strict(self):
        for key, value in (("not_null", True), ("default_present", True), ("generated", "s"),
                ("identity", "a"), ("type_name", "int4"), ("local", False), ("inherited_count", 1)):
            with self.subTest(key=key):
                before, after = self.pair()
                after["graph"]["slots"][-1][key] = value
                self.refused(before, after)

    def test_v1_and_v2_formats_cannot_be_interchanged(self):
        from test_catalog_semantics import FIXTURE as V1_FIXTURE
        old = json.loads(V1_FIXTURE.read_bytes())
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.parse_observation(wire(old["before"]))
        with self.assertRaises(v1.SemanticRefusal):
            v1.parse_observation(wire(self.fixture["before"]))
        disguised = copy.deepcopy(self.fixture["before"])
        disguised["format_version"] = disguised["graph"]["format_version"] = 1
        del disguised["graph"]["security"]["default_acl_rows"]
        with self.assertRaises(v1.SemanticRefusal):
            v1.parse_observation(wire(disguised))
        # Its real nonzero snapshot/security counters may not be rewritten by the v1 API.
        v1.parse_observation(wire(old["before"]))

    def test_canonical_json_and_closed_versions(self):
        raw = wire(self.fixture["before"])
        for malformed in (raw[:-1], b" " + raw, raw + b"{}", raw.replace(b'"format_version":2',
                b'"format_version":2.0', 1), raw.replace(b'"format_version":2',
                b'"format_version":2,"format_version":2', 1), b"[" * 1000 + b"]" * 1000):
            with self.assertRaises(semantic.SemanticRefusal):
                semantic.parse_observation(malformed)
        for key, value in (("format_version", True), ("format_version", 1), ("postgres_major", 16), ("expected_profile", {})):
            before, after = self.pair()
            after[key] = value
            self.refused(before, after)

    def test_direct_construction_and_explanations_are_revalidated(self):
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.canonical_observation(semantic.NullableObservationV2(b"{}\n"))
        with self.assertRaises(semantic.SemanticRefusal):
            semantic.canonical_observation(v1.NullableObservationV1(wire(self.fixture["before"])))
        for key, value in (("format_version", True), ("default_acl_contract_sha256", "0" * 64),
                ("before_observation_sha256", "bad"), ("explained", []), ("compatible", True)):
            explanation = copy.deepcopy(self.fixture["explanation"])
            explanation[key] = value
            with self.assertRaises(semantic.SemanticRefusal):
                semantic.canonical_explanation(semantic.NullableExplanationV2(wire(explanation)))


if __name__ == "__main__":
    unittest.main()
