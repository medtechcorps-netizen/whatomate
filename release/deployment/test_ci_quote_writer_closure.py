"""Finite Quote writer source admission through public CI, legacy and v2 APIs.

The fixture builds real Git histories in a temporary repository. Its floor is
the reviewed production tree, and its writer uses the actual reviewed Git
blobs. No synthetic Go digest or shape assertion substitutes for generator CI.
"""
from __future__ import annotations

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import schema_change as sc
from test_ship_support import TempRepo, git_env


SOURCE = Path(__file__).resolve().parents[2]
MAIN_TREE = "1945c1053cb77990bd2ee6563dfd5b47a9706f67"
FLOOR_TREE = "cfb0df5ff3dbad8013b0da9cd5ab3cda5980e517"
READER_FILES = {
    "internal/models/models.go": "6da6117a03831afc00bb4cfd75fe9f35cf915af8",
    "internal/models/message_json_privacy_test.go": "1083b290d155994df88e63abd2c119f5656f9d41",
}
WRITER_FILES = {
    "cmd/whatomate/main.go": "52d164f63839a169ceac9429ebb1730169971ba5",
    "internal/handlers/chatbot_processor.go": "331c9c11c71b033c1efa8c5045ccb19e3dad5d83",
    "internal/handlers/coexistence_pending_quote.go": "9d3051bf9863d826721cd04d465ed14c7d4fef0f",
    "internal/handlers/contacts.go": "641829e2853810733cf3ed0140664845eab19650",
    "internal/handlers/customer_workspace.go": "6b45ca9fa71a6a6548c2f4610c517f633824a81e",
    "internal/handlers/inbox_attachment_scope.go": "8c8e03fa5d0218fbe716c7d4fe1825e658975765",
    "internal/handlers/inbox_attachment_storage.go": "41db1038e41ea0acbf121d57d423af5536d01166",
    "internal/handlers/inbox_attachment_storage_linux.go": "8ee000a3b35375ffb3f6a9c759226fed4a1a1498",
    "internal/handlers/inbox_attachment_storage_other.go": "a077e5bb80af70692f40c16b625c0138a30b43ab",
    "internal/handlers/inbox_channels.go": "5e90a7043430408be6c5ff2b7444255d9276e105",
    "internal/handlers/media.go": "1fc30c6d10fff474d57cebd0f41a4a0edccebad5",
    "internal/handlers/native_media_binding_read.go": "c76a0e6bbb25f856e565f4f0577144b8ae6efb48",
    "internal/handlers/quoted_reply_context.go": "42e9ec17eea661051cc7bc7f5452e5df18f506ae",
    "internal/handlers/whatsapp_coexistence_webhook.go": "f1b3a104d1a14d4475a65f7fbdf4975c5106a8ec",
    "internal/models/models.go": "4c6fb50eaa28d34669a3d3ab86a1c50c4607bfdd",
}
ADDED = tuple("internal/handlers/" + name for name in (
    "coexistence_pending_quote.go", "inbox_attachment_scope.go", "inbox_attachment_storage.go",
    "inbox_attachment_storage_linux.go", "native_media_binding_read.go", "quoted_reply_context.go",
))
TARGET = "BackfillChatbotFlowGraph"
CAPS = "internal/dbcatalog/registry/closure_caps.json"
CAP_TEST = "internal/dbcatalog/registry/migration_path_test.go"
ACTIVE = (sc.CATALOG, sc.HISTORY, sc.GLOBALS, sc.GOLDEN + "production-v0.json",
          "internal/dbcatalog/shape/production-v0.sql", sc.SEEDS)
HASHES = {
    sc.REGISTRY: "89f8deb49e041cfeecffe5903c825637c02cb16b6aab7c314a2ded58b94a097f",
    sc.MIGRATION_PATH: "53c8bad110afa5e64b0db0a7b4f17c19bb59c01d5dc768196bd7364c46e04b7e",
}


def wire(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode()


def table(doc, path):
    if path == sc.MIGRATION_PATH:
        return doc
    return next(row for row in doc if row["id"] == TARGET)


def git(repo, *args, data=None):
    return subprocess.run(["git", "-C", str(repo), *args], input=data, env=git_env(),
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True).stdout


class QuoteWriterClosureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(temp.cleanup)
        cls.repo = TempRepo(Path(temp.name) / "repo").root
        # release-tests checks out full history. Only objects are borrowed; all
        # commits, changes and the index in this fixture are private to the test.
        common = git(SOURCE, "rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip()
        (cls.repo / ".git/objects/info/alternates").write_bytes(((Path(common)/"objects").as_posix()+"\n").encode())
        # Reconstruct the reviewed reader tree even after squash-merging its PR.
        floor = cls.tree(MAIN_TREE, READER_FILES)
        if floor != FLOOR_TREE:
            raise AssertionError("reader floor tree differs")
        cls.ancestor = cls.commit(floor)
        cls.base = cls.commit(floor, cls.ancestor)
        cls.before = {p: cls.blob(cls.base, p) for p in (*HASHES, CAPS, CAP_TEST)}
        cls.after = {}
        for path, digest in HASHES.items():
            doc = json.loads(cls.before[path])
            row = table(doc, path)
            row["closure_files"] = sorted(row["closure_files"] + list(ADDED))
            row["closure_sha256"] = digest
            cls.after[path] = wire(doc)
        caps = json.loads(cls.before[CAPS])
        if caps[TARGET] != 215 or cls.before[CAP_TEST].count(b"const migrationPathFileCap = 239\n") != 1:
            raise AssertionError("reader limits differ")
        caps[TARGET] = 221
        cls.after[CAPS] = wire(caps)
        cls.after[CAP_TEST] = cls.before[CAP_TEST].replace(b"const migrationPathFileCap = 239\n", b"const migrationPathFileCap = 245\n")
        cls.post_tree = cls.tree(cls.base, WRITER_FILES, cls.after)
        cls.post = cls.commit(cls.post_tree, cls.base)

    @classmethod
    def blob(cls, revision, path):
        return git(cls.repo, "show", revision + ":" + path)

    @classmethod
    def tree(cls, parent, oids=None, values=None, modes=None):
        git(cls.repo, "read-tree", parent)
        entries = dict(oids or {})
        for path, value in (values or {}).items():
            entries[path] = None if value is None else git(cls.repo, "hash-object", "-w", "--stdin", data=value).decode().strip()
        changes = []
        for path, oid in entries.items():
            mode = (modes or {}).get(path, "100644")
            changes.append(((mode if oid else "0") + " " + (oid or "0"*40) + "\t" + path + "\0").encode())
        git(cls.repo, "update-index", "-z", "--index-info", data=b"".join(changes))
        return git(cls.repo, "write-tree").decode().strip()

    @classmethod
    def commit(cls, tree, *parents):
        args = [s for parent in parents for s in ("-p", parent)]
        return git(cls.repo, "commit-tree", tree, *args, "-m", "synthetic source history").decode().strip()

    def public_refused(self, head, base=None, *, reason=None):
        reasons = sc.check_ci_source(self.repo, base or self.base, head)
        self.assertTrue(reasons, "Quote source transition should be refused")
        if reason is not None:
            self.assertIn(reason, reasons)

    def refused(self, head, base=None):
        # The Git inputs and read bounds are real. Exercise the finite checker
        # directly in the case matrix without repeating every independent
        # legacy/v2 classifier once per malformed field or intermediate path.
        with self.assertRaises(sc._Invalid):
            sc._ci_quote_writer_closure(sc._Repository(self.repo), base or self.base, head)

    def refuse_final(self, path, data):
        self.refused(self.commit(self.tree(self.post_tree, values={path: data}), self.base))

    def test_push_and_synthetic_pr_only_open_ci_source(self):
        main = self.commit(self.tree(self.base, values={"quote-ci-fixture.txt": b"unrelated\n"}), self.base)
        clean_side = self.commit(self.tree(main, values={"quote-test-side.txt": b"feature note\n"}), main)
        notes = {"quote-ci-fixture.txt": b"unrelated\n", "quote-test-side.txt": b"feature note\n"}
        feature = self.commit(self.tree(self.post_tree, values=notes), clean_side)
        merged = self.commit(self.tree(feature), main, feature)
        for base, head in ((self.base, self.post), (main, merged)):
            with self.subTest(base=base):
                self.assertEqual(sc.check_ci_source(self.repo, base, head), ())
        self.assertEqual(sc.check(self.repo, self.base, self.post), ("migration-path-changed:closure",))
        policy = sc.classify_v2(self.repo, self.base, self.post)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons), ("none", False, ()))
        self.assertEqual(policy.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_exact_ordered_inventories_are_required_in_each_document(self):
        for path in HASHES:
            for change in ("missing", "additional", "fallback", "test-file", "duplicate", "reordered"):
                with self.subTest(path=path, change=change):
                    doc = json.loads(self.after[path])
                    files = table(doc, path)["closure_files"]
                    index = files.index(ADDED[3])
                    if change == "missing":
                        files.pop(index)
                    elif change == "additional":
                        files.append("internal/handlers/extra.go")
                        files.sort()
                    elif change == "fallback":
                        files[index] = ADDED[3].replace("_linux.go", "_other.go")
                        files.sort()
                    elif change == "test-file":
                        files[index] = ADDED[3].replace(".go", "_test.go")
                        files.sort()
                    elif change == "duplicate":
                        files.insert(index, files[index])
                    else:
                        files[index:index+2] = reversed(files[index:index+2])
                    self.refuse_final(path, wire(doc))

    def test_generated_digest_and_document_identity_are_exact(self):
        for path in HASHES:
            stale = table(json.loads(self.before[path]), path)["closure_sha256"]
            swapped = next(value for other, value in HASHES.items() if other != path)
            for digest in (stale, swapped):
                with self.subTest(path=path, digest=digest):
                    doc = json.loads(self.after[path])
                    table(doc, path)["closure_sha256"] = digest
                    self.refuse_final(path, wire(doc))
            with self.subTest(path=path, change="noncanonical"):
                self.refuse_final(path, self.after[path] + b"\n")
        for name in ("rev", "func", "flags", "gates", "parent", "other row", "ID order"):
            with self.subTest(change=name):
                doc = json.loads(self.after[sc.REGISTRY])
                row = table(doc, sc.REGISTRY)
                if name == "rev":
                    row["rev"] += 1
                elif name == "func":
                    row["func"] += "Other"
                elif name == "flags":
                    row["n1_safe"] = not row["n1_safe"]
                elif name == "gates":
                    row["requires_gates"] = ["unreviewed"]
                elif name == "parent":
                    row["parent"] = doc[0]["id"]
                elif name == "other row":
                    other = next(r for r in doc if r["id"] != TARGET)
                    other["closure_sha256"] = "a"*64
                else:
                    doc[0], doc[1] = doc[1], doc[0]
                self.refuse_final(sc.REGISTRY, wire(doc))
        for k in ("roots", "version"):
            with self.subTest(key=k):
                doc = json.loads(self.after[sc.MIGRATION_PATH])
                if k == "roots":
                    doc[k].reverse()
                else:
                    doc[k] += 1
                self.refuse_final(sc.MIGRATION_PATH, wire(doc))

    def test_atomic_caps_must_match_both_approved_limits(self):
        for path in (CAPS, CAP_TEST):
            for state in ("missing", "old", "larger"):
                with self.subTest(path=path, state=state):
                    data = self.before[path] if state == "old" else None
                    if state == "larger":
                        if path == CAPS:
                            caps = json.loads(self.after[path])
                            caps[TARGET] += 1
                            data = wire(caps)
                        else:
                            data = self.after[path].replace(b"migrationPathFileCap = 245\n", b"migrationPathFileCap = 246\n")
                    if path == CAPS and state == "larger":
                        head = self.commit(self.tree(self.post_tree, values={path: data}), self.base)
                        self.public_refused(head, reason="migration-path-changed:closure")
                    else:
                        self.refuse_final(path, data)
        caps = json.loads(self.after[CAPS])
        caps[next(k for k in caps if k != TARGET)] += 1
        self.refuse_final(CAPS, wire(caps))

    def test_physical_quote_inputs_and_active_contracts(self):
        for path in (*ADDED, "internal/handlers/contacts.go", "internal/handlers/inbox_attachment_storage_other.go",
                     "internal/handlers/chatbot_flow_migration.go", *ACTIVE):
            with self.subTest(path=path):
                self.refuse_final(path, self.blob(self.post, path)+b"\n")
        self.refuse_final(sc.GOLDEN+"pending/quote.json", b"{}\n")
        self.refuse_final(ADDED[3], None)
        self.refuse_final(sc.MIGRATION_PATH, b"{malformed\n")
        self.refuse_final(sc.REGISTRY, b" "*(sc.MAX_ARTIFACT_BYTES+1))
        changed_initializer = self.blob(self.post, ADDED[1])
        before = b"var inboxAttachmentSlots = make(chan struct{}, 2)"
        self.assertEqual(changed_initializer.count(before), 1)
        self.refuse_final(ADDED[1], changed_initializer.replace(before, b"var inboxAttachmentSlots = make(chan struct{}, 3)"))
        for path in HASHES:
            with self.subTest(executable_metadata=path):
                oid = git(self.repo, "rev-parse", self.post + ":" + path).decode().strip()
                wrong_mode = self.tree(self.post_tree, {path: oid}, modes={path: "100755"})
                self.refused(self.commit(wrong_mode, self.base))
        for path in (ADDED[0], CAPS, sc.MIGRATION_PATH):
            with self.subTest(nonregular=path):
                bad = self.tree(self.post, values={path: b"fixture-target"}, modes={path: "120000"})
                self.refused(self.commit(bad, self.base))

    def test_intermediate_protected_inputs_must_match_a_complete_reviewed_state(self):
        for path in (*ADDED, CAPS, CAP_TEST, sc.CATALOG, sc.SEEDS):
            with self.subTest(path=path):
                changed = self.tree(self.post, values={path: self.blob(self.post, path)+b"\n"})
                middle = self.commit(changed, self.post)
                restored = self.commit(self.post_tree, middle)
                self.refused(restored)
                # A pull_request test also receives a two-parent synthetic
                # merge. Its feature parent's earlier change must stay visible.
                merge = self.commit(self.post_tree, self.base, restored)
                if path == ADDED[3]:
                    self.public_refused(merge, reason="migration-path-changed:closure")
                else:
                    self.refused(merge)
        missing_source = self.commit(self.tree(self.post, values={ADDED[5]: None}), self.post)
        self.refused(self.commit(self.post_tree, missing_source))
        for path in HASHES:
            with self.subTest(split=path):
                mid = self.commit(self.tree(self.post_tree, values={path: self.before[path]}), self.base)
                self.refused(self.commit(self.post_tree, mid))
        reverted = self.commit(FLOOR_TREE, self.post)
        self.refused(self.commit(self.post_tree, reverted))
        parallel = self.commit(self.tree(self.post_tree, values={"quote-side.txt": b"second\n"}), self.base)
        self.refused(self.commit(self.post_tree, self.post, parallel))

    def test_stale_preimage_and_metadata_without_measured_source(self):
        wrong_base = self.commit(self.tree(self.base, values={"internal/handlers/contacts.go":
                                 self.blob(self.base, "internal/handlers/contacts.go") + b"\n"}), self.base)
        self.refused(self.commit(self.post_tree, wrong_base), wrong_base)
        metadata_only = self.tree(self.base, values=self.after)
        self.refused(self.commit(metadata_only, self.base))
        writer_only = self.tree(self.base, WRITER_FILES)
        self.public_refused(self.commit(writer_only, self.base), reason="quote-writer-closure-incomplete")
        caps_only = self.tree(writer_only, values={p: self.after[p] for p in (CAPS, CAP_TEST)})
        self.refused(self.commit(caps_only, self.base))
        for path in ACTIVE:
            with self.subTest(unchanged_but_wrong=path):
                altered = self.blob(self.base, path) + b"\n"
                bad_floor = self.commit(self.tree(self.base, values={path: altered}), self.base)
                same_bad_active = self.tree(self.post_tree, values={path: altered})
                self.refused(self.commit(same_bad_active, bad_floor), bad_floor)

    def test_first_parent_and_shared_read_bounds(self):
        unrelated = self.commit(self.tree(self.base, values={"unrelated": b"separate branch\n"}), self.ancestor)
        self.refused(self.commit(self.post_tree, unrelated, self.post))
        newer_main = self.commit(self.tree(self.base, values={"main-only.txt": b"unrelated\n"}), self.base)
        stale_pr = self.commit(self.tree(self.post_tree, values={"main-only.txt": b"unrelated\n"}), newer_main, self.post)
        self.refused(stale_pr, newer_main)
        duplicate = self.commit(self.post_tree, self.post)
        shallow = self.repo / ".git/shallow"
        shallow.write_bytes((duplicate + "\n").encode())
        try:
            try:
                self.public_refused(duplicate)
            except sc.common.ReleaseError:
                # A failed Git ancestry read is itself a blocking CI result.
                pass
        finally:
            shallow.unlink()
        original = sc._Repository.__init__
        def exhausted(repo, root):
            original(repo, root)
            repo.total = 64 * 1024 * 1024
        with mock.patch.object(sc._Repository, "__init__", exhausted):
            self.public_refused(self.post)
