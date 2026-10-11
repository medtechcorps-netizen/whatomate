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


if __name__ == '__main__':
    unittest.main()
