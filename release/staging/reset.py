#!/usr/bin/env python3
"""Retain a verified staging app while the owner recreates its synthetic DB.

This adapter never archives an app, drops a database, changes a role, flushes
Valkey, or cancels a release. The owner maintains an exclusive maintenance
window. A local journal is not a lock on console, canary, or hosted callers.
Any interrupted phase remains pending and needs read-only reconciliation.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import ipaddress
import os
from pathlib import Path
import secrets
import subprocess
import sys
import urllib.parse

import setup

# setup adds the sibling deployment directory to sys.path.
import ship
import ship_common as common
import stage_contract as contract
import stage_fixture
import stage_report

require = setup.require
MAX_PRIVATE = 2 * 1024 * 1024
ACTIVE_STATUSES = ("queued", "in_progress", "waiting", "pending", "requested")
STEPS = {"db": {"prepared"}, "redeploy": {"database-ready", "fixture-ready"},
         "provision": {"disabled-ready"}, "verify": {"enabled-ready"}, "finalize": {"verified"}}
AMBIENT_PREFIXES = ("CANARY_", "STAGING_", "WHATOMATE_", "META_RELAY_", "GMAIL_RELAY_", "STUB_", "NODE_")
AMBIENT_TRANSPORT = {"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
                     "PLAYWRIGHT_TEST_BASE_URL"}


def safe_environment(env):
    require(not any(key.upper().startswith(AMBIENT_PREFIXES) or key.upper() in AMBIENT_TRANSPORT for key in env),
            "reset-ambient")


def encoded(value):
    return common.canonical_payload_bytes(value)


def fingerprint(value):
    return hashlib.sha256(encoded(value)).hexdigest()


def read_private(path, runner):
    setup.private_path(path, runner.root)
    setup.check_private_state(path, runner)
    require(path.is_file() and path.stat().st_size <= MAX_PRIVATE, "reset-private-file")
    return common.loads_strict(path.read_bytes(), code="input-invalid:reset-private-json")


def file_hash(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def operator_firewalls(kit, operator_ip):
    try:
        address = str(ipaddress.ip_address(operator_ip))
    except ValueError:
        raise setup.Refused("reset-operator-ip") from None
    for key in ("postgres_id", "valkey_id"):
        rules = kit.firewall(kit.target[key])
        require([(item["type"], item["value"]) for item in rules] == [("ip_addr", address)],
                "reset-operator-only-firewalls")
    return address


def app_firewalls(kit):
    for key in ("postgres_id", "valkey_id"):
        rules = kit.firewall(kit.target[key])
        require([(item["type"], item["value"]) for item in rules] == [("app", kit.state["app_id"])],
                "reset-app-only-firewalls")


def projected(spec, *, archived=False, private=True):
    value = copy.deepcopy(spec)
    if archived:
        # Documented AppSpec archive flag only. No instance/session visibility
        # is inferred from it: quiescence is separately confirmed by the owner.
        maintenance = value.pop("maintenance", None)
        require(type(maintenance) is dict and set(maintenance) <= {"archive", "enabled"} and
                maintenance.get("archive") is True and maintenance.get("enabled", True) is True,
                "reset-app-not-archived")
    return contract.projection(value, private_values=private)


def observe(kit, expected, *, archived=False):
    """Two equal bounded observations; never retry a changed provider response."""
    observations = []
    for _ in range(2):
        app = setup.one(kit.runner.do(kit.config, "apps", "get", kit.state["app_id"]))
        require(app.get("id") == kit.state["app_id"] and app.get("default_ingress") == kit.state["canary"]["origin"],
                "reset-app-identity")
        require(not any(app.get(key) for key in ("pending_deployment", "in_progress_deployment", "pinned_deployment")),
                "reset-app-transition")
        spec = app.get("spec")
        public = projected(spec, archived=archived, private=False)
        require(encoded(public) == encoded(projected(expected, private=False)), "reset-baseline-spec-drift")
        snapshot = {"app_id": app["id"], "origin": app["default_ingress"],
                    "spec": projected(spec, archived=archived)}
        if not archived:
            active = app.get("active_deployment") or {}
            require(setup.canonical_uuid(active.get("id")) and active.get("phase") == "ACTIVE", "reset-active-deployment")
            deployment = setup.one(kit.runner.do(kit.config, "apps", "get-deployment", app["id"], active["id"]))
            require(deployment.get("id") == active["id"] and deployment.get("phase") == "ACTIVE", "reset-active-deployment")
            require(encoded(projected(deployment.get("spec"))) == encoded(snapshot["spec"]), "reset-app-active-spec-drift")
            require(setup.do_app.migration_succeeded(deployment, job_name="rereply-rls-migrate",
                    web_digest=kit.latest.images["web"]), "reset-migration-not-proven")
            snapshot["deployment_id"] = active["id"]
        observations.append(snapshot)
    require(encoded(observations[0]) == encoded(observations[1]), "reset-observation-changed")
    return observations[0]


def same_material(left, right):
    # Archive/restore can replace deployment identity, never app/origin/spec.
    return encoded({key: left[key] for key in ("app_id", "origin", "spec")}) == encoded(
        {key: right[key] for key in ("app_id", "origin", "spec")})


class Runner(setup.Runner):
    def release_idle(self):
        for status in ACTIVE_STATUSES:
            value = setup.object_json(self.run(["gh", "api", "--hostname", "github.com",
                f"repos/{setup.REPOSITORY}/actions/workflows/ship.yml/runs?status={status}&per_page=1"]))
            require(type(value) is dict and type(value.get("total_count")) is int and value["total_count"] == 0 and
                    value.get("workflow_runs") == [], "reset-release-active-or-unknown")

    def ci(self, head):
        ship.require_ci_green(setup.LocalGh(self), head)

    def frontend(self, command, private_file, report=None):
        env = dict(os.environ)
        safe_environment(env)
        env.update(CANARY_PROFILE="staging", CANARY_PRIVATE_FILE=str(private_file), CANARY_DRILL="none")
        if command == "provision":
            args = ["node", "--experimental-strip-types", "e2e/canary/provision.ts"]
        else:
            require(command == "verify" and report is not None, "reset-frontend-command")
            env["CANARY_REPORT_FILE"] = str(report)
            args = ["node", "node_modules/@playwright/test/cli.js", "test", "--config=playwright.canary.config.ts"]
        try:
            result = subprocess.run(args, cwd=self.root / "frontend", env=env, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=1200, check=False)
            require(result.returncode == 0 and len(result.stdout) + len(result.stderr) <= 16 * 1024 * 1024,
                    "reset-frontend-failed-or-ambiguous")
        except (OSError, subprocess.SubprocessError):
            raise setup.Refused("reset-frontend-failed-or-ambiguous") from None


class RedeployRunner:
    """Add one reset-specific compare before the unchanged setup update call."""
    def __init__(self, owner, kit, expected, before):
        self.owner, self.kit, self.delegate = owner, kit, kit.runner
        self.expected, self.before = expected, before
        self.input_spec = kit.build_spec()
        self.pending_state = copy.deepcopy(kit.state)
        self.pending_state["pending_operation"] = "redeploy"
        self.attempted = False

    def __getattr__(self, name):
        return getattr(self.delegate, name)

    def do(self, config, *args, input=None):
        if args[:2] in (("apps", "update"), ("apps", "create")):
            require(args == ("apps", "update", self.before["app_id"], "--spec", "-") and not self.attempted and
                    encoded(common.loads_strict(input)) == encoded(self.input_spec), "reset-update-capability")
            self.owner.check_inputs()
            self.owner.check_source()
            require(encoded(read_private(self.owner.working_path, self.delegate)) == encoded(self.pending_state),
                    "reset-pending-working-changed")
            self.delegate.release_idle()
            app_firewalls(self.kit)
            require(encoded(observe(self.kit, self.expected)) == encoded(self.before), "reset-update-observation-changed")
            self.attempted = True  # A timeout/error consumes this capability too.
        return self.delegate.do(config, *args, input=input)


class Reset:
    def __init__(self, runner, config, target_path, images_path, private_file, reset_id):
        require(setup.canonical_uuid(reset_id), "reset-id")
        self.runner, self.reset_id = runner, reset_id
        self.source = setup.private_path(private_file, runner.root)
        self.inputs = {"config": setup.private_path(config, runner.root),
                       "target": setup.private_path(target_path, runner.root),
                       "images": setup.private_path(images_path, runner.root)}
        require(len({self.source, *self.inputs.values()}) == 4, "reset-path-alias")
        parent = self.source.parent
        self.journal_path = parent / f"reset-{reset_id}.json"
        self.backup_path = parent / f"reset-{reset_id}-original.json"
        self.working_path = parent / f"reset-{reset_id}-working.json"
        self.report_path = parent / f"reset-{reset_id}-report.json"
        self.lock_path = parent / "reset.lock"
        self.journal = None

    def kit(self, state_path):
        for path in self.inputs.values():
            setup.check_private_state(path, self.runner)
            require(path.is_file() and path.stat().st_size <= MAX_PRIVATE, "reset-input-file")
        kit = setup.Setup(self.runner, self.inputs["config"], read_private(self.inputs["target"], self.runner),
                          read_private(self.inputs["images"], self.runner), state_path)
        return kit

    def preflight(self, kit):
        safe_environment(os.environ)
        require(os.environ.get("GH_HOST", "github.com") == "github.com" and
                not any(key in os.environ for key in ("GH_REPO", "GH_TOKEN", "GITHUB_TOKEN")), "reset-gh-ambient")
        pins = common.loads_strict((self.runner.root / "release/deployment/ship-target-staging.json").read_bytes())
        contract.validate_pins(pins, kit.production)
        require(setup.digest(kit.state.get("app_id", "")) == pins["app_id_sha256"] and
                kit.target.get("team_sha256") == pins["team_uuid_sha256"], "reset-staging-pins")
        require(encoded(kit.state.get("images")) == encoded(kit.images), "reset-support-image-drift")
        head = kit.preflight()
        require(len(kit.apps) == 1 and kit.apps[0].get("id") == kit.state.get("app_id") and
                len(kit.runner.do(kit.config, "databases", "list")) == 2, "reset-dedicated-inventory")
        require(type(kit.state.get("schema_version")) is int and kit.state.get("database_verified") is True and
                setup.canonical_uuid(kit.state.get("app_id")), "reset-state-not-ready")
        require(type(kit.target.get("schema_version")) is int and kit.target["schema_version"] == 1,
                "reset-target-schema")
        require(set(kit.state["secrets"]) == set(setup.SECRET_NAMES) and
                all(type(value) is str and value for value in kit.state["secrets"].values()), "reset-secret-set")
        canary = kit.state["canary"]
        require(canary.get("admin_password") == kit.state["secrets"]["admin_password"] and
                all(canary.get(key) == kit.state["secrets"][key] for key in ("stub_control_key", "stub_access_token", "stub_app_secret")),
                "reset-secret-coherence")
        origin = contract.stage_origin(canary.get("origin"), kit.production["default_ingress_sha256"])
        require(canary.get("stub_origin") == origin + "/_stub" and kit.state.get("origin_sha256") == setup.digest(origin),
                "reset-origin-binding")
        kit.verify_images()  # Signed baseline/support images and bootstrap schema compatibility.
        self.runner.ci(head)
        self.runner.release_idle()
        return head

    def save_journal(self):
        setup.save_private(self.journal_path, self.journal)

    def arm(self, operation, payload):
        self.runner.release_idle()
        self.check_local()
        self.check_source()
        self.journal["pending"] = {"operation": operation, "payload": copy.deepcopy(payload)}
        self.save_journal()  # Never clear this after an uncertain subprocess/PUT.

    def finish(self, phase, kit):
        self.journal["working_sha256"] = file_hash(self.working_path)
        self.journal["phase"] = phase
        self.journal["completed"].append(self.journal.pop("pending"))
        self.save_journal()

    def prepare(self, *, maintenance_window):
        require(maintenance_window is True, "reset-owner-maintenance-window-required")
        require(not any(path.exists() for path in (self.journal_path, self.backup_path, self.working_path, self.report_path)),
                "reset-path-already-consumed")
        original = read_private(self.source, self.runner)
        kit = self.kit(self.source)
        input_hashes = {key: file_hash(path) for key, path in self.inputs.items()}
        head = self.preflight(kit)
        stage_fixture.export_fixture(original, kit.production["default_ingress_sha256"])
        app_firewalls(kit)
        snapshot = observe(kit, kit.build_spec())
        kit.runner.health(snapshot["origin"])
        require(encoded(snapshot) == encoded(observe(kit, kit.build_spec())), "reset-health-observation-changed")
        require(encoded(read_private(self.source, self.runner)) == encoded(original), "reset-source-changed")
        require(all(file_hash(path) == input_hashes[key] for key, path in self.inputs.items()), "reset-input-changed")
        # An immutable private original, including full secret-bearing readback,
        # is kept separately from the mutable phase journal and working copy.
        backup = {"schema_version": 1, "state": original, "snapshot": snapshot}
        marker = f"reset:{self.reset_id}"
        canonical = copy.deepcopy(original)
        canonical["pending_operation"] = marker
        working = copy.deepcopy(original)
        working["canary"].pop("fixture", None)
        working["canary"].pop("klinik_organization_id", None)
        working["canary"]["namespace"] = "rereply-staging-" + secrets.token_hex(8)
        # Keep applied_canary_organization: Setup.app needs the actual old gate
        # to validate its before-spec, then clears it after the first redeploy.
        self.journal = {"schema_version": 1, "reset_id": self.reset_id, "source_sha": head,
                        "phase": "preparing", "pending": {"operation": "prepare", "payload": canonical},
                        "inputs": input_hashes,
                        "source_path": str(self.source), "original_sha256": fingerprint(backup),
                        "canonical_sha256": fingerprint(canonical), "completed": [],
                        "owner_maintenance_window": True}
        stage_fixture.publish_private(self.journal_path, self.journal)
        stage_fixture.publish_private(self.backup_path, backup)
        stage_fixture.publish_private(self.working_path, working)
        require(encoded(read_private(self.source, self.runner)) == encoded(original), "reset-source-changed")
        setup.save_private(self.source, canonical)
        self.finish("prepared", kit)

    def load(self, command):
        self.journal = read_private(self.journal_path, self.runner)
        j = self.journal
        require(type(j.get("schema_version")) is int and j["schema_version"] == 1 and j.get("reset_id") == self.reset_id and
                j.get("source_path") == str(self.source) and not j.get("pending") and j.get("phase") in STEPS[command],
                "reset-phase-consumed-or-pending")
        self.check_local()
        kit = self.kit(self.working_path)
        require(self.preflight(kit) == j["source_sha"], "reset-source-head-changed")
        backup = read_private(self.backup_path, self.runner)
        require(fingerprint(backup) == j["original_sha256"], "reset-backup-changed")
        self.backup = backup
        return kit

    def check_local(self):
        self.check_inputs()
        read_private(self.working_path, self.runner)
        require(file_hash(self.working_path) == self.journal["working_sha256"], "reset-working-changed")

    def check_inputs(self):
        j = self.journal
        require(fingerprint(read_private(self.source, self.runner)) == j["canonical_sha256"] and
                read_private(self.source, self.runner).get("pending_operation") == f"reset:{self.reset_id}", "reset-canonical-changed")
        for key, path in self.inputs.items():
            setup.check_private_state(path, self.runner)
            require(file_hash(path) == j["inputs"][key], "reset-input-changed")

    def check_source(self):
        r, head = self.runner, self.journal["source_sha"]
        require(not r.git("status", "--porcelain", "--untracked-files=all") and
                r.git("rev-parse", "HEAD") == r.git("rev-parse", "origin/main") == head,
                "reset-source-head-changed")
        remote = setup.object_json(r.run(["gh", "api", "--hostname", "github.com", f"repos/{setup.REPOSITORY}/commits/main"]))
        require(remote.get("sha") == head, "reset-source-head-changed")

    def db(self, kit, *, operator_ip, quiesced, empty_database, owned_valkey_clean):
        require(quiesced is True and empty_database is True and owned_valkey_clean is True,
                "reset-owner-recreation-confirmation-required")
        operator_ip = operator_firewalls(kit, operator_ip)
        old = copy.copy(kit)
        old.state = copy.deepcopy(self.backup["state"])
        old.clusters, old.latest = kit.clusters, kit.latest
        snapshot = observe(kit, old.build_spec(), archived=True)
        require(same_material(snapshot, self.backup["snapshot"]), "reset-archived-material-drift")
        public, private = {}, {}
        for is_private, output in ((False, public), (True, private)):
            command = ["databases", "connection", kit.target["postgres_id"]] + (["--private"] if is_private else [])
            connection = setup.one(kit.runner.do(kit.config, *command))
            require(connection.get("ssl") is True and type(connection.get("host")) is str and connection["host"] and
                    type(connection.get("port")) is int and 0 < connection["port"] < 65536, "reset-database-connection")
            for user in ("doadmin", "rereply_app"):
                credential = setup.one(kit.runner.do(kit.config, "databases", "user", "get", kit.target["postgres_id"], user))
                require(credential.get("name") == user and type(credential.get("password")) is str and credential["password"], "reset-database-user")
                output[user] = "postgresql://{}:{}@{}:{}/rereply?sslmode=require".format(
                    user, urllib.parse.quote(credential["password"], safe=""), connection["host"], connection["port"])
        valkey = setup.one(kit.runner.do(kit.config, "databases", "connection", kit.target["valkey_id"], "--private"))
        require(encoded(private) == encoded(kit.state["database_private"]) and valkey.get("uri") == kit.state["redis_url"] and
                kit.state["redis_url"].startswith("rediss://"), "reset-credentials-changed")
        self.check_local()
        self.arm("bootstrap", {"operator_ip": operator_ip, "owner_quiesced": True, "owner_recreated_rereply": True,
                               "owner_owned_valkey_clean": True, "snapshot": snapshot})
        config = kit.runtime_config(public).encode()
        # Exact unchanged setup.db bootstrap/verifier commands and role/empty-DB
        # checks. No SQL destruction or password in argv/environment is added.
        kit.runner.run(["docker", "run", "--rm", "-i", "--platform", "linux/amd64", kit.images["bootstrap"],
                        "-config", "/dev/stdin"], input=config, timeout=1200)
        kit.runner.run(["docker", "run", "--rm", "-i", "--platform", "linux/amd64",
                        common.IMAGE_REPOSITORY["web"] + "@" + kit.latest.images["web"], "rls-migrate", "-config", "/dev/stdin"],
                       input=config, timeout=1200)
        self.finish("database-ready", kit)

    def redeploy(self, kit):
        first = self.journal["phase"] == "database-ready"
        app_firewalls(kit)
        old = copy.deepcopy(kit.build_spec())
        prior = kit.state.get("applied_canary_organization")
        for component in old["services"] + old["jobs"]:
            for env in component["envs"]:
                if env["key"] == "WHATOMATE_LEGACY_WHATSAPP_REPLY__ENABLED": env["value"] = "true" if prior else "false"
                if env["key"] == "WHATOMATE_LEGACY_WHATSAPP_REPLY__ALLOWED_ORGANIZATION_IDS": env["value"] = prior or ""
        snapshot = observe(kit, old)
        previous = self.backup["snapshot"] if first else self.journal["snapshot"]
        require(same_material(snapshot, previous) if first else encoded(snapshot) == encoded(previous),
                "reset-before-redeploy-material-drift")
        self.check_local()
        self.arm("disable-old-allowlist" if first else "enable-new-allowlist", {"before": snapshot, "spec": kit.build_spec()})
        kit.runner = RedeployRunner(self, kit, old, snapshot)
        try:
            kit.app(redeploy=True)  # Ordinary setup checks/mutations remain unchanged.
        finally:
            kit.runner = self.runner
        require(kit.state["app_id"] == self.backup["state"]["app_id"] and
                kit.state["origin_sha256"] == self.backup["state"]["origin_sha256"], "reset-retained-identity-changed")
        self.journal["snapshot"] = observe(kit, kit.build_spec())
        app_firewalls(kit)
        self.finish("disabled-ready" if first else "enabled-ready", kit)

    def provision(self, kit):
        require(not kit.state["canary"].get("fixture") and not kit.state.get("applied_canary_organization"), "reset-old-fixture-or-gate")
        app_firewalls(kit)
        require(encoded(observe(kit, kit.build_spec())) == encoded(self.journal["snapshot"]), "reset-provision-app-drift")
        before = copy.deepcopy(kit.state)
        self.check_local()
        self.arm("provision", {"working_sha256": self.journal["working_sha256"], "namespace": before["canary"]["namespace"]})
        self.runner.frontend("provision", self.working_path)
        self.check_source()
        current = read_private(self.working_path, self.runner)
        projected_state = copy.deepcopy(current)
        for key in ("fixture", "klinik_organization_id"):
            projected_state["canary"].pop(key, None)
        require(encoded(projected_state) == encoded(before), "reset-provision-state-drift")
        require(current["canary"].get("klinik_organization_id") != self.backup["state"]["canary"]["klinik_organization_id"], "reset-old-organization")
        exported = copy.deepcopy(current)
        exported["applied_canary_organization"] = current["canary"].get("klinik_organization_id")
        stage_fixture.export_fixture(exported, kit.production["default_ingress_sha256"])
        kit.state = current
        self.finish("fixture-ready", kit)

    def verify(self, kit):
        require(not self.report_path.exists(), "reset-report-already-exists")
        fixture = stage_fixture.export_fixture(kit.state, kit.production["default_ingress_sha256"])
        app_firewalls(kit)
        snapshot = observe(kit, kit.build_spec())
        require(encoded(snapshot) == encoded(self.journal["snapshot"]), "reset-verify-app-drift")
        self.check_local()
        self.arm("canary", {"source_sha": self.journal["source_sha"], "fixture_sha256": fingerprint(fixture),
                            "origin_sha256": kit.state["origin_sha256"], "namespace": kit.state["canary"]["namespace"],
                            "report_path": str(self.report_path), "working_sha256": self.journal["working_sha256"]})
        self.runner.frontend("verify", self.working_path, self.report_path)
        self.check_local()
        self.check_source()
        stage_report.exact_passes(read_private(self.report_path, self.runner))
        kit.runner.health(snapshot["origin"])
        require(encoded(observe(kit, kit.build_spec())) == encoded(snapshot), "reset-post-canary-app-drift")
        app_firewalls(kit)
        self.journal["report_sha256"] = file_hash(self.report_path)
        self.finish("verified", kit)

    def finalize(self, kit):
        stage_report.exact_passes(read_private(self.report_path, self.runner))
        require(file_hash(self.report_path) == self.journal["report_sha256"], "reset-report-changed")
        stage_fixture.export_fixture(kit.state, kit.production["default_ingress_sha256"])
        app_firewalls(kit)
        require(encoded(observe(kit, kit.build_spec())) == encoded(self.journal["snapshot"]), "reset-final-app-drift")
        kit.runner.health(kit.state["canary"]["origin"])
        require(encoded(observe(kit, kit.build_spec())) == encoded(self.journal["snapshot"]), "reset-final-health-drift")
        self.check_local()
        self.arm("publish-canonical", {"state": kit.state, "report_sha256": self.journal["report_sha256"]})
        setup.save_private(self.source, kit.state)
        self.finish("complete", kit)

    def execute(self, command, **options):
        # Canonical parent must already be owner-only. Exclusive local lock is
        # only for this adapter; a killed process leaves it for reconciliation.
        read_private(self.source, self.runner)
        descriptor = os.open(self.lock_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(descriptor)
        try:
            if command == "prepare":
                self.prepare(maintenance_window=options.get("maintenance_window"))
            else:
                require(command in STEPS, "reset-command")
                kit = self.load(command)
                if command == "db":
                    self.db(kit, operator_ip=options.get("operator_ip"), quiesced=options.get("quiesced"),
                            empty_database=options.get("empty_database"), owned_valkey_clean=options.get("owned_valkey_clean"))
                else:
                    getattr(self, command)(kit)
        finally:
            self.lock_path.unlink()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", *STEPS))
    for name in ("doctl-config", "target", "images", "private-file"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--reset-id", required=True)
    parser.add_argument("--operator-ip")
    for name in ("maintenance-window", "quiesced", "empty-database", "owned-valkey-clean"):
        parser.add_argument("--confirm-" + name, action="store_true")
    args = parser.parse_args(argv)
    try:
        reset = Reset(Runner(), args.doctl_config, args.target, args.images, args.private_file, args.reset_id)
        reset.execute(args.command, maintenance_window=args.confirm_maintenance_window, operator_ip=args.operator_ip,
                      quiesced=args.confirm_quiesced, empty_database=args.confirm_empty_database,
                      owned_valkey_clean=args.confirm_owned_valkey_clean)
        print("staging-reset: phase complete")
        return 0
    except Exception:
        # OS/provider/node/JSON/attestation errors can contain private payloads.
        print("staging-reset: refused; retain the private journal for reconciliation", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
