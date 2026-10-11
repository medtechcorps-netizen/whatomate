"""Rollback source operations using synthetic Git/GitHub/verifier captures."""
import unittest
from unittest import mock

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


if __name__ == '__main__':
    unittest.main()
