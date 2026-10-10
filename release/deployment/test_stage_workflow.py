"""Evaluate the actual workflow expressions and credential/freshness boundaries."""

import ast
import base64
import copy
import hashlib
import json
import itertools
import re
import subprocess
import sys
import unittest

from test_ship_workflow import DOC, JOBS, SOURCE, step, steps


def fixture_mask(value, *, raw=None, origin="https://synthetic.invalid", legacy=False):
    body = steps("e2e-staging")[0]["run"]
    code = body.split("<<'PY'\n", 1)[1].split("\nPY", 1)[0]
    if legacy:
        # Reproduce the old recursive mask set without changing the fixture.
        code = code.replace("mask(fixture['canary'])", "mask(fixture)")
    env = {"STAGING_ORIGIN": origin, "STAGING_STUB_CONTROL_KEY": "synthetic-control-key",
           "STAGING_CANARY_FIXTURE_JSON": raw if raw is not None else json.dumps(value)}
    return subprocess.run([sys.executable, "-I", "-S", "-B", "-c", code], env=env,
                          capture_output=True, text=True, timeout=10)


def mask_values(result):
    values = []
    for line in result.stdout.splitlines():
        if not line.startswith("::add-mask::"):
            raise AssertionError("unexpected masker output")
        values.append(line.removeprefix("::add-mask::").replace("%0D", "\r").replace("%0A", "\n").replace("%25", "%"))
    return values


def runner_base64(value, shift=0):
    # Literal UTF-8 byte-offset behavior of Base64StringEscape/Shift1/Shift2:
    # https://github.com/actions/runner/blob/main/src/Sdk/DTLogging/Logging/ValueEncoders.cs
    raw = value.encode("utf-8")
    return base64.b64encode(raw[shift:] if len(raw) > shift else raw).decode("ascii")


def runner_output_matches(output, values):
    # JobExtension skips a job output if MaskSecrets changes it. These are the
    # raw and Base64 encoders involved here, not a complete runner emulator.
    # https://github.com/actions/runner/blob/main/src/Runner.Worker/JobExtension.cs
    return {encoded for value in values for encoded in (value, *(runner_base64(value, n) for n in range(3)))
            if encoded and encoded in output}


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
        secret = "synthetic%value\r\n::error::do-not-interpret"
        origin = "https://synthetic.invalid"
        value = {"schema_version": 1, "origin_sha256": hashlib.sha256(origin.encode()).hexdigest(),
                 "canary": {"origin": origin, "nested": [{"password": secret}]}}
        result = fixture_mask(value)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("::add-mask::synthetic%25value%0D%0A::error::do-not-interpret", result.stdout)
        self.assertEqual(mask_values(result), [origin, json.dumps(value), "synthetic-control-key", origin, secret])

    def test_public_checksum_exception_is_only_top_level(self):
        origin = "https://synthetic.invalid"
        digest = hashlib.sha256(origin.encode()).hexdigest()
        value = {"schema_version": 1, "origin_sha256": digest, "canary": {
            "origin": origin, "password": "a" * 64, "nested": [{"origin_sha256": digest}]}}
        result = fixture_mask(value)
        self.assertEqual(result.returncode, 0, result.stderr)
        # Even the same public checksum is masked if used as a nested private leaf.
        self.assertEqual(mask_values(result), [origin, json.dumps(value), "synthetic-control-key", origin, "a" * 64, digest])

    def test_masker_rejects_unbound_or_malformed_top_level_metadata(self):
        origin = "https://synthetic.invalid"
        value = {"schema_version": 1, "origin_sha256": hashlib.sha256(origin.encode()).hexdigest(), "canary": {"origin": origin}}
        changes = [lambda v: v.pop("schema_version"), lambda v: v.pop("origin_sha256"), lambda v: v.pop("canary"),
                   lambda v: v.update(extra="synthetic-extra"), lambda v: v.update(schema_version=True),
                   lambda v: v.update(schema_version=2), lambda v: v.update(schema_version=1.0),
                   lambda v: v.update(origin_sha256="f" * 64), lambda v: v.update(origin_sha256=v["origin_sha256"].upper()),
                   lambda v: v.update(origin_sha256="f" * 63), lambda v: v.update(origin_sha256=1),
                   lambda v: v.update(canary=[]), lambda v: v["canary"].update(origin="https://foreign.invalid"),
                   lambda v: v["canary"].pop("origin")]
        for change in changes:
            altered = copy.deepcopy(value); change(altered)
            with self.subTest(value=altered):
                result = fixture_mask(altered)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stderr, "stage-fixture: refused\n")
                # Actual secrets are registered before parsing; no exception detail leaks.
                self.assertEqual(mask_values(result), [origin, json.dumps(altered), "synthetic-control-key"])
        for raw in ("not-json", "[]", "null", json.dumps(value) + " trailing",
                    json.dumps(value).replace('"schema_version": 1', '"schema_version": 1, "schema_version": 1'),
                    json.dumps(value).replace('"canary": {', '"canary": {"origin": "hidden-secret",'),
                    json.dumps(value).replace('"canary": {', '"canary": {"extra": NaN,'),
                    json.dumps(value).replace('"canary": {', '"canary": {"extra": Infinity,')):
            with self.subTest(raw=raw):
                result = fixture_mask(None, raw=raw)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stderr, "stage-fixture: refused\n")
                self.assertEqual(mask_values(result), [origin, raw, "synthetic-control-key"])

    def test_runner_base64_encoders_match_byte_offset_vectors(self):
        self.assertEqual([runner_base64("abcdef", n) for n in range(3)], ["YWJjZGVm", "YmNkZWY=", "Y2RlZg=="])
        self.assertEqual(runner_base64("a", 2), "YQ==")
        self.assertEqual(runner_base64("\u00e9x", 1), "qXg=")  # UTF-8 bytes, not character slicing.

    def test_canonical_report_and_receipt_survive_masking_without_reencoding(self):
        import ship_common as common
        import stage_contract as contract
        import stage_fixture
        import stage_report
        from test_stage_contract import data, receipt, ORIGIN
        from test_stage_fixture import fixture, CONTROL
        from test_stage_report import passing_report
        production, _, pins, _ = data()
        value = fixture(); raw_fixture = json.dumps(value)
        receipt_value = receipt(); receipt_raw = contract.receipt_bytes(receipt_value)
        args = dict(receipt_b64=base64.b64encode(receipt_raw).decode(), receipt_sha256=common.sha256_bytes(receipt_raw),
                    run_id=receipt_value["run_id"], candidate_sha256=receipt_value["candidate_sha256"],
                    app_id_sha256=pins["app_id_sha256"], production_origin_sha256=production["default_ingress_sha256"])
        saved = stage_fixture.import_fixture(raw_fixture, origin=ORIGIN, control_key=CONTROL, **args)
        report = stage_report.bound_report(passing_report(), saved, **args)
        report_raw = common.canonical_file_bytes(report)
        results = [fixture_mask(value, origin=ORIGIN, legacy=legacy) for legacy in (True, False)]
        for result in results: self.assertEqual(result.returncode, 0, result.stderr)
        old_masks, new_masks = map(mask_values, results)
        self.assertEqual(old_masks, new_masks[:3] + [value["origin_sha256"]] + new_masks[3:])
        for raw in (receipt_raw, report_raw):
            encoded = base64.b64encode(raw).decode()
            with self.subTest(keys=sorted(json.loads(raw))):
                self.assertTrue(runner_output_matches(encoded, old_masks))
                self.assertFalse(runner_output_matches(encoded, new_masks))
                self.assertTrue(runner_output_matches(json.dumps(json.loads(raw)), old_masks))
                self.assertFalse(runner_output_matches(json.dumps(json.loads(raw)), new_masks))
                self.assertEqual(base64.b64decode(encoded, validate=True), raw)
                self.assertEqual(raw, common.canonical_file_bytes(json.loads(raw)))
        self.assertEqual(report["receipt_sha256"], common.sha256_bytes(receipt_raw))
        self.assertEqual(report["origin_sha256"], receipt_value["ingress_sha256"])


if __name__ == "__main__": unittest.main()
