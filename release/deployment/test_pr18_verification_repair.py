"""Finite PR18 repair histories under the externally reviewed trusted consumer.

The original PR18 fixture remains its own independent oracle. The additional
19-path fixture is checked against literal reviewed source pins. Current policy,
this test and fixture bytes are INPUTS to synthetic commits, not an oracle for
policy acceptance. Expected public outcomes are written explicitly below;
actual range/source qualification remains external to these tests.
"""
import hashlib
import json
from pathlib import Path
import re
import shutil
import tempfile
import unittest

import schema_change as sc
import source_git
import test_ship_schema_change_v2 as original
import test_ship_support as support

POLICY = "release/deployment/schema_change.py"
TEST = "release/deployment/test_pr18_verification_repair.py"
FIXTURE = "release/deployment/fixtures/pr18-verification-repair.json"
POLICY_BEFORE_SHA256 = "832d346aeb377f9b54358b69f8fad74146927c20a6957ce66bcfdc797a06ba86"
# Literal reviewed source pins from the frozen 19-path repair packet.
# No policy/test/fixture AFTER hash belongs here.
REPAIR_SOURCE_PINS = {'.github/workflows/ship.yml': ('30663f4c8c3978d6a504e294a57d58cd66af76b2375d76ba2658623ab1613f6a',
                                '36b18f4ebe049755ced6693b93c755507c75ce9763da72ccba976168b980a74f'),
 'release/deployment/compat_collect.py': ('42694ecff0cd35013eac2c85f84442f483489d1bb345ca3fc3347fb3d73b4c7b',
                                          '8bf796595a577e92c7e8f45f49131fb099836ef52e05b41f86775eb8754acc3d'),
 'release/deployment/record_resolver.py': ('9367358d7db9fffb2507d785d0256135c14e6a87590d67ab2030137d2b467c52',
                                           '3a4cd096b701ca92ac8023a834de320891620a889d1314ec90a85fd9b70e6d67'),
 'release/deployment/record_transport.py': ('125764da4008a029a07f13b69a19958a951834617085d3a1f2317f63649a14f3',
                                            'a683969db650e819fa7a4bc79d2d12dd948cedb75a715489e12ff6f6d4a49fd0'),
 'release/deployment/ship.py': ('d2d3f4c413f907293fc0f591cb18e291be1143f2ae676bb849abb5b678ae238d',
                                '91d134d2af2044ccd6679cb6b9c51ffc6926c5de0c52cfdd13a41e5ce097b921'),
 'release/deployment/ship_compat.py': ('946a7d3c0e30c96deae43e45640bd34b00e04e9720677bef314815210e1ea836',
                                       'f3db82a76e9ca8d8f4b6f44053649dcf0cc872cae360f770f141b6eebd19362c'),
 'release/deployment/ship_flow.py': ('14acc4ff9a5f1fab8cb927d61c817127efd5a781f391c5581eff32c752c0972a',
                                     'bd259294554413dd36a9c7143d07485393698e75575c46c819ffb132c1dc12ab'),
 'release/deployment/ship_outage_compat.py': ('d03ef4387fe9e9a54a34a7db13584ac5e35254ca26781fcaead9d7e7f88e9a8a',
                                              'a4d472659f2d200b9ba6f81782b16db1fcde2dc772270729f7147bd96630edee'),
 'release/deployment/ship_rollback_compat.py': ('c31bd6cd7208d5ec194b1e5212a68d02a8a2d4563bc3265b00ecbe988793b47f',
                                                '75dcf78d8e50a3b0f6a33c2e90fa8e199ce4aa52c8258ea4f12e720906f37482'),
 'release/deployment/source_git.py': ('bb3fc7dd032aac31683a1afe442c28107f4b85b0382ebd695b308ac88be1fcac',
                                      'e2b47e5be4ad1c774e3d6587008d2d98b0931e7c77b8e94630485a13b502de73'),
 'release/deployment/test_record_resolver.py': ('fb59fb0ee39fe3acd5f15c5e56c975bf5275e4de5d5c5ea31f6f9c7330ac85fb',
                                                'd99f208bdc3ef18fe4130e1ac5a346e8f20f00633727b8ea50b1e76e4769a57a'),
 'release/deployment/test_record_transport.py': ('c411ddcad55c18d7064add4b2d7f89a2f75d10c970dc3701854db7ad231a4117',
                                                 '4c87aba34c30d05bc0dc95624155fb4f841b72d44c54d2c86be08d136080894c'),
 'release/deployment/test_record_transport_asset_retry.py': (None,
                                                             '3b76939cb1f20c8841e7cab4f5293439fb88530990b90682db3c9e5fe3867bcf'),
 'release/deployment/test_ship_compat.py': ('d4a7b4559a8b2fc074fadc79196acb0722f2a70397fd1f0a34ad9a5a60c12b5d',
                                            'd750579985000e9cbc3cf88163b8aab2a90f9114a70f43dfa2c5471f8ebae7ac'),
 'release/deployment/test_ship_outage_compat.py': ('2f41cbbb365391c1837aee705ba7323d7c8b689ad9045e002f0739844c766495',
                                                   '7c784099e2b0a07d696fca19323db40755fef2b2adf5b7b3435f507253fabcce'),
 'release/deployment/test_ship_rollback_compat.py': ('8170c47ee1e6774cbdb78295f6bf38c108d25a7612b47b6648e667f5af7b9230',
                                                     '6e4d5b977de0b789abbb78ccba6c8319c3b943a4077e156396a48f7bc9229841'),
 'release/deployment/test_ship_workflow.py': ('92c6292826a3c10f8f880387e83ad16e9fee2623cdc77ac0d95abd0a65408cf3',
                                              'b86143a5038dabdfc7f44658f946523d22c74d4a73f5cd6b58295c13a982121d'),
 'release/deployment/test_source_git.py': ('ef1a0d3b29e02f8b614b70e2334beecf6fdad7c04e1653497795fd8dbf933103',
                                           '6ea0637697cecfafabb9d48134c1dae66e0e4f6129cbff49905ac522452f6d22'),
 'release/deployment/test_verification_diagnostics.py': (None,
                                                         '5c513c539c17a6fd8845dbce1c342deba918d2fa812c1b45d504e5647181be37')}
_SLOT = re.compile(rb"(?m)^_PR18_VERIFICATION_REPAIR_POLICY_SHA256 = '([0-9a-f]{64})'\n")


class PR18VerificationRepairTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        original.PR18ObserverTransitionTests.setUpClass()
        cls.anchor_fixture = original.PR18ObserverTransitionTests
        if type(REPAIR_SOURCE_PINS) is not dict or len(REPAIR_SOURCE_PINS) != 19:
            raise AssertionError("unbound reviewed repair fixture pins")
        raw = (Path(__file__).resolve().parent / "fixtures/pr18-verification-repair.json").read_bytes()
        fixture = json.loads(raw)
        if (type(fixture) is not dict or set(fixture) != {"schema_version", "before", "after"}
                or type(fixture["schema_version"]) is not int or fixture["schema_version"] != 1
                or type(fixture["before"]) is not dict or type(fixture["after"]) is not dict
                or set(fixture["before"]) != set(REPAIR_SOURCE_PINS) | {POLICY}
                or set(fixture["after"]) != set(REPAIR_SOURCE_PINS)):
            raise AssertionError("closed repair fixture schema")
        for path, pair in REPAIR_SOURCE_PINS.items():
            if type(pair) is not tuple or len(pair) != 2:
                raise AssertionError("literal repair pin pair")
            for side, expected in zip(("before", "after"), pair, strict=True):
                value = fixture[side][path]
                digest = hashlib.sha256(value.encode("utf-8")).hexdigest() if type(value) is str else None
                if (value is not None and type(value) is not str) or digest != expected:
                    raise AssertionError("independent repair source identity")
        if hashlib.sha256(fixture["before"][POLICY].encode()).hexdigest() != POLICY_BEFORE_SHA256:
            raise AssertionError("independent policy preimage")
        if set(REPAIR_SOURCE_PINS) & set(cls.anchor_fixture.SOURCE):
            raise AssertionError("repair changes original product anchor")
        cls.repair_before = fixture["before"]
        cls.policy_input = Path(sc.__file__).read_bytes()
        cls.repair_after = {**fixture["after"], POLICY: cls.policy_input.decode("utf-8"),
            TEST: Path(__file__).read_bytes().decode("utf-8"), FIXTURE: raw.decode("utf-8")}
        if len(cls.repair_after) != 22 or TEST in fixture["before"] or FIXTURE in fixture["before"]:
            raise AssertionError("closed repair after inventory")

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.repo = support.TempRepo(Path(temp.name) / "repo")
        self.base = self.repo.commit({**self.anchor_fixture.before, **self.repair_before}, "independent S1 plus repair preimages")

    def anchor(self):
        self.repo.checkout(self.base)
        return self.repo.commit(self.anchor_fixture.after, "original exact31 PR18 anchor")

    def repaired(self, changes=None):
        anchor = self.anchor()
        head = self.repo.commit({**self.repair_after, **(changes or {})}, "one exact22 verification repair")
        return anchor, head

    def data(self, head):
        value = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((value.kind, value.dormant, value.reasons), ("data", True, ()))
        self.assertEqual(value.requirements, self.anchor_fixture.REQUIREMENTS)
        self.assertEqual(value.migration_changes, self.anchor_fixture.MIGRATION_OBSERVATIONS)

    def full_refuses(self, head):
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))
        value = sc.classify_v2(self.repo.root, self.base, head)
        self.assertTrue(value.reasons)

    def local_refuses(self, anchor, head):
        self.assertIn("pr18-verification-repair-incomplete", sc.check_ci_source(self.repo.root, anchor, head))
        self.assertIn("classifier-v2-invalid:pr18-verification-repair",
                      sc.classify_v2(self.repo.root, anchor, head).reasons)

    def test_original_pr18_anchor_is_unchanged_and_still_admitted(self):
        head = self.anchor()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.data(head)
        self.assertEqual(sc.check(self.repo.root, self.base, head), self.anchor_fixture.LEGACY_REASONS)

    def test_exact_full_origin_and_local_repair_have_distinct_truthful_classifications(self):
        anchor, head = self.repaired()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.data(head)
        self.assertEqual(sc.check_ci_source(self.repo.root, anchor, head), ())
        value = sc.classify_v2(self.repo.root, anchor, head)
        self.assertEqual((value.kind, value.dormant, value.reasons), ("none", False, ()))
        expected = sorted(("A" if self.repair_before.get(path) is None else "M", path)
                          for path in self.repair_after)
        self.assertEqual(sorted(tuple(line.split("\t")) for line in self.repo.git(
            "diff", "--name-status", anchor, head).splitlines()), expected)
        for path in self.repair_after:
            self.assertTrue(self.repo.git("ls-tree", head, "--", path).startswith("100644 blob "))

    def test_noop_suffix_and_noop_merge_do_not_create_second_repair(self):
        anchor, head = self.repaired()
        head = self.repo.commit({}, "empty suffix")
        self.assertEqual(sc.check_ci_source(self.repo.root, anchor, head), ())
        self.data(head)
        self.repo.git("checkout", "--quiet", "-b", "noop-side")
        self.repo.commit({}, "empty side")
        self.repo.checkout(head)
        self.repo.git("merge", "--no-ff", "--no-edit", "noop-side")
        head = self.repo.head()
        self.assertEqual(sc.check_ci_source(self.repo.root, anchor, head), ())
        self.data(head)

    def test_representative_partial_and_missing_new_file_refuse_both_paths(self):
        existing = next(path for path, (before, _) in REPAIR_SOURCE_PINS.items() if before is not None)
        for changes in ({existing: self.repair_after[existing]}, {**self.repair_after, TEST: None}):
            with self.subTest(paths=tuple(changes)):
                anchor = self.anchor()
                head = self.repo.commit(changes, "partial repair")
                self.local_refuses(anchor, head)
                self.full_refuses(head)

    def test_split_and_revert_reapply_cannot_launder_repair(self):
        path = next(iter(REPAIR_SOURCE_PINS))
        anchor = self.anchor()
        self.repo.commit({path: self.repair_after[path]}, "partial first edge")
        head = self.repo.commit(self.repair_after, "complete later edge")
        self.local_refuses(anchor, head); self.full_refuses(head)
        anchor, _ = self.repaired()
        reverse = {name: self.repair_before.get(name) for name in self.repair_after}
        self.repo.commit(reverse, "reverse entire repair")
        head = self.repo.commit(self.repair_after, "repeat repair")
        self.local_refuses(anchor, head); self.full_refuses(head)

    def test_complete_and_partial_undo_net_zero_still_refuse_owned_history(self):
        path = next(path for path, (before, _) in REPAIR_SOURCE_PINS.items() if before is not None)
        for complete in (True, False):
            with self.subTest(complete=complete):
                anchor = self.anchor()
                changes = self.repair_after if complete else {path: self.repair_after[path]}
                self.repo.commit(changes, "owned repair changes")
                head = self.repo.commit({name: self.repair_before.get(name) for name in changes},
                                        "undo owned changes to identical unrepaired tree")
                self.assertEqual(self.repo.git("rev-parse", anchor + "^{tree}"),
                                 self.repo.git("rev-parse", head + "^{tree}"))
                self.local_refuses(anchor, head)
                self.full_refuses(head)

    def test_unrepaired_noop_and_unrelated_local_ci_remain_available(self):
        anchor = self.anchor()
        for head in (anchor, self.repo.commit({}, "unrepaired no-op")):
            self.assertEqual(sc.check_ci_source(self.repo.root, anchor, head), ())
            value = sc.classify_v2(self.repo.root, anchor, head)
            self.assertEqual((value.kind, value.dormant, value.reasons), ("none", False, ()))
            self.data(head)
        head = self.repo.commit({"release/deployment/test_compat_adapter.py": "# unrelated fixture change\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, anchor, head), ())
        value = sc.classify_v2(self.repo.root, anchor, head)
        self.assertEqual((value.kind, value.dormant, value.reasons), ("none", False, ()))
        self.full_refuses(head)

    def test_nonempty_unrelated_suffix_refuses_from_unrepaired_origin(self):
        anchor, _ = self.repaired()
        head = self.repo.commit({"README.md": "unreviewed suffix\n"})
        self.local_refuses(anchor, head); self.full_refuses(head)

    def test_pr286_clock_fixture_change_before_after_or_mixed_does_not_qualify_full_origin(self):
        path = "release/deployment/test_compat_adapter.py"
        # The path models the separate PR286 fixture repair, not its raw bytes.
        # It is deliberately outside this finite repair's independent inventory.
        self.assertNotIn(path, self.repair_after)
        for order in ("before", "after", "mixed"):
            with self.subTest(order=order):
                anchor = self.anchor()
                extra = {path: "# synthetic PR286-path fixture change, not actual PR286 source\n"}
                if order == "before": self.repo.commit(extra)
                head = self.repo.commit({**self.repair_after, **(extra if order == "mixed" else {})})
                if order == "after": head = self.repo.commit(extra)
                self.full_refuses(head)

    def test_already_repaired_base_has_no_permanent_filename_reservation(self):
        _, repaired = self.repaired()
        path = "release/deployment/test_record_resolver.py"
        head = self.repo.commit({path: self.repair_after[path] + "\n# ordinary later test change\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, repaired, head), ())
        value = sc.classify_v2(self.repo.root, repaired, head)
        self.assertEqual((value.kind, value.reasons), ("none", ()))
        self.full_refuses(head)

    def test_product_protected_and_mode_changes_are_not_part_of_repair(self):
        for path in (self.anchor_fixture.MAIN, "internal/dbcatalog/shape/production-v0.sql"):
            with self.subTest(path=path):
                raw = self.anchor_fixture.after.get(path, self.anchor_fixture.before.get(path))
                anchor, head = self.repaired({path: raw + "\n# changed protected product\n"})
                self.local_refuses(anchor, head); self.full_refuses(head)
        anchor = self.anchor()
        self.repo.commit(self.repair_after)
        self.repo.git("update-index", "--chmod=+x", POLICY)
        self.repo.git("commit", "--quiet", "--amend", "--no-edit")
        self.local_refuses(anchor, self.repo.head()); self.full_refuses(self.repo.head())

    def test_policy_slot_mutants_cannot_supply_their_own_trusted_fingerprint(self):
        matches = list(_SLOT.finditer(self.policy_input))
        self.assertEqual(len(matches), 1)
        match = matches[0]
        wrong = self.policy_input[:match.start(1)] + b"f" * 64 + self.policy_input[match.end(1):]
        body = self.policy_input + b"\n# unapproved policy body\n"
        canonical = body[:match.start(1)] + b"0" * 64 + body[match.end(1):]
        recomputed = body[:match.start(1)] + hashlib.sha256(canonical).hexdigest().encode() + body[match.end(1):]
        duplicate = self.policy_input + matches[0].group(0)
        noncanonical = self.policy_input[:match.start()] + match.group(0).replace(b"'", b'"') + self.policy_input[match.end():]
        trusted = Path(sc.__file__).read_bytes()
        for changed in (body, wrong, recomputed, duplicate, noncanonical):
            with self.subTest(mutant=hashlib.sha256(changed).hexdigest()):
                anchor, head = self.repaired({POLICY: changed.decode()})
                self.local_refuses(anchor, head); self.full_refuses(head)
                self.assertEqual(Path(sc.__file__).read_bytes(), trusted)

    def test_repair_base_on_only_second_parent_is_not_complete_history(self):
        anchor = self.anchor()
        self.repo.git("checkout", "--quiet", "-b", "second-parent")
        side = self.repo.commit({}, "side base")
        self.repo.checkout(anchor)
        self.repo.git("merge", "--no-ff", "--no-commit", "second-parent")
        head = self.repo.commit(self.repair_after, "repair with second-parent base")
        self.assertEqual(self.repo.git("rev-list", "--parents", "-n", "1", head).split(), [head, anchor, side])
        self.local_refuses(side, head)

    def test_real_bounded_reader_deadline_propagates_without_policy_fallback(self):
        anchor, head = self.repaired()
        for call in (sc.check_ci_source, sc.classify_v2):
            with self.subTest(entrypoint=call.__name__):
                now = [100.0]
                reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")),
                    clock=lambda: now[0])
                with self.assertRaisesRegex(source_git.SourceGitRefusal, "^git-failed:source-deadline$"):
                    with reader.operation():
                        now[0] = reader.deadline
                        call(self.repo.root, anchor, head)
                self.assertTrue(reader.failed and reader.closed)


if __name__ == "__main__":
    unittest.main()
