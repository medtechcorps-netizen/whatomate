from __future__ import annotations

import json
import unittest
from pathlib import Path
from typing import Any


SCHEMA_PATH = Path(__file__).with_name("production-main-lock-proof.schema.json")
TOP_KEYS = {
    "schema_version",
    "authority",
    "repository",
    "created_at",
    "control",
    "operation",
    "mutation_intent",
    "root_acquire_intent",
    "branch",
    "acquisition",
}
ARTIFACT_KEYS = {
    "run_id",
    "run_attempt",
    "artifact_id",
    "artifact_name",
    "artifact_digest",
    "sha256",
}
CONTROL_KEYS = {
    "workflow_sha",
    "workflow_path",
    "run_id",
    "run_attempt",
    "runner_environment",
}
BRANCH_KEYS = {
    "main_sha",
    "rule_id",
    "rule_identity_sha256",
    "strategy",
    "pre_lock",
    "post_lock",
}
PROJECTION_KEYS = {
    "lock_branch",
    "is_admin_enforced",
    "lock_allows_fetch_and_merge",
}
ACQUISITION_KEYS = {
    "http_methods_used",
    "graphql_operations_used",
    "mutation_request_count",
    "outcome",
    "mutation_fingerprint_sha256",
    "read_confirmed",
}


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    output: dict[str, Any] = {}
    for key, value in pairs:
        if key in output:
            raise ValueError(f"duplicate JSON key: {key}")
        output[key] = value
    return output


def reject_float(_value: str) -> Any:
    raise ValueError("floating point JSON is forbidden")


def reject_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON is forbidden: {value}")


def load_schema() -> dict[str, Any]:
    value = json.loads(
        SCHEMA_PATH.read_text(encoding="utf-8"),
        object_pairs_hook=reject_duplicate_keys,
        parse_float=reject_float,
        parse_constant=reject_constant,
    )
    if type(value) is not dict:
        raise AssertionError("lock-proof schema is not an object")
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


def exact_keys(schema: dict[str, Any], definition: str) -> set[str]:
    node = schema["$defs"][definition]
    required = set(node["required"])
    properties = set(node["properties"])
    if required != properties:
        raise AssertionError(f"{definition} does not require every property")
    return properties


class ProductionMainLockProofSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = load_schema()

    def test_identity_and_exact_root_contract(self) -> None:
        self.assertEqual(
            self.schema["$schema"], "https://json-schema.org/draft/2020-12/schema"
        )
        self.assertEqual(
            self.schema["$id"],
            "https://rereply.app/schemas/production-main-lock-proof-v1.json",
        )
        self.assertEqual(self.schema["title"], "ReReply production main lock proof")
        self.assertIs(self.schema["additionalProperties"], False)
        self.assertEqual(set(self.schema["required"]), TOP_KEYS)
        self.assertEqual(set(self.schema["properties"]), TOP_KEYS)
        self.assertEqual(self.schema["properties"]["schema_version"], {"const": 1})
        self.assertEqual(
            self.schema["properties"]["authority"],
            {"const": "production-main-lock-proof"},
        )
        self.assertEqual(
            self.schema["properties"]["repository"],
            {"const": "medtechcorps-netizen/whatomate"},
        )

    def test_all_references_are_local_and_resolve(self) -> None:
        for path, value in walk(self.schema):
            if type(value) is dict and "$ref" in value:
                with self.subTest(path=path):
                    local_ref_target(self.schema, value["$ref"])

    def test_every_declared_object_is_closed_and_exact(self) -> None:
        for path, value in walk(self.schema):
            if type(value) is dict and value.get("type") == "object":
                with self.subTest(path=path):
                    self.assertIs(value.get("additionalProperties"), False)
                    self.assertEqual(
                        set(value.get("required", [])),
                        set(value.get("properties", {})),
                    )

    def test_control_and_operation_workflow_mapping_are_exact(self) -> None:
        self.assertEqual(exact_keys(self.schema, "control"), CONTROL_KEYS)
        control = self.schema["$defs"]["control"]["properties"]
        self.assertEqual(control["run_attempt"], {"const": 1})
        self.assertEqual(control["runner_environment"], {"const": "github-hosted"})
        expected = {
            "apply": (
                ".github/workflows/apply-production-phase.yml",
                "acquire",
            ),
            "rollback": (
                ".github/workflows/rollback-production-phase.yml",
                "acquire",
            ),
            "orphan-rollback": (
                ".github/workflows/rollback-production-orphan.yml",
                "inherit",
            ),
        }
        operation_conditions = self.schema["allOf"][:3]
        observed: dict[str, tuple[str, str]] = {}
        for condition in operation_conditions:
            operation = condition["if"]["properties"]["operation"]["const"]
            then_properties = condition["then"]["properties"]
            observed[operation] = (
                then_properties["control"]["properties"]["workflow_path"]["const"],
                then_properties["branch"]["properties"]["strategy"]["const"],
            )
        self.assertEqual(observed, expected)

    def test_both_authority_inputs_use_full_artifact_bindings(self) -> None:
        self.assertEqual(exact_keys(self.schema, "artifactBinding"), ARTIFACT_KEYS)
        for name in ("mutation_intent", "root_acquire_intent"):
            self.assertEqual(
                self.schema["properties"][name],
                {"$ref": "#/$defs/artifactBinding"},
            )
        binding = self.schema["$defs"]["artifactBinding"]["properties"]
        self.assertEqual(binding["artifact_digest"], {"$ref": "#/$defs/digest"})
        self.assertEqual(binding["sha256"], {"$ref": "#/$defs/sha256"})

    def test_branch_and_lock_projections_are_exact_and_sanitized(self) -> None:
        self.assertEqual(exact_keys(self.schema, "branch"), BRANCH_KEYS)
        for definition in ("lockProjection", "unlockedProjection", "lockedProjection"):
            self.assertEqual(exact_keys(self.schema, definition), PROJECTION_KEYS)
        branch = self.schema["$defs"]["branch"]["properties"]
        self.assertEqual(branch["main_sha"], {"$ref": "#/$defs/sha1"})
        self.assertEqual(branch["rule_identity_sha256"], {"$ref": "#/$defs/sha256"})
        self.assertEqual(branch["strategy"], {"enum": ["acquire", "inherit"]})
        unlocked = self.schema["$defs"]["unlockedProjection"]["properties"]
        locked = self.schema["$defs"]["lockedProjection"]["properties"]
        self.assertEqual(unlocked["lock_branch"], {"const": False})
        self.assertEqual(locked["lock_branch"], {"const": True})
        for projection in (unlocked, locked):
            self.assertEqual(projection["is_admin_enforced"], {"const": True})
            self.assertEqual(
                projection["lock_allows_fetch_and_merge"], {"const": False}
            )

    def test_acquisition_ledger_is_graphql_only_and_read_confirmed(self) -> None:
        self.assertEqual(exact_keys(self.schema, "acquisition"), ACQUISITION_KEYS)
        acquisition = self.schema["$defs"]["acquisition"]["properties"]
        self.assertEqual(acquisition["http_methods_used"], {"const": ["POST"]})
        self.assertEqual(acquisition["read_confirmed"], {"const": True})
        self.assertEqual(
            acquisition["outcome"]["enum"],
            ["applied", "ambiguous-reconciled", "already-locked-inherited"],
        )
        self.assertEqual(
            acquisition["mutation_fingerprint_sha256"],
            {"$ref": "#/$defs/sha256"},
        )

    def test_strategy_conditions_bind_pre_state_ledger_counter_and_outcome(self) -> None:
        strategy_conditions = self.schema["allOf"][3:]
        observed: dict[str, tuple[str, list[str], int, Any]] = {}
        for condition in strategy_conditions:
            strategy = condition["if"]["properties"]["branch"]["properties"][
                "strategy"
            ]["const"]
            then_properties = condition["then"]["properties"]
            acquisition = then_properties["acquisition"]["properties"]
            observed[strategy] = (
                then_properties["branch"]["properties"]["pre_lock"]["$ref"],
                acquisition["graphql_operations_used"]["const"],
                acquisition["mutation_request_count"]["const"],
                acquisition["outcome"],
            )
        self.assertEqual(
            observed,
            {
                "acquire": (
                    "#/$defs/unlockedProjection",
                    ["query", "mutation", "query"],
                    1,
                    {"enum": ["applied", "ambiguous-reconciled"]},
                ),
                "inherit": (
                    "#/$defs/lockedProjection",
                    ["query"],
                    0,
                    {"const": "already-locked-inherited"},
                ),
            },
        )
        for condition in strategy_conditions:
            self.assertEqual(
                condition["then"]["properties"]["branch"]["properties"][
                    "post_lock"
                ],
                {"$ref": "#/$defs/lockedProjection"},
            )

    def test_public_contract_excludes_raw_or_sensitive_fields(self) -> None:
        forbidden_names = {
            "actor",
            "actors",
            "authorization",
            "credential",
            "credentials",
            "password",
            "raw_response",
            "response",
            "token",
            "url",
            "urls",
        }
        property_names = {
            key
            for _path, value in walk(self.schema)
            if type(value) is dict and type(value.get("properties")) is dict
            for key in value["properties"]
        }
        self.assertFalse(property_names.intersection(forbidden_names))
        serialized = json.dumps(self.schema, sort_keys=True, separators=(",", ":"))
        self.assertNotIn('"GET"', serialized)
        self.assertNotIn("https://api.github.com", serialized)
        self.assertNotIn("graphql_url", serialized)

    def test_schema_contains_no_float_or_number_contract(self) -> None:
        for path, value in walk(self.schema):
            with self.subTest(path=path):
                self.assertIsNot(type(value), float)
                if type(value) is dict and "type" in value:
                    declared = value["type"]
                    self.assertNotEqual(declared, "number")
                    if type(declared) is list:
                        self.assertNotIn("number", declared)


if __name__ == "__main__":
    unittest.main()
