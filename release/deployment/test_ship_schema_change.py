from __future__ import annotations

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


if __name__ == "__main__":
    unittest.main()
