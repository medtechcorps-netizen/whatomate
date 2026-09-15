from __future__ import annotations

import io
import json
import os
import unittest
from unittest import mock

try:
    from . import observe_crm_fixture_prestate as observer
except ImportError:  # pragma: no cover - direct module execution
    import observe_crm_fixture_prestate as observer


class TestObservePrestateRunner(unittest.TestCase):
    def test_runner_prints_one_content_free_json_line(self):
        observed = {"schema_version": 1, "kind": "crm-canary-fixture-provider-prestate",
                    "provider_prestate_sha256": "a" * 64,
                    "spec_sha256": "b" * 64, "app_updated_at_sha256": "c" * 64,
                    "active_deployment_sha256": "d" * 64,
                    "deployment_sha256": "e" * 64, "allowlist_members": 3}
        with mock.patch.object(observer.fixture, "observe_prestate",
                               lambda root: dict(observed)), \
                mock.patch.dict(os.environ, {"GITHUB_SHA": "f" * 40,
                                             "CONTROL_ROOT": "."}), \
                mock.patch("sys.stdout", new_callable=io.StringIO) as captured:
            self.assertEqual(observer.main(), 0)
            raw = captured.getvalue()
        self.assertTrue(raw.endswith("\n"))
        self.assertEqual(raw.count("\n"), 1)
        payload = json.loads(raw)
        self.assertEqual(payload["control_sha"], "f" * 40)
        self.assertEqual(payload["provider_prestate_sha256"], "a" * 64)
        self.assertEqual(sorted(payload), sorted(list(observed) + ["control_sha"]))


if __name__ == "__main__":
    unittest.main()
