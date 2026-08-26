from __future__ import annotations

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
