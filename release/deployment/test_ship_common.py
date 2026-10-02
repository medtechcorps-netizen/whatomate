from __future__ import annotations

import ast
import decimal
import io
import re
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import test_ship_support as support


HERE = Path(__file__).resolve().parent
NEW_MODULES = (
    "ship_common.py", "spec_images.py", "do_app.py", "backup_check.py", "smoke.py",
    "release_record.py", "schema_change.py", "trivy_policy.py", "ship.py",
)


def good_env() -> dict[str, str]:
    sha = "a" * 40
    return {
        "GITHUB_REPOSITORY": common.REPOSITORY,
        "GITHUB_REF": "refs/heads/main",
        "GITHUB_REF_PROTECTED": "true",
        "GITHUB_EVENT_NAME": "workflow_dispatch",
        "GITHUB_WORKFLOW_REF": common.SHIP_WORKFLOW_REF,
        "GITHUB_WORKFLOW_SHA": sha,
        "GITHUB_SHA": sha,
        "RUNNER_ENVIRONMENT": "github-hosted",
        "GITHUB_RUN_ATTEMPT": "1",
        "GITHUB_RUN_ID": "77",
    }


class StrictJsonTests(unittest.TestCase):
    def test_duplicate_keys_are_refused(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^input-invalid"):
            common.loads_strict('{"a":1,"a":2}')

    def test_floats_and_non_finite_numbers_are_refused_by_default(self) -> None:
        for raw in ('{"a":1.5}', '{"a":NaN}', '{"a":Infinity}', '[-Infinity]'):
            with self.subTest(raw=raw), self.assertRaises(common.ReleaseError):
                common.loads_strict(raw)

    def test_decimals_flag_parses_fractions_but_never_nan(self) -> None:
        value = common.loads_strict('{"size":0.0328}', decimals=True)
        self.assertEqual(value["size"], decimal.Decimal("0.0328"))
        with self.assertRaises(common.ReleaseError):
            common.loads_strict('{"size":NaN}', decimals=True)

    def test_invalid_utf8_and_malformed_json_use_the_given_code(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^provider-invalid:json$"):
            common.loads_strict(b"\xff", code="provider-invalid:json")
        with self.assertRaisesRegex(common.ReleaseError, "^provider-invalid:json$"):
            common.loads_strict("{", code="provider-invalid:json")

    def test_canonical_bytes_are_sorted_compact_ascii_with_newline(self) -> None:
        raw = common.canonical_file_bytes({"b": "\u00e9", "a": [1, None, True]})
        self.assertEqual(raw, b'{"a":[1,null,true],"b":"\\u00e9"}\n')
        self.assertEqual(common.sha256_value({"a": 1}), common.sha256_bytes(b'{"a":1}'))

    def test_load_json_requires_canonical_regular_files(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "value.json"
            path.write_bytes(b'{"a": 1}\n')
            with self.assertRaisesRegex(common.ReleaseError, "^target-invalid$"):
                common.load_json(path, "target-invalid")
            self.assertEqual(common.load_json(path, "target-invalid", canonical=False), {"a": 1})
            path.write_bytes(b'{"a":1}\n')
            self.assertEqual(common.load_json(path, "target-invalid"), {"a": 1})
            with self.assertRaises(common.ReleaseError):
                common.load_json(Path(temp) / "missing.json", "target-invalid")

    def test_exact_helpers(self) -> None:
        self.assertEqual(common.exact_keys({"a": 1}, {"a"}, "input-invalid"), {"a": 1})
        for bad in ({"a": 1, "b": 2}, [], None):
            with self.assertRaises(common.ReleaseError):
                common.exact_keys(bad, {"a"}, "input-invalid")
        for bad in ("", "a\nb", "a\x00", 3, "x" * 5000):
            with self.assertRaises(common.ReleaseError):
                common.exact_string(bad, "input-invalid")
        with self.assertRaises(common.ReleaseError):
            common.exact_int(True, "input-invalid")
        with self.assertRaises(common.ReleaseError):
            common.exact_bool(1, "input-invalid")
        common.require_sha1("0" * 40, "input-invalid")
        common.require_digest("sha256:" + "0" * 64, "input-invalid")
        common.require_uuid(support.APP_ID, "input-invalid")
        for bad in ("A" * 40, "0" * 39):
            with self.assertRaises(common.ReleaseError):
                common.require_sha1(bad, "input-invalid")
        with self.assertRaises(common.ReleaseError):
            common.require_uuid("11111111-1111-1111-1111-111111111111", "input-invalid")

    def test_timestamps_are_second_precision_utc(self) -> None:
        parsed = common.require_timestamp("2026-10-02T15:11:28Z", "input-invalid")
        self.assertEqual(common.format_timestamp(parsed), "2026-10-02T15:11:28Z")
        self.assertEqual(common.compact_timestamp("2026-10-02T15:11:28Z"), "20261002T151128Z")
        for bad in ("2026-10-02T15:11:28.1Z", "2026-10-02T15:11:28+00:00", "2026-13-02T15:11:28Z", "x"):
            with self.subTest(bad=bad), self.assertRaises(common.ReleaseError):
                common.require_timestamp(bad, "input-invalid")


class ReasonCodeTests(unittest.TestCase):
    def test_reason_codes_are_constant_and_known(self) -> None:
        self.assertEqual(str(common.ReleaseError("drift")), "drift")
        self.assertEqual(common.ReleaseError("drift:detail").reason, "drift")
        self.assertEqual(str(common.ReleaseError("not-a-known-reason")), "internal-error:unknown-reason-code")
        self.assertEqual(str(common.ReleaseError("drift " + support.APP_ID)), "internal-error:invalid-reason-code")

    def test_every_literal_reason_in_the_new_modules_is_known(self) -> None:
        pattern = common.CODE_RE
        calls = {"fail", "ReleaseError", "AmbiguousMutation", "PostDeployGuard", "TerminalDeployment",
                 "ReconcileTimeout"}
        seen = 0
        for name in NEW_MODULES:
            tree = ast.parse((HERE / name).read_text(encoding="utf-8"))
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                func = node.func
                called = func.attr if isinstance(func, ast.Attribute) else getattr(func, "id", "")
                if called not in calls or not node.args:
                    continue
                first = node.args[0]
                if isinstance(first, ast.Constant) and isinstance(first.value, str):
                    seen += 1
                    with self.subTest(module=name, code=first.value):
                        self.assertRegex(first.value, pattern)
                        self.assertIn(first.value.split(":", 1)[0], common.REASONS)
        self.assertGreater(seen, 100)

    def test_code_keyword_and_assignment_literals_are_known(self) -> None:
        for name in NEW_MODULES:
            source = (HERE / name).read_text(encoding="utf-8")
            for match in re.finditer(r'\bcode(?:\s*=\s*|=)"([a-z0-9-]+)(?::[^"]*)?"', source):
                with self.subTest(module=name, code=match.group(1)):
                    self.assertIn(match.group(1), common.REASONS)


class SanitizeTests(unittest.TestCase):
    def test_uuid_provider_hosts_and_credentials_are_refused(self) -> None:
        marker = "dop_" + "v1_" + "zz"
        for value in (
            {"text": "x " + support.APP_ID},
            {"text": "id 1234abcd-0000-0000-0000-00000000abcd"},
            {"text": "https://api.digitalocean.com/v2"},
            {"text": "rereply-x." + "ONDIGITALOCEAN" + ".APP"},
            {"text": marker},
            {"text": "EV[1:x]"},
            {"text": "-----BEGIN PRIVATE KEY"},
            {"spec": {}},
            {"Token": "x"},
            {"value": 1.5},
        ):
            with self.subTest(value=list(value)), self.assertRaisesRegex(common.ReleaseError, "^output-unsafe"):
                common.sanitize_public(value)

    def test_private_values_are_refused_anywhere(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^output-unsafe"):
            common.sanitize_public({"a": ["prefix-secret-suffix"]}, private=["secret"])
        with self.assertRaisesRegex(common.ReleaseError, "^output-unsafe"):
            common.require_public_text("host secret.example.invalid", ["secret.example.invalid"])

    def test_public_values_pass(self) -> None:
        common.sanitize_public({"web": "sha256:" + "a" * 64, "count": 3, "ok": True, "none": None})
        self.assertEqual(common.require_public_text("prod-20261004T120000Z-0123abcd"), "prod-20261004T120000Z-0123abcd")

    def test_require_public_text_refuses_control_and_non_ascii(self) -> None:
        for text in ("a\nb", "a\tb", "caf\u00e9", 7):
            with self.subTest(text=text), self.assertRaises(common.ReleaseError):
                common.require_public_text(text)


class OutputTests(unittest.TestCase):
    def test_outputs_are_validated_and_appended(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "out"
            output = common.Output(stdout=io.StringIO(), output_path=str(path))
            output.set_outputs({"release": "prod-0000", "empty": ""})
            self.assertEqual(path.read_text(encoding="ascii"), "release=prod-0000\nempty=\n")
            for mapping in ({"x": "a\nb"}, {"x": "a b"}, {"Bad": "a"}, {"x": support.APP_ID}, {"x": 3}):
                with self.subTest(mapping=mapping), self.assertRaisesRegex(common.ReleaseError, "^output-unsafe"):
                    output.set_outputs(mapping)

    def test_add_mask_prints_once_and_then_blocks_the_value(self) -> None:
        stdout = io.StringIO()
        output = common.Output(stdout=stdout)
        output.add_mask("secret-host.example.invalid")
        output.add_mask("secret-host.example.invalid")
        self.assertEqual(stdout.getvalue().count("::add-mask::secret-host.example.invalid\n"), 1)
        with self.assertRaises(common.ReleaseError):
            output.text("probing secret-host.example.invalid")
        with self.assertRaises(common.ReleaseError):
            output.add_mask("a\nb")

    def test_summary_and_emit_are_sanitized(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            summary = Path(temp) / "summary"
            stdout = io.StringIO()
            output = common.Output(stdout=stdout, summary_path=str(summary))
            output.summary(["## Title", "- web: sha256:" + "a" * 64])
            output.emit({"event": "x", "n": 1})
            self.assertIn("## Title\n", summary.read_text(encoding="ascii"))
            self.assertEqual(stdout.getvalue(), '{"event":"x","n":1}\n')
            with self.assertRaises(common.ReleaseError):
                output.summary(["leak " + support.PG_ID])


class EnvironmentTests(unittest.TestCase):
    def test_scrubbed_env_never_carries_the_deploy_token(self) -> None:
        base = {"PATH": "/usr/bin", "HOME": "/home/x", "SHIP_DO_TOKEN": support.DO_TOKEN,
                "SHIP_TARGET_JSON": "{}", "GH_TOKEN": "x", "AWS_SECRET": "y"}
        env = common.scrubbed_env({"GH_TOKEN": "y"}, base=base)
        self.assertEqual(env["PATH"], "/usr/bin")
        self.assertEqual(env["GH_TOKEN"], "y")
        for name in ("SHIP_DO_TOKEN", "SHIP_TARGET_JSON", "AWS_SECRET"):
            self.assertNotIn(name, env)
        for extra in ({"SHIP_DO_TOKEN": "x"}, {"SHIP_TARGET_JSON": "x"}, {"A": 1}):
            with self.subTest(extra=extra), self.assertRaises(common.ReleaseError):
                common.scrubbed_env(extra, base=base)


class ContextTests(unittest.TestCase):
    def test_valid_context(self) -> None:
        control = common.validate_context(good_env())
        self.assertEqual(control["sha"], "a" * 40)
        self.assertEqual(control["run_attempt"], 1)

    def test_context_refusals(self) -> None:
        cases = {
            "repository": {"GITHUB_REPOSITORY": "fork/whatomate"},
            "non-main": {"GITHUB_REF": "refs/heads/feature"},
            "unprotected": {"GITHUB_REF_PROTECTED": "false"},
            "event": {"GITHUB_EVENT_NAME": "push"},
            "workflow-ref": {"GITHUB_WORKFLOW_REF": common.REPOSITORY + "/.github/workflows/other.yml@refs/heads/main"},
            "sha-mismatch": {"GITHUB_SHA": "b" * 40},
            "attempt-2": {"GITHUB_RUN_ATTEMPT": "2"},
            "self-hosted": {"RUNNER_ENVIRONMENT": "self-hosted"},
            "debug": {"RUNNER_DEBUG": "1"},
            "run-id": {"GITHUB_RUN_ID": "0"},
        }
        for label, change in cases.items():
            env = good_env()
            env.update(change)
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, "^context-invalid"):
                common.validate_context(env)

    def test_rerun_allowed_only_when_asked(self) -> None:
        env = good_env()
        env["GITHUB_RUN_ATTEMPT"] = "2"
        self.assertEqual(common.validate_context(env, allow_rerun=True)["run_attempt"], 2)


if __name__ == "__main__":
    unittest.main()
