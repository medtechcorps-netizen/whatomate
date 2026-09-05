from __future__ import annotations

import copy
import datetime as dt
import json
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any, Mapping


sys.path.insert(0, str(Path(__file__).resolve().parent))
import finalize_production_orphan_lock as finalizer
import reconcile_production_orphan as reconcile
import test_reconcile_production_orphan as fixtures
import verify_production_release as common


NOW = dt.datetime(2026, 8, 27, 0, 7, 0, tzinfo=dt.timezone.utc)
SCHEMA_PATH = Path(__file__).with_name(
    "production-orphan-lock-finalization.schema.json"
)


def sha(character: str) -> str:
    return character * 64


def attested(
    subject: Mapping[str, Any],
    *,
    workflow_path: str,
    predicate: str,
    artifact_name: str,
    artifact_id: str,
) -> dict[str, Any]:
    control = subject["control"]
    binding = fixtures.artifact_binding(
        run_id=str(control["run_id"]),
        artifact_id=artifact_id,
        artifact_name=artifact_name,
        file_sha256=common.sha256_bytes(common.canonical_file_bytes(subject)),
        digest_character="e",
    )
    return {
        "binding": binding,
        "signer_workflow": f"{common.REPOSITORY}/{workflow_path}",
        "signer_digest": control["workflow_sha"],
        "source_digest": control["workflow_sha"],
        "source_ref": "refs/heads/main",
        "runner_environment": "github-hosted",
        "provenance_predicate_type": finalizer.PROVENANCE_PREDICATE_TYPE,
        "policy_predicate_type": predicate,
        "provenance_verification_sha256": sha("a"),
        "policy_verification_sha256": sha("b"),
    }


def control() -> dict[str, Any]:
    return finalizer._control(
        workflow_sha=fixtures.RECONCILE_CONTROL_SHA,
        workflow_run_id="501",
        workflow_run_attempt=1,
        policy_sha256=sha("c"),
        change_schema_sha256=sha("d"),
        intent_schema_sha256=sha("e"),
        reconciliation_schema_sha256=sha("6"),
        finalization_schema_sha256=sha("8"),
        controller_sha256=sha("9"),
    )


def chain(
    outcome: str, *, timestamp_only_mismatch: bool = False
) -> dict[str, Any]:
    intent = fixtures.valid_intent()
    intent["lock"]["root_acquire_intent"]["artifact_name"] = (
        "production-main-lock-apply-101-1"
    )
    common.validate_mutation_intent(intent)
    provider_job = None
    public = fixtures.before_state()
    migration = False
    original = None
    original_binding = None
    if outcome in {"committed", "already-receipted"}:
        provider_job = fixtures.lock_assertion_request(intent)["original_provider_job"]
        provider_job["conclusion"] = "success"
        provider_job["steps"] = [
            {
                "number": 1,
                "name": "Apply with one isolated app-update capability",
                "status": "completed",
                "conclusion": "success",
            }
        ]
        public = fixtures.desired_public_state()
        migration = True
    elif timestamp_only_mismatch:
        public["app_updated_at_sha256"] = sha("a")
    assertion = fixtures.valid_lock_assertion(intent, provider_job=provider_job)
    intent_authority = fixtures.intent_binding(intent)
    assertion_binding = fixtures.lock_assertion_binding(assertion)
    if outcome == "already-receipted":
        original, original_binding = fixtures.original_apply_receipt(intent, public)
    receipt = reconcile.build_reconciliation_receipt(
        control=fixtures.reconciliation_control(),
        intent=intent,
        intent_binding=intent_authority,
        lock_assertion=assertion,
        lock_assertion_binding=assertion_binding,
        observed=fixtures.observed(public, migration_succeeded=migration),
        request_log=fixtures.REQUEST_LOG,
        original_receipt=original,
        original_receipt_binding=original_binding,
        completed_at=fixtures.NOW,
    )
    self_outcome = receipt["classification"]["outcome"]
    if self_outcome != outcome:
        raise AssertionError((self_outcome, outcome))
    phase = None
    phase_authority = None
    if outcome in {"committed", "already-receipted"}:
        receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
        phase = common.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256=sha("f"),
            control={
                "workflow_sha": fixtures.RECONCILE_CONTROL_SHA,
                "workflow_path": finalizer.CANARY_WORKFLOW_PATH,
                "run_id": "401",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": sha("c"),
                "change_schema_sha256": sha("d"),
            },
            completed_at="2026-08-27T00:06:00Z",
        )
        phase_authority = attested(
            phase,
            workflow_path=finalizer.CANARY_WORKFLOW_PATH,
            predicate=finalizer.CANARY_PREDICATE_TYPE,
            artifact_name="production-phase-state-401-1",
            artifact_id="704",
        )
    intent_attested = attested(
        intent,
        workflow_path=intent["control"]["workflow_path"],
        predicate=finalizer.INTENT_PREDICATE_TYPE,
        artifact_name="production-mutation-intent-apply-101-1",
        artifact_id="701",
    )
    intent_attested["binding"] = copy.deepcopy(intent_authority)
    assertion_attested = attested(
        assertion,
        workflow_path=reconcile.WORKFLOW_PATH,
        predicate=finalizer.ASSERTION_PREDICATE_TYPE,
        artifact_name="production-main-lock-assertion-201-1",
        artifact_id="702",
    )
    assertion_attested["binding"] = copy.deepcopy(assertion_binding)
    reconciliation_attested = attested(
        receipt,
        workflow_path=reconcile.WORKFLOW_PATH,
        predicate=finalizer.RECONCILIATION_PREDICATE_TYPE,
        artifact_name="production-orphan-reconciliation-201-1",
        artifact_id="703",
    )
    closure = (
        finalizer.CLOSURE_NO_MUTATION
        if outcome == "no-mutation"
        else finalizer.CLOSURE_RECONCILIATION
    )
    receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
    phase_hash = (
        common.sha256_bytes(common.canonical_file_bytes(phase))
        if phase is not None else None
    )
    request = finalizer._expected_break_glass_request(
        control=control(),
        intent=intent,
        reconciliation=receipt,
        closure_kind=closure,
        closure_receipt_sha256=receipt_hash,
        orphan_rollback_sha256=None,
        phase_state_sha256=phase_hash,
    )
    return {
        "request": request,
        "control": control(),
        "mutation_intent": intent,
        "mutation_intent_authority": intent_attested,
        "lock_assertion": assertion,
        "lock_assertion_authority": assertion_attested,
        "reconciliation": receipt,
        "reconciliation_authority": reconciliation_attested,
        "orphan_rollback_intent": None,
        "orphan_rollback_intent_authority": None,
        "orphan_rollback": None,
        "orphan_rollback_authority": None,
        "phase_state": phase,
        "phase_state_authority": phase_authority,
        "now": NOW,
    }


def orphan_rollback_chain() -> dict[str, Any]:
    arguments = chain("committed")
    reconciliation = arguments["reconciliation"]
    reconciliation_authority = arguments["reconciliation_authority"]
    original_intent = arguments["mutation_intent"]
    reconciliation_hash = common.sha256_bytes(
        common.canonical_file_bytes(reconciliation)
    )
    current = reconciliation["after"]
    desired = fixtures.desired_projection()
    desired["canonical_spec_sha256"] = sha("0")
    desired["images"] = fixtures.image_records("1")
    desired["migration_digest"] = fixtures.digest("1")
    current_binding = copy.deepcopy(reconciliation_authority["binding"])
    inherited = copy.deepcopy(original_intent["lock"])
    inherited["strategy"] = "inherit"
    inherited["expected_pre_lock"]["lock_branch"] = True
    inherited["owner_intent_sha256"] = reconciliation["intent"]["binding"]["sha256"]
    before_hash = common.sha256_value(current)
    desired_hash = common.sha256_value(desired)
    orphan_intent = {
        "schema_version": 2,
        "authority": "production-mutation-intent",
        "repository": common.REPOSITORY,
        "prepared_at": "2026-08-27T00:05:10Z",
        "expires_at": "2026-08-27T00:15:10Z",
        "control": {
            "workflow_sha": fixtures.RECONCILE_CONTROL_SHA,
            "workflow_path": ".github/workflows/rollback-production-orphan.yml",
            "run_id": "301",
            "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": sha("c"),
            "change_schema_sha256": sha("d"),
            "mutation_intent_schema_sha256": sha("e"),
            "controller_sha256": sha("f"),
        },
        "operation": "rollback",
        "lineage": {
            "event_sequence": 2,
            "phase_ordinal": 1,
            "operation": "rollback",
            "from": "bridge",
            "to": "baseline",
            "predecessor_kind": "reconciliation-receipt",
            "predecessor_state_sha256": reconciliation_hash,
            "phase": "baseline",
            "phase_source_sha": "1" * 40,
        },
        "authorities": {
            "rollout_plan_sha256": original_intent["authorities"]["rollout_plan_sha256"],
            "current_state": {"kind": "reconciliation-receipt", **current_binding},
            "target_state": fixtures.artifact_binding(
                run_id="88", artifact_id="188",
                artifact_name="production-phase-state-88-1",
                file_sha256=sha("2"), digest_character="2",
            ),
            "recovery": copy.deepcopy(original_intent["authorities"]["recovery"]),
            "target_authority": {"production_plan_sha256": sha("2")},
        },
        "lock": inherited,
        "before": copy.deepcopy(current),
        "desired": desired,
        "mutation": {
            "http_method": "PUT",
            "endpoint_label": "app",
            "update_all_source_versions": False,
            "before_sha256": before_hash,
            "desired_sha256": desired_hash,
            "mutation_fingerprint_sha256": common.sha256_value({
                "before_sha256": before_hash,
                "desired_sha256": desired_hash,
                "http_method": "PUT",
                "endpoint_label": "app",
                "update_all_source_versions": False,
            }),
        },
        "rollback": copy.deepcopy(common.ROLLBACK_FLOORS["baseline"]),
        "canary": {
            "required": True,
            "completed": False,
            "endpoint_labels": copy.deepcopy(reconciliation["canary"]["endpoint_labels"]),
            "route_contract_sha256": reconciliation["canary"]["route_contract_sha256"],
        },
    }
    # The fixture reconciliation lands baseline.  Reframe only its public lineage
    # as bridge so the rollback transition is valid while retaining exact hashes.
    reconciliation["lineage"].update({
        "event_sequence": 2, "phase_ordinal": 2, "from": "baseline",
        "to": "bridge", "phase": "bridge",
        "predecessor_kind": "phase-state", "predecessor_state_sha256": sha("3"),
    })
    original_intent["lineage"] = copy.deepcopy(reconciliation["lineage"])
    original_intent["authorities"]["predecessor_state"] = {
        "kind": "phase-state", **fixtures.artifact_binding(
            run_id="77", artifact_id="177",
            artifact_name="production-phase-state-77-1",
            file_sha256=sha("3"), digest_character="3",
        )
    }
    original_intent["rollback"] = copy.deepcopy(common.ROLLBACK_FLOORS["bridge"])
    reconciliation["rollback"] = copy.deepcopy(common.ROLLBACK_FLOORS["bridge"])
    arguments["mutation_intent"] = common.validate_mutation_intent(original_intent)
    intent_hash = common.sha256_bytes(common.canonical_file_bytes(original_intent))
    reconciliation["intent"]["binding"]["sha256"] = intent_hash
    arguments["mutation_intent_authority"]["binding"]["sha256"] = intent_hash
    arguments["lock_assertion"]["mutation_intent_sha256"] = intent_hash
    reconciliation["lock_assertion"]["mutation_intent_sha256"] = intent_hash
    reconciliation["authorities"]["upstream"] = copy.deepcopy(
        original_intent["authorities"]
    )
    assertion_hash = common.sha256_bytes(
        common.canonical_file_bytes(arguments["lock_assertion"])
    )
    arguments["lock_assertion_authority"]["binding"]["sha256"] = assertion_hash
    reconciliation["lock_assertion"]["binding"] = copy.deepcopy(
        arguments["lock_assertion_authority"]["binding"]
    )
    # Rebuild dependent hashes after the test-only lineage reframing.
    reconcile.validate_lock_assertion(
        arguments["lock_assertion"], intent=original_intent
    )
    common.validate_reconciliation_receipt(reconciliation)
    reconciliation_hash = common.sha256_bytes(common.canonical_file_bytes(reconciliation))
    arguments["reconciliation_authority"]["binding"]["sha256"] = reconciliation_hash
    orphan_intent["lineage"]["predecessor_state_sha256"] = reconciliation_hash
    orphan_intent["authorities"]["current_state"]["sha256"] = reconciliation_hash
    orphan_intent["lock"]["owner_intent_sha256"] = reconciliation["intent"]["binding"]["sha256"]
    common.validate_mutation_intent(orphan_intent)
    orphan_intent_authority = attested(
        orphan_intent,
        workflow_path=".github/workflows/rollback-production-orphan.yml",
        predicate=finalizer.INTENT_PREDICATE_TYPE,
        artifact_name="production-mutation-intent-orphan-rollback-301-1",
        artifact_id="801",
    )
    receipt_current = {
        key: value for key, value in orphan_intent["authorities"]["current_state"].items()
        if key != "artifact_name"
    }
    after = copy.deepcopy(current)
    after.update({
        "app_updated_at_sha256": sha("4"),
        "active_deployment_identity_sha256": sha("5"),
        "canonical_spec_sha256": desired["canonical_spec_sha256"],
        "source_mode": "digest-images",
        "images": copy.deepcopy(desired["images"]),
    })
    orphan_receipt = {
        "schema_version": 1,
        "authority": "production-orphan-rollback-receipt",
        "repository": common.REPOSITORY,
        "completed_at": "2026-08-27T00:06:00Z",
        "control": {
            "workflow_sha": fixtures.RECONCILE_CONTROL_SHA,
            "workflow_path": ".github/workflows/rollback-production-orphan.yml",
            "run_id": "301", "run_attempt": 1,
            "runner_environment": "github-hosted",
            "release_policy_sha256": sha("c"),
            "change_schema_sha256": sha("d"),
            "controller_sha256": sha("f"),
        },
        "lineage": copy.deepcopy(orphan_intent["lineage"]),
        "authorities": {
            "rollout_plan_sha256": orphan_intent["authorities"]["rollout_plan_sha256"],
            "current_state": receipt_current,
            "target_state": {key: value for key, value in orphan_intent["authorities"]["target_state"].items() if key != "artifact_name"},
            "recovery": {key: value for key, value in orphan_intent["authorities"]["recovery"].items() if key != "artifact_name"},
            "mutation_intent": copy.deepcopy(orphan_intent_authority["binding"]),
            "main_lock_proof": fixtures.artifact_binding(
                run_id="301", artifact_id="802",
                artifact_name="production-main-lock-proof-orphan-rollback-301-1",
                file_sha256=sha("7"), digest_character="7",
            ),
        },
        "provider_transition": {
            "http_methods_used": ["GET", "PUT"], "http_request_count": 11,
            "mutation_request_count": 1, "endpoint_labels": ["app", "deployment"],
            "mutation_fingerprint_sha256": orphan_intent["mutation"]["mutation_fingerprint_sha256"],
            "ambiguous_reconciled": False,
        },
        "before": copy.deepcopy(orphan_intent["before"]),
        "after": after,
        "target_authority": {"production_plan_sha256": sha("2")},
        "gates": {"deployment_succeeded": True, "migration_succeeded": True},
        "rollback": copy.deepcopy(common.ROLLBACK_FLOORS["baseline"]),
        "canary": {
            "required": True, "completed": False,
            "endpoint_labels": copy.deepcopy(reconciliation["canary"]["endpoint_labels"]),
            "route_contract_sha256": reconciliation["canary"]["route_contract_sha256"],
        },
    }
    rollback = __import__("rollback_production_change")
    rollback.validate_orphan_rollback_receipt(orphan_receipt)
    orphan_receipt_authority = attested(
        orphan_receipt,
        workflow_path=".github/workflows/rollback-production-orphan.yml",
        predicate=finalizer.ORPHAN_ROLLBACK_PREDICATE_TYPE,
        artifact_name="production-orphan-rollback-301-1",
        artifact_id="803",
    )
    orphan_hash = common.sha256_bytes(common.canonical_file_bytes(orphan_receipt))
    phase = common.build_phase_state(
        orphan_receipt, change_receipt_sha256=orphan_hash, canary_sha256=sha("8"),
        control={
            "workflow_sha": fixtures.RECONCILE_CONTROL_SHA,
            "workflow_path": finalizer.CANARY_WORKFLOW_PATH,
            "run_id": "402", "run_attempt": 1, "runner_environment": "github-hosted",
            "release_policy_sha256": sha("c"), "change_schema_sha256": sha("d"),
        }, completed_at="2026-08-27T00:06:30Z",
    )
    phase_authority = attested(
        phase, workflow_path=finalizer.CANARY_WORKFLOW_PATH,
        predicate=finalizer.CANARY_PREDICATE_TYPE,
        artifact_name="production-phase-state-402-1", artifact_id="804",
    )
    arguments.update({
        "orphan_rollback_intent": orphan_intent,
        "orphan_rollback_intent_authority": orphan_intent_authority,
        "orphan_rollback": orphan_receipt,
        "orphan_rollback_authority": orphan_receipt_authority,
        "phase_state": phase,
        "phase_state_authority": phase_authority,
    })
    phase_hash = common.sha256_bytes(common.canonical_file_bytes(phase))
    arguments["request"] = finalizer._expected_break_glass_request(
        control=arguments["control"], intent=original_intent,
        reconciliation=reconciliation,
        closure_kind=finalizer.CLOSURE_ORPHAN_ROLLBACK,
        closure_receipt_sha256=orphan_hash,
        orphan_rollback_sha256=orphan_hash,
        phase_state_sha256=phase_hash,
    )
    return arguments


class FinalizeProductionOrphanLockTests(unittest.TestCase):
    def test_workflow_competing_inventory_includes_fixture_controls_at_every_boundary(self) -> None:
        workflows = Path(__file__).resolve().parents[2] / ".github" / "workflows"
        source = (workflows / "finalize-production-orphan-lock.yml").read_text(
            encoding="utf-8"
        )
        names: set[str] = set()
        paths: set[str] = set()
        for path in workflows.glob("*.yml"):
            content = path.read_text(encoding="utf-8")
            if not re.search(r"(?m)^  group: rereply-production$", content):
                continue
            name = re.fullmatch(r"name: ([^\r\n]+)", content.splitlines()[0])
            self.assertIsNotNone(name)
            names.add(name.group(1))
            paths.add(f".github/workflows/{path.name}")
        additions = {
            "Provision Production CRM Canary Fixture":
                ".github/workflows/provision-production-crm-canary-fixture.yml",
            "Cleanup Production CRM Canary Fixture":
                ".github/workflows/cleanup-production-crm-canary-fixture.yml",
        }
        self.assertTrue(set(additions).issubset(names))
        self.assertTrue(set(additions.values()).issubset(paths))

        def require_exact_inventories(candidate: str) -> None:
            active = "\n".join(
                line for line in candidate.splitlines()
                if not line.lstrip().startswith("#")
            )
            for field, expected, copies in (("name", names, 1), ("path", paths, 2)):
                blocks = re.findall(
                    rf"\(\.{field} \| IN\(\s*(.*?)\s*\) \| not\)",
                    active,
                    re.DOTALL,
                )
                self.assertEqual(len(blocks), copies)
                for block in blocks:
                    inventory = json.loads("[" + block + "]")
                    self.assertEqual(len(inventory), len(set(inventory)))
                    self.assertEqual(set(inventory), expected)

        require_exact_inventories(source)
        for name, path in additions.items():
            for value, copies in ((name, 1), (path, 2)):
                needle = f'"{value}",'
                positions = [match.start() for match in re.finditer(re.escape(needle), source)]
                self.assertEqual(len(positions), copies)
                for position in positions:
                    prefix, suffix = source[:position], source[position + len(needle):]
                    mutants = (
                        prefix + suffix,
                        prefix + "# " + needle + suffix,
                        prefix + needle + " " + needle + suffix,
                    )
                    for mutant in mutants:
                        self.assertNotEqual(mutant, source)
                        with self.subTest(value=value, boundary=position):
                            with self.assertRaises((AssertionError, json.JSONDecodeError)):
                                require_exact_inventories(mutant)

    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))

    def test_isolated_entrypoint_and_frozen_cli(self) -> None:
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(Path(finalizer.__file__).resolve()), "prepare", "--help"],
            check=False, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for flag in (
            "--request", "--mutation-intent", "--mutation-intent-authority",
            "--lock-assertion", "--reconciliation", "--phase-state",
            "--orphan-rollback-intent", "--finalization-schema", "--output",
        ):
            self.assertIn(flag, result.stdout)

    def test_no_mutation_builds_pre_release_only_and_validates_schema(self) -> None:
        authorization = finalizer.build_finalization_authorization(**chain("no-mutation"))
        self.assertEqual(
            authorization["resolution"]["closure_kind"],
            finalizer.CLOSURE_NO_MUTATION,
        )
        self.assertTrue(authorization["resolution"]["provider_job_never_started"])
        self.assertFalse(authorization["branch_action"]["release_performed"])
        self.assertEqual(authorization["branch_action"]["branch_mutation_request_count"], 0)
        self.assertIn("root_acquire_intent", authorization["orphan"])
        finalizer.validate_finalization_authorization(authorization, now=NOW)
        self.assertEqual(set(authorization), set(self.schema["required"]))

    def test_timestamp_only_no_mutation_chain_is_exactly_attested(self) -> None:
        arguments = chain("no-mutation", timestamp_only_mismatch=True)
        receipt = arguments["reconciliation"]
        self.assertEqual(receipt["classification"]["outcome"], "no-mutation")
        self.assertEqual(
            {
                key
                for key in receipt["before"]
                if receipt["before"][key] != receipt["after"][key]
            },
            {"app_updated_at_sha256"},
        )
        self.assertEqual(
            arguments["reconciliation_authority"]["binding"]["sha256"],
            common.sha256_bytes(common.canonical_file_bytes(receipt)),
        )
        authorization = finalizer.build_finalization_authorization(**arguments)
        self.assertEqual(
            authorization["resolution"]["closure_kind"],
            finalizer.CLOSURE_NO_MUTATION,
        )
        self.assertTrue(
            authorization["resolution"]["provider_job_never_started"]
        )
        self.assertIsNone(authorization["authorities"]["phase_state"])
        self.assertIsNone(authorization["authorities"]["orphan_rollback"])
        self.assertEqual(
            authorization["resolution"]["closure_receipt_sha256"],
            authorization["resolution"]["reconciliation_sha256"],
        )
        finalizer.validate_finalization_authorization(authorization, now=NOW)

        arguments["reconciliation"]["after"]["app_updated_at_sha256"] = sha("b")
        with self.assertRaises(common.ReleaseError):
            finalizer.build_finalization_authorization(**arguments)

    def test_finalization_expiry_is_exclusive_at_boundary_and_fraction(self) -> None:
        authorization = finalizer.build_finalization_authorization(**chain("no-mutation"))
        expires = common.require_timestamp(
            authorization["expires_at"], "test finalization expiry"
        )
        for checked in (expires, expires + dt.timedelta(microseconds=1)):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    finalizer.validate_finalization_authorization(
                        authorization, now=checked
                    )

    def test_committed_requires_exact_canary_phase_state(self) -> None:
        arguments = chain("committed")
        authorization = finalizer.build_finalization_authorization(**arguments)
        self.assertEqual(
            authorization["resolution"]["closure_kind"],
            finalizer.CLOSURE_RECONCILIATION,
        )
        self.assertTrue(authorization["resolution"]["canary_certified"])
        self.assertEqual(set(authorization), set(self.schema["required"]))
        arguments["phase_state"] = None
        arguments["phase_state_authority"] = None
        with self.assertRaises(common.ReleaseError):
            finalizer.build_finalization_authorization(**arguments)

    def test_orphan_rollback_canary_binds_inherited_root_and_phase(self) -> None:
        arguments = orphan_rollback_chain()
        authorization = finalizer.build_finalization_authorization(**arguments)
        self.assertEqual(
            authorization["resolution"]["closure_kind"],
            finalizer.CLOSURE_ORPHAN_ROLLBACK,
        )
        self.assertEqual(
            authorization["resolution"]["orphan_rollback_sha256"],
            authorization["authorities"]["orphan_rollback"]["binding"]["sha256"],
        )
        self.assertEqual(
            authorization["orphan"]["root_acquire_intent"],
            arguments["mutation_intent"]["lock"]["root_acquire_intent"],
        )
        finalizer.validate_finalization_authorization(authorization, now=NOW)
        self.assertEqual(set(authorization), set(self.schema["required"]))

    def test_schema_objects_are_closed_and_require_every_property(self) -> None:
        def walk(value: object) -> None:
            if type(value) is dict:
                if value.get("type") == "object":
                    self.assertIs(value.get("additionalProperties"), False)
                    self.assertEqual(
                        set(value.get("required", [])),
                        set(value.get("properties", {})),
                    )
                ref = value.get("$ref")
                if ref is not None:
                    self.assertIsInstance(ref, str)
                    self.assertTrue(ref.startswith("#/$defs/"))
                    self.assertIn(ref.removeprefix("#/$defs/"), self.schema["$defs"])
                for child in value.values():
                    walk(child)
            elif type(value) is list:
                for child in value:
                    walk(child)

        walk(self.schema)
        spliced = orphan_rollback_chain()
        spliced["orphan_rollback_intent"]["lock"]["owner_intent_sha256"] = sha("0")
        with self.assertRaises(common.ReleaseError):
            finalizer.build_finalization_authorization(**spliced)

    def test_cross_spliced_authorities_request_and_phase_state_fail(self) -> None:
        mutations = []
        wrong_signer = chain("committed")
        wrong_signer["reconciliation_authority"]["signer_digest"] = "f" * 40
        mutations.append(wrong_signer)
        wrong_request = chain("committed")
        wrong_request["request"]["original_run_id"] = "999"
        mutations.append(wrong_request)
        wrong_phase = chain("committed")
        wrong_phase["phase_state"]["evidence"]["change_receipt_sha256"] = sha("0")
        mutations.append(wrong_phase)
        wrong_root = chain("committed")
        wrong_root["mutation_intent"]["lock"]["root_acquire_intent"]["artifact_name"] = "unrelated-101-1"
        mutations.append(wrong_root)
        for arguments in mutations:
            with self.subTest(index=mutations.index(arguments)):
                with self.assertRaises(common.ReleaseError):
                    finalizer.build_finalization_authorization(**arguments)

    def test_v1_pending_indeterminate_and_fabricated_counters_fail(self) -> None:
        legacy = chain("no-mutation")
        legacy["reconciliation"]["intent"]["schema_version"] = 1
        with self.assertRaises(common.ReleaseError):
            finalizer.build_finalization_authorization(**legacy)
        for outcome in ("pending", "indeterminate"):
            arguments = chain("no-mutation")
            semantics = copy.deepcopy(common.RECONCILIATION_OUTCOMES[outcome])
            semantics.setdefault("original_receipt_present", False)
            arguments["reconciliation"]["classification"] = {
                "outcome": outcome, **semantics
            }
            with self.assertRaises(common.ReleaseError):
                finalizer.build_finalization_authorization(**arguments)
        counters = chain("no-mutation")
        counters["reconciliation"]["provider_observation"]["http_request_count"] = 5
        with self.assertRaises(common.ReleaseError):
            finalizer.build_finalization_authorization(**counters)

    def test_validator_rejects_claimed_release_and_extra_keys(self) -> None:
        authorization = finalizer.build_finalization_authorization(**chain("no-mutation"))
        claimed = copy.deepcopy(authorization)
        claimed["branch_action"]["release_performed"] = True
        with self.assertRaises(common.ReleaseError):
            finalizer.validate_finalization_authorization(claimed)
        extra = copy.deepcopy(authorization)
        extra["unlock_success"] = True
        with self.assertRaises(common.ReleaseError):
            finalizer.validate_finalization_authorization(extra)
        authority_mutations = (
            ("mutation_intent", "policy_predicate_type", "https://invalid.example/predicate"),
            ("lock_assertion", "signer_digest", "f" * 40),
            ("reconciliation", "source_ref", "refs/heads/other"),
        )
        for key, field, replacement in authority_mutations:
            tampered = copy.deepcopy(authorization)
            tampered["authorities"][key][field] = replacement
            with self.subTest(authority=key, field=field):
                with self.assertRaises(common.ReleaseError):
                    finalizer.validate_finalization_authorization(tampered)
        wrong_hash = copy.deepcopy(authorization)
        wrong_hash["resolution"]["reconciliation_sha256"] = sha("0")
        wrong_hash["resolution"]["closure_receipt_sha256"] = sha("0")
        with self.assertRaises(common.ReleaseError):
            finalizer.validate_finalization_authorization(wrong_hash)

    def test_validate_cli_requires_exact_canonical_file_hash(self) -> None:
        authorization = finalizer.build_finalization_authorization(**chain("no-mutation"))
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "authorization.json"
            path.write_bytes(common.canonical_file_bytes(authorization))
            exact_hash = common.sha256_bytes(path.read_bytes())
            self.assertEqual(
                finalizer.run_cli(["validate", "--authorization", str(path), "--sha256", exact_hash]),
                0,
            )
            with self.assertRaises(common.ReleaseError):
                finalizer.run_cli(["validate", "--authorization", str(path), "--sha256", sha("0")])

    def test_controller_source_has_no_network_or_token_capability(self) -> None:
        source = Path(finalizer.__file__).read_text(encoding="utf-8").lower()
        for forbidden in (
            "urllib", "requests.", "http.client", "digitalocean", "put_app",
            "do_production", "branch_read_token", "admin_api(",
        ):
            self.assertNotIn(forbidden, source)


if __name__ == "__main__":
    unittest.main()
