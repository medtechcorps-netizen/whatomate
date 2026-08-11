#!/usr/bin/env python3
"""Fail-closed deployment audit for Meta relay account mappings.

The audit compares the deployed DigitalOcean app spec with a separately
maintained expected-account inventory, reads ReReply's non-secret readiness key
fingerprint, then calls the relay's signed HEAD health endpoint for every
expected account. It never receives a Meta access token and never calls a Meta
API directly.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import hmac
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable, Iterable
from typing import Any

SIGNATURE_HEADER = "X-ReReply-Signature-256"
READINESS_HEADER = "X-ReReply-Relay-Readiness"
CHANNEL_HEADER = "X-ReReply-Channel"
EXTERNAL_ACCOUNT_HEADER = "X-ReReply-External-Account-ID"
CHANNEL_ACCOUNT_HEADER = "X-ReReply-Channel-Account-ID"
ORGANIZATION_HEADER = "X-ReReply-Organization-ID"
META_BUSINESS_HEADER = "X-ReReply-Meta-Business-ID"
PROVIDER_PROOF_HEADER = "X-ReReply-Meta-Provider-Proof-256"
PROVIDER_PROOF_KEY_ID_HEADER = "X-ReReply-Meta-Provider-Proof-Key-ID"
MAPPING_ENV = "META_RELAY_ACCOUNTS_JSON"
EXPECTED_ENV = "META_RELAY_EXPECTED_ACCOUNTS_JSON"
SECRETS_ENV = "META_RELAY_PREFLIGHT_SECRETS_JSON"
RELAY_REREPLY_BASE_ENV = "META_RELAY_REREPLY_BASE_URL"
REREPLY_RELAY_BASE_ENV = "WHATOMATE_META_RELAY__BASE_URL"
REREPLY_EXPECTED_ACCOUNTS_ENV = "WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON"
RELAY_PROVIDER_PROOF_SECRET_ENV = "META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"
REREPLY_PROVIDER_PROOF_SECRET_ENV = "WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET"
REVIEW_RELAY_WEB_ENV_PREFIX = "WHATOMATE_META_MESSENGER_REVIEW_RELAY__"
REVIEW_RELAY_ENV_PREFIX = "META_RELAY_REVIEW_"
RELAY_RUNTIME_MODE_ENV = "META_RELAY_RUNTIME_MODE"
# Reject the abandoned alias too, so an older candidate spec cannot silently
# retain review behavior after the runtime moved to RELAY_RUNTIME_MODE_ENV.
RELAY_MODE_ENV = "META_RELAY_MODE"
RELAY_MODE_ENVS = frozenset({RELAY_RUNTIME_MODE_ENV, RELAY_MODE_ENV})
STAGING_MESSENGER_REVIEW_MODE = "staging_messenger_review"
RUNTIME_COMPONENT_COLLECTIONS = ("services", "workers", "jobs")
RELAY_FIXED_SECRET_ENVS = frozenset(
    {
        "META_RELAY_REDIS_URL",
        "META_RELAY_MESSENGER_APP_SECRET",
        "META_RELAY_MESSENGER_VERIFY_TOKEN",
        "META_RELAY_INSTAGRAM_APP_SECRET",
        "META_RELAY_INSTAGRAM_VERIFY_TOKEN",
    }
)
ACCOUNT_SECRET_REFERENCE_FIELDS = (
    "access_token_env",
    "rereply_inbound_secret_env",
    "rereply_outbound_secret_env",
)
KEY_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
ENV_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
META_ID_PATTERN = re.compile(r"^[1-9][0-9]{0,31}$")
META_APP_ID_PATTERN = re.compile(r"^[1-9][0-9]{5,31}$")
PERMISSION_PATTERN = re.compile(r"^[a-z][a-z0-9_]{0,127}$")
SIGNATURE_PATTERN = re.compile(r"^sha256=[0-9a-f]{64}$")
REVIEWER_PATTERN = re.compile(r"^[^<>\r\n]{2,100} <[^<>\s@]+@[^<>\s@]+\.[^<>\s@]+>$")
REVIEWED_AT_PATTERN = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$"
)
MAX_GOVERNANCE_REVIEW_AGE = datetime.timedelta(days=90)
GOVERNANCE_CLOCK_SKEW = datetime.timedelta(minutes=5)
MIN_HMAC_SECRET_BYTES = 32
MESSENGER_RUNTIME_PERMISSIONS = frozenset(
    {"pages_messaging", "pages_manage_metadata"}
)
MESSENGER_FUTURE_ORGANIZATION_ADVANCED_ACCESS_PERMISSIONS = (
    MESSENGER_RUNTIME_PERMISSIONS
    | frozenset(
        {
            "pages_show_list",
            "pages_read_engagement",
            "business_management",
        }
    )
)
MESSENGER_FACEBOOK_LOGIN_INSTAGRAM_PERMISSIONS = frozenset(
    {"instagram_basic", "instagram_manage_messages"}
)
APP_ENVIRONMENT_BINDINGS = {
    "messenger_app": {
        "app_id": "META_RELAY_MESSENGER_APP_ID",
        "app_mode": "META_RELAY_MESSENGER_APP_MODE",
        "owner_business_id": "META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID",
        "app_review_status": "META_RELAY_MESSENGER_APP_REVIEW_STATUS",
        "app_review_permissions": "META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS",
        "tech_provider_status": "META_RELAY_MESSENGER_TECH_PROVIDER_STATUS",
        "reviewer": "META_RELAY_MESSENGER_REVIEWED_BY",
        "reviewed_at": "META_RELAY_MESSENGER_REVIEWED_AT",
        "evidence": "META_RELAY_MESSENGER_REVIEW_EVIDENCE",
    },
    "instagram_app": {
        "app_id": "META_RELAY_INSTAGRAM_APP_ID",
        "app_mode": "META_RELAY_INSTAGRAM_APP_MODE",
        "owner_business_id": "META_RELAY_INSTAGRAM_APP_OWNER_BUSINESS_ID",
        "app_review_status": "META_RELAY_INSTAGRAM_APP_REVIEW_STATUS",
        "app_review_permissions": "META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS",
        "tech_provider_status": "META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS",
        "reviewer": "META_RELAY_INSTAGRAM_REVIEWED_BY",
        "reviewed_at": "META_RELAY_INSTAGRAM_REVIEWED_AT",
        "evidence": "META_RELAY_INSTAGRAM_REVIEW_EVIDENCE",
    },
}
EXPECTED_APP_FIELDS = {
    "app_id",
    "app_mode",
    "owner_business_id",
    "app_review_status",
    "app_review_permissions",
    "tech_provider_status",
    "review",
}
EXPECTED_REVIEW_FIELDS = {"reviewer", "reviewed_at", "evidence"}

ACTUAL_FIELDS = {
    "key",
    "organization_id",
    "meta_business_id",
    "channel",
    "external_account_id",
    "instagram_api_mode",
    "rereply_webhook_url",
    "access_token_env",
    "rereply_inbound_secret_env",
    "rereply_outbound_secret_env",
}
EXPECTED_FIELDS = {
    "key",
    "organization_id",
    "organization_name",
    "meta_business_id",
    "channel",
    "external_account_id",
    "instagram_api_mode",
    "rereply_account_id",
    "access_token_env",
    "rereply_inbound_secret_env",
    "rereply_outbound_secret_env",
}
EXPECTED_INVENTORY_FIELDS = {"messenger_app", "instagram_app", "accounts"}
COMPARE_FIELDS = (
    "organization_id",
    "meta_business_id",
    "channel",
    "external_account_id",
    "instagram_api_mode",
    "access_token_env",
    "rereply_inbound_secret_env",
    "rereply_outbound_secret_env",
)


class PreflightError(Exception):
    """Safe-to-display configuration error."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Keep an account-scoped signing secret on the configured relay origin."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def utc_now() -> datetime.datetime:
    return datetime.datetime.now(datetime.timezone.utc)


def parse_json(raw: str, label: str) -> Any:
    try:
        return json.loads(raw)
    except (TypeError, json.JSONDecodeError) as exc:
        raise PreflightError(f"{label} is not valid JSON") from exc


def load_app_spec(stream) -> dict[str, Any]:
    parsed = parse_json(stream.read(), "DigitalOcean app spec")
    if not isinstance(parsed, dict):
        raise PreflightError("DigitalOcean app spec must be a JSON object")
    return parsed


def load_json_environment(name: str, expected_type: type) -> Any:
    raw = os.environ.get(name, "")
    if not raw.strip():
        raise PreflightError(f"required environment variable {name} is empty")
    parsed = parse_json(raw, f"environment variable {name}")
    if not isinstance(parsed, expected_type):
        kind = "array" if expected_type is list else "object"
        raise PreflightError(f"environment variable {name} must contain a JSON {kind}")
    return parsed


def deployed_environment(spec: dict[str, Any], service_name: str) -> dict[str, str]:
    services = spec.get("services")
    if not isinstance(services, list):
        raise PreflightError("DigitalOcean app spec has no services array")
    matches = [
        item
        for item in services
        if isinstance(item, dict) and item.get("name") == service_name
    ]
    if len(matches) != 1:
        raise PreflightError(
            f"expected exactly one {service_name!r} service, found {len(matches)}"
        )

    envs = matches[0].get("envs")
    if not isinstance(envs, list):
        raise PreflightError(f"service {service_name!r} has no environment array")
    deployed: dict[str, str] = {}
    for index, item in enumerate(envs):
        if not isinstance(item, dict):
            raise PreflightError(
                f"service {service_name!r} environment entry {index} must be an object"
            )
        key = item.get("key")
        value = item.get("value")
        if not isinstance(key, str) or not key:
            raise PreflightError(
                f"service {service_name!r} environment entry {index} has no key"
            )
        if key in deployed:
            raise PreflightError(
                f"service {service_name!r} defines environment variable {key} more than once"
            )
        deployed[key] = value if isinstance(value, str) else ""
    return deployed


def production_review_runtime_errors(spec: dict[str, Any]) -> list[str]:
    """Reject staging-only Messenger review wiring in production specs.

    Review configuration is forbidden by key presence, not by its value. This
    prevents an apparently inert ``ENABLED=false`` declaration from carrying
    staging-only broker or credential-provisioning wiring into production.
    The normal Meta App Review governance variables do not use either reserved
    prefix and remain valid production evidence.
    """

    errors: list[str] = []
    for collection_name in RUNTIME_COMPONENT_COLLECTIONS:
        components = spec.get(collection_name)
        if not isinstance(components, list):
            continue
        for component_index, component in enumerate(components):
            if not isinstance(component, dict):
                continue
            component_name = component.get("name")
            if not isinstance(component_name, str) or not component_name:
                component_name = f"{collection_name}[{component_index}]"
            envs = component.get("envs")
            if not isinstance(envs, list):
                continue
            for item in envs:
                if not isinstance(item, dict):
                    continue
                key = item.get("key")
                if not isinstance(key, str):
                    continue
                if key.startswith(REVIEW_RELAY_WEB_ENV_PREFIX) or key.startswith(
                    REVIEW_RELAY_ENV_PREFIX
                ):
                    errors.append(
                        f"production {collection_name[:-1]} {component_name!r} "
                        f"must not declare staging-only Messenger review environment variable {key}"
                    )
                    continue
                value = item.get("value")
                if (
                    key in RELAY_MODE_ENVS
                    and isinstance(value, str)
                    and value.strip().lower() == STAGING_MESSENGER_REVIEW_MODE
                ):
                    errors.append(
                        f"production {collection_name[:-1]} {component_name!r} "
                        f"must not use {key}={STAGING_MESSENGER_REVIEW_MODE}"
                    )
    return errors


def service_secret_presence_errors(
    spec: dict[str, Any], service_name: str, env_names: Iterable[str]
) -> list[str]:
    """Require opaque runtime secret declarations without exposing their values."""

    services = spec.get("services")
    if not isinstance(services, list):
        return ["DigitalOcean app spec has no services array"]

    service_matches = [
        item
        for item in services
        if isinstance(item, dict) and item.get("name") == service_name
    ]
    if len(service_matches) != 1:
        return [f"cannot verify secrets: expected exactly one {service_name!r} service"]

    envs = service_matches[0].get("envs")
    if not isinstance(envs, list):
        return [f"service {service_name!r} has no environment array"]

    errors: list[str] = []
    for env_name in sorted(set(env_names)):
        matches = [
            item
            for item in envs
            if isinstance(item, dict) and item.get("key") == env_name
        ]
        if len(matches) != 1:
            errors.append(
                f"service {service_name!r} must declare {env_name} exactly once as a SECRET environment variable"
            )
            continue
        entry = matches[0]
        if entry.get("type") != "SECRET":
            errors.append(
                f"service {service_name!r} must declare {env_name} with type SECRET"
            )
        if not isinstance(entry.get("value"), str) or not entry["value"].strip():
            errors.append(
                f"service {service_name!r} has no configured value for secret {env_name}"
            )
    return errors


def provider_proof_secret_presence_errors(
    spec: dict[str, Any], relay_service: str, rereply_service: str
) -> list[str]:
    """Require both provider-proof declarations without comparing secret bytes."""

    errors = service_secret_presence_errors(
        spec, relay_service, (RELAY_PROVIDER_PROOF_SECRET_ENV,)
    )
    errors.extend(
        service_secret_presence_errors(
            spec, rereply_service, (REREPLY_PROVIDER_PROOF_SECRET_ENV,)
        )
    )
    return errors


def relay_runtime_secret_presence_errors(
    spec: dict[str, Any], relay_service: str, accounts: Iterable[dict[str, str]]
) -> list[str]:
    """Require fixed and account-referenced relay secrets before app mutation."""

    env_names = set(RELAY_FIXED_SECRET_ENVS)
    for account in accounts:
        env_names.update(account[field] for field in ACCOUNT_SECRET_REFERENCE_FIELDS)
    return service_secret_presence_errors(spec, relay_service, env_names)


def deployed_accounts(
    environment: dict[str, str], service_name: str
) -> list[dict[str, Any]]:
    raw = environment.get(MAPPING_ENV, "")
    if not isinstance(raw, str) or not raw.strip():
        raise PreflightError(f"service {service_name!r} has an empty {MAPPING_ENV}")
    if raw.startswith("EV["):
        raise PreflightError(
            f"{MAPPING_ENV} is encrypted in the exported app spec; keep this env-name-only mapping as GENERAL"
        )
    accounts = parse_json(raw, f"service {service_name!r} {MAPPING_ENV}")
    if not isinstance(accounts, list):
        raise PreflightError(
            f"service {service_name!r} {MAPPING_ENV} must be a JSON array"
        )
    return accounts


def normalize_permissions(raw: Any, label: str) -> str:
    if not isinstance(raw, list) or not raw:
        raise PreflightError(f"{label} must be a non-empty array")
    normalized: list[str] = []
    for index, value in enumerate(raw):
        if not isinstance(value, str) or not PERMISSION_PATTERN.fullmatch(value):
            raise PreflightError(
                f"{label} entry {index} must be a lowercase Meta permission name"
            )
        if value in normalized:
            raise PreflightError(f"{label} contains duplicate permission {value!r}")
        normalized.append(value)
    return ",".join(sorted(normalized))


def normalize_deployed_permissions(raw: str, env_name: str) -> str:
    values = [value.strip() for value in raw.split(",")]
    if not raw.strip() or any(not value for value in values):
        raise PreflightError(f"{env_name} must be a non-empty comma-separated set")
    return normalize_permissions(values, env_name)


def normalize_expected_app(raw: Any, label: str) -> dict[str, str]:
    if not isinstance(raw, dict):
        raise PreflightError(f"expected inventory {label} must be an object")
    unknown = sorted(set(raw) - EXPECTED_APP_FIELDS)
    if unknown:
        raise PreflightError(
            f"expected inventory {label} contains unknown field(s): {', '.join(unknown)}"
        )

    app_id = required_text(raw, "app_id", f"expected inventory {label}")
    owner_business_id = required_text(
        raw, "owner_business_id", f"expected inventory {label}"
    )
    if not META_APP_ID_PATTERN.fullmatch(app_id):
        raise PreflightError(f"expected inventory {label} app_id must be numeric")
    app_mode = required_text(raw, "app_mode", f"expected inventory {label}").lower()
    if app_mode != "live":
        raise PreflightError(f"expected inventory {label} app_mode must be live")
    if not META_ID_PATTERN.fullmatch(owner_business_id):
        raise PreflightError(
            f"expected inventory {label} owner_business_id must be numeric"
        )
    app_review_status = required_text(
        raw, "app_review_status", f"expected inventory {label}"
    ).lower()
    if app_review_status != "approved":
        raise PreflightError(
            f"expected inventory {label} app_review_status must be approved"
        )
    app_review_permissions = normalize_permissions(
        raw.get("app_review_permissions"),
        f"expected inventory {label} app_review_permissions",
    )
    tech_provider_status = required_text(
        raw, "tech_provider_status", f"expected inventory {label}"
    ).lower()
    if tech_provider_status != "verified":
        raise PreflightError(
            f"expected inventory {label} tech_provider_status must be verified"
        )

    review = raw.get("review")
    if not isinstance(review, dict):
        raise PreflightError(f"expected inventory {label} review must be an object")
    unknown_review = sorted(set(review) - EXPECTED_REVIEW_FIELDS)
    if unknown_review:
        raise PreflightError(
            f"expected inventory {label} review contains unknown field(s): "
            + ", ".join(unknown_review)
        )
    reviewer = required_text(review, "reviewer", f"expected inventory {label} review")
    if not REVIEWER_PATTERN.fullmatch(reviewer):
        raise PreflightError(
            f"expected inventory {label} review reviewer must use 'Name <email>' format"
        )
    reviewed_at = required_text(
        review, "reviewed_at", f"expected inventory {label} review"
    )
    if not REVIEWED_AT_PATTERN.fullmatch(reviewed_at):
        raise PreflightError(
            f"expected inventory {label} review reviewed_at must be RFC3339 UTC"
        )
    try:
        reviewed_time = datetime.datetime.fromisoformat(
            reviewed_at.replace("Z", "+00:00")
        )
    except ValueError as exc:
        raise PreflightError(
            f"expected inventory {label} review reviewed_at is not a valid timestamp"
        ) from exc
    now = utc_now()
    if reviewed_time > now + GOVERNANCE_CLOCK_SKEW:
        raise PreflightError(
            f"expected inventory {label} review reviewed_at is in the future"
        )
    if reviewed_time < now - MAX_GOVERNANCE_REVIEW_AGE:
        raise PreflightError(
            f"expected inventory {label} review is older than 90 days"
        )
    evidence = required_text(review, "evidence", f"expected inventory {label} review")
    normalize_https_base(evidence, f"expected inventory {label} review evidence")

    return {
        "app_id": app_id,
        "app_mode": app_mode,
        "owner_business_id": owner_business_id,
        "app_review_status": app_review_status,
        "app_review_permissions": app_review_permissions,
        "tech_provider_status": tech_provider_status,
        "reviewer": reviewer,
        "reviewed_at": reviewed_at,
        "evidence": evidence,
    }


def normalize_expected_inventory(
    raw: Any,
) -> tuple[dict[str, dict[str, str]], list[dict[str, str]]]:
    if not isinstance(raw, dict):
        raise PreflightError("expected inventory must be a JSON object")
    unknown = sorted(set(raw) - EXPECTED_INVENTORY_FIELDS)
    if unknown:
        raise PreflightError(
            f"expected inventory contains unknown field(s): {', '.join(unknown)}"
        )
    apps = {
        app_name: normalize_expected_app(raw.get(app_name), app_name)
        for app_name in APP_ENVIRONMENT_BINDINGS
    }
    if apps["messenger_app"]["app_id"] == apps["instagram_app"]["app_id"]:
        raise PreflightError(
            "Messenger and Instagram Login must use distinct Meta app IDs"
        )
    accounts = raw.get("accounts")
    if not isinstance(accounts, list):
        raise PreflightError("expected inventory accounts must be a JSON array")
    normalized_accounts = normalize_accounts(accounts, expected=True)
    validate_messenger_advanced_access_profile(apps, normalized_accounts)
    return apps, normalized_accounts


def validate_messenger_advanced_access_profile(
    apps: dict[str, dict[str, str]], accounts: list[dict[str, str]]
) -> None:
    """Require the reviewed permissions needed to onboard future organizations.

    The relay process itself needs only the two Messenger delivery permissions.
    Production deployment preflight is deliberately stricter: operators must
    record Advanced Access for Page discovery, ownership review, and Business
    Portfolio inspection before the installation is declared future-org ready.
    """

    required = set(MESSENGER_FUTURE_ORGANIZATION_ADVANCED_ACCESS_PERMISSIONS)
    if any(
        account["channel"] == "instagram"
        and account["instagram_api_mode"] == "facebook_login"
        for account in accounts
    ):
        required.update(MESSENGER_FACEBOOK_LOGIN_INSTAGRAM_PERMISSIONS)

    approved = set(apps["messenger_app"]["app_review_permissions"].split(","))
    missing = sorted(required - approved)
    if missing:
        raise PreflightError(
            "expected inventory messenger_app app_review_permissions is missing "
            "required Advanced Access permission(s) for future-organization "
            f"onboarding: {', '.join(missing)}"
        )


def compare_app_bindings(
    environment: dict[str, str],
    expected_apps: dict[str, dict[str, str]],
) -> list[str]:
    errors: list[str] = []
    for app_name, bindings in APP_ENVIRONMENT_BINDINGS.items():
        expected = expected_apps[app_name]
        for field, env_name in bindings.items():
            value = environment.get(env_name, "").strip()
            if value.startswith("EV["):
                errors.append(
                    f"{env_name} must be inspectable GENERAL configuration, not an encrypted secret"
                )
                continue
            if field == "app_review_permissions":
                try:
                    value = normalize_deployed_permissions(value, env_name)
                except PreflightError as exc:
                    errors.append(str(exc))
                    continue
            elif field == "app_mode":
                value = value.lower()
            if value != expected[field]:
                errors.append(
                    f"deployed relay has an unexpected or missing {env_name} binding"
                )
    return errors


def compare_runtime_trust_bindings(
    spec: dict[str, Any],
    relay_environment: dict[str, str],
    expected_inventory: dict[str, Any],
    rereply_service: str,
    rereply_origin: str,
    relay_base_url: str,
) -> list[str]:
    """Bind both runtime processes to the same protected production trust."""

    rereply_environment = deployed_environment(spec, rereply_service)
    errors: list[str] = []

    expected_urls = (
        (relay_environment, RELAY_REREPLY_BASE_ENV, rereply_origin),
        (rereply_environment, REREPLY_RELAY_BASE_ENV, relay_base_url),
    )
    for environment, env_name, expected_url in expected_urls:
        value = environment.get(env_name, "").strip()
        if value.startswith("EV["):
            errors.append(
                f"{env_name} must be inspectable GENERAL configuration, not an encrypted secret"
            )
        elif value != expected_url:
            errors.append(f"deployed runtime has an unexpected or missing {env_name}")

    deployed_expected_raw = rereply_environment.get(
        REREPLY_EXPECTED_ACCOUNTS_ENV, ""
    ).strip()
    if deployed_expected_raw.startswith("EV["):
        errors.append(
            f"{REREPLY_EXPECTED_ACCOUNTS_ENV} must be inspectable GENERAL configuration, not an encrypted secret"
        )
    elif not deployed_expected_raw:
        errors.append(
            f"deployed runtime has a missing {REREPLY_EXPECTED_ACCOUNTS_ENV} binding"
        )
    else:
        try:
            deployed_expected = parse_json(
                deployed_expected_raw,
                f"service {rereply_service!r} {REREPLY_EXPECTED_ACCOUNTS_ENV}",
            )
        except PreflightError as exc:
            errors.append(str(exc))
        else:
            if deployed_expected != expected_inventory:
                errors.append(
                    f"deployed runtime {REREPLY_EXPECTED_ACCOUNTS_ENV} does not match protected inventory"
                )

    return errors


def required_text(account: dict[str, Any], field: str, label: str) -> str:
    value = account.get(field)
    if not isinstance(value, str) or not value.strip():
        raise PreflightError(f"{label} field {field!r} must be a non-empty string")
    return value.strip()


def canonical_uuid(value: str, label: str) -> str:
    try:
        parsed = uuid.UUID(value)
    except (ValueError, AttributeError) as exc:
        raise PreflightError(f"{label} must be a UUID") from exc
    canonical = str(parsed)
    if parsed.int == 0 or value != canonical:
        raise PreflightError(f"{label} must use canonical non-zero UUID form")
    return canonical


def validate_account(account: Any, expected: bool, index: int) -> dict[str, str]:
    source = "expected inventory" if expected else "deployed mapping"
    label = f"{source} account {index}"
    if not isinstance(account, dict):
        raise PreflightError(f"{label} must be an object")
    allowed = EXPECTED_FIELDS if expected else ACTUAL_FIELDS
    unknown = sorted(set(account) - allowed)
    if unknown:
        raise PreflightError(f"{label} contains unknown field(s): {', '.join(unknown)}")

    key = required_text(account, "key", label)
    if not KEY_PATTERN.fullmatch(key):
        raise PreflightError(f"{label} key is not a safe 1-64 character identifier")
    organization_id = canonical_uuid(
        required_text(account, "organization_id", label),
        f"account {key!r} organization_id",
    )
    meta_business_id = required_text(account, "meta_business_id", label)
    if not META_ID_PATTERN.fullmatch(meta_business_id):
        raise PreflightError(
            f"account {key!r} meta_business_id must be a numeric Meta Business ID"
        )
    organization_name_value = account.get("organization_name", "")
    if organization_name_value is None:
        organization_name_value = ""
    if not isinstance(organization_name_value, str):
        raise PreflightError(f"account {key!r} organization_name must be a string")
    channel = required_text(account, "channel", label)
    if channel not in {"messenger", "instagram"}:
        raise PreflightError(f"account {key!r} channel must be messenger or instagram")
    external_id = required_text(account, "external_account_id", label)
    if not META_ID_PATTERN.fullmatch(external_id):
        raise PreflightError(
            f"account {key!r} external_account_id must be a numeric Meta asset ID"
        )

    mode_value = account.get("instagram_api_mode", "")
    if mode_value is None:
        mode_value = ""
    if not isinstance(mode_value, str):
        raise PreflightError(f"account {key!r} instagram_api_mode must be a string")
    mode = mode_value.strip()
    if channel == "messenger" and mode:
        raise PreflightError(f"account {key!r} must omit instagram_api_mode")
    if channel == "instagram" and mode not in {"instagram_login", "facebook_login"}:
        raise PreflightError(
            f"account {key!r} instagram_api_mode must be instagram_login or facebook_login"
        )

    normalized = {
        "key": key,
        "organization_id": organization_id,
        "organization_name": organization_name_value.strip(),
        "meta_business_id": meta_business_id,
        "channel": channel,
        "external_account_id": external_id,
        "instagram_api_mode": mode,
    }
    for field in (
        "access_token_env",
        "rereply_inbound_secret_env",
        "rereply_outbound_secret_env",
    ):
        value = required_text(account, field, label)
        if not ENV_PATTERN.fullmatch(value):
            raise PreflightError(
                f"account {key!r} {field} must name an environment variable"
            )
        normalized[field] = value

    if expected:
        normalized["rereply_account_id"] = canonical_uuid(
            required_text(account, "rereply_account_id", label),
            f"account {key!r} rereply_account_id",
        )
    else:
        normalized["rereply_webhook_url"] = required_text(
            account, "rereply_webhook_url", label
        )
    return normalized


def validate_uniqueness(accounts: list[dict[str, str]], expected: bool) -> None:
    seen: dict[tuple[str, str], str] = {}
    unique_fields = (
        ("key",),
        ("channel", "external_account_id"),
        (("rereply_account_id",) if expected else ("rereply_webhook_url",)),
        ("rereply_inbound_secret_env",),
        ("rereply_outbound_secret_env",),
    )
    for fields in unique_fields:
        seen.clear()
        for account in accounts:
            value = tuple(account[field] for field in fields)
            if value in seen:
                joined = ", ".join(fields)
                raise PreflightError(
                    f"accounts {seen[value]!r} and {account['key']!r} reuse unique field(s) {joined}"
                )
            seen[value] = account["key"]

    seen_hmac_env_names: dict[str, tuple[str, str]] = {}
    for account in accounts:
        for field in (
            "rereply_inbound_secret_env",
            "rereply_outbound_secret_env",
        ):
            env_name = account[field]
            prior = seen_hmac_env_names.get(env_name)
            if prior is not None:
                prior_account, prior_field = prior
                raise PreflightError(
                    f"accounts {prior_account!r} ({prior_field}) and "
                    f"{account['key']!r} ({field}) reuse HMAC environment "
                    f"variable {env_name!r}"
                )
            seen_hmac_env_names[env_name] = (account["key"], field)

    organization_businesses: dict[str, str] = {}
    for account in accounts:
        organization_id = account["organization_id"]
        meta_business_id = account["meta_business_id"]
        prior = organization_businesses.setdefault(organization_id, meta_business_id)
        if prior != meta_business_id:
            raise PreflightError(
                f"organization {organization_id!r} is bound to multiple Meta Business IDs"
            )


def normalize_accounts(
    raw_accounts: Iterable[Any], expected: bool
) -> list[dict[str, str]]:
    accounts = [
        validate_account(item, expected, index)
        for index, item in enumerate(raw_accounts)
    ]
    if not accounts:
        source = "expected inventory" if expected else "deployed mapping"
        raise PreflightError(f"{source} must contain at least one account")
    validate_uniqueness(accounts, expected)
    return accounts


def normalize_https_base(raw: str, label: str) -> urllib.parse.SplitResult:
    parsed = urllib.parse.urlsplit(raw)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise PreflightError(
            f"{label} must be an HTTPS URL without credentials, query, or fragment"
        )
    return parsed


def expected_webhook_url(origin: urllib.parse.SplitResult, account_id: str) -> str:
    path = origin.path.rstrip("/") + "/api/webhooks/channels/" + account_id
    return urllib.parse.urlunsplit((origin.scheme, origin.netloc, path, "", ""))


def compare_inventory(
    actual: list[dict[str, str]],
    expected: list[dict[str, str]],
    rereply_origin: str,
) -> list[str]:
    origin = normalize_https_base(rereply_origin, "ReReply webhook origin")
    actual_by_key = {account["key"]: account for account in actual}
    expected_by_key = {account["key"]: account for account in expected}
    errors: list[str] = []

    for key in sorted(set(expected_by_key) - set(actual_by_key)):
        errors.append(
            f"required account {key!r} is missing from the deployed relay mapping"
        )
    for key in sorted(set(actual_by_key) - set(expected_by_key)):
        errors.append(
            f"unexpected account {key!r} is present in the deployed relay mapping"
        )
    for key in sorted(set(actual_by_key) & set(expected_by_key)):
        deployed = actual_by_key[key]
        wanted = expected_by_key[key]
        for field in COMPARE_FIELDS:
            if deployed[field] != wanted[field]:
                errors.append(f"account {key!r} has an unexpected {field} binding")
        wanted_webhook = expected_webhook_url(origin, wanted["rereply_account_id"])
        if deployed["rereply_webhook_url"] != wanted_webhook:
            errors.append(
                f"account {key!r} is bound to an unexpected ReReply channel account"
            )
    return errors


def account_health_url(base: urllib.parse.SplitResult, account: dict[str, str]) -> str:
    path = (
        base.path.rstrip("/")
        + "/v1/accounts/"
        + urllib.parse.quote(account["channel"], safe="")
        + "/"
        + urllib.parse.quote(account["external_account_id"], safe="")
    )
    return urllib.parse.urlunsplit((base.scheme, base.netloc, path, "", ""))


def rereply_ready_url(rereply_origin: str) -> str:
    base = normalize_https_base(rereply_origin, "ReReply origin")
    path = base.path.rstrip("/") + "/ready"
    return urllib.parse.urlunsplit((base.scheme, base.netloc, path, "", ""))


def probe_rereply_provider_proof_key_id(
    rereply_origin: str,
    attempts: int,
    retry_delay: float,
    opener=None,
    sleeper: Callable[[float], None] = time.sleep,
) -> tuple[str, list[str]]:
    """Read ReReply's non-secret key fingerprint for cross-runtime comparison."""
    if attempts < 1:
        raise PreflightError("probe attempts must be at least one")
    if retry_delay < 0:
        raise PreflightError("probe retry delay must not be negative")
    opener = opener or urllib.request.build_opener(NoRedirect())
    request = urllib.request.Request(
        rereply_ready_url(rereply_origin),
        method="GET",
        headers={
            "Accept": "application/json",
            "User-Agent": "ReReply-Meta-Relay-Preflight/1.0",
        },
    )
    final_status = 0
    transport_failed = False
    key_id = ""
    for attempt in range(attempts):
        try:
            response = opener.open(request, timeout=10)
            final_status = int(response.status)
            candidate = response.headers.get(PROVIDER_PROOF_KEY_ID_HEADER, "")
            key_id = candidate.strip() if isinstance(candidate, str) else ""
            response.close()
            transport_failed = False
        except urllib.error.HTTPError as exc:
            final_status = int(exc.code)
            key_id = ""
            exc.close()
            transport_failed = False
        except (urllib.error.URLError, TimeoutError, OSError):
            final_status = 0
            key_id = ""
            transport_failed = True

        if 200 <= final_status < 300 and SIGNATURE_PATTERN.fullmatch(key_id):
            return key_id, []
        retryable = transport_failed or final_status == 429 or final_status >= 500 or (
            200 <= final_status < 300
        )
        if not retryable or attempt + 1 == attempts:
            break
        sleeper(retry_delay)

    if transport_failed:
        return "", ["ReReply provider-proof key-ID probe had a transport failure"]
    if 200 <= final_status < 300:
        return "", [
            f"ReReply readiness is missing a valid {PROVIDER_PROOF_KEY_ID_HEADER}"
        ]
    return "", [f"ReReply provider-proof key-ID probe returned HTTP {final_status}"]


def validate_readiness_headers(
    headers: Any,
    account: dict[str, str],
    provider_proof_key_id: str = "",
) -> list[str]:
    expected = {
        READINESS_HEADER: "v2",
        CHANNEL_HEADER: account["channel"],
        EXTERNAL_ACCOUNT_HEADER: account["external_account_id"],
        CHANNEL_ACCOUNT_HEADER: account["rereply_account_id"],
        ORGANIZATION_HEADER: account["organization_id"],
        META_BUSINESS_HEADER: account["meta_business_id"],
    }
    errors: list[str] = []
    for name, wanted in expected.items():
        value = headers.get(name, "") if headers is not None else ""
        if not isinstance(value, str) or value.strip() != wanted:
            errors.append(
                f"account {account['key']!r} has a missing or mismatched {name} attestation"
            )
    provider_proof = (
        headers.get(PROVIDER_PROOF_HEADER, "") if headers is not None else ""
    )
    if not isinstance(provider_proof, str) or not SIGNATURE_PATTERN.fullmatch(
        provider_proof.strip()
    ):
        errors.append(
            f"account {account['key']!r} has a missing or malformed {PROVIDER_PROOF_HEADER} attestation"
        )
    deployed_key_id = (
        headers.get(PROVIDER_PROOF_KEY_ID_HEADER, "") if headers is not None else ""
    )
    if not isinstance(deployed_key_id, str) or not SIGNATURE_PATTERN.fullmatch(
        deployed_key_id.strip()
    ):
        errors.append(
            f"account {account['key']!r} has a missing or malformed {PROVIDER_PROOF_KEY_ID_HEADER} attestation"
        )
    elif provider_proof_key_id and deployed_key_id.strip() != provider_proof_key_id:
        errors.append(
            f"account {account['key']!r} uses a different provider-proof key than ReReply"
        )
    return errors


def probe_accounts(
    expected: list[dict[str, str]],
    secrets: dict[str, Any],
    relay_base_url: str,
    attempts: int,
    retry_delay: float,
    opener=None,
    sleeper: Callable[[float], None] = time.sleep,
    provider_proof_key_id: str = "",
) -> list[str]:
    base = normalize_https_base(relay_base_url, "Meta relay base URL")
    if attempts < 1:
        raise PreflightError("probe attempts must be at least one")
    if retry_delay < 0:
        raise PreflightError("probe retry delay must not be negative")
    if not isinstance(secrets, dict):
        raise PreflightError("preflight signing secrets must be a JSON object")
    opener = opener or urllib.request.build_opener(NoRedirect())

    secret_values: dict[str, str] = {}
    seen_secret_values: set[str] = set()
    for account in expected:
        reference = account["rereply_outbound_secret_env"]
        secret = secrets.get(reference)
        if not isinstance(secret, str) or not secret.strip():
            raise PreflightError(
                f"preflight signing secret is missing for account {account['key']!r}"
            )
        if secret != secret.strip():
            raise PreflightError(
                f"preflight signing secret must not contain surrounding whitespace for account {account['key']!r}"
            )
        if len(secret.encode("utf-8")) < MIN_HMAC_SECRET_BYTES:
            raise PreflightError(
                "preflight signing secret must contain at least "
                f"{MIN_HMAC_SECRET_BYTES} UTF-8 bytes for account {account['key']!r}"
            )
        if secret in seen_secret_values:
            raise PreflightError(
                "preflight signing secrets must be unique per relay account"
            )
        seen_secret_values.add(secret)
        secret_values[account["key"]] = secret

    errors: list[str] = []
    for account in expected:
        signature = (
            "sha256="
            + hmac.new(
                secret_values[account["key"]].encode("utf-8"),
                b"",
                hashlib.sha256,
            ).hexdigest()
        )
        request = urllib.request.Request(
            account_health_url(base, account),
            method="HEAD",
            headers={
                SIGNATURE_HEADER: signature,
                "Accept": "application/json",
                "Content-Length": "0",
                "User-Agent": "ReReply-Meta-Relay-Preflight/1.0",
            },
        )
        final_status = 0
        transport_failed = False
        attestation_failures: list[str] = []
        for attempt in range(attempts):
            try:
                response = opener.open(request, timeout=10)
                final_status = int(response.status)
                attestation_failures = (
                    validate_readiness_headers(
                        response.headers,
                        account,
                        provider_proof_key_id,
                    )
                    if final_status == 204
                    else []
                )
                response.close()
                transport_failed = False
            except urllib.error.HTTPError as exc:
                final_status = int(exc.code)
                attestation_failures = []
                exc.close()
                transport_failed = False
            except (urllib.error.URLError, TimeoutError, OSError):
                final_status = 0
                attestation_failures = []
                transport_failed = True

            if final_status == 204 and not attestation_failures:
                break
            retryable = (
                transport_failed
                or final_status == 429
                or final_status >= 500
                or bool(attestation_failures)
            )
            if not retryable or attempt + 1 == attempts:
                break
            sleeper(retry_delay)

        if final_status == 204 and attestation_failures:
            errors.extend(attestation_failures)
        elif final_status != 204:
            if transport_failed:
                errors.append(
                    f"account {account['key']!r} relay health probe had a transport failure"
                )
            else:
                errors.append(
                    f"account {account['key']!r} relay health probe returned HTTP {final_status}"
                )
    return errors


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--app-spec",
        default="-",
        help="DigitalOcean app spec JSON path, or - for stdin",
    )
    parser.add_argument(
        "--service", default="meta-relay", help="DigitalOcean service name"
    )
    parser.add_argument(
        "--rereply-service",
        default="omnitech-web",
        help="DigitalOcean ReReply application service name",
    )
    parser.add_argument(
        "--rereply-origin",
        default="https://app.rereply.app",
        help="Allowed origin for account-specific ReReply webhook URLs",
    )
    parser.add_argument(
        "--relay-base-url",
        default="https://app.rereply.app/meta-relay",
        help="Public Meta relay route prefix used for signed HEAD probes",
    )
    parser.add_argument("--expected-env", default=EXPECTED_ENV)
    parser.add_argument("--secrets-env", default=SECRETS_ENV)
    parser.add_argument("--attempts", type=int, default=6)
    parser.add_argument("--retry-delay", type=float, default=5.0)
    parser.add_argument(
        "--validate-only",
        action="store_true",
        help="Validate deployed mappings without performing signed account-binding probes",
    )
    parser.add_argument(
        "--reject-review-runtime-only",
        action="store_true",
        help="Only reject staging review runtime wiring; does not require a production relay inventory",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.app_spec == "-":
            spec = load_app_spec(sys.stdin)
        else:
            with open(args.app_spec, encoding="utf-8") as stream:
                spec = load_app_spec(stream)

        review_runtime_failures = production_review_runtime_errors(spec)
        if args.reject_review_runtime_only:
            if review_runtime_failures:
                for failure in review_runtime_failures:
                    print(f"[FAIL] {failure}", file=sys.stderr)
                print(
                    "Production staging-review runtime check failed with "
                    f"{len(review_runtime_failures)} issue(s).",
                    file=sys.stderr,
                )
                return 1
            print("Production staging-review runtime check passed.")
            return 0

        expected_raw = load_json_environment(args.expected_env, dict)
        expected_apps, expected = normalize_expected_inventory(expected_raw)
        environment = deployed_environment(spec, args.service)
        actual = normalize_accounts(
            deployed_accounts(environment, args.service), expected=False
        )
        failures = review_runtime_failures
        failures.extend(
            provider_proof_secret_presence_errors(
                spec, args.service, args.rereply_service
            )
        )
        failures.extend(relay_runtime_secret_presence_errors(spec, args.service, actual))
        failures.extend(compare_app_bindings(environment, expected_apps))
        failures.extend(
            compare_runtime_trust_bindings(
                spec,
                environment,
                expected_raw,
                args.rereply_service,
                args.rereply_origin,
                args.relay_base_url,
            )
        )
        failures.extend(compare_inventory(actual, expected, args.rereply_origin))
        if not failures and not args.validate_only:
            secrets = load_json_environment(args.secrets_env, dict)
            provider_proof_key_id, key_id_failures = (
                probe_rereply_provider_proof_key_id(
                    args.rereply_origin,
                    args.attempts,
                    args.retry_delay,
                )
            )
            failures.extend(key_id_failures)
            if not key_id_failures:
                failures.extend(
                    probe_accounts(
                        expected,
                        secrets,
                        args.relay_base_url,
                        args.attempts,
                        args.retry_delay,
                        provider_proof_key_id=provider_proof_key_id,
                    )
                )
        if failures:
            for failure in failures:
                print(f"[FAIL] {failure}", file=sys.stderr)
            print(
                f"Meta relay preflight failed with {len(failures)} issue(s).",
                file=sys.stderr,
            )
            return 1

        probe_count = 0 if args.validate_only else len(expected)
        print(
            f"Meta relay preflight passed: {len(expected)} mapping(s), "
            f"{probe_count} signed binding probe(s)."
        )
        return 0
    except PreflightError as exc:
        print(f"Meta relay preflight configuration error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
