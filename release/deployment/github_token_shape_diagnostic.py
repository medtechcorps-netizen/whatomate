#!/usr/bin/env python3
import hashlib
import json
import os
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import verify_production_release as common

app_id = os.environ["GITHUB_TOKEN_SHAPE_APP_ID"]
token = os.environ["DO_PRODUCTION_FIXTURE_READ_TOKEN"]


def get(path: str) -> dict:
    req = urllib.request.Request(
        "https://api.digitalocean.com" + path,
        headers={"Authorization": "Bearer " + token, "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as response:
        return common.loads_strict(response.read().decode("utf-8"))


app = get("/v2/apps/" + app_id)["app"]
active = app["active_deployment"]["id"]
deployments = get(f"/v2/apps/{app_id}/deployments?page=1&per_page=200")
meta_total = deployments.get("meta", {}).get("total")
record = {
    "token_sha256": hashlib.sha256(token.encode("utf-8")).hexdigest(),
    "token_length": len(token),
    "app_active_deployment": active,
    "deployment_response_keys": sorted(deployments.keys()),
    "meta_total_type": type(meta_total).__name__,
    "meta_total_value": meta_total,
    "deployment_count": len(deployments.get("deployments", [])),
}
print(common.canonical_payload_bytes(record).decode("utf-8"))
