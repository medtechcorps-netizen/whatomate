from __future__ import annotations

import copy
import datetime as dt
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import do_app
import ship
import ship_common as common
import spec_images
import test_ship_support as support


HERE = Path(__file__).resolve().parent
OLD_MODULES = (
    "verify_production_release", "apply_production_change", "observe_production_recovery",
    "provider_native_valkey_recovery", "verify_production_plan", "verify_production_crm_canary",
    "launch_production_prerequisites",
)


class ProductionCase(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.repo, self.base = support.base_repo(self.root / "repo")
        self.head = self.repo.commit({"README.md": "readme v2\n"}, "feature")
        self.h = support.Harness(self.root, repo=self.repo, head=self.head, bootstrap_sha=self.base)
        self.clients: list[do_app.DOAppClient] = []
        original = do_app.DOAppClient

        def recording(*args: object, **kwargs: object) -> do_app.DOAppClient:
            client = original(*args, **kwargs)
            self.clients.append(client)
            return client

        patcher = mock.patch.object(do_app, "DOAppClient", side_effect=recording)
        patcher.start()
        self.addCleanup(patcher.stop)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def production(self, mode: str = "promote", **kwargs: object) -> int:
        self.h.production_env(mode, **kwargs)
        return self.h.run("production")

    def assert_sanitized(self) -> None:
        for label, text in (("stdout", self.h.text()), ("summary", self.h.summary()),
                            ("outputs", json.dumps(self.h.outputs()))):
            with self.subTest(channel=label):
                self.assertEqual(support.scan_private(text), [])
        for argv, env in self.h.gh.calls:
            self.assertNotIn("SHIP_DO_TOKEN", env)
            self.assertNotIn("SHIP_TARGET_JSON", env)
            self.assertNotIn(support.DO_TOKEN, json.dumps([argv, env]))
            self.assertNotIn(support.APP_ID, json.dumps([argv, env]))
        self.assertNotIn("SHIP_DO_TOKEN", self.h.env)
        self.assertNotIn("SHIP_TARGET_JSON", self.h.env)

    def desired_spec(self, images: dict = support.NEW) -> dict:
        return spec_images.set_images(support.make_spec(), images)


class DryRunTests(ProductionCase):
    def test_dry_run_makes_zero_puts_and_prints_the_digest_diff(self) -> None:
        self.assertEqual(self.production("dry-run"), ship.EXIT_OK)
        self.assertEqual(self.h.do.put_count(), 0)
        text = self.h.text()
        self.assertIn("dry-run complete: no PUT", text)
        for component in common.COMPONENTS:
            self.assertIn(support.LIVE[component], text)
            self.assertIn(support.NEW[component], text)
        self.assertIn("environment/topology fingerprints unchanged, VPC bound", self.h.summary())
        self.assertEqual(len(self.h.https.calls), 6)
        self.assertEqual(self.h.outputs(), {})
        self.assertFalse(self.clients[0].allow_put)
        self.assert_sanitized()

    def test_dry_run_cas_change_fails(self) -> None:
        def change(provider: support.FakeDO, path: str) -> None:
            if path.endswith("/backups?page=1&per_page=200"):
                provider.updated_at = "2026-10-04T11:59:00Z"

        self.h.do.get_hooks.append(change)
        self.assertEqual(self.production("dry-run"), ship.EXIT_REFUSED)
        self.assertIn("refused: cas-changed", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)

    def test_dry_run_still_requires_the_backup_gate(self) -> None:
        self.h.do = support.FakeDO(backup_age_hours=37)
        self.assertEqual(self.production("dry-run"), ship.EXIT_REFUSED)
        self.assertIn("refused: backup-stale", self.h.text())


class PromoteTests(ProductionCase):
    def test_promote_success(self) -> None:
        self.assertEqual(self.production(), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        body = self.h.do.put_bodies[0]
        self.assertEqual(body["spec"], self.desired_spec())
        self.assertIs(body["update_all_source_versions"], False)
        self.assertEqual(spec_images.changed_leaf_pointers(support.make_spec(), body["spec"]),
                         sorted(spec_images.image_digest_pointers(support.make_spec())))
        active = self.h.do.deployments[self.h.do.active]
        self.assertEqual(active["jobs"][0]["source_image_digest"], support.NEW["web"])
        outputs = self.h.outputs()
        self.assertEqual(outputs["kind"], "promote")
        self.assertEqual(outputs["sha"], self.head)
        self.assertEqual(outputs["release"], f"prod-20261004T120000Z-{self.head[:8]}")
        self.assertEqual((outputs["web_digest"], outputs["meta_relay_digest"], outputs["gmail_relay_digest"]),
                         (support.NEW["web"], support.NEW["meta-relay"], support.NEW["gmail-relay"]))
        self.assertEqual(outputs["previous"], "prod-0000")
        self.assertEqual(outputs["rolled_back_from"], "")
        self.assertEqual(outputs["built_at"], "2026-10-04T11:00:00Z")
        self.assertEqual(outputs["reconciled"], "false")
        self.assertEqual(len(self.h.https.calls), 6)
        self.assertEqual(len(self.clients), 1)
        methods = {method for method, _ in self.h.do.requests}
        self.assertEqual(methods, {"GET", "PUT"})
        self.assert_sanitized()

    def test_first_run_binds_to_the_bootstrap_record(self) -> None:
        self.h.production_env()
        self.h.env["PLAN_LATEST_RELEASE"] = "prod-20261003T000000Z-00000000"
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("latest-changed-since-plan", self.h.text())

    def test_lagging_deployment_listing_never_locks_the_old_active(self) -> None:
        self.h.do = support.FakeDO(lag=2)
        self.assertEqual(self.production(), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)

    def test_provider_rejection_is_exit_1_and_unchanged(self) -> None:
        self.h.do = support.FakeDO(scenarios=["reject-422"])
        self.assertEqual(self.production(), ship.EXIT_REFUSED)
        self.assertIn("refused: provider-rejected; production unchanged", self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertEqual(self.h.do.spec, support.make_spec())
        self.assert_sanitized()

    def test_ambiguous_applied_is_reconciled_with_one_put(self) -> None:
        self.h.do = support.FakeDO(scenarios=["ambiguous-applied"])
        self.assertEqual(self.production(), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertIn("ambiguous, reconciled", self.h.text())

    def test_ambiguous_not_applied_is_manual_with_one_put(self) -> None:
        self.h.do = support.FakeDO(scenarios=["ambiguous-not-applied"])
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertIn("ambiguous-not-observed", self.h.text())
        self.assertIn("follow docs/emergency-rollback.md", self.h.text())

    def test_reconcile_timeout_is_manual_without_a_rollback_put(self) -> None:
        self.h.do = support.FakeDO(scenarios=["stall"])
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertIn("reconcile-timeout", self.h.text())
        self.assertEqual(len(self.clients), 1)

    def test_error_rolls_back_with_a_separate_client(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error", "accept"])
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)
        self.assertEqual(self.h.do.put_bodies[1]["spec"], support.make_spec())
        self.assertEqual(len(self.clients), 2)
        self.assertEqual([client.put_count() for client in self.clients], [1, 1])
        self.assertIn("deployment-error; rolled back to prod-0000", self.h.text())
        self.assertEqual(self.h.outputs(), {})
        self.assertEqual(spec_images.extract_image_digests(self.h.do.spec), support.LIVE)
        self.assert_sanitized()

    def test_error_then_someone_else_changed_live_is_manual(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error"])
        failed = support.NEW_DEPLOYMENT_IDS[0]

        def console_edit(provider: support.FakeDO, path: str) -> None:
            if provider.deployments.get(failed, {}).get("phase") == "ERROR":
                provider.spec["services"][0]["instance_count"] = 5

        self.h.do.get_hooks.append(console_edit)
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertIn("rollback-precondition-failed", self.h.text())

    def test_cancel_rolls_back(self) -> None:
        self.h.do = support.FakeDO(scenarios=["cancel", "accept"])
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK)

    def test_smoke_failure_rolls_back(self) -> None:
        self.h.https = support.FakeHttps(script=[False, False])
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)
        self.assertEqual(self.h.do.put_bodies[1]["spec"], support.make_spec())
        self.assertIn("smoke-failed; rolled back to prod-0000", self.h.text())

    def test_rollback_put_ambiguous_then_reconciled(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error", "ambiguous-applied"])
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)

    def test_rollback_put_rejected_is_manual(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error", "reject-422"])
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertIn("rollback-failed:provider-rejected", self.h.text())

    def test_rollback_deployment_error_is_manual(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error", "error"])
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertEqual(self.h.do.put_count(), 2)

    def test_health_failure_after_rollback_is_manual(self) -> None:
        self.h.https = support.FakeHttps(failing=True)
        self.assertEqual(self.production(), ship.EXIT_MANUAL)
        self.assertEqual(self.h.do.put_count(), 2)
        self.assertIn("rollback-failed:health", self.h.text())

    def test_migration_failure_rolls_back(self) -> None:
        self.h.do.migration_plan = ["ERROR"]
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertIn("post-deploy-guard; rolled back", self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)

    def test_migration_on_the_wrong_web_digest_rolls_back(self) -> None:
        self.h.do.digest_plan = [support.digest("9")]
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)

    def test_cas_change_right_before_the_put_refuses(self) -> None:
        def change(provider: support.FakeDO, path: str) -> None:
            if path.endswith("/backups?page=1&per_page=200"):
                provider.updated_at = "2026-10-04T11:59:00Z"

        self.h.do.get_hooks.append(change)
        self.assertEqual(self.production(), ship.EXIT_REFUSED)
        self.assertEqual(self.h.do.put_count(), 0)


class RefusalTests(ProductionCase):
    def refused(self, code: str, *, requests: bool | None = None) -> None:
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED, self.h.text())
        self.assertIn(f"refused: {code}", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)
        if requests is False:
            self.assertEqual(self.h.do.requests, [])
        self.assert_sanitized()

    def test_drift_is_refused_with_zero_puts(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.OTHER))
        self.h.production_env()
        self.refused("drift")

    def test_backup_stale(self) -> None:
        self.h.do = support.FakeDO(backup_age_hours=40)
        self.h.production_env()
        self.refused("backup-stale")

    def test_vpc_missing(self) -> None:
        spec = support.make_spec()
        spec.pop("vpc")
        self.h.do = support.FakeDO(spec=spec)
        self.h.production_env()
        self.refused("vpc-missing-or-differs")

    def test_legacy_git_source_is_refused(self) -> None:
        spec = support.make_spec()
        spec["services"][0].pop("image")
        spec["services"][0]["git"] = {"repo_clone_url": "https://example.invalid/r.git", "branch": "main"}
        self.h.do = support.FakeDO(spec=spec)
        self.h.production_env()
        self.refused("topology-differs:source-mode")

    def test_forbidden_image_field(self) -> None:
        spec = support.make_spec()
        spec["services"][1]["image"]["deploy_on_push"] = {"enabled": True}
        self.h.do = support.FakeDO(spec=spec)
        self.h.production_env()
        self.refused("forbidden-image-field")

    def test_app_id_mismatch_makes_zero_requests(self) -> None:
        self.h.production_env()
        self.h.env["SHIP_TARGET_JSON"] = json.dumps(
            {"schema_version": 1, "app_id": "99999999-9999-4999-8999-999999999999", "postgres_cluster_id": support.PG_ID}
        )
        self.refused("app-identity-mismatch", requests=False)
        self.assertEqual(self.h.gh.calls, [])

    def test_malformed_target_secret(self) -> None:
        for value in ("", "{}", json.dumps({"schema_version": 1, "app_id": support.APP_ID}),
                      json.dumps({"schema_version": 1, "app_id": support.APP_ID, "postgres_cluster_id": "x"})):
            with self.subTest(value=value[:20]):
                self.h.production_env()
                self.h.env["SHIP_TARGET_JSON"] = value
                self.h.stdout.truncate(0)
                self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
                self.assertIn("refused: target-invalid", self.h.text())

    def test_pinned_deployment(self) -> None:
        self.h.do = support.FakeDO(pinned=True)
        self.h.production_env()
        self.refused("cas-changed:deployment-pending")

    def test_latest_changed_since_plan(self) -> None:
        self.h.production_env()
        self.h.env["PLAN_LATEST_MANIFEST_SHA256"] = "0" * 64
        self.refused("latest-changed-since-plan", requests=False)

    def test_record_added_after_the_plan(self) -> None:
        record = support.manifest(kind="promote", sha=self.head, images=support.NEW, previous="prod-0000",
                                  deployed_at="2026-10-04T10:00:00Z")
        self.h.gh.add_record(record)
        self.h.production_env()
        self.refused("latest-changed-since-plan", requests=False)

    def test_candidate_tampered_stale_foreign_or_future(self) -> None:
        cases = {
            "tampered": lambda env: env.update({"CANDIDATE_SHA256": "0" * 64}),
            "not-base64": lambda env: env.update({"CANDIDATE_B64": "%%%"}),
            "stale": lambda env: env.update(dict(zip(("CANDIDATE_B64", "CANDIDATE_SHA256"), support.candidate_b64(
                support.NEW, self.head, support.NOW - dt.timedelta(hours=25))))),
            "future": lambda env: env.update(dict(zip(("CANDIDATE_B64", "CANDIDATE_SHA256"), support.candidate_b64(
                support.NEW, self.head, support.NOW + dt.timedelta(minutes=5))))),
            "foreign": lambda env: env.update(dict(zip(("CANDIDATE_B64", "CANDIDATE_SHA256"), support.candidate_b64(
                support.NEW, self.base, support.NOW - dt.timedelta(hours=1))))),
            "bootstrap-digest": lambda env: env.update(dict(zip(("CANDIDATE_B64", "CANDIDATE_SHA256"), support.candidate_b64(
                dict(support.NEW, web=support.LIVE["web"]), self.head, support.NOW - dt.timedelta(hours=1))))),
        }
        expected = {"stale": "candidate-stale", "future": "candidate-stale"}
        for label, mutate in cases.items():
            with self.subTest(case=label):
                self.h.production_env()
                mutate(self.h.env)
                self.h.stdout.truncate(0)
                self.h.stdout.seek(0)
                self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
                self.assertIn(f"refused: {expected.get(label, 'candidate-invalid')}", self.h.text())
                self.assertEqual(self.h.do.requests, [])

    def test_attestation_failure(self) -> None:
        self.h.production_env()
        self.h.gh.image_attestations = {key: value for key, value in self.h.gh.image_attestations.items()
                                        if key[1] != support.NEW["gmail-relay"]}
        self.refused("attestation-unverified", requests=False)

    def test_downgrade(self) -> None:
        self.repo.checkout(self.base)
        sibling = self.repo.commit({"other.txt": "x"}, "sibling")
        record = support.manifest(kind="promote", sha=sibling, images=support.NEW, previous="prod-0000",
                                  deployed_at="2026-10-04T10:00:00Z")
        raw = self.h.gh.add_record(record)
        self.h.gh.attest_ship_images(support.OTHER, self.head)
        self.h.production_env(images=support.OTHER)
        self.h.env["PLAN_LATEST_RELEASE"] = record["release"]
        self.h.env["PLAN_LATEST_MANIFEST_SHA256"] = support.hashlib.sha256(raw).hexdigest()
        self.refused("downgrade-refused", requests=False)

    def test_schema_change(self) -> None:
        self.head = self.repo.commit({"internal/models/new.go": "package models\n"}, "model")
        self.h.head = self.head
        self.h.env.update(support.context_env(self.head, self.root))
        self.h.gh.ci_green(self.head)
        self.h.production_env()
        self.refused("schema-change-blocked", requests=False)
        self.assertIn("guarded-tree:internal/models/new.go", self.h.text())

    def test_context_refusals(self) -> None:
        for change in ({"GITHUB_RUN_ATTEMPT": "2"}, {"RUNNER_DEBUG": "1"}, {"GITHUB_REF": "refs/heads/other"},
                       {"DIGITALOCEAN_ACCESS_TOKEN": "x"}, {"DO_TOKEN": ""}):
            with self.subTest(change=sorted(change)):
                self.h.production_env()
                self.h.env.update(change)
                self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
                self.assertIn("refused: context-invalid", self.h.text())
                self.assertEqual(self.h.do.requests, [])
                for name in change:
                    self.h.env.pop(name, None)
                self.h.env.update(support.context_env(self.head, self.root))

    def test_old_lane_active(self) -> None:
        self.h.gh.active["in_progress"] = [support.run_record(".github/workflows/old-lane.yml", status="in_progress",
                                                              conclusion=None, run_id=9)]
        self.h.production_env()
        self.refused("old-lane-active", requests=False)

    def test_unrelated_active_runs_do_not_block(self) -> None:
        self.h.gh.active["queued"] = [support.run_record(".github/workflows/test.yml", status="queued", conclusion=None)]
        self.assertEqual(self.production("dry-run"), ship.EXIT_OK, self.h.text())

    def test_mode_inputs(self) -> None:
        self.h.production_env()
        self.h.env["SHIP_MODE"] = "deploy"
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.h.production_env()
        self.h.env["PLAN_TARGET_RELEASE"] = "prod-0000"
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)


class RollbackModeTests(ProductionCase):
    def setUp(self) -> None:
        super().setUp()
        self.record = support.manifest(kind="promote", sha=self.head, images=support.NEW, previous="prod-0000",
                                       deployed_at="2026-10-04T10:00:00Z")
        self.raw = self.h.gh.add_record(self.record)
        self.h.gh.attest_ship_images(support.NEW, self.head)

    def rollback_env(self, target: str, target_sha256: str) -> None:
        self.h.production_env("rollback", target_release=target)
        self.h.env["PLAN_LATEST_RELEASE"] = self.record["release"]
        self.h.env["PLAN_LATEST_MANIFEST_SHA256"] = support.hashlib.sha256(self.raw).hexdigest()
        self.h.env["PLAN_TARGET_MANIFEST_SHA256"] = target_sha256

    def test_rollback_to_prod_0000_uses_the_legacy_signer(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW))
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertEqual(spec_images.extract_image_digests(self.h.do.put_bodies[0]["spec"]), support.LIVE)
        outputs = self.h.outputs()
        self.assertEqual((outputs["kind"], outputs["sha"], outputs["rolled_back_from"], outputs["previous"]),
                         ("rollback", self.base, self.record["release"], self.record["release"]))
        self.assertEqual(outputs["reconciled"], "false")
        self.assertEqual(outputs["built_at"], "")
        legacy = [argv for argv, _ in self.h.gh.calls
                  if "attestation" in argv and f"{common.REPOSITORY}/.github/workflows/build-attest-exact-release-images.yml" in argv]
        self.assertEqual(len(legacy), 6)
        self.assert_sanitized()

    def test_reconcile_after_a_console_rollback_makes_no_put(self) -> None:
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)
        outputs = self.h.outputs()
        self.assertEqual((outputs["kind"], outputs["reconciled"], outputs["sha"]), ("rollback", "true", self.base))
        self.assertIn("reconcile", self.h.text())

    def test_restore_when_live_matches_no_record(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.OTHER))
        self.rollback_env(self.record["release"], support.hashlib.sha256(self.raw).hexdigest())
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertEqual(spec_images.extract_image_digests(self.h.do.spec), support.NEW)
        outputs = self.h.outputs()
        self.assertEqual((outputs["kind"], outputs["sha"], outputs["rolled_back_from"]), ("restore", self.head, ""))

    def test_nothing_to_roll_back(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW))
        self.rollback_env(self.record["release"], support.hashlib.sha256(self.raw).hexdigest())
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("refused: nothing-to-roll-back", self.h.text())

    def test_drift_in_rollback_mode(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.OTHER))
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("refused: drift", self.h.text())

    def test_target_binding_and_unknown_targets(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW))
        self.rollback_env("prod-0000", "0" * 64)
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("latest-changed-since-plan:target", self.h.text())
        self.rollback_env("prod-20990101T000000Z-99999999", "0" * 64)
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("rollback-target-invalid", self.h.text())

    def test_failed_rollback_put_is_itself_rolled_back(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW), scenarios=["error", "accept"])
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(spec_images.extract_image_digests(self.h.do.spec), support.NEW)


class EntrypointTests(unittest.TestCase):
    def test_help_runs_isolated(self) -> None:
        result = subprocess.run([sys.executable, "-I", "-S", "-B", str(HERE / "ship.py"), "--help"],
                                capture_output=True, check=False, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(b"production", result.stdout)

    def test_imports_only_new_sibling_modules(self) -> None:
        code = (
            "import sys; sys.path.insert(0, sys.argv[1]); import ship; "
            "print(','.join(sorted(name for name in sys.modules if name in sys.argv[2].split(','))))"
        )
        result = subprocess.run([sys.executable, "-I", "-S", "-B", "-c", code, str(HERE), ",".join(OLD_MODULES)],
                                capture_output=True, check=False, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), b"")

    def test_unexpected_exceptions_print_no_traceback(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo, base = support.base_repo(root / "repo")
            harness = support.Harness(root, repo=repo, head=repo.commit({"x": "y"}), bootstrap_sha=base)
            harness.production_env()
            with mock.patch.object(ship.release_record, "resolve_chain", side_effect=KeyError(support.APP_ID)):
                self.assertEqual(harness.run("production"), ship.EXIT_REFUSED)
            self.assertIn("refused: internal-error", harness.text())
            self.assertNotIn("Traceback", harness.text())
            self.assertEqual(support.scan_private(harness.text()), [])


if __name__ == "__main__":
    unittest.main()
