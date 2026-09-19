#!/usr/bin/env python3
"""One protected-control driver bootstrap; never a production rollout.

Only a later separately approved main workflow may use ``install``. The public
packet binds all choices and the protected descriptor; missing choices are not
defaults. There is no retry/resume/update/delete or database/user/fixture create
path. An ambiguous create or secret installation requires read-only review.

Managed database credentials are resolved only by App Platform's fixed bindable
expression. This controller never requests a database connection or user record.
The sole allowed ledger write is the existing driver's table initialization.
"""
from __future__ import annotations

import argparse
import base64
import copy
import datetime as dt
import hashlib
import http.client
import io
import ipaddress
import os
from pathlib import Path
import re
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import zipfile
from typing import Any

try:
    from . import provision_production_crm_canary_fixture as fixture
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import provision_production_crm_canary_fixture as fixture

common = fixture.common
WORKFLOW = ".github/workflows/bootstrap-production-crm-canary-driver.yml"
PUBLISHER = ".github/workflows/publish-attest-production-crm-canary-driver.yml"
ENVIRONMENT = "rereply-production-canary"
SECRET_NAME = "CRM_CANARY_SYNTHETIC_DRIVER_JSON"
IMAGE = "ghcr.io/medtechcorps-netizen/rereply-crm-canary-driver"
PREDICATE = "https://rereply.app/attestations/production-crm-canary-driver-image/v1"
LEDGER_EXPRESSION = (
    "postgresql://${ledger.USERNAME}:${ledger.PASSWORD}@${ledger.HOSTNAME}:"
    "${ledger.PORT}/${ledger.DATABASE}?sslmode=require&uselibpqcompat=true"
)
EFFECTS = {
    "new_driver_apps": 1, "new_driver_instances": 1,
    "existing_ledger_app_admissions": 1, "ledger_table_initialization": True,
    "github_environment": ENVIRONMENT, "github_secret": SECRET_NAME,
    "github_secret_installations": 1, "production_app_updates": 0,
    "database_creations": 0, "database_user_creations": 0,
    "fixture_mutations": 0, "synthetic_executions": 0,
    "credential_redacted_selected_ledger_metadata_read": True,
}
AUTH_KEYS = {"schema_version", "kind", "control_sha", "operation_id", "issued_at",
             "expires_at", "protected_descriptor_sha256", "plan_sha256", "fixture_evidence",
             "driver_evidence", "effects"}
PLAN_KEYS = {"provider_account_uuid", "app_name", "region", "instance_size_slug",
             "instance_count", "monthly_usd", "ledger", "github_environment_sha256"}
LEDGER_KEYS = {"cluster_id", "cluster_name", "database", "user", "version",
               "firewall_sha256", "dedicated_synthetic_ledger"}
DESCRIPTOR_KEYS = {"schema_version", "control_sha", "operation_id",
                   "public_packet_sha256", "fixture_descriptor_sha256",
                   "hmac_key_base64", "scope_review", "plan"}
FIXTURE_EVIDENCE_KEYS = {"control_sha", "run_id", "artifact_id", "artifact_digest", "result_sha256"}
DRIVER_EVIDENCE_KEYS = {"control_sha", "run_id", "artifact_id", "artifact_digest",
                        "digest", "driver_version_sha256"}
SCOPE_REVIEW = {
    "read": ["account:read", "actions:read", "app:read", "database:read", "regions:read", "sizes:read"],
    "create": ["actions:read", "app:create", "app:read", "database:read",
               "database:update", "regions:read", "sizes:read"],
    "github": ["environments:write"],
    "github_read": ["actions:read", "administration:read", "attestations:read", "contents:read", "environments:read"],
    "ledger_user": "existing-dedicated-database-table-initialization-only",
}
PRIVATE_NAMES = (
    "CRM_CANARY_DRIVER_BOOTSTRAP_JSON", "CRM_CANARY_FIXTURE_INPUT_JSON",
    "DO_DRIVER_BOOTSTRAP_READ_TOKEN", "DO_DRIVER_BOOTSTRAP_CREATE_TOKEN",
    "GH_CANARY_ENVIRONMENT_WRITE_TOKEN",
)
LABEL = re.compile(r"^[a-z][a-z0-9-]{1,62}$")
APP_NAME = re.compile(r"^[a-z][a-z0-9-]{0,30}[a-z0-9]$")
DB_LABEL = re.compile(r"^[a-z][a-z0-9_]{1,62}$")
# The real attested publisher archive contains a ~21 MiB SBOM bundle and
# ~16 MiB SPDX document. Keep a bounded aggregate that admits that exact shape.
MAX_PUBLIC = 64 * 1024 * 1024


def require(ok: bool, message: str) -> None:
    if not ok:
        common.fail(message)


def public_packet_hash(authorization: dict[str, Any]) -> str:
    """Two-pass binding, not a recursive hash: public choices first, private next."""
    return common.sha256_value({k: v for k, v in authorization.items()
                                if k != "protected_descriptor_sha256"})


def validate_authorization(value: Any, *, control_sha: str, now: dt.datetime) -> dict[str, Any]:
    a = common.exact_keys(value, AUTH_KEYS, "bootstrap authorization")
    require(type(a["schema_version"]) is int and a["schema_version"] == 1
            and a["kind"] == "production-crm-canary-driver-bootstrap-v1",
            "bootstrap authorization schema differs")
    require(common.require_sha1(a["control_sha"], "control") == control_sha, "bootstrap control differs")
    common.require_uuid(a["operation_id"], "bootstrap operation")
    common.require_sha256(a["protected_descriptor_sha256"], "protected descriptor digest")
    issued = common.require_timestamp(a["issued_at"], "bootstrap issue time")
    expires = common.require_timestamp(a["expires_at"], "bootstrap expiry")
    require(issued <= now < expires and dt.timedelta(0) < expires - issued <= dt.timedelta(hours=2),
            "bootstrap authorization window differs")
    require(a["effects"] == EFFECTS, "bootstrap effects differ")
    common.require_sha256(a["plan_sha256"], "protected resource plan")
    for field, keys in (("fixture_evidence", FIXTURE_EVIDENCE_KEYS),
                        ("driver_evidence", DRIVER_EVIDENCE_KEYS)):
        e = common.exact_keys(a[field], keys, "bootstrap evidence")
        common.require_sha1(e["control_sha"], "evidence control")
        for key in ("run_id", "artifact_id"):
            common.require_run_id(e[key], "evidence identity")
        common.require_digest(e["artifact_digest"], "evidence artifact")
    common.require_sha256(a["fixture_evidence"]["result_sha256"], "fixture result")
    common.require_digest(a["driver_evidence"]["digest"], "driver image")
    common.require_sha256(a["driver_evidence"]["driver_version_sha256"], "driver input manifest")
    return copy.deepcopy(a)


def validate_plan(value: Any) -> dict[str, Any]:
    p = common.exact_keys(value, PLAN_KEYS, "bootstrap plan")
    common.require_uuid(p["provider_account_uuid"], "provider account")
    common.require_sha256(p["github_environment_sha256"], "reviewed environment metadata")
    require(common.exact_string(p["app_name"], "driver app", APP_NAME).startswith("rereply-canary-driver-"),
            "driver app namespace differs")
    common.exact_string(p["region"], "driver region", LABEL)
    common.exact_string(p["instance_size_slug"], "driver size", re.compile(r"^[a-z0-9.-]{1,64}$"))
    require(type(p["instance_count"]) is int and p["instance_count"] == 1, "driver instance count differs")
    common.exact_string(p["monthly_usd"], "driver monthly cost", re.compile(r"^[1-9][0-9]{0,3}\.[0-9]{2}$"))
    ledger = common.exact_keys(p["ledger"], LEDGER_KEYS, "existing ledger")
    common.require_uuid(ledger["cluster_id"], "ledger cluster")
    common.exact_string(ledger["cluster_name"], "ledger cluster name", LABEL)
    common.exact_string(ledger["database"], "ledger database", DB_LABEL)
    require(ledger["user"] == "crm_canary_driver", "dedicated ledger role differs")
    require(ledger["dedicated_synthetic_ledger"] is True, "dedicated ledger review missing")
    common.exact_string(ledger["version"], "ledger version", re.compile(r"^[1-9][0-9]$"))
    common.require_sha256(ledger["firewall_sha256"], "ledger firewall digest")
    return copy.deepcopy(p)


def validate_descriptor(value: Any, authorization: dict[str, Any]) -> dict[str, Any]:
    d = common.exact_keys(value, DESCRIPTOR_KEYS, "protected bootstrap descriptor")
    require(type(d["schema_version"]) is int and d["schema_version"] == 1
            and d["control_sha"] == authorization["control_sha"]
            and d["operation_id"] == authorization["operation_id"]
            and d["public_packet_sha256"] == public_packet_hash(authorization)
            and common.sha256_value(d) == authorization["protected_descriptor_sha256"],
            "protected bootstrap packet binding differs")
    common.require_sha256(d["fixture_descriptor_sha256"], "fixture runtime binding")
    require(common.sha256_value(validate_plan(d["plan"])) == authorization["plan_sha256"],
            "protected resource plan binding differs")
    require(d["scope_review"] == SCOPE_REVIEW, "purpose-scoped credential review missing")
    try:
        key = base64.b64decode(d["hmac_key_base64"], validate=True)
        require(len(key) == 32 and base64.b64encode(key).decode() == d["hmac_key_base64"],
                "bootstrap HMAC key differs")
    except (ValueError, TypeError):
        common.fail("bootstrap HMAC key differs")
    return copy.deepcopy(d)


def require_unique_run(api: Any, control_sha: str, run_id: str) -> None:
    """Complete exact-control history is the conservative one-shot latch.

    A second dispatch burns this control even if it failed before any mutation.
    This cannot detect deleted history; deleting workflow runs is outside the
    operator contract. The shared workflow concurrency is an additional boundary.
    """
    path = fixture.API_PREFIX + "/actions/workflows/" + WORKFLOW.rsplit("/", 1)[1]
    result = api.pages(path + "/runs?head_sha=" + control_sha, "workflow_runs")
    runs = result["workflow_runs"]
    require(result["total_count"] == 1 and len(runs) == 1, "bootstrap control is not unused")
    run = runs[0]
    require(str(run.get("id")) == run_id and run.get("head_sha") == control_sha
            and run.get("run_attempt") == 1 and run.get("head_branch") == "main"
            and run.get("event") == "workflow_dispatch" and run.get("path") == WORKFLOW
            and run.get("status") in ("queued", "in_progress", "waiting", "pending"),
            "bootstrap run inventory differs")


def runtime_spec(a: dict[str, Any], d: dict[str, Any], protected: dict[str, Any],
                 driver_descriptor: dict[str, Any]) -> dict[str, Any]:
    p, ledger = d["plan"], d["plan"]["ledger"]
    require(common.sha256_value(driver_descriptor) == d["fixture_descriptor_sha256"],
            "rehydrated fixture binding differs")
    values = {
        "CRM_CANARY_FIXTURE_DESCRIPTOR_JSON": common.canonical_payload_bytes(driver_descriptor).decode(),
        "CRM_CANARY_KLINIK_LOGIN_JSON": common.canonical_payload_bytes({
            "email": protected["registration"]["klinik_email"],
            "password": protected["credentials"]["klinik_password"]}).decode(),
        "CRM_CANARY_NON_KLINIK_LOGIN_JSON": common.canonical_payload_bytes({
            "email": protected["registration"]["non_klinik_email"],
            "password": protected["credentials"]["non_klinik_password"]}).decode(),
        "CRM_CANARY_META_APP_SECRET": protected["credentials"]["meta_app_secret"],
        "CRM_CANARY_HMAC_KEY_BASE64": d["hmac_key_base64"],
    }
    return {
        "name": p["app_name"], "region": p["region"],
        "databases": [{"name": "ledger", "engine": "PG", "production": True,
                       "cluster_name": ledger["cluster_name"], "db_name": ledger["database"],
                       "db_user": ledger["user"], "version": ledger["version"]}],
        "services": [{"name": "driver", "instance_count": 1,
                      "instance_size_slug": p["instance_size_slug"], "http_port": 8080,
                      "image": {"registry_type": "GHCR", "registry": "medtechcorps-netizen",
                                "repository": "rereply-crm-canary-driver",
                                "digest": a["driver_evidence"]["digest"],
                                "deploy_on_push": {"enabled": False}},
                      "health_check": {"http_path": "/healthz", "port": 8080},
                      "routes": [{"path": "/"}],
                      "envs": [{"key": key, "value": value, "scope": "RUN_TIME", "type": "SECRET"}
                               for key, value in sorted(values.items())] + [
                          {"key": "CRM_CANARY_DRIVER_VERSION_SHA256", "value": a["driver_evidence"]["driver_version_sha256"],
                           "scope": "RUN_TIME", "type": "GENERAL"},
                          {"key": "CRM_CANARY_LEDGER_DATABASE_URL", "value": LEDGER_EXPRESSION,
                           "scope": "RUN_TIME", "type": "GENERAL"}]}],
    }


def firewall_projection(raw: Any) -> list[dict[str, str]]:
    require(type(raw) is dict and type(raw.get("rules")) is list and 0 < len(raw["rules"]) <= 100,
            "ledger trusted sources are not restricted")
    rules = []
    for rule in raw["rules"]:
        require(type(rule) is dict and set(rule) <= {"uuid", "cluster_uuid", "type", "value", "created_at"},
                "ledger firewall rule fields differ")
        kind, value = rule.get("type"), rule.get("value")
        require(kind in ("app", "ip_addr") and type(value) is str, "ledger firewall rule kind differs")
        if kind == "app":
            common.require_uuid(value, "trusted app")
        else:
            network = ipaddress.ip_network(value, strict=False)
            require(network.prefixlen == network.max_prefixlen and network.is_global,
                    "ledger trusted IP is not one public address")
        rules.append({"type": kind, "value": value})
    require(len({(r["type"], r["value"]) for r in rules}) == len(rules), "duplicate ledger trusted source")
    return sorted(rules, key=lambda r: (r["type"], r["value"]))


def public_receipt(a: dict[str, Any], app_id: str, deployment_id: str, driver_url: str) -> dict[str, Any]:
    """Only fixed labels, IDs and high-entropy/public source hashes escape."""
    return {"schema_version": 1, "kind": "production-crm-canary-driver-bootstrap-result-v1",
            "control_sha": a["control_sha"], "authorization_sha256": common.sha256_value(a),
            "operation_id": a["operation_id"], "app_id_sha256": common.sha256_bytes(app_id.encode()),
            "deployment_id_sha256": common.sha256_bytes(deployment_id.encode()),
            "driver_url_sha256": common.sha256_bytes(driver_url.encode()),
            "driver_version_sha256": a["driver_evidence"]["driver_version_sha256"],
            "image_digest": a["driver_evidence"]["digest"],
            "fixture_result_sha256": a["fixture_evidence"]["result_sha256"],
            "state": "active-health-and-environment-install-observed",
            "synthetic_execution": "not-performed"}


def subprocess_environment(*, token: str | None = None) -> dict[str, str]:
    # No provider/private descriptor, GH_DEBUG, proxy, git config injection or
    # user CLI configuration is inherited by a child process.
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
           "GH_HOST": "github.com", "GH_PROMPT_DISABLED": "1",
           "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}
    for name in ("SYSTEMROOT", "WINDIR", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"):
        if name in os.environ:
            env[name] = os.environ[name]
    if os.environ.get("RUNNER_TEMP"):
        env["HOME"] = os.environ["RUNNER_TEMP"]
        env["GH_CONFIG_DIR"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-empty-gh-config")
        env["XDG_STATE_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-state")
        env["XDG_CACHE_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-cache")
        env["XDG_CONFIG_HOME"] = str(Path(os.environ["RUNNER_TEMP"]) / "bootstrap-config")
    if token is not None:
        env["GH_TOKEN"] = fixture._secret(token)
    return env


class GitHubRead(fixture.GitHubRead):
    """Permit only the exact SHA comparison syntax the producer rejects.

    The pinned producer's generic '..' rejection also rejects GitHub's valid
    comparison separator. Do not alter that producer or broaden arbitrary paths.
    """
    def __init__(self, token: str):
        super().__init__(token)
        self.__comparison_token = fixture._secret(token)

    def get(self, path: str) -> Any:
        if re.fullmatch(re.escape(fixture.API_PREFIX) + r"/compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40}", path):
            return common.loads_strict(fixture._wire(self.opener, "https://api.github.com" + path,
                headers={"Authorization": "Bearer " + self.__comparison_token,
                         "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}, maximum=MAX_PUBLIC))
        return super().get(path)


def driver_manifest(root: Path) -> bytes:
    proc = subprocess.run(["git", "-C", str(root), "ls-tree", "-rz", "--full-tree", "HEAD", "--",
                           "docker/crm-canary-driver.Dockerfile", "frontend/package.json",
                           "frontend/package-lock.json", "frontend/canary-driver"],
                          env=subprocess_environment(), capture_output=True, check=False, timeout=30)
    require(proc.returncode == 0 and len(proc.stdout) < 256 * 1024, "driver tree read failed")
    rows = []
    for entry in proc.stdout.split(b"\0"):
        if not entry:
            continue
        match = re.fullmatch(rb"(100644|100755) blob ([0-9a-f]{40})\t([A-Za-z0-9._/-]+)", entry)
        require(match is not None, "driver source tree differs")
        mode, blob, raw_path = (item.decode() for item in match.groups())
        require(".." not in raw_path and not raw_path.startswith("/"), "driver source path differs")
        path = root / raw_path
        require(path.is_file() and not path.is_symlink(), "driver source type differs")
        raw = path.read_bytes()
        require(hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == blob,
                "driver checkout bytes differ")
        rows.append((raw_path, f"{mode}\t{blob}\t{common.sha256_bytes(raw)}\t{raw_path}\n"))
    names = [name for name, _ in rows]
    require(len(names) >= 4 and len(set(names)) == len(names)
            and {"docker/crm-canary-driver.Dockerfile", "frontend/package.json", "frontend/package-lock.json"}
                <= set(names), "driver input inventory differs")
    return "".join(row for _, row in sorted(rows)).encode()


def publisher_files(raw: bytes) -> dict[str, bytes]:
    expected = {"driver-inputs.tsv", "driver-predicate.json", "image-inspect.json", "image.json",
                "provenance.bundle.json", "remote-descriptor.json", "sbom.bundle.json", "sbom.spdx.json",
                "scan.json", "secret-report.json", "source-binding.bundle.json", "unit-test.txt",
                "vulnerability-report.json"}
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        records = archive.infolist()
        require(len(records) == len(expected) and {r.filename for r in records} == expected,
                "publisher artifact file inventory differs")
        require(sum(r.file_size for r in records) <= MAX_PUBLIC
                and all(not r.is_dir() and not (r.flag_bits & 1)
                        and ((r.external_attr >> 16) & 0o170000) != 0o120000 for r in records),
                "publisher artifact bounds differ")
        return {r.filename: archive.read(r) for r in records}


def authenticate_driver(api: Any, root: Path, gh: Path, evidence: dict[str, Any], current: str,
                        *, gh_token: str) -> None:
    sha, run_id = evidence["control_sha"], str(evidence["run_id"])
    prefix = fixture.API_PREFIX
    comparison = api.get(prefix + "/compare/" + sha + "..." + current)
    require(comparison.get("status") in ("ahead", "identical")
            and comparison.get("merge_base_commit", {}).get("sha") == sha,
            "publisher control is not protected ancestry")
    old = api.get(prefix + "/contents/" + PUBLISHER + "?ref=" + sha)
    new = api.get(prefix + "/contents/" + PUBLISHER + "?ref=" + current)
    require(common.require_sha1(old.get("sha"), "publisher workflow") == new.get("sha"),
            "publisher workflow changed after evidence")
    run = api.get(prefix + "/actions/runs/" + run_id)
    require(str(run.get("id")) == run_id and run.get("head_sha") == sha
            and run.get("head_branch") == "main" and run.get("path") == PUBLISHER
            and run.get("event") == "workflow_dispatch" and run.get("run_attempt") == 1
            and run.get("status") == "completed" and run.get("conclusion") == "success"
            and run.get("repository", {}).get("full_name") == common.REPOSITORY,
            "publisher run is not exact successful authority")
    jobs = api.pages(prefix + "/actions/runs/" + run_id + "/attempts/1/jobs", "jobs")
    names = {"Verify protected-main Test authority", "Build and publish exact CRM canary driver",
             "Scan, SBOM, unit-test, and inspect exact driver", "Attest exact CRM canary driver",
             "Verify exact attestations and anonymous digest pull", "Exact production CRM canary driver image gate"}
    require(jobs["total_count"] == 6 and len(jobs["jobs"]) == 6
            and {j["name"] for j in jobs["jobs"]} == names
            and all(j.get("conclusion") == "success" for j in jobs["jobs"]),
            "publisher job inventory differs")
    path = prefix + "/actions/runs/" + run_id + "/artifacts"
    inventory = api.pages(path, "artifacts")
    expected = {kind + "-crm-canary-driver-" + run_id + "-1"
                for kind in ("attested", "image", "scanned", "verified")}
    require(inventory["total_count"] == 4 and len(inventory["artifacts"]) == 4
            and {a["name"] for a in inventory["artifacts"]} == expected,
            "publisher artifact inventory differs")
    for artifact in inventory["artifacts"]:
        fixture._artifact_record(artifact, artifact["name"], run_id, sha,
                                 common.require_digest(artifact.get("digest"), "publisher artifact digest"))
    artifact = next(a for a in inventory["artifacts"] if a["name"].startswith("attested-"))
    require(str(artifact["id"]) == str(evidence["artifact_id"])
            and artifact["digest"] == evidence["artifact_digest"], "publisher artifact selection differs")
    files = publisher_files(api.artifact(str(artifact["id"]), evidence["artifact_digest"]))
    manifest = driver_manifest(root)
    require(files["driver-inputs.tsv"] == manifest
            and common.sha256_bytes(manifest) == evidence["driver_version_sha256"],
            "published driver inputs differ from current control")
    policy = common.loads_strict(files["driver-predicate.json"])
    require(policy.get("schema_version") == 1 and policy.get("repository") == common.REPOSITORY
            and policy.get("workflow_path") == PUBLISHER and policy.get("workflow_sha") == sha
            and str(policy.get("run_id")) == run_id and policy.get("run_attempt") == 1
            and policy.get("driver_version_sha256") == evidence["driver_version_sha256"]
            and policy.get("subject") == {"image": IMAGE, "digest": evidence["digest"], "platform": "linux/amd64"},
            "publisher signed policy binding differs")
    identity = common.loads_strict(files["image.json"])
    require(policy.get("image_identity") == identity and identity.get("control_sha") == sha
            and identity.get("digest") == evidence["digest"] and identity.get("image") == IMAGE
            and identity.get("platform") == "linux/amd64" and identity.get("tag_is_authority") is False
            and identity.get("driver_version_sha256") == evidence["driver_version_sha256"]
            and "sha256:" + common.sha256_bytes(files["remote-descriptor.json"]) == evidence["digest"]
            and policy.get("verification") == common.loads_strict(files["scan.json"]),
            "publisher image identity differs")
    flags = ["--repo", common.REPOSITORY, "--signer-workflow", common.REPOSITORY + "/" + PUBLISHER,
             "--signer-digest", sha, "--source-digest", sha, "--source-ref", "refs/heads/main",
             "--deny-self-hosted-runners", "--format", "json"]
    with tempfile.TemporaryDirectory(prefix="driver-public-evidence-", dir=os.environ["RUNNER_TEMP"]) as temp:
        for bundle, predicate, expected_policy in (
                ("provenance.bundle.json", "https://slsa.dev/provenance/v1", None),
                ("sbom.bundle.json", "https://spdx.dev/Document/v2.3", common.loads_strict(files["sbom.spdx.json"])),
                ("source-binding.bundle.json", PREDICATE, policy)):
            target = Path(temp) / bundle
            target.write_bytes(files[bundle])  # Public signed evidence only.
            result = subprocess.run([str(gh), "attestation", "verify", "oci://" + IMAGE + "@" + evidence["digest"],
                                     *flags, "--bundle", str(target), "--predicate-type", predicate],
                                    env=subprocess_environment(token=gh_token), capture_output=True,
                                    check=False, timeout=120)
            require(result.returncode == 0 and len(result.stdout) <= MAX_PUBLIC,
                    "driver signature verification failed")
            verified = common.loads_strict(result.stdout)
            require(type(verified) is list and 0 < len(verified) <= 100,
                    "driver signature verification empty")
            if expected_policy is not None:
                require(any(v.get("verificationResult", {}).get("statement", {}).get("predicate") == expected_policy
                            for v in verified), "driver signed predicate differs")
    time.sleep(2)
    require(inventory == api.pages(path, "artifacts"), "publisher artifact inventory changed")
    require(api.get(prefix + "/actions/runs/" + run_id).get("run_attempt") == 1,
            "publisher attempt changed")


def reject_database_credentials(value: Any) -> None:
    """Defense in depth, not token-scope introspection or scope proof.

    Only the independently reviewed database:read credential may reach this
    endpoint. A mis-scoped response fails without printing or persisting it.
    """
    if type(value) is dict:
        for key, child in value.items():
            if any(word in key.lower() for word in ("password", "secret", "token")) or key.lower() in (
                    "uri", "url", "connection_string"):
                require(child in (None, ""), "ledger response contains forbidden credentials")
            reject_database_credentials(child)
    elif type(value) is list:
        for child in value:
            reject_database_credentials(child)


class Provider:
    """One app POST, fixed GET allowlist. No DB write/user API or app update."""
    def __init__(self, plan: dict[str, Any], read_token: str, create_token: str):
        self.plan = copy.deepcopy(plan)
        self.__read_token = fixture._secret(read_token)
        self.__create_token = fixture._secret(create_token)
        require(self.__read_token != self.__create_token, "provider credential roles are not separate")
        self.opener = fixture._opener()
        self.created = False
        self.app_id: str | None = None
        self.deployment_id: str | None = None

    def get(self, path: str) -> Any:
        ledger = self.plan["ledger"]
        fixed = {"/v2/account", "/v2/apps/regions",
                 "/v2/apps/tiers/instance_sizes/" + self.plan["instance_size_slug"],
                 "/v2/databases/" + ledger["cluster_id"],
                 "/v2/databases/" + ledger["cluster_id"] + "/firewall",
                 "/v2/databases/" + ledger["cluster_id"] + "/dbs/" + ledger["database"]}
        if self.app_id is not None:
            fixed.add("/v2/apps/" + self.app_id)
            if self.deployment_id is not None:
                fixed.add("/v2/apps/" + self.app_id + "/deployments/" + self.deployment_id)
        require(path in fixed or re.fullmatch(r"/v2/apps\?page=[1-9][0-9]?&per_page=200", path) is not None,
                "bootstrap provider read route differs")
        raw = fixture._wire(self.opener, common.API_ORIGIN + path,
                            headers={"Authorization": "Bearer " + self.__read_token, "Accept": "application/json"},
                            maximum=MAX_PUBLIC)
        value = common.loads_strict(raw)
        if path.startswith("/v2/databases/"):
            reject_database_credentials(value)
        return value

    def apps(self) -> list[dict[str, str]]:
        rows, total = [], None
        for page in range(1, 100):
            value = self.get(f"/v2/apps?page={page}&per_page=200")
            observed = common.exact_int(value.get("meta", {}).get("total"), "app inventory total", 0, 19800)
            chunk = value.get("apps")
            require(type(chunk) is list and len(chunk) <= 200 and total in (None, observed),
                    "app inventory moved")
            total = observed
            for app in chunk:
                rows.append({"id": common.require_uuid(app.get("id"), "app inventory identity"),
                             "name": common.exact_string(app.get("spec", {}).get("name"), "app inventory name", LABEL)})
            require(len(rows) <= total and len({r["id"] for r in rows}) == len(rows), "app inventory incomplete")
            if len(rows) == total:
                require(not value.get("links", {}).get("pages", {}).get("next"), "excess app page")
                return sorted(rows, key=lambda row: row["id"])
            require(len(chunk) > 0, "app inventory missing page")
        common.fail("app inventory exceeds bound")

    def preflight(self) -> tuple[list[dict[str, str]], list[dict[str, str]]]:
        p, ledger = self.plan, self.plan["ledger"]
        account = self.get("/v2/account").get("account", {})
        require(account.get("uuid") == p["provider_account_uuid"] and account.get("status") == "active",
                "provider account differs")
        size = self.get("/v2/apps/tiers/instance_sizes/" + p["instance_size_slug"]).get("instance_size", {})
        require(size.get("slug") == p["instance_size_slug"] and size.get("usd_per_month") == p["monthly_usd"],
                "driver size or current monthly cost differs")
        regions = self.get("/v2/apps/regions").get("regions")
        require(type(regions) is list, "app region inventory missing")
        matches = [r for r in regions if r.get("slug") == p["region"] and r.get("disabled", False) is False]
        require(len(matches) == 1, "driver region differs")
        cluster = self.get("/v2/databases/" + ledger["cluster_id"]).get("database", {})
        require(cluster.get("id") == ledger["cluster_id"] and cluster.get("name") == ledger["cluster_name"]
                and cluster.get("engine") == "pg" and cluster.get("version") == ledger["version"]
                and cluster.get("status") == "online" and cluster.get("region") in matches[0].get("data_centers", []),
                "selected existing ledger metadata differs")
        require(self.get("/v2/databases/" + ledger["cluster_id"] + "/dbs/" + ledger["database"])
                .get("db") == {"name": ledger["database"]}, "selected ledger database is not existing")
        # User existence/least privilege is independently reviewed in the
        # protected packet. No user-list, user-detail, credential or SQL API.
        rules = firewall_projection(self.get("/v2/databases/" + ledger["cluster_id"] + "/firewall"))
        require(common.sha256_value(rules) == ledger["firewall_sha256"], "ledger firewall prestate differs")
        apps = self.apps()
        require(all(app["name"] != p["app_name"] for app in apps), "driver app already exists")
        return apps, rules

    def create(self, spec: dict[str, Any]) -> tuple[str, str, dict[str, Any]]:
        require(not self.created, "app creation was already attempted")
        self.created = True  # Burn before transport, including timeout/HTTP error.
        raw = fixture._wire(self.opener, common.API_ORIGIN + "/v2/apps", method="POST",
                            headers={"Authorization": "Bearer " + self.__create_token,
                                     "Accept": "application/json", "Content-Type": "application/json"},
                            body=common.canonical_payload_bytes({"spec": spec}), maximum=MAX_PUBLIC)
        app = common.loads_strict(raw).get("app", {})
        self.app_id = common.require_uuid(app.get("id"), "created driver app")
        # Strict acceptance requirement: capture this create's associated
        # deployment, never guess by selecting a later latest deployment.
        self.deployment_id = common.require_uuid(app.get("pending_deployment", {}).get("id"), "created driver deployment")
        require(app.get("owner_uuid") == self.plan["provider_account_uuid"], "created driver owner differs")
        return self.app_id, self.deployment_id, app


def spec_projection(actual: Any, expected: dict[str, Any]) -> dict[str, Any]:
    """Strict public shape plus opaque initial secret ciphertext equality.

    The provider is trusted to store submitted secrets. Readback proves stable
    ciphertext/spec identity, not independent decryption or image measurement.
    """
    observed = copy.deepcopy(actual)
    require(type(observed) is dict, "driver spec missing")
    # Only explicitly benign empty API defaults may be added.
    for key in ("alerts", "domains", "envs", "features", "jobs", "workers", "static_sites", "functions"):
        if observed.get(key) == []:
            observed.pop(key)
    require(set(observed) == set(expected), "driver spec fields differ")
    require(type(observed.get("services")) is list and len(observed["services"]) == 1,
            "driver service inventory differs")
    service = observed["services"][0]
    expected_service = expected["services"][0]
    require(set(service) == set(expected_service), "driver service fields differ")
    envs = service.get("envs")
    require(type(envs) is list and len(envs) == len(expected_service["envs"]), "runtime variable inventory differs")
    by_key = {e.get("key"): e for e in envs if type(e) is dict}
    require(len(by_key) == len(envs), "duplicate runtime variable")
    for env in expected_service["envs"]:
        actual_env = by_key.get(env["key"])
        if type(actual_env) is dict and env["type"] == "GENERAL" and "type" not in actual_env:
            actual_env["type"] = "GENERAL"
        require(type(actual_env) is dict and set(actual_env) == set(env)
                and all(actual_env[k] == env[k] for k in ("key", "scope", "type")),
                "runtime variable custody differs")
        value = actual_env["value"]
        if env["type"] == "SECRET":
            require(type(value) is str and re.fullmatch(r"EV\[[^\r\n]{8,262144}\]", value),
                    "runtime variable storage differs")
        else:
            require(value == env["value"], "public runtime version differs")
    projection = copy.deepcopy(observed)
    projection["services"][0]["envs"] = copy.deepcopy(expected_service["envs"])
    require(projection == expected, "driver spec policy differs")
    service["envs"] = sorted(envs, key=lambda item: item["key"])
    return observed


def driver_origin(app: dict[str, Any], app_name: str) -> str:
    ingress = app.get("default_ingress")
    require(type(ingress) is str, "driver default domain missing")
    parsed = urllib.parse.urlsplit(ingress)
    host = parsed.hostname
    require(parsed.scheme == "https" and parsed.netloc == host and not parsed.path
            and not parsed.query and not parsed.fragment and type(host) is str
            and re.fullmatch(re.escape(app_name) + r"-[a-z0-9]+\.ondigitalocean\.app", host),
            "driver default domain differs")
    origin = "https://" + host
    require(app.get("live_url") in (origin, origin + "/") and app.get("live_domain") == host,
            "driver live URL differs")
    return origin


def health(origin: str) -> None:
    parsed = urllib.parse.urlsplit(origin)
    require(parsed.scheme == "https" and parsed.netloc == parsed.hostname and not parsed.path
            and not parsed.query and not parsed.fragment and parsed.hostname.endswith(".ondigitalocean.app"),
            "driver health origin differs")
    addresses = sorted({row[4][0] for row in socket.getaddrinfo(parsed.hostname, 443, type=socket.SOCK_STREAM)})
    require(0 < len(addresses) <= 16 and all(ipaddress.ip_address(ip).is_global for ip in addresses),
            "driver health address is not public")
    connection = http.client.HTTPSConnection(parsed.hostname, timeout=15, context=ssl.create_default_context())
    try:
        # Pin the already checked address while retaining hostname certificate
        # verification/SNI. Never send auth, follow redirects or call execute.
        connection.sock = connection._context.wrap_socket(
            socket.create_connection((addresses[0], 443), timeout=15), server_hostname=parsed.hostname)
        connection.request("GET", "/healthz", headers={"Host": parsed.hostname, "Accept": "*/*"})
        response = connection.getresponse()
        require(response.status == 204 and response.read(1) == b"", "driver health did not pass")
    finally:
        connection.close()


class EnvironmentWriter:
    def __init__(self, token: str, gh: Path, expected_metadata_sha256: str):
        self.__token = fixture._secret(token)
        self.api = fixture.GitHubRead(self.__token)
        self.gh = gh
        self.attempted = False
        self.expected_metadata_sha256 = common.require_sha256(expected_metadata_sha256, "environment metadata digest")
        self.initial: Any = None

    def snapshot(self) -> dict[str, Any]:
        path = fixture.API_PREFIX + "/environments/" + ENVIRONMENT
        env = self.api.get(path)
        require(env.get("name") == ENVIRONMENT
                and env.get("deployment_branch_policy") == {"protected_branches": False, "custom_branch_policies": True},
                "canary environment protection differs")
        policies = self.api.pages(path + "/deployment-branch-policies", "branch_policies")
        require(policies["total_count"] == 1 and len(policies["branch_policies"]) == 1
                and policies["branch_policies"][0].get("name") == "main"
                and policies["branch_policies"][0].get("type") == "branch", "canary environment is not main-only")
        snapshot = {"environment": {key: env.get(key) for key in (
            "id", "name", "protection_rules", "deployment_branch_policy")},
            "branch_policies": policies["branch_policies"]}
        for kind in ("secrets", "variables"):
            rows, total = [], None
            for page in range(1, 11):
                result = self.api.get(path + f"/{kind}?per_page=100&page={page}")
                count = common.exact_int(result.get("total_count"), "environment metadata count", 0, 1000)
                chunk = result.get(kind)
                require(total in (None, count) and type(chunk) is list and len(chunk) <= 100,
                        "environment metadata inventory differs")
                total = count
                rows.extend(chunk)
                for row in rows:
                    common.exact_keys(row, {"name", "created_at", "updated_at"} | ({"value"} if kind == "variables" else set()),
                                      "environment metadata record")
                names = [row["name"] for row in rows]
                require(len(names) == len(set(names)) and all(type(name) is str for name in names)
                        and len(rows) <= total, "environment metadata is ambiguous")
                if len(rows) == total:
                    break
                require(chunk, "environment metadata inventory incomplete")
            else:
                common.fail("environment metadata inventory exceeds bound")
            snapshot[kind] = sorted(rows, key=lambda row: row["name"])
        return snapshot

    def require_absent(self) -> None:
        value = self.snapshot()
        require({row["name"] for row in value["secrets"]}
                == {"CRM_CANARY_PUBLIC_TARGETS_JSON", "REREPLY_APPLY_READ_PARITY"},
                "canary preexisting secret names differ")
        require(common.sha256_value(value) == self.expected_metadata_sha256,
                "reviewed environment metadata differs")
        if self.initial is None:
            self.initial = value
        require(value == self.initial, "environment metadata changed before installation")

    def install(self, config: dict[str, Any]) -> None:
        require(not self.attempted, "environment installation was already attempted")
        common.exact_keys(config, {"schema_version", "url", "driver_version_sha256", "fixture_descriptor_sha256", "hmac_key_base64"},
                          "driver configuration")
        self.require_absent()
        public_key_path = fixture.API_PREFIX + "/environments/" + ENVIRONMENT + "/secrets/public-key"
        public_key = self.api.get(public_key_path)
        common.exact_keys(public_key, {"key_id", "key"}, "environment public key")
        require(type(public_key["key_id"]) is str and re.fullmatch(r"[0-9]+", public_key["key_id"])
                and len(base64.b64decode(public_key["key"], validate=True)) == 32, "environment public key differs")
        # Pinned gh v2.98.0 setSecret returns BEFORE putEnvSecret when
        # --no-store is set. It seals stdin locally; no mutation is delegated.
        # github.com/cli/cli/blob/v2.98.0/pkg/cmd/secret/set/set.go#L328-L330
        result = subprocess.run([str(self.gh), "secret", "set", SECRET_NAME, "--env", ENVIRONMENT,
                                 "--repo", common.REPOSITORY, "--no-store"],
                                input=common.canonical_payload_bytes(config), env=subprocess_environment(token=self.__token),
                                capture_output=True, timeout=90, check=False)
        require(result.returncode == 0 and len(result.stdout) <= 16384, "environment secret sealing failed")
        encrypted = result.stdout.strip().decode("ascii")
        require(len(base64.b64decode(encrypted, validate=True)) == len(common.canonical_payload_bytes(config)) + 48,
                "environment sealed value length differs")
        require(self.api.get(public_key_path) == public_key, "environment encryption key changed")
        self.require_absent()
        self.attempted = True  # Burn before exactly one explicit, nonretry PUT.
        fixture._wire(fixture._opener(), "https://api.github.com" + fixture.API_PREFIX
                      + "/environments/" + ENVIRONMENT + "/secrets/" + SECRET_NAME, method="PUT",
                      headers={"Authorization": "Bearer " + self.__token, "Accept": "application/vnd.github+json",
                               "Content-Type": "application/json", "X-GitHub-Api-Version": "2022-11-28"},
                      body=common.canonical_payload_bytes({"key_id": public_key["key_id"], "encrypted_value": encrypted}),
                      maximum=4096)
        after = self.snapshot()
        additions = [row for row in after["secrets"] if row["name"] == SECRET_NAME]
        require(len(additions) == 1, "environment installation metadata absent")
        after["secrets"] = [row for row in after["secrets"] if row["name"] != SECRET_NAME]
        require(after == self.initial, "environment metadata changed during installation")
        # GitHub intentionally provides no secret-value readback. Successful
        # encrypted write plus metadata is not a runtime synthetic probe.


def observe_created(provider: Any, spec: dict[str, Any], initial_spec: dict[str, Any],
                    before_apps: list[dict[str, str]], before_rules: list[dict[str, str]]) -> str | None:
    app_id, deployment_id = provider.app_id, provider.deployment_id
    app = provider.get("/v2/apps/" + app_id).get("app", {})
    dep = provider.get("/v2/apps/" + app_id + "/deployments/" + deployment_id).get("deployment", {})
    require(app.get("id") == app_id and app.get("owner_uuid") == provider.plan["provider_account_uuid"]
            and dep.get("id") == deployment_id, "created driver identity changed")
    require(spec_projection(app.get("spec"), spec) == initial_spec, "created driver spec changed")
    phase = dep.get("phase")
    require(phase in ("PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING", "ACTIVE"),
            "created driver deployment did not progress safely")
    if phase != "ACTIVE":
        return None
    require(app.get("active_deployment", {}).get("id") == deployment_id
            and spec_projection(dep.get("spec"), spec) == initial_spec,
            "active driver deployment differs")
    for key in ("in_progress_deployment", "pending_deployment", "pinned_deployment"):
        value = app.get(key)
        require(not value or (value.get("id") == deployment_id and value.get("phase") == "ACTIVE"),
                "competing driver deployment exists")
    expected_apps = sorted(before_apps + [{"id": app_id, "name": provider.plan["app_name"]}], key=lambda a: a["id"])
    require(provider.apps() == expected_apps, "app inventory after creation differs")
    rules = firewall_projection(provider.get("/v2/databases/" + provider.plan["ledger"]["cluster_id"] + "/firewall"))
    expected_rules = sorted(before_rules + [{"type": "app", "value": app_id}], key=lambda r: (r["type"], r["value"]))
    require(rules == expected_rules, "automatic ledger admission was not exactly observed")
    return driver_origin(app, provider.plan["app_name"])


def install_once(a: dict[str, Any], d: dict[str, Any], protected: Any, *, provider: Any, writer: Any,
                 authenticate_fixture: Any, authenticate_image: Any, current_guard: Any,
                 rehydrate: Any, transport: Any, sleep: Any = time.sleep, probe: Any = health,
                 now: Any = lambda: dt.datetime.now(dt.timezone.utc)) -> dict[str, Any]:
    """Injected boundaries are tests only; CLI binds fixed real adapters below."""
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    validate_descriptor(d, a)
    current_guard()
    writer.require_absent()
    # Genuine signature/artifact checks must finish before any fixture login.
    raw = authenticate_fixture()
    require(common.sha256_bytes(raw) == a["fixture_evidence"]["result_sha256"], "authenticated fixture file differs")
    result = fixture.validate_terminal_result(common.loads_strict(raw))
    require(result["fixture_descriptor_sha256"] == d["fixture_descriptor_sha256"], "fixture runtime authority differs")
    authenticate_image()
    request = {key: result[key] for key in fixture.REQUEST_KEYS}
    checked = fixture.validate_protected_input(protected, request)
    before_apps, before_rules = provider.preflight()
    reconstructed = rehydrate(request, checked, result, transport)
    spec = runtime_spec(a, d, checked, reconstructed)
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    writer.require_absent()
    require(provider.preflight() == (before_apps, before_rules), "driver prestate changed before creation")
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    _, _, app = provider.create(spec)
    initial = spec_projection(app.get("spec"), spec)
    origin = None
    for _ in range(30):
        validate_authorization(a, control_sha=a["control_sha"], now=now())
        origin = observe_created(provider, spec, initial, before_apps, before_rules)
        if origin is not None:
            break
        sleep(10)
    require(origin is not None, "driver did not become ACTIVE within bound")
    probe(origin)
    sleep(30)
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    require(observe_created(provider, spec, initial, before_apps, before_rules) == origin,
            "driver was not stably ACTIVE")
    probe(origin)
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    writer.install({"schema_version": 1, "url": origin + "/v1/execute",
                    "driver_version_sha256": a["driver_evidence"]["driver_version_sha256"],
                    "fixture_descriptor_sha256": d["fixture_descriptor_sha256"],
                    "hmac_key_base64": d["hmac_key_base64"]})
    current_guard()
    validate_authorization(a, control_sha=a["control_sha"], now=now())
    return public_receipt(a, provider.app_id, provider.deployment_id, origin)


def current_guard(api: Any, root: Path) -> str:
    sha = fixture._current_guard(api, root, workflow=WORKFLOW)
    branch = api.get(fixture.API_PREFIX + "/branches/main")
    require(branch.get("protected") is True and branch.get("commit", {}).get("sha") == sha,
            "bootstrap main is not protected")
    require_unique_run(api, sha, common.require_run_id(os.environ.get("GITHUB_RUN_ID"), "bootstrap run"))
    return sha


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("validate", "install"))
    parser.add_argument("--control-root", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args(argv)
    try:
        # Remove private values before any subprocess/imported adapter can run.
        private = {name: os.environ.pop(name, None) for name in PRIVATE_NAMES}
        raw = os.environ.get("BOOTSTRAP_AUTHORIZATION_JSON")
        require(type(raw) is str and len(raw.encode()) <= 32768, "public authorization missing")
        sha = common.require_sha1(os.environ.get("CONTROL_SHA"), "bootstrap control")
        a = validate_authorization(common.loads_strict(raw), control_sha=sha, now=dt.datetime.now(dt.timezone.utc))
        require(raw.encode() == common.canonical_payload_bytes(a), "public authorization is not canonical")
        token = fixture._secret(os.environ.get("GH_TOKEN"))
        if args.command == "install":
            require(all(type(private[n]) is str and private[n] for n in PRIVATE_NAMES), "protected setup inputs missing")
            require(len(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"].encode()) <= 32768
                    and len(private["CRM_CANARY_FIXTURE_INPUT_JSON"].encode()) <= fixture.MAX_BODY_BYTES,
                    "protected setup input exceeds bound")
            d = validate_descriptor(common.loads_strict(private["CRM_CANARY_DRIVER_BOOTSTRAP_JSON"]), a)
            protected = common.loads_strict(private["CRM_CANARY_FIXTURE_INPUT_JSON"])
            require(args.output_dir is not None, "public receipt output missing")
            output = args.output_dir.resolve()
            runner_temp = Path(os.environ["RUNNER_TEMP"]).resolve(strict=True)
            require(output.parent == runner_temp and not output.exists(), "public receipt location differs")
        else:
            require(not any(private.values()) and args.output_dir is None, "public validation received private authority")
        root = args.control_root.resolve(strict=True)
        api = GitHubRead(token)
        current_guard(api, root)
        if args.command == "validate":
            print(common.canonical_payload_bytes({"state": "public-authority-validated",
                                                 "authorization_sha256": common.sha256_value(a)}).decode())
            return 0
        # Import the new public verifier, not the pinned producer's
        # verify_fixture_result wrapper (which requires an existing runtime).
        try:
            from . import launch_production_prerequisites as prerequisites
        except ImportError:
            import launch_production_prerequisites as prerequisites
        gh = fixture._pinned_gh()
        reader = prerequisites.GitHubEvidence(root, transport=prerequisites.GitHubGetOnly(gh=str(gh)))
        provider = Provider(d["plan"], private["DO_DRIVER_BOOTSTRAP_READ_TOKEN"], private["DO_DRIVER_BOOTSTRAP_CREATE_TOKEN"])
        writer = EnvironmentWriter(private["GH_CANARY_ENVIRONMENT_WRITE_TOKEN"], gh, d["plan"]["github_environment_sha256"])
        transport = fixture.ProductHTTP(protected["credentials"]["meta_access_token"])
        receipt = install_once(a, d, protected, provider=provider, writer=writer,
            authenticate_fixture=lambda: reader.authenticate_public_fixture(common.canonical_payload_bytes(a["fixture_evidence"]), sha),
            authenticate_image=lambda: authenticate_driver(api, root, gh, a["driver_evidence"], sha, gh_token=token),
            current_guard=lambda: current_guard(api, root), rehydrate=fixture.rehydrate, transport=transport)
        output.mkdir(mode=0o700, parents=False, exist_ok=False)
        receipt_bytes = common.canonical_file_bytes(receipt)
        (output / "receipt.json").write_bytes(receipt_bytes)
        (output / "receipt.sha256").write_text(common.sha256_bytes(receipt_bytes) + "\n", encoding="ascii")
        print(common.canonical_payload_bytes(receipt).decode())
        return 0
    except Exception:
        # Never include exception/provider/CLI text: it may contain private data.
        print("driver bootstrap stopped; no retry or further mutation is authorized", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
