from __future__ import annotations

import copy
import datetime as dt
import http.client
import inspect
import json
import subprocess
import sys
import unittest
import urllib.error
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import apply_production_change as apply
import verify_production_release as common


APP_ID = "11111111-1111-4111-8111-111111111111"
OLD_DEPLOYMENT = "22222222-2222-4222-8222-222222222222"
NEW_DEPLOYMENT = "33333333-3333-4333-8333-333333333333"


def digest(character: str) -> str:
    return "sha256:" + character * 64


def digest_spec(character: str = "1") -> dict[str, object]:
    return {
        "name": "rereply",
        "region": "sgp",
        "vpc": {"id": "private-vpc"},
        "envs": [{"key": "SAFE", "value": "EV[private]", "type": "SECRET", "scope": "RUN_TIME"}],
        "services": [
            {"name": "omnitech-web", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-web", "digest": digest(character)}, "envs": []},
            {"name": "meta-relay", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-meta-relay", "digest": digest(character)}, "envs": []},
            {"name": "gmail-relay", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-gmail-relay", "digest": digest(character)}, "envs": []},
        ],
        "jobs": [
            {"name": "rereply-rls-migrate", "image": {"registry_type": "GHCR", "registry": "ghcr.io", "repository": "medtechcorps-netizen/rereply-release-web", "digest": digest(character)}, "envs": []},
        ],
        "ingress": {"rules": [{"match": {"path": {"prefix": "/"}}, "component": {"name": "omnitech-web"}}]},
        "domains": [{"domain": "example.invalid", "type": "PRIMARY"}],
        "databases": [],
    }


def legacy_spec() -> dict[str, object]:
    spec = digest_spec()
    paths = {
        "omnitech-web": "docker/Dockerfile",
        "meta-relay": "docker/meta-relay.Dockerfile",
        "gmail-relay": "docker/gmail-relay.Dockerfile",
        "rereply-rls-migrate": "docker/Dockerfile",
    }
    for collection in ("services", "jobs"):
        for component in spec[collection]:
            component.pop("image")
            component["git"] = {"repo_clone_url": "https://github.com/medtechcorps-netizen/whatomate.git", "branch": "main"}
            component["dockerfile_path"] = paths[component["name"]]
    return spec


def app_response(spec: dict[str, object], deployment_id: str, *, pinned: bool = False) -> dict[str, object]:
    return {
        "app": {
            "id": APP_ID,
            "updated_at": "2026-08-27T00:01:00Z",
            "default_ingress": "https://example.invalid",
            "spec": copy.deepcopy(spec),
            "active_deployment": {"id": deployment_id, "phase": "ACTIVE", "spec": copy.deepcopy(spec)},
            "in_progress_deployment": None,
            "pending_deployment": None,
            "pinned_deployment": ({"id": deployment_id} if pinned else None),
        }
    }


def deployment_response(spec: dict[str, object], deployment_id: str) -> dict[str, object]:
    return {
        "deployment": {
            "id": deployment_id,
            "phase": "ACTIVE",
            "spec": copy.deepcopy(spec),
            "jobs": [{"name": "rereply-rls-migrate", "phase": "SUCCEEDED"}],
        }
    }


class FakeResponse:
    def __init__(self, value: object, url: str, *, content_type: str = "application/json", status: int = 200) -> None:
        self.raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.url = url
        self.status = status
        self.headers = {"Content-Type": content_type}

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def geturl(self) -> str:
        return self.url

    def getcode(self) -> int:
        return self.status

    def read(self, amount: int) -> bytes:
        return self.raw[:amount]


class IncompleteResponse(FakeResponse):
    def read(self, amount: int) -> bytes:
        raise http.client.IncompleteRead(b'{"app":')


class QueueOpener:
    def __init__(self, values: list[object]) -> None:
        self.values = list(values)
        self.requests: list[object] = []

    def open(self, request: object, timeout: int) -> FakeResponse:
        self.requests.append(request)
        value = self.values.pop(0)
        if isinstance(value, BaseException):
            raise value
        if isinstance(value, FakeResponse):
            return value
        return FakeResponse(value, request.full_url)


class ApplyControllerTests(unittest.TestCase):
    def test_isolated_direct_entrypoint_resolves_only_sibling_controls(self) -> None:
        result = subprocess.run(
            [sys.executable, "-I", "-S", "-B", str(Path(apply.__file__).resolve()), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_plan_and_recovery_expiring_during_preflight_block_before_put(self) -> None:
        initial = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        expired = initial + dt.timedelta(minutes=6)
        plan = {
            "issued_at": common.format_timestamp(initial),
            "expires_at": common.format_timestamp(initial + dt.timedelta(minutes=5)),
        }
        recovery = {
            "issued_at": common.format_timestamp(initial),
            "expires_at": common.format_timestamp(initial + dt.timedelta(minutes=5)),
        }
        moments = iter((initial, expired))
        clock = lambda: next(moments)
        self.assertEqual(apply._clock_value(clock), initial)
        with self.assertRaises(common.ReleaseError):
            apply.require_fresh_immediately_before_mutation(
                plan=plan, recovery=recovery, clock=clock
            )
        source = inspect.getsource(apply.apply_change)
        self.assertLess(source.index("cas_before"), source.index("require_fresh_immediately_before_mutation"))
        self.assertLess(source.index("require_fresh_immediately_before_mutation"), source.index("put_app_once"))

    def test_expiry_is_exclusive_and_fractional_clock_is_not_truncated(self) -> None:
        issued = dt.datetime(2026, 8, 27, 0, 0, 0, tzinfo=dt.timezone.utc)
        expires = issued + dt.timedelta(minutes=5)
        authority = {
            "issued_at": common.format_timestamp(issued),
            "expires_at": common.format_timestamp(expires),
        }
        fractional = expires + dt.timedelta(microseconds=999_999)
        self.assertEqual(apply._clock_value(lambda: fractional), fractional)
        for checked in (expires, fractional):
            with self.subTest(checked=checked):
                with self.assertRaises(common.ReleaseError):
                    apply.require_fresh_immediately_before_mutation(
                        plan=authority, recovery=authority, clock=lambda: checked
                    )

    def test_plan_control_must_match_exact_artifact_and_current_controls(self) -> None:
        authority = {
            "run_id": "101",
            "run_attempt": 1,
            "artifact_id": "201",
            "artifact_digest": digest("1"),
            "sha256": "2" * 64,
        }
        plan = {
            "control": {
                "workflow_sha": "a" * 40,
                "workflow_path": ".github/workflows/plan-production-rollout.yml",
                "run_id": "101",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "contract_sha256": "3" * 64,
                "release_policy_sha256": "4" * 64,
                "change_schema_sha256": "5" * 64,
                "verifier_sha256": "6" * 64,
            }
        }
        apply._validate_plan_control(
            plan, {"workflow_sha": "a" * 40}, authority, "4" * 64, "5" * 64
        )
        tampered = copy.deepcopy(plan)
        tampered["control"]["run_id"] = "102"
        with self.assertRaises(common.ReleaseError):
            apply._validate_plan_control(
                tampered,
                {"workflow_sha": "a" * 40},
                authority,
                "4" * 64,
                "5" * 64,
            )

    def test_exact_image_update_preserves_every_non_source_leaf(self) -> None:
        before = legacy_spec()
        desired = common.set_phase_images(
            before,
            {"web": digest("1"), "meta-relay": digest("2"), "gmail-relay": digest("3")},
        )
        changes = common.require_exact_image_change(before, desired)
        self.assertEqual(len(changes), 12)
        self.assertEqual(common.non_source_fingerprint(before), common.non_source_fingerprint(desired))
        self.assertEqual(common.environment_value_fingerprint(before), common.environment_value_fingerprint(desired))
        self.assertEqual(common.extract_image_digests(desired)["web"], digest("1"))
        self.assertEqual(common.extract_image_digests(desired)["meta-relay"], digest("2"))

    def test_digest_phase_changes_exactly_four_digest_leaves(self) -> None:
        before = digest_spec("1")
        desired = common.set_phase_images(before, {key: digest("2") for key in ("web", "meta-relay", "gmail-relay")})
        changes = common.require_exact_image_change(before, desired)
        self.assertEqual(len(changes), 4)
        self.assertTrue(all(pointer.endswith("/image/digest") for pointer in changes))

    def test_sent_put_with_malformed_200_is_ambiguous_and_reconciles_get_only(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                FakeResponse({"app": {}}, app_url, content_type="text/plain"),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        app_value, deployment_value, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual(app_value["app"]["spec"], desired)
        self.assertEqual(deployment_value["deployment"]["phase"], "ACTIVE")
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_http_408_after_put_is_ambiguous_and_never_retries_mutation(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                urllib.error.HTTPError(app_url, 408, "timeout", {}, None),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        _, _, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_truncated_put_response_is_ambiguous_and_reconciles_get_only(self) -> None:
        desired = digest_spec("2")
        app_url = common.API_ORIGIN + f"/v2/apps/{APP_ID}"
        opener = QueueOpener(
            [
                IncompleteResponse({"app": {}}, app_url),
                app_response(desired, NEW_DEPLOYMENT),
                deployment_response(desired, NEW_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaises(common.AmbiguousMutation):
            client.put_app_once(desired)
        _, _, ambiguous = apply.reconcile_until_active(
            client, desired, sleeper=lambda _seconds: None, poll_limit=1
        )
        self.assertTrue(ambiguous)
        self.assertEqual([request.method for request in opener.requests], ["PUT", "GET", "GET"])
        with self.assertRaises(common.ReleaseError):
            client.put_app_once(desired)

    def test_pinned_or_pending_deployment_blocks_before_mutation(self) -> None:
        spec = digest_spec()
        for key, value in (("pinned_deployment", {"id": OLD_DEPLOYMENT}), ("pending_deployment", {"id": OLD_DEPLOYMENT})):
            app_value = app_response(spec, OLD_DEPLOYMENT)
            app_value["app"][key] = value
            opener = QueueOpener(
                [
                    app_value,
                    deployment_response(spec, OLD_DEPLOYMENT),
                    copy.deepcopy(app_value),
                    deployment_response(spec, OLD_DEPLOYMENT),
                ]
            )
            client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
            with self.subTest(key=key):
                with self.assertRaises(common.ReleaseError):
                    apply.observe_stable(client)
                self.assertFalse(any(request.method == "PUT" for request in opener.requests))

    def test_timestamp_drift_is_allowed_only_for_predecessor_lineage(self) -> None:
        import test_verify_production_release as release_fixtures

        receipt = release_fixtures.apply_receipt()
        receipt_hash = common.sha256_bytes(common.canonical_file_bytes(receipt))
        state = common.build_phase_state(
            receipt,
            change_receipt_sha256=receipt_hash,
            canary_sha256="9" * 64,
            control={
                "workflow_sha": "a" * 40,
                "workflow_path": (
                    ".github/workflows/verify-production-crm-canary.yml"
                ),
                "run_id": "401",
                "run_attempt": 1,
                "runner_environment": "github-hosted",
                "release_policy_sha256": "b" * 64,
                "change_schema_sha256": "c" * 64,
            },
            completed_at="2026-08-27T00:01:00Z",
        )
        state_hash = common.sha256_bytes(common.canonical_file_bytes(state))
        live = copy.deepcopy(state["provider_state"])
        live["app_updated_at_sha256"] = "9" * 64
        apply._validate_predecessor(
            state, state_hash, "baseline", live, state_hash
        )

        for key in (
            "app_identity_sha256",
            "default_ingress_sha256",
            "active_deployment_identity_sha256",
            "canonical_spec_sha256",
            "environment_values_sha256",
            "non_source_projection_sha256",
        ):
            with self.subTest(key=key):
                drifted = copy.deepcopy(live)
                drifted[key] = "0" * 64
                with self.assertRaises(common.ReleaseError):
                    apply._validate_predecessor(
                        state, state_hash, "baseline", drifted, state_hash
                    )

        image_drift = copy.deepcopy(live)
        image = image_drift["images"][0]
        image["digest"] = digest("9")
        image["subject"] = image["repository"] + "@" + digest("9")
        with self.assertRaises(common.ReleaseError):
            apply._validate_predecessor(
                state, state_hash, "baseline", image_drift, state_hash
            )

        observation = {
            "provider_observation": {
                "app_identity_sha256": live["app_identity_sha256"],
                "default_ingress_sha256": live["default_ingress_sha256"],
                "app_updated_at_sha256": live["app_updated_at_sha256"],
                "active_deployment_identity_sha256": live[
                    "active_deployment_identity_sha256"
                ],
                "live_canonical_spec_sha256": live[
                    "canonical_spec_sha256"
                ],
                "environment_values_sha256": live[
                    "environment_values_sha256"
                ],
                "non_source_projection_sha256": live[
                    "non_source_projection_sha256"
                ],
                "live_active_equal": True,
                "predecessor_match": True,
            }
        }
        apply._match_plan_observation(observation, live)
        observation["provider_observation"]["app_updated_at_sha256"] = "8" * 64
        with self.assertRaises(common.ReleaseError):
            apply._match_plan_observation(observation, live)

    def test_live_timestamp_change_between_apply_reads_fails_closed(self) -> None:
        spec = digest_spec()
        first_app = app_response(spec, OLD_DEPLOYMENT)
        second_app = app_response(spec, OLD_DEPLOYMENT)
        second_app["app"]["updated_at"] = "2026-08-27T00:01:01Z"
        opener = QueueOpener(
            [
                first_app,
                deployment_response(spec, OLD_DEPLOYMENT),
                second_app,
                deployment_response(spec, OLD_DEPLOYMENT),
            ]
        )
        client = apply.ProductionAppClient(APP_ID, "t" * 24, opener=opener)
        with self.assertRaisesRegex(
            common.ReleaseError, "production changed between the two exact reads"
        ):
            apply.observe_stable(client)
        self.assertEqual([request.method for request in opener.requests], ["GET"] * 4)

    def test_baseline_uses_non_null_genesis_authority_without_file(self) -> None:
        before = {
            "source_mode": "legacy-git",
        }
        genesis = "a" * 64
        apply._validate_predecessor(None, None, "genesis", before, genesis)
        with self.assertRaises(common.ReleaseError):
            apply._validate_predecessor(None, None, "genesis", before, "not-a-hash")
        request = apply.validate_evidence_request(
            {
                "production_plan": {"run_id": "1", "run_attempt": 1, "artifact_id": "2", "artifact_digest": "sha256:" + "1" * 64, "sha256": "2" * 64},
                "recovery": {"run_id": "3", "run_attempt": 1, "artifact_id": "4", "artifact_digest": "sha256:" + "3" * 64, "sha256": "4" * 64},
                "rollout_plan_sha256": "5" * 64,
                "predecessor_state_sha256": genesis,
            }
        )
        self.assertEqual(request["predecessor_state_sha256"], genesis)


if __name__ == "__main__":
    unittest.main()
