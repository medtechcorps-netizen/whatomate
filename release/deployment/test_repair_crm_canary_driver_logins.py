"""Offline contracts for the protected two-value CRM canary driver login repair.

All identifiers, ciphertexts and credentials are synthetic. No test performs a
provider, GitHub or product request.
"""
from __future__ import annotations

import base64
import copy
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest import mock

try:
    from . import repair_crm_canary_driver_logins as repair
except ImportError:
    import repair_crm_canary_driver_logins as repair

common, policy, boot = repair.common, repair.policy, repair.boot
ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / repair.WORKFLOW
APP_ID = "11111111-1111-4111-8111-111111111111"
FAILED_ID = "22222222-2222-4222-8222-222222222222"
REDEPLOYED_ID = "33333333-3333-4333-8333-333333333333"
LATER_ID = "44444444-4444-4444-8444-444444444444"
NEW_ID = "55555555-5555-4555-8555-555555555555"
OTHER_APP = "66666666-6666-4666-8666-666666666666"
ROLL_ID = "88888888-8888-4888-8888-888888888888"
UNKNOWN_ID = "99999999-9999-4999-8999-999999999999"
HOST = "rereply-canary-driver-test-abcde.ondigitalocean.app"
ORIGIN = "https://" + HOST
# Synthetic NEW publisher evidence: every value is made up for the tests.
ROLL_EVIDENCE = {"artifact_digest": "sha256:" + "a1" * 32, "artifact_id": "222222222", "control_sha": "c" * 40,
                 "digest": "sha256:" + "b2" * 32, "driver_version_sha256": "d3" * 32, "run_id": "111111111"}


def digest(value: str) -> str:
    return hashlib.sha256(value.encode("ascii")).hexdigest()


PINS = {"app_id_sha256": digest(APP_ID), "failed_deployment_id_sha256": digest(FAILED_ID),
        "redeployed_deployment_id_sha256": digest(REDEPLOYED_ID)}
PLAN = {"app_name": "rereply-canary-driver-test", "region": "sgp", "instance_size_slug": "apps-s-1vcpu-1gb",
        "ledger": {"cluster_name": "rereply-canary-ledger", "database": "canary_ledger", "user": "crm_canary_driver"}}
D = {"plan": PLAN, "fixture_descriptor_sha256": "a" * 64, "hmac_key_base64": "A" * 43 + "="}
PROTECTED = {"registration": {"klinik_email": "k-synthetic@example.invalid",
                              "non_klinik_email": "n-synthetic@example.invalid"},
             "credentials": {"klinik_password": "k" * 43, "non_klinik_password": "n" * 43}}


def cipher(label: str) -> str:
    return "EV[1:" + label + "nonce:" + label + "ciphertext]"


def spec() -> dict:
    envs = [
        {"key": "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "scope": "RUN_TIME", "type": "SECRET", "value": cipher("d")},
        {"key": "CRM_CANARY_HMAC_KEY_BASE64", "scope": "RUN_TIME", "type": "SECRET", "value": cipher("h")},
        {"key": "CRM_CANARY_KLINIK_LOGIN_JSON", "scope": "RUN_TIME", "type": "SECRET", "value": cipher("k")},
        {"key": "CRM_CANARY_META_APP_SECRET", "scope": "RUN_TIME", "type": "SECRET", "value": cipher("m")},
        {"key": "CRM_CANARY_NON_KLINIK_LOGIN_JSON", "scope": "RUN_TIME", "type": "SECRET", "value": cipher("n")},
        {"key": "CRM_CANARY_DRIVER_VERSION_SHA256", "scope": "RUN_TIME",
         "value": repair.TARGET_DRIVER_EVIDENCE["driver_version_sha256"]},
        {"key": "CRM_CANARY_LEDGER_DATABASE_URL", "scope": "RUN_TIME", "value": boot.LEDGER_EXPRESSION},
    ]
    return {"name": PLAN["app_name"], "region": "sgp", "features": ["buildpack-stack=ubuntu-22"],
            "ingress": {"rules": [{"match": {"path": {"prefix": "/"}}, "component": {"name": "driver"}}]},
            "databases": [{"name": "ledger", "engine": "PG", "version": "17", "production": True,
                           "cluster_name": "rereply-canary-ledger", "db_name": "canary_ledger",
                           "db_user": "crm_canary_driver"}],
            "services": [{"name": "driver", "instance_count": 1, "instance_size_slug": "apps-s-1vcpu-1gb",
                          "http_port": 8080, "health_check": {"http_path": "/healthz", "port": 8080},
                          "image": {"registry_type": "GHCR", "registry": "medtechcorps-netizen",
                                    "repository": "rereply-crm-canary-driver",
                                    "digest": repair.TARGET_DRIVER_EVIDENCE["digest"]},
                          "envs": envs}]}


def app(**changes) -> dict:
    value = {"id": APP_ID, "owner_uuid": "77777777-7777-4777-8777-777777777777",
             "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z", "spec": spec()}
    value.update(changes)
    return value


def history(*rows) -> dict:
    rows = rows or ((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (LATER_ID, "ERROR"))
    return {"deployments": [{"id": i, "phase": p} for i, p in rows], "meta": {"total": len(rows)}, "links": {"pages": {}}}


def logins() -> dict:
    return repair.corrected_logins(PROTECTED)


def relogged(value: dict, *, other: str | None = None) -> dict:
    """Provider-style readback: new login ciphertexts, optionally one other change."""
    result = copy.deepcopy(value)
    for row in result["services"][0]["envs"]:
        if row["key"] in repair.LOGIN_KEYS:
            row["value"] = cipher("new-" + row["key"][11:14].lower())
        if other is not None and row["key"] == other:
            row["value"] = cipher("changed")
    return result


def evidence_raw(value: dict | None = None) -> str:
    return common.canonical_payload_bytes(ROLL_EVIDENCE if value is None else value).decode()


def rolled(value: dict, evidence: dict = ROLL_EVIDENCE) -> dict:
    """The spec moved to the evidence's image digest and version; all else carried."""
    result = copy.deepcopy(value)
    result["services"][0]["image"]["digest"] = evidence["digest"]
    for row in result["services"][0]["envs"]:
        if row["key"] == repair.VERSION_KEY:
            row["value"] = evidence["driver_version_sha256"]
    return result


def running(spec_value: dict | None = None, active: str = NEW_ID, **changes) -> dict:
    return app(spec=spec() if spec_value is None else spec_value,
               active_deployment={"id": active, "phase": "ACTIVE"}, **changes)


def active_history(*extra) -> dict:
    return history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (LATER_ID, "ERROR"), (NEW_ID, "ACTIVE"), *extra)


class Pinned(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.dict(policy.IDENTITY_PINS, PINS)
        patcher.start()
        self.addCleanup(patcher.stop)

    def assertStopped(self, code: str, fn, *args, **kwargs) -> None:
        with self.assertRaises(repair.Stopped) as caught:
            fn(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)


class LoginContract(Pinned):
    def test_corrected_logins_are_canonical_versioned_driver_shape(self) -> None:
        values = logins()
        self.assertEqual(set(values), set(repair.LOGIN_KEYS))
        klinik = values["CRM_CANARY_KLINIK_LOGIN_JSON"]
        self.assertEqual(klinik, '{"email":"k-synthetic@example.invalid","password":"' + "k" * 43
                         + '","schema_version":1}')
        self.assertEqual(json.loads(values["CRM_CANARY_NON_KLINIK_LOGIN_JSON"])["email"],
                         "n-synthetic@example.invalid")

    def test_login_rejects_values_the_driver_would_reject(self) -> None:
        for path, bad in ((("registration", "klinik_email"), "no-at-sign"),
                          (("registration", "klinik_email"), "space in@example.invalid"),
                          (("credentials", "klinik_password"), "short"),
                          (("credentials", "klinik_password"), "x" * 257),
                          (("credentials", "klinik_password"), 12345)):
            protected = copy.deepcopy(PROTECTED)
            protected[path[0]][path[1]] = bad
            self.assertStopped("LOGIN_SHAPE", repair.corrected_logins, protected)
        self.assertStopped("LOGIN_SHAPE", repair.corrected_logins, {"registration": {}})


class Selection(Pinned):
    def inventory(self, *ids: str) -> dict:
        rows = [{"id": i, "spec": {"name": PLAN["app_name"] if i == APP_ID else "rereply"}} for i in ids]
        return {"apps": rows, "meta": {"total": len(rows)}, "links": {}}

    def test_selects_exactly_the_pinned_app_in_any_size_inventory(self) -> None:
        self.assertEqual(repair.select_driver(self.inventory(OTHER_APP, APP_ID), PLAN["app_name"]), APP_ID)
        many = self.inventory(APP_ID, *["%08d-0000-4000-8000-000000000000" % n for n in range(7)])
        self.assertEqual(repair.select_driver(many, PLAN["app_name"]), APP_ID)

    def test_rejects_incomplete_or_ambiguous_inventory(self) -> None:
        partial = self.inventory(APP_ID)
        partial["meta"]["total"] = 2
        self.assertStopped("APPS_INVENTORY", repair.select_driver, partial, PLAN["app_name"])
        paged = self.inventory(APP_ID)
        paged["links"] = {"pages": {"next": "https://example.invalid"}}
        self.assertStopped("APPS_INVENTORY", repair.select_driver, paged, PLAN["app_name"])
        self.assertStopped("APPS_INVENTORY", repair.select_driver, self.inventory(APP_ID, APP_ID), PLAN["app_name"])
        self.assertStopped("DRIVER_SELECTION", repair.select_driver, self.inventory(OTHER_APP), PLAN["app_name"])
        self.assertStopped("DRIVER_SELECTION", repair.select_driver, self.inventory(APP_ID), "another-name")


class DriverState(Pinned):
    def test_accepts_idle_failed_history_containing_both_pinned_deployments(self) -> None:
        self.assertEqual(repair.validate_driver_state(app(), history(), D), ({FAILED_ID, REDEPLOYED_ID, LATER_ID}, None))
        two = history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "CANCELED"))
        self.assertEqual(repair.validate_driver_state(app(), two, D), ({FAILED_ID, REDEPLOYED_ID}, None))

    def test_accepts_an_already_active_driver_only_on_its_single_active_deployment(self) -> None:
        active = history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (LATER_ID, "ERROR"), (NEW_ID, "ACTIVE"))
        running = app(active_deployment={"id": NEW_ID, "phase": "ACTIVE"})
        self.assertEqual(repair.validate_driver_state(running, active, D),
                         ({FAILED_ID, REDEPLOYED_ID, LATER_ID, NEW_ID}, NEW_ID))
        self.assertStopped("DRIVER_HISTORY", repair.validate_driver_state, app(), active, D)
        self.assertStopped("DRIVER_HISTORY", repair.validate_driver_state,
                           app(active_deployment={"id": LATER_ID, "phase": "ACTIVE"}), active, D)
        self.assertStopped("DRIVER_NOT_IDLE", repair.validate_driver_state,
                           app(active_deployment={"id": NEW_ID, "phase": "DEPLOYING"}), active, D)
        self.assertStopped("DRIVER_NOT_IDLE", repair.validate_driver_state,
                           app(active_deployment={"id": NEW_ID, "phase": "ACTIVE"}, pending_deployment={"id": LATER_ID}),
                           active, D)

    def test_rejects_active_or_busy_driver_and_non_failed_history(self) -> None:
        for slot in repair.SLOTS[1:]:
            self.assertStopped("DRIVER_NOT_IDLE", repair.validate_driver_state,
                               app(**{slot: {"id": LATER_ID}}), history(), D)
        for phase in ("ACTIVE", "SUPERSEDED", "BUILDING"):
            self.assertStopped("DRIVER_HISTORY", repair.validate_driver_state, app(),
                               history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (LATER_ID, phase)), D)
        self.assertStopped("DRIVER_HISTORY", repair.validate_driver_state, app(),
                           history((FAILED_ID, "ERROR"), (LATER_ID, "ERROR")), D)
        self.assertStopped("DRIVER_IDENTITY", repair.validate_driver_state, app(id=OTHER_APP), history(), D)

    def test_rejects_any_unexpected_spec_shape(self) -> None:
        cases = {
            "SPEC_IDENTITY": lambda s: s.update(name="other"),
            "SPEC_SERVICE": lambda s: s["services"][0].update(instance_count=2),
            "SPEC_IMAGE": lambda s: s["services"][0]["image"].update(digest="sha256:" + "0" * 64),
            "SPEC_DATABASE": lambda s: s["databases"][0].update(db_user="doadmin"),
        }
        for code, change in cases.items():
            value = app()
            change(value["spec"])
            self.assertStopped(code, repair.validate_driver_state, value, history(), D)
        for index, change in ((0, {"type": "GENERAL"}), (2, {"value": '{"email":"a@b","password":"plaintext"}'}),
                              (5, {"value": "0" * 64}), (6, {"value": "postgresql://literal"}),
                              (5, {"type": "SECRET"}), (1, {"scope": "BUILD_TIME"})):
            value = app()
            value["spec"]["services"][0]["envs"][index].update(change)
            self.assertStopped("SPEC_ENV", repair.validate_driver_state, value, history(), D)
        value = app()
        value["spec"]["services"][0]["envs"].append({"key": "EXTRA", "scope": "RUN_TIME", "value": "x"})
        self.assertStopped("SPEC_ENV", repair.validate_driver_state, value, history(), D)


class Proposal(Pinned):
    def test_changes_exactly_the_two_login_values_and_carries_every_ciphertext(self) -> None:
        before = spec()
        proposal = repair.build_proposal(before, logins())
        self.assertEqual(repair.diff_paths(before, proposal),
                         ["spec.services[0].envs[2].value", "spec.services[0].envs[4].value"])
        values = {row["key"]: row["value"] for row in proposal["services"][0]["envs"]}
        self.assertEqual(values, {**{row["key"]: row["value"] for row in before["services"][0]["envs"]},
                                  **logins()})
        self.assertEqual(before, spec())

    def test_carried_check_permits_only_new_login_ciphertexts(self) -> None:
        before = spec()
        sent = logins()
        repair.require_carried(before, relogged(before), "POSTSTATE", sent)
        echoed = repair.build_proposal(before, sent)
        repair.require_carried(before, echoed, "POSTSTATE", sent)
        self.assertStopped("POSTSTATE", repair.require_carried, before, before, "POSTSTATE", sent)
        for other in ("CRM_CANARY_HMAC_KEY_BASE64", "CRM_CANARY_META_APP_SECRET",
                      "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON", "CRM_CANARY_DRIVER_VERSION_SHA256"):
            self.assertStopped("POSTSTATE", repair.require_carried, before, relogged(before, other=other), "POSTSTATE", sent)
        plaintext = relogged(before)
        plaintext["services"][0]["envs"][2]["value"] = '{"email":"x@y","password":"unsent-value"}'
        self.assertStopped("POSTSTATE", repair.require_carried, before, plaintext, "POSTSTATE", sent)
        moved = relogged(before)
        moved["services"][0]["image"]["digest"] = "sha256:" + "1" * 64
        self.assertStopped("POSTSTATE", repair.require_carried, before, moved, "POSTSTATE", sent)


class UpdateAndReadback(Pinned):
    def response(self, **pending) -> dict:
        slot = {"id": NEW_ID, "phase": "PENDING_BUILD", "spec": relogged(spec())}
        slot.update(pending)
        return {"app": {**app(), "spec": relogged(spec()), "pending_deployment": slot}}

    def test_update_response_binds_exactly_one_new_deployment(self) -> None:
        known = {FAILED_ID, REDEPLOYED_ID, LATER_ID}
        self.assertEqual(repair.validate_update_response(self.response(), app(), known, logins()), NEW_ID)
        self.assertStopped("UPDATE_RESPONSE", repair.validate_update_response, self.response(id=LATER_ID), app(), known, logins())
        self.assertStopped("UPDATE_RESPONSE", repair.validate_update_response, self.response(phase="ERROR"), app(), known, logins())
        moved = self.response()
        moved["app"]["owner_uuid"] = OTHER_APP
        self.assertStopped("UPDATE_RESPONSE", repair.validate_update_response, moved, app(), known, logins())
        leaked = self.response(spec=relogged(spec(), other="CRM_CANARY_META_APP_SECRET"))
        self.assertStopped("UPDATE_RESPONSE", repair.validate_update_response, leaked, app(), known, logins())

    def provider(self, phases: list[str]) -> mock.Mock:
        provider = mock.Mock()
        apps = []
        for phase in phases:
            slot = {"id": NEW_ID, "phase": phase}
            apps.append({**app(), "spec": relogged(spec()),
                         **({"active_deployment": slot} if phase == "ACTIVE" else {"in_progress_deployment": slot})})
        provider.app.side_effect = apps
        provider.deployment.side_effect = [{"id": NEW_ID, "phase": p, "spec": relogged(spec())} for p in phases]
        return provider

    def test_await_active_follows_monotone_phases_to_active(self) -> None:
        sleeps = []
        result = repair.await_active(self.provider(["PENDING_BUILD", "BUILDING", "DEPLOYING", "ACTIVE"]), app(), NEW_ID,
                                     logins(), sleep=sleeps.append)
        self.assertEqual(result["active_deployment"]["id"], NEW_ID)
        self.assertEqual(sleeps, [repair.POLL_SECONDS] * 3)

    def test_await_active_stops_on_error_regression_or_timeout(self) -> None:
        self.assertStopped("DEPLOYMENT_ERROR", repair.await_active, self.provider(["BUILDING", "ERROR"]), app(), NEW_ID,
                           logins(), sleep=lambda _: None)
        self.assertStopped("DEPLOYMENT_CANCELED", repair.await_active, self.provider(["CANCELED"]), app(), NEW_ID,
                           logins(), sleep=lambda _: None)
        self.assertStopped("DEPLOYMENT_PHASE", repair.await_active, self.provider(["DEPLOYING", "BUILDING"]), app(),
                           NEW_ID, logins(), sleep=lambda _: None)
        with mock.patch.object(repair, "POLL_LIMIT", 2):
            self.assertStopped("DEPLOYMENT_TIMEOUT", repair.await_active, self.provider(["BUILDING", "BUILDING"]), app(),
                               NEW_ID, logins(), sleep=lambda _: None)

    def test_await_health_needs_two_consecutive_passes_and_is_bounded(self) -> None:
        provider = mock.Mock()
        provider.app.return_value = {"default_ingress": "https://rereply-canary-driver-test-abcde.ondigitalocean.app",
                                     "live_url": "https://rereply-canary-driver-test-abcde.ondigitalocean.app",
                                     "live_domain": "rereply-canary-driver-test-abcde.ondigitalocean.app"}
        outcomes = iter([OSError("dns"), None, OSError("blip"), None, None])
        def health(_origin):
            outcome = next(outcomes)
            if outcome:
                raise outcome
        origin = repair.await_health(provider, PLAN["app_name"], sleep=lambda _: None, health=health)
        self.assertEqual(origin, "https://rereply-canary-driver-test-abcde.ondigitalocean.app")
        provider.app.return_value = {"default_ingress": None}
        with mock.patch.object(repair, "HEALTH_LIMIT", 3):
            self.assertStopped("HEALTH", repair.await_health, provider, PLAN["app_name"], sleep=lambda _: None,
                               health=lambda _origin: None)

    def test_synthetic_configuration_is_the_exact_canary_driver_shape(self) -> None:
        config = repair.synthetic_config("https://rereply-canary-driver-test-abcde.ondigitalocean.app", D)
        self.assertEqual(set(config), {"schema_version", "url", "driver_version_sha256", "fixture_descriptor_sha256",
                                       "hmac_key_base64"})
        self.assertTrue(config["url"].endswith("/v1/execute"))
        self.assertEqual(config["driver_version_sha256"], repair.TARGET_DRIVER_EVIDENCE["driver_version_sha256"])


class TokenViewsAndContinuation(Pinned):
    def test_views_must_match_on_everything_but_provider_bookkeeping(self) -> None:
        repair.compare_views(app(updated_at="2026-02-02T00:00:00Z", last_deployment_active_at=None), app())
        self.assertStopped("TOKEN_VIEW", repair.compare_views, app(spec=relogged(spec())), app())
        self.assertStopped("TOKEN_VIEW", repair.compare_views, app(pending_deployment={"id": NEW_ID}), app())
        redacted = spec()
        redacted["services"][0]["envs"][1]["value"] = ""
        self.assertStopped("TOKEN_VIEW", repair.compare_views, app(spec=redacted), app())

    def test_active_continuation_requires_the_running_deployment_spec(self) -> None:
        running = app(active_deployment={"id": NEW_ID, "phase": "ACTIVE"})
        provider = mock.Mock()
        provider.deployment.return_value = {"id": NEW_ID, "phase": "ACTIVE", "spec": spec()}
        repair.require_active_spec(provider, running, NEW_ID, D)
        stale = spec()
        stale["services"][0]["image"]["digest"] = "sha256:" + "2" * 64
        provider.deployment.return_value = {"id": NEW_ID, "phase": "ACTIVE", "spec": stale}
        self.assertStopped("ACTIVE_SPEC", repair.require_active_spec, provider, running, NEW_ID, D)
        provider.deployment.return_value = {"id": NEW_ID, "phase": "SUPERSEDED", "spec": spec()}
        self.assertStopped("ACTIVE_SPEC", repair.require_active_spec, provider, running, NEW_ID, D)

    def reader(self, *names: str) -> mock.Mock:
        base = {"environment": {"name": boot.ENVIRONMENT}, "branch_policies": [], "variables": [],
                "secrets": [{"name": n, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
                            for n in sorted(repair.CANARY_BASE_NAMES)]}
        reader = mock.Mock()
        reader.expected_metadata_sha256 = common.sha256_value(base)
        snapshot = copy.deepcopy(base)
        snapshot["secrets"] += [{"name": n, "created_at": "2026-01-03T00:00:00Z", "updated_at": "2026-01-03T00:00:00Z"}
                                for n in names]
        reader.snapshot.return_value = snapshot
        return reader

    def test_canary_state_distinguishes_reviewed_absent_and_previously_installed(self) -> None:
        absent = self.reader()
        self.assertEqual(repair.canary_state(absent), "absent")
        absent.require_absent.assert_called_once_with()
        present = self.reader(boot.SECRET_NAME)
        self.assertEqual(repair.canary_state(present), "present")
        present.require_absent.assert_not_called()
        self.assertStopped("CANARY_STATE", repair.canary_state, self.reader("UNREVIEWED_SECRET"))
        drifted = self.reader(boot.SECRET_NAME)
        drifted.expected_metadata_sha256 = "0" * 64
        self.assertStopped("CANARY_STATE", repair.canary_state, drifted)

    def test_writer_preflight_reads_environment_and_key_without_writing(self) -> None:
        writer = mock.Mock()
        writer.api.get.return_value = {"key_id": "123", "key": "k" * 44}
        repair.writer_preflight(writer)
        writer.require_absent.assert_called_once_with()
        writer.install.assert_not_called()
        writer.api.get.return_value = {"key_id": "abc", "key": "k"}
        self.assertStopped("WRITER_PREFLIGHT", repair.writer_preflight, writer)

    def test_readback_tolerates_a_bounded_run_of_transport_misses(self) -> None:
        provider = mock.Mock()
        slot = {"id": NEW_ID, "phase": "ACTIVE"}
        provider.app.side_effect = [common.ReleaseError("bounded HTTP operation failed"),
                                    {**app(), "spec": relogged(spec()), "active_deployment": slot}]
        provider.deployment.side_effect = [{"id": NEW_ID, "phase": "ACTIVE", "spec": relogged(spec())}]
        result = repair.await_active(provider, app(), NEW_ID, logins(), sleep=lambda _: None)
        self.assertEqual(result["active_deployment"]["id"], NEW_ID)
        failing = mock.Mock()
        failing.app.side_effect = common.ReleaseError("bounded HTTP operation failed")
        self.assertStopped("DEPLOYMENT_PHASE", repair.await_active, failing, app(), NEW_ID, logins(),
                           sleep=lambda _: None)
        self.assertEqual(failing.app.call_count, repair.READ_MISSES)


class Provider(Pinned):
    def test_routes_are_fixed_and_the_single_put_burns_before_transport(self) -> None:
        provider = repair.DriverProvider("t" * 40, allow_update=True)
        self.assertStopped("DRIVER_SELECTION", provider._get, "/v2/apps/" + APP_ID)
        self.assertStopped("DRIVER_SELECTION", provider._get, "/v2/databases")
        self.assertStopped("DRIVER_SELECTION", provider.account)
        provider.app_id = APP_ID
        self.assertStopped("DRIVER_SELECTION", provider._get, "/v2/apps/" + OTHER_APP)
        self.assertStopped("DRIVER_SELECTION", provider._get, "/v2/apps/" + APP_ID + "/logs")
        opener = mock.Mock()
        opener.open.side_effect = OSError("offline")
        with mock.patch.object(provider, "_DriverProvider__opener", opener):
            self.assertStopped("UPDATE_REJECTED", provider.update, spec())
            self.assertTrue(provider.attempted)
            self.assertStopped("UPDATE_REJECTED", provider.update, spec())
        self.assertEqual(opener.open.call_count, 1)
        self.assertNotIn("t" * 40, repr(provider))

    def test_plan_provider_can_never_update(self) -> None:
        provider = repair.DriverProvider("t" * 40, allow_update=False)
        provider.app_id = APP_ID
        self.assertStopped("UPDATE_REJECTED", provider.update, spec())
        self.assertFalse(provider.attempted)


class RejectionDiagnostics(Pinned):
    def test_rejection_detail_is_a_fixed_content_free_classification(self) -> None:
        body = json.dumps({"id": "bad_request", "message": "Validation of db_user credential failed for crm-x"}).encode()
        detail = repair.rejection_detail(400, body)
        self.assertEqual(detail, {"http_status": 400, "provider_error_id": "bad_request",
                                  "message_keywords": ["credential", "db_user"]})
        self.assertNotIn("crm-x", json.dumps(detail))
        self.assertEqual(repair.rejection_detail(403, b'{"id":"Forbidden <x>","message":7}'),
                         {"http_status": 403, "provider_error_id": "unrecognized", "message_keywords": []})
        self.assertEqual(repair.rejection_detail(999, b"not json")["http_status"], None)
        self.assertEqual(repair.rejection_detail(400, b"x" * (repair.MAX_ERROR_BODY + 1))["provider_error_id"],
                         "unrecognized")

    def test_failure_report_revalidates_every_detail_field(self) -> None:
        good = repair.Stopped("UPDATE_REJECTED", {"http_status": 403, "provider_error_id": "forbidden",
                                                  "message_keywords": ["scope", "scope"],
                                                  "update_view_spec_sha256": "a" * 64,
                                                  "update_view_public_spec_sha256": "b" * 64})
        with mock.patch.object(repair, "STAGE", "UPDATE_APP"):
            report = repair.failure_report(good)
            self.assertEqual(report, {"schema_version": 1, "state": "driver-login-repair-stopped", "stage": "UPDATE_APP",
                                      "code": "UPDATE_REJECTED", "retry_authorized": False, "http_status": 403,
                                      "provider_error_id": "forbidden", "message_keywords": ["scope"],
                                      "update_view_spec_sha256": "a" * 64, "update_view_public_spec_sha256": "b" * 64})
            hostile = repair.Stopped("UPDATE_REJECTED", {"http_status": "403", "provider_error_id": "a b",
                                                         "message_keywords": ["password=x"], "message": "leak",
                                                         "update_view_spec_sha256": "leak"})
            self.assertEqual(set(repair.failure_report(hostile)),
                             {"schema_version", "state", "stage", "code", "retry_authorized"})
            other = repair.Stopped("DEPLOYMENT_ERROR", {"http_status": 400})
            self.assertNotIn("http_status", repair.failure_report(other))

    def test_http_rejection_is_classified_without_retry(self) -> None:
        import io
        import urllib.error
        provider = repair.DriverProvider("t" * 40, allow_update=True)
        provider.app_id = APP_ID
        error = urllib.error.HTTPError("https://example.invalid", 400, "Bad Request", {},
                                       io.BytesIO(b'{"id":"bad_request","message":"invalid spec"}'))
        opener = mock.Mock()
        opener.open.side_effect = error
        with mock.patch.object(provider, "_DriverProvider__opener", opener):
            with self.assertRaises(repair.Stopped) as caught:
                provider.update(spec())
        self.assertEqual(caught.exception.detail, {"http_status": 400, "provider_error_id": "bad_request",
                                                   "message_keywords": ["invalid", "spec"]})
        self.assertEqual(opener.open.call_count, 1)

    def test_spec_fingerprints_blank_only_secret_values(self) -> None:
        base = repair.spec_fingerprints(spec(), "read_view")
        self.assertEqual(set(base), {"read_view_spec_sha256", "read_view_public_spec_sha256"})
        rotated = repair.spec_fingerprints(relogged(spec()), "read_view")
        self.assertNotEqual(base["read_view_spec_sha256"], rotated["read_view_spec_sha256"])
        self.assertEqual(base["read_view_public_spec_sha256"], rotated["read_view_public_spec_sha256"])
        moved = spec()
        moved["services"][0]["image"]["digest"] = "sha256:" + "3" * 64
        self.assertNotEqual(base["read_view_public_spec_sha256"],
                            repair.spec_fingerprints(moved, "read_view")["read_view_public_spec_sha256"])


class Reporting(unittest.TestCase):
    def test_failure_report_is_content_free(self) -> None:
        with mock.patch.object(repair, "STAGE", "UPDATE_APP"):
            report = repair.failure_report(repair.Stopped("DEPLOYMENT_ERROR"))
        self.assertEqual(report, {"schema_version": 1, "state": "driver-login-repair-stopped", "stage": "UPDATE_APP",
                                  "code": "DEPLOYMENT_ERROR", "retry_authorized": False})
        leaked = common.ReleaseError("secret " + "x" * 40)
        self.assertNotIn("x" * 40, json.dumps(repair.failure_report(leaked)))
        self.assertIsNone(repair.Stopped("NOT_A_CODE").code)

    def test_main_rejects_wrong_private_inputs_before_any_request(self) -> None:
        base = {"REPAIR_MODE": "plan", "GH_TOKEN": "g" * 40, "CRM_CANARY_FIXTURE_INPUT_JSON": "{}",
                "CRM_CANARY_DRIVER_BOOTSTRAP_JSON": "{}", "DO_DRIVER_BOOTSTRAP_READ_TOKEN": "d" * 40}
        with mock.patch.object(repair, "run") as run:
            for env, argv in (({**base, "GH_CANARY_ENVIRONMENT_WRITE_TOKEN": "w" * 40}, ["plan"]),
                              ({**base, "DO_DRIVER_RECOVERY_UPDATE_TOKEN": "u" * 40}, ["plan"]),
                              ({**base, "REPAIR_MODE": "apply", "DO_DRIVER_RECOVERY_UPDATE_TOKEN": "u" * 40},
                               ["apply", "--output-dir", "somewhere"]),
                              ({**base, "REPAIR_MODE": "apply"}, ["plan"]),
                              ({key: value for key, value in base.items() if key != "GH_TOKEN"}, ["plan"]),
                              (base, ["plan", "--output-dir", "somewhere"])):
                with mock.patch.dict(repair.os.environ, env, clear=True), \
                        mock.patch("sys.stderr") as stderr, mock.patch("sys.stdout"):
                    self.assertEqual(repair.main([*argv, "--control-root", str(ROOT)]), 1)
                    self.assertNotIn("d" * 40, "".join(str(c) for c in stderr.write.call_args_list))
            run.assert_not_called()


class RollEvidence(Pinned):
    def test_prior_pins_start_with_the_current_target_and_stay_distinct(self) -> None:
        self.assertIs(repair.PRIOR_DRIVER_EVIDENCE[0], repair.TARGET_DRIVER_EVIDENCE)
        for key in ("digest", "driver_version_sha256"):
            values = [evidence[key] for evidence in repair.PRIOR_DRIVER_EVIDENCE]
            self.assertEqual(len(values), len(set(values)))
        for evidence in repair.PRIOR_DRIVER_EVIDENCE:
            self.assertEqual(set(evidence), boot.DRIVER_EVIDENCE_KEYS)

    def test_new_evidence_is_exact_canonical_public_evidence(self) -> None:
        self.assertEqual(repair.load_driver_evidence(evidence_raw()), ROLL_EVIDENCE)
        prior = repair.TARGET_DRIVER_EVIDENCE
        bad = [None, "", 7, json.dumps(ROLL_EVIDENCE), json.dumps(ROLL_EVIDENCE, sort_keys=True, indent=1),
               evidence_raw()[:-1], evidence_raw() + "\n", "x" * (repair.MAX_EVIDENCE_BYTES + 1),
               '{"control_sha":"' + "c" * 40 + '","control_sha":"' + "c" * 40 + '"}']
        mutations = [
            {"extra": "x"}, {"run_id": 111111111}, {"artifact_id": "0"}, {"control_sha": "C" * 40},
            {"control_sha": "c" * 39}, {"digest": "b2" * 32}, {"artifact_digest": "sha256:" + "A" * 64},
            {"driver_version_sha256": "d3" * 31}, {"digest": prior["digest"]},
            {"driver_version_sha256": prior["driver_version_sha256"]},
        ]
        for change in mutations:
            bad.append(evidence_raw({**ROLL_EVIDENCE, **change}))
        missing = dict(ROLL_EVIDENCE)
        missing.pop("artifact_id")
        bad.append(evidence_raw(missing))
        for raw in bad:
            with self.subTest(raw=str(raw)[:40]):
                self.assertStopped("DRIVER_EVIDENCE", repair.load_driver_evidence, raw)


class RollState(Pinned):
    def test_accepts_a_driver_active_at_a_reviewed_prior_pin(self) -> None:
        result = repair.validate_roll_state(running(), active_history(), D, ROLL_EVIDENCE)
        self.assertEqual(result, ({FAILED_ID, REDEPLOYED_ID, LATER_ID, NEW_ID}, NEW_ID, repair.TARGET_DRIVER_EVIDENCE))
        replaced = active_history((UNKNOWN_ID, "SUPERSEDED"))
        self.assertEqual(repair.validate_roll_state(running(), replaced, D, ROLL_EVIDENCE)[1], NEW_ID)

    def test_accepts_a_continuation_already_active_at_new(self) -> None:
        rows = history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (LATER_ID, "ERROR"),
                       (NEW_ID, "SUPERSEDED"), (ROLL_ID, "ACTIVE"))
        result = repair.validate_roll_state(running(rolled(spec()), ROLL_ID), rows, D, ROLL_EVIDENCE)
        self.assertEqual(result, ({FAILED_ID, REDEPLOYED_ID, LATER_ID, NEW_ID, ROLL_ID}, ROLL_ID, None))

    def test_superseded_history_is_accepted_only_in_roll_modes(self) -> None:
        replaced = active_history((UNKNOWN_ID, "SUPERSEDED"))
        self.assertStopped("DRIVER_HISTORY", repair.validate_driver_state, running(), replaced, D)
        repair.validate_roll_state(running(), replaced, D, ROLL_EVIDENCE)
        self.assertNotIn("SUPERSEDED", repair.FAILED_PHASES)

    def test_rejects_a_prestate_at_neither_prior_nor_new(self) -> None:
        for evidence in ({**ROLL_EVIDENCE, "digest": "sha256:" + "e" * 64},
                         {**ROLL_EVIDENCE, "digest": repair.TARGET_DRIVER_EVIDENCE["digest"]},
                         {**repair.TARGET_DRIVER_EVIDENCE, "digest": ROLL_EVIDENCE["digest"]}):
            with self.subTest(evidence=evidence["digest"][:16]):
                self.assertStopped("DRIVER_PINS", repair.validate_roll_state, running(rolled(spec(), evidence)),
                                   active_history(), D, ROLL_EVIDENCE)

    def test_rejects_an_idle_busy_or_drifted_driver(self) -> None:
        self.assertStopped("DRIVER_NOT_IDLE", repair.validate_roll_state, app(), history(), D, ROLL_EVIDENCE)
        self.assertStopped("DRIVER_NOT_IDLE", repair.validate_roll_state,
                           running(pending_deployment={"id": ROLL_ID}), active_history(), D, ROLL_EVIDENCE)
        self.assertStopped("DRIVER_HISTORY", repair.validate_roll_state, running(),
                           active_history((ROLL_ID, "DEPLOYING")), D, ROLL_EVIDENCE)
        self.assertStopped("DRIVER_HISTORY", repair.validate_roll_state, running(),
                           history((FAILED_ID, "ERROR"), (NEW_ID, "ACTIVE")), D, ROLL_EVIDENCE)
        drifted = rolled(spec())
        drifted["services"][0]["instance_count"] = 2
        self.assertStopped("SPEC_SERVICE", repair.validate_roll_state, running(drifted, ROLL_ID),
                           history((FAILED_ID, "ERROR"), (REDEPLOYED_ID, "ERROR"), (ROLL_ID, "ACTIVE")),
                           D, ROLL_EVIDENCE)


class RollDelta(Pinned):
    def test_proposal_changes_exactly_the_image_digest_and_version_value(self) -> None:
        before = spec()
        proposal, paths = repair.build_roll_proposal(before, ROLL_EVIDENCE)
        self.assertEqual(paths, ["spec.services[0].envs[5].value", "spec.services[0].image.digest"])
        self.assertEqual(repair.diff_paths(before, proposal), paths)
        self.assertEqual(proposal, rolled(spec()))
        self.assertEqual(before, spec())
        self.assertStopped("PLAN_DELTA", repair.build_roll_proposal, rolled(spec()), ROLL_EVIDENCE)

    def test_carried_check_rejects_any_other_change_or_any_changed_secret_ciphertext(self) -> None:
        before = spec()
        repair.require_roll_carried(before, rolled(before), "POSTSTATE", ROLL_EVIDENCE)
        for key in sorted(repair.SECRET_KEYS):
            with self.subTest(secret=key[11:]):
                self.assertStopped("POSTSTATE", repair.require_roll_carried, before,
                                   rolled(relogged(before, other=key)), "POSTSTATE", ROLL_EVIDENCE)
        changes = [
            lambda s: s["services"][0].update(instance_count=2),
            lambda s: s["databases"][0].update(db_user="doadmin"),
            lambda s: s["services"][0]["envs"][6].update(value="postgresql://literal"),
            lambda s: s["services"][0]["image"].update(digest="sha256:" + "f" * 64),
            lambda s: s["services"][0]["envs"][5].update(value=repair.TARGET_DRIVER_EVIDENCE["driver_version_sha256"]),
            lambda s: s["services"][0]["image"].update(digest=repair.TARGET_DRIVER_EVIDENCE["digest"]),
            lambda s: s["services"][0]["envs"].reverse(),
            lambda s: s["services"][0]["envs"].pop(0),
            lambda s: s["services"][0]["envs"][0].update(type="GENERAL"),
            lambda s: s.update(region="nyc"),
        ]
        for index, change in enumerate(changes):
            after = rolled(before)
            change(after)
            with self.subTest(change=index):
                self.assertStopped("POSTSTATE", repair.require_roll_carried, before, after, "POSTSTATE", ROLL_EVIDENCE)
        self.assertStopped("POSTSTATE", repair.require_roll_carried, before, None, "POSTSTATE", ROLL_EVIDENCE)


class RollUpdateAndReadback(Pinned):
    KNOWN = {FAILED_ID, REDEPLOYED_ID, LATER_ID, NEW_ID}

    def response(self, **changes) -> dict:
        pending = {"id": ROLL_ID, "phase": "PENDING_DEPLOY", "spec": rolled(spec())}
        value = {**running(rolled(spec())), "pending_deployment": pending}
        value.update(changes)
        return {"app": value}

    def test_update_response_keeps_the_prior_active_and_binds_one_new_deployment(self) -> None:
        self.assertEqual(repair.validate_roll_update_response(self.response(), app(), self.KNOWN, NEW_ID,
                                                              ROLL_EVIDENCE), ROLL_ID)
        cases = [
            self.response(pending_deployment={"id": LATER_ID, "phase": "PENDING_DEPLOY"}),
            self.response(pending_deployment={"id": ROLL_ID, "phase": "ERROR"}),
            self.response(active_deployment=None),
            self.response(active_deployment={"id": UNKNOWN_ID, "phase": "ACTIVE"}),
            self.response(pinned_deployment={"id": ROLL_ID}),
            self.response(owner_uuid=OTHER_APP),
            self.response(spec=rolled(relogged(spec(), other="CRM_CANARY_HMAC_KEY_BASE64"))),
            self.response(pending_deployment={"id": ROLL_ID, "phase": "PENDING_DEPLOY", "spec": spec()}),
        ]
        for index, value in enumerate(cases):
            with self.subTest(case=index):
                self.assertStopped("UPDATE_RESPONSE", repair.validate_roll_update_response, value, app(),
                                   self.KNOWN, NEW_ID, ROLL_EVIDENCE)

    def provider(self, phases: list[str], *, active: str | None = None, deployment_spec: dict | None = None):
        provider = mock.Mock()
        apps = []
        for phase in phases:
            slot = {"id": ROLL_ID, "phase": phase}
            value = running(rolled(spec()), ROLL_ID if phase == "ACTIVE" else (active or NEW_ID))
            if phase != "ACTIVE":
                value["in_progress_deployment"] = slot
            apps.append(value)
        provider.app.side_effect = apps
        provider.deployment.side_effect = [{"id": ROLL_ID, "phase": p,
                                            "spec": deployment_spec or rolled(spec())} for p in phases]
        return provider

    def test_await_roll_active_follows_monotone_phases_while_the_prior_stays_active(self) -> None:
        sleeps = []
        result = repair.await_roll_active(self.provider(["PENDING_DEPLOY", "DEPLOYING", "ACTIVE"]), app(), ROLL_ID,
                                          NEW_ID, ROLL_EVIDENCE, sleep=sleeps.append)
        self.assertEqual(result["active_deployment"]["id"], ROLL_ID)
        self.assertEqual(sleeps, [repair.POLL_SECONDS] * 2)

    def test_await_roll_active_stops_fail_closed(self) -> None:
        quiet = {"sleep": lambda _: None}
        self.assertStopped("DEPLOYMENT_ERROR", repair.await_roll_active, self.provider(["DEPLOYING", "ERROR"]),
                           app(), ROLL_ID, NEW_ID, ROLL_EVIDENCE, **quiet)
        self.assertStopped("DEPLOYMENT_CANCELED", repair.await_roll_active, self.provider(["SUPERSEDED"]),
                           app(), ROLL_ID, NEW_ID, ROLL_EVIDENCE, **quiet)
        # Each stop is followed by an ACTIVE state, so a missing check could not pass silently.
        self.assertStopped("DEPLOYMENT_PHASE", repair.await_roll_active,
                           self.provider(["DEPLOYING", "BUILDING", "ACTIVE"]), app(), ROLL_ID, NEW_ID, ROLL_EVIDENCE,
                           **quiet)
        self.assertStopped("DEPLOYMENT_PHASE", repair.await_roll_active,
                           self.provider(["DEPLOYING", "ACTIVE"], active=UNKNOWN_ID), app(), ROLL_ID, NEW_ID,
                           ROLL_EVIDENCE, **quiet)
        self.assertStopped("DEPLOYMENT_SPEC", repair.await_roll_active,
                           self.provider(["DEPLOYING"], deployment_spec=rolled(relogged(spec()))), app(), ROLL_ID,
                           NEW_ID, ROLL_EVIDENCE, **quiet)
        with mock.patch.object(repair, "POLL_LIMIT", 2):
            self.assertStopped("DEPLOYMENT_TIMEOUT", repair.await_roll_active,
                               self.provider(["DEPLOYING", "DEPLOYING"]), app(), ROLL_ID, NEW_ID, ROLL_EVIDENCE,
                               **quiet)


SECRET_TIMES = {"created_at": "2026-01-03T00:00:00Z", "updated_at": "2026-01-03T00:00:00Z"}


class FakeEnvironmentAPI:
    """Synthetic canary environment metadata; the PUT swaps in the next rows."""
    PREFIX = repair.fixture.API_PREFIX + "/environments/" + boot.ENVIRONMENT

    def __init__(self, names, *, after=None, keys=None):
        self.rows = [{"name": n, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
                     for n in sorted(repair.CANARY_BASE_NAMES)]
        self.rows += [{"name": n, **SECRET_TIMES} for n in names]
        self.after = list(after or [])
        self.keys = list(keys or [])
        self.key = {"key_id": "123", "key": base64.b64encode(b"k" * 32).decode()}
        self.routes = []

    def get(self, path: str):
        self.routes.append(path)
        if path == self.PREFIX:
            return {"id": 1, "name": boot.ENVIRONMENT, "protection_rules": [],
                    "deployment_branch_policy": {"protected_branches": False, "custom_branch_policies": True}}
        if path == self.PREFIX + "/secrets/public-key":
            return dict(self.keys.pop(0) if self.keys else self.key)
        if path.startswith(self.PREFIX + "/secrets?"):
            return {"total_count": len(self.rows), "secrets": copy.deepcopy(self.rows)}
        if path.startswith(self.PREFIX + "/variables?"):
            return {"total_count": 0, "variables": []}
        raise AssertionError("unexpected route")

    def pages(self, path: str, key: str):
        assert path == self.PREFIX + "/deployment-branch-policies" and key == "branch_policies"
        return {"total_count": 1, "branch_policies": [{"id": 7, "name": "main", "type": "branch"}]}

    def written(self) -> None:
        if self.after:
            self.rows = self.after.pop(0)


def driver_secret_rows(**times) -> list:
    rows = [{"name": n, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
            for n in sorted(repair.CANARY_BASE_NAMES)]
    return rows + [{"name": boot.SECRET_NAME, **{**SECRET_TIMES, **times}}]


class EnvironmentReplacement(Pinned):
    CONFIG = repair.synthetic_config(ORIGIN, D, ROLL_EVIDENCE)

    def replacer(self, api: FakeEnvironmentAPI) -> repair.EnvironmentReplacer:
        value = repair.EnvironmentReplacer("w" * 40, Path("synthetic-gh"), "0" * 64)
        value.api = api
        rows = api.rows
        api.rows = [row for row in rows if row["name"] != boot.SECRET_NAME]
        value.expected_metadata_sha256 = common.sha256_value(value.snapshot())
        api.rows = rows
        self.sleeps = []
        value.sleep = self.sleeps.append
        return value

    def seal(self, *, returncode: int = 0, extra: int = 0):
        calls = []

        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            sealed = base64.b64encode(b"s" * (len(kwargs["input"]) + 48 + extra)) + b"\n"
            return subprocess.CompletedProcess(argv, returncode, stdout=sealed, stderr=b"")
        return calls, run

    def replace(self, api: FakeEnvironmentAPI, **seal):
        replacer = self.replacer(api)
        calls, run = self.seal(**seal)
        puts = []

        def wire(_opener, url, **kwargs):
            puts.append((url, kwargs))
            api.written()
            return b""
        with mock.patch.object(repair.subprocess, "run", side_effect=run), \
                mock.patch.object(repair.fixture, "_wire", side_effect=wire):
            replacer.preflight()
            try:
                replacer.replace(self.CONFIG)
            finally:
                self.calls, self.puts = calls, puts
        return replacer

    def test_replaces_only_the_existing_driver_secret_once(self) -> None:
        api = FakeEnvironmentAPI([boot.SECRET_NAME], after=[driver_secret_rows(updated_at="2026-01-04T00:00:00Z")])
        replacer = self.replace(api)
        self.assertEqual(len(self.calls), 1)
        argv, kwargs = self.calls[0]
        self.assertEqual(argv, ["synthetic-gh", "secret", "set", boot.SECRET_NAME, "--env", boot.ENVIRONMENT,
                                "--repo", common.REPOSITORY, "--no-store"])
        self.assertEqual(kwargs["input"], common.canonical_payload_bytes(self.CONFIG))
        self.assertEqual(kwargs["env"]["GH_TOKEN"], "w" * 40)
        self.assertEqual(len(self.puts), 1)
        url, request = self.puts[0]
        self.assertEqual(url, "https://api.github.com" + FakeEnvironmentAPI.PREFIX + "/secrets/" + boot.SECRET_NAME)
        self.assertEqual(request["method"], "PUT")
        self.assertEqual(set(json.loads(request["body"])), {"key_id", "encrypted_value"})
        self.assertEqual(self.sleeps, [])
        self.assertNotIn("w" * 40, repr(replacer))
        with mock.patch.object(repair.fixture, "_wire") as wire:
            self.assertStopped("CANARY_REPLACE", replacer.replace, self.CONFIG)
        wire.assert_not_called()

    def test_refuses_absent_extra_or_drifted_metadata_before_any_write(self) -> None:
        for names in ([], [boot.SECRET_NAME, "UNREVIEWED_SECRET"]):
            with self.subTest(names=len(names)):
                replacer = self.replacer(FakeEnvironmentAPI(names))
                with mock.patch.object(repair.subprocess, "run") as run, \
                        mock.patch.object(repair.fixture, "_wire") as wire:
                    self.assertStopped("CANARY_STATE", replacer.preflight)
                run.assert_not_called()
                wire.assert_not_called()
        drifted = self.replacer(FakeEnvironmentAPI([boot.SECRET_NAME]))
        drifted.expected_metadata_sha256 = "0" * 64
        self.assertStopped("CANARY_STATE", drifted.preflight)
        moved_api = FakeEnvironmentAPI([boot.SECRET_NAME])
        moved = self.replacer(moved_api)
        moved.preflight()
        moved_api.rows = driver_secret_rows(updated_at="2026-01-05T00:00:00Z")
        with mock.patch.object(repair.subprocess, "run") as run, mock.patch.object(repair.fixture, "_wire") as wire:
            self.assertStopped("CANARY_STATE", moved.replace, self.CONFIG)
        run.assert_not_called()
        wire.assert_not_called()

    def test_rejects_a_bad_seal_or_moved_key_before_the_put(self) -> None:
        for seal in ({"returncode": 1}, {"extra": 1}):
            with self.subTest(seal=seal):
                with self.assertRaises(repair.Stopped) as caught:
                    self.replace(FakeEnvironmentAPI([boot.SECRET_NAME]), **seal)
                self.assertEqual(caught.exception.code, "CANARY_REPLACE")
                self.assertEqual(self.puts, [])
        other = {"key_id": "124", "key": base64.b64encode(b"o" * 32).decode()}
        api = FakeEnvironmentAPI([boot.SECRET_NAME])
        api.keys = [api.key, api.key, other]  # preflight, before sealing, after sealing
        with self.assertRaises(repair.Stopped) as caught:
            self.replace(api)
        self.assertEqual(caught.exception.code, "CANARY_REPLACE")
        self.assertEqual(self.puts, [])

    def test_metadata_that_moves_during_the_seal_stops_before_the_put(self) -> None:
        api = FakeEnvironmentAPI([boot.SECRET_NAME], after=[driver_secret_rows(updated_at="2026-01-06T00:00:00Z")])
        original = self.seal

        def seal(**kwargs):
            calls, run = original(**kwargs)

            def moving(argv, **options):
                result = run(argv, **options)
                api.rows = driver_secret_rows(updated_at="2026-01-05T00:00:00Z")
                return result
            return calls, moving
        self.seal = seal
        with self.assertRaises(repair.Stopped) as caught:
            self.replace(api)
        self.assertEqual(caught.exception.code, "CANARY_STATE")
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.puts, [])

    def test_the_seal_runs_with_exactly_the_minimal_subprocess_environment(self) -> None:
        api = FakeEnvironmentAPI([boot.SECRET_NAME], after=[driver_secret_rows(updated_at="2026-01-04T00:00:00Z")])
        self.replace(api)
        _argv, kwargs = self.calls[0]
        self.assertEqual(kwargs["env"], boot.subprocess_environment(token="w" * 40))

    def test_post_check_requires_kept_created_at_moved_updated_at_and_unchanged_base(self) -> None:
        late = FakeEnvironmentAPI([boot.SECRET_NAME], after=[driver_secret_rows(updated_at="2026-01-04T00:00:00Z")])
        late.written = lambda: setattr(late, "pending", True)
        reads = {"count": 0}
        original_get = late.get

        def lagging(path):
            value = original_get(path)
            if getattr(late, "pending", False) and path.startswith(late.PREFIX + "/secrets?"):
                reads["count"] += 1
                if reads["count"] >= 2:
                    value["secrets"] = driver_secret_rows(updated_at="2026-01-04T00:00:00Z")
            return value
        late.get = lagging
        self.replace(late)
        self.assertEqual(self.sleeps, [2])
        cases = [driver_secret_rows(created_at="2026-01-04T00:00:00Z", updated_at="2026-01-04T00:00:00Z"),
                 driver_secret_rows(),
                 driver_secret_rows(updated_at="2026-01-02T00:00:00Z"),
                 [{**row, "updated_at": "2026-01-04T00:00:00Z"} for row in driver_secret_rows()]]
        for index, rows in enumerate(cases):
            with self.subTest(case=index):
                api = FakeEnvironmentAPI([boot.SECRET_NAME], after=[rows])
                with self.assertRaises(repair.Stopped) as caught:
                    self.replace(api)
                self.assertEqual(caught.exception.code, "CANARY_REPLACE")
                self.assertEqual(len(self.puts), 1)


class PinnedEvidenceExecutable(unittest.TestCase):
    def test_a_second_pinned_gh_fetch_fails_closed_without_any_download(self) -> None:
        # The real helper refuses an existing binary, so a roll must fetch it once.
        with tempfile.TemporaryDirectory() as temp:
            (Path(temp) / "fixture-gh").mkdir()
            (Path(temp) / "fixture-gh" / "gh").write_bytes(b"synthetic")
            with mock.patch.dict(repair.os.environ, {"RUNNER_TEMP": temp}), \
                    mock.patch.object(repair.fixture.urllib.request, "build_opener",
                                      side_effect=AssertionError("network forbidden")):
                with self.assertRaisesRegex(common.ReleaseError, "unexpected existing evidence executable"):
                    repair.fixture._pinned_gh()


class DriverWorld:
    """One synthetic driver app shared by the read-token and update-token views."""

    def __init__(self, *, at_new: bool = False, phases: tuple = ("PENDING_DEPLOY", "DEPLOYING", "ACTIVE")):
        self.prior_spec = spec()
        self.spec = rolled(spec()) if at_new else spec()
        self.active = ROLL_ID if at_new else NEW_ID
        self.rows = {FAILED_ID: "ERROR", REDEPLOYED_ID: "ERROR", LATER_ID: "ERROR",
                     NEW_ID: "SUPERSEDED" if at_new else "ACTIVE"}
        if at_new:
            self.rows[ROLL_ID] = "ACTIVE"
        self.phases, self.step, self.rolling = list(phases), 0, False
        self.puts, self.update_view, self.moved_host = [], None, None
        # Late-drift hooks: a read-token app view that changes after its first
        # read, rows that only the update token lists, and a default origin
        # that moves after the first update-token read even without a PUT.
        self.read_reads, self.update_reads, self.read_drift = 0, 0, False
        self.update_history_extra, self.move_without_put = (), False

    def phase(self) -> str:
        return self.phases[min(self.step, len(self.phases) - 1)]

    def app(self, *, update_token: bool = False) -> dict:
        if self.rolling and self.phase() == "ACTIVE":
            self.rolling, self.active = False, ROLL_ID
            self.rows.update({NEW_ID: "SUPERSEDED", ROLL_ID: "ACTIVE"})
        if update_token:
            self.update_reads += 1
        else:
            self.read_reads += 1
        moved = self.puts or (self.move_without_put and self.update_reads > 1)
        host = self.moved_host if self.moved_host and moved else HOST
        value = running(copy.deepcopy(self.spec), self.active, default_ingress="https://" + host,
                        live_url="https://" + host, live_domain=host)
        if not update_token and self.read_drift and self.read_reads > 1:
            value["spec"]["region"] = "nyc"
        if self.rolling:
            value["in_progress_deployment"] = {"id": ROLL_ID, "phase": self.phase()}
        if update_token and self.update_view is not None:
            value["spec"] = copy.deepcopy(self.update_view)
        return value

    def history(self, *, update_token: bool = False) -> dict:
        return history(*self.rows.items(), *(self.update_history_extra if update_token else ()))

    def deployment(self, identity: str) -> dict:
        if identity == ROLL_ID and self.rolling:
            phase = self.phase()
            self.step += 1
            if phase == "ERROR":
                self.rolling = False
            self.rows[ROLL_ID] = phase
            return {"id": ROLL_ID, "phase": phase, "spec": copy.deepcopy(self.spec)}
        spec_value = self.prior_spec if identity == NEW_ID else rolled(spec())
        return {"id": identity, "phase": self.rows[identity], "spec": copy.deepcopy(spec_value)}

    def update(self, value: dict) -> dict:
        self.puts.append(copy.deepcopy(value))
        self.spec, self.rolling, self.step = copy.deepcopy(value), True, 0
        self.rows[ROLL_ID] = self.phases[0]
        response = running(copy.deepcopy(value), self.active)
        response["pending_deployment"] = {"id": ROLL_ID, "phase": self.phases[0], "spec": copy.deepcopy(value)}
        return {"app": response}


class FakeDriver:
    def __init__(self, world: DriverWorld, token: str, *, allow_update: bool):
        self.world, self.token, self.allow_update, self.app_id = world, token, allow_update, None

    def select(self, app_name: str) -> None:
        assert app_name == PLAN["app_name"] and self.app_id is None
        self.app_id = APP_ID

    def account(self) -> dict:
        assert not self.allow_update
        return {"status": "active"}

    def app(self) -> dict:
        return self.world.app(update_token=self.allow_update)

    def deployments(self) -> dict:
        return self.world.history(update_token=self.allow_update)

    def deployment(self, identity: str) -> dict:
        return self.world.deployment(identity)

    def update(self, value: dict) -> dict:
        assert self.allow_update
        return self.world.update(value)


class FakeReplacer:
    instances: list = []
    after_replace = None  # Optional hook: drift that lands right after the replacement.

    def __init__(self, token: str, gh: Path, expected: str):
        self.token, self.gh, self.expected = token, gh, expected
        self.preflights, self.configs, self.sleep = 0, [], None
        FakeReplacer.instances.append(self)

    def preflight(self) -> None:
        self.preflights += 1

    def replace(self, config: dict) -> None:
        assert self.preflights == 1
        self.configs.append(copy.deepcopy(config))
        if FakeReplacer.after_replace is not None:
            FakeReplacer.after_replace()


ROLL_D = {**D, "plan": {**PLAN, "github_environment_sha256": "e" * 64}}


class GuardedPrivate(dict):
    """main() passes every private name, absent ones as None; the fixture input
    must never even be looked up in roll modes."""
    def __getitem__(self, key):
        if key == repair.FIXTURE_INPUT_NAME:
            raise AssertionError("roll modes never read the fixture input")
        return super().__getitem__(key)

    def get(self, key, default=None):
        if key == repair.FIXTURE_INPUT_NAME:
            raise AssertionError("roll modes never read the fixture input")
        return super().get(key, default)


class RollRun(Pinned):
    SHA = "f" * 40

    def setUp(self) -> None:
        super().setUp()
        FakeReplacer.instances, FakeReplacer.after_replace = [], None
        self.events, self.gh_calls, self.health = [], [], []
        canary = TokenViewsAndContinuation.reader(self, boot.SECRET_NAME)

        def pinned_gh():
            self.gh_calls.append(1)
            if len(self.gh_calls) > 1:
                common.fail("unexpected existing evidence executable")
            return Path("synthetic-gh-1")
        patches = [
            mock.patch.object(repair, "load_origin", return_value={"synthetic": True}),
            mock.patch.object(boot, "GitHubRead", return_value="api"),
            mock.patch.object(repair.fixture, "_current_guard", return_value=self.SHA),
            mock.patch.object(repair.recovery, "authenticate_origin", return_value=({}, {})),
            mock.patch.object(policy, "validate_origin", return_value=ROLL_D),
            mock.patch.object(repair.fixture, "_pinned_gh", side_effect=pinned_gh),
            mock.patch.object(boot, "authenticate_driver", side_effect=lambda *a, **k: self.events.append("image")),
            mock.patch.object(boot, "EnvironmentReader", return_value=canary),
            mock.patch.object(policy, "validate_account_owner"),
            mock.patch.object(repair, "EnvironmentReplacer", FakeReplacer),
            mock.patch.object(boot, "health", side_effect=self.health.append),
        ]
        for patcher in patches:
            patcher.start()
            self.addCleanup(patcher.stop)
        self.canary = canary
        self.drivers = []

    def private(self, mode: str) -> GuardedPrivate:
        names = repair.REQUIRED_PRIVATE_NAMES[mode]
        return GuardedPrivate({name: None if name not in names else "{}" if name == "CRM_CANARY_DRIVER_BOOTSTRAP_JSON"
                               else "p-" + name.lower() + "-synthetic" for name in repair.PRIVATE_NAMES})

    def roll(self, world: DriverWorld, mode: str) -> dict:
        def factory(token, *, allow_update):
            self.events.append("provider")
            driver = FakeDriver(world, token, allow_update=allow_update)
            self.drivers.append(driver)
            return driver
        with mock.patch.object(repair, "DriverProvider", side_effect=factory):
            return repair.run_roll(mode, ROOT, self.private(mode), "g" * 40, "{}", evidence_raw(),
                                   Path("out") if mode == "roll-apply" else None, sleep=lambda _: None)

    def assertContentFree(self, report: dict) -> None:
        raw = json.dumps(report)
        for value in (APP_ID, NEW_ID, ROLL_ID, FAILED_ID, HOST, D["hmac_key_base64"], "EV[", "example.invalid",
                      "k" * 43, "-synthetic", "ondigitalocean", "postgresql"):
            self.assertNotIn(value, raw)

    def test_roll_plan_reports_the_exact_delta_without_any_write(self) -> None:
        world = DriverWorld()
        report = self.roll(world, "roll-plan")
        self.assertEqual(report["state"], "driver-image-roll-planned")
        self.assertEqual(report["kind"], repair.ROLL_KIND)
        self.assertEqual(report["prior_driver_evidence"], repair.TARGET_DRIVER_EVIDENCE)
        self.assertEqual(report["driver_evidence"], ROLL_EVIDENCE)
        self.assertEqual(report["changed_spec_paths"], ["spec.services[0].envs[5].value",
                                                        "spec.services[0].image.digest"])
        self.assertEqual(report["changed_env_keys"], [repair.VERSION_KEY])
        self.assertEqual(report["carried_secret_keys"], sorted(repair.SECRET_KEYS))
        self.assertFalse(report["driver_already_at_new"])
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances, [])
        self.assertEqual([d.allow_update for d in self.drivers], [False])
        self.assertEqual(len(self.gh_calls), 1)
        boot.authenticate_driver.assert_called_once_with("api", ROOT, Path("synthetic-gh-1"), ROLL_EVIDENCE,
                                                         self.SHA, gh_token="g" * 40)
        self.assertContentFree(report)

    def test_roll_apply_puts_once_then_replaces_only_the_driver_version(self) -> None:
        world = DriverWorld()
        report = self.roll(world, "roll-apply")
        self.assertEqual(world.puts, [rolled(spec())])
        self.assertEqual(world.active, ROLL_ID)
        self.assertEqual(self.health, [ORIGIN, ORIGIN])
        [replacer] = FakeReplacer.instances
        self.assertEqual(len(self.gh_calls), 1)
        self.assertEqual(replacer.gh, Path("synthetic-gh-1"))
        self.assertEqual(replacer.token, "p-gh_canary_environment_write_token-synthetic")
        self.assertEqual(replacer.configs, [{**repair.synthetic_config(ORIGIN, D),
                                             "driver_version_sha256": ROLL_EVIDENCE["driver_version_sha256"]}])
        self.assertEqual(report["state"], "driver-image-roll-complete")
        self.assertEqual(report["deployment_id_sha256"], digest(ROLL_ID))
        self.assertTrue(report["canary_configuration_replaced"])
        self.assertFalse(report["synthetic_execution_performed"])
        self.assertEqual([d.allow_update for d in self.drivers], [False, True])
        self.assertContentFree(report)

    def test_continuation_at_new_skips_the_put_and_re_replaces(self) -> None:
        world = DriverWorld(at_new=True)
        report = self.roll(world, "roll-apply")
        self.assertEqual(world.puts, [])
        self.assertTrue(report["driver_already_at_new"])
        self.assertIsNone(report["prior_driver_evidence"])
        self.assertEqual(report["changed_spec_paths"], [])
        self.assertEqual(report["deployment_id_sha256"], digest(ROLL_ID))
        [replacer] = FakeReplacer.instances
        self.assertEqual(replacer.configs[0]["driver_version_sha256"], ROLL_EVIDENCE["driver_version_sha256"])
        self.assertEqual(len(self.gh_calls), 1)
        # Health is re-proved on the unchanged origin before the re-replacement.
        self.assertEqual(self.health, [ORIGIN, ORIGIN])

    def test_continuation_with_a_moved_origin_stops_before_the_replacement(self) -> None:
        world = DriverWorld(at_new=True)
        world.moved_host, world.move_without_put = "rereply-canary-driver-test-fghij.ondigitalocean.app", True
        self.assertStopped("DRIVER_ORIGIN", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "HEALTH")
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances[0].configs, [])

    def test_main_moved_before_the_put_stops_without_any_write(self) -> None:
        repair.fixture._current_guard.side_effect = [self.SHA, "e" * 40, "e" * 40]
        world = DriverWorld()
        self.assertStopped("PRESTATE_MOVED", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "PRE_UPDATE")
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances, [])

    def test_main_moved_before_the_replacement_stops_after_the_put(self) -> None:
        repair.fixture._current_guard.side_effect = [self.SHA, self.SHA, "e" * 40]
        world = DriverWorld()
        self.assertStopped("PRESTATE_MOVED", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "CANARY_REPLACE")
        self.assertEqual(len(world.puts), 1)
        self.assertEqual(FakeReplacer.instances[0].configs, [])

    def test_a_read_view_that_moves_before_the_put_stops_without_any_write(self) -> None:
        world = DriverWorld()
        world.read_drift = True
        self.assertStopped("PRESTATE_MOVED", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "PRE_UPDATE")
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances, [])

    def test_update_token_history_that_differs_stops_without_any_write(self) -> None:
        world = DriverWorld()
        world.update_history_extra = ((UNKNOWN_ID, "ERROR"),)
        self.assertStopped("PRESTATE_MOVED", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "PRE_UPDATE")
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances, [])

    def test_a_redeploy_after_health_stops_before_the_replacement(self) -> None:
        world = DriverWorld()

        def health(origin):
            self.health.append(origin)
            if len(self.health) == 2:
                world.active = LATER_ID
        boot.health.side_effect = health
        self.assertStopped("PRESTATE_MOVED", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "CANARY_REPLACE")
        self.assertEqual(len(world.puts), 1)
        self.assertEqual(FakeReplacer.instances[0].configs, [])

    def test_drift_right_after_the_replacement_fails_the_poststate(self) -> None:
        world = DriverWorld()
        FakeReplacer.after_replace = lambda: setattr(world, "active", LATER_ID)
        self.assertStopped("POSTSTATE", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "POSTSTATE")
        self.assertEqual(len(FakeReplacer.instances[0].configs), 1)

    def test_image_authority_failure_stops_before_any_provider_or_environment_read(self) -> None:
        boot.authenticate_driver.side_effect = common.ReleaseError("publisher run is not exact successful authority")
        with self.assertRaises(common.ReleaseError):
            self.roll(DriverWorld(), "roll-apply")
        self.assertEqual(repair.STAGE, "IMAGE_AUTHORITY")
        self.assertNotIn("provider", self.events)
        boot.EnvironmentReader.assert_not_called()

    def test_canary_configuration_must_already_be_present(self) -> None:
        absent = TokenViewsAndContinuation.reader(self)
        boot.EnvironmentReader.return_value = absent
        self.assertStopped("CANARY_STATE", self.roll, DriverWorld(), "roll-plan")
        self.assertNotIn("provider", self.events)

    def test_token_view_mismatch_stops_before_the_put(self) -> None:
        world = DriverWorld()
        world.update_view = relogged(spec())
        self.assertStopped("TOKEN_VIEW", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "PRE_UPDATE")
        self.assertEqual(world.puts, [])
        self.assertEqual(FakeReplacer.instances, [])

    def test_a_moved_driver_origin_stops_before_the_replacement(self) -> None:
        world = DriverWorld()
        world.moved_host = "rereply-canary-driver-test-fghij.ondigitalocean.app"
        self.assertStopped("DRIVER_ORIGIN", self.roll, world, "roll-apply")
        self.assertEqual(repair.STAGE, "HEALTH")
        self.assertEqual(len(world.puts), 1)
        self.assertEqual(FakeReplacer.instances[0].configs, [])

    def test_a_new_deployment_ending_in_error_leaves_later_rolls_stopped_fail_closed(self) -> None:
        world = DriverWorld(phases=("PENDING_DEPLOY", "ERROR"))
        self.assertStopped("DEPLOYMENT_ERROR", self.roll, world, "roll-apply")
        self.assertEqual(len(world.puts), 1)
        self.assertEqual(FakeReplacer.instances[0].configs, [])
        # The app spec is at NEW while the PRIOR deployment stays ACTIVE: every
        # later roll stops before any write (runbook: no re-PUT in this state).
        for mode in ("roll-plan", "roll-apply"):
            with self.subTest(mode=mode):
                FakeReplacer.instances = []
                self.gh_calls.clear()  # Each dispatch runs on a fresh runner.
                self.assertStopped("ACTIVE_SPEC", self.roll, world, mode)
                self.assertEqual(repair.STAGE, "DRIVER_PRESTATE")
        self.assertEqual(len(world.puts), 1)

    def test_failure_report_names_the_roll(self) -> None:
        with mock.patch.object(repair, "STAGE", "CANARY_REPLACE"):
            report = repair.failure_report(repair.Stopped("CANARY_REPLACE"), roll=True)
        self.assertEqual(report, {"schema_version": 1, "state": "driver-image-roll-stopped", "stage": "CANARY_REPLACE",
                                  "code": "CANARY_REPLACE", "retry_authorized": False})


class RollMain(unittest.TestCase):
    BASE = {"GH_TOKEN": "g" * 40, "CRM_CANARY_DRIVER_BOOTSTRAP_JSON": "{}", "DO_DRIVER_BOOTSTRAP_READ_TOKEN": "d" * 40,
            "DRIVER_EVIDENCE_JSON": evidence_raw()}
    WRITE = {"DO_DRIVER_RECOVERY_UPDATE_TOKEN": "u" * 40, "GH_CANARY_ENVIRONMENT_WRITE_TOKEN": "w" * 40}

    def main(self, env: dict, argv: list[str]) -> tuple[int, str]:
        with mock.patch.dict(repair.os.environ, env, clear=True), \
                mock.patch("sys.stderr") as stderr, mock.patch("sys.stdout"):
            code = repair.main([*argv, "--control-root", str(ROOT)])
        return code, "".join(str(c) for c in stderr.write.call_args_list)

    def test_private_input_matrix_never_admits_the_fixture_input_in_roll_modes(self) -> None:
        fixture_input = {repair.FIXTURE_INPUT_NAME: "{}"}
        plan = {**self.BASE, "REPAIR_MODE": "roll-plan"}
        apply = {**self.BASE, **self.WRITE, "REPAIR_MODE": "roll-apply"}
        out = ["--output-dir", "somewhere"]
        cases = [
            ({**plan, **fixture_input}, ["roll-plan"]),
            ({**apply, **fixture_input}, ["roll-apply", *out]),
            ({**plan, "DO_DRIVER_RECOVERY_UPDATE_TOKEN": "u" * 40}, ["roll-plan"]),
            ({**plan, "GH_CANARY_ENVIRONMENT_WRITE_TOKEN": "w" * 40}, ["roll-plan"]),
            ({k: v for k, v in apply.items() if k != "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"}, ["roll-apply", *out]),
            ({k: v for k, v in plan.items() if k != "DRIVER_EVIDENCE_JSON"}, ["roll-plan"]),
            ({**plan, "DRIVER_EVIDENCE_JSON": ""}, ["roll-plan"]),
            ({**plan, "DO_DRIVER_BOOTSTRAP_CREATE_TOKEN": "c" * 40}, ["roll-plan"]),
            ({k: v for k, v in plan.items() if k != "GH_TOKEN"}, ["roll-plan"]),
            (apply, ["roll-apply"]),
            (plan, ["roll-plan", *out]),
            (apply, ["roll-plan"]),
            ({**self.BASE, **fixture_input, "REPAIR_MODE": "plan"}, ["plan"]),
        ]
        with mock.patch.object(repair, "run_roll") as run_roll, mock.patch.object(repair, "run") as run:
            for index, (env, argv) in enumerate(cases):
                with self.subTest(case=index):
                    code, stderr = self.main(env, argv)
                    self.assertEqual(code, 1)
                    self.assertNotIn("d" * 40, stderr)
                    self.assertNotIn("w" * 40, stderr)
            run_roll.assert_not_called()
            run.assert_not_called()

    def test_roll_modes_dispatch_with_exactly_their_inputs_and_write_the_receipt(self) -> None:
        record = {"schema_version": 1, "kind": repair.ROLL_KIND, "state": "driver-image-roll-complete"}
        with tempfile.TemporaryDirectory() as temp, \
                mock.patch.object(repair, "run_roll", return_value=record) as run_roll:
            code, _ = self.main({**self.BASE, "REPAIR_MODE": "roll-plan"}, ["roll-plan"])
            self.assertEqual(code, 0)
            mode, _root, private, gh_token, _origin, evidence, output = run_roll.call_args.args
            self.assertEqual((mode, gh_token, evidence, output), ("roll-plan", "g" * 40, evidence_raw(), None))
            self.assertEqual({k for k, v in private.items() if v is not None}, set(repair.ROLL_PLAN_PRIVATE_NAMES))
            target = Path(temp) / "driver-image-roll-receipt"
            env = {**self.BASE, **self.WRITE, "REPAIR_MODE": "roll-apply", "RUNNER_TEMP": temp}
            code, _ = self.main(env, ["roll-apply", "--output-dir", str(target)])
            self.assertEqual(code, 0)
            private = run_roll.call_args.args[2]
            self.assertEqual({k for k, v in private.items() if v is not None}, set(repair.ROLL_APPLY_PRIVATE_NAMES))
            self.assertIsNone(private[repair.FIXTURE_INPUT_NAME])
            receipt = (target / "receipt.json").read_bytes()
            self.assertEqual(receipt, common.canonical_file_bytes(record))
            self.assertEqual((target / "receipt.sha256").read_text(encoding="ascii"),
                             common.sha256_bytes(receipt) + "\n")

    def test_roll_failures_are_content_free_and_named(self) -> None:
        with mock.patch.object(repair, "run_roll", side_effect=common.ReleaseError("secret " + "x" * 40)):
            code, stderr = self.main({**self.BASE, "REPAIR_MODE": "roll-plan"}, ["roll-plan"])
        self.assertEqual(code, 1)
        self.assertIn("driver-image-roll-stopped", stderr)
        self.assertNotIn("x" * 40, stderr)


class Workflow(unittest.TestCase):
    source = WORKFLOW.read_text(encoding="utf-8")

    def job(self, name: str) -> str:
        match = re.search(r"(?ms)^  " + name + r":\n(.*?)(?=^  [a-z]+:\n|\Z)", self.source)
        self.assertIsNotNone(match)
        return match.group(1)

    def test_dispatch_only_protected_fixture_environment_with_pinned_actions(self) -> None:
        self.assertRegex(self.source, r"(?m)^on:\n  workflow_dispatch:\n")
        self.assertNotRegex(self.source, r"(?m)^  (push|pull_request|schedule|workflow_run):")
        for name in ("plan", "apply"):
            block = self.job(name)
            self.assertIn("environment: rereply-production-crm-fixture", block)
            self.assertIn('[[ "$GITHUB_REF" == refs/heads/main && "$REF_PROTECTED" == true ]]', block)
            self.assertIn('[[ "$GITHUB_RUN_ATTEMPT" == 1 && "$WORKFLOW_SHA" == "$CONTROL_SHA" ]]', block)
            self.assertIn("persist-credentials: false", block)
        for line in re.findall(r"uses: (\S+)", self.source):
            self.assertRegex(line, r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$")

    def test_plan_job_has_no_write_credential_or_permission(self) -> None:
        plan = self.job("plan")
        self.assertIn("if: ${{ inputs.mode == 'plan' }}", plan)
        self.assertNotIn("GH_CANARY_ENVIRONMENT_WRITE_TOKEN", plan)
        self.assertNotIn("DO_DRIVER_RECOVERY_UPDATE_TOKEN", plan)
        self.assertIn("DO_DRIVER_BOOTSTRAP_READ_TOKEN: ${{ secrets.DO_DRIVER_BOOTSTRAP_READ_TOKEN }}", plan)
        self.assertNotIn("write", re.search(r"(?ms)permissions:\n(.*?)\n    steps:", plan).group(1))
        self.assertIn("repair_crm_canary_driver_logins.py plan --control-root control", plan)
        apply = self.job("apply")
        self.assertIn("if: ${{ inputs.mode == 'apply' }}", apply)
        self.assertIn("repair_crm_canary_driver_logins.py apply --control-root control --output-dir", apply)
        for name in ("DO_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_DRIVER_RECOVERY_UPDATE_TOKEN",
                     "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"):
            self.assertIn(name + ": ${{ secrets." + name + " }}", apply)
        self.assertNotIn("DO_DRIVER_BOOTSTRAP_CREATE_TOKEN", self.source)
        self.assertRegex(self.source, r"(?m)^concurrency:\n  group: rereply-production\n  cancel-in-progress: false$")

    def test_job_keys_have_no_hyphens_and_cover_all_four_modes(self) -> None:
        jobs = self.source.split("\njobs:\n", 1)[1]
        self.assertEqual(re.findall(r"(?m)^  ([^ \n][^:\n]*):$", jobs), ["plan", "apply", "rollplan", "rollapply"])
        options = re.search(r"(?ms)^        options:\n((?:          - [a-z-]+\n)+)", self.source).group(1)
        self.assertEqual(re.findall(r"- ([a-z-]+)", options), ["plan", "apply", "roll-plan", "roll-apply"])
        self.assertIn("      driver_evidence_json:\n", self.source)
        self.assertRegex(self.source, r"(?m)^      driver_evidence_json:\n(?:        .*\n)*?        required: false\n")
        self.assertRegex(self.source, r"(?m)^      driver_evidence_json:\n(?:        .*\n)*?        default: ''\n")
        self.assertIn("\n  DRIVER_EVIDENCE_JSON: ${{ inputs.driver_evidence_json }}\n", self.source)
        run_name = re.search(r"(?m)^run-name: (.*)$", self.source).group(1)
        self.assertEqual(run_name, "${{ (inputs.mode == 'apply' || inputs.mode == 'roll-apply') && 'Apply' || 'Plan' }}"
                                   " CRM canary driver ${{ startsWith(inputs.mode, 'roll-') && 'image roll'"
                                   " || 'login repair' }}")
        for name, mode in (("plan", "plan"), ("apply", "apply"), ("rollplan", "roll-plan"), ("rollapply", "roll-apply")):
            self.assertIn("if: ${{ inputs.mode == '" + mode + "' }}", self.job(name))
        # Dispatch inputs reach scripts only through the reviewed top-level env.
        for line in self.source.splitlines():
            if "${{ inputs." in line:
                self.assertRegex(line, r"^(run-name: |  [A-Z_]+: \$\{\{ inputs\.[a-z_]+ \}\}$|    if: )")

    def test_roll_jobs_are_protected_with_exactly_their_credentials(self) -> None:
        roll_plan, roll_apply = self.job("rollplan"), self.job("rollapply")
        for block in (roll_plan, roll_apply):
            self.assertIn("environment: rereply-production-crm-fixture", block)
            self.assertIn('[[ "$GITHUB_REF" == refs/heads/main && "$REF_PROTECTED" == true ]]', block)
            self.assertIn('[[ "$GITHUB_RUN_ATTEMPT" == 1 && "$WORKFLOW_SHA" == "$CONTROL_SHA" ]]', block)
            self.assertIn("persist-credentials: false", block)
            self.assertNotIn("CRM_CANARY_FIXTURE_INPUT_JSON", block)
            for name in ("GH_TOKEN: ${{ secrets.GH_DRIVER_BOOTSTRAP_READ_TOKEN }}",
                         "CRM_CANARY_DRIVER_BOOTSTRAP_JSON: ${{ secrets.CRM_CANARY_DRIVER_BOOTSTRAP_JSON }}",
                         "DO_DRIVER_BOOTSTRAP_READ_TOKEN: ${{ secrets.DO_DRIVER_BOOTSTRAP_READ_TOKEN }}"):
                self.assertIn(name, block)
        self.assertEqual(len(re.findall(r"\$\{\{ secrets\.", roll_plan)), 3)
        self.assertNotIn("write", re.search(r"(?ms)permissions:\n(.*?)\n    steps:", roll_plan).group(1))
        self.assertIn("repair_crm_canary_driver_logins.py roll-plan --control-root control\n", roll_plan)
        self.assertNotIn("--output-dir", roll_plan)
        self.assertEqual(len(re.findall(r"\$\{\{ secrets\.", roll_apply)), 5)
        for name in ("DO_DRIVER_RECOVERY_UPDATE_TOKEN", "GH_CANARY_ENVIRONMENT_WRITE_TOKEN"):
            self.assertIn(name + ": ${{ secrets." + name + " }}", roll_apply)
        permissions = re.search(r"(?ms)permissions:\n(.*?)\n    steps:", roll_apply).group(1)
        self.assertEqual(permissions.split("\n"), ["      actions: read", "      contents: read",
                                                   "      attestations: write", "      id-token: write"])
        self.assertIn('repair_crm_canary_driver_logins.py roll-apply --control-root control --output-dir '
                      '"$RUNNER_TEMP/driver-image-roll-receipt"', roll_apply)
        self.assertIn("predicate-type: https://rereply.app/attestations/crm-canary-driver-image-roll/v1", roll_apply)
        self.assertIn("name: crm-canary-driver-image-roll-${{ github.run_id }}-1", roll_apply)
        self.assertEqual(roll_apply.count("subject-path: ${{ runner.temp }}/driver-image-roll-receipt/receipt.json"), 2)
        self.assertLess(roll_apply.index("Attest exact driver image roll policy"),
                        roll_apply.index("Upload only the public driver image roll receipt"))


if __name__ == "__main__":
    unittest.main()
