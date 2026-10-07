#!/usr/bin/env python3
"""Staging lifecycle invoked by ship.yml's stage mode.

The executor requires explicit attestation-verifier and plan-verifier adapters.
There is no permissive/default verifier. Provider clients have one PUT each;
rollback obtains a new capability only after revalidating ownership and CAS.
Production release functions are imported unchanged where their contract fits.
"""
from __future__ import annotations

import copy
import argparse
import base64
from dataclasses import dataclass
import os
import signal
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import do_app
import ship
import ship_common as common
import smoke
import stage_contract as contract

require = contract.require
PAGE_SIZE = 200
MAX_PAGES = 10


class StageClient(do_app.DOAppClient):
    """The existing one-PUT client with a stage-only read inventory allowlist."""

    def __init__(self, target, token, *, expected_app_id_sha256, allow_put, opener=None):
        super().__init__(target["app_id"], target["postgres_id"], token,
            expected_app_id_sha256=expected_app_id_sha256, allow_put=allow_put, opener=opener)
        self.valkey_id = common.require_uuid(target["valkey_id"], "target-invalid:staging-valkey")

    def _url(self, path):
        allowed = {self.app_path, "/v2/account"}
        for cluster in (self.postgres_cluster_id, self.valkey_id):
            allowed.add(f"/v2/databases/{cluster}/firewall")
        for collection in ("apps", "databases"):
            allowed.update(f"/v2/{collection}?page={page}&per_page={PAGE_SIZE}" for page in range(1, MAX_PAGES + 1))
        deployment_prefix = self.app_path + "/deployments/"
        require(type(path) is str and (path in allowed or
            (path.startswith(deployment_prefix) and common.UUID_RE.fullmatch(path[len(deployment_prefix):]))), "provider-invalid:stage-path")
        return common.API_ORIGIN + path

    def _inventory_pages(self, collection):
        require(collection in {"apps", "databases"}, "internal-error:stage-collection")
        result, total = [], None
        for page in range(1, MAX_PAGES + 1):
            path = f"/v2/{collection}?page={page}&per_page={PAGE_SIZE}"
            value = self._get(path, "stage-inventory", decimals=True)
            require(type(value) is dict and type(value.get(collection)) is list and type(value.get("meta")) is dict, "provider-invalid:stage-pagination")
            current_total = common.exact_int(value["meta"].get("total"), "provider-invalid:stage-pagination", 0, PAGE_SIZE * MAX_PAGES)
            require(total is None or current_total == total, "provider-invalid:stage-pagination-drift")
            total = current_total
            values = value[collection]
            require(len(values) <= PAGE_SIZE, "provider-invalid:stage-pagination")
            result.extend(values)
            links = value.get("links", {})
            require(type(links) is dict and type(links.get("pages", {})) is dict, "provider-invalid:stage-pagination")
            next_link = links.get("pages", {}).get("next")
            if next_link is None:
                require(len(result) == total, "provider-invalid:stage-pagination-incomplete")
                return result
            expected = common.API_ORIGIN + f"/v2/{collection}?page={page + 1}&per_page={PAGE_SIZE}"
            require(next_link == expected and len(values) == PAGE_SIZE and len(result) < total, "provider-invalid:stage-pagination")
        common.fail("provider-invalid:stage-pagination-limit")

    def inventory(self):
        account = self._get("/v2/account", "stage-account")
        require(type(account) is dict and type(account.get("account")) is dict, "provider-invalid:stage-account")
        apps = self._inventory_pages("apps")
        clusters = self._inventory_pages("databases")
        firewalls = {}
        for cluster in (self.postgres_cluster_id, self.valkey_id):
            value = self._get(f"/v2/databases/{cluster}/firewall", "stage-firewall")
            require(type(value) is dict and type(value.get("rules")) is list, "provider-invalid:stage-firewall")
            firewalls[cluster] = value["rules"]
        return {"account": account["account"], "apps": apps, "clusters": clusters, "firewalls": firewalls}


@dataclass(frozen=True)
class Snapshot:
    spec: dict
    images: dict
    deployment_id: str
    updated_sha256: str
    spec_sha256: str


@dataclass(frozen=True)
class Outcome:
    # Never contain a provider body, raw ID, ingress, token, or spec.
    status: str
    receipt: dict | None = None


class DeploymentFailure(common.ReleaseError):
    def __init__(self, code, candidate_id=None):
        super().__init__(code)
        self.candidate_id = candidate_id


class StageLane:
    def __init__(self, *, target, pins, production, template, env, client_factory,
                 verify_products, verify_staging, verify_plan, request=smoke.secure_https_request,
                 sleeper=time.sleep, monotonic=time.monotonic, poll_limit=90, poll_seconds=10, deadline_seconds=1200):
        # Validate before the client factory can even see a credential/request.
        contract.validate_environment(env)
        contract.validate_target(target, pins, production, template)
        require(all(callable(fn) for fn in (client_factory, verify_products, verify_staging, verify_plan, request)), "context-invalid:stage-adapter")
        common.exact_int(poll_limit, "context-invalid:stage-budget", 1, 90)
        common.exact_int(poll_seconds, "context-invalid:stage-budget", 0, 10)
        common.exact_int(deadline_seconds, "context-invalid:stage-budget", 1, 1200)
        self.target, self.pins, self.production, self.template = map(copy.deepcopy, (target, pins, production, template))
        self.factory, self.verify_products, self.verify_staging, self.verify_plan = client_factory, verify_products, verify_staging, verify_plan
        self.request, self.sleeper, self.monotonic = request, sleeper, monotonic
        self.poll_limit, self.poll_seconds, self.deadline_seconds = poll_limit, poll_seconds, deadline_seconds
        self.clients = []

    def client(self, allow_put):
        client = self.factory(allow_put=allow_put)
        require(not any(client is existing for existing in self.clients), "context-invalid:stage-reused-client")
        require(client.allow_put is allow_put and not client.mutation_attempted, "context-invalid:stage-client-capability")
        self.clients.append(client)
        return client

    def inventory(self, client):
        contract.validate_inventory(**client.inventory(), target=self.target, pins=self.pins, production=self.production)

    def verify_product_images(self, images, expected_source=None):
        source = self.verify_products(copy.deepcopy(images), expected_source)
        common.require_sha1(source, "attestation-unverified:stage-product")
        require(expected_source is None or source == expected_source, "attestation-unverified:stage-source")
        return source

    def verify_stub_images(self):
        for key in ("graph_stub", "bootstrap"):
            binding = self.target[key]
            require(self.verify_staging(copy.deepcopy(binding)) == binding["source_sha"], "attestation-unverified:stage-support")

    def app(self, client):
        app = do_app.app_object(client.get_app())
        require(app.get("id") == self.target["app_id"], "app-identity-mismatch")
        require(app.get("default_ingress") == self.target["origin"], "topology-differs:stage-origin")
        require(not app.get("pinned_deployment"), "cas-changed:stage-pinned")
        return app

    def observe(self, client):
        app = self.app(client)
        do_app.no_transition(app)
        identity = do_app.active_id(app)
        deployment = do_app.deployment_object(client.get_deployment(identity))
        require(deployment.get("id") == identity and deployment.get("phase") == "ACTIVE", "cas-changed:stage-deployment")
        images = contract.image_set(app.get("spec"))
        contract.validate_spec(app["spec"], self.target, self.template, images)
        contract.validate_spec(deployment.get("spec"), self.target, self.template, images)
        fingerprint = contract.spec_fingerprint(app["spec"])
        require(fingerprint == contract.spec_fingerprint(deployment["spec"]), "cas-changed:stage-live-active")
        do_app.migration_succeeded(deployment, job_name=common.PRE_DEPLOY_JOB, web_digest=images["web"])
        updated = common.exact_string(app.get("updated_at"), "provider-invalid:stage-updated")
        return Snapshot(copy.deepcopy(app["spec"]), images, identity, common.sha256_text(updated), fingerprint)

    def stable(self, client):
        before, after = self.observe(client), self.observe(client)
        require(before == after, "cas-changed:stage-double-read")
        return after

    def health(self, drill="none"):
        if drill == "health-fail":
            # The probe executes, but cannot accidentally turn a drill green if
            # a catch-all route unexpectedly returns 200 for the missing path.
            self.request(self.target["origin"] + "/_stage_drill_intentionally_missing")
            common.fail("smoke-failed:stage-drill")
        for _, path, expected in smoke.HEALTH:
            status, headers, body = self.request(self.target["origin"] + path)
            require(status == expected and not any(key.lower() == "location" for key in headers) and
                    type(body) is bytes and len(body) <= smoke.MAX_HEALTH_BODY_BYTES, "smoke-failed:stage-health")

    def reconcile(self, client, desired, before, *, candidate_id=None, excluded=()):
        wanted = contract.spec_fingerprint(desired)
        deadline = self.monotonic() + self.deadline_seconds
        excluded = set(excluded) | {before.deployment_id}
        for attempt in range(self.poll_limit):
            app = self.app(client)
            app_fingerprint = contract.spec_fingerprint(app.get("spec"))
            require(app_fingerprint in {wanted, before.spec_sha256}, "reconcile-failed:stage-spec-drift")
            inflight = []
            for key in ("pending_deployment", "in_progress_deployment"):
                summary = app.get(key)
                if summary is not None:
                    require(type(summary) is dict, "provider-invalid:stage-transition")
                    identity = common.require_uuid(summary.get("id"), "provider-invalid:stage-deployment")
                    require(identity not in excluded, "reconcile-failed:stage-old-inflight")
                    inflight.append(identity)
            require(len(set(inflight)) <= 1, "reconcile-failed:stage-multiple-inflight")
            observed = inflight[0] if inflight else (app.get("active_deployment") or {}).get("id")
            if observed and observed not in excluded:
                common.require_uuid(observed, "provider-invalid:stage-deployment")
                require(candidate_id is None or candidate_id == observed, "reconcile-failed:stage-candidate-changed")
                candidate_id = observed
            if candidate_id:
                deployment = do_app.deployment_object(client.get_deployment(candidate_id))
                require(deployment.get("id") == candidate_id, "reconcile-failed:stage-deployment-identity")
                require(contract.spec_fingerprint(deployment.get("spec")) == wanted, "reconcile-failed:stage-deployment-spec")
                if deployment.get("phase") in {"ERROR", "CANCELED"}:
                    raise DeploymentFailure("deployment-error:stage-terminal", candidate_id)
                active = app.get("active_deployment") or {}
                if deployment.get("phase") == "ACTIVE" and active.get("id") == candidate_id and not inflight:
                    require(app_fingerprint == wanted and active.get("phase") == "ACTIVE", "reconcile-failed:stage-active-spec")
                    do_app.no_transition(app)
                    try:
                        web_digest = spec_images_web_digest(desired)
                        do_app.migration_succeeded(deployment, job_name=common.PRE_DEPLOY_JOB, web_digest=web_digest)
                    except common.ReleaseError as error:
                        raise DeploymentFailure("post-deploy-guard:stage-migration", candidate_id) from error
                    return candidate_id
            if attempt + 1 == self.poll_limit or self.monotonic() >= deadline:
                break
            self.sleeper(self.poll_seconds)
        raise DeploymentFailure("reconcile-timeout:stage-candidate", candidate_id)

    def put_reconcile(self, client, desired, before, *, excluded=()):
        candidate = None
        try:
            response = client.put_app_once(desired)
            app = do_app.app_object(response)
            require(app.get("id") == self.target["app_id"], "provider-ambiguous:stage-put-identity")
            candidates = set()
            for key in ("pending_deployment", "in_progress_deployment", "active_deployment"):
                summary = app.get(key)
                if summary is not None:
                    require(type(summary) is dict, "provider-ambiguous:stage-put-shape")
                    identity = common.require_uuid(summary.get("id"), "provider-ambiguous:stage-put-identity")
                    if identity not in set(excluded) | {before.deployment_id}:
                        candidates.add(identity)
            require(len(candidates) <= 1, "provider-ambiguous:stage-put-candidates")
            candidate = next(iter(candidates), None)
        except common.AmbiguousMutation:
            # The existing client counts an attempted PUT before opening the
            # socket. There is never a second candidate PUT after ambiguity.
            pass
        except common.ReleaseError as error:
            if error.reason == "provider-rejected":
                raise
            if not client.mutation_attempted:
                raise
            # A malformed successful response is ambiguous too; reads only.
        return self.reconcile(client, desired, before, candidate_id=candidate, excluded=excluded)

    def rollback_owned(self, desired, before, candidate_id, *, previous_source):
        """Two ownership reads allow terminal failure with old ACTIVE + new spec."""
        reader = self.client(False)
        self.inventory(reader)
        wanted = contract.spec_fingerprint(desired)
        observations = []
        for _ in range(2):
            app = self.app(reader)
            do_app.no_transition(app)
            require(contract.spec_fingerprint(app.get("spec")) == wanted, "rollback-precondition-failed:stage-drift")
            active = do_app.active_id(app)
            require(candidate_id is not None and active in {before.deployment_id, candidate_id}, "rollback-precondition-failed:stage-active")
            failed = do_app.deployment_object(reader.get_deployment(candidate_id))
            require(failed.get("id") == candidate_id and contract.spec_fingerprint(failed.get("spec")) == wanted and
                    failed.get("phase") in {"ACTIVE", "ERROR", "CANCELED"}, "rollback-precondition-failed:stage-candidate")
            observations.append((active, common.exact_string(app.get("updated_at"), "provider-invalid:stage-updated"), failed.get("phase")))
        require(observations[0] == observations[1], "rollback-precondition-failed:stage-observation-changed")
        self.verify_product_images(before.images, previous_source)
        # CAS immediately before rollback's fresh one-PUT capability is used.
        writer = self.client(True)
        self.inventory(writer)
        app = self.app(writer)
        do_app.no_transition(app)
        require(contract.spec_fingerprint(app.get("spec")) == wanted and do_app.active_id(app) == observations[-1][0] and
                app.get("updated_at") == observations[-1][1], "rollback-precondition-failed:stage-cas")
        # Exclude both the old ACTIVE ID and failed candidate; a fresh rollback
        # deployment must be observed even if its images equal an older ACTIVE.
        live_before = Snapshot(copy.deepcopy(desired), {}, observations[-1][0],
            common.sha256_text(observations[-1][1]), wanted)
        result_id = self.put_reconcile(writer, before.spec, live_before, excluded={candidate_id, before.deployment_id})
        after = self.stable(writer)
        require(after.deployment_id == result_id and after.spec_sha256 == before.spec_sha256, "rollback-failed:stage-final-state")
        self.health()
        require(self.stable(writer) == after, "rollback-failed:stage-after-health")
        return Outcome("rolled-back")

    def deploy(self, candidate, *, candidate_sha256, run_id, drill="none"):
        try:
            return self._deploy(candidate, candidate_sha256=candidate_sha256, run_id=run_id, drill=drill)
        finally:
            self.scrub()

    def _deploy(self, candidate, *, candidate_sha256, run_id, drill):
        contract.validate_drill("stage", drill)
        common.require_run_id(run_id, "input-invalid:stage-run")
        require(type(run_id) is str, "input-invalid:stage-run")
        ship.validate_candidate(candidate)
        require(common.sha256_bytes(common.canonical_file_bytes(candidate)) == candidate_sha256, "candidate-invalid:stage-hash")
        # The adapter must validate main/CI/schema/no-downgrade/Trivy/freshness
        # and this exact canonical candidate, not merely return its image set.
        require(self.verify_plan(copy.deepcopy(candidate), candidate_sha256, run_id) is True, "candidate-invalid:stage-plan")
        self.verify_stub_images()
        self.verify_product_images(candidate["images"], candidate["sha"])
        reader = self.client(False)
        self.inventory(reader)
        before = self.stable(reader)
        previous_source = self.verify_product_images(before.images)
        desired = contract.set_images(before.spec, candidate["images"])
        if drill == "bad-image":
            desired = contract.bad_image_spec(desired, self.target, mode="stage", drill=drill)
        writer = self.client(True)
        self.inventory(writer)
        require(self.stable(writer) == before, "cas-changed:stage-before-put")
        candidate_id = None
        try:
            candidate_id = self.put_reconcile(writer, desired, before)
            if drill == "bad-image":
                raise DeploymentFailure("post-deploy-guard:stage-bad-image-unexpected-success", candidate_id)
            after = self.stable(writer)
            require(after.deployment_id == candidate_id and after.spec_sha256 == contract.spec_fingerprint(desired), "post-deploy-guard:stage-final-state")
            self.health(drill)
            final = self.stable(writer)
            require(final == after, "post-deploy-guard:stage-after-health")
        except common.ReleaseError as error:
            candidate_id = getattr(error, "candidate_id", None) or candidate_id
            if candidate_id is None:
                # Unknown ownership (including a still-pending timeout) is a
                # manual reconciliation condition, never a blind rollback PUT.
                raise
            self.rollback_owned(desired, before, candidate_id, previous_source=previous_source)
            return Outcome("failed-rolled-back")
        value = {"schema_version": 1, "profile": "staging", "run_id": run_id, "candidate_sha256": candidate_sha256,
            "ingress_sha256": common.sha256_text(self.target["origin"]), "app_id_sha256": self.pins["app_id_sha256"], "drill": drill,
            "previous_images": before.images, "candidate_images": candidate["images"],
            "before_spec_sha256": before.spec_sha256, "after_spec_sha256": after.spec_sha256,
            "before_deployment_sha256": common.sha256_text(before.deployment_id), "candidate_deployment_sha256": common.sha256_text(after.deployment_id),
            "previous_source_sha": previous_source, "candidate_source_sha": candidate["sha"]}
        contract.validate_receipt(value)
        return Outcome("deployed", value)

    def rollback(self, receipt_b64, receipt_sha256, *, run_id, candidate_sha256):
        try:
            return self._rollback(receipt_b64, receipt_sha256, run_id=run_id, candidate_sha256=candidate_sha256)
        finally:
            self.scrub()

    def _rollback(self, receipt_b64, receipt_sha256, *, run_id, candidate_sha256):
        receipt = contract.decode_receipt(receipt_b64, receipt_sha256, run_id=run_id,
            candidate_sha256=candidate_sha256, origin=self.target["origin"],
            production_origin_sha256=self.production["default_ingress_sha256"], app_id_sha256=self.pins["app_id_sha256"])
        require(receipt["drill"] in {"none", "e2e-fail"}, "rollback-precondition-failed:stage-drill")
        self.verify_stub_images()
        self.verify_product_images(receipt["candidate_images"], receipt["candidate_source_sha"])
        self.verify_product_images(receipt["previous_images"], receipt["previous_source_sha"])
        reader = self.client(False)
        self.inventory(reader)
        current = self.stable(reader)
        require(common.sha256_text(current.deployment_id) == receipt["candidate_deployment_sha256"] and
                current.spec_sha256 == receipt["after_spec_sha256"] and current.images == receipt["candidate_images"], "rollback-precondition-failed:stage-receipt-state")
        prior_spec = contract.set_images(current.spec, receipt["previous_images"])
        require(contract.spec_fingerprint(prior_spec) == receipt["before_spec_sha256"], "rollback-precondition-failed:stage-prior-spec")
        # Cross-job rollback never needs the old raw ID: current is the sole
        # permitted ACTIVE baseline, and its ID is excluded during reconciliation.
        before = Snapshot(prior_spec, receipt["previous_images"], current.deployment_id, current.updated_sha256, receipt["before_spec_sha256"])
        return self.rollback_owned(current.spec, before, current.deployment_id, previous_source=receipt["previous_source_sha"])

    def scrub(self):
        for client in self.clients:
            client.scrub()


def spec_images_web_digest(spec):
    services = {component["name"]: component for component in spec["services"]}
    return common.require_digest(services["omnitech-web"]["image"]["digest"], "topology-differs:stage-web-digest")


def _mask_target(out, target):
    # Do not mask public enum names or image/source hashes: those are part of
    # the public receipt. Every raw provider/fixture identity is private.
    for key in ("app_id", "origin", "postgres_id", "valkey_id", "vpc_id", "postgres_name"):
        out.add_mask(target[key])
    values = target["template_values"]
    for key in ("admin_email", "stub_app_id", "canary_organization"):
        out.add_mask(values[key])
    for account in values["stub_accounts"]:
        for value in account.values(): out.add_mask(value)


def main(argv=None, env=None, deps=None, *, adapter_factory=None, pins_path=None, template_path=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("deploy", "rollback"))
    arguments = parser.parse_args(argv)
    environment = os.environ if env is None else env
    # Remove protected inputs before Context construction or any subprocess.
    token = environment.pop("STAGING_DO_TOKEN", None)
    target_raw = environment.pop("STAGING_TARGET_JSON", None)
    dependencies = deps or ship.Deps()
    out = common.Output(stdout=dependencies.stdout or sys.stdout,
        output_path=environment.get("GITHUB_OUTPUT"), summary_path=environment.get("GITHUB_STEP_SUMMARY"))
    lane = None
    installed = threading.current_thread() is threading.main_thread()
    previous_handler = signal.signal(signal.SIGTERM, ship._interrupt) if installed else None
    try:
        contract.validate_environment(environment)
        require(not any(key.startswith("STAGING_") for key in environment), "context-invalid:stage-mixed-secrets")
        require(environment.get("SHIP_MODE") == "stage", "input-invalid:stage-mode")
        drill = contract.validate_drill("stage", environment.get("SHIP_DRILL", "none"))
        require(not any(environment.get(key) for key in ("SHIP_TARGET_RELEASE", "PLAN_TARGET_RELEASE", "PLAN_TARGET_MANIFEST_SHA256")), "input-invalid:stage-production-target")
        control = common.validate_context(environment)
        common.exact_string(token, "target-invalid:token")
        require(len(token) >= 20 and not any(ch.isspace() for ch in token), "target-invalid:token")
        out.add_mask(token)
        target = common.loads_strict(target_raw, code="target-invalid:staging-target")
        production = common.load_json(dependencies.target_path, "target-invalid:production")
        pins = common.load_json(pins_path or dependencies.repo_dir / "release/deployment/ship-target-staging.json", "target-invalid:staging-pins")
        template = common.load_json(template_path or dependencies.repo_dir / "release/staging/app-spec.template.yaml", "target-invalid:staging-template")
        contract.validate_target(target, pins, production, template)
        _mask_target(out, target)
        ctx = ship.Context(environment, dependencies, out)
        candidate = ship.decode_candidate(environment, control, ctx.now())
        candidate_hash = environment["CANDIDATE_SHA256"]
        if arguments.command == "rollback":
            receipt = contract.decode_receipt(environment.get("STAGE_RECEIPT_B64"), environment.get("STAGE_RECEIPT_SHA256"),
                run_id=control["run_id"], candidate_sha256=candidate_hash, origin=target["origin"],
                production_origin_sha256=production["default_ingress_sha256"], app_id_sha256=pins["app_id_sha256"])
            require(receipt["candidate_images"] == candidate["images"] and receipt["candidate_source_sha"] == candidate["sha"] and
                    receipt["drill"] == drill, "rollback-precondition-failed:stage-candidate-binding")
        if adapter_factory is None:
            # The separate integration commit supplies real verification. A
            # missing module fails closed; there is no permissive fallback.
            from stage_adapters import make_adapters
            adapter_factory = make_adapters
        adapters = adapter_factory(ctx, control)
        lane = StageLane(target=target, pins=pins, production=production, template=template, env=environment,
            client_factory=lambda *, allow_put: StageClient(target, token,
                expected_app_id_sha256=pins["app_id_sha256"], allow_put=allow_put, opener=dependencies.opener),
            verify_products=adapters.verify_products, verify_staging=adapters.verify_staging, verify_plan=adapters.verify_plan,
            request=dependencies.https_request, sleeper=dependencies.sleeper, poll_limit=dependencies.poll_limit)
        if arguments.command == "deploy":
            outcome = lane.deploy(candidate, candidate_sha256=candidate_hash, run_id=control["run_id"], drill=drill)
        else:
            outcome = lane.rollback(environment.get("STAGE_RECEIPT_B64"), environment.get("STAGE_RECEIPT_SHA256"),
                run_id=control["run_id"], candidate_sha256=candidate_hash)
        if outcome.status == "deployed":
            common.sanitize_public(outcome.receipt, private=out.private)
            raw = contract.receipt_bytes(outcome.receipt)
            out.set_outputs({"receipt_b64": base64.b64encode(raw).decode("ascii"), "receipt_sha256": common.sha256_bytes(raw)})
            out.emit(outcome.receipt)
        out.text("stage: " + outcome.status)
        return ship.EXIT_OK if outcome.status in {"deployed", "rolled-back"} else ship.EXIT_ROLLED_BACK
    except (Exception, KeyboardInterrupt):
        attempted = lane is not None and any(client.mutation_attempted for client in lane.clients)
        out.text("stage: failed after an attempted mutation; reconcile with reads before any retry" if attempted else "stage: refused before mutation")
        return ship.EXIT_MANUAL if attempted else ship.EXIT_REFUSED
    finally:
        if installed: signal.signal(signal.SIGTERM, previous_handler)
        if lane is not None: lane.scrub()
        token, target_raw = None, None


if __name__ == "__main__": sys.exit(main())
