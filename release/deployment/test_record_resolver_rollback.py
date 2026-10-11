"""Rollback source operations using synthetic Git/GitHub/verifier captures."""
import unittest
from unittest import mock
from dataclasses import replace
import hashlib
import json
from pathlib import Path
import subprocess

import test_record_resolver as fixtures

resolver, records = fixtures.resolver, fixtures.records
policy, rt, schema_change = fixtures.policy, fixtures.rt, fixtures.schema_change


class RollbackResolverTests(unittest.TestCase):
    setUpClass = fixtures.ResolverTests.__dict__['setUpClass']
    setUp = fixtures.ResolverTests.setUp
    add = fixtures.ResolverTests.add
    profile = fixtures.ForwardResolverTests.profile
    WORKFLOW = fixtures.ForwardResolverTests.WORKFLOW
    MAIN = fixtures.ForwardResolverTests.MAIN
    RECIPE = fixtures.ForwardResolverTests.RECIPE

    def rollback(self, tag):
        self.assertIsNone(self.client)
        config, work = self.root / 'config', self.root / 'work'
        config.mkdir(); work.mkdir()
        budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
        self.client = rt.ReleaseGh(self.gh, fixtures.support.GH_TOKEN, config, work,
                                  budget=budget, base_env={'SHIP_DO_TOKEN': 'PRIVATE'})
        return resolver.resolve_rollback(self.client, self.repo, self.git, tag)

    def legacy(self):
        old = self.add(version=1)
        latest = self.add(version=1)
        for doc in (self.boot, old, latest):
            self.profile(doc['sha'])
        return old, latest

    def closed(self, failed=False):
        self.assertTrue(self.client.budget.closed)
        self.assertEqual(self.client.budget.failed, failed)
        self.assertIsNone(records._GIT_OUTPUT_TRANSPORT.get())

    def test_all_v1_uses_exact_authenticated_documents_without_live_invention(self):
        old, latest = self.legacy()
        result = self.rollback(old['release'])
        self.assertEqual(result.resolved.selected.release, old['release'])
        self.assertEqual(result.resolved.latest.release, latest['release'])
        self.assertIsNone(result.resolved.latest.live)
        self.assertIsNone(result.resolved.chain.baseline)
        self.assertIsNone(result.decision)
        self.assertEqual(result.legacy_classification.reasons, ())
        self.assertEqual(len(result.legacy_chain.entries), 3)
        for entry, raw in zip(result.legacy_chain.entries, result.resolved.chain.inputs, strict=True):
            self.assertEqual(fixtures.common.canonical_file_bytes(entry.document), raw.raw)
            self.assertEqual(entry.manifest_sha256, raw.manifest_sha256)
        self.closed()

    def test_legacy_latest_target_preserves_restore_intent(self):
        _, latest = self.legacy()
        result = self.rollback(latest['release'])
        self.assertEqual(result.resolved.selected, result.resolved.latest)
        self.assertIsNotNone(result.legacy_chain)
        self.closed()

    def test_legacy_bootstrap_exact_target_is_preserved(self):
        self.legacy()
        result = self.rollback(self.boot['release'])
        self.assertEqual(result.resolved.selected.index, 0)
        self.assertEqual(result.legacy_chain.bootstrap.document, self.boot)
        self.closed()

    def test_legacy_guard_runs_inside_original_immutable_source_scope(self):
        old, _ = self.legacy()
        original = schema_change.guard
        seen = []
        def check(*args, **kwargs):
            self.assertIsNotNone(records._GIT_OUTPUT_TRANSPORT.get())
            self.assertFalse(self.client.budget.closed)
            seen.append((args, kwargs))
            return original(*args, **kwargs)
        with mock.patch.object(schema_change, 'guard', side_effect=check):
            self.rollback(old['release'])
        self.assertEqual(len(seen), 1)
        self.assertEqual(seen[0][1], {'mode': 'rollback'})
        self.closed()

    def test_real_legacy_catalog_change_refuses(self):
        old, latest = self.legacy()
        self.world.files[latest['sha'], policy.GOLDEN] += b'\n'
        with self.assertRaises(schema_change.SchemaChangeBlocked):
            self.rollback(old['release'])
        self.closed(True)

    def test_legacy_rollback_keeps_existing_v0_migration_path_exemption(self):
        old, latest = self.legacy()
        self.profile(latest['sha'], closure='b' * 64, body=2)
        result = self.rollback(old['release'])
        self.assertEqual(result.legacy_classification.reasons, ())
        self.closed()

    def test_pre_golden_bootstrap_target_uses_existing_reviewed_v0_exception(self):
        latest = self.add(version=1)
        self.profile(latest['sha'])
        result = self.rollback(self.boot['release'])
        self.assertEqual(result.legacy_classification.reasons, ())
        self.assertIsNone(result.resolved.latest.live)
        self.closed()

    def test_mixed_v2_uses_chain_policy_without_any_legacy_guard(self):
        old = self.add(version=1)
        latest = self.add()
        with mock.patch.object(schema_change, 'guard', side_effect=AssertionError('legacy route')):
            result = self.rollback(old['release'])
        self.assertIsNone(result.legacy_chain)
        self.assertIsNone(result.legacy_classification)
        self.assertEqual(result.decision.target.release, old['release'])
        self.assertEqual(result.decision.live.origin_release, latest['release'])
        self.assertEqual(result.decision.live, result.resolved.latest.live)
        self.closed()

    def test_v2_restore_and_rolled_back_image_retain_database_origin(self):
        old = self.add(version=1, images=self.boot['images'])
        origin = self.add()
        latest = self.add(kind='rollback', target=old)
        result = self.rollback(latest['release'])
        self.assertEqual(result.resolved.selected, result.resolved.latest)
        self.assertEqual(result.decision.live.origin_release, origin['release'])
        self.assertEqual(result.resolved.selected.record_sha, old['sha'])
        self.assertEqual(result.resolved.selected.actual_web_source_sha, self.boot['sha'])
        self.assertEqual(len({old['sha'], self.boot['sha'], origin['sha']}), 3)
        self.closed()

    def test_irreversible_intermediate_refuses_with_fixed_policy_code(self):
        old = self.add(version=1)
        self.add(reversible=False)
        with self.assertRaisesRegex(policy.PolicyRefusal, '^rollback-irreversible-span$'):
            self.rollback(old['release'])
        self.closed(True)

    def test_two_non_none_records_need_dedicated_proof(self):
        old = self.add(version=1)
        self.add(db_change='data', dormant=True)
        self.add(db_change='data', dormant=True)
        with self.assertRaisesRegex(policy.PolicyRefusal, '^rollback-qualification-unavailable$'):
            self.rollback(old['release'])
        self.closed(True)

    def test_one_non_none_record_is_retained_in_complete_span(self):
        old = self.add(version=1)
        self.add(db_change='data', dormant=True)
        result = self.rollback(old['release'])
        self.assertEqual(result.decision.non_none_count, 1)
        self.assertEqual(len(result.decision.intermediates), 1)
        self.closed()

    def test_missing_explicit_selection_is_not_moving_latest(self):
        self.add()
        with self.assertRaises(rt.TransportRefusal):
            self.rollback(None)
        self.closed(True)

    def test_v1_after_v2_cannot_reopen_legacy_route(self):
        self.add()
        latest = self.add(version=1)
        with self.assertRaisesRegex(policy.PolicyRefusal, '^record-policy-invalid$'):
            self.rollback(latest['release'])
        self.closed(True)

    def test_transport_driver_message_is_not_a_public_policy_error(self):
        old, _ = self.legacy()
        self.world.before = lambda argv: ((_ for _ in ()).throw(ValueError('PRIVATE'))
                                         if '--version' not in argv else None)
        with self.assertRaises(rt.TransportRefusal) as caught:
            self.rollback(old['release'])
        self.assertNotIn('PRIVATE', str(caught.exception))
        self.closed(True)

    def test_expired_original_budget_cannot_skip_final_refresh(self):
        old, _ = self.legacy()
        original = schema_change.guard
        def expire(*args, **kwargs):
            result = original(*args, **kwargs)
            self.clock.now += 601
            return result
        with mock.patch.object(schema_change, 'guard', side_effect=expire), self.assertRaises(rt.TransportRefusal):
            self.rollback(old['release'])
        self.closed(True)


class RollbackProofResolverTests(unittest.TestCase):
    """Actual preparatory resolver with controlled Git/GH/signature captures."""
    setUpClass = fixtures.ResolverTests.__dict__['setUpClass']
    setUp = fixtures.ResolverTests.setUp
    add = fixtures.ResolverTests.add
    profile = fixtures.ForwardResolverTests.profile
    WORKFLOW = fixtures.ForwardResolverTests.WORKFLOW
    MAIN = fixtures.ForwardResolverTests.MAIN
    RECIPE = fixtures.ForwardResolverTests.RECIPE
    closed = RollbackResolverTests.closed

    def scenario(self, *, target_sha=None, target_images=None):
        target = self.add(version=1, sha=target_sha, images=target_images)
        first = self.add(db_change='data', dormant=True)
        latest = self.add(db_change='expand', dormant=True)
        self.profile(target['sha']); self.profile(latest['sha'])
        producer = self.world.main
        self.profile(producer)
        return producer, target, first, latest

    def proof(self, producer, target):
        self.assertIsNone(self.client)
        config, work = self.root / 'config', self.root / 'work'
        config.mkdir(); work.mkdir()
        budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
        self.client = rt.ReleaseGh(self.gh, fixtures.support.GH_TOKEN, config, work,
                                  budget=budget, base_env={'SHIP_DO_TOKEN': 'PRIVATE'})
        return resolver.resolve_rollback_proof(self.client, self.repo, self.git, producer, target)

    def refused(self, producer, target, code=None):
        with self.assertRaises((rt.TransportRefusal, policy.PolicyRefusal)) as caught:
            self.proof(producer, target)
        if code is not None:
            self.assertEqual(caught.exception.args, (code,))
        self.assertNotIn('PRIVATE', str(caught.exception))
        self.closed(True)

    def test_complete_exact_span_retains_real_source_roles_without_forward_or_decision(self):
        producer, target, first, latest = self.scenario()
        self.world.files[target['sha'], self.RECIPE] += b'# target\n'
        self.world.files[latest['sha'], self.RECIPE] += b'# live\n'
        result = self.proof(producer, target['release'])
        self.assertIs(type(result), resolver.RollbackProofResolution)
        self.assertEqual(result.producer.sha, result.resolved.inventory.main_sha)
        self.assertEqual(result.requirements.non_none_count, 2)
        self.assertEqual(result.requirements.span,
            tuple((entry.release, entry.record.manifest_sha256) for entry in result.resolved.chain.entries[1:]))
        self.assertEqual(result.requirements.live.origin_release, latest['release'])
        self.assertEqual(result.target_recipe.source_sha, target['sha'])
        self.assertEqual(result.live_recipe.source_sha, latest['sha'])
        self.assertNotEqual(result.target_recipe.raw, result.live_recipe.raw)
        self.assertEqual(result.workflow_sha256, fixtures.common.sha256_bytes(result.workflow))
        self.assertEqual({row.source_sha for row in result.materials},
                         {producer, target['sha'], first['sha'], latest['sha']})
        for material in result.materials:
            self.assertEqual(material.provenance, 'equal-reviewed-v0')
            self.assertEqual(tuple((row.path, fixtures.common.sha256_bytes(row.raw)) for row in material.blobs),
                             result.producer.artifacts)
        self.assertFalse(hasattr(result.requirements, 'image_only_facts'))
        self.assertNotIsInstance(result, resolver.ForwardResolution)
        with self.assertRaises(fixtures.FrozenInstanceError):
            result.producer = None
        self.closed()

    def test_public_rollback_still_refuses_same_supported_span(self):
        _, target, _, _ = self.scenario()
        with self.assertRaisesRegex(policy.PolicyRefusal, '^rollback-qualification-unavailable$'):
            RollbackResolverTests.rollback(self, target['release'])
        self.closed(True)

    def test_rolled_back_live_image_keeps_origin_and_duplicate_tags(self):
        producer, target, _, origin = self.scenario()
        latest = self.add(kind='rollback', target=target)
        result = self.proof(producer, target['release'])
        self.assertEqual(result.resolved.latest.record_sha, target['sha'])
        self.assertEqual(result.live_recipe, result.target_recipe)
        self.assertEqual(result.requirements.live.origin_source.sha, origin['sha'])
        self.assertEqual(result.requirements.span[-1][0], latest['release'])
        self.assertEqual(len(result.requirements.span), 4)
        self.assertEqual(len({tag for tag, _ in result.requirements.span}), 4)
        self.assertEqual(result.requirements.non_none_count, 2)
        self.closed()

    def test_repeated_producer_source_requires_complete_facts_before_legacy_decode(self):
        producer, target, _, _ = self.scenario(target_sha=self.world.main)
        result = self.proof(producer, target['release'])
        self.assertEqual(result.resolved.selected.reader, result.producer)
        self.assertEqual(result.resolved.selected.reader.mode, 'reviewed-v0')
        self.assertEqual(sum(row.source_sha == producer for row in result.materials), 1)
        self.closed()

    def test_repeated_producer_missing_complete_does_not_fall_back_to_reader0(self):
        producer, target, _, _ = self.scenario(target_sha=self.world.main)
        del self.world.files[producer, policy.PROFILE]
        self.refused(producer, target['release'])

    def test_e001_only_two_absent_fields_and_actual_image_source_are_preserved(self):
        producer, target, _, _ = self.scenario(target_sha=policy.V0_RELEASED_SOURCE)
        actual = policy.V0_RELEASED_SOURCE
        self.profile(actual)
        for path in (policy.PROFILE, policy.OVERLAY):
            del self.world.files[actual, path]
        result = self.proof(producer, target['release'])
        self.assertEqual(result.target_recipe.source_sha, actual)
        material = next(row for row in result.materials if row.source_sha == actual)
        self.assertEqual(material.provenance, 'legacy-e001-absent-overlay')
        self.assertEqual({row.path for row in material.blobs if row.raw is None},
                         {policy.PROFILE, policy.OVERLAY})
        for row in material.blobs:
            if row.raw is not None:
                self.assertEqual(fixtures.common.sha256_bytes(row.raw), dict(result.producer.artifacts)[row.path])
        self.assertEqual(result.resolved.selected.reader.mode, 'legacy-reader0')
        self.closed()

    def test_bootstrap_image_source_distinct_from_record_requires_all_seven_present(self):
        producer, target, _, _ = self.scenario(target_images=self.boot['images'])
        actual = self.boot['legacy_signer']['source_commit']
        self.assertNotEqual(actual, policy.V0_RELEASED_SOURCE)
        self.profile(actual)
        result = self.proof(producer, target['release'])
        self.assertNotEqual(result.resolved.selected.record_sha, actual)
        self.assertEqual(result.target_recipe.source_sha, actual)
        material = next(row for row in result.materials if row.source_sha == actual)
        self.assertEqual(material.provenance, 'equal-reviewed-v0')
        self.assertEqual(tuple((row.path, fixtures.common.sha256_bytes(row.raw)) for row in material.blobs),
                         result.producer.artifacts)
        self.assertEqual(result.resolved.selected.reader.mode, 'legacy-reader0')
        self.closed()

    def test_e001_exception_never_ignores_present_profile_or_unequal_seed_bytes(self):
        for path in (policy.PROFILE, policy.OVERLAY, policy.SEEDS, policy.STEPS):
            with self.subTest(path=path):
                case = type(self)('test_e001_only_two_absent_fields_and_actual_image_source_are_preserved')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario(target_sha=policy.V0_RELEASED_SOURCE)
                    actual = policy.V0_RELEASED_SOURCE
                    case.profile(actual)
                    for absent in (policy.PROFILE, policy.OVERLAY):
                        if absent != path:
                            del case.world.files[actual, absent]
                    if path in (policy.SEEDS, policy.STEPS):
                        case.world.files[actual, path] += b'\n'
                    case.refused(producer, target['release'], 'rollback-proof-source-unsupported')
                finally:
                    case.doCleanups()

    def test_every_non_e001_endpoint_blob_is_required_and_seed_registry_must_equal(self):
        for path in policy.SOURCE_PATHS:
            with self.subTest(path=path):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario()
                    del case.world.files[target['sha'], path]
                    case.refused(producer, target['release'], 'rollback-proof-source-unsupported')
                finally:
                    case.doCleanups()
        for path in (policy.SEEDS, policy.STEPS):
            with self.subTest(unequal=path):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario()
                    case.world.files[producer, path] += b'\n'  # Valid decoder shape; unequal exact bytes.
                    case.refused(producer, target['release'], 'rollback-proof-source-unsupported')
                finally:
                    case.doCleanups()

    def test_active_intermediate_refuses_before_material_and_recipe_collection(self):
        target = self.add(version=1)
        self.add(db_change='data', dormant=False)
        self.add(db_change='expand', dormant=True)
        producer = self.profile(self.world.main)
        self.refused(producer, target['release'], 'rollback-proof-source-unsupported')
        self.assertFalse(any(args[-1] == self.RECIPE for args, _ in self.world.trace))

    def test_intermediate_origin_seed_or_registry_drift_refuses_even_when_endpoints_match(self):
        for path, field in ((policy.SEEDS, 'seeds_sha256'), (policy.STEPS, 'data_steps_sha256')):
            with self.subTest(path=path):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    target = case.add(version=1)
                    changed = case.source_bytes[path] + b'\n'
                    first = case.add(db_change='data', dormant=True,
                                     **{field: fixtures.common.sha256_bytes(changed)})
                    case.world.files[first['sha'], path] = changed
                    case.add(db_change='data', dormant=True)
                    producer = case.profile(case.world.main)
                    case.refused(producer, target['release'], 'rollback-proof-source-unsupported')
                finally:
                    case.doCleanups()

    def test_none_and_restore_records_and_their_origins_remain_in_full_span(self):
        producer, target, first, expanded = self.scenario()
        none = self.add()
        latest = self.add(kind='restore')
        self.profile(latest['sha'])
        result = self.proof(producer, target['release'])
        self.assertEqual(tuple(tag for tag, _ in result.requirements.span),
                         tuple(row['release'] for row in (target, first, expanded, none, latest)))
        self.assertEqual(result.requirements.non_none_count, 2)
        self.assertEqual(result.requirements.live.origin_release, none['release'])
        self.assertEqual({row.source_sha for row in result.materials},
                         {producer, target['sha'], first['sha'], expanded['sha'], none['sha']})
        self.closed()

    def test_irreversible_intermediate_is_not_overridden_by_dormant_count(self):
        target = self.add(version=1)
        self.add(reversible=False)
        self.add(db_change='data', dormant=True); self.add(db_change='data', dormant=True)
        producer = self.profile(self.world.main)
        self.refused(producer, target['release'], 'rollback-irreversible-span')

    def test_zero_one_and_all_v1_are_not_converted_into_new_proof_lane(self):
        for count in (0, 1):
            with self.subTest(count=count):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    target = case.add()
                    if count:
                        case.add(db_change='data', dormant=True)
                    producer = case.profile(case.world.main)
                    case.refused(producer, target['release'], 'rollback-proof-not-required')
                finally:
                    case.doCleanups()
        target = self.add(version=1); self.add(version=1)
        producer = self.profile(self.world.main)
        self.refused(producer, target['release'], 'record-policy-legacy-live-unavailable')

    def test_producer_must_equal_captured_main_before_signature_or_git_work(self):
        producer, target, _, _ = self.scenario()
        self.world.main = 'b' * 40
        self.refused(producer, target['release'], 'record-chain-invalid:transport-resolver')
        self.assertFalse(any(args[0] == str(self.git) or args[1:3] == ['attestation', 'verify']
                             for args, _ in self.world.trace))

    def test_invalid_source_and_target_are_refused_before_snapshot(self):
        for producer, target in (('main', 'prod-0000'), ('a'*40, None), ('a'*40, 'main')):
            with self.subTest(producer=producer, target=target):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    case.refused(producer, target)
                    self.assertEqual(len(case.world.trace), 1)
                finally:
                    case.doCleanups()

    def test_producer_must_descend_from_live_record_and_retained_origin(self):
        for role in ('record', 'origin'):
            with self.subTest(role=role):
                case = type(self)('test_rolled_back_live_image_keeps_origin_and_duplicate_tags')
                case.setUp()
                try:
                    producer, target, _, origin = case.scenario()
                    latest = case.add(kind='rollback', target=target)
                    base = latest['sha'] if role == 'record' else origin['sha']
                    case.world.bad_pairs.add((base, producer))
                    case.refused(producer, target['release'], 'rollback-proof-source-unsupported')
                finally:
                    case.doCleanups()

    def test_wrong_regular_mode_and_recipe_absence_fail_closed(self):
        for mode in ('symlink', 'recipe', 'workflow'):
            with self.subTest(mode=mode):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario()
                    if mode == 'symlink':
                        original = case.world.git
                        def git(args):
                            result = original(args)
                            if args == ['ls-tree', '-z', target['sha'], '--', policy.GOLDEN]:
                                return rt.runtime.Capture(0, result.stdout.replace(b'100644 blob', b'120000 blob'), b'')
                            return result
                        case.world.git = git
                    elif mode == 'recipe':
                        del case.world.files[target['sha'], self.RECIPE]
                    else:
                        del case.world.files[producer, self.WORKFLOW]
                    case.refused(producer, target['release'])
                finally:
                    case.doCleanups()

    def test_material_collection_remains_inside_original_scope_and_budget(self):
        producer, target, _, _ = self.scenario()
        observed = []
        def check(args):
            if args[-5:] == ['ls-tree', '-z', target['sha'], '--', policy.GOLDEN]:
                observed.append(records._GIT_OUTPUT_TRANSPORT.get())
                self.assertFalse(self.client.budget.closed)
        self.world.before = check
        self.proof(producer, target['release'])
        self.assertTrue(observed)
        self.assertTrue(all(value is not None for value in observed))
        self.assertEqual(self.client.budget.commands, len(self.world.trace))
        self.assertGreater(self.client.budget.captured_bytes, 4 * len(self.source_bytes[policy.GOLDEN]))
        self.closed()

    def test_no_new_allowance_for_source_calls_bytes_or_elapsed_time(self):
        for mode in ('calls', 'bytes', 'time'):
            with self.subTest(mode=mode):
                case = type(self)('test_material_collection_remains_inside_original_scope_and_budget')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario()
                    if mode == 'time':
                        def expire(args):
                            if args[-5:] == ['ls-tree', '-z', producer, '--', self.WORKFLOW]:
                                case.clock.now = 701
                        case.world.after = expire
                        case.refused(producer, target['release'])
                    else:
                        name, value = ('MAX_COMMANDS', 80) if mode == 'calls' else ('MAX_BYTES', 5 * 1024 * 1024)
                        with mock.patch.object(rt, name, value):
                            case.refused(producer, target['release'])
                finally:
                    case.doCleanups()

    def test_final_git_and_complete_gh_rechecks_are_not_skipped(self):
        for mode in ('git', 'main', 'asset'):
            with self.subTest(mode=mode):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    producer, target, _, _ = case.scenario()
                    def change(args):
                        if args[-5:] == ['ls-tree', '-z', producer, '--', self.WORKFLOW]:
                            if mode == 'git': (case.repo / '.git' / 'shallow').write_text('synthetic')
                            elif mode == 'main': case.world.main = 'b' * 40
                            else: case.world.fake.releases[-1]['assets'][0]['id'] += 1
                    case.world.after = change
                    case.refused(producer, target['release'])
                    if mode == 'git':
                        self.assertEqual(sum(rt.API + '/git/ref/heads/main' in args
                                             for args, _ in case.world.trace), 1)
                finally:
                    case.doCleanups()

    def test_nonweb_attestation_failure_prevents_source_preparation(self):
        producer, target, _, latest = self.scenario()
        del self.world.fake.image_attestations[(fixtures.common.IMAGE_REPOSITORY['meta-relay'],
                                                latest['images']['meta-relay'])]
        self.refused(producer, target['release'])
        self.assertFalse(any(args[0] == str(self.git) for args, _ in self.world.trace))

    def test_final_recheck_is_after_last_recipe_bytes_and_still_within_deadline(self):
        for mode in ('main', 'deadline'):
            with self.subTest(mode=mode):
                case = type(self)('test_complete_exact_span_retains_real_source_roles_without_forward_or_decision')
                case.setUp()
                try:
                    producer, target, _, latest = case.scenario()
                    raw = b'FROM scratch\n# distinct last recipe\n'
                    case.world.files[latest['sha'], self.RECIPE] = raw
                    oid = fixtures.hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest()
                    reached = []
                    def change(args):
                        if args[-3:] == ['cat-file', 'blob', oid]:
                            reached.append(True)
                            if mode == 'main': case.world.main = 'b' * 40
                            else: case.clock.now = 701
                    case.world.after = change
                    case.refused(producer, target['release'])
                    self.assertEqual(reached, [True])
                finally:
                    case.doCleanups()


class RollbackMaterialDigestTests(unittest.TestCase):
    """Pure framing fixtures deliberately carry no authenticated source claim.

    Unprojected chain/policy fields are absent to establish that a commitment
    alone cannot substitute for the complete catalog/source validator.
    """
    def setUp(self):
        self.h='a'*40;self.target='b'*40;self.live='c'*40
        facts=policy.legacy_reader0(self.h)
        def selection(source):
            return resolver.ReleaseSelection(0,'fixture-not-a-release',source,'0'*64,(),source,facts,None)
        resolved=resolver.ResolvedChain(None,None,(),selection(self.live),selection(self.target))
        self.workflow=b'name: fixture\n'
        self.target_raw=b'FROM scratch\n# target\n';self.live_raw=b'FROM scratch\n# live\n'
        self.blobs=tuple(resolver.BlobObservation(path,b'raw:'+str(i).encode())
                         for i,path in enumerate(policy.SOURCE_PATHS))
        self.value=resolver.RollbackProofResolution(resolved,None,facts,self.workflow,self.digest(self.workflow),
            (resolver.RollbackSourceMaterial('1'*40,'equal-reviewed-v0',self.blobs),),
            resolver.WebRecipe(self.target,'docker/release/web.Dockerfile',self.target_raw,self.digest(self.target_raw)),
            resolver.WebRecipe(self.live,'docker/release/web.Dockerfile',self.live_raw,self.digest(self.live_raw)))

    @staticmethod
    def digest(raw):
        return hashlib.sha256(raw).hexdigest()

    @staticmethod
    def canonical(value):
        return json.dumps(value,sort_keys=True,separators=(',',':'),ensure_ascii=True).encode('ascii')

    def expected(self):
        return {'purpose':'rollback-v0-material-v1','producer_sha':self.h,
            'workflow':{'path':'.github/workflows/compat.yml','bytes':len(self.workflow),'sha256':self.digest(self.workflow)},
            'materials':[{'source_sha':'1'*40,'provenance':'equal-reviewed-v0',
                'blobs':[[path,{'bytes':5,'sha256':self.digest(b'raw:'+str(i).encode())}]
                         for i,path in enumerate(policy.SOURCE_PATHS)]}],
            'recipes':{'target':{'source_sha':self.target,'path':'docker/release/web.Dockerfile',
                                 'bytes':len(self.target_raw),'sha256':self.digest(self.target_raw)},
                       'live':{'source_sha':self.live,'path':'docker/release/web.Dockerfile',
                               'bytes':len(self.live_raw),'sha256':self.digest(self.live_raw)}}}

    def rejected(self,value):
        with self.assertRaises(policy.PolicyRefusal) as caught:resolver.rollback_material_digest(value)
        self.assertEqual(caught.exception.args,('rollback-material-invalid',))

    def material(self,*,source='1'*40,provenance='equal-reviewed-v0',blobs=None):
        return resolver.RollbackSourceMaterial(source,provenance,self.blobs if blobs is None else blobs)

    def test_exact_raw_projection_domain_and_no_trailing_lf(self):
        raw=self.canonical(self.expected())
        actual=resolver.rollback_material_digest(self.value)
        self.assertEqual(actual,self.digest(raw))
        self.assertNotEqual(actual,self.digest(raw+b'\n'))
        # Deliberately partial non-v0 framing is not source/chain authority.
        self.assertIsNone(self.value.resolved.chain);self.assertEqual(self.value.producer.mode,'legacy-reader0')

    def test_no_io_client_construction_or_policy_validation(self):
        with mock.patch.object(Path,'read_bytes',side_effect=AssertionError('file read')), \
             mock.patch.object(subprocess,'Popen',side_effect=AssertionError('process')), \
             mock.patch.object(resolver.records,'git',side_effect=AssertionError('Git')), \
             mock.patch.object(resolver.transport,'ReleaseGh',side_effect=AssertionError('client')), \
             mock.patch.object(resolver.policy,'normalize_chain',side_effect=AssertionError('policy')):
            self.assertEqual(resolver.rollback_material_digest(self.value),self.digest(self.canonical(self.expected())))

    def test_each_raw_observation_and_source_or_provenance_changes_commitment(self):
        baseline=resolver.rollback_material_digest(self.value);changes=[]
        for i,blob in enumerate(self.blobs):
            rows=tuple(replace(row,raw=row.raw+b'x') if n==i else row for n,row in enumerate(self.blobs))
            changes.append(replace(self.value,materials=(self.material(blobs=rows),)))
        changes += [replace(self.value,producer=replace(self.value.producer,sha='d'*40)),
                    replace(self.value,materials=(self.material(source='2'*40),)),
                    replace(self.value,materials=(self.material(provenance='legacy-e001-absent-overlay'),)),
                    replace(self.value,workflow=self.workflow+b'x',workflow_sha256=self.digest(self.workflow+b'x'))]
        for role in ('target_recipe','live_recipe'):
            recipe=getattr(self.value,role);raw=recipe.raw+b'x'
            changes.append(replace(self.value,**{role:replace(recipe,raw=raw,sha256=self.digest(raw))}))
            selected='selected' if role=='target_recipe' else 'latest'
            resolution=replace(self.value.resolved,**{selected:replace(getattr(self.value.resolved,selected),
                                                                     actual_web_source_sha='d'*40)})
            changes.append(replace(self.value,resolved=resolution,**{role:replace(recipe,source_sha='d'*40)}))
        for i,value in enumerate(changes):
            with self.subTest(index=i):self.assertNotEqual(resolver.rollback_material_digest(value),baseline)

    def test_equal_endpoint_roles_are_both_committed_without_deduplication(self):
        same=replace(self.value,resolved=replace(self.value.resolved,latest=self.value.resolved.selected),
                     live_recipe=self.value.target_recipe)
        expected=self.expected();expected['recipes']['live']=dict(expected['recipes']['target'])
        actual=resolver.rollback_material_digest(same)
        self.assertEqual(actual,self.digest(self.canonical(expected)))
        del expected['recipes']['live']
        self.assertNotEqual(actual,self.digest(self.canonical(expected)))

    def test_null_and_empty_bytes_are_distinct_without_absence_authority(self):
        results=[]
        for raw in (None,b'',b'raw:0'):
            rows=(replace(self.blobs[0],raw=raw),*self.blobs[1:])
            value=replace(self.value,materials=(self.material(blobs=rows),))
            results.append(resolver.rollback_material_digest(value))
            expected=self.expected()
            expected['materials'][0]['blobs'][0][1]=(None if raw is None else {'bytes':len(raw),'sha256':self.digest(raw)})
            self.assertEqual(results[-1],self.digest(self.canonical(expected)))
        self.assertEqual(len(set(results)),3)

    def test_material_inventory_order_unique_count_and_exact_types(self):
        first=self.material();second=self.material(source='2'*40)
        for rows in ((),[first],(first,first),(second,first),(object(),),
                     (self.material(source='bad'),),(self.material(provenance='PRIVATE-unknown'),),
                     (self.material(provenance=True),)):
            with self.subTest(rows=type(rows).__name__):self.rejected(replace(self.value,materials=rows))
        values=tuple(self.material(source=f'{i:040x}',blobs=tuple(replace(b,raw=None) for b in self.blobs))
                     for i in range(2*(records.MAX_RECORDS+1)))
        self.assertRegex(resolver.rollback_material_digest(replace(self.value,materials=values)),r'^[0-9a-f]{64}$')
        self.rejected(replace(self.value,materials=(*values,self.material(source='f'*40))))

    def test_all_seven_paths_cannot_be_omitted_reordered_duplicated_or_replaced(self):
        variants=(self.blobs[:-1],tuple(reversed(self.blobs)),(self.blobs[0],)*len(self.blobs),list(self.blobs),
                  (replace(self.blobs[0],path='PRIVATE/unknown'),*self.blobs[1:]),
                  (replace(self.blobs[0],path=True),*self.blobs[1:]),
                  (object(),*self.blobs[1:]))
        for rows in variants:
            with self.subTest(kind=type(rows).__name__):self.rejected(replace(self.value,materials=(self.material(blobs=rows),)))

    def test_recipe_path_source_and_stored_hash_disagreement_refuse(self):
        for role in ('target_recipe','live_recipe'):
            recipe=getattr(self.value,role)
            for field,value in (('path','Dockerfile'),('path',True),('source_sha','f'*40),('source_sha',False),
                                ('sha256','0'*64),('sha256',b'0'*64),('raw',recipe.raw+b'x'),('raw',b'')):
                with self.subTest(role=role,field=field):self.rejected(replace(self.value,**{role:replace(recipe,**{field:value})}))
        for value in ('0'*64,True,b'0'*64,'A'*64):self.rejected(replace(self.value,workflow_sha256=value))
        self.rejected(replace(self.value,workflow=self.workflow+b'x'))

    def test_exact_projection_and_sha_types_refuse_without_formatting_private_values(self):
        class Private:
            def __str__(self):raise AssertionError('private str')
            def __repr__(self):raise AssertionError('private repr')
            def __eq__(self,other):raise AssertionError('private equality')
        for value in (None,{},True,Private()):self.rejected(value)
        for value in (True,b'a'*40,'A'*40,'short',Private()):
            self.rejected(replace(self.value,producer=replace(self.value.producer,sha=value)))
            selected=replace(self.value.resolved.selected,actual_web_source_sha=value)
            self.rejected(replace(self.value,resolved=replace(self.value.resolved,selected=selected)))
        self.rejected(replace(self.value,resolved=Private()))
        self.rejected(replace(self.value,producer=Private()))
        self.rejected(replace(self.value,target_recipe=Private()))
        self.rejected(replace(self.value,materials=(self.material(provenance=Private()),)))

    def test_raw_types_and_nonempty_workflow_recipe_guards(self):
        for raw in (True,'PRIVATE',bytearray(b'raw'),memoryview(b'raw'),object()):
            self.rejected(replace(self.value,workflow=raw))
            self.rejected(replace(self.value,target_recipe=replace(self.value.target_recipe,raw=raw)))
            rows=(replace(self.blobs[0],raw=raw),*self.blobs[1:])
            self.rejected(replace(self.value,materials=(self.material(blobs=rows),)))
        self.rejected(replace(self.value,workflow=b'',workflow_sha256=self.digest(b'')))

    def test_per_file_and_per_material_boundaries(self):
        raw=b'x'*policy.MAX_SOURCE_FILE_BYTES
        two=tuple(replace(b,raw=raw if i<2 else None) for i,b in enumerate(self.blobs))
        self.assertRegex(resolver.rollback_material_digest(replace(self.value,materials=(self.material(blobs=two),))),r'^[0-9a-f]{64}$')
        over=tuple(replace(b,raw=b'x' if i==2 else b.raw) for i,b in enumerate(two))
        self.rejected(replace(self.value,materials=(self.material(blobs=over),)))
        too_big=raw+b'x'
        self.rejected(replace(self.value,workflow=too_big,workflow_sha256=self.digest(too_big)))
        self.rejected(replace(self.value,target_recipe=replace(self.value.target_recipe,raw=too_big,sha256=self.digest(too_big))))
        rows=(replace(self.blobs[0],raw=too_big),*self.blobs[1:])
        self.rejected(replace(self.value,materials=(self.material(blobs=rows),)))

    def test_aggregate_raw_bound_counts_both_recipe_roles(self):
        raw=b'x'*policy.MAX_SOURCE_FILE_BYTES
        overhead=len(self.workflow)+len(self.target_raw)+len(self.live_raw)
        rows=[]
        for i in range(rt.MAX_BYTES//policy.MAX_SOURCE_BYTES):
            tail=raw[:-overhead] if i==rt.MAX_BYTES//policy.MAX_SOURCE_BYTES-1 else raw
            blobs=tuple(replace(b,raw=raw if j==0 else tail if j==1 else None)
                        for j,b in enumerate(self.blobs))
            rows.append(self.material(source=f'{i:040x}',blobs=blobs))
        exact=replace(self.value,materials=tuple(rows))
        self.assertRegex(resolver.rollback_material_digest(exact),r'^[0-9a-f]{64}$')
        recipe=exact.live_recipe;changed=recipe.raw+b'x'
        self.rejected(replace(exact,live_recipe=replace(recipe,raw=changed,sha256=self.digest(changed))))


if __name__ == '__main__':
    unittest.main()
