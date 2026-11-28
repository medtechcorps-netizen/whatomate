"""Campaign inventory source policy, using real histories in a temporary Git repo.

Default discovery needs only the checked-out tree, including in a shallow clone.
It substitutes fixture pins to exercise the policy independently of private Git
objects. Set REREPLY_TEST_CAMPAIGN_INVENTORY_HEAD to a full candidate commit
to repeat the matrix on its sole parent and exact source projection.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
import unittest
from contextlib import nullcontext
from unittest import mock

import schema_change as sc
import test_ci_quote_writer_closure as quote
from test_ship_support import TempRepo


DATA = (
    "internal/campaignpacing/pacing.go",
    "internal/handlers/campaign_contact_audience.go",
    "internal/handlers/campaign_pacing.go",
)
WORKER = "internal/worker/campaign_continuation.go"
MIGRATION = (*DATA, WORKER)
REVIEWED = (
    ("e3ef94beacbcbb1a5c81a5fe53b45b1e2ac9fdc7c943ae96bbd964db67109502",
     "3bc8937ac37d12c08fea36c30e3df67ec3602e14583cdc965e3fd132f32a59de",
     "a802c5a1cbf5ccb79cda455805eed4cc94d91bb61fbe25395b66a0c0a8455b50"),
    ("0e3ebc418abbf445f0d9b7f9b9aadace26ea0851821a8dbbe03b0c773881c85b",
     "666761550aabcf434894f519e332cbbc5b28a7e3faec55cf91cb1c3222a790f0",
     "7a0406bd2ed15af61bf1e1f2be2c259a61634575d0f73d7006412d196ee99925"),
)
DOCS = (sc.REGISTRY, sc.MIGRATION_PATH)
FLOOR_LIMITS = (221, 245)
REVIEWED_LIMITS = (224, 249)


class CampaignInventoryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        private = tempfile.TemporaryDirectory()
        cls.addClassCleanup(private.cleanup)
        cls.repo = TempRepo(Path(private.name) / "repo").root
        source = Path(sc.__file__).resolve().parents[2]
        common = quote.git(source, "rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip()
        (cls.repo / ".git/objects/info/alternates").write_bytes(((Path(common) / "objects").as_posix() + "\n").encode())
        cls.exact_head = os.environ.get("REREPLY_TEST_CAMPAIGN_INVENTORY_HEAD")
        if cls.exact_head is not None:
            if not re.fullmatch(r"[0-9a-f]{40}", cls.exact_head):
                raise AssertionError("candidate must be a full lowercase Git commit ID")
            if quote.git(cls.repo, "cat-file", "-t", cls.exact_head) != b"commit\n":
                raise AssertionError("candidate must identify a Git commit")
            parents = quote.git(cls.repo, "show", "-s", "--format=%P", cls.exact_head).decode().split()
            if len(parents) != 1:
                raise AssertionError("candidate must have exactly one parent")
            cls.base, cls.post = parents[0], cls.exact_head
            cls.states = REVIEWED
            if (cls.state(cls.base), cls.state(cls.post)) != REVIEWED:
                raise AssertionError("candidate and parent must match both reviewed source states")
        else:
            starting = quote.git(source, "rev-parse", "HEAD^{tree}").decode().strip()
            cls.base, cls.post = cls.fixture_pair(starting)
            cls.states = (cls.state(cls.base), cls.state(cls.post))
        cls.post_tree = quote.git(cls.repo, "rev-parse", cls.post + "^{tree}").decode().strip()

    @classmethod
    def blob(cls, revision, path):
        return quote.git(cls.repo, "show", revision + ":" + path)

    @classmethod
    def commit(cls, tree, parents=(), changes=None):
        quote.git(cls.repo, "read-tree", tree)
        entries = []
        for path, data in (changes or {}).items():
            mode, oid = "0", "0" * 40
            if data is not None:
                mode = "100644"
                oid = quote.git(cls.repo, "hash-object", "-w", "--stdin", data=data).decode().strip()
            entries.append(f"{mode} {oid}\t{path}\0".encode())
        if entries:
            quote.git(cls.repo, "update-index", "-z", "--index-info", data=b"".join(entries))
        tree = quote.git(cls.repo, "write-tree").decode().strip()
        args = [arg for parent in parents for arg in ("-p", parent)]
        return quote.git(cls.repo, "commit-tree", tree, *args, "-m", "Campaign inventory fixture").decode().strip()

    @classmethod
    def fixture_pair(cls, source):
        # All inventory members are actual checkout paths. No prior checkout or
        # private feature blob is required by the portable source-policy tests.
        tracked = quote.git(cls.repo, "ls-tree", "-r", "--name-only", "-z", source, "--", "internal")
        existing = sorted(p.decode() for p in tracked.split(b"\0")
                          if p.endswith(b".go") and not p.endswith(b"_test.go") and p.decode() not in MIGRATION)
        before = {path: None for path in MIGRATION}
        after = {path: b"package fixture\n" for path in MIGRATION}
        for path, count, added in zip(DOCS, FLOOR_LIMITS, (DATA, MIGRATION)):
            doc = json.loads(cls.blob(source, path))
            row = quote.table(doc, path)
            row["closure_files"] = existing[:count]
            row["closure_sha256"] = "a" * 64
            before[path] = quote.wire(doc)
            row["closure_files"] = sorted([*existing[:count], *added])
            row["closure_sha256"] = "b" * 64
            after[path] = quote.wire(doc)
        caps = json.loads(cls.blob(source, quote.CAPS))
        caps[quote.TARGET] = FLOOR_LIMITS[0]
        before[quote.CAPS] = quote.wire(caps)
        caps[quote.TARGET] = REVIEWED_LIMITS[0]
        after[quote.CAPS] = quote.wire(caps)
        for values, limit in ((before, FLOOR_LIMITS[1]), (after, REVIEWED_LIMITS[1])):
            values[quote.CAP_TEST] = f"package registry\nconst migrationPathFileCap = {limit}\n".encode()
        base = cls.commit(source, changes=before)
        return base, cls.commit(base, (base,), after)

    @classmethod
    def state(cls, revision):
        docs = tuple(hashlib.sha256(cls.blob(revision, p)).hexdigest() for p in DOCS)
        return (*docs, sc._ci_inventory_source_digest(sc._Repository(cls.repo), revision))

    def admitted(self, head):
        with self.fixture_pins():
            sc._ci_campaign_inventory_closure(sc._Repository(self.repo), self.base, head)

    def fixture_pins(self):
        return nullcontext() if self.exact_head is not None else mock.patch.object(sc, "_CI_CAMPAIGN_STATES", self.states)

    def test_reviewed_pins_paths_and_limits(self):
        self.assertEqual(sc._CI_CAMPAIGN_STATES, REVIEWED)
        self.assertEqual(tuple(sc._CI_CAMPAIGN_DATA_FILES), DATA)
        self.assertEqual(tuple(sc._CI_CAMPAIGN_MIGRATION_FILES), MIGRATION)
        self.assertEqual((self.state(self.base), self.state(self.post)), self.states)
        for revision, (backfill, migration) in ((self.base, FLOOR_LIMITS), (self.post, REVIEWED_LIMITS)):
            for path, limit in zip(DOCS, (backfill, migration)):
                row = quote.table(json.loads(self.blob(revision, path)), path)
                self.assertEqual(len(row["closure_files"]), limit)
            self.assertEqual(json.loads(self.blob(revision, quote.CAPS))[quote.TARGET], backfill)
            self.assertIn(f"const migrationPathFileCap = {migration}\n".encode(),
                          self.blob(revision, quote.CAP_TEST))

    def test_atomic_push_and_two_parent_merge_admit_only_to_ci(self):
        self.admitted(self.post)
        merged = self.commit(self.post_tree, (self.base, self.post))
        self.admitted(merged)
        with self.fixture_pins():
            self.assertEqual(sc.check_ci_source(self.repo, self.base, merged), ())
        self.assertEqual(sc.check(self.repo, self.base, self.post), ("migration-path-changed:closure",))

    def test_equal_count_swaps_missing_and_extra_are_independently_refused(self):
        for path, old, replacement in ((sc.REGISTRY, DATA[2], WORKER),
                                       (sc.MIGRATION_PATH, WORKER, "internal/handlers/extra.go")):
            for action in ("swap", "omit", "extra"):
                with self.subTest(path=path, action=action):
                    doc = json.loads(self.blob(self.post, path))
                    row = quote.table(doc, path)
                    files = row["closure_files"]
                    if action in ("swap", "omit"):
                        files.remove(old)
                    if action in ("swap", "extra"):
                        files.append(replacement)
                    files.sort()
                    head = self.commit(self.post, (self.base,), {path: quote.wire(doc)})
                    # Even supplying matching hashes cannot authorize a
                    # different row membership or move the worker to backfill.
                    forged = (self.states[0], self.state(head))
                    with mock.patch.object(sc, "_CI_CAMPAIGN_STATES", forged), self.assertRaises(sc._Invalid):
                        sc._ci_campaign_inventory_closure(sc._Repository(self.repo), self.base, head)

    def test_caps_and_unrelated_source_are_pinned(self):
        caps = json.loads(self.blob(self.post, quote.CAPS))
        caps[quote.TARGET] += 1
        migration = self.blob(self.post, quote.CAP_TEST)
        mutations = ((quote.CAPS, quote.wire(caps)),
                     (quote.CAP_TEST, migration.replace(b"migrationPathFileCap = 249", b"migrationPathFileCap = 250")),
                     ("internal/handlers/unreviewed.go", b"package handlers\n"),
                     (DATA[1], self.blob(self.post, DATA[1]) + b"\n"))
        for path, data in mutations:
            with self.subTest(path=path), self.assertRaises(sc._Invalid):
                self.admitted(self.commit(self.post, (self.base,), {path: data}))

    def test_each_standalone_new_path_refuses_the_legacy_clean_fast_path(self):
        for path in MIGRATION:
            with self.subTest(path=path):
                head = self.commit(self.base, (self.base,), {path: self.blob(self.post, path)})
                self.assertIn("campaign-inventory-incomplete", sc.check_ci_source(self.repo, self.base, head))

    def test_partial_history_cannot_be_repaired_at_the_merge(self):
        for path in (*DOCS, quote.CAPS, quote.CAP_TEST, WORKER):
            with self.subTest(path=path):
                old = None if path == WORKER else self.blob(self.base, path)
                partial = self.commit(self.post, (self.base,), {path: old})
                restored = self.commit(self.post_tree, (partial,))
                merged = self.commit(self.post_tree, (self.base, restored))
                with self.assertRaises(sc._Invalid):
                    self.admitted(merged)
        other = self.commit(self.post_tree, (self.base,), {"feature-note.txt": b"second introduction\n"})
        with self.assertRaises(sc._Invalid):
            self.admitted(self.commit(self.post_tree, (self.post, other)))
