#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

readonly APP_ID="54bc0d92-73e9-4f8b-86cd-c3fd96e06beb"
readonly APP_SERVICE="omnitech-web"
readonly META_RELAY_SERVICE="meta-relay"
readonly GMAIL_RELAY_SERVICE="gmail-relay"
readonly MIGRATION_JOB="rereply-rls-migrate"
readonly REPOSITORY_URL="https://github.com/medtechcorps-netizen/whatomate.git"
readonly AUDITED_BASE_SHA="869679c1ac44a2d56cf94929b18644f4127809d2"
readonly AUTHORIZED_ACTOR="medtechcorps-netizen"
readonly AUTHORIZED_ACTOR_ID="286957464"
readonly OPERATOR_RUN_ID="CHANGE-20260822-MANAGED-IG-001"
readonly TARGET_SHA256="210929629d85625858330076b3777bfbac539d2ee2cc438e7524409684a565bd"
readonly TARGET_ENV_KEY="REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID"
readonly DEPLOYMENT_NONCE_ENV_KEY="REREPLY_COMPLIANCE_BOOTSTRAP_DEPLOYMENT_NONCE"
readonly STANDARD_MIGRATION_COMMAND="./rereply rls-migrate -config config.toml"
readonly DRY_FRESH="Platform compliance bootstrap DRY RUN (read only): requested=2 changes=1 unchanged=0 purpose_created=true purpose_unchanged=false audit_written=false"
readonly DRY_COMMITTED="Platform compliance bootstrap DRY RUN (read only): requested=2 changes=0 unchanged=1 purpose_created=false purpose_unchanged=true audit_written=false"
readonly APPLY_FRESH="Platform compliance bootstrap APPLY COMPLETE: requested=2 changes=1 unchanged=0 purpose_created=true purpose_unchanged=false audit_written=true"
readonly APPLY_COMMITTED="Platform compliance bootstrap APPLY COMPLETE: requested=2 changes=0 unchanged=1 purpose_created=false purpose_unchanged=true audit_written=false"
readonly WORK_DIR="${RUNNER_TEMP:?}/platform-compliance-bootstrap-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
readonly DESIRED_SPEC="${WORK_DIR}/desired.json"
readonly ACTIVE_DEPLOYMENT="${WORK_DIR}/active-deployment.json"
readonly ACTIVE_SPEC="${WORK_DIR}/active-spec.json"
readonly BASELINE_SPEC="${WORK_DIR}/baseline.json"
readonly STAGED_SPEC="${WORK_DIR}/staged.json"
readonly REFETCHED_SPEC="${WORK_DIR}/refetched.json"
readonly APP_STATE="${WORK_DIR}/app.json"
readonly DEPLOYMENT_STATE="${WORK_DIR}/deployment.json"
readonly DEPLOYMENT_LOG="${WORK_DIR}/deployment.log"
readonly DEFINITE_NO_COMMIT_MARKER="${WORK_DIR}/definite-no-commit"
readonly RECOVERY_BASELINE="${WORK_DIR}/recovery-baseline.json"

die() {
	printf 'Platform compliance operator failed: %s\n' "$1" >&2
	exit 1
}

scrub_work_dir() {
	if [[ -d "$WORK_DIR" ]]; then
		find "$WORK_DIR" -type f -exec shred -u {} + 2>/dev/null || true
		rmdir "$WORK_DIR" 2>/dev/null || true
	fi
}

trap scrub_work_dir EXIT

require_tool() {
	command -v "$1" >/dev/null 2>&1 || die "required tool is unavailable"
}

is_uuid() {
	[[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]
}

canonical_hash() {
	local canonical
	canonical="$(jq -S -c . "$1")" || die "failed to canonicalize a protected app spec"
	printf '%s' "$canonical" | sha256sum | awk '{print $1}'
}

same_json() {
	local left_hash right_hash
	left_hash="$(canonical_hash "$1")" || return 1
	right_hash="$(canonical_hash "$2")" || return 1
	[[ "$left_hash" == "$right_hash" ]]
}

validate_target() {
	is_uuid "${BOOTSTRAP_ORGANIZATION_ID:-}" || die "protected organization identifier is not canonical"
	[[ "$(printf '%s' "$BOOTSTRAP_ORGANIZATION_ID" | sha256sum | awk '{print $1}')" == "$TARGET_SHA256" ]] ||
		die "protected organization identifier does not match the reviewed operation"
}

dry_command() {
	cat <<'COMMAND'
/bin/sh -ceu ': "${REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID:?}"; reviewed_hash="210929629d85625858330076b3777bfbac539d2ee2cc438e7524409684a565bd"; target_hash="$(printf "%s" "$REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID" | sha256sum)"; target_hash="${target_hash%% *}"; [ "$target_hash" = "$reviewed_hash" ] || { echo "Protected organization identifier does not match the reviewed operation." >&2; exit 1; }; expected="Platform compliance bootstrap DRY RUN (read only): requested=2 changes=1 unchanged=0 purpose_created=true purpose_unchanged=false audit_written=false"; actual="$(./rereply bootstrap-platform-compliance -config config.toml -organization-id "$REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID" -operator-run-id CHANGE-20260822-MANAGED-IG-001 -create-purpose -feature instagram)"; [ "$actual" = "$expected" ] || { echo "Unexpected dry-run result; no apply was attempted." >&2; exit 1; }; printf "%s\n" "$actual"'
COMMAND
}

apply_command() {
	cat <<'COMMAND'
/bin/sh -ceu ': "${REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID:?}"; reviewed_hash="210929629d85625858330076b3777bfbac539d2ee2cc438e7524409684a565bd"; target_hash="$(printf "%s" "$REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID" | sha256sum)"; target_hash="${target_hash%% *}"; [ "$target_hash" = "$reviewed_hash" ] || { echo "Protected organization identifier does not match the reviewed operation." >&2; exit 1; }; apply_fresh="Platform compliance bootstrap APPLY COMPLETE: requested=2 changes=1 unchanged=0 purpose_created=true purpose_unchanged=false audit_written=true"; apply_committed="Platform compliance bootstrap APPLY COMPLETE: requested=2 changes=0 unchanged=1 purpose_created=false purpose_unchanged=true audit_written=false"; dry_committed="Platform compliance bootstrap DRY RUN (read only): requested=2 changes=0 unchanged=1 purpose_created=false purpose_unchanged=true audit_written=false"; applied="$(./rereply bootstrap-platform-compliance -config config.toml -organization-id "$REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID" -operator-run-id CHANGE-20260822-MANAGED-IG-001 -create-purpose -feature instagram -apply)"; case "$applied" in "$apply_fresh"|"$apply_committed") ;; *) echo "Apply completed with an unexpected sanitized result; rerun the identical apply operation before any other action." >&2; exit 1 ;; esac; printf "%s\n" "$applied"; post="$(./rereply bootstrap-platform-compliance -config config.toml -organization-id "$REREPLY_COMPLIANCE_BOOTSTRAP_ORGANIZATION_ID" -operator-run-id CHANGE-20260822-MANAGED-IG-001 -create-purpose -feature instagram)"; [ "$post" = "$dry_committed" ] || { echo "Post-apply reconciliation failed; rerun the identical apply operation before any other action." >&2; exit 1; }; printf "%s\n" "$post"'
COMMAND
}

phase_command() {
	case "${BOOTSTRAP_PHASE:?}" in
	dry-run) dry_command ;;
	apply) apply_command ;;
	*) die "phase is invalid" ;;
	esac
}

expected_confirmation() {
	case "${BOOTSTRAP_PHASE:?}" in
	dry-run) printf 'DRY-RUN %s' "$OPERATOR_RUN_ID" ;;
	apply) printf 'APPLY %s' "$OPERATOR_RUN_ID" ;;
	*) die "phase is invalid" ;;
	esac
}

verify_basic_context() {
	[[ "${GITHUB_EVENT_NAME:-}" == "workflow_dispatch" ]] || die "event is not workflow_dispatch"
	[[ "${GITHUB_REF:-}" == "refs/heads/main" ]] || die "workflow must run from main"
	[[ "${GITHUB_ACTOR:-}" == "$AUTHORIZED_ACTOR" ]] || die "actor is not authorized"
	[[ "${GITHUB_ACTOR_ID:-}" == "$AUTHORIZED_ACTOR_ID" ]] || die "actor identity is not authorized"
	[[ "${GITHUB_RUN_ATTEMPT:-}" == "1" ]] || die "workflow reruns are forbidden; dispatch a new exact operation"
	[[ "${BOOTSTRAP_CONFIRMATION:-}" == "$(expected_confirmation)" ]] || die "confirmation does not match the reviewed operation"
	validate_target
}

verify_github_control_plane() {
	verify_basic_context

	local main_sha compare_file checks_file conclusion prior_runs
	main_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" --jq '.object.sha')"
	[[ "$main_sha" == "${GITHUB_SHA:?}" ]] || die "main moved after dispatch"

	compare_file="${WORK_DIR}/compare.json"
	gh api "repos/${GITHUB_REPOSITORY}/compare/${AUDITED_BASE_SHA}...${GITHUB_SHA}" >"$compare_file"
	jq -e '
    ([.files[].filename] | sort) == [
      ".github/scripts/bootstrap-platform-compliance-once-test.sh",
      ".github/scripts/bootstrap-platform-compliance-once.sh",
      ".github/workflows/bootstrap-platform-compliance-once.yml",
      ".github/workflows/deploy-production.yml"
    ]
  ' "$compare_file" >/dev/null || die "main contains changes outside the reviewed one-shot operator"

	checks_file="${WORK_DIR}/checks.json"
	gh api -H 'Accept: application/vnd.github+json' \
		"repos/${GITHUB_REPOSITORY}/commits/${GITHUB_SHA}/check-runs?per_page=100" >"$checks_file"
	for check_name in test lint build security e2e tenant-isolation; do
		conclusion="$(jq -r --arg name "$check_name" \
			'[.check_runs[] | select(.name == $name)] | sort_by(.started_at // .created_at) | last | .conclusion // empty' \
			"$checks_file")"
		[[ "$conclusion" == "success" ]] || die "required protected-main check is not successful"
	done

	if [[ "$BOOTSTRAP_PHASE" == "apply" ]]; then
		prior_runs="${WORK_DIR}/prior-runs.json"
		gh api \
			"repos/${GITHUB_REPOSITORY}/actions/workflows/bootstrap-platform-compliance-once.yml/runs?branch=main&event=workflow_dispatch&status=success&per_page=100" \
			>"$prior_runs"
		jq -e \
			--arg sha "$GITHUB_SHA" \
			--arg actor "$AUTHORIZED_ACTOR" \
			--arg title "Platform compliance dry-run ${OPERATOR_RUN_ID}" \
			--argjson current_run "$GITHUB_RUN_ID" '
        any(
          .workflow_runs[];
          .head_sha == $sha
          and .display_title == $title
          and .actor.login == $actor
          and .event == "workflow_dispatch"
          and .conclusion == "success"
          and .run_attempt == 1
          and .id < $current_run
        )
      ' "$prior_runs" >/dev/null || die "no successful exact dry-run exists on this main revision"
	fi
}

fetch_app_state() {
	local active_id
	doctl apps get "$APP_ID" --output json >"$APP_STATE"
	jq -e 'length == 1' "$APP_STATE" >/dev/null || die "DigitalOcean app lookup was ambiguous"
	active_id="$(jq -r '.[0].active_deployment.id // empty' "$APP_STATE")"
	is_uuid "$active_id" || die "active deployment identifier is invalid"
	doctl apps get-deployment "$APP_ID" "$active_id" --output json >"$ACTIVE_DEPLOYMENT"
	jq -e 'length == 1' "$ACTIVE_DEPLOYMENT" >/dev/null || die "active deployment lookup was ambiguous"
	jq '.[0].spec' "$ACTIVE_DEPLOYMENT" >"$ACTIVE_SPEC"
}

verify_healthy_active() {
	fetch_app_state
	jq -e \
		--arg sha "$GITHUB_SHA" \
		--arg service "$APP_SERVICE" \
		--arg relay "$META_RELAY_SERVICE" \
		--arg gmail "$GMAIL_RELAY_SERVICE" \
		--arg job "$MIGRATION_JOB" '
      .[0] as $app
      | ($app.in_progress_deployment == null)
        and ($app.pending_deployment == null)
        and ($app.active_deployment.phase == "ACTIVE")
        and ($app.active_deployment.progress.success_steps == $app.active_deployment.progress.total_steps)
        and ((($app.active_deployment.workers // []) | length) == 0)
        and ((($app.active_deployment.static_sites // []) | length) == 0)
        and ((($app.active_deployment.functions // []) | length) == 0)
        and (($app.active_deployment.services | map(.name) | sort) == ([$service, $relay, $gmail] | sort))
        and (($app.active_deployment.jobs | map(.name)) == [$job])
        and all($app.active_deployment.services[]; .source_commit_hash == $sha)
        and all($app.active_deployment.jobs[]; .source_commit_hash == $sha)
    ' "$APP_STATE" >/dev/null || die "active deployment is not the exact healthy reviewed revision"
}

verify_standard_spec() {
	local spec_path="$1"
	jq -e \
		--arg repository "$REPOSITORY_URL" \
		--arg service "$APP_SERVICE" \
		--arg relay "$META_RELAY_SERVICE" \
		--arg gmail "$GMAIL_RELAY_SERVICE" \
		--arg job "$MIGRATION_JOB" \
		--arg standard "$STANDARD_MIGRATION_COMMAND" \
		--arg target "$BOOTSTRAP_ORGANIZATION_ID" '
      (.services | map(select(.name == $service))) as $web
      | (.services | map(select(.name == $relay))) as $meta
      | (.services | map(select(.name == $gmail))) as $gmail_service
      | (.jobs | map(select(.name == $job))) as $migration
      | (((.envs // []) | length) == 0)
        and (((.workers // []) | length) == 0)
        and (((.static_sites // []) | length) == 0)
        and (((.functions // []) | length) == 0)
        and (($web | length) == 1)
        and (($meta | length) == 1)
        and (($gmail_service | length) == 1)
        and (($migration | length) == 1)
        and ((.jobs | length) == 1)
        and ((.services | map(.name) | sort) == ([$service, $relay, $gmail] | sort))
        and all(.services[]; (.git.deploy_on_push // false) == false)
        and all(.jobs[]; (.git.deploy_on_push // false) == false)
        and $web[0].git.repo_clone_url == $repository
        and $web[0].git.branch == "main"
        and $web[0].dockerfile_path == "docker/Dockerfile"
        and $meta[0].git.repo_clone_url == $repository
        and $meta[0].git.branch == "main"
        and $meta[0].dockerfile_path == "docker/meta-relay.Dockerfile"
        and $gmail_service[0].git.repo_clone_url == $repository
        and $gmail_service[0].git.branch == "main"
        and $gmail_service[0].dockerfile_path == "docker/gmail-relay.Dockerfile"
        and $migration[0].kind == "PRE_DEPLOY"
        and $migration[0].git.repo_clone_url == $repository
        and $migration[0].git.branch == "main"
        and $migration[0].dockerfile_path == "docker/Dockerfile"
        and $migration[0].run_command == $standard
        and (($migration[0].envs | map(.key) | sort) == ["WHATOMATE_DATABASE__MIGRATION_URL", "WHATOMATE_DATABASE__RUNTIME_ROLE"])
        and any($migration[0].envs[]; .key == "WHATOMATE_DATABASE__MIGRATION_URL" and .type == "SECRET" and .scope == "RUN_TIME" and (.value | startswith("EV[")))
        and any($migration[0].envs[]; .key == "WHATOMATE_DATABASE__RUNTIME_ROLE" and .value == "rereply_app")
        and (($web[0].envs | group_by(.key) | all(length == 1)))
        and any($web[0].envs[]; .key == "WHATOMATE_APP__ENVIRONMENT" and .value == "production")
        and any($web[0].envs[]; .key == "WHATOMATE_DATABASE__RLS_ENABLED" and .value == "true")
        and any($web[0].envs[]; .key == "WHATOMATE_DATABASE__RUNTIME_ROLE" and .value == "rereply_app")
        and any($web[0].envs[]; .key == "WHATOMATE_META_REGISTRY__ENABLED" and .value == "true")
        and any($web[0].envs[]; .key == "WHATOMATE_META_REGISTRY__QUEUE_READER_VERSION" and .value == "2")
        and any($web[0].envs[]; .key == "WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED" and .value == "true")
        and any($web[0].envs[]; .key == "WHATOMATE_META_MESSENGER_ONBOARDING__ALLOW_ALL_ORGANIZATIONS" and .value == "false")
        and any($web[0].envs[]; .key == "WHATOMATE_META_MESSENGER_ONBOARDING__ALLOWED_ORGANIZATION_IDS" and (.value | length) > 0)
        and ([
          $web[0].envs[]
          | select(
              (.key | startswith("WHATOMATE_META_INSTAGRAM_ONBOARDING__"))
              or (.key | startswith("WHATOMATE_THREADS_MANAGED__"))
              or (.key | startswith("WHATOMATE_THREADS_APP_REVIEW__"))
            )
        ] | length) == 0
        and any($meta[0].envs[]; .key == "META_RELAY_REGISTRY_ENABLED" and .value == "true")
        and any($meta[0].envs[]; .key == "META_RELAY_DYNAMIC_QUEUE_READER_VERSION" and .value == "2")
        and any($meta[0].envs[]; .key == "META_RELAY_REGISTRY_URL" and .value == "https://app.rereply.app/internal/meta-registry/v1/resolve")
        and any($meta[0].envs[]; .key == "META_RELAY_REGISTRY_SECRET" and .type == "SECRET")
        and any($meta[0].envs[]; .key == "META_RELAY_REGISTRY_EDGE_SECRET" and .type == "SECRET")
        and any($meta[0].envs[]; .key == "META_RELAY_ACCOUNTS_JSON" and ((.value | fromjson) | type == "array"))
        and ([ $meta[0].envs[] | select(.key | startswith("META_RELAY_MANAGED_INSTAGRAM")) ] | length) == 0
        and ([
          (.envs // [])[]?,
          .services[]?.envs[]?,
          .jobs[]?.envs[]?
          | select((.type // "GENERAL") != "SECRET")
          | select((.value // "") | contains($target))
        ] | length) == 0
    ' "$spec_path" >/dev/null || die "production app spec does not match the reviewed bootstrap preconditions"
}

build_staged_spec() {
	local baseline_path="$1" command="$2" output_path="$3" deployment_nonce="${4:-}"
	jq \
		--arg service "$APP_SERVICE" \
		--arg job "$MIGRATION_JOB" \
		--arg command "$command" \
		--arg target_key "$TARGET_ENV_KEY" \
		--arg target "$BOOTSTRAP_ORGANIZATION_ID" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
		--arg nonce "$deployment_nonce" '
      (.services[] | select(.name == $service).envs) as $web_envs
      | (.jobs[] | select(.name == $job).envs) as $migration_envs
      | [
          $web_envs[]
          | select(
              .key == "WHATOMATE_APP__ENVIRONMENT"
              or .key == "WHATOMATE_APP__ENCRYPTION_KEY"
              or .key == "WHATOMATE_SERVER__WRITE_TIMEOUT"
              or .key == "WHATOMATE_DATABASE__RLS_ENABLED"
              or (.key | startswith("WHATOMATE_META_REGISTRY__"))
              or (.key | startswith("WHATOMATE_META_MESSENGER_ONBOARDING__"))
              or (.key | startswith("WHATOMATE_META_INSTAGRAM_ONBOARDING__"))
              or (.key | startswith("WHATOMATE_THREADS_MANAGED__"))
              or (.key | startswith("WHATOMATE_THREADS_APP_REVIEW__"))
            )
        ] as $config_envs
      | [
          $migration_envs[]
          | select(
              .key == "WHATOMATE_DATABASE__MIGRATION_URL"
              or .key == "WHATOMATE_DATABASE__RUNTIME_ROLE"
            )
        ] as $owner_envs
      | .jobs |= map(
          if .name == $job then
            .run_command = $command
            | .envs = (
                $config_envs
                + $owner_envs
                + [{
                    key: $target_key,
                    value: $target,
                    scope: "RUN_TIME",
                    type: "SECRET"
                  }]
                + (if $nonce == "" then [] else [{
                    key: $nonce_key,
                    value: $nonce,
                    scope: "RUN_TIME",
                    type: "GENERAL"
                  }] end)
              )
          else . end
        )
    ' "$baseline_path" >"$output_path"

	jq -e \
		--arg job "$MIGRATION_JOB" \
		--arg command "$command" \
		--arg target_key "$TARGET_ENV_KEY" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
		--arg nonce "$deployment_nonce" '
      (.jobs | map(select(.name == $job))) as $migration
      | ($migration | length) == 1
        and $migration[0].run_command == $command
        and (($migration[0].envs | group_by(.key) | all(length == 1)))
        and any($migration[0].envs[]; .key == "WHATOMATE_DATABASE__MIGRATION_URL" and .type == "SECRET")
        and any($migration[0].envs[]; .key == "WHATOMATE_DATABASE__RUNTIME_ROLE" and .value == "rereply_app")
        and any($migration[0].envs[]; .key == "WHATOMATE_META_MESSENGER_ONBOARDING__ALLOWED_ORGANIZATION_IDS")
        and any($migration[0].envs[]; .key == "WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET" and .type == "SECRET")
        and any($migration[0].envs[]; .key == $target_key and .type == "SECRET" and .scope == "RUN_TIME")
        and (if $nonce == "" then
          ([ $migration[0].envs[] | select(.key == $nonce_key) ] | length) == 0
        else
          ([
            $migration[0].envs[]
            | select(
                .key == $nonce_key
                and .value == $nonce
                and .type == "GENERAL"
                and .scope == "RUN_TIME"
              )
          ] | length) == 1
        end)
    ' "$output_path" >/dev/null || die "temporary migration job environment is invalid"
	doctl apps spec validate "$output_path" --schema-only >/dev/null
}

normalize_bootstrap_spec() {
	local input_path="$1" output_path="$2"
	jq --arg job "$MIGRATION_JOB" --arg target_key "$TARGET_ENV_KEY" --arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" '
    .jobs |= map(
      if .name == $job then
        .envs |= (
          map(select(.key != $nonce_key))
          | map(if .key == $target_key then .value = "<protected-target>" else . end)
        )
      else . end
    )
  ' "$input_path" >"$output_path"
}

reconstruct_baseline() {
	local armed_path="$1" output_path="$2"
	jq \
		--arg job "$MIGRATION_JOB" \
		--arg standard "$STANDARD_MIGRATION_COMMAND" '
      .jobs |= map(
        if .name == $job then
          .run_command = $standard
          | .envs = [
              .envs[]
              | select(
                  .key == "WHATOMATE_DATABASE__MIGRATION_URL"
                  or .key == "WHATOMATE_DATABASE__RUNTIME_ROLE"
                )
            ]
        else . end
      )
    ' "$armed_path" >"$output_path"
}

verify_armed_spec() {
	local armed_path="$1" command="$2"
	local expected="${WORK_DIR}/expected-staged.json"
	local normalized_armed="${WORK_DIR}/normalized-armed.json"
	local normalized_expected="${WORK_DIR}/normalized-expected.json"
	jq -e \
		--arg job "$MIGRATION_JOB" \
		--arg target_key "$TARGET_ENV_KEY" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" '
      (.jobs | map(select(.name == $job))) as $migration
      | ([ $migration[0].envs[] | select(.key == $target_key) ]) as $target
      | ([ $migration[0].envs[] | select(.key == $nonce_key) ]) as $nonce
      | ($migration | length) == 1
        and ($target | length) == 1
        and $target[0].type == "SECRET"
        and $target[0].scope == "RUN_TIME"
        and ($target[0].value | startswith("EV["))
        and ($nonce | length) <= 1
        and all($nonce[];
          .type == "GENERAL"
          and .scope == "RUN_TIME"
          and (.value | test("^[0-9]+$"))
        )
    ' "$armed_path" >/dev/null || die "temporary bootstrap credential or deployment nonce drifted"
	reconstruct_baseline "$armed_path" "$BASELINE_SPEC"
	verify_standard_spec "$BASELINE_SPEC"
	build_staged_spec "$BASELINE_SPEC" "$command" "$expected"
	normalize_bootstrap_spec "$armed_path" "$normalized_armed"
	normalize_bootstrap_spec "$expected" "$normalized_expected"
	same_json "$normalized_armed" "$normalized_expected" || die "temporary bootstrap job drifted from the reviewed operation"
}

build_standard_redeployment_spec() {
	local baseline_path="$1" output_path="$2"
	jq \
		--arg job "$MIGRATION_JOB" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
		--arg nonce "$GITHUB_RUN_ID" '
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
    ' "$baseline_path" >"$output_path"
	jq -e \
		--arg job "$MIGRATION_JOB" \
		--arg standard "$STANDARD_MIGRATION_COMMAND" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
		--arg nonce "$GITHUB_RUN_ID" '
      (.jobs | map(select(.name == $job))) as $migration
      | ($migration | length) == 1
        and $migration[0].run_command == $standard
        and ([
          $migration[0].envs[]
          | select(
              .key == $nonce_key
              and .value == $nonce
              and .type == "GENERAL"
              and .scope == "RUN_TIME"
            )
        ] | length) == 1
    ' "$output_path" >/dev/null || die "standard migration redeployment spec is invalid"
	doctl apps spec validate "$output_path" --schema-only >/dev/null
}

verify_standard_redeployment_spec() {
	local input_path="$1"
	local normalized_input="${WORK_DIR}/normalized-standard-redeployment.json"
	local normalized_baseline="${WORK_DIR}/normalized-standard-baseline.json"
	jq -e \
		--arg job "$MIGRATION_JOB" \
		--arg standard "$STANDARD_MIGRATION_COMMAND" \
		--arg target_key "$TARGET_ENV_KEY" \
		--arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" '
      (.jobs | map(select(.name == $job))) as $migration
      | ([ $migration[0].envs[] | select(.key == $target_key) ]) as $target
      | ([ $migration[0].envs[] | select(.key == $nonce_key) ]) as $nonce
      | ($migration | length) == 1
        and $migration[0].run_command == $standard
        and ($target | length) == 0
        and ($nonce | length) == 1
        and $nonce[0].type == "GENERAL"
        and $nonce[0].scope == "RUN_TIME"
        and ($nonce[0].value | test("^[0-9]+$"))
    ' "$input_path" >/dev/null || die "standard migration redeployment spec drifted"
	reconstruct_baseline "$input_path" "$BASELINE_SPEC"
	verify_standard_spec "$BASELINE_SPEC"
	normalize_bootstrap_spec "$input_path" "$normalized_input"
	normalize_bootstrap_spec "$BASELINE_SPEC" "$normalized_baseline"
	same_json "$normalized_input" "$normalized_baseline" || die "standard migration redeployment changed more than its nonce"
}

assert_desired_unchanged() {
	local expected_path="$1"
	doctl apps spec get "$APP_ID" --format json >"$REFETCHED_SPEC"
	same_json "$expected_path" "$REFETCHED_SPEC" || die "production app spec changed concurrently"
}

capture_update_deployment() {
	local current_path="$1" staged_path="$2" output deployment_id
	assert_desired_unchanged "$current_path"
	output="$(doctl apps update "$APP_ID" --spec "$staged_path" --format 'InProgressDeployment.ID' --no-header)"
	deployment_id="$(grep -Eo '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' <<<"$output" | tail -n 1 || true)"
	is_uuid "$deployment_id" || die "DigitalOcean did not return a deployment identifier"
	printf '%s' "$deployment_id"
}

wait_for_deployment() {
	local deployment_id="$1" deadline phase
	deadline=$((SECONDS + 2700))
	while ((SECONDS < deadline)); do
		doctl apps get-deployment "$APP_ID" "$deployment_id" --output json >"$DEPLOYMENT_STATE"
		jq -e 'length == 1' "$DEPLOYMENT_STATE" >/dev/null || return 1
		phase="$(jq -r '.[0].phase // empty' "$DEPLOYMENT_STATE")"
		case "$phase" in
		ACTIVE)
			jq -e \
				--arg sha "$GITHUB_SHA" \
				--arg service "$APP_SERVICE" \
				--arg relay "$META_RELAY_SERVICE" \
				--arg gmail "$GMAIL_RELAY_SERVICE" \
				--arg job "$MIGRATION_JOB" '
            .[0] as $deployment
            | ($deployment.progress.success_steps == $deployment.progress.total_steps)
              and ((($deployment.workers // []) | length) == 0)
              and ((($deployment.static_sites // []) | length) == 0)
              and ((($deployment.functions // []) | length) == 0)
              and (($deployment.services | map(.name) | sort) == ([$service, $relay, $gmail] | sort))
              and (($deployment.jobs | map(.name)) == [$job])
              and all($deployment.services[]; .source_commit_hash == $sha)
              and all($deployment.jobs[]; .source_commit_hash == $sha)
          ' "$DEPLOYMENT_STATE" >/dev/null || return 1
			return 0
			;;
		ERROR | CANCELED | SUPERSEDED)
			return 1
			;;
		esac
		sleep 10
	done
	return 1
}

capture_bootstrap_log() {
	local deployment_id="$1"
	doctl apps logs "$APP_ID" "$MIGRATION_JOB" \
		--deployment "$deployment_id" --type run --no-prefix >"$DEPLOYMENT_LOG" 2>&1 || true
	tr -d '\r' <"$DEPLOYMENT_LOG" >"${DEPLOYMENT_LOG}.normalized"
	mv "${DEPLOYMENT_LOG}.normalized" "$DEPLOYMENT_LOG"
	if grep -Fq "$BOOTSTRAP_ORGANIZATION_ID" "$DEPLOYMENT_LOG" ||
		grep -Eqi 'postgres(ql)?://|password[=:]|WHATOMATE_DATABASE__MIGRATION_URL|EV\[' "$DEPLOYMENT_LOG"; then
		printf 'Deployment log contained disallowed sensitive material; output was suppressed.\n' >&2
		return 1
	fi
}

has_exact_definite_no_commit_log() {
	local fixed_message
	for fixed_message in \
		'Invalid bootstrap flags: use named flags only, write booleans as -apply or -apply=false, and do not use positional arguments or --; no configuration was loaded and no database changes were made.' \
		'A canonical non-nil -organization-id is required; no database changes were made.' \
		'A reviewed -operator-run-id is required; no database changes were made.' \
		'Failed to load the compliance bootstrap configuration; no database changes were made.' \
		'database.migration_url is required; no database changes were made.' \
		'Failed to connect using database.migration_url; no database changes were made.' \
		'Failed to initialize the migration database connection; no database changes were made.'; do
		if [[ "$(grep -Fxc "$fixed_message" "$DEPLOYMENT_LOG" || true)" == "1" ]]; then
			return 0
		fi
	done
	grep -Eq '^Platform compliance bootstrap failed: .+; the transaction was rolled back and no changes were committed\.$' "$DEPLOYMENT_LOG"
}

inspect_bootstrap_log() {
	local deployment_id="$1" phase="$2" fresh_count committed_count post_count
	if ! capture_bootstrap_log "$deployment_id"; then
		[[ "$phase" == "apply" ]] && return 42
		return 1
	fi
	if [[ "$phase" == "dry-run" ]]; then
		if [[ "$(grep -Fxc "$DRY_FRESH" "$DEPLOYMENT_LOG" || true)" != "1" ]]; then
			printf 'Dry-run did not emit the exact reviewed result.\n' >&2
			return 1
		fi
		printf '%s\n' "$DRY_FRESH"
		return 0
	fi

	if grep -Fq 'Platform compliance bootstrap apply outcome is indeterminate' "$DEPLOYMENT_LOG"; then
		printf 'Apply outcome is indeterminate. Dispatch the identical apply operation again before any other action.\n' >&2
		return 42
	fi
	fresh_count="$(grep -Fxc "$APPLY_FRESH" "$DEPLOYMENT_LOG" || true)"
	committed_count="$(grep -Fxc "$APPLY_COMMITTED" "$DEPLOYMENT_LOG" || true)"
	post_count="$(grep -Fxc "$DRY_COMMITTED" "$DEPLOYMENT_LOG" || true)"
	if [[ "$post_count" == "1" && "$fresh_count" == "1" && "$committed_count" == "0" ]]; then
		printf '%s\n' "$APPLY_FRESH"
		printf '%s\n' "$DRY_COMMITTED"
		return 0
	fi
	if [[ "$post_count" == "1" && "$fresh_count" == "0" && "$committed_count" == "1" ]]; then
		printf '%s\n' "$APPLY_COMMITTED"
		printf '%s\n' "$DRY_COMMITTED"
		return 0
	fi
	if [[ "$post_count" == "0" && "$fresh_count" == "0" && "$committed_count" == "0" ]] &&
		has_exact_definite_no_commit_log; then
		: >"$DEFINITE_NO_COMMIT_MARKER"
		printf 'Apply failed before commit; the temporary execution surface may be retried or restored safely.\n' >&2
		return 1
	fi
	printf 'Apply result is missing, duplicated, ambiguous, or lacks its exact post-commit proof.\n' >&2
	return 42
}

execute_operation() {
	local command current_command active_command deployment_id inspection_status
	local deployment_nonce="" standard_nonce_count
	verify_github_control_plane
	verify_healthy_active
	doctl apps spec get "$APP_ID" --format json >"$DESIRED_SPEC"
	command="$(phase_command)"
	current_command="$(jq -r --arg job "$MIGRATION_JOB" '.jobs[] | select(.name == $job) | .run_command' "$DESIRED_SPEC")"
	active_command="$(jq -r --arg job "$MIGRATION_JOB" '.jobs[] | select(.name == $job) | .run_command' "$ACTIVE_SPEC")"

	if [[ "$current_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
		standard_nonce_count="$(jq -r --arg job "$MIGRATION_JOB" --arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
			'[.jobs[] | select(.name == $job).envs[] | select(.key == $nonce_key)] | length' "$DESIRED_SPEC")"
		if [[ "$standard_nonce_count" == "0" ]]; then
			verify_standard_spec "$DESIRED_SPEC"
			if same_json "$DESIRED_SPEC" "$ACTIVE_SPEC"; then
				cp "$DESIRED_SPEC" "$BASELINE_SPEC"
			else
				if [[ "$active_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
					verify_standard_redeployment_spec "$ACTIVE_SPEC"
				else
					verify_armed_spec "$ACTIVE_SPEC" "$command"
				fi
				same_json "$DESIRED_SPEC" "$BASELINE_SPEC" ||
					die "standard desired spec does not match the recoverable active deployment"
				deployment_nonce="$GITHUB_RUN_ID"
			fi
		elif [[ "$standard_nonce_count" == "1" ]]; then
			verify_standard_redeployment_spec "$DESIRED_SPEC"
			cp "$BASELINE_SPEC" "$RECOVERY_BASELINE"
			if same_json "$DESIRED_SPEC" "$ACTIVE_SPEC" || same_json "$BASELINE_SPEC" "$ACTIVE_SPEC"; then
				:
			else
				if [[ "$active_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
					verify_standard_redeployment_spec "$ACTIVE_SPEC"
				else
					verify_armed_spec "$ACTIVE_SPEC" "$command"
				fi
				same_json "$RECOVERY_BASELINE" "$BASELINE_SPEC" ||
					die "transient standard desired spec does not match the recoverable active deployment"
			fi
			cp "$RECOVERY_BASELINE" "$BASELINE_SPEC"
			deployment_nonce="$GITHUB_RUN_ID"
		else
			die "standard migration job contains an unexpected deployment nonce"
		fi
	elif [[ "$current_command" == "$command" ]]; then
		verify_armed_spec "$DESIRED_SPEC" "$command"
		cp "$BASELINE_SPEC" "$RECOVERY_BASELINE"
		if ! same_json "$DESIRED_SPEC" "$ACTIVE_SPEC" && ! same_json "$BASELINE_SPEC" "$ACTIVE_SPEC"; then
			if [[ "$active_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
				verify_standard_redeployment_spec "$ACTIVE_SPEC"
			else
				verify_armed_spec "$ACTIVE_SPEC" "$command"
			fi
			same_json "$RECOVERY_BASELINE" "$BASELINE_SPEC" ||
				die "armed desired spec does not match the recoverable active deployment"
		fi
		cp "$RECOVERY_BASELINE" "$BASELINE_SPEC"
		deployment_nonce="$GITHUB_RUN_ID"
	else
		die "migration job is not in the exact standard or recoverable state"
	fi

	build_staged_spec "$BASELINE_SPEC" "$command" "$STAGED_SPEC" "$deployment_nonce"
	deployment_id="$(capture_update_deployment "$DESIRED_SPEC" "$STAGED_SPEC")"
	wait_for_deployment "$deployment_id" || true
	if inspect_bootstrap_log "$deployment_id" "$BOOTSTRAP_PHASE"; then
		return 0
	else
		inspection_status=$?
	fi
	return "$inspection_status"
}

wait_for_existing_deployment() {
	doctl apps get "$APP_ID" --output json >"$APP_STATE"
	local deployment_id
	for deployment_id in \
		"$(jq -r '.[0].in_progress_deployment.id // empty' "$APP_STATE")" \
		"$(jq -r '.[0].pending_deployment.id // empty' "$APP_STATE")"; do
		if [[ -n "$deployment_id" ]]; then
			is_uuid "$deployment_id" || die "pending deployment identifier is invalid"
			wait_for_deployment "$deployment_id" || true
		fi
	done
}

verify_public_readiness() {
	curl --fail --silent --show-error --retry 12 --retry-all-errors --retry-delay 5 \
		https://app.rereply.app/ready >/dev/null
	curl --fail --silent --show-error --retry 12 --retry-all-errors --retry-delay 5 \
		https://app.rereply.app/meta-relay/readyz >/dev/null
	curl --fail --silent --show-error --retry 12 --retry-all-errors --retry-delay 5 \
		https://app.rereply.app/gmail-relay/readyz >/dev/null
}

cleanup_operation() {
	local command current_command active_command deployment_id standard_nonce_count
	verify_basic_context
	command="$(phase_command)"
	wait_for_existing_deployment
	verify_healthy_active
	doctl apps spec get "$APP_ID" --format json >"$DESIRED_SPEC"
	current_command="$(jq -r --arg job "$MIGRATION_JOB" '.jobs[] | select(.name == $job) | .run_command' "$DESIRED_SPEC")"
	active_command="$(jq -r --arg job "$MIGRATION_JOB" '.jobs[] | select(.name == $job) | .run_command' "$ACTIVE_SPEC")"

	if [[ "$current_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
		standard_nonce_count="$(jq -r --arg job "$MIGRATION_JOB" --arg nonce_key "$DEPLOYMENT_NONCE_ENV_KEY" \
			'[.jobs[] | select(.name == $job).envs[] | select(.key == $nonce_key)] | length' "$DESIRED_SPEC")"
		if [[ "$standard_nonce_count" == "0" ]]; then
			verify_standard_spec "$DESIRED_SPEC"
			if same_json "$DESIRED_SPEC" "$ACTIVE_SPEC"; then
				verify_public_readiness
				printf 'Migration job was already restored and production readiness verified.\n'
				return 0
			fi

			if [[ "$active_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
				verify_standard_redeployment_spec "$ACTIVE_SPEC"
			else
				verify_armed_spec "$ACTIVE_SPEC" "$command"
			fi
			same_json "$DESIRED_SPEC" "$BASELINE_SPEC" ||
				die "standard desired spec does not match the recoverable active deployment"
			build_standard_redeployment_spec "$BASELINE_SPEC" "$STAGED_SPEC"
			deployment_id="$(capture_update_deployment "$DESIRED_SPEC" "$STAGED_SPEC")"
			wait_for_deployment "$deployment_id" || die "standard migration recovery deployment failed"
			verify_healthy_active
			doctl apps spec get "$APP_ID" --format json >"$DESIRED_SPEC"
			same_json "$DESIRED_SPEC" "$STAGED_SPEC" || die "standard migration recovery spec did not become desired"
			deployment_id="$(capture_update_deployment "$DESIRED_SPEC" "$BASELINE_SPEC")"
			wait_for_deployment "$deployment_id" || die "exact standard cleanup deployment failed"
		elif [[ "$standard_nonce_count" == "1" ]]; then
			verify_standard_redeployment_spec "$DESIRED_SPEC"
			cp "$BASELINE_SPEC" "$RECOVERY_BASELINE"
			if ! same_json "$DESIRED_SPEC" "$ACTIVE_SPEC" && ! same_json "$BASELINE_SPEC" "$ACTIVE_SPEC"; then
				if [[ "$active_command" == "$STANDARD_MIGRATION_COMMAND" ]]; then
					verify_standard_redeployment_spec "$ACTIVE_SPEC"
				else
					verify_armed_spec "$ACTIVE_SPEC" "$command"
				fi
				same_json "$RECOVERY_BASELINE" "$BASELINE_SPEC" ||
					die "transient standard desired spec does not match the recoverable active deployment"
			fi
			cp "$RECOVERY_BASELINE" "$BASELINE_SPEC"
			deployment_id="$(capture_update_deployment "$DESIRED_SPEC" "$BASELINE_SPEC")"
			wait_for_deployment "$deployment_id" || die "transient standard cleanup deployment failed"
		else
			die "standard migration job contains an unexpected deployment nonce"
		fi
	else
		[[ "$current_command" == "$command" ]] || die "cleanup refused unexpected migration job drift"
		verify_armed_spec "$DESIRED_SPEC" "$command"
		doctl apps spec validate "$BASELINE_SPEC" --schema-only >/dev/null
		deployment_id="$(capture_update_deployment "$DESIRED_SPEC" "$BASELINE_SPEC")"
		wait_for_deployment "$deployment_id" || die "cleanup deployment failed"
	fi

	verify_healthy_active
	doctl apps spec get "$APP_ID" --format json >"$REFETCHED_SPEC"
	verify_standard_spec "$REFETCHED_SPEC"
	same_json "$REFETCHED_SPEC" "$BASELINE_SPEC" || die "restored desired spec differs from the exact baseline"
	same_json "$REFETCHED_SPEC" "$ACTIVE_SPEC" || die "restored desired spec is not the active spec"
	verify_public_readiness
	printf 'Migration job restored and all public readiness checks passed.\n'
}

main() {
	require_tool curl
	require_tool doctl
	require_tool gh
	require_tool jq
	require_tool sha256sum
	mkdir -p "$WORK_DIR"
	chmod 700 "$WORK_DIR"

	local status=0
	local operator_status=0
	case "${1:-}" in
	execute)
		if [[ "${BOOTSTRAP_PHASE:-}" == "apply" && -n "${GITHUB_OUTPUT:-}" ]]; then
			# Fail closed if the runner is interrupted after DigitalOcean accepts
			# the apply spec but before the final status can be classified.
			printf 'operator_status=42\n' >>"$GITHUB_OUTPUT"
		fi
		set +e
		(
			trap - EXIT
			set -Eeuo pipefail
			execute_operation
		)
		status=$?
		set -e
		operator_status="$status"
		if [[ "${BOOTSTRAP_PHASE:-}" == "apply" && "$status" != "0" ]]; then
			operator_status=42
			if [[ -f "$DEFINITE_NO_COMMIT_MARKER" ]]; then
				operator_status=1
			fi
		fi
		if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
			printf 'operator_status=%s\n' "$operator_status" >>"$GITHUB_OUTPUT"
		fi
		;;
	cleanup) cleanup_operation ;;
	*) die "expected execute or cleanup" ;;
	esac
	return "$status"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
