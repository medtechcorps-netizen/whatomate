from __future__ import annotations

import io
import json
import os
import unittest
from pathlib import Path
from unittest import mock

try:
    from . import observe_crm_fixture_prestate as observer
    from . import provision_production_crm_canary_fixture as fixture
    from . import verify_production_plan as planner
except ImportError:  # pragma: no cover - direct module execution
    import observe_crm_fixture_prestate as observer
    import provision_production_crm_canary_fixture as fixture
    import verify_production_plan as planner

common = fixture.common

APP_ID = "54bc0d92-73e9-4f8b-86cd-c3fd96e06beb"
INGRESS = "https://omnitech-3rilx.ondigitalocean.app"
ACTIVE_DEPLOYMENT = "fc2df9b6-64d5-45dd-ba72-e9756c49bb9a"
UPDATED_AT = "2026-09-15T13:39:40Z"


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

    def test_observation_hashes_the_raw_provider_timestamp_and_contract_state(self):
        app = {"app": {"id": APP_ID, "updated_at": UPDATED_AT,
                       "active_deployment": {"id": ACTIVE_DEPLOYMENT}}}
        deployment = {"deployment": {"id": ACTIVE_DEPLOYMENT}}
        state = {"active_deployment_identity_sha256": "a" * 64}
        spec = {"name": "rereply"}
        observed_position = {}

        class FakeProvider:
            """Minimal provider double; only the observation surface exists."""

            def __init__(self, root, authority, read_token, update_token):
                observed_position["authority"] = authority
                self.planner = planner
                self.contract = None
                self.target = authority["provider_target"]
                self.expected = None

            def current(self):
                return app, deployment, "/v2/apps/" + APP_ID

            def allowlist_control(self, value):
                observed_position["spec"] = value
                return {"value": "a,b,c"}

        environment = {
            "DO_PRODUCTION_FIXTURE_READ_TOKEN": "SyntheticReadToken123",
            "PROVIDER_TARGET_JSON": json.dumps({"app_id": APP_ID,
                                                "default_ingress": INGRESS}),
        }
        root = Path(__file__).resolve().parents[2]
        with mock.patch.dict(os.environ, environment), \
                mock.patch.object(fixture, "ProviderFixture", FakeProvider), \
                mock.patch.object(planner, "provider_state",
                                  lambda *args, **kwargs: (state, spec)):
            observed = fixture.observe_prestate(root)
        self.assertEqual(observed["provider_prestate_sha256"], common.sha256_value(state))
        self.assertEqual(observed["spec_sha256"], common.sha256_value(spec))
        self.assertEqual(observed["app_updated_at_sha256"],
                         common.sha256_bytes(UPDATED_AT.encode("utf-8")))
        self.assertEqual(observed["active_deployment_sha256"],
                         common.sha256_bytes(ACTIVE_DEPLOYMENT.encode("utf-8")))
        self.assertEqual(observed["allowlist_members"], 3)
        self.assertEqual(observed["kind"], "crm-canary-fixture-provider-prestate")
        self.assertIs(observed_position["spec"], spec)
        self.assertEqual(observed_position["authority"]["provider_target"],
                         {"app_id": APP_ID, "default_ingress": INGRESS})


if __name__ == "__main__":
    unittest.main()
