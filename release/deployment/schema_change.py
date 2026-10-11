#!/usr/bin/env python3
"""Read-only release classifier. Catalog application is not implemented here.

Pre-catalog records retain the original schema freeze. Catalog-bearing records
can carry dormant code/data changes, while migration entrypoints and active
golden changes remain blocked. Registry CI generates and verifies declaration
closure hashes; this module verifies their history/revision contract.
"""

from __future__ import annotations

import argparse
import difflib
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any, Iterable, NamedTuple

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_record
import ship_common as common
import pending_plan
import schema_nullable
import schema_index
import schema_relation
import schema_tenant_profile


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
MIGRATION_FUNCTIONS = (
    ("internal/database/postgres.go", "RunRLSMigrationCoordinator"),
    ("internal/database/postgres.go", "runRLSMigrationCoordinatorForPhase"),
    ("internal/database/postgres.go", "withMigrationSession"),
    ("internal/database/postgres.go", "decideRLSMigrationAction"),
    ("internal/database/postgres.go", "classifyLegacyRLSCatalog"),
    ("internal/database/postgres.go", "verifyFutureRLSPostcondition"),
    ("internal/database/platform_compliance.go", "detectPlatformComplianceIdentityReviewTriggerProfile"),
    ("internal/database/tenant.go", "VerifyTenantRLS"),
)
GUARDED_DECLS = (("internal/database/postgres.go", "compiledRLSMigrationPhase"),)
GOLDEN = "internal/dbcatalog/golden/"
CATALOG = GOLDEN + "catalog.json"
HISTORY = GOLDEN + "history.json"
GLOBALS = GOLDEN + "global_tables.json"
SEEDS = GOLDEN + "seeds.json"
REGISTRY = "internal/dbcatalog/registry/data_steps.json"
MIGRATION_PATH = "internal/dbcatalog/registry/migration_path.json"
MIGRATION_ROOTS = [
    "github.com/shridarpatil/whatomate/cmd/whatomate.requiredStartupDatabaseContract",
    "github.com/shridarpatil/whatomate/cmd/whatomate.runRLSMigration",
    "github.com/shridarpatil/whatomate/cmd/whatomate.validateServerMigrationMode",
    "github.com/shridarpatil/whatomate/cmd/whatomate.verifyRLSMigrationRuntime",
    "github.com/shridarpatil/whatomate/cmd/whatomate.verifyStartupDatabaseContract",
    "github.com/shridarpatil/whatomate/internal/database.compiledRLSMigrationPhase",
]
DORMANT = "internal/dbcatalog/dormant_entrypoints.json"
# Actual PR3 synthetic bootstrap artifacts, not merely a version label.
BASELINE_V0_SHA256 = {
    CATALOG: "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8",
    HISTORY: "9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513",
    GLOBALS: "c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3",
}
MAX_ARTIFACT_BYTES = 4 * 1024 * 1024
MAX_HISTORY_COMMITS = 2048
PUBLIC_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_./-]{0,255}\Z")
GO_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")


class Classification(NamedTuple):
    kind: str
    dormant: bool
    reasons: tuple[str, ...]
    summary: tuple[str, ...]
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


def _legacy_check(repo_dir: Path, base: str, head: str) -> tuple[str, ...]:
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


class _Invalid(ValueError):
    pass


def _valid(ok: bool) -> None:
    if not ok:
        raise _Invalid()


def _integer(value: Any, minimum: int = 0) -> bool:
    return type(value) is int and minimum <= value <= 2**31 - 1


def _path(value: Any) -> bool:
    return (type(value) is str and PUBLIC_NAME.fullmatch(value) is not None
            and all(part not in {"", ".", ".."} for part in value.split("/")))


def _changed_path(value: Any) -> bool:
    # Changed files include repository metadata such as .github/workflows.
    # Registry identifiers retain their separate, narrower _path contract.
    return (type(value) is str
            and re.fullmatch(r"[A-Za-z_.][A-Za-z0-9_./-]{0,511}", value) is not None
            and all(part.casefold() not in {"", ".", "..", ".git"}
                    for part in value.split("/")))


class _Repository:
    """Bounded artifact reads, cached only for this classifier invocation."""

    def __init__(self, root: Path):
        self.root = root
        self.cache: dict[tuple[str, str], bytes | None] = {}
        self.total = 0

    def blob(self, revision: str, path: str) -> bytes | None:
        key = (revision, path)
        if key in self.cache:
            return self.cache[key]
        raw = _git(self.root, ["ls-tree", "-z", revision, "--", path])
        rows = [row for row in raw.split(b"\0") if row]
        value = None
        if rows:
            _valid(len(rows) == 1)
            meta, sep, name = rows[0].partition(b"\t")
            parts = meta.split()
            _valid(bool(sep) and name == path.encode() and len(parts) == 3
                   and parts[0] in {b"100644", b"100755"} and parts[1] == b"blob")
            size = _git(self.root, ["cat-file", "-s", parts[2].decode("ascii")]).strip()
            _valid(size.isdigit() and int(size) <= MAX_ARTIFACT_BYTES)
            self.total += int(size)
            _valid(self.total <= 64 * 1024 * 1024)
            value = _git(self.root, ["cat-file", "blob", parts[2].decode("ascii")])
            _valid(len(value) == int(size))
        self.cache[key] = value
        return value

    def text(self, revision: str, path: str) -> str | None:
        raw = self.blob(revision, path)
        if raw is None:
            return None
        try:
            return raw.decode("utf-8", "strict")
        except UnicodeError as exc:
            raise _Invalid() from exc

    def document(self, revision: str, path: str, validator, *, optional: bool = False):
        raw = self.blob(revision, path)
        if raw is None:
            _valid(optional)
            return None
        try:
            value = common.loads_strict(raw)
        except common.ReleaseError as exc:
            raise _Invalid() from exc
        validator(value)
        return value


def _catalog(value: Any) -> None:
    _valid(type(value) is dict and set(value) == {"version", "objects"}
           and _integer(value["version"]) and type(value["objects"]) is list
           and len(value["objects"]) <= 20000)
    seen = set()
    for row in value["objects"]:
        _valid(type(row) is dict and {"kind", "identity", "attributes"} <= set(row)
               and set(row) <= {"kind", "identity", "parent", "attributes"})
        _valid(row["kind"] in {"relations", "columns", "constraints", "indexes", "triggers",
                               "functions", "policies", "sequences", "types", "extensions"})
        _valid(all(type(row.get(k, "")) is str and len(row.get(k, "")) <= 2048 for k in ("identity", "parent")))
        identity = (row["kind"], row["identity"])
        _valid(identity not in seen)
        seen.add(identity)
        _valid(type(row["attributes"]) is dict and len(row["attributes"]) <= 64
               and all(type(k) is str and type(v) is str and len(k) <= 128 and len(v) <= 16384
                       for k, v in row["attributes"].items()))


def _history(value: Any) -> None:
    _valid(type(value) is list and 1 <= len(value) <= 1024)
    prior = -1
    for row in value:
        _valid(type(row) is dict and {"version", "class", "min_reader_version"} <= set(row)
               and set(row) <= {"version", "class", "min_reader_version", "apply_engine"})
        _valid(_integer(row["version"]) and row["version"] > prior
               and _integer(row["min_reader_version"]) and row["min_reader_version"] <= row["version"]
               and row["class"] in {"baseline", "expand", "contract"}
               and type(row.get("apply_engine", False)) is bool)
        prior = row["version"]
    _valid(value[0]["version"] == 0 and value[0]["class"] == "baseline"
           and value[0]["min_reader_version"] == 0
           and all(row["class"] != "baseline" for row in value[1:]))


def _globals(value: Any) -> None:
    _valid(type(value) is list and len(value) <= 1024)
    names = set()
    for row in value:
        _valid(type(row) is dict and {"name", "reason"} <= set(row)
               and set(row) <= {"name", "reason", "keys"})
        _valid(type(row["name"]) is str and GO_NAME.fullmatch(row["name"]) is not None
               and row["name"] not in names and type(row["reason"]) is str
               and 0 < len(row["reason"]) <= 2048)
        names.add(row["name"])
        keys = row.get("keys", [])
        _valid(type(keys) is list and len(keys) <= 16 and len(set(keys)) == len(keys)
               and all(type(k) is str and GO_NAME.fullmatch(k) is not None for k in keys))


def _registry(value: Any) -> None:
    _valid(type(value) is list and len(value) <= 512)
    seen = set()
    fields = {"id", "rev", "func", "kind", "baseline", "idempotent", "n1_safe",
              "requires_gates", "closure_files", "closure_sha256"}
    for row in value:
        _valid(type(row) is dict and fields <= set(row) and set(row) <= fields | {"parent"})
        _valid(type(row["id"]) is str and PUBLIC_NAME.fullmatch(row["id"]) is not None
               and row["id"] not in seen and _integer(row["rev"], 1)
               and type(row["func"]) is str and PUBLIC_NAME.fullmatch(row["func"]) is not None
               and row["kind"] in {"seed-reconciler", "baseline-pre-ledger"})
        seen.add(row["id"])
        _valid("parent" not in row or (type(row["parent"]) is str
               and PUBLIC_NAME.fullmatch(row["parent"]) is not None and row["parent"] != row["id"]))
        _valid(all(type(row[k]) is bool for k in ("baseline", "idempotent", "n1_safe")))
        for field, cap in (("requires_gates", 32), ("closure_files", 512)):
            items = row[field]
            _valid(type(items) is list and len(items) <= cap and all(_path(p) for p in items))
            _valid(items == sorted(set(items)))
        _valid(bool(row["closure_files"]) and all(p.endswith(".go") and not p.endswith("_test.go")
                                                  for p in row["closure_files"]))
        _valid(type(row["closure_sha256"]) is str
               and re.fullmatch(r"[0-9a-f]{64}", row["closure_sha256"]) is not None)


def _seeds(value: Any) -> None:
    _valid(type(value) is dict and _integer(value.get("version")) and len(value) <= 64)
    # Only bounded, escaped lines passing the public-output filter are emitted.
    stack = [(value, 0)]
    count = 0
    while stack:
        item, depth = stack.pop()
        count += 1
        _valid(count <= 100000 and depth <= 32)
        if type(item) is dict:
            _valid(all(type(k) is str and len(k) <= 256 for k in item))
            stack.extend((v, depth + 1) for v in item.values())
        elif type(item) is list:
            stack.extend((v, depth + 1) for v in item)
        else:
            _valid(item is None or type(item) in {str, bool, int})
            _valid(type(item) is not str or len(item) <= 16384)


def _go_code(source: str) -> str:
    """Blank comments/literals without changing offsets, for declarations/calls."""
    pattern = r'//[^\n]*|/\*[\s\S]*?\*/|`[^`]*`|"(?:\\[\s\S]|[^"\\])*"|\'(?:\\[\s\S]|[^\'\\])*\''
    return re.sub(pattern, lambda m: re.sub(r"[^\n]", " ", m.group()), source)


def _functions(source: str, name: str) -> list[tuple[int, int, int]]:
    code = _go_code(source)
    spans = []
    for match in re.finditer(r"(?m)^func\s+(" + re.escape(name) + r")\s*\(", code):
        parameter_end, depth = match.end(), 1
        while parameter_end < len(code) and depth:
            depth += (code[parameter_end] == "(") - (code[parameter_end] == ")")
            parameter_end += 1
        _valid(depth == 0)
        opening = code.find("{", parameter_end)
        _valid(opening >= 0)
        _valid(re.search(r"\b(?:struct|interface|func)\b", code[parameter_end:opening]) is None)
        level = 1
        end = opening + 1
        while end < len(code) and level:
            level += (code[end] == "{") - (code[end] == "}")
            end += 1
        _valid(level == 0)
        spans.append((match.start(), end, match.start(1)))
    return spans


def _function_bytes(source: str | None, name: str) -> tuple[str, ...]:
    return tuple(source[a:b] for a, b, _ in _functions(source, name)) if source is not None else ()


def _constant_bytes(source: str | None, name: str) -> tuple[str, ...]:
    if source is None:
        return ()
    code = _go_code(source)
    found = []
    # Guard the whole containing const declaration, including inherited iota.
    for match in re.finditer(r"(?m)^const\b", code):
        start = match.end()
        while start < len(code) and code[start].isspace():
            start += 1
        if start < len(code) and code[start] == "(":
            end = code.find("\n)", start)
            _valid(end >= 0)
            end += 2
        else:
            end = code.find("\n", start)
            if end < 0:
                end = len(code)
        if re.search(r"\b" + re.escape(name) + r"\b", code[start:end]):
            found.append(source[match.start():end])
    return tuple(found)


def _imports(source: str) -> tuple[dict[str, str], list[tuple[int, int]]]:
    """Supported gofmt import grammar; ambiguity is a refusal, not a guess."""
    code = _go_code(source)
    bindings: dict[str, str] = {}
    spans = []
    for match in re.finditer(r"(?m)^import\b", code):
        start = match.end()
        while start < len(source) and source[start] in " \t":
            start += 1
        if start < len(code) and code[start] == "(":
            end = code.find("\n)", start)
            _valid(end >= 0)
            end += 2
            body = source[start + 1:end - 1]
        else:
            end = code.find("\n", start)
            if end < 0:
                end = len(code)
            body = source[start:end]
        spans.append((match.start(), end))
        for line in body.splitlines():
            if not _go_code(line).strip() and not line.lstrip().startswith('"'):
                continue
            item = re.fullmatch(r'\s*(?:([A-Za-z_][A-Za-z0-9_]*|\.)\s+)?"([A-Za-z0-9_./-]+)"\s*(?://[^\n]*)?', line)
            _valid(item is not None)
            alias = item[1] or item[2].rsplit("/", 1)[-1]
            _valid(alias not in bindings)
            bindings[alias] = item[2]
    return bindings, spans


def _direct_call_identity(source: str, position: int, file: str, defining_file: str, package: str) -> None:
    code = _go_code(source)
    prefix = code[:position].rstrip()
    if not prefix.endswith("."):
        packages = re.findall(r"(?m)^package\s+([A-Za-z_][A-Za-z0-9_]*)\b", code)
        _valid(Path(file).parent == Path(defining_file).parent and packages == [package])
        return
    receiver = re.search(r"([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*$", prefix)
    _valid(receiver is not None and not prefix[:receiver.start()].rstrip().endswith("."))
    alias = receiver[1]
    imports, import_spans = _imports(source)
    expected = "github.com/shridarpatil/whatomate/" + str(Path(defining_file).parent).replace("\\", "/")
    _valid(imports.get(alias) == expected)
    # Every non-import alias occurrence must be a package selector. Parameters,
    # local variables, receiver names and function-value escapes are ambiguous.
    for use in re.finditer(r"\b" + re.escape(alias) + r"\b", code):
        if any(start <= use.start() < end for start, end in import_spans):
            continue
        _valid(re.match(r"\s*\.", code[use.end():]) is not None)


def _dormant(repo: _Repository, head: str) -> dict[str, list[tuple[int, int]]]:
    def validate(value):
        _valid(type(value) is list and len(value) <= 32)
        seen = set()
        for row in value:
            _valid(type(row) is dict and set(row) == {"file", "func"})
            _valid(_path(row["file"]) and row["file"].endswith(".go")
                   and not row["file"].endswith("_test.go") and row["file"].startswith(SCANNED_ROOTS)
                   and type(row["func"]) is str and GO_NAME.fullmatch(row["func"]) is not None)
            pair = (row["file"], row["func"])
            _valid(pair not in seen)
            seen.add(pair)
    entries = repo.document(head, DORMANT, validate, optional=True) or []
    if not entries:
        return {}
    paths = _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", head, "--", *SCANNED_ROOTS]).split(b"\0")
    _valid(len(paths) <= 10000)
    sources = {}
    for raw in paths:
        path = raw.decode("utf-8", "strict")
        if path.endswith(".go") and not path.endswith("_test.go"):
            sources[path] = repo.text(head, path) or ""
    output: dict[str, list[tuple[int, int]]] = {}
    for row in entries:
        path, name = row["file"], row["func"]
        source = sources.get(path, "")
        spans = _functions(source, name)
        _valid(len(spans) == 1)
        packages = re.findall(r"(?m)^package\s+([A-Za-z_][A-Za-z0-9_]*)\b", _go_code(source))
        _valid(len(packages) == 1)
        calls = 0
        for other, text in sources.items():
            code = _go_code(text)
            for match in re.finditer(r"\b" + re.escape(name) + r"\b", code):
                if other == path and match.start() == spans[0][2]:
                    continue
                _valid(re.match(r"\s*\(", code[match.end():]) is not None)
                # Recursive references do not establish a dormant external entry.
                _valid(not (other == path and spans[0][0] <= match.start() < spans[0][1]))
                declarations = re.finditer(r"\bfunc\s+(?:\([^)]*\)\s*)?(" + re.escape(name) + r")\s*\(", code)
                _valid(not any(match.start() == declaration.start(1) for declaration in declarations))
                _direct_call_identity(text, match.start(), other, path, packages[0])
                calls += 1
        _valid(calls == 1)
        start, end, _ = spans[0]
        output.setdefault(path, []).append((start, end))
    return output


def _added_numbered(repo_dir: Path, base: str, head: str, path: str):
    raw = _git(repo_dir, ["diff", "--no-renames", "-U0", base, head, "--", path]).decode("utf-8", "strict")
    number = 0
    for line in raw.splitlines():
        match = re.match(r"@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,[0-9]+)? @@", line)
        if match:
            number = int(match[1])
        elif line.startswith("+") and not line.startswith("+++"):
            yield number, line[1:]
            number += 1
        elif line.startswith(" "):
            number += 1


def _registry_changes(repo: _Repository, base: str, head: str, reasons: set[str], summary: list[str]) -> bool:
    old = repo.document(base, REGISTRY, _registry, optional=True)
    new = repo.document(head, REGISTRY, _registry, optional=True)
    commits = _git(repo.root, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"]).splitlines()
    _valid(len(commits) <= MAX_HISTORY_COMMITS)
    introduced = old is not None
    prior = {row["id"]: row for row in old or []}
    for raw in commits:
        revision = raw.decode("ascii")
        rows = repo.document(revision, REGISTRY, _registry, optional=True)
        if rows is not None and not introduced:
            introduced, old = True, rows
            summary.append("registry introduction: first-parent anchor")
            for row in rows:
                if row["kind"] == "seed-reconciler":
                    for path in row["closure_files"]:
                        before = repo.blob(base, path)
                        if before is None or before != repo.blob(revision, path):
                            reasons.add("registry-introduction-changed:" + path)
        elif introduced and rows is None:
            reasons.add("data-step-removed:registry")
        current = {row["id"]: row for row in rows or []}
        # Keep all earlier identities even across a deletion, so a reintroduced
        # row cannot reset its revision or kind. Endpoint hashes still decide
        # the final closure/revision change; no intermediate deployment implied.
        for name, before in prior.items():
            after = current.get(name)
            if after is None:
                reasons.add("data-step-removed:" + name)
            else:
                _valid(after["kind"] == before["kind"] and after["baseline"] == before["baseline"])
                if after["rev"] < before["rev"]:
                    reasons.add("data-step-invalid-rev:" + name)
        prior.update(current)
    if not introduced:
        return False
    old_by_id = {row["id"]: row for row in old or []}
    new_by_id = {row["id"]: row for row in new or []}
    data = False
    for name in sorted(old_by_id.keys() | new_by_id.keys()):
        before, after = old_by_id.get(name), new_by_id.get(name)
        if after is None:
            reasons.add("data-step-removed:" + name)
        elif before is None:
            data = True
            summary.append("data step added (dormant): " + name)
        elif after != before:
            _valid(after["kind"] == before["kind"] and after["baseline"] == before["baseline"])
            if after["rev"] < before["rev"]:
                reasons.add("data-step-invalid-rev:" + name)
            changed_closure = any(after[k] != before[k] for k in (
                "func", "closure_files", "closure_sha256", "idempotent", "n1_safe", "requires_gates"))
            changed_closure = changed_closure or after.get("parent") != before.get("parent")
            if before["kind"] == "seed-reconciler":
                if changed_closure and after["rev"] <= before["rev"]:
                    reasons.add("data-step-edited-without-rev:" + name)
                if changed_closure or after["rev"] > before["rev"]:
                    data = True
                    summary.append("data step changed (dormant): " + name)
            else:
                summary.append("baseline-pre-ledger changed (bootstrap-only): " + name)
    return data


def _migration_contract(repo: _Repository, base: str, head: str, reasons: set[str], summary: list[str]) -> bool:
    def validate(row):
        _valid(type(row) is dict and set(row) == {"version", "roots", "closure_files", "closure_sha256"})
        _valid(type(row["version"]) is int and row["version"] == 1 and row["roots"] == MIGRATION_ROOTS)
        files = row["closure_files"]
        _valid(type(files) is list and 1 <= len(files) <= 512
               and all(_path(p) and p.endswith(".go") and not p.endswith("_test.go") for p in files)
               and files == sorted(set(files)))
        _valid(type(row["closure_sha256"]) is str
               and re.fullmatch(r"[0-9a-f]{64}", row["closure_sha256"]) is not None)
    old = repo.document(base, MIGRATION_PATH, validate, optional=True)
    new = repo.document(head, MIGRATION_PATH, validate, optional=True)
    commits = _git(repo.root, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"]).splitlines()
    _valid(len(commits) <= MAX_HISTORY_COMMITS)
    introduced = old is not None
    for raw in commits:
        revision = raw.decode("ascii")
        row = repo.document(revision, MIGRATION_PATH, validate, optional=True)
        if row is not None and not introduced:
            introduced, old = True, row
            summary.append("migration closure introduction: first-parent anchor")
            for path in row["closure_files"]:
                before = repo.blob(base, path)
                if before is None or before != repo.blob(revision, path):
                    reasons.add("migration-path-changed:" + path)
        elif introduced and row is None:
            reasons.add("migration-path-changed:contract-removed")
    if not introduced:
        summary.append("migration closure absent: database/models freeze retained")
        return False
    if new is None or old != new:
        reasons.add("migration-path-changed:closure")
    return new is not None and old is not None


def classify(repo_dir: Path, base: str, head: str, *, mode: str = "forward") -> Classification:
    return _classify(_Repository(repo_dir), base, head, mode=mode)


def _classify(repo: _Repository, base: str, head: str, *, mode: str = "forward") -> Classification:
    repo_dir = repo.root
    common.require_sha1(base, "internal-error:schema-base")
    common.require_sha1(head, "internal-error:schema-head")
    if mode not in {"forward", "rollback"}:
        common.fail("input-invalid:schema-mode")
    release_record.require_commit(repo_dir, base, "git-failed:schema-guard")
    release_record.require_commit(repo_dir, head, "git-failed:schema-guard")
    reasons: set[str] = set()
    summary: list[str] = []
    section = "catalog"
    try:
        base_catalog = repo.blob(base, CATALOG)
        head_catalog = repo.blob(head, CATALOG)
        if base_catalog is None and (mode == "forward" or head_catalog is None):
            return Classification("none", False, _legacy_check(repo_dir, base, head),
                                  ("pre-catalog record: Stage 1 schema freeze",))
        documents = {}
        for path, validator, label in ((CATALOG, _catalog, "catalog"), (HISTORY, _history, "history"),
                                       (GLOBALS, _globals, "global-tables")):
            section = label
            documents[path] = repo.document(head, path, validator)
            if base_catalog is not None:
                repo.document(base, path, validator)
            left, right = repo.blob(base, path), repo.blob(head, path)
            equal = (left == right if base_catalog is not None else
                     hashlib.sha256(right or b"").hexdigest() == BASELINE_V0_SHA256[path])
            if not equal:
                reasons.add("catalog-changed:" + path)
        engine = documents[HISTORY][-1].get("apply_engine", False)
        summary.append("history apply_engine: " + ("true; active changes still refused until engine implementation" if engine else "false"))
        if mode == "rollback":
            summary.append("rollback: exact catalog/history/global-tables equality; dormant data ignored")
            return Classification("none", False, tuple(sorted(reasons)), tuple(summary))
        changes = changed_paths(repo_dir, base, head)
        changed = {path for _, path in changes}
        section = "history-range"
        _valid(_git(repo_dir, ["merge-base", base, head]).strip() == base.encode())
        section = "migration-path"
        protected = _migration_contract(repo, base, head, reasons, summary)
        if not protected:
            for path in changed:
                if path.startswith(GUARDED_TREES) and not path.endswith("_test.go"):
                    reasons.add("guarded-tree:" + path)
        section = "migration-source"
        for path in GUARDED_FILES:
            if path in changed:
                reasons.add("guarded-file:" + path)
        for path, name in (*GUARDED_FUNCTIONS, *MIGRATION_FUNCTIONS):
            if path in changed and _function_bytes(repo.text(base, path), name) != _function_bytes(repo.text(head, path), name):
                reasons.add(f"migration-path-changed:{path}#{name}")
        for path, name in GUARDED_DECLS:
            if path in changed and _constant_bytes(repo.text(base, path), name) != _constant_bytes(repo.text(head, path), name):
                reasons.add(f"migration-path-changed:{path}#{name}")
        section = "dormant-entrypoints"
        dormant = _dormant(repo, head)
        for status, path in changes:
            if (status not in {"A", "M", "T"} or path.startswith(GUARDED_TREES) or path in GUARDED_FILES
                    or not path.endswith(".go") or path.endswith("_test.go") or not path.startswith(SCANNED_ROOTS)):
                continue
            source = repo.text(head, path) or ""
            offsets, offset = [], 0
            for source_line in source.splitlines(keepends=True):
                offsets.append(offset)
                offset += len(source_line)
            for number, line in _added_numbered(repo_dir, base, head, path):
                _valid(1 <= number <= len(offsets))
                for pattern in MIGRATION_CALL_PATTERNS:
                    for match in pattern.finditer(line):
                        begin, end = offsets[number - 1] + match.start(), offsets[number - 1] + match.end()
                        if not any(start <= begin and end <= stop for start, stop in dormant.get(path, [])):
                            reasons.add("migration-call:" + path)
        section = "registry"
        data = _registry_changes(repo, base, head, reasons, summary)
        section = "seeds"
        for revision in (base, head):
            repo.document(revision, SEEDS, _seeds, optional=True)
        before, after = repo.text(base, SEEDS), repo.text(head, SEEDS)
        if before != after:
            _valid(after is not None)
            data = True
            summary.append("seeds changed (dormant): public source diff follows")
            count, output_bytes = 0, 0
            for line in difflib.unified_diff((before or "").splitlines(), after.splitlines(), n=0):
                if line.startswith(("+", "-")) and not line.startswith(("+++", "---")):
                    count += 1
                    _valid(count <= 4096 and len(line) <= 4096)
                    escaped = json.dumps(line[1:], ensure_ascii=True)
                    output_bytes += len(escaped)
                    _valid(output_bytes <= 512 * 1024)
                    try:
                        common.require_public_text(escaped)
                    except common.ReleaseError as exc:
                        raise _Invalid() from exc
                    summary.append("seeds diff " + line[0] + " " + escaped)
        if any(path.startswith("internal/dbcatalog/shape/") for path in changed):
            summary.append("shape-only metadata: none (not an active golden input)")
        if not data:
            summary.append("classification: none")
        return Classification("data" if data else "none", data, tuple(sorted(reasons)), tuple(summary))
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        reasons.add("classifier-invalid:" + section)
        return Classification("none", False, tuple(sorted(reasons)), tuple(summary))


def check(repo_dir: Path, base: str, head: str, *, mode: str = "forward") -> tuple[str, ...]:
    """Compatibility API for CI/setup; classification adds public explanations."""
    return classify(repo_dir, base, head, mode=mode).reasons


def guard(repo_dir: Path, base: str, head: str, *, mode: str = "forward") -> Classification:
    result = classify(repo_dir, base, head, mode=mode)
    if result.reasons:
        raise SchemaChangeBlocked(result.reasons)
    return result


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Preview the read-only release schema classification")
    commands = parser.add_subparsers(dest="command", required=True)
    preview = commands.add_parser("preview")
    preview.add_argument("--base", required=True)
    preview.add_argument("--head", required=True)
    preview.add_argument("--mode", choices=("forward", "rollback"), default="forward")
    args = parser.parse_args(argv)
    try:
        result = classify(Path.cwd(), args.base, args.head, mode=args.mode)
        for line in (*result.reasons, *result.summary):
            common.require_public_text(line)
        payload = json.dumps(result._asdict(), sort_keys=True, separators=(",", ":"))
        print(payload)
        return 1 if result.reasons else 0
    except common.ReleaseError as exc:
        print(exc.code, file=sys.stderr)
        return 1


# Preparation-only policy API. No operational caller above selects this API.
# Requirements describe missing capabilities/obligations, not satisfied gates.
class PolicyClassification(NamedTuple):
    kind: str
    dormant: bool
    reasons: tuple[str, ...]
    summary: tuple[str, ...]
    migration_changes: tuple[str, ...]
    requirements: tuple[str, ...]


_V2_ATTRIBUTES = {
    "relations": {"relkind": "kind", "rls": "bool", "force_rls": "bool", "owner": "role", "acl": "acl", "security_invoker": "bool", "view_sha256": "hash", "partition_key_sha256": "hash", "partition_bound_sha256": "hash", "parents_sha256": "hash"},
    "columns": {"position": "int", "type_sha256": "hash", "not_null": "bool", "default_sha256": "hash", "identity": "identity", "generated": "generated", "collation_sha256": "hash", "acl": "acl"},
    "constraints": {"type": "constraint", "definition_sha256": "hash", "validated": "bool", "deferrable": "bool", "deferred": "bool"},
    "indexes": {"definition_sha256": "hash", "valid": "bool", "ready": "bool", "live": "bool", "unique": "bool", "primary": "bool", "exclusion": "bool"},
    "triggers": {"definition_sha256": "hash", "enabled": "enabled", "internal": "bool", "function_sha256": "hash"},
    "functions": {"source_sha256": "hash", "definition_sha256": "hash", "security_definer": "bool", "owner": "role", "acl": "acl", "config_sha256": "hash", "volatility": "volatility", "parallel": "parallel", "strict": "bool", "leakproof": "bool", "language_sha256": "hash"},
    "policies": {"command": "command", "permissive": "bool", "roles": "roles", "using_sha256": "hash", "check_sha256": "hash"},
    "sequences": {"type_sha256": "hash", "start": "int", "increment": "int", "min": "int", "max": "int", "cache": "int", "cycle": "bool", "owner": "role", "acl": "acl", "owned_by_sha256": "hash"},
    "types": {"type": "type", "definition_sha256": "hash", "owner": "role", "acl": "acl", "enum_sha256": "hash"},
    "extensions": {"version_sha256": "hash", "relocatable": "bool", "owner": "role"},
}
_V2_IDENTIFIER = re.compile(r"[a-z_][a-z0-9_]{0,62}\Z")
_V2_ROLES = {"MIGRATION_OWNER", "RUNTIME", "PUBLIC", "OTHER"}
_V2_CHILDREN = {"columns", "constraints", "indexes", "triggers", "policies"}
_V2_PENDING = GOLDEN + "pending/"
_V2_TENANT = "internal/database/tenant.go"


def _v2_attribute(shape: str, value: str) -> bool:
    if type(value) is not str:
        return False
    if shape == "hash":
        return re.fullmatch(r"[0-9a-f]{64}", value) is not None
    if shape == "bool":
        return value in {"true", "false"}
    if shape == "int":
        return re.fullmatch(r"-?[0-9]{1,20}", value) is not None
    if shape == "role":
        return value in _V2_ROLES
    if shape == "roles":
        items = value.split(",")
        return len(value) <= 64 and items == sorted(set(items)) and all(x in _V2_ROLES for x in items)
    if shape == "acl":
        if not value:
            return True
        entries = value.split(";")
        if len(value) > 8192 or entries != sorted(set(entries)):
            return False
        privileges = {"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "EXECUTE", "USAGE", "CREATE", "CONNECT", "TEMPORARY", "SET", "ALTER_SYSTEM", "MAINTAIN"}
        for entry in entries:
            match = re.fullmatch(r"(MIGRATION_OWNER|RUNTIME|PUBLIC|OTHER)>(MIGRATION_OWNER|RUNTIME|PUBLIC):([A-Z_]+):(true|false)", entry)
            if match is None or match[3] not in privileges:
                return False
        return True
    enums = {"kind": "rpvmfS", "identity": ("", "a", "d"), "generated": ("", "s", "v"),
             "constraint": "cfpuxnt", "enabled": "ODRA", "volatility": "isv",
             "parallel": "sru", "command": "*rawd", "type": "bcdeprm"}
    options = enums.get(shape, ())
    return value in options and (not isinstance(options, str) or len(value) == 1)


def _v2_catalog(value: Any) -> None:
    """Closed current object schema; version policy is checked separately.

    This does not infer SQL semantics from a definition hash. Parents must be
    represented public relations, not unverified names supplied by a caller.
    """
    _catalog(value)
    relations = {r["identity"] for r in value["objects"] if r["kind"] == "relations"}
    for row in value["objects"]:
        kind, name, parent = row["kind"], row["identity"], row.get("parent", "")
        if kind in _V2_CHILDREN:
            _valid(parent in relations and _V2_IDENTIFIER.fullmatch(parent) is not None
                   and name.startswith(parent + ".") and _V2_IDENTIFIER.fullmatch(name[len(parent) + 1:]) is not None)
        elif kind == "functions":
            _valid(not parent and re.fullmatch(r"[a-z_][a-z0-9_]{0,62}\([0-9a-f]{64}\)", name) is not None)
        else:
            _valid(not parent and _V2_IDENTIFIER.fullmatch(name) is not None)
        attrs = row["attributes"]
        _valid(set(attrs) == set(_V2_ATTRIBUTES[kind]))
        _valid(all(_v2_attribute(shape, attrs[key]) for key, shape in _V2_ATTRIBUTES[kind].items()))


def _v2_source_budget(declarations, before_raw, after_raw, extra_roles=None):
    """Charge each source role once before any new declaration is decoded."""
    _valid(type(declarations) is tuple and len(declarations) in (3, 5))
    if all(raw is None for raw in declarations):
        return
    limits = (schema_nullable.MAX_DOCUMENT, schema_nullable.MAX_DOCUMENT, schema_index.MAX_DECLARATION,
              schema_relation.MAX_DECLARATION, schema_tenant_profile.MAX_DECLARATION)
    _valid(all(raw is None or type(raw) is bytes and 0 < len(raw) <= limit
               for raw, limit in zip(declarations, limits)))
    _valid(all(type(raw) is bytes and 0 < len(raw) <= schema_nullable.MAX_DOCUMENT
               for raw in (before_raw, after_raw)))
    extra = ()
    if len(declarations) == 5 and any(raw is not None for raw in declarations[3:]):
        _valid(type(extra_roles) is tuple and len(extra_roles) == 4)
        _valid(all(type(raw) is bytes and 0 < len(raw) <= limit for raw, limit in zip(extra_roles,
                   (schema_tenant_profile.MAX_GLOBALS,) * 2 + (schema_tenant_profile.MAX_TENANT,) * 2)))
        extra = extra_roles
    else:
        _valid(extra_roles is None)
    _valid(sum(len(raw) for raw in declarations if raw is not None)
           + len(before_raw) + len(after_raw) + sum(map(len, extra)) <= schema_nullable.MAX_TOTAL)
    _valid(declarations[0] is None or declarations[1] is None)


def _v2_catalog_delta(before: dict, after: dict, old_globals: list, new_globals: list,
                      reasons: set[str], summary: list[str], requirements: set[str], *,
                      source=None, constant_source=None, index_source=None,
                      relation_source=None, profile_source=None) -> str:
    """Direct current-format rules; full transitions include every companion.

    A relation-only synthetic expand example is not a complete table migration.
    Row/array types and other opaque companions conservatively raise its class
    unless the complete closed source declaration explains that exact delta.
    """
    # Recheck the joint raw boundary even for direct pure callers. Each family
    # must refer to the same immutable comparison, not just equal parsed JSON.
    sources = (source, constant_source, index_source, relation_source, profile_source)
    present = [item for item in sources[:3] if item is not None]
    extended = [item for item in sources[3:] if item is not None]
    profile_delta = None
    if extended:
        _valid(all(type(item) is tuple and len(item) == 9 for item in extended))
        _valid(all(item[1:] == extended[0][1:] for item in extended))
        bc, ac, bg, ag, bt, at, base_sha, head_sha = extended[0][1:]
        _v2_source_budget(tuple(None if item is None else item[0] for item in sources), bc, ac, (bg, ag, bt, at))
        _valid(all(type(item) is tuple and len(item) == 5
                   and item[1:] == (bc, ac, base_sha, head_sha) for item in present))
        _valid(common.loads_strict(bc) == before and common.loads_strict(ac) == after
               and common.loads_strict(bg) == old_globals and common.loads_strict(ag) == new_globals)
    if present:
        _valid(all(type(item) is tuple and len(item) == 5 for item in present))
        _valid(all(type(item[1]) is bytes and type(item[2]) is bytes
                   and type(item[3]) is str and type(item[4]) is str for item in present))
        before_raw, after_raw, base_sha, head_sha = present[0][1:]
        if not extended:
            _v2_source_budget(tuple(None if item is None else item[0] for item in sources[:3]), before_raw, after_raw)
        _valid(all(item[1:] == present[0][1:] for item in present))
    _v2_catalog(before)
    _v2_catalog(after)
    _globals(old_globals)
    _globals(new_globals)
    explained = set()
    for label, material in zip(("nullable", "constant", "index", "relation", "tenant-profile"), sources):
        if material is None:
            continue
        # Only raw immutable material is accepted here, never a supplied verdict.
        declaration, before_raw, after_raw = material[:3]
        base_sha, head_sha = material[-2:]
        if label == "nullable":
            delta = schema_nullable.classify_source(declaration, before_raw, after_raw,
                                                   base_sha=base_sha, head_sha=head_sha)
        elif label == "constant":
            delta = schema_nullable.classify_constant_source(declaration, before_raw, after_raw,
                                                            base_sha=base_sha, head_sha=head_sha)
        elif label == "index":
            delta = schema_index.classify_source(declaration, before_raw, after_raw,
                                                base_sha=base_sha, head_sha=head_sha)
        elif label == "relation":
            delta = schema_relation.classify_source(declaration, *material[1:7], base_sha=base_sha, head_sha=head_sha)
            _valid(delta.operation.mode != "direct-tenant" or profile_source is not None)
        else:
            delta = schema_tenant_profile.classify_source(declaration,
                None if relation_source is None else relation_source[0],
                *material[1:7], base_sha=base_sha, head_sha=head_sha)
            profile_delta = delta
        _valid(common.loads_strict(before_raw) == before and common.loads_strict(after_raw) == after)
        current = {(ref.kind, ref.identity) for ref in delta.explained}
        _valid(not current.intersection(explained))
        explained.update(current)
        summary.append("source " + label + " declaration: sha256=" + delta.declaration_sha256
                       + ("; operation=" + delta.operation_sha256 if label != "tenant-profile" else "")
                       + "; source interpretation only")
    left = {(r["kind"], r["identity"]): (r.get("parent", ""), r["attributes"]) for r in before["objects"]}
    right = {(r["kind"], r["identity"]): (r.get("parent", ""), r["attributes"]) for r in after["objects"]}
    old_global = {r["name"]: r for r in old_globals}
    new_global = {r["name"]: r for r in new_globals}
    validated_profiles = set() if profile_delta is None else {
        (item.function_ref.kind, item.function_ref.identity) for item in profile_delta.profiles}
    if any(kind == "functions" and re.fullmatch(r"rereply_tenant_policy_additive_fingerprint_v(?:[2-9]|[1-9][0-9]+)\([0-9a-f]{64}\)", name)
           and (kind, name) not in validated_profiles for kind, name in right):
        reasons.add("additive-profile-unsupported")
        requirements.add("additive-profile-unsupported")
    old_relations = {name for kind, name in left if kind == "relations"}
    new_relations = {name for kind, name in right if kind == "relations"} - old_relations
    # An added exemption is expansion only alongside its new relation. Existing
    # exemptions (including reason/seed-key changes) remain part of the contract.
    classification = "none"
    if any(old_global.get(name) != new_global.get(name) for name in old_global.keys() | new_global.keys()
           if name not in new_relations or name in old_global):
        classification = "contract"
        summary.append("global table policy changed")
    for key in sorted(left.keys() | right.keys()):
        old, new = left.get(key), right.get(key)
        if old == new:
            continue
        kind, name = key
        proposed = "contract"
        if old is not None:
            summary.append(("removed " if new is None else "changed ") + kind + " " + name)
        else:
            parent, attrs = new
            summary.append("new " + kind + " " + name)
            if kind == "relations":
                relation_kind = attrs["relkind"]
                if relation_kind in {"r", "p", "f"}:
                    protected = attrs["rls"] == attrs["force_rls"] == "true"
                    if not protected and name not in new_global:
                        reasons.add("unprotected-new-relation:" + name)
                    elif relation_kind in {"r", "p"}:
                        proposed = "expand"
                elif relation_kind == "v":
                    summary.append("new view " + name)
                    if attrs["security_invoker"] == "true":
                        requirements.add("view-dependency-unsupported")
                elif relation_kind == "m":
                    summary.append("new materialized view " + name)
            elif kind == "columns":
                if (parent in old_relations and attrs["not_null"] == "false"
                        and attrs["identity"] == attrs["generated"] == attrs["acl"] == ""
                        and attrs["default_sha256"] == hashlib.sha256(b"").hexdigest()):
                    proposed = "expand"
            elif kind == "functions":
                if attrs["security_definer"] == "true":
                    summary.append("new definer function " + name)
                elif attrs["owner"] == "MIGRATION_OWNER" and attrs["acl"] in {
                        "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>PUBLIC:EXECUTE:false",
                        "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>RUNTIME:EXECUTE:false"}:
                    proposed = "expand"
            elif kind == "indexes" and attrs["unique"] == "false" and key not in explained:
                requirements.add("index-operation-unsupported")
        if key in explained:
            proposed = "expand"
        if proposed == "contract" or classification == "contract":
            classification = "contract"
        elif proposed == "expand":
            classification = "expand"
    return classification


def _v2_history(before: list, after: list, old_catalog: dict, new_catalog: dict,
                semantic_changed: bool, kind: str, reasons: set[str]) -> bool:
    _history(before)
    _history(after)
    # Absent and explicit false have identical meaning. No other historical
    # edit, deletion or insertion is an append-only history.
    normalize = lambda rows: [{**r, "apply_engine": r.get("apply_engine", False)} for r in rows]
    if normalize(after[:len(before)]) != normalize(before):
        reasons.add("history-rewritten")
    if old_catalog["version"] != before[-1]["version"] or new_catalog["version"] != after[-1]["version"]:
        reasons.add("history-catalog-version-mismatch")
    rise = new_catalog["version"] > old_catalog["version"]
    if semantic_changed != rise or new_catalog["version"] < old_catalog["version"]:
        reasons.add("history-version-change-mismatch")
    floor = -1
    for row in after:
        if row["min_reader_version"] < floor:
            reasons.add("history-reader-decreased")
        if row["min_reader_version"] > max(floor, 0) and row["class"] != "contract":
            reasons.add("history-reader-requires-contract")
        floor = row["min_reader_version"]
    if semantic_changed and kind == "contract" and after[-1]["class"] != "contract":
        reasons.add("history-class-understated")
    return after[-1].get("apply_engine", False)


def _v2_migrations(repo: _Repository, base: str, head: str, changes: list[tuple[str, str]],
                   reasons: set[str], summary: list[str]) -> tuple[str, ...]:
    # Reuse the established first-parent/whole-file introduction fences. Only
    # the exact endpoint closure observation can become nonblocking, and only
    # after BOTH endpoints validate as existing contracts with unchanged roots.
    observed: set[str] = set()
    protected = _migration_contract(repo, base, head, observed, [])
    old = repo.document(base, MIGRATION_PATH, lambda _: None, optional=True)
    new = repo.document(head, MIGRATION_PATH, lambda _: None, optional=True)
    if not protected or new is None:
        reasons.add("migration-contract-required")
        return ()
    fence_failures = observed - {"migration-path-changed:closure"}
    if fence_failures:
        reasons.update(fence_failures)
        return ()
    if old is None:
        # A valid first introduction keeps the old full-file anchor result.
        # It cannot opt a bundled source/endpoint closure change out of a fence.
        return ()
    output = []
    if old != new:
        reasons.discard("migration-path-changed:closure")
        output.append("generated migration closure changed")
    files = set(old["closure_files"]) & set(new["closure_files"])
    changed = {path for _, path in changes}
    for path, name in (*GUARDED_FUNCTIONS, *MIGRATION_FUNCTIONS, *GUARDED_DECLS):
        reason = f"migration-path-changed:{path}#{name}"
        if reason in reasons and path in changed and path in files:
            if old["closure_sha256"] == new["closure_sha256"]:
                reasons.add("migration-contract-stale")
            else:
                reasons.remove(reason)
                output.append("migration path changed: " + path + "#" + name)
    summary.extend(output)
    return tuple(output)


def _v2_tenant_declarations(source: str | None) -> dict[str, str]:
    """Recognize only the existing gofmt top-level literal inventory.

    Alternate declaration syntax refuses; there is no parser for a future
    additive-version grammar and no inference from a fingerprint string.
    """
    if source is None:
        return {}
    code = _go_code(source)
    output = {}
    for name in ("DirectTenantTables", "RelatedTenantTables", "tenantRLSAdditiveProfileV1"):
        tokens = list(re.finditer(r"\b" + name + r"\b", code))
        declaration = re.search(r"(?m)^var " + name + r" = (?:\[\]string|map\[string\]string)\{", code)
        if not tokens:
            continue
        _valid(declaration is not None)
        opening = declaration.end() - 1
        depth, end = 1, opening + 1
        while end < len(code) and depth:
            depth += (code[end] == "{") - (code[end] == "}")
            end += 1
        _valid(depth == 0)
        output[name] = source[declaration.start():end]
    return output


def _v2_pending(repo: _Repository, base: str, head: str, reasons: set[str],
                summary: list[str], requirements: set[str]) -> bool:
    """Read the complete immutable pending history; never infer execution.

    Format 1 consumption/activation and non-expand plans remain unsupported.
    An existing plan is immutable, including its active-byte binding. Empty
    endpoints do not excuse an invalid or removed intermediate introduction.
    """
    raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
    _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(len(revisions) <= MAX_HISTORY_COMMITS and len(set(revisions)) == len(revisions)
           and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
           and ((base == head and not revisions) or (base != head and revisions and revisions[-1] == head)))
    prior = None
    plan = None
    introduced = False
    active = None
    existing_producer_block = "producer-class-unsupported" in requirements
    for revision in (base, *revisions):
        # Without the trailing slash, a blob/gitlink masquerading as the
        # pending directory is visible too and fails the closed path check.
        raw = _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")])
        if not raw:
            _valid(prior is None)
            continue
        reasons.add("pending-format-unsupported")
        requirements.update(("pending-format-unsupported", "producer-class-unsupported"))
        _valid(len(raw) <= MAX_ARTIFACT_BYTES and raw.endswith(b"\0"))
        paths = raw[:-1].decode("utf-8", "strict").split("\0")
        _valid(len(paths) == pending_plan.MAX_ENTRIES + 2 and len(set(paths)) == len(paths)
               and all(_path(path) and path.startswith(_V2_PENDING) for path in paths))
        files = {path: repo.blob(revision, path) for path in paths}
        _valid(all(type(value) is bytes for value in files.values()))
        current = tuple(repo.blob(revision, path) for path in (CATALOG, GLOBALS, HISTORY))
        if active is None:
            active = tuple(repo.blob(base, path) for path in (CATALOG, GLOBALS, HISTORY))
        _valid(current == active and all(type(value) is bytes for value in active))
        plan = pending_plan.parse_bundle(active_catalog=current[0], active_globals=current[1],
                                         active_history=current[2], files=files, prior_files=prior)
        if prior is None:
            introduced = revision != base
        prior = files
    if plan is None:
        return False
    # This is only the source-declared dormant expansion vocabulary. Opaque
    # target hashes and operation labels are not SQL/CONCURRENTLY proof.
    supported = all(entry.kind == "expand" for entry in plan.entries)
    if supported:
        reasons.discard("pending-format-unsupported")
        requirements.discard("pending-format-unsupported")
        if not existing_producer_block:
            requirements.discard("producer-class-unsupported")
    summary.append("pending source plan: active=" + str(plan.active_version) + "; target="
                   + str(plan.entries[0].version) + "; source=" + head)
    for path, digest in plan.files:
        summary.append("pending source file: " + path + " sha256=" + digest)
    for step in plan.steps:
        summary.append("pending source step: " + step.id + "; operation=" + step.operation
                       + "; source_sha256=" + step.source_sha256 + "; requires_gates="
                       + (",".join(step.requires_gates) or "none"))
    summary.append("pending source plan only; actual ledger/apply state unavailable; gates not evaluated")
    if len(plan.steps) > 1 and any(step.requires_gates for step in plan.steps):
        summary.append("pending gates: a failed gate would hold back unrelated steps; holdback review required")
    return introduced and supported


def classify_v2(repo_dir: Path, base: str, head: str) -> PolicyClassification:
    """Prepare forward policy; does NOT authorize a release or lift guard().

    Source catalog labels are not measured DB shape or signed evidence. Active
    future catalogs and additive >=2 producers remain explicitly unsupported.
    A valid immutable pending expansion is dormant metadata; it still needs
    the exact source-bound compatibility experiment and is not SQL-safety or
    apply-engine authority. REQUIRE_COMPAT adoption remains atomic.
    """
    repo = _Repository(repo_dir)
    legacy = _classify(repo, base, head)
    return _classify_v2(repo, base, head, legacy)


def _v2_profile_history(repo: _Repository, base: str, head: str) -> None:
    """Check retained identities at every first parent, in the original scope."""
    raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
    _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(len(revisions) <= MAX_HISTORY_COMMITS and len(set(revisions)) == len(revisions)
           and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
           and ((base == head and not revisions) or (base != head and revisions and revisions[-1] == head)))
    prior = tuple(repo.blob(base, path) for path in (CATALOG, GLOBALS, _V2_TENANT))
    for revision in revisions:
        current = tuple(repo.blob(revision, path) for path in (CATALOG, GLOBALS, _V2_TENANT))
        schema_tenant_profile.validate_retained_pair(prior[0], current[0], prior[1], current[1], prior[2], current[2])
        prior = current


def _v2_tenant_explained(left: str, right: str, delta) -> bool:
    """Freshly replayed exact declaration hashes, never a whole-file waiver."""
    before = dict(schema_relation._tenant_inventory(left.encode()).declarations)
    after = dict(schema_relation._tenant_inventory(right.encode()).declarations)
    changes = {name: (None if name not in before else hashlib.sha256(before[name]).hexdigest(),
                     None if name not in after else hashlib.sha256(after[name]).hexdigest())
               for name in before.keys() | after.keys() if before.get(name) != after.get(name)}
    expected = {item.name: (item.before_declaration_sha256, item.after_declaration_sha256)
                for item in delta.tenant_changes}
    return changes == expected


def _classify_v2(repo: _Repository, base: str, head: str,
                 legacy: Classification) -> PolicyClassification:
    repo_dir = repo.root
    reasons = set(legacy.reasons)
    summary = [line for line in legacy.summary if not line.startswith(("history apply_engine:", "classification:"))]
    requirements = {"compat-required", "generated-migration-closure-ci"}
    migrations: tuple[str, ...] = ()
    kind, dormant = legacy.kind, legacy.dormant
    section = "catalog"
    try:
        section = "nullable-source"
        declaration = repo.blob(head, schema_nullable.SOURCE_PATH)
        section = "constant-source"
        constant_declaration = repo.blob(head, schema_nullable.CONSTANT_SOURCE_PATH)
        section = "index-source"
        index_declaration = repo.blob(head, schema_index.SOURCE_PATH)
        section = "relation-source"
        relation_declaration = repo.blob(head, schema_relation.SOURCE_PATH)
        section = "tenant-profile-source"
        profile_declaration = repo.blob(head, schema_tenant_profile.SOURCE_PATH)
        declarations = (declaration, constant_declaration, index_declaration, relation_declaration, profile_declaration)
        labels = ("nullable-source", "constant-source", "index-source", "relation-source", "tenant-profile-source")
        source_section = next((label for label, raw in zip(labels, declarations) if raw is not None), "catalog")
        if sum(raw is not None for raw in declarations) > 1:
            source_section = "source-classification"
        section = "catalog"
        before_raw = repo.blob(base, CATALOG)
        after_raw = None
        extra_roles = None
        if any(raw is not None for raw in declarations):
            after_raw = repo.blob(head, CATALOG)
            if relation_declaration is not None or profile_declaration is not None:
                section = "global-tables"
                before_global, after_global = repo.blob(base, GLOBALS), repo.blob(head, GLOBALS)
                section = "tenant-profile"
                before_tenant, after_tenant = repo.blob(base, _V2_TENANT), repo.blob(head, _V2_TENANT)
                extra_roles = (before_global, after_global, before_tenant, after_tenant)
            section = source_section
            # Bound all descriptor/catalog roles before this v2 pass decodes;
            # the unchanged legacy classifier has its own original read bounds.
            _v2_source_budget(declarations, before_raw, after_raw, extra_roles)
        if before_raw is None:
            reasons.add("catalog-baseline-required")
            return PolicyClassification(kind, dormant, tuple(sorted(reasons)), tuple(summary), (), tuple(sorted(requirements)))
        section = "catalog"
        old = repo.document(base, CATALOG, _v2_catalog)
        new = repo.document(head, CATALOG, _v2_catalog)
        section = "global-tables"
        old_global = repo.document(base, GLOBALS, _globals)
        new_global = repo.document(head, GLOBALS, _globals)
        section = source_section
        source = None if declaration is None else (
            declaration, before_raw, after_raw, base, head)
        constant_source = None if constant_declaration is None else (
            constant_declaration, before_raw, after_raw, base, head)
        index_source = None if index_declaration is None else (
            index_declaration, before_raw, after_raw, base, head)
        relation_source = None if relation_declaration is None else (
            relation_declaration, before_raw, after_raw, *extra_roles, base, head)
        profile_source = None if profile_declaration is None else (
            profile_declaration, before_raw, after_raw, *extra_roles, base, head)
        semantic = _v2_catalog_delta(old, new, old_global, new_global, reasons, summary, requirements,
                                     source=source, constant_source=constant_source, index_source=index_source,
                                     relation_source=relation_source, profile_source=profile_source)
        profile_delta = None
        if profile_source is not None:
            profile_delta = schema_tenant_profile.classify_source(profile_declaration, relation_declaration,
                *profile_source[1:7], base_sha=base, head_sha=head)
            section = "tenant-profile-history"
            _v2_profile_history(repo, base, head)
        section = "history"
        before = repo.document(base, HISTORY, _history)
        after = repo.document(head, HISTORY, _history)
        if after[-1]["min_reader_version"] > before[-1]["min_reader_version"]:
            semantic = "contract"
            summary.append("minimum reader version increased")
        old_sections = ({(r["kind"], r["identity"]): (r.get("parent", ""), r["attributes"]) for r in old["objects"]},
                        {r["name"]: r for r in old_global})
        new_sections = ({(r["kind"], r["identity"]): (r.get("parent", ""), r["attributes"]) for r in new["objects"]},
                        {r["name"]: r for r in new_global})
        engine = _v2_history(before, after, old, new, old_sections != new_sections, semantic, reasons)
        for path in (CATALOG, HISTORY, GLOBALS):
            reasons.discard("catalog-changed:" + path)
        if semantic == "contract":
            kind, dormant = "contract", False
        elif legacy.kind == "data":
            kind, dormant = "data", not engine
        elif semantic == "expand":
            kind, dormant = "expand", False
        if semantic != "none" and not engine:
            reasons.add("active-catalog-without-apply-engine")
        if engine or old["version"] != 0 or new["version"] != 0:
            requirements.add("catalog-format/profile-unsupported")
        section = "history-range"
        _valid(_git(repo_dir, ["merge-base", base, head]).strip() == base.encode())
        changes = changed_paths(repo_dir, base, head)
        _valid(len(changes) <= 10000 and all(_changed_path(path) for _, path in changes))
        section = "migration-path"
        migrations = _v2_migrations(repo, base, head, changes, reasons, summary)
        section = "tenant-profile"
        if any(path == _V2_TENANT for _, path in changes):
            left, right = repo.text(base, _V2_TENANT), repo.text(head, _V2_TENANT)
            if _v2_tenant_declarations(left) != _v2_tenant_declarations(right):
                if profile_delta is None or not _v2_tenant_explained(left, right, profile_delta):
                    kind, dormant = "contract", False
                    summary.append("tenant profile declarations changed")
            for name in ("tenantRLSRoutingVersion", "tenantRLSAdditiveProfileVersion", "tenantPolicyFingerprintSignature", "tenantAdditivePolicyFingerprintSignature"):
                if _constant_bytes(left, name) != _constant_bytes(right, name):
                    kind, dormant = "contract", False
                    requirements.add("additive-profile-unsupported")
            if (profile_delta is None
                    and re.search(r"\btenantRLSAdditiveProfileV(?:[2-9]|[1-9][0-9]+)\b", _go_code(right or ""))):
                reasons.add("additive-profile-unsupported")
                requirements.add("additive-profile-unsupported")
        section = "pending"
        pending_introduced = _v2_pending(repo, base, head, reasons, summary, requirements)
        if pending_introduced and kind == "none":
            kind, dormant = "expand", True
        section = "registry"
        rows = repo.document(head, REGISTRY, _registry, optional=True) or []
        if len(rows) > 1 and any(row["requires_gates"] for row in rows):
            summary.append("gated step inventory: holdback review required; pending/apply state unavailable")
        if kind == "contract":
            requirements.add("contract-label")
        if kind in {"expand", "data", "contract"} and not dormant:
            requirements.add("restore-drills")
        if kind != "none" and not (dormant and (kind == "data"
                or (kind == "expand" and pending_introduced))):
            requirements.add("producer-class-unsupported")
        summary.append("policy classification: " + kind + (" (dormant)" if dormant else ""))
        _valid(len(summary) <= 25000)
        for line in (*reasons, *summary, *migrations):
            common.require_public_text(line)
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        reasons.add("classifier-v2-invalid:" + section)
        if section == "history":
            requirements.add("history-format-unsupported")
    return PolicyClassification(kind, dormant, tuple(sorted(reasons)), tuple(summary), migrations, tuple(sorted(requirements)))


def check_ci_source(repo_dir: Path, base: str, head: str) -> tuple[str, ...]:
    """CI source eligibility only; generated tests AND exact compat must pass.

    Preserve legacy clean/refused decisions except the sole endpoint closure
    reason under the existing none-class source contract. This does not grant
    experiment, merge or release authority, or change check()/guard().
    """
    repo = _Repository(repo_dir)
    legacy = _classify(repo, base, head)
    if legacy.reasons != ("migration-path-changed:closure",):
        return legacy.reasons
    policy = _classify_v2(repo, base, head, legacy)
    if (policy.reasons or policy.kind != "none" or policy.dormant is not False
            or policy.requirements != ("compat-required", "generated-migration-closure-ci")
            or policy.migration_changes != ("generated migration closure changed",)):
        return legacy.reasons
    try:
        # _classify_v2 has validated both manifests and the complete history.
        # File-list-only edits cannot stand in for an actual new closure hash.
        old = repo.document(base, MIGRATION_PATH, lambda _: None)
        new = repo.document(head, MIGRATION_PATH, lambda _: None)
        _valid(old["roots"] == new["roots"] == MIGRATION_ROOTS
               and old["closure_sha256"] != new["closure_sha256"])
        for path in (CATALOG, HISTORY, GLOBALS, GOLDEN + "production-v0.json",
                     "internal/dbcatalog/shape/production-v0.sql", REGISTRY, SEEDS):
            before, after = repo.blob(base, path), repo.blob(head, path)
            # blob() enforces regular mode, per-file and shared total bounds.
            # Absence or two empty blobs are not the active source contract.
            _valid(type(before) is bytes and type(after) is bytes
                   and 0 < len(before) <= MAX_ARTIFACT_BYTES and before == after)
        changes = changed_paths(repo.root, base, head)
        _valid(len(changes) <= 10000 and all(_changed_path(path) for _, path in changes)
               and not any(path == _V2_PENDING.rstrip("/") or path.startswith(_V2_PENDING)
                           for _, path in changes))
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return legacy.reasons
    return ()


if __name__ == "__main__":
    raise SystemExit(main())
