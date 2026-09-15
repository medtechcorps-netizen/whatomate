#!/usr/bin/env python3
"""Reviewed inverse for a quarantined production CRM canary fixture.

A quarantined fixture execution can leave the two reserved canary
organizations behind together with the two issued fixture logins. This
controller retires exactly those four identities so a fresh custody operation
can run, and it does nothing else.

Scope and limits:

* it never touches the DigitalOcean app spec, the reply allowlist, the provider
  deployment, messages, contacts, or any customer-visible record;
* it makes no Graph API call; the shared fixture input envelope supplies the
  operator login and the fixture selector only;
* every resource is selected by the descriptor's exact organization name,
  reseller portfolio and issued login address, and every selected identity must
  equal the reviewed inverse authority before anything is deleted;
* each of the four deletions is attempted at most once in a run, an ambiguous
  DELETE stops the run immediately, and no deletion is repeated or improvised;
* deletion is idempotent, so a later reviewed run observes an already-retired
  identity and reports it instead of failing;
* published evidence carries hashes, counts and booleans only.
"""
from __future__ import annotations

import argparse
import copy
import datetime as dt
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import provision_production_crm_canary_fixture as fixture
import verify_production_release as common

_require = fixture._require
_schema = fixture._schema

INVERSE_WORKFLOW_PATH = ".github/workflows/inverse-production-crm-canary-fixture.yml"
ROLES = ("klinik", "non_klinik")
ORDER = (
    ("delete_klinik_user", "klinik", "user"),
    ("delete_klinik_org", "klinik", "organization"),
    ("delete_non_klinik_user", "non_klinik", "user"),
    ("delete_non_klinik_org", "non_klinik", "organization"),
)
STAGES = tuple(stage for stage, _, _ in ORDER)
USER_PAGE_LIMIT = 100
USER_PAGE_BOUND = 20
MAX_AUTHORITY_LIFETIME_SECONDS = 24 * 60 * 60
RESERVED_PREFIX = "rereply-canary"
ROUTE_RE = re.compile(
    r"/api/(?:users|organizations)/"
    r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
AUTHORITY_KEYS = {
    "schema_version", "kind", "control_sha", "request_sha256", "operation_sha256",
    "descriptor_sha256", "registration_sha256", "origin", "targets", "expires_at",
}
ORIGIN_KEYS = {"run_id", "control_sha", "artifact_id", "artifact_digest", "intent_sha256"}
TARGET_KEYS = {"organization_id", "user_id"}
PLAN_KEYS = {
    "schema_version", "kind", "control_sha", "request_sha256", "operation_sha256",
    "descriptor_sha256", "registration_sha256", "organization_count",
    "reserved_organization_count", "issued_login_count", "targets",
}
RESULT_KEYS = {
    "schema_version", "kind", "control_sha", "request_sha256", "operation_sha256",
    "descriptor_sha256", "origin", "targets_sha256", "stages", "state",
    "reserved_organization_present", "issued_login_present", "organization_count",
}
STAGE_KEYS = {"stage", "route", "target_sha256", "action", "status"}


class AmbiguousInverse(common.ReleaseError):
    """One deletion may have reached the product; reconciliation is required."""


def request_from_environment() -> dict[str, Any]:
    return fixture.validate_request(common.loads_strict(os.environ["REQUEST_JSON"]))


def protected_from_environment(request: Any) -> dict[str, Any]:
    return fixture.validate_protected_input(
        common.loads_strict(os.environ["CRM_CANARY_FIXTURE_INPUT_JSON"]), request
    )


class ProductInverse:
    """Survey, retire and verify exactly the two reserved fixture tenants."""

    def __init__(self, request: Any, protected: Any, transport: Any):
        self.request = fixture.validate_request(request)
        self.protected = protected
        self.d = protected["descriptor"]
        self.registration = protected["registration"]
        self.transport = transport
        self.session = None
        self.stages: list[dict[str, Any]] = []
        self.started = False

    def _once(self) -> None:
        """A single-use controller: one plan or one inverse per process."""
        _require(not self.started, "inverse controller was already used")
        self.started = True

    # --- read-only transport helpers -------------------------------------

    def _request(self, method: str, path: str, *, org: str | None = None) -> Any:
        try:
            result = self.transport.request(method, path, session=self.session, organization_id=org)
            _require(type(result) is dict, "inverse response shape differs")
            _require(set(result) <= {"status", "data"} and result.get("status") == "success"
                     and "data" in result, "inverse response envelope differs")
            return result["data"]
        except Exception as exc:
            raise common.ReleaseError(
                "inverse transport did not complete cleanly: " + fixture._reason(exc)
            ) from None

    def _get(self, path: str, *, org: str | None = None) -> Any:
        return self._request("GET", path, org=org)

    def login(self) -> None:
        login = self.protected["credentials"]["super_admin_login"]
        self.session = self.transport.login(login["email"], login["password"])
        me = self._get("/api/me")
        _require(type(me) is dict and me.get("id") == self.d["super_admin_id"]
                 and me.get("organization_id") == self.d["super_admin_home_org_id"]
                 and me.get("is_super_admin") is True and me.get("is_active") is True,
                 "inverse principal differs")

    @staticmethod
    def _rows(data: Any, key: str) -> list[dict[str, Any]]:
        _require(type(data) is dict and type(data.get(key)) is list,
                 "inverse inventory shape differs")
        rows = data[key]
        _require(all(type(row) is dict for row in rows), "inverse inventory row differs")
        return rows

    def organizations(self) -> list[dict[str, Any]]:
        rows = self._rows(self._get("/api/organizations"), "organizations")
        identities = [common.require_inventory_uuid(row.get("id"), "inverse organization identity")
                      for row in rows]
        _require(len(set(identities)) == len(identities),
                 "inverse organization identities differ")
        return rows

    def users(self, org: str) -> list[dict[str, Any]]:
        """Every listed user of one organization; multi-page inventories fail closed."""
        rows: list[dict[str, Any]] = []
        total = None
        for page in range(1, USER_PAGE_BOUND + 1):
            data = self._get(f"/api/users?page={page}&limit={USER_PAGE_LIMIT}", org=org)
            page_rows = self._rows(data, "users")
            observed = common.exact_int(data.get("total"), "inverse user total", 0,
                                        USER_PAGE_BOUND * USER_PAGE_LIMIT)
            _require(data.get("page") == page and data.get("limit") == USER_PAGE_LIMIT
                     and (total is None or total == observed), "inverse user pagination changed")
            total = observed
            identities = [common.require_inventory_uuid(row.get("id"), "inverse user identity")
                          for row in page_rows]
            _require(len(set(identities)) == len(identities), "inverse user identities differ")
            rows.extend(page_rows)
            _require(len(rows) <= total, "inverse user inventory exceeds total")
            if len(rows) == total:
                return rows
        common.fail("inverse user inventory exceeds bound")

    # --- survey -----------------------------------------------------------

    def survey(self) -> dict[str, Any]:
        """Read-only selection of at most one organization and login per role."""
        organizations = self.organizations()
        roles: dict[str, Any] = {}
        reserved = 0
        for row in organizations:
            if str(row.get("name", "")).lower().startswith(RESERVED_PREFIX) \
                    or str(row.get("slug", "")).lower().startswith(RESERVED_PREFIX):
                reserved += 1
        for role in ROLES:
            wanted = self.d[role]
            matches = [row for row in organizations
                       if row.get("name") == wanted["organization_name"]
                       and row.get("reseller_id") == self.d["reseller_id"]]
            _require(len(matches) <= 1, "inverse organization is not unique")
            organization = matches[0] if matches else None
            logins: list[dict[str, Any]] = []
            if organization is not None:
                email = self.registration[role + "_email"]
                logins = [row for row in self.users(
                    common.require_inventory_uuid(organization["id"], "inverse organization")
                ) if row.get("email") == email]
                _require(len(logins) <= 1, "issued fixture login is not unique")
            if organization is not None:
                org_id = common.require_uuid(organization["id"], "inverse organization")
                _require(org_id != self.d["super_admin_home_org_id"],
                         "inverse target is the operator home organization")
            if logins:
                _require(logins[0].get("organization_id") == organization["id"]
                         and logins[0].get("is_super_admin") is False,
                         "issued fixture login is not a native tenant user")
            roles[role] = {
                "organization": organization,
                "user": logins[0] if logins else None,
            }
        return {"organizations": organizations, "reserved": reserved, "roles": roles,
                "home": self.d["super_admin_home_org_id"]}

    def plan(self) -> dict[str, Any]:
        self._once()
        survey = self.survey()
        targets = {}
        for role in ROLES:
            entry = survey["roles"][role]
            _require(entry["organization"] is not None and entry["user"] is not None,
                     "reserved fixture state is incomplete")
            targets[role] = {
                "organization_id": common.require_uuid(entry["organization"]["id"],
                                                       "inverse organization"),
                "user_id": common.require_uuid(entry["user"]["id"], "inverse user"),
            }
        _require(targets["klinik"]["organization_id"] != targets["non_klinik"]["organization_id"]
                 and targets["klinik"]["user_id"] != targets["non_klinik"]["user_id"],
                 "inverse targets are not distinct")
        return _schema({
            "schema_version": 1, "kind": "crm-canary-fixture-inverse-plan",
            "control_sha": self.request["control_sha"],
            "request_sha256": common.sha256_value(self.request),
            "operation_sha256": self.request["operation_sha256"],
            "descriptor_sha256": self.request["descriptor_sha256"],
            "registration_sha256": common.sha256_value(self.registration),
            "organization_count": len(survey["organizations"]),
            "reserved_organization_count": survey["reserved"],
            "issued_login_count": sum(
                1 for role in ROLES if survey["roles"][role]["user"] is not None),
            "targets": targets,
        }, PLAN_KEYS, "inverse plan")

    # --- mutation ---------------------------------------------------------

    def _residual(self) -> dict[str, Any]:
        """Post-condition scan that never requires either target to exist."""
        organizations = self.organizations()
        issued = {self.registration[role + "_email"] for role in ROLES}
        emails = [row.get("email") for organization in organizations
                  for row in self.users(common.require_uuid(organization["id"],
                                                            "inverse organization"))]
        return {
            "reserved_organization_present": any(
                str(row.get("name", "")).lower().startswith(RESERVED_PREFIX)
                or str(row.get("slug", "")).lower().startswith(RESERVED_PREFIX)
                for row in organizations),
            "issued_login_present": any(email in issued for email in emails),
            "organization_count": len(organizations),
        }

    def _retire(self, stage: str, role: str, resource: str, pinned: Any, home: str,
                survey: Any) -> None:
        target = pinned[role]
        key = "organization_id" if resource == "organization" else "user_id"
        route = "organizations" if resource == "organization" else "users"
        path = f"/api/{route}/{target[key]}"
        _require(ROUTE_RE.fullmatch(path) is not None, "inverse delete route differs")
        record = _schema({"stage": stage, "route": route, "action": "observed",
                          "target_sha256": common.sha256_value(target[key]), "status": "absent"},
                         STAGE_KEYS, "inverse stage")
        live = survey["roles"][role]
        present = live["organization"] is not None if resource == "organization" \
            else live["user"] is not None
        if present:
            _require(common.require_uuid(
                (live["organization"] if resource == "organization" else live["user"])["id"],
                "inverse live identity") == target[key],
                "inverse target differs from the reviewed authority")
            record["status"] = "present"
        if not present:
            record["action"] = "already_absent"
            self.stages.append(record)
            return
        # A user is retired from its own tenant; an organization is retired from
        # the operator's home tenant, which can never be its own target.
        header = target["organization_id"] if resource == "user" else home
        _require(resource == "user" or home != target["organization_id"],
                 "inverse audit organization differs")
        try:
            self.transport.delete(path, session=self.session, organization_id=header)
        except Exception as exc:
            record["action"] = "ambiguous"
            record["status"] = "unknown"
            self.stages.append(record)
            raise AmbiguousInverse(
                f"inverse deletion is ambiguous at {stage}: " + fixture._reason(exc)
            ) from None
        record["action"] = "deleted"
        after = self.survey()
        remaining = after["roles"][role]
        record["status"] = "present" if (
            remaining["organization"] is not None if resource == "organization"
            else remaining["user"] is not None) else "absent"
        self.stages.append(record)
        _require(record["status"] == "absent",
                 f"inverse {stage} did not retire the identity")

    def apply(self, authority: Any) -> dict[str, Any]:
        self._once()
        survey = self.survey()
        home = survey["home"]
        for stage, role, resource in ORDER:
            self._retire(stage, role, resource, authority["targets"], home, survey)
        _require(tuple(record["stage"] for record in self.stages) == STAGES,
                 "inverse stage inventory differs")
        final = self._residual()
        _require(final["reserved_organization_present"] is False
                 and final["issued_login_present"] is False,
                 "inverse verification found residual fixture state")
        retired = sum(1 for record in self.stages if record["action"] == "deleted")
        return _schema({
            "schema_version": 1, "kind": "crm-canary-fixture-inverse-result",
            "control_sha": self.request["control_sha"],
            "request_sha256": common.sha256_value(self.request),
            "operation_sha256": self.request["operation_sha256"],
            "descriptor_sha256": self.request["descriptor_sha256"],
            "origin": copy.deepcopy(authority["origin"]),
            "targets_sha256": common.sha256_value(authority["targets"]),
            "stages": copy.deepcopy(self.stages),
            "state": "inverse_verified" if retired else "inverse_already_absent",
            "reserved_organization_present": final["reserved_organization_present"],
            "issued_login_present": final["issued_login_present"],
            "organization_count": final["organization_count"],
        }, RESULT_KEYS, "inverse result")


def verify_origin(api: Any, root: Path, origin: Any, request: Any) -> dict[str, Any]:
    """Bind the inverse to the exact quarantined fixture execution."""
    o = _schema(origin, ORIGIN_KEYS, "inverse origin")
    run_id = common.require_run_id(o["run_id"], "inverse origin run")
    common.require_sha1(o["control_sha"], "inverse origin control")
    artifact_id = common.require_run_id(o["artifact_id"], "inverse origin artifact")
    digest = common.require_digest(o["artifact_digest"], "inverse origin digest")
    common.require_sha256(o["intent_sha256"], "inverse origin intent")
    run = api.get(fixture.API_PREFIX + "/actions/runs/" + run_id)
    _require(run.get("run_attempt") == 1 and run.get("head_branch") == "main"
             and run.get("event") == "workflow_dispatch"
             and run.get("path") == fixture.WORKFLOW_PATH
             and run.get("head_sha") == o["control_sha"]
             and run.get("status") == "completed" and run.get("conclusion") == "failure",
             "inverse origin run differs")
    ancestry = subprocess.run(
        ["git", "-C", str(root), "merge-base", "--is-ancestor", o["control_sha"],
         os.environ["CONTROL_SHA"]],
        capture_output=True, check=False,
    )
    _require(ancestry.returncode == 0,
             "inverse origin is not an ancestor of this control")
    meta = api.get(fixture.API_PREFIX + "/actions/artifacts/" + artifact_id)
    fixture._artifact_record(meta, "crm-canary-fixture-intent-" + run_id + "-1",
                             run_id, o["control_sha"], digest)
    files = fixture._extract_exact(api.artifact(artifact_id, digest),
                                   {"intent.json", "intent.sha256"})
    _require(common.sha256_bytes(files["intent.json"]) == o["intent_sha256"]
             and files["intent.sha256"] == (o["intent_sha256"] + "\n").encode(),
             "inverse origin intent differs")
    intent = fixture.validate_origin_intent(common.loads_strict(files["intent.json"]))
    _require(intent["origin_run_id"] == run_id and intent["control_sha"] == o["control_sha"]
             and intent["request"]["schema_version"] == request["schema_version"]
             and intent["request"]["operation_sha256"] == request["operation_sha256"]
             and intent["request"]["descriptor_sha256"] == request["descriptor_sha256"],
             "inverse origin request differs")
    inventory = api.pages(fixture.API_PREFIX + "/actions/runs/" + run_id + "/artifacts",
                          "artifacts")
    stages = {str(a.get("name", "")).rsplit("-1-", 1)[1]
              for a in inventory["artifacts"]
              if str(a.get("name", "")).startswith("crm-canary-fixture-burn-" + run_id + "-1-")}
    _require(bool(stages) and stages <= set(fixture.STAGES),
             "inverse origin has no burned fixture effects")
    return {"run_id": run_id, "control_sha": o["control_sha"], "artifact_id": artifact_id,
            "artifact_digest": digest, "intent_sha256": o["intent_sha256"],
            "burn_count": len(stages)}


def validate_authority(value: Any, api: Any, root: Path, request: Any, protected: Any,
                       *, now: dt.datetime | None = None) -> dict[str, Any]:
    moment = now or dt.datetime.now(dt.timezone.utc)
    a = _schema(value, AUTHORITY_KEYS, "inverse authority")
    _require(a["schema_version"] == 1 and a["kind"] == "crm-canary-fixture-inverse-authority",
             "inverse authority schema differs")
    _require(a["control_sha"] == os.environ["CONTROL_SHA"], "inverse authority control differs")
    for key in ("request_sha256", "operation_sha256", "descriptor_sha256", "registration_sha256"):
        common.require_sha256(a[key], "inverse authority digest")
    _require(a["request_sha256"] == common.sha256_value(request)
             and a["operation_sha256"] == request["operation_sha256"]
             and a["descriptor_sha256"] == request["descriptor_sha256"]
             and a["registration_sha256"] == common.sha256_value(protected["registration"]),
             "inverse authority binding differs")
    expires = common.require_timestamp(a["expires_at"], "inverse authority expiry")
    _require(common.format_timestamp(expires) == a["expires_at"]
             and moment < expires
             and (expires - moment).total_seconds() <= MAX_AUTHORITY_LIFETIME_SECONDS,
             "inverse authority window differs")
    targets = _schema(a["targets"], set(ROLES), "inverse authority targets")
    checked = {}
    for role in ROLES:
        target = _schema(targets[role], TARGET_KEYS, "inverse authority target")
        checked[role] = {
            "organization_id": common.require_uuid(target["organization_id"],
                                                   "inverse organization"),
            "user_id": common.require_uuid(target["user_id"], "inverse user"),
        }
    _require(checked["klinik"]["organization_id"] != checked["non_klinik"]["organization_id"]
             and checked["klinik"]["user_id"] != checked["non_klinik"]["user_id"],
             "inverse authority targets are not distinct")
    result = copy.deepcopy(a)
    result["targets"] = checked
    result["origin"] = verify_origin(api, root, a["origin"], request)
    return result


def _reason_text(exc: BaseException) -> str:
    return fixture._reason(exc)


def write_attempt(directory: Path, request: Any, stages: Any, reason: str) -> None:
    """Content-free journal for a stopped inverse, so evidence survives the run."""
    fixture._write_public(directory, "inverse-attempt", {
        "schema_version": 1, "kind": "crm-canary-fixture-inverse-attempt",
        "control_sha": request["control_sha"],
        "request_sha256": common.sha256_value(request),
        "stages": copy.deepcopy(stages), "reason": reason,
    })


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Reviewed CRM canary fixture inverse")
    parser.add_argument("command", choices=["plan", "apply"])
    parser.add_argument("--control-root", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args(argv)
    request = None
    inverse = None
    try:
        root = args.control_root.resolve(strict=True)
        api = fixture.GitHubRead(os.environ["GH_TOKEN"])
        fixture._current_guard(api, root, workflow=INVERSE_WORKFLOW_PATH)
        request = request_from_environment()
        protected = protected_from_environment(request)
        transport = fixture.ProductHTTP(protected["credentials"]["meta_access_token"])
        inverse = ProductInverse(request, protected, transport)
        inverse.login()
        if args.command == "plan":
            fixture._write_public(args.output_dir, "inverse-plan", inverse.plan())
        else:
            authority = validate_authority(
                common.loads_strict(os.environ["CRM_CANARY_FIXTURE_INVERSE_AUTHORITY_JSON"]),
                api, root, request, protected,
            )
            try:
                result = inverse.apply(authority)
            except Exception as exc:
                _record_attempt(args.output_dir, request, inverse, exc)
                raise
            fixture._write_public(args.output_dir, "inverse-result", result)
        return 0
    except Exception as exc:
        print("fixture inverse stopped; no deletion is repeated: "
              f"{type(exc).__name__}: {_reason_text(exc)}", file=sys.stderr)
        return 1


def _record_attempt(directory: Path, request: Any, inverse: Any, exc: BaseException) -> None:
    """Best-effort journal; a failed run must still publish what it attempted."""
    try:
        if request is not None and inverse is not None:
            write_attempt(directory, request, inverse.stages, _reason_text(exc))
    except Exception:
        pass


if __name__ == "__main__":
    raise SystemExit(main())
