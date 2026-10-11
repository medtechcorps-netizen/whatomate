"""Pure writer composition using concrete adapters with synthetic captures.

No actual signature authority, Git process, provider or network is exercised.
"""
from dataclasses import FrozenInstanceError, replace
from datetime import timedelta
import hashlib
from pathlib import Path
import unittest
from unittest import mock

import record_writer as writer
import test_compat_request as composition
import test_ship_record_policy as policy_cases

policy, records, common = writer.policy, writer.records, writer.common
resolver, transport = writer.resolver, writer.transport


def execution(**changes):
    return replace(writer.WorkflowExecution(common.REPOSITORY, records.SHIP_SIGNER,
                   'd' * 40, 39100000001, 1), **changes)


def publication(*, built=True, **changes):
    return replace(writer.PublicationFacts(execution(), '2026-10-11T12:00:00Z',
        '2026-10-11T11:30:00Z' if built else None, 'e' * 64, 'f' * 64), **changes)


def images(value):
    sha = value.candidate.sha
    return writer.CandidateImages(sha, tuple((part, 'sha256:' + hashlib.sha256((sha + part).encode()).hexdigest())
        for part in common.COMPONENTS), tuple((part, sha) for part in common.COMPONENTS))


def drills(value, *, now=composition.actions.NOW):
    # Actual pure observer over real resolver-derived synthetic source bytes.
    return writer.forward_gates.observe_forward_drills(value, now=now)


def projected(chain, *, selected=None, main='b' * 40):
    """Synthetic current view; never represented as authenticated outside tests."""
    bootstrap = common.loads_strict(chain.inputs[0].raw)
    inventory = transport.InventorySnapshot(transport.REPOSITORY_ID, main,
        tuple(transport.ReleaseIdentity(row.release, 100 + i, 200 + i, len(row.record.raw),
              'sha256:' + row.record.manifest_sha256)
              for i, row in enumerate(sorted(chain.entries[1:], key=lambda row: row.release))),
        tuple(transport.TagIdentity(row.release, row.sha) for row in sorted(chain.entries[1:], key=lambda row: row.release)))
    bindings = tuple(transport.VerifiedImages(row.release, row.sha, row.images,
        tuple((part, bootstrap['legacy_signer']['source_commit'] if dict(row.images)[part] == bootstrap['images'][part]
               else row.sha) for part in common.COMPONENTS)) for row in chain.entries)
    latest = resolver._selection(chain, len(chain.entries) - 1)
    return resolver.ResolvedChain(chain, inventory, bindings, latest,
        latest if selected is None else resolver._selection(chain, selected))


def appended(value, raw, candidate=None):
    document = common.loads_strict(raw)
    chain = value.chain
    sources = chain.sources
    if candidate is not None and candidate.sha not in {row.sha for row in sources}:
        sources += (candidate,)
    ancestors = chain.ancestors
    pair = (chain.entries[-1].sha, document['sha'])
    if document['kind'] == 'promote' and pair[0] != pair[1] and pair not in ancestors:
        ancestors += (pair,)
    bootstrap = common.loads_strict(chain.inputs[0].raw)
    web = bootstrap['sha'] if document['images']['web'] == bootstrap['images']['web'] else document['sha']
    item = policy.RecordInput(raw, common.sha256_bytes(raw), web)
    return projected(policy.normalize_chain(chain.inputs + (item,), sources, ancestors))


class WriterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        composition.source.ForwardResolverTests.setUpClass()
        cls.forward = composition.resolution()
        cls.legacy = composition.resolution(legacy=True)
        cls.dormant = composition.resolution(dormant=True)
        cls.history = composition.collected(cls.forward)
        cls.legacy_history = composition.collected(cls.legacy)
        cls.dormant_history = composition.collected(cls.dormant)

    def prepare(self, value=None, history=None, **options):
        value = self.forward if value is None else value
        return writer.prepare_promotion(value, self.history if history is None else history,
            options.pop('candidate_images', images(value)), options.pop('execution', execution()),
            staging=options.pop('staging', 'passed'), drills=options.pop('drills', drills(self.forward)), **options)

    def refusal(self, call, *args, code=None, **kwargs):
        with self.assertRaises(writer.WriterRefusal) as caught:
            call(*args, **kwargs)
        self.assertNotIn('PRIVATE', str(caught.exception))
        self.assertIsNone(caught.exception.__cause__)
        if code is not None:
            self.assertEqual(str(caught.exception), code)

    def test_observation_survives_later_publication_and_main_without_retry_clock(self):
        original = drills(self.forward)
        plan = self.prepare(drills=original)
        later = replace(publication(), deployed_at='2027-05-01T00:00:00Z')
        fresh = replace(self.forward.resolved, inventory=replace(self.forward.resolved.inventory, main_sha='f' * 40))
        raw = writer.build_promotion(plan, later, fresh)
        self.assertEqual(plan.drills, original)
        self.assertEqual(common.loads_strict(raw)['deployed_at'], later.deployed_at)

    def test_original_observation_precedes_no_collected_evidence_and_cannot_invent_dates(self):
        original = drills(self.forward)
        for changed in (replace(original, observed_at=self.history.history.observed_at - timedelta(microseconds=1)),
                        replace(original, required=True), replace(original, staging_restore=original.observed_at),
                        replace(original, source_sha256='f' * 64)):
            self.refusal(self.prepare, drills=changed)
        historical = self.prepare(drills=None)
        self.assertIsNone(historical.drills)
        self.assertTrue(writer.build_promotion(historical, publication(), self.forward.resolved))

    def test_current_active_class_hold_is_not_lifted_by_a_synthetic_valid_drill(self):
        from datetime import datetime, timezone
        change = self.forward.classification._replace(kind='expand', dormant=False)
        data = b'{"version":1,"staging_restore":"2026-10-11T00:00:00Z","production_fork":"2026-10-11T00:00:00Z"}'
        value = replace(self.forward, classification=change,
            audit=replace(self.forward.audit, drills=replace(self.forward.audit.drills, raw=data)))
        observation = drills(value, now=datetime(2026, 10, 11, 12, tzinfo=timezone.utc))
        self.assertTrue(observation.required)
        self.refusal(writer.prepare_promotion, value, self.history, images(value), execution(),
                     staging='passed', drills=observation)

    def test_first_v2_promotion_establishes_measured_contract_from_real_composition(self):
        plan = self.prepare(self.legacy, self.legacy_history)
        self.assertIsNone(plan.resolution.resolved.latest.live)
        raw = writer.build_promotion(plan, publication(), self.legacy.resolved)
        doc = common.loads_strict(raw)
        self.assertEqual(set(doc), records.MANIFEST_V2_KEYS)
        self.assertEqual(doc['schema_version'], 2)
        self.assertEqual(doc['sha'], self.legacy.candidate.sha)
        self.assertEqual(doc['catalog_sha256'], policy.V0_MEASUREMENT)
        self.assertNotEqual(doc['catalog_sha256'], dict(policy.V0_PINS)[policy.GOLDEN])
        self.assertEqual(doc['data_steps_sha256'], self.legacy.candidate.catalog.data_steps_sha256)
        self.assertEqual(doc['seeds_sha256'], self.legacy.candidate.catalog.seeds_sha256)
        self.assertEqual(doc['compat_run_id'], self.legacy_history.history.attempts[-1].run_id)
        self.assertEqual((doc['compat'], doc['old_server_ok'], doc['reversible'], doc['fixed_forward_from']),
                         ('ok', True, True, None))
        current = appended(self.legacy.resolved, raw, self.legacy.candidate)
        self.assertEqual(current.latest.live.origin_release, doc['release'])
        self.assertEqual(current.latest.live.origin_source, self.legacy.candidate)
        self.assertEqual(len(current.chain.entries), len(self.legacy.resolved.chain.entries) + 1)

    def test_promotion_after_rollback_preserves_three_distinct_source_identities(self):
        plan = self.prepare()
        latest = self.forward.resolved.latest
        self.assertEqual(len({latest.record_sha, latest.actual_web_source_sha,
                              latest.live.origin_source.sha}), 3)
        doc = common.loads_strict(writer.build_promotion(plan, publication(), self.forward.resolved))
        self.assertEqual(doc['previous'], latest.release)
        self.assertEqual(doc['sha'], self.forward.candidate.sha)
        self.assertEqual(doc['images'], dict(images(self.forward).images))
        self.assertEqual(doc['signer']['workflow_sha'], execution().workflow_sha)

    def test_main_advancement_after_deployment_does_not_requalify_or_rewrite_capture(self):
        plan = self.prepare()
        fresh = replace(self.forward.resolved,
                        inventory=replace(self.forward.resolved.inventory, main_sha='f' * 40))
        self.assertNotEqual(fresh.inventory.main_sha, self.forward.candidate.sha)
        raw = writer.build_promotion(plan, publication(), fresh)
        self.assertEqual(raw, writer.build_promotion(plan, publication(), self.forward.resolved))
        self.assertEqual(plan.resolution.resolved.inventory.main_sha, self.forward.candidate.sha)

    def concrete_pair(self, *, legacy=False, overlap=False):
        case = composition.source.ForwardResolverTests()
        case.setUp()
        self.addCleanup(case.doCleanups)
        sha = composition.fixtures.SHA
        if legacy:
            old = case.add(version=1, sha=sha if overlap else policy.V0_RELEASED_SOURCE)
            case.profile(old['sha'])
            if not overlap:
                for path in (policy.PROFILE, policy.OVERLAY):
                    del case.world.files[old['sha'], path]
        else:
            old = case.add(version=1, sha=sha if overlap else None)
            case.profile(old['sha'])
            origin = case.add()
            case.profile(origin['sha'])
            case.add(kind='rollback', target=old)
        case.candidate(sha)
        forward = case.forward(sha)
        history = composition.collected(forward)
        case.world.main = 'f' * 40
        return case, forward, history

    def fresh_plain(self, case, name):
        config, work = case.root / (name + '-config'), case.root / (name + '-work')
        config.mkdir(); work.mkdir()
        budget = transport.OperationBudget(700, capture=case.world, clock=case.clock)
        client = transport.ReleaseGh(case.gh, composition.source.support.GH_TOKEN,
                                     config, work, budget=budget, base_env={})
        return resolver.resolve_authenticated(client, case.repo, case.git)

    def concrete_publish(self, *, legacy=False, overlap=False):
        case, forward, history = self.concrete_pair(legacy=legacy, overlap=overlap)
        current = self.fresh_plain(case, 'after-deployment')
        self.assertNotEqual(current.inventory.main_sha, forward.candidate.sha)
        self.assertEqual(current.chain.inputs, forward.resolved.chain.inputs)
        if overlap:
            self.assertEqual(current.latest.reader.mode, 'legacy-reader0')
            self.assertEqual(forward.resolved.latest.reader, forward.candidate)
        else:
            self.assertNotIn(forward.candidate.sha, {row.sha for row in current.chain.sources})
        raw = writer.build_promotion(self.prepare(forward, history), publication(), current)
        doc = common.loads_strict(raw)
        case.world.add(doc)
        case.world.fake.attest_ship_images(doc['images'], doc['sha'])
        after = self.fresh_plain(case, 'after-publication')
        self.assertEqual(after.latest.record_sha, forward.candidate.sha)
        self.assertEqual(after.latest.live.origin_source, forward.candidate)
        self.assertEqual(after.chain.inputs[:-1], current.chain.inputs)
        self.assertEqual(writer.published_state(after, raw), 'published')
        self.assertEqual(current.inventory.main_sha, 'f' * 40)

    def test_concrete_plain_mixed_parent_can_append_original_unreleased_candidate(self):
        self.concrete_publish()

    def test_concrete_plain_all_v1_parent_can_establish_first_v2(self):
        self.concrete_publish(legacy=True)

    def test_concrete_plain_v1_candidate_overlap_upgrades_only_original_candidate(self):
        self.concrete_publish(legacy=True, overlap=True)

    def test_concrete_plain_mixed_candidate_overlap_preserves_retained_origin(self):
        self.concrete_publish(overlap=True)

    def test_concrete_plain_advanced_signed_history_still_refuses(self):
        case, forward, history = self.concrete_pair()
        case.add(kind='restore')
        current = self.fresh_plain(case, 'advanced-history')
        self.refusal(writer.build_promotion, self.prepare(forward, history), publication(), current,
                     code='record-writer-parent-changed')

    def test_candidate_enrichment_does_not_ignore_unrelated_source_or_ancestry_changes(self):
        case, forward, history = self.concrete_pair()
        current = self.fresh_plain(case, 'historical-parent')
        plan = self.prepare(forward, history)
        old_sha = case.documents[1]['sha']
        upgraded = policy.reviewed_v0_source(old_sha, tuple(policy.SourceBlob(path, case.source_bytes[path])
                                                            for path in policy.SOURCE_PATHS))
        sources = tuple(upgraded if row.sha == old_sha else row for row in current.chain.sources)
        changed_source = policy.normalize_chain(current.chain.inputs, sources, current.chain.ancestors)
        changed_ancestry = policy.normalize_chain(current.chain.inputs, current.chain.sources,
            current.chain.ancestors + (('e' * 40, 'f' * 40),))
        for chain in (changed_source, changed_ancestry):
            self.refusal(writer.build_promotion, plan, publication(), projected(chain),
                         code='record-writer-parent-changed')

    def test_candidate_enrichment_cannot_replace_different_complete_candidate_facts(self):
        case, forward, history = self.concrete_pair(legacy=True, overlap=True)
        current = self.fresh_plain(case, 'overlap-parent')
        changed = policy.reviewed_v0_source(forward.candidate.sha,
            tuple(policy.SourceBlob(path, case.source_bytes[path] + (b'\n' if path == policy.SEEDS else b''))
                  for path in policy.SOURCE_PATHS))
        self.assertNotEqual(changed, forward.candidate)
        sources = tuple(changed if row.sha == changed.sha else row for row in current.chain.sources)
        chain = policy.normalize_chain(current.chain.inputs, sources, current.chain.ancestors)
        self.refusal(writer.build_promotion, self.prepare(forward, history), publication(), projected(chain),
                     code='record-writer-parent-changed')

    def test_dormant_source_change_and_staging_bypass_do_not_invent_compat_bypass(self):
        plan = self.prepare(self.dormant, self.dormant_history, staging='bypassed')
        doc = common.loads_strict(writer.build_promotion(plan, publication(), self.dormant.resolved))
        self.assertEqual((doc['db_change'], doc['dormant'], doc['compat'], doc['staging']),
                         ('data', True, 'ok', 'bypassed'))
        self.assertEqual(doc['seeds_sha256'], self.dormant.candidate.catalog.seeds_sha256)

    def test_new_promotion_requires_actual_qualified_pairs_not_arbitrary_receipt(self):
        qualified = composition.bridge.qualify(self.forward, self.history)
        self.refusal(self.prepare, history=qualified)
        world = composition.actions.World(); world.add(negative=True)
        self.refusal(self.prepare, history=composition.collected(self.forward, world))
        world = composition.actions.World(); world.add(failure=True)
        self.refusal(self.prepare, history=composition.collected(self.forward, world))

    def test_later_unknown_reused_window_and_stale_context_refuse(self):
        world = composition.actions.World(); world.add(); world.add(run=101, number=11, minute=30, failure=True)
        self.refusal(self.prepare, history=composition.collected(self.forward, world))
        first = self.history.history.attempts[0]
        changed = replace(first, execution_window=replace(first.execution_window, fresh=False))
        self.refusal(self.prepare, history=replace(self.history,
            history=replace(self.history.history, attempts=(changed,))))
        for options in ({'old_protocol': True}, {'wrong_catalog': True}):
            self.refusal(self.prepare, history=composition.collected(self.forward, **options))

    def test_candidate_images_require_exact_three_components_and_actual_candidate_sources(self):
        original = images(self.forward)
        changes = [replace(original, sha='f' * 40), replace(original, images=original.images[:-1]),
            replace(original, images=tuple(reversed(original.images))),
            replace(original, images=tuple((name, original.images[0][1]) for name in common.COMPONENTS)),
            replace(original, source_shas=(('web', self.forward.previous_web_source_sha),) + original.source_shas[1:]),
            replace(original, images=(('web', 'PRIVATE'),) + original.images[1:])]
        for value in changes:
            with self.subTest(images=value.sha): self.refusal(self.prepare, candidate_images=value)

    def test_execution_identity_and_closed_input_types_refuse_private_or_foreign_values(self):
        for value in (execution(repository='PRIVATE'), execution(workflow='PRIVATE'),
                      execution(workflow_sha='PRIVATE'), execution(run_id=True),
                      execution(run_attempt=0), execution(run_attempt=True)):
            self.refusal(self.prepare, execution=value)
        self.refusal(self.prepare, staging='PRIVATE')
        with self.assertRaises(FrozenInstanceError): self.prepare().facts = None
        with self.assertRaises(TypeError): writer.PublicationFacts(execution(), '', None, '', '', fixed_forward_from='x')

    def test_changed_handoff_derived_catalog_class_flags_or_run_cannot_override_original(self):
        plan = self.prepare()
        for changed in (plan.facts._replace(db_change='data'), plan.facts._replace(compat_run_id=123),
                        plan.facts._replace(reversible=False), plan.facts._replace(old_server_ok=False),
                        plan.facts._replace(catalog=plan.facts.catalog._replace(catalog_sha256='f' * 64))):
            self.refusal(writer.build_promotion, replace(plan, facts=changed), publication(),
                         self.forward.resolved, code='record-writer-handoff-changed')

    def test_record_job_must_replay_original_production_execution_identity(self):
        plan = self.prepare()
        for changed in (execution(run_id=39100000002), execution(run_attempt=2), execution(workflow_sha='c' * 40)):
            self.refusal(writer.build_promotion, plan, publication(execution=changed),
                         self.forward.resolved, code='record-writer-execution-changed')
        # A record-only retry passes the original successful production identity;
        # authenticating that custody belongs to its caller, not this serializer.
        self.assertTrue(writer.build_promotion(plan, publication(), self.forward.resolved))

    def test_parent_advancement_and_changed_complete_history_refuse_new_append(self):
        plan = self.prepare()
        raw = writer.build_promotion(plan, publication(), self.forward.resolved)
        advanced = appended(self.forward.resolved, raw, self.forward.candidate)
        self.refusal(writer.build_promotion, plan, publication(), advanced, code='record-writer-parent-changed')
        chain = self.forward.resolved.chain
        self.refusal(writer.build_promotion, plan, publication(),
            replace(self.forward.resolved, chain=replace(chain, entries=chain.entries[:-1])))

    def test_catalog_source_origin_classifier_and_workflow_tampering_refuse(self):
        value = self.forward
        altered = [replace(value, previous_web_source_sha=value.database_base_source),
            replace(value, database_base_source=value.previous_web_source_sha),
            replace(value, workflow=value.workflow + b'\n'),
            replace(value, candidate=replace(value.candidate, mode='synthetic-policy')),
            replace(value, classification=value.classification._replace(reasons=('PRIVATE',)))]
        for changed in altered:
            self.refusal(self.prepare, changed)

    def test_noncanonical_publication_and_invalid_timing_refuse_without_echo(self):
        plan = self.prepare()
        for pub in (publication(deployed_at='PRIVATE'), publication(built_at=None),
                    publication(built_at='2026-10-12T00:00:00Z'), publication(environment_values_sha256='PRIVATE'),
                    publication(deployed_at='2026-10-04T12:03:00Z', built_at='2026-10-04T12:00:00Z')):
            self.refusal(writer.build_promotion, plan, pub, self.forward.resolved)

    def test_exact_published_retry_needs_no_handoff_expired_artifact_or_fresh_main(self):
        plan = self.prepare()
        raw = writer.build_promotion(plan, publication(), self.forward.resolved)
        current = appended(self.forward.resolved, raw, self.forward.candidate)
        with mock.patch.object(writer.request, 'qualify', side_effect=AssertionError('must not qualify')):
            self.assertEqual(writer.published_state(current, raw), 'published')
            self.assertEqual(writer.published_state(self.forward.resolved, raw), 'absent')

    def test_same_tag_different_bytes_or_old_nonfinal_exact_record_refuse(self):
        plan = self.prepare(); raw = writer.build_promotion(plan, publication(), self.forward.resolved)
        current = appended(self.forward.resolved, raw, self.forward.candidate)
        doc = common.loads_strict(raw); doc['fingerprints']['environment_values_sha256'] = 'a' * 64
        self.refusal(writer.published_state, current, common.canonical_file_bytes(doc), code='record-writer-published-conflict')
        restore = writer.build_image_only(current, publication(built=False, deployed_at='2026-10-11T12:01:00Z'))
        later = appended(current, restore)
        self.refusal(writer.published_state, later, raw, code='record-writer-published-conflict')
        self.refusal(writer.published_state, current, raw + b' ')

    def test_image_only_rollback_to_actual_legacy_web_keeps_live_origin_and_record_sha(self):
        resolved = self.forward.resolved
        selected = replace(resolved, selected=resolver._selection(resolved.chain, 1))
        raw = writer.build_image_only(selected, publication(built=False), reconciled=True)
        doc = common.loads_strict(raw)
        self.assertEqual((doc['kind'], doc['sha'], doc['reconciled']), ('rollback', selected.selected.record_sha, True))
        self.assertNotEqual(doc['sha'], selected.selected.actual_web_source_sha)
        self.assertEqual(doc['images'], dict(selected.selected.images))
        self.assertEqual(tuple(doc[key] for key in records.DocumentCatalog._fields), tuple(resolved.latest.live.catalog))
        self.assertEqual((doc['db_change'], doc['dormant'], doc['reversible'], doc['compat'],
            doc['compat_run_id'], doc['old_server_ok'], doc['staging']), ('none', False, True, 'bypassed', None, 'unproven', 'bypassed'))
        after = appended(selected, raw)
        self.assertEqual(after.latest.live, resolved.latest.live)
        self.assertEqual(len(after.chain.entries), len(resolved.chain.entries) + 1)

    def test_restore_uses_latest_image_and_cannot_be_reconciled_or_built(self):
        resolved = self.forward.resolved
        doc = common.loads_strict(writer.build_image_only(resolved, publication(built=False)))
        self.assertEqual((doc['kind'], doc['rolled_back_from'], doc['sha']), ('restore', None, resolved.latest.record_sha))
        self.assertEqual(doc['images'], dict(resolved.latest.images))
        self.refusal(writer.build_image_only, resolved, publication(built=False), reconciled=True)
        self.refusal(writer.build_image_only, resolved, publication())

    def test_no_first_v2_image_only_tuple_is_invented_for_all_v1(self):
        self.refusal(writer.build_image_only, self.legacy.resolved, publication(built=False))
        self.assertTrue(writer.prepare_promotion(self.legacy, self.legacy_history,
                        images(self.legacy), execution(), staging='passed'))

    def test_exact_requested_target_span_and_duplicate_images_are_not_collapsed(self):
        scenario = policy_cases.Scenario()
        source = policy_cases.current()
        first = scenario.add(source)
        scenario.add(source, db_change='data', dormant=True)
        latest = scenario.add(source, db_change='data', dormant=True)
        chain = scenario.normalize()
        early = projected(chain, selected=1)
        self.refusal(writer.build_image_only, early, publication(built=False))
        later = projected(chain, selected=2)
        doc = common.loads_strict(writer.build_image_only(later, publication(built=False)))
        self.assertEqual(doc['sha'], first['sha'])
        self.assertEqual(doc['images'], latest['images'])
        self.assertEqual(len(appended(later, common.canonical_file_bytes(doc)).chain.entries), 5)

    def test_irreversible_span_and_unsupported_future_reader_facts_refuse(self):
        scenario = policy_cases.Scenario(); source = policy_cases.current()
        scenario.add(source)
        scenario.add(replace(source, sha='2' * 40), reversible=False)
        self.refusal(writer.build_image_only, projected(scenario.normalize(), selected=1), publication(built=False))
        scenario = policy_cases.Scenario(); scenario.add(source)
        scenario.add(policy_cases.future('2' * 40, minimum=1, kind='contract'), db_change='contract')
        self.refusal(writer.build_image_only, projected(scenario.normalize(), selected=1), publication(built=False))

    def test_image_bindings_inventory_and_runtime_projections_cannot_be_omitted(self):
        resolved = self.forward.resolved
        changes = [replace(resolved, image_bindings=resolved.image_bindings[:-1]),
            replace(resolved, inventory=replace(resolved.inventory, records=resolved.inventory.records[:-1])),
            replace(resolved, inventory=replace(resolved.inventory, tags=resolved.inventory.tags[:-1])),
            replace(resolved, chain=replace(resolved.chain, runtime_source_supported=False)),
            replace(resolved, selected=replace(resolved.selected, index=True))]
        for value in changes:
            self.refusal(writer.build_image_only, value, publication(built=False))

    def test_pure_builders_do_not_read_collect_run_commands_or_modify_legacy_writer(self):
        plan = self.prepare()
        legacy_fields = common.loads_strict(self.legacy.resolved.chain.inputs[-1].raw)
        fields = dict(legacy_fields, workflow_sha=legacy_fields['signer']['workflow_sha'], **legacy_fields['fingerprints'])
        original = records.build_manifest(fields)
        with mock.patch.object(Path, 'open', side_effect=AssertionError('I/O')), \
             mock.patch.object(Path, 'read_bytes', side_effect=AssertionError('I/O')), \
             mock.patch.object(transport.runtime, 'bounded_command', side_effect=AssertionError('command')), \
             mock.patch.object(writer.collect, 'collect_history', side_effect=AssertionError('network')):
            raw = writer.build_promotion(plan, publication(), self.forward.resolved)
            self.assertEqual(writer.published_state(self.forward.resolved, raw), 'absent')
            writer.build_image_only(self.forward.resolved, publication(built=False))
            self.assertEqual(records.build_manifest(fields), original)
        self.assertEqual(common.loads_strict(original)['schema_version'], 1)


if __name__ == '__main__':
    unittest.main()
