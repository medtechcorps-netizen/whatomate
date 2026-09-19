"""Offline bootstrap tests: real policy/rehydration, mocked external transports."""
from __future__ import annotations

import base64
import copy
import datetime as dt
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import zipfile

try:
    from . import bootstrap_production_crm_canary_driver as boot
    from . import test_provision_production_crm_canary_fixture as samples
except ImportError:
    import bootstrap_production_crm_canary_driver as boot
    import test_provision_production_crm_canary_fixture as samples

common = boot.common
NOW = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
uid = samples.uid


def metadata():
    return {"environment": {"id": 37, "name": boot.ENVIRONMENT, "protection_rules": [],
                            "deployment_branch_policy": {"protected_branches": False, "custom_branch_policies": True}},
            "branch_policies": [{"id": 43, "name": "main", "type": "branch"}], "variables": [],
            "secrets": [{"name": name, "created_at": "2026-08-01T00:00:00Z", "updated_at": "2026-08-02T00:00:00Z"}
                        for name in ("CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY")]}


def packets():
    request, protected = samples.inputs()
    transport = samples.FakeTransport(protected)
    driver, receipt = boot.fixture.ProductProvisioner(request, protected, transport, samples.FakeGate()).provision()
    terminal = samples.terminal_receipt(receipt, protected)
    terminal_bytes = common.canonical_file_bytes(terminal)
    rules = [{"type": "app", "value": uid(10)}]
    plan = {"provider_account_uuid": uid(11), "app_name": "rereply-canary-driver-test", "region": "sgp",
            "instance_size_slug": "apps-s-1vcpu-1gb", "instance_count": 1, "monthly_usd": "12.00",
            "github_environment_sha256": common.sha256_value(metadata()),
            "ledger": {"cluster_id": uid(12), "cluster_name": "rereply-canary-ledger", "database": "canary_ledger",
                       "user": "crm_canary_driver", "version": "16", "firewall_sha256": common.sha256_value(rules),
                       "dedicated_synthetic_ledger": True}}
    a = {"schema_version": 1, "kind": "production-crm-canary-driver-bootstrap-v1", "control_sha": "c" * 40,
         "operation_id": uid(13), "issued_at": (NOW - dt.timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ"),
         "expires_at": (NOW + dt.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ"),
         "plan_sha256": common.sha256_value(plan), "protected_descriptor_sha256": "0" * 64,
         "fixture_evidence": {"control_sha": request["control_sha"], "run_id": "12345", "artifact_id": "91",
                              "artifact_digest": "sha256:" + "a" * 64, "result_sha256": common.sha256_bytes(terminal_bytes)},
         "driver_evidence": {"control_sha": "b" * 40, "run_id": "12346", "artifact_id": "92",
                             "artifact_digest": "sha256:" + "d" * 64, "digest": "sha256:" + "e" * 64,
                             "driver_version_sha256": "f" * 64}, "effects": copy.deepcopy(boot.EFFECTS)}
    d = {"schema_version": 1, "control_sha": a["control_sha"], "operation_id": a["operation_id"],
         "public_packet_sha256": boot.public_packet_hash(a), "fixture_descriptor_sha256": common.sha256_value(driver),
         "hmac_key_base64": base64.b64encode(b"h" * 32).decode(), "scope_review": copy.deepcopy(boot.SCOPE_REVIEW), "plan": plan}
    a["protected_descriptor_sha256"] = common.sha256_value(d)
    return a, d, protected, terminal_bytes, transport, driver


def rebind(a, d):
    a["plan_sha256"] = common.sha256_value(d["plan"])
    d["public_packet_sha256"] = boot.public_packet_hash(a)
    a["protected_descriptor_sha256"] = common.sha256_value(d)


class FakeProvider:
    def __init__(self, plan):
        self.plan = plan
        self.app_id, self.deployment_id = uid(14), uid(15)
        self.calls = []
        self.created = False
        self.create_count = 0
        self.spec = None
        self.change = None
        self.phase = "ACTIVE"

    def preflight(self):
        self.calls.append("preflight")
        return [{"id": uid(10), "name": "production-app"}], [{"type": "app", "value": uid(10)}]

    def create(self, spec):
        self.calls.append("create")
        self.create_count += 1
        self.created = True
        self.spec = copy.deepcopy(spec)
        for env in self.spec["services"][0]["envs"]:
            if env["type"] == "SECRET":
                env["value"] = "EV[synthetic-ciphertext-" + env["key"] + "]"
        return self.app_id, self.deployment_id, {"spec": copy.deepcopy(self.spec)}

    def apps(self):
        return sorted([{"id": uid(10), "name": "production-app"},
                       {"id": self.app_id, "name": self.plan["app_name"]}], key=lambda a: a["id"])

    def get(self, path):
        self.calls.append(path)
        if path.endswith("/firewall"):
            value = {"rules": [{"type": "app", "value": uid(10)}, {"type": "app", "value": self.app_id}]}
        elif "/deployments/" in path:
            value = {"deployment": {"id": self.deployment_id, "phase": self.phase, "spec": copy.deepcopy(self.spec)}}
        else:
            host = self.plan["app_name"] + "-abc.ondigitalocean.app"
            value = {"app": {"id": self.app_id, "owner_uuid": self.plan["provider_account_uuid"],
                             "spec": copy.deepcopy(self.spec), "active_deployment": {"id": self.deployment_id},
                             "default_ingress": "https://" + host, "live_url": "https://" + host, "live_domain": host}}
        if self.change:
            self.change(path, value)
        return value


class FakeWriter:
    def __init__(self):
        self.installed = []
        self.calls = []

    def require_absent(self):
        self.calls.append("absent")

    def install(self, config):
        self.calls.append("install")
        self.installed.append(copy.deepcopy(config))


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.a, self.d, self.protected, self.receipt, self.transport, self.driver = packets()
        self.provider = FakeProvider(self.d["plan"])
        self.writer = FakeWriter()
        self.probe = mock.Mock()
        self.current = mock.Mock()
        self.authenticate = mock.Mock(return_value=self.receipt)
        self.image = mock.Mock()

    def execute(self):
        return boot.install_once(self.a, self.d, self.protected, provider=self.provider, writer=self.writer,
            authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
            rehydrate=boot.fixture.rehydrate, transport=self.transport, sleep=lambda _: None,
            probe=self.probe, now=lambda: NOW)

    def test_complete_positive_runs_real_readonly_rehydrate_and_no_synthetic_execution(self):
        before = len(self.transport.calls)
        result = self.execute()
        self.assertTrue(all(call[0] == "GET" for call in self.transport.calls[before:]))
        self.assertEqual(self.provider.create_count, 1)
        self.assertEqual(len(self.writer.installed), 1)
        self.assertEqual(self.probe.call_count, 2)
        self.assertEqual(self.current.call_count, 6)
        self.assertEqual(result["synthetic_execution"], "not-performed")
        public = common.canonical_payload_bytes(result).decode()
        for value in (self.provider.app_id, self.provider.deployment_id, self.d["hmac_key_base64"],
                      self.protected["credentials"]["klinik_password"], self.d["plan"]["ledger"]["cluster_id"]):
            self.assertNotIn(value, public)

    def test_public_packet_contains_no_private_resource_plan(self):
        raw = common.canonical_payload_bytes(self.a).decode()
        for value in ("crm_canary_driver", "canary_ledger", self.d["plan"]["provider_account_uuid"], self.d["hmac_key_base64"]):
            self.assertNotIn(value, raw)
        boot.validate_descriptor(self.d, self.a)

    def test_exact_packet_schema_and_binding_reject_every_extra_key(self):
        for candidate, validate in ((self.a, lambda v: boot.validate_authorization(v, control_sha=self.a["control_sha"], now=NOW)),
                                    (self.d, lambda v: boot.validate_descriptor(v, self.a))):
            with self.subTest(candidate=set(candidate)), self.assertRaises(common.ReleaseError):
                validate({**candidate, "unexpected": True})
        for key in self.a:
            with self.subTest(missing=key), self.assertRaises(common.ReleaseError):
                boot.validate_authorization({k: v for k, v in self.a.items() if k != key}, control_sha=self.a["control_sha"], now=NOW)

    def test_rebound_unreviewed_plan_and_scope_variations_rejected(self):
        variations = (
            lambda d: d["plan"].update(instance_count=2),
            lambda d: d["plan"].update(instance_count=True),
            lambda d: d["plan"].update(app_name="production-app"),
            lambda d: d["plan"]["ledger"].update(user="doadmin"),
            lambda d: d["plan"]["ledger"].update(dedicated_synthetic_ledger=False),
            lambda d: d["scope_review"]["read"].append("database:view_credentials"),
            lambda d: d["scope_review"]["create"].append("database:view_credentials"),
            lambda d: d.update(hmac_key_base64=base64.b64encode(b"x" * 31).decode()),
        )
        for variation in variations:
            a, d = copy.deepcopy(self.a), copy.deepcopy(self.d)
            variation(d); rebind(a, d)
            with self.subTest(variation=variation), self.assertRaises(common.ReleaseError):
                boot.validate_descriptor(d, a)

    def test_overlength_app_name_rejected_before_provider(self):
        self.d["plan"]["app_name"] = "rereply-canary-driver-" + "x" * 20
        rebind(self.a, self.d)
        with self.assertRaises(common.ReleaseError):
            self.execute()
        self.assertEqual(self.provider.calls, [])
        self.authenticate.assert_not_called()

    def test_trailing_hyphen_app_name_rejected_before_provider(self):
        self.d["plan"]["app_name"] = "rereply-canary-driver-test-"
        rebind(self.a, self.d)
        with self.assertRaises(common.ReleaseError):
            self.execute()
        self.assertEqual(self.provider.calls, [])
        self.authenticate.assert_not_called()

    def test_missing_or_bad_receipt_signature_stops_before_login_and_provider(self):
        self.authenticate.side_effect = common.ReleaseError("signature rejected")
        before = len(self.transport.calls)
        with self.assertRaisesRegex(common.ReleaseError, "signature rejected"):
            self.execute()
        self.assertEqual(len(self.transport.calls), before)
        self.assertEqual(self.provider.calls, [])

    def test_bad_image_evidence_stops_before_provider_or_rehydration(self):
        self.image.side_effect = common.ReleaseError("image rejected")
        before = len(self.transport.calls)
        with self.assertRaisesRegex(common.ReleaseError, "image rejected"):
            self.execute()
        self.assertEqual(len(self.transport.calls), before)
        self.assertFalse(self.provider.created)

    def test_authority_change_immediately_before_create_stops_all_mutations(self):
        self.current.side_effect = [None, None, common.ReleaseError("main moved")]
        with self.assertRaisesRegex(common.ReleaseError, "main moved"):
            self.execute()
        self.assertFalse(self.provider.created)
        self.assertEqual(self.writer.installed, [])

    def test_authority_change_after_health_stops_secret_install(self):
        self.current.side_effect = [None, None, None, None, common.ReleaseError("main moved")]
        with self.assertRaisesRegex(common.ReleaseError, "main moved"):
            self.execute()
        self.assertEqual(self.provider.create_count, 1)
        self.assertEqual(self.writer.installed, [])

    def test_ambiguous_create_is_not_retried(self):
        with mock.patch.object(self.provider, "create", side_effect=TimeoutError("synthetic secret must not print")) as create:
            with self.assertRaises(TimeoutError):
                self.execute()
        self.assertEqual(create.call_count, 1)
        self.assertEqual(self.writer.installed, [])

    def test_terminal_failed_deployment_is_not_redeployed(self):
        self.provider.phase = "ERROR"
        with self.assertRaisesRegex(common.ReleaseError, "progress safely"):
            self.execute()
        self.assertEqual(self.provider.create_count, 1)
        self.assertEqual(self.writer.installed, [])

    def test_created_app_drift_or_missing_admission_stops_secret_install(self):
        changes = (
            lambda path, v: v["rules"].pop() if "rules" in v else None,
            lambda path, v: v["rules"].append({"type": "ip_addr", "value": "0.0.0.0/0"}) if "rules" in v else None,
            lambda path, v: v["app"].update(owner_uuid=uid(99)) if "app" in v else None,
            lambda path, v: v["app"].update(in_progress_deployment={"id": uid(99), "phase": "DEPLOYING"}) if "app" in v else None,
            lambda path, v: v["app"].update(default_ingress="http://bad.example") if "app" in v else None,
            lambda path, v: v["deployment"]["spec"]["services"][0]["image"].update(digest="sha256:" + "0" * 64) if "deployment" in v else None,
        )
        for change in changes:
            self.provider = FakeProvider(self.d["plan"])
            self.provider.change = change
            with self.subTest(change=change), self.assertRaises(common.ReleaseError):
                self.execute()
            self.assertEqual(self.writer.installed, [])

    def test_runtime_spec_has_only_five_secret_values_and_literal_managed_binding(self):
        spec = boot.runtime_spec(self.a, self.d, self.protected, self.driver)
        env = {item["key"]: item for item in spec["services"][0]["envs"]}
        self.assertEqual(sum(item["type"] == "SECRET" for item in env.values()), 5)
        self.assertEqual(env["CRM_CANARY_LEDGER_DATABASE_URL"]["type"], "GENERAL")
        self.assertEqual(env["CRM_CANARY_LEDGER_DATABASE_URL"]["value"], boot.LEDGER_EXPRESSION)
        self.assertNotIn("super_admin_login", common.canonical_payload_bytes(spec).decode())
        self.assertNotIn(self.protected["credentials"]["meta_access_token"], common.canonical_payload_bytes(spec).decode())
        with self.assertRaisesRegex(common.ReleaseError, "storage differs"):
            boot.spec_projection(spec, spec)

    def test_readback_normalizes_only_omitted_general_type(self):
        spec = boot.runtime_spec(self.a, self.d, self.protected, self.driver)
        self.provider.create(spec)
        observed = copy.deepcopy(self.provider.spec)
        for env in observed["services"][0]["envs"]:
            if env["type"] == "GENERAL":
                env.pop("type")
        self.assertEqual(boot.spec_projection(observed, spec), boot.spec_projection(self.provider.spec, spec))
        for kind, change in (("SECRET", lambda e: e.pop("type")), ("GENERAL", lambda e: e.update(type=None)),
                             ("GENERAL", lambda e: e.update(type="unknown")),
                             ("GENERAL", lambda e: e.update(scope="RUN_AND_BUILD_TIME")),
                             ("GENERAL", lambda e: e.update(unexpected=True))):
            observed = copy.deepcopy(self.provider.spec)
            target = next(e for e in observed["services"][0]["envs"] if e["type"] == kind)
            change(target)
            with self.subTest(kind=kind, change=change), self.assertRaises(common.ReleaseError):
                boot.spec_projection(observed, spec)

    def test_reviewed_read_scope_dependencies_and_no_credential_view(self):
        self.assertEqual(boot.SCOPE_REVIEW["read"], ["account:read", "actions:read", "app:read", "database:read", "regions:read", "sizes:read"])
        self.assertNotIn("database:view_credentials", boot.SCOPE_REVIEW["read"])
        self.assertNotIn("database:view_credentials", boot.SCOPE_REVIEW["create"])

    def test_health_rejects_private_dns_before_connect_and_redirect_response(self):
        origin = "https://rereply-canary-driver-test-abc.ondigitalocean.app"
        with (mock.patch.object(boot.socket, "getaddrinfo", return_value=[(2, 1, 6, "", ("127.0.0.1", 443))]),
              mock.patch.object(boot.http.client, "HTTPSConnection") as connect,
              self.assertRaisesRegex(common.ReleaseError, "not public")):
            boot.health(origin)
        connect.assert_not_called()
        connection = mock.MagicMock()
        connection.getresponse.return_value.status = 302
        with (mock.patch.object(boot.socket, "getaddrinfo", return_value=[(2, 1, 6, "", ("8.8.8.8", 443))]),
              mock.patch.object(boot.socket, "create_connection") as socket_connect,
              mock.patch.object(boot.http.client, "HTTPSConnection", return_value=connection),
              self.assertRaisesRegex(common.ReleaseError, "health did not pass")):
            boot.health(origin)
        socket_connect.assert_called_once_with(("8.8.8.8", 443), timeout=15)
        self.assertEqual(connection.request.call_args.args, ("GET", "/healthz"))
        self.assertNotIn("Authorization", connection.request.call_args.kwargs["headers"])
        connection.close.assert_called_once()

    def test_literal_provider_https_default_ingress_and_redirects(self):
        origin = "https://rereply-canary-driver-test-abc.ondigitalocean.app"
        app = {"default_ingress": origin, "live_url": origin, "live_domain": origin.removeprefix("https://")}
        self.assertEqual(boot.driver_origin(app, "rereply-canary-driver-test"), origin)
        for bad in (origin.removeprefix("https://"), origin + "/", origin + "/redirect", origin + "?x=1", "http://" + app["live_domain"]):
            with self.subTest(bad=bad), self.assertRaises(common.ReleaseError):
                boot.driver_origin({**app, "default_ingress": bad}, "rereply-canary-driver-test")

    def test_one_shot_workflow_inventory_rejects_prior_dispatch_and_rerun(self):
        run = {"id": 123, "head_sha": self.a["control_sha"], "run_attempt": 1, "head_branch": "main",
               "event": "workflow_dispatch", "path": boot.WORKFLOW, "status": "in_progress"}
        api = mock.Mock()
        api.pages.return_value = {"total_count": 1, "workflow_runs": [run]}
        boot.require_unique_run(api, self.a["control_sha"], "123")
        for result in ({"total_count": 2, "workflow_runs": [run, {**run, "id": 122, "status": "completed"}]},
                       {"total_count": 1, "workflow_runs": [{**run, "run_attempt": 2}]},
                       {"total_count": 0, "workflow_runs": []}):
            api.pages.return_value = result
            with self.assertRaises(common.ReleaseError):
                boot.require_unique_run(api, self.a["control_sha"], "123")

    def test_database_credential_response_rejected_without_echo(self):
        for value in ({"database": {"connection": {"password": "MUST-NOT-PRINT"}}},
                      {"database": {"users": [{"password": "MUST-NOT-PRINT"}]}},
                      {"connection": {"uri": "postgresql://MUST-NOT-PRINT"}}):
            with self.assertRaisesRegex(common.ReleaseError, "^ledger response contains forbidden credentials$"):
                boot.reject_database_credentials(value)
        boot.reject_database_credentials({"database": {"id": uid(12), "connection": {"password": None, "uri": ""}}})

    def test_provider_get_allowlist_denies_user_credentials_and_all_foreign_targets(self):
        provider = boot.Provider(self.d["plan"], "synthetic-read-token", "synthetic-create-token")
        with mock.patch.object(boot.fixture, "_wire") as wire:
            for path in ("/v2/databases/" + uid(12) + "/users", "/v2/apps/" + uid(10),
                         "/v2/databases/" + uid(99), "/v2/databases", "/v2/account?token=bad"):
                with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                    provider.get(path)
            wire.assert_not_called()

    def test_real_provider_post_burns_before_uncertain_transport(self):
        provider = boot.Provider(self.d["plan"], "synthetic-read-token", "synthetic-create-token")
        with mock.patch.object(boot.fixture, "_wire", side_effect=TimeoutError()) as wire:
            with self.assertRaises(TimeoutError):
                provider.create({})
            with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
                provider.create({})
            self.assertEqual(wire.call_count, 1)
            self.assertEqual(wire.call_args.kwargs["method"], "POST")

    def test_child_environment_excludes_private_credentials_and_debug(self):
        with mock.patch.dict(os.environ, {"GH_DEBUG": "api", "HTTP_PROXY": "bad", "GH_TOKEN": "not-inherited",
                                          **{name: "NEVER-INHERIT" for name in boot.PRIVATE_NAMES}}):
            env = boot.subprocess_environment(token="specific-read-token")
        self.assertEqual(env["GH_TOKEN"], "specific-read-token")
        for name in (*boot.PRIVATE_NAMES, "GH_DEBUG", "HTTP_PROXY"):
            self.assertNotIn(name, env)

    def test_isolated_cli_from_unrelated_directory_and_missing_input_never_http(self):
        with tempfile.TemporaryDirectory() as directory:
            proc = subprocess.run([sys.executable, "-I", "-S", "-B", str(Path(boot.__file__).resolve()), "--help"],
                                  cwd=directory, env=boot.subprocess_environment(), capture_output=True, timeout=20)
            self.assertEqual(proc.returncode, 0, proc.stderr.decode())
        env = {"BOOTSTRAP_AUTHORIZATION_JSON": common.canonical_payload_bytes(self.a).decode(),
               "CONTROL_SHA": self.a["control_sha"], "GH_TOKEN": "synthetic-read-token"}
        with (mock.patch.dict(os.environ, env, clear=True), mock.patch.object(boot, "GitHubRead") as network,
              mock.patch("sys.stderr", new_callable=io.StringIO) as stderr):
            self.assertEqual(boot.main(["install", "--control-root", "."]), 1)
            network.assert_not_called()
            self.assertEqual(stderr.getvalue(), "driver bootstrap stopped; no retry or further mutation is authorized\n")

    def test_public_validation_never_imports_private_fixture_adapter(self):
        env = {"BOOTSTRAP_AUTHORIZATION_JSON": common.canonical_payload_bytes(self.a).decode(),
               "CONTROL_SHA": self.a["control_sha"], "GH_TOKEN": "synthetic-read-token"}
        with (mock.patch.dict(os.environ, env, clear=True), mock.patch.object(boot, "GitHubRead"),
              mock.patch.object(boot, "current_guard"), mock.patch.object(boot.fixture, "_pinned_gh") as gh,
              mock.patch.object(boot, "Provider") as provider, mock.patch("sys.stdout", new_callable=io.StringIO)):
            self.assertEqual(boot.main(["validate", "--control-root", "."]), 0)
            gh.assert_not_called()
            provider.assert_not_called()

    def test_expired_or_future_packet_never_mutates(self):
        for key, value in (("expires_at", (NOW - dt.timedelta(seconds=1)).strftime("%Y-%m-%dT%H:%M:%SZ")),
                           ("issued_at", (NOW + dt.timedelta(seconds=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))):
            changed = {**self.a, key: value}
            with self.subTest(key=key), self.assertRaisesRegex(common.ReleaseError, "window differs"):
                boot.validate_authorization(changed, control_sha=self.a["control_sha"], now=NOW)


class FakeEnvironment:
    def __init__(self):
        self.value = metadata()
        self.key = {"key_id": "1234", "key": base64.b64encode(b"p" * 32).decode()}
        self.gets = []

    def get(self, path):
        self.gets.append(path)
        if path.endswith("/public-key"):
            return copy.deepcopy(self.key)
        if "/secrets?" in path:
            return {"total_count": len(self.value["secrets"]), "secrets": copy.deepcopy(self.value["secrets"])}
        if "/variables?" in path:
            return {"total_count": len(self.value["variables"]), "variables": copy.deepcopy(self.value["variables"])}
        if path.endswith("/" + boot.ENVIRONMENT):
            return copy.deepcopy(self.value["environment"])
        raise AssertionError("unexpected synthetic GitHub endpoint")

    def pages(self, path, key):
        if key != "branch_policies" or not path.endswith("/deployment-branch-policies"):
            raise AssertionError("unexpected synthetic pagination")
        return {"total_count": len(self.value["branch_policies"]), "branch_policies": copy.deepcopy(self.value["branch_policies"])}


class EnvironmentWriterTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeEnvironment()
        self.writer = boot.EnvironmentWriter("synthetic-writer-token", Path("mocked-pinned-gh"), common.sha256_value(metadata()))
        self.writer.api = self.api
        self.config = {"schema_version": 1, "url": "https://rereply-canary-driver-test-abc.ondigitalocean.app/v1/execute",
                       "driver_version_sha256": "a" * 64, "fixture_descriptor_sha256": "b" * 64,
                       "hmac_key_base64": base64.b64encode(b"h" * 32).decode()}
        self.puts = []

    def seal(self, args, **kwargs):
        self.assertEqual(args, ["mocked-pinned-gh", "secret", "set", boot.SECRET_NAME, "--env", boot.ENVIRONMENT,
                                "--repo", common.REPOSITORY, "--no-store"])
        self.assertEqual(kwargs["input"], common.canonical_payload_bytes(self.config))
        self.assertEqual(kwargs["env"]["GH_TOKEN"], "synthetic-writer-token")
        self.assertNotIn(self.config["hmac_key_base64"], " ".join(args))
        return subprocess.CompletedProcess(args, 0, base64.b64encode(b"z" * (len(kwargs["input"]) + 48)) + b"\n", b"")

    def put(self, opener, url, **kwargs):
        self.assertEqual(url, "https://api.github.com" + boot.fixture.API_PREFIX + "/environments/" + boot.ENVIRONMENT
                         + "/secrets/" + boot.SECRET_NAME)
        self.assertEqual(kwargs["method"], "PUT")
        body = common.loads_strict(kwargs["body"])
        self.assertEqual(set(body), {"key_id", "encrypted_value"})
        self.assertNotIn(self.config["hmac_key_base64"], kwargs["body"].decode())
        self.puts.append(body)
        self.api.value["secrets"].append({"name": boot.SECRET_NAME, "created_at": "2026-09-20T00:00:00Z",
                                          "updated_at": "2026-09-20T00:00:00Z"})
        return b""

    def test_real_writer_seals_only_then_one_explicit_put_and_exact_metadata_readback(self):
        with (mock.patch.object(boot.subprocess, "run", side_effect=self.seal) as seal,
              mock.patch.object(boot.fixture, "_wire", side_effect=self.put) as wire):
            self.writer.install(self.config)
            self.assertEqual(seal.call_count, 1)
            self.assertEqual(wire.call_count, 1)
            with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
                self.writer.install(self.config)
        self.assertEqual(len(self.puts), 1)

    def test_ambiguous_put_is_burned_without_retry(self):
        with (mock.patch.object(boot.subprocess, "run", side_effect=self.seal),
              mock.patch.object(boot.fixture, "_wire", side_effect=TimeoutError("do not repeat")) as wire):
            with self.assertRaises(TimeoutError):
                self.writer.install(self.config)
            with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
                self.writer.install(self.config)
            self.assertEqual(wire.call_count, 1)

    def test_unknown_names_values_and_protection_fail_before_sealing(self):
        changes = (
            lambda v: v["secrets"].append({"name": "UNREVIEWED", "created_at": "x", "updated_at": "x"}),
            lambda v: v["secrets"][0].update(updated_at="changed"),
            lambda v: v["variables"].append({"name": "X", "value": "unexpected", "created_at": "x", "updated_at": "x"}),
            lambda v: v["environment"].update(protection_rules=[{"type": "required_reviewers"}]),
            lambda v: v["branch_policies"][0].update(name="*"),
        )
        for change in changes:
            self.api.value = metadata()
            change(self.api.value)
            with (self.subTest(change=change), mock.patch.object(boot.subprocess, "run") as seal,
                  self.assertRaises(common.ReleaseError)):
                self.writer.install(self.config)
            seal.assert_not_called()

    def test_postwrite_metadata_drift_rejected_not_hidden_by_new_secret(self):
        def changed(*args, **kwargs):
            result = self.put(*args, **kwargs)
            self.api.value["secrets"][0]["updated_at"] = "changed"
            return result
        with (mock.patch.object(boot.subprocess, "run", side_effect=self.seal),
              mock.patch.object(boot.fixture, "_wire", side_effect=changed),
              self.assertRaisesRegex(common.ReleaseError, "changed during installation")):
            self.writer.install(self.config)
        self.assertEqual(len(self.puts), 1)

    def test_key_change_or_bad_sealed_length_rejected_before_put(self):
        def changed(args, **kwargs):
            result = self.seal(args, **kwargs)
            self.api.key["key_id"] = "5678"
            return result
        with (mock.patch.object(boot.subprocess, "run", side_effect=changed),
              mock.patch.object(boot.fixture, "_wire") as wire,
              self.assertRaisesRegex(common.ReleaseError, "encryption key changed")):
            self.writer.install(self.config)
        wire.assert_not_called()


class PublisherBoundaryTests(unittest.TestCase):
    def test_valid_comparison_route_is_not_rejected_as_path_traversal(self):
        api = boot.GitHubRead("synthetic-reader-token")
        path = boot.fixture.API_PREFIX + "/compare/" + "a" * 40 + "..." + "b" * 40
        with mock.patch.object(boot.fixture, "_wire", return_value=b'{"status":"ahead"}') as wire:
            self.assertEqual(api.get(path), {"status": "ahead"})
            self.assertEqual(wire.call_count, 1)
        with self.assertRaises(common.ReleaseError):
            api.get(boot.fixture.API_PREFIX + "/contents/../secret")

    def test_archive_exact_thirteen_files_duplicate_symlink_and_extra_rejected(self):
        names = ("driver-inputs.tsv", "driver-predicate.json", "image-inspect.json", "image.json", "provenance.bundle.json",
                 "remote-descriptor.json", "sbom.bundle.json", "sbom.spdx.json", "scan.json", "secret-report.json",
                 "source-binding.bundle.json", "unit-test.txt", "vulnerability-report.json")
        for variant in ("valid", "extra", "symlink", "missing"):
            raw = io.BytesIO()
            with zipfile.ZipFile(raw, "w") as archive:
                for name in names:
                    if variant == "missing" and name == "unit-test.txt":
                        continue
                    info = zipfile.ZipInfo(name)
                    info.external_attr = (0o120777 if variant == "symlink" and name == "unit-test.txt" else 0o100644) << 16
                    archive.writestr(info, b"{}")
                if variant == "extra":
                    archive.writestr("unreviewed.json", b"{}")
            if variant == "valid":
                self.assertEqual(set(boot.publisher_files(raw.getvalue())), set(names))
            else:
                with self.subTest(variant=variant), self.assertRaises(common.ReleaseError):
                    boot.publisher_files(raw.getvalue())

    def test_archive_limit_admits_real_september15_archive_sizes(self):
        # Observed from authenticated artifact 10421321160. No contents/keys.
        sizes = [869, 3028, 5182, 983, 11983, 2425, 21636334, 16219138, 1222, 2046197, 14167, 1964, 2046198]
        self.assertGreater(sum(sizes), 32 * 1024 * 1024)
        self.assertLess(sum(sizes), boot.MAX_PUBLIC)


if __name__ == "__main__":
    unittest.main()
