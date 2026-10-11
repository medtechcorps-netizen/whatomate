from __future__ import annotations

import copy
import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record as records
import ship_common as common
import test_ship_release_record_v2 as wire
import test_ship_support as support


class RecordCollectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fake = support.FakeGh()
        self.bootstrap = support.bootstrap_record("b" * 40)

    def client(self):
        return records.Gh(
            "/fake/bin/gh", support.GH_TOKEN, common.REPOSITORY,
            expected_version=support.GH_VERSION, runner=self.fake,
            base_env={"PATH": "/usr/bin"}, config_dir=str(self.root / "gh"),
        )

    def collect(self):
        return records.collect_authenticated_records(self.client(), work_dir=self.root / "raw")

    def resolve(self, bootstrap=None):
        return records.resolve_chain(
            self.client(), None, bootstrap=self.bootstrap if bootstrap is None else bootstrap,
            work_dir=self.root / "legacy",
        )

    def first(self):
        return support.manifest(
            kind="promote", sha="2" * 40, images=support.NEW,
            previous="prod-0000", deployed_at="2026-10-04T12:00:00Z",
        )

    def third(self, previous):
        return support.manifest(
            kind="promote", sha="3" * 40, images=support.OTHER,
            previous=previous, deployed_at="2026-10-06T12:00:00Z",
        )

    def operations(self):
        result = []
        for argv, _ in self.fake.calls:
            if argv[1:3] == ["release", "download"]:
                result.append(("download", argv[3]))
            elif argv[1:3] == ["attestation", "verify"]:
                result.append(("verify", argv[3]))
        return result

    def test_inventory_is_eager_and_authentication_is_lazy(self):
        first = self.first()
        second = wire.v2()
        self.fake.add_record(second)
        self.fake.add_record(first)
        iterator = self.collect()
        self.assertEqual(self.operations(), [])
        self.assertEqual(len([a for a, _ in self.fake.calls if a[1] == "api"]), 2)
        self.assertIs(iter(iterator), iterator)
        one = next(iterator)
        self.assertEqual(one.release, first["release"])
        self.assertEqual([x[0] for x in self.operations()], ["download", "verify"])
        two = next(iterator)
        self.assertEqual(two.release, second["release"])
        self.assertEqual([x[0] for x in self.operations()], ["download", "verify"] * 2)
        with self.assertRaises(StopIteration):
            next(iterator)

    def test_mixed_wire_bytes_are_immutable_and_not_policy_authority(self):
        first = self.first()
        second = wire.v2(db_change="contract", reversible=False, old_server_ok=False,
                         catalog_version=3, min_reader_version=2)
        raw1 = self.fake.add_record(first)
        raw2 = self.fake.add_record(second)
        collected = list(self.collect())
        for item, raw, document in zip(collected, (raw1, raw2), (first, second)):
            self.assertIs(type(item.raw), bytes)
            self.assertEqual(item.raw, raw)
            self.assertEqual(item.manifest_sha256, hashlib.sha256(raw).hexdigest())
            self.assertEqual((item.release, item.sha), (document["release"], document["sha"]))
            with self.assertRaises(AttributeError):
                item.raw = b"replacement"
            mutable = common.loads_strict(item.raw)
            mutable["sha"] = "0" * 40
            self.assertEqual(common.loads_strict(item.raw)["sha"], document["sha"])
        self.assertFalse(common.loads_strict(collected[-1].raw)["old_server_ok"])
        self.assertFalse(any(a[3].startswith("oci://") for a, _ in self.fake.calls
                             if a[1:3] == ["attestation", "verify"]))

    def test_legacy_v2_refusal_precedes_next_download(self):
        first, second = self.first(), wire.v2()
        third = self.third(second["release"])
        for document in (third, second, first):
            self.fake.add_record(document)
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:policy-unavailable$"):
            self.resolve()
        self.assertEqual([value for op, value in self.operations() if op == "download"],
                         [first["release"], second["release"]])
        self.assertEqual(sum(op == "verify" for op, _ in self.operations()), 2)

    def test_legacy_link_refusal_precedes_later_attestation(self):
        first = self.first()
        first["previous"] = "prod-20261003T120000Z-44444444"
        later = self.third(first["release"])
        self.fake.add_record(first)
        self.fake.add_record(later, attested=False)
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:link$"):
            self.resolve()
        self.assertEqual([x[0] for x in self.operations()], ["download", "verify"])
        self.assertEqual(self.operations()[0][1], first["release"])

    def test_inventory_refusal_precedes_bootstrap_and_download(self):
        first = self.first()
        self.fake.add_record(first, with_tag=False)
        with mock.patch.object(records, "bootstrap_entry", side_effect=AssertionError("must not run")):
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:tag-release-parity$"):
                self.resolve()
        self.assertEqual(self.operations(), [])

    def test_bootstrap_refusal_precedes_download_after_inventory(self):
        self.fake.add_record(self.first())
        with mock.patch.object(records, "bootstrap_entry", side_effect=common.ReleaseError("record-chain-invalid:bootstrap")):
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:bootstrap$"):
                self.resolve()
        self.assertEqual(len([a for a, _ in self.fake.calls if a[1] == "api"]), 2)
        self.assertEqual(self.operations(), [])

    def test_inventory_errors_are_eager_without_partial_authentication(self):
        def duplicate_release():
            self.fake.releases.append(copy.deepcopy(self.fake.releases[0]))

        def duplicate_tag():
            self.fake.tags.append(copy.deepcopy(self.fake.tags[0]))

        cases = (
            (duplicate_release, "duplicate"),
            (duplicate_tag, "tag"),
            (lambda: self.fake.releases[0].update(prerelease=True), "release-state"),
            (lambda: self.fake.releases[0]["assets"].append({}), "assets"),
            (lambda: self.fake.releases[0]["assets"][0].update(size=True), "assets"),
            (lambda: self.fake.tags[0]["object"].update(type="tag"), "tag"),
            (lambda: self.fake.tags.clear(), "tag-release-parity"),
        )
        for mutate, code in cases:
            with self.subTest(code=code):
                self.fake = support.FakeGh()
                self.fake.add_record(self.first())
                mutate()
                with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:" + code + "$"):
                    self.collect()
                self.assertEqual(self.operations(), [])

    def test_record_limit_precedes_any_download(self):
        self.fake.add_record(self.first())
        with mock.patch.object(records, "MAX_RECORDS", 0):
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:too-many$"):
                self.collect()
        self.assertEqual(self.operations(), [])

    def test_download_canonical_tag_size_and_signature_guards(self):
        for label, expected, mutate in (
            ("download", "download", lambda d: self.fake.assets.clear()),
            ("size", "asset-size", lambda d: self.fake.releases[0]["assets"][0].update(size=1)),
            ("tag", "tag-binding", lambda d: self.fake.tags[0]["object"].update(sha="9" * 40)),
            ("signature", "attestation", lambda d: self.fake.attested_files.clear()),
            ("canonical", "manifest", lambda d: self.replace_raw(d, json.dumps(d, indent=1).encode())),
            ("duplicate-key", "manifest", lambda d: self.replace_raw(d, b'{"sha":"private",' + common.canonical_file_bytes(d)[1:])),
            ("unknown-field", "manifest", lambda d: self.replace_raw(d, common.canonical_file_bytes(dict(d, private="never print")))),
        ):
            with self.subTest(label=label):
                self.fake = support.FakeGh()
                document = wire.v2()
                self.fake.add_record(document)
                mutate(document)
                iterator = self.collect()
                with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:" + expected + "$"):
                    next(iterator)
                count = len(self.operations())
                with self.assertRaises(StopIteration):
                    next(iterator)
                self.assertEqual(len(self.operations()), count)

    def replace_raw(self, document, raw):
        self.fake.assets[document["release"]] = raw
        self.fake.releases[0]["assets"][0]["size"] = len(raw)

    def test_verifier_binds_rollback_workflow_source_not_image_source(self):
        document = wire.v2("rollback")
        document["signer"]["workflow_sha"] = "9" * 40
        self.fake.add_record(document)
        item = next(self.collect())
        self.assertEqual(item.sha, document["sha"])
        argv = [a for a, _ in self.fake.calls if a[1:3] == ["attestation", "verify"]][0]
        for flag in ("--signer-digest", "--source-digest"):
            self.assertEqual(argv[argv.index(flag) + 1], "9" * 40)
        self.assertIn("--deny-self-hosted-runners", argv)
        self.assertEqual(argv[argv.index("--source-ref") + 1], "refs/heads/main")
        self.assertEqual(argv[argv.index("--cert-identity") + 1],
                         f"https://github.com/{common.REPOSITORY}/.github/workflows/ship.yml@refs/heads/main")

    def test_signature_failure_never_yields_first_item(self):
        self.fake.add_record(self.first(), attested=False)
        iterator = self.collect()
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:attestation$"):
            next(iterator)
        with self.assertRaises(StopIteration):
            next(iterator)
        self.assertEqual([x[0] for x in self.operations()], ["download", "verify"])

    def test_absent_remote_bootstrap_and_drafts_do_not_invent_records(self):
        self.fake.add_record(self.first(), draft=True)
        self.fake.releases.append({"tag_name": "prod-0000"})
        self.fake.tags.append({"ref": "refs/tags/prod-0000", "object": {}})
        self.assertEqual(list(self.collect()), [])
        self.assertEqual(self.operations(), [])
        self.assertEqual(self.resolve().latest.release, "prod-0000")

    def test_v1_chain_entry_bytes_and_links_remain_unchanged(self):
        first = self.first()
        later = self.third(first["release"])
        raw1 = self.fake.add_record(first)
        raw2 = self.fake.add_record(later)
        chain = self.resolve()
        self.assertEqual(chain.entries, (
            records.bootstrap_entry(self.bootstrap),
            records.manifest_entry(first, raw1), records.manifest_entry(later, raw2),
        ))


if __name__ == "__main__":
    unittest.main()
