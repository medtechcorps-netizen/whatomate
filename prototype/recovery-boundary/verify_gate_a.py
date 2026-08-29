"""Offline containment checks for the local-only Gate-A prototype.

This script performs no DNS resolution or network access. It validates only the
filesystem subtree containing it and treats every local durability helper as a
test oracle rather than production authority.
"""

from __future__ import annotations

import ast
import hashlib
import ipaddress
import json
import os
import re
import string
import sys
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parent
REPARSE_POINT = 0x400
PATH_BLOB_MANIFEST = "PATH_BLOBS.sha256"

FORBIDDEN_NAMES = {"go.mod", "go.sum", "go.work"}
FORBIDDEN_PARTS = {
    ".git", ".openai", "vendor", "__pycache__", ".pytest_cache", ".mypy_cache"
}
FORBIDDEN_SUFFIXES = {".pyc", ".pyo", ".exe", ".dll", ".so", ".dylib"}

# One-way fingerprints bind the protected preflight denylist without placing
# any raw selector, endpoint, environment name, source identity, or live
# projection in this public subtree. Candidate values are normalized before
# hashing so case changes cannot bypass the comparison.
FORBIDDEN_CANDIDATE_SHA256 = frozenset({
    "19f0f20b8dbeae3b7aa4eadfdb788fa6f9c2ab30266b0e5b67591a96d9ccc6b3",
    "345204a9b9887668f08017052e647833ace899b04e9e383a15beea86e7fbf08f",
    "42e54540c9a6923b9cc0254ead61d98d85eaaf98e63329ce5da2be579b5a2418",
    "439ecb65c0e711036d39b26a4d38b91f555dad32325dbdd86544230743d3cb0f",
    "701725722c0968bdbb98ed4587a99c7317ec4272dfbad2d7f6660455e7316036",
    "938f7910b58f67ed9c34361dddcccf51a301ec10722bfeac6619b12908e22e40",
    "ba501baffd226750d689b2936e798f02672571fb7af3ef9d5d3daf8dca4fb3a1",
    "bedbb5c0788934c3dcf9d9094840a026c56f7e33059a4366dd76524d8b589e39",
    "d474a8c5f105a3a6590fd95b2788e96ead89ba4a40cd38c13efe4bee261bb7fa",
    "e4f4d5092c24d2b6d0219339da2d46ff62c35d60590e812de509abdc3703671d",
})
# Lengths are non-secret metadata.  Keeping them alongside the one-way
# fingerprints lets the scanner hash every bounded window in a larger token,
# rather than trusting a greedy token boundary that an ordinary suffix could
# mask.
FORBIDDEN_CANDIDATE_LENGTHS = frozenset({11, 18, 21, 24, 25, 36, 40, 64})
PROTECTED_CANDIDATE_RE = re.compile(
    r"(?<![A-Za-z0-9_.:-])[A-Za-z0-9_.:-]+(?![A-Za-z0-9_.:-])"
)

GO_STRING_TOKEN_RE = re.compile(r'`[^`]*`|"(?:\\.|[^"\\])*"')
GO_CONCAT_TOKEN_RE = re.compile(
    r'\s*(?:(?P<string>`[^`]*`|"(?:\\.|[^"\\])*")|'
    r'(?P<name>[A-Za-z_][A-Za-z0-9_]*)|(?P<operator>[+()]))'
)
GO_STATIC_TOKEN_RE = re.compile(
    r'`[^`]*`|"(?:\\.|[^"\\])*"|[A-Za-z_][A-Za-z0-9_]*|[+()]'
)
SHELL_ADJACENT_LITERAL_RE = re.compile(
    r"(?:(?:\"(?:\\.|[^\"\\])*\"|'[^']*')){2,}"
)
SHELL_LITERAL_RE = re.compile(r'\"(?:\\.|[^\"\\])*\"|\'[^\']*\'')

KNOWN_TOKEN_PATTERNS = (
    re.compile(re.escape("dop_" + "v1_") + r"[A-Za-z0-9]{16,}"),
    re.compile(re.escape("github_" + "pat_") + r"[A-Za-z0-9_]{16,}"),
    re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b"),
    re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{16,}\b"),
    re.compile(r"\bAKIA[A-Z0-9]{16}\b"),
    re.compile(re.escape("sk-" + "proj-") + r"[A-Za-z0-9_-]{20,}"),
)
PRIVATE_KEY_BEGIN = "-----BEGIN " + "PRIVATE KEY-----"
PRIVATE_KEY_END = "-----END " + "PRIVATE KEY-----"
PRIVATE_KEY_MARKERS = (
    PRIVATE_KEY_BEGIN,
    "-----BEGIN RSA " + "PRIVATE KEY-----",
    "-----BEGIN EC " + "PRIVATE KEY-----",
    "-----BEGIN OPENSSH " + "PRIVATE KEY-----",
)
CREDENTIALED_URI_RE = re.compile(r"(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@")
URL_RE = re.compile(r"https?://[A-Za-z0-9._~!$&'()*+,;=:%-]+(?:/[^\s\"'`<>]*)?")
SCHEME_URI_RE = re.compile(
    r"(?i)\b([a-z][a-z0-9+.-]*)://([^\s\"'`<>]+)"
)
IPV4_RE = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")
IPV6_COLON = ":"
IPV6_CANDIDATE_RE = re.compile(
    r"(?<![0-9A-Za-z])\[?([0-9A-Fa-f]*(?:"
    + IPV6_COLON
    + r"[0-9A-Fa-f]*){2,})\]?(?![0-9A-Za-z])"
)
BARE_HOST_PORT_RE = re.compile(
    r"(?i)(?<![A-Za-z0-9_.-])"
    r"(localhost|[A-Za-z](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|"
    r"(?:[0-9]{1,3}\.){3}[0-9]{1,3})"
    r":([1-9][0-9]{0,4})(?![0-9])"
)
BARE_HOSTNAME_VALUE_RE = re.compile(
    r"(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?\.)+[a-z]{2,63}$"
)

EXTERNAL_CALL_MARKERS = (
    "http.Get(",
    "http.Post(",
    "net.Dial(",
    "DialContext(",
    "urlopen(",
    "requests.",
    "socket.create_connection(",
    "Invoke-WebRequest",
    "Invoke-RestMethod",
    "http.NewRequest(",
    "http.NewRequestWithContext(",
    "net.Listen(",
    "exec.Command(",
    "subprocess.",
    "os.system(",
)

ALLOWED_GO_IMPORTS = {
    "bytes", "context", "crypto", "crypto/aes", "crypto/cipher",
    "crypto/ed25519", "crypto/rand", "crypto/rsa", "crypto/sha256",
    "crypto/subtle", "crypto/x509", "encoding/base64", "encoding/binary",
    "encoding/hex", "encoding/json", "encoding/pem", "errors", "fmt",
    "go/parser", "go/token", "io", "math/big", "net/url", "os",
    "path/filepath", "reflect", "regexp", "strconv", "strings", "sync",
    "sync/atomic", "testing", "time", "unicode/utf8",
    "github.com/shridarpatil/whatomate/prototype/recovery-boundary/internal/rolecmd",
}
ALLOWED_PYTHON_IMPORTS = {
    "tests/fault_oracle.py": {"__future__", "dataclasses", "typing"},
    "tests/schema_tools.py": {
        "__future__", "base64", "datetime", "hashlib", "json", "pathlib", "re", "typing",
    },
    "tests/test_boundary.py": {
        "__future__", "copy", "fault_oracle", "hashlib", "ipaddress", "pathlib",
        "re", "schema_tools", "socket", "struct", "unittest", "urllib.request",
    },
    "verify_gate_a.py": {
        "__future__", "ast", "hashlib", "ipaddress", "json", "os", "pathlib",
        "re", "string", "sys", "urllib.parse",
    },
}

UUID_RE = re.compile(
    r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b"
)
SYNTHETIC_UUID_RE = re.compile(
    r"^00000000-0000-4[0-9a-fA-F]{3}-8[0-9a-fA-F]{3}-000000000[0-9a-fA-F]{3}$"
)

EXPECTED_WORKFLOW_SHA256 = {
    "workflows/build.tmpl": "99b16f3b8c1745eea66ad642f45d3f673cd8eae125ad446982b7db7ff0fbc9d2",
    "workflows/cleanup.tmpl": "dcf18d5b14c712baabd80123ab62ce1fe80c3833fb0f1c9e373fbb5865cd5795",
    "workflows/exercise.tmpl": "d5730755521f08a4f6595633d0213865b9336630f5223e23d91b17401fcd2eb0",
}
EXPECTED_DOCKERFILE_SHA256 = {
    "docker/observer-authority.Dockerfile": "0f6a74da80e4ca3294809aa4c983f97e8771e4dcecb9c970903069c84f3ebdf6",
    "docker/observer-broker.Dockerfile": "f349491a83ee93c66521e364acd4fb24af85ac7541dd7e0ae4b5d32d9141a949",
    "docker/writer-authority.Dockerfile": "3b1fe5e1c6f11b63910e80a80fffe49011818f53a1747484edf2f784d4dc1e3b",
    "docker/writer-broker.Dockerfile": "d3d929d0a0d28b4f8136b01e4e738817516c53e5a21092f258fbda25d33ccead",
}

EXPECTED_FILES = {
    "PATH_BLOBS.sha256",
    "README.md",
    "cmd/observer-authority/main.go",
    "cmd/observer-broker/main.go",
    "cmd/writer-authority/main.go",
    "cmd/writer-broker/main.go",
    "docker/observer-authority.Dockerfile",
    "docker/observer-broker.Dockerfile",
    "docker/writer-authority.Dockerfile",
    "docker/writer-broker.Dockerfile",
    "docs/gate-c-resource-ceiling.md",
    "docs/state-machine-runbook.md",
    "docs/threat-model.md",
    "fixtures/domain-separation.noncanonical.json",
    "fixtures/domain-separation.valid.json",
    "fixtures/lifecycle-receipt.invalid-writer-present.json",
    "fixtures/lifecycle-receipt.valid.json",
    "fixtures/observer-receipt.invalid-source-read.json",
    "fixtures/observer-receipt.valid.json",
    "fixtures/observer-admission-publication.valid.json",
    "fixtures/observer-admission-lifecycle.valid.json",
    "fixtures/observer-cleanup-lifecycle.valid.json",
    "fixtures/observer-evidence-publication.valid.json",
    "fixtures/recovery-admission-authorization.valid.json",
    "fixtures/recovery-admission.valid.json",
    "fixtures/recovery-boundary-continuity.valid.json",
    "fixtures/writer-receipt.invalid-extra.json",
    "fixtures/writer-receipt.valid.json",
    "fixtures/writer-authorization-publication.valid.json",
    "internal/model/boundary.go",
    "internal/model/durable_effect.go",
    "internal/model/oracle.go",
    "internal/model/oracle_test.go",
    "internal/model/observer_lifecycle.go",
    "internal/model/observer_lifecycle_test.go",
    "internal/model/protocol_test.go",
    "internal/model/protocol.go",
    "internal/model/sanitize.go",
    "internal/model/sanitize_test.go",
    "internal/model/types.go",
    "internal/model/writer_lifecycle_provider.go",
    "internal/model/writer_lifecycle_provider_test.go",
    "internal/protocol/canonical.go",
    "internal/protocol/canonical_test.go",
    "internal/protocol/ledger.go",
    "internal/protocol/ledger_test.go",
    "internal/protocol/no_network_test.go",
    "internal/protocol/oidc.go",
    "internal/protocol/oidc_test.go",
    "internal/protocol/signature.go",
    "internal/rolecmd/rolecmd.go",
    "internal/rolecmd/rolecmd_test.go",
    "schemas/domain-separation.schema.json",
    "schemas/lifecycle-receipt.schema.json",
    "schemas/ledger-publication.schema.json",
    "schemas/observer-admission-lifecycle.schema.json",
    "schemas/observer-cleanup-lifecycle.schema.json",
    "schemas/observer-receipt.schema.json",
    "schemas/recovery-admission-authorization.schema.json",
    "schemas/recovery-admission.schema.json",
    "schemas/recovery-boundary-continuity.schema.json",
    "schemas/writer-receipt.schema.json",
    "tests/fault_oracle.py",
    "tests/schema_tools.py",
    "tests/test_boundary.py",
    "verify_gate_a.py",
    "workflows/build.tmpl",
    "workflows/cleanup.tmpl",
    "workflows/exercise.tmpl",
}


def regular_files() -> list[Path]:
    files: list[Path] = []
    resolved_root = ROOT.resolve(strict=True)
    for path in ROOT.rglob("*"):
        relative = path.relative_to(ROOT)
        if any(part in FORBIDDEN_PARTS for part in relative.parts):
            raise AssertionError(f"forbidden path part: {relative.as_posix()}")
        try:
            resolved = path.resolve(strict=True)
            resolved.relative_to(resolved_root)
        except (OSError, ValueError) as exc:
            raise AssertionError(
                f"path escapes exact prototype root: {relative.as_posix()}"
            ) from exc
        stat = path.lstat()
        attributes = getattr(stat, "st_file_attributes", 0)
        if path.is_symlink() or attributes & REPARSE_POINT:
            raise AssertionError(f"link or reparse point: {relative.as_posix()}")
        if path.is_dir():
            continue
        if not path.is_file():
            raise AssertionError(f"non-regular entry: {relative.as_posix()}")
        if stat.st_nlink != 1:
            raise AssertionError(f"hard-linked file: {relative.as_posix()}")
        if path.name in FORBIDDEN_NAMES or path.suffix.lower() in FORBIDDEN_SUFFIXES:
            raise AssertionError(f"forbidden file: {relative.as_posix()}")
        files.append(path)
    return sorted(files)


def text_files(files: list[Path]) -> list[tuple[Path, str]]:
    decoded: list[tuple[Path, str]] = []
    for path in files:
        data = path.read_bytes()
        if b"\x00" in data:
            raise AssertionError(f"binary content: {path.relative_to(ROOT).as_posix()}")
        if b"\r" in data:
            raise AssertionError(f"non-LF line ending: {path.relative_to(ROOT).as_posix()}")
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise AssertionError(
                f"non-UTF-8 content: {path.relative_to(ROOT).as_posix()}"
            ) from exc
        if any(line != line.rstrip(" \t") for line in text.splitlines()):
            raise AssertionError(
                f"trailing whitespace: {path.relative_to(ROOT).as_posix()}"
            )
        decoded.append((path, text))
    return decoded


def check_public_fixture_keys(text: str, relative: str) -> None:
    try:
        value = json.loads(text)
    except (TypeError, ValueError) as exc:
        raise AssertionError(f"public fixture is not valid JSON: {relative}") from exc

    def walk(item: object) -> None:
        if isinstance(item, dict):
            for key, child in item.items():
                lowered = key.lower()
                if "seed" in lowered:
                    raise AssertionError(
                        f"private seed-like field in public fixture: {relative}"
                    )
                sensitive = any(
                    marker in lowered
                    for marker in (
                        "private", "secret", "credential", "password", "token",
                        "endpoint", "hostname", "app_id", "deployment_id",
                        "database_id", "cluster_id", "network_id", "vpc_id",
                        "project_id", "team_id", "source_id", "fork_id",
                    )
                )
                harmless_attestation = (
                    lowered.endswith("_sha256")
                    or lowered.endswith("_absent")
                    or lowered.endswith("_revoked")
                    or (lowered.endswith("_present") and isinstance(child, bool))
                )
                if sensitive and not harmless_attestation:
                    raise AssertionError(
                        f"private material or raw selector field in public fixture: {relative}"
                    )
                if lowered.endswith("_key") and lowered != "signing_public_key":
                    raise AssertionError(
                        f"raw key field in public fixture: {relative}"
                    )
                walk(child)
        elif isinstance(item, list):
            for child in item:
                walk(child)
        elif isinstance(item, str):
            hostname = item[:-1] if item.endswith(".") else item
            if not BARE_HOSTNAME_VALUE_RE.fullmatch(hostname):
                return
            lowered = hostname.lower()
            if lowered != "synthetic.invalid" and not lowered.endswith(
                ".synthetic.invalid"
            ):
                raise AssertionError(
                    f"bare hostname-shaped value in public fixture: {relative}"
                )

    walk(value)


def candidate_sha256(value: str) -> str:
    return hashlib.sha256(value.casefold().encode("utf-8")).hexdigest()


MAX_STATIC_STRING_VALUES = 256
MAX_STATIC_STRING_LENGTH = 1 << 20
MAX_PROTECTED_FILE_BYTES = 1 << 20
MAX_PROTECTED_VALUE_BYTES = 1 << 20
MAX_PROTECTED_TOKENS_PER_FILE = 100_000
MAX_PROTECTED_HASH_WINDOWS = 10_000_000
MAX_PROTECTED_STATIC_VALUES = 100_000
MAX_GO_STATIC_WINDOW_TOKENS = 32
MAX_GO_STATIC_WINDOWS = 100_000


def _checked_static_repeat(value: object, count: int) -> object:
    """Repeat a folded primitive only after proving the allocation is bounded."""

    repeated_length = len(value) * max(count, 0)  # type: ignore[arg-type]
    limit = (
        MAX_STATIC_STRING_LENGTH
        if isinstance(value, str)
        else MAX_STATIC_STRING_VALUES
    )
    if repeated_length > limit:
        raise AssertionError("static Python repetition exceeded allocation bound")
    return value * count  # type: ignore[operator]


def _preflight_percent_template(template: str) -> None:
    """Reject format widths that could allocate beyond the static scan ceiling."""

    percent_spec = re.compile(
        r"%(?:\([^)]+\))?[#0\- +]*"
        r"(?P<width>\*|[0-9]+)?(?:\.(?P<precision>\*|[0-9]+))?"
        r"[hlL]?[diouxXeEfFgGcrsa%]"
    )
    position = 0
    while position < len(template):
        marker = template.find("%", position)
        if marker < 0:
            return
        if marker + 1 < len(template) and template[marker + 1] == "%":
            position = marker + 2
            continue
        match = percent_spec.match(template, marker)
        if match is None:
            position = marker + 1
            continue
        for field in (match.group("width"), match.group("precision")):
            if field == "*":
                raise AssertionError(
                    "static Python percent format used dynamic allocation width"
                )
            if field is not None and int(field) > MAX_STATIC_STRING_LENGTH:
                raise AssertionError(
                    "static Python percent format exceeded allocation bound"
                )
        position = match.end()


def _preflight_format_spec(spec: str) -> None:
    if "{" in spec or "}" in spec:
        raise AssertionError("static Python format used dynamic allocation width")
    for numeric in re.findall(r"[0-9]+", spec):
        if int(numeric) > MAX_STATIC_STRING_LENGTH:
            raise AssertionError("static Python format exceeded allocation bound")


def _preflight_brace_template(template: str) -> None:
    try:
        parsed = list(string.Formatter().parse(template))
    except ValueError:
        return
    literal_length = sum(len(literal) for literal, *_ in parsed)
    if literal_length > MAX_STATIC_STRING_LENGTH:
        raise AssertionError("static Python format exceeded allocation bound")
    for _, field_name, format_spec, _ in parsed:
        if field_name is not None:
            _preflight_format_spec(format_spec)


def _static_product(parts: list[set[str]]) -> set[tuple[str, ...]]:
    combined: set[tuple[str, ...]] = {tuple()}
    for choices in parts:
        if not choices:
            return set()
        if len(combined) * len(choices) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python string analysis exceeded value bound")
        expanded = {
            prefix + (choice,)
            for prefix in combined
            for choice in choices
        }
        if len(expanded) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python string analysis exceeded value bound")
        if any(
            sum(len(value) for value in values) > MAX_STATIC_STRING_LENGTH
            for values in expanded
        ):
            raise AssertionError("static Python string analysis exceeded length bound")
        combined = expanded
    return combined


def _combine_static_parts(parts: list[set[str]], separator: str = "") -> set[str]:
    return {separator.join(values) for values in _static_product(parts)}


def _static_sequence_product(
    parts: list[list[tuple[str, ...]]],
) -> set[tuple[str, ...]]:
    combined: set[tuple[str, ...]] = {tuple()}
    for choices in parts:
        if not choices:
            return set()
        if len(combined) * len(choices) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python sequence analysis exceeded value bound")
        expanded = {
            prefix + choice
            for prefix in combined
            for choice in choices
        }
        if len(expanded) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python sequence analysis exceeded value bound")
        if any(
            sum(len(value) for value in sequence) > MAX_STATIC_STRING_LENGTH
            for sequence in expanded
        ):
            raise AssertionError("static Python sequence analysis exceeded length bound")
        combined = expanded
    return combined


def _is_identity_sequence_expression(node: ast.AST, target: str) -> bool:
    if isinstance(node, ast.Name):
        return node.id == target
    return (
        isinstance(node, ast.Subscript)
        and isinstance(node.value, ast.Name)
        and node.value.id == target
        and isinstance(node.slice, ast.Slice)
        and node.slice.lower is None
        and node.slice.upper is None
        and node.slice.step is None
    )


def _static_python_objects(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str] = frozenset(),
) -> list[object]:
    """Fold a bounded literal-only Python value lattice without execution."""

    if isinstance(node, ast.Constant) and isinstance(
        node.value, (str, int, bool, type(None))
    ):
        return [node.value]
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return []
        values: list[object] = []
        for definition in definitions.get(node.id, []):
            choices = _static_python_objects(
                definition, definitions, resolving | frozenset({node.id})
            )
            if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python object analysis exceeded value bound")
            values.extend(choices)
        return values
    if isinstance(node, ast.NamedExpr):
        return _static_python_objects(node.value, definitions, resolving)
    if isinstance(node, ast.IfExp):
        if isinstance(node.test, ast.Constant) and isinstance(node.test.value, bool):
            branch = node.body if node.test.value else node.orelse
            return _static_python_objects(branch, definitions, resolving)
        left = _static_python_objects(node.body, definitions, resolving)
        right = _static_python_objects(node.orelse, definitions, resolving)
        if len(left) + len(right) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python object analysis exceeded value bound")
        return left + right
    if isinstance(node, ast.BoolOp):
        parts = [
            _static_python_objects(value, definitions, resolving)
            for value in node.values
        ]
        combinations: list[tuple[object, ...]] = [tuple()]
        for choices in parts:
            if not choices:
                return []
            if len(combinations) * len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python object analysis exceeded value bound")
            combinations = [
                prefix + (choice,)
                for prefix in combinations
                for choice in choices
            ]
        results: list[object] = []
        for combination in combinations:
            result = combination[-1]
            if isinstance(node.op, ast.And):
                for choice in combination:
                    result = choice
                    if not choice:
                        break
            elif isinstance(node.op, ast.Or):
                for choice in combination:
                    result = choice
                    if choice:
                        break
            else:
                return []
            results.append(result)
        return results
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, (ast.UAdd, ast.USub)):
        values = _static_python_objects(node.operand, definitions, resolving)
        return [
            value if isinstance(node.op, ast.UAdd) else -value
            for value in values
            if isinstance(value, int) and not isinstance(value, bool)
        ]
    if isinstance(node, (ast.List, ast.Tuple)):
        combinations: list[list[object]] = [[]]
        for element in node.elts:
            choices = _static_python_objects(
                element.value if isinstance(element, ast.Starred) else element,
                definitions,
                resolving,
            )
            expansions: list[list[object]] = []
            for choice in choices:
                if isinstance(element, ast.Starred):
                    if not isinstance(choice, (list, tuple)):
                        continue
                    expansions.append(list(choice))
                else:
                    expansions.append([choice])
            if not expansions:
                return []
            if len(combinations) * len(expansions) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python object analysis exceeded value bound")
            combinations = [
                prefix + expansion
                for prefix in combinations
                for expansion in expansions
            ]
            if any(len(value) > MAX_STATIC_STRING_VALUES for value in combinations):
                raise AssertionError("static Python sequence analysis exceeded value bound")
        constructor = list if isinstance(node, ast.List) else tuple
        return [constructor(value) for value in combinations]
    if isinstance(node, ast.Dict):
        mappings: list[dict[object, object]] = [{}]
        for key_node, value_node in zip(node.keys, node.values):
            if key_node is None:
                choices = [
                    value
                    for value in _static_python_objects(
                        value_node, definitions, resolving
                    )
                    if isinstance(value, dict)
                ]
                if len(mappings) * len(choices) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python mapping analysis exceeded value bound")
                mappings = [
                    {**existing, **choice}
                    for existing in mappings
                    for choice in choices
                ]
                continue
            keys = _static_python_objects(key_node, definitions, resolving)
            values = _static_python_objects(value_node, definitions, resolving)
            keys = [value for value in keys if isinstance(value, (str, int))]
            if len(mappings) * len(keys) * len(values) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python mapping analysis exceeded value bound")
            mappings = [
                {**existing, key: value}
                for existing in mappings
                for key in keys
                for value in values
            ]
        return mappings
    if (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Name)
        and node.func.id == "dict"
        and len(node.args) <= 1
    ):
        sources = (
            _static_python_objects(node.args[0], definitions, resolving)
            if node.args
            else [{}]
        )
        mappings: list[dict[object, object]] = []
        for source in sources:
            try:
                mapping = dict(source)  # type: ignore[arg-type]
            except (TypeError, ValueError):
                continue
            mappings.append(mapping)
        for keyword in node.keywords:
            if keyword.arg is None:
                choices = [
                    value
                    for value in _static_python_objects(
                        keyword.value, definitions, resolving
                    )
                    if isinstance(value, dict)
                ]
                if len(mappings) * len(choices) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python mapping analysis exceeded value bound")
                mappings = [
                    {**existing, **choice}
                    for existing in mappings
                    for choice in choices
                ]
            else:
                choices = _static_python_objects(
                    keyword.value, definitions, resolving
                )
                if len(mappings) * len(choices) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python mapping analysis exceeded value bound")
                mappings = [
                    {**existing, keyword.arg: choice}
                    for existing in mappings
                    for choice in choices
                ]
        return mappings
    if isinstance(node, ast.BinOp):
        left = _static_python_objects(node.left, definitions, resolving)
        right = _static_python_objects(node.right, definitions, resolving)
        if len(left) * len(right) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python object analysis exceeded value bound")
        values: list[object] = []
        for left_value in left:
            for right_value in right:
                try:
                    if isinstance(node.op, ast.Add) and (
                        isinstance(left_value, type(right_value))
                        and isinstance(left_value, (str, list, tuple, int))
                    ):
                        value = left_value + right_value  # type: ignore[operator]
                    elif (
                        isinstance(node.op, ast.Sub)
                        and isinstance(left_value, int)
                        and not isinstance(left_value, bool)
                        and isinstance(right_value, int)
                        and not isinstance(right_value, bool)
                    ):
                        value = left_value - right_value
                    elif (
                        isinstance(node.op, ast.BitOr)
                        and isinstance(left_value, dict)
                        and isinstance(right_value, dict)
                    ):
                        value = {**left_value, **right_value}
                    elif isinstance(node.op, ast.Mult):
                        if isinstance(left_value, int) and isinstance(
                            right_value, (str, list, tuple)
                        ):
                            value = _checked_static_repeat(right_value, left_value)
                        elif isinstance(right_value, int) and isinstance(
                            left_value, (str, list, tuple)
                        ):
                            value = _checked_static_repeat(left_value, right_value)
                        else:
                            continue
                    else:
                        continue
                except (MemoryError, OverflowError):
                    raise AssertionError(
                        "static Python object analysis exceeded resource bound"
                    ) from None
                if isinstance(value, str) and len(value) > MAX_STATIC_STRING_LENGTH:
                    raise AssertionError("static Python string analysis exceeded length bound")
                if isinstance(value, (list, tuple, dict)) and len(value) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python object analysis exceeded value bound")
                values.append(value)
        return values
    if isinstance(node, ast.Subscript):
        sources = _static_python_objects(node.value, definitions, resolving)
        if isinstance(node.slice, ast.Slice):
            bound_choices: list[list[int | None]] = []
            for bound in (node.slice.lower, node.slice.upper, node.slice.step):
                if bound is None:
                    bound_choices.append([None])
                else:
                    bound_choices.append([
                        value
                        for value in _static_python_objects(
                            bound, definitions, resolving
                        )
                        if isinstance(value, int) and not isinstance(value, bool)
                    ])
            selectors: list[object] = [
                slice(lower, upper, step)
                for lower in bound_choices[0]
                for upper in bound_choices[1]
                for step in bound_choices[2]
            ]
        else:
            selectors = _static_python_objects(node.slice, definitions, resolving)
        values: list[object] = []
        operations = 0
        for source in sources:
            if not isinstance(source, (str, list, tuple, dict)):
                continue
            for selector in selectors:
                operations += 1
                if operations > MAX_STATIC_STRING_VALUES * MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python object analysis exceeded work bound")
                try:
                    values.append(source[selector])  # type: ignore[index]
                except (IndexError, KeyError, TypeError, ValueError):
                    continue
                if len(values) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Python object analysis exceeded value bound")
        return values
    return []


def _static_python_outer_row_nodes(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str],
) -> list[ast.AST]:
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return []
        rows: list[ast.AST] = []
        for definition in definitions.get(node.id, []):
            rows.extend(
                _static_python_outer_row_nodes(
                    definition, definitions, resolving | frozenset({node.id})
                )
            )
            if len(rows) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python outer-row analysis exceeded value bound")
        return rows
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        rows = (
            _static_python_outer_row_nodes(node.left, definitions, resolving)
            + _static_python_outer_row_nodes(node.right, definitions, resolving)
        )
        if len(rows) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python outer-row analysis exceeded value bound")
        return rows
    if not isinstance(node, (ast.List, ast.Tuple)):
        return []
    rows = []
    for value in node.elts:
        if isinstance(value, ast.Starred):
            rows.extend(
                _static_python_outer_row_nodes(value.value, definitions, resolving)
            )
        else:
            rows.append(value)
        if len(rows) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python outer-row analysis exceeded value bound")
    return rows


def _static_object_ast(value: object) -> ast.AST | None:
    if isinstance(value, (str, int, bool, type(None))):
        return ast.Constant(value=value)
    if isinstance(value, list):
        elements = [_static_object_ast(item) for item in value]
        if any(element is None for element in elements):
            return None
        return ast.List(elts=elements, ctx=ast.Load())  # type: ignore[arg-type]
    if isinstance(value, tuple):
        elements = [_static_object_ast(item) for item in value]
        if any(element is None for element in elements):
            return None
        return ast.Tuple(elts=elements, ctx=ast.Load())  # type: ignore[arg-type]
    if isinstance(value, dict):
        keys = [_static_object_ast(key) for key in value]
        values = [_static_object_ast(item) for item in value.values()]
        if any(item is None for item in [*keys, *values]):
            return None
        return ast.Dict(keys=keys, values=values)  # type: ignore[arg-type]
    return None


def _bind_static_comprehension_target(
    target: ast.AST,
    value: object,
    definitions: dict[str, list[ast.AST]],
) -> dict[str, list[ast.AST]] | None:
    bound = {name: list(nodes) for name, nodes in definitions.items()}

    def bind(item_target: ast.AST, item_value: object) -> bool:
        if isinstance(item_target, ast.Name):
            node = _static_object_ast(item_value)
            if node is None:
                return False
            bound[item_target.id] = [node]
            return True
        if isinstance(item_target, ast.Starred):
            return bind(item_target.value, item_value)
        if not isinstance(item_target, (ast.List, ast.Tuple)) or not isinstance(
            item_value, (list, tuple)
        ):
            return False
        starred = [
            index
            for index, element in enumerate(item_target.elts)
            if isinstance(element, ast.Starred)
        ]
        if len(starred) > 1:
            return False
        if not starred and len(item_target.elts) != len(item_value):
            return False
        if starred and len(item_value) < len(item_target.elts) - 1:
            return False
        for index, element in enumerate(item_target.elts):
            if isinstance(element, ast.Starred):
                trailing = len(item_target.elts) - index - 1
                selected: object = list(
                    item_value[index:len(item_value) - trailing if trailing else None]
                )
            elif not starred or index < starred[0]:
                selected = item_value[index]
            else:
                selected = item_value[index - len(item_target.elts)]
            if not bind(element, selected):
                return False
        return True

    return bound if bind(target, value) else None


def _static_comprehension_sequence(
    node: ast.GeneratorExp | ast.ListComp,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str],
) -> list[tuple[str, ...]]:
    if not 1 <= len(node.generators) <= 2:
        return []

    def expand(
        generator_index: int,
        scoped_definitions: dict[str, list[ast.AST]],
    ) -> list[tuple[str, ...]]:
        if generator_index == len(node.generators):
            return [
                (choice,)
                for choice in static_python_strings(
                    node.elt, scoped_definitions, resolving
                )
            ]
        generator = node.generators[generator_index]
        if generator.is_async:
            return []
        results: list[tuple[str, ...]] = []
        for iterable in _static_python_objects(
            generator.iter, scoped_definitions, resolving
        ):
            if not isinstance(iterable, (list, tuple)):
                continue
            sequence_options: set[tuple[str, ...]] = {tuple()}
            for item in iterable:
                bound = _bind_static_comprehension_target(
                    generator.target, item, scoped_definitions
                )
                if bound is None:
                    sequence_options = set()
                    break
                suffixes = expand(generator_index + 1, bound)
                sequence_options = _static_sequence_product([
                    list(sequence_options), suffixes
                ])
            results.extend(sorted(sequence_options))
            if len(results) > MAX_STATIC_STRING_VALUES:
                raise AssertionError(
                    "static Python comprehension analysis exceeded value bound"
                )
        return results

    return expand(0, definitions)


def _static_python_sequence(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str],
) -> list[tuple[str, ...]]:
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return []
        sequences: set[tuple[str, ...]] = set()
        for definition in definitions.get(node.id, []):
            choices = _static_python_sequence(
                definition, definitions, resolving | frozenset({node.id})
            )
            if len(sequences) + len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python sequence analysis exceeded value bound")
            sequences.update(choices)
        return sorted(sequences)
    if isinstance(node, ast.IfExp):
        if isinstance(node.test, ast.Constant) and isinstance(node.test.value, bool):
            branch = node.body if node.test.value else node.orelse
            return _static_python_sequence(branch, definitions, resolving)
        return sorted(set(
            _static_python_sequence(node.body, definitions, resolving)
            + _static_python_sequence(node.orelse, definitions, resolving)
        ))
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left = _static_python_sequence(node.left, definitions, resolving)
        right = _static_python_sequence(node.right, definitions, resolving)
        return sorted(_static_sequence_product([left, right]))
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Mult):
        sequences = {
            tuple(value)
            for value in _static_python_objects(node, definitions, resolving)
            if isinstance(value, (list, tuple))
            and all(isinstance(item, str) for item in value)
        }
        if len(sequences) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python sequence analysis exceeded value bound")
        return sorted(sequences)
    if isinstance(node, (ast.GeneratorExp, ast.ListComp)):
        # Filters are deliberately treated as potentially true.  This
        # over-approximation prevents a static filter from hiding a protected
        # reconstruction without executing arbitrary Python.
        return _static_comprehension_sequence(node, definitions, resolving)
    if (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Name)
        and not node.keywords
    ):
        sequences: list[tuple[str, ...]] = []
        if (
            node.func.id == "map"
            and len(node.args) == 2
            and isinstance(node.args[0], ast.Name)
            and node.args[0].id == "str"
        ):
            sequences = _static_python_sequence(
                node.args[1], definitions, resolving
            )
        elif (
            node.func.id == "filter"
            and len(node.args) == 2
            and isinstance(node.args[0], ast.Constant)
            and node.args[0].value is None
        ):
            sequences = [
                tuple(value for value in sequence if value)
                for sequence in _static_python_sequence(
                    node.args[1], definitions, resolving
                )
            ]
        elif node.func.id == "reversed" and len(node.args) == 1:
            sequences = [
                tuple(reversed(sequence))
                for sequence in _static_python_sequence(
                    node.args[0], definitions, resolving
                )
            ]
        if sequences:
            if len(sequences) > MAX_STATIC_STRING_VALUES or any(
                len(sequence) > MAX_STATIC_STRING_VALUES for sequence in sequences
            ):
                raise AssertionError(
                    "static Python sequence analysis exceeded value bound"
                )
            return sorted(set(sequences))
    if isinstance(node, ast.Subscript):
        sequences = _static_python_sequence(node.value, definitions, resolving)
        selectors: list[int | slice] = []
        if isinstance(node.slice, ast.Slice):
            bound_choices: list[set[int | None]] = []
            for bound in (node.slice.lower, node.slice.upper, node.slice.step):
                if bound is None:
                    bound_choices.append({None})
                else:
                    choices = _static_python_integers(bound, definitions, resolving)
                    if not choices:
                        return []
                    bound_choices.append(set(choices))
            selectors.extend(
                slice(lower, upper, step)
                for lower in bound_choices[0]
                for upper in bound_choices[1]
                for step in bound_choices[2]
            )
        else:
            selectors.extend(
                sorted(_static_python_integers(node.slice, definitions, resolving))
            )
        selected: set[tuple[str, ...]] = set()
        for sequence in sequences:
            for selector in selectors:
                try:
                    value = sequence[selector]
                except (IndexError, ValueError):
                    continue
                selected.add(value if isinstance(value, tuple) else (value,))
        return sorted(selected)
    if not isinstance(node, (ast.List, ast.Tuple)):
        return []
    parts: list[list[tuple[str, ...]]] = []
    for value in node.elts:
        if isinstance(value, ast.Starred):
            parts.append(
                _static_python_sequence(value.value, definitions, resolving)
            )
        else:
            parts.append([
                (choice,)
                for choice in static_python_strings(value, definitions, resolving)
            ])
    return sorted(_static_sequence_product(parts))


def _static_python_integers(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str],
) -> set[int]:
    if (
        isinstance(node, ast.Constant)
        and isinstance(node.value, int)
        and not isinstance(node.value, bool)
    ):
        return {node.value}
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return set()
        values: set[int] = set()
        for definition in definitions.get(node.id, []):
            choices = _static_python_integers(
                definition, definitions, resolving | frozenset({node.id})
            )
            if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python integer analysis exceeded value bound")
            values.update(choices)
        return values
    if isinstance(node, ast.NamedExpr):
        return _static_python_integers(node.value, definitions, resolving)
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, (ast.UAdd, ast.USub)):
        values = _static_python_integers(node.operand, definitions, resolving)
        return values if isinstance(node.op, ast.UAdd) else {-value for value in values}
    if isinstance(node, ast.IfExp):
        if isinstance(node.test, ast.Constant) and isinstance(node.test.value, bool):
            branch = node.body if node.test.value else node.orelse
            return _static_python_integers(branch, definitions, resolving)
        return (
            _static_python_integers(node.body, definitions, resolving)
            | _static_python_integers(node.orelse, definitions, resolving)
        )
    if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Sub)):
        left = _static_python_integers(node.left, definitions, resolving)
        right = _static_python_integers(node.right, definitions, resolving)
        if isinstance(node.op, ast.Add):
            return {left_value + right_value for left_value in left for right_value in right}
        return {left_value - right_value for left_value in left for right_value in right}
    return set()


def _static_python_mappings(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str],
) -> list[dict[str, str]]:
    object_mappings = [
        value
        for value in _static_python_objects(node, definitions, resolving)
        if isinstance(value, dict)
        and all(
            isinstance(key, str) and isinstance(item, str)
            for key, item in value.items()
        )
    ]
    if object_mappings:
        if len(object_mappings) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python mapping analysis exceeded value bound")
        return [dict(value) for value in object_mappings]
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return []
        mappings: list[dict[str, str]] = []
        for definition in definitions.get(node.id, []):
            choices = _static_python_mappings(
                definition, definitions, resolving | frozenset({node.id})
            )
            if len(mappings) + len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python mapping analysis exceeded value bound")
            mappings.extend(choices)
        return mappings
    if isinstance(node, ast.IfExp):
        if isinstance(node.test, ast.Constant) and isinstance(node.test.value, bool):
            branch = node.body if node.test.value else node.orelse
            return _static_python_mappings(branch, definitions, resolving)
        return (
            _static_python_mappings(node.body, definitions, resolving)
            + _static_python_mappings(node.orelse, definitions, resolving)
        )
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
        left = _static_python_mappings(node.left, definitions, resolving)
        right = _static_python_mappings(node.right, definitions, resolving)
        merged: list[dict[str, str]] = []
        for left_value in left:
            for right_value in right:
                merged.append({**left_value, **right_value})
        if len(merged) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python mapping analysis exceeded value bound")
        return merged
    if (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Name)
        and node.func.id == "dict"
        and len(node.args) <= 1
    ):
        mappings = (
            _static_python_mappings(node.args[0], definitions, resolving)
            if node.args
            else [{}]
        )
        if not mappings:
            return []
        for keyword in node.keywords:
            if keyword.arg is None:
                choices = _static_python_mappings(
                    keyword.value, definitions, resolving
                )
                mappings = [
                    {**existing, **choice}
                    for existing in mappings
                    for choice in choices
                ]
            else:
                values = static_python_strings(
                    keyword.value, definitions, resolving
                )
                mappings = [
                    {**existing, keyword.arg: value}
                    for existing in mappings
                    for value in values
                ]
            if len(mappings) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python mapping analysis exceeded value bound")
        return mappings
    if isinstance(node, (ast.List, ast.Tuple)):
        mappings: list[dict[str, str]] = [{}]
        for pair in node.elts:
            if (
                not isinstance(pair, (ast.List, ast.Tuple))
                or len(pair.elts) != 2
            ):
                return []
            keys = static_python_strings(pair.elts[0], definitions, resolving)
            values = static_python_strings(pair.elts[1], definitions, resolving)
            mappings = [
                {**existing, key: value}
                for existing in mappings
                for key in keys
                for value in values
            ]
            if len(mappings) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python mapping analysis exceeded value bound")
        return mappings
    if not isinstance(node, ast.Dict):
        return []

    mappings: list[dict[str, str]] = [{}]
    for key_node, value_node in zip(node.keys, node.values):
        if key_node is None:
            choices = _static_python_mappings(value_node, definitions, resolving)
            expanded: list[dict[str, str]] = []
            for existing in mappings:
                for choice in choices:
                    if existing.keys() & choice.keys():
                        continue
                    expanded.append({**existing, **choice})
            mappings = expanded
        else:
            keys = static_python_strings(key_node, definitions, resolving)
            values = static_python_strings(value_node, definitions, resolving)
            expanded = []
            for existing in mappings:
                for key in keys:
                    if key in existing:
                        continue
                    for value in values:
                        expanded.append({**existing, key: value})
            mappings = expanded
        if len(mappings) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python mapping analysis exceeded value bound")
    return mappings


def static_python_strings(
    node: ast.AST,
    definitions: dict[str, list[ast.AST]],
    resolving: frozenset[str] = frozenset(),
) -> set[str]:
    """Conservatively fold non-executing Python string expressions.

    This is intentionally a small static interpreter, not ``eval``.  It
    recognizes the ordinary compile-time reconstruction forms that could hide
    a protected candidate while failing closed on analysis expansion.
    """

    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return {node.value}
    if isinstance(node, ast.Name):
        if node.id in resolving:
            return set()
        values: set[str] = set()
        for definition in definitions.get(node.id, []):
            choices = static_python_strings(
                definition, definitions, resolving | frozenset({node.id})
            )
            if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Python string analysis exceeded value bound")
            values.update(choices)
        return values
    if isinstance(node, ast.NamedExpr):
        return static_python_strings(node.value, definitions, resolving)
    if isinstance(node, ast.BoolOp):
        values = {
            value
            for value in _static_python_objects(node, definitions, resolving)
            if isinstance(value, str)
        }
        if len(values) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python string analysis exceeded value bound")
        return values
    if isinstance(node, ast.IfExp):
        if isinstance(node.test, ast.Constant) and isinstance(node.test.value, bool):
            branch = node.body if node.test.value else node.orelse
            return static_python_strings(branch, definitions, resolving)
        return (
            static_python_strings(node.body, definitions, resolving)
            | static_python_strings(node.orelse, definitions, resolving)
        )
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        return _combine_static_parts([
            static_python_strings(node.left, definitions, resolving),
            static_python_strings(node.right, definitions, resolving),
        ])
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Mult):
        right_counts = _static_python_integers(node.right, definitions, resolving)
        left_counts = _static_python_integers(node.left, definitions, resolving)
        if right_counts:
            strings = static_python_strings(node.left, definitions, resolving)
            counts = right_counts
        elif left_counts:
            strings = static_python_strings(node.right, definitions, resolving)
            counts = left_counts
        else:
            return set()
        values = {
            _checked_static_repeat(value, count)
            for value in strings
            for count in counts
        }
        if any(len(value) > MAX_STATIC_STRING_LENGTH for value in values):
            raise AssertionError("static Python string analysis exceeded length bound")
        return values
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Mod):
        templates = static_python_strings(node.left, definitions, resolving)
        arguments = _static_python_objects(node.right, definitions, resolving)
        if not arguments:
            arguments = list(static_python_strings(node.right, definitions, resolving))
        if len(arguments) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Python format analysis exceeded value bound")
        values: set[str] = set()
        for template in templates:
            _preflight_percent_template(template)
            for argument in arguments:
                try:
                    value = template % argument
                except (TypeError, ValueError):
                    continue
                if isinstance(value, str):
                    if len(value) > MAX_STATIC_STRING_LENGTH:
                        raise AssertionError(
                            "static Python percent format exceeded allocation bound"
                        )
                    values.add(value)
        return values
    if isinstance(node, ast.JoinedStr):
        parts: list[set[str]] = []
        for value in node.values:
            if isinstance(value, ast.FormattedValue):
                choices = static_python_strings(value.value, definitions, resolving)
                if value.conversion == ord("r"):
                    choices = {repr(choice) for choice in choices}
                elif value.conversion == ord("a"):
                    choices = {ascii(choice) for choice in choices}
                elif value.conversion not in (-1, ord("s")):
                    return set()
                if value.format_spec is not None:
                    specs = static_python_strings(
                        value.format_spec, definitions, resolving
                    )
                    formatted: set[str] = set()
                    for choice in choices:
                        for spec in specs:
                            _preflight_format_spec(spec)
                            try:
                                formatted.add(format(choice, spec))
                            except ValueError:
                                continue
                    choices = formatted
                parts.append(choices)
            else:
                parts.append(static_python_strings(value, definitions, resolving))
        return _combine_static_parts(parts)
    if isinstance(node, ast.Subscript):
        sources = static_python_strings(node.value, definitions, resolving)
        mapping_keys = static_python_strings(node.slice, definitions, resolving)
        mappings = _static_python_mappings(node.value, definitions, resolving)
        selectors: list[int | slice] = []
        if not isinstance(node.slice, ast.Slice):
            selectors.extend(
                sorted(_static_python_integers(node.slice, definitions, resolving))
            )
        else:
            bound_choices: list[set[int | None]] = []
            for bound in (node.slice.lower, node.slice.upper, node.slice.step):
                if bound is None:
                    bound_choices.append({None})
                else:
                    choices = _static_python_integers(bound, definitions, resolving)
                    if not choices:
                        return set()
                    bound_choices.append(set(choices))
            selectors.extend(
                slice(lower, upper, step)
                for lower in bound_choices[0]
                for upper in bound_choices[1]
                for step in bound_choices[2]
            )
        values: set[str] = set()
        values.update(
            value
            for value in _static_python_objects(node, definitions, resolving)
            if isinstance(value, str)
        )
        sequences = _static_python_sequence(node.value, definitions, resolving)
        for source in sources:
            for selector in selectors:
                try:
                    values.add(source[selector])
                except (IndexError, ValueError):
                    continue
        for sequence in sequences:
            for selector in selectors:
                if not isinstance(selector, int):
                    continue
                try:
                    values.add(sequence[selector])
                except IndexError:
                    continue
        for mapping in mappings:
            for key in mapping_keys:
                if key in mapping:
                    values.add(mapping[key])
        return values
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
        receiver = static_python_strings(node.func.value, definitions, resolving)
        if (
            node.func.attr == "replace"
            and len(node.args) == 2
            and not node.keywords
            and isinstance(node.args[0], ast.Constant)
            and isinstance(node.args[0].value, str)
            and node.args[0].value
        ):
            old = node.args[0].value
            replacements = static_python_strings(
                node.args[1], definitions, resolving
            )
            if len(receiver) * len(replacements) > MAX_STATIC_STRING_VALUES:
                raise AssertionError(
                    "static Python replace analysis exceeded value bound"
                )
            values: set[str] = set()
            for source in receiver:
                if source.count(old) != 1:
                    continue
                for replacement in replacements:
                    predicted_length = len(source) - len(old) + len(replacement)
                    if predicted_length > MAX_STATIC_STRING_LENGTH:
                        raise AssertionError(
                            "static Python replace exceeded allocation bound"
                        )
                    values.add(source.replace(old, replacement))
            return values
        if node.func.attr == "join" and len(node.args) == 1 and not node.keywords:
            sequences = _static_python_sequence(node.args[0], definitions, resolving)
            if len(receiver) * len(sequences) > MAX_STATIC_STRING_VALUES:
                raise AssertionError(
                    "static Python join analysis exceeded value bound"
                )
            values: set[str] = set()
            for separator in receiver:
                for sequence in sequences:
                    if len(sequence) > MAX_STATIC_STRING_VALUES:
                        raise AssertionError(
                            "static Python sequence analysis exceeded value bound"
                        )
                    predicted_length = sum(len(value) for value in sequence)
                    predicted_length += max(len(sequence) - 1, 0) * len(separator)
                    if predicted_length > MAX_STATIC_STRING_LENGTH:
                        raise AssertionError(
                            "static Python join exceeded allocation bound"
                        )
                    values.add(separator.join(sequence))
            return values
        if node.func.attr == "format":
            format_arguments = list(node.args)
            if (
                isinstance(node.func.value, ast.Name)
                and node.func.value.id == "str"
                and format_arguments
            ):
                receiver = static_python_strings(
                    format_arguments.pop(0), definitions, resolving
                )
            positional_combinations: list[tuple[object, ...]] = [tuple()]
            for argument in format_arguments:
                expansions: list[tuple[object, ...]] = []
                if isinstance(argument, ast.Starred):
                    expansions.extend(
                        tuple(value)
                        for value in _static_python_objects(
                            argument.value, definitions, resolving
                        )
                        if isinstance(value, (list, tuple))
                    )
                else:
                    expansions.extend(
                        (value,)
                        for value in _static_python_objects(
                            argument, definitions, resolving
                        )
                    )
                if not expansions:
                    expansions.extend(
                        (value,)
                        for value in static_python_strings(
                            argument, definitions, resolving
                        )
                    )
                if (
                    len(positional_combinations) * len(expansions)
                    > MAX_STATIC_STRING_VALUES
                ):
                    raise AssertionError(
                        "static Python format analysis exceeded value bound"
                    )
                positional_combinations = [
                    prefix + expansion
                    for prefix in positional_combinations
                    for expansion in expansions
                ]
            keyword_combinations: list[dict[str, object]] = [{}]
            for keyword in node.keywords:
                if keyword.arg is None:
                    mapping_choices = [
                        value
                        for value in _static_python_objects(
                            keyword.value, definitions, resolving
                        )
                        if isinstance(value, dict)
                        and all(isinstance(key, str) for key in value)
                    ]
                else:
                    mapping_choices = [
                        {keyword.arg: value}
                        for value in _static_python_objects(
                            keyword.value, definitions, resolving
                        )
                    ]
                if (
                    len(keyword_combinations) * len(mapping_choices)
                    > MAX_STATIC_STRING_VALUES
                ):
                    raise AssertionError(
                        "static Python format analysis exceeded value bound"
                    )
                keyword_combinations = [
                    {**existing, **choice}
                    for existing in keyword_combinations
                    for choice in mapping_choices
                    if not (existing.keys() & choice.keys())
                ]
            values: set[str] = set()
            for template in receiver:
                _preflight_brace_template(template)
                for positional_args in positional_combinations:
                    for keyword_args in keyword_combinations:
                        try:
                            rendered = template.format(
                                *positional_args, **keyword_args
                            )
                        except (AttributeError, IndexError, KeyError, TypeError, ValueError):
                            continue
                        if len(rendered) > MAX_STATIC_STRING_LENGTH:
                            raise AssertionError(
                                "static Python format exceeded allocation bound"
                            )
                        values.add(rendered)
            return values
        if (
            node.func.attr == "format_map"
            and len(node.args) == 1
            and not node.keywords
        ):
            mappings = [
                value
                for value in _static_python_objects(
                    node.args[0], definitions, resolving
                )
                if isinstance(value, dict)
            ]
            values: set[str] = set()
            for template in receiver:
                _preflight_brace_template(template)
                for mapping in mappings:
                    try:
                        rendered = template.format_map(mapping)
                    except (AttributeError, KeyError, TypeError, ValueError):
                        continue
                    if len(rendered) > MAX_STATIC_STRING_LENGTH:
                        raise AssertionError(
                            "static Python format exceeded allocation bound"
                        )
                    values.add(rendered)
            return values
    return set()


PYTHON_SCOPE_NODES = (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda)


def python_scope_nodes(scope: ast.AST) -> list[ast.AST]:
    nodes: list[ast.AST] = []
    stack = [scope]
    while stack:
        node = stack.pop()
        nodes.append(node)
        if node is not scope and isinstance(node, PYTHON_SCOPE_NODES):
            continue
        stack.extend(reversed(list(ast.iter_child_nodes(node))))
    return nodes


def python_string_definitions(tree: ast.AST) -> dict[str, list[ast.AST]]:
    definitions: dict[str, list[ast.AST]] = {}

    def bind_name(name: str, value: ast.AST) -> None:
        prior = list(definitions.get(name, []))
        definitions.setdefault(name, []).append(value)
        if not (
            prior
            and isinstance(value, ast.BinOp)
            and isinstance(value.op, ast.Add)
        ):
            return
        if isinstance(value.left, ast.Name) and value.left.id == name:
            definitions[name].append(
                ast.BinOp(left=prior[-1], op=ast.Add(), right=value.right)
            )
        if isinstance(value.right, ast.Name) and value.right.id == name:
            definitions[name].append(
                ast.BinOp(left=value.left, op=ast.Add(), right=prior[-1])
            )

    def bind_target(target: ast.AST, value: ast.AST) -> None:
        if isinstance(target, ast.Name):
            bind_name(target.id, value)
            return
        if isinstance(target, ast.Starred):
            bind_target(target.value, value)
            return
        if not isinstance(target, (ast.List, ast.Tuple)):
            return
        starred_indexes = [
            index
            for index, target_item in enumerate(target.elts)
            if isinstance(target_item, ast.Starred)
        ]
        for index, target_item in enumerate(target.elts):
            if isinstance(target_item, ast.Starred):
                trailing = len(target.elts) - index - 1
                selector: ast.AST = ast.Slice(
                    lower=ast.Constant(value=index),
                    upper=(ast.Constant(value=-trailing) if trailing else None),
                )
            else:
                selector = ast.Constant(
                    value=(
                        index
                        if not starred_indexes or index < starred_indexes[0]
                        else index - len(target.elts)
                    )
                )
            bind_target(
                target_item,
                ast.Subscript(value=value, slice=selector, ctx=ast.Load()),
            )

    if isinstance(tree, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
        positional = [*tree.args.posonlyargs, *tree.args.args]
        if tree.args.defaults:
            for argument, default in zip(
                positional[-len(tree.args.defaults):], tree.args.defaults
            ):
                bind_name(argument.arg, default)
        for argument, default in zip(tree.args.kwonlyargs, tree.args.kw_defaults):
            if default is not None:
                bind_name(argument.arg, default)

    assignment_nodes = sorted(
        (
            node
            for node in python_scope_nodes(tree)
            if isinstance(node, (ast.Assign, ast.AnnAssign, ast.NamedExpr, ast.AugAssign))
        ),
        key=lambda node: (getattr(node, "lineno", 0), getattr(node, "col_offset", 0)),
    )
    for node in assignment_nodes:
        if isinstance(node, ast.Assign):
            for target in node.targets:
                bind_target(target, node.value)
        elif isinstance(node, ast.AnnAssign):
            if node.value is not None:
                bind_target(node.target, node.value)
        elif isinstance(node, ast.NamedExpr) and isinstance(node.target, ast.Name):
            bind_name(node.target.id, node.value)
        elif (
            isinstance(node, ast.AugAssign)
            and isinstance(node.target, ast.Name)
            and isinstance(node.op, ast.Add)
        ):
            prior = definitions.get(node.target.id, [])
            if prior:
                definitions.setdefault(node.target.id, []).append(
                    ast.BinOp(left=prior[-1], op=ast.Add(), right=node.value)
                )
    return definitions


def _decode_static_literal(token: str) -> str | None:
    if len(token) < 2:
        return None
    if token[0] == "`" and token[-1] == "`":
        return token[1:-1].replace("\r", "")
    if token[0] == "'" and token[-1] == "'":
        return token[1:-1]
    if token[0] == '"' and token[-1] == '"':
        try:
            value = ast.literal_eval(token)
        except (SyntaxError, ValueError):
            return None
        return value if isinstance(value, str) else None
    return None


def _static_double_quoted_strings(text: str) -> set[str]:
    """Decode bounded YAML/template-style quoted literals without execution."""

    values: set[str] = set()
    index = 0
    while index < len(text):
        start = text.find('"', index)
        if start < 0:
            break
        end = start + 1
        while end < len(text):
            if text[end] == "\\":
                end += 2
                continue
            if text[end] == '"':
                break
            end += 1
        if end >= len(text):
            if re.search(r"\\(?:x|u|U)", text[start:]):
                raise AssertionError("unterminated escaped static string")
            break
        token = text[start:end + 1]
        decoded = _decode_static_literal(token)
        if decoded is None:
            if re.search(r"\\(?:x|u|U)", token):
                raise AssertionError("invalid escaped static string")
        else:
            if len(decoded) > MAX_STATIC_STRING_LENGTH:
                raise AssertionError("escaped static string exceeded length bound")
            values.add(decoded)
        index = end + 1
    return values


def _json_static_strings(value: object) -> list[str]:
    values: list[str] = []
    stack = [value]
    while stack:
        item = stack.pop()
        if isinstance(item, str):
            values.append(item)
        elif isinstance(item, dict):
            stack.extend(item.keys())
            stack.extend(item.values())
        elif isinstance(item, list):
            stack.extend(item)
        if len(values) > MAX_PROTECTED_STATIC_VALUES:
            raise AssertionError("JSON static string analysis exceeded value bound")
    return values


def _without_go_comments(text: str) -> str:
    output: list[str] = []
    index = 0
    state = "normal"
    while index < len(text):
        char = text[index]
        next_char = text[index + 1] if index + 1 < len(text) else ""
        if state == "line_comment":
            if char == "\n":
                output.append(char)
                state = "normal"
            else:
                output.append(" ")
        elif state == "block_comment":
            if char == "*" and next_char == "/":
                output.extend((" ", " "))
                index += 1
                state = "normal"
            else:
                output.append("\n" if char == "\n" else " ")
        elif state == "normal":
            if char == "/" and next_char == "/":
                output.extend((" ", " "))
                index += 1
                state = "line_comment"
            elif char == "/" and next_char == "*":
                output.extend((" ", " "))
                index += 1
                state = "block_comment"
            else:
                output.append(char)
                if char == '"':
                    state = "quoted"
                elif char == "'":
                    state = "rune"
                elif char == "`":
                    state = "raw"
        else:
            output.append(char)
            if state in {"quoted", "rune"} and char == "\\":
                if index + 1 < len(text):
                    output.append(text[index + 1])
                    index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
        index += 1
    return "".join(output)


def _go_concat_tokens(expression: str) -> list[tuple[str, str]]:
    tokens: list[tuple[str, str]] = []
    position = 0
    while position < len(expression):
        match = GO_CONCAT_TOKEN_RE.match(expression, position)
        if match is None:
            if expression[position:].strip():
                return []
            break
        kind = ""
        value = ""
        for candidate_kind in ("string", "name", "operator"):
            candidate_value = match.group(candidate_kind)
            if candidate_value is not None:
                kind = candidate_kind
                value = candidate_value
                break
        tokens.append((kind, value))
        position = match.end()
    return tokens


def _static_go_expression_strings(
    expression: str,
    definitions: dict[str, list[str]],
    string_types: frozenset[str] = frozenset({"string"}),
    resolving: frozenset[str] = frozenset(),
) -> set[str]:
    tokens = _go_concat_tokens(expression)
    if not tokens:
        return set()
    position = 0

    def parse_term() -> set[str]:
        nonlocal position
        if position >= len(tokens):
            return set()
        kind, value = tokens[position]
        if kind == "string":
            position += 1
            decoded = _decode_static_literal(value)
            return {decoded} if decoded is not None else set()
        if kind == "name":
            position += 1
            if (
                value in string_types
                and position < len(tokens)
                and tokens[position] == ("operator", "(")
            ):
                position += 1
                values = parse_expression()
                if position >= len(tokens) or tokens[position] != ("operator", ")"):
                    return set()
                position += 1
                return values
            if value in resolving:
                return set()
            choices: set[str] = set()
            for definition in definitions.get(value, []):
                definition_choices = _static_go_expression_strings(
                    definition,
                    definitions,
                    string_types,
                    resolving | frozenset({value}),
                )
                if len(choices) + len(definition_choices) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError("static Go string analysis exceeded value bound")
                choices.update(definition_choices)
            return choices
        if kind == "operator" and value == "(":
            position += 1
            values = parse_expression()
            if position >= len(tokens) or tokens[position] != ("operator", ")"):
                return set()
            position += 1
            return values
        return set()

    def parse_expression() -> set[str]:
        nonlocal position
        values = parse_term()
        while position < len(tokens) and tokens[position] == ("operator", "+"):
            position += 1
            values = _combine_static_parts([values, parse_term()])
        return values

    values = parse_expression()
    return values if position == len(tokens) else set()


def static_go_strings(text: str) -> set[str]:
    clean = _without_go_comments(text)
    definitions: dict[str, list[str]] = {}
    expressions: list[str] = []
    pending = ""
    static_work = 0

    def consume_static_work() -> None:
        nonlocal static_work
        static_work += 1
        if static_work > MAX_GO_STATIC_WINDOWS:
            raise AssertionError(
                "static Go expression analysis exceeded work bound"
            )

    def split_top_level(source: str, delimiter: str) -> list[str]:
        parts: list[str] = []
        start = 0
        state = "normal"
        depth = 0
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                elif char == "(":
                    depth += 1
                elif char == ")" and depth:
                    depth -= 1
                elif char == delimiter and depth == 0:
                    parts.append(source[start:index])
                    start = index + 1
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        parts.append(source[start:])
        return parts

    def split_call_top_level(source: str) -> list[str]:
        parts: list[str] = []
        start = 0
        state = "normal"
        nesting: list[str] = []
        closing = {"(": ")", "[": "]", "{": "}"}
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                elif char in closing:
                    nesting.append(char)
                elif char in closing.values() and nesting:
                    if closing[nesting[-1]] == char:
                        nesting.pop()
                elif char == "," and not nesting:
                    parts.append(source[start:index])
                    start = index + 1
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        parts.append(source[start:])
        return parts

    logical_lines: list[str] = []

    def parenthesis_depth(source: str) -> int:
        depth = 0
        state = "normal"
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                elif char == "(":
                    depth += 1
                elif char == ")":
                    depth -= 1
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        return depth

    def parenthesized_bodies(source: str) -> list[str]:
        bodies: list[str] = []
        starts: list[int] = []
        state = "normal"
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                elif char == "(":
                    starts.append(index + 1)
                elif char == ")" and starts:
                    start = starts.pop()
                    bodies.append(source[start:index])
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        return bodies

    def named_call_bodies(source: str, callee: str) -> list[str]:
        bodies: list[str] = []
        state = "normal"
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                    index += 1
                    continue
                if source.startswith(callee, index) and (
                    index == 0 or source[index - 1] not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_."
                ):
                    after = index + len(callee)
                    if after == len(source) or source[after] in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_":
                        index += 1
                        continue
                    while after < len(source) and source[after].isspace():
                        after += 1
                    if after < len(source) and source[after] == "(":
                        consume_static_work()
                        open_index = after
                        depth = 0
                        call_state = "normal"
                        cursor = open_index
                        while cursor < len(source):
                            call_char = source[cursor]
                            if call_state == "normal":
                                if call_char in {'"', "'", "`"}:
                                    call_state = {
                                        '"': "quoted", "'": "rune", "`": "raw"
                                    }[call_char]
                                elif call_char == "(":
                                    depth += 1
                                elif call_char == ")":
                                    depth -= 1
                                    if depth == 0:
                                        bodies.append(source[open_index + 1:cursor])
                                        index = open_index + 1
                                        break
                            elif call_state in {"quoted", "rune"} and call_char == "\\":
                                cursor += 1
                            elif (
                                (call_state == "quoted" and call_char == '"')
                                or (call_state == "rune" and call_char == "'")
                                or (call_state == "raw" and call_char == "`")
                            ):
                                call_state = "normal"
                            cursor += 1
                        else:
                            raise AssertionError("unterminated static Go call")
                        continue
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        return bodies

    def split_structural(source: str) -> list[str]:
        parts: list[str] = []
        start = 0
        state = "normal"
        index = 0
        while index < len(source):
            char = source[index]
            if state == "normal":
                if char in {'"', "'", "`"}:
                    state = {'"': "quoted", "'": "rune", "`": "raw"}[char]
                elif char in "{}":
                    parts.append(source[start:index])
                    start = index + 1
            elif state in {"quoted", "rune"} and char == "\\":
                index += 1
            elif (
                (state == "quoted" and char == '"')
                or (state == "rune" and char == "'")
                or (state == "raw" and char == "`")
            ):
                state = "normal"
            index += 1
        parts.append(source[start:])
        return parts

    for raw_line in clean.splitlines():
        stripped = raw_line.strip()
        if not stripped:
            continue
        candidate = f"{pending} {stripped}".strip() if pending else stripped
        paren_depth = parenthesis_depth(candidate)
        if candidate.rstrip().endswith("+") or paren_depth > 0 and not re.fullmatch(
            r"(?:const|var)\s*\(", candidate
        ):
            pending = candidate
            continue
        pending = ""
        inline_block = re.fullmatch(r"(const|var)\s*\((.*)\)\s*", candidate)
        if inline_block is not None and inline_block.group(2).strip():
            kind, body = inline_block.groups()
            logical_lines.append(f"{kind} (")
            logical_lines.extend(split_top_level(body, ";"))
            logical_lines.append(")")
        else:
            logical_lines.extend(split_top_level(candidate, ";"))
    if pending:
        logical_lines.extend(split_top_level(pending, ";"))

    scan_lines = [
        segment
        for logical_line in logical_lines
        for segment in split_structural(logical_line)
        if segment.strip()
    ]
    string_types = frozenset(
        {"string"}
        | set(
            re.findall(
                r"\btype\s+([A-Za-z_][A-Za-z0-9_]*)\s+"
                r"(?:(?:=|~)\s*)?string\b",
                clean,
            )
        )
    )
    assignment_operator_re = re.compile(r":=|(?<![=!<>])=(?!=)")
    assignment_names_re = re.compile(
        r"([A-Za-z_][A-Za-z0-9_]*(?:\s*,\s*[A-Za-z_][A-Za-z0-9_]*)*)"
        r"(?:\s+[A-Za-z_][A-Za-z0-9_]*)?$"
    )
    for raw_line in scan_lines:
        line = raw_line.strip()
        operator_match = assignment_operator_re.search(line)
        if operator_match is None:
            continue
        lhs = line[:operator_match.start()].strip()
        lhs = re.sub(r"^(?:(?:const|var|if|for|switch)\s+)+", "", lhs)
        names_match = assignment_names_re.search(lhs)
        if names_match is None:
            continue
        names_text = names_match.group(1)
        expression_text = line[operator_match.end():].strip()
        names = [name.strip() for name in names_text.split(",")]
        expression_parts = [
            part.strip() for part in split_top_level(expression_text, ",")
        ]
        if len(names) != len(expression_parts):
            continue
        for name, expression in zip(names, expression_parts):
            entries = definitions.setdefault(name, [])
            if len(entries) >= MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Go definition analysis exceeded value bound")
            entries.append(expression)
            expressions.append(expression)

    access_aliases: dict[str, str] = {}

    def bind_access(access: str, expression: str) -> None:
        alias = f"gateAStaticAccess{len(access_aliases)}"
        access_aliases[access] = alias
        definitions.setdefault(alias, []).append(expression.strip())

    composite_assignment_re = re.compile(
        r"\b([A-Za-z_][A-Za-z0-9_]*)\s*(?:"
        + re.escape(":" + "=")
        + r"|(?<![=!<>])=(?!=))\s*"
    )
    for assignment_match in composite_assignment_re.finditer(clean):
        name = assignment_match.group(1)
        tail = clean[assignment_match.end():]
        struct_match = re.match(
            r"struct\s*\{([^{}]*)\}\s*\{([^{}]*)\}", tail, re.DOTALL
        )
        if struct_match is not None:
            field_names: list[str] = []
            valid_fields = True
            for declaration in split_top_level(struct_match.group(1), ";"):
                identifiers = re.findall(r"[A-Za-z_][A-Za-z0-9_]*", declaration)
                if len(identifiers) < 2 or identifiers[-1] not in string_types:
                    valid_fields = False
                    break
                field_names.extend(identifiers[:-1])
            members = [
                part.strip()
                for part in split_top_level(struct_match.group(2), ",")
                if part.strip()
            ]
            if valid_fields and len(field_names) == len(members):
                for field_name, member in zip(field_names, members):
                    bind_access(f"{name}.{field_name}", member)
            continue
        sequence_match = re.match(
            r"\[\s*\d*\s*\]\s*([A-Za-z_][A-Za-z0-9_]*)\s*"
            r"\{([^{}]*)\}",
            tail,
            re.DOTALL,
        )
        if sequence_match is not None and sequence_match.group(1) in string_types:
            members = [
                part.strip()
                for part in split_top_level(sequence_match.group(2), ",")
                if part.strip()
            ]
            if len(members) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Go collection analysis exceeded value bound")
            for index, member in enumerate(members):
                bind_access(f"{name}[{index}]", member)
            continue
        mapping_match = re.match(
            r"map\s*\[\s*string\s*\]\s*"
            r"([A-Za-z_][A-Za-z0-9_]*)\s*\{([^{}]*)\}",
            tail,
            re.DOTALL,
        )
        if mapping_match is not None and mapping_match.group(1) in string_types:
            members = [
                part.strip()
                for part in split_top_level(mapping_match.group(2), ",")
                if part.strip()
            ]
            if len(members) > MAX_STATIC_STRING_VALUES:
                raise AssertionError("static Go collection analysis exceeded value bound")
            for member in members:
                pair = split_top_level(member, ":")
                if len(pair) != 2:
                    continue
                key = _decode_static_literal(pair[0].strip())
                if key is None:
                    continue
                bind_access(f'{name}[{json.dumps(key)}]', pair[1])

    values: set[str] = set()

    for body in named_call_bodies(clean, "fmt.Sprintf"):
        arguments = [part.strip() for part in split_call_top_level(body)]
        if len(arguments) != 3:
            continue
        templates = _static_go_expression_strings(
            arguments[0], definitions, string_types
        )
        if "%s%s" not in templates:
            continue
        choices = _combine_static_parts([
            _static_go_expression_strings(arguments[1], definitions, string_types),
            _static_go_expression_strings(arguments[2], definitions, string_types),
        ])
        if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Go string analysis exceeded value bound")
        values.update(choices)

    for body in named_call_bodies(clean, "strings.Join"):
        arguments = [part.strip() for part in split_call_top_level(body)]
        if len(arguments) != 2:
            continue
        separators = _static_go_expression_strings(
            arguments[1], definitions, string_types
        )
        if "" not in separators:
            continue
        sequence_match = re.fullmatch(
            r"\[\s*\]\s*string\s*\{(.*)\}", arguments[0], re.DOTALL
        )
        if sequence_match is None:
            continue
        members = [
            part.strip()
            for part in split_call_top_level(sequence_match.group(1))
            if part.strip()
        ]
        if len(members) != 2:
            continue
        choices = _combine_static_parts([
            _static_go_expression_strings(members[0], definitions, string_types),
            _static_go_expression_strings(members[1], definitions, string_types),
        ])
        if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Go string analysis exceeded value bound")
        values.update(choices)

    for expression in expressions:
        choices = _static_go_expression_strings(
            expression, definitions, string_types
        )
        if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static Go string analysis exceeded value bound")
        values.update(choices)

    for source in scan_lines:
        for access, alias in sorted(
            access_aliases.items(), key=lambda item: len(item[0]), reverse=True
        ):
            source = source.replace(access, alias)
        runs: list[list[str]] = []
        run: list[str] = []
        previous_end = 0
        for match in GO_STATIC_TOKEN_RE.finditer(source):
            gap = source[previous_end:match.start()]
            if run and gap.strip():
                runs.append(run)
                run = []
            run.append(match.group(0))
            previous_end = match.end()
        if run:
            runs.append(run)
        for run in runs:
            if "+" in run and len(run) > MAX_GO_STATIC_WINDOW_TOKENS:
                raise AssertionError("static Go expression exceeded token-window bound")
            for start in range(len(run)):
                stop = min(len(run), start + MAX_GO_STATIC_WINDOW_TOKENS)
                for end in range(start + 1, stop + 1):
                    window = run[start:end]
                    if "+" not in window:
                        continue
                    consume_static_work()
                    choices = _static_go_expression_strings(
                        "".join(window), definitions, string_types
                    )
                    if len(values) + len(choices) > MAX_STATIC_STRING_VALUES:
                        raise AssertionError("static Go string analysis exceeded value bound")
                    values.update(choices)
    return values


def static_shell_adjacent_strings(text: str) -> set[str]:
    values: set[str] = set()
    normalized = re.sub(r"\\\r?\n", "", text)
    for adjacent_match in SHELL_ADJACENT_LITERAL_RE.finditer(normalized):
        parts: list[str] = []
        for literal_match in SHELL_LITERAL_RE.finditer(adjacent_match.group(0)):
            value = _decode_static_literal(literal_match.group(0))
            if value is None:
                parts = []
                break
            parts.append(value)
        if len(parts) >= 2:
            combined = "".join(parts)
            if len(combined) > MAX_STATIC_STRING_LENGTH:
                raise AssertionError("static shell string analysis exceeded length bound")
            values.add(combined)

    def decode_ansi_c(value: str) -> str | None:
        output: list[str] = []
        index = 0
        simple = {
            "a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b",
            "f": "\f", "n": "\n", "r": "\r", "t": "\t",
            "v": "\v", "\\": "\\", "'": "'", '"': '"',
        }
        while index < len(value):
            if value[index] != "\\":
                output.append(value[index])
                index += 1
                continue
            if index + 1 >= len(value):
                return None
            escape = value[index + 1]
            if escape in simple:
                output.append(simple[escape])
                index += 2
                continue
            if escape == "x":
                match = re.match(r"[0-9A-Fa-f]{1,2}", value[index + 2:])
                if match is None:
                    return None
                output.append(chr(int(match.group(0), 16)))
                index += 2 + len(match.group(0))
                continue
            if escape in {"u", "U"}:
                width = 4 if escape == "u" else 8
                digits = value[index + 2:index + 2 + width]
                if len(digits) != width or re.fullmatch(
                    rf"[0-9A-Fa-f]{{{width}}}", digits
                ) is None:
                    return None
                codepoint = int(digits, 16)
                if codepoint > 0x10FFFF or 0xD800 <= codepoint <= 0xDFFF:
                    return None
                output.append(chr(codepoint))
                index += 2 + width
                continue
            if escape in "01234567":
                match = re.match(r"[0-7]{1,3}", value[index + 1:])
                if match is None:
                    return None
                output.append(chr(int(match.group(0), 8)))
                index += 1 + len(match.group(0))
                continue
            return None
        return "".join(output)

    def decode_word(word: str) -> list[str]:
        parts: list[str] = []
        index = 0
        while index < len(word):
            if word.startswith('$"', index):
                end = index + 2
                escaped = False
                while end < len(word):
                    if word[end] == '"' and not escaped:
                        break
                    if word[end] == "\\" and not escaped:
                        escaped = True
                    else:
                        escaped = False
                    end += 1
                if end >= len(word):
                    parts = []
                    break
                decoded = _decode_static_literal(word[index + 1:end + 1])
                if decoded is None:
                    parts = []
                    break
                parts.append(decoded)
                index = end + 1
            elif word.startswith("$'", index):
                end = index + 2
                escaped = False
                while end < len(word):
                    if word[end] == "'" and not escaped:
                        break
                    if word[end] == "\\" and not escaped:
                        escaped = True
                    else:
                        escaped = False
                    end += 1
                if end >= len(word):
                    parts = []
                    break
                decoded = decode_ansi_c(word[index + 2:end])
                if decoded is None:
                    parts = []
                    break
                parts.append(decoded)
                index = end + 1
            elif word[index] == "\\":
                if index + 1 >= len(word):
                    parts = []
                    break
                parts.append(word[index + 1])
                index += 2
            elif word[index] in {'"', "'"}:
                quote = word[index]
                end = index + 1
                escaped = False
                while end < len(word):
                    if word[end] == quote and not escaped:
                        break
                    escaped = word[end] == "\\" and not escaped
                    if word[end] != "\\":
                        escaped = False
                    end += 1
                if end >= len(word):
                    parts = []
                    break
                literal = word[index:end + 1]
                decoded = _decode_static_literal(literal)
                if decoded is None:
                    parts = []
                    break
                parts.append(decoded)
                index = end + 1
            else:
                end = index
                while end < len(word) and word[end] not in {'"', "'", "$", "`", "\\"}:
                    end += 1
                if end == index:
                    parts = []
                    break
                parts.append(word[index:end])
                index = end
        return parts

    definitions: dict[str, set[str]] = {}
    for match in re.finditer(
        r"(?m)(?:^|[;\n])\s*([A-Za-z_][A-Za-z0-9_]*)=([^\s;#]+)",
        normalized,
    ):
        name, word = match.groups()
        if "$" in word or "`" in word:
            continue
        parts = decode_word(word)
        if not parts:
            continue
        value = "".join(parts)
        if len(value) > MAX_STATIC_STRING_LENGTH:
            raise AssertionError("static shell string analysis exceeded length bound")
        definitions.setdefault(name, set()).add(value)
        if sum(len(choices) for choices in definitions.values()) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static shell definition analysis exceeded value bound")

    def expand_simple_variables(word: str) -> set[str]:
        if len(word) < 2 or word[0] != '"' or word[-1] != '"':
            return set()
        body = word[1:-1]
        variable_re = re.compile(
            r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)"
        )
        parts: list[set[str]] = []
        position = 0
        for match in variable_re.finditer(body):
            if match.start() != position:
                return set()
            name = match.group(1) or match.group(2)
            choices = definitions.get(name, set())
            if not choices:
                return set()
            parts.append(choices)
            position = match.end()
        if not parts or position != len(body):
            return set()
        return _combine_static_parts(parts)

    candidate_words = {
        match.group(1)
        for match in re.finditer(
            r"(?m)(?:^|[;\s])(?:[A-Za-z_][A-Za-z0-9_]*)=([^\s;#]+)",
            normalized,
        )
    }
    candidate_words.update(
        match.group(0)
        for match in re.finditer(r"[^\s;#]+", normalized)
        if (
            '"' in match.group(0)
            or "'" in match.group(0)
            or "\\" in match.group(0)
        )
    )
    for word in candidate_words:
        expanded = expand_simple_variables(word)
        if len(values) + len(expanded) > MAX_STATIC_STRING_VALUES:
            raise AssertionError("static shell string analysis exceeded value bound")
        values.update(expanded)
        parts = decode_word(word)
        if parts and (len(parts) >= 2 or "$'" in word):
            combined = "".join(parts)
            if len(combined) > MAX_STATIC_STRING_LENGTH:
                raise AssertionError("static shell string analysis exceeded length bound")
            values.add(combined)
    return values


def check_protected_candidates(path: Path, text: str) -> None:
    relative = path.relative_to(ROOT).as_posix()
    if len(text.encode("utf-8")) > MAX_PROTECTED_FILE_BYTES:
        raise AssertionError(f"protected candidate file exceeded byte bound: {relative}")

    token_count = 0
    hash_windows = 0
    static_value_count = 0
    seen_static_values: set[bytes] = set()

    def check_candidate(candidate: str) -> None:
        nonlocal hash_windows
        hash_windows += 1
        if hash_windows > MAX_PROTECTED_HASH_WINDOWS:
            raise AssertionError("protected candidate scan exceeded work bound")
        if candidate_sha256(candidate) in FORBIDDEN_CANDIDATE_SHA256:
            raise AssertionError(f"known protected candidate in {relative}")

    def scan_value(value: str, *, static: bool) -> None:
        nonlocal token_count, static_value_count
        encoded = value.encode("utf-8")
        if len(encoded) > MAX_PROTECTED_VALUE_BYTES:
            raise AssertionError("protected candidate value exceeded byte bound")
        if static:
            digest = hashlib.sha256(encoded).digest()
            if digest in seen_static_values:
                return
            static_value_count += 1
            if static_value_count > MAX_PROTECTED_STATIC_VALUES:
                raise AssertionError("protected candidate scan exceeded value bound")
            seen_static_values.add(digest)
        for match in PROTECTED_CANDIDATE_RE.finditer(value):
            token_count += 1
            if token_count > MAX_PROTECTED_TOKENS_PER_FILE:
                raise AssertionError("protected candidate scan exceeded token bound")
            token = match.group(0)
            check_candidate(token)
            trimmed = token.strip(".:")
            if trimmed and trimmed != token:
                check_candidate(trimmed)
            for length in FORBIDDEN_CANDIDATE_LENGTHS:
                if length > len(token):
                    continue
                for offset in range(len(token) - length + 1):
                    check_candidate(token[offset:offset + length])

    scan_value(text, static=False)
    if path.suffix == ".json":
        try:
            decoded_json = json.loads(text)
        except (TypeError, ValueError) as exc:
            raise AssertionError(f"invalid JSON source in {relative}") from exc
        for value in _json_static_strings(decoded_json):
            scan_value(value, static=True)
    elif path.suffix == ".py":
        try:
            tree = ast.parse(text, filename=relative)
        except SyntaxError as exc:
            raise AssertionError(f"invalid Python source in {relative}") from exc
        scopes: list[ast.AST] = [tree] + [
            node for node in ast.walk(tree) if isinstance(node, PYTHON_SCOPE_NODES)
        ]
        parents: dict[ast.AST, ast.AST] = {
            child: parent
            for parent in ast.walk(tree)
            for child in ast.iter_child_nodes(parent)
        }
        scope_set = set(scopes)
        scope_parents: dict[ast.AST, ast.AST | None] = {tree: None}
        for scope in scopes[1:]:
            parent = parents.get(scope)
            while parent is not None and parent not in scope_set:
                parent = parents.get(parent)
            scope_parents[scope] = parent
        local_definitions = {
            scope: python_string_definitions(scope) for scope in scopes
        }
        visible_cache: dict[ast.AST, dict[str, list[ast.AST]]] = {}

        def visible_definitions(scope: ast.AST) -> dict[str, list[ast.AST]]:
            if scope in visible_cache:
                return visible_cache[scope]
            parent = scope_parents[scope]
            visible = (
                {
                    name: list(nodes)
                    for name, nodes in visible_definitions(parent).items()
                }
                if parent is not None
                else {}
            )
            for name, nodes in local_definitions[scope].items():
                combined = visible.setdefault(name, [])
                if len(combined) + len(nodes) > MAX_STATIC_STRING_VALUES:
                    raise AssertionError(
                        "static Python lexical analysis exceeded value bound"
                    )
                combined.extend(nodes)
            visible_cache[scope] = visible
            return visible

        for scope in scopes:
            definitions = visible_definitions(scope)
            for name in definitions:
                for value in static_python_strings(
                    ast.Name(id=name, ctx=ast.Load()), definitions
                ):
                    scan_value(value, static=True)
            for node in python_scope_nodes(scope):
                if isinstance(node, ast.Name) and not isinstance(node.ctx, ast.Load):
                    continue
                for value in static_python_strings(node, definitions):
                    scan_value(value, static=True)
    elif path.suffix == ".go":
        for value in static_go_strings(text):
            scan_value(value, static=True)
    elif path.suffix in {".sh", ".bash", ".tmpl"}:
        for value in static_shell_adjacent_strings(text):
            scan_value(value, static=True)
        for value in _static_double_quoted_strings(text):
            scan_value(value, static=True)
    elif path.suffix in {".yaml", ".yml"}:
        for value in _static_double_quoted_strings(text):
            scan_value(value, static=True)


def check_text(path: Path, text: str) -> None:
    relative = path.relative_to(ROOT).as_posix()
    if relative.startswith("fixtures/"):
        check_public_fixture_keys(text, relative)
    check_protected_candidates(path, text)
    for pattern in KNOWN_TOKEN_PATTERNS:
        if pattern.search(text):
            raise AssertionError(f"credential-shaped token in {relative}")
    if CREDENTIALED_URI_RE.search(text):
        raise AssertionError(f"credential-bearing URI in {relative}")
    private_key_count = sum(text.count(marker) for marker in PRIVATE_KEY_MARKERS)
    private_key_end_count = text.count(PRIVATE_KEY_END)
    if private_key_count or private_key_end_count:
        if (
            relative != "internal/protocol/oidc_test.go"
            or private_key_count != 1
            or private_key_end_count != 1
            or "syntheticRSAPrivateKeyPEM" not in text
        ):
            raise AssertionError(f"private-key material outside the exact synthetic OIDC fixture in {relative}")
    for value in UUID_RE.findall(text):
        if not SYNTHETIC_UUID_RE.fullmatch(value):
            raise AssertionError(f"non-synthetic UUID in {relative}")
    for candidate in IPV4_RE.findall(text):
        try:
            address = ipaddress.ip_address(candidate)
        except ValueError as exc:
            raise AssertionError(f"invalid IP-shaped literal in {relative}") from exc
        if not address.is_loopback:
            raise AssertionError(f"non-loopback IP literal in {relative}")
    for candidate in IPV6_CANDIDATE_RE.findall(text):
        try:
            address = ipaddress.ip_address(candidate.split("%", 1)[0])
        except ValueError:
            continue
        if address.version == 6 and not address.is_loopback:
            raise AssertionError(f"non-loopback IPv6 literal in {relative}")
    for match in SCHEME_URI_RE.finditer(text):
        scheme, remainder = match.groups()
        if scheme.lower() == "synthetic":
            if not re.fullmatch(
                r"[a-z0-9-]+(?:/[A-Za-z0-9%_-]+)?", remainder
            ):
                raise AssertionError(f"synthetic label URI has selector shape in {relative}")
            continue
        if scheme.lower() not in {"http", "https"}:
            raise AssertionError(f"non-HTTP selector URI in {relative}")
        parsed = urlsplit(f"{scheme}://{remainder}")
        host = (parsed.hostname or "").lower()
        if not host or (host != "synthetic.invalid" and not host.endswith(".synthetic.invalid")):
            raise AssertionError(f"non-synthetic endpoint URL in {relative}")
    # The four exact-hash-locked Dockerfiles legitimately contain image tags,
    # digest prefixes, and numeric USER ownership that are syntactically
    # host:port-like. Everything else must use loopback for a bare selector.
    if not relative.startswith("docker/"):
        for host, port_text in BARE_HOST_PORT_RE.findall(text):
            port = int(port_text)
            if port > 65535:
                raise AssertionError(f"invalid bare host:port selector in {relative}")
            if host.lower() != "localhost":
                try:
                    address = ipaddress.ip_address(host)
                except ValueError:
                    address = None
                if address is None or not address.is_loopback:
                    raise AssertionError(f"non-loopback bare host:port selector in {relative}")
    for candidate in URL_RE.findall(text):
        parsed = urlsplit(candidate)
        host = (parsed.hostname or "").lower()
        if not host or (host != "synthetic.invalid" and not host.endswith(".synthetic.invalid")):
            raise AssertionError(f"non-synthetic endpoint URL in {relative}")
    # The Python test suite installs process-wide socket/URL guards before it
    # invokes its negative probes. Those deliberate calls prove the guard;
    # executable prototype code and fixtures may not contain call sites.
    if relative != "verify_gate_a.py" and not relative.startswith("tests/"):
        for marker in EXTERNAL_CALL_MARKERS:
            if marker in text:
                raise AssertionError(f"external-call marker {marker!r} in {relative}")


def go_imports(text: str) -> set[str]:
    imports: set[str] = set()
    in_block = False
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("//"):
            continue
        if not in_block:
            if line == "import (":
                in_block = True
                continue
            match = re.fullmatch(
                r'import\s+(?:[._A-Za-z][A-Za-z0-9_]*\s+)?["`]([^"`]+)["`]'
                r"(?:\s*//.*)?",
                line,
            )
            if match:
                imports.add(match.group(1))
            continue
        if line == ")":
            in_block = False
            continue
        match = re.fullmatch(
            r'(?:[._A-Za-z][A-Za-z0-9_]*\s+)?["`]([^"`]+)["`]'
            r"(?:\s*//.*)?",
            line,
        )
        if not match:
            raise AssertionError("malformed or dynamic Go import declaration")
        imports.add(match.group(1))
    if in_block:
        raise AssertionError("unterminated Go import declaration")
    return imports


def check_language_surface(path: Path, text: str) -> None:
    relative = path.relative_to(ROOT).as_posix()
    if path.suffix == ".go":
        imports = go_imports(text)
        unexpected = sorted(imports - ALLOWED_GO_IMPORTS)
        if unexpected:
            raise AssertionError(f"unapproved Go import in {relative}: {unexpected}")
    elif path.suffix == ".py":
        try:
            tree = ast.parse(text, filename=relative)
        except SyntaxError as exc:
            raise AssertionError(f"invalid Python source in {relative}") from exc
        imports: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imports.update(alias.name for alias in node.names)
                for alias in node.names:
                    if alias.name in {"socket", "urllib.request"} and alias.asname:
                        raise AssertionError(f"aliased network-guard import in {relative}")
            elif isinstance(node, ast.ImportFrom) and node.module:
                imports.add(node.module)
                if node.module in {"socket", "urllib", "urllib.request"}:
                    raise AssertionError(f"network module must use the exact guarded import in {relative}")
        expected = ALLOWED_PYTHON_IMPORTS.get(relative)
        if expected is None or imports != expected:
            raise AssertionError(
                f"Python import surface differs in {relative}: "
                f"missing={sorted((expected or set()) - imports)}, "
                f"extra={sorted(imports - (expected or set()))}"
            )


def active_yaml_lines(text: str) -> list[tuple[int, str]]:
    result: list[tuple[int, str]] = []
    for raw in text.splitlines():
        stripped = raw.lstrip()
        if not stripped or stripped.startswith("#"):
            continue
        result.append((len(raw) - len(stripped), stripped.rstrip()))
    return result


def check_workflows(files: list[tuple[Path, str]]) -> None:
    workflow_root = ROOT / "workflows"
    for path, text in files:
        if workflow_root not in path.parents:
            continue
        relative = path.relative_to(ROOT).as_posix()
        if path.suffix != ".tmpl":
            raise AssertionError(f"workflow fixture must be .tmpl: {relative}")
        expected_digest = EXPECTED_WORKFLOW_SHA256.get(relative)
        actual_digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
        if expected_digest is None or actual_digest != expected_digest:
            raise AssertionError(f"workflow fixture bytes differ from reviewed inert template: {relative}")
        active = active_yaml_lines(text)
        top_level_triggers = [
            line for indent, line in active
            if indent == 0 and line.startswith("on:")
        ]
        if top_level_triggers != ["on: []"]:
            raise AssertionError(f"workflow fixture is not inert: {relative}")
        forbidden = (
            "id-token: write",
            "secrets.",
            "workflow_dispatch",
            "curl ",
            "wget ",
            "doctl ",
            "gh api",
        )
        for marker in forbidden:
            if marker in text:
                raise AssertionError(f"active workflow marker {marker!r} in {relative}")
        permission_lines: list[str] = []
        in_permissions = False
        job_names: list[str] = []
        job_false_guards: set[str] = set()
        current_job = ""
        in_jobs = False
        for indent, line in active:
            if indent == 0:
                in_permissions = line == "permissions:"
                in_jobs = line == "jobs:"
                current_job = ""
                continue
            if in_permissions:
                if indent != 2:
                    raise AssertionError(f"malformed permissions in {relative}")
                permission_lines.append(line)
            if in_jobs and indent == 2 and line.endswith(":"):
                current_job = line[:-1]
                job_names.append(current_job)
            elif (
                in_jobs
                and indent == 4
                and line == "if: ${{ false }}"
                and current_job
            ):
                job_false_guards.add(current_job)
        if permission_lines != ["contents: read"]:
            raise AssertionError(
                f"workflow permissions are not exact read-only: {relative}"
            )
        if (
            not job_names
            or len(job_names) != len(set(job_names))
            or set(job_names) != job_false_guards
        ):
            raise AssertionError(
                f"every workflow job must have one active false guard: {relative}"
            )


def check_dockerfiles(files: list[tuple[Path, str]]) -> None:
    seen: set[str] = set()
    for path, text in files:
        relative = path.relative_to(ROOT).as_posix()
        if not relative.startswith("docker/"):
            continue
        seen.add(relative)
        expected_digest = EXPECTED_DOCKERFILE_SHA256.get(relative)
        actual_digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
        if expected_digest is None or actual_digest != expected_digest:
            raise AssertionError(f"Dockerfile bytes differ from reviewed inert image: {relative}")
        instructions = [
            line.split(None, 1)[0].upper()
            for line in text.splitlines()
            if line.strip() and not line.lstrip().startswith("#")
        ]
        if any(
            instruction not in {"FROM", "ENV", "WORKDIR", "COPY", "RUN", "USER", "ENTRYPOINT"}
            for instruction in instructions
        ):
            raise AssertionError(f"Dockerfile contains an unapproved instruction: {relative}")
        run_lines = [line.strip() for line in text.splitlines() if line.lstrip().upper().startswith("RUN ")]
        if len(run_lines) != 1 or not run_lines[0].startswith("RUN go build "):
            raise AssertionError(f"Dockerfile RUN surface is not the exact offline build: {relative}")
    if seen != set(EXPECTED_DOCKERFILE_SHA256):
        raise AssertionError("Dockerfile inventory differs from the reviewed four-role set")


def check_path_blob_manifest(files: list[Path]) -> None:
    manifest_path = ROOT / PATH_BLOB_MANIFEST
    lines = manifest_path.read_text(encoding="utf-8").splitlines()
    if lines[:3] != [
        "# recovery-boundary-path-blobs/v1",
        "# fields: git_blob_sha1 content_sha256 path",
        "# self: excluded",
    ]:
        raise AssertionError("path/blob manifest header differs")
    entries: dict[str, tuple[str, str]] = {}
    for line in lines[3:]:
        match = re.fullmatch(r"([0-9a-f]{40}) ([0-9a-f]{64})  ([!-~]+)", line)
        if not match:
            raise AssertionError("malformed path/blob manifest entry")
        git_blob_sha1, content_sha256, relative = match.groups()
        if relative in entries:
            raise AssertionError(f"duplicate path/blob manifest entry: {relative}")
        entries[relative] = (git_blob_sha1, content_sha256)
    expected_paths = {
        path.relative_to(ROOT).as_posix()
        for path in files
        if path.relative_to(ROOT).as_posix() != PATH_BLOB_MANIFEST
    }
    if list(entries) != sorted(entries) or set(entries) != expected_paths:
        raise AssertionError("path/blob manifest inventory differs from exact tree")
    for relative, (expected_blob, expected_content) in entries.items():
        data = (ROOT / relative).read_bytes()
        blob_payload = b"blob " + str(len(data)).encode("ascii") + b"\x00" + data
        actual_blob = hashlib.sha1(blob_payload).hexdigest()
        actual_content = hashlib.sha256(data).hexdigest()
        if actual_blob != expected_blob or actual_content != expected_content:
            raise AssertionError(f"path/blob digest mismatch: {relative}")


def main() -> int:
    files = regular_files()
    actual = {path.relative_to(ROOT).as_posix() for path in files}
    if actual != EXPECTED_FILES:
        missing = sorted(EXPECTED_FILES - actual)
        extra = sorted(actual - EXPECTED_FILES)
        raise AssertionError(f"exact path manifest mismatch: missing={missing}, extra={extra}")
    decoded = text_files(files)
    for path, text in decoded:
        check_text(path, text)
        check_language_surface(path, text)
    check_workflows(decoded)
    check_dockerfiles(decoded)
    check_path_blob_manifest(files)
    print(f"gate-a containment: PASS ({len(files)} regular UTF-8 files)")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as exc:
        print(f"gate-a containment: FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
