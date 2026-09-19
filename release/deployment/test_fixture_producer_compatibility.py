"""Offline regression oracles for the one reviewed September 15 fixture receipt."""

from __future__ import annotations

import ast
import base64
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

try:
    from . import provision_production_crm_canary_fixture as fixture
    from . import test_provision_production_crm_canary_fixture as samples
except ImportError:
    import provision_production_crm_canary_fixture as fixture
    import test_provision_production_crm_canary_fixture as samples

common = fixture.common
CONTROLLER = "release/deployment/provision_production_crm_canary_fixture.py"
OLD_CONTROL = "974c875ec554df996986bb369e2900a9a79ff0cd"
OLD_RUN = "35004135116"
OLD_BLOB = "e18a9d20279105f4a016ea062f86bb713fdb2ba3"
REVIEWED_BLOB = "4302ab0c8942bf318a385cc48af7b79541b3217a"
WORKFLOW_BLOB = "d2cd700bdf31f999b25563fd906cf1354f0d8feb"
CALL = b"        _verify_fixture_producer_compatibility(path, old, new, d)\n"
ORIGINAL_CALL = b'        _require(old.get("sha") == new.get("sha"),"fixture producer changed after receipt")\n'
READ_ONLY_ADDITION = (
    b"        # The contract rebaseline that follows a completed allowlist append needs\n"
    b"        # exactly these two fingerprints, taken from the same provider read.\n"
    b'        "environment_values_sha256": common.environment_value_fingerprint(spec),\n'
    b'        "non_source_projection_sha256": common.non_source_fingerprint(spec),\n'
)


def blob(source):
    return hashlib.sha1(b"blob " + str(len(source)).encode() + b"\0" + source).hexdigest()


def content(source):
    return {"sha": blob(source), "encoding": "base64",
            "content": base64.b64encode(source).decode("ascii")}


def sources():
    current = Path(fixture.__file__).read_bytes().replace(b"\r\n", b"\n")
    helper = next(n for n in ast.parse(current).body if isinstance(n, ast.FunctionDef)
                  and n.name == "_verify_fixture_producer_compatibility")
    lines = current.splitlines(keepends=True)
    del lines[helper.lineno - 1:helper.end_lineno + 2]
    reviewed = b"".join(lines).replace(CALL, ORIGINAL_CALL)
    return current, reviewed, reviewed.replace(READ_ONLY_ADDITION, b"")


class FixtureProducerCompatibilityTests(unittest.TestCase):
    def setUp(self):
        self.source, self.reviewed, self.old_source = sources()
        self.descriptor = {"control_sha": OLD_CONTROL, "run_id": OLD_RUN}

    def verify(self, source=None, *, descriptor=None, old_blob=OLD_BLOB):
        source = self.source if source is None else source
        # Simulate a genuinely changed protected checkout as well as changed
        # API bytes. The immutable reconstructed-source pin must still reject it.
        with mock.patch.object(Path, "read_bytes", return_value=source):
            fixture._verify_fixture_producer_compatibility(
                CONTROLLER, {"sha": old_blob}, content(source),
                self.descriptor if descriptor is None else descriptor)

    def test_exact_old_and_reviewed_source_snapshots_are_immutable(self):
        self.assertEqual(blob(self.old_source), OLD_BLOB)
        self.assertEqual(blob(self.reviewed), REVIEWED_BLOB)
        self.assertEqual(self.reviewed.count(READ_ONLY_ADDITION), 1)
        self.assertEqual(self.reviewed.replace(READ_ONLY_ADDITION, b""), self.old_source)

    def test_exact_ancestor_receipt_crosses_only_reviewed_change(self):
        self.verify()
        self.verify(descriptor={**self.descriptor, "run_id": int(OLD_RUN)})

    def test_identical_old_or_current_producer_retains_original_rule(self):
        for identity in (OLD_BLOB, REVIEWED_BLOB, blob(self.source)):
            with self.subTest(blob=identity):
                fixture._verify_fixture_producer_compatibility(
                    CONTROLLER, {"sha": identity}, {"sha": identity},
                    {"control_sha": "a" * 40, "run_id": "12345"})

    def test_exception_is_not_a_general_previous_producer_allowlist(self):
        for old_blob in (REVIEWED_BLOB, "a" * 40):
            with self.subTest(old_blob=old_blob), self.assertRaisesRegex(
                    common.ReleaseError, "producer changed"):
                self.verify(old_blob=old_blob)
        for key, value in (("run_id", "35004135117"), ("control_sha", "a" * 40)):
            with self.subTest(key=key), self.assertRaisesRegex(common.ReleaseError, "producer changed"):
                self.verify(descriptor={**self.descriptor, key: value})

    def test_workflow_must_be_the_one_reviewed_unchanged_blob(self):
        fixture._verify_fixture_producer_compatibility(
            fixture.WORKFLOW_PATH, {"sha": WORKFLOW_BLOB}, {"sha": WORKFLOW_BLOB}, self.descriptor)
        for old, new in ((WORKFLOW_BLOB, "b" * 40), ("b" * 40, "b" * 40)):
            with self.subTest(old=old, new=new), self.assertRaises(common.ReleaseError):
                fixture._verify_fixture_producer_compatibility(
                    fixture.WORKFLOW_PATH, {"sha": old}, {"sha": new}, self.descriptor)

    def test_added_execution_global_verifier_and_module_code_are_rejected(self):
        candidates = {
            "executor": self.source.replace(b"class ProductProvisioner:\n", b"class ProductProvisioner:\n    injected = True\n"),
            "global": self.source.replace(b"MAX_PAGES = 100\n", b"MAX_PAGES = 101\n"),
            "verifier": self.source.replace(b'"fixture run not successful")', b'"fixture run authority drift")'),
            "outside_helper": self.source + b"\n_verify_fixture_producer_compatibility = lambda *args: None\n",
            "unrelated_comment": self.source + b"\n# Unreviewed source drift.\n",
            "read_only_change": self.source.replace(READ_ONLY_ADDITION, READ_ONLY_ADDITION + b'        "unreviewed": True,\n'),
        }
        for label, source in candidates.items():
            self.assertNotEqual(source, self.source)
            with self.subTest(label=label), self.assertRaisesRegex(common.ReleaseError, "drift exceeds"):
                self.verify(source)

    def test_missing_duplicate_or_ambiguous_helper_and_call_site_fail_closed(self):
        helper = next(n for n in ast.parse(self.source).body if isinstance(n, ast.FunctionDef)
                      and n.name == "_verify_fixture_producer_compatibility")
        lines = self.source.splitlines(keepends=True)
        helper_source = b"".join(lines[helper.lineno - 1:helper.end_lineno])
        missing_helper = b"".join(lines[:helper.lineno - 1] + lines[helper.end_lineno + 2:])
        candidates = (
            missing_helper,
            self.source + b"\n\n" + helper_source,
            self.source.replace(CALL, ORIGINAL_CALL),
            self.source.replace(CALL, CALL + CALL),
            self.source.replace(CALL, CALL.rstrip(b"\n") + b"  # ambiguous call\n"),
            self.source.replace(CALL, CALL.replace(b", new, d)", b", old, d)")),
            self.source.replace(b"def _verify_fixture_producer_compatibility(", b"@staticmethod\ndef _verify_fixture_producer_compatibility("),
            self.source.replace(b"\n\ndef verify_fixture_result", b"\n# hidden boundary\ndef verify_fixture_result"),
        )
        for index, source in enumerate(candidates):
            with self.subTest(index=index), self.assertRaises(common.ReleaseError):
                self.verify(source)

    def test_api_source_cannot_supply_a_different_compatibility_helper(self):
        other = self.source.replace(b"    import ast\n", b"    import ast\n    return\n")
        with self.assertRaisesRegex(common.ReleaseError, "executing checkout"):
            fixture._verify_fixture_producer_compatibility(
                CONTROLLER, {"sha": OLD_BLOB}, content(other), self.descriptor)

    def test_api_encoding_hash_and_source_size_are_bound(self):
        for change in ({"sha": "f" * 40}, {"encoding": "none"},
                       {"content": "invalid%"}, {"content": "A" * 350001}):
            with self.subTest(change=list(change)), self.assertRaises(common.ReleaseError):
                fixture._verify_fixture_producer_compatibility(
                    CONTROLLER, {"sha": OLD_BLOB}, {**content(self.source), **change}, self.descriptor)
        for old, new in (({}, {}), ({"sha": None}, {"sha": None})):
            with self.assertRaises(common.ReleaseError):
                fixture._verify_fixture_producer_compatibility(CONTROLLER, old, new, self.descriptor)


class ReceiptAPI:
    """Synthetic in-memory GitHub API; never constructs a transport or token."""

    def __init__(self):
        current, _, old = sources()
        self.current_control = "a" * 40  # No pin to the consuming main SHA.
        self.compare = {"status": "ahead", "merge_base_commit": {"sha": OLD_CONTROL}}
        self.old, self.current = content(old), content(current)
        self.run = {"head_sha": OLD_CONTROL, "head_branch": "main", "path": fixture.WORKFLOW_PATH,
                    "event": "workflow_dispatch", "run_attempt": 1, "status": "completed", "conclusion": "success"}
        self.jobs = [{"name": name, "conclusion": conclusion} for name, conclusion in zip(
            fixture.WORKFLOW_JOB_NAMES, ("success", "skipped", "success", "skipped", "success"))]
        request, protected = samples.inputs()
        request["control_sha"] = OLD_CONTROL
        _, base = fixture.ProductProvisioner(request, protected, samples.FakeTransport(protected), samples.FakeGate()).provision()
        self.result = samples.terminal_receipt(base, protected)
        self.result["origin"]["run_id"] = OLD_RUN
        for burn in self.result["burns"]:
            burn["origin_run_id"] = OLD_RUN
            burn["artifact_name"] = burn["artifact_name"].replace("12345", OLD_RUN)
        fixture.validate_terminal_result(self.result)
        result_bytes = common.canonical_file_bytes(self.result)
        self.descriptor = {"control_sha": OLD_CONTROL, "run_id": OLD_RUN, "artifact_id": "10411885260",
                           "artifact_digest": "sha256:" + "f" * 64,
                           "result_sha256": common.sha256_bytes(result_bytes)}
        archive = fixture.io.BytesIO()
        with fixture.zipfile.ZipFile(archive, "w") as zipped:
            zipped.writestr("result.json", result_bytes)
            zipped.writestr("result.sha256", self.descriptor["result_sha256"] + "\n")
        self.archive = archive.getvalue()
        self.descriptor["artifact_digest"] = "sha256:" + common.sha256_bytes(self.archive)
        self.artifacts = [self.meta(self.descriptor["artifact_id"], "crm-canary-fixture-result-" + OLD_RUN + "-1",
                                    self.descriptor["artifact_digest"])]
        origin = self.result["origin"]
        self.artifacts.append(self.meta(origin["artifact_id"], "crm-canary-fixture-intent-" + OLD_RUN + "-1", origin["artifact_digest"]))
        self.artifacts.extend(self.meta(b["artifact_id"], b["artifact_name"], b["artifact_digest"]) for b in self.result["burns"])
        self.driver = {"schema_version": 1, "url": "https://synthetic.invalid", "driver_version_sha256": "a" * 64,
                       "fixture_descriptor_sha256": self.result["fixture_descriptor_sha256"],
                       "hmac_key_base64": base64.b64encode(b"synthetic-test-key-only" * 2).decode()}

    def meta(self, identity, name, digest):
        return {"id": int(identity), "name": name, "digest": digest, "expired": False, "size_in_bytes": 1000,
                "expires_at": "2099-01-01T00:00:00Z",
                "workflow_run": {"id": int(OLD_RUN), "head_sha": OLD_CONTROL, "head_branch": "main"}}

    def get(self, path):
        if "/compare/" in path:
            return copy.deepcopy(self.compare)
        if "/contents/" in path:
            if fixture.WORKFLOW_PATH in path:
                return {"sha": WORKFLOW_BLOB}
            return copy.deepcopy(self.old if path.endswith("ref=" + OLD_CONTROL) else self.current)
        if path.endswith("/actions/runs/" + OLD_RUN):
            return copy.deepcopy(self.run)
        if path.endswith("/actions/artifacts/" + self.descriptor["artifact_id"]):
            return copy.deepcopy(self.artifacts[0])
        raise AssertionError("unexpected synthetic route: " + path)

    def pages(self, path, key):
        rows = self.jobs if key == "jobs" else self.artifacts
        return {"total_count": len(rows), key: copy.deepcopy(rows)}

    def artifact(self, identity, digest):
        if identity != self.descriptor["artifact_id"] or "sha256:" + common.sha256_bytes(self.archive) != digest:
            raise common.ReleaseError("artifact archive digest differs")
        return self.archive


class FixtureCompatibilityConsumerTests(unittest.TestCase):
    def verify(self, api, *, signature_error=False):
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(fixture.os.environ, {
                "FIXTURE_EVIDENCE_JSON": json.dumps(api.descriptor), "CONTROL_SHA": api.current_control,
                "CRM_CANARY_SYNTHETIC_DRIVER_JSON": json.dumps(api.driver), "RUNNER_TEMP": directory}), \
                mock.patch.object(fixture.time, "sleep"), \
                mock.patch.object(fixture, "_verify_intent_attestations") as attest:
            if signature_error:
                attest.side_effect = common.ReleaseError("signature deliberately rejected")
            result = fixture.verify_fixture_result(api, Path(directory), Path("never-run-gh"))
            attest.assert_called_once()
            self.assertEqual(attest.call_args.kwargs["policy_type"], "https://rereply.app/attestations/crm-canary-fixture-result/v1")
            self.assertEqual(attest.call_args.args[1]["control_sha"], OLD_CONTROL)
            return result

    def test_full_consumer_accepts_compatible_receipt_with_existing_bindings(self):
        api = ReceiptAPI()
        self.assertEqual(self.verify(api), api.result)

    def test_compatibility_never_bypasses_signature_verification(self):
        with self.assertRaisesRegex(common.ReleaseError, "signature deliberately rejected"):
            self.verify(ReceiptAPI(), signature_error=True)

    def test_ancestry_run_executor_artifact_and_runtime_bindings_still_fail_closed(self):
        mutations = {
            "ancestry": lambda api: api.compare.update(status="diverged"),
            "merge_base": lambda api: api.compare["merge_base_commit"].update(sha="b" * 40),
            "rerun": lambda api: api.run.update(run_attempt=2),
            "failed_run": lambda api: api.run.update(conclusion="failure"),
            "wrong_run_source": lambda api: api.run.update(head_sha="b" * 40),
            "executor": lambda api: api.jobs[2].update(conclusion="skipped"),
            "extra_job": lambda api: api.jobs.append({"name": "unexpected", "conclusion": "success"}),
            "expired_artifact": lambda api: api.artifacts[0].update(expired=True),
            "artifact_origin": lambda api: api.artifacts[0]["workflow_run"].update(head_sha="b" * 40),
            "missing_inventory": lambda api: api.artifacts.pop(),
            "content_hash": lambda api: api.descriptor.update(result_sha256="b" * 64),
            "runtime_fixture": lambda api: api.driver.update(fixture_descriptor_sha256="b" * 64),
        }
        for label, mutate in mutations.items():
            api = ReceiptAPI()
            mutate(api)
            with self.subTest(label=label), self.assertRaises(common.ReleaseError):
                self.verify(api)


if __name__ == "__main__":
    unittest.main()
