"""Staging lifecycle state-machine tests. Every provider and probe is synthetic."""
import base64
import copy
import datetime as dt
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import do_app
import ship_common as common
import stage
import stage_contract as contract
from test_stage_contract import APP, PG, VK, VPC, ORIGIN, TEAM, IMAGES, NEW, data
from test_stage_contract import receipt as synthetic_receipt
from test_ship_smoke import FakeSocket, FakeContext, addresses

OLD_ID = "77777777-7777-4777-8777-777777777777"
OLD_SOURCE, NEW_SOURCE = "a" * 40, "b" * 40


def candidate():
    return {"schema_version": 1, "sha": NEW_SOURCE, "workflow_sha": NEW_SOURCE,
        "built_at": "2026-10-07T12:00:00Z", "images": NEW.copy(),
        "trivy": {"db_updated_at": "2026-10-07T11:00:00Z", "active_exceptions": 0}}


class World:
    def __init__(self):
        self.production, self.template, self.pins, self.target = data()
        self.spec = contract.expected_spec(self.target, self.template, IMAGES)
        self.active = OLD_ID
        self.deployments = {OLD_ID: self.deployment(OLD_ID, self.spec, "ACTIVE")}
        self.updated = "2026-10-07T11:00:00Z"
        self.pending = None
        self.pinned = None
        self.writes, self.clients, self.probes, self.verified = [], [], [], []
        self.inventory_hook, self.app_hook = None, None
        self.put_outcome = "active"
        self.health_failure = False
        self.fail_previous_attestation = False
        self.fail_candidate_attestation = False
        self.fail_plan = False
        self.get_count = 0

    def deployment(self, identity, spec, phase):
        return {"id": identity, "phase": phase, "spec": copy.deepcopy(spec),
            "jobs": [{"name": common.PRE_DEPLOY_JOB, "phase": "SUCCEEDED", "source_image_digest": stage.spec_images_web_digest(spec)}]}

    def app(self):
        self.get_count += 1
        if self.app_hook: self.app_hook(self)
        return {"id": APP, "default_ingress": ORIGIN, "updated_at": self.updated, "spec": copy.deepcopy(self.spec),
            "active_deployment": {"id": self.active, "phase": "ACTIVE"},
            "pending_deployment": self.pending, "in_progress_deployment": None, "pinned_deployment": self.pinned}

    def inventory(self):
        value = {"account": {"status": "active", "team": {"name": "ReReply Staging", "uuid": TEAM}},
            "apps": [{"id": APP}], "clusters": [
            {"id": PG, "name": "rereply-staging-pg", "engine": "pg", "version": "17", "status": "online", "region": "sgp1", "private_network_uuid": VPC},
            {"id": VK, "name": "rereply-staging-valkey", "engine": "valkey", "status": "online", "region": "sgp1", "private_network_uuid": VPC}],
            "firewalls": {key: [{"type": "app", "value": APP}] for key in (PG, VK)}}
        if self.inventory_hook: self.inventory_hook(value)
        return value

    def factory(self, *, allow_put):
        client = FakeClient(self, allow_put)
        self.clients.append(client)
        return client

    def verify_products(self, images, source):
        self.verified.append((images.copy(), source))
        expected = OLD_SOURCE if images == IMAGES else NEW_SOURCE if images == NEW else None
        if (images == IMAGES and self.fail_previous_attestation) or (images == NEW and self.fail_candidate_attestation):
            common.fail("attestation-unverified:synthetic")
        if expected is None or (source is not None and source != expected): common.fail("attestation-unverified:synthetic")
        return expected

    def request(self, url):
        self.probes.append(url)
        path = url.removeprefix(ORIGIN)
        if self.health_failure:
            self.health_failure = False
            return 503, {}, b"synthetic failure"
        if path == "/_stage_drill_intentionally_missing.txt": return 404, {}, b""
        statuses = {path: status for _, path, status in stage.smoke.HEALTH}
        return statuses[path], {}, b""

    def lane(self, **changes):
        values = dict(target=self.target, pins=self.pins, production=self.production, template=self.template,
            env={}, client_factory=self.factory, verify_products=self.verify_products,
            verify_staging=lambda binding: binding["source_sha"], verify_plan=lambda *args: not self.fail_plan,
            request=self.request, sleeper=lambda _: None, monotonic=lambda: 0, poll_limit=3, poll_seconds=0)
        values.update(changes)
        return stage.StageLane(**values)

    def deploy(self, lane=None, drill="none"):
        value = candidate()
        return (lane or self.lane()).deploy(value, candidate_sha256=common.sha256_bytes(common.canonical_file_bytes(value)), run_id="1234567", drill=drill)


class FakeClient:
    def __init__(self, world, allow_put):
        self.world, self.allow_put = world, allow_put
        self.mutation_attempted = False
        self.mutation_ambiguous = False
        self.puts = 0
        self.scrubbed = False

    def inventory(self): return self.world.inventory()
    def get_app(self): return {"app": self.world.app()}
    def get_deployment(self, identity): return {"deployment": copy.deepcopy(self.world.deployments[identity])}
    def scrub(self): self.scrubbed = True

    def put_app_once(self, spec):
        if not self.allow_put: common.fail("dry-run-client-cannot-mutate")
        if self.mutation_attempted: common.fail("second-mutation-blocked")
        self.mutation_attempted = True
        self.puts += 1
        world = self.world
        if world.put_outcome == "rejected": common.fail("provider-rejected")
        world.writes.append(copy.deepcopy(spec))
        identity = f"{len(world.writes) + 8:08d}-8888-4888-8888-888888888888"
        bootstrap = spec["services"][0]["image"]["repository"].endswith("staging-bootstrap")
        phase = "ERROR" if bootstrap or world.put_outcome == "terminal" else "DEPLOYING" if world.put_outcome == "pending" else "ACTIVE"
        world.deployments[identity] = world.deployment(identity, spec, phase)
        world.spec = copy.deepcopy(spec)
        world.updated = "2026-10-07T12:00:0" + str(len(world.writes)) + "Z"
        if phase == "ACTIVE": world.active = identity
        world.pending = {"id": identity, "phase": phase} if phase == "DEPLOYING" else None
        response = {"app": world.app()}
        response["app"]["pending_deployment"] = {"id": identity, "phase": phase}
        outcome = world.put_outcome
        world.put_outcome = "active"
        if outcome == "ambiguous":
            self.mutation_ambiguous = True
            raise common.AmbiguousMutation("provider-ambiguous")
        return response


class ProviderCloneWorld(World):
    """One failed candidate followed by a provider clone, without an extra PUT."""
    CLONE = "99999999-9999-4999-8999-999999999999"

    def __init__(self, phases=("DEPLOYING", "ACTIVE")):
        super().__init__()
        self.prior = copy.deepcopy(self.spec)
        self.clone_phases, self.clone_reads = phases, 0
        self.failed_id = None
        self.clone_hook, self.snapshot_hook = None, None
        self.omit_put_hint = False
        self.candidate_first = False
        self.candidate_polls = 0

    def factory(self, *, allow_put):
        client = ProviderCloneClient(self, allow_put)
        self.clients.append(client)
        return client


class ProviderCloneClient(FakeClient):
    def put_app_once(self, spec):
        response = super().put_app_once(spec)
        world = self.world
        if len(world.writes) == 1:
            world.failed_id = response["app"]["pending_deployment"]["id"]
            world.deployments[world.failed_id]["jobs"][0]["phase"] = "FAILED"
            clone = world.deployment(world.CLONE, world.prior, world.clone_phases[0])
            clone["cloned_from"] = OLD_ID
            world.deployments[world.CLONE] = clone
            if world.omit_put_hint:
                response["app"]["pending_deployment"] = None
        return response

    def get_app(self):
        world = self.world
        if len(world.writes) == 1:
            if world.candidate_first and world.candidate_polls == 0:
                world.candidate_polls += 1
                world.deployments[world.failed_id]["phase"] = "DEPLOYING"
                world.pending = {"id": world.failed_id, "phase": "DEPLOYING"}
                return super().get_app()
            if world.candidate_first and world.candidate_polls == 1:
                world.candidate_polls += 1
                world.deployments[world.failed_id]["phase"] = "ERROR"
            phase = world.clone_phases[min(world.clone_reads, len(world.clone_phases) - 1)]
            world.clone_reads += 1
            world.deployments[world.CLONE]["phase"] = phase
            world.active = world.CLONE if phase == "ACTIVE" else OLD_ID
            world.pending = None if phase == "ACTIVE" else {"id": world.CLONE, "phase": phase}
            world.updated = "2026-10-07T12:02:00Z"
            if world.clone_hook:
                world.clone_hook(world)
        response = super().get_app()
        if len(world.writes) == 1:
            for key in ("active_deployment", "pending_deployment", "in_progress_deployment"):
                if (response["app"].get(key) or {}).get("id") == world.CLONE:
                    response["app"][key] = copy.deepcopy(world.deployments[world.CLONE])
            if world.snapshot_hook:
                world.snapshot_hook(response["app"])
        return response

    def get_deployment(self, identity):
        if identity not in self.world.deployments:
            common.fail("provider-get-failed")
        return super().get_deployment(identity)


class LifecycleTests(unittest.TestCase):
    def test_provider_previous_clone_settles_then_restores_full_prior_spec(self):
        for phases in (("ACTIVE",), ("DEPLOYING", "ACTIVE"),
                       ("PENDING_BUILD", "BUILDING", "PENDING_DEPLOY", "DEPLOYING", "ACTIVE")):
            world = ProviderCloneWorld(phases); lane = world.lane(poll_limit=6)
            with self.subTest(phases=phases):
                result = world.deploy(lane, drill="bad-image")
                self.assertEqual(result.status, "failed-rolled-back")
                self.assertEqual(world.spec, world.prior)
                self.assertEqual(world.writes[1], world.prior)
                self.assertNotEqual(world.writes[0]["services"][0]["image"], world.prior["services"][0]["image"])
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(sum(c.puts for c in world.clients), 2)
                self.assertTrue(all(c.puts <= 1 and c.scrubbed for c in world.clients))
                self.assertEqual(len(world.probes), 6)
                self.assertNotIn(world.active, (OLD_ID, world.failed_id, world.CLONE))
                self.assertEqual(lane.original_failure,
                    ("candidate-put-reconcile", "deployment-error:stage-terminal"))
        world = ProviderCloneWorld(); world.omit_put_hint = world.candidate_first = True
        self.assertEqual(world.deploy(world.lane(poll_limit=4), drill="bad-image").status, "failed-rolled-back")
        self.assertEqual(world.candidate_polls, 2)  # The failed ID can be pinned by the first poll.
        self.assertEqual(world.writes[1], world.prior)
        self.assertEqual(len(world.writes), 2)
        self.assertEqual(len(world.probes), 6)

    def test_provider_clone_rejects_conflicting_app_summaries(self):
        for key in ("active_deployment", "pending_deployment", "in_progress_deployment"):
            for field in ("phase", "lineage", "missing-spec", "secret"):
                world = ProviderCloneWorld(("ACTIVE",) if key == "active_deployment" else ("DEPLOYING",))
                def conflict(app):
                    if key == "in_progress_deployment":
                        app[key] = copy.deepcopy(app["pending_deployment"])
                    summary = app[key]
                    if field == "phase": summary["phase"] = "ERROR"
                    elif field == "lineage": summary["cloned_from"] = PG
                    elif field == "missing-spec": summary.pop("spec")
                    else: next(e for s in summary["spec"]["services"] for e in s["envs"] if e["type"] == "SECRET")["value"] = "EV[foreign]"
                world.snapshot_hook = conflict
                with self.subTest(key=key, field=field), self.assertRaises(common.ReleaseError):
                    world.deploy(drill="bad-image")
                self.assertEqual(len(world.writes), 1)
                self.assertEqual(len(world.clients), 2)
                self.assertEqual(world.probes, [])

        for boundary in ("reader", "writer", "final-cas"):
            for field in ("phase", "lineage", "secret"):
                world = ProviderCloneWorld(("ACTIVE",)); writer_reads = []
                def late_conflict(app):
                    if len(world.clients) == 4: writer_reads.append(1)
                    if ((boundary == "reader" and len(world.clients) == 3) or
                        (boundary == "writer" and len(world.clients) == 4) or
                        (boundary == "final-cas" and len(writer_reads) == 2)):
                        summary = app["active_deployment"]
                        if field == "phase": summary["phase"] = "ERROR"
                        elif field == "lineage": summary["cloned_from"] = PG
                        else: next(e for s in summary["spec"]["services"] for e in s["envs"] if e["type"] == "SECRET")["value"] = "EV[foreign]"
                world.snapshot_hook = late_conflict
                with self.subTest(boundary=boundary, field=field), self.assertRaises(common.ReleaseError):
                    world.deploy(drill="bad-image")
                self.assertEqual(len(world.writes), 1)
                self.assertEqual(world.probes, [])

    def test_provider_clone_refuses_lineage_full_spec_and_candidate_drift(self):
        mutations = {
            "missing-lineage": lambda w: w.deployments[w.CLONE].pop("cloned_from", None),
            "foreign-lineage": lambda w: w.deployments[w.CLONE].update(cloned_from=PG),
            "clone-identity": lambda w: w.deployments[w.CLONE].update(id=PG),
            "selector": lambda w: w.deployments[w.CLONE]["spec"]["services"][0]["image"].update(repository="foreign"),
            "general": lambda w: next(e for s in w.deployments[w.CLONE]["spec"]["services"] for e in s["envs"] if e["type"] == "GENERAL").update(value="foreign"),
            "secret": lambda w: next(e for s in w.deployments[w.CLONE]["spec"]["services"] for e in s["envs"] if e["type"] == "SECRET").update(value="EV[foreign]"),
            "candidate-spec": lambda w: w.deployments[w.failed_id].update(spec=copy.deepcopy(w.prior)),
            "candidate-not-failed": lambda w: w.deployments[w.failed_id].update(phase="DEPLOYING"),
            "candidate-canceled": lambda w: w.deployments[w.failed_id].update(phase="CANCELED"),
            "clone-migration": lambda w: w.deployments[w.CLONE]["jobs"][0].update(phase="FAILED"),
        }
        for name, change in mutations.items():
            world = ProviderCloneWorld(("ACTIVE",)); world.clone_hook = change
            with self.subTest(name=name), self.assertRaises(common.ReleaseError):
                world.deploy(drill="bad-image")
            self.assertEqual(len(world.writes), 1)
            self.assertEqual(len(world.clients), 2)
            self.assertEqual(world.probes, [])
            self.assertTrue(all(c.scrubbed and c.puts <= 1 for c in world.clients))

    def test_provider_clone_does_not_adopt_unknown_multiple_or_changed_inflight(self):
        mutations = {
            "unknown": lambda app: app.update(pending_deployment={"id": PG}),
            "multiple": lambda app: app.update(in_progress_deployment={"id": PG}),
            "old": lambda app: app.update(pending_deployment={"id": OLD_ID}),
            "malformed": lambda app: app.update(pending_deployment={"id": None}),
            "pinned": lambda app: app.update(pinned_deployment={"id": PG}),
            "ingress": lambda app: app.update(default_ingress="https://foreign.example"),
            "foreign-active": lambda app: app.update(active_deployment={"id": PG, "phase": "ACTIVE"}),
        }
        for name, change in mutations.items():
            world = ProviderCloneWorld(("DEPLOYING",)); world.snapshot_hook = change
            with self.subTest(name=name), self.assertRaises(common.ReleaseError):
                world.deploy(drill="bad-image")
            self.assertEqual(len(world.writes), 1)
            self.assertEqual(world.probes, [])
        world = ProviderCloneWorld(("ACTIVE",)); world.omit_put_hint = True
        with self.assertRaisesRegex(common.ReleaseError, "stage-deployment-spec"):
            world.deploy(drill="bad-image")
        self.assertEqual(len(world.writes), 1)  # Never discover the failed ID by guessing/history.
        world = ProviderCloneWorld(("DEPLOYING",))
        def changed(app):
            if world.clone_reads == 2:
                app["pending_deployment"] = {"id": PG}
        world.snapshot_hook = changed
        with self.assertRaisesRegex(common.ReleaseError, "stage-previous-clone-changed"):
            world.deploy(drill="bad-image")
        self.assertEqual(len(world.writes), 1)

    def test_provider_clone_wait_has_count_deadline_and_phase_guards(self):
        for phases in (("ERROR",), ("CANCELED",), ("SUPERSEDED",), ("UNKNOWN",), ("DEPLOYING", "BUILDING")):
            world = ProviderCloneWorld(phases)
            with self.subTest(phases=phases), self.assertRaises(common.ReleaseError):
                world.deploy(drill="bad-image")
            self.assertLessEqual(world.clone_reads, 2)
            self.assertEqual(len(world.writes), 1)
        world = ProviderCloneWorld(("DEPLOYING",))
        with self.assertRaisesRegex(common.ReleaseError, "reconcile-timeout:stage-previous-clone"):
            world.deploy(drill="bad-image")
        self.assertEqual(world.clone_reads, 3)
        self.assertEqual(len(world.clients), 2)
        clock = [0]; sleeps = []
        def now(): clock[0] += 1; return clock[0]
        world = ProviderCloneWorld(("DEPLOYING",))
        lane = world.lane(poll_limit=90, poll_seconds=1, deadline_seconds=3,
                          monotonic=now, sleeper=sleeps.append)
        with self.assertRaisesRegex(common.ReleaseError, "reconcile-timeout:stage-previous-clone"):
            world.deploy(lane, drill="bad-image")
        self.assertEqual(world.clone_reads, 1)
        self.assertEqual(sleeps, [1])
        self.assertEqual(len(world.writes), 1)
        clock[0] = 0
        world = ProviderCloneWorld(("ACTIVE",))
        with self.assertRaisesRegex(common.ReleaseError, "reconcile-timeout:stage-previous-clone"):
            world.deploy(world.lane(monotonic=now, deadline_seconds=1), drill="bad-image")
        self.assertEqual(world.clone_reads, 1)  # Expiry after reads cannot grant rollback authority.
        self.assertEqual(len(world.writes), 1)
        self.assertEqual(world.probes, [])

    def test_provider_clone_ownership_and_final_cas_refuse_later_drift(self):
        for client_count in (3, 4):
            for field in ("lineage", "secret", "candidate", "migration"):
                world = ProviderCloneWorld(("ACTIVE",))
                def drift(value):
                    if len(world.clients) == client_count:
                        if field == "lineage": world.deployments[world.CLONE]["cloned_from"] = PG
                        elif field == "candidate": world.deployments[world.failed_id]["phase"] = "ACTIVE"
                        elif field == "migration": world.deployments[world.CLONE]["jobs"][0]["phase"] = "FAILED"
                        else: next(e for s in world.deployments[world.CLONE]["spec"]["services"] for e in s["envs"] if e["type"] == "SECRET")["value"] = "EV[foreign]"
                world.inventory_hook = drift
                with self.subTest(client_count=client_count, field=field), self.assertRaises(common.ReleaseError):
                    world.deploy(drill="bad-image")
                self.assertEqual(len(world.writes), 1)
                self.assertEqual(world.probes, [])
        world = ProviderCloneWorld(("ACTIVE",)); lane = world.lane(); reads = []
        def app_cas(app):
            if lane.checkpoint == "rollback-pre-put-cas":
                reads.append(1)
                if len(reads) == 2: app["updated_at"] = "2026-10-07T13:00:00Z"
        world.snapshot_hook = app_cas
        with self.assertRaisesRegex(common.ReleaseError, "rollback-precondition-failed:stage-cas"):
            world.deploy(lane, drill="bad-image")
        self.assertEqual(len(world.writes), 1)

    def test_local_identity_and_ambient_refuse_before_client_creation(self):
        world = World()
        for change in ({"target": {**world.target, "app_id": PG}}, {"env": {"SHIP_DO_TOKEN": ""}}):
            with self.assertRaises(common.ReleaseError): world.lane(**change)
        self.assertEqual(world.clients, [])

    def test_normal_stage_one_put_exact_deployment_six_health_bound_receipt(self):
        world = World(); lane = world.lane()
        outcome = world.deploy(lane)
        self.assertEqual(outcome.status, "deployed")
        self.assertEqual(len(world.writes), 1)
        self.assertEqual([client.puts for client in world.clients], [0, 1])
        self.assertEqual(len(world.probes), 6)
        self.assertEqual(outcome.receipt["previous_source_sha"], OLD_SOURCE)
        self.assertEqual(outcome.receipt["candidate_source_sha"], NEW_SOURCE)
        self.assertEqual(outcome.receipt["previous_images"], IMAGES)
        self.assertIn((IMAGES, None), world.verified)
        self.assertIn((NEW, NEW_SOURCE), world.verified)
        raw = contract.receipt_bytes(outcome.receipt)
        for private in (APP, PG, VK, VPC, ORIGIN, OLD_ID): self.assertNotIn(private.encode(), raw)
        lane.scrub(); self.assertTrue(all(client.scrubbed for client in world.clients))

    def test_omitted_service_ports_deploy_once_with_compatible_private_receipt(self):
        world = World()
        baseline = copy.deepcopy(world.spec)
        for component in baseline["services"] + baseline["jobs"]:
            for env in component["envs"]:
                if env["type"] == "SECRET": env["value"] = "EV[synthetic-encrypted-value]"
        world.spec = copy.deepcopy(baseline)
        for component in world.spec["services"]: del component["internal_ports"]
        world.deployments[OLD_ID] = world.deployment(OLD_ID, world.spec, "ACTIVE")
        outcome = world.deploy()
        self.assertEqual(outcome.status, "deployed")
        self.assertEqual(len(world.writes), 1)
        self.assertEqual([client.puts for client in world.clients], [0, 1])
        self.assertEqual(len(world.probes), 6)
        self.assertTrue(all("internal_ports" not in component for component in world.writes[0]["services"]))
        self.assertEqual(outcome.receipt["before_spec_sha256"], contract.spec_fingerprint(baseline))
        restored_defaults = copy.deepcopy(world.spec)
        for component in restored_defaults["services"]: component["internal_ports"] = []
        self.assertEqual(outcome.receipt["after_spec_sha256"], contract.spec_fingerprint(restored_defaults))
        self.assertEqual(common.loads_strict(contract.receipt_bytes(outcome.receipt)), outcome.receipt)
        self.assertTrue(all(client.scrubbed for client in world.clients))

    def test_plan_and_each_attestation_failure_make_zero_puts(self):
        for flag in ("fail_plan", "fail_candidate_attestation", "fail_previous_attestation"):
            world = World(); setattr(world, flag, True)
            with self.subTest(flag=flag), self.assertRaises(common.ReleaseError): world.deploy()
            self.assertEqual(world.writes, [])
        world = World()
        with self.assertRaises(common.ReleaseError): world.deploy(world.lane(verify_staging=lambda _: "f" * 40))
        self.assertEqual(world.writes, [])

    def test_wrong_candidate_hash_and_reused_client_fail_closed(self):
        world = World(); lane = world.lane()
        with self.assertRaises(common.ReleaseError): lane.deploy(candidate(), candidate_sha256="0" * 64, run_id="1234567")
        self.assertEqual(world.clients, [])
        shared = FakeClient(world, False)
        with self.assertRaises(common.ReleaseError): world.deploy(world.lane(client_factory=lambda **_: shared))
        self.assertEqual(world.writes, [])

    def test_inventory_and_live_active_mismatch_make_zero_puts(self):
        world = World()
        world.inventory_hook = lambda inventory: inventory["firewalls"][VK].append({"type": "ip_addr", "value": "0.0.0.0/0"})
        with self.assertRaises(common.ReleaseError): world.deploy()
        self.assertEqual(world.writes, [])

    def test_prior_migration_proof_and_pinned_deployment_fail_before_write(self):
        for mutation in ("migration", "pinned"):
            world = World()
            if mutation == "migration": world.deployments[OLD_ID]["jobs"][0]["phase"] = "FAILED"
            else: world.pinned = {"id": OLD_ID}
            with self.subTest(mutation=mutation), self.assertRaises(common.ReleaseError): world.deploy()
            self.assertEqual(world.writes, [])
            self.assertTrue(all(client.scrubbed for client in world.clients))
        world = World(); world.deployments[OLD_ID]["spec"]["services"][0]["instance_count"] = 2
        with self.assertRaises(common.ReleaseError): world.deploy()
        self.assertEqual(world.writes, [])

    def test_cas_drift_after_initial_stability_makes_zero_puts(self):
        world = World()
        def change(world):
            if world.get_count == 3: world.updated = "2026-10-07T11:01:00Z"
        world.app_hook = change
        with self.assertRaisesRegex(common.ReleaseError, "stage-before-put"): world.deploy()
        self.assertEqual(world.writes, [])

    def test_ambiguous_candidate_put_is_reconciled_never_retried(self):
        world = World(); world.put_outcome = "ambiguous"
        self.assertEqual(world.deploy().status, "deployed")
        self.assertEqual(len(world.writes), 1)
        self.assertEqual(sum(client.puts for client in world.clients), 1)

    def test_owned_timestamp_pairs_settle_without_additional_puts(self):
        phases = ("candidate-final-state", "candidate-after-health", "rollback-ownership",
                  "rollback-final-state", "rollback-after-health")
        for phase in phases:
            for drill in (("none", "bad-image") if phase == "rollback-ownership" else ("none",)):
                world = World(); original = copy.deepcopy(world.spec); lane = world.lane()
                world.health_failure = phase.startswith("rollback") and drill == "none"
                seen = []
                def timestamp(value):
                    if lane.checkpoint == phase:
                        seen.append(value.updated)
                        if len(seen) == 2: value.updated = "2026-10-07T12:01:00Z"
                world.app_hook = timestamp
                with self.subTest(phase=phase, drill=drill):
                    result = world.deploy(lane, drill=drill)
                    restored = phase.startswith("rollback")
                    self.assertEqual(result.status, "failed-rolled-back" if restored else "deployed")
                    self.assertGreaterEqual(len(seen), 4)  # A new equal pair, not a single good read.
                    self.assertEqual(len(world.writes), 2 if restored else 1)
                    self.assertEqual(sum(c.puts for c in world.clients), len(world.writes))
                    self.assertTrue(all(c.puts <= 1 and c.scrubbed for c in world.clients))
                    if restored: self.assertEqual(world.spec, original)
                    else: contract.validate_receipt(result.receipt)

    def test_cross_job_rollback_settles_owned_timestamp_pair(self):
        world = World(); original = copy.deepcopy(world.spec)
        receipt = world.deploy(drill="e2e-fail").receipt
        raw = contract.receipt_bytes(receipt); lane = world.lane(); calls = []
        def timestamp(value):
            if lane.checkpoint == "rollback-ownership":
                calls.append(1)
                if len(calls) == 2: value.updated = "2026-10-07T12:01:00Z"
        world.app_hook = timestamp
        result = lane.rollback(base64.b64encode(raw).decode(), common.sha256_bytes(raw),
            run_id=receipt["run_id"], candidate_sha256=receipt["candidate_sha256"])
        self.assertEqual(result.status, "rolled-back")
        self.assertEqual(world.spec, original)
        self.assertEqual(len(world.writes), 2)
        self.assertEqual(calls, [1] * 4)
        self.assertEqual(len(world.probes), 12)

    def test_settling_refuses_material_provider_or_validation_change_without_retry(self):
        modes = ("identity", "general", "secret", "images", "phase", "ingress", "pending", "pinned", "migration", "provider")
        foreign = "99999999-9999-4999-8999-999999999999"
        for mode in modes:
            world = World(); lane = world.lane(); client = lane.client(False)
            before = contract.spec_fingerprint(world.spec)
            original = client.get_app; calls = []
            def response():
                value = original(); calls.append(1)
                if len(calls) == 2:
                    if mode == "identity":
                        world.deployments[foreign] = world.deployment(foreign, world.spec, "ACTIVE")
                        value["app"]["active_deployment"]["id"] = foreign
                    elif mode in {"general", "secret"}:
                        env = next(e for c in value["app"]["spec"]["services"] for e in c["envs"]
                                   if e["type"] == ("SECRET" if mode == "secret" else "GENERAL"))
                        env["value"] = "EV[foreign-secret]" if mode == "secret" else "foreign-change"
                    elif mode == "images": value["app"]["spec"] = contract.set_images(world.spec, NEW)
                    elif mode == "phase": world.deployments[OLD_ID]["phase"] = "ERROR"
                    elif mode == "ingress": value["app"]["default_ingress"] = "https://foreign.example"
                    elif mode == "pending": value["app"]["pending_deployment"] = {"id": foreign}
                    elif mode == "pinned": value["app"]["pinned_deployment"] = {"id": foreign}
                    elif mode == "migration": world.deployments[OLD_ID]["jobs"][0]["phase"] = "FAILED"
                    else: common.fail("provider-get-failed")
                return value
            client.get_app = response
            with self.subTest(mode=mode), self.assertRaises(common.ReleaseError):
                lane.settled_owned(client, OLD_ID, before)
            self.assertEqual(len(calls), 2)
            self.assertEqual(world.writes, [])

    def test_timestamp_settling_has_count_and_production_wall_clock_caps(self):
        for bound in ("count", "clock"):
            world = World(); clock = [0]; sleeps = []
            lane = world.lane(monotonic=lambda: clock[0], sleeper=sleeps.append)
            client = lane.client(False); before = contract.spec_fingerprint(world.spec)
            def churn(value):
                value.updated = "metadata-" + str(value.get_count)
                if bound == "clock": clock[0] += 150
            world.app_hook = churn
            with self.subTest(bound=bound), self.assertRaisesRegex(common.ReleaseError, "stage-settle-timeout"):
                lane.settled_owned(client, OLD_ID, before)
            self.assertEqual(world.get_count, 6 if bound == "count" else 2)
            self.assertEqual(len(sleeps), 2 if bound == "count" else 0)
            self.assertEqual(world.writes, [])

    def test_timestamp_churn_never_creates_extra_candidate_or_rollback_attempts(self):
        for phase in ("candidate-final-state", "rollback-ownership", "rollback-final-state"):
            world = World(); lane = world.lane(); world.health_failure = phase.startswith("rollback")
            def churn(value):
                if lane.checkpoint == phase: value.updated = "metadata-" + str(value.get_count)
            world.app_hook = churn
            with self.subTest(phase=phase):
                if phase == "candidate-final-state":
                    result = world.deploy(lane)
                    self.assertEqual(result.status, "failed-rolled-back")
                    self.assertEqual(lane.original_failure, (phase, "post-deploy-guard:stage-settle-timeout"))
                else:
                    with self.assertRaisesRegex(common.ReleaseError, "stage-settle-timeout"): world.deploy(lane)
                self.assertEqual(len(world.writes), 1 if phase == "rollback-ownership" else 2)
                self.assertEqual(sum(c.puts for c in world.clients), len(world.writes))
                self.assertTrue(all(c.puts <= 1 and c.scrubbed for c in world.clients))

    def test_failed_settling_read_and_deadline_after_put_never_retry_candidate(self):
        for fault in ("provider", "deadline"):
            world = World(); clock = [0]; lane = world.lane(monotonic=lambda: clock[0])
            calls = []
            def fail(value):
                if lane.checkpoint == "candidate-final-state":
                    calls.append(1)
                    if fault == "provider": common.fail("provider-get-failed")
                    clock[0] += 150
            world.app_hook = fail
            with self.subTest(fault=fault):
                result = world.deploy(lane)
                self.assertEqual(result.status, "failed-rolled-back")
                self.assertEqual(len(calls), 1 if fault == "provider" else 2)
                self.assertEqual(len(world.writes), 2)  # One candidate and one verified restore.
                self.assertEqual(sum(c.puts for c in world.clients), 2)
                self.assertEqual(lane.original_failure, ("candidate-final-state",
                    "provider-get-failed" if fault == "provider" else "post-deploy-guard:stage-settle-timeout"))

    def test_each_public_operation_clears_previous_failure_provenance(self):
        world = World(); lane = world.lane(); world.health_failure = True
        self.assertEqual(world.deploy(lane).status, "failed-rolled-back")
        self.assertIsNotNone(lane.original_failure)
        world.fail_plan = True
        with self.assertRaises(common.ReleaseError): world.deploy(lane)
        self.assertIsNone(lane.original_failure)
        lane.original_failure = ("candidate-health", "smoke-failed:stage-health")
        with self.assertRaises(common.ReleaseError):
            lane.rollback("invalid", "0" * 64, run_id="1234567", candidate_sha256="0" * 64)
        self.assertIsNone(lane.original_failure)
        self.assertEqual(len(world.writes), 2)

    def test_rollback_ownership_never_settles_phase_or_active_identity_changes(self):
        for changed in ("phase", "active"):
            world = World(); lane = world.lane(); calls = []
            def drift(value):
                if lane.checkpoint == "rollback-ownership":
                    calls.append(1)
                    if len(calls) == 2:
                        failed = next(identity for identity in value.deployments if identity != OLD_ID)
                        if changed == "phase": value.deployments[failed]["phase"] = "ACTIVE"
                        else: value.active = failed
            world.app_hook = drift
            with self.subTest(changed=changed), self.assertRaisesRegex(common.ReleaseError, "stage-observation-changed"):
                world.deploy(lane, drill="bad-image")
            self.assertEqual(calls, [1, 1])
            self.assertEqual(len(world.writes), 1)

    def test_definitive_rejection_does_not_trigger_rollback(self):
        world = World(); world.put_outcome = "rejected"
        with self.assertRaisesRegex(common.ReleaseError, "provider-rejected"): world.deploy()
        self.assertEqual(world.writes, [])
        self.assertEqual(len(world.clients), 2)

    def test_pending_timeout_is_manual_no_blind_rollback(self):
        world = World(); world.put_outcome = "pending"
        with self.assertRaises(common.ReleaseError): world.deploy()
        self.assertEqual(len(world.writes), 1)
        self.assertEqual(sum(client.puts for client in world.clients), 1)

    def test_health_failure_rolls_back_prior_verified_staging_images(self):
        world = World(); original = copy.deepcopy(world.spec); world.health_failure = True
        outcome = world.deploy()
        self.assertEqual(outcome.status, "failed-rolled-back")
        self.assertIsNone(outcome.receipt)
        self.assertEqual(len(world.writes), 2)
        self.assertEqual(world.spec, original)
        self.assertTrue(all(client.puts <= 1 for client in world.clients))
        self.assertIn((IMAGES, OLD_SOURCE), world.verified)

    def test_failed_candidate_migration_rolls_back_with_old_or_new_active(self):
        for mode in ("terminal", "migration"):
            world = World(); original = copy.deepcopy(world.spec)
            if mode == "terminal": world.put_outcome = "terminal"
            else:
                def fail_migration(world):
                    if len(world.writes) == 1:
                        world.deployments[world.active]["jobs"][0]["phase"] = "FAILED"
                world.app_hook = fail_migration
            with self.subTest(mode=mode):
                self.assertEqual(world.deploy().status, "failed-rolled-back")
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(world.spec, original)

    def test_drift_after_health_failure_refuses_rollback_authority(self):
        world = World()
        def changed(url):
            world.spec["services"][0]["envs"][0]["value"] = "foreign-change"
            return 503, {}, b""
        with self.assertRaises(common.ReleaseError): world.deploy(world.lane(request=changed))
        self.assertEqual(len(world.writes), 1)

    def test_rollback_cas_change_or_failure_cannot_create_a_retry_loop(self):
        for mode in ("cas", "reject"):
            world = World(); world.health_failure = True; lane = world.lane()
            def before_rollback_inventory(value):
                # Reader and initial writer have already been allocated. The
                # fourth client is the fresh rollback writer.
                if len(world.clients) == 4 and mode == "reject": world.put_outcome = "rejected"
            world.inventory_hook = before_rollback_inventory
            def final_drift(value):
                if mode == "cas" and lane.checkpoint == "rollback-pre-put-cas": value.updated = "2026-10-07T13:00:00Z"
            world.app_hook = final_drift
            with self.subTest(mode=mode), self.assertRaises(common.ReleaseError): world.deploy(lane)
            self.assertEqual(len(world.writes), 1)
            self.assertTrue(all(client.puts <= 1 for client in world.clients))
            self.assertTrue(all(client.scrubbed for client in world.clients))

    def test_rollback_refreshes_timestamp_after_attestation_and_writer_inventory(self):
        for gap in ("attestation", "inventory"):
            for kind in ("health", "terminal", "clone"):
                world = ProviderCloneWorld(("ACTIVE",)) if kind == "clone" else World()
                prior = copy.deepcopy(world.spec); changed = []; fresh_reads = []
                def change():
                    changed.append(1)
                    world.updated = "2026-10-07T13:00:00Z"
                def verify(images, source):
                    result = world.verify_products(images, source)
                    if gap == "attestation" and lane.checkpoint == "rollback-prior-attestations": change()
                    return result
                lane = world.lane(verify_products=verify)
                def inventory(_):
                    if gap == "inventory" and len(world.clients) == 4: change()
                world.inventory_hook = inventory
                def read(value):
                    if kind == "clone" and changed: value.updated = "2026-10-07T13:00:00Z"
                    if lane.checkpoint == "rollback-pre-put-settle": fresh_reads.append(value.updated)
                world.app_hook = read
                with self.subTest(gap=gap, kind=kind):
                    outcome = world.deploy(lane, drill="health-fail" if kind == "health" else "bad-image")
                    self.assertEqual(outcome.status, "failed-rolled-back")
                    self.assertEqual(outcome.health_drill_completed, kind == "health")
                    self.assertEqual(changed, [1]); self.assertEqual(fresh_reads, ["2026-10-07T13:00:00Z"] * 2)
                    self.assertEqual(world.writes[1], prior)
                    self.assertEqual(len(world.writes), 2)
                    self.assertTrue(all(client.puts <= 1 and client.scrubbed for client in world.clients))

    def test_rollback_refresh_never_adopts_gap_material_drift(self):
        for gap in ("attestation", "inventory"):
            for drift in ("spec", "raw-spec", "active", "phase"):
                world = World(); fresh_reads = []
                def change():
                    if drift == "spec":
                        next(e for service in world.spec["services"] for e in service["envs"] if e["type"] == "SECRET")["value"] = "EV[foreign]"
                    elif drift == "raw-spec":
                        world.spec["services"][0].pop("internal_ports")
                    elif drift == "active": world.active = OLD_ID
                    else: world.deployments[world.active]["phase"] = "ERROR"
                def verify(images, source):
                    result = world.verify_products(images, source)
                    if gap == "attestation" and lane.checkpoint == "rollback-prior-attestations": change()
                    return result
                lane = world.lane(verify_products=verify)
                def inventory(_):
                    if gap == "inventory" and len(world.clients) == 4: change()
                world.inventory_hook = inventory
                def read(_):
                    if lane.checkpoint == "rollback-pre-put-settle": fresh_reads.append(1)
                world.app_hook = read
                with self.subTest(gap=gap, drift=drift), self.assertRaises(common.ReleaseError):
                    world.deploy(lane, drill="health-fail")
                self.assertEqual(len(fresh_reads), 1)
                self.assertEqual(len(world.writes), 1)
                self.assertTrue(all(client.puts <= 1 and client.scrubbed for client in world.clients))

    def test_rollback_strict_final_cas_rejects_changes_after_fresh_pair(self):
        for drift in ("spec", "raw-spec", "active", "updated"):
            world = World(); lane = world.lane(); fresh_reads = []
            def read(value):
                if lane.checkpoint == "rollback-pre-put-settle": fresh_reads.append(1)
                if lane.checkpoint == "rollback-pre-put-cas":
                    if drift == "spec": value.spec["services"][0]["envs"][0]["value"] = "foreign"
                    elif drift == "raw-spec": value.spec["services"][0].pop("internal_ports")
                    elif drift == "active": value.active = OLD_ID
                    else: value.updated = "2026-10-07T14:00:00Z"
            world.app_hook = read
            code = "spec" if drift == "raw-spec" else drift
            with self.subTest(drift=drift), self.assertRaisesRegex(common.ReleaseError, "stage-cas-" + code):
                world.deploy(lane, drill="health-fail")
            self.assertEqual(fresh_reads, [1, 1]); self.assertEqual(len(world.writes), 1)
            self.assertEqual(lane.checkpoint, "rollback-pre-put-cas")

    def test_rollback_fresh_pair_churn_and_read_failures_are_bounded(self):
        for failure in ("count", "clock", "provider"):
            world = World(); clock = [0]; reads = []; sleeps = []
            lane = world.lane(monotonic=lambda: clock[0], sleeper=sleeps.append)
            def read(value):
                if lane.checkpoint == "rollback-pre-put-settle":
                    reads.append(1)
                    if failure == "provider": common.fail("provider-get-failed")
                    value.updated = "metadata-" + str(len(reads))
                    if failure == "clock": clock[0] += 150
            world.app_hook = read
            code = "provider-get-failed" if failure == "provider" else "stage-settle-timeout"
            with self.subTest(failure=failure), self.assertRaisesRegex(common.ReleaseError, code):
                world.deploy(lane, drill="health-fail")
            self.assertEqual(len(reads), 6 if failure == "count" else 2 if failure == "clock" else 1)
            self.assertEqual(len(sleeps), 2 if failure == "count" else 0)
            self.assertEqual(len(world.writes), 1)

    def test_rollback_fresh_writer_inventory_refuses_before_refresh_or_put(self):
        world = World(); lane = world.lane(); reads = []
        def inventory(value):
            if len(world.clients) == 4: value["firewalls"][PG][0]["value"] = PG
        world.inventory_hook = inventory
        def read(_):
            if lane.checkpoint == "rollback-pre-put-settle": reads.append(1)
        world.app_hook = read
        with self.assertRaisesRegex(common.ReleaseError, "staging-firewall"):
            world.deploy(lane, drill="health-fail")
        self.assertEqual(reads, []); self.assertEqual(len(world.writes), 1)

    def test_foreign_deployment_during_rollback_health_is_not_reported_restored(self):
        world = World(); world.health_failure = True
        request = world.request
        foreign = "99999999-9999-4999-8999-999999999999"
        def drift(url):
            if len(world.writes) == 2 and world.active != foreign:
                world.spec["services"][0]["envs"][0]["value"] = "foreign-change"
                world.deployments[foreign] = world.deployment(foreign, world.spec, "ACTIVE")
                world.active = foreign
                world.updated = "2026-10-07T14:00:00Z"
            return request(url)
        with self.assertRaises(common.ReleaseError): world.deploy(world.lane(request=drift))
        self.assertEqual(world.active, foreign)
        self.assertEqual(len(world.writes), 2)
        self.assertTrue(all(client.puts <= 1 for client in world.clients))

    def test_health_and_bad_image_drills_restore_prior_staging(self):
        for drill in ("health-fail", "bad-image"):
            world = World(); original = copy.deepcopy(world.spec)
            with self.subTest(drill=drill):
                outcome = world.deploy(drill=drill)
                self.assertEqual(outcome.status, "failed-rolled-back")
                self.assertIs(outcome.health_drill_completed, drill == "health-fail")
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(world.spec, original)
                if drill == "health-fail": self.assertTrue(any("intentionally_missing" in url for url in world.probes))
                else: self.assertTrue(world.writes[0]["jobs"][0]["image"]["repository"].endswith("staging-bootstrap"))

    def test_health_drill_evidence_requires_completed_probe_and_verified_restoration(self):
        for status in (404, 200):
            world = World(); original = copy.deepcopy(world.spec)
            def request(url):
                result = world.request(url)
                return (status, {}, b"synthetic-private-probe-body") if "intentionally_missing" in url else result
            with self.subTest(status=status):
                outcome = world.deploy(world.lane(request=request), drill="health-fail")
                self.assertEqual(outcome.status, "failed-rolled-back")
                self.assertIs(outcome.health_drill_completed, True)
                self.assertIsNone(outcome.receipt)
                self.assertEqual(world.spec, original)
                self.assertEqual(len(world.writes), 2)
                self.assertNotEqual(world.active, OLD_ID)
                self.assertEqual(len(world.probes), 7)  # Deliberate probe + restored health6.
                self.assertEqual(world.probes[0], ORIGIN + "/_stage_drill_intentionally_missing.txt")
                self.assertTrue(all(client.scrubbed and client.puts <= 1 for client in world.clients))

    def test_pre_health_and_transport_errors_cannot_claim_intentional_health_evidence(self):
        for failure in ("terminal", "migration", "final-state", "transport", "same-code"):
            world = World(); original = copy.deepcopy(world.spec); lane = world.lane()
            if failure == "terminal": world.put_outcome = "terminal"
            elif failure == "migration":
                def fail_migration(value):
                    if len(value.writes) == 1: value.deployments[value.active]["jobs"][0]["phase"] = "FAILED"
                world.app_hook = fail_migration
            elif failure == "final-state":
                observe = lane.observe
                def wrong_candidate(client):
                    value = observe(client)
                    # Fail the post-PUT identity guard, leaving real provider
                    # state intact so the existing rollback can still verify it.
                    return stage.Snapshot(value.spec, value.images, OLD_ID, value.updated_sha256, value.spec_sha256) if lane.checkpoint == "candidate-final-state" else value
                lane.observe = wrong_candidate
            else:
                def failed_probe(url):
                    if "intentionally_missing" in url:
                        world.probes.append(url)
                        common.fail("smoke-failed:stage-drill" if failure == "same-code" else "smoke-failed:transport")
                    return world.request(url)
                lane.request = failed_probe
            with self.subTest(failure=failure):
                outcome = world.deploy(lane, drill="health-fail")
                self.assertEqual(outcome.status, "failed-rolled-back")
                self.assertIs(outcome.health_drill_completed, False)
                self.assertEqual(world.spec, original)
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(any("intentionally_missing" in url for url in world.probes), failure in {"transport", "same-code"})

    def test_intentional_health_failure_cannot_claim_evidence_when_restoration_fails(self):
        for failure in ("cas", "reject", "health", "after-health"):
            world = World()
            if failure in {"cas", "reject"}:
                def before_rollback(_):
                    if len(world.clients) == 4 and failure == "reject": world.put_outcome = "rejected"
                world.inventory_hook = before_rollback
                def final_drift(value):
                    if failure == "cas" and lane.checkpoint == "rollback-pre-put-cas": value.updated = "2026-10-07T13:00:00Z"
                world.app_hook = final_drift
            def request(url):
                result = world.request(url)
                if len(world.writes) == 2:
                    if failure == "health": return 503, {}, b"synthetic-private-body"
                    if failure == "after-health": world.spec["services"][0]["envs"][0]["value"] = "foreign-change"
                return result
            lane = world.lane(request=request)
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                world.deploy(lane, drill="health-fail")
            self.assertEqual(world.probes[0], ORIGIN + "/_stage_drill_intentionally_missing.txt")
            self.assertEqual(len(world.writes), 1 if failure in {"cas", "reject"} else 2)
            self.assertTrue(all(client.scrubbed and client.puts <= 1 for client in world.clients))

    def test_cross_job_e2e_failure_uses_bound_receipt_and_fresh_capability(self):
        world = World(); original = copy.deepcopy(world.spec)
        outcome = world.deploy(drill="e2e-fail")
        self.assertEqual(outcome.receipt["drill"], "e2e-fail")
        raw = contract.receipt_bytes(outcome.receipt)
        result = world.lane().rollback(base64.b64encode(raw).decode(), common.sha256_bytes(raw),
            run_id=outcome.receipt["run_id"], candidate_sha256=outcome.receipt["candidate_sha256"])
        self.assertEqual(result.status, "rolled-back")
        self.assertEqual(world.spec, original)
        self.assertEqual(len(world.writes), 2)
        self.assertTrue(all(client.puts <= 1 for client in world.clients))

    def test_foreign_receipt_or_changed_candidate_never_rolls_back(self):
        for change in ("run", "spec", "active"):
            world = World(); outcome = world.deploy(); raw = contract.receipt_bytes(outcome.receipt)
            if change == "spec": world.spec["services"][0]["envs"][0]["value"] = "changed"
            if change == "active":
                identity = "99999999-9999-4999-8999-999999999999"
                world.deployments[identity] = world.deployment(identity, world.spec, "ACTIVE"); world.active = identity
            with self.subTest(change=change), self.assertRaises(common.ReleaseError):
                world.lane().rollback(base64.b64encode(raw).decode(), common.sha256_bytes(raw),
                    run_id="9" if change == "run" else outcome.receipt["run_id"], candidate_sha256=outcome.receipt["candidate_sha256"])
            self.assertEqual(len(world.writes), 1)

    def test_cross_job_rollback_reverifies_previous_images(self):
        world = World(); outcome = world.deploy(); raw = contract.receipt_bytes(outcome.receipt)
        world.fail_previous_attestation = True
        with self.assertRaises(common.ReleaseError):
            world.lane().rollback(base64.b64encode(raw).decode(), common.sha256_bytes(raw), run_id=outcome.receipt["run_id"], candidate_sha256=outcome.receipt["candidate_sha256"])
        self.assertEqual(len(world.writes), 1)


class Response:
    def __init__(self, url, value): self.url, self.raw, self.status, self.headers = url, common.canonical_payload_bytes(value), 200, {"Content-Type": "application/json"}
    def geturl(self): return self.url
    def read(self, maximum): return self.raw[:maximum]
    def __enter__(self): return self
    def __exit__(self, *_): return False


class ProviderClientTests(unittest.TestCase):
    def test_inherits_one_put_and_rejects_other_resource_paths(self):
        _, _, pins, target = data()
        class Opener:
            def __init__(self): self.calls = []
            def open(self, request, timeout):
                self.calls.append(request.get_method()); return Response(request.full_url, {"app": {"id": APP}})
        opener = Opener()
        client = stage.StageClient(target, "synthetic-staging-token-long-enough", expected_app_id_sha256=pins["app_id_sha256"], allow_put=True, opener=opener)
        for path in (f"/v2/apps/{PG}", f"/v2/databases/{PG}", "/v2/apps?page=11&per_page=200",
                     "/v2/databases?page=1&per_page=200", "/v2/databases?tag_name=partial",
                     "https://foreign.invalid", "/v2/account?token=x"):
            with self.subTest(path=path), self.assertRaises(common.ReleaseError): client._get(path, "test")
        self.assertEqual(opener.calls, [])
        client.put_app_once({})
        with self.assertRaisesRegex(common.ReleaseError, "second-mutation-blocked"): client.put_app_once({})
        self.assertEqual(opener.calls, ["PUT"])

    def test_pagination_is_complete_constructed_bounded_and_stable(self):
        _, _, pins, target = data()
        client = stage.StageClient(target, "synthetic-staging-token-long-enough", expected_app_id_sha256=pins["app_id_sha256"], allow_put=False)
        calls = []
        pages = [{"apps": [{}] * 200, "meta": {"total": 201}, "links": {"pages": {"next": common.API_ORIGIN + "/v2/apps?page=2&per_page=200"}}},
                 {"apps": [{}], "meta": {"total": 201}, "links": {}}]
        def get(path, *_, **__): calls.append(path); return pages[len(calls) - 1]
        client._get = get
        self.assertEqual(len(client._inventory_pages("apps")), 201)
        self.assertEqual(calls[-1], "/v2/apps?page=2&per_page=200")
        for broken in ({"apps": []}, {"apps": None, "meta": {"total": 0}},
                       {"apps": [], "meta": {"total": True}}, {"apps": [], "meta": {"total": -1}},
                       {"apps": [], "meta": {"total": 1}}, {"apps": [], "meta": {"total": 0}, "links": None},
                       {"apps": [{}], "meta": {"total": 2}, "links": {"pages": {"next": "https://foreign.invalid"}}}):
            client._get = lambda *args, **kwargs: broken
            with self.assertRaises(common.ReleaseError): client._inventory_pages("apps")
        calls.clear()
        pages[1]["meta"]["total"] = 200
        client._get = get
        with self.assertRaisesRegex(common.ReleaseError, "provider-invalid:stage-pagination-drift"):
            client._inventory_pages("apps")
        self.assertEqual(len(calls), 2)

    def inventory_client(self, inventory, database_response):
        _, _, pins, target = data()
        calls = []
        responses = {
            "/v2/account": {"account": inventory["account"]},
            "/v2/apps?page=1&per_page=200": {"apps": inventory["apps"], "meta": {"total": len(inventory["apps"])}},
            "/v2/databases": database_response,
            **{f"/v2/databases/{identity}/firewall": {"rules": rules} for identity, rules in inventory["firewalls"].items()},
        }
        class Opener:
            def open(self, request, timeout):
                assert request.get_method() == "GET" and request.data is None
                assert request.full_url.startswith(common.API_ORIGIN)
                path = request.full_url.removeprefix(common.API_ORIGIN)
                calls.append(path)
                return Response(request.full_url, responses[path])
        return stage.StageClient(target, "synthetic-staging-token-long-enough",
            expected_app_id_sha256=pins["app_id_sha256"], allow_put=False, opener=Opener()), calls

    def test_documented_unpaginated_database_envelope_completes_exact_inventory(self):
        world = World(); inventory = world.inventory()
        client, calls = self.inventory_client(inventory, {"databases": inventory["clusters"]})
        observed = client.inventory()
        self.assertEqual(observed, inventory)
        contract.validate_inventory(**observed, target=world.target, pins=world.pins, production=world.production)
        self.assertEqual(calls, ["/v2/account", "/v2/apps?page=1&per_page=200", "/v2/databases",
            f"/v2/databases/{PG}/firewall", f"/v2/databases/{VK}/firewall"])
        self.assertFalse(client.mutation_attempted)
        self.assertEqual(client.put_count(), 0)

    def test_database_envelope_refuses_malformed_or_pagination_shapes_without_followup(self):
        inventory = World().inventory()
        for response in (None, [], {}, {"databases": None}, {"databases": {}},
                         {"databases": inventory["clusters"], "meta": {"total": 2}},
                         {"databases": inventory["clusters"], "links": {}},
                         {"databases": inventory["clusters"], "links": {"pages": {"next": "https://foreign.invalid"}}}):
            with self.subTest(response=response):
                client, calls = self.inventory_client(inventory, response)
                with self.assertRaisesRegex(common.ReleaseError, "provider-invalid:stage-databases"):
                    client.inventory()
                self.assertEqual(calls, ["/v2/account", "/v2/apps?page=1&per_page=200", "/v2/databases"])
                self.assertFalse(client.mutation_attempted)

    def test_unpaginated_transport_retains_exact_cluster_and_firewall_validation(self):
        for mutate in (lambda v: v["clusters"].clear(), lambda v: v["clusters"].append(v["clusters"][0]),
                       lambda v: v["clusters"].__setitem__(1, copy.deepcopy(v["clusters"][0])),
                       lambda v: v["clusters"][1].update(id=APP), lambda v: v["clusters"][0].update(version="18"),
                       lambda v: v["clusters"][1].update(private_network_uuid=PG),
                       lambda v: v["clusters"][1].update(engine="redis"),
                       lambda v: v["account"]["team"].update(uuid="foreign"),
                       lambda v: v["firewalls"][VK].append({"type": "ip_addr", "value": "192.0.2.1"})):
            with self.subTest(mutate=mutate):
                world = World(); inventory = world.inventory(); mutate(inventory)
                client, calls = self.inventory_client(inventory, {"databases": inventory["clusters"]})
                observed = client.inventory()
                with self.assertRaises(common.ReleaseError):
                    contract.validate_inventory(**observed, target=world.target, pins=world.pins, production=world.production)
                self.assertEqual(len(calls), 5)
                self.assertFalse(client.mutation_attempted)
                self.assertEqual(client.put_count(), 0)


class CLITests(unittest.TestCase):
    def test_provider_clone_cli_restores_without_receipt_or_health_drill_claim(self):
        world = ProviderCloneWorld()
        result, logs = self.world_cli(world, "bad-image")
        self.assertEqual(result, stage.ship.EXIT_ROLLED_BACK)
        self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back",
            "stage-original-failure: checkpoint=candidate-put-reconcile; code=deployment-error:stage-terminal"])
        self.assertEqual(world.spec, world.prior)
        self.assertEqual(len(world.writes), 2)
        self.assertEqual(len(world.probes), 6)

    def environment(self):
        value = candidate()
        return {"STAGING_DO_TOKEN": "synthetic-provider-token-never-log", "STAGING_TARGET_JSON": json.dumps(data()[3]),
            "SHIP_MODE": "stage", "SHIP_DRILL": "none", "GITHUB_REPOSITORY": common.REPOSITORY,
            "GITHUB_REF": "refs/heads/main", "GITHUB_REF_PROTECTED": "true", "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_WORKFLOW_REF": common.SHIP_WORKFLOW_REF, "GITHUB_WORKFLOW_SHA": NEW_SOURCE, "GITHUB_SHA": NEW_SOURCE,
            "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "1234567",
            "CANDIDATE_B64": base64.b64encode(common.canonical_file_bytes(value)).decode(), "CANDIDATE_SHA256": common.sha256_bytes(common.canonical_file_bytes(value))}

    def world_cli(self, world, drill, *, request=None, stdout=None, command="deploy", receipt=None):
        logs = stdout if stdout is not None else io.StringIO()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file, template_file = (root / name for name in ("production.json", "pins.json", "template.json"))
            for path, value in ((production_file, world.production), (pins_file, world.pins), (template_file, world.template)):
                path.write_bytes(common.canonical_file_bytes(value))
            env = self.environment(); env["SHIP_DRILL"] = drill
            if receipt is not None:
                raw = contract.receipt_bytes(receipt)
                env.update(STAGE_RECEIPT_B64=base64.b64encode(raw).decode(), STAGE_RECEIPT_SHA256=common.sha256_bytes(raw))
            deps = stage.ship.Deps(stdout=logs, target_path=production_file,
                clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc),
                https_request=request or world.request, sleeper=lambda _: None, poll_limit=3)
            adapters = lambda *_: SimpleNamespace(verify_products=world.verify_products,
                verify_staging=lambda binding: binding["source_sha"], verify_plan=lambda *_: True)
            with mock.patch.object(stage, "StageClient", side_effect=lambda *_, allow_put, **__: world.factory(allow_put=allow_put)):
                result = stage.main([command], env, deps, adapter_factory=adapters,
                    pins_path=pins_file, template_path=template_file)
        ordinary = "\n".join(line for line in logs.getvalue().splitlines() if not line.startswith("::add-mask::"))
        for private in ("synthetic-provider-token-never-log", "synthetic-private-body", APP, PG, VK, VPC, ORIGIN):
            self.assertNotIn(private, ordinary)
        return result, ordinary

    def test_failure_checkpoints_keep_candidate_and_rollback_write_counts(self):
        cases = (
            ("rejected", "candidate-put-reconcile", "provider-rejected", 0, 1),
            ("get", "candidate-put-reconcile", "provider-get-failed", 1, 1),
            ("guard", "candidate-put-reconcile", "reconcile-failed:stage-spec-drift", 1, 1),
            ("rollback-guard", "rollback-ownership", "rollback-precondition-failed:stage-drift", 1, 1),
            ("rollback-health", "rollback-health", "smoke-failed:stage-health", 2, 2),
        )
        for case, checkpoint, code, writes, attempts in cases:
            world = World()
            if case == "rejected": world.put_outcome = "rejected"
            if case in {"get", "guard"}:
                def changed(value):
                    if value.writes:
                        if case == "get": common.fail("provider-get-failed")
                        value.spec["services"][0]["health_check"]["initial_delay_seconds"] = 999
                world.app_hook = changed
            def request(url):
                if case == "rollback-guard": world.spec["services"][0]["envs"][0]["value"] = "foreign-change"
                return 503, {}, b"synthetic-private-body"
            with self.subTest(case=case):
                result, logs = self.world_cli(world, "none", request=request)
                self.assertEqual(result, stage.ship.EXIT_MANUAL)
                expected = [
                    "stage: failed after an attempted mutation; reconcile with reads before any retry",
                    f"stage-diagnostic: checkpoint={checkpoint}; code={code}"]
                if case.startswith("rollback"):
                    expected.append("stage-original-failure: checkpoint=candidate-health; code=smoke-failed:stage-health")
                self.assertEqual(logs.splitlines(), expected)
                self.assertEqual(len(world.writes), writes)
                self.assertEqual(sum(client.puts for client in world.clients), attempts)
                self.assertTrue(all(client.puts <= 1 and client.scrubbed for client in world.clients))

    def test_unknown_exception_details_and_untrusted_checkpoint_never_print(self):
        secret = "privateSentinelUnmasked0123456789"
        class PrivateException(Exception):
            def __str__(self): raise AssertionError("must not stringify exceptions")
        cases = (
            (common.ReleaseError("provider-invalid:" + secret), "provider-invalid"),
            (RuntimeError(secret + ORIGIN + APP), "internal-error"),
            (PrivateException(), "internal-error"),
            (KeyboardInterrupt(secret), "internal-error:interrupted"),
        )
        for error, expected in cases:
            world = World()
            def refuse(value):
                if value.writes: raise error
            world.app_hook = refuse
            with self.subTest(code=expected):
                result, logs = self.world_cli(world, "none")
                self.assertEqual(result, stage.ship.EXIT_MANUAL)
                self.assertIn("checkpoint=candidate-put-reconcile; code=" + expected, logs)
                self.assertNotIn(secret, logs)
                self.assertEqual(len(world.writes), 1)
                self.assertEqual(sum(client.puts for client in world.clients), 1)
        output = io.StringIO()
        stage.report_failure(common.Output(stdout=output), RuntimeError(secret), secret, False)
        self.assertEqual(output.getvalue().splitlines()[-1], "stage-diagnostic: checkpoint=unknown; code=internal-error")
        self.assertNotIn(secret, output.getvalue())

    def test_original_failure_and_rollback_failure_are_both_safe_and_best_effort(self):
        secret = "unmaskedOriginalFailureSecret0123456789"
        for broken in (False, True):
            world = World()
            def request(_): common.fail("smoke-failed:" + secret)
            def drift(value):
                if len(world.clients) == 3: value["account"]["status"] = "suspended"
            world.inventory_hook = drift
            original = common.Output.text
            def output(out, line):
                if broken and line.startswith("stage-original-failure:"): raise OSError(secret)
                return original(out, line)
            with self.subTest(broken_output=broken), mock.patch.object(common.Output, "text", output):
                result, logs = self.world_cli(world, "none", request=request)
            self.assertEqual(result, stage.ship.EXIT_MANUAL)
            self.assertIn("stage-diagnostic: checkpoint=rollback-ownership", logs)
            self.assertEqual("stage-original-failure: checkpoint=candidate-health; code=smoke-failed" in logs, not broken)
            self.assertNotIn(secret, logs)
            self.assertEqual(len(world.writes), 1)
            self.assertTrue(all(c.puts <= 1 and c.scrubbed for c in world.clients))
        output = io.StringIO()
        stage.report_failure(common.Output(stdout=output), RuntimeError(secret), "rollback-ownership", True,
            original_failure=(secret, secret))
        self.assertIn("stage-original-failure: checkpoint=unknown; code=internal-error", output.getvalue())
        self.assertNotIn(secret, output.getvalue())

    def test_receipt_output_failures_are_after_deploy_and_do_not_retry_or_rollback(self):
        for method, checkpoint, error, code in (
            ("set_outputs", "receipt-outputs", OSError("synthetic-private-body"), "internal-error"),
            ("emit", "receipt-emit", common.ReleaseError("output-unsafe:value"), "output-unsafe:value"),
        ):
            world = World()
            with self.subTest(method=method), mock.patch.object(common.Output, method, side_effect=error) as failed:
                result, logs = self.world_cli(world, "none")
            failed.assert_called_once()
            self.assertEqual(result, stage.ship.EXIT_MANUAL)
            self.assertIn(f"checkpoint={checkpoint}; code={code}", logs)
            self.assertEqual(len(world.writes), 1)
            self.assertEqual(sum(client.puts for client in world.clients), 1)
            self.assertEqual(len(world.probes), 6)
            self.assertEqual(contract.image_set(world.spec), NEW)

    def test_broken_output_preserves_refusal_or_manual_without_raw_fallback(self):
        for after_write in (False, True):
            world = World()
            class Broken(io.StringIO):
                def write(self, value):
                    if not after_write or world.writes: raise OSError("synthetic-private-body")
                    return super().write(value)
            with self.subTest(after_write=after_write):
                result, logs = self.world_cli(world, "none", stdout=Broken())
                self.assertEqual(result, stage.ship.EXIT_MANUAL if after_write else stage.ship.EXIT_REFUSED)
                self.assertEqual(len(world.writes), int(after_write))
                self.assertEqual(sum(client.puts for client in world.clients), int(after_write))
                self.assertEqual(logs, "")
        world = World(); world.put_outcome = "rejected"
        original = common.Output.text
        def guarded(out, line):
            if line.startswith("stage-diagnostic:"): common.fail("output-unsafe:text")
            return original(out, line)
        with mock.patch.object(common.Output, "text", guarded):
            result, logs = self.world_cli(world, "none")
        self.assertEqual(result, stage.ship.EXIT_MANUAL)
        self.assertEqual(logs, "stage: failed after an attempted mutation; reconcile with reads before any retry")
        self.assertEqual(sum(client.puts for client in world.clients), 1)

    def test_success_and_completed_in_job_rollback_preserve_status_and_safe_cause(self):
        for failure in (False, True):
            world = World(); world.health_failure = failure
            with self.subTest(health_failure=failure):
                result, logs = self.world_cli(world, "none")
                self.assertEqual(result, stage.ship.EXIT_ROLLED_BACK if failure else stage.ship.EXIT_OK)
                self.assertNotIn("stage-diagnostic:", logs)
                self.assertEqual(len(world.writes), 2 if failure else 1)
                if failure: self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back",
                    "stage-original-failure: checkpoint=candidate-health; code=smoke-failed:stage-health"])
                else:
                    lines = logs.splitlines()
                    contract.validate_receipt(json.loads(lines[0]))
                    self.assertEqual(lines[1:], ["stage: deployed"])

    def test_cross_job_rollback_failure_reports_owned_phase_without_extra_put(self):
        world = World(); receipt = world.deploy().receipt
        result, logs = self.world_cli(world, "none", command="rollback", receipt=receipt,
            request=lambda _: (503, {}, b"synthetic-private-body"))
        self.assertEqual(result, stage.ship.EXIT_MANUAL)
        self.assertIn("checkpoint=rollback-health; code=smoke-failed:stage-health", logs)
        self.assertEqual(len(world.writes), 2)  # One earlier deploy plus exactly one cross-job restore.
        self.assertEqual(sum(client.puts for client in world.clients), 2)
        self.assertEqual(contract.image_set(world.spec), IMAGES)

    def test_health_drill_cli_marker_requires_exact_trigger_and_completed_restoration(self):
        for case in ("404", "200", "transport", "transport-exception", "same-code", "rollback-health", "none", "e2e-fail", "bad-image"):
            world = World()
            def request(url):
                if "intentionally_missing" in url:
                    if case == "transport-exception": raise TimeoutError("synthetic-private-body")
                    if case in {"transport", "same-code"}:
                        common.fail("smoke-failed:stage-drill" if case == "same-code" else "synthetic-private-body")
                    world.probes.append(url)
                    return (200 if case == "200" else 404), {}, b"synthetic-private-body"
                if case == "rollback-health" and len(world.writes) == 2:
                    return 503, {}, b"synthetic-private-body"
                return world.request(url)
            drill = case if case in {"none", "e2e-fail", "bad-image"} else "health-fail"
            with self.subTest(case=case):
                result, logs = self.world_cli(world, drill, request=request)
                self.assertEqual(logs.count(stage.HEALTH_DRILL_EVIDENCE), 1 if case in {"404", "200"} else 0)
                expected = stage.ship.EXIT_OK if case in {"none", "e2e-fail"} else stage.ship.EXIT_MANUAL if case in {"rollback-health", "transport-exception"} else stage.ship.EXIT_ROLLED_BACK
                self.assertEqual(result, expected)
                if case in {"404", "200"}:
                    self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back",
                        "stage-original-failure: checkpoint=candidate-health; code=smoke-failed:stage-drill",
                        stage.HEALTH_DRILL_EVIDENCE])
                    self.assertEqual(len(world.writes), 2)

    def test_health_drill_real_transport_avoids_spa_body_and_retains_size_guard(self):
        old_path = "/_stage_drill_intentionally_missing"
        static_path = old_path + ".txt"
        sockets = []

        def transport(url, *, oversized_static=False, static_status=404):
            path = url.removeprefix(ORIGIN)
            if path == old_path or (path == static_path and oversized_static):
                status, body = 200, b"x" * 5000  # Extensionless frontend index exceeds the unchanged 4096 cap.
            elif path == static_path:
                status, body = static_status, b"404 page not found\n"
            else:
                status, body = {path: status for _, path, status in stage.smoke.HEALTH}[path], b""
            response = f"HTTP/1.1 {status} Synthetic\r\nContent-Length: {len(body)}\r\n\r\n".encode() + body
            sock = FakeSocket(response); sockets.append(sock)
            return stage.smoke.secure_https_request(url, getaddrinfo=addresses("93.184.216.34"),
                create_connection=lambda *_, **__: sock, context_factory=lambda: FakeContext(sock))

        self.assertEqual(stage.smoke.MAX_HEALTH_BODY_BYTES, 4096)
        with self.assertRaisesRegex(common.ReleaseError, "^smoke-failed:length$"):
            transport(ORIGIN + old_path)
        self.assertTrue(sockets[-1].sent.startswith(f"GET {old_path} HTTP/1.1\r\n".encode()))
        for status, oversized in ((404, False), (200, False), (200, True)):
            world = World(); sockets.clear()
            before = contract.spec_fingerprint(world.spec)
            def request(url):
                world.probes.append(url)
                return transport(url, oversized_static=oversized, static_status=status)
            with self.subTest(status=status, oversized=oversized):
                result, logs = self.world_cli(world, "health-fail", request=request)
                self.assertEqual(result, stage.ship.EXIT_ROLLED_BACK)
                cause = "smoke-failed:length" if oversized else "smoke-failed:stage-drill"
                self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back",
                    f"stage-original-failure: checkpoint=candidate-health; code={cause}"] +
                    ([] if oversized else [stage.HEALTH_DRILL_EVIDENCE]))
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(sum(client.puts for client in world.clients), 2)
                self.assertTrue(all(client.puts <= 1 and client.scrubbed for client in world.clients))
                self.assertEqual(contract.spec_fingerprint(world.spec), before)
                self.assertEqual(world.probes, [ORIGIN + static_path] + [ORIGIN + path for _, path, _ in stage.smoke.HEALTH])
                self.assertEqual(len(sockets), 7)
                self.assertTrue(sockets[0].sent.startswith(f"GET {static_path} HTTP/1.1\r\n".encode()))
                self.assertTrue(all(sock.closed and b"Authorization:" not in sock.sent for sock in sockets))

    def test_completed_rollback_original_cause_is_private_and_output_is_best_effort(self):
        secret = "unmaskedSuccessfulRollbackSecret0123456789"
        for drill in ("none", "health-fail"):
            for error in (None, OSError(secret), common.ReleaseError("output-unsafe:" + secret), KeyboardInterrupt(secret)):
                world = World()
                def request(url):
                    if drill == "none" and len(world.writes) == 1:
                        common.fail("smoke-failed:" + secret + ORIGIN + APP)
                    return world.request(url)
                original = common.Output.text
                def output(out, line):
                    if error is not None and line.startswith("stage-original-failure:"): raise error
                    return original(out, line)
                with self.subTest(drill=drill, output_error=type(error).__name__), mock.patch.object(common.Output, "text", output):
                    result, logs = self.world_cli(world, drill, request=request)
                self.assertEqual(result, stage.ship.EXIT_ROLLED_BACK)
                cause = "smoke-failed:stage-drill" if drill == "health-fail" else "smoke-failed"
                self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back"] +
                    ([f"stage-original-failure: checkpoint=candidate-health; code={cause}"] if error is None else []) +
                    ([stage.HEALTH_DRILL_EVIDENCE] if drill == "health-fail" else []))
                self.assertNotIn(secret, logs)
                self.assertEqual(len(world.writes), 2)
                self.assertEqual(sum(client.puts for client in world.clients), 2)
                self.assertTrue(all(client.puts <= 1 and client.scrubbed for client in world.clients))
                self.assertEqual(contract.image_set(world.spec), IMAGES)

    def template_cli(self, command, *, template_bytes=None, target_changes=None, formatted_identity=None):
        production, template, pins, target = data()
        target.update(target_changes or {})
        logs = io.StringIO()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file = root / "production.json", root / "pins.json"
            for name, path, value in (("production", production_file, production), ("pins", pins_file, pins)):
                raw = (json.dumps(value, indent=2) + "\n").encode() if formatted_identity == name else common.canonical_file_bytes(value)
                path.write_bytes(raw)
            template_file = None
            if template_bytes is not None:
                template_file = root / "template.json"
                template_file.write_bytes(template_bytes)
            env = self.environment(); env["STAGING_TARGET_JSON"] = json.dumps(target)
            receipt = synthetic_receipt(candidate_sha256=env["CANDIDATE_SHA256"])
            raw = contract.receipt_bytes(receipt)
            env.update(STAGE_RECEIPT_B64=base64.b64encode(raw).decode(), STAGE_RECEIPT_SHA256=common.sha256_bytes(raw))
            adapters = mock.Mock(return_value=SimpleNamespace(
                verify_products=lambda *args: OLD_SOURCE, verify_staging=lambda *args: OLD_SOURCE,
                verify_plan=lambda *args: True))
            fake_lane = SimpleNamespace(clients=[], scrub=lambda: None,
                deploy=mock.Mock(return_value=stage.Outcome("deployed", receipt)),
                rollback=mock.Mock(return_value=stage.Outcome("rolled-back")))
            deps = stage.ship.Deps(repo_dir=contract.ROOT, stdout=logs, target_path=production_file,
                clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc))
            with mock.patch.object(stage, "StageLane", return_value=fake_lane) as lane, mock.patch.object(stage, "StageClient") as client:
                result = stage.main([command], env, deps, adapter_factory=adapters,
                    pins_path=pins_file, template_path=template_file)
                client.assert_not_called()
            return result, adapters, lane, fake_lane, logs.getvalue()

    def test_cli_loads_committed_formatted_template_for_deploy_and_rollback(self):
        # Use the actual default repository path, plus its exact LF checkout
        # representation, rather than reserializing it into a canonical fixture.
        raw = contract.TEMPLATE_PATH.read_bytes().replace(b"\r\n", b"\n")
        template = common.loads_strict(raw)
        self.assertNotEqual(raw, common.canonical_file_bytes(template))
        for command in ("deploy", "rollback"):
            for content in (None, raw):
                with self.subTest(command=command, repository_path=content is None):
                    result, adapters, lane, fake_lane, _ = self.template_cli(command, template_bytes=content)
                    self.assertEqual(result, stage.ship.EXIT_OK)
                    adapters.assert_called_once()
                    lane.assert_called_once()
                    self.assertEqual(lane.call_args.kwargs["template"], template)
                    getattr(fake_lane, command).assert_called_once()

    def test_cli_rejects_invalid_or_unbound_formatted_template_before_effects(self):
        _, template, _, _ = data()
        changed = copy.deepcopy(template); changed["region"] = "foreign-region"
        duplicate = '{"region":' + json.dumps(template["region"]) + ',' + json.dumps(template)[1:]
        cases = (
            # Both duplicate values agree, so permissive JSON parsing would
            # preserve the expected hash and wrongly reach the adapter.
            (duplicate.encode(), None),
            (b'{"number":1.5}', None),
            (b'{"number":NaN}', None),
            (b'{"broken":', None),
            ((json.dumps(changed, indent=2) + "\n").encode(), None),
            (None, {"template_sha256": "0" * 64}),
        )
        for command in ("deploy", "rollback"):
            for index, (content, target_changes) in enumerate(cases):
                with self.subTest(command=command, case=index):
                    result, adapters, lane, _, logs = self.template_cli(command,
                        template_bytes=content, target_changes=target_changes)
                    self.assertEqual(result, stage.ship.EXIT_REFUSED)
                    adapters.assert_not_called(); lane.assert_not_called()
                    self.assertIn("stage: refused before mutation", logs)

    def test_cli_keeps_identity_files_canonical(self):
        for name in ("production", "pins"):
            with self.subTest(name=name):
                result, adapters, lane, _, _ = self.template_cli("deploy", formatted_identity=name)
                self.assertEqual(result, stage.ship.EXIT_REFUSED)
                adapters.assert_not_called(); lane.assert_not_called()

    def test_cli_pops_provider_secrets_before_adapter_and_masks_private_values(self):
        production, template, pins, target = data()
        logs = io.StringIO()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file, template_file = (root / name for name in ("production.json", "pins.json", "template.json"))
            for path, value in ((production_file, production), (pins_file, pins), (template_file, template)):
                path.write_bytes(common.canonical_file_bytes(value))
            output = root / "outputs.txt"
            env = self.environment(); env["GITHUB_OUTPUT"] = str(output)
            def adapters(ctx, control):
                self.assertFalse(any(key.startswith("STAGING_") for key in ctx.env))
                self.assertEqual(control["run_id"], "1234567")
                return SimpleNamespace(verify_products=lambda *args: OLD_SOURCE, verify_staging=lambda *args: OLD_SOURCE, verify_plan=lambda *args: True)
            receipt = synthetic_receipt(candidate_sha256=env["CANDIDATE_SHA256"])
            fake_lane = SimpleNamespace(clients=[], deploy=lambda *args, **kwargs: stage.Outcome("deployed", receipt), scrub=lambda: None)
            deps = stage.ship.Deps(stdout=logs, target_path=production_file, clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc))
            with mock.patch.object(stage, "StageLane", return_value=fake_lane) as lane:
                self.assertEqual(stage.main(["deploy"], env, deps, adapter_factory=adapters, pins_path=pins_file, template_path=template_file), stage.ship.EXIT_OK)
                self.assertFalse(any(key.startswith("STAGING_") for key in lane.call_args.kwargs["env"]))
            self.assertNotIn("STAGING_DO_TOKEN", env); self.assertNotIn("STAGING_TARGET_JSON", env)
            ordinary = "\n".join(line for line in logs.getvalue().splitlines() if not line.startswith("::add-mask::"))
            for value in ("synthetic-provider-token-never-log", APP, PG, VK, VPC, ORIGIN, target["template_values"]["admin_email"]):
                self.assertNotIn(value, ordinary)
                self.assertIn("::add-mask::" + value, logs.getvalue())
            outputs = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(common.sha256_bytes(base64.b64decode(outputs["receipt_b64"])), outputs["receipt_sha256"])

    def test_unconfigured_or_mixed_context_never_constructs_adapter(self):
        production, template, pins, _ = data()
        cases = [({}, {key: None}) for key in ("team_uuid_sha256", "app_id_sha256")]
        cases.append(({}, {"team_uuid_sha256": None, "app_id_sha256": None}))
        cases.extend((changes, {}) for changes in (
            {"SHIP_MODE": "promote"}, {"SHIP_DO_TOKEN": ""},
            {"STAGING_CANARY_FIXTURE_JSON": "synthetic-private-value"}))
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file, template_file = (root / name for name in ("production.json", "pins.json", "template.json"))
            for path, value in ((production_file, production), (template_file, template)):
                path.write_bytes(common.canonical_file_bytes(value))
            for changes, pin_changes in cases:
                # Refusal must not depend on the real committed target failing
                # to match these synthetic identities. Other cases use valid pins.
                pins_file.write_bytes(common.canonical_file_bytes({**pins, **pin_changes}))
                env = self.environment(); env.update(changes); logs = io.StringIO()
                adapters = mock.Mock(side_effect=AssertionError("adapter must not be constructed"))
                deps = stage.ship.Deps(stdout=logs, target_path=production_file, clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc))
                with self.subTest(changes=list(changes), null_pins=list(pin_changes)), mock.patch.object(stage, "StageClient") as client:
                    self.assertEqual(stage.main(["deploy"], env, deps, adapter_factory=adapters,
                                               pins_path=pins_file, template_path=template_file), stage.ship.EXIT_REFUSED)
                    adapters.assert_not_called(); client.assert_not_called()
                    self.assertNotIn("synthetic-private-value", logs.getvalue())
                    self.assertNotIn("STAGING_DO_TOKEN", env)
                    self.assertNotIn("STAGING_TARGET_JSON", env)

    def test_rollback_candidate_and_drill_mismatch_refuse_before_adapters_or_provider(self):
        production, template, pins, _ = data()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file, template_file = (root / name for name in ("production.json", "pins.json", "template.json"))
            for path, value in ((production_file, production), (pins_file, pins), (template_file, template)):
                path.write_bytes(common.canonical_file_bytes(value))
            for change in ({"candidate_images": IMAGES}, {"candidate_source_sha": OLD_SOURCE}, {"drill": "e2e-fail"}):
                env = self.environment(); logs = io.StringIO()
                receipt = synthetic_receipt(candidate_sha256=env["CANDIDATE_SHA256"], **change)
                raw = contract.receipt_bytes(receipt)
                # Deliberately recompute a valid receipt hash: integrity alone
                # must not substitute for binding to the candidate payload.
                env.update(STAGE_RECEIPT_B64=base64.b64encode(raw).decode(), STAGE_RECEIPT_SHA256=common.sha256_bytes(raw))
                adapters = mock.Mock(side_effect=AssertionError("must refuse before adapter/provider"))
                deps = stage.ship.Deps(stdout=logs, target_path=production_file, clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc))
                with self.subTest(change=list(change)):
                    self.assertEqual(stage.main(["rollback"], env, deps, adapter_factory=adapters, pins_path=pins_file, template_path=template_file), stage.ship.EXIT_REFUSED)
                    adapters.assert_not_called()


if __name__ == "__main__": unittest.main()
