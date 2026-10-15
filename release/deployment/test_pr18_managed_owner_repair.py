"""Independent finite managed-owner inputs and genuine bounded-source histories.

Saved historical policy bytes are commit inputs only. The imported current
consumer is trusted by external source review; these tests never execute the
policy saved inside the synthetic repository or invent real release evidence.
"""
import hashlib
import json
from pathlib import Path
import re
import shutil
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import schema_change as sc
import source_git
import record_resolver
import test_pr18_verification_repair as historical
import test_ship_support as support

POLICY = "release/deployment/schema_change.py"
OLD_TEST = "release/deployment/test_pr18_verification_repair.py"
TEST = "release/deployment/test_pr18_managed_owner_repair.py"
FIXTURE = "release/deployment/fixtures/pr18-managed-owner-repair.json"
SOURCE_PINS = {'cmd/whatomate/migration_plan.go': ('43bbdc7d9733e5cd0fa76a1129cab1ed70dd70be7d516061278cdd15e17e9dde',
                                     '46e40d028a5b341b1ff966c5f51defde6c89d63850093f6aba828bf5ecff5fc8'),
 'cmd/whatomate/migration_plan_managed_owner_admission_pg_test.go': (None,
                                                                     '1f65a0b7da0d8b0637416e64bf061f7fd7fbdbcf27dd95799c06ea1fb04b6abb'),
 'cmd/whatomate/migration_plan_managed_owner_pg_test.go': (None,
                                                           'b8cc9cc0090e03a06d657f1af5d70bf97c60dabd8dd0f6e65aa211007bd8d5d4'),
 'docs/catalog.md': ('7fec0d54292d16d617eb5b596b539f1ffad55091f1d88b9b5b3b9af235eb7fd7',
                     '29f182362f3128732ea55f6a3ba43dcc099e84fb2f1e3b3a599db4b3cbe115dc'),
 'internal/database/data_gates.go': ('fdd250d045ba4676262d8e803df14645d8bfd896906a15c12956252facf6513d',
                                     'b8cb4b1b608f87069e2724ae29d4b6b38bcdbcf85e5c2bf085734015902953cd'),
 'internal/database/plan.go': ('91ab1d7114a42d4166aa88b632d22c56291da2a39d265bba3c12770cf091d3ee',
                               '83d6f4e83ced0c9a02f3ce18470e3da54914cb63c77f70f9a8fda7cb44c68365'),
 'internal/database/plan_test.go': ('dabb7a6982cbd7b066fa5b41e83d06f9de5e64798e251389395e4351b29f13cb',
                                    '083ed1514dda2ddb0d7ef5081fd471e1de34412ebba44f64fb954dcc7ca7f763'),
 'internal/dbcatalog/registry/migration_path.json': ('bde5e0c8ec946d37d60727ba6b20ede20ba6be19321e103dfe963d2615cf072e',
                                                     '4acecb4e3caed65bb71f8d61ea2b3e760129523bfc599130fa3aee5ed421969d')}
HISTORICAL_WRAPPER_SHA256 = 'eed47103a70d533d7d8e2687111025ff469bd05f264d9e9bbbe0defa826fc9e1'
SLOT = re.compile(rb"(?m)^_PR18_MANAGED_OWNER_POLICY_SHA256 = '([0-9a-f]{64})'\n")


class PR18ManagedOwnerRepairTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        historical.PR18VerificationRepairTests.setUpClass()
        cls.old = historical.PR18VerificationRepairTests
        raw = (Path(__file__).parent / "fixtures/pr18-managed-owner-repair.json").read_bytes()
        doc = json.loads(raw)
        if (type(doc) is not dict or set(doc) != {"schema_version", "before", "after"}
                or type(doc["schema_version"]) is not int or doc["schema_version"] != 1
                or len(SOURCE_PINS) != 8
                or any(type(doc[side]) is not dict or set(doc[side]) != set(SOURCE_PINS)
                       for side in ("before", "after"))):
            raise AssertionError("independent managed-owner fixture schema")
        for path, pair in SOURCE_PINS.items():
            for side, expected in zip(("before", "after"), pair, strict=True):
                value = doc[side][path]
                digest = hashlib.sha256(value.encode()).hexdigest() if type(value) is str else None
                if (value is not None and type(value) is not str) or digest != expected:
                    raise AssertionError("independent managed-owner fixture identity")
        wrapper = Path(historical.__file__).read_bytes()
        if hashlib.sha256(wrapper).hexdigest() != HISTORICAL_WRAPPER_SHA256:
            raise AssertionError("independent historical input wrapper identity")
        cls.policy_input = Path(sc.__file__).read_bytes()
        cls.fixture = doc
        cls.after = {**doc["after"], POLICY: cls.policy_input.decode(),
                     OLD_TEST: wrapper.decode(), TEST: Path(__file__).read_text(encoding="utf-8"),
                     FIXTURE: raw.decode()}
        if len(cls.after) != 12:
            raise AssertionError("independent managed-owner twelve-path inventory")

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.repo = support.TempRepo(Path(temp.name) / "repo")
        files = {**self.old.anchor_fixture.before, **self.old.repair_before}
        # Only paths absent from both historical edge inventories belong in
        # their unchanged prefix. Never preinstall an observer-added Go file.
        for path, value in self.fixture["before"].items():
            if path not in self.old.anchor_fixture.SOURCE and path not in self.old.repair_after:
                if value is not None:
                    files[path] = value
        self.base = self.repo.commit(files, "independent historical baseline")
        self.anchor = self.repo.commit(self.old.anchor_fixture.after, "exact original observer31")
        self.repaired = self.repo.commit(self.old.repair_after, "exact historical repair38")
        for path, value in self.fixture["before"].items():
            actual = self.repo.root / path
            self.assertEqual(actual.read_text(encoding="utf-8") if actual.is_file() else None, value)

    def head(self, extra=None):
        self.repo.checkout(self.repaired)
        return self.repo.commit({**self.after, **(extra or {})}, "one finite managed-owner correction")

    def local_refuses(self, head, base=None):
        base = base or self.repaired
        self.assertIn("pr18-managed-owner-repair-incomplete", sc.check_ci_source(self.repo.root, base, head))
        self.assertIn("classifier-v2-invalid:pr18-managed-owner-repair", sc.classify_v2(self.repo.root, base, head).reasons)

    def full_refuses(self, head):
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))
        self.assertIn("migration-call:cmd/whatomate/main.go", sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_exact_local_and_full_origin_are_distinct_truthful_results(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.repaired, head)
        self.assertEqual((legacy.kind, legacy.dormant, legacy.reasons),
                         ("none", False, ("migration-path-changed:closure",)))
        local = sc.classify_v2(self.repo.root, self.repaired, head)
        self.assertEqual((local.kind, local.dormant, local.reasons), ("none", False, ()))
        self.assertEqual(local.migration_changes, ("generated migration closure changed",))
        full = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((full.kind, full.dormant, full.reasons), ("data", True, ()))
        self.assertEqual(full.migration_changes, self.old.anchor_fixture.MIGRATION_OBSERVATIONS)
        for base in (self.base, self.repaired):
            self.assertEqual(sc.check_ci_source(self.repo.root, base, head), ())
        self.assertEqual(full.requirements, ("compat-required", "generated-migration-closure-ci"))

    def test_each_missing_input_and_wrong_product_bytes_refuse(self):
        for path in self.after:
            with self.subTest(missing=path):
                prior = self.fixture["before"].get(path)
                if path == POLICY: prior = self.old.policy_input.decode()
                if path == OLD_TEST: prior = self.old.repair_after[OLD_TEST]
                head = self.head({path: prior})
                self.local_refuses(head); self.full_refuses(head)
        head = self.head({"internal/database/plan.go": self.after["internal/database/plan.go"] + "\n// not reviewed\n"})
        self.local_refuses(head); self.full_refuses(head)

    def test_stale_generated_metadata_and_protected_source_refuse(self):
        path = "internal/dbcatalog/registry/migration_path.json"
        head = self.head({path: self.fixture["before"][path]})
        self.local_refuses(head); self.full_refuses(head)
        for path in ("internal/dbcatalog/registry/data_steps.json", "internal/dbcatalog/golden/seeds.json",
                     "internal/dbcatalog/dormant_entrypoints.json"):
            self.repo.checkout(self.repaired)
            value = (self.repo.root/path).read_text(encoding="utf-8") + "\n"
            head = self.head({path:value})
            self.local_refuses(head); self.full_refuses(head)

    def test_split_extra_suffix_and_reapply_cannot_launder_edge(self):
        path = "internal/database/plan.go"
        self.repo.commit({path:self.after[path]}, "partial")
        head = self.repo.commit(self.after, "remaining")
        self.local_refuses(head); self.full_refuses(head)
        head = self.head({"README.md":"unreviewed extra\n"})
        self.local_refuses(head); self.full_refuses(head)
        head = self.head()
        head = self.repo.commit({"README.md":"nonempty suffix\n"})
        self.local_refuses(head); self.full_refuses(head)
        self.repo.checkout(self.repaired)
        self.repo.commit({path:self.after[path]}, "partial")
        self.repo.commit({path:self.fixture["before"][path]}, "undo")
        head = self.repo.commit(self.after, "reapply")
        self.local_refuses(head); self.full_refuses(head)

    def test_complete_undo_net_zero_remains_observed(self):
        self.head()
        undo = {**self.fixture["before"], POLICY:self.old.policy_input.decode(),
                OLD_TEST:self.old.repair_after[OLD_TEST], TEST:None, FIXTURE:None}
        head = self.repo.commit(undo, "complete undo")
        self.assertEqual(self.repo.git("diff", "--name-only", self.repaired, head), "")
        self.local_refuses(head); self.full_refuses(head)

    def test_noop_suffix_is_allowed_but_wrong_mode_is_not(self):
        head = self.head()
        self.repo.git("commit", "--allow-empty", "-m", "noop")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.assertEqual(sc.check_ci_source(self.repo.root,self.repaired,head), ())
        self.assertFalse(sc.classify_v2(self.repo.root,self.base,head).reasons)
        self.repo.checkout(self.repaired)
        for path, value in self.after.items():
            dest = self.repo.root/path; dest.parent.mkdir(parents=True,exist_ok=True); dest.write_text(value,encoding="utf-8",newline="\n")
        self.repo.git("add", "-A")
        self.repo.git("update-index", "--chmod=+x", "internal/database/plan.go")
        self.repo.git("commit", "-m", "wrong mode")
        head = self.repo.git("rev-parse","HEAD").strip()
        self.local_refuses(head); self.full_refuses(head)

    def test_policy_slot_cannot_self_authorize_changed_consumer(self):
        match, = SLOT.finditer(self.policy_input)
        body = self.policy_input + b"\n# unreviewed policy extension\n"
        normalized = body[:match.start(1)] + b"0"*64 + body[match.end(1):]
        recomputed = body[:match.start(1)] + hashlib.sha256(normalized).hexdigest().encode() + body[match.end(1):]
        for value in (body, recomputed, self.policy_input+match.group(0),
                      self.policy_input[:match.start(1)]+b"f"*64+self.policy_input[match.end(1):]):
            with self.subTest(digest=hashlib.sha256(value).hexdigest()):
                head=self.head({POLICY:value.decode()}); self.local_refuses(head); self.full_refuses(head)

    def test_unrepaired_or_reordered_managed_edge_refuses_full_origin(self):
        self.repo.checkout(self.anchor)
        head=self.repo.commit(self.after, "managed before historical repair")
        self.full_refuses(head)
        self.repo.checkout(self.base)
        self.repo.commit(self.after, "managed before observer")
        head=self.repo.commit(self.old.anchor_fixture.after,"observer after managed")
        self.full_refuses(head)

    def test_second_parent_base_is_not_the_required_chain(self):
        self.repo.git("checkout", "-b", "managed-side", self.repaired)
        self.repo.git("commit", "--allow-empty", "-m", "side")
        side=self.repo.git("rev-parse","HEAD").strip()
        self.repo.git("checkout", "-b", "managed-main", self.repaired)
        head=self.repo.commit(self.after,"exact edge")
        self.repo.git("merge", "--no-ff", side, "-m", "side merged")
        merged=self.repo.git("rev-parse","HEAD").strip()
        self.local_refuses(merged,side)

    def test_real_reader_deadline_and_history_bound_fail_closed(self):
        head=self.head()
        for call in (sc.check_ci_source,sc.classify_v2):
            clock=[100.0]
            reader=source_git.ImmutableGit(self.repo.root,Path(shutil.which("git")),clock=lambda:clock[0])
            with self.assertRaises(source_git.SourceGitRefusal):
                with reader.operation():
                    clock[0]=reader.deadline
                    call(self.repo.root,self.repaired,head)
            self.assertTrue(reader.failed and reader.closed)
        original=sc._git
        def huge(root,args):
            if args[:3]==["rev-list","--first-parent","--reverse"]:
                return (b"a"*40+b"\n")*(sc.MAX_HISTORY_COMMITS+1)
            return original(root,args)
        with mock.patch.object(sc,"_git",side_effect=huge): self.local_refuses(head)

    def test_oversized_new_product_blob_retains_original_artifact_cap(self):
        head=self.head({"internal/database/plan.go":" "*(sc.MAX_ARTIFACT_BYTES+1)})
        self.local_refuses(head)
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))
        self.assertEqual(sc.classify_v2(self.repo.root, self.base, head).reasons,
                         ("classifier-invalid:dormant-entrypoints",))

    def test_resolver_uses_real_signed_origin_classification_not_local_clean_result(self):
        head=self.head()
        latest=SimpleNamespace(record_sha=self.repaired,live=SimpleNamespace(origin_source=SimpleNamespace(sha=self.base)))
        def resolve(candidate):
            reader=source_git.ImmutableGit(self.repo.root,Path(shutil.which("git")))
            with reader.operation():
                value=record_resolver._forward_source(reader,self.repo.root,latest,SimpleNamespace(sha=candidate))
            self.assertTrue(reader.closed)
            return value
        value=resolve(head)
        self.assertEqual(value[3],self.base)
        self.assertEqual((value[1].kind,value[1].dormant,value[1].reasons),("data",True,()))
        later=self.repo.commit({"README.md":"local clean but unqualified full history\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root,head,later),())
        self.assertIn("migration-call:cmd/whatomate/main.go",resolve(later)[1].reasons)


if __name__ == "__main__":
    unittest.main()
