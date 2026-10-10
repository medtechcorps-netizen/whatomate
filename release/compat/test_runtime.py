from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import runtime
import proof
import release_record
import ship_common as common
import test_ship_support as support


class CommandTests(unittest.TestCase):
    def call(self, script, **changes):
        options = dict(env=common.scrubbed_env(), timeout=5, stdout_limit=4096, stderr_limit=4096)
        options.update(changes)
        return runtime.bounded_command([sys.executable, "-I", "-S", "-B", "-c", script], **options)

    def test_simultaneous_pipes_and_semantic_nonzero_are_complete_not_success(self):
        result = self.call("import os; os.write(1,b'out'); os.write(2,b'private'); raise SystemExit(7)")
        self.assertEqual((result.returncode, result.stdout, result.stderr), (7, b"out", b"private"))
        self.assertEqual(repr(result), "Capture(returncode=7)")
        self.assertNotIn("private", repr(result))

    def test_exact_limits_and_both_overflows(self):
        result = self.call("import os; os.write(1,b'x'*4096); os.write(2,b'y'*4096)")
        self.assertEqual(len(result.stdout), 4096)
        self.assertEqual(len(result.stderr), 4096)
        for descriptor in (1, 2):
            with self.subTest(descriptor=descriptor), self.assertRaisesRegex(proof.InfrastructureFailure, "^capture$"):
                self.call(f"import os; os.write({descriptor},b'z'*4097)")

    def test_timeout_reaps_child_and_never_exposes_raw_error_or_arguments(self):
        seen = []
        real = subprocess.Popen
        def record(*args, **kwargs):
            process = real(*args, **kwargs)
            seen.append(process)
            return process
        start = time.monotonic()
        with mock.patch.object(runtime.subprocess, "Popen", side_effect=record):
            with self.assertRaisesRegex(proof.InfrastructureFailure, "^capture$"):
                self.call("import time; time.sleep(30)", timeout=0.15)
        self.assertLess(time.monotonic() - start, 3)
        self.assertIsNotNone(seen[0].poll())
        with mock.patch.object(runtime.subprocess, "Popen", side_effect=OSError("sensitive argv and host")):
            with self.assertRaisesRegex(proof.InfrastructureFailure, "^process$") as error:
                self.call("sensitive argv")
        self.assertNotIn("sensitive", str(error.exception))
        self.assertTrue(error.exception.__suppress_context__)

    def test_no_shell_no_inherited_stdin_and_environment_is_explicit(self):
        with mock.patch.dict(os.environ, {"COMPAT_TEST_PRIVATE": "synthetic-private"}):
            result = self.call("import os,sys; print(os.getenv('COMPAT_TEST_PRIVATE','absent')); print(len(sys.stdin.read()))")
        self.assertEqual(result.stdout.splitlines(), [b"absent", b"0"])

    def test_invalid_limits_refuse_before_process(self):
        for change in ({"timeout": False}, {"timeout": float("inf")}, {"timeout": 601},
                       {"stdout_limit": -1}, {"stderr_limit": 65 * runtime.MIB}):
            with self.subTest(change=change), mock.patch.object(runtime.subprocess, "Popen") as spawn:
                with self.assertRaises(proof.EvidenceError):
                    self.call("pass", **change)
                spawn.assert_not_called()

    def test_reader_thread_failure_reaps_process(self):
        seen = []
        real = subprocess.Popen
        def record(*args, **kwargs):
            process = real(*args, **kwargs)
            seen.append(process)
            return process
        with mock.patch.object(runtime.subprocess, "Popen", side_effect=record), mock.patch.object(runtime.threading.Thread, "start", side_effect=RuntimeError("synthetic")):
            with self.assertRaisesRegex(proof.InfrastructureFailure, "^process$"):
                self.call("import time; time.sleep(30)")
        self.assertIsNotNone(seen[0].poll())
        self.assertTrue(seen[0].stdout.closed and seen[0].stderr.closed)

    def test_bounded_input_and_simultaneous_output(self):
        data = b"x" * runtime.MIB
        result = self.call("import os,sys; os.write(1,b'o'*4096); os.write(2,b'e'*4096); data=sys.stdin.buffer.read(); raise SystemExit(0 if data==b'x'*1048576 else 3)", input_bytes=data)
        self.assertEqual((result.returncode, len(result.stdout), len(result.stderr)), (0, 4096, 4096))
        for value in ("secret", bytearray(b"x"), b"x" * (runtime.MIB + 1)):
            with self.subTest(kind=type(value).__name__), mock.patch.object(runtime.subprocess, "Popen") as spawn:
                with self.assertRaises(proof.EvidenceError):
                    self.call("pass", input_bytes=value)
                spawn.assert_not_called()

    def test_blocked_input_and_early_close_fail_closed(self):
        for script in ("import time; time.sleep(30)", "import os; os.close(0)"):
            seen = []
            real = subprocess.Popen
            def record(*args, **kwargs):
                child = real(*args, **kwargs)
                seen.append(child)
                return child
            with self.subTest(script=script), mock.patch.object(runtime.subprocess, "Popen", side_effect=record):
                with self.assertRaisesRegex(proof.InfrastructureFailure, "^capture$"):
                    self.call(script, timeout=0.2, input_bytes=b"x" * runtime.MIB)
            self.assertIsNotNone(seen[0].poll())


class DockerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name).resolve()
        self.calls = []
        self.result = runtime.Capture(0, b"", b"")
        def capture(argv, **options):
            self.calls.append((argv, options))
            return self.result
        self.docker = runtime.Docker("docker", self.root, capture=capture,
                                     base_env={"PATH": "safe", "DOCKER_HOST": "foreign", "GH_TOKEN": "private"})

    def tearDown(self):
        self.tmp.cleanup()

    def test_pins_exact_platform_and_no_ambient_credentials_or_host(self):
        self.docker.pull(runtime.POSTGRES)
        argv, options = self.calls[0]
        self.assertEqual(argv, ["docker", "--config", str(self.root), "--context", "default", "image", "pull", "--platform", "linux/amd64", runtime.POSTGRES])
        self.assertEqual(options["env"], {"PATH": "safe"})
        self.assertEqual(options["stdout_limit"], runtime.MIB)

    def test_mutable_foreign_or_option_images_refuse_without_call(self):
        for image in ("postgres:17", "--privileged", "foreign.invalid/x@sha256:" + "a" * 64,
                      "ghcr.io/medtechcorps-netizen/rereply-release-web:latest", runtime.REDIS + "\n"):
            with self.subTest(image=image), self.assertRaises(proof.EvidenceError):
                self.docker.pull(image)
        self.assertEqual(self.calls, [])

    def test_pull_failure_is_infrastructure_not_a_semantic_result(self):
        self.result = runtime.Capture(1, b"private", b"private")
        with self.assertRaisesRegex(proof.InfrastructureFailure, "^pull$"):
            self.docker.pull(runtime.REDIS)

    def test_inspect_exact_digest_platform_and_malformed_capture(self):
        good = {"Os": "linux", "Architecture": "amd64", "RepoDigests": ["postgres@" + runtime.POSTGRES.split("@")[1]]}
        self.result = runtime.Capture(0, json.dumps([good]).encode(), b"")
        self.assertEqual(self.docker.inspect(runtime.POSTGRES), good)
        for data in ([dict(good, Architecture="arm64")], [dict(good, RepoDigests=[])], [good, good], {}):
            self.result = runtime.Capture(0, json.dumps(data).encode(), b"")
            with self.subTest(data=data), self.assertRaisesRegex(proof.InfrastructureFailure, "^capture$"):
                self.docker.inspect(runtime.POSTGRES)

    def test_nonempty_cli_config_refused(self):
        (self.root / "config.json").write_text("{}")
        with self.assertRaises(proof.EvidenceError):
            runtime.Docker("docker", self.root, capture=lambda *a, **k: self.fail("called"))


class GhTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name).resolve()
        self.fake = support.FakeGh()
        self.calls = []
        self.corrupt_certificate = False
        self.asset_bytes_override = None
        self.make_client()

    def tearDown(self):
        self.tmp.cleanup()

    def capture(self, argv, **options):
        self.calls.append((argv, options))
        args = argv[1:]
        if len(args) == 4 and args[0] == "api" and "/releases/assets/" in args[1]:
            identity = int(args[1].rsplit("/", 1)[1])
            release = next(r for r in self.fake.releases if r["assets"][0]["id"] == identity)
            raw = self.fake.assets[release["tag_name"]] if self.asset_bytes_override is None else self.asset_bytes_override
            return runtime.Capture(0, raw, b"")
        result = self.fake(argv, env=options["env"])
        raw = result.stdout
        if self.corrupt_certificate and args[:2] == ["attestation", "verify"] and result.returncode == 0:
            value = json.loads(raw)
            value[0]["verificationResult"]["signature"]["certificate"]["runnerEnvironment"] = "self-hosted"
            raw = json.dumps(value).encode()
        return runtime.Capture(result.returncode, raw, result.stderr)

    def make_client(self):
        index = len(list(self.root.iterdir()))
        config, work = self.root / f"config-{index}", self.root / f"work-{index}"
        config.mkdir(); work.mkdir()
        self.client = runtime.ReadOnlyGh("gh", support.GH_TOKEN, expected_version=support.GH_VERSION,
                                         config_dir=config, work_dir=work, capture=self.capture,
                                         base_env={"PATH": "safe", "SHIP_DO_TOKEN": "private", "GH_HOST": "foreign"})

    def record(self, **changes):
        manifest = support.manifest(kind="promote", sha="1" * 40, images=support.NEW,
                                    previous="prod-0000", deployed_at="2026-10-04T12:00:00Z")
        manifest.update(changes)
        self.fake.add_record(manifest)
        self.fake.attest_ship_images(manifest["images"], manifest["sha"])
        return manifest

    def resolve(self, **kwargs):
        # Local ancestry is the unchanged production helper; isolate only that
        # Git operation in this transport suite, not trust/schema/signatures.
        with mock.patch.object(release_record, "is_ancestor", return_value=True):
            return runtime.verified_previous(self.client, self.root, **kwargs)

    def test_real_chain_and_attestation_parsers_with_bounded_asset_transport(self):
        manifest = self.record()
        result = self.resolve()
        self.assertEqual(result.release, manifest["release"])
        self.assertEqual(result.images, manifest["images"])
        self.assertEqual((self.client.work_dir / "record-0" / "release-manifest.json").read_bytes(), common.canonical_file_bytes(manifest))
        verifies = [x[0] for x in self.calls if x[0][1:3] == ["attestation", "verify"]]
        self.assertEqual(len(verifies), 4)  # One record, all three image subjects.
        self.assertTrue(all("--deny-self-hosted-runners" in x for x in verifies))
        for argv, options in self.calls:
            self.assertNotIn("SHIP_DO_TOKEN", options["env"])
            self.assertLessEqual(options["stdout_limit"], 64 * runtime.MIB)
            self.assertEqual(options["timeout"], 180)
            if argv[1] != "--version":
                self.assertEqual(options["env"]["GH_HOST"], "github.com")
        self.assertEqual(self.fake.writes, [])

    def test_read_only_commands_and_foreign_assets_refuse_before_capture(self):
        for args in (["api", "/user"], ["api", "--method", "POST", runtime._REPO_API + "/releases"],
                     ["release", "download", "x"], ["auth", "token"], ["attestation", "verify", "foreign"],
                     ["api", runtime._REPO_API + "/releases/assets/999", "-H", "Accept: application/octet-stream"]):
            before = len(self.calls)
            with self.subTest(args=args), self.assertRaises(proof.EvidenceError):
                self.client._run(args, code="gh-failed")
            self.assertEqual(len(self.calls), before)
        with self.assertRaisesRegex(proof.EvidenceError, "^gh-write-refused$"):
            self.client.run(["release", "create"], code="gh-failed")
        before = len(self.calls)
        with self.assertRaises(proof.EvidenceError):
            self.client.verify_image(common.IMAGE_REPOSITORY["web"], "sha256:" + "a" * 64,
                                     signer_workflow=release_record.SHIP_SIGNER, signer_digest="1" * 40,
                                     source_digest="1" * 40, predicate_type=release_record.SLSA_PREDICATE,
                                     bundle="foreign-local-file")
        self.assertEqual(len(self.calls), before)

    def test_wrong_certificate_and_broken_chain_fail_without_flags(self):
        self.record()
        self.corrupt_certificate = True
        with self.assertRaisesRegex(proof.EvidenceError, "^release-verification-failed$"):
            self.resolve()
        self.make_client()
        self.corrupt_certificate = False
        self.fake.tags[0]["object"]["sha"] = "2" * 40
        with self.assertRaisesRegex(proof.EvidenceError, "^release-verification-failed$"):
            self.resolve()

    def test_download_cannot_delete_or_escape_scratch_and_size_mismatch_refuses(self):
        manifest = self.record()
        self.client.releases()
        occupied = self.client.work_dir / "record-0"
        occupied.mkdir(); (occupied / "keep").write_bytes(b"unchanged")
        for directory in (occupied, self.root / "record-9", self.client.work_dir / "not-a-record"):
            before = len(self.calls)
            with self.subTest(directory=directory.name), self.assertRaises(proof.EvidenceError):
                self.client.download_manifest(manifest["release"], directory)
            self.assertEqual(len(self.calls), before)
        self.assertEqual((occupied / "keep").read_bytes(), b"unchanged")
        self.asset_bytes_override = b"short"
        with self.assertRaises(proof.EvidenceError):
            self.client.download_manifest(manifest["release"], self.client.work_dir / "record-1")
        self.assertFalse((self.client.work_dir / "record-1").exists())

    def test_invalid_target_fails_before_any_read(self):
        before = len(self.calls)
        with self.assertRaises(proof.EvidenceError):
            self.resolve(rollback_target="prod-0000\n")
        self.assertEqual(len(self.calls), before)

    def test_record_signature_only_reads_own_unchanged_download(self):
        manifest = self.record()
        self.client.releases()
        before = len(self.calls)
        with self.assertRaises(proof.EvidenceError):
            self.client.verify_file(self.root / "foreign", signer_digest="1" * 40, source_digest="1" * 40)
        self.assertEqual(len(self.calls), before)
        directory = self.client.work_dir / "record-0"
        self.client.download_manifest(manifest["release"], directory)
        path = directory / release_record.MANIFEST_ASSET
        original = path.read_bytes()
        path.write_bytes(original + b" ")
        before = len(self.calls)
        with self.assertRaises(proof.EvidenceError):
            self.client.verify_file(path, signer_digest="1" * 40, source_digest="1" * 40)
        self.assertEqual(len(self.calls), before)
        path.write_bytes(original)
        original_capture = self.client.capture
        def mutate_after_verify(argv, **options):
            result = original_capture(argv, **options)
            path.write_bytes(original + b" ")
            return result
        self.client.capture = mutate_after_verify
        with self.assertRaises(proof.EvidenceError):
            self.client.verify_file(path, signer_digest="1" * 40, source_digest="1" * 40)

    def test_bootstrap_reuses_exact_legacy_attestation_and_predicate(self):
        bootstrap = release_record.load_bootstrap()
        self.fake.attest_legacy_images(bootstrap["images"], bootstrap["sha"], workflow_sha=bootstrap["legacy_signer"]["workflow_sha"])
        entry = self.resolve(rollback_target="prod-0000")
        self.assertEqual(entry.release, "prod-0000")
        verifies = [x[0] for x in self.calls if x[0][1:3] == ["attestation", "verify"]]
        self.assertEqual(len(verifies), 6)
        self.make_client()
        records = self.fake.image_attestations[(common.IMAGE_REPOSITORY["web"], bootstrap["images"]["web"])]
        records[1]["predicate"]["source"]["commit"] = "f" * 40
        with self.assertRaisesRegex(proof.EvidenceError, "^release-verification-failed$"):
            self.resolve()

    def test_network_nonzero_is_infrastructure_and_stderr_never_propagates(self):
        self.fake.fail_commands.add("api")
        with self.assertRaisesRegex(proof.InfrastructureFailure, "^network$") as error:
            self.resolve()
        self.assertNotIn("stderr", repr(error.exception))


if __name__ == "__main__":
    unittest.main(verbosity=2)
