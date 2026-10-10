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
    common.require_sha1(base, "internal-error:schema-base")
    common.require_sha1(head, "internal-error:schema-head")
    if mode not in {"forward", "rollback"}:
        common.fail("input-invalid:schema-mode")
    release_record.require_commit(repo_dir, base, "git-failed:schema-guard")
    release_record.require_commit(repo_dir, head, "git-failed:schema-guard")
    repo = _Repository(repo_dir)
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


if __name__ == "__main__":
    raise SystemExit(main())
