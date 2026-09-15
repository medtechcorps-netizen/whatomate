#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import provision_production_crm_canary_fixture as fixture
import verify_production_release as common

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
registration = protected["registration"]
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


organizations = unwrap(
    transport.request("GET", "/api/organizations", session=session)
)
if isinstance(organizations, list):
    organization_rows = organizations
elif isinstance(organizations, dict) and isinstance(
    organizations.get("organizations"), list
):
    organization_rows = organizations["organizations"]
else:
    print(
        common.canonical_payload_bytes(
            {
                "stage": "organizations_shape",
                "type": type(organizations).__name__,
                "keys": sorted(organizations.keys())
                if isinstance(organizations, dict)
                else None,
            }
        ).decode("utf-8")
    )
    raise SystemExit(0)

organization_names = {
    row.get("name") for row in organization_rows if isinstance(row, dict)
}

users = unwrap(
    transport.request(
        "GET",
        "/api/users?page=1&limit=100",
        session=session,
        organization_id=descriptor["super_admin_home_org_id"],
    )
)
if isinstance(users, list):
    user_rows = users
elif isinstance(users, dict) and isinstance(users.get("users"), list):
    user_rows = users["users"]
else:
    print(
        common.canonical_payload_bytes(
            {
                "stage": "users_shape",
                "type": type(users).__name__,
                "keys": sorted(users.keys()) if isinstance(users, dict) else None,
            }
        ).decode("utf-8")
    )
    raise SystemExit(0)

user_emails = {
    row.get("email") for row in user_rows if isinstance(row, dict)
}

result = {
    "klinik_org_present": descriptor["klinik"]["organization_name"]
    in organization_names,
    "non_klinik_org_present": descriptor["non_klinik"]["organization_name"]
    in organization_names,
    "klinik_user_present": registration["klinik_email"] in user_emails,
    "non_klinik_user_present": registration["non_klinik_email"] in user_emails,
    "organization_count": len(organization_names),
    "user_count": len(user_emails),
}
print(common.canonical_payload_bytes(result).decode("utf-8"))
