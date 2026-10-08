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
        if path == "/_stage_drill_intentionally_missing": return 404, {}, b""
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


class LifecycleTests(unittest.TestCase):
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
            world = World(); world.health_failure = True
            def before_rollback_inventory(value):
                # Reader and initial writer have already been allocated. The
                # fourth client is the fresh rollback writer.
                if len(world.clients) == 4:
                    if mode == "cas": world.updated = "2026-10-07T13:00:00Z"
                    else: world.put_outcome = "rejected"
            world.inventory_hook = before_rollback_inventory
            with self.subTest(mode=mode), self.assertRaises(common.ReleaseError): world.deploy()
            self.assertEqual(len(world.writes), 1)
            self.assertTrue(all(client.puts <= 1 for client in world.clients))
            self.assertTrue(all(client.scrubbed for client in world.clients))

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
                self.assertEqual(world.probes[0], ORIGIN + "/_stage_drill_intentionally_missing")
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
                stable, count = lane.stable, 0
                def wrong_candidate(client):
                    nonlocal count
                    value = stable(client); count += 1
                    # Fail the post-PUT identity guard, leaving real provider
                    # state intact so the existing rollback can still verify it.
                    return stage.Snapshot(value.spec, value.images, OLD_ID, value.updated_sha256, value.spec_sha256) if count == 3 else value
                lane.stable = wrong_candidate
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
                    if len(world.clients) == 4:
                        if failure == "cas": world.updated = "2026-10-07T13:00:00Z"
                        else: world.put_outcome = "rejected"
                world.inventory_hook = before_rollback
            def request(url):
                result = world.request(url)
                if len(world.writes) == 2:
                    if failure == "health": return 503, {}, b"synthetic-private-body"
                    if failure == "after-health": world.updated = "2026-10-07T14:00:00Z"
                return result
            with self.subTest(failure=failure), self.assertRaises(common.ReleaseError):
                world.deploy(world.lane(request=request), drill="health-fail")
            self.assertEqual(world.probes[0], ORIGIN + "/_stage_drill_intentionally_missing")
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
    def environment(self):
        value = candidate()
        return {"STAGING_DO_TOKEN": "synthetic-provider-token-never-log", "STAGING_TARGET_JSON": json.dumps(data()[3]),
            "SHIP_MODE": "stage", "SHIP_DRILL": "none", "GITHUB_REPOSITORY": common.REPOSITORY,
            "GITHUB_REF": "refs/heads/main", "GITHUB_REF_PROTECTED": "true", "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_WORKFLOW_REF": common.SHIP_WORKFLOW_REF, "GITHUB_WORKFLOW_SHA": NEW_SOURCE, "GITHUB_SHA": NEW_SOURCE,
            "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "1234567",
            "CANDIDATE_B64": base64.b64encode(common.canonical_file_bytes(value)).decode(), "CANDIDATE_SHA256": common.sha256_bytes(common.canonical_file_bytes(value))}

    def world_cli(self, world, drill, *, request=None):
        logs = io.StringIO()
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            production_file, pins_file, template_file = (root / name for name in ("production.json", "pins.json", "template.json"))
            for path, value in ((production_file, world.production), (pins_file, world.pins), (template_file, world.template)):
                path.write_bytes(common.canonical_file_bytes(value))
            env = self.environment(); env["SHIP_DRILL"] = drill
            deps = stage.ship.Deps(stdout=logs, target_path=production_file,
                clock=lambda: dt.datetime(2026, 10, 7, 12, 1, tzinfo=dt.timezone.utc),
                https_request=request or world.request, sleeper=lambda _: None, poll_limit=3)
            adapters = lambda *_: SimpleNamespace(verify_products=world.verify_products,
                verify_staging=lambda binding: binding["source_sha"], verify_plan=lambda *_: True)
            with mock.patch.object(stage, "StageClient", side_effect=lambda *_, allow_put, **__: world.factory(allow_put=allow_put)):
                result = stage.main(["deploy"], env, deps, adapter_factory=adapters,
                    pins_path=pins_file, template_path=template_file)
        ordinary = "\n".join(line for line in logs.getvalue().splitlines() if not line.startswith("::add-mask::"))
        for private in ("synthetic-provider-token-never-log", "synthetic-private-body", APP, PG, VK, VPC, ORIGIN):
            self.assertNotIn(private, ordinary)
        return result, ordinary

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
                    self.assertEqual(logs.splitlines(), ["stage: failed-rolled-back", stage.HEALTH_DRILL_EVIDENCE])
                    self.assertEqual(len(world.writes), 2)

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
