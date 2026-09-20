"""Offline bootstrap tests: real policy/rehydration, mocked external transports."""
from __future__ import annotations

import base64
import copy
from contextlib import ExitStack
import datetime as dt
import io
import os
from pathlib import Path
import ssl
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import urllib.error
import urllib.request
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


class FakeProposer:
    def __init__(self):
        self.attempted = False
        self.calls = []
        self.error = None

    def propose(self, spec):
        if self.attempted:
            raise AssertionError("synthetic proposal was already attempted")
        self.attempted = True
        self.calls.append(copy.deepcopy(spec))
        if self.error is not None:
            raise self.error


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.a, self.d, self.protected, self.receipt, self.transport, self.driver = packets()
        self.provider = FakeProvider(self.d["plan"])
        self.writer = FakeWriter()
        self.read_provider = FakeProviderReader(self.provider)
        self.read_environment = FakeMetadataReader(self.writer)
        self.proposer = FakeProposer()
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
            proposer=self.proposer,
            authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
            rehydrate=boot.fixture.rehydrate, transport=self.transport, now=lambda: NOW)

    def test_check_runs_full_real_rehydration_and_second_cas_without_mutation_or_runtime_output(self):
        before = len(self.transport.calls)
        with (mock.patch.object(boot, "runtime_spec", wraps=boot.runtime_spec) as spec,
              mock.patch.object(self.provider, "create", side_effect=AssertionError("check must not create")),
              mock.patch.object(self.writer, "install", side_effect=AssertionError("check must not install"))):
            result = self.check()
        self.assertEqual(result, {"schema_version": 1, "state": "private-driver-prerequisites-verified",
                                  "mutation_performed": False, "app_spec_proposal": "accepted"})
        self.assertGreater(len(self.transport.calls), before)
        self.assertTrue(all(call[0] == "GET" for call in self.transport.calls[before:]))
        self.assertEqual(self.provider.calls, ["preflight", "preflight", "preflight"])
        self.assertEqual(self.writer.calls, ["absent", "absent", "absent"])
        self.assertEqual(self.current.call_count, 4)
        self.authenticate.assert_called_once_with()
        self.image.assert_called_once_with()
        spec.assert_called_once_with(self.a, self.d, self.protected, self.driver)
        self.assertEqual(self.proposer.calls, [boot.runtime_spec(self.a, self.d, self.protected, self.driver)])
        self.assertTrue(self.proposer.attempted)
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
                    proposer=self.proposer,
                    authenticate_fixture=self.authenticate, authenticate_image=self.image, current_guard=self.current,
                    rehydrate=boot.fixture.rehydrate, transport=self.transport, now=lambda: NOW)
            self.assertEqual(self.provider.calls, [])
            self.assertEqual(self.writer.calls, [])
            self.authenticate.assert_not_called()
            self.assertEqual(self.proposer.calls, [])

    def test_check_rejects_proposer_with_unrelated_capabilities_before_preflight(self):
        for capability in ("create", "install", "update", "delete", "get"):
            proposer = FakeProposer()
            setattr(proposer, capability, mock.Mock())
            with self.subTest(capability=capability), self.assertRaises(common.ReleaseError):
                boot.check_once(self.a, self.d, self.protected, provider=self.read_provider,
                    reader=self.read_environment, proposer=proposer,
                    authenticate_fixture=self.authenticate, authenticate_image=self.image,
                    current_guard=self.current, rehydrate=boot.fixture.rehydrate,
                    transport=self.transport, now=lambda: NOW)
            self.assertEqual(proposer.calls, [])
            self.assertEqual(self.provider.calls, [])
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
                self.assertEqual(self.proposer.calls, [])

    def test_check_proposal_failure_still_reconciles_without_any_mutation(self):
        for error in (common.ReleaseError("synthetic proposal rejected"), TimeoutError("PRIVATE-DO-NOT-EMIT")):
            self.setUp()
            self.proposer.error = error
            with self.subTest(kind=type(error).__name__), self.assertRaises(type(error)):
                self.check()
            self.assertEqual(len(self.proposer.calls), 1)
            self.assertEqual(self.provider.calls, ["preflight"] * 3)
            self.assertEqual(self.writer.calls, ["absent"] * 3)
            self.assertEqual(self.current.call_count, 4)
            self.assertEqual(self.provider.create_count, 0)
            self.assertEqual(self.writer.installed, [])
            self.probe.assert_not_called()

    def test_check_postproposal_drift_or_authority_failure_cannot_succeed(self):
        for boundary in ("provider", "environment", "authority", "expiry"):
            self.setUp()
            with self.subTest(boundary=boundary), ExitStack() as stack:
                if boundary == "provider":
                    first = ([{"id": uid(10), "name": "production-app"}],
                             [{"type": "app", "value": uid(10)}])
                    stack.enter_context(mock.patch.object(self.provider, "preflight",
                        side_effect=[first, first, ([], first[1])]))
                elif boundary == "environment":
                    stack.enter_context(mock.patch.object(self.writer, "require_absent",
                        side_effect=[None, None, common.ReleaseError("synthetic metadata drift")]))
                elif boundary == "authority":
                    self.current.side_effect = [None, None, None, common.ReleaseError("synthetic authority drift")]
                now = mock.Mock(side_effect=[NOW, NOW, NOW, NOW + dt.timedelta(hours=2)]) if boundary == "expiry" else lambda: NOW
                with self.assertRaises(common.ReleaseError):
                    boot.check_once(self.a, self.d, self.protected, provider=self.read_provider,
                        reader=self.read_environment, proposer=self.proposer,
                        authenticate_fixture=self.authenticate, authenticate_image=self.image,
                        current_guard=self.current, rehydrate=boot.fixture.rehydrate,
                        transport=self.transport, now=now)
            self.assertEqual(len(self.proposer.calls), 1)
            self.assertEqual(self.provider.create_count, 0)
            self.assertEqual(self.writer.installed, [])

    def test_check_postproposal_drift_overrides_proposal_rejection(self):
        self.proposer.error = boot.ProposalRejected("PROPOSAL_HTTP_REJECTED", 422, ("APP_NAME",))
        first = ([{"id": uid(10), "name": "production-app"}], [{"type": "app", "value": uid(10)}])
        with (mock.patch.object(self.provider, "preflight", side_effect=[first, first, ([], first[1])]),
              self.assertRaises(common.ReleaseError) as caught):
            self.check()
        self.assertNotIsInstance(caught.exception, boot.ProposalRejected)
        self.assertEqual(boot.failure_report(caught.exception), {"schema_version": 1, "outcome": "ERROR",
            "stage": "PROPOSAL_POSTSTATE", "code": "BOOTSTRAP_CHECK_FAILED"})
        self.assertEqual(len(self.proposer.calls), 1)
        self.assertEqual(self.provider.create_count, 0)
        self.assertEqual(self.writer.installed, [])

    def test_install_path_does_not_call_proposal_adapter(self):
        with mock.patch.object(boot, "AppSpecProposer") as proposer:
            self.execute()
        proposer.assert_not_called()
        self.assertEqual(self.proposer.calls, [])

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
                  mock.patch.object(boot, "AppSpecProposer") as proposer,
                  mock.patch.object(boot.fixture, "_pinned_gh") as gh,
                  mock.patch("sys.stdout", new_callable=io.StringIO)):
                self.assertEqual(boot.main(["validate", "--control-root", "."]), 0)
                guard.assert_called_once_with(api.return_value, Path(".").resolve(), mode)
                reader.assert_not_called()
                provider.assert_not_called()
                proposer.assert_not_called()
                gh.assert_not_called()

    def test_check_cli_constructs_only_read_adapters_and_emits_no_artifact(self):
        result = {"schema_version": 1, "state": "private-driver-prerequisites-verified",
                  "mutation_performed": False, "app_spec_proposal": "accepted"}
        with (mock.patch.dict(os.environ, self.cli_env("check", private=True), clear=True),
              mock.patch.object(boot, "GitHubRead"), mock.patch.object(boot, "current_guard") as guard,
              mock.patch.object(boot.fixture, "_pinned_gh", return_value=Path("mocked-pinned-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence"),
              mock.patch.object(boot, "ReadOnlyProductTransport") as transport,
              mock.patch.object(boot, "ReadOnlyProvider") as provider,
              mock.patch.object(boot, "EnvironmentReader") as reader,
              mock.patch.object(boot, "AppSpecProposer") as proposer,
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
            proposer.assert_called_once_with("synthetic-provider-reader")
            reader.assert_called_once_with("synthetic-read-token", self.d["plan"]["github_environment_sha256"])
            transport.assert_called_once_with(self.protected["credentials"]["meta_access_token"])
            self.assertIs(check.call_args.kwargs["provider"], provider.return_value)
            self.assertIs(check.call_args.kwargs["reader"], reader.return_value)
            self.assertIs(check.call_args.kwargs["proposer"], proposer.return_value)
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
                  mock.patch.object(boot, "AppSpecProposer") as proposer,
                  mock.patch.object(boot, "check_once") as check,
                  mock.patch("sys.stdout", new_callable=io.StringIO) as out,
                  mock.patch("sys.stderr", new_callable=io.StringIO) as err):
                self.assertEqual(boot.main(["check", "--control-root", ".", *args]), 1)
                api.assert_not_called()
                check.assert_not_called()
                proposer.assert_not_called()
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
                  mock.patch.object(boot, "AppSpecProposer") as proposer,
                  mock.patch.object(boot, "check_once") as check,
                  mock.patch.object(boot, "install_once") as install,
                  mock.patch("sys.stderr", new_callable=io.StringIO)):
                self.assertEqual(boot.main(["install", "--control-root", "."]), 1)
                api.assert_not_called()
                check.assert_not_called()
                install.assert_not_called()
                proposer.assert_not_called()

    def test_install_cli_never_constructs_proposal_adapter(self):
        env = self.cli_env("install", private=True)
        env.update(DO_DRIVER_BOOTSTRAP_CREATE_TOKEN="synthetic-create",
                   GH_CANARY_ENVIRONMENT_WRITE_TOKEN="synthetic-write", RUNNER_TEMP=str(Path.cwd()))
        receipt = boot.public_receipt(self.a, uid(14), uid(15), "https://synthetic.ondigitalocean.app")
        with (mock.patch.dict(os.environ, env, clear=True),
              mock.patch.object(boot, "GitHubRead"), mock.patch.object(boot, "current_guard"),
              mock.patch.object(boot.fixture, "_pinned_gh", return_value=Path("mocked-pinned-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence"),
              mock.patch.object(boot, "ReadOnlyProductTransport"),
              mock.patch.object(boot, "AppSpecProposer") as proposer,
              mock.patch.object(boot, "Provider"), mock.patch.object(boot, "EnvironmentWriter"),
              mock.patch.object(boot, "check_once") as check,
              mock.patch.object(boot, "install_once", return_value=receipt) as install,
              mock.patch.object(Path, "exists", return_value=False),
              mock.patch.object(Path, "mkdir"), mock.patch.object(Path, "write_bytes"),
              mock.patch.object(Path, "write_text"),
              mock.patch("sys.stdout", new_callable=io.StringIO),
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            self.assertEqual(boot.main(["install", "--control-root", ".", "--output-dir",
                str(Path.cwd() / "synthetic-unused-bootstrap-receipt")]), 0, err.getvalue())
        proposer.assert_not_called()
        check.assert_not_called()
        install.assert_called_once()

    def test_cli_proposal_diagnostic_never_serializes_private_exception_or_response(self):
        sentinel = "RAW-PROPOSAL-PASSWORD-TOKEN-URL-DO-NOT-EMIT"
        failure = boot.ProposalRejected("PROPOSAL_HTTP_REJECTED", 422, ("ENV_KLINIK_LOGIN",))
        failure.args = (sentinel,)
        with (mock.patch.dict(os.environ, self.cli_env("check", private=True), clear=True),
              mock.patch.object(boot, "GitHubRead"), mock.patch.object(boot, "current_guard"),
              mock.patch.object(boot.fixture, "_pinned_gh", return_value=Path("mocked-pinned-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence"),
              mock.patch.object(boot, "ReadOnlyProductTransport"), mock.patch.object(boot, "ReadOnlyProvider"),
              mock.patch.object(boot, "EnvironmentReader"), mock.patch.object(boot, "AppSpecProposer"),
              mock.patch.object(boot, "check_once", side_effect=failure),
              mock.patch.object(boot, "Provider") as create, mock.patch.object(boot, "EnvironmentWriter") as writer,
              mock.patch.object(Path, "mkdir") as mkdir, mock.patch.object(Path, "write_bytes") as write_bytes,
              mock.patch.object(Path, "write_text") as write_text,
              mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            self.assertEqual(boot.main(["check", "--control-root", "."]), 1)
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(common.loads_strict(err.getvalue()), {"schema_version": 1, "outcome": "ERROR",
            "stage": "PROPOSE_APP", "code": "PROPOSAL_HTTP_REJECTED",
            "proposal": {"http_status": 422, "field_mentions": ["ENV_KLINIK_LOGIN"]}})
        self.assertNotIn(sentinel, err.getvalue())
        for unused in (create, writer, mkdir, write_bytes, write_text):
            unused.assert_not_called()

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
              mock.patch.object(boot, "AppSpecProposer"),
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


class LedgerMetadataProjectionTests(unittest.TestCase):
    CONNECTIONS = ('connection', 'private_connection', 'standby_connection', 'standby_private_connection')
    HOST = 'ledger-unit-test.db.ondigitalocean.com'

    def setUp(self):
        _, descriptor, _, _, _, _ = packets()
        self.plan = descriptor['plan']
        self.route = '/v2/databases/' + self.plan['ledger']['cluster_id']
        self.base = provider_responses(self.plan)[self.route]

    def connection(self, **changes):
        value = {'host': self.HOST, 'port': 25060, 'database': 'defaultdb', 'ssl': True,
                 'user': None, 'password': '',
                 'uri': 'postgres://' + self.HOST + ':25060/defaultdb?sslmode=require'}
        value.update(changes)
        return value

    def payload(self, name='connection', **changes):
        value = copy.deepcopy(self.base)
        value['database'][name] = self.connection(**changes)
        return value

    def project(self, value):
        return boot.project_ledger_metadata(value, self.plan['ledger']['cluster_id'])

    def assert_rejected(self, value):
        before = copy.deepcopy(value)
        provider = boot.ReadOnlyProvider(self.plan, 'synthetic-reader-token')
        with (mock.patch.object(boot.fixture, '_wire', return_value=common.canonical_payload_bytes(value)) as wire,
              mock.patch('sys.stdout', new_callable=io.StringIO) as out,
              mock.patch('sys.stderr', new_callable=io.StringIO) as err,
              self.assertRaises(common.ReleaseError) as rejected):
            provider.get(self.route)
        wire.assert_called_once()
        self.assertEqual(wire.call_args.args[1], common.API_ORIGIN + self.route)
        self.assertEqual(wire.call_args.kwargs.get('method', 'GET'), 'GET')
        self.assertNotIn('body', wire.call_args.kwargs)
        self.assertEqual(value, before)
        self.assertEqual(out.getvalue() + err.getvalue(), '')
        for private in (self.HOST, 'postgres://', 'postgresql://', 'RAW_SENTINEL', 'REDACTED'):
            self.assertNotIn(private, str(rejected.exception))
        return rejected.exception

    def test_four_documented_connections_project_to_metadata_without_mutating_input(self):
        self.assertEqual(tuple(boot.CONNECTION_NAMES), self.CONNECTIONS)
        for name in self.CONNECTIONS:
            value = self.payload(name)
            before = copy.deepcopy(value)
            projected = self.project(value)
            self.assertEqual(projected, self.base)
            self.assertEqual(value, before)
            self.assertIsNot(projected, value)
            self.assertIsNot(projected['database'], value['database'])
            self.assertNotIn(self.HOST, common.canonical_payload_bytes(projected).decode())
        all_connections = copy.deepcopy(self.base)
        all_connections['database'].update({name: self.connection() for name in self.CONNECTIONS})
        before = copy.deepcopy(all_connections)
        self.assertEqual(self.project(all_connections), self.base)
        self.assertEqual(all_connections, before)

    def test_optional_connections_and_empty_uri_preserve_original_noncredential_acceptance(self):
        self.assertEqual(self.project(self.base), self.base)
        for name in self.CONNECTIONS:
            for optional in (None, {}, {'uri': None}, {'uri': ''}, {'uri': '', 'user': '', 'password': None}):
                value = copy.deepcopy(self.base)
                value['database'][name] = optional
                self.assertEqual(self.project(value), self.base)
        # A generic cluster connection describes defaultdb, not necessarily the
        # dedicated selected database checked separately by the /dbs endpoint.
        self.assertNotEqual(self.plan['ledger']['database'], 'defaultdb')
        self.assertEqual(self.project(self.payload()), self.base)

    def test_only_three_literal_empty_userinfo_forms_with_empty_credential_siblings_pass(self):
        for name in self.CONNECTIONS:
            for prefix in ('', '@', ':@'):
                for user in ('absent', None, ''):
                    for password in ('absent', None, ''):
                        value = self.payload(name, uri='postgres://' + prefix + self.HOST + ':25060/defaultdb?sslmode=require')
                        connection = value['database'][name]
                        for field, setting in (('user', user), ('password', password)):
                            if setting == 'absent':
                                connection.pop(field)
                            else:
                                connection[field] = setting
                        before = copy.deepcopy(value)
                        with self.subTest(name=name, prefix=prefix, user=user, password=password):
                            self.assertEqual(self.project(value), self.base)
                            self.assertEqual(value, before)

    def test_any_inserted_ascii_or_unicode_userinfo_character_is_rejected(self):
        for character in [chr(number) for number in range(128)] + ['é', '＠', '%40', '%3A', '%00']:
            for prefix in (character + ':@', ':' + character + '@'):
                with self.subTest(prefix=prefix):
                    self.assert_rejected(self.payload(uri='postgres://' + prefix + self.HOST + ':25060/defaultdb?sslmode=require'))

    def test_nonempty_credentials_rejected_in_every_connection_even_literal_redaction(self):
        for name in self.CONNECTIONS:
            for field in ('user', 'password', 'access_token', 'secret', 'client_secret'):
                for value in ('RAW_SENTINEL', 'REDACTED', '********', 0, False, [], {}):
                    with self.subTest(name=name, field=field, value=value):
                        self.assert_rejected(self.payload(name, **{field: value}))

    def test_known_connection_unknown_keys_and_unknown_uri_locations_never_get_exempted(self):
        for field in ('username', 'url', 'connection_string', 'options', 'extra', 'URI'):
            with self.subTest(field=field):
                self.assert_rejected(self.payload(**{field: ''}))
        uri = self.connection()['uri']
        cases = (
            {'outside': {'uri': uri}},
            {'database': {'other_connection': {'uri': uri}}},
            {'database': {'connection': {'nested': {'uri': uri}}}},
            {'database': {'Connection': {'uri': uri}}},
            {'database': {'users': [{'uri': uri}]}},
            {'connection': {'uri': uri}},
        )
        for extra in cases:
            value = copy.deepcopy(self.base)
            if 'database' in extra:
                value['database'].update(extra['database'])
            else:
                value.update(extra)
            with self.subTest(extra=extra):
                self.assert_rejected(value)

    def test_full_payload_late_credential_overrides_earlier_valid_or_null_connection(self):
        for connection in (self.connection(), None):
            for field in ('password', 'access_token', 'client_secret'):
                value = copy.deepcopy(self.base)
                value['database']['connection'] = connection
                value['database']['late_metadata'] = [{'nested': {field: 'RAW_SENTINEL'}}]
                self.assert_rejected(value)
                value = self.payload()
                value['late_response_metadata'] = {field: 'RAW_SENTINEL'}
                self.assert_rejected(value)
        value = self.payload()
        value['database']['standby_private_connection'] = self.connection(password='RAW_SENTINEL')
        self.assert_rejected(value)

    def test_fixed_projection_diagnostics_never_emit_rejected_fields_or_raw_uri(self):
        expected_codes = {'LEDGER_CONNECTION_SHAPE_REJECTED', 'LEDGER_CONNECTION_CREDENTIALS_REJECTED',
                          'LEDGER_URI_USERINFO_REJECTED', 'LEDGER_URI_NONCANONICAL_REJECTED',
                          'LEDGER_RESPONSE_TYPE_REJECTED', 'LEDGER_DATABASE_TYPE_REJECTED', 'LEDGER_CLUSTER_ID_REJECTED',
                          'LEDGER_CONNECTION_TYPE_REJECTED', 'LEDGER_CONNECTION_FIELDS_REJECTED',
                          'LEDGER_URI_TYPE_REJECTED', 'LEDGER_URI_LENGTH_REJECTED', 'LEDGER_HOST_REJECTED',
                          'LEDGER_PORT_REJECTED', 'LEDGER_DATABASE_LABEL_REJECTED', 'LEDGER_SSL_FLAG_REJECTED',
                          'LEDGER_PROTOCOL_TYPE_REJECTED', 'LEDGER_PROTOCOL_VALUE_REJECTED',
                          'LEDGER_APPLICATION_PORTS_TYPE_REJECTED', 'LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED'}
        self.assertEqual(set(boot.LEDGER_METADATA_CODES), expected_codes)
        cases = (
            (self.payload(extra=''), 'LEDGER_CONNECTION_FIELDS_REJECTED'),
            (self.payload(user='RAW_SENTINEL'), 'LEDGER_CONNECTION_CREDENTIALS_REJECTED'),
            (self.payload(uri='postgres://RAW_SENTINEL@' + self.HOST + ':25060/defaultdb?sslmode=require'),
             'LEDGER_URI_USERINFO_REJECTED'),
            (self.payload(uri=self.connection()['uri'] + '#RAW_SENTINEL'), 'LEDGER_URI_NONCANONICAL_REJECTED'),
            (self.payload(password='RAW_SENTINEL'), 'LEDGER_CREDENTIAL_RESPONSE_REJECTED'),
        )
        boot.mark_stage('PROVIDER_LEDGER_METADATA')
        for value, code in cases:
            report = boot.failure_report(self.assert_rejected(value))
            self.assertEqual(report, {'schema_version': 1, 'outcome': 'ERROR',
                                     'stage': 'PROVIDER_LEDGER_METADATA', 'code': code})
            self.assertNotIn('RAW_SENTINEL', common.canonical_payload_bytes(report).decode())
        value = self.payload(uri='postgres://RAW_SENTINEL@' + self.HOST + ':25060/defaultdb?sslmode=require')
        value['late'] = {'password': 'RAW_SENTINEL'}
        self.assertEqual(boot.failure_report(self.assert_rejected(value))['code'], 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')
        with self.assertRaises(common.ReleaseError):
            boot.LedgerMetadataRejected('RAW_SENTINEL')
        forged = boot.LedgerMetadataRejected('LEDGER_CONNECTION_SHAPE_REJECTED')
        forged.code = 'RAW_SENTINEL'
        self.assertEqual(boot.failure_report(forged)['code'], 'BOOTSTRAP_CHECK_FAILED')

    def assert_projection_code(self, value, code):
        before = copy.deepcopy(value)
        boot.mark_stage('PROVIDER_LEDGER_METADATA')
        with (mock.patch('sys.stdout', new_callable=io.StringIO) as out,
              mock.patch('sys.stderr', new_callable=io.StringIO) as err,
              self.assertRaises(common.ReleaseError) as rejected):
            self.project(value)
        report = boot.failure_report(rejected.exception)
        self.assertEqual(report, {'schema_version': 1, 'outcome': 'ERROR',
                                 'stage': 'PROVIDER_LEDGER_METADATA', 'code': code})
        self.assertEqual(value, before)
        self.assertEqual(out.getvalue() + err.getvalue(), '')
        public = common.canonical_payload_bytes(report).decode()
        for received in ('RAW_FIELD_NAME', 'RAW_SENTINEL', self.HOST, 'postgres://', 'postgresql://',
                         self.plan['ledger']['cluster_id']):
            self.assertNotIn(received, public)
            self.assertNotIn(received, str(rejected.exception))

    def optional_payload(self, name, uri, **options):
        value = self.payload(name, uri=uri, **options)
        if uri == 'absent':
            value['database'][name].pop('uri')
        return value

    def test_optional_sdk_fields_accept_exact_protocol_and_empty_ports_combinations(self):
        self.assertEqual(set(boot.CONNECTION_FIELDS), {'uri', 'database', 'host', 'port', 'user', 'password', 'ssl'})
        self.assertEqual(set(boot.CONNECTION_OPTIONAL_FIELDS), {'protocol', 'application_ports'})
        omitted = object()
        for name in self.CONNECTIONS:
            for uri in ('absent', None, '', self.connection()['uri']):
                for protocol in (omitted, None, '', 'postgresql'):
                    for ports in (omitted, None, {}):
                        options = {key: setting for key, setting in (('protocol', protocol), ('application_ports', ports))
                                   if setting is not omitted}
                        value = self.optional_payload(name, uri, **options)
                        before = copy.deepcopy(value)
                        with (self.subTest(name=name, uri=uri, options=options),
                              mock.patch('sys.stdout', new_callable=io.StringIO) as out,
                              mock.patch('sys.stderr', new_callable=io.StringIO) as err):
                            self.assertEqual(self.project(value), self.base)
                            self.assertEqual(value, before)
                            self.assertEqual(out.getvalue() + err.getvalue(), '')

    def test_optional_protocol_type_and_unsupported_values_rejected_before_uri_bypass(self):
        for name in self.CONNECTIONS:
            for uri in ('absent', None, '', self.connection()['uri']):
                for protocol in (False, True, 0, 1, 1.5, [], {}, [''], {'RAW_FIELD_NAME': ''}):
                    with self.subTest(name=name, uri=uri, protocol=protocol):
                        self.assert_projection_code(self.optional_payload(name, uri, protocol=protocol),
                                                    'LEDGER_PROTOCOL_TYPE_REJECTED')
                for protocol in ('postgres', 'pg', 'redis', 'RAW_SENTINEL', ' ', '\x00', 'é', '\u200b', '{}', 'null'):
                    with self.subTest(name=name, uri=uri, protocol=protocol):
                        self.assert_projection_code(self.optional_payload(name, uri, protocol=protocol),
                                                    'LEDGER_PROTOCOL_VALUE_REJECTED')

    def test_optional_application_ports_types_and_nonempty_maps_rejected_before_uri_bypass(self):
        for name in self.CONNECTIONS:
            for uri in ('absent', None, '', self.connection()['uri']):
                for ports in (False, True, 0, 1, 1.5, [], '', 'RAW_SENTINEL', 'é', ['']):
                    with self.subTest(name=name, uri=uri, ports=ports):
                        self.assert_projection_code(self.optional_payload(name, uri, application_ports=ports),
                                                    'LEDGER_APPLICATION_PORTS_TYPE_REJECTED')
                for ports in ({'postgresql': 25060}, {'25060': 25060}, {'RAW_FIELD_NAME': 'RAW_SENTINEL'},
                              {'empty': {}}, {'port': None}, {'port': ''}):
                    with self.subTest(name=name, uri=uri, ports=ports):
                        self.assert_projection_code(self.optional_payload(name, uri, application_ports=ports),
                                                    'LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED')

    def test_optional_fields_do_not_allow_other_extra_or_case_variant_names(self):
        for name in self.CONNECTIONS:
            for extra in ('RAW_FIELD_NAME', 'Protocol', 'applicationPorts', 'application_ports_extra', 'protocol_version'):
                for uri in ('absent', None, '', self.connection()['uri']):
                    value = self.optional_payload(name, uri, protocol='', application_ports={}, **{extra: ''})
                    with self.subTest(name=name, uri=uri, extra=extra):
                        self.assert_projection_code(value, 'LEDGER_CONNECTION_FIELDS_REJECTED')

    def test_optional_field_order_and_recursive_secret_scan_precedence(self):
        cases = (
            ({'RAW_FIELD_NAME': '', 'protocol': False}, 'LEDGER_CONNECTION_FIELDS_REJECTED'),
            ({'user': 'RAW_SENTINEL', 'protocol': False}, 'LEDGER_CONNECTION_CREDENTIALS_REJECTED'),
            ({'protocol': False, 'application_ports': False}, 'LEDGER_PROTOCOL_TYPE_REJECTED'),
            ({'protocol': 'postgres', 'application_ports': False}, 'LEDGER_PROTOCOL_VALUE_REJECTED'),
            ({'protocol': '', 'application_ports': False}, 'LEDGER_APPLICATION_PORTS_TYPE_REJECTED'),
            ({'protocol': '', 'application_ports': {'RAW_FIELD_NAME': ''}}, 'LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED'),
        )
        for name in self.CONNECTIONS:
            for uri in ('absent', None, '', self.connection()['uri']):
                for options, code in cases:
                    value = self.optional_payload(name, uri, **options)
                    with self.subTest(name=name, uri=uri, code=code):
                        self.assert_projection_code(value, code)
                        value['late_metadata'] = {'access_token': 'RAW_SENTINEL'}
                        self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')
                for field in ('password', 'secret', 'access_token'):
                    for option in ('protocol', 'application_ports'):
                        nested = {'nested': [{field: 'RAW_SENTINEL'}]}
                        value = self.optional_payload(name, uri, **{option: nested})
                        with self.subTest(name=name, uri=uri, option=option, nested=field):
                            self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')
                value = self.optional_payload(name, uri, protocol=False,
                    application_ports={'nested': {'uri': 'postgres://RAW_SENTINEL'}})
                self.assert_projection_code(value, 'LEDGER_URI_RESPONSE_REJECTED')
            self.assert_projection_code(self.payload(name, uri=False, protocol=False), 'LEDGER_URI_RESPONSE_REJECTED')
            self.assert_projection_code(self.payload(name, uri='x' * 1025, protocol=False), 'LEDGER_PROTOCOL_TYPE_REJECTED')

    def test_all_optional_connections_are_dropped_by_shared_provider_get_only(self):
        value = copy.deepcopy(self.base)
        value['database'].update({name: self.connection(protocol='', application_ports={}) for name in self.CONNECTIONS})
        before = copy.deepcopy(value)
        for provider in (boot.ReadOnlyProvider(self.plan, 'synthetic-reader-token'),
                         boot.Provider(self.plan, 'synthetic-reader-token', 'synthetic-create-token')):
            with (mock.patch.object(boot.fixture, '_wire', return_value=common.canonical_payload_bytes(value)) as wire,
                  mock.patch('sys.stdout', new_callable=io.StringIO) as out,
                  mock.patch('sys.stderr', new_callable=io.StringIO) as err):
                projected = provider.get(self.route)
                self.assertEqual(projected, self.base)
                self.assertEqual(value, before)
                self.assertEqual(out.getvalue() + err.getvalue(), '')
                wire.assert_called_once()
                self.assertEqual(wire.call_args.args[1], common.API_ORIGIN + self.route)
                self.assertEqual(wire.call_args.kwargs.get('method', 'GET'), 'GET')
                self.assertNotIn(self.HOST, repr(wire.call_args))
                public = common.canonical_payload_bytes(projected).decode()
                for removed in (*self.CONNECTIONS, 'protocol', 'application_ports', self.HOST, 'postgres://'):
                    self.assertNotIn(removed, public)

    def test_typed_empty_sdk_extensions_do_not_bypass_existing_uri_or_credential_checks(self):
        cases = (
            ({'user': 'RAW_SENTINEL'}, 'LEDGER_CONNECTION_CREDENTIALS_REJECTED'),
            ({'password': 'RAW_SENTINEL'}, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED'),
            ({'uri': False}, 'LEDGER_URI_RESPONSE_REJECTED'),
            ({'uri': 'x' * 1025}, 'LEDGER_URI_LENGTH_REJECTED'),
            ({'uri': 'postgres://RAW_SENTINEL@' + self.HOST + ':25060/defaultdb?sslmode=require'},
             'LEDGER_URI_USERINFO_REJECTED'),
            ({'uri': self.connection()['uri'] + '#RAW_SENTINEL'}, 'LEDGER_URI_NONCANONICAL_REJECTED'),
            ({'host': None}, 'LEDGER_HOST_REJECTED'),
            ({'port': False}, 'LEDGER_PORT_REJECTED'),
            ({'database': 'RAW_SENTINEL'}, 'LEDGER_DATABASE_LABEL_REJECTED'),
            ({'ssl': False}, 'LEDGER_SSL_FLAG_REJECTED'),
        )
        for name in self.CONNECTIONS:
            for changes, code in cases:
                with self.subTest(name=name, code=code):
                    self.assert_projection_code(self.payload(name, protocol='', application_ports={}, **changes), code)

    def test_both_literal_uri_aliases_and_exact_protocol_preserve_projection(self):
        omitted = object()
        for name in self.CONNECTIONS:
            for protocol in (omitted, None, '', 'postgresql'):
                for scheme in ('postgres', 'postgresql'):
                    for prefix in ('', '@', ':@'):
                        for sibling in (omitted, None, ''):
                            value = self.payload(name, uri=f'{scheme}://{prefix}{self.HOST}:25060/defaultdb?sslmode=require')
                            connection = value['database'][name]
                            if protocol is not omitted:
                                connection['protocol'] = protocol
                            for field in ('user', 'password'):
                                if sibling is omitted:
                                    connection.pop(field)
                                else:
                                    connection[field] = sibling
                            before = copy.deepcopy(value)
                            legacy = copy.deepcopy(value)
                            legacy['database'][name]['protocol'] = ''
                            legacy['database'][name]['uri'] = f'postgres://{prefix}{self.HOST}:25060/defaultdb?sslmode=require'
                            with (self.subTest(name=name, protocol=protocol, scheme=scheme, prefix=prefix),
                                  mock.patch('sys.stdout', new_callable=io.StringIO) as out,
                                  mock.patch('sys.stderr', new_callable=io.StringIO) as err):
                                self.assertEqual(self.project(value), self.base)
                                self.assertEqual(self.project(value), self.project(legacy))
                                self.assertEqual(value, before)
                                self.assertEqual(out.getvalue() + err.getvalue(), '')

    def test_only_exact_lowercase_postgresql_protocol_is_newly_accepted(self):
        invalid = ('postgres', 'pg', 'PostgreSQL', 'POSTGRESQL', 'Postgresql', 'postgresql ', ' postgresql',
                   'postgresql\t', '\npostgresql', 'postgresql\x00', 'postgre%73ql', 'postgreｓql',
                   'REDACTED', '********', 'RAW_SENTINEL', 'https', 'mysql', 'postgresql://')
        for name in self.CONNECTIONS:
            for scheme in ('postgres', 'postgresql'):
                uri = f'{scheme}://{self.HOST}:25060/defaultdb?sslmode=require'
                for protocol in invalid:
                    with self.subTest(name=name, scheme=scheme, protocol=protocol):
                        self.assert_projection_code(self.payload(name, uri=uri, protocol=protocol),
                                                    'LEDGER_PROTOCOL_VALUE_REJECTED')
                for protocol in (False, 0, [], {}, ['postgresql']):
                    self.assert_projection_code(self.payload(name, uri=uri, protocol=protocol),
                                                'LEDGER_PROTOCOL_TYPE_REJECTED')

    def test_both_uri_aliases_report_same_fixed_userinfo_and_canonical_failures(self):
        for name in self.CONNECTIONS:
            for scheme in ('postgres', 'postgresql'):
                uri = f'{scheme}://{self.HOST}:25060/defaultdb?sslmode=require'
                for prefix in ('user@', 'user:@', ':password@', 'user:password@', '@@', '::@',
                               'REDACTED@', '%40@', '%3A@', 'é:@', '\t:@'):
                    with self.subTest(name=name, scheme=scheme, prefix=prefix):
                        self.assert_projection_code(self.payload(name, protocol='postgresql',
                            uri=f'{scheme}://{prefix}{self.HOST}:25060/defaultdb?sslmode=require'),
                            'LEDGER_URI_USERINFO_REJECTED')
                malformed = (uri + ' ', ' ' + uri, uri + '\x00', uri + '#RAW_SENTINEL', uri + '&extra=1',
                    uri + '&sslmode=require', uri.replace('sslmode=require', 'sslmode=disable'),
                    uri.replace('sslmode=require', 'sslmode=verify-full'),
                    uri.replace(':25060', ':025060'), uri.replace('/defaultdb', '/default%64b'),
                    uri.replace(self.HOST, self.HOST.upper()), uri.replace(self.HOST, self.HOST + '.'),
                    uri.replace(scheme + '://', scheme.upper() + '://'),
                    uri.replace(scheme + '://', 'pgsql://'), uri.replace(scheme + '://', 'https://'))
                for candidate in malformed:
                    with self.subTest(name=name, scheme=scheme, uri=candidate):
                        self.assert_projection_code(self.payload(name, protocol='postgresql', uri=candidate),
                                                    'LEDGER_URI_NONCANONICAL_REJECTED')

    def test_new_alias_and_protocol_do_not_bypass_siblings_tls_ports_or_secret_scan(self):
        changes = (
            ({'host': 'other.db.ondigitalocean.com'}, 'LEDGER_URI_NONCANONICAL_REJECTED'),
            ({'port': 25061}, 'LEDGER_URI_NONCANONICAL_REJECTED'),
            ({'database': 'otherdb'}, 'LEDGER_URI_NONCANONICAL_REJECTED'),
            ({'host': False}, 'LEDGER_HOST_REJECTED'), ({'port': True}, 'LEDGER_PORT_REJECTED'),
            ({'ssl': False}, 'LEDGER_SSL_FLAG_REJECTED'), ({'ssl': 1}, 'LEDGER_SSL_FLAG_REJECTED'),
            ({'user': 'REDACTED'}, 'LEDGER_CONNECTION_CREDENTIALS_REJECTED'),
            ({'password': 'RAW_SENTINEL'}, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED'),
            ({'RAW_FIELD_NAME': ''}, 'LEDGER_CONNECTION_FIELDS_REJECTED'),
            ({'application_ports': False}, 'LEDGER_APPLICATION_PORTS_TYPE_REJECTED'),
            ({'application_ports': {'postgresql': 25060}}, 'LEDGER_APPLICATION_PORTS_NONEMPTY_REJECTED'),
        )
        for name in self.CONNECTIONS:
            for scheme in ('postgres', 'postgresql'):
                uri = f'{scheme}://{self.HOST}:25060/defaultdb?sslmode=require'
                for change, code in changes:
                    value = self.payload(name, protocol='postgresql', uri=uri, **change)
                    with self.subTest(name=name, scheme=scheme, code=code):
                        self.assert_projection_code(value, code)
                        value['late'] = {'nested': [{'secret': 'RAW_SENTINEL'}]}
                        self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')
                self.assert_projection_code(self.payload(name, protocol='postgresql', uri=uri,
                    application_ports={'nested': {'uri': uri}}), 'LEDGER_URI_RESPONSE_REJECTED')

    def test_postgresql_metadata_never_selects_request_or_enters_projection(self):
        value = copy.deepcopy(self.base)
        value['database'].update({name: self.connection(protocol='postgresql', application_ports={},
            uri='postgresql://' + self.HOST + ':25060/defaultdb?sslmode=require') for name in self.CONNECTIONS})
        before = copy.deepcopy(value)
        for provider in (boot.ReadOnlyProvider(self.plan, 'synthetic-reader-token'),
                         boot.Provider(self.plan, 'synthetic-reader-token', 'synthetic-create-token')):
            with mock.patch.object(boot.fixture, '_wire', return_value=common.canonical_payload_bytes(value)) as wire:
                self.assertEqual(provider.get(self.route), self.base)
                wire.assert_called_once()
                self.assertEqual(wire.call_args.args[1], common.API_ORIGIN + self.route)
                self.assertEqual(wire.call_args.kwargs.get('method', 'GET'), 'GET')
                self.assertNotIn('postgresql', repr(wire.call_args))
                self.assertNotIn(self.HOST, repr(wire.call_args))
            self.assertEqual(value, before)

    def test_precise_response_database_and_cluster_identity_codes(self):
        for response in (None, [], '', 'RAW_SENTINEL', False, 1, 1.5):
            with self.subTest(response=response):
                self.assert_projection_code(response, 'LEDGER_RESPONSE_TYPE_REJECTED')
        self.assert_projection_code({}, 'LEDGER_DATABASE_TYPE_REJECTED')
        for database in (None, [], '', 'RAW_SENTINEL', False, 1, 1.5):
            with self.subTest(database=database):
                self.assert_projection_code({'database': database}, 'LEDGER_DATABASE_TYPE_REJECTED')
        self.assert_projection_code({'database': {}}, 'LEDGER_CLUSTER_ID_REJECTED')
        for cluster in (None, '', 'RAW_SENTINEL', uid(99), False, 1, 1.5, [], {}):
            value = self.payload()
            value['database']['id'] = cluster
            with self.subTest(cluster=cluster):
                self.assert_projection_code(value, 'LEDGER_CLUSTER_ID_REJECTED')

    def test_precise_connection_field_codes_in_all_four_documented_locations(self):
        missing = object()
        fields = (
            ('host', (missing, None, False, 1, 1.5, [], {}, '', 'RAW_SENTINEL',
                      'UPPER.db.ondigitalocean.com', 'a' * 254), 'LEDGER_HOST_REJECTED'),
            ('port', (missing, None, False, True, '25060', 1.0, [], {}, -1, 0, 65536), 'LEDGER_PORT_REJECTED'),
            ('database', (missing, None, False, 1, 1.5, [], {}, '', 'a', '1db', 'db-name', 'déb', 'a' * 64),
             'LEDGER_DATABASE_LABEL_REJECTED'),
            ('ssl', (missing, None, False, 1, 1.0, 'true', [], {}), 'LEDGER_SSL_FLAG_REJECTED'),
        )
        for name in self.CONNECTIONS:
            for malformed in ('', 'RAW_SENTINEL', [], False, 1, 1.5):
                value = copy.deepcopy(self.base)
                value['database'][name] = malformed
                with self.subTest(name=name, connection=malformed):
                    self.assert_projection_code(value, 'LEDGER_CONNECTION_TYPE_REJECTED')
            self.assert_projection_code(self.payload(name, RAW_FIELD_NAME='RAW_SENTINEL'),
                                        'LEDGER_CONNECTION_FIELDS_REJECTED')
            self.assert_projection_code(self.payload(name, user='RAW_SENTINEL'),
                                        'LEDGER_CONNECTION_CREDENTIALS_REJECTED')
            for field, rejected_values, code in fields:
                for rejected in rejected_values:
                    value = self.payload(name)
                    if rejected is missing:
                        value['database'][name].pop(field)
                    else:
                        value['database'][name][field] = rejected
                    with self.subTest(name=name, field=field, value='missing' if rejected is missing else rejected):
                        self.assert_projection_code(value, code)

    def test_nonstring_uri_still_rejected_by_earlier_recursive_credential_guard(self):
        # The defensive URI_TYPE code is not reached for malformed JSON values:
        # their nonempty uri slot remains visible to the unchanged earlier guard.
        for name in self.CONNECTIONS:
            for uri in (False, True, 0, 1, 1.5, [], {}, ['RAW_SENTINEL'], {'RAW_FIELD_NAME': 'RAW_SENTINEL'}):
                with self.subTest(name=name, uri=uri):
                    self.assert_projection_code(self.payload(name, uri=uri), 'LEDGER_URI_RESPONSE_REJECTED')
            self.assert_projection_code(self.payload(name, uri='x' * 1025), 'LEDGER_URI_LENGTH_REJECTED')
            self.assert_projection_code(self.payload(name, uri='x' * 1024), 'LEDGER_URI_NONCANONICAL_REJECTED')
            self.assert_projection_code(self.payload(name, uri='x'), 'LEDGER_URI_NONCANONICAL_REJECTED')

    def test_valid_port_host_and_database_boundaries_remain_accepted(self):
        suffix = '.db.ondigitalocean.com'
        prefix = '.'.join(['a' * 63] * 3)
        host_253 = prefix + '.' + 'b' * (253 - len(prefix) - 1 - len(suffix)) + suffix
        self.assertEqual(len(host_253), 253)
        for name in self.CONNECTIONS:
            for port in (1, 65535):
                for database in ('ab', 'a' * 63):
                    value = self.payload(name, host=host_253, port=port, database=database,
                        uri=f'postgres://{host_253}:{port}/{database}?sslmode=require')
                    with self.subTest(name=name, port=port, database_length=len(database)):
                        self.assertEqual(self.project(value), self.base)

    def test_missing_empty_null_uri_bypass_keeps_original_sibling_acceptance(self):
        for name in self.CONNECTIONS:
            for absent in ('missing', None, ''):
                value = self.payload(name, uri=absent, host=['RAW_SENTINEL'], port=False,
                                     database={'RAW_FIELD_NAME': 'RAW_SENTINEL'}, ssl=False)
                if absent == 'missing':
                    value['database'][name].pop('uri')
                before = copy.deepcopy(value)
                with self.subTest(name=name, uri=absent):
                    self.assertEqual(self.project(value), self.base)
                    self.assertEqual(value, before)
                value['database'][name]['password'] = 'RAW_SENTINEL'
                self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')

    def test_precise_diagnostic_order_and_full_payload_credential_precedence(self):
        pairs = (
            ({'RAW_FIELD_NAME': '', 'user': 'RAW_SENTINEL'}, 'LEDGER_CONNECTION_FIELDS_REJECTED'),
            ({'user': 'RAW_SENTINEL', 'host': None}, 'LEDGER_CONNECTION_CREDENTIALS_REJECTED'),
            ({'uri': 'x' * 1025, 'host': None}, 'LEDGER_URI_LENGTH_REJECTED'),
            ({'host': None, 'port': False}, 'LEDGER_HOST_REJECTED'),
            ({'port': False, 'database': None}, 'LEDGER_PORT_REJECTED'),
            ({'database': None, 'ssl': False}, 'LEDGER_DATABASE_LABEL_REJECTED'),
            ({'ssl': False, 'uri': 'postgres://RAW_SENTINEL@' + self.HOST + ':25060/defaultdb?sslmode=require'},
             'LEDGER_SSL_FLAG_REJECTED'),
        )
        for name in self.CONNECTIONS:
            for changes, code in pairs:
                value = self.payload(name, **changes)
                with self.subTest(name=name, first=code):
                    self.assert_projection_code(value, code)
                    value['late_response_metadata'] = {'access_token': 'RAW_SENTINEL'}
                    self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')
        self.assert_projection_code({'database': None, 'late': {'password': 'RAW_SENTINEL'}},
                                    'LEDGER_DATABASE_TYPE_REJECTED')
        value = self.payload()
        value['database']['id'] = uid(99)
        value['late'] = {'password': 'RAW_SENTINEL'}
        self.assert_projection_code(value, 'LEDGER_CLUSTER_ID_REJECTED')
        value = self.payload('connection', host=None)
        value['database']['standby_private_connection'] = self.connection(user='RAW_SENTINEL')
        self.assert_projection_code(value, 'LEDGER_HOST_REJECTED')
        value['database']['standby_private_connection']['password'] = 'RAW_SENTINEL'
        self.assert_projection_code(value, 'LEDGER_CREDENTIAL_RESPONSE_REJECTED')

    def test_all_fixed_diagnostic_codes_keep_four_key_schema_and_reject_forgery(self):
        boot.mark_stage('PROVIDER_LEDGER_METADATA')
        for code in boot.LEDGER_METADATA_CODES:
            error = boot.LedgerMetadataRejected(code)
            report = boot.failure_report(error)
            self.assertEqual(report, {'schema_version': 1, 'outcome': 'ERROR',
                                     'stage': 'PROVIDER_LEDGER_METADATA', 'code': code})
            error.code = 'RAW_SENTINEL'
            self.assertEqual(boot.failure_report(error)['code'], 'BOOTSTRAP_CHECK_FAILED')
        for forged in ('RAW_SENTINEL', '', None, 1):
            with self.subTest(forged=forged), self.assertRaises(common.ReleaseError):
                boot.LedgerMetadataRejected(forged)

    def test_sibling_host_port_database_ssl_and_required_field_drift_rejected(self):
        changes = (
            {'host': 'different.db.ondigitalocean.com'}, {'port': 25061}, {'database': 'differentdb'}, {'ssl': False},
            {'host': None}, {'host': True}, {'host': ''}, {'port': '25060'}, {'port': 25060.0}, {'port': True},
            {'port': 0}, {'port': -1}, {'port': 65536}, {'database': None}, {'database': True}, {'database': ''},
            {'ssl': 'true'}, {'ssl': 1}, {'ssl': None}, {'uri': True}, {'uri': 1}, {'uri': []}, {'uri': {}},
        )
        for change in changes:
            with self.subTest(change=change):
                self.assert_rejected(self.payload(**change))
        for missing in ('host', 'port', 'database', 'ssl'):
            value = self.payload()
            value['database']['connection'].pop(missing)
            self.assert_rejected(value)
        for malformed in ('', 'RAW_SENTINEL', [], 1, True):
            value = copy.deepcopy(self.base)
            value['database']['connection'] = malformed
            self.assert_rejected(value)

    def test_mutually_matching_but_invalid_host_database_or_port_forms_rejected(self):
        hosts = ('localhost', '127.0.0.1', '[::1]', 'foreign.example.test', 'notdb.ondigitalocean.com',
                 'ledger.db.ondigitalocean.com.evil.test', 'UPPER.db.ondigitalocean.com',
                 'ledger.db.ondigitalocean.com.', '-ledger.db.ondigitalocean.com',
                 'ledger-.db.ondigitalocean.com', 'ledger..db.ondigitalocean.com',
                 'ledger_name.db.ondigitalocean.com', 'lédger.db.ondigitalocean.com',
                 'a' * 64 + '.db.ondigitalocean.com', '.'.join(['a' * 63] * 4) + '.db.ondigitalocean.com')
        for host in hosts:
            with self.subTest(host=host):
                self.assert_rejected(self.payload(host=host, uri='postgres://' + host + ':25060/defaultdb?sslmode=require'))
        for database in ('UPPER', '_leading', '1leading', 'has-dash', 'has.dot', 'db/name', 'db name', 'déb', 'a' * 64):
            with self.subTest(database=database):
                self.assert_rejected(self.payload(database=database,
                    uri='postgres://' + self.HOST + ':25060/' + database + '?sslmode=require'))

    def test_uri_parser_normalization_userinfo_query_and_control_adversaries_rejected(self):
        uri = self.connection()['uri']
        bad = (
            uri.replace('postgres:', 'pgsql:'), uri.replace('postgres:', 'POSTGRES:'),
            uri.replace(self.HOST, self.HOST.upper()), uri.replace(self.HOST, self.HOST + '.'),
            uri.replace(self.HOST, 'user@' + self.HOST), uri.replace(self.HOST, 'user:password@' + self.HOST),
            uri.replace(self.HOST, '%40' + self.HOST), uri.replace('defaultdb', 'default%64b'),
            uri.replace(':25060', ':025060'), uri.replace(':25060', ':+25060'), uri.replace(':25060', ':25060.0'),
            uri.replace(':25060', ''), uri.replace('/defaultdb', '//defaultdb'),
            uri.replace('?sslmode=require', ''), uri.replace('sslmode=require', 'sslmode=verify-full'),
            uri.replace('sslmode=require', 'sslmode=disable'), uri.replace('sslmode=require', 'SSLMode=require'),
            uri + '&x=1', uri + '&sslmode=require', uri + '#fragment', uri + '#', uri + '?x=1',
            uri + ' ', ' ' + uri, '\t' + uri, '\r\n' + uri, uri + '\x00',
            uri.replace('postgres://', 'postgres:\\'), uri.replace('ledger', 'led\nger', 1),
            uri.replace('ledger', 'lédger', 1), uri.replace('ledger', 'led%67er', 1),
            'postgres://' + 'x' * 1025,
        )
        for candidate in bad:
            with self.subTest(uri=candidate):
                self.assert_rejected(self.payload(uri=candidate))

    def test_selected_cluster_only_and_both_check_install_adapters_share_same_get(self):
        self.assertIs(boot.Provider.get, boot.ReadOnlyProvider.get)
        for provider in (boot.ReadOnlyProvider(self.plan, 'synthetic-reader-token'),
                         boot.Provider(self.plan, 'synthetic-reader-token', 'synthetic-create-token')):
            value = self.payload()
            with (mock.patch.object(boot.fixture, '_wire', return_value=common.canonical_payload_bytes(value)) as wire,
                  mock.patch.object(boot, 'project_ledger_metadata', wraps=boot.project_ledger_metadata) as project):
                self.assertEqual(provider.get(self.route), self.base)
                project.assert_called_once_with(value, self.plan['ledger']['cluster_id'])
                wire.assert_called_once()
                self.assertEqual(wire.call_args.args[1], common.API_ORIGIN + self.route)
                self.assertNotIn(self.HOST, repr(wire.call_args))
            for route in (self.route + '/firewall', self.route + '/dbs/' + self.plan['ledger']['database']):
                with (mock.patch.object(boot.fixture, '_wire', return_value=common.canonical_payload_bytes(value)),
                      mock.patch.object(boot, 'project_ledger_metadata') as project,
                      self.assertRaises(common.ReleaseError)):
                    provider.get(route)
                project.assert_not_called()
            with mock.patch.object(boot.fixture, '_wire') as wire:
                for route in ('/v2/databases/' + uid(99), self.route + '/users', self.route + '?x=1'):
                    with self.assertRaises(common.ReleaseError):
                        provider.get(route)
                wire.assert_not_called()

    def test_cluster_envelope_identity_and_nonconnection_fields_are_not_hidden(self):
        for value in ({}, {'database': None}, {'database': []}, {'database': 'RAW_SENTINEL'}):
            self.assert_rejected(value)
        value = self.payload()
        value['database']['id'] = uid(99)
        self.assert_rejected(value)
        value = self.payload()
        value['database']['version'] = '99'
        projected = self.project(value)
        self.assertEqual(projected['database']['version'], '99')
        self.assertNotIn('connection', projected['database'])
        # Projection does not conceal ordinary metadata drift from preflight.
        provider = boot.ReadOnlyProvider(self.plan, 'synthetic-reader-token')
        responses = provider_responses(self.plan)
        responses[self.route] = projected
        with mock.patch.object(provider, 'get', side_effect=lambda path: responses[path]), self.assertRaises(common.ReleaseError):
            provider.preflight()


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


class ProposalResponse(io.BytesIO):
    def __init__(self, raw, *, status=200, url=None):
        super().__init__(raw)
        self.status = status
        self.url = boot.PROPOSAL_URL if url is None else url
        self.read_sizes = []

    def getcode(self):
        return self.status

    def geturl(self):
        return self.url

    def read(self, size=-1):
        self.read_sizes.append(size)
        return super().read(size)


class AppSpecProposalTests(unittest.TestCase):
    def setUp(self):
        self.a, self.d, self.protected, _, _, self.driver = packets()
        self.spec = boot.runtime_spec(self.a, self.d, self.protected, self.driver)
        self.accepted = {"app_name_available": True, "spec": copy.deepcopy(self.spec)}
        # The adapter transport is mocked; do not initialize host OpenSSL after
        # clearing Windows runtime variables just to exercise HTTP boundaries.
        self.tls_context = mock.Mock(check_hostname=True, verify_mode=ssl.CERT_REQUIRED, keylog_filename=None)

    def proposer(self, response=None):
        with (mock.patch.dict(os.environ, {}, clear=True),
              mock.patch.object(boot.ssl, "create_default_context", return_value=self.tls_context)):
            proposer = boot.AppSpecProposer("SYNTHETIC-PROPOSAL-READ-TOKEN")
        proposer.opener = mock.Mock()
        if response is not None:
            proposer.opener.open.return_value = response
        return proposer

    def reject(self, response):
        proposer = self.proposer(response)
        with self.assertRaises(boot.ProposalRejected) as caught:
            proposer.propose(self.spec)
        self.assertTrue(proposer.attempted)
        self.assertEqual(proposer.opener.open.call_count, 1)
        self.assertTrue(response.closed)
        return boot.failure_report(caught.exception)

    def test_exact_real_runtime_wrapper_route_headers_and_single_attempt(self):
        response = ProposalResponse(common.canonical_payload_bytes(self.accepted))
        proposer = self.proposer(response)
        original = copy.deepcopy(self.spec)
        def observed(request, *, timeout):
            self.assertTrue(proposer.attempted)
            self.assertEqual(request.full_url, "https://api.digitalocean.com/v2/apps/propose")
            self.assertEqual(request.get_method(), "POST")
            self.assertEqual(timeout, 30)
            self.assertEqual(request.get_header("Authorization"), "Bearer SYNTHETIC-PROPOSAL-READ-TOKEN")
            self.assertEqual(request.get_header("Content-type"), "application/json")
            self.assertEqual(request.data, common.canonical_payload_bytes({"spec": original}))
            self.assertEqual(set(common.loads_strict(request.data)), {"spec"})
            return response
        proposer.opener.open.side_effect = observed
        with (mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err,
              mock.patch.object(Path, "write_bytes") as write,
              mock.patch.object(boot.subprocess, "run") as child):
            self.assertIsNone(proposer.propose(self.spec))
        self.assertEqual(self.spec, original)
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")
        self.assertEqual(response.read_sizes, [boot.MAX_PROPOSAL_BYTES + 1])
        self.assertTrue(response.closed)
        write.assert_not_called()
        child.assert_not_called()
        with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
            proposer.propose(self.spec)
        self.assertEqual(proposer.opener.open.call_count, 1)
        for capability in ("create", "install", "update", "delete", "get"):
            self.assertFalse(hasattr(proposer, capability))
        self.assertFalse(issubclass(boot.AppSpecProposer, boot.ReadOnlyProvider))
        self.assertFalse(issubclass(boot.AppSpecProposer, boot.Provider))

    def test_request_bound_and_serialization_failure_burn_before_transport(self):
        variants = ({**self.spec, "oversized": "x" * boot.MAX_PROPOSAL_BYTES},
                    {**self.spec, "nonfinite": float("nan")}, {**self.spec, "unserializable": object()})
        for spec in variants:
            proposer = self.proposer()
            with self.subTest(kind=next(reversed(spec))), self.assertRaises(Exception):
                proposer.propose(spec)
            self.assertTrue(proposer.attempted)
            proposer.opener.open.assert_not_called()
            with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
                proposer.propose(self.spec)

    def test_http_error_exact_status_closed_fields_and_no_echo(self):
        sentinel = "RAW_PRIVATE_PASSWORD_OR_TOKEN_DO_NOT_EMIT"
        message = sentinel + " spec.services[0].envs.CRM_CANARY_KLINIK_LOGIN_JSON spec.name spec.name"
        stream = io.BytesIO(common.canonical_payload_bytes({"message": message, "request_id": sentinel}))
        error = urllib.error.HTTPError(boot.PROPOSAL_URL, 422, sentinel, {"X-Request-ID": sentinel}, stream)
        proposer = self.proposer()
        proposer.opener.open.side_effect = error
        with self.assertRaises(boot.ProposalRejected) as caught:
            proposer.propose(self.spec)
        report = boot.failure_report(caught.exception)
        self.assertEqual(report, {"schema_version": 1, "outcome": "ERROR", "stage": "PROPOSE_APP",
            "code": "PROPOSAL_HTTP_REJECTED", "proposal": {"http_status": 422,
                "field_mentions": ["APP_NAME", "ENVIRONMENT", "ENV_KLINIK_LOGIN", "SERVICE"]}})
        self.assertNotIn(sentinel, common.canonical_payload_bytes(report).decode())
        self.assertNotIn(sentinel, str(caught.exception))
        self.assertTrue(stream.closed)
        with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
            proposer.propose(self.spec)
        self.assertEqual(proposer.opener.open.call_count, 1)

    def test_all_http_statuses_are_exact_and_never_become_create_authority(self):
        raw = common.canonical_payload_bytes({"message": "PRIVATE spec.name health_check"})
        for status in range(100, 600):
            if status == 200:
                continue
            with self.subTest(status=status):
                report = self.reject(ProposalResponse(raw, status=status))
                self.assertEqual(report["proposal"]["http_status"], status)
                expected = "PROPOSAL_REDIRECT_REJECTED" if 300 <= status < 400 else "PROPOSAL_HTTP_REJECTED"
                self.assertEqual(report["code"], expected)
                self.assertNotIn("PRIVATE", common.canonical_payload_bytes(report).decode())

    def test_unknown_or_nonliteral_field_messages_are_not_echoed_or_promoted(self):
        messages = ("PRIVATE_UNKNOWN_FIELD=value", "xspec.name spec.nameX Xhealth_check health_check_",
                    "SPEC.NAME", ["spec.name"], {"spec.name": "private"}, None)
        for message in messages:
            with self.subTest(kind=type(message).__name__):
                report = self.reject(ProposalResponse(common.canonical_payload_bytes({"message": message}), status=400))
                self.assertEqual(report["proposal"]["field_mentions"], [])
                self.assertEqual(report["code"], "PROPOSAL_HTTP_REJECTED")

    def test_every_reviewed_public_field_literal_maps_only_to_closed_mentions(self):
        for literal, expected in boot.PROPOSAL_FIELDS.items():
            with self.subTest(field=literal):
                raw = common.canonical_payload_bytes({"message": literal + " PRIVATE-DO-NOT-EMIT " + literal})
                report = self.reject(ProposalResponse(raw, status=422))
                self.assertEqual(report["proposal"]["field_mentions"], [expected])
                raw = common.canonical_payload_bytes({"message": "X" + literal + " " + literal + "_PRIVATE"})
                self.assertEqual(self.reject(ProposalResponse(raw, status=422))["proposal"]["field_mentions"], [])

    def test_invalid_json_duplicate_keys_nonfinite_and_oversize_fail_closed(self):
        for raw in (b"<html>PRIVATE</html>", b'{"message":"first","message":"PRIVATE"}',
                    b'{"message":NaN}', b"\xff", b""):
            with self.subTest(raw_size=len(raw)):
                report = self.reject(ProposalResponse(raw, status=422))
                self.assertEqual(report["code"], "PROPOSAL_RESPONSE_SHAPE_REJECTED")
                self.assertEqual(report["proposal"]["http_status"], 422)
        response = ProposalResponse(b"x" * (boot.MAX_PROPOSAL_BYTES + 1), status=422)
        report = self.reject(response)
        self.assertEqual(report["code"], "PROPOSAL_RESPONSE_BOUND_REJECTED")
        self.assertEqual(response.read_sizes, [boot.MAX_PROPOSAL_BYTES + 1])

    def test_transport_exceptions_are_closed_and_never_retried(self):
        for error in (TimeoutError("PRIVATE-TIMEOUT"), OSError("PRIVATE-SOCKET"),
                      ssl.SSLError("PRIVATE-TLS"), RuntimeError("PRIVATE-TRANSPORT")):
            proposer = self.proposer()
            proposer.opener.open.side_effect = error
            with self.subTest(kind=type(error).__name__), self.assertRaises(boot.ProposalRejected) as caught:
                proposer.propose(self.spec)
            self.assertEqual(boot.failure_report(caught.exception), {"schema_version": 1, "outcome": "ERROR",
                "stage": "PROPOSE_APP", "code": "PROPOSAL_TRANSPORT_FAILED",
                "proposal": {"http_status": None, "field_mentions": []}})
            self.assertTrue(proposer.attempted)
            with self.assertRaisesRegex(common.ReleaseError, "already attempted"):
                proposer.propose(self.spec)
            self.assertEqual(proposer.opener.open.call_count, 1)

    def test_redirect_handler_and_response_url_guard_never_read_redirect_body(self):
        for status in (301, 302, 303, 307, 308):
            with self.subTest(status=status), self.assertRaises(boot.ProposalRejected) as caught:
                boot._ProposalNoRedirect().redirect_request(None, None, status, "PRIVATE", {}, "https://other.invalid")
            self.assertEqual(boot.failure_report(caught.exception)["code"], "PROPOSAL_REDIRECT_REJECTED")
        for status, url in ((302, boot.PROPOSAL_URL), (200, "https://other.invalid/propose"),
                            (200, boot.PROPOSAL_URL + "?unexpected=1")):
            response = ProposalResponse(b"PRIVATE-REDIRECT", status=status, url=url)
            report = self.reject(response)
            self.assertEqual(report["code"], "PROPOSAL_REDIRECT_REJECTED")
            self.assertEqual(response.read_sizes, [])

    def test_invalid_response_status_type_or_range_has_no_dynamic_output(self):
        for status in (True, "200", None, 99, 600):
            report = self.reject(ProposalResponse(b"PRIVATE", status=status))
            self.assertEqual(report["code"], "PROPOSAL_RESPONSE_SHAPE_REJECTED")
            self.assertIsNone(report["proposal"]["http_status"])

    def test_accepted_response_must_match_exact_name_service_and_digest(self):
        variants = (
            lambda v: v.update(spec=None),
            lambda v: v.update(app_name_available=False),
            lambda v: v.update(app_name_available=1),
            lambda v: v["spec"].update(name="foreign"),
            lambda v: v["spec"].update(services=[]),
            lambda v: v["spec"]["services"].append(copy.deepcopy(v["spec"]["services"][0])),
            lambda v: v["spec"]["services"].__setitem__(0, "PRIVATE"),
            lambda v: v["spec"]["services"][0].update(name="foreign"),
            lambda v: v["spec"]["services"][0].update(image=None),
            lambda v: v["spec"]["services"][0]["image"].update(digest="sha256:" + "0" * 64),
        )
        expected = ["PROPOSAL_RESPONSE_SHAPE_REJECTED", "PROPOSAL_NAME_UNAVAILABLE", "PROPOSAL_NAME_UNAVAILABLE"]
        for index, change in enumerate(variants):
            value = copy.deepcopy(self.accepted)
            change(value)
            with self.subTest(index=index):
                report = self.reject(ProposalResponse(common.canonical_payload_bytes(value)))
                self.assertEqual(report["code"], expected[index] if index < len(expected) else "PROPOSAL_RESPONSE_IDENTITY_REJECTED")
                self.assertEqual(report["proposal"]["http_status"], 200)

    def test_success_discards_provider_normalized_secret_values(self):
        value = copy.deepcopy(self.accepted)
        value["spec"]["services"][0]["envs"][0]["value"] = "EV[PRIVATE-NORMALIZED-DO-NOT-EMIT]"
        proposer = self.proposer(ProposalResponse(common.canonical_payload_bytes(value)))
        with (mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            self.assertIsNone(proposer.propose(self.spec))
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")
        self.assertNotIn("PRIVATE-NORMALIZED", common.canonical_payload_bytes(self.spec).decode())

    def test_tls_environment_override_and_insecure_context_rejected_before_transport(self):
        for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR"):
            with (self.subTest(name=name), mock.patch.dict(os.environ, {name: "PRIVATE"}, clear=True),
                  mock.patch.object(boot.ssl, "create_default_context") as context,
                  self.assertRaises(common.ReleaseError)):
                boot.AppSpecProposer("synthetic-token")
            context.assert_not_called()
        for attributes in ({"check_hostname": False}, {"verify_mode": ssl.CERT_NONE}, {"keylog_filename": "PRIVATE"}):
            context = mock.Mock(check_hostname=True, verify_mode=ssl.CERT_REQUIRED, keylog_filename=None)
            for name, value in attributes.items():
                setattr(context, name, value)
            with (self.subTest(attributes=tuple(attributes)), mock.patch.dict(os.environ, {}, clear=True),
                  mock.patch.object(boot.ssl, "create_default_context", return_value=context),
                  mock.patch.object(boot.urllib.request, "build_opener") as build,
                  self.assertRaises(common.ReleaseError)):
                boot.AppSpecProposer("synthetic-token")
            build.assert_not_called()
        with (mock.patch.dict(os.environ, {"HTTPS_PROXY": "http://private.invalid"}, clear=True),
              mock.patch.object(boot.ssl, "create_default_context", return_value=self.tls_context) as context,
              mock.patch.object(boot.urllib.request, "build_opener") as build):
            boot.AppSpecProposer("synthetic-token")
        context.assert_called_once_with()
        handlers = build.call_args.args
        self.assertEqual(next(h.proxies for h in handlers if isinstance(h, urllib.request.ProxyHandler)), {})
        self.assertTrue(any(isinstance(h, boot._ProposalNoRedirect) for h in handlers))
        self.assertIs(next(h._context for h in handlers if isinstance(h, urllib.request.HTTPSHandler)), self.tls_context)

    def test_forged_diagnostic_attributes_and_subclasses_cannot_escape_closed_report(self):
        changes = ({"code": "PRIVATE"}, {"http_status": True}, {"http_status": "PRIVATE"},
                   {"http_status": 600}, {"field_mentions": ["APP_NAME"]},
                   {"field_mentions": ("PRIVATE",)}, {"field_mentions": ("APP_NAME", "APP_NAME")},
                   {"field_mentions": ("SERVICE", "APP_NAME")})
        boot.mark_stage("PROPOSE_APP")
        for values in changes:
            error = boot.ProposalRejected("PROPOSAL_HTTP_REJECTED", 422, ("APP_NAME",))
            error.__dict__.update(values)
            with self.subTest(attributes=tuple(values)):
                report = boot.failure_report(error)
                self.assertEqual(report, {"schema_version": 1, "outcome": "ERROR", "stage": "PROPOSE_APP",
                                          "code": "BOOTSTRAP_CHECK_FAILED"})
                self.assertNotIn("PRIVATE", common.canonical_payload_bytes(report).decode())
        class Forged(boot.ProposalRejected):
            pass
        self.assertNotIn("proposal", boot.failure_report(Forged("PROPOSAL_HTTP_REJECTED", 422, ("APP_NAME",))))


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
