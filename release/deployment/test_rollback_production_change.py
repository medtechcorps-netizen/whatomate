from __future__ import annotations

import copy
import datetime as dt
import inspect
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import rollback_production_change as rollback
import verify_production_release as common


def binding(number: int, digest_character: str, hash_character: str) -> dict[str, object]:
    return {
        "run_id": str(number),
        "run_attempt": 1,
        "artifact_id": str(number + 100),
        "artifact_digest": "sha256:" + digest_character * 64,
        "sha256": hash_character * 64,
    }


def state(digest_character: str) -> dict[str, object]:
    digests = {key: "sha256:" + digest_character * 64 for key in ("web", "meta-relay", "gmail-relay")}
    return {
        "app_identity_sha256": "1" * 64,
        "default_ingress_sha256": "2" * 64,
        "app_updated_at_sha256": "3" * 64,
        "active_deployment_identity_sha256": "4" * 64,
        "canonical_spec_sha256": "5" * 64,
        "environment_values_sha256": "6" * 64,
        "non_source_projection_sha256": "7" * 64,
        "source_mode": "digest-images",
        "images": common.sanitized_image_records(digests),
    }


def rollback_receipt() -> dict[str, object]:
    before = state("2")
    after = state("1")
    after["app_updated_at_sha256"] = "8" * 64
    after["active_deployment_identity_sha256"] = "9" * 64
    after["canonical_spec_sha256"] = "a" * 64
    current = {"kind": "apply-receipt", **binding(101, "1", "b")}
    target = binding(102, "2", "c")
    recovery = binding(103, "3", "d")
    return {
        "schema_version": 1,
        "authority": "production-phase-rollback-receipt",
        "repository": common.REPOSITORY,
        "completed_at": "2026-08-27T00:02:00Z",
        "control": {
            "workflow_sha": "a" * 40,
            "workflow_path": ".github/workflows/rollback-production-phase.yml",
            "run_id": "501",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": "b" * 64,
            "change_schema_sha256": "c" * 64,
            "controller_sha256": "d" * 64,
        },
        "lineage": {
            "event_sequence": 3,
            "phase_ordinal": 1,
            "operation": "rollback",
            "from": "bridge",
            "to": "baseline",
            "predecessor_kind": "apply-receipt",
            "predecessor_state_sha256": current["sha256"],
            "phase": "baseline",
            "phase_source_sha": "e" * 40,
        },
        "authorities": {
            "rollout_plan_sha256": "f" * 64,
            "current_state": current,
            "target_state": target,
            "recovery": recovery,
            "mutation_intent": {
                "run_id": "501",
                "run_attempt": 1,
                "artifact_id": "701",
                "artifact_name": "production-mutation-intent-rollback-501-1",
                "artifact_digest": "sha256:" + "4" * 64,
                "sha256": "5" * 64,
            },
            "main_lock_proof": {
                "run_id": "501",
                "run_attempt": 1,
                "artifact_id": "702",
                "artifact_name": "production-main-lock-proof-rollback-501-1",
                "artifact_digest": "sha256:" + "6" * 64,
                "sha256": "7" * 64,
            },
        },
        "target_authority": {"production_plan_sha256": "1" * 64},
        "provider_transition": {
            "http_methods_used": ["GET", "PUT"],
            "http_request_count": 15,
            "mutation_request_count": 1,
            "endpoint_labels": ["app", "deployment"],
            "mutation_fingerprint_sha256": "2" * 64,
            "ambiguous_reconciled": False,
        },
        "before": before,
        "after": after,
        "gates": {"deployment_succeeded": True, "migration_succeeded": True},
        "rollback": common.ROLLBACK_FLOORS["baseline"],
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": [
                "app-health", "app-ready", "meta-live", "meta-ready",
                "gmail-live", "gmail-ready",
            ],
            "route_contract_sha256": "3" * 64,
        },
    }


class RollbackControllerTests(unittest.TestCase):
    def test_isolated_direct_entrypoint_resolves_only_sibling_controls(self) -> None:
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(Path(rollback.__file__).resolve()), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_recovery_expiring_during_rollback_preflight_blocks_before_put(self) -> None:
        initial = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        recovery = {
            "issued_at": common.format_timestamp(initial),
            "expires_at": common.format_timestamp(initial + dt.timedelta(minutes=5)),
        }
        moments = iter((initial, initial + dt.timedelta(minutes=6)))
        clock = lambda: next(moments)
        self.assertEqual(rollback.apply_control._clock_value(clock), initial)
        with self.assertRaises(common.ReleaseError):
            rollback.apply_control.require_fresh_immediately_before_mutation(
                recovery=recovery, clock=clock
            )
        source = inspect.getsource(rollback.rollback_change)
        self.assertLess(source.index("cas_before"), source.index("require_fresh_immediately_before_mutation"))
        self.assertLess(source.index("require_fresh_immediately_before_mutation"), source.index("put_app_once"))

    def test_rollback_expiry_is_exclusive_at_boundary_and_fraction(self) -> None:
        issued = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        expires = issued + dt.timedelta(minutes=5)
        recovery = {
            "issued_at": common.format_timestamp(issued),
            "expires_at": common.format_timestamp(expires),
        }
        for checked in (expires, expires + dt.timedelta(microseconds=1)):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    rollback.apply_control.require_fresh_immediately_before_mutation(
                        recovery=recovery, clock=lambda: checked
                    )

    def test_failed_canary_apply_receipt_is_valid_current_authority(self) -> None:
        request = rollback.validate_request(
            {
                "current_state": {"kind": "apply-receipt", **binding(1, "1", "a")},
                "target_state": binding(2, "2", "b"),
                "recovery": binding(3, "3", "c"),
                "rollout_plan_sha256": "d" * 64,
            }
        )
        self.assertEqual(request["current_state"]["kind"], "apply-receipt")
        request["current_state"]["kind"] = "unknown"
        with self.assertRaises(common.ReleaseError):
            rollback._current_binding(request["current_state"], "current")

    def test_rollback_receipt_and_final_state_preserve_two_link_lineage(self) -> None:
        receipt = rollback_receipt()
        rollback.validate_rollback_receipt(receipt)
        receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
        final = common.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="4" * 64,
            control={
                "workflow_sha": "a" * 40,
                "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
                "run_id": "601",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": "b" * 64,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:03:00Z",
        )
        self.assertEqual(final["lineage"]["predecessor_kind"], "rollback-receipt")
        self.assertEqual(final["lineage"]["predecessor_state_sha256"], receipt_hash)
        self.assertEqual(final["lineage"]["event_sequence"], 3)
        self.assertEqual(final["lineage"]["phase_ordinal"], 1)
        common.validate_phase_state(final)

    def test_forbidden_floor_and_tampered_current_binding_fail(self) -> None:
        receipt = rollback_receipt()
        receipt["lineage"]["from"] = "backend"
        with self.assertRaises(common.ReleaseError):
            rollback.validate_rollback_receipt(receipt)

    def test_phase_state_from_another_rollout_plan_cannot_be_spliced(self) -> None:
        authorities = {"rollout_plan_sha256": "a" * 64}
        current = {"evidence": {"rollout_plan_sha256": "b" * 64}}
        target = {"evidence": {"rollout_plan_sha256": "a" * 64}}
        with self.assertRaises(common.ReleaseError):
            rollback._require_rollout_plan_authority(
                authorities, "phase-state", current, target
            )

    def test_failed_apply_receipt_from_another_rollout_plan_cannot_be_spliced(self) -> None:
        authorities = {"rollout_plan_sha256": "a" * 64}
        current = {"authorities": {"rollout_plan_sha256": "b" * 64}}
        target = {"evidence": {"rollout_plan_sha256": "a" * 64}}
        with self.assertRaises(common.ReleaseError):
            rollback._require_rollout_plan_authority(
                authorities, "apply-receipt", current, target
            )
        receipt = rollback_receipt()
        receipt["lineage"]["predecessor_state_sha256"] = "0" * 64
        with self.assertRaises(common.ReleaseError):
            rollback.validate_rollback_receipt(receipt)

    def test_orphan_rollback_inherits_only_the_exact_reconciled_lock_chain(self) -> None:
        rule_id = "BPR_lock_123"
        root = {
            **binding(501, "4", "5"),
            "artifact_name": "production-main-lock-apply-501-1",
        }
        acquired = {
            "mode": "planned",
            "strategy": "acquire",
            "branch": "main",
            "rule_id": rule_id,
            "rule_identity_sha256": common.sha256_bytes(rule_id.encode("utf-8")),
            "expected_pre_lock": {
                "lock_branch": False,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "expected_post_lock": {
                "lock_branch": True,
                "is_admin_enforced": True,
                "lock_allows_fetch_and_merge": False,
            },
            "root_acquire_intent": root,
            "owner_operation": "apply",
            "owner_run_id": "501",
            "owner_run_attempt": 1,
            "owner_control_sha": "a" * 40,
            "owner_intent_sha256": None,
        }
        intent_sha = "6" * 64
        current = {
            "intent": {
                "binding": {**binding(501, "7", "6")},
                "lock": copy.deepcopy(acquired),
            }
        }
        inherited = copy.deepcopy(acquired)
        inherited["strategy"] = "inherit"
        inherited["expected_pre_lock"]["lock_branch"] = True
        inherited["owner_intent_sha256"] = intent_sha
        self.assertEqual(
            rollback._bind_rollback_lock_authority(
                "reconciliation-receipt", current, inherited
            ),
            inherited,
        )
        for mutate in (
            lambda value: value.__setitem__("rule_id", "BPR_other"),
            lambda value: value["root_acquire_intent"].__setitem__("sha256", "8" * 64),
            lambda value: value.__setitem__("owner_intent_sha256", "9" * 64),
        ):
            candidate = copy.deepcopy(inherited)
            mutate(candidate)
            with self.assertRaises(common.ReleaseError):
                rollback._bind_rollback_lock_authority(
                    "reconciliation-receipt", current, candidate
                )

        nested = {"intent": {"binding": binding(601, "8", "9"), "lock": inherited}}
        self.assertEqual(
            rollback._bind_rollback_lock_authority(
                "reconciliation-receipt", nested, inherited
            ),
            inherited,
        )
        with self.assertRaises(common.ReleaseError):
            rollback._bind_rollback_lock_authority(
                "phase-state", {}, inherited
            )


if __name__ == "__main__":
    unittest.main()
