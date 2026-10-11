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
        self.assertEqual(self.added(row("functions", "fn(" + H + ")"))[:2], ("expand", set()))

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


if __name__ == "__main__":
    unittest.main()
