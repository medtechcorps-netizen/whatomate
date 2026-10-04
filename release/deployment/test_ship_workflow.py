"""Semantic tests for .github/workflows/ship.yml and the Stage 1 files.

The workflow is parsed by a small YAML-subset reader (no PyYAML on the
runner); when PyYAML happens to be importable the parse is cross-checked.
"""

from __future__ import annotations

import ast
import json
import re
import sys
import unittest
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))
import do_app
import ship
import ship_common as common


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
WORKFLOW_PATH = ROOT / ".github" / "workflows" / "ship.yml"
OLD_GROUP = "rereply" + "-production"
OLD_GROUP_LINE = "group: " + OLD_GROUP
FORBIDDEN_FRAGMENT = "live" + "-evidence"
ALLOWED_ACTIONS = {
    "actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4",
    "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4",
    "actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4",
    "docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8 # v6",
    "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6 # v4",
}
# The Release workflow, its two runbooks and every file in this directory:
# the retired release tooling is gone, so whatever lands here is held to the
# same rules without editing a list.
NEW_FILES = (
    ".github/workflows/ship.yml",
    *sorted(f"release/deployment/{path.name}" for path in HERE.iterdir() if path.is_file()),
    "docs/release.md",
    "docs/emergency-rollback.md",
)
# The release modules the four-phase machinery used, retired with it. None may
# come back, and no Stage 1 file may import one.
RETIRED_MODULES = frozenset({
    "apply_production_change",
    "authorize_production_main_lock_release",
    "bootstrap_production_crm_canary_driver",
    "cleanup_production_crm_canary_fixture",
    "confirm_production_orphan_lock_release",
    "finalize_production_orphan_lock",
    "inverse_production_crm_canary_fixture",
    "launch_production_prerequisites",
    "observe_crm_fixture_prestate",
    "observe_production_recovery",
    "provider_native_valkey_recovery",
    "provision_production_crm_canary_fixture",
    "reconcile_crm_fixture_readonly",
    "reconcile_production_main_lock_release",
    "reconcile_production_orphan",
    "reconcile_production_orphan_lock_release",
    "recover_production_crm_canary_driver",
    "repair_crm_canary_driver_logins",
    "rollback_production_change",
    "run_existing_crm_canary_driver_recovery",
    "sanitized_provider_parity",
    "verify_crm_canary_fixture_binding",
    "verify_production_crm_canary",
    "verify_production_plan",
    "verify_production_release",
    "verify_rollout_evidence",
})


# --------------------------------------------------------------------------
# A YAML-subset reader for the formatting contract of ship.yml.
# --------------------------------------------------------------------------


KEY_RE = re.compile(r"^([A-Za-z0-9_.-]+):(?:[ ]+(.*))?$")


class MiniYaml:
    def __init__(self, text: str) -> None:
        self.lines = text.split("\n")
        self.index = 0

    @staticmethod
    def indent(line: str) -> int:
        return len(line) - len(line.lstrip(" "))

    def skip(self) -> None:
        while self.index < len(self.lines):
            stripped = self.lines[self.index].strip()
            if stripped and not stripped.startswith("#"):
                return
            self.index += 1

    def parse(self) -> Any:
        self.skip()
        value = self.block(0)
        self.skip()
        if self.index != len(self.lines):
            raise ValueError(f"unparsed content at line {self.index + 1}")
        return value

    def block(self, indent: int) -> Any:
        self.skip()
        line = self.lines[self.index]
        if self.indent(line) != indent:
            raise ValueError(f"bad indentation at line {self.index + 1}")
        if line.strip().startswith("- "):
            return self.sequence(indent)
        return self.mapping(indent)

    def mapping(self, indent: int) -> dict[str, Any]:
        result: dict[str, Any] = {}
        while True:
            self.skip()
            if self.index >= len(self.lines):
                return result
            line = self.lines[self.index]
            current = self.indent(line)
            if current < indent:
                return result
            if current > indent or line.strip().startswith("- "):
                raise ValueError(f"bad mapping entry at line {self.index + 1}")
            match = KEY_RE.match(line.strip())
            if match is None:
                raise ValueError(f"not a key at line {self.index + 1}")
            key, rest = match.group(1), match.group(2) or ""
            if key in result:
                raise ValueError(f"duplicate key {key}")
            self.index += 1
            result[key] = self.value(rest, indent)

    def value(self, rest: str, indent: int) -> Any:
        if rest in {"|", "|-"}:
            return self.literal(indent, keep_last=rest == "|")
        if rest.startswith(("&", "*", "!")) or rest in {">", ">-"}:
            raise ValueError("anchors, aliases, tags and folded scalars are outside the contract")
        if rest:
            return scalar(rest)
        self.skip()
        if self.index < len(self.lines) and self.indent(self.lines[self.index]) > indent:
            return self.block(self.indent(self.lines[self.index]))
        return None

    def literal(self, indent: int, *, keep_last: bool) -> str:
        collected: list[str] = []
        block_indent: int | None = None
        while self.index < len(self.lines):
            line = self.lines[self.index]
            if line.strip() == "":
                collected.append("")
                self.index += 1
                continue
            current = self.indent(line)
            if current <= indent:
                break
            if block_indent is None:
                block_indent = current
            if current < block_indent:
                raise ValueError("literal block indentation shrank")
            collected.append(line[block_indent:])
            self.index += 1
        while collected and collected[-1] == "":
            collected.pop()
        text = "\n".join(collected)
        return text + "\n" if keep_last and text else text

    def sequence(self, indent: int) -> list[Any]:
        result: list[Any] = []
        while True:
            self.skip()
            if self.index >= len(self.lines):
                return result
            line = self.lines[self.index]
            current = self.indent(line)
            if current < indent:
                return result
            if current != indent or not line.strip().startswith("- "):
                if current == indent:
                    return result
                raise ValueError(f"bad sequence entry at line {self.index + 1}")
            item = line.strip()[2:]
            if KEY_RE.match(item):
                self.lines[self.index] = " " * (indent + 2) + item
                result.append(self.mapping(indent + 2))
            else:
                self.index += 1
                result.append(scalar(item))


def scalar(raw: str) -> Any:
    raw = raw.strip()
    if raw.startswith('"'):
        end = raw.index('"', 1)
        rest = raw[end + 1:].strip()
        if rest and not rest.startswith("#"):
            raise ValueError("text after a quoted scalar")
        return json.loads(raw[: end + 1])
    if raw.startswith("'"):
        end = raw.index("'", 1)
        return raw[1:end]
    if raw == "{}":
        return {}
    if raw.startswith("[") and raw.endswith("]"):
        return [scalar(item) for item in raw[1:-1].split(",") if item.strip()]
    plain = re.split(r"\s+#", raw, maxsplit=1)[0].strip()
    if plain in {"true", "false"}:
        return plain == "true"
    if plain in {"null", "~"}:
        return None
    if re.fullmatch(r"-?[0-9]+", plain):
        return int(plain)
    return plain


SOURCE = WORKFLOW_PATH.read_text(encoding="utf-8")
DOC = MiniYaml(SOURCE).parse()
JOBS = DOC["jobs"]


def steps(job: str) -> list[dict[str, Any]]:
    return JOBS[job]["steps"]


def step(job: str, name: str) -> dict[str, Any]:
    matches = [item for item in steps(job) if item.get("name") == name]
    if len(matches) != 1:
        raise AssertionError(f"step {name!r} is not unique in {job}")
    return matches[0]


def all_steps() -> list[tuple[str, dict[str, Any]]]:
    return [(job, item) for job in JOBS for item in steps(job)]


def walk_keys(value: Any) -> list[str]:
    keys: list[str] = []
    if type(value) is dict:
        for key, child in value.items():
            keys.append(key)
            keys.extend(walk_keys(child))
    elif type(value) is list:
        for child in value:
            keys.extend(walk_keys(child))
    return keys


class ParserTests(unittest.TestCase):
    def test_parser_handles_the_contract_and_refuses_anchors(self) -> None:
        self.assertEqual(MiniYaml("a:\n  b: 1\n  c:\n    - x\n    - d: |\n        line\n").parse(),
                         {"a": {"b": 1, "c": ["x", {"d": "line\n"}]}})
        with self.assertRaises(ValueError):
            MiniYaml("a: &x 1\nb: *x\n").parse()
        with self.assertRaises(ValueError):
            MiniYaml("a: 1\na: 2\n").parse()

    def test_optional_pyyaml_cross_check(self) -> None:
        try:
            import yaml  # type: ignore[import-not-found]
        except ImportError:
            self.skipTest("PyYAML is not installed")
        loaded = yaml.safe_load(SOURCE)
        if True in loaded:
            loaded["on"] = loaded.pop(True)
        self.assertEqual(loaded, DOC)


class TriggerAndTopLevelTests(unittest.TestCase):
    def test_only_workflow_dispatch_with_exact_inputs(self) -> None:
        self.assertEqual(DOC["name"], "Release")
        self.assertEqual(DOC["run-name"], "Release ${{ inputs.mode }} ${{ inputs.target_release }}")
        self.assertEqual(set(DOC["on"]), {"workflow_dispatch"})
        inputs = DOC["on"]["workflow_dispatch"]["inputs"]
        self.assertEqual(set(inputs), {"mode", "target_release"})
        self.assertEqual({key: inputs["mode"][key] for key in ("required", "type", "default", "options")},
                         {"required": True, "type": "choice", "default": "dry-run", "options": ["dry-run", "promote", "rollback"]})
        self.assertEqual({key: inputs["target_release"][key] for key in ("required", "type", "default")},
                         {"required": False, "type": "string", "default": ""})
        for trigger in ("push", "pull_request", "pull_request_target", "schedule", "workflow_run", "workflow_call"):
            self.assertNotRegex(SOURCE, rf"(?m)^  {trigger}:")

    def test_top_level_permissions_concurrency_defaults(self) -> None:
        self.assertEqual(DOC["permissions"], {})
        self.assertEqual(DOC["concurrency"], {"group": "ship-release", "cancel-in-progress": False})
        self.assertEqual(DOC["defaults"], {"run": {"shell": "bash"}})
        self.assertRegex(SOURCE, r"(?m)^concurrency:\n  group: ship-release\n")

    def test_old_suite_traps_are_avoided(self) -> None:
        self.assertNotIn(OLD_GROUP, SOURCE)
        for relative in NEW_FILES:
            text = (ROOT / relative).read_text(encoding="utf-8")
            with self.subTest(path=relative):
                self.assertNotIn(OLD_GROUP_LINE, text)
                self.assertNotIn(FORBIDDEN_FRAGMENT, text)
        for name in (".trivyignore", ".trivyignore.yaml", "trivy.yaml", "trivy.yml"):
            self.assertFalse((ROOT / name).exists())

    def test_pinned_tools_are_exact(self) -> None:
        pinned = {key: value for key, value in DOC["env"].items() if key.startswith("PINNED_")}
        for tool in ("GH", "TRIVY", "SYFT", "BUILDX"):
            with self.subTest(tool=tool):
                version = pinned[f"PINNED_{tool}_VERSION"]
                self.assertRegex(version, r"^v?[0-9]+\.[0-9]+\.[0-9]+$")
                self.assertTrue(pinned[f"PINNED_{tool}_URL"].startswith("https://github.com/"))
                self.assertIn(version.removeprefix("v"), pinned[f"PINNED_{tool}_URL"])
                self.assertRegex(pinned[f"PINNED_{tool}_SHA256"], r"^[0-9a-f]{64}$")
        self.assertRegex(pinned["PINNED_BUILDKIT_IMAGE"], r"@sha256:[0-9a-f]{64}$")
        self.assertEqual(len(pinned), 13)
        self.assertNotIn("PINNED_TRIVY_DB", DOC["env"])
        self.assertNotIn("PINNED_JQ_URL", DOC["env"])
        self.assertEqual(DOC["env"]["SHIP_REPOSITORY"], common.REPOSITORY)
        self.assertEqual(DOC["env"]["SHIP_WORKFLOW_PATH"], common.SHIP_WORKFLOW_PATH)


class JobShapeTests(unittest.TestCase):
    def test_job_set_names_and_runners(self) -> None:
        self.assertEqual(list(JOBS), ["plan", "images", "attest", "production", "record"])
        required_contexts = {"test", "lint", "build", "security", "e2e", "tenant-isolation"}
        for name, job in JOBS.items():
            with self.subTest(job=name):
                self.assertEqual(job["runs-on"], "ubuntu-24.04")
                self.assertIs(type(job["timeout-minutes"]), int)
                self.assertNotIn(job["name"], required_contexts)
        self.assertEqual(JOBS["production"]["timeout-minutes"], ship.PRODUCTION_TIMEOUT_MINUTES)

    def test_production_timeout_covers_the_worst_case_deploy_and_rollback(self) -> None:
        budget = ship.production_worst_case_seconds()
        # At least ten minutes of margin over the computed bound.
        self.assertGreaterEqual(ship.PRODUCTION_TIMEOUT_MINUTES * 60, budget + 600)
        self.assertLessEqual(ship.PRODUCTION_TIMEOUT_MINUTES, 360)
        # The bound really covers both reconciles, both settles and both smokes.
        self.assertGreater(budget, 2 * (do_app.RECONCILE_DEADLINE_SECONDS + do_app.SETTLE_DEADLINE_SECONDS))
        self.assertGreaterEqual(do_app.RECONCILE_DEADLINE_SECONDS, do_app.POLL_LIMIT * do_app.POLL_SECONDS)

    def test_least_privilege_permissions(self) -> None:
        expected = {
            "plan": {"actions": "read", "attestations": "read", "contents": "read"},
            "images": {"contents": "read", "packages": "write"},
            "attest": {"attestations": "write", "contents": "read", "id-token": "write"},
            "production": {"actions": "read", "attestations": "read", "contents": "read"},
            "record": {"attestations": "write", "contents": "write", "id-token": "write"},
        }
        for name, permissions in expected.items():
            with self.subTest(job=name):
                self.assertEqual(JOBS[name]["permissions"], permissions)
        self.assertNotIn("deployments: write", SOURCE)

    def test_needs_and_conditions(self) -> None:
        self.assertNotIn("needs", JOBS["plan"])
        self.assertNotIn("if", JOBS["plan"])
        self.assertEqual(JOBS["images"]["needs"], "plan")
        self.assertEqual(JOBS["images"]["if"], "${{ inputs.mode != 'rollback' }}")
        self.assertEqual(JOBS["attest"]["needs"], ["plan", "images"])
        self.assertEqual(JOBS["attest"]["if"], "${{ inputs.mode != 'rollback' }}")
        self.assertEqual(JOBS["production"]["needs"], ["plan", "attest"])
        self.assertEqual(
            JOBS["production"]["if"],
            "${{ !cancelled() && github.ref == 'refs/heads/main' && needs.plan.result == 'success' && "
            "((inputs.mode == 'rollback' && needs.attest.result == 'skipped') || "
            "(inputs.mode != 'rollback' && needs.attest.result == 'success')) }}",
        )
        self.assertEqual(JOBS["record"]["needs"], ["production"])
        self.assertEqual(JOBS["record"]["if"],
                         "${{ !cancelled() && needs.production.result == 'success' && inputs.mode != 'dry-run' }}")

    def test_environment_and_job_concurrency_only_on_production(self) -> None:
        for name, job in JOBS.items():
            with self.subTest(job=name):
                if name == "production":
                    self.assertEqual(job["environment"], "production")
                    self.assertEqual(job["concurrency"], {"group": "ship-production", "cancel-in-progress": False})
                else:
                    self.assertNotIn("environment", job)
                    self.assertNotIn("concurrency", job)
        self.assertEqual(SOURCE.count("environment: production"), 1)

    def test_exactly_two_secrets_both_in_the_production_ship_step(self) -> None:
        self.assertEqual(len(re.findall(r"secrets\.", SOURCE)), 2)
        self.assertNotIn("secrets: inherit", SOURCE)
        env = step("production", "Verify, guard, deploy, smoke and roll back")["env"]
        self.assertEqual(env["SHIP_DO_TOKEN"], "${{ secrets.DO_PRODUCTION_DEPLOY_TOKEN }}")
        self.assertEqual(env["SHIP_TARGET_JSON"], "${{ secrets.PRODUCTION_TARGET_JSON }}")

    def test_attempt_guard_in_every_job_but_record(self) -> None:
        for name in ("plan", "images", "attest", "production"):
            with self.subTest(job=name):
                first = steps(name)[0]
                self.assertEqual(first["name"], "Refuse re-runs")
                self.assertEqual(first["env"], {"RUN_ATTEMPT": "${{ github.run_attempt }}",
                                                "RUNNER_ENVIRONMENT_NAME": "${{ runner.environment }}"})
                self.assertIn('[[ "$RUN_ATTEMPT" == "1" ]]', first["run"])
                self.assertIn('[[ "$GITHUB_REF" == "refs/heads/main" ]]', first["run"])
                self.assertIn('[[ "$RUNNER_ENVIRONMENT_NAME" == "github-hosted" ]]', first["run"])
        self.assertIn('[[ "${RUNNER_DEBUG:-}" != "1" ]]', steps("production")[0]["run"])
        self.assertNotIn("Refuse re-runs", [item["name"] for item in steps("record")])

    def test_outputs(self) -> None:
        self.assertEqual(set(JOBS["plan"]["outputs"]), {"latest_release", "latest_manifest_sha256", "latest_sha",
                                                         "target_release", "target_manifest_sha256"})
        self.assertEqual(set(JOBS["attest"]["outputs"]), {"candidate_b64", "candidate_sha256"})
        self.assertEqual(set(JOBS["production"]["outputs"]), {
            "release", "kind", "sha", "web_digest", "meta_relay_digest", "gmail_relay_digest", "previous",
            "rolled_back_from", "deployed_at", "built_at", "reconciled", "environment_values_sha256",
            "non_source_projection_sha256",
        })
        for key, value in JOBS["production"]["outputs"].items():
            self.assertEqual(value, f"${{{{ steps.ship.outputs.{key} }}}}")
        env = step("record", "Build the release manifest")["env"]
        for key in JOBS["production"]["outputs"]:
            self.assertEqual(env[f"REC_{key.upper()}"], f"${{{{ needs.production.outputs.{key} }}}}")


class StepContentTests(unittest.TestCase):
    def test_every_uses_is_an_allowlisted_pin(self) -> None:
        uses = re.findall(r"(?m)^\s+uses: (.+)$", SOURCE)
        self.assertTrue(uses)
        for value in uses:
            with self.subTest(uses=value):
                self.assertIn(value.strip(), ALLOWED_ACTIONS)
                self.assertRegex(value, r"@[0-9a-f]{40} # v[0-9]+$")

    def test_production_job_has_no_uses_and_only_reviewed_tools(self) -> None:
        forbidden = re.compile(r"\b(?:npm|npx|node|pip|pip3|playwright|docker|jq|yarn|pnpm|corepack|awk|sed|wget)\b")
        for item in steps("production"):
            with self.subTest(step=item["name"]):
                self.assertNotIn("uses", item)
                self.assertIsNone(forbidden.search(item["run"]))
                for line in item["run"].splitlines():
                    if "python" in line:
                        self.assertEqual(line.strip(), "/usr/bin/python3 -I -S -B release/deployment/ship.py production")
        ship_step = step("production", "Verify, guard, deploy, smoke and roll back")
        self.assertEqual(ship_step["working-directory"], "src")
        self.assertEqual(ship_step["id"], "ship")
        fetch = step("production", "Fetch the dispatched commit without credentials")
        self.assertIn("--filter=blob:none --no-checkout --single-branch --branch main --no-tags", fetch["run"])
        self.assertEqual(fetch["env"], {"GIT_TERMINAL_PROMPT": "0"})

    def test_run_bodies_contain_no_expressions_and_no_debug_escape_hatches(self) -> None:
        for job, item in all_steps():
            if "run" not in item:
                continue
            with self.subTest(job=job, step=item["name"]):
                self.assertNotIn("${{", item["run"])
                self.assertNotRegex(item["run"], r"set -[a-z]*x")
                for fragment in ("ACTIONS_STEP_DEBUG", "--ignore-unfixed", "db-repository", "skip-dirs", "skip-files"):
                    self.assertNotIn(fragment, item["run"])
        self.assertNotIn("continue-on-error", walk_keys(DOC))
        self.assertNotIn("continue-on-error", SOURCE)

    def test_python_steps_run_isolated_ship_stages(self) -> None:
        pattern = re.compile(r"^/usr/bin/python3 -I -S -B release/deployment/ship\.py (plan|production|trivy-policy|"
                             r"image --stage (pins|contract|trivy-db|record) --component \"\$COMPONENT\"|"
                             r"candidate --stage (read|verify|assemble)|record --stage (manifest|publish|verify))$")
        for job, item in all_steps():
            for line in item.get("run", "").splitlines():
                if "python" in line:
                    with self.subTest(job=job, line=line):
                        self.assertRegex(line.strip(), pattern)

    def test_gh_install_is_identical_and_pinned(self) -> None:
        bodies = {job: step(job, "Install pinned GitHub CLI")["run"] for job in ("plan", "attest", "production", "record")}
        self.assertEqual(len(set(bodies.values())), 1)
        body = bodies["plan"]
        self.assertIn('sha256sum --check --strict', body)
        self.assertIn("printf 'SHIP_GH_BIN=%s\\n'", body)
        self.assertNotIn("GITHUB_PATH", body)

    def test_build_step_fields(self) -> None:
        build = step("images", "Build the exact AMD64 image without registry credentials")
        self.assertEqual(build["env"], {"DOCKER_BUILD_RECORD_UPLOAD": "false"})
        self.assertEqual(build["with"], {
            "context": ".",
            "file": "./docker/release/${{ matrix.component }}.Dockerfile",
            "platforms": "linux/amd64",
            "push": False,
            "load": True,
            "pull": True,
            "no-cache": True,
            "provenance": False,
            "sbom": False,
            "builder": "rereply-ship",
            "github-token": "",
            "tags": "${{ steps.identity.outputs.image_ref }}",
            "build-args": "TARGETOS=linux\nTARGETARCH=amd64\n",
            "labels": (
                "org.opencontainers.image.source=https://github.com/medtechcorps-netizen/whatomate\n"
                "org.opencontainers.image.revision=${{ github.sha }}\n"
                "org.opencontainers.image.version=ship-${{ github.sha }}\n"
                "org.opencontainers.image.title=rereply-release-${{ matrix.component }}\n"
                "io.rereply.release.workflow=ship\n"
            ),
        })
        self.assertEqual(JOBS["images"]["strategy"], {"fail-fast": True, "matrix": {"component": ["web", "meta-relay", "gmail-relay"]}})

    def test_trivy_steps(self) -> None:
        database = step("images", "Download a fresh Trivy database")["run"]
        self.assertIn("--download-db-only", database)
        self.assertNotIn("--db-repository", database)
        self.assertIn("ship.py image --stage trivy-db", database)
        secrets = step("images", "Fail on embedded secrets")["run"]
        self.assertIn("--scanners secret", secrets)
        self.assertNotIn("--ignorefile", secrets)
        vulnerabilities = step("images", "Fail on HIGH or CRITICAL vulnerabilities")["run"]
        for fragment in ("--scanners vuln", "--pkg-types os,library", "--severity HIGH,CRITICAL",
                         "--ignorefile release/deployment/ship.trivyignore", "--exit-code 1"):
            self.assertIn(fragment, vulnerabilities)
        names = [item["name"] for item in steps("images")]
        self.assertLess(names.index("Check reviewed vulnerability exceptions"), names.index("Fail on HIGH or CRITICAL vulnerabilities"))

    def test_attestation_steps(self) -> None:
        attest = [item for item in steps("attest") if item.get("uses", "").startswith("actions/attest@")]
        self.assertEqual(len(attest), 6)
        for component, key in (("web", "web"), ("meta-relay", "meta_relay"), ("gmail-relay", "gmail_relay")):
            provenance = step("attest", f"Attest {component} provenance")
            sbom = step("attest", f"Attest {component} SBOM")
            self.assertEqual(provenance["id"], f"prov-{component}")
            self.assertEqual(sbom["id"], f"sbom-{component}")
            for item in (provenance, sbom):
                self.assertEqual(item["with"]["subject-name"], f"ghcr.io/medtechcorps-netizen/rereply-release-{component}")
                self.assertEqual(item["with"]["subject-digest"], f"${{{{ steps.images.outputs.{key} }}}}")
                self.assertIs(item["with"]["push-to-registry"], False)
            self.assertNotIn("sbom-path", provenance["with"])
            self.assertEqual(sbom["with"]["sbom-path"],
                             f"${{{{ runner.temp }}}}/ship-images/ship-image-{component}-${{{{ github.run_id }}}}/sbom.spdx.json")
        verify = step("attest", "Verify every attestation")["env"]
        self.assertEqual(sum(key.startswith("SHIP_PROV_BUNDLE_") or key.startswith("SHIP_SBOM_BUNDLE_") for key in verify), 6)
        record = step("record", "Attest the release manifest")
        self.assertEqual(record["if"], "${{ steps.manifest.outputs.state != 'published' }}")
        self.assertEqual(record["with"], {"subject-path": "${{ steps.manifest.outputs.path }}", "push-to-registry": False})

    def test_anonymous_pull_uses_an_empty_docker_config(self) -> None:
        pull = step("attest", "Require anonymous pullability")
        self.assertEqual(pull["env"]["DOCKER_CONFIG"], "${{ runner.temp }}/anonymous-docker-config")
        self.assertIn("unset GH_TOKEN GITHUB_TOKEN CR_PAT REGISTRY_TOKEN DOCKER_AUTH_CONFIG", pull["run"])
        self.assertIn('[[ ! -e "$DOCKER_CONFIG/config.json" ]]', pull["run"])
        self.assertIn('[[ "$pulled" -eq 3 ]]', pull["run"])

    def test_artifacts(self) -> None:
        upload = step("images", "Upload the image record")
        self.assertEqual(upload["with"], {"name": "ship-image-${{ matrix.component }}-${{ github.run_id }}",
                                          "path": "${{ runner.temp }}/ship-image/", "if-no-files-found": "error",
                                          "retention-days": 7})
        download = step("attest", "Download the three image records")
        self.assertEqual(download["with"], {"pattern": "ship-image-*-${{ github.run_id }}",
                                            "path": "${{ runner.temp }}/ship-images", "merge-multiple": False})

    def test_checkouts_never_persist_credentials(self) -> None:
        for job, item in all_steps():
            if item.get("uses", "").startswith("actions/checkout@"):
                with self.subTest(job=job):
                    self.assertIs(item["with"]["persist-credentials"], False)
                    self.assertEqual(item["with"]["ref"], "${{ github.sha }}")
        self.assertEqual(step("plan", "Check out the dispatched commit")["with"]["fetch-depth"], 0)


class RepositoryFileTests(unittest.TestCase):
    def test_new_files_are_lf_ascii_and_json_is_canonical(self) -> None:
        for relative in NEW_FILES:
            raw = (ROOT / relative).read_bytes()
            with self.subTest(path=relative):
                self.assertNotIn(b"\r", raw)
                raw.decode("ascii")
                self.assertTrue(raw.endswith(b"\n"))
                self.assertFalse(raw.startswith(b"\xef\xbb\xbf"))
                if relative.endswith(".json"):
                    self.assertEqual(raw, common.canonical_file_bytes(json.loads(raw)))

    def test_no_new_module_imports_a_pre_existing_release_module(self) -> None:
        self.assertIn("release/deployment/ship.py", NEW_FILES)
        for name in sorted(RETIRED_MODULES):
            with self.subTest(retired=name):
                self.assertFalse((HERE / f"{name}.py").exists())
        for relative in NEW_FILES:
            if not relative.endswith(".py"):
                continue
            tree = ast.parse((ROOT / relative).read_text(encoding="utf-8"))
            imported: set[str] = set()
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    imported.update(alias.name.split(".")[0] for alias in node.names)
                elif isinstance(node, ast.ImportFrom) and node.module:
                    imported.add(node.module.split(".")[0])
            with self.subTest(path=relative):
                self.assertEqual(imported & RETIRED_MODULES, set())

    def test_new_files_carry_no_provider_identifiers(self) -> None:
        for relative in NEW_FILES:
            if relative.startswith("release/deployment/test_"):
                continue
            text = (ROOT / relative).read_text(encoding="utf-8")
            with self.subTest(path=relative):
                self.assertFalse("ondigitalocean" + ".app" in text.lower(), "App Platform hostname fragment")
                self.assertFalse(common.ANY_UUID_RE.search(text), "identifier-shaped value")

    def test_release_dockerfiles_are_pinned(self) -> None:
        for component in common.COMPONENTS:
            text = (ROOT / "docker" / "release" / f"{component}.Dockerfile").read_text(encoding="ascii")
            for line in text.splitlines():
                if line.startswith("FROM "):
                    with self.subTest(component=component, line=line[:60]):
                        self.assertTrue("@sha256:" in line or line.strip() == "FROM scratch")
                if line.startswith("ADD ") and "https://" in line:
                    with self.subTest(component=component, line=line[:60]):
                        self.assertRegex(line, r"--checksum=sha256:[0-9a-f]{64} ")

    def test_docs_state_the_owner_only_approval_rule(self) -> None:
        release = (ROOT / "docs" / "release.md").read_text(encoding="ascii")
        emergency = (ROOT / "docs" / "emergency-rollback.md").read_text(encoding="ascii")
        self.assertIn("Claude/Codex never approve", release)
        self.assertIn("Approve and deploy", release)
        for code in ("approval-gate-misconfigured", "approval-missing", "ci-not-green", "old-lane-active", "record-chain-invalid", "latest-changed-since-plan",
                     "downgrade-refused", "schema-change-blocked", "trivy-exception-invalid", "candidate-stale",
                     "attestation-unverified", "app-identity-mismatch", "vpc-missing-or-differs", "topology-differs",
                     "forbidden-image-field", "drift", "backup-stale", "cas-changed", "provider-rejected",
                     "deployment-error", "smoke-failed", "rollback-precondition-failed"):
            with self.subTest(code=code):
                self.assertIn(f"`{code}`", release)
                self.assertIn(code, common.REASONS)
        for fragment in ("Rollback", "Commit", "rls-migrate -rollback", "target_release", "`previous`",
                         "timed out", "nothing-to-roll-back", "drift:live-matches-record"):
            self.assertIn(fragment, emergency)
        self.assertIn("timed out", release)
        # The newest record is not automatically the known-good one.
        self.assertIn("It is not always the newest", " ".join(emergency.split()))


if __name__ == "__main__":
    unittest.main()
