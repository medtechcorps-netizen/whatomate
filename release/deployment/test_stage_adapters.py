from __future__ import annotations

import base64
import copy
import io
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import release_record
import ship
import ship_common as common
import stage_adapters
import test_compat_request as composition
import test_ship_support as support
import test_stage as lifecycle


class DiscoveryGh(support.FakeGh):
    """The subprocess boundary: discovery has no caller-supplied source SHA."""

    def __init__(self):
        super().__init__()
        self.discovery_changes = {}
        self.discovery_result = None

    def _verify(self, argv, args):
        if "--source-digest" in args:
            return super()._verify(argv, args)
        image, _, digest = args[2].removeprefix("oci://").partition("@")
        if (image != common.IMAGE_REPOSITORY["web"] or
                self._flag(args, "--repo") != common.REPOSITORY or
                self._flag(args, "--cert-identity") != self.identity(release_record.SHIP_SIGNER) or
                self._flag(args, "--source-ref") != "refs/heads/main" or
                self._flag(args, "--predicate-type") != release_record.SLSA_PREDICATE or
                "--deny-self-hosted-runners" not in args or "--cert-identity-regex" in args):
            return self._result(argv, 1)
        if self.discovery_result is not None:
            return self._json(argv, self.discovery_result)
        entries = [entry for entry in self.image_attestations.get((image, digest), [])
                   if entry["predicate_type"] == release_record.SLSA_PREDICATE]
        if not entries:
            return self._result(argv, 1)
        return self._json(argv, [{"verificationResult": {"signature": {"certificate": {
            **self._certificate(self.identity(entry["workflow"]), entry["signer"], entry["source"]),
            **self.discovery_changes}}, "statement": {"predicate": {"source_sha": "f" * 40}}}}
            for entry in entries])


class AdapterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Typed mixed-v2 projection from controlled resolver/Actions captures.
        # Adapter tests mock the top-level gate, never claim live qualification.
        composition.source.ForwardResolverTests.setUpClass()
        forward = composition.resolution()
        history = composition.collected(forward)
        normal = stage_adapters.ship_flow.ship_compat.NormalPreflight(forward,
            composition.bridge.qualify(forward, history), history, 1, 1)
        cls.gate_value = stage_adapters.ship_flow.Gate(forward.resolved, normal, None)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo, self.base = support.base_repo(self.root / "repo")
        self.previous = self.repo.commit({"README.md": "previous staging build\n"})
        self.head = self.repo.commit({"README.md": "new staging build\n"})
        self.h = support.Harness(self.root, repo=self.repo, head=self.head, bootstrap_sha=self.base)
        self.h.gh = DiscoveryGh()
        self.h.gh.attest_legacy_images(support.LIVE, self.base)
        self.h.gh.ci_green(self.head)
        self.h.production_env("stage")
        self.h.env.pop("SHIP_DO_TOKEN")
        self.h.env.pop("SHIP_TARGET_JSON")
        self.h.env["SHIP_DRILL"] = "none"
        self.control = common.validate_context(self.h.env)
        self.ctx = ship.Context(self.h.env, self.h.deps(), common.Output(stdout=io.StringIO()))
        self.adapters = stage_adapters.make_adapters(self.ctx, self.control)
        self.candidate = ship.decode_candidate(self.h.env, self.control, self.ctx.now())
        self.prior_images = {key: "sha256:" + str(index + 5) * 64 for index, key in enumerate(common.COMPONENTS)}
        self.h.gh.attest_ship_images(self.prior_images, self.previous)

    def verify_plan(self):
        return self.adapters.verify_plan(self.candidate, self.h.env["CANDIDATE_SHA256"], self.control["run_id"])

    def test_construction_is_pure_and_rejects_mixed_context(self):
        self.assertEqual(self.h.gh.calls, [])
        self.assertEqual(self.h.do.requests, [])
        for extra in ({"STAGING_DO_TOKEN": ""}, {"SHIP_DO_TOKEN": ""}, {"DO_TOKEN": ""}, {"SHIP_MODE": "promote"}):
            with self.subTest(extra=extra):
                saved = dict(self.h.env)
                self.h.env.update(extra)
                with self.assertRaises(common.ReleaseError):
                    stage_adapters.make_adapters(self.ctx, self.control)
                self.h.env.clear(); self.h.env.update(saved)
        self.assertEqual(self.h.gh.calls, [])

    def planned_latest(self):
        latest = self.gate_value.resolved.latest
        self.h.env.update(PLAN_LATEST_RELEASE=latest.release,
                         PLAN_LATEST_MANIFEST_SHA256=latest.manifest_sha256)

    def test_plan_consumes_mixed_gate_and_forwards_exact_context(self):
        self.planned_latest()
        self.assertIsNotNone(self.gate_value.resolved.latest.live)
        self.assertNotEqual(self.gate_value.resolved.latest.actual_web_source_sha,
                            self.gate_value.resolved.latest.live.origin_source.sha)
        with mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True,
                return_value=self.gate_value) as gate, \
                mock.patch.object(stage_adapters.time, "monotonic", return_value=10.0), \
                mock.patch.object(release_record, "resolve_chain", side_effect=AssertionError("legacy chain used")):
            self.assertTrue(self.verify_plan())
        gate.assert_called_once_with(self.ctx, self.control, mode="stage", target="", deadline=2410.0)
        self.assertFalse(any("/approvals" in str(args) for args, _ in self.h.gh.calls))
        self.assertEqual(self.h.do.requests, [])

    def test_planned_latest_tag_or_hash_must_match_fresh_gate(self):
        for field in ("PLAN_LATEST_RELEASE", "PLAN_LATEST_MANIFEST_SHA256"):
            with self.subTest(field=field):
                self.planned_latest()
                self.h.env[field] = "0" * 64
                with mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True,
                        return_value=self.gate_value) as gate:
                    with self.assertRaisesRegex(common.ReleaseError, "latest-changed-since-plan"):
                        self.verify_plan()
                gate.assert_called_once()
                self.assertEqual(self.h.do.requests, [])

    def test_source_deadline_precedes_clients_and_is_not_reset_after_ci(self):
        self.planned_latest()
        clock = [10.0]
        gh = self.ctx.gh
        ci = ship.require_ci_green
        old_lanes = ship.require_old_lanes_idle
        def make_gh():
            clock[0] += 20.0
            return gh()
        def check_ci(*args):
            clock[0] += 30.0
            return ci(*args)
        def check_lanes(*args):
            clock[0] += 40.0
            return old_lanes(*args)
        def gated(ctx, control, mode, target, *, deadline):
            self.assertEqual(clock[0], 100.0)
            self.assertEqual(deadline, 2410.0)
            return self.gate_value
        with mock.patch.object(stage_adapters.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(self.ctx, "gh", side_effect=make_gh), \
                mock.patch.object(ship, "require_ci_green", side_effect=check_ci), \
                mock.patch.object(ship, "require_old_lanes_idle", side_effect=check_lanes), \
                mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True, side_effect=gated):
            self.assertTrue(self.verify_plan())

    def test_source_refusals_propagate_before_provider_or_image_verification(self):
        for code in ("candidate-invalid:compat-stale", "candidate-invalid:compat-missing",
                     "candidate-invalid:compat-old-server-incompatible", "schema-change-blocked:fixture",
                     "record-chain-invalid:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True,
                        side_effect=common.ReleaseError(code)) as gate:
                with self.assertRaises(common.ReleaseError) as caught:
                    self.verify_plan()
                self.assertEqual(caught.exception.code, code)
                gate.assert_called_once()
                self.assertEqual(self.h.do.requests, [])
                self.assertFalse(any(args[1:3] == ["attestation", "verify"] for args, _ in self.h.gh.calls))

    def test_real_lane_does_not_construct_provider_after_gate_refusal(self):
        world = lifecycle.World()
        lane = world.lane(verify_plan=self.adapters.verify_plan)
        with mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True,
                side_effect=common.ReleaseError("candidate-invalid:compat-stale")):
            with self.assertRaisesRegex(common.ReleaseError, "candidate-invalid:compat-stale"):
                lane.deploy(self.candidate, candidate_sha256=self.h.env["CANDIDATE_SHA256"],
                            run_id=self.control["run_id"])
        self.assertEqual(world.clients, [])
        self.assertEqual(world.writes, [])
        self.assertEqual(world.probes, [])
        self.assertEqual(world.verified, [])

    def test_candidate_tampering_run_binding_freshness_and_trivy_fail_before_gh(self):
        patcher = mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True)
        gate = patcher.start()
        self.addCleanup(patcher.stop)
        original = copy.deepcopy(self.candidate)
        for change in ({"built_at": "2026-10-01T00:00:00Z"}, {"sha": "f" * 40},
                       {"trivy": {"db_updated_at": "2026-10-01T00:00:00Z", "active_exceptions": 0}},
                       {"trivy": {"db_updated_at": "2026-10-04T06:00:00Z", "active_exceptions": 1}}):
            with self.subTest(change=change):
                self.candidate = {**copy.deepcopy(original), **change}
                raw = common.canonical_file_bytes(self.candidate)
                self.h.env.update(CANDIDATE_B64=base64.b64encode(raw).decode(), CANDIDATE_SHA256=common.sha256_bytes(raw))
                with self.assertRaises(common.ReleaseError): self.verify_plan()
                self.assertEqual(self.h.gh.calls, [])
        with self.assertRaises(common.ReleaseError):
            self.adapters.verify_plan(original, "0" * 64, "99999")
        gate.assert_not_called()

    def test_missing_ci_and_old_lane_stop_before_source_gate(self):
        with mock.patch.object(stage_adapters.ship_flow, "gate", autospec=True) as gate:
            self.h.gh.ci_runs["test.yml"] = []
            with self.assertRaisesRegex(common.ReleaseError, "ci-not-green"): self.verify_plan()
            self.h.gh.ci_green(self.head)
            self.h.gh.active["waiting"] = [support.run_record(".github/workflows/old-lane.yml", status="waiting", conclusion=None)]
            with self.assertRaisesRegex(common.ReleaseError, "old-lane-active"): self.verify_plan()
        gate.assert_not_called()
        self.assertEqual(self.h.do.requests, [])

    def test_prior_staging_source_need_not_be_a_production_record(self):
        self.assertEqual(self.h.gh.releases, [])
        self.assertEqual(self.adapters.verify_products(self.prior_images), self.previous)
        attestation_calls = [args for args, _ in self.h.gh.calls if args[1:3] == ["attestation", "verify"]]
        self.assertEqual(len(attestation_calls), 7)  # discovery, three provenance, three SBOM
        self.assertNotIn("--source-digest", attestation_calls[0])
        for args in attestation_calls[1:]:
            self.assertEqual(self.h.gh._flag(args, "--source-digest"), self.previous)
            self.assertEqual(self.h.gh._flag(args, "--signer-digest"), self.previous)
        self.assertEqual(self.h.do.requests, [])

    def test_known_candidate_and_legacy_bootstrap_use_exact_verification(self):
        self.assertEqual(self.adapters.verify_products(support.NEW, self.head), self.head)
        self.assertEqual(self.adapters.verify_products(support.LIVE), self.base)
        with self.assertRaises(common.ReleaseError): self.adapters.verify_products(support.LIVE, self.head)
        self.assertFalse(any("--source-digest" not in args for args, _ in self.h.gh.calls if args[1:3] == ["attestation", "verify"]))

    def test_untrusted_certificate_fields_cannot_select_the_prior_source(self):
        for changes in ({"sourceRepositoryDigest": "bad"}, {"buildSignerDigest": "f" * 40},
                        {"sourceRepositoryRef": "refs/heads/other"}, {"runnerEnvironment": "self-hosted"},
                        {"buildSignerURI": "https://foreign.invalid/workflow"},
                        {"subjectAlternativeName": "https://foreign.invalid/workflow"}):
            with self.subTest(changes=changes):
                self.h.gh.discovery_changes = changes
                with self.assertRaisesRegex(common.ReleaseError, "attestation-unverified"):
                    self.adapters.verify_products(self.prior_images)
        self.h.gh.discovery_changes = {}
        for value in ({}, [None], [{"verificationResult": {}}]):
            self.h.gh.discovery_result = value
            with self.assertRaises(common.ReleaseError): self.adapters.verify_products(self.prior_images)

    def test_missing_other_component_attestation_and_mixed_sources_refuse(self):
        key = (common.IMAGE_REPOSITORY["gmail-relay"], self.prior_images["gmail-relay"])
        original = copy.deepcopy(self.h.gh.image_attestations[key])
        for entries in ([], [{**entry, "source": self.head, "signer": self.head} for entry in original],
                        [entry for entry in original if entry["predicate_type"] == release_record.SLSA_PREDICATE]):
            with self.subTest(entries=len(entries)):
                self.h.gh.image_attestations[key] = entries
                with self.assertRaisesRegex(common.ReleaseError, "attestation-unverified"):
                    self.adapters.verify_products(self.prior_images)

    def test_stub_uses_only_the_staging_image_signer_and_bound_source(self):
        image = "ghcr.io/medtechcorps-netizen/rereply-staging-graph-stub"
        digest = "sha256:" + "9" * 64
        binding = {"image": image + "@" + digest, "source_sha": self.previous}
        self.h.gh.image_attestations[(image, digest)] = [{"workflow": stage_adapters.STAGING_SIGNER,
            "signer": self.previous, "source": self.previous, "predicate_type": release_record.SLSA_PREDICATE, "predicate": {}}]
        self.assertEqual(self.adapters.verify_staging(binding), self.previous)
        self.h.gh.image_attestations[(image, digest)][0]["workflow"] = release_record.SHIP_SIGNER
        with self.assertRaises(common.ReleaseError): self.adapters.verify_staging(binding)
        with self.assertRaises(common.ReleaseError): self.adapters.verify_staging({**binding, "image": "oci://foreign.invalid/x"})
        for _, env in self.h.gh.calls:
            self.assertFalse(any(key.startswith("STAGING_") for key in env))
            self.assertNotIn("SHIP_DO_TOKEN", env)


if __name__ == "__main__": unittest.main()
