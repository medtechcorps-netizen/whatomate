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
    "prepare-production-valkey-recovery-fork.yml",
    "verify-production-recovery-readiness.yml",
    "apply-production-phase.yml",
    "verify-production-crm-canary.yml",
    "rollback-production-phase.yml",
    "reconcile-production-orphan.yml",
    "rollback-production-orphan.yml",
    "finalize-production-orphan-lock.yml",
    "reconcile-production-orphan-lock-release.yml",
    "reconcile-production-main-lock-release.yml",
    "cleanup-production-valkey-recovery-fork.yml",
)
AUXILIARY_PRODUCTION_CONTROLS = (
    "publish-attest-production-crm-canary-driver.yml",
)
PINNED_ACTION = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}(?:\s+#.*)?$")
TERMINAL_PARITY_WORKFLOW_SHA256 = {
    "apply-production-phase.yml": (
        "fbb3757e5eec112b78a86e75a851cfe46fdc58e0fed9aa08b115dc9b7869b2f1"
    ),
    "rollback-production-phase.yml": (
        "33e97fb2b723e34c4539022b07433ac410deda40e33af0d61985a9c2c0361663"
    ),
    "finalize-production-orphan-lock.yml": (
        "170779c1e1885b4b5eeecdd34e33910f77544ee03c81c6930b4eb11c98e37433"
    ),
    "reconcile-production-orphan.yml": (
        "97abfef7db4cfa729aba1e491ea45e4a4b1e63fa33e3908f6d86c9fa76b346da"
    ),
    "reconcile-production-main-lock-release.yml": (
        "069c7eefa27b3a9159bb41d870f1c871db55b37e4b052061392b0306617b5a99"
    ),
    "verify-production-crm-canary.yml": (
        "75f1396d79de838602aaf3ceb7429ff89d90e4d5d44cfe103e63b7745a61b07e"
    ),
}
EXACT_IMAGE_BUILD_ACTION = (
    "docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8 # v6"
)
EXACT_RELEASE_IMAGE_WORKFLOW_SHA256 = (
    "8c0b7eccb22a5cc0f64ec40f58203da01455bb8f847cbc7aaf979a2f777812ff"
)
EXACT_CRM_CANARY_DRIVER_PUBLISHER_SHA256 = (
    "97fb1ee75c363914dcf7ba5baa25c51ea7e6ba0090151e26b305d9eed79c8357"
)
EXACT_IMAGE_GATE_STEP_SHA256 = (
    "1b4bf101f1756d43193ccc0050cf44bb9dd22df25302e084c9a9a91ede2db4a5"
)
EXACT_IMAGE_AUTHORITY_MATRIX_STEP_SHA256 = (
    "7ee856b9d2567f683ffa0a45b0b5664ed2f78914390b237c25c15bd5e290d6f4"
)
EXACT_IMAGE_UPLOAD_NAMES = (
    "image-${{ needs.authority.outputs.phase }}-${{ matrix.component }}-"
    "${{ github.run_id }}-${{ github.run_attempt }}",
    "scanned-${{ needs.authority.outputs.phase }}-${{ matrix.component }}-"
    "${{ github.run_id }}-${{ github.run_attempt }}",
    "attested-${{ needs.authority.outputs.phase }}-${{ matrix.component }}-"
    "${{ github.run_id }}-${{ github.run_attempt }}",
    "verified-${{ needs.authority.outputs.phase }}-${{ matrix.component }}-"
    "${{ github.run_id }}-${{ github.run_attempt }}",
    "release-set-${{ needs.authority.outputs.phase }}-${{ github.run_id }}-"
    "${{ github.run_attempt }}",
    "verified-release-set-${{ needs.authority.outputs.phase }}-"
    "${{ github.run_id }}-${{ github.run_attempt }}",
)
EXACT_AGGREGATE_ARTIFACT_BOUNDARY_SHA256 = {
    "Verify all four exact image runs and acquire immutable evidence": (
        "97b71fcdffaba1f0832042d97e31b351686bb026723a20be6e2974f2ee275742"
    ),
    "Reverify all run attempts and exact artifact records": (
        "b91069173df06f0dbdf2b0bcaade40bf6555bec45cb52a2e7acb3be114d15766"
    ),
}
EXACT_GATE_B_TEST_WORKFLOW_SHA256 = (
    "88cd01af3d37deda224bae886cedaf806611f3b43f089c3a41142c1f015c701c"
)
EXACT_GATE_B_TEST_JOB_SHA256 = {
    "go-race": "0aa5bc14dd3d264191134048bc4e9dc6b50f77b58b13d3f596dadcd93a1c1a98",
    "lint": "acff25313c68d1a86740a8cafe808480a5ca1a418c8e48e47d17c8d34285a8fd",
    "security": "ed9572d5895abf26417ce1ebf87970cc67b41be00379d42df75df58f73c8cbd4",
    "recovery-boundary-images": (
        "90daa97f1350ea5ec53dfc0b86416138fc4737928e6a7d089c33276aa36eaea2"
    ),
    "test": "4cb797b5003caf4dcc2599733d839093b3874c60a17eb9eab1c2620e91d97c1f",
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


def normalized_active_lines(source: str) -> tuple[str, ...]:
    return tuple(
        candidate.strip()
        for candidate in source.splitlines()
        if candidate.strip() and not candidate.lstrip().startswith("#")
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


def normalized_active_sha256(source: str) -> str:
    canonical = "\n".join(normalized_active_lines(source)).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def assert_gate_b_test_workflow(source: str) -> None:
    if hashlib.sha256(source.encode("utf-8")).hexdigest() != (
        EXACT_GATE_B_TEST_WORKFLOW_SHA256
    ):
        raise AssertionError("Gate-B Test workflow bytes differ")
    if exact_yaml_mapping_active_lines(source, 0, "permissions") != (
        "permissions:",
        "contents: read",
    ):
        raise AssertionError("Test workflow permissions differ from contents:read")

    expected_jobs = (
        "tenant-isolation",
        "go-race",
        "lint",
        "build",
        "security",
        "recovery-boundary-images",
        "test",
    )
    if job_ids(source) != expected_jobs:
        raise AssertionError("Gate-B Test workflow job inventory differs")
    if not canonical_workflow_step_uses_refs(source):
        raise AssertionError("Gate-B Test workflow has no pinned external actions")

    for job, expected_sha256 in EXACT_GATE_B_TEST_JOB_SHA256.items():
        actual_sha256 = normalized_active_sha256(job_block(source, job))
        if actual_sha256 != expected_sha256:
            raise AssertionError(f"Gate-B job bytes differ: {job}")

    go_race = job_block(source, "go-race")
    gate_a = step_block(go_race, "Verify Gate-A recovery boundary")
    for line in (
        'PYTHONDONTWRITEBYTECODE: "1"',
        'PYTHONHASHSEED: "0"',
        "python3 -B -m unittest discover -s prototype/recovery-boundary/tests "
        "-p 'test_*.py' -v",
        "python3 -B prototype/recovery-boundary/verify_gate_a.py",
    ):
        require_active_source_line(gate_a, line)
    run_tests = step_block(go_race, "Run tests")
    for line in (
        'package_output="$(go list -mod=readonly ./...)"',
        "mapfile -t packages < <(printf '%s\\n' \"$package_output\")",
        '-- -mod=readonly -race -p 1 -timeout 60m '
        '-coverprofile=coverage.out "${packages[@]}"',
    ):
        require_active_source_line(run_tests, line)
    if "grep -v /test/" in run_tests or " -race " not in run_tests:
        raise AssertionError("root-module race coverage is weakened")

    lint = step_block(
        job_block(source, "lint"), "Lint workflows and inert recovery templates"
    )
    for line in (
        "go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7",
        "actionlint .github/workflows/test.yml",
        "actionlint -ignore '\"on\" section should not be empty' "
        "prototype/recovery-boundary/workflows/*.tmpl",
    ):
        require_active_source_line(lint, line)

    security = job_block(source, "security")
    frontend_audit = step_block(security, "Audit frontend dependencies")
    frontend_unit = step_block(security, "Test frontend unit suite")
    if normalized_active_lines(frontend_unit) != (
        "- name: Test frontend unit suite",
        "working-directory: frontend",
        "run: npx --no-install vitest run src",
    ):
        raise AssertionError("frontend unit test step differs")
    driver_protocol_test = step_block(security, "Test CRM canary driver protocol")
    if normalized_active_lines(driver_protocol_test) != (
        "- name: Test CRM canary driver protocol",
        "run: node --test frontend/canary-driver/driver.test.mjs",
    ):
        raise AssertionError("CRM canary driver protocol test differs")
    driver_build = step_block(security, "Build CRM canary driver container")
    if normalized_active_lines(driver_build) != (
        "- name: Build CRM canary driver container",
        "run: docker build -f docker/crm-canary-driver.Dockerfile "
        "-t rereply-crm-canary-driver:ci .",
    ):
        raise AssertionError("CRM canary driver image build differs")
    scan_policy_guard = step_block(security, "Reject ambient Trivy suppression policy")
    if normalized_active_lines(scan_policy_guard) != (
        "- name: Reject ambient Trivy suppression policy",
        "run: |",
        "set -euo pipefail",
        "for path in .trivyignore .trivyignore.yaml trivy.yaml trivy.yml; do",
        '[[ ! -e "$path" && ! -L "$path" ]]',
        "done",
    ):
        raise AssertionError("ambient Trivy suppression guard differs")
    for name in (
        ".trivyignore",
        ".trivyignore.yaml",
        "trivy.yaml",
        "trivy.yml",
    ):
        path = ROOT / name
        if path.exists() or path.is_symlink():
            raise AssertionError(f"ambient Trivy suppression policy exists: {name}")
    driver_scan = step_block(security, "Scan CRM canary driver container")
    if normalized_active_lines(driver_scan) != (
        "- name: Scan CRM canary driver container",
        "uses: aquasecurity/trivy-action@"
        "a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0",
        "with:",
        "image-ref: rereply-crm-canary-driver:ci",
        "format: table",
        'exit-code: "1"',
        "vuln-type: os,library",
        "severity: CRITICAL,HIGH",
        "scanners: vuln",
    ):
        raise AssertionError("CRM canary driver scan differs")
    production_scan = step_block(security, "Scan production container")
    if not (
        security.index(frontend_audit)
        < security.index(frontend_unit)
        < security.index(driver_protocol_test)
        < security.index(driver_build)
        < security.index(scan_policy_guard)
        < security.index(production_scan)
        < security.index(driver_scan)
    ):
        raise AssertionError("CRM canary driver test/build/scan order differs")
    if canonical_workflow_step_uses_refs(driver_scan) != (
        "aquasecurity/trivy-action@a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8",
    ):
        raise AssertionError("CRM canary driver Trivy action pin differs")
    for line in (
        "image-ref: rereply-crm-canary-driver:ci",
        "format: table",
        'exit-code: "1"',
        "vuln-type: os,library",
        "severity: CRITICAL,HIGH",
        "scanners: vuln",
    ):
        require_active_source_line(driver_scan, line)
    for forbidden in (
        "continue-on-error:",
        "ignore-unfixed:",
        "ignorefile:",
        "skip-dirs:",
        "skip-files:",
    ):
        if forbidden in driver_scan:
            raise AssertionError(f"CRM canary driver scan is suppressed: {forbidden}")
    if "TRIVY_" in source:
        raise AssertionError("CRM canary driver scan has an ambient Trivy override")

    images = job_block(source, "recovery-boundary-images")
    expected_matrix = (
        (
            "writer-authority",
            "prototype/recovery-boundary/docker/writer-authority.Dockerfile",
            "recovery-boundary-writer-authority:ci",
        ),
        (
            "writer-broker",
            "prototype/recovery-boundary/docker/writer-broker.Dockerfile",
            "recovery-boundary-writer-broker:ci",
        ),
        (
            "observer-authority",
            "prototype/recovery-boundary/docker/observer-authority.Dockerfile",
            "recovery-boundary-observer-authority:ci",
        ),
        (
            "observer-broker",
            "prototype/recovery-boundary/docker/observer-broker.Dockerfile",
            "recovery-boundary-observer-broker:ci",
        ),
    )
    matrix = tuple(
        match.groups()
        for match in re.finditer(
            r"(?m)^          - role: ([a-z-]+)$\n"
            r"^            dockerfile: ([A-Za-z0-9_./-]+)$\n"
            r"^            image: ([a-z0-9:-]+)$",
            images,
        )
    )
    if matrix != expected_matrix:
        raise AssertionError("Gate-B image matrix differs")
    require_active_source_line(images, "fail-fast: false")
    build_image = step_block(images, "Build inert recovery boundary image")
    for line in (
        "docker build --pull --no-cache --platform linux/amd64",
        '-f "${{ matrix.dockerfile }}"',
        '-t "${{ matrix.image }}" .',
    ):
        require_active_source_line(build_image, line)
    scan_image = step_block(images, "Scan inert recovery boundary image")
    if canonical_workflow_step_uses_refs(scan_image) != (
        "aquasecurity/trivy-action@a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8",
    ):
        raise AssertionError("Gate-B Trivy action pin differs")
    for line in (
        "image-ref: ${{ matrix.image }}",
        "format: table",
        'exit-code: "1"',
        "vuln-type: os,library",
        "severity: CRITICAL,HIGH",
    ):
        require_active_source_line(scan_image, line)
    for forbidden in (
        "continue-on-error:",
        "ignore-unfixed:",
        "ignorefile:",
        "skip-dirs:",
        "skip-files:",
        "upload-artifact@",
        "packages: write",
        "id-token: write",
    ):
        if forbidden in images:
            raise AssertionError(f"forbidden Gate-B image behavior: {forbidden}")

    aggregate = job_block(source, "test")
    expected_needs = (
        "tenant-isolation",
        "go-race",
        "lint",
        "build",
        "security",
        "recovery-boundary-images",
    )
    needs_match = re.search(
        r"(?m)^    needs:\s*$\n(?P<body>(?:^      - [a-z0-9-]+\s*$\n?)+)",
        aggregate,
    )
    if needs_match is None:
        raise AssertionError("Gate-B aggregate needs list is missing")
    needs = tuple(
        line.removeprefix("      - ").strip()
        for line in needs_match.group("body").splitlines()
    )
    if needs != expected_needs:
        raise AssertionError("Gate-B aggregate dependencies differ")
    require_active_source_line(aggregate, "if: ${{ always() }}")
    if canonical_workflow_step_uses_refs(aggregate) != (
        "actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
    ):
        raise AssertionError("Gate-B aggregate checkout authority differs")
    aggregate_checkout = step_block(aggregate, "Checkout repository")
    require_active_source_line(aggregate_checkout, "fetch-depth: 0")
    require_active_source_line(aggregate_checkout, "persist-credentials: false")
    for line in (
        "python3 -m py_compile \\",
        "python3 -m unittest discover -s release/deployment -p 'test_*.py' -v",
        "python3 -B -m unittest discover -s release/canary -p 'test_*.py' -v",
    ):
        require_active_source_line(aggregate, line)
    for dependency in expected_needs:
        env_name = dependency.replace("-", "_").upper() + "_RESULT"
        require_active_source_line(
            aggregate,
            f"{env_name}: ${{{{ needs.{dependency}.result }}}}",
        )
        require_active_source_line(
            aggregate,
            f'[[ "${env_name}" == "success" ]]',
        )
    if "continue-on-error:" in aggregate:
        raise AssertionError("Gate-B aggregate accepts a non-success step")


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


def canonical_workflow_step_uses_refs(source: str) -> tuple[str, ...]:
    refs: list[str] = []
    candidate = re.compile(
        r"^(?:      - |        )(?:uses|\x22uses\x22|'uses')[ \t]*:"
    )
    canonical = re.compile(
        r"^(?:      - |        )uses: "
        r"(?P<ref>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40})"
        r"(?:\s+#.*)?$"
    )
    quoted_key = re.compile(
        r"^(?:      - |        )(?:\x22(?:\\.|[^\x22\\])*\x22|"
        r"'(?:[^']|'')*')[ \t]*:"
    )
    explicit_key = re.compile(r"^(?:      - |        )[?:](?:[ \t]|$)")
    for block in workflow_step_blocks(source):
        first_line = block.splitlines()[0]
        if re.match(r"^      -[ \t]*\{", first_line):
            raise AssertionError("flow-style workflow steps are forbidden")
        for line in block.splitlines():
            if quoted_key.match(line) is not None:
                raise AssertionError("quoted workflow step mapping keys are forbidden")
            if explicit_key.match(line) is not None:
                raise AssertionError("explicit workflow step mapping keys are forbidden")
            if candidate.match(line) is None:
                continue
            match = canonical.fullmatch(line)
            if match is None:
                raise AssertionError("workflow step uses mapping is not canonical")
            refs.append(match.group("ref"))
    return tuple(refs)


def exact_yaml_mapping_active_lines(
    source: str, indent: int, key: str
) -> tuple[str, ...]:
    lines = source.splitlines()
    marker = f"{' ' * indent}{key}:"
    starts = [index for index, line in enumerate(lines) if line == marker]
    if len(starts) != 1:
        raise AssertionError(
            f"expected one exact {key} mapping at indent {indent}, found {len(starts)}"
        )
    start = starts[0]
    end = len(lines)
    for index in range(start + 1, len(lines)):
        line = lines[index]
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        line_indent = len(line) - len(line.lstrip(" "))
        if line_indent <= indent:
            end = index
            break
    return normalized_active_lines("\n".join(lines[start:end]))


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


def assert_exact_release_image_artifact_controls(source: str) -> None:
    if hashlib.sha256(source.encode("utf-8")).hexdigest() != (
        EXACT_RELEASE_IMAGE_WORKFLOW_SHA256
    ):
        raise AssertionError("exact release image workflow source differs")
    build_step = step_block(
        source, "Build the exact AMD64 image without registry credentials"
    )
    action_line = f"uses: {EXACT_IMAGE_BUILD_ACTION}"
    source_active = normalized_active_lines(source)
    build_active = normalized_active_lines(build_step)
    workflow_uses_refs = canonical_workflow_step_uses_refs(source)
    build_action_refs = tuple(
        ref
        for ref in workflow_uses_refs
        if ref.startswith("docker/build-push-action@")
    )
    exact_build_ref = EXACT_IMAGE_BUILD_ACTION.split(" #", 1)[0]
    if (
        build_action_refs != (exact_build_ref,)
        or source_active.count(action_line) != 1
        or build_active.count(action_line) != 1
        or canonical_workflow_step_uses_refs(build_step) != (exact_build_ref,)
    ):
        raise AssertionError("exactly one pinned release image build action is required")
    if source_active.count('DOCKER_BUILD_RECORD_UPLOAD: "false"') != 1:
        raise AssertionError("build-record upload control must occur exactly once")
    step_scoped_record_control = re.compile(
        r'(?m)^        env:\r?\n'
        r'          DOCKER_BUILD_RECORD_UPLOAD: "false"\r?\n'
        r'^        with:\s*$'
    )
    if step_scoped_record_control.search(build_step) is None:
        raise AssertionError("build-record upload must be literal false on the build step")

    authority_matrix_step = step_block(
        source, "Resolve the reviewed source and component matrix"
    )
    authority_matrix_sha256 = hashlib.sha256(
        authority_matrix_step.encode("utf-8")
    ).hexdigest()
    if authority_matrix_sha256 != EXACT_IMAGE_AUTHORITY_MATRIX_STEP_SHA256:
        raise AssertionError("exact image authority matrix step differs")
    authority_matrix_active = normalized_active_lines(authority_matrix_step)
    for line in (
        "matrix=\"$(jq -c '[.release.components | to_entries[] | "
        ".value + {component: .key}]' \"$manifest\")\"",
        '[[ "$(jq \'length\' <<< "$matrix")" -eq 3 ]]',
        "printf 'matrix=%s\\n' \"$matrix\"",
    ):
        if authority_matrix_active.count(line) != 1:
            raise AssertionError(f"exact image authority matrix differs: {line}")
    if len(
        [line for line in authority_matrix_active if line.startswith("matrix=")]
    ) != 1:
        raise AssertionError("image authority matrix assignment count differs")
    authority = job_block(source, "authority")
    authority_matrix_outputs = tuple(
        line
        for line in authority.splitlines()
        if re.match(r"^      (?:matrix|\x22matrix\x22|'matrix')[ \t]*:", line)
    )
    if authority_matrix_outputs != (
        "      matrix: ${{ steps.source.outputs.matrix }}",
    ):
        raise AssertionError("authority matrix output binding differs")

    def artifact_upload_names(block: str) -> tuple[str, ...]:
        names: list[str] = []
        for step in workflow_step_blocks(block):
            if not is_artifact_upload_step(step):
                continue
            assert_unconditional_artifact_upload(step)
            matches = re.findall(r"(?m)^          name: ([^\r\n]+)$", step)
            if len(matches) != 1:
                raise AssertionError(
                    "every artifact upload must have one literal name binding"
                )
            names.extend(matches)
        return tuple(names)

    upload_names = artifact_upload_names(source)
    upload_action_ref = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
    upload_action_refs = tuple(
        ref for ref in workflow_uses_refs if ref.startswith("actions/upload-artifact@")
    )
    if (
        artifact_upload_use_count(source) != 6
        or upload_action_refs != (upload_action_ref,) * 6
        or upload_names != EXACT_IMAGE_UPLOAD_NAMES
    ):
        raise AssertionError("release image producer upload inventory differs")

    shared_matrix_line = "include: ${{ fromJSON(needs.authority.outputs.matrix) }}"
    for job_name, expected_upload_name in zip(
        ("build", "scan", "attest", "verify"),
        EXACT_IMAGE_UPLOAD_NAMES[:4],
        strict=True,
    ):
        block = job_block(source, job_name)
        expected_strategy = (
            "strategy:",
            "fail-fast: false",
            *(("max-parallel: 1",) if job_name == "build" else ()),
            "matrix:",
            shared_matrix_line,
        )
        if exact_yaml_mapping_active_lines(block, 4, "strategy") != expected_strategy:
            raise AssertionError(f"release image job matrix differs: {job_name}")
        strategy_keys = yaml_mapping_key_pattern(4, "strategy").findall(block)
        matrix_keys = yaml_mapping_key_pattern(6, "matrix").findall(block)
        if len(strategy_keys) != 1 or len(matrix_keys) != 1:
            raise AssertionError(f"release image job fan-out differs: {job_name}")
        if (
            artifact_upload_use_count(block) != 1
            or artifact_upload_names(block) != (expected_upload_name,)
        ):
            raise AssertionError(f"release image job upload differs: {job_name}")
    for job_name, expected_upload_name in zip(
        ("aggregate", "verify_set"),
        EXACT_IMAGE_UPLOAD_NAMES[4:],
        strict=True,
    ):
        block = job_block(source, job_name)
        assert_no_dynamic_job_fanout(block)
        if (
            artifact_upload_use_count(block) != 1
            or artifact_upload_names(block) != (expected_upload_name,)
        ):
            raise AssertionError(f"release image job upload differs: {job_name}")

    gate = step_block(source, "Require every release image control to pass")
    gate_sha256 = hashlib.sha256(gate.encode("utf-8")).hexdigest()
    if gate_sha256 != EXACT_IMAGE_GATE_STEP_SHA256:
        raise AssertionError("exact release image artifact gate step differs")
    gate_active = normalized_active_lines(gate)
    expected_name_lines = (
        '(["gmail-relay", "meta-relay", "web"] as $components |',
        '["image", "scanned", "attested", "verified"] as $kinds |',
        r'"\($kind)-\($phase)-\($component)-\($run_id)-\($attempt)"]',
        r'"release-set-\($phase)-\($run_id)-\($attempt)",',
        r'"verified-release-set-\($phase)-\($run_id)-\($attempt)"',
        '[[ "$(jq -r \'length\' <<< "$expected_artifact_names")" -eq 14 ]]',
    )
    for line in expected_name_lines:
        if gate_active.count(line) != 1:
            raise AssertionError(f"release image expected-name builder differs: {line}")

    function_blocks = {
        function_name: shell_function_block(gate, function_name)
        for function_name in (
            "require_bounded_artifact_inventory",
            "require_exact_artifact_inventory",
            "canonical_artifact_inventory",
        )
    }
    bounded_active = normalized_active_lines(
        function_blocks["require_bounded_artifact_inventory"]
    )
    for line in (
        '--arg control_sha "$CONTROL_SHA" \\',
        '--argjson expected_names "$expected_artifact_names" \\',
        '--argjson run_id "$GITHUB_RUN_ID" \'',
        '(.total_count | type) == "number" and',
        "(.total_count | floor) == .total_count and",
        ".total_count >= 0 and",
        ".total_count <= 14 and",
        '(.artifacts | type) == "array" and',
        "(.artifacts | length) <= 14 and",
        ".total_count == (.artifacts | length) and",
        "([.artifacts[].name] | length) == "
        "([.artifacts[].name] | unique | length) and",
        "all(.artifacts[];",
        '(.name | type) == "string" and',
        '(.name | endswith(".dockerbuild") | not) and',
        "(.name as $name | ($expected_names | index($name)) != null) and",
        ".expired == false and",
        '(.id | type) == "number" and',
        "(.id | floor) == .id and",
        ".id > 0 and",
        '(.size_in_bytes | type) == "number" and',
        "(.size_in_bytes | floor) == .size_in_bytes and",
        ".size_in_bytes > 0 and",
        ".size_in_bytes <= 67108864 and",
        '(.digest | type) == "string" and',
        '(.digest | test("^sha256:[0-9a-f]{64}$")) and',
        '(.workflow_run | type) == "object" and',
        ".workflow_run.id == $run_id and",
        '.workflow_run.head_branch == "main" and',
        ".workflow_run.head_sha == $control_sha",
    ):
        if bounded_active.count(line) != 1:
            raise AssertionError(f"bounded artifact inventory differs: {line}")

    exact_active = normalized_active_lines(
        function_blocks["require_exact_artifact_inventory"]
    )
    for line in (
        'require_bounded_artifact_inventory "$artifacts_json"',
        ".total_count == 14 and",
        "(.artifacts | length) == 14 and",
        "([.artifacts[].name] | sort) == $expected_names",
    ):
        if exact_active.count(line) != 1:
            raise AssertionError(f"exact artifact inventory differs: {line}")

    canonical_active = normalized_active_lines(
        function_blocks["canonical_artifact_inventory"]
    )
    for line in (
        "id,",
        "name,",
        "size_in_bytes,",
        "digest,",
        "expired,",
        "id: .workflow_run.id,",
        "head_branch: .workflow_run.head_branch,",
        "head_sha: .workflow_run.head_sha",
        "}] | sort_by(.name)",
    ):
        if canonical_active.count(line) != 1:
            raise AssertionError(f"canonical artifact inventory differs: {line}")

    artifact_endpoint = (
        '"/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"'
    )
    if gate_active.count(artifact_endpoint) != 2:
        raise AssertionError("artifact inventory must use two complete current-run reads")
    ordered_flow = (
        'artifacts_json="$(gh api \\',
        'if ! require_bounded_artifact_inventory "$artifacts_json"; then',
        'artifact_count="$(jq -r \'.total_count\' <<< "$artifacts_json")"',
        'returned_count="$(jq -r \'.artifacts | length\' <<< "$artifacts_json")"',
        'if [[ "$artifact_count" -eq 14 && "$returned_count" -eq 14 ]]; then',
        'require_exact_artifact_inventory "$artifacts_json"',
        'first_artifact_inventory="$(canonical_artifact_inventory '
        '"$artifacts_json")"',
        "break",
        'if [[ -z "$first_artifact_inventory" ]]; then',
        "sleep 2",
        'final_artifacts_json="$(gh api \\',
        'require_exact_artifact_inventory "$final_artifacts_json"',
        'final_artifact_inventory="$(canonical_artifact_inventory '
        '"$final_artifacts_json")"',
        '[[ "$first_artifact_inventory" == "$final_artifact_inventory" ]]',
    )
    ordered_positions: list[int] = []
    for line in ordered_flow:
        if gate_active.count(line) != 1:
            raise AssertionError(f"artifact inventory control-flow line differs: {line}")
        ordered_positions.append(gate_active.index(line))
    if ordered_positions != sorted(ordered_positions):
        raise AssertionError("artifact inventory control-flow order differs")
    for assignment in (
        'artifacts_json="$(gh api \\',
        'final_artifacts_json="$(gh api \\',
    ):
        position = gate_active.index(assignment)
        if gate_active[position + 1] != artifact_endpoint:
            raise AssertionError("artifact inventory endpoint is not bound to its read")

    active_gate_source = "\n".join(gate_active)
    for variable in ("artifacts_json", "final_artifacts_json"):
        assignments = re.findall(
            rf"(?m)^{re.escape(variable)}=", active_gate_source
        )
        if len(assignments) != 1:
            raise AssertionError(f"artifact inventory assignment count differs: {variable}")
    for forbidden in (
        ".total_count >= 14",
        ".total_count == 17",
        "map(select(",
        ".artifacts[] | select(",
        "del(.artifacts",
        "delete-artifact",
        "!*.dockerbuild",
    ):
        if forbidden in active_gate_source:
            raise AssertionError(f"artifact extras may not be filtered or tolerated: {forbidden}")


def assert_exact_aggregate_artifact_boundaries(source: str) -> None:
    for step_name, expected_sha256 in EXACT_AGGREGATE_ARTIFACT_BOUNDARY_SHA256.items():
        block = step_block(source, step_name)
        actual_sha256 = hashlib.sha256(block.encode("utf-8")).hexdigest()
        if actual_sha256 != expected_sha256:
            raise AssertionError(f"aggregate artifact boundary differs: {step_name}")


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


def assert_crm_canary_driver_publisher(source: str) -> None:
    if hashlib.sha256(source.encode("utf-8")).hexdigest() != (
        EXACT_CRM_CANARY_DRIVER_PUBLISHER_SHA256
    ):
        raise AssertionError("exact CRM canary driver publisher source differs")
    if not source.startswith(
        "name: Publish and Attest Production CRM Canary Driver\n\n"
        "run-name: Publish exact production CRM canary driver\n"
    ):
        raise AssertionError("CRM canary driver publisher identity differs")
    if not re.search(r"(?m)^  workflow_dispatch:\s*$", source):
        raise AssertionError("driver publisher must be manually dispatched")
    if re.search(
        r"(?m)^  (?:push|pull_request|schedule|workflow_run|workflow_call|repository_dispatch):\s*$",
        source,
    ):
        raise AssertionError("driver publisher has an unexpected trigger")
    dispatch = re.search(
        r"(?ms)^  workflow_dispatch:\s*$.*?(?=^permissions:|\Z)", source
    )
    if dispatch is None or normalized_active_lines(dispatch.group(0)) != (
        "workflow_dispatch:",
        "inputs:",
        "test_run_id:",
        "description: Successful exact-main push Test workflow run ID",
        "required: true",
        "type: string",
    ):
        raise AssertionError("driver publisher input contract differs")
    if exact_yaml_mapping_active_lines(source, 0, "on") != (
        "on:",
        "workflow_dispatch:",
        "inputs:",
        "test_run_id:",
        "description: Successful exact-main push Test workflow run ID",
        "required: true",
        "type: string",
    ):
        raise AssertionError("driver publisher trigger map differs")
    for line in (
        "group: rereply-crm-canary-driver-publication",
        "cancel-in-progress: false",
        "DRIVER_IMAGE: ghcr.io/medtechcorps-netizen/rereply-crm-canary-driver",
        "DRIVER_DOCKERFILE: docker/crm-canary-driver.Dockerfile",
    ):
        require_active_source_line(source, line)
    if "group: rereply-production" in normalized_active_lines(source):
        raise AssertionError("driver publisher must not join production mutation lock")
    if tuple(name for name, _ in job_definitions(source)) != (
        "authority",
        "build",
        "scan",
        "attest",
        "verify",
        "gate",
    ):
        raise AssertionError("driver publisher job inventory differs")
    if exact_yaml_mapping_active_lines(source, 0, "permissions") != (
        "permissions:",
        "contents: read",
    ):
        raise AssertionError("driver workflow permission map differs")
    expected_job_permissions = {
        "authority": ("permissions:", "actions: read", "contents: read"),
        "build": (
            "permissions:",
            "actions: read",
            "contents: read",
            "packages: write",
        ),
        "scan": ("permissions:", "contents: read", "packages: read"),
        "attest": (
            "permissions:",
            "actions: read",
            "attestations: write",
            "contents: read",
            "id-token: write",
        ),
        "verify": (
            "permissions:",
            "actions: read",
            "attestations: read",
            "contents: read",
        ),
        "gate": ("permissions:", "actions: read", "contents: read"),
    }
    for job_name, expected_permissions in expected_job_permissions.items():
        if exact_yaml_mapping_active_lines(
            job_block(source, job_name), 4, "permissions"
        ) != expected_permissions:
            raise AssertionError(f"driver job permissions differ: {job_name}")
    source_active = normalized_active_lines(source)
    if any(line.startswith("continue-on-error:") for line in source_active):
        raise AssertionError("driver publisher cannot tolerate job or step failures")
    for job_name, _ in job_definitions(source):
        for block in workflow_step_blocks(job_block(source, job_name)):
            block_active = normalized_active_lines(block)
            if any(line.startswith("if:") for line in block_active):
                raise AssertionError("driver publisher steps must be unconditional")
            run_indices = tuple(
                index for index, line in enumerate(block_active) if line == "run: |"
            )
            if run_indices:
                if len(run_indices) != 1 or run_indices[0] + 1 >= len(block_active):
                    raise AssertionError("driver run-step structure differs")
                if block_active[run_indices[0] + 1] != "set -euo pipefail":
                    raise AssertionError("driver run steps must start in strict mode")
    if source_active.count("packages: write") != 1 or source_active.count(
        "packages: read"
    ) != 1:
        raise AssertionError("driver package permissions differ")
    if source_active.count("id-token: write") != 1:
        raise AssertionError("driver attestation signing authority differs")
    workflow_uses_refs = canonical_workflow_step_uses_refs(source)
    expected_action_counts = {
        "actions/checkout@11d5960a326750d5838078e36cf38b85af677262": 2,
        "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02": 4,
        "actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093": 3,
        "docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8": 1,
        "docker/login-action@c94ce9fb468520275223c153574b00df6fe4bcc9": 1,
        "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6": 3,
    }
    if len(workflow_uses_refs) != sum(expected_action_counts.values()):
        raise AssertionError("driver action inventory differs")
    for action_ref, expected_count in expected_action_counts.items():
        if workflow_uses_refs.count(action_ref) != expected_count:
            raise AssertionError(f"driver action count differs: {action_ref}")
    if re.search(r"(?m)^\s+environment:\s*", source):
        raise AssertionError("driver publisher must not use a deployment environment")
    for forbidden in (
        "secrets.",
        "deployments: write",
        "DO_PRODUCTION_",
        "DIGITALOCEAN",
        "doctl",
        "aggregate-exact-four-phase-rollout",
        "build-attest-exact-release-images",
    ):
        if forbidden.lower() in source.lower():
            raise AssertionError(f"driver publisher contains forbidden authority: {forbidden}")

    for job_name in ("authority", "build", "scan", "attest", "verify", "gate"):
        job_active = normalized_active_lines(job_block(source, job_name))
        if job_active.count("RUNNER_ENVIRONMENT: ${{ runner.environment }}") != 1:
            raise AssertionError(f"driver hosted-runner input differs: {job_name}")
        if job_active.count('[[ "$RUNNER_ENVIRONMENT" == "github-hosted" ]]') != 1:
            raise AssertionError(f"driver hosted-runner guard differs: {job_name}")

    authority = job_block(source, "authority")
    authority_active = normalized_active_lines(authority)
    authority_expected_counts = {
        "REF_PROTECTED: ${{ github.ref_protected }}": 1,
        "EVENT_NAME: ${{ github.event_name }}": 1,
        '[[ "$EVENT_NAME" == "workflow_dispatch" ]]': 1,
        '[[ "$REF_PROTECTED" == "true" ]]': 1,
        '[[ "$RUN_ATTEMPT" == "1" ]]': 1,
        '.run_attempt == 1 and': 2,
        '.event == "push" and': 1,
        '.head_branch == "main" and': 1,
        '.path == ".github/workflows/test.yml"': 1,
        '([.jobs[] | select(.name == "security")] | length) == 1 and': 1,
        '.name == "security" and': 1,
        '([.steps[] | select(.name == "Test CRM canary driver protocol" and .conclusion == "success")] | length) == 1 and': 1,
        '([.steps[] | select(.name == "Build CRM canary driver container" and .conclusion == "success")] | length) == 1 and': 1,
        '([.steps[] | select(.name == "Scan CRM canary driver container" and .conclusion == "success")] | length) == 1': 1,
    }
    for required, expected_count in authority_expected_counts.items():
        if authority_active.count(required) != expected_count:
            raise AssertionError(f"driver Test authority differs: {required}")
    authority_main_step = normalized_active_lines(
        step_block(authority, "Require live main and exact successful push Test")
    )
    if authority_main_step.count(
        'live_main_sha="$(gh api "/repos/$REPOSITORY/git/ref/heads/main" --jq \'.object.sha\')"'
    ) != 1 or authority_main_step.count(
        '[[ "$live_main_sha" == "$CONTROL_SHA" ]]'
    ) != 1:
        raise AssertionError("driver initial live-main authority differs")

    derive_step = step_block(authority, "Derive exact driver build-input version")
    derive_active = normalized_active_lines(derive_step)
    for line in (
        "docker/crm-canary-driver.Dockerfile \\",
        "frontend/package.json \\",
        "frontend/package-lock.json",
        "git ls-tree -r --name-only HEAD -- frontend/canary-driver",
        ": > driver-inputs.tsv",
        "driver_version_sha256=\"$(sha256sum driver-inputs.tsv | awk '{print $1}')\"",
    ):
        if derive_active.count(line) != 1:
            raise AssertionError(f"driver authority build-input binding differs: {line}")
    recompute_step = step_block(
        job_block(source, "build"), "Recompute exact driver build-input version"
    )
    recompute_active = normalized_active_lines(recompute_step)
    for line in (
        "printf '%s\\n' docker/crm-canary-driver.Dockerfile frontend/package.json frontend/package-lock.json",
        "git ls-tree -r --name-only HEAD -- frontend/canary-driver",
        ": > driver-inputs.tsv",
        '[[ "$(sha256sum driver-inputs.tsv | awk \'{print $1}\')" == "$EXPECTED_DRIVER_VERSION" ]]',
    ):
        if recompute_active.count(line) != 1:
            raise AssertionError(f"driver build recomputation differs: {line}")

    build_step = step_block(
        source, "Build exact AMD64 CRM canary driver without registry credentials"
    )
    build_active = normalized_active_lines(build_step)
    exact_build_ref = EXACT_IMAGE_BUILD_ACTION.split(" #", 1)[0]
    if canonical_workflow_step_uses_refs(build_step) != (exact_build_ref,):
        raise AssertionError("driver build action pin differs")
    for line in (
        'DOCKER_BUILD_RECORD_UPLOAD: "false"',
        "context: ./source",
        "file: ./source/docker/crm-canary-driver.Dockerfile",
        "platforms: linux/amd64",
        "push: false",
        "load: true",
        "pull: true",
        "no-cache: true",
        'github-token: ""',
        "tags: ${{ steps.identity.outputs.image_ref }}",
        "io.rereply.crm-canary.driver-version-sha256=${{ needs.authority.outputs.driver_version_sha256 }}",
    ):
        if build_active.count(line) != 1:
            raise AssertionError(f"driver build step differs: {line}")
    identity_active = normalized_active_lines(
        step_block(source, "Create unique non-authority image tag")
    )
    if identity_active.count(
        'tag="control-${CONTROL_SHA:0:12}-driver-${DRIVER_VERSION:0:12}-run-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"'
    ) != 1:
        raise AssertionError("driver unique tag formula differs")
    publish_active = normalized_active_lines(
        step_block(source, "Recheck live main and publish unique non-authority tag")
    )
    for line in (
        'docker push "$IMAGE_REF" 2>&1 | tee docker-push.txt',
        'docker buildx imagetools inspect "$DRIVER_IMAGE@$digest" --raw > remote-descriptor.json',
    ):
        if publish_active.count(line) != 1:
            raise AssertionError(f"driver publish binding differs: {line}")
    live_main_guard = (
        '[[ "$(gh api "/repos/$REPOSITORY/git/ref/heads/main" '
        '--jq \'.object.sha\')" == "$CONTROL_SHA" ]]'
    )
    if publish_active.count(live_main_guard) != 1:
        raise AssertionError("driver pre-publish live-main guard differs")
    if source_active.count("tag_is_authority: false,") != 2:
        raise AssertionError("driver tag must remain non-authoritative")
    scan = job_block(source, "scan")
    scan_active = normalized_active_lines(scan)
    scan_active_source = "\n".join(scan_active)
    for step_name in (
        "Pull exact digest and verify unit and metadata contracts",
        "Fail on embedded secrets",
        "Fail on HIGH or CRITICAL vulnerabilities",
        "Generate exact SPDX SBOM",
        "Record exact scan, unit, and metadata evidence",
    ):
        step_block(scan, step_name)
    for required in (
        "--test canary-driver/driver.test.mjs",
        ".Config.User == \"pwuser\"",
        ".Config.WorkingDir == \"/app\"",
        ".Config.Entrypoint == [\"node\", \"canary-driver/index.mjs\"]",
        '--arg healthcheck_js "$EXPECTED_HEALTHCHECK_JS"',
        '.[0].Config.Healthcheck.Test == ["CMD", "node", "-e", $healthcheck_js]',
        "--scanners secret",
        "--image-config-scanners secret",
        "--severity HIGH,CRITICAL",
        "--exit-code 1",
        "spdx-json=sbom.spdx.json",
    ):
        if required not in scan_active_source:
            raise AssertionError(f"driver verification differs: {required}")
    for line in (
        'image_ref="$DRIVER_IMAGE@$digest"',
        "--test canary-driver/driver.test.mjs | tee unit-test.txt",
        '.[0].Config.User == "pwuser" and',
        '.[0].Config.WorkingDir == "/app" and',
        '.[0].Config.Entrypoint == ["node", "canary-driver/index.mjs"] and',
        '.[0].Config.Cmd == null and',
        '(.[0].Config.ExposedPorts | has("8080/tcp")) and',
        'EXPECTED_HEALTHCHECK_JS: "fetch(\'http://127.0.0.1:8080/healthz\').then(r=>{if(r.status!==204)process.exit(1)}).catch(()=>process.exit(1))"',
        '--arg healthcheck_js "$EXPECTED_HEALTHCHECK_JS" \\',
        '.[0].Config.Healthcheck.Test == ["CMD", "node", "-e", $healthcheck_js] and',
        '.[0].Config.Labels["org.opencontainers.image.revision"] == $control_sha and',
        '.[0].Config.Labels["org.opencontainers.image.version"] == $driver_version and',
        '.[0].Config.Labels["io.rereply.crm-canary.control-sha"] == $control_sha and',
        '.[0].Config.Labels["io.rereply.crm-canary.driver-version-sha256"] == $driver_version',
        "--scanners secret --image-config-scanners secret --exit-code 1 --format json \\",
        '--output secret-report.json --timeout 30m "$DRIVER_IMAGE@$digest"',
        "--scanners vuln --pkg-types os,library --severity HIGH,CRITICAL --exit-code 1 \\",
        '--format json --output vulnerability-report.json --timeout 30m "$DRIVER_IMAGE@$digest"',
        'syft scan "docker:$DRIVER_IMAGE@$digest" --output spdx-json=sbom.spdx.json',
    ):
        if scan_active.count(line) != 1:
            raise AssertionError(f"driver active verification line differs: {line}")
    for forbidden in ("--ignore-unfixed", ".trivyignore", "continue-on-error"):
        if forbidden in scan:
            raise AssertionError(f"driver scan weakening found: {forbidden}")

    attest = job_block(source, "attest")
    if normalized_active_lines(attest).count(
        "uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6 # v4"
    ) != 3:
        raise AssertionError("driver must have exactly three pinned attestations")
    for step_name in (
        "Create standard GitHub provenance attestation",
        "Create SPDX SBOM attestation",
        "Create exact driver source-binding attestation",
    ):
        step_block(attest, step_name)
    predicate_active = normalized_active_lines(
        step_block(attest, "Build exact driver source-binding predicate")
    )
    if predicate_active.count(live_main_guard) != 1:
        raise AssertionError("driver pre-attestation live-main guard differs")

    verify = job_block(source, "verify")
    verify_active = normalized_active_lines(verify)
    verify_active_source = "\n".join(verify_active)
    for required in (
        "gh attestation verify",
        "--predicate-type https://slsa.dev/provenance/v1",
        '--predicate-type "$SPDX_PREDICATE"',
        '--predicate-type "$DRIVER_PREDICATE"',
        "unset GH_TOKEN GITHUB_TOKEN CR_PAT REGISTRY_TOKEN DOCKER_AUTH_CONFIG",
        "anonymous_docker_config=\"$(mktemp -d)\"",
        "https://ghcr.io/token?scope=repository:${repository_path}:pull",
        "https://ghcr.io/v2/${repository_path}/manifests/${digest}",
        "https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}",
        '[[ "sha256:$(sha256sum anonymous-manifest.json | awk \'{print $1}\')" == "$digest" ]]',
    ):
        if required not in verify_active_source:
            raise AssertionError(f"driver anonymous/attestation verification differs: {required}")
    for line in (
        '--repo "$RELEASE_REPOSITORY"',
        '--signer-workflow "$signer_workflow"',
        '--signer-digest "$CONTROL_SHA"',
        '--source-digest "$CONTROL_SHA"',
        "--source-ref refs/heads/main",
        "--deny-self-hosted-runners",
        "--format json",
        "unset GH_TOKEN GITHUB_TOKEN CR_PAT REGISTRY_TOKEN DOCKER_AUTH_CONFIG",
        'anonymous_docker_config="$(mktemp -d)"',
        '"https://ghcr.io/v2/${repository_path}/manifests/${digest}" \\',
        '"https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}" \\',
        '[[ "sha256:$(sha256sum anonymous-manifest.json | awk \'{print $1}\')" == "$digest" ]]',
    ):
        if verify_active.count(line) != 1:
            raise AssertionError(f"driver active anonymous-pull line differs: {line}")
    if "docker/login-action" in verify_active_source or "packages: read" in verify_active_source:
        raise AssertionError("driver anonymous verification must not receive registry auth")
    attestation_verify_active = normalized_active_lines(
        step_block(verify, "Cryptographically verify all three exact attestations")
    )
    if attestation_verify_active.count(live_main_guard) != 1:
        raise AssertionError("driver attestation verification live-main guard differs")

    expected_upload_names = (
        "image-crm-canary-driver-${{ github.run_id }}-${{ github.run_attempt }}",
        "scanned-crm-canary-driver-${{ github.run_id }}-${{ github.run_attempt }}",
        "attested-crm-canary-driver-${{ github.run_id }}-${{ github.run_attempt }}",
        "verified-crm-canary-driver-${{ github.run_id }}-${{ github.run_attempt }}",
    )
    upload_blocks = tuple(
        block for block in workflow_step_blocks(source) if is_artifact_upload_step(block)
    )
    for block in upload_blocks:
        assert_unconditional_artifact_upload(block)
    upload_names = tuple(
        re.search(r"(?m)^          name: ([^\r\n]+)$", block).group(1)
        for block in upload_blocks
    )
    if artifact_upload_use_count(source) != 4 or upload_names != expected_upload_names:
        raise AssertionError("driver artifact producer inventory differs")

    gate = step_block(source, "Require stable exact four-artifact driver authority")
    gate_active = normalized_active_lines(gate)
    if gate_active.count(live_main_guard) != 2:
        raise AssertionError("driver final-gate live-main guards differ")
    for line in (
        ".total_count >= 0 and .total_count <= 4 and",
        "(.artifacts | length) <= 4 and",
        ".total_count == 4 and",
        "(.artifacts | length) == 4 and",
        '(.name | endswith(".dockerbuild") | not) and',
        '.size_in_bytes > 0 and .size_in_bytes <= 67108864 and',
        '(.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")) and',
        'artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"',
        'require_exact_artifact_inventory "$final_artifacts_json"',
        '[[ "$first_inventory" == "$final_inventory" ]]',
    ):
        if gate_active.count(line) != 1:
            raise AssertionError(f"driver stable artifact gate differs: {line}")
    ordered_gate_lines = (
        'artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"',
        'require_exact_artifact_inventory "$artifacts_json"',
        'first_inventory="$(canonical_artifact_inventory "$artifacts_json")"',
        "sleep 2",
        'final_artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"',
        'require_exact_artifact_inventory "$final_artifacts_json"',
        'final_inventory="$(canonical_artifact_inventory "$final_artifacts_json")"',
        '[[ "$first_inventory" == "$final_inventory" ]]',
    )
    positions: list[int] = []
    for line in ordered_gate_lines:
        if gate_active.count(line) != 1:
            raise AssertionError(f"driver artifact double-read line differs: {line}")
        positions.append(gate_active.index(line))
    if positions != sorted(positions) or len(set(positions)) != len(positions):
        raise AssertionError("driver artifact double-read order differs")
    if sum(line.startswith("artifacts_json=") for line in gate_active) != 1:
        raise AssertionError("driver first artifact inventory assignment differs")
    if sum(line.startswith("final_artifacts_json=") for line in gate_active) != 1:
        raise AssertionError("driver final artifact inventory assignment differs")
    gate_active_source = "\n".join(gate_active)
    for forbidden in (
        "map(select(",
        ".artifacts[] | select(",
        "del(.artifacts",
        "delete-artifact",
        "--method DELETE",
    ):
        if forbidden in gate_active_source:
            raise AssertionError(f"driver artifact inventory filtering found: {forbidden}")


class WorkflowAuthorityPolicyTests(unittest.TestCase):
    def test_pinned_jq_release_checksum_is_exact_in_every_consumer(self) -> None:
        url = "https://github.com/jqlang/jq/releases/download/jq-1.8.2/jq-linux-amd64"
        expected = (
            "b1c22172dd303f3be49e935aa56aa48a8b7a46e0bc838b4997d3bb451495870f"
        )

        def assert_exact_pin(source: str) -> None:
            active = normalized_active_lines(source)
            self.assertEqual(active.count(f"PINNED_JQ_URL: {url}"), 1)
            pins = tuple(
                line for line in active if line.startswith("PINNED_JQ_SHA256:")
            )
            self.assertEqual(pins, (f"PINNED_JQ_SHA256: {expected}",))

        consumers = (
            "aggregate-exact-four-phase-rollout.yml",
            "apply-production-phase.yml",
            "build-attest-exact-release-images.yml",
            "cleanup-production-valkey-recovery-fork.yml",
            "finalize-production-orphan-lock.yml",
            "plan-production-rollout.yml",
            "prepare-production-valkey-recovery-fork.yml",
            "publish-attest-production-crm-canary-driver.yml",
            "reconcile-production-main-lock-release.yml",
            "reconcile-production-orphan-lock-release.yml",
            "reconcile-production-orphan.yml",
            "rollback-production-orphan.yml",
            "rollback-production-phase.yml",
            "verify-production-crm-canary.yml",
            "verify-production-recovery-readiness.yml",
        )
        for name in consumers:
            with self.subTest(workflow=name):
                assert_exact_pin(workflow(name))

        source = workflow("prepare-production-valkey-recovery-fork.yml")
        pin_line = f"PINNED_JQ_SHA256: {expected}"
        mutants = (
            source.replace(pin_line, "PINNED_JQ_SHA256: " + expected[:-1], 1),
            source.replace(pin_line, "PINNED_JQ_SHA256: " + "0" * 64, 1),
            source.replace(pin_line, "", 1),
            source.replace(pin_line, pin_line + "\n  " + pin_line, 1),
            source.replace(f"PINNED_JQ_URL: {url}", "", 1),
            source.replace(
                f"PINNED_JQ_URL: {url}",
                "PINNED_JQ_URL: https://example.invalid/jq",
                1,
            ),
        )
        for mutant in mutants:
            self.assertNotEqual(mutant, source)
            with self.assertRaises(AssertionError):
                assert_exact_pin(mutant)

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
        for name in ACTIVE_PRODUCTION_CONTROLS + AUXILIARY_PRODUCTION_CONTROLS:
            with self.subTest(workflow=name):
                for line in workflow(name).splitlines():
                    match = re.match(r"\s*-?\s*uses:\s*(\S.*)$", line)
                    if match is not None and "/" in match.group(1) and "@" in match.group(1):
                        self.assertRegex(match.group(1), PINNED_ACTION)

    def test_crm_canary_driver_publisher_is_digest_only_and_independent(self) -> None:
        source = workflow("publish-attest-production-crm-canary-driver.yml")
        assert_crm_canary_driver_publisher(source)
        runbook = (ROOT / "docs" / "crm-production-release-control.md").read_text(
            encoding="utf-8"
        )
        for required in (
            "### Independent CRM canary driver image",
            "successful attempt-1 push `Test` run ID",
            "Tags are unique diagnostic labels and never deployment authority.",
            "two identical complete reads of exactly four current-run",
            "does not modify or participate in `Build and Attest Exact",
            "The driver digest is not a fourth component of the product",
            "does not deploy the driver, provision its ledger",
        ):
            self.assertIn(required, runbook)

        def without_step_strict_mode(step_name: str) -> str:
            original = step_block(source, step_name)
            mutated = original.replace(
                "        run: |\n          set -euo pipefail\n",
                "        run: |\n",
                1,
            )
            self.assertNotEqual(mutated, original)
            return source.replace(original, mutated, 1)

        def without_step_line(step_name: str, line: str) -> str:
            original = step_block(source, step_name)
            mutated = original.replace(f"          {line}\n", "", 1)
            self.assertNotEqual(mutated, original)
            return source.replace(original, mutated, 1)

        mutations = {
            "unexpected-push-trigger": source.replace(
                "  workflow_dispatch:\n", "  workflow_dispatch:\n  push:\n", 1
            ),
            "callable-trigger-added": source.replace(
                "  workflow_dispatch:\n",
                "  workflow_call:\n"
                "    inputs:\n"
                "      test_run_id:\n"
                "        required: true\n"
                "        type: string\n"
                "  workflow_dispatch:\n",
                1,
            ),
            "input-renamed": source.replace("test_run_id:", "test_run:", 1),
            "production-concurrency": source.replace(
                "group: rereply-crm-canary-driver-publication",
                "group: rereply-production",
                1,
            ),
            "protected-ref-bypass": source.replace(
                '[[ "$REF_PROTECTED" == "true" ]]', "true", 1
            ),
            "protected-ref-comment-decoy": source.replace(
                '          [[ "$REF_PROTECTED" == "true" ]]',
                '          # [[ "$REF_PROTECTED" == "true" ]]\n'
                "          true",
                1,
            ),
            "protected-ref-inline-decoy": source.replace(
                '          [[ "$REF_PROTECTED" == "true" ]]',
                '          true # [[ "$REF_PROTECTED" == "true" ]]',
                1,
            ),
            "test-event-weakened": source.replace(
                '.event == "push"', '.event == "workflow_dispatch"', 1
            ),
            "test-event-comment-decoy": source.replace(
                '                .event == "push" and',
                '                # .event == "push" and\n'
                '                .event == "workflow_dispatch" and',
                1,
            ),
            "test-event-inline-decoy": source.replace(
                '                .event == "push" and',
                '                .event == "workflow_dispatch" and # .event == "push"',
                1,
            ),
            "test-security-step-renamed": source.replace(
                '.name == "Test CRM canary driver protocol"',
                '.name == "Test some driver protocol"',
                1,
            ),
            "driver-lockfile-omitted": source.replace(
                "                frontend/package-lock.json\n", "", 1
            ),
            "driver-lockfile-dual-omission-decoys": source.replace(
                "                frontend/package-lock.json\n",
                "                : # frontend/package-lock.json\n",
                1,
            ).replace(
                "          printf '%s\\n' docker/crm-canary-driver.Dockerfile frontend/package.json frontend/package-lock.json\n",
                "          printf '%s\\n' docker/crm-canary-driver.Dockerfile frontend/package.json\n"
                "          : # frontend/package-lock.json\n",
                1,
            ),
            "build-record-upload-enabled": source.replace(
                'DOCKER_BUILD_RECORD_UPLOAD: "false"',
                'DOCKER_BUILD_RECORD_UPLOAD: "true"',
                1,
            ),
            "quoted-action-key": source.replace(
                "        uses: actions/checkout@",
                '        "uses": actions/checkout@',
                1,
            ),
            "build-context-retargeted": source.replace(
                "file: ./source/docker/crm-canary-driver.Dockerfile",
                "file: ./source/docker/Dockerfile",
                1,
            ),
            "mutable-shared-tag": source.replace(
                '          tag="control-${CONTROL_SHA:0:12}-driver-${DRIVER_VERSION:0:12}-run-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"',
                '          tag="latest"',
                1,
            ),
            "published-tag-retargeted": source.replace(
                '          docker push "$IMAGE_REF" 2>&1 | tee docker-push.txt',
                '          docker push "$DRIVER_IMAGE:latest" 2>&1 | tee docker-push.txt',
                1,
            ),
            "pre-publish-live-main-recheck-removed": without_step_line(
                "Recheck live main and publish unique non-authority tag",
                '[[ "$(gh api "/repos/$REPOSITORY/git/ref/heads/main" --jq \'.object.sha\')" == "$CONTROL_SHA" ]]',
            ),
            "build-label-removed": source.replace(
                "            io.rereply.crm-canary.driver-version-sha256=${{ needs.authority.outputs.driver_version_sha256 }}\n",
                "",
                1,
            ),
            "tag-authority-enabled": source.replace(
                "tag_is_authority: false", "tag_is_authority: true"
            ),
            "unit-test-disabled": source.replace(
                "--test canary-driver/driver.test.mjs", "--version", 1
            ),
            "pull-inspect-tag-target": source.replace(
                '          image_ref="$DRIVER_IMAGE@$digest"',
                '          image_ref="$DRIVER_IMAGE:latest"',
                1,
            ),
            "metadata-user-inline-decoy": source.replace(
                '              .[0].Config.User == "pwuser" and',
                '              true and # .[0].Config.User == "pwuser"',
                1,
            ),
            "healthcheck-shell-quote-regression": source.replace(
                '            --arg healthcheck_js "$EXPECTED_HEALTHCHECK_JS" \\\n',
                "",
                1,
            ).replace(
                '              .[0].Config.Healthcheck.Test == ["CMD", "node", "-e", $healthcheck_js] and',
                '              .[0].Config.Healthcheck.Test == ["CMD", "node", "-e", "fetch(\'http://127.0.0.1:8080/healthz\').then(r=>{if(r.status!==204)process.exit(1)}).catch(()=>process.exit(1))"] and',
                1,
            ),
            "secret-scan-disabled": source.replace(
                "--scanners secret", "--scanners vuln", 1
            ),
            "secret-scan-tag-target": source.replace(
                '            --output secret-report.json --timeout 30m "$DRIVER_IMAGE@$digest"',
                '            --output secret-report.json --timeout 30m "$DRIVER_IMAGE:latest"',
                1,
            ),
            "vulnerability-severity-weakened": source.replace(
                "--severity HIGH,CRITICAL", "--severity CRITICAL", 1
            ),
            "vulnerability-scan-tag-target": source.replace(
                '            --format json --output vulnerability-report.json --timeout 30m "$DRIVER_IMAGE@$digest"',
                '            --format json --output vulnerability-report.json --timeout 30m "$DRIVER_IMAGE:latest"',
                1,
            ),
            "vulnerability-severity-comment-decoy": source.replace(
                "            --scanners vuln --pkg-types os,library --severity HIGH,CRITICAL --exit-code 1 \\\n",
                "            # --scanners vuln --pkg-types os,library --severity HIGH,CRITICAL --exit-code 1\n"
                "            --scanners vuln --pkg-types os,library --severity CRITICAL --exit-code 1 \\\n",
                1,
            ),
            "vulnerability-severity-inline-decoy": source.replace(
                "            --scanners vuln --pkg-types os,library --severity HIGH,CRITICAL --exit-code 1 \\\n"
                "            --format json --output vulnerability-report.json --timeout 30m \"$DRIVER_IMAGE@$digest\"",
                "            --scanners vuln --pkg-types os,library --severity CRITICAL --exit-code 1 \\\n"
                "            --format json --output vulnerability-report.json --timeout 30m \"$DRIVER_IMAGE@$digest\"\n"
                "          : # --severity HIGH,CRITICAL",
                1,
            ),
            "attestation-removed": source.replace(
                "        uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6 # v4\n",
                "        run: true\n",
                1,
            ),
            "attestation-comment-decoy": source.replace(
                "        uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6 # v4\n",
                "        # uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6 # v4\n"
                "        run: true\n",
                1,
            ),
            "anonymous-credential-clearing-removed": source.replace(
                "          unset GH_TOKEN GITHUB_TOKEN CR_PAT REGISTRY_TOKEN DOCKER_AUTH_CONFIG\n",
                "",
                1,
            ),
            "attestation-signer-workflow-unbound": source.replace(
                '            --signer-workflow "$signer_workflow"\n', "", 1
            ),
            "attestation-signer-digest-unbound": source.replace(
                '            --signer-digest "$CONTROL_SHA"\n', "", 1
            ),
            "attestation-self-hosted-allowed": source.replace(
                "            --deny-self-hosted-runners\n", "", 1
            ),
            "anonymous-blob-pull-removed": source.replace(
                '              "https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}" \\\n',
                '              "https://example.invalid/${blob_digest}" \\\n',
                1,
            ),
            "attestation-verification-tolerated": source.replace(
                "      - name: Cryptographically verify all three exact attestations\n",
                "      - name: Cryptographically verify all three exact attestations\n"
                "        continue-on-error: true\n",
                1,
            ),
            "attestation-verification-not-strict": without_step_strict_mode(
                "Cryptographically verify all three exact attestations"
            ),
            "anonymous-verification-tolerated": source.replace(
                "      - name: Require credential-free anonymous exact-digest pull\n",
                "      - name: Require credential-free anonymous exact-digest pull\n"
                "        continue-on-error: true\n",
                1,
            ),
            "anonymous-verification-not-strict": without_step_strict_mode(
                "Require credential-free anonymous exact-digest pull"
            ),
            "anonymous-blob-comment-decoy": source.replace(
                '              "https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}" \\\n',
                '              # "https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}" \\\n'
                '              "https://example.invalid/${blob_digest}" \\\n',
                1,
            ),
            "anonymous-blob-inline-decoy": source.replace(
                '              "https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}" \\\n'
                '              --output "$blob_file"',
                '              "https://example.invalid/${blob_digest}" \\\n'
                '              --output "$blob_file"\n'
                '            : # https://ghcr.io/v2/${repository_path}/blobs/${blob_digest}',
                1,
            ),
            "artifact-upper-bound-weakened": source.replace(
                ".total_count >= 0 and .total_count <= 4 and",
                ".total_count >= 0 and .total_count <= 5 and",
                1,
            ),
            "artifact-extras-filtered": source.replace(
                '            artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"',
                '            artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"\n'
                '            artifacts_json="$(jq \'del(.artifacts[4:]) | .total_count = (.artifacts | length)\' <<< "$artifacts_json")"',
                1,
            ),
            "artifact-upload-tolerated": source.replace(
                "      - name: Upload immutable driver image identity\n",
                "      - name: Upload immutable driver image identity\n"
                "        continue-on-error: true\n",
                1,
            ),
            "final-artifact-read-unchecked": source.replace(
                '          require_exact_artifact_inventory "$final_artifacts_json"',
                "          true",
                1,
            ),
            "final-artifact-fetch-reused-inline-decoy": source.replace(
                '          final_artifacts_json="$(gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100")"',
                '          final_artifacts_json="$artifacts_json" # gh api "/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100"',
                1,
            ),
            "stable-artifact-delay-inline-decoy": source.replace(
                "          sleep 2", "          true # sleep 2", 1
            ),
            "stable-artifact-equality-removed": source.replace(
                '          [[ "$first_inventory" == "$final_inventory" ]]',
                "          true",
                1,
            ),
            "deployment-environment-added": source.replace(
                "  build:\n    name: Build and publish exact CRM canary driver\n",
                "  build:\n    name: Build and publish exact CRM canary driver\n"
                "    environment: production\n",
                1,
            ),
        }
        self.assertEqual(len(mutations), len(set(mutations.values())))
        for label, mutant in mutations.items():
            self.assertNotEqual(mutant, source)
            with self.subTest(label=label):
                with self.assertRaises(AssertionError):
                    assert_crm_canary_driver_publisher(mutant)

    def test_every_artifact_id_download_flattens_the_exact_selection(self) -> None:
        expected_counts = {
            "apply-production-phase.yml": 10,
            "cleanup-production-valkey-recovery-fork.yml": 6,
            "finalize-production-orphan-lock.yml": 3,
            "prepare-production-valkey-recovery-fork.yml": 4,
            "reconcile-production-main-lock-release.yml": 6,
            "reconcile-production-orphan-lock-release.yml": 5,
            "reconcile-production-orphan.yml": 5,
            "rollback-production-orphan.yml": 3,
            "rollback-production-phase.yml": 10,
            "verify-production-crm-canary.yml": 1,
            "verify-production-recovery-readiness.yml": 2,
        }
        observed_total = 0
        for workflow_name, expected_count in expected_counts.items():
            downloads = tuple(
                step
                for step in workflow_step_blocks(workflow(workflow_name))
                if re.search(r"(?m)^          artifact-ids:", step)
            )
            with self.subTest(workflow=workflow_name):
                self.assertEqual(len(downloads), expected_count)
                for download in downloads:
                    self.assertRegex(
                        download,
                        r"(?m)^        uses: actions/download-artifact@"
                        r"d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4$",
                    )
                    self.assertEqual(
                        len(
                            re.findall(
                                r"(?m)^          merge-multiple: true[ \t]*$",
                                download,
                            )
                        ),
                        1,
                    )
            observed_total += len(downloads)
        self.assertEqual(observed_total, 55)

    def test_gate_b_recovery_boundary_is_a_required_protected_ci_result(self) -> None:
        source = workflow("test.yml")
        assert_gate_b_test_workflow(source)

        protocol_step_source = (
            "      - name: Test CRM canary driver protocol\n"
            "        run: node --test frontend/canary-driver/driver.test.mjs\n\n"
        )
        driver_build_step_source = (
            "      - name: Build CRM canary driver container\n"
            "        run: docker build -f docker/crm-canary-driver.Dockerfile "
            "-t rereply-crm-canary-driver:ci ."
        )
        reordered_protocol_source = source.replace(
            protocol_step_source,
            "",
            1,
        ).replace(
            driver_build_step_source,
            driver_build_step_source
            + "\n\n      # Test CRM canary driver protocol\n"
            + protocol_step_source.rstrip(),
            1,
        )

        mutations = {
            "push-removed": source.replace(
                "  push:\n    branches:\n      - main\n",
                "",
                1,
            ),
            "pull-request-removed": source.replace(
                "  pull_request:\n    branches:\n      - main\n",
                "",
                1,
            ),
            "pull-request-retargeted": source.replace(
                "  pull_request:\n    branches:\n      - main\n",
                "  pull_request:\n    branches:\n      - develop\n",
                1,
            ),
            "pull-request-path-filtered": source.replace(
                "  pull_request:\n    branches:\n      - main\n",
                "  pull_request:\n    branches:\n      - main\n"
                "    paths-ignore:\n      - prototype/**\n",
                1,
            ),
            "gate-a-verifier-removed": source.replace(
                "python3 -B prototype/recovery-boundary/verify_gate_a.py",
                "true",
                1,
            ),
            "gate-a-step-disabled": source.replace(
                "      - name: Verify Gate-A recovery boundary\n",
                "      - name: Verify Gate-A recovery boundary\n"
                "        if: ${{ false }}\n",
                1,
            ),
            "gate-a-step-tolerated": source.replace(
                "      - name: Verify Gate-A recovery boundary\n",
                "      - name: Verify Gate-A recovery boundary\n"
                "        continue-on-error: true\n",
                1,
            ),
            "actionlint-template-check-removed": source.replace(
                "actionlint -ignore '\"on\" section should not be empty' "
                "prototype/recovery-boundary/workflows/*.tmpl",
                "true",
                1,
            ),
            "actionlint-workflow-check-removed": source.replace(
                "actionlint .github/workflows/test.yml",
                "true",
                1,
            ),
            "actionlint-workflow-check-retargeted": source.replace(
                "actionlint .github/workflows/test.yml",
                "actionlint .github/workflows/e2e.yml",
                1,
            ),
            "actionlint-step-tolerated": source.replace(
                "      - name: Lint workflows and inert recovery templates\n",
                "      - name: Lint workflows and inert recovery templates\n"
                "        continue-on-error: true\n",
                1,
            ),
            "image-build-cache-enabled": source.replace(
                "--pull --no-cache", "--pull", 1
            ),
            "image-build-folded-comment": source.replace(
                "          docker build --pull --no-cache --platform linux/amd64\n",
                "          # suppress image build\n"
                "          docker build --pull --no-cache --platform linux/amd64\n",
                1,
            ),
            "image-scan-unbound": source.replace(
                "image-ref: ${{ matrix.image }}",
                "image-ref: recovery-boundary-unbound:ci",
                1,
            ),
            "image-matrix-fail-fast": source.replace(
                "fail-fast: false", "fail-fast: true", 1
            ),
            "image-build-tolerated": source.replace(
                "      - name: Build inert recovery boundary image\n",
                "      - name: Build inert recovery boundary image\n"
                "        continue-on-error: true\n",
                1,
            ),
            "image-scan-tolerated": source.replace(
                "      - name: Scan inert recovery boundary image\n",
                "      - name: Scan inert recovery boundary image\n"
                "        continue-on-error: true\n",
                1,
            ),
            "security-write-permission": source.replace(
                "  security:\n    name: security\n",
                "  security:\n    name: security\n"
                "    permissions:\n      packages: write\n",
                1,
            ),
            "security-secret": source.replace(
                "  security:\n    name: security\n",
                "  security:\n    name: security\n"
                "    env:\n      PROVIDER_TOKEN: ${{ secrets.PROVIDER_TOKEN }}\n",
                1,
            ),
            "security-push": source.replace(
                "      - name: Build production container\n"
                "        run: docker build -f docker/Dockerfile -t rereply:ci .",
                "      - name: Build production container\n"
                "        run: |\n"
                "          docker build -f docker/Dockerfile -t rereply:ci .\n"
                "          docker push rereply:ci",
                1,
            ),
            "security-scan-tolerated": source.replace(
                "      - name: Scan production container\n",
                "      - name: Scan production container\n"
                "        continue-on-error: true\n",
                1,
            ),
            "crm-driver-protocol-test-retargeted": source.replace(
                "node --test frontend/canary-driver/driver.test.mjs",
                "node --test frontend/canary-driver/runner.test.mjs",
                1,
            ),
            "crm-driver-protocol-test-tolerated": source.replace(
                "      - name: Test CRM canary driver protocol\n",
                "      - name: Test CRM canary driver protocol\n"
                "        continue-on-error: true\n",
                1,
            ),
            "crm-driver-protocol-reordered-with-decoy": reordered_protocol_source,
            "crm-driver-build-retargeted": source.replace(
                "docker build -f docker/crm-canary-driver.Dockerfile "
                "-t rereply-crm-canary-driver:ci .",
                "docker build -f docker/Dockerfile "
                "-t rereply-crm-canary-driver:ci .",
                1,
            ),
            "crm-driver-build-tolerated": source.replace(
                "      - name: Build CRM canary driver container\n",
                "      - name: Build CRM canary driver container\n"
                "        continue-on-error: true\n",
                1,
            ),
            "trivy-suppression-guard-weakened": source.replace(
                ".trivyignore .trivyignore.yaml trivy.yaml trivy.yml",
                ".trivyignore.yaml trivy.yaml trivy.yml",
                1,
            ),
            "trivy-suppression-guard-tolerated": source.replace(
                "      - name: Reject ambient Trivy suppression policy\n",
                "      - name: Reject ambient Trivy suppression policy\n"
                "        continue-on-error: true\n",
                1,
            ),
            "crm-driver-scan-unbound": source.replace(
                "image-ref: rereply-crm-canary-driver:ci",
                "image-ref: rereply:ci",
                1,
            ),
            "crm-driver-scan-tolerated": source.replace(
                "      - name: Scan CRM canary driver container\n",
                "      - name: Scan CRM canary driver container\n"
                "        continue-on-error: true\n",
                1,
            ),
            "crm-driver-scan-severity-weakened": source.replace(
                "      - name: Scan CRM canary driver container\n"
                "        uses: aquasecurity/trivy-action@"
                "a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0\n"
                "        with:\n"
                "          image-ref: rereply-crm-canary-driver:ci\n"
                "          format: table\n"
                '          exit-code: "1"\n'
                "          vuln-type: os,library\n"
                "          severity: CRITICAL,HIGH",
                "      - name: Scan CRM canary driver container\n"
                "        uses: aquasecurity/trivy-action@"
                "a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0\n"
                "        with:\n"
                "          image-ref: rereply-crm-canary-driver:ci\n"
                "          format: table\n"
                '          exit-code: "1"\n'
                "          vuln-type: os,library\n"
                "          severity: CRITICAL",
                1,
            ),
            "crm-driver-scan-suppressed": source.replace(
                "      - name: Scan CRM canary driver container\n"
                "        uses: aquasecurity/trivy-action@"
                "a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0\n"
                "        with:\n",
                "      - name: Scan CRM canary driver container\n"
                "        uses: aquasecurity/trivy-action@"
                "a9c7b0f06e461e9d4b4d1711f154ee024b8d7ab8 # v0.36.0\n"
                "        with:\n"
                "          ignore-unfixed: true\n",
                1,
            ),
            "crm-driver-vulnerability-scanner-replaced": source.replace(
                "          scanners: vuln\n\n  recovery-boundary-images:",
                "          scanners: secret\n\n  recovery-boundary-images:",
                1,
            ),
            "crm-driver-scan-ambient-suppression": source.replace(
                "  security:\n    name: security\n",
                "  security:\n    name: security\n"
                "    env:\n      TRIVY_IGNORE_UNFIXED: true\n",
                1,
            ),
            "crm-driver-scan-workflow-ambient-suppression": source.replace(
                "permissions:\n  contents: read\n",
                "permissions:\n  contents: read\n\n"
                "env:\n  TRIVY_IGNORE_UNFIXED: true\n",
                1,
            ),
            "build-job-tolerated": source.replace(
                "  build:\n    name: build\n    runs-on: ubuntu-24.04\n",
                "  build:\n    name: build\n    runs-on: ubuntu-24.04\n"
                "    continue-on-error: true\n",
                1,
            ),
            "aggregate-weakened": source.replace(
                '[[ "$RECOVERY_BOUNDARY_IMAGES_RESULT" == "success" ]]',
                "true",
                1,
            ),
        }
        self.assertEqual(len(mutations), len(set(mutations.values())))
        for label, mutant in mutations.items():
            self.assertNotEqual(mutant, source)
            with self.subTest(
                label=label,
                mutant=hashlib.sha256(mutant.encode()).hexdigest(),
            ):
                with self.assertRaises(AssertionError):
                    assert_gate_b_test_workflow(mutant)

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

    def test_release_image_producer_requires_stable_exact_artifact_inventory(self) -> None:
        source = workflow("build-attest-exact-release-images.yml")
        assert_exact_release_image_artifact_controls(source)

        aggregate = workflow("aggregate-exact-four-phase-rollout.yml")
        assert_exact_aggregate_artifact_boundaries(aggregate)

        second_step_name = "Reverify all run attempts and exact artifact records"
        second_boundary = step_block(aggregate, second_step_name)
        comparison_start = (
            "                ([.artifacts | sort_by(.name)[] | {"
        )
        comparison_end = "                }])"
        start = second_boundary.index(comparison_start)
        end = second_boundary.index(comparison_end, start) + len(comparison_end)
        comparison = second_boundary[start:end]
        commented_comparison = "\n".join(
            f"{line[: len(line) - len(line.lstrip())]}# {line.lstrip()}"
            for line in comparison.splitlines()
        )
        weakened_boundary = (
            second_boundary[:start]
            + "                true\n"
            + commented_comparison
            + second_boundary[end:]
        )
        weakened_aggregate = aggregate.replace(
            second_boundary, weakened_boundary, 1
        )
        self.assertNotEqual(weakened_aggregate, aggregate)
        with self.assertRaises(AssertionError):
            assert_exact_aggregate_artifact_boundaries(weakened_aggregate)

    def test_release_image_artifact_control_rejects_weakening_mutations(self) -> None:
        source = workflow("build-attest-exact-release-images.yml")
        gate = step_block(source, "Require every release image control to pass")
        build = step_block(
            source, "Build the exact AMD64 image without registry credentials"
        )
        authority_matrix = step_block(
            source, "Resolve the reviewed source and component matrix"
        )

        def mutate_gate(old: str, new: str, occurrences: int = 1) -> str:
            self.assertEqual(gate.count(old), occurrences)
            return source.replace(gate, gate.replace(old, new, 1), 1)

        def mutate_build(old: str, new: str) -> str:
            self.assertEqual(build.count(old), 1)
            return source.replace(build, build.replace(old, new, 1), 1)

        def mutate_authority_matrix(old: str, new: str) -> str:
            self.assertEqual(authority_matrix.count(old), 1)
            mutated_step = authority_matrix.replace(old, new, 1)
            return source.replace(authority_matrix, mutated_step, 1)

        def mutate_job(job_name: str, old: str, new: str) -> str:
            block = job_block(source, job_name)
            self.assertEqual(block.count(old), 1)
            mutated_block = block.replace(old, new, 1)
            return source.replace(block, mutated_block, 1)

        def mutate_gate_function(name: str, old: str, new: str) -> str:
            function = shell_function_block(gate, name)
            self.assertEqual(function.count(old), 1)
            mutated_function = function.replace(old, new, 1)
            return source.replace(gate, gate.replace(function, mutated_function, 1), 1)

        record_env = (
            "        env:\n"
            '          DOCKER_BUILD_RECORD_UPLOAD: "false"\n'
        )
        record_line = '          DOCKER_BUILD_RECORD_UPLOAD: "false"\n'

        def add_record_control_to_other_step(document: str) -> str:
            step_name = "Require a GitHub-hosted runner"
            other_step = step_block(document, step_name)
            env_header = "        env:\n"
            self.assertEqual(other_step.count(env_header), 1)
            self.assertNotIn(record_line, other_step)
            replacement = other_step.replace(
                env_header, env_header + record_line, 1
            )
            return document.replace(other_step, replacement, 1)

        def insert_step_after(document: str, anchor_name: str, new_step: str) -> str:
            anchor = step_block(document, anchor_name)
            separator = "" if anchor.endswith("\n") else "\n"
            return document.replace(anchor, anchor + separator + new_step, 1)

        upload_action = (
            "uses: actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02 # v4"
        )
        removed_record_control = mutate_build(record_env, "")
        duplicate_record_control = add_record_control_to_other_step(source)
        moved_record_control = add_record_control_to_other_step(
            removed_record_control
        )
        extra_build_step = (
            "      - name: Unreviewed second release image build\n"
            "        uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e9\n"
            "        with:\n"
            "          context: ./source\n"
        )
        second_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            extra_build_step,
        )
        precolon_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            extra_build_step.replace("        uses:", "        uses :", 1),
        )
        quoted_value_build_step = extra_build_step.replace(
            "        uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e9",
            '        uses: "docker/build-push-action@'
            '10e90e3645eae34f1e60eeb005ba3a3d33f178e9"',
            1,
        )
        self.assertNotEqual(quoted_value_build_step, extra_build_step)
        quoted_value_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            quoted_value_build_step,
        )
        flow_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            "      - {name: Unreviewed flow release image build, "
            "uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e8, "
            "with: {context: ./source}}\n",
        )
        spaced_inline_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            "      -   uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e8\n"
            "          with:\n"
            "            context: ./source\n",
        )
        newline_flow_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            "      -\n"
            "        {name: Unreviewed newline-flow release image build, "
            "uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e8, "
            "with: {context: ./source}}\n",
        )
        anchored_flow_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            "      - &hidden {name: Unreviewed anchored release image build, "
            "uses: docker/build-push-action@"
            "10e90e3645eae34f1e60eeb005ba3a3d33f178e8, "
            "with: {context: ./source}}\n",
        )
        case_variant_build_action = insert_step_after(
            source,
            "Build the exact AMD64 image without registry credentials",
            extra_build_step.replace(
                "docker/build-push-action@", "Docker/build-push-action@", 1
            ),
        )
        extra_upload_step = (
            "      - name: Unreviewed extra release artifact\n"
            f"        {upload_action}\n"
            "        with:\n"
            "          name: unexpected-release-artifact\n"
            "          path: image.json\n"
        )
        second_upload_action = insert_step_after(
            source, "Upload immutable image identity", extra_upload_step
        )
        precolon_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            extra_upload_step.replace("        uses:", "        uses :", 1),
        )
        quoted_value_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            extra_upload_step.replace(
                "        uses: actions/upload-artifact@"
                "ea165f8d65b6e75b540449e92b4886f43607fa02 # v4",
                '        uses: "actions/upload-artifact@'
                'ea165f8d65b6e75b540449e92b4886f43607fa02" # v4',
                1,
            ),
        )
        flow_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            "      - {name: Unreviewed flow artifact upload, "
            "uses: actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02, "
            "with: {name: unexpected-release-artifact, path: image.json}}\n",
        )
        spaced_inline_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            "      -   uses: actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02\n"
            "          with:\n"
            "            name: unexpected-release-artifact\n"
            "            path: image.json\n",
        )
        newline_flow_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            "      -\n"
            "        {name: Unreviewed newline-flow artifact upload, "
            "uses: actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02, "
            "with: {name: unexpected-release-artifact, path: image.json}}\n",
        )
        anchored_flow_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            "      - &hidden {name: Unreviewed anchored artifact upload, "
            "uses: actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02, "
            "with: {name: unexpected-release-artifact, path: image.json}}\n",
        )
        case_variant_upload_action = insert_step_after(
            source,
            "Upload immutable image identity",
            extra_upload_step.replace(
                "actions/upload-artifact@", "Actions/upload-artifact@", 1
            ),
        )
        quoted_upload_step = (
            "      - name: Quoted-key extra release artifact\n"
            '        "uses": actions/upload-artifact@'
            "ea165f8d65b6e75b540449e92b4886f43607fa02 # v4\n"
            "        with:\n"
            "          name: quoted-key-release-artifact\n"
            "          path: image.json\n"
        )
        quoted_upload_action = insert_step_after(
            source, "Upload immutable image identity", quoted_upload_step
        )
        unicode_escaped_upload_key = insert_step_after(
            source,
            "Upload immutable image identity",
            "      - name: Unicode-escaped-key extra release artifact\n"
            '        "u\\u0073es": actions/upload-artifact@'
            "ea165f8d65b6e75b540449e92b4886f43607fa02 # v4\n"
            "        with:\n"
            "          name: unicode-key-release-artifact\n"
            "          path: image.json\n",
        )
        explicit_upload_key = insert_step_after(
            source,
            "Upload immutable image identity",
            "      - name: Explicit-key extra release artifact\n"
            "        ? uses\n"
            "        : actions/upload-artifact@"
            "ea165f8d65b6e75b540449e92b4886f43607fa02 # v4\n"
            "        with:\n"
            "          name: explicit-key-release-artifact\n"
            "          path: image.json\n",
        )
        matrix_assignment = (
            "          matrix=\"$(jq -c '[.release.components | to_entries[] | "
            ".value + {component: .key}]' \"$manifest\")\"\n"
            '          [[ "$(jq \'length\' <<< "$matrix")" -eq 3 ]]'
        )
        fourth_matrix_row = mutate_authority_matrix(
            matrix_assignment,
            matrix_assignment.splitlines()[0]
            + "\n"
            + "          matrix=\"$(jq -c '. + [.[-1]]' <<< \"$matrix\")\"\n"
            + '          [[ "$(jq \'length\' <<< "$matrix")" -eq 4 ]]',
        )
        authority_output_override = source.replace(
            "      matrix: ${{ steps.source.outputs.matrix }}",
            "      matrix: '[{\"component\":\"web\"},"
            "{\"component\":\"meta-relay\"},"
            "{\"component\":\"gmail-relay\"},"
            "{\"component\":\"web\"}]'",
            1,
        )
        build_extra_axis = mutate_job(
            "build",
            "      matrix:\n"
            "        include: ${{ fromJSON(needs.authority.outputs.matrix) }}",
            "      matrix:\n"
            "        include: ${{ fromJSON(needs.authority.outputs.matrix) }}\n"
            "        unreviewed: [one, two]",
        )
        aggregate_extra_axis = mutate_job(
            "aggregate",
            "    steps:",
            "    strategy:\n"
            "      matrix:\n"
            "        unreviewed: [one, two]\n"
            "    steps:",
        )
        verify_set_extra_axis = mutate_job(
            "verify_set",
            "    steps:",
            "    strategy:\n"
            "      matrix:\n"
            "        unreviewed: [one, two]\n"
            "    steps:",
        )
        conditional_upload = mutate_job(
            "build",
            "      - name: Upload immutable image identity\n",
            "      - name: Upload immutable image identity\n"
            "        if: always()\n",
        )
        tolerated_upload_failure = mutate_job(
            "build",
            "      - name: Upload immutable image identity\n",
            "      - name: Upload immutable image identity\n"
            "        continue-on-error: true\n",
        )
        mutations = (
            removed_record_control,
            mutate_build(
                'DOCKER_BUILD_RECORD_UPLOAD: "false"',
                'DOCKER_BUILD_RECORD_UPLOAD: "true"',
            ),
            mutate_build(
                f"uses: {EXACT_IMAGE_BUILD_ACTION}",
                f"# uses: {EXACT_IMAGE_BUILD_ACTION}\n"
                "        uses: docker/build-push-action@"
                "0000000000000000000000000000000000000001 # v6",
            ),
            duplicate_record_control,
            moved_record_control,
            second_build_action,
            precolon_build_action,
            quoted_value_build_action,
            flow_build_action,
            spaced_inline_build_action,
            newline_flow_build_action,
            anchored_flow_build_action,
            case_variant_build_action,
            second_upload_action,
            precolon_upload_action,
            quoted_value_upload_action,
            flow_upload_action,
            spaced_inline_upload_action,
            newline_flow_upload_action,
            anchored_flow_upload_action,
            case_variant_upload_action,
            quoted_upload_action,
            unicode_escaped_upload_key,
            explicit_upload_key,
            fourth_matrix_row,
            authority_output_override,
            build_extra_axis,
            aggregate_extra_axis,
            verify_set_extra_axis,
            conditional_upload,
            tolerated_upload_failure,
            mutate_gate(".total_count <= 14", ".total_count <= 17"),
            mutate_gate(
                "                .total_count <= 14 and\n"
                '                (.artifacts | type) == "array" and\n'
                "                (.artifacts | length) <= 14 and",
                "                true and\n"
                "                # .total_count <= 14 and\n"
                '                (.artifacts | type) == "array" and\n'
                "                # (.artifacts | length) <= 14 and",
            ),
            mutate_gate(
                ".total_count == (.artifacts | length)",
                ".total_count >= (.artifacts | length)",
            ),
            mutate_gate(
                "([.artifacts[].name] | length) == "
                "([.artifacts[].name] | unique | length)",
                "true",
            ),
            mutate_gate(
                '(.name | endswith(".dockerbuild") | not)',
                '(.name | type) == "string"',
            ),
            mutate_gate(
                "(.name as $name | ($expected_names | index($name)) != null)",
                "(.name | type) == \"string\"",
            ),
            mutate_gate(".expired == false", "true"),
            mutate_gate(".id > 0", ".id >= 0"),
            mutate_gate(".size_in_bytes > 0", ".size_in_bytes >= 0"),
            mutate_gate(
                ".size_in_bytes <= 67108864",
                ".size_in_bytes <= 67108865",
            ),
            mutate_gate(
                '(.digest | test("^sha256:[0-9a-f]{64}$"))',
                '(.digest | startswith("sha256:"))',
            ),
            mutate_gate(
                ".workflow_run.id == $run_id",
                ".workflow_run.id > 0",
            ),
            mutate_gate(
                '.workflow_run.head_branch == "main"',
                '(.workflow_run.head_branch | type) == "string"',
            ),
            mutate_gate(
                ".workflow_run.head_sha == $control_sha",
                '(.workflow_run.head_sha | type) == "string"',
            ),
            mutate_gate_function(
                "require_bounded_artifact_inventory",
                '--arg control_sha "$CONTROL_SHA" \\',
                '--arg control_sha "$VALIDATION_RUN_ID" \\',
            ),
            mutate_gate_function(
                "require_bounded_artifact_inventory",
                '--argjson run_id "$GITHUB_RUN_ID" \'',
                '--argjson run_id "$VALIDATION_RUN_ID" \'',
            ),
            mutate_gate(".total_count == 14", ".total_count >= 14"),
            mutate_gate(
                "([.artifacts[].name] | sort) == $expected_names",
                "($expected_names - [.artifacts[].name]) == []",
            ),
            mutate_gate(
                '"/repos/$REPOSITORY/actions/runs/$GITHUB_RUN_ID/'
                'artifacts?per_page=100"',
                '"/repos/$REPOSITORY/actions/runs/$VALIDATION_RUN_ID/'
                'artifacts?per_page=100"',
                2,
            ),
            mutate_gate(
                "            artifacts_json=\"$(gh api \\\n"
                "              \"/repos/$REPOSITORY/actions/runs/"
                "$GITHUB_RUN_ID/artifacts?per_page=100\")\"",
                "            artifacts_json=\"$(gh api \\\n"
                "              \"/repos/$REPOSITORY/actions/runs/"
                "$GITHUB_RUN_ID/artifacts?per_page=100\" | \\\n"
                "              jq '.artifacts |= map(select((.name | "
                "endswith(\".dockerbuild\")) | not)) | "
                ".total_count = (.artifacts | length)')\"",
            ),
            mutate_gate(
                "for delay_seconds in 0 1 2 4 8; do",
                "for delay_seconds in 0 1 2 4 8 16; do",
            ),
            mutate_gate("sleep 2", "sleep 0"),
            mutate_gate(
                '[[ "$first_artifact_inventory" == "$final_artifact_inventory" ]]',
                "true",
            ),
            mutate_gate(
                '          require_exact_artifact_inventory "$final_artifacts_json"',
                "          true\n"
                '          # require_exact_artifact_inventory "$final_artifacts_json"',
            ),
            mutate_gate(
                '          require_exact_artifact_inventory "$final_artifacts_json"',
                '          final_artifacts_json="$artifacts_json"\n'
                '          require_exact_artifact_inventory "$final_artifacts_json"',
            ),
            mutate_gate(
                "digest,\n                expired,",
                "expired,",
            ),
        )
        for index, mutated in enumerate(mutations):
            with self.subTest(mutation=index):
                self.assertNotEqual(mutated, source)
                with self.assertRaises(AssertionError):
                    assert_exact_release_image_artifact_controls(mutated)


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

    def test_recovery_unsigned_artifact_name_matches_strict_orphan_consumer(self) -> None:
        recovery = workflow("verify-production-recovery-readiness.yml")
        names = step_block(recovery, "Create sanitized sidecar and internal artifact")
        upload = step_block(recovery, "Upload unsigned sanitized recovery evidence")
        orphan = workflow("rollback-production-orphan.yml")

        producer = (
            "internal_name=unsigned-production-recovery-readiness-%s-%s\\n"
        )
        self.assertEqual(names.count(producer), 1)
        self.assertNotIn("internal_name=unsigned-production-recovery-%s-%s\\n", names)
        self.assertEqual(
            upload.count("name: ${{ steps.names.outputs.internal_name }}"), 1
        )
        self.assertEqual(
            orphan.count(
                "`unsigned-production-recovery-readiness-${value.recovery.run_id}-1`"
            ),
            1,
        )
        self.assertNotIn(
            "`unsigned-production-recovery-${value.recovery.run_id}-1`", orphan
        )

    def test_provider_native_valkey_fork_is_exact_token_isolated_and_terminally_cleaned(self) -> None:
        prepare = workflow("prepare-production-valkey-recovery-fork.yml")
        recovery = workflow("verify-production-recovery-readiness.yml")
        cleanup = workflow("cleanup-production-valkey-recovery-fork.yml")
        finalizer = workflow("finalize-production-orphan-lock.yml")

        self.assertEqual(
            job_names(prepare),
            (
                "Authenticate exact production Valkey recovery fork authority",
                "Prepare and attest exact production Valkey recovery fork intent",
                "Create exact production Valkey recovery fork",
                "Exact production Valkey recovery fork gate",
            ),
        )
        self.assertEqual(
            job_names(recovery),
            (
                "Authenticate exact production recovery authority",
                "Observe exact production recovery state",
                "Exact production recovery readiness gate",
            ),
        )
        self.assertEqual(
            job_names(cleanup),
            (
                "Authenticate exact Valkey recovery cleanup authority",
                "Delete or reconcile exact production Valkey recovery fork",
                "Exact production Valkey recovery cleanup gate",
            ),
        )

        artifact_id_download_counts = {
            "prepare": (prepare, 4),
            "recovery": (recovery, 2),
            "cleanup": (cleanup, 6),
        }
        for workflow_name, (source, expected_count) in artifact_id_download_counts.items():
            downloads = tuple(
                step
                for step in workflow_step_blocks(source)
                if re.search(r"(?m)^          artifact-ids:", step)
            )
            with self.subTest(artifact_id_downloads=workflow_name):
                self.assertEqual(len(downloads), expected_count)
                for download in downloads:
                    self.assertEqual(
                        len(
                            re.findall(
                                r"(?m)^          merge-multiple: true[ \t]*$",
                                download,
                            )
                        ),
                        1,
                    )

        intent_step = step_block(
            prepare, "Prepare exact immutable fork intent with GET-only controller path"
        )
        create_step = step_block(
            prepare, "Create one exact provider-native Valkey recovery fork"
        )
        create_gate_step = step_block(
            prepare, "Verify signed intent and live-bind exact clean-create receipt"
        )
        observe_step = step_block(
            recovery,
            "Observe recovery with the GET-only database read capability and exact app admission",
        )
        delete_step = step_block(
            cleanup, "Delete or reconcile one exact provider-native recovery fork"
        )
        cleanup_gate = step_block(
            cleanup, "Validate exact sanitized cleanup receipt"
        )
        cleanup_authority = step_block(cleanup, "Authenticate exact cleanup evidence")
        cleanup_verify = step_block(
            cleanup, "Verify signed cleanup authorities before delete capability"
        )
        recovery_fork_download = step_block(
            recovery, "Download exact signed Valkey fork receipt"
        )
        recovery_unsigned_download = step_block(
            recovery, "Download unsigned evidence"
        )
        cleanup_downloads = (
            ("Download exact fork create authority", "prepare_run_id"),
            ("Download exact terminal recovery evidence", "recovery_run_id"),
            ("Download exact terminal phase state", "canary_run_id"),
            (
                "Download exact no-mutation reconciliation",
                "reconciliation_run_id",
            ),
            ("Download exact cleanup create authority", "prepare_run_id"),
        )
        self.assertIn("github-token: ${{ github.token }}", recovery_fork_download)
        self.assertIn("repository: ${{ github.repository }}", recovery_fork_download)
        self.assertIn(
            "run-id: ${{ needs.authority.outputs.fork_run_id }}",
            recovery_fork_download,
        )
        self.assertIn("artifact-ids:", recovery_fork_download)
        self.assertEqual(recovery_fork_download.count("merge-multiple: true"), 1)
        self.assertIn("artifact-ids:", recovery_unsigned_download)
        self.assertEqual(recovery_unsigned_download.count("merge-multiple: true"), 1)
        for step_name, run_output in cleanup_downloads:
            with self.subTest(cross_run_download=step_name):
                download = step_block(cleanup, step_name)
                self.assertIn("github-token: ${{ github.token }}", download)
                self.assertIn("repository: ${{ github.repository }}", download)
                self.assertIn(
                    f"run-id: ${{{{ needs.authority.outputs.{run_output} }}}}",
                    download,
                )
        same_run_cleanup = step_block(
            cleanup, "Download exact unsigned cleanup receipt"
        )
        self.assertNotIn("github-token:", same_run_cleanup)
        self.assertNotIn("run-id:", same_run_cleanup)
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            intent_step,
        )
        self.assertNotIn("DATABASE_CREATE_TOKEN", intent_step)
        self.assertIn(
            "DO_PRODUCTION_DATABASE_CREATE_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_CREATE_TOKEN }}",
            create_step,
        )
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            create_step,
        )
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            create_gate_step,
        )
        self.assertIn("validate-create-receipt", create_gate_step)
        self.assertIn("--intent-sha256 \"$INTENT_SHA256\"", create_gate_step)
        self.assertNotIn("DATABASE_CREATE_TOKEN", create_gate_step)
        self.assertNotIn("DATABASE_DELETE_TOKEN", create_gate_step)
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            observe_step,
        )
        self.assertNotIn("DATABASE_CREATE_TOKEN", observe_step)
        self.assertNotIn("DATABASE_DELETE_TOKEN", observe_step)
        self.assertIn(
            "DO_PRODUCTION_DATABASE_DELETE_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_DELETE_TOKEN }}",
            delete_step,
        )
        self.assertNotIn("DATABASE_CREATE_TOKEN", delete_step)
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            delete_step,
        )
        self.assertIn(
            'authority_contract_sha="$(sha256sum "authority-control/$CONTRACT_PATH"',
            delete_step,
        )
        self.assertIn(
            '"contract_sha256":os.environ["authority_contract_sha"]',
            delete_step,
        )
        self.assertIn('--contract "authority-control/$CONTRACT_PATH"', delete_step)
        self.assertNotIn('--contract "control/$CONTRACT_PATH"', delete_step)
        self.assertIn('"control/$CONTROLLER_PATH" delete-or-reconcile', delete_step)
        self.assertNotIn(
            '"authority-control/$CONTROLLER_PATH" delete-or-reconcile', delete_step
        )
        self.assertIn(
            '"authority_workflow_sha":os.environ["AUTHORITY_CONTROL_SHA"]',
            delete_step,
        )
        self.assertIn(
            '"authority_controller_sha256":os.environ["authority_controller_sha"]',
            delete_step,
        )
        self.assertIn(
            'authority_controller_sha="$(sha256sum "authority-control/$CONTROLLER_PATH"',
            delete_step,
        )
        self.assertIn(
            "DO_PRODUCTION_DATABASE_READ_TOKEN: ${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}",
            cleanup_gate,
        )
        self.assertIn("validate-delete-receipt", cleanup_gate)
        self.assertIn(
            'authority_contract_sha="$(sha256sum "authority-control/$CONTRACT_PATH"',
            cleanup_gate,
        )
        self.assertIn(
            '"contract_sha256":os.environ["authority_contract_sha"]',
            cleanup_gate,
        )
        self.assertIn(
            '--contract "authority-control/$CONTRACT_PATH"', cleanup_gate
        )
        self.assertNotIn('--contract "control/$CONTRACT_PATH"', cleanup_gate)
        self.assertIn('"control/$CONTROLLER_PATH" validate-delete-receipt', cleanup_gate)
        self.assertNotIn(
            '"authority-control/$CONTROLLER_PATH" validate-delete-receipt',
            cleanup_gate,
        )
        self.assertIn(
            '"authority_workflow_sha":os.environ["AUTHORITY_CONTROL_SHA"]',
            cleanup_gate,
        )
        self.assertIn(
            '"authority_controller_sha256":os.environ["authority_controller_sha"]',
            cleanup_gate,
        )
        self.assertIn('cmp --silent "$subject"', cleanup_gate)
        self.assertNotIn("DATABASE_CREATE_TOKEN", cleanup_gate)
        self.assertNotIn("DATABASE_DELETE_TOKEN", cleanup_gate)
        self.assertEqual(
            prepare.count("${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}"), 3
        )
        self.assertEqual(
            prepare.count("${{ secrets.DO_PRODUCTION_DATABASE_CREATE_TOKEN }}"), 1
        )
        self.assertEqual(
            cleanup.count("${{ secrets.DO_PRODUCTION_DATABASE_DELETE_TOKEN }}"), 1
        )
        self.assertEqual(
            cleanup.count("${{ secrets.DO_PRODUCTION_DATABASE_READ_TOKEN }}"), 2
        )

        for source in (prepare, recovery, cleanup):
            self.assertNotIn("view_credentials", source)
            self.assertNotIn("api:write", source)
            self.assertNotIn("DO_PRODUCTION_VALKEY_SENTINEL", source)
            self.assertNotIn("HMAC_KEY", source)
        self.assertIn("production-valkey-recovery-fork-create-intent/v2", prepare)
        self.assertIn("production-valkey-recovery-fork-create-receipt/v2", prepare)
        self.assertIn("production-recovery-readiness/v2", recovery)
        self.assertIn("production-valkey-recovery-fork-delete-receipt/v2", cleanup)
        self.assertIn("validate-delete-receipt", cleanup)
        self.assertIn("CLEANUP_EVIDENCE_SHA256", cleanup)
        for mode in ("terminal", "never-started", "no-mutation", "quarantine"):
            self.assertIn(mode, cleanup)
        self.assertIn("now>=expires", cleanup)
        self.assertIn("classification\"][\"outcome\"]==\"no-mutation\"", cleanup)
        self.assertIn("never-started cleanup recovery authority differs", cleanup)
        self.assertIn("never-started cleanup found nonterminal recovery evidence", cleanup)
        self.assertIn("state[\"evidence\"][\"recovery_sha256\"]", cleanup)
        self.assertIn(
            "recovery[\"authorities\"][\"valkey_fork\"][\"receipt_sha256\"]",
            cleanup,
        )
        self.assertIn(
            "artifact.digest!==process.env.ARTIFACT_DIGEST", cleanup
        )
        self.assertIn(
            "['authority','authority_control_sha','canary','confirmation','control_sha','mode','phase','reconciliation','recovery']",
            cleanup_authority,
        )
        self.assertIn("compareCommitsWithBasehead", cleanup_authority)
        self.assertIn(
            "ancestry.data.merge_base_commit.sha!==value.authority_control_sha",
            cleanup_authority,
        )
        self.assertIn("ancestry.data.behind_by!==0", cleanup_authority)
        self.assertIn("run.head_sha!==expectedSha", cleanup_authority)
        self.assertIn(
            "createArtifact.data.workflow_run.head_sha!==value.authority_control_sha",
            cleanup_authority,
        )
        self.assertIn(
            "runs.some(run=>Date.parse(run.created_at)>=Date.parse(createRun.created_at))",
            cleanup_authority,
        )
        self.assertNotIn(
            "runs.some(run=>run.head_sha===value.authority_control_sha",
            cleanup_authority,
        )
        self.assertEqual(
            cleanup_verify.count('--signer-digest "$AUTHORITY_CONTROL_SHA"'), 4
        )
        self.assertNotIn('--signer-digest "$CONTROL_SHA"', cleanup_verify)
        for checkout_name in (
            "Check out exact cleanup authority controls",
            "Check out exact cleanup authority controls for final binding",
        ):
            checkout = step_block(cleanup, checkout_name)
            self.assertIn(
                "ref: ${{ needs.authority.outputs.authority_control_sha }}",
                checkout,
            )
            self.assertIn("path: authority-control", checkout)
        recovery_inventory = step_block(
            recovery, "Require exact pre-signing recovery job and artifact inventory"
        )
        self.assertIn("listWorkflowRunArtifacts", recovery_inventory)
        self.assertIn("total_count!==1", recovery_inventory)
        self.assertIn("artifact.digest!==process.env.ARTIFACT_DIGEST", recovery_inventory)
        recovery_gate = step_block(
            recovery, "Require exact receipt and unchanged authority"
        )
        self.assertIn("expected_control=expected_control", recovery_gate)
        self.assertIn("cleanup-gate-control.json", cleanup_gate)
        self.assertIn("cleanup-gate-target.json", cleanup_gate)
        self.assertIn("validated-cleanup-receipt", cleanup_gate)
        self.assertNotIn('control=dict(value["control"])', cleanup_gate)
        runbook = (ROOT / "docs" / "crm-production-release-control.md").read_text(
            encoding="utf-8"
        )
        self.assertIn("`database:create` and `vpc:read`", runbook)
        self.assertIn("exactly one `app` trusted-source rule", runbook)
        self.assertIn("`authority_control_sha`", runbook)
        self.assertIn("exact ancestor of current `main`", runbook)
        self.assertNotIn("fork firewall must be\nempty", runbook)

        self.assertIn(
            "retention-days: 30",
            step_block(prepare, "Upload exact signed fork intent"),
        )
        self.assertIn(
            "retention-days: 2",
            step_block(prepare, "Upload unsigned exact fork receipt"),
        )
        self.assertIn(
            "retention-days: 30",
            step_block(prepare, "Upload exact signed fork receipt"),
        )
        self.assertIn(
            "retention-days: 30",
            step_block(recovery, "Upload exact signed recovery artifact"),
        )

        self.assertEqual(
            prepare.count(
                "artifact_name=production-valkey-recovery-fork-create-intent-%s-%s\\n"
            ),
            1,
        )
        self.assertEqual(
            prepare.count(
                "artifact_name=unsigned-production-valkey-recovery-fork-create-%s-%s\\n"
            ),
            1,
        )
        self.assertEqual(
            prepare.count(
                "production-valkey-recovery-fork-create-${{ github.run_id }}-${{ github.run_attempt }}"
            ),
            1,
        )
        self.assertEqual(
            cleanup.count(
                "unsigned-production-valkey-recovery-fork-delete-%s-%s\\n"
            ),
            1,
        )
        self.assertEqual(
            cleanup.count(
                "production-valkey-recovery-fork-delete-${{ github.run_id }}-${{ github.run_attempt }}"
            ),
            1,
        )
        for path in (
            ".github/workflows/prepare-production-valkey-recovery-fork.yml",
            ".github/workflows/cleanup-production-valkey-recovery-fork.yml",
        ):
            self.assertEqual(finalizer.count(path), 3)
        for name in (
            "Prepare Production Valkey Recovery Fork",
            "Cleanup Production Valkey Recovery Fork",
        ):
            self.assertEqual(finalizer.count(name), 1)

    def test_recovery_consumers_bind_v2_and_exact_observer_source(self) -> None:
        apply_source = (
            ROOT / "release" / "deployment" / "apply_production_change.py"
        ).read_text(encoding="utf-8")
        workflows = (
            workflow("apply-production-phase.yml"),
            workflow("rollback-production-phase.yml"),
            workflow("rollback-production-orphan.yml"),
            workflow("verify-production-recovery-readiness.yml"),
        )
        self.assertIn(
            'with_name(\n        "observe_production_recovery.py"\n    )',
            apply_source,
        )
        self.assertIn(
            "recovery evidence is not bound to the current recovery controller",
            apply_source,
        )
        for source in workflows:
            self.assertIn("production-recovery-readiness/v2", source)
            self.assertNotIn("production-recovery-readiness/v1", source)

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
