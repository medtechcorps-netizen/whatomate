"""Preparatory concrete normal preflight; no deployment or writer activation.

The public operation constructs its own release and Actions adapters. Returned
frozen projections are evidence data, not capabilities or a promise of future
freshness. A future ship caller must run this operation afresh at plan and
production time. No rollback, break-glass, provider or mutation path exists.
"""
from dataclasses import dataclass, field
from datetime import datetime, timezone
import math
import os
from pathlib import Path
import threading
import time

import compat_collect as collect
import compat_evidence as evidence
import compat_request as bridge
import record_resolver as resolver
import record_transport as transport
import forward_gates

common, runtime = transport.common, transport.runtime
MAX_SECONDS = 900
MAX_COMMANDS = 6000
MAX_BYTES = 128 * 1024 * 1024
CLEANUP_RESERVE = 2.25


def _need(value, detail):
    if not value:
        raise evidence.CompatRefusal("compat-stale", "preflight-" + detail)


def _finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def _regular(path, *, directory=False):
    _need(isinstance(path, Path) and path.is_absolute() and path.resolve() == path, "path")
    _need(not any(part.is_symlink() or getattr(part, "is_junction", lambda: False)()
                  for part in (path, *path.parents)), "path")
    _need(all(part.is_dir() for part in path.parents), "path")
    _need(path.is_dir() if directory else path.is_file(), "path")
    stat = path.stat()
    return stat.st_dev, stat.st_ino


@dataclass(frozen=True, slots=True)
class NormalPreflight:
    resolution: resolver.ForwardResolution = field(repr=False)
    qualified: evidence.QualifiedCompat
    collected: collect.CollectedHistory = field(repr=False)
    command_count: int
    captured_bytes: int
    drills: forward_gates.DrillGate | None = None


class _Budget:
    """An aggregate allowance, not a replacement for either transport grammar."""
    def __init__(self, deadline, *, capture, clock):
        now = clock()
        _need(_finite(now) and _finite(deadline) and now < deadline, "deadline")
        self.end, self._last = min(deadline, now + MAX_SECONDS), now
        self.clock, self._capture = clock, capture
        self.commands = self.captured_bytes = 0
        self.failed = self.closed = False
        self._paths = ()
        self._lock = threading.Lock()

    def fail(self, detail):
        self.failed = True
        raise evidence.CompatRefusal("compat-stale", "preflight-" + detail) from None

    def remaining(self):
        if self.failed or self.closed:
            self.fail("closed")
        now = self.clock()
        if not _finite(now) or not self._last <= now < self.end:
            self.fail("deadline")
        self._last = now
        return self.end - now

    def bind(self, root, gh, git):
        self._paths = tuple((path, directory, _regular(path, directory=directory))
                            for path, directory in ((root, True), (gh, False), (git, False)))

    def check(self):
        self.remaining()
        for path, directory, identity in self._paths:
            _need(_regular(path, directory=directory) == identity, "path-changed")

    def close(self):
        if self.closed:
            return
        try:
            if not self.failed:
                self.check()
        finally:
            self.closed = True

    def capture(self, argv, *, env, timeout, stdout_limit, stderr_limit):
        self.check()
        _need(type(argv) is list and argv and type(argv[0]) is str
              and argv[0] in {str(row[0]) for row in self._paths[1:]}, "command")
        _need(_finite(timeout) and timeout > 0
              and all(type(limit) is int and 0 <= limit <= 64 * 1024 * 1024
                      for limit in (stdout_limit, stderr_limit)), "capture-input")
        if not self._lock.acquire(blocking=False):
            self.fail("concurrent")
        try:
            allowance = MAX_BYTES - self.captured_bytes
            if self.commands >= MAX_COMMANDS or allowance <= stderr_limit:
                self.fail("bound")
            duration = min(timeout, self.remaining() - CLEANUP_RESERVE)
            if duration <= 0:
                self.fail("deadline")
            limit = min(stdout_limit, allowance - stderr_limit)
            self.commands += 1
            try:
                result = self._capture(argv, env=env, timeout=duration,
                                       stdout_limit=limit, stderr_limit=stderr_limit)
            except Exception:
                self.fail("capture")
            if (type(result) is not runtime.Capture or type(result.returncode) is not int
                    or type(result.stdout) is not bytes or type(result.stderr) is not bytes
                    or len(result.stdout) > limit or len(result.stderr) > stderr_limit):
                self.fail("capture")
            self.captured_bytes += len(result.stdout) + len(result.stderr)
            self.check()
            return result
        finally:
            self._lock.release()


def _scratch(path, repo):
    _need(isinstance(path, Path) and path.is_absolute() and path.resolve() == path, "scratch")
    _regular(path.parent, directory=True)
    _need(not path.exists() and not path.is_symlink()
          and not getattr(path, "is_junction", lambda: False)()
          and not path.is_relative_to(repo) and not repo.is_relative_to(path), "scratch")
    path.mkdir(mode=0o700)
    _regular(path, directory=True)
    for phase in ("source-before", "actions", "source-after"):
        parent = path / phase
        parent.mkdir(mode=0o700)
        for child in ("config", "work"):
            (parent / child).mkdir(mode=0o700)


def _source(budget, repo, sha, gh, git, token, phase, env):
    budget.check()
    operation = transport.OperationBudget(budget.end, capture=budget.capture, clock=budget.clock)
    try:
        client = transport.ReleaseGh(gh, token, phase / "config", phase / "work",
                                     budget=operation, base_env=env)
        return resolver.resolve_forward(client, repo, git, sha)
    except Exception:
        # close must not mask the primary source refusal on construction errors.
        operation.failed = True
        raise
    finally:
        operation.close()


def _preflight(repo_dir, candidate_sha, *, gh_binary, git_binary, token, scratch_dir,
               deadline, capture, clock, wall_clock, base_env):
    """Private test seams control commands/time, never supplied evidence facts."""
    budget = _Budget(deadline, capture=capture, clock=clock)
    try:
        _need(type(candidate_sha) is str and common.SHA1_RE.fullmatch(candidate_sha), "source")
        _need(type(token) is str and 0 < len(token) <= 8192
              and not any(char in token for char in "\r\n\0"), "token")
        _regular(repo_dir, directory=True)
        _regular(gh_binary)
        _regular(git_binary)
        _scratch(scratch_dir, repo_dir)
        budget.bind(scratch_dir, gh_binary, git_binary)
        env = common.scrubbed_env(base=base_env)
        before = _source(budget, repo_dir, candidate_sha, gh_binary, git_binary, token,
                         scratch_dir / "source-before", env)
        budget.check()
        _need(before.resolved.inventory.main_sha == candidate_sha, "main-changed")
        phase = scratch_dir / "actions"
        client = collect.ActionsGh(str(gh_binary), token, phase / "config",
            seconds=min(600, budget.remaining()), base_env=env, capture=budget.capture, clock=budget.clock)
        history = collect.collect_history(collect.CollectionRequest(candidate_sha, before.workflow),
                                           client, phase / "work", wall_clock=wall_clock)
        budget.check()
        after = _source(budget, repo_dir, candidate_sha, gh_binary, git_binary, token,
                        scratch_dir / "source-after", env)
        budget.check()
        _need(after == before, "source-changed")
        qualified = bridge.qualify(after, history)
        budget.check()
        observed_at = wall_clock()
        _need(type(observed_at) is datetime and observed_at.tzinfo is timezone.utc
              and observed_at.fold == 0 and observed_at >= history.history.observed_at, "drill-clock")
        drills = forward_gates.observe_forward_drills(after, now=observed_at)
        budget.check()
        return NormalPreflight(after, qualified, history, budget.commands, budget.captured_bytes, drills)
    except forward_gates.DrillRefusal as exc:
        budget.failed = True
        common.fail("schema-change-blocked:" + str(exc))
    except (evidence.CompatRefusal, common.ReleaseError):
        budget.failed = True
        raise
    except Exception:
        budget.fail("invalid")
    finally:
        budget.close()


def preflight(repo_dir: Path, candidate_sha: str, *, gh_binary: Path, git_binary: Path,
              token: str, scratch_dir: Path, deadline: float) -> NormalPreflight:
    """Resolve, collect, re-resolve and qualify under one caller-owned deadline.

    deadline is an absolute time.monotonic value created by the caller before
    invoking this function. The operation caps itself at 900 seconds and never
    fetches Git history. scratch_dir must be a fresh private location outside
    the repository; failed/successful evidence is retained, never replayed.
    No caller-supplied clients, source facts or proof receipts are accepted.
    """
    return _preflight(repo_dir, candidate_sha, gh_binary=gh_binary, git_binary=git_binary,
        token=token, scratch_dir=scratch_dir, deadline=deadline,
        capture=runtime.bounded_command, clock=time.monotonic,
        wall_clock=lambda: datetime.now(timezone.utc), base_env=os.environ)
