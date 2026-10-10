"""Retained-app reset orchestration on synthetic files/provider responses only."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest import mock

import reset
import setup
from test_setup import FakeRunner, PG, VK, VPC, APP, DEPLOY, ORG, SOURCE, SECRET
from test_stage_fixture import fixture

RID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
NEW_ORG = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
ORIGIN = "https://staging-fixture.ondigitalocean.app"


def make_fixture(namespace, organization):
    value = fixture()
    raw = json.dumps(value).replace(value["canary"]["namespace"], namespace).replace(
        value["canary"]["origin"], ORIGIN).replace(value["canary"]["klinik_organization_id"], organization)
    value = json.loads(raw)
    value["origin_sha256"] = setup.digest(ORIGIN)
    return value["canary"]["fixture"]


def passing_report():
    return {"errors": [], "specs": [{"title": name, "tests": [{"expectedStatus": "passed", "status": "expected",
            "results": [{"status": "passed", "retry": 0}]}]} for name in reset.stage_report.CHECKS]}


class World(FakeRunner):
    def __init__(self, root):
        super().__init__()
        self.root = root
        self.current_deployment = DEPLOY
        self.frontend_calls = []
        self.idle = True
        self.ci_green = True
        self.frontend_failure = False
        self.report = passing_report()
        self.provider_hook = None

    def ci(self, head):
        setup.require(self.ci_green and head == SOURCE, "ci-not-green")

    def release_idle(self):
        setup.require(self.idle, "reset-release-active-or-unknown")

    def do(self, config, *args, input=None):
        if self.provider_hook:
            self.provider_hook(args)
        if args[:2] == ("apps", "update"):
            self.current_deployment = f"{len(self.writes) + 100:08d}-dddd-4ddd-8ddd-dddddddddddd"
            result = super().do(config, *args, input=input)
            result[0]["pending_deployment"]["id"] = self.current_deployment
            return result
        result = super().do(config, *args, input=input)
        if args[:2] == ("apps", "get"):
            result[0]["active_deployment"]["id"] = self.current_deployment
        if args[:2] == ("apps", "get-deployment"):
            result[0]["id"] = self.current_deployment
        return result

    def frontend(self, command, private_file, report=None):
        self.frontend_calls.append(command)
        if self.frontend_failure:
            raise setup.Refused("synthetic-private-error-" + SECRET)
        if command == "provision":
            state = setup.object_json(private_file.read_bytes())
            state["canary"].update(klinik_organization_id=NEW_ORG,
                                  fixture=make_fixture(state["canary"]["namespace"], NEW_ORG))
            setup.save_private(private_file, state)
        else:
            setup.save_private(report, self.report)


class ResetTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        root = base / "source"
        for name in ("release/staging/app-spec.template.yaml", "release/deployment/ship-target.json"):
            dest = root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(setup.ROOT / name, dest)
        pins = {"schema_version": 1, "profile": "staging", "app_id_sha256": setup.digest(APP),
                "team_uuid_sha256": setup.digest("opaque-staging-team-identity")}
        (root / "release/deployment/ship-target-staging.json").write_text(json.dumps(pins), encoding="utf-8")
        self.parent = base / "private" / "rereply-staging"
        self.parent.mkdir(parents=True)
        self.parent.chmod(0o700)
        self.config = self.parent / "doctl.yaml"
        self.config.write_text('access-token: ""\nauth-contexts:\n  rereply-staging: synthetic-token\ncontext: rereply-staging\n', encoding="utf-8")
        self.target = {"schema_version": 1, "team_sha256": pins["team_uuid_sha256"], "postgres_id": PG, "valkey_id": VK, "vpc_id": VPC}
        self.images = {"source_sha": SOURCE, "graph-stub": "ghcr.io/medtechcorps-netizen/rereply-staging-graph-stub@sha256:" + "c" * 64,
                       "bootstrap": "ghcr.io/medtechcorps-netizen/rereply-staging-bootstrap@sha256:" + "d" * 64}
        self.target_path, self.images_path = self.parent / "target.json", self.parent / "images.json"
        setup.save_private(self.target_path, self.target)
        setup.save_private(self.images_path, self.images)
        self.source = self.parent / "state.json"
        self.latest = setup.release_record.Entry("prod-example", "promote", SOURCE,
            {key: "sha256:" + "b" * 64 for key in ("web", "meta-relay", "gmail-relay")}, "f" * 64, "", None, None, False, {})
        private = {user: f"postgresql://{user}:{SECRET}@staging.invalid:25060/rereply?sslmode=require" for user in ("doadmin", "rereply_app")}
        namespace = "rereply-staging-old123"
        self.original = {"schema_version": 1, "target": self.target, "images": self.images, "product": self.latest._asdict(),
            "secrets": {key: SECRET for key in setup.SECRET_NAMES}, "database_private": private,
            "redis_url": "rediss://default:" + SECRET + "@staging.invalid:25061", "database_verified": True,
            "app_id": APP, "deployment_id": DEPLOY, "origin_sha256": setup.digest(ORIGIN), "applied_canary_organization": ORG,
            "canary": {"origin": ORIGIN, "stub_origin": ORIGIN + "/_stub", "namespace": namespace,
                "admin_email": "staging-admin@rereply.invalid", "admin_password": SECRET,
                "stub_app_id": "900000000000001", "stub_phone_id": "900000000000002", "stub_waba_id": "900000000000003",
                "stub_control_key": SECRET, "stub_access_token": SECRET, "stub_app_secret": SECRET,
                "klinik_organization_id": ORG, "fixture": make_fixture(namespace, ORG)}}
        setup.save_private(self.source, self.original)
        self.runner = World(root)
        self.runner.apps = [{"id": APP}]
        self.runner.rules = {key: [{"type": "app", "value": APP}] for key in (PG, VK)}
        self.acl = mock.patch.object(setup, "check_private_state")
        self.acl.start()
        self.addCleanup(self.acl.stop)
        self.env = mock.patch.dict(os.environ, {}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        latest = self.latest
        def verify(kit):
            kit.latest = latest
            setup.require(kit.state["product"]["sha"] == latest.sha and kit.state["product"]["images"] == latest.images,
                          "private-product-record-not-verified")
            return latest
        self.images_verifier = mock.patch.object(setup.Setup, "verify_images", verify)
        self.images_verifier.start()
        self.addCleanup(self.images_verifier.stop)
        self.no_network = mock.patch("socket.create_connection", side_effect=AssertionError("network forbidden"))
        self.no_network.start()
        self.addCleanup(self.no_network.stop)
        kit = self.kit()
        kit.clusters = {"postgres_id": self.runner.cluster_inventory[0], "valkey_id": self.runner.cluster_inventory[1]}
        kit.latest = self.latest
        self.runner.spec = kit.build_spec()
        self.original_spec = copy.deepcopy(self.runner.spec)

    def reset(self):
        return reset.Reset(self.runner, self.config, self.target_path, self.images_path, self.source, RID)

    def kit(self):
        return setup.Setup(self.runner, self.config, self.target, self.images, self.source)

    def execute(self, command, **options):
        operation = self.reset()
        operation.execute(command, **options)
        return operation

    def prepare(self):
        return self.execute("prepare", maintenance_window=True)

    def archived(self):
        self.runner.spec["maintenance"] = {"archive": True, "enabled": True}
        self.runner.rules = {key: [{"type": "ip_addr", "value": "198.51.100.8"}] for key in (PG, VK)}

    def db(self):
        return self.execute("db", operator_ip="198.51.100.8", quiesced=True, empty_database=True, owned_valkey_clean=True)

    def to_disabled(self):
        self.prepare()
        self.archived()
        self.db()
        self.runner.rules = {key: [{"type": "app", "value": APP}] for key in (PG, VK)}
        del self.runner.spec["maintenance"]
        return self.execute("redeploy")

    def to_enabled(self):
        self.to_disabled()
        self.execute("provision")
        return self.execute("redeploy")

    def test_complete_positive_flow_retains_app_secrets_and_old_allowlist_before_first_redeploy(self):
        operation = self.prepare()
        backup_bytes = operation.backup_path.read_bytes()
        working = setup.object_json(operation.working_path.read_bytes())
        self.assertEqual(working["applied_canary_organization"], ORG)
        self.assertNotIn("fixture", working["canary"])
        self.assertNotIn("klinik_organization_id", working["canary"])
        self.assertEqual(working["secrets"], self.original["secrets"])
        self.assertNotEqual(working["canary"]["namespace"], self.original["canary"]["namespace"])
        self.assertEqual(self.runner.writes, [])
        self.archived()
        self.db()
        self.assertEqual(len(self.runner.containers), 2)
        for argv, config in self.runner.containers:
            self.assertNotIn(SECRET, str(argv))
            self.assertIn(SECRET.encode(), config)
            self.assertIn("/dev/stdin", argv)
        self.runner.rules = {key: [{"type": "app", "value": APP}] for key in (PG, VK)}
        del self.runner.spec["maintenance"]
        self.execute("redeploy")
        self.assertIsNone(setup.object_json(operation.working_path.read_bytes())["applied_canary_organization"])
        self.execute("provision")
        self.execute("redeploy")
        self.execute("verify")
        self.execute("finalize")
        final = setup.object_json(self.source.read_bytes())
        for key in ("app_id", "origin_sha256", "secrets", "database_private", "redis_url", "product", "images", "target"):
            self.assertEqual(final[key], self.original[key], key)
        self.assertEqual(final["applied_canary_organization"], NEW_ORG)
        self.assertNotIn("pending_operation", final)
        self.assertEqual(operation.backup_path.read_bytes(), backup_bytes)
        journal = setup.object_json(operation.journal_path.read_bytes())
        self.assertEqual(journal["phase"], "complete")
        self.assertNotIn("pending", journal)
        self.assertEqual([entry["operation"] for entry in journal["completed"]],
            ["prepare", "bootstrap", "disable-old-allowlist", "provision", "enable-new-allowlist", "canary", "publish-canonical"])
        self.assertEqual(self.runner.frontend_calls, ["provision", "verify"])
        self.assertEqual(sum(args[:2] == ("apps", "update") for args in self.runner.writes), 2)
        self.assertTrue(all(args[:2] == ("apps", "update") or args[:3] == ("databases", "firewalls", "replace") for args in self.runner.writes))
        for rules in self.runner.rules.values(): self.assertEqual(rules, [{"type": "app", "value": APP}])
        reset.stage_fixture.export_fixture(final, self.kit().production["default_ingress_sha256"])

    def test_baseline_only_never_adopts_or_downgrades_candidate_images(self):
        self.runner.spec["services"][0]["image"]["digest"] = "sha256:" + "9" * 64
        with self.assertRaisesRegex(setup.Refused, "baseline-spec-drift"): self.prepare()
        self.assertEqual(self.runner.writes, [])
        self.assertFalse(self.reset().journal_path.exists())

    def test_no_owner_confirmation_no_journal_or_mutation(self):
        with self.assertRaisesRegex(setup.Refused, "maintenance-window"): self.execute("prepare")
        self.assertFalse(self.reset().journal_path.exists())
        self.prepare()
        self.archived()
        for key in ("quiesced", "empty_database", "owned_valkey_clean"):
            values = dict(operator_ip="198.51.100.8", quiesced=True, empty_database=True, owned_valkey_clean=True)
            values[key] = False
            with self.subTest(key=key), self.assertRaisesRegex(setup.Refused, "recreation-confirmation"):
                self.execute("db", **values)
        self.assertEqual(self.runner.writes + self.runner.containers, [])

    def test_pending_canonical_blocks_unchanged_setup_and_exports(self):
        self.prepare()
        with self.assertRaisesRegex(setup.Refused, "reconciliation"): self.kit().preflight()
        with self.assertRaises(reset.common.ReleaseError):
            reset.stage_fixture.export_fixture(setup.object_json(self.source.read_bytes()), self.kit().production["default_ingress_sha256"])

    def test_dirty_source_ci_release_activity_and_exact_pins_fail_before_write(self):
        for attr in ("dirty", "ci_green", "idle"):
            old = getattr(self.runner, attr)
            setattr(self.runner, attr, attr == "dirty")
            with self.subTest(attr=attr), self.assertRaises(Exception): self.prepare()
            self.assertFalse(self.reset().journal_path.exists())
            setattr(self.runner, attr, old)
        pins_path = self.runner.root / "release/deployment/ship-target-staging.json"
        pins = json.loads(pins_path.read_text())
        pins["app_id_sha256"] = "1" * 64
        pins_path.write_text(json.dumps(pins))
        with self.assertRaisesRegex(setup.Refused, "staging-pins"): self.prepare()
        self.assertEqual(self.runner.writes, [])

    def test_archive_only_exception_does_not_change_ordinary_spec_validator(self):
        self.prepare()
        self.runner.rules = {key: [{"type": "ip_addr", "value": "198.51.100.8"}] for key in (PG, VK)}
        with self.assertRaisesRegex(setup.Refused, "not-archived"): self.db()
        self.archived()
        for value in ({"enabled": True}, {"archive": 1}, {"archive": True, "enabled": False}, {"archive": True, "offline_page_url": "https://example.invalid"}):
            self.runner.spec["maintenance"] = value
            with self.subTest(value=value), self.assertRaisesRegex(setup.Refused, "not-archived"): self.db()
        self.runner.spec["maintenance"] = {"archive": True}
        with self.assertRaises(reset.common.ReleaseError): reset.contract.projection(self.runner.spec)
        self.db()

    def test_secret_or_material_drift_during_archive_refuses_bootstrap(self):
        self.prepare()
        self.archived()
        old = copy.deepcopy(self.runner.spec)
        secret = next(env for env in self.runner.spec["services"][0]["envs"] if env["type"] == "SECRET")
        secret["value"] += "-drift"
        with self.assertRaisesRegex(setup.Refused, "archived-material-drift"): self.db()
        self.runner.spec = old
        self.runner.spec["services"][0]["health_check"]["initial_delay_seconds"] = 16
        with self.assertRaisesRegex(setup.Refused, "baseline-spec-drift"): self.db()
        self.assertEqual(self.runner.containers, [])

    def test_operator_firewalls_and_credentials_are_exact(self):
        self.prepare()
        self.archived()
        self.runner.rules[PG].append({"type": "app", "value": APP})
        with self.assertRaisesRegex(setup.Refused, "operator-only"): self.db()
        self.runner.rules[PG].pop()
        original = self.runner.do
        def changed(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:3] == ("databases", "user", "get"): result[0]["password"] += "rotated"
            return result
        with mock.patch.object(self.runner, "do", side_effect=changed), self.assertRaisesRegex(setup.Refused, "credentials-changed"): self.db()
        self.assertEqual(self.runner.containers, [])

    def test_bootstrap_failure_is_journaled_before_call_and_cannot_retry(self):
        operation = self.prepare()
        self.archived()
        original = self.runner.run
        def failed(args, **kwargs):
            if args[0] == "docker":
                self.assertEqual(setup.object_json(operation.journal_path.read_bytes())["pending"]["operation"], "bootstrap")
                raise setup.Refused("private error " + SECRET)
            return original(args, **kwargs)
        with mock.patch.object(self.runner, "run", side_effect=failed), self.assertRaises(setup.Refused): self.db()
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.db()
        self.assertEqual(self.runner.containers, [])

    def test_provider_update_unknown_retains_pending_and_never_repeats(self):
        operation = self.prepare()
        self.archived(); self.db(); del self.runner.spec["maintenance"]
        self.runner.rules = {key: [{"type": "app", "value": APP}] for key in (PG, VK)}
        original = self.runner.do
        def failed(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] == ("apps", "update"):
                self.assertEqual(setup.object_json(operation.journal_path.read_bytes())["pending"]["operation"], "disable-old-allowlist")
                raise setup.Refused("ambiguous")
            return result
        with mock.patch.object(self.runner, "do", side_effect=failed), self.assertRaises(setup.Refused): self.execute("redeploy")
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("redeploy")
        self.assertEqual(sum(args[:2] == ("apps", "update") for args in self.runner.writes), 1)

    def test_restore_connectivity_must_be_app_only_before_first_update(self):
        self.prepare(); self.archived(); self.db()
        del self.runner.spec["maintenance"]
        with self.assertRaisesRegex(setup.Refused, "app-only-firewalls"): self.execute("redeploy")
        self.assertEqual(self.runner.writes, [])
        self.runner.rules[PG] = [{"type": "app", "value": APP}]
        with self.assertRaisesRegex(setup.Refused, "app-only-firewalls"): self.execute("redeploy")
        self.runner.rules[VK] = [{"type": "app", "value": APP}]
        self.execute("redeploy")

    def test_second_update_refuses_identical_foreign_deployment(self):
        self.to_disabled(); self.execute("provision")
        before = len(self.runner.writes)
        self.runner.current_deployment = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
        with self.assertRaisesRegex(setup.Refused, "before-redeploy-material-drift"): self.execute("redeploy")
        self.assertEqual(len(self.runner.writes), before)

    def test_private_secret_drift_after_setup_before_readbacks_refuses_actual_put(self):
        self.to_disabled(); self.execute("provision")
        before = len(self.runner.writes)
        # Setup.app's pending checkpoint happens after its ordinary before
        # readbacks. Inject drift at this boundary, before the actual PUT.
        original = setup.Setup.checkpoint
        def changed(kit, operation):
            original(kit, operation)
            env = next(e for e in self.runner.spec["services"][0]["envs"] if e["type"] == "SECRET")
            env["value"] += "foreign-secret"
        with mock.patch.object(setup.Setup, "checkpoint", changed), self.assertRaisesRegex(setup.Refused, "update-observation-changed"):
            self.execute("redeploy")
        self.assertEqual(len(self.runner.writes), before)
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("redeploy")

    def test_input_working_canonical_and_backup_drift_prevent_next_phase(self):
        operation = self.prepare()
        self.archived()
        for path in (self.config, operation.working_path, self.source, operation.backup_path):
            raw = path.read_bytes()
            if path == self.config: path.write_bytes(raw + b"# drift\n")
            else:
                value = setup.object_json(raw); value["unexpected"] = True; setup.save_private(path, value)
            with self.subTest(path=path.name), self.assertRaises(Exception): self.db()
            path.write_bytes(raw)
        self.assertEqual(self.runner.containers, [])

    def test_same_id_is_create_only_and_local_lock_does_not_get_overwritten(self):
        operation = self.prepare()
        raw = operation.journal_path.read_bytes()
        with self.assertRaisesRegex(setup.Refused, "already-consumed"): self.prepare()
        self.assertEqual(operation.journal_path.read_bytes(), raw)
        operation.lock_path.write_text("another operator")
        with self.assertRaises(FileExistsError): self.db()
        self.assertEqual(operation.lock_path.read_text(), "another operator")

    def test_provision_failure_or_extra_state_mutation_never_replays(self):
        operation = self.to_disabled()
        self.runner.frontend_failure = True
        with self.assertRaises(setup.Refused): self.execute("provision")
        self.runner.frontend_failure = False
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("provision")
        self.assertEqual(self.runner.frontend_calls, ["provision"])
        self.assertEqual(setup.object_json(operation.journal_path.read_bytes())["pending"]["operation"], "provision")

    def test_provision_cannot_change_credentials(self):
        self.to_disabled()
        original = self.runner.frontend
        def poisoned(command, path, report=None):
            original(command, path, report)
            value = setup.object_json(path.read_bytes())
            value["secrets"]["jwt"] += "unreviewed"
            setup.save_private(path, value)
        with mock.patch.object(self.runner, "frontend", side_effect=poisoned), self.assertRaisesRegex(setup.Refused, "provision-state-drift"):
            self.execute("provision")
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("provision")

    def test_provision_cannot_reuse_old_tenant_even_with_well_formed_fixture(self):
        self.to_disabled()
        def old_tenant(command, path, report=None):
            value = setup.object_json(path.read_bytes())
            value["canary"].update(klinik_organization_id=ORG, fixture=make_fixture(value["canary"]["namespace"], ORG))
            setup.save_private(path, value)
        with mock.patch.object(self.runner, "frontend", side_effect=old_tenant), self.assertRaisesRegex(setup.Refused, "old-organization"):
            self.execute("provision")

    def test_source_change_during_canary_cannot_publish_a_bound_success(self):
        operation = self.to_enabled()
        original = self.runner.frontend
        def changed(command, path, report=None):
            original(command, path, report)
            self.runner.remote = "e" * 40
        with mock.patch.object(self.runner, "frontend", side_effect=changed), self.assertRaisesRegex(setup.Refused, "source-head-changed"):
            self.execute("verify")
        self.assertEqual(setup.object_json(operation.journal_path.read_bytes())["pending"]["operation"], "canary")

    def test_failed_post_canary_health_stops_without_extra_writes(self):
        operation = self.to_enabled()
        count = len(self.runner.writes)
        self.runner.health_ok = False
        with self.assertRaisesRegex(setup.Refused, "health-failed"): self.execute("verify")
        self.assertEqual(len(self.runner.writes), count)
        self.assertEqual(setup.object_json(operation.journal_path.read_bytes())["pending"]["operation"], "canary")

    def test_different_full_specs_between_observation_reads_refuse_before_prepare(self):
        calls = 0
        def drift(args):
            nonlocal calls
            if args[:2] == ("apps", "get"):
                calls += 1
                if calls == 2:
                    env = next(e for e in self.runner.spec["services"][0]["envs"] if e["type"] == "SECRET")
                    env["value"] += "second-read-change"
        self.runner.provider_hook = drift
        with self.assertRaisesRegex(setup.Refused, "observation-changed"): self.prepare()
        self.assertFalse(self.reset().journal_path.exists())
        self.assertEqual(self.runner.writes, [])

    def test_state_bool_schema_and_partial_fixture_are_refused_before_preparation(self):
        state = copy.deepcopy(self.original)
        state["schema_version"] = True
        setup.save_private(self.source, state)
        with self.assertRaisesRegex(setup.Refused, "state-not-ready"): self.prepare()
        state["schema_version"] = 1
        state["canary"].pop("fixture")
        setup.save_private(self.source, state)
        with self.assertRaises(reset.common.ReleaseError): self.prepare()
        self.assertFalse(self.reset().journal_path.exists())

    def test_report_must_be_new_and_exact_single_pass_and_retry_zero(self):
        operation = self.to_enabled()
        setup.save_private(operation.report_path, passing_report())
        with self.assertRaisesRegex(setup.Refused, "already-exists"): self.execute("verify")
        operation.report_path.unlink()
        self.runner.report["specs"][0]["tests"][0]["results"][0]["retry"] = 1
        with self.assertRaises(reset.common.ReleaseError): self.execute("verify")
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("verify")
        self.assertEqual(self.runner.frontend_calls, ["provision", "verify"])

    def test_verified_report_and_fixture_are_bound_before_final_publication(self):
        operation = self.to_enabled()
        self.execute("verify")
        journal = setup.object_json(operation.journal_path.read_bytes())
        bound = journal["completed"][-1]["payload"]
        working = setup.object_json(operation.working_path.read_bytes())
        self.assertEqual(bound["source_sha"], SOURCE)
        self.assertEqual(bound["namespace"], working["canary"]["namespace"])
        self.assertEqual(bound["fixture_sha256"], reset.fingerprint(reset.stage_fixture.export_fixture(working, self.kit().production["default_ingress_sha256"])))
        report = passing_report(); report["specs"].reverse(); setup.save_private(operation.report_path, report)
        with self.assertRaisesRegex(setup.Refused, "report-changed"): self.execute("finalize")
        self.assertEqual(setup.object_json(self.source.read_bytes())["pending_operation"], "reset:" + RID)

    def test_final_publication_ambiguity_keeps_exact_private_payload_and_no_repeat(self):
        operation = self.to_enabled()
        self.execute("verify")
        original = setup.save_private
        def ambiguous(path, value):
            original(path, value)
            if path == self.source: raise OSError("unknown commit")
        with mock.patch.object(setup, "save_private", side_effect=ambiguous), self.assertRaises(OSError): self.execute("finalize")
        journal = setup.object_json(operation.journal_path.read_bytes())
        self.assertEqual(journal["pending"]["payload"]["state"], setup.object_json(self.source.read_bytes()))
        with self.assertRaisesRegex(setup.Refused, "consumed-or-pending"): self.execute("finalize")

    def test_foreign_spec_secret_drift_after_bootstrap_and_new_deployment_are_refused(self):
        self.to_enabled()
        self.runner.current_deployment = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
        with self.assertRaisesRegex(setup.Refused, "verify-app-drift"): self.execute("verify")
        self.assertEqual(self.runner.frontend_calls, ["provision"])

    def test_main_never_emits_private_exception_text(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        args = ["prepare", "--doctl-config", str(self.config), "--target", str(self.target_path), "--images", str(self.images_path),
                "--private-file", str(self.source), "--reset-id", RID, "--confirm-maintenance-window"]
        with mock.patch.object(reset.Reset, "execute", side_effect=RuntimeError(SECRET)), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(reset.main(args), 1)
        self.assertNotIn(SECRET, stdout.getvalue() + stderr.getvalue())
        self.assertIn("retain the private journal", stderr.getvalue())


class RunnerBoundaryTests(unittest.TestCase):
    def test_release_idle_is_five_bounded_exact_read_only_routes_and_rejects_partial_shapes(self):
        runner = reset.Runner()
        with mock.patch.object(runner, "run", return_value=b'{"total_count":0,"workflow_runs":[]}') as run:
            runner.release_idle()
        self.assertEqual(run.call_count, 5)
        for call, status in zip(run.call_args_list, reset.ACTIVE_STATUSES):
            self.assertEqual(call.args[0], ["gh", "api", "--hostname", "github.com",
                f"repos/{setup.REPOSITORY}/actions/workflows/ship.yml/runs?status={status}&per_page=1"])
        for value in ({"total_count": True, "workflow_runs": []}, {"total_count": 1, "workflow_runs": []},
                      {"total_count": 0}, {"total_count": 0, "workflow_runs": [{}]}):
            with self.subTest(value=value), mock.patch.object(runner, "run", return_value=json.dumps(value).encode()), self.assertRaises(setup.Refused):
                runner.release_idle()

    def test_frontend_uses_existing_commands_explicit_profile_and_never_echoes_child_failures(self):
        runner = reset.Runner()
        state, report = Path("synthetic-state.json"), Path("synthetic-report.json")
        completed = mock.Mock(returncode=0, stdout=b"private", stderr=b"")
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(reset.subprocess, "run", return_value=completed) as run:
            runner.frontend("provision", state)
            runner.frontend("verify", state, report)
        first, second = run.call_args_list
        self.assertEqual(first.args[0], ["node", "--experimental-strip-types", "e2e/canary/provision.ts"])
        self.assertEqual(second.kwargs["env"], {"CANARY_PROFILE": "staging", "CANARY_PRIVATE_FILE": str(state),
                                             "CANARY_DRILL": "none", "CANARY_REPORT_FILE": str(report)})
        self.assertEqual(second.kwargs["cwd"], setup.ROOT / "frontend")
        self.assertEqual(second.kwargs["timeout"], 1200)
        for name in ("CANARY_ORIGIN", "CANARY_STRESS", "NODE_OPTIONS", "NODE_TLS_REJECT_UNAUTHORIZED", "https_proxy",
                     "STAGING_DO_TOKEN", "WHATOMATE_APP__ENVIRONMENT"):
            with self.subTest(name=name), mock.patch.dict(os.environ, {name: ""}, clear=True), mock.patch.object(reset.subprocess, "run") as run, self.assertRaises(setup.Refused):
                runner.frontend("verify", state, report)
            run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
