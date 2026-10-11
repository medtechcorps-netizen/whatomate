"""Concrete source reader wiring with synthetic command captures only."""
import unittest
from unittest import mock

import ship_records as reader
import test_record_resolver as fixture


class ReaderTests(unittest.TestCase):
    setUpClass = fixture.ResolverTests.__dict__['setUpClass']
    setUp = fixture.ResolverTests.setUp
    add = fixture.ResolverTests.add

    def read(self, *, mode='chain', selected_tag=None, scratch=None, deadline=700):
        return reader._read(self.repo, gh_binary=self.gh, git_binary=self.git,
            token=fixture.support.GH_TOKEN, scratch_dir=scratch or self.root / 'read',
            deadline=deadline, selected_tag=selected_tag, mode=mode,
            capture=self.world, clock=self.clock, base_env={'SHIP_DO_TOKEN': 'PRIVATE'})

    def test_full_mixed_chain_is_concretely_verified_without_current_main_requirement(self):
        self.add(version=1)
        latest = self.add()
        self.world.main = 'f' * 40
        value = self.read()
        self.assertEqual(value.latest.record_sha, latest['sha'])
        self.assertEqual(value.inventory.main_sha, 'f' * 40)
        self.assertEqual(value.latest.live.origin_release, latest['release'])
        self.assertGreater(len(self.world.trace), 20)
        self.assertIsNone(fixture.records._GIT_OUTPUT_TRANSPORT.get())
        self.assertTrue(all('SHIP_DO_TOKEN' not in options['env'] for _, options in self.world.trace))

    def test_exact_rollback_invokes_chain_policy(self):
        old = self.add(version=1)
        self.add()
        value = self.read(mode='rollback', selected_tag=old['release'])
        self.assertEqual(value.resolved.selected.release, old['release'])
        self.assertEqual(value.decision.target.release, old['release'])

    def test_plain_historical_selection_does_not_grant_rollback(self):
        old = self.add(version=1)
        self.add(reversible=False)
        value = self.read(selected_tag=old['release'])
        self.assertIs(type(value), fixture.resolver.ResolvedChain)
        self.assertFalse(hasattr(value, 'decision'))

    def test_rollback_refusal_remains_fixed_and_pre_effect(self):
        old = self.add(version=1)
        self.add(reversible=False)
        with self.assertRaisesRegex(fixture.policy.PolicyRefusal, '^rollback-irreversible-span$'):
            self.read(mode='rollback', selected_tag=old['release'])

    def test_create_only_scratch_refuses_replay(self):
        self.add()
        self.read()
        count = len(self.world.trace)
        with self.assertRaises(fixture.rt.TransportRefusal):
            self.read()
        self.assertEqual(len(self.world.trace), count)

    def test_repository_scratch_and_missing_binary_refuse_before_capture(self):
        with self.assertRaises(fixture.rt.TransportRefusal):
            self.read(scratch=self.repo / 'scratch')
        self.gh.unlink()
        with self.assertRaises(fixture.rt.TransportRefusal):
            self.read()
        self.assertEqual(self.world.trace, [])

    def test_constructor_version_consumes_original_deadline(self):
        def advance(argv):
            if '--version' in argv:
                self.clock.now += 601
        self.world.after = advance
        with self.assertRaises(fixture.rt.TransportRefusal):
            self.read()
        self.assertEqual(len(self.world.trace), 1)

    def test_unknown_mode_or_missing_rollback_target_never_construct_client(self):
        for mode in ('future', 'rollback'):
            with self.subTest(mode=mode), self.assertRaises(fixture.rt.TransportRefusal):
                self.read(mode=mode)
        self.assertEqual(self.world.trace, [])

    def test_public_entry_uses_concrete_adapters_without_supplied_facts(self):
        self.add()
        with mock.patch.object(reader.runtime, 'bounded_command', self.world), \
                mock.patch.object(reader.time, 'monotonic', self.clock):
            value = reader.read(self.repo, gh_binary=self.gh, git_binary=self.git,
                token=fixture.support.GH_TOKEN, scratch_dir=self.root / 'public', deadline=700)
        self.assertIs(type(value), fixture.resolver.ResolvedChain)
        with self.assertRaises(TypeError):
            reader.read(self.repo, facts=value)


if __name__ == '__main__':
    unittest.main()
