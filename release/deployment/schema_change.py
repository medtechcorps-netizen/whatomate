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
        self._trees: dict[tuple[str, str], bytes] = {}
        self.total = 0

    def _tree(self, revision: str, path: str) -> bytes:
        key = (revision, path)
        if key not in self._trees:
            self._trees[key] = _git(self.root, ["ls-tree", "-z", revision, "--", path])
        return self._trees[key]

    def blob(self, revision: str, path: str) -> bytes | None:
        key = (revision, path)
        if key in self.cache:
            return self.cache[key]
        raw = self._tree(revision, path)
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
    history_raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"])
    commits = history_raw.splitlines()
    _valid(len(commits) <= MAX_HISTORY_COMMITS)
    _pr18_run_metadata_remember_history(repo, base, head, history_raw)
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
    history_raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"])
    commits = history_raw.splitlines()
    _valid(len(commits) <= MAX_HISTORY_COMMITS)
    _pr18_run_metadata_remember_history(repo, base, head, history_raw)
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
        section = "pr18-verification-repair"
        if _pr18_verification_repair_changed(repo, base, head):
            _pr18_verification_repair_local(repo, base, head)
        section = "pr18-managed-owner-repair"
        if _pr18_managed_owner_changed(repo, base, head):
            _pr18_managed_owner_local(repo, base, head)
        section = "pr18-run-metadata-repair"
        # An existing classifier-invalid refusal is never cleared below. Keep
        # its original validation/error ownership instead of adding a new scan.
        if (not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_run_metadata_changed(repo, base, head)):
            _pr18_run_metadata_local(repo, base, head)
        section = "pr18-managed-replication-repair"
        # This exact legacy refusal can be cleared only by the mandatory
        # final observer transition below, which replays the replication edge.
        # Keep the introduction/undo scan for every ordinary or local range.
        if legacy.reasons == _PR18_OBSERVER_LEGACY:
            if repo._tree(head, _PR18_PLANNER_PUBLIC_OWNER_MARKER):
                _pr18_planner_public_owner_prime_protected(repo, base, head)
            else:
                _pr18_managed_replication_prime_protected(repo, base, head)
        if (legacy.reasons != _PR18_OBSERVER_LEGACY
                and not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_managed_replication_changed(repo, base, head)):
            _pr18_managed_replication_local(repo, base, head)
        section = "pr18-planner-public-owner-repair"
        if (legacy.reasons != _PR18_OBSERVER_LEGACY
                and not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_planner_public_owner_changed(repo, base, head)):
            _pr18_planner_public_owner_local(repo, base, head)
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
        if (reasons == {_PR18_OBSERVER_CALL} and kind == "data" and dormant is True
                and tuple(sorted(requirements)) == _PR18_OBSERVER_REQUIREMENTS
                and migrations == _PR18_OBSERVER_MIGRATIONS):
            # Only the reviewed existing-call rewrite can remove this one reason.
            # Proof/bounded-source refusal preserves it; transport errors propagate.
            try:
                _pr18_observer_transition(repo, base, head)
            except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
                pass
            else:
                reasons.remove(_PR18_OBSERVER_CALL)
        summary.append("policy classification: " + kind + (" (dormant)" if dormant else ""))
        _valid(len(summary) <= 25000)
        for line in (*reasons, *summary, *migrations):
            common.require_public_text(line)
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        reasons.add("classifier-v2-invalid:" + section)
        if section == "history":
            requirements.add("history-format-unsupported")
    return PolicyClassification(kind, dormant, tuple(sorted(reasons)), tuple(summary), migrations, tuple(sorted(requirements)))


# One source-only removes-write exception, not generic dormant-data admission.
_CI_SEED_PATH = "internal/database/postgres.go"
_CI_SEED_NAME = "SeedSystemRolesForAllOrgs"
_CI_SEED_CHILD = _CI_SEED_NAME + "/legacy-super-admin-update"
_CI_SEED_BINDINGS = "internal/dbcatalog/registry/coordinator_bindings.json"
_CI_SEED_REASON = "migration-path-changed:" + _CI_SEED_PATH + "#" + _CI_SEED_NAME
_CI_SEED_SQL = "UPDATE users SET is_super_admin = true WHERE email = 'admin@admin.com'"
_CI_SEED_REMOVAL = (
    "\t// Make admin@admin.com a super admin if exists\n"
    '\tif err := db.Exec("' + _CI_SEED_SQL + '").Error; err != nil {\n'
    '\t\treturn fmt.Errorf("failed to set super admin: %w", err)\n'
    "\t}\n\n"
)
_CI_SEED_ROLES = (_CI_SEED_PATH, REGISTRY, MIGRATION_PATH, _CI_SEED_BINDINGS)
_CI_SEED_PATHS = (
    *(("M", path) for path in _CI_SEED_ROLES),
    ("M", "internal/dbcatalog/registry/registry_test.go"),
    ("A", "internal/database/catalog_legacy_admin_test.go"),
    ("A", "release/deployment/fixtures/classifier/pr17-rev-bump.json"),
    ("A", "release/deployment/fixtures/classifier/pr17-no-rev-refused.json"),
)


def _ci_seed_changed(repo: _Repository, base: str, head: str) -> bool:
    if not any(path == _CI_SEED_PATH for _, path in changed_paths(repo.root, base, head)):
        return False
    before = _function_bytes(repo.text(base, _CI_SEED_PATH), _CI_SEED_NAME)
    after = _function_bytes(repo.text(head, _CI_SEED_PATH), _CI_SEED_NAME)
    return before != after or len(before) > 1 or len(after) > 1


def _ci_seed_paths(repo: _Repository, base: str, head: str, *, complete: bool = False) -> None:
    changes = changed_paths(repo.root, base, head)
    _valid(len(changes) <= 10000 and len(changes) == len(set(changes))
           and all(change in _CI_SEED_PATHS for change in changes))
    if complete:
        _valid(all(("M", path) in changes for path in _CI_SEED_ROLES))
    for status, path in changes:
        # Reject mode-only/type changes too; all reviewed source inputs are 644.
        for revision in ((head,) if status == "A" else (base, head)):
            raw = _git(repo.root, ["ls-tree", "-z", revision, "--", path])
            _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
            _valid(type(repo.blob(revision, path)) is bytes)


def _ci_seed_removal(repo: _Repository, base: str, head: str, legacy: Classification) -> None:
    _valid(legacy.reasons == ("migration-path-changed:closure",))
    policy = _classify_v2(repo, base, head, legacy)
    _valid(not policy.reasons and policy.kind == "data" and policy.dormant is True
           and policy.requirements == ("compat-required", "generated-migration-closure-ci")
           and policy.migration_changes == ("generated migration closure changed",))
    before, after = (repo.text(rev, _CI_SEED_PATH) for rev in (base, head))
    old_fn, new_fn = (_function_bytes(raw, _CI_SEED_NAME) for raw in (before, after))
    _valid(len(old_fn) == len(new_fn) == 1)
    _valid(hashlib.sha256(old_fn[0].encode()).hexdigest() ==
           "4f2f5aa66866133bb2e5cc01a30ed2f726649e660c2bd8a75c6fdcdbd5401b34"
           and hashlib.sha256(new_fn[0].encode()).hexdigest() ==
           "4691de0f24459d4274cc553df2cd6cb6f08eff32648df3de93ad72fad92ac5bd")
    _valid(before.count(_CI_SEED_REMOVAL) == old_fn[0].count(_CI_SEED_REMOVAL) == 1
           and after == before.replace(_CI_SEED_REMOVAL, "", 1))

    old_rows = repo.document(base, REGISTRY, _registry)
    new_rows = repo.document(head, REGISTRY, _registry)
    _valid([row["id"] for row in old_rows] == [row["id"] for row in new_rows])
    old = {row["id"]: row for row in old_rows}
    new = {row["id"]: row for row in new_rows}
    _valid({name for name in old if old[name] != new[name]} == {_CI_SEED_NAME, _CI_SEED_CHILD})
    for name in (_CI_SEED_NAME, _CI_SEED_CHILD):
        left, right = old[name], new[name]
        _valid(left["rev"] == 1 and right["rev"] == 2
               and left["func"] == "github.com/shridarpatil/whatomate/internal/database.SeedSystemRolesForAllOrgs"
               and left["kind"] == "seed-reconciler" and left["baseline"] is True
               and left["idempotent"] is (name == _CI_SEED_CHILD)
               and left["n1_safe"] is False and left["requires_gates"] == []
               and ((name == _CI_SEED_NAME and "parent" not in left)
                    or (name == _CI_SEED_CHILD and left.get("parent") == _CI_SEED_NAME))
               and {k: v for k, v in left.items() if k not in {"rev", "closure_sha256"}}
                   == {k: v for k, v in right.items() if k not in {"rev", "closure_sha256"}})
    _valid(old[_CI_SEED_NAME]["closure_sha256"] == old[_CI_SEED_CHILD]["closure_sha256"]
           and new[_CI_SEED_NAME]["closure_sha256"] == new[_CI_SEED_CHILD]["closure_sha256"]
           and old[_CI_SEED_NAME]["closure_sha256"] != new[_CI_SEED_NAME]["closure_sha256"])
    # The real generator must still prove these claimed closure digests.
    old_contract = repo.document(base, MIGRATION_PATH, lambda _: None)
    new_contract = repo.document(head, MIGRATION_PATH, lambda _: None)
    _valid(old_contract["roots"] == new_contract["roots"] == MIGRATION_ROOTS
           and old_contract["closure_files"] == new_contract["closure_files"]
           and old_contract["closure_sha256"] != new_contract["closure_sha256"])

    old_binding = repo.document(base, _CI_SEED_BINDINGS, lambda _: None)
    new_binding = repo.document(head, _CI_SEED_BINDINGS, lambda _: None)
    _valid(type(old_binding) is dict and set(old_binding) == {
        "version", "execution_labels", "model_descriptors", "model_list_sha256",
        "coordinator_sha256", "coordinator_calls", "seed_child_calls", "seed_child_order",
        "inline_sql", "callbacks", "callback_calls", "callback_sha256",
        "idempotency_rationale", "n1_safety_note", "child_semantics"})
    children = ["seedSystemRolesForOrg", "FixSystemRolePermissions",
                "ApplyManagerSettingsPolicyMigration", "MigrateExistingUserRoles"]
    _valid(type(old_binding) is dict and type(old_binding.get("version")) is int
           and old_binding["version"] == 1 and old_binding["seed_child_order"] == [*children, "legacy-super-admin-update"]
           and old_binding["seed_child_calls"] == children and old_binding["inline_sql"] == _CI_SEED_SQL
           and old_binding["idempotency_rationale"][_CI_SEED_CHILD] ==
               "Existing inline UPDATE sets the same boolean; no permission-policy endorsement")
    expected = {**old_binding, "seed_child_order": children, "inline_sql": "",
                "idempotency_rationale": {**old_binding["idempotency_rationale"], _CI_SEED_CHILD:
                    "removes-write: legacy admin@admin.com escalation removed at rev 2; retained inert marker bound to parent closure"}}
    _valid(common.canonical_file_bytes(expected) == common.canonical_file_bytes(new_binding))

    for path in (CATALOG, HISTORY, GLOBALS, GOLDEN + "production-v0.json",
                 "internal/dbcatalog/shape/production-v0.sql", SEEDS):
        left, right = repo.blob(base, path), repo.blob(head, path)
        _valid(type(left) is bytes and bool(left) and left == right)
    for revision in (base, head):
        _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
        for path in (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                     schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH):
            _valid(repo.blob(revision, path) is None)
    _ci_seed_paths(repo, base, head, complete=True)
    raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
    _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(set(revisions)) == len(revisions)
           and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
           and revisions[-1] == head)
    old_state = tuple(repo.blob(base, path) for path in _CI_SEED_ROLES)
    new_state = tuple(repo.blob(head, path) for path in _CI_SEED_ROLES)
    prior, advanced = base, False
    for revision in revisions:
        _ci_seed_paths(repo, prior, revision)
        state = tuple(repo.blob(revision, path) for path in _CI_SEED_ROLES)
        _valid(state == new_state or (not advanced and state == old_state))
        advanced = advanced or state == new_state
        prior = revision
    _valid(advanced)


# Fixed reviewed PR18 read-only observer transition. These are source fingerprints,
# not release/commit identities or a general migration-call permission. The same
# bytes can be recut after a separately qualified policy prerequisite.
_PR18_OBSERVER_REASON = "ci-source-pr18-observer-transition-required"
_PR18_OBSERVER_MAIN = "cmd/whatomate/main.go"
_PR18_OBSERVER_CALL = "migration-call:" + _PR18_OBSERVER_MAIN
_PR18_OBSERVER_MAIN_STATES = (
    "6e4555346d9e8ebd41eac8992cb5b5add17a653239938a181e20c2f9f67e2d94",  # before PR7
    "3a13697d9839c9ef2cf1b2f6b76d53449c81ce2927cc087e7d2773f6f4cf1e0b",  # before PR18
    "6da28438261bf29ffe1b02a36c8ed8ff2ba2620df821979b458790e882bc41d8",  # reviewed PR18
)
_PR18_OBSERVER_FILES = {
    '.github/workflows/test.yml': ('8adfd6a64bff17a078e8daa626cecc85d1118ebbc96a063b5c0c5cfc739ff2f9', '3f8d54640780c2e7dee3a59480a9d3a44004c2c8462f367e44bbfad546b04c16'),
    'cmd/whatomate/main.go': ('3a13697d9839c9ef2cf1b2f6b76d53449c81ce2927cc087e7d2773f6f4cf1e0b', '6da28438261bf29ffe1b02a36c8ed8ff2ba2620df821979b458790e882bc41d8'),
    'cmd/whatomate/migration_plan.go': (None, '43bbdc7d9733e5cd0fa76a1129cab1ed70dd70be7d516061278cdd15e17e9dde'),
    'cmd/whatomate/migration_plan_pg_test.go': (None, '49dfd9e27e336b909bd6e2a698863db089bab94616dafc92830c52178cc16814'),
    'cmd/whatomate/migration_plan_relay_test.go': (None, 'f2f3b65c119c82437ea25b2015d8685242442e423a5736b72f15bab96f7326e6'),
    'cmd/whatomate/migration_plan_test.go': (None, 'c3ce07dc149bb428142d52a13c17be30be1ac95ff100886313f8d2077c5d6a49'),
    'cmd/whatomate/migration_plan_two_server_pg_test.go': (None, '5e8225306bd5e4243f9c880ef3079525977bdfee17642e73a04b4dd524f618a3'),
    'internal/database/catalog_plan_integration_test.go': (None, '7a571e81f11963e73707603af2a1fe339bd69795b9b5d3486c7acd8b5517cc5e'),
    'internal/database/data_gates.go': (None, 'fdd250d045ba4676262d8e803df14645d8bfd896906a15c12956252facf6513d'),
    'internal/database/data_gates_export_test.go': (None, 'dd231916a0e9616aef48dd49de376644b23943986fc402c7f783af8378b974d9'),
    'internal/database/data_gates_integration_test.go': (None, '71b46620c46cb1c6e1e4b9b77ec21102fa8d9f300b4f118e473f97c82be26777'),
    'internal/database/data_gates_lost_ack_integration_test.go': (None, 'e41c2456cc1a926607e16fd84a19f729202f126ad86c6b2ce503ecab840793aa'),
    'internal/database/plan.go': (None, '91ab1d7114a42d4166aa88b632d22c56291da2a39d265bba3c12770cf091d3ee'),
    'internal/database/plan_semantics_test.go': (None, '193b5e7882fdf3df5efbbd503650a0f16ed583292df088612ebf48c5480429cb'),
    'internal/database/plan_test.go': (None, 'dabb7a6982cbd7b066fa5b41e83d06f9de5e64798e251389395e4351b29f13cb'),
    'internal/database/plan_visibility.go': (None, 'b9e332f30ca4f03f25bef9e75e8e7ed8d60762c4d1e38e13ffe5d27215e3b9e4'),
    'internal/database/postgres.go': ('1b7ca0bdcfdf0696ca88aeec8f7a528834ef41cf9c8e47e338a18382719e6fb4', '3dbf326842c2b369551dd2b37c779159ffeff2bd8ea65bd4fd6d031a2d66c76d'),
    'internal/dbcatalog/builtin.go': ('9e1bf8e46f2eb061a1e841c72b476d7e40c02f0865107060995845250e6f1506', '74b0b9550591e8152f02ed2d0c5d207936f8fb02f5829d3595eedb11c30fa524'),
    'internal/dbcatalog/capture.go': ('2db6d6f6e7df718f2dbd0afcb7394e9486f1ef08567e2775bd58c1a4e5ca6c42', 'cf8f820421107e59db55af28dfdfa5e601ad8f36493f19770b9eefac25e03b06'),
    'internal/dbcatalog/capture_plan_test.go': (None, '5b6945842dd89eb2afbad34c1e74666e9d36fae8186a3745f4cdbd8ede7876b8'),
    'internal/dbcatalog/format.go': ('e374e1ae11f9dbf4b6e2e49736552aece0172be366f586ae8e964dd80339c9d7', '985251243b85ceb04948b00c988c471567df7ccd0197e4d7995b2e466c64c5da'),
    'internal/dbcatalog/plan.go': (None, 'ffe9b24f4558227f37169c5b15b6677bf7dca2931925905f7349d2b95e50e9f8'),
    'internal/dbcatalog/plan_source.go': (None, 'b2a7dc810a68606c8349635953e23f57f14d41e107c497d65f165efbd889e35c'),
    'internal/dbcatalog/plan_source_test.go': (None, 'ad78c536d2637f00f6376f334cce582a82388f0ff2d9cbe33ff48bb31db780ae'),
    'internal/dbcatalog/plan_test.go': (None, 'e125ae07909633759cfc7992b0a7e317d063684f76a06626a754a809fb0f07db'),
    'internal/dbcatalog/registry/closure_caps.json': ('02ea976eb866f8fac166b1629803f8c85afe654fec6e3198f1d2e62357c374b6', '9ccfdc7e61e9fb52d7a60a0653fc69ffdc2a82740fc0b5d728e1516fb40c8dcf'),
    'internal/dbcatalog/registry/data_steps.json': ('25041acd83c2f87921d994746d63a3b00bc937191c13d91cfa59e2d1a09f668a', '7df7b9277ca507ef634e62ae2992d464f4701f5c5084fcb93a18951d6edf019c'),
    'internal/dbcatalog/registry/migration_path.json': ('55b0b67461813c345b62a2588bc8c85d4ff50ea88b5c78732cc134488765d455', 'bde5e0c8ec946d37d60727ba6b20ede20ba6be19321e103dfe963d2615cf072e'),
    'internal/dbcatalog/registry/migration_path_test.go': ('9d566011163f0cd2ab78609334948e105f1a552d5dd26a943e9e4f7c24d13c13', '41aa23161c5467d7f4d72cd9e9eeb971e8b223830e749da49d95bc05313830e9'),
    'internal/dbcatalog/registry/registry_test.go': ('b5082386816506c38da02c508e930f115cbac28fc2071aa380c1d729bdce8c79', '8da9c1d3e143b96590ad4bdca0086a01403e6b7a03696feffebfed1e65e27ef3'),
    'release/deployment/test_ci_workflows.py': ('6131bf1587594ec935b4aca2e5f2183623dbc16461d54588d0cb6d95ec64043e', '94a85a349b24446c83227d14c88d5f7480adb906a08511e0473d537ebb8b7233'),
}
_PR18_OBSERVER_PROTECTED = {
    'internal/dbcatalog/golden/catalog.json': '17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8',
    'internal/dbcatalog/golden/history.json': '9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513',
    'internal/dbcatalog/golden/global_tables.json': 'c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3',
    'internal/dbcatalog/golden/seeds.json': 'dac095bdb1d6aff7c54fccd13bcbfa4bbff131154f9589dc5ae476331ffeca77',
    'internal/dbcatalog/golden/production-v0.json': '808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d',
    'internal/dbcatalog/shape/production-v0.sql': '48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d',
}
_PR18_OBSERVER_MIGRATIONS = (
    "generated migration closure changed",
    "migration path changed: cmd/whatomate/main.go#runRLSMigration",
    "migration path changed: internal/database/postgres.go#runRLSMigrationCoordinatorForPhase",
)
_PR18_OBSERVER_LEGACY = (
    _PR18_OBSERVER_CALL,
    "migration-path-changed:closure",
    "migration-path-changed:cmd/whatomate/main.go#runRLSMigration",
    "migration-path-changed:internal/database/postgres.go#runRLSMigrationCoordinatorForPhase",
)
_PR18_OBSERVER_REQUIREMENTS = ("compat-required", "generated-migration-closure-ci")


def _pr18_observer_blob(repo: _Repository, revision: str, path: str,
                       digest: str | None) -> bytes | None:
    raw = repo._tree(revision, path)
    if digest is None:
        _valid(raw == b"" and repo.blob(revision, path) is None)
        return None
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value) and hashlib.sha256(value).hexdigest() == digest)
    return value


def _pr18_observer_changed(repo: _Repository, base: str, head: str) -> bool:
    # Recognize absent-at-base additions and exact changed postimages before
    # a legacy-clean return, including workflow/test-only fragments. Existing
    # PR18 paths alone do not fence unrelated future none-class edits.
    for _, path in changed_paths(repo.root, base, head):
        if path not in _PR18_OBSERVER_FILES:
            continue
        before, after = _PR18_OBSERVER_FILES[path]
        if before is None and repo.blob(base, path) is None:
            return True
        value = repo.blob(head, path)
        if (type(value) is bytes and hashlib.sha256(value).hexdigest() == after
                and repo.blob(base, path) != value):
            return True
    return False


# One finite verification repair after the original PR18 product transition.
# This is a historical fingerprint under the reviewed consumer, not a policy
# supplied by a caller. Preserve the original transition and these pins when
# adding future policies. External source review binds this consumer's bytes.
_PR18_VERIFICATION_REPAIR_POLICY = "release/deployment/schema_change.py"
_PR18_VERIFICATION_REPAIR_POLICY_SHA256 = '9e0cd09c7321dd70393297f9780f7d5cf7dc9fcdc855f6f37f8cac8c49cab7a4'
_PR18_VERIFICATION_REPAIR_REASON = "pr18-verification-repair-incomplete"
_PR18_VERIFICATION_REPAIR_FILES = {'.github/workflows/compat.yml': ('56005fe2015e8385e6ecfc621a403c06a534ed9a5712e5ec4ec4f9c70407b941',
                                  '73c655c829fa36a109dfee621f1efa4529cf89ffa112599a8ff1bb8391b7eff9'),
 '.github/workflows/ship.yml': ('30663f4c8c3978d6a504e294a57d58cd66af76b2375d76ba2658623ab1613f6a',
                                '36b18f4ebe049755ced6693b93c755507c75ce9763da72ccba976168b980a74f'),
 'docs/compat.md': ('0a759a0fb3a3cf15afbdf8cb62cdbdb774b03972330cd4f2a64c535d5153c9eb',
                    '9d892fba25e0c4c58b5292d1d573f39a748032fad860103ff1e21cd8961f1682'),
 'release/compat/cli.py': ('149a4a15d22b847851b9e8d254c6043010f7663cac2ad74edfb532f6a0e52594',
                           'd72ba333dade49fa33b621e4e89d908df607cbbcf5f5a3c1acaea02c097634bd'),
 'release/compat/compat.py': ('5130a79d382e173a8c8d994cd28dd3250b879e900aa0c04dbf88219d64767625',
                              '6daef5063dfc64eb8ef7a5cb5f5b78953d493ba81dcc9be30946dab3879a54cc'),
 'release/compat/images.py': ('bfbde077b3eea4cd474dd7648b51cb01f9fd92d5c8b73380a5d9db19da402825',
                              '6ca23d0ea36937ee4049f3255411040a449aa58dae6c4bdd440ba5c35cd92c2a'),
 'release/compat/test_cli.py': ('3dc4f5be0571e7baa0612dc4f2805bc5d90c4bf6f48ce70c8bda763b9cdbb4ee',
                                'b9dce47b2ead040dd9f4ae4348f03d22d76f13c0a4a5011be399709ca171cda7'),
 'release/compat/test_cli_rollback.py': ('a658ca92d20d8ac29222a21485b5eda9b37e1ee9cc18fe5d56f9222e89e1f889',
                                         '9582964fbce43369340ddc421366b3b3e260b3d493a30463e773cabdb290fb5f'),
 'release/compat/test_compat.py': ('f389734e712e570f400d32b40d7b5b5316a1b767065db8b07c3bd5eb82a78a86',
                                   '845a7b59cd45ba9108d2bf9f6f33cab9b60f19bf806a59306d0d0e8e100cffcc'),
 'release/compat/test_images.py': ('ec605de0b31e92a2ed5532fa9f3b6b1cb5075ccdcb42955763131c9e0b9548b3',
                                   'c8f5d2c1f1ab0a74f7fcb355fe147d5eaf8e45d1d3b30da5e7df247ad3728084'),
 'release/compat/test_workflow.py': ('09cf6e091ac0f408c6741fba33fc9326d61356d344d4d7d6cd674800831739db',
                                     'ce8a183081203c181b35adde4b4ed2d45af364d929d8e3a6efb80f4ac533e20a'),
 'release/deployment/compat_collect.py': ('42694ecff0cd35013eac2c85f84442f483489d1bb345ca3fc3347fb3d73b4c7b',
                                          'b4a9d1a38be1900cb3bc9b895ee3f10f231f9726106730f243f7f779d5455b69'),
 'release/deployment/compat_envelope.py': ('0e789087e9e857996c82da851b1dd979c85bafe7f4935b0ffff20b9fedb8779e',
                                           'c0a752452ab72387a248a7a90addc844781bd512dbed0b331bfe77822a0c8586'),
 'release/deployment/compat_request.py': ('0f7d66a8f2e067d075ca10f4d3bab4c3324ff8cc3ac355a87f0b4ddfc1c5410b',
                                          'e33ec8a0405862ea7d39276e18668bd610fb7aa268aa4e74ba07f87fb3d582b9'),
 'release/deployment/fixtures/compat-workflow-p0.yml': (None,
                                                        '56005fe2015e8385e6ecfc621a403c06a534ed9a5712e5ec4ec4f9c70407b941'),
 'release/deployment/fixtures/pr18-verification-repair.json': (None,
                                                               '992accd166a964e970977964789f2c93a052b7570334f591356c09d6a9398000'),
 'release/deployment/record_resolver.py': ('9367358d7db9fffb2507d785d0256135c14e6a87590d67ab2030137d2b467c52',
                                           '3a4cd096b701ca92ac8023a834de320891620a889d1314ec90a85fd9b70e6d67'),
 'release/deployment/record_transport.py': ('125764da4008a029a07f13b69a19958a951834617085d3a1f2317f63649a14f3',
                                            'a683969db650e819fa7a4bc79d2d12dd948cedb75a715489e12ff6f6d4a49fd0'),
 'release/deployment/schema_change.py': ('832d346aeb377f9b54358b69f8fad74146927c20a6957ce66bcfdc797a06ba86',
                                         'reviewed-policy-slot'),
 'release/deployment/ship.py': ('d2d3f4c413f907293fc0f591cb18e291be1143f2ae676bb849abb5b678ae238d',
                                '91d134d2af2044ccd6679cb6b9c51ffc6926c5de0c52cfdd13a41e5ce097b921'),
 'release/deployment/ship_compat.py': ('946a7d3c0e30c96deae43e45640bd34b00e04e9720677bef314815210e1ea836',
                                       'f3db82a76e9ca8d8f4b6f44053649dcf0cc872cae360f770f141b6eebd19362c'),
 'release/deployment/ship_flow.py': ('14acc4ff9a5f1fab8cb927d61c817127efd5a781f391c5581eff32c752c0972a',
                                     'bd259294554413dd36a9c7143d07485393698e75575c46c819ffb132c1dc12ab'),
 'release/deployment/ship_outage_compat.py': ('d03ef4387fe9e9a54a34a7db13584ac5e35254ca26781fcaead9d7e7f88e9a8a',
                                              'a4d472659f2d200b9ba6f81782b16db1fcde2dc772270729f7147bd96630edee'),
 'release/deployment/ship_rollback_compat.py': ('c31bd6cd7208d5ec194b1e5212a68d02a8a2d4563bc3265b00ecbe988793b47f',
                                                '75dcf78d8e50a3b0f6a33c2e90fa8e199ce4aa52c8258ea4f12e720906f37482'),
 'release/deployment/source_git.py': ('bb3fc7dd032aac31683a1afe442c28107f4b85b0382ebd695b308ac88be1fcac',
                                      'e2b47e5be4ad1c774e3d6587008d2d98b0931e7c77b8e94630485a13b502de73'),
 'release/deployment/test_compat_collect.py': ('11b50f821b20f8cb24a35cb17aa3c0fb39678c84d1d0f5d0818f24cc06b1ca90',
                                               '4fe4620d576a317cae9265fbf4001e28d14a21514d37513b8d7eb651554426e3'),
 'release/deployment/test_compat_request.py': ('6cd08a453af3137653dd57a6971af4b169a5c42377b6627d5d99f870c70403bd',
                                               'ae86c52d025216ead56f22f16b13977f016852985e1427ae85a88bea73575a5f'),
 'release/deployment/test_pr18_verification_repair.py': (None,
                                                         '280ea8149708e1780c19f02f3625ff1a8a0cf209c8ceb65cd425d0c5a64ba761'),
 'release/deployment/test_record_resolver.py': ('fb59fb0ee39fe3acd5f15c5e56c975bf5275e4de5d5c5ea31f6f9c7330ac85fb',
                                                'd99f208bdc3ef18fe4130e1ac5a346e8f20f00633727b8ea50b1e76e4769a57a'),
 'release/deployment/test_record_transport.py': ('c411ddcad55c18d7064add4b2d7f89a2f75d10c970dc3701854db7ad231a4117',
                                                 '4c87aba34c30d05bc0dc95624155fb4f841b72d44c54d2c86be08d136080894c'),
 'release/deployment/test_record_transport_asset_retry.py': (None,
                                                             '3b76939cb1f20c8841e7cab4f5293439fb88530990b90682db3c9e5fe3867bcf'),
 'release/deployment/test_ship_compat.py': ('d4a7b4559a8b2fc074fadc79196acb0722f2a70397fd1f0a34ad9a5a60c12b5d',
                                            'd750579985000e9cbc3cf88163b8aab2a90f9114a70f43dfa2c5471f8ebae7ac'),
 'release/deployment/test_ship_outage_compat.py': ('2f41cbbb365391c1837aee705ba7323d7c8b689ad9045e002f0739844c766495',
                                                   '7c784099e2b0a07d696fca19323db40755fef2b2adf5b7b3435f507253fabcce'),
 'release/deployment/test_ship_rollback_compat.py': ('8170c47ee1e6774cbdb78295f6bf38c108d25a7612b47b6648e667f5af7b9230',
                                                     '6e4d5b977de0b789abbb78ccba6c8319c3b943a4077e156396a48f7bc9229841'),
 'release/deployment/test_ship_workflow.py': ('92c6292826a3c10f8f880387e83ad16e9fee2623cdc77ac0d95abd0a65408cf3',
                                              'b86143a5038dabdfc7f44658f946523d22c74d4a73f5cd6b58295c13a982121d'),
 'release/deployment/test_source_git.py': ('ef1a0d3b29e02f8b614b70e2334beecf6fdad7c04e1653497795fd8dbf933103',
                                           '6ea0637697cecfafabb9d48134c1dae66e0e4f6129cbff49905ac522452f6d22'),
 'release/deployment/test_stage_adapters.py': ('83c5ab1cb1c3b56a0629302e228d4edcb2804b7804f68da3979f0d096036c2c0',
                                               'eb064f00a046208501fd7470d4e638ba1aaca413a0a6aab5a56ea82ffc063721'),
 'release/deployment/test_verification_diagnostics.py': (None,
                                                         '5c513c539c17a6fd8845dbce1c342deba918d2fa812c1b45d504e5647181be37')}


def _pr18_verification_repair_blob(repo, revision, path, digest):
    if digest != "reviewed-policy-slot":
        return _pr18_observer_blob(repo, revision, path, digest)
    _valid(path == _PR18_VERIFICATION_REPAIR_POLICY)
    raw = repo._tree(revision, path)
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value))
    slot = re.compile(rb"(?m)^_PR18_VERIFICATION_REPAIR_POLICY_SHA256 = '([0-9a-f]{64})'\n")
    matches = list(slot.finditer(value))
    _valid(len(matches) == 1)
    match = matches[0]
    _valid(match.group(1).decode("ascii") == _PR18_VERIFICATION_REPAIR_POLICY_SHA256)
    canonical = value[:match.start(1)] + b"0" * 64 + value[match.end(1):]
    _valid(hashlib.sha256(canonical).hexdigest() == _PR18_VERIFICATION_REPAIR_POLICY_SHA256)
    return value


def _pr18_verification_repair_product(repo, revision):
    for path, (_, digest) in _PR18_OBSERVER_FILES.items():
        _pr18_observer_blob(repo, revision, path, digest)
    for path, digest in _PR18_OBSERVER_PROTECTED.items():
        _pr18_observer_blob(repo, revision, path, digest)
    _pr18_observer_blob(repo, revision, DORMANT,
                       "041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d")
    _pr18_observer_blob(repo, revision, "internal/dbcatalog/registry/coordinator_bindings.json",
                       "ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4")
    _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
    for path in (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                 schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH):
        _pr18_observer_blob(repo, revision, path, None)


def _pr18_verification_repair_before(repo, revision):
    _valid(bool(_PR18_VERIFICATION_REPAIR_FILES)
           and not set(_PR18_VERIFICATION_REPAIR_FILES) & set(_PR18_OBSERVER_FILES))
    for path, (old, _) in _PR18_VERIFICATION_REPAIR_FILES.items():
        _pr18_verification_repair_blob(repo, revision, path, old)
    _pr18_verification_repair_product(repo, revision)


def _pr18_verification_repair_history(repo, base, head):
    _valid(base != head and _git(repo.root, ["merge-base", base, head]).strip() == base.encode())
    raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
    _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
           and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
           and revisions[-1] == head)
    prior, edges = base, []
    for revision in revisions:
        _valid(_git(repo.root, ["rev-parse", "--verify", revision + "^1"]) == prior.encode() + b"\n")
        edges.append((prior, revision))
        prior = revision
    return tuple(edges)


def _pr18_verification_repair_changed(repo, base, head):
    # Reserve only this complete unrepaired profile, not these filenames for
    # all future releases. Full-origin admission separately proves the anchor.
    if base == head or not _PR18_VERIFICATION_REPAIR_FILES:
        return False
    try:
        _pr18_verification_repair_before(repo, base)
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return False
    if any(path in _PR18_VERIFICATION_REPAIR_FILES for _, path in changed_paths(repo.root, base, head)):
        return True
    # A repair followed by its complete undo has no net changed path. It must
    # not disappear through a clean local CI/policy fast return.
    return any(path in _PR18_VERIFICATION_REPAIR_FILES
               for prior, revision in _pr18_verification_repair_history(repo, base, head)
               for _, path in changed_paths(repo.root, prior, revision))


def _pr18_verification_repair_edge(repo, base, head):
    _pr18_verification_repair_before(repo, base)
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_VERIFICATION_REPAIR_FILES.items())
    _valid(sorted(changed_paths(repo.root, base, head)) == expected)
    _pr18_verification_repair_product(repo, head)
    for path, (_, new) in _PR18_VERIFICATION_REPAIR_FILES.items():
        _pr18_verification_repair_blob(repo, head, path, new)


def _pr18_verification_repair_local(repo, base, head):
    _pr18_verification_repair_before(repo, base)
    repaired = False
    for prior, revision in _pr18_verification_repair_history(repo, base, head):
        if changed_paths(repo.root, prior, revision):
            _valid(not repaired)
            _pr18_verification_repair_edge(repo, prior, revision)
            repaired = True
    _valid(repaired)


# One finite managed-owner observation correction after the exact PR18 repair.
# These fixed source fingerprints do not authorize arbitrary later edits.
_PR18_MANAGED_OWNER_POLICY_SHA256 = '4d9c597c2e6f689a1190aee3725765229fa0899f6abc35501f18fde011fc97cb'
_PR18_MANAGED_OWNER_REASON = "pr18-managed-owner-repair-incomplete"
_PR18_MANAGED_OWNER_FILES = {'cmd/whatomate/migration_plan.go': ('43bbdc7d9733e5cd0fa76a1129cab1ed70dd70be7d516061278cdd15e17e9dde',
                                     '46e40d028a5b341b1ff966c5f51defde6c89d63850093f6aba828bf5ecff5fc8'),
 'cmd/whatomate/migration_plan_managed_owner_admission_pg_test.go': (None,
                                                                     '1f65a0b7da0d8b0637416e64bf061f7fd7fbdbcf27dd95799c06ea1fb04b6abb'),
 'cmd/whatomate/migration_plan_managed_owner_pg_test.go': (None,
                                                           'b8cc9cc0090e03a06d657f1af5d70bf97c60dabd8dd0f6e65aa211007bd8d5d4'),
 'docs/catalog.md': ('7fec0d54292d16d617eb5b596b539f1ffad55091f1d88b9b5b3b9af235eb7fd7',
                     '29f182362f3128732ea55f6a3ba43dcc099e84fb2f1e3b3a599db4b3cbe115dc'),
 'internal/database/data_gates.go': ('fdd250d045ba4676262d8e803df14645d8bfd896906a15c12956252facf6513d',
                                     'b8cb4b1b608f87069e2724ae29d4b6b38bcdbcf85e5c2bf085734015902953cd'),
 'internal/database/plan.go': ('91ab1d7114a42d4166aa88b632d22c56291da2a39d265bba3c12770cf091d3ee',
                               '83d6f4e83ced0c9a02f3ce18470e3da54914cb63c77f70f9a8fda7cb44c68365'),
 'internal/database/plan_test.go': ('dabb7a6982cbd7b066fa5b41e83d06f9de5e64798e251389395e4351b29f13cb',
                                    '083ed1514dda2ddb0d7ef5081fd471e1de34412ebba44f64fb954dcc7ca7f763'),
 'internal/dbcatalog/registry/migration_path.json': ('bde5e0c8ec946d37d60727ba6b20ede20ba6be19321e103dfe963d2615cf072e',
                                                     '4acecb4e3caed65bb71f8d61ea2b3e760129523bfc599130fa3aee5ed421969d'),
 'release/deployment/fixtures/pr18-managed-owner-repair.json': (None,
                                                                'd57a0e1437923340549b8ca0a64166ba7d83e3ea256fe670277d3fd78077f525'),
 'release/deployment/schema_change.py': ('421802386bdfcc1948b8a33f976660281ed426ed11ff7f81a75f96a3bb688917',
                                         'reviewed-managed-owner-policy-slot'),
 'release/deployment/test_pr18_managed_owner_repair.py': (None,
                                                          'c0d7253bbd3f3873ef659710d01275d5264dbaa11203a76f2a78ac0e0b805f8e'),
 'release/deployment/test_pr18_verification_repair.py': ('280ea8149708e1780c19f02f3625ff1a8a0cf209c8ceb65cd425d0c5a64ba761',
                                                         'eed47103a70d533d7d8e2687111025ff469bd05f264d9e9bbbe0defa826fc9e1')}


def _pr18_managed_owner_blob(repo, revision, path, digest):
    if digest != "reviewed-managed-owner-policy-slot":
        return _pr18_observer_blob(repo, revision, path, digest)
    _valid(path == _PR18_VERIFICATION_REPAIR_POLICY)
    raw = repo._tree(revision, path)
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value))
    slot = re.compile(rb"(?m)^_PR18_MANAGED_OWNER_POLICY_SHA256 = '([0-9a-f]{64})'\n")
    matches = list(slot.finditer(value))
    _valid(len(matches) == 1)
    match = matches[0]
    _valid(match.group(1).decode("ascii") == _PR18_MANAGED_OWNER_POLICY_SHA256)
    canonical = value[:match.start(1)] + b"0" * 64 + value[match.end(1):]
    _valid(hashlib.sha256(canonical).hexdigest() == _PR18_MANAGED_OWNER_POLICY_SHA256)
    return value


def _pr18_managed_owner_before(repo, revision):
    _valid(bool(_PR18_MANAGED_OWNER_FILES))
    _pr18_verification_repair_product(repo, revision)
    for path, (_, digest) in _PR18_VERIFICATION_REPAIR_FILES.items():
        _pr18_verification_repair_blob(repo, revision, path, digest)
    for path, (digest, _) in _PR18_MANAGED_OWNER_FILES.items():
        _pr18_managed_owner_blob(repo, revision, path, digest)


def _pr18_managed_owner_changed(repo, base, head):
    if base == head or not _PR18_MANAGED_OWNER_FILES:
        return False
    try:
        _pr18_managed_owner_before(repo, base)
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return False
    if any(path in _PR18_MANAGED_OWNER_FILES for _, path in changed_paths(repo.root, base, head)):
        return True
    return any(path in _PR18_MANAGED_OWNER_FILES
               for prior, revision in _pr18_verification_repair_history(repo, base, head)
               for _, path in changed_paths(repo.root, prior, revision))


def _pr18_managed_owner_edge(repo, base, head):
    _pr18_managed_owner_before(repo, base)
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_MANAGED_OWNER_FILES.items())
    _valid(sorted(changed_paths(repo.root, base, head)) == expected)
    for path, (_, digest) in _PR18_MANAGED_OWNER_FILES.items():
        _pr18_managed_owner_blob(repo, head, path, digest)


def _pr18_managed_owner_local(repo, base, head):
    _pr18_managed_owner_before(repo, base)
    repaired = False
    for prior, revision in _pr18_verification_repair_history(repo, base, head):
        if changed_paths(repo.root, prior, revision):
            _valid(not repaired)
            _pr18_managed_owner_edge(repo, prior, revision)
            repaired = True
    _valid(repaired)


# One exact run-metadata consistency repair after the historical managed edge.
_PR18_RUN_METADATA_REPAIR_POLICY_SHA256 = '46fc37c0f5717fc2688434884c359a6b23db492a8062c7bb1307b4af00d070e6'
_PR18_RUN_METADATA_REPAIR_REASON = "pr18-run-metadata-repair-incomplete"
_PR18_RUN_METADATA_REPAIR_FILES = {'release/deployment/compat_collect.py': ('b4a9d1a38be1900cb3bc9b895ee3f10f231f9726106730f243f7f779d5455b69',
                                          '716667cbcc15af9bbc15b21cd8f15976429502a4c99219662d2f0f232e5363ec'),
 'release/deployment/compat_rollback_collect.py': ('21eb2ddce2b6d12cb90b10331262f5d0382845b544cf9c4769e48873b78faf6d',
                                                   '519177f147065c3cd9445ed7c614c85f537e04a6d041da1fecf0fdf84dde7ba6'),
 'release/deployment/schema_change.py': ('39f5cc68495cb53ab01129ad40d0817bbf796b6cff173644a0ac5028937bccf6',
                                         'reviewed-run-metadata-policy-slot'),
 'release/deployment/test_compat_collect.py': ('4fe4620d576a317cae9265fbf4001e28d14a21514d37513b8d7eb651554426e3',
                                               'c73010ee0544470b37816d21a2d06243fa551fba66d505d651924c1319d279e3'),
 'release/deployment/test_compat_rollback_collect.py': ('b4f984089d57a9dc2fc5ebc790e692f2b693538e91508ad547b33a9cbfaa0a6e',
                                                        'cb24389b719dc04b442d6116962cd1d0441f446274ea51c0f328f70983b2c8ca'),
 'release/deployment/test_pr18_managed_owner_repair.py': ('c0d7253bbd3f3873ef659710d01275d5264dbaa11203a76f2a78ac0e0b805f8e',
                                                          '9aeae54eb05bf150b7db3d82622698388cf149196c60091163527531d8bcb70c'),
 'release/deployment/test_pr18_run_metadata_repair.py': (None,
                                                         'ebd8f028d75e8ad095ffc031ab02e8bbe078a8dadbadf17d5ede80d2d163b49f'),
 'release/deployment/test_ship_outage_compat.py': ('7c784099e2b0a07d696fca19323db40755fef2b2adf5b7b3435f507253fabcce',
                                                  '151d087b92a362ee5e6de8469e56dd51332a238e11d5ffd94178eb8192cdb134')}


def _pr18_run_metadata_blob(repo, revision, path, digest):
    if digest != "reviewed-run-metadata-policy-slot":
        return _pr18_observer_blob(repo, revision, path, digest)
    _valid(path == _PR18_VERIFICATION_REPAIR_POLICY)
    raw = repo._tree(revision, path)
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value))
    slot = re.compile(rb"(?m)^_PR18_RUN_METADATA_REPAIR_POLICY_SHA256 = '([0-9a-f]{64})'\n")
    matches = list(slot.finditer(value))
    _valid(len(matches) == 1)
    match = matches[0]
    _valid(match.group(1).decode("ascii") == _PR18_RUN_METADATA_REPAIR_POLICY_SHA256)
    canonical = value[:match.start(1)] + b"0" * 64 + value[match.end(1):]
    _valid(hashlib.sha256(canonical).hexdigest() == _PR18_RUN_METADATA_REPAIR_POLICY_SHA256)
    return value


def _pr18_run_metadata_before(repo, revision):
    _valid(len(_PR18_RUN_METADATA_REPAIR_FILES) == 8)
    # Later fixed edges supersede overlapping historical postimages. In
    # particular, never validate the managed policy using the older slot.
    expected = {path: digest for path, (_, digest) in _PR18_OBSERVER_FILES.items()}
    expected.update({path: digest for path, (_, digest) in _PR18_VERIFICATION_REPAIR_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_MANAGED_OWNER_FILES.items()})
    for path, digest in expected.items():
        _pr18_managed_owner_blob(repo, revision, path, digest)
    for path, digest in _PR18_OBSERVER_PROTECTED.items():
        _pr18_observer_blob(repo, revision, path, digest)
    _pr18_observer_blob(repo, revision, DORMANT,
                       "041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d")
    _pr18_observer_blob(repo, revision, "internal/dbcatalog/registry/coordinator_bindings.json",
                       "ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4")
    _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
    for path in (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                 schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH):
        _pr18_observer_blob(repo, revision, path, None)
    for path, (digest, _) in _PR18_RUN_METADATA_REPAIR_FILES.items():
        _pr18_observer_blob(repo, revision, path, digest)


def _pr18_run_metadata_remember_history(repo, base, head, raw):
    """Retain a successful immutable read, without replacing its old caller."""
    if (not all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
                for value in (base, head))
            or type(raw) is not bytes or len(raw) > MAX_HISTORY_COMMITS * 41):
        return
    try:
        revisions = raw.decode("ascii").splitlines()
    except UnicodeError:
        return
    if (len(revisions) > MAX_HISTORY_COMMITS or len(revisions) != len(set(revisions))
            or not all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
            or not ((base == head and not revisions)
                    or (base != head and revisions and revisions[-1] == head))):
        return
    # Both legacy callers still perform their original reads and validation.
    # This is invocation-local read custody, not cached admission or failure.
    key = (base, head)
    retained = getattr(repo, "_run_metadata_history_reads", {})
    _valid(key not in retained or retained[key] == raw)
    retained[key] = raw
    repo._run_metadata_history_reads = retained


def _pr18_run_metadata_changed(repo, base, head):
    if base == head or not _PR18_RUN_METADATA_REPAIR_FILES:
        return False
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    key = (base, head)
    checked = getattr(repo, "_run_metadata_introductions_checked", set())
    if key in checked:
        # The invocation-local success cache never replaces the active reader's
        # deadline/failure checks, and never accepts mutable revision aliases.
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        return False
    marker, = (path for path, (old, _) in _PR18_RUN_METADATA_REPAIR_FILES.items()
               if old is None)
    if repo._tree(base, marker):
        # A completed/marked base has no permanent path reservation.
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        checked.add(key)
        repo._run_metadata_introductions_checked = checked
        return False
    # This exact historical blob OID is only a cheap negative discriminator.
    # A match still requires the complete before-profile and SHA256 proof.
    policy_tree = (b"100644 blob 02e2dee827a2e1ce3109cdd5229cc3c511183989\t"
                   + _PR18_VERIFICATION_REPAIR_POLICY.encode() + b"\x00")
    before = False
    if repo._tree(base, _PR18_VERIFICATION_REPAIR_POLICY) == policy_tree:
        try:
            _pr18_run_metadata_before(repo, base)
        except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
            pass
        else:
            before = True
    if not before:
        raw = getattr(repo, "_run_metadata_history_reads", {}).get(key)
        if raw is None:
            raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
        else:
            # Reuse is confined to this invocation and the current source reader.
            _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
        revisions = raw.decode("ascii").splitlines()
        _valid(len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
               and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
               and (not revisions or revisions[-1] == head))
        # Absence grants no repair authority or new ordinary-lineage rule.
        # Only a range containing the marker needs the strict edge proof.
        if any(repo._tree(revision, marker) for revision in revisions):
            present = False
            # Endpoint equality cannot hide introduction followed by undo.
            for prior, revision in _pr18_verification_repair_history(repo, base, head):
                now = bool(repo._tree(revision, marker))
                if now and not present:
                    _pr18_run_metadata_edge(repo, prior, revision)
                present = now
        checked.add(key)
        repo._run_metadata_introductions_checked = checked
        return False
    if any(path in _PR18_RUN_METADATA_REPAIR_FILES for _, path in changed_paths(repo.root, base, head)):
        return True
    # Observe complete undo as well as net changes before clean fast returns.
    return any(path in _PR18_RUN_METADATA_REPAIR_FILES
               for prior, revision in _pr18_verification_repair_history(repo, base, head)
               for _, path in changed_paths(repo.root, prior, revision))


def _pr18_run_metadata_edge(repo, base, head):
    _pr18_run_metadata_before(repo, base)
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_RUN_METADATA_REPAIR_FILES.items())
    _valid(sorted(changed_paths(repo.root, base, head)) == expected)
    for path, (_, digest) in _PR18_RUN_METADATA_REPAIR_FILES.items():
        _pr18_run_metadata_blob(repo, head, path, digest)


def _pr18_run_metadata_local(repo, base, head):
    _pr18_run_metadata_before(repo, base)
    repaired = False
    for prior, revision in _pr18_verification_repair_history(repo, base, head):
        if changed_paths(repo.root, prior, revision):
            _valid(not repaired)
            _pr18_run_metadata_edge(repo, prior, revision)
            repaired = True
    _valid(repaired)


# One exact managed-owner replication observation correction after the
# historical run-metadata edge. Older fingerprints remain immutable.
_PR18_MANAGED_REPLICATION_POLICY_SHA256 = '005cb7d53546eeaa53861ff4e321fc243d0168077fbe3e3b9c5abe8ae03f2048'
_PR18_MANAGED_REPLICATION_REASON = "pr18-managed-replication-repair-incomplete"
_PR18_MANAGED_REPLICATION_MARKER = "release/deployment/test_pr18_managed_replication_repair.py"
_PR18_MANAGED_REPLICATION_FILES = {'cmd/whatomate/migration_plan_managed_owner_admission_pg_test.go': ('1f65a0b7da0d8b0637416e64bf061f7fd7fbdbcf27dd95799c06ea1fb04b6abb',
                                                                     '3e4ef3d4ffc94f4be16e03eb2c1efef227f5a7869e95d8a586b846e3a78616d5'),
 'cmd/whatomate/migration_plan_managed_owner_pg_test.go': ('b8cc9cc0090e03a06d657f1af5d70bf97c60dabd8dd0f6e65aa211007bd8d5d4',
                                                           'faa099ea8cc47569f1bbaddabf03285b7d0bde8f1d797abd9908a3407e48e215'),
 'docs/catalog.md': ('29f182362f3128732ea55f6a3ba43dcc099e84fb2f1e3b3a599db4b3cbe115dc',
                     '4c2641310c4f316af3a7553579beae81df5431688bfef7687675784fac40dae1'),
 'internal/database/data_gates.go': ('b8cb4b1b608f87069e2724ae29d4b6b38bcdbcf85e5c2bf085734015902953cd',
                                     'a56b6a9281cddb47f845ccd9a9b5920ecfb061f7c87d12d35d54f27c29977d06'),
 'internal/database/plan.go': ('83d6f4e83ced0c9a02f3ce18470e3da54914cb63c77f70f9a8fda7cb44c68365',
                               '922b3f623817045bbe0f45a870bd910844a697a0289d23f94ab6503bed9df67b'),
 'internal/dbcatalog/registry/migration_path.json': ('4acecb4e3caed65bb71f8d61ea2b3e760129523bfc599130fa3aee5ed421969d',
                                                     '08618e383eed2ab9eb771918b0ef618efa2ecce8edb99cfd476bd18980938ba6'),
 'release/deployment/fixtures/pr18-managed-replication-repair.json': (None,
                                                                      '59da4fe651b89d4ef544c07f6e6990db795ce8f255f997c15c88c4f8ed7b0977'),
 'release/deployment/schema_change.py': ('3ca123b80da6c8c22dc12bd962d94322135bac6b8da306f99636debd7e3d6c28',
                                         'reviewed-managed-replication-policy-slot'),
 'release/deployment/test_pr18_managed_replication_repair.py': (None, '023f81db0b9ac67724e799234658ec2bee8001479156737dd55897bb64d72393'),
 'release/deployment/test_pr18_run_metadata_repair.py': ('ebd8f028d75e8ad095ffc031ab02e8bbe078a8dadbadf17d5ede80d2d163b49f',
                                                         '750c846b773a4b8cc8d5129c0136f9d3b59dd304d42233f1e83ff30c4a8c384c')}


def _pr18_managed_replication_blob(repo, revision, path, digest):
    if digest != "reviewed-managed-replication-policy-slot":
        return _pr18_observer_blob(repo, revision, path, digest)
    _valid(path == _PR18_VERIFICATION_REPAIR_POLICY)
    raw = repo._tree(revision, path)
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value))
    slot = re.compile(rb"(?m)^_PR18_MANAGED_REPLICATION_POLICY_SHA256 = '([0-9a-f]{64})'\n")
    matches = list(slot.finditer(value))
    _valid(len(matches) == 1)
    match = matches[0]
    _valid(match.group(1).decode("ascii") == _PR18_MANAGED_REPLICATION_POLICY_SHA256)
    canonical = value[:match.start(1)] + b"0" * 64 + value[match.end(1):]
    _valid(hashlib.sha256(canonical).hexdigest() == _PR18_MANAGED_REPLICATION_POLICY_SHA256)
    return value


def _pr18_managed_replication_reuse_before_blob(repo, revision, path, digest, *, proof_only=False):
    """Reuse only a proven immutable preimage; the caller checks reader liveness.

    The public-owner finite edge may verify a matching immutable value without
    creating another revision/path buffer. Historical callers retain exactly
    the charged materialization path below; misses always return to normal IO.
    """
    key = (revision, path)
    if key in repo.cache or re.fullmatch(r"[0-9a-f]{64}", digest or "") is None:
        return
    tree = repo._tree(revision, path)
    match = re.fullmatch(rb"100644 blob ([0-9a-f]{40})\t" + re.escape(path.encode()) + rb"\x00", tree)
    if match is None:
        return
    for prior, value in tuple(repo.cache.items()):
        if type(value) is not bytes or repo._trees.get(prior) != tree:
            continue
        # A cache row is usable only under the fresh current mode/path/OID,
        # the reviewed SHA256, and the object's own Git blob identity.
        if (hashlib.sha256(value).hexdigest() != digest
                or hashlib.sha1(b"blob " + str(len(value)).encode() + b"\x00" + value).hexdigest().encode()
                != match.group(1)):
            continue
        _valid(len(value) <= MAX_ARTIFACT_BYTES)
        if proof_only:
            _valid(repo.total <= 64 * 1024 * 1024)
            return value
        repo.total += len(value)
        _valid(repo.total <= 64 * 1024 * 1024)
        repo.cache[key] = value
        return



def _pr18_managed_replication_prime_protected(repo, base, head):
    """Prime six fixed protected inputs, never a history or admission verdict."""
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
    if base == head:
        return
    raw = getattr(repo, "_run_metadata_history_reads", {}).get((base, head))
    if raw is None:
        return
    _valid(type(raw) is bytes and len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
           and all(re.fullmatch(r"[0-9a-f]{40}", revision) and revision != base for revision in revisions)
           and revisions[-1] == head)
    marker = _PR18_MANAGED_REPLICATION_MARKER
    if not repo._tree(head, marker):
        return
    _pr18_managed_replication_blob(repo, head, marker, _PR18_MANAGED_REPLICATION_FILES[marker][1])
    # Reuse requires an already verified raw value, a fresh matching strict
    # tree identity, and both content hashes. Misses do not fetch raw blobs.
    # Each newly cached revision/path retains the original logical byte charge.
    # The final observer transition still proves every parent and exact edge.
    for revision in (base, *revisions):
        for path, digest in _PR18_OBSERVER_PROTECTED.items():
            _pr18_managed_replication_reuse_before_blob(repo, revision, path, digest)


def _pr18_managed_replication_before(repo, revision, *, public_owner_fixture=False):
    _valid(type(public_owner_fixture) is bool)
    _valid(len(_PR18_MANAGED_REPLICATION_FILES) == 10)
    # Even an entirely cached profile must observe the active reader budget.
    _valid(_git(repo.root, ["cat-file", "-t", revision]) == b"commit\n")
    # Replay the latest historical postimage at overlapping paths, including
    # the run-metadata policy's own historical slot, never an older slot.
    expected = {path: digest for path, (_, digest) in _PR18_OBSERVER_FILES.items()}
    expected.update({path: digest for path, (_, digest) in _PR18_VERIFICATION_REPAIR_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_MANAGED_OWNER_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_RUN_METADATA_REPAIR_FILES.items()})
    for path, digest in expected.items():
        reuse_digest = _PR18_MANAGED_REPLICATION_FILES.get(path, (digest, None))[0]
        # Only the final ordered edge into the selected public-owner predecessor
        # can avoid this one duplicate historical fixture buffer. The same strict
        # current tree and both hashes prove a hit; every miss keeps normal IO.
        if (public_owner_fixture
                and path == "release/deployment/fixtures/pr18-verification-repair.json"
                and reuse_digest == "992accd166a964e970977964789f2c93a052b7570334f591356c09d6a9398000"
                and _pr18_managed_replication_reuse_before_blob(
                    repo, revision, path, reuse_digest, proof_only=True) is not None):
            continue
        _pr18_managed_replication_reuse_before_blob(repo, revision, path, reuse_digest)
        _pr18_run_metadata_blob(repo, revision, path, digest)
    for path, digest in _PR18_OBSERVER_PROTECTED.items():
        _pr18_observer_blob(repo, revision, path, digest)
    _pr18_observer_blob(repo, revision, DORMANT,
                       "041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d")
    _pr18_observer_blob(repo, revision, "internal/dbcatalog/registry/coordinator_bindings.json",
                       "ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4")
    _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
    for path in (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                 schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH):
        _pr18_observer_blob(repo, revision, path, None)
    for path, (digest, _) in _PR18_MANAGED_REPLICATION_FILES.items():
        _pr18_observer_blob(repo, revision, path, digest)


def _pr18_managed_replication_changed(repo, base, head):
    if base == head or not _PR18_MANAGED_REPLICATION_FILES:
        return False
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    key = (base, head)
    checked = getattr(repo, "_managed_replication_introductions_checked", set())
    if key in checked:
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        return False
    marker = _PR18_MANAGED_REPLICATION_MARKER
    if repo._tree(base, marker):
        # This marker is not a permanent path reservation. Full-origin
        # admission separately replays every prior nonempty edge.
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        checked.add(key)
        repo._managed_replication_introductions_checked = checked
        return False
    policy_tree = (b"100644 blob 275140b1ababc4df84a6e47ef6b4844f2d0c674f\t"
                   + _PR18_VERIFICATION_REPAIR_POLICY.encode() + b"\x00")
    before = False
    if repo._tree(base, _PR18_VERIFICATION_REPAIR_POLICY) == policy_tree:
        try:
            _pr18_managed_replication_before(repo, base)
        except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
            pass
        else:
            before = True
    if not before:
        # Both existing migration/registry readers retain their own reads and
        # validation. Reuse only successful immutable bytes from this invocation.
        raw = getattr(repo, "_run_metadata_history_reads", {}).get(key)
        if raw is None:
            raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
        else:
            _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
        revisions = raw.decode("ascii").splitlines()
        _valid(len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
               and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
               and (not revisions or revisions[-1] == head))
        if any(repo._tree(revision, marker) for revision in revisions):
            present = False
            for prior, revision in _pr18_verification_repair_history(repo, base, head):
                now = bool(repo._tree(revision, marker))
                if now and not present:
                    _pr18_managed_replication_edge(repo, prior, revision)
                present = now
        checked.add(key)
        repo._managed_replication_introductions_checked = checked
        return False
    # A completed predecessor's test remains an ordinary editable test path.
    # Once another repair path triggers admission, its exact wrapper bytes are
    # still mandatory in the ten-path atomic edge, with no prefix or suffix.
    triggers = _PR18_MANAGED_REPLICATION_FILES.keys() - {
        "release/deployment/test_pr18_run_metadata_repair.py"}
    if any(path in triggers for _, path in changed_paths(repo.root, base, head)):
        return True
    return any(path in triggers
               for prior, revision in _pr18_verification_repair_history(repo, base, head)
               for _, path in changed_paths(repo.root, prior, revision))


def _pr18_managed_replication_edge(repo, base, head, *, public_owner_predecessor=None):
    if public_owner_predecessor is None:
        _pr18_managed_replication_before(repo, base)
    else:
        _valid(type(public_owner_predecessor) is str
               and re.fullmatch(r"[0-9a-f]{40}", public_owner_predecessor) is not None
               and head == public_owner_predecessor)
        _pr18_managed_replication_before(repo, base, public_owner_fixture=True)
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_MANAGED_REPLICATION_FILES.items())
    _valid(sorted(changed_paths(repo.root, base, head)) == expected)
    for path, (_, digest) in _PR18_MANAGED_REPLICATION_FILES.items():
        _pr18_managed_replication_blob(repo, head, path, digest)


def _pr18_managed_replication_local(repo, base, head):
    _pr18_managed_replication_before(repo, base)
    repaired = False
    for prior, revision in _pr18_verification_repair_history(repo, base, head):
        if changed_paths(repo.root, prior, revision):
            _valid(not repaired)
            _pr18_managed_replication_edge(repo, prior, revision)
            repaired = True
    _valid(repaired)



# One exact planner-local pg_database_owner observation correction after the
# immutable managed-replication edge. No capture/SQL/role/schema semantics change.
_PR18_PLANNER_PUBLIC_OWNER_POLICY_SHA256 = '9a4305867ee9755dacade389dca70dc1f8abde438f64ebddc5396a4330d71e13'
_PR18_PLANNER_PUBLIC_OWNER_REASON = "pr18-planner-public-owner-repair-incomplete"
_PR18_PLANNER_PUBLIC_OWNER_MARKER = "release/deployment/test_pr18_planner_public_owner_repair.py"
_PR18_PLANNER_PUBLIC_OWNER_FILES = {'cmd/whatomate/migration_plan.go': ('46e40d028a5b341b1ff966c5f51defde6c89d63850093f6aba828bf5ecff5fc8', 'f43155be4be09f3bb6ace6e239dac52d5aeb88590a775a00e89fa5d27d03d325'), 'cmd/whatomate/migration_plan_public_owner_pg_test.go': (None, '6d09d965c5e02edb45882bfe01005499a6b9bf2f112105b8569819c29557bb78'), 'internal/dbcatalog/registry/migration_path.json': ('08618e383eed2ab9eb771918b0ef618efa2ecce8edb99cfd476bd18980938ba6', '08dc20242f40e66f0df7e94ddc4c22472b639136188184f08fba6eaf29bef241'), 'release/deployment/schema_change.py': ('ad898c247153afaa43da9a78a52ba8739f40900e373bba468f14587790078c12', 'reviewed-planner-public-owner-policy-slot'), 'release/deployment/test_pr18_managed_replication_repair.py': ('023f81db0b9ac67724e799234658ec2bee8001479156737dd55897bb64d72393', '0b0254a11d0518ad43fe23da18391931ddc4fc06e5c3d203e4857148f4e397f7'), 'release/deployment/test_pr18_planner_public_owner_repair.py': (None, 'f83cbeef1683161b02a82e797af9faf3eaa2012169e37478549ea8217ee3dbfd'), 'release/deployment/fixtures/pr18-planner-public-owner-repair.json': (None, '8837561cfe9f11d1aea647fa1dafec24b6e80579891c33a21598e1ee7579d1ad')}


def _pr18_planner_public_owner_blob(repo, revision, path, digest):
    if digest != "reviewed-planner-public-owner-policy-slot":
        return _pr18_observer_blob(repo, revision, path, digest)
    _valid(path == _PR18_VERIFICATION_REPAIR_POLICY)
    raw = repo._tree(revision, path)
    _valid(re.fullmatch(rb"100644 blob [0-9a-f]{40}\t" + re.escape(path.encode()) + rb"\x00", raw) is not None)
    value = repo.blob(revision, path)
    _valid(type(value) is bytes and bool(value))
    slot = re.compile(rb"(?m)^_PR18_PLANNER_PUBLIC_OWNER_POLICY_SHA256 = '([0-9a-f]{64})'\n")
    matches = list(slot.finditer(value))
    _valid(len(matches) == 1)
    match = matches[0]
    _valid(match.group(1).decode("ascii") == _PR18_PLANNER_PUBLIC_OWNER_POLICY_SHA256)
    canonical = value[:match.start(1)] + b"0" * 64 + value[match.end(1):]
    _valid(hashlib.sha256(canonical).hexdigest() == _PR18_PLANNER_PUBLIC_OWNER_POLICY_SHA256)
    return value


def _pr18_planner_public_owner_protected(repo, revision, path, digest):
    """Prove only an unchanged protected value without a duplicate buffer."""
    _valid(path in _PR18_OBSERVER_PROTECTED and digest == _PR18_OBSERVER_PROTECTED[path])
    value = _pr18_managed_replication_reuse_before_blob(
        repo, revision, path, digest, proof_only=True)
    if value is None:
        return _pr18_observer_blob(repo, revision, path, digest)
    return value


def _pr18_planner_public_owner_protected_predecessor(repo, base, head, raw):
    """Select one exact new-edge predecessor; this is never an admission proof.

    The final ordered transition must still prove its complete old profile and
    the atomic new edge. This only avoids another buffer for the six unchanged
    protected values at that predecessor; every value is checked in that loop.
    """
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
    _valid(type(raw) is bytes and len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
           and all(re.fullmatch(r"[0-9a-f]{40}", revision) and revision != base for revision in revisions)
           and revisions[-1] == head)
    marker = _PR18_PLANNER_PUBLIC_OWNER_MARKER
    _pr18_planner_public_owner_blob(repo, head, marker, _PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])
    prior, present, selected = base, bool(repo._tree(base, marker)), None
    policy = _PR18_VERIFICATION_REPAIR_POLICY
    expected_tree = b"100644 blob 9a6e2b136e3a3db7b038ea056e7adaee14192449\t" + policy.encode() + b"\x00"
    for revision in revisions:
        now = bool(repo._tree(revision, marker))
        if now and not present:
            _valid(selected is None and repo._tree(prior, policy) == expected_tree)
            _pr18_planner_public_owner_blob(repo, prior, policy, _PR18_PLANNER_PUBLIC_OWNER_FILES[policy][0])
            _pr18_planner_public_owner_blob(repo, revision, marker, _PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])
            _valid(_git(repo.root, ["rev-parse", "--verify", revision + "^1"]) == prior.encode() + b"\n")
            selected = prior
        prior, present = revision, now
    return selected


def _pr18_planner_public_owner_prime_protected(repo, base, head):
    """Keep charged historical priming; new-edge proof adds no duplicate buffer."""
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
    if base == head:
        return
    raw = getattr(repo, "_run_metadata_history_reads", {}).get((base, head))
    if raw is None:
        return
    _valid(type(raw) is bytes and len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
           and all(re.fullmatch(r"[0-9a-f]{40}", revision) and revision != base for revision in revisions)
           and revisions[-1] == head)
    marker = _PR18_PLANNER_PUBLIC_OWNER_MARKER
    _pr18_planner_public_owner_blob(repo, head, marker, _PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])
    protected_predecessor = _pr18_planner_public_owner_protected_predecessor(repo, base, head, raw)
    for revision in (base, *revisions):
        if revision == protected_predecessor:
            continue
        if repo._tree(revision, marker):
            _pr18_planner_public_owner_blob(repo, revision, marker, _PR18_PLANNER_PUBLIC_OWNER_FILES[marker][1])
            continue
        for path, digest in _PR18_OBSERVER_PROTECTED.items():
            _pr18_managed_replication_reuse_before_blob(repo, revision, path, digest)


def _pr18_planner_public_owner_before(repo, revision):
    _valid(len(_PR18_PLANNER_PUBLIC_OWNER_FILES) == 7)
    _valid(_git(repo.root, ["cat-file", "-t", revision]) == b"commit\n")
    expected = {path: digest for path, (_, digest) in _PR18_OBSERVER_FILES.items()}
    expected.update({path: digest for path, (_, digest) in _PR18_VERIFICATION_REPAIR_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_MANAGED_OWNER_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_RUN_METADATA_REPAIR_FILES.items()})
    expected.update({path: digest for path, (_, digest) in _PR18_MANAGED_REPLICATION_FILES.items()})
    for path, digest in expected.items():
        before_digest = _PR18_PLANNER_PUBLIC_OWNER_FILES.get(path, (digest, None))[0]
        value = _pr18_managed_replication_reuse_before_blob(
            repo, revision, path, before_digest, proof_only=True)
        if value is None:
            _pr18_managed_replication_blob(repo, revision, path, digest)
    for path, digest in _PR18_OBSERVER_PROTECTED.items():
        _pr18_planner_public_owner_protected(repo, revision, path, digest)
    _pr18_planner_public_owner_blob(repo, revision, DORMANT,
                       "041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d")
    _pr18_planner_public_owner_blob(repo, revision, "internal/dbcatalog/registry/coordinator_bindings.json",
                       "ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4")
    _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
    for path in (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                 schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH):
        _pr18_observer_blob(repo, revision, path, None)
    for path, (digest, _) in _PR18_PLANNER_PUBLIC_OWNER_FILES.items():
        # Only the just-proven predecessor profile can reuse an immutable
        # buffer here. New absent inputs still take the ordinary absence proof.
        if path in expected and _pr18_managed_replication_reuse_before_blob(
                repo, revision, path, digest, proof_only=True) is not None:
            continue
        _pr18_planner_public_owner_blob(repo, revision, path, digest)


def _pr18_planner_public_owner_changed(repo, base, head):
    if base == head or not _PR18_PLANNER_PUBLIC_OWNER_FILES:
        return False
    _valid(all(type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value)
               for value in (base, head)))
    key = (base, head)
    checked = getattr(repo, "_planner_public_owner_introductions_checked", set())
    if key in checked:
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        return False
    marker = _PR18_PLANNER_PUBLIC_OWNER_MARKER
    if repo._tree(base, marker):
        # This marker is not a permanent path reservation. Full-origin
        # admission separately replays every prior nonempty edge.
        _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        checked.add(key)
        repo._planner_public_owner_introductions_checked = checked
        return False
    policy_tree = (b"100644 blob 9a6e2b136e3a3db7b038ea056e7adaee14192449\t"
                   + _PR18_VERIFICATION_REPAIR_POLICY.encode() + b"\x00")
    before = False
    if repo._tree(base, _PR18_VERIFICATION_REPAIR_POLICY) == policy_tree:
        try:
            _pr18_planner_public_owner_before(repo, base)
        except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
            pass
        else:
            before = True
    if not before:
        # Both existing migration/registry readers retain their own reads and
        # validation. Reuse only successful immutable bytes from this invocation.
        raw = getattr(repo, "_run_metadata_history_reads", {}).get(key)
        if raw is None:
            raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
        else:
            _valid(_git(repo.root, ["cat-file", "-t", base]) == b"commit\n")
        _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
        revisions = raw.decode("ascii").splitlines()
        _valid(len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
               and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
               and (not revisions or revisions[-1] == head))
        if any(repo._tree(revision, marker) for revision in revisions):
            present = False
            for prior, revision in _pr18_verification_repair_history(repo, base, head):
                now = bool(repo._tree(revision, marker))
                if now and not present:
                    _pr18_planner_public_owner_edge(repo, prior, revision)
                present = now
        checked.add(key)
        repo._planner_public_owner_introductions_checked = checked
        return False
    # A completed predecessor's test remains an ordinary editable test path.
    # Once another repair path triggers admission, its exact wrapper bytes are
    # still mandatory in the seven-path atomic edge, with no prefix or suffix.
    triggers = _PR18_PLANNER_PUBLIC_OWNER_FILES.keys() - {
        "release/deployment/test_pr18_managed_replication_repair.py"}
    if any(path in triggers for _, path in changed_paths(repo.root, base, head)):
        return True
    return any(path in triggers
               for prior, revision in _pr18_verification_repair_history(repo, base, head)
               for _, path in changed_paths(repo.root, prior, revision))


def _pr18_planner_public_owner_edge(repo, base, head):
    _pr18_planner_public_owner_before(repo, base)
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_PLANNER_PUBLIC_OWNER_FILES.items())
    _valid(sorted(changed_paths(repo.root, base, head)) == expected)
    for path, (_, digest) in _PR18_PLANNER_PUBLIC_OWNER_FILES.items():
        _pr18_planner_public_owner_blob(repo, head, path, digest)


def _pr18_planner_public_owner_local(repo, base, head):
    _pr18_planner_public_owner_before(repo, base)
    repaired = False
    for prior, revision in _pr18_verification_repair_history(repo, base, head):
        if changed_paths(repo.root, prior, revision):
            _valid(not repaired)
            _pr18_planner_public_owner_edge(repo, prior, revision)
            repaired = True
    _valid(repaired)


def _pr18_observer_transition(repo: _Repository, base: str, head: str,
                             *, ci: bool = False) -> tuple[str, str]:
    """Prove one exact transition without calling either classifier or CI.

    Older release history may contain only the independently reviewed main.go
    states, in order. CI additionally admits no unrelated prefix/suffix edits.
    All reads retain the existing per-blob, aggregate and first-parent bounds.
    """
    _valid(base != head and _git(repo.root, ["merge-base", base, head]).strip() == base.encode())
    raw = _git(repo.root, ["rev-list", "--first-parent", "--reverse", base + ".." + head])
    _valid(len(raw) <= MAX_HISTORY_COMMITS * 41)
    revisions = raw.decode("ascii").splitlines()
    _valid(0 < len(revisions) <= MAX_HISTORY_COMMITS and len(revisions) == len(set(revisions))
           and all(re.fullmatch(r"[0-9a-f]{40}", rev) and rev != base for rev in revisions)
           and revisions[-1] == head)
    # The range alone does not prove its first included commit descends from
    # base along first parents: excluded ancestors can hide a second-parent
    # base. Check every actual first-parent OID below, including the first edge.
    expected = sorted(("A" if old is None else "M", path)
                      for path, (old, _) in _PR18_OBSERVER_FILES.items())
    descriptors = (schema_nullable.SOURCE_PATH, schema_nullable.CONSTANT_SOURCE_PATH,
                   schema_index.SOURCE_PATH, schema_relation.SOURCE_PATH, schema_tenant_profile.SOURCE_PATH)
    protected_predecessor = None
    if repo._tree(head, _PR18_PLANNER_PUBLIC_OWNER_MARKER):
        protected_predecessor = _pr18_planner_public_owner_protected_predecessor(repo, base, head, raw)
    prior, prior_state, anchor = None, None, None
    repaired, managed, metadata_repaired, replication_repaired, public_owner_repaired = False, False, False, False, False
    for revision in (base, *revisions):
        if prior is not None:
            _valid(_git(repo.root, ["rev-parse", "--verify", revision + "^1"]) == prior.encode() + b"\n")
        main = repo.blob(revision, _PR18_OBSERVER_MAIN)
        _valid(type(main) is bytes)
        digest = hashlib.sha256(main).hexdigest()
        _valid(digest in _PR18_OBSERVER_MAIN_STATES)
        state = _PR18_OBSERVER_MAIN_STATES.index(digest)
        _pr18_observer_blob(repo, revision, _PR18_OBSERVER_MAIN, digest)
        if prior is None:
            _valid(state in {0, 1})
        else:
            _valid(state in {prior_state, prior_state + 1})
        # Historical PR7 introduced this exact manifest with the S0->S1 source
        # transition. Arbitrary dormant declarations cannot hide another call.
        _pr18_observer_blob(repo, revision, DORMANT, None if state == 0 else
                            "041af21a905dd160657431d8a4482d8f52f162f312d4cc9148a071400bbd508d")
        public_owner_marker = (replication_repaired
            and bool(repo._tree(revision, _PR18_PLANNER_PUBLIC_OWNER_MARKER)))
        if public_owner_marker:
            _pr18_planner_public_owner_blob(repo, revision, _PR18_PLANNER_PUBLIC_OWNER_MARKER,
                _PR18_PLANNER_PUBLIC_OWNER_FILES[_PR18_PLANNER_PUBLIC_OWNER_MARKER][1])
        for path, protected in _PR18_OBSERVER_PROTECTED.items():
            if public_owner_marker or revision == protected_predecessor:
                _pr18_planner_public_owner_protected(repo, revision, path, protected)
            else:
                _pr18_observer_blob(repo, revision, path, protected)
        _valid(not _git(repo.root, ["ls-tree", "-r", "--name-only", "-z", revision, "--", _V2_PENDING.rstrip("/")]))
        for path in descriptors:
            _pr18_observer_blob(repo, revision, path, None)
        if state < 2:
            for path, (old, _) in _PR18_OBSERVER_FILES.items():
                if old is None:
                    _pr18_observer_blob(repo, revision, path, None)
        if prior is not None:
            changes = changed_paths(repo.root, prior, revision)
            if prior_state == 1 and state == 2:
                _valid(anchor is None and sorted(changes) == expected)
                for path, (old, new) in _PR18_OBSERVER_FILES.items():
                    _pr18_observer_blob(repo, prior, path, old)
                    _pr18_observer_blob(repo, revision, path, new)
                for rev in (prior, revision):
                    _pr18_observer_blob(repo, rev, "internal/dbcatalog/registry/coordinator_bindings.json",
                                       "ecc49fb74995dcd121a645fd139ebff2fdc9e22bd2a1b01df91a3eb4e24daee4")
                anchor = (prior, revision)
            elif prior_state == state == 2 and changes:
                _valid(anchor is not None)
                if not repaired:
                    _pr18_verification_repair_edge(repo, prior, revision)
                    repaired = True
                elif not managed:
                    _pr18_managed_owner_edge(repo, prior, revision)
                    managed = True
                elif not metadata_repaired:
                    _pr18_run_metadata_edge(repo, prior, revision)
                    metadata_repaired = True
                elif not replication_repaired:
                    if revision == protected_predecessor:
                        _pr18_managed_replication_edge(repo, prior, revision,
                            public_owner_predecessor=protected_predecessor)
                    else:
                        _pr18_managed_replication_edge(repo, prior, revision)
                    replication_repaired = True
                else:
                    _valid(not public_owner_repaired)
                    _pr18_planner_public_owner_edge(repo, prior, revision)
                    public_owner_repaired = True
            elif ci or state == 2:
                _valid(not changes)
        prior, prior_state = revision, state
    _valid(anchor is not None and prior_state == 2)

    # Whole-file pins cover imports, implementation, metadata and tests. Also
    # assert the two reviewed declaration transforms and unchanged remainders.
    for path, name, old_digest, new_digest in (
        (_PR18_OBSERVER_MAIN, "runRLSMigration",
         "62d2f37e594c0027b8ccdb203da87118a8b8e1f58cdb86db812b9c2a615582b5",
         "beb7595c484b2a9c08847ed3b1fb9efcd7627161a667a6572d698aa07ffd29a7"),
        ("internal/database/postgres.go", "runRLSMigrationCoordinatorForPhase",
         "2afd699f059c3f0514f9c43230d05a5560a417d558a3b94b725489e32440954f",
         "b0c63d1e8d3e460914ccfc8097cf4c52ce564154f470f0b0c5f3487aa090293f"),
    ):
        left, right = (repo.text(rev, path) for rev in anchor)
        old_fn, new_fn = (_function_bytes(text, name) for text in (left, right))
        _valid(len(old_fn) == len(new_fn) == 1
               and hashlib.sha256(old_fn[0].encode()).hexdigest() == old_digest
               and hashlib.sha256(new_fn[0].encode()).hexdigest() == new_digest
               and left.replace(old_fn[0], "", 1) == right.replace(new_fn[0], "", 1))

    source = repo.text(head, _PR18_OBSERVER_MAIN)
    spans = _functions(source, "runRLSMigration")
    _valid(len(spans) == 1)
    dormant = _dormant(repo, head)
    offsets, offset = [], 0
    for line in source.splitlines(keepends=True):
        offsets.append(offset)
        offset += len(line)
    observed = []
    for number, line in _added_numbered(repo.root, base, head, _PR18_OBSERVER_MAIN):
        _valid(1 <= number <= len(offsets))
        for pattern in MIGRATION_CALL_PATTERNS:
            for match in pattern.finditer(line):
                begin, end = offsets[number - 1] + match.start(), offsets[number - 1] + match.end()
                if not any(start <= begin and end <= stop for start, stop in dormant.get(_PR18_OBSERVER_MAIN, [])):
                    _valid(spans[0][0] <= begin and end <= spans[0][1])
                    observed.append(line)
    _valid(observed == ["\tcoordinatorErr := database.RunRLSMigrationCoordinator("])
    return anchor


# Ordinary CI prerequisite for the reviewed H1 bootstrap closure shape.
# These files add package initialization to two existing protected closures;
# generated Go checks, exact compatibility and ordinary staging remain required.
_CI_H1_BASELINE_ID = "BackfillChatbotFlowGraph"
_CI_H1_CLOSURE_ADDITIONS = (
    "internal/handlers/inbound_continuation_admission.go",
    "internal/handlers/inbound_continuation_capacity.go",
)
_CI_H1_ACTIVE = (CATALOG, HISTORY, GLOBALS, GOLDEN + "production-v0.json",
                 "internal/dbcatalog/shape/production-v0.sql", SEEDS)


def _ci_h1_registry_bytes(rows: list[dict]) -> bytes:
    # _registry has already constrained every string to its ASCII grammar.
    # Reproduce the Go registry generator's field order and indentation; an
    # unrelated formatting/key-order edit is not a closure-only refresh.
    fields = ("id", "rev", "func", "kind", "baseline", "idempotent", "n1_safe",
              "requires_gates", "closure_files", "closure_sha256")
    ordered = []
    for row in rows:
        value = {field: row[field] for field in fields}
        if "parent" in row:
            value["parent"] = row["parent"]
        ordered.append(value)
    return (json.dumps(ordered, ensure_ascii=True, indent=2) + "\n").encode("ascii")


def _ci_h1_bootstrap_closure(repo: _Repository, base: str, head: str,
                             old_migration: dict, new_migration: dict) -> None:
    """Validate one paired bootstrap metadata refresh, never its computed hash."""
    old_rows = repo.document(base, REGISTRY, _registry)
    new_rows = repo.document(head, REGISTRY, _registry)
    old_registry, new_registry = repo.blob(base, REGISTRY), repo.blob(head, REGISTRY)
    _valid(old_registry == _ci_h1_registry_bytes(old_rows)
           and new_registry == _ci_h1_registry_bytes(new_rows)
           and old_registry != new_registry and len(old_rows) == len(new_rows))
    _valid([row["id"] for row in old_rows] == [row["id"] for row in new_rows])
    expected_identity = {
        "id": _CI_H1_BASELINE_ID, "rev": 1,
        "func": "github.com/shridarpatil/whatomate/internal/handlers.BackfillChatbotFlowGraph",
        "kind": "baseline-pre-ledger", "baseline": True, "idempotent": False,
        "n1_safe": False, "requires_gates": [],
    }
    target = None
    for before, after in zip(old_rows, new_rows):
        if before["id"] != _CI_H1_BASELINE_ID:
            _valid(before == after)
            continue
        _valid({key: value for key, value in before.items() if key not in ("closure_files", "closure_sha256")} == expected_identity
               and {key: value for key, value in after.items() if key not in ("closure_files", "closure_sha256")} == expected_identity)
        target = (before, after)
    _valid(target is not None)
    _valid(old_migration["version"] == new_migration["version"] == 1
           and old_migration["roots"] == new_migration["roots"] == MIGRATION_ROOTS)
    for revision, document in ((base, old_migration), (head, new_migration)):
        ordered = {key: document[key] for key in ("version", "roots", "closure_files", "closure_sha256")}
        canonical = (json.dumps(ordered, ensure_ascii=True, indent=2) + "\n").encode("ascii")
        _valid(repo.blob(revision, MIGRATION_PATH) == canonical)
    for before, after in (target, (old_migration, new_migration)):
        _valid(not set(_CI_H1_CLOSURE_ADDITIONS).intersection(before["closure_files"])
               and after["closure_files"] == sorted(before["closure_files"] + list(_CI_H1_CLOSURE_ADDITIONS))
               and before["closure_sha256"] != after["closure_sha256"])
    for path in _CI_H1_CLOSURE_ADDITIONS:
        raw = repo.blob(head, path)
        _valid(type(raw) is bytes and 0 < len(raw) <= MAX_ARTIFACT_BYTES)

    active = {path: repo.blob(base, path) for path in _CI_H1_ACTIVE}
    _valid(all(type(raw) is bytes and 0 < len(raw) <= MAX_ARTIFACT_BYTES for raw in active.values()))
    old_pair = (old_registry, repo.blob(base, MIGRATION_PATH))
    new_pair = (new_registry, repo.blob(head, MIGRATION_PATH))
    raw_history = _git(repo.root, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"])
    _valid(len(raw_history) <= MAX_HISTORY_COMMITS * 41)
    commits = raw_history.splitlines()
    _valid(raw_history == b"\n".join(commits) + b"\n"
           and 0 < len(commits) <= MAX_HISTORY_COMMITS and len(set(commits)) == len(commits)
           and all(re.fullmatch(rb"[0-9a-f]{40}", value) is not None for value in commits)
           and commits[-1] == head.encode("ascii") and base.encode("ascii") not in commits)
    transitioned = False
    previous = base.encode("ascii")
    for raw_revision in commits:
        revision = raw_revision.decode("ascii")
        # Verify the actual first-parent link as well as the listing shape.
        # A truncated prefix/middle must not omit an intermediate bad state.
        parent = _git(repo.root, ["rev-parse", "--verify", revision + "^1"])
        _valid(parent == previous + b"\n")
        previous = raw_revision
        pair = (repo.blob(revision, REGISTRY), repo.blob(revision, MIGRATION_PATH))
        if pair == old_pair:
            _valid(not transitioned)
        else:
            _valid(pair == new_pair)
            transitioned = True
        for path, before in active.items():
            _valid(repo.blob(revision, path) == before)
    _valid(transitioned)


def check_ci_source(repo_dir: Path, base: str, head: str) -> tuple[str, ...]:
    """CI source eligibility only; generated tests AND exact compat must pass.

    Preserve the existing none-class contract, reviewed named transitions and
    the closed ordinary migration/bootstrap closure eligibility below.
    Incomplete target edits refuse. This does not grant
    experiment, merge or release authority, or change check()/guard().
    """
    repo = _Repository(repo_dir)
    legacy = _classify(repo, base, head)
    try:
        if _pr18_verification_repair_changed(repo, base, head):
            _pr18_verification_repair_local(repo, base, head)
            _valid(not legacy.reasons and legacy.kind == "none" and legacy.dormant is False)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "none" and policy.dormant is False
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == ())
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_VERIFICATION_REPAIR_REASON}))
    try:
        if _pr18_managed_owner_changed(repo, base, head):
            _pr18_managed_owner_local(repo, base, head)
            _valid(legacy.reasons == ("migration-path-changed:closure",)
                   and legacy.kind == "none" and legacy.dormant is False)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "none" and policy.dormant is False
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == ("generated migration closure changed",))
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_MANAGED_OWNER_REASON}))
    try:
        # An existing classifier-invalid refusal is never cleared below. Keep
        # its original validation/error ownership instead of adding a new scan.
        if (not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_run_metadata_changed(repo, base, head)):
            _pr18_run_metadata_local(repo, base, head)
            _valid(not legacy.reasons and legacy.kind == "none" and legacy.dormant is False)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "none" and policy.dormant is False
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == ())
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_RUN_METADATA_REPAIR_REASON}))
    try:
        # This exact legacy refusal can be cleared only by the mandatory
        # final observer transition below, which replays the replication edge.
        # Keep the introduction/undo scan for every ordinary or local range.
        if legacy.reasons == _PR18_OBSERVER_LEGACY:
            if repo._tree(head, _PR18_PLANNER_PUBLIC_OWNER_MARKER):
                _pr18_planner_public_owner_prime_protected(repo, base, head)
            else:
                _pr18_managed_replication_prime_protected(repo, base, head)
        if (legacy.reasons != _PR18_OBSERVER_LEGACY
                and not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_managed_replication_changed(repo, base, head)):
            _pr18_managed_replication_local(repo, base, head)
            _valid(legacy.reasons == ("migration-path-changed:closure",)
                   and legacy.kind == "none" and legacy.dormant is False)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "none" and policy.dormant is False
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == ("generated migration closure changed",))
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_MANAGED_REPLICATION_REASON}))
    try:
        if (legacy.reasons != _PR18_OBSERVER_LEGACY
                and not any(reason.startswith("classifier-invalid:") for reason in legacy.reasons)
                and _pr18_planner_public_owner_changed(repo, base, head)):
            _pr18_planner_public_owner_local(repo, base, head)
            _valid(legacy.reasons == ("migration-path-changed:closure",)
                   and legacy.kind == "none" and legacy.dormant is False)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "none" and policy.dormant is False
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == ("generated migration closure changed",))
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_PLANNER_PUBLIC_OWNER_REASON}))
    # This fixed target must not disappear behind the legacy clean fast path
    # when its generated manifest or paired registry revisions are stale.
    try:
        target_changed = _ci_seed_changed(repo, base, head)
        if target_changed:
            _ci_seed_removal(repo, base, head, legacy)
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_CI_SEED_REASON}))
    try:
        if legacy.reasons == _PR18_OBSERVER_LEGACY or _pr18_observer_changed(repo, base, head):
            _valid(legacy.reasons == _PR18_OBSERVER_LEGACY)
            _pr18_observer_transition(repo, base, head, ci=True)
            policy = _classify_v2(repo, base, head, legacy)
            _valid(not policy.reasons and policy.kind == "data" and policy.dormant is True
                   and policy.requirements == _PR18_OBSERVER_REQUIREMENTS
                   and policy.migration_changes == _PR18_OBSERVER_MIGRATIONS)
            return ()
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return tuple(sorted(set(legacy.reasons) | {_PR18_OBSERVER_REASON}))
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
                   and 0 < len(before) <= MAX_ARTIFACT_BYTES
                   and 0 < len(after) <= MAX_ARTIFACT_BYTES)
            if path == REGISTRY and before != after:
                _ci_h1_bootstrap_closure(repo, base, head, old, new)
            else:
                _valid(before == after)
        changes = changed_paths(repo.root, base, head)
        _valid(len(changes) <= 10000 and all(_changed_path(path) for _, path in changes)
               and not any(path == _V2_PENDING.rstrip("/") or path.startswith(_V2_PENDING)
                           for _, path in changes))
    except (_Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
        return legacy.reasons
    return ()


if __name__ == "__main__":
    raise SystemExit(main())
