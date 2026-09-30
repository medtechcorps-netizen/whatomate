"""The canary binding step must reach the pinned verifier through a real reader."""

from __future__ import annotations

import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

try:
    from . import verify_crm_canary_fixture_binding as binding
    from . import test_fixture_producer_compatibility as compatibility
except ImportError:
    import verify_crm_canary_fixture_binding as binding
    import test_fixture_producer_compatibility as compatibility

fixture = binding.fixture
common = fixture.common
ORIGIN = "https://api.github.com"


class WiredReceipt:
    """Serves the synthetic receipt only below the real GitHubRead route guard."""

    def __init__(self):
        self.api = compatibility.ReceiptAPI()
        self.urls: list[str] = []

    def wire(self, _opener, url, **_kwargs):
        self.urls.append(url)
        assert url.startswith(ORIGIN + fixture.API_PREFIX + "/")
        path = url[len(ORIGIN):]
        if "per_page=100&page=1" in path:
            key = "jobs" if "/jobs" in path else "artifacts"
            page = copy.deepcopy(self.api.pages(path, key))
            for index, row in enumerate(page[key], 1):
                row.setdefault("id", index)
            return common.canonical_payload_bytes(page)
        return common.canonical_payload_bytes(self.api.get(path))


class CanaryFixtureBindingTests(unittest.TestCase):
    def run_consumer(self, reader_type):
        wired = WiredReceipt()
        reader = reader_type("synthetic-reader-token")
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(fixture.os.environ, {
                "FIXTURE_EVIDENCE_JSON": json.dumps(wired.api.descriptor),
                "CONTROL_SHA": wired.api.current_control,
                "CRM_CANARY_SYNTHETIC_DRIVER_JSON": json.dumps(wired.api.driver),
                "RUNNER_TEMP": directory}), \
                mock.patch.object(fixture, "_wire", side_effect=wired.wire), \
                mock.patch.object(reader, "artifact", side_effect=wired.api.artifact), \
                mock.patch.object(fixture.time, "sleep"), \
                mock.patch.object(fixture, "_verify_intent_attestations"):
            return fixture.verify_fixture_result(reader, Path(directory), Path("never-run-gh")), wired

    def test_pinned_reader_rejects_the_comparison_separator(self):
        # Regression oracle for canary run 36774347298.
        with self.assertRaisesRegex(common.ReleaseError, "GitHub route differs"):
            self.run_consumer(fixture.GitHubRead)

    def test_binding_reader_reaches_every_existing_verifier_check(self):
        result, wired = self.run_consumer(binding.ComparisonGitHubRead)
        self.assertEqual(result, wired.api.result)
        self.assertEqual(sum("/compare/" in url for url in wired.urls), 1)

    def test_only_the_exact_comparison_route_is_widened(self):
        reader = binding.ComparisonGitHubRead("synthetic-reader-token")
        prefix = fixture.API_PREFIX
        for path in (prefix + "/compare/" + "a" * 40 + ".." + "b" * 40,
                     prefix + "/compare/" + "a" * 40 + "...main",
                     prefix + "/compare/" + "A" * 40 + "..." + "b" * 40,
                     prefix + "/compare/" + "a" * 40 + "..." + "b" * 40 + "?x=1",
                     prefix + "/compare/" + "a" * 40 + "..." + "b" * 40 + "/../contents",
                     prefix + "/contents/../secret",
                     "/repos/other/repo/compare/" + "a" * 40 + "..." + "b" * 40):
            with self.subTest(path=path), mock.patch.object(fixture, "_wire") as wire, \
                    self.assertRaises(common.ReleaseError):
                reader.get(path)
            wire.assert_not_called()

    def test_main_runs_only_the_existing_guard_tool_and_verifier(self):
        calls = []
        with mock.patch.dict(fixture.os.environ, {"GH_TOKEN": "synthetic-reader-token"}), \
                mock.patch.object(fixture, "_current_guard", side_effect=lambda api, root, workflow: calls.append(("guard", type(api), workflow))), \
                mock.patch.object(fixture, "_pinned_gh", side_effect=lambda: calls.append(("gh",)) or Path("gh")), \
                mock.patch.object(fixture, "verify_fixture_result", side_effect=lambda api, root, gh: calls.append(("verify", type(api)))):
            self.assertEqual(binding.main(["verify-fixture-result", "--control-root", "."]), 0)
        self.assertEqual(calls, [("guard", binding.ComparisonGitHubRead, binding.CANARY_WORKFLOW), ("gh",),
                                 ("verify", binding.ComparisonGitHubRead)])
        for command in ("execute", "claim-test", "prepare-intent", "reconcile", "acquire-origin"):
            with self.subTest(command=command), self.assertRaises(SystemExit):
                binding.main([command, "--control-root", "."])


if __name__ == "__main__":
    unittest.main()
