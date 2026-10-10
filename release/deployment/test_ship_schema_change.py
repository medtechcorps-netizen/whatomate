from __future__ import annotations

import dataclasses
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import schema_change
import ship_common as common
import test_ship_support as support


MAIN = (
    "package main\n\n"
    "func runRLSMigration(args []string) {\n\tprintln(1)\n}\n\n"
    "func verifyRLSMigrationRuntime(cfg *Config) error {\n\treturn nil\n}\n\n"
    "func other() {\n\tprintln(2)\n}\n"
)
LEGACY = "package channel\n\nfunc BackfillLegacyWhatsAppInbox(\n\tctx Context,\n) error {\n\treturn nil\n}\n"


class SchemaGuardTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.base = self.repo.commit({
            "cmd/whatomate/main.go": MAIN,
            "internal/channel/legacy_meta.go": LEGACY,
            "internal/database/postgres.go": "package database\n",
            "internal/models/models.go": "package models\n",
            "internal/handlers/chatbot_flow_migration.go": "package handlers\n",
            "internal/handlers/messages.go": "package handlers\n\nfunc send() {}\n",
            "pkg/util/util.go": "package util\n",
            "README.md": "x\n",
        }, "base")

    def tearDown(self) -> None:
        self.temp.cleanup()

    def reasons(self, files: dict) -> tuple[str, ...]:
        head = self.repo.commit(files)
        return schema_change.check(self.repo.root, self.base, head)

    def test_same_commit_is_clean(self) -> None:
        self.assertEqual(schema_change.check(self.repo.root, self.base, self.base), ())

    def test_unrelated_changes_are_clean(self) -> None:
        self.assertEqual(self.reasons({
            "README.md": "y\n",
            "docs/x.md": "doc\n",
            "cmd/whatomate/main.go": MAIN.replace("println(2)", "println(3)"),
            "internal/handlers/messages.go": "package handlers\n\nfunc send() { db.Migrator().HasTable(x) }\n",
        }), ())

    def test_r1_guarded_trees_including_non_go_files(self) -> None:
        self.assertEqual(self.reasons({"internal/models/new_model.go": "package models\n"}),
                         ("guarded-tree:internal/models/new_model.go",))
        self.repo.checkout(self.base)
        self.assertEqual(self.reasons({"internal/database/testdata/seed.sql": "select 1;\n"}),
                         ("guarded-tree:internal/database/testdata/seed.sql",))

    def test_r1_test_files_never_reach_the_binary(self) -> None:
        self.assertEqual(self.reasons({"internal/database/postgres_test.go": "package database\n"}), ())

    def test_r1_deletion_and_rename_out_of_a_guarded_tree_trip(self) -> None:
        self.assertEqual(self.reasons({"internal/models/models.go": None}), ("guarded-tree:internal/models/models.go",))
        self.repo.checkout(self.base)
        reasons = self.reasons({"internal/database/postgres.go": None, "internal/other/postgres.go": "package database\n"})
        self.assertIn("guarded-tree:internal/database/postgres.go", reasons)

    def test_r2_guarded_file(self) -> None:
        self.assertEqual(self.reasons({"internal/handlers/chatbot_flow_migration.go": "package handlers\n// x\n"}),
                         ("guarded-file:internal/handlers/chatbot_flow_migration.go",))

    def test_r3_guarded_functions_change_appear_and_disappear(self) -> None:
        changed = MAIN.replace("println(1)", "println(9)")
        self.assertEqual(self.reasons({"cmd/whatomate/main.go": changed}),
                         ("guarded-function:cmd/whatomate/main.go#runRLSMigration",))
        self.repo.checkout(self.base)
        added = MAIN + "\nfunc validateServerMigrationMode(cfg *Config, migrate bool) error {\n\treturn nil\n}\n"
        self.assertEqual(self.reasons({"cmd/whatomate/main.go": added}),
                         ("guarded-function:cmd/whatomate/main.go#validateServerMigrationMode",))
        self.repo.checkout(self.base)
        removed = MAIN.replace("func verifyRLSMigrationRuntime(cfg *Config) error {\n\treturn nil\n}\n\n", "")
        self.assertEqual(self.reasons({"cmd/whatomate/main.go": removed}),
                         ("guarded-function:cmd/whatomate/main.go#verifyRLSMigrationRuntime",))
        self.repo.checkout(self.base)
        self.assertEqual(self.reasons({"internal/channel/legacy_meta.go": LEGACY.replace("return nil", "return err")}),
                         ("guarded-function:internal/channel/legacy_meta.go#BackfillLegacyWhatsAppInbox",))

    def test_r4_migration_calls(self) -> None:
        calls = (
            "db.AutoMigrate(&models.X{})",
            "database.RunMigrationWithProgressContext(ctx)",
            "database.RunRLSMigrationCoordinator(ctx)",
            "ApplyTenantRLS(db)",
            "RemoveTenantRLS(db)",
            "SeedPermissionsAndRoles(db)",
            "FixSystemRolePermissions(db)",
            "CreateDefaultAdmin(db)",
            "CreateIndexes(db)",
            "BackfillContacts(ctx)",
            "db.Migrator().CreateTable(&X{})",
            "db.Migrator().DropColumn(&X{}, \"y\")",
            'db.Exec("ALTER TABLE contacts ADD COLUMN x int")',
            "db.Raw(`  create index y on z(a)`)",
            'db.Exec("comment on table x is \'y\'")',
            'db.Exec("GRANT SELECT ON x TO y")',
        )
        for call in calls:
            self.repo.checkout(self.base)
            with self.subTest(call=call):
                self.assertEqual(
                    self.reasons({"internal/handlers/messages.go": f"package handlers\n\nfunc send() {{ {call} }}\n"}),
                    ("migration-call:internal/handlers/messages.go",),
                )
        self.repo.checkout(self.base)
        self.assertEqual(self.reasons({"pkg/util/new.go": "package util\nfunc f() { db.AutoMigrate(x) }\n"}),
                         ("migration-call:pkg/util/new.go",))

    def test_r4_ignores_tests_reads_and_removed_lines(self) -> None:
        self.assertEqual(self.reasons({
            "internal/handlers/messages_test.go": "package handlers\nfunc t() { db.AutoMigrate(x) }\n",
            "internal/handlers/query.go": 'package handlers\nfunc q() { db.Raw("select 1") ; db.Migrator().HasTable(x) }\n',
            "scripts/tool.go": "package main\nfunc main() { db.AutoMigrate(x) }\n",
        }), ())

    def test_revert_to_the_base_tree_unblocks(self) -> None:
        self.repo.commit({"internal/models/models.go": "package models\n// change\n"})
        reverted = self.repo.commit({"internal/models/models.go": "package models\n"})
        self.assertEqual(schema_change.check(self.repo.root, self.base, reverted), ())

    def test_guard_raises_with_sorted_public_reasons(self) -> None:
        head = self.repo.commit({"internal/models/z.go": "package models\n", "internal/models/a.go": "package models\n"})
        with self.assertRaises(schema_change.SchemaChangeBlocked) as caught:
            schema_change.guard(self.repo.root, self.base, head)
        self.assertEqual(str(caught.exception), "schema-change-blocked")
        self.assertEqual(caught.exception.reasons, ("guarded-tree:internal/models/a.go", "guarded-tree:internal/models/z.go"))
        for reason in caught.exception.reasons:
            common.require_public_text(reason)

    def test_unknown_commits_fail_closed(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^git-failed:schema-guard$"):
            schema_change.check(self.repo.root, self.base, "0" * 40)


class FunctionTextTests(unittest.TestCase):
    def test_extraction(self) -> None:
        self.assertEqual(schema_change.function_text(MAIN, "runRLSMigration"),
                         ("func runRLSMigration(args []string) {\n\tprintln(1)\n}",))
        self.assertEqual(schema_change.function_text(MAIN, "missing"), ())
        self.assertEqual(schema_change.function_text(None, "runRLSMigration"), ())


# Fixtures describe only synthetic public source, and create local Git histories.
# They never import application code, execute Go callbacks or connect to a DB.
FIXTURES = Path(__file__).resolve().parent / "fixtures" / "classifier"
REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
GOLDEN = "internal/dbcatalog/golden/"
REGISTRY = "internal/dbcatalog/registry/data_steps.json"


def fixture_files(files: dict[str, str | None]) -> dict[str, str | None]:
    output = {}
    for path, value in files.items():
        if path.startswith("/") or ".." in Path(path).parts:
            raise AssertionError("fixture path escapes synthetic repository")
        baseline_placeholders = {"@released-baseline-v0-catalog@": "catalog.json",
                                 "@released-baseline-v0-history@": "history.json",
                                 "@released-baseline-v0-globals@": "global_tables.json"}
        if value in baseline_placeholders:
            value = (REPOSITORY_ROOT / (GOLDEN + baseline_placeholders[value])).read_text(encoding="utf-8")
        output[path] = value
    return output


class ClassifierHistoryFixtures(unittest.TestCase):
    def run_fixture(self, case: dict) -> None:
        self.assertEqual(set(case), {"name", "base_files", "commits", "mode", "expected"})
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            refs = {"base": repo.commit(fixture_files(case["base_files"]), "synthetic base")}
            head = refs["base"]
            for index, change in enumerate(case["commits"]):
                if "parent" in change:
                    repo.checkout(refs[change["parent"]])
                if "merge" in change:
                    repo.git("merge", "--no-ff", "--no-edit", refs[change["merge"]])
                    head = repo.head()
                else:
                    head = repo.commit(fixture_files(change["files"]), change.get("ref", str(index)))
                if "ref" in change:
                    self.assertNotIn(change["ref"], refs)
                    refs[change["ref"]] = head
            expected = case["expected"]
            if expected.get("error"):
                with self.assertRaises(common.ReleaseError):
                    schema_change.classify(repo.root, refs["base"], head, mode=case["mode"])
                return
            result = schema_change.classify(repo.root, refs["base"], head, mode=case["mode"])
            self.assertIsInstance(result.reasons, tuple)
            self.assertIsInstance(result.summary, tuple)
            self.assertEqual(bool(result.reasons), expected.get("blocked", False), result)
            for key in ("kind", "dormant"):
                if key in expected:
                    self.assertEqual(getattr(result, key), expected[key], result)
            for prefix in expected.get("reason_prefixes", []):
                self.assertTrue(any(reason.startswith(prefix) for reason in result.reasons), result)
            for value in expected.get("summary_contains", []):
                self.assertIn(value, "\n".join(result.summary))
            for line in (*result.reasons, *result.summary):
                common.require_public_text(line)
            with self.assertRaises((dataclasses.FrozenInstanceError, AttributeError)):
                result.kind = "data"


def _history_method(case: dict):
    def test(self):
        self.run_fixture(case)
    test.__doc__ = case["name"]
    return test


for _path in sorted(FIXTURES.glob("*.json")):
    _case = json.loads(_path.read_text(encoding="utf-8"))
    setattr(ClassifierHistoryFixtures, "test_history_" + _path.stem.replace("-", "_"), _history_method(_case))


class ClassifierFixtureContractTests(unittest.TestCase):
    def test_fixture_inventory_is_nonempty_and_unique(self) -> None:
        cases = [json.loads(path.read_text(encoding="utf-8")) for path in FIXTURES.glob("*.json")]
        self.assertGreaterEqual(len(cases), 26)
        self.assertEqual(len({case["name"] for case in cases}), len(cases))

    def test_check_preserves_reason_tuple_compatibility(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            base = repo.commit({"README.md": "base\n"})
            for files in ({"README.md": "next\n"}, {"internal/models/new.go": "package models\n"}):
                head = repo.commit(files)
                verdict = schema_change.classify(repo.root, base, head)
                reasons = schema_change.check(repo.root, base, head)
                self.assertIsInstance(reasons, tuple)
                self.assertEqual(reasons, verdict.reasons)
                self.assertEqual(bool(reasons), bool(verdict.reasons))

    def test_actual_frozen_catalog_and_registry_artifacts_parse(self) -> None:
        # The real golden has optional parents, and PR5 has five inert child rows.
        # These bytes must pass strict shape validation without normalizing them.
        artifacts = {path: (REPOSITORY_ROOT / path).read_text(encoding="utf-8")
                     for path in (GOLDEN + "catalog.json", GOLDEN + "history.json",
                                  GOLDEN + "global_tables.json", GOLDEN + "seeds.json", REGISTRY)}
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            base = repo.commit(artifacts)
            head = repo.commit({"README.md": "unrelated\n"})
            result = schema_change.classify(repo.root, base, head)
            self.assertEqual(result.reasons, ())
            self.assertEqual(result.kind, "none")

    def test_actual_catalog_to_registry_introduction_includes_all_seed_lines(self) -> None:
        # Copy frozen public source into a disposable Git history. This exercises
        # actual 21-entry closure lists and all 1187 seed lines, not a tiny proxy.
        registry = json.loads((REPOSITORY_ROOT / REGISTRY).read_text(encoding="utf-8"))
        migration_path = "internal/dbcatalog/registry/migration_path.json"
        migration = json.loads((REPOSITORY_ROOT / migration_path).read_text(encoding="utf-8"))
        sources = sorted({path for step in registry for path in step["closure_files"]}
                         | set(migration["closure_files"]))
        base_files = {path: (REPOSITORY_ROOT / path).read_text(encoding="utf-8") for path in sources}
        for name in ("catalog.json", "history.json", "global_tables.json"):
            base_files[GOLDEN + name] = (REPOSITORY_ROOT / (GOLDEN + name)).read_text(encoding="utf-8")
        additions = {path: (REPOSITORY_ROOT / path).read_text(encoding="utf-8")
                     for path in (REGISTRY, GOLDEN + "seeds.json", migration_path,
                                  "internal/dbcatalog/registry/coordinator_bindings.json",
                                  "internal/dbcatalog/registry/closure_caps.json")}
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            base = repo.commit(base_files, "actual catalog source before registry")
            head = repo.commit(additions, "actual registry and migration contracts")
            result = schema_change.classify(repo.root, base, head)
            self.assertEqual(result.reasons, ())
            self.assertEqual((result.kind, result.dormant), ("data", True))
            seed_lines = additions[GOLDEN + "seeds.json"].splitlines()
            self.assertGreater(len(seed_lines), 512)
            self.assertEqual(sum(line.startswith("seeds diff +") for line in result.summary), len(seed_lines))
            completed = subprocess.run(
                [sys.executable, "-I", "-S", "-B", str(Path(schema_change.__file__)),
                 "preview", "--base", base, "--head", head],
                cwd=repo.root, env=support.git_env(), capture_output=True,
                text=True, timeout=180, check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertGreater(len(completed.stdout.encode("utf-8")), 8192)
            preview = json.loads(completed.stdout)
            self.assertEqual(preview, {"kind": result.kind, "dormant": result.dormant,
                                       "reasons": list(result.reasons), "summary": list(result.summary)})
            # Whole-file introduction also binds an unrelated comment: no hash
            # regeneration or same-file method narrowing can launder this edit.
            repo.checkout(base)
            changed_path = registry[0]["closure_files"][0]
            bad = repo.commit({**additions, changed_path: base_files[changed_path] + "\n// bundled edit\n"})
            result = schema_change.classify(repo.root, base, bad)
            self.assertIn("registry-introduction-changed:" + changed_path, result.reasons)

    def test_non_ancestral_forward_history_refuses(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            ancestor = repo.commit({"README.md": "common\n",
                                    GOLDEN + "catalog.json": '{"version":0,"objects":[]}\n',
                                    GOLDEN + "history.json": '[{"version":0,"class":"baseline","min_reader_version":0}]\n',
                                    GOLDEN + "global_tables.json": "[]\n"})
            base = repo.commit({"README.md": "left\n"})
            repo.checkout(ancestor)
            head = repo.commit({"README.md": "right\n"})
            result = schema_change.classify(repo.root, base, head)
            self.assertTrue(result.reasons)
            self.assertTrue(any(reason.startswith("classifier-invalid:history-range")
                                for reason in result.reasons))

    def test_preview_agrees_with_library_without_provider_imports(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = support.TempRepo(Path(directory) / "repo")
            base = repo.commit({"README.md": "base\n"})
            head = repo.commit({"README.md": "next\n"})
            result = schema_change.classify(repo.root, base, head)
            command = [sys.executable, "-I", "-S", "-B", str(Path(schema_change.__file__)),
                       "preview", "--base", base, "--head", head]
            completed = subprocess.run(command, cwd=repo.root, env=support.git_env(),
                                       capture_output=True, text=True, timeout=30, check=False)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertIn(result.kind, completed.stdout)
            self.assertNotIn("Traceback", completed.stdout + completed.stderr)


if __name__ == "__main__":
    unittest.main()
