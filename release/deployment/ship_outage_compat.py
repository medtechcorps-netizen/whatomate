"""Concrete, currently unconfigured outage admission under one original budget.

Captured raw facts and finite timestamps are not transferable permissions.
The caller must roundtrip custody and check its original deadline before I/O.
"""
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import hashlib
import os
from pathlib import Path
import time

import compat_collect as collect
import compat_evidence as evidence
import compat_negative_index as negative
import compat_negative_transport as index_transport
import compat_outage_policy as loader
import compat_request as bridge
import forward_gates
import record_resolver as resolver
import record_transport as transport
import ship_compat as normal

common, runtime, policy = transport.common, transport.runtime, resolver.policy
OUTAGE_FRESHNESS_SECONDS = 900


def _need(value, detail):
    if not value:
        raise evidence.CompatRefusal("compat-stale", "outage-preflight-" + detail)


def _utc(value):
    _need(type(value) is datetime and value.tzinfo is timezone.utc and value.fold == 0, "clock")
    return value


@dataclass(frozen=True, slots=True)
class ServiceUnavailable:
    observation: negative.HttpObservation = field(repr=False)
    observed_at: datetime


@dataclass(frozen=True, slots=True)
class MissingExpiredEvidence:
    observation: negative.HttpObservation = field(repr=False)
    observed_at: datetime
    collected: collect.CollectedOutageHistory = field(repr=False)


@dataclass(frozen=True, slots=True)
class OutagePreflight:
    source: resolver.OutageForwardResolution = field(repr=False)
    availability: ServiceUnavailable | MissingExpiredEvidence = field(repr=False)
    index: negative.IndexSnapshot = field(repr=False)
    entry_at: datetime
    checked_at: datetime
    remaining_us: int
    valid_until: datetime
    command_count: int
    captured_bytes: int


def _coverage(source):
    _need(type(source) is resolver.OutageForwardResolution
          and type(source.admission) is resolver.ForwardAdmission
          and type(source.coverage) is resolver.SourceCoverage, "source")
    resolution, coverage = source.admission.resolution, source.coverage
    parsed = loader.parse_consumer_policy(coverage.policy_raw)
    _need(type(coverage.policy_blob) is resolver.BlobObservation
          and coverage.policy_blob.path == loader.DATA_PATH
          and type(coverage.policy_blob.raw) is bytes
          and coverage.policy_blob.raw == parsed.raw, "policy-source")
    sha = resolution.candidate.sha
    _need(type(coverage.ancestry) is tuple and len(coverage.ancestry) == len(parsed.lineage)
          and type(coverage.blobs) is tuple
          and len(coverage.blobs) == 2 * sum(len(row.inventory) for row in parsed.lineage)
          and type(coverage.namespaces) is tuple
          and len(coverage.namespaces) == 2 * len(parsed.lineage), "inventory")
    _need(all(type(row) is resolver.PublisherNamespace and type(row.raw) is bytes
              and len(row.raw) <= resolver.source_git.PUBLISHER_NAMESPACE_LIMIT
              for row in coverage.namespaces)
          and all(type(row) is resolver.PublisherBlob and (row.raw is None or
              type(row.raw) is bytes and len(row.raw) <= negative.MAX_SOURCE) for row in coverage.blobs),
          "source-bound")
    _need(sum(len(row.raw) for row in coverage.namespaces)
          + sum(len(row.raw) if row.raw is not None else 0 for row in coverage.blobs)
          <= negative.MAX_SOURCE_TOTAL, "source-bound")
    total, position, namespace_position, covered = 0, 0, 0, False
    for lineage, ancestry in zip(parsed.lineage, coverage.ancestry, strict=True):
        _need(type(ancestry) is resolver.PublisherAncestry
              and ancestry.producer_sha == lineage.producer_sha and ancestry.head_sha == sha
              and type(ancestry.merge_base_sha) is str
              and common.SHA1_RE.fullmatch(ancestry.merge_base_sha), "ancestry")
        matches = ancestry.merge_base_sha == lineage.producer_sha
        namespaces = []
        for source_sha in (lineage.producer_sha, sha):
            namespace = coverage.namespaces[namespace_position]; namespace_position += 1
            _need(type(namespace) is resolver.PublisherNamespace and namespace.source_sha == source_sha
                  and type(namespace.raw) is bytes
                  and len(namespace.raw) <= resolver.source_git.PUBLISHER_NAMESPACE_LIMIT, "namespace-role")
            total += len(namespace.raw)
            _need(total <= negative.MAX_SOURCE_TOTAL, "source-bound")
            namespaces.append(resolver.source_git.publisher_namespace(namespace.raw))
        if any(row is None for row in namespaces):
            matches = False
        else:
            matches &= (tuple(row[:3] for row in namespaces[0]) == tuple(row[:3] for row in namespaces[1]))
        namespace_maps = tuple({row[0]: row for row in rows} if rows is not None else {}
                               for rows in namespaces)
        for row in lineage.inventory:
            for role, source_sha in enumerate((lineage.producer_sha, sha)):
                blob = coverage.blobs[position]; position += 1
                _need(type(blob) is resolver.PublisherBlob and blob.source_sha == source_sha
                      and blob.path == row.path, "blob-role")
                if blob.raw is None:
                    _need(blob.mode is None, "blob-absence")
                    matches = False
                else:
                    _need(type(blob.raw) is bytes and len(blob.raw) <= negative.MAX_SOURCE
                          and type(blob.mode) is str and blob.mode in ("100644", "100755"), "blob")
                    total += len(blob.raw)
                    _need(total <= negative.MAX_SOURCE_TOTAL, "source-bound")
                    matches &= (blob.mode == row.mode and len(blob.raw) == row.byte_count
                                and common.sha256_bytes(blob.raw) == row.sha256)
                    if any(row.path.startswith(root + "/") for root in resolver.source_git._PUBLISHER_ROOTS):
                        oid = hashlib.sha1(b"blob " + str(len(blob.raw)).encode("ascii") + b"\0" + blob.raw).hexdigest()
                        matches &= namespace_maps[role].get(row.path) == (row.path, blob.mode, "blob", oid)
        # Deterministic sorted policy order; no caller-selected producer.
        if matches and not covered:
            covered = True
    _need(covered, "uncovered")
    return parsed


def _eligible(source, checked_at):
    resolution = source.admission.resolution
    latest, candidate, change = resolution.resolved.latest, resolution.candidate, resolution.classification
    live = latest.live
    _need(change.kind == "none" and change.dormant is False and not change.reasons
          and not change.migration_changes and live is not None
          and source.admission.contract_observation is None, "class")
    _need(latest.record_sha == live.origin_source.sha == resolution.database_base_source
          and candidate.mode == latest.reader.mode == live.origin_source.mode == "reviewed-v0"
          and candidate.artifacts == latest.reader.artifacts == live.origin_source.artifacts
          and tuple(path for path, _ in candidate.artifacts) == tuple(sorted(policy.SOURCE_PATHS))
          and candidate.catalog == live.catalog == latest.reader.catalog == live.origin_source.catalog,
          "live-material")
    gate = forward_gates.observe_forward_drills(resolution, now=checked_at)
    _need(gate.required is False, "drills")
    forward_gates.validate_preserved_contract(resolution, None)


def _preserved_common(value):
    _need(type(value) is OutagePreflight, "preserved-type")
    _need(type(value.command_count) is int and 0 < value.command_count <= normal.MAX_COMMANDS
          and type(value.captured_bytes) is int and 0 < value.captured_bytes <= normal.MAX_BYTES
          and type(value.remaining_us) is int and 0 < value.remaining_us <= OUTAGE_FRESHNESS_SECONDS * 1_000_000,
          "bounds")
    entry, checked, until = _utc(value.entry_at), _utc(value.checked_at), _utc(value.valid_until)
    _need(entry <= checked < until and until == min(
        checked + timedelta(microseconds=value.remaining_us), entry + timedelta(seconds=OUTAGE_FRESHNESS_SECONDS)),
        "validity")
    parsed = _coverage(value.source)
    _eligible(value.source, checked)
    available = value.availability
    _need(type(available) in (ServiceUnavailable, MissingExpiredEvidence)
          and type(available.observation) is negative.HttpObservation, "availability")
    observed = negative.replay_http(available.observation.raw, endpoint=negative.ACTIONS_ENDPOINT)
    _need(negative._same(observed, available.observation), "service")
    _need(entry <= _utc(available.observed_at), "service-clock")
    if type(available) is ServiceUnavailable:
        _need(observed.status in (502, 503, 504), "unavailable")
        history_at = available.observed_at
    else:
        _need(observed.status == 200 and type(available.collected) is collect.CollectedOutageHistory,
              "missing")
        history_at = _utc(available.collected.normal.history.observed_at)
        _need(available.observed_at <= _utc(available.collected.started_at) <= history_at
              and value.command_count >= available.collected.normal.command_count
              and value.captured_bytes >= available.collected.normal.captured_bytes, "history-clock")
    snapshot = value.index
    _need(type(snapshot) is negative.IndexSnapshot
          and type(snapshot.before_rule) is negative.RuleView and type(snapshot.after_rule) is negative.RuleView
          and type(snapshot.before_refs) is negative.RefInventory and type(snapshot.after_refs) is negative.RefInventory,
          "index")
    replayed = negative.replay_index(parsed.setup, snapshot.before_rule.raw, snapshot.before_refs.raw,
        snapshot.after_rule.raw, snapshot.after_refs.raw, producer_sha=value.source.admission.resolution.candidate.sha,
        started_at=snapshot.started_at, observed_at=snapshot.observed_at)
    _need(negative._same(replayed, snapshot) and not replayed.before_refs.rows and not replayed.after_refs.rows,
          "negative-intent")
    _need(history_at <= _utc(snapshot.started_at) <= _utc(snapshot.observed_at) <= checked, "index-clock")
    return value


def validate_preserved(value):
    """Current raw replay; counters prove bounds, not process-transcript truth."""
    try:
        _need(type(value) is OutagePreflight and type(value.source) is resolver.OutageForwardResolution
              and type(value.source.admission) is resolver.ForwardAdmission, "preserved-type")
        resolution = value.source.admission.resolution
        bridge.validate_current_source(resolution)
        _preserved_common(value)
        if type(value.availability) is MissingExpiredEvidence:
            collected = value.availability.collected
            request = bridge.build_request(resolution, collected.normal)
            collect.require_missing_expired(request, collected, workflow=resolution.workflow)
        return value
    except evidence.CompatRefusal:
        raise
    except Exception:
        raise evidence.CompatRefusal("compat-stale", "outage-preflight-preserved-invalid") from None


def _validate_preserved_custody(value):
    """Closed original-time replay; no current loader/clock/network access."""
    try:
        _need(type(value) is OutagePreflight and type(value.source) is resolver.OutageForwardResolution
              and type(value.source.admission) is resolver.ForwardAdmission, "preserved-type")
        resolution = value.source.admission.resolution
        bridge._validate_source_custody(resolution)
        _preserved_common(value)
        if type(value.availability) is MissingExpiredEvidence:
            collected = value.availability.collected
            request = bridge._build_context3_custody_request(resolution, collected.normal)
            collect._require_missing_expired_custody(request, collected, workflow=resolution.workflow)
        return value
    except evidence.CompatRefusal:
        raise
    except Exception:
        raise evidence.CompatRefusal("compat-stale", "outage-preflight-preserved-invalid") from None


def _source(budget, repo, sha, gh, git, token, phase, env):
    budget.check()
    operation = transport.OperationBudget(budget.end, capture=budget.capture, clock=budget.clock)
    try:
        client = transport.ReleaseGh(gh, token, phase / "config", phase / "work",
                                     budget=operation, base_env=env)
        return resolver.resolve_outage_forward(client, repo, git, sha)
    except Exception:
        operation.failed = True
        raise
    finally:
        operation.close()


def _preflight(repo_dir, candidate_sha, *, gh_binary, git_binary, token, scratch_dir,
               deadline, capture, clock, wall_clock, base_env):
    installed = loader.load_consumer_policy()
    if installed is None:
        return normal._preflight(repo_dir, candidate_sha, gh_binary=gh_binary, git_binary=git_binary,
            token=token, scratch_dir=scratch_dir, deadline=deadline, capture=capture,
            clock=clock, wall_clock=wall_clock, base_env=base_env)
    budget = normal._Budget(deadline, capture=capture, clock=clock)
    # Keep the configured outage operation and wire8 validity on their original
    # clock/window; the ordinary normal fallback above uses the larger window.
    budget.end = min(budget.end, budget._last + OUTAGE_FRESHNESS_SECONDS)
    index_operation = None
    try:
        entry = _utc(wall_clock())
        _need(type(candidate_sha) is str and common.SHA1_RE.fullmatch(candidate_sha), "source")
        _need(type(token) is str and 0 < len(token) <= 4096 and all(33 <= ord(c) <= 126 for c in token), "token")
        normal._regular(repo_dir, directory=True)
        normal._regular(gh_binary); normal._regular(git_binary)
        normal._scratch(scratch_dir, repo_dir)
        index_dir = scratch_dir / "negative-index"
        index_dir.mkdir(mode=0o700); (index_dir / "config").mkdir(mode=0o700)
        budget.bind(scratch_dir, gh_binary, git_binary)
        env = common.scrubbed_env(base=base_env)
        before = _source(budget, repo_dir, candidate_sha, gh_binary, git_binary, token,
                         scratch_dir / "source-before", env)
        bridge.validate_current_source(before.admission.resolution)
        _need(before.coverage.policy_raw == installed.raw, "policy-changed")
        index_operation = transport.OperationBudget(budget.end, capture=budget.capture, clock=clock)
        reader = index_transport.open_reader(gh_binary, token, index_dir / "config",
                                             budget=index_operation, base_env=env)
        _need(reader.setup.raw == installed.setup.raw, "setup-changed")
        raw = reader.observe_actions_service()
        service = negative.replay_http(raw, endpoint=negative.ACTIONS_ENDPOINT)
        service_at = _utc(wall_clock())
        _need(entry <= service_at, "service-clock")
        qualified = history = None
        if service.status == 200:
            phase = scratch_dir / "actions"
            client = collect.ActionsGh(str(gh_binary), token, phase / "config",
                seconds=min(600, budget.remaining()), base_env=env, capture=budget.capture, clock=clock)
            history = collect.collect_outage_history(collect.CollectionRequest(candidate_sha,
                before.admission.resolution.workflow), client, phase / "work", wall_clock=wall_clock)
            collect.validate_outage_history(history, workflow=before.admission.resolution.workflow,
                                            producer_sha=candidate_sha)
            try:
                qualified = bridge.qualify(before.admission.resolution, history.normal)
            except evidence.CompatRefusal:
                # Only a pure policy result chooses this branch; never a failed
                # capture/collection retry or a supplied unavailable enum.
                request = bridge.build_request(before.admission.resolution, history.normal)
                collect.require_missing_expired(request, history, workflow=before.admission.resolution.workflow)
            availability = MissingExpiredEvidence(service, service_at, history)
        else:
            _need(service.status in (502, 503, 504), "service")
            availability = ServiceUnavailable(service, service_at)
        budget.check()
        if qualified is None:
            _coverage(before)
            _eligible(before, service_at)
            started = _utc(wall_clock())
            first_rule, first_refs = reader.read_rule(), reader.read_refs(candidate_sha)
        after = _source(budget, repo_dir, candidate_sha, gh_binary, git_binary, token,
                        scratch_dir / "source-after", env)
        budget.check()
        observed = history.normal.history.observed_at if history is not None else service_at
        contract = normal._contract_pair(before.admission, after.admission, observed)
        _need(before.coverage == after.coverage, "source-changed")
        if qualified is not None:
            qualified = bridge.qualify(after.admission.resolution, history.normal)
            checked = _utc(wall_clock())
            _need(entry <= service_at <= history.started_at <= observed <= checked, "clock")
            _need(contract is None or contract.observed_at <= checked, "contract-clock")
            drills = forward_gates.observe_forward_drills(after.admission.resolution, now=checked)
            budget.check()
            return normal.NormalPreflight(after.admission.resolution, qualified, history.normal,
                budget.commands, budget.captured_bytes, drills, contract)
        last_rule, last_refs = reader.read_rule(), reader.read_refs(candidate_sha)
        index_at = _utc(wall_clock())
        snapshot = negative.replay_index(installed.setup, first_rule, first_refs, last_rule, last_refs,
            producer_sha=candidate_sha, started_at=started, observed_at=index_at)
        budget.check()
        checked = _utc(wall_clock())
        remaining = int(budget.remaining() * 1_000_000)
        until = min(checked + timedelta(microseconds=remaining), entry + timedelta(seconds=OUTAGE_FRESHNESS_SECONDS))
        value = OutagePreflight(after, availability, snapshot, entry, checked, remaining, until,
                               budget.commands, budget.captured_bytes)
        validate_preserved(value)
        budget.check()
        return value
    except Exception:
        budget.failed = True
        if index_operation is not None:
            index_operation.failed = True
        raise
    finally:
        try:
            if index_operation is not None:
                index_operation.close()
        finally:
            budget.close()


def preflight(repo_dir: Path, candidate_sha: str, *, gh_binary: Path, git_binary: Path,
              token: str, scratch_dir: Path, deadline: float):
    """Fixed concrete configuration; never accept externally supplied evidence."""
    try:
        return _preflight(repo_dir, candidate_sha, gh_binary=gh_binary, git_binary=git_binary,
            token=token, scratch_dir=scratch_dir, deadline=deadline, capture=runtime.bounded_command,
            clock=time.monotonic, wall_clock=lambda: datetime.now(timezone.utc), base_env=os.environ)
    except (evidence.CompatRefusal, common.ReleaseError):
        raise
    except Exception:
        raise evidence.CompatRefusal("compat-stale", "outage-preflight-invalid") from None
