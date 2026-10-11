"""Concrete source-to-CLI integration with synthetic command captures only."""
from dataclasses import replace
import copy
import json
from pathlib import Path
import unittest
from unittest import mock

import cli
import test_cli
import test_compat as engine_tests
import test_record_resolver as source_tests
import test_compat_evidence as policy_tests


class ResolutionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source_tests.ForwardResolverTests.setUpClass()
        source_tests.ForwardPendingTests.setUpClass()

    def setUp(self):
        self.case = source_tests.ForwardResolverTests()
        self.case.setUp()
        self.addCleanup(self.case.doCleanups)
        old = self.case.add(version=1, images=self.case.boot["images"])
        self.case.profile(old["sha"], body=0)
        self.case.world.files[(self.case.boot["sha"], self.case.RECIPE)] = b"FROM scratch\n# actual old source\n"
        self.origin = self.case.add()
        self.case.profile(self.origin["sha"])
        self.case.add(kind="rollback", target=old)
        self.case.candidate(test_cli.SHA)
        env = test_cli.environment(); env["GITHUB_RUN_ID"] = "100"
        self.ctx = cli.context(env)
        self.count = 0

    def client(self, deadline):
        self.count += 1
        work = self.case.root / ("client-" + str(self.count)); work.mkdir()
        for name in ("gh-config", "gh-work"):
            (work / name).mkdir()
        env = {"COMPAT_GH_BIN":str(self.case.gh), "GH_TOKEN":"synthetic-not-a-credential"}
        with mock.patch.object(cli.runtime, "bounded_command", side_effect=self.case.world):
            client = cli._client(work, env, deadline)
        return client

    def preview(self):
        deadline = cli.images._Deadline(clock=self.case.clock)
        deadline.end = deadline.now() + 240
        client = self.client(deadline)
        with mock.patch.object(cli.runtime, "bounded_command", side_effect=self.case.world):
            snap = cli.preview(self.case.repo, self.ctx, client, git_binary=self.case.git, deadline=deadline)
        self.assertTrue(client.budget.closed)
        return snap

    def raw(self, snap, *, negative=False):
        value = test_cli.result(self.ctx)
        value.update(previous_release=snap["selected"]["release"],
                     previous_web_digest=snap["selected"]["web_digest"])
        value["classification"] = {key: snap["preview"][key] for key in ("kind", "dormant")}
        if negative:
            value["old_server_ok"]["reverse"] = False
        return cli.proof.result_bytes(value)

    def execute(self, snap, *, negative=False):
        deadline = cli.images._Deadline(clock=self.case.clock)
        client = self.client(deadline)
        with mock.patch.object(cli.runtime, "bounded_command", side_effect=self.case.world), \
             mock.patch.object(cli.compat, "_run_resolved_experiment", return_value=self.raw(snap, negative=negative)) as engine:
            try:
                value = cli.execute(self.case.repo, self.ctx, client, snap,
                    git_binary=self.case.git, deadline=deadline, docker_binary="/synthetic-docker",
                    config_dir=self.case.root / "docker", image_work_dir=self.case.root / "images", buildx_binary=None)
            except Exception:
                engine.assert_not_called()
                raise
        self.assertTrue(client.budget.closed)
        self.assertIs(engine.call_args.kwargs["deadline"], deadline)
        self.assertEqual(engine.call_args.args[1].database_base_source, self.origin["sha"])
        return value

    def test_concrete_preview_keeps_record_image_and_retained_origin_distinct(self):
        snap = self.preview()
        self.assertEqual(snap["version"], 4)
        self.assertEqual(snap["latest"]["sha"], "1" * 40)
        self.assertEqual(snap["selected_web_source_sha"], self.case.boot["sha"])
        self.assertEqual(snap["database_base"]["source_sha"], "2" * 40)
        self.assertEqual(snap["database_base"]["origin"]["release"], self.origin["release"])
        self.assertEqual(snap["preview"], {"kind":"none", "dormant":False, "blocked":False})
        self.assertEqual(snap["pending"]["previous_sha"], snap["latest"]["sha"])
        self.assertNotEqual(snap["pending"]["previous_sha"], snap["database_base"]["source_sha"])
        self.assertEqual(snap["pending"]["candidate_files"], [])
        self.assertIsNone(cli.records._GIT_OUTPUT_TRANSPORT.get())

    def test_concrete_reresolution_matches_then_preserves_actual_negative(self):
        snap = self.preview()
        raw = self.execute(snap, negative=True)
        self.assertFalse(cli.accepted(json.loads(raw)))
        context = cli.evidence_context(raw, snap, self.ctx)
        self.assertEqual(context["version"], 3)
        pair = policy_tests.pack(raw, context)
        measured = cli.compat_evidence.parse_authenticated_pair(pair, policy_tests.attempt())
        self.assertFalse(measured.positive)
        self.assertEqual(measured.selected_web_source_sha, snap["selected_web_source_sha"])
        self.assertEqual(measured.database_base.source_sha, self.origin["sha"])
        self.assertEqual(measured.pending_binding, cli.pending_binding.parse_wire(snap["pending"]))

    def test_each_preview_origin_or_actual_source_disagreement_refuses_before_engine(self):
        snap = self.preview()
        mutations = [lambda s:s.update(selected_web_source_sha="f" * 40),
                     lambda s:s["database_base"].update(source_sha="f" * 40),
                     lambda s:s["database_base"]["origin"].update(manifest_sha256="f" * 64),
                     lambda s:s["database_base"]["catalog"].update(seeds_sha256="f" * 64),
                     lambda s:s["database_base"]["catalog"].update(data_steps_sha256="f" * 64)]
        for index, mutate in enumerate(mutations):
            changed = copy.deepcopy(snap); mutate(changed)
            with self.subTest(index=index), self.assertRaisesRegex(cli.proof.EvidenceError, "cli-resolution-changed"):
                self.execute(changed)

    def test_current_latest_drift_between_jobs_refuses_before_engine(self):
        snap = self.preview()
        latest = self.case.add()
        self.case.profile(latest["sha"])
        with self.assertRaisesRegex(cli.proof.EvidenceError, "cli-resolution-changed"):
            self.execute(snap)

    def test_constructor_time_is_inside_original_preview_budget(self):
        deadline = cli.images._Deadline(clock=self.case.clock)
        deadline.end = deadline.now() + 240
        with mock.patch.object(self.case.world, "after", lambda argv:setattr(self.case.clock, "now", deadline.end)):
            with self.assertRaises(cli.record_transport.TransportRefusal):
                self.client(deadline)
        self.assertEqual(len(self.case.world.trace), 1)

    def test_legacy_snapshot_cannot_be_silently_upgraded(self):
        snap = self.preview()
        for version in (1, 2, 3, True):
            changed = copy.deepcopy(snap); changed["version"] = version
            with self.assertRaises(cli.proof.EvidenceError): cli.validate_snapshot(changed, self.ctx)
        for field in ("database_base", "selected_web_source_sha", "pending"):
            changed = copy.deepcopy(snap); del changed[field]
            with self.assertRaises(cli.proof.EvidenceError): cli.validate_snapshot(changed, self.ctx)

    def test_context3_positive_roundtrip_qualifies_only_matching_origin_and_inventory(self):
        snap = self.preview(); raw = self.execute(snap)
        context = cli.evidence_context(raw, snap, self.ctx)
        pair = policy_tests.pack(raw, context)
        latest = cli.compat_evidence.ReleaseIdentity(**snap["latest"])
        base = cli.compat_evidence._database_base_dict(snap["database_base"], latest)
        req = policy_tests.request(latest=latest, base=base, web_source=snap["selected_web_source_sha"])
        binding = cli.pending_binding.parse_wire(snap["pending"])
        req = replace(req, source=replace(req.source,
            expected_after_catalog_sha256=json.loads(raw)["catalog_sha256"], pending_binding=binding,
            catalog_authority=tuple(cli.compat_evidence.CatalogBlob(path, digest)
                for path, digest in sorted(cli.compat.catalog._PINS.items()))))
        hist = policy_tests.history(policy_tests.attempt(measurement=pair))
        qualified = cli.compat_evidence.require_compat(req, hist)
        self.assertEqual(qualified.database_base, base)
        self.assertEqual(qualified.pending_binding, binding)
        stale = replace(req, source=replace(req.source, previous_web_source_sha=latest.sha))
        with self.assertRaises(cli.compat_evidence.CompatRefusal):
            cli.compat_evidence.require_compat(stale, hist)

    def test_valid_pending_preview_and_fresh_resolution_retain_supported_dormant_class(self):
        files = source_tests.ForwardPendingTests.pending_files
        self.case.world.files.update({(self.ctx.sha, path): raw for path, raw in files.items()})
        snap = self.preview()
        self.assertEqual(snap["preview"], {"kind": "expand", "dormant": True, "blocked": False})
        self.assertEqual(snap["pending"]["target_version"], 1)
        self.assertEqual(snap["pending"]["previous_sha"], snap["latest"]["sha"])
        self.assertEqual(len(snap["pending"]["candidate_files"]), 3)
        self.assertEqual(snap["pending"]["previous_files"], [])
        raw = self.execute(snap)
        context = cli.evidence_context(raw, snap, self.ctx)
        measured = cli.compat_evidence.parse_authenticated_pair(
            policy_tests.pack(raw, context), policy_tests.attempt())
        self.assertEqual(measured.classification, cli.compat_evidence.Classification("expand", True))
        self.assertEqual(measured.pending_binding, cli.pending_binding.parse_wire(snap["pending"]))

    def test_complete_malformed_pending_never_becomes_empty_snapshot_binding(self):
        files = dict(source_tests.ForwardPendingTests.pending_files)
        files[cli.pending_binding.source.plan.PLAN] = b"PRIVATE_INVALID_JSON"
        self.case.world.files.update({(self.ctx.sha, path): raw for path, raw in files.items()})
        with self.assertRaisesRegex(cli.proof.EvidenceError, "^catalog-pending-invalid$"):
            self.preview()


class SourceClosureProducerTests(unittest.TestCase):
    """Real source/CLI/engine/proof path; only service/build edges are synthetic.

    Synthetic declaration inputs are not Go generator or hosted evidence. No
    resolver, classifier, engine entrypoint, evaluator or qualifier is replaced.
    """
    @classmethod
    def setUpClass(cls):
        engine_tests.ResolvedExperimentTests.setUpClass()

    def setUp(self):
        self.engine = engine_tests.ResolvedExperimentTests()
        self.engine.setUp()
        self.addCleanup(self.engine.doCleanups)
        self.case = self.engine.case
        env = test_cli.environment(attempt='2')
        env['GITHUB_RUN_ID'] = '12345'
        self.ctx = cli.context(env)
        self.clients = []

    def install(self, *, prior_seed_delta=False):
        self.ci_base, self.manifests = engine_tests.source_closure_fixture(
            self.case, self.engine.resolution.database_base_source, self.ctx.sha,
            prior_seed_delta=prior_seed_delta)

    def capture(self, argv, **options):
        if str(argv[0]) in (str(self.case.git), str(self.case.gh)):
            return self.case.world(argv, **options)
        return self.engine.w.owner.raw_capture(argv, **options)

    def client(self, deadline):
        work = self.case.root / ('closure-cli-' + str(len(self.clients)))
        work.mkdir()
        for name in ('gh-config', 'gh-work'):
            (work / name).mkdir()
        with mock.patch.object(cli.runtime, 'bounded_command', side_effect=self.capture):
            client = cli._client(work, {'COMPAT_GH_BIN': str(self.case.gh),
                'GH_TOKEN': source_tests.support.GH_TOKEN}, deadline)
        self.clients.append(client)
        return client

    def preview(self):
        deadline = cli.images._Deadline(clock=self.engine.w.clock)
        deadline.end = deadline.now() + 600
        client = self.client(deadline)
        with mock.patch.object(cli.runtime, 'bounded_command', side_effect=self.capture):
            snap = cli.preview(self.case.repo, self.ctx, client,
                               git_binary=self.case.git, deadline=deadline)
        self.assertTrue(client.budget.closed)
        return snap

    def execute(self, snapshot):
        deadline = self.engine.deadline
        original_end = deadline.end
        client = self.client(deadline)
        def build(repo, actual, binary, config, work, actual_deadline, *, buildx_binary):
            self.assertIs(actual_deadline, deadline)
            self.assertEqual(actual_deadline.end, original_end)
            self.assertEqual(actual.candidate.sha, self.ctx.sha)
            self.assertEqual(actual.classification.migration_changes,
                             ('generated migration closure changed',))
            self.assertEqual(actual.classification.reasons, ())
            self.assertEqual(actual.classification.requirements,
                             ('compat-required', 'generated-migration-closure-ci'))
            self.assertEqual(cli._resolved_snapshot(actual, self.ctx), snapshot)
            self.engine.w.event('admission.resolved')
            return self.engine.admission
        with mock.patch.object(cli.runtime, 'bounded_command', side_effect=self.capture), \
             mock.patch.object(cli.images, '_admit_resolved', side_effect=build):
            raw = cli.execute(self.case.repo, self.ctx, client, snapshot,
                git_binary=self.case.git, deadline=deadline, docker_binary='docker',
                config_dir=self.engine.p, image_work_dir=self.engine.p, buildx_binary=None)
        self.assertTrue(client.budget.closed)
        self.assertEqual(deadline.end, original_end)
        self.assertTrue(self.engine.w.owner._closed)
        return raw

    def source_comparison(self, base):
        reader = source_tests.resolver.source_git.ImmutableGit(self.case.repo, self.case.git,
            capture=self.case.world, clock=self.engine.w.clock)
        with reader.operation():
            legacy = source_tests.schema_change.classify(self.case.repo, base, self.ctx.sha)
            current = source_tests.schema_change.classify_v2(self.case.repo, base, self.ctx.sha)
        return legacy, current

    def assert_pair(self, raw, snap, *, positive):
        value = cli.validate_result(raw, snap, self.ctx)
        self.assertIs(cli.accepted(value), positive)
        context = cli.evidence_context(raw, snap, self.ctx)
        self.assertEqual(context['version'], 3)
        self.assertEqual(context['sha'], self.ctx.sha)
        self.assertEqual(context['pending'], snap['pending'])
        cli.validate_evidence(cli.canonical(context), raw, snap, self.ctx)
        self.engine.assert_closure_phases(value)
        return value

    def test_closure_only_preview_fresh_resolution_and_real_proof_need_every_phase(self):
        self.install()
        legacy, current = self.source_comparison(self.ci_base)
        self.assertEqual(legacy.reasons, ('migration-path-changed:closure',))
        self.assertEqual((current.kind, current.dormant, current.reasons), ('none', False, ()))
        snap = self.preview()
        self.assertEqual(snap['preview'], {'kind': 'none', 'dormant': False, 'blocked': False})
        preview_calls = len(self.case.world.trace)
        raw = self.execute(snap)
        self.assertGreater(len(self.case.world.trace), preview_calls)
        self.assert_pair(raw, snap, positive=True)
        self.assertEqual(self.engine.w.trace.count('admission.resolved'), 1)

    def test_ci_none_does_not_replace_actual_signed_range_dormant_data(self):
        self.install(prior_seed_delta=True)
        _, current = self.source_comparison(self.ci_base)
        self.assertEqual((current.kind, current.dormant, current.reasons), ('none', False, ()))
        snap = self.preview()
        self.assertEqual(snap['database_base']['source_sha'], self.engine.resolution.database_base_source)
        self.assertNotEqual(snap['database_base']['source_sha'], self.ci_base)
        self.assertEqual(snap['preview'], {'kind': 'data', 'dormant': True, 'blocked': False})
        raw = self.execute(snap)
        result = self.assert_pair(raw, snap, positive=True)
        self.assertEqual(result['classification'], {'kind': 'data', 'dormant': True})

    def test_closure_only_completed_negative_remains_negative_canonical_pair(self):
        self.install(); snap = self.preview()
        self.engine.w.false_case = 'read'
        raw = self.execute(snap)
        result = self.assert_pair(raw, snap, positive=False)
        self.assertFalse(result['old_server_ok']['read'])
        self.assertFalse(result['old_server_ok']['reverse'])

    def test_closure_only_final_capture_failure_returns_no_evidence(self):
        self.install(); snap = self.preview()
        self.engine.w.fail = 'catalog.final'
        with self.assertRaises(cli.proof.InfrastructureFailure):
            self.execute(snap)
        self.assertIn('pg.control', self.engine.w.trace)
        self.assertTrue(self.engine.w.owner._closed)

    def test_changed_latest_after_closure_preview_refuses_before_build_or_stack(self):
        self.install(); snap = self.preview()
        changed = self.case.add(); self.case.profile(changed['sha'])
        with self.assertRaisesRegex(cli.proof.EvidenceError, '^cli-resolution-changed$'):
            self.execute(snap)
        self.assertNotIn('admission.resolved', self.engine.w.trace)
        self.assertFalse(hasattr(self.engine.w, 'owner'))


if __name__ == "__main__": unittest.main(verbosity=2)
