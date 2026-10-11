"""Concrete resolver/collector composition with synthetic command captures.

No GitHub, Git process, Docker, or real signature authority is exercised.
"""
from dataclasses import replace
from pathlib import Path
from unittest import mock
import unittest

import compat_request as bridge
import compat_evidence as evidence
import pending_source
import test_compat_collect as actions
import test_compat_evidence as fixtures
import test_record_resolver as source


def resolution(*, legacy=False, dormant=False):
    case = source.ForwardResolverTests()
    case.setUp()
    try:
        if legacy:
            old = case.add(version=1, sha=source.policy.V0_RELEASED_SOURCE)
            case.profile(old['sha'])
            for path in (source.policy.PROFILE, source.policy.OVERLAY):
                del case.world.files[old['sha'], path]
        else:
            old = case.add(version=1, images=case.boot['images'])
            origin = case.add()
            case.profile(origin['sha'])
            case.add(kind='rollback', target=old)
            case.world.files[case.boot['sha'], case.RECIPE] = b'FROM scratch\n'
        sha = case.candidate(fixtures.SHA)
        if dormant:
            case.world.files[sha, source.policy.SEEDS] += b'\n'
        value = case.forward(sha)
        assert case.client.budget.closed and not case.client.budget.failed
        return value
    finally:
        case.doCleanups()


def identities(value):
    latest = value.resolved.latest
    identity = evidence.ReleaseIdentity(latest.release, latest.manifest_sha256,
                                        latest.record_sha, dict(latest.images)['web'])
    if latest.live is None:
        base = evidence.DatabaseBase('legacy-source-only', latest.record_sha, None, None)
    else:
        live = latest.live
        base = evidence.DatabaseBase('retained-live-v2', live.origin_source.sha,
            evidence.OriginIdentity(live.origin_release, live.origin_manifest_sha256),
            evidence.CatalogTuple(*live.catalog))
    return identity, base


def pending(value):
    active = {row.path: row.raw for row in value.catalog_material.candidate_blobs}
    return pending_source.replay(value.pending, candidate_sha=value.candidate.sha,
        previous_sha=value.resolved.latest.record_sha, active_catalog=active[source.policy.GOLDEN],
        active_globals=active[source.policy.GLOBALS], active_history=active[source.policy.HISTORY]).binding


def bind_pairs(world, value, *, old_protocol=False, wrong_catalog=False, version=3):
    latest, base = identities(value)
    binding = pending(value) if not old_protocol and version == 3 else None
    for (run, attempt), original in tuple(world.pairs.items()):
        result = actions.json.loads(original.result)
        pair = fixtures.pair(run, attempt, latest=latest,
            classification=evidence.Classification(value.classification.kind, value.classification.dormant),
            base=base, web_source=value.previous_web_source_sha, version=1 if old_protocol else version,
            pending=binding,
            server_false=next((key for key, passed in result['old_server_ok'].items() if not passed), None))
        pair = fixtures.edit_pair(pair, result=lambda row: row.update(catalog_sha256=(
            dict(source.policy.V0_PINS)[source.policy.GOLDEN] if wrong_catalog else source.policy.V0_MEASUREMENT)))
        world.pairs[run, attempt] = pair
        for meta in world.artifacts[run]:
            if meta['name'].endswith('-' + str(attempt)):
                raw = actions.signature.archive(pair)
                world.archives[meta['id']] = raw, (run, attempt)
                meta['size_in_bytes'] = len(raw)


def collected(value, world=None, **options):
    world = world or actions.World()
    if not world.runs:
        world.add()
    bind_pairs(world, value, **options)
    with actions.harness(world) as (_, client, scratch):
        result = actions.execute(client, scratch)
    return result


class RequestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source.ForwardResolverTests.setUpClass()
        cls.resolution = resolution()
        cls.legacy = resolution(legacy=True)
        cls.dormant = resolution(dormant=True)
        cls.collected = collected(cls.resolution)

    def refuse(self, value=None, history=None, code='compat-stale'):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            bridge.qualify(self.resolution if value is None else value,
                           self.collected if history is None else history)
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn('PRIVATE', str(caught.exception))
        return caught.exception

    def test_concrete_resolver_collector_verifier_and_policy_compose(self):
        value, history = self.resolution, self.collected
        request = bridge.build_request(value, history)
        result = bridge.qualify(value, history)
        latest, base = identities(value)
        self.assertEqual(request.latest, latest)
        self.assertEqual(result.previous, latest)
        self.assertEqual(request.source.database_base, base)
        self.assertEqual(result.database_base, base)
        self.assertEqual(result.previous_web_source_sha, value.previous_web_source_sha)
        self.assertEqual(len({latest.sha, base.source_sha, result.previous_web_source_sha}), 3)
        self.assertEqual(result.catalog_sha256, source.policy.V0_MEASUREMENT)
        self.assertNotEqual(result.catalog_sha256, dict(source.policy.V0_PINS)[source.policy.GOLDEN])
        self.assertEqual(tuple((row.path, row.sha256) for row in result.catalog_authority),
                         tuple(sorted(source.policy.V0_PINS)))
        self.assertEqual(request.observation_id, history.history.observation_id)
        self.assertEqual(result.observation_id, history.history.observation_id)
        self.assertTrue(result.old_server and result.reversible)

    def test_legacy_source_only_never_invents_live_tuple(self):
        history = collected(self.legacy)
        result = bridge.qualify(self.legacy, history)
        self.assertEqual(result.database_base, evidence.DatabaseBase('legacy-source-only',
                         self.legacy.resolved.latest.record_sha, None, None))

    def test_real_dormant_classifier_result_is_preserved(self):
        self.assertTrue(self.dormant.classification.summary)
        history = collected(self.dormant)
        self.assertEqual(bridge.qualify(self.dormant, history).classification,
                         evidence.Classification('data', True))

    def test_bridge_performs_no_source_reads_commands_or_collection(self):
        with mock.patch.object(Path, 'open', side_effect=AssertionError('I/O')), \
             mock.patch.object(Path, 'read_bytes', side_effect=AssertionError('I/O')), \
             mock.patch.object(source.rt.runtime, 'bounded_command', side_effect=AssertionError('command')), \
             mock.patch.object(actions.collect, 'collect_history', side_effect=AssertionError('recollection')):
            self.assertTrue(bridge.qualify(self.resolution, self.collected).forward)

    def test_candidate_must_equal_captured_production_main(self):
        resolved = self.resolution.resolved
        self.refuse(replace(self.resolution, resolved=replace(resolved,
            inventory=replace(resolved.inventory, main_sha='f' * 40))))

    def test_historical_target_cannot_be_silently_treated_as_normal(self):
        resolved = self.resolution.resolved
        self.refuse(replace(self.resolution, resolved=replace(resolved,
            selected=source.resolver._selection(resolved.chain, 1))))

    def test_full_raw_chain_and_derived_projection_must_agree(self):
        value = self.resolution; resolved = value.resolved; chain = resolved.chain
        changes = [replace(chain, entries=chain.entries[:-1]),
                   replace(chain, inputs=chain.inputs[:-1]),
                   replace(chain, entries=chain.entries[:-1] + (replace(chain.entries[-1], live=None),)),
                   replace(chain, runtime_source_supported=False),
                   replace(chain, inputs=(replace(chain.inputs[0], raw=b'PRIVATE'),) + chain.inputs[1:])]
        for changed in changes:
            with self.subTest(change=changed.runtime_source_supported):
                self.refuse(replace(value, resolved=replace(resolved, chain=changed)))

    def test_inventory_missing_extra_foreign_or_rebound_records_refuses(self):
        value = self.resolution; resolved = value.resolved; inventory = resolved.inventory
        first = inventory.records[0]
        changes = [replace(inventory, records=inventory.records[:-1]),
                   replace(inventory, tags=inventory.tags[:-1]),
                   replace(inventory, repository_id=True),
                   replace(inventory, records=inventory.records + (first,)),
                   replace(inventory, records=(replace(first, digest='sha256:' + '0' * 64),) + inventory.records[1:]),
                   replace(inventory, tags=(replace(inventory.tags[0], sha='f' * 40),) + inventory.tags[1:])]
        for changed in changes:
            with self.subTest(inventory=changed.repository_id):
                self.refuse(replace(value, resolved=replace(resolved, inventory=changed)))

    def test_all_image_source_bindings_are_required(self):
        value = self.resolution; resolved = value.resolved
        changed = replace(resolved.image_bindings[1], source_shas=(('web', 'f' * 40),) +
                          resolved.image_bindings[1].source_shas[1:])
        for bindings in (resolved.image_bindings[:-1], (resolved.image_bindings[0], changed) + resolved.image_bindings[2:]):
            self.refuse(replace(value, resolved=replace(resolved, image_bindings=bindings)))

    def test_candidate_source_and_five_material_pins_are_checked(self):
        value = self.resolution; material = value.catalog_material
        first = material.candidate_blobs[0]
        changes = [replace(value, candidate=replace(value.candidate, mode='synthetic-policy')),
                   replace(value, candidate=replace(value.candidate, catalog=value.candidate.catalog._replace(catalog_sha256='f' * 64))),
                   replace(value, catalog_material=replace(material, candidate_sha='f' * 40)),
                   replace(value, catalog_material=replace(material, candidate_blobs=material.candidate_blobs[:-1])),
                   replace(value, catalog_material=replace(material, candidate_blobs=(first._replace(raw=b'PRIVATE'),) + material.candidate_blobs[1:]))]
        for changed in changes:
            with self.subTest(mode=changed.candidate.mode): self.refuse(changed)

    def test_origin_and_actual_web_source_substitutions_refuse(self):
        value = self.resolution; material = value.catalog_material
        for changed in (replace(value, database_base_source=value.previous_web_source_sha),
                        replace(value, previous_web_source_sha=value.database_base_source),
                        replace(value, catalog_material=replace(material, baseline_sha=value.previous_web_source_sha)),
                        replace(value, catalog_material=replace(material, baseline_blobs=material.baseline_blobs[:-1]))):
            self.refuse(changed)
        absent = self.legacy.catalog_material
        rows = tuple(replace(row, raw=b'PRIVATE') if row.raw is None else row for row in absent.baseline_blobs)
        self.refuse(replace(self.legacy, catalog_material=replace(absent, baseline_blobs=rows)))

    def test_full_classifier_shape_blockers_and_future_requirements_refuse(self):
        value = self.resolution; c = value.classification
        for changed in (c._replace(reasons=('PRIVATE',)), c._replace(kind='expand'),
                        c._replace(dormant=True), c._replace(summary=['omitted']),
                        c._replace(migration_changes=None), c._replace(requirements=()),
                        c._replace(requirements=c.requirements + ('future-format',))):
            self.refuse(replace(value, classification=changed))

    def test_source_workflow_bytes_hash_and_reviewed_collector_contract_agree(self):
        value = self.resolution; changed = value.workflow + b'\n'
        for row in (replace(value, workflow=changed), replace(value, workflow_sha256='f' * 64),
                    replace(value, workflow=changed, workflow_sha256=source.common.sha256_bytes(changed))):
            self.refuse(row)

    def test_collection_type_sha_and_observation_are_not_locally_invented(self):
        self.refuse(history=self.collected.history)
        for field, value in (('sha', 'f' * 40), ('observation_id', 'PRIVATE')):
            self.refuse(history=replace(self.collected, history=replace(self.collected.history, **{field:value})))
        for field, value in (('transcript_sha256', 'PRIVATE'), ('command_count', True), ('captured_bytes', -1)):
            self.refuse(history=replace(self.collected, **{field:value}))
        with self.assertRaises(TypeError):
            bridge.build_request(self.resolution, self.collected, observation_id='f' * 64)

    def test_actual_collected_negative_is_not_a_positive_request_shortcut(self):
        world = actions.World(); world.add(negative=True)
        self.refuse(history=collected(self.resolution, world), code='compat-old-server-incompatible')

    def test_old_context_and_golden_hash_cannot_be_new_positive(self):
        for options in ({'old_protocol':True}, {'wrong_catalog':True}):
            with self.subTest(options=options): self.refuse(history=collected(self.resolution, **options))

    def test_later_unknown_and_incomplete_enumeration_still_refuse(self):
        world = actions.World(); world.add(); world.add(run=101, number=11, minute=30, failure=True)
        self.refuse(history=collected(self.resolution, world))
        self.refuse(history=replace(self.collected,
            history=replace(self.collected.history, enumeration='incomplete')))

    def test_earlier_completed_unknown_issue_can_recover_under_existing_window_policy(self):
        world = actions.World(); world.add(); world.add(run=101, number=11, minute=30)
        world.signers[100, 1].fail_name = 'compat-context.json'
        history = collected(self.resolution, world)
        self.assertEqual(history.issues, ('artifact-unverified',))
        self.assertEqual(bridge.qualify(self.resolution, history).run_id, 101)

    def test_reused_rerun_jobs_never_gain_positive_window(self):
        world = actions.World(); world.add(attempts=2)
        for name in evidence.JOBS:
            prior = next(row for row in world.jobs[100, 1] if row['name'] == name)
            world.set_job(100, 2, name, started_at=prior['started_at'], completed_at=prior['completed_at'],
                          steps=prior['steps'])
        history = collected(self.resolution, world)
        self.assertFalse(history.history.attempts[-1].execution_window)
        self.refuse(history=history)


if __name__ == '__main__':
    unittest.main()
