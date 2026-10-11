"""Scoped transport behavior; default release Git authority is unchanged."""
from concurrent.futures import ThreadPoolExecutor
from contextvars import copy_context
from pathlib import Path
import subprocess
import threading
import unittest
from unittest import mock

import release_record as records
import ship_common as common


class ScopedGitTests(unittest.TestCase):
    def test_default_path_retains_original_git_call_and_stdout(self):
        repo = Path("synthetic")
        with mock.patch.object(records, "git", return_value=subprocess.CompletedProcess([], 0, b"legacy", b"")) as old:
            self.assertEqual(records.git_output(repo, ["cat-file", "-t", "a" * 40], code="git-failed:test"), b"legacy")
        old.assert_called_once_with(repo, ["cat-file", "-t", "a" * 40], code="git-failed:test")

    def test_default_nonzero_still_refuses(self):
        with mock.patch.object(records, "git", return_value=subprocess.CompletedProcess([], 1, b"private", b"private")):
            with self.assertRaisesRegex(common.ReleaseError, "git-failed:test"):
                records.git_output(Path("synthetic"), [], code="git-failed:test")

    def test_scoped_reader_gets_original_arguments_and_never_old_runner(self):
        reader = mock.Mock(return_value=b"scoped")
        with mock.patch.object(records, "git", side_effect=AssertionError("legacy bypass")):
            with records.scoped_git_output(reader):
                self.assertEqual(records.git_output(Path("synthetic"), ["fixed"], code="git-failed:test"), b"scoped")
        reader.assert_called_once_with(Path("synthetic"), ["fixed"], code="git-failed:test")

    def test_exception_restores_prior_default_context(self):
        with self.assertRaisesRegex(RuntimeError, "synthetic"):
            with records.scoped_git_output(lambda *a, **k: b"wrong"):
                raise RuntimeError("synthetic")
        with mock.patch.object(records, "git", return_value=subprocess.CompletedProcess([], 0, b"restored", b"")):
            self.assertEqual(records.git_output(Path("repo"), [], code="git-failed:test"), b"restored")

    def test_nested_override_refuses_without_replacing_outer(self):
        with records.scoped_git_output(lambda *a, **k: b"outer"):
            with self.assertRaisesRegex(common.ReleaseError, "transport-scope"):
                with records.scoped_git_output(lambda *a, **k: b"inner"):
                    self.fail("nested scope entered")
            self.assertEqual(records.git_output(Path("repo"), [], code="git-failed:test"), b"outer")

    def test_noncallable_and_nonbytes_refuse(self):
        with self.assertRaisesRegex(common.ReleaseError, "transport-scope"):
            with records.scoped_git_output(None):
                pass
        with records.scoped_git_output(lambda *a, **k: "private"):
            with self.assertRaisesRegex(common.ReleaseError, "transport-result"):
                records.git_output(Path("repo"), [], code="git-failed:test")

    def test_concurrent_contexts_are_isolated(self):
        barrier = threading.Barrier(2)
        def run(value):
            with records.scoped_git_output(lambda *a, **k: value):
                barrier.wait(timeout=5)
                return records.git_output(Path("repo"), [], code="git-failed:test")
        with ThreadPoolExecutor(max_workers=2) as pool:
            a, b = pool.submit(run, b"a"), pool.submit(run, b"b")
            self.assertEqual((a.result(), b.result()), (b"a", b"b"))
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_copied_context_keeps_explicit_outer_scope_no_override(self):
        with records.scoped_git_output(lambda *a, **k: b"same operation"):
            ctx = copy_context()
            self.assertEqual(ctx.run(records.git_output, Path("repo"), [], code="git-failed:test"), b"same operation")
            def override():
                with records.scoped_git_output(lambda *a, **k: b"new"):
                    pass
            with self.assertRaisesRegex(common.ReleaseError, "transport-scope"):
                ctx.run(override)


if __name__ == "__main__":
    unittest.main()
