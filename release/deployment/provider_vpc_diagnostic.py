#!/usr/bin/env python3
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import verify_production_plan as planner

app_id = os.environ["PROVIDER_VPC_APP_ID"]
default_ingress = os.environ["PROVIDER_VPC_DEFAULT_INGRESS"]
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

spec = app.get("spec")
if not isinstance(spec, dict):
    raise RuntimeError("app spec missing")

print(
    planner.canonical_file_bytes(
        {
            "vpc": spec.get("vpc"),
            "spec_top_level_keys": sorted(spec.keys()),
        }
    ).decode()
)
