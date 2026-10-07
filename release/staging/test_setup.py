"""Synthetic staging/provider tests. These tests perform no network requests."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import setup

PG = "11111111-1111-4111-8111-111111111111"
VK = "22222222-2222-4222-8222-222222222222"
VPC = "33333333-3333-4333-8333-333333333333"
APP = "44444444-4444-4444-8444-444444444444"
DEPLOY = "55555555-5555-4555-8555-555555555555"
ORG = "66666666-6666-4666-8666-666666666666"
SOURCE = "a" * 40
SECRET = "synthetic-secret-must-never-reach-output"


class FakeRunner:
    root = setup.ROOT

    def __init__(self):
        self.calls, self.writes, self.apps, self.containers = [], [], [], []
        self.team = {"name": "ReReply Staging", "uuid": "opaque-staging-team-identity"}
        self.dirty = False
        self.head = SOURCE
        self.remote = SOURCE
        self.cluster_inventory = [
            {"id": PG, "name": "rereply-staging-pg", "engine": "pg", "version": "17", "status": "online", "region": "sgp1", "private_network_uuid": VPC},
            {"id": VK, "name": "rereply-staging-valkey", "engine": "valkey", "status": "online", "region": "sgp1", "private_network_uuid": VPC}]
        self.rules = {PG: [{"type": "ip_addr", "value": "198.51.100.8"}], VK: [{"type": "ip_addr", "value": "198.51.100.8"}]}
        self.spec = None
        self.fail_command = None
        self.deployment_response_missing = False
        self.health_ok = True

    def git(self, *args):
        if args[0] == "status": return " M changed" if self.dirty else ""
        if args == ("rev-parse", "HEAD"): return self.head
        if args == ("rev-parse", "origin/main"): return SOURCE
        return ""

    def run(self, args, *, input=None, timeout=180):
        self.calls.append(list(args))
        if args[0] == "gh" and args[1] == "api": return json.dumps({"sha": self.remote}).encode()
        if args[0] == "whoami": return b'"user","S-1-5-21-123-456-789-1001"'
        if args[0] == "docker": self.containers.append((args, input))
        if self.fail_command == args[0]: raise setup.Refused("command-failed-or-ambiguous")
        return b"[]"

    def do(self, config, *args, input=None):
        self.calls.append(list(args))
        if args == ("account", "get"): return [{"status": "active", "team": self.team}]
        if args == ("apps", "list"): return copy.deepcopy(self.apps)
        if args == ("databases", "list"): return copy.deepcopy(self.cluster_inventory)
        if args[:3] == ("databases", "firewalls", "list"): return copy.deepcopy(self.rules[args[3]])
        if args[:3] == ("databases", "firewalls", "replace"):
            self.writes.append(args)
            self.rules[args[3]] = [dict(zip(("type", "value"), rule.split(":", 1))) for rule in args[5].split(",")]
            assert self.rules[args[3]]
            return None
        if args[:2] == ("databases", "connection"):
            return [{"ssl": True, "host": "staging.invalid", "port": 25060, "uri": "rediss://default:" + SECRET + "@staging.invalid:25061"}]
        if args[:3] == ("databases", "user", "get"): return [{"name": args[4], "password": SECRET}]
        if args[:2] in (("apps", "create"), ("apps", "update")):
            self.writes.append(args)
            self.spec = json.loads(input)
            self.apps = [{"id": APP, "spec": self.spec}]
            result = {"id": APP}
            if not self.deployment_response_missing: result["pending_deployment"] = {"id": DEPLOY}
            return [result]
        if args[:2] == ("apps", "get"):
            return [{"id": APP, "spec": self.spec, "default_ingress": "https://staging-fixture.ondigitalocean.app",
                     "active_deployment": {"id": DEPLOY, "phase": "ACTIVE"}}]
        if args[:2] == ("apps", "get-deployment"):
            return [{"id": DEPLOY, "phase": "ACTIVE", "spec": self.spec,
                     "jobs": [{"name": "rereply-rls-migrate", "phase": "SUCCEEDED", "source_image_digest": "sha256:" + "b" * 64}]}]
        raise AssertionError("unexpected synthetic command")

    def health(self, origin):
        if not self.health_ok: raise setup.Refused("health-failed")


class SetupTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.config = self.base / "doctl.yaml"
        self.config.write_text('access-token: ""\nauth-contexts:\n  rereply-staging: synthetic-staging-token\ncurrent-context: rereply-staging\n')
        self.target = {"schema_version": 1, "team_sha256": setup.digest("opaque-staging-team-identity"), "postgres_id": PG, "valkey_id": VK, "vpc_id": VPC}
        self.images = {"source_sha": SOURCE, "graph-stub": "ghcr.io/medtechcorps-netizen/rereply-staging-graph-stub@sha256:" + "c" * 64,
                       "bootstrap": "ghcr.io/medtechcorps-netizen/rereply-staging-bootstrap@sha256:" + "d" * 64}
        self.runner = FakeRunner()
        self.kit = setup.Setup(self.runner, self.config, self.target, self.images, self.base / "rereply-staging" / "state.json")
        self.kit.latest = setup.release_record.Entry("prod-example", "promote", SOURCE, {key: "sha256:" + "b" * 64 for key in ("web", "meta-relay", "gmail-relay")}, "f" * 64, "", None, None, False, {})
        self.env = mock.patch.dict(os.environ, {}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def bootstrap(self):
        self.kit.preflight()
        self.kit.db("198.51.100.8")

    def assert_refused_without_writes(self, code, call=None):
        with self.assertRaisesRegex(setup.Refused, code):
            (call or self.kit.preflight)()
        self.assertEqual(self.runner.writes, [])
        self.assertEqual(self.runner.containers, [])

    def test_ambient_credentials_and_context_fail_before_reading_provider(self):
        for key in setup.AMBIENT:
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: ""}):
                self.assert_refused_without_writes("ambient-digitalocean")
        self.assertEqual(self.runner.calls, [])

    def test_other_context_and_global_token_are_refused(self):
        self.config.write_text("auth-contexts:\n  rereply-staging: token\n  production: another-token\ncurrent-context: rereply-staging\n")
        self.assert_refused_without_writes("staging-config-contexts")
        self.config.write_text("access-token: synthetic-token\nauth-contexts:\n  rereply-staging: token\ncurrent-context: rereply-staging\n")
        self.assert_refused_without_writes("global-token")

    def test_dirty_and_stale_main_are_refused(self):
        self.runner.dirty = True
        self.assert_refused_without_writes("checkout-dirty")
        self.runner.dirty = False
        self.runner.remote = "e" * 40
        self.assert_refused_without_writes("checkout-not-current-main")
        self.runner.remote = SOURCE
        self.runner.head = "e" * 40
        self.assert_refused_without_writes("checkout-not-main")

    def test_wrong_team_is_refused(self):
        self.runner.team["uuid"] = "wrong-team"
        self.assert_refused_without_writes("wrong-staging-team")

    def test_visible_production_app_or_cluster_is_refused(self):
        self.runner.apps = [{"id": APP}]
        self.kit.production["app_id_sha256"] = setup.digest(APP)
        self.assert_refused_without_writes("production-app-visible")
        self.runner.apps = []
        self.kit.production["postgres"]["cluster_name_sha256"] = setup.digest("rereply-staging-pg")
        self.assert_refused_without_writes("production-cluster-visible")

    def test_wrong_vpc_and_missing_cluster_are_refused(self):
        self.runner.cluster_inventory[0]["private_network_uuid"] = APP
        self.assert_refused_without_writes("staging-cluster-shape")
        self.runner.cluster_inventory = []
        self.assert_refused_without_writes("staging-cluster-missing")

    def test_operator_ip_must_be_in_both_nonempty_firewalls(self):
        self.kit.preflight()
        self.runner.rules[VK] = [{"type": "ip_addr", "value": "198.51.100.9"}]
        self.assert_refused_without_writes("add-operator-ip", lambda: self.kit.db("198.51.100.8"))
        self.runner.rules[VK] = []
        self.assert_refused_without_writes("trusted-sources-empty", lambda: self.kit.db("198.51.100.8"))

    def test_db_has_no_provider_writes_and_passes_secrets_only_on_stdin(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr): self.bootstrap()
        self.assertEqual(self.runner.writes, [])
        self.assertEqual(len(self.runner.containers), 2)
        for argv, config in self.runner.containers:
            self.assertNotIn(SECRET, str(argv))
            self.assertIn(SECRET.encode(), config)
            self.assertIn("/dev/stdin", argv)
        self.assertNotIn(SECRET, stdout.getvalue() + stderr.getvalue())
        self.assertTrue(self.kit.state["database_verified"])
        self.assertFalse(list(self.base.rglob("*.toml")))

    def test_app_finishes_with_exact_app_firewalls_never_empty(self):
        self.bootstrap()
        self.kit.app()
        for cluster in (PG, VK): self.assertEqual(self.runner.rules[cluster], [{"type": "app", "value": APP}])
        self.assertEqual(self.kit.state["origin_sha256"], setup.digest("https://staging-fixture.ondigitalocean.app"))
        self.assertEqual(self.kit.state["canary"]["stub_origin"], "https://staging-fixture.ondigitalocean.app/_stub")
        self.assertFalse(self.kit.state.get("pending_operation"))

    def test_missing_deployment_identity_stops_without_firewall_replacement(self):
        self.bootstrap()
        self.runner.deployment_response_missing = True
        with self.assertRaisesRegex(setup.Refused, "deployment-response-ambiguous"): self.kit.app()
        self.assertEqual(len(self.runner.writes), 1)
        self.assertEqual(self.kit.state["pending_operation"], "create-app")

    def test_failed_health_retains_operator_access_and_journal(self):
        self.bootstrap()
        self.runner.health_ok = False
        with self.assertRaisesRegex(setup.Refused, "health-failed"): self.kit.app()
        for rules in self.runner.rules.values():
            self.assertIn({"type": "ip_addr", "value": "198.51.100.8"}, rules)
            self.assertIn({"type": "app", "value": APP}, rules)
        self.assertEqual(self.kit.state["pending_operation"], "create-app")

    def test_redeploy_refuses_non_secret_environment_drift_before_write(self):
        self.bootstrap()
        self.kit.app()
        self.runner.writes.clear()
        self.runner.spec["services"][0]["envs"][0]["value"] = "production"
        with self.assertRaisesRegex(setup.Refused, "non-secret-env-drift"): self.kit.app(True)
        self.assertEqual(self.runner.writes, [])

    def test_record_and_template_keep_images_and_owner_credential_scoped(self):
        self.bootstrap()
        spec = self.kit.build_spec()
        for service in spec["services"]:
            self.assertNotIn("WHATOMATE_DATABASE__MIGRATION_URL", {e["key"] for e in service["envs"]})
            self.assertNotIn("registry_credentials", service["image"])
            self.assertFalse(service["image"]["deploy_on_push"]["enabled"])
            self.assertEqual(service["image"]["registry"], "ghcr.io")
            self.assertTrue(service["image"]["repository"].startswith("medtechcorps-netizen/rereply-"))
        self.assertEqual(spec["jobs"][0]["run_command"], "./rereply rls-migrate -config config.toml")
        self.assertEqual(spec["jobs"][0]["kind"], "PRE_DEPLOY")
        self.assertNotIn("domains", spec)

    def test_redeploy_enables_only_provisioned_organization(self):
        self.bootstrap()
        self.kit.app()
        self.kit.state["canary"]["klinik_organization_id"] = ORG
        # Simulate a newly issued deployment for the update.
        old_do = self.runner.do
        def with_old_deployment(config, *args, **kwargs):
            value = old_do(config, *args, **kwargs)
            if args[:2] == ("apps", "get") and len(self.runner.writes) == 5:
                value[0]["active_deployment"]["id"] = "77777777-7777-4777-8777-777777777777"
            if args[:2] == ("apps", "get-deployment"):
                value[0]["id"] = args[3]
            return value
        self.runner.do = with_old_deployment
        self.kit.app(True)
        self.assertEqual(self.kit.state["applied_canary_organization"], ORG)

    def test_pending_operation_is_never_retried(self):
        self.bootstrap()
        self.kit.checkpoint("create-app")
        with self.assertRaisesRegex(setup.Refused, "operation-needs-reconciliation"): self.kit.preflight()
        self.assertEqual(self.runner.writes, [])

    def test_private_state_cannot_live_in_another_checkout(self):
        other = self.base / "another-checkout"
        other.mkdir()
        (other / ".git").write_text("gitdir: synthetic\n")
        with self.assertRaisesRegex(setup.Refused, "private-path-in-checkout"):
            setup.private_path(other / "rereply-staging" / "state.json")

    def test_subprocess_failures_do_not_echo_private_output(self):
        runner = setup.Runner()
        completed = setup.subprocess.CompletedProcess([], 1, SECRET.encode(), SECRET.encode())
        with mock.patch.object(setup.subprocess, "run", return_value=completed):
            with self.assertRaisesRegex(setup.Refused, "command-failed-or-ambiguous") as caught:
                runner.run(["doctl", "anything"])
        self.assertNotIn(SECRET, str(caught.exception))

    def test_predeploy_success_must_name_the_migration_job(self):
        self.bootstrap()
        original = self.runner.do
        def bad(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] == ("apps", "get-deployment"):
                result[0]["jobs"] = [{"name": "other", "phase": "SUCCEEDED"}]
            return result
        self.runner.do = bad
        with self.assertRaises(setup.do_app.PostDeployGuard): self.kit.app()
        self.assertTrue(self.kit.state.get("pending_operation"))

    def test_provider_default_omission_and_secret_ciphertext_are_accepted(self):
        self.bootstrap()
        expected = self.kit.build_spec()
        actual = copy.deepcopy(expected)
        for rule in actual["ingress"]["rules"]:
            rule["component"].pop("preserve_path_prefix", None)
        for component in actual["services"] + actual["jobs"]:
            component["image"].pop("deploy_on_push")
            for env in component["envs"]:
                if env["type"] == "SECRET": env["value"] = "EV[1:synthetic-ciphertext]"
                if env["type"] == "GENERAL": env.pop("type")
                env.pop("scope")
        actual["services"].reverse()
        actual["databases"].reverse()
        self.kit.verify_spec(actual, expected)

    def test_adversarial_provider_topology_is_refused(self):
        self.bootstrap()
        expected = self.kit.build_spec()
        mutations = {
            "product-digest": lambda s: s["services"][0]["image"].update(digest="sha256:" + "e" * 64),
            "stub-repository": lambda s: s["services"][3]["image"].update(repository="other/stub"),
            "image-tag": lambda s: s["services"][0]["image"].update(tag="latest"),
            "auto-deploy": lambda s: s["services"][0]["image"].update(deploy_on_push={"enabled": True}),
            "registry-credential": lambda s: s["services"][0]["image"].update(registry_credentials=SECRET),
            "git-source": lambda s: s["services"][0].update(github={"repo": "other/source"}),
            "control-route": lambda s: s["ingress"]["rules"][0]["match"]["path"].update(prefix="/_stub"),
            "control-rewrite": lambda s: s["ingress"]["rules"][0]["component"].update(rewrite="/"),
            "redirect": lambda s: s["ingress"]["rules"][0].update(redirect={"authority": "other.invalid"}),
            "db-owner": lambda s: s["databases"][0].update(db_user="doadmin"),
            "db-cluster": lambda s: s["databases"][0].update(cluster_name="other-cluster"),
            "extra-service": lambda s: s["services"].append(dict(s["services"][0], name="extra")),
            "duplicate-service": lambda s: s["services"].append(copy.deepcopy(s["services"][0])),
            "duplicate-job": lambda s: s["jobs"].append(copy.deepcopy(s["jobs"][0])),
            "swapped-component-group": lambda s: s["jobs"].append(s["services"].pop(0)),
            "job-command": lambda s: s["jobs"][0].update(run_command="./rereply server"),
            "job-kind": lambda s: s["jobs"][0].update(kind="POST_DEPLOY"),
            "service-command": lambda s: s["services"][0].update(run_command="sleep infinity"),
            "health-path": lambda s: s["services"][0]["health_check"].update(http_path="/"),
            "internal-port": lambda s: s["services"][0].update(internal_ports=[9999]),
            "size": lambda s: s["services"][0].update(instance_count=2),
            "autoscale": lambda s: s["services"][0].update(autoscaling={"max_instance_count": 5}),
            "log-sink": lambda s: s["services"][0].update(log_destinations=[{"name": "external"}]),
            "domain": lambda s: s.update(domains=[{"domain": "other.invalid"}]),
            "app-env": lambda s: s.update(envs=[{"key": "WHATOMATE_APP__ENVIRONMENT", "value": "production"}]),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                actual = copy.deepcopy(expected)
                mutate(actual)
                with self.assertRaises(setup.Refused): self.kit.verify_spec(actual, expected)

    def test_active_deployment_drift_retains_operator_access(self):
        self.bootstrap()
        original = self.runner.do
        def drift(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] == ("apps", "get-deployment"):
                result = copy.deepcopy(result)
                result[0]["spec"]["services"][3]["image"]["digest"] = "sha256:" + "e" * 64
            return result
        self.runner.do = drift
        with self.assertRaisesRegex(setup.Refused, "staging-image-drift"): self.kit.app()
        for rules in self.runner.rules.values():
            self.assertIn({"type": "ip_addr", "value": "198.51.100.8"}, rules)
        self.assertEqual(len(self.runner.writes), 3)

    def test_redeploy_checks_actual_deployment_before_writing(self):
        self.bootstrap()
        self.kit.app()
        self.runner.writes.clear()
        original = self.runner.do
        def drift(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] == ("apps", "get-deployment"):
                result = copy.deepcopy(result)
                result[0]["spec"]["databases"][0]["cluster_name"] = "other"
            return result
        self.runner.do = drift
        with self.assertRaisesRegex(setup.Refused, "staging-database-drift"): self.kit.app(True)
        self.assertEqual(self.runner.writes, [])


@unittest.skipUnless(os.name == "nt", "Windows ACL behavior")
class WindowsPrivateStateTests(unittest.TestCase):
    def test_replaces_explicit_directory_grants_and_refuses_file_grants(self):
        # Synthetic temporary files only. Exercise real Windows ACLs rather than
        # asserting that an expected PowerShell command string was constructed.
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / "rereply-staging"
            directory.mkdir()
            runner = setup.Runner()
            runner.run(["icacls", str(directory), "/grant", "*S-1-1-0:(OI)(CI)(R)"])
            setup.private_directory(directory, runner)
            state = directory / "state.json"
            setup.save_private(state, {"synthetic": True})
            setup.check_private_state(state, runner)
            runner.run(["icacls", str(state), "/grant", "*S-1-1-0:(R)"])
            with self.assertRaises(setup.Refused): setup.check_private_state(state, runner)


if __name__ == "__main__":
    unittest.main()
