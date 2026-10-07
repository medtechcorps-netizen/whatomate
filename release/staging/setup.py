#!/usr/bin/env python3
"""Owner-run staging setup. Never imports a production token or copies a database.

All child output is captured, including failures. Only fixed codes and public
fingerprints are emitted. A private journal is saved BEFORE each provider write;
an interrupted write requires read-only reconciliation, never an automatic retry.
"""
from __future__ import annotations

import argparse
import base64
import copy
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))
import release_record
import schema_change
import do_app
import ship_common

REPOSITORY = "medtechcorps-netizen/whatomate"
CONTEXT = "rereply-staging"
MIN_PRODUCT_SHA = "cfcc085b11f87cbffa887f1ad7390f1ca6ba0f6b"
AMBIENT = ("DIGITALOCEAN_ACCESS_TOKEN", "DIGITALOCEAN_TOKEN", "DIGITALOCEAN_CONTEXT",
           "DO_ACCESS_TOKEN", "DO_TOKEN", "DOCTL_CONTEXT", "DOCTL_CONFIG", "DOCTL_API_URL", "DOCTL_ACCESS_TOKEN")
SHA = re.compile(r"[0-9a-f]{64}")
COMMIT = re.compile(r"[0-9a-f]{40}")
IMAGE = re.compile(r"ghcr\.io/medtechcorps-netizen/rereply-(?:release-(?:web|meta-relay|gmail-relay)|staging-(?:graph-stub|bootstrap))@sha256:[0-9a-f]{64}")
SECRET_NAMES = ("encryption_key", "jwt", "admin_password", "stub_control_key", "stub_access_token",
                "stub_app_secret", "webhook_verify", "messenger_secret", "messenger_verify",
                "instagram_secret", "instagram_verify", "gmail_encryption", "gmail_setup", "gmail_secret",
                "page_inbound", "page_outbound")


class Refused(Exception):
    """Reason codes only; never attach caller/provider text."""


def require(condition, code):
    if not condition:
        raise Refused(code)


def digest(value):
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def object_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "duplicate-json-key")
            result[key] = value
        return result
    try:
        return json.loads(raw, object_pairs_hook=unique)
    except (ValueError, UnicodeError):
        raise Refused("invalid-json") from None


def canonical_uuid(value):
    try:
        return isinstance(value, str) and str(uuid.UUID(value)) == value and uuid.UUID(value).int != 0
    except ValueError:
        return False


def one(value):
    if isinstance(value, list):
        require(len(value) == 1, "provider-shape")
        value = value[0]
    require(isinstance(value, dict), "provider-shape")
    return value


def private_path(path, root=ROOT):
    path = Path(path).expanduser().absolute()
    for item in (path, *path.parents):
        require(not item.is_symlink(), "private-path-symlink")
        try:
            attributes = getattr(item.lstat(), "st_file_attributes", 0)
        except FileNotFoundError:
            attributes = 0
        # Windows junctions (including broken targets) are reparse points but
        # not always symlinks. lstat must run even when exists() would be false.
        require(not attributes & 0x400, "private-path-reparse-point")
        require(not (item / ".git").exists(), "private-path-in-checkout")
    require(not path.resolve().is_relative_to(root.resolve()), "private-path-in-checkout")
    return path


def private_directory(path, runner):
    # Never remove inherited ACLs from an arbitrary/shared parent such as C:\
    # or the user's home. This is a dedicated directory chosen for this kit.
    require(path.name == "rereply-staging", "private-directory-must-be-rereply-staging")
    require(not path.exists() or not any(path.iterdir()), "private-directory-not-empty")
    path.mkdir(parents=True, exist_ok=True)
    if os.name == "nt":
        # Replace the whole DACL: removing inheritance alone leaves explicit
        # Everyone/Users grants in place on an existing directory.
        literal = str(path).replace("'", "''")
        script = """$ErrorActionPreference='Stop'
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User
$acl=[Security.AccessControl.DirectorySecurity]::new()
$acl.SetOwner($sid)
$acl.SetAccessRuleProtection($true,$false)
$rule=[Security.AccessControl.FileSystemAccessRule]::new($sid,'FullControl','ContainerInherit,ObjectInherit','None','Allow')
$acl.AddAccessRule($rule)
[IO.Directory]::SetAccessControl('""" + literal + "', $acl)\n"
        encoded = base64.b64encode(script.encode("utf-16-le")).decode("ascii")
        runner.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded])
    else:
        path.chmod(0o700)


def check_private_state(path, runner):
    require(path.parent.name == "rereply-staging", "private-directory-must-be-rereply-staging")
    if os.name == "nt":
        literals = ",".join("'" + str(p).replace("'", "''") + "'" for p in (path.parent, path))
        script = """$ErrorActionPreference='Stop'
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
foreach ($path in @(""" + literals + """)) {
  if ([IO.Directory]::Exists($path)) { $acl=[IO.Directory]::GetAccessControl($path) }
  else { $acl=[IO.File]::GetAccessControl($path) }
  if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -ne $sid) { exit 2 }
  $allow=$acl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier]) | Where-Object { $_.AccessControlType -eq 'Allow' }
  if (-not $allow -or @($allow | Where-Object { $_.IdentityReference.Value -ne $sid }).Count -ne 0) { exit 2 }
}
"""
        encoded = base64.b64encode(script.encode("utf-16-le")).decode("ascii")
        runner.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded])
    else:
        for item in (path.parent, path):
            require(item.stat().st_uid == os.getuid() and item.stat().st_mode & 0o077 == 0, "private-state-access")


def save_private(path, value):
    # Parent directory has already had inheritance removed before any secret exists.
    require(path.parent.is_dir() and not path.is_symlink(), "private-state-path")
    fd, temporary = tempfile.mkstemp(prefix=".staging-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
            json.dump(value, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def check_doctl_config(path):
    """Accept doctl's single-context YAML shape, never interpret general YAML."""
    require(path.is_file() and not path.is_symlink(), "staging-config-missing")
    raw = path.read_text(encoding="utf-8")
    require(len(raw) < 65536, "staging-config-shape")
    contexts = []
    section = False
    current = None
    for line in raw.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line == "auth-contexts:":
            require(not section, "staging-config-shape")
            section = True
            continue
        if line[0].isspace():
            require(section, "staging-config-shape")
            match = re.fullmatch(r"[ ]{2,4}([a-zA-Z0-9_-]+):[ ]*(\S+)[ ]*", line)
            require(match is not None, "staging-config-shape")
            contexts.append(match.group(1))
            continue
        section = False
        key, sep, value = line.partition(":")
        require(sep and key in {"access-token", "current-context", "api-url"}, "staging-config-shape")
        value = value.strip().strip('"').strip("'")
        if key == "current-context":
            current = value
        elif key == "access-token":
            require(value == "", "staging-config-global-token")
        else:
            require(value in {"", "https://api.digitalocean.com"}, "staging-config-api")
    require(contexts == [CONTEXT] and current == CONTEXT, "staging-config-contexts")


class Runner:
    def __init__(self, root=ROOT):
        self.root = root

    def run(self, args, *, input=None, timeout=180):
        env = dict(os.environ)
        for name in list(env):
            if name.startswith(("WHATOMATE_", "STUB_", "META_RELAY_", "GMAIL_RELAY_")):
                del env[name]
        env.update(GH_PROMPT_DISABLED="1", GH_NO_UPDATE_NOTIFIER="1", GIT_TERMINAL_PROMPT="0")
        try:
            process = subprocess.run(args, input=input, capture_output=True, cwd=self.root,
                                     env=env, timeout=timeout, check=False)
        except (OSError, subprocess.SubprocessError):
            raise Refused("command-failed-or-ambiguous") from None
        require(process.returncode == 0 and len(process.stdout) <= 16 * 1024 * 1024, "command-failed-or-ambiguous")
        return process.stdout

    def git(self, *args):
        return self.run(["git", *args]).decode().strip()

    def do(self, config, *args, input=None):
        raw = self.run(["doctl", "--config", str(config), "--context", CONTEXT,
                        "--api-url", "https://api.digitalocean.com", "--http-retry-max", "0", "--interactive=false", "--output", "json", *args], input=input)
        return object_json(raw) if raw.strip() else None

    def health(self, origin):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        opener = urllib.request.build_opener(NoRedirect)
        for path, status in (("/health", 200), ("/ready", 200), ("/meta-relay/livez", 204),
                             ("/meta-relay/readyz", 204), ("/gmail-relay/livez", 204), ("/gmail-relay/readyz", 204)):
            try:
                with opener.open(origin + path, timeout=15) as response:
                    require(response.status == status, "health-failed")
            except Exception:
                raise Refused("health-failed") from None


class LocalGh(release_record.Gh):
    """Reuse the record verifier without exporting the owner's gh credential."""
    def __init__(self, runner):
        self.repo = REPOSITORY
        self.runner = runner

    def _run(self, args, *, code):
        return self.runner.run(["gh", *args])


class Setup:
    def __init__(self, runner, config, target, images, state_path):
        self.runner, self.config = runner, Path(config)
        self.target, self.images, self.state_path = target, images, Path(state_path)
        if self.state_path.exists():
            check_private_state(self.state_path, runner)
        self.state = object_json(self.state_path.read_bytes()) if self.state_path.exists() else None
        self.production = object_json((runner.root / "release/deployment/ship-target.json").read_bytes())
        self.template = object_json((runner.root / "release/staging/app-spec.template.yaml").read_bytes())

    def preflight(self):
        require(not any(name in os.environ for name in AMBIENT), "ambient-digitalocean-credential")
        check_doctl_config(self.config)
        r, t = self.runner, self.target
        require(not r.git("status", "--porcelain", "--untracked-files=all"), "checkout-dirty")
        head = r.git("rev-parse", "HEAD")
        require(COMMIT.fullmatch(head) and head == r.git("rev-parse", "origin/main"), "checkout-not-main")
        remote = object_json(r.run(["gh", "api", f"repos/{REPOSITORY}/commits/main"]))
        require(remote.get("sha") == head, "checkout-not-current-main")
        require(t.get("schema_version") == 1 and SHA.fullmatch(t.get("team_sha256", "")), "target-shape")
        require(all(canonical_uuid(t.get(key)) for key in ("postgres_id", "valkey_id", "vpc_id")), "target-resource-id")
        require(t.get("postgres_id") != t.get("valkey_id"), "target-clusters")
        account = one(r.do(self.config, "account", "get"))
        team = account.get("team", {})
        require(account.get("status") == "active" and team.get("name") == "ReReply Staging" and
                isinstance(team.get("uuid"), str) and digest(team["uuid"]) == t["team_sha256"], "wrong-staging-team")
        apps = r.do(self.config, "apps", "list")
        clusters = r.do(self.config, "databases", "list")
        require(isinstance(apps, list) and isinstance(clusters, list), "provider-inventory-shape")
        require(all(isinstance(a, dict) and isinstance(a.get("id"), str) and
                    digest(a["id"]) != self.production["app_id_sha256"] for a in apps), "production-app-visible")
        require(all(isinstance(c, dict) and isinstance(c.get("name"), str) and
                    digest(c["name"]) != self.production["postgres"]["cluster_name_sha256"] for c in clusters), "production-cluster-visible")
        self.apps = apps
        self.clusters = {}
        for key, engine, name in (("postgres_id", "pg", "rereply-staging-pg"),
                                  ("valkey_id", "valkey", "rereply-staging-valkey")):
            candidates = [c for c in clusters if c.get("id") == t[key]]
            require(len(candidates) == 1, "staging-cluster-missing")
            cluster = candidates[0]
            require(cluster.get("engine") == engine and cluster.get("name") == name and cluster.get("status") == "online" and
                    cluster.get("region") == "sgp1" and cluster.get("private_network_uuid") == t["vpc_id"], "staging-cluster-shape")
            if engine == "pg":
                require(str(cluster.get("version")) == "17", "staging-postgres-version")
            self.clusters[key] = cluster
        if self.state:
            require(self.state.get("schema_version") == 1 and self.state.get("target") == t, "private-target-drift")
            require(not self.state.get("pending_operation"), "operation-needs-reconciliation")
        return head

    def verify_images(self):
        r, images = self.runner, self.images
        source = images.get("source_sha", "")
        require(COMMIT.fullmatch(source), "staging-image-source")
        r.git("merge-base", "--is-ancestor", source, "HEAD")
        gh = LocalGh(r)
        for component in ("graph-stub", "bootstrap"):
            image = images.get(component, "")
            prefix = f"ghcr.io/{REPOSITORY.split('/')[0]}/rereply-staging-{component}@sha256:"
            require(IMAGE.fullmatch(image) and image.startswith(prefix), "staging-image-reference")
            workflow = ".github/workflows/staging-images.yml"
            gh._verify("oci://" + image,
                       gh.signer_flags(workflow, source, source, release_record.SLSA_PREDICATE),
                       gh.signer_identity(workflow, source, source), code="attestation-unverified:staging-image")
        with tempfile.TemporaryDirectory(prefix="staging-records-") as folder:
            bootstrap = release_record.load_bootstrap()
            chain = release_record.resolve_chain(gh, r.root, bootstrap=bootstrap, work_dir=Path(folder))
            self.latest = chain.latest
            if self.state:
                # Keep an in-progress setup pinned when a normal production
                # release happens. The chosen record must still be in the fully
                # verified chain; a caller-written image set is never authority.
                stored = self.state.get("product", {})
                matches = [entry for entry in chain.entries if entry.sha == stored.get("sha") and entry.images == stored.get("images")]
                require(matches, "private-product-record-not-verified")
                self.latest = matches[-1]
            r.git("merge-base", "--is-ancestor", MIN_PRODUCT_SHA, self.latest.sha)
            require(not schema_change.check(r.root, self.latest.sha, source), "bootstrap-schema-differs")
            release_record.verify_images(gh, self.latest.images, self.latest.sha, bootstrap)
        return self.latest

    def firewall(self, cluster_id):
        rules = self.runner.do(self.config, "databases", "firewalls", "list", cluster_id)
        require(isinstance(rules, list) and rules, "trusted-sources-empty")
        require(all(isinstance(rule, dict) and rule.get("type") in {"ip_addr", "app"} and
                    isinstance(rule.get("value"), str) for rule in rules), "trusted-sources-shape")
        return rules

    def checkpoint(self, operation):
        self.state["pending_operation"] = operation
        save_private(self.state_path, self.state)

    def completed(self):
        self.state.pop("pending_operation", None)
        save_private(self.state_path, self.state)

    def db(self, operator_ip):
        require(self.state is None, "bootstrap-state-exists")
        try:
            address = ipaddress.ip_address(operator_ip)
        except ValueError:
            raise Refused("operator-ip-invalid") from None
        require(not self.apps, "staging-team-already-has-apps")
        for key in ("postgres_id", "valkey_id"):
            rules = self.firewall(self.target[key])
            allowed = any(rule["type"] == "ip_addr" and address in ipaddress.ip_network(rule["value"], strict=False) for rule in rules)
            require(allowed, "add-operator-ip-in-database-console")
        generated = {name: secrets.token_urlsafe(36) for name in SECRET_NAMES}
        pg_public, pg_private = {}, {}
        for private, output in ((False, pg_public), (True, pg_private)):
            command = ["databases", "connection", self.target["postgres_id"]]
            if private:
                command.append("--private")
            connection = one(self.runner.do(self.config, *command))
            require(connection.get("ssl") is True and isinstance(connection.get("host"), str), "database-connection")
            for user in ("doadmin", "rereply_app"):
                credential = one(self.runner.do(self.config, "databases", "user", "get", self.target["postgres_id"], user))
                require(credential.get("name") == user and isinstance(credential.get("password"), str) and credential["password"], "database-user")
                output[user] = "postgresql://{}:{}@{}:{}/rereply?sslmode=require".format(
                    user, urllib.parse.quote(credential["password"], safe=""), connection["host"], int(connection["port"]))
        valkey = one(self.runner.do(self.config, "databases", "connection", self.target["valkey_id"], "--private"))
        require(isinstance(valkey.get("uri"), str) and valkey["uri"].startswith("rediss://"), "valkey-tls-required")
        self.state = {"schema_version": 1, "target": self.target, "images": self.images, "product": self.latest._asdict(),
                      "secrets": generated, "database_private": pg_private, "redis_url": valkey["uri"],
                      "canary": {"namespace": "rereply-staging-" + secrets.token_hex(8),
                                 "admin_email": "staging-admin@rereply.invalid", "admin_password": generated["admin_password"],
                                 "stub_app_id": "900000000000001", "stub_phone_id": "900000000000002", "stub_waba_id": "900000000000003",
                                 "stub_control_key": generated["stub_control_key"], "stub_access_token": generated["stub_access_token"],
                                 "stub_app_secret": generated["stub_app_secret"]}}
        private_directory(self.state_path.parent, self.runner)
        self.checkpoint("bootstrap")
        config = self.runtime_config(pg_public)
        # /dev/stdin avoids a world-readable bind mount for uid 65532 and
        # never places connection strings in argv, container env or a TOML file.
        self.runner.run(["docker", "run", "--rm", "-i", "--platform", "linux/amd64",
                         self.images["bootstrap"], "-config", "/dev/stdin"], input=config.encode(), timeout=1200)
        self.runner.run(["docker", "run", "--rm", "-i", "--platform", "linux/amd64",
                         ship_common.IMAGE_REPOSITORY["web"] + "@" + self.latest.images["web"], "rls-migrate", "-config", "/dev/stdin"], input=config.encode(), timeout=1200)
        self.state["database_verified"] = True
        self.completed()

    def runtime_config(self, urls):
        values = {"app": {"environment": "staging", "encryption_key": self.state["secrets"]["encryption_key"]},
                  "database": {"url": urls["rereply_app"], "migration_url": urls["doadmin"], "runtime_role": "rereply_app", "rls_enabled": True},
                  "default_admin": {"email": self.state["canary"]["admin_email"], "password": self.state["secrets"]["admin_password"], "full_name": "Synthetic staging administrator"}}
        return "\n".join("[" + section + "]\n" + "\n".join(key + " = " + json.dumps(value) for key, value in entries.items()) for section, entries in values.items())

    def build_spec(self):
        s, t = self.state, self.target
        require(s and s.get("database_verified"), "database-not-verified")
        values = {**s["secrets"], "vpc_id": t["vpc_id"], "pg_name": self.clusters["postgres_id"]["name"],
                  "runtime_url": s["database_private"]["rereply_app"], "migration_url": s["database_private"]["doadmin"],
                  "redis_url": s["redis_url"], "admin_email": s["canary"]["admin_email"],
                  "stub_accounts": json.dumps([{"business_account_id": s["canary"]["stub_waba_id"], "phone_number_id": s["canary"]["stub_phone_id"], "display_phone_number": "+15555550101"}]),
                  "stub_app_id": s["canary"]["stub_app_id"], "allowlist": "", "reply_enabled": "false"}
        organization = s["canary"].get("klinik_organization_id")
        if organization is not None:
            require(canonical_uuid(organization), "canary-organization-invalid")
            values.update(allowlist=organization, reply_enabled="true")
        def fill(value):
            if isinstance(value, str) and value.startswith("@@") and value.endswith("@@"):
                require(value[2:-2] in values, "template-placeholder")
                return values[value[2:-2]]
            if isinstance(value, dict):
                return {key: fill(item) for key, item in value.items()}
            if isinstance(value, list):
                return [fill(item) for item in value]
            return value
        spec = fill(self.template)
        refs = {key: ship_common.IMAGE_REPOSITORY[key] + "@" + value for key, value in s["product"]["images"].items()}
        refs["graph-stub"] = self.images["graph-stub"]
        for component in spec["services"] + spec["jobs"]:
            kind = {"omnitech-web": "web", "rereply-rls-migrate": "web"}.get(component["name"], component["name"])
            ref = refs[kind]
            require(IMAGE.fullmatch(ref), "image-reference")
            repository, sha = ref.split("@")
            component["image"] = {"registry_type": "GHCR", "registry": "ghcr.io",
                                  "repository": repository.removeprefix("ghcr.io/"), "digest": sha,
                                  "deploy_on_push": {"enabled": False}}
        return spec

    @staticmethod
    def env_projection(spec):
        result = {}
        for component in spec.get("services", []) + spec.get("jobs", []):
            require(component["name"] not in result, "duplicate-component")
            envs = component.get("envs", [])
            require(len({item["key"] for item in envs}) == len(envs), "duplicate-env")
            result[component["name"]] = sorted((item["key"], item.get("scope", "RUN_TIME"), item.get("type", "GENERAL"),
                                               None if item.get("type") == "SECRET" else item.get("value")) for item in envs)
        return result

    def verify_spec(self, actual, expected):
        require(self.env_projection(actual) == self.env_projection(expected), "non-secret-env-drift")
        require(actual.get("name") == expected["name"] and actual.get("region") == expected["region"] and
                actual.get("vpc") == expected["vpc"] and not actual.get("domains"), "staging-spec-drift")
        require(not any(actual.get(key) for key in ("workers", "static_sites", "functions", "envs")), "staging-unexpected-component")
        def ingress_projection(ingress):
            require(isinstance(ingress, dict), "staging-ingress-drift")
            projected = copy.deepcopy(ingress)
            for rule in projected.get("rules", []):
                component = rule.get("component", {})
                # The provider may omit false/empty defaults in GET responses.
                # Preserve every other leaf, including extra routing rules.
                if component.get("preserve_path_prefix") is False:
                    del component["preserve_path_prefix"]
                if component.get("rewrite") == "":
                    del component["rewrite"]
            return projected
        require(ingress_projection(actual.get("ingress")) == ingress_projection(expected["ingress"]), "staging-ingress-drift")
        databases = actual.get("databases", [])
        require(isinstance(databases, list) and len(databases) == len(expected["databases"]), "staging-database-drift")
        for wanted in expected["databases"]:
            matches = [item for item in databases if item.get("name") == wanted["name"]]
            require(len(matches) == 1 and all(matches[0].get(key) == value for key, value in wanted.items()), "staging-database-drift")
        for group in ("services", "jobs"):
            observed = {item["name"]: item for item in actual.get(group, [])}
            require(set(observed) == {item["name"] for item in expected[group]}, "staging-component-set-drift")
            for item in expected[group]:
                live = observed.get(item["name"], {})
                image = live.get("image", {})
                require(not image.get("tag") and not image.get("registry_credentials") and
                        not image.get("deploy_on_push", {}).get("enabled"), "staging-image-drift")
                require(all(image.get(key) == item["image"][key] for key in ("registry_type", "registry", "repository", "digest")), "staging-image-drift")
                require(not any(live.get(key) for key in ("git", "github", "gitlab", "bitbucket", "dockerfile_path", "source_dir", "build_command", "routes")), "staging-component-source-drift")
                require(not any(live.get(key) for key in ("autoscaling", "log_destinations", "volumes")), "staging-component-drift")
                require(live.get("instance_count") == item["instance_count"] and live.get("instance_size_slug") == item["instance_size_slug"], "staging-component-size-drift")
                if group == "jobs":
                    require(live.get("kind") == "PRE_DEPLOY" and live.get("run_command") == item["run_command"], "staging-job-drift")
                else:
                    require(live.get("http_port") == item["http_port"] and live.get("internal_ports", []) == item["internal_ports"] and live.get("protocol", "HTTP") == "HTTP" and
                            (live.get("health_check") or {}).get("http_path") == item["health_check"]["http_path"] and not live.get("run_command"), "staging-service-drift")

    def app(self, redeploy=False):
        require(self.state and self.state.get("database_verified"), "database-not-verified")
        require(self.state["images"] == self.images, "staging-image-change-requires-review")
        require(self.state["product"]["sha"] == self.latest.sha, "latest-release-changed-rerun-db-proof")
        require(self.state["product"]["images"] == self.latest.images, "private-product-images-drift")
        spec = self.build_spec()
        if redeploy:
            app_id = self.state.get("app_id")
            require(canonical_uuid(app_id), "staging-app-missing")
            before = one(self.runner.do(self.config, "apps", "get", app_id))
            require(before.get("id") == app_id and before.get("spec", {}).get("name") == "rereply-stage", "staging-app-identity")
            # The sole accepted non-secret change is the newly provisioned tenant gate.
            old = copy.deepcopy(spec)
            prior = self.state.get("applied_canary_organization")
            for component in old["services"] + old["jobs"]:
                for env in component.get("envs", []):
                    if env["key"] == "WHATOMATE_LEGACY_WHATSAPP_REPLY__ENABLED": env["value"] = "true" if prior else "false"
                    if env["key"] == "WHATOMATE_LEGACY_WHATSAPP_REPLY__ALLOWED_ORGANIZATION_IDS": env["value"] = prior or ""
            self.verify_spec(before["spec"], old)
            active_before = before.get("active_deployment") or {}
            require(canonical_uuid(active_before.get("id")) and active_before.get("phase") == "ACTIVE", "staging-active-deployment")
            active_before_id = active_before["id"]
            active_before = one(self.runner.do(self.config, "apps", "get-deployment", app_id, active_before_id))
            require(active_before.get("id") == active_before_id and active_before.get("phase") == "ACTIVE", "active-deployment-identity")
            self.verify_spec(active_before.get("spec", {}), old)
            require(before["spec"].get("vpc") == spec["vpc"] and not any(before.get(key) for key in
                    ("pending_deployment", "in_progress_deployment", "pinned_deployment")), "staging-app-drift")
            previous_deployment = before.get("active_deployment", {}).get("id")
        else:
            require(not self.apps and not self.state.get("app_id"), "staging-app-already-exists")
            previous_deployment = None
        for key in ("postgres_id", "valkey_id"):
            self.firewall(self.target[key])
        self.checkpoint("redeploy" if redeploy else "create-app")
        command = ["apps", "update", app_id] if redeploy else ["apps", "create"]
        app = one(self.runner.do(self.config, *command, "--spec", "-", input=json.dumps(spec).encode()))
        app_id = app.get("id")
        require(canonical_uuid(app_id) and (not redeploy or app_id == self.state["app_id"]), "created-app-identity")
        self.state["app_id"] = app_id
        save_private(self.state_path, self.state)
        deployment_id = None
        for key in ("pending_deployment", "in_progress_deployment", "active_deployment"):
            candidate = (app.get(key) or {}).get("id")
            if candidate and candidate != previous_deployment:
                require(canonical_uuid(candidate) and (deployment_id is None or deployment_id == candidate), "deployment-response-ambiguous")
                deployment_id = candidate
        require(deployment_id is not None, "deployment-response-ambiguous")
        self.state["deployment_id"] = deployment_id
        save_private(self.state_path, self.state)
        # Admission is additive until every health check passes, then replacement
        # is one nonempty request per cluster. A failure never empties either list.
        for key in ("postgres_id", "valkey_id"):
            rules = self.firewall(self.target[key])
            if not any(rule["type"] == "app" and rule["value"] == app_id for rule in rules):
                joined = sorted({f'{rule["type"]}:{rule["value"]}' for rule in rules} | {"app:" + app_id})
                self.runner.do(self.config, "databases", "firewalls", "replace", self.target[key], "--rule", ",".join(joined))
        for _ in range(120):
            app = one(self.runner.do(self.config, "apps", "get", app_id))
            active = app.get("active_deployment", {}) or {}
            pending = app.get("in_progress_deployment") or app.get("pending_deployment") or {}
            require(not app.get("pinned_deployment"), "staging-deployment-pinned")
            require(not pending or pending.get("id") == deployment_id, "staging-competing-deployment")
            require(pending.get("phase") not in {"ERROR", "CANCELED"}, "staging-deployment-failed")
            if active.get("phase") == "ACTIVE" and active.get("id") == deployment_id and not pending:
                break
            time.sleep(10)
        else:
            raise Refused("staging-deployment-timeout")
        self.verify_spec(app["spec"], spec)
        deployment = one(self.runner.do(self.config, "apps", "get-deployment", app_id, active["id"]))
        require(deployment.get("id") == deployment_id, "active-deployment-identity")
        self.verify_spec(deployment.get("spec", {}), spec)
        require(deployment.get("phase") == "ACTIVE" and do_app.migration_succeeded(
            deployment, job_name="rereply-rls-migrate", web_digest=self.latest.images["web"]), "predeploy-not-proven")
        origin = app.get("default_ingress", "").rstrip("/")
        parsed = urllib.parse.urlsplit(origin)
        require(parsed.scheme == "https" and parsed.hostname and parsed.hostname.endswith(".ondigitalocean.app") and
                not parsed.path and not parsed.query and not parsed.fragment and not parsed.username, "staging-origin")
        require(digest(origin) != self.production["default_ingress_sha256"], "production-origin")
        self.runner.health(origin)
        for key in ("postgres_id", "valkey_id"):
            self.runner.do(self.config, "databases", "firewalls", "replace", self.target[key], "--rule", "app:" + app_id)
            rules = self.firewall(self.target[key])
            require([(rule["type"], rule["value"]) for rule in rules] == [("app", app_id)], "trusted-sources-readback")
        self.state["origin_sha256"] = digest(origin)
        self.state["canary"].update(origin=origin, stub_origin=origin + "/_stub")
        self.state["applied_canary_organization"] = self.state["canary"].get("klinik_organization_id")
        self.completed()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("db", "app", "redeploy"))
    parser.add_argument("--doctl-config", type=Path, default=Path.home() / "rereply-staging/doctl.yaml")
    parser.add_argument("--target", type=Path, required=True)
    parser.add_argument("--images", type=Path, required=True)
    parser.add_argument("--private-file", type=Path, required=True)
    parser.add_argument("--operator-ip")
    args = parser.parse_args(argv)
    try:
        state = private_path(args.private_file)
        setup = Setup(Runner(), private_path(args.doctl_config), object_json(args.target.read_bytes()), object_json(args.images.read_bytes()), state)
        setup.preflight()
        setup.verify_images()
        if args.command == "db":
            setup.db(args.operator_ip)
        else:
            setup.app(args.command == "redeploy")
        print("staging-setup: complete")
        print("team_sha256=" + setup.target["team_sha256"])
        if setup.state.get("origin_sha256"):
            print("origin_sha256=" + setup.state["origin_sha256"])
        return 0
    except Refused as exc:
        print("staging-setup: refused: " + str(exc), file=sys.stderr)
    except Exception:
        print("staging-setup: refused: unexpected-failure; inspect private journal, do not retry a pending write", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
