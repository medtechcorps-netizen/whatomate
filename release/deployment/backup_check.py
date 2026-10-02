#!/usr/bin/env python3
"""The 36-hour PostgreSQL backup gate for ship.yml.

Copied and trimmed from observe_production_recovery.py:30-32, 129-180 and
200-233. The cluster read binds the protected cluster id to the app spec's
PostgreSQL binding through the cluster-name hash. A cluster object can carry
connection credentials, so it is validated and discarded at once; nothing
from it is returned or emitted.
"""

from __future__ import annotations

import datetime as dt
import decimal
import sys
from pathlib import Path
from typing import Any, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common


MAX_BACKUP_AGE = dt.timedelta(hours=36)


def validate_cluster(
    value: Any,
    *,
    expected_id: str,
    expected_name_sha256: str,
    expected_version: str,
    expected_region: str,
) -> None:
    if type(value) is not dict or type(value.get("database")) is not dict:
        common.fail("provider-invalid:database")
    database = value["database"]
    try:
        if common.require_uuid(database.get("id"), "target-invalid:postgres-cluster") != expected_id:
            common.fail("target-invalid:postgres-cluster")
        if database.get("status") != "online":
            common.fail("topology-differs:postgres-cluster-status")
        engine = database.get("engine")
        if type(engine) is not str or engine.lower() != "pg":
            common.fail("topology-differs:postgres-cluster-engine")
        if database.get("version") != expected_version:
            common.fail("topology-differs:postgres-cluster-version")
        if database.get("region") != expected_region:
            common.fail("topology-differs:postgres-cluster-region")
        name = database.get("name")
        if type(name) is not str or not name or common.sha256_text(name) != expected_name_sha256:
            common.fail("target-invalid:postgres-cluster")
    finally:
        # The envelope can contain connection URIs and passwords.
        database = None
        value = None


def fresh_backup(value: Any, now: dt.datetime) -> dict[str, int]:
    """observe_production_recovery.py:200-233: the newest backup must be at
    most 36 hours old and not future-dated."""
    if (
        type(value) is not dict
        or "backup_progress" in value
        or type(value.get("backups")) is not list
        or not value["backups"]
    ):
        common.fail("backup-stale:inventory")
    timestamps: list[dt.datetime] = []
    for item in value["backups"]:
        if type(item) is not dict:
            common.fail("backup-stale:inventory")
        size = item.get("size_gigabytes")
        size_valid = (type(size) is int and size > 0) or (
            type(size) is decimal.Decimal and size.is_finite() and size > 0
        )
        if not size_valid:
            common.fail("backup-stale:size")
        timestamps.append(common.require_timestamp(item.get("created_at"), "backup-stale:created-at"))
    newest = max(timestamps)
    checked = common.checked_clock(now)
    if newest > checked or checked - newest > MAX_BACKUP_AGE:
        common.fail("backup-stale")
    return {"age_hours": int((checked - newest).total_seconds() // 3600)}


def check(client: Any, *, target: Mapping[str, Any], now: dt.datetime) -> dict[str, int]:
    postgres = target["postgres"]
    validate_cluster(
        client.get_database(),
        expected_id=client.postgres_cluster_id,
        expected_name_sha256=postgres["cluster_name_sha256"],
        expected_version=postgres["version"],
        expected_region=postgres["region"],
    )
    return fresh_backup(client.get_backups(), now)
