import base64
import copy
import contextlib
import io
import json
from pathlib import Path
import re
import sys
import tempfile
import unittest
from unittest import mock

import ship_common as common
import stage_contract as contract
import stage_fixture as bridge
import stage_report
from test_stage_contract import data, receipt, APP, ORIGIN
from test_stage_fixture import fixture, CONTROL


def passing_report():
    return {"errors": [], "specs": [{"title": name, "tests": [{"expectedStatus": "passed", "status": "expected", "results": [{"status": "passed", "retry": 0}]}]} for name in reversed(stage_report.CHECKS)]}


class ReportTests(unittest.TestCase):
    def setUp(self):
        production, _, pins, _ = data()
        value = receipt(); raw = contract.receipt_bytes(value)
        self.args = dict(receipt_b64=base64.b64encode(raw).decode(), receipt_sha256=common.sha256_bytes(raw),
            run_id=value["run_id"], candidate_sha256=value["candidate_sha256"], app_id_sha256=pins["app_id_sha256"],
            production_origin_sha256=production["default_ingress_sha256"])
        self.saved = bridge.import_fixture(json.dumps(fixture()), origin=ORIGIN, control_key=CONTROL, **self.args)

    def test_inventory_matches_pr7_and_output_is_canonical_public_binding(self):
        source = (contract.ROOT / "frontend/e2e/canary/checks.ts").read_text()
        inventory = re.search(r"UI_CHECKS = Object.freeze\((\[[\s\S]*?\])\)", source).group(1)
        self.assertEqual(json.loads(re.sub(r",\s*]", "]", inventory)), list(stage_report.CHECKS))
        report = stage_report.bound_report(passing_report(), self.saved, **self.args)
        self.assertEqual(report["passed"], 13)
        self.assertEqual(report["checks"], list(stage_report.CHECKS))
        self.assertEqual(report["receipt_sha256"], self.args["receipt_sha256"])
        raw = common.canonical_file_bytes(report)
        for private in (APP, ORIGIN, CONTROL, "synthetic-klinik-password", "synthetic-stub-app-secret"):
            self.assertNotIn(private.encode(), raw)

    def test_missing_duplicate_failed_retried_skipped_extra_data_refused(self):
        changes = (
            lambda r: r["specs"].pop(), lambda r: r["specs"].__setitem__(0, r["specs"][1]),
            lambda r: r["specs"][0]["tests"][0]["results"][0].update(status="failed"),
            lambda r: r["specs"][0]["tests"][0]["results"][0].update(status="skipped"),
            lambda r: r["specs"][0]["tests"][0]["results"][0].update(retry=1),
            lambda r: r["specs"][0]["tests"][0]["results"][0].update(retry=False),
            lambda r: r["specs"][0]["tests"][0].update(expectedStatus="failed"),
            lambda r: r["specs"][0]["tests"][0].update(status="flaky"),
            lambda r: r["specs"][0].update(error="synthetic-secret-error"),
            lambda r: r["errors"].append("synthetic-secret-global-error"),
        )
        for change in changes:
            report = passing_report(); change(report)
            with self.subTest(change=change), self.assertRaises(common.ReleaseError) as error:
                stage_report.bound_report(report, self.saved, **self.args)
            self.assertNotIn("synthetic-secret", str(error.exception))

    def test_any_drill_is_ineligible_even_when_all_thirteen_pass(self):
        for drill in ("e2e-fail", "health-fail", "bad-image", "invalid"):
            with self.subTest(drill=drill), self.assertRaises(common.ReleaseError):
                stage_report.bound_report(passing_report(), self.saved, **self.args, drill=drill)
        raw = contract.receipt_bytes(receipt(drill="e2e-fail"))
        with self.assertRaises(common.ReleaseError):
            stage_report.bound_report(passing_report(), self.saved, **{**self.args, "receipt_b64": base64.b64encode(raw).decode(), "receipt_sha256": common.sha256_bytes(raw)})

    def test_report_cannot_rebind_to_foreign_run_origin_or_owner_state(self):
        for change in ({"run_id": "7654321"}, {"candidate_sha256": "0" * 64}, {"app_id_sha256": "0" * 64}):
            with self.subTest(change=change), self.assertRaises(common.ReleaseError):
                stage_report.bound_report(passing_report(), self.saved, **{**self.args, **change})
        for key in ("database_private", "secrets", "admin_password"):
            saved = copy.deepcopy(self.saved); saved[key] = "synthetic-private-owner-value"
            with self.assertRaises(common.ReleaseError): stage_report.bound_report(passing_report(), saved, **self.args)

    def test_cli_emits_only_bound_public_report_and_keeps_private_files_unchanged(self):
        sys.path.insert(0, str(contract.ROOT / "release/staging"))
        import setup
        production, _, pins, _ = data()
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder) / "rereply-staging"
            setup.private_directory(directory, setup.Runner())
            state, report = directory / "canary.json", directory / "canary-report.json"
            setup.save_private(state, self.saved)
            setup.save_private(report, passing_report())
            originals = state.read_bytes(), report.read_bytes()
            output = directory / "outputs.txt"
            env = {"GITHUB_OUTPUT": str(output), "GITHUB_RUN_ID": self.args["run_id"], "STAGE_CANDIDATE_SHA256": self.args["candidate_sha256"],
                "STAGE_RECEIPT_B64": self.args["receipt_b64"], "STAGE_RECEIPT_SHA256": self.args["receipt_sha256"]}
            def load(path, code): return production if Path(path).name == "ship-target.json" else pins
            logs = io.StringIO()
            with mock.patch.object(common, "load_json", side_effect=load), contextlib.redirect_stdout(logs):
                self.assertEqual(stage_report.main(["--private-file", str(state), "--report", str(report)], env), 0)
            outputs = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(outputs["passed"], "true")
            raw = base64.b64decode(outputs["report_b64"])
            self.assertEqual(common.sha256_bytes(raw), outputs["report_sha256"])
            self.assertEqual(json.loads(raw)["checks"], list(stage_report.CHECKS))
            self.assertEqual((state.read_bytes(), report.read_bytes()), originals)
            for private in (str(state), str(report), APP, ORIGIN, CONTROL, "synthetic-klinik-password"):
                self.assertNotIn(private, logs.getvalue() + output.read_text())

    def test_cli_refuses_secrets_and_failure_without_success_output(self):
        logs = io.StringIO()
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "outputs.txt"
            env = {"STAGING_CANARY_FIXTURE_JSON": "synthetic-secret-marker", "GITHUB_OUTPUT": str(output)}
            with contextlib.redirect_stdout(logs):
                self.assertEqual(stage_report.main(["--private-file", "missing", "--report", "missing-too"], env), 1)
            self.assertFalse(output.exists())
            self.assertEqual(logs.getvalue(), "stage-report: refused\n")


if __name__ == "__main__": unittest.main()
