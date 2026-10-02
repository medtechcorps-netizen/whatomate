from __future__ import annotations

import datetime as dt
import decimal
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import backup_check
import do_app
import ship_common as common
import spec_images
import test_ship_support as support


NOW = support.NOW


def stamp(hours: float) -> str:
    return (NOW - dt.timedelta(hours=hours)).strftime("%Y-%m-%dT%H:%M:%SZ")


def inventory(*items: dict) -> dict:
    return {"backups": list(items)}


def backup(hours: float, size: object = decimal.Decimal("0.03")) -> dict:
    return {"created_at": stamp(hours), "size_gigabytes": size}


class FreshBackupTests(unittest.TestCase):
    def test_35_hours_passes_and_37_fails(self) -> None:
        self.assertEqual(backup_check.fresh_backup(inventory(backup(35)), NOW), {"age_hours": 35})
        with self.assertRaisesRegex(common.ReleaseError, "^backup-stale$"):
            backup_check.fresh_backup(inventory(backup(37)), NOW)

    def test_newest_backup_decides(self) -> None:
        self.assertEqual(backup_check.fresh_backup(inventory(backup(100), backup(3), backup(50)), NOW), {"age_hours": 3})

    def test_future_dated_backup_fails(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^backup-stale$"):
            backup_check.fresh_backup(inventory(backup(-1)), NOW)

    def test_empty_inventory_and_backup_in_progress_fail(self) -> None:
        for value in (inventory(), {"backups": None}, {}, [], {"backups": [backup(1)], "backup_progress": "50"}):
            with self.subTest(value=value), self.assertRaisesRegex(common.ReleaseError, "^backup-stale"):
                backup_check.fresh_backup(value, NOW)

    def test_sizes(self) -> None:
        self.assertEqual(backup_check.fresh_backup(inventory(backup(1, 2)), NOW), {"age_hours": 1})
        for size in (0, -1, None, "1", decimal.Decimal("NaN"), decimal.Decimal("0"), True):
            with self.subTest(size=size), self.assertRaisesRegex(common.ReleaseError, "^backup-stale:size$"):
                backup_check.fresh_backup(inventory(backup(1, size)), NOW)

    def test_decimal_sizes_parse_from_provider_json(self) -> None:
        value = common.loads_strict('{"backups":[{"created_at":"%s","size_gigabytes":0.0328}]}' % stamp(2), decimals=True)
        self.assertEqual(backup_check.fresh_backup(value, NOW), {"age_hours": 2})

    def test_bad_timestamps(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^backup-stale:created-at$"):
            backup_check.fresh_backup(inventory({"created_at": "yesterday", "size_gigabytes": 1}), NOW)


class ClusterBindingTests(unittest.TestCase):
    def client(self, fake: support.FakeDO) -> do_app.DOAppClient:
        return do_app.DOAppClient(
            support.APP_ID, support.PG_ID, support.DO_TOKEN,
            expected_app_id_sha256=support.sha256_text(support.APP_ID), allow_put=False, opener=fake,
        )

    def test_check_passes_and_returns_only_the_age(self) -> None:
        fake = support.FakeDO(backup_age_hours=5)
        result = backup_check.check(self.client(fake), target=spec_images.validate_target(support.make_target()), now=NOW)
        self.assertEqual(result, {"age_hours": 5})
        self.assertNotIn(support.FAKE_PASSWORD, repr(result))
        self.assertEqual([path for _method, path in fake.requests],
                         [f"/v2/databases/{support.PG_ID}", f"/v2/databases/{support.PG_ID}/backups?page=1&per_page=200"])

    def test_cluster_binding_refusals(self) -> None:
        target = spec_images.validate_target(support.make_target())
        cases = {
            "identity": ({"id": "99999999-9999-4999-8999-999999999999"}, "target-invalid:postgres-cluster"),
            "status": ({"status": "migrating"}, "topology-differs:postgres-cluster-status"),
            "engine": ({"engine": "mysql"}, "topology-differs:postgres-cluster-engine"),
            "version": ({"version": "16"}, "topology-differs:postgres-cluster-version"),
            "region": ({"region": "nyc1"}, "topology-differs:postgres-cluster-region"),
            "name": ({"name": "another-cluster"}, "target-invalid:postgres-cluster"),
        }
        for label, (change, code) in cases.items():
            fake = support.FakeDO()
            fake.database = dict(fake.database, **change)
            with self.subTest(case=label), self.assertRaises(common.ReleaseError) as caught:
                backup_check.check(self.client(fake), target=target, now=NOW)
            self.assertEqual(str(caught.exception), code)
            self.assertNotIn(support.FAKE_PASSWORD, str(caught.exception))

    def test_stale_backup_through_the_client(self) -> None:
        fake = support.FakeDO(backup_age_hours=40)
        with self.assertRaisesRegex(common.ReleaseError, "^backup-stale$"):
            backup_check.check(self.client(fake), target=spec_images.validate_target(support.make_target()), now=NOW)

    def test_malformed_envelope(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^provider-invalid:database$"):
            backup_check.validate_cluster({"databases": []}, expected_id=support.PG_ID, expected_name_sha256="0" * 64,
                                          expected_version="17", expected_region="sgp1")


if __name__ == "__main__":
    unittest.main()
