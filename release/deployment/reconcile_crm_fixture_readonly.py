#!/usr/bin/env python3
"""Read-only inventory shape diagnosis for the production CRM canary preflight.

Every call is a GET against the ReReply product using the fixture principal's
own session. No fixture effect, provider call, registration resume, custody
secret or upload retry is exercised; the script cannot mutate product state.

The preflight in provision_production_crm_canary_fixture.py requires every
inventory row identity to be a canonical RFC 4122 version 1-5 UUID. This
diagnosis reports, for the same endpoints the preflight walks, only shape
metadata: response keys, pagination echoes, row key sets, and a classification
of each identity string. Raw identities and personal values are never printed,
only truncated SHA-256 digests that allow correlation between runs.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import provision_production_crm_canary_fixture as fixture
import verify_production_release as common

CANONICAL_UUID = common.UUID_RE
RELAXED_UUID = re.compile(r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
MAX_ROWS_REPORTED = 40

request = {
    "schema_version": 1,
    "control_sha": os.environ["GITHUB_SHA"],
    "operation_sha256": os.environ["CRM_FIXTURE_OPERATION_SHA256"],
    "descriptor_sha256": os.environ["CRM_FIXTURE_DESCRIPTOR_SHA256"],
}
protected = fixture.validate_protected_input(
    common.loads_strict(os.environ["CRM_CANARY_FIXTURE_INPUT_JSON"]),
    request,
)
descriptor = protected["descriptor"]
credentials = protected["credentials"]
transport = fixture.ProductHTTP(credentials["meta_access_token"])
session = transport.login(
    credentials["super_admin_login"]["email"],
    credentials["super_admin_login"]["password"],
)


def unwrap(value: object) -> object:
    if (
        isinstance(value, dict)
        and value.get("status") == "success"
        and "data" in value
    ):
        return value["data"]
    return value


def digest(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8", "surrogatepass")).hexdigest()[:16]


def classify(value: object) -> dict[str, object]:
    if value is None:
        return {"type": "null"}
    if type(value) is not str:
        return {"type": type(value).__name__}
    info: dict[str, object] = {
        "type": "str",
        "length": len(value),
        "canonical": CANONICAL_UUID.fullmatch(value) is not None,
        "relaxed": RELAXED_UUID.fullmatch(value) is not None,
        "digest": digest(value),
    }
    if RELAXED_UUID.fullmatch(value) is not None:
        info["version_char"] = value[14]
        info["variant_char"] = value[19]
        info["uppercase"] = value != value.lower()
        info["nil"] = value == "00000000-0000-0000-0000-000000000000"
    return info


def envelope_shape(value: object) -> dict[str, object]:
    if isinstance(value, dict):
        return {"type": "dict", "keys": sorted(value)}
    if isinstance(value, list):
        return {"type": "list", "length": len(value)}
    return {"type": type(value).__name__}


def row_shapes(rows: object, key: str) -> dict[str, object]:
    if not isinstance(rows, list):
        return {"type": type(rows).__name__}
    report: dict[str, object] = {"count": len(rows)}
    identities: dict[str, object] = {}
    keysets: dict[str, int] = {}
    for index, row in enumerate(rows[:MAX_ROWS_REPORTED]):
        if not isinstance(row, dict):
            keysets[type(row).__name__] = keysets.get(type(row).__name__, 0) + 1
            continue
        signature = ",".join(sorted(row))
        keysets[signature] = keysets.get(signature, 0) + 1
        identities[str(index)] = classify(row.get("id"))
    report["row_key_sets"] = dict(sorted(keysets.items()))
    report["identities"] = identities
    if len(rows) > MAX_ROWS_REPORTED:
        report["truncated"] = len(rows) - MAX_ROWS_REPORTED
    return report


def get(path: str, org: str | None = None) -> object:
    return unwrap(
        transport.request("GET", path, session=session, organization_id=org)
    )


def list_endpoint(path: str, key: str, org: str | None = None) -> dict[str, object]:
    try:
        data = get(path, org)
    except Exception as exc:  # noqa: BLE001 - shape diagnosis records, never raises
        return {"error": type(exc).__name__}
    shape = envelope_shape(data)
    rows = data.get(key) if isinstance(data, dict) else None
    return {"envelope": shape, "rows": row_shapes(rows, key)}


def paged_endpoint(path: str, key: str, org: str | None = None) -> dict[str, object]:
    try:
        data = get(path + "?page=1&limit=100", org)
    except Exception as exc:  # noqa: BLE001 - shape diagnosis records, never raises
        return {"error": type(exc).__name__}
    shape = envelope_shape(data)
    result: dict[str, object] = {"envelope": shape}
    if isinstance(data, dict):
        result["echo"] = {
            name: data.get(name)
            for name in ("page", "limit", "total", "online_count")
            if name in data
        }
    rows = data.get(key) if isinstance(data, dict) else None
    result["rows"] = row_shapes(rows, key)
    return result


report: dict[str, object] = {
    "schema_version": 1,
    "stage": "inventory_shape",
    "super_admin_home_org_digest": digest(descriptor["super_admin_home_org_id"]),
    "resellers": list_endpoint("/api/resellers", "resellers"),
}

organizations = get("/api/organizations")
report["organizations"] = {
    "envelope": envelope_shape(organizations),
}
org_rows = organizations.get("organizations") if isinstance(organizations, dict) else None
report["organizations"]["rows"] = row_shapes(org_rows, "organizations")

org_reports: list[dict[str, object]] = []
if isinstance(org_rows, list):
    for index, org in enumerate(org_rows):
        if not isinstance(org, dict):
            org_reports.append({"index": index, "type": type(org).__name__})
            continue
        org_id = org.get("id")
        entry: dict[str, object] = {
            "index": index,
            "id": classify(org_id),
            "home_org": org_id == descriptor["super_admin_home_org_id"],
            "reseller_bound": org.get("reseller_id") is not None,
            "users": paged_endpoint("/api/users", "users", org_id),
            "accounts": list_endpoint("/api/accounts", "accounts", org_id),
        }
        org_reports.append(entry)
report["org_count"] = len(org_reports)
report["orgs"] = org_reports

print(json.dumps(report, sort_keys=True, separators=(",", ":")))
