"""Mock-only executable adapter tests. No credentials or provider network access."""

from __future__ import annotations

import copy
import hashlib
import io
import json
import sys
import traceback
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

import sanitized_provider_parity as parity


if Path(__file__).resolve().parent.name == "deployment":
    WORKTREE = Path(__file__).resolve().parents[2]
else:
    WORKTREE = Path(__file__).resolve().parents[2] / "rereply-release-controller-rehearsal-20260920"
DEPLOYMENT = WORKTREE / "release" / "deployment"
sys.path.insert(0, str(DEPLOYMENT))
import test_verify_production_plan as fixtures

v = fixtures.verifier
CONTROL = "a" * 40
SECRET_SENTINEL = "synthetic-sensitive-token-not-live-123456789"


class ProviderParityTests(unittest.TestCase):
    def setUp(self):
        base = fixtures.ProductionPlanTests()
        base.setUp()
        self.addCleanup(base.doCleanups)
        self.base = base
        base.spec = fixtures.digest_source_spec()
        self.contract = base.contract
        bootstrap = self.contract["bootstrap_state"]
        bootstrap["source_mode"] = "digest-images"
        bootstrap["images"] = copy.deepcopy(v.BOOTSTRAP_IMAGES)
        bootstrap["canonical_spec_sha256"] = v.sha256_value(base.spec)
        bootstrap["environment_values_sha256"] = v.environment_value_fingerprint(base.spec)
        bootstrap["non_source_projection_sha256"] = v.non_source_fingerprint(base.spec, self.contract)
        bootstrap["genesis_state_sha256"] = v.genesis_state_sha256(self.contract)
        self.app, self.deployment = base.responses()
        self.target_json = json.dumps(base.target)
        self.private = mock.Mock(return_value=(self.target_json, SECRET_SENTINEL))
        self.authenticate = mock.Mock(return_value=None)
        self.loader = mock.patch.object(parity, "_load_reviewed_verifier", return_value=(v, self.contract))
        self.loader.start()
        self.addCleanup(self.loader.stop)

    def opener(self, values=None):
        app_path, active_path = v.provider_paths(self.contract, self.base.target, fixtures.ACTIVE_DEPLOYMENT_ID)
        origin = self.contract["provider"]["api_origin"]
        if values is None:
            values = [self.app, self.deployment, copy.deepcopy(self.app), copy.deepcopy(self.deployment)]
        return fixtures.FakeOpener(values, [origin + app_path, origin + active_path] * 2)

    def invoke(self, *, opener=None, phase="baseline", authenticate=None):
        selected = opener or self.opener()
        with mock.patch.object(v.urllib.request, "build_opener", return_value=selected) as build, \
                mock.patch.object(parity.time, "sleep") as sleep:
            result = parity.require_provider_parity(
                worktree=WORKTREE, control_sha=CONTROL, phase=phase,
                authenticate_predecessor=authenticate or self.authenticate,
                read_private_inputs=self.private,
            )
        return result, selected, sleep, build

    def assert_blocked(self, *, opener=None, phase="baseline", authenticate=None):
        with self.assertRaises(parity.ProviderParityError) as captured:
            self.invoke(opener=opener, phase=phase, authenticate=authenticate)
        self.assertIs(type(captured.exception), parity.ProviderParityError)
        self.assertEqual(str(captured.exception), parity.ERROR_CODE)
        self.assertTrue(captured.exception.__suppress_context__)
        return captured.exception

    def signed_state(self, phase="baseline"):
        bootstrap = self.contract["bootstrap_state"]
        return {
            "authority": "production-phase-state", "repository": self.contract["repository"],
            "control": {"workflow_sha": CONTROL, "workflow_path": parity.STATE_WORKFLOW,
                        "runner_environment": "github-hosted", "run_id": "123", "run_attempt": 1},
            "lineage": {"phase": phase, "to": phase, "phase_ordinal": parity.PHASES.index(phase) + 1},
            "gates": {"deployment_succeeded": True, "migration_succeeded": True, "canary_succeeded": True},
            "provider_state": {
                "app_identity_sha256": self.contract["provider"]["app_id_sha256"],
                "default_ingress_sha256": self.contract["provider"]["default_ingress_sha256"],
                "app_updated_at_sha256": hashlib.sha256(fixtures.APP_UPDATED_AT.encode()).hexdigest(),
                "active_deployment_identity_sha256": bootstrap["active_deployment_id_sha256"],
                "canonical_spec_sha256": bootstrap["canonical_spec_sha256"],
                "environment_values_sha256": bootstrap["environment_values_sha256"],
                "non_source_projection_sha256": bootstrap["non_source_projection_sha256"],
                "source_mode": "digest-images", "images": copy.deepcopy(bootstrap["images"]),
            },
        }

    def test_genesis_four_fixed_gets_and_exact_sixty_second_delay(self):
        result, opener, sleep, build = self.invoke()
        self.assertEqual(result["status"], "provider-parity-verified")
        self.assertEqual(len(opener.requests), 4)
        self.assertTrue(all(r.method == "GET" and r.data is None for r in opener.requests))
        self.assertTrue(all(r.full_url.startswith("https://api.digitalocean.com/v2/apps/") for r in opener.requests))
        self.assertTrue(all(r.get_header("Authorization") == "Bearer " + SECRET_SENTINEL for r in opener.requests))
        sleep.assert_called_once_with(60)
        self.assertEqual(build.call_args.args[0].proxies, {})
        self.assertIsInstance(build.call_args.args[1], v.RejectRedirects)
        self.assertEqual(self.authenticate.call_count, 2)
        self.private.assert_called_once_with()

    def test_report_is_only_fixed_statuses_and_hashes_no_raw_identifiers(self):
        result, _, _, _ = self.invoke()
        expected_statuses = {
            "status": "provider-parity-verified",
            "observation_status": "two-complete-identical-pairs-60s-apart",
            "active_status": "ACTIVE", "source_status": "digest-images",
            "pending_policy_status": "accepted-no-nonnull-pending-fields",
        }
        for key, value in result.items():
            if key in expected_statuses:
                self.assertEqual(value, expected_statuses[key])
            else:
                self.assertRegex(value, r"^[a-f0-9]{40}$" if key.endswith("sha1") else r"^[a-f0-9]{64}$")
        text = json.dumps(result)
        for secret in (SECRET_SENTINEL, fixtures.ACTIVE_DEPLOYMENT_ID, self.base.target["app_id"],
                       self.base.target["default_ingress"], fixtures.APP_UPDATED_AT, "EV["):
            self.assertNotIn(secret, text)
        self.assertNotIn("pending_deployment", result)
        self.assertNotIn("app_name", result)

    def test_pending_policy_distinguishes_omitted_or_null_from_nonnull(self):
        for field in ("pending_deployment", "in_progress_deployment", "pinned_deployment"):
            for value in ({}, False, "", {"id": fixtures.ACTIVE_DEPLOYMENT_ID}):
                with self.subTest(field=field, value=value):
                    app = copy.deepcopy(self.app)
                    app["app"][field] = value
                    self.assert_blocked(opener=self.opener([app, self.deployment, app, self.deployment]))
            self.app["app"][field] = None
        self.invoke()

    def test_authentication_fails_before_private_or_provider_access(self):
        authenticate = mock.Mock(side_effect=RuntimeError(SECRET_SENTINEL))
        opener = self.opener()
        self.assert_blocked(opener=opener, authenticate=authenticate)
        self.private.assert_not_called()
        self.assertEqual(opener.requests, [])

    def test_private_input_and_transport_errors_are_sanitized_without_output(self):
        for boundary in ("private", "transport"):
            with self.subTest(boundary=boundary):
                self.private.side_effect = RuntimeError(SECRET_SENTINEL) if boundary == "private" else None
                opener = self.opener()
                if boundary == "transport":
                    opener.open = mock.Mock(side_effect=OSError(SECRET_SENTINEL))
                stdout, stderr = io.StringIO(), io.StringIO()
                with redirect_stdout(stdout), redirect_stderr(stderr):
                    try:
                        self.invoke(opener=opener)
                    except parity.ProviderParityError:
                        formatted = traceback.format_exc()
                    else:
                        self.fail("expected sanitized error")
                self.assertNotIn(SECRET_SENTINEL, formatted)
                self.assertEqual(stdout.getvalue() + stderr.getvalue(), "")

    def test_private_target_mismatch_extra_keys_and_malformed_token_fail(self):
        cases = [
            (json.dumps({**self.base.target, "app_id": "33333333-3333-4333-8333-333333333333"}), SECRET_SENTINEL),
            (json.dumps({**self.base.target, "default_ingress": "https://other.invalid"}), SECRET_SENTINEL),
            (json.dumps({**self.base.target, "extra": True}), SECRET_SENTINEL),
            (self.target_json, "short"), (self.target_json, SECRET_SENTINEL + "\n"),
            (" " * 4097, SECRET_SENTINEL),
        ]
        for value in cases:
            with self.subTest(value=value):
                self.private.return_value = value
                opener = self.opener()
                self.assert_blocked(opener=opener)
                self.assertEqual(opener.requests, [])

    def test_provider_app_and_active_identity_status_and_spec_drift_fail(self):
        changes = [
            lambda app, dep: app["app"].update(id="33333333-3333-4333-8333-333333333333"),
            lambda app, dep: app["app"].update(default_ingress="https://other.invalid"),
            lambda app, dep: app["app"].pop("updated_at"),
            lambda app, dep: app["app"]["active_deployment"].update(phase="DEPLOYING"),
            lambda app, dep: dep["deployment"].update(phase="PENDING_BUILD"),
            lambda app, dep: dep["deployment"].update(id="33333333-3333-4333-8333-333333333333"),
            lambda app, dep: dep["deployment"]["spec"].update(name="other"),
            lambda app, dep: app["app"]["active_deployment"].update(spec={}),
            lambda app, dep: app["app"].pop("spec"),
        ]
        for change in changes:
            with self.subTest(change=changes.index(change)):
                app, dep = copy.deepcopy(self.app), copy.deepcopy(self.deployment)
                change(app, dep)
                self.assert_blocked(opener=self.opener([app, dep, app, dep]))

    def test_entire_pair_detects_unselected_metadata_drift(self):
        for side in ("app", "deployment"):
            with self.subTest(side=side):
                second_app, second_dep = copy.deepcopy(self.app), copy.deepcopy(self.deployment)
                target = second_app["app"] if side == "app" else second_dep["deployment"]
                target["otherwise_unselected_metadata"] = "changed"
                self.authenticate.reset_mock()
                opener = self.opener([self.app, self.deployment, second_app, second_dep])
                with self.assertRaisesRegex(parity.ProviderNotQuiescent, "^" + parity.NOT_QUIESCENT_CODE + "$"):
                    self.invoke(opener=opener)
                self.assertEqual(len(opener.requests), 4)
                self.assertEqual(self.authenticate.call_count, 2)

    def test_timestamp_only_drift_is_typed_after_fixed_delay_and_reauthentication(self):
        second = copy.deepcopy(self.app)
        second["app"]["updated_at"] = "2026-09-20T10:20:30Z"
        opener = self.opener([self.app, self.deployment, second, self.deployment])
        events = []
        authenticate = mock.Mock(side_effect=lambda: events.append("authenticated"))
        with mock.patch.object(v.urllib.request, "build_opener", return_value=opener), \
                mock.patch.object(parity.time, "sleep", side_effect=lambda seconds: events.append(seconds)):
            with self.assertRaises(parity.ProviderNotQuiescent):
                parity.require_provider_parity(worktree=WORKTREE, control_sha=CONTROL, phase="baseline",
                    authenticate_predecessor=authenticate, read_private_inputs=self.private)
        self.assertEqual(events, ["authenticated", 60, "authenticated"])
        self.assertEqual(len(opener.requests), 4)

    def test_metadata_drift_never_masks_fresh_authentication_failure(self):
        second = copy.deepcopy(self.app)
        second["app"]["updated_at"] = "2026-09-20T10:20:30Z"
        for failure in (RuntimeError(SECRET_SENTINEL), b"changed-predecessor",
                        parity.ProviderNotQuiescent(parity.NOT_QUIESCENT_CODE)):
            with self.subTest(failure_type=type(failure).__name__):
                opener = self.opener([self.app, self.deployment, second, self.deployment])
                self.assert_blocked(opener=opener, authenticate=mock.Mock(side_effect=[None, failure]))
                self.assertEqual(len(opener.requests), 4)

    def test_non_timestamp_state_difference_and_second_round_semantic_failure_are_not_transient(self):
        observed, _ = v.provider_state(self.app, self.deployment, self.contract, self.base.target,
            *v.predecessor_provider_expectation(self.contract, {}, None))
        changed = {**observed, "active_phase": "not-ACTIVE"}
        with mock.patch.object(v, "provider_state", side_effect=[(observed, {}), (changed, {})]):
            self.assert_blocked()
        second = copy.deepcopy(self.app)
        second["app"]["updated_at"] = "2026-09-20T10:20:30Z"
        second["app"]["pending_deployment"] = {}
        self.assert_blocked(opener=self.opener([self.app, self.deployment, second, self.deployment]))

    def test_untrusted_early_transient_exceptions_are_collapsed_to_generic(self):
        self.assert_blocked(authenticate=mock.Mock(side_effect=parity.ProviderNotQuiescent(SECRET_SENTINEL)))
        self.private.side_effect = parity.ProviderNotQuiescent(SECRET_SENTINEL)
        self.assert_blocked()
        self.private.side_effect = None
        opener = self.opener()
        opener.open = mock.Mock(side_effect=parity.ProviderNotQuiescent(SECRET_SENTINEL))
        self.assert_blocked(opener=opener)

    def test_active_rebinding_fails_before_fourth_get(self):
        second = copy.deepcopy(self.app)
        second["app"]["active_deployment"]["id"] = "33333333-3333-4333-8333-333333333333"
        opener = self.opener([self.app, self.deployment, second, self.deployment])
        self.assert_blocked(opener=opener)
        self.assertEqual(len(opener.requests), 3)

    def test_bounded_strict_json_status_content_type_and_redirect_errors(self):
        for case in ("duplicate", "nan", "utf8", "empty", "oversized", "redirect", "status", "content_type"):
            with self.subTest(case=case):
                opener = self.opener()
                response = opener.responses[0]
                raw = {"duplicate": b'{"app":{},"app":{}}', "nan": b'{"app":NaN}',
                       "utf8": b'\xff', "empty": b'', "oversized": b' ' * (v.MAX_JSON_BYTES + 1)}
                if case in raw:
                    response.raw = raw[case]
                elif case == "redirect":
                    response.url = "https://other.invalid/"
                elif case == "status":
                    response.status = 403
                else:
                    response.headers["Content-Type"] = "text/plain"
                self.assert_blocked(opener=opener)
                self.assertEqual(len(opener.requests), 1)

    def test_each_authenticated_predecessor_phase_accepted(self):
        for index, phase in enumerate(parity.PHASES[1:], 1):
            with self.subTest(phase=phase):
                raw = v.canonical_file_bytes(self.signed_state(parity.PHASES[index - 1]))
                authenticate = mock.Mock(return_value=raw)
                report, _, _, _ = self.invoke(phase=phase, authenticate=authenticate)
                self.assertEqual(report["predecessor_state_sha256"], hashlib.sha256(raw).hexdigest())
                self.assertEqual(authenticate.call_count, 2)

    def test_old_control_bad_lineage_identity_environment_and_non_source_fail(self):
        changes = [
            lambda s: s["control"].update(workflow_sha="b" * 40),
            lambda s: s["control"].update(workflow_path=".github/workflows/other.yml"),
            lambda s: s["control"].update(run_attempt=True),
            lambda s: s["control"].update(runner_environment="self-hosted"),
            lambda s: s["lineage"].update(phase="bridge"),
            lambda s: s["lineage"].update(phase_ordinal=True),
            lambda s: s["gates"].update(canary_succeeded=False),
        ]
        changes += [lambda s, key=key: s["provider_state"].update({key: "b" * 64}) for key in
                    ("app_identity_sha256", "default_ingress_sha256", "environment_values_sha256", "non_source_projection_sha256")]
        for change in changes:
            with self.subTest(change=changes.index(change)):
                state = self.signed_state()
                change(state)
                self.private.reset_mock()
                self.assert_blocked(phase="bridge", authenticate=mock.Mock(return_value=v.canonical_file_bytes(state)))
                self.private.assert_not_called()

    def test_image_inventory_repository_subject_duplicates_and_digest_fail(self):
        changes = [
            lambda records: records.pop(),
            lambda records: records.append(copy.deepcopy(records[0])),
            lambda records: records[0].update(repository="ghcr.io/untrusted/image"),
            lambda records: records[0].update(subject="ghcr.io/untrusted/image@sha256:" + "a" * 64),
            lambda records: records[0].update(digest="latest"),
            lambda records: records[1].update(component=records[0]["component"]),
            lambda records: records.reverse(),
            lambda records: records[0].update(extra=True),
        ]
        for change in changes:
            with self.subTest(change=changes.index(change)):
                state = self.signed_state()
                change(state["provider_state"]["images"])
                self.assert_blocked(phase="bridge", authenticate=mock.Mock(return_value=v.canonical_file_bytes(state)))

    def test_live_image_mismatch_still_fails_when_predecessor_spec_hash_matches(self):
        app, dep = copy.deepcopy(self.app), copy.deepcopy(self.deployment)
        app["app"]["spec"]["services"][0]["image"]["digest"] = "sha256:" + "b" * 64
        dep["deployment"]["spec"] = copy.deepcopy(app["app"]["spec"])
        state = self.signed_state()
        state["provider_state"]["canonical_spec_sha256"] = v.sha256_value(app["app"]["spec"])
        self.assert_blocked(phase="bridge", authenticate=mock.Mock(return_value=v.canonical_file_bytes(state)),
                            opener=self.opener([app, dep, app, dep]))

    def test_topology_rejected_even_if_authority_spec_fingerprint_matches(self):
        app, dep = copy.deepcopy(self.app), copy.deepcopy(self.deployment)
        app["app"]["spec"]["services"][0]["http_port"] = 9999
        dep["deployment"]["spec"] = copy.deepcopy(app["app"]["spec"])
        bootstrap = self.contract["bootstrap_state"]
        bootstrap["canonical_spec_sha256"] = v.sha256_value(app["app"]["spec"])
        bootstrap["non_source_projection_sha256"] = v.non_source_fingerprint(app["app"]["spec"], self.contract)
        bootstrap["genesis_state_sha256"] = v.genesis_state_sha256(self.contract)
        self.assert_blocked(opener=self.opener([app, dep, app, dep]))

    def test_live_environment_and_non_source_drift_rejected_with_matching_spec_hash(self):
        for field in ("environment", "non_source"):
            with self.subTest(field=field):
                app, dep = copy.deepcopy(self.app), copy.deepcopy(self.deployment)
                spec = app["app"]["spec"]
                if field == "environment":
                    spec["services"][0]["envs"][0]["value"] = "synthetic-environment-drift"
                else:
                    spec["services"][0]["instance_count"] += 1
                dep["deployment"]["spec"] = copy.deepcopy(spec)
                bootstrap = self.contract["bootstrap_state"]
                bootstrap["canonical_spec_sha256"] = v.sha256_value(spec)
                bootstrap["genesis_state_sha256"] = v.genesis_state_sha256(self.contract)
                self.assert_blocked(opener=self.opener([app, dep, app, dep]))

    def test_second_authentication_change_or_error_fails_after_four_gets(self):
        for refreshed in (RuntimeError(SECRET_SENTINEL), b"changed"):
            with self.subTest(refreshed=type(refreshed).__name__):
                authenticate = mock.Mock(side_effect=[None, refreshed])
                opener = self.opener()
                self.assert_blocked(opener=opener, authenticate=authenticate)
                self.assertEqual(len(opener.requests), 4)

    def test_no_predecessor_file_flag_or_genesis_fallback(self):
        for phase, payload in (("bridge", None), ("bridge", {"verified": True}),
                               ("bridge", b'{}'), ("bridge", b' ' * 131073),
                               ("baseline", v.canonical_file_bytes(self.signed_state()))):
            with self.subTest(phase=phase, payload_type=type(payload).__name__):
                self.assert_blocked(phase=phase, authenticate=mock.Mock(return_value=payload))


class PinnedLoadingTests(unittest.TestCase):
    def test_private_environment_reader_is_lazy_consuming_and_memory_cached(self):
        inputs = {"DO_PRODUCTION_TARGET_JSON": "synthetic-target-json", "DO_PRODUCTION_READ_TOKEN": SECRET_SENTINEL}
        with mock.patch.dict(parity.os.environ, inputs, clear=True):
            reader = parity.make_environment_private_input_reader()
            self.assertEqual(dict(parity.os.environ), inputs)
            self.assertEqual(reader(), ("synthetic-target-json", SECRET_SENTINEL))
            self.assertEqual(dict(parity.os.environ), {})
            self.assertEqual(reader(), ("synthetic-target-json", SECRET_SENTINEL))

    def test_private_environment_reader_missing_inputs_does_not_fallback(self):
        for values in ({}, {"DO_PRODUCTION_READ_TOKEN": SECRET_SENTINEL},
                       {"DO_PRODUCTION_TARGET_JSON": "synthetic-target-json"},
                       {"DIGITALOCEAN_ACCESS_TOKEN": SECRET_SENTINEL, "DO_TOKEN": SECRET_SENTINEL}):
            with self.subTest(keys=sorted(values)):
                with mock.patch.dict(parity.os.environ, values, clear=True):
                    reader = parity.make_environment_private_input_reader()
                    with self.assertRaisesRegex(parity.ProviderParityError, "^" + parity.ERROR_CODE + "$"):
                        reader()

    def test_actual_candidate_hashes_load_without_credentials_or_network(self):
        with mock.patch("urllib.request.build_opener", side_effect=AssertionError("network forbidden")):
            loaded, contract = parity._load_reviewed_verifier(WORKTREE)
        self.assertEqual(contract["bootstrap_state"]["source_mode"], "digest-images")
        self.assertEqual(loaded.__name__, "_reviewed_provider_verifier")

    def test_modified_verifier_or_contract_bytes_rejected_before_execution(self):
        source = (DEPLOYMENT / "verify_production_plan.py").read_bytes()
        contract = (DEPLOYMENT / "production-app-contract.json").read_bytes()
        for pair in ((source + b"\n", contract), (source, contract + b"\n")):
            with self.subTest(which=0 if pair[0] != source else 1):
                with mock.patch.object(Path, "open", side_effect=[io.BytesIO(pair[0]), io.BytesIO(pair[1])]), \
                        mock.patch("builtins.exec", side_effect=AssertionError("must not execute")) as execute:
                    with self.assertRaises(parity.ProviderParityError):
                        parity._load_reviewed_verifier(WORKTREE)
                    execute.assert_not_called()

    def test_import_has_no_network_credentials_sleep_or_file_operations(self):
        source = Path(parity.__file__).read_text(encoding="utf-8")
        ambient = mock.MagicMock()
        for method in ("get", "pop", "__getitem__", "__iter__", "__contains__"):
            getattr(ambient, method).side_effect = AssertionError("environment access forbidden")
        with mock.patch.object(Path, "open", side_effect=AssertionError("file access forbidden")) as file_access, \
                mock.patch("urllib.request.build_opener", side_effect=AssertionError("network forbidden")) as network, \
                mock.patch.object(parity.os, "environ", ambient), \
                mock.patch.object(parity.time, "sleep", side_effect=AssertionError("sleep forbidden")) as sleep:
            exec(compile(source, "import-safety-test", "exec"), {"__name__": "import_safety_test"})
        file_access.assert_not_called()
        network.assert_not_called()
        sleep.assert_not_called()
        self.assertEqual(ambient.mock_calls, [])


if __name__ == "__main__":
    unittest.main()
