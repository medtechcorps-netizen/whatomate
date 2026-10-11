"""Release integration over synthetic adapter results; no live authority.

Concrete source/Actions captures are covered in test_ship_compat/ship_records.
These cases exercise the same-run handoff, writer, retries and drift semantics.
"""
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
import tempfile
import time
import unittest
from unittest import mock

import ship_flow as flow
import test_record_writer as fixture

w, p, r, c = flow.record_writer, flow.policy, flow.records, flow.common


def fields_from(raw):
    doc = c.loads_strict(raw)
    return {**doc, 'workflow_sha': doc['signer']['workflow_sha'], **doc['fingerprints']}


class FlowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fixture.WriterTests.setUpClass()
        cls.forward = fixture.WriterTests.forward
        cls.history = fixture.WriterTests.history
        cls.preflight = flow.ship_compat.NormalPreflight(cls.forward,
            fixture.composition.bridge.qualify(cls.forward, cls.history), cls.history, 1, 1)
        cls.gate = flow.Gate(cls.forward.resolved, cls.preflight, None)
        cls.control = dict(sha=cls.forward.candidate.sha, workflow_sha=cls.forward.candidate.sha,
                           run_id=str(fixture.execution().run_id), run_attempt=1)
        cls.encoded = flow.promotion_outputs(cls.gate, cls.control,
            dict(fixture.images(cls.forward).images), staging='passed')
        cls.handoff = flow.record_handoff.decode(cls.encoded, candidate_sha=cls.control['sha'],
            workflow_sha=cls.control['workflow_sha'], run_id=int(cls.control['run_id']), record_attempt=1)
        cls.publication = replace(fixture.publication(), execution=flow._execution(cls.control))
        cls.raw = w.build_promotion(cls.handoff, cls.publication, cls.forward.resolved)
        cls.fields = fields_from(cls.raw)
        # Unlike a forward capture, a genuine plain reader has no unreleased
        # candidate. This is the specific postdeploy composition regression.
        chain = cls.forward.resolved.chain
        plain = p.normalize_chain(chain.inputs,
            tuple(source for source in chain.sources if source.sha != cls.forward.candidate.sha), chain.ancestors)
        cls.current = replace(cls.forward.resolved, chain=plain,
            inventory=replace(cls.forward.resolved.inventory, main_sha='f' * 40))

    def setUp(self):
        temp = tempfile.TemporaryDirectory(); self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        def work(name):
            result = root / name; result.mkdir(exist_ok=True); return result
        self.ctx = SimpleNamespace(repo_dir=root, work_dir=work,
            env={'SHIP_GH_BIN': str(root / 'gh.exe'), 'GH_TOKEN': 'synthetic',
                 **{'REC_' + key.upper(): value for key, value in self.encoded.items()}})
        self.deadline = time.monotonic() + 600

    def record(self, fields=None, current=None, control=None):
        with mock.patch.object(flow.ship_records, 'read', return_value=current or self.current) as read:
            result = flow.record_bytes(self.ctx, control or self.control, fields or self.fields, deadline=self.deadline)
        self.assertEqual(read.call_args.kwargs['deadline'], self.deadline)
        return result

    def test_normal_gate_calls_concrete_preflight_with_original_deadline(self):
        with mock.patch.object(flow.ship_compat, 'preflight', return_value=self.preflight) as preflight:
            result = flow.gate(self.ctx, self.control, 'promote', '', deadline=self.deadline)
        self.assertEqual(result, self.gate)
        self.assertEqual(preflight.call_args.args, (self.ctx.repo_dir, self.control['sha']))
        self.assertEqual(preflight.call_args.kwargs['deadline'], self.deadline)
        self.assertFalse(preflight.call_args.kwargs['scratch_dir'].exists())

    def test_compat_refusal_maps_to_fixed_public_code(self):
        with mock.patch.object(flow.ship_compat, 'preflight',
                side_effect=flow.compat_evidence.CompatRefusal('compat-missing', 'PRIVATE')):
            with self.assertRaisesRegex(c.ReleaseError, '^candidate-invalid:compat-missing$'):
                flow.gate(self.ctx, self.control, 'promote', '', deadline=self.deadline)

    def test_irreversible_refusal_keeps_specific_reason_without_private_detail(self):
        for mode in ('promote', 'promote-without-staging'):
            with self.subTest(mode=mode), mock.patch.object(flow.ship_compat, 'preflight',
                    side_effect=flow.compat_evidence.CompatRefusal(
                        'irreversible-needs-fix-forward-mode', 'PRIVATE token/path')):
                with self.assertRaises(c.ReleaseError) as caught:
                    flow.gate(self.ctx, self.control, mode, '', deadline=self.deadline)
                self.assertEqual(caught.exception.code, 'candidate-invalid:irreversible-needs-fix-forward-mode')
                self.assertNotIn('PRIVATE', str(caught.exception))
        with mock.patch.object(flow.ship_compat, 'preflight',
                side_effect=flow.compat_evidence.CompatRefusal('PRIVATE', 'PRIVATE')):
            with self.assertRaisesRegex(c.ReleaseError, '^candidate-invalid:compat-stale$'):
                flow.gate(self.ctx, self.control, 'promote', '', deadline=self.deadline)

    def bypass_preflight(self, *, dormant=False, migration=False, registry=False, rollback=False):
        # The source projection is produced by the real resolver/classifier over
        # controlled captures. Only the top-level network gate is mocked below.
        case = fixture.composition.source.ForwardResolverTests()
        case.setUp(); self.addCleanup(case.doCleanups)
        old = case.add()
        case.profile(old['sha'])
        def baseline_revision(sha):
            rows = c.loads_strict(case.world.files[sha, p.STEPS])
            row = next(row for row in rows if row['kind'] == 'baseline-pre-ledger')
            row['rev'] += 1
            case.world.files[sha, p.STEPS] = c.canonical_file_bytes(rows)
        if rollback:
            origin = case.add()
            case.profile(origin['sha'])
            case.add(kind='rollback', target=old)
        sha = case.candidate(self.control['sha'], **({'closure': 'b' * 64, 'body': 2} if migration else {}))
        if dormant:
            case.world.files[sha, p.SEEDS] += b'\n'
        if registry:
            baseline_revision(sha)
        resolution = case.forward(sha)
        history = fixture.composition.collected(resolution)
        return flow.ship_compat.NormalPreflight(resolution,
            fixture.composition.bridge.qualify(resolution, history), history, 1, 1)

    def bypass_gate(self, preflight):
        with mock.patch.object(flow.ship_compat, 'preflight', return_value=preflight) as call:
            value = flow.gate(self.ctx, self.control, 'promote-without-staging', '', deadline=self.deadline)
        call.assert_called_once()
        self.assertEqual(call.call_args.kwargs['deadline'], self.deadline)
        return value

    def test_staging_bypass_still_requires_successful_normal_compat(self):
        value = self.bypass_preflight()
        self.assertEqual(self.bypass_gate(value).normal, value)
        with mock.patch.object(flow.ship_compat, 'preflight',
                side_effect=flow.compat_evidence.CompatRefusal('compat-missing', 'not an outage waiver')):
            with self.assertRaisesRegex(c.ReleaseError, '^candidate-invalid:compat-missing$'):
                flow.gate(self.ctx, self.control, 'promote-without-staging', '', deadline=self.deadline)

    def test_staging_bypass_refuses_measured_dormant_data(self):
        value = self.bypass_preflight(dormant=True)
        self.assertEqual((value.resolution.classification.kind, value.resolution.classification.dormant), ('data', True))
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:staging-bypass$'):
            self.bypass_gate(value)
        with mock.patch.object(flow.ship_compat, 'preflight', return_value=value):
            self.assertEqual(flow.gate(self.ctx, self.control, 'promote', '', deadline=self.deadline).normal, value)

    def test_staging_bypass_refuses_migration_change_even_when_class_none(self):
        value = self.bypass_preflight(migration=True)
        self.assertEqual(value.resolution.classification.kind, 'none')
        self.assertTrue(value.resolution.classification.migration_changes)
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:staging-bypass$'):
            self.bypass_gate(value)

    def test_staging_bypass_refuses_bootstrap_only_registry_metadata_change(self):
        value = self.bypass_preflight(registry=True)
        self.assertEqual(value.resolution.classification.kind, 'none')
        self.assertFalse(value.resolution.classification.migration_changes)
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:staging-bypass-artifacts$'):
            self.bypass_gate(value)

    def test_staging_bypass_refuses_divergent_sources_even_with_equal_artifacts(self):
        value = self.bypass_preflight(rollback=True)
        resolution = value.resolution
        origin = flow._complete_source(resolution, resolution.database_base_source)
        latest = flow._complete_source(resolution, resolution.resolved.latest.record_sha)
        self.assertEqual(resolution.candidate.artifacts, origin.artifacts)
        self.assertEqual(resolution.candidate.artifacts, latest.artifacts)
        self.assertNotEqual(resolution.database_base_source, resolution.resolved.latest.record_sha)
        self.assertFalse(resolution.classification.migration_changes)
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:staging-bypass-source-diverged$'):
            self.bypass_gate(value)

    def test_staging_bypass_missing_legacy_artifact_facts_are_not_equality(self):
        resolution = fixture.WriterTests.legacy
        history = fixture.WriterTests.legacy_history
        value = flow.ship_compat.NormalPreflight(resolution,
            fixture.composition.bridge.qualify(resolution, history), history, 1, 1)
        self.assertEqual(resolution.classification.kind, 'none')
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:staging-bypass-source-unavailable$'):
            self.bypass_gate(value)

    def test_summary_reports_exact_known_values_and_artifact_paths(self):
        value = self.bypass_preflight(dormant=True)
        lines = flow.details(flow.Gate(value.resolution.resolved, value, None))
        self.assertIn('- Database change: data; dormant: true', lines)
        self.assertIn('- Reversible: true; old_server_ok: true', lines)
        self.assertIn(f'- Compatibility: ok; run: {value.qualified.run_id}', lines)
        self.assertIn('- Catalog: version 0, minimum reader 0', lines)
        self.assertIn('- Changed database artifact paths: ' + p.SEEDS, lines)
        self.assertIn('- Other changed source paths: unavailable in the retained preflight projection', lines)

    def test_rollback_summaries_do_not_invent_legacy_catalog_or_new_compat(self):
        legacy = fixture.WriterTests.legacy.resolved
        lines = flow.details(flow.Gate(legacy, None, None))
        self.assertIn('- Reversible: unproven; old_server_ok: unproven', lines)
        self.assertIn('- Catalog versions and changed artifact paths: unavailable (legacy source guard only)', lines)
        selected = replace(self.current, selected=flow.record_resolver._selection(self.current.chain, 1))
        decision = p.rollback_plan(selected.chain, selected.selected.release)
        rollback = flow.record_resolver.RollbackResolution(selected, None, None, decision)
        lines = flow.details(flow.Gate(selected, None, rollback))
        self.assertIn('- Reversible: true (image-only decision); old_server_ok: unproven', lines)
        self.assertIn(f'- Target reader version: {decision.target.reader.reader_version}', lines)
        self.assertIn(f'- Retained catalog: version {decision.live.catalog.catalog_version}, minimum reader {decision.live.catalog.min_reader_version}', lines)
        self.assertIn('- Compatibility run: none (image-only rollback; no new experiment claimed)', lines)

    def test_expired_deadline_does_not_construct_adapter(self):
        with mock.patch.object(flow.ship_records, 'read') as read:
            with self.assertRaisesRegex(c.ReleaseError, '^record-chain-invalid:source-deadline$'):
                flow.read(self.ctx, deadline=time.monotonic() - 1)
        read.assert_not_called()

    def test_original_output_preflight_roundtrips_complete_handoff(self):
        self.assertEqual(set(self.encoded), set(flow.record_handoff.OUTPUT_KEYS))
        self.assertEqual(self.handoff.resolution, self.forward)
        self.assertEqual(self.handoff.collected, self.history)
        self.assertTrue(all(len(value) <= 8192 for value in self.encoded.values()))

    def test_oversized_handoff_refuses_at_preparation(self):
        with mock.patch.object(flow.record_handoff, 'encode',
                side_effect=flow.record_handoff.HandoffRefusal('record-handoff-invalid')):
            with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:handoff$'):
                flow.promotion_outputs(self.gate, self.control,
                    dict(fixture.images(self.forward).images), staging='passed')

    def test_new_promotion_uses_original_capture_after_main_advanced(self):
        raw, state = self.record()
        self.assertEqual((raw, state), (self.raw, 'absent'))
        self.assertEqual(c.loads_strict(raw)['schema_version'], 2)
        self.assertNotEqual(self.current.inventory.main_sha, self.fields['sha'])

    def test_record_only_rerun_preserves_first_production_attempt(self):
        self.assertEqual(self.record(control={**self.control, 'run_attempt': 3}), (self.raw, 'absent'))
        for changed in ({'run_id': '39100000002'}, {'workflow_sha': 'c' * 40}):
            with self.subTest(changed=changed), self.assertRaisesRegex(c.ReleaseError, '^record-invalid:handoff$'):
                self.record(control={**self.control, **changed})

    def test_missing_or_tampered_needs_outputs_cannot_create_new_record(self):
        self.ctx.env['REC_HANDOFF_01'] = ''
        with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:handoff$'):
            self.record()

    def test_exact_published_retry_does_not_decode_old_handoff(self):
        current = fixture.appended(self.forward.resolved, self.raw, self.forward.candidate)
        self.ctx.env.clear()
        with mock.patch.object(flow.record_handoff, 'decode', side_effect=AssertionError('expired evidence')):
            self.assertEqual(self.record(current=current), (self.raw, 'published'))

    def test_exact_published_retry_rejects_changed_production_metadata(self):
        current = fixture.appended(self.forward.resolved, self.raw, self.forward.candidate)
        with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:published-differs$'):
            self.record(current=current, fields={**self.fields, 'environment_values_sha256': 'a' * 64})

    def test_same_bytes_at_nonfinal_record_do_not_hide_newer_history(self):
        current = fixture.appended(self.forward.resolved, self.raw, self.forward.candidate)
        restored = w.build_image_only(current, replace(self.publication, built_at=None,
                                     deployed_at='2026-10-11T12:01:00Z'))
        later = fixture.appended(current, restored)
        with self.assertRaisesRegex(c.ReleaseError, '^latest-changed-since-production$'):
            self.record(current=later)

    def test_changed_production_digests_cannot_rewrite_qualified_handoff(self):
        fields = {**self.fields, 'images': dict(self.fields['images'])}
        fields['images']['web'] = 'sha256:' + 'f' * 64
        with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:production-differs$'):
            self.record(fields=fields)

    def test_image_only_rollback_uses_exact_target_and_retains_live_catalog(self):
        current = self.current
        selected = replace(current, selected=flow.record_resolver._selection(current.chain, 1))
        decision = p.rollback_plan(selected.chain, selected.selected.release)
        rollback = flow.record_resolver.RollbackResolution(selected, None, None, decision)
        publication = replace(self.publication, built_at=None)
        raw = w.build_image_only(selected, publication)
        self.ctx.env = {'SHIP_TARGET_RELEASE': selected.selected.release}
        with mock.patch.object(flow.ship_records, 'rollback', return_value=rollback) as reread:
            actual, state = self.record(fields_from(raw))
        self.assertEqual((actual, state), (raw, 'absent'))
        self.assertEqual(reread.call_args.kwargs['deadline'], self.deadline)
        self.assertEqual(c.loads_strict(actual)['catalog_sha256'], current.latest.live.catalog.catalog_sha256)

    def test_legacy_restore_remains_schema1_without_invented_live_contract(self):
        current = fixture.WriterTests.legacy.resolved
        docs = [c.loads_strict(item.raw) for item in current.chain.inputs]
        legacy = r.Chain((r.bootstrap_entry(docs[0]),
            *(r.manifest_entry(doc, item.raw) for doc, item in zip(docs[1:], current.chain.inputs[1:]))))
        rollback = flow.record_resolver.RollbackResolution(current, legacy,
            flow.record_resolver.schema_change.Classification('none', False, (), ()), None)
        fields = {**self.fields, 'kind': 'restore', 'release': 'prod-20261011T120000Z-' + current.latest.record_sha[:8],
            'sha': current.latest.record_sha, 'images': dict(current.latest.images),
            'previous': current.latest.release, 'rolled_back_from': None, 'built_at': None}
        self.ctx.env['SHIP_TARGET_RELEASE'] = current.latest.release
        with mock.patch.object(flow.ship_records, 'rollback', return_value=rollback):
            raw, state = self.record(fields, current)
        self.assertEqual(state, 'absent')
        self.assertEqual(c.loads_strict(raw)['schema_version'], 1)

    def test_rollback_case_preserves_drift_and_reconcile_rules(self):
        resolved = replace(self.current, selected=flow.record_resolver._selection(self.current.chain, 1))
        # This fixture's latest is itself a rollback to the target, so equality
        # must reconcile rather than perform another PUT.
        self.assertEqual(flow.rollback_case(dict(resolved.selected.images), resolved), 'reconcile')
        with self.assertRaisesRegex(c.ReleaseError, '^drift$'):
            flow.rollback_case(dict(fixture.images(self.forward).images), resolved)
        latest = replace(resolved, selected=resolved.latest)
        with self.assertRaisesRegex(c.ReleaseError, '^nothing-to-roll-back$'):
            flow.rollback_case(dict(latest.latest.images), latest)
        self.assertEqual(flow.rollback_case(dict(fixture.images(self.forward).images), latest), 'restore')

    def test_verify_uses_fresh_full_chain_and_original_budget(self):
        current = fixture.appended(self.forward.resolved, self.raw, self.forward.candidate)
        with mock.patch.object(flow.ship_records, 'read', return_value=current) as reader:
            flow.verify_published(self.ctx, self.raw, deadline=self.deadline)
        self.assertEqual(reader.call_args.kwargs['deadline'], self.deadline)

    def test_helpers_do_not_accept_supplied_resolutions(self):
        with self.assertRaises(TypeError):
            flow.record_bytes(self.ctx, self.control, self.fields, deadline=self.deadline, current=self.current)


if __name__ == '__main__':
    unittest.main()
