from __future__ import annotations

import json
import unittest
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent
INTENT_PATH = ROOT / "production-mutation-intent.schema.json"
RECONCILIATION_PATH = ROOT / "production-orphan-reconciliation.schema.json"
LOCK_ASSERTION_PATH = ROOT / "production-main-lock-assertion.schema.json"

INTENT_TOP_KEYS = {
    "schema_version",
    "authority",
    "repository",
    "prepared_at",
    "expires_at",
    "control",
    "operation",
    "lineage",
    "authorities",
    "lock",
    "before",
    "desired",
    "mutation",
    "rollback",
    "canary",
}
RECONCILIATION_TOP_KEYS = {
    "schema_version",
    "authority",
    "repository",
    "completed_at",
    "control",
    "intent",
    "lock_assertion",
    "lineage",
    "authorities",
    "classification",
    "provider_observation",
    "before",
    "desired",
    "after",
    "gates",
    "rollback",
    "canary",
}
LOCK_ASSERTION_TOP_KEYS = {
    "schema_version",
    "authority",
    "repository",
    "created_at",
    "control",
    "actor_provenance",
    "original_workflow_path",
    "original_control_sha",
    "original_run_id",
    "original_run_attempt",
    "rule_id",
    "rule_identity_sha256",
    "current_main_sha",
    "mutation_intent_sha256",
    "typed_confirmation_sha256",
    "original_provider_job",
}
ARTIFACT_KEYS = {
    "run_id",
    "run_attempt",
    "artifact_id",
    "artifact_name",
    "artifact_digest",
    "sha256",
}
PROVIDER_STATE_KEYS = {
    "app_identity_sha256",
    "default_ingress_sha256",
    "app_updated_at_sha256",
    "active_deployment_identity_sha256",
    "canonical_spec_sha256",
    "environment_values_sha256",
    "non_source_projection_sha256",
    "source_mode",
    "images",
}
DESIRED_KEYS = {
    "canonical_spec_sha256",
    "environment_values_sha256",
    "non_source_projection_sha256",
    "source_mode",
    "images",
    "migration_job",
    "migration_digest",
}
LINEAGE_KEYS = {
    "event_sequence",
    "phase_ordinal",
    "operation",
    "from",
    "to",
    "predecessor_kind",
    "predecessor_state_sha256",
    "phase",
    "phase_source_sha",
}
APPLY_AUTHORITY_KEYS = {
    "rollout_plan_sha256",
    "rollout_authority",
    "production_plan",
    "recovery",
    "predecessor_state",
}
ROLLBACK_AUTHORITY_KEYS = {
    "rollout_plan_sha256",
    "current_state",
    "target_state",
    "recovery",
    "target_authority",
}


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def reject_float(_value: str) -> Any:
    raise ValueError("floating point JSON is forbidden")


def reject_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON is forbidden: {value}")


def load_schema(path: Path) -> dict[str, Any]:
    value = json.loads(
        path.read_text(encoding="utf-8"),
        object_pairs_hook=reject_duplicate_keys,
        parse_float=reject_float,
        parse_constant=reject_constant,
    )
    if type(value) is not dict:
        raise AssertionError(f"{path.name} is not a JSON object")
    return value


def walk(value: Any, path: str = "$") -> list[tuple[str, Any]]:
    output = [(path, value)]
    if type(value) is dict:
        for key, child in value.items():
            output.extend(walk(child, f"{path}.{key}"))
    elif type(value) is list:
        for index, child in enumerate(value):
            output.extend(walk(child, f"{path}[{index}]"))
    return output


def properties(schema: dict[str, Any], definition: str) -> set[str]:
    return set(schema["$defs"][definition]["properties"])


def required(schema: dict[str, Any], definition: str) -> set[str]:
    return set(schema["$defs"][definition]["required"])


def local_ref_target(schema: dict[str, Any], ref: str) -> Any:
    if not ref.startswith("#/"):
        raise AssertionError(f"external JSON Schema reference is forbidden: {ref}")
    value: Any = schema
    for raw in ref[2:].split("/"):
        key = raw.replace("~1", "/").replace("~0", "~")
        if type(value) is not dict or key not in value:
            raise AssertionError(f"unresolved JSON Schema reference: {ref}")
        value = value[key]
    return value


class SharedStrictSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.intent = load_schema(INTENT_PATH)
        cls.reconciliation = load_schema(RECONCILIATION_PATH)
        cls.lock_assertion = load_schema(LOCK_ASSERTION_PATH)

    def test_schema_dialect_and_local_references_are_closed(self) -> None:
        for schema in (self.intent, self.reconciliation, self.lock_assertion):
            self.assertEqual(
                schema["$schema"], "https://json-schema.org/draft/2020-12/schema"
            )
            for path, value in walk(schema):
                if type(value) is dict and "$ref" in value:
                    with self.subTest(schema=schema["$id"], path=path):
                        local_ref_target(schema, value["$ref"])

    def test_every_declared_object_is_closed(self) -> None:
        for schema in (self.intent, self.reconciliation, self.lock_assertion):
            for path, value in walk(schema):
                if type(value) is dict and value.get("type") == "object":
                    with self.subTest(schema=schema["$id"], path=path):
                        self.assertIs(value.get("additionalProperties"), False)
                        self.assertEqual(
                            set(value.get("required", [])),
                            set(value.get("properties", {})),
                        )

    def test_no_float_schema_or_floating_literal_exists(self) -> None:
        for schema in (self.intent, self.reconciliation, self.lock_assertion):
            for path, value in walk(schema):
                with self.subTest(schema=schema["$id"], path=path):
                    self.assertIsNot(type(value), float)
                    if type(value) is dict and "type" in value:
                        declared = value["type"]
                        self.assertNotEqual(declared, "number")
                        if type(declared) is list:
                            self.assertNotIn("number", declared)

    def test_public_property_names_exclude_secret_and_raw_provider_fields(self) -> None:
        forbidden = {
            "access_token",
            "authorization",
            "credential",
            "credentials",
            "deployment_id",
            "environment",
            "envs",
            "password",
            "private_key",
            "provider_app_id",
            "raw_spec",
            "secret",
            "spec",
            "token",
            "unlock",
            "unlock_request_count",
            "update_payload",
        }
        for schema in (self.intent, self.reconciliation, self.lock_assertion):
            public_names: set[str] = set()
            for _path, value in walk(schema):
                if type(value) is dict and type(value.get("properties")) is dict:
                    public_names.update(value["properties"])
            self.assertFalse(
                public_names.intersection(forbidden),
                f"unsafe public fields in {schema['$id']}",
            )

    def test_shared_sanitized_definitions_are_identical(self) -> None:
        shared = {
            "sha1",
            "sha256",
            "digest",
            "runId",
            "attempt",
            "timestamp",
            "phase",
            "state",
            "imageWeb",
            "imageMetaRelay",
            "imageGmailRelay",
            "digestImages",
            "providerState",
            "desiredProjection",
            "artifactBinding",
            "predecessorBinding",
            "applyAuthorities",
            "targetAuthority",
            "rollbackAuthorities",
            "lineage",
            "rollback",
        }
        for name in shared:
            with self.subTest(definition=name):
                self.assertEqual(
                    self.intent["$defs"][name],
                    self.reconciliation["$defs"][name],
                )

    def test_sanitized_state_and_artifact_bindings_are_exact(self) -> None:
        for schema in (self.intent, self.reconciliation):
            self.assertEqual(properties(schema, "artifactBinding"), ARTIFACT_KEYS)
            self.assertEqual(required(schema, "artifactBinding"), ARTIFACT_KEYS)
            self.assertEqual(properties(schema, "providerState"), PROVIDER_STATE_KEYS)
            self.assertEqual(required(schema, "providerState"), PROVIDER_STATE_KEYS)
            self.assertEqual(properties(schema, "desiredProjection"), DESIRED_KEYS)
            self.assertEqual(required(schema, "desiredProjection"), DESIRED_KEYS)
            images = schema["$defs"]["digestImages"]
            self.assertEqual(images["minItems"], 3)
            self.assertEqual(images["maxItems"], 3)
            self.assertIs(images["items"], False)
            self.assertEqual(
                images["prefixItems"],
                [
                    {"$ref": "#/$defs/imageWeb"},
                    {"$ref": "#/$defs/imageMetaRelay"},
                    {"$ref": "#/$defs/imageGmailRelay"},
                ],
            )
            desired = schema["$defs"]["desiredProjection"]["properties"]
            self.assertEqual(desired["source_mode"], {"const": "digest-images"})
            self.assertEqual(desired["migration_job"], {"const": "rereply-rls-migrate"})
            self.assertEqual(desired["migration_digest"], {"$ref": "#/$defs/digest"})


class MutationIntentSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = load_schema(INTENT_PATH)

    def test_identity_and_exact_root_contract(self) -> None:
        self.assertEqual(
            self.schema["$id"],
            "https://rereply.app/schemas/production-mutation-intent-v2.json",
        )
        self.assertEqual(
            self.schema["title"], "ReReply durable production mutation intent"
        )
        self.assertEqual(set(self.schema["required"]), INTENT_TOP_KEYS)
        self.assertEqual(set(self.schema["properties"]), INTENT_TOP_KEYS)
        self.assertEqual(self.schema["properties"]["schema_version"], {"const": 2})
        self.assertEqual(
            self.schema["properties"]["authority"],
            {"const": "production-mutation-intent"},
        )
        self.assertEqual(
            self.schema["properties"]["repository"],
            {"const": "medtechcorps-netizen/whatomate"},
        )

    def test_control_lineage_and_authorities_match_runtime_validator(self) -> None:
        control_keys = {
            "workflow_sha",
            "workflow_path",
            "run_id",
            "run_attempt",
            "runner_environment",
            "release_policy_sha256",
            "change_schema_sha256",
            "mutation_intent_schema_sha256",
            "controller_sha256",
        }
        lock_keys = {
            "mode",
            "strategy",
            "branch",
            "rule_id",
            "rule_identity_sha256",
            "expected_pre_lock",
            "expected_post_lock",
            "root_acquire_intent",
            "owner_operation",
            "owner_run_id",
            "owner_run_attempt",
            "owner_control_sha",
            "owner_intent_sha256",
        }
        self.assertEqual(properties(self.schema, "control"), control_keys)
        self.assertEqual(required(self.schema, "control"), control_keys)
        self.assertEqual(properties(self.schema, "lineage"), LINEAGE_KEYS)
        self.assertEqual(required(self.schema, "lineage"), LINEAGE_KEYS)
        self.assertEqual(properties(self.schema, "applyAuthorities"), APPLY_AUTHORITY_KEYS)
        self.assertEqual(required(self.schema, "applyAuthorities"), APPLY_AUTHORITY_KEYS)
        self.assertEqual(
            properties(self.schema, "rollbackAuthorities"), ROLLBACK_AUTHORITY_KEYS
        )
        self.assertEqual(
            required(self.schema, "rollbackAuthorities"), ROLLBACK_AUTHORITY_KEYS
        )
        self.assertEqual(properties(self.schema, "lock"), lock_keys)
        self.assertEqual(required(self.schema, "lock"), lock_keys)

    def test_operation_and_lock_conditions_are_fail_closed(self) -> None:
        serialized = json.dumps(self.schema, sort_keys=True, separators=(",", ":"))
        for required_fragment in (
            '"const":"activate"',
            '"const":"rollback"',
            '"$ref":"#/$defs/applyAuthorities"',
            '"$ref":"#/$defs/rollbackAuthorities"',
            '"const":"planned"',
            '"const":"acquire"',
            '"const":"inherit"',
            '"const":"apply"',
            '.github/workflows/apply-production-phase.yml',
            '.github/workflows/rollback-production-phase.yml',
            '.github/workflows/rollback-production-orphan.yml',
        ):
            self.assertIn(required_fragment, serialized)
        lock = self.schema["$defs"]["lock"]
        self.assertEqual(lock["properties"]["branch"], {"const": "main"})
        self.assertEqual(lock["properties"]["mode"], {"const": "planned"})
        self.assertEqual(
            lock["properties"]["root_acquire_intent"],
            {"$ref": "#/$defs/artifactBinding"},
        )

    def test_mutation_binds_before_and_desired_without_prospective_counters(self) -> None:
        mutation_keys = {
            "http_method",
            "endpoint_label",
            "update_all_source_versions",
            "before_sha256",
            "desired_sha256",
            "mutation_fingerprint_sha256",
        }
        self.assertEqual(properties(self.schema, "mutation"), mutation_keys)
        self.assertEqual(required(self.schema, "mutation"), mutation_keys)
        mutation = self.schema["$defs"]["mutation"]["properties"]
        self.assertEqual(mutation["http_method"], {"const": "PUT"})
        self.assertEqual(mutation["endpoint_label"], {"const": "app"})
        self.assertEqual(
            mutation["update_all_source_versions"], {"const": False}
        )
        all_property_names = {
            key
            for _path, value in walk(self.schema)
            if type(value) is dict and type(value.get("properties")) is dict
            for key in value["properties"]
        }
        self.assertNotIn("http_request_count", all_property_names)
        self.assertNotIn("mutation_request_count", all_property_names)


class MainLockAssertionSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = load_schema(LOCK_ASSERTION_PATH)

    def test_identity_and_exact_root_contract(self) -> None:
        self.assertEqual(
            self.schema["$id"],
            "https://rereply.app/schemas/production-main-lock-assertion-v1.json",
        )
        self.assertEqual(
            self.schema["title"],
            "ReReply production main lock ownership assertion",
        )
        self.assertEqual(set(self.schema["required"]), LOCK_ASSERTION_TOP_KEYS)
        self.assertEqual(set(self.schema["properties"]), LOCK_ASSERTION_TOP_KEYS)
        self.assertEqual(self.schema["properties"]["schema_version"], {"const": 1})
        self.assertEqual(
            self.schema["properties"]["authority"],
            {"const": "production-main-lock-ownership-assertion"},
        )
        self.assertEqual(
            self.schema["properties"]["repository"],
            {"const": "medtechcorps-netizen/whatomate"},
        )

    def test_control_and_original_authority_paths_are_exact(self) -> None:
        control_keys = {"workflow_sha", "workflow_path", "run_id", "run_attempt"}
        self.assertEqual(properties(self.schema, "control"), control_keys)
        self.assertEqual(required(self.schema, "control"), control_keys)
        control = self.schema["$defs"]["control"]["properties"]
        self.assertEqual(
            control["workflow_path"],
            {"const": ".github/workflows/reconcile-production-orphan.yml"},
        )
        self.assertEqual(control["run_attempt"], {"const": 1})
        self.assertEqual(
            self.schema["properties"]["original_workflow_path"]["enum"],
            [
                ".github/workflows/apply-production-phase.yml",
                ".github/workflows/rollback-production-phase.yml",
                ".github/workflows/rollback-production-orphan.yml",
            ],
        )
        self.assertEqual(
            self.schema["properties"]["original_run_attempt"], {"const": 1}
        )

    def test_provenance_and_only_hashed_confirmation_are_public(self) -> None:
        self.assertEqual(
            self.schema["properties"]["actor_provenance"],
            {"const": "single-operator-assertion-not-audit-log"},
        )
        self.assertNotIn("typed_confirmation", self.schema["properties"])
        self.assertEqual(
            self.schema["properties"]["typed_confirmation_sha256"],
            {"$ref": "#/$defs/sha256"},
        )
        self.assertEqual(
            self.schema["properties"]["mutation_intent_sha256"],
            {"$ref": "#/$defs/sha256"},
        )

    def test_provider_job_evidence_is_content_free_and_exact(self) -> None:
        expected = {
            "job_id", "job_name", "status", "conclusion", "timing_sha256",
            "step_inventory_sha256", "step_count", "all_steps_skipped",
            "provider_step_name", "provider_step_status",
            "provider_step_conclusion", "never_started",
        }
        self.assertEqual(properties(self.schema, "originalProviderJob"), expected)
        self.assertEqual(required(self.schema, "originalProviderJob"), expected)
        provider = self.schema["$defs"]["originalProviderJob"]["properties"]
        self.assertEqual(provider["status"], {"const": "completed"})
        for raw_key in ("started_at", "completed_at", "steps", "runner_name"):
            self.assertNotIn(raw_key, provider)

    def test_rule_and_main_bindings_are_sanitized(self) -> None:
        rule = self.schema["properties"]["rule_id"]
        self.assertEqual(rule["pattern"], "^[A-Za-z0-9_+/=-]+$")
        self.assertEqual(rule["maxLength"], 512)
        self.assertEqual(
            self.schema["properties"]["rule_identity_sha256"],
            {"$ref": "#/$defs/sha256"},
        )
        self.assertEqual(
            self.schema["properties"]["current_main_sha"],
            {"$ref": "#/$defs/sha1"},
        )


class ReconciliationSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = load_schema(RECONCILIATION_PATH)

    def test_identity_and_exact_root_contract(self) -> None:
        self.assertEqual(
            self.schema["$id"],
            "https://rereply.app/schemas/production-orphan-reconciliation-v1.json",
        )
        self.assertEqual(
            self.schema["title"],
            "ReReply GET-only production orphan reconciliation receipt",
        )
        self.assertEqual(set(self.schema["required"]), RECONCILIATION_TOP_KEYS)
        self.assertEqual(set(self.schema["properties"]), RECONCILIATION_TOP_KEYS)
        self.assertEqual(self.schema["properties"]["schema_version"], {"const": 1})
        self.assertEqual(
            self.schema["properties"]["authority"],
            {"const": "production-orphan-reconciliation-receipt"},
        )

    def test_nested_keysets_match_runtime_validator(self) -> None:
        control_keys = {
            "workflow_sha",
            "workflow_path",
            "run_id",
            "run_attempt",
            "runner_environment",
            "release_policy_sha256",
            "change_schema_sha256",
            "mutation_intent_schema_sha256",
            "reconciliation_schema_sha256",
            "controller_sha256",
        }
        intent_keys = {
            "schema_version", "operation", "workflow_path", "binding", "lock"
        }
        lock_keys = {
            "mode", "strategy", "branch", "rule_id", "rule_identity_sha256",
            "expected_pre_lock", "expected_post_lock",
            "root_acquire_intent", "owner_operation", "owner_run_id",
            "owner_run_attempt", "owner_control_sha", "owner_intent_sha256",
        }
        assertion_keys = {
            "authority",
            "actor_provenance",
            "original_workflow_path",
            "original_control_sha",
            "original_run_id",
            "original_run_attempt",
            "rule_id",
            "rule_identity_sha256",
            "current_main_sha",
            "mutation_intent_sha256",
            "typed_confirmation_sha256",
            "original_provider_job",
            "binding",
        }
        classification_keys = {
            "outcome",
            "terminal",
            "canary_eligible",
            "original_receipt_present",
            "reason",
        }
        observation_keys = {
            "http_methods_used",
            "http_request_count",
            "mutation_request_count",
            "endpoint_labels",
            "observation_rounds",
            "double_read_equal",
            "app_spec_matches_active_deployment",
            "transition_absent",
            "migration_succeeded",
        }
        gates_keys = {
            "artifacts_authenticated",
            "main_unchanged",
            "lock_owned",
            "get_only",
            "double_read_complete",
            "app_spec_matches_active_deployment",
            "deployment_succeeded",
            "migration_succeeded",
        }
        canary_keys = {
            "required",
            "eligible",
            "completed",
            "endpoint_labels",
            "route_contract_sha256",
        }
        for definition, expected in (
            ("control", control_keys),
            ("intentAuthority", intent_keys),
            ("mutationLock", lock_keys),
            ("lockAssertion", assertion_keys),
            ("lineage", LINEAGE_KEYS),
            ("classification", classification_keys),
            ("providerObservation", observation_keys),
            ("gates", gates_keys),
            ("canary", canary_keys),
        ):
            with self.subTest(definition=definition):
                self.assertEqual(properties(self.schema, definition), expected)
                self.assertEqual(required(self.schema, definition), expected)

    def test_all_five_outcomes_have_exact_boolean_semantics(self) -> None:
        outcomes = self.schema["$defs"]["classification"]["properties"]["outcome"][
            "enum"
        ]
        self.assertEqual(
            outcomes,
            [
                "committed",
                "already-receipted",
                "no-mutation",
                "pending",
                "indeterminate",
            ],
        )
        cases = {
            "committedOutcome": (
                True,
                True,
                False,
                "desired-active-without-receipt",
            ),
            "alreadyReceiptedOutcome": (
                True,
                True,
                True,
                "desired-active-with-signed-receipt",
            ),
            "noMutationOutcome": (
                True,
                False,
                False,
                "exact-before-no-provider-transition",
            ),
            "pendingOutcome": (
                False,
                False,
                False,
                "desired-provider-transition-pending",
            ),
            "indeterminateOutcome": (
                False,
                False,
                None,
                "provider-state-indeterminate",
            ),
        }
        for definition, expected in cases.items():
            with self.subTest(definition=definition):
                classification = self.schema["$defs"][definition]["properties"][
                    "classification"
                ]["properties"]
                self.assertEqual(classification["terminal"], {"const": expected[0]})
                self.assertEqual(
                    classification["canary_eligible"], {"const": expected[1]}
                )
                if expected[2] is None:
                    self.assertNotIn("original_receipt_present", classification)
                else:
                    self.assertEqual(
                        classification["original_receipt_present"],
                        {"const": expected[2]},
                    )
                self.assertEqual(classification["reason"], {"const": expected[3]})

        self.assertTrue(
            self.schema["$defs"]["noMutationOutcome"]["properties"]
            ["lock_assertion"]["properties"]["original_provider_job"]
            ["properties"]["never_started"]["const"]
        )

    def test_provider_ledger_is_get_only_and_never_claims_a_mutation(self) -> None:
        observation = self.schema["$defs"]["providerObservation"]["properties"]
        self.assertEqual(observation["http_methods_used"], {"const": ["GET"]})
        self.assertEqual(observation["http_request_count"], {"const": 4})
        self.assertEqual(observation["mutation_request_count"], {"const": 0})
        self.assertEqual(observation["endpoint_labels"], {"const": ["app", "deployment"]})
        self.assertEqual(observation["observation_rounds"], {"const": 2})
        self.assertEqual(observation["double_read_equal"], {"const": True})
        serialized = json.dumps(self.schema, sort_keys=True, separators=(",", ":"))
        self.assertNotIn('"PUT"', serialized)
        self.assertNotIn("unlock", serialized.lower())

    def test_legacy_intent_is_only_eligible_with_a_signed_receipt(self) -> None:
        serialized = json.dumps(self.schema["allOf"], sort_keys=True, separators=(",", ":"))
        self.assertIn('"schema_version":{"const":1}', serialized)
        self.assertIn('"outcome":{"const":"already-receipted"}', serialized)
        original = self.schema["$defs"]["originalReceipt"]
        self.assertEqual(
            original["properties"]["kind"]["enum"],
            ["apply", "rollback", "orphan-rollback"],
        )
        self.assertEqual(
            original["properties"]["binding"],
            {"$ref": "#/$defs/artifactBinding"},
        )

    def test_only_committed_outcomes_are_canary_eligible(self) -> None:
        for definition in ("committedOutcome", "alreadyReceiptedOutcome"):
            canary = self.schema["$defs"][definition]["properties"]["canary"][
                "properties"
            ]
            gates = self.schema["$defs"][definition]["properties"]["gates"][
                "properties"
            ]
            self.assertEqual(canary["required"], {"const": True})
            self.assertEqual(canary["eligible"], {"const": True})
            self.assertEqual(gates["deployment_succeeded"], {"const": True})
            self.assertEqual(gates["migration_succeeded"], {"const": True})
        for definition in ("noMutationOutcome", "pendingOutcome", "indeterminateOutcome"):
            canary = self.schema["$defs"][definition]["properties"]["canary"][
                "properties"
            ]
            gates = self.schema["$defs"][definition]["properties"]["gates"][
                "properties"
            ]
            self.assertEqual(canary["required"], {"const": False})
            self.assertEqual(canary["eligible"], {"const": False})
            self.assertEqual(gates["deployment_succeeded"], {"const": False})
            self.assertEqual(gates["migration_succeeded"], {"const": False})


if __name__ == "__main__":
    unittest.main()
