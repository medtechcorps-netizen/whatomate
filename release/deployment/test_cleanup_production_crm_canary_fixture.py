
from __future__ import annotations
import copy
import datetime as dt
import unittest

try:
    from . import cleanup_production_crm_canary_fixture as cleanup
    from . import provision_production_crm_canary_fixture as fixture
    from .test_provision_production_crm_canary_fixture import origin_intent
except ImportError:
    import cleanup_production_crm_canary_fixture as cleanup
    import provision_production_crm_canary_fixture as fixture
    from test_provision_production_crm_canary_fixture import origin_intent

common=fixture.common
NOW=dt.datetime(2026,9,5,12,tzinfo=dt.timezone.utc)


def evidence_fixture():
    intent=origin_intent()
    run={"id":12345,"run_attempt":1,"head_sha":intent["control_sha"],"head_branch":"main",
         "event":"workflow_dispatch","path":fixture.WORKFLOW_PATH,"status":"completed",
         "conclusion":"success","previous_attempt_url":None,
         "repository":{"full_name":common.REPOSITORY}}
    jobs=[]
    for i,name in enumerate(fixture.WORKFLOW_JOB_NAMES):
        jobs.append({"id":101+i,"run_id":12345,"run_attempt":1,"head_sha":intent["control_sha"],
                     "name":name,"status":"completed",
                     "conclusion":"skipped" if name in (fixture.EXECUTOR_JOB,fixture.WORKFLOW_JOB_NAMES[3]) else "success",
                     "steps":[]})
    artifact={"id":700,"name":"crm-canary-fixture-intent-12345-1","size_in_bytes":2048,
              "expired":False,"digest":"sha256:"+"a"*64,"created_at":"2026-09-05T00:00:10Z",
              "expires_at":"2026-12-01T00:00:00Z",
              "workflow_run":{"id":12345,"head_sha":intent["control_sha"],"head_branch":"main"}}
    envelope={"run":run,"attempt":copy.deepcopy(run),"jobs":{"total_count":len(jobs),"jobs":jobs},
              "artifacts":{"total_count":1,"artifacts":[artifact]}}
    digest=common.sha256_bytes(common.canonical_file_bytes(intent))
    def statement(predicate_type,predicate):
        return [{"verificationResult":{"statement":{"_type":"https://in-toto.io/Statement/v1",
                 "subject":[{"name":"intent.json","digest":{"sha256":digest}}],
                 "predicateType":predicate_type,"predicate":predicate}}}]
    provenance=statement("https://slsa.dev/provenance/v1",{
        "buildDefinition":{"buildType":"https://actions.github.io/buildtypes/workflow/v1",
            "externalParameters":{"workflow":{"repository":"https://github.com/"+common.REPOSITORY,
                       "path":fixture.WORKFLOW_PATH,"ref":"refs/heads/main"}},
            "resolvedDependencies":[{"uri":"git+https://github.com/"+common.REPOSITORY+"@refs/heads/main",
                                     "digest":{"gitCommit":intent["control_sha"]}}]},
        "runDetails":{"builder":{"id":"https://github.com/actions/runner/github-hosted"},
          "metadata":{"invocationId":"https://github.com/"+common.REPOSITORY+"/actions/runs/12345/attempts/1"}}})
    policy=statement(fixture.INTENT_PREDICATE,intent)
    kwargs={"now":NOW,"expected_control_sha":intent["control_sha"],"expected_origin_run_id":"12345",
            "expected_intent_sha256":digest,"expected_origin_artifact_id":"700",
            "expected_origin_artifact_digest":"sha256:"+"a"*64}
    return intent,envelope,provenance,policy,kwargs


class TestFixtureCleanup(unittest.TestCase):
    def report(self,parts):
        return cleanup.build_report(*parts[:4],**parts[4])

    def test_no_executor_and_complete_origin_proves_abort(self):
        result=self.report(evidence_fixture())
        self.assertEqual(result["classification"],"aborted_before_effect")
        self.assertEqual(result["effects_possible_upper_bound"],0)
        self.assertFalse(result["requires_separate_inverse"])

    def test_started_executor_with_zero_burns_is_quarantined(self):
        parts=evidence_fixture()
        executor=next(j for j in parts[1]["jobs"]["jobs"] if j["name"]==fixture.EXECUTOR_JOB)
        executor.update(conclusion="failure",steps=[{"number":1,"name":"start","status":"completed","conclusion":"success"}])
        result=self.report(parts)
        self.assertEqual(result["classification"],"quarantined")
        self.assertEqual(result["effects_possible_upper_bound"],13)
        self.assertTrue(result["requires_separate_inverse"])

    def test_missing_expired_wrong_run_or_incomplete_evidence_never_aborts(self):
        mutations=[
            lambda e:e["artifacts"].update(total_count=0,artifacts=[]),
            lambda e:e["artifacts"]["artifacts"][0].update(expired=True),
            lambda e:e["artifacts"]["artifacts"][0]["workflow_run"].update(id=54321),
            lambda e:e["jobs"].update(total_count=6),
            lambda e:e["jobs"]["jobs"].pop(),
            lambda e:e["run"].update(run_attempt=2),
            lambda e:e["attempt"].update(head_sha="f"*40),
            lambda e:e["run"].update(status="in_progress"),
        ]
        for mutate in mutations:
            parts=evidence_fixture();mutate(parts[1])
            with self.assertRaises(common.ReleaseError):
                self.report(parts)

    def test_burn_without_running_executor_cannot_claim_abort(self):
        parts=evidence_fixture()
        artifact=copy.deepcopy(parts[1]["artifacts"]["artifacts"][0])
        artifact.update(id=701,name="crm-canary-fixture-burn-12345-1-create_account")
        parts[1]["artifacts"]={"total_count":2,"artifacts":[parts[1]["artifacts"]["artifacts"][0],artifact]}
        self.assertEqual(self.report(parts)["classification"],"quarantined")

    def test_wrong_signed_subject_policy_or_workflow_is_rejected(self):
        for lane in (2,3):
            parts=evidence_fixture()
            parts[lane][0]["verificationResult"]["statement"]["subject"][0]["digest"]["sha256"]="f"*64
            with self.assertRaises(common.ReleaseError):
                self.report(parts)
        parts=evidence_fixture()
        parts[2][0]["verificationResult"]["statement"]["predicate"]["runDetails"]["metadata"]["invocationId"] += "2"
        with self.assertRaises(common.ReleaseError):
            self.report(parts)

    def test_controller_has_no_provider_or_deletion_client(self):
        import ast
        tree=ast.parse(fixture.Path(cleanup.__file__).read_text("utf-8"))
        forbidden={"urllib","requests","http","subprocess","socket"}
        for node in ast.walk(tree):
            if isinstance(node,ast.Import):
                self.assertFalse({x.name.split(".")[0] for x in node.names}&forbidden)
            if isinstance(node,ast.ImportFrom):
                self.assertNotIn((node.module or "").split(".")[0],forbidden)
