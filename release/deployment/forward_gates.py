"""Source-bound restore-drill observation; no reads or deployment authority.

The caller must obtain drills.json from reviewed immutable source and pass an
actual UTC observation time. Dates are reviewed claims, not proof of a restore
by themselves. Production preserves its observation for same-run record retry.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
import hashlib

import ship_common as common
import record_resolver as resolver


MAX_DRILL_BYTES = 4096
MAX_DRILL_AGE = timedelta(days=90)
DRILL_KEYS = ("staging_restore", "production_fork")


class DrillRefusal(Exception):
    """Fixed module-owned messages, without untrusted record values."""


@dataclass(frozen=True, slots=True)
class DrillGate:
    required: bool
    observed_at: datetime
    source_sha256: str | None
    staging_restore: datetime | None
    production_fork: datetime | None


def require_restore_drills(raw: bytes | None, *, direction: str, kind: str,
                          dormant: bool, now: datetime) -> DrillGate:
    """Require both successful-drill dates only for active forward DB changes.

    The v1 file records the last successfully reviewed completion of each drill,
    or null when none is established. A failed/partial drill must never replace
    one of those dates. PR21 will update this same file after actual drills.
    This gate never exempts a caller from other forward or rollback checks.
    """
    if (type(direction) is not str or direction not in {"forward", "rollback", "restore"}
            or type(kind) is not str or kind not in {"none", "expand", "data", "contract"}
            or type(dormant) is not bool or dormant and kind not in {"expand", "data"}
            or type(now) is not datetime or now.utcoffset() != timedelta(0)):
        raise DrillRefusal("restore-drills-input-invalid")
    required = direction == "forward" and kind != "none" and not dormant
    if not required:
        return DrillGate(False, now, None, None, None)
    if raw is None:
        raise DrillRefusal("restore-drills-missing")
    if type(raw) is not bytes or not 0 < len(raw) <= MAX_DRILL_BYTES:
        raise DrillRefusal("restore-drills-invalid")
    try:
        value = common.loads_strict(raw)
        if (type(value) is not dict or set(value) != {"version", *DRILL_KEYS}
                or type(value["version"]) is not int or value["version"] != 1):
            raise DrillRefusal("restore-drills-invalid")
        dates = []
        for name in DRILL_KEYS:
            if value[name] is None:
                raise DrillRefusal("restore-drills-missing")
            completed = common.require_timestamp(value[name], "restore-drills-invalid")
            if completed > now:
                raise DrillRefusal("restore-drills-future")
            if now - completed > MAX_DRILL_AGE:
                raise DrillRefusal("restore-drills-stale")
            dates.append(completed)
    except (common.ReleaseError, ValueError, TypeError, KeyError, RecursionError):
        raise DrillRefusal("restore-drills-invalid") from None
    return DrillGate(True, now, hashlib.sha256(raw).hexdigest(), *dates)


def observe_forward_drills(resolution, *, now: datetime) -> DrillGate:
    """Use the complete immutable audit already captured by the source operation.

    A typed value is not authentication: the operational preflight constructs
    this projection itself. No new source read, clock or deadline is created.
    """
    try:
        if (type(resolution) is not resolver.ForwardResolution
                or type(now) is not datetime or now.tzinfo is not timezone.utc or now.fold != 0):
            raise DrillRefusal("restore-drills-input-invalid")
        audit = resolver.validate_forward_audit(resolution.audit,
            candidate_sha=resolution.candidate.sha,
            database_base_source=resolution.database_base_source,
            latest_record_sha=resolution.resolved.latest.record_sha,
            selected_web_source_sha=resolution.resolved.selected.actual_web_source_sha)
        change = resolution.classification
        if type(change) is not resolver.schema_change.PolicyClassification:
            raise DrillRefusal("restore-drills-input-invalid")
        return require_restore_drills(audit.drills.raw, direction="forward",
            kind=change.kind, dormant=change.dormant, now=now)
    except (resolver.policy.PolicyRefusal, AttributeError, TypeError, ValueError):
        raise DrillRefusal("restore-drills-input-invalid") from None


def validate_preserved_drills(resolution, observation: DrillGate) -> DrillGate:
    """Replay integrity at the ORIGINAL instant, never record-retry wall time."""
    if (type(observation) is not DrillGate or type(observation.required) is not bool
            or (observation.source_sha256 is not None and type(observation.source_sha256) is not str)
            or any(value is not None and (type(value) is not datetime
                or value.tzinfo is not timezone.utc or value.fold != 0)
                for value in (observation.staging_restore, observation.production_fork))):
        raise DrillRefusal("restore-drills-observation-invalid")
    expected = observe_forward_drills(resolution, now=observation.observed_at)
    if expected != observation:
        raise DrillRefusal("restore-drills-observation-invalid")
    return observation
