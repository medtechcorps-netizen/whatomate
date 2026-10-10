from __future__ import annotations

import base64
import datetime as dt
import hashlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import release_record
import ship
import ship_common as common
import stage
import stage_adapters
import stage_contract
import stage_evidence
import test_ship_support as support
from test_ship_workflow import step
from test_stage import World


class PromotionEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo, self.base = support.base_repo(self.root / "repo")
        self.head = self.repo.commit({"README.md": "new release\n"})
        self.h = support.Harness(self.root, repo=self.repo, head=self.head, bootstrap_sha=self.base)

    def evidence(self):
        return [json.loads(base64.b64decode(self.h.env[name])) for name in ("STAGE_RECEIPT_B64", "STAGE_REPORT_B64")]

    def replace_evidence(self, receipt, report):
        raw = common.canonical_file_bytes(receipt)
        receipt_hash = common.sha256_bytes(raw)
        report["receipt_sha256"] = receipt_hash
        report_raw = common.canonical_file_bytes(report)
        self.h.env.update(STAGE_RECEIPT_B64=base64.b64encode(raw).decode(), STAGE_RECEIPT_SHA256=receipt_hash,
            STAGE_REPORT_B64=base64.b64encode(report_raw).decode(), STAGE_REPORT_SHA256=common.sha256_bytes(report_raw))

    def refused_before_network(self):
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED, self.h.text())
        self.assertEqual(self.h.do.requests, [])
        self.assertEqual(self.h.gh.calls, [])
        self.assertEqual(self.h.https.calls, [])

    def test_promote_workflow_environment_runs_real_stage_cli_and_adapters(self):
        self.h.production_env("promote")
        self.assertEqual(self.h.run("plan"), ship.EXIT_OK, self.h.text())
        plan = self.h.outputs()
        world = World()
        for binding in (world.target["graph_stub"], world.target["bootstrap"]):
            binding["source_sha"] = self.base
            repository, digest = binding["image"].split("@")
            self.h.gh.image_attestations[(repository, digest)] = [{
                "workflow": stage_adapters.STAGING_SIGNER, "signer": self.base,
                "source": self.base, "predicate_type": release_record.SLSA_PREDICATE,
                "predicate": {}}]
        world.spec = stage_contract.expected_spec(world.target, world.template, support.LIVE)
        world.deployments[world.active] = world.deployment(world.active, world.spec, "ACTIVE")
        pins_file, template_file = self.root / "stage-pins.json", self.root / "stage-template.json"
        for path, value in ((pins_file, world.pins), (template_file, world.template)):
            path.write_bytes(common.canonical_file_bytes(value))
        values = {
            "inputs.mode": "promote", "inputs.drill": "none", "inputs.target_release": "",
            "secrets.STAGING_DO_TOKEN": "synthetic-staging-provider-token",
            "secrets.STAGING_TARGET_JSON": json.dumps(world.target),
            "github.token": self.h.env["GH_TOKEN"],
            "needs.plan.outputs.latest_release": plan["latest_release"],
            "needs.plan.outputs.latest_manifest_sha256": plan["latest_manifest_sha256"],
            "needs.attest.outputs.candidate_b64": self.h.env["CANDIDATE_B64"],
            "needs.attest.outputs.candidate_sha256": self.h.env["CANDIDATE_SHA256"],
        }
        environment = support.context_env(self.head, self.root)
        for name, value in step("deploy-staging", "Verify and deploy staging")["env"].items():
            environment[name] = values[value[3:-3].strip()] if value.startswith("${{") else value
        self.assertEqual(environment["SHIP_MODE"], "stage")
        deps = self.h.deps()
        deps.https_request = world.request
        # Keep the actual CLI, lifecycle and GH verification adapters together;
        # only the provider transport and GH subprocess boundary are synthetic.
        with mock.patch.object(stage, "StageClient", side_effect=lambda *args, allow_put, **kwargs: world.factory(allow_put=allow_put)):
            result = stage.main(["deploy"], environment, deps, pins_path=pins_file, template_path=template_file)
        self.assertEqual(result, ship.EXIT_OK, self.h.text())
        self.assertEqual(len(world.writes), 1)
        self.assertEqual(stage_contract.image_set(world.writes[0]), support.NEW)
        self.assertEqual(len(world.probes), 6)
        receipt = json.loads(base64.b64decode(self.h.outputs()["receipt_b64"]))
        self.assertEqual(receipt["candidate_source_sha"], self.head)
        self.assertEqual(receipt["candidate_sha256"], self.h.env["CANDIDATE_SHA256"])
        self.assertEqual(receipt["run_id"], support.RUN_ID)
        self.assertEqual(receipt["drill"], "none")
        self.assertFalse(any("/approvals" in str(args) for args, _ in self.h.gh.calls))

    def test_missing_tampered_noncanonical_or_extra_report_refuses_before_network(self):
        for field in ("STAGE_REPORT_B64", "STAGE_REPORT_SHA256", "STAGE_RECEIPT_B64", "STAGE_RECEIPT_SHA256"):
            with self.subTest(missing=field):
                self.h.production_env(); self.h.env.pop(field)
                self.refused_before_network()
        for change in ({"STAGE_REPORT_B64": "invalid!"}, {"STAGE_REPORT_SHA256": "0" * 64},
                       {"STAGE_RECEIPT_SHA256": "0" * 64}):
            with self.subTest(change=change):
                self.h.production_env(); self.h.env.update(change)
                self.refused_before_network()
        self.h.production_env()
        receipt, report = self.evidence(); report["extra"] = "forbidden"
        self.replace_evidence(receipt, report); self.refused_before_network()
        self.h.production_env()
        _, report = self.evidence()
        raw = json.dumps(report, indent=2).encode()
        self.h.env.update(STAGE_REPORT_B64=base64.b64encode(raw).decode(), STAGE_REPORT_SHA256=common.sha256_bytes(raw))
        self.refused_before_network()

    def test_foreign_run_candidate_app_source_images_and_origin_refuse(self):
        for field, value in (("run_id", "9999"), ("candidate_sha256", "a" * 64), ("candidate_source_sha", self.base),
                             ("candidate_images", support.OTHER), ("app_id_sha256", "b" * 64)):
            with self.subTest(field=field):
                self.h.production_env(); receipt, report = self.evidence()
                receipt[field] = value
                self.replace_evidence(receipt, report); self.refused_before_network()
        for change in ({"run_id": "9999"}, {"candidate_sha256": "a" * 64}, {"origin_sha256": "b" * 64}):
            with self.subTest(report=change):
                self.h.production_env(); receipt, report = self.evidence()
                report.update(change)
                self.replace_evidence(receipt, report); self.refused_before_network()
        self.h.production_env(); receipt, report = self.evidence()
        receipt["ingress_sha256"] = report["origin_sha256"] = support.make_target()["default_ingress_sha256"]
        self.replace_evidence(receipt, report); self.refused_before_network()

    def test_exact_thirteen_passes_and_none_drill_are_required(self):
        for passed in (0, 12, 14, True, "13"):
            with self.subTest(passed=passed):
                self.h.production_env(); receipt, report = self.evidence()
                report["passed"] = passed
                self.replace_evidence(receipt, report); self.refused_before_network()
        for change in (lambda checks: checks[:-1], lambda checks: checks[::-1], lambda checks: [checks[0]] * 13):
            self.h.production_env(); receipt, report = self.evidence()
            report["checks"] = change(report["checks"])
            self.replace_evidence(receipt, report); self.refused_before_network()
        for drill in ("e2e-fail", "health-fail", "bad-image"):
            with self.subTest(drill=drill):
                self.h.production_env(); receipt, report = self.evidence()
                receipt["drill"] = drill
                self.replace_evidence(receipt, report); self.refused_before_network()

    def test_unconfigured_pins_refuse_normal_promote(self):
        self.h.production_env()
        self.h.paths["staging_pins"].write_bytes(common.canonical_file_bytes({
            "schema_version": 1, "profile": "staging", "team_uuid_sha256": None, "app_id_sha256": None}))
        self.refused_before_network()

    def test_dry_run_rollback_and_bypass_never_read_staging_evidence(self):
        self.h.paths["staging_pins"].unlink()
        with mock.patch.object(stage_evidence, "verify", side_effect=AssertionError("staging must not be consulted")) as verify:
            self.h.production_env("dry-run")
            self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
            self.h.production_env("rollback", target_release="prod-0000")
            self.h.env["PLAN_TARGET_MANIFEST_SHA256"] = self.h.bootstrap_sha256
            self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
            self.assertIn("nothing-to-roll-back", self.h.text())
            self.h.production_env(ship.BYPASS_MODE)
            self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
            self.assertEqual(self.h.outputs()["kind"], "promote")
            verify.assert_not_called()

    def test_bypass_requires_owner_dispatch_and_unchanged_schema(self):
        for actor in (None, "someone-else"):
            self.h.production_env(ship.BYPASS_MODE)
            if actor is None: self.h.env.pop("GITHUB_ACTOR", None)
            else: self.h.env["GITHUB_ACTOR"] = actor
            self.refused_before_network()
        self.head = self.repo.commit({"internal/database/new.go": "package database\n"})
        self.h.head = self.head
        self.h.env.update(support.context_env(self.head, self.root))
        self.h.production_env(ship.BYPASS_MODE)
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("schema-change-blocked", self.h.text())
        self.assertEqual(self.h.do.requests, [])

    def test_bypass_keeps_ci_trivy_and_owner_approval_gates(self):
        self.h.production_env(ship.BYPASS_MODE)
        self.h.gh.ci_runs["test.yml"] = []
        self.assertEqual(self.h.run("plan"), ship.EXIT_REFUSED)
        self.assertIn("ci-not-green", self.h.text())
        self.h.gh.ci_green(self.head)
        self.h.paths["policy"].write_bytes(b"CVE-2026-1 exp:2026-01-01\n")
        self.assertEqual(self.h.run("plan"), ship.EXIT_REFUSED)
        self.assertIn("trivy-exception-invalid", self.h.text())
        self.h.paths["policy"].write_bytes(b"# no exceptions\n")
        self.h.gh.approvals[support.RUN_ID] = []
        self.assertEqual(self.h.run("production"), ship.EXIT_REFUSED)
        self.assertIn("approval-missing", self.h.text())
        self.assertEqual(self.h.do.requests, [])

    def test_bypass_record_extends_chain_and_next_staged_promote_passes_drift(self):
        self.h.production_env(ship.BYPASS_MODE)
        self.assertEqual(self.h.run("plan"), ship.EXIT_OK, self.h.text())
        self.assertIn("[!CAUTION]", self.h.summary())
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        production = self.h.outputs()
        self.assertEqual(production["kind"], "promote")
        self.assertEqual(self.h.do.put_count(), 1)
        record_keys = {"release", "kind", "sha", "web_digest", "meta_relay_digest", "gmail_relay_digest", "previous",
            "rolled_back_from", "deployed_at", "built_at", "reconciled", "environment_values_sha256", "non_source_projection_sha256"}
        self.h.env.update({"REC_" + key.upper(): production[key] for key in record_keys})
        self.assertEqual(self.h.run("record", "--stage", "manifest"), ship.EXIT_OK, self.h.text())
        path = Path(self.h.outputs()["path"])
        raw = path.read_bytes(); manifest = json.loads(raw)
        self.assertEqual(manifest["kind"], "promote")
        self.assertEqual(set(manifest), release_record.MANIFEST_KEYS)
        self.assertNotIn(b"bypass", raw)
        self.h.gh.attested_files[hashlib.sha256(raw).hexdigest()] = self.head
        self.assertEqual(self.h.run("record", "--stage", "publish"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.run("record", "--stage", "verify"), ship.EXIT_OK, self.h.text())
        self.assertIn("staging: bypassed", (path.parent / "notes.md").read_text())
        ctx = ship.Context(self.h.env, self.h.deps(), common.Output(stdout=io.StringIO()))
        chain = release_record.resolve_chain(ctx.gh(), self.repo.root, bootstrap=self.h.bootstrap, work_dir=self.root / "verified-chain")
        self.assertEqual(chain.latest.release, manifest["release"])
        release_record.require_link(list(chain.entries[:-1]), chain.latest, self.repo.root)

        next_head = self.repo.commit({"README.md": "next normal staged release\n"})
        self.h.head = next_head
        self.h.env.update(support.context_env(next_head, self.root))
        self.h.clock_value += dt.timedelta(hours=1)
        self.h.gh.ci_green(next_head)
        self.h.production_env("promote", images=support.OTHER, built_at=self.h.clock_value)
        self.h.env.update(PLAN_LATEST_RELEASE=manifest["release"], PLAN_LATEST_MANIFEST_SHA256=common.sha256_bytes(raw))
        self.assertEqual(self.h.run("production"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.do.put_count(), 2)
        self.assertEqual(self.h.outputs()["kind"], "promote")
        self.assertEqual(self.h.outputs()["previous"], manifest["release"])


if __name__ == "__main__": unittest.main()
