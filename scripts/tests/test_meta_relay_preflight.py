from __future__ import annotations

import datetime
import hashlib
import hmac
import io
import json
import os
import pathlib
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import meta_relay_preflight as preflight

MESSENGER_APP_ID = "100000000000001"
INSTAGRAM_APP_ID = "100000000000002"
ACCOUNT_ID = "d80c8035-364d-4b39-95d0-a2313d194cc3"
ORGANIZATION_ID = "4b3c81b0-264d-4f15-ad87-9f65f3a5f112"
META_BUSINESS_ID = "987654321098765"
MESSENGER_OWNER_BUSINESS_ID = "111122223333444"
INSTAGRAM_OWNER_BUSINESS_ID = "555566667777888"
MESSENGER_ASSET_ID = "700000000000001"
REVIEWER = "Operations Reviewer <reviewer@example.com>"
REVIEWED_AT = (
    datetime.datetime.now(datetime.timezone.utc)
    .replace(microsecond=0)
    .isoformat()
    .replace("+00:00", "Z")
)
EVIDENCE = "https://evidence.example.com/meta/review-2026-08-06"


def expected_account(**updates):
    account = {
        "key": "example-organization-messenger",
        "organization_id": ORGANIZATION_ID,
        "organization_name": "Example organization",
        "meta_business_id": META_BUSINESS_ID,
        "channel": "messenger",
        "external_account_id": MESSENGER_ASSET_ID,
        "rereply_account_id": ACCOUNT_ID,
        "access_token_env": "META_EXAMPLE_ORGANIZATION_MESSENGER_PAGE_TOKEN",
        "rereply_inbound_secret_env": "REREPLY_EXAMPLE_ORGANIZATION_MESSENGER_INBOUND_SECRET",
        "rereply_outbound_secret_env": "REREPLY_EXAMPLE_ORGANIZATION_MESSENGER_OUTBOUND_SECRET",
    }
    account.update(updates)
    return account


def deployed_account(**updates):
    account = expected_account()
    account.pop("organization_name")
    account["rereply_webhook_url"] = (
        "https://app.rereply.app/api/webhooks/channels/"
        + account.pop("rereply_account_id")
    )
    account.update(updates)
    return account


def deployed_environment(accounts=None, **updates):
    inventory = expected_inventory()
    account_mappings = accounts or [deployed_account()]
    environment = {
        preflight.MAPPING_ENV: json.dumps(account_mappings),
        preflight.RELAY_REREPLY_BASE_ENV: "https://app.rereply.app",
        preflight.RELAY_PROVIDER_PROOF_SECRET_ENV: "EV[opaque-relay-provider-proof]",
    }
    for env_name in preflight.RELAY_FIXED_SECRET_ENVS:
        environment[env_name] = f"EV[opaque-fixed-{env_name.lower()}]"
    for index, account in enumerate(account_mappings):
        for field in preflight.ACCOUNT_SECRET_REFERENCE_FIELDS:
            environment[account[field]] = f"EV[opaque-account-{index}-{field}]"
    for app_name, bindings in preflight.APP_ENVIRONMENT_BINDINGS.items():
        expected_app = inventory[app_name]
        flattened = {
            "app_id": expected_app["app_id"],
            "app_mode": expected_app["app_mode"],
            "owner_business_id": expected_app["owner_business_id"],
            "app_review_status": expected_app["app_review_status"],
            "app_review_permissions": ",".join(
                reversed(expected_app["app_review_permissions"])
            ),
            "tech_provider_status": expected_app["tech_provider_status"],
            **expected_app["review"],
        }
        for field, env_name in bindings.items():
            environment[env_name] = flattened[field]
    environment.update(updates)
    return environment


def rereply_runtime_environment(inventory=None, **updates):
    environment = {
        preflight.REREPLY_RELAY_BASE_ENV: "https://app.rereply.app/meta-relay",
        preflight.REREPLY_EXPECTED_ACCOUNTS_ENV: json.dumps(
            inventory or expected_inventory()
        ),
        preflight.REREPLY_PROVIDER_PROOF_SECRET_ENV: "EV[opaque-web-provider-proof]",
    }
    environment.update(updates)
    return environment


def expected_app(app_id, owner_business_id):
    messenger = app_id == MESSENGER_APP_ID
    return {
        "app_id": app_id,
        "app_mode": "live",
        "owner_business_id": owner_business_id,
        "app_review_status": "approved",
        "app_review_permissions": (
            [
                "pages_messaging",
                "pages_manage_metadata",
                "pages_show_list",
                "pages_read_engagement",
                "business_management",
            ]
            if messenger
            else [
                "instagram_business_basic",
                "instagram_business_manage_messages",
            ]
        ),
        "tech_provider_status": "verified",
        "review": {
            "reviewer": REVIEWER,
            "reviewed_at": REVIEWED_AT,
            "evidence": EVIDENCE,
        },
    }


def app_spec(environment=None, runtime_environment=None):
    env = environment or deployed_environment()
    runtime_env = runtime_environment or rereply_runtime_environment()
    return {
        "services": [
            {
                "name": "meta-relay",
                "envs": [
                    {
                        "key": key,
                        "value": value,
                        "type": (
                            "SECRET"
                            if value.startswith("EV[opaque-")
                            else "GENERAL"
                        ),
                    }
                    for key, value in env.items()
                ],
            },
            {
                "name": "omnitech-web",
                "envs": [
                    {
                        "key": key,
                        "value": value,
                        "type": (
                            "SECRET"
                            if value.startswith("EV[opaque-")
                            else "GENERAL"
                        ),
                    }
                    for key, value in runtime_env.items()
                ],
            },
        ]
    }


def expected_inventory(accounts=None):
    return {
        "messenger_app": expected_app(MESSENGER_APP_ID, MESSENGER_OWNER_BUSINESS_ID),
        "instagram_app": expected_app(INSTAGRAM_APP_ID, INSTAGRAM_OWNER_BUSINESS_ID),
        "accounts": accounts or [expected_account()],
    }


def readiness_headers(**updates):
    headers = {
        preflight.READINESS_HEADER: "v2",
        preflight.CHANNEL_HEADER: "messenger",
        preflight.EXTERNAL_ACCOUNT_HEADER: MESSENGER_ASSET_ID,
        preflight.CHANNEL_ACCOUNT_HEADER: ACCOUNT_ID,
        preflight.ORGANIZATION_HEADER: ORGANIZATION_ID,
        preflight.META_BUSINESS_HEADER: META_BUSINESS_ID,
        preflight.PROVIDER_PROOF_HEADER: "sha256=" + ("a" * 64),
        preflight.PROVIDER_PROOF_KEY_ID_HEADER: "sha256=" + ("b" * 64),
    }
    headers.update(updates)
    return headers


class FakeResponse:
    def __init__(self, status, headers=None):
        self.status = status
        self.headers = headers or {}
        self.closed = False

    def close(self):
        self.closed = True


class FakeOpener:
    def __init__(self, statuses):
        self.statuses = list(statuses)
        self.requests = []

    def open(self, request, timeout):
        self.requests.append((request, timeout))
        response = self.statuses.pop(0)
        if isinstance(response, int):
            headers = readiness_headers() if response == 204 else {}
            response = FakeResponse(response, headers)
        return response


class MetaRelayPreflightTests(unittest.TestCase):
    def normalized_expected(self, accounts=None):
        _, normalized = preflight.normalize_expected_inventory(
            expected_inventory(accounts)
        )
        return normalized

    def normalized_actual(self, accounts=None):
        return preflight.normalize_accounts(
            accounts or [deployed_account()], expected=False
        )

    def test_validate_only_cli_checks_complete_two_source_inventory(self):
        standard_input = io.StringIO(json.dumps(app_spec()))
        environment = {
            preflight.EXPECTED_ENV: json.dumps(expected_inventory()),
        }
        with (
            mock.patch.dict(os.environ, environment, clear=False),
            mock.patch("sys.stdin", standard_input),
            mock.patch("sys.stdout", new_callable=io.StringIO) as output,
        ):
            result = preflight.main(["--validate-only"])

        self.assertEqual(result, 0)
        self.assertIn("Meta relay preflight passed", output.getvalue())

    def test_validate_only_cli_rejects_missing_referenced_runtime_secret(self):
        spec = app_spec()
        env_name = expected_account()["access_token_env"]
        relay = spec["services"][0]
        relay["envs"] = [
            entry for entry in relay["envs"] if entry["key"] != env_name
        ]
        environment = {
            preflight.EXPECTED_ENV: json.dumps(expected_inventory()),
        }
        with (
            mock.patch.dict(os.environ, environment, clear=False),
            mock.patch("sys.stdin", io.StringIO(json.dumps(spec))),
            mock.patch("sys.stderr", new_callable=io.StringIO) as error_output,
        ):
            result = preflight.main(["--validate-only"])

        self.assertEqual(result, 1)
        self.assertIn(env_name, error_output.getvalue())
        self.assertNotIn("opaque-", error_output.getvalue())

    def test_matching_inventory_and_app_bindings_pass(self):
        spec = app_spec()
        environment = preflight.deployed_environment(spec, "meta-relay")
        actual = preflight.normalize_accounts(
            preflight.deployed_accounts(environment, "meta-relay"), expected=False
        )
        apps, expected = preflight.normalize_expected_inventory(expected_inventory())

        self.assertEqual(preflight.compare_app_bindings(environment, apps), [])
        self.assertEqual(
            preflight.provider_proof_secret_presence_errors(
                spec, "meta-relay", "omnitech-web"
            ),
            [],
        )
        self.assertEqual(
            preflight.compare_runtime_trust_bindings(
                spec,
                environment,
                expected_inventory(),
                "omnitech-web",
                "https://app.rereply.app",
                "https://app.rereply.app/meta-relay",
            ),
            [],
        )
        self.assertEqual(
            preflight.compare_inventory(actual, expected, "https://app.rereply.app"), []
        )

    def test_provider_proof_secrets_are_required_as_secret_entries_without_comparison(self):
        spec = app_spec()
        self.assertEqual(
            preflight.provider_proof_secret_presence_errors(
                spec, "meta-relay", "omnitech-web"
            ),
            [],
        )

        for service_name, env_name in (
            ("meta-relay", preflight.RELAY_PROVIDER_PROOF_SECRET_ENV),
            ("omnitech-web", preflight.REREPLY_PROVIDER_PROOF_SECRET_ENV),
        ):
            with self.subTest(service=service_name, env=env_name):
                missing = app_spec()
                service = next(
                    item
                    for item in missing["services"]
                    if item["name"] == service_name
                )
                service["envs"] = [
                    entry for entry in service["envs"] if entry["key"] != env_name
                ]

                errors = preflight.provider_proof_secret_presence_errors(
                    missing, "meta-relay", "omnitech-web"
                )

                self.assertEqual(len(errors), 1)
                self.assertIn(env_name, errors[0])
                self.assertNotIn("opaque-", errors[0])

        general = app_spec()
        relay_secret = next(
            entry
            for entry in general["services"][0]["envs"]
            if entry["key"] == preflight.RELAY_PROVIDER_PROOF_SECRET_ENV
        )
        relay_secret["type"] = "GENERAL"
        errors = preflight.provider_proof_secret_presence_errors(
            general, "meta-relay", "omnitech-web"
        )
        self.assertEqual(len(errors), 1)
        self.assertIn("type SECRET", errors[0])
        self.assertNotIn(relay_secret["value"], errors[0])

    def test_relay_fixed_and_account_referenced_secrets_are_structurally_required(self):
        actual = self.normalized_actual()
        required = set(preflight.RELAY_FIXED_SECRET_ENVS)
        required.update(
            actual[0][field]
            for field in preflight.ACCOUNT_SECRET_REFERENCE_FIELDS
        )

        for env_name in sorted(required):
            with self.subTest(missing=env_name):
                spec = app_spec()
                relay = spec["services"][0]
                relay["envs"] = [
                    entry for entry in relay["envs"] if entry["key"] != env_name
                ]

                errors = preflight.relay_runtime_secret_presence_errors(
                    spec, "meta-relay", actual
                )

                self.assertEqual(len(errors), 1)
                self.assertIn(env_name, errors[0])
                self.assertNotIn("opaque-", errors[0])

        referenced_env = actual[0]["access_token_env"]
        for mutation, expected_error in (
            (lambda entry: entry.update(type="GENERAL"), "type SECRET"),
            (lambda entry: entry.update(value=""), "no configured value"),
        ):
            with self.subTest(expected_error=expected_error):
                spec = app_spec()
                entry = next(
                    item
                    for item in spec["services"][0]["envs"]
                    if item["key"] == referenced_env
                )
                opaque_value = entry["value"]
                mutation(entry)

                errors = preflight.relay_runtime_secret_presence_errors(
                    spec, "meta-relay", actual
                )

                self.assertEqual(len(errors), 1)
                self.assertIn(expected_error, errors[0])
                self.assertNotIn(opaque_value, errors[0])

        duplicate = app_spec()
        duplicate_relay = duplicate["services"][0]
        entry = next(
            item
            for item in duplicate_relay["envs"]
            if item["key"] == referenced_env
        )
        duplicate_relay["envs"].append(dict(entry))
        errors = preflight.relay_runtime_secret_presence_errors(
            duplicate, "meta-relay", actual
        )
        self.assertEqual(len(errors), 1)
        self.assertIn("exactly once", errors[0])
        self.assertNotIn(entry["value"], errors[0])

    def test_runtime_trust_copies_and_production_bases_must_match(self):
        wrong_inventory = expected_inventory()
        wrong_inventory["accounts"][0]["meta_business_id"] = "999999999999999"
        spec = app_spec(
            environment=deployed_environment(
                **{
                    preflight.RELAY_REREPLY_BASE_ENV: "https://wrong.example.com"
                }
            ),
            runtime_environment=rereply_runtime_environment(
                wrong_inventory,
                **{
                    preflight.REREPLY_RELAY_BASE_ENV: "https://wrong.example.com/meta-relay"
                },
            ),
        )
        relay_environment = preflight.deployed_environment(spec, "meta-relay")

        errors = preflight.compare_runtime_trust_bindings(
            spec,
            relay_environment,
            expected_inventory(),
            "omnitech-web",
            "https://app.rereply.app",
            "https://app.rereply.app/meta-relay",
        )

        self.assertEqual(len(errors), 3)
        self.assertTrue(any(preflight.RELAY_REREPLY_BASE_ENV in error for error in errors))
        self.assertTrue(any(preflight.REREPLY_RELAY_BASE_ENV in error for error in errors))
        self.assertTrue(
            any(preflight.REREPLY_EXPECTED_ACCOUNTS_ENV in error for error in errors)
        )

    def test_wrong_target_and_rereply_account_are_reported(self):
        actual = self.normalized_actual(
            [
                deployed_account(
                    external_account_id="700000000000099",
                    rereply_webhook_url=(
                        "https://app.rereply.app/api/webhooks/channels/"
                        "e85e7e69-8d94-40c0-9721-939ff5cc63f5"
                    ),
                )
            ]
        )

        errors = preflight.compare_inventory(
            actual, self.normalized_expected(), "https://app.rereply.app"
        )

        self.assertTrue(any("external_account_id" in error for error in errors))
        self.assertTrue(
            any("unexpected ReReply channel account" in error for error in errors)
        )

    def test_wrong_organization_and_asset_owner_business_are_reported(self):
        actual = self.normalized_actual(
            [
                deployed_account(
                    organization_id="97cc5e17-8c7b-4608-9fb8-80c472257d4d",
                    meta_business_id="999999999999999",
                )
            ]
        )

        errors = preflight.compare_inventory(
            actual, self.normalized_expected(), "https://app.rereply.app"
        )

        self.assertTrue(any("organization_id" in error for error in errors))
        self.assertTrue(any("meta_business_id" in error for error in errors))

    def test_account_authority_ids_require_canonical_uuid_and_numeric_business(self):
        for updates, expected_error in (
            ({"organization_id": ORGANIZATION_ID.upper()}, "canonical non-zero UUID"),
            ({"rereply_account_id": ACCOUNT_ID.upper()}, "canonical non-zero UUID"),
            (
                {"organization_id": "00000000-0000-0000-0000-000000000000"},
                "canonical non-zero UUID",
            ),
            ({"meta_business_id": "business-name"}, "numeric Meta Business ID"),
        ):
            with self.subTest(updates=updates):
                with self.assertRaises(preflight.PreflightError) as raised:
                    self.normalized_expected([expected_account(**updates)])

                self.assertIn(expected_error, str(raised.exception))

    def test_deployed_mapping_rejects_display_only_organization_name(self):
        with self.assertRaises(preflight.PreflightError) as raised:
            self.normalized_actual(
                [deployed_account(organization_name="must-not-be-deployed")]
            )

        self.assertIn("unknown field", str(raised.exception))

    def test_one_organization_cannot_bind_multiple_meta_businesses(self):
        second = expected_account(
            key="example-organization-instagram",
            channel="instagram",
            external_account_id="800000000000001",
            instagram_api_mode="instagram_login",
            rereply_account_id="89745148-713b-46fc-9cc2-8ddee39b5e1e",
            meta_business_id="999999999999999",
            access_token_env="META_EXAMPLE_ORGANIZATION_INSTAGRAM_USER_TOKEN",
            rereply_inbound_secret_env="REREPLY_EXAMPLE_ORGANIZATION_INSTAGRAM_INBOUND_SECRET",
            rereply_outbound_secret_env="REREPLY_EXAMPLE_ORGANIZATION_INSTAGRAM_OUTBOUND_SECRET",
        )

        with self.assertRaises(preflight.PreflightError) as raised:
            self.normalized_expected([expected_account(), second])

        self.assertIn("multiple Meta Business IDs", str(raised.exception))

    def test_hmac_environment_names_are_unique_across_both_directions(self):
        first = expected_account()
        second = expected_account(
            key="second-organization-messenger",
            organization_id="97cc5e17-8c7b-4608-9fb8-80c472257d4d",
            organization_name="Second organization",
            external_account_id="700000000000002",
            rereply_account_id="b6f096b8-7c94-46e4-96d3-d2c6a8e46d7d",
            access_token_env="META_SECOND_ORGANIZATION_MESSENGER_PAGE_TOKEN",
            rereply_inbound_secret_env="REREPLY_SECOND_ORGANIZATION_MESSENGER_INBOUND_SECRET",
            rereply_outbound_secret_env=first["rereply_inbound_secret_env"],
        )

        with self.assertRaises(preflight.PreflightError) as raised:
            self.normalized_expected([first, second])

        self.assertIn("reuse HMAC environment variable", str(raised.exception))

    def test_app_review_and_tech_provider_status_are_separate_gates(self):
        inventory = expected_inventory()
        inventory["messenger_app"]["tech_provider_status"] = "unverified"

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.normalize_expected_inventory(inventory)

        self.assertIn("tech_provider_status", str(raised.exception))
        self.assertNotIn("app_review_status must be approved", str(raised.exception))

    def test_both_meta_apps_must_be_attested_live(self):
        for app_name in ("messenger_app", "instagram_app"):
            with self.subTest(app=app_name):
                inventory = expected_inventory()
                inventory[app_name]["app_mode"] = "development"

                with self.assertRaises(preflight.PreflightError) as raised:
                    preflight.normalize_expected_inventory(inventory)

                self.assertIn("app_mode must be live", str(raised.exception))

    def test_runtime_only_messenger_permissions_are_not_future_org_ready(self):
        inventory = expected_inventory()
        inventory["messenger_app"]["app_review_permissions"] = sorted(
            preflight.MESSENGER_RUNTIME_PERMISSIONS
        )

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.normalize_expected_inventory(inventory)

        message = str(raised.exception)
        self.assertIn("Advanced Access", message)
        for permission in (
            "pages_show_list",
            "pages_read_engagement",
            "business_management",
        ):
            self.assertIn(permission, message)

    def test_facebook_login_instagram_requires_its_advanced_access_scopes(self):
        facebook_login_account = expected_account(
            channel="instagram",
            external_account_id="800000000000001",
            instagram_api_mode="facebook_login",
        )
        inventory = expected_inventory([facebook_login_account])

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.normalize_expected_inventory(inventory)

        message = str(raised.exception)
        self.assertIn("instagram_basic", message)
        self.assertIn("instagram_manage_messages", message)

    def test_review_record_requires_reviewer_time_and_evidence_formats(self):
        invalid_records = (
            ("reviewer", "anonymous", "Name <email>"),
            ("reviewed_at", "06/08/2026 12:30", "RFC3339 UTC"),
            ("evidence", "http://example.com/evidence", "HTTPS URL"),
        )
        for field, value, expected_error in invalid_records:
            with self.subTest(field=field):
                inventory = expected_inventory()
                inventory["messenger_app"]["review"][field] = value

                with self.assertRaises(preflight.PreflightError) as raised:
                    preflight.normalize_expected_inventory(inventory)

                self.assertIn(expected_error, str(raised.exception))

    def test_review_record_rejects_future_and_expired_governance(self):
        now = preflight.utc_now()
        cases = (
            (now + datetime.timedelta(minutes=6), "in the future"),
            (now - datetime.timedelta(days=91), "older than 90 days"),
        )
        for reviewed_at, expected_error in cases:
            with self.subTest(expected_error=expected_error):
                inventory = expected_inventory()
                inventory["messenger_app"]["review"]["reviewed_at"] = (
                    reviewed_at.replace(microsecond=0)
                    .isoformat()
                    .replace("+00:00", "Z")
                )

                with self.assertRaises(preflight.PreflightError) as raised:
                    preflight.normalize_expected_inventory(inventory)

                self.assertIn(expected_error, str(raised.exception))

    def test_deployed_owner_tech_status_and_review_record_must_match_exactly(self):
        bindings = preflight.APP_ENVIRONMENT_BINDINGS["messenger_app"]
        environment = deployed_environment(
            **{
                bindings["owner_business_id"]: "999999999999999",
                bindings["tech_provider_status"]: "unverified",
                bindings["reviewer"]: "Another Reviewer <other@example.com>",
                bindings["reviewed_at"]: "2026-08-06T04:31:00Z",
                bindings["evidence"]: "https://evidence.example.com/wrong-record",
            }
        )
        apps, _ = preflight.normalize_expected_inventory(expected_inventory())

        errors = preflight.compare_app_bindings(environment, apps)

        self.assertEqual(len(errors), 5)
        for field in (
            "owner_business_id",
            "tech_provider_status",
            "reviewer",
            "reviewed_at",
            "evidence",
        ):
            self.assertTrue(any(bindings[field] in error for error in errors))

    def test_expected_app_permissions_reject_duplicates_and_invalid_names(self):
        for permissions, expected_error in (
            (["pages_messaging", "pages_messaging"], "duplicate permission"),
            (["Pages_Messaging"], "lowercase Meta permission name"),
        ):
            with self.subTest(permissions=permissions):
                inventory = expected_inventory()
                inventory["messenger_app"]["app_review_permissions"] = permissions

                with self.assertRaises(preflight.PreflightError) as raised:
                    preflight.normalize_expected_inventory(inventory)

                self.assertIn(expected_error, str(raised.exception))

    def test_deployed_permissions_fail_on_missing_extra_duplicate_or_invalid(self):
        permission_env = preflight.APP_ENVIRONMENT_BINDINGS["messenger_app"][
            "app_review_permissions"
        ]
        cases = (
            ("pages_messaging", "unexpected or missing"),
            (
                "pages_messaging,pages_manage_metadata,ads_management",
                "unexpected or missing",
            ),
            (
                "pages_messaging,pages_manage_metadata,pages_messaging",
                "duplicate permission",
            ),
            ("Pages_Messaging,pages_manage_metadata", "lowercase Meta permission"),
        )
        apps, _ = preflight.normalize_expected_inventory(expected_inventory())
        for deployed_permissions, expected_error in cases:
            with self.subTest(deployed_permissions=deployed_permissions):
                environment = deployed_environment(
                    **{permission_env: deployed_permissions}
                )

                errors = preflight.compare_app_bindings(environment, apps)

                self.assertEqual(len(errors), 1)
                self.assertIn(expected_error, errors[0])

    def test_missing_app_id_and_unapproved_review_state_fail_independently(self):
        messenger_bindings = preflight.APP_ENVIRONMENT_BINDINGS["messenger_app"]
        instagram_bindings = preflight.APP_ENVIRONMENT_BINDINGS["instagram_app"]
        environment = deployed_environment(
            **{
                messenger_bindings["app_id"]: "",
                instagram_bindings["app_review_status"]: "pending",
            }
        )
        apps, _ = preflight.normalize_expected_inventory(expected_inventory())

        errors = preflight.compare_app_bindings(environment, apps)

        self.assertEqual(len(errors), 2)
        self.assertTrue(any(messenger_bindings["app_id"] in error for error in errors))
        self.assertTrue(
            any(instagram_bindings["app_review_status"] in error for error in errors)
        )

    def test_probe_uses_signed_head_and_never_calls_meta_directly(self):
        secret = "independent-account-secret-32-bytes"
        opener = FakeOpener([204])
        expected = self.normalized_expected()

        errors = preflight.probe_accounts(
            expected,
            {expected[0]["rereply_outbound_secret_env"]: secret},
            "https://app.rereply.app/meta-relay",
            attempts=1,
            retry_delay=0,
            opener=opener,
        )

        self.assertEqual(errors, [])
        request, timeout = opener.requests[0]
        self.assertEqual(request.get_method(), "HEAD")
        self.assertEqual(timeout, 10)
        self.assertEqual(
            request.full_url,
            f"https://app.rereply.app/meta-relay/v1/accounts/messenger/{MESSENGER_ASSET_ID}",
        )
        self.assertNotIn("facebook.com", request.full_url)
        self.assertNotIn("instagram.com", request.full_url)
        headers = {key.lower(): value for key, value in request.header_items()}
        wanted = "sha256=" + hmac.new(secret.encode(), b"", hashlib.sha256).hexdigest()
        self.assertEqual(headers[preflight.SIGNATURE_HEADER.lower()], wanted)

    def test_rereply_readiness_returns_provider_proof_key_id_for_comparison(self):
        key_id = "sha256=" + ("c" * 64)
        opener = FakeOpener(
            [FakeResponse(200, {preflight.PROVIDER_PROOF_KEY_ID_HEADER: key_id})]
        )

        actual, errors = preflight.probe_rereply_provider_proof_key_id(
            "https://app.rereply.app",
            attempts=1,
            retry_delay=0,
            opener=opener,
        )

        self.assertEqual(errors, [])
        self.assertEqual(actual, key_id)
        request, timeout = opener.requests[0]
        self.assertEqual(request.get_method(), "GET")
        self.assertEqual(request.full_url, "https://app.rereply.app/ready")
        self.assertEqual(timeout, 10)

    def test_account_probe_rejects_provider_proof_key_mismatch(self):
        expected = self.normalized_expected()
        secret = "account-specific-secret-at-least-32-bytes"
        rereply_key_id = "sha256=" + ("c" * 64)
        relay_key_id = "sha256=" + ("d" * 64)

        errors = preflight.probe_accounts(
            expected,
            {expected[0]["rereply_outbound_secret_env"]: secret},
            "https://app.rereply.app/meta-relay",
            attempts=1,
            retry_delay=0,
            opener=FakeOpener(
                [
                    FakeResponse(
                        204,
                        readiness_headers(
                            **{
                                preflight.PROVIDER_PROOF_KEY_ID_HEADER: relay_key_id
                            }
                        ),
                    )
                ]
            ),
            provider_proof_key_id=rereply_key_id,
        )

        self.assertEqual(len(errors), 1)
        self.assertIn("different provider-proof key", errors[0])
        self.assertNotIn(rereply_key_id, errors[0])
        self.assertNotIn(relay_key_id, errors[0])

    def test_rereply_key_id_probe_rejects_malformed_success(self):
        actual, errors = preflight.probe_rereply_provider_proof_key_id(
            "https://app.rereply.app",
            attempts=1,
            retry_delay=0,
            opener=FakeOpener(
                [
                    FakeResponse(
                        200,
                        {preflight.PROVIDER_PROOF_KEY_ID_HEADER: "not-a-key-id"},
                    )
                ]
            ),
        )

        self.assertEqual(actual, "")
        self.assertEqual(len(errors), 1)
        self.assertIn(preflight.PROVIDER_PROOF_KEY_ID_HEADER, errors[0])

    def test_probe_failure_does_not_expose_secret(self):
        secret = "never-print-this-account-secret-32-bytes"
        expected = self.normalized_expected()

        errors = preflight.probe_accounts(
            expected,
            {expected[0]["rereply_outbound_secret_env"]: secret},
            "https://app.rereply.app/meta-relay",
            attempts=1,
            retry_delay=0,
            opener=FakeOpener([503]),
        )

        self.assertEqual(len(errors), 1)
        self.assertIn("HTTP 503", errors[0])
        self.assertNotIn(secret, "\n".join(errors))

    def test_probe_rejects_204_with_each_missing_attestation_header(self):
        expected = self.normalized_expected()
        secret = "account-specific-secret-at-least-32-bytes"
        for missing_header in (
            preflight.READINESS_HEADER,
            preflight.CHANNEL_HEADER,
            preflight.EXTERNAL_ACCOUNT_HEADER,
            preflight.CHANNEL_ACCOUNT_HEADER,
            preflight.ORGANIZATION_HEADER,
            preflight.META_BUSINESS_HEADER,
            preflight.PROVIDER_PROOF_HEADER,
            preflight.PROVIDER_PROOF_KEY_ID_HEADER,
        ):
            with self.subTest(missing_header=missing_header):
                headers = readiness_headers()
                headers.pop(missing_header)

                errors = preflight.probe_accounts(
                    expected,
                    {expected[0]["rereply_outbound_secret_env"]: secret},
                    "https://app.rereply.app/meta-relay",
                    attempts=1,
                    retry_delay=0,
                    opener=FakeOpener([FakeResponse(204, headers)]),
                )

                self.assertEqual(len(errors), 1)
                self.assertIn(missing_header, errors[0])

    def test_probe_rejects_mismatched_attestation_values(self):
        expected = self.normalized_expected()
        secret = "account-specific-secret-at-least-32-bytes"
        headers = readiness_headers(
            **{
                preflight.READINESS_HEADER: "v0",
                preflight.CHANNEL_HEADER: "instagram",
                preflight.EXTERNAL_ACCOUNT_HEADER: "700000000000099",
                preflight.CHANNEL_ACCOUNT_HEADER: (
                    "e85e7e69-8d94-40c0-9721-939ff5cc63f5"
                ),
                preflight.ORGANIZATION_HEADER: ("97cc5e17-8c7b-4608-9fb8-80c472257d4d"),
                preflight.META_BUSINESS_HEADER: "999999999999999",
                preflight.PROVIDER_PROOF_HEADER: "sha256=not-a-valid-proof",
                preflight.PROVIDER_PROOF_KEY_ID_HEADER: "sha256=not-a-valid-key-id",
            }
        )

        errors = preflight.probe_accounts(
            expected,
            {expected[0]["rereply_outbound_secret_env"]: secret},
            "https://app.rereply.app/meta-relay",
            attempts=1,
            retry_delay=0,
            opener=FakeOpener([FakeResponse(204, headers)]),
        )

        self.assertEqual(len(errors), 8)
        self.assertEqual(
            {name for name in headers if any(name in error for error in errors)},
            set(headers),
        )

    def test_duplicate_account_signing_secret_is_rejected_without_value(self):
        second = expected_account(
            key="second-organization-messenger",
            organization_id="97cc5e17-8c7b-4608-9fb8-80c472257d4d",
            organization_name="Second organization",
            external_account_id="700000000000002",
            rereply_account_id="b6f096b8-7c94-46e4-96d3-d2c6a8e46d7d",
            access_token_env="META_SECOND_ORGANIZATION_MESSENGER_PAGE_TOKEN",
            rereply_inbound_secret_env="REREPLY_SECOND_ORGANIZATION_MESSENGER_INBOUND_SECRET",
            rereply_outbound_secret_env="REREPLY_SECOND_ORGANIZATION_MESSENGER_OUTBOUND_SECRET",
        )
        expected = self.normalized_expected([expected_account(), second])
        secret = "accidentally-shared-secret-at-least-32-bytes"
        secrets = {
            expected[0]["rereply_outbound_secret_env"]: secret,
            expected[1]["rereply_outbound_secret_env"]: secret,
        }

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.probe_accounts(
                expected,
                secrets,
                "https://app.rereply.app/meta-relay",
                attempts=1,
                retry_delay=0,
                opener=FakeOpener([]),
            )

        self.assertNotIn(secret, str(raised.exception))

    def test_probe_rejects_short_secret_without_exposing_it(self):
        secret = "short-secret"
        expected = self.normalized_expected()

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.probe_accounts(
                expected,
                {expected[0]["rereply_outbound_secret_env"]: secret},
                "https://app.rereply.app/meta-relay",
                attempts=1,
                retry_delay=0,
                opener=FakeOpener([]),
            )

        self.assertIn("at least 32 UTF-8 bytes", str(raised.exception))
        self.assertNotIn(secret, str(raised.exception))

    def test_probe_rejects_surrounding_secret_whitespace_without_exposing_it(self):
        secret = " account-specific-secret-at-least-32-bytes "
        expected = self.normalized_expected()

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.probe_accounts(
                expected,
                {expected[0]["rereply_outbound_secret_env"]: secret},
                "https://app.rereply.app/meta-relay",
                attempts=1,
                retry_delay=0,
                opener=FakeOpener([]),
            )

        self.assertIn("surrounding whitespace", str(raised.exception))
        self.assertNotIn(secret, str(raised.exception))

    def test_mapping_must_remain_inspectable_without_containing_tokens(self):
        environment = deployed_environment()
        environment[preflight.MAPPING_ENV] = "EV[encrypted-value]"

        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.deployed_accounts(environment, "meta-relay")

        self.assertIn("GENERAL", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
