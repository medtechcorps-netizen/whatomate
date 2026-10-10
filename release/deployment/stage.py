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
CLONE_PHASES = ("PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING", "ACTIVE")
HEALTH_DRILL_EVIDENCE = "stage-drill: health-fail: intentional failure observed; staging restored"
FAILURE_CHECKPOINTS = frozenset({
    "preflight", "adapter-initialization", "lane-initialization", "lane",
    "deploy-preflight", "rollback-preflight", "candidate-pre-put-cas",
    "candidate-put-reconcile", "candidate-final-state", "candidate-health", "candidate-after-health",
    "receipt-validation", "receipt-sanitize", "receipt-serialization", "receipt-outputs", "receipt-emit", "status-output",
    "rollback-ownership", "rollback-prior-attestations", "rollback-pre-put-settle", "rollback-pre-put-cas",
    "rollback-put-reconcile", "rollback-final-state", "rollback-health", "rollback-after-health",
})
# ReleaseError checks syntax and the reason prefix, not whether its full detail
# is a source constant. Only these complete codes may enter a public diagnostic.
FAILURE_CODES = frozenset({
    "provider-rejected", "provider-get-failed", "provider-ambiguous",
    "provider-invalid:response-url", "provider-invalid:status", "provider-invalid:content-type",
    "provider-invalid:size", "provider-invalid:json", "provider-invalid:app", "provider-invalid:deployment",
    "provider-invalid:deployment-identity", "provider-invalid:stage-transition", "provider-invalid:stage-deployment",
    "provider-invalid:stage-updated", "cas-changed:active-not-stable", "cas-changed:deployment-pending",
    "cas-changed:stage-pinned", "cas-changed:stage-deployment", "cas-changed:stage-live-active",
    "cas-changed:stage-double-read", "cas-changed:stage-before-put", "topology-differs:stage-origin",
    "topology-differs:staging-spec", "topology-differs:staging-template",
    "reconcile-failed:stage-spec-drift", "reconcile-failed:stage-old-inflight", "reconcile-failed:stage-multiple-inflight",
    "reconcile-failed:stage-candidate-changed", "reconcile-failed:stage-deployment-identity",
    "reconcile-failed:stage-previous-clone", "reconcile-failed:stage-previous-clone-changed",
    "reconcile-timeout:stage-previous-clone",
    "reconcile-failed:stage-deployment-spec", "reconcile-failed:stage-active-spec", "reconcile-timeout:stage-candidate",
    "deployment-error:stage-terminal", "post-deploy-guard:stage-migration",
    "post-deploy-guard:stage-bad-image-unexpected-success", "post-deploy-guard:stage-final-state",
    "post-deploy-guard:stage-after-health", "post-deploy-guard:migration-inventory",
    "post-deploy-guard:stage-settle-timeout", "rollback-precondition-failed:stage-settle-timeout",
    "rollback-failed:stage-settle-timeout",
    "post-deploy-guard:migration-digest", "post-deploy-guard:migration-failed", "post-deploy-guard:migration-progress",
    "rollback-precondition-failed:stage-drift", "rollback-precondition-failed:stage-active",
    "rollback-precondition-failed:stage-candidate", "rollback-precondition-failed:stage-observation-changed",
    "rollback-precondition-failed:stage-cas", "rollback-precondition-failed:stage-cas-spec",
    "rollback-precondition-failed:stage-cas-active", "rollback-precondition-failed:stage-cas-updated",
    "rollback-failed:stage-final-state", "rollback-failed:stage-after-health",
    "smoke-failed:stage-health", "smoke-failed:stage-drill", "smoke-failed:dns", "smoke-failed:deadline",
    "smoke-failed:non-public-address", "smoke-failed:redirect", "smoke-failed:length",
    "output-unsafe:key", "output-unsafe:value", "output-unsafe:type", "output-unsafe:text",
    "output-unsafe:mask", "output-unsafe:output-key", "output-unsafe:output-value",
    "internal-error:invalid-reason-code", "internal-error:unknown-reason-code",
})


def failure_code(error):
    if isinstance(error, KeyboardInterrupt):
        return "internal-error:interrupted"
    if isinstance(error, common.ReleaseError):
        code = error.code
        if type(code) is str:
            if code in FAILURE_CODES:
                return code
            reason = code.split(":", 1)[0]
            if reason in common.REASONS:
                return reason
    return "internal-error"


def report_failure(out, error, checkpoint, attempted, original_failure=None):
    # Diagnostics cannot replace the selected exit status, even if Output's
    # privacy guard refuses a line or stdout is unavailable. No raw fallback.
    for diagnostic in (False, True):
        try:
            if diagnostic:
                phase = checkpoint if type(checkpoint) is str and checkpoint in FAILURE_CHECKPOINTS else "unknown"
                line = f"stage-diagnostic: checkpoint={phase}; code={failure_code(error)}"
            else:
                line = ("stage: failed after an attempted mutation; reconcile with reads before any retry" if attempted else
                        "stage: refused before mutation")
            out.text(line)
        except (Exception, KeyboardInterrupt):
            pass
    report_original_failure(out, original_failure)


def report_original_failure(out, original_failure):
    if type(original_failure) is tuple and len(original_failure) == 2:
        try:
            phase, code = original_failure
            phase = phase if type(phase) is str and phase in FAILURE_CHECKPOINTS else "unknown"
            code = code if type(code) is str and (code in FAILURE_CODES or code in common.REASONS or
                code == "internal-error:interrupted") else "internal-error"
            out.text(f"stage-original-failure: checkpoint={phase}; code={code}")
        except (Exception, KeyboardInterrupt):
            pass


class StageClient(do_app.DOAppClient):
    """The existing one-PUT client with a stage-only read inventory allowlist."""

    def __init__(self, target, token, *, expected_app_id_sha256, allow_put, opener=None):
        super().__init__(target["app_id"], target["postgres_id"], token,
            expected_app_id_sha256=expected_app_id_sha256, allow_put=allow_put, opener=opener)
        self.valkey_id = common.require_uuid(target["valkey_id"], "target-invalid:staging-valkey")

    def _url(self, path):
        allowed = {self.app_path, "/v2/account", "/v2/databases"}
        for cluster in (self.postgres_cluster_id, self.valkey_id):
            allowed.add(f"/v2/databases/{cluster}/firewall")
        allowed.update(f"/v2/apps?page={page}&per_page={PAGE_SIZE}" for page in range(1, MAX_PAGES + 1))
        deployment_prefix = self.app_path + "/deployments/"
        require(type(path) is str and (path in allowed or
            (path.startswith(deployment_prefix) and common.UUID_RE.fullmatch(path[len(deployment_prefix):]))), "provider-invalid:stage-path")
        return common.API_ORIGIN + path

    def _inventory_pages(self, collection):
        require(collection == "apps", "internal-error:stage-collection")
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

    def _database_inventory(self):
        # DigitalOcean lists all clusters in one unpaginated databases envelope.
        # Do not silently accept a future paginated/partial response shape.
        code = "provider-invalid:stage-databases"
        value = self._get("/v2/databases", "stage-databases", decimals=True)
        common.exact_keys(value, {"databases"}, code)
        require(type(value["databases"]) is list, code)
        return value["databases"]

    def inventory(self):
        account = self._get("/v2/account", "stage-account")
        require(type(account) is dict and type(account.get("account")) is dict, "provider-invalid:stage-account")
        apps = self._inventory_pages("apps")
        clusters = self._database_inventory()
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
    health_drill_completed: bool = False


class DeploymentFailure(common.ReleaseError):
    def __init__(self, code, candidate_id=None, previous_clone_id=None):
        super().__init__(code)
        self.candidate_id = candidate_id
        self.previous_clone_id = previous_clone_id


class IntentionalHealthFailure(common.ReleaseError):
    """Only the completed deliberate probe raises this credential-free signal."""

    def __init__(self):
        super().__init__("smoke-failed:stage-drill")


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
        self.checkpoint = "preflight"
        self.original_failure = None

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

    @staticmethod
    def same_owned_state(left, right):
        # Exclude only provider metadata time, never identity or full SECRET spec.
        return (left.deployment_id == right.deployment_id and left.images == right.images and
            left.spec_sha256 == right.spec_sha256 and
            common.canonical_payload_bytes(left.spec) == common.canonical_payload_bytes(right.spec))

    def _settle_timestamp_pair(self, read, same_material, changed_code, timeout_code):
        """GET-only: retry timestamp-only movement, with an equal pair required.

        Validation/provider errors and material movement fail immediately. This
        never replaces stable() or either strict write-time CAS comparison.
        """
        deadline = self.monotonic() + min(self.deadline_seconds, do_app.SETTLE_DEADLINE_SECONDS)
        reference = None
        for attempt in range(self.poll_limit):
            if self.monotonic() >= deadline:
                break
            left, right = read(), read()
            if reference is None:
                reference = left
            require(same_material(reference, left) and same_material(reference, right), changed_code)
            if self.monotonic() >= deadline:
                break
            if left == right:
                return right
            if attempt + 1 < self.poll_limit:
                remaining = deadline - self.monotonic()
                if remaining <= 0:
                    break
                self.sleeper(min(self.poll_seconds, remaining))
        common.fail(timeout_code)

    def settled_owned(self, client, deployment_id, spec_sha256, *, rollback=False):
        code = "rollback-failed:stage-final-state" if rollback else "post-deploy-guard:stage-final-state"
        def read():
            value = self.observe(client)
            require(value.deployment_id == deployment_id and value.spec_sha256 == spec_sha256, code)
            return value
        return self._settle_timestamp_pair(read, self.same_owned_state, code,
            "rollback-failed:stage-settle-timeout" if rollback else "post-deploy-guard:stage-settle-timeout")

    def health(self, drill="none"):
        if drill == "health-fail":
            # A missing static-style path avoids the extensionless SPA fallback
            # and its oversized index body. A bounded 200 still cannot pass the drill.
            self.request(self.target["origin"] + "/_stage_drill_intentionally_missing.txt")
            raise IntentionalHealthFailure()
        for _, path, expected in smoke.HEALTH:
            status, headers, body = self.request(self.target["origin"] + path)
            require(status == expected and not any(key.lower() == "location" for key in headers) and
                    type(body) is bytes and len(body) <= smoke.MAX_HEALTH_BODY_BYTES, "smoke-failed:stage-health")

    def previous_clone(self, client, identity, before, candidate_id):
        """Verify lineage and the full prior private spec, never image equality alone."""
        code = "reconcile-failed:stage-previous-clone"
        common.require_uuid(identity, code)
        require(identity not in {before.deployment_id, candidate_id}, code)
        clone = do_app.deployment_object(client.get_deployment(identity))
        require(clone.get("id") == identity and clone.get("cloned_from") == before.deployment_id and
                contract.spec_fingerprint(clone.get("spec")) == before.spec_sha256 and
                clone.get("phase") in CLONE_PHASES, code)
        if clone["phase"] == "ACTIVE":
            do_app.migration_succeeded(clone, job_name=common.PRE_DEPLOY_JOB, web_digest=before.images["web"])
        return clone

    @staticmethod
    def clone_summaries(app, clone, before):
        # A matching ID cannot override contradictory app-embedded evidence.
        code = "reconcile-failed:stage-previous-clone"
        for key in ("active_deployment", "pending_deployment", "in_progress_deployment"):
            summary = app.get(key)
            if type(summary) is dict and summary.get("id") == clone["id"]:
                require(summary.get("phase") == clone["phase"] and
                        summary.get("cloned_from") == before.deployment_id and
                        contract.spec_fingerprint(summary.get("spec")) == before.spec_sha256 and
                        (key != "active_deployment" or summary["phase"] == "ACTIVE"), code)

    def clone_active(self, client, app, clone, before, inflight):
        # This is read-only settling after the candidate and clone are bound.
        # It never supplies ACTIVE ownership to rollback or a write-time CAS.
        code = "reconcile-failed:stage-previous-clone-changed"
        active = app.get("active_deployment")
        if active is None:
            require(set(inflight) == {clone["id"]}, code)
            return None
        require(type(active) is dict and active.get("id") in {before.deployment_id, clone["id"]}, code)
        if active.get("phase") == "ACTIVE":
            require(active["id"] != clone["id"] or clone["phase"] == "ACTIVE", code)
            if "spec" in active:
                require(contract.spec_fingerprint(active["spec"]) == before.spec_sha256, code)
            return active["id"]
        # The prior deployment can be marked SUPERSEDED before the owned clone
        # becomes ACTIVE. Require its full spec and an agreeing direct read;
        # no other non-ACTIVE summary, including the clone itself, is accepted.
        require(active["id"] == before.deployment_id and active.get("phase") == "SUPERSEDED" and
                set(inflight) == {clone["id"]} and
                contract.spec_fingerprint(active.get("spec")) == before.spec_sha256, code)
        prior = do_app.deployment_object(client.get_deployment(before.deployment_id))
        require(prior.get("id") == before.deployment_id and prior.get("phase") == "SUPERSEDED" and
                contract.spec_fingerprint(prior.get("spec")) == before.spec_sha256 and
                prior.get("cloned_from") == active.get("cloned_from"), code)
        if prior.get("cloned_from") is not None:
            common.require_uuid(prior["cloned_from"], code)
        return None

    def reconcile(self, client, desired, before, *, candidate_id=None, excluded=()):
        wanted = contract.spec_fingerprint(desired)
        deadline = self.monotonic() + self.deadline_seconds
        excluded = set(excluded) | {before.deployment_id}
        previous_clone_id, clone_phase = None, -1
        for attempt in range(self.poll_limit):
            if previous_clone_id is not None and self.monotonic() >= deadline:
                break
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
                if candidate_id is None:
                    candidate_id = observed
            if candidate_id:
                deployment = do_app.deployment_object(client.get_deployment(candidate_id))
                require(deployment.get("id") == candidate_id, "reconcile-failed:stage-deployment-identity")
                require(contract.spec_fingerprint(deployment.get("spec")) == wanted, "reconcile-failed:stage-deployment-spec")
                changed = observed and observed not in excluded and observed != candidate_id
                if changed or previous_clone_id is not None:
                    # DigitalOcean may launch a clone of the previous ACTIVE
                    # deployment after this exact candidate fails. Keep the
                    # failed candidate pinned; the clone is never its replacement.
                    require(deployment.get("phase") == "ERROR" and app_fingerprint == wanted,
                            "reconcile-failed:stage-candidate-changed")
                    require(changed and (previous_clone_id is None or previous_clone_id == observed),
                            "reconcile-failed:stage-previous-clone-changed")
                    clone = self.previous_clone(client, observed, before, candidate_id)
                    self.clone_summaries(app, clone, before)
                    phase = CLONE_PHASES.index(clone["phase"])
                    require(phase >= clone_phase, "reconcile-failed:stage-previous-clone-changed")
                    previous_clone_id, clone_phase = observed, phase
                    active = self.clone_active(client, app, clone, before, inflight)
                    if self.monotonic() >= deadline:
                        break
                    if clone["phase"] == "ACTIVE" and active == previous_clone_id and not inflight:
                        do_app.no_transition(app)
                        raise DeploymentFailure("deployment-error:stage-terminal", candidate_id, previous_clone_id)
                    # Only this fully bound clone may settle with reads. All
                    # foreign/inconsistent IDs, specs and phases refused above.
                    if attempt + 1 == self.poll_limit:
                        break
                    self.sleeper(min(self.poll_seconds, max(0, deadline - self.monotonic())))
                    continue
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
        if previous_clone_id is not None:
            common.fail("reconcile-timeout:stage-previous-clone")
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

    def rollback_owned(self, desired, before, candidate_id, *, previous_source, previous_clone_id=None):
        """Two ownership reads allow terminal failure with old ACTIVE + new spec."""
        self.checkpoint = "rollback-ownership"
        reader = self.client(False)
        self.inventory(reader)
        wanted = contract.spec_fingerprint(desired)
        def ownership(client=reader):
            app = self.app(client)
            do_app.no_transition(app)
            require(contract.spec_fingerprint(app.get("spec")) == wanted, "rollback-precondition-failed:stage-drift")
            active = do_app.active_id(app)
            require(candidate_id is not None and active in {before.deployment_id, candidate_id, previous_clone_id}, "rollback-precondition-failed:stage-active")
            failed = do_app.deployment_object(client.get_deployment(candidate_id))
            require(failed.get("id") == candidate_id and contract.spec_fingerprint(failed.get("spec")) == wanted and
                    failed.get("phase") in {"ACTIVE", "ERROR", "CANCELED"}, "rollback-precondition-failed:stage-candidate")
            clone_spec = None
            if previous_clone_id is not None:
                require(active == previous_clone_id and failed.get("phase") == "ERROR",
                        "rollback-precondition-failed:stage-active")
                clone = self.previous_clone(client, active, before, candidate_id)
                require(clone["phase"] == "ACTIVE", "rollback-precondition-failed:stage-active")
                self.clone_summaries(app, clone, before)
                clone_spec = common.canonical_payload_bytes(clone["spec"])
            return (active, common.exact_string(app.get("updated_at"), "provider-invalid:stage-updated"),
                failed.get("phase"), common.canonical_payload_bytes(app["spec"]),
                common.canonical_payload_bytes(failed["spec"]), clone_spec)
        def same_material(left, right):
            return left[:1] + left[2:] == right[:1] + right[2:]
        reference = self._settle_timestamp_pair(ownership, same_material,
            "rollback-precondition-failed:stage-observation-changed", "rollback-precondition-failed:stage-settle-timeout")
        self.checkpoint = "rollback-prior-attestations"
        self.verify_product_images(before.images, previous_source)
        self.checkpoint = "rollback-pre-put-settle"
        writer = self.client(True)
        self.inventory(writer)
        def fresh_ownership():
            value = ownership(writer)
            # Attestations and inventory can outlast provider metadata updates.
            # Refresh only time; the original owned material remains authoritative.
            require(same_material(reference, value), "rollback-precondition-failed:stage-observation-changed")
            return value
        observed = self._settle_timestamp_pair(fresh_ownership, same_material,
            "rollback-precondition-failed:stage-observation-changed", "rollback-precondition-failed:stage-settle-timeout")
        # The equal pair does not authorize later timestamp or material movement.
        self.checkpoint = "rollback-pre-put-cas"
        if previous_clone_id is not None:
            require(ownership(writer) == observed, "rollback-precondition-failed:stage-cas")
        app = self.app(writer)
        do_app.no_transition(app)
        require(contract.spec_fingerprint(app.get("spec")) == wanted and
                common.canonical_payload_bytes(app["spec"]) == observed[3], "rollback-precondition-failed:stage-cas-spec")
        require(do_app.active_id(app) == observed[0], "rollback-precondition-failed:stage-cas-active")
        require(app.get("updated_at") == observed[1], "rollback-precondition-failed:stage-cas-updated")
        if previous_clone_id is not None:
            self.clone_summaries(app, {"id": previous_clone_id, "phase": "ACTIVE"}, before)
        # Exclude both the old ACTIVE ID and failed candidate; a fresh rollback
        # deployment must be observed even if its images equal an older ACTIVE.
        live_before = Snapshot(copy.deepcopy(desired), {}, observed[0], common.sha256_text(observed[1]), wanted)
        self.checkpoint = "rollback-put-reconcile"
        result_id = self.put_reconcile(writer, before.spec, live_before, excluded={candidate_id, before.deployment_id})
        self.checkpoint = "rollback-final-state"
        after = self.settled_owned(writer, result_id, before.spec_sha256, rollback=True)
        self.checkpoint = "rollback-health"
        self.health()
        self.checkpoint = "rollback-after-health"
        final = self.settled_owned(writer, result_id, before.spec_sha256, rollback=True)
        require(self.same_owned_state(final, after), "rollback-failed:stage-after-health")
        return Outcome("rolled-back")

    def deploy(self, candidate, *, candidate_sha256, run_id, drill="none"):
        self.original_failure = None
        try:
            return self._deploy(candidate, candidate_sha256=candidate_sha256, run_id=run_id, drill=drill)
        finally:
            self.scrub()

    def _deploy(self, candidate, *, candidate_sha256, run_id, drill):
        self.checkpoint = "deploy-preflight"
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
        self.checkpoint = "candidate-pre-put-cas"
        writer = self.client(True)
        self.inventory(writer)
        require(self.stable(writer) == before, "cas-changed:stage-before-put")
        candidate_id = None
        try:
            self.checkpoint = "candidate-put-reconcile"
            candidate_id = self.put_reconcile(writer, desired, before)
            if drill == "bad-image":
                raise DeploymentFailure("post-deploy-guard:stage-bad-image-unexpected-success", candidate_id)
            self.checkpoint = "candidate-final-state"
            after = self.settled_owned(writer, candidate_id, contract.spec_fingerprint(desired))
            self.checkpoint = "candidate-health"
            self.health(drill)
            self.checkpoint = "candidate-after-health"
            final = self.settled_owned(writer, candidate_id, after.spec_sha256)
            require(self.same_owned_state(final, after), "post-deploy-guard:stage-after-health")
        except common.ReleaseError as error:
            candidate_id = getattr(error, "candidate_id", None) or candidate_id
            if candidate_id is None:
                # Unknown ownership (including a still-pending timeout) is a
                # manual reconciliation condition, never a blind rollback PUT.
                raise
            self.original_failure = (self.checkpoint, failure_code(error))
            self.rollback_owned(desired, before, candidate_id, previous_source=previous_source,
                previous_clone_id=getattr(error, "previous_clone_id", None))
            return Outcome("failed-rolled-back", health_drill_completed=(
                drill == "health-fail" and type(error) is IntentionalHealthFailure and
                error.code == "smoke-failed:stage-drill"))
        self.checkpoint = "receipt-validation"
        value = {"schema_version": 1, "profile": "staging", "run_id": run_id, "candidate_sha256": candidate_sha256,
            "ingress_sha256": common.sha256_text(self.target["origin"]), "app_id_sha256": self.pins["app_id_sha256"], "drill": drill,
            "previous_images": before.images, "candidate_images": candidate["images"],
            "before_spec_sha256": before.spec_sha256, "after_spec_sha256": after.spec_sha256,
            "before_deployment_sha256": common.sha256_text(before.deployment_id), "candidate_deployment_sha256": common.sha256_text(after.deployment_id),
            "previous_source_sha": previous_source, "candidate_source_sha": candidate["sha"]}
        contract.validate_receipt(value)
        return Outcome("deployed", value)

    def rollback(self, receipt_b64, receipt_sha256, *, run_id, candidate_sha256):
        self.original_failure = None
        try:
            return self._rollback(receipt_b64, receipt_sha256, run_id=run_id, candidate_sha256=candidate_sha256)
        finally:
            self.scrub()

    def _rollback(self, receipt_b64, receipt_sha256, *, run_id, candidate_sha256):
        self.checkpoint = "rollback-preflight"
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
    checkpoint = "preflight"
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
        # The public template is formatted JSON; validate_target binds its
        # parsed contents to the export's canonical hash below.
        template = common.load_json(template_path or dependencies.repo_dir / "release/staging/app-spec.template.yaml", "target-invalid:staging-template", canonical=False)
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
        checkpoint = "adapter-initialization"
        adapters = adapter_factory(ctx, control)
        checkpoint = "lane-initialization"
        lane = StageLane(target=target, pins=pins, production=production, template=template, env=environment,
            client_factory=lambda *, allow_put: StageClient(target, token,
                expected_app_id_sha256=pins["app_id_sha256"], allow_put=allow_put, opener=dependencies.opener),
            verify_products=adapters.verify_products, verify_staging=adapters.verify_staging, verify_plan=adapters.verify_plan,
            request=dependencies.https_request, sleeper=dependencies.sleeper, poll_limit=dependencies.poll_limit)
        checkpoint = "lane"
        if arguments.command == "deploy":
            outcome = lane.deploy(candidate, candidate_sha256=candidate_hash, run_id=control["run_id"], drill=drill)
        else:
            outcome = lane.rollback(environment.get("STAGE_RECEIPT_B64"), environment.get("STAGE_RECEIPT_SHA256"),
                run_id=control["run_id"], candidate_sha256=candidate_hash)
        if outcome.status == "deployed":
            checkpoint = "receipt-sanitize"
            common.sanitize_public(outcome.receipt, private=out.private)
            checkpoint = "receipt-serialization"
            raw = contract.receipt_bytes(outcome.receipt)
            checkpoint = "receipt-outputs"
            out.set_outputs({"receipt_b64": base64.b64encode(raw).decode("ascii"), "receipt_sha256": common.sha256_bytes(raw)})
            checkpoint = "receipt-emit"
            out.emit(outcome.receipt)
        checkpoint = "status-output"
        out.text("stage: " + outcome.status)
        if arguments.command == "deploy" and outcome.status == "failed-rolled-back":
            report_original_failure(out, lane.original_failure)
        if (arguments.command == "deploy" and drill == "health-fail" and
                outcome.status == "failed-rolled-back" and outcome.health_drill_completed is True):
            out.text(HEALTH_DRILL_EVIDENCE)
        return ship.EXIT_OK if outcome.status in {"deployed", "rolled-back"} else ship.EXIT_ROLLED_BACK
    except (Exception, KeyboardInterrupt) as error:
        attempted = lane is not None and any(client.mutation_attempted for client in lane.clients)
        if checkpoint == "lane" and lane is not None:
            checkpoint = lane.checkpoint
        report_failure(out, error, checkpoint, attempted,
            getattr(lane, "original_failure", None) if lane is not None else None)
        return ship.EXIT_MANUAL if attempted else ship.EXIT_REFUSED
    finally:
        if installed: signal.signal(signal.SIGTERM, previous_handler)
        if lane is not None: lane.scrub()
        token, target_raw = None, None


if __name__ == "__main__": sys.exit(main())
