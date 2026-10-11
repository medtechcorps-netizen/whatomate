"""Synthetic trust-boundary integration; no real GitHub, Git or crypto claims.

Real transport/verifier/policy classes consume controlled command captures.
Pinned bootstrap/profile bytes are real fixtures; synthetic object IDs describe
only this fake Git object store and are never represented as hosted evidence.
"""
import copy
from dataclasses import FrozenInstanceError
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import record_policy as policy
import record_resolver as resolver
import record_transport as rt
import release_record as records
import ship_common as common
import test_record_transport as transport_tests
import test_ship_support as support


ROOT = Path(__file__).resolve().parents[2]


class ControlledWorld(transport_tests.World):
    def __init__(self, repo, binary, clock):
        super().__init__()
        self.repo, self.binary, self.clock = repo, binary, clock
        self.commits, self.files, self.blobs = set(), {}, {}
        self.bad_ancestor = False
        self.before = self.after = None
        self.trace = []

    def __call__(self, argv, **options):
        argv = list(argv)
        self.trace.append((argv, dict(options)))
        if self.before:
            self.before(argv)
        if argv[0] == str(self.binary):
            # Fixed source_git prefix ends at this advice setting. No shell or
            # executable is launched, and all remaining commands are explicit.
            args = argv[argv.index("advice.graftFileDeprecated=false") + 1:]
            result = self.git(args)
        else:
            result = super().__call__(argv, **options)
        if self.after:
            self.after(argv)
        return result

    def git(self, args):
        data, code = b"", 0
        if args == ["rev-parse", "--show-toplevel"]:
            data = (str(self.repo) + "\n").encode()
        elif args == ["rev-parse", "--show-object-format"]:
            data = b"sha1\n"
        elif args == ["rev-parse", "--is-shallow-repository"]:
            data = b"false\n"
        elif args == ["for-each-ref", "--format=%(refname)", "refs/replace/"]:
            pass
        elif args[:2] == ["config", "--get-regexp"]:
            code = 1
        elif args[0] == "rev-parse" and args[1] in ("--absolute-git-dir", "--git-common-dir"):
            data = (str(self.repo / ".git") + "\n").encode()
        elif args[:2] == ["cat-file", "-t"]:
            data = b"commit\n" if args[2] in self.commits else b"blob\n"
        elif args[:2] == ["ls-tree", "-z"]:
            raw = self.files.get((args[2], args[4]))
            if raw is not None:
                oid = hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()
                self.blobs[oid] = raw
                data = f"100644 blob {oid}\t{args[4]}\0".encode()
        elif args[:2] == ["cat-file", "-s"]:
            data = str(len(self.blobs[args[2]])).encode() + b"\n"
        elif args[:2] == ["cat-file", "blob"]:
            data = self.blobs[args[2]]
        elif args[0] == "merge-base":
            data = (("f" * 40 if self.bad_ancestor else args[1]) + "\n").encode()
        else:
            raise AssertionError("unmodeled synthetic Git command")
        return rt.runtime.Capture(code, data, b"")


class ResolverTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source_bytes = {path: (ROOT / path).read_bytes() for path in policy.SOURCE_PATHS}
        cls.v0 = policy.reviewed_v0_source("1" * 40,
            tuple(policy.SourceBlob(path, cls.source_bytes[path]) for path in policy.SOURCE_PATHS))

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        (self.repo / ".git").mkdir(parents=True)
        self.git = self.root / "git.exe"
        self.gh = self.root / "gh.exe"
        self.git.write_bytes(b"not executable")
        self.gh.write_bytes(b"not executable")
        self.clock = transport_tests.Clock()
        self.world = ControlledWorld(self.repo, self.git, self.clock)
        self.boot = records.load_bootstrap()
        self.documents = [self.boot]
        self.world.commits.add(self.boot["sha"])
        self.world.fake.attest_legacy_images(self.boot["images"], self.boot["sha"],
            workflow_sha=self.boot["legacy_signer"]["workflow_sha"])
        self.client = None

    def add(self, *, version=2, sha=None, kind="promote", target=None, images=None, **changes):
        prior = self.documents[-1]
        sha = sha or str(len(self.documents)) * 40
        if target:
            sha, images = target["sha"], target["images"]
        if kind == "restore":
            sha, images = prior["sha"], prior["images"]
        images = images or {key: "sha256:" + hashlib.sha256((sha + key).encode()).hexdigest()
                            for key in common.COMPONENTS}
        doc = support.manifest(kind=kind, sha=sha, images=images, previous=prior["release"],
            deployed_at=f"2026-10-04T12:{len(self.documents):02d}:00Z",
            rolled_back_from=prior["release"] if kind == "rollback" else None)
        if version == 2:
            doc.update(schema_version=2, db_change="none", dormant=False, reversible=True,
                old_server_ok=True, compat_run_id=38100000001, compat="ok", staging="passed",
                fixed_forward_from=None, **self.v0.catalog._asdict())
            if kind != "promote":
                doc.update(old_server_ok="unproven", compat_run_id=None, compat="bypassed", staging="bypassed")
        doc.update(changes)
        self.documents.append(doc)
        self.world.add(doc)
        self.world.fake.attest_ship_images(images, sha)
        self.world.commits.add(sha)
        if version == 2:
            self.world.files.update({(sha, path): raw for path, raw in self.source_bytes.items()})
        return doc

    def resolve(self, tag=None):
        self.assertIsNone(self.client, "each synthetic operation must be fresh")
        config, work = self.root / "config", self.root / "work"
        config.mkdir(); work.mkdir()
        budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
        self.client = rt.ReleaseGh(self.gh, support.GH_TOKEN, config, work, budget=budget,
                                  base_env={"SHIP_DO_TOKEN": "private-do-token"})
        return resolver.resolve_authenticated(self.client, self.repo, self.git, tag)

    def refused(self, tag=None):
        with self.assertRaisesRegex(rt.TransportRefusal, "record-chain-invalid:transport-resolver$") as caught:
            self.resolve(tag)
        self.assertNotIn("private", str(caught.exception))
        self.assertTrue(self.client.budget.closed)
        self.assertTrue(self.client.budget.failed)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_bootstrap_only_has_no_invented_live_contract(self):
        result = self.resolve()
        self.assertEqual(len(result.chain.entries), 1)
        self.assertIsNone(result.latest.live)
        self.assertIsNone(result.chain.baseline)
        self.assertEqual(result.latest.record_sha, self.boot["sha"])
        self.assertEqual(result.latest.reader.mode, "legacy-reader0")
        self.assertEqual(result.chain.inputs[0].raw, records.BOOTSTRAP_PATH.read_bytes())
        self.assertEqual(result.selected, result.latest)
        self.assertTrue(self.client.budget.closed)

    def test_all_v1_selection_preserves_complete_raw_chain_and_none_live(self):
        first, last = self.add(version=1), self.add(version=1)
        result = self.resolve(first["release"])
        self.assertEqual(result.latest.release, last["release"])
        self.assertEqual(result.selected.index, 1)
        self.assertEqual(len(result.chain.inputs), 3)
        self.assertTrue(all(entry.live is None for entry in result.chain.entries))
        self.assertTrue(all(source.mode == "legacy-reader0" for source in result.chain.sources))
        self.assertFalse(any("ls-tree" in args for args, _ in self.world.trace))
        self.assertEqual(result.chain.inputs[2].raw, self.world.fake.assets[last["release"]])

    def test_v2_binds_seven_blobs_and_measured_profile_not_golden_hash(self):
        self.add(version=1)
        latest = self.add()
        result = self.resolve()
        source = result.latest.reader
        self.assertEqual(source.mode, "reviewed-v0")
        self.assertEqual(source.catalog.catalog_sha256, policy.V0_MEASUREMENT)
        self.assertNotEqual(source.catalog.catalog_sha256, dict(source.artifacts)[policy.GOLDEN])
        loaded = [(args[-3], args[-1]) for args, _ in self.world.trace if "ls-tree" in args]
        self.assertEqual(loaded, [(latest["sha"], path) for path in policy.SOURCE_PATHS])
        self.assertEqual(result.latest.live.origin_release, latest["release"])
        self.assertEqual(result.latest.live.origin_manifest_sha256, result.latest.manifest_sha256)
        self.assertTrue(result.chain.runtime_source_supported)

    def test_v2_rollback_to_v1_and_restore_retain_live_origin(self):
        old = self.add(version=1)
        origin = self.add()
        rollback = self.add(kind="rollback", target=old)
        restored = self.add(kind="restore")
        result = self.resolve(rollback["release"])
        self.assertEqual(result.selected.reader.mode, "legacy-reader0")
        self.assertEqual(result.latest.release, restored["release"])
        self.assertEqual(result.latest.live.origin_release, origin["release"])
        self.assertEqual(result.latest.record_sha, old["sha"])
        self.assertNotEqual(result.latest.live.origin_source.sha, result.latest.record_sha)

    def test_bootstrap_component_exception_keeps_record_and_actual_source_distinct(self):
        latest = self.add(images=self.boot["images"])
        result = self.resolve()
        self.assertEqual(result.latest.record_sha, latest["sha"])
        self.assertEqual(result.latest.actual_web_source_sha, self.boot["sha"])
        self.assertEqual(result.latest.reader.mode, "legacy-reader0")
        self.assertEqual(result.latest.live.origin_source.sha, latest["sha"])
        self.assertEqual(dict(result.image_bindings[-1].source_shas),
                         {key: self.boot["sha"] for key in common.COMPONENTS})

    def test_repeated_sha_required_complete_wins_over_legacy(self):
        first = self.add(version=1)
        self.add(sha=first["sha"])
        result = self.resolve()
        matching = [source for source in result.chain.sources if source.sha == first["sha"]]
        self.assertEqual(len(matching), 1)
        self.assertEqual(matching[0].mode, "reviewed-v0")
        self.assertEqual(result.chain.entries[1].reader, matching[0])

    def test_repeated_sha_missing_complete_never_falls_back(self):
        first = self.add(version=1)
        self.add(sha=first["sha"])
        del self.world.files[(first["sha"], policy.HISTORY)]
        self.refused()

    def test_each_missing_source_blob_refuses(self):
        for path in policy.SOURCE_PATHS:
            with self.subTest(path=path):
                case = ResolverTests("test_bootstrap_only_has_no_invented_live_contract")
                case.setUp()
                try:
                    latest = case.add()
                    del case.world.files[(latest["sha"], path)]
                    case.refused()
                finally:
                    case.doCleanups()

    def test_changed_pinned_shape_has_no_future_or_legacy_fallback(self):
        latest = self.add()
        self.world.files[(latest["sha"], policy.PROFILE)] += b" "
        self.refused()

    def test_malformed_seed_and_registry_contract_refuse(self):
        for path in (policy.SEEDS, policy.STEPS):
            with self.subTest(path=path):
                case = ResolverTests("test_bootstrap_only_has_no_invented_live_contract")
                case.setUp()
                try:
                    latest = case.add()
                    case.world.files[(latest["sha"], path)] = b'{"unknown":true}'
                    case.refused()
                finally:
                    case.doCleanups()

    def test_all_image_components_are_authenticated_including_bootstrap(self):
        self.add()
        self.resolve()
        subjects = [args[3] for args, _ in self.world.trace if args[1:3] == ["attestation", "verify"]
                    and args[3].startswith("oci://")]
        self.assertEqual(len(subjects), 9)  # two legacy predicates per bootstrap component + three new
        for component in common.COMPONENTS:
            self.assertTrue(any(common.IMAGE_REPOSITORY[component] in subject for subject in subjects))

    def test_nonweb_image_signature_failure_refuses(self):
        latest = self.add()
        del self.world.fake.image_attestations[(common.IMAGE_REPOSITORY["meta-relay"], latest["images"]["meta-relay"])]
        self.refused()

    def test_bootstrap_image_signature_failure_refuses(self):
        del self.world.fake.image_attestations[(common.IMAGE_REPOSITORY["gmail-relay"], self.boot["images"]["gmail-relay"])]
        self.refused()

    def test_record_signature_failure_prevents_policy_reads(self):
        self.add()
        self.world.fake.attested_files.clear()
        self.refused()
        self.assertFalse(any(args[0] == str(self.git) for args, _ in self.world.trace))

    def test_noncanonical_record_refuses_before_signature(self):
        latest = self.add()
        self.world.fake.assets[latest["release"]] += b" "
        self.world.fake.releases[-1]["assets"][0]["size"] += 1
        self.refused()

    def test_complete_inventory_is_consumed_before_source_derivation(self):
        self.add(); self.add()
        self.resolve()
        trace = [args for args, _ in self.world.trace]
        first_git = next(i for i, args in enumerate(trace) if args[0] == str(self.git))
        downloads = [i for i, args in enumerate(trace) if any("/releases/assets/" in arg for arg in args)]
        self.assertEqual(len(downloads), 2)
        self.assertLess(max(downloads), first_git)

    def test_missing_record_commit_refuses_even_if_web_is_bootstrap(self):
        latest = self.add(version=1, images=self.boot["images"])
        self.world.commits.remove(latest["sha"])
        self.refused()

    def test_missing_actual_component_commit_refuses(self):
        self.add()
        self.world.commits.remove(self.boot["sha"])
        self.refused()

    def test_nonancestor_promote_refuses(self):
        self.add()
        self.world.bad_ancestor = True
        self.refused()

    def test_v1_after_v2_refuses(self):
        self.add(); self.add(version=1)
        self.refused()

    def test_wrong_v2_live_contract_refuses_without_baseline_invention(self):
        self.add(catalog_sha256="f" * 64)
        self.refused()

    def test_first_v2_rollback_cannot_invent_legacy_live_shape(self):
        old = self.add(version=1)
        self.add(version=1)
        self.add(kind="rollback", target=old)
        self.refused()

    def test_exact_tag_selection_does_not_collapse_equal_images(self):
        first = self.add(version=1)
        second = self.add(version=1, sha=first["sha"], images=first["images"])
        result = self.resolve(first["release"])
        self.assertEqual(result.selected.index, 1)
        self.assertEqual(result.latest.index, 2)
        self.assertNotEqual(result.selected.release, second["release"])

    def test_unknown_selection_refuses(self):
        self.refused("prod-20261004T120000Z-11111111")

    def test_mutable_ref_selection_refuses_before_get(self):
        self.refused("main")
        self.assertEqual(len(self.world.trace), 1)  # constructor version check only

    def test_final_main_drift_refuses_after_final_git_preflight(self):
        self.add()
        count = 0
        def change(args):
            nonlocal count
            if "--show-toplevel" in args:
                count += 1
                if count == 2:
                    self.world.main = "b" * 40
        self.world.after = change
        self.refused()
        self.assertEqual(count, 2)

    def test_final_selected_asset_drift_refuses(self):
        latest = self.add()
        def change(args):
            if args[1:4] == ["api", "--method", "GET"] and "/releases/tags/" in args[4]:
                self.world.fake.releases[-1]["assets"][0]["id"] += 1
        self.world.before = change
        self.refused(latest["release"])

    def test_git_final_metadata_drift_refuses_before_final_gh_reads(self):
        self.add()
        def change(args):
            if "merge-base" in args:
                (self.repo / ".git" / "shallow").write_text("synthetic")
        self.world.after = change
        self.refused()
        main_gets = [args for args, _ in self.world.trace if rt.API + "/git/ref/heads/main" in args]
        self.assertEqual(len(main_gets), 1)

    def test_shared_budget_deadline_covers_git_not_only_gh(self):
        self.add()
        def expire(args):
            if "ls-tree" in args:
                self.clock.now = 701
        self.world.after = expire
        self.refused()

    def test_deadline_after_last_snapshot_call_still_refuses(self):
        self.add()
        refs = 0
        def expire(args):
            nonlocal refs
            if len(args) > 4 and "/git/ref/tags/" in args[4]:
                refs += 1
                if refs == 3:  # initial list, final list, selected/latest exact read
                    self.clock.now = 701
        self.world.after = expire
        self.refused()
        self.assertEqual(refs, 3)

    def test_shared_call_and_byte_bounds_cannot_be_reset_by_git(self):
        for bound, value in (("MAX_COMMANDS", 14), ("MAX_BYTES", 150000)):
            with self.subTest(bound=bound):
                case = ResolverTests("test_bootstrap_only_has_no_invented_live_contract")
                case.setUp()
                try:
                    case.add()
                    with mock.patch.object(rt, bound, value):
                        case.refused()
                finally:
                    case.doCleanups()

    def test_private_capture_exception_is_redacted_and_context_restored(self):
        self.add()
        def fail(args):
            if "cat-file" in args:
                raise RuntimeError("private-token DSN@example.test")
        self.world.before = fail
        self.refused()

    def test_projection_is_deeply_immutable_and_not_legacy_entry(self):
        self.add()
        result = self.resolve()
        with self.assertRaises(FrozenInstanceError):
            result.latest.record_sha = "f" * 40
        with self.assertRaises(TypeError):
            result.chain.inputs[0] = result.chain.inputs[1]
        self.assertNotIsInstance(result.latest, records.Entry)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_exact_client_type_required_without_duck_typed_authority(self):
        with self.assertRaisesRegex(rt.TransportRefusal, "resolver-client"):
            resolver.resolve_authenticated(object(), self.repo, self.git)

    def test_git_environment_cannot_inherit_gh_token(self):
        self.add()
        self.resolve()
        for args, options in self.world.trace:
            if args[0] == str(self.git):
                self.assertNotIn("GH_TOKEN", options["env"])
                self.assertNotIn("SHIP_DO_TOKEN", options["env"])
            self.assertLessEqual(options["timeout"], 60)
        self.assertEqual(self.client.budget.commands, len(self.world.trace))
        self.assertGreater(self.client.budget.captured_bytes, len(self.source_bytes[policy.GOLDEN]))


if __name__ == "__main__":
    unittest.main()
