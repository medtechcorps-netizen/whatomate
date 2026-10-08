from __future__ import annotations

import base64
import copy
import io
import tempfile
import unittest
from pathlib import Path

import release_record
import ship
import ship_common as common
import stage_adapters
import test_ship_support as support


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

    def test_actual_plan_rechecks_chain_ci_schema_policy_and_binding(self):
        self.assertTrue(self.verify_plan())
        self.assertFalse(any("/approvals" in str(args) for args, _ in self.h.gh.calls))
        self.assertEqual(self.h.do.requests, [])
        self.h.env["PLAN_LATEST_MANIFEST_SHA256"] = "0" * 64
        with self.assertRaisesRegex(common.ReleaseError, "latest-changed-since-plan"):
            self.verify_plan()

    def test_candidate_tampering_run_binding_freshness_and_trivy_fail_before_gh(self):
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

    def test_missing_ci_old_lane_and_schema_change_stop_plan(self):
        self.h.gh.ci_runs["test.yml"] = []
        with self.assertRaisesRegex(common.ReleaseError, "ci-not-green"): self.verify_plan()
        self.h.gh.ci_green(self.head)
        self.h.gh.active["waiting"] = [support.run_record(".github/workflows/old-lane.yml", status="waiting", conclusion=None)]
        with self.assertRaisesRegex(common.ReleaseError, "old-lane-active"): self.verify_plan()
        self.h.gh.active["waiting"] = []
        self.repo.commit({"internal/database/changed.go": "package database\n"})
        # The candidate must identify that changed commit, not merely a dirty file.
        new_head = self.repo.head()
        self.h.env.update(support.context_env(new_head, self.root))
        self.h.gh.ci_green(new_head)
        encoded, digest = support.candidate_b64(support.NEW, new_head, support.NOW)
        self.h.env.update(CANDIDATE_B64=encoded, CANDIDATE_SHA256=digest)
        control = common.validate_context(self.h.env)
        adapters = stage_adapters.make_adapters(self.ctx, control)
        candidate = ship.decode_candidate(self.h.env, control, self.ctx.now())
        with self.assertRaisesRegex(common.ReleaseError, "schema-change-blocked"):
            adapters.verify_plan(candidate, digest, control["run_id"])

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
