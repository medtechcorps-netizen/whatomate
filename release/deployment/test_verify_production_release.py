from __future__ import annotations

import copy
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verify_production_release as release


SHA = "a" * 40
HASH = "b" * 64
DIGESTS = {
    "web": "sha256:" + "1" * 64,
    "meta-relay": "sha256:" + "2" * 64,
    "gmail-relay": "sha256:" + "3" * 64,
}


def provider_state() -> dict[str, object]:
    return {
        "app_identity_sha256": "1" * 64,
        "default_ingress_sha256": "2" * 64,
        "app_updated_at_sha256": "3" * 64,
        "active_deployment_identity_sha256": "4" * 64,
        "canonical_spec_sha256": "5" * 64,
        "environment_values_sha256": "6" * 64,
        "non_source_projection_sha256": "7" * 64,
        "source_mode": "digest-images",
        "images": release.sanitized_image_records(DIGESTS),
    }


def apply_receipt() -> dict[str, object]:
    before = provider_state()
    before["source_mode"] = "legacy-git"
    before["images"] = []
    after = provider_state()
    return {
        "schema_version": 1,
        "authority": "production-phase-apply-receipt",
        "repository": release.REPOSITORY,
        "completed_at": "2026-08-27T00:00:00Z",
        "control": {
            "workflow_sha": SHA,
            "workflow_path": ".github/workflows/apply-production-phase.yml",
            "run_id": "101",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": HASH,
            "change_schema_sha256": "c" * 64,
            "controller_sha256": "d" * 64,
        },
        "lineage": {
            "event_sequence": 1,
            "phase_ordinal": 1,
            "operation": "activate",
            "from": "genesis",
            "to": "baseline",
            "predecessor_kind": "genesis",
            "predecessor_state_sha256": "e" * 64,
            "phase": "baseline",
            "phase_source_sha": "f" * 40,
        },
        "authorities": {
            "rollout_plan_sha256": "8" * 64,
            "production_plan": {
                "run_id": "201",
                "run_attempt": 1,
                "artifact_id": "301",
                "artifact_digest": "sha256:" + "9" * 64,
                "sha256": "a" * 64,
            },
            "recovery": {
                "run_id": "202",
                "run_attempt": 1,
                "artifact_id": "302",
                "artifact_digest": "sha256:" + "8" * 64,
                "sha256": "b" * 64,
            },
            "mutation_intent": {
                "run_id": "101",
                "run_attempt": 1,
                "artifact_id": "303",
                "artifact_name": "production-mutation-intent-apply-101-1",
                "artifact_digest": "sha256:" + "7" * 64,
                "sha256": "6" * 64,
            },
            "main_lock_proof": {
                "run_id": "101",
                "run_attempt": 1,
                "artifact_id": "304",
                "artifact_name": "production-main-lock-proof-apply-101-1",
                "artifact_digest": "sha256:" + "5" * 64,
                "sha256": "4" * 64,
            },
        },
        "provider_transition": {
            "http_methods_used": ["GET", "PUT"],
            "http_request_count": 15,
            "mutation_request_count": 1,
            "endpoint_labels": ["app", "deployment"],
            "mutation_fingerprint_sha256": "c" * 64,
            "ambiguous_reconciled": False,
        },
        "before": before,
        "after": after,
        "gates": {"deployment_succeeded": True, "migration_succeeded": True},
        "rollback": release.ROLLBACK_FLOORS["baseline"],
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": [
                "app-health", "app-ready", "meta-live", "meta-ready",
                "gmail-live", "gmail-ready",
            ],
            "route_contract_sha256": "d" * 64,
        },
    }


def full_binding(run_id: str, artifact_id: str, name: str, digest: str) -> dict[str, object]:
    return {
        "run_id": run_id,
        "run_attempt": 1,
        "artifact_id": artifact_id,
        "artifact_name": name,
        "artifact_digest": "sha256:" + "7" * 64,
        "sha256": digest,
    }


class ProductionReleaseVerifierTests(unittest.TestCase):
    def test_strict_json_rejects_duplicates_and_floats(self) -> None:
        with self.assertRaises(release.ReleaseError):
            release.loads_strict('{"a":1,"a":2}')
        with self.assertRaises(release.ReleaseError):
            release.loads_strict('{"a":1.5}')

    def test_semantic_provider_lineage_excludes_only_app_timestamp(self) -> None:
        predecessor = provider_state()
        current = copy.deepcopy(predecessor)
        current["app_updated_at_sha256"] = "9" * 64
        self.assertTrue(
            release.provider_states_share_semantic_lineage(
                predecessor, current, allow_legacy=False
            )
        )
        self.assertNotEqual(
            release.sha256_value(predecessor), release.sha256_value(current)
        )

        mutations = {
            "app identity": lambda state: state.__setitem__(
                "app_identity_sha256", "0" * 64
            ),
            "default ingress": lambda state: state.__setitem__(
                "default_ingress_sha256", "0" * 64
            ),
            "active deployment": lambda state: state.__setitem__(
                "active_deployment_identity_sha256", "0" * 64
            ),
            "canonical spec": lambda state: state.__setitem__(
                "canonical_spec_sha256", "0" * 64
            ),
            "environment": lambda state: state.__setitem__(
                "environment_values_sha256", "0" * 64
            ),
            "non-source": lambda state: state.__setitem__(
                "non_source_projection_sha256", "0" * 64
            ),
            "source mode": lambda state: state.update(
                {"source_mode": "legacy-git", "images": []}
            ),
            "nested image": lambda state: state["images"][0].update(
                {
                    "digest": "sha256:" + "9" * 64,
                    "subject": (
                        state["images"][0]["repository"]
                        + "@sha256:"
                        + "9" * 64
                    ),
                }
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                changed = copy.deepcopy(predecessor)
                mutate(changed)
                self.assertFalse(
                    release.provider_states_share_semantic_lineage(
                        predecessor, changed, allow_legacy=True
                    )
                )

        malformed = copy.deepcopy(predecessor)
        malformed.pop("app_updated_at_sha256")
        with self.assertRaises(release.ReleaseError):
            release.provider_states_share_semantic_lineage(
                predecessor, malformed, allow_legacy=False
            )
        malformed = copy.deepcopy(predecessor)
        malformed["app_updated_at_sha256"] = "not-a-hash"
        with self.assertRaises(release.ReleaseError):
            release.provider_states_share_semantic_lineage(
                predecessor, malformed, allow_legacy=False
            )
        malformed = copy.deepcopy(predecessor)
        malformed["future_unreviewed_key"] = "0" * 64
        with self.assertRaises(release.ReleaseError):
            release.provider_states_share_semantic_lineage(
                predecessor, malformed, allow_legacy=False
            )

    def test_target_descriptor_requires_exact_stable_https_ingress(self) -> None:
        value = release.validate_target_descriptor(
            {
                "app_id": "11111111-1111-4111-8111-111111111111",
                "default_ingress": "https://example.invalid",
            }
        )
        self.assertEqual(value["default_ingress"], "https://example.invalid")
        for bad in (
            {"app_id": value["app_id"]},
            {**value, "default_ingress": "http://example.invalid"},
            {**value, "default_ingress": "https://example.invalid/path"},
        ):
            with self.assertRaises(release.ReleaseError):
                release.validate_target_descriptor(bad)

    def test_recovery_target_descriptor_contains_only_stable_source_identities(self) -> None:
        descriptor = {
            "postgres_cluster_id": "11111111-1111-4111-8111-111111111111",
            "valkey_cluster_id": "22222222-2222-4222-8222-222222222222",
        }
        self.assertEqual(
            release.validate_target_descriptor(descriptor, recovery=True),
            descriptor,
        )
        with self.assertRaises(release.ReleaseError):
            release.validate_target_descriptor(
                {
                    **descriptor,
                    "valkey_recovery_cluster_id": "33333333-3333-4333-8333-333333333333",
                },
                recovery=True,
            )

    def test_apply_receipt_and_canary_state_match_cross_schema_surface(self) -> None:
        receipt = apply_receipt()
        self.assertIs(release.validate_apply_receipt(receipt), receipt)
        receipt_hash = release.sha256_bytes(release.canonical_file_bytes(receipt))
        state = release.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="9" * 64,
            control={
                "workflow_sha": SHA,
                "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
                "run_id": "401",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": HASH,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:01:00Z",
        )
        self.assertEqual(state["lineage"]["event_sequence"], 1)
        self.assertEqual(state["lineage"]["phase_ordinal"], 1)
        self.assertEqual(state["lineage"]["predecessor_kind"], "apply-receipt")
        self.assertEqual(state["lineage"]["predecessor_state_sha256"], receipt_hash)
        self.assertEqual(state["evidence"]["change_receipt_sha256"], receipt_hash)
        release.validate_phase_state(state)

        root = Path(__file__).resolve().parents[2]
        schema = json.loads(
            (root / "release/deployment/production-change.schema.json").read_text(encoding="utf-8")
        )
        required_lineage = set(schema["$defs"]["phaseState"]["properties"]["lineage"]["required"])
        required_evidence = set(schema["$defs"]["phaseState"]["properties"]["evidence"]["required"])
        self.assertEqual(set(state["lineage"]), required_lineage)
        self.assertEqual(set(state["evidence"]), required_evidence)

    def test_receipt_rejects_compatibility_drift_and_false_canary(self) -> None:
        for mutate in (
            lambda value: value["after"].__setitem__("environment_values_sha256", "0" * 64),
            lambda value: value["canary"].__setitem__("completed", True),
            lambda value: value["provider_transition"].__setitem__("mutation_request_count", 2),
        ):
            receipt = apply_receipt()
            mutate(receipt)
            with self.assertRaises(release.ReleaseError):
                release.validate_apply_receipt(receipt)

    def test_reconciled_phase_state_carries_both_exact_artifact_bindings(self) -> None:
        receipt = apply_receipt()
        receipt_hash = release.sha256_bytes(release.canonical_file_bytes(receipt))
        receipt_binding = full_binding(
            "101", "701", "production-phase-apply-101-1", receipt_hash
        )
        reconciliation_binding = full_binding(
            "901",
            "902",
            "production-main-lock-release-reconciliation-901-1",
            "8" * 64,
        )
        state = release.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="9" * 64,
            control={
                "workflow_sha": SHA,
                "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
                "run_id": "403",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": HASH,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:03:00Z",
            change_receipt_binding=receipt_binding,
            main_lock_release_reconciliation_binding=reconciliation_binding,
        )
        self.assertEqual(state["schema_version"], 2)
        self.assertEqual(state["lineage"]["predecessor_kind"], "apply-reconciled-receipt")
        self.assertEqual(state["evidence"]["change_receipt_binding"], receipt_binding)
        self.assertEqual(
            state["evidence"]["main_lock_release_reconciliation_binding"],
            reconciliation_binding,
        )
        release.validate_phase_state(state)
        for key in (
            "change_receipt_binding",
            "main_lock_release_reconciliation_binding",
        ):
            candidate = copy.deepcopy(state)
            candidate["evidence"].pop(key)
            with self.assertRaises(release.ReleaseError):
                release.validate_phase_state(candidate)
        cross_operation = copy.deepcopy(state)
        cross_operation["evidence"]["change_receipt_binding"][
            "artifact_name"
        ] = "production-phase-rollback-101-1"
        with self.assertRaises(release.ReleaseError):
            release.validate_phase_state(cross_operation)

        wrong_builder_binding = copy.deepcopy(receipt_binding)
        wrong_builder_binding["artifact_name"] = "production-phase-rollback-101-1"
        with self.assertRaises(release.ReleaseError):
            release.build_phase_state(
                receipt,
                change_receipt_sha256=receipt_hash,
                canary_sha256="9" * 64,
                control=state["control"],
                completed_at="2026-08-27T00:03:00Z",
                change_receipt_binding=wrong_builder_binding,
                main_lock_release_reconciliation_binding=reconciliation_binding,
            )

    def test_phase_state_operation_kind_pairs_are_exact(self) -> None:
        receipt = apply_receipt()
        receipt_hash = release.sha256_bytes(release.canonical_file_bytes(receipt))
        state = release.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="9" * 64,
            control={
                "workflow_sha": SHA,
                "workflow_path": ".github/workflows/verify-production-crm-canary.yml",
                "run_id": "402",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": HASH,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:02:00Z",
        )
        for kind in ("apply-receipt", "reconciliation-receipt"):
            candidate = copy.deepcopy(state)
            candidate["lineage"]["predecessor_kind"] = kind
            release.validate_phase_state(candidate)
        for kind in ("rollback-receipt", "orphan-rollback-receipt"):
            candidate = copy.deepcopy(state)
            candidate["lineage"]["predecessor_kind"] = kind
            with self.assertRaises(release.ReleaseError):
                release.validate_phase_state(candidate)

        rollback = copy.deepcopy(state)
        rollback["lineage"].update(
            {
                "event_sequence": 2,
                "operation": "rollback",
                "from": "bridge",
                "to": "baseline",
                "phase": "baseline",
            }
        )
        for kind in (
            "rollback-receipt", "orphan-rollback-receipt",
            "reconciliation-receipt",
        ):
            candidate = copy.deepcopy(rollback)
            candidate["lineage"]["predecessor_kind"] = kind
            release.validate_phase_state(candidate)
        rollback["lineage"]["predecessor_kind"] = "apply-receipt"
        with self.assertRaises(release.ReleaseError):
            release.validate_phase_state(rollback)

    def test_rollback_floors_are_exact(self) -> None:
        release.validate_rollback_transition("bridge", "baseline")
        release.validate_rollback_transition("ui", "bridge")
        for edge in (("baseline", "baseline"), ("backend", "baseline"), ("ui", "baseline")):
            with self.assertRaises(release.ReleaseError):
                release.validate_rollback_transition(*edge)

    def test_public_sanitizer_rejects_raw_topology_and_secret_keys(self) -> None:
        for value in (
            {"app_id": "11111111-1111-4111-8111-111111111111"},
            {"safe": "dop_v1_not-public"},
            {"spec": {}},
        ):
            with self.assertRaises(release.ReleaseError):
                release.sanitize_public(value)

    def test_output_is_exclusive_and_canonical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "receipt.json"
            digest = release.write_canonical_output(output, {"ok": True}, root)
            self.assertEqual(digest, release.sha256_bytes(output.read_bytes()))
            self.assertEqual(output.read_bytes(), b'{"ok":true}\n')
            with self.assertRaises(release.ReleaseError):
                release.write_canonical_output(output, {"ok": True}, root)

    def test_mutation_workflows_emit_raw_hash_sidecars_and_recheck_main(self) -> None:
        root = Path(__file__).resolve().parents[2]
        cases = {
            ".github/workflows/apply-production-phase.yml": "production-phase-apply-receipt",
            ".github/workflows/rollback-production-phase.yml": "production-phase-rollback-receipt",
        }
        for relative, stem in cases.items():
            with self.subTest(workflow=relative):
                workflow = (root / relative).read_text(encoding="utf-8")
                raw_sidecar = (
                    f"sha256sum {stem}.json | awk '{{print $1}}' > {stem}.sha256"
                )
                self.assertIn(raw_sidecar, workflow)
                self.assertNotIn(f"sha256sum {stem}.json > {stem}.sha256", workflow)
                self.assertNotIn(f"sha256sum -c {stem}.sha256", workflow)
                self.assertIn("Recheck exact protected main before capability use", workflow)
                self.assertIn("Recheck exact protected main before attestation", workflow)
                self.assertGreaterEqual(workflow.count("git.getRef"), 3)

        apply_workflow = (
            root / ".github/workflows/apply-production-phase.yml"
        ).read_text(encoding="utf-8")
        self.assertIn(
            'production-phase-state-{}-{}".format(pred["run_id"],pred["run_attempt"])',
            apply_workflow,
        )
        self.assertNotIn(
            'production-phase-state-{}".format(pred["phase"])',
            apply_workflow,
        )

        rollback_workflow = (
            root / ".github/workflows/rollback-production-phase.yml"
        ).read_text(encoding="utf-8")
        self.assertIn("PINNED_GH_VERSION: \"2.98.0\"", rollback_workflow)
        self.assertIn("PINNED_JQ_VERSION: \"1.8.2\"", rollback_workflow)
        self.assertIn("echo \"$PINNED_GH_SHA256  $RUNNER_TEMP/gh.tar.gz\" | sha256sum -c -", rollback_workflow)
        self.assertIn("echo \"$PINNED_JQ_SHA256  $tools_dir/jq\" | sha256sum -c -", rollback_workflow)
        self.assertIn('[[ "$(command -v gh)" == "$RUNNER_TEMP/release-tools/gh" ]]', rollback_workflow)
        self.assertIn('[[ "$(command -v jq)" == "$RUNNER_TEMP/release-tools/jq" ]]', rollback_workflow)


if __name__ == "__main__":
    unittest.main()
