#!/usr/bin/env bash

# Verify one immutable release source's compile-time PostgreSQL rollout role and
# the shared compatibility/coordinator contract. This harness is intentionally
# supplied by the protected control checkout, never by the product checkout it
# is evaluating.
set -euo pipefail

readonly authority="exact-database-phase-compatibility/v1"
readonly harness_path="release/validation/verify_database_phase_compatibility.sh"
readonly package="./internal/database"
readonly unit_tests=(
  TestRLSMigrationPhasePolicyIsCompileTimeAndFailClosed
  TestRLSMigrationLostAcknowledgementClassificationIsReadOnlyAndFailClosed
  TestMigrationActivationIsSeparatedFromPreparation
)
readonly integration_tests=(
  TestBaselineRLSMigrationPreservesLegacyProfileAndIsIdempotent
  TestBaselineRLSMigrationAcceptsExactPreAdditiveLegacyPredecessor
  TestBaselineRLSMigrationPinsPublicBeforePreAdditivePreparation
  TestBaselineRLSMigrationRejectsDisabledForeignKeyEnforcementBeforeCallbacks
  TestBaselineRLSMigrationRejectsSettableDefaultPrivilegeBeforeTableCreation
  TestBaselineRLSMigrationQuarantinesMalformedCatalogBeforeCallbacks
  TestRLSMigrationFingerprintVerificationNeverExecutesStoredBody
  TestRLSMigrationCallbackScanAuthorityRejectsRawDistinctCanonicalEquivalentPolicy
  TestTenantRLSRejectsCreateRoleRuntimeAuthority
  TestVerifyTenantRLSAcceptsExactSeparatedRuntimeAuthority
  TestVerifyTenantRLSRejectsPublicBtrimRoutingIndexShadow
  TestTenantRLSRejectsSessionReplicationRoleParameterAuthority
  TestTenantRLSRejectsPersistedReplicaSessionDefault
  TestApplyTenantRLSRejectsReplicaMigrationSession
  TestTenantRLSRejectsDangerousRuntimeTableAuthority
  TestTenantRLSRejectsDangerousRuntimeDefaultTableAuthority
  TestBaselineRLSMigrationOnFutureProfileIsReadOnlyAndRepeatable
  TestRLSMigrationCoordinatorRejectsFutureOnlyPhasesAndRecoversAfterLateBridgeFailure
)
readonly tests=("${unit_tests[@]}" "${integration_tests[@]}")

phase=""
source_dir=""
manifest_path=""
workflow_sha=""
workflow_path=""
workflow_repository=""
runner_environment=""
output_path=""

usage() {
  cat >&2 <<'EOF'
usage: verify_database_phase_compatibility.sh \
  --phase baseline|bridge|backend|ui \
  --source-dir PATH \
  --manifest PATH \
  --workflow-sha SHA1 \
  --workflow-path PATH \
  --workflow-repository OWNER/REPOSITORY \
  --runner-environment github-hosted \
  --output PATH
EOF
  exit 2
}

set_once() {
  local name="$1"
  local current="$2"
  local value="$3"
  [[ -z "$current" ]] || {
    printf 'duplicate option: %s\n' "$name" >&2
    exit 2
  }
  printf '%s' "$value"
}

while [[ "$#" -gt 0 ]]; do
  [[ "$#" -ge 2 ]] || usage
  case "$1" in
    --phase)
      phase="$(set_once "$1" "$phase" "$2")"
      ;;
    --source-dir)
      source_dir="$(set_once "$1" "$source_dir" "$2")"
      ;;
    --manifest)
      manifest_path="$(set_once "$1" "$manifest_path" "$2")"
      ;;
    --workflow-sha)
      workflow_sha="$(set_once "$1" "$workflow_sha" "$2")"
      ;;
    --workflow-path)
      workflow_path="$(set_once "$1" "$workflow_path" "$2")"
      ;;
    --workflow-repository)
      workflow_repository="$(set_once "$1" "$workflow_repository" "$2")"
      ;;
    --runner-environment)
      runner_environment="$(set_once "$1" "$runner_environment" "$2")"
      ;;
    --output)
      output_path="$(set_once "$1" "$output_path" "$2")"
      ;;
    *)
      usage
      ;;
  esac
  shift 2
done

for required in phase source_dir manifest_path workflow_sha workflow_path workflow_repository runner_environment output_path; do
  [[ -n "${!required}" ]] || usage
done

case "$phase" in
  baseline)
    compiled_phase="rlsMigrationPhaseBaseline"
    legacy_action="prepare-legacy-without-future-activation"
    future_action="verify-future-read-only"
    future_activation="false"
    ;;
  bridge)
    compiled_phase="rlsMigrationPhaseBridge"
    legacy_action="activate-complete-future-profile"
    future_action="verify-future-read-only"
    future_activation="true"
    ;;
  backend)
    compiled_phase="rlsMigrationPhaseBackend"
    legacy_action="reject-before-mutation"
    future_action="verify-future-read-only"
    future_activation="false"
    ;;
  ui)
    compiled_phase="rlsMigrationPhaseUI"
    legacy_action="reject-before-mutation"
    future_action="verify-future-read-only"
    future_activation="false"
    ;;
  *)
    printf 'release phase is not allowlisted: %s\n' "$phase" >&2
    exit 1
    ;;
esac

[[ "$workflow_sha" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'workflow SHA is malformed\n' >&2
  exit 1
}
case "$workflow_path" in
  .github/workflows/test.yml|.github/workflows/validate-exact-release-source.yml|.github/workflows/build-attest-exact-release-images.yml)
    ;;
  *)
    printf 'workflow path is not authorized for database compatibility evidence\n' >&2
    exit 1
    ;;
esac
[[ "$runner_environment" == "github-hosted" ]] || {
  printf 'database compatibility evidence requires a GitHub-hosted runner\n' >&2
  exit 1
}
[[ -n "${TEST_DATABASE_URL:-}" ]] || {
  printf 'TEST_DATABASE_URL is required; compatibility tests must not skip PostgreSQL\n' >&2
  exit 1
}

readonly script_path="$(realpath -e -- "${BASH_SOURCE[0]}")"
readonly source_root="$(realpath -e -- "$source_dir")"
readonly manifest_file="$(realpath -e -- "$manifest_path")"
[[ -f "$script_path" && ! -L "${BASH_SOURCE[0]}" ]] || {
  printf 'compatibility harness must be a regular non-symlink file\n' >&2
  exit 1
}
[[ -d "$source_root" && ! -L "$source_dir" ]] || {
  printf 'source checkout must be a regular directory\n' >&2
  exit 1
}
[[ -f "$manifest_file" && ! -L "$manifest_path" ]] || {
  printf 'source manifest must be a regular non-symlink file\n' >&2
  exit 1
}

readonly harness_sha256="$(sha256sum "$script_path" | awk '{print $1}')"
readonly manifest_sha256="$(sha256sum "$manifest_file" | awk '{print $1}')"

mapfile -t manifest_values < <(
  python3 - "$manifest_file" "$phase" "$authority" "$harness_path" "$harness_sha256" <<'PY'
import json
import re
import sys
from pathlib import Path


def reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


manifest_path, phase, authority, harness_path, harness_sha256 = sys.argv[1:]
try:
    raw = Path(manifest_path).read_bytes()
    if raw.startswith(b"\xef\xbb\xbf") or b"\r" in raw:
        raise ValueError("manifest encoding is non-canonical")
    manifest = json.loads(
        raw.decode("utf-8"),
        object_pairs_hook=reject_duplicate_keys,
        parse_float=lambda _value: (_ for _ in ()).throw(ValueError("float rejected")),
        parse_constant=lambda _value: (_ for _ in ()).throw(ValueError("constant rejected")),
    )
except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
    raise SystemExit(f"invalid source manifest: {exc}")

if type(manifest) is not dict or set(manifest) != {
    "schema_version", "repository", "validation", "phases", "release"
}:
    raise SystemExit("source manifest top-level contract differs")
if manifest["schema_version"] != 1 or manifest["repository"] != "medtechcorps-netizen/whatomate":
    raise SystemExit("source manifest identity differs")
validation = manifest["validation"]
if type(validation) is not dict or set(validation) != {
    "workflow_path", "gate_job_name", "database_phase_compatibility"
}:
    raise SystemExit("source manifest validation contract differs")
if validation["workflow_path"] != ".github/workflows/validate-exact-release-source.yml":
    raise SystemExit("source validation workflow differs")
if validation["gate_job_name"] != "Exact source validation gate":
    raise SystemExit("source validation gate differs")
compatibility = validation["database_phase_compatibility"]
expected_compatibility = {
    "authority": authority,
    "harness_path": harness_path,
    "harness_sha256": harness_sha256,
    "job_name": "Exact database phase compatibility",
}
if compatibility != expected_compatibility:
    raise SystemExit("database phase compatibility harness authority differs")
phases = manifest["phases"]
if type(phases) is not dict or set(phases) != {"baseline", "bridge", "backend", "ui"}:
    raise SystemExit("source manifest phase set differs")
for phase_name, phase_entry in phases.items():
    if type(phase_entry) is not dict or set(phase_entry) != {
        "source_sha", "root_tree", "frontend_tree", "internal_tree"
    }:
        raise SystemExit(f"source manifest {phase_name} entry differs")
    for key in ("source_sha", "root_tree", "frontend_tree", "internal_tree"):
        value = phase_entry[key]
        if type(value) is not str or re.fullmatch(r"[0-9a-f]{40}", value) is None:
            raise SystemExit(f"source manifest {phase_name}.{key} is malformed")
for key in ("source_sha", "root_tree", "internal_tree"):
    if len({phases[phase_name][key] for phase_name in phases}) != len(phases):
        raise SystemExit(f"database phase source allocation is not unique: {key}")
entry = phases.get(phase)
print(manifest["repository"])
print(entry["source_sha"])
print(entry["root_tree"])
print(entry["frontend_tree"])
print(entry["internal_tree"])
PY
)
[[ "${#manifest_values[@]}" -eq 5 ]] || {
  printf 'source manifest authority output differs\n' >&2
  exit 1
}
readonly repository="${manifest_values[0]}"
readonly source_sha="${manifest_values[1]}"
readonly root_tree="${manifest_values[2]}"
readonly frontend_tree="${manifest_values[3]}"
readonly internal_tree="${manifest_values[4]}"

[[ "$workflow_repository" == "$repository" ]] || {
  printf 'workflow repository differs from source authority\n' >&2
  exit 1
}

[[ "$(git -C "$source_root" rev-parse 'HEAD^{commit}')" == "$source_sha" ]]
[[ "$(git -C "$source_root" show -s --format=%T HEAD)" == "$root_tree" ]]
[[ "$(git -C "$source_root" rev-parse 'HEAD:frontend')" == "$frontend_tree" ]]
[[ "$(git -C "$source_root" rev-parse 'HEAD:internal')" == "$internal_tree" ]]
[[ -z "$(git -C "$source_root" status --porcelain)" ]] || {
  printf 'source checkout is not clean\n' >&2
  exit 1
}

readonly phase_source_path="internal/database/postgres.go"
read -r source_mode source_type source_object source_entry < <(
  git -C "$source_root" ls-tree HEAD -- "$phase_source_path"
)
[[ "$source_mode" == "100644" && "$source_type" == "blob" && "$source_entry" == "$phase_source_path" ]]
readonly phase_source_file="$(realpath -e -- "$source_root/$phase_source_path")"
[[ "$phase_source_file" == "$source_root/"* ]]
[[ -f "$phase_source_file" && ! -L "$source_root/$phase_source_path" ]]
[[ "$(git -C "$source_root" hash-object "$phase_source_path")" == "$source_object" ]]

python3 - "$phase_source_file" "$compiled_phase" <<'PY'
import re
import sys
from pathlib import Path

path, expected = sys.argv[1:]
raw = Path(path).read_bytes()
if raw.startswith(b"\xef\xbb\xbf") or b"\r" in raw:
    raise SystemExit("database phase source encoding is non-canonical")
text = raw.decode("utf-8")
declarations = re.findall(
    r"(?m)^const compiledRLSMigrationPhase = (rlsMigrationPhase[A-Za-z]+)$",
    text,
)
if declarations != [expected]:
    raise SystemExit(
        "compiled database phase authority differs: "
        f"expected {expected}, found {declarations!r}"
    )
phase_type = re.findall(r"(?m)^type rlsMigrationPhase uint8$", text)
if phase_type != ["type rlsMigrationPhase uint8"]:
    raise SystemExit("database phase authority type differs")
enum_blocks = re.findall(
    r"(?m)^const \(\n"
    r"\trlsMigrationPhaseBaseline rlsMigrationPhase = iota\n"
    r"\trlsMigrationPhaseBridge\n"
    r"\trlsMigrationPhaseBackend\n"
    r"\trlsMigrationPhaseUI\n"
    r"\)$",
    text,
)
if len(enum_blocks) != 1:
    raise SystemExit("database phase authority enumeration differs")
production_binding = re.findall(
    r"(?ms)^func RunRLSMigrationCoordinator\(\n"
    r"\tdb \*gorm\.DB,\n"
    r"\tadminCfg \*config\.DefaultAdminConfig,\n"
    r"\truntimeRole string,\n"
    r"\tbackfill func\(\*gorm\.DB\) error,\n"
    r"\tverifyRuntime func\(\) error,\n"
    r"\) error \{\n"
    r"\treturn runRLSMigrationCoordinatorForPhase\(\n"
    r"\t\tdb,\n"
    r"\t\tadminCfg,\n"
    r"\t\truntimeRole,\n"
    r"\t\tcompiledRLSMigrationPhase,\n"
    r"\t\tbackfill,\n"
    r"\t\tverifyRuntime,\n"
    r"\t\)\n"
    r"\}$",
    text,
)
if len(production_binding) != 1:
    raise SystemExit("production coordinator is not bound to compile-time phase authority")
for forbidden in (
    "var compiledRLSMigrationPhase",
    "os.Getenv(\"REREPLY_RLS_MIGRATION_PHASE\")",
    "os.LookupEnv(\"REREPLY_RLS_MIGRATION_PHASE\")",
    "//go:linkname compiledRLSMigrationPhase",
):
    if forbidden in text:
        raise SystemExit(f"runtime database phase selector is forbidden: {forbidden}")
PY

selector="^("
separator=""
for test_name in "${tests[@]}"; do
  selector+="$separator$test_name"
  separator="|"
done
selector+=")$"

readonly work_dir="$(mktemp -d)"
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

(
  cd "$source_root"
  go test -mod=readonly "$package" -list "$selector"
) | grep '^Test' | LC_ALL=C sort > "$work_dir/listed-tests"
printf '%s\n' "${tests[@]}" | LC_ALL=C sort > "$work_dir/expected-tests"
diff -u "$work_dir/expected-tests" "$work_dir/listed-tests"

set +e
(
  cd "$source_root"
  go test -mod=readonly -v "$package" -run "$selector" -count=1 -timeout 30m
) 2>&1 | tee "$work_dir/go-test.log"
test_status="${PIPESTATUS[0]}"
set -e
[[ "$test_status" -eq 0 ]] || exit "$test_status"
for test_name in "${tests[@]}"; do
  [[ "$(grep -Ec "^--- PASS: ${test_name}( |$)" "$work_dir/go-test.log")" -eq 1 ]] || {
    printf 'required compatibility test did not pass exactly once: %s\n' "$test_name" >&2
    exit 1
  }
  [[ "$(grep -Ec "^--- (SKIP|FAIL): ${test_name}( |$)" "$work_dir/go-test.log")" -eq 0 ]]
done
[[ -z "$(git -C "$source_root" status --porcelain)" ]] || {
  printf 'compatibility tests mutated the immutable source checkout\n' >&2
  exit 1
}

readonly test_inventory_sha256="$({
  printf '%s\n' "${unit_tests[@]}"
  printf '%s\n' "${integration_tests[@]}"
} | sha256sum | awk '{print $1}')"
readonly output_parent="$(dirname -- "$output_path")"
mkdir -p -- "$output_parent"
[[ ! -L "$output_path" ]] || {
  printf 'compatibility evidence output cannot be a symlink\n' >&2
  exit 1
}
readonly temporary_output="$(mktemp "$output_parent/.database-phase-compatibility.XXXXXX")"

python3 - \
  "$temporary_output" "$authority" "$phase" "$repository" "$source_sha" \
  "$root_tree" "$frontend_tree" "$internal_tree" "$manifest_sha256" \
  "$workflow_path" "$workflow_sha" "$runner_environment" "$harness_path" \
  "$workflow_repository" "$harness_sha256" "$phase_source_path" "$compiled_phase" "$legacy_action" \
  "$future_action" "$future_activation" "$package" "$selector" \
  "$test_inventory_sha256" "${unit_tests[*]}" "${integration_tests[*]}" <<'PY'
import json
import sys
from pathlib import Path

(
    output_path,
    authority,
    phase,
    repository,
    source_sha,
    root_tree,
    frontend_tree,
    internal_tree,
    manifest_sha256,
    workflow_path,
    workflow_sha,
    runner_environment,
    harness_path,
    workflow_repository,
    harness_sha256,
    phase_source_path,
    compiled_phase,
    legacy_action,
    future_action,
    future_activation,
    package,
    selector,
    test_inventory_sha256,
    unit_tests,
    integration_tests,
) = sys.argv[1:]

value = {
    "authority": authority,
    "compatibility": {
        "future_activation_from_legacy": future_activation == "true",
        "future_database": future_action,
        "future_replay": "verify-future-read-only",
        "legacy_database": legacy_action,
    },
    "compile_time": {
        "constant": "compiledRLSMigrationPhase",
        "path": phase_source_path,
        "value": compiled_phase,
    },
    "harness": {
        "path": harness_path,
        "sha256": harness_sha256,
    },
    "phase": phase,
    "schema_version": 1,
    "source": {
        "commit": source_sha,
        "frontend_tree": frontend_tree,
        "internal_tree": internal_tree,
        "manifest_sha256": manifest_sha256,
        "repository": repository,
        "root_tree": root_tree,
    },
    "tests": {
        "integration": integration_tests.split(),
        "inventory_sha256": test_inventory_sha256,
        "package": package,
        "result": "passed",
        "selector": selector,
        "unit": unit_tests.split(),
    },
    "workflow": {
        "path": workflow_path,
        "repository": workflow_repository,
        "runner_environment": runner_environment,
        "sha": workflow_sha,
    },
}
Path(output_path).write_bytes(
    json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True).encode("ascii")
    + b"\n"
)
PY

chmod 0600 "$temporary_output"
mv -f -- "$temporary_output" "$output_path"
printf 'Exact %s database phase compatibility passed for %s.\n' "$phase" "$source_sha"
