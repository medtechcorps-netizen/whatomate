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


def contract_observation(value, *, at=composition.actions.NOW, events=1):
    """Synthetic complete owner facts; never a live collector receipt."""
    a = writer.contract_authorization
    repo = transport.REPOSITORY_ID
    label = a.LabelIdentity('LABEL_contract', repo, a.LABEL_NAME,
        at - timedelta(days=10), at - timedelta(days=2))
    event = a.LabelEvent('EVENT_contract', 'labeled', 'PULL_contract', label,
        'User', 'OWNER_contract', common.APPROVER_LOGIN, at - timedelta(minutes=1))
    labels, rows = [label], [event]
    if events > 1:
        # Deterministic high entropy within the actual policy's name/id bounds.
        for i in range(99):
            token = hashlib.shake_256(('label-' + str(i)).encode()).hexdigest(120)
            labels.append(a.LabelIdentity(token, repo, 'fixture-' + token[:40],
                at - timedelta(days=10), at - timedelta(days=2)))
        for i in range(events - 1):
            token = hashlib.shake_256(('event-' + str(i)).encode()).hexdigest(120)
            actor = hashlib.shake_256(('actor-' + str(i)).encode()).hexdigest(120)
            rows.append(a.LabelEvent(token, 'labeled', 'PULL_contract', labels[1 + i % 99],
                'User', actor, 'actor-' + actor[:30], at - timedelta(seconds=i + 2)))
    pull = a.MergedPull('PULL_contract', 30, repo, common.REPOSITORY, 'main',
        value.candidate.sha, 'b' * 40, True, False, at - timedelta(hours=1), tuple(labels), tuple(rows))
    return a.ContractObservation(repo, common.REPOSITORY, value.candidate.sha,
        'User', 'OWNER_contract', common.APPROVER_LOGIN, (pull,), at, 'complete-visible')


def historical_handoff(value, *, observation=None, workflow_execution=None):
    """Synthetic historical context2 custody, never a current admission."""
    history = composition.collected(value, version=2)
    return writer._prepare_promotion_context2_custody(replace(value, pending=None), history,
        images(value), execution() if workflow_execution is None else workflow_execution,
        staging='passed', drills=observation)


def retained_pending_resolution():
    """Real resolver/parsers over synthetic Git captures and full public v0."""
    source = composition.source
    source.ForwardPendingTests.setUpClass()
    case = source.ForwardPendingTests(); case.setUp()
    try:
        old = case.add()
        case.profile(old['sha']); case.install(old['sha'])
        sha = case.candidate(composition.fixtures.SHA); case.install(sha)
        value = case.forward(sha)
        assert value.pending.candidate_files == value.pending.previous_files
        assert (value.classification.kind, value.classification.dormant, value.classification.reasons) == ('none', False, ())
        assert len(case.replay(value).plan.entries[0].objects) == 4452
        return value
    finally:
        case.doCleanups()


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
        self.refusal(self.prepare, drills=None)
        historical = historical_handoff(self.forward)
        self.assertIsNone(historical.drills)
        self.refusal(writer.build_promotion, historical, publication(), self.forward.resolved)
        self.assertTrue(writer._build_promotion_context2_custody(historical, publication(), self.forward.resolved))

    def test_current_pending_is_mandatory_and_old_context_cannot_admit_new_promotion(self):
        for value in (replace(self.forward, pending=None),
                      replace(self.forward, pending=replace(self.forward.pending, previous_sha='f' * 40))):
            self.refusal(self.prepare, value=value)
        old = historical_handoff(self.forward, observation=drills(self.forward))
        self.refusal(self.prepare, history=old.collected)
        self.refusal(writer._prepare_promotion_context2_custody, self.forward, old.collected,
            images(self.forward), execution(), staging='passed', drills=old.drills)
        # Historical context2 cannot be manufactured from current context3 by
        # dropping the raw pending observation alone.
        self.refusal(writer._prepare_promotion_context2_custody, replace(self.forward, pending=None),
            self.history, images(self.forward), execution(), staging='passed', drills=old.drills)

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
                        images(self.legacy), execution(), staging='passed', drills=drills(self.legacy)))

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


    def test_contract_original_time_policy_and_chronology_are_replayed_without_clock(self):
        value = replace(self.forward, classification=self.forward.classification._replace(kind='contract', dormant=False))
        at = self.history.history.observed_at
        observation = contract_observation(value, at=at)
        original_drills = replace(drills(self.forward), observed_at=at + timedelta(seconds=1))
        with mock.patch.object(transport.runtime, 'bounded_command', side_effect=AssertionError('no command')):
            receipt = writer._contract_custody(value, self.history, original_drills, observation)
        self.assertEqual(receipt.observed_at, at)
        self.assertEqual(receipt.candidate_sha, value.candidate.sha)
        for moment in (at - timedelta(seconds=1), at + timedelta(seconds=2)):
            with self.subTest(moment=moment), self.assertRaises(writer.WriterRefusal):
                writer._contract_custody(value, self.history, original_drills, contract_observation(value, at=moment))
        with self.assertRaises(writer.WriterRefusal):
            writer._contract_custody(value, self.history, None, observation)

    def test_owner_observation_none_rule_and_invalid_owner_remain_refusals(self):
        value = replace(self.forward, classification=self.forward.classification._replace(kind='contract', dormant=False))
        observation = contract_observation(value)
        a = writer.contract_authorization
        for invalid in (None, replace(observation, main_sha='f' * 40),
                        replace(observation, owner_login='other'), replace(observation, enumeration='partial')):
            with self.subTest(invalid=invalid), self.assertRaises(a.ContractRefusal):
                writer._contract_custody(value, self.history, drills(self.forward), invalid)
        with self.assertRaises(a.ContractRefusal):
            writer._contract_custody(self.forward, self.history, drills(self.forward), observation)
        self.assertIsNone(writer._contract_custody(self.forward, self.history, drills(self.forward), None))

    def test_valid_owner_data_does_not_lift_active_contract_compatibility_hold(self):
        value = replace(self.forward, classification=self.forward.classification._replace(kind='contract', dormant=False))
        observation = contract_observation(value)
        writer.forward_gates.validate_preserved_contract(value, observation)
        with self.assertRaises(writer.WriterRefusal):
            writer.prepare_promotion(value, self.history, images(value), execution(), staging='passed',
                drills=drills(self.forward), contract_observation=observation)
        self.assertIsNone(self.prepare().contract_observation)





def rollback_fixture(testcase, *, rows=None, producer_overlap=False):
    """Actual source/Actions/verification/bridge with controlled command edges.

    No project predicate or positive receipt is mocked. The later ordinary
    resolver uses the same captured world with a new bounded source operation.
    """
    import test_ship_rollback_compat as preflight_cases
    preflight_cases.RollbackPreflightTests.setUpClass()
    case = preflight_cases.RollbackPreflightTests()
    case.setUp(); testcase.addCleanup(case.doCleanups)
    if producer_overlap:
        # H is an existing v1 source, so plain reader0 must be enriched exactly.
        case.case.world.main = case.live_image['sha']
        case.producer = case.case.world.main
        # The preliminary setup operation is closed; derive the new exact source
        # with a fresh location, leaving the public preflight to repeat it again.
        case.case.client = None
        config, work = case.case.root / 'overlap-config', case.case.root / 'overlap-work'
        config.mkdir(); work.mkdir()
        budget = transport.OperationBudget(700, capture=case.case.world, clock=case.case.clock)
        client = transport.ReleaseGh(case.case.gh, preflight_cases.source.fixtures.support.GH_TOKEN,
            config, work, budget=budget, base_env={})
        case.expected = resolver.resolve_rollback_proof(client, case.case.repo, case.case.git,
            case.producer, case.target['release'])
        case.world = preflight_cases.collector.World(preflight_cases.requests.producer_binding(case.expected))
        case.world.add()
    if rows is not None:
        case.world = preflight_cases.collector.World(case.world.binding)
        for options in rows:
            case.world.add(**options)
    preflight = case.run_preflight()
    execution_value = execution(workflow_sha=preflight.resolution.producer.sha)
    return case, writer.prepare_rollback(preflight, execution_value)


def rollback_publication(handoff, **changes):
    return publication(execution=handoff.execution, deployed_at='2026-10-13T12:00:00Z',
                       built=False, **changes)


def rollback_plain(case, *, main='f' * 40):
    """Fresh ordinary concrete read; its sources intentionally omit unused H."""
    source = case.case
    source.world.main = main
    config, work = source.root / 'plain-config', source.root / 'plain-work'
    config.mkdir(); work.mkdir()
    budget = transport.OperationBudget(source.clock.now + 600,
        capture=source.world, clock=source.clock)
    import test_ship_rollback_compat as preflight_cases
    client = transport.ReleaseGh(source.gh, preflight_cases.source.fixtures.support.GH_TOKEN,
        config, work, budget=budget, base_env={})
    return resolver.resolve_authenticated(client, source.repo, source.git)


class QualifiedRollbackWriterTests(unittest.TestCase):
    def setUp(self):
        self.case, self.handoff = rollback_fixture(self)

    def current(self):
        return rollback_plain(self.case)

    def build(self, *, handoff=None, current=None, publication_value=None, **options):
        handoff = self.handoff if handoff is None else handoff
        return writer.build_qualified_rollback(handoff,
            rollback_publication(handoff) if publication_value is None else publication_value,
            self.current() if current is None else current, **options)

    def test_real_preflight_and_fresh_plain_parent_allow_main_advance(self):
        current = self.current(); handoff = self.handoff
        self.assertNotIn(handoff.preflight.resolution.producer.sha,
                         {row.sha for row in current.chain.sources})
        self.assertNotEqual(current.inventory.main_sha, handoff.execution.workflow_sha)
        raw = self.build(current=current)
        value = common.loads_strict(raw)
        required = handoff.preflight.resolution.requirements
        self.assertEqual((value['schema_version'], value['kind'], value['sha'], value['images']),
                         (2, 'rollback', required.target.sha, dict(required.target.images)))
        self.assertEqual((value['previous'], value['rolled_back_from']),
                         (current.latest.release, current.latest.release))
        self.assertEqual((value['db_change'], value['dormant'], value['reversible'],
            value['old_server_ok'], value['compat'], value['compat_run_id'], value['staging']),
            ('none', False, True, True, 'ok', handoff.preflight.qualified.run_id, 'bypassed'))
        self.assertIsNone(value['fixed_forward_from'])
        self.assertEqual({key: value[key] for key in required.live.catalog._fields}, required.live.catalog._asdict())
        new = appended(current, raw)
        self.assertEqual(new.latest.live, current.latest.live)
        self.assertEqual(new.latest.actual_web_source_sha, required.target.record.web_source_sha)
        with self.assertRaisesRegex(policy.PolicyRefusal, 'rollback-qualification-unavailable'):
            policy.rollback_plan(current.chain, required.target.release)

    def test_real_plain_reader_overlap_only_upgrades_original_H(self):
        case, handoff = rollback_fixture(self, producer_overlap=True)
        current = rollback_plain(case)
        old = next(row for row in current.chain.sources if row.sha == handoff.execution.workflow_sha)
        self.assertEqual(old.mode, 'legacy-reader0')
        raw = writer.build_qualified_rollback(handoff, rollback_publication(handoff), current)
        self.assertEqual(common.loads_strict(raw)['sha'], handoff.preflight.resolution.requirements.target.sha)

    def test_same_digest_duplicate_target_does_not_change_exact_requested_span(self):
        required = self.handoff.preflight.resolution.requirements
        selected = self.handoff.preflight.resolution.resolved.selected
        self.assertNotEqual(required.target.sha, selected.actual_web_source_sha)
        self.assertEqual(required.target.release, selected.release)
        bad = replace(self.handoff.preflight.resolution,
            resolved=replace(self.handoff.preflight.resolution.resolved,
                             selected=self.handoff.preflight.resolution.resolved.latest))
        with self.assertRaises(writer.WriterRefusal):
            writer.prepare_rollback(replace(self.handoff.preflight, resolution=bad), self.handoff.execution)

    def test_facts_or_qualified_verdict_injection_refuse(self):
        current = self.current()
        for bad in (replace(self.handoff, facts=self.handoff.facts._replace(compat='bypassed')),
                    replace(self.handoff, facts=self.handoff.facts._replace(catalog=None)),
                    replace(self.handoff, preflight=replace(self.handoff.preflight,
                        qualified=replace(self.handoff.preflight.qualified, run_id=999)))):
            with self.subTest(bad=bad.facts.compat), self.assertRaises(writer.WriterRefusal):
                self.build(handoff=bad, current=current)

    def test_original_entry_history_checked_and_expiry_are_replayed(self):
        value = self.handoff.preflight
        for bad in (replace(value, entry_at=value.checked_at + timedelta(seconds=1)),
                    replace(value, checked_at=value.collected.history.observed_at - timedelta(seconds=1)),
                    replace(value, checked_at=value.artifact_expires_at),
                    replace(value, artifact_expires_at=value.artifact_expires_at + timedelta(seconds=1)),
                    replace(value, entry_at=value.entry_at.replace(tzinfo=None)),
                    replace(value, command_count=True), replace(value, captured_bytes=0)):
            with self.subTest(bad=bad.checked_at), self.assertRaises(writer.WriterRefusal):
                writer.prepare_rollback(bad, self.handoff.execution)

    def test_later_retry_uses_original_time_without_fetch_or_clock(self):
        current = self.current()
        with mock.patch.object(writer.rollback_preflight, '_source', side_effect=AssertionError('fetch')), \
             mock.patch.object(writer.rollback_preflight, '_actions', side_effect=AssertionError('fetch')), \
             mock.patch.object(writer.rollback_preflight.time, 'monotonic', side_effect=AssertionError('clock')):
            raw = self.build(current=current)
        self.assertEqual(common.loads_strict(raw)['compat'], 'ok')
        self.assertLess(self.handoff.preflight.artifact_expires_at,
                        common.require_timestamp('2027-01-01T00:00:00Z', 'fixture'))

    def test_only_original_ship_H_and_attempt1_can_prepare(self):
        for changed in (execution(workflow_sha='b' * 40), replace(self.handoff.execution, run_attempt=2),
                        replace(self.handoff.execution, run_id=True),
                        replace(self.handoff.execution, repository='PRIVATE'),
                        replace(self.handoff.execution, workflow=evidence_workflow())):
            with self.subTest(value=changed), self.assertRaises(writer.WriterRefusal):
                writer.prepare_rollback(self.handoff.preflight, changed)

    def test_publication_execution_or_preobservation_timestamp_refuse(self):
        current = self.current()
        for pub in (replace(rollback_publication(self.handoff), execution=replace(self.handoff.execution, run_id=33)),
                    replace(rollback_publication(self.handoff), deployed_at='2026-10-11T12:00:00Z')):
            with self.assertRaises(writer.WriterRefusal): self.build(current=current, publication_value=pub)

    def test_same_second_publication_preserves_full_fractional_checked_time(self):
        preflight = replace(self.handoff.preflight,
                            checked_at=self.handoff.preflight.checked_at.replace(microsecond=900000))
        handoff = writer.prepare_rollback(preflight, self.handoff.execution)
        stamp = preflight.checked_at.replace(microsecond=0).isoformat().replace('+00:00', 'Z')
        pub = replace(rollback_publication(handoff), deployed_at=stamp)
        current = self.current()
        raw = self.build(handoff=handoff, current=current, publication_value=pub)
        self.assertEqual(common.loads_strict(raw)['deployed_at'], stamp)
        self.assertEqual(handoff.preflight.checked_at.microsecond, 900000)
        prior = (preflight.checked_at.replace(microsecond=0) - timedelta(seconds=1)).isoformat().replace('+00:00', 'Z')
        with self.assertRaises(writer.WriterRefusal):
            self.build(handoff=handoff, current=current, publication_value=replace(pub, deployed_at=prior))

    def test_fresh_changed_signed_parent_refuses_even_same_images(self):
        self.case.case.add(kind='restore')
        with self.assertRaisesRegex(writer.WriterRefusal, 'parent-changed'):
            self.build()

    def test_changed_historical_source_or_ancestor_is_not_H_enrichment(self):
        current = self.current()
        for chain in (replace(current.chain, ancestors=current.chain.ancestors[:-1]),
                      replace(current.chain, sources=current.chain.sources[:-1]),
                      replace(current.chain, inputs=current.chain.inputs[:-1])):
            with self.assertRaises(writer.WriterRefusal):
                self.build(current=replace(current, chain=chain))

    def test_all_component_image_bindings_and_latest_selection_are_preserved(self):
        current = self.current(); bindings = list(current.image_bindings)
        bindings[1] = replace(bindings[1], source_shas=tuple((part, 'e' * 40) for part in common.COMPONENTS))
        for bad in (replace(current, image_bindings=tuple(bindings)),
                    replace(current, selected=resolver._selection(current.chain, 1))):
            with self.assertRaises(writer.WriterRefusal): self.build(current=bad)

    def test_false_actual_measurements_and_later_foreign_work_refuse(self):
        import test_compat_rollback_request as request_cases
        h = request_cases.histories
        value = self.handoff.preflight
        binding = value.qualified.binding
        for rows in ((h.row(binding, server_false='workers'),),
                     (h.row(binding, migration_false='rw'),),
                     (h.row(binding), h.row(binding, run=101, number=11, minute=20, sha='b'*40))):
            collected = replace(value.collected, history=h.history(binding, *rows))
            with self.assertRaises(writer.WriterRefusal):
                writer.prepare_rollback(replace(value, collected=collected), self.handoff.execution)

    def test_zero_one_legacy_and_normal_values_cannot_substitute_a_preflight(self):
        for value in (None, {}, self.handoff.preflight.resolution, self.handoff.preflight.collected,
                      self.handoff.preflight.qualified):
            with self.assertRaises(writer.WriterRefusal): writer.prepare_rollback(value, self.handoff.execution)

    def test_published_idempotency_does_not_require_replaying_old_proof(self):
        current = self.current(); raw = self.build(current=current)
        final = appended(current, raw)
        with mock.patch.object(writer, 'prepare_rollback', side_effect=AssertionError('replay')):
            self.assertEqual(writer.published_state(final, raw), 'published')
        other = dict(common.loads_strict(raw)); other['reconciled'] = True
        with self.assertRaises(writer.WriterRefusal):
            writer.published_state(final, common.canonical_file_bytes(other))

    def test_public_outputs_are_immutable_and_reconciled_type_is_strict(self):
        with self.assertRaises(FrozenInstanceError): self.handoff.facts = None
        current = self.current()
        for value in (1, 'true', None):
            with self.assertRaises(writer.WriterRefusal): self.build(current=current, reconciled=value)
        self.assertTrue(common.loads_strict(self.build(current=current, reconciled=True))['reconciled'])


def evidence_workflow():
    return writer.evidence.WORKFLOW


if __name__ == '__main__':
    unittest.main()
