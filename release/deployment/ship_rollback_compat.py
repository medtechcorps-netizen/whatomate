"""Concrete rollback admission, without transferable operational authority.

Three fresh closed phases share the original outer capture allowance. Complete
source and all-head evidence are retained. The separate named admission route
keeps legacy and zero/one-change transitions free of Actions evidence reads.
"""
from dataclasses import dataclass, field
from datetime import datetime, timezone
import os
from pathlib import Path
import time

import compat_evidence as evidence
import compat_envelope as envelope
import compat_request as bridge
import compat_rollback_collect as collect
import compat_rollback_history as history
import record_resolver as resolver
import record_transport as transport
import ship_compat as normal

common, runtime = transport.common, transport.runtime
ROLLBACK_MAX_SECONDS = 900


def _need(value, detail):
    if not value:
        raise evidence.CompatRefusal('compat-stale', 'rollback-preflight-' + detail)


def _utc(value):
    _need(type(value) is datetime and value.tzinfo is timezone.utc and value.fold == 0,
          'clock')
    return value


@dataclass(frozen=True, slots=True)
class RollbackPreflight:
    """Observed data only; checked_at/expiry are not a permission lifetime."""
    resolution: resolver.RollbackProofResolution = field(repr=False)
    qualified: history.QualifiedRollback
    collected: history.CollectedRollbackHistory = field(repr=False)
    command_count: int
    captured_bytes: int
    checked_at: datetime
    artifact_expires_at: datetime
    entry_at: datetime


def _source(budget, repo, producer, target, gh, git, token, phase, env, *, admission=False):
    budget.check()
    operation = transport.OperationBudget(budget.end, capture=budget.capture, clock=budget.clock)
    try:
        client = transport.ReleaseGh(gh, token, phase / 'config', phase / 'work',
                                     budget=operation, base_env=env)
        if admission:
            return resolver.resolve_rollback_admission(client, repo, git, producer, target)
        return resolver.resolve_rollback_proof(client, repo, git, producer, target)
    except Exception:
        operation.failed = True
        raise
    finally:
        operation.close()


def _actions(budget, repo, producer, gh, git, token, phase, env, wall_clock):
    budget.check()
    operation = transport.OperationBudget(budget.end, capture=budget.capture, clock=budget.clock)
    try:
        return collect._collect(producer, repo, gh_binary=gh, git_binary=git, token=token,
            config_dir=phase / 'config', scratch=phase / 'work', budget=operation,
            wall_clock=wall_clock, base_env=env)
    except Exception:
        operation.failed = True
        raise
    finally:
        operation.close()


def _expiry(qualified, collected, checked_at):
    """Check retained selected metadata without inventing a later observation."""
    source = next(row for row in collected.history.workflows
                  if row.sha == qualified.binding.producer_sha)
    matches = []
    for row in collected.history.attempts:
        run = common.loads_strict(row.envelope.raw)['run']
        if (run['id'], run['run_attempt']) == (qualified.run_id, qualified.run_attempt):
            matches.append(row)
    _need(len(matches) == 1, 'selected-attempt')
    replayed = envelope.replay(source, matches[0].envelope,
                              observed_at=collected.history.observed_at)
    artifacts = [item for item in replayed.artifacts
                 if (item.id, item.name) == (qualified.artifact_id, qualified.artifact_name)]
    _need(len(artifacts) == 1 and artifacts[0].expired is False
          and checked_at < artifacts[0].expires, 'artifact-expired')
    return artifacts[0].expires


def _preflight(repo_dir, producer_sha, target_tag, *, gh_binary, git_binary, token,
               scratch_dir, deadline, capture, clock, wall_clock, base_env, admission=False):
    """Only command/time seams are injectable here, never favorable facts."""
    budget = normal._Budget(deadline, capture=capture, clock=clock)
    # Preserve the rollback window using the original constructor clock sample.
    budget.end = min(budget.end, budget._last + ROLLBACK_MAX_SECONDS)
    try:
        entry_utc = _utc(wall_clock())
        _need(type(producer_sha) is str and common.SHA1_RE.fullmatch(producer_sha), 'source')
        _need(type(target_tag) is str and (target_tag == common.BOOTSTRAP_TAG
              or common.RECORD_TAG_RE.fullmatch(target_tag)), 'target')
        _need(type(token) is str and 0 < len(token) <= 8192
              and not any(char in token for char in '\r\n\0'), 'token')
        normal._regular(repo_dir, directory=True)
        normal._regular(gh_binary)
        normal._regular(git_binary)
        normal._scratch(scratch_dir, repo_dir)
        budget.bind(scratch_dir, gh_binary, git_binary)
        env = common.scrubbed_env(base=base_env)
        before = _source(budget, repo_dir, producer_sha, target_tag, gh_binary, git_binary,
                         token, scratch_dir / 'source-before', env, admission=admission)
        budget.check()
        if admission and type(before) is resolver.RollbackResolution:
            # Its source operation already performed complete final checks.
            # Do not fetch any Actions evidence for legacy or zero/one spans.
            return before
        collected = _actions(budget, repo_dir, producer_sha, gh_binary, git_binary,
                             token, scratch_dir / 'actions', env, wall_clock)
        budget.check()
        after = _source(budget, repo_dir, producer_sha, target_tag, gh_binary, git_binary,
                        token, scratch_dir / 'source-after', env, admission=admission)
        budget.check()
        _need(type(before) is resolver.RollbackProofResolution and before == after,
              'source-changed')
        qualified = bridge.qualify_rollback(after, collected)
        budget.check()
        checked_at = _utc(wall_clock())
        _need(entry_utc <= collected.history.observed_at <= checked_at, 'clock')
        expires = _expiry(qualified, collected, checked_at)
        budget.check()
        return RollbackPreflight(after, qualified, collected, budget.commands,
                                 budget.captured_bytes, checked_at, expires, entry_utc)
    except (evidence.CompatRefusal, common.ReleaseError, resolver.policy.PolicyRefusal,
            resolver.schema_change.SchemaChangeBlocked):
        budget.failed = True
        raise
    except Exception:
        budget.fail('invalid')
    finally:
        budget.close()


def preflight(repo_dir: Path, producer_sha: str, target_tag: str, *, gh_binary: Path,
              git_binary: Path, token: str, scratch_dir: Path,
              deadline: float) -> RollbackPreflight:
    """Prepare exact target/live proof from fresh concrete source and history.

    deadline is the caller's original absolute monotonic deadline. The shared
    outer maximum remains 900 seconds, 6000 commands and 128MiB across all three
    phases. Each phase closes independently and never renews those allowances.
    Scratch is create-only and outside the repository. Artifacts are retained
    facts, not deleted-history coverage or a promise of future source freshness.
    This function is not called by operational rollback, restore or publication.
    """
    return _preflight(repo_dir, producer_sha, target_tag, gh_binary=gh_binary,
        git_binary=git_binary, token=token, scratch_dir=scratch_dir, deadline=deadline,
        capture=runtime.bounded_command, clock=time.monotonic,
        wall_clock=lambda: datetime.now(timezone.utc), base_env=os.environ)


def admit(repo_dir: Path, producer_sha: str, target_tag: str, *, gh_binary: Path,
          git_binary: Path, token: str, scratch_dir: Path,
          deadline: float) -> resolver.RollbackResolution | RollbackPreflight:
    """Fresh exact-target source policy, fetching compatibility only if needed.

    The protected caller repeats this operation before provider effects. The
    public no-proof rollback policy is unchanged; a multiple-change result
    must complete the dedicated source/Actions/source path without new caps.
    """
    return _preflight(repo_dir, producer_sha, target_tag, gh_binary=gh_binary,
        git_binary=git_binary, token=token, scratch_dir=scratch_dir, deadline=deadline,
        capture=runtime.bounded_command, clock=time.monotonic,
        wall_clock=lambda: datetime.now(timezone.utc), base_env=os.environ, admission=True)


def validate_preserved(value: RollbackPreflight) -> RollbackPreflight:
    """Pure original-time replay for closed custody, never a fresh permission.

    The codec/writer must bind the original protected workflow execution and
    exact parent independently. No caller-provided verdict, retry clock or
    deserialized type establishes source or signature provenance.
    """
    try:
        _need(type(value) is RollbackPreflight and type(value.qualified) is history.QualifiedRollback,
              'preserved-type')
        _need(type(value.command_count) is int and 0 < value.command_count <= normal.MAX_COMMANDS
              and type(value.captured_bytes) is int and 0 < value.captured_bytes <= normal.MAX_BYTES,
              'preserved-bounds')
        qualified = bridge.qualify_rollback(value.resolution, value.collected)
        _need(value.qualified == qualified, 'preserved-qualification')
        _need(_utc(value.entry_at) <= value.collected.history.observed_at <= _utc(value.checked_at)
              and value.command_count >= value.collected.command_count
              and value.captured_bytes >= value.collected.captured_bytes, 'preserved-clock')
        expires = _expiry(qualified, value.collected, value.checked_at)
        _need(_utc(value.artifact_expires_at) == expires, 'preserved-expiry')
        return value
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, resolver.policy.PolicyRefusal, ValueError, TypeError,
            AttributeError, KeyError, IndexError, StopIteration, RecursionError, OverflowError):
        raise evidence.CompatRefusal('compat-stale', 'rollback-preflight-preserved-invalid') from None
