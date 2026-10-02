from __future__ import annotations

import copy
import datetime as dt
import json
import signal
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

    def assert_masked_before_use(self, values: tuple) -> None:
        lines = self.h.text().splitlines()
        for value in values:
            with self.subTest(value=value[:12]):
                mask = f"::add-mask::{value}"
                self.assertIn(mask, lines)
                index = lines.index(mask)
                self.assertEqual([line for line in lines[:index] if value in line], [])

    def test_identifiers_are_masked_and_the_token_is_private(self) -> None:
        outputs: list[common.Output] = []
        original = common.Output

        def recording(*args: object, **kwargs: object) -> common.Output:
            instance = original(*args, **kwargs)
            outputs.append(instance)
            return instance

        with mock.patch.object(common, "Output", side_effect=recording):
            self.assertEqual(self.production(), ship.EXIT_OK, self.h.text())
        self.assert_masked_before_use((
            support.APP_ID, support.PG_ID, support.CLUSTER_NAME, "rereply-fake.example.invalid", support.INGRESS,
            support.VPC_ID, support.ACTIVE_ID, support.NEW_DEPLOYMENT_IDS[0],
        ))
        self.assertEqual(len(outputs), 1)
        self.assertIn(support.DO_TOKEN, outputs[0].private)
        self.assertNotIn(f"::add-mask::{support.DO_TOKEN}", self.h.text())
        self.assert_sanitized()

    def test_failed_and_rollback_deployment_ids_are_masked(self) -> None:
        self.h.do = support.FakeDO(scenarios=["error", "accept"])
        self.assertEqual(self.production(), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assert_masked_before_use((support.ACTIVE_ID, support.NEW_DEPLOYMENT_IDS[0]))
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

    def settled_variant(self, change: object) -> int:
        """The first settled read returns a changed snapshot and the final
        equality check is relaxed, so each later post-deploy guard is the
        only thing between the change and a success."""
        original = do_app.observe_settled
        calls = {"n": 0}

        def settled(*args: object, **kwargs: object) -> do_app.Snapshot:
            snapshot = original(*args, **kwargs)
            calls["n"] += 1
            return change(snapshot) if calls["n"] == 1 else snapshot  # type: ignore[operator]

        with mock.patch.object(do_app, "observe_settled", side_effect=settled), \
                mock.patch.object(do_app, "require_materially_unchanged", return_value=None):
            return self.production()

    def assert_guard_rolled_back(self, code: int) -> None:
        self.assertEqual(code, ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertIn("post-deploy-guard: starting the automatic rollback", self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)
        self.assertEqual(self.h.do.put_bodies[1]["spec"], support.make_spec())
        self.assertEqual([client.put_count() for client in self.clients], [1, 1])
        self.assertEqual(self.h.outputs(), {})
        self.assert_sanitized()

    def test_post_deploy_guard_catches_wrong_settled_digests(self) -> None:
        self.assert_guard_rolled_back(self.settled_variant(
            lambda snap: snap._replace(spec=spec_images.set_images(snap.spec, support.OTHER))))

    def changed_fingerprint(self, key: str) -> None:
        self.assert_guard_rolled_back(self.settled_variant(
            lambda snap: snap._replace(public=dict(snap.public, **{key: "0" * 64}))))

    def test_post_deploy_guard_catches_a_changed_environment_fingerprint(self) -> None:
        self.changed_fingerprint("environment_values_sha256")

    def test_post_deploy_guard_catches_a_changed_non_source_fingerprint(self) -> None:
        self.changed_fingerprint("non_source_projection_sha256")

    def test_post_deploy_guard_catches_a_lost_vpc(self) -> None:
        def without_vpc(snap: do_app.Snapshot) -> do_app.Snapshot:
            spec = copy.deepcopy(snap.spec)
            del spec["vpc"]
            return snap._replace(spec=spec)

        self.assert_guard_rolled_back(self.settled_variant(without_vpc))

    def tampered_desired_spec(self, label: str, change: object) -> None:
        real_set_images = spec_images.set_images

        def tampered(spec: dict, images: dict) -> dict:
            desired = real_set_images(spec, images)
            change(desired)  # type: ignore[operator]
            return desired

        def image_only(before: dict, after: dict) -> list:
            return sorted(spec_images.image_digest_pointers(before))

        with mock.patch.object(spec_images, "set_images", side_effect=tampered), \
                mock.patch.object(spec_images, "require_image_only_change", side_effect=image_only):
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.h.text())
        self.assertIn(f"refused: topology-differs:{label}", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)

    def test_desired_spec_environment_fingerprint_is_checked_before_the_put(self) -> None:
        self.tampered_desired_spec("environment", lambda spec: spec["envs"][1].update(value="changed-value"))

    def test_desired_spec_non_source_fingerprint_is_checked_before_the_put(self) -> None:
        self.tampered_desired_spec("non-source", lambda spec: spec["services"][0].update(instance_count=5))

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

    def test_approval_gate_is_rechecked_before_any_provider_request(self) -> None:
        for label, mutate in {**support.APPROVAL_GATE_MUTATIONS, **support.APPROVAL_MUTATIONS}.items():
            with self.subTest(case=label):
                self.h.gh = support.FakeGh()
                self.h.gh.attest_legacy_images(support.LIVE, self.base)
                self.h.do = support.FakeDO()
                mutate(self.h.gh)
                self.h.stdout.truncate(0)
                self.h.stdout.seek(0)
                self.h.production_env()
                expected = "approval-missing" if label in support.APPROVAL_MUTATIONS else "approval-gate-misconfigured"
                self.refused(expected, requests=False)

    def test_approval_is_read_for_this_run(self) -> None:
        self.assertEqual(self.production(), ship.EXIT_OK, self.h.text())
        paths = [argv[-1] for argv, _ in self.h.gh.calls if argv[1:2] == ["api"]]
        self.assertIn(f"/repos/{common.REPOSITORY}/actions/runs/{support.RUN_ID}/approvals", paths)
        self.assertIn(f"/repos/{common.REPOSITORY}/environments/production", paths)
        self.assertFalse(any("pending_deployments" in " ".join(argv) for argv, _ in self.h.gh.calls))

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
                  if "attestation" in argv and f"https://github.com/{common.REPOSITORY}/.github/workflows/build-attest-exact-release-images.yml@refs/heads/main" in argv]
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

    def test_restore_refuses_when_live_equals_an_older_record(self) -> None:
        # A console rollback from the latest record to release #0, then a
        # restore dispatch: it must not re-deploy the release just rolled away.
        self.rollback_env(self.record["release"], support.hashlib.sha256(self.raw).hexdigest())
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED, self.h.text())
        self.assertIn("refused: drift:live-matches-record", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)
        self.assertEqual(self.h.outputs(), {})
        # The supported route: reconcile to the record that is live.
        self.h.stdout.truncate(0)
        self.h.stdout.seek(0)
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)
        self.assertEqual(self.h.outputs()["reconciled"], "true")

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

    def test_production_rechecks_the_schema_guard_for_rollback(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW))
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        blocked = ship.schema_change.SchemaChangeBlocked(["guarded-tree:internal/models/example.go"])
        with mock.patch.object(ship.schema_change, "guard", side_effect=blocked) as guard:
            self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED, self.h.text())
        self.assertEqual(guard.call_args.args[1:], (self.base, self.head))
        self.assertIn("schema-change-blocked", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)
        self.assertEqual(self.h.do.requests, [])

    def test_failed_rollback_put_is_itself_rolled_back(self) -> None:
        self.h.do = support.FakeDO(spec=support.make_spec(support.NEW), scenarios=["error", "accept"])
        self.rollback_env("prod-0000", self.h.bootstrap_sha256)
        self.assertEqual(self.h.run("production"), ship.EXIT_ROLLED_BACK, self.h.text())
        self.assertEqual(spec_images.extract_image_digests(self.h.do.spec), support.NEW)


class InterruptTests(ProductionCase):
    def test_cancel_after_the_put_is_manual(self) -> None:
        with mock.patch.object(do_app, "reconcile_until_active", side_effect=KeyboardInterrupt):
            self.assertEqual(self.production(), ship.EXIT_MANUAL, self.h.text())
        self.assertEqual(self.h.do.put_count(), 1)
        self.assertIn("cancelled after the PUT", self.h.text())
        self.assertIn(ship.MANUAL_LINE, self.h.text())
        self.assertEqual(self.h.outputs(), {})
        self.assertTrue(all(client._token == "" for client in self.clients))

    def test_cancel_before_the_put_is_refused(self) -> None:
        with mock.patch.object(ship.release_record, "resolve_chain", side_effect=KeyboardInterrupt):
            self.assertEqual(self.production(), ship.EXIT_REFUSED, self.h.text())
        self.assertIn("cancelled before the PUT; production unchanged", self.h.text())
        self.assertEqual(self.h.do.put_count(), 0)

    def test_sigterm_is_routed_to_the_handler_and_restored(self) -> None:
        before = signal.getsignal(signal.SIGTERM)
        seen: list[object] = []

        def check_handler(*args: object, **kwargs: object) -> object:
            seen.append(signal.getsignal(signal.SIGTERM))
            raise KeyboardInterrupt

        with mock.patch.object(ship.release_record, "resolve_chain", side_effect=check_handler):
            self.assertEqual(self.production(), ship.EXIT_REFUSED)
        self.assertEqual(seen, [ship._interrupt])
        self.assertIs(signal.getsignal(signal.SIGTERM), before)
        with self.assertRaises(KeyboardInterrupt):
            ship._interrupt(signal.SIGTERM, None)


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
