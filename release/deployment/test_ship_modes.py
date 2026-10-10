from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import ship
import ship_common as common
import test_ship_support as support


class ModeBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo, self.base = support.base_repo(self.root / "repo")
        self.head = self.repo.commit({"README.md": "stage candidate\n"})
        self.h = support.Harness(self.root, repo=self.repo, head=self.head, bootstrap_sha=self.base)

    def test_all_production_modes_refuse_staging_inputs_before_any_io(self):
        for mode in ship.PRODUCTION_MODES:
            for extra in ({"SHIP_MODE": "stage"}, {"SHIP_DRILL": "e2e-fail"}, {"SHIP_DRILL": "health-fail"},
                          {"SHIP_DRILL": "bad-image"}, {"STAGING_DO_TOKEN": ""}, {"STAGING_TARGET_JSON": ""},
                          {"STAGING_CANARY_FIXTURE_JSON": "private"}, {"STAGING_FUTURE_SECRET": "private"}):
                with self.subTest(mode=mode, extra=list(extra)):
                    self.h.env = support.context_env(self.head, self.root)
                    self.h.production_env(mode, target_release="prod-0000" if mode == "rollback" else "")
                    self.h.env.update(extra)
                    with mock.patch.object(ship.Context, "ship_target", side_effect=AssertionError("target I/O")) as target:
                        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
                    target.assert_not_called()
                    self.assertEqual(self.h.gh.calls, [])
                    self.assertEqual(self.h.do.requests, [])
                    self.assertEqual(self.h.https.calls, [])
                    self.assertNotIn("SHIP_DO_TOKEN", self.h.env)

    def test_stage_plan_has_promote_gates_without_production_approval(self):
        self.h.env.update(SHIP_MODE="stage", SHIP_DRILL="none")
        self.h.gh.environment.clear()
        self.assertEqual(self.h.run("plan"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.outputs()["latest_release"], "prod-0000")
        self.assertIn("Schema guard: clean", self.h.summary())
        self.assertIn("CI: Test and E2E", self.h.summary())
        self.assertNotIn(ship.APPROVAL_BANNER, self.h.summary())
        self.assertFalse(any("/environments/production" in str(call) for call in self.h.gh.calls))

    def test_stage_plan_refuses_missing_ci_and_schema_changes(self):
        self.h.env.update(SHIP_MODE="stage", SHIP_DRILL="none")
        self.h.gh.ci_runs["test.yml"] = []
        self.assertEqual(self.h.run("plan"), ship.EXIT_REFUSED)
        self.assertIn("ci-not-green", self.h.text())
        head = self.repo.commit({"internal/database/new.go": "package database\n"})
        self.h.env.update(support.context_env(head, self.root))
        self.h.gh.ci_green(head)
        self.assertEqual(self.h.run("plan"), ship.EXIT_REFUSED)
        self.assertIn("schema-change-blocked", self.h.text())

    def test_plan_drills_are_stage_only_before_github_io(self):
        for mode in ship.MODES:
            for drill in ship.DRILLS | {"unknown", ""}:
                with self.subTest(mode=mode, drill=drill):
                    env = {"SHIP_MODE": mode, "SHIP_DRILL": drill,
                           "GITHUB_ACTOR": common.APPROVER_LOGIN,
                           "SHIP_TARGET_RELEASE": "prod-0000" if mode == "rollback" else ""}
                    allowed = drill in ship.DRILLS and (mode == "stage" or drill == "none")
                    if allowed:
                        self.assertEqual(ship.parse_mode(env)[0], mode)
                    else:
                        self.h.env.update(env)
                        self.assertEqual(self.h.run("plan"), ship.EXIT_REFUSED)
                        self.assertEqual(self.h.gh.calls, [])

    def test_forbidden_ambient_addition_is_exact(self):
        self.assertIn("STAGING_DO_TOKEN", common.FORBIDDEN_AMBIENT)
        self.assertIn("STAGING_TARGET_JSON", common.FORBIDDEN_AMBIENT)
        self.assertEqual(ship.PUT_MODES, {"promote", "rollback", ship.BYPASS_MODE})
        self.assertEqual(ship.PRODUCTION_MODES, {"dry-run", "promote", "rollback", ship.BYPASS_MODE})


if __name__ == "__main__":
    unittest.main()
