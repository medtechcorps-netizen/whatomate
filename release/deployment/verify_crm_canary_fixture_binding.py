#!/usr/bin/env python3
"""Canary-side runner for the pinned fixture verifier's verify-fixture-result.

The reviewed fixture producer must stay byte-identical outside its compatibility
helper, and its generic '..' route rejection also rejects GitHub's valid
SHA...SHA comparison separator. This module admits only that exact comparison
route and otherwise delegates every guard, read, and verification to the
unmodified producer. It exposes no other producer command.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from pathlib import Path
from typing import Any

try:
    from . import provision_production_crm_canary_fixture as fixture
except ImportError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import provision_production_crm_canary_fixture as fixture

common = fixture.common
CANARY_WORKFLOW = ".github/workflows/verify-production-crm-canary.yml"
COMPARISON = re.compile(re.escape(fixture.API_PREFIX) + r"/compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40}")


class ComparisonGitHubRead(fixture.GitHubRead):
    """Permit only the exact SHA comparison syntax the producer rejects."""

    def __init__(self, token: str):
        super().__init__(token)
        self.__comparison_token = fixture._secret(token)

    def get(self, path: str) -> Any:
        if type(path) is str and COMPARISON.fullmatch(path) is not None:
            return common.loads_strict(fixture._wire(
                self.opener, "https://api.github.com" + path,
                headers={"Authorization": "Bearer " + self.__comparison_token,
                         "Accept": "application/vnd.github+json",
                         "X-GitHub-Api-Version": "2022-11-28"},
                maximum=fixture.MAX_DOWNLOAD))
        return super().get(path)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Bind the UI canary runtime fixture to its signed result")
    parser.add_argument("command", choices=["verify-fixture-result"])
    parser.add_argument("--control-root", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        # The imported producer must be the sibling file in this exact checkout.
        fixture._require(Path(fixture.__file__).resolve()
                         == Path(__file__).resolve().parent / "provision_production_crm_canary_fixture.py",
                         "fixture producer location differs")
        root = args.control_root.resolve(strict=True)
        api = ComparisonGitHubRead(os.environ["GH_TOKEN"])
        fixture._current_guard(api, root, workflow=CANARY_WORKFLOW)
        gh = fixture._pinned_gh()
        fixture.verify_fixture_result(api, root, gh)
        return 0
    except Exception as exc:
        print(f"fixture control stopped; no repeat execution is authorized: {type(exc).__name__}: {exc}",
              file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
