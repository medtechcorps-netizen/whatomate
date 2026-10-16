"""Exact managed-owner replication edge under an externally reviewed consumer.

Historical source bytes are inert inputs to disposable Git histories. These
tests do not establish live role facts, provider acceptance or release authority.
"""
import base64
import hashlib
import json
from pathlib import Path
import re
import shutil
import unittest
import zlib
from unittest import mock

import schema_change as sc
import source_git
import test_pr18_run_metadata_repair as historical

POLICY = "release/deployment/schema_change.py"
OLD_TEST = "release/deployment/test_pr18_run_metadata_repair.py"
TEST = "release/deployment/test_pr18_managed_replication_repair.py"
FIXTURE = "release/deployment/fixtures/pr18-managed-replication-repair.json"
SLOT = re.compile(rb"(?m)^_PR18_MANAGED_REPLICATION_POLICY_SHA256 = '([0-9a-f]{64})'\n")
HISTORICAL_WRAPPER_SHA256 = '750c846b773a4b8cc8d5129c0136f9d3b59dd304d42233f1e83ff30c4a8c384c'
SOURCE_PINS = {'cmd/whatomate/migration_plan_managed_owner_admission_pg_test.go': ('1f65a0b7da0d8b0637416e64bf061f7fd7fbdbcf27dd95799c06ea1fb04b6abb',
                                                                     '3e4ef3d4ffc94f4be16e03eb2c1efef227f5a7869e95d8a586b846e3a78616d5'),
 'cmd/whatomate/migration_plan_managed_owner_pg_test.go': ('b8cc9cc0090e03a06d657f1af5d70bf97c60dabd8dd0f6e65aa211007bd8d5d4',
                                                           'faa099ea8cc47569f1bbaddabf03285b7d0bde8f1d797abd9908a3407e48e215'),
 'docs/catalog.md': ('29f182362f3128732ea55f6a3ba43dcc099e84fb2f1e3b3a599db4b3cbe115dc',
                     '4c2641310c4f316af3a7553579beae81df5431688bfef7687675784fac40dae1'),
 'internal/database/data_gates.go': ('b8cb4b1b608f87069e2724ae29d4b6b38bcdbcf85e5c2bf085734015902953cd',
                                     'a56b6a9281cddb47f845ccd9a9b5920ecfb061f7c87d12d35d54f27c29977d06'),
 'internal/database/plan.go': ('83d6f4e83ced0c9a02f3ce18470e3da54914cb63c77f70f9a8fda7cb44c68365',
                               '922b3f623817045bbe0f45a870bd910844a697a0289d23f94ab6503bed9df67b'),
 'internal/dbcatalog/registry/migration_path.json': ('4acecb4e3caed65bb71f8d61ea2b3e760129523bfc599130fa3aee5ed421969d',
                                                     '08618e383eed2ab9eb771918b0ef618efa2ecce8edb99cfd476bd18980938ba6')}


class PR18ManagedReplicationRepairTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        historical.PR18RunMetadataRepairTests.setUpClass()
        raw = (Path(__file__).parent / "fixtures/pr18-managed-replication-repair.json").read_bytes()
        doc = json.loads(raw)
        if (type(doc) is not dict or set(doc) != {"schema_version", "before", "after"}
                or type(doc["schema_version"]) is not int or doc["schema_version"] != 1
                or len(SOURCE_PINS) != 6
                or any(type(doc[side]) is not dict or set(doc[side]) != set(SOURCE_PINS)
                       for side in ("before", "after"))):
            raise AssertionError("independent replication fixture schema")
        for path, pair in SOURCE_PINS.items():
            for side, expected in zip(("before", "after"), pair, strict=True):
                value = doc[side][path]
                if type(value) is not str or hashlib.sha256(value.encode()).hexdigest() != expected:
                    raise AssertionError("independent replication fixture identity")
        wrapper = Path(historical.__file__).read_bytes()
        if hashlib.sha256(wrapper).hexdigest() != HISTORICAL_WRAPPER_SHA256:
            raise AssertionError("independent run-metadata wrapper identity")
        cls.policy_input = Path(sc.__file__).read_bytes()
        cls.before = {**doc["before"],
                      POLICY: historical._historical_run_metadata_input(POLICY).decode(),
                      OLD_TEST: historical._historical_run_metadata_input(OLD_TEST).decode(),
                      TEST: None, FIXTURE: None}
        cls.after = {**doc["after"], POLICY: cls.policy_input.decode(),
                     OLD_TEST: wrapper.decode(), TEST: Path(__file__).read_text(encoding="utf-8"),
                     FIXTURE: raw.decode()}
        if len(cls.after) != 10 or set(cls.before) != set(cls.after):
            raise AssertionError("closed independent ten-path repair")

    def setUp(self):
        # The predecessor constructs its original exact history from frozen
        # inputs, including its original policy and test, under this consumer.
        self.prior = historical.PR18RunMetadataRepairTests()
        self.prior.setUp()
        self.addCleanup(self.prior.doCleanups)
        self.repo, self.base = self.prior.repo, self.prior.base
        self.predecessor = self.prior.head()
        for path, value in self.before.items():
            dest = self.repo.root / path
            self.assertEqual(dest.read_text(encoding="utf-8") if dest.is_file() else None, value)

    def head(self, extra=None):
        self.repo.checkout(self.predecessor)
        return self.repo.commit({**self.after, **(extra or {})}, "one exact managed replication repair")

    def local_refuses(self, head, base=None):
        base = base or self.predecessor
        self.assertIn("pr18-managed-replication-repair-incomplete",
                      sc.check_ci_source(self.repo.root, base, head))
        self.assertIn("classifier-v2-invalid:pr18-managed-replication-repair",
                      sc.classify_v2(self.repo.root, base, head).reasons)

    def full_refuses(self, head):
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))
        self.assertTrue(sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_exact_local_and_full_origin_keep_distinct_truthful_results(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.predecessor, head)
        self.assertEqual((legacy.kind, legacy.dormant, legacy.reasons),
                         ("none", False, ("migration-path-changed:closure",)))
        local = sc.classify_v2(self.repo.root, self.predecessor, head)
        self.assertEqual((local.kind, local.dormant, local.reasons), ("none", False, ()))
        self.assertEqual(local.migration_changes, ("generated migration closure changed",))
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof:
            full = sc.classify_v2(self.repo.root, self.base, head)
            self.assertEqual(proof.call_count, 1)
        self.assertEqual((full.kind, full.dormant, full.reasons), ("data", True, ()))
        self.assertEqual(full.migration_changes,
                         self.prior.historical.old.anchor_fixture.MIGRATION_OBSERVATIONS)
        self.assertEqual(local.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(full.requirements, local.requirements)
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof:
            self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
            self.assertGreaterEqual(proof.call_count, 1)

    def test_deferred_full_origin_cannot_clear_a_failed_final_transition(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.base, head)
        self.assertEqual(legacy.reasons, sc._PR18_OBSERVER_LEGACY)
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("the exact legacy path is deferred")), \
                mock.patch.object(sc, "_pr18_observer_transition", side_effect=sc._Invalid) as proof:
            full = sc.classify_v2(self.repo.root, self.base, head)
            self.assertIn(sc._PR18_OBSERVER_CALL, full.reasons)
            self.assertEqual(proof.call_count, 1)
            proof.reset_mock()
            ci = sc.check_ci_source(self.repo.root, self.base, head)
            self.assertIn(sc._PR18_OBSERVER_CALL, ci)
            self.assertIn(sc._PR18_OBSERVER_REASON, ci)
            self.assertEqual(proof.call_count, 1)

    def test_historical_run_metadata_inputs_are_not_current_consumer_bytes(self):
        for path, size, digest in (('release/deployment/schema_change.py',
  131619,
  '3ca123b80da6c8c22dc12bd962d94322135bac6b8da306f99636debd7e3d6c28'),
 ('release/deployment/test_pr18_run_metadata_repair.py',
  601800,
  'ebd8f028d75e8ad095ffc031ab02e8bbe078a8dadbadf17d5ede80d2d163b49f')):
            raw = historical._historical_run_metadata_input(path)
            self.assertEqual((len(raw), hashlib.sha256(raw).hexdigest()), (size, digest))
            self.assertEqual(self.before[path].encode(), raw)
            self.assertNotEqual(self.after[path].encode(), raw)
        size, digest, encoded = historical.HISTORICAL_RUN_METADATA_INPUTS[POLICY]
        packed = base64.b64decode(encoded, validate=True)
        variants = (
            (size - 1, digest, encoded),
            (601801, digest, encoded),
            (True, digest, encoded),
            (size, "0" * 64, encoded),
            (size, digest, base64.b64encode(packed[:-1]).decode()),
            (size, digest, base64.b64encode(packed + b"trailing").decode()),
            (size, digest, base64.b64encode(zlib.compress(b"x" * (size + 10))).decode()),
            (size, digest, "!invalid-base64!"),
        )
        for index, variant in enumerate(variants):
            with self.subTest(decoder_variant=index), \
                    mock.patch.dict(historical.HISTORICAL_RUN_METADATA_INPUTS, {POLICY: variant}):
                with self.assertRaises(AssertionError):
                    historical._historical_run_metadata_input(POLICY)

    def test_each_missing_or_changed_input_refuses(self):
        for path in self.after:
            with self.subTest(path=path, mutation="missing"):
                head = self.head({path: self.before[path]})
                self.local_refuses(head); self.full_refuses(head)
            with self.subTest(path=path, mutation="changed"):
                head = self.head({path: self.after[path] + "\n# unreviewed\n"})
                if path == "internal/dbcatalog/registry/migration_path.json":
                    # Invalid JSON retains the existing classifier's refusal;
                    # it must not be relabelled as an admission-edge failure.
                    self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                                     ("classifier-invalid:migration-path",))
                    self.assertEqual(sc.classify_v2(self.repo.root, self.predecessor, head).reasons,
                                     ("classifier-invalid:migration-path",
                                      "classifier-v2-invalid:migration-path"))
                else:
                    self.local_refuses(head)
                self.full_refuses(head)
        path = "internal/dbcatalog/registry/migration_path.json"
        with self.subTest(path=path, mutation="valid-json-whitespace"):
            # Valid, semantically identical JSON must still match the reviewed
            # whole-file postimage exactly; the original pin assertions apply.
            head = self.head({path: self.after[path] + "\n"})
            self.local_refuses(head); self.full_refuses(head)

    def test_stale_closure_and_protected_source_refuse(self):
        path = "internal/dbcatalog/registry/migration_path.json"
        head = self.head({path: self.before[path]})
        self.local_refuses(head); self.full_refuses(head)
        for path in ("internal/dbcatalog/registry/data_steps.json", "internal/dbcatalog/golden/seeds.json",
                     "internal/dbcatalog/dormant_entrypoints.json", sc.CATALOG):
            self.repo.checkout(self.predecessor)
            value = (self.repo.root / path).read_text(encoding="utf-8") + "\n"
            head = self.head({path: value})
            self.local_refuses(head); self.full_refuses(head)
        for path in (sc._V2_PENDING + "unreviewed.json", sc.schema_nullable.SOURCE_PATH):
            head = self.head({path: "{}\n"})
            self.local_refuses(head); self.full_refuses(head)

    def test_split_extra_prefix_suffix_and_reapply_refuse(self):
        path = "internal/database/plan.go"
        self.repo.commit({path: self.after[path]}, "partial")
        head = self.repo.commit(self.after, "remaining")
        self.local_refuses(head); self.full_refuses(head)
        head = self.head({"README.md": "unreviewed extra\n"})
        self.local_refuses(head); self.full_refuses(head)
        self.repo.checkout(self.predecessor)
        self.repo.commit({OLD_TEST: self.after[OLD_TEST]}, "historical wrapper prefix")
        head = self.repo.commit(self.after, "repair after separate wrapper edit")
        self.local_refuses(head); self.full_refuses(head)
        self.repo.checkout(self.predecessor)
        self.repo.commit({"README.md": "nonempty prefix\n"})
        head = self.repo.commit(self.after, "repair after prefix")
        self.local_refuses(head); self.full_refuses(head)
        self.head()
        head = self.repo.commit({"README.md": "nonempty suffix\n"})
        self.local_refuses(head); self.full_refuses(head)
        self.head()
        self.repo.commit(self.before, "undo repair")
        head = self.repo.commit(self.after, "repeat repair")
        self.local_refuses(head); self.full_refuses(head)

    def test_complete_undo_cannot_escape_clean_delta(self):
        self.head()
        head = self.repo.commit(self.before, "complete undo")
        self.assertEqual(self.repo.git("diff", "--name-only", self.predecessor, head), "")
        self.local_refuses(head); self.full_refuses(head)

    def test_invalid_marker_introduction_then_undo_still_refuses(self):
        self.repo.checkout(self.base)
        self.repo.commit({TEST: self.after[TEST]}, "marker without predecessors")
        head = self.repo.commit({TEST: None}, "undo marker")
        self.assertEqual(self.repo.git("diff", "--name-only", self.base, head), "")
        self.local_refuses(head, self.base)

    def test_noop_prefix_and_suffix_preserve_exact_edge(self):
        self.repo.git("commit", "--allow-empty", "-m", "prefix noop")
        self.repo.commit(self.after, "exact edge after noop")
        self.repo.git("commit", "--allow-empty", "-m", "suffix noop")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assertFalse(sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_wrong_mode_and_renamed_input_refuse(self):
        for path, value in self.after.items():
            dest = self.repo.root / path
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(value, encoding="utf-8", newline="\n")
        (self.repo.root / TEST).chmod(0o755)
        self.repo.git("add", "-A")
        self.repo.git("update-index", "--chmod=+x", TEST)
        self.repo.git("commit", "-m", "wrong executable mode")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.local_refuses(head); self.full_refuses(head)
        head = self.head({TEST: None, TEST + ".renamed": self.after[TEST]})
        self.local_refuses(head); self.full_refuses(head)

    def test_policy_slot_does_not_self_authorize_changes(self):
        match, = SLOT.finditer(self.policy_input)
        body = self.policy_input + b"\n# unreviewed policy change\n"
        canonical = body[:match.start(1)] + b"0" * 64 + body[match.end(1):]
        recomputed = body[:match.start(1)] + hashlib.sha256(canonical).hexdigest().encode() + body[match.end(1):]
        for value in (body, recomputed, self.policy_input + match.group(0),
                      self.policy_input[:match.start(1)] + b"f" * 64 + self.policy_input[match.end(1):]):
            with self.subTest(digest=hashlib.sha256(value).hexdigest()):
                head = self.head({POLICY: value.decode()})
                self.local_refuses(head); self.full_refuses(head)

    def test_wrong_preimage_and_preexisting_marker_fail_exact_edge(self):
        for path, value in (("internal/database/plan.go", "// wrong preimage\n"),
                            (TEST, "# preexisting marker\n")):
            self.repo.checkout(self.predecessor)
            wrong = self.repo.commit({path: value}, "wrong predecessor")
            head = self.repo.commit(self.after, "otherwise exact edge")
            with self.assertRaises(sc._Invalid):
                sc._pr18_managed_replication_local(sc._Repository(self.repo.root), wrong, head)
            self.full_refuses(head)

    def test_second_parent_base_does_not_prove_first_parent_lineage(self):
        self.repo.git("checkout", "-b", "replication-side", self.predecessor)
        self.repo.git("commit", "--allow-empty", "-m", "side")
        side = self.repo.git("rev-parse", "HEAD").strip()
        self.repo.git("checkout", "-b", "replication-main", self.predecessor)
        self.repo.commit(self.after, "exact edge")
        self.repo.git("merge", "--no-ff", side, "-m", "side merged")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.local_refuses(head, side)

    def test_completed_base_has_no_permanent_test_path_reservation(self):
        head = self.repo.commit({OLD_TEST: self.before[OLD_TEST] + "\n# ordinary historical test edit\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        policy = sc.classify_v2(self.repo.root, self.predecessor, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons, policy.migration_changes),
                         ("none", False, (), ()))
        self.full_refuses(head)
        base = self.head()
        head = self.repo.commit({TEST: self.after[TEST] + "\n# ordinary later test edit\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, base, head), ())
        policy = sc.classify_v2(self.repo.root, base, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons, policy.migration_changes),
                         ("none", False, (), ()))
        self.full_refuses(head)

    def test_original_deadline_history_and_blob_bounds_remain(self):
        head = self.head()
        for call in (sc.check_ci_source, sc.classify_v2):
            clock = [100.0]
            reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")), clock=lambda: clock[0])
            with self.assertRaises(source_git.SourceGitRefusal):
                with reader.operation():
                    clock[0] = reader.deadline
                    call(self.repo.root, self.predecessor, head)
            self.assertTrue(reader.failed and reader.closed)
        original = sc._git
        def huge(root, args):
            if args[:3] == ["rev-list", "--first-parent", "--reverse"]:
                return (b"a" * 40 + b"\n") * (sc.MAX_HISTORY_COMMITS + 1)
            return original(root, args)
        with mock.patch.object(sc, "_git", side_effect=huge):
            self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                             ("classifier-invalid:migration-path",))
        head = self.head({"internal/database/plan.go": " " * (sc.MAX_ARTIFACT_BYTES + 1)})
        # The original dormant-entrypoint reader enforces this bound before
        # the repair guard; retain that precise fail-closed error ownership.
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                         ("ci-source-pr18-observer-transition-required",
                          "classifier-invalid:dormant-entrypoints", "migration-path-changed:closure"))
        self.assertEqual(sc.classify_v2(self.repo.root, self.predecessor, head).reasons,
                         ("classifier-invalid:dormant-entrypoints",))
        self.full_refuses(head)


class ManagedReplicationHistoryReuseTests(unittest.TestCase):
    def test_retained_history_avoids_an_added_scan_and_unrelated_before_probe(self):
        base, head = "a" * 40, "b" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        raw = (head + "\n").encode()
        sc._pr18_run_metadata_remember_history(repository, base, head, raw)
        with mock.patch.object(repository, "_tree", return_value=b"") as tree, \
                mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(sc, "_pr18_managed_replication_before") as before:
            self.assertFalse(sc._pr18_managed_replication_changed(repository, base, head))
            self.assertFalse(sc._pr18_managed_replication_changed(repository, base, head))
        before.assert_not_called()
        self.assertEqual(git.call_args_list, [mock.call(repository.root, ["cat-file", "-t", base])] * 2)
        self.assertIn(mock.call(head, sc._PR18_MANAGED_REPLICATION_MARKER), tree.call_args_list)
        self.assertEqual(repository._run_metadata_history_reads[(base, head)], raw)

    def test_failed_introduction_is_not_cached_and_undo_is_visible(self):
        base, introduced, head = "a" * 40, "b" * 40, "c" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        sc._pr18_run_metadata_remember_history(repository, base, head,
                                             (introduced + "\n" + head + "\n").encode())
        def tree(revision, path):
            return b"present" if (revision, path) == (introduced, sc._PR18_MANAGED_REPLICATION_MARKER) else b""
        with mock.patch.object(repository, "_tree", side_effect=tree), \
                mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(sc, "_pr18_verification_repair_history",
                                  return_value=((base, introduced), (introduced, head))) as history, \
                mock.patch.object(sc, "_pr18_managed_replication_edge", side_effect=sc._Invalid) as edge:
            for _ in range(2):
                with self.assertRaises(sc._Invalid):
                    sc._pr18_managed_replication_changed(repository, base, head)
            self.assertEqual(history.call_count, 2)
            self.assertEqual(edge.call_args_list, [mock.call(repository, base, introduced)] * 2)
        self.assertNotIn((base, head), getattr(repository, "_managed_replication_introductions_checked", set()))

    def test_cached_and_marked_paths_retain_active_reader_failures(self):
        base, head = "a" * 40, "b" * 40
        for cached in (False, True):
            repository = sc._Repository(Path("unused-static-replication-history"))
            if cached:
                repository._managed_replication_introductions_checked = {(base, head)}
            else:
                repository._trees[(base, sc._PR18_MANAGED_REPLICATION_MARKER)] = b"present"
            with mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal("git-failed:source-budget")):
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_changed(repository, base, head)
            if not cached:
                self.assertFalse(getattr(repository, "_managed_replication_introductions_checked", set()))


class ManagedReplicationBeforeReuseTests(unittest.TestCase):
    @staticmethod
    def fixture():
        prior, current = "a" * 40, "b" * 40
        path = "internal/database/plan.go"
        value = b"// reviewed synthetic source\n"
        oid = hashlib.sha1(b"blob " + str(len(value)).encode() + b"\x00" + value).hexdigest()
        tree = b"100644 blob " + oid.encode() + b"\t" + path.encode() + b"\x00"
        repo = sc._Repository(Path("unused-static-replication-before"))
        repo.cache[(prior, path)] = value
        repo._trees[(prior, path)] = tree
        repo.total = len(value)
        return repo, prior, current, path, value, tree

    def test_exact_current_tree_and_both_hashes_reuse_success_with_original_accounting(self):
        repo, prior, current, path, value, tree = self.fixture()
        with mock.patch.object(repo, "_tree", return_value=tree) as read:
            sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest())
        read.assert_called_once_with(current, path)
        self.assertEqual(repo.cache[(current, path)], value)
        self.assertEqual(repo.total, 2 * len(value))
        self.assertEqual(repo.cache[(prior, path)], value)

    def test_wrong_identity_or_poisoned_cache_never_primes_current_preimage(self):
        for variant in ("current-oid", "current-mode", "current-path", "current-absent",
                        "current-malformed", "prior-oid",
                        "prior-mode", "prior-path", "poisoned-bytes", "missing-bytes",
                        "mutable-bytes", "wrong-sha256", "both-wrong-oid"):
            with self.subTest(variant=variant):
                repo, prior, current, path, value, tree = self.fixture()
                digest = hashlib.sha256(value).hexdigest()
                current_tree = tree
                wrong_oid = b"100644 blob " + b"f" * 40 + b"\t" + path.encode() + b"\x00"
                if variant == "current-oid": current_tree = wrong_oid
                elif variant == "current-mode": current_tree = tree.replace(b"100644", b"100755", 1)
                elif variant == "current-path": current_tree = tree.replace(path.encode(), b"wrong/path.go")
                elif variant == "current-absent": current_tree = b""
                elif variant == "current-malformed": current_tree = b"malformed tree row"
                elif variant == "prior-oid": repo._trees[(prior, path)] = wrong_oid
                elif variant == "prior-mode": repo._trees[(prior, path)] = tree.replace(b"100644", b"100755", 1)
                elif variant == "prior-path": repo._trees[(prior, path)] = tree.replace(path.encode(), b"wrong/path.go")
                elif variant == "poisoned-bytes": repo.cache[(prior, path)] = value + b"unreviewed"
                elif variant == "missing-bytes": repo.cache[(prior, path)] = None
                elif variant == "mutable-bytes": repo.cache[(prior, path)] = bytearray(value)
                elif variant == "wrong-sha256": digest = "0" * 64
                elif variant == "both-wrong-oid":
                    current_tree = wrong_oid
                    repo._trees[(prior, path)] = wrong_oid
                with mock.patch.object(repo, "_tree", return_value=current_tree) as read:
                    sc._pr18_managed_replication_reuse_before_blob(repo, current, path, digest)
                read.assert_called_once_with(current, path)
                self.assertNotIn((current, path), repo.cache)
                self.assertEqual(repo.total, len(value))
        # An existing current row is never overwritten as a substitute for the
        # original validator, which still rejects a corrupted current value.
        repo, prior, current, path, value, tree = self.fixture()
        repo.cache[(current, path)] = value + b"unreviewed"
        repo._trees[(current, path)] = tree
        with mock.patch.object(repo, "_tree", return_value=tree) as read:
            sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest())
            read.assert_not_called()
        with self.assertRaises(sc._Invalid):
            sc._pr18_run_metadata_blob(repo, current, path, hashlib.sha256(value).hexdigest())

    def test_per_blob_and_aggregate_bounds_still_refuse_without_cache_entry(self):
        for bound in ("blob", "aggregate"):
            with self.subTest(bound=bound):
                repo, prior, current, path, value, tree = self.fixture()
                if bound == "aggregate": repo.total = 64 * 1024 * 1024
                limit = len(value) - 1 if bound == "blob" else sc.MAX_ARTIFACT_BYTES
                with mock.patch.object(repo, "_tree", return_value=tree), \
                        mock.patch.object(sc, "MAX_ARTIFACT_BYTES", limit):
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_managed_replication_reuse_before_blob(
                            repo, current, path, hashlib.sha256(value).hexdigest())
                self.assertNotIn((current, path), repo.cache)

    def test_before_profile_preserves_active_reader_failure_before_reuse(self):
        repo, prior, current, path, value, tree = self.fixture()
        for code in ("git-failed:source-budget", "git-failed:source-closed"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_before(repo, current)
            reuse.assert_not_called()
            self.assertNotIn((current, path), repo.cache)



class ManagedReplicationProtectedHistoryReuseTests(unittest.TestCase):
    def repository(self, raw=None):
        repo = sc._Repository(Path("/synthetic-repository"))
        base, middle, head = "a" * 40, "b" * 40, "c" * 40
        repo._run_metadata_history_reads = {(base, head):
            ((middle + "\n" + head + "\n").encode() if raw is None else raw)}
        return repo, base, middle, head

    def test_only_six_protected_paths_use_retained_history_after_marker_proof(self):
        repo, base, middle, head = self.repository()
        events = []
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree", return_value=b"present"), \
                mock.patch.object(sc, "_pr18_managed_replication_blob",
                    side_effect=lambda *args: events.append(("marker", args[1:]))) as marker, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob",
                    side_effect=lambda _repo, revision, path, digest: events.append((revision, path, digest))):
            sc._pr18_managed_replication_prime_protected(repo, base, head)
        git.assert_called_once_with(repo.root, ["cat-file", "-t", base])
        marker.assert_called_once_with(repo, head, sc._PR18_MANAGED_REPLICATION_MARKER,
            sc._PR18_MANAGED_REPLICATION_FILES[sc._PR18_MANAGED_REPLICATION_MARKER][1])
        self.assertEqual(events[0][0], "marker")
        self.assertEqual(events[1:], [(revision, path, digest)
            for revision in (base, middle, head) for path, digest in sc._PR18_OBSERVER_PROTECTED.items()])
        self.assertEqual(len(sc._PR18_OBSERVER_PROTECTED), 6)
        self.assertNotIn(sc.DORMANT, sc._PR18_OBSERVER_PROTECTED)

    def test_missing_or_invalid_history_never_traverses_or_primes(self):
        repo, base, middle, head = self.repository()
        variants = (b"", b"x" * (sc.MAX_HISTORY_COMMITS * 41 + 1),
                    (middle + "\n" + middle + "\n" + head + "\n").encode(),
                    (base + "\n" + head + "\n").encode(), (middle + "\n").encode(),
                    b"invalid-oid\n", "not-bytes")
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree") as tree, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            repo._run_metadata_history_reads = {}
            sc._pr18_managed_replication_prime_protected(repo, base, head)
            for raw in variants:
                with self.subTest(raw_kind=type(raw).__name__, size=len(raw)):
                    repo._run_metadata_history_reads = {(base, head): raw}
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_managed_replication_prime_protected(repo, base, head)
            self.assertTrue(all(call.args[1] == ["cat-file", "-t", base] for call in git.call_args_list))
            tree.assert_not_called(); reuse.assert_not_called()

    def test_absent_or_wrong_marker_and_reader_failure_never_prime(self):
        repo, base, middle, head = self.repository()
        with mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(repo, "_tree", return_value=b""), \
                mock.patch.object(sc, "_pr18_managed_replication_blob") as marker, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            sc._pr18_managed_replication_prime_protected(repo, base, head)
            marker.assert_not_called(); reuse.assert_not_called()
        with mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(repo, "_tree", return_value=b"present"), \
                mock.patch.object(sc, "_pr18_managed_replication_blob", side_effect=sc._Invalid), \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            with self.assertRaises(sc._Invalid):
                sc._pr18_managed_replication_prime_protected(repo, base, head)
            reuse.assert_not_called()
        for code in ("git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(repo, "_tree") as tree, \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_prime_protected(repo, base, head)
                tree.assert_not_called(); reuse.assert_not_called()

if __name__ == "__main__":
    unittest.main()
