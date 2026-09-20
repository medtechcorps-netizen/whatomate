"""Offline bootstrap tests: real policy/rehydration, mocked external transports."""
from __future__ import annotations

import base64
import copy
from contextlib import ExitStack
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
    from . import launch_production_prerequisites as prerequisites
    from . import test_provision_production_crm_canary_fixture as samples
except ImportError:
    import bootstrap_production_crm_canary_driver as boot
    import launch_production_prerequisites as prerequisites
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


def provider_responses(plan):
    ledger = plan["ledger"]
    cluster = "/v2/databases/" + ledger["cluster_id"]
    return {
        "/v2/account": {"account": {"uuid": plan["provider_account_uuid"], "status": "active"}},
        "/v2/apps/tiers/instance_sizes/" + plan["instance_size_slug"]:
            {"instance_size": {"slug": plan["instance_size_slug"], "usd_per_month": plan["monthly_usd"]}},
        "/v2/apps/regions": {"regions": [{"slug": plan["region"], "disabled": False, "data_centers": ["sgp1"]}]},
        cluster: {"database": {"id": ledger["cluster_id"], "name": ledger["cluster_name"], "engine": "pg",
                                "version": ledger["version"], "status": "online", "region": "sgp1"}},
        cluster + "/dbs/" + ledger["database"]: {"db": {"name": ledger["database"]}},
        cluster + "/firewall": {"rules": [{"type": "app", "value": uid(10)}]},
        "/v2/apps?page=1&per_page=200": {"apps": [{"id": uid(10), "spec": {"name": "production-app"}}],
                                           "meta": {"total": 1}},
    }


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


class FakeProviderReader:
    def __init__(self, provider):
        self.provider = provider

    def preflight(self):
        return self.provider.preflight()


class FakeMetadataReader:
    def __init__(self, writer):
        self.writer = writer

    def require_absent(self):
        return self.writer.require_absent()


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.a, self.d, self.protected, self.receipt, self.transport, self.driver = packets()
        self.provider = FakeProvider(self.d["plan"])
        self.writer = FakeWriter()
        self.read_provider = FakeProviderReader(self.provider)
        self.read_environment = FakeMetadataReader(self.writer)
        self.probe = mock.Mock()
        self.current = mock.Mock()
        self.authenticate = mock.Mock(return_value=self.receipt)
        self.image = mock.Mock()

    def execute(self):
        return boot.install_once(self.a, self.d, self.protected, provider=self.provider, writer=self.writer,
            authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
            rehydrate=boot.fixture.rehydrate, transport=self.transport, sleep=lambda _: None,
            probe=self.probe, now=lambda: NOW)

    def check(self):
        return boot.check_once(self.a, self.d, self.protected, provider=self.read_provider, reader=self.read_environment,
            authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
            rehydrate=boot.fixture.rehydrate, transport=self.transport, now=lambda: NOW)

    def test_check_runs_full_real_rehydration_and_second_cas_without_mutation_or_runtime_output(self):
        before = len(self.transport.calls)
        with (mock.patch.object(boot, "runtime_spec", wraps=boot.runtime_spec) as spec,
              mock.patch.object(self.provider, "create", side_effect=AssertionError("check must not create")),
              mock.patch.object(self.writer, "install", side_effect=AssertionError("check must not install"))):
            result = self.check()
        self.assertEqual(result, {"schema_version": 1, "state": "private-driver-prerequisites-verified",
                                  "mutation_performed": False})
        self.assertGreater(len(self.transport.calls), before)
        self.assertTrue(all(call[0] == "GET" for call in self.transport.calls[before:]))
        self.assertEqual(self.provider.calls, ["preflight", "preflight"])
        self.assertEqual(self.writer.calls, ["absent", "absent"])
        self.assertEqual(self.current.call_count, 3)
        self.authenticate.assert_called_once_with()
        self.image.assert_called_once_with()
        spec.assert_called_once_with(self.a, self.d, self.protected, self.driver)
        self.probe.assert_not_called()
        public = common.canonical_payload_bytes(result).decode()
        for value in (self.d["hmac_key_base64"], self.protected["credentials"]["klinik_password"],
                      self.d["plan"]["provider_account_uuid"], self.d["plan"]["ledger"]["cluster_id"]):
            self.assertNotIn(value, public)

    def test_check_and_install_delegate_to_identical_shared_preflight(self):
        with mock.patch.object(boot, "preflight", wraps=boot.preflight) as preflight:
            self.check()
            checked = preflight.call_args
            self.execute()
            installed = preflight.call_args
        self.assertEqual(preflight.call_count, 2)
        self.assertEqual(checked.args, installed.args)
        self.assertEqual(set(checked.kwargs), set(installed.kwargs))
        self.assertIs(checked.kwargs["provider"], self.read_provider)
        self.assertIs(installed.kwargs["provider"], self.provider)
        self.assertIs(checked.kwargs["reader"], self.read_environment)
        self.assertIs(installed.kwargs["reader"], self.writer)
        for key in checked.kwargs.keys() - {"now", "provider", "reader"}:
            self.assertIs(checked.kwargs[key], installed.kwargs[key], key)

    def test_check_rejects_mutation_capable_injected_adapters_before_preflight(self):
        for provider, reader in ((self.provider, self.read_environment), (self.read_provider, self.writer)):
            with self.subTest(provider=type(provider).__name__, reader=type(reader).__name__), self.assertRaises(common.ReleaseError):
                boot.check_once(self.a, self.d, self.protected, provider=provider, reader=reader,
                    authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
                    rehydrate=boot.fixture.rehydrate, transport=self.transport, now=lambda: NOW)
            self.assertEqual(self.provider.calls, [])
            self.assertEqual(self.writer.calls, [])
            self.authenticate.assert_not_called()

    def test_check_second_cas_changes_and_last_authority_failure_never_mutate(self):
        for boundary in ("provider", "environment", "authority"):
            self.setUp()
            with self.subTest(boundary=boundary), ExitStack() as stack:
                if boundary == "provider":
                    first = self.provider.preflight()
                    stack.enter_context(mock.patch.object(self.provider, "preflight", side_effect=[first, ([], first[1])]))
                elif boundary == "environment":
                    stack.enter_context(mock.patch.object(self.writer, "require_absent",
                        side_effect=[None, common.ReleaseError("changed")]))
                else:
                    self.current.side_effect = [None, None, common.ReleaseError("changed")]
                with self.assertRaises(common.ReleaseError):
                    self.check()
                self.assertEqual(self.provider.create_count, 0)
                self.assertEqual(self.writer.installed, [])

    def test_every_provider_preflight_stage_failure_blocks_create_and_install(self):
        plan = self.d["plan"]
        routes = provider_responses(plan)
        stages = ("PROVIDER_ACCOUNT", "PROVIDER_SIZE", "PROVIDER_REGION", "PROVIDER_LEDGER_METADATA",
                  "PROVIDER_LEDGER_DATABASE", "PROVIDER_FIREWALL", "PROVIDER_APPS")
        for route, stage in zip(routes, stages):
            for occurrence in (1, 2):
                self.provider = boot.Provider(plan, "synthetic-read-token", "synthetic-create-token")
                seen = []
                def get(path):
                    seen.append(path)
                    if path == route and seen.count(route) == occurrence:
                        raise common.ReleaseError("bounded HTTP operation failed: status 403")
                    return copy.deepcopy(routes[path])
                with (self.subTest(stage=stage, pass_number=occurrence),
                      mock.patch.object(self.provider, "get", side_effect=get),
                      mock.patch.object(self.provider, "create") as create, self.assertRaises(common.ReleaseError) as error):
                    self.execute()
                self.assertEqual(boot.failure_report(error.exception), {"schema_version": 1, "outcome": "ERROR",
                    "stage": stage, "code": "HTTP_FORBIDDEN"})
                create.assert_not_called()
                self.assertEqual(self.writer.installed, [])
                self.assertEqual(seen[-1], route)

    def test_real_provider_rejects_each_prestate_drift_before_create(self):
        plan = self.d["plan"]
        cluster = "/v2/databases/" + plan["ledger"]["cluster_id"]
        changes = (
            ("/v2/account", lambda v: v["account"].update(uuid=uid(99)), "PROVIDER_ACCOUNT"),
            ("/v2/apps/tiers/instance_sizes/" + plan["instance_size_slug"],
             lambda v: v["instance_size"].update(usd_per_month="99.00"), "PROVIDER_SIZE"),
            ("/v2/apps/regions", lambda v: v["regions"][0].update(disabled=True), "PROVIDER_REGION"),
            (cluster, lambda v: v["database"].update(version="99"), "PROVIDER_LEDGER_METADATA"),
            (cluster + "/dbs/" + plan["ledger"]["database"],
             lambda v: v["db"].update(name="foreign_database"), "PROVIDER_LEDGER_DATABASE"),
            (cluster + "/firewall", lambda v: v["rules"].clear(), "PROVIDER_FIREWALL"),
            ("/v2/apps?page=1&per_page=200", lambda v: v["apps"][0]["spec"].update(name=plan["app_name"]),
             "PROVIDER_APPS"),
        )
        for route, change, stage in changes:
            routes = provider_responses(plan)
            change(routes[route])
            self.provider = boot.Provider(plan, "synthetic-read-token", "synthetic-create-token")
            with (self.subTest(stage=stage), mock.patch.object(self.provider, "get", side_effect=lambda p: routes[p]),
                  mock.patch.object(self.provider, "create") as create, self.assertRaises(common.ReleaseError) as error):
                self.execute()
            self.assertEqual(boot.failure_report(error.exception)["stage"], stage)
            create.assert_not_called()
            self.assertEqual(self.writer.installed, [])

    def test_real_read_only_provider_and_environment_reader_have_no_mutation_capability(self):
        provider = boot.ReadOnlyProvider(self.d["plan"], "synthetic-read-token")
        reader = boot.EnvironmentReader("synthetic-gh-reader", common.sha256_value(metadata()))
        self.assertFalse(hasattr(provider, "create"))
        self.assertFalse(hasattr(reader, "install"))
        routes = provider_responses(self.d["plan"])
        def wire(opener, url, **kwargs):
            self.assertEqual(kwargs.get("method", "GET"), "GET")
            self.assertEqual(kwargs["headers"]["Authorization"], "Bearer synthetic-read-token")
            self.assertNotIn("body", kwargs)
            return common.canonical_payload_bytes(routes[url.removeprefix(common.API_ORIGIN)])
        with mock.patch.object(boot.fixture, "_wire", side_effect=wire) as request:
            self.assertEqual(provider.preflight(), ([{"id": uid(10), "name": "production-app"}],
                                                    [{"type": "app", "value": uid(10)}]))
            self.assertEqual(request.call_count, 7)
        reader.api = FakeEnvironment()
        with mock.patch.object(boot.fixture, "_wire") as request, mock.patch.object(boot.subprocess, "run") as cli:
            reader.require_absent()
            reader.require_absent()
            request.assert_not_called()
            cli.assert_not_called()

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
               "event": "workflow_dispatch", "path": boot.WORKFLOW, "status": "in_progress",
               "workflow_id": 37, "display_title": boot.RUN_TITLES["install"], "conclusion": None}
        api = mock.Mock()
        api.get.return_value = {"id": 37, "path": boot.WORKFLOW}
        api.pages.return_value = {"total_count": 1, "workflow_runs": [run]}
        boot.require_unique_run(api, self.a["control_sha"], "123")
        for result in ({"total_count": 2, "workflow_runs": [run, {**run, "id": 122, "status": "completed"}]},
                       {"total_count": 1, "workflow_runs": [{**run, "run_attempt": 2}]},
                       {"total_count": 0, "workflow_runs": []}):
            api.pages.return_value = result
            with self.assertRaises(common.ReleaseError):
                boot.require_unique_run(api, self.a["control_sha"], "123")

    def test_exact_completed_check_history_allows_checks_then_only_one_install(self):
        api = mock.Mock()
        api.get.return_value = {"id": 37, "path": boot.WORKFLOW}
        base = {"head_sha": self.a["control_sha"], "run_attempt": 1, "head_branch": "main",
                "event": "workflow_dispatch", "path": boot.WORKFLOW, "workflow_id": 37}
        prior = [{**base, "id": 121, "status": "completed", "conclusion": "success",
                  "display_title": boot.RUN_TITLES["check"]},
                 {**base, "id": 122, "status": "completed", "conclusion": "failure",
                  "display_title": boot.RUN_TITLES["check"]}]
        for mode in ("check", "install"):
            current = {**base, "id": 123, "status": "in_progress", "conclusion": None,
                       "display_title": boot.RUN_TITLES[mode]}
            api.pages.return_value = {"total_count": 3, "workflow_runs": [current, *prior]}
            boot.require_unique_run(api, self.a["control_sha"], "123", mode)
            self.assertIn("?head_sha=" + self.a["control_sha"], api.pages.call_args.args[0])
            self.assertEqual(api.pages.call_args.args[1], "workflow_runs")
            for conclusion in ("success", "failure", "cancelled", "timed_out", None):
                burned = {**prior[0], "display_title": boot.RUN_TITLES["install"], "conclusion": conclusion}
                api.pages.return_value = {"total_count": 2, "workflow_runs": [current, burned]}
                with self.subTest(mode=mode, prior_install=conclusion), self.assertRaises(common.ReleaseError):
                    boot.require_unique_run(api, self.a["control_sha"], "123", mode)

    def test_full_run_history_rejects_unknown_foreign_duplicate_rerun_and_nonterminal_check(self):
        api = mock.Mock()
        api.get.return_value = {"id": 37, "path": boot.WORKFLOW}
        current = {"id": 123, "head_sha": self.a["control_sha"], "run_attempt": 1, "head_branch": "main",
                   "event": "workflow_dispatch", "path": boot.WORKFLOW, "status": "in_progress",
                   "workflow_id": 37, "display_title": boot.RUN_TITLES["install"], "conclusion": None}
        prior = {**current, "id": 122, "display_title": boot.RUN_TITLES["check"],
                 "status": "completed", "conclusion": "success"}
        variations = ({"display_title": "unknown"}, {"display_title": None}, {"workflow_id": 38},
                      {"path": boot.PUBLISHER}, {"head_sha": "d" * 40}, {"head_branch": "feature"},
                      {"event": "push"}, {"run_attempt": 2}, {"run_attempt": True},
                      {"status": "in_progress"}, {"status": "queued"}, {"conclusion": None},
                      {"conclusion": "unknown"}, {"id": 123})
        for change in variations:
            api.pages.return_value = {"total_count": 2, "workflow_runs": [current, {**prior, **change}]}
            with self.subTest(prior=change), self.assertRaises(common.ReleaseError):
                boot.require_unique_run(api, self.a["control_sha"], "123", "install")
        for change in ({"display_title": boot.RUN_TITLES["check"]}, {"status": "completed"},
                       {"conclusion": "failure"}, {"id": 124}, {"run_attempt": 2}):
            api.pages.return_value = {"total_count": 1, "workflow_runs": [{**current, **change}]}
            with self.subTest(current=change), self.assertRaises(common.ReleaseError):
                boot.require_unique_run(api, self.a["control_sha"], "123", "install")
        for count, rows in ((0, []), (2, [current]), (1, [current, prior]), (2, [current, current])):
            api.pages.return_value = {"total_count": count, "workflow_runs": rows}
            with self.subTest(count=count, rows=len(rows)), self.assertRaises(common.ReleaseError):
                boot.require_unique_run(api, self.a["control_sha"], "123", "install")

    def test_database_credential_response_rejected_without_echo(self):
        for value in ({"database": {"connection": {"password": "MUST-NOT-PRINT"}}},
                      {"database": {"users": [{"password": "MUST-NOT-PRINT"}]}},
                      {"connection": {"uri": "postgresql://MUST-NOT-PRINT"}},
                      {"connection": {"uri": "postgresql://REDACTED"}},
                      {"connection": {"uri": "REDACTED"}}):
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
               "CONTROL_SHA": self.a["control_sha"], "GH_TOKEN": "synthetic-read-token", boot.MODE_ENV: "install"}
        with (mock.patch.dict(os.environ, env, clear=True), mock.patch.object(boot, "GitHubRead") as network,
              mock.patch("sys.stderr", new_callable=io.StringIO) as stderr):
            self.assertEqual(boot.main(["install", "--control-root", "."]), 1)
            network.assert_not_called()
            report = common.loads_strict(stderr.getvalue())
            self.assertEqual(report, {"schema_version": 1, "outcome": "ERROR", "stage": "PRIVATE_INPUTS",
                                      "code": "BOOTSTRAP_CHECK_FAILED"})

    def test_public_validation_never_imports_private_fixture_adapter(self):
        env = {"BOOTSTRAP_AUTHORIZATION_JSON": common.canonical_payload_bytes(self.a).decode(),
               "CONTROL_SHA": self.a["control_sha"], "GH_TOKEN": "synthetic-read-token", boot.MODE_ENV: "check"}
        with (mock.patch.dict(os.environ, env, clear=True), mock.patch.object(boot, "GitHubRead"),
              mock.patch.object(boot, "current_guard"), mock.patch.object(boot.fixture, "_pinned_gh") as gh,
              mock.patch.object(boot, "Provider") as provider, mock.patch("sys.stdout", new_callable=io.StringIO)):
            self.assertEqual(boot.main(["validate", "--control-root", "."]), 0)
            gh.assert_not_called()
            provider.assert_not_called()

    def cli_env(self, mode, *, private=False):
        env = {"BOOTSTRAP_AUTHORIZATION_JSON": common.canonical_payload_bytes(self.a).decode(),
               "CONTROL_SHA": self.a["control_sha"], "GH_TOKEN": "synthetic-read-token", boot.MODE_ENV: mode}
        if private:
            env.update(CRM_CANARY_DRIVER_BOOTSTRAP_JSON=common.canonical_payload_bytes(self.d).decode(),
                       CRM_CANARY_FIXTURE_INPUT_JSON=common.canonical_payload_bytes(self.protected).decode(),
                       DO_DRIVER_BOOTSTRAP_READ_TOKEN="synthetic-provider-reader")
        return env

    def test_cli_mode_is_exact_and_must_match_private_command_before_any_adapter(self):
        for command, mode in (("check", "install"), ("install", "check"), ("check", "CHECK"),
                              ("install", "install "), ("validate", "unknown"), ("validate", None)):
            env = self.cli_env(mode)
            if mode is None:
                env.pop(boot.MODE_ENV)
            with (self.subTest(command=command, mode=mode), mock.patch.dict(os.environ, env, clear=True),
                  mock.patch.object(boot, "GitHubRead") as api, mock.patch.object(boot.fixture, "_wire") as wire,
                  mock.patch("sys.stdout", new_callable=io.StringIO) as out,
                  mock.patch("sys.stderr", new_callable=io.StringIO) as err):
                self.assertEqual(boot.main([command, "--control-root", "."]), 1)
                self.assertEqual(out.getvalue(), "")
                self.assertEqual(common.loads_strict(err.getvalue()), {"schema_version": 1, "outcome": "ERROR",
                    "stage": "MODE", "code": "BOOTSTRAP_CHECK_FAILED"})
                api.assert_not_called()
                wire.assert_not_called()

    def test_public_validate_binds_both_modes_to_the_current_run_guard(self):
        for mode in ("check", "install"):
            with (self.subTest(mode=mode), mock.patch.dict(os.environ, self.cli_env(mode), clear=True),
                  mock.patch.object(boot, "GitHubRead") as api,
                  mock.patch.object(boot, "current_guard") as guard,
                  mock.patch.object(boot, "ReadOnlyProvider") as reader,
                  mock.patch.object(boot, "Provider") as provider,
                  mock.patch.object(boot.fixture, "_pinned_gh") as gh,
                  mock.patch("sys.stdout", new_callable=io.StringIO)):
                self.assertEqual(boot.main(["validate", "--control-root", "."]), 0)
                guard.assert_called_once_with(api.return_value, Path(".").resolve(), mode)
                reader.assert_not_called()
                provider.assert_not_called()
                gh.assert_not_called()

    def test_check_cli_constructs_only_read_adapters_and_emits_no_artifact(self):
        result = {"schema_version": 1, "state": "private-driver-prerequisites-verified", "mutation_performed": False}
        with (mock.patch.dict(os.environ, self.cli_env("check", private=True), clear=True),
              mock.patch.object(boot, "GitHubRead"), mock.patch.object(boot, "current_guard") as guard,
              mock.patch.object(boot.fixture, "_pinned_gh", return_value=Path("mocked-pinned-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence"),
              mock.patch.object(boot, "ReadOnlyProductTransport") as transport,
              mock.patch.object(boot, "ReadOnlyProvider") as provider,
              mock.patch.object(boot, "EnvironmentReader") as reader,
              mock.patch.object(boot, "Provider") as mutable_provider,
              mock.patch.object(boot, "EnvironmentWriter") as writer,
              mock.patch.object(boot, "check_once", return_value=result) as check,
              mock.patch.object(boot, "install_once") as install,
              mock.patch.object(boot.fixture, "_wire") as wire,
              mock.patch.object(Path, "mkdir") as mkdir,
              mock.patch.object(Path, "write_bytes") as write_bytes,
              mock.patch.object(Path, "write_text") as write_text,
              mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            self.assertEqual(boot.main(["check", "--control-root", "."]), 0)
            provider.assert_called_once_with(self.d["plan"], "synthetic-provider-reader")
            reader.assert_called_once_with("synthetic-read-token", self.d["plan"]["github_environment_sha256"])
            transport.assert_called_once_with(self.protected["credentials"]["meta_access_token"])
            self.assertIs(check.call_args.kwargs["provider"], provider.return_value)
            self.assertIs(check.call_args.kwargs["reader"], reader.return_value)
            self.assertIs(check.call_args.kwargs["transport"], transport.return_value)
            check.call_args.kwargs["current_guard"]()
            self.assertEqual(guard.call_args.args[-1], "check")
            for unused in (mutable_provider, writer, install, wire, mkdir, write_bytes, write_text):
                unused.assert_not_called()
            self.assertTrue(all(name not in os.environ for name in boot.PRIVATE_NAMES))
            self.assertEqual(common.loads_strict(out.getvalue()), result)
            self.assertEqual(err.getvalue(), "")

    def test_check_rejects_missing_private_read_input_and_any_write_input_before_network(self):
        cases = []
        for missing in boot.CHECK_PRIVATE_NAMES:
            env = self.cli_env("check", private=True)
            env.pop(missing)
            cases.append(("missing-" + missing, env, []))
        for forbidden in ("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"):
            for value in ("RAW-SENTINEL-TOKEN", ""):
                env = self.cli_env("check", private=True)
                env[forbidden] = value
                cases.append(("forbidden-" + forbidden, env, []))
        cases.append(("output", self.cli_env("check", private=True), ["--output-dir", "forbidden-check-output"]))
        for label, env, args in cases:
            with (self.subTest(label=label), mock.patch.dict(os.environ, env, clear=True),
                  mock.patch.object(boot, "GitHubRead") as api,
                  mock.patch.object(boot, "check_once") as check,
                  mock.patch("sys.stdout", new_callable=io.StringIO) as out,
                  mock.patch("sys.stderr", new_callable=io.StringIO) as err):
                self.assertEqual(boot.main(["check", "--control-root", ".", *args]), 1)
                api.assert_not_called()
                check.assert_not_called()
                self.assertEqual(out.getvalue(), "")
                report = common.loads_strict(err.getvalue())
                self.assertEqual(set(report), {"schema_version", "outcome", "stage", "code"})
                self.assertEqual(report["stage"], "PRIVATE_INPUTS")
                self.assertNotIn("RAW-SENTINEL", err.getvalue())

    def test_install_missing_either_write_authority_is_not_treated_as_check(self):
        for missing in ("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"):
            env = self.cli_env("install", private=True)
            env.update(DO_DRIVER_BOOTSTRAP_CREATE_TOKEN="synthetic-create", GH_CANARY_ENVIRONMENT_WRITE_TOKEN="synthetic-write")
            env.pop(missing)
            with (self.subTest(missing=missing), mock.patch.dict(os.environ, env, clear=True),
                  mock.patch.object(boot, "GitHubRead") as api,
                  mock.patch.object(boot, "check_once") as check,
                  mock.patch.object(boot, "install_once") as install,
                  mock.patch("sys.stderr", new_callable=io.StringIO)):
                self.assertEqual(boot.main(["install", "--control-root", "."]), 1)
                api.assert_not_called()
                check.assert_not_called()
                install.assert_not_called()

    def test_cli_failure_never_serializes_private_exception_or_writes_artifacts(self):
        sentinel = 'RAW_KEY=RAW_VALUE token=RAW_TOKEN postgresql://RAW_USER:RAW_PASSWORD@RAW_HOST/db RAW_EXCEPTION'
        def fail(*args, **kwargs):
            boot.mark_stage("FIXTURE_REHYDRATION")
            raise RuntimeError(sentinel)
        with (mock.patch.dict(os.environ, self.cli_env("check", private=True), clear=True),
              mock.patch.object(boot, "GitHubRead"), mock.patch.object(boot, "current_guard"),
              mock.patch.object(boot.fixture, "_pinned_gh", return_value=Path("mocked-pinned-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence"),
              mock.patch.object(boot, "ReadOnlyProductTransport"),
              mock.patch.object(boot, "ReadOnlyProvider"), mock.patch.object(boot, "EnvironmentReader"),
              mock.patch.object(boot, "check_once", side_effect=fail),
              mock.patch.object(boot.fixture, "_wire") as wire,
              mock.patch.object(Path, "mkdir") as mkdir,
              mock.patch.object(Path, "write_bytes") as write_bytes,
              mock.patch.object(Path, "write_text") as write_text,
              mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            self.assertEqual(boot.main(["check", "--control-root", "."]), 1)
            self.assertEqual(out.getvalue(), "")
            self.assertEqual(common.loads_strict(err.getvalue()), {"schema_version": 1, "outcome": "ERROR",
                "stage": "FIXTURE_REHYDRATION", "code": "BOOTSTRAP_CHECK_FAILED"})
            for value in (sentinel, "RAW_", "postgresql://", self.d["hmac_key_base64"]):
                self.assertNotIn(value, err.getvalue())
            for unused in (wire, mkdir, write_bytes, write_text):
                unused.assert_not_called()

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


class ReadOnlyBoundaryTests(unittest.TestCase):
    def test_product_read_adapter_allows_only_login_and_bodyless_get(self):
        with mock.patch.object(boot.fixture, "ProductHTTP") as constructor:
            transport = boot.ReadOnlyProductTransport("synthetic-meta-token")
        delegate = constructor.return_value
        delegate.login.return_value = "synthetic-session"
        delegate.request.return_value = {"synthetic": True}
        self.assertEqual(transport.login("synthetic@example.test", "synthetic-password"), "synthetic-session")
        delegate.login.assert_called_once_with("synthetic@example.test", "synthetic-password")
        self.assertEqual(transport.request("GET", "/api/me", session="synthetic-session"), {"synthetic": True})
        for method, body in (("POST", None), ("PUT", None), ("DELETE", None), ("PATCH", None),
                             ("get", None), ("GET", {}), ("GET", "RAW_VALUE")):
            before = delegate.request.call_count
            with self.subTest(method=method, body=body), self.assertRaises(common.ReleaseError):
                transport.request(method, "/api/me", body, session="synthetic-session")
            self.assertEqual(delegate.request.call_count, before)
        self.assertFalse(hasattr(transport, "delete"))
        self.assertFalse(hasattr(transport, "rename"))

    def test_fixed_diagnostics_have_exact_shape_stage_enum_and_no_raw_exception_fields(self):
        allowed_codes = {"BOOTSTRAP_CHECK_FAILED", "LEDGER_CREDENTIAL_RESPONSE_REJECTED", "LEDGER_URI_RESPONSE_REJECTED", "HTTP_UNAUTHORIZED",
                         "HTTP_FORBIDDEN", "HTTP_NOT_FOUND", "HTTP_RATE_LIMITED", "HTTP_SERVER_ERROR", "HTTP_REJECTED"}
        sentinel = 'RAW_KEY RAW_VALUE RAW_TOKEN postgresql://RAW_USER:RAW_PASSWORD@RAW_HOST/db RAW_EXCEPTION'
        for stage in boot.STAGES:
            boot.mark_stage(stage)
            for error in (RuntimeError(sentinel), common.ReleaseError(sentinel), KeyError(sentinel)):
                report = boot.failure_report(error)
                self.assertEqual(report, {"schema_version": 1, "outcome": "ERROR", "stage": stage,
                                          "code": "BOOTSTRAP_CHECK_FAILED"})
                public = common.canonical_payload_bytes(report).decode()
                self.assertNotIn("RAW_", public)
                self.assertNotIn("postgresql://", public)
                self.assertIn(report["code"], allowed_codes)
        for status, code in ((400, "HTTP_REJECTED"), (401, "HTTP_UNAUTHORIZED"), (403, "HTTP_FORBIDDEN"),
                             (404, "HTTP_NOT_FOUND"), (429, "HTTP_RATE_LIMITED"), (500, "HTTP_SERVER_ERROR"),
                             (503, "HTTP_SERVER_ERROR")):
            report = boot.failure_report(common.ReleaseError("bounded HTTP operation failed: status " + str(status)))
            self.assertEqual(report["code"], code)
            self.assertEqual(set(report), {"schema_version", "outcome", "stage", "code"})
        self.assertEqual(boot.failure_report(common.ReleaseError("ledger response contains forbidden credentials"))["code"],
                         "LEDGER_CREDENTIAL_RESPONSE_REJECTED")
        for message in ("bounded HTTP operation failed: status 403 " + sentinel,
                        "bounded HTTP operation failed: status 403\n", "bounded HTTP operation failed: status 4030",
                        "ledger response contains forbidden credentials: " + sentinel):
            self.assertEqual(boot.failure_report(common.ReleaseError(message))["code"], "BOOTSTRAP_CHECK_FAILED")

    def test_ledger_diagnostics_distinguish_rejected_field_classes_without_weakening_redactions(self):
        boot.mark_stage("PROVIDER_LEDGER_METADATA")
        for key, expected in (("uri", "LEDGER_URI_RESPONSE_REJECTED"), ("url", "LEDGER_URI_RESPONSE_REJECTED"),
                              ("connection_string", "LEDGER_URI_RESPONSE_REJECTED"),
                              ("password", "LEDGER_CREDENTIAL_RESPONSE_REJECTED"),
                              ("access_token", "LEDGER_CREDENTIAL_RESPONSE_REJECTED"),
                              ("secret", "LEDGER_CREDENTIAL_RESPONSE_REJECTED")):
            for value in ("postgresql://RAW_USER:RAW_PASSWORD@RAW_HOST/db", "RAW_TOKEN", "REDACTED", "********"):
                with self.subTest(key=key, value=value), self.assertRaises(common.ReleaseError) as error:
                    boot.reject_database_credentials({"database": {"connection": {key: value}}})
                self.assertEqual(str(error.exception), "ledger response contains forbidden credentials")
                report = boot.failure_report(error.exception)
                self.assertEqual(report, {"schema_version": 1, "outcome": "ERROR", "stage": "PROVIDER_LEDGER_METADATA",
                                          "code": expected})
                public = common.canonical_payload_bytes(report).decode()
                self.assertNotIn(value, public)
                self.assertNotIn("RAW_", public)
        for absent in (None, ""):
            boot.reject_database_credentials({"connection": {"uri": absent, "password": absent}})
        forged = boot.LedgerCredentialResponseRejected(uri=True)
        forged.code = "RAW_TOKEN"
        self.assertEqual(boot.failure_report(forged)["code"], "LEDGER_CREDENTIAL_RESPONSE_REJECTED")

    def test_diagnostic_stage_and_exception_string_fail_closed(self):
        class HostileReleaseError(common.ReleaseError):
            def __str__(self):
                raise RuntimeError("RAW_EXCEPTION")
        with self.assertRaises(common.ReleaseError):
            boot.mark_stage("RAW_TOKEN")
        with mock.patch.object(boot, "_diagnostic_stage", "RAW_TOKEN"):
            self.assertEqual(boot.failure_report(HostileReleaseError()), {"schema_version": 1, "outcome": "ERROR",
                "stage": "INITIALIZATION", "code": "BOOTSTRAP_CHECK_FAILED"})


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
