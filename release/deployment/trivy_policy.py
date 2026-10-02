#!/usr/bin/env python3
"""Reviewed, expiring Trivy vulnerability exceptions for ship.yml.

Owner decision: fail every new HIGH or CRITICAL finding, except entries in
release/deployment/ship.trivyignore that a reviewed pull request added with a
reason and an expiry at most 90 days ahead. Entries use Trivy's own
``.trivyignore`` ``exp:`` syntax, so Trivy itself stops honouring an expired
entry; this checker additionally fails the run on any expired or malformed
entry until a reviewed PR removes or renews it. Secret findings can never be
excepted: the secret scan gets no ignore file. The file lives here, not at the
repository root, because test.yml:403-408 forbids a root ignore file.
"""

from __future__ import annotations

import datetime as dt
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common


POLICY_PATH = Path(__file__).resolve().with_name("ship.trivyignore")
MAX_BYTES = 16 * 1024
MAX_ENTRIES = 50
MAX_DAYS = 90
ENTRY_RE = re.compile(
    r"^(CVE-[0-9]{4}-[0-9]{4,7}|GHSA(?:-[23456789cfghjmpqrvwx]{4}){3}) exp:([0-9]{4}-[0-9]{2}-[0-9]{2})$"
)
REASON_RE = re.compile(r"^# reason: (.{10,200})$")


def check(path: Path, today: dt.date) -> int:
    """Validate the exception file and return the number of active entries."""
    code = "trivy-exception-invalid"
    if type(today) is not dt.date:
        common.fail("internal-error:trivy-today")
    if path.is_symlink() or not path.is_file():
        common.fail(code)
    raw = path.read_bytes()
    if len(raw) > MAX_BYTES or b"\r" in raw or raw.startswith(b"\xef\xbb\xbf"):
        common.fail(code)
    if raw and not raw.endswith(b"\n"):
        common.fail(code)
    try:
        text = raw.decode("ascii")
    except UnicodeError as exc:
        raise common.ReleaseError(code) from exc
    if any(ord(ch) < 0x20 and ch != "\n" for ch in text) or "\x7f" in text:
        common.fail(code)
    lines = text.split("\n")[:-1] if text else []
    seen: set[str] = set()
    comments: list[str] = []
    for line in lines:
        if line == "":
            comments = []
            continue
        if line.startswith("#"):
            comments.append(line)
            continue
        match = ENTRY_RE.fullmatch(line)
        if match is None:
            common.fail(code)
        identifier, expiry_raw = match.groups()
        if not any(REASON_RE.fullmatch(comment) for comment in comments):
            common.fail(code)
        try:
            expiry = dt.date.fromisoformat(expiry_raw)
        except ValueError as exc:
            raise common.ReleaseError(code) from exc
        if not today < expiry <= today + dt.timedelta(days=MAX_DAYS):
            common.fail(code)
        if identifier in seen:
            common.fail(code)
        seen.add(identifier)
        comments = []
        if len(seen) > MAX_ENTRIES:
            common.fail(code)
    return len(seen)
