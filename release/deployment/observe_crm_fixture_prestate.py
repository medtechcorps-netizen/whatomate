#!/usr/bin/env python3
"""Read-only provider prestate observation for one CRM canary fixture control.

The execution authority pins the DigitalOcean prestate, and the provider moves
incidentally (``app.updated_at`` advances without an app event), so a fresh
authority must be built from a fresh observation. This runner performs only the
contract-pinned GETs with the fixture read token: no claim, no burn, no
authority, no write, and no Meta call. Output is one canonical JSON line of
content-free facts.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release" / "deployment"))

import provision_production_crm_canary_fixture as fixture


def main(argv: list[str] | None = None) -> int:
    root = Path(os.environ.get("CONTROL_ROOT", "control")).resolve(strict=True)
    observed = fixture.observe_prestate(root)
    observed["control_sha"] = os.environ["GITHUB_SHA"]
    print(json.dumps(observed, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
