"""Concrete bounded record/rollback reads for future ship callers.

Fresh resolution authenticates the full history and immutable sources; it does
not authorize a provider mutation or record publication. In particular a
post-deployment record read need not make the deployed candidate current main.
"""
import os
from pathlib import Path
import time

import record_resolver as resolver
import record_transport as transport

common, runtime = transport.common, transport.runtime


def _regular(path, *, directory=False):
    if (not isinstance(path, Path) or not path.is_absolute() or path.resolve() != path
            or any(part.is_symlink() or getattr(part, 'is_junction', lambda: False)()
                   for part in (path, *path.parents))
            or not all(part.is_dir() for part in path.parents)
            or not (path.is_dir() if directory else path.is_file())):
        raise transport.TransportRefusal('record-chain-invalid:reader-path')


def _read(repo_dir, *, gh_binary, git_binary, token, scratch_dir, deadline,
          selected_tag, mode, capture, clock, base_env):
    # Start before directory/client work. The existing budget caps this source
    # operation at 600 seconds, including construction and final inventory.
    budget = transport.OperationBudget(deadline, capture=capture, clock=clock)
    try:
        if mode not in ('chain', 'rollback') or (mode == 'rollback' and selected_tag is None):
            budget.fail('reader-mode')
        _regular(repo_dir, directory=True)
        _regular(gh_binary)
        _regular(git_binary)
        if not isinstance(scratch_dir, Path) or not scratch_dir.is_absolute() or scratch_dir.resolve() != scratch_dir:
            budget.fail('reader-scratch')
        _regular(scratch_dir.parent, directory=True)
        if (scratch_dir.exists() or scratch_dir.is_symlink()
                or getattr(scratch_dir, 'is_junction', lambda: False)()
                or scratch_dir.is_relative_to(repo_dir) or repo_dir.is_relative_to(scratch_dir)):
            budget.fail('reader-scratch')
        scratch_dir.mkdir(mode=0o700)
        config, work = scratch_dir / 'config', scratch_dir / 'work'
        config.mkdir(mode=0o700)
        work.mkdir(mode=0o700)
        budget.check()
        client = transport.ReleaseGh(gh_binary, token, config, work, budget=budget,
                                     base_env=common.scrubbed_env(base=base_env))
        if mode == 'rollback':
            return resolver.resolve_rollback(client, repo_dir, git_binary, selected_tag)
        return resolver.resolve_authenticated(client, repo_dir, git_binary, selected_tag)
    except (common.ReleaseError, resolver.policy.PolicyRefusal, resolver.schema_change.SchemaChangeBlocked):
        budget.failed = True
        raise
    except Exception:
        budget.fail('reader')
    finally:
        budget.close()


def read(repo_dir: Path, *, gh_binary: Path, git_binary: Path, token: str,
         scratch_dir: Path, deadline: float, selected_tag: str | None = None):
    """Read a fresh complete chain; selection alone grants no rollback policy."""
    return _read(repo_dir, gh_binary=gh_binary, git_binary=git_binary, token=token,
        scratch_dir=scratch_dir, deadline=deadline, selected_tag=selected_tag, mode='chain',
        capture=runtime.bounded_command, clock=time.monotonic, base_env=os.environ)


def rollback(repo_dir: Path, selected_tag: str, *, gh_binary: Path, git_binary: Path,
             token: str, scratch_dir: Path, deadline: float):
    """Fresh exact-target source policy, including the legacy-only transition."""
    return _read(repo_dir, gh_binary=gh_binary, git_binary=git_binary, token=token,
        scratch_dir=scratch_dir, deadline=deadline, selected_tag=selected_tag, mode='rollback',
        capture=runtime.bounded_command, clock=time.monotonic, base_env=os.environ)
