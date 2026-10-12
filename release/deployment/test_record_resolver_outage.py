"""Actual resolver/source operations with controlled immutable Git captures."""
from dataclasses import FrozenInstanceError
import unittest

import record_resolver as resolver
import compat_outage_policy as loader
import test_ship_outage_compat as fixture


class OutageResolverTests(unittest.TestCase):
    def case(self): return fixture.outage_case(self)

    def resolve(self, case):
        case.serial += 1
        phase = case.root / ("outage-source-" + str(case.serial)); phase.mkdir()
        config, work = phase / "config", phase / "work"; config.mkdir(); work.mkdir()
        case.clock.now = 100.0
        self.budget = resolver.transport.OperationBudget(1000, capture=case.world, clock=case.clock)
        client = resolver.transport.ReleaseGh(case.gh, fixture.source.support.GH_TOKEN, config, work,
                                             budget=self.budget, base_env={})
        with case.installed():
            return resolver.resolve_outage_forward(client, case.repo, case.git, case.sha)

    def refuse(self, case):
        with self.assertRaises(resolver.transport.TransportRefusal): self.resolve(case)
        self.assertTrue(self.budget.failed); self.assertTrue(self.budget.closed)
        self.assertIsNone(resolver.records._GIT_OUTPUT_TRANSPORT.get())

    def test_full_p_h_roles_and_ancestry_captured_before_final_checks(self):
        case = self.case(); value = self.resolve(case)
        self.assertEqual(value.admission.resolution, case.expected)
        self.assertEqual(value.coverage.policy_raw, case.policy_raw)
        self.assertEqual(value.coverage.policy_blob.path, loader.DATA_PATH)
        self.assertEqual(value.coverage.ancestry, (resolver.PublisherAncestry(case.producer, case.sha, case.producer),))
        expected = [(sha, path, raw) for path, raw in sorted(case.inventory.items())
                    for sha in (case.producer, case.sha)]
        self.assertEqual([(b.source_sha, b.path, b.raw) for b in value.coverage.blobs], expected)
        self.assertTrue(self.budget.closed); self.assertFalse(self.budget.failed)
        with self.assertRaises(FrozenInstanceError): value.coverage.policy_raw = b"x"

    def test_nonancestor_and_absent_or_changed_h_blobs_remain_data(self):
        for change in ("ancestry", "missing", "empty", "changed"):
            with self.subTest(change=change):
                case = self.case()
                path = "release/compat/negative_index.py"
                if change == "ancestry": case.world.bad_pairs.add((case.producer, case.sha))
                elif change == "missing": del case.world.files[case.sha, path]
                elif change == "empty": case.world.files[case.sha, path] = b""
                else: case.world.files[case.sha, path] += b"# changed\n"
                result = self.resolve(case)
                self.assertIs(type(result), resolver.OutageForwardResolution)
                self.assertFalse(self.budget.failed)

    def test_installed_raw_policy_must_equal_exact_h_blob(self):
        case = self.case(); case.world.files[case.sha, loader.DATA_PATH] += b" "
        self.refuse(case)

    def test_missing_consumer_blob_or_unconfigured_file_refuses(self):
        for missing in ("git", "local"):
            case = self.case()
            if missing == "git": del case.world.files[case.sha, loader.DATA_PATH]
            else: case.policy_path.unlink()
            self.refuse(case)

    def test_missing_p_commit_refuses_without_fetch(self):
        case = self.case(); case.world.commits.remove(case.producer)
        self.refuse(case)
        self.assertFalse(any("fetch" in argv for argv, _ in case.world.trace))

    def test_exact_regular_mode_required_not_symlink_or_tree(self):
        for mode in (b"120000", b"160000", b"040000"):
            case = self.case(); original = case.world.git
            def git(args):
                result = original(args)
                if args[:2] == ["ls-tree", "-z"] and args[-1] == "release/compat/negative_index.py":
                    return resolver.transport.runtime.Capture(0, result.stdout.replace(b"100644", mode), b"")
                return result
            case.world.git = git
            self.refuse(case)

    def test_aggregate_raw_roles_not_deduplicated_for_source_limit(self):
        case = self.case()
        case.add_source("release/compat/opaque-source.bin", b"x" * (4 << 20))
        self.refuse(case)
        # The policy alone fits8MiB; P+H observations exceed the same aggregate.
        self.assertLess(sum(len(v) for v in case.inventory.values()), 8 << 20)

    def test_final_main_recheck_still_follows_all_coverage_reads(self):
        case = self.case(); switched = []
        def after(argv):
            if "cat-file" in argv and "blob" in argv and case.world.blobs.get(argv[-1]) == case.inventory["release/compat/negative_index.py"]:
                switched.append(1); case.world.main = "f" * 40
        case.world.after = after
        self.refuse(case)
        self.assertTrue(switched)

    def test_repeated_p_equal_h_retains_both_positional_roles(self):
        case = self.case(); case.producer = case.sha; case._policy()
        value = self.resolve(case)
        self.assertEqual(len(value.coverage.blobs), 2 * len(case.inventory))
        for i in range(0, len(value.coverage.blobs), 2):
            self.assertEqual(value.coverage.blobs[i], value.coverage.blobs[i + 1])


if __name__ == "__main__": unittest.main()
