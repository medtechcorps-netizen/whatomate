#!/usr/bin/env python3
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import verify_production_plan as planner

app_id = os.environ["PROVIDER_FINGERPRINT_APP_ID"]
default_ingress = os.environ["PROVIDER_FINGERPRINT_DEFAULT_INGRESS"]
token = os.environ["DO_PRODUCTION_FIXTURE_READ_TOKEN"]

contract = planner.load_json(
    ROOT / "release" / "deployment" / "production-app-contract.json",
    "production app contract",
)
target = {"app_id": app_id, "default_ingress": default_ingress}
client = planner.ProviderClient(contract, target, token)

app_response = client.get_json(client.app_path)
app = app_response.get("app")
if not isinstance(app, dict):
    raise RuntimeError("app response shape differs")

active_id = app["active_deployment"]["id"]
deployment_response = client.get_json(
    client.bind_active_deployment(target, active_id)
)
deployment = deployment_response.get("deployment")
if not isinstance(deployment, dict):
    raise RuntimeError("deployment response shape differs")

spec = app.get("spec")
if not isinstance(spec, dict):
    raise RuntimeError("app spec missing")

print(
    planner.canonical_file_bytes(
        {
            "canonical_spec_sha256": planner.sha256_value(spec),
            "environment_values_sha256": planner.environment_value_fingerprint(spec),
            "non_source_projection_sha256": planner.non_source_fingerprint(
                spec, contract
            ),
            "active_deployment_id_sha256": planner.sha256_bytes(
                active_id.encode("utf-8")
            ),
            "app_updated_at_sha256": planner.sha256_bytes(
                app["updated_at"].encode("utf-8")
            ),
        }
    ).decode()
)
