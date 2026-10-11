"""Synthetic trust-boundary integration; no real GitHub, Git or crypto claims.

Real transport/verifier/policy classes consume controlled command captures.
Pinned bootstrap/profile bytes are real fixtures; synthetic object IDs describe
only this fake Git object store and are never represented as hosted evidence.
"""
import copy
from dataclasses import FrozenInstanceError
import difflib
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import record_policy as policy
import record_resolver as resolver
import record_transport as rt
import release_record as records
import schema_change
import ship_common as common
import test_record_transport as transport_tests
import test_ship_support as support
import pending_source
import test_pending_plan


ROOT = Path(__file__).resolve().parents[2]


class ControlledWorld(transport_tests.World):
    def __init__(self, repo, binary, clock):
        super().__init__()
        self.repo, self.binary, self.clock = repo, binary, clock
        self.commits, self.files, self.blobs = set(), {}, {}
        self.bad_ancestor = False
        self.bad_pairs = set()
        self.history = {}
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
            data = (("f" * 40 if self.bad_ancestor or tuple(args[1:]) in self.bad_pairs else args[1]) + "\n").encode()
        elif args[:3] == ["rev-list", "--first-parent", "--reverse"]:
            base, head = args[3].split("..")
            revisions = self.history.get((base, head), [] if base == head else [head])
            data = b"".join((revision + "\n").encode() for revision in revisions)
        elif args[:4] == ["ls-tree", "-r", "--name-only", "-z"]:
            data = b"".join(path.encode() + b"\0" for revision, path in sorted(self.files)
                            if revision == args[4] and any(path == prefix.rstrip("/")
                                or path.startswith(prefix.rstrip("/") + "/") for prefix in args[6:]))
        elif args[0] == "diff" and "--name-status" in args:
            base, head = args[-3:-1]
            paths = sorted({path for revision, path in self.files if revision in (base, head)})
            for path in paths:
                before, after = self.files.get((base, path)), self.files.get((head, path))
                if before != after:
                    status = "A" if before is None else "D" if after is None else "M"
                    data += (status + "\0" + path + "\0").encode()
        elif args[0] == "diff" and "-U0" in args:
            base, head, _, path = args[-4:]
            before = self.files.get((base, path), b"").decode().splitlines(keepends=True)
            after = self.files.get((head, path), b"").decode().splitlines(keepends=True)
            data = "".join(difflib.unified_diff(before, after, fromfile="a/" + path,
                                             tofile="b/" + path, n=0)).encode()
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


class ForwardResolverTests(unittest.TestCase):
    # Reuse only scenario setup; the original 35 test methods remain singly
    # discovered and unchanged. No classifier or authentication call is mocked.
    setUpClass = ResolverTests.__dict__["setUpClass"]
    setUp = ResolverTests.setUp
    add = ResolverTests.add
    WORKFLOW = ".github/workflows/compat.yml"
    MAIN = "cmd/whatomate/main.go"
    RECIPE = "docker/release/web.Dockerfile"

    def profile(self, sha, *, closure="a" * 64, body=1):
        self.world.commits.add(sha)
        files = dict(self.source_bytes)
        files[self.WORKFLOW] = (ROOT / self.WORKFLOW).read_bytes()
        files[self.RECIPE] = b"FROM scratch\n# immutable synthetic recipe\n"
        files[resolver._DRILLS] = (ROOT / resolver._DRILLS).read_bytes()
        files[self.MAIN] = f"package main\n\nfunc runRLSMigration(args []string) {{\n\tprintln({body})\n}}\n".encode()
        files[schema_change.MIGRATION_PATH] = common.canonical_file_bytes({
            "version": 1, "roots": schema_change.MIGRATION_ROOTS,
            "closure_files": [self.MAIN], "closure_sha256": closure})
        self.world.files.update({(sha, path): raw for path, raw in files.items()})
        return sha

    def candidate(self, sha="7" * 40, **kwargs):
        return self.profile(sha, **kwargs)

    def forward(self, sha, *, selected_tag=None):
        self.assertIsNone(self.client)
        config, work = self.root / "config", self.root / "work"
        config.mkdir(); work.mkdir()
        budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
        self.client = rt.ReleaseGh(self.gh, support.GH_TOKEN, config, work, budget=budget,
                                  base_env={"SHIP_DO_TOKEN": "private-do-token"})
        return resolver.resolve_forward(self.client, self.repo, self.git, sha, selected_tag=selected_tag)

    def refused(self, sha, *, selected_tag=None):
        with self.assertRaisesRegex(rt.TransportRefusal, "record-chain-invalid:transport-resolver$"):
            self.forward(sha, selected_tag=selected_tag)
        self.assertTrue(self.client.budget.closed)
        self.assertTrue(self.client.budget.failed)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_forward_returns_full_classification_and_source_bound_workflow(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        result = self.forward(sha)
        self.assertEqual((result.candidate.sha, result.candidate.mode), (sha, "reviewed-v0"))
        self.assertEqual((result.classification.kind, result.classification.dormant, result.classification.reasons),
                         ("none", False, ()))
        self.assertEqual(result.classification.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(result.workflow, (ROOT / self.WORKFLOW).read_bytes())
        self.assertEqual(result.workflow_sha256, hashlib.sha256(result.workflow).hexdigest())
        self.assertEqual(result.previous_web_source_sha, old["sha"])
        self.assertEqual(result.database_base_source, old["sha"])
        self.assertTrue(self.client.budget.closed)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_rollback_classifies_from_retained_origin_even_when_candidate_equals_origin(self):
        old = self.add(version=1)
        self.profile(old["sha"], body=0)
        origin = self.add()
        self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        result = self.forward(origin["sha"])
        self.assertEqual(result.database_base_source, origin["sha"])
        self.assertEqual(result.previous_web_source_sha, old["sha"])
        self.assertEqual(result.resolved.latest.record_sha, old["sha"])
        self.assertEqual((result.classification.kind, result.classification.reasons), ("none", ()))
        ranges = [args[-1] for args, _ in self.world.trace if "rev-list" in args]
        self.assertEqual(set(ranges), {origin["sha"] + ".." + origin["sha"],
                                      old["sha"] + ".." + origin["sha"]})
        # Only pending continuity uses latest-record here. The complete
        # classifier and its migration roots still use database origin.
        self.assertEqual(ranges.count(old["sha"] + ".." + origin["sha"]), 1)

    def test_candidate_must_descend_from_both_record_and_retained_origin(self):
        for which in ("record", "origin"):
            with self.subTest(which=which):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add(version=1)
                    origin = case.add()
                    case.profile(origin["sha"])
                    case.add(kind="rollback", target=old)
                    sha = case.candidate()
                    base = old["sha"] if which == "record" else origin["sha"]
                    case.world.bad_pairs.add((base, sha))
                    case.refused(sha)
                finally:
                    case.doCleanups()

    def test_all_v1_uses_record_base_without_inventing_live_or_decoding_absent_profile(self):
        old = self.add(version=1, sha=policy.V0_RELEASED_SOURCE)
        # Real early source could have a catalog but no production profile. It
        # remains reader0/live=None; comparison does not invent those bytes.
        for path in (policy.GOLDEN, policy.HISTORY, policy.GLOBALS):
            self.world.files[(old["sha"], path)] = self.source_bytes[path]
        self.world.files[(old["sha"], self.RECIPE)] = b"FROM scratch\n"
        result = self.forward(self.candidate())
        self.assertEqual(result.database_base_source, old["sha"])
        self.assertEqual(result.previous_web_source_sha, old["sha"])
        self.assertIsNone(result.resolved.latest.live)
        self.assertIsNone(result.resolved.chain.baseline)
        self.assertEqual(result.resolved.latest.reader.mode, "legacy-reader0")
        old_reads = {args[-1] for args, _ in self.world.trace if "ls-tree" in args and old["sha"] in args}
        self.assertIn(policy.PROFILE, old_reads)  # explicit absence observations
        self.assertIn(policy.OVERLAY, old_reads)
        material = result.catalog_material
        self.assertEqual(material.baseline_provenance, "legacy-e001-absent-overlay")
        missing = {row.path for row in material.baseline_blobs if row.raw is None}
        self.assertEqual(missing, {policy.PROFILE, policy.OVERLAY})

    def test_pre_catalog_without_supported_baseline_material_refuses(self):
        self.add(version=1)
        self.refused(self.candidate())
        self.assertTrue(any("diff" in args for args, _ in self.world.trace))

    def test_dormant_seed_change_keeps_explanation_and_no_favorable_shortcut(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        # Whitespace is source-visible but structurally valid; the real
        # classifier must retain its bounded public seed diff and obligations.
        self.world.files[(sha, policy.SEEDS)] += b"\n"
        result = self.forward(sha)
        self.assertEqual((result.classification.kind, result.classification.dormant), ("data", True))
        self.assertTrue(any(line.startswith("seeds diff ") for line in result.classification.summary))
        self.assertNotEqual(result.candidate.catalog.seeds_sha256, self.v0.catalog.seeds_sha256)

    def test_migration_changes_and_requirements_are_not_erased(self):
        old = self.add()
        self.profile(old["sha"])
        result = self.forward(self.candidate(closure="b" * 64, body=2))
        self.assertIn("generated migration closure changed", result.classification.migration_changes)
        self.assertIn("migration path changed: " + self.MAIN + "#runRLSMigration",
                      result.classification.migration_changes)
        self.assertIn("generated-migration-closure-ci", result.classification.requirements)

    def test_stale_migration_hash_remains_blocking(self):
        old = self.add()
        self.profile(old["sha"])
        result = self.forward(self.candidate(body=2))
        self.assertIn("migration-contract-stale", result.classification.reasons)

    def test_pending_format_remains_explicitly_unsupported(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        self.world.files[(sha, "internal/dbcatalog/golden/pending/catalog-v1.json")] = b"{}\n"
        result = self.forward(sha)
        self.assertIn("pending-format-unsupported", result.classification.reasons)
        self.assertIn("pending-format-unsupported", result.classification.requirements)
        self.assertEqual(result.pending.candidate_files, (
            pending_source.PendingBlob("internal/dbcatalog/golden/pending/catalog-v1.json", b"{}\n"),))
        self.assertEqual(result.pending.previous_files, ())
        self.assertIn("classifier-v2-invalid:pending-material", result.classification.reasons)
        self.assertIn("producer-class-unsupported", result.classification.requirements)

    def test_generic_preview_is_not_production_main_authority(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        result = self.forward(sha)
        self.assertNotEqual(result.candidate.sha, result.resolved.inventory.main_sha)
        self.assertNotIsInstance(result, records.Entry)
        self.assertFalse(hasattr(result, "normal_request"))

    def test_candidate_identifier_must_be_full_immutable_sha(self):
        for value in (None, "main", "7" * 39, "A" * 40, "7" * 40 + ":private", True):
            with self.subTest(value=value):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    case.refused(value)
                    self.assertEqual(len(case.world.trace), 1)
                finally:
                    case.doCleanups()

    def test_missing_candidate_commit_refuses(self):
        sha = self.candidate()
        self.world.commits.remove(sha)
        self.refused(sha)

    def test_missing_candidate_profile_cannot_reuse_historical_legacy_reader0(self):
        old = self.add(version=1)
        self.refused(old["sha"])

    def test_candidate_repeated_in_v1_prefix_has_one_consistent_complete_source(self):
        old = self.add(version=1)
        self.profile(old["sha"])
        result = self.forward(old["sha"])
        sources = [fact for fact in result.resolved.chain.sources if fact.sha == old["sha"]]
        self.assertEqual(sources, [result.candidate])
        self.assertEqual(result.resolved.latest.reader, result.candidate)
        self.assertEqual(result.resolved.latest.reader.mode, "reviewed-v0")
        self.assertIsNone(result.resolved.latest.live)
        self.assertEqual(tuple((row.path, row.raw) for row in result.catalog_material.baseline_blobs),
                         result.catalog_material.candidate_blobs)

    def test_unsupported_candidate_profile_refuses_before_classification(self):
        old = self.add()
        sha = self.candidate()
        self.world.files[(sha, policy.PROFILE)] += b" "
        self.refused(sha)
        self.assertFalse(any("diff" in args for args, _ in self.world.trace))
        self.assertEqual(self.documents[-1], old)

    def test_missing_or_empty_workflow_refuses_after_classification(self):
        for value in (None, b""):
            with self.subTest(value=value):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add()
                    case.profile(old["sha"])
                    sha = case.candidate()
                    if value is None:
                        del case.world.files[(sha, self.WORKFLOW)]
                    else:
                        case.world.files[(sha, self.WORKFLOW)] = value
                    case.refused(sha)
                    self.assertTrue(any("diff" in args for args, _ in case.world.trace))
                finally:
                    case.doCleanups()

    def test_workflow_bytes_are_source_bound_but_admission_is_separate(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        exact = b"name: Unreviewed synthetic workflow\n"
        self.world.files[(sha, self.WORKFLOW)] = exact
        result = self.forward(sha)
        self.assertEqual(result.workflow, exact)
        self.assertEqual(result.workflow_sha256, hashlib.sha256(exact).hexdigest())
        self.assertFalse(hasattr(result, "workflow_admitted"))

    def test_symlink_workflow_is_rejected_as_not_regular_source(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        real_git = self.world.git
        def symlink(args):
            result = real_git(args)
            if args == ["ls-tree", "-z", sha, "--", self.WORKFLOW]:
                return rt.runtime.Capture(0, result.stdout.replace(b"100644", b"120000", 1), b"")
            return result
        self.world.git = symlink
        self.refused(sha)

    def test_workflow_work_consumes_same_remaining_deadline(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        def expire(args):
            if args[-5:] == ["ls-tree", "-z", sha, "--", self.WORKFLOW]:
                self.clock.now = 701
        self.world.after = expire
        self.refused(sha)

    def test_shared_bound_includes_classifier_calls(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        def exhaust(args):
            if "diff" in args:
                self.client.budget.commands = rt.MAX_COMMANDS
        self.world.after = exhaust
        self.refused(sha)

    def test_final_snapshot_occurs_after_candidate_work_and_final_git_preflight(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        workflow_read = False
        def drift(args):
            nonlocal workflow_read
            if args[-5:] == ["ls-tree", "-z", sha, "--", self.WORKFLOW]:
                workflow_read = True
            if workflow_read and "--show-toplevel" in args:
                self.world.main = "b" * 40
        self.world.after = drift
        self.refused(sha)
        self.assertTrue(workflow_read)
        tops = [args for args, _ in self.world.trace if "--show-toplevel" in args]
        self.assertEqual(len(tops), 2)

    def test_full_forward_projection_is_immutable_and_closed(self):
        old = self.add()
        self.profile(old["sha"])
        result = self.forward(self.candidate())
        with self.assertRaises(FrozenInstanceError):
            result.database_base_source = "f" * 40
        with self.assertRaises(AttributeError):
            result.classification.kind = "none"
        before = len(self.world.trace)
        with self.assertRaises(rt.TransportRefusal):
            self.client.capture_snapshot()
        self.assertEqual(len(self.world.trace), before)

    def test_material_is_exact_five_sorted_pin_bytes_and_live_origin(self):
        old = self.add()
        self.profile(old["sha"])
        result = self.forward(self.candidate())
        material = result.catalog_material
        pins = dict(policy.V0_PINS)
        self.assertEqual(material.candidate_sha, result.candidate.sha)
        self.assertEqual(tuple(row.path for row in material.candidate_blobs), tuple(sorted(pins)))
        self.assertEqual(tuple(row.path for row in material.baseline_blobs), tuple(sorted(pins)))
        self.assertEqual(material.baseline_sha, result.resolved.latest.live.origin_source.sha)
        self.assertEqual(material.baseline_provenance, "retained-live-v2")
        for row in (*material.candidate_blobs, *material.baseline_blobs):
            self.assertIs(type(row.raw), bytes)
            self.assertEqual(hashlib.sha256(row.raw).hexdigest(), pins[row.path])
        self.assertEqual(result.resolved.latest.live.catalog,
                         result.resolved.latest.live.origin_source.catalog)

    def test_explicit_target_keeps_latest_database_origin_and_actual_selected_recipe(self):
        selected = self.add(version=1, images=self.boot["images"])
        self.world.files[(self.boot["sha"], self.RECIPE)] = b"FROM scratch\n# actual image source\n"
        self.world.files[(selected["sha"], self.RECIPE)] = b"FROM scratch\n# wrong manifest source\n"
        latest = self.add()
        self.profile(latest["sha"])
        result = self.forward(self.candidate(), selected_tag=selected["release"])
        self.assertEqual(result.resolved.selected.release, selected["release"])
        self.assertEqual(result.resolved.latest.release, latest["release"])
        self.assertEqual(result.previous_web_source_sha, self.boot["sha"])
        self.assertEqual(result.database_base_source, latest["sha"])
        self.assertEqual(result.catalog_material.baseline_sha, latest["sha"])
        self.assertEqual(result.previous_recipe.source_sha, self.boot["sha"])
        self.assertEqual(result.previous_recipe.path, self.RECIPE)
        self.assertIn(b"actual image source", result.previous_recipe.raw)
        self.assertEqual(result.previous_recipe.sha256, hashlib.sha256(result.previous_recipe.raw).hexdigest())
        self.assertFalse(hasattr(result, "rollback_decision"))

    def test_all_v1_explicit_target_uses_selected_catalog_but_latest_comparison(self):
        selected = self.add(version=1)
        self.profile(selected["sha"])
        latest = self.add(version=1)
        self.profile(latest["sha"])
        result = self.forward(self.candidate(), selected_tag=selected["release"])
        self.assertEqual(result.catalog_material.baseline_sha, selected["sha"])
        self.assertEqual(result.catalog_material.baseline_provenance, "legacy-equal-reviewed-v0")
        self.assertEqual(result.database_base_source, latest["sha"])
        self.assertEqual(result.previous_web_source_sha, selected["sha"])
        self.assertIsNone(result.resolved.latest.live)
        self.assertIsNone(result.resolved.chain.baseline)
        self.assertEqual((result.classification.kind, result.classification.reasons), ("none", ()))

    def test_explicit_bootstrap_target_has_no_general_legacy_catalog_exception(self):
        old = self.add(version=1)
        self.profile(old["sha"])
        self.refused(self.candidate(), selected_tag=common.BOOTSTRAP_TAG)

    def test_unknown_or_mutable_target_never_silently_selects_latest(self):
        for tag in ("main", "prod-20261004T120000Z-aaaaaaaa"):
            with self.subTest(tag=tag):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add()
                    case.profile(old["sha"])
                    case.refused(case.candidate(), selected_tag=tag)
                finally:
                    case.doCleanups()

    def test_e001_legacy_requires_both_absent_not_equal_or_single_absent(self):
        for present in ((policy.PROFILE,), (policy.OVERLAY,), (policy.PROFILE, policy.OVERLAY)):
            with self.subTest(present=present):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add(version=1, sha=policy.V0_RELEASED_SOURCE)
                    case.profile(old["sha"])
                    for path in {policy.PROFILE, policy.OVERLAY} - set(present):
                        del case.world.files[(old["sha"], path)]
                    case.refused(case.candidate())
                finally:
                    case.doCleanups()

    def test_e001_legacy_still_requires_all_three_existing_pins(self):
        for path in (policy.GOLDEN, policy.HISTORY, policy.GLOBALS):
            with self.subTest(path=path):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add(version=1, sha=policy.V0_RELEASED_SOURCE)
                    case.profile(old["sha"])
                    for missing in (policy.PROFILE, policy.OVERLAY):
                        del case.world.files[(old["sha"], missing)]
                    case.world.files[(old["sha"], path)] += b" "
                    case.refused(case.candidate())
                finally:
                    case.doCleanups()

    def test_non_e001_legacy_absence_is_never_generalized(self):
        for path in (policy.PROFILE, policy.OVERLAY):
            with self.subTest(path=path):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add(version=1)
                    case.profile(old["sha"])
                    del case.world.files[(old["sha"], path)]
                    case.refused(case.candidate())
                finally:
                    case.doCleanups()

    def test_v2_e001_source_requires_complete_pins_without_legacy_absence(self):
        old = self.add(sha=policy.V0_RELEASED_SOURCE)
        self.profile(old["sha"])
        result = self.forward(self.candidate())
        self.assertEqual(result.catalog_material.baseline_provenance, "retained-live-v2")
        self.assertTrue(all(row.raw is not None for row in result.catalog_material.baseline_blobs))

    def test_candidate_bytes_changed_after_facts_refuse_material(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        def drift(args):
            if args[-5:] == ["ls-tree", "-z", sha, "--", self.WORKFLOW]:
                self.world.files[(sha, policy.OVERLAY)] += b"-- changed bytes\n"
        self.world.after = drift
        self.refused(sha)

    def test_live_origin_bytes_changed_after_facts_refuse_material(self):
        old = self.add()
        self.profile(old["sha"])
        sha = self.candidate()
        def drift(args):
            if args[-5:] == ["ls-tree", "-z", sha, "--", self.WORKFLOW]:
                self.world.files[(old["sha"], policy.PROFILE)] += b" "
        self.world.after = drift
        self.refused(sha)

    def test_selected_recipe_missing_or_empty_refuses(self):
        for raw in (None, b""):
            with self.subTest(raw=raw):
                case = ForwardResolverTests()
                case.setUp()
                try:
                    old = case.add()
                    case.profile(old["sha"])
                    if raw is None:
                        del case.world.files[(old["sha"], self.RECIPE)]
                    else:
                        case.world.files[(old["sha"], self.RECIPE)] = raw
                    case.refused(case.candidate())
                finally:
                    case.doCleanups()

    def test_selected_recipe_nonregular_refuses(self):
        old = self.add()
        self.profile(old["sha"])
        real_git = self.world.git
        def symlink(args):
            result = real_git(args)
            if args == ["ls-tree", "-z", old["sha"], "--", self.RECIPE]:
                return rt.runtime.Capture(0, result.stdout.replace(b"100644", b"120000", 1), b"")
            return result
        self.world.git = symlink
        self.refused(self.candidate())

    def test_recipe_consumes_original_budget_and_precedes_final_refresh(self):
        old = self.add()
        self.profile(old["sha"])
        def expire(args):
            if args[-5:] == ["ls-tree", "-z", old["sha"], "--", self.RECIPE]:
                self.clock.now = 701
        self.world.after = expire
        self.refused(self.candidate())
        main_gets = [args for args, _ in self.world.trace if rt.API + "/git/ref/heads/main" in args]
        self.assertEqual(len(main_gets), 1)

    def test_material_and_recipe_are_deeply_immutable(self):
        old = self.add()
        self.profile(old["sha"])
        result = self.forward(self.candidate())
        with self.assertRaises(FrozenInstanceError):
            result.catalog_material.baseline_sha = "f" * 40
        with self.assertRaises(FrozenInstanceError):
            result.catalog_material.baseline_blobs[0].raw = b"foreign"
        with self.assertRaises(AttributeError):
            result.catalog_material.candidate_blobs[0].raw = b"foreign"
        with self.assertRaises(FrozenInstanceError):
            result.previous_recipe.raw = b"foreign"


class ForwardAuditTests(unittest.TestCase):
    setUpClass = ResolverTests.__dict__["setUpClass"]
    setUp = ResolverTests.setUp
    add = ResolverTests.add
    profile = ForwardResolverTests.profile
    candidate = ForwardResolverTests.candidate
    forward = ForwardResolverTests.forward
    refused = ForwardResolverTests.refused
    WORKFLOW = ForwardResolverTests.WORKFLOW
    MAIN = ForwardResolverTests.MAIN
    RECIPE = ForwardResolverTests.RECIPE

    def ordinary(self):
        old = self.add()
        self.profile(old["sha"])
        return old["sha"], self.candidate()

    def audit_output(self, raw):
        original = self.world.git
        active = False
        def response(args):
            nonlocal active
            if args[:2] == ["ls-tree", "-z"] and args[-1] == resolver._DRILLS:
                active = True
            if active and args[0] == "diff" and "--name-status" in args:
                return rt.runtime.Capture(0, raw, b"")
            return original(args)
        patcher = mock.patch.object(self.world, "git", side_effect=response)
        patcher.start(); self.addCleanup(patcher.stop)

    def test_all_equal_bases_have_one_complete_range_and_all_three_roles(self):
        base, sha = self.ordinary()
        self.world.files[base, "docs/removed.md"] = b"before"
        self.world.files[sha, "docs/added.md"] = b"after"
        self.world.files[base, "docs/changed.md"] = b"before"
        self.world.files[sha, "docs/changed.md"] = b"after"
        result = self.forward(sha)
        audit = result.audit
        self.assertEqual(audit.candidate_sha, sha)
        self.assertEqual(audit.drills.raw, (ROOT / resolver._DRILLS).read_bytes())
        self.assertEqual(audit.changes, (resolver.ChangedRange(
            ("database-origin", "latest-record", "selected-web"), base, sha,
            (resolver.ChangedPath("A", "docs/added.md"), resolver.ChangedPath("M", "docs/changed.md"),
             resolver.ChangedPath("D", "docs/removed.md"))),))
        with self.assertRaises(FrozenInstanceError): audit.drills.raw = b"other"
        with self.assertRaises(TypeError): audit.changes[0].rows[0] = None

    def test_three_distinct_bases_keep_origin_record_and_actual_web_labels(self):
        old = self.add(version=1, images=self.boot["images"])
        origin = self.add(); self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        self.world.files[self.boot["sha"], self.RECIPE] = b"FROM scratch\n"
        sha = self.candidate()
        result = self.forward(sha)
        self.assertEqual({row.base_sha: row.roles for row in result.audit.changes}, {
            origin["sha"]: ("database-origin",), old["sha"]: ("latest-record",),
            self.boot["sha"]: ("selected-web",)})
        self.assertTrue(all(row.head_sha == sha for row in result.audit.changes))
        boot = next(row for row in result.audit.changes if row.base_sha == self.boot["sha"])
        self.assertIn(resolver.ChangedPath("A", policy.GOLDEN), boot.rows)
        origin_range = next(row for row in result.audit.changes if row.base_sha == origin["sha"])
        self.assertEqual(origin_range.rows, ())

    def test_two_equal_roles_deduplicate_and_same_commit_diff_is_empty(self):
        old = self.add(version=1); self.profile(old["sha"])
        origin = self.add(); self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        result = self.forward(origin["sha"])
        self.assertEqual({row.base_sha: row.roles for row in result.audit.changes}, {
            old["sha"]: ("latest-record", "selected-web"), origin["sha"]: ("database-origin",)})
        self.assertTrue(all(row.rows == () for row in result.audit.changes))

    def test_drills_absence_and_unparsed_irrelevant_bytes_are_preserved(self):
        for raw in (None, b"malformed but irrelevant source bytes"):
            with self.subTest(absent=raw is None):
                case = ForwardAuditTests(); case.setUp()
                try:
                    _, sha = case.ordinary()
                    if raw is None: del case.world.files[sha, resolver._DRILLS]
                    else: case.world.files[sha, resolver._DRILLS] = raw
                    self.assertEqual(case.forward(sha).audit.drills, resolver.BlobObservation(resolver._DRILLS, raw))
                finally: case.doCleanups()

    def test_status_typechange_and_rename_as_add_delete_preserve_complete_rows(self):
        _, sha = self.ordinary()
        self.audit_output(b"T\0file.py\0A\0new.py\0D\0old.py\0")
        self.assertEqual(self.forward(sha).audit.changes[0].rows,
            (resolver.ChangedPath("T", "file.py"), resolver.ChangedPath("A", "new.py"),
             resolver.ChangedPath("D", "old.py")))

    def test_bad_framing_utf8_status_unsafe_and_duplicate_paths_refuse(self):
        for raw in (b"M\0file.py", b"M\0", b"M\0\xff\0", b"R100\0a\0b\0",
                    b"PRIVATE\0file.py\0", b"M\0../PRIVATE\0", b"M\0a//b\0", b"M\0.git/config\0",
                    b"M\0C:/PRIVATE\0", b"M\0a\\b\0", b"M\0a\nPRIVATE\0",
                    b"M\0same.py\0M\0same.py\0", b"A\0same.py\0D\0same.py\0"):
            with self.subTest(length=len(raw)):
                case = ForwardAuditTests(); case.setUp()
                try:
                    _, sha = case.ordinary(); case.audit_output(raw); case.refused(sha)
                finally: case.doCleanups()

    def test_count_and_combined_projection_caps_refuse_without_truncating(self):
        for raw in (b"".join(f"M\0p{i}.py\0".encode() for i in range(10001)),
                    b"".join(f"M\0{'a' * 490}{i:04d}\0".encode() for i in range(1100))):
            case = ForwardAuditTests(); case.setUp()
            try:
                _, sha = case.ordinary(); case.audit_output(raw); case.refused(sha)
            finally: case.doCleanups()

    def test_audit_uses_original_budget_and_precedes_final_rechecks(self):
        _, sha = self.ordinary()
        self.forward(sha)
        commands = [args for args, _ in self.world.trace]
        at = next(i for i, args in enumerate(commands) if "ls-tree" in args and args[-1] == resolver._DRILLS)
        audit_diffs = [i for i, args in enumerate(commands) if i > at and "--name-status" in args]
        self.assertEqual(len(audit_diffs), 1)
        self.assertTrue(any("--show-toplevel" in args for args in commands[audit_diffs[-1] + 1:]))
        self.assertEqual(self.client.budget.commands, len(commands))
        self.assertTrue(self.client.budget.closed)
        self.assertFalse(self.client.budget.failed)

    def test_expiration_during_drill_read_and_late_source_drift_refuse(self):
        for drift in (False, True):
            case = ForwardAuditTests(); case.setUp()
            try:
                _, sha = case.ordinary()
                def after(args):
                    if "ls-tree" in args and args[-1] == resolver._DRILLS:
                        if drift: (case.repo / ".git" / "shallow").write_bytes(b"changed")
                        else: case.clock.now += 800
                case.world.after = after
                case.refused(sha)
            finally: case.doCleanups()

    def test_missing_role_range_and_same_source_nonempty_rows_are_not_complete(self):
        from dataclasses import replace
        base, sha = self.ordinary(); result = self.forward(sha)
        for changed in (replace(result.audit, changes=()),
                        replace(result.audit, changes=(replace(result.audit.changes[0], roles=("database-origin",)),))):
            with self.assertRaises(policy.PolicyRefusal):
                resolver.validate_forward_audit(changed, candidate_sha=sha, database_base_source=base,
                    latest_record_sha=base, selected_web_source_sha=base)
        same = resolver.ForwardAuditMaterial(base, result.audit.drills, (
            resolver.ChangedRange(("database-origin", "latest-record", "selected-web"), base, base,
                (resolver.ChangedPath("M", "file.py"),)),))
        with self.assertRaises(policy.PolicyRefusal):
            resolver.validate_forward_audit(same, candidate_sha=base, database_base_source=base,
                latest_record_sha=base, selected_web_source_sha=base)


class ForwardPendingTests(unittest.TestCase):
    setUp = ResolverTests.setUp
    add = ResolverTests.add
    profile = ForwardResolverTests.profile
    candidate = ForwardResolverTests.candidate
    forward = ForwardResolverTests.forward
    refused = ForwardResolverTests.refused
    ordinary = ForwardAuditTests.ordinary
    WORKFLOW = ForwardResolverTests.WORKFLOW
    MAIN = ForwardResolverTests.MAIN
    RECIPE = ForwardResolverTests.RECIPE

    @classmethod
    def setUpClass(cls):
        ResolverTests.setUpClass.__func__(cls)
        args = test_pending_plan.fixture()
        plan = pending_source.plan
        target_name = plan.PREFIX + "catalog-v1.json"
        target = json.loads(args["files"][target_name])
        target["objects"] = [*json.loads(cls.source_bytes[policy.GOLDEN])["objects"],
            *(row for row in target["objects"] if row["identity"] not in ("organizations", "organizations.id"))]
        globals_ = [*json.loads(cls.source_bytes[policy.GLOBALS]),
            *(row for row in json.loads(args["files"][plan.GLOBALS]) if row["name"] != "organizations")]
        cls.active = dict(active_catalog=cls.source_bytes[policy.GOLDEN],
                          active_globals=cls.source_bytes[policy.GLOBALS], active_history=cls.source_bytes[policy.HISTORY])
        files = {target_name: plan.canonical(target), plan.GLOBALS: plan.canonical(globals_)}
        document = json.loads(args["files"][plan.PLAN])
        document.update({key + "_file_sha256": plan.digest(raw) for key, raw in cls.active.items()})
        document["globals_file_sha256"] = plan.digest(files[plan.GLOBALS])
        document["entries"][0].update(catalog_file_sha256=plan.digest(files[target_name]),
                                     globals_sha256=plan.digest(files[plan.GLOBALS]))
        files[plan.PLAN] = plan.canonical(document)
        cls.pending_files = files

    def install(self, sha, files=None):
        files = self.pending_files if files is None else files
        self.world.files.update({(sha, path): raw for path, raw in files.items()})

    def replay(self, result):
        return pending_source.replay(result.pending, candidate_sha=result.candidate.sha,
            previous_sha=result.resolved.latest.record_sha, **self.active)

    def material_transport(self, change):
        # Alter only controlled outputs after the existing source audit starts;
        # prior classifier reads remain real fixed-source parser inputs.
        original = self.world.git
        active = False
        def response(args):
            nonlocal active
            if args[:2] == ["ls-tree", "-z"] and args[-1] == resolver._DRILLS:
                active = True
            result = original(args)
            return change(args, result) if active else result
        self.world.git = response

    def test_complete_real_v0_target_is_raw_bound_dormant_and_not_live(self):
        base, sha = self.ordinary(); self.install(sha)
        result = self.forward(sha)
        projection = self.replay(result)
        self.assertEqual(len(projection.plan.entries[0].objects), 4452)
        self.assertEqual((result.pending.candidate_sha, result.pending.previous_sha), (sha, base))
        self.assertEqual(result.pending.candidate_files,
            tuple(pending_source.PendingBlob(path, raw) for path, raw in sorted(self.pending_files.items())))
        self.assertEqual(result.pending.previous_files, ())
        self.assertEqual((result.classification.kind, result.classification.dormant, result.classification.reasons),
                         ("expand", True, ()))
        self.assertIn("producer-class-unsupported", result.classification.requirements)
        self.assertEqual(result.candidate.catalog, self.v0.catalog)
        self.assertEqual(result.resolved.latest.live.origin_source.sha, base)
        self.assertEqual(result.resolved.latest.live.catalog, self.v0.catalog)
        with self.assertRaises(FrozenInstanceError): result.pending.previous_sha = sha
        with self.assertRaises(FrozenInstanceError): result.pending.candidate_files[0].raw = b"other"

    def test_explicit_empty_observation_is_not_historical_none(self):
        from dataclasses import replace
        base, sha = self.ordinary()
        result = self.forward(sha)
        self.assertEqual(result.pending, pending_source.PendingMaterial(sha, base, (), ()))
        self.assertIsNone(self.replay(result).plan)
        historical = replace(result, pending=None)
        self.assertIsNone(historical.pending)
        self.assertNotEqual(result, historical)
        # The old wire lists fields explicitly: it has not acquired this new
        # observation. Codec/request/producer migration is a separate lease.

    def test_existing_bundle_continuity_and_same_source_observation(self):
        for same in (False, True):
            case = ForwardPendingTests(); case.setUp()
            try:
                base, candidate = case.ordinary(); case.install(base)
                sha = base if same else candidate
                case.install(sha)
                result = case.forward(sha)
                self.assertEqual((result.classification.kind, result.classification.reasons), ("none", ()))
                self.assertEqual(result.pending.candidate_files, result.pending.previous_files)
                self.assertEqual(case.replay(result).binding.candidate_digest, case.replay(result).binding.previous_digest)
                commands = [args for args, _ in case.world.trace]
                at = next(i for i, args in enumerate(commands) if "ls-tree" in args and args[-1] == resolver._DRILLS)
                prefixes = [args for args in commands[at + 1:] if "--name-only" in args
                            and args[-1] == pending_source.plan.PREFIX.rstrip("/")]
                self.assertEqual(len(prefixes), 1 if same else 2)
            finally: case.doCleanups()

    def test_removal_replacement_and_malformed_source_keep_bytes_but_block_preview(self):
        for variant in ("removed", "replacement", "malformed"):
            with self.subTest(variant=variant):
                case = ForwardPendingTests(); case.setUp()
                try:
                    base, sha = case.ordinary(); case.install(base)
                    files = dict(case.pending_files)
                    if variant == "replacement":
                        doc = json.loads(files[pending_source.plan.PLAN]); doc["steps"][0]["source_sha256"] = "f" * 64
                        files[pending_source.plan.PLAN] = pending_source.plan.canonical(doc)
                    elif variant == "malformed": files[pending_source.plan.PLAN] = b"PRIVATE_INVALID_JSON"
                    if variant != "removed": case.install(sha, files)
                    result = case.forward(sha)
                    self.assertEqual(result.pending.previous_files,
                        tuple(pending_source.PendingBlob(path, raw) for path, raw in sorted(case.pending_files.items())))
                    expected = () if variant == "removed" else tuple(pending_source.PendingBlob(path, raw) for path, raw in sorted(files.items()))
                    self.assertEqual(result.pending.candidate_files, expected)
                    self.assertIn("classifier-v2-invalid:pending-material", result.classification.reasons)
                    self.assertIn("producer-class-unsupported", result.classification.requirements)
                    self.assertNotIn("PRIVATE", repr(result.classification))
                    with self.assertRaises(pending_source.PendingSourceRefusal): case.replay(result)
                finally: case.doCleanups()

    def test_rollback_material_uses_latest_record_not_origin_or_selected_web(self):
        old = self.add(version=1, images=self.boot["images"])
        self.profile(old["sha"]); self.install(old["sha"])
        origin = self.add(); self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        self.world.files[self.boot["sha"], self.RECIPE] = b"FROM scratch\n"
        sha = self.candidate(); self.install(sha)
        result = self.forward(sha)
        self.assertEqual((result.pending.previous_sha, result.database_base_source, result.previous_web_source_sha),
                         (old["sha"], origin["sha"], self.boot["sha"]))
        self.assertEqual(result.pending.previous_files, result.pending.candidate_files)
        self.assertEqual(self.replay(result).binding.previous_sha, old["sha"])
        ranges = [args[-1] for args, _ in self.world.trace if "rev-list" in args]
        self.assertEqual(ranges.count(old["sha"] + ".." + sha), 1)
        self.assertEqual(ranges.count(origin["sha"] + ".." + sha), 4)

    def test_latest_range_checks_transient_removal_even_when_origin_range_is_empty(self):
        old = self.add(version=1); self.profile(old["sha"]); self.install(old["sha"])
        origin = self.add(); self.profile(origin["sha"]); self.install(origin["sha"])
        self.add(kind="rollback", target=old)
        missing = self.profile("8" * 40)
        self.world.history[old["sha"], origin["sha"]] = [missing, origin["sha"]]
        result = self.forward(origin["sha"])
        # Equal endpoints alone replay successfully; the independently read
        # latest-source history is essential and must still block admission.
        self.assertIsNotNone(self.replay(result).plan)
        self.assertIn("classifier-v2-invalid:pending-latest-record", result.classification.reasons)
        self.assertIn("producer-class-unsupported", result.classification.requirements)

    def test_latest_transient_introduction_is_seen_with_both_endpoint_inventories_empty(self):
        old = self.add(version=1); self.profile(old["sha"])
        origin = self.add(); self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        transient = self.profile("8" * 40); self.install(transient)
        self.world.history[old["sha"], origin["sha"]] = [transient, origin["sha"]]
        result = self.forward(origin["sha"])
        self.assertEqual((result.pending.candidate_files, result.pending.previous_files), ((), ()))
        self.assertIn("classifier-v2-invalid:pending-latest-record", result.classification.reasons)

    def test_latest_introduction_upgrades_only_none_and_never_clears_origin_failure(self):
        for bad_origin in (False, True):
            case = ForwardPendingTests(); case.setUp()
            try:
                old = case.add(version=1); case.profile(old["sha"])
                origin = case.add(); case.profile(origin["sha"]); case.install(origin["sha"])
                case.add(kind="rollback", target=old)
                sha = case.candidate(); case.install(sha)
                if bad_origin:
                    removed = case.profile("8" * 40)
                    case.world.history[origin["sha"], sha] = [removed, sha]
                result = case.forward(sha)
                self.assertEqual((result.classification.kind, result.classification.dormant), ("expand", True))
                if bad_origin:
                    self.assertIn("classifier-v2-invalid:pending", result.classification.reasons)
                    self.assertIn("pending-format-unsupported", result.classification.reasons)
                else: self.assertEqual(result.classification.reasons, ())
            finally: case.doCleanups()

    def test_unsafe_unknown_duplicate_or_truncated_inventory_is_hard_refusal(self):
        prefix = pending_source.plan.PREFIX
        variants = [b"PRIVATE\0", (prefix + "extra.json\0").encode(),
                    (prefix + "catalog-v1.json").encode(), b"\xff\0",
                    ((prefix + "catalog-v1.json\0") * 2).encode(),
                    b"".join((prefix + f"catalog-v{i}.json\0").encode() for i in range(1, 5))]
        for raw in variants:
            with self.subTest(length=len(raw)):
                case = ForwardPendingTests(); case.setUp()
                try:
                    _, sha = case.ordinary()
                    def change(args, result):
                        if args[:4] == ["ls-tree", "-r", "--name-only", "-z"] and args[-1] == prefix.rstrip("/"):
                            return rt.runtime.Capture(0, raw, b"")
                        return result
                    case.material_transport(change); case.refused(sha)
                finally: case.doCleanups()

    def test_nonregular_missing_and_oversized_listed_blobs_are_hard_refusal(self):
        for variant in ("mode", "missing", "oversize"):
            with self.subTest(variant=variant):
                case = ForwardPendingTests(); case.setUp()
                try:
                    _, sha = case.ordinary(); case.install(sha)
                    pending_blob = False
                    def change(args, result):
                        nonlocal pending_blob
                        if args[:2] == ["ls-tree", "-z"] and args[-1].startswith(pending_source.plan.PREFIX):
                            pending_blob = True
                            if variant == "mode": return rt.runtime.Capture(0, result.stdout.replace(b"100644", b"120000"), b"")
                            if variant == "missing": return rt.runtime.Capture(0, b"", b"")
                        if pending_blob and variant == "oversize" and args[:2] == ["cat-file", "-s"]:
                            return rt.runtime.Capture(0, str(pending_source.MAX_FILE_BYTES + 1).encode(), b"")
                        return result
                    case.material_transport(change); case.refused(sha)
                finally: case.doCleanups()

    def test_combined_material_cap_is_hard_and_counts_both_occurrences(self):
        base, sha = self.ordinary(); self.install(base); self.install(sha)
        single = sum(map(len, self.active.values())) + sum(map(len, self.pending_files.values()))
        with mock.patch.object(pending_source, "MAX_MATERIAL_BYTES", single): self.refused(sha)

    def test_pending_capture_spends_original_budget(self):
        for failure in ("deadline", "calls", "bytes"):
            case = ForwardPendingTests(); case.setUp()
            try:
                _, sha = case.ordinary(); case.install(sha)
                active = False
                def after(args):
                    nonlocal active
                    if "ls-tree" in args and args[-1] == resolver._DRILLS: active = True
                    if active and "--name-only" in args and args[-1] == pending_source.plan.PREFIX.rstrip("/"):
                        if failure == "deadline": case.clock.now += 800
                        elif failure == "calls": case.client.budget.commands = rt.MAX_COMMANDS
                        else: case.client.budget.captured_bytes = rt.MAX_BYTES
                case.world.after = after; case.refused(sha)
            finally: case.doCleanups()

    def test_latest_history_spends_original_budget_without_second_classifier(self):
        old = self.add(version=1); self.profile(old["sha"])
        origin = self.add(); self.profile(origin["sha"])
        self.add(kind="rollback", target=old)
        sha = self.candidate()
        def after(args):
            if "rev-list" in args and args[-1] == old["sha"] + ".." + sha:
                self.clock.now += 800
        self.world.after = after
        self.refused(sha)
        ranges = [args[-1] for args, _ in self.world.trace if "rev-list" in args]
        self.assertEqual(ranges.count(old["sha"] + ".." + sha), 1)

    def test_latest_pending_introduction_preserves_data_class_precedence(self):
        old = self.add(version=1); self.profile(old["sha"])
        origin = self.add(); self.profile(origin["sha"]); self.install(origin["sha"])
        self.add(kind="rollback", target=old)
        sha = self.candidate(); self.install(sha)
        self.world.files[(sha, policy.SEEDS)] += b"\n"
        result = self.forward(sha)
        self.assertEqual((result.classification.kind, result.classification.dormant), ("data", True))
        self.assertEqual(result.classification.reasons, ())
        self.assertIn("producer-class-unsupported", result.classification.requirements)

    def test_final_git_and_complete_inventory_refresh_follow_pending_capture(self):
        base, sha = self.ordinary()
        result = self.forward(sha)
        commands = [args for args, _ in self.world.trace]
        audit = next(i for i, args in enumerate(commands) if "ls-tree" in args and args[-1] == resolver._DRILLS)
        pending = [i for i, args in enumerate(commands) if i > audit and "--name-only" in args
                   and args[-1] == pending_source.plan.PREFIX.rstrip("/")]
        self.assertEqual(len(pending), 2)
        self.assertTrue(any("--show-toplevel" in args for args in commands[pending[-1] + 1:]))
        self.assertTrue(any(args[0] == str(self.gh) for args in commands[pending[-1] + 1:]))
        self.assertEqual(self.client.budget.commands, len(commands))
        self.assertTrue(self.client.budget.closed)
        self.assertFalse(self.client.budget.failed)
        self.assertEqual((result.pending.candidate_sha, result.pending.previous_sha), (sha, base))

    def test_source_or_main_drift_after_pending_read_still_refuses(self):
        for change in ("source", "main"):
            case = ForwardPendingTests(); case.setUp()
            try:
                _, sha = case.ordinary()
                active = False
                def after(args):
                    nonlocal active
                    if "ls-tree" in args and args[-1] == resolver._DRILLS: active = True
                    if active and "--name-only" in args and args[-1] == pending_source.plan.PREFIX.rstrip("/"):
                        if change == "source": (case.repo / ".git" / "shallow").write_bytes(b"changed")
                        else: case.world.main = "b" * 40
                case.world.after = after; case.refused(sha)
            finally: case.doCleanups()


if __name__ == "__main__":
    unittest.main()
