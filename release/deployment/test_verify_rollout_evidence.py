from __future__ import annotations

import copy
import hashlib
import io
import json
import re
import stat
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_rollout_evidence as verifier


ROOT = Path(__file__).resolve().parents[2]
CONTRACT_PATH = ROOT / "release" / "deployment" / "exact-rollout-contract.json"
SCHEMA_PATH = ROOT / "release" / "deployment" / "rollout-plan.schema.json"
VERIFIER_PATH = ROOT / "release" / "deployment" / "verify_rollout_evidence.py"
MANIFEST_PATH = ROOT / "release" / "exact-sources.json"
WORKFLOW_PATH = ROOT / ".github" / "workflows" / "aggregate-exact-four-phase-rollout.yml"
VALIDATION_WORKFLOW_PATH = (
    ROOT / ".github" / "workflows" / "validate-exact-release-source.yml"
)
IMAGE_WORKFLOW_PATH = (
    ROOT / ".github" / "workflows" / "build-attest-exact-release-images.yml"
)
TEST_WORKFLOW_PATH = ROOT / ".github" / "workflows" / "test.yml"
CONTROL_SHA = "a" * 40
EXPECTED_FINAL_SOURCE = {
    "source_sha": "ff0c9c6b8d94a085af164e564028d25d38b0a02c",
    "root_tree": "6b3d030924913a562fee2b75fce318b01b421792",
    "frontend_tree": "f1cdd8186e2b724dc4bba41081578b2b003a6910",
    "internal_tree": "a1da97143c17f3d02e47250269d00f238ac0e38c",
}
STALE_FINAL_SOURCE = {
    "source_sha": "ab44af2e7c093b4502c1928126c31306b2ba0389",
    "root_tree": "3553b783d5cfdcbda2ffcee332a2aa392a897bc5",
    "frontend_tree": "2f92a064f2f3d7f1867c835ef3be7f41e2f30444",
    "internal_tree": "494d3957ff3559375f406886be45646049ec9378",
}


def validation_workflow_sources(workflow: str) -> dict[str, dict[str, str]]:
    """Parse only the stable, phase-local source selection case statement."""

    marker = 'case "$REQUESTED_PHASE" in'
    self_contained = workflow.split(marker, 1)[1].split("          esac", 1)[0]
    result: dict[str, dict[str, str]] = {}
    case_pattern = re.compile(
        r"(?ms)^\s{12}(baseline|bridge|backend|ui)\)\s*$"
        r"(?P<body>.*?)^\s{14};;\s*$"
    )
    assignment_pattern = re.compile(
        r'^\s{14}(source_sha|root_tree|frontend_tree|internal_tree)="([0-9a-f]{40})"\s*$',
        re.MULTILINE,
    )
    for match in case_pattern.finditer(self_contained):
        phase = match.group(1)
        values = dict(assignment_pattern.findall(match.group("body")))
        if set(values) != {"source_sha", "root_tree", "frontend_tree", "internal_tree"}:
            raise AssertionError(f"workflow source tuple is malformed for {phase}")
        result[phase] = values
    return result


def image_workflow_sources(workflow: str) -> dict[str, dict[str, str]]:
    """Parse the four localized phase objects from the jq authority gate."""

    result: dict[str, dict[str, str]] = {}
    for phase in ("baseline", "bridge", "backend", "ui"):
        matches = re.findall(
            rf"(?ms)\.phases\.{phase}\s*==\s*\{{(?P<body>.*?)^\s*\}}\s+and\s*$",
            workflow,
        )
        if len(matches) != 1:
            raise AssertionError(f"image workflow source object count differs for {phase}")
        values = dict(
            re.findall(
                r'(source_sha|root_tree|frontend_tree|internal_tree)\s*:\s*"([0-9a-f]{40})"',
                matches[0],
            )
        )
        if set(values) != {"source_sha", "root_tree", "frontend_tree", "internal_tree"}:
            raise AssertionError(f"image workflow source tuple is malformed for {phase}")
        result[phase] = values
    return result


def digest(label: str) -> str:
    return hashlib.sha256(label.encode("utf-8")).hexdigest()


class RolloutEvidenceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.contract = verifier.validate_contract(
            verifier.load_json(CONTRACT_PATH, "test contract")
        )
        self.manifest = verifier.validate_manifest(
            verifier.load_json(MANIFEST_PATH, "test manifest")
        )

    def valid_input_value(self) -> dict:
        return {
            "control_sha": CONTROL_SHA,
            "phases": [
                {
                    "phase": phase,
                    "image_run_id": str(1000 + index),
                    "image_run_attempt": 1,
                    "release_set_artifact_id": str(2000 + index),
                    "release_set_sha256": digest(f"placeholder-{phase}"),
                }
                for index, phase in enumerate(verifier.PHASES)
            ],
        }

    def test_contract_and_manifest_are_strictly_accepted(self) -> None:
        self.assertEqual(self.contract["phase_order"], verifier.PHASES)
        self.assertEqual(len(self.contract["image_workflow"]["expected_jobs"]), 16)
        self.assertEqual(set(self.manifest["phases"]), set(verifier.PHASES))
        self.assertEqual(
            self.contract["migration"]["arguments"],
            ["rls-migrate", "-config", "config.toml"],
        )
        self.assertEqual(
            self.contract["rollback"]["ui"],
            {"allowed_targets": ["backend", "bridge"], "forbidden_targets": ["baseline"]},
        )
        schema = verifier.validate_plan_schema(verifier.load_json(SCHEMA_PATH, "test schema"))
        self.assertEqual(
            schema["$defs"]["migration"]["properties"]["arguments"]["const"],
            ["rls-migrate", "-config", "config.toml"],
        )
        self.assertEqual(
            schema["$defs"]["artifact"]["properties"]["size_in_bytes"]["maximum"],
            verifier.MAX_ARCHIVE_FILE_BYTES,
        )

    def test_exact_source_authorities_share_the_reviewed_final_tuple(self) -> None:
        expected_keys = {"source_sha", "root_tree", "frontend_tree", "internal_tree"}
        manifest_phases = self.manifest["phases"]
        self.assertEqual(set(manifest_phases), set(verifier.PHASES))
        self.assertEqual(manifest_phases, verifier.EXPECTED_PHASE_SOURCES)
        for phase in verifier.PHASES:
            with self.subTest(phase=phase):
                self.assertEqual(set(manifest_phases[phase]), expected_keys)
                for value in manifest_phases[phase].values():
                    self.assertRegex(value, r"^[0-9a-f]{40}$")

        self.assertEqual(manifest_phases["ui"], EXPECTED_FINAL_SOURCE)

        validation_workflow = VALIDATION_WORKFLOW_PATH.read_text(encoding="utf-8")
        self.assertEqual(
            validation_workflow_sources(validation_workflow),
            manifest_phases,
        )

        self.assertEqual(
            image_workflow_sources(IMAGE_WORKFLOW_PATH.read_text(encoding="utf-8")),
            manifest_phases,
        )
        self.assertNotEqual(manifest_phases["ui"], STALE_FINAL_SOURCE)

    def test_ui_validation_harness_applies_only_the_reviewed_completion_patch(self) -> None:
        workflow = VALIDATION_WORKFLOW_PATH.read_text(encoding="utf-8")
        marker = 'case "$PHASE" in'
        harness_case = workflow.split(marker, 1)[1].split("          esac", 1)[0]
        ui_case = re.search(
            r"(?ms)^\s{12}ui\)\s*$(?P<body>.*?)^\s{14};;\s*$",
            harness_case,
        )
        self.assertIsNotNone(ui_case)
        body = ui_case.group("body")
        assignments = dict(
            re.findall(
                r'^\s{14}(expected_original_blob|expected_common_blob|apply_common_patch|completion_patch_relative_path|expected_patched_blob)="([^"]+)"\s*$',
                body,
                re.MULTILINE,
            )
        )
        self.assertEqual(
            assignments,
            {
                "expected_original_blob": "906e09c8ebf43d4ba23fcd3caf856175c6ecc332",
                "expected_common_blob": "906e09c8ebf43d4ba23fcd3caf856175c6ecc332",
                "apply_common_patch": "false",
                "completion_patch_relative_path": "$UI_COMPLETION_PATCH_RELATIVE_PATH",
                "expected_patched_blob": "f9ae2342e236274db62dc50e9acd19dba7e03f2f",
            },
        )
        self.assertIn('if [[ "$apply_common_patch" == "true" ]]; then', workflow)
        self.assertIn('[[ "$apply_common_patch" == "false" ]]', workflow)
        self.assertIn('"$completion_patch_path"', workflow)

    def test_ui_validation_harness_behavior_matches_the_reviewed_blobs(self) -> None:
        target_relative = Path("frontend/src/views/channels/ChannelsView.test.ts")
        common_relative = Path(
            "release/validation/patches/channels-view-webcrypto-waits.patch"
        )
        completion_relative = Path(
            "release/validation/patches/channels-view-webcrypto-completion-ui.patch"
        )

        def head_blob(path: Path) -> bytes:
            return subprocess.run(
                ["git", "-C", str(ROOT), "show", f"HEAD:{path.as_posix()}"],
                check=True,
                capture_output=True,
            ).stdout

        source_bytes = head_blob(target_relative)
        common_bytes = head_blob(common_relative)
        completion_bytes = head_blob(completion_relative)
        self.assertEqual(
            hashlib.sha256(common_bytes).hexdigest(),
            "917868ced113fe03b0f93d5a0d2acbb9c75021f666517c80af2092a2e2fc2ab1",
        )
        self.assertEqual(
            hashlib.sha256(completion_bytes).hexdigest(),
            "be427efad3c9b2deffdab3dbd51691cf8d79f212ecbdd3dc250449c271a00dd9",
        )

        with tempfile.TemporaryDirectory() as temporary:
            checkout = Path(temporary) / "source"
            target = checkout / target_relative
            target.parent.mkdir(parents=True)
            target.write_bytes(source_bytes)
            patch_root = Path(temporary) / "control"
            patch_root.mkdir()
            common_patch = patch_root / "common.patch"
            completion_patch = patch_root / "completion.patch"
            common_patch.write_bytes(common_bytes)
            completion_patch.write_bytes(completion_bytes)

            def git(*arguments: str, check: bool = True) -> subprocess.CompletedProcess[str]:
                return subprocess.run(
                    ["git", "-C", str(checkout), *arguments],
                    check=check,
                    capture_output=True,
                    text=True,
                )

            self.assertEqual(
                git("hash-object", target_relative.as_posix()).stdout.strip(),
                "906e09c8ebf43d4ba23fcd3caf856175c6ecc332",
            )
            common_check = git(
                "apply",
                "--unidiff-zero",
                "--check",
                "--whitespace=error-all",
                str(common_patch),
                check=False,
            )
            self.assertNotEqual(common_check.returncode, 0)
            git(
                "apply",
                "--unidiff-zero",
                "--check",
                "--whitespace=error-all",
                str(completion_patch),
            )
            git(
                "apply",
                "--unidiff-zero",
                "--whitespace=error-all",
                str(completion_patch),
            )
            self.assertEqual(
                git("hash-object", target_relative.as_posix()).stdout.strip(),
                "f9ae2342e236274db62dc50e9acd19dba7e03f2f",
            )
            self.assertEqual(
                [path.relative_to(checkout).as_posix() for path in checkout.rglob("*") if path.is_file()],
                [target_relative.as_posix()],
            )

    def test_workflow_remains_no_deploy_and_has_one_strict_input(self) -> None:
        workflow = WORKFLOW_PATH.read_text(encoding="utf-8")
        self.assertIn("phase_evidence_json:", workflow)
        self.assertEqual(workflow.count("      phase_evidence_json:"), 1)
        forbidden = [
            "secrets.",
            "packages: write",
            "docker login",
            "docker push",
            "doctl",
            "kubectl",
            "terraform apply",
            "gh release create",
        ]
        for token in forbidden:
            with self.subTest(token=token):
                self.assertNotIn(token, workflow)
        self.assertNotIn("1073741824", workflow)
        self.assertEqual(workflow.count("67108864"), 2)
        self.assertNotRegex(workflow, r"(?m)^\s+environment:\s*")
        self.assertNotRegex(workflow, r"(?m)^\s+runs-on:\s*.*self-hosted")
        self.assertNotRegex(workflow, r"(?m)^\s+uses:\s+[^\s]+@(main|master|v[0-9]+)\s*$")

        test_workflow = TEST_WORKFLOW_PATH.read_text(encoding="utf-8")
        self.assertNotIn("  release-control:", test_workflow)
        required_test_job = test_workflow.split("  test:\n", 1)[1].split("\n  lint:", 1)[0]
        self.assertIn("python3 -m unittest discover -s release/deployment", required_test_job)
        self.assertIn("python3 -m py_compile", required_test_job)
        self.assertIn("release/deployment/verify_rollout_evidence.py", required_test_job)

    def test_input_requires_exact_order_and_exact_keys(self) -> None:
        value = self.valid_input_value()
        normalized = verifier.normalize_input(json.dumps(value), CONTROL_SHA)
        self.assertEqual([item["phase"] for item in normalized["phases"]], verifier.PHASES)

        wrong_order = copy.deepcopy(value)
        wrong_order["phases"][0], wrong_order["phases"][1] = (
            wrong_order["phases"][1],
            wrong_order["phases"][0],
        )
        with self.assertRaises(verifier.EvidenceError):
            verifier.normalize_input(json.dumps(wrong_order), CONTROL_SHA)

        extra_key = copy.deepcopy(value)
        extra_key["deploy"] = True
        with self.assertRaises(verifier.EvidenceError):
            verifier.normalize_input(json.dumps(extra_key), CONTROL_SHA)

        boolean_attempt = copy.deepcopy(value)
        boolean_attempt["phases"][0]["image_run_attempt"] = True
        with self.assertRaises(verifier.EvidenceError):
            verifier.normalize_input(json.dumps(boolean_attempt), CONTROL_SHA)

    def test_strict_json_rejects_duplicates_nonfinite_numbers_and_all_floats(self) -> None:
        bad_documents = {
            "duplicate": '{"value":1,"value":2}',
            "nan": '{"value":NaN}',
            "positive-infinity": '{"value":Infinity}',
            "negative-infinity": '{"value":-Infinity}',
            "decimal": '{"value":1.0}',
            "exponent": '{"value":1e3}',
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for label, raw in bad_documents.items():
                with self.subTest(label=label):
                    path = root / f"{label}.json"
                    path.write_text(raw, encoding="utf-8")
                    with self.assertRaises(verifier.EvidenceError):
                        verifier.load_json(path, label)

        value = self.valid_input_value()
        raw = json.dumps(value)
        with self.assertRaises(verifier.EvidenceError):
            verifier.normalize_input(raw[:-1] + ',"control_sha":"' + CONTROL_SHA + '"}', CONTROL_SHA)
        with self.assertRaises(verifier.EvidenceError):
            verifier.normalize_input(raw.replace('"image_run_attempt": 1', '"image_run_attempt": 1.0', 1), CONTROL_SHA)
        for value in ({"value": 1.5}, {"value": float("nan")}, {"value": float("inf")}):
            with self.subTest(canonical=value):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.canonical_json_bytes(value)

    def test_exact_integer_fields_reject_booleans(self) -> None:
        contract = copy.deepcopy(self.contract)
        contract["schema_version"] = True
        with self.assertRaises(verifier.EvidenceError):
            verifier.validate_contract(contract)

        manifest = copy.deepcopy(self.manifest)
        manifest["schema_version"] = True
        with self.assertRaises(verifier.EvidenceError):
            verifier.validate_manifest(manifest)

        schema = verifier.validate_plan_schema(verifier.load_json(SCHEMA_PATH, "test schema"))
        with self.assertRaises(verifier.EvidenceError):
            verifier.validate_json_schema(True, schema["properties"]["schema_version"], schema)

    def test_manifest_rejects_unexpected_or_untyped_component_fields(self) -> None:
        mutations = [
            ("user-type", "user", 7),
            ("user-value", "user", "root"),
            ("working-dir", "working_dir", "/tmp"),
            ("entrypoint-type", "entrypoint", "./rereply"),
            ("entrypoint-value", "entrypoint", ["/bin/sh"]),
            ("command-type", "cmd", "server"),
            ("command-value", "cmd", ["server"]),
            ("port-type", "port", 8080),
            ("port-value", "port", "99999/tcp"),
            ("smoke-type", "smoke", 1),
            ("smoke-value", "smoke", "shell"),
        ]
        for label, field, value in mutations:
            with self.subTest(label=label):
                manifest = copy.deepcopy(self.manifest)
                manifest["release"]["components"]["web"][field] = value
                with self.assertRaises(verifier.EvidenceError):
                    verifier.validate_manifest(manifest)

        manifest = copy.deepcopy(self.manifest)
        manifest["release"]["components"]["web"]["unexpected"] = "forbidden"
        with self.assertRaises(verifier.EvidenceError):
            verifier.validate_manifest(manifest)

    def test_manifest_rejects_every_untyped_or_unreviewed_material_shape(self) -> None:
        def cases() -> list[tuple[str, dict]]:
            result: list[tuple[str, dict]] = []
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["unexpected"] = {}
            result.append(("extra-material", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["ubuntu_snapshot"]["id"] = 20260824
            result.append(("snapshot-id-type", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["ubuntu_snapshot"]["packages"]["ffmpeg"] = 7
            result.append(("package-version-type", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["ubuntu_snapshot"]["packages"]["ffmpeg"] = "7.0"
            result.append(("package-version-unreviewed", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["base_images"][0]["uses"] = "web"
            result.append(("base-uses-type", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["base_images"][0]["digest"] = "sha256:" + "f" * 64
            result.append(("base-digest-unreviewed", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["scratch_stages"][0]["stage"] = "builder"
            result.append(("scratch-stage-unreviewed", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["direct_downloads"][0]["url"] = 7
            result.append(("download-url-type", manifest))
            manifest = copy.deepcopy(self.manifest)
            manifest["release"]["materials"]["direct_downloads"][0]["runtime_mode"] = "0777"
            result.append(("download-mode-unreviewed", manifest))
            return result

        for label, manifest in cases():
            with self.subTest(label=label):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.validate_manifest(manifest)

        with tempfile.TemporaryDirectory() as temporary:
            bad_manifest = Path(temporary) / "manifest.json"
            bad_manifest.write_text(
                MANIFEST_PATH.read_text(encoding="utf-8").replace('"20260824T000000Z"', "NaN", 1),
                encoding="utf-8",
            )
            with self.assertRaises(verifier.EvidenceError):
                verifier.validate_manifest(verifier.load_json(bad_manifest, "NaN manifest"))

    def test_archive_inventory_and_safe_extraction(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "artifact.zip"
            with zipfile.ZipFile(archive, "w") as bundle:
                for name in verifier.ARTIFACT_INVENTORIES["image"]:
                    bundle.writestr(name, b"{}\n")
            output = root / "out"
            inventory = verifier.inspect_archive(archive, "image", self.contract, output)
            self.assertEqual(inventory, verifier.ARTIFACT_INVENTORIES["image"])
            self.assertEqual(sorted(path.name for path in output.iterdir()), inventory)

    def test_archive_rejects_traversal_and_symlink_members(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            traversal = root / "traversal.zip"
            with zipfile.ZipFile(traversal, "w") as bundle:
                bundle.writestr("../image.json", b"{}")
                bundle.writestr("remote-descriptor.json", b"{}")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(traversal, "image", self.contract, None)

            symlink = root / "symlink.zip"
            link = zipfile.ZipInfo("image.json")
            link.create_system = 3
            link.external_attr = (stat.S_IFLNK | 0o777) << 16
            with zipfile.ZipFile(symlink, "w") as bundle:
                bundle.writestr(link, "remote-descriptor.json")
                bundle.writestr("remote-descriptor.json", b"{}")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(symlink, "image", self.contract, None)

    def test_archive_rejects_aliases_empty_files_and_excessive_compression(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            alias = root / "alias.zip"
            with zipfile.ZipFile(alias, "w") as bundle:
                bundle.writestr("./image.json", b"{}\n")
                bundle.writestr("remote-descriptor.json", b"{}\n")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(alias, "image", self.contract, None)

            double_slash = root / "double-slash.zip"
            with zipfile.ZipFile(double_slash, "w") as bundle:
                bundle.writestr("nested//image.json", b"{}\n")
                bundle.writestr("remote-descriptor.json", b"{}\n")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(double_slash, "image", self.contract, None)

            empty = root / "empty.zip"
            with zipfile.ZipFile(empty, "w") as bundle:
                bundle.writestr("image.json", b"")
                bundle.writestr("remote-descriptor.json", b"{}\n")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(empty, "image", self.contract, None)

            ratio = root / "ratio.zip"
            with zipfile.ZipFile(ratio, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
                bundle.writestr("image.json", b"A" * 100_000)
                bundle.writestr("remote-descriptor.json", b"{}\n")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_archive(ratio, "image", self.contract, None)

    def test_archive_enforces_metadata_and_streaming_byte_budgets(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "artifact.zip"
            with zipfile.ZipFile(archive, "w") as bundle:
                bundle.writestr("image.json", b"{}\n")
                bundle.writestr("remote-descriptor.json", b"{}\n")

            with mock.patch.object(verifier, "MAX_ARCHIVE_FILE_BYTES", 2):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.inspect_archive(archive, "image", self.contract, None)
            with mock.patch.object(verifier, "MAX_ARCHIVE_UNCOMPRESSED_BYTES", 5):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.inspect_archive(archive, "image", self.contract, None)
            with mock.patch.object(
                zipfile.ZipFile, "open", return_value=io.BytesIO(b"more-than-declared")
            ):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.inspect_archive(archive, "image", self.contract, None)

    def test_capsule_directory_entries_have_exact_names_modes_and_no_encryption(self) -> None:
        allowed_directory = zipfile.ZipInfo("phases/")
        allowed_directory.create_system = 3
        allowed_directory.external_attr = (stat.S_IFDIR | 0o755) << 16
        allowed_directory.CRC = 0
        allowed_directory.compress_size = 0
        self.assertEqual(verifier.canonical_zip_name(allowed_directory, "capsule"), "phases/")

        encrypted_directory = copy.copy(allowed_directory)
        encrypted_directory.flag_bits |= 0x1
        with self.assertRaises(verifier.EvidenceError):
            verifier.canonical_zip_name(encrypted_directory, "capsule")

        symlink_directory = copy.copy(allowed_directory)
        symlink_directory.external_attr = (stat.S_IFLNK | 0o777) << 16
        with self.assertRaises(verifier.EvidenceError):
            verifier.canonical_zip_name(symlink_directory, "capsule")

        alias_directory = copy.copy(allowed_directory)
        alias_directory.filename = "phases//"
        with self.assertRaises(verifier.EvidenceError):
            verifier.canonical_zip_name(alias_directory, "capsule")

        oversized_directory = copy.copy(allowed_directory)
        oversized_directory.compress_size = verifier.MAX_DIRECTORY_COMPRESSED_BYTES + 1
        with self.assertRaises(verifier.EvidenceError):
            verifier.canonical_zip_name(oversized_directory, "capsule")

        nonempty_directory = copy.copy(allowed_directory)
        nonempty_directory.CRC = 1
        with self.assertRaises(verifier.EvidenceError):
            verifier.canonical_zip_name(nonempty_directory, "capsule")

    def create_capsule(self, root: Path) -> tuple[Path, dict]:
        release_root = root / "phases"
        artifact_phases = []
        input_value = self.valid_input_value()
        manifest_hash = verifier.sha256_file(MANIFEST_PATH)

        for index, phase in enumerate(verifier.PHASES):
            phase_dir = release_root / phase
            phase_dir.mkdir(parents=True)
            item = input_value["phases"][index]
            run_id = item["image_run_id"]
            attempt = item["image_run_attempt"]
            source = self.manifest["phases"][phase]
            images = []
            tag = (
                f"{phase}-{source['source_sha'][:12]}-control-{CONTROL_SHA[:12]}-"
                f"run-{run_id}-{attempt}"
            )
            for component in verifier.COMPONENTS:
                expected = self.manifest["release"]["components"][component]
                images.append(
                    {
                        "component": component,
                        "image": expected["image"],
                        "digest": f"sha256:{digest(f'{phase}-{component}')}",
                        "platform": "linux/amd64",
                        "tag": tag,
                        "tag_is_authority": False,
                        "dockerfile": expected["dockerfile"],
                        "dockerfile_sha256": expected["dockerfile_sha256"],
                    }
                )
            release_set = {
                "schema_version": 1,
                "authority": "digest-only",
                "phase": phase,
                "source": {
                    "repository": self.manifest["repository"],
                    "commit": source["source_sha"],
                    "root_tree": source["root_tree"],
                    "frontend_tree": source["frontend_tree"],
                    "internal_tree": source["internal_tree"],
                    "manifest_sha256": manifest_hash,
                },
                "validation": {
                    "run_id": str(3000 + index),
                    "run_attempt": 1,
                    "run_url": (
                        "https://github.com/medtechcorps-netizen/whatomate/actions/runs/"
                        f"{3000 + index}"
                    ),
                },
                "builder": {
                    "workflow_sha": CONTROL_SHA,
                    "run_id": run_id,
                    "run_attempt": str(attempt),
                    "runner_environment": "github-hosted",
                },
                "images": images,
            }
            verifier.dump_canonical(release_set, phase_dir / "release-set.json")
            release_hash = verifier.sha256_file(phase_dir / "release-set.json")
            (phase_dir / "release-set.sha256").write_bytes((release_hash + "\n").encode("ascii"))
            (phase_dir / "release-set-provenance.bundle.json").write_bytes(b"{}\n")
            (phase_dir / "release-set-source-binding.bundle.json").write_bytes(b"{}\n")
            item["release_set_sha256"] = release_hash

            records = []
            names = verifier.artifact_name_map(phase, run_id, attempt)
            for record_index, (name, kind) in enumerate(sorted(names.items())):
                artifact_id = str(4000 + index * 100 + record_index)
                if kind == "release-set":
                    artifact_id = item["release_set_artifact_id"]
                archive_hash = digest(f"archive-{phase}-{name}")
                records.append(
                    {
                        "artifact_id": artifact_id,
                        "name": name,
                        "archive_digest": f"sha256:{archive_hash}",
                        "archive_sha256": archive_hash,
                        "size_in_bytes": 42,
                        "inventory": verifier.ARTIFACT_INVENTORIES[kind],
                    }
                )
            artifact_phases.append({"phase": phase, "artifacts": records})

        normalized = verifier.normalize_input(json.dumps(input_value), CONTROL_SHA)
        artifact_evidence = {"schema_version": 1, "phases": artifact_phases}
        plan = verifier.build_plan(
            normalized,
            artifact_evidence,
            release_root,
            self.manifest,
            self.contract,
            CONTROL_SHA,
            "9999",
            1,
            CONTRACT_PATH,
            MANIFEST_PATH,
            SCHEMA_PATH,
            VERIFIER_PATH,
        )
        verifier.dump_canonical(plan, root / "rollout-plan.json")
        (root / "rollout-plan.sha256").write_bytes(
            (verifier.sha256_file(root / "rollout-plan.json") + "\n").encode("ascii")
        )
        (root / "rollout-plan-provenance.bundle.json").write_bytes(b"{}\n")
        (root / "rollout-plan-policy.bundle.json").write_bytes(b"{}\n")
        return root, plan

    def write_plan(self, capsule: Path, plan: dict, *, canonical: bool = True) -> None:
        plan_path = capsule / "rollout-plan.json"
        if canonical:
            verifier.dump_canonical(plan, plan_path)
        else:
            plan_path.write_text(json.dumps(plan, indent=2) + "\n", encoding="utf-8", newline="\n")
        (capsule / "rollout-plan.sha256").write_bytes(
            (verifier.sha256_file(plan_path) + "\n").encode("ascii")
        )

    def test_schema_self_policy_rejects_tampering(self) -> None:
        schema = verifier.load_json(SCHEMA_PATH, "test schema")
        mutations = []
        value = copy.deepcopy(schema)
        value["$schema"] = "https://json-schema.org/draft/2019-09/schema"
        mutations.append(("draft", value))
        value = copy.deepcopy(schema)
        value["$id"] = "https://example.invalid/schema.json"
        mutations.append(("id", value))
        value = copy.deepcopy(schema)
        value["additionalProperties"] = True
        mutations.append(("root-open", value))
        value = copy.deepcopy(schema)
        value["description"] = "ignored keyword"
        mutations.append(("unknown-keyword", value))
        value = copy.deepcopy(schema)
        value["required"] = value["required"][:-1]
        mutations.append(("root-required", value))
        value = copy.deepcopy(schema)
        value["$defs"]["attempt"]["minimum"] = True
        mutations.append(("boolean-bound", value))
        value = copy.deepcopy(schema)
        value["$defs"]["sha1"]["pattern"] = "["
        mutations.append(("invalid-pattern", value))
        value = copy.deepcopy(schema)
        value["$defs"]["control"]["properties"]["workflow_sha"]["$ref"] = "https://example.invalid"
        mutations.append(("remote-ref", value))
        value = copy.deepcopy(schema)
        value["$defs"]["control"]["properties"]["workflow_sha"]["ignored"] = True
        mutations.append(("ref-sibling", value))

        for label, mutated in mutations:
            with self.subTest(label=label):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.validate_plan_schema(mutated)

    def test_schema_evaluator_enforces_every_used_instance_keyword(self) -> None:
        schema = verifier.validate_plan_schema(verifier.load_json(SCHEMA_PATH, "test schema"))
        with tempfile.TemporaryDirectory() as temporary:
            _, plan = self.create_capsule(Path(temporary))
            mutations = [
                ("const", lambda value: value.__setitem__("authority", "tag-authority")),
                ("required", lambda value: value.pop("repository")),
                ("additionalProperties", lambda value: value.__setitem__("unexpected", True)),
                ("strict-integer-type", lambda value: value.__setitem__("schema_version", True)),
                ("ref-and-pattern", lambda value: value["control"].__setitem__("workflow_sha", "Z" * 40)),
                ("enum", lambda value: value["phases"][0]["images"][0].__setitem__("component", "other")),
                ("minimum", lambda value: value["control"].__setitem__("run_attempt", 0)),
                ("maximum", lambda value: value["phases"][0].__setitem__("ordinal", 4)),
                ("minItems", lambda value: value["phases"][0]["images"].pop()),
                (
                    "maxItems-items-false",
                    lambda value: value["phases"].append(copy.deepcopy(value["phases"][-1])),
                ),
                (
                    "prefixItems",
                    lambda value: value["phases"][0].__setitem__("ordinal", "0"),
                ),
                (
                    "items-schema",
                    lambda value: value["phases"][0]["input_artifacts"][0].__setitem__(
                        "archive_sha256", "bad"
                    ),
                ),
                (
                    "uniqueItems",
                    lambda value: value["phases"][2]["rollback"].__setitem__(
                        "allowed_targets", ["bridge", "bridge"]
                    ),
                ),
                (
                    "minLength",
                    lambda value: value["phases"][0]["input_artifacts"][0].__setitem__("name", ""),
                ),
                (
                    "maxLength",
                    lambda value: value["phases"][0]["input_artifacts"][0].__setitem__(
                        "name", "x" * 256
                    ),
                ),
            ]
            for label, mutate in mutations:
                with self.subTest(label=label):
                    mutated = copy.deepcopy(plan)
                    mutate(mutated)
                    with self.assertRaises(verifier.EvidenceError):
                        verifier.validate_json_schema(mutated, schema)

    def test_validate_capsule_applies_the_loaded_schema(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            capsule, plan = self.create_capsule(root / "capsule")
            schema = verifier.load_json(SCHEMA_PATH, "test schema")
            schema["$defs"]["artifact"]["properties"]["size_in_bytes"]["maximum"] = 41
            schema_path = root / "strict-schema.json"
            verifier.dump_canonical(schema, schema_path)
            plan["control"]["plan_schema_sha256"] = verifier.sha256_file(schema_path)
            self.write_plan(capsule, plan)
            with self.assertRaises(verifier.EvidenceError):
                verifier.validate_capsule(
                    capsule, CONTRACT_PATH, MANIFEST_PATH, schema_path, VERIFIER_PATH
                )

    def test_exact_twenty_file_capsule_is_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            capsule, plan = self.create_capsule(Path(temporary))
            validated = verifier.validate_capsule(
                capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
            )
            self.assertEqual(validated, plan)
            self.assertEqual(len(verifier.capsule_paths(self.contract)), 20)
            for phase in validated["phases"]:
                self.assertEqual(
                    phase["migration"]["arguments"],
                    ["rls-migrate", "-config", "config.toml"],
                )
            self.assertEqual(
                validated["phases"][3]["rollback"],
                {"allowed_targets": ["backend", "bridge"], "forbidden_targets": ["baseline"]},
            )

    def test_capsule_requires_canonical_plan_bytes_even_when_hash_matches(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            capsule, plan = self.create_capsule(Path(temporary))
            self.write_plan(capsule, plan, canonical=False)
            with self.assertRaises(verifier.EvidenceError):
                verifier.validate_capsule(
                    capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                )

    def test_validation_run_ids_must_be_unique_across_phases(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            capsule, plan = self.create_capsule(Path(temporary))
            duplicate_id = plan["phases"][0]["validation"]["run_id"]
            duplicate_validation = {
                "run_id": duplicate_id,
                "run_attempt": 1,
                "run_url": (
                    "https://github.com/medtechcorps-netizen/whatomate/actions/runs/"
                    + duplicate_id
                ),
            }
            release_set_path = capsule / "phases" / "bridge" / "release-set.json"
            release_set = verifier.load_json(release_set_path, "bridge release set")
            release_set["validation"] = duplicate_validation
            verifier.dump_canonical(release_set, release_set_path)
            release_hash = verifier.sha256_file(release_set_path)
            (release_set_path.parent / "release-set.sha256").write_bytes(
                (release_hash + "\n").encode("ascii")
            )
            plan["phases"][1]["validation"] = duplicate_validation
            plan["phases"][1]["image_build"]["release_set_sha256"] = release_hash
            self.write_plan(capsule, plan)
            with self.assertRaisesRegex(verifier.EvidenceError, "validation run IDs repeat"):
                verifier.validate_capsule(
                    capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                )

    def test_capsule_rejects_migration_digest_and_rollback_mutations(self) -> None:
        for mutation in ("migration", "backend-rollback", "ui-rollback"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                capsule, _ = self.create_capsule(Path(temporary))
                plan_path = capsule / "rollout-plan.json"
                plan = json.loads(plan_path.read_text(encoding="utf-8"))
                if mutation == "migration":
                    plan["phases"][3]["migration"]["digest"] = "sha256:" + "f" * 64
                elif mutation == "backend-rollback":
                    plan["phases"][2]["rollback"]["allowed_targets"] = ["baseline"]
                else:
                    plan["phases"][3]["rollback"] = {
                        "allowed_targets": ["backend"],
                        "forbidden_targets": ["baseline", "bridge"],
                    }
                self.write_plan(capsule, plan)
                with self.assertRaises(verifier.EvidenceError):
                    verifier.validate_capsule(
                        capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                    )

    def test_capsule_rejects_extra_file(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            capsule, _ = self.create_capsule(Path(temporary))
            (capsule / "unexpected.txt").write_text("not authority\n", encoding="utf-8")
            with self.assertRaises(verifier.EvidenceError):
                verifier.validate_capsule(
                    capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                )

    def test_capsule_rejects_empty_required_file(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            capsule, _ = self.create_capsule(Path(temporary))
            (capsule / "phases" / "ui" / "release-set-provenance.bundle.json").write_bytes(b"")
            with self.assertRaises(verifier.EvidenceError):
                verifier.validate_capsule(
                    capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                )

        with tempfile.TemporaryDirectory() as temporary:
            capsule, _ = self.create_capsule(Path(temporary))
            with mock.patch.object(verifier, "MAX_ARCHIVE_FILE_BYTES", 1):
                with self.assertRaises(verifier.EvidenceError):
                    verifier.validate_capsule(
                        capsule, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
                    )

    def test_capsule_archive_requires_exact_twenty_file_inventory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            capsule, _ = self.create_capsule(root / "source")
            archive = root / "capsule.zip"
            with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
                bundle.writestr("phases/", b"")
                for phase in verifier.PHASES:
                    bundle.writestr(f"phases/{phase}/", b"")
                for relative in verifier.capsule_paths(self.contract):
                    bundle.write(capsule / relative, relative)
            extracted = root / "extracted"
            inventory = verifier.inspect_capsule_archive(
                archive, self.contract, extracted
            )
            self.assertEqual(inventory, verifier.capsule_paths(self.contract))
            verifier.validate_capsule(
                extracted, CONTRACT_PATH, MANIFEST_PATH, SCHEMA_PATH, VERIFIER_PATH
            )

    def test_capsule_archive_rejects_unexpected_empty_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            capsule, _ = self.create_capsule(root / "source")
            archive = root / "capsule.zip"
            with zipfile.ZipFile(archive, "w") as bundle:
                for relative in verifier.capsule_paths(self.contract):
                    bundle.write(capsule / relative, relative)
                bundle.writestr("phases/unreviewed/", b"")
            with self.assertRaises(verifier.EvidenceError):
                verifier.inspect_capsule_archive(archive, self.contract, None)


if __name__ == "__main__":
    unittest.main()
