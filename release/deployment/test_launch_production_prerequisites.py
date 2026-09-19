"""Synthetic-only tests: no secret file, credential store, GitHub or provider I/O."""

import ast
import hashlib
import io
import json
import socket
import subprocess
import unittest
import zipfile
from pathlib import Path
from unittest.mock import Mock, patch

import importlib.util
import sys

_module_path = Path(__file__).resolve().with_name("launch_production_prerequisites.py")
_spec = importlib.util.spec_from_file_location("launch_guard_test_target", _module_path)
guard = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = guard
_spec.loader.exec_module(guard)
evidence = guard

CONTROL = "a" * 40
ORIGIN = "b" * 40


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def artifact(subject, filename):
    output = io.BytesIO()
    digest = hashlib.sha256(subject).hexdigest()
    with zipfile.ZipFile(output, "w") as archive:
        archive.writestr(filename, subject)
        archive.writestr(filename.removesuffix(".json") + ".sha256", digest + "\n")
    blob = output.getvalue()
    return blob, digest, "sha256:" + hashlib.sha256(blob).hexdigest()


class FakeReader:
    def __init__(self):
        self.main = CONTROL
        self.worktree = CONTROL
        self.identities = {i: path for i, path in enumerate(sorted(guard.CONTROLLED_WORKFLOW_PATHS), 1)}
        self.inventory = guard.RunInventory([], guard.BLOCKING_STATUSES, True)
        self.secrets = guard.SecretMetadata(guard.CANARY_ENVIRONMENT, guard.REQUIRED_CANARY_SECRET_NAMES, True, True)
        self.fixture_authorized = True
        self.signature_valid = True
        self.signatures = []
        self.latest_state = "20"
        self.fixture_body = encoded({"kind": "crm-canary-fixture-provisioning",
            "state": "allowlist_deployment_verified", "control_sha": ORIGIN,
            "origin": {"run_id": "10"}, "fixture_descriptor_sha256": "c" * 64})
        self.fixture_blob, result_hash, fixture_digest = artifact(self.fixture_body, "result.json")
        self.fixture = {"control_sha": ORIGIN, "run_id": "10", "artifact_id": "11",
                        "artifact_digest": fixture_digest, "result_sha256": result_hash}
        self.state_body = encoded({"authority": "production-phase-state", "repository": guard.REPOSITORY,
            "control": {"workflow_sha": CONTROL, "workflow_path": guard.CANARY_PATH,
                "run_id": "20", "run_attempt": 1, "runner_environment": "github-hosted"},
            "lineage": {"phase": "baseline", "to": "baseline", "phase_ordinal": 1},
            "gates": {"deployment_succeeded": True, "migration_succeeded": True, "canary_succeeded": True}})
        self.state_blob, state_hash, state_digest = artifact(self.state_body, "production-phase-state.json")
        self.predecessor = {"run_id": "20", "run_attempt": 1, "artifact_id": "21",
                            "artifact_digest": state_digest, "state_sha256": state_hash}
        self.metadata = {
            "11": {"id": 11, "name": "crm-canary-fixture-result-10-1", "digest": fixture_digest,
                   "expired": False, "workflow_run": {"id": 10}},
            "21": {"id": 21, "name": "production-phase-state-20-1", "digest": state_digest,
                   "expired": False, "workflow_run": {"id": 20}},
        }
        self.run = {"id": 20, "run_attempt": 1, "workflow_id": self.canary_id, "path": guard.CANARY_PATH,
            "repository": {"full_name": guard.REPOSITORY}, "event": "workflow_dispatch", "head_branch": "main",
            "head_sha": CONTROL, "status": "completed", "conclusion": "success"}
        self.jobs = [{"name": "Exact production phase gate", "run_attempt": 1, "status": "completed",
                      "conclusion": "success", "runner_name": "synthetic-hosted-runner"}]

    @property
    def canary_id(self):
        return next(k for k, v in self.identities.items() if v == guard.CANARY_PATH)

    def context(self, phase="baseline"):
        return guard.LaunchContext(CONTROL, phase, encoded(self.fixture),
            None if phase == "baseline" else encoded(self.predecessor),
            None if phase == "baseline" else self.state_body)

    def current_main_sha(self): return self.main
    def worktree_sha(self): return self.worktree
    def workflow_identities(self): return self.identities
    def active_runs(self, statuses): return self.inventory
    def canary_secret_metadata(self): return self.secrets
    def fixture_public_authority(self, descriptor, current_control): return self.fixture_authorized
    def artifact_metadata(self, artifact_id): return self.metadata[artifact_id]
    def artifact_archive(self, artifact_id): return self.fixture_blob if artifact_id == "11" else self.state_blob
    def run_metadata(self, run_id, attempt=None): return self.run
    def run_jobs(self, run_id, attempt): return self.jobs
    def latest_successful_state_run_id(self, control_sha): return self.latest_state
    def verify_attestation(self, subject, predicate, signer_path, control_sha):
        self.signatures.append((hashlib.sha256(subject).hexdigest(), predicate, signer_path, control_sha))
        return self.signature_valid


class GuardTests(unittest.TestCase):
    def setUp(self):
        self.reader = FakeReader()
        # Accidental I/O added later must fail these tests, not reach a service.
        self.socket_guard = patch.object(socket, "socket", side_effect=AssertionError("network prohibited"))
        self.process_guard = patch.object(subprocess, "run", side_effect=AssertionError("subprocess prohibited"))
        self.socket_guard.start()
        self.process_guard.start()
        self.addCleanup(self.socket_guard.stop)
        self.addCleanup(self.process_guard.stop)

    def blocked(self, context=None):
        with self.assertRaises(guard.LaunchBlocked):
            guard.require_launch_ready(self.reader, context or self.reader.context())

    def test_all_future_ui_prerequisites_required_at_baseline(self):
        self.reader.secrets = guard.SecretMetadata(guard.CANARY_ENVIRONMENT,
            frozenset({"CRM_CANARY_PUBLIC_TARGETS_JSON"}), True, True)
        self.blocked()
        self.assertEqual(self.reader.signatures, [])

    def test_missing_fixture_rejected_before_any_adapter_call(self):
        reader = Mock()
        with self.assertRaises(guard.LaunchBlocked):
            guard.require_launch_ready(reader, guard.LaunchContext(CONTROL, "baseline", None))
        self.assertEqual(reader.mock_calls, [])

    def test_no_default_live_adapter(self):
        with self.assertRaisesRegex(guard.LaunchBlocked, "explicit-reviewed"):
            guard.require_launch_ready(None, self.reader.context())

    def test_genesis_valid_public_fixture_and_secret_metadata(self):
        guard.require_launch_ready(self.reader, self.reader.context())
        self.assertEqual(len(self.reader.signatures), 2)
        self.assertTrue(all(s[3] == ORIGIN for s in self.reader.signatures))

    def test_missing_metadata_protection_or_completeness(self):
        for environment, protected, complete in (("wrong", True, True),
                (guard.CANARY_ENVIRONMENT, False, True), (guard.CANARY_ENVIRONMENT, True, False)):
            with self.subTest(environment=environment, protected=protected, complete=complete):
                self.reader.secrets = guard.SecretMetadata(environment, guard.REQUIRED_CANARY_SECRET_NAMES, protected, complete)
                self.blocked()

    def test_fixture_producer_or_inventory_drift_blocks(self):
        self.reader.fixture_authorized = False
        self.blocked()

    def test_fixture_signature_failure_blocks(self):
        self.reader.signature_valid = False
        self.blocked()

    def test_expired_fixture_artifact_blocks(self):
        self.reader.metadata["11"]["expired"] = True
        self.blocked()

    def test_fixture_archive_digest_mismatch_blocks(self):
        self.reader.fixture_blob += b"tamper"
        self.blocked()

    def test_unknown_descriptor_fields_and_duplicates_rejected(self):
        fixture = {**self.reader.fixture, "secret_value": "sensitive-sentinel"}
        self.blocked(guard.LaunchContext(CONTROL, "baseline", encoded(fixture)))
        with self.assertRaises(guard.LaunchBlocked):
            guard.public_json(b'{"run_id":"10","run_id":"11"}')
        with self.assertRaises(guard.LaunchBlocked):
            guard.public_json(b'{"invalid":NaN}')

    def test_main_or_worktree_mismatch(self):
        for field in ("main", "worktree"):
            with self.subTest(field=field):
                reader = FakeReader()
                setattr(reader, field, ORIGIN)
                with self.assertRaises(guard.LaunchBlocked):
                    guard.require_launch_ready(reader, reader.context())

    def test_main_is_rechecked_after_attestation(self):
        previous = self.reader.verify_attestation
        def verify(*args):
            result = previous(*args)
            self.reader.main = ORIGIN
            return result
        self.reader.verify_attestation = verify
        self.blocked()

    def test_all_blocking_statuses_match_exact_path(self):
        for status in guard.BLOCKING_STATUSES:
            with self.subTest(status=status):
                self.reader.inventory = guard.RunInventory([{"id": 123, "status": status,
                    "path": guard.CANARY_PATH, "workflow_id": self.reader.canary_id,
                    "name": "anything, not a prefix heuristic"}], guard.BLOCKING_STATUSES, True)
                self.blocked()

    def test_workflow_id_still_blocks_when_path_disagrees(self):
        self.reader.inventory = guard.RunInventory([{"id": 123, "status": "waiting",
            "path": ".github/workflows/unknown.yml", "workflow_id": self.reader.canary_id}],
            guard.BLOCKING_STATUSES, True)
        self.assertTrue(guard.production_lock_busy(self.reader))

    def test_similar_display_name_does_not_block_unrelated_workflow(self):
        self.reader.inventory = guard.RunInventory([{"id": 123, "status": "in_progress",
            "path": ".github/workflows/unrelated.yml", "workflow_id": 9000,
            "name": "Apply one exact production phase"}], guard.BLOCKING_STATUSES, True)
        self.assertFalse(guard.production_lock_busy(self.reader))

    def test_completed_run_not_lock_conflict(self):
        self.reader.inventory = guard.RunInventory([{"status": "completed", "path": guard.CANARY_PATH,
            "workflow_id": self.reader.canary_id}], guard.BLOCKING_STATUSES, True)
        self.assertFalse(guard.production_lock_busy(self.reader))

    def test_unknown_controlled_status_fails_closed(self):
        self.reader.inventory = guard.RunInventory([{"status": "new-provider-status", "path": guard.CANARY_PATH,
            "workflow_id": self.reader.canary_id}], guard.BLOCKING_STATUSES, True)
        self.assertTrue(guard.production_lock_busy(self.reader))

    def test_partial_or_missing_status_inventory_fails_closed(self):
        for inventory in (guard.RunInventory([], guard.BLOCKING_STATUSES, False),
                          guard.RunInventory([], frozenset({"in_progress"}), True)):
            self.reader.inventory = inventory
            with self.assertRaises(guard.LaunchBlocked):
                guard.production_lock_busy(self.reader)

    def test_missing_active_identity_and_workflow_inventory_fail_closed(self):
        self.reader.inventory = guard.RunInventory([{"status": "pending"}], guard.BLOCKING_STATUSES, True)
        with self.assertRaises(guard.LaunchBlocked): guard.production_lock_busy(self.reader)
        self.reader.inventory = guard.RunInventory([], guard.BLOCKING_STATUSES, True)
        self.reader.identities.pop(self.reader.canary_id)
        with self.assertRaises(guard.LaunchBlocked): guard.production_lock_busy(self.reader)

    def test_bridge_authenticates_predecessor_signatures_control_and_latest_run(self):
        guard.require_launch_ready(self.reader, self.reader.context("bridge"))
        self.assertEqual(len(self.reader.signatures), 4)
        self.assertEqual(self.reader.signatures[-1][2:], (guard.CANARY_PATH, CONTROL))

    def test_wrong_predecessor_control_or_attempt_or_path(self):
        for field, value in (("head_sha", ORIGIN), ("run_attempt", 2), ("path", "incorrect")):
            with self.subTest(field=field):
                self.reader = FakeReader()
                self.reader.run[field] = value
                self.blocked(self.reader.context("bridge"))

    def test_newer_successful_predecessor_rejected(self):
        self.reader.latest_state = "22"
        self.blocked(self.reader.context("bridge"))

    def test_missing_predecessor_or_wrong_phase_blocks(self):
        self.blocked(guard.LaunchContext(CONTROL, "bridge", encoded(self.reader.fixture)))
        self.blocked(self.reader.context("backend"))

    def test_predecessor_local_file_tamper_blocks(self):
        context = self.reader.context("bridge")
        self.blocked(guard.LaunchContext(CONTROL, "bridge", context.fixture_descriptor,
            context.predecessor_descriptor, context.predecessor_state + b" "))

    def test_unsigned_or_failed_phase_gate_blocks(self):
        self.reader.jobs[0]["conclusion"] = "failure"
        self.blocked(self.reader.context("bridge"))

    def test_predecessor_custom_signature_failure_blocks(self):
        previous = self.reader.verify_attestation
        def verify(*args):
            valid = previous(*args)
            return False if args[1] == guard.STATE_PREDICATE else valid
        self.reader.verify_attestation = verify
        self.blocked(self.reader.context("bridge"))

    def test_upstream_exception_is_not_echoed(self):
        self.reader.canary_secret_metadata = Mock(side_effect=RuntimeError("sensitive-sentinel"))
        with self.assertRaises(guard.LaunchBlocked) as result:
            guard.require_launch_ready(self.reader, self.reader.context())
        self.assertNotIn("sensitive-sentinel", str(result.exception))


import base64
import hashlib
import json
import os
import socket
import subprocess
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch


CONTROL = "a" * 40


def response(value):
    return SimpleNamespace(returncode=0, stdout=json.dumps(value).encode(), stderr=b"")


class TransportTests(unittest.TestCase):
    def test_get_is_explicit_and_mutation_routes_are_not_allowed(self):
        runner = Mock(return_value=response({"ok": True}))
        api = evidence.GitHubGetOnly(run=runner)
        path = evidence.PREFIX + "/actions/runs?status=waiting"
        api.get(path)
        self.assertEqual(runner.call_args.args[0], ["gh", "api", "--hostname", "github.com", "--method", "GET", path])
        for path in (evidence.PREFIX + "/actions/workflows/apply-production-phase.yml/dispatches",
                     evidence.PREFIX + "/environments/other/secrets",
                     evidence.PREFIX + "/contents/../../credentials", "https://example.test/data",
                     evidence.PREFIX + "/actions/runs?method=POST"):
            with self.subTest(path=path), self.assertRaises(guard.LaunchBlocked): api.get(path)

    def test_compare_route_and_public_source_reads_allowed(self):
        api = evidence.GitHubGetOnly(run=Mock(return_value=response({})))
        api.get(evidence.PREFIX + "/compare/" + "b" * 40 + "..." + CONTROL)
        api.get(evidence.PREFIX + "/contents/" + guard.FIXTURE_CONTROLLER + "?ref=" + CONTROL)

    def test_pages_requires_stable_total_and_all_records(self):
        api = evidence.GitHubGetOnly(run=Mock())
        api.get = Mock(side_effect=[{"total_count": 101, "jobs": [{"id": i} for i in range(100)]},
                                   {"total_count": 101, "jobs": [{"id": 100}]}])
        self.assertEqual(len(api.pages(evidence.PREFIX + "/actions/runs/1/jobs", "jobs")["jobs"]), 101)
        self.assertIn("page=2", api.get.call_args.args[0])
        for pages in ([{"total_count": 1, "jobs": []}],
                      [{"total_count": 2, "jobs": [{"id": 1}, {"id": 1}]}],
                      [{"total_count": 2, "jobs": [{"id": 1}]}, {"total_count": 1, "jobs": [{"id": 2}]}]):
            api.get = Mock(side_effect=pages)
            with self.assertRaises(guard.LaunchBlocked): api.pages(evidence.PREFIX + "/actions/runs/1/jobs", "jobs")

    def test_secret_pages_use_names_not_values(self):
        api = evidence.GitHubGetOnly(run=Mock())
        api.get = Mock(return_value={"total_count": 1, "secrets": [{"name": "CRM_CANARY_SYNTHETIC_DRIVER_JSON"}]})
        self.assertEqual(api.pages(evidence.PREFIX + "/environments/rereply-production-canary/secrets", "secrets")["total_count"], 1)

    def test_error_does_not_echo_child_output_or_retry(self):
        runner = Mock(return_value=SimpleNamespace(returncode=1, stdout=b"sensitive-sentinel", stderr=b"sensitive-sentinel"))
        api = evidence.GitHubGetOnly(run=runner)
        with self.assertRaises(guard.LaunchBlocked) as caught: api.get(evidence.PREFIX + "/git/ref/heads/main")
        self.assertNotIn("sensitive-sentinel", str(caught.exception))
        self.assertEqual(runner.call_count, 1)

    def test_impostor_host_and_provider_inputs_do_not_reach_subprocess(self):
        with patch.dict(os.environ, {"GH_HOST": "impostor.invalid", "GH_REPO": "wrong/repo",
                        "GH_DEBUG": "api", "GIT_TRACE": "1", "DO_PRODUCTION_READ_TOKEN": "private",
                        "DO_PRODUCTION_TARGET_JSON": "private", "GH_TOKEN": "gh-auth"}):
            env = evidence.github_subprocess_environment()
        self.assertEqual(env["GH_HOST"], "github.com")
        self.assertEqual(env["GH_REPO"], guard.REPOSITORY)
        self.assertEqual(env["GH_TOKEN"], "gh-auth")
        for name in ("GH_DEBUG", "GIT_TRACE", "DO_PRODUCTION_READ_TOKEN", "DO_PRODUCTION_TARGET_JSON"):
            self.assertNotIn(name, env)

    def test_git_cleanliness_check_does_not_access_credential_values(self):
        class NamesOnlyEnvironment(dict):
            def __getitem__(self, key):
                if key in {"GH_TOKEN", "DO_PRODUCTION_READ_TOKEN", "DO_PRODUCTION_TARGET_JSON"}:
                    raise AssertionError("credential value accessed before authority")
                return super().__getitem__(key)
        env = NamesOnlyEnvironment(PATH="runtime", GH_TOKEN="not-read", DO_PRODUCTION_READ_TOKEN="not-read",
                                   DO_PRODUCTION_TARGET_JSON="not-read")
        runner = Mock(return_value=SimpleNamespace(returncode=0, stdout=b"", stderr=b""))
        with patch.object(evidence.os, "environ", env):
            evidence.GitHubGetOnly(run=runner).command(["git", "status", "--porcelain"])
        self.assertNotIn("GH_TOKEN", runner.call_args.kwargs["env"])


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.socket = patch.object(socket, "socket", side_effect=AssertionError("network prohibited"))
        self.process = patch.object(subprocess, "run", side_effect=AssertionError("process prohibited"))
        self.socket.start(); self.process.start()
        self.addCleanup(self.socket.stop); self.addCleanup(self.process.stop)
        self.adapter = object.__new__(evidence.GitHubEvidence)
        self.adapter.root = Path("synthetic-control")
        self.adapter.candidate_audit = False
        self.adapter.api = Mock()
        self.adapter.api.gh = "gh"
        self.adapter._identities = None
        self.adapter._identity_control = None

    def test_all_lock_statuses_are_queried(self):
        self.adapter.api.pages.return_value = {"total_count": 0, "workflow_runs": []}
        inventory = self.adapter.active_runs(guard.BLOCKING_STATUSES)
        self.assertTrue(inventory.complete)
        routes = [call.args[0] for call in self.adapter.api.pages.call_args_list]
        for status in guard.BLOCKING_STATUSES:
            self.assertTrue(any("status=" + status in route for route in routes))

    def test_inventory_includes_retired_dynamic_and_shared_lock_not_environment_names(self):
        paths = sorted(guard.CONTROLLED_WORKFLOW_PATHS) + [
            ".github/workflows/retired.yml", ".github/workflows/dynamic.yml", ".github/workflows/readonly.yml"]
        records = [{"id": i + 1, "path": path} for i, path in enumerate(paths)]
        self.adapter.current_main_sha = Mock(return_value=CONTROL)
        self.adapter.api.pages.return_value = {"workflows": records}
        self.adapter.api.get.return_value = [{"type": "file", "path": path} for path in paths if "retired" not in path]
        self.adapter.source = Mock(side_effect=lambda path, control: path)
        self.adapter.source_bytes = Mock(side_effect=lambda path:
            b"concurrency:\n  group: rereply-production\n" if path in guard.CONTROLLED_WORKFLOW_PATHS else
            b"concurrency: ${{ vars.ANY_GROUP }}\n" if "dynamic" in path else
            b"jobs:\n  read:\n    environment: rereply-production-crm-fixture\n")
        identities = self.adapter.workflow_identities()
        self.assertIn(paths.index(".github/workflows/retired.yml") + 1, identities)
        self.assertIn(paths.index(".github/workflows/dynamic.yml") + 1, identities)
        self.assertNotIn(paths.index(".github/workflows/readonly.yml") + 1, identities)
        self.assertTrue(all("retired" not in call.args[0] for call in self.adapter.source.call_args_list))

    def test_active_run_movement_between_status_lists_blocks(self):
        first = {"id": 1, "status": "queued"}
        second = {"id": 1, "status": "in_progress"}
        self.adapter.api.pages.side_effect = [{"workflow_runs": [first]}, {"workflow_runs": [second]}]
        with self.assertRaises(guard.LaunchBlocked): self.adapter.active_runs(guard.BLOCKING_STATUSES)

    def test_candidate_mode_cannot_authorize_launch(self):
        self.adapter.candidate_audit = True
        with self.assertRaisesRegex(guard.LaunchBlocked, "not-launch-authority"): self.adapter.worktree_sha()
        self.adapter.api.command.assert_not_called()

    def test_dirty_checkout_cannot_authorize_launch(self):
        self.adapter.api.command.return_value = b" M release/control.py\n"
        with self.assertRaisesRegex(guard.LaunchBlocked, "uncommitted"): self.adapter.worktree_sha()
        self.assertIn("--untracked-files=all", self.adapter.api.command.call_args.args[0])

    def test_dirty_and_untracked_reject_before_import_or_credentials(self):
        for status in (b" M control.py\n", b"?? release/deployment/injected.py\n"):
            api = Mock(); api.command.return_value = status
            with patch.object(evidence.GitHubEvidence, "_load_fixture_verifier") as loader:
                with self.assertRaisesRegex(guard.LaunchBlocked, "uncommitted"):
                    evidence.GitHubEvidence(Path.cwd(), transport=api)
                loader.assert_not_called()
                api.get.assert_not_called()

    def test_wrong_main_rejects_before_import(self):
        api = Mock(); api.command.side_effect = [b"", CONTROL.encode()]
        api.get.return_value = {"object": {"sha": "b" * 40}}
        with patch.object(evidence.GitHubEvidence, "_load_fixture_verifier") as loader:
            with self.assertRaisesRegex(guard.LaunchBlocked, "not-current-main"):
                evidence.GitHubEvidence(Path.cwd(), transport=api)
            loader.assert_not_called()

    def test_protected_environment_requires_main_only_policy(self):
        self.adapter.require_protected_main = Mock()
        self.adapter.api.get.return_value = {"name": guard.CANARY_ENVIRONMENT,
            "deployment_branch_policy": {"protected_branches": False, "custom_branch_policies": True}}
        self.adapter.api.pages.side_effect = [
            {"total_count": 1, "branch_policies": [{"id": 1, "name": "main", "type": "branch"}]},
            {"total_count": 1, "secrets": [{"name": "CRM_CANARY_PUBLIC_TARGETS_JSON"}]},
        ]
        result = self.adapter.canary_secret_metadata()
        self.assertTrue(result.protected_environment_verified)
        self.assertNotIn("CRM_CANARY_SYNTHETIC_DRIVER_JSON", result.names)

    def test_signature_binds_flags_subject_and_custom_predicate(self):
        subject = b'{"public":"receipt"}'
        digest = hashlib.sha256(subject).hexdigest()
        predicate = guard.FIXTURE_PREDICATE
        statement = {"subject": [{"digest": {"sha256": digest}}], "predicateType": predicate,
                     "predicate": {"public": "receipt"}}
        self.adapter.api.command.return_value = json.dumps([{"verificationResult": {"statement": statement}}]).encode()
        self.assertTrue(self.adapter.verify_attestation(subject, predicate, guard.FIXTURE_PATH, CONTROL))
        command = self.adapter.api.command.call_args.args[0]
        for flag, value in (("--source-digest", CONTROL), ("--signer-digest", CONTROL),
                            ("--source-ref", "refs/heads/main"), ("--predicate-type", predicate)):
            self.assertEqual(command[command.index(flag) + 1], value)
        self.assertIn("--deny-self-hosted-runners", command)
        self.assertFalse(Path(command[3]).exists())
        for field, value in (("predicate", {"public": "tampered"}), ("predicateType", "wrong"), ("subject", [])):
            bad = dict(statement); bad[field] = value
            self.adapter.api.command.return_value = json.dumps([{"verificationResult": {"statement": bad}}]).encode()
            with self.assertRaises(guard.LaunchBlocked):
                self.adapter.verify_attestation(subject, predicate, guard.FIXTURE_PATH, CONTROL)

    def test_source_blob_authentication(self):
        raw = b"public source\n"
        record = {"encoding": "base64", "content": base64.b64encode(raw).decode(),
                  "sha": hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()}
        self.assertEqual(self.adapter.source_bytes(record), raw)
        record["sha"] = "b" * 40
        with self.assertRaises(guard.LaunchBlocked): self.adapter.source_bytes(record)

    def test_current_api_producer_bytes_must_equal_executing_checkout(self):
        self.adapter._fixture = Mock()
        self.adapter.require_protected_main = Mock()
        self.adapter.current_main_sha = Mock(return_value=CONTROL)
        origin = "b" * 40
        self.adapter.api.get.return_value = {"status": "ahead", "merge_base_commit": {"sha": origin}}
        self.adapter.source = Mock(return_value={})
        self.adapter.source_bytes = Mock(return_value=b"api-source")
        with patch.object(Path, "read_bytes", return_value=b"different-checkout-source"):
            with self.assertRaisesRegex(guard.LaunchBlocked, "current-producer-source-differs"):
                self.adapter.fixture_public_authority({"control_sha": origin}, CONTROL)
        self.adapter._fixture._verify_fixture_producer_compatibility.assert_not_called()

    def test_predecessor_callback_reauthenticates_before_return(self):
        context = guard.LaunchContext(CONTROL, "bridge", b"{}", b"{}", b"state")
        with patch.object(guard, "require_current_control") as control, patch.object(guard, "descriptor", return_value={}), patch.object(guard, "_predecessor") as verify:
            self.assertEqual(self.adapter.authenticate_predecessor(context), b"state")
            self.assertEqual(control.call_count, 2)
            verify.assert_called_once_with(self.adapter, context, {})

    def test_public_fixture_api_returns_authenticated_bytes_and_rechecks_control(self):
        with patch.object(guard, "require_current_control") as control, patch.object(guard, "descriptor", return_value={}), patch.object(guard, "_fixture", return_value=b"signed-public-result") as verify:
            self.assertEqual(self.adapter.authenticate_public_fixture(b"descriptor", CONTROL), b"signed-public-result")
            verify.assert_called_once_with(self.adapter, {}, CONTROL)
            self.assertEqual(control.call_count, 2)
        self.adapter.candidate_audit = True
        with self.assertRaises(guard.LaunchBlocked): self.adapter.authenticate_public_fixture(b"descriptor", CONTROL)

    def test_only_typed_provider_quiescence_maps_to_transient(self):
        class TypedTransient(RuntimeError): pass
        self.adapter._parity = SimpleNamespace(ProviderNotQuiescent=TypedTransient,
            require_provider_parity=Mock(side_effect=TypedTransient("constant")))
        self.adapter._read_private_inputs = Mock()
        with patch.object(guard, "require_current_control"):
            with self.assertRaisesRegex(guard.LaunchNotQuiescent, "provider-not-yet-quiescent"):
                self.adapter.require_provider_parity(guard.LaunchContext(CONTROL, "baseline", b"{}"))
            self.adapter._parity.require_provider_parity.side_effect = RuntimeError("hard-failure")
            with self.assertRaises(RuntimeError) as caught:
                self.adapter.require_provider_parity(guard.LaunchContext(CONTROL, "baseline", b"{}"))
            self.assertNotIsInstance(caught.exception, guard.LaunchNotQuiescent)

    def test_dispatch_identity_binds_api_workflow_and_explicit_phase_title(self):
        self.adapter.workflow_identities = Mock(return_value={7: guard.CANARY_PATH})
        self.adapter.source = Mock(return_value={})
        self.adapter.source_bytes = Mock(return_value=b"run-name: Verify ${{ inputs.phase }}\n")
        expected = self.adapter.dispatch_identity("verify-production-crm-canary.yml", ("-f", "phase=ui"), CONTROL)
        self.assertEqual(expected, guard.DispatchIdentity(7, guard.CANARY_PATH, CONTROL, "Verify ui"))
        for fields in (("-f", "phase=unknown"), ("-f", "phase=ui", "-f", "phase=bridge"), ("phase=ui",)):
            with self.assertRaises(guard.LaunchBlocked):
                self.adapter.dispatch_identity("verify-production-crm-canary.yml", fields, CONTROL)


class MaterializationTests(unittest.TestCase):
    def setUp(self):
        self.expected = guard.DispatchIdentity(7, guard.CANARY_PATH, CONTROL, "Verify ui")
        self.run = {"id": 12, "workflow_id": 7, "path": guard.CANARY_PATH,
                    "repository": {"full_name": guard.REPOSITORY}, "event": "workflow_dispatch",
                    "head_branch": "main", "head_sha": CONTROL, "run_attempt": 1,
                    "previous_attempt_url": None, "display_title": "Verify ui"}

    def test_one_exact_new_run_and_no_run(self):
        self.assertEqual(guard.materialized_dispatch_run(frozenset({11}), [self.run], self.expected), 12)
        self.assertIsNone(guard.materialized_dispatch_run(frozenset({12}), [self.run], self.expected))

    def test_foreign_or_misbound_run_fails_closed(self):
        for field, value in (("workflow_id", 8), ("path", ".github/workflows/foreign.yml"),
                             ("event", "push"), ("head_branch", "other"), ("head_sha", "b" * 40),
                             ("run_attempt", 2), ("run_attempt", True), ("previous_attempt_url", "previous"),
                             ("display_title", "Verify baseline"), ("repository", {"full_name": "wrong/repo"})):
            with self.subTest(field=field), self.assertRaises(guard.LaunchBlocked):
                guard.materialized_dispatch_run(frozenset(), [{**self.run, field: value}], self.expected)

    def test_duplicate_or_multiple_new_runs_never_adopted(self):
        for rows in ([self.run, self.run], [self.run, {**self.run, "id": 13}]):
            with self.assertRaises(guard.LaunchBlocked):
                guard.materialized_dispatch_run(frozenset(), rows, self.expected)


class IsolatedEntrypointTests(unittest.TestCase):
    def test_isolated_help_from_unrelated_directory(self):
        import tempfile
        with tempfile.TemporaryDirectory(prefix="launch-help-test-") as temp:
            result = subprocess.run([sys.executable, "-I", "-S", "-B", str(_module_path), "--help"],
                                    cwd=temp, capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertIn(b"audit-public", result.stdout)
        self.assertIn(b"check-launch", result.stdout)

    def test_isolated_negative_never_loads_controller_or_reads_credentials(self):
        import tempfile
        with tempfile.TemporaryDirectory(prefix="launch-negative-test-") as temp:
            result = subprocess.run([sys.executable, "-I", "-S", "-B", str(_module_path), "check-launch",
                                     "--control-root", temp, "--fixture-evidence", str(Path(temp) / "absent.json")],
                                    cwd=temp, capture_output=True, timeout=20,
                                    env={**os.environ, "GH_HOST": "impostor.invalid",
                                         "DO_PRODUCTION_READ_TOKEN": "private-sentinel"})
        self.assertEqual(result.returncode, 1)
        self.assertIn(b"readonly-command-failed", result.stdout)
        self.assertNotIn(b"private-sentinel", result.stdout + result.stderr)

    def test_isolated_audit_missing_public_verifier_has_no_secret_or_network_fallback(self):
        import tempfile
        with tempfile.TemporaryDirectory(prefix="launch-audit-negative-test-") as temp:
            result = subprocess.run([sys.executable, "-I", "-S", "-B", str(_module_path), "audit-public",
                                     "--control-root", temp, "--fixture-evidence", str(Path(temp) / "absent.json")],
                                    cwd=temp, capture_output=True, timeout=20,
                                    env={**os.environ, "DO_PRODUCTION_READ_TOKEN": "private-sentinel"})
        self.assertEqual(result.returncode, 1)
        self.assertIn(b"reviewed-fixture-verifier-missing", result.stdout)
        self.assertNotIn(b"private-sentinel", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
