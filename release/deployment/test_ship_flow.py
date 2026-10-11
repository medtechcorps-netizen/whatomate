"""Release integration over synthetic adapter results; no live authority.

Concrete source/Actions captures are covered in test_ship_compat/ship_records.
These cases exercise the same-run handoff, writer, retries and drift semantics.
"""
from dataclasses import replace
from datetime import timedelta
import io
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
            fixture.composition.bridge.qualify(cls.forward, cls.history), cls.history, 1, 1, fixture.drills(cls.forward))
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
            fixture.composition.bridge.qualify(resolution, history), history, 1, 1, fixture.drills(resolution))

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
            fixture.composition.bridge.qualify(resolution, history), history, 1, 1, fixture.drills(resolution))
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
        self.assertTrue(any(line.startswith('- Changed source range (') for line in lines))
        self.assertIn('  - M ' + p.SEEDS, lines)

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

    def test_current_outputs_require_observation_but_historical_summary_is_explicit(self):
        historical = replace(self.preflight, drills=None)
        value = flow.Gate(self.forward.resolved, historical, None)
        self.assertIn('- Restore drills: unobserved (historical custody)', flow.details(value))
        with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:production-handoff$'):
            flow.promotion_outputs(value, self.control, dict(fixture.images(self.forward).images), staging='passed')

    def test_summary_uses_preserved_utc_and_never_claims_completed_exempt_drills(self):
        lines = flow.details(self.gate)
        drill_lines = [line for line in lines if line.startswith('- Restore drills:')]
        stamp = self.preflight.drills.observed_at.isoformat().replace('+00:00', 'Z')
        self.assertEqual(drill_lines, [f'- Restore drills: not required for none or dormant changes; observed at {stamp}'])
        changed = replace(self.preflight.drills, required=True)
        invalid = flow.Gate(self.forward.resolved, replace(self.preflight, drills=changed), None)
        with self.assertRaisesRegex(c.ReleaseError, '^schema-change-blocked:restore-drills-observation-invalid$'):
            flow.details(invalid)

    def test_original_output_preflight_roundtrips_complete_handoff(self):
        self.assertEqual(set(self.encoded), set(flow.record_handoff.OUTPUT_KEYS))
        self.assertEqual(self.encoded['handoff_version'], '6')
        self.assertEqual(self.handoff.resolution, self.forward)
        self.assertIs(type(self.handoff.resolution.pending), flow.record_resolver.pending_source.PendingMaterial)
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
        with mock.patch.object(flow.record_handoff, 'decode', side_effect=AssertionError('expired evidence')), \
             mock.patch.object(flow.record_handoff, 'build_record', side_effect=AssertionError('expired evidence')):
            self.assertEqual(self.record(current=current), (self.raw, 'published'))

    def test_historical_v4_record_only_retry_uses_closed_original_context2_custody(self):
        from test_record_handoff import xz_envelope
        codec = flow.record_handoff
        old = fixture.historical_handoff(self.forward, observation=self.preflight.drills,
                                         workflow_execution=flow._execution(self.control))
        budget = codec._Budget(version=4)
        node = codec._value(old, w.PromotionHandoff, budget, 0, encoding=True)
        outputs = xz_envelope(codec._frame(node, budget.blobs, version=4), version='4')
        self.ctx.env.update(('REC_' + key.upper(), value) for key, value in outputs.items())
        self.assertEqual(self.record(control={**self.control, 'run_attempt': 4}), (self.raw, 'absent'))
        with self.assertRaises(w.WriterRefusal):
            w.build_promotion(old, self.publication, self.current)
        with self.assertRaises(codec.HandoffRefusal): codec.encode(old)
        self.ctx.env['REC_HANDOFF_VERSION'] = '5'
        with self.assertRaisesRegex(c.ReleaseError, '^record-invalid:handoff$'): self.record()

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


    def test_current_none_custody_and_summary_never_invent_owner_observation(self):
        self.assertEqual(self.encoded['handoff_version'], '6')
        self.assertIsNone(self.handoff.contract_observation)
        self.assertIn('- Contract owner label: not required for this change', flow.details(self.gate))
        invalid = replace(self.preflight, contract_observation=fixture.contract_observation(self.forward))
        bad_gate = replace(self.gate, normal=invalid)
        with self.assertRaises(c.ReleaseError): flow.details(bad_gate)
        with self.assertRaises(c.ReleaseError):
            flow.promotion_outputs(bad_gate, self.control, dict(fixture.images(self.forward).images), staging='passed')

    def test_contract_summary_uses_original_policy_receipt_and_bounded_ids_only(self):
        # Pure summary of synthetic facts, not an active qualifying preflight.
        resolution = replace(self.forward, classification=self.forward.classification._replace(kind='contract', dormant=False))
        observation = fixture.contract_observation(resolution)
        normal = replace(self.preflight, resolution=resolution, contract_observation=observation)
        lines = flow._contract_details(normal)
        stamp = observation.observed_at.isoformat().replace('+00:00', 'Z')
        self.assertEqual(lines, [f'- Contract owner label: observed at {stamp}; PR 30; '
            'event EVENT_contract (original observation; no DDL authority)'])
        self.assertNotIn(observation.owner_login, lines[0])
        bad = replace(normal, contract_observation=replace(observation, enumeration='unknown'))
        with self.assertRaises(c.ReleaseError): flow._contract_details(bad)

    def test_v5_record_retry_retains_pending_drills_and_absent_owner_without_fetch(self):
        codec = flow.record_handoff
        budget = codec._Budget(version=5)
        node = codec._value(self.handoff, w.PromotionHandoff, budget, 0, encoding=True)
        raw = codec._frame(node, budget.blobs, version=5)
        import test_record_handoff as codec_cases
        output = codec_cases.xz_envelope(raw, version='5')
        self.ctx.env.update({'REC_' + key.upper(): text for key, text in output.items()})
        with mock.patch.object(flow.ship_compat, 'preflight', side_effect=AssertionError('no new admission')):
            self.assertEqual(self.record(), (self.raw, 'absent'))


class ForwardSummaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fixture.composition.source.ForwardAuditTests.setUpClass()

    def setUp(self):
        self.case = fixture.composition.source.ForwardAuditTests()
        self.case.setUp(); self.addCleanup(self.case.doCleanups)

    def lines(self, resolution):
        # Real resolver-derived source audit; qualification is already covered
        # by the normal gate composition. No fabricated authority is exercised.
        normal = SimpleNamespace(resolution=resolution, drills=None, contract_observation=None,
            qualified=SimpleNamespace(reversible=True, old_server=True, run_id=38100000001))
        return flow.details(flow.Gate(resolution.resolved, normal, None))

    def ranges(self, lines):
        return [line for line in lines if line.startswith('- Changed source range (')]

    def test_three_distinct_sources_keep_all_roles_headers_and_full_rows(self):
        case = self.case
        old = case.add(version=1, images=case.boot['images'])
        origin = case.add(); case.profile(origin['sha']); case.add(kind='rollback', target=old)
        case.world.files[case.boot['sha'], case.RECIPE] = b'FROM scratch\n'
        sha = case.candidate()
        case.world.files[sha, '.github/workflows/audit.yml'] = b'candidate'
        resolution = case.forward(sha); lines = self.lines(resolution)
        self.assertEqual(len(self.ranges(lines)), 3)
        self.assertEqual({item.roles for item in resolution.audit.changes},
            {('database-origin',), ('latest-record',), ('selected-web',)})
        for item in resolution.audit.changes:
            header = f'- Changed source range ({", ".join(item.roles)}): {item.base_sha} -> {sha}; paths: {len(item.rows)}'
            at = lines.index(header)
            self.assertEqual(lines[at+1:at+1+len(item.rows)],
                [f'  - {row.status} {row.path}' for row in item.rows])
        self.assertEqual(sum(line == '  - A .github/workflows/audit.yml' for line in lines), 3)
        self.assertFalse(any('unavailable' in line for line in self.ranges(lines)))

    def test_equal_bases_deduplicate_headers_without_losing_add_modify_delete_or_hidden_path(self):
        base, sha = self.case.ordinary()
        self.case.world.files[base, 'docs/removed.md'] = b'before'
        self.case.world.files[base, 'docs/changed.md'] = b'before'
        self.case.world.files[sha, 'docs/changed.md'] = b'after'
        self.case.world.files[sha, '.github/workflows/added.yml'] = b'after'
        lines = self.lines(self.case.forward(sha))
        self.assertEqual(self.ranges(lines), [f'- Changed source range (database-origin, latest-record, selected-web): '
            f'{base} -> {sha}; paths: 3'])
        self.assertEqual([line for line in lines if line.startswith('  - ')],
            ['  - A .github/workflows/added.yml', '  - M docs/changed.md', '  - D docs/removed.md'])

    def test_empty_ranges_are_explicit_and_historical_absence_is_not_empty(self):
        old = self.case.add(version=1); self.case.profile(old['sha'])
        origin = self.case.add(); self.case.profile(origin['sha'])
        self.case.add(kind='rollback', target=old)
        resolution = self.case.forward(origin['sha']); lines = self.lines(resolution)
        self.assertEqual(len(self.ranges(lines)), 2)
        self.assertTrue(all(line.endswith('; paths: 0') for line in self.ranges(lines)))
        self.assertFalse(any(line.startswith('  - ') for line in lines))
        historical = self.lines(replace(resolution, audit=None))
        self.assertFalse(self.ranges(historical))
        self.assertIn('- Other changed source paths: unavailable (historical source audit absent)', historical)

    def test_projection_type_partition_source_status_order_and_bounds_refuse(self):
        _, sha = self.case.ordinary(); resolution = self.case.forward(sha)
        audit = resolution.audit; change = audit.changes[0]; rr = flow.record_resolver
        invalid = [object(), replace(audit, candidate_sha='f'*40), replace(audit, changes=()),
            replace(audit, changes=(replace(change, roles=('database-origin',)),)),
            replace(audit, changes=(replace(change, head_sha='f'*40),)),
            replace(audit, changes=(replace(change, rows=(rr.ChangedPath('R','docs/a'),)),)),
            replace(audit, changes=(replace(change, rows=(rr.ChangedPath('A','../escape'),)),)),
            replace(audit, changes=(replace(change, rows=(rr.ChangedPath('A','docs/a'),)*2),)),
            replace(audit, changes=(replace(change, rows=(rr.ChangedPath('A','docs/z'),rr.ChangedPath('A','docs/a'))),)),
            replace(audit, changes=(replace(change, rows=(rr.ChangedPath('A','docs/a'),)*10001),))]
        for value in invalid:
            with self.subTest(value=type(value).__name__), self.assertRaisesRegex(c.ReleaseError,'^record-chain-invalid:policy$'):
                self.lines(replace(resolution, audit=value))
        with self.assertRaisesRegex(c.ReleaseError,'^record-chain-invalid:policy$'):
            flow._forward_paths(SimpleNamespace(audit=None))

    def test_public_text_rules_refuse_credential_path_and_output_private_match(self):
        _, sha = self.case.ordinary()
        self.case.world.files[sha, 'docs/ghp_synthetic_marker.txt'] = b'public fixture'
        resolution = self.case.forward(sha)
        with self.assertRaisesRegex(c.ReleaseError, '^output-unsafe:text$'):
            self.lines(resolution)
        change = resolution.audit.changes[0]; rr = flow.record_resolver
        safe = replace(resolution, audit=replace(resolution.audit,
            changes=(replace(change, rows=(rr.ChangedPath('A','docs/synthetic_private.txt'),)),)))
        lines = self.lines(safe)
        out = c.Output(stdout=io.StringIO()); out.add_private('synthetic_private')
        with self.assertRaisesRegex(c.ReleaseError, '^output-unsafe:text$'):
            out.summary(lines)

    def test_near_cap_projection_emits_every_path_within_summary_bound(self):
        _, sha = self.case.ordinary()
        # Controlled Git responses still pass the real resolver's projection
        # validation. The canonical audit is close to its512KiB bound.
        rows = [('A', f'docs/p{i:04d}-' + 'x'*485) for i in range(1000)]
        raw = b''.join((status+'\0'+path+'\0').encode() for status,path in rows)
        self.case.audit_output(raw)
        resolution = self.case.forward(sha); lines = self.lines(resolution)
        rendered = [line for line in lines if line.startswith('  - ')]
        self.assertEqual(rendered, [f'  - {status} {path}' for status,path in rows])
        self.assertGreater(len('\n'.join(rendered)), 480*1024)
        self.assertLess(len('\n'.join(lines))+1, flow.record_resolver.MAX_AUDIT_BYTES+2048)


if __name__ == '__main__':
    unittest.main()
