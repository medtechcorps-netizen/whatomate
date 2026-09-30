"""Offline contracts for the protected two-value CRM canary driver login repair.

All identifiers, ciphertexts and credentials are synthetic. No test performs a
provider, GitHub or product request.
"""
from __future__ import annotations

import copy
import hashlib
import json
from pathlib import Path
import re
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


if __name__ == "__main__":
    unittest.main()
