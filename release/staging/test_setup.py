"""Synthetic staging/provider tests. These tests perform no network requests."""
import contextlib
import copy
import io
import json
import os
import shutil
import subprocess
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
        self.config.write_text('access-token: ""\nauth-contexts:\n  rereply-staging: synthetic-staging-token\ncontext: rereply-staging\n')
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
        self.config.write_text("auth-contexts:\n  rereply-staging: token\n  production: another-token\ncontext: rereply-staging\n")
        self.assert_refused_without_writes("staging-config-contexts")
        self.config.write_text("access-token: synthetic-token\nauth-contexts:\n  rereply-staging: token\ncontext: rereply-staging\n")
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

    def test_successful_app_and_redeploy_print_hashes_without_private_identifiers(self):
        target = self.base / "target.json"
        images = self.base / "images.json"
        target.write_text(json.dumps(self.target))
        images.write_text(json.dumps(self.images))
        completed = mock.Mock(target=self.target, state={
            "app_id": APP, "deployment_id": DEPLOY, "secret": SECRET,
            "origin_sha256": setup.digest("https://synthetic.ondigitalocean.app"),
        })
        for command in ("app", "redeploy"):
            with self.subTest(command=command):
                stdout, stderr = io.StringIO(), io.StringIO()
                with mock.patch.object(Path, "home", return_value=self.base), mock.patch.object(setup, "Setup", return_value=completed), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    result = setup.main([command, "--target", str(target), "--images", str(images),
                                         "--private-file", str(self.kit.state_path), "--doctl-config", str(self.config)])
                self.assertEqual(result, 0)
                self.assertEqual(stderr.getvalue(), "")
                self.assertEqual(stdout.getvalue().splitlines(), [
                    "staging-setup: complete", "team_sha256=" + self.target["team_sha256"],
                    "app_id_sha256=" + setup.digest(APP), "origin_sha256=" + completed.state["origin_sha256"],
                ])
                for private in (APP, DEPLOY, PG, VK, VPC, SECRET):
                    self.assertNotIn(private, stdout.getvalue() + stderr.getvalue())

    def test_invalid_completed_app_identity_is_refused_before_success_output(self):
        target = self.base / "target.json"
        images = self.base / "images.json"
        target.write_text(json.dumps(self.target))
        images.write_text(json.dumps(self.images))
        completed = mock.Mock(target=self.target, state={"app_id": SECRET})
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(Path, "home", return_value=self.base), mock.patch.object(setup, "Setup", return_value=completed), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = setup.main(["app", "--target", str(target), "--images", str(images),
                                 "--private-file", str(self.kit.state_path), "--doctl-config", str(self.config)])
        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("completed-app-identity", stderr.getvalue())
        self.assertNotIn(SECRET, stderr.getvalue())

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

    def test_template_uses_default_lan_routes_without_duplicate_listener_ports(self):
        self.bootstrap()
        spec = self.kit.build_spec()
        services = {item["name"]: item for item in spec["services"]}
        self.assertEqual({name: item["http_port"] for name, item in services.items()},
                         {"omnitech-web": 8080, "meta-relay": 8081, "gmail-relay": 8082, "graph-stub": 8090})
        for item in services.values():
            self.assertEqual(item["internal_ports"], [])
            self.assertNotIn(item["http_port"], item["internal_ports"])
        envs = {item["name"]: {env["key"]: env["value"] for env in item["envs"]}
                for item in spec["services"] + spec["jobs"]}
        for name in ("omnitech-web", "rereply-rls-migrate"):
            self.assertEqual(envs[name]["WHATOMATE_WHATSAPP__BASE_URL"], "http://graph-stub")
        for key in ("META_RELAY_FACEBOOK_GRAPH_BASE_URL", "META_RELAY_INSTAGRAM_GRAPH_BASE_URL"):
            self.assertEqual(envs["meta-relay"][key], "http://graph-stub")
        self.assertEqual(envs["graph-stub"]["STUB_CALLBACK_ORIGIN"], "http://omnitech-web")
        for name, key, listen in (("meta-relay", "META_RELAY_LISTEN_ADDR", ":8081"),
                                  ("gmail-relay", "GMAIL_RELAY_LISTEN_ADDR", ":8082"),
                                  ("graph-stub", "STUB_LISTEN_ADDR", ":8090")):
            self.assertEqual(envs[name][key], listen)
        self.assertEqual([rule for rule in spec["ingress"]["rules"] if rule["component"]["name"] == "graph-stub"],
                         [{"match": {"path": {"prefix": "/_stub/_control"}},
                           "component": {"name": "graph-stub", "rewrite": "/_control"}}])
        control = next(env for env in services["graph-stub"]["envs"] if env["key"] == "STUB_CONTROL_KEY")
        self.assertEqual(control["type"], "SECRET")
        self.kit.verify_spec(spec, spec)

    def test_absent_additional_ports_complete_app_verification(self):
        self.bootstrap()
        original = self.runner.do
        def readback(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] in (("apps", "get"), ("apps", "get-deployment")):
                result = copy.deepcopy(result)
                for component in result[0]["spec"]["services"]:
                    del component["internal_ports"]
            return result
        self.runner.do = readback
        self.kit.app()
        self.assertFalse(self.kit.state.get("pending_operation"))
        self.assertTrue(self.kit.state.get("origin_sha256"))
        for rules in self.runner.rules.values():
            self.assertEqual(rules, [{"type": "app", "value": APP}])

    def test_nonempty_or_malformed_additional_ports_are_refused(self):
        self.bootstrap()
        expected = self.kit.build_spec()
        for index, service in enumerate(expected["services"]):
            for value in (None, False, 0, "", "[]", {}, [service["http_port"]], [9999]):
                actual = copy.deepcopy(expected)
                actual["services"][index]["internal_ports"] = value
                with self.subTest(service=service["name"], value=value), self.assertRaisesRegex(setup.Refused, "staging-service-drift"):
                    self.kit.verify_spec(actual, expected)
            # A template regression must not turn a duplicated main listener
            # into an accepted topology merely by copying it into expected.
            duplicate = copy.deepcopy(expected)
            duplicate["services"][index]["internal_ports"] = [service["http_port"]]
            with self.assertRaisesRegex(setup.Refused, "staging-service-drift"):
                self.kit.verify_spec(duplicate, duplicate)

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

    def test_doctl_omitted_empty_general_values_complete_app_verification(self):
        self.bootstrap()
        original = self.runner.do
        omitted = []
        def readback(config, *args, **kwargs):
            result = original(config, *args, **kwargs)
            if args[:2] in (("apps", "get"), ("apps", "get-deployment")):
                result = copy.deepcopy(result)
                for component in result[0]["spec"]["services"] + result[0]["spec"]["jobs"]:
                    # godo serializes an explicit false as an empty object too.
                    component["image"]["deploy_on_push"] = {}
                    for env in component["envs"]:
                        if env["type"] == "GENERAL" and env["value"] == "":
                            del env["value"]
                            omitted.append((component["name"], env["key"]))
                            if component["name"] == "rereply-rls-migrate": del env["type"]
                        elif env["type"] == "SECRET":
                            env["value"] = "EV[1:synthetic-ciphertext]"
            return result
        self.runner.do = readback
        self.kit.app()
        self.assertTrue(any(name == "omnitech-web" for name, _ in omitted))
        self.assertTrue(any(name == "rereply-rls-migrate" for name, _ in omitted))
        self.assertTrue(self.kit.state.get("origin_sha256"))
        for rules in self.runner.rules.values():
            self.assertEqual(rules, [{"type": "app", "value": APP}])

    def test_empty_general_equivalence_rejects_null_wrong_type_and_missing_nonempty(self):
        self.bootstrap()
        expected = self.kit.build_spec()
        for group in ("services", "jobs"):
            for omit_type in (False, True):
                for value in (None, False, 0, [], {}, "different"):
                    actual = copy.deepcopy(expected)
                    env = next(item for item in actual[group][0]["envs"] if item["type"] == "GENERAL" and item["value"] == "")
                    env["value"] = value
                    if omit_type: del env["type"]
                    with self.subTest(group=group, omit_type=omit_type, value=value), self.assertRaisesRegex(setup.Refused, "non-secret-env-drift"):
                        self.kit.verify_spec(actual, expected)
                actual = copy.deepcopy(expected)
                env = next(item for item in actual[group][0]["envs"] if item["type"] == "GENERAL" and item["value"])
                del env["value"]
                if omit_type: del env["type"]
                with self.subTest(group=group, missing_nonempty=True, omit_type=omit_type), self.assertRaisesRegex(setup.Refused, "non-secret-env-drift"):
                    self.kit.verify_spec(actual, expected)

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


# Representative output of doctl auth init/switch v1.164: all command defaults
# coexist with a root `context` selector. Every value here is synthetic.
DOCTL_DEFAULTS = """1-click:
  list:
    type: ""
access-token: ""
api-url: ""
apps:
  create:
    spec: ""
    wait: false
  logs:
    follow: false
auth:
  init:
    token-validation-server: https://cloud.digitalocean.com
auth-contexts:
    default: "true"
    rereply-staging: synthetic-staging-token
compute:
  droplet:
    create:
      enable-monitoring: false
      image: ""
context: rereply-staging
databases:
  connection:
    private: false
    user: doadmin
http-retry-max: 5
interactive: false
output: text
trace: false
verbose: false
"""


class ConfigNormalizationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name) / "rereply-staging"
        self.directory.mkdir(mode=0o700)
        self.config = self.directory / "doctl.yaml"
        self.config.write_text(DOCTL_DEFAULTS)
        self.config.chmod(0o600)
        self.runner = FakeRunner()
        self.team_hash = setup.digest(self.runner.team["uuid"])
        self.environment = mock.patch.dict(os.environ, {key: value for key, value in os.environ.items() if key not in setup.AMBIENT}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def normalize(self):
        setup.normalize_doctl_config(self.config, self.team_hash, self.runner)

    def assert_preserved(self, reason):
        original = self.config.read_bytes()
        with self.assertRaisesRegex(setup.Refused, reason): self.normalize()
        self.assertEqual(self.config.read_bytes(), original)
        self.assertEqual(list(self.directory.iterdir()), [self.config])
        self.assertEqual(self.runner.writes, [])
        self.assertEqual(self.runner.containers, [])

    def test_defaults_are_discarded_before_the_only_provider_read(self):
        with self.assertRaisesRegex(setup.Refused, "staging-config-shape"):
            setup.check_doctl_config(self.config)
        original = self.runner.do

        def account(config, *args, **kwargs):
            self.assertEqual(args, ("account", "get"))
            self.assertNotEqual(config, self.config)
            setup.check_doctl_config(config)
            self.assertNotIn("compute:", config.read_text())
            self.assertIn("api-url: https://api.digitalocean.com", config.read_text())
            self.assertEqual(self.config.read_text(), DOCTL_DEFAULTS)
            return original(config, *args, **kwargs)

        self.runner.do = account
        self.normalize()
        expected = ('access-token: ""\napi-url: https://api.digitalocean.com\nauth-contexts:\n'
                    '  rereply-staging: synthetic-staging-token\ncontext: rereply-staging\n')
        self.assertEqual(self.config.read_text(), expected)
        self.assertEqual([call for call in self.runner.calls if call[0] != "powershell.exe"], [["account", "get"]])
        self.assertEqual(list(self.directory.iterdir()), [self.config])
        self.runner.do = original
        self.normalize()  # Minimal output is stable and still identity-checked.
        self.assertEqual(self.config.read_text(), expected)

    def test_explicit_switch_and_real_context_key_are_required(self):
        for before, after, code in (
            ("context: rereply-staging", "context: default", "staging-config-contexts"),
            ("context: rereply-staging", "current-context: rereply-staging", "staging-config-context-key"),
            ("context: rereply-staging", "context: another-team", "staging-config-contexts"),
        ):
            with self.subTest(after=after):
                self.config.write_text(DOCTL_DEFAULTS.replace(before, after))
                self.assert_preserved(code)

    def test_ambiguous_or_foreign_authentication_is_rejected_without_account_read(self):
        for before, after, code in (
            ('access-token: ""', 'access-token: foreign-global-token', "staging-config-global-token"),
            ('api-url: ""', 'api-url: https://foreign.invalid', "staging-config-api"),
            ('api-url: ""', 'api-url: ""\napi-url: https://api.digitalocean.com', "staging-config-duplicate"),
            ('context: rereply-staging', 'context: rereply-staging\ncontext: default', "staging-config-duplicate"),
            ('    rereply-staging: synthetic-staging-token', '    rereply-staging: synthetic-staging-token\n    production: another-token', "staging-config-contexts"),
            ('    default: "true"', '    default: another-token', "staging-config-contexts"),
            ('    rereply-staging: synthetic-staging-token', '    rereply-staging: one\n    rereply-staging: two', "staging-config-duplicate"),
            ('    rereply-staging: synthetic-staging-token', '    rereply-staging: *alias', "staging-config-shape"),
            ('auth-contexts:', 'auth-contexts: {rereply-staging: synthetic}', "staging-config-shape"),
        ):
            with self.subTest(code=code, after=after):
                self.config.write_text(DOCTL_DEFAULTS.replace(before, after))
                self.assert_preserved(code)
        self.assertNotIn(["account", "get"], self.runner.calls)

    def test_every_ambient_override_is_rejected_before_provider_read(self):
        for key in setup.AMBIENT:
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: ""}):
                self.assert_preserved("ambient-digitalocean-credential")
        self.assertEqual(self.runner.calls, [])

    def test_foreign_token_team_leaves_original_unchanged(self):
        for field, value in (("uuid", "foreign-team"), ("name", "Production")):
            with self.subTest(field=field):
                original = self.runner.team.copy()
                self.runner.team[field] = value
                self.assert_preserved("wrong-staging-team")
                self.runner.team = original

    def test_concurrent_config_change_is_not_overwritten(self):
        original = self.runner.do
        replacement = DOCTL_DEFAULTS + "# user changed this during the account read\n"

        def changed(config, *args, **kwargs):
            self.config.write_text(replacement)
            return original(config, *args, **kwargs)

        self.runner.do = changed
        with self.assertRaisesRegex(setup.Refused, "staging-config-changed"): self.normalize()
        self.assertEqual(self.config.read_text(), replacement)
        self.assertEqual(list(self.directory.iterdir()), [self.config])

    def test_cli_success_and_failure_do_not_print_tokens_or_team_identifiers(self):
        for valid in (True, False):
            with self.subTest(valid=valid):
                self.config.write_text(DOCTL_DEFAULTS)
                self.runner.team["name"] = "ReReply Staging" if valid else "Production"
                out, err = io.StringIO(), io.StringIO()
                with mock.patch.object(setup, "Runner", return_value=self.runner), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                    result = setup.main(["normalize-config", "--doctl-config", str(self.config), "--team-sha256", self.team_hash])
                self.assertEqual(result, 0 if valid else 1)
                for private in ("synthetic-staging-token", self.runner.team["uuid"], str(self.config)):
                    self.assertNotIn(private, out.getvalue() + err.getvalue())
                if valid: self.assertEqual(out.getvalue(), "staging-config: normalized\n")
                else: self.assertIn("wrong-staging-team", err.getvalue())

    @unittest.skipUnless(shutil.which("doctl"), "optional real doctl configuration compatibility")
    def test_real_doctl_switch_output_and_normalized_context_selection(self):
        # auth switch/list are local-only in doctl; no API call uses this token.
        # Generate the full installed-doctl output instead of relying solely on
        # our small representative fixture. Never load the user's config.
        self.config.write_text('access-token: ""\nauth-contexts:\n  rereply-staging: synthetic-staging-token\ncontext: default\n')
        args = [shutil.which("doctl"), "--config", str(self.config)]
        subprocess.run([*args, "auth", "switch", "--context", "rereply-staging"], capture_output=True, check=True)
        expanded = self.config.read_text()
        self.assertIn("apps:", expanded)
        self.assertIn("context: rereply-staging", expanded)
        self.normalize()
        result = subprocess.run([*args, "auth", "list", "--output", "json"], capture_output=True, check=True)
        contexts = json.loads(result.stdout)
        selected = [item["name"] for item in contexts if item["current"]]
        self.assertEqual(selected, ["rereply-staging"])


@unittest.skipUnless(os.name == "nt", "Windows ACL behavior")
class WindowsPrivateStateTests(unittest.TestCase):
    def test_normalization_preserves_private_acl_and_refuses_extra_reader(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / "rereply-staging"
            runner = setup.Runner()
            setup.private_directory(directory, runner)
            config = directory / "doctl.yaml"
            config.write_text(DOCTL_DEFAULTS)
            team = {"name": "ReReply Staging", "uuid": "synthetic-staging-team"}
            with mock.patch.object(runner, "do", return_value=[{"status": "active", "team": team}]), mock.patch.dict(os.environ, {key: value for key, value in os.environ.items() if key not in setup.AMBIENT}, clear=True):
                setup.normalize_doctl_config(config, setup.digest(team["uuid"]), runner)
            setup.check_private_state(config, runner)
            before = config.read_bytes()
            runner.run(["icacls", str(config), "/grant", "*S-1-1-0:(R)"])
            with mock.patch.object(runner, "do") as provider, mock.patch.dict(os.environ, {key: value for key, value in os.environ.items() if key not in setup.AMBIENT}, clear=True):
                with self.assertRaises(setup.Refused): setup.normalize_doctl_config(config, setup.digest(team["uuid"]), runner)
                provider.assert_not_called()
            self.assertEqual(config.read_bytes(), before)

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
