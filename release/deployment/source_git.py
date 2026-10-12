"""Bounded immutable Git reads for preparatory source policy, not release authority.

No command runs at import. A single-use operation redirects only the existing
release_record.git_output seam. The caller supplies its trusted Git executable
and repository; neither signatures nor current-main/GitHub freshness are proved
here. Native Windows capture bounds/reaps the direct child, not arbitrary process
trees. The fixed Git grammar disables helper execution and implicit fetching.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
import math
import os
from pathlib import Path
import re
import sys
import threading
import time

import release_record as records
import record_policy
import ship_common as common

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "compat"))
import runtime

MIB = 1024 * 1024
# runtime capture reaps the direct child for <=2 seconds and joins two readers
# for <=0.1 second each. Reserve that tail within the operation's time budget.
CLEANUP_RESERVE = 2.25
_OID = re.compile(r"[0-9a-f]{40}\Z")
_PATH = re.compile(r"[A-Za-z_.][A-Za-z0-9_./-]{0,511}\Z")
_PARTIAL = r"^(extensions\.partialclone|remote\..*\.promisor)$"
PUBLISHER_NAMESPACE_LIMIT = 65536
PUBLISHER_NAMESPACE_RECORDS = 1024
_PUBLISHER_ROOTS = ("release/compat", "release/deployment")
_PUBLISHER_ATTRIBUTES = (".gitattributes", "release/.gitattributes")
_PUBLISHER_PATHS = (*_PUBLISHER_ATTRIBUTES, *_PUBLISHER_ROOTS)
_PUBLISHER_CONSUMER = "release/deployment/compat-negative-consumer-v1.json"
_PUBLISHER_TREE_ARGS = ("ls-tree", "-r", "-t", "-z", "--full-tree")


class SourceGitRefusal(common.ReleaseError):
    """Only fixed module-owned codes; no command, path or stream disclosure."""


def _need(ok, detail="input"):
    if not ok:
        raise SourceGitRefusal("git-failed:source-" + detail)


def _oid(value):
    return type(value) is str and _OID.fullmatch(value) is not None


def _path(value, *, directory=False):
    if type(value) is not str or _PATH.fullmatch(value) is None:
        return False
    parts = value.removesuffix("/").split("/") if directory else value.split("/")
    return all(part.casefold() not in {"", ".", "..", ".git"} for part in parts)


def publisher_namespace(raw):
    """Replay a complete fixed Git listing; None means ineligible topology.

    Raw capture is provenance, not authentication by a parser. Blob OIDs are
    retained for consistency with the separately pinned executable bytes.
    Well-framed unsupported topology never becomes a normal-proof veto.
    """
    _need(type(raw) is bytes and len(raw) <= PUBLISHER_NAMESPACE_LIMIT, "namespace-bound")
    if not raw:
        return None
    _need(raw.endswith(b"\0"), "namespace-frame")
    records = raw[:-1].split(b"\0")
    _need(len(records) <= PUBLISHER_NAMESPACE_RECORDS and all(records), "namespace-bound")
    rows, seen, folded = [], {}, set()
    eligible = True
    for record in records:
        meta, sep, name = record.partition(b"\t")
        _need(sep and 0 < len(name) <= 512, "namespace-frame")
        try:
            mode, kind, oid = meta.decode("ascii").split(" ")
            path = name.decode("ascii")
        except (UnicodeError, ValueError):
            raise SourceGitRefusal("git-failed:source-namespace-frame") from None
        _need((mode, kind) in (("040000", "tree"), ("100644", "blob"),
              ("100755", "blob"), ("120000", "blob"), ("160000", "commit"))
              and _oid(oid), "namespace-frame")
        parts = path.split("/")
        _need(len(parts) <= 16 and all(parts)
              and all(32 <= ord(char) <= 126 and char != "\\" for char in path)
              and all(part.casefold() not in (".", "..", ".git") for part in parts)
              and not path.startswith("/") and path not in seen
              and path.casefold() not in folded, "namespace-path")
        _need(path == "release" or any(path == root or path.startswith(root + "/")
              for root in _PUBLISHER_PATHS), "namespace-scope")
        parent = path.rpartition("/")[0]
        _need(not parent or parent in seen and seen[parent][1:3] == ("040000", "tree"),
              "namespace-parent")
        row = (path, mode, kind, oid)
        rows.append(row); seen[path] = row; folded.add(path.casefold())
        lower = tuple(part.casefold() for part in parts)
        if (mode in ("120000", "160000") or ".gitattributes" in lower
                or "__pycache__" in lower
                or any(part.endswith((".pyc", ".pyo", ".so", ".pyd")) for part in lower)):
            eligible = False
        if path == _PUBLISHER_CONSUMER and (kind != "blob" or mode not in ("100644", "100755")):
            eligible = False
    # Git compares tree names as if suffixed with '/', and emits each tree
    # before its descendants. Plain lexical sorting of paths is not this order.
    ordered = sorted(rows, key=lambda row: row[0] + ("/" if row[2] == "tree" else ""))
    _need(rows == ordered, "namespace-order")
    if any(root not in seen or seen[root][1:3] != ("040000", "tree")
           for root in ("release", *_PUBLISHER_ROOTS)):
        eligible = False
    if not eligible:
        return None
    return tuple(row for row in rows if row[0] != _PUBLISHER_CONSUMER)


@dataclass(frozen=True, slots=True)
class Limits:
    seconds: float = 600
    calls: int = 4096
    total_bytes: int = 64 * MIB
    command_seconds: float = 15


class ImmutableGit:
    """One budget, one scope, fixed reads; failures poison the operation."""

    def __init__(self, repo: Path, binary: Path, *, limits=Limits(),
                 capture=runtime.bounded_command, clock=time.monotonic, base_env=None):
        _need(type(limits) is Limits and type(limits.calls) is int and 1 <= limits.calls <= 4096
              and type(limits.total_bytes) is int and 4096 < limits.total_bytes <= 64 * MIB)
        _need(all(type(v) in (int, float) and math.isfinite(v) and 0 < v <= bound
                  for v, bound in ((limits.seconds, 600), (limits.command_seconds, 15))))
        _need(isinstance(repo, Path) and repo.is_absolute() and isinstance(binary, Path) and binary.is_absolute())
        self.clock, self.capture, self.limits = clock, capture, limits
        self.deadline = clock() + limits.seconds
        try:
            self.repo = repo.resolve(strict=True)
            self.binary = binary.resolve(strict=True)
            _need(self.repo.is_dir() and self.binary.is_file())
        except (OSError, ValueError):
            raise SourceGitRefusal("git-failed:source-input") from None
        self.env = common.scrubbed_env({
            "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C", "GIT_OPTIONAL_LOCKS": "0",
            "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_COUNT": "0",
            "GIT_ATTR_NOSYSTEM": "1", "GIT_NO_REPLACE_OBJECTS": "1",
            "GIT_NO_LAZY_FETCH": "1", "GIT_GRAFT_FILE": os.devnull,
        }, base=base_env)
        # Absolute executable and builtin-only commands need no helper PATH.
        self.env["PATH"] = str(self.binary.parent)
        self.calls = self.bytes = 0
        self.failed = self.active = self.closed = False
        self._lock = threading.Lock()
        self._metadata = ()

    def _fail(self, detail):
        self.failed = True
        raise SourceGitRefusal("git-failed:source-" + detail) from None

    def _check(self):
        if self.failed or self.closed:
            self._fail("closed")
        if self.clock() >= self.deadline:
            self._fail("deadline")
        try:
            for directory in self._metadata:
                # Check existence only; never read local graft/config content.
                if (directory / "info/grafts").exists() or (directory / "shallow").exists():
                    self._fail("repository")
        except OSError:
            self._fail("repository")

    def _call(self, args, *, allowed=(0,), limit=4 * MIB):
        self._check()
        if not self._lock.acquire(blocking=False):
            self._fail("concurrent")
        try:
            remaining = self.limits.total_bytes - self.bytes
            if self.calls >= self.limits.calls or remaining <= 4096:
                self._fail("budget")
            self.calls += 1
            timeout = min(self.limits.command_seconds, self.deadline - self.clock() - CLEANUP_RESERVE)
            if timeout <= 0:
                self._fail("deadline")
            stdout_limit = min(limit, remaining - 4096)
            argv = [str(self.binary), "--no-pager", "--no-replace-objects", "-C", str(self.repo),
                    "-c", "core.hooksPath=" + os.devnull, "-c", "core.fsmonitor=false",
                    "-c", "core.quotepath=off", "-c", "protocol.allow=never",
                    # GIT_GRAFT_FILE points to the empty OS null device. Git
                    # otherwise emits a deprecation hint even for that device.
                    "-c", "advice.graftFileDeprecated=false", *args]
            try:
                result = self.capture(argv, env=dict(self.env), timeout=timeout,
                                      stdout_limit=stdout_limit, stderr_limit=4096)
            except Exception:
                self._fail("capture")
            if (type(result) is not runtime.Capture or type(result.returncode) is not int or type(result.stdout) is not bytes
                    or type(result.stderr) is not bytes or len(result.stdout) > stdout_limit
                    or len(result.stderr) > 4096):
                self._fail("capture")
            self.bytes += len(result.stdout) + len(result.stderr)
            self._check()
            if result.returncode not in allowed or result.stderr:
                self._fail("command")
            return result
        finally:
            self._lock.release()

    def _preflight(self):
        raw_root = self._call(["rev-parse", "--show-toplevel"], limit=8192).stdout
        try:
            root = raw_root.decode("utf-8", "strict").rstrip("\n")
            if "\r" in root or "\n" in root or Path(root).resolve(strict=True) != self.repo:
                self._fail("repository")
        except (OSError, UnicodeError, ValueError):
            self._fail("repository")
        if self._call(["rev-parse", "--show-object-format"], limit=128).stdout != b"sha1\n":
            self._fail("repository")
        if self._call(["rev-parse", "--is-shallow-repository"], limit=128).stdout != b"false\n":
            self._fail("repository")
        if self._call(["for-each-ref", "--format=%(refname)", "refs/replace/"], limit=4096).stdout:
            self._fail("repository")
        partial = self._call(["config", "--get-regexp", _PARTIAL], allowed=(0, 1), limit=4096)
        if partial.returncode != 1 or partial.stdout:
            self._fail("repository")
        directories = []
        for flag in ("--absolute-git-dir", "--git-common-dir"):
            raw = self._call(["rev-parse", flag], limit=8192).stdout
            try:
                text = raw.decode("utf-8", "strict").rstrip("\n")
                _need(text and "\n" not in text and "\r" not in text)
                value = Path(text)
                value = (self.repo / value).resolve(strict=True) if not value.is_absolute() else value.resolve(strict=True)
                _need(value.is_dir())
                directories.append(value)
            except (OSError, UnicodeError, ValueError, common.ReleaseError):
                self._fail("repository")
        if self._metadata and tuple(directories) != self._metadata:
            self._fail("repository")
        self._metadata = tuple(directories)
        self._check()

    @contextmanager
    def operation(self):
        if self.active or self.closed or self.failed:
            self._fail("closed")
        try:
            with records.scoped_git_output(self.output):
                self._preflight()
                self.active = True
                yield self
                self._preflight()
        finally:
            self.active = False
            self.closed = True

    def output(self, repo_dir, args, *, code):
        """The exact classifier grammar; no arbitrary Git command passthrough."""
        del code  # Caller diagnostics cannot inject content into our fixed codes.
        self._check()
        try:
            same_repo = isinstance(repo_dir, Path) and repo_dir.resolve(strict=True) == self.repo
        except (OSError, ValueError):
            same_repo = False
        if not self.active or not same_repo:
            self._fail("scope")
        if (type(args) not in (list, tuple) or not 1 <= len(args) <= 16
                or not all(type(v) is str and len(v) <= 1024 for v in args)):
            self._fail("grammar")
        values = list(args)
        if values[:2] == ["-c", "core.quotepath=off"]:
            values = values[2:]
        if len(values) == 3 and values[:2] in (["cat-file", "-t"], ["cat-file", "-s"], ["cat-file", "blob"]) and _oid(values[2]):
            return self._call(values, limit=4 * MIB if values[1] == "blob" else 128).stdout
        if (len(values) == 5 and values[:2] == ["ls-tree", "-z"]
                and _oid(values[2]) and values[3] == "--" and _path(values[4])):
            return self._call(values).stdout
        if (7 <= len(values) <= 14 and values[:4] == ["ls-tree", "-r", "--name-only", "-z"]
                and _oid(values[4]) and values[5] == "--" and all(_path(p, directory=True) for p in values[6:])):
            return self._call(values).stdout
        if (len(values) == 11 and tuple(values[:5]) == _PUBLISHER_TREE_ARGS
                and _oid(values[5]) and values[6] == "--"
                and tuple(values[7:]) == _PUBLISHER_PATHS):
            return self._call(values, limit=PUBLISHER_NAMESPACE_LIMIT).stdout
        if len(values) == 3 and values[0] == "merge-base" and all(_oid(v) for v in values[1:]):
            return self._call(values, limit=128).stdout
        if (len(values) == 4 and values[:3] == ["rev-list", "--first-parent", "--reverse"]
                and re.fullmatch(r"[0-9a-f]{40}\.\.[0-9a-f]{40}", values[3])):
            return self._call(values, limit=2048 * 41).stdout
        if len(values) == 2 and values[0] == "show":
            sha, colon, path = values[1].partition(":")
            if colon and _oid(sha) and _path(path):
                value = self.read_blob(sha, path)
                if value is None:
                    self._fail("blob")
                return value
        named = (len(values) == 7 and values[:4] == ["diff", "--no-renames", "--name-status", "-z"]
                 and all(_oid(v) for v in values[4:6]) and values[6] == "--")
        patch = (len(values) == 7 and values[:3] == ["diff", "--no-renames", "-U0"]
                 and all(_oid(v) for v in values[3:5]) and values[5] == "--" and _path(values[6]))
        if named or patch:
            return self._call(["diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
                               "--text", "--ignore-submodules=none", "--submodule=short", *values[1:]]).stdout
        self._fail("grammar")

    def read_blob(self, sha: str, path: str) -> bytes | None:
        if not _oid(sha) or not _path(path):
            self._fail("grammar")
        raw = self.output(self.repo, ["ls-tree", "-z", sha, "--", path], code="unused")
        if not raw:
            return None
        rows = raw.split(b"\0")
        try:
            meta, sep, name = rows[0].partition(b"\t")
            mode, kind, oid = meta.split(b" ")
            if (len(rows) != 2 or rows[1] or not sep or name != path.encode()
                    or mode not in {b"100644", b"100755"} or kind != b"blob" or not _oid(oid.decode("ascii"))):
                self._fail("blob")
            size = self.output(self.repo, ["cat-file", "-s", oid.decode()], code="unused").strip()
            if not size.isdigit() or int(size) > record_policy.MAX_SOURCE_FILE_BYTES:
                self._fail("blob")
            value = self.output(self.repo, ["cat-file", "blob", oid.decode()], code="unused")
            if len(value) != int(size):
                self._fail("blob")
            return value
        except (ValueError, UnicodeError):
            self._fail("blob")

    def reviewed_v0(self, sha: str) -> record_policy.SourceFacts:
        if not _oid(sha):
            self._fail("grammar")
        if self.output(self.repo, ["cat-file", "-t", sha], code="unused") != b"commit\n":
            self._fail("blob")
        blobs, total = [], 0
        for path in record_policy.SOURCE_PATHS:
            value = self.read_blob(sha, path)
            if value is None:
                self._fail("blob")
            total += len(value)
            if total > record_policy.MAX_SOURCE_BYTES:
                self._fail("budget")
            blobs.append(record_policy.SourceBlob(path, value))
        try:
            return record_policy.reviewed_v0_source(sha, tuple(blobs))
        except record_policy.PolicyRefusal:
            self._fail("v0")
