"""Shared fakes for the Release (ship.yml) tests. No TestCase lives here.

Every identifier is synthetic: RFC 4122 UUIDs made of repeated digits, hosts
under example.invalid, and obviously fake credentials.
"""

from __future__ import annotations

import base64
import copy
import datetime as dt
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common


REPO = common.REPOSITORY
APP_ID = "11111111-1111-4111-8111-111111111111"
PG_ID = "22222222-2222-4222-8222-222222222222"
ACTIVE_ID = "33333333-3333-4333-8333-333333333333"
VPC_ID = "44444444-4444-4444-8444-444444444444"
NEW_DEPLOYMENT_IDS = (
    "55555555-5555-4555-8555-555555555555",
    "66666666-6666-4666-8666-666666666666",
    "77777777-7777-4777-8777-777777777777",
    "88888888-8888-4888-8888-888888888888",
)
CLUSTER_NAME = "fake-pg-cluster"
INGRESS = "https://rereply-fake.example.invalid"
# Deliberately not shaped like any real provider token (secret scanners).
DO_TOKEN = "fake-do-token-" + "z" * 40
GH_TOKEN = "fake-github-token-0000000000"
FAKE_PASSWORD = "fake-db-password-should-never-leak"
FAKE_ENV_SECRET = "EV[1:fake:ciphertext-should-never-leak]"
FAKE_ENV_VALUE = "plain-env-value-should-never-leak"
LEGACY_WORKFLOW_SHA = "9" * 40
GH_VERSION = "2.98.0"
NOW = dt.datetime(2026, 10, 4, 12, 0, 0, tzinfo=dt.timezone.utc)
RUN_ID = "4242"
PRIVATE_VALUES = (
    APP_ID, PG_ID, ACTIVE_ID, VPC_ID, *NEW_DEPLOYMENT_IDS, CLUSTER_NAME, INGRESS,
    "rereply-fake.example.invalid", DO_TOKEN, FAKE_PASSWORD, FAKE_ENV_SECRET, FAKE_ENV_VALUE,
)
OLD_GROUP_LINE = "  group: " + "rereply" + "-production"
FORBIDDEN_FRAGMENT = "live" + "-evidence"


def digest(character: str) -> str:
    return "sha256:" + character * 64


LIVE = {"web": digest("a"), "meta-relay": digest("b"), "gmail-relay": digest("c")}
NEW = {"web": digest("d"), "meta-relay": digest("e"), "gmail-relay": digest("f")}
OTHER = {"web": digest("1"), "meta-relay": digest("2"), "gmail-relay": digest("3")}


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def canonical(value: Any) -> bytes:
    return common.canonical_file_bytes(value)


def make_target() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "app_name": "rereply",
        "region": "sgp",
        "app_id_sha256": sha256_text(APP_ID),
        "default_ingress_sha256": sha256_text(INGRESS),
        "vpc_id_sha256": sha256_text(VPC_ID),
        "postgres": {"cluster_name_sha256": sha256_text(CLUSTER_NAME), "version": "17", "region": "sgp1"},
        "services": {
            "omnitech-web": {"http_port": 8080, "health_path": "/ready"},
            "meta-relay": {"http_port": 8081, "health_path": "/readyz"},
            "gmail-relay": {"http_port": 8082, "health_path": "/readyz"},
        },
        "pre_deploy_job": {"name": "rereply-rls-migrate", "run_command": "./rereply rls-migrate -config config.toml"},
    }


def image_selector(component: str, value: str) -> dict[str, str]:
    return {
        "registry_type": "GHCR",
        "registry": "ghcr.io",
        "repository": f"medtechcorps-netizen/rereply-release-{component}",
        "digest": value,
    }


def make_spec(images: Mapping[str, str] = LIVE) -> dict[str, Any]:
    return {
        "name": "rereply",
        "region": "sgp",
        "vpc": {"id": VPC_ID},
        "envs": [
            {"key": "APP_SECRET", "value": FAKE_ENV_SECRET, "type": "SECRET", "scope": "RUN_TIME"},
            {"key": "APP_PLAIN", "value": FAKE_ENV_VALUE},
        ],
        "services": [
            {
                "name": "omnitech-web",
                "http_port": 8080,
                "health_check": {"http_path": "/ready"},
                "instance_count": 1,
                "image": image_selector("web", images["web"]),
                "envs": [{"key": "WEB_ONLY", "value": "web-value", "scope": "RUN_TIME"}],
            },
            {
                "name": "meta-relay",
                "http_port": 8081,
                "health_check": {"http_path": "/readyz"},
                "image": image_selector("meta-relay", images["meta-relay"]),
                "envs": [],
            },
            {
                "name": "gmail-relay",
                "http_port": 8082,
                "health_check": {"http_path": "/readyz"},
                "image": image_selector("gmail-relay", images["gmail-relay"]),
            },
        ],
        "jobs": [
            {
                "name": "rereply-rls-migrate",
                "kind": "PRE_DEPLOY",
                "run_command": "./rereply rls-migrate -config config.toml",
                "image": image_selector("web", images["web"]),
                "envs": [{"key": "JOB_SECRET", "value": "EV[job]", "type": "SECRET"}],
            }
        ],
        "databases": [
            {"name": "db-runtime", "engine": "PG", "version": "17", "production": True, "cluster_name": CLUSTER_NAME},
            {"name": "db-owner", "engine": "PG", "version": "17", "production": True, "cluster_name": CLUSTER_NAME},
            {"name": "cache", "engine": "VALKEY", "version": "8", "production": True, "cluster_name": "fake-valkey"},
        ],
        "ingress": {"rules": [{"match": {"path": {"prefix": "/"}}, "component": {"name": "omnitech-web"}}]},
        "domains": [{"domain": "app.example.invalid", "type": "PRIMARY"}],
    }


def migration_progress(status: str) -> dict[str, Any]:
    """The progress tree shape DigitalOcean reports (see test_apply_production_change.py)."""
    return {
        "steps": [
            {"name": "build", "status": "SUCCESS", "steps": [{"name": "components", "status": "SUCCESS", "steps": []}]},
            {
                "name": "deploy",
                "status": "SUCCESS" if status == "SUCCESS" else "ERROR",
                "steps": [
                    {
                        "name": "components",
                        "status": "SUCCESS" if status == "SUCCESS" else "ERROR",
                        "steps": [
                            {
                                "name": "rereply-rls-migrate",
                                "status": status,
                                "component_name": "rereply-rls-migrate",
                                "steps": [
                                    {"name": "deploy", "status": status, "component_name": "rereply-rls-migrate"},
                                    {"name": "wait", "status": status, "component_name": "rereply-rls-migrate"},
                                ],
                            }
                        ],
                    }
                ],
            },
        ]
    }


def deployment_object(
    deployment_id: str,
    spec: Mapping[str, Any],
    phase: str,
    *,
    migration_status: str = "SUCCESS",
    source_image_digest: str | None = None,
) -> dict[str, Any]:
    web = [item for item in spec["jobs"] if item["name"] == "rereply-rls-migrate"][0]["image"]["digest"]
    return {
        "id": deployment_id,
        "phase": phase,
        "spec": copy.deepcopy(dict(spec)),
        "jobs": [{"name": "rereply-rls-migrate", "source_image_digest": source_image_digest or web}],
        "progress": migration_progress(migration_status),
    }


class FakeResponse:
    def __init__(self, raw: bytes, url: str, *, status: int = 200, content_type: str = "application/json") -> None:
        self.raw = raw
        self.url = url
        self.status = status
        self.headers = {"Content-Type": content_type}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def getcode(self) -> int:
        return self.status

    def read(self, amount: int = -1) -> bytes:
        return self.raw if amount < 0 else self.raw[:amount]


def http_error(url: str, code: int) -> urllib.error.HTTPError:
    return urllib.error.HTTPError(url, code, "fake", {}, None)


class FakeDO:
    """A stateful fake of the four DigitalOcean paths ship.yml may reach.

    PUT scenarios (one consumed per PUT): accept, error, cancel,
    reject-<code>, ambiguous-applied, ambiguous-not-applied, stall.
    """

    def __init__(
        self,
        *,
        spec: Mapping[str, Any] | None = None,
        scenarios: Iterable[str] = ("accept",),
        ticks: int = 2,
        lag: int = 0,
        pinned: bool = False,
        migration_status: str = "SUCCESS",
        migration_digest: str | None = None,
        backup_age_hours: float = 2,
        backups: list[dict[str, Any]] | None = None,
        database: dict[str, Any] | None = None,
    ) -> None:
        self.spec = copy.deepcopy(dict(spec or make_spec()))
        self.deployments: dict[str, dict[str, Any]] = {
            ACTIVE_ID: deployment_object(ACTIVE_ID, self.spec, "ACTIVE")
        }
        self.active = ACTIVE_ID
        self.in_progress: str | None = None
        self.hidden_in_progress: str | None = None
        self.lag_remaining = 0
        self.lag = lag
        self.pending: str | None = None
        self.pinned = ACTIVE_ID if pinned else None
        self.updated_at = "2026-10-04T11:00:00Z"
        self.scenarios = list(scenarios)
        self.ticks = ticks
        self.remaining: dict[str, int] = {}
        self.outcome: dict[str, str] = {}
        self.new_ids = list(NEW_DEPLOYMENT_IDS)
        self.migration_status = migration_status
        self.migration_digest = migration_digest
        # Per new deployment, consumed in order; empty means the defaults.
        self.migration_plan: list[str] = []
        self.digest_plan: list[str | None] = []
        self.requests: list[tuple[str, str]] = []
        self.put_bodies: list[dict[str, Any]] = []
        self.headers: list[dict[str, str]] = []
        self.get_hooks: list[Callable[["FakeDO", str], None]] = []
        created = (NOW - dt.timedelta(hours=backup_age_hours)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.backups = backups if backups is not None else [
            {"created_at": "2026-09-30T02:00:00Z", "size_gigabytes": 0.0213},
            {"created_at": created, "size_gigabytes": 0.0328},
        ]
        self.database = database or {
            "id": PG_ID,
            "name": CLUSTER_NAME,
            "engine": "pg",
            "version": "17",
            "region": "sgp1",
            "status": "online",
            "size": "db-s-1vcpu-1gb",
            "num_nodes": 1,
            "created_at": "2026-01-01T00:00:00Z",
            "connection": {
                "uri": f"postgresql://doadmin:{FAKE_PASSWORD}@db.example.invalid:25060/defaultdb",
                "password": FAKE_PASSWORD,
            },
            "users": [{"name": "doadmin", "password": FAKE_PASSWORD}],
        }

    # -- helpers ---------------------------------------------------------

    def put_count(self) -> int:
        return sum(1 for method, _ in self.requests if method == "PUT")

    def _deployment_summary(self, deployment_id: str | None) -> dict[str, Any] | None:
        if deployment_id is None:
            return None
        deployment = self.deployments[deployment_id]
        return {"id": deployment_id, "phase": deployment["phase"], "spec": copy.deepcopy(deployment["spec"])}

    def app_envelope(self) -> dict[str, Any]:
        return {
            "app": {
                "id": APP_ID,
                "updated_at": self.updated_at,
                "default_ingress": INGRESS,
                "spec": copy.deepcopy(self.spec),
                "active_deployment": self._deployment_summary(self.active),
                "in_progress_deployment": self._deployment_summary(self.in_progress),
                "pending_deployment": self._deployment_summary(self.pending),
                "pinned_deployment": ({"id": self.pinned} if self.pinned else None),
            }
        }

    def _advance(self) -> None:
        if self.hidden_in_progress is not None:
            self.lag_remaining -= 1
            if self.lag_remaining <= 0:
                self.in_progress = self.hidden_in_progress
                self.hidden_in_progress = None
            return
        if self.in_progress is None:
            return
        identity = self.in_progress
        if self.outcome[identity] == "stall":
            return
        self.remaining[identity] -= 1
        if self.remaining[identity] > 0:
            self.deployments[identity]["phase"] = "DEPLOYING"
            return
        self.in_progress = None
        outcome = self.outcome[identity]
        if outcome == "accept":
            self.deployments[self.active]["phase"] = "SUPERSEDED"
            self.deployments[identity]["phase"] = "ACTIVE"
            self.active = identity
            self.updated_at = "2026-10-04T11:30:00Z"
        else:
            self.deployments[identity]["phase"] = "ERROR" if outcome == "error" else "CANCELED"

    def _apply(self, spec: Mapping[str, Any], outcome: str) -> None:
        identity = self.new_ids.pop(0)
        self.spec = copy.deepcopy(dict(spec))
        self.updated_at = f"2026-10-04T11:1{len(self.put_bodies)}:00Z"
        self.deployments[identity] = deployment_object(
            identity,
            spec,
            "PENDING_DEPLOY",
            migration_status=self.migration_plan.pop(0) if self.migration_plan else self.migration_status,
            source_image_digest=self.digest_plan.pop(0) if self.digest_plan else self.migration_digest,
        )
        self.remaining[identity] = self.ticks
        self.outcome[identity] = outcome
        if self.lag:
            self.hidden_in_progress = identity
            self.lag_remaining = self.lag
        else:
            self.in_progress = identity

    # -- urllib opener protocol -------------------------------------------

    def open(self, request: Any, timeout: float | None = None) -> FakeResponse:
        del timeout
        url = request.full_url
        method = request.get_method()
        parsed = urllib.parse.urlsplit(url)
        path = parsed.path + (f"?{parsed.query}" if parsed.query else "")
        self.requests.append((method, path))
        self.headers.append({key.lower(): value for key, value in request.header_items()})
        if parsed.scheme != "https" or parsed.hostname != "api.digitalocean.com":
            raise AssertionError("fake provider reached an unexpected origin")
        if method == "GET":
            for hook in list(self.get_hooks):
                hook(self, path)
            return self._get(path, url)
        if method == "PUT":
            return self._put(path, url, request.data)
        raise AssertionError("unexpected method")

    def _get(self, path: str, url: str) -> FakeResponse:
        if path == f"/v2/apps/{APP_ID}":
            self._advance()
            return FakeResponse(json.dumps(self.app_envelope()).encode(), url)
        prefix = f"/v2/apps/{APP_ID}/deployments/"
        if path.startswith(prefix):
            identity = path[len(prefix):]
            if identity not in self.deployments:
                raise http_error(url, 404)
            return FakeResponse(json.dumps({"deployment": self.deployments[identity]}).encode(), url)
        if path == f"/v2/databases/{PG_ID}":
            return FakeResponse(json.dumps({"database": self.database}).encode(), url)
        if path == f"/v2/databases/{PG_ID}/backups?page=1&per_page=200":
            return FakeResponse(json.dumps({"backups": self.backups, "links": {}, "meta": {"total": len(self.backups)}}).encode(), url)
        raise AssertionError("fake provider path is not allowlisted")

    def _put(self, path: str, url: str, data: bytes) -> FakeResponse:
        if path != f"/v2/apps/{APP_ID}":
            raise AssertionError("PUT outside the app path")
        body = json.loads(data.decode("utf-8"))
        self.put_bodies.append(body)
        scenario = self.scenarios.pop(0) if self.scenarios else "accept"
        if scenario.startswith("reject-"):
            raise http_error(url, int(scenario.split("-", 1)[1]))
        if scenario == "ambiguous-not-applied":
            raise http_error(url, 500)
        if scenario == "timeout-not-applied":
            raise TimeoutError("fake timeout")
        outcome = {"accept": "accept", "error": "error", "cancel": "cancel", "stall": "stall",
                   "ambiguous-applied": "accept", "timeout-applied": "accept"}[scenario]
        self._apply(body["spec"], outcome)
        if scenario == "ambiguous-applied":
            raise http_error(url, 502)
        if scenario == "timeout-applied":
            raise TimeoutError("fake timeout")
        return FakeResponse(json.dumps(self.app_envelope()).encode(), url)


class FakeHttps:
    """The smoke probe transport: statuses per path, optionally scripted."""

    def __init__(self, *, failing: bool = False, script: list[bool] | None = None) -> None:
        self.failing = failing
        self.script = list(script or [])
        self.calls: list[str] = []
        self.expected = {
            "/health": 200, "/ready": 200, "/meta-relay/livez": 204, "/meta-relay/readyz": 204,
            "/gmail-relay/livez": 204, "/gmail-relay/readyz": 204,
        }

    def __call__(self, url: str, **kwargs: Any) -> tuple[int, dict[str, str], bytes]:
        del kwargs
        self.calls.append(url)
        if not url.startswith(INGRESS + "/"):
            raise AssertionError("smoke probe left the reviewed origin")
        path = url[len(INGRESS):]
        healthy = not self.failing
        if self.script and path == "/health":
            healthy = self.script.pop(0)
        if not healthy:
            return 503, {}, b""
        return self.expected[path], {}, b""


# --------------------------------------------------------------------------
# Fake gh
# --------------------------------------------------------------------------


def run_record(path: str, *, status: str = "completed", conclusion: str | None = "success", sha: str = "0" * 40,
               event: str = "push", branch: str = "main", run_id: int = 1) -> dict[str, Any]:
    return {
        "id": run_id, "path": path, "status": status, "conclusion": conclusion,
        "head_sha": sha, "event": event, "head_branch": branch,
    }


class FakeGh:
    """A subprocess.run stand-in for the pinned gh binary."""

    def __init__(self, *, version: str = GH_VERSION) -> None:
        self.version = version
        self.calls: list[tuple[list[str], dict[str, str]]] = []
        self.releases: list[dict[str, Any]] = []
        self.tags: list[dict[str, Any]] = []
        self.assets: dict[str, bytes] = {}
        self.attested_files: dict[str, str] = {}
        self.image_attestations: dict[tuple[str, str], list[dict[str, Any]]] = {}
        self.ci_runs: dict[str, list[dict[str, Any]]] = {"test.yml": [], "e2e-tests.yml": []}
        self.active: dict[str, list[dict[str, Any]]] = {}
        self.fail_commands: set[str] = set()
        self.writes: list[list[str]] = []
        self.next_release_id = 100
        self.drafts_hidden = False
        # The owner-only approval gate, configured as docs/release.md asks.
        self.environment: dict[str, Any] = {
            "id": 9001, "name": "production", "can_admins_bypass": False,
            "protection_rules": [
                {"id": 1, "type": "required_reviewers", "prevent_self_review": False,
                 "reviewers": [{"type": "User", "reviewer": {"login": "medtechcorps-netizen", "id": 7}}]},
                {"id": 2, "type": "branch_policy"},
            ],
            "deployment_branch_policy": {"protected_branches": False, "custom_branch_policies": True},
        }
        self.branch_policies: list[dict[str, Any]] = [{"id": 3, "name": "main", "type": "branch"}]
        self.approvals: dict[str, list[dict[str, Any]]] = {
            RUN_ID: [{"state": "approved", "comment": "", "user": {"login": "medtechcorps-netizen", "id": 7},
                      "environments": [{"id": 9001, "name": "production"}]}],
        }

    # -- scenario builders --------------------------------------------------

    def ci_green(self, sha: str) -> None:
        for workflow in ("test.yml", "e2e-tests.yml"):
            self.ci_runs[workflow] = [run_record(f".github/workflows/{workflow}", sha=sha)]

    def attest_ship_images(self, images: Mapping[str, str], sha: str, *, sbom: Any = None) -> None:
        for component, value in images.items():
            image = common.IMAGE_REPOSITORY[component]
            entries = self.image_attestations.setdefault((image, value), [])
            entries.append({"workflow": ".github/workflows/ship.yml", "signer": sha, "source": sha,
                            "predicate_type": "https://slsa.dev/provenance/v1", "predicate": {"buildType": "fake"}})
            entries.append({"workflow": ".github/workflows/ship.yml", "signer": sha, "source": sha,
                            "predicate_type": "https://spdx.dev/Document/v2.3",
                            "predicate": sbom if sbom is not None else {"spdxVersion": "SPDX-2.3"}})

    def attest_legacy_images(self, images: Mapping[str, str], source_commit: str, *, workflow_sha: str = LEGACY_WORKFLOW_SHA) -> None:
        for component, value in images.items():
            image = common.IMAGE_REPOSITORY[component]
            entries = self.image_attestations.setdefault((image, value), [])
            base = {"workflow": ".github/workflows/build-attest-exact-release-images.yml",
                    "signer": workflow_sha, "source": workflow_sha}
            entries.append({**base, "predicate_type": "https://slsa.dev/provenance/v1", "predicate": {"buildType": "fake"}})
            entries.append({
                **base,
                "predicate_type": "https://rereply.app/attestations/exact-release-image/v1",
                "predicate": {
                    "image": {"component": component, "tag_is_authority": False},
                    "source": {"commit": source_commit},
                    "phase": "ui",
                    "builder": {"workflow_sha": workflow_sha},
                },
            })

    def add_record(self, manifest: Mapping[str, Any], *, attested: bool = True, tag_sha: str | None = None,
                   draft: bool = False, prerelease: bool = False, asset_name: str = "release-manifest.json",
                   raw: bytes | None = None, with_tag: bool = True) -> bytes:
        data = raw if raw is not None else canonical(dict(manifest))
        tag = manifest["release"]
        self.next_release_id += 1
        self.releases.append({
            "id": self.next_release_id, "tag_name": tag, "draft": draft, "prerelease": prerelease,
            "target_commitish": manifest["sha"],
            "assets": [{"id": self.next_release_id * 10, "name": asset_name, "size": len(data), "state": "uploaded"}],
        })
        self.assets[tag] = data
        if with_tag and not draft:
            self.tags.append({"ref": f"refs/tags/{tag}", "object": {"type": "commit", "sha": tag_sha or manifest["sha"]}})
        if attested:
            self.attested_files[hashlib.sha256(data).hexdigest()] = manifest["signer"]["workflow_sha"]
        return data

    # -- protocol -----------------------------------------------------------

    def _result(self, argv: list[str], code: int, stdout: bytes = b"") -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(argv, code, stdout=stdout, stderr=b"fake gh stderr that must never be echoed")

    def _json(self, argv: list[str], value: Any) -> subprocess.CompletedProcess:
        return self._result(argv, 0, json.dumps(value).encode("utf-8"))

    def __call__(self, argv: list[str], **kwargs: Any) -> subprocess.CompletedProcess:
        env = dict(kwargs.get("env") or {})
        self.calls.append((list(argv), env))
        args = list(argv[1:])
        if args and args[0] in self.fail_commands:
            return self._result(argv, 1)
        if args == ["--version"]:
            return self._result(argv, 0, f"gh version {self.version} (2026-09-01)\nhttps://github.com/cli/cli/releases/tag/v{self.version}\n".encode())
        if args[:3] == ["api", "--paginate", "--slurp"] and len(args) == 4:
            return self._api(argv, args[3])
        if args[:1] == ["api"] and len(args) == 2:
            if args[1] == f"/repos/{REPO}/environments/production":
                return self._json(argv, self.environment)
            return self._result(argv, 1)
        if args[:2] == ["release", "download"]:
            return self._download(argv, args)
        if args[:2] == ["attestation", "verify"]:
            return self._verify(argv, args)
        if args[:2] in (["release", "create"], ["release", "upload"], ["release", "edit"]):
            return self._write(argv, args)
        return self._result(argv, 2)

    def _api(self, argv: list[str], path: str) -> subprocess.CompletedProcess:
        parsed = urllib.parse.urlsplit(path)
        query = dict(urllib.parse.parse_qsl(parsed.query))
        if parsed.path == f"/repos/{REPO}/releases":
            visible = [item for item in self.releases if not (self.drafts_hidden and item["draft"])]
            return self._json(argv, [visible[index:index + 100] for index in range(0, max(len(visible), 1), 100)])
        if parsed.path == f"/repos/{REPO}/git/matching-refs/tags/prod-":
            return self._json(argv, [self.tags])
        prefix = f"/repos/{REPO}/actions/workflows/"
        if parsed.path.startswith(prefix) and parsed.path.endswith("/runs"):
            workflow = parsed.path[len(prefix):-len("/runs")]
            runs = [run for run in self.ci_runs.get(workflow, []) if run["head_sha"] == query.get("head_sha")]
            return self._json(argv, [{"total_count": len(runs), "workflow_runs": runs}])
        if parsed.path == f"/repos/{REPO}/actions/runs":
            runs = self.active.get(query.get("status", ""), [])
            return self._json(argv, [{"total_count": len(runs), "workflow_runs": runs}])
        if parsed.path == f"/repos/{REPO}/environments/production/deployment-branch-policies":
            return self._json(argv, [{"total_count": len(self.branch_policies), "branch_policies": self.branch_policies}])
        approvals_prefix = f"/repos/{REPO}/actions/runs/"
        if parsed.path.startswith(approvals_prefix) and parsed.path.endswith("/approvals"):
            run_id = parsed.path[len(approvals_prefix):-len("/approvals")]
            return self._json(argv, [self.approvals.get(run_id, [])])
        return self._result(argv, 1)

    def _download(self, argv: list[str], args: list[str]) -> subprocess.CompletedProcess:
        tag = args[2]
        directory = Path(args[args.index("--dir") + 1])
        if tag not in self.assets or args[args.index("--pattern") + 1] != "release-manifest.json":
            return self._result(argv, 1)
        (directory / "release-manifest.json").write_bytes(self.assets[tag])
        return self._result(argv, 0)

    @staticmethod
    def _flag(args: list[str], name: str) -> str | None:
        return args[args.index(name) + 1] if name in args else None

    @staticmethod
    def identity(workflow: str) -> str:
        return f"https://github.com/{REPO}/{workflow}@refs/heads/main"

    @staticmethod
    def _certificate(uri: str, signer: str, source: str) -> dict[str, str]:
        return {
            "buildSignerURI": uri,
            "subjectAlternativeName": uri,
            "buildSignerDigest": signer,
            "sourceRepositoryDigest": source,
            "sourceRepositoryRef": "refs/heads/main",
            "runnerEnvironment": "github-hosted",
        }

    def _verify(self, argv: list[str], args: list[str]) -> subprocess.CompletedProcess:
        subject = args[2]
        required = ("--repo", "--cert-identity", "--signer-digest", "--source-digest", "--source-ref", "--predicate-type")
        if any(name not in args for name in required) or "--deny-self-hosted-runners" not in args:
            return self._result(argv, 1)
        if "--signer-workflow" in args or "--cert-identity-regex" in args:
            return self._result(argv, 1)
        if self._flag(args, "--repo") != REPO or self._flag(args, "--source-ref") != "refs/heads/main":
            return self._result(argv, 1)
        if self._flag(args, "--format") != "json":
            return self._result(argv, 1)
        identity = self._flag(args, "--cert-identity")
        signer = self._flag(args, "--signer-digest")
        source = self._flag(args, "--source-digest")
        predicate_type = self._flag(args, "--predicate-type")
        if subject.startswith("oci://"):
            image, _, value = subject[len("oci://"):].partition("@")
            # "matched_as" lets a test model gh matching a certificate whose
            # real identity ("workflow") differs from the requested one.
            matches = [
                item for item in self.image_attestations.get((image, value), [])
                if self.identity(item.get("matched_as", item["workflow"])) == identity and item["signer"] == signer
                and item["source"] == source and item["predicate_type"] == predicate_type
            ]
            if not matches:
                return self._result(argv, 1)
            return self._json(argv, [
                {
                    "verificationResult": {
                        "statement": {"predicate": item["predicate"]},
                        "signature": {"certificate": self._certificate(self.identity(item["workflow"]), signer, source)},
                    }
                }
                for item in matches
            ])
        data = Path(subject).read_bytes()
        expected = self.attested_files.get(hashlib.sha256(data).hexdigest())
        if (
            expected is None
            or identity != self.identity(".github/workflows/ship.yml")
            or signer != expected
            or source != expected
            or predicate_type != "https://slsa.dev/provenance/v1"
        ):
            return self._result(argv, 1)
        return self._json(argv, [{
            "verificationResult": {
                "statement": {"predicate": {"buildType": "fake"}},
                "signature": {"certificate": self._certificate(identity, signer, source)},
            }
        }])

    def _write(self, argv: list[str], args: list[str]) -> subprocess.CompletedProcess:
        self.writes.append(list(args))
        tag = args[2]
        if args[1] == "create":
            self.next_release_id += 1
            self.releases.append({
                "id": self.next_release_id, "tag_name": tag, "draft": "--draft" in args, "prerelease": False,
                "target_commitish": self._flag(args, "--target"), "assets": [],
            })
            return self._result(argv, 0)
        release = [item for item in self.releases if item["tag_name"] == tag]
        if len(release) != 1:
            return self._result(argv, 1)
        release = release[0]
        if args[1] == "upload":
            if release["assets"] and "--clobber" not in args:
                return self._result(argv, 1)
            data = Path(args[3]).read_bytes()
            self.assets[tag] = data
            release["assets"] = [{"id": 1, "name": Path(args[3]).name, "size": len(data), "state": "uploaded"}]
            return self._result(argv, 0)
        if "--draft=false" in args:
            release["draft"] = False
            self.tags.append({"ref": f"refs/tags/{tag}", "object": {"type": "commit", "sha": release["target_commitish"]}})
        return self._result(argv, 0)


def _reviewers(fake: "FakeGh") -> list[dict[str, Any]]:
    return [rule for rule in fake.environment["protection_rules"] if rule["type"] == "required_reviewers"][0]["reviewers"]


# Each mutation breaks the owner-only approval gate in one way (the live
# environment on 2026-10-03 had no reviewer and admin bypass on).
APPROVAL_GATE_MUTATIONS: dict[str, Callable[["FakeGh"], None]] = {
    "no-reviewer-rule": lambda fake: fake.environment.update(
        protection_rules=[rule for rule in fake.environment["protection_rules"] if rule["type"] != "required_reviewers"]),
    "empty-reviewers": lambda fake: _reviewers(fake).clear(),
    "two-reviewers": lambda fake: _reviewers(fake).append({"type": "User", "reviewer": {"login": "someone-else", "id": 8}}),
    "wrong-login": lambda fake: _reviewers(fake)[0]["reviewer"].update(login="someone-else"),
    "team-reviewer": lambda fake: _reviewers(fake)[0].update(type="Team"),
    "two-reviewer-rules": lambda fake: fake.environment["protection_rules"].append(
        copy.deepcopy([rule for rule in fake.environment["protection_rules"] if rule["type"] == "required_reviewers"][0])),
    "admin-bypass-on": lambda fake: fake.environment.update(can_admins_bypass=True),
    "admin-bypass-missing": lambda fake: fake.environment.pop("can_admins_bypass"),
    "protected-branches-policy": lambda fake: fake.environment.update(
        deployment_branch_policy={"protected_branches": True, "custom_branch_policies": False}),
    "no-branch-policy": lambda fake: fake.environment.update(deployment_branch_policy=None),
    "extra-branch-policy": lambda fake: fake.branch_policies.append({"id": 4, "name": "release/*", "type": "branch"}),
    "tag-policy": lambda fake: fake.branch_policies[0].update(type="tag"),
    "other-branch": lambda fake: fake.branch_policies[0].update(name="develop"),
    "no-branch-rules": lambda fake: fake.branch_policies.clear(),
    "environment-unreadable": lambda fake: fake.environment.clear(),
}

APPROVAL_MUTATIONS: dict[str, Callable[["FakeGh"], None]] = {
    "no-approval": lambda fake: fake.approvals[RUN_ID].clear(),
    "rejected": lambda fake: fake.approvals[RUN_ID][0].update(state="rejected"),
    "other-user": lambda fake: fake.approvals[RUN_ID][0]["user"].update(login="someone-else"),
    "other-environment": lambda fake: fake.approvals[RUN_ID][0].update(environments=[{"id": 1, "name": "staging"}]),
    "other-run": lambda fake: fake.approvals.update({"1": fake.approvals.pop(RUN_ID)}),
}


# --------------------------------------------------------------------------
# Records
# --------------------------------------------------------------------------


def bootstrap_record(sha: str, images: Mapping[str, str] = LIVE) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "kind": "bootstrap",
        "release": "prod-0000",
        "sha": sha,
        "images": dict(images),
        "db_change": False,
        "previous": None,
        "deployed_at": "2026-10-02T15:11:28Z",
        "legacy_signer": {
            "workflow": ".github/workflows/build-attest-exact-release-images.yml",
            "workflow_sha": LEGACY_WORKFLOW_SHA,
            "source_ref": "refs/heads/main",
            "source_predicate": "https://rereply.app/attestations/exact-release-image/v1",
            "source_commit": sha,
            "phase": "ui",
        },
        "evidence": {
            "phase_state_sha256": "ac" * 32,
            "phase_state_signer": ".github/workflows/verify-production-crm-canary.yml",
            "phase_state_signer_sha": LEGACY_WORKFLOW_SHA,
            "phase_state_completed_at": "2026-10-02T15:15:45Z",
        },
    }


def manifest(
    *,
    kind: str,
    sha: str,
    images: Mapping[str, str],
    previous: str,
    deployed_at: str,
    workflow_sha: str | None = None,
    rolled_back_from: str | None = None,
    reconciled: bool = False,
    built_at: str | None = None,
) -> dict[str, Any]:
    compact = deployed_at.replace("-", "").replace(":", "")
    if kind == "promote" and built_at is None:
        built_at = deployed_at
    return {
        "schema_version": 1,
        "kind": kind,
        "release": f"prod-{compact}-{sha[:8]}",
        "sha": sha,
        "images": dict(images),
        "db_change": False,
        "schema_guard": "clean",
        "previous": previous,
        "rolled_back_from": rolled_back_from,
        "reconciled": reconciled,
        "built_at": built_at,
        "deployed_at": deployed_at,
        "signer": {"workflow": ".github/workflows/ship.yml", "workflow_sha": workflow_sha or sha},
        "fingerprints": {"environment_values_sha256": "e" * 64, "non_source_projection_sha256": "d" * 64},
    }


# --------------------------------------------------------------------------
# Temporary git repositories
# --------------------------------------------------------------------------


def git_env() -> dict[str, str]:
    env = dict(os.environ)
    env.update({
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_AUTHOR_NAME": "Ship Test",
        "GIT_AUTHOR_EMAIL": "ship-test@example.invalid",
        "GIT_COMMITTER_NAME": "Ship Test",
        "GIT_COMMITTER_EMAIL": "ship-test@example.invalid",
        "GIT_TERMINAL_PROMPT": "0",
    })
    return env


class TempRepo:
    def __init__(self, root: Path) -> None:
        self.root = root
        root.mkdir(parents=True, exist_ok=True)
        self.git("init", "--quiet", "--initial-branch=main")
        for key, value in (
            ("user.name", "Ship Test"), ("user.email", "ship-test@example.invalid"),
            ("commit.gpgsign", "false"), ("core.autocrlf", "false"), ("core.safecrlf", "false"),
        ):
            self.git("config", key, value)

    def git(self, *args: str) -> str:
        result = subprocess.run(
            ["git", "-C", str(self.root), *args],
            env=git_env(), capture_output=True, check=False, stdin=subprocess.DEVNULL,
        )
        if result.returncode != 0:
            raise AssertionError("git failed in a temporary test repository")
        return result.stdout.decode("utf-8", "replace")

    def write(self, files: Mapping[str, str | None]) -> None:
        for name, content in files.items():
            path = self.root / name
            if content is None:
                if path.exists():
                    path.unlink()
                continue
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content.encode("utf-8"))

    def commit(self, files: Mapping[str, str | None], message: str = "change") -> str:
        self.write(files)
        self.git("add", "-A")
        self.git("commit", "--quiet", "--allow-empty", "-m", message)
        return self.head()

    def head(self) -> str:
        return self.git("rev-parse", "HEAD").strip()

    def checkout(self, revision: str) -> None:
        self.git("checkout", "--quiet", revision)


def base_repo(root: Path) -> tuple[TempRepo, str]:
    """A repository with an old-lane workflow and a product tree."""
    repo = TempRepo(root)
    sha = repo.commit({
        ".github/workflows/old-lane.yml": "name: Old\non: workflow_dispatch\nconcurrency:\n" + OLD_GROUP_LINE + "\n  cancel-in-progress: false\n",
        ".github/workflows/test.yml": "name: Test\non: push\n",
        "cmd/whatomate/main.go": "package main\n\nfunc runRLSMigration(args []string) {\n\tprintln(1)\n}\n\nfunc other() {\n}\n",
        "internal/database/postgres.go": "package database\n",
        "internal/models/models.go": "package models\n",
        "internal/handlers/handler.go": "package handlers\n",
        "README.md": "readme\n",
    }, "base")
    return repo, sha


# --------------------------------------------------------------------------
# A production / plan / record harness
# --------------------------------------------------------------------------


def context_env(sha: str, temp: Path) -> dict[str, str]:
    runner_temp = temp / "runner"
    runner_temp.mkdir(parents=True, exist_ok=True)
    env = {
        "GITHUB_REPOSITORY": REPO,
        "GITHUB_ACTOR": common.APPROVER_LOGIN,
        "GITHUB_REF": "refs/heads/main",
        "GITHUB_REF_PROTECTED": "true",
        "GITHUB_EVENT_NAME": "workflow_dispatch",
        "GITHUB_WORKFLOW_REF": f"{REPO}/.github/workflows/ship.yml@refs/heads/main",
        "GITHUB_WORKFLOW_SHA": sha,
        "GITHUB_SHA": sha,
        "RUNNER_ENVIRONMENT": "github-hosted",
        "GITHUB_RUN_ATTEMPT": "1",
        "GITHUB_RUN_ID": RUN_ID,
        "RUNNER_TEMP": str(runner_temp),
        "GITHUB_OUTPUT": str(temp / "github-output"),
        "GITHUB_STEP_SUMMARY": str(temp / "github-summary"),
        "SHIP_GH_BIN": "/fake/bin/gh",
        "GH_TOKEN": GH_TOKEN,
        "PINNED_GH_VERSION": GH_VERSION,
        "PINNED_TRIVY_VERSION": "0.70.0",
        "PATH": os.environ.get("PATH", ""),
    }
    for name in ("HOME", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE"):
        if name in os.environ:
            env[name] = os.environ[name]
    return env


def read_outputs(path: Path) -> dict[str, str]:
    if not path.exists():
        return {}
    output: dict[str, str] = {}
    for line in path.read_text(encoding="ascii").splitlines():
        key, _, value = line.partition("=")
        output[key] = value
    return output


def candidate_b64(images: Mapping[str, str], sha: str, built_at: dt.datetime, *, workflow_sha: str | None = None) -> tuple[str, str]:
    value = {
        "schema_version": 1,
        "sha": sha,
        "workflow_sha": workflow_sha or sha,
        "built_at": built_at.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "images": dict(images),
        "trivy": {"db_updated_at": "2026-10-04T06:00:00Z", "active_exceptions": 0},
    }
    raw = canonical(value)
    return base64.b64encode(raw).decode("ascii"), hashlib.sha256(raw).hexdigest()


def staging_evidence(candidate: Mapping[str, Any], candidate_hash: str, *, previous_sha: str) -> dict[str, str]:
    """Synthetic successful same-run stage outputs; no real fixture or identifiers."""
    from stage_report import CHECKS
    receipt = {
        "schema_version": 1, "profile": "staging", "run_id": RUN_ID, "candidate_sha256": candidate_hash,
        "ingress_sha256": "f" * 64, "app_id_sha256": "e" * 64, "drill": "none",
        "previous_images": dict(LIVE), "candidate_images": dict(candidate["images"]),
        "before_spec_sha256": "1" * 64, "after_spec_sha256": "2" * 64,
        "before_deployment_sha256": "3" * 64, "candidate_deployment_sha256": "4" * 64,
        "previous_source_sha": previous_sha, "candidate_source_sha": candidate["sha"],
    }
    raw = canonical(receipt)
    receipt_hash = hashlib.sha256(raw).hexdigest()
    report = {"receipt_sha256": receipt_hash, "run_id": RUN_ID, "candidate_sha256": candidate_hash,
              "origin_sha256": "f" * 64, "checks": list(CHECKS), "passed": 13}
    report_raw = canonical(report)
    return {"STAGE_RECEIPT_B64": base64.b64encode(raw).decode(), "STAGE_RECEIPT_SHA256": receipt_hash,
            "STAGE_REPORT_B64": base64.b64encode(report_raw).decode(), "STAGE_REPORT_SHA256": hashlib.sha256(report_raw).hexdigest()}


def scan_private(text: str, extra: Iterable[str] = ()) -> list[str]:
    """Return every private marker found in text, ignoring ::add-mask:: lines."""
    findings: list[str] = []
    lines = [line for line in text.splitlines() if not line.startswith("::add-mask::")]
    body = "\n".join(lines)
    for value in (*PRIVATE_VALUES, *extra):
        if value and value in body:
            findings.append(value)
    lowered = body.lower()
    for fragment in ("api.digitalocean.com", "ondigitalocean" + ".app"):
        if fragment in lowered:
            findings.append(fragment)
    if common.ANY_UUID_RE.search(body):
        findings.append("uuid")
    return findings


class Harness:
    """Runs ship.main for one command with every effect faked."""

    def __init__(self, temp: Path, *, repo: TempRepo, head: str, bootstrap_sha: str) -> None:
        import ship  # noqa: PLC0415 - imported lazily so the module path is set

        self.ship = ship
        self.temp = temp
        self.repo = repo
        self.head = head
        self.env = context_env(head, temp)
        self.stdout = io.StringIO()
        self.do = FakeDO()
        self.gh = FakeGh()
        self.https = FakeHttps()
        self.sleeps: list[float] = []
        self.clock_value = NOW
        target = temp / "ship-target.json"
        target.write_bytes(canonical(make_target()))
        staging_pins = temp / "ship-target-staging.json"
        staging_pins.write_bytes(canonical({"schema_version": 1, "profile": "staging", "team_uuid_sha256": "d" * 64, "app_id_sha256": "e" * 64}))
        self.bootstrap = bootstrap_record(bootstrap_sha)
        bootstrap_path = temp / "ship-bootstrap-record.json"
        bootstrap_raw = canonical(self.bootstrap)
        bootstrap_path.write_bytes(bootstrap_raw)
        self.bootstrap_sha256 = hashlib.sha256(bootstrap_raw).hexdigest()
        policy = temp / "ship.trivyignore"
        policy.write_bytes(b"# no exceptions\n")
        self.paths = {"target": target, "bootstrap": bootstrap_path, "policy": policy, "staging_pins": staging_pins}
        self.gh.attest_legacy_images(LIVE, bootstrap_sha)
        self.gh.ci_green(head)
        self.poll_limit = 8

    def deps(self) -> Any:
        return self.ship.Deps(
            clock=lambda: self.clock_value,
            sleeper=self.sleeps.append,
            opener=self.do,
            gh_runner=self.gh,
            https_request=self.https,
            stdout=self.stdout,
            repo_dir=self.repo.root,
            target_path=self.paths["target"],
            staging_pins_path=self.paths["staging_pins"],
            bootstrap_path=self.paths["bootstrap"],
            bootstrap_sha256=self.bootstrap_sha256,
            trivy_policy_path=self.paths["policy"],
            work_dir=self.temp / "work",
            poll_limit=self.poll_limit,
            smoke_rounds=2,
            smoke_delay=0,
        )

    def production_env(self, mode: str = "promote", *, target_release: str = "", images: Mapping[str, str] = NEW,
                       built_at: dt.datetime | None = None) -> None:
        for name in ("STAGE_RECEIPT_B64", "STAGE_RECEIPT_SHA256", "STAGE_REPORT_B64", "STAGE_REPORT_SHA256"):
            self.env.pop(name, None)
        self.env.update({
            "SHIP_DO_TOKEN": DO_TOKEN,
            "SHIP_TARGET_JSON": json.dumps({"schema_version": 1, "app_id": APP_ID, "postgres_cluster_id": PG_ID}),
            "SHIP_MODE": mode,
            "SHIP_TARGET_RELEASE": target_release,
            "PLAN_LATEST_RELEASE": "prod-0000",
            "PLAN_LATEST_MANIFEST_SHA256": self.bootstrap_sha256,
            "PLAN_TARGET_RELEASE": target_release,
            "PLAN_TARGET_MANIFEST_SHA256": "",
            "CANDIDATE_B64": "",
            "CANDIDATE_SHA256": "",
        })
        if mode != "rollback":
            encoded, digest_value = candidate_b64(images, self.head, built_at or (NOW - dt.timedelta(hours=1)))
            self.env["CANDIDATE_B64"] = encoded
            self.env["CANDIDATE_SHA256"] = digest_value
            self.gh.attest_ship_images(images, self.head)
            if mode == "promote":
                self.env.update(staging_evidence(json.loads(base64.b64decode(encoded)), digest_value, previous_sha=self.bootstrap["sha"]))

    def run(self, *argv: str) -> int:
        return self.ship.main(list(argv), self.env, self.deps())

    def outputs(self) -> dict[str, str]:
        return read_outputs(Path(self.env["GITHUB_OUTPUT"]))

    def summary(self) -> str:
        path = Path(self.env["GITHUB_STEP_SUMMARY"])
        return path.read_text(encoding="ascii") if path.exists() else ""

    def text(self) -> str:
        return self.stdout.getvalue()


def new_temp() -> tempfile.TemporaryDirectory:
    return tempfile.TemporaryDirectory(prefix="ship-test-")
