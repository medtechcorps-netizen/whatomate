"""Independent complete source fixtures for qualified PG17 creation templates.

The literal type, ACL and predicate preimages were committed before the owned
PG17 template probe. These tests exercise source interpretation, not SQL/Go
execution or production permission. Existing golden and production baselines
come from the separately checked complete source reconstruction fixture.
"""
import copy
from dataclasses import FrozenInstanceError, replace
import hashlib
import json
from pathlib import Path
import re
import unittest
from unittest import mock

import schema_relation as relation
import test_ship_schema_nullable as columns

ROOT = Path(__file__).resolve().parents[2]
BASE, HEAD = "1" * 40, "2" * 40
CATALOG_PATH = "internal/dbcatalog/golden/catalog.json"
GLOBALS_PATH = "internal/dbcatalog/golden/global_tables.json"
TENANT_PATH = "internal/database/tenant.go"
PREDICATE = "(organization_id = (NULLIF(current_setting('app.current_organization_id'::text, true), ''::text))::uuid)"
TABLE_ACL = ";".join(
    ["MIGRATION_OWNER>MIGRATION_OWNER:" + p + ":false" for p in
     ("DELETE", "INSERT", "MAINTAIN", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE")]
    + ["MIGRATION_OWNER>RUNTIME:" + p + ":false" for p in ("DELETE", "INSERT", "SELECT", "UPDATE")])
TYPE_ACL = "MIGRATION_OWNER>MIGRATION_OWNER:USAGE:false;MIGRATION_OWNER>PUBLIC:USAGE:false"


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def wire(value):
    return columns.wire(value)


def catalog_bytes(value):
    return columns.catalog_bytes(value)


def bind_documents(declaration, before_catalog, after_catalog, before_globals, after_globals,
                   before_tenant, after_tenant):
    value = copy.deepcopy(declaration)
    for role, cr, gr, tr in (("before", before_catalog, before_globals, before_tenant),
                             ("after", after_catalog, after_globals, after_tenant)):
        endpoint = {"role": role}
        for key, path, raw in (("catalog", CATALOG_PATH, cr), ("globals", GLOBALS_PATH, gr),
                               ("tenant_source", TENANT_PATH, tr)):
            endpoint[key] = {"path": path, "bytes": len(raw), "sha256": sha(raw)}
        document = json.loads(cr)
        endpoint["catalog"].update(catalog_version=document["version"],
            catalog_sha256=sha(columns.catalog_bytes(document, version=0)))
        value[role] = endpoint
    return value


def _row(kind, identity, attrs, parent=None):
    row = {"kind": kind, "identity": identity, "attributes": attrs}
    if parent:
        row["parent"] = parent
    return row


def _creation_bundle(table, tenant):
    # Fixed independent source template: no values copied from the interpreter.
    fields = ([[1, "organization_id", "uuid", False, ""], [2, "value", "bigint", False, ""]]
              if tenant else [[1, "value", "bigint", False, ""]])
    row_definition = ["c", "C", False, "-", "", -1, False, "d", "x",
        "record_in(cstring,oid,integer)", "record_out(record)", "-", fields, []]
    array_definition = ["b", "A", False, "-", "", -1, False, "d", "x",
        "array_in(cstring,oid,integer)", "array_out(anyarray)", "public." + table, [], []]
    pg = lambda value: json.dumps(value, ensure_ascii=False, separators=(", ", ": "))
    row_raw, array_raw = pg(row_definition), pg(array_definition)
    empty = sha(b"")
    rows = [_row("relations", table, {"relkind": "r", "rls": str(tenant).lower(),
        "force_rls": str(tenant).lower(), "owner": "MIGRATION_OWNER", "acl": TABLE_ACL,
        "security_invoker": "false", "view_sha256": empty, "partition_key_sha256": empty,
        "partition_bound_sha256": empty, "parents_sha256": sha(b"[]")})]
    for identity, typ, raw in ((table, "c", row_raw), ("_" + table, "b", array_raw)):
        rows.append(_row("types", identity, {"type": typ, "definition_sha256": sha(raw.encode()),
            "owner": "MIGRATION_OWNER", "acl": TYPE_ACL, "enum_sha256": sha(b"[]")}))
    for number, name, typ, _, _ in fields:
        rows.append(_row("columns", table + "." + name, {"position": str(number),
            "type_sha256": sha(typ.encode()), "not_null": "false", "default_sha256": empty,
            "identity": "", "generated": "", "collation_sha256": empty, "acl": ""}, table))
    if tenant:
        for name, role, predicate in (("rereply_migration_access", "MIGRATION_OWNER", "true"),
                                      ("rereply_tenant_isolation", "RUNTIME", PREDICATE)):
            rows.append(_row("policies", table + "." + name, {"command": "*", "permissive": "true",
                "roles": role, "using_sha256": sha(predicate.encode()), "check_sha256": sha(predicate.encode())}, table))
    return rows, row_raw, array_raw


def add_relation(before_catalog, before_globals, before_tenant, *, mode, table, version=None):
    """Build a complete controlled pair, also reusable by profile integration."""
    tenant = mode == "direct-tenant"
    if mode not in ("global", "direct-tenant") or re.fullmatch(r"[a-z_][a-z0-9_]{0,61}", table) is None:
        raise AssertionError("fixture operation")
    old = json.loads(before_catalog)
    new = copy.deepcopy(old)
    new["version"] += 1
    rows, row_raw, array_raw = _creation_bundle(table, tenant)
    if any(r["identity"] in (table, "_" + table) for r in old["objects"]):
        raise AssertionError("fixture target exists")
    new["objects"].extend(rows)
    after_catalog = catalog_bytes(new)
    after_globals, after_tenant = before_globals, before_tenant
    if tenant:
        start = before_tenant.index(b"var DirectTenantTables = []string{")
        end = before_tenant.index(b"}", start)
        newline = b"\r\n" if b"\r\n" in before_tenant else b"\n"
        after_tenant = before_tenant[:end] + b'\t"' + table.encode() + b'",' + newline + before_tenant[end:]
        existing = [int(n) for n in re.findall(rb"(?m)^var tenantRLSAdditiveProfileV([0-9]+) =", before_tenant)]
        number = max(existing) + 1 if version is None else version
        after_tenant += newline + b"var tenantRLSAdditiveProfileV" + str(number).encode() + b" = []string{" + newline
        after_tenant += b'\t"' + table.encode() + b'",' + newline + b"}" + newline
    else:
        globals_ = json.loads(before_globals)
        globals_.append({"name": table, "reason": "Controlled source-only global relation template"})
        after_globals = wire(globals_)
    names = (("organization_id", "pg_catalog.uuid"), ("value", "pg_catalog.int8")) if tenant else (("value", "pg_catalog.int8"),)
    declaration = {"format_version": 1, "postgres_major": 17, "purpose": "ordinary-relation-source-shape",
        "operation": {"format_version": 1, "id": "create_" + table, "kind": "create_ordinary_relation",
            "mode": mode, "schema": "public", "relation": table,
            "columns": [{"name": name, "type": typ, "nullable": True, "default": None} for name, typ in names],
            "policy_template": "direct-tenant-v5-pg17-v1" if tenant else None},
        "row_definition_json": row_raw, "array_definition_json": array_raw}
    raws = (before_catalog, after_catalog, before_globals, after_globals, before_tenant, after_tenant)
    return (*raws, bind_documents(declaration, *raws))


def declared_catalogs(*, mode="global", production=False, table=None):
    before, _, _ = columns.declared_catalogs(production=production)
    table = table or ("source_profile_v2_items" if mode == "direct-tenant" else "source_global_items")
    return add_relation(before, (ROOT / GLOBALS_PATH).read_bytes(), (ROOT / TENANT_PATH).read_bytes(),
                        mode=mode, table=table)


def rebound(fixture, **changes):
    names = ("before", "after", "before_globals", "after_globals", "before_tenant", "after_tenant")
    raws = tuple(changes.get(name, raw) for name, raw in zip(names, fixture[:6]))
    return (*raws, bind_documents(changes.get("declaration", fixture[6]), *raws))


class RelationSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.global_ = declared_catalogs()
        cls.tenant = declared_catalogs(mode="direct-tenant")
        cls.production_global = declared_catalogs(production=True)
        cls.production_tenant = declared_catalogs(mode="direct-tenant", production=True)

    def parse(self, fixture, **kwargs):
        return relation.classify_source(wire(fixture[6]), *fixture[:6],
            base_sha=kwargs.get("base_sha", BASE), head_sha=kwargs.get("head_sha", HEAD))

    def refused(self, fixture, **kwargs):
        with self.assertRaisesRegex(relation.RelationSourceRefusal, "^relation-source-invalid$"):
            self.parse(fixture, **kwargs)

    def changed(self, fixture, kind, object_identity, **attrs):
        doc = json.loads(fixture[1])
        row = next(r for r in doc["objects"] if (r["kind"], r["identity"]) == (kind, object_identity))
        row["attributes"].update(attrs)
        return rebound(fixture, after=catalog_bytes(doc))

    def test_complete_global_and_direct_source_and_production_baselines(self):
        for fixture in (self.global_, self.tenant, self.production_global, self.production_tenant):
            with self.subTest(mode=fixture[6]["operation"]["mode"], source=sha(fixture[0])):
                result = self.parse(fixture)
                count = 7 if result.operation.mode == "direct-tenant" else 4
                self.assertEqual(len(result.explained), count)
                self.assertEqual(result.residual, ())
                self.assertEqual((result.base_sha, result.head_sha), (BASE, HEAD))
                self.assertEqual(result.declaration_sha256, sha(wire(fixture[6])))
                self.assertEqual(tuple(getattr(result, field) for field in (
                    "before_catalog_sha256", "after_catalog_sha256", "before_globals_sha256", "after_globals_sha256",
                    "before_tenant_source_sha256", "after_tenant_source_sha256")), tuple(map(sha, fixture[:6])))
        self.assertEqual(sha(self.global_[0]), columns.GOLDEN)
        self.assertEqual(sha(self.production_global[0]), columns.PRODUCTION)

    def test_statement_and_operation_hash_have_distinct_honest_scope(self):
        global_result, tenant_result = self.parse(self.global_), self.parse(self.tenant)
        global_sql = b'CREATE TABLE "public"."source_global_items" ("value" pg_catalog.int8);\n'
        tenant_sql = (b'CREATE TABLE "public"."source_profile_v2_items" ("organization_id" pg_catalog.uuid, "value" pg_catalog.int8);\n'
            b'ALTER TABLE "public"."source_profile_v2_items" ENABLE ROW LEVEL SECURITY;\n'
            b'ALTER TABLE "public"."source_profile_v2_items" FORCE ROW LEVEL SECURITY;\n')
        for result, fixture, statement in ((global_result, self.global_, global_sql), (tenant_result, self.tenant, tenant_sql)):
            self.assertEqual(relation.render_statement(result.operation), statement)
            self.assertEqual(result.statement_sha256, sha(statement))
            self.assertEqual(result.operation_sha256, sha(wire(fixture[6]["operation"])))
            self.assertNotIn(b"MIGRATION_OWNER", statement)
            self.assertNotIn(b"CREATE POLICY", statement)
        self.assertEqual(sha(PREDICATE.encode()), "7758c4cd69a5af99259bc354bcf04db06e3ad1f46f3855284559f6288b11e6f2")

    def test_immutable_projection_and_revalidated_renderer(self):
        result = self.parse(self.tenant)
        for obj, name, value in ((result, "base_sha", HEAD), (result.operation, "mode", "global"),
                                 (result.operation.columns[0], "nullable", False)):
            with self.assertRaises(FrozenInstanceError):
                setattr(obj, name, value)
        for operation in (replace(result.operation, relation='x";DROP'),
                          replace(result.operation, columns=list(result.operation.columns)),
                          replace(result.operation, columns=(replace(result.operation.columns[0], nullable=1), *result.operation.columns[1:]))):
            with self.assertRaises(relation.RelationSourceRefusal):
                relation.render_statement(operation)

    def test_endpoint_exact_paths_roles_counts_and_hashes(self):
        for role in ("before", "after"):
            for raw_role in ("catalog", "globals", "tenant_source"):
                for key, bad in (("path", "../catalog.json"), ("bytes", True), ("bytes", 1), ("sha256", "f" * 64), ("extra", 0)):
                    fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6])
                    fixture[6][role][raw_role][key] = bad
                    with self.subTest(role=role, raw_role=raw_role, key=key): self.refused(fixture)
            for key, bad in (("role", "head"), ("extra", False)):
                fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6]); fixture[6][role][key] = bad
                self.refused(fixture)

    def test_logical_catalog_hash_does_not_mean_raw_versioned_document(self):
        fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6])
        self.assertNotEqual(fixture[6]["after"]["catalog"]["sha256"], fixture[6]["after"]["catalog"]["catalog_sha256"])
        fixture[6]["after"]["catalog"]["catalog_sha256"] = sha(fixture[1]); self.refused(fixture)
        for value in (True, -1, 42):
            fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6])
            fixture[6]["before"]["catalog"]["catalog_version"] = value; self.refused(fixture)

    def test_external_identity_and_mixed_comparison_refuse(self):
        for key in ("base_sha", "head_sha"):
            for bad in (None, True, "MAIN", "A" * 40, "f" * 39):
                with self.subTest(key=key, bad=bad): self.refused(self.global_, **{key: bad})
        self.refused(self.global_, head_sha=BASE)
        fixture = list(self.global_); fixture[0] = self.production_global[0]; self.refused(fixture)
        fixture = list(self.tenant); fixture[4], fixture[5] = fixture[5], fixture[4]; self.refused(fixture)

    def test_closed_descriptor_and_numeric_types(self):
        for key, bad in (("format_version", True), ("format_version", 2), ("postgres_major", True),
                         ("postgres_major", 16), ("purpose", "observed"), ("extra", 0)):
            fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6]); fixture[6][key] = bad
            with self.subTest(key=key): self.refused(fixture)
        for key in self.global_[6]:
            fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6]); del fixture[6][key]
            with self.subTest(missing=key): self.refused(fixture)

    def test_canonical_wire_duplicates_trailing_and_nonfinite_refuse(self):
        raw = wire(self.global_[6])
        bads = (raw[:-1], b" " + raw, raw + b"{}", raw.replace(b'"format_version":1', b'"format_version":1,"format_version":1', 1),
                raw.replace(b'"format_version":1', b'"format_version":NaN', 1), raw.replace(b'"format_version":1', b'"format_version":1.0', 1))
        for bad in bads:
            with self.subTest(size=len(bad)), self.assertRaises(relation.RelationSourceRefusal):
                relation.classify_source(bad, *self.global_[:6], base_sha=BASE, head_sha=HEAD)

    def test_closed_operation_is_only_fixed_global_or_tenant_shape(self):
        for key, bad in (("format_version", True), ("id", "a;DROP"), ("kind", "raw_sql"), ("schema", "other"),
                         ("mode", "related-tenant"), ("relation", "A"), ("policy_template", "other"), ("extra", 0)):
            fixture = list(self.tenant); fixture[6] = copy.deepcopy(fixture[6]); fixture[6]["operation"][key] = bad
            with self.subTest(key=key): self.refused(fixture)
        for key, bad in (("name", "tenant_id"), ("type", "pg_catalog.text"), ("nullable", 1), ("default", "0"), ("extra", 0)):
            fixture = list(self.tenant); fixture[6] = copy.deepcopy(fixture[6]); fixture[6]["operation"]["columns"][0][key] = bad
            with self.subTest(column=key): self.refused(fixture)

    def test_all_new_companions_are_required(self):
        for fixture in (self.global_, self.tenant):
            name = fixture[6]["operation"]["relation"]
            doc = json.loads(fixture[1]); additions = [r for r in doc["objects"] if r["identity"] in (name, "_" + name) or r.get("parent") == name]
            for row in additions:
                bad = copy.deepcopy(doc); bad["objects"].remove(row)
                with self.subTest(mode=fixture[6]["operation"]["mode"], kind=row["kind"], identity=row["identity"]):
                    self.refused(rebound(fixture, after=catalog_bytes(bad)))

    def test_exact_new_relation_security_and_shape(self):
        for key, bad in (("relkind", "p"), ("rls", "false"), ("force_rls", "false"), ("owner", "RUNTIME"),
                         ("security_invoker", "true"), ("view_sha256", "a" * 64), ("partition_key_sha256", "a" * 64),
                         ("partition_bound_sha256", "a" * 64), ("parents_sha256", "a" * 64)):
            with self.subTest(key=key): self.refused(self.changed(self.tenant, "relations", "source_profile_v2_items", **{key: bad}))
        self.refused(self.changed(self.global_, "relations", "source_global_items", rls="true"))

    def test_complete_creation_acl_profiles_no_extra_or_missing_grant(self):
        for fixture in (self.global_, self.tenant):
            table = fixture[6]["operation"]["relation"]
            for kind, name, acl in (("relations", table, TABLE_ACL), ("types", table, TYPE_ACL), ("types", "_" + table, TYPE_ACL)):
                bads = ("", ";".join(acl.split(";")[:-1]), acl.replace(":false", ":true", 1),
                        ";".join(sorted(acl.split(";") + ["MIGRATION_OWNER>PUBLIC:SELECT:false"])))
                for bad in bads:
                    with self.subTest(mode=fixture[6]["operation"]["mode"], kind=kind, acl=bad):
                        self.refused(self.changed(fixture, kind, name, acl=bad))
            self.refused(self.changed(fixture, "columns", table + ".value", acl="MIGRATION_OWNER>RUNTIME:SELECT:false"))

    def test_exact_column_types_positions_and_absent_defaults(self):
        for key, bad in (("position", "2"), ("type_sha256", sha(b"text")), ("not_null", "true"),
                         ("default_sha256", sha(b"0")), ("identity", "a"), ("generated", "s"), ("collation_sha256", "a" * 64)):
            with self.subTest(key=key): self.refused(self.changed(self.global_, "columns", "source_global_items.value", **{key: bad}))

    def test_exact_policy_roles_predicates_and_inventory(self):
        for policy in ("rereply_migration_access", "rereply_tenant_isolation"):
            name = "source_profile_v2_items." + policy
            for key, bad in (("roles", "PUBLIC"), ("command", "r"), ("permissive", "false"),
                             ("using_sha256", "a" * 64), ("check_sha256", "a" * 64)):
                with self.subTest(policy=policy, key=key): self.refused(self.changed(self.tenant, "policies", name, **{key: bad}))
        doc = json.loads(self.tenant[1]); extra = copy.deepcopy(next(r for r in doc["objects"] if r["kind"] == "policies" and r.get("parent") == "source_profile_v2_items"))
        extra["identity"] = "source_profile_v2_items.extra"; doc["objects"].append(extra)
        self.refused(rebound(self.tenant, after=catalog_bytes(doc)))

    def test_coherent_type_preimage_changes_still_refuse(self):
        for field in ("row_definition_json", "array_definition_json"):
            for index, bad in ((3, "x"), (5, 0), (6, True), (7, "i"), (8, "p"), (9, "evil()"), (10, "evil()"), (11, "public.other")):
                fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6]); value = json.loads(fixture[6][field]); value[index] = bad
                fixture[6][field] = json.dumps(value, separators=(", ", ": "))
                typ = "source_global_items" if field == "row_definition_json" else "_source_global_items"
                fixture = self.changed(fixture, "types", typ, definition_sha256=sha(fixture[6][field].encode()))
                with self.subTest(field=field, index=index): self.refused(fixture)

    def test_type_bool_int_confusion_and_non_pg_framing_refuse(self):
        for field, mutate in (("row_definition_json", lambda v: v[12][0].__setitem__(0, True)),
                               ("row_definition_json", lambda v: v.__setitem__(5, False)),
                               ("array_definition_json", lambda v: v.__setitem__(6, 0))):
            fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6]); value = json.loads(fixture[6][field]); mutate(value)
            fixture[6][field] = json.dumps(value, separators=(", ", ": "))
            name = "source_global_items" if field == "row_definition_json" else "_source_global_items"
            self.refused(self.changed(fixture, "types", name, definition_sha256=sha(fixture[6][field].encode())))
        fixture = list(self.global_); fixture[6] = copy.deepcopy(fixture[6])
        fixture[6]["row_definition_json"] = json.dumps(json.loads(fixture[6]["row_definition_json"]), separators=(",", ":"))
        self.refused(self.changed(fixture, "types", "source_global_items", definition_sha256=sha(fixture[6]["row_definition_json"].encode())))

    def test_extra_child_is_not_hidden_by_whole_relation_explanation(self):
        doc = json.loads(self.global_[1])
        index = copy.deepcopy(next(r for r in doc["objects"] if r["kind"] == "indexes"))
        index["parent"] = "source_global_items"; index["identity"] = "source_global_items.extra_idx"
        doc["objects"].append(index)
        self.refused(rebound(self.global_, after=catalog_bytes(doc)))

    def test_existing_type_and_relation_namespace_collisions_refuse(self):
        for typ in ("b", "c"):
            for name in ("source_global_items", "_source_global_items"):
                before, after = json.loads(self.global_[0]), json.loads(self.global_[1])
                old = _row("types", name, {"type": typ, "definition_sha256": "a" * 64, "enum_sha256": sha(b"[]"), "owner": "MIGRATION_OWNER", "acl": TYPE_ACL})
                before["objects"].append(old)
                self.refused(rebound(self.global_, before=catalog_bytes(before), after=catalog_bytes(after)))
        before = json.loads(self.global_[0]); old = copy.deepcopy(next(r for r in before["objects"] if r["kind"] == "indexes"))
        old["identity"] = old["parent"] + ".source_global_items"; before["objects"].append(old)
        self.refused(rebound(self.global_, before=catalog_bytes(before)))

    def test_existing_privilege_and_unrelated_catalog_deltas_remain_residual(self):
        fixture = self.changed(self.global_, "relations", "organizations", acl="")
        fixture = self.changed(fixture, "types", "users", definition_sha256="a" * 64)
        result = self.parse(fixture)
        self.assertEqual([(r.kind, r.identity) for r in result.residual], [("relations", "organizations"), ("types", "users")])
        self.assertTrue(all(r.identity.startswith(("source_global_items", "_source_global_items")) for r in result.explained))

    def test_unrelated_added_function_is_only_residual(self):
        doc = json.loads(self.tenant[1]); row = copy.deepcopy(next(r for r in doc["objects"] if r["kind"] == "functions"))
        row["identity"] = "unrelated(" + sha(b"") + ")"; doc["objects"].append(row)
        result = self.parse(rebound(self.tenant, after=catalog_bytes(doc)))
        self.assertEqual([(r.kind, r.identity) for r in result.residual], [("functions", row["identity"])])

    def test_global_requires_only_one_complete_membership_delta(self):
        self.refused(rebound(self.global_, after_globals=self.global_[2]))
        for update in ("reason", "keys", "extra", "missing_relation"):
            rows = json.loads(self.global_[3])
            if update == "reason": rows[0]["reason"] = "changed existing reason"
            elif update == "keys": rows[-1]["keys"] = []
            elif update == "extra": rows.append({"name": "organizations", "reason": "ambiguous"})
            else: rows.append({"name": "nonexistent", "reason": "unknown"})
            with self.subTest(update=update): self.refused(rebound(self.global_, after_globals=wire(rows)))

    def test_global_and_direct_membership_are_disjoint(self):
        source = self.global_[5].replace(b'\t"agent_transfers",', b'\t"source_global_items",\n\t"agent_transfers",', 1)
        fixture = rebound(self.global_, before_tenant=source, after_tenant=source)
        self.refused(fixture)
        rows = json.loads(self.tenant[3]); rows.append({"name": "source_profile_v2_items", "reason": "conflict"})
        self.refused(rebound(self.tenant, after_globals=wire(rows)))

    def test_tenant_requires_exact_direct_and_next_version_membership(self):
        for source in (self.tenant[4], self.tenant[5].replace(b"tenantRLSAdditiveProfileV2 =", b"tenantRLSAdditiveProfileV3 ="),
                       self.tenant[5].replace(b'\t"source_profile_v2_items",', b'\t"organizations",', 1),
                       self.tenant[5].replace(b'\t"agent_transfers",', b'\t"source_extra",\n\t"agent_transfers",', 1)):
            self.refused(rebound(self.tenant, after_tenant=source))

    def test_real_legacy_literal_order_multiline_predicates_and_constants(self):
        inventory = relation._tenant_inventory(self.tenant[4])
        self.assertLess(inventory.direct.index("customer_activity_events"), inventory.direct.index("crm_leads"))
        self.assertIn("\n", dict(inventory.related)["bulk_message_recipients"])
        self.assertEqual(len(inventory.v1), 3)
        self.assertEqual(inventory.profiles, ())
        self.assertEqual(set(dict(inventory.constants)), {"tenantRLSRoutingVersion", "tenantRLSAdditiveProfileVersion",
            "tenantPolicyFingerprintSignature", "tenantAdditivePolicyFingerprintSignature"})

    def test_duplicate_shadow_rebound_alias_and_computed_memberships_refuse(self):
        raw = self.tenant[5]
        for suffix in (b'\nvar DirectTenantTables = []string{"x",}\n',
                       b'\nfunc bad() { DirectTenantTables = nil }\n',
                       b'\nfunc bad() { DirectTenantTables := []string{"x"}; _ = DirectTenantTables }\n',
                       b'\nvar alias = DirectTenantTables\n', b'\nfunc bad() { DirectTenantTables[0] = "x" }\n'):
            self.refused(rebound(self.tenant, after_tenant=raw + suffix))
        for old, new in ((b'\t"agent_transfers",', b'\t"agent_transfers",\n\t"agent_transfers",'),
                         (b'\t"agent_transfers",', b'\t"agent_" + "transfers",'),
                         (b"var tenantRLSAdditiveProfileV2 = []string{", b"var tenantRLSAdditiveProfileV2 = make([]string, 1) //")):
            self.refused(rebound(self.tenant, after_tenant=raw.replace(old, new, 1)))

    def test_existing_core_constants_and_memberships_cannot_change(self):
        for old, new in ((b"tenantRLSRoutingVersion         = 5", b"tenantRLSRoutingVersion         = 6"),
                         (b"tenantRLSAdditiveProfileVersion = 1", b"tenantRLSAdditiveProfileVersion = 2"),
                         (b'\t"whatsapp_coexistence_states",', b'\t"source_profile_v2_items",'),
                         (b'\t"agent_transfers",', b'')):
            self.assertIn(old, self.tenant[5])
            self.refused(rebound(self.tenant, after_tenant=self.tenant[5].replace(old, new, 1)))

    def test_len_shadow_selector_member_and_signature_forms_refuse(self):
        # These do not contain another reserved-name mutation. Passing the
        # slice to an impostor len was the concrete misleading read pattern.
        raw = self.tenant[5]
        bads = (
            b'\nfunc len(values []string) int { values[0] = "changed"; return 1 }\n',
            b'\nfunc shadow() { len := func(values []string) int { values[0] = "changed"; return 1 }; _ = len(DirectTenantTables) }\n',
            b'\nfunc parameter(len func([]string) int) { _ = len(DirectTenantTables) }\n',
            b'\nfunc selector() { _ = helper . /* hidden qualifier */ len (DirectTenantTables) }\n',
            b'\ntype helper struct { len func([]string) int }\n',
            b'\nfunc namedReturn() (len func([]string) int) { return }\n',
            b'\nfunc parenthesized(len (FunctionType)) { _ = len(DirectTenantTables) }\n',
            b'\nvar ( len (FunctionType) )\n',
            b'\nfunc (h helper) /* method */ len(values []string) int { return 1 }\n',
            b'\ntype FunctionType func([]string) int\nfunc shadow(x struct{}, len (FunctionType)) { _ = len(DirectTenantTables) }\n',
            b'\ntype FunctionType func([]string) int\nfunc shadow(x interface{}, len (FunctionType)) { _ = len(DirectTenantTables) }\n',
            b'\ntype FunctionType func([]string) int\nfunc shadow(x struct{}) (len (FunctionType)) { _ = len(DirectTenantTables); return }\n',
            b'\ntype FunctionType func([]string) int\nfunc shadow(x interface{}) (len (FunctionType)) { _ = len(DirectTenantTables); return }\n',
            b'\ntype FunctionType func([]string) int\nfunc\nshadow(x struct{}, len (FunctionType)) { _ = len(DirectTenantTables) }\n',
            b'\ntype FunctionType func([]string) int\nfunc /* split\nheader */ shadow(x struct{}, len (FunctionType)) { _ = len(DirectTenantTables) }\n',
        )
        for suffix in bads:
            with self.subTest(source=suffix): self.refused(rebound(self.tenant, after_tenant=raw + suffix))
        read = b'\nfunc ordinaryRead() { _ = len /* builtin */ (DirectTenantTables) }\n'
        self.parse(rebound(self.tenant, after_tenant=raw + read))
        read = b'\nfunc structuredRead(x struct{}, y interface{}) { _ = len /* builtin */ (DirectTenantTables) }\n'
        self.parse(rebound(self.tenant, after_tenant=raw + read))

    def test_append_v3_preserves_every_old_literal_span(self):
        fixture = add_relation(self.tenant[1], self.tenant[3], self.tenant[5], mode="direct-tenant", table="source_profile_v3_items", version=3)
        result = self.parse(fixture)
        self.assertEqual(len(result.explained), 7)
        self.assertEqual(result.residual, ())
        self.assertEqual(relation._tenant_inventory(fixture[5]).profiles, ((2, ("source_profile_v2_items",)), (3, ("source_profile_v3_items",))))
        newline = b"\r\n" if b"\r\n" in fixture[5] else b"\n"
        altered = fixture[5].replace(b'\t"source_profile_v2_items",' + newline + b'}',
                                     b'\t"source_profile_v3_items",' + newline + b'}', 1)
        self.assertNotEqual(altered, fixture[5]); self.refused(rebound(fixture, after_tenant=altered))

    def test_retained_protection_does_not_freeze_whole_column_inventory(self):
        doc = json.loads(self.tenant[1])
        doc["objects"].append(_row("columns", "source_profile_v2_items.later", {"position": "3", "type_sha256": sha(b"bigint"),
            "not_null": "false", "default_sha256": sha(b""), "identity": "", "generated": "", "collation_sha256": sha(b""), "acl": ""}, "source_profile_v2_items"))
        _, facts = relation._facts(catalog_bytes(doc))
        relation._validate_direct_protection(facts, table="source_profile_v2_items")
        self.refused(rebound(self.tenant, after=catalog_bytes(doc)))
        facts[next(r for r in facts if r.kind == "policies" and r.parent == "source_profile_v2_items")]["roles"] = "PUBLIC"
        with self.assertRaises(relation.RelationSourceRefusal): relation._validate_direct_protection(facts, table="source_profile_v2_items")

    def test_complete_existing_globals_are_validated_even_if_unchanged(self):
        rows = json.loads(self.global_[2]); rows.append({"name": "absent", "reason": "not a real source relation"})
        bad = wire(rows)
        self.refused(rebound(self.global_, before_globals=bad))
        rows = json.loads(self.global_[2]); rows[0]["keys"] = ["missing_column"]
        self.refused(rebound(self.global_, before_globals=wire(rows)))

    def test_per_role_bounds_exact_types_and_aggregate_precede_any_parse(self):
        roles = [wire(self.global_[6]), *self.global_[:6]]
        limits = (64 << 10, 4 << 20, 4 << 20, 256 << 10, 256 << 10, 512 << 10, 512 << 10)
        for i, limit in enumerate(limits):
            for value in (None, bytearray(roles[i]), b"x" * (limit + 1)):
                bad = list(roles); bad[i] = value
                with self.subTest(role=i, type=type(value).__name__), mock.patch.object(relation.wire, "_load", side_effect=AssertionError("parse before bounds")):
                    with self.assertRaises(relation.RelationSourceRefusal):
                        relation.classify_source(*bad, base_sha=BASE, head_sha=HEAD)
        # Equal aliased catalog roles are both charged, not deduplicated.
        bad = list(roles); bad[1] = bad[2] = b"x" * (4 << 20)
        with mock.patch.object(relation.wire, "_load", side_effect=AssertionError("parse before aggregate")):
            with self.assertRaises(relation.RelationSourceRefusal):
                relation.classify_source(*bad, base_sha=BASE, head_sha=HEAD)


if __name__ == "__main__":
    unittest.main()
