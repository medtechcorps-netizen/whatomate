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


# Exact 98dbbe2d historical postimages, decoded only as inert Git fixture data.
# The imported current schema_change consumer still evaluates every test.
HISTORICAL_PUBLIC_OWNER_INPUTS = {
    'release/deployment/schema_change.py': (163535, 'e7284885e29fd2df4006b237e9977133699ef023f3237d01bd96e31e7060594a', (
        'eNrsvft/G8eVL/g7/4oOvHcJyCDY3XjTYTK0TDvakS1dSs5MhmKQfkqIQIADgJIVSf/7fs85VdVV/QBA2Zl7Z/fmM2MR3dX1PHXe'
        'j69+d3q/WZ/G8+Vptnzn3X3Yvlkt+0etVusqi9KT1XLxwVtniyzaZF6yiDabeT7P1j3vcbSNFqvXXnR3t5gn0Xa+Wnrzjbdcbb35'
        '7d0iu82W2yz13mTrrHd09HydnSTqi3WWrNbpBv9uo/nS277JvNV6/nq+jBbeJnmT3UZevs6yf2RmkJM4i9bzpfn0KImWXhKt1x+8'
        'dLW+jZZbL1ml2WmK5l7yJlq+zjZd7/2b+SLzbuev1zI7TGj94W41X243XrRMvSjZzt9lR69XizRb6s8wxC3NKl6skrdZ2vOustfz'
        'DT70Hj/xXmfLDJ1l8v27bE17sfHSDBsjgxwli9Xmfp15b6LNm2zzDVaHTbldpfeYivkAS56vPbzZrtYfTtfZu/mGJpisMEPMqke7'
        'f3SUr1e33myW32/R4WxG27pabzE09pgH2xwd6Wfr13fRepPp3+k8zxfzWP+kyVg//77BRNXfa/PN5sNGhryLttRaj/ccP+XF9sMd'
        'HYJ6frH80PWebLEh8SLrej9Ft1n68h4nf3SErnrUS2++3GTrbdvvetjCNvXUxoJwLLNZp7fONqvFu6zdQds1DqfTKSbF8DaT4zYT'
        'fDO/myWr21tsVbTx5C/98i5bppjc7G4RmWcCTLPl/WJBcyw9ni/T7JfSMwwsx+g+3mZLwNjsbr2iuR8dHf3w88XVd5ffzV5eXV6+'
        '8M69dgtQla0BwQyDMSZ/2up6xVNAQLbYnLY65tPvnzwtfwoITBfZenMKWNzGq+0sX6zezwwA916vWl2rg59/evzyybOfuJMjD/9r'
        't5Lb9PQ9Pl7dAkpPCZD5I6+1vl9ePX3xo+6q1enu+4KB9YP90dX9cju/zQ75NlrMsQ/Zi2yNbkwHP2ITDvh6nf3n/XydpS+20Xp7'
        'f/ed2tDH6nYcPPe935uNp7u/zBani+x1lHyY3QIxqZ6+jZK3OPPFU37zbxhsc3F392QZr36hfjpHPz754eqCjqH2PKpQcbfabF8D'
        '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'
        '7gy71T3u1m5Gt26yxcBKBYohK6DarYJVt3Tg3erJdJs2z9pnpeA3272kSottSSpjZXJTeX8piZtMs+uVUoFXkwB3C57ZIQKU6/fA'
        'ZM+sly+lBSjPWCXjOSmVDK29mjYL7+YNdGBGEIlSu1gJ6K10bk7qfzuNcumVSlpx6HpLpm5rwDJEV2ZXzgzQ4PdoJ79qaHJWE3No'
        '9lAs0idb8S+ttFQ7Kq10ejKTNKe8xUqhpcvJW8vTb3ZNpZTwqGku0kzPRX7VzMXSqVWnY72sxjc7p9/esQPdXVNyOy6W6QK80+gr'
        '71u2JUAZ6+kau6v1qbapyJSUxohLf76DHYQKdYoSefNNqTdihYsAHsWqF3wAqYRESHq/LAQlih+TRMWb3p6kmQ8teeCgDeuu1joj'
        'uOWxVZgjHS0pKeqCZyxVTm3hUkdXcGiRbCpoU2lbaDhsF5pdiKk5nMkog61EnkWfzZFLu7/bf8GdjFl1EyuU006S0ErirLrJ7fh2'
        '70VQ9uFzo88sYbUiRUy75H29N6ViLUdZSjhpDVzL4O2YQT0z8YCp2Em3rXlUWb4dk6jhXR4wg3L2wvNqXsMD51HPMDVM5ZFdY6F2'
        'YqX8jsW8Gmhf07Rqmv+KWemYWrIuVioP6CRNdqEBu8KA8aU3gaaum/yBnmHK/6Uhk/v5zszu506a98PGK1cV2JMb/rwuVfzOXO1F'
        '9oiGjJ5nO7NdHpbLsxYOamG2mqDTnVldms5Sis4mVqDMzitle6tCdusM63vl40pvxmZWh+6VJblr8kaU+OQGTL/jM2KVizjyhoB5'
        'O0tCXZOSsFDct5o8IHVuO+hzfnt/60m/pvYbF7gthfbTLVXbx4zfb5q2kmMSi6SVzXdtb/LKzy4d/qfNmIMlf8WMC0RnzVh5upzX'
        'pFRRqLfruXjTrE+lBNS/uwYYDB7tOOUpq+Z2A6taGKlnOI0jpGY6dQCm9gN0pV8Dks25HcpWqqJdt1RVUrIyF0Ys7pU0+ft65DZS'
        '+kP22O3SmWVtdpxKj6pVeYZ2Z7/T+RyN7lEZCJsZeRUcpbf2vcSEnNh+UK52QUEMGWzoCjn5UnwVe19+vMPL1ZEmxJH2dHfy0CpG'
        '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'
    )),
    'release/deployment/test_pr18_planner_public_owner_repair.py': (49457, 'f83cbeef1683161b02a82e797af9faf3eaa2012169e37478549ea8217ee3dbfd', (
        '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'
    )),
}

def _historical_public_owner_input(path):
    size, expected, encoded = HISTORICAL_PUBLIC_OWNER_INPUTS[path]
    if (type(size) is not int or not 0 < size <= 163535
            or type(encoded) is not str or len(encoded) > 4 * 163535):
        raise AssertionError("independent historical public-owner input bounds")
    try:
        packed = base64.b64decode(encoded, validate=True)
        decoder = zlib.decompressobj()
        raw = decoder.decompress(packed, size + 1)
    except (ValueError, zlib.error) as exc:
        raise AssertionError("independent historical public-owner input encoding") from exc
    if (len(raw) != size or not decoder.eof or decoder.unused_data or decoder.unconsumed_tail
            or hashlib.sha256(raw).hexdigest() != expected):
        raise AssertionError("independent historical public-owner input identity")
    return raw


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
        cls.policy_input = _historical_public_owner_input(POLICY)
        cls.before = {**doc["before"],
                      POLICY: historical._historical_replication_input(POLICY).decode(),
                      OLD_TEST: historical._historical_replication_input(OLD_TEST).decode(),
                      TEST: None, FIXTURE: None}
        cls.after = {**doc["after"], POLICY: cls.policy_input.decode(),
                     OLD_TEST: wrapper.decode(), TEST: _historical_public_owner_input(TEST).decode(),
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

    def test_historical_public_owner_inputs_are_exact_inert_and_bounded(self):
        expected = {'release/deployment/schema_change.py': (163535, 'e7284885e29fd2df4006b237e9977133699ef023f3237d01bd96e31e7060594a'), 'release/deployment/test_pr18_planner_public_owner_repair.py': (49457, 'f83cbeef1683161b02a82e797af9faf3eaa2012169e37478549ea8217ee3dbfd')}
        for path, (size, digest) in expected.items():
            raw = _historical_public_owner_input(path)
            self.assertEqual((len(raw), hashlib.sha256(raw).hexdigest()), (size, digest))
            self.assertEqual(self.after[path].encode(), raw)
            current = Path(sc.__file__) if path == POLICY else Path(__file__)
            self.assertNotEqual(current.read_bytes(), raw)
        match, = SLOT.finditer(self.policy_input)
        canonical = self.policy_input[:match.start(1)] + b"0" * 64 + self.policy_input[match.end(1):]
        self.assertEqual(match.group(1).decode(), '9a4305867ee9755dacade389dca70dc1f8abde438f64ebddc5396a4330d71e13')
        self.assertEqual(hashlib.sha256(canonical).hexdigest(), '9a4305867ee9755dacade389dca70dc1f8abde438f64ebddc5396a4330d71e13')
        size, digest, encoded = HISTORICAL_PUBLIC_OWNER_INPUTS[POLICY]
        packed = base64.b64decode(encoded, validate=True)
        variants = (
            (size - 1, digest, encoded),
            (163536, digest, encoded),
            (True, digest, encoded),
            (size, "0" * 64, encoded),
            (size, digest, base64.b64encode(packed[:-1]).decode()),
            (size, digest, base64.b64encode(packed + b"trailing").decode()),
            (size, digest, base64.b64encode(zlib.compress(b"x" * (size + 10))).decode()),
            (size, digest, "!invalid-base64!"),
            (size, digest, "A" * (4 * 163535 + 1)),
        )
        for index, variant in enumerate(variants):
            with self.subTest(decoder_variant=index), \
                    mock.patch.dict(HISTORICAL_PUBLIC_OWNER_INPUTS, {POLICY: variant}):
                with self.assertRaises(AssertionError):
                    _historical_public_owner_input(POLICY)

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
