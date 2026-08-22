#!/usr/bin/env bash

set -Eeuo pipefail

test_fail() {
	printf 'bootstrap operator test failed: %s\n' "$1" >&2
	exit 1
}

readonly TEST_RUNNER_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/rereply-bootstrap-operator-test.XXXXXX")"
export RUNNER_TEMP="$TEST_RUNNER_TEMP"
export GITHUB_RUN_ID="9001001"
export GITHUB_RUN_ATTEMPT="1"
export GITHUB_SHA="869679c1ac44a2d56cf94929b18644f4127809d2"
export BOOTSTRAP_PHASE="dry-run"
export BOOTSTRAP_ORGANIZATION_ID="11111111-1111-4111-8111-111111111111"

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=bootstrap-platform-compliance-once.sh
source "${SCRIPT_DIR}/bootstrap-platform-compliance-once.sh"

# Local Windows verification uses gojq, whose compact output is already
# key-sorted but whose CLI does not accept jq's -S spelling. CI uses jq. Keep
# the harness portable without changing the operator's production requirement.
readonly JQ_BIN="$(command -v jq)"
if "$JQ_BIN" -S -c '.' <<<'{}' >/dev/null 2>&1; then
	readonly JQ_ACCEPTS_SORT_KEYS=1
else
	readonly JQ_ACCEPTS_SORT_KEYS=0
fi
jq() {
	local argument
	local -a arguments=()
	for argument in "$@"; do
		if [[ "$JQ_ACCEPTS_SORT_KEYS" == "0" && "$argument" == "-S" ]]; then
			continue
		fi
		arguments+=("$argument")
	done
	"$JQ_BIN" "${arguments[@]}"
}

cleanup_test() {
	scrub_work_dir
	rmdir "$TEST_RUNNER_TEMP" 2>/dev/null || true
}
trap cleanup_test EXIT

mkdir -p "$WORK_DIR"
chmod 700 "$WORK_DIR"

readonly FIXTURE_SPEC="${WORK_DIR}/fixture-standard.json"
readonly ARMED_SPEC="${WORK_DIR}/fixture-armed.json"
readonly ARMED_OLD_NONCE_SPEC="${WORK_DIR}/fixture-armed-old-nonce.json"
readonly ARMED_NEW_NONCE_SPEC="${WORK_DIR}/fixture-armed-new-nonce.json"
readonly STANDARD_OLD_NONCE_SPEC="${WORK_DIR}/fixture-standard-old-nonce.json"
readonly STANDARD_NEW_NONCE_SPEC="${WORK_DIR}/fixture-standard-new-nonce.json"
readonly WRONG_TARGET_ENV_SPEC="${WORK_DIR}/fixture-wrong-target-env.json"
readonly MISSING_TARGET_ENV_SPEC="${WORK_DIR}/fixture-missing-target-env.json"
readonly MALFORMED_JSON_LEFT="${WORK_DIR}/malformed-left.json"
readonly MALFORMED_JSON_RIGHT="${WORK_DIR}/malformed-right.json"
readonly RETRY_SPEC="${WORK_DIR}/fixture-retry.json"
readonly NORMALIZED_SPEC="${WORK_DIR}/fixture-normalized.json"
readonly NORMALIZED_RETRY_SPEC="${WORK_DIR}/fixture-normalized-retry.json"
readonly TEST_STDOUT="${WORK_DIR}/test.stdout"
readonly TEST_STDERR="${WORK_DIR}/test.stderr"
readonly MOCK_STATE_DIR="${WORK_DIR}/mock-state"
readonly MOCK_DESIRED_STATE="${MOCK_STATE_DIR}/desired.json"
readonly MOCK_ACTIVE_STATE="${MOCK_STATE_DIR}/active.json"
readonly MOCK_UPDATE_COUNT_FILE="${MOCK_STATE_DIR}/update-count"
readonly MOCK_WAIT_COUNT_FILE="${MOCK_STATE_DIR}/wait-count"
readonly MOCK_READINESS_MARKER="${MOCK_STATE_DIR}/readiness-checked"

MOCK_LOG_CONTENT=""
MOCK_CASE_NAME="uninitialized"
MOCK_FAIL_WAIT_NUMBER=""
MOCK_CAPTURE_MODE=""
MOCK_CAPTURE_UPDATE_SEEN="0"
MOCK_CAPTURE_DESIRED_DRIFT="0"
MOCK_CAPTURE_WRONG_APP_ID="0"
MOCK_CAPTURE_DEPLOYMENT_DRIFT="0"
readonly MOCK_CAPTURE_PRIOR_ID="00000000-0000-4000-8000-000000000001"
readonly MOCK_CAPTURE_DEPLOYMENT_ID="00000000-0000-4000-8000-000000000002"
readonly MOCK_CAPTURE_SECOND_ID="00000000-0000-4000-8000-000000000003"
readonly MOCK_CAPTURE_STAGED_STATE="${MOCK_STATE_DIR}/capture-staged.json"
readonly MOCK_CAPTURE_OBSERVED_STATE="${MOCK_STATE_DIR}/capture-observed.json"
readonly MOCK_CAPTURE_DEPLOYMENT_STATE="${MOCK_STATE_DIR}/capture-deployment.json"
readonly MOCK_CAPTURE_UPDATE_COUNT_FILE="${MOCK_STATE_DIR}/capture-update-count"

mock_capture_app_state() {
	local state="$1" app_id="$APP_ID" active_id="$MOCK_CAPTURE_PRIOR_ID" pending_id="" in_progress_id=""
	if [[ "$MOCK_CAPTURE_UPDATE_SEEN" == "1" && "$MOCK_CAPTURE_WRONG_APP_ID" == "1" ]]; then
		app_id="00000000-0000-4000-8000-000000000099"
	fi
	case "$state" in
	prior) ;;
	pending) pending_id="$MOCK_CAPTURE_DEPLOYMENT_ID" ;;
	in-progress) in_progress_id="$MOCK_CAPTURE_DEPLOYMENT_ID" ;;
	fast-active) active_id="$MOCK_CAPTURE_DEPLOYMENT_ID" ;;
	multiple)
		pending_id="$MOCK_CAPTURE_DEPLOYMENT_ID"
		in_progress_id="$MOCK_CAPTURE_SECOND_ID"
		;;
	*) test_fail "unknown mock capture state" ;;
	esac
	jq -n \
		--arg app "$app_id" \
		--arg active "$active_id" \
		--arg pending "$pending_id" \
		--arg in_progress "$in_progress_id" '[{
      id: $app,
      active_deployment: {id: $active},
      pending_deployment: (if $pending == "" then null else {id: $pending} end),
      in_progress_deployment: (if $in_progress == "" then null else {id: $in_progress} end)
    }]'
}

# The mock has an explicit allow-list. Any new provider operation makes this
# test fail instead of accidentally reaching a real control plane.
doctl() {
	if [[ -n "$MOCK_CAPTURE_MODE" && "${1:-}" == "apps" && "${2:-}" == "get" ]]; then
		if [[ "$MOCK_CAPTURE_UPDATE_SEEN" == "0" ]]; then
			mock_capture_app_state prior
		elif [[ "$MOCK_CAPTURE_MODE" == "delayed-pending" ]]; then
			mock_capture_app_state pending
		else
			mock_capture_app_state "$MOCK_CAPTURE_MODE"
		fi
		return 0
	fi
	if [[ -n "$MOCK_CAPTURE_MODE" && "${1:-}" == "apps" && "${2:-}" == "update" ]]; then
		local index spec_path="" update_count no_source_refresh="0" json_output="0"
		for ((index = 1; index <= $#; index++)); do
			if [[ "${!index}" == "--update-sources=false" ]]; then
				no_source_refresh="1"
			elif [[ "${!index}" == "--output" ]]; then
				index=$((index + 1))
				[[ "${!index}" == "json" ]] && json_output="1"
			elif [[ "${!index}" == "--spec" ]]; then
				index=$((index + 1))
				spec_path="${!index}"
			fi
		done
		[[ -n "$spec_path" ]] || test_fail "mock app update omitted its staged spec"
		[[ "$no_source_refresh" == "1" ]] || test_fail "mock app update permitted a source refresh"
		[[ "$json_output" == "1" ]] || test_fail "mock app update did not request its complete JSON state"
		cp "$spec_path" "$MOCK_CAPTURE_STAGED_STATE"
		jq --arg job "$MIGRATION_JOB" --arg target_key "$TARGET_ENV_KEY" '
        .jobs |= map(
          if .name == $job then
            .envs |= map(if .key == $target_key then .value = "EV[mock-protected-target]" else . end)
          else . end
        )
      ' "$spec_path" >"$MOCK_CAPTURE_OBSERVED_STATE"
		if [[ "$MOCK_CAPTURE_DESIRED_DRIFT" == "1" ]]; then
			jq '.name = "concurrent-drift"' "$MOCK_CAPTURE_OBSERVED_STATE" >"$MOCK_DESIRED_STATE"
		else
			cp "$MOCK_CAPTURE_OBSERVED_STATE" "$MOCK_DESIRED_STATE"
		fi
		update_count="$(command cat "$MOCK_CAPTURE_UPDATE_COUNT_FILE")"
		printf '%s\n' "$((update_count + 1))" >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
		MOCK_CAPTURE_UPDATE_SEEN="1"
		if [[ "$MOCK_CAPTURE_MODE" == "delayed-pending" ]]; then
			mock_capture_app_state prior
		else
			mock_capture_app_state "$MOCK_CAPTURE_MODE"
		fi
		return 0
	fi
	if [[ -n "$MOCK_CAPTURE_MODE" && "${1:-}" == "apps" && "${2:-}" == "get-deployment" ]]; then
		[[ -f "$MOCK_CAPTURE_OBSERVED_STATE" ]] || test_fail "mock deployment was queried before app update"
		if [[ "$MOCK_CAPTURE_DEPLOYMENT_DRIFT" == "1" ]]; then
			jq --arg job "$MIGRATION_JOB" --arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" '
          .jobs |= map(
            if .name == $job then
              .envs |= map(if .key == $nonce_key then .value = "9999999" else . end)
            else . end
          )
        ' "$MOCK_CAPTURE_OBSERVED_STATE" >"$MOCK_CAPTURE_DEPLOYMENT_STATE"
		else
			cp "$MOCK_CAPTURE_OBSERVED_STATE" "$MOCK_CAPTURE_DEPLOYMENT_STATE"
		fi
		jq -n --arg id "$MOCK_CAPTURE_DEPLOYMENT_ID" --slurpfile spec "$MOCK_CAPTURE_DEPLOYMENT_STATE" \
			'[{id: $id, spec: $spec[0]}]'
		return 0
	fi
	if [[ "${1:-}" == "apps" && "${2:-}" == "spec" && "${3:-}" == "validate" ]]; then
		jq -e 'type == "object"' "${4:?}" >/dev/null
		return 0
	fi
	if [[ "${1:-}" == "apps" && "${2:-}" == "logs" ]]; then
		printf '%s\n' "$MOCK_LOG_CONTENT"
		return 0
	fi
	if [[ "${1:-}" == "apps" && "${2:-}" == "spec" && "${3:-}" == "get" ]]; then
		[[ -f "$MOCK_DESIRED_STATE" ]] || test_fail "mock desired spec was not initialized"
		command cat "$MOCK_DESIRED_STATE"
		return 0
	fi
	test_fail "an unmocked DigitalOcean operation was attempted"
}

gh() {
	test_fail "a GitHub network operation was attempted"
}

curl() {
	test_fail "a public network operation was attempted"
}

cat >"$FIXTURE_SPEC" <<'JSON'
{
  "name": "rereply-test-fixture",
  "envs": [],
  "services": [
    {
      "name": "omnitech-web",
      "git": {
        "repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git",
        "branch": "main",
        "deploy_on_push": false
      },
      "dockerfile_path": "docker/Dockerfile",
      "envs": [
        {"key": "WHATOMATE_APP__ENVIRONMENT", "value": "production", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_DATABASE__RLS_ENABLED", "value": "true", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_DATABASE__RUNTIME_ROLE", "value": "rereply_app", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_REGISTRY__ENABLED", "value": "true", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_REGISTRY__QUEUE_READER_VERSION", "value": "2", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "value": "true", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_MESSENGER_ONBOARDING__ALLOW_ALL_ORGANIZATIONS", "value": "false", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_MESSENGER_ONBOARDING__ALLOWED_ORGANIZATION_IDS", "value": "22222222-2222-4222-8222-222222222222", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "value": "EV[test-messenger-app-secret]", "scope": "RUN_TIME", "type": "SECRET"}
      ]
    },
    {
      "name": "meta-relay",
      "git": {
        "repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git",
        "branch": "main",
        "deploy_on_push": false
      },
      "dockerfile_path": "docker/meta-relay.Dockerfile",
      "envs": [
        {"key": "META_RELAY_REGISTRY_ENABLED", "value": "true", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "META_RELAY_DYNAMIC_QUEUE_READER_VERSION", "value": "2", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "META_RELAY_REGISTRY_URL", "value": "https://app.rereply.app/internal/meta-registry/v1/resolve", "scope": "RUN_TIME", "type": "GENERAL"},
        {"key": "META_RELAY_REGISTRY_SECRET", "value": "EV[test-registry-secret]", "scope": "RUN_TIME", "type": "SECRET"},
        {"key": "META_RELAY_REGISTRY_EDGE_SECRET", "value": "EV[test-edge-secret]", "scope": "RUN_TIME", "type": "SECRET"},
        {"key": "META_RELAY_ACCOUNTS_JSON", "value": "[]", "scope": "RUN_TIME", "type": "GENERAL"}
      ]
    },
    {
      "name": "gmail-relay",
      "git": {
        "repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git",
        "branch": "main",
        "deploy_on_push": false
      },
      "dockerfile_path": "docker/gmail-relay.Dockerfile",
      "envs": []
    }
  ],
  "jobs": [
    {
      "name": "rereply-rls-migrate",
      "kind": "PRE_DEPLOY",
      "git": {
        "repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git",
        "branch": "main",
        "deploy_on_push": false
      },
      "dockerfile_path": "docker/Dockerfile",
      "run_command": "./rereply rls-migrate -config config.toml",
      "envs": [
        {"key": "WHATOMATE_DATABASE__MIGRATION_URL", "value": "EV[test-migration-url]", "scope": "RUN_TIME", "type": "SECRET"},
        {"key": "WHATOMATE_DATABASE__RUNTIME_ROLE", "value": "rereply_app", "scope": "RUN_TIME", "type": "GENERAL"}
      ]
    }
  ]
}
JSON

verify_standard_spec "$FIXTURE_SPEC"

printf '{"broken":' >"$MALFORMED_JSON_LEFT"
printf '["also-broken"' >"$MALFORMED_JSON_RIGHT"
if same_json "$MALFORMED_JSON_LEFT" "$MALFORMED_JSON_RIGHT" >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "two malformed JSON documents compared equal"
fi
grep -Fq 'failed to canonicalize a protected app spec' "$TEST_STDERR" ||
	test_fail "malformed JSON comparison did not fail closed"

readonly DRY_COMMAND_TEXT="$(dry_command)"
if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" <<<"$DRY_COMMAND_TEXT"; then
	test_fail "the temporary command embedded the protected identifier"
fi

build_staged_spec "$FIXTURE_SPEC" "$DRY_COMMAND_TEXT" "$STAGED_SPEC"
jq -e \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" \
	--arg target "$BOOTSTRAP_ORGANIZATION_ID" \
	--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" '
    (.jobs[] | select(.name == $job)) as $migration
    | $migration.run_command != "./rereply rls-migrate -config config.toml"
      and ([
        $migration.envs[]
        | select(
            .key == $target_key
            and .value == $target
            and .type == "SECRET"
            and .scope == "RUN_TIME"
          )
      ] | length) == 1
      and ([ $migration.envs[] | select(.key == $nonce_key) ] | length) == 0
  ' "$STAGED_SPEC" >/dev/null || test_fail "fresh staged spec was not exact"

normalize_bootstrap_spec "$STAGED_SPEC" "$NORMALIZED_SPEC"
jq -e \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    [
      .jobs[]
      | select(.name == $job)
      | .envs[]
      | select(.key == $target_key and .value == "<protected-target>")
    ] | length == 1
  ' "$NORMALIZED_SPEC" >/dev/null || test_fail "protected identifier was not normalized"
if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$NORMALIZED_SPEC"; then
	test_fail "normalized comparison retained the protected identifier"
fi

jq \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    (.jobs[] | select(.name == $job).envs[] | select(.key == $target_key).value) = "EV[test-protected-target]"
  ' "$STAGED_SPEC" >"$ARMED_SPEC"
verify_armed_spec "$ARMED_SPEC" "$DRY_COMMAND_TEXT"
same_json "$BASELINE_SPEC" "$FIXTURE_SPEC" || test_fail "armed spec did not reconstruct the exact baseline"

build_staged_spec "$FIXTURE_SPEC" "$DRY_COMMAND_TEXT" "$RETRY_SPEC" "$GITHUB_RUN_ID"
jq -e \
	--arg job "$MIGRATION_JOB" \
	--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
	--arg nonce "$GITHUB_RUN_ID" '
    [
      .jobs[]
      | select(.name == $job)
      | .envs[]
      | select(
          .key == $nonce_key
          and .value == $nonce
          and .scope == "RUN_TIME"
          and .type == "GENERAL"
        )
    ] | length == 1
  ' "$RETRY_SPEC" >/dev/null || test_fail "retry deployment nonce was not exact"
normalize_bootstrap_spec "$RETRY_SPEC" "$NORMALIZED_RETRY_SPEC"
same_json "$NORMALIZED_RETRY_SPEC" "$NORMALIZED_SPEC" || test_fail "retry nonce changed the protected operation"

readonly OLD_ARMED_NONCE="8000001"
readonly OLD_STANDARD_NONCE="8000002"
build_staged_spec "$FIXTURE_SPEC" "$DRY_COMMAND_TEXT" "$ARMED_OLD_NONCE_SPEC" "$OLD_ARMED_NONCE"
jq \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    (.jobs[] | select(.name == $job).envs[] | select(.key == $target_key).value) = "EV[test-protected-target-old-nonce]"
  ' "$ARMED_OLD_NONCE_SPEC" >"${ARMED_OLD_NONCE_SPEC}.encrypted"
mv "${ARMED_OLD_NONCE_SPEC}.encrypted" "$ARMED_OLD_NONCE_SPEC"
verify_armed_spec "$ARMED_OLD_NONCE_SPEC" "$DRY_COMMAND_TEXT"

jq \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    (.jobs[] | select(.name == $job).envs[] | select(.key == $target_key).value) = "EV[test-protected-target-new-nonce]"
  ' "$RETRY_SPEC" >"$ARMED_NEW_NONCE_SPEC"
verify_armed_spec "$ARMED_NEW_NONCE_SPEC" "$DRY_COMMAND_TEXT"

jq \
	--arg job "$MIGRATION_JOB" \
	--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
	--arg nonce "$OLD_STANDARD_NONCE" '
    .jobs |= map(
      if .name == $job then
        .envs += [{
          key: $nonce_key,
          value: $nonce,
          scope: "RUN_TIME",
          type: "GENERAL"
        }]
      else . end
    )
  ' "$FIXTURE_SPEC" >"$STANDARD_OLD_NONCE_SPEC"
verify_standard_redeployment_spec "$STANDARD_OLD_NONCE_SPEC"
same_json "$BASELINE_SPEC" "$FIXTURE_SPEC" || test_fail "standard nonce spec did not reconstruct the baseline"

build_standard_redeployment_spec "$FIXTURE_SPEC" "$STANDARD_NEW_NONCE_SPEC"
verify_standard_redeployment_spec "$STANDARD_NEW_NONCE_SPEC"
same_json "$BASELINE_SPEC" "$FIXTURE_SPEC" || test_fail "current standard nonce spec did not reconstruct the baseline"

# DigitalOcean returns protected values as EV[...] references. Reject an
# encrypted-looking target field if its secret metadata is wrong, and reject
# a recoverable spec that omits the protected target field entirely.
jq \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    (.jobs[] | select(.name == $job).envs[] | select(.key == $target_key).type) = "GENERAL"
  ' "$ARMED_OLD_NONCE_SPEC" >"$WRONG_TARGET_ENV_SPEC"
jq \
	--arg job "$MIGRATION_JOB" \
	--arg target_key "$TARGET_ENV_KEY" '
    .jobs |= map(
      if .name == $job then
        .envs |= map(select(.key != $target_key))
      else . end
    )
  ' "$ARMED_OLD_NONCE_SPEC" >"$MISSING_TARGET_ENV_SPEC"

expect_log_status() {
	local expected_status="$1" phase="$2" content="$3" status
	MOCK_LOG_CONTENT="$content"
	rm -f "$DEFINITE_NO_COMMIT_MARKER"
	if inspect_bootstrap_log "00000000-0000-4000-8000-000000000001" "$phase" >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
		status=0
	else
		status=$?
	fi
	[[ "$status" == "$expected_status" ]] ||
		test_fail "log classifier returned ${status}, expected ${expected_status} for ${phase}"
}

expect_log_status 0 dry-run "$DRY_FRESH"
grep -Fxq "$DRY_FRESH" "$TEST_STDOUT" || test_fail "dry-run proof was not emitted"

expect_log_status 0 apply "${APPLY_FRESH}"$'\n'"${DRY_COMMITTED}"
grep -Fxq "$APPLY_FRESH" "$TEST_STDOUT" || test_fail "fresh apply proof was not emitted"
grep -Fxq "$DRY_COMMITTED" "$TEST_STDOUT" || test_fail "post-apply proof was not emitted"

expect_log_status 0 apply "${APPLY_COMMITTED}"$'\n'"${DRY_COMMITTED}"
grep -Fxq "$APPLY_COMMITTED" "$TEST_STDOUT" || test_fail "idempotent apply proof was not emitted"

expect_log_status 1 apply "Platform compliance bootstrap failed: test failure; the transaction was rolled back and no changes were committed."
[[ -f "$DEFINITE_NO_COMMIT_MARKER" ]] || test_fail "definite rollback was not marked safe to restore"

expect_log_status 1 apply "Failed to connect using database.migration_url; no database changes were made."
[[ -f "$DEFINITE_NO_COMMIT_MARKER" ]] || test_fail "pre-database failure was not marked safe to restore"

expect_log_status 42 apply "Unexpected subsystem failure; no database changes were made."
[[ ! -f "$DEFINITE_NO_COMMIT_MARKER" ]] || test_fail "near-miss rollback suffix was marked safe to restore"

expect_log_status 42 apply "Platform compliance bootstrap apply outcome is indeterminate: test fixture"
[[ ! -f "$DEFINITE_NO_COMMIT_MARKER" ]] || test_fail "indeterminate apply was marked safe to restore"

expect_log_status 42 apply "$APPLY_FRESH"
[[ ! -f "$DEFINITE_NO_COMMIT_MARKER" ]] || test_fail "apply without reconciliation was marked safe to restore"

readonly SENSITIVE_SENTINEL="test-sensitive-value-must-not-escape"
expect_log_status 42 apply "password=${SENSITIVE_SENTINEL}"
if grep -Fq "$SENSITIVE_SENTINEL" "$TEST_STDOUT" || grep -Fq "$SENSITIVE_SENTINEL" "$TEST_STDERR"; then
	test_fail "sensitive deployment output escaped the classifier"
fi

expect_log_status 42 apply "diagnostic target=${BOOTSTRAP_ORGANIZATION_ID}"
if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDOUT" || grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDERR"; then
	test_fail "raw protected target escaped the classifier"
fi

mkdir -p "$MOCK_STATE_DIR"

run_capture_update_case() {
	local mode="$1" staged_path="${2:-$ARMED_SPEC}" captured
	MOCK_CAPTURE_MODE="$mode"
	MOCK_CAPTURE_UPDATE_SEEN="0"
	MOCK_CAPTURE_DESIRED_DRIFT="0"
	MOCK_CAPTURE_WRONG_APP_ID="0"
	MOCK_CAPTURE_DEPLOYMENT_DRIFT="0"
	cp "$FIXTURE_SPEC" "$MOCK_DESIRED_STATE"
	printf '0\n' >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
	rm -f "$MOCK_CAPTURE_STAGED_STATE" "$MOCK_CAPTURE_OBSERVED_STATE"
	captured="$(capture_update_deployment "$FIXTURE_SPEC" "$staged_path")"
	[[ "$captured" == "$MOCK_CAPTURE_DEPLOYMENT_ID" ]] ||
		test_fail "${mode} app update did not return the accepted deployment"
	[[ "$(command cat "$MOCK_CAPTURE_UPDATE_COUNT_FILE")" == "1" ]] ||
		test_fail "${mode} app update did not execute exactly once"
	same_json "$MOCK_CAPTURE_STAGED_STATE" "$staged_path" ||
		test_fail "${mode} app update changed the staged spec"
}

# doctl 1.167 can return an empty InProgressDeployment.ID column even though
# App Platform accepted the update under pending_deployment. The production
# capture path must also recognize in-progress, fast-active, and a response
# that exposes the accepted deployment only on the first follow-up poll.
run_capture_update_case pending
run_capture_update_case in-progress
run_capture_update_case fast-active
run_capture_update_case delayed-pending
run_capture_update_case pending "$STANDARD_NEW_NONCE_SPEC"
run_capture_update_case fast-active "$FIXTURE_SPEC"

MOCK_CAPTURE_MODE="multiple"
MOCK_CAPTURE_UPDATE_SEEN="0"
MOCK_CAPTURE_DESIRED_DRIFT="0"
cp "$FIXTURE_SPEC" "$MOCK_DESIRED_STATE"
printf '0\n' >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
if (
	trap - EXIT
	capture_update_deployment "$FIXTURE_SPEC" "$ARMED_SPEC"
) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "multiple provider deployment identifiers were accepted"
fi
grep -Fq 'multiple concurrent deployment identifiers' "$TEST_STDERR" ||
	test_fail "multiple provider deployment identifiers failed for an unexpected reason"

MOCK_CAPTURE_MODE="pending"
MOCK_CAPTURE_UPDATE_SEEN="0"
MOCK_CAPTURE_DESIRED_DRIFT="1"
MOCK_CAPTURE_WRONG_APP_ID="0"
MOCK_CAPTURE_DEPLOYMENT_DRIFT="0"
cp "$FIXTURE_SPEC" "$MOCK_DESIRED_STATE"
printf '0\n' >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
if (
	trap - EXIT
	capture_update_deployment "$FIXTURE_SPEC" "$ARMED_SPEC"
) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "concurrent desired-spec drift was accepted"
fi
grep -Fq 'staged spec differs from the reviewed update' "$TEST_STDERR" ||
	test_fail "concurrent desired-spec drift failed for an unexpected reason"

MOCK_CAPTURE_MODE="pending"
MOCK_CAPTURE_UPDATE_SEEN="0"
MOCK_CAPTURE_DESIRED_DRIFT="0"
MOCK_CAPTURE_WRONG_APP_ID="1"
MOCK_CAPTURE_DEPLOYMENT_DRIFT="0"
cp "$FIXTURE_SPEC" "$MOCK_DESIRED_STATE"
printf '0\n' >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
if (
	trap - EXIT
	capture_update_deployment "$FIXTURE_SPEC" "$ARMED_SPEC"
) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "wrong app identity in update response was accepted"
fi
grep -Fq 'app update response was ambiguous' "$TEST_STDERR" ||
	test_fail "wrong app identity failed for an unexpected reason"

MOCK_CAPTURE_MODE="pending"
MOCK_CAPTURE_UPDATE_SEEN="0"
MOCK_CAPTURE_DESIRED_DRIFT="0"
MOCK_CAPTURE_WRONG_APP_ID="0"
MOCK_CAPTURE_DEPLOYMENT_DRIFT="1"
cp "$FIXTURE_SPEC" "$MOCK_DESIRED_STATE"
printf '0\n' >"$MOCK_CAPTURE_UPDATE_COUNT_FILE"
if (
	trap - EXIT
	capture_update_deployment "$FIXTURE_SPEC" "$ARMED_OLD_NONCE_SPEC"
) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "deployment spec or nonce drift was accepted"
fi
grep -Fq 'staged spec differs from the reviewed update' "$TEST_STDERR" ||
	test_fail "deployment spec drift failed for an unexpected reason"

MOCK_CAPTURE_MODE=""
MOCK_CAPTURE_DESIRED_DRIFT="0"
MOCK_CAPTURE_WRONG_APP_ID="0"
MOCK_CAPTURE_DEPLOYMENT_DRIFT="0"

verify_basic_context() {
	:
}

verify_github_control_plane() {
	:
}

wait_for_existing_deployment() {
	:
}

verify_healthy_active() {
	cp "$MOCK_ACTIVE_STATE" "$ACTIVE_SPEC"
}

capture_update_deployment() {
	local current_path="$1" staged_path="$2" update_count
	same_json "$current_path" "$MOCK_DESIRED_STATE" || test_fail "mock update did not use the current desired spec"
	update_count="$(command cat "$MOCK_UPDATE_COUNT_FILE")"
	update_count=$((update_count + 1))
	cp "$staged_path" "${MOCK_STATE_DIR}/${MOCK_CASE_NAME}-update-${update_count}.json"
	cp "$staged_path" "$MOCK_DESIRED_STATE"
	printf '%s\n' "$update_count" >"$MOCK_UPDATE_COUNT_FILE"
	printf '00000000-0000-4000-8000-%012d' "$update_count"
}

wait_for_deployment() {
	local wait_count
	wait_count="$(command cat "$MOCK_WAIT_COUNT_FILE")"
	wait_count=$((wait_count + 1))
	printf '%s\n' "$wait_count" >"$MOCK_WAIT_COUNT_FILE"
	if [[ -n "$MOCK_FAIL_WAIT_NUMBER" && "$wait_count" == "$MOCK_FAIL_WAIT_NUMBER" ]]; then
		return 1
	fi
	cp "$MOCK_DESIRED_STATE" "$MOCK_ACTIVE_STATE"
	return 0
}

verify_public_readiness() {
	: >"$MOCK_READINESS_MARKER"
}

reset_mock_state() {
	local case_name="$1" desired_path="$2" active_path="$3"
	MOCK_CASE_NAME="$case_name"
	cp "$desired_path" "$MOCK_DESIRED_STATE"
	cp "$active_path" "$MOCK_ACTIVE_STATE"
	printf '0\n' >"$MOCK_UPDATE_COUNT_FILE"
	printf '0\n' >"$MOCK_WAIT_COUNT_FILE"
	MOCK_FAIL_WAIT_NUMBER=""
	rm -f "$MOCK_READINESS_MARKER"
}

run_execute_recovery() {
	local case_name="$1" desired_path="$2" active_path="$3" old_nonce="$4"
	local update_path normalized_update
	reset_mock_state "$case_name" "$desired_path" "$active_path"
	MOCK_LOG_CONTENT="$DRY_FRESH"
	if ! (
		trap - EXIT
		execute_operation
	) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
		test_fail "execute recovery ${case_name} failed"
	fi
	[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "1" ]] ||
		test_fail "execute recovery ${case_name} did not create exactly one deployment"
	update_path="${MOCK_STATE_DIR}/${case_name}-update-1.json"
	normalized_update="${MOCK_STATE_DIR}/${case_name}-normalized.json"
	jq -e \
		--arg job "$MIGRATION_JOB" \
		--arg command "$DRY_COMMAND_TEXT" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
		--arg nonce "$GITHUB_RUN_ID" \
		--arg old_nonce "$old_nonce" \
		--arg target_key "$TARGET_ENV_KEY" \
		--arg target "$BOOTSTRAP_ORGANIZATION_ID" '
      (.jobs[] | select(.name == $job)) as $migration
      | $migration.run_command == $command
        and ([
          $migration.envs[]
          | select(
              .key == $nonce_key
              and .value == $nonce
              and .scope == "RUN_TIME"
              and .type == "GENERAL"
            )
        ] | length) == 1
        and ([ $migration.envs[] | select(.key == $nonce_key and .value == $old_nonce) ] | length) == 0
        and ([
          $migration.envs[]
          | select(
              .key == $target_key
              and .value == $target
              and .scope == "RUN_TIME"
              and .type == "SECRET"
            )
        ] | length) == 1
    ' "$update_path" >/dev/null || test_fail "execute recovery ${case_name} did not rotate to the new exact nonce"
	normalize_bootstrap_spec "$update_path" "$normalized_update"
	same_json "$normalized_update" "$NORMALIZED_SPEC" ||
		test_fail "execute recovery ${case_name} changed more than the retry nonce"
	grep -Fxq "$DRY_FRESH" "$TEST_STDOUT" || test_fail "execute recovery ${case_name} lost its dry-run proof"
	if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDOUT" || grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDERR"; then
		test_fail "execute recovery ${case_name} exposed the protected identifier"
	fi
}

expect_execute_rejection() {
	local case_name="$1" desired_path="$2" active_path="$3"
	reset_mock_state "$case_name" "$desired_path" "$active_path"
	MOCK_LOG_CONTENT="$DRY_FRESH"
	if (
		trap - EXIT
		execute_operation
	) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
		test_fail "execute recovery ${case_name} accepted invalid protected target state"
	fi
	[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "0" ]] ||
		test_fail "execute recovery ${case_name} mutated provider state before rejection"
	grep -Fq 'temporary bootstrap credential or deployment nonce drifted' "$TEST_STDERR" ||
		test_fail "execute recovery ${case_name} failed for an unexpected reason"
	if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDOUT" || grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$TEST_STDERR"; then
		test_fail "execute recovery ${case_name} exposed the protected identifier"
	fi
}

run_execute_recovery "execute-armed-active-armed" \
	"$ARMED_OLD_NONCE_SPEC" "$ARMED_OLD_NONCE_SPEC" "$OLD_ARMED_NONCE"
run_execute_recovery "execute-armed-active-baseline" \
	"$ARMED_OLD_NONCE_SPEC" "$FIXTURE_SPEC" "$OLD_ARMED_NONCE"
run_execute_recovery "execute-armed-new-active-armed-old" \
	"$ARMED_NEW_NONCE_SPEC" "$ARMED_OLD_NONCE_SPEC" "$OLD_ARMED_NONCE"
run_execute_recovery "execute-armed-active-standard-old-nonce" \
	"$ARMED_NEW_NONCE_SPEC" "$STANDARD_OLD_NONCE_SPEC" "$OLD_STANDARD_NONCE"
run_execute_recovery "execute-standard-active-armed" \
	"$FIXTURE_SPEC" "$ARMED_OLD_NONCE_SPEC" "$OLD_ARMED_NONCE"
run_execute_recovery "execute-standard-active-standard-nonce" \
	"$FIXTURE_SPEC" "$STANDARD_OLD_NONCE_SPEC" "$OLD_STANDARD_NONCE"
run_execute_recovery "execute-standard-nonce-transient" \
	"$STANDARD_OLD_NONCE_SPEC" "$STANDARD_OLD_NONCE_SPEC" "$OLD_STANDARD_NONCE"
run_execute_recovery "execute-standard-new-nonce-active-standard-old-nonce" \
	"$STANDARD_NEW_NONCE_SPEC" "$STANDARD_OLD_NONCE_SPEC" "$OLD_STANDARD_NONCE"
expect_execute_rejection "execute-wrong-target-env" "$WRONG_TARGET_ENV_SPEC" "$WRONG_TARGET_ENV_SPEC"
expect_execute_rejection "execute-missing-target-env" "$MISSING_TARGET_ENV_SPEC" "$MISSING_TARGET_ENV_SPEC"

# Exercise the recovery state in cleanup_operation itself: DigitalOcean has
# accepted the exact standard desired spec, while the still-active deployment
# remains armed. The operator must deploy standard+nonce once, then the exact
# standard baseline, and become idempotent after recovery.
reset_mock_state "cleanup-standard-active-armed" "$FIXTURE_SPEC" "$ARMED_SPEC"

cleanup_operation >"$TEST_STDOUT" 2>"$TEST_STDERR"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "2" ]] ||
	test_fail "standard recovery did not perform exactly two controlled deployments"
jq -e \
	--arg job "$MIGRATION_JOB" \
	--arg standard "$STANDARD_MIGRATION_COMMAND" \
	--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
	--arg nonce "$GITHUB_RUN_ID" \
	--arg target_key "$TARGET_ENV_KEY" '
    (.jobs[] | select(.name == $job)) as $migration
    | $migration.run_command == $standard
      and ([ $migration.envs[] | select(.key == $nonce_key and .value == $nonce) ] | length) == 1
      and ([ $migration.envs[] | select(.key == $target_key) ] | length) == 0
  ' "${MOCK_STATE_DIR}/cleanup-standard-active-armed-update-1.json" >/dev/null ||
	test_fail "standard recovery nonce deployment was not exact"
same_json "${MOCK_STATE_DIR}/cleanup-standard-active-armed-update-2.json" "$FIXTURE_SPEC" ||
	test_fail "standard recovery did not finish with the exact baseline"
same_json "$MOCK_DESIRED_STATE" "$FIXTURE_SPEC" || test_fail "desired spec was not restored"
same_json "$MOCK_ACTIVE_STATE" "$FIXTURE_SPEC" || test_fail "active spec was not restored"
[[ -f "$MOCK_READINESS_MARKER" ]] || test_fail "readiness was not checked after cleanup recovery"

cleanup_operation >"$TEST_STDOUT" 2>"$TEST_STDERR"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "2" ]] ||
	test_fail "idempotent standard cleanup created another deployment"
grep -Fxq 'Migration job was already restored and production readiness verified.' "$TEST_STDOUT" ||
	test_fail "idempotent cleanup did not report the restored state"

# The same two-step cleanup must recover when the active deployment is the
# standard command carrying an older nonce rather than an armed command.
reset_mock_state "cleanup-baseline-active-standard-old-nonce" "$FIXTURE_SPEC" "$STANDARD_OLD_NONCE_SPEC"
cleanup_operation >"$TEST_STDOUT" 2>"$TEST_STDERR"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "2" ]] ||
	test_fail "baseline plus active old standard nonce did not use two controlled cleanup deployments"
same_json "${MOCK_STATE_DIR}/cleanup-baseline-active-standard-old-nonce-update-1.json" "$STANDARD_NEW_NONCE_SPEC" ||
	test_fail "cleanup did not rotate the transient standard nonce"
same_json "${MOCK_STATE_DIR}/cleanup-baseline-active-standard-old-nonce-update-2.json" "$FIXTURE_SPEC" ||
	test_fail "cleanup from the old standard nonce did not finish at baseline"
same_json "$MOCK_DESIRED_STATE" "$FIXTURE_SPEC" || test_fail "cleanup did not restore desired baseline"
same_json "$MOCK_ACTIVE_STATE" "$FIXTURE_SPEC" || test_fail "cleanup did not restore active baseline"

# A desired standard spec already carrying the current nonce can reconcile an
# active deployment carrying an older nonce directly back to the baseline.
reset_mock_state "cleanup-standard-new-active-standard-old" "$STANDARD_NEW_NONCE_SPEC" "$STANDARD_OLD_NONCE_SPEC"
cleanup_operation >"$TEST_STDOUT" 2>"$TEST_STDERR"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "1" ]] ||
	test_fail "cross-generation standard nonce cleanup did not use exactly one deployment"
same_json "${MOCK_STATE_DIR}/cleanup-standard-new-active-standard-old-update-1.json" "$FIXTURE_SPEC" ||
	test_fail "cross-generation standard nonce cleanup did not deploy the baseline"
same_json "$MOCK_DESIRED_STATE" "$FIXTURE_SPEC" || test_fail "cross-generation cleanup did not restore desired baseline"
same_json "$MOCK_ACTIVE_STATE" "$FIXTURE_SPEC" || test_fail "cross-generation cleanup did not restore active baseline"

# Fault injection at the second cleanup wait models an accepted baseline spec
# whose deployment fails or becomes unobservable while the prior standard+nonce
# deployment remains active. A fresh cleanup must recognize and recover that
# exact desired/active split without manual mutation.
reset_mock_state "cleanup-second-update-fault" "$FIXTURE_SPEC" "$ARMED_OLD_NONCE_SPEC"
MOCK_FAIL_WAIT_NUMBER="2"
if (
	trap - EXIT
	cleanup_operation
) >"$TEST_STDOUT" 2>"$TEST_STDERR"; then
	test_fail "fault after the second cleanup update was not surfaced"
fi
grep -Fq 'exact standard cleanup deployment failed' "$TEST_STDERR" ||
	test_fail "second cleanup update fault failed for an unexpected reason"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "2" ]] ||
	test_fail "second cleanup update fault did not occur at the intended boundary"
same_json "$MOCK_DESIRED_STATE" "$FIXTURE_SPEC" ||
	test_fail "accepted second cleanup update did not leave the desired baseline"
same_json "$MOCK_ACTIVE_STATE" "$STANDARD_NEW_NONCE_SPEC" ||
	test_fail "failed second cleanup deployment did not preserve the prior active standard nonce"
[[ ! -f "$MOCK_READINESS_MARKER" ]] || test_fail "readiness ran after a failed cleanup deployment"

MOCK_CASE_NAME="cleanup-second-update-retry"
printf '0\n' >"$MOCK_UPDATE_COUNT_FILE"
printf '0\n' >"$MOCK_WAIT_COUNT_FILE"
MOCK_FAIL_WAIT_NUMBER=""
cleanup_operation >"$TEST_STDOUT" 2>"$TEST_STDERR"
[[ "$(command cat "$MOCK_UPDATE_COUNT_FILE")" == "2" ]] ||
	test_fail "fresh cleanup did not recover the second-update fault state"
same_json "${MOCK_STATE_DIR}/cleanup-second-update-retry-update-1.json" "$STANDARD_NEW_NONCE_SPEC" ||
	test_fail "fault recovery did not re-establish the exact transient standard spec"
same_json "${MOCK_STATE_DIR}/cleanup-second-update-retry-update-2.json" "$FIXTURE_SPEC" ||
	test_fail "fault recovery did not finish at the exact baseline"
same_json "$MOCK_DESIRED_STATE" "$FIXTURE_SPEC" || test_fail "fault recovery did not restore desired baseline"
same_json "$MOCK_ACTIVE_STATE" "$FIXTURE_SPEC" || test_fail "fault recovery did not restore active baseline"
[[ -f "$MOCK_READINESS_MARKER" ]] || test_fail "fault recovery did not verify readiness"

printf 'bootstrap-platform-compliance-once state-machine tests: PASS\n'
