"""Offline contracts for the protected existing-app recovery runner.

All inputs are synthetic. Inherited transport/process tripwires forbid real
provider calls, old installer execution, credential access, and file writes.
"""
from __future__ import annotations

import ast
import copy
import datetime as dt
import io
import os
from pathlib import Path
import re
from types import SimpleNamespace
import unittest
from unittest import mock

try:
    from . import run_existing_crm_canary_driver_recovery as runner
    from . import test_recover_production_crm_canary_driver as fixtures
except ImportError:
    import run_existing_crm_canary_driver_recovery as runner
    import test_recover_production_crm_canary_driver as fixtures

common = fixtures.common
boot = fixtures.boot
kernel = fixtures.recovery
NOW = fixtures.NOW
CONTROL = fixtures.CONTROL
PRIVATE = fixtures.PRIVATE
uid = fixtures.uid
REAL_SUBPROCESS_RUN = boot.subprocess.run
REAL_PATH_WRITE_BYTES = Path.write_bytes
REAL_PATH_WRITE_TEXT = Path.write_text


class RunnerFixtures(fixtures.RecoveryFixtures):
    def setUp(self):
        super().setUp()
        patch = mock.patch.object(Path, "mkdir", side_effect=AssertionError("offline directory write crossed"))
        patch.start()
        self.addCleanup(patch.stop)

    def packet(self, mode="check"):
        value = copy.deepcopy(self.case["authorization"])
        value.update({
            "kind": "production-crm-canary-driver-existing-recovery-v1",
            "mode": mode,
            "binding_sha256": "0" * 64,
            "check_run_id": None,
            "check_operation_id": None,
            "production_history_sha256": "6" * 64,
        })
        value["effects"]["ledger_table_initialization"] = mode == "recover"
        if mode == "recover":
            value["operation_id"] = uid(24)
            value["check_run_id"] = "76543"
            value["check_operation_id"] = self.case["authorization"]["operation_id"]
            for key in ("app_updates", "deployments", "secret_updates"):
                value["effects"][key] = 1
        value["binding_sha256"] = runner.binding_hash(value)
        return value

    def validate(self, value):
        return runner.validate_authorization(value, control_sha=CONTROL, now=NOW)

    def rejected(self, value):
        with self.assertRaises(common.ReleaseError):
            self.validate(value)

    def execution(self, mode="check"):
        owner = self
        packet = self.packet(mode)
        events = []
        with mock.patch.object(fixtures.samples, "NOW", fixtures.ORIGIN_TIME):
            _, _, _, receipt, transport, _ = fixtures.samples.packets()
        self.assertTrue(common.sha256_bytes(receipt)
                        == self.case["original"]["fixture_evidence"]["result_sha256"])
        state = {"written": False, "update_count": 0, "reconcile_count": 0,
                 "plans": [], "configs": [], "snapshot_count": 0, "observe_count": 0}
        target_id = uid(25)
        origin = "https://" + self.case["descriptor"]["plan"]["app_name"] + "-abc.ondigitalocean.app"

        class Reader:
            def require_absent(self):
                events.append("require_absent")
                if state["written"]:
                    raise common.ReleaseError("synthetic secret already present")

        class Provider:
            def snapshot(self):
                events.append("snapshot")
                state["snapshot_count"] += 1
                return copy.deepcopy(owner.case["first"] if state["snapshot_count"] == 1 else owner.case["second"])

            def reconcile(self):
                events.append("reconcile")
                state["reconcile_count"] += 1

        class Updater(Provider):
            def update(self, plan):
                events.append("update")
                state["update_count"] += 1
                if state["update_count"] != 1:
                    raise AssertionError("synthetic PUT retried")
                state["plans"].append(copy.deepcopy(plan))
                spec = copy.deepcopy(plan)
                for row in spec["services"][0]["envs"]:
                    if row["key"] in kernel.LOGIN_KEYS:
                        row["value"] = "EV[new-synthetic-ciphertext-" + row["key"] + "]"
                state["stored_spec"] = spec
                pending = {"id": target_id, "phase": "PENDING_DEPLOY", "spec": copy.deepcopy(spec),
                           "created_at": fixtures.stamp(NOW), "updated_at": fixtures.stamp(NOW)}
                app = copy.deepcopy(owner.case["second"]["app"])
                app.update(spec=copy.deepcopy(spec), updated_at=fixtures.stamp(NOW), pending_deployment=pending)
                return {"app": app}

            def observe(self, identity):
                events.append("observe")
                state["observe_count"] += 1
                if identity != target_id:
                    raise AssertionError("unbound synthetic deployment selected")
                value = copy.deepcopy(owner.case["second"])
                value["observed_at"] = fixtures.stamp(NOW)
                spec = copy.deepcopy(state["stored_spec"])
                selected = {"id": target_id, "phase": "ACTIVE", "spec": copy.deepcopy(spec),
                            "created_at": fixtures.stamp(NOW), "updated_at": fixtures.stamp(NOW)}
                value["deployments"]["deployments"].append(selected)
                value["deployments"]["meta"]["total"] = 3
                value["app"].update(spec=spec, updated_at=fixtures.stamp(NOW), default_ingress=origin,
                                     live_url=origin, live_domain=origin.removeprefix("https://"),
                                     active_deployment={"id": target_id, "phase": "ACTIVE"})
                if state["written"]:
                    value["canary_environment"]["secrets"].append({
                        "name": boot.SECRET_NAME, "created_at": fixtures.stamp(NOW),
                        "updated_at": fixtures.stamp(NOW)})
                return value

        class Writer:
            def install(self, config):
                events.append("install")
                if state["written"]:
                    raise AssertionError("synthetic secret PUT retried")
                state["configs"].append(copy.deepcopy(config))
                state["written"] = True

        def callback(event, value=None):
            def called(*_args):
                events.append(event)
                return copy.deepcopy(value)
            return called

        provider = Provider() if mode == "check" else Updater()
        callbacks = {
            "provider": provider, "reader": Reader(),
            "authenticate_origin": callback("authenticate_origin", (self.case["origin_run"], self.case["origin_artifacts"])),
            "authenticate_fixture": callback("authenticate_fixture", receipt),
            "authenticate_image": callback("authenticate_image"),
            "current_guard": callback("current_guard"), "rehydrate": callback("rehydrate", self.driver),
            "transport": transport, "now": lambda: NOW,
            "sleep": lambda seconds: events.append(("sleep", seconds)),
            "health": callback("health"), "writer": None if mode == "check" else Writer(),
        }
        return {"packet": packet, "callbacks": callbacks, "events": events, "state": state,
                "target_id": target_id, "origin": origin}

    def run_execution(self, test):
        return runner.run_once(test["packet"], self.case["original"], self.case["descriptor"],
                               self.protected, **test["callbacks"])

    def assert_no_private_report(self, value):
        encoded = common.canonical_payload_bytes(value).decode()
        private = [PRIVATE, self.app_id, self.user_id, self.team_id,
                   self.case["descriptor"]["hmac_key_base64"],
                   self.protected["credentials"]["klinik_password"],
                   self.protected["credentials"]["non_klinik_password"]]
        private += [row["value"] for row in self.case["second"]["app"]["spec"]["services"][0]["envs"]
                    if row["type"] == "SECRET"]
        for value in private:
            self.assertNotIn(value, encoded)


class AuthorizationTests(RunnerFixtures):
    def test_check_and_recover_are_separate_operations_with_same_frozen_binding(self):
        check, recover = self.packet(), self.packet("recover")
        self.assertEqual(check["binding_sha256"], recover["binding_sha256"])
        self.assertNotEqual(check["operation_id"], recover["operation_id"])
        for value in (check, recover):
            original = copy.deepcopy(value)
            result = self.validate(value)
            self.assertEqual(value, original)
            self.assertEqual(result, original)
            self.assertIsNot(result, value)
        self.assertEqual(runner.run_title(check),
                         "Check existing CRM driver " + check["operation_id"] + " " + check["binding_sha256"])
        self.assertEqual(runner.run_title(recover),
                         "Recover existing CRM driver " + recover["operation_id"] + " " + recover["binding_sha256"])

    def test_binding_excludes_only_the_named_mode_operation_and_time_fields(self):
        value = self.packet()
        excluded = {"kind", "mode", "operation_id", "issued_at", "expires_at", "effects",
                    "binding_sha256", "check_run_id", "check_operation_id"}
        expected = {key: item for key, item in value.items() if key not in excluded}
        self.assertEqual(runner.binding_hash(value), common.sha256_value(expected))
        for key in ("production_state_sha256", "fixture_environment_sha256", "firewall_sha256",
                    "driver_metadata_sha256", "production_history_sha256", "apps_inventory_sha256",
                    "origin_authorization_sha256"):
            changed = copy.deepcopy(value)
            changed[key] = "0" * 64
            with self.subTest(key=key):
                self.assertNotEqual(runner.binding_hash(changed), value["binding_sha256"])
                self.rejected(changed)
        changed = copy.deepcopy(value)
        changed["target_driver_evidence"]["digest"] = "sha256:" + "0" * 64
        self.assertNotEqual(runner.binding_hash(changed), value["binding_sha256"])
        self.rejected(changed)

    def test_exact_modes_keys_kind_current_control_and_binding(self):
        for field, replacement in (("mode", "install"), ("mode", None), ("kind", "other"),
                                   ("control_sha", "a" * 40), ("schema_version", True),
                                   ("binding_sha256", "0" * 64)):
            value = self.packet()
            value[field] = replacement
            with self.subTest(field=field):
                self.rejected(value)
        value = self.packet()
        value["unexpected"] = PRIVATE
        self.rejected(value)
        value = self.packet()
        del value["check_operation_id"]
        self.rejected(value)

    def test_check_never_has_predecessor_selectors_and_recover_requires_both(self):
        for field, replacement in (("check_run_id", "76543"), ("check_operation_id", uid(24))):
            value = self.packet()
            value[field] = replacement
            with self.subTest(field=field):
                self.rejected(value)
        for field, replacements in (("check_run_id", (None, "", True, "latest", "0")),
                                    ("check_operation_id", (None, "", True, "not-a-uuid"))):
            for replacement in replacements:
                value = self.packet("recover")
                value[field] = replacement
                with self.subTest(field=field, value_type=type(replacement).__name__):
                    self.rejected(value)
        value = self.packet("recover")
        value["check_operation_id"] = value["operation_id"]
        self.rejected(value)

    def test_effects_are_exact_integer_budgets_for_each_mode(self):
        for mode in ("check", "recover"):
            baseline = self.packet(mode)
            for field in baseline["effects"]:
                if field == "ledger_table_initialization":
                    continue
                for replacement in (True, False, "1", 1.0, -1, 2):
                    value = copy.deepcopy(baseline)
                    value["effects"][field] = replacement
                    with self.subTest(mode=mode, field=field, value_type=type(replacement).__name__):
                        self.rejected(value)
            for field in ("app_creations", "firewall_updates", "synthetic_executions"):
                value = copy.deepcopy(baseline)
                value["effects"][field] = 1
                self.rejected(value)
            for replacement in (0, 1, None, "true", not (mode == "recover")):
                value = copy.deepcopy(baseline)
                value["effects"]["ledger_table_initialization"] = replacement
                self.rejected(value)

    def test_current_window_cannot_reuse_expired_original_authority(self):
        for issued, expires in (
            (NOW - dt.timedelta(hours=2), NOW),
            (NOW + dt.timedelta(seconds=1), NOW + dt.timedelta(hours=1)),
            (NOW, NOW),
            (NOW - dt.timedelta(seconds=1), NOW + dt.timedelta(hours=2)),
        ):
            value = self.packet()
            value["issued_at"], value["expires_at"] = fixtures.stamp(issued), fixtures.stamp(expires)
            self.rejected(value)
        value = self.packet()
        value["issued_at"] = self.case["original"]["issued_at"]
        value["expires_at"] = self.case["original"]["expires_at"]
        self.rejected(value)


class CheckExecutionTests(RunnerFixtures):
    def test_check_authenticates_old_fixture_and_new_image_without_mutation_capability(self):
        test = self.execution()
        before = copy.deepcopy(self.case)
        result = self.run_execution(test)
        self.assertEqual(self.case, before)
        self.assertEqual(result["state"], "existing-driver-check-complete")
        self.assertEqual(result["binding_sha256"], test["packet"]["binding_sha256"])
        self.assertIs(result["mutation_performed"], False)
        self.assertIs(result["ready_for_operation"], False)
        self.assertEqual(test["events"][:6], ["current_guard", "authenticate_origin", "authenticate_fixture",
                                              "authenticate_image", "rehydrate", "require_absent"])
        self.assertEqual(test["state"]["snapshot_count"], 2)
        self.assertNotIn("update", test["events"])
        self.assertNotIn("install", test["events"])
        self.assertNotIn("health", test["events"])
        self.assert_no_private_report(result)

    def test_check_rejects_writer_or_update_capability_before_any_authenticated_work(self):
        for key in ("writer", "provider"):
            test = self.execution()
            test["callbacks"][key] = mock.Mock()
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["events"], [])

    def test_authentication_and_current_guard_failure_never_reaches_mutation(self):
        for mode in ("check", "recover"):
            for callback in ("authenticate_origin", "authenticate_fixture", "authenticate_image", "current_guard", "rehydrate"):
                test = self.execution(mode)
                test["callbacks"][callback] = mock.Mock(side_effect=common.ReleaseError(PRIVATE))
                with self.subTest(mode=mode, callback=callback), self.assertRaises(common.ReleaseError):
                    self.run_execution(test)
                self.assertEqual(test["state"]["update_count"], 0)
                self.assertFalse(test["state"]["written"])

    def test_original_run_or_artifact_drift_cannot_authenticate_history(self):
        for field, replacement in (("run_attempt", 2), ("head_sha", CONTROL),
                                   ("display_title", "other"), ("conclusion", "success")):
            test = self.execution()
            run = copy.deepcopy(self.case["origin_run"])
            run[field] = replacement
            test["callbacks"]["authenticate_origin"] = lambda: (run, self.case["origin_artifacts"])
            with self.subTest(field=field), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
        test = self.execution()
        test["callbacks"]["authenticate_origin"] = lambda: (self.case["origin_run"], {"total_count": 1, "artifacts": [{}]})
        with self.assertRaises(common.ReleaseError):
            self.run_execution(test)

    def test_fixture_bytes_and_rehydrated_descriptor_remain_original_bound(self):
        for callback, value in (("authenticate_fixture", b"{}"), ("authenticate_fixture", PRIVATE),
                                ("rehydrate", {"changed": PRIVATE})):
            test = self.execution()
            test["callbacks"][callback] = lambda *_args: value
            with self.subTest(callback=callback), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["snapshot_count"], 0)

    def test_second_snapshot_resource_history_and_firewall_drift_fail_before_update(self):
        for path, value in (
            (("app", "id"), uid(40)),
            (("deployments", "meta", "total"), 3),
            (("deployments", "deployments", 0, "phase"), "ACTIVE"),
            (("firewall", "rules"), [{"type": "app", "value": self.app_id}]),
            (("production_state_sha256",), "0" * 64),
            (("fixture_environment_sha256",), "0" * 64),
        ):
            test = self.execution("recover")
            original_snapshot = test["callbacks"]["provider"].snapshot
            def changed():
                snapshot = original_snapshot()
                if test["state"]["snapshot_count"] == 2:
                    fixtures.put(snapshot, path, value)
                return snapshot
            test["callbacks"]["provider"].snapshot = changed
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["update_count"], 0)
            self.assertFalse(test["state"]["written"])


class PrestateDiagnosticTests(RunnerFixtures):
    def test_each_read_only_orchestration_boundary_has_fixed_code_and_no_effects(self):
        for failure, expected in (("canary", "CANARY_ABSENCE"), ("first", "FIRST_SNAPSHOT"),
                                  ("second", "SECOND_SNAPSHOT"), ("guard", "PRESTATE_GUARD")):
            test = self.execution()
            callbacks = test["callbacks"]
            if failure == "canary":
                callbacks["reader"].require_absent = mock.Mock(side_effect=RuntimeError(PRIVATE))
            elif failure in ("first", "second"):
                results = [RuntimeError(PRIVATE)] if failure == "first" else [copy.deepcopy(self.case["first"]), RuntimeError(PRIVATE)]
                callbacks["provider"].snapshot = mock.Mock(side_effect=results)
            else:
                callbacks["current_guard"] = mock.Mock(side_effect=[None, RuntimeError(PRIVATE)])
            with self.subTest(failure=failure), self.assertRaises(kernel.PrestateRejected) as caught:
                self.run_execution(test)
            report = runner.failure_report(caught.exception)
            self.assertEqual(report, {"schema_version": 1, "state": "existing-driver-recovery-stopped",
                "code": "RECOVERY_STOPPED_RECONCILE_ONLY", "stage": "PROVIDER_PRESTATE",
                "retry_authorized": False, "diagnostic_code": expected})
            self.assertEqual(test["state"]["update_count"], 0)
            self.assertEqual(test["state"]["configs"], [])
            self.assert_no_private_report(report)

    def test_inner_prestate_diagnostic_is_not_erased_by_outer_snapshot_boundary(self):
        test = self.execution()
        test["callbacks"]["provider"].snapshot = mock.Mock(side_effect=kernel.PrestateRejected("ACCOUNT_READ"))
        with self.assertRaises(kernel.PrestateRejected) as caught:
            self.run_execution(test)
        self.assertEqual(runner.failure_report(caught.exception)["diagnostic_code"], "ACCOUNT_READ")
        self.assertEqual(test["state"]["update_count"], 0)

    def test_public_diagnostic_ignores_hostile_subclasses_unknown_codes_and_other_stages(self):
        class Hostile(kernel.PrestateRejected):
            @property
            def code(self):
                raise AssertionError("private property read")
            def __str__(self):
                raise AssertionError("private exception formatted")
        hostile = Hostile.__new__(Hostile)
        Exception.__init__(hostile, PRIVATE)
        with mock.patch.object(runner, "CURRENT_STAGE", "PROVIDER_PRESTATE"):
            for error in (hostile, RuntimeError(PRIVATE)):
                self.assertNotIn("diagnostic_code", runner.failure_report(error))
            for value in (PRIVATE, None, True, object(), [PRIVATE], {PRIVATE: PRIVATE}):
                error = kernel.PrestateRejected("ACCOUNT_READ")
                error.code = value
                report = runner.failure_report(error)
                self.assertNotIn("diagnostic_code", report)
                self.assert_no_private_report(report)
            for code in kernel.PRESTATE_DIAGNOSTICS:
                report = runner.failure_report(kernel.PrestateRejected(code))
                self.assertEqual(report["diagnostic_code"], code)
                self.assertFalse(report["retry_authorized"])
                self.assert_no_private_report(report)
        for stage in runner.STAGES - {"PROVIDER_PRESTATE"}:
            with mock.patch.object(runner, "CURRENT_STAGE", stage):
                self.assertNotIn("diagnostic_code", runner.failure_report(kernel.PrestateRejected("ACCOUNT_READ")))


class RecoveryExecutionTests(RunnerFixtures):
    def test_last_prewrite_snapshot_drift_stops_before_consuming_put(self):
        test = self.execution("recover")
        provider = test["callbacks"]["provider"]
        snapshot = provider.snapshot
        def changed():
            value = snapshot()
            if test["state"]["snapshot_count"] == 3:
                value["firewall"]["rules"].pop()
            return value
        provider.snapshot = changed
        with self.assertRaises(common.ReleaseError):
            self.run_execution(test)
        self.assertEqual(test["state"]["update_count"], 0)
        self.assertFalse(test["state"]["written"])

    def test_pending_creation_is_bound_to_send_time_and_not_future_or_original_issue(self):
        for stamp in (NOW - dt.timedelta(seconds=1), NOW + dt.timedelta(seconds=1)):
            test = self.execution("recover")
            provider = test["callbacks"]["provider"]
            update = provider.update
            def changed(plan):
                value = update(plan)
                value["app"]["pending_deployment"]["created_at"] = fixtures.stamp(stamp)
                return value
            provider.update = changed
            with self.subTest(future=stamp > NOW), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertEqual(test["state"]["observe_count"], 0)

    def test_forward_same_deployment_phase_race_waits_for_two_coherent_active_health_checks(self):
        test = self.execution("recover")
        provider = test["callbacks"]["provider"]
        observe = provider.observe
        def progressing(identity):
            value = observe(identity)
            if test["state"]["observe_count"] == 1:
                value["app"].pop("active_deployment")
                value["app"]["pending_deployment"] = {"id": identity, "phase": "DEPLOYING"}
            return value
        provider.observe = progressing
        self.run_execution(test)
        self.assertEqual(test["state"]["update_count"], 1)
        self.assertEqual(test["state"]["observe_count"], 4)
        self.assertEqual(test["events"].count("health"), 2)
        self.assertEqual(test["events"].count("install"), 1)
        self.assertGreaterEqual(test["events"].count(("sleep", 10)), 2)

    def test_backward_error_canceled_or_unbounded_nonterminal_state_never_installs(self):
        for failure in ("backward", "ERROR", "CANCELED", "never_active"):
            test = self.execution("recover")
            provider = test["callbacks"]["provider"]
            observe = provider.observe
            def altered(identity):
                value = observe(identity)
                phase = failure
                if failure == "backward":
                    phase = "DEPLOYING" if test["state"]["observe_count"] == 1 else "BUILDING"
                elif failure == "never_active":
                    phase = "DEPLOYING"
                value["deployments"]["deployments"][-1]["phase"] = phase
                value["app"].pop("active_deployment")
                value["app"]["pending_deployment"] = {"id": identity, "phase": phase}
                return value
            provider.observe = altered
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertFalse(test["state"]["written"])
            self.assertEqual(test["state"]["observe_count"], 80 if failure == "never_active" else 2 if failure == "backward" else 1)

    def test_expiry_after_put_or_secret_install_stops_without_retry(self):
        for after in ("update", "install"):
            test = self.execution("recover")
            test["callbacks"]["now"] = lambda: NOW + dt.timedelta(hours=2) if (
                test["state"]["written"] if after == "install" else test["state"]["update_count"] > 0) else NOW
            with self.subTest(after=after), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertEqual(test["events"].count("install"), int(after == "install"))
            self.assertEqual(test["state"]["reconcile_count"], 1)

    def test_recover_has_one_update_two_health_observations_then_one_secret_addition(self):
        test = self.execution("recover")
        result = self.run_execution(test)
        self.assertEqual(result["state"], "existing-driver-recovery-complete")
        self.assertEqual(test["state"]["update_count"], 1)
        self.assertEqual(test["events"].count("health"), 2)
        self.assertEqual(test["events"].count("install"), 1)
        install_index = test["events"].index("install")
        self.assertEqual(test["events"][:install_index].count("health"), 2)
        self.assertEqual(test["events"][install_index + 1:].count("observe"), 1)
        self.assertEqual(test["state"]["reconcile_count"], 0)
        self.assertEqual(result["effects"], runner.RECOVER_EFFECTS)
        self.assertEqual(result["health_observations"], 2)
        self.assertIs(result["synthetic_execution_performed"], False)
        self.assertEqual(test["state"]["configs"], [{
            "schema_version": 1, "url": test["origin"] + "/v1/execute",
            "driver_version_sha256": test["packet"]["target_driver_evidence"]["driver_version_sha256"],
            "fixture_descriptor_sha256": self.case["descriptor"]["fixture_descriptor_sha256"],
            "hmac_key_base64": self.case["descriptor"]["hmac_key_base64"],
        }])
        self.assert_no_private_report(result)

    def test_update_ambiguity_never_retries_or_installs_and_reconciles_once(self):
        test = self.execution("recover")
        provider = test["callbacks"]["provider"]
        update = provider.update
        def ambiguous(plan):
            update(plan)
            raise RuntimeError(PRIVATE)
        provider.update = ambiguous
        with self.assertRaises(common.ReleaseError) as caught:
            self.run_execution(test)
        self.assertEqual(str(caught.exception), "existing driver recovery stopped")
        self.assertEqual(test["state"]["update_count"], 1)
        self.assertEqual(test["state"]["reconcile_count"], 1)
        self.assertFalse(test["state"]["written"])
        self.assert_no_private_report(runner.failure_report(caught.exception))

    def test_bad_associated_pending_identity_never_selects_a_later_deployment(self):
        for path, value in (
            (("app", "id"), uid(40)), (("app", "owner_uuid"), self.user_id),
            (("app", "pending_deployment"), None),
            (("app", "pending_deployment", "id"), self.deployment_id),
            (("app", "pending_deployment", "phase"), "ERROR"),
            (("app", "pending_deployment", "created_at"), fixtures.stamp(fixtures.ORIGIN_TIME)),
        ):
            test = self.execution("recover")
            provider = test["callbacks"]["provider"]
            original_update = provider.update
            def changed(plan):
                response = original_update(plan)
                fixtures.put(response, path, value)
                return response
            provider.update = changed
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["observe_count"], 0)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertEqual(test["state"]["reconcile_count"], 1)
            self.assertFalse(test["state"]["written"])

    def test_poststate_extra_apps_deployments_old_history_secret_or_firewall_drift_stop(self):
        for path, value in (
            (("apps", "meta", "total"), 4),
            (("deployments", "meta", "total"), 4),
            (("deployments", "deployments", 0, "updated_at"), fixtures.stamp(NOW)),
            (("deployments", "deployments", 2, "phase"), "ERROR"),
            (("app", "spec", "services", 0, "envs", 0, "value"), "EV[changed-synthetic-private-value]"),
            (("firewall", "rules"), []),
            (("app", "pending_deployment"), {"id": uid(40), "phase": "ACTIVE"}),
            (("production_state_sha256",), "0" * 64),
        ):
            test = self.execution("recover")
            provider = test["callbacks"]["provider"]
            observe = provider.observe
            def changed(identity):
                value_snapshot = observe(identity)
                fixtures.put(value_snapshot, path, value)
                return value_snapshot
            provider.observe = changed
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertEqual(test["state"]["reconcile_count"], 1)
            self.assertFalse(test["state"]["written"])

    def test_second_active_ciphertext_or_health_failure_prevents_secret_install(self):
        for failure in ("ciphertext", "health"):
            test = self.execution("recover")
            if failure == "health":
                test["callbacks"]["health"] = mock.Mock(side_effect=[None, RuntimeError(PRIVATE)])
            else:
                provider = test["callbacks"]["provider"]
                observe = provider.observe
                def changed(identity):
                    value = observe(identity)
                    if test["state"]["observe_count"] == 2:
                        for spec in (value["app"]["spec"], value["deployments"]["deployments"][-1]["spec"]):
                            next(row for row in spec["services"][0]["envs"]
                                 if row["key"] in kernel.LOGIN_KEYS)["value"] = "EV[changed-later-login-ciphertext]"
                    return value
                provider.observe = changed
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertFalse(test["state"]["written"])
            self.assertEqual(test["state"]["update_count"], 1)

    def test_secret_writer_ambiguity_and_final_metadata_drift_never_retry(self):
        for failure in ("writer", "metadata"):
            test = self.execution("recover")
            if failure == "writer":
                install = test["callbacks"]["writer"].install
                def ambiguous(config):
                    install(config)
                    raise RuntimeError(PRIVATE)
                test["callbacks"]["writer"].install = ambiguous
            else:
                provider = test["callbacks"]["provider"]
                observe = provider.observe
                def changed(identity):
                    value = observe(identity)
                    if test["state"]["written"]:
                        value["canary_environment"]["secrets"][0]["updated_at"] = fixtures.stamp(NOW)
                    return value
                provider.observe = changed
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.run_execution(test)
            self.assertEqual(test["events"].count("install"), 1)
            self.assertEqual(test["state"]["update_count"], 1)
            self.assertEqual(test["state"]["reconcile_count"], 1)


class ProviderBoundaryTests(RunnerFixtures):
    def provider(self, update=False):
        try:
            from . import verify_production_plan as planner
        except ImportError:
            import verify_production_plan as planner
        contract = {"provider": {"app_id_sha256": kernel._hash_id(uid(10))}}
        api, reader = mock.Mock(), mock.Mock()
        with (mock.patch.dict(os.environ, {}, clear=True),
              mock.patch.object(common, "load_json", return_value=contract),
              mock.patch.object(planner, "validate_contract", side_effect=lambda value: value),
              mock.patch.object(runner.fixture, "_opener", return_value=object())):
            cls = runner.ProviderUpdate if update else runner.ProviderRead
            kwargs = {"update_token": "synthetic-update-token-" + PRIVATE} if update else {}
            result = cls(Path(__file__).resolve().parents[2], self.packet("recover" if update else "check"),
                         self.case["descriptor"], "synthetic-read-token-" + PRIVATE, api, reader,
                         now=lambda: NOW, **kwargs)
        return result

    def test_read_transport_permits_only_bound_fixed_gets_and_never_update_token(self):
        provider = self.provider()
        provider.app_id, provider.production_id = self.app_id, uid(10)
        provider.driver_deployment_ids.add(self.deployment_id)
        self.assertFalse(hasattr(provider, "update"))
        paths = ["/v2/account", "/v2/apps?per_page=200&page=1", "/v2/apps/regions",
                 "/v2/apps/tiers/instance_sizes/" + self.case["descriptor"]["plan"]["instance_size_slug"],
                 "/v2/databases/" + self.case["descriptor"]["plan"]["ledger"]["cluster_id"] + "/firewall",
                 "/v2/apps/" + self.app_id, "/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1",
                 "/v2/apps/" + self.app_id + "/deployments/" + self.deployment_id]
        with mock.patch.object(runner.fixture, "_wire", return_value=b"{}") as wire:
            for path in paths:
                self.assertEqual(provider._get(path), {})
            self.assertEqual(wire.call_count, len(paths))
            for call, path in zip(wire.call_args_list, paths):
                self.assertEqual(call.args[1], common.API_ORIGIN + path)
                self.assertEqual(call.kwargs["method"], "GET")
                self.assertNotIn("body", call.kwargs)
                self.assertTrue(call.kwargs["headers"]["Authorization"].startswith("Bearer synthetic-read-token-"))
            for path in ("/v2/apps", "/v2/apps/" + uid(40), "/v2/apps/" + self.app_id + "/deployments/" + uid(40),
                         "/v2/apps/" + self.app_id + "/logs", "/v2/apps/" + self.app_id + "/exec",
                         "/v2/databases/" + uid(12) + "/users", "/v2/databases/" + uid(12) + "/credentials",
                         "https://untrusted.invalid/v2/account", "/v2/apps?per_page=200&page=2"):
                with self.subTest(route=path.split("/")[-1]), self.assertRaises(common.ReleaseError):
                    provider._get(path)
            self.assertEqual(wire.call_count, len(paths))

    def test_update_burns_once_before_transport_success_timeout_or_malformed_body(self):
        response = common.canonical_payload_bytes({"app": {"pending_deployment": {"id": uid(25)}}})
        for outcome in (response, b"{}", b"not-json", TimeoutError(PRIVATE)):
            provider = self.provider(update=True)
            provider.app_id = self.app_id
            opener = mock.Mock()
            response_mock = mock.Mock(status=200)
            response_mock.__enter__ = mock.Mock(return_value=response_mock)
            response_mock.__exit__ = mock.Mock(return_value=False)
            opener.open.return_value = response_mock
            with mock.patch.object(runner.fixture, "_opener", return_value=opener):
                if isinstance(outcome, Exception):
                    opener.open.side_effect = outcome
                else:
                    response_mock.read.return_value = outcome
                try:
                    provider.update({"synthetic": "private-plan"})
                except (common.ReleaseError, runner.UpdateRejected):
                    pass
                self.assertTrue(provider.attempted)
                with self.assertRaises(common.ReleaseError):
                    provider.update({"synthetic": "private-plan"})
                self.assertEqual(opener.open.call_count, 1)
                call = opener.open.call_args
                request = call.args[0]
                self.assertEqual(request.full_url, common.API_ORIGIN + "/v2/apps/" + self.app_id)
                self.assertEqual(request.method, "PUT")
                self.assertTrue(request.get_header("Authorization").startswith("Bearer synthetic-update-token-"))
                self.assertEqual(common.loads_strict(request.data), {"spec": {"synthetic": "private-plan"}})
                self.assertEqual(call.kwargs["timeout"], 30)

    def test_unselected_or_wrong_selected_app_cannot_consume_update(self):
        for identity in (None, uid(40)):
            provider = self.provider(update=True)
            provider.app_id = identity
            with mock.patch.object(runner.fixture, "_wire") as wire, self.assertRaises(common.ReleaseError):
                provider.update({})
            wire.assert_not_called()
            self.assertFalse(provider.attempted)

    def test_ambiguity_reconciliation_is_two_gets_once_and_never_adopts_identity(self):
        provider = self.provider(update=True)
        provider.app_id = self.app_id
        with mock.patch.object(provider, "_get", return_value={}) as get:
            provider.reconcile()
            with self.assertRaises(common.ReleaseError):
                provider.reconcile()
        self.assertEqual([call.args[0] for call in get.call_args_list], [
            "/v2/apps/" + self.app_id, "/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1"])
        self.assertIsNone(provider.new_deployment_id)
        self.assertEqual(provider.driver_deployment_ids, set())
        self.assertEqual(provider.production_deployment_ids, set())
        with self.assertRaises(common.ReleaseError):
            provider.observe(uid(25))

    def test_tls_diagnostic_overrides_rejected_before_transport_initialization(self):
        for key in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR"):
            with (mock.patch.dict(os.environ, {key: PRIVATE}, clear=True),
                  mock.patch.object(runner.fixture, "_opener") as opener,
                  self.assertRaises(common.ReleaseError)):
                runner.ProviderRead(Path("."), self.packet(), self.case["descriptor"], PRIVATE, None, None)
            opener.assert_not_called()

    def snapshot_fixture(self):
        provider = self.provider()
        descriptor, before = self.case["descriptor"], copy.deepcopy(self.case["second"])
        metadata = {"synthetic": "fixed-environment-metadata"}
        provider.a["fixture_environment_sha256"] = common.sha256_value(metadata)
        routes = {
            "/v2/apps/tiers/instance_sizes/" + descriptor["plan"]["instance_size_slug"]:
                {"instance_size": {"slug": descriptor["plan"]["instance_size_slug"],
                                   "usd_per_month": descriptor["plan"]["monthly_usd"]}},
            "/v2/apps/regions": {"regions": [{"slug": descriptor["plan"]["region"], "disabled": False}]},
            "/v2/apps?per_page=200&page=1": before["apps"],
            "/v2/apps/" + self.app_id: {"app": before["app"]},
            "/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1": before["deployments"],
            "/v2/account": {"account": before["account"]},
            "/v2/databases/" + descriptor["plan"]["ledger"]["cluster_id"] + "/firewall": before["firewall"],
        }
        for row in before["deployments"]["deployments"]:
            routes["/v2/apps/" + self.app_id + "/deployments/" + row["id"]] = {"deployment": row}
        provider.reader.snapshot.return_value = before["canary_environment"]
        return provider, routes, metadata

    def test_read_snapshot_selects_exact_existing_app_two_failed_deployments_and_cost(self):
        provider, routes, metadata = self.snapshot_fixture()
        descriptor, before = self.case["descriptor"], self.case["second"]
        with (mock.patch.object(provider, "_get", side_effect=lambda path: copy.deepcopy(routes[path])),
              mock.patch.object(provider, "_production", return_value=before["production_state_sha256"]),
              mock.patch.object(runner, "fixture_metadata", return_value=metadata)):
            result = provider.snapshot()
            self.assertEqual(result["app"], before["app"])
            self.assertEqual({row["id"] for row in result["deployments"]["deployments"]},
                             {self.deployment_id, self.redeployed_deployment_id})
            self.assertTrue(all(row["phase"] == "ERROR" for row in result["deployments"]["deployments"]))
            self.assertEqual(provider.app_id, self.app_id)
            self.assertEqual(provider.production_id, uid(10))
            size = routes["/v2/apps/tiers/instance_sizes/" + descriptor["plan"]["instance_size_slug"]]
            size["instance_size"]["usd_per_month"] = "999"
            with self.assertRaises(common.ReleaseError):
                provider.snapshot()

    def test_snapshot_read_failures_emit_only_exact_closed_boundary_codes(self):
        cases = [("/v2/apps/tiers/instance_sizes/" + self.case["descriptor"]["plan"]["instance_size_slug"], "SIZE_READ"),
                 ("/v2/apps/regions", "REGIONS_READ"), ("/v2/apps?per_page=200&page=1", "APPS_READ"),
                 ("/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1", "DRIVER_HISTORY_READ"),
                 ("/v2/apps/" + self.app_id, "DRIVER_APP_READ"),
                 ("/v2/apps/" + self.app_id + "/deployments/" + self.deployment_id, "DRIVER_DEPLOYMENT_READ"),
                 ("/v2/account", "ACCOUNT_READ"),
                 ("/v2/databases/" + self.case["descriptor"]["plan"]["ledger"]["cluster_id"] + "/firewall", "FIREWALL_READ")]
        for path, code in cases:
            provider, routes, metadata = self.snapshot_fixture()
            def get(selected):
                if selected == path:
                    raise RuntimeError(PRIVATE)
                return copy.deepcopy(routes[selected])
            with (self.subTest(code=code), mock.patch.object(provider, "_get", side_effect=get),
                  mock.patch.object(provider, "_production", return_value=self.case["second"]["production_state_sha256"]),
                  mock.patch.object(runner, "fixture_metadata", return_value=metadata),
                  self.assertRaises(kernel.PrestateRejected) as caught):
                provider.snapshot()
            self.assertEqual(caught.exception.code, code)
            with mock.patch.object(runner, "CURRENT_STAGE", "PROVIDER_PRESTATE"):
                self.assert_no_private_report(runner.failure_report(caught.exception))

    def test_snapshot_policy_failures_are_not_confused_with_transport_failures(self):
        for mutation, code in (("size", "SIZE_POLICY"), ("region", "REGIONS_POLICY"),
                               ("inventory", "APPS_INVENTORY"), ("selection", "APP_SELECTION"),
                               ("idle", "APP_IDLE"), ("fixture", "FIXTURE_METADATA_BINDING")):
            provider, routes, metadata = self.snapshot_fixture()
            if mutation == "size":
                routes["/v2/apps/tiers/instance_sizes/" + self.case["descriptor"]["plan"]["instance_size_slug"]]["instance_size"]["usd_per_month"] = PRIVATE
            elif mutation == "region":
                routes["/v2/apps/regions"]["regions"] = []
            elif mutation == "inventory":
                routes["/v2/apps?per_page=200&page=1"]["meta"]["total"] = 4
            elif mutation == "selection":
                provider.app_id = uid(40)
            elif mutation == "idle":
                routes["/v2/apps?per_page=200&page=1"]["apps"][0]["pending_deployment"] = {"id": uid(40)}
            else:
                provider.a["fixture_environment_sha256"] = "0" * 64
            with (self.subTest(code=code), mock.patch.object(provider, "_get", side_effect=lambda path: copy.deepcopy(routes[path])),
                  mock.patch.object(provider, "_production", return_value=self.case["second"]["production_state_sha256"]),
                  mock.patch.object(runner, "fixture_metadata", return_value=metadata),
                  self.assertRaises(kernel.PrestateRejected) as caught):
                provider.snapshot()
            self.assertEqual(caught.exception.code, code)

    def test_production_snapshot_requires_exact_active_state_and_complete_164_record_history(self):
        provider = self.provider()
        provider.production_id = uid(10)
        active = uid(100)
        state = {"synthetic": "fixed-current-production-state"}
        provider.a["production_state_sha256"] = common.sha256_value(state)
        history = [{"id": uid(index + 100), "phase": "ACTIVE" if index == 0 else "SUPERSEDED",
                    "created_at": fixtures.stamp(NOW - dt.timedelta(minutes=index + 5)),
                    "updated_at": fixtures.stamp(NOW - dt.timedelta(minutes=2))} for index in range(164)]
        provider.a["production_history_sha256"] = common.sha256_value(sorted(history, key=lambda row: row["id"]))
        app = {"id": uid(10), "active_deployment": {"id": active}, "default_ingress": "https://production.invalid"}
        dep = {"id": active, "created_at": history[0]["created_at"]}
        routes = {"/v2/apps/" + uid(10): {"app": app},
                  "/v2/apps/" + uid(10) + "/deployments/" + active: {"deployment": dep},
                  "/v2/apps/" + uid(10) + "/deployments?per_page=200&page=1":
                      {"deployments": history, "meta": {"total": 164}}}
        with (mock.patch.object(provider, "_get", side_effect=lambda path: copy.deepcopy(routes[path])),
              mock.patch.object(provider.planner, "normalize_target_descriptor", return_value={}),
              mock.patch.object(provider.planner, "predecessor_provider_expectation", return_value=({}, {})),
              mock.patch.object(provider.planner, "provider_state", return_value=(state, None))):
            self.assertEqual(provider._production({}), common.sha256_value(state))
            self.assertEqual(provider.production_deployment_ids, {active})
            self.assertEqual(provider.driver_deployment_ids, set())
            for failure in ("count", "phase", "active", "time", "hash"):
                baseline = copy.deepcopy(history)
                if failure == "count":
                    history.pop()
                elif failure == "phase":
                    history[1]["phase"] = "DEPLOYING"
                elif failure == "active":
                    history[1]["phase"] = "ACTIVE"
                elif failure == "time":
                    history[1]["created_at"] = fixtures.stamp(NOW)
                else:
                    history[1]["updated_at"] = fixtures.stamp(NOW)
                with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                    provider._production({})
                history[:] = baseline

    def test_cross_app_deployment_id_cannot_be_used_on_other_apps_route(self):
        provider = self.provider()
        provider.app_id, provider.production_id = self.app_id, uid(10)
        provider.driver_deployment_ids.add(self.deployment_id)
        provider.production_deployment_ids.add(uid(100))
        with mock.patch.object(runner.fixture, "_wire") as wire:
            for path in ("/v2/apps/" + uid(10) + "/deployments/" + self.deployment_id,
                         "/v2/apps/" + self.app_id + "/deployments/" + uid(100)):
                with self.assertRaises(common.ReleaseError):
                    provider._get(path)
        wire.assert_not_called()


class HostedGuardTests(RunnerFixtures):
    def guard_fixture(self, mode="recover", job=None):
        packet = self.packet(mode)
        current_id, workflow_id = "87654", 363575080
        prefix = runner.fixture.API_PREFIX
        endpoint = prefix + "/actions/workflows/" + runner.WORKFLOW.rsplit("/", 1)[1]
        def run(identity, title, status, conclusion, created, updated):
            return {"id": int(identity), "workflow_id": workflow_id, "head_sha": CONTROL,
                    "head_branch": "main", "path": runner.WORKFLOW, "event": "workflow_dispatch",
                    "run_attempt": 1, "previous_attempt_url": None, "repository": {"full_name": common.REPOSITORY},
                    "display_title": title, "status": status, "conclusion": conclusion,
                    "created_at": fixtures.stamp(created), "updated_at": fixtures.stamp(updated)}
        current = run(current_id, runner.run_title(packet), "in_progress", None,
                      NOW - dt.timedelta(minutes=1), NOW)
        predecessor = run("76543", "Check existing CRM driver " + self.packet()["operation_id"] + " "
                          + packet["binding_sha256"], "completed", "success",
                          NOW - dt.timedelta(minutes=4), NOW - dt.timedelta(minutes=2))
        selected_job = job or mode
        def row(key, status, conclusion, identity, ordinal):
            return {"id": ordinal, "run_id": int(identity), "run_attempt": 1, "name": runner.JOB_NAMES[key],
                    "status": status, "conclusion": conclusion,
                    "started_at": fixtures.stamp(NOW - dt.timedelta(minutes=3)),
                    "completed_at": fixtures.stamp(NOW - dt.timedelta(minutes=2)) if status == "completed" else None,
                    "steps": [{"name": "synthetic successful step", "number": 1,
                               "status": "completed", "conclusion": "success"}]}
        current_jobs = [row(selected_job, "in_progress", None, current_id, 10)]
        if selected_job != "authority":
            current_jobs.insert(0, row("authority", "completed", "success", current_id, 11))
        previous_jobs = [row(key, "completed", "skipped" if key == "recover" else "success", "76543", index + 20)
                         for index, key in enumerate(runner.JOB_NAMES)]
        next(item for item in previous_jobs if item["name"] == runner.JOB_NAMES["recover"])["steps"] = []
        data = {
            prefix + "/branches/main": {"protected": True, "commit": {"sha": CONTROL}},
            prefix + "/branches/main/protection": {
                "enforce_admins": {"enabled": True}, "allow_force_pushes": {"enabled": False},
                "allow_deletions": {"enabled": False}, "required_conversation_resolution": {"enabled": True},
                "required_pull_request_reviews": {"dismiss_stale_reviews": True, "require_code_owner_reviews": False,
                    "require_last_push_approval": False, "required_approving_review_count": 0,
                    "bypass_pull_request_allowances": {"users": [], "teams": [], "apps": []}},
                "required_status_checks": {"strict": True,
                    "contexts": ["test", "lint", "build", "security", "e2e", "tenant-isolation"],
                    "checks": [{"context": key, "app_id": 15368}
                               for key in ("test", "lint", "build", "security", "e2e", "tenant-isolation")]}},
            endpoint: {"id": workflow_id, "path": runner.WORKFLOW, "state": "active"},
            endpoint + "/runs": {"total_count": 2 if mode == "recover" else 1,
                "workflow_runs": [current, predecessor] if mode == "recover" else [current]},
            prefix + "/actions/runs/" + current_id + "/artifacts": {"total_count": 0, "artifacts": []},
            prefix + "/actions/runs/" + current_id + "/attempts/1/jobs":
                {"total_count": len(current_jobs), "jobs": current_jobs},
            prefix + "/actions/runs/76543": copy.deepcopy(predecessor),
            prefix + "/actions/runs/76543/artifacts": {"total_count": 0, "artifacts": []},
            prefix + "/actions/runs/76543/attempts/1/jobs": {"total_count": 4, "jobs": previous_jobs},
        }
        for status in runner.ACTIVE_STATUSES:
            data[prefix + "/actions/runs?status=" + status] = {
                "total_count": 1 if status == "in_progress" else 0,
                "workflow_runs": [current] if status == "in_progress" else []}
        quarantined = {
            "id": 35643414394, "workflow_id": workflow_id,
            "head_sha": "f4b94fdc82a8c2914c356e2644f7632be4e4851e",
            "head_branch": "main", "path": runner.WORKFLOW, "event": "workflow_dispatch",
            "run_attempt": 1, "previous_attempt_url": None,
            "repository": {"full_name": common.REPOSITORY},
            "display_title": "Check existing CRM driver 627c314e-14c9-4750-a832-926956bc2324 "
                "205f27d01a8eb98f8e44c404158f1c83aecd3ca1cb616f78dde94c685a52a080",
            "status": "completed", "conclusion": "failure",
            "created_at": "2026-09-21T19:13:05Z", "updated_at": "2026-09-21T19:16:53Z",
        }
        quarantined_jobs = []
        for key, (identity, conclusion, started, completed) in runner.FAILED_CHECK_JOBS.items():
            if key == "recover":
                steps = []
            elif key == "gate":
                steps = [("Set up job", "success"), ("Require exactly the selected recovery mode", "failure"),
                         ("Complete job", "success")]
            else:
                step = "Validate public authority without private credentials" if key == "authority" \
                    else "Rehydrate and inspect only inside the protected boundary"
                steps = [("Set up job", "success"), ("Check out exact protected controls", "success"),
                         (step, conclusion), ("Post Check out exact protected controls", "success"),
                         ("Complete job", "success")]
            quarantined_jobs.append({"id": identity, "run_id": 35643414394, "run_attempt": 1,
                "name": runner.JOB_NAMES[key], "status": "completed", "conclusion": conclusion,
                "started_at": started, "completed_at": completed,
                "steps": [{"name": name, "status": "completed", "conclusion": result} for name, result in steps]})
        data[endpoint + "/runs"]["workflow_runs"].append(quarantined)
        data[endpoint + "/runs"]["total_count"] += 1
        data[prefix + "/actions/runs/35643414394"] = copy.deepcopy(quarantined)
        data[prefix + "/actions/runs/35643414394/attempts/1/jobs"] = {"total_count": 4, "jobs": quarantined_jobs}
        data[prefix + "/actions/runs/35643414394/artifacts"] = {"total_count": 0, "artifacts": []}
        quarantined2 = copy.deepcopy(quarantined)
        quarantined2.update({"id": int(runner.FAILED_CHECK_2_ID),
                             "head_sha": runner.FAILED_CHECK_2_CONTROL,
                             "display_title": runner.FAILED_CHECK_2_TITLE,
                             **runner.FAILED_CHECK_2_TIMES})
        quarantined2_jobs = []
        for key, (identity, conclusion, started, completed) in runner.FAILED_CHECK_2_JOBS.items():
            if key == "recover":
                steps = []
            elif key == "gate":
                steps = [("Set up job", "success"), ("Require exactly the selected recovery mode", "failure"),
                         ("Complete job", "success")]
            else:
                step = "Validate public authority without private credentials" if key == "authority" \
                    else "Rehydrate and inspect only inside the protected boundary"
                steps = [("Set up job", "success"), ("Check out exact protected controls", "success"),
                         (step, conclusion), ("Post Check out exact protected controls", "success"),
                         ("Complete job", "success")]
            quarantined2_jobs.append({"id": identity, "run_id": int(runner.FAILED_CHECK_2_ID), "run_attempt": 1,
                "name": runner.JOB_NAMES[key], "status": "completed", "conclusion": conclusion,
                "started_at": started, "completed_at": completed,
                "steps": [{"name": name, "status": "completed", "conclusion": result} for name, result in steps]})
        data[endpoint + "/runs"]["workflow_runs"].append(quarantined2)
        data[endpoint + "/runs"]["total_count"] += 1
        data[prefix + "/actions/runs/" + runner.FAILED_CHECK_2_ID] = copy.deepcopy(quarantined2)
        data[prefix + "/actions/runs/" + runner.FAILED_CHECK_2_ID + "/attempts/1/jobs"] = {
            "total_count": 4, "jobs": quarantined2_jobs}
        data[prefix + "/actions/runs/" + runner.FAILED_CHECK_2_ID + "/artifacts"] = {
            "total_count": 0, "artifacts": []}
        api = mock.Mock(spec=["get", "pages"])
        api.get.side_effect = lambda path: copy.deepcopy(data[path])
        api.pages.side_effect = lambda path, key: copy.deepcopy(data[path])
        return {"packet": packet, "api": api, "data": data, "current": current,
                "predecessor": predecessor, "current_jobs": current_jobs, "previous_jobs": previous_jobs,
                "quarantined": quarantined, "quarantined_jobs": quarantined_jobs,
                "quarantined2": quarantined2, "quarantined2_jobs": quarantined2_jobs,
                "endpoint": endpoint, "env": {"RECOVERY_MODE": mode, "GITHUB_RUN_ID": current_id,
                                              "GITHUB_JOB": selected_job}}

    def guard(self, test):
        with (mock.patch.dict(os.environ, test["env"], clear=True),
              mock.patch.object(runner.fixture, "_current_guard", return_value=CONTROL)):
            return runner.current_guard(test["api"], Path("."), test["packet"], now=lambda: NOW)

    def test_exact_check_or_recover_authority_and_selected_job_can_pass(self):
        for mode in ("check", "recover"):
            for job in ("authority", mode):
                with self.subTest(mode=mode, job=job):
                    self.assertEqual(self.guard(self.guard_fixture(mode, job)), CONTROL)

    def test_frozen_failed_check_is_retained_but_never_a_recovery_predecessor(self):
        test = self.guard_fixture()
        test["packet"]["check_run_id"] = "35643414394"
        with self.assertRaises(common.ReleaseError):
            self.guard(test)
        for mutation in ("missing", "duplicate", "other_old_head", "rerun_latest", "artifact", "recover_steps"):
            test = self.guard_fixture("check")
            inventory = test["data"][test["endpoint"] + "/runs"]
            if mutation == "missing":
                inventory["workflow_runs"].remove(test["quarantined"])
                inventory["total_count"] -= 1
            elif mutation == "duplicate":
                inventory["workflow_runs"].append(copy.deepcopy(test["quarantined"]))
                inventory["total_count"] += 1
            elif mutation == "other_old_head":
                test["quarantined"]["id"] += 1
            elif mutation == "rerun_latest":
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/35643414394"]["run_attempt"] = 2
            elif mutation == "artifact":
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/35643414394/artifacts"] = {
                    "total_count": 1, "artifacts": [{"id": 123}]}
            else:
                test["quarantined_jobs"][2]["steps"] = [{"name": "unexpected write", "status": "completed", "conclusion": "success"}]
            with self.subTest(mutation=mutation), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_quarantined_run_identity_and_terminal_metadata_are_frozen(self):
        for key, value in (("head_sha", CONTROL), ("head_branch", "other"), ("event", "push"),
                           ("run_attempt", 2), ("run_attempt", True), ("previous_attempt_url", "present"),
                           ("display_title", "Recover existing CRM driver " + uid(40) + " " + "f" * 64),
                           ("status", "in_progress"), ("conclusion", "success"),
                           ("created_at", fixtures.stamp(NOW)), ("updated_at", fixtures.stamp(NOW)),
                           ("path", boot.WORKFLOW), ("workflow_id", 1), ("repository", {"full_name": "other/repo"})):
            for source in ("inventory", "latest"):
                test = self.guard_fixture("check")
                row = test["quarantined"] if source == "inventory" else test["data"][
                    runner.fixture.API_PREFIX + "/actions/runs/35643414394"]
                row[key] = value
                with self.subTest(field=key, source=source), self.assertRaises(common.ReleaseError):
                    self.guard(test)

    def test_quarantined_jobs_are_exact_and_cannot_mask_prior_write(self):
        for index in range(4):
            for key, value in (("id", 1), ("run_id", 1), ("run_attempt", 2), ("run_attempt", True),
                               ("status", "in_progress"), ("conclusion", "cancelled"),
                               ("started_at", fixtures.stamp(NOW)), ("completed_at", fixtures.stamp(NOW))):
                test = self.guard_fixture("check")
                test["quarantined_jobs"][index][key] = value
                with self.subTest(index=index, field=key), self.assertRaises(common.ReleaseError):
                    self.guard(test)
        test = self.guard_fixture("check")
        test["quarantined_jobs"][1]["steps"][2]["conclusion"] = "success"
        with self.assertRaises(common.ReleaseError):
            self.guard(test)

    def test_second_failed_check_is_retained_but_never_a_recovery_predecessor(self):
        test = self.guard_fixture()
        test["packet"]["check_run_id"] = runner.FAILED_CHECK_2_ID
        test["packet"]["binding_sha256"] = runner.binding_hash(test["packet"])
        with self.assertRaises(common.ReleaseError):
            self.guard(test)
        for mutation in ("missing", "duplicate", "foreign_id", "rerun_latest", "artifact", "write_step"):
            test = self.guard_fixture("check")
            inventory = test["data"][test["endpoint"] + "/runs"]
            if mutation == "missing":
                inventory["workflow_runs"].remove(test["quarantined2"])
                inventory["total_count"] -= 1
            elif mutation == "duplicate":
                inventory["workflow_runs"].append(copy.deepcopy(test["quarantined2"]))
                inventory["total_count"] += 1
            elif mutation == "foreign_id":
                test["quarantined2"]["id"] += 1
            elif mutation == "rerun_latest":
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/" +
                             runner.FAILED_CHECK_2_ID]["run_attempt"] = 2
            elif mutation == "artifact":
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/" +
                             runner.FAILED_CHECK_2_ID + "/artifacts"] = {
                    "total_count": 1, "artifacts": [{"id": 123}]}
            else:
                test["quarantined2_jobs"][2]["steps"] = [
                    {"name": "unexpected write", "status": "completed", "conclusion": "success"}]
            with self.subTest(mutation=mutation), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_second_failed_check_identity_jobs_and_skipped_clock_are_frozen(self):
        for source in ("inventory", "latest"):
            for key, value in (("head_sha", CONTROL), ("run_attempt", 2),
                               ("previous_attempt_url", "present"), ("conclusion", "success"),
                               ("updated_at", fixtures.stamp(NOW))):
                test = self.guard_fixture("check")
                row = test["quarantined2"] if source == "inventory" else test["data"][
                    runner.fixture.API_PREFIX + "/actions/runs/" + runner.FAILED_CHECK_2_ID]
                row[key] = value
                with self.subTest(source=source, key=key), self.assertRaises(common.ReleaseError):
                    self.guard(test)
        for key, value in (("id", 1), ("run_attempt", 2), ("conclusion", "success"),
                           ("completed_at", "2026-09-22T21:12:32Z"),
                           ("started_at", "2026-09-22T21:12:31Z")):
            test = self.guard_fixture("check")
            test["quarantined2_jobs"][2][key] = value
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_public_authority_does_not_request_administration_protection_endpoint(self):
        test = self.guard_fixture("check", "authority")
        del test["data"][runner.fixture.API_PREFIX + "/branches/main/protection"]
        self.assertEqual(self.guard(test), CONTROL)
        self.assertTrue(all(not call.args[0].endswith("/protection") for call in test["api"].get.call_args_list))

    def test_private_guard_preserves_all_actual_review_fields_without_admin_bypass(self):
        for key, value in (("dismiss_stale_reviews", False), ("require_code_owner_reviews", True),
                           ("require_last_push_approval", True), ("required_approving_review_count", True),
                           ("required_approving_review_count", 1), ("bypass_pull_request_allowances", {"users": [{"id": 5}]})):
            test = self.guard_fixture()
            test["data"][runner.fixture.API_PREFIX + "/branches/main/protection"]["required_pull_request_reviews"][key] = value
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                self.guard(test)
        for failure in ("missing", "restrictions"):
            test = self.guard_fixture()
            protection = test["data"][runner.fixture.API_PREFIX + "/branches/main/protection"]
            if failure == "missing":
                del protection["required_pull_request_reviews"]
            else:
                protection["restrictions"] = {}
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_private_guard_accepts_only_omitted_null_or_exact_empty_bypass(self):
        for mode in ("check", "recover"):
            for bypass in ("omitted", None, {"users": [], "teams": [], "apps": []}):
                test = self.guard_fixture(mode)
                reviews = test["data"][runner.fixture.API_PREFIX + "/branches/main/protection"][
                    "required_pull_request_reviews"]
                if bypass == "omitted":
                    del reviews["bypass_pull_request_allowances"]
                else:
                    reviews["bypass_pull_request_allowances"] = bypass
                with self.subTest(mode=mode, bypass=bypass):
                    self.assertEqual(self.guard(test), CONTROL)

    def test_private_guard_rejects_malformed_or_nonempty_bypass(self):
        invalid = (True, False, 0, "", [], {}, {"users": []},
                   {"users": [], "teams": [], "apps": [], "extra": []},
                   {"users": (), "teams": [], "apps": []},
                   {"users": [{"id": 5}], "teams": [], "apps": []},
                   {"users": [], "teams": [{"id": 5}], "apps": []},
                   {"users": [], "teams": [], "apps": [{"id": 5}]})
        for bypass in invalid:
            test = self.guard_fixture("check")
            test["data"][runner.fixture.API_PREFIX + "/branches/main/protection"][
                "required_pull_request_reviews"]["bypass_pull_request_allowances"] = bypass
            with self.subTest(bypass=bypass), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_skipped_opposite_job_accepts_only_one_second_clock_inversion(self):
        for mode in ("check", "recover"):
            opposite = "recover" if mode == "check" else "check"
            test = self.guard_fixture(mode)
            row = copy.deepcopy(test["current_jobs"][0])
            row.update(id=44, name=runner.JOB_NAMES[opposite], status="completed",
                       conclusion="skipped", steps=[],
                       started_at=fixtures.stamp(NOW - dt.timedelta(seconds=1)),
                       completed_at=fixtures.stamp(NOW - dt.timedelta(seconds=2)))
            test["current_jobs"].append(row)
            test["data"][runner.fixture.API_PREFIX + "/actions/runs/87654/attempts/1/jobs"]["total_count"] += 1
            with self.subTest(mode=mode, case="exact"):
                self.assertEqual(self.guard(test), CONTROL)
            for key, value in (("name", runner.JOB_NAMES["gate"]), ("status", "in_progress"),
                               ("conclusion", "success"), ("steps", [{"name": "unexpected"}]),
                               ("completed_at", fixtures.stamp(NOW - dt.timedelta(seconds=3)))):
                invalid = self.guard_fixture(mode)
                changed = copy.deepcopy(row)
                changed[key] = value
                invalid["current_jobs"].append(changed)
                invalid["data"][runner.fixture.API_PREFIX + "/actions/runs/87654/attempts/1/jobs"]["total_count"] += 1
                with self.subTest(mode=mode, key=key), self.assertRaises(common.ReleaseError):
                    self.guard(invalid)
        predecessor = self.guard_fixture("recover")
        skipped = next(row for row in predecessor["previous_jobs"]
                       if row["name"] == runner.JOB_NAMES["recover"])
        skipped.update(steps=[], started_at=fixtures.stamp(NOW - dt.timedelta(seconds=1)),
                       completed_at=fixtures.stamp(NOW - dt.timedelta(seconds=2)))
        self.assertEqual(self.guard(predecessor), CONTROL)

    def test_normal_clock_skipped_job_cannot_hide_steps_or_wrong_mode(self):
        for mode in ("check", "recover"):
            opposite = "recover" if mode == "check" else "check"
            test = self.guard_fixture(mode)
            row = copy.deepcopy(test["current_jobs"][0])
            row.update(id=44, name=runner.JOB_NAMES[opposite], status="completed",
                       conclusion="skipped", steps=[],
                       started_at=fixtures.stamp(NOW - dt.timedelta(seconds=2)),
                       completed_at=fixtures.stamp(NOW - dt.timedelta(seconds=1)))
            test["current_jobs"].append(row)
            test["data"][runner.fixture.API_PREFIX + "/actions/runs/87654/attempts/1/jobs"]["total_count"] += 1
            self.assertEqual(self.guard(test), CONTROL)
            for key, value in (("steps", [{"name": "unexpected", "status": "completed",
                                           "conclusion": "success"}]),
                               ("name", runner.JOB_NAMES["gate"])):
                invalid = self.guard_fixture(mode)
                changed = copy.deepcopy(row)
                changed[key] = value
                invalid["current_jobs"].append(changed)
                invalid["data"][runner.fixture.API_PREFIX + "/actions/runs/87654/attempts/1/jobs"]["total_count"] += 1
                with self.subTest(mode=mode, key=key), self.assertRaises(common.ReleaseError):
                    self.guard(invalid)

    def test_job_run_ids_attempts_and_duplicate_names_fail_closed(self):
        for key, value in (("run_id", 87653), ("run_attempt", True), ("id", 0),
                           ("name", runner.JOB_NAMES["authority"])):
            test = self.guard_fixture()
            test["current_jobs"][-1][key] = value
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_future_current_creation_and_moved_predecessor_time_are_rejected(self):
        for failure in ("future", "predecessor"):
            test = self.guard_fixture()
            if failure == "future":
                test["current"]["created_at"] = fixtures.stamp(NOW + dt.timedelta(seconds=1))
            else:
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/76543"]["updated_at"] = fixtures.stamp(NOW)
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_current_head_attempt_event_title_and_previous_attempt_drift_rejected(self):
        for key, value in (("head_sha", "a" * 40), ("head_branch", "other"), ("event", "push"),
                           ("run_attempt", 2), ("run_attempt", True), ("previous_attempt_url", "present"),
                           ("display_title", "Recover existing CRM driver " + uid(40) + " " + "f" * 64),
                           ("path", boot.WORKFLOW), ("workflow_id", 1), ("status", "completed")):
            test = self.guard_fixture()
            test["current"][key] = value
            with self.subTest(field=key), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_any_previous_recover_is_consumed_even_on_new_operation_failure_or_cancellation(self):
        for status, conclusion in (("completed", "success"), ("completed", "failure"), ("completed", "cancelled")):
            test = self.guard_fixture()
            prior = copy.deepcopy(test["current"])
            prior.update(id=87653, status=status, conclusion=conclusion)
            inventory = test["data"][test["endpoint"] + "/runs"]
            inventory["workflow_runs"].append(prior)
            inventory["total_count"] += 1
            with self.subTest(conclusion=conclusion), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_predecessor_requires_exact_binding_fresh_terminal_success_all_jobs_steps_and_zero_artifacts(self):
        for failure in ("binding", "conclusion", "old", "gate", "step", "artifact", "latest"):
            test = self.guard_fixture()
            if failure == "binding":
                test["predecessor"]["display_title"] = test["predecessor"]["display_title"][:-64] + "0" * 64
            elif failure == "conclusion":
                test["predecessor"]["conclusion"] = "failure"
            elif failure == "old":
                test["predecessor"]["created_at"] = fixtures.stamp(NOW - dt.timedelta(hours=3))
            elif failure == "gate":
                next(row for row in test["previous_jobs"] if row["name"] == runner.JOB_NAMES["gate"])["conclusion"] = "failure"
            elif failure == "step":
                test["previous_jobs"][0]["steps"][0]["conclusion"] = "skipped"
            elif failure == "artifact":
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/76543/artifacts"] = {
                    "total_count": 1, "artifacts": [{"id": 123}]}
            else:
                test["data"][runner.fixture.API_PREFIX + "/actions/runs/76543"]["run_attempt"] = 2
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.guard(test)

    def test_main_protection_extra_active_runs_and_current_jobs_are_fail_closed(self):
        for failure in ("head", "protected", "admin", "contexts", "force", "another_active", "authority", "own_job"):
            test = self.guard_fixture()
            prefix = runner.fixture.API_PREFIX
            protection = test["data"][prefix + "/branches/main/protection"]
            if failure == "head":
                test["data"][prefix + "/branches/main"]["commit"]["sha"] = "a" * 40
            elif failure == "protected":
                test["data"][prefix + "/branches/main"]["protected"] = False
            elif failure == "admin":
                protection["enforce_admins"]["enabled"] = False
            elif failure == "contexts":
                protection["required_status_checks"]["contexts"].pop()
            elif failure == "force":
                protection["allow_force_pushes"]["enabled"] = True
            elif failure == "another_active":
                test["data"][prefix + "/actions/runs?status=waiting"] = {"total_count": 1, "workflow_runs": [{"id": 87653}]}
            elif failure == "authority":
                test["current_jobs"][0]["conclusion"] = "failure"
            else:
                test["current_jobs"][-1]["status"] = "queued"
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                self.guard(test)


class CliBoundaryTests(RunnerFixtures):
    def environment(self, command="check"):
        packet = self.packet("recover" if command == "recover" else "check")
        env = {"RECOVERY_MODE": packet["mode"], "CONTROL_SHA": CONTROL,
               "RECOVERY_AUTHORIZATION_JSON": common.canonical_payload_bytes(packet).decode(),
               "ORIGIN_AUTHORIZATION_JSON": common.canonical_payload_bytes(self.case["original"]).decode(),
               "GH_TOKEN": "synthetic-gh-read-token-" + PRIVATE}
        if command != "validate":
            names = runner.PRIVATE_NAMES if command == "recover" else runner.CHECK_PRIVATE_NAMES
            env.update({name: "synthetic-private-value-" + PRIVATE for name in names})
            env["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"] = common.canonical_payload_bytes(self.case["descriptor"]).decode()
            env["CRM_CANARY_FIXTURE_INPUT_JSON"] = common.canonical_payload_bytes(self.protected).decode()
        return env

    def cli(self, command="validate", env=None, invoke=None, extra=()):
        try:
            from . import launch_production_prerequisites as prerequisites
        except ImportError:
            import launch_production_prerequisites as prerequisites
        class FrozenDateTime(dt.datetime):
            @classmethod
            def now(cls, tz=None):
                return NOW
        environment = self.environment(command) if env is None else env
        seen = []
        def guarded(*args):
            seen.append("guard")
            self.assertTrue(all(key not in os.environ for key in runner.PRIVATE_NAMES))
            return CONTROL
        def executing(a, original, descriptor, protected, **kwargs):
            seen.append("execute")
            self.assertTrue(all(key not in os.environ for key in runner.PRIVATE_NAMES))
            if invoke:
                return invoke(a, original, descriptor, protected, **kwargs)
            return {"schema_version": 1, "state": "existing-driver-check-complete"}
        with (mock.patch.dict(os.environ, environment, clear=True),
              mock.patch.object(runner, "dt", SimpleNamespace(datetime=FrozenDateTime, timezone=dt.timezone,
                                                            timedelta=dt.timedelta)),
              mock.patch.object(boot, "GitHubRead") as api,
              mock.patch.object(runner, "current_guard", side_effect=guarded) as guard,
              mock.patch.object(runner.fixture, "_pinned_gh", return_value=Path("synthetic-gh")),
              mock.patch.object(prerequisites, "GitHubEvidence") as evidence,
              mock.patch.object(prerequisites, "GitHubGetOnly"),
              mock.patch.object(boot, "EnvironmentReader"),
              mock.patch.object(boot, "EnvironmentWriter") as writer,
              mock.patch.object(runner, "ProviderRead") as provider_read,
              mock.patch.object(runner, "ProviderUpdate") as provider_update,
              mock.patch.object(boot, "ReadOnlyProductTransport"),
              mock.patch.object(boot, "authenticate_driver") as image_auth,
              mock.patch.object(runner, "authenticate_origin") as origin_auth,
              mock.patch.object(runner, "run_once", side_effect=executing) as execute,
              mock.patch("sys.stdout", new_callable=io.StringIO) as stdout,
              mock.patch("sys.stderr", new_callable=io.StringIO) as stderr):
            result = runner.main([command, "--control-root", str(Path(__file__).resolve().parents[2]), *extra])
            output, error = stdout.getvalue(), stderr.getvalue()
            private_removed = all(key not in os.environ for key in runner.PRIVATE_NAMES)
        for stream in (output, error):
            self.assertNotIn(PRIVATE, stream)
            self.assertNotIn(self.case["descriptor"]["hmac_key_base64"], stream)
            self.assertNotIn(self.protected["credentials"]["klinik_password"], stream)
        return {"code": result, "output": output, "error": error, "seen": seen, "removed": private_removed,
                "api": api, "guard": guard, "execute": execute, "evidence": evidence,
                "writer": writer, "provider_read": provider_read, "provider_update": provider_update,
                "image": image_auth, "origin": origin_auth}

    def test_public_validation_no_private_providers_fixture_rehydration_or_writer(self):
        result = self.cli()
        self.assertEqual(result["code"], 0)
        self.assertEqual(result["error"], "")
        self.assertEqual(result["seen"], ["guard"])
        self.assertEqual(common.loads_strict(result["output"])["state"], "existing-driver-public-authority-validated")
        for key in ("execute", "evidence", "writer", "provider_read", "provider_update"):
            result[key].assert_not_called()

    def test_every_private_value_is_removed_before_even_failed_public_validation(self):
        for name in runner.PRIVATE_NAMES:
            env = self.environment("validate")
            env[name] = PRIVATE
            with self.subTest(name=name):
                result = self.cli(env=env)
                self.assertEqual(result["code"], 1)
                self.assertTrue(result["removed"])
                result["api"].assert_not_called()
                result["execute"].assert_not_called()
                self.assertEqual(result["output"], "")

    def test_check_rejects_either_write_token_and_all_commands_reject_create_token(self):
        for name in ("DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"):
            env = self.environment()
            env[name] = PRIVATE
            result = self.cli("check", env)
            self.assertEqual(result["code"], 1)
            result["api"].assert_not_called()
        for command in ("validate", "check", "recover"):
            env = self.environment(command)
            env["DO_DRIVER_BOOTSTRAP_CREATE_TOKEN"] = PRIVATE
            result = self.cli(command, env)
            self.assertEqual(result["code"], 1)
            result["api"].assert_not_called()

    def test_check_wires_historical_fixture_current_image_and_only_read_provider(self):
        def invoke(a, original, descriptor, protected, **kwargs):
            self.assertEqual(original, self.case["original"])
            self.assertEqual(descriptor, self.case["descriptor"])
            self.assertEqual(protected, self.protected)
            self.assertIsNone(kwargs["writer"])
            kwargs["authenticate_origin"]()
            kwargs["authenticate_fixture"]()
            kwargs["authenticate_image"]()
            kwargs["current_guard"]()
            return {"schema_version": 1, "state": "existing-driver-check-complete"}
        result = self.cli("check", invoke=invoke)
        self.assertEqual(result["code"], 0)
        self.assertEqual(result["error"], "")
        result["provider_read"].assert_called_once()
        result["provider_update"].assert_not_called()
        result["writer"].assert_not_called()
        result["origin"].assert_called_once()
        result["evidence"].return_value.authenticate_public_fixture.assert_called_once_with(
            common.canonical_payload_bytes(self.case["original"]["fixture_evidence"]), CONTROL)
        image_args = result["image"].call_args.args
        self.assertEqual(image_args[3], self.packet()["target_driver_evidence"])
        self.assertEqual(image_args[4], CONTROL)

    def test_noncanonical_input_mode_mismatch_and_output_path_on_check_fail_before_credentials(self):
        for failure in ("noncanonical", "mode", "origin", "output"):
            env = self.environment("check")
            extra = ()
            if failure == "noncanonical":
                env["RECOVERY_AUTHORIZATION_JSON"] += "\n"
            elif failure == "mode":
                env["RECOVERY_MODE"] = "recover"
            elif failure == "origin":
                original = copy.deepcopy(self.case["original"])
                original["operation_id"] = uid(40)
                env["ORIGIN_AUTHORIZATION_JSON"] = common.canonical_payload_bytes(original).decode()
            else:
                extra = ("--output-dir", "synthetic-output-forbidden")
            with self.subTest(failure=failure):
                result = self.cli("check", env, extra=extra)
                self.assertEqual(result["code"], 1)
                result["api"].assert_not_called()

    def test_hostile_exceptions_emit_only_fixed_stage_and_optional_safe_status(self):
        class HostileError(Exception):
            def __str__(self):
                raise AssertionError("exception coerced")
            @property
            def status(self):
                raise AssertionError("status getter invoked")
        for error in (HostileError(PRIVATE), RuntimeError(PRIVATE), runner.UpdateRejected(403), runner.UpdateRejected(PRIVATE)):
            def invoke(*args, **kwargs):
                runner.mark_stage("UPDATE_APP")
                raise error
            result = self.cli("check", invoke=invoke)
            self.assertEqual(result["code"], 1)
            report = common.loads_strict(result["error"])
            self.assertEqual(report["stage"], "UPDATE_APP")
            self.assertIs(report["retry_authorized"], False)
            self.assertEqual(report.get("http_status"), 403 if type(error) is runner.UpdateRejected and error.status == 403 else None)
        with mock.patch.object(runner, "CURRENT_STAGE", PRIVATE):
            self.assertEqual(runner.failure_report(RuntimeError(PRIVATE))["stage"], "AUTHORIZATION")

    def test_recover_cli_writes_only_two_public_receipt_files_after_successful_run(self):
        root = Path(__file__).resolve().parents[2]
        output = root / "synthetic-offline-recovery-output-must-not-exist"
        self.assertFalse(output.exists())
        env = self.environment("recover")
        env["RUNNER_TEMP"] = str(root)
        report = {"schema_version": 1, "state": "existing-driver-recovery-complete"}
        def invoke(a, original, descriptor, protected, **kwargs):
            self.assertIsNotNone(kwargs["writer"])
            self.assertEqual(a["effects"], runner.RECOVER_EFFECTS)
            return report
        with (mock.patch.object(Path, "mkdir") as mkdir,
              mock.patch.object(Path, "write_bytes", autospec=REAL_PATH_WRITE_BYTES) as write_bytes,
              mock.patch.object(Path, "write_text", autospec=REAL_PATH_WRITE_TEXT) as write_text):
            result = self.cli("recover", env, invoke=invoke, extra=("--output-dir", str(output)))
        self.assertEqual(result["code"], 0)
        self.assertFalse(output.exists())
        result["provider_update"].assert_called_once()
        result["provider_read"].assert_not_called()
        result["writer"].assert_called_once()
        mkdir.assert_called_once_with(mode=0o700, parents=False, exist_ok=False)
        expected_bytes = common.canonical_file_bytes(report)
        write_bytes.assert_called_once_with(output / "receipt.json", expected_bytes)
        write_text.assert_called_once_with(output / "receipt.sha256", common.sha256_bytes(expected_bytes) + "\n", encoding="ascii")

    def test_isolated_standard_library_cli_help_imports_without_private_environment_or_execution(self):
        # A real isolated child only imports source and exits at argparse help.
        # Its environment is allowlisted; all actual action seams remain unused.
        environment = {"PYTHONDONTWRITEBYTECODE": "1"}
        for key in ("SystemRoot", "SYSTEMROOT", "WINDIR"):
            if key in os.environ:
                environment[key] = os.environ[key]
        result = REAL_SUBPROCESS_RUN([runner.sys.executable, "-I", "-S", "-B", str(Path(runner.__file__)), "--help"],
                                     env=environment, capture_output=True, check=False, timeout=30)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stderr, b"")
        self.assertIn(b"{validate,check,recover}", result.stdout)
        self.assertNotIn(PRIVATE.encode(), result.stdout)


class SourceSafetyTests(unittest.TestCase):
    def test_source_has_no_old_installer_no_create_or_delete_and_one_update_request_site(self):
        tree = ast.parse(Path(runner.__file__).read_text())
        calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)]
        forbidden = {"install_once", "check_once", "preflight", "create", "delete", "redeploy", "cancel"}
        self.assertTrue(all(not isinstance(call.func, ast.Attribute) or call.func.attr not in forbidden for call in calls))
        methods = [keyword.value.value for call in calls for keyword in call.keywords
                   if keyword.arg == "method" and isinstance(keyword.value, ast.Constant)]
        self.assertEqual(sorted(methods), ["GET", "PUT"])
        provider = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "ProviderRead")
        self.assertNotIn("update", {node.name for node in provider.body if isinstance(node, ast.FunctionDef)})
        update = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "ProviderUpdate")
        self.assertFalse(any(isinstance(node, (ast.For, ast.While)) for node in ast.walk(update)))

    def test_private_mutation_names_are_not_runtime_environment_configuration_or_public_report_keys(self):
        tree = ast.parse(Path(runner.__file__).read_text())
        functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
        for name in ("failure_report", "run_once"):
            keys = {key.value for node in ast.walk(functions[name]) if isinstance(node, ast.Dict)
                    for key in node.keys if isinstance(key, ast.Constant) and type(key.value) is str}
            self.assertFalse(keys & runner.PRIVATE_NAMES)
            self.assertNotIn("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN", keys)
        self.assertFalse(any(isinstance(node, ast.Import) and any(alias.name == "subprocess" for alias in node.names)
                             for node in tree.body))


class WorkflowContractTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(__file__).resolve().parents[2]
        self.text = (self.root / ".github/workflows/recover-production-crm-canary-driver.yml").read_text()
        self.public, jobs = self.text.split("\njobs:\n", 1)
        matches = list(re.finditer(r"^  ([a-z][a-z0-9_-]*):\n", jobs, re.MULTILINE))
        self.jobs = {match.group(1): jobs[match.start():matches[index + 1].start()
                                        if index + 1 < len(matches) else len(jobs)]
                     for index, match in enumerate(matches)}

    def secret_names(self, job):
        return set(re.findall(r"secrets\.([A-Z][A-Z0-9_]*)", self.jobs[job]))

    def test_only_manual_check_recover_modes_share_non_canceling_production_lock(self):
        ci = (Path(__file__).resolve().parents[2] / ".github/workflows/test.yml").read_text(encoding="utf-8")
        self.assertIn("          actionlint .github/workflows/recover-production-crm-canary-driver.yml\n", ci)
        self.assertEqual(set(self.jobs), {"authority", "check", "recover", "gate"})
        self.assertIn("  workflow_dispatch:\n", self.public)
        self.assertNotRegex(self.public, r"(?m)^  (?:push|pull_request|schedule|workflow_call):")
        self.assertIn("        default: check\n", self.public)
        self.assertIn("        options:\n          - check\n          - recover\n", self.public)
        self.assertIn("  group: rereply-production\n  cancel-in-progress: false", self.public)
        self.assertIn("  contents: read\n", self.public)
        self.assertNotIn("secrets.", self.public)
        self.assertNotIn("continue-on-error", self.text)
        self.assertNotRegex(self.text, r"--admin|bypass|cancel-in-progress: true")

    def test_public_inputs_bind_old_packet_new_packet_mode_and_run_title(self):
        for field in ("origin_authorization_json", "recovery_authorization_json"):
            self.assertIn("      " + field + ":\n", self.public)
        for entry in (
            "RECOVERY_MODE: ${{ inputs.mode }}",
            "ORIGIN_AUTHORIZATION_JSON: ${{ inputs.origin_authorization_json }}",
            "RECOVERY_AUTHORIZATION_JSON: ${{ inputs.recovery_authorization_json }}",
            "CONTROL_SHA: ${{ github.sha }}", "WORKFLOW_SHA: ${{ github.workflow_sha }}",
            "REF_PROTECTED: ${{ github.ref_protected }}",
        ):
            self.assertIn(entry, self.public)
        title = next(line for line in self.public.splitlines() if line.startswith("run-name:"))
        self.assertIn("fromJSON(inputs.recovery_authorization_json).operation_id", title)
        self.assertIn("fromJSON(inputs.recovery_authorization_json).binding_sha256", title)

    def test_check_job_has_only_exact_read_and_historical_fixture_secrets(self):
        read = {"GH_DRIVER_BOOTSTRAP_READ_TOKEN", "CRM_CANARY_FIXTURE_INPUT_JSON",
                "CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "DO_DRIVER_BOOTSTRAP_READ_TOKEN"}
        self.assertEqual(self.secret_names("authority"), set())
        self.assertEqual(self.secret_names("check"), read)
        self.assertEqual(self.secret_names("recover"), read | {
            "DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"})
        self.assertEqual(self.secret_names("gate"), set())
        self.assertNotIn("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN", self.text)
        self.assertNotIn("attestations: write", self.jobs["check"])
        self.assertNotIn("id-token: write", self.jobs["check"])
        self.assertNotIn("--output-dir", self.jobs["check"])

    def test_both_private_jobs_use_ordinary_protected_fixture_environment(self):
        for mode in ("check", "recover"):
            job = self.jobs[mode]
            self.assertIn("    needs: authority\n", job)
            self.assertIn("    if: ${{ inputs.mode == '" + mode + "' }}\n", job)
            self.assertIn("    environment: rereply-production-crm-fixture\n", job)
        self.assertNotIn("    environment:", self.jobs["authority"])
        self.assertNotIn("    environment:", self.jobs["gate"])

    def test_every_execution_checks_protected_main_attempt_one_and_exact_checkout(self):
        for job_name, mode in (("authority", "validate"), ("check", "check"), ("recover", "recover")):
            job = self.jobs[job_name]
            self.assertIn("ref: ${{ github.sha }}", job)
            self.assertIn("fetch-depth: 0", job)
            self.assertIn("persist-credentials: false", job)
            self.assertIn('[[ "$GITHUB_REF" == refs/heads/main && "$REF_PROTECTED" == true ]]', job)
            self.assertIn('[[ "$GITHUB_RUN_ATTEMPT" == 1 && "$WORKFLOW_SHA" == "$CONTROL_SHA" ]]', job)
            self.assertIn("python3 -I -S -B control/release/deployment/run_existing_crm_canary_driver_recovery.py "
                          + mode + " --control-root control", job)
            self.assertIn("set -euo pipefail", job)

    def test_only_recover_can_attest_and_upload_exact_nonoverwrite_public_receipt(self):
        for job in ("authority", "check", "gate"):
            self.assertNotIn("actions/attest@", self.jobs[job])
            self.assertNotIn("actions/upload-artifact@", self.jobs[job])
        recover = self.jobs["recover"]
        self.assertEqual(recover.count("actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6"), 2)
        self.assertEqual(recover.count("actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"), 1)
        self.assertIn("subject-path: ${{ runner.temp }}/driver-recovery-receipt/receipt.json", recover)
        self.assertIn("predicate-type: https://rereply.app/attestations/crm-canary-driver-existing-recovery/v1", recover)
        self.assertIn("name: crm-canary-driver-existing-recovery-${{ github.run_id }}-1", recover)
        self.assertIn("path: ${{ runner.temp }}/driver-recovery-receipt\n", recover)
        for setting in ("if-no-files-found: error", "overwrite: false", "include-hidden-files: false",
                        "compression-level: 0", "retention-days: 90"):
            self.assertIn(setting, recover)
        uses = re.findall(r"uses: ([^\s]+)", self.text)
        self.assertEqual(len(uses), 6)
        self.assertTrue(all(re.fullmatch(r"[^@]+@[0-9a-f]{40}", use) for use in uses))

    def test_final_gate_requires_selected_success_opposite_skipped_and_authority_success(self):
        gate = self.jobs["gate"]
        for fragment in (
            "needs: [authority, check, recover]", "if: ${{ always() }}", "permissions: {}",
            '[[ "$AUTHORITY_RESULT" == success ]]',
            'if [[ "$RECOVERY_MODE" == check ]]; then',
            '[[ "$CHECK_RESULT" == success && "$RECOVER_RESULT" == skipped ]]',
            'elif [[ "$RECOVERY_MODE" == recover ]]; then',
            '[[ "$CHECK_RESULT" == skipped && "$RECOVER_RESULT" == success ]]',
            "else\n            exit 1\n          fi",
        ):
            self.assertIn(fragment, gate)


if __name__ == "__main__":
    unittest.main()
