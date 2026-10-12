"""Fixed consumer-file boundary; all setup is synthetic and uninstalled."""
from dataclasses import replace
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import compat_outage_policy as loader
import test_compat_negative_index as fixture


class OutagePolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.path = self.root / "consumer.json"
        self.patch = mock.patch.object(loader, "_POLICY_PATH", self.path)
        self.patch.start(); self.addCleanup(self.patch.stop)
        self.raw = fixture.wire(fixture.consumer_dict())

    def test_absent_is_explicit_none_and_required_loader_refuses(self):
        self.assertIsNone(loader.load_consumer_policy())
        with self.assertRaisesRegex(loader.policy.NegativeIndexRefusal, "negative-index-unconfigured"):
            loader.require_consumer_policy()
        self.assertFalse(self.path.exists())

    def test_real_canonical_file_is_reparsed_with_exact_raw_witness(self):
        self.path.write_bytes(self.raw)
        value = loader.require_consumer_policy()
        self.assertEqual(value.raw, self.raw)
        self.assertEqual(value.setup.raw, fixture.setup_bytes())
        self.assertEqual(value, loader.policy.parse_consumer_policy(self.raw))

    def test_present_malformed_empty_oversize_and_noncanonical_refuse(self):
        for raw in (b"", b"null\n", self.raw + b" ", b"x" * (loader.policy.MAX_CONFIG + 1)):
            with self.subTest(size=len(raw)):
                self.path.write_bytes(raw)
                with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()

    def test_consumer_data_cannot_appear_in_its_own_lineage_inventory(self):
        value = fixture.consumer_dict()
        value["lineage"][0]["inventory"][1]["path"] = loader.DATA_PATH
        self.path.write_bytes(fixture.wire(value))
        with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()

    def test_regular_hardlinked_file_is_refused(self):
        self.path.write_bytes(self.raw)
        os.link(self.path, self.root / "alias")
        with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()

    def test_directory_at_fixed_file_path_is_not_absent(self):
        self.path.mkdir()
        with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()

    def test_actual_file_mutation_between_handle_checks_refuses(self):
        self.path.write_bytes(self.raw)
        original, calls = os.fstat, []
        def changed(fd):
            calls.append(fd)
            if len(calls) == 2: self.path.write_bytes(self.raw + b" ")
            return original(fd)
        with mock.patch.object(loader.os, "fstat", changed):
            with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()
        self.assertEqual(len(calls), 2)

    def test_parent_link_or_junction_is_refused_before_file_read(self):
        self.path.write_bytes(self.raw)
        original = Path.is_symlink
        with mock.patch.object(Path, "is_symlink", lambda p: p == self.root or original(p)):
            with self.assertRaises(loader.policy.NegativeIndexRefusal): loader.load_consumer_policy()

    def test_birthtime_normalizes_cross_api_only_and_missing_birthtime_is_exact(self):
        left = (1, 2, 33206, 50, 100, 10, 1, 10)
        self.assertTrue(loader._cross(left, (*left[:5], 20, *left[6:])))
        for slot in (0, 1, 2, 3, 4, 6, 7):
            changed = list(left); changed[slot] += 1
            self.assertFalse(loader._cross(left, tuple(changed)))
        self.assertFalse(loader._cross((*left[:7], None), (*left[:5], 20, 1, None)))

    def test_environment_and_old_installed_constant_cannot_supply_consumer_data(self):
        with mock.patch.dict(os.environ, {"COMPAT_CONSUMER_POLICY": str(self.path)}), \
             mock.patch.object(loader.policy, "_INSTALLED_CONSUMER_POLICY", self.raw):
            self.assertIsNone(loader.load_consumer_policy())


if __name__ == "__main__": unittest.main()
