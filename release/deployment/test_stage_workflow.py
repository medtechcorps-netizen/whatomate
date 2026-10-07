"""Evaluate the actual workflow expressions and credential/freshness boundaries."""

import ast
import json
import itertools
import re
import subprocess
import sys
import unittest

from test_ship_workflow import DOC, JOBS, SOURCE, step, steps


def condition(job, mode, *, results=None, cancelled=False, ref="refs/heads/main"):
    expression = JOBS[job]["if"].removeprefix("${{").removesuffix("}}").strip()
    values = {"inputs.mode": mode, "github.ref": ref}
    values.update({f"needs.{name}.result": result for name, result in (results or {}).items()})
    symbols = {}
    def reference(match):
        key = "value_" + str(len(symbols))
        symbols[key] = values.get(match.group(0), "")
        return key
    expression = re.sub(r"\b(?:inputs|github|needs)\.[A-Za-z0-9_.-]+", reference, expression)
    expression = expression.replace("&&", " and ").replace("||", " or ")
    expression = re.sub(r"!(?!=)", "not ", expression).strip()
    tree = ast.parse(expression, mode="eval")
    allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.UnaryOp, ast.Not,
               ast.Compare, ast.Eq, ast.NotEq, ast.Call, ast.Name, ast.Load, ast.Constant)
    if any(not isinstance(node, allowed) for node in ast.walk(tree)):
        raise AssertionError("workflow condition escaped the reviewed expression grammar")
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and (not isinstance(node.func, ast.Name) or node.func.id not in {"contains", "fromJSON", "cancelled"}):
            raise AssertionError("unexpected condition function")
    symbols.update(contains=lambda values, value: value in values, fromJSON=json.loads, cancelled=lambda: cancelled)
    return bool(eval(compile(tree, "workflow-condition", "eval"), {"__builtins__": {}}, symbols))


class StageWorkflowTests(unittest.TestCase):
    def test_mode_truth_table_and_unknown_mode_fail_closed(self):
        for mode in ("dry-run", "promote", "rollback", "stage", "promote-without-staging", "unknown", ""):
            results = {name: "success" for name in JOBS}
            if mode in {"dry-run", "rollback", "promote-without-staging"}:
                results.update({"deploy-staging": "skipped", "e2e-staging": "skipped"})
            if mode == "rollback": results["attest"] = "skipped"
            expected = {
                "images": mode in {"dry-run", "promote", "stage", "promote-without-staging"},
                "attest": mode in {"dry-run", "promote", "stage", "promote-without-staging"},
                "production": mode in {"dry-run", "promote", "rollback", "promote-without-staging"},
                "record": mode in {"promote", "rollback", "promote-without-staging"},
                "deploy-staging": mode in {"stage", "promote"}, "e2e-staging": mode in {"stage", "promote"}, "staging-rollback": False,
            }
            for job, allowed in expected.items():
                with self.subTest(mode=mode, job=job):
                    self.assertEqual(condition(job, mode, results=results), allowed)
        self.assertNotRegex(SOURCE, r"inputs\.mode\s*!=")

    def test_dependency_failure_cancellation_and_rollback_conditions(self):
        results = {name: "success" for name in JOBS}
        for status in ("failure", "cancelled", "skipped", "success"):
            results["e2e-staging"] = status
            self.assertEqual(condition("staging-rollback", "stage", results=results), status in {"failure", "cancelled"})
            self.assertEqual(condition("staging-rollback", "promote", results=results), status in {"failure", "cancelled"})
            self.assertFalse(condition("staging-rollback", "promote-without-staging", results=results))
            self.assertFalse(condition("staging-rollback", "stage", results=results, cancelled=True))
        results["e2e-staging"] = "failure"
        for status in ("failure", "cancelled", "skipped"):
            results["deploy-staging"] = status
            self.assertFalse(condition("staging-rollback", "stage", results=results))
            self.assertFalse(condition("e2e-staging", "stage", results=results))
        results = {name: "success" for name in JOBS}
        for job, mode in (("production", "promote"), ("deploy-staging", "stage")):
            self.assertFalse(condition(job, mode, results=results, ref="refs/heads/other"))
            self.assertFalse(condition(job, mode, results={**results, "plan": "failure"}))
            self.assertFalse(condition(job, mode, results={**results, "attest": "failure"}))

    def test_production_result_combinations_never_bypass_staging_by_accident(self):
        names = ("plan", "attest", "deploy-staging", "e2e-staging")
        for mode in ("dry-run", "promote", "rollback", "stage", "promote-without-staging", "unknown"):
            for values in itertools.product(("success", "skipped", "failure", "cancelled"), repeat=4):
                plan, attest, deploy, e2e = values
                expected = plan == "success" and (
                    (mode == "promote" and attest == deploy == e2e == "success") or
                    (mode in {"dry-run", "promote-without-staging"} and attest == "success" and deploy == e2e == "skipped") or
                    (mode == "rollback" and attest == deploy == e2e == "skipped"))
                with self.subTest(mode=mode, results=values):
                    self.assertEqual(condition("production", mode, results=dict(zip(names, values))), expected)

    def test_drill_input_and_step_scopes_are_exact(self):
        self.assertEqual(JOBS["e2e-staging"]["container"], {
            "image": "mcr.microsoft.com/playwright:v1.57.0-noble@sha256:8fb7af3bb488c51364d6554876a8eddf377736608327dbdf4177b4901faf7bc9"})
        drill = DOC["on"]["workflow_dispatch"]["inputs"]["drill"]
        self.assertEqual({key: drill[key] for key in ("required", "type", "default", "options")},
                         {"required": True, "type": "choice", "default": "none", "options": ["none", "e2e-fail", "health-fail", "bad-image"]})
        expected_secrets = {"STAGING_ORIGIN", "STAGING_CANARY_FIXTURE_JSON", "STAGING_STUB_CONTROL_KEY"}
        allowed_steps = {"Mask staging fixture before all other steps", "Import fixture and run staging CRM checks"}
        self.assertNotIn("env", JOBS["e2e-staging"])
        self.assertEqual(steps("e2e-staging")[0]["name"], "Mask staging fixture before all other steps")
        for item in steps("e2e-staging"):
            secrets = {key for key, value in item.get("env", {}).items() if "secrets." in str(value)}
            self.assertEqual(secrets, expected_secrets if item["name"] in allowed_steps else set())
            self.assertFalse(any(key in item.get("env", {}) for key in ("GH_TOKEN", "SHIP_DO_TOKEN", "STAGING_DO_TOKEN", "STAGING_TARGET_JSON")))
            self.assertNotIn("upload-artifact", item.get("uses", ""))
        for job in ("deploy-staging", "staging-rollback"):
            self.assertFalse(any("uses" in item for item in steps(job)))
            secret_steps = [item for item in steps(job) if any("secrets." in str(v) for v in item.get("env", {}).values())]
            self.assertEqual(len(secret_steps), 1)
            self.assertEqual(secret_steps[0]["env"]["SHIP_MODE"], "stage")
            self.assertEqual({key for key, value in secret_steps[0]["env"].items() if "secrets." in str(value)}, {"STAGING_DO_TOKEN", "STAGING_TARGET_JSON"})

    def test_fixture_and_report_freshness_precede_browser_requests(self):
        run = step("e2e-staging", "Import fixture and run staging CRM checks")["run"]
        self.assertLess(run.index("import-fixture"), run.index("unset STAGING_ORIGIN"))
        self.assertLess(run.index("unset STAGING_ORIGIN"), run.index("npm --prefix frontend run test:canary"))
        self.assertIn('[[ ! -e "$CANARY_REPORT_FILE" && ! -L "$CANARY_REPORT_FILE" ]]', run)
        self.assertLess(run.index('[[ ! -e "$CANARY_REPORT_FILE"'), run.index("npm --prefix"))
        report = step("e2e-staging", "Verify the bound staging report")
        self.assertNotIn("if", report)  # default success() prevents rebinding a stale report after a failed test
        self.assertLess(steps("e2e-staging").index(step("e2e-staging", "Import fixture and run staging CRM checks")), steps("e2e-staging").index(report))
        self.assertFalse(any(key.startswith("STAGING_") for key in report["env"]))
        self.assertIn('"$RUNNER_TEMP/rereply-staging/canary.json"', report["run"])
        self.assertIn('"$RUNNER_TEMP/rereply-staging/report.json"', report["run"])
        install = step("e2e-staging", "Install frontend dependencies without lifecycle scripts")
        self.assertEqual(install["run"], "npm ci --ignore-scripts")
        self.assertNotIn("env", install)

    def test_first_mask_executes_and_escapes_nested_secrets_before_checkout(self):
        body = steps("e2e-staging")[0]["run"]
        code = body.split("<<'PY'\n", 1)[1].split("\nPY", 1)[0]
        secret = "synthetic%value\r\n::error::do-not-interpret"
        env = {"STAGING_ORIGIN": "https://synthetic.invalid", "STAGING_STUB_CONTROL_KEY": "synthetic-control-key",
               "STAGING_CANARY_FIXTURE_JSON": json.dumps({"nested": [{"password": secret}]})}
        result = subprocess.run([sys.executable, "-I", "-S", "-B", "-c", code], env=env,
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("::add-mask::synthetic%25value%0D%0A::error::do-not-interpret", result.stdout)
        self.assertTrue(all(line.startswith("::add-mask::") for line in result.stdout.splitlines()))


if __name__ == "__main__": unittest.main()
