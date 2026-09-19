"""Execute the orphan workflows' actual identity filters and branch code offline.

These tests require the workflow-pinned jq (RELEASE_TEST_JQ or PATH) and Bash
(RELEASE_TEST_BASH or PATH). Missing tools are failures, never skipped coverage.
All GitHub responses are synthetic; the one extracted gh-using function runs
with a rejecting local stub. No workflow dispatch or provider request occurs.
"""

from __future__ import annotations

import copy
import json
import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
FINALIZER = "finalize-production-orphan-lock.yml"
RECONCILIATION = "reconcile-production-orphan-lock-release.yml"
CONTROL_SHA = "a" * 40
REPOSITORY = "synthetic/release-tests"


def workflow(filename: str) -> str:
    return (WORKFLOWS / filename).read_text(encoding="utf-8")


def run_title(filename: str) -> str:
    source = workflow(filename)
    match = re.search(r"(?m)^run-name: (.+)$", source)
    if match is None:
        match = re.search(r"(?m)^name: (.+)$", source)
    if match is None:
        raise AssertionError(f"workflow title missing: {filename}")
    return match.group(1).replace("${{ inputs.phase }}", "ui")


def extracted_jq(source: str) -> list[tuple[str, str]]:
    # Each selected workflow uses a single-quoted literal jq program. Arguments
    # remain available so --slurp is exercised rather than silently emulated.
    return [
        (match.group("args"), match.group("program"))
        for match in re.finditer(
            r"(?ms)^[ \t]+jq\b(?P<args>[^']*)'(?P<program>.*?)'", source
        )
    ]


def shell_function(source: str, name: str) -> str:
    matches = list(re.finditer(
        rf"(?ms)^(?P<indent> +){re.escape(name)}\(\) \{{\n"
        rf".*?^(?P=indent)\}}$", source
    ))
    if len(matches) != 1:
        raise AssertionError(f"expected one exact function: {name}")
    return textwrap.dedent(matches[0].group(0))


def shell_arms(source: str, start: str) -> list[str]:
    return [textwrap.dedent(match.group(0)) for match in re.finditer(
        rf"(?ms)^(?P<indent> +){re.escape(start)}\n.*?^(?P=indent)fi$", source
    )]


class ReleaseOrphanIdentityRuntimeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.finalizer = workflow(FINALIZER)
        cls.reconciliation = workflow(RECONCILIATION)
        jq = os.environ.get("RELEASE_TEST_JQ") or shutil.which("jq")
        if not jq:
            raise AssertionError("pinned jq is required; set RELEASE_TEST_JQ")
        cls.jq = str(Path(jq).resolve())
        version = subprocess.run(
            [cls.jq, "--version"], capture_output=True, text=True, timeout=10,
            check=False,
        )
        expected = re.search(r"(?m)^  PINNED_JQ_VERSION: (.+)$", cls.finalizer)
        if expected is None or version.returncode != 0 or version.stdout.strip() != "jq-" + expected.group(1).strip('"'):
            raise AssertionError("runtime test jq must match the workflow pin")
        bash = os.environ.get("RELEASE_TEST_BASH")
        if not bash and os.name == "nt":
            git_bash = Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git/bin/bash.exe"
            if git_bash.is_file():
                bash = str(git_bash)
        bash = bash or shutil.which("bash")
        if not bash:
            raise AssertionError("Bash is required; set RELEASE_TEST_BASH")
        cls.bash = str(Path(bash).resolve())

    def run_filter(self, expression: tuple[str, str], value: object, **bindings: object) -> subprocess.CompletedProcess[str]:
        arguments, program = expression
        command = [self.jq, "-e"]
        if "--slurp" in arguments:
            command.append("--slurp")
            self.assertIsInstance(value, list)
            payload = "\n".join(json.dumps(item) for item in value)
        else:
            payload = json.dumps(value)
        for name, binding in bindings.items():
            if isinstance(binding, str):
                command.extend(["--arg", name, binding])
            else:
                command.extend(["--argjson", name, json.dumps(binding)])
        command.append(program)
        return subprocess.run(
            command, input=payload, capture_output=True, text=True, timeout=10,
            check=False,
        )

    def assert_filter(self, expression: tuple[str, str], value: object, valid: bool, **bindings: object) -> None:
        result = self.run_filter(expression, value, **bindings)
        if valid:
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), "true")
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)

    def run_shell(self, program: str, **variables: str) -> subprocess.CompletedProcess[str]:
        # Do not pass ambient shell startup files or provider/GitHub credentials.
        environment = {key: os.environ[key] for key in (
            "PATH", "SystemRoot", "WINDIR", "TEMP", "TMP"
        ) if key in os.environ}
        environment.update(variables)
        environment["RELEASE_TEST_JQ"] = Path(self.jq).as_posix()
        prelude = 'set -euo pipefail\njq() { command "$RELEASE_TEST_JQ" "$@"; }\n'
        return subprocess.run(
            [self.bash, "--noprofile", "--norc", "-c", prelude + program],
            env=environment, capture_output=True, text=True, timeout=15,
            check=False,
        )

    def run_record(self, filename: str, **changes: object) -> dict[str, object]:
        value: dict[str, object] = {
            "id": 77, "repository": {"full_name": REPOSITORY},
            "path": ".github/workflows/" + filename,
            "name": run_title(filename), "display_title": run_title(filename),
            "head_sha": CONTROL_SHA, "head_branch": "main",
            "event": "workflow_dispatch", "run_attempt": 1,
            "status": "completed", "conclusion": "success",
        }
        value.update(changes)
        return value

    def assert_bound_fields(self, expression: tuple[str, str], record: dict[str, object], fields: dict[str, object], **bindings: object) -> None:
        paired = "--slurp" in expression[0]
        self.assert_filter(expression, [record, record] if paired else record, True, **bindings)
        for field, wrong in fields.items():
            for absent in (False, True):
                candidate = copy.deepcopy(record)
                if absent:
                    candidate.pop(field, None)
                else:
                    candidate[field] = wrong
                # Mutate either endpoint independently; no missing value is
                # replaced with a false/default metadata value.
                for endpoint in range(2 if paired else 1):
                    payload = [copy.deepcopy(record), copy.deepcopy(record)] if paired else candidate
                    if paired:
                        payload[endpoint] = candidate
                    with self.subTest(field=field, absent=absent, endpoint=endpoint):
                        self.assert_filter(expression, payload, False, **bindings)

    def test_all_raw_subject_and_marker_filters_accept_real_run_titles(self) -> None:
        expressions = [item for item in extracted_jq(self.finalizer)
                       if ".name == $name" in item[1] and ".head_sha" in item[1]]
        self.assertEqual(len(expressions), 4)
        filenames = (
            "apply-production-phase.yml", "rollback-production-phase.yml",
            "rollback-production-orphan.yml", "reconcile-production-orphan.yml",
            "verify-production-crm-canary.yml",
        )
        for expression in expressions:
            for filename in filenames:
                with self.subTest(filter=expression[1][:70], workflow=filename):
                    record = self.run_record(filename)
                    fields: dict[str, object] = {
                        "name": workflow(filename).splitlines()[0].removeprefix("name: "),
                        "path": ".github/workflows/wrong.yml", "head_sha": "b" * 40,
                        "head_branch": "other", "event": "push", "run_attempt": 2,
                        "status": "in_progress",
                    }
                    if "$source_mode" in expression[1]:
                        fields["conclusion"] = "failure"
                    if ".repository.full_name" in expression[1]:
                        fields["repository"] = {"full_name": "wrong/repository"}
                    self.assert_bound_fields(
                        expression, record, fields, repository=REPOSITORY,
                        workflow=record["path"], name=record["name"],
                        sha=CONTROL_SHA, attempt=1, source_mode="terminal",
                    )
                    if "$source_mode" in expression[1]:
                        orphan = self.run_record(filename, conclusion="failure")
                        self.assert_filter(
                            expression, [orphan, orphan] if "--slurp" in expression[0] else orphan,
                            True, repository=REPOSITORY, workflow=orphan["path"],
                            name=orphan["name"], sha=CONTROL_SHA, attempt=1,
                            source_mode="orphan",
                        )

    def test_literal_subject_call_sites_bind_declared_run_titles(self) -> None:
        calls = re.findall(
            r'"\$(RECONCILE|CANARY|ORPHAN_ROLLBACK)_WORKFLOW_PATH" "([^"]+)"',
            self.finalizer,
        )
        self.assertEqual(len(calls), 10)
        for variable, name in calls:
            path = re.search(rf"(?m)^  {variable}_WORKFLOW_PATH: (.+)$", self.finalizer)
            self.assertIsNotNone(path)
            self.assertEqual(name, run_title(Path(path.group(1)).name))

    def test_actual_marker_shell_branches_select_exact_titles(self) -> None:
        arms = shell_arms(self.finalizer,
            'if [[ "$artifact_name" == "production-main-lock-apply-$run_id-1" ]]; then')
        self.assertEqual(len(arms), 2)
        emit = '\nprintf "%s\\n%s\\n" "$workflow_path" "$workflow_name"\n'
        for arm in arms:
            for operation in ("apply", "rollback"):
                filename = operation + "-production-phase.yml"
                result = self.run_shell(arm + emit, run_id="77",
                    artifact_name=f"production-main-lock-{operation}-77-1")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(), [
                    ".github/workflows/" + filename, run_title(filename)
                ])
            self.assertNotEqual(self.run_shell(arm + emit, run_id="77",
                artifact_name="production-main-lock-apply-77-2").returncode, 0)

    def test_actual_intent_shell_branches_select_exact_titles(self) -> None:
        arms = shell_arms(self.finalizer,
            'if [[ "$intent_operation" == "activate" && "$intent_workflow_path" == ".github/workflows/apply-production-phase.yml" ]]; then')
        self.assertEqual(len(arms), 1)
        emit = '\nprintf "%s\\n" "${original_workflow_name:-$original_name}"\n'
        for arm in arms:
            for operation, filename in (
                ("activate", "apply-production-phase.yml"),
                ("rollback", "rollback-production-phase.yml"),
                ("rollback", "rollback-production-orphan.yml"),
            ):
                result = self.run_shell(arm + emit, intent_operation=operation,
                    intent_workflow_path=".github/workflows/" + filename,
                    ORPHAN_ROLLBACK_WORKFLOW_PATH=".github/workflows/rollback-production-orphan.yml")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), run_title(filename))
            self.assertNotEqual(self.run_shell(arm + emit,
                intent_operation="activate", intent_workflow_path=".github/workflows/wrong.yml",
                ORPHAN_ROLLBACK_WORKFLOW_PATH=".github/workflows/rollback-production-orphan.yml").returncode, 0)

    def test_actual_reacquire_case_checks_operation_and_path(self) -> None:
        arms = list(re.finditer(
            r'(?ms)^(?P<indent> +)case "\$ORIGINAL_INTENT_SLUG" in\n.*?^(?P=indent)esac$',
            self.finalizer,
        ))
        self.assertEqual(len(arms), 1)
        program = textwrap.dedent(arms[0].group(0)) + '\nprintf "%s\\n" "$original_name"\n'
        for slug, operation, filename in (
            ("apply", "activate", "apply-production-phase.yml"),
            ("rollback", "rollback", "rollback-production-phase.yml"),
            ("orphan-rollback", "rollback", "rollback-production-orphan.yml"),
        ):
            variables = {
                "ORIGINAL_INTENT_SLUG": slug, "ORIGINAL_OPERATION": operation,
                "ORIGINAL_WORKFLOW_PATH": ".github/workflows/" + filename,
                "ORPHAN_ROLLBACK_WORKFLOW_PATH": ".github/workflows/rollback-production-orphan.yml",
            }
            result = self.run_shell(program, **variables)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), run_title(filename))
            for field in ("ORIGINAL_INTENT_SLUG", "ORIGINAL_OPERATION", "ORIGINAL_WORKFLOW_PATH"):
                self.assertNotEqual(self.run_shell(program, **{**variables, field: "wrong"}).returncode, 0)

    def test_current_finalizer_and_reconciliation_filters(self) -> None:
        for filename in (FINALIZER, RECONCILIATION):
            expressions = [item for item in extracted_jq(workflow(filename))
                           if '.name == "' in item[1] and '.status == "in_progress"' in item[1]
                           and ".jobs" not in item[1]]
            self.assertEqual(len(expressions), 2)
            for expression in expressions:
                record = self.run_record(filename, status="in_progress", conclusion=None)
                self.assert_bound_fields(expression, record, {
                    "path": ".github/workflows/wrong.yml", "name": "old title",
                    "head_sha": "b" * 40, "head_branch": "other", "event": "push",
                    "run_attempt": 2, "status": "completed",
                }, sha=CONTROL_SHA)

    def test_failed_finalizer_reconciliation_filters_preserve_terminal_policy(self) -> None:
        expressions = [item for item in extracted_jq(self.reconciliation)
                       if '.name == "' in item[1] and '.status == "completed"' in item[1]
                       and '.github/workflows/finalize-production-orphan-lock.yml' in item[1]]
        self.assertEqual(len(expressions), 3)
        for expression in expressions:
            for conclusion in ("failure", "cancelled", "timed_out"):
                record = self.run_record(FINALIZER, conclusion=conclusion)
                self.assert_bound_fields(expression, record, {
                    "id": 99, "path": ".github/workflows/wrong.yml", "name": "old title",
                    "head_sha": "b" * 40, "head_branch": "other", "event": "push",
                    "run_attempt": 2, "status": "in_progress", "conclusion": "success",
                }, sha=CONTROL_SHA, run_id="77")

    def test_all_competing_filters_use_paths_and_reject_missing_metadata(self) -> None:
        expressions = [item for item in extracted_jq(self.finalizer)
                       if ".workflow_runs" in item[1] and "IN(" in item[1]]
        self.assertEqual(len(expressions), 3)
        controlled = sorted(path.name for path in WORKFLOWS.glob("*.yml")
                            if re.search(r"(?m)^  group: rereply-production$", path.read_text(encoding="utf-8")))
        for expression in expressions:
            self.assert_filter(expression, {"total_count": 0, "workflow_runs": []}, True, run_id="88")
            for filename in controlled:
                record = self.run_record(filename, status="in_progress", conclusion=None)
                self.assert_filter(expression, {"total_count": 1, "workflow_runs": [record]}, False, run_id="88")
            own = self.run_record(FINALIZER, id=88)
            unrelated = self.run_record("test.yml")
            for record in (own, unrelated):
                self.assert_filter(expression, {"total_count": 1, "workflow_runs": [record]}, True, run_id="88")
                for field in ("id", "path"):
                    candidate = copy.deepcopy(record)
                    candidate.pop(field)
                    self.assert_filter(expression, {"total_count": 1, "workflow_runs": [candidate]}, False, run_id="88")
            self.assert_filter(expression, {"total_count": 2, "workflow_runs": [unrelated]}, False, run_id="88")

    def test_actual_competing_shell_function_has_only_mocked_api_reads(self) -> None:
        function = shell_function(self.finalizer, "assert_no_competing_control_run")
        mock = '''
gh() {
  [[ "$#" == 2 && "$1" == api && "$2" == "/repos/$REPOSITORY/actions/runs?status=$TEST_STATUS&per_page=100" ]] || return 90
  printf '%s\\n' "$MOCK_RUNS_JSON"
}
'''
        for status in ("queued", "in_progress"):
            for competing in (False, True):
                record = self.run_record("apply-production-phase.yml" if competing else "test.yml", status=status)
                with tempfile.TemporaryDirectory(prefix="orphan-identity-runtime-") as temporary:
                    result = self.run_shell(mock + function + '\nassert_no_competing_control_run "$TEST_STATUS"\n',
                        RUNNER_TEMP=Path(temporary).as_posix(), REPOSITORY=REPOSITORY,
                        RUN_ID="88", TEST_STATUS=status,
                        MOCK_RUNS_JSON=json.dumps({"total_count": 1, "workflow_runs": [record]}))
                if competing:
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_normalized_evidence_labels_are_not_api_run_titles(self) -> None:
        self.assertIn('workflow_name:"Finalize Production Orphan Lock",run_id:$run_id,run_attempt:1,', self.reconciliation)
        for filename in (
            "production-orphan-lock-release-assertion.schema.json",
            "production-orphan-lock-release-reconciliation.schema.json",
        ):
            self.assertIn('"workflow_name": {"const": "Finalize Production Orphan Lock"}',
                          (Path(__file__).parent / filename).read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
