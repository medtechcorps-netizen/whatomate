#!/usr/bin/env python3
"""Interim schema and migration guard for ship.yml (judge fix i).

Steady-state migrations do not exist yet: on the future profile the
PRE_DEPLOY ``rls-migrate`` job is verify-only, so a schema, seed or backfill
change would pass PRE_DEPLOY and fail at runtime. Until Stage 2 replaces this
module with a catalog-diff classifier, any such change between two released
SHAs blocks the release. There are no waivers.

The diff is tree-based (``git diff base head``), so reverting an offending
change unblocks. Reasons name only public repository paths and rule names.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path
from typing import Iterable

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record
import ship_common as common


GUARDED_TREES = ("internal/database/", "internal/models/")
GUARDED_FILES = ("internal/handlers/chatbot_flow_migration.go",)
GUARDED_FUNCTIONS = (
    ("cmd/whatomate/main.go", "runRLSMigration"),
    ("cmd/whatomate/main.go", "verifyRLSMigrationRuntime"),
    ("cmd/whatomate/main.go", "validateServerMigrationMode"),
    ("cmd/whatomate/main.go", "requiredStartupDatabaseContract"),
    ("cmd/whatomate/main.go", "verifyStartupDatabaseContract"),
    ("internal/channel/legacy_meta.go", "BackfillLegacyWhatsAppInbox"),
)
SCANNED_ROOTS = ("cmd/", "internal/", "pkg/")
MIGRATION_CALL_PATTERNS = tuple(
    re.compile(pattern)
    for pattern in (
        r"\.AutoMigrate\(",
        r"\bRunMigrationWithProgress\w*\(",
        r"\bRunRLSMigrationCoordinator\w*\(",
        r"\bApplyTenantRLS\(",
        r"\bRemoveTenantRLS\(",
        r"\bSeedPermissionsAndRoles\(",
        r"\bFixSystemRolePermissions\(",
        r"\bCreateDefaultAdmin\(",
        r"\bCreateIndexes\(",
        r"\bBackfill[A-Z]\w*\(",
        r"\.Migrator\(\)\.(?:Create|Add|Alter|Drop|Rename)\w*\(",
        r"(?i)\b(?:Exec|Raw)\(\s*[`\"]\s*(?:create|alter|drop|truncate|grant|revoke|comment\s+on)\b",
    )
)


class SchemaChangeBlocked(common.ReleaseError):
    def __init__(self, reasons: Iterable[str]) -> None:
        super().__init__("schema-change-blocked")
        self.reasons = tuple(sorted(set(reasons)))


def _git(repo_dir: Path, args: list[str]) -> bytes:
    return release_record.git_output(repo_dir, ["-c", "core.quotepath=off", *args], code="git-failed:schema-guard")


def changed_paths(repo_dir: Path, base: str, head: str) -> list[tuple[str, str]]:
    raw = _git(repo_dir, ["diff", "--no-renames", "--name-status", "-z", base, head, "--"])
    fields = raw.split(b"\x00")
    if fields and fields[-1] == b"":
        fields.pop()
    if len(fields) % 2:
        common.fail("git-failed:schema-guard")
    output: list[tuple[str, str]] = []
    for index in range(0, len(fields), 2):
        status = fields[index].decode("ascii", "replace")
        try:
            path = fields[index + 1].decode("utf-8")
        except UnicodeError as exc:
            raise common.ReleaseError("git-failed:schema-guard") from exc
        if re.fullmatch(r"[ADMTUX]", status) is None:
            common.fail("git-failed:schema-guard")
        output.append((status, path))
    return output


def _blob(repo_dir: Path, revision: str, path: str) -> str | None:
    listing = _git(repo_dir, ["ls-tree", "-z", revision, "--", path]).split(b"\x00")
    entries = [item for item in listing if item]
    if not entries:
        return None
    meta, _, name = entries[0].partition(b"\t")
    if len(entries) != 1 or name.decode("utf-8", "replace") != path or meta.split(b" ")[1:2] != [b"blob"]:
        return None
    return _git(repo_dir, ["show", f"{revision}:{path}"]).decode("utf-8", "replace")


def function_text(source: str | None, name: str) -> tuple[str, ...]:
    """Every gofmt top-level definition from ``func <name>(`` to the next
    line that is exactly ``}``."""
    if source is None:
        return ()
    lines = source.split("\n")
    found: list[str] = []
    index = 0
    while index < len(lines):
        if lines[index].startswith(f"func {name}("):
            end = index
            while end < len(lines) and lines[end].rstrip("\r") != "}":
                end += 1
            found.append("\n".join(lines[index:end + 1]))
            index = end + 1
        else:
            index += 1
    return tuple(found)


def _added_lines(repo_dir: Path, base: str, head: str, path: str) -> list[str]:
    raw = _git(repo_dir, ["diff", "--no-renames", "-U0", base, head, "--", path]).decode("utf-8", "replace")
    return [
        line[1:]
        for line in raw.split("\n")
        if line.startswith("+") and not line.startswith("+++")
    ]


def check(repo_dir: Path, base: str, head: str) -> tuple[str, ...]:
    """Return the sorted public reasons (empty when the change is clean)."""
    common.require_sha1(base, "internal-error:schema-base")
    common.require_sha1(head, "internal-error:schema-head")
    if base == head:
        return ()
    release_record.require_commit(repo_dir, base, "git-failed:schema-guard")
    release_record.require_commit(repo_dir, head, "git-failed:schema-guard")
    changes = changed_paths(repo_dir, base, head)
    reasons: set[str] = set()
    changed = {path for _status, path in changes}
    flagged: set[str] = set()
    for _status, path in changes:
        if path.startswith(GUARDED_TREES) and not path.endswith("_test.go"):
            reasons.add(f"guarded-tree:{path}")
            flagged.add(path)
        if path in GUARDED_FILES:
            reasons.add(f"guarded-file:{path}")
            flagged.add(path)
    for path, name in GUARDED_FUNCTIONS:
        if path not in changed:
            continue
        if function_text(_blob(repo_dir, base, path), name) != function_text(_blob(repo_dir, head, path), name):
            reasons.add(f"guarded-function:{path}#{name}")
    for status, path in changes:
        if (
            status not in {"A", "M", "T"}
            or path in flagged
            or not path.endswith(".go")
            or path.endswith("_test.go")
            or not path.startswith(SCANNED_ROOTS)
        ):
            continue
        added = _added_lines(repo_dir, base, head, path)
        if any(pattern.search(line) for line in added for pattern in MIGRATION_CALL_PATTERNS):
            reasons.add(f"migration-call:{path}")
    return tuple(sorted(reasons))


def guard(repo_dir: Path, base: str, head: str) -> None:
    reasons = check(repo_dir, base, head)
    if reasons:
        raise SchemaChangeBlocked(reasons)
