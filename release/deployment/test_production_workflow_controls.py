from __future__ import annotations

import hashlib
import json
import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
ACTIVE_PRODUCTION_CONTROLS = (
    "validate-exact-release-source.yml",
    "build-attest-exact-release-images.yml",
    "aggregate-exact-four-phase-rollout.yml",
    "plan-production-rollout.yml",
    "verify-production-recovery-readiness.yml",
    "apply-production-phase.yml",
    "verify-production-crm-canary.yml",
    "rollback-production-phase.yml",
    "reconcile-production-orphan.yml",
    "rollback-production-orphan.yml",
    "finalize-production-orphan-lock.yml",
    "reconcile-production-orphan-lock-release.yml",
    "reconcile-production-main-lock-release.yml",
)
PINNED_ACTION = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}(?:\s+#.*)?$")
TERMINAL_PARITY_WORKFLOW_SHA256 = {
    "apply-production-phase.yml": (
        "f2f03a0a42b6d7d816eda310fa0b5dfaa8f8f843d280ba96b03bce916e1581c6"
    ),
    "rollback-production-phase.yml": (
        "2a1b66fa716ae078fad7d5c92788b7bec88a8c5ee899762418124b787b0ac8f7"
    ),
    "finalize-production-orphan-lock.yml": (
        "1c4b6e4147afe099ceddd2faa1feaae5da769f1c9c2e57abef61750ffb94370a"
    ),
    "reconcile-production-orphan.yml": (
        "ca16cd68409b28ef34a131c764ba2ee8c0349be420cb99fab1f587ab7512e9ef"
    ),
    "reconcile-production-main-lock-release.yml": (
        "069c7eefa27b3a9159bb41d870f1c871db55b37e4b052061392b0306617b5a99"
    ),
    "verify-production-crm-canary.yml": (
        "7e75973086634d18b1c8c1f5082608de4e08d4f2db227cfc5d95fc2dd5cfc02a"
    ),
}


def workflow(name: str) -> str:
    path = WORKFLOWS / name
    if not path.is_file() or path.is_symlink():
        raise AssertionError(f"required workflow is missing or not regular: {name}")
    return path.read_text(encoding="utf-8")


def job_block(source: str, job: str) -> str:
    match = re.search(
        rf"(?ms)^  {re.escape(job)}:\s*$.*?(?=^  [a-zA-Z0-9_-]+:\s*$|\Z)",
        source,
    )
    if match is None:
        raise AssertionError(f"job not found: {job}")
    return match.group(0)


def step_block(source: str, name: str) -> str:
    match = re.search(
        rf"(?ms)^      - name: {re.escape(name)}\s*$.*?(?=^      - name: |\Z)",
        source,
    )
    if match is None:
        raise AssertionError(f"step not found: {name}")
    return match.group(0)


def shell_function_block(source: str, name: str) -> str:
    matches = re.findall(
        rf"(?ms)^          {re.escape(name)}\(\) \{{[ \t]*$.*?"
        rf"^          \}}[ \t]*$",
        source,
    )
    if len(matches) != 1:
        raise AssertionError(
            f"expected one shell function {name}, found {len(matches)}"
        )
    return matches[0]


def require_active_source_line(source: str, line: str, count: int = 1) -> None:
    matches = sum(
        candidate.strip() == line
        for candidate in source.splitlines()
        if not candidate.lstrip().startswith("#")
    )
    if matches != count:
        raise AssertionError(
            f"required active source line count differs: {line}: "
            f"expected {count}, found {matches}"
        )


def job_definitions(source: str) -> tuple[tuple[str, str], ...]:
    matches = re.findall(r"(?ms)^jobs:[ \t]*$\n(?P<body>.*)\Z", source)
    if len(matches) != 1:
        raise AssertionError(f"expected one jobs mapping, found {len(matches)}")
    body = matches[0]
    job_pattern = re.compile(
        r"(?m)^  (?:(?P<plain>[a-zA-Z_][a-zA-Z0-9_-]*)|"
        r"\x22(?P<double>[a-zA-Z_][a-zA-Z0-9_-]*)\x22|"
        r"'(?P<single>[a-zA-Z_][a-zA-Z0-9_-]*)')[ \t]*:[^\r\n]*$"
    )
    jobs = tuple(job_pattern.finditer(body))
    top_level_keys = tuple(
        match
        for match in re.finditer(r"(?m)^  (?!#)[^ \t\r\n][^\r\n]*$", body)
    )
    if tuple(match.start() for match in jobs) != tuple(
        match.start() for match in top_level_keys
    ):
        raise AssertionError("unparsed top-level job key found")
    definitions: list[tuple[str, str]] = []
    for index, match in enumerate(jobs):
        job_id = next(
            value
            for value in (
                match.group("plain"),
                match.group("double"),
                match.group("single"),
            )
            if value is not None
        )
        end = jobs[index + 1].start() if index + 1 < len(jobs) else len(body)
        block = body[match.start() : end]
        names = re.findall(r"(?m)^    name: (.+)$", block)
        if len(names) != 1:
            raise AssertionError(
                f"job {job_id} must have exactly one explicit name, found {len(names)}"
            )
        definitions.append((job_id, names[0]))
    return tuple(definitions)


def job_names(source: str) -> tuple[str, ...]:
    return tuple(name for _, name in job_definitions(source))


def job_ids(source: str) -> tuple[str, ...]:
    return tuple(job_id for job_id, _ in job_definitions(source))


def shell_json_array_assignments(
    source: str, variable: str
) -> tuple[tuple[str, ...], ...]:
    pattern = rf"(?m)^\s*{re.escape(variable)}='(\[[^\r\n]*\])'\s*$"
    arrays = tuple(tuple(json.loads(raw)) for raw in re.findall(pattern, source))
    assignment_count = len(
        re.findall(
            rf"(?m)^[ \t]*{re.escape(variable)}(?:\+)?=",
            source,
        )
    )
    if assignment_count != len(arrays):
        raise AssertionError(
            f"unparsed shell assignment found for {variable}: "
            f"parsed {len(arrays)} of {assignment_count}"
        )
    if any(not all(isinstance(value, str) for value in values) for values in arrays):
        raise AssertionError(f"non-string shell array item found: {variable}")
    return arrays


def single_shell_json_array(source: str, variable: str, label: str) -> tuple[str, ...]:
    arrays = shell_json_array_assignments(source, variable)
    if len(arrays) != 1:
        raise AssertionError(
            f"expected one {variable} assignment in {label}, found {len(arrays)}"
        )
    return arrays[0]


def assert_no_nested_shell_control(source: str, label: str) -> None:
    nested_control = re.compile(
        r"(?m)^[ \t]*(?:(?:if|elif|else|fi|case|esac|for|while|until|select|"
        r"function)\b|[a-zA-Z_][a-zA-Z0-9_]*[ \t]*\(\)[ \t]*\{|"
        r"[{}][ \t]*$)"
    )
    if nested_control.search(source) is not None or "<<" in source:
        raise AssertionError(f"nested shell control found in {label}")


def shell_if_arm_array(
    source: str, condition: str, variable: str
) -> tuple[str, ...]:
    matches = re.findall(
        rf"(?ms)^[ \t]*(?:if|elif)[ \t]+\[\[[ \t]+"
        rf"{re.escape(condition)}[ \t]+\]\];[ \t]+then[ \t]*$"
        rf"(?P<body>.*?)(?=^[ \t]*(?:elif[ \t]+\[\[|else[ \t]*$|fi[ \t]*$))",
        source,
    )
    if len(matches) != 1:
        raise AssertionError(
            f"expected one shell conditional arm for {condition}, found {len(matches)}"
        )
    assert_no_nested_shell_control(matches[0], condition)
    return single_shell_json_array(matches[0], variable, condition)


def shell_case_arm_array(
    source: str, arm_pattern: str, variable: str
) -> tuple[str, ...]:
    arms = re.findall(
        rf"(?ms)^[ \t]+(?:{arm_pattern})\)[ \t]*$"
        rf"(?P<body>.*?)(?=^[ \t]+;;[ \t]*$)",
        source,
    )
    assignments: list[tuple[str, ...]] = []
    for arm in arms:
        arrays = shell_json_array_assignments(arm, variable)
        if len(arrays) > 1:
            raise AssertionError(
                f"multiple {variable} assignments in case arm {arm_pattern}"
            )
        if arrays:
            assert_no_nested_shell_control(arm, arm_pattern)
            assignments.extend(arrays)
    if len(assignments) != 1:
        raise AssertionError(
            f"expected one {variable} assignment for case arm {arm_pattern}, "
            f"found {len(assignments)}"
        )
    return assignments[0]


def javascript_string_array(body: str, label: str) -> tuple[str, ...]:
    if re.fullmatch(r"\s*'[^'\r\n]+'(?:\s*,\s*'[^'\r\n]+')*\s*", body) is None:
        raise AssertionError(f"unparsed JavaScript array item found: {label}")
    return tuple(re.findall(r"'([^']+)'", body))


def javascript_without_block_comments(source: str) -> str:
    return re.sub(r"/\*.*?\*/", "", source, flags=re.DOTALL)


def require_active_javascript_line(source: str, line: str) -> None:
    matches = re.findall(rf"(?m)^[ \t]+{re.escape(line)}[ \t]*$", source)
    if len(matches) != 1:
        raise AssertionError(f"required JavaScript dataflow line differs: {line}")


def javascript_array_assignment(source: str, variable: str) -> tuple[str, ...]:
    active_source = javascript_without_block_comments(source)
    matches = re.findall(
        rf"(?m)^[ \t]+const {re.escape(variable)}="
        rf"\[(?P<body>[^\]]*)\];[ \t]*$",
        active_source,
    )
    if len(matches) != 1:
        raise AssertionError(
            f"expected one JavaScript array assignment for {variable}, found {len(matches)}"
        )
    assert_javascript_array_is_not_mutated(active_source, variable)
    return javascript_string_array(matches[0], variable)


def assert_javascript_array_is_not_mutated(source: str, variable: str) -> None:
    mutating_method = re.compile(
        rf"\b{re.escape(variable)}\."
        rf"(?:copyWithin|fill|pop|push|reverse|shift|sort|splice|unshift)\s*\("
    )
    if mutating_method.search(source) is not None:
        raise AssertionError(f"JavaScript array is mutated: {variable}")
    without_declaration = source.replace(f"const {variable}=", "", 1)
    reassignment = re.compile(
        rf"\b{re.escape(variable)}\s*(?:\[[^\]]+\]\s*)?"
        rf"(?:=|\+=|-=|\*=|/=|%=|\+\+|--)"
    )
    if reassignment.search(without_declaration) is not None:
        raise AssertionError(f"JavaScript array is reassigned: {variable}")


def reconcile_operation_job_arrays(source: str) -> dict[str, tuple[str, ...]]:
    active_source = javascript_without_block_comments(source)
    pattern = re.compile(
        r"(?m)^[ \t]+const expectedJobs=operation==='apply'\?\s*"
        r"\[(?P<apply>[^\]]*)\]:\s*operation==='rollback'\?\s*"
        r"\[(?P<rollback>[^\]]*)\]:\s*"
        r"\[(?P<orphan_rollback>[^\]]*)\];[ \t]*$"
    )
    matches = tuple(pattern.finditer(active_source))
    declaration_count = len(
        re.findall(r"(?m)^[ \t]+const expectedJobs=", active_source)
    )
    if declaration_count != 1 or len(matches) != 1:
        raise AssertionError("expected exactly one reconcile expectedJobs assignment")
    assert_javascript_array_is_not_mutated(active_source, "expectedJobs")
    if len(re.findall(r"\bexpectedJobs\b", active_source)) != 4:
        raise AssertionError("reconcile expectedJobs reference count differs")
    require_active_javascript_line(
        active_source,
        "if(JSON.stringify(jobs.map(j=>j.name).sort())!==JSON.stringify("
        "[...expectedJobs].sort())) throw new Error('original job inventory differs');",
    )
    require_active_javascript_line(
        active_source,
        "const prerequisiteJobs=operation==='orphan-rollback'?"
        "expectedJobs.slice(0,2):expectedJobs.slice(0,3);",
    )
    match = matches[0]
    return {
        operation: javascript_string_array(
            match.group(operation), f"reconcile {operation} jobs"
        )
        for operation in ("apply", "rollback", "orphan_rollback")
    }


def sorted_inventory(values: tuple[str, ...]) -> tuple[str, ...]:
    return tuple(sorted(values))


def jq_literal_job_inventory(source: str) -> tuple[str, ...]:
    matches = re.findall(
        r"\(\[\.jobs\[\]\.name\]\s*\|\s*sort\)\s*==\s*"
        r"\(\[(?P<body>.*?)\]\s*\|\s*sort\)",
        source,
        re.DOTALL,
    )
    if len(matches) != 1:
        raise AssertionError(
            f"expected one literal jq job inventory, found {len(matches)}"
        )
    values = json.loads(f"[{matches[0]}]")
    if not all(isinstance(value, str) for value in values):
        raise AssertionError("non-string jq job inventory item found")
    return tuple(values)


def workflow_step_blocks(source: str) -> tuple[str, ...]:
    return tuple(
        match.group(0)
        for match in re.finditer(
            r"(?ms)^      -(?: [^\r\n]*)?[ \t]*$.*?"
            r"(?=^      -(?: [^\r\n]*)?[ \t]*$|\Z)",
            source,
        )
    )


def artifact_upload_use_count(source: str) -> int:
    return len(
        re.findall(
            r"(?m)^(?:      - uses:|        uses:) "
            r"actions/upload-artifact@[0-9a-f]{40}"
            r"(?:\s+#.*)?$",
            source,
        )
    )


def is_artifact_upload_step(source: str) -> bool:
    return artifact_upload_use_count(source) == 1


def yaml_mapping_key_pattern(indent: int, key: str) -> re.Pattern[str]:
    escaped = re.escape(key)
    return re.compile(
        rf"(?m)^ {{{indent}}}(?:{escaped}|\x22{escaped}\x22|'{escaped}')"
        rf"[ \t]*:"
    )


def assert_no_quoted_mapping_key(source: str, indent: int, label: str) -> None:
    quoted_key = re.compile(
        rf"(?m)^ {{{indent}}}(?:\x22(?:\\.|[^\x22\\])*\x22|"
        rf"'(?:[^']|'')*')[ \t]*:"
    )
    if quoted_key.search(source) is not None:
        raise AssertionError(f"quoted YAML key is forbidden in {label}")


def assert_no_dynamic_job_fanout(source: str) -> None:
    assert_no_quoted_mapping_key(source, 4, "producer job properties")
    assert_no_quoted_mapping_key(source, 6, "producer strategy properties")
    for indent, key in ((4, "strategy"), (6, "matrix")):
        if yaml_mapping_key_pattern(indent, key).search(source) is not None:
            raise AssertionError(f"producer job fan-out is forbidden: {key}")


def assert_unconditional_artifact_upload(source: str) -> None:
    assert_no_quoted_mapping_key(source, 8, "artifact upload step")
    for key in ("if", "continue-on-error"):
        if yaml_mapping_key_pattern(8, key).search(source) is not None:
            raise AssertionError(f"artifact upload step is conditional: {key}")


def workflow_step_identity(source: str) -> str:
    match = re.match(r"^      - name: (?P<name>.+?)\s*$", source, re.MULTILINE)
    if match is None:
        raise AssertionError("workflow step identity is missing")
    return match.group("name")


def assert_github_output_is_not_rebound(source: str) -> None:
    active_lines = "\n".join(
        line for line in source.splitlines() if not line.lstrip().startswith("#")
    )
    forbidden = (
        r"os\.environ\[[\x22\x27]GITHUB_OUTPUT[\x22\x27]\]\s*"
        r"(?:=|:=|\+=|-=|\*=|/=|%=)",
        r"\bdel\s+os\.environ\[[\x22\x27]GITHUB_OUTPUT[\x22\x27]\]",
        r"os\.environ\.(?:clear|popitem|update)\s*\(",
        r"os\.environ\.(?:pop|setdefault|__setitem__)\s*\([ \t]*"
        r"[\x22\x27]GITHUB_OUTPUT[\x22\x27]",
        r"os\.(?:putenv|unsetenv)\s*\([ \t]*[\x22\x27]GITHUB_OUTPUT[\x22\x27]",
        r"(?m)^[ \t]*(?:(?:export|readonly|local|declare(?:[ \t]+-[a-zA-Z]+)?)"
        r"[ \t]+)?GITHUB_OUTPUT(?:\+)?=",
        r"(?m)^[ \t]*(?:unset|read)[^\r\n]*\bGITHUB_OUTPUT\b",
        r"(?m)^[ \t]*printf[ \t]+-v[ \t]+GITHUB_OUTPUT\b",
    )
    if any(re.search(pattern, active_lines) is not None for pattern in forbidden):
        raise AssertionError("GITHUB_OUTPUT is rebound or mutated in producer step")


def exact_github_output_names(source: str) -> tuple[str, ...]:
    active_lines = tuple(
        line for line in source.splitlines() if not line.lstrip().startswith("#")
    )
    active_source = "\n".join(active_lines)
    python_sink = re.compile(
        r"^(?P<indent>[ \t]+)with pathlib\.Path\("
        r"os\.environ\['GITHUB_OUTPUT'\]\)\.open\('a',[^\r\n]*\) "
        r"as stream:[ \t]*$"
    )
    python_sinks = tuple(
        (index, match)
        for index, line in enumerate(active_lines)
        if (match := python_sink.match(line)) is not None
    )
    if python_sinks:
        if len(python_sinks) != 1 or active_source.count("GITHUB_OUTPUT") != 1:
            raise AssertionError("Python producer output sink differs")
        index, match = python_sinks[0]
        parent_indent = len(match.group("indent").expandtabs(8))
        suite: list[str] = []
        for line in active_lines[index + 1 :]:
            if not line.strip():
                continue
            indent = len(line) - len(line.lstrip(" \t"))
            if indent <= parent_indent:
                break
            suite.append(line)
        write_pattern = re.compile(
            r"^[ \t]+stream\.write\(f(?P<quote>[\x22\x27])"
            r"(?P<name>[a-z][a-z0-9_]*)=[^\r\n]*\\n"
            r"(?P=quote)\)[ \t]*$"
        )
        writes = tuple(write_pattern.fullmatch(line) for line in suite)
        if not suite or any(match is None for match in writes):
            raise AssertionError("Python producer output writes differ")
        return tuple(match.group("name") for match in writes if match is not None)

    shell_write = re.compile(
        r"printf[ \t]+(?P<quote>[\x22\x27])"
        r"(?P<name>[a-z][a-z0-9_]*)=[^\x22\x27\r\n]*\\n"
        r"(?P=quote)[^;\r\n]*[ \t]+>>[ \t]+"
        r"[\x22\x27]\$GITHUB_OUTPUT[\x22\x27]"
    )
    writes = tuple(shell_write.finditer(active_source))
    if not writes or active_source.count("GITHUB_OUTPUT") != len(writes):
        raise AssertionError("shell producer output writes differ")
    return tuple(match.group("name") for match in writes)


def emitted_output_artifact_prefix(
    source: str,
    output_name: str,
    producer_step_id: str,
    expected_output_names: tuple[str, ...],
) -> tuple[str, str]:
    assignment_patterns = (
        re.compile(
            rf"(?m)^[ \t]+with pathlib\.Path\(os\.environ\['GITHUB_OUTPUT'\]\)"
            rf"\.open\('a',[^\r\n]*\) as stream:[ \t]*\r?\n"
            rf"[ \t]+stream\.write\(f[\x22\x27]"
            rf"{re.escape(output_name)}=(?P<prefix>[a-z0-9][a-z0-9-]*)-"
            rf"(?=\{{os\.environ\[)"
        ),
        re.compile(
            rf"(?m)^[ \t]+printf[ \t]+[\x22\x27]"
            rf"{re.escape(output_name)}=(?P<prefix>[a-z0-9][a-z0-9-]*)-"
            rf"(?=%s)[^;\r\n]*[ \t]+>>[ \t]+"
            rf"[\x22\x27]\$GITHUB_OUTPUT[\x22\x27](?:[ \t]*;|[ \t]*$)"
        ),
    )
    emitted: list[tuple[str, str]] = []
    for block in workflow_step_blocks(source):
        matches = [
            match
            for pattern in assignment_patterns
            for match in pattern.findall(block)
        ]
        if matches:
            if len(matches) != 1:
                raise AssertionError(f"ambiguous producer output: {output_name}")
            emitted.append((block, matches[0]))
    if len(emitted) != 1:
        raise AssertionError(
            f"expected one producer output for {output_name}, found {len(emitted)}"
        )
    emitter, prefix = emitted[0]
    assert_github_output_is_not_rebound(emitter)
    actual_output_names = exact_github_output_names(emitter)
    if (
        sorted_inventory(actual_output_names)
        != sorted_inventory(expected_output_names)
        or len(actual_output_names) != len(set(actual_output_names))
    ):
        raise AssertionError(
            f"producer output inventory differs for {output_name}"
        )
    if not re.search(rf"(?m)^        id: {re.escape(producer_step_id)}\s*$", emitter):
        raise AssertionError(f"producer step id differs for {output_name}")

    binding_pattern = re.compile(
        rf"(?m)^          name: \$\{{\{{[ \t]+steps\."
        rf"{re.escape(producer_step_id)}\.outputs\.{re.escape(output_name)}"
        rf"[ \t]+\}}\}}[ \t]*$"
    )
    uploads = [
        block
        for block in workflow_step_blocks(source)
        if binding_pattern.search(block) is not None and is_artifact_upload_step(block)
    ]
    if len(uploads) != 1 or len(binding_pattern.findall(uploads[0])) != 1:
        raise AssertionError(f"artifact upload binding differs for {output_name}")
    assert_unconditional_artifact_upload(uploads[0])
    return prefix, workflow_step_identity(uploads[0])


def emitted_direct_artifact_prefix(source: str) -> tuple[str, str]:
    pattern = re.compile(
        r"(?m)^          name: (?P<prefix>[a-z0-9][a-z0-9-]*)-"
        r"\$\{\{\s*github\.run_id\s*\}\}-"
        r"\$\{\{\s*github\.run_attempt\s*\}\}\s*$"
    )
    matches: list[str] = []
    matching_uploads: list[str] = []
    for block in workflow_step_blocks(source):
        if not is_artifact_upload_step(block):
            continue
        block_matches = pattern.findall(block)
        matches.extend(block_matches)
        if block_matches:
            matching_uploads.append(block)
    if len(matches) != 1 or len(matching_uploads) != 1:
        raise AssertionError(
            f"expected one direct artifact-name producer, found {len(matches)}"
        )
    assert_unconditional_artifact_upload(matching_uploads[0])
    return matches[0], workflow_step_identity(matching_uploads[0])


def producer_artifact_prefixes(source: str, operation: str) -> tuple[str, ...]:
    operation_job = "apply" if operation == "apply" else "rollback"
    emitted = (
        emitted_output_artifact_prefix(
            job_block(source, "prelock"),
            "root_marker_name",
            "prepare",
            (
                "root_marker_name",
                "root_marker_sha256",
                "rule_id",
                "rule_identity_sha256",
                "route_contract_sha256",
            ),
        ),
        emitted_output_artifact_prefix(
            job_block(source, "intent"),
            "intent_name",
            "prepare",
            ("intent_name", "intent_sha256"),
        ),
        emitted_output_artifact_prefix(
            job_block(source, "lock_proof"),
            "proof_name",
            "prepare",
            ("proof_name", "proof_sha256"),
        ),
        emitted_output_artifact_prefix(
            job_block(source, operation_job),
            "internal_name",
            "name",
            ("internal_name",),
        ),
        emitted_direct_artifact_prefix(job_block(source, "gate")),
        emitted_output_artifact_prefix(
            job_block(source, "release_authorization"),
            "intent_name",
            "release_intent",
            ("intent_name", "intent_sha256"),
        ),
    )
    step_blocks = workflow_step_blocks(source)
    if (
        source.count("actions/upload-artifact@") != 6
        or artifact_upload_use_count(source) != 6
    ):
        raise AssertionError("producer must contain exactly six artifact uploads")
    all_uploads = tuple(
        workflow_step_identity(block)
        for block in step_blocks
        if is_artifact_upload_step(block)
    )
    selected_uploads = tuple(upload for _, upload in emitted)
    if len(all_uploads) != 6 or sorted(all_uploads) != sorted(selected_uploads):
        raise AssertionError(
            "producer artifact upload inventory is not exhausted by six bound outputs"
        )
    return tuple(prefix for prefix, _ in emitted)


class WorkflowAuthorityPolicyTests(unittest.TestCase):
    def test_every_active_release_control_is_manual_exact_main_and_serialized(self) -> None:
        for name in ACTIVE_PRODUCTION_CONTROLS:
            with self.subTest(workflow=name):
                source = workflow(name)
                self.assertRegex(source, r"(?m)^  workflow_dispatch:\s*$")
                self.assertNotRegex(
                    source,
                    r"(?m)^  (?:push|pull_request|schedule|workflow_run|repository_dispatch):\s*$",
                )
                self.assertIn("refs/heads/main", source)
                self.assertIn("${{ github.workflow_sha }}", source)
                self.assertRegex(source, r"(?m)^concurrency:\s*$")
                self.assertRegex(source, r"(?m)^  group: rereply-production\s*$")
                self.assertRegex(source, r"(?m)^  cancel-in-progress: false\s*$")

    def test_every_external_action_is_pinned_to_a_full_commit(self) -> None:
        for name in ACTIVE_PRODUCTION_CONTROLS:
            with self.subTest(workflow=name):
                for line in workflow(name).splitlines():
                    match = re.match(r"\s*-?\s*uses:\s*(\S.*)$", line)
                    if match is not None and "/" in match.group(1) and "@" in match.group(1):
                        self.assertRegex(match.group(1), PINNED_ACTION)

    def test_every_attestation_verification_job_has_explicit_read_authority(self) -> None:
        for filename in ACTIVE_PRODUCTION_CONTROLS:
            source = workflow(filename)
            for match in re.finditer(
                r"(?ms)^  (?P<job>[a-zA-Z0-9_-]+):\s*$.*?(?=^  [a-zA-Z0-9_-]+:\s*$|\Z)",
                source,
            ):
                block = match.group(0)
                if "attestation verify" not in block:
                    continue
                with self.subTest(workflow=filename, job=match.group("job")):
                    self.assertRegex(
                        block,
                        r"(?m)^      attestations: (?:read|write)\s*(?:#.*)?$",
                    )

    def test_signed_authority_artifacts_are_uploaded_only_after_both_attestations(self) -> None:
        authorities = (
            (
                "apply-production-phase.yml",
                "intent",
                "Attest production mutation intent provenance",
                "Attest exact production mutation intent policy",
                "Upload exact production mutation intent",
            ),
            (
                "apply-production-phase.yml",
                "lock_proof",
                "Attest production apply main lock proof provenance",
                "Attest exact production apply main lock proof policy",
                "Upload exact production apply main lock proof",
            ),
            (
                "rollback-production-phase.yml",
                "intent",
                "Attest production rollback mutation intent provenance",
                "Attest exact production rollback mutation intent policy",
                "Upload exact production rollback mutation intent",
            ),
            (
                "rollback-production-phase.yml",
                "lock_proof",
                "Attest production rollback main lock proof provenance",
                "Attest exact production rollback main lock proof policy",
                "Upload exact production rollback main lock proof",
            ),
            (
                "rollback-production-orphan.yml",
                "intent",
                "Attest production orphan rollback mutation intent provenance",
                "Attest exact production orphan rollback mutation intent policy",
                "Upload exact production orphan rollback mutation intent",
            ),
            (
                "rollback-production-orphan.yml",
                "lock_proof",
                "Attest production orphan rollback main lock proof provenance",
                "Attest exact production orphan rollback main lock proof policy",
                "Upload exact production orphan rollback main lock proof",
            ),
            (
                "reconcile-production-orphan.yml",
                "assertion",
                "Attest exact lock assertion provenance",
                "Attest exact single-operator lock assertion",
                "Upload exact production main lock assertion",
            ),
            (
                "finalize-production-orphan-lock.yml",
                "preauthorization",
                "Attest pre-unlock authorization provenance",
                "Attest exact pre-unlock authorization policy",
                "Upload exact pre-unlock authorization",
            ),
            (
                "reconcile-production-orphan-lock-release.yml",
                "assertion",
                "Attest exact incomplete-finalizer assertion provenance",
                "Attest exact incomplete-finalizer assertion policy",
                "Upload exact incomplete-finalizer assertion",
            ),
            (
                "reconcile-production-main-lock-release.yml",
                "assertion",
                "Attest exact failed normal-release assertion provenance",
                "Attest exact failed normal-release assertion policy",
                "Upload exact failed normal-release assertion",
            ),
        )
        for filename, job, provenance, policy, upload in authorities:
            with self.subTest(workflow=filename, job=job):
                block = job_block(workflow(filename), job)
                upload_position = block.index(f"- name: {upload}")
                self.assertLess(block.index(f"- name: {provenance}"), upload_position)
                self.assertLess(block.index(f"- name: {policy}"), upload_position)

    def test_no_signed_authority_job_uploads_before_its_attestations(self) -> None:
        for filename in ACTIVE_PRODUCTION_CONTROLS:
            source = workflow(filename)
            for match in re.finditer(
                r"(?ms)^  (?P<job>[a-zA-Z0-9_-]+):\s*$.*?(?=^  [a-zA-Z0-9_-]+:\s*$|\Z)",
                source,
            ):
                block = match.group(0)
                attestation_positions = [
                    item.start()
                    for item in re.finditer(r"uses: actions/attest@", block)
                ]
                upload_positions = [
                    item.start()
                    for item in re.finditer(r"uses: actions/upload-artifact@", block)
                ]
                if not attestation_positions or not upload_positions:
                    continue
                with self.subTest(workflow=filename, job=match.group("job")):
                    self.assertLess(max(attestation_positions), min(upload_positions))

    def test_exact_source_container_validation_uses_separate_immutable_inputs(self) -> None:
        source = workflow("validate-exact-release-source.yml")
        self.assertIn("path: validation-control", source)
        self.assertIn("path: source", source)
        self.assertIn('control_dir="$GITHUB_WORKSPACE/validation-control"', source)
        self.assertIn('source_dir="$GITHUB_WORKSPACE/source"', source)
        for component in ("web", "meta-relay", "gmail-relay"):
            self.assertIn(f"validation-control/docker/release/{component}.Dockerfile", source)
        self.assertGreaterEqual(source.count('"$GITHUB_WORKSPACE/source"'), 3)
        self.assertNotIn("docker/meta-relay.Dockerfile -t", source)
        self.assertNotIn("docker/gmail-relay.Dockerfile -t", source)
        self.assertIn("release/exact-sources.json", source)
        self.assertIn("FROM scratch", source)
        self.assertNotIn("ignore-unfixed", source)
        self.assertNotIn(".trivyignore", source)

    def test_release_image_gate_requires_credential_free_exact_digest_pulls(self) -> None:
        source = workflow("build-attest-exact-release-images.yml")
        verifier = job_block(source, "verify_set")
        gate = job_block(source, "gate")
        self.assertIn("timeout-minutes: 45", verifier)
        self.assertIn(
            "Require anonymous pullability of every exact release digest", verifier
        )
        self.assertIn("DOCKER_CONFIG: ${{ runner.temp }}/anonymous-docker-config", verifier)
        self.assertIn("unset GH_TOKEN GITHUB_TOKEN CR_PAT REGISTRY_TOKEN DOCKER_AUTH_CONFIG", verifier)
        self.assertIn('[[ ! -e "$DOCKER_CONFIG" ]]', verifier)
        self.assertIn('docker pull --platform linux/amd64 "$subject"', verifier)
        self.assertIn('[[ "$pulled" -eq 3 ]]', verifier)
        self.assertIn('[[ ! -e "$DOCKER_CONFIG/config.json" ]]', verifier)
        self.assertNotIn("docker login", verifier)
        self.assertNotRegex(verifier, r"(?m)^\s+packages:\s+(?:read|write)\s*$")
        self.assertRegex(gate, r"(?m)^\s+- verify_set\s*$")
        self.assertIn("VERIFY_SET_RESULT: ${{ needs.verify_set.result }}", gate)
        self.assertIn('"$VERIFY_SET_RESULT"', gate)


class PermissionAndCredentialIsolationTests(unittest.TestCase):
    def test_normal_unlock_recovery_is_three_job_read_only_and_dual_attested(self) -> None:
        source = workflow("reconcile-production-main-lock-release.yml")
        jobs_source = source.split("\njobs:\n", 1)[1]
        self.assertEqual(
            re.findall(r"(?m)^  ([a-zA-Z0-9_-]+):\s*$", jobs_source),
            ["assertion", "observe", "gate"],
        )
        assertion = job_block(source, "assertion")
        observer = job_block(source, "observe")
        gate = job_block(source, "gate")
        expected_permissions = {
            "assertion": (
                "    permissions:\n"
                "      actions: read\n"
                "      attestations: write\n"
                "      contents: read\n"
                "      id-token: write\n"
            ),
            "observe": (
                "    permissions:\n"
                "      actions: read\n"
                "      attestations: read\n"
                "      contents: read\n"
            ),
            "gate": (
                "    permissions:\n"
                "      actions: read\n"
                "      attestations: write\n"
                "      contents: read\n"
                "      id-token: write\n"
            ),
        }
        for name, block in (
            ("assertion", assertion),
            ("observe", observer),
            ("gate", gate),
        ):
            match = re.search(r"(?m)^    permissions:\n(?:^      [^\n]+\n)+", block)
            self.assertIsNotNone(match)
            self.assertEqual(match.group(0), expected_permissions[name])
        self.assertNotRegex(assertion, r"(?m)^    environment:")
        self.assertNotIn("secrets.", assertion)
        self.assertIn("attestations: write", assertion)
        self.assertIn("id-token: write", assertion)
        self.assertIn("environment: rereply-production-orphan-observe", observer)
        self.assertIn("environment: rereply-production-orphan-observe", gate)
        self.assertEqual(
            source.count("${{ secrets.GH_PRODUCTION_BRANCH_READ_TOKEN }}"), 3
        )
        read_steps = (
            step_block(
                source,
                "Observe exact unlocked main twice with read-only branch capability",
            ),
            step_block(source, "Final-read exact unlocked main and build reconciliation"),
            step_block(
                source,
                "Recheck exact unlocked rule and fresh authority immediately before signing",
            ),
        )
        for block in read_steps:
            self.assertEqual(
                block.count("${{ secrets.GH_PRODUCTION_BRANCH_READ_TOKEN }}"), 1
            )
            self.assertIn("branchProtectionRules", block)
            self.assertNotIn("mutation(", block)
        outside_read_steps = source
        for block in read_steps:
            outside_read_steps = outside_read_steps.replace(block, "")
        self.assertNotIn("GH_PRODUCTION_BRANCH_READ_TOKEN", outside_read_steps)
        for forbidden in (
            "GH_PRODUCTION_BRANCH_LOCK_TOKEN",
            "DO_PRODUCTION_",
            "updateBranchProtectionRule",
            "mutation(",
            "lock=false",
        ):
            self.assertNotIn(forbidden, source)
        self.assertIn("http_request_count:9", source)
        self.assertIn("graphql_query_count:3", source)
        self.assertIn("branch_mutation_request_count:0", source)
        self.assertIn("mutation_text_present:false", source)

    def test_normal_unlock_jobs_have_one_token_isolated_release_and_durable_authority(self) -> None:
        for filename, operation in (
            ("apply-production-phase.yml", "apply"),
            ("rollback-production-phase.yml", "rollback"),
        ):
            with self.subTest(workflow=filename):
                source = workflow(filename)
                authority = job_block(source, "release_authorization")
                unlock = job_block(source, "unlock")
                self.assertNotIn("environment:", authority)
                self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", authority)
                self.assertEqual(unlock.count('-F lock=false'), 1)
                self.assertIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", unlock)
                self.assertIn(
                    "production-main-lock-release-authorization.sha256", authority
                )
                self.assertIn(
                    f"Authorize exact production {operation} main lock release",
                    source,
                )

    def test_terminal_normal_run_inventories_match_every_consumer(self) -> None:
        # These security-critical workflows intentionally fail closed on any
        # syntactically valid alternate YAML, shell, or JavaScript encoding.
        # A control change must update its semantic assertions and this
        # normalized-source fingerprint in the same reviewed commit.
        for filename, expected_sha256 in TERMINAL_PARITY_WORKFLOW_SHA256.items():
            with self.subTest(workflow=filename, boundary="source-fingerprint"):
                actual_sha256 = hashlib.sha256(
                    workflow(filename).encode("utf-8")
                ).hexdigest()
                self.assertEqual(actual_sha256, expected_sha256)

        producer_files = {
            "apply": "apply-production-phase.yml",
            "rollback": "rollback-production-phase.yml",
        }
        producers = {
            operation: workflow(filename)
            for operation, filename in producer_files.items()
        }
        terminal_jobs = {
            operation: job_names(source) for operation, source in producers.items()
        }
        artifact_prefixes = {
            operation: producer_artifact_prefixes(source, operation)
            for operation, source in producers.items()
        }

        for operation, source in producers.items():
            with self.subTest(operation=operation, boundary="producer"):
                producer_job_ids = job_ids(source)
                self.assertEqual(len(producer_job_ids), 9)
                self.assertEqual(len(set(producer_job_ids)), 9)
                assert_no_dynamic_job_fanout(source)
                self.assertEqual(len(terminal_jobs[operation]), 9)
                self.assertEqual(len(set(terminal_jobs[operation])), 9)
                self.assertEqual(len(artifact_prefixes[operation]), 6)
                self.assertEqual(len(set(artifact_prefixes[operation])), 6)
                self.assertIn(
                    f"Authorize exact production {operation} main lock release",
                    terminal_jobs[operation],
                )
                release_authorization = job_block(source, "release_authorization")
                unlock = job_block(source, "unlock")
                self.assertEqual(release_authorization.count(".total_count == 8"), 1)
                self.assertEqual(release_authorization.count(".total_count == 5"), 1)
                self.assertNotIn(".total_count == 9", release_authorization)
                self.assertNotIn(".total_count == 6", release_authorization)
                self.assertEqual(unlock.count(".total_count == 9"), 1)
                self.assertEqual(unlock.count(".total_count == 6"), 1)
                self.assertNotIn(".total_count == 8", unlock)
                self.assertNotIn(".total_count == 5", unlock)
                release_job = f"Release exact production {operation} main lock"
                pre_unlock_jobs = tuple(
                    name for name in terminal_jobs[operation] if name != release_job
                )
                self.assertEqual(len(pre_unlock_jobs), 8)
                self.assertEqual(
                    sorted_inventory(jq_literal_job_inventory(release_authorization)),
                    sorted_inventory(pre_unlock_jobs),
                )
                self.assertEqual(
                    sorted_inventory(jq_literal_job_inventory(unlock)),
                    sorted_inventory(terminal_jobs[operation]),
                )

        finalizer = workflow("finalize-production-orphan-lock.yml")
        finalizer_job_arrays = shell_json_array_assignments(
            finalizer, "original_jobs"
        )
        finalizer_artifact_arrays = shell_json_array_assignments(
            finalizer, "original_artifacts"
        )
        canary = workflow("verify-production-crm-canary.yml")
        canary_job_arrays = shell_json_array_assignments(canary, "expected_jobs")
        canary_artifact_arrays = shell_json_array_assignments(
            canary, "expected_artifact_prefixes"
        )
        self.assertEqual(len(finalizer_job_arrays), 6)
        self.assertEqual(len(finalizer_artifact_arrays), 3)
        self.assertEqual(len(canary_job_arrays), 4)
        self.assertEqual(len(canary_artifact_arrays), 4)
        self.assertEqual(finalizer.count("$original_jobs"), 2)
        self.assertEqual(finalizer.count("$original_artifacts"), 1)
        self.assertEqual(finalizer.count("$original_prerequisites"), 2)
        authority_call = (
            '          authenticate_subject \\\n'
            '            "$evidence_dir/intent-binding.json" '
            '"$intent_workflow_path" "$original_workflow_name" \\\n'
            '            "$original_jobs" "$original_artifacts" \\\n'
            '            "production-mutation-intent-$intent_slug" '
            '"$INTENT_PREDICATE" \\\n'
            '            "$evidence_dir/mutation-intent" orphan '
            '"$original_prerequisites"'
        )
        preauthorization_call = (
            '          reacquire_subject "$MUTATION_INTENT_BINDING_JSON" \\\n'
            '            "$ORIGINAL_WORKFLOW_PATH" "$original_name" \\\n'
            '            "production-mutation-intent-$ORIGINAL_INTENT_SLUG" '
            '"$INTENT_PREDICATE" "$sources/mutation-intent" \\\n'
            '            orphan "$original_jobs" "$original_prerequisites"'
        )
        self.assertEqual(finalizer.count(authority_call), 1)
        self.assertEqual(finalizer.count(preauthorization_call), 1)
        finalizer_authority = shell_function_block(finalizer, "authenticate_subject")
        finalizer_reacquire = shell_function_block(finalizer, "reacquire_subject")
        for function in (finalizer_authority, finalizer_reacquire):
            require_active_source_line(
                function,
                "([.jobs[].name] | sort) == ($expected | sort) and",
            )
            require_active_source_line(
                function,
                "all(.jobs[]; .status == \"completed\" and "
                ".conclusion == \"success\")",
            )
        require_active_source_line(
            finalizer_authority,
            "([.artifacts[].name] | sort) == $expected_names",
        )
        self.assertEqual(canary.count("$expected_jobs"), 1)
        self.assertEqual(canary.count("$expected_artifact_prefixes"), 1)
        self.assertEqual(
            canary.count(
                '          jq -e --argjson expected "$expected_jobs" '
                '--arg reconciled "$reconciled" --arg release_job "$release_job" \''
            ),
            1,
        )
        self.assertEqual(
            canary.count(
                '            --argjson expected_prefixes '
                '"$expected_artifact_prefixes" '
                + "\\"
            ),
            1,
        )
        canary_receipt = step_block(
            canary, "Acquire and verify the exact successful change receipt"
        )
        require_active_source_line(
            canary_receipt,
            "([.jobs[].name] | sort) == ($expected | sort) and",
        )
        require_active_source_line(
            canary_receipt,
            "all(.jobs[]; .status == \"completed\") and",
        )
        require_active_source_line(
            canary_receipt,
            "([.artifacts[].name] | sort) == $expected_names and",
        )

        finalizer_authority_jobs = {
            "apply": shell_if_arm_array(
                finalizer,
                '"$intent_operation" == "activate" && '
                '"$intent_workflow_path" == '
                '".github/workflows/apply-production-phase.yml"',
                "original_jobs",
            ),
            "rollback": shell_if_arm_array(
                finalizer,
                '"$intent_operation" == "rollback" && '
                '"$intent_workflow_path" == '
                '".github/workflows/rollback-production-phase.yml"',
                "original_jobs",
            ),
        }
        finalizer_authority_artifacts = {
            "apply": shell_if_arm_array(
                finalizer,
                '"$intent_operation" == "activate" && '
                '"$intent_workflow_path" == '
                '".github/workflows/apply-production-phase.yml"',
                "original_artifacts",
            ),
            "rollback": shell_if_arm_array(
                finalizer,
                '"$intent_operation" == "rollback" && '
                '"$intent_workflow_path" == '
                '".github/workflows/rollback-production-phase.yml"',
                "original_artifacts",
            ),
        }
        finalizer_preauthorization_jobs = {
            operation: shell_case_arm_array(finalizer, operation, "original_jobs")
            for operation in producer_files
        }
        canary_bound_jobs = {
            operation: shell_case_arm_array(
                canary,
                rf"{operation}\|{operation}-reconciled",
                "expected_jobs",
            )
            for operation in producer_files
        }
        canary_bound_artifacts = {
            operation: shell_case_arm_array(
                canary,
                rf"{operation}\|{operation}-reconciled",
                "expected_artifact_prefixes",
            )
            for operation in producer_files
        }

        reconcile = workflow("reconcile-production-orphan.yml")
        reconcile_jobs = reconcile_operation_job_arrays(reconcile)

        rollback_authority = javascript_without_block_comments(
            step_block(
                producers["rollback"], "Authenticate immutable rollback artifacts"
            )
        )
        require_active_javascript_line(
            rollback_authority,
            "if(JSON.stringify(actual)!==JSON.stringify([...expectedJobs].sort()) "
            "|| jobs.some(job=>job.status!=='completed' || "
            "job.conclusion!=='success')) throw new Error(`${kind} job inventory "
            "differs`);",
        )
        rollback_direct_apply_jobs = javascript_array_assignment(
            producers["rollback"], "applyJobs"
        )
        rollback_active_source = javascript_without_block_comments(
            producers["rollback"]
        )
        self.assertEqual(
            len(re.findall(r"\bapplyJobs\b", rollback_active_source)), 2
        )
        require_active_javascript_line(
            rollback_active_source,
            "const currentJobs=value.current_state.kind==='phase-state'?"
            "canaryJobs:applyJobs;",
        )
        require_active_javascript_line(
            rollback_active_source,
            "const currentName=await artifact(value.current_state,'current',"
            "currentWorkflow,`${currentPrefix}-${value.current_state.run_id}-1`,"
            "currentJobs);",
        )
        self.assertEqual(
            sorted_inventory(rollback_direct_apply_jobs),
            sorted_inventory(terminal_jobs["apply"]),
        )

        for operation in producer_files:
            expected_jobs = sorted_inventory(terminal_jobs[operation])
            expected_artifacts = sorted_inventory(artifact_prefixes[operation])
            with self.subTest(operation=operation, boundary="consumers"):
                self.assertEqual(
                    sorted_inventory(finalizer_authority_jobs[operation]),
                    expected_jobs,
                )
                self.assertEqual(
                    sorted_inventory(finalizer_preauthorization_jobs[operation]),
                    expected_jobs,
                )
                self.assertEqual(
                    sorted_inventory(reconcile_jobs[operation]), expected_jobs
                )
                self.assertEqual(
                    sorted_inventory(canary_bound_jobs[operation]), expected_jobs
                )
                self.assertEqual(
                    sorted_inventory(finalizer_authority_artifacts[operation]),
                    expected_artifacts,
                )
                self.assertEqual(
                    sorted_inventory(canary_bound_artifacts[operation]),
                    expected_artifacts,
                )

        for filename in ACTIVE_PRODUCTION_CONTROLS:
            with self.subTest(workflow=filename, boundary="obsolete-prefix"):
                self.assertNotIn("production-main-release-intent-", workflow(filename))

    def test_canary_observer_and_signer_have_disjoint_credentials(self) -> None:
        source = workflow("verify-production-crm-canary.yml")
        authority = job_block(source, "authority")
        observer = job_block(source, "observe")
        signer = job_block(source, "gate")
        for block, expected in (
            (
                authority,
                "    permissions:\n"
                "      actions: read\n"
                "      attestations: read\n"
                "      contents: read\n",
            ),
            (
                observer,
                "    permissions:\n"
                "      actions: read\n"
                "      contents: read\n",
            ),
            (
                signer,
                "    permissions:\n"
                "      actions: read\n"
                "      attestations: write\n"
                "      contents: read\n"
                "      id-token: write\n",
            ),
        ):
            match = re.search(r"(?m)^    permissions:\n(?:^      [^\n]+\n)+", block)
            self.assertIsNotNone(match)
            self.assertEqual(match.group(0), expected)
        self.assertIn("environment: rereply-production-canary", observer)
        self.assertIn("secrets.CRM_CANARY_PUBLIC_TARGETS_JSON", observer)
        self.assertIn("secrets.CRM_CANARY_SYNTHETIC_DRIVER_JSON", observer)
        self.assertNotIn("environment:", signer)
        self.assertNotIn("secrets.", signer)
        self.assertIn("attestations: write", signer)
        self.assertIn("id-token: write", signer)
        self.assertEqual(source.count("${{ secrets.CRM_CANARY_PUBLIC_TARGETS_JSON }}"), 1)
        self.assertEqual(source.count("secrets.CRM_CANARY_SYNTHETIC_DRIVER_JSON"), 1)
        self.assertNotRegex(
            source,
            r"GH_PRODUCTION_BRANCH_(?:READ|LOCK)_TOKEN|secrets\.DO_PRODUCTION_",
        )
        self.assertNotIn("packages: write", source)
        self.assertNotIn("deployments: write", source)
        self.assertNotRegex(
            source,
            r"(?i)\b(?:doctl|do_token|do_production|digitalocean_access_token)\b|secrets\.digitalocean",
        )

    def test_canary_is_strict_dual_kind_content_free_and_attested(self) -> None:
        source = workflow("verify-production-crm-canary.yml")
        for value in (
            "receipt_kind",
            "production-phase-apply-receipt/v1",
            "production-phase-rollback-receipt/v1",
            "gh attestation verify",
            "production-crm-canary.json",
            "production-phase-state.json",
            "Exact production phase gate",
        ):
            self.assertIn(value, source)
        self.assertNotIn("--screenshot", source)
        self.assertNotIn("--trace", source)
        self.assertNotIn("--video", source)
        self.assertNotIn("--fail-with-body", source)
        self.assertNotIn("create-deployment", source)
        self.assertNotIn("update_all_source_versions", source)
        receipt_acquisition = step_block(
            source, "Acquire and verify the exact successful change receipt"
        )
        self.assertEqual(receipt_acquisition.count("release_job=''"), 1)
        self.assertLess(
            receipt_acquisition.index("release_job=''"),
            receipt_acquisition.index('case "$RECEIPT_KIND" in'),
        )

    def test_canary_rechecks_live_main_at_each_authority_boundary(self) -> None:
        source = workflow("verify-production-crm-canary.yml")
        self.assertGreaterEqual(job_block(source, "authority").count("git/ref/heads/main"), 2)
        self.assertGreaterEqual(job_block(source, "observe").count("git/ref/heads/main"), 2)
        self.assertGreaterEqual(job_block(source, "gate").count("git/ref/heads/main"), 3)
        pre_sign = step_block(
            source, "Recheck exact main and receipt immediately before signing"
        )
        for value in (
            'actions/runs/$RECEIPT_RUN_ID',
            'actions/artifacts/$RECEIPT_ARTIFACT_ID',
            'actions/runs/$RECONCILIATION_RUN_ID',
            'actions/artifacts/$RECONCILIATION_ARTIFACT_ID',
            '.run_attempt == 1',
            '.expired == false',
        ):
            self.assertIn(value, pre_sign)

    def test_change_receipt_sidecars_are_exact_raw_hashes(self) -> None:
        for name, stem in (
            ("apply-production-phase.yml", "production-phase-apply-receipt"),
            ("rollback-production-phase.yml", "production-phase-rollback-receipt"),
        ):
            with self.subTest(workflow=name):
                source = workflow(name)
                canonical = (
                    f"sha256sum {stem}.json | awk '{{print $1}}' > {stem}.sha256"
                )
                self.assertIn(canonical, source)
                self.assertIn(
                    f'[[ "$(cat {stem}.sha256)" == "$(sha256sum {stem}.json | awk \'{{print $1}}\')" ]]',
                    source,
                )
                self.assertNotIn(f"sha256sum {stem}.json > {stem}.sha256", source)
                self.assertNotIn(f"sha256sum -c {stem}.sha256", source)

    def test_phase_state_artifact_name_is_exact_across_producer_and_consumers(self) -> None:
        canary = workflow("verify-production-crm-canary.yml")
        plan = workflow("plan-production-rollout.yml")
        apply = workflow("apply-production-phase.yml")
        rollback = workflow("rollback-production-phase.yml")
        self.assertIn(
            "name: production-phase-state-${{ github.run_id }}-${{ github.run_attempt }}",
            canary,
        )
        self.assertNotIn("production-phase-state-${{ needs.authority.outputs.phase }}", canary)
        self.assertIn(
            'expected_name="production-phase-state-${predecessor_run_id}-${predecessor_run_attempt}"',
            plan,
        )
        self.assertIn(
            'f"predecessor_name=production-phase-state-{pred[\\"run_id\\"]}-{pred[\\"run_attempt\\"]}"',
            apply,
        )
        self.assertNotIn('production-phase-state-{pred[\\"phase\\"]}', apply)
        self.assertIn(
            "`production-phase-state-${value.target_state.run_id}-1`",
            rollback,
        )
        self.assertIn(
            "value.current_state.kind==='phase-state'?'production-phase-state':'production-phase-apply'",
            rollback,
        )

    def test_provider_credentials_exist_only_in_mutation_jobs(self) -> None:
        for name, mutation_job in (
            ("apply-production-phase.yml", "apply"),
            ("rollback-production-phase.yml", "rollback"),
            ("rollback-production-orphan.yml", "rollback"),
        ):
            with self.subTest(workflow=name):
                source = workflow(name)
                self.assertIn("environment: rereply-production-apply", source)
                mutation = job_block(source, mutation_job)
                self.assertIn("secrets.DO_PRODUCTION_APPLY_TOKEN", mutation)
                outside = source.replace(mutation, "")
                self.assertNotIn("secrets.DO_PRODUCTION_APPLY_TOKEN", outside)
                self.assertNotIn("secrets.DIGITALOCEAN_ACCESS_TOKEN", source)
                self.assertNotIn("packages: write", source)
                self.assertNotIn("deployments: write", source)

    def test_branch_mutation_token_is_step_scoped_to_acquire_and_unlock(self) -> None:
        for name, acquire_name, unlock_name in (
            (
                "apply-production-phase.yml",
                "Acquire the exact main lock with one reconciled mutation",
                "Release the exact authorized apply main lock",
            ),
            (
                "rollback-production-phase.yml",
                "Acquire the exact rollback main lock with one reconciled mutation",
                "Release the exact authorized rollback main lock",
            ),
        ):
            with self.subTest(workflow=name):
                source = workflow(name)
                acquire = step_block(source, acquire_name)
                release = step_block(source, unlock_name)
                self.assertIn("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN", acquire)
                self.assertIn("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN", release)
                self.assertEqual(source.count("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN"), 2)
                outside = source.replace(acquire, "").replace(release, "")
                self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", outside)
                self.assertIn("secrets.GH_PRODUCTION_BRANCH_READ_TOKEN", outside)
        orphan = workflow("rollback-production-orphan.yml")
        self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", orphan)

    def test_read_only_and_canary_environments_are_not_reused_for_mutation(self) -> None:
        plan = workflow("plan-production-rollout.yml")
        recovery = workflow("verify-production-recovery-readiness.yml")
        apply = workflow("apply-production-phase.yml")
        rollback = workflow("rollback-production-phase.yml")
        self.assertIn("environment: rereply-production-plan", plan)
        self.assertNotIn("rereply-production-apply", plan)
        self.assertIn("environment: rereply-production-recovery", recovery)
        self.assertNotIn("rereply-production-apply", recovery)
        self.assertIn("rereply-production-apply", apply)
        self.assertIn("rereply-production-apply", rollback)

    def test_apply_and_rollback_repull_exact_tuples_without_credentials(self) -> None:
        cases = (
            (
                "apply-production-phase.yml",
                "Require fresh anonymous pullability of the exact apply tuple",
                "Apply with one isolated app-update capability",
            ),
            (
                "rollback-production-phase.yml",
                "Require fresh anonymous pullability of the exact rollback tuple",
                "Roll back with one isolated app-update capability",
            ),
        )
        for name, pull_name, mutation_name in cases:
            with self.subTest(workflow=name):
                source = workflow(name)
                pull = step_block(source, pull_name)
                mutation = step_block(source, mutation_name)
                self.assertLess(source.index(pull), source.index(mutation))
                between = source[source.index(pull) + len(pull) : source.index(mutation)]
                self.assertNotRegex(between, r"(?m)^      - name:")
                self.assertIn("unset GH_TOKEN GITHUB_TOKEN CR_PAT", pull)
                self.assertIn("unset DO_PRODUCTION_APPLY_TOKEN DO_PRODUCTION_TARGET_JSON", pull)
                self.assertIn('[[ ! -e "$anonymous_root" ]]', pull)
                self.assertIn('DOCKER_CONFIG="$anonymous_config"', pull)
                self.assertIn('HOME="$anonymous_home"', pull)
                self.assertIn("env -i", pull)
                self.assertIn('/usr/bin/docker pull --platform linux/amd64 "$image"', pull)
                self.assertIn('[[ "$pulled" -eq 3 ]]', pull)
                self.assertNotIn("secrets.", pull)
                self.assertNotIn("docker login", pull.lower())

    def test_main_lock_intent_is_durable_before_one_get_reconciled_acquisition(self) -> None:
        cases = (
            (
                "apply-production-phase.yml",
                "Create exact durable apply pre-lock marker",
                "Upload durable apply pre-lock marker",
                "Prepare exact sanitized v2 mutation intent without credentials",
                "Upload exact production mutation intent",
                "Acquire the exact main lock with one reconciled mutation",
                "Recheck the exact locked projection and build proof",
                "Require fresh anonymous pullability of the exact apply tuple",
                "Apply with one isolated app-update capability",
                "apply-main-lock.json",
            ),
            (
                "rollback-production-phase.yml",
                "Create exact durable rollback pre-lock marker",
                "Upload durable rollback pre-lock marker",
                "Prepare exact sanitized rollback mutation intent without credentials",
                "Upload exact production rollback mutation intent",
                "Acquire the exact rollback main lock with one reconciled mutation",
                "Recheck the exact rollback lock and build proof",
                "Require fresh anonymous pullability of the exact rollback tuple",
                "Roll back with one isolated app-update capability",
                "rollback-main-lock.json",
            ),
        )
        for (
            name,
            marker_name,
            marker_upload_name,
            intent_name,
            intent_upload_name,
            acquire_name,
            proof_name,
            pull_name,
            mutation_name,
            marker,
        ) in cases:
            with self.subTest(workflow=name):
                source = workflow(name)
                durable_marker = step_block(source, marker_name)
                marker_upload = step_block(source, marker_upload_name)
                intent = step_block(source, intent_name)
                intent_upload = step_block(source, intent_upload_name)
                acquire = step_block(source, acquire_name)
                proof = step_block(source, proof_name)
                pull = step_block(source, pull_name)
                mutation = step_block(source, mutation_name)
                order = [durable_marker, marker_upload, intent, intent_upload, acquire, proof, pull, mutation]
                self.assertEqual([source.index(block) for block in order], sorted(source.index(block) for block in order))
                self.assertLess(source.index(pull), source.index(mutation))
                self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", durable_marker)
                self.assertIn("lockBranch == false", durable_marker)
                self.assertIn("acquire-intent", durable_marker)
                self.assertIn(marker, durable_marker)
                self.assertIn("os.O_EXCL", durable_marker)
                self.assertIn("0o600", durable_marker)
                self.assertIn(marker, marker_upload)
                self.assertIn("retention-days: 30", marker_upload)
                self.assertIn("prepare-intent", intent)
                self.assertIn("mode':'planned'", intent)
                self.assertIn("strategy':'acquire'", intent)
                self.assertIn("retention-days: 30", intent_upload)
                self.assertIn("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN", acquire)
                self.assertEqual(acquire.count("-F lock=true"), 1)
                self.assertIn("set +e", acquire)
                self.assertIn("lockBranch == true", acquire)
                self.assertIn("build-main-lock-proof", proof)
                self.assertIn("mutation_request_count':1", proof)
                self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", proof)
                self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", mutation)
                self.assertIn("git/ref/heads/main", mutation)
                self.assertIn("/attempts/1", mutation)
                self.assertIn("validate_fresh_window", mutation)
                self.assertIn("--main-lock-proof", mutation)
                self.assertIn("lockBranch == true", mutation)
                self.assertIn("git/ref/heads/main", mutation)
                self.assertNotIn("-F lock=false", mutation)
                self.assertNotIn("state': 'acquired'", source)

    def test_signed_receipt_gate_precedes_success_only_authenticated_unlock(self) -> None:
        cases = (
            (
                "apply-production-phase.yml",
                "apply",
                "Release exact production apply main lock",
                "production-phase-apply-receipt.json",
            ),
            (
                "rollback-production-phase.yml",
                "rollback",
                "Release exact production rollback main lock",
                "production-phase-rollback-receipt.json",
            ),
        )
        for (
            name,
            mutation_job_name,
            unlock_display_name,
            receipt,
        ) in cases:
            with self.subTest(workflow=name):
                source = workflow(name)
                mutation = job_block(source, mutation_job_name)
                gate = job_block(source, "gate")
                authorization = job_block(source, "release_authorization")
                unlock = job_block(source, "unlock")
                self.assertLess(source.index(mutation), source.index(gate))
                self.assertLess(source.index(gate), source.index(authorization))
                self.assertLess(source.index(authorization), source.index(unlock))
                self.assertIn(f"name: {unlock_display_name}", unlock)
                self.assertIn(
                    f"{mutation_job_name}, gate, release_authorization]", unlock
                )
                self.assertIn(
                    f"needs.{mutation_job_name}.result == 'success'",
                    unlock,
                )
                self.assertIn("needs.gate.result == 'success'", unlock)
                self.assertIn(
                    "needs.release_authorization.result == 'success'", unlock
                )
                self.assertNotIn("always()", unlock)
                self.assertIn("environment: rereply-production-apply", unlock)
                self.assertIn("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN", unlock)
                self.assertNotRegex(unlock, r"secrets\.(?:DO|DIGITALOCEAN)[A-Z0-9_]*")
                self.assertNotIn("DO_PRODUCTION_APPLY_TOKEN", unlock)
                self.assertIn(receipt, unlock)
                self.assertGreaterEqual(unlock.count("attestation verify"), 2)
                self.assertIn(".total_count == 9", unlock)
                self.assertIn(".total_count == 6", unlock)
                self.assertIn("artifact-ids:", unlock)
                release = step_block(
                    unlock,
                    f"Release the exact authorized {mutation_job_name} main lock",
                )
                self.assertIn(
                    "production-main-lock-release-authorization.json", release
                )
                self.assertGreaterEqual(release.count(" validate \\"), 2)
                self.assertIn("--receipt", release)
                self.assertLess(
                    release.index("before="), release.rindex(" validate \\")
                )
                self.assertLess(
                    release.rindex(" validate \\"),
                    release.index('response="$(mutate_rule'),
                )
                self.assertIn("lockBranch == true", unlock)
                self.assertEqual(unlock.count("-F lock=false"), 1)
                self.assertLess(release.index("set +e"), release.index("-F lock=false"))
                self.assertLess(release.index("-F lock=false"), release.index("mutation_status=$?"))
                self.assertLess(release.index("mutation_status=$?"), release.index("after="))
                self.assertIn('if [[ "$mutation_status" -eq 0 ]]', release)
                self.assertIn("lockBranch == false", release)
                self.assertIn("retention-days: 30", authorization)
                self.assertIn(
                    "production-main-lock-release-authorization.sha256",
                    authorization,
                )
                self.assertGreaterEqual(authorization.count("actions/attest"), 2)
                self.assertNotIn("-F lock=false", source.replace(unlock, ""))

    def test_protected_target_never_appears_in_process_argv(self) -> None:
        for name, mutation_name in (
            ("apply-production-phase.yml", "Apply with one isolated app-update capability"),
            ("rollback-production-phase.yml", "Roll back with one isolated app-update capability"),
        ):
            with self.subTest(workflow=name):
                source = workflow(name)
                mutation = step_block(source, mutation_name)
                self.assertIn('os.environ.pop("DO_PRODUCTION_TARGET_JSON")', mutation)
                self.assertIn("controller.run_cli(arguments)", mutation)
                self.assertRegex(mutation, r'["\']--target["\']\s*,\s*target')
                self.assertNotRegex(source, r'--target\s+"\$(?:TARGET_JSON|DO_PRODUCTION_TARGET_JSON)"')
                self.assertNotRegex(
                    mutation,
                    r'env -i[^\n]*DO_PRODUCTION_(?:APPLY_TOKEN|TARGET_JSON)=',
                )

    def test_apply_independently_verifies_plan_attestations_and_current_controls(self) -> None:
        source = workflow("apply-production-phase.yml")
        apply = job_block(source, "apply")
        verification = step_block(
            apply,
            "Reverify the exact production plan before capability use",
        )
        self.assertIn("attestations: read", apply)
        self.assertEqual(verification.count("attestation verify"), 2)
        self.assertIn("https://slsa.dev/provenance/v1", verification)
        self.assertIn("$PLAN_PREDICATE", verification)
        self.assertIn("production-app-contract.json", verification)
        self.assertIn("verify_production_plan.py", verification)
        self.assertIn("contract_sha256", verification)
        self.assertIn("verifier_sha256", verification)
        self.assertIn("production plan sidecar differs", verification)

    def test_attestation_signers_are_full_repository_workflow_identities(self) -> None:
        for name in ACTIVE_PRODUCTION_CONTROLS:
            with self.subTest(workflow=name):
                source = workflow(name)
                for bad in (
                    '--signer-workflow "$WORKFLOW_PATH"',
                    '--signer-workflow "$ORIGINAL_WORKFLOW_PATH"',
                    '--signer-workflow "$workflow"',
                    '--signer-workflow "$signer_path"',
                ):
                    self.assertNotIn(bad, source)
        for name in (
            "apply-production-phase.yml",
            "rollback-production-phase.yml",
            "reconcile-production-orphan.yml",
            "rollback-production-orphan.yml",
        ):
            with self.subTest(exact_producer=name):
                source = workflow(name)
                self.assertIn("--signer-workflow", source)
                self.assertRegex(
                    source,
                    r'--signer-workflow "\$(?:RELEASE_REPOSITORY/[^"\n]+|signer)"',
                )
        reconcile = workflow("reconcile-production-orphan.yml")
        self.assertIn(
            '--signer-workflow "$RELEASE_REPOSITORY/$ORIGINAL_WORKFLOW_PATH"',
            reconcile,
        )

    def test_upload_digests_are_canonicalized_before_authority_handoff(self) -> None:
        cases = {
            "aggregate-exact-four-phase-rollout.yml": (
                "Canonicalize exact rollout capsule artifact digest",
            ),
            "plan-production-rollout.yml": (
                "Canonicalize verified rollout-input artifact digest",
                "Canonicalize verified production-plan artifact digest",
            ),
            "apply-production-phase.yml": (
                "Canonicalize apply pre-lock marker artifact digest",
                "Canonicalize apply mutation-intent artifact digest",
                "Canonicalize apply main-lock-proof artifact digest",
            ),
            "rollback-production-phase.yml": (
                "Canonicalize rollback pre-lock marker artifact digest",
                "Canonicalize rollback mutation-intent artifact digest",
                "Canonicalize rollback main-lock-proof artifact digest",
            ),
            "reconcile-production-orphan.yml": (
                "Canonicalize main-lock-assertion artifact digest",
            ),
            "rollback-production-orphan.yml": (
                "Canonicalize orphan rollback mutation-intent artifact digest",
                "Canonicalize orphan rollback main-lock-proof artifact digest",
            ),
        }
        for name, steps in cases.items():
            with self.subTest(workflow=name):
                source = workflow(name)
                self.assertNotIn(
                    "artifact_digest: ${{ steps.upload.outputs.artifact-digest }}",
                    source,
                )
                self.assertNotRegex(
                    source,
                    r"(?m)^\s+[a-z_]+_artifact_digest:\s+\$\{\{ steps\.upload\.outputs\.artifact-digest \}\}\s*$",
                )
                for step_name in steps:
                    block = step_block(source, step_name)
                    self.assertIn(
                        "RAW_ARTIFACT_DIGEST: ${{ steps.upload.outputs.artifact-digest }}",
                        block,
                    )
                    self.assertIn(
                        '[[ "$RAW_ARTIFACT_DIGEST" =~ ^[0-9a-f]{64}$ ]]', block
                    )
                    self.assertIn(
                        "artifact_digest=sha256:%s", block
                    )
                    self.assertNotIn("sha256:sha256:", block)

    def test_orphan_rollback_retains_inherited_lock_and_has_exact_chain(self) -> None:
        source = workflow("rollback-production-orphan.yml")
        jobs = [
            "Authenticate exact production orphan rollback authority",
            "Prepare and attest exact production orphan rollback mutation intent",
            "Attest exact production orphan rollback main lock proof",
            "Roll back exact production orphan",
            "Exact production orphan rollback receipt gate",
        ]
        positions = [source.index(f"name: {name}") for name in jobs]
        self.assertEqual(positions, sorted(positions))
        self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", source)
        self.assertNotIn("-F lock=true", source)
        self.assertNotIn("-F lock=false", source)
        self.assertIn("strategy']='inherit'", source)
        self.assertIn("already-locked-inherited", source)
        self.assertIn("mutation_request_count':0", source)
        self.assertIn("Roll back with one inherited-lock app-update capability", source)
        self.assertIn("expected=['gmail-relay','meta-relay','web']", source)
        self.assertIn("set(item)!={'component','repository','digest','subject'}", source)
        self.assertNotIn("'omnitech-web'", source)
        self.assertIn("validate_orphan_rollback_receipt", source)
        self.assertNotIn(
            "r.validate_rollback_receipt(v.load_json(p,\"orphan rollback receipt\"))",
            source,
        )
        self.assertEqual(source.count("secrets.DO_PRODUCTION_APPLY_TOKEN"), 1)
        for prefix in (
            "production-mutation-intent-orphan-rollback-",
            "production-main-lock-proof-orphan-rollback-",
            "unsigned-production-orphan-rollback-",
            "production-orphan-rollback-${{ github.run_id }}-",
        ):
            self.assertIn(prefix, source)

    def test_reconciliation_signs_exact_provider_job_never_started_projection(self) -> None:
        source = workflow("reconcile-production-orphan.yml")
        auth = step_block(
            source, "Authenticate the exact locked run intent and optional signed receipt"
        )
        assertion = step_block(
            source, "Build the exact hash-only single-operator lock assertion"
        )
        for name in (
            "Apply exact production phase",
            "Roll back exact production phase",
            "Roll back exact production orphan",
        ):
            self.assertIn(name, auth)
        for key in (
            "job_id",
            "job_name",
            "status",
            "conclusion",
            "started_at",
            "completed_at",
            "steps",
        ):
            self.assertIn(key, auth)
        self.assertIn("original_provider_job_b64", auth)
        self.assertIn("original_provider_job", assertion)
        self.assertNotIn("never_started", auth)
        self.assertNotIn("never_started", assertion)
        self.assertIn("prepare-lock-assertion", assertion)

    def test_orphan_finalizer_has_exact_four_job_three_artifact_chain(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        expected_jobs = (
            "Authenticate exact production orphan finalization authority",
            "Prepare and attest exact production orphan lock finalization",
            "Release exact production orphan main lock",
            "Exact production orphan lock release receipt gate",
        )
        self.assertEqual(
            re.findall(r"(?m)^    name: (.+)$", source), list(expected_jobs)
        )
        for prefix in (
            "production-orphan-lock-finalization-${{ github.run_id }}-${{ github.run_attempt }}",
            "unsigned-production-orphan-lock-release-${{ github.run_id }}-${{ github.run_attempt }}",
            "production-orphan-lock-release-${{ github.run_id }}-${{ github.run_attempt }}",
        ):
            self.assertEqual(source.count(f"name: {prefix}"), 1)
        self.assertIn("production-orphan-lock-finalization/v1", source)
        self.assertIn("production-orphan-lock-release-receipt/v1", source)
        self.assertIn("retention-days: 30", source)
        self.assertEqual(source.count("retention-days: 30"), 3)

    def test_orphan_finalizer_preserves_semantic_operation_and_exact_signer_slug(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        authority = job_block(source, "authority")
        preauthorization = job_block(source, "preauthorization")
        for value in (
            '"$intent_operation" == "activate" && "$intent_workflow_path" == ".github/workflows/apply-production-phase.yml"',
            '"$intent_operation" == "rollback" && "$intent_workflow_path" == ".github/workflows/rollback-production-phase.yml"',
            '"$intent_operation" == "rollback" && "$intent_workflow_path" == "$ORPHAN_ROLLBACK_WORKFLOW_PATH"',
            "intent_slug=apply",
            "intent_slug=rollback",
            "intent_slug=orphan-rollback",
            "original_intent_slug",
        ):
            self.assertIn(value, authority)
        self.assertIn('case "$ORIGINAL_INTENT_SLUG" in', preauthorization)
        self.assertIn('"production-mutation-intent-$ORIGINAL_INTENT_SLUG"', preauthorization)
        self.assertIn(
            '"$sources/mutation-intent/production-mutation-intent-$ORIGINAL_INTENT_SLUG.json"',
            preauthorization,
        )
        self.assertIn("--arg original_operation \"$ORIGINAL_OPERATION\"", preauthorization)
        self.assertNotIn("production-mutation-intent-$ORIGINAL_OPERATION", preauthorization)

    def test_orphan_finalizer_lock_token_is_one_step_scoped_and_provider_free(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        release = step_block(
            source, "Release with one exact branch-rule mutation and GET reconciliation"
        )
        self.assertEqual(source.count("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN"), 1)
        self.assertIn("secrets.GH_PRODUCTION_BRANCH_LOCK_TOKEN", release)
        self.assertEqual(source.count("lockBranch:false"), 1)
        self.assertIn('["query","mutation","query"]', release)
        self.assertIn("mutation_request_count: 1", release)
        self.assertIn("confirm_production_orphan_lock_release", source)
        self.assertIn("dt.datetime.now(dt.timezone.utc)", release)
        self.assertIn("expires - checked < dt.timedelta(seconds=120)", release)
        self.assertIn('if [[ "$release_status" -eq 0 ]] && jq -e', release)
        self.assertIn('> /dev/null 2>&1; then', release)
        self.assertLess(
            release.index('if [[ "$release_status" -eq 0 ]] && jq -e'),
            release.index('> "$RUNNER_TEMP/release-post-rule.json"'),
        )
        self.assertNotRegex(source, r"secrets\.(?:DO|DIGITALOCEAN)[A-Z0-9_]*")
        self.assertNotIn("DO_PRODUCTION_", source)
        self.assertNotIn("doctl", source.lower())
        self.assertNotIn("create-deployment", source)

    def test_orphan_finalizer_competing_set_matches_shared_concurrency_workflows(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        self.assertIn('"Reconcile Production Main Lock Release"', source)
        self.assertEqual(
            source.count(
                '".github/workflows/reconcile-production-main-lock-release.yml"'
            ),
            2,
        )
        expected_names = {
            workflow(path.name).splitlines()[0].removeprefix("name: ")
            for path in WORKFLOWS.glob("*.yml")
            if "group: rereply-production" in workflow(path.name)
        }
        expected_paths = {
            f'.github/workflows/{path.name}'
            for path in WORKFLOWS.glob("*.yml")
            if "group: rereply-production" in workflow(path.name)
        }
        for name in expected_names:
            self.assertIn(f'"{name}"', source)
        for path in expected_paths:
            self.assertIn(f'"{path}"', source)

    def test_orphan_finalizer_revalidates_signed_pre_and_post_authorities(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        preauthorization = job_block(source, "preauthorization")
        release = job_block(source, "release")
        gate = job_block(source, "gate")
        self.assertIn("$RELEASE_REPOSITORY/$workflow_path", preauthorization)
        self.assertIn("--deny-self-hosted-runners", preauthorization)
        self.assertIn("$RELEASE_REPOSITORY/$WORKFLOW_PATH", release)
        self.assertIn("$RELEASE_REPOSITORY/$WORKFLOW_PATH", gate)
        self.assertIn("$FINALIZER_PATH\" validate", release)
        self.assertIn("validate_finalization_authorization(authorization,now=completed)", gate)
        self.assertIn("$RELEASE_CONFIRMATION_PATH\" validate", gate)
        self.assertGreaterEqual(source.count("--preauthorization-authority"), 3)
        self.assertIn("production-orphan-lock-finalization.sha256", release)
        self.assertIn("production-orphan-lock-release-receipt.sha256", gate)
        self.assertIn("lockBranch == false", gate)
        final_upload = gate.index("Upload exact confirmed lock-release receipt")
        self.assertLess(gate.index("Attest confirmed lock-release receipt provenance"), final_upload)
        self.assertLess(gate.index("Attest exact confirmed lock-release receipt policy"), final_upload)

    def test_orphan_finalizer_reauthenticates_root_and_latest_source_attempts(self) -> None:
        source = workflow("finalize-production-orphan-lock.yml")
        authority = job_block(source, "authority")
        preauthorization = job_block(source, "preauthorization")
        release = job_block(source, "release")
        gate = job_block(source, "gate")
        for block in (authority, preauthorization):
            self.assertIn("root_acquire_intent", block)
            self.assertIn("production-main-lock-apply-$run_id-1", block)
            self.assertIn("production-main-lock-rollback-$run_id-1", block)
            self.assertIn('unzip -Z1 "$output_dir/artifact.zip" | wc -l', block)
            self.assertIn('.state == "acquire-intent"', block)
        for block, prefix in ((release, "release"), (gate, "gate")):
            self.assertIn(".orphan.root_acquire_intent.run_id", block)
            self.assertIn(f'{prefix}-root-run.json', block)
            self.assertIn(f'{prefix}-root-artifact.json', block)
            self.assertIn(".run_attempt == 1", block)
            self.assertIn(".authorities[] | select(. != null) | .binding", block)
            self.assertIn(f'{prefix}-source-run-$source_run_id.json', block)
            self.assertIn(f'{prefix}-source-artifact-$source_artifact_id.json', block)
        release_mutation = release.index("GH_TOKEN=\"$branch_lock_token\" gh api graphql")
        self.assertLess(release.index("release-root-run.json"), release_mutation)
        self.assertLess(release.index("$FINALIZER_PATH\" validate", release.index("release_mutation=")), release_mutation)
        final_rule_read = release.index('> "$RUNNER_TEMP/release-pre-rule.json"')
        freshness = release.index("expires - checked < dt.timedelta(seconds=120)")
        self.assertLess(release.index("release-source-artifact-$source_artifact_id.json"), freshness)
        self.assertLess(freshness, final_rule_read)
        self.assertLess(final_rule_read, release_mutation)
        self.assertNotIn("release-pre-rule.json", release[: release.index("release_mutation=")])

    def test_lock_release_reconciliation_is_exact_three_job_observation_chain(self) -> None:
        source = workflow("reconcile-production-orphan-lock-release.yml")
        expected_jobs = (
            "Authenticate and attest exact incomplete finalizer assertion",
            "Observe exact orphan lock release with read-only capability",
            "Exact production orphan lock release reconciliation gate",
        )
        self.assertEqual(re.findall(r"(?m)^    name: (.+)$", source), list(expected_jobs))
        for name in (
            "production-orphan-lock-release-assertion-${{ github.run_id }}-${{ github.run_attempt }}",
            "unsigned-production-orphan-lock-release-reconciliation-${{ github.run_id }}-${{ github.run_attempt }}",
            "production-orphan-lock-release-reconciliation-${{ github.run_id }}-${{ github.run_attempt }}",
        ):
            self.assertEqual(source.count(f"name: {name}"), 1)
        self.assertEqual(source.count("retention-days: 30"), 3)
        self.assertIn("production-orphan-lock-release-assertion/v1", source)
        self.assertIn("production-orphan-lock-release-reconciliation/v1", source)
        gate = job_block(source, "gate")
        final_upload = gate.index("Upload exact signed lock-release reconciliation")
        self.assertLess(gate.index("Attest lock-release reconciliation provenance"), final_upload)
        self.assertLess(gate.index("Attest exact lock-release reconciliation policy"), final_upload)

    def test_lock_release_reconciliation_has_read_only_capability_boundary(self) -> None:
        source = workflow("reconcile-production-orphan-lock-release.yml")
        observe = step_block(source, "Observe exact unlocked main twice with read-only branch capability")
        gate_read = step_block(source, "Recheck exact unlocked rule immediately before signing")
        self.assertEqual(source.count("secrets.GH_PRODUCTION_BRANCH_READ_TOKEN"), 2)
        for block in (observe, gate_read):
            self.assertIn("secrets.GH_PRODUCTION_BRANCH_READ_TOKEN", block)
            self.assertIn("unset GITHUB_READ_TOKEN GH_PRODUCTION_BRANCH_READ_TOKEN", block)
            self.assertIn("branchProtectionRules", block)
            self.assertNotIn("mutation(", block)
        self.assertNotIn("GH_PRODUCTION_BRANCH_LOCK_TOKEN", source)
        self.assertNotIn("DO_PRODUCTION_", source)
        self.assertNotIn("DIGITALOCEAN", source)
        self.assertNotIn("doctl", source.lower())
        self.assertNotIn("lockBranch:false", source)
        self.assertNotIn("updateBranchProtectionRule", source)

    def test_lock_release_reconciliation_binds_failed_finalizer_and_double_read(self) -> None:
        source = workflow("reconcile-production-orphan-lock-release.yml")
        assertion = job_block(source, "assertion")
        observe = job_block(source, "observe")
        gate = job_block(source, "gate")
        for name in (
            "Authenticate exact production orphan finalization authority",
            "Prepare and attest exact production orphan lock finalization",
            "Release exact production orphan main lock",
            "Exact production orphan lock release receipt gate",
        ):
            self.assertIn(name, assertion)
        self.assertIn('(.conclusion | IN("failure","cancelled","timed_out"))', assertion)
        self.assertIn("receipt_truth:$receipt_truth[0]", assertion)
        for classification in (
            "preauthorization-only",
            "unsigned-unattested",
            "attested-receipt-upload-incomplete",
        ):
            self.assertIn(classification, source)
        self.assertGreaterEqual(source.count("receipt-provenance-query.json"), 3)
        self.assertGreaterEqual(source.count("receipt-policy-query.json"), 3)
        self.assertIn("Recheck terminal source receipt truth immediately before signing", gate)
        self.assertIn("The original finalizer already has an exact signed release receipt", gate)
        self.assertIn("Final source receipt truth recheck", gate)
        self.assertIn("Observation SHA-256", gate)
        self.assertIn("source_receipt_truth:$source_receipt_truth[0]", observe)
        self.assertLess(
            gate.index("Recheck terminal source receipt truth immediately before signing"),
            gate.index("Recheck exact unlocked rule immediately before signing"),
        )
        self.assertLess(
            gate.index("Recheck exact unlocked rule immediately before signing"),
            gate.index("Attest lock-release reconciliation provenance"),
        )
        self.assertEqual(
            source.count(".total_count == 4 and (.jobs | length) == 4"), 3
        )
        self.assertIn("static_max_branch_mutations:1", assertion)
        self.assertIn("RECONCILE UNLOCKED PRODUCTION", assertion)
        self.assertIn("for round in 1 2", observe)
        self.assertIn("http_request_count:4", observe)
        self.assertIn("graphql_query_count:2", observe)
        self.assertIn("branch_mutation_request_count:0", observe)
        self.assertIn("mutation_text_present:false", observe)
        self.assertIn("validate-reconciliation", observe)
        self.assertIn("--preauthorization-authority", observe)
        self.assertIn("validate-reconciliation", gate)
        self.assertIn("--preauthorization-authority", gate)
        for block in (observe, gate):
            self.assertIn("$RELEASE_REPOSITORY/$FINALIZER_WORKFLOW_PATH", block)
            self.assertIn("$RELEASE_REPOSITORY/$WORKFLOW_PATH", block)
            self.assertIn("--deny-self-hosted-runners", block)


class DisabledLegacyWorkflowTests(unittest.TestCase):
    def test_legacy_deploy_and_release_are_fail_closed(self) -> None:
        for name in ("deploy-production.yml", "release.yml"):
            with self.subTest(workflow=name):
                source = workflow(name)
                self.assertRegex(source, r"(?m)^  workflow_dispatch:\s*$")
                self.assertIn("exit 1", source)
                self.assertNotIn("secrets.", source)
                self.assertNotRegex(source, r"(?i)\bdoctl\b")
                self.assertNotIn("create-deployment", source)
                self.assertNotIn("packages: write", source)
                self.assertNotIn("deployments: write", source)
                self.assertNotIn("environment: production", source)


if __name__ == "__main__":
    unittest.main()
