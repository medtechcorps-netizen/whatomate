"""Fake read-only GH and disposable Git; no live GitHub or workspace Git."""
import copy
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
import unittest
from unittest import mock

import record_transport as rt
import release_record as records
import ship_common as common
import source_git
import test_ship_release_record_v2 as wire
import test_ship_support as support


class Clock:
    def __init__(self): self.now = 100.0
    def __call__(self): return self.now


class World:
    def __init__(self):
        self.fake = support.FakeGh()
        self.main = "a" * 40
        self.calls = []
        self.overrides = {}
        self.after_verify = None
        self.nonzero = False

    def add(self, document, **kwargs):
        return self.fake.add_record(document, **kwargs)

    def __call__(self, argv, **options):
        self.calls.append((list(argv), dict(options)))
        if self.nonzero:
            return rt.runtime.Capture(1, b"private stdout", b"private stderr")
        args = argv[1:]
        if args == ["--version"] or args[:2] == ["attestation", "verify"]:
            result = self.fake(argv, env=options["env"])
            if args[:2] == ["attestation", "verify"] and self.after_verify:
                self.after_verify(args[2])
            return rt.runtime.Capture(result.returncode, result.stdout, result.stderr)
        if args[:3] != ["api", "--method", "GET"]:
            raise AssertionError("unexpected fake command")
        path = args[3]
        if path in self.overrides:
            value = self.overrides[path]
            return rt.runtime.Capture(0, value if type(value) is bytes else common.canonical_file_bytes(value), b"")
        if path == rt.API:
            value = {"id": rt.REPOSITORY_ID, "full_name": common.REPOSITORY}
        elif path == rt.API + "/git/ref/heads/main":
            value = {"ref": "refs/heads/main", "object": {"type": "commit", "sha": self.main}}
        elif path.startswith(rt.API + "/releases?per_page=100&page="):
            page = int(path.rsplit("=", 1)[1])
            value = self.fake.releases[(page-1)*100:page*100]
        elif path.startswith(rt.API + "/tags?per_page=100&page="):
            page = int(path.rsplit("=", 1)[1])
            rows = [{"name": row["ref"].removeprefix("refs/tags/"), "commit": {"sha": row["object"]["sha"]}}
                    for row in self.fake.tags]
            value = rows[(page-1)*100:page*100]
        elif path.startswith(rt.API + "/git/ref/tags/"):
            name = path.removeprefix(rt.API + "/git/ref/tags/")
            value = next(row for row in self.fake.tags if row["ref"] == "refs/tags/" + name)
        elif path.startswith(rt.API + "/releases/tags/"):
            name = path.removeprefix(rt.API + "/releases/tags/")
            value = next(row for row in self.fake.releases if row["tag_name"] == name)
        elif path.startswith(rt.API + "/releases/assets/"):
            identity = int(path.rsplit("/", 1)[1])
            name = next(row["tag_name"] for row in self.fake.releases if row["assets"][0]["id"] == identity)
            return rt.runtime.Capture(0, self.fake.assets[name], b"")
        else:
            raise AssertionError("unexpected fake route")
        return rt.runtime.Capture(0, common.canonical_file_bytes(value), b"")


class ReleaseTransportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binary = self.root / "gh.exe"
        self.binary.write_bytes(b"fake executable never launched")
        self.world = World()
        self.clock = Clock()
        self.serial = 0

    def client(self, *, world=None, deadline=700):
        self.serial += 1
        base = self.root / str(self.serial)
        base.mkdir()
        config, work = base / "config", base / "work"
        config.mkdir(); work.mkdir()
        world = world or self.world
        budget = rt.OperationBudget(deadline, capture=world, clock=self.clock)
        return rt.ReleaseGh(self.binary, support.GH_TOKEN, config, work, budget=budget,
                            base_env={"PATH": "/usr/bin", "SHIP_DO_TOKEN": "private"})

    def first(self):
        return support.manifest(kind="promote", sha="2"*40, images=support.NEW,
            previous="prod-0000", deployed_at="2026-10-04T12:00:00Z")

    def collect(self, client):
        return list(records.collect_authenticated_records(client, work_dir=client.work_dir))

    def test_fixed_contract_queries_typed_variables_and_same_capture_budget(self):
        client = self.client(); client.capture_snapshot()
        calls, original = [], self.world
        def capture(argv, **options):
            if argv[1:5] == ["api", "--method", "POST", "graphql"]:
                calls.append((list(argv), options))
                return rt.runtime.Capture(0, b'{"data":{}}', b"")
            return original(argv, **options)
        client.budget._capture = capture
        before = client.budget.commands
        for stage, kwargs in (("associations", {}), ("labels", {"number": 268}),
                              ("events", {"number": 268, "after": "opaque-end-cursor"})):
            self.assertEqual(client._contract_page(stage, self.world.main,
                byte_allowance=rt._CONTRACT_BYTES, **kwargs), b'{"data":{}}')
            self.assertIsNone(client._contract_request)
        self.assertEqual(client.budget.commands, before + 3)
        for (args, options), stage in zip(calls, ("associations", "labels", "events")):
            self.assertIn("query=" + rt._CONTRACT_QUERIES[stage], args)
            self.assertIn("candidate=" + self.world.main, args)
            self.assertIn("owner=medtechcorps-netizen", args)
            self.assertIn("name=whatomate", args)
            self.assertNotIn(support.GH_TOKEN, args)
            self.assertNotIn("SHIP_DO_TOKEN", options["env"])
            self.assertEqual(options["env"]["GH_HOST"], "github.com")
            self.assertLessEqual(options["stdout_limit"] + options["stderr_limit"], rt._CONTRACT_BYTES)
        number = calls[1][0].index("number=268")
        self.assertEqual(calls[1][0][number - 1], "-F")
        self.assertNotIn("after=opaque-end-cursor", calls[1][0])
        self.assertIn("after=opaque-end-cursor", calls[2][0])

    def test_contract_method_refuses_arbitrary_source_stage_and_variables(self):
        cases = [("mutation", self.world.main, {}), ("associations", "main", {}),
                 ("associations", "b" * 40, {}), ("associations", self.world.main, {"number": 268}),
                 ("labels", self.world.main, {"number": True}),
                 ("labels", self.world.main, {"number": 2**31}),
                 ("labels", self.world.main, {"number": 268, "after": "cursor"}),
                 ("events", self.world.main, {"number": 268, "after": "PRIVATE\n"})]
        for stage, sha, kwargs in cases:
            with self.subTest(stage=stage, kwargs=kwargs):
                client = self.client(); client.capture_snapshot(); before = len(self.world.calls)
                with self.assertRaises(rt.TransportRefusal) as caught:
                    client._contract_page(stage, sha, byte_allowance=rt._CONTRACT_BYTES, **kwargs)
                self.assertNotIn("PRIVATE", str(caught.exception))
                self.assertTrue(client.budget.failed)
                self.assertEqual(len(self.world.calls), before)
        client = self.client(); client.capture_snapshot(); before = len(self.world.calls)
        with self.assertRaises(rt.TransportRefusal):
            client._run(["api", "--method", "POST", "graphql", "-f", "query=mutation { PRIVATE }"], code="ignored")
        self.assertEqual(len(self.world.calls), before)

    def test_contract_reserves_stderr_and_restores_scope_on_failure(self):
        for mode in ("bounded", "stderr", "nonzero"):
            client = self.client(); client.capture_snapshot(); options_seen = []
            def capture(argv, **options):
                options_seen.append(options)
                return rt.runtime.Capture(1 if mode == "nonzero" else 0, b"{}",
                                          b"PRIVATE" if mode == "stderr" else b"")
            client.budget._capture = capture
            call = lambda: client._contract_page("associations", self.world.main,
                                                 byte_allowance=rt._CONTRACT_STDERR + 12)
            if mode == "bounded": self.assertEqual(call(), b"{}")
            else:
                with self.assertRaises(rt.TransportRefusal) as caught: call()
                self.assertNotIn("PRIVATE", str(caught.exception))
                self.assertTrue(client.budget.failed)
            self.assertIsNone(client._contract_request)
            self.assertEqual(options_seen[0]["stdout_limit"], 12)
            self.assertEqual(options_seen[0]["stderr_limit"], rt._CONTRACT_STDERR)

    def test_contract_foreign_scope_replacement_is_preserved_and_poisoned(self):
        client = self.client(); client.capture_snapshot(); foreign = object()
        def capture(argv, **options):
            client._contract_request = foreign
            return rt.runtime.Capture(0, b"{}", b"")
        client.budget._capture = capture
        with self.assertRaises(rt.TransportRefusal):
            client._contract_page("associations", self.world.main, byte_allowance=rt._CONTRACT_BYTES)
        self.assertIs(client._contract_request, foreign)
        self.assertTrue(client.budget.failed)

    def test_mixed_raw_and_final_snapshot_with_real_inherited_verifier(self):
        first, second = self.first(), wire.v2()
        raw1, raw2 = self.world.add(first), self.world.add(second)
        self.world.fake.attest_ship_images(first["images"], first["sha"])
        self.world.fake.attest_ship_images(second["images"], second["sha"])
        client = self.client()
        snapshot = client.capture_snapshot()
        items = self.collect(client)
        self.assertEqual([item.raw for item in items], [raw1, raw2])
        for item in items:
            verified = client.verify_selected_images(item)
            self.assertEqual(verified.record_sha, item.sha)
            self.assertEqual(set(dict(verified.source_shas).values()), {item.sha})
        self.assertIs(client.recheck(snapshot, selected_tag=first["release"], latest_tag=second["release"]), snapshot)
        self.assertFalse(client.budget.closed)
        client.budget.close()
        with self.assertRaises(rt.TransportRefusal): client.releases()
        for args, options in self.world.calls:
            self.assertNotIn("--paginate", args)
            self.assertNotIn("SHIP_DO_TOKEN", options["env"])
            self.assertEqual(options["env"]["GH_HOST"], "github.com")

    def test_bootstrap_images_keep_actual_legacy_source_distinct(self):
        bootstrap = records.load_bootstrap()
        legacy = bootstrap["legacy_signer"]
        self.world.fake.attest_legacy_images(bootstrap["images"], legacy["source_commit"], workflow_sha=legacy["workflow_sha"])
        client = self.client()
        snapshot = client.capture_snapshot()
        self.assertEqual(self.collect(client), [])
        verified = client.verify_selected_images(None)
        self.assertEqual(verified.record_sha, bootstrap["sha"])
        self.assertEqual(set(dict(verified.source_shas).values()), {legacy["source_commit"]})
        client.recheck(snapshot, selected_tag="prod-0000", latest_tag="prod-0000")
        self.assertEqual(sum(a[1:3] == ["attestation", "verify"] for a, _ in self.world.calls), 6)

    def test_later_record_reusing_bootstrap_images_retains_actual_legacy_source(self):
        bootstrap = records.load_bootstrap()
        legacy = bootstrap["legacy_signer"]
        document = self.first()
        document["images"] = dict(bootstrap["images"])
        self.world.add(document)
        self.world.fake.attest_legacy_images(bootstrap["images"], legacy["source_commit"], workflow_sha=legacy["workflow_sha"])
        client = self.client()
        item = self.collect(client)[0]
        verified = client.verify_selected_images(item)
        self.assertEqual(verified.record_sha, document["sha"])
        self.assertEqual(set(dict(verified.source_shas).values()), {legacy["source_commit"]})
        self.assertNotEqual(verified.record_sha, legacy["source_commit"])

    def test_legacy_bad_link_stops_before_later_download(self):
        first = self.first()
        first["previous"] = "prod-20261003T120000Z-11111111"
        self.world.add(first); self.world.add(wire.v2())
        client = self.client()
        with self.assertRaisesRegex(common.ReleaseError, "record-chain-invalid:link"):
            records.resolve_chain(client, None, bootstrap=records.load_bootstrap(), work_dir=client.work_dir)
        downloads = [a for a, _ in self.world.calls if len(a)>4 and "/releases/assets/" in a[4]]
        self.assertEqual(len(downloads), 1)

    def test_v2_operational_guard_remains(self):
        self.world.add(wire.v2())
        client = self.client()
        with self.assertRaisesRegex(common.ReleaseError, "policy-unavailable"):
            records.resolve_chain(client, None, bootstrap=records.load_bootstrap(), work_dir=client.work_dir)

    def test_explicit_full_page_requires_empty_sentinel(self):
        self.world.fake.releases = [{"id": i+1, "tag_name": "v"+str(i)} for i in range(100)]
        self.world.fake.tags = [{"ref": "refs/tags/v"+str(i), "object": {"sha": "1"*40}} for i in range(100)]
        client = self.client()
        self.assertEqual(client.capture_snapshot().records, ())
        paths = [a[4] for a, _ in self.world.calls if a[1:4] == ["api", "--method", "GET"]]
        for endpoint in ("releases", "tags"):
            self.assertIn(rt.API+f"/{endpoint}?per_page=100&page=2", paths)

    def test_page_bound_duplicates_and_malformed_refuse(self):
        for mode in ("bound", "duplicate", "shape", "too-many", "tag-duplicate"):
            with self.subTest(mode=mode):
                world = World()
                if mode == "bound":
                    world.fake.releases = [{"id": i+1, "tag_name": "v"+str(i)} for i in range(2000)]
                elif mode == "duplicate":
                    world.fake.releases = [{"id": 1, "tag_name": "v1"}]*2
                elif mode == "tag-duplicate":
                    world.fake.tags = [{"ref":"refs/tags/v1","object":{"sha":"1"*40}}]*2
                else:
                    world.overrides[rt.API+"/releases?per_page=100&page=1"] = {} if mode == "shape" else [{"id":i+1,"tag_name":"v1"} for i in range(101)]
                client = self.client(world=world)
                with self.assertRaises(rt.TransportRefusal): client.capture_snapshot()
                before = len(world.calls)
                with self.assertRaises(rt.TransportRefusal): client.capture_snapshot()
                self.assertEqual(len(world.calls), before)

    def test_tag_type_target_stray_and_release_identity_refuse(self):
        for mode in ("annotated", "target", "stray", "repository", "asset-id", "digest"):
            with self.subTest(mode=mode):
                world = World(); doc = self.first(); world.add(doc)
                if mode == "annotated": world.fake.tags[0]["object"]["type"] = "tag"
                if mode == "target": world.overrides[rt.API+"/git/ref/tags/"+doc["release"]] = {"ref":"refs/tags/"+doc["release"],"object":{"type":"commit","sha":"9"*40}}
                if mode == "stray": world.fake.releases.clear()
                if mode == "repository": world.overrides[rt.API] = {"id": 42,"full_name":common.REPOSITORY}
                if mode == "asset-id": world.fake.releases[0]["assets"][0]["id"] = True
                if mode == "digest": world.fake.releases[0]["assets"][0]["digest"] = "private"
                with self.assertRaises(rt.TransportRefusal): self.client(world=world).capture_snapshot()

    def test_recheck_refuses_inventory_main_asset_and_exact_selection_drift(self):
        for mode in ("main", "remove", "asset", "tag", "selected-release", "selected-ref"):
            with self.subTest(mode=mode):
                world = World(); doc = self.first(); world.add(doc)
                client = self.client(world=world); snapshot = client.capture_snapshot()
                if mode == "main": world.main = "b"*40
                if mode == "remove": world.fake.releases.clear(); world.fake.tags.clear()
                if mode == "asset": world.fake.releases[0]["assets"][0]["id"] += 1
                if mode == "tag": world.fake.tags[0]["object"]["sha"] = "9"*40
                if mode == "selected-release":
                    other = copy.deepcopy(world.fake.releases[0]); other["id"] += 1
                    world.overrides[rt.API+"/releases/tags/"+doc["release"]] = other
                if mode == "selected-ref":
                    # Change only after the full snapshot comparison, when the
                    # exact selected release is fetched a second time.
                    original = world.__call__
                    def mutate(argv, **kwargs):
                        result = original(argv, **kwargs)
                        if len(argv)>4 and argv[4] == rt.API+"/releases/tags/"+doc["release"]:
                            world.fake.tags[0]["object"]["sha"] = "8"*40
                        return result
                    client.budget._capture = mutate
                with self.assertRaises(rt.TransportRefusal):
                    client.recheck(snapshot, selected_tag=doc["release"], latest_tag=doc["release"])

    def test_recheck_rejects_fabricated_equal_snapshot_and_unknown_selection(self):
        for mode in ("snapshot", "selection"):
            client = self.client(); snapshot = client.capture_snapshot()
            if mode == "snapshot": snapshot = rt.InventorySnapshot(snapshot.repository_id,snapshot.main_sha,(),())
            with self.assertRaises(rt.TransportRefusal):
                client.recheck(snapshot,selected_tag="prod-unknown" if mode=="selection" else "prod-0000",latest_tag="prod-0000")
        self.world.add(self.first())
        client=self.client();snapshot=client.capture_snapshot()
        with self.assertRaises(rt.TransportRefusal):
            client.recheck(snapshot,selected_tag="prod-0000",latest_tag="prod-0000")

    def test_routes_and_direct_signature_or_image_calls_are_closed(self):
        operations = [
            lambda c:c._run(["api","--method","POST",rt.API],code="private"),
            lambda c:c.api_list(rt.API+"/releases",code="private"),
            lambda c:c._run(["api","--method","GET","https://private.invalid"],code="private"),
            lambda c:c._run(["api","--method","GET",rt.API+"/releases/assets/1","-H","Accept: application/octet-stream","--allow-escape-sequences"],code="private"),
            lambda c:c.run(["release","create"],code="private"),
            lambda c:c.verify_file(c.work_dir/records.MANIFEST_ASSET,signer_digest="1"*40,source_digest="1"*40),
            lambda c:c.verify_image(common.IMAGE_REPOSITORY["web"],support.NEW["web"],signer_workflow=records.SHIP_SIGNER,signer_digest="1"*40,source_digest="1"*40,predicate_type=records.SLSA_PREDICATE),
        ]
        for operation in operations:
            client = self.client(); count = len(self.world.calls)
            with self.assertRaises(rt.TransportRefusal) as caught: operation(client)
            self.assertNotIn("private", str(caught.exception))
            self.assertEqual(len(self.world.calls), count)

    def test_unauthenticated_or_changed_raw_cannot_authorize_images(self):
        doc = self.first(); raw = self.world.add(doc)
        for changed in (False, True):
            client = self.client(); client.capture_snapshot()
            raw_value = raw+b" " if changed else raw
            forged = records._AuthenticatedRecord(raw_value,doc["release"],doc["sha"],hashlib.sha256(raw_value).hexdigest())
            with self.assertRaises(rt.TransportRefusal): client.verify_selected_images(forged)

    def test_owned_download_is_exclusive_and_does_not_delete_existing_files(self):
        doc = self.first(); self.world.add(doc)
        client = self.client(); client.capture_snapshot()
        directory = client.work_dir/"record-0"; directory.mkdir(); marker=directory/"keep"; marker.write_text("unchanged")
        with self.assertRaises(rt.TransportRefusal): client.download_manifest(doc["release"],directory)
        self.assertEqual(marker.read_text(),"unchanged")

    def test_download_wrong_size_hash_and_repeated_attempt_refuse(self):
        for mode in ("size", "hash", "repeat"):
            world=World();doc=self.first();raw=world.add(doc)
            if mode=="size": world.fake.releases[0]["assets"][0]["size"] += 1
            if mode=="hash": world.fake.releases[0]["assets"][0]["digest"] = "sha256:"+"0"*64
            client=self.client(world=world);client.capture_snapshot()
            if mode=="repeat": self.assertEqual(client.download_manifest(doc["release"],client.work_dir/"record-0"),raw)
            with self.assertRaises(rt.TransportRefusal): client.download_manifest(doc["release"],client.work_dir/"record-1")

    def test_manifest_changed_during_verifier_or_replaced_file_refuses(self):
        for mode in ("during", "replaced", "noncanonical"):
            world=World();doc=self.first();raw=world.add(doc)
            if mode=="noncanonical":
                world.fake.assets[doc["release"]]=json.dumps(doc,indent=1).encode()
                world.fake.releases[0]["assets"][0]["size"]=len(world.fake.assets[doc["release"]])
            client=self.client(world=world);client.capture_snapshot()
            client.download_manifest(doc["release"],client.work_dir/"record-0")
            path=client.work_dir/"record-0"/records.MANIFEST_ASSET
            if mode=="during": world.after_verify=lambda subject:Path(subject).write_bytes(b"private replacement")
            if mode=="replaced":
                old=path.with_suffix(".old");path.rename(old);path.write_bytes(raw)
            with self.assertRaises(rt.TransportRefusal): client.verify_file(path,signer_digest=doc["sha"],source_digest=doc["sha"])

    def test_tag_mismatched_direct_verification_does_not_register_raw(self):
        doc=self.first();self.world.add(doc,tag_sha="9"*40)
        client=self.client();client.capture_snapshot();client.download_manifest(doc["release"],client.work_dir/"record-0")
        with self.assertRaises(rt.TransportRefusal):
            client.verify_file(client.work_dir/"record-0"/records.MANIFEST_ASSET,signer_digest=doc["sha"],source_digest=doc["sha"])
        self.assertEqual(client._verified,{})

    def test_signature_failure_poisons_the_client(self):
        doc=self.first();self.world.add(doc,attested=False)
        client=self.client()
        with self.assertRaises(common.ReleaseError): self.collect(client)
        self.assertTrue(client.budget.failed)

    def test_certificate_mismatch_cannot_register_an_owned_manifest(self):
        doc=self.first();self.world.add(doc)
        client=self.client();original=self.world
        def wrong_certificate(argv,**options):
            result=original(argv,**options)
            if argv[1:3]==["attestation","verify"]:
                data=common.loads_strict(result.stdout)
                data[0]["verificationResult"]["signature"]["certificate"]["sourceRepositoryRef"]="refs/heads/private"
                return rt.runtime.Capture(0,common.canonical_file_bytes(data),b"")
            return result
        client.budget._capture=wrong_certificate
        with self.assertRaises(common.ReleaseError):self.collect(client)
        self.assertEqual(client._verified,{})
        self.assertTrue(client.budget.failed)

    def test_reentrant_capture_refuses_and_cannot_resume(self):
        client=self.client()
        def recursive(argv,**options):
            return client.budget.capture(argv,**options)
        client.budget._capture=recursive
        with self.assertRaises(rt.TransportRefusal):client.capture_snapshot()
        self.assertTrue(client.budget.failed)
        count=client.budget.commands
        with self.assertRaises(rt.TransportRefusal):client.capture_snapshot()
        self.assertEqual(client.budget.commands,count)

    def test_legacy_predicate_failure_poisons_the_client(self):
        world=World();bootstrap=records.load_bootstrap();legacy=bootstrap["legacy_signer"]
        world.fake.attest_legacy_images(bootstrap["images"],legacy["source_commit"],workflow_sha=legacy["workflow_sha"])
        for rows in world.fake.image_attestations.values(): rows[1]["predicate"]["phase"]="wrong"
        client=self.client(world=world)
        with self.assertRaises(rt.TransportRefusal):client.verify_selected_images(None)
        self.assertTrue(client.budget.failed)

    def test_directory_identity_and_private_transport_failure(self):
        client=self.client();old=client._config.with_name("old");client._config.rename(old);client._config.mkdir()
        count=len(self.world.calls)
        with self.assertRaises(rt.TransportRefusal):client.capture_snapshot()
        self.assertEqual(len(self.world.calls),count)
        client=self.client();self.world.nonzero=True
        with self.assertRaises(rt.TransportRefusal) as caught:client.capture_snapshot()
        self.assertNotIn("private",str(caught.exception));self.assertTrue(client.budget.failed)

    def test_single_deadline_clamps_version_pages_and_late_result(self):
        client=self.client(deadline=110)
        self.assertLessEqual(self.world.calls[0][1]["timeout"],5)
        self.clock.now=107
        client.capture_snapshot()
        self.assertTrue(all(options["timeout"]<=0.75 for _,options in self.world.calls[1:]))
        self.clock.now=110
        count=len(self.world.calls)
        with self.assertRaises(rt.TransportRefusal):client.releases()
        self.assertEqual(len(self.world.calls),count)
        clock=Clock()
        def late(argv,**options):clock.now=105;return rt.runtime.Capture(0,b"complete but late",b"")
        budget=rt.OperationBudget(105,capture=late,clock=clock)
        with self.assertRaises(rt.TransportRefusal):budget.capture(["fake"],env={},timeout=60,stdout_limit=100,stderr_limit=0)
        self.assertTrue(budget.failed)

    def test_aggregate_bounds_bad_clock_and_failure_close(self):
        for mode in ("calls","bytes","nan","backward"):
            client=self.client()
            if mode=="calls":client.budget.commands=rt.MAX_COMMANDS
            if mode=="bytes":client.budget.captured_bytes=rt.MAX_BYTES-10
            if mode=="nan":self.clock.now=float("nan")
            if mode=="backward":self.clock.now=0
            with self.assertRaises(rt.TransportRefusal):client.capture_snapshot()
            client.budget.close();client.budget.close()
            self.assertTrue(client.budget.closed)
            self.clock.now=100

    def test_oversized_and_malformed_capture_poison_without_echo(self):
        for result in (rt.runtime.Capture(0,b"x"*(4*rt.MIB+1),b""),object()):
            client=self.client();client.budget._capture=lambda *a,**kw:result
            with self.assertRaises(rt.TransportRefusal) as caught:client.capture_snapshot()
            self.assertLess(len(str(caught.exception)),100)
            self.assertTrue(client.budget.failed)

    def test_shared_budget_counts_real_disposable_git_and_restores_scope(self):
        repo=support.TempRepo(self.root/"repo")
        base=repo.commit({"README.md":"one\n"})
        head=repo.commit({"README.md":"two\n"})
        git=Path(shutil.which("git")).resolve()
        world=self.world
        def capture(argv,**options):
            return rt.runtime.bounded_command(argv,**options) if argv[0]==str(git) else world(argv,**options)
        budget=rt.OperationBudget(time.monotonic()+90,capture=capture)
        config=self.root/"config-real";work=self.root/"work-real";config.mkdir();work.mkdir()
        client=rt.ReleaseGh(self.binary,support.GH_TOKEN,config,work,budget=budget)
        snapshot=client.capture_snapshot();before=budget.commands
        reader=source_git.ImmutableGit(repo.root,git,limits=source_git.Limits(seconds=budget.remaining()),
                                       capture=budget.capture,clock=budget.clock)
        with reader.operation():
            self.assertEqual(reader.output(repo.root,["cat-file","-t",head],code="ignored"),b"commit\n")
            self.assertEqual(reader.output(repo.root,["merge-base",base,head],code="ignored"),base.encode()+b"\n")
            self.assertEqual(reader.read_blob(head,"README.md"),b"two\n")
        self.assertGreater(budget.commands,before)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())
        client.recheck(snapshot,selected_tag="prod-0000",latest_tag="prod-0000")
        self.assertGreater(budget.captured_bytes,0);budget.close()


if __name__ == "__main__":
    unittest.main()
