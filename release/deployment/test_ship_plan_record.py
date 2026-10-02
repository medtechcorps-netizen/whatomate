from __future__ import annotations

import base64
import datetime as dt
import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record
import ship
import ship_common as common
import test_ship_support as support


HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent


class StageCase(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.repo, self.base = support.base_repo(self.root / "repo")
        self.head = self.repo.commit({"README.md": "readme v2\n"}, "Add the second readme line")
        self.h = support.Harness(self.root, repo=self.repo, head=self.head, bootstrap_sha=self.base)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def plan(self, mode: str = "promote", target: str = "") -> int:
        self.h.env.update({"SHIP_MODE": mode, "SHIP_TARGET_RELEASE": target})
        return self.h.run("plan")

    def assert_clean_output(self) -> None:
        self.assertEqual(support.scan_private(self.h.text() + self.h.summary() + json.dumps(self.h.outputs())), [])


class PlanTests(StageCase):
    def test_promote_plan_outputs_and_summary(self) -> None:
        self.assertEqual(self.plan(), ship.EXIT_OK, self.h.text())
        outputs = self.h.outputs()
        self.assertEqual(outputs, {
            "latest_release": "prod-0000",
            "latest_manifest_sha256": self.h.bootstrap_sha256,
            "latest_sha": self.base,
            "target_release": "",
            "target_manifest_sha256": "",
        })
        summary = self.h.summary()
        self.assertIn(ship.APPROVAL_BANNER, summary)
        self.assertIn("Add the second readme line", summary)
        self.assertIn("Schema guard: clean", summary)
        self.assertIn("CI: Test and E2E push runs on main succeeded", summary)
        self.assertIn(ship.APPROVAL_BANNER, self.h.text())
        self.assert_clean_output()

    def test_ci_gate_refusals(self) -> None:
        cases = {
            "missing": [],
            "failed": [support.run_record(".github/workflows/test.yml", conclusion="failure", sha=self.head)],
            "cancelled": [support.run_record(".github/workflows/test.yml", conclusion="cancelled", sha=self.head)],
            "in-progress": [support.run_record(".github/workflows/test.yml", status="in_progress", conclusion=None, sha=self.head)],
            "pull-request": [support.run_record(".github/workflows/test.yml", event="pull_request", sha=self.head)],
            "other-branch": [support.run_record(".github/workflows/test.yml", branch="feature", sha=self.head)],
            "other-path": [support.run_record(".github/workflows/other.yml", sha=self.head)],
        }
        for label, runs in cases.items():
            with self.subTest(case=label):
                self.h.gh.ci_green(self.head)
                self.h.gh.ci_runs["test.yml"] = runs
                self.assertEqual(self.plan(), ship.EXIT_REFUSED)
                self.assertIn("refused: ci-not-green", self.h.text())
        self.h.gh.ci_green(self.head)
        self.h.gh.ci_runs["e2e-tests.yml"] = []
        self.assertEqual(self.plan(), ship.EXIT_REFUSED)

    def test_approval_gate_must_be_configured_before_anyone_approves(self) -> None:
        for label, mutate in support.APPROVAL_GATE_MUTATIONS.items():
            for mode, target in (("promote", ""), ("dry-run", ""), ("rollback", "prod-0000")):
                with self.subTest(case=label, mode=mode):
                    self.h.gh = support.FakeGh()
                    self.h.gh.ci_green(self.head)
                    mutate(self.h.gh)
                    self.h.stdout.truncate(0)
                    self.h.stdout.seek(0)
                    self.assertEqual(self.plan(mode, target), ship.EXIT_REFUSED)
                    self.assertIn("refused: approval-gate-misconfigured", self.h.text())
                    self.assert_clean_output()
        self.h.gh = support.FakeGh()
        self.h.gh.ci_green(self.head)
        self.assertEqual(self.plan(), ship.EXIT_OK, self.h.text())
        self.assertIn("Approval gate: environment production requires the owner's review", self.h.summary())

    def test_approval_gate_reads_only_the_environment(self) -> None:
        self.assertEqual(self.plan(), ship.EXIT_OK, self.h.text())
        paths = [argv[-1] for argv, _ in self.h.gh.calls if argv[1:2] == ["api"]]
        self.assertIn(f"/repos/{common.REPOSITORY}/environments/production", paths)
        self.assertIn(f"/repos/{common.REPOSITORY}/environments/production/deployment-branch-policies?per_page=100", paths)
        self.assertFalse(any("/approvals" in path for path in paths))
        self.assertFalse(any("pending_deployments" in " ".join(argv) for argv, _ in self.h.gh.calls))

    def test_ci_gate_is_skipped_for_rollback(self) -> None:
        self.h.gh.ci_runs = {"test.yml": [], "e2e-tests.yml": []}
        self.assertEqual(self.plan("rollback", "prod-0000"), ship.EXIT_OK, self.h.text())
        outputs = self.h.outputs()
        self.assertEqual(outputs["target_release"], "prod-0000")
        self.assertEqual(outputs["target_manifest_sha256"], self.h.bootstrap_sha256)
        self.assertIn("rollback (live equals the latest record", self.h.summary())

    def test_old_lane_gate(self) -> None:
        self.h.gh.active["waiting"] = [support.run_record(".github/workflows/old-lane.yml", status="waiting", conclusion=None)]
        self.assertEqual(self.plan(), ship.EXIT_REFUSED)
        self.assertIn("refused: old-lane-active", self.h.text())
        self.h.gh.active["waiting"] = [{"id": 1, "status": "waiting"}]
        self.assertEqual(self.plan(), ship.EXIT_REFUSED)
        self.assertIn("refused: gh-failed:active-runs", self.h.text())

    def test_old_lane_regex_matches_only_the_old_group(self) -> None:
        lanes = ship.old_lane_paths(self.repo.root)
        self.assertEqual(lanes, {".github/workflows/old-lane.yml"})
        live = ship.old_lane_paths(REPO_ROOT)
        self.assertNotIn(".github/workflows/ship.yml", live)
        self.assertNotIn(".github/workflows/release.yml", live)
        expected = {
            f".github/workflows/{path.name}"
            for path in (REPO_ROOT / ".github" / "workflows").glob("*.yml")
            if support.OLD_GROUP_LINE.strip() in path.read_text(encoding="utf-8")
        }
        self.assertEqual(live, expected)

    def test_inputs(self) -> None:
        for mode, target in (("rollback", ""), ("rollback", "latest"), ("promote", "prod-0000"), ("deploy", "")):
            with self.subTest(mode=mode, target=target):
                self.assertEqual(self.plan(mode, target), ship.EXIT_REFUSED)
                self.assertIn("refused: input-invalid", self.h.text())

    def test_schema_change_and_downgrade_in_plan(self) -> None:
        head = self.repo.commit({"internal/database/new.go": "package database\n"}, "db")
        self.h.env.update(support.context_env(head, self.root))
        self.h.gh.ci_green(head)
        self.assertEqual(self.plan(), ship.EXIT_REFUSED)
        self.assertIn("schema-change-blocked", self.h.text())
        self.assertIn("guarded-tree:internal/database/new.go", self.h.summary())

    def test_invalid_trivy_exceptions_stop_the_plan(self) -> None:
        self.h.paths["policy"].write_bytes(b"CVE-2026-1 exp:2026-01-01\n")
        self.assertEqual(self.plan(), ship.EXIT_REFUSED)
        self.assertIn("refused: trivy-exception-invalid", self.h.text())

    def test_unknown_rollback_target(self) -> None:
        self.assertEqual(self.plan("rollback", "prod-20990101T000000Z-99999999"), ship.EXIT_REFUSED)
        self.assertIn("rollback-target-invalid", self.h.text())

    def test_commit_subjects_with_private_looking_values_are_withheld(self) -> None:
        head = self.repo.commit({"README.md": "v3\n"}, "Fix " + support.APP_ID)
        self.h.env.update(support.context_env(head, self.root))
        self.h.gh.ci_green(head)
        self.assertEqual(self.plan(), ship.EXIT_OK, self.h.text())
        self.assertIn("(subject withheld)", self.h.summary())
        self.assert_clean_output()


def inspect_fixture(component: str, digest: str, sha: str) -> list:
    contract = ship.IMAGE_CONTRACT[component]
    image = common.IMAGE_REPOSITORY[component]
    return [{
        "Id": "sha256:" + "0" * 64,
        "RepoDigests": [f"{image}@{digest}"],
        "Os": "linux",
        "Architecture": "amd64",
        "Size": 1234,
        "Config": {
            "User": contract["user"],
            "WorkingDir": contract["working_dir"],
            "Entrypoint": contract["entrypoint"],
            "Cmd": contract["cmd"],
            "ExposedPorts": {contract["port"]: {}},
            "Labels": {
                "org.opencontainers.image.source": "https://github.com/medtechcorps-netizen/whatomate",
                "org.opencontainers.image.revision": sha,
                "org.opencontainers.image.version": f"ship-{sha}",
                "org.opencontainers.image.title": f"rereply-release-{component}",
                "io.rereply.release.workflow": "ship",
            },
        },
    }]


SBOM = {"spdxVersion": "SPDX-2.3", "documentNamespace": "https://example.invalid/spdx/1", "packages": [{"name": "x"}]}


class ImageStageTests(StageCase):
    def setUp(self) -> None:
        super().setUp()
        dockerfiles = {
            f"docker/release/{component}.Dockerfile": (REPO_ROOT / "docker" / "release" / f"{component}.Dockerfile").read_text(encoding="ascii")
            for component in common.COMPONENTS
        }
        self.head = self.repo.commit(dockerfiles, "Add release Dockerfiles")
        self.h.env.update(support.context_env(self.head, self.root))
        self.scan = Path(self.h.env["RUNNER_TEMP"]) / "ship-scan"
        self.scan.mkdir()

    def stage(self, stage: str, component: str = "web", **env: str) -> int:
        self.h.env.update(env)
        return self.h.run("image", "--stage", stage, "--component", component)

    def write_scan(self, component: str, digest: str, *, vulnerabilities: list | None = None, secrets: list | None = None,
                   sbom: dict | None = None) -> None:
        (self.scan / "inspect.json").write_text(json.dumps(inspect_fixture(component, digest, self.head)), encoding="ascii")
        (self.scan / "sbom.spdx.json").write_text(json.dumps(sbom or SBOM), encoding="ascii")
        (self.scan / "vulnerability-report.json").write_text(json.dumps(
            {"Results": [{"Target": "x", "Vulnerabilities": vulnerabilities or []}]}), encoding="ascii")
        (self.scan / "secret-report.json").write_text(json.dumps(
            {"Results": [{"Target": "x", "Secrets": secrets or []}]}), encoding="ascii")
        database = Path(self.h.env["RUNNER_TEMP"]) / "trivy-cache" / "db"
        database.mkdir(parents=True, exist_ok=True)
        (database / "trivy.db").write_bytes(b"db")
        (database / "metadata.json").write_text(json.dumps({"Version": 2, "UpdatedAt": "2026-10-04T06:15:23.123456789Z"}), encoding="ascii")

    def test_committed_release_dockerfiles_pass_the_pin_check(self) -> None:
        for component in common.COMPONENTS:
            with self.subTest(component=component):
                digest = ship.check_dockerfile_pins(REPO_ROOT, component)
                self.assertEqual(digest, hashlib.sha256((REPO_ROOT / ship.DOCKERFILES[component]).read_bytes()).hexdigest())

    def test_pin_refusals(self) -> None:
        original = (REPO_ROOT / "docker/release/meta-relay.Dockerfile").read_text(encoding="ascii")
        web = (REPO_ROOT / "docker/release/web.Dockerfile").read_text(encoding="ascii")
        cases = {
            "unpinned-from": original.replace("@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae", "", 1),
            "other-platform": original.replace("--platform=linux/amd64", "--platform=linux/arm64", 1),
            "copy-from-image": original.replace("COPY --from=builder /out/passwd", "COPY --from=alpine:3 /etc/passwd", 1),
            "mount-from-image": original.replace("RUN go mod download", "RUN --mount=type=bind,from=alpine:3,target=/x go mod download", 1),
            "syntax-directive": "# syntax=docker/dockerfile:1\n" + original,
            "onbuild": original + "ONBUILD RUN true\n",
        }
        web_cases = {
            "add-without-checksum": web.replace("ADD --checksum=sha256:a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992 ", "ADD ", 1),
            "add-http": web.replace("https://github.com/rhasspy/piper", "http://github.com/rhasspy/piper", 1),
        }
        for label, (component, text) in {**{k: ("meta-relay", v) for k, v in cases.items()},
                                          **{k: ("web", v) for k, v in web_cases.items()}}.items():
            with self.subTest(case=label):
                self.repo.commit({ship.DOCKERFILES[component]: text}, label)
                with self.assertRaisesRegex(common.ReleaseError, "^dockerfile-pin-invalid$"):
                    ship.check_dockerfile_pins(self.repo.root, component)
                self.repo.commit({ship.DOCKERFILES[component]: (REPO_ROOT / ship.DOCKERFILES[component]).read_text(encoding="ascii")})

    def test_pin_check_requires_a_clean_tracked_100644_blob(self) -> None:
        path = self.repo.root / ship.DOCKERFILES["gmail-relay"]
        path.write_bytes(path.read_bytes() + b"# edited\n")
        with self.assertRaisesRegex(common.ReleaseError, "^dockerfile-pin-invalid$"):
            ship.check_dockerfile_pins(self.repo.root, "gmail-relay")
        self.repo.git("checkout", "--", ship.DOCKERFILES["gmail-relay"])
        self.repo.git("update-index", "--chmod=+x", ship.DOCKERFILES["gmail-relay"])
        self.repo.git("commit", "--quiet", "-m", "exec")
        with self.assertRaisesRegex(common.ReleaseError, "^dockerfile-pin-invalid$"):
            ship.check_dockerfile_pins(self.repo.root, "gmail-relay")

    def test_contract_stage(self) -> None:
        digest = support.NEW["web"]
        self.write_scan("web", digest)
        env = {"IMAGE": common.IMAGE_REPOSITORY["web"], "DIGEST": digest}
        self.assertEqual(self.stage("contract", **env), ship.EXIT_OK, self.h.text())
        for label, mutate in {
            "user": lambda item: item["Config"].update({"User": "root"}),
            "entrypoint": lambda item: item["Config"].update({"Entrypoint": ["/bin/sh"]}),
            "cmd": lambda item: item["Config"].update({"Cmd": None}),
            "port": lambda item: item["Config"].update({"ExposedPorts": {"9000/tcp": {}}}),
            "revision": lambda item: item["Config"]["Labels"].update({"org.opencontainers.image.revision": self.base}),
            "workflow-label": lambda item: item["Config"]["Labels"].pop("io.rereply.release.workflow"),
            "repo-digest": lambda item: item.update({"RepoDigests": []}),
            "arch": lambda item: item.update({"Architecture": "arm64"}),
        }.items():
            fixture = inspect_fixture("web", digest, self.head)
            mutate(fixture[0])
            (self.scan / "inspect.json").write_text(json.dumps(fixture), encoding="ascii")
            with self.subTest(case=label):
                self.assertEqual(self.stage("contract", **env), ship.EXIT_REFUSED)
                self.assertIn("refused: image-contract-differs", self.h.text())
        self.assertEqual(self.stage("contract", IMAGE="ghcr.io/other/image", DIGEST=digest), ship.EXIT_REFUSED)

    def test_trivy_db_stage(self) -> None:
        self.write_scan("web", support.NEW["web"])
        self.assertEqual(self.stage("trivy-db"), ship.EXIT_OK, self.h.text())
        record = json.loads((self.scan / "trivy-db.json").read_text(encoding="ascii"))
        self.assertEqual(record["updated_at"], "2026-10-04T06:15:23Z")
        self.assertEqual(record["version"], "0.70.0")
        for stamp in ("2026-10-02T11:59:59Z", "2026-10-04T13:00:01Z", "yesterday"):
            (self.scan / "trivy-db.json").unlink()
            metadata = Path(self.h.env["RUNNER_TEMP"]) / "trivy-cache" / "db" / "metadata.json"
            metadata.write_text(json.dumps({"UpdatedAt": stamp}), encoding="ascii")
            with self.subTest(stamp=stamp):
                self.assertEqual(self.stage("trivy-db"), ship.EXIT_REFUSED)
                self.assertIn("image-contract-differs:trivy-db", self.h.text())
            (self.scan / "trivy-db.json").write_text("{}", encoding="ascii")

    def record_env(self, component: str, digest: str) -> dict[str, str]:
        return {"IMAGE": common.IMAGE_REPOSITORY[component], "DIGEST": digest, "TAG": f"ship-{self.head[:12]}-r{support.RUN_ID}"}

    def test_record_stage_writes_the_four_files(self) -> None:
        digest = support.NEW["meta-relay"]
        self.write_scan("meta-relay", digest)
        self.assertEqual(self.stage("trivy-db", "meta-relay"), ship.EXIT_OK)
        self.assertEqual(self.stage("record", "meta-relay", **self.record_env("meta-relay", digest)), ship.EXIT_OK, self.h.text())
        output = Path(self.h.env["RUNNER_TEMP"]) / "ship-image"
        self.assertEqual(sorted(path.name for path in output.iterdir()), sorted(ship.IMAGE_FILES))
        record = json.loads((output / "image.json").read_text(encoding="ascii"))
        self.assertEqual(record["digest"], digest)
        self.assertIs(record["tag_is_authority"], False)
        self.assertEqual(record["sbom_sha256"], hashlib.sha256((output / "sbom.spdx.json").read_bytes()).hexdigest())
        self.assertEqual(record["trivy"]["active_exceptions"], 0)
        self.assertEqual(record["built_at"], "2026-10-04T12:00:00Z")

    def test_record_stage_refusals(self) -> None:
        digest = support.NEW["web"]
        cases = {
            "high-vulnerability": dict(vulnerabilities=[{"VulnerabilityID": "CVE-2026-1", "Severity": "HIGH"}]),
            "secret": dict(secrets=[{"RuleID": "generic"}]),
            "sbom": dict(sbom={"spdxVersion": "SPDX-2.2", "documentNamespace": "x", "packages": [1]}),
        }
        for label, options in cases.items():
            with self.subTest(case=label):
                for name in ("trivy-db.json",):
                    (self.scan / name).unlink(missing_ok=True)
                self.write_scan("web", digest, **options)
                self.assertEqual(self.stage("trivy-db"), ship.EXIT_OK)
                self.assertEqual(self.stage("record", **self.record_env("web", digest)), ship.EXIT_REFUSED)
                self.assertIn("refused: artifact-invalid:reports", self.h.text())
        env = self.record_env("web", digest)
        env["TAG"] = "ship-000000000000-r1"
        self.assertEqual(self.stage("record", **env), ship.EXIT_REFUSED)
        self.assertIn("image-contract-differs:tag", self.h.text())


class CandidateStageTests(StageCase):
    def setUp(self) -> None:
        super().setUp()
        self.images_root = Path(self.h.env["RUNNER_TEMP"]) / "ship-images"
        for component in common.COMPONENTS:
            directory = self.images_root / f"ship-image-{component}-{support.RUN_ID}"
            directory.mkdir(parents=True)
            sbom = dict(SBOM, documentNamespace=f"https://example.invalid/spdx/{component}")
            raw = json.dumps(sbom).encode("ascii")
            (directory / "sbom.spdx.json").write_bytes(raw)
            (directory / "vulnerability-report.json").write_text('{"Results":[]}', encoding="ascii")
            (directory / "secret-report.json").write_text('{"Results":[]}', encoding="ascii")
            built = {"web": "2026-10-04T11:10:00Z", "meta-relay": "2026-10-04T11:05:00Z", "gmail-relay": "2026-10-04T11:20:00Z"}
            record = {
                "schema_version": 1, "component": component, "image": common.IMAGE_REPOSITORY[component],
                "digest": support.NEW[component], "sha": self.head, "tag": f"ship-{self.head[:12]}-r{support.RUN_ID}",
                "tag_is_authority": False, "dockerfile": ship.DOCKERFILES[component], "dockerfile_sha256": "a" * 64,
                "built_at": built[component], "sbom_sha256": hashlib.sha256(raw).hexdigest(),
                "trivy": {"version": "0.70.0", "db_updated_at": "2026-10-04T06:00:00Z", "db_metadata_sha256": "b" * 64,
                          "active_exceptions": 0},
            }
            (directory / "image.json").write_bytes(common.canonical_file_bytes(record))
            self.h.gh.attest_ship_images({component: support.NEW[component]}, self.head, sbom=sbom)
            for kind in ("PROV", "SBOM"):
                bundle = self.root / f"{kind}-{component}.jsonl"
                bundle.write_text("{}", encoding="ascii")
                self.h.env[f"SHIP_{kind}_BUNDLE_{ship.STAGE_SUFFIX[component]}"] = str(bundle)

    def candidate(self, stage: str) -> int:
        return self.h.run("candidate", "--stage", stage)

    def test_read_outputs_the_three_digests(self) -> None:
        self.assertEqual(self.candidate("read"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.outputs(), {"web": support.NEW["web"], "meta_relay": support.NEW["meta-relay"],
                                            "gmail_relay": support.NEW["gmail-relay"]})

    def test_inventory_is_exact(self) -> None:
        extra = self.images_root / f"ship-image-web-{support.RUN_ID}" / "extra.txt"
        extra.write_text("x", encoding="ascii")
        self.assertEqual(self.candidate("read"), ship.EXIT_REFUSED)
        self.assertIn("refused: artifact-invalid:inventory", self.h.text())
        extra.unlink()
        (self.images_root / "ship-image-web-9999").mkdir()
        self.assertEqual(self.candidate("read"), ship.EXIT_REFUSED)

    def test_record_binding(self) -> None:
        path = self.images_root / f"ship-image-gmail-relay-{support.RUN_ID}" / "sbom.spdx.json"
        path.write_text(json.dumps(SBOM) + " ", encoding="ascii")
        self.assertEqual(self.candidate("read"), ship.EXIT_REFUSED)
        self.assertIn("refused: artifact-invalid:sbom", self.h.text())

    def test_verify_checks_bundles_sbom_and_api_lookup(self) -> None:
        self.assertEqual(self.candidate("verify"), ship.EXIT_OK, self.h.text())
        verify = [argv for argv, _ in self.h.gh.calls if argv[1:3] == ["attestation", "verify"]]
        self.assertEqual(len(verify), 9)
        self.assertEqual(sum("--bundle" in argv for argv in verify), 6)
        for argv in verify:
            self.assertIn(f"https://github.com/{common.REPOSITORY}/.github/workflows/ship.yml@refs/heads/main", argv)
            self.assertNotIn("--signer-workflow", argv)
            self.assertEqual(argv[argv.index("--signer-digest") + 1], self.head)
            self.assertEqual(argv[argv.index("--source-digest") + 1], self.head)

    def test_verify_refuses_a_different_sbom_predicate(self) -> None:
        for entries in self.h.gh.image_attestations.values():
            entries[1]["predicate"] = {"spdxVersion": "SPDX-2.3", "packages": []}
        self.assertEqual(self.candidate("verify"), ship.EXIT_REFUSED)
        self.assertIn("refused: attestation-unverified:sbom", self.h.text())

    def test_verify_requires_bundle_paths(self) -> None:
        self.h.env.pop("SHIP_SBOM_BUNDLE_WEB")
        self.assertEqual(self.candidate("verify"), ship.EXIT_REFUSED)
        self.assertIn("refused: artifact-invalid:bundle", self.h.text())

    def test_assemble_writes_a_canonical_candidate(self) -> None:
        self.assertEqual(self.candidate("assemble"), ship.EXIT_OK, self.h.text())
        outputs = self.h.outputs()
        raw = base64.b64decode(outputs["candidate_b64"])
        self.assertEqual(hashlib.sha256(raw).hexdigest(), outputs["candidate_sha256"])
        candidate = json.loads(raw)
        self.assertEqual(raw, common.canonical_file_bytes(candidate))
        self.assertEqual(candidate["built_at"], "2026-10-04T11:05:00Z")
        self.assertEqual(candidate["images"], support.NEW)
        self.assertIn("attestations verified (signer ship.yml, ref main)", self.h.summary())
        control = {"sha": self.head, "workflow_sha": self.head}
        ship.decode_candidate({"CANDIDATE_B64": outputs["candidate_b64"], "CANDIDATE_SHA256": outputs["candidate_sha256"]},
                              control, support.NOW)


class RecordStageTests(StageCase):
    def setUp(self) -> None:
        super().setUp()
        self.manifest = support.manifest(kind="promote", sha=self.head, images=support.NEW, previous="prod-0000",
                                         deployed_at="2026-10-04T12:00:00Z", built_at="2026-10-04T11:00:00Z")
        values = {
            "RELEASE": self.manifest["release"], "KIND": "promote", "SHA": self.head,
            "WEB_DIGEST": support.NEW["web"], "META_RELAY_DIGEST": support.NEW["meta-relay"],
            "GMAIL_RELAY_DIGEST": support.NEW["gmail-relay"], "PREVIOUS": "prod-0000", "ROLLED_BACK_FROM": "",
            "DEPLOYED_AT": "2026-10-04T12:00:00Z", "BUILT_AT": "2026-10-04T11:00:00Z", "RECONCILED": "false",
            "ENVIRONMENT_VALUES_SHA256": "e" * 64, "NON_SOURCE_PROJECTION_SHA256": "d" * 64,
        }
        self.h.env.update({f"REC_{key}": value for key, value in values.items()})
        self.h.env["GITHUB_RUN_ATTEMPT"] = "2"
        self.raw = common.canonical_file_bytes(self.manifest)

    def record(self, stage: str) -> int:
        return self.h.run("record", "--stage", stage)

    def attest_local(self) -> None:
        self.h.gh.attested_files[hashlib.sha256(self.raw).hexdigest()] = self.head

    def test_new_record_full_cycle(self) -> None:
        self.assertEqual(self.record("manifest"), ship.EXIT_OK, self.h.text())
        outputs = self.h.outputs()
        self.assertEqual((outputs["tag"], outputs["state"]), (self.manifest["release"], "new"))
        path = Path(outputs["path"])
        self.assertEqual(path.read_bytes(), self.raw)
        self.attest_local()
        self.assertEqual(self.record("publish"), ship.EXIT_OK, self.h.text())
        self.assertEqual([write[:2] for write in self.h.gh.writes], [["release", "create"], ["release", "upload"], ["release", "edit"]])
        create = self.h.gh.writes[0]
        self.assertIn("--draft", create)
        self.assertEqual(create[create.index("--target") + 1], self.head)
        self.assertNotIn("--clobber", self.h.gh.writes[1])
        self.assertEqual(self.record("verify"), ship.EXIT_OK, self.h.text())
        notes = (path.parent / "notes.md").read_text(encoding="ascii")
        self.assertEqual(support.scan_private(notes), [])
        self.assertIn(self.manifest["release"], notes)
        self.assertNotIn("http", notes.split("Written by")[0])
        self.assert_clean_output()

    def test_draft_state_uploads_with_clobber(self) -> None:
        self.h.gh.releases.append({"id": 5, "tag_name": self.manifest["release"], "draft": True, "prerelease": False,
                                   "target_commitish": self.head, "assets": []})
        self.assertEqual(self.record("manifest"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.outputs()["state"], "draft")
        self.attest_local()
        self.assertEqual(self.record("publish"), ship.EXIT_OK, self.h.text())
        self.assertEqual([write[:2] for write in self.h.gh.writes], [["release", "upload"], ["release", "edit"]])
        self.assertIn("--clobber", self.h.gh.writes[0])
        self.assertEqual(self.record("verify"), ship.EXIT_OK, self.h.text())

    def test_published_equal_is_a_no_op(self) -> None:
        self.h.gh.add_record(self.manifest)
        self.assertEqual(self.record("manifest"), ship.EXIT_OK, self.h.text())
        self.assertEqual(self.h.outputs()["state"], "published")
        self.assertEqual(self.record("publish"), ship.EXIT_OK)
        self.assertEqual(self.h.gh.writes, [])
        self.assertEqual(self.record("verify"), ship.EXIT_OK, self.h.text())

    def test_published_different_fails(self) -> None:
        other = dict(self.manifest, fingerprints={"environment_values_sha256": "1" * 64, "non_source_projection_sha256": "d" * 64})
        self.h.gh.add_record(other)
        self.assertEqual(self.record("manifest"), ship.EXIT_REFUSED)
        self.assertIn("refused: record-invalid:published-differs", self.h.text())

    def test_latest_moved_fails(self) -> None:
        moved = support.manifest(kind="promote", sha=self.head, images=support.OTHER, previous="prod-0000",
                                 deployed_at="2026-10-04T11:30:00Z")
        self.h.gh.add_record(moved)
        self.assertEqual(self.record("manifest"), ship.EXIT_REFUSED)
        self.assertIn("refused: latest-changed-since-production", self.h.text())

    def test_record_must_extend_the_chain(self) -> None:
        self.h.env["REC_DEPLOYED_AT"] = "2026-10-01T12:00:00Z"
        self.h.env["REC_RELEASE"] = f"prod-20261001T120000Z-{self.head[:8]}"
        self.h.env["REC_BUILT_AT"] = "2026-10-01T11:00:00Z"
        self.assertEqual(self.record("manifest"), ship.EXIT_REFUSED)
        self.assertIn("record-chain-invalid:link", self.h.text())

    def test_record_inputs_are_validated(self) -> None:
        for name, value in (("WEB_DIGEST", "sha256:x"), ("KIND", "deploy"), ("RECONCILED", "yes"),
                            ("RELEASE", "prod-20261004T120000Z-00000000"), ("SHA", self.base), ("BUILT_AT", "")):
            with self.subTest(field=name):
                original = self.h.env[f"REC_{name}"]
                self.h.env[f"REC_{name}"] = value
                self.assertEqual(self.record("manifest"), ship.EXIT_REFUSED)
                self.assertIn("refused: record-invalid:inputs", self.h.text())
                self.h.env[f"REC_{name}"] = original

    def test_verify_requires_the_attestation(self) -> None:
        self.assertEqual(self.record("manifest"), ship.EXIT_OK)
        self.assertEqual(self.record("publish"), ship.EXIT_OK)
        self.assertEqual(self.record("verify"), ship.EXIT_REFUSED)
        self.assertIn("attestation-unverified", self.h.text())
        self.assertEqual(len(self.h.sleeps), 5)


class TrivyPolicyStageTests(StageCase):
    def test_trivy_policy_stage(self) -> None:
        self.assertEqual(self.h.run("trivy-policy"), ship.EXIT_OK)
        self.assertIn("0 active", self.h.text())


if __name__ == "__main__":
    unittest.main()
