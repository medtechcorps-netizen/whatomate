from __future__ import annotations

import copy
import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record
import ship_common as common
import test_ship_support as support


HERE = Path(__file__).resolve().parent
BASE = "b" * 40
SHA_1 = "1" * 40
SHA_2 = "2" * 40


def gh_client(fake: support.FakeGh, temp: Path) -> release_record.Gh:
    return release_record.Gh(
        "/fake/bin/gh", support.GH_TOKEN, common.REPOSITORY,
        expected_version=support.GH_VERSION, runner=fake,
        base_env={"PATH": "/usr/bin", "SHIP_DO_TOKEN": support.DO_TOKEN}, config_dir=str(temp / "gh"),
    )


class BootstrapTests(unittest.TestCase):
    def test_committed_bootstrap_is_canonical_pinned_and_valid(self) -> None:
        raw = release_record.BOOTSTRAP_PATH.read_bytes()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), release_record.BOOTSTRAP_SHA256)
        record = release_record.load_bootstrap()
        self.assertEqual(raw, common.canonical_file_bytes(record))
        self.assertEqual(record["release"], "prod-0000")
        self.assertEqual(record["sha"], "c482dbbc287ae29ea0f6fe11081d4cc16d8ca525")
        self.assertEqual(record["legacy_signer"]["workflow_sha"], "0825df34bea71dcd1828f581461551694f3906e2")
        self.assertEqual(record["deployed_at"], "2026-10-02T15:11:28Z")
        self.assertTrue(record["images"]["web"].startswith("sha256:17f765ae"))
        self.assertTrue(record["images"]["meta-relay"].startswith("sha256:b5a33ed0"))
        self.assertTrue(record["images"]["gmail-relay"].startswith("sha256:685e858c"))
        self.assertTrue(record["evidence"]["phase_state_sha256"].startswith("ac3e0df9"))

    def test_bootstrap_carries_no_provider_identity(self) -> None:
        text = release_record.BOOTSTRAP_PATH.read_text(encoding="ascii")
        common.sanitize_public(json.loads(text))
        for key in ("app_identity", "ingress", "deployment_identity", "spec_sha256"):
            self.assertNotIn(key, text)

    def test_tampered_bootstrap_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "bootstrap.json"
            record = json.loads(release_record.BOOTSTRAP_PATH.read_text(encoding="ascii"))
            record["images"]["web"] = support.digest("0")
            raw = common.canonical_file_bytes(record)
            path.write_bytes(raw)
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:bootstrap$"):
                release_record.load_bootstrap(path)
            self.assertEqual(release_record.load_bootstrap(path, hashlib.sha256(raw).hexdigest())["images"]["web"], support.digest("0"))
            path.write_bytes(raw.replace(b"\n", b" \n"))
            with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:bootstrap$"):
                release_record.load_bootstrap(path, hashlib.sha256(path.read_bytes()).hexdigest())


class ManifestTests(unittest.TestCase):
    def promote(self, **changes: object) -> dict:
        value = support.manifest(kind="promote", sha=SHA_1, images=support.NEW, previous="prod-0000",
                                 deployed_at="2026-10-04T12:00:00Z")
        value.update(changes)
        return value

    def test_valid_kinds(self) -> None:
        release_record.validate_manifest(self.promote())
        rollback = support.manifest(kind="rollback", sha=BASE, images=support.LIVE, previous="prod-20261004T120000Z-11111111",
                                    rolled_back_from="prod-20261004T120000Z-11111111", deployed_at="2026-10-04T13:00:00Z",
                                    workflow_sha=SHA_1, reconciled=True)
        release_record.validate_manifest(rollback)
        restore = support.manifest(kind="restore", sha=SHA_1, images=support.NEW, previous="prod-20261004T120000Z-11111111",
                                   deployed_at="2026-10-04T14:00:00Z")
        release_record.validate_manifest(restore)

    def test_manifest_refusals(self) -> None:
        cases = {
            "extra-key": dict(self.promote(), extra=1),
            "kind": self.promote(kind="bootstrap"),
            "tag-time": self.promote(release="prod-20261004T120001Z-11111111"),
            "tag-sha8": self.promote(release="prod-20261004T120000Z-22222222"),
            "db-change": self.promote(db_change=True),
            "schema-guard": self.promote(schema_guard="waived"),
            "built-after-deploy": self.promote(built_at="2026-10-04T12:00:01Z"),
            "promote-rolled-back": self.promote(rolled_back_from="prod-0000"),
            "promote-reconciled": self.promote(reconciled=True),
            "signer": self.promote(signer={"workflow": ".github/workflows/other.yml", "workflow_sha": SHA_1}),
            "fingerprint": self.promote(fingerprints={"environment_values_sha256": "x", "non_source_projection_sha256": "d" * 64}),
            "previous": self.promote(previous="latest"),
            "images": self.promote(images={"web": support.NEW["web"]}),
        }
        for label, value in cases.items():
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, "^record-invalid$"):
                release_record.validate_manifest(value)
        rollback = support.manifest(kind="rollback", sha=BASE, images=support.LIVE, previous="prod-20261004T120000Z-11111111",
                                    rolled_back_from="prod-20261004T110000Z-11111111", deployed_at="2026-10-04T13:00:00Z")
        with self.assertRaisesRegex(common.ReleaseError, "^record-invalid$"):
            release_record.validate_manifest(rollback)
        restore = support.manifest(kind="restore", sha=SHA_1, images=support.NEW, previous="prod-20261004T120000Z-11111111",
                                   deployed_at="2026-10-04T14:00:00Z", reconciled=True)
        with self.assertRaisesRegex(common.ReleaseError, "^record-invalid$"):
            release_record.validate_manifest(restore)

    def test_build_manifest_is_canonical(self) -> None:
        fields = dict(self.promote())
        fields["workflow_sha"] = SHA_1
        fields.update(fields.pop("fingerprints"))
        raw = release_record.build_manifest(fields)
        self.assertEqual(raw, common.canonical_file_bytes(json.loads(raw)))


class ChainTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fake = support.FakeGh()
        self.bootstrap = support.bootstrap_record(BASE)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def resolve(self) -> release_record.Chain:
        return release_record.resolve_chain(gh_client(self.fake, self.root), None, bootstrap=self.bootstrap,
                                            work_dir=self.root / "work")

    def first(self) -> dict:
        return support.manifest(kind="promote", sha=SHA_1, images=support.NEW, previous="prod-0000",
                                deployed_at="2026-10-04T12:00:00Z")

    def second(self, first: dict) -> dict:
        return support.manifest(kind="promote", sha=SHA_2, images=support.OTHER, previous=first["release"],
                                deployed_at="2026-10-05T12:00:00Z")

    def test_empty_chain_is_the_bootstrap(self) -> None:
        chain = self.resolve()
        self.assertEqual(chain.latest.release, "prod-0000")
        self.assertEqual(chain.latest.manifest_sha256,
                         hashlib.sha256(common.canonical_file_bytes(self.bootstrap)).hexdigest())

    def test_committed_bootstrap_entry_hash_is_the_pinned_file_hash(self) -> None:
        entry = release_record.bootstrap_entry(release_record.load_bootstrap())
        self.assertEqual(entry.manifest_sha256, release_record.BOOTSTRAP_SHA256)

    def test_linked_records_resolve_in_tag_order(self) -> None:
        first = self.first()
        second = self.second(first)
        self.fake.add_record(second)
        self.fake.add_record(first)
        self.fake.releases.append({"id": 1, "tag_name": "v1.0.0", "draft": False, "prerelease": False, "assets": []})
        self.fake.releases.append({"id": 2, "tag_name": "prod-0000", "draft": False, "prerelease": False, "assets": []})
        self.fake.tags.append({"ref": "refs/tags/prod-0000", "object": {"type": "commit", "sha": "0" * 40}})
        chain = self.resolve()
        self.assertEqual([entry.release for entry in chain.entries], ["prod-0000", first["release"], second["release"]])
        self.assertEqual(chain.latest.manifest_sha256, hashlib.sha256(common.canonical_file_bytes(second)).hexdigest())

    def test_rollback_and_restore_links(self) -> None:
        first = self.first()
        self.fake.add_record(first)
        rollback = support.manifest(kind="rollback", sha=BASE, images=support.LIVE, previous=first["release"],
                                    rolled_back_from=first["release"], deployed_at="2026-10-04T13:00:00Z", workflow_sha=SHA_2)
        self.fake.add_record(rollback)
        restore = support.manifest(kind="restore", sha=BASE, images=support.LIVE, previous=rollback["release"],
                                   deployed_at="2026-10-04T14:00:00Z", workflow_sha=SHA_2)
        self.fake.add_record(restore)
        self.assertEqual(self.resolve().latest.kind, "restore")

    def test_chain_anomalies_fail_closed(self) -> None:
        def broken_previous(fake: support.FakeGh) -> None:
            fake.add_record(support.manifest(kind="promote", sha=SHA_1, images=support.NEW, previous="prod-20261001T000000Z-00000000",
                                             deployed_at="2026-10-04T12:00:00Z"))

        def out_of_time(fake: support.FakeGh) -> None:
            first = self.first()
            fake.add_record(first)
            fake.add_record(support.manifest(kind="promote", sha=SHA_2, images=support.OTHER, previous=first["release"],
                                             deployed_at="2026-10-04T12:00:00Z"))

        def foreign_rollback(fake: support.FakeGh) -> None:
            first = self.first()
            fake.add_record(first)
            fake.add_record(support.manifest(kind="rollback", sha=SHA_2, images=support.OTHER, previous=first["release"],
                                             rolled_back_from=first["release"], deployed_at="2026-10-04T13:00:00Z"))

        def bad_restore(fake: support.FakeGh) -> None:
            first = self.first()
            fake.add_record(first)
            fake.add_record(support.manifest(kind="restore", sha=SHA_2, images=support.OTHER, previous=first["release"],
                                             deployed_at="2026-10-04T13:00:00Z"))

        cases = {
            "previous-link": (broken_previous, "link"),
            "same-second": (out_of_time, "link"),
            "foreign-rollback": (foreign_rollback, "link"),
            "bad-restore": (bad_restore, "link"),
            "unattested": (lambda fake: fake.add_record(self.first(), attested=False), "attestation"),
            "tag-elsewhere": (lambda fake: fake.add_record(self.first(), tag_sha=SHA_2), "tag-binding"),
            "missing-tag": (lambda fake: fake.add_record(self.first(), with_tag=False), "tag-release-parity"),
            "prerelease": (lambda fake: fake.add_record(self.first(), prerelease=True), "release-state"),
            "asset-name": (lambda fake: fake.add_record(self.first(), asset_name="manifest.json"), "assets"),
            "non-canonical": (lambda fake: fake.add_record(self.first(), raw=json.dumps(self.first(), indent=1).encode() + b"\n"), "manifest"),
            "bad-tag": (lambda fake: fake.releases.append({"id": 9, "tag_name": "prod-latest", "draft": False,
                                                           "prerelease": False, "assets": []}), "tag-format"),
        }
        for label, (build, detail) in cases.items():
            self.fake = support.FakeGh()
            build(self.fake)
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, f"^record-chain-invalid:{detail}$"):
                self.resolve()

    def test_stray_tag_and_extra_asset(self) -> None:
        self.fake.tags.append({"ref": "refs/tags/prod-20261004T120000Z-11111111", "object": {"type": "commit", "sha": SHA_1}})
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:tag-release-parity$"):
            self.resolve()
        self.fake = support.FakeGh()
        first = self.first()
        self.fake.add_record(first)
        self.fake.releases[-1]["assets"].append({"id": 5, "name": "extra.txt", "size": 3})
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:assets$"):
            self.resolve()
        self.fake = support.FakeGh()
        self.fake.add_record(first)
        self.fake.tags[-1]["object"]["type"] = "tag"
        with self.assertRaisesRegex(common.ReleaseError, "^record-chain-invalid:tag$"):
            self.resolve()

    def test_drafts_are_not_records(self) -> None:
        first = self.first()
        self.fake.add_record(first, draft=True)
        self.assertEqual(self.resolve().latest.release, "prod-0000")

    def test_record_attestation_flags_are_exact(self) -> None:
        first = self.first()
        self.fake.add_record(first)
        self.resolve()
        verify = [argv for argv, _env in self.fake.calls if argv[1:3] == ["attestation", "verify"]]
        self.assertEqual(len(verify), 1)
        argv = verify[0]
        self.assertEqual(argv[4:], [
            "--repo", common.REPOSITORY,
            "--cert-identity", f"https://github.com/{common.REPOSITORY}/.github/workflows/ship.yml@refs/heads/main",
            "--signer-digest", SHA_1, "--source-digest", SHA_1, "--source-ref", "refs/heads/main",
            "--deny-self-hosted-runners", "--predicate-type", "https://slsa.dev/provenance/v1", "--format", "json",
        ])

    def test_gh_environment_is_scrubbed_and_never_echoes_stderr(self) -> None:
        self.resolve()
        for _argv, env in self.fake.calls:
            self.assertNotIn("SHIP_DO_TOKEN", env)
            self.assertNotIn(support.DO_TOKEN, json.dumps(env))
            self.assertEqual(env["GH_TOKEN"], support.GH_TOKEN)
            self.assertEqual(env["GH_PROMPT_DISABLED"], "1")
        self.fake.fail_commands.add("api")
        with self.assertRaises(common.ReleaseError) as caught:
            self.resolve()
        self.assertNotIn("stderr", str(caught.exception))

    def test_gh_version_is_pinned(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^gh-failed:version$"):
            gh_client(support.FakeGh(version="2.97.0"), self.root)


class ImageVerificationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fake = support.FakeGh()
        self.bootstrap = support.bootstrap_record(BASE)
        self.fake.attest_legacy_images(support.LIVE, BASE)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def verify(self, images: dict, sha: str) -> None:
        release_record.verify_images(gh_client(self.fake, self.root), images, sha, self.bootstrap)

    def test_legacy_allowance_only_for_exact_bootstrap_digests(self) -> None:
        self.verify(dict(support.LIVE), BASE)
        legacy_calls = [argv for argv, _ in self.fake.calls if "attestation" in argv]
        self.assertEqual(len(legacy_calls), 6)
        self.assertTrue(all(f"https://github.com/{common.REPOSITORY}/.github/workflows/build-attest-exact-release-images.yml@refs/heads/main" in argv
                            for argv in legacy_calls))

    def test_bootstrap_digest_under_another_component_fails(self) -> None:
        swapped = {"web": support.LIVE["meta-relay"], "meta-relay": support.LIVE["web"], "gmail-relay": support.LIVE["gmail-relay"]}
        with self.assertRaisesRegex(common.ReleaseError, "^attestation-unverified"):
            self.verify(swapped, BASE)

    def test_legacy_predicate_fields_are_bound(self) -> None:
        for field in ("component", "commit", "phase", "workflow_sha"):
            fake = support.FakeGh()
            fake.attest_legacy_images(support.LIVE, BASE)
            for entries in fake.image_attestations.values():
                predicate = entries[1]["predicate"]
                if field == "component":
                    predicate["image"]["component"] = "web-other"
                elif field == "commit":
                    predicate["source"]["commit"] = SHA_2
                elif field == "phase":
                    predicate["phase"] = "backend"
                else:
                    predicate["builder"]["workflow_sha"] = SHA_2
            self.fake = fake
            with self.subTest(field=field), self.assertRaisesRegex(common.ReleaseError, "^attestation-unverified:legacy-predicate$"):
                self.verify(dict(support.LIVE), BASE)

    def test_ship_signer_for_new_digests(self) -> None:
        self.fake.attest_ship_images(support.NEW, SHA_1)
        self.verify(dict(support.NEW), SHA_1)
        with self.assertRaisesRegex(common.ReleaseError, "^attestation-unverified:image$"):
            self.verify(dict(support.NEW), SHA_2)
        mixed = dict(support.NEW, web=support.LIVE["web"])
        self.verify(mixed, SHA_1)

    def test_image_flags_use_the_exact_certificate_identity(self) -> None:
        self.fake.attest_ship_images(support.NEW, SHA_1)
        self.verify(dict(support.NEW), SHA_1)
        argv = [argv for argv, _ in self.fake.calls if argv[1:3] == ["attestation", "verify"]][0]
        self.assertEqual(argv[4:-2], [
            "--repo", common.REPOSITORY,
            "--cert-identity", f"https://github.com/{common.REPOSITORY}/.github/workflows/ship.yml@refs/heads/main",
            "--signer-digest", SHA_1, "--source-digest", SHA_1, "--source-ref", "refs/heads/main",
            "--deny-self-hosted-runners", "--predicate-type", "https://slsa.dev/provenance/v1",
        ])
        self.assertNotIn("--signer-workflow", argv)

    def test_a_prefix_matched_signer_is_refused(self) -> None:
        # gh's --signer-workflow is a prefix regex: a certificate from
        # ship.yml-x.yml would satisfy "ship.yml". Model gh returning such a
        # certificate and require the exact identity check to refuse it.
        for workflow in (".github/workflows/ship.yml-x.yml", ".github/workflows/shipXyml"):
            fake = support.FakeGh()
            fake.attest_ship_images(support.NEW, SHA_1)
            for entries in fake.image_attestations.values():
                for item in entries:
                    item["matched_as"] = item["workflow"]
                    item["workflow"] = workflow
            self.fake = fake
            with self.subTest(workflow=workflow), self.assertRaisesRegex(common.ReleaseError, "^attestation-unverified:image$"):
                self.verify(dict(support.NEW), SHA_1)

    def test_every_certificate_field_is_bound(self) -> None:
        identity = gh_client(self.fake, self.root).signer_identity(".github/workflows/ship.yml", SHA_1, SHA_1)
        item = {"verificationResult": {"signature": {"certificate": dict(identity)}}}
        self.assertTrue(release_record.certificate_matches(item, identity))
        for key in identity:
            changed = copy.deepcopy(item)
            changed["verificationResult"]["signature"]["certificate"][key] = "other"
            with self.subTest(key=key):
                self.assertFalse(release_record.certificate_matches(changed, identity))
            missing = copy.deepcopy(item)
            del missing["verificationResult"]["signature"]["certificate"][key]
            self.assertFalse(release_record.certificate_matches(missing, identity))
        self.assertFalse(release_record.certificate_matches({"verificationResult": {}}, identity))
        self.assertFalse(release_record.certificate_matches({"verificationResult": {"signature": {"certificate": []}}}, identity))


class AncestryTests(unittest.TestCase):
    def test_no_downgrade(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = support.TempRepo(Path(temp) / "repo")
            first = repo.commit({"a.txt": "1"})
            second = repo.commit({"a.txt": "2"})
            repo.git("checkout", "--quiet", "-b", "side", first)
            sibling = repo.commit({"b.txt": "x"})
            release_record.require_no_downgrade(repo.root, first, second)
            release_record.require_no_downgrade(repo.root, second, second)
            for latest, candidate in ((second, first), (second, sibling)):
                with self.subTest(latest=latest[:7], candidate=candidate[:7]):
                    with self.assertRaisesRegex(common.ReleaseError, "^downgrade-refused$"):
                        release_record.require_no_downgrade(repo.root, latest, candidate)
            with self.assertRaisesRegex(common.ReleaseError, "^downgrade-refused:unknown-commit$"):
                release_record.require_no_downgrade(repo.root, "0" * 40, second)


class RollbackPlanningTests(unittest.TestCase):
    def chain(self) -> release_record.Chain:
        bootstrap = release_record.bootstrap_entry(support.bootstrap_record(BASE), "f" * 64)
        first = support.manifest(kind="promote", sha=SHA_1, images=support.NEW, previous="prod-0000", deployed_at="2026-10-04T12:00:00Z")
        second = support.manifest(kind="promote", sha=SHA_2, images=support.OTHER, previous=first["release"],
                                  deployed_at="2026-10-05T12:00:00Z")
        entries = [bootstrap]
        for item in (first, second):
            entries.append(release_record.manifest_entry(item, common.canonical_file_bytes(item)))
        return release_record.Chain(tuple(entries))

    def test_plan_targets(self) -> None:
        chain = self.chain()
        target, intermediates = release_record.rollback_plan(chain, "prod-0000")
        self.assertEqual(target.release, "prod-0000")
        self.assertEqual(len(intermediates), 2)
        target, intermediates = release_record.rollback_plan(chain, chain.entries[1].release)
        self.assertEqual(len(intermediates), 1)
        target, intermediates = release_record.rollback_plan(chain, chain.latest.release)
        self.assertEqual(intermediates, ())
        for tag in ("prod-20990101T000000Z-99999999", "latest", ""):
            with self.subTest(tag=tag), self.assertRaisesRegex(common.ReleaseError, "^rollback-target-invalid"):
                release_record.rollback_plan(chain, tag)

    def test_decide_cases(self) -> None:
        chain = self.chain()
        latest = chain.latest
        older = chain.entries[1]
        unrecorded = {"web": support.digest("7"), "meta-relay": support.digest("8"), "gmail-relay": support.digest("9")}
        self.assertEqual(release_record.decide_rollback_case(latest.images, chain, older), "rollback")
        self.assertEqual(release_record.decide_rollback_case(older.images, chain, older), "reconcile")
        self.assertEqual(release_record.decide_rollback_case(unrecorded, chain, latest), "restore")
        partly = dict(latest.images, web=support.LIVE["web"])
        self.assertEqual(release_record.decide_rollback_case(partly, chain, latest), "restore")
        with self.assertRaisesRegex(common.ReleaseError, "^nothing-to-roll-back$"):
            release_record.decide_rollback_case(latest.images, chain, latest)
        with self.assertRaisesRegex(common.ReleaseError, "^drift$"):
            release_record.decide_rollback_case(unrecorded, chain, older)

    def test_restore_never_undoes_a_rollback_to_an_older_record(self) -> None:
        chain = self.chain()
        latest = chain.latest
        for label, live in (("bootstrap", support.LIVE), ("intermediate", chain.entries[1].images)):
            with self.subTest(live=label), self.assertRaisesRegex(common.ReleaseError, "^drift:live-matches-record$"):
                release_record.decide_rollback_case(dict(live), chain, latest)


if __name__ == "__main__":
    unittest.main()
