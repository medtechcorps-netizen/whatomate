"""Real disposable Git objects plus bounded-capture controls; no live repository."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest import mock

import source_git as sg
import schema_change as sc
import record_policy as policy
import release_record as records
import test_ship_support as support


ROOT = Path(__file__).resolve().parents[2]
GIT = Path(shutil.which("git")).resolve()


class SourceGitTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        self.base = self.repo.commit({"README.md": "committed\n"})

    def transport(self, **kwargs):
        return sg.ImmutableGit(self.repo.root, GIT, **kwargs)

    def test_committed_blob_ignores_dirty_worktree_and_derives_exact_v0(self):
        files = {p: (ROOT / p).read_text(encoding="utf-8") for p in policy.SOURCE_PATHS}
        head = self.repo.commit(files)
        self.repo.write({policy.GOLDEN: "dirty private replacement"})
        with self.transport().operation() as reader:
            facts = reader.reviewed_v0(head)
            self.assertEqual((facts.sha, facts.mode, facts.reader_version), (head, "reviewed-v0", 0))
            self.assertEqual(facts.catalog.catalog_sha256, policy.V0_MEASUREMENT)
            self.assertEqual(dict(facts.artifacts), {p: hashlib.sha256(v.encode()).hexdigest() for p, v in files.items()})

    def test_v0_missing_and_changed_pin_refuse_without_partial_facts(self):
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-blob"):
            with self.transport().operation() as reader:
                reader.reviewed_v0(self.base)
        files = {p: (ROOT / p).read_text(encoding="utf-8") for p in policy.SOURCE_PATHS}
        files[policy.GOLDEN] += " "
        head = self.repo.commit(files)
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-v0"):
            with self.transport().operation() as reader:
                reader.reviewed_v0(head)

    def test_regular_modes_only_symlink_and_gitlink(self):
        oid = self.repo.git("rev-parse", self.base + ":README.md").strip()
        for mode, object_id in (("120000", oid), ("160000", self.base)):
            self.repo.git("update-index", "--add", "--cacheinfo", mode + "," + object_id + ",foreign")
            self.repo.git("commit", "--quiet", "-m", "nonregular")
            with self.assertRaisesRegex(sg.SourceGitRefusal, "source-blob"):
                with self.transport().operation() as reader:
                    reader.read_blob(self.repo.head(), "foreign")

    def test_immutable_grammar_rejects_refs_options_paths_and_mutations(self):
        bad = [["cat-file", "-t", "HEAD"], ["show", self.base + ":../private"],
               ["show", self.base + ":.git/config"], ["show", self.base + ":(top)private"],
               ["fetch"], ["checkout", self.base], ["config", "--list"],
               ["-c", "alias.x=!private", "x"], ["diff", "--ext-diff", self.base, self.base],
               ["cat-file", "--filters", self.base + ":README.md"]]
        for args in bad:
            with self.subTest(args=args):
                with self.assertRaisesRegex(sg.SourceGitRefusal, "source-grammar"):
                    with self.transport().operation():
                        records.git_output(self.repo.root, args, code="private")

    def test_closed_and_foreign_repository_refuse(self):
        reader = self.transport()
        with reader.operation():
            self.assertEqual(reader.read_blob(self.base, "README.md"), b"committed\n")
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-closed"):
            reader.read_blob(self.base, "README.md")
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-scope"):
            with self.transport().operation():
                records.git_output(self.repo.root.parent, ["cat-file", "-t", self.base], code="git-failed:test")

    def test_subdirectory_cannot_hide_repository_source_paths(self):
        child = self.repo.root / "child"
        child.mkdir()
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-repository"):
            with sg.ImmutableGit(child, GIT).operation(): pass

    def test_replacements_grafts_shallow_and_promisor_refuse(self):
        other = self.repo.commit({"README.md": "other\n"})
        self.repo.git("update-ref", "refs/replace/" + self.base, other)
        with self.assertRaises(sg.SourceGitRefusal):
            with self.transport().operation(): pass
        self.repo.git("update-ref", "-d", "refs/replace/" + self.base)
        for name, content in (("info/grafts", self.base), ("shallow", other)):
            path = self.repo.root / ".git" / name
            path.parent.mkdir(exist_ok=True)
            path.write_text(content + "\n")
            with self.assertRaises(sg.SourceGitRefusal):
                with self.transport().operation(): pass
            path.unlink()
        self.repo.git("config", "remote.synthetic.promisor", "true")
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-repository"):
            with self.transport().operation(): pass

    def test_metadata_introduced_during_operation_refuses(self):
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-repository"):
            with self.transport().operation():
                (self.repo.root / ".git/info/grafts").write_text(self.base + "\n")
                records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="git-failed:test")

    def test_metadata_os_error_is_fixed_and_scope_is_restored(self):
        with self.assertRaises(sg.SourceGitRefusal) as caught:
            with self.transport().operation():
                with mock.patch.object(Path, "exists", side_effect=OSError("private path")):
                    records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="unused")
        self.assertEqual(str(caught.exception), "git-failed:source-repository")
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_no_external_diff_textconv_or_private_env(self):
        self.repo.git("config", "diff.external", "nonexistent-private-helper")
        self.repo.git("config", "diff.synthetic.textconv", "nonexistent-private-helper")
        head = self.repo.commit({"README.md": "new\n", ".gitattributes": "README.md diff=synthetic\n"})
        calls = []
        def capture(argv, **kwargs):
            calls.append((argv, kwargs))
            return sg.runtime.bounded_command(argv, **kwargs)
        with self.transport(capture=capture, base_env={**os.environ, "GH_TOKEN": "private", "GIT_CONFIG_COUNT": "99"}).operation():
            diff = records.git_output(self.repo.root, ["diff", "--no-renames", "-U0", self.base, head, "--", "README.md"], code="git-failed:test")
            self.assertIn(b"+new", diff)
        for argv, kwargs in calls:
            self.assertNotIn("GH_TOKEN", kwargs["env"])
            self.assertEqual(kwargs["env"]["GIT_CONFIG_COUNT"], "0")
            self.assertEqual(kwargs["env"]["GIT_NO_LAZY_FETCH"], "1")
            self.assertEqual(kwargs["env"]["PATH"], str(GIT.parent))
            self.assertLessEqual(kwargs["timeout"], 15)
            if "diff" in argv:
                self.assertIn("--no-ext-diff", argv)
                self.assertIn("--no-textconv", argv)

    def test_binary_attribute_does_not_hide_source_patch(self):
        head = self.repo.commit({"README.md": "new source\n", ".gitattributes": "README.md -diff\n"})
        with self.transport().operation():
            diff = records.git_output(self.repo.root, ["diff", "--no-renames", "-U0", self.base, head, "--", "README.md"], code="unused")
            self.assertIn(b"+new source", diff)

    def test_caught_refusal_still_prevents_successful_scope_exit(self):
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-closed"):
            with self.transport().operation() as reader:
                with self.assertRaisesRegex(sg.SourceGitRefusal, "source-grammar"):
                    reader.read_blob("HEAD", "README.md")

    def test_invalid_capture_shape_and_oversized_blob_refuse(self):
        armed = [False]
        def capture(argv, **kwargs):
            if armed[0]: return object()
            return sg.runtime.bounded_command(argv, **kwargs)
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-capture"):
            with self.transport(capture=capture).operation():
                armed[0] = True
                records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="unused")
        head = self.repo.commit({"large": "x" * (4 * sg.MIB + 1)})
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-blob"):
            with self.transport().operation() as reader:
                reader.read_blob(head, "large")

    def test_call_budget_includes_preflight_and_poisoned_scope(self):
        reader = self.transport(limits=sg.Limits(calls=8))
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-budget"):
            with reader.operation():
                records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="unused")
                records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="unused")
        self.assertEqual(reader.calls, 8)
        self.assertTrue(reader.failed and reader.closed)

    def test_shared_deadline_and_late_capture_refuse(self):
        ticks = [0.0]
        armed, seen = [False], []
        def capture(argv, **kwargs):
            seen.append(kwargs["timeout"])
            if armed[0]:
                ticks[0] = 11
                return sg.runtime.Capture(0, b"commit\n", b"")
            return sg.runtime.bounded_command(argv, **kwargs)
        reader = self.transport(limits=sg.Limits(seconds=10), clock=lambda: ticks[0], capture=capture)
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-deadline"):
            with reader.operation():
                ticks[0] = 7
                armed[0] = True
                records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="unused")
        self.assertEqual(seen[-1], 3 - sg.CLEANUP_RESERVE)

    def test_aggregate_bytes_reduce_actual_capture_limit(self):
        armed, seen = [False], []
        def capture(argv, **kwargs):
            seen.append(kwargs)
            if armed[0]:
                return sg.runtime.Capture(0, b"x" * (kwargs["stdout_limit"] + 1), b"")
            return sg.runtime.bounded_command(argv, **kwargs)
        reader = self.transport(limits=sg.Limits(total_bytes=8192), capture=capture)
        with self.assertRaisesRegex(sg.SourceGitRefusal, "source-capture"):
            with reader.operation():
                armed[0] = True
                reader.read_blob(self.base, "README.md")
        self.assertLess(seen[-1]["stdout_limit"], 4096)
        self.assertLessEqual(seen[-1]["stdout_limit"] + seen[-1]["stderr_limit"], 8192 - reader.bytes)

    def test_private_capture_exception_stderr_and_nonzero_are_fixed(self):
        for mode in ("exception", "stderr", "exit"):
            armed = [False]
            def capture(argv, **kwargs):
                if not armed[0]: return sg.runtime.bounded_command(argv, **kwargs)
                if mode == "exception": raise RuntimeError("private-token")
                return sg.runtime.Capture(1 if mode == "exit" else 0, b"private-token", b"private-token" if mode == "stderr" else b"")
            with self.assertRaises(sg.SourceGitRefusal) as caught:
                with self.transport(capture=capture).operation():
                    armed[0] = True
                    records.git_output(self.repo.root, ["cat-file", "-t", self.base], code="private-token")
            self.assertNotIn("private", str(caught.exception))

    def test_classifier_v2_all_reads_share_scope_and_budget(self):
        files = {sc.CATALOG: '{"version":0,"objects":[]}',
                 sc.HISTORY: '[{"version":0,"class":"baseline","min_reader_version":0}]', sc.GLOBALS: '[]',
                 "cmd/whatomate/main.go": "package main\n",
                 sc.MIGRATION_PATH: json.dumps({"version": 1, "roots": sc.MIGRATION_ROOTS,
                     "closure_files": ["cmd/whatomate/main.go"], "closure_sha256": "b" * 64})}
        base = self.repo.commit(files)
        head = self.repo.commit({"README.md": "next\n"})
        with mock.patch.object(records, "git", side_effect=AssertionError("unbounded bypass")):
            with self.transport().operation() as reader:
                result = sc.classify_v2(self.repo.root, base, head)
                self.assertEqual((result.kind, result.dormant, result.reasons), ("none", False, ()))
                self.assertGreater(reader.calls, 15)
        with mock.patch.object(records, "git", side_effect=AssertionError("unbounded bypass")):
            with self.assertRaisesRegex(sg.SourceGitRefusal, "source-budget"):
                with self.transport(limits=sg.Limits(calls=10)).operation():
                    sc.classify_v2(self.repo.root, base, head)

    def test_legacy_show_path_also_uses_regular_blob_reads(self):
        path = "cmd/whatomate/main.go"
        base = self.repo.commit({path: "package main\n\nfunc runRLSMigration() {\n}\n"})
        head = self.repo.commit({path: "package main\n\nfunc runRLSMigration() {\n changed()\n}\n"})
        with mock.patch.object(records, "git", side_effect=AssertionError("unbounded bypass")):
            with self.transport().operation():
                result = sc.classify(self.repo.root, base, head)
                self.assertIn("guarded-function:cmd/whatomate/main.go#runRLSMigration", result.reasons)


if __name__ == "__main__":
    unittest.main()
