"""Finite planner public-owner edge; historical blobs are inert Git inputs.

These tests prove source classification, never provider or release acceptance.
"""
import base64
from contextlib import ExitStack, contextmanager
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
import test_pr18_managed_replication_repair as historical

POLICY = 'release/deployment/schema_change.py'
OLD_TEST = 'release/deployment/test_pr18_managed_replication_repair.py'
TEST = 'release/deployment/test_pr18_planner_public_owner_repair.py'
FIXTURE = 'release/deployment/fixtures/pr18-planner-public-owner-repair.json'
SLOT = re.compile(rb"(?m)^_PR18_PLANNER_PUBLIC_OWNER_POLICY_SHA256 = '([0-9a-f]{64})'\n")
HISTORICAL_WRAPPER_SHA256 = '0b0254a11d0518ad43fe23da18391931ddc4fc06e5c3d203e4857148f4e397f7'
SOURCE_PINS = {'cmd/whatomate/migration_plan.go': ('46e40d028a5b341b1ff966c5f51defde6c89d63850093f6aba828bf5ecff5fc8', 'f43155be4be09f3bb6ace6e239dac52d5aeb88590a775a00e89fa5d27d03d325'), 'cmd/whatomate/migration_plan_public_owner_pg_test.go': (None, '6d09d965c5e02edb45882bfe01005499a6b9bf2f112105b8569819c29557bb78'), 'internal/dbcatalog/registry/migration_path.json': ('08618e383eed2ab9eb771918b0ef618efa2ecce8edb99cfd476bd18980938ba6', '08dc20242f40e66f0df7e94ddc4c22472b639136188184f08fba6eaf29bef241')}


class PR18PlannerPublicOwnerRepairTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        historical.PR18ManagedReplicationRepairTests.setUpClass()
        raw = (Path(__file__).parent / "fixtures/pr18-planner-public-owner-repair.json").read_bytes()
        doc = json.loads(raw)
        if (type(doc) is not dict or set(doc) != {"schema_version", "before", "after"}
                or type(doc["schema_version"]) is not int or doc["schema_version"] != 1
                or len(SOURCE_PINS) != 3
                or any(type(doc[side]) is not dict or set(doc[side]) != set(SOURCE_PINS)
                       for side in ("before", "after"))):
            raise AssertionError("independent public-owner fixture schema")
        for path, pair in SOURCE_PINS.items():
            for side, expected in zip(("before", "after"), pair, strict=True):
                value = doc[side][path]
                if expected is None:
                    if value is not None:
                        raise AssertionError("independent public-owner absent preimage")
                elif type(value) is not str or hashlib.sha256(value.encode()).hexdigest() != expected:
                    raise AssertionError("independent public-owner fixture identity")
        wrapper = Path(historical.__file__).read_bytes()
        if hashlib.sha256(wrapper).hexdigest() != HISTORICAL_WRAPPER_SHA256:
            raise AssertionError("independent replication wrapper identity")
        cls.policy_input = Path(sc.__file__).read_bytes()
        cls.before = {**doc["before"],
                      POLICY: historical._historical_replication_input(POLICY).decode(),
                      OLD_TEST: historical._historical_replication_input(OLD_TEST).decode(),
                      TEST: None, FIXTURE: None}
        cls.after = {**doc["after"], POLICY: cls.policy_input.decode(),
                     OLD_TEST: wrapper.decode(), TEST: Path(__file__).read_text(encoding="utf-8"),
                     FIXTURE: raw.decode()}
        if len(cls.after) != 7 or set(cls.before) != set(cls.after):
            raise AssertionError("closed independent seven-path repair")

    def setUp(self):
        self.prior = historical.PR18ManagedReplicationRepairTests()
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
        self.assertIn("pr18-planner-public-owner-repair-incomplete",
                      sc.check_ci_source(self.repo.root, base, head))
        self.assertIn("classifier-v2-invalid:pr18-planner-public-owner-repair",
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
        with mock.patch.object(sc, "_pr18_planner_public_owner_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof, \
                mock.patch.object(sc, "_pr18_managed_replication_edge", wraps=sc._pr18_managed_replication_edge) as replication:
            full = sc.classify_v2(self.repo.root, self.base, head)
            self.assertEqual(proof.call_count, 1)
            contextual = [call for call in replication.call_args_list if call.kwargs]
            self.assertEqual(len(contextual), 1)
            self.assertEqual(contextual[0].args[2], self.predecessor)
            self.assertEqual(contextual[0].kwargs, {"public_owner_predecessor": self.predecessor})
        self.assertEqual((full.kind, full.dormant, full.reasons), ("data", True, ()))
        self.assertEqual(full.migration_changes,
                         self.prior.prior.historical.old.anchor_fixture.MIGRATION_OBSERVATIONS)
        self.assertEqual(local.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(full.requirements, local.requirements)
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        with mock.patch.object(sc, "_pr18_planner_public_owner_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof:
            self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
            self.assertGreaterEqual(proof.call_count, 1)

    def test_deferred_full_origin_cannot_clear_a_failed_final_transition(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.base, head)
        self.assertEqual(legacy.reasons, sc._PR18_OBSERVER_LEGACY)
        with mock.patch.object(sc, "_pr18_planner_public_owner_changed",
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
        path = "cmd/whatomate/migration_plan.go"
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
        for path, value in (("cmd/whatomate/migration_plan.go", "// wrong preimage\n"),
                            (TEST, "# preexisting marker\n")):
            self.repo.checkout(self.predecessor)
            wrong = self.repo.commit({path: value}, "wrong predecessor")
            head = self.repo.commit(self.after, "otherwise exact edge")
            with self.assertRaises(sc._Invalid):
                sc._pr18_planner_public_owner_local(sc._Repository(self.repo.root), wrong, head)
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

    def test_historical_replication_inputs_are_exact_inert_and_bounded(self):
        expected = {'release/deployment/schema_change.py': (146262, 'ad898c247153afaa43da9a78a52ba8739f40900e373bba468f14587790078c12'), 'release/deployment/test_pr18_managed_replication_repair.py': (34304, '023f81db0b9ac67724e799234658ec2bee8001479156737dd55897bb64d72393')}
        for path, (size, digest) in expected.items():
            raw = historical._historical_replication_input(path)
            self.assertEqual((len(raw), hashlib.sha256(raw).hexdigest()), (size, digest))
            self.assertEqual(self.before[path].encode(), raw)
            self.assertNotEqual(self.after[path].encode(), raw)
        size, digest, encoded = historical.HISTORICAL_REPLICATION_INPUTS[POLICY]
        packed = base64.b64decode(encoded, validate=True)
        for variant in ((size - 1, digest, encoded), (True, digest, encoded),
                        (size, "0" * 64, encoded), (size, digest, "!invalid!"),
                        (size, digest, base64.b64encode(packed[:-1]).decode()),
                        (size, digest, base64.b64encode(packed + b"trailing").decode()),
                        (size, digest, base64.b64encode(zlib.compress(b"x" * (size + 10))).decode())):
            with self.subTest(variant_digest=hashlib.sha256(repr(variant).encode()).hexdigest()), \
                    mock.patch.dict(historical.HISTORICAL_REPLICATION_INPUTS, {POLICY: variant}):
                with self.assertRaises(AssertionError):
                    historical._historical_replication_input(POLICY)

class PlannerPublicOwnerHistoryReuseTests(unittest.TestCase):
    def test_retained_history_avoids_an_added_scan_and_unrelated_before_probe(self):
        base, head = "a" * 40, "b" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        raw = (head + "\n").encode()
        sc._pr18_run_metadata_remember_history(repository, base, head, raw)
        with mock.patch.object(repository, "_tree", return_value=b"") as tree, \
                mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(sc, "_pr18_planner_public_owner_before") as before:
            self.assertFalse(sc._pr18_planner_public_owner_changed(repository, base, head))
            self.assertFalse(sc._pr18_planner_public_owner_changed(repository, base, head))
        before.assert_not_called()
        self.assertEqual(git.call_args_list, [mock.call(repository.root, ["cat-file", "-t", base])] * 2)
        self.assertIn(mock.call(head, sc._PR18_PLANNER_PUBLIC_OWNER_MARKER), tree.call_args_list)
        self.assertEqual(repository._run_metadata_history_reads[(base, head)], raw)

    def test_failed_introduction_is_not_cached_and_undo_is_visible(self):
        base, introduced, head = "a" * 40, "b" * 40, "c" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        sc._pr18_run_metadata_remember_history(repository, base, head,
                                             (introduced + "\n" + head + "\n").encode())
        def tree(revision, path):
            return b"present" if (revision, path) == (introduced, sc._PR18_PLANNER_PUBLIC_OWNER_MARKER) else b""
        with mock.patch.object(repository, "_tree", side_effect=tree), \
                mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(sc, "_pr18_verification_repair_history",
                                  return_value=((base, introduced), (introduced, head))) as history, \
                mock.patch.object(sc, "_pr18_planner_public_owner_edge", side_effect=sc._Invalid) as edge:
            for _ in range(2):
                with self.assertRaises(sc._Invalid):
                    sc._pr18_planner_public_owner_changed(repository, base, head)
            self.assertEqual(history.call_count, 2)
            self.assertEqual(edge.call_args_list, [mock.call(repository, base, introduced)] * 2)
        self.assertNotIn((base, head), getattr(repository, "_planner_public_owner_introductions_checked", set()))

    def test_cached_and_marked_paths_retain_active_reader_failures(self):
        base, head = "a" * 40, "b" * 40
        for cached in (False, True):
            repository = sc._Repository(Path("unused-static-replication-history"))
            if cached:
                repository._planner_public_owner_introductions_checked = {(base, head)}
            else:
                repository._trees[(base, sc._PR18_PLANNER_PUBLIC_OWNER_MARKER)] = b"present"
            with mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal("git-failed:source-budget")):
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_planner_public_owner_changed(repository, base, head)
            if not cached:
                self.assertFalse(getattr(repository, "_planner_public_owner_introductions_checked", set()))


class PlannerPublicOwnerProofReuseTests(unittest.TestCase):
    fixture = staticmethod(historical.ManagedReplicationBeforeReuseTests.fixture)

    def test_proof_only_reuses_identity_without_materializing_or_charging(self):
        repo, prior, current, path, value, tree = self.fixture()
        before_cache, before_total = dict(repo.cache), repo.total
        with mock.patch.object(repo, "_tree", return_value=tree) as read:
            result = sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest(), proof_only=True)
        read.assert_called_once_with(current, path)
        self.assertIs(result, value)
        self.assertEqual((repo.cache, repo.total), (before_cache, before_total))
        # Historical callers still materialize and charge the additional row.
        with mock.patch.object(repo, "_tree", return_value=tree):
            sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest())
        self.assertEqual(repo.cache[(current, path)], value)
        self.assertEqual(repo.total, 2 * len(value))

    def test_proof_only_misses_wrong_identity_or_unsuccessful_cache(self):
        for variant in ("current-oid", "current-mode", "current-path", "current-absent",
                        "current-malformed", "prior-oid", "prior-mode", "prior-path",
                        "poisoned-bytes", "missing-bytes", "mutable-bytes", "wrong-sha256", "both-wrong-oid"):
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
                before_cache = dict(repo.cache)
                with mock.patch.object(repo, "_tree", return_value=current_tree) as read:
                    self.assertIsNone(sc._pr18_managed_replication_reuse_before_blob(
                        repo, current, path, digest, proof_only=True))
                read.assert_called_once_with(current, path)
                self.assertEqual(repo.cache, before_cache)
                self.assertEqual(repo.total, len(value))

    def test_existing_current_cache_is_validated_not_replaced(self):
        repo, prior, current, path, value, tree = self.fixture()
        repo.cache[(current, path)] = value + b"unreviewed"
        repo._trees[(current, path)] = tree
        with mock.patch.object(repo, "_tree", return_value=tree):
            self.assertIsNone(sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest(), proof_only=True))
            with self.assertRaises(sc._Invalid):
                sc._pr18_planner_public_owner_blob(repo, current, path, hashlib.sha256(value).hexdigest())
        self.assertEqual(repo.cache[(current, path)], value + b"unreviewed")

    def test_proof_only_keeps_per_blob_and_existing_aggregate_bounds(self):
        for bound in ("blob", "aggregate"):
            repo, prior, current, path, value, tree = self.fixture()
            if bound == "aggregate": repo.total = 64 * 1024 * 1024 + 1
            limit = len(value) - 1 if bound == "blob" else sc.MAX_ARTIFACT_BYTES
            with self.subTest(bound=bound), mock.patch.object(repo, "_tree", return_value=tree), \
                    mock.patch.object(sc, "MAX_ARTIFACT_BYTES", limit):
                with self.assertRaises(sc._Invalid):
                    sc._pr18_managed_replication_reuse_before_blob(
                        repo, current, path, hashlib.sha256(value).hexdigest(), proof_only=True)
            self.assertNotIn((current, path), repo.cache)

    def test_protected_scope_miss_and_postimage_reads_stay_explicit(self):
        repo = sc._Repository(Path("unused-public-owner-proof-scope"))
        revision = "b" * 40
        path, digest = next(iter(sc._PR18_OBSERVER_PROTECTED.items()))
        with mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob", return_value=None) as reuse, \
                mock.patch.object(sc, "_pr18_observer_blob", return_value=b"normal bounded read") as normal:
            self.assertEqual(sc._pr18_planner_public_owner_protected(repo, revision, path, digest),
                             b"normal bounded read")
            reuse.assert_called_once_with(repo, revision, path, digest, proof_only=True)
            normal.assert_called_once_with(repo, revision, path, digest)
            for wrong_path, wrong_digest in ((sc.DORMANT, digest), (path, "0" * 64)):
                with self.assertRaises(sc._Invalid):
                    sc._pr18_planner_public_owner_protected(repo, revision, wrong_path, wrong_digest)
            reuse.reset_mock(); normal.reset_mock()
            sc._pr18_planner_public_owner_blob(repo, revision, path, digest)
            reuse.assert_not_called()
            normal.assert_called_once_with(repo, revision, path, digest)

    def test_new_before_checks_active_reader_before_any_reuse(self):
        repo, prior, current, path, value, tree = self.fixture()
        for code in ("git-failed:source-budget", "git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_planner_public_owner_before(repo, current)
                reuse.assert_not_called()


class PlannerPublicOwnerProtectedHistoryTests(unittest.TestCase):
    repository = historical.ManagedReplicationProtectedHistoryReuseTests.repository

    def test_old_revisions_charge_but_new_marker_revision_defers_to_final_proof(self):
        repo, base, middle, head = self.repository()
        marker = sc._PR18_PLANNER_PUBLIC_OWNER_MARKER
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree", side_effect=lambda revision, path: b"present" if revision == head else b""), \
                mock.patch.object(sc, "_pr18_planner_public_owner_blob") as prove, \
                mock.patch.object(sc, "_pr18_planner_public_owner_protected_predecessor", return_value=middle) as select, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            sc._pr18_planner_public_owner_prime_protected(repo, base, head)
        git.assert_called_once_with(repo.root, ["cat-file", "-t", base])
        self.assertEqual(prove.call_args_list, [mock.call(repo, head, marker,
            sc._PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])] * 2)
        select.assert_called_once_with(repo, base, head, repo._run_metadata_history_reads[(base, head)])
        self.assertEqual(reuse.call_args_list, [mock.call(repo, base, path, digest)
            for path, digest in sc._PR18_OBSERVER_PROTECTED.items()])
        self.assertEqual(len(sc._PR18_OBSERVER_PROTECTED), 6)
        self.assertNotIn(sc.DORMANT, sc._PR18_OBSERVER_PROTECTED)

    def test_invalid_retained_history_never_adds_a_scan_or_primes(self):
        repo, base, middle, head = self.repository()
        variants = (b"", b"x" * (sc.MAX_HISTORY_COMMITS * 41 + 1),
                    (middle + "\n" + middle + "\n" + head + "\n").encode(),
                    (base + "\n" + head + "\n").encode(), (middle + "\n").encode(),
                    b"invalid-oid\n", "not-bytes")
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree") as tree, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            repo._run_metadata_history_reads = {}
            sc._pr18_planner_public_owner_prime_protected(repo, base, head)
            for raw in variants:
                repo._run_metadata_history_reads = {(base, head): raw}
                with self.assertRaises(sc._Invalid):
                    sc._pr18_planner_public_owner_prime_protected(repo, base, head)
            self.assertTrue(all(call.args[1] == ["cat-file", "-t", base] for call in git.call_args_list))
            tree.assert_not_called(); reuse.assert_not_called()

    def test_wrong_marker_or_failed_reader_never_primes(self):
        repo, base, middle, head = self.repository()
        with mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(sc, "_pr18_planner_public_owner_blob", side_effect=sc._Invalid), \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            with self.assertRaises(sc._Invalid):
                sc._pr18_planner_public_owner_prime_protected(repo, base, head)
            reuse.assert_not_called()
        for code in ("git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_planner_public_owner_prime_protected(repo, base, head)
                reuse.assert_not_called()



class PlannerPublicOwnerPredecessorTests(unittest.TestCase):
    def fixture(self):
        repo = sc._Repository(Path("unused-public-owner-predecessor"))
        base, predecessor, head = "a" * 40, "b" * 40, "c" * 40
        marker, policy = sc._PR18_PLANNER_PUBLIC_OWNER_MARKER, sc._PR18_VERIFICATION_REPAIR_POLICY
        raw = (predecessor + "\n" + head + "\n").encode()
        policy_tree = b"100644 blob 9a6e2b136e3a3db7b038ea056e7adaee14192449\t" + policy.encode() + b"\x00"
        trees = {(head, marker): b"pinned marker", (predecessor, policy): policy_tree}
        return repo, base, predecessor, head, raw, trees

    def test_exact_adjacent_predecessor_requires_policy_marker_and_first_parent(self):
        repo, base, predecessor, head, raw, trees = self.fixture()
        with mock.patch.object(repo, "_tree", side_effect=lambda rev, path: trees.get((rev, path), b"")), \
                mock.patch.object(sc, "_git", side_effect=(b"commit\n", (predecessor + "\n").encode())) as git, \
                mock.patch.object(sc, "_pr18_planner_public_owner_blob") as prove:
            self.assertEqual(sc._pr18_planner_public_owner_protected_predecessor(repo, base, head, raw), predecessor)
        self.assertEqual(git.call_args_list, [mock.call(repo.root, ["cat-file", "-t", base]),
            mock.call(repo.root, ["rev-parse", "--verify", head + "^1"])])
        marker, policy = sc._PR18_PLANNER_PUBLIC_OWNER_MARKER, sc._PR18_VERIFICATION_REPAIR_POLICY
        self.assertEqual(prove.call_args_list, [
            mock.call(repo, head, marker, sc._PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1]),
            mock.call(repo, predecessor, policy, sc._PR18_PLANNER_PUBLIC_OWNER_FILES[policy][0]),
            mock.call(repo, head, marker, sc._PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])])
        self.assertEqual(repo.cache, {})
        self.assertEqual(repo.total, 0)

    def test_wrong_policy_marker_parent_or_reintroduction_never_selects(self):
        for variant in ("policy-oid", "policy-mode", "policy-path", "policy-absent",
                        "predecessor-marked", "marker-proof", "policy-proof", "first-parent", "reintroduction"):
            with self.subTest(variant=variant):
                repo, base, predecessor, head, raw, trees = self.fixture()
                marker, policy = sc._PR18_PLANNER_PUBLIC_OWNER_MARKER, sc._PR18_VERIFICATION_REPAIR_POLICY
                key = predecessor, policy
                if variant == "policy-oid": trees[key] = trees[key].replace(b"9a6e2b", b"000000", 1)
                elif variant == "policy-mode": trees[key] = trees[key].replace(b"100644", b"100755", 1)
                elif variant == "policy-path": trees[key] = trees[key].replace(policy.encode(), b"wrong/path")
                elif variant == "policy-absent": trees.pop(key)
                elif variant == "predecessor-marked": trees[(predecessor, marker)] = b"already marked"
                elif variant == "reintroduction":
                    undo, final = "d" * 40, "e" * 40
                    raw = (predecessor + "\n" + head + "\n" + undo + "\n" + final + "\n").encode()
                    trees[(undo, policy)] = trees[key]
                    trees[(final, marker)] = trees[(head, marker)]
                    head = final
                def proof(_repo, revision, path, digest):
                    if variant == "marker-proof" and path == marker:
                        raise sc._Invalid
                    if variant == "policy-proof" and path == policy:
                        raise sc._Invalid
                parent = ("f" * 40 if variant == "first-parent" else predecessor) + "\n"
                with mock.patch.object(repo, "_tree", side_effect=lambda rev, path: trees.get((rev, path), b"")), \
                        mock.patch.object(sc, "_git", side_effect=(b"commit\n", parent.encode())), \
                        mock.patch.object(sc, "_pr18_planner_public_owner_blob", side_effect=proof):
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_planner_public_owner_protected_predecessor(repo, base, head, raw)
                self.assertEqual(repo.cache, {})
        # A range whose marker already exists has no eligible introduction.
        repo, base, predecessor, head, raw, trees = self.fixture()
        trees[(base, sc._PR18_PLANNER_PUBLIC_OWNER_MARKER)] = b"already marked"
        trees[(predecessor, sc._PR18_PLANNER_PUBLIC_OWNER_MARKER)] = b"already marked"
        with mock.patch.object(repo, "_tree", side_effect=lambda rev, path: trees.get((rev, path), b"")), \
                mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(sc, "_pr18_planner_public_owner_blob"):
            self.assertIsNone(sc._pr18_planner_public_owner_protected_predecessor(repo, base, head, raw))
        git.assert_called_once_with(repo.root, ["cat-file", "-t", base])

    def test_selector_rejects_invalid_retained_history_and_active_reader_failure(self):
        repo, base, predecessor, head, raw, trees = self.fixture()
        variants = (None, b"", bytearray(raw), b"x" * (sc.MAX_HISTORY_COMMITS * 41 + 1),
                    (predecessor + "\n" + predecessor + "\n" + head + "\n").encode(),
                    (base + "\n" + head + "\n").encode(), (predecessor + "\n").encode(), b"invalid\n")
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree") as tree, \
                mock.patch.object(sc, "_pr18_planner_public_owner_blob") as prove:
            for value in variants:
                with self.subTest(kind=type(value).__name__), self.assertRaises(sc._Invalid):
                    sc._pr18_planner_public_owner_protected_predecessor(repo, base, head, value)
            self.assertTrue(all(call.args[1] == ["cat-file", "-t", base] for call in git.call_args_list))
            tree.assert_not_called(); prove.assert_not_called()
        for code in ("git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(repo, "_tree") as tree:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_planner_public_owner_protected_predecessor(repo, base, head, raw)
                tree.assert_not_called()


class PlannerPublicOwnerReplicationFixtureTests(unittest.TestCase):
    path = "release/deployment/fixtures/pr18-verification-repair.json"
    digest = "992accd166a964e970977964789f2c93a052b7570334f591356c09d6a9398000"

    @contextmanager
    def isolated_before(self, variant="hit"):
        value = (Path(__file__).parent / "fixtures/pr18-verification-repair.json").read_bytes()
        self.assertEqual(hashlib.sha256(value).hexdigest(), self.digest)
        repo = sc._Repository(Path("unused-public-owner-replication-fixture"))
        prior, current = "a" * 40, "b" * 40
        path = self.path if variant != "wrong-path" else "release/deployment/fixtures/other.json"
        oid = hashlib.sha1(b"blob " + str(len(value)).encode() + b"\x00" + value).hexdigest()
        tree = b"100644 blob " + oid.encode() + b"\t" + path.encode() + b"\x00"
        repo.cache[(prior, path)] = value
        repo._trees[(prior, path)] = tree
        repo.total = len(value)
        if variant == "miss":
            repo.cache.clear(); repo._trees.clear(); repo.total = 0
        elif variant == "poisoned-prior":
            repo.cache[(prior, path)] = value + b"unreviewed"
        elif variant == "poisoned-current":
            repo.cache[(current, path)] = value + b"unreviewed"
            repo._trees[(current, path)] = tree
        current_tree = tree
        if variant == "wrong-mode": current_tree = tree.replace(b"100644", b"100755", 1)
        if variant == "wrong-oid": current_tree = tree.replace(oid.encode(), b"f" * 40, 1)
        calls = []
        def git(_root, args):
            calls.append(args)
            if args == ["cat-file", "-t", current]: return b"commit\n"
            if args[:2] == ["ls-tree", "-r"]: return b""
            if args == ["cat-file", "-s", oid]: return str(len(value)).encode() + b"\n"
            if args == ["cat-file", "blob", oid]: return value
            if variant == "wrong-oid" and args == ["cat-file", "-s", "f" * 40]: return b"10\n"
            if variant == "wrong-oid" and args == ["cat-file", "blob", "f" * 40]: return b"unreviewed"
            raise AssertionError("unexpected bounded test read")
        original = sc._pr18_observer_blob
        def normal(_repo, revision, candidate, digest):
            if candidate == path:
                return original(_repo, revision, candidate, digest)
        verification = dict(sc._PR18_VERIFICATION_REPAIR_FILES)
        if variant in {"wrong-path", "wrong-digest"}:
            pair = verification.pop(self.path)
            verification[path] = (pair[0], "0" * 64 if variant == "wrong-digest" else pair[1])
        with ExitStack() as stack:
            stack.enter_context(mock.patch.object(sc, "_git", side_effect=git))
            stack.enter_context(mock.patch.object(repo, "_tree", side_effect=lambda revision, candidate: current_tree if candidate == path else b""))
            stack.enter_context(mock.patch.object(sc, "_PR18_VERIFICATION_REPAIR_FILES", verification))
            stack.enter_context(mock.patch.object(sc, "_pr18_observer_blob", side_effect=normal))
            checked = stack.enter_context(mock.patch.object(sc, "_pr18_run_metadata_blob", side_effect=normal))
            reuse = stack.enter_context(mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob", wraps=sc._pr18_managed_replication_reuse_before_blob))
            yield repo, current, value, calls, checked, reuse, path

    def test_single_fixture_proof_hit_preserves_default_charge_and_miss_io(self):
        for mode in ("historical", "scoped", "miss"):
            with self.subTest(mode=mode), self.isolated_before("miss" if mode == "miss" else "hit") as fixture:
                repo, current, value, calls, checked, reuse, path = fixture
                before = repo.total
                if mode == "historical": sc._pr18_managed_replication_before(repo, current)
                else: sc._pr18_managed_replication_before(repo, current, public_owner_fixture=True)
                scoped = [call for call in reuse.call_args_list if call.kwargs.get("proof_only")]
                self.assertEqual(scoped, [] if mode == "historical" else [mock.call(repo, current, path, self.digest, proof_only=True)])
                if mode == "scoped":
                    self.assertEqual(repo.total, before)
                    self.assertNotIn((current, path), repo.cache)
                    self.assertFalse(any(call.args[2] == path for call in checked.call_args_list))
                else:
                    self.assertEqual(repo.total, before + len(value))
                    self.assertEqual(repo.cache[(current, path)], value)
                self.assertEqual([args for args in calls if args[:2] == ["cat-file", "blob"]],
                                 [["cat-file", "blob", hashlib.sha1(b"blob " + str(len(value)).encode() + b"\x00" + value).hexdigest()]] if mode == "miss" else [])

    def test_fixture_scope_and_invalid_cache_never_gain_unchecked_reuse(self):
        for variant in ("wrong-path", "wrong-digest", "wrong-mode", "wrong-oid", "poisoned-prior", "poisoned-current"):
            with self.subTest(variant=variant), self.isolated_before(variant) as fixture:
                repo, current, value, calls, checked, reuse, path = fixture
                if variant in {"wrong-digest", "wrong-mode", "wrong-oid", "poisoned-current"}:
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_managed_replication_before(repo, current, public_owner_fixture=True)
                else:
                    sc._pr18_managed_replication_before(repo, current, public_owner_fixture=True)
                    self.assertEqual(repo.cache[(current, path)], value)
                self.assertTrue(any(call.args[2] == path for call in checked.call_args_list))
                if variant in {"wrong-path", "wrong-digest"}:
                    self.assertFalse(any(call.kwargs.get("proof_only") for call in reuse.call_args_list))
                if variant == "poisoned-prior":
                    self.assertTrue(any(args[:2] == ["cat-file", "blob"] for args in calls))

    def test_context_binds_edge_destination_and_preserves_atomic_proof(self):
        repo, base, head = sc._Repository(Path("unused-fixture-edge")), "a" * 40, "b" * 40
        changes = sorted(("A" if old is None else "M", path) for path, (old, _) in sc._PR18_MANAGED_REPLICATION_FILES.items())
        with mock.patch.object(sc, "_pr18_managed_replication_before") as before, \
                mock.patch.object(sc, "changed_paths", return_value=changes), \
                mock.patch.object(sc, "_pr18_managed_replication_blob") as post:
            sc._pr18_managed_replication_edge(repo, base, head)
            before.assert_called_once_with(repo, base)
            before.reset_mock(); post.reset_mock()
            sc._pr18_managed_replication_edge(repo, base, head, public_owner_predecessor=head)
            before.assert_called_once_with(repo, base, public_owner_fixture=True)
            self.assertEqual(len(post.call_args_list), 10)
            for wrong in (True, "", "c" * 40, head.upper()):
                before.reset_mock(); post.reset_mock()
                with self.assertRaises(sc._Invalid):
                    sc._pr18_managed_replication_edge(repo, base, head, public_owner_predecessor=wrong)
                before.assert_not_called(); post.assert_not_called()
        with mock.patch.object(sc, "_pr18_managed_replication_before"), \
                mock.patch.object(sc, "changed_paths", return_value=changes + [("A", "unreviewed.go")]), \
                mock.patch.object(sc, "_pr18_managed_replication_blob") as post:
            with self.assertRaises(sc._Invalid):
                sc._pr18_managed_replication_edge(repo, base, head, public_owner_predecessor=head)
            post.assert_not_called()

    def test_scoped_fixture_retains_active_reader_and_boolean_bounds(self):
        repo, current = sc._Repository(Path("unused-fixture-active-reader")), "b" * 40
        for code in ("git-failed:source-budget", "git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_before(repo, current, public_owner_fixture=True)
                reuse.assert_not_called()
        for wrong in (None, 1, "true"):
            with self.subTest(wrong=wrong), mock.patch.object(sc, "_git") as git:
                with self.assertRaises(sc._Invalid):
                    sc._pr18_managed_replication_before(repo, current, public_owner_fixture=wrong)
                git.assert_not_called()



if __name__ == "__main__":
    unittest.main()
