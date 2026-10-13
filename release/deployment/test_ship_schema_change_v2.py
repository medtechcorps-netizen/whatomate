"""Preparation-only v2 policy tests; no release/network/DB capability is granted.

Direct-rule fixtures are intentionally small synthetic catalog fragments. The
paired-table case demonstrates why they do not prove a full real DB expansion.
Source-history cases use real commits only inside disposable test repositories.
"""
from __future__ import annotations

import hashlib
import json
import shutil
import tempfile
import unittest
from unittest import mock
from pathlib import Path

import schema_change as sc
import test_ship_support as support
import test_pending_plan as pending_fixture
import source_git


ROOT = Path(__file__).resolve().parents[2]
H = "a" * 64
J = "b" * 64
EMPTY = hashlib.sha256(b"").hexdigest()
MAIN = "package main\n\nfunc runRLSMigration(args []string) {\n\tprintln(1)\n}\n\nfunc unrelated() {\n\tprintln(2)\n}\n"
TENANT = "package database\n\nvar DirectTenantTables = []string{\n\t\"t\",\n}\n\nvar RelatedTenantTables = map[string]string{}\n\nvar tenantRLSAdditiveProfileV1 = []string{}\n"


def row(kind, name, parent="", **changes):
    # Independent full wire examples, not generated from the implementation's
    # attribute inventory. These are data fixtures, not expected-policy code.
    attrs = {
        "relations": dict(relkind="r", rls="true", force_rls="true", owner="MIGRATION_OWNER", acl="", security_invoker="false", view_sha256=EMPTY, partition_key_sha256=EMPTY, partition_bound_sha256=EMPTY, parents_sha256=H),
        "columns": dict(position="2", type_sha256=H, not_null="false", default_sha256=EMPTY, identity="", generated="", collation_sha256=EMPTY, acl=""),
        "constraints": dict(type="c", definition_sha256=H, validated="true", deferrable="false", deferred="false"),
        "indexes": dict(definition_sha256=H, valid="true", ready="true", live="true", unique="false", primary="false", exclusion="false"),
        "triggers": dict(definition_sha256=H, enabled="O", internal="false", function_sha256=H),
        "functions": dict(source_sha256=H, definition_sha256=H, security_definer="false", owner="MIGRATION_OWNER", acl="", config_sha256=H, volatility="v", parallel="u", strict="false", leakproof="false", language_sha256=H),
        "policies": dict(command="*", permissive="true", roles="PUBLIC", using_sha256=H, check_sha256=H),
        "sequences": dict(type_sha256=H, start="1", increment="1", min="1", max="999", cache="1", cycle="false", owner="MIGRATION_OWNER", acl="", owned_by_sha256=H),
        "types": dict(type="c", definition_sha256=H, owner="MIGRATION_OWNER", acl="", enum_sha256=H),
        "extensions": dict(version_sha256=H, relocatable="true", owner="MIGRATION_OWNER"),
    }[kind]
    attrs.update(changes)
    result = {"kind": kind, "identity": name, "attributes": attrs}
    if parent:
        result["parent"] = parent
    return result


def catalog(objects=None, version=0):
    return {"version": version, "objects": [row("relations", "t")] if objects is None else objects}


def history(version=0, kind="contract", engine=False, floor=0):
    rows = [{"version": 0, "class": "baseline", "min_reader_version": 0}]
    if version:
        rows.append({"version": version, "class": kind, "min_reader_version": floor, "apply_engine": engine})
    return rows


def text(value):
    return json.dumps(value, sort_keys=True, indent=2) + "\n"


def migration(digest=H):
    return {"version": 1, "roots": sc.MIGRATION_ROOTS,
            "closure_files": ["cmd/whatomate/main.go", "internal/database/tenant.go"], "closure_sha256": digest}


def step(rev=1, digest=H, **changes):
    value = {"id": "seed", "rev": rev, "func": "database.Seed", "kind": "seed-reconciler",
             "baseline": False, "idempotent": True, "n1_safe": True, "requires_gates": [],
             "closure_files": ["cmd/whatomate/main.go"], "closure_sha256": digest}
    value.update(changes)
    return value


class DirectCurrentFormatRuleTests(unittest.TestCase):
    """Synthetic direct rules; these do not execute or authenticate migrations."""

    def delta(self, before, after, old_global=None, new_global=None):
        reasons, summary, needs = set(), [], set()
        kind = sc._v2_catalog_delta(catalog(before), catalog(after), old_global or [], new_global or [], reasons, summary, needs)
        return kind, reasons, summary, needs

    def added(self, item, **kwargs):
        return self.delta([row("relations", "t")], [row("relations", "t"), item], **kwargs)

    def test_direct_protected_new_table(self):
        for relation_kind in ("r", "p"):
            with self.subTest(kind=relation_kind):
                result = self.added(row("relations", "new_table", relkind=relation_kind))
                self.assertEqual(result[:2], ("expand", set()))

    def test_direct_new_global_table_has_reason(self):
        result = self.added(row("relations", "new_table", rls="false", force_rls="false"),
                            new_global=[{"name": "new_table", "reason": "Synthetic global control"}])
        self.assertEqual(result[:2], ("expand", set()))

    def test_incomplete_rls_protection_is_blocked(self):
        for enable, force in (("false", "false"), ("true", "false"), ("false", "true")):
            with self.subTest(enable=enable, force=force):
                result = self.added(row("relations", "new_table", rls=enable, force_rls=force))
                self.assertIn("unprotected-new-relation:new_table", result[1])

    def test_existing_rls_change_is_contract(self):
        for field in ("rls", "force_rls"):
            with self.subTest(field=field):
                self.assertEqual(self.delta([row("relations", "t")], [row("relations", "t", **{field: "false"})])[0], "contract")

    def test_invoker_view_keeps_dependency_unknown(self):
        result = self.added(row("relations", "v", relkind="v", security_invoker="true"))
        self.assertEqual(result[0], "contract")
        self.assertIn("view-dependency-unsupported", result[3])

    def test_owner_view_has_required_summary(self):
        result = self.added(row("relations", "v", relkind="v"))
        self.assertEqual(result[0], "contract")
        self.assertIn("new view v", result[2])

    def test_materialized_view_allowlist_does_not_waive_contract(self):
        self.assertEqual(self.added(row("relations", "v", relkind="m"), new_global=[{"name": "v", "reason": "Synthetic"}])[0], "contract")

    def test_changed_view_definition_is_contract(self):
        self.assertEqual(self.delta([row("relations", "v", relkind="v")], [row("relations", "v", relkind="v", view_sha256=J)])[0], "contract")

    def test_foreign_table_requires_policy_and_remains_opaque(self):
        item = row("relations", "foreign_table", relkind="f", rls="false", force_rls="false")
        self.assertIn("unprotected-new-relation:foreign_table", self.added(item)[1])
        result = self.added(item, new_global=[{"name": "foreign_table", "reason": "Synthetic"}])
        self.assertEqual(result[:2], ("contract", set()))

    def test_direct_nullable_column_without_default(self):
        self.assertEqual(self.added(row("columns", "t.new_col", "t"))[:2], ("expand", set()))

    def test_new_column_opaque_default_is_not_constant_proof(self):
        for not_null in ("true", "false"):
            with self.subTest(not_null=not_null):
                self.assertEqual(self.added(row("columns", "t.new_col", "t", default_sha256=H, not_null=not_null))[0], "contract")

    def test_new_column_identity_generated_or_grant_is_contract(self):
        for change in ({"identity": "a"}, {"generated": "s"}, {"acl": "MIGRATION_OWNER>PUBLIC:SELECT:false"}):
            with self.subTest(change=change):
                self.assertEqual(self.added(row("columns", "t.c", "t", **change))[0], "contract")

    def test_existing_column_semantic_changes_are_contract(self):
        before = [row("relations", "t"), row("columns", "t.c", "t")]
        for change in ({"type_sha256": J}, {"default_sha256": J}, {"collation_sha256": J}, {"generated": "s"}, {"identity": "d"}, {"not_null": "true"}, {"position": "3"}):
            with self.subTest(change=change):
                self.assertEqual(self.delta(before, [before[0], row("columns", "t.c", "t", **change)])[0], "contract")

    def test_nullable_direction_is_also_contract(self):
        self.assertEqual(self.delta([row("relations", "t"), row("columns", "t.c", "t", not_null="true")], [row("relations", "t"), row("columns", "t.c", "t")])[0], "contract")

    def test_remove_or_rename_is_contract(self):
        before = [row("relations", "t"), row("columns", "t.c", "t")]
        for after in ([before[0]], [before[0], row("columns", "t.renamed", "t")]):
            with self.subTest(after=after):
                self.assertEqual(self.delta(before, after)[0], "contract")

    def test_nonunique_index_does_not_prove_concurrently(self):
        result = self.added(row("indexes", "t.idx", "t"))
        self.assertEqual(result[0], "contract")
        self.assertIn("index-operation-unsupported", result[3])

    def test_unique_and_exclusion_indexes_are_contract(self):
        for change in ({"unique": "true"}, {"exclusion": "true"}):
            with self.subTest(change=change):
                self.assertEqual(self.added(row("indexes", "t.idx", "t", **change))[0], "contract")

    def test_new_constraints_and_trigger_are_contract(self):
        for kind, changes in (("constraints", {"type": "c"}), ("constraints", {"type": "u"}), ("constraints", {"type": "f"}), ("constraints", {"type": "x"}), ("triggers", {})):
            with self.subTest(kind=kind, changes=changes):
                self.assertEqual(self.added(row(kind, "t.rule", "t", **changes))[0], "contract")

    def test_existing_index_constraint_trigger_change(self):
        for kind in ("indexes", "constraints", "triggers"):
            with self.subTest(kind=kind):
                self.assertEqual(self.delta([row("relations", "t"), row(kind, "t.rule", "t")], [row("relations", "t"), row(kind, "t.rule", "t", definition_sha256=J)])[0], "contract")

    def test_direct_new_invoker_function(self):
        acl = "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>PUBLIC:EXECUTE:false"
        self.assertEqual(self.added(row("functions", "fn(" + H + ")", acl=acl))[:2], ("expand", set()))

    def test_new_definer_even_without_public_acl(self):
        result = self.added(row("functions", "fn(" + H + ")", security_definer="true"))
        self.assertEqual(result[0], "contract")
        self.assertIn("new definer function fn(" + H + ")", result[2])

    def test_existing_function_and_policy_hash_changes(self):
        for old, new in ((row("functions", "fn(" + H + ")"), row("functions", "fn(" + H + ")", source_sha256=J)),
                         (row("policies", "t.policy", "t"), row("policies", "t.policy", "t", using_sha256=J))):
            with self.subTest(kind=old["kind"]):
                self.assertEqual(self.delta([row("relations", "t"), old], [row("relations", "t"), new])[0], "contract")

    def test_existing_privileges_and_global_policy(self):
        for change in ({"owner": "OTHER"}, {"acl": "MIGRATION_OWNER>PUBLIC:SELECT:false"}):
            with self.subTest(change=change):
                self.assertEqual(self.delta([row("relations", "t")], [row("relations", "t", **change)])[0], "contract")
        self.assertEqual(self.delta([row("relations", "t")], [row("relations", "t")], [], [{"name": "t", "reason": "New exemption"}])[0], "contract")

    def test_new_opaque_type_sequence_extension(self):
        for kind in ("types", "sequences", "extensions"):
            with self.subTest(kind=kind):
                self.assertEqual(self.added(row(kind, "item"))[0], "contract")

    def test_additive_fingerprint_name_never_proves_membership(self):
        result = self.added(row("functions", "rereply_tenant_policy_additive_fingerprint_v2(" + H + ")"))
        self.assertIn("additive-profile-unsupported", result[1])
        self.assertIn("additive-profile-unsupported", result[3])

    def test_realistic_new_table_companions_do_not_claim_expand(self):
        # A real regenerated table includes its composite and array types,
        # columns, indexes, and policies. No implicit companion exemption.
        before = [row("relations", "t")]
        after = before + [row("relations", "new_table"), row("columns", "new_table.id", "new_table", not_null="true"),
                          row("types", "new_table", type="c"), row("types", "_new_table", type="b"),
                          row("indexes", "new_table.pk", "new_table", unique="true", primary="true"),
                          row("constraints", "new_table.pk", "new_table", type="p"), row("policies", "new_table.tenant", "new_table")]
        self.assertEqual(self.delta(before, after)[:2], ("contract", set()))

    def test_actual_current_catalog_loads_and_order_is_semantic_none(self):
        actual = json.loads((ROOT / sc.CATALOG).read_bytes())
        globals_ = json.loads((ROOT / sc.GLOBALS).read_bytes())
        self.assertEqual(len(actual["objects"]), 4432)
        sc._v2_catalog(actual)
        result = self.delta(actual["objects"], list(reversed(actual["objects"])), globals_, list(reversed(globals_)))
        self.assertEqual(result[:2], ("none", set()))

    def test_closed_attribute_and_identity_schema(self):
        bad = []
        missing = row("relations", "t"); del missing["attributes"]["acl"]; bad.append(missing)
        extra = row("relations", "t"); extra["attributes"]["sql"] = "DROP DATABASE anything"; bad.append(extra)
        bad.extend([row("relations", "t", rls=True), row("relations", "t", relkind="unknown"), row("relations", "not safe"),
                    row("relations", "t", owner="raw@example.test"), row("relations", "t", acl="OTHER>OTHER:SELECT:false"),
                    row("relations", "t", view_sha256="raw SQL"), row("relations", "t", acl="MIGRATION_OWNER>PUBLIC:INVALID:false")])
        for item in bad:
            with self.subTest(item=item):
                with self.assertRaises(sc._Invalid): sc._v2_catalog(catalog([item]))

    def test_duplicate_or_unbound_child_refuses(self):
        for objects in ([row("relations", "t"), row("relations", "t")], [row("columns", "missing.c", "missing")],
                        [row("relations", "t"), row("columns", "other.c", "t")]):
            with self.subTest(objects=objects):
                with self.assertRaises(sc._Invalid): sc._v2_catalog(catalog(objects))

    def test_acl_and_policy_roles_canonical(self):
        for item in (row("relations", "t", acl="MIGRATION_OWNER>PUBLIC:SELECT:false;MIGRATION_OWNER>PUBLIC:SELECT:false"),
                     row("policies", "t.p", "t", roles="RUNTIME,PUBLIC")):
            with self.subTest(item=item):
                with self.assertRaises(sc._Invalid): sc._v2_catalog(catalog([row("relations", "t"), item]))


class HistoryPolicyTests(unittest.TestCase):
    def check(self, before, after, old_version, new_version, changed, kind):
        reasons = set()
        engine = sc._v2_history(before, after, catalog(version=old_version), catalog(version=new_version), changed, kind, reasons)
        return reasons, engine

    def test_history_append_and_engine(self):
        self.assertEqual(self.check(history(), history(1, "expand", True), 0, 1, True, "expand"), (set(), True))

    def test_absent_false_is_equivalent(self):
        after = history(); after[0]["apply_engine"] = False
        self.assertEqual(self.check(history(), after, 0, 0, False, "none"), (set(), False))

    def test_history_rewrite_and_delete_refuse(self):
        before = history(1)
        for after in (history(), [{"version": 0, "class": "baseline", "min_reader_version": 0, "apply_engine": True}, before[1]]):
            with self.subTest(after=after): self.assertIn("history-rewritten", self.check(before, after, 1, after[-1]["version"], False, "none")[0])

    def test_version_must_change_iff_sections_change(self):
        self.assertIn("history-version-change-mismatch", self.check(history(), history(1), 0, 1, False, "none")[0])
        self.assertIn("history-version-change-mismatch", self.check(history(), history(), 0, 0, True, "contract")[0])

    def test_version_matches_history(self):
        self.assertIn("history-catalog-version-mismatch", self.check(history(), history(), 0, 1, True, "expand")[0])

    def test_floor_only_contract_and_never_decreases(self):
        self.assertIn("history-reader-requires-contract", self.check(history(), history(1, "expand", True, 1), 0, 1, True, "expand")[0])
        self.assertEqual(self.check(history(), history(1, "contract", True, 1), 0, 1, True, "contract")[0], set())
        before = history(1, "contract", True, 1)
        after = before + [{"version": 2, "class": "contract", "min_reader_version": 0, "apply_engine": True}]
        self.assertIn("history-reader-decreased", self.check(before, after, 1, 2, True, "contract")[0])

    def test_declared_expand_cannot_understate_contract(self):
        self.assertIn("history-class-understated", self.check(history(), history(1, "expand", True), 0, 1, True, "contract")[0])

    def test_future_pending_data_and_step_history_grammar_refuses(self):
        for change in ({"pending": True}, {"steps": ["create_state"]}, {"class": "data"}):
            after = history(1); after[1].update(change)
            with self.subTest(change=change):
                with self.assertRaises(sc._Invalid): self.check(history(), after, 0, 1, True, "contract")


class SourceHistoryPolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.base_files = {sc.CATALOG: text(catalog()), sc.HISTORY: text(history()), sc.GLOBALS: "[]\n",
                           sc.MIGRATION_PATH: text(migration()), "cmd/whatomate/main.go": MAIN,
                           "internal/database/tenant.go": TENANT, "README.md": "synthetic\n"}
        self.base = self.repo.commit(self.base_files, "synthetic policy baseline")

    def result(self, files, base=None):
        head = self.repo.commit(files)
        return sc.classify_v2(self.repo.root, base or self.base, head)

    def test_shared_blob_reads_are_once_per_invocation_and_refetched_next_time(self):
        middle = self.repo.commit({"README.md": "middle\n"})
        head = self.repo.commit({"README.md": "head\n"})
        real_git = sc._git
        listings = []

        def count(root, args):
            if args[:2] == ["ls-tree", "-z"]:
                listings.append((args[2], args[-1]))
            return real_git(root, args)

        with mock.patch.object(sc, "_git", side_effect=count):
            first = sc.classify_v2(self.repo.root, self.base, head)
            self.assertEqual((first.kind, first.dormant, first.reasons), ("none", False, ()))
            self.assertEqual(len(listings), len(set(listings)))
            for revision in (self.base, middle, head):
                self.assertEqual(listings.count((revision, sc.MIGRATION_PATH)), 1)
            self.assertEqual(listings.count((head, sc.REGISTRY)), 1)  # Successful absence is reusable.
            first_keys = set(listings)
            second = sc.classify_v2(self.repo.root, self.base, head)
            self.assertEqual(second, first)
            self.assertEqual(set(listings), first_keys)
            self.assertTrue(all(listings.count(key) == 2 for key in first_keys))

    def test_cached_missing_intermediate_contract_still_blocks_restored_endpoint(self):
        removed = self.repo.commit({sc.MIGRATION_PATH: None})
        head = self.repo.commit({sc.MIGRATION_PATH: text(migration())})
        real_git = sc._git
        missing_reads = []

        def count(root, args):
            if args == ["ls-tree", "-z", removed, "--", sc.MIGRATION_PATH]:
                missing_reads.append(removed)
            return real_git(root, args)

        with mock.patch.object(sc, "_git", side_effect=count):
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual(missing_reads, [removed])
        self.assertIn("migration-path-changed:contract-removed", result.reasons)
        self.assertEqual(result.migration_changes, ())

    def test_cached_legacy_catalog_still_receives_stricter_v2_validation(self):
        invalid = catalog()
        invalid["objects"][0]["attributes"] = {}
        head = self.repo.commit({sc.CATALOG: text(invalid)})
        legacy = sc.classify(self.repo.root, self.base, head)
        self.assertNotIn("classifier-invalid:catalog", legacy.reasons)
        real_git = sc._git
        reads = []

        def count(root, args):
            if args == ["ls-tree", "-z", head, "--", sc.CATALOG]:
                reads.append(head)
            return real_git(root, args)

        with mock.patch.object(sc, "_git", side_effect=count):
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual(reads, [head])
        self.assertIn("classifier-v2-invalid:catalog", result.reasons)

    def test_second_ancestry_transport_failure_is_not_hidden_by_blob_cache(self):
        head = self.repo.commit({"README.md": "head\n"})
        real_git = sc._git
        ancestry = []

        def fail_second(root, args):
            if args == ["merge-base", self.base, head]:
                ancestry.append(tuple(args))
                if len(ancestry) == 2:
                    raise sc.common.ReleaseError("git-failed:schema-guard")
            return real_git(root, args)

        with mock.patch.object(sc, "_git", side_effect=fail_second):
            with self.assertRaises(sc.common.ReleaseError) as caught:
                sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual(caught.exception.code, "git-failed:schema-guard")
        self.assertEqual(len(ancestry), 2)

    def test_unchanged_catalog_and_unrelated_body(self):
        result = self.result({"cmd/whatomate/main.go": MAIN.replace("println(2)", "println(3)")})
        self.assertEqual((result.kind, result.dormant, result.reasons, result.migration_changes), ("none", False, (), ()))
        self.assertIn("compat-required", result.requirements)
        with self.assertRaises(AttributeError): result.kind = "expand"

    def test_workflow_and_hidden_config_changes_are_valid_repository_paths(self):
        result = self.result({".github/workflows/compat.yml": "name: synthetic\n",
                              ".gitignore": "synthetic-output/\n",
                              ".config/tool.json": "{}\n"})
        self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
        # Hidden repository files do not broaden the registry identifier grammar.
        self.assertFalse(sc._path(".github/workflows/compat.yml"))

    def test_workflow_change_does_not_hide_an_unsafe_migration_change(self):
        result = self.result({".github/workflows/compat.yml": "name: synthetic\n",
                              "cmd/whatomate/main.go": MAIN.replace("println(1)", "println(9)")})
        self.assertIn("migration-contract-stale", result.reasons)
        self.assertNotIn("classifier-v2-invalid:history-range", result.reasons)

    def test_changed_path_projection_still_refuses_unsafe_or_oversized_names(self):
        head = self.repo.commit({"README.md": "changed\n"})
        for path in ("../outside", ".github/../outside", ".git/config", "x/.GIT/config",
                     "/absolute", "C:/absolute", "x//y", "x\\y", "x\ny", "x" * 513):
            with self.subTest(path=path), mock.patch.object(sc, "changed_paths", return_value=[("M", path)]):
                result = sc.classify_v2(self.repo.root, self.base, head)
                self.assertIn("classifier-v2-invalid:history-range", result.reasons)

    def test_valid_migration_change_only_new_api_is_nonblocking(self):
        head = self.repo.commit({"cmd/whatomate/main.go": MAIN.replace("println(1)", "println(3)"), sc.MIGRATION_PATH: text(migration(J))})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual(result.reasons, ())
        self.assertIn("generated migration closure changed", result.migration_changes)
        self.assertIn("migration path changed: cmd/whatomate/main.go#runRLSMigration", result.migration_changes)
        self.assertIn("migration-path-changed:closure", sc.check(self.repo.root, self.base, head))
        with self.assertRaises(sc.SchemaChangeBlocked): sc.guard(self.repo.root, self.base, head)
        self.assertEqual(sc.classify(self.repo.root, self.base, head, mode="rollback").reasons, ())

    def test_transitive_closure_change_does_not_invent_function(self):
        result = self.result({sc.MIGRATION_PATH: text(migration(J))})
        self.assertEqual(result.migration_changes, ("generated migration closure changed",))

    def test_named_migration_with_stale_generated_hash_stays_blocked(self):
        result = self.result({"cmd/whatomate/main.go": MAIN.replace("println(1)", "println(9)")})
        self.assertIn("migration-contract-stale", result.reasons)
        self.assertIn("migration-path-changed:cmd/whatomate/main.go#runRLSMigration", result.reasons)

    def test_missing_contract_remains_blocked(self):
        result = self.result({sc.MIGRATION_PATH: None})
        self.assertIn("migration-contract-required", result.reasons)
        self.assertIn("migration-path-changed:contract-removed", result.reasons)

    def test_malformed_contract_and_roots_refuse(self):
        value = migration(); value["roots"] = value["roots"][:-1]
        self.assertIn("classifier-v2-invalid:migration-path", self.result({sc.MIGRATION_PATH: text(value)}).reasons)

    def test_valid_introduction_retains_whole_file_fence(self):
        absent = self.repo.commit({sc.MIGRATION_PATH: None})
        good = self.repo.commit({sc.MIGRATION_PATH: text(migration())})
        self.assertEqual(sc.classify_v2(self.repo.root, absent, good).reasons, ())
        bundled = self.repo.commit({"cmd/whatomate/main.go": MAIN.replace("println(1)", "println(9)")})
        result = sc.classify_v2(self.repo.root, absent, bundled)
        self.assertIn("migration-path-changed:cmd/whatomate/main.go#runRLSMigration", result.reasons)

    def test_introduction_with_bundled_file_change_refuses(self):
        absent = self.repo.commit({sc.MIGRATION_PATH: None})
        result = self.result({sc.MIGRATION_PATH: text(migration()), "cmd/whatomate/main.go": MAIN + "// bundled\n"}, base=absent)
        self.assertIn("migration-path-changed:cmd/whatomate/main.go", result.reasons)

    def test_transient_migration_removal_cannot_be_laundered(self):
        self.repo.commit({sc.MIGRATION_PATH: None})
        result = self.result({sc.MIGRATION_PATH: text(migration())})
        self.assertIn("migration-path-changed:contract-removed", result.reasons)

    def test_registry_revision_then_seed_diff_is_dormant_data(self):
        base = self.repo.commit({sc.REGISTRY: text([step()])})
        result = self.result({sc.REGISTRY: text([step(2, J)]), sc.SEEDS: text({"version": 0, "synthetic": ["new_key"]})}, base)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("data", True, ()))
        self.assertTrue(any("new_key" in line for line in result.summary))

    def test_registry_changed_without_revision_refuses(self):
        base = self.repo.commit({sc.REGISTRY: text([step()])})
        self.assertIn("data-step-edited-without-rev:seed", self.result({sc.REGISTRY: text([step(digest=J)])}, base).reasons)

    def test_registry_transient_row_removal_refuses(self):
        base = self.repo.commit({sc.REGISTRY: text([step()])})
        self.repo.commit({sc.REGISTRY: "[]"})
        self.assertIn("data-step-removed:seed", self.result({sc.REGISTRY: text([step(2)])}, base).reasons)

    def test_registry_introduction_bundled_source_refuses(self):
        result = self.result({sc.REGISTRY: text([step()]), "cmd/whatomate/main.go": MAIN + "// changed\n"})
        self.assertIn("registry-introduction-changed:cmd/whatomate/main.go", result.reasons)

    def test_direct_expand_policy_keeps_future_producer_unavailable(self):
        result = self.result({sc.CATALOG: text(catalog([row("relations", "t"), row("columns", "t.c", "t")], 1)), sc.HISTORY: text(history(1, "expand", True))})
        self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", False, ()))
        self.assertTrue({"compat-required", "restore-drills", "producer-class-unsupported", "catalog-format/profile-unsupported"} <= set(result.requirements))
        self.assertTrue(sc.check(self.repo.root, self.base, self.repo.head()))

    def test_active_catalog_engine_false_cannot_claim_dormant_pending(self):
        result = self.result({sc.CATALOG: text(catalog([row("relations", "t"), row("columns", "t.c", "t")], 1)), sc.HISTORY: text(history(1, "expand", False))})
        self.assertIn("active-catalog-without-apply-engine", result.reasons)
        self.assertFalse(result.dormant)

    def test_mixed_data_and_expand_takes_data_class(self):
        result = self.result({sc.CATALOG: text(catalog([row("relations", "t"), row("columns", "t.c", "t")], 1)), sc.HISTORY: text(history(1, "expand", True)), sc.SEEDS: text({"version": 0, "key": "value"})})
        self.assertEqual((result.kind, result.dormant), ("data", False))
        self.assertIn("producer-class-unsupported", result.requirements)

    def test_contract_dominates_data_and_requires_label(self):
        result = self.result({sc.CATALOG: text(catalog([row("relations", "t", rls="false")], 1)), sc.HISTORY: text(history(1, "contract", True)), sc.SEEDS: text({"version": 0})})
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_reader_floor_raises_expand_to_contract(self):
        result = self.result({sc.CATALOG: text(catalog([row("relations", "t"), row("columns", "t.c", "t")], 1)), sc.HISTORY: text(history(1, "contract", True, 1))})
        self.assertEqual((result.kind, result.reasons), ("contract", ()))
        self.assertIn("contract-label", result.requirements)

    def test_current_registry_introduction_all_actual_seed_lines(self):
        # Actual public artifacts and every declared closure source are copied
        # into this disposable repo, not read via Git from the workspace.
        registry = json.loads((ROOT / sc.REGISTRY).read_bytes())
        contract = json.loads((ROOT / sc.MIGRATION_PATH).read_bytes())
        paths = set(contract["closure_files"])
        for item in registry: paths.update(item["closure_files"])
        files = {p: (ROOT / p).read_text(encoding="utf-8") for p in paths}
        files.update({p: (ROOT / p).read_text(encoding="utf-8") for p in (sc.CATALOG, sc.HISTORY, sc.GLOBALS)})
        files[sc.MIGRATION_PATH] = None
        base = self.repo.commit(files)
        additions = {p: (ROOT / p).read_text(encoding="utf-8") for p in (sc.REGISTRY, sc.SEEDS, sc.MIGRATION_PATH)}
        result = self.result(additions, base)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("data", True, ()))
        self.assertEqual(sum(s.startswith("seeds diff +") for s in result.summary), len(additions[sc.SEEDS].splitlines()))
        self.assertGreater(sum(s.startswith("seeds diff +") for s in result.summary), 512)

    def test_core_tenant_change_contract_without_catalog_guess(self):
        result = self.result({"internal/database/tenant.go": TENANT.replace('"t"', '"new_tenant"'), sc.MIGRATION_PATH: text(migration(J))})
        self.assertEqual(result.kind, "contract")
        self.assertIn("tenant profile declarations changed", result.summary)

    def test_future_additive_grammar_is_unsupported(self):
        result = self.result({"internal/database/tenant.go": TENANT + "\nvar tenantRLSAdditiveProfileV2 = []string{\"new_table\"}\n", sc.MIGRATION_PATH: text(migration(J))})
        self.assertIn("additive-profile-unsupported", result.reasons)

    def test_shape_only_metadata_preserves_current_boundary(self):
        result = self.result({"internal/dbcatalog/shape/production-v0.sql": "-- synthetic metadata\n"})
        self.assertEqual((result.kind, result.reasons), ("none", ()))

    def test_canonical_pending_files_are_unsupported_not_guessed(self):
        path = sc.GOLDEN + "pending/"
        result = self.result({path + "catalog-v1.json": text(catalog(version=1)), path + "global_tables.pending.json": "[]"})
        self.assertIn("pending-format-unsupported", result.reasons)
        self.assertNotEqual((result.kind, result.dormant), ("expand", True))
        removed = self.repo.commit({path + "catalog-v1.json": None, path + "global_tables.pending.json": None})
        self.assertIn("pending-format-unsupported", sc.classify_v2(self.repo.root, self.repo.git("rev-parse", removed + "^").strip(), removed).reasons)

    def test_pending_history_named_steps_remain_unsupported(self):
        future = history(); future.append({"version": 1, "class": "expand", "min_reader_version": 0, "pending": True, "steps": ["create_state"]})
        result = self.result({sc.HISTORY: text(future)})
        self.assertIn("classifier-v2-invalid:history", result.reasons)
        self.assertIn("history-format-unsupported", result.requirements)

    def test_apply_engine_flag_alone_is_not_authority(self):
        value = history(); value[0]["apply_engine"] = True
        result = self.result({sc.HISTORY: text(value)})
        self.assertIn("history-rewritten", result.reasons)
        self.assertIn("catalog-format/profile-unsupported", result.requirements)

    def test_gated_inventory_reports_holdback_without_evaluation(self):
        result = self.result({sc.REGISTRY: text([step(requires_gates=["synthetic_gate"]), step(id="second")])})
        self.assertTrue(any("holdback review required" in line for line in result.summary))

    def test_invalid_dormant_and_new_uncovered_call_still_block(self):
        result = self.result({"pkg/new/new.go": "package new\nfunc Run() { BackfillSomething(db) }\n", sc.DORMANT: text([{"file": "pkg/new/new.go", "func": "Run"}])})
        self.assertIn("classifier-invalid:dormant-entrypoints", result.reasons)

    def test_outside_tree_migration_call_is_not_lifted(self):
        result = self.result({"pkg/new/new.go": "package new\nfunc Run() { BackfillSomething(db) }\n"})
        self.assertIn("migration-call:pkg/new/new.go", result.reasons)

    def test_duplicate_json_keys_refuse_without_printing_payload(self):
        result = self.result({sc.CATALOG: '{"version":0,"version":0,"objects":[]}'})
        self.assertIn("classifier-v2-invalid:catalog", result.reasons)

    def test_artifact_size_and_mode_bounds(self):
        head = self.repo.commit({"README.md": "changed"})
        real_git = sc._git
        def oversized(root, args):
            if args[:2] == ["cat-file", "-s"]: return str(sc.MAX_ARTIFACT_BYTES + 1).encode()
            return real_git(root, args)
        with mock.patch.object(sc, "_git", side_effect=oversized):
            self.assertIn("classifier-v2-invalid:catalog", sc.classify_v2(self.repo.root, self.base, head).reasons)
        def symlink(root, args):
            output = real_git(root, args)
            if args and args[0] == "ls-tree" and args[-1] == sc.CATALOG:
                return output.replace(b"100644 blob", b"120000 blob")
            return output
        with mock.patch.object(sc, "_git", side_effect=symlink):
            self.assertIn("classifier-v2-invalid:catalog", sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_first_parent_history_cap_refuses_before_reading_rows(self):
        head = self.repo.commit({"README.md": "changed"})
        real_git = sc._git
        def capped(root, args):
            if args and args[0] == "rev-list": return (head.encode() + b"\n") * (sc.MAX_HISTORY_COMMITS + 1)
            return real_git(root, args)
        with mock.patch.object(sc, "_git", side_effect=capped):
            self.assertIn("classifier-v2-invalid:migration-path", sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_no_catalog_baseline_does_not_lift_freeze(self):
        base = self.repo.commit({sc.CATALOG: None})
        result = self.result({"internal/database/new.go": "package database\n"}, base)
        self.assertIn("catalog-baseline-required", result.reasons)
        self.assertIn("guarded-tree:internal/database/new.go", result.reasons)

    def test_nonancestral_source_refuses(self):
        left = self.repo.commit({"README.md": "left\n"})
        self.repo.checkout(self.base)
        result = self.result({"README.md": "right\n"}, left)
        self.assertIn("classifier-v2-invalid:history-range", result.reasons)


class PendingSourceHistoryTests(unittest.TestCase):
    """Real immutable commits; planned operations never stand in for SQL proof."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.args = pending_fixture.fixture()
        self.files = {name: raw.decode() for name, raw in self.args["files"].items()}
        self.base = self.repo.commit({sc.CATALOG: self.args["active_catalog"].decode(),
                                     sc.GLOBALS: self.args["active_globals"].decode(),
                                     sc.HISTORY: self.args["active_history"].decode(),
                                     sc.MIGRATION_PATH: text(migration()), "cmd/whatomate/main.go": MAIN,
                                     "internal/database/tenant.go": TENANT, "README.md": "baseline\n"})

    def classify(self, head, base=None):
        return sc.classify_v2(self.repo.root, base or self.base, head)

    def changed_plan(self, change):
        plan = json.loads(self.files[sc.pending_plan.PLAN])
        change(plan)
        return sc.pending_plan.canonical(plan).decode()

    def test_pr22_shaped_introduction_requires_source_bound_dormant_compatibility(self):
        introduced = self.repo.commit(self.files)
        head = self.repo.commit({"README.md": "after introduction\n"})
        result = self.classify(head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", True, ()))
        self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertIn("pending source plan: active=0; target=1; source=" + head, result.summary)
        self.assertIn("pending source step: create_ledger; operation=create_relation; source_sha256=" + H + "; requires_gates=schema_ready", result.summary)
        self.assertEqual(sum(line.startswith("pending source file:") for line in result.summary), 3)
        self.assertEqual(sum(line.startswith("pending source step:") for line in result.summary), 4)
        self.assertIn("pending source plan only; actual ledger/apply state unavailable; gates not evaluated", result.summary)
        self.assertIn("pending gates: a failed gate would hold back unrelated steps; holdback review required", result.summary)
        # Legacy behavior remains distinct and cannot claim the new source rule.
        legacy = sc.classify(self.repo.root, self.base, introduced)
        self.assertEqual((legacy.kind, legacy.dormant, legacy.reasons), ("none", False, ()))

    def test_unchanged_plan_unrelated_change_stays_none_and_requires_compatibility(self):
        base = self.repo.commit(self.files)
        head = self.repo.commit({"README.md": "unrelated\n"})
        for end in (base, head):
            with self.subTest(end=end):
                result = self.classify(end, base)
                self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
                self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_supported_pending_does_not_remove_an_existing_producer_refusal(self):
        head = self.repo.commit(self.files)
        reasons = {"unrelated-source-refusal"}
        requirements = {"producer-class-unsupported", "unrelated-requirement"}
        self.assertTrue(sc._v2_pending(sc._Repository(self.repo.root), self.base, head,
                                      reasons, [], requirements))
        self.assertEqual(reasons, {"unrelated-source-refusal"})
        self.assertEqual(requirements, {"producer-class-unsupported", "unrelated-requirement"})

    def test_pending_reads_use_committed_blobs_not_working_tree(self):
        head = self.repo.commit(self.files)
        self.repo.write({sc.pending_plan.PLAN: "private invalid working-tree bytes"})
        result = self.classify(head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", True, ()))
        self.assertNotIn("private", " ".join(result.summary))

    def test_removed_and_reintroduced_bundle_refuses_even_with_equal_endpoints(self):
        base = self.repo.commit(self.files)
        self.repo.commit({name: None for name in self.files})
        head = self.repo.commit(self.files)
        result = self.classify(head, base)
        self.assertIn("classifier-v2-invalid:pending", result.reasons)
        self.assertIn("producer-class-unsupported", result.requirements)

    def test_transient_introduction_removed_at_head_cannot_hide_between_empty_endpoints(self):
        self.repo.commit(self.files)
        head = self.repo.commit({name: None for name in self.files})
        result = self.classify(head)
        self.assertIn("classifier-v2-invalid:pending", result.reasons)
        self.assertIn("pending-format-unsupported", result.reasons)

    def test_valid_replacement_then_restore_is_not_laundered(self):
        base = self.repo.commit(self.files)
        changed = self.changed_plan(lambda doc: doc["steps"][0].update(source_sha256=J))
        sc.pending_plan.parse_bundle(**{**self.args, "files": {**self.args["files"], sc.pending_plan.PLAN: changed.encode()}})
        self.repo.commit({sc.pending_plan.PLAN: changed})
        head = self.repo.commit(self.files)
        self.assertIn("classifier-v2-invalid:pending", self.classify(head, base).reasons)

    def test_invalid_intermediate_before_valid_introduction_refuses(self):
        self.repo.commit({sc.pending_plan.PLAN: "{}\n"})
        head = self.repo.commit(self.files)
        self.assertIn("classifier-v2-invalid:pending", self.classify(head).reasons)

    def test_bundle_closed_hash_canonical_and_dag_failures_refuse(self):
        variants = [
            {sc.pending_plan.PLAN: None},
            {sc.pending_plan.PREFIX + "extra.json": "{}\n"},
            {sc.pending_plan.PLAN: json.dumps(json.loads(self.files[sc.pending_plan.PLAN]), indent=2)},
            {sc.pending_plan.PLAN: self.changed_plan(lambda d: d.update(active_catalog_file_sha256=J))},
            {sc.pending_plan.PLAN: self.changed_plan(lambda d: d["steps"][0].update(depends_on=["grant_runtime"]))},
            {sc.pending_plan.PLAN: self.changed_plan(lambda d: d["steps"][0].update(operation="executed_concurrently"))},
        ]
        for delta in variants:
            with self.subTest(delta=list(delta)):
                self.repo.checkout(self.base)
                head = self.repo.commit({**self.files, **delta})
                result = self.classify(head)
                self.assertIn("classifier-v2-invalid:pending", result.reasons)
                self.assertIn("pending-format-unsupported", result.reasons)
                self.assertNotEqual((result.kind, result.dormant), ("expand", True))

    def test_non_expand_declarations_are_not_relabelled_as_expansion(self):
        for kind in ("data", "contract"):
            with self.subTest(kind=kind):
                self.repo.checkout(self.base)
                changed = self.changed_plan(lambda d: d["entries"][0].update({"class": kind}))
                head = self.repo.commit({**self.files, sc.pending_plan.PLAN: changed})
                result = self.classify(head)
                self.assertEqual((result.kind, result.dormant), ("none", False))
                self.assertIn("pending-format-unsupported", result.reasons)
                self.assertIn("producer-class-unsupported", result.requirements)

    def test_active_byte_edits_cannot_rebind_an_existing_plan_or_first_introduction(self):
        for path, field in ((sc.CATALOG, "active_catalog_file_sha256"), (sc.GLOBALS, "active_globals_file_sha256"),
                            (sc.HISTORY, "active_history_file_sha256")):
            for existing in (False, True):
                with self.subTest(path=path, existing=existing):
                    self.repo.checkout(self.base)
                    base = self.repo.commit(self.files) if existing else self.base
                    raw = self.repo.git("show", self.base + ":" + path) + " \n"
                    changed = self.changed_plan(lambda d: d.update({field: hashlib.sha256(raw.encode()).hexdigest()}))
                    head = self.repo.commit({**self.files, path: raw, sc.pending_plan.PLAN: changed})
                    self.assertIn("classifier-v2-invalid:pending", self.classify(head, base).reasons)

    def test_pending_never_downgrades_data_active_change_or_contract(self):
        cases = [({sc.SEEDS: text({"version": 0, "synthetic": ["new_key"]})}, "data", True),
                 ({sc.SEEDS: text({"version": 0}), sc.HISTORY: text([{"version": 0, "class": "baseline", "min_reader_version": 0, "apply_engine": True}])}, "data", False),
                 ({"internal/database/tenant.go": TENANT.replace('"t"', '"changed"'), sc.MIGRATION_PATH: text(migration(J))}, "contract", False)]
        for delta, kind, dormant in cases:
            with self.subTest(kind=kind, dormant=dormant):
                self.repo.checkout(self.base)
                result = self.classify(self.repo.commit({**self.files, **delta}))
                self.assertEqual((result.kind, result.dormant), (kind, dormant))
                if dormant:
                    self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))
                else:
                    self.assertIn("producer-class-unsupported", result.requirements)
                if not dormant:
                    # The active-engine refusal occurs before final requirement
                    # collection; its existing refusal remains sufficient.
                    self.assertTrue(result.reasons or "restore-drills" in result.requirements)

    def test_active_catalog_floor_change_is_not_dormant_pending(self):
        target = json.loads(self.files[sc.pending_plan.PREFIX + "catalog-v1.json"])
        result = self.classify(self.repo.commit({**self.files, sc.CATALOG: text(target), sc.HISTORY: text(history(1, "contract", True, 1))}))
        self.assertEqual((result.kind, result.dormant), ("contract", False))
        self.assertIn("classifier-v2-invalid:pending", result.reasons)
        self.assertIn("catalog-format/profile-unsupported", result.requirements)

    def test_migration_guards_and_observations_remain_separate(self):
        for generated in (False, True):
            with self.subTest(generated=generated):
                self.repo.checkout(self.base)
                delta = {"cmd/whatomate/main.go": MAIN.replace("println(1)", "println(9)")}
                if generated: delta[sc.MIGRATION_PATH] = text(migration(J))
                result = self.classify(self.repo.commit({**self.files, **delta}))
                self.assertEqual((result.kind, result.dormant), ("expand", True))
                if generated:
                    self.assertEqual(result.reasons, ())
                    self.assertIn("generated migration closure changed", result.migration_changes)
                else:
                    self.assertIn("migration-contract-stale", result.reasons)

    def test_closed_listing_rejects_bad_nul_duplicate_paths_and_directory_blob(self):
        head = self.repo.commit(self.files)
        real_git = sc._git
        normal = b"\0".join(name.encode() for name in sorted(self.files)) + b"\0"
        for raw in (normal[:-1], normal + b"\0", normal + normal.split(b"\0")[0] + b"\0",
                    normal.replace(b"catalog-v1.json", b"../catalog-v1.json"), b"\xff\0",
                    sc.pending_plan.PREFIX.rstrip("/").encode() + b"\0"):
            def capture(root, args):
                if args[:4] == ["ls-tree", "-r", "--name-only", "-z"] and args[4] == head and args[-1] == sc._V2_PENDING.rstrip("/"):
                    return raw
                return real_git(root, args)
            with self.subTest(length=len(raw)), mock.patch.object(sc, "_git", side_effect=capture):
                self.assertIn("classifier-v2-invalid:pending", self.classify(head).reasons)

    def test_pending_regular_blob_and_existing_per_blob_cap_are_enforced(self):
        head = self.repo.commit(self.files)
        real_git = sc._git
        for failure in ("mode", "size"):
            pending_blob = False
            def capture(root, args):
                nonlocal pending_blob
                if args[:2] == ["ls-tree", "-z"] and args[-1].startswith(sc._V2_PENDING):
                    pending_blob = True
                    raw = real_git(root, args)
                    return raw.replace(b"100644 blob", b"120000 blob") if failure == "mode" else raw
                if pending_blob and args[:2] == ["cat-file", "-s"]:
                    return str(sc.MAX_ARTIFACT_BYTES + 1).encode()
                return real_git(root, args)
            with self.subTest(failure=failure), mock.patch.object(sc, "_git", side_effect=capture):
                self.assertIn("classifier-v2-invalid:pending", self.classify(head).reasons)

    def test_actual_blob_at_pending_directory_name_is_not_an_empty_inventory(self):
        head = self.repo.commit({sc.pending_plan.PREFIX.rstrip("/"): "not a pending directory\n"})
        result = self.classify(head)
        self.assertIn("classifier-v2-invalid:pending", result.reasons)
        self.assertIn("pending-format-unsupported", result.reasons)

    def test_empty_history_prefix_is_checked_once_per_commit_and_has_no_blob_cost(self):
        middle = self.repo.commit({"README.md": "middle\n"})
        head = self.repo.commit({"README.md": "head\n"})
        real_git = sc._git
        commands = []
        def capture(root, args):
            result = real_git(root, args)
            commands.append((tuple(args), len(result)))
            return result
        repo = sc._Repository(self.repo.root)
        with mock.patch.object(sc, "_git", side_effect=capture):
            self.assertFalse(sc._v2_pending(repo, self.base, head, set(), [], set()))
        self.assertEqual(len(commands), 4)  # One bounded range plus base and both first parents.
        self.assertEqual(sum(size for _, size in commands), 82)
        self.assertEqual([args[4] for args, _ in commands[1:]], [self.base, middle, head])
        self.assertEqual((repo.total, repo.cache), (0, {}))

    def test_real_immutable_git_scope_and_exhaustion_keep_fixed_grammar_and_no_fallback(self):
        head = self.repo.commit(self.files)
        reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
        with reader.operation():
            result = self.classify(head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", True, ()))
        self.assertTrue(reader.closed)
        for reason in ("calls", "bytes", "deadline"):
            real_git = sc._git
            requests = []
            def refuse(root, args):
                if args[:4] == ["ls-tree", "-r", "--name-only", "-z"] and args[-1] == sc._V2_PENDING.rstrip("/"):
                    requests.append(tuple(args))
                    raise source_git.SourceGitRefusal("git-failed:source-" + reason)
                return real_git(root, args)
            with self.subTest(reason=reason), mock.patch.object(sc, "_git", side_effect=refuse):
                with self.assertRaises(source_git.SourceGitRefusal) as caught:
                    self.classify(head)
                self.assertEqual(caught.exception.code, "git-failed:source-" + reason)
                self.assertEqual(len(requests), 1)

    def test_pending_range_cap_and_malformed_revision_refuse_before_listing(self):
        # Isolate the new history traversal; legacy traversal has its own cap.
        head = self.repo.commit({"README.md": "head\n"})
        for raw in ((head.encode() + b"\n") * (sc.MAX_HISTORY_COMMITS + 1), b"private-invalid-revision\n",
                    (head + "\n" + head + "\n").encode(), b""):
            commands = []
            def capture(root, args):
                commands.append(tuple(args))
                return raw
            with self.subTest(length=len(raw)), mock.patch.object(sc, "_git", side_effect=capture):
                with self.assertRaises(sc._Invalid):
                    sc._v2_pending(sc._Repository(self.repo.root), self.base, head, set(), [], set())
                self.assertEqual(len(commands), 1)


class CISourceGuardTests(unittest.TestCase):
    """Source eligibility is separate from actual generated/compatibility proof."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.profile = "internal/dbcatalog/golden/production-v0.json"
        self.overlay = "internal/dbcatalog/shape/production-v0.sql"
        self.active = {
            sc.CATALOG: text(catalog()), sc.HISTORY: text(history()), sc.GLOBALS: "[]\n",
            self.profile: '{"fixture":"profile"}\n', self.overlay: "-- synthetic overlay\n",
            sc.REGISTRY: "[]\n", sc.SEEDS: '{"version":0}\n',
        }
        self.files = {**self.active, sc.MIGRATION_PATH: text(migration()),
                      "cmd/whatomate/main.go": MAIN, "internal/database/tenant.go": TENANT,
                      "README.md": "synthetic baseline\n"}
        self.base = self.repo.commit(self.files)

    def closure_head(self, extra=None):
        return self.repo.commit({sc.MIGRATION_PATH: text(migration(J)), **(extra or {})})

    def check(self, head, base=None):
        return sc.check_ci_source(self.repo.root, base or self.base, head)

    def test_only_new_ci_predicate_admits_complete_closure_only_source(self):
        head = self.closure_head({"internal/dbcatalog/capture.go": "package dbcatalog\n"})
        self.assertEqual(self.check(head), ())
        self.assertEqual(sc.check(self.repo.root, self.base, head), ("migration-path-changed:closure",))
        with self.assertRaises(sc.SchemaChangeBlocked):
            sc.guard(self.repo.root, self.base, head)
        self.assertEqual(sc.classify_v2(self.repo.root, self.base, head).kind, "none")

    def test_legacy_clean_path_does_not_require_new_material_or_v2(self):
        base = self.repo.commit({self.profile: None, self.overlay: None})
        head = self.repo.commit({"README.md": "legacy clean\n"})
        with mock.patch.object(sc, "_classify_v2", side_effect=AssertionError("not the new exception")):
            self.assertEqual(self.check(head, base), ())
        self.assertEqual(sc.check(self.repo.root, base, head), ())

    def test_named_migration_change_is_not_part_of_the_exception(self):
        head = self.closure_head({"cmd/whatomate/main.go": MAIN.replace("println(1)", "println(7)")})
        self.assertEqual(self.check(head), sc.check(self.repo.root, self.base, head))
        self.assertIn("migration-path-changed:cmd/whatomate/main.go#runRLSMigration", self.check(head))

    def test_manifest_file_list_change_without_new_hash_refuses(self):
        value = migration()
        value["closure_files"].append("internal/models/new.go")
        head = self.repo.commit({sc.MIGRATION_PATH: text(value), "internal/models/new.go": "package models\n"})
        self.assertEqual(self.check(head), ("migration-path-changed:closure",))

    def test_wrong_roots_or_invalid_manifest_remain_refused(self):
        value = migration(J)
        value["roots"] = value["roots"][:-1]
        for raw in (text(value), '{"private":"not a manifest"}\n'):
            with self.subTest(raw=raw):
                head = self.repo.commit({sc.MIGRATION_PATH: raw})
                self.assertTrue(self.check(head))

    def test_removed_intermediate_contract_is_not_laundered(self):
        self.repo.commit({sc.MIGRATION_PATH: None})
        head = self.closure_head()
        self.assertIn("migration-path-changed:contract-removed", self.check(head))

    def test_first_introduction_with_bundled_source_keeps_original_fence(self):
        absent = self.repo.commit({sc.MIGRATION_PATH: None})
        head = self.closure_head({"cmd/whatomate/main.go": MAIN + "// bundled change\n"})
        self.assertTrue(self.check(head, absent))
        self.assertEqual(self.check(head, absent), sc.check(self.repo.root, absent, head))

    def test_every_active_blob_must_exist_at_both_endpoints(self):
        for path in self.active:
            with self.subTest(path=path):
                base = self.repo.commit({**self.files, path: None})
                head = self.closure_head()
                if path == sc.CATALOG:
                    # The existing pre-catalog policy sees only a manifest
                    # change. Preserve its clean result, outside the exception.
                    self.assertEqual(sc.check(self.repo.root, base, head), ())
                    with mock.patch.object(sc, "_classify_v2",
                                           side_effect=AssertionError("not the new exception")):
                        self.assertEqual(self.check(head, base), ())
                else:
                    self.assertTrue(self.check(head, base))
        # A catalog-bearing baseline cannot become a pre-catalog source by
        # removing the candidate catalog, even with a changed closure hash.
        base = self.repo.commit(self.files)
        head = self.closure_head({sc.CATALOG: None})
        self.assertTrue(sc.check(self.repo.root, base, head))
        self.assertTrue(self.check(head, base))
        # Specifically cover one-sided absence where legacy treats shape-only
        # material as unguarded, so the new equality condition is decisive.
        base = self.repo.commit(self.files)
        head = self.closure_head({self.overlay: None})
        self.assertTrue(self.check(head, base))

    def test_active_blob_equality_is_byte_exact_and_empty_is_not_present(self):
        for path in self.active:
            with self.subTest(path=path):
                base = self.repo.commit(self.files)
                head = self.closure_head({path: self.active[path] + "\n"})
                self.assertTrue(self.check(head, base))
        base = self.repo.commit({**self.files, self.profile: "", self.overlay: ""})
        self.assertTrue(self.check(self.closure_head(), base))

    def test_regular_modes_per_blob_bound_and_partial_bytes_refuse(self):
        head = self.closure_head()
        real_git = sc._git
        for fault in ("symlink", "gitlink", "size", "partial"):
            reading_profile = False
            def capture(root, args):
                nonlocal reading_profile
                if args[:2] == ["ls-tree", "-z"]:
                    reading_profile = args[-1] == self.profile
                    raw = real_git(root, args)
                    if reading_profile and fault == "symlink":
                        return raw.replace(b"100644 blob", b"120000 blob")
                    if reading_profile and fault == "gitlink":
                        return raw.replace(b"100644 blob", b"160000 commit")
                    return raw
                if reading_profile and args[:2] == ["cat-file", "-s"] and fault == "size":
                    return str(sc.MAX_ARTIFACT_BYTES + 1).encode()
                raw = real_git(root, args)
                if reading_profile and args[:2] == ["cat-file", "blob"] and fault == "partial":
                    return raw[:-1]
                return raw
            with self.subTest(fault=fault), mock.patch.object(sc, "_git", side_effect=capture):
                self.assertTrue(self.check(head))

    def test_full_v2_validation_cannot_use_weaker_legacy_catalog_shape(self):
        malformed = catalog()
        malformed["objects"][0]["attributes"] = {}
        base = self.repo.commit({sc.CATALOG: text(malformed)})
        head = self.closure_head()
        self.assertEqual(sc.check(self.repo.root, base, head), ("migration-path-changed:closure",))
        self.assertEqual(self.check(head, base), ("migration-path-changed:closure",))

    def test_active_engine_and_dormant_data_do_not_enter_none_exception(self):
        base = self.repo.commit({sc.HISTORY: text([{"version": 0, "class": "baseline",
            "min_reader_version": 0, "apply_engine": True}])})
        self.assertTrue(self.check(self.closure_head(), base))
        base = self.repo.commit({**self.files, sc.REGISTRY: text([step()])})
        head = self.closure_head({sc.REGISTRY: text([step(2, J)]),
                                  sc.SEEDS: '{"version":0,"synthetic":["new"]}\n'})
        result = sc.classify_v2(self.repo.root, base, head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("data", True, ()))
        self.assertTrue(self.check(head, base))

    def test_extra_requirement_or_migration_observation_never_becomes_allowance(self):
        head = self.closure_head()
        result = sc.classify_v2(self.repo.root, self.base, head)
        for changed in (result._replace(requirements=(*result.requirements, "future-authority")),
                        result._replace(migration_changes=(*result.migration_changes, "other path"))):
            with self.subTest(changed=changed), mock.patch.object(sc, "_classify_v2", return_value=changed):
                self.assertTrue(self.check(head))

    def test_retained_pending_is_valid_but_introduction_or_history_removal_is_not(self):
        args = pending_fixture.fixture()
        active = {sc.CATALOG: args["active_catalog"].decode(),
                  sc.GLOBALS: args["active_globals"].decode(), sc.HISTORY: args["active_history"].decode()}
        files = {path: raw.decode() for path, raw in args["files"].items()}
        base = self.repo.commit({**self.files, **active})
        introduced = self.closure_head(files)
        self.assertTrue(self.check(introduced, base))
        retained = self.repo.commit({sc.MIGRATION_PATH: text(migration())})
        head = self.closure_head()
        self.assertEqual(self.check(head, retained), ())
        self.repo.commit({path: None for path in files})
        restored = self.closure_head(files)
        self.assertTrue(self.check(restored, retained))

    def test_pending_file_mode_change_is_not_an_unchanged_inventory(self):
        args = pending_fixture.fixture()
        files = {path: raw.decode() for path, raw in args["files"].items()}
        base = self.repo.commit({**self.files, **files, sc.CATALOG: args["active_catalog"].decode(),
                                sc.GLOBALS: args["active_globals"].decode(),
                                sc.HISTORY: args["active_history"].decode()})
        self.closure_head()
        self.repo.git("update-index", "--chmod=+x", sc.pending_plan.PLAN)
        self.repo.git("commit", "--quiet", "-m", "pending mode only")
        head = self.repo.head()
        self.assertTrue(self.check(head, base))

    def test_one_cache_for_both_policies_and_material_then_refetch_next_call(self):
        head = self.closure_head()
        real_git = sc._git
        listings = []
        def capture(root, args):
            if args[:2] == ["ls-tree", "-z"]:
                listings.append((args[2], args[-1]))
            return real_git(root, args)
        with mock.patch.object(sc, "_git", side_effect=capture):
            self.assertEqual(self.check(head), ())
            first = tuple(listings)
            self.assertEqual(len(first), len(set(first)))
            for path in self.active:
                for sha in (self.base, head):
                    self.assertIn((sha, path), first)
            self.assertEqual(self.check(head), ())
            self.assertEqual(listings, list(first) * 2)

    def test_transport_failure_after_legacy_result_is_not_swallowed(self):
        head = self.closure_head()
        real_git = sc._git
        def capture(root, args):
            if args[:2] == ["ls-tree", "-z"] and args[-1] == self.profile:
                raise source_git.SourceGitRefusal("git-failed:source-budget")
            return real_git(root, args)
        with mock.patch.object(sc, "_git", side_effect=capture):
            with self.assertRaises(source_git.SourceGitRefusal):
                self.check(head)


class InvocationArtifactBudgetTests(unittest.TestCase):
    def test_shared_raw_cache_keeps_total_cap_without_charging_cache_hits(self):
        # Reuse one small allocation across sixteen returned blobs; the declared
        # total is 64 MiB, but this test need not allocate sixteen large bodies.
        body = b"x" * (4 * 1024 * 1024)
        object_id = "a" * 40
        commands = []

        def capture(root, args):
            commands.append(tuple(args))
            if args[:2] == ["ls-tree", "-z"]:
                return ("100644 blob " + object_id + "\t" + args[-1] + "\0").encode()
            if args[:2] == ["cat-file", "-s"]:
                return str(len(body)).encode()
            if args[:2] == ["cat-file", "blob"]:
                return body
            self.fail("unexpected synthetic Git command")

        repo = sc._Repository(Path("synthetic-bounded-cache"))
        with mock.patch.object(sc, "_git", side_effect=capture):
            for index in range(16):
                self.assertIs(repo.blob("b" * 40, f"part-{index}.json"), body)
            before = len(commands)
            self.assertIs(repo.blob("b" * 40, "part-0.json"), body)
            self.assertEqual(len(commands), before)
            self.assertEqual(repo.total, 64 * 1024 * 1024)
            with self.assertRaises(sc._Invalid):
                repo.blob("b" * 40, "one-more.json")
        self.assertEqual(sum(command[:2] == ("cat-file", "blob") for command in commands), 16)
        self.assertNotIn(("b" * 40, "one-more.json"), repo.cache)

class CompleteNullableSourcePolicyTests(unittest.TestCase):
    """Real disposable immutable source history; no DB or future engine claim."""

    @classmethod
    def setUpClass(cls):
        import test_ship_schema_nullable as fixture
        cls.fixture = fixture
        cls.source_inputs = fixture.declared_catalogs()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.before, self.after, self.declaration = self.source_inputs
        self.base_files = {sc.CATALOG: self.before.decode(), sc.HISTORY: text(history()), sc.GLOBALS: "[]\n",
            sc.MIGRATION_PATH: text(migration()), "cmd/whatomate/main.go": MAIN,
            "internal/database/tenant.go": TENANT, "README.md": "source-only future fixture\n"}
        self.base = self.repo.commit(self.base_files)

    def head(self, *, after=None, declaration=None, engine=True, kind="expand", floor=0, changes=None):
        after = self.after if after is None else after
        declaration = self.declaration if declaration is None else declaration
        files = {sc.CATALOG: after.decode(), sc.HISTORY: text(history(1, kind, engine, floor)),
            sc.schema_nullable.SOURCE_PATH: self.fixture.wire(declaration).decode()}
        files.update(changes or {})
        return self.repo.commit(files)

    def result(self, **kwargs):
        head = self.head(**kwargs)
        return sc.classify_v2(self.repo.root, self.base, head)

    def test_full_declared_nullable_delta_is_expand_but_runtime_stays_held(self):
        head = self.head()
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", False, ()))
        self.assertTrue({"compat-required", "restore-drills", "producer-class-unsupported", "catalog-format/profile-unsupported"} <= set(result.requirements))
        self.assertNotIn("contract-label", result.requirements)
        self.assertTrue(any("source nullable declaration:" in line for line in result.summary))
        self.assertTrue(sc.check(self.repo.root, self.base, head))  # Legacy operational guard is unchanged.

    def test_missing_declaration_keeps_complete_delta_contract(self):
        result = self.result(changes={sc.schema_nullable.SOURCE_PATH: None}, kind="contract")
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_engine_false_remains_blocked_not_dormant(self):
        result = self.result(engine=False)
        self.assertEqual((result.kind, result.dormant), ("expand", False))
        self.assertIn("active-catalog-without-apply-engine", result.reasons)
        self.assertIn("producer-class-unsupported", result.requirements)

    def test_missing_engine_flag_remains_blocked(self):
        value = history(1, "expand", True); value[-1].pop("apply_engine")
        result = self.result(changes={sc.HISTORY: text(value)})
        self.assertIn("active-catalog-without-apply-engine", result.reasons)

    def test_none_current_head_without_declaration_is_unchanged(self):
        head = self.repo.commit({"README.md": "unchanged current source\n"})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
        self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_malformed_present_cannot_hide_behind_none(self):
        head = self.repo.commit({sc.schema_nullable.SOURCE_PATH: "{}\n"})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertIn("classifier-v2-invalid:nullable-source", result.reasons)

    def test_malformed_present_cannot_hide_behind_dormant_data(self):
        head = self.repo.commit({sc.schema_nullable.SOURCE_PATH: "{}\n", sc.SEEDS: text({"version": 0})})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertIn("classifier-v2-invalid:nullable-source", result.reasons)

    def test_malformed_present_cannot_hide_behind_precatalog_baseline(self):
        base = self.repo.commit({sc.CATALOG: None})
        head = self.repo.commit({sc.schema_nullable.SOURCE_PATH: "{}\n"})
        result = sc.classify_v2(self.repo.root, base, head)
        self.assertIn("classifier-v2-invalid:nullable-source", result.reasons)

    def test_residual_existing_type_contract_dominates_nullable(self):
        value = json.loads(self.after)
        next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("types", "users"))["attributes"]["definition_sha256"] = J
        after = self.fixture.catalog_bytes(value)
        declaration = self.fixture.bind_documents(self.declaration, self.before, after)
        result = self.result(after=after, declaration=declaration, kind="contract")
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_residual_policy_change_contract_dominates_data(self):
        value = json.loads(self.after)
        next(r for r in value["objects"] if r["kind"] == "policies")["attributes"]["using_sha256"] = J
        after = self.fixture.catalog_bytes(value)
        declaration = self.fixture.bind_documents(self.declaration, self.before, after)
        result = self.result(after=after, declaration=declaration, kind="contract", changes={sc.SEEDS: text({"version": 0})})
        self.assertEqual(result.kind, "contract")

    def test_data_precedence_over_explained_expand(self):
        result = self.result(changes={sc.SEEDS: text({"version": 0})})
        self.assertEqual((result.kind, result.dormant), ("data", False))
        self.assertIn("producer-class-unsupported", result.requirements)

    def test_unprotected_residual_relation_still_blocked(self):
        value = json.loads(self.after)
        value["objects"].append(row("relations", "unprotected", rls="false", force_rls="false"))
        after = self.fixture.catalog_bytes(value)
        declaration = self.fixture.bind_documents(self.declaration, self.before, after)
        result = self.result(after=after, declaration=declaration, kind="contract")
        self.assertIn("unprotected-new-relation:unprotected", result.reasons)

    def test_floor_and_history_precedence_remains(self):
        result = self.result(kind="contract", floor=1)
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_history_version_and_append_only_checks_remain(self):
        bad = history(1, "expand", True); bad[0]["apply_engine"] = True
        result = self.result(changes={sc.HISTORY: text(bad)})
        self.assertIn("history-rewritten", result.reasons)
        head = self.head(changes={sc.HISTORY: text(history())})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertIn("history-catalog-version-mismatch", result.reasons)

    def test_stale_raw_catalog_binding_refuses(self):
        value = json.loads(self.after)
        next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("types", "users"))["attributes"]["definition_sha256"] = J
        result = self.result(after=self.fixture.catalog_bytes(value))
        self.assertIn("classifier-v2-invalid:nullable-source", result.reasons)

    def test_source_namespace_is_not_a_pending_plan_or_activation(self):
        result = self.result()
        self.assertFalse(any(line.startswith("pending source plan:") for line in result.summary))
        self.assertIn("catalog-format/profile-unsupported", result.requirements)

    def test_real_scoped_git_binds_refs_and_original_budget(self):
        head = self.head()
        reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
        with reader.operation():
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertTrue(reader.closed)
        for detail in ("calls", "bytes", "deadline"):
            real_git = sc._git
            def refuse(root, args):
                if args[:2] == ["ls-tree", "-z"] and args[-1] == sc.schema_nullable.SOURCE_PATH:
                    raise source_git.SourceGitRefusal("git-failed:source-" + detail)
                return real_git(root, args)
            with self.subTest(detail=detail), mock.patch.object(sc, "_git", side_effect=refuse):
                with self.assertRaises(source_git.SourceGitRefusal): sc.classify_v2(self.repo.root, self.base, head)

    def test_descriptor_blob_must_be_regular_and_bounded(self):
        head = self.head()
        real_git = sc._git
        for reply in (b"120000 blob " + b"a"*40 + b"\t" + sc.schema_nullable.SOURCE_PATH.encode() + b"\0",
                      b"160000 commit " + b"a"*40 + b"\t" + sc.schema_nullable.SOURCE_PATH.encode() + b"\0"):
            def invalid(root, args):
                if args[:2] == ["ls-tree", "-z"] and args[-1] == sc.schema_nullable.SOURCE_PATH: return reply
                return real_git(root, args)
            with self.subTest(reply=reply[:6]), mock.patch.object(sc, "_git", side_effect=invalid):
                result = sc.classify_v2(self.repo.root, self.base, head)
            self.assertIn("classifier-v2-invalid:nullable-source", result.reasons)

    def test_declared_refs_are_actual_immutable_comparison_inputs(self):
        head = self.head()
        real = sc.schema_nullable.classify_source
        calls = []
        def observed(declaration, before, after, *, base_sha, head_sha):
            calls.append((base_sha, head_sha, self.fixture.sha(before), self.fixture.sha(after)))
            return real(declaration, before, after, base_sha=base_sha, head_sha=head_sha)
        with mock.patch.object(sc.schema_nullable, "classify_source", side_effect=observed):
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual(result.kind, "expand")
        self.assertEqual(calls, [(self.base, head, self.fixture.sha(self.before), self.fixture.sha(self.after))])



class CompleteColumnIndexSourcePolicyTests(unittest.TestCase):
    """Complete source-derived catalogs on disposable immutable Git histories.

    These controlled future source declarations are not an activated engine,
    measured transition, pending plan or operational compatibility proof.
    """

    @classmethod
    def setUpClass(cls):
        import test_ship_schema_nullable as columns
        import test_ship_schema_index as indexes
        cls.columns, cls.indexes = columns, indexes
        cls.nullable_inputs = columns.declared_catalogs()
        cls.constant_inputs = columns.constant_variant(*cls.nullable_inputs)
        cls.index_inputs = indexes.declared_catalogs()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.before = self.constant_inputs[0]
        self.base = self.repo.commit({sc.CATALOG: self.before.decode(), sc.HISTORY: text(history()),
            sc.GLOBALS: "[]\n", sc.MIGRATION_PATH: text(migration()), "cmd/whatomate/main.go": MAIN,
            "internal/database/tenant.go": TENANT, "README.md": "controlled source-only future intent\n"})

    def inputs(self, family="constant", *, index=False, nullable=True):
        if family == "index":
            before, after, declaration = self.index_inputs
            return before, after, {sc.schema_index.SOURCE_PATH: declaration}
        before, after, declaration = self.nullable_inputs
        path = sc.schema_nullable.SOURCE_PATH
        if family == "constant":
            before, after, declaration = self.columns.constant_variant(before, after, declaration, nullable=nullable)
            path = sc.schema_nullable.CONSTANT_SOURCE_PATH
        declarations = {path: declaration}
        if index:
            after, index_declaration = self.indexes.add_index(before, after)
            declarations[path] = self.columns.bind_documents(declaration, before, after)
            declarations[sc.schema_index.SOURCE_PATH] = index_declaration
        return before, after, declarations

    def head(self, inputs=None, *, kind="expand", engine=True, floor=0, changes=None):
        before, after, declarations = self.inputs() if inputs is None else inputs
        self.assertEqual(before, self.before)
        files = {sc.CATALOG: after.decode(), sc.HISTORY: text(history(1, kind, engine, floor))}
        files.update({path: self.columns.wire(value).decode() for path, value in declarations.items()})
        files.update(changes or {})
        return self.repo.commit(files)

    def classify(self, inputs=None, **kwargs):
        return sc.classify_v2(self.repo.root, self.base, self.head(inputs, **kwargs))

    def rebind(self, inputs, after):
        before, _, declarations = inputs
        return before, after, {path: (self.indexes if path == sc.schema_index.SOURCE_PATH else self.columns)
            .bind_documents(declaration, before, after) for path, declaration in declarations.items()}

    def test_nullable_and_notnull_constant_are_source_expand_with_runtime_holds(self):
        for nullable in (True, False):
            with self.subTest(nullable=nullable):
                result = self.classify(self.inputs(nullable=nullable))
                self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", False, ()))
                self.assertTrue({"compat-required", "restore-drills", "producer-class-unsupported",
                    "catalog-format/profile-unsupported"} <= set(result.requirements))
                self.assertNotIn("contract-label", result.requirements)
                self.assertTrue(any(line.startswith("source constant declaration:") for line in result.summary))

    def test_index_source_expand_removes_only_missing_operation_requirement(self):
        result = self.classify(self.inputs("index"))
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertNotIn("index-operation-unsupported", result.requirements)
        self.assertIn("producer-class-unsupported", result.requirements)
        self.assertTrue(any(line.startswith("source index declaration:") for line in result.summary))

    def test_nullable_and_index_explanations_compose(self):
        result = self.classify(self.inputs("nullable", index=True))
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertEqual(sum(line.startswith("source ") and " declaration:" in line for line in result.summary), 2)
        self.assertNotIn("index-operation-unsupported", result.requirements)

    def test_constant_and_index_explanations_compose(self):
        result = self.classify(self.inputs(index=True, nullable=False))
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertEqual(sum(line.startswith("source ") and " declaration:" in line for line in result.summary), 2)
        self.assertNotIn("index-operation-unsupported", result.requirements)

    def test_missing_constant_declaration_keeps_full_delta_contract(self):
        result = self.classify(kind="contract", changes={sc.schema_nullable.CONSTANT_SOURCE_PATH: None})
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_missing_index_declaration_still_requires_operation_proof(self):
        result = self.classify(self.inputs("index"), changes={sc.schema_index.SOURCE_PATH: None})
        self.assertIn("index-operation-unsupported", result.requirements)
        self.assertFalse(any(line.startswith("source index declaration:") for line in result.summary))

    def test_index_cannot_hide_unexplained_companion_column_rowtype(self):
        result = self.classify(self.inputs(index=True), kind="contract",
                              changes={sc.schema_nullable.CONSTANT_SOURCE_PATH: None})
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)
        self.assertNotIn("index-operation-unsupported", result.requirements)

    def test_column_cannot_satisfy_index_operation_requirement(self):
        result = self.classify(self.inputs(index=True), changes={sc.schema_index.SOURCE_PATH: None})
        self.assertIn("index-operation-unsupported", result.requirements)

    def test_engine_false_and_missing_engine_keep_active_hold(self):
        for engine in (False, None):
            rows = history(1, "expand", False)
            if engine is None: rows[-1].pop("apply_engine")
            with self.subTest(engine=engine):
                result = self.classify(self.inputs(index=True), changes={sc.HISTORY: text(rows)})
                self.assertEqual((result.kind, result.dormant), ("expand", False))
                self.assertIn("active-catalog-without-apply-engine", result.reasons)

    def test_missing_all_declarations_preserves_current_none_requirements(self):
        head = self.repo.commit({"README.md": "current no-delta source\n"})
        result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
        self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_malformed_present_constant_or_index_refuses_before_none(self):
        for path, label in ((sc.schema_nullable.CONSTANT_SOURCE_PATH, "constant-source"), (sc.schema_index.SOURCE_PATH, "index-source")):
            with self.subTest(path=path):
                head = self.repo.commit({sc.schema_nullable.CONSTANT_SOURCE_PATH: None, sc.schema_index.SOURCE_PATH: None, path: "{}\n"})
                result = sc.classify_v2(self.repo.root, self.base, head)
                self.assertIn("classifier-v2-invalid:" + label, result.reasons)

    def test_malformed_present_refuses_before_dormant_seed_change(self):
        for path, label in ((sc.schema_nullable.CONSTANT_SOURCE_PATH, "constant-source"), (sc.schema_index.SOURCE_PATH, "index-source")):
            with self.subTest(path=path):
                head = self.repo.commit({sc.schema_nullable.CONSTANT_SOURCE_PATH: None, sc.schema_index.SOURCE_PATH: None,
                    path: "{}\n", sc.SEEDS: text({"version": 0})})
                self.assertIn("classifier-v2-invalid:" + label, sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_malformed_present_refuses_precatalog_shortcut(self):
        base = self.repo.commit({sc.CATALOG: None})
        head = self.repo.commit({sc.schema_nullable.CONSTANT_SOURCE_PATH: "{}\n"})
        self.assertIn("classifier-v2-invalid:constant-source", sc.classify_v2(self.repo.root, base, head).reasons)

    def test_old_nullable_plus_constant_is_ambiguous_before_any_new_decode(self):
        before, after, declarations = self.inputs()
        head = self.head((before, after, {**declarations, sc.schema_nullable.SOURCE_PATH: self.nullable_inputs[2]}))
        with mock.patch.object(sc.schema_nullable.wire, "_load", side_effect=AssertionError("ambiguous decoded")):
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertIn("classifier-v2-invalid:source-classification", result.reasons)

    def test_joint_budget_counts_all_three_declarations_before_decode(self):
        raw = b" " * (3 << 20)
        with mock.patch.object(sc.schema_nullable.wire, "_load", side_effect=AssertionError("overcap decoded")):
            with self.assertRaises(sc._Invalid): sc._v2_source_budget((raw, raw, b"{}\n"), raw, raw)

    def test_complete_catalogs_each_family_fits_but_joint_sum_refuses(self):
        before, after, declarations = self.inputs(index=True)
        column = declarations[sc.schema_nullable.CONSTANT_SOURCE_PATH]
        size = (sc.schema_nullable.MAX_TOTAL - len(self.columns.wire(column))) // 2
        before = b" " * (size - len(before)) + before
        after = b" " * (size - len(after)) + after
        column = self.columns.bind_documents(column, before, after)
        index = self.indexes.bind_documents(declarations[sc.schema_index.SOURCE_PATH], before, after)
        c, i = self.columns.wire(column), self.columns.wire(index)
        self.assertLessEqual(len(c) + len(before) + len(after), sc.schema_nullable.MAX_TOTAL)
        self.assertLessEqual(len(i) + len(before) + len(after), sc.schema_nullable.MAX_TOTAL)
        self.assertGreater(len(c) + len(i) + len(before) + len(after), sc.schema_nullable.MAX_TOTAL)
        with mock.patch.object(sc.schema_nullable.wire, "_load", side_effect=AssertionError("joint overcap decoded")):
            with self.assertRaises(sc._Invalid): sc._v2_catalog_delta(json.loads(before), json.loads(after), [], [], set(), [], set(),
                constant_source=(c, before, after, "a"*40, "b"*40), index_source=(i, before, after, "a"*40, "b"*40))

    def test_per_input_limits_and_aliased_catalog_roles_are_not_deduplicated(self):
        before, after, declarations = self.inputs(index=True)
        with self.assertRaises(sc._Invalid): sc._v2_source_budget((None, None, b" "*(sc.schema_index.MAX_DECLARATION+1)), before, after)
        with self.assertRaises(sc._Invalid): sc._v2_source_budget((None, b" "*(sc.schema_nullable.MAX_DOCUMENT+1), None), before, after)
        same = b" " * sc.schema_nullable.MAX_DOCUMENT
        with self.assertRaises(sc._Invalid): sc._v2_source_budget((None, b"{}\n", None), same, same)

    def test_fixed_family_inputs_require_same_raw_roles_and_source_ids(self):
        before, after, declarations = self.inputs(index=True)
        c = self.columns.wire(declarations[sc.schema_nullable.CONSTANT_SOURCE_PATH])
        i = self.columns.wire(declarations[sc.schema_index.SOURCE_PATH])
        for right in ((i, b" " + before, after, "a"*40, "b"*40), (i, before, after, "c"*40, "b"*40),
                      (i, before, after, "a"*40, "c"*40), (i, before, after, True, "b"*40)):
            with self.subTest(right_sha=right[3:]), self.assertRaises(sc._Invalid):
                sc._v2_catalog_delta(json.loads(before), json.loads(after), [], [], set(), [], set(),
                    constant_source=(c, before, after, "a"*40, "b"*40), index_source=right)

    def test_preparsed_or_forged_delta_never_substitutes_for_raw_source(self):
        before, after, declarations = self.inputs()
        parsed = sc.schema_nullable.classify_constant_source(self.columns.wire(next(iter(declarations.values()))), before, after,
                                                            base_sha="a"*40, head_sha="b"*40)
        with self.assertRaises(sc._Invalid): sc._v2_catalog_delta(json.loads(before), json.loads(after), [], [], set(), [], set(), constant_source=parsed)

    def test_full_catalog_residual_type_contract_dominates_both_rules(self):
        inputs = self.inputs(index=True); value = json.loads(inputs[1])
        next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("types", "users"))["attributes"]["definition_sha256"] = J
        result = self.classify(self.rebind(inputs, self.columns.catalog_bytes(value)), kind="contract")
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_full_security_residual_contract_dominates_data_and_expand(self):
        inputs = self.inputs(index=True); value = json.loads(inputs[1])
        next(r for r in value["objects"] if r["kind"] == "policies")["attributes"]["using_sha256"] = J
        result = self.classify(self.rebind(inputs, self.columns.catalog_bytes(value)), kind="contract", changes={sc.SEEDS: text({"version": 0})})
        self.assertEqual(result.kind, "contract")

    def test_existing_unrelated_default_edit_remains_contract(self):
        inputs = self.inputs(index=True); value = json.loads(inputs[1])
        next(r for r in value["objects"] if (r["kind"], r["identity"]) == ("columns", "users.id"))["attributes"]["default_sha256"] = J
        result = self.classify(self.rebind(inputs, self.columns.catalog_bytes(value)), kind="contract")
        self.assertEqual(result.kind, "contract")

    def test_data_and_min_floor_precedence_remain(self):
        result = self.classify(self.inputs(index=True), changes={sc.SEEDS: text({"version": 0})})
        self.assertEqual((result.kind, result.dormant), ("data", False))
        result = self.classify(self.inputs(index=True), kind="contract", floor=1)
        self.assertEqual(result.kind, "contract")

    def test_history_append_only_and_catalog_version_remain_enforced(self):
        rows = history(1, "expand", True); rows[0]["apply_engine"] = True
        self.assertIn("history-rewritten", self.classify(self.inputs(index=True), changes={sc.HISTORY: text(rows)}).reasons)
        self.assertIn("history-catalog-version-mismatch", self.classify(self.inputs(index=True), changes={sc.HISTORY: text(history())}).reasons)

    def test_unprotected_new_relation_is_not_hidden_by_explanations(self):
        inputs = self.inputs(index=True); value = json.loads(inputs[1])
        value["objects"].append(row("relations", "unprotected", rls="false", force_rls="false"))
        result = self.classify(self.rebind(inputs, self.columns.catalog_bytes(value)), kind="contract")
        self.assertIn("unprotected-new-relation:unprotected", result.reasons)

    def test_stale_index_or_constant_complete_document_binding_refuses(self):
        for path in (sc.schema_nullable.CONSTANT_SOURCE_PATH, sc.schema_index.SOURCE_PATH):
            inputs = self.inputs(index=True)
            declarations = json.loads(self.columns.wire(inputs[2])); declarations[path]["after"]["sha256"] = J
            with self.subTest(path=path):
                self.assertIn("classifier-v2-invalid:source-classification", self.classify((inputs[0], inputs[1], declarations)).reasons)

    def test_real_scope_passes_exact_refs_to_both_fixed_interpreters(self):
        inputs = self.inputs(index=True); head = self.head(inputs); calls = []
        column, index = sc.schema_nullable.classify_constant_source, sc.schema_index.classify_source
        def c(raw, before, after, **kwargs):
            calls.append(("column", kwargs, before, after)); return column(raw, before, after, **kwargs)
        def i(raw, before, after, **kwargs):
            calls.append(("index", kwargs, before, after)); return index(raw, before, after, **kwargs)
        reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
        with reader.operation(), mock.patch.object(sc.schema_nullable, "classify_constant_source", side_effect=c), mock.patch.object(sc.schema_index, "classify_source", side_effect=i):
            result = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertEqual(calls, [(label, {"base_sha": self.base, "head_sha": head}, inputs[0], inputs[1]) for label in ("column", "index")])
        self.assertTrue(reader.closed)

    def test_transport_deadline_and_call_failures_stay_hard_refusals(self):
        head = self.head(self.inputs(index=True)); real_git = sc._git
        for path in (sc.schema_nullable.CONSTANT_SOURCE_PATH, sc.schema_index.SOURCE_PATH):
            def refuse(root, args):
                if args[:2] == ["ls-tree", "-z"] and args[-1] == path: raise source_git.SourceGitRefusal("git-failed:source-deadline")
                return real_git(root, args)
            with self.subTest(path=path), mock.patch.object(sc, "_git", side_effect=refuse):
                with self.assertRaises(source_git.SourceGitRefusal): sc.classify_v2(self.repo.root, self.base, head)

    def test_nonregular_new_declaration_blobs_refuse(self):
        head = self.head(self.inputs(index=True)); real_git = sc._git
        for path, label in ((sc.schema_nullable.CONSTANT_SOURCE_PATH, "constant-source"), (sc.schema_index.SOURCE_PATH, "index-source")):
            def invalid(root, args):
                if args[:2] == ["ls-tree", "-z"] and args[-1] == path: return b"120000 blob " + b"a"*40 + b"\t" + path.encode() + b"\0"
                return real_git(root, args)
            with self.subTest(path=path), mock.patch.object(sc, "_git", side_effect=invalid):
                self.assertIn("classifier-v2-invalid:" + label, sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_catalog_raw_failure_keeps_catalog_diagnostic_with_or_without_declaration(self):
        for changes in ({}, {sc.schema_nullable.CONSTANT_SOURCE_PATH: None}):
            head = self.head(changes=changes); original = sc._Repository.blob
            def oversized(repo, sha, path):
                if sha == self.base and path == sc.CATALOG: raise sc._Invalid()
                return original(repo, sha, path)
            with self.subTest(present=not changes), mock.patch.object(sc._Repository, "blob", oversized):
                result = sc.classify_v2(self.repo.root, self.base, head)
            self.assertIn("classifier-v2-invalid:catalog", result.reasons)


class InvokerCreationPrivilegeTests(unittest.TestCase):
    """New-object creation tuples do not waive any old privilege changes."""

    A = "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>PUBLIC:EXECUTE:false"
    B = "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>RUNTIME:EXECUTE:false"

    def classify(self, old, new):
        return sc._v2_catalog_delta(catalog(old), catalog(new), [], [], set(), [], set())

    def test_exact_a_and_b_new_invokers(self):
        for acl in (self.A, self.B):
            with self.subTest(acl=acl):
                self.assertEqual(self.classify([], [row("functions", "f(" + H + ")", acl=acl)]), "expand")

    def test_empty_extra_foreign_and_owner_privileges_are_contract(self):
        invalid = ("", self.A.split(";")[0], self.A + ";MIGRATION_OWNER>RUNTIME:EXECUTE:false",
            self.A.replace("MIGRATION_OWNER>PUBLIC", "OTHER>PUBLIC"),
            self.A.replace("EXECUTE:false", "EXECUTE:true"), self.A + ";MIGRATION_OWNER>PUBLIC:USAGE:false")
        for acl in invalid:
            with self.subTest(acl=acl):
                self.assertEqual(self.classify([], [row("functions", "f(" + H + ")", acl=acl)]), "contract")
        with self.assertRaises(sc._Invalid):
            self.classify([], [row("functions", "f(" + H + ")", acl=self.A.replace("PUBLIC", "OTHER"))])
        for owner in ("RUNTIME", "PUBLIC", "OTHER"):
            self.assertEqual(self.classify([], [row("functions", "f(" + H + ")", owner=owner, acl=self.A)]), "contract")

    def test_existing_owner_acl_and_definer_remain_contract(self):
        old = row("functions", "f(" + H + ")", acl=self.A)
        for changed in (row("functions", "f(" + H + ")", acl=self.B),
                        row("functions", "f(" + H + ")", acl=self.A, owner="OTHER")):
            self.assertEqual(self.classify([old], [changed]), "contract")
        self.assertEqual(self.classify([], [row("functions", "f(" + H + ")", acl=self.A, security_definer="true")]), "contract")
        historical = row("functions", "f(" + H + ")", acl="")
        self.assertEqual(self.classify([historical], [historical]), "none")


class CompleteRelationProfileSourcePolicyTests(unittest.TestCase):
    """Future immutable source intent, actual classifier; no engine or DB grant."""

    @classmethod
    def setUpClass(cls):
        import test_ship_schema_tenant_profile as profiles
        import test_ship_schema_relation as relations
        import test_ship_schema_index as indexes
        cls.p, cls.r, cls.i = profiles, relations, indexes
        cls.introduction = profiles.scenario()
        cls.production = profiles.scenario(production=True)
        cls.retention = profiles.retained(cls.introduction)
        cls.third = profiles.scenario(prior=cls.introduction, version=3)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")

    def rows(self, version):
        return history() + [{"version": n, "class": "expand", "min_reader_version": 0, "apply_engine": True}
                            for n in range(1, version + 1)]

    def pair(self, value=None, *, kind="expand", engine=True, floor=0, changes=None):
        value = self.introduction if value is None else value
        profile_raw, relation_raw, bc, ac, bg, ag, bt, at = value
        old_version, new_version = json.loads(bc)["version"], json.loads(ac)["version"]
        files = {sc.CATALOG: bc.decode(), sc.GLOBALS: bg.decode(), sc._V2_TENANT: bt.decode(),
            sc.HISTORY: text(self.rows(old_version)), sc.MIGRATION_PATH: text(migration()),
            "cmd/whatomate/main.go": MAIN, "README.md": "controlled source-only future intent\n",
            sc.SEEDS: None, sc.schema_nullable.SOURCE_PATH: None, sc.schema_nullable.CONSTANT_SOURCE_PATH: None,
            sc.schema_index.SOURCE_PATH: None, sc.schema_relation.SOURCE_PATH: None,
            sc.schema_tenant_profile.SOURCE_PATH: None}
        base = self.repo.commit(files)
        rows = self.rows(new_version)
        if new_version > old_version:
            rows[-1].update({"class": kind, "apply_engine": engine, "min_reader_version": floor})
        files = {sc.CATALOG: ac.decode(), sc.GLOBALS: ag.decode(), sc._V2_TENANT: at.decode(), sc.HISTORY: text(rows),
            sc.schema_relation.SOURCE_PATH: None if relation_raw is None else relation_raw.decode(),
            sc.schema_tenant_profile.SOURCE_PATH: None if profile_raw is None else profile_raw.decode(),
            "README.md": "controlled prospective head\n"}
        files.update(changes or {})
        return base, self.repo.commit(files)

    def classify(self, value=None, **kwargs):
        return sc.classify_v2(self.repo.root, *self.pair(value, **kwargs))

    def global_value(self, *, production=False):
        fixture = self.r.declared_catalogs(production=production)
        return (None, self.r.wire(fixture[6]), *fixture[:6])

    def test_complete_global_bundle_source_and_production(self):
        for production in (False, True):
            result = self.classify(self.global_value(production=production))
            self.assertEqual((result.kind, result.reasons), ("expand", ()))
            self.assertIn("producer-class-unsupported", result.requirements)

    def test_complete_tenant_v2_source_and_production(self):
        for value in (self.introduction, self.production):
            result = self.classify(value)
            self.assertEqual((result.kind, result.dormant, result.reasons), ("expand", False, ()))
            self.assertNotIn("additive-profile-unsupported", result.requirements)
            self.assertTrue({"producer-class-unsupported", "catalog-format/profile-unsupported", "restore-drills"}
                            <= set(result.requirements))
            self.assertEqual(sum(" declaration:" in s and s.startswith("source ") for s in result.summary), 2)

    def test_append_v3_preserves_v2_and_core(self):
        result = self.classify(self.third)
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertNotIn("additive-profile-unsupported", result.requirements)

    def test_retained_noop_is_usable_but_not_an_execution_grant(self):
        result = self.classify(self.retention)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
        self.assertNotIn("additive-profile-unsupported", result.requirements)
        self.assertIn("catalog-format/profile-unsupported", result.requirements)

    def test_retained_profile_composes_with_independent_index(self):
        value = self.retention
        after, declaration = self.i.add_index(value[2], value[3])
        declaration = self.i.bind_documents(declaration, value[2], after)
        value = self.p.rebind(value, raw_changes={1: after})
        result = self.classify(value, changes={sc.schema_index.SOURCE_PATH: self.i.wire(declaration).decode()})
        self.assertEqual((result.kind, result.reasons), ("expand", ()))
        self.assertNotIn("index-operation-unsupported", result.requirements)

    def test_retained_profile_and_unrelated_global_bundle(self):
        value = self.retention
        relation = self.r.add_relation(value[2], value[4], value[6], mode="global", table="later_global_items")
        value = self.p.bind(relation[:6], relation[6])
        result = self.classify(value)
        self.assertEqual((result.kind, result.reasons), ("expand", ()))

    def test_missing_profile_keeps_unsupported_and_cannot_explain_tenant_bundle(self):
        result = self.classify(changes={sc.schema_tenant_profile.SOURCE_PATH: None})
        self.assertIn("classifier-v2-invalid:relation-source", result.reasons)
        result = self.classify(self.retention, changes={sc.schema_tenant_profile.SOURCE_PATH: None})
        self.assertIn("additive-profile-unsupported", result.reasons)

    def test_missing_relation_refuses_profile_introduction(self):
        result = self.classify(changes={sc.schema_relation.SOURCE_PATH: None})
        self.assertIn("classifier-v2-invalid:tenant-profile-source", result.reasons)

    def test_malformed_present_before_noop_and_dormant_shortcuts(self):
        value = (None, None, *self.introduction[2:])
        value = (None, None, value[2], value[2], value[4], value[4], value[6], value[6])
        for path, label in ((sc.schema_relation.SOURCE_PATH, "relation-source"),
                            (sc.schema_tenant_profile.SOURCE_PATH, "tenant-profile-source")):
            for changes in ({}, {sc.SEEDS: text({"version": 0})}):
                result = self.classify(value, changes={**changes, path: "{}\n"})
                self.assertIn("classifier-v2-invalid:" + label, result.reasons)

    def test_malformed_present_before_catalog_baseline_shortcut(self):
        base, _ = self.pair()
        base = self.repo.commit({sc.CATALOG: None})
        head = self.repo.commit({sc.schema_relation.SOURCE_PATH: "{}\n", sc.schema_tenant_profile.SOURCE_PATH: None})
        self.assertIn("classifier-v2-invalid:relation-source", sc.classify_v2(self.repo.root, base, head).reasons)

    def test_existing_unrelated_security_remains_contract(self):
        def change(doc):
            next(r for r in doc["objects"] if (r["kind"], r["identity"]) == ("relations", "organizations"))["attributes"]["owner"] = "OTHER"
        result = self.classify(self.p.mutate_catalog(self.introduction, change), kind="contract")
        self.assertEqual(result.kind, "contract")
        self.assertIn("contract-label", result.requirements)

    def test_retained_acl_policy_or_fingerprint_edits_refuse(self):
        for kind, name, key, bad in (("relations", "source_profile_v2_items", "acl", ""),
            ("policies", "source_profile_v2_items.rereply_tenant_isolation", "roles", "PUBLIC"),
            ("functions", "rereply_tenant_policy_additive_fingerprint_v2(" + EMPTY + ")", "source_sha256", J)):
            def change(doc):
                next(r for r in doc["objects"] if (r["kind"], r["identity"]) == (kind, name))["attributes"][key] = bad
            result = self.classify(self.p.mutate_catalog(self.retention, change))
            self.assertIn("classifier-v2-invalid:tenant-profile-source", result.reasons)

    def test_exact_new_bundle_extra_acl_or_policy_refuses(self):
        def change(doc):
            attrs = next(r for r in doc["objects"] if (r["kind"], r["identity"]) == ("relations", "source_profile_v2_items"))["attributes"]
            attrs["acl"] = ";".join(sorted(attrs["acl"].split(";") + ["MIGRATION_OWNER>PUBLIC:SELECT:false"]))
        result = self.classify(self.p.mutate_catalog(self.introduction, change))
        self.assertIn("classifier-v2-invalid:source-classification", result.reasons)

    def test_data_and_reader_floor_still_dominate(self):
        self.assertEqual(self.classify(changes={sc.SEEDS: text({"version": 0})}).kind, "data")
        self.assertEqual(self.classify(kind="contract", floor=1).kind, "contract")

    def test_current_engine_false_stays_blocked(self):
        result = self.classify(engine=False)
        self.assertEqual((result.kind, result.dormant), ("expand", False))
        self.assertIn("active-catalog-without-apply-engine", result.reasons)
        self.assertIn("producer-class-unsupported", result.requirements)

    def test_history_version_and_append_only_rules_remain(self):
        bad = self.rows(1); bad[0]["apply_engine"] = True
        self.assertIn("history-rewritten", self.classify(changes={sc.HISTORY: text(bad)}).reasons)
        self.assertIn("history-catalog-version-mismatch", self.classify(changes={sc.HISTORY: text(history())}).reasons)

    def test_changed_then_reverted_fingerprint_refuses_in_original_history(self):
        _, base = self.pair()
        doc = json.loads(self.retention[3])
        next(r for r in doc["objects"] if r["identity"].startswith("rereply_tenant_policy_additive_fingerprint_v2("))["attributes"]["source_sha256"] = J
        self.repo.commit({sc.CATALOG: self.p.encoded(doc).decode()})
        head = self.repo.commit({sc.CATALOG: self.retention[3].decode(), sc.schema_relation.SOURCE_PATH: None,
            sc.schema_tenant_profile.SOURCE_PATH: self.retention[0].decode()})
        self.assertIn("classifier-v2-invalid:tenant-profile-history", sc.classify_v2(self.repo.root, base, head).reasons)

    def test_changed_then_reverted_profile_source_refuses(self):
        _, base = self.pair()
        source = self.retention[-1].replace(b'"source_profile_v2_items"', b'"organizations"')
        self.repo.commit({sc._V2_TENANT: source.decode()})
        head = self.repo.commit({sc._V2_TENANT: self.retention[-1].decode(), sc.schema_relation.SOURCE_PATH: None,
            sc.schema_tenant_profile.SOURCE_PATH: self.retention[0].decode()})
        self.assertIn("classifier-v2-invalid:tenant-profile-history", sc.classify_v2(self.repo.root, base, head).reasons)

    def test_first_parent_bound_does_not_skip_oversized_history(self):
        base, head = self.pair(self.retention); original = sc._git
        def bounded(root, args):
            if args[:3] == ["rev-list", "--first-parent", "--reverse"]:
                return (b"a" * 40 + b"\n") * (sc.MAX_HISTORY_COMMITS + 1)
            return original(root, args)
        with mock.patch.object(sc, "_git", side_effect=bounded):
            result = sc.classify_v2(self.repo.root, base, head)
        self.assertIn("classifier-v2-invalid:tenant-profile-history", result.reasons)

    def test_joint_eleven_roles_count_aliases_before_any_decode(self):
        doc = b" " * (4 << 20)
        with mock.patch.object(sc.schema_nullable.wire, "_load", side_effect=AssertionError("decoded")):
            with self.assertRaises(sc._Invalid):
                sc._v2_source_budget((None, None, None, b"{}\n", b"{}\n"), doc, doc, (b"[]\n",)*2 + (b"x",)*2)
        for index, limit in ((0, 256 << 10), (2, 512 << 10)):
            extras = [b"[]\n", b"[]\n", b"x", b"x"]; extras[index] = b" " * (limit + 1)
            with self.assertRaises(sc._Invalid): sc._v2_source_budget((None, None, None, b"{}\n", None), b"{}", b"{}", tuple(extras))

    def test_full_documents_fit_each_pair_but_not_joint_union(self):
        value = self.introduction
        doc = self.p.encoded({"padding": "x" * ((4 << 20) - (128 << 10))})
        self.assertLess(len(doc), sc.schema_nullable.MAX_DOCUMENT)
        # All four auxiliary roles are real, complete JSON/string documents.
        auxiliary = (self.p.encoded({"padding": "x" * (80 << 10)}),) * 2
        tenant = b"// " + b"x" * (80 << 10) + b"\n"
        raw = (None, None, None, value[1], value[0])
        self.assertGreater(sum(len(x) for x in raw if x is not None) + 2*len(doc)
                           + sum(map(len, auxiliary)) + 2*len(tenant), 8 << 20)
        with mock.patch.object(sc.schema_nullable.wire, "_load", side_effect=AssertionError("overcap decoded")):
            with self.assertRaises(sc._Invalid): sc._v2_source_budget(raw, doc, doc, (*auxiliary, tenant, tenant))

    def test_same_comparison_raw_bindings_are_required(self):
        value = self.introduction
        material = (value[1], *value[2:], "a"*40, "b"*40)
        for at in (3, 5, 7):
            other = list((value[0], *value[2:], "a"*40, "b"*40))
            other[at] = (b" " + other[at]) if type(other[at]) is bytes else "c"*40
            with self.assertRaises(sc._Invalid):
                sc._v2_catalog_delta(json.loads(value[2]), json.loads(value[3]), json.loads(value[4]), json.loads(value[5]),
                    set(), [], set(), relation_source=material, profile_source=tuple(other))

    def test_preparsed_profile_is_not_an_input_capability(self):
        value = self.introduction
        parsed = sc.schema_tenant_profile.classify_source(*value, base_sha="a"*40, head_sha="b"*40)
        with self.assertRaises(sc._Invalid):
            sc._v2_catalog_delta(json.loads(value[2]), json.loads(value[3]), json.loads(value[4]), json.loads(value[5]),
                                set(), [], set(), profile_source=parsed)

    def test_actual_raw_profile_hash_path_and_length_are_not_advisory(self):
        for key, bad in (("path", "internal/elsewhere.json"), ("bytes", True), ("sha256", J)):
            changed = self.p.rebind(self.introduction,
                declaration_change=lambda d: d["after"]["tenant_source"].update({key: bad}))
            self.assertIn("classifier-v2-invalid:source-classification", self.classify(changed).reasons)

    def test_catalog_io_diagnostic_remains_catalog(self):
        base, head = self.pair(); original = sc._Repository.blob
        def fail(repo, revision, path):
            if revision == base and path == sc.CATALOG: raise sc._Invalid()
            return original(repo, revision, path)
        with mock.patch.object(sc._Repository, "blob", fail):
            self.assertIn("classifier-v2-invalid:catalog", sc.classify_v2(self.repo.root, base, head).reasons)

    def test_absent_descriptors_preserve_normal_none_requirements(self):
        value = self.introduction
        raw = (None, None, value[2], value[2], value[4], value[4], value[6], value[6])
        result = self.classify(raw)
        self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
        self.assertEqual(result.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_generic_tenant_exemption_does_not_cover_other_declaration_edits(self):
        value = self.introduction
        source = value[-1].replace(b'"whatsapp_coexistence_states"', b'"organizations"')
        value = self.p.rebind(value, raw_changes={5: source})
        self.assertIn("classifier-v2-invalid:source-classification", self.classify(value).reasons)

    def test_legacy_public_guard_is_not_silently_replaced(self):
        base, head = self.pair()
        self.assertIn("catalog-changed:" + sc.CATALOG, sc.classify(self.repo.root, base, head).reasons)

    def test_real_source_scope_identity_and_transport_refusal(self):
        base, head = self.pair(); original = sc._git
        for path in (sc.schema_relation.SOURCE_PATH, sc.schema_tenant_profile.SOURCE_PATH):
            def fail(root, args):
                if args[:2] == ["ls-tree", "-z"] and args[-1] == path:
                    raise source_git.SourceGitRefusal("git-failed:source-deadline")
                return original(root, args)
            with mock.patch.object(sc, "_git", side_effect=fail), self.assertRaises(source_git.SourceGitRefusal):
                sc.classify_v2(self.repo.root, base, head)


class FixedRemovesWriteCISourceTests(unittest.TestCase):
    """Immutable Git cases, not a measured Go closure or seed execution claim."""

    POSTGRES = "internal/database/postgres.go"
    NAME = "SeedSystemRolesForAllOrgs"
    CHILD = NAME + "/legacy-super-admin-update"
    BINDINGS = "internal/dbcatalog/registry/coordinator_bindings.json"
    REASON = "migration-path-changed:" + POSTGRES + "#" + NAME
    BEFORE_FUNCTION = 'func SeedSystemRolesForAllOrgs(db *gorm.DB) error {\n\tvar orgs []models.Organization\n\tif err := db.Scopes(ExcludePlatformComplianceOrganizations).Find(&orgs).Error; err != nil {\n\t\treturn fmt.Errorf("failed to fetch organizations: %w", err)\n\t}\n\n\tfor _, org := range orgs {\n\t\tif err := seedSystemRolesForOrg(db, org.ID, false); err != nil {\n\t\t\treturn fmt.Errorf("failed to seed roles for org %s: %w", org.ID, err)\n\t\t}\n\t}\n\n\t// Fix any system roles that don\'t have permissions linked\n\tif err := FixSystemRolePermissions(db); err != nil {\n\t\treturn fmt.Errorf("failed to fix role permissions: %w", err)\n\t}\n\n\t// Remove the legacy sensitive Settings grants from system manager roles\n\t// exactly once per organization. The version marker prevents a later\n\t// explicit super-admin grant from being removed on restart.\n\tif err := ApplyManagerSettingsPolicyMigration(db); err != nil {\n\t\treturn fmt.Errorf("failed to apply manager settings policy: %w", err)\n\t}\n\n\t// Migrate existing users from old role column to new role_id\n\tif err := MigrateExistingUserRoles(db); err != nil {\n\t\treturn fmt.Errorf("failed to migrate user roles: %w", err)\n\t}\n\n\t// Make admin@admin.com a super admin if exists\n\tif err := db.Exec("UPDATE users SET is_super_admin = true WHERE email = \'admin@admin.com\'").Error; err != nil {\n\t\treturn fmt.Errorf("failed to set super admin: %w", err)\n\t}\n\n\treturn nil\n}'
    REMOVED = '\t// Make admin@admin.com a super admin if exists\n\tif err := db.Exec("UPDATE users SET is_super_admin = true WHERE email = \'admin@admin.com\'").Error; err != nil {\n\t\treturn fmt.Errorf("failed to set super admin: %w", err)\n\t}\n\n'
    OLD_HASH = "8d0b423d4384c1f4427fbe3f0118e98a3ccdcb2ad24b9f29520111ad346f95e1"
    CHILDREN = ["seedSystemRolesForOrg", "FixSystemRolePermissions",
                "ApplyManagerSettingsPolicyMigration", "MigrateExistingUserRoles"]
    RATIONALE = "removes-write: legacy admin@admin.com escalation removed at rev 2; retained inert marker bound to parent closure"

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.repo = support.TempRepo(Path(temp.name) / "repo")
        self.old_source = "package database\n\n" + self.BEFORE_FUNCTION + "\n\nfunc unrelated() {}\n"
        self.new_source = self.old_source.replace(self.REMOVED, "", 1)
        # Independently embedded reviewed source preimage; never copy the gate's
        # expected source/hash or derive the old function from its new result.
        self.assertEqual(hashlib.sha256(self.BEFORE_FUNCTION.encode()).hexdigest(),
                         "4f2f5aa66866133bb2e5cc01a30ed2f726649e660c2bd8a75c6fdcdbd5401b34")
        self.assertEqual(self.BEFORE_FUNCTION.count(self.REMOVED), 1)
        self.active = {path: (ROOT / path).read_text(encoding="utf-8") for path in (
            sc.CATALOG, sc.HISTORY, sc.GLOBALS, sc.SEEDS,
            "internal/dbcatalog/golden/production-v0.json", "internal/dbcatalog/shape/production-v0.sql")}
        rows = json.loads((ROOT / sc.REGISTRY).read_bytes())
        for entry in rows:
            if entry["id"] in (self.NAME, self.CHILD):
                entry.update(rev=1, closure_sha256=self.OLD_HASH)
        self.old_rows = rows
        self.new_rows = json.loads(text(rows))
        # A coherent synthetic claim only; real PR17 hashes require Go CI.
        for entry in self.new_rows:
            if entry["id"] in (self.NAME, self.CHILD):
                entry.update(rev=2, closure_sha256=hashlib.sha256(self.new_source.encode()).hexdigest())
        bindings = json.loads((ROOT / self.BINDINGS).read_bytes())
        bindings["seed_child_order"] = [*self.CHILDREN, "legacy-super-admin-update"]
        bindings["inline_sql"] = "UPDATE users SET is_super_admin = true WHERE email = 'admin@admin.com'"
        bindings["idempotency_rationale"][self.CHILD] = "Existing inline UPDATE sets the same boolean; no permission-policy endorsement"
        self.old_binding = bindings
        self.new_binding = json.loads(text(bindings))
        self.new_binding.update(seed_child_order=self.CHILDREN, inline_sql="")
        self.new_binding["idempotency_rationale"][self.CHILD] = self.RATIONALE
        self.old_migration = json.loads((ROOT / sc.MIGRATION_PATH).read_bytes())
        self.old_migration["closure_sha256"] = H
        self.new_migration = {**self.old_migration, "closure_sha256": J}
        self.files = {**self.active, self.POSTGRES: self.old_source,
            sc.REGISTRY: text(self.old_rows), self.BINDINGS: text(self.old_binding),
            sc.MIGRATION_PATH: text(self.old_migration),
            "internal/dbcatalog/registry/registry_test.go": "package registry\n", "README.md": "before\n"}
        self.target = {self.POSTGRES: self.new_source, sc.REGISTRY: text(self.new_rows),
                       self.BINDINGS: text(self.new_binding), sc.MIGRATION_PATH: text(self.new_migration)}
        self.base = self.repo.commit(self.files, "reviewed synthetic before")

    def head(self, extra=None):
        self.repo.checkout(self.base)
        return self.repo.commit({**self.target, **(extra or {})}, "synthetic removal")

    def refused(self, head, base=None):
        result = sc.check_ci_source(self.repo.root, base or self.base, head)
        self.assertIn(self.REASON, result)
        return result

    def test_exact_removal_admits_only_ci_and_preserves_release_refusal(self):
        head = self.head()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        policy = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons), ("data", True, ()))
        self.assertEqual(policy.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(sc.check(self.repo.root, self.base, head), ("migration-path-changed:closure",))
        with self.assertRaises(sc.SchemaChangeBlocked):
            sc.guard(self.repo.root, self.base, head)

    def test_allowed_test_commits_surround_one_atomic_transition(self):
        self.repo.commit({"internal/database/catalog_legacy_admin_test.go": "package database\n"})
        self.repo.commit(self.target)
        head = self.repo.commit({"release/deployment/fixtures/classifier/pr17-rev-bump.json": "{}\n",
            "release/deployment/fixtures/classifier/pr17-no-rev-refused.json": "{}\n",
            "internal/dbcatalog/registry/registry_test.go": "package registry\n// rev 2 contract\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())

    def test_unrelated_legacy_clean_source_path_keeps_existing_behavior(self):
        head = self.repo.commit({self.POSTGRES: self.old_source.replace("func unrelated() {}", "func unrelated() { println(2) }")})
        self.assertEqual(sc.check(self.repo.root, self.base, head), ())
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())

    def test_stale_manifest_cannot_hide_target_behind_clean_fast_path(self):
        head = self.head({sc.MIGRATION_PATH: text(self.old_migration)})
        self.assertEqual(sc.check(self.repo.root, self.base, head), ())
        self.refused(head)

    def test_missing_or_invalid_paired_revision_refuses(self):
        for names, value in (((self.NAME,), 1), ((self.CHILD,), 1), ((self.NAME, self.CHILD), 1),
                             ((self.NAME,), True), ((self.CHILD,), 3), ((self.NAME,), 0)):
            with self.subTest(names=names, value=value):
                rows = json.loads(text(self.new_rows))
                for entry in rows:
                    if entry["id"] in names:
                        entry["rev"] = value
                self.refused(self.head({sc.REGISTRY: text(rows)}))

    def test_complete_registry_identity_order_and_unrelated_rows_refuse(self):
        mutations = []
        rows = json.loads(text(self.new_rows)); rows = [r for r in rows if r["id"] != self.CHILD]; mutations.append(rows)
        rows = json.loads(text(self.new_rows)); rows.reverse(); mutations.append(rows)
        rows = json.loads(text(self.new_rows)); rows.append({**rows[0], "id": "UnreviewedHistoricalID"}); mutations.append(rows)
        rows = json.loads(text(self.new_rows)); next(r for r in rows if r["id"] not in (self.NAME,self.CHILD))["rev"] += 1; mutations.append(rows)
        for rows in mutations:
            with self.subTest(ids=[r["id"] for r in rows]):
                self.refused(self.head({sc.REGISTRY: text(rows)}))

    def test_parent_child_security_and_closure_metadata_refuse(self):
        variants = (("func", "example.invalid.Other"), ("kind", "baseline-pre-ledger"), ("baseline", False),
                    ("idempotent", False), ("n1_safe", True), ("requires_gates", ["new-gate"]),
                    ("closure_files", ["internal/database/postgres.go"]), ("parent", "Other"))
        for field, value in variants:
            with self.subTest(field=field):
                rows = json.loads(text(self.new_rows))
                next(r for r in rows if r["id"] == self.CHILD)[field] = value
                self.refused(self.head({sc.REGISTRY: text(rows)}))

    def test_old_claims_must_also_have_reviewed_parent_child_semantics(self):
        for field, value in (("n1_safe", True), ("idempotent", False), ("parent", "Other")):
            with self.subTest(field=field):
                self.repo.checkout(self.base)
                old, new = json.loads(text(self.old_rows)), json.loads(text(self.new_rows))
                for rows in (old, new):
                    next(r for r in rows if r["id"] == self.CHILD)[field] = value
                base = self.repo.commit({sc.REGISTRY: text(old)})
                head = self.repo.commit({**self.target, sc.REGISTRY: text(new)})
                self.refused(head, base)

    def test_shared_closure_hashes_must_agree_and_advance(self):
        for names, digest in (((self.CHILD,), H), ((self.NAME, self.CHILD), self.OLD_HASH)):
            with self.subTest(names=names):
                rows = json.loads(text(self.new_rows))
                for entry in rows:
                    if entry["id"] in names: entry["closure_sha256"] = digest
                self.refused(self.head({sc.REGISTRY: text(rows)}))

    def test_noop_source_with_plausible_generated_claims_is_not_allowed(self):
        head = self.head({self.POSTGRES: self.old_source})
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))

    def test_altered_function_is_not_a_removal(self):
        for source in (self.old_source.replace("admin@admin.com", "another@example.invalid"),
                       self.old_source.replace("is_super_admin = true", "is_super_admin = false"),
                       self.new_source.replace("return nil", "return err", 1),
                       self.new_source.replace("FixSystemRolePermissions", "UnreviewedWriter")):
            with self.subTest(source=hashlib.sha256(source.encode()).hexdigest()):
                self.refused(self.head({self.POSTGRES: source}))

    def test_removal_does_not_allow_other_file_remainder_changes(self):
        for source in (self.new_source + "func addedWriter() {}\n",
                       self.new_source.replace("package database", 'package database\nimport "unsafe"'),
                       self.new_source.replace("func unrelated() {}", "func unrelated() { println(3) }")):
            with self.subTest(source=hashlib.sha256(source.encode()).hexdigest()):
                self.refused(self.head({self.POSTGRES: source}))

    def test_missing_duplicate_and_malformed_target_refuse(self):
        for source in (None, "package database\n", self.new_source + self.BEFORE_FUNCTION,
                       "package database\nfunc SeedSystemRolesForAllOrgs( {\n"):
            with self.subTest(source=source is None):
                self.refused(self.head({self.POSTGRES: source}))

    def test_exact_coordinator_transform_is_required(self):
        for field, value in (("inline_sql", self.old_binding["inline_sql"]),
                             ("seed_child_order", self.old_binding["seed_child_order"]),
                             ("seed_child_calls", self.CHILDREN[:-1]),
                             ("coordinator_sha256", H), ("callback_sha256", J),
                             ("model_descriptors", []), ("version", True)):
            with self.subTest(field=field):
                binding = {**self.new_binding, field: value}
                self.refused(self.head({self.BINDINGS: text(binding)}))
        binding = json.loads(text(self.new_binding)); binding["idempotency_rationale"][self.CHILD] = "removes-write"
        self.refused(self.head({self.BINDINGS: text(binding)}))

    def test_incomplete_coordinator_cannot_be_retained_at_both_endpoints(self):
        for key in ("coordinator_sha256", "callback_calls", "model_list_sha256"):
            with self.subTest(key=key):
                self.repo.checkout(self.base)
                old, new = dict(self.old_binding), dict(self.new_binding)
                del old[key]; del new[key]
                base = self.repo.commit({self.BINDINGS: text(old)})
                head = self.repo.commit({**self.target, self.BINDINGS: text(new)})
                self.refused(head, base)

    def test_changed_before_function_is_not_the_reviewed_preimage(self):
        before = self.old_source.replace("FixSystemRolePermissions", "OtherSeedWriter")
        base = self.repo.commit({self.POSTGRES: before})
        head = self.repo.commit({**self.target, self.POSTGRES: before.replace(self.REMOVED, "", 1)})
        self.refused(head, base)

    def test_migration_roots_files_and_hash_must_match_fixed_transition(self):
        for change in ({"roots": self.new_migration["roots"][:-1]},
                       {"closure_files": self.new_migration["closure_files"][:-1]},
                       {"closure_sha256": H}, {"version": True}):
            with self.subTest(change=tuple(change)):
                self.refused(self.head({sc.MIGRATION_PATH: text({**self.new_migration, **change})}))

    def test_active_material_must_be_present_nonempty_byte_equal(self):
        for path in self.active:
            with self.subTest(path=path):
                self.refused(self.head({path: self.active[path] + "\n"}))
                self.refused(self.head({path: None}))
        for path in ("internal/dbcatalog/golden/production-v0.json", "internal/dbcatalog/shape/production-v0.sql"):
            with self.subTest(empty=path):
                self.repo.checkout(self.base)
                base = self.repo.commit({path: ""})
                head = self.repo.commit(self.target)
                self.refused(head, base)

    def test_no_other_production_policy_configuration_or_workflow_edit(self):
        for path in ("internal/config/config.go", "internal/models/models.go", "internal/database/tenant.go",
                     "cmd/whatomate/main.go", "go.mod", ".github/workflows/test.yml", ".gitattributes",
                     "release/deployment/schema_change.py", "README.md"):
            with self.subTest(path=path):
                self.refused(self.head({path: "unreviewed\n"}))

    def test_pending_and_source_declarations_cannot_be_retained_or_added(self):
        paths = ("internal/dbcatalog/golden/pending/example.json", sc.schema_nullable.SOURCE_PATH,
                 sc.schema_nullable.CONSTANT_SOURCE_PATH, sc.schema_index.SOURCE_PATH,
                 sc.schema_relation.SOURCE_PATH, sc.schema_tenant_profile.SOURCE_PATH)
        for path in paths:
            with self.subTest(path=path):
                self.refused(self.head({path: "{}\n"}))
                self.repo.checkout(self.base)
                base = self.repo.commit({path: "{}\n"})
                head = self.repo.commit(self.target)
                self.refused(head, base)

    def test_missing_or_malformed_generated_documents_refuse(self):
        for path in (sc.REGISTRY, sc.MIGRATION_PATH, self.BINDINGS):
            for raw in (None, "{", '{"version":1,"version":1}\n'):
                with self.subTest(path=path, raw=raw):
                    self.refused(self.head({path: raw}))

    def test_active_engine_cannot_enter_dormant_exception(self):
        active_history = json.loads(self.active[sc.HISTORY])
        active_history[-1]["apply_engine"] = True
        base = self.repo.commit({sc.HISTORY: text(active_history)})
        head = self.repo.commit(self.target)
        self.refused(head, base)

    def test_partial_first_parent_generated_state_refuses(self):
        self.repo.commit({self.POSTGRES: self.new_source})
        head = self.repo.commit(self.target)
        self.refused(head)

    def test_intermediate_wrong_registry_metadata_cannot_be_restored(self):
        rows = json.loads(text(self.new_rows)); rows.reverse()
        self.repo.commit({**self.target, sc.REGISTRY: text(rows)})
        self.refused(self.repo.commit(self.target))

    def test_atomic_state_cannot_revert_then_reapply(self):
        self.repo.commit(self.target)
        self.repo.commit({path: self.files[path] for path in self.target})
        self.refused(self.repo.commit(self.target))

    def test_transient_unapproved_source_change_cannot_be_laundered(self):
        self.repo.commit({"internal/config/config.go": "package config\n"})
        self.refused(self.repo.commit({**self.target, "internal/config/config.go": None}))

    def test_generated_contract_removal_reintroduction_refuses(self):
        self.repo.commit({sc.MIGRATION_PATH: None})
        self.refused(self.repo.commit(self.target))

    def test_changed_regular_file_mode_is_not_a_byte_only_removal(self):
        self.head()
        self.repo.git("update-index", "--chmod=+x", self.POSTGRES)
        self.repo.git("commit", "--quiet", "-m", "unsupported mode")
        self.refused(self.repo.head())

    def test_full_overcap_document_uses_existing_blob_bound(self):
        self.refused(self.head({self.BINDINGS: " " * (sc.MAX_ARTIFACT_BYTES + 1)}))


class PR18ObserverTransitionTests(unittest.TestCase):
    """Pinned source with one reviewed timing transform; histories are synthetic.

    The fixture is self-contained. Neither source currently checked out, private
    predicate constants, generated expectations nor network state supply an
    oracle. The old public classifier remains a refusal authority.
    """

    FIXTURE_SHA = '8fff1020532785da88e79cac5d2f398cbb2cf94936f9511c40773300a11c3235'
    FIXTURE_BYTES = 3104343
    SOURCE = {'.github/workflows/test.yml': ['M',
                                '8adfd6a64bff17a078e8daa626cecc85d1118ebbc96a063b5c0c5cfc739ff2f9',
                                '3f8d54640780c2e7dee3a59480a9d3a44004c2c8462f367e44bbfad546b04c16',
                                '100644'],
 'cmd/whatomate/main.go': ['M',
                           '3a13697d9839c9ef2cf1b2f6b76d53449c81ce2927cc087e7d2773f6f4cf1e0b',
                           '6da28438261bf29ffe1b02a36c8ed8ff2ba2620df821979b458790e882bc41d8',
                           '100644'],
 'cmd/whatomate/migration_plan.go': ['A',
                                     None,
                                     '43bbdc7d9733e5cd0fa76a1129cab1ed70dd70be7d516061278cdd15e17e9dde',
                                     '100644'],
 'cmd/whatomate/migration_plan_pg_test.go': ['A',
                                             None,
                                             '49dfd9e27e336b909bd6e2a698863db089bab94616dafc92830c52178cc16814',
                                             '100644'],
 'cmd/whatomate/migration_plan_relay_test.go': ['A',
                                                None,
                                                'f2f3b65c119c82437ea25b2015d8685242442e423a5736b72f15bab96f7326e6',
                                                '100644'],
 'cmd/whatomate/migration_plan_test.go': ['A',
                                          None,
                                          'c3ce07dc149bb428142d52a13c17be30be1ac95ff100886313f8d2077c5d6a49',
                                          '100644'],
 'cmd/whatomate/migration_plan_two_server_pg_test.go': ['A',
                                                        None,
                                                        '5e8225306bd5e4243f9c880ef3079525977bdfee17642e73a04b4dd524f618a3',
                                                        '100644'],
 'internal/database/catalog_plan_integration_test.go': ['A',
                                                        None,
                                                        '7a571e81f11963e73707603af2a1fe339bd69795b9b5d3486c7acd8b5517cc5e',
                                                        '100644'],
 'internal/database/data_gates.go': ['A',
                                     None,
                                     'fdd250d045ba4676262d8e803df14645d8bfd896906a15c12956252facf6513d',
                                     '100644'],
 'internal/database/data_gates_export_test.go': ['A',
                                                 None,
                                                 'dd231916a0e9616aef48dd49de376644b23943986fc402c7f783af8378b974d9',
                                                 '100644'],
 'internal/database/data_gates_integration_test.go': ['A',
                                                      None,
                                                      '71b46620c46cb1c6e1e4b9b77ec21102fa8d9f300b4f118e473f97c82be26777',
                                                      '100644'],
 'internal/database/data_gates_lost_ack_integration_test.go': ['A',
                                                               None,
                                                               'e41c2456cc1a926607e16fd84a19f729202f126ad86c6b2ce503ecab840793aa',
                                                               '100644'],
 'internal/database/plan.go': ['A',
                               None,
                               '91ab1d7114a42d4166aa88b632d22c56291da2a39d265bba3c12770cf091d3ee',
                               '100644'],
 'internal/database/plan_semantics_test.go': ['A',
                                              None,
                                              '193b5e7882fdf3df5efbbd503650a0f16ed583292df088612ebf48c5480429cb',
                                              '100644'],
 'internal/database/plan_test.go': ['A',
                                    None,
                                    'dabb7a6982cbd7b066fa5b41e83d06f9de5e64798e251389395e4351b29f13cb',
                                    '100644'],
 'internal/database/plan_visibility.go': ['A',
                                          None,
                                          'b9e332f30ca4f03f25bef9e75e8e7ed8d60762c4d1e38e13ffe5d27215e3b9e4',
                                          '100644'],
 'internal/database/postgres.go': ['M',
                                   '1b7ca0bdcfdf0696ca88aeec8f7a528834ef41cf9c8e47e338a18382719e6fb4',
                                   '3dbf326842c2b369551dd2b37c779159ffeff2bd8ea65bd4fd6d031a2d66c76d',
                                   '100644'],
 'internal/dbcatalog/builtin.go': ['M',
                                   '9e1bf8e46f2eb061a1e841c72b476d7e40c02f0865107060995845250e6f1506',
                                   '74b0b9550591e8152f02ed2d0c5d207936f8fb02f5829d3595eedb11c30fa524',
                                   '100644'],
 'internal/dbcatalog/capture.go': ['M',
                                   '2db6d6f6e7df718f2dbd0afcb7394e9486f1ef08567e2775bd58c1a4e5ca6c42',
                                   'cf8f820421107e59db55af28dfdfa5e601ad8f36493f19770b9eefac25e03b06',
                                   '100644'],
 'internal/dbcatalog/capture_plan_test.go': ['A',
                                             None,
                                             '5b6945842dd89eb2afbad34c1e74666e9d36fae8186a3745f4cdbd8ede7876b8',
                                             '100644'],
 'internal/dbcatalog/format.go': ['M',
                                  'e374e1ae11f9dbf4b6e2e49736552aece0172be366f586ae8e964dd80339c9d7',
                                  '985251243b85ceb04948b00c988c471567df7ccd0197e4d7995b2e466c64c5da',
                                  '100644'],
 'internal/dbcatalog/plan.go': ['A',
                                None,
                                'ffe9b24f4558227f37169c5b15b6677bf7dca2931925905f7349d2b95e50e9f8',
                                '100644'],
 'internal/dbcatalog/plan_source.go': ['A',
                                       None,
                                       'b2a7dc810a68606c8349635953e23f57f14d41e107c497d65f165efbd889e35c',
                                       '100644'],
 'internal/dbcatalog/plan_source_test.go': ['A',
                                            None,
                                            'ad78c536d2637f00f6376f334cce582a82388f0ff2d9cbe33ff48bb31db780ae',
                                            '100644'],
 'internal/dbcatalog/plan_test.go': ['A',
                                     None,
                                     'e125ae07909633759cfc7992b0a7e317d063684f76a06626a754a809fb0f07db',
                                     '100644'],
 'internal/dbcatalog/registry/closure_caps.json': ['M',
                                                   '02ea976eb866f8fac166b1629803f8c85afe654fec6e3198f1d2e62357c374b6',
                                                   '9ccfdc7e61e9fb52d7a60a0653fc69ffdc2a82740fc0b5d728e1516fb40c8dcf',
                                                   '100644'],
 'internal/dbcatalog/registry/data_steps.json': ['M',
                                                 '25041acd83c2f87921d994746d63a3b00bc937191c13d91cfa59e2d1a09f668a',
                                                 '7df7b9277ca507ef634e62ae2992d464f4701f5c5084fcb93a18951d6edf019c',
                                                 '100644'],
 'internal/dbcatalog/registry/migration_path.json': ['M',
                                                     '55b0b67461813c345b62a2588bc8c85d4ff50ea88b5c78732cc134488765d455',
                                                     'bde5e0c8ec946d37d60727ba6b20ede20ba6be19321e103dfe963d2615cf072e',
                                                     '100644'],
 'internal/dbcatalog/registry/migration_path_test.go': ['M',
                                                        '9d566011163f0cd2ab78609334948e105f1a552d5dd26a943e9e4f7c24d13c13',
                                                        '41aa23161c5467d7f4d72cd9e9eeb971e8b223830e749da49d95bc05313830e9',
                                                        '100644'],
 'internal/dbcatalog/registry/registry_test.go': ['M',
                                                  'b5082386816506c38da02c508e930f115cbac28fc2071aa380c1d729bdce8c79',
                                                  '8da9c1d3e143b96590ad4bdca0086a01403e6b7a03696feffebfed1e65e27ef3',
                                                  '100644'],
 'release/deployment/test_ci_workflows.py': ['M',
                                             '6131bf1587594ec935b4aca2e5f2183623dbc16461d54588d0cb6d95ec64043e',
                                             '94a85a349b24446c83227d14c88d5f7480adb906a08511e0473d537ebb8b7233',
                                             '100644']}
    SHARED = {'go.mod': 'a4118a0f3460522627efa2e1074df40ccd3c72fdb39b09e6dd201ca7d3f47e25',
 'go.sum': '053cf6edc13c5f2570c9d5bdadca83d8399b0c3c63c7f2f9f8cbb54d1c1ee101',
 'internal/channel/legacy_meta.go': '007a73958e8525627dd93a8f40132ddc173a09013a86845e9924ff732266b002',
 'internal/database/platform_compliance.go': '20c4532d8d1cb7a3f59c56f85b1f0f3dc100ccae46b13205547906b8a22bd218',
 'internal/database/tenant.go': '69101f7f18b2c2fe8b4a16d22033995857580b93e7a658d6aabc55984b2dbf5f',
 'internal/dbcatalog/dormant_entrypoints.json': '041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d',
 'internal/dbcatalog/golden/catalog.json': '17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8',
 'internal/dbcatalog/golden/global_tables.json': 'c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3',
 'internal/dbcatalog/golden/history.json': '9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513',
 'internal/dbcatalog/golden/production-v0.json': '808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d',
 'internal/dbcatalog/golden/seeds.json': 'dac095bdb1d6aff7c54fccd13bcbfa4bbff131154f9589dc5ae476331ffeca77',
 'internal/dbcatalog/registry/coordinator_bindings.json': 'ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4',
 'internal/dbcatalog/shape/production-v0.sql': '48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d'}
    HISTORICAL_SHA = '6e4555346d9e8ebd41eac8992cb5b5add17a653239938a181e20c2f9f67e2d94'
    MAIN = "cmd/whatomate/main.go"
    POSTGRES = "internal/database/postgres.go"
    DORMANT = "internal/dbcatalog/dormant_entrypoints.json"
    BINDINGS = "internal/dbcatalog/registry/coordinator_bindings.json"
    REGISTRY = "internal/dbcatalog/registry/data_steps.json"
    MIGRATION = "internal/dbcatalog/registry/migration_path.json"
    ACTIVE = (
        "internal/dbcatalog/golden/catalog.json",
        "internal/dbcatalog/golden/history.json",
        "internal/dbcatalog/golden/global_tables.json",
        "internal/dbcatalog/golden/seeds.json",
        "internal/dbcatalog/golden/production-v0.json",
        "internal/dbcatalog/shape/production-v0.sql",
    )
    DESCRIPTORS = (
        "internal/dbcatalog/profiles/nullable-column-v1/source-shape-v1.json",
        "internal/dbcatalog/source-classification/int8-constant-column-v1.json",
        "internal/dbcatalog/source-classification/index-concurrent-v1.json",
        "internal/dbcatalog/source-classification/relation-v1.json",
        "internal/dbcatalog/source-classification/tenant-additive-v1.json",
    )
    LEGACY_REASONS = (
        "migration-call:cmd/whatomate/main.go",
        "migration-path-changed:closure",
        "migration-path-changed:cmd/whatomate/main.go#runRLSMigration",
        "migration-path-changed:internal/database/postgres.go#runRLSMigrationCoordinatorForPhase",
    )
    REQUIREMENTS = ("compat-required", "generated-migration-closure-ci")
    MIGRATION_OBSERVATIONS = (
        "generated migration closure changed",
        "migration path changed: cmd/whatomate/main.go#runRLSMigration",
        "migration path changed: internal/database/postgres.go#runRLSMigrationCoordinatorForPhase",
    )

    @classmethod
    def setUpClass(cls):
        # Whole-file digest and exact section inventories make this tiny parser
        # fixture-specific. No generic patch application or external command.
        path = Path(__file__).parent / "fixtures/pr18-observer-transition.patch"
        with path.open("rb") as stream:
            raw = stream.read(cls.FIXTURE_BYTES + 1)
        if len(raw) != cls.FIXTURE_BYTES or hashlib.sha256(raw).hexdigest() != cls.FIXTURE_SHA:
            raise AssertionError("PR18 fixture identity changed")
        header, separator, rest = raw.partition(b"\n")
        if not separator or header != b"# PR18 source fixture with reviewed 45-minute release-test budget; synthetic histories only, not qualification.":
            raise AssertionError("PR18 fixture header")
        sections = {}
        while rest:
            header, separator, rest = rest.partition(b"\n")
            parts = header.decode("ascii").split(" ")
            if not separator or len(parts) != 4 or parts[0] != "@@@" or not parts[3].isdigit():
                raise AssertionError("PR18 fixture section")
            key = (parts[1], parts[2])
            size = int(parts[3])
            if key in sections or not 0 < size <= cls.FIXTURE_BYTES or len(rest) < size:
                raise AssertionError("PR18 fixture section bounds")
            sections[key], rest = rest[:size], rest[size:]
        expected = {("PATCH", "transition"), ("HISTORICAL", cls.MAIN)} | {("SHARED", p) for p in cls.SHARED}
        if set(sections) != expected:
            raise AssertionError("PR18 fixture section inventory")
        before, after = {}, {}
        lines = iter(sections[("PATCH", "transition")].decode("utf-8").splitlines(keepends=True))
        for name, (status, old_sha, new_sha, mode) in cls.SOURCE.items():
            old_header = "a/" + name if status == "M" else "/dev/null"
            if next(lines, None) != "--- " + old_header + "\n" or next(lines, None) != "+++ b/" + name + "\n":
                raise AssertionError("PR18 fixture patch path")
            hunk = next(lines, "").strip().split(" ")
            if len(hunk) != 4 or hunk[0] != "@@" or hunk[3] != "@@":
                raise AssertionError("PR18 fixture patch hunk")
            left, right = [], []
            ranges = []
            for field, sign in zip(hunk[1:3], ("-", "+")):
                if not field.startswith(sign):
                    raise AssertionError("PR18 fixture patch range")
                parts = field[1:].split(",")
                if len(parts) not in (1, 2) or not all(p.isdigit() for p in parts):
                    raise AssertionError("PR18 fixture patch count")
                start, count = int(parts[0]), int(parts[1]) if len(parts) == 2 else 1
                if start != (1 if count else 0) or count > cls.FIXTURE_BYTES:
                    raise AssertionError("PR18 fixture full-context range")
                ranges.append(count)
            while len(left) < ranges[0] or len(right) < ranges[1]:
                line = next(lines, "")
                if not line or line[0] not in " +-":
                    raise AssertionError("PR18 fixture patch body")
                if line[0] in " -":
                    left.append(line[1:])
                if line[0] in " +":
                    right.append(line[1:])
                if len(left) > ranges[0] or len(right) > ranges[1]:
                    raise AssertionError("PR18 fixture patch overflow")
            left, right = "".join(left), "".join(right)
            if mode != "100644" or hashlib.sha256(right.encode()).hexdigest() != new_sha:
                raise AssertionError("PR18 fixture postimage identity")
            if status == "A":
                if left or old_sha is not None:
                    raise AssertionError("PR18 fixture preimage must be absent")
            elif status == "M" and hashlib.sha256(left.encode()).hexdigest() == old_sha:
                before[name] = left
            else:
                raise AssertionError("PR18 fixture preimage identity")
            after[name] = right
        if next(lines, None) is not None or len(before) != 12 or len(after) != 31:
            raise AssertionError("PR18 fixture closed patch inventory")
        for name, digest in cls.SHARED.items():
            value = sections[("SHARED", name)]
            if hashlib.sha256(value).hexdigest() != digest or name in before or name in after:
                raise AssertionError("PR18 fixture shared identity")
            before[name] = value.decode("utf-8")
        historical = sections[("HISTORICAL", cls.MAIN)]
        if hashlib.sha256(historical).hexdigest() != cls.HISTORICAL_SHA:
            raise AssertionError("PR18 fixture historical identity")
        cls.before, cls.after, cls.historical = before, after, historical.decode("utf-8")

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.base = self.repo.commit(self.before, "fixed M source fixture")

    def head(self, extra=None):
        self.repo.checkout(self.base)
        return self.repo.commit({**self.after, **(extra or {})}, "fixed C18 source fixture")

    def policy(self, head, base=None):
        return sc.classify_v2(self.repo.root, base or self.base, head)

    def assert_policy_admitted(self, head, base=None):
        policy = self.policy(head, base)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons), ("data", True, ()))
        self.assertEqual(policy.requirements, self.REQUIREMENTS)
        self.assertEqual(policy.migration_changes, self.MIGRATION_OBSERVATIONS)

    def refused(self, head, base=None, *, ci=True, policy=True):
        if ci:
            self.assertTrue(sc.check_ci_source(self.repo.root, base or self.base, head))
        if policy:
            actual = self.policy(head, base)
            self.assertNotEqual((actual.kind, actual.dormant, actual.reasons, actual.requirements),
                                ("data", True, (), self.REQUIREMENTS))

    def historical_base(self):
        self.repo.checkout(self.base)
        historical = self.repo.commit({self.MAIN: self.historical, self.DORMANT: None}, "synthetic historical B")
        self.repo.commit({self.MAIN: self.before[self.MAIN], self.DORMANT: self.before[self.DORMANT]},
                         "reviewed bootstrap and dormant introduction")
        return historical

    def test_fixture_is_exact_12_modified_19_added_with_fixed_modes(self):
        head = self.head()
        expected = "".join(status + "\t" + path + "\n" for path, (status, *_) in self.SOURCE.items())
        self.assertEqual(self.repo.git("diff", "--name-status", self.base, head), expected)
        for path, (status, *_) in self.SOURCE.items():
            self.assertTrue(self.repo.git("ls-tree", head, "--", path).startswith("100644 blob "))
            if status == "A":
                self.assertEqual(self.repo.git("ls-tree", self.base, "--", path), "")
        for path in self.DESCRIPTORS:
            self.assertNotIn(path, self.before)
        self.assertFalse(any(path.startswith("internal/dbcatalog/golden/pending/") for path in self.before))
        for source in (self.before[".github/workflows/test.yml"], self.after[".github/workflows/test.yml"]):
            release_job = source.split("  release-tests:\n", 1)[1].split("  schema-guard:\n", 1)[0]
            self.assertIn("    timeout-minutes: 45\n", release_job)
            self.assertEqual(release_job.count("timeout-minutes:"), 1)

    def test_exact_transition_admits_ci_v2_but_not_legacy_release_guard(self):
        head = self.head()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assert_policy_admitted(head)
        self.assertEqual(sc.check(self.repo.root, self.base, head), self.LEGACY_REASONS)
        with self.assertRaises(sc.SchemaChangeBlocked) as caught:
            sc.guard(self.repo.root, self.base, head)
        self.assertEqual(caught.exception.reasons, self.LEGACY_REASONS)

    def test_exact_transition_through_real_bounded_immutable_git(self):
        head = self.head()
        for call in (sc.check_ci_source, sc.classify_v2):
            with self.subTest(entrypoint=call.__name__):
                reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
                with reader.operation():
                    result = call(self.repo.root, self.base, head)
                self.assertTrue(reader.closed)
                self.assertFalse(reader.failed)
                self.assertGreater(reader.calls, 0)
                self.assertLessEqual(reader.calls, 4096)
                self.assertLessEqual(reader.bytes, 64 * 1024 * 1024)
                if call is sc.check_ci_source:
                    self.assertEqual(result, ())
                else:
                    self.assertEqual((result.kind, result.dormant, result.reasons), ("data", True, ()))
                    self.assertEqual(result.requirements, self.REQUIREMENTS)
                    self.assertEqual(result.migration_changes, self.MIGRATION_OBSERVATIONS)

    def test_parent_reader_rejects_nonliteral_or_other_parent_revspecs(self):
        head = self.head()
        for revision in ("HEAD^1", head + "^2", head + "~1", "A" * 40 + "^1", head + "^1 "):
            with self.subTest(revision=revision):
                reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
                before_calls = None
                with self.assertRaises(source_git.SourceGitRefusal) as caught:
                    with reader.operation():
                        before_calls = reader.calls
                        reader.output(self.repo.root, ["rev-parse", "--verify", revision], code="unused-test-code")
                self.assertEqual(str(caught.exception), "git-failed:source-grammar")
                self.assertTrue(reader.failed and reader.closed)
                self.assertIsNotNone(before_calls)
                self.assertEqual(reader.calls, before_calls)

    def test_parent_transport_refusal_is_not_a_semantic_policy_refusal(self):
        head = self.head()
        for call in (sc.check_ci_source, sc.classify_v2):
            with self.subTest(entrypoint=call.__name__):
                real_git, seen = sc._git, []
                def refuse(root, args):
                    if args == ["rev-parse", "--verify", head + "^1"]:
                        seen.append(tuple(args))
                        raise source_git.SourceGitRefusal("git-failed:source-budget")
                    return real_git(root, args)
                with mock.patch.object(sc, "_git", side_effect=refuse):
                    with self.assertRaises(source_git.SourceGitRefusal) as caught:
                        call(self.repo.root, self.base, head)
                self.assertEqual(str(caught.exception), "git-failed:source-budget")
                self.assertEqual(len(seen), 1)

    def test_transport_budget_refusal_propagates_without_policy_fallback(self):
        head = self.head()
        for call in (sc.check_ci_source, sc.classify_v2):
            with self.subTest(entrypoint=call.__name__, stage="real-budget"):
                reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")),
                                                  limits=source_git.Limits(calls=8))
                with self.assertRaises(source_git.SourceGitRefusal) as caught:
                    with reader.operation():
                        call(self.repo.root, self.base, head)
                self.assertEqual(str(caught.exception), "git-failed:source-budget")
                self.assertTrue(reader.failed and reader.closed)
                self.assertEqual(reader.calls, 8)
            real_git, observed = sc._git, []
            def refuse(root, args):
                # The new proof reads this protected role after legacy/v2 work;
                # an external transport refusal must not be caught as _Invalid.
                if args[:2] == ["ls-tree", "-z"] and args[-1] == "internal/dbcatalog/shape/production-v0.sql":
                    observed.append(tuple(args))
                    raise source_git.SourceGitRefusal("git-failed:source-budget")
                return real_git(root, args)
            with self.subTest(entrypoint=call.__name__, stage="transition-proof"):
                with mock.patch.object(sc, "_git", side_effect=refuse):
                    with self.assertRaises(source_git.SourceGitRefusal) as caught:
                        call(self.repo.root, self.base, head)
                self.assertEqual(str(caught.exception), "git-failed:source-budget")
                self.assertEqual(len(observed), 1)

    def test_noop_prefix_and_suffix_do_not_create_another_transition(self):
        self.repo.commit({}, "empty prefix")
        self.repo.commit(self.after)
        head = self.repo.commit({}, "empty suffix")
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assert_policy_admitted(head)

    def test_exact_transition_on_first_parent_merge_is_not_tied_to_commit_id(self):
        self.repo.git("checkout", "--quiet", "-b", "candidate")
        self.repo.commit(self.after)
        self.repo.checkout(self.base)
        self.repo.git("merge", "--no-ff", "--no-edit", "candidate")
        head = self.repo.head()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assert_policy_admitted(head)

    def test_base_reachable_only_on_non_first_parent_cannot_supply_anchor(self):
        self.repo.git("checkout", "--quiet", "-b", "side")
        side_base = self.repo.commit({}, "base on non-first-parent branch")
        self.repo.checkout(self.base)
        self.repo.commit(self.after)
        self.repo.git("merge", "--no-ff", "--no-edit", "side")
        self.refused(self.repo.head(), side_base)

    def test_merge_with_base_only_as_second_parent_is_not_a_transition(self):
        # A -> B, then H with parents [A, B]. B is an ancestor of H, but
        # B..H --first-parent returns only H; B is not H's actual first parent.
        self.repo.git("checkout", "--quiet", "-b", "second-parent-base")
        side_base = self.repo.commit({}, "B descends from A")
        self.repo.checkout(self.base)
        self.repo.git("merge", "--no-ff", "--no-commit", "second-parent-base")
        head = self.repo.commit(self.after, "H product tree with parents A and B")
        self.assertEqual(self.repo.git("rev-list", "--parents", "-n", "1", head).split(),
                         [head, self.base, side_base])
        self.assertEqual(self.repo.git("rev-list", "--first-parent", "--reverse", side_base + ".." + head).split(),
                         [head])
        self.refused(head, side_base)

    def test_existing_history_limit_still_refuses(self):
        self.repo.commit({}, "empty prefix")
        head = self.repo.commit(self.after)
        with mock.patch.object(sc, "MAX_HISTORY_COMMITS", 1):
            self.refused(head)

    def test_each_exact_modified_postimage_alone_is_an_incomplete_bundle(self):
        for path, (status, *_) in self.SOURCE.items():
            if status == "M":
                with self.subTest(path=path):
                    self.repo.checkout(self.base)
                    head = self.repo.commit({path: self.after[path]}, "exact but partial PR18 postimage")
                    self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))

    def test_unrelated_none_edits_do_not_gain_a_general_source_fence(self):
        for path, suffix in ((".github/workflows/test.yml", "\n# unrelated comment\n"),
                             ("release/deployment/test_ci_workflows.py", "\n# unrelated comment\n"),
                             ("internal/dbcatalog/format.go", "\n// unrelated comment\n")):
            with self.subTest(path=path):
                self.repo.checkout(self.base)
                head = self.repo.commit({path: self.before[path] + suffix}, "unrelated none-class edit")
                self.assertEqual(sc.check(self.repo.root, self.base, head), ())
                self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())

    def test_every_postimage_is_fixed_including_tests_workflow_and_metadata(self):
        for path in self.SOURCE:
            with self.subTest(path=path):
                self.refused(self.head({path: self.after[path] + "\n"}))
        path = ".github/workflows/test.yml"
        with self.subTest(path=path, release_timeout="old-20-minute-postimage"):
            self.refused(self.head({path: self.after[path].replace("    timeout-minutes: 45\n", "    timeout-minutes: 20\n", 1)}))

    def test_every_modified_preimage_is_fixed(self):
        for path, (status, *_) in self.SOURCE.items():
            if status == "M":
                with self.subTest(path=path):
                    self.repo.checkout(self.base)
                    base = self.repo.commit({path: self.before[path] + "\n"})
                    self.refused(self.repo.commit(self.after), base)
        path = ".github/workflows/test.yml"
        with self.subTest(path=path, release_timeout="old-20-minute-preimage"):
            self.repo.checkout(self.base)
            base = self.repo.commit({path: self.before[path].replace("    timeout-minutes: 45\n", "    timeout-minutes: 20\n", 1)})
            self.refused(self.repo.commit(self.after), base)

    def test_every_added_file_must_be_absent_before_atomic_transition(self):
        for path, (status, *_) in self.SOURCE.items():
            if status == "A":
                with self.subTest(path=path):
                    self.repo.checkout(self.base)
                    base = self.repo.commit({path: self.after[path]})
                    self.refused(self.repo.commit(self.after), base)

    def test_changed_modes_on_modified_added_or_protected_files_refuse(self):
        for path in (self.MAIN, "cmd/whatomate/migration_plan.go", self.ACTIVE[0]):
            with self.subTest(path=path):
                self.head()
                self.repo.git("update-index", "--chmod=+x", path)
                self.repo.git("commit", "--quiet", "-m", "unsupported executable mode")
                self.repo.git("checkout", "--", path)
                self.assertEqual(self.repo.git("status", "--porcelain=v1", "--untracked-files=all"), "")
                self.refused(self.repo.head())

    def test_missing_or_renamed_postimage_refuses(self):
        path = "internal/database/plan.go"
        self.refused(self.head({path: None}))
        self.refused(self.head({path: None, "internal/database/unreviewed_plan.go": self.after[path]}))

    def test_each_active_protected_artifact_is_fixed_at_anchor(self):
        for path in self.ACTIVE:
            with self.subTest(path=path):
                self.refused(self.head({path: self.before[path] + "\n"}))

    def test_different_but_equal_active_endpoints_are_not_reviewed_state(self):
        path = self.ACTIVE[0]
        base = self.repo.commit({path: self.before[path] + "\n"})
        self.refused(self.repo.commit(self.after), base)

    def test_protected_intermediate_edit_cannot_be_restored(self):
        path = self.ACTIVE[0]
        self.repo.commit({path: self.before[path] + "\n"})
        self.repo.commit({path: self.before[path]})
        self.refused(self.repo.commit(self.after))

    def test_descriptors_and_pending_state_cannot_enter_exception(self):
        paths = (*self.DESCRIPTORS, "internal/dbcatalog/golden/pending/001.json")
        for path in paths:
            with self.subTest(path=path):
                self.refused(self.head({path: "{}\n"}))

    def test_intermediate_descriptor_cannot_be_removed_to_hide_it(self):
        path = self.DESCRIPTORS[0]
        self.repo.commit({path: "{}\n"})
        self.repo.commit({path: None})
        self.refused(self.repo.commit(self.after))

    def test_dormant_and_coordinator_bindings_are_not_byte_waivers(self):
        for path, value in ((self.DORMANT, None), (self.DORMANT, "[]\n"),
                            (self.DORMANT, self.before[self.DORMANT] + "\n"),
                            (self.BINDINGS, self.before[self.BINDINGS] + "\n")):
            with self.subTest(path=path, absent=value is None):
                self.refused(self.head({path: value}))

    def test_migration_closure_and_seed_revisions_must_be_actual_postimages(self):
        rows = json.loads(self.after[self.REGISTRY])
        mutations = []
        paired = {"SeedSystemRolesForAllOrgs", "SeedSystemRolesForAllOrgs/legacy-super-admin-update"}
        for row in rows:
            if row["id"] in paired:
                row["rev"] = 2
        mutations.append(text(rows))
        rows = json.loads(self.after[self.REGISTRY])
        rows[0]["closure_sha256"] = "0" * 64
        mutations.append(text(rows))
        rows = json.loads(self.after[self.REGISTRY])
        rows.reverse()
        mutations.append(text(rows))
        for raw in mutations:
            with self.subTest(digest=hashlib.sha256(raw.encode()).hexdigest()):
                self.refused(self.head({self.REGISTRY: raw}))
        self.refused(self.head({self.MIGRATION: self.before[self.MIGRATION]}))

    def test_partial_source_then_complete_metadata_is_not_atomic(self):
        self.repo.commit({self.MAIN: self.after[self.MAIN]})
        self.refused(self.repo.commit(self.after))

    def test_atomic_transition_cannot_revert_and_reapply(self):
        self.repo.commit(self.after)
        reset = {path: self.before.get(path) for path in self.SOURCE}
        self.repo.commit(reset)
        self.refused(self.repo.commit(self.after))

    def test_mutated_postanchor_state_cannot_be_restored(self):
        self.repo.commit(self.after)
        path = "cmd/whatomate/migration_plan.go"
        self.repo.commit({path: self.after[path] + "\n"})
        self.refused(self.repo.commit({path: self.after[path]}))

    def test_ci_whole_history_fence_rejects_reverted_unrelated_prefix(self):
        self.repo.commit({"README.md": "transient unrelated input\n"})
        self.repo.commit({"README.md": None})
        self.refused(self.repo.commit(self.after), policy=False)

    def test_ci_whole_history_fence_rejects_unrelated_suffix(self):
        self.repo.commit(self.after)
        self.refused(self.repo.commit({"README.md": "unrelated suffix\n"}), policy=False)

    def test_reviewed_historical_main_prefix_is_v2_only_not_ci(self):
        base = self.historical_base()
        head = self.repo.commit(self.after)
        reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")))
        with reader.operation():
            self.assert_policy_admitted(head, base)
        self.assertTrue(reader.closed)
        self.assertFalse(reader.failed)
        self.assertTrue(sc.check_ci_source(self.repo.root, base, head))
        self.assertIn("migration-call:cmd/whatomate/main.go", sc.check(self.repo.root, base, head))

    def test_historical_dormant_manifest_cannot_exist_before_reviewed_introduction(self):
        base = self.repo.commit({self.MAIN: self.historical}, "wrong dormant state at B")
        self.repo.commit({self.MAIN: self.before[self.MAIN]})
        self.refused(self.repo.commit(self.after), base)

    def test_historical_main_states_cannot_revert(self):
        base = self.historical_base()
        self.repo.commit({self.MAIN: self.historical, self.DORMANT: None})
        self.repo.commit({self.MAIN: self.before[self.MAIN], self.DORMANT: self.before[self.DORMANT]})
        self.refused(self.repo.commit(self.after), base)

    def test_unrelated_historical_main_call_cannot_be_laundered_by_restoration(self):
        self.repo.checkout(self.base)
        base = self.repo.commit({self.MAIN: self.historical, self.DORMANT: None})
        self.repo.commit({self.MAIN: self.historical + "\nfunc unreviewedMigration() { database.RunRLSMigrationCoordinator() }\n"})
        self.repo.commit({self.MAIN: self.historical})
        self.repo.commit({self.MAIN: self.before[self.MAIN], self.DORMANT: self.before[self.DORMANT]})
        self.refused(self.repo.commit(self.after), base)

    def test_added_call_on_another_path_keeps_its_own_refusal(self):
        path = "cmd/whatomate/unreviewed_migration.go"
        head = self.head({path: "package main\nfunc unexpected() { database.RunRLSMigrationCoordinator() }\n"})
        self.refused(head)
        self.assertIn("migration-call:" + path, self.policy(head).reasons)

    def test_completed_pr18_base_allows_later_test_only_edits(self):
        completed = self.head()
        for path in ("cmd/whatomate/migration_plan_test.go", "release/deployment/test_ci_workflows.py"):
            with self.subTest(path=path):
                self.repo.checkout(completed)
                head = self.repo.commit({path: self.after[path] + "\n"}, "later test-only newline")
                self.assertEqual(sc.check(self.repo.root, completed, head), ())
                self.assertEqual(sc.check_ci_source(self.repo.root, completed, head), ())

    def test_unchanged_reviewed_base_retains_generic_none_behavior(self):
        head = self.repo.commit({}, "unchanged source")
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assertEqual(sc.check(self.repo.root, self.base, head), ())



if __name__ == "__main__":
    unittest.main()
