from __future__ import annotations

import copy
import datetime as dt
import unittest
from unittest import mock

try:
    from . import inverse_production_crm_canary_fixture as inverse
    from . import provision_production_crm_canary_fixture as fixture
    from . import test_provision_production_crm_canary_fixture as controls
except ImportError:  # pragma: no cover - direct module execution
    import inverse_production_crm_canary_fixture as inverse
    import provision_production_crm_canary_fixture as fixture
    import test_provision_production_crm_canary_fixture as controls

common = fixture.common


def uid(n):
    return f"{n:08x}-1111-4111-8111-111111111111"


class FakeTransport:
    """Product double: only the routes the inverse may use are implemented."""

    def __init__(self, protected, *, fail_delete_at=None):
        self.p = protected
        self.d = protected["descriptor"]
        self.registration = protected["registration"]
        self.home = self.d["super_admin_home_org_id"]
        self.klinik_org, self.non_klinik_org = uid(200), uid(300)
        self.klinik_user, self.non_klinik_user = uid(201), uid(301)
        self.organizations = [
            {"id": self.home, "name": "Home", "slug": "home",
             "reseller_id": self.d["reseller_id"]},
            {"id": uid(400), "name": "Other", "slug": "other",
             "reseller_id": self.d["reseller_id"]},
            {"id": self.klinik_org, "name": self.d["klinik"]["organization_name"],
             "slug": "rereply-canary-klinik", "reseller_id": self.d["reseller_id"]},
            {"id": self.non_klinik_org, "name": self.d["non_klinik"]["organization_name"],
             "slug": "rereply-canary-non-klinik", "reseller_id": self.d["reseller_id"]},
        ]
        self.users = {
            self.klinik_org: [{"id": self.klinik_user,
                               "email": self.registration["klinik_email"],
                               "organization_id": self.klinik_org, "is_super_admin": False},
                              {"id": self.home, "email": "owner@example.test",
                               "organization_id": self.home, "is_super_admin": True}],
            self.non_klinik_org: [{"id": self.non_klinik_user,
                                   "email": self.registration["non_klinik_email"],
                                   "organization_id": self.non_klinik_org,
                                   "is_super_admin": False}],
        }
        self.calls = []
        self.deletes = []
        self.fail_delete_at = fail_delete_at

    def login(self, email, password):
        return "admin"

    def request(self, method, path, body=None, *, session=None, organization_id=None,
                headers=None, graph=False):
        self.calls.append((method, path, organization_id))
        route = path.split("?")[0]

        def result(data):
            return {"status": "success", "data": copy.deepcopy(data)}

        if route == "/api/me":
            return result({"id": self.d["super_admin_id"], "organization_id": self.home,
                           "is_super_admin": True, "is_active": True})
        if route == "/api/organizations":
            return result({"organizations": copy.deepcopy(self.organizations)})
        if route == "/api/users":
            rows = copy.deepcopy(self.users.get(organization_id, []))
            return result({"users": rows, "total": len(rows), "page": 1, "limit": 100,
                           "online_count": 0})
        raise AssertionError("unexpected inverse route " + route)

    def delete(self, path, *, session, organization_id=None):
        self.deletes.append((path, organization_id))
        if len(self.deletes) == self.fail_delete_at:
            raise common.ReleaseError("bounded HTTP operation failed: status 500")
        route, identity = path.split("/")[2], path.rsplit("/", 1)[1]
        if route == "users":
            for org, rows in self.users.items():
                self.users[org] = [row for row in rows if row["id"] != identity]
        else:
            self.organizations = [row for row in self.organizations if row["id"] != identity]
            self.users.pop(identity, None)


def authority_for(protected, transport, *, expires_in=3600, overrides=None):
    request = {"schema_version": 1, "control_sha": "a" * 40, "operation_sha256": "b" * 64,
               "descriptor_sha256": common.sha256_value(protected["descriptor"])}
    value = {
        "schema_version": 1, "kind": "crm-canary-fixture-inverse-authority",
        "control_sha": "a" * 40,
        "request_sha256": common.sha256_value(request),
        "operation_sha256": request["operation_sha256"],
        "descriptor_sha256": request["descriptor_sha256"],
        "registration_sha256": common.sha256_value(protected["registration"]),
        "origin": {"run_id": "12345", "control_sha": "c" * 40, "artifact_id": "71",
                   "artifact_digest": "sha256:" + "d" * 64,
                   "intent_sha256": "e" * 64},
        "targets": {
            "klinik": {"organization_id": transport.klinik_org, "user_id": transport.klinik_user},
            "non_klinik": {"organization_id": transport.non_klinik_org,
                           "user_id": transport.non_klinik_user},
        },
        "expires_at": common.format_timestamp(
            dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=expires_in)),
    }
    if overrides:
        value.update(overrides)
    return value, request


class TestProductInverse(unittest.TestCase):
    def setUp(self):
        self.request, self.protected = controls.inputs()
        self.transport = FakeTransport(self.protected)

    def controller(self, transport=None):
        return inverse.ProductInverse(self.request, self.protected,
                                      transport or self.transport)

    def test_plan_selects_only_the_two_reserved_tenants(self):
        plan = self.controller().plan()
        self.assertEqual(plan["kind"], "crm-canary-fixture-inverse-plan")
        self.assertEqual(plan["organization_count"], 4)
        self.assertEqual(plan["reserved_organization_count"], 2)
        self.assertEqual(plan["issued_login_count"], 2)
        self.assertEqual(plan["targets"]["klinik"],
                         {"organization_id": self.transport.klinik_org,
                          "user_id": self.transport.klinik_user})
        self.assertEqual(plan["targets"]["non_klinik"],
                         {"organization_id": self.transport.non_klinik_org,
                          "user_id": self.transport.non_klinik_user})
        rendered = str(plan) + str(self.transport.calls)
        for role in inverse.ROLES:
            self.assertNotIn(self.registration_email(role), rendered)

    def registration_email(self, role):
        return self.protected["registration"][role + "_email"]

    def test_apply_retires_users_before_organizations_and_verifies(self):
        authority, _ = authority_for(self.protected, self.transport)
        result = self.controller().apply(authority)
        self.assertEqual([path for path, _ in self.transport.deletes], [
            "/api/users/" + self.transport.klinik_user,
            "/api/organizations/" + self.transport.klinik_org,
            "/api/users/" + self.transport.non_klinik_user,
            "/api/organizations/" + self.transport.non_klinik_org,
        ])
        self.assertEqual([org for _, org in self.transport.deletes], [
            self.transport.klinik_org, self.transport.home,
            self.transport.non_klinik_org, self.transport.home,
        ])
        self.assertEqual(result["state"], "inverse_verified")
        self.assertFalse(result["reserved_organization_present"])
        self.assertFalse(result["issued_login_present"])
        self.assertEqual([stage["action"] for stage in result["stages"]],
                         ["deleted"] * 4)
        self.assertEqual([stage["stage"] for stage in result["stages"]],
                         list(inverse.STAGES))
        for key, value in self.protected["credentials"].items():
            self.assertNotIn(value["password"] if key == "super_admin_login" else value,
                             str(result))

    def test_second_reviewed_run_is_an_idempotent_no_op(self):
        authority, _ = authority_for(self.protected, self.transport)
        self.controller().apply(authority)
        attempts = len(self.transport.deletes)
        result = self.controller().apply(authority)
        self.assertEqual(result["state"], "inverse_already_absent")
        self.assertEqual(len(self.transport.deletes), attempts)
        self.assertEqual([stage["action"] for stage in result["stages"]],
                         ["already_absent"] * 4)
        self.assertFalse(result["reserved_organization_present"])

    def test_apply_refuses_an_identity_that_differs_from_the_authority(self):
        authority, _ = authority_for(self.protected, self.transport)
        authority["targets"]["klinik"]["user_id"] = uid(999)
        with self.assertRaises(common.ReleaseError) as error:
            self.controller().apply(authority)
        self.assertIn("differs from the reviewed authority", str(error.exception))
        self.assertEqual(self.transport.deletes, [])

    def test_ambiguous_deletion_stops_without_repeating_anything(self):
        transport = FakeTransport(self.protected, fail_delete_at=2)
        authority, _ = authority_for(self.protected, transport)
        controller = self.controller(transport)
        with self.assertRaises(inverse.AmbiguousInverse) as error:
            controller.apply(authority)
        self.assertIn("status 500", str(error.exception))
        self.assertEqual(len(transport.deletes), 2)
        self.assertEqual([stage["action"] for stage in controller.stages],
                         ["deleted", "ambiguous"])
        with self.assertRaises(common.ReleaseError) as second:
            controller.apply(authority)
        self.assertIn("already used", str(second.exception))
        self.assertEqual(len(transport.deletes), 2)

    def test_authority_binding_window_and_distinctness(self):
        authority, request = authority_for(self.protected, self.transport)
        stub = lambda *args, **kwargs: {"run_id": "12345", "control_sha": "c" * 40,
                                        "artifact_id": "71",
                                        "artifact_digest": "sha256:" + "d" * 64,
                                        "intent_sha256": "e" * 64, "burn_count": 12}
        with mock.patch.object(inverse, "verify_origin", stub), \
                mock.patch.dict("os.environ", {"CONTROL_SHA": "a" * 40}):
            checked = inverse.validate_authority(copy.deepcopy(authority), None, None,
                                                 request, self.protected)
            self.assertEqual(checked["targets"], authority["targets"])
            for mutate in (
                lambda a: a.update({"request_sha256": "f" * 64}),
                lambda a: a.update({"descriptor_sha256": "f" * 64}),
                lambda a: a.update({"registration_sha256": "f" * 64}),
                lambda a: a["targets"]["klinik"].update(
                    {"organization_id": a["targets"]["non_klinik"]["organization_id"]}),
                lambda a: a["targets"]["klinik"].update({"user_id": "not-a-uuid"}),
            ):
                broken = copy.deepcopy(authority)
                mutate(broken)
                with self.assertRaises(common.ReleaseError):
                    inverse.validate_authority(broken, None, None, request, self.protected)
            expired, _ = authority_for(self.protected, self.transport, expires_in=-60)
            with self.assertRaises(common.ReleaseError) as error:
                inverse.validate_authority(expired, None, None, request, self.protected)
            self.assertIn("window differs", str(error.exception))
            stale, _ = authority_for(self.protected, self.transport, expires_in=3 * 24 * 3600)
            with self.assertRaises(common.ReleaseError):
                inverse.validate_authority(stale, None, None, request, self.protected)


class TestInverseTransportCapability(unittest.TestCase):
    def test_delete_is_limited_to_the_two_reviewed_routes(self):
        transport = fixture.ProductHTTP("SyntheticMetaToken123")
        for path in ("/api/users/not-a-uuid", "/api/organizations",
                     "/api/users/../../etc", "/api/accounts/" + uid(5),
                     "/api/users/8da8b3e1-1111-4111-8111-111111111111/../x"):
            with self.assertRaises(common.ReleaseError) as error:
                transport.delete(path, session="unknown")
            self.assertIn("inverse delete route differs", str(error.exception))
        with self.assertRaises(common.ReleaseError) as error:
            transport.delete("/api/users/" + uid(5), session="unknown")
        self.assertIn("unknown product session", str(error.exception))

    def test_forward_sequence_still_cannot_issue_delete(self):
        transport = fixture.ProductHTTP("SyntheticMetaToken123")
        with self.assertRaises(common.ReleaseError) as error:
            transport.request("DELETE", "/api/users/" + uid(5))
        self.assertIn("product method differs", str(error.exception))


if __name__ == "__main__":
    unittest.main()
