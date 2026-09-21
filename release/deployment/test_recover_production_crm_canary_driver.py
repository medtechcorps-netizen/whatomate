"""Offline-only regressions for the existing-driver read-only policy kernel.

Every packet, identity, HMAC and ciphertext below is synthetic. Frozen incident
selectors are patched to these fixtures, never copied from protected files.
Transport and subprocess tripwires keep an accidental live action test-fatal.
"""
from __future__ import annotations

import ast
import base64
import copy
from contextlib import contextmanager, redirect_stderr, redirect_stdout
import datetime as dt
import hashlib
import io
from pathlib import Path
import unittest
from unittest import mock

try:
    from . import recover_production_crm_canary_driver as recovery
    from . import test_bootstrap_production_crm_canary_driver as samples
except ImportError:
    import recover_production_crm_canary_driver as recovery
    import test_bootstrap_production_crm_canary_driver as samples

boot = recovery.boot
common = recovery.common
uid = samples.uid
ORIGIN_TIME = dt.datetime(2026, 1, 1, 12, 0, tzinfo=dt.timezone.utc)
NOW = ORIGIN_TIME + dt.timedelta(days=1)
CONTROL = "9" * 40
PRIVATE = "SYNTHETIC_PRIVATE_MUST_NEVER_APPEAR"
LOGIN_KEYS = ("CRM_CANARY_KLINIK_LOGIN_JSON", "CRM_CANARY_NON_KLINIK_LOGIN_JSON")


def stamp(value):
    return value.strftime("%Y-%m-%dT%H:%M:%SZ")


def put(value, path, replacement):
    for key in path[:-1]:
        value = value[key]
    value[path[-1]] = replacement


def snapshot_specs(snapshot):
    return [snapshot["app"]["spec"]] + [row["spec"] for row in snapshot["deployments"]["deployments"]]


def driver_metadata(snapshot):
    keys = ("id", "created_at", "updated_at")
    return {"app": {key: snapshot["app"][key] for key in keys},
            "deployments": sorted([{key: row[key] for key in keys}
                                    for row in snapshot["deployments"]["deployments"]],
                                   key=lambda row: row["id"])}


class RecoveryFixtures(unittest.TestCase):
    def setUp(self):
        # Do not initialize a live provider, inspect the host environment, run a
        # CLI, read a protected descriptor or invoke the old install/check path.
        for owner, name in ((boot.fixture, "_wire"), (boot, "install_once"),
                            (boot, "check_once"), (boot, "preflight"),
                            (boot.subprocess, "run"), (boot.socket, "create_connection"),
                            (Path, "write_bytes"), (Path, "write_text")):
            guard = mock.patch.object(owner, name, side_effect=AssertionError("offline boundary crossed"))
            guard.start()
            self.addCleanup(guard.stop)
        with mock.patch.object(samples, "NOW", ORIGIN_TIME):
            original, descriptor, self.protected, _, _, self.driver = samples.packets()
        original["issued_at"] = stamp(ORIGIN_TIME - dt.timedelta(minutes=1))
        original["expires_at"] = stamp(ORIGIN_TIME + dt.timedelta(hours=1))
        samples.rebind(original, descriptor)
        self.app_id, self.deployment_id = uid(14), uid(15)
        self.redeployed_deployment_id = uid(19)
        self.original_created = ORIGIN_TIME + dt.timedelta(minutes=4)
        self.redeployed_created = ORIGIN_TIME + dt.timedelta(hours=2)
        self.user_id, self.team_id = descriptor["plan"]["provider_account_uuid"], uid(16)
        self.pins = {
            "app_id_sha256": common.sha256_bytes(self.app_id.encode("ascii")),
            "failed_deployment_id_sha256": common.sha256_bytes(self.deployment_id.encode("ascii")),
            "redeployed_deployment_id_sha256": common.sha256_bytes(self.redeployed_deployment_id.encode("ascii")),
            "provider_user_id_sha256": common.sha256_bytes(self.user_id.encode("ascii")),
            "provider_team_id_sha256": common.sha256_bytes(self.team_id.encode("ascii")),
        }
        self.origin_hash = common.sha256_value(original)
        constants = {
            "ORIGIN_CONTROL": original["control_sha"], "ORIGIN_RUN": "45678",
            "ORIGIN_PACKET_SHA256": self.origin_hash, "ORIGIN_WORKFLOW_ID": 67890,
            "ORIGIN_CREATED_AT": stamp(ORIGIN_TIME), "IDENTITY_PINS": self.pins,
            "LEDGER_ID_SHA256": common.sha256_bytes(descriptor["plan"]["ledger"]["cluster_id"].encode("ascii")),
            "APP_UPDATED_AT": stamp(self.redeployed_created + dt.timedelta(minutes=1)),
            "ORIGINAL_DEPLOYMENT_TIMES": {
                "created_at": stamp(self.original_created),
                "updated_at": stamp(self.original_created + dt.timedelta(minutes=1))},
            "REDEPLOYMENT_TIMES": {
                "created_at": stamp(self.redeployed_created),
                "updated_at": stamp(self.redeployed_created + dt.timedelta(minutes=1))},
        }
        for name, value in constants.items():
            patch = mock.patch.object(recovery, name, value)
            patch.start()
            self.addCleanup(patch.stop)
        expected = recovery.historical_expected_spec(original, descriptor, self.protected, self.driver)
        stored = copy.deepcopy(expected)
        for env in stored["services"][0]["envs"]:
            if env["type"] == "SECRET":
                env["value"] = "EV[synthetic-private-ciphertext-" + env["key"] + "]"
        apps = [
            {"id": uid(10), "spec": {"name": "production-app"}},
            {"id": uid(17), "spec": {"name": "existing-fixture-app"}},
            {"id": self.app_id, "spec": {"name": descriptor["plan"]["app_name"]}},
        ]
        app_rows = sorted([{"id": a["id"], "name": a["spec"]["name"]} for a in apps],
                          key=lambda a: a["id"])
        authorization = {
            "schema_version": 1, "kind": "production-crm-canary-driver-existing-inspection-v1",
            "control_sha": CONTROL, "operation_id": uid(18),
            "issued_at": stamp(NOW - dt.timedelta(minutes=5)),
            "expires_at": stamp(NOW + dt.timedelta(minutes=55)),
            "origin_authorization_sha256": self.origin_hash, "identities": copy.deepcopy(self.pins),
            "apps_inventory_sha256": common.sha256_value(app_rows),
            "production_state_sha256": "1" * 64, "fixture_environment_sha256": "2" * 64,
            "firewall_sha256": common.sha256_value([
                {"type": "app", "value": uid(10)}, {"type": "app", "value": self.app_id}]),
            "target_driver_evidence": {
                "control_sha": CONTROL, "run_id": "22346", "artifact_id": "192",
                "artifact_digest": "sha256:" + "3" * 64, "digest": "sha256:" + "4" * 64,
                "driver_version_sha256": "5" * 64},
            "effects": copy.deepcopy(recovery.EFFECTS),
        }
        first = {
            "observed_at": stamp(NOW - dt.timedelta(seconds=10)),
            "account": {"status": "active", "uuid": self.user_id, "team": {"uuid": self.team_id}},
            "apps": {"apps": apps, "meta": {"total": 3}},
            "app": {"id": self.app_id, "owner_uuid": self.team_id, "spec": copy.deepcopy(stored),
                    "created_at": stamp(self.original_created),
                    "updated_at": stamp(self.redeployed_created + dt.timedelta(minutes=1))},
            "deployments": {"deployments": [
                {"id": self.deployment_id, "phase": "ERROR", "spec": copy.deepcopy(stored),
                 "created_at": stamp(self.original_created),
                 "updated_at": stamp(self.original_created + dt.timedelta(minutes=1))},
                {"id": self.redeployed_deployment_id, "phase": "ERROR", "spec": copy.deepcopy(stored),
                 "created_at": stamp(self.redeployed_created),
                 "updated_at": stamp(self.redeployed_created + dt.timedelta(minutes=1))}],
                "meta": {"total": 2}},
            "firewall": {"rules": [{"type": "app", "value": uid(10)},
                                     {"type": "app", "value": self.app_id}]},
            "canary_environment": samples.metadata(),
            "fixture_environment_sha256": authorization["fixture_environment_sha256"],
            "production_state_sha256": authorization["production_state_sha256"],
        }
        authorization["driver_metadata_sha256"] = common.sha256_value(driver_metadata(first))
        second = copy.deepcopy(first)
        second["observed_at"] = stamp(NOW - dt.timedelta(seconds=5))
        self.case = {
            "authorization": authorization, "original": original, "descriptor": descriptor,
            "origin_run": {
                "id": 45678, "workflow_id": 67890, "path": boot.WORKFLOW,
                "head_sha": original["control_sha"], "head_branch": "main", "event": "workflow_dispatch",
                "run_attempt": 1, "previous_attempt_url": None, "display_title": boot.RUN_TITLES["install"],
                "status": "completed", "conclusion": "failure",
                "repository": {"full_name": common.REPOSITORY}, "created_at": stamp(ORIGIN_TIME),
            },
            "origin_artifacts": {"total_count": 0, "artifacts": []},
            "first": first, "second": second, "control_sha": CONTROL,
            "expected_spec": expected, "now": NOW,
        }

    def inspect(self, case=None):
        return recovery.inspect_pair(**(self.case if case is None else case))

    def reject(self, change):
        case = copy.deepcopy(self.case)
        change(case)
        with self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def reject_value(self, path, replacement):
        self.reject(lambda c: put(c, path, replacement))

    def reject_snapshot(self, change, both=False):
        def mutate(case):
            change(case["first"])
            if both:
                change(case["second"])
        self.reject(mutate)


class InspectionTests(RecoveryFixtures):
    def test_expired_original_is_historical_only_and_result_remains_not_ready(self):
        self.assertLess(common.require_timestamp(self.case["original"]["expires_at"], "test"), NOW)
        before = copy.deepcopy(self.case)
        result = self.inspect()
        self.assertEqual(self.case, before)
        self.assertEqual(result, {
            "schema_version": 1, "state": "existing-driver-inspection-complete",
            "authorization_sha256": common.sha256_value(self.case["authorization"]),
            "origin_run_id": "45678", "app_id_sha256": self.pins["app_id_sha256"],
            "failed_deployment_id_sha256": self.pins["failed_deployment_id_sha256"],
            "deployment_phase": "ERROR",
            "redeployed_deployment_id_sha256": self.pins["redeployed_deployment_id_sha256"],
            "blockers": ["CANARY_CONFIGURATION_MISSING", "DEPLOYMENT_ERROR"],
            "planned_image_digest": self.case["authorization"]["target_driver_evidence"]["digest"],
            "planned_driver_version_sha256": self.case["authorization"]["target_driver_evidence"]["driver_version_sha256"],
            "plan_validation": "image-version-login-schema-only",
            "mutation_performed": False, "ready_for_finalize": False, "ready_for_operation": False,
            "historical_create_cause": "not-proven", "deployment_failure_cause": "not-proven",
            "historical_configuration_incompatibility": "SCHEMA_VERSION_MISSING",
            "historical_configuration_evidence": "source-construction-only",
        })

    def test_success_has_no_private_id_runtime_ciphertext_or_secret_material(self):
        with (mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            encoded = common.canonical_payload_bytes(self.inspect()).decode()
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")
        secrets = [self.app_id, self.deployment_id, self.redeployed_deployment_id, self.user_id, self.team_id,
                   self.case["descriptor"]["hmac_key_base64"],
                   self.case["descriptor"]["plan"]["ledger"]["cluster_id"],
                   self.protected["credentials"]["klinik_password"]]
        secrets += [e["value"] for e in self.case["first"]["app"]["spec"]["services"][0]["envs"]
                    if e["type"] == "SECRET"]
        for secret in secrets:
            with self.subTest(kind="synthetic-secret"):
                self.assertNotIn(secret, encoded)

    def test_arbitrary_provider_reason_messages_and_log_urls_are_ignored(self):
        case = copy.deepcopy(self.case)
        for snapshot in (case["first"], case["second"]):
            dep = snapshot["deployments"]["deployments"][0]
            dep.update(reason={"code": PRIVATE, "message": PRIVATE},
                       progress={"steps": [{"reason": {"code": PRIVATE, "message": PRIVATE}}]},
                       logs={"live_url": "https://private.invalid/" + PRIVATE})
            snapshot["app"]["message"] = PRIVATE
        self.assertEqual(self.inspect(case), self.inspect())
        self.assertNotIn(PRIVATE, common.canonical_payload_bytes(self.inspect(case)).decode())

    def test_inventory_order_and_environment_order_are_not_resource_authority(self):
        case = copy.deepcopy(self.case)
        case["second"]["apps"]["apps"].reverse()
        case["second"]["app"]["spec"]["services"][0]["envs"].reverse()
        case["second"]["deployments"]["deployments"][0]["spec"]["services"][0]["envs"].reverse()
        self.assertEqual(self.inspect(case), self.inspect())

    def test_two_error_history_order_is_not_resource_authority(self):
        case = copy.deepcopy(self.case)
        case["second"]["deployments"]["deployments"].reverse()
        self.assertEqual(self.inspect(case), self.inspect())

    def test_pair_minimum_and_maximum_intervals_are_inclusive(self):
        for seconds in (3, 120):
            case = copy.deepcopy(self.case)
            case["first"]["observed_at"] = stamp(NOW - dt.timedelta(seconds=seconds))
            case["second"]["observed_at"] = stamp(NOW)
            with self.subTest(seconds=seconds):
                self.assertFalse(self.inspect(case)["ready_for_finalize"])


class FreshAuthorizationTests(RecoveryFixtures):
    def test_schema_kind_control_and_operation_are_exact(self):
        cases = (("schema_version", True), ("schema_version", 2), ("kind", "install"),
                 ("control_sha", "8" * 40), ("operation_id", "latest"))
        for key, value in cases:
            with self.subTest(key=key, kind=type(value).__name__):
                self.reject_value(("authorization", key), value)
        self.reject_value(("control_sha",), "8" * 40)

    def test_extra_or_missing_authorization_fields_fail(self):
        self.reject(lambda c: c["authorization"].update(write_credential=PRIVATE))
        for key in recovery.AUTH_KEYS:
            with self.subTest(key=key):
                self.reject(lambda c, k=key: c["authorization"].pop(k))

    def test_current_expired_future_zero_or_overlong_window_fail(self):
        pairs = ((NOW - dt.timedelta(hours=1), NOW),
                 (NOW + dt.timedelta(seconds=1), NOW + dt.timedelta(hours=1)),
                 (NOW, NOW), (NOW, NOW - dt.timedelta(seconds=1)),
                 (NOW - dt.timedelta(minutes=5), NOW + dt.timedelta(hours=2)))
        for issued, expires in pairs:
            with self.subTest(issued=stamp(issued), expires=stamp(expires)):
                self.reject(lambda c: c["authorization"].update(issued_at=stamp(issued), expires_at=stamp(expires)))

    def test_current_authority_does_not_accept_original_operation(self):
        self.reject_value(("authorization", "operation_id"), self.case["original"]["operation_id"])

    def test_origin_hash_identity_selectors_and_snapshot_hashes_fail_on_drift(self):
        self.reject_value(("authorization", "origin_authorization_sha256"), "0" * 64)
        for key in self.pins:
            with self.subTest(key=key):
                self.reject_value(("authorization", "identities", key), "0" * 64)
        self.reject(lambda c: c["authorization"]["identities"].update(extra="0" * 64))
        for key in ("apps_inventory_sha256", "production_state_sha256", "fixture_environment_sha256",
                    "firewall_sha256", "driver_metadata_sha256"):
            for value in ("not-a-hash", "0" * 64):
                with self.subTest(key=key, malformed=value == "not-a-hash"):
                    self.reject_value(("authorization", key), value)

    def test_every_effect_is_exact_integer_zero(self):
        for key in recovery.EFFECTS:
            for value in (1, -1, False, 0.0, "0"):
                with self.subTest(effect=key, value_type=type(value).__name__):
                    self.reject_value(("authorization", "effects", key), value)
        self.reject(lambda c: c["authorization"]["effects"].update(reruns=0))
        self.reject(lambda c: c["authorization"]["effects"].pop("app_creations"))

    def test_authorization_validation_returns_independent_copy(self):
        a = self.case["authorization"]
        validated = recovery.validate_recovery_authorization(a, control_sha=CONTROL, now=NOW)
        validated["identities"]["app_id_sha256"] = "0" * 64
        validated["effects"]["app_creations"] = 1
        self.assertEqual(a["identities"], self.pins)
        self.assertEqual(a["effects"], recovery.EFFECTS)
        validated["target_driver_evidence"]["digest"] = "sha256:" + "0" * 64
        self.assertEqual(a["target_driver_evidence"]["digest"], "sha256:" + "4" * 64)

    def test_target_evidence_is_separate_closed_and_bound_to_current_control(self):
        target = self.case["authorization"]["target_driver_evidence"]
        self.assertNotEqual(target, self.case["original"]["driver_evidence"])
        for key in boot.DRIVER_EVIDENCE_KEYS:
            with self.subTest(missing=key):
                self.reject(lambda c, k=key: c["authorization"]["target_driver_evidence"].pop(k))
        self.reject(lambda c: c["authorization"]["target_driver_evidence"].update(write_token=PRIVATE))
        self.reject_value(("authorization", "target_driver_evidence", "control_sha"), "8" * 40)
        for key in ("control_sha", "run_id", "artifact_id", "artifact_digest", "digest", "driver_version_sha256"):
            for value in (True, False, None, {}, [], PRIVATE):
                with self.subTest(key=key, kind=type(value).__name__):
                    self.reject_value(("authorization", "target_driver_evidence", key), value)

    def test_target_image_and_general_version_both_have_to_change(self):
        for key in ("digest", "driver_version_sha256"):
            with self.subTest(key=key):
                self.reject_value(("authorization", "target_driver_evidence", key),
                                  self.case["original"]["driver_evidence"][key])

    def test_current_authority_window_boundaries_use_real_current_time(self):
        authority = copy.deepcopy(self.case["authorization"])
        authority.update(issued_at=stamp(NOW), expires_at=stamp(NOW + dt.timedelta(hours=2)))
        self.assertEqual(recovery.validate_recovery_authorization(authority, control_sha=CONTROL, now=NOW), authority)
        with self.assertRaises(common.ReleaseError):
            recovery.validate_recovery_authorization(authority, control_sha=CONTROL,
                                                       now=NOW + dt.timedelta(hours=2))
        with self.assertRaises(common.ReleaseError):
            recovery.validate_recovery_authorization(authority, control_sha=CONTROL, now=ORIGIN_TIME)


class HistoricalLineageTests(RecoveryFixtures):
    def test_every_original_packet_binding_is_frozen(self):
        changes = (
            (("control_sha",), "8" * 40), (("operation_id",), uid(99)),
            (("issued_at",), stamp(ORIGIN_TIME - dt.timedelta(minutes=2))),
            (("expires_at",), stamp(ORIGIN_TIME + dt.timedelta(minutes=59))),
            (("plan_sha256",), "0" * 64), (("protected_descriptor_sha256",), "0" * 64),
            (("fixture_evidence", "result_sha256"), "0" * 64),
            (("fixture_evidence", "artifact_id"), "99999"),
            (("driver_evidence", "digest"), "sha256:" + "0" * 64),
            (("driver_evidence", "run_id"), "99999"),
        )
        for path, value in changes:
            with self.subTest(path=path):
                self.reject_value(("original",) + path, value)

    def test_descriptor_hmac_plan_scope_control_and_operation_cannot_change(self):
        changes = (
            (("hmac_key_base64",), base64.b64encode(b"different-synthetic-key-32bytes!!").decode()),
            (("control_sha",), "8" * 40), (("operation_id",), uid(99)),
            (("public_packet_sha256",), "0" * 64), (("fixture_descriptor_sha256",), "0" * 64),
            (("plan", "region"), "nyc"), (("plan", "ledger", "user"), "other_user"),
            (("scope_review",), {}),
        )
        for path, value in changes:
            with self.subTest(path=path):
                self.reject_value(("descriptor",) + path, value)

    def test_rebinding_altered_descriptor_does_not_change_frozen_original_authority(self):
        def mutate(c):
            c["descriptor"]["hmac_key_base64"] = base64.b64encode(b"q" * 32).decode()
            samples.rebind(c["original"], c["descriptor"])
        self.reject(mutate)

    def test_origin_run_exact_identity_workflow_branch_event_status_title(self):
        changes = {
            "id": 45679, "workflow_id": 67891, "path": ".github/workflows/other.yml",
            "head_sha": "8" * 40, "head_branch": "feature", "event": "push",
            "display_title": boot.RUN_TITLES["check"], "status": "in_progress", "conclusion": "success",
            "repository": {"full_name": "synthetic/unbound"},
            "created_at": stamp(ORIGIN_TIME + dt.timedelta(seconds=1)),
        }
        for key, value in changes.items():
            with self.subTest(key=key):
                self.reject_value(("origin_run", key), value)

    def test_attempt_one_is_not_bool_string_or_rerun_and_predecessor_is_absent(self):
        for value in (True, "1", 0, 2):
            with self.subTest(value_type=type(value).__name__):
                self.reject_value(("origin_run", "run_attempt"), value)
        self.reject_value(("origin_run", "previous_attempt_url"), "https://private.invalid/" + PRIVATE)
        self.reject(lambda c: c["origin_run"].pop("previous_attempt_url"))
        self.reject_value(("origin_run", "workflow_id"), 67890.0)
        self.reject_value(("origin_run", "workflow_id"), "67890")

    def test_origin_artifact_inventory_is_exact_empty_and_closed(self):
        for value in ({"total_count": 1, "artifacts": [{"id": 99}]},
                      {"total_count": 0, "artifacts": [{"id": 99}]},
                      {"total_count": False, "artifacts": []},
                      {"total_count": 0, "artifacts": [], "next": PRIVATE},
                      {"artifacts": []}):
            with self.subTest(keys=tuple(value)):
                self.reject_value(("origin_artifacts",), value)

    def test_historical_run_must_fall_inside_original_bounded_window(self):
        # Patching only the test incident digest removes the outer exact-hash
        # rejection to independently exercise the historical lifetime guard.
        windows = ((ORIGIN_TIME, ORIGIN_TIME),
                   (ORIGIN_TIME - dt.timedelta(hours=1), ORIGIN_TIME),
                   (ORIGIN_TIME + dt.timedelta(seconds=1), ORIGIN_TIME + dt.timedelta(hours=1)),
                   (ORIGIN_TIME - dt.timedelta(hours=1), ORIGIN_TIME + dt.timedelta(hours=2)))
        for issued, expires in windows:
            case = copy.deepcopy(self.case)
            case["original"].update(issued_at=stamp(issued), expires_at=stamp(expires))
            samples.rebind(case["original"], case["descriptor"])
            digest = common.sha256_value(case["original"])
            case["authorization"]["origin_authorization_sha256"] = digest
            with self.subTest(issued=stamp(issued)), mock.patch.object(recovery, "ORIGIN_PACKET_SHA256", digest):
                with self.assertRaises(common.ReleaseError):
                    self.inspect(case)


class OwnerAndInventoryTests(RecoveryFixtures):
    def test_user_team_and_owner_are_distinct_pinned_roles(self):
        self.assertNotEqual(self.user_id, self.team_id)
        self.assertFalse(self.inspect()["ready_for_finalize"])
        for path, value in ((("account", "uuid"), uid(99)), (("account", "uuid"), self.team_id),
                            (("account", "team", "uuid"), self.user_id),
                            (("account", "team", "uuid"), uid(99)),
                            (("app", "owner_uuid"), self.user_id), (("app", "owner_uuid"), uid(99)),
                            (("account", "status"), "inactive"), (("account", "team"), None)):
            with self.subTest(path=path):
                self.reject_snapshot(lambda s: put(s, path, value), both=True)

    def test_exact_app_and_deployment_ids_never_latest_name_or_replacements(self):
        for path in (("app", "id"), ("deployments", "deployments", 0, "id")):
            for value in (uid(99), "latest", "rereply-canary-driver-test", None):
                with self.subTest(path=path, kind=type(value).__name__):
                    self.reject_snapshot(lambda s: put(s, path, value), both=True)

    def test_same_selector_wrong_name_and_same_name_wrong_selector_fail(self):
        self.reject_snapshot(lambda s: s["apps"]["apps"][2]["spec"].update(name="wrong-name"), both=True)
        self.reject_snapshot(lambda s: s["apps"]["apps"][2].update(id=uid(99)), both=True)
        self.reject_snapshot(lambda s: s["app"]["spec"].update(name="wrong-name"), both=True)

    def test_competing_driver_namespace_fails_even_with_rebound_inventory_hash(self):
        def mutate(c):
            for s in (c["first"], c["second"]):
                s["apps"]["apps"][1]["spec"]["name"] = "rereply-canary-driver-competing"
            rows = sorted([{"id": r["id"], "name": r["spec"]["name"]}
                           for r in c["first"]["apps"]["apps"]], key=lambda r: r["id"])
            c["authorization"]["apps_inventory_sha256"] = common.sha256_value(rows)
        self.reject(mutate)

    def test_apps_count_missing_extra_duplicate_and_hash_drift_fail(self):
        changes = (
            lambda s: s["apps"]["meta"].update(total=2),
            lambda s: s["apps"]["meta"].update(total="3"),
            lambda s: s["apps"]["apps"].pop(),
            lambda s: s["apps"]["apps"].append({"id": uid(99), "spec": {"name": "extra"}}),
            lambda s: s["apps"]["apps"].__setitem__(1, copy.deepcopy(s["apps"]["apps"][0])),
            lambda s: s["apps"]["apps"][0]["spec"].update(name="changed-production-name"),
            lambda s: s["apps"]["apps"][0].update(id=uid(99)),
            lambda s: s["apps"].update(links={"pages": {"next": PRIVATE}}),
        )
        for index, change in enumerate(changes):
            with self.subTest(case=index):
                self.reject_snapshot(change, both=True)

    def test_deployment_inventory_requires_exactly_two_complete_unique_originals(self):
        changes = (
            lambda s: s["deployments"]["meta"].update(total=0),
            lambda s: s["deployments"]["meta"].update(total=True),
            lambda s: s["deployments"]["meta"].update(total=2.0),
            lambda s: s["deployments"]["meta"].update(total="2"),
            lambda s: s["deployments"]["deployments"].clear(),
            lambda s: s["deployments"]["deployments"].pop(),
            lambda s: s["deployments"]["deployments"][1].update(id=self.deployment_id),
            lambda s: s["deployments"]["deployments"][1].update(id=uid(99)),
            lambda s: s["deployments"]["deployments"].append(copy.deepcopy(s["deployments"]["deployments"][0])),
            lambda s: s["deployments"].update(links={"pages": {"next": PRIVATE}}),
        )
        for index, change in enumerate(changes):
            with self.subTest(case=index):
                self.reject_snapshot(change, both=True)

    def test_deployment_phase_cannot_be_promoted_or_reflected_unchecked(self):
        for index in (0, 1):
            for phase in ("ACTIVE", "PENDING_BUILD", "CANCELED", "SUPERSEDED", "error", PRIVATE, None):
                with self.subTest(index=index, phase_type=type(phase).__name__):
                    self.reject_snapshot(lambda s: s["deployments"]["deployments"][index].update(phase=phase), both=True)

    def test_no_active_pending_inprogress_or_pinned_slot(self):
        for key in ("active_deployment", "pending_deployment", "in_progress_deployment", "pinned_deployment"):
            for identity in (self.deployment_id, self.redeployed_deployment_id, uid(99)):
                with self.subTest(slot=key, original=identity == self.deployment_id):
                    self.reject_snapshot(lambda s: s["app"].update({key: {"id": identity}}), both=True)

    def test_empty_slot_values_are_only_null_or_empty_object(self):
        for key in ("active_deployment", "pending_deployment", "in_progress_deployment", "pinned_deployment"):
            for value in (False, 0, [], ""):
                with self.subTest(slot=key, value_type=type(value).__name__):
                    self.reject_snapshot(lambda s: s["app"].update({key: value}), both=True)
        case = copy.deepcopy(self.case)
        for s in (case["first"], case["second"]):
            s["app"].update(active_deployment=None, pending_deployment={},
                            in_progress_deployment=None, pinned_deployment={})
        self.assertEqual(self.inspect(case), self.inspect())


class DriverMetadataTests(RecoveryFixtures):
    def test_metadata_projection_is_closed_identity_timestamp_only_and_order_independent(self):
        first = self.case["first"]
        before = copy.deepcopy(first)
        actual = recovery._driver_metadata(first["app"], first["deployments"]["deployments"])
        self.assertEqual(actual, driver_metadata(first))
        self.assertEqual(common.sha256_value(actual), self.case["authorization"]["driver_metadata_sha256"])
        self.assertEqual(first, before)
        for row in [actual["app"]] + actual["deployments"]:
            self.assertEqual(set(row), {"id", "created_at", "updated_at"})
        reordered = list(reversed(first["deployments"]["deployments"]))
        self.assertEqual(recovery._driver_metadata(first["app"], reordered), actual)

    def test_all_six_creation_and_update_fields_are_required_and_strict(self):
        for root in (("app",), ("deployments", "deployments", 0), ("deployments", "deployments", 1)):
            for key in ("created_at", "updated_at"):
                for value in (True, False, 0, None, [], {}, "yesterday", "2026-01-01T00:00:00+00:00"):
                    with self.subTest(root=root, key=key, value_type=type(value).__name__):
                        self.reject_snapshot(lambda s: put(s, root + (key,), value), both=True)
                def remove(s):
                    row = s
                    for component in root:
                        row = row[component]
                    row.pop(key)
                with self.subTest(root=root, missing=key):
                    self.reject_snapshot(remove, both=True)

    def test_timestamp_drift_before_or_between_captures_fails(self):
        for root in (("app",), ("deployments", "deployments", 0), ("deployments", "deployments", 1)):
            for key in ("created_at", "updated_at"):
                for both in (False, True):
                    def mutate(s):
                        row = s
                        for component in root:
                            row = row[component]
                        row[key] = stamp(common.require_timestamp(row[key], "synthetic time") + dt.timedelta(seconds=1))
                    with self.subTest(root=root, key=key, both=both):
                        self.reject_snapshot(mutate, both=both)

    def test_source_pinned_metadata_cannot_be_rebound_by_fresh_hash(self):
        paths = (("app", "updated_at"),
                 ("deployments", "deployments", 0, "created_at"),
                 ("deployments", "deployments", 0, "updated_at"),
                 ("deployments", "deployments", 1, "created_at"),
                 ("deployments", "deployments", 1, "updated_at"))
        for path in paths:
            case = copy.deepcopy(self.case)
            for snapshot in (case["first"], case["second"]):
                row = snapshot
                for component in path[:-1]:
                    row = row[component]
                row[path[-1]] = stamp(common.require_timestamp(row[path[-1]], "synthetic time") + dt.timedelta(seconds=1))
            case["authorization"]["driver_metadata_sha256"] = common.sha256_value(driver_metadata(case["first"]))
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_app_creation_canonical_encoding_and_order_cannot_be_rebound(self):
        original = self.case["first"]["app"]["created_at"]
        variants = (original.replace("-", ""), original.replace("Z", ".000Z"),
                    stamp(self.redeployed_created + dt.timedelta(minutes=2)))
        for value in variants:
            case = copy.deepcopy(self.case)
            for snapshot in (case["first"], case["second"]):
                snapshot["app"]["created_at"] = value
            case["authorization"]["driver_metadata_sha256"] = common.sha256_value(driver_metadata(case["first"]))
            with self.subTest(value=value), self.assertRaises(common.ReleaseError):
                self.inspect(case)


class RuntimeAndStateTests(RecoveryFixtures):
    def test_observed_and_expected_numeric_types_cannot_be_coerced(self):
        for path, value in ((("services", 0, "instance_count"), True),
                            (("services", 0, "instance_count"), 1.0),
                            (("services", 0, "http_port"), 8080.0)):
            with self.subTest(path=path, value_type=type(value).__name__):
                def mutate(s):
                    put(s["app"]["spec"], path, value)
                    put(s["deployments"]["deployments"][0]["spec"], path, value)
                self.reject_snapshot(mutate, both=True)
                self.reject_value(("expected_spec",) + path, value)

    def test_expected_hmac_fixture_and_environment_shape_keep_original_custody(self):
        envs = self.case["expected_spec"]["services"][0]["envs"]
        for name, value in (("CRM_CANARY_HMAC_KEY_BASE64", base64.b64encode(b"q" * 32).decode()),
                            ("CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "{}"),
                            ("CRM_CANARY_DRIVER_VERSION_SHA256", "0" * 64)):
            index = next(i for i, e in enumerate(envs) if e["key"] == name)
            with self.subTest(name=name):
                self.reject_value(("expected_spec", "services", 0, "envs", index, "value"), value)
        self.reject(lambda c: c["expected_spec"]["services"][0]["envs"].pop())
        self.reject(lambda c: c["expected_spec"]["services"][0]["envs"].append(copy.deepcopy(envs[0])))

    def test_runtime_spec_region_size_count_ledger_image_and_public_values_drift(self):
        changes = (
            (("region",), "nyc"), (("services", 0, "instance_count"), 2),
            (("services", 0, "instance_size_slug"), "apps-s-2vcpu-4gb"),
            (("services", 0, "http_port"), 8081),
            (("services", 0, "image", "digest"), "sha256:" + "0" * 64),
            (("services", 0, "image", "repository"), "other-image"),
            (("services", 0, "image", "deploy_on_push", "enabled"), True),
            (("databases", 0, "cluster_name"), "other-ledger"),
            (("databases", 0, "db_name"), "other_database"),
            (("databases", 0, "db_user"), "other_user"),
            (("services", 0, "health_check", "http_path"), "/v1/execute"),
        )
        for path, value in changes:
            with self.subTest(path=path):
                def change(s):
                    put(s["app"]["spec"], path, value)
                    put(s["deployments"]["deployments"][0]["spec"], path, value)
                self.reject_snapshot(change, both=True)

    def test_expected_digest_cannot_be_changed_with_matching_provider_specs(self):
        def mutate(c):
            c["expected_spec"]["services"][0]["image"]["digest"] = "sha256:" + "0" * 64
            for s in (c["first"], c["second"]):
                for spec in (s["app"]["spec"], s["deployments"]["deployments"][0]["spec"]):
                    spec["services"][0]["image"]["digest"] = "sha256:" + "0" * 64
        self.reject(mutate)

    def test_expected_and_observed_runtime_cannot_drift_together(self):
        changes = (
            (("region",), "nyc"), (("services", 0, "instance_count"), 2),
            (("services", 0, "instance_size_slug"), "apps-s-2vcpu-4gb"),
            (("databases", 0, "cluster_name"), "unbound-ledger"),
            (("databases", 0, "db_name"), "unbound-database"),
            (("databases", 0, "db_user"), "unbound-user"),
            (("services", 0, "envs", -1, "value"), "unbound-ledger-expression"),
        )
        for path, value in changes:
            with self.subTest(path=path):
                def mutate(c):
                    put(c["expected_spec"], path, value)
                    for s in (c["first"], c["second"]):
                        put(s["app"]["spec"], path, value)
                        put(s["deployments"]["deployments"][0]["spec"], path, value)
                self.reject(mutate)

    def test_app_deployment_spec_mismatch_cannot_pass(self):
        self.reject_snapshot(lambda s: s["deployments"]["deployments"][0]["spec"]["services"][0].update(http_port=8081))

    def test_each_historical_deployment_retains_exact_runtime_and_ciphertexts(self):
        for index in (0, 1):
            for path, value in ((("services", 0, "http_port"), 8081),
                                (("services", 0, "image", "digest"), "sha256:" + "0" * 64),
                                (("services", 0, "envs", 0, "value"), "EV[changed-synthetic-ciphertext]")):
                with self.subTest(deployment=index, path=path):
                    self.reject_snapshot(lambda s: put(s["deployments"]["deployments"][index]["spec"], path, value),
                                         both=True)

    def test_runtime_environment_count_duplicates_scope_type_plaintext_fail(self):
        changes = (
            lambda envs: envs.pop(),
            lambda envs: envs.append(copy.deepcopy(envs[0])),
            lambda envs: envs.__setitem__(1, copy.deepcopy(envs[0])),
            lambda envs: envs[0].update(scope="BUILD_TIME"),
            lambda envs: envs[0].update(type="GENERAL"),
            lambda envs: envs[0].update(value=PRIVATE),
            lambda envs: envs[0].update(value="EV[bad\nvalue]"),
            lambda envs: envs[-1].update(value="postgres://" + PRIVATE),
        )
        for index, change in enumerate(changes):
            with self.subTest(case=index):
                def mutate(s):
                    change(s["app"]["spec"]["services"][0]["envs"])
                    change(s["deployments"]["deployments"][0]["spec"]["services"][0]["envs"])
                self.reject_snapshot(mutate, both=True)

    def test_ciphertext_must_match_app_deployment_and_across_pair(self):
        self.reject_value(("first", "app", "spec", "services", 0, "envs", 0, "value"), "EV[changed-private-ciphertext]")
        def mutate(c):
            for spec in (c["second"]["app"]["spec"], c["second"]["deployments"]["deployments"][0]["spec"]):
                spec["services"][0]["envs"][0]["value"] = "EV[changed-private-ciphertext]"
        self.reject(mutate)

    def test_every_one_of_five_secret_ciphertexts_is_stable_across_pair(self):
        envs = self.case["second"]["app"]["spec"]["services"][0]["envs"]
        secret_keys = [row["key"] for row in envs if row["type"] == "SECRET"]
        self.assertEqual(len(secret_keys), 5)
        for secret_key in secret_keys:
            def mutate(c):
                for spec in snapshot_specs(c["second"]):
                    next(row for row in spec["services"][0]["envs"]
                         if row["key"] == secret_key)["value"] = "EV[changed-synthetic-ciphertext]"
            with self.subTest(secret_key=secret_key):
                self.reject(mutate)

    def test_nonempty_additional_runtime_components_and_unknown_fields_fail(self):
        for key, value in (("workers", [{"name": "unbound"}]), ("jobs", [{"name": "unbound"}]),
                           ("extra", PRIVATE)):
            with self.subTest(key=key):
                self.reject_snapshot(lambda s: s["app"]["spec"].update({key: value}), both=True)

    def test_firewall_broadening_replacement_duplicates_or_missing_admission_fail(self):
        rules = self.case["first"]["firewall"]["rules"]
        variants = ([], rules[:1], rules + [{"type": "app", "value": self.app_id}],
                    rules + [{"type": "ip_addr", "value": "0.0.0.0/0"}],
                    rules + [{"type": "ip_addr", "value": "8.8.8.8/32"}],
                    rules + copy.deepcopy(rules), [{"type": "app", "value": uid(99)}],
                    [{"type": "tag", "value": "unbound"}])
        for index, value in enumerate(variants):
            with self.subTest(case=index):
                self.reject_snapshot(lambda s: s["firewall"].update(rules=value), both=True)

    def test_firewall_original_history_cannot_be_rebound_to_include_driver_admission(self):
        case = copy.deepcopy(self.case)
        rules = case["first"]["firewall"]["rules"]
        rules = sorted(rules, key=lambda r: (r["type"], r["value"]))
        for s in (case["first"], case["second"]):
            s["firewall"]["rules"] = copy.deepcopy(rules)
        case["descriptor"]["plan"]["ledger"]["firewall_sha256"] = common.sha256_value(rules)
        samples.rebind(case["original"], case["descriptor"])
        digest = common.sha256_value(case["original"])
        case["authorization"]["origin_authorization_sha256"] = digest
        with mock.patch.object(recovery, "ORIGIN_PACKET_SHA256", digest), self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_firewall_provider_cluster_binding_is_exact_when_present(self):
        self.reject_snapshot(lambda s: s["firewall"]["rules"][0].update(cluster_uuid=uid(99)), both=True)
        case = copy.deepcopy(self.case)
        for s in (case["first"], case["second"]):
            s["firewall"]["rules"][0]["cluster_uuid"] = case["descriptor"]["plan"]["ledger"]["cluster_id"]
        self.assertEqual(self.inspect(case), self.inspect())

    def test_firewall_fresh_hash_cannot_authorize_broadening_or_replacement(self):
        for replacement in ([{"type": "app", "value": uid(99)}, {"type": "app", "value": self.app_id}],
                            [{"type": "app", "value": uid(10)}],
                            self.case["first"]["firewall"]["rules"] + [{"type": "ip_addr", "value": "0.0.0.0/0"}]):
            case = copy.deepcopy(self.case)
            for s in (case["first"], case["second"]):
                s["firewall"]["rules"] = copy.deepcopy(replacement)
            normalized = sorted(replacement, key=lambda row: (row["type"], row["value"]))
            case["authorization"]["firewall_sha256"] = common.sha256_value(normalized)
            with self.subTest(rule_count=len(replacement)), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_admitted_firewall_order_is_not_a_resource_change(self):
        case = copy.deepcopy(self.case)
        case["second"]["firewall"]["rules"].reverse()
        self.assertEqual(self.inspect(case), self.inspect())

    def test_canary_secret_addition_removal_duplicate_metadata_or_variable_drift_fail(self):
        changes = (
            lambda e: e["secrets"].append({"name": "CRM_CANARY_DRIVER_CONFIG_JSON"}),
            lambda e: e["secrets"].pop(),
            lambda e: e["secrets"].__setitem__(1, copy.deepcopy(e["secrets"][0])),
            lambda e: e["secrets"][0].update(updated_at=stamp(NOW)),
            lambda e: e["variables"].append({"name": "EXTRA", "value": PRIVATE}),
            lambda e: e["branch_policies"][0].update(name="feature"),
        )
        for index, change in enumerate(changes):
            with self.subTest(case=index):
                self.reject_snapshot(lambda s: change(s["canary_environment"]), both=True)

    def test_canary_config_shape_cannot_be_rebound_to_missing_blocker_claim(self):
        case = copy.deepcopy(self.case)
        for s in (case["first"], case["second"]):
            s["canary_environment"]["secrets"].append({"name": "CRM_CANARY_DRIVER_CONFIG_JSON"})
        case["descriptor"]["plan"]["github_environment_sha256"] = common.sha256_value(case["first"]["canary_environment"])
        samples.rebind(case["original"], case["descriptor"])
        digest = common.sha256_value(case["original"])
        case["authorization"]["origin_authorization_sha256"] = digest
        with mock.patch.object(recovery, "ORIGIN_PACKET_SHA256", digest), self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_fixture_environment_and_production_state_must_match_fresh_authority(self):
        for key in ("fixture_environment_sha256", "production_state_sha256"):
            with self.subTest(key=key):
                self.reject_snapshot(lambda s: s.update({key: "0" * 64}), both=True)


class ExactIngressTests(RecoveryFixtures):
    def modern_case(self):
        case = copy.deepcopy(self.case)
        for s in (case["first"], case["second"]):
            for spec in snapshot_specs(s):
                service = spec["services"][0]
                service.pop("routes")
                spec["ingress"] = {"rules": [{"component": {"name": service["name"]},
                                              "match": {"path": {"prefix": "/"}}}]}
        return case

    def reject_modern(self, change):
        case = self.modern_case()
        for s in (case["first"], case["second"]):
            for spec in snapshot_specs(s):
                change(spec)
        with self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_exact_modern_single_root_route_is_equivalent_without_input_mutation(self):
        case = self.modern_case()
        before = copy.deepcopy(case)
        self.assertEqual(self.inspect(case), self.inspect())
        self.assertEqual(case, before)

    def test_routing_representation_cannot_change_between_observations(self):
        case = self.modern_case()
        case["first"] = copy.deepcopy(self.case["first"])
        with self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_mixed_routing_systems_fail_even_if_legacy_routes_are_empty(self):
        for routes in ([], [{"path": "/"}], None, False):
            with self.subTest(kind=type(routes).__name__):
                self.reject_modern(lambda spec: spec["services"][0].update(routes=routes))

    def test_root_and_rule_extra_fields_cannot_be_discarded(self):
        for key, value in (("custom_error_page_url", PRIVATE), ("unknown", None)):
            with self.subTest(key=key):
                self.reject_modern(lambda spec: spec["ingress"].update({key: value}))
        for key, value in (("cors", {}), ("redirect", {}), ("unknown", PRIVATE)):
            with self.subTest(key=key):
                self.reject_modern(lambda spec: spec["ingress"]["rules"][0].update({key: value}))

    def test_path_host_and_component_must_be_exact(self):
        paths = (("rules", 0, "component", "name"), ("rules", 0, "match", "path", "prefix"))
        for path in paths:
            for value in (PRIVATE, "", None, True, 0, "/v1/execute"):
                with self.subTest(path=path, kind=type(value).__name__):
                    self.reject_modern(lambda spec: put(spec["ingress"], path, value))
        self.reject_modern(lambda spec: spec["ingress"]["rules"][0]["match"].update(authority={"exact": PRIVATE}))
        self.reject_modern(lambda spec: spec["ingress"]["rules"][0]["match"]["path"].update(exact="/"))

    def test_preserve_and_rewrite_flags_are_not_silently_interpreted(self):
        for key in ("preserve_path_prefix", "rewrite"):
            for value in (False, True, "false", "true", "", "/", None):
                with self.subTest(key=key, kind=type(value).__name__):
                    self.reject_modern(lambda spec: spec["ingress"]["rules"][0]["component"].update({key: value}))

    def test_rule_cardinality_types_and_missing_routing_fail(self):
        for ingress in (None, [], {}, {"rules": []}, {"rules": {}}, {"rules": [None]},
                        {"rules": [{}, {}]}, False):
            with self.subTest(kind=type(ingress).__name__):
                self.reject_modern(lambda spec: spec.update(ingress=ingress))
        self.reject_modern(lambda spec: spec["ingress"]["rules"].append(copy.deepcopy(spec["ingress"]["rules"][0])))
        self.reject_modern(lambda spec: spec.pop("ingress"))

    def test_unknown_malformed_duplicate_or_combined_features_are_never_erased(self):
        for features in ([PRIVATE], None, {}, False, "buildpack-stack=ubuntu-22",
                         ["buildpack-stack=ubuntu-24"], ["buildpack-stack=ubuntu-22", PRIVATE],
                         ["buildpack-stack=ubuntu-22", "buildpack-stack=ubuntu-22"], [False], [None]):
            with self.subTest(kind=type(features).__name__):
                self.reject_modern(lambda spec: spec.update(features=features))

    def test_exact_inert_feature_or_empty_features_are_accepted_without_input_mutation(self):
        for features in ([], ["buildpack-stack=ubuntu-22"]):
            case = self.modern_case()
            for s in (case["first"], case["second"]):
                for spec in snapshot_specs(s):
                    spec["features"] = copy.deepcopy(features)
            before = copy.deepcopy(case)
            with self.subTest(feature_count=len(features)):
                self.assertEqual(self.inspect(case), self.inspect())
                self.assertEqual(case, before)

    def test_allowed_feature_representation_cannot_change_between_observations(self):
        for features in ([], ["buildpack-stack=ubuntu-22"]):
            case = self.modern_case()
            for spec in snapshot_specs(case["second"]):
                spec["features"] = copy.deepcopy(features)
            with self.subTest(feature_count=len(features)), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_normalization_does_not_hide_other_spec_or_ciphertext_drift(self):
        self.reject_modern(lambda spec: spec["services"][0].update(instance_count=True))
        self.reject_modern(lambda spec: spec["services"][0]["image"].update(digest="sha256:" + "0" * 64))
        case = self.modern_case()
        for spec in (case["second"]["app"]["spec"], case["second"]["deployments"]["deployments"][0]["spec"]):
            next(env for env in spec["services"][0]["envs"] if env["type"] == "SECRET")["value"] = "EV[changed-synthetic-ciphertext]"
        with self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_normalization_cannot_authorize_a_different_expected_route(self):
        for routes in ([{"path": "/different"}], [], [{"path": "/", "preserve_path_prefix": True}]):
            case = self.modern_case()
            case["expected_spec"]["services"][0]["routes"] = routes
            with self.subTest(routes_shape=len(routes)), self.assertRaises(common.ReleaseError):
                self.inspect(case)


class HistoricalExpectedSpecTests(RecoveryFixtures):
    def historical(self, case=None, protected=None, driver=None):
        c = self.case if case is None else case
        return recovery.historical_expected_spec(
            c["original"], c["descriptor"], self.protected if protected is None else protected,
            self.driver if driver is None else driver)

    def test_historical_wrapper_removes_only_two_exact_login_schema_fields(self):
        before = copy.deepcopy((self.case, self.protected, self.driver))
        current = boot.runtime_spec(self.case["original"], self.case["descriptor"], self.protected, self.driver)
        expected = copy.deepcopy(current)
        for row in expected["services"][0]["envs"]:
            if row["key"] in LOGIN_KEYS:
                login = common.loads_strict(row["value"])
                self.assertEqual(set(login), {"schema_version", "email", "password"})
                version = login.pop("schema_version")
                self.assertIs(type(version), int)
                self.assertEqual(version, 1)
                row["value"] = common.canonical_payload_bytes(login).decode()
        historical = self.historical()
        self.assertEqual(common.canonical_payload_bytes(historical), common.canonical_payload_bytes(expected))
        self.assertEqual(historical, self.case["expected_spec"])
        self.assertEqual((self.case, self.protected, self.driver), before)
        rows = {row["key"]: row for row in historical["services"][0]["envs"]}
        self.assertEqual(rows["CRM_CANARY_HMAC_KEY_BASE64"]["value"], self.case["descriptor"]["hmac_key_base64"])
        self.assertEqual(common.loads_strict(rows["CRM_CANARY_FIXTURE_DESCRIPTOR_JSON"]["value"]), self.driver)

    def test_historical_wrapper_cannot_hide_bad_current_login_schema_or_extra_fields(self):
        current = boot.runtime_spec(self.case["original"], self.case["descriptor"], self.protected, self.driver)
        for key in LOGIN_KEYS:
            for version in (True, False, 1.0, "1", 0, 2, None):
                bad = copy.deepcopy(current)
                row = next(row for row in bad["services"][0]["envs"] if row["key"] == key)
                login = common.loads_strict(row["value"])
                login["schema_version"] = version
                row["value"] = common.canonical_payload_bytes(login).decode()
                with (self.subTest(key=key, version_type=type(version).__name__),
                      mock.patch.object(boot, "runtime_spec", return_value=bad),
                      self.assertRaises(common.ReleaseError)):
                    self.historical()
            for field, replacement in (("schema_version", None), ("email", None), ("password", None),
                                        ("extra", PRIVATE)):
                bad = copy.deepcopy(current)
                row = next(row for row in bad["services"][0]["envs"] if row["key"] == key)
                login = common.loads_strict(row["value"])
                if replacement is None:
                    login.pop(field)
                else:
                    login[field] = replacement
                row["value"] = common.canonical_payload_bytes(login).decode()
                with (self.subTest(key=key, field=field), mock.patch.object(boot, "runtime_spec", return_value=bad),
                      self.assertRaises(common.ReleaseError)):
                    self.historical()

    def test_historical_login_expected_values_cannot_already_have_any_schema_version(self):
        for key in LOGIN_KEYS:
            for version in (1, True, 1.0, "1", None, 2):
                case = copy.deepcopy(self.case)
                row = next(row for row in case["expected_spec"]["services"][0]["envs"] if row["key"] == key)
                login = common.loads_strict(row["value"])
                login["schema_version"] = version
                row["value"] = common.canonical_payload_bytes(login).decode()
                with self.subTest(key=key, version_type=type(version).__name__), self.assertRaises(common.ReleaseError):
                    self.inspect(case)

    def test_historical_wrapper_still_binds_original_packet_descriptor_and_fixture(self):
        for path, value in (
            (("original", "control_sha"), CONTROL),
            (("original", "driver_evidence", "digest"), "sha256:" + "0" * 64),
            (("descriptor", "hmac_key_base64"), base64.b64encode(b"q" * 32).decode()),
            (("descriptor", "fixture_descriptor_sha256"), "0" * 64),
        ):
            case = copy.deepcopy(self.case)
            put(case, path, value)
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.historical(case)

    def test_historical_wrapper_and_public_report_do_not_emit_private_material(self):
        with (mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            historical = self.historical()
            report = self.inspect()
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")
        encoded = common.canonical_payload_bytes(report).decode()
        for row in historical["services"][0]["envs"]:
            if row["type"] == "SECRET":
                self.assertNotIn(row["value"], encoded)
        for credential in self.protected["credentials"].values():
            if type(credential) is str:
                self.assertNotIn(credential, encoded)


class TargetUpdatePlanTests(RecoveryFixtures):
    def plan(self, case=None, observed=None, evidence=None):
        c = self.case if case is None else case
        return recovery.target_update_plan(
            c["original"], c["descriptor"], c["expected_spec"],
            c["authorization"]["target_driver_evidence"] if evidence is None else evidence,
            c["first"]["app"]["spec"] if observed is None else observed,
            control_sha=c["control_sha"])

    def test_private_plan_has_exactly_image_version_and_two_login_value_delta(self):
        before = copy.deepcopy(self.case)
        observed = self.case["first"]["app"]["spec"]
        expected = copy.deepcopy(observed)
        evidence = self.case["authorization"]["target_driver_evidence"]
        expected["services"][0]["image"]["digest"] = evidence["digest"]
        version = next(row for row in expected["services"][0]["envs"]
                       if row["key"] == "CRM_CANARY_DRIVER_VERSION_SHA256")
        self.assertEqual(version["type"], "GENERAL")
        self.assertEqual(version["scope"], "RUN_TIME")
        version["value"] = evidence["driver_version_sha256"]
        original_envs = {row["key"]: row for row in self.case["expected_spec"]["services"][0]["envs"]}
        for row in expected["services"][0]["envs"]:
            if row["key"] in LOGIN_KEYS:
                login = common.loads_strict(original_envs[row["key"]]["value"])
                self.assertEqual(set(login), {"email", "password"})
                login["schema_version"] = 1
                row["value"] = common.canonical_payload_bytes(login).decode()
        actual = self.plan()
        self.assertEqual(common.canonical_payload_bytes(actual), common.canonical_payload_bytes(expected))
        self.assertEqual(self.case, before)
        old_secrets = {row["key"]: row for row in observed["services"][0]["envs"] if row["type"] == "SECRET"}
        new_secrets = {row["key"]: row for row in actual["services"][0]["envs"] if row["type"] == "SECRET"}
        self.assertEqual(len(old_secrets), 5)
        self.assertEqual(len(new_secrets), 5)
        stable_secrets = set(old_secrets) - set(LOGIN_KEYS)
        self.assertEqual(len(stable_secrets), 3)
        for key in stable_secrets:
            self.assertEqual(new_secrets[key], old_secrets[key])
            self.assertTrue(new_secrets[key]["value"].startswith("EV["))
        for key in LOGIN_KEYS:
            self.assertNotEqual(new_secrets[key]["value"], old_secrets[key]["value"])
            self.assertEqual({k: v for k, v in new_secrets[key].items() if k != "value"},
                             {k: v for k, v in old_secrets[key].items() if k != "value"})
            parsed = common.loads_strict(new_secrets[key]["value"])
            self.assertEqual(set(parsed), {"schema_version", "email", "password"})
            self.assertIs(type(parsed["schema_version"]), int)
            self.assertEqual(parsed["schema_version"], 1)
            del parsed["schema_version"]
            self.assertEqual(parsed, common.loads_strict(original_envs[key]["value"]))

    def test_private_plan_is_independent_and_preserves_actual_layout_and_env_order(self):
        case = copy.deepcopy(self.case)
        observed = case["first"]["app"]["spec"]
        service = observed["services"][0]
        service.pop("routes")
        service["envs"].reverse()
        observed["ingress"] = {"rules": [{"component": {"name": service["name"]},
                                           "match": {"path": {"prefix": "/"}}}]}
        observed["features"] = ["buildpack-stack=ubuntu-22"]
        before = copy.deepcopy(case)
        planned = self.plan(case)
        self.assertEqual(planned["ingress"], observed["ingress"])
        self.assertEqual(planned["features"], observed["features"])
        self.assertNotIn("routes", planned["services"][0])
        self.assertEqual([row["key"] for row in planned["services"][0]["envs"]],
                         [row["key"] for row in service["envs"]])
        planned["services"][0]["envs"][0]["value"] = PRIVATE
        planned["ingress"]["rules"][0]["match"]["path"]["prefix"] = "/changed"
        planned["features"].append(PRIVATE)
        self.assertEqual(case, before)

    def test_corrected_login_values_reuse_original_fixture_accounts_and_passwords(self):
        before = copy.deepcopy((self.case, self.protected, self.driver))
        planned = self.plan()
        rows = {row["key"]: row for row in planned["services"][0]["envs"]}
        for key, role in ((LOGIN_KEYS[0], "klinik"), (LOGIN_KEYS[1], "non_klinik")):
            with self.subTest(role=role):
                self.assertEqual(common.loads_strict(rows[key]["value"]), {
                    "schema_version": 1,
                    "email": self.protected["registration"][role + "_email"],
                    "password": self.protected["credentials"][role + "_password"],
                })
                self.assertEqual(rows[key]["type"], "SECRET")
                self.assertEqual(rows[key]["scope"], "RUN_TIME")
        stored_rows = {row["key"]: row for row in self.case["first"]["app"]["spec"]["services"][0]["envs"]}
        for key in ("CRM_CANARY_HMAC_KEY_BASE64", "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "CRM_CANARY_META_APP_SECRET"):
            self.assertEqual(rows[key], stored_rows[key])
        self.assertEqual(rows["CRM_CANARY_LEDGER_DATABASE_URL"], stored_rows["CRM_CANARY_LEDGER_DATABASE_URL"])
        self.assertEqual((self.case, self.protected, self.driver), before)

    def test_planner_rejects_unbound_image_version_secret_shape_and_other_resource_drift(self):
        for path, value in (
            (("services", 0, "image", "digest"), "sha256:" + "0" * 64),
            (("services", 0, "image", "repository"), PRIVATE),
            (("services", 0, "instance_count"), True),
            (("services", 0, "http_port"), 8081),
            (("services", 0, "envs", 0, "value"), PRIVATE),
            (("services", 0, "envs", 0, "type"), "GENERAL"),
            (("services", 0, "envs", 0, "scope"), "BUILD_TIME"),
            (("databases", 0, "db_user"), "other_user"),
            (("region",), "nyc"),
            (("features",), ["buildpack-stack=ubuntu-24"]),
        ):
            observed = copy.deepcopy(self.case["first"]["app"]["spec"])
            put(observed, path, value)
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.plan(observed=observed)
        observed = copy.deepcopy(self.case["first"]["app"]["spec"])
        version = next(row for row in observed["services"][0]["envs"]
                       if row["key"] == "CRM_CANARY_DRIVER_VERSION_SHA256")
        version["value"] = "0" * 64
        with self.assertRaises(common.ReleaseError):
            self.plan(observed=observed)

    def test_no_original_authority_descriptor_hmac_or_fixture_rebinding(self):
        for path, value in (
            (("original", "driver_evidence", "digest"), "sha256:" + "0" * 64),
            (("original", "driver_evidence", "driver_version_sha256"), "0" * 64),
            (("descriptor", "hmac_key_base64"), base64.b64encode(b"q" * 32).decode()),
            (("descriptor", "plan", "ledger", "firewall_sha256"), "0" * 64),
            (("descriptor", "fixture_descriptor_sha256"), "0" * 64),
        ):
            case = copy.deepcopy(self.case)
            put(case, path, value)
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.plan(case)

    def test_planner_never_accepts_writer_provider_or_token_capabilities(self):
        for key in ("provider", "writer", "write_token", "create_token", "read_token"):
            capability = mock.Mock()
            with self.subTest(key=key), self.assertRaises(TypeError):
                recovery.target_update_plan(self.case["original"], self.case["descriptor"],
                    self.case["expected_spec"], self.case["authorization"]["target_driver_evidence"],
                    self.case["first"]["app"]["spec"], control_sha=CONTROL, **{key: capability})
            self.assertEqual(capability.mock_calls, [])

    def test_planner_emits_nothing_and_inspection_never_returns_private_plan(self):
        with (mock.patch("sys.stdout", new_callable=io.StringIO) as out,
              mock.patch("sys.stderr", new_callable=io.StringIO) as err):
            plan = self.plan()
            report = self.inspect()
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")
        self.assertNotIn("spec", report)
        self.assertNotIn("target_spec", report)
        self.assertNotIn("target_plan", report)
        self.assertIs(report["ready_for_finalize"], False)
        self.assertIs(report["ready_for_operation"], False)
        encoded = common.canonical_payload_bytes(report).decode()
        for row in plan["services"][0]["envs"]:
            if row["type"] == "SECRET":
                self.assertNotIn(row["value"], encoded)
                if row["key"] in LOGIN_KEYS:
                    login = common.loads_strict(row["value"])
                    self.assertNotIn(login["email"], encoded)
                    self.assertNotIn(login["password"], encoded)


class MaliciousObjectTests(RecoveryFixtures):
    class Poison:
        def _touched(self, *_args, **_kwargs):
            raise AssertionError("malicious synthetic object was evaluated")

        __str__ = __repr__ = __eq__ = __bool__ = __deepcopy__ = __iter__ = _touched

    def test_non_json_objects_rejected_before_comparison_formatting_or_copying(self):
        for path in (
            ("authorization", "kind"), ("authorization", "identities", "app_id_sha256"),
            ("authorization", "target_driver_evidence", "digest"), ("origin_run", "id"),
            ("origin_run", "repository"), ("first", "account", "status"),
            ("first", "app", "owner_uuid"), ("first", "app", "spec"),
            ("first", "deployments", "deployments", 1, "phase"),
            ("first", "apps", "links"),
        ):
            case = copy.deepcopy(self.case)
            put(case, path, self.Poison())
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_container_subclasses_cannot_run_overrides(self):
        class PoisonDict(dict):
            get = items = keys = __iter__ = __deepcopy__ = MaliciousObjectTests.Poison._touched

        class PoisonList(list):
            __iter__ = __len__ = __getitem__ = __deepcopy__ = MaliciousObjectTests.Poison._touched

        for path, value in (
            (("authorization",), PoisonDict(self.case["authorization"])),
            (("first", "app", "spec"), PoisonDict(self.case["first"]["app"]["spec"])),
            (("first", "deployments", "deployments"), PoisonList()),
        ):
            case = copy.deepcopy(self.case)
            put(case, path, value)
            with self.subTest(path=path), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_trusted_context_arguments_also_reject_hostile_objects(self):
        for key in ("control_sha", "now"):
            case = copy.deepcopy(self.case)
            case[key] = self.Poison()
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                self.inspect(case)
        with self.assertRaises(common.ReleaseError):
            recovery.validate_target_driver_evidence(self.case["authorization"]["target_driver_evidence"],
                                                     control_sha=self.Poison())

    def test_nonfinite_cyclic_overdeep_and_nonstring_key_values_rejected(self):
        cycle = []
        cycle.append(cycle)
        nested = []
        for _ in range(50):
            nested = [nested]
        for value in (float("nan"), float("inf"), float("-inf"), cycle, nested, {1: PRIVATE}):
            case = copy.deepcopy(self.case)
            case["first"]["app"]["message"] = value
            with self.subTest(kind=type(value).__name__), self.assertRaises(common.ReleaseError):
                self.inspect(case)

    def test_malformed_json_pagination_shapes_are_closed_rejections(self):
        for inventory in ("apps", "deployments"):
            for links in (None, [], PRIVATE, {"pages": None}, {"pages": []}, {"pages": PRIVATE}):
                case = copy.deepcopy(self.case)
                case["first"][inventory]["links"] = links
                with self.subTest(inventory=inventory, links_type=type(links).__name__), self.assertRaises(common.ReleaseError):
                    self.inspect(case)


class ObservationTimingTests(RecoveryFixtures):
    def test_pair_reversal_equal_too_short_and_too_long_fail(self):
        for seconds in (-3, 0, 2, 121):
            with self.subTest(seconds=seconds):
                self.reject(lambda c: c["first"].update(observed_at=stamp(NOW - dt.timedelta(seconds=5 + seconds))))

    def test_future_second_observation_is_rejected(self):
        self.reject_value(("second", "observed_at"), stamp(NOW + dt.timedelta(seconds=1)))

    def test_stale_pair_within_unexpired_authority_is_rejected(self):
        def mutate(c):
            c["first"]["observed_at"] = stamp(NOW - dt.timedelta(minutes=4))
            c["second"]["observed_at"] = stamp(NOW - dt.timedelta(minutes=3, seconds=55))
        self.reject(mutate)

    def test_second_observation_freshness_boundary_is_exact(self):
        case = copy.deepcopy(self.case)
        case["first"]["observed_at"] = stamp(NOW - dt.timedelta(seconds=125))
        case["second"]["observed_at"] = stamp(NOW - dt.timedelta(seconds=120))
        self.assertFalse(self.inspect(case)["ready_for_finalize"])
        case["first"]["observed_at"] = stamp(NOW - dt.timedelta(seconds=126))
        case["second"]["observed_at"] = stamp(NOW - dt.timedelta(seconds=121))
        with self.assertRaises(common.ReleaseError):
            self.inspect(case)

    def test_observations_cannot_precede_issue_or_reach_expiry(self):
        for key in ("first", "second"):
            for value in (stamp(NOW - dt.timedelta(minutes=5, seconds=1)),
                          self.case["authorization"]["expires_at"]):
                with self.subTest(snapshot=key, time=value):
                    self.reject_value((key, "observed_at"), value)

    def test_missing_extra_snapshot_fields_or_invalid_timestamp_fail(self):
        self.reject(lambda c: c["first"].update(unreviewed=PRIVATE))
        for key in recovery.SNAPSHOT_KEYS:
            with self.subTest(key=key):
                self.reject(lambda c, k=key: c["first"].pop(k))
        self.reject_value(("first", "observed_at"), "yesterday")


class ReadOnlyAdapterTests(RecoveryFixtures):
    def reader(self, app_id=None, deployment_id=None, ledger_id=None, redeployed_deployment_id=None):
        with (mock.patch.object(boot.fixture, "_opener", return_value=mock.sentinel.opener),
              mock.patch.dict(recovery.os.environ, {}, clear=True)):
            return recovery.ExistingAppReader("SYNTHETIC-READ-ONLY-TOKEN",
                self.app_id if app_id is None else app_id,
                self.deployment_id if deployment_id is None else deployment_id,
                self.case["descriptor"]["plan"]["ledger"]["cluster_id"] if ledger_id is None else ledger_id,
                redeployed_deployment_id=(self.redeployed_deployment_id if redeployed_deployment_id is None
                                         else redeployed_deployment_id))

    def test_exact_seven_fixed_get_routes_headers_origin_and_bound(self):
        reader = self.reader()
        ledger = self.case["descriptor"]["plan"]["ledger"]["cluster_id"]
        routes = ("/v2/account", "/v2/apps/" + self.app_id,
                  "/v2/apps/" + self.app_id + "/deployments/" + self.deployment_id,
                  "/v2/apps/" + self.app_id + "/deployments/" + self.redeployed_deployment_id,
                  "/v2/apps/" + self.app_id + "/deployments?per_page=200&page=1",
                  "/v2/apps?per_page=200&page=1", "/v2/databases/" + ledger + "/firewall")
        with mock.patch.object(boot.fixture, "_wire", return_value=b'{"synthetic":"ok"}') as wire:
            for path in routes:
                with self.subTest(route=path):
                    self.assertEqual(reader.get(path), {"synthetic": "ok"})
                    wire.assert_called_with(mock.sentinel.opener, common.API_ORIGIN + path,
                        method="GET", headers={"Authorization": "Bearer SYNTHETIC-READ-ONLY-TOKEN",
                                               "Accept": "application/json"}, maximum=boot.MAX_PUBLIC)
            self.assertEqual(wire.call_count, 7)

    def test_forbidden_user_credentials_logs_exec_and_unbound_routes_never_transport(self):
        reader = self.reader()
        app = "/v2/apps/" + self.app_id
        ledger = "/v2/databases/" + self.case["descriptor"]["plan"]["ledger"]["cluster_id"]
        paths = ("/v2/apps", app + "/deployments", app + "/deployments/latest",
                 app + "/deployments/" + uid(99), "/v2/apps/" + uid(99),
                 app + "/logs", app + "/exec", app + "/credentials", app + "/users",
                 app + "/deployments/" + self.deployment_id + "/logs", ledger + "/users",
                 ledger + "/users/crm_canary_driver", ledger + "/credentials", ledger,
                 "/v2/users", "/v1/execute", "/v2/apps/propose",
                 "/v2/apps?per_page=200&page=2", "/v2/apps?page=1&per_page=200",
                 app + "/", app + "?fields=credentials", app + "#fragment", app + "/../logs",
                 common.API_ORIGIN + app, "https://private.invalid/", None, b"/v2/account")
        with mock.patch.object(boot.fixture, "_wire") as wire:
            for path in paths:
                with self.subTest(path_type=type(path).__name__), self.assertRaises(common.ReleaseError):
                    reader.get(path)
            wire.assert_not_called()

    def test_identity_selector_substitution_and_malformed_ledger_fail_before_opener(self):
        for values in ({"app_id": uid(99)}, {"deployment_id": uid(99)},
                       {"redeployed_deployment_id": uid(99)},
                       {"redeployed_deployment_id": self.deployment_id},
                       {"redeployed_deployment_id": "latest"},
                       {"app_id": "latest"}, {"deployment_id": "latest"},
                       {"ledger_id": uid(99)}, {"ledger_id": "credentials"}):
            with self.subTest(fields=tuple(values)), mock.patch.object(boot.fixture, "_opener") as opener:
                with self.assertRaises(common.ReleaseError):
                    recovery.ExistingAppReader("SYNTHETIC-READ-ONLY-TOKEN",
                        values.get("app_id", self.app_id), values.get("deployment_id", self.deployment_id),
                        values.get("ledger_id", self.case["descriptor"]["plan"]["ledger"]["cluster_id"]),
                        redeployed_deployment_id=values.get("redeployed_deployment_id", self.redeployed_deployment_id))
                opener.assert_not_called()

    def test_tls_environment_overrides_fail_before_opener_or_transport(self):
        for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR"):
            with (self.subTest(name=name), mock.patch.dict(recovery.os.environ, {name: PRIVATE}, clear=True),
                  mock.patch.object(boot.fixture, "_opener") as opener,
                  mock.patch.object(boot.fixture, "_wire") as wire,
                  self.assertRaises(common.ReleaseError)):
                recovery.ExistingAppReader("SYNTHETIC-READ-ONLY-TOKEN", self.app_id, self.deployment_id,
                    self.case["descriptor"]["plan"]["ledger"]["cluster_id"],
                    redeployed_deployment_id=self.redeployed_deployment_id)
            opener.assert_not_called()
            wire.assert_not_called()

    def test_no_mutating_generic_request_or_log_methods_are_exposed(self):
        reader = self.reader()
        for name in ("post", "put", "patch", "delete", "request", "create", "update", "install",
                     "redeploy", "deploy", "execute", "exec", "logs", "credentials", "users", "finalize"):
            with self.subTest(name=name):
                self.assertFalse(hasattr(reader, name))
        with self.assertRaises(TypeError):
            reader.get("/v2/account", method="POST")
        with self.assertRaises(TypeError):
            recovery.ExistingAppReader("SYNTHETIC-READ-ONLY-TOKEN", self.app_id, self.deployment_id,
                self.case["descriptor"]["plan"]["ledger"]["cluster_id"],
                redeployed_deployment_id=self.redeployed_deployment_id, write_token=PRIVATE)

    def test_reader_requires_second_deployment_identity_without_guessing(self):
        with mock.patch.object(boot.fixture, "_opener") as opener, self.assertRaises(TypeError):
            recovery.ExistingAppReader("SYNTHETIC-READ-ONLY-TOKEN", self.app_id, self.deployment_id,
                self.case["descriptor"]["plan"]["ledger"]["cluster_id"])
        opener.assert_not_called()

    def test_inspection_does_not_accept_mutation_capable_provider_or_writer(self):
        adapter = mock.Mock()
        for name in ("provider", "writer", "create_token", "write_token"):
            with self.subTest(capability=name), self.assertRaises(TypeError):
                recovery.inspect_pair(**self.case, **{name: adapter})
        self.assertEqual(adapter.mock_calls, [])

    def test_strict_json_duplicate_nonfinite_and_malformed_response_fail(self):
        reader = self.reader()
        for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'<html>private</html>', b'\xff'):
            with self.subTest(length=len(raw)), mock.patch.object(boot.fixture, "_wire", return_value=raw):
                with self.assertRaises(common.ReleaseError):
                    reader.get("/v2/account")


class DiagnosticAndCapabilityTests(unittest.TestCase):
    def test_historical_regeneration_uses_wrapper_not_current_generator_directly(self):
        tree = ast.parse(Path(recovery.__file__).read_text(encoding="utf-8"))
        validator = next(node for node in tree.body
                         if isinstance(node, ast.FunctionDef) and node.name == "_validate_expected_spec")
        calls = [ast.unparse(node.func) for node in ast.walk(validator) if isinstance(node, ast.Call)]
        self.assertIn("historical_expected_spec", calls)
        self.assertNotIn("boot.runtime_spec", calls)
        self.assertFalse(any(isinstance(node, ast.FunctionDef) and node.name == "_legacy_route_projection"
                             for node in tree.body))

    def test_failure_report_never_formats_exception_or_emits_arbitrary_properties(self):
        class Malicious(Exception):
            def __str__(self):
                raise AssertionError(PRIVATE)

            def __repr__(self):
                raise AssertionError(PRIVATE)

        expected = {"schema_version": 1, "state": "existing-driver-inspection-rejected",
                    "code": "RECOVERY_INSPECTION_REJECTED", "mutation_performed": False,
                    "ready_for_finalize": False, "ready_for_operation": False}
        for error in (Malicious(PRIVATE), RuntimeError(PRIVATE), common.ReleaseError(PRIVATE)):
            error.__dict__.update(code=PRIVATE, reason=PRIVATE, message=PRIVATE,
                                  log_url="https://private.invalid/" + PRIVATE, status=PRIVATE)
            with mock.patch("sys.stdout", new_callable=io.StringIO) as out, \
                    mock.patch("sys.stderr", new_callable=io.StringIO) as err:
                result = recovery.failure_report(error)
            self.assertEqual(result, expected)
            self.assertNotIn(PRIVATE, common.canonical_payload_bytes(result).decode())
            self.assertEqual(out.getvalue(), "")
            self.assertEqual(err.getvalue(), "")

    def test_ast_capability_boundary_has_no_mutation_or_original_installer_execution(self):
        tree = ast.parse(Path(recovery.__file__).read_text(encoding="utf-8"))
        forbidden_calls = {"install_once", "check_once", "preflight", "create", "update", "delete",
                           "post", "put", "patch", "deploy", "redeploy", "execute", "dispatch",
                           "approve", "cancel", "rerun", "write_bytes", "write_text", "write",
                           "system", "Popen", "run", "call", "check_call", "check_output",
                           "exec", "eval", "compile", "__import__"}
        allowed_boot_calls = {"validate_descriptor", "spec_projection", "firewall_projection", "runtime_spec"}
        allowed_fixture_calls = {"boot.fixture._secret", "boot.fixture._opener", "boot.fixture._wire"}
        calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)]
        for node in calls:
            name = node.func.attr if isinstance(node.func, ast.Attribute) else getattr(node.func, "id", "")
            self.assertNotIn(name, forbidden_calls, "forbidden capability at line " + str(node.lineno))
            if isinstance(node.func, ast.Attribute) and isinstance(node.func.value, ast.Name) and node.func.value.id == "boot":
                self.assertIn(name, allowed_boot_calls)
            dotted = ast.unparse(node.func)
            if dotted.startswith("boot.fixture."):
                self.assertIn(dotted, allowed_fixture_calls)
        wire_calls = [n for n in calls if isinstance(n.func, ast.Attribute) and n.func.attr == "_wire"]
        self.assertEqual(len(wire_calls), 1)
        self.assertEqual(next(k.value.value for k in wire_calls[0].keywords if k.arg == "method"), "GET")
        reader = next(n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "ExistingAppReader")
        self.assertEqual({n.name for n in reader.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))},
                         {"__init__", "get"})
        self.assertFalse(any(isinstance(n, ast.If) and "__name__" in ast.unparse(n.test) for n in tree.body))
        self.assertFalse(any(isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name in ("main", "install", "finalize")
                             for n in tree.body))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                self.assertTrue({a.name for a in node.names} <= {"copy", "datetime", "os", "bootstrap_production_crm_canary_driver"})
            elif isinstance(node, ast.ImportFrom):
                self.assertIn(node.module, (None, "__future__", "typing", "contextlib"))


class PrestateDiagnosticTests(RecoveryFixtures):
    def test_closed_codes_accept_only_exact_allowlisted_strings(self):
        for code in recovery.PRESTATE_DIAGNOSTICS:
            error = recovery.PrestateRejected(code)
            self.assertIs(type(error), recovery.PrestateRejected)
            self.assertEqual(error.code, code)
            self.assertEqual(error.args, ("existing-driver prestate rejected",))
            with recovery.prestate_diagnostic(code):
                pass

        class Hostile:
            def __str__(self):
                raise AssertionError("formatted diagnostic input")
            def __eq__(self, other):
                raise AssertionError("compared diagnostic input")
            def __hash__(self):
                raise AssertionError("hashed diagnostic input")
        class TextSubclass(str):
            def __hash__(self):
                raise AssertionError("hashed diagnostic subclass")
        for code in (None, True, 1, [], {}, PRIVATE, Hostile(), TextSubclass("APP_IDENTITY")):
            with self.subTest(kind=type(code).__name__):
                with self.assertRaises(common.ReleaseError) as caught:
                    recovery.PrestateRejected(code)
                self.assertIsNot(type(caught.exception), recovery.PrestateRejected)
                entered = False
                with self.assertRaises(common.ReleaseError):
                    with recovery.prestate_diagnostic(code):
                        entered = True
                self.assertFalse(entered)

    def test_arbitrary_and_hostile_exceptions_are_content_free_and_chainless(self):
        class HostileError(Exception):
            def __str__(self):
                raise AssertionError("exception formatted")
            @property
            def code(self):
                raise AssertionError("exception code inspected")
            @property
            def status(self):
                raise AssertionError("exception status inspected")
        class DiagnosticSubclass(recovery.PrestateRejected):
            @property
            def code(self):
                raise AssertionError("subclass code inspected")
            @code.setter
            def code(self, value):
                pass
        for error in (RuntimeError(PRIVATE), ValueError({PRIVATE: PRIVATE}),
                      HostileError(PRIVATE), DiagnosticSubclass("ACCOUNT_OWNER")):
            out, err = io.StringIO(), io.StringIO()
            with redirect_stdout(out), redirect_stderr(err), self.assertRaises(recovery.PrestateRejected) as caught:
                with recovery.prestate_diagnostic("SPEC_PROJECTION"):
                    raise error
            result = caught.exception
            self.assertIs(type(result), recovery.PrestateRejected)
            self.assertEqual(result.code, "SPEC_PROJECTION")
            self.assertEqual(result.args, ("existing-driver prestate rejected",))
            self.assertIsNone(result.__cause__)
            self.assertTrue(result.__suppress_context__)
            self.assertEqual(out.getvalue(), "")
            self.assertEqual(err.getvalue(), "")

    def test_exact_nested_diagnostic_preserves_innermost_fixed_code(self):
        inner = recovery.PrestateRejected("SPEC_PUBLIC_EQUALITY")
        with self.assertRaises(recovery.PrestateRejected) as caught:
            with recovery.prestate_diagnostic("APP_SPEC"):
                with recovery.prestate_diagnostic("FIRST_SNAPSHOT"):
                    raise inner
        self.assertIs(caught.exception, inner)
        self.assertEqual(caught.exception.code, "SPEC_PUBLIC_EQUALITY")

    def test_tampered_exact_diagnostic_is_sanitized_and_base_exceptions_are_not_swallowed(self):
        for bad in (None, PRIVATE, [], {}, object()):
            error = recovery.PrestateRejected("SPEC_PROJECTION")
            error.code = bad
            with self.assertRaises(recovery.PrestateRejected) as caught:
                with recovery.prestate_diagnostic("APP_SPEC"):
                    raise error
            self.assertEqual(caught.exception.code, "APP_SPEC")
        missing = recovery.PrestateRejected("SPEC_PROJECTION")
        del missing.code
        with self.assertRaises(recovery.PrestateRejected) as caught:
            with recovery.prestate_diagnostic("APP_SPEC"):
                raise missing
        self.assertEqual(caught.exception.code, "APP_SPEC")
        for error in (KeyboardInterrupt(), SystemExit(1)):
            with self.assertRaises(type(error)) as caught:
                with recovery.prestate_diagnostic("APP_SPEC"):
                    raise error
            self.assertIs(caught.exception, error)

    def test_diagnostic_wrappers_preserve_the_four_original_function_asts(self):
        # Frozen from published f4b94fdc. Removing only literal diagnostic
        # contexts must leave every acceptance predicate and execution order.
        expected = {
            "_exact_spec": "9ab25dacd0fe91e35eb65ca157924cc7072403978baf77eae42fcbb247267d95",
            "target_update_plan": "7abde705f2b4bdad1b328d5f6a0b40e76570a86f5cd318c4fc4fe1cd4782f2d0",
            "_snapshot": "26b59bdbd219d01377e05161c5066416b78dadf85b407fed76320d686d478e00",
            "inspect_pair": "f27a440c325cc79da6d921196de0401fa5438c04afa2514d7b465cabde1a68f6",
        }
        owner = self
        class Unwrap(ast.NodeTransformer):
            def visit_With(self, node):
                self.generic_visit(node)
                owner.assertEqual(len(node.items), 1)
                item = node.items[0]
                owner.assertIsNone(item.optional_vars)
                call = item.context_expr
                owner.assertIsInstance(call, ast.Call)
                owner.assertIsInstance(call.func, ast.Name)
                owner.assertEqual(call.func.id, "prestate_diagnostic")
                owner.assertEqual(len(call.args), 1)
                owner.assertEqual(call.keywords, [])
                owner.assertIsInstance(call.args[0], ast.Constant)
                owner.assertIs(type(call.args[0].value), str)
                owner.assertIn(call.args[0].value, recovery.PRESTATE_DIAGNOSTICS)
                return node.body
        tree = ast.parse(Path(recovery.__file__).read_text(encoding="utf-8"))
        functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
        for name, digest in expected.items():
            with self.subTest(function=name):
                unwrapped = Unwrap().visit(functions[name])
                self.assertEqual(hashlib.sha256(ast.dump(unwrapped, include_attributes=False).encode()).hexdigest(), digest)

    def test_specific_policy_assertion_families_have_closed_diagnostics(self):
        changes = (
            (lambda c: c["first"].update(observed_at=stamp(NOW - dt.timedelta(hours=3))), "OBSERVATION_WINDOW"),
            (lambda c: c["first"].update(production_state_sha256="0" * 64), "SNAPSHOT_BINDINGS"),
            (lambda c: c["first"]["account"].update(status=PRIVATE), "ACCOUNT_OWNER"),
            (lambda c: c["first"]["app"].update(id=uid(99)), "APP_IDENTITY"),
            (lambda c: c["first"]["apps"]["apps"].pop(), "APP_INVENTORY_BINDING"),
            (lambda c: c["first"]["deployments"]["deployments"][0].update(phase=PRIVATE), "DEPLOYMENT_IDENTITIES"),
            (lambda c: c["first"]["app"].update(updated_at=stamp(NOW)), "DRIVER_METADATA_BINDING"),
            (lambda c: c["first"]["app"].update(pending_deployment={"id": uid(99)}), "APP_DEPLOYMENT_SLOTS"),
            (lambda c: c["first"]["app"]["spec"].update(region=PRIVATE), "SPEC_PROJECTION"),
            (lambda c: c["first"]["firewall"]["rules"].pop(), "FIREWALL_ADMISSION_BINDING"),
            (lambda c: c["first"]["canary_environment"].update(variables=[PRIVATE]), "CANARY_METADATA_BINDING"),
            (lambda c: c["second"].update(observed_at=c["first"]["observed_at"]), "PAIR_TIME"),
        )
        for change, code in changes:
            case = copy.deepcopy(self.case)
            change(case)
            with self.subTest(code=code), self.assertRaises(recovery.PrestateRejected) as caught:
                self.inspect(case)
            self.assertEqual(caught.exception.code, code)
            self.assertNotIn(PRIVATE, str(caught.exception))

    def test_spec_projection_and_final_public_equality_remain_distinct(self):
        expected = self.case["expected_spec"]
        actual = self.case["first"]["app"]["spec"]
        with mock.patch.object(boot, "spec_projection", side_effect=RuntimeError(PRIVATE)):
            with self.assertRaises(recovery.PrestateRejected) as caught:
                recovery._exact_spec(actual, expected)
            self.assertEqual(caught.exception.code, "SPEC_PROJECTION")
        projected = copy.deepcopy(actual)
        projected["region"] = PRIVATE
        with mock.patch.object(boot, "spec_projection", return_value=projected):
            with self.assertRaises(recovery.PrestateRejected) as caught:
                recovery._exact_spec(actual, expected)
            self.assertEqual(caught.exception.code, "SPEC_PUBLIC_EQUALITY")

    def test_secret_fuzz_rejections_match_unwrapped_acceptance_without_output(self):
        @contextmanager
        def no_diagnostic(_code):
            yield
        paths = (
            ("first", "app", "spec", "name"),
            ("first", "app", "spec", "services", 0, "envs", 0, "value"),
            ("first", "account", "uuid"), ("first", "account", "team", "uuid"),
            ("first", "app", "id"), ("first", "app", "owner_uuid"),
            ("first", "app", "created_at"), ("first", "app", "pending_deployment"),
            ("first", "canary_environment", "secrets"), ("first", "firewall", "rules"),
            ("second", "observed_at"), ("authorization", "production_state_sha256"),
        )
        values = (None, True, 0, -1, 1.0, PRIVATE, "https://private.invalid/" + PRIVATE,
                  {PRIVATE: PRIVATE}, [PRIVATE])
        cases = [copy.deepcopy(self.case)]
        for path in paths:
            for value in values:
                case = copy.deepcopy(self.case)
                put(case, path, value)
                cases.append(case)
        for index, case in enumerate(cases):
            out, err = io.StringIO(), io.StringIO()
            with self.subTest(case=index), redirect_stdout(out), redirect_stderr(err):
                try:
                    result = self.inspect(case)
                    actual = (True, common.canonical_payload_bytes(result))
                except recovery.PrestateRejected as error:
                    self.assertIs(type(error), recovery.PrestateRejected)
                    self.assertIn(error.code, recovery.PRESTATE_DIAGNOSTICS)
                    self.assertEqual(error.args, ("existing-driver prestate rejected",))
                    actual = (False, None)
                with mock.patch.object(recovery, "prestate_diagnostic", no_diagnostic):
                    try:
                        result = self.inspect(case)
                        expected = (True, common.canonical_payload_bytes(result))
                    except Exception:
                        expected = (False, None)
                self.assertEqual(actual, expected)
            self.assertEqual(out.getvalue(), "")
            self.assertEqual(err.getvalue(), "")


if __name__ == "__main__":
    unittest.main()
