
from __future__ import annotations
import copy
import datetime as dt
import unittest
from unittest import mock

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
        "runDetails":{"builder":{"id":
            "https://github.com/medtechcorps-netizen/whatomate/.github/workflows/"
            "provision-production-crm-canary-fixture.yml@refs/heads/main"},
          "metadata":{"invocationId":"https://github.com/"+common.REPOSITORY+"/actions/runs/12345/attempts/1"}}})
    policy=statement(fixture.INTENT_PREDICATE,copy.deepcopy(intent))
    kwargs={"now":NOW,"expected_control_sha":intent["control_sha"],"expected_origin_run_id":"12345",
            "expected_intent_sha256":digest,"expected_origin_artifact_id":"700",
            "expected_origin_artifact_digest":"sha256:"+"a"*64}
    return intent,envelope,provenance,policy,kwargs


class TestFixtureCleanup(unittest.TestCase):
    def report(self,parts):
        return cleanup.build_report(*parts[:4],**parts[4])

    def test_current_producer_provenance_and_complete_origin_prove_abort(self):
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

    def test_wrong_builder_authority_is_rejected(self):
        cases=[
            ("wrong-repository","https://github.com/other/whatomate/.github/workflows/provision-production-crm-canary-fixture.yml@refs/heads/main"),
            ("wrong-workflow","https://github.com/medtechcorps-netizen/whatomate/.github/workflows/other.yml@refs/heads/main"),
            ("wrong-branch","https://github.com/medtechcorps-netizen/whatomate/.github/workflows/provision-production-crm-canary-fixture.yml@refs/heads/other"),
            ("tag-ref","https://github.com/medtechcorps-netizen/whatomate/.github/workflows/provision-production-crm-canary-fixture.yml@refs/tags/main"),
            ("legacy-github-hosted","https://github.com/actions/runner/github-hosted"),
            ("legacy-self-hosted","https://github.com/actions/runner/self-hosted"),
        ]
        for label,builder_id in cases:
            with self.subTest(case=label):
                parts=evidence_fixture()
                predicate=parts[2][0]["verificationResult"]["statement"]["predicate"]
                predicate["runDetails"]["builder"]["id"]=builder_id
                with self.assertRaisesRegex(common.ReleaseError,"^fixture verified provenance authority differs$"):
                    self.report(parts)

    def test_wrong_workflow_definition_is_rejected(self):
        cases=[
            ("repository",lambda d:d["externalParameters"]["workflow"].update(repository="https://github.com/other/whatomate")),
            ("workflow",lambda d:d["externalParameters"]["workflow"].update(path=".github/workflows/other.yml")),
            ("ref",lambda d:d["externalParameters"]["workflow"].update(ref="refs/heads/other")),
            ("build-type",lambda d:d.update(buildType="https://actions.github.io/buildtypes/workflow/v2")),
        ]
        for label,mutate in cases:
            with self.subTest(case=label):
                parts=evidence_fixture()
                mutate(parts[2][0]["verificationResult"]["statement"]["predicate"]["buildDefinition"])
                with self.assertRaisesRegex(common.ReleaseError,"^fixture verified provenance authority differs$"):
                    self.report(parts)

    def test_wrong_or_ambiguous_source_is_rejected(self):
        cases=[
            ("repository",lambda d:d["resolvedDependencies"][0].update(uri="git+https://github.com/other/whatomate@refs/heads/main"),"fixture verified provenance authority differs"),
            ("ref",lambda d:d["resolvedDependencies"][0].update(uri="git+https://github.com/medtechcorps-netizen/whatomate@refs/heads/other"),"fixture verified provenance authority differs"),
            ("commit",lambda d:d["resolvedDependencies"][0]["digest"].update(gitCommit="f"*40),"fixture verified provenance authority differs"),
            ("missing",lambda d:d.pop("resolvedDependencies"),"fixture provenance sources differ"),
            ("empty",lambda d:d.update(resolvedDependencies=[]),"fixture provenance sources differ"),
            ("duplicate",lambda d:d["resolvedDependencies"].append(copy.deepcopy(d["resolvedDependencies"][0])),"fixture verified provenance authority differs"),
        ]
        for label,mutate,error in cases:
            with self.subTest(case=label):
                parts=evidence_fixture()
                mutate(parts[2][0]["verificationResult"]["statement"]["predicate"]["buildDefinition"])
                with self.assertRaisesRegex(common.ReleaseError,"^"+error+"$"):
                    self.report(parts)

    def test_wrong_invocation_is_rejected(self):
        cases=[
            ("repository","https://github.com/other/whatomate/actions/runs/12345/attempts/1"),
            ("run","https://github.com/medtechcorps-netizen/whatomate/actions/runs/54321/attempts/1"),
            ("attempt","https://github.com/medtechcorps-netizen/whatomate/actions/runs/12345/attempts/2"),
        ]
        for label,invocation in cases:
            with self.subTest(case=label):
                parts=evidence_fixture()
                predicate=parts[2][0]["verificationResult"]["statement"]["predicate"]
                predicate["runDetails"]["metadata"]["invocationId"]=invocation
                with self.assertRaisesRegex(common.ReleaseError,"^fixture verified provenance authority differs$"):
                    self.report(parts)

    def test_wrong_subject_or_predicate_type_is_rejected_in_both_lanes(self):
        for lane,label in ((2,"provenance"),(3,"policy")):
            for field in ("subject","predicate-type"):
                with self.subTest(lane=label,field=field):
                    parts=evidence_fixture()
                    statement=parts[lane][0]["verificationResult"]["statement"]
                    if field == "subject":
                        statement["subject"][0]["digest"]["sha256"]="f"*64
                    else:
                        statement["predicateType"]="https://example.invalid/other-predicate/v1"
                    with self.assertRaisesRegex(common.ReleaseError,"^fixture verified subject binding differs$"):
                        self.report(parts)

    def test_full_policy_must_match_without_mutating_origin_intent(self):
        cases=[
            ("controller",lambda p:p.update(controller_sha256="f"*64)),
            ("nested-request",lambda p:p["request"].update(operation_sha256="f"*64)),
            ("extra-field",lambda p:p.update(unexpected=True)),
        ]
        for label,mutate in cases:
            with self.subTest(case=label):
                parts=evidence_fixture()
                original_intent=copy.deepcopy(parts[0])
                statement=parts[3][0]["verificationResult"]["statement"]
                statement["predicate"]=copy.deepcopy(statement["predicate"])
                mutate(statement["predicate"])
                self.assertEqual(parts[0],original_intent)
                with self.assertRaisesRegex(common.ReleaseError,"^fixture verified policy differs$"):
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


class TestFixtureAttestationBoundary(unittest.TestCase):
    """Mocks check command construction and fail-closed handling, not cryptography."""

    def expected_commands(self,intent,path,gh):
        flags=[
            "--repo","medtechcorps-netizen/whatomate",
            "--signer-workflow","medtechcorps-netizen/whatomate/.github/workflows/provision-production-crm-canary-fixture.yml",
            "--signer-digest",intent["control_sha"],
            "--source-digest",intent["control_sha"],
            "--source-ref","refs/heads/main",
            "--deny-self-hosted-runners","--format","json",
        ]
        return [[str(gh),"attestation","verify",str(path),*flags,"--predicate-type",predicate]
                for predicate in ("https://slsa.dev/provenance/v1",
                                  "https://rereply.app/attestations/crm-canary-fixture-intent/v1")]

    def test_both_actual_verifier_commands_keep_exact_hosted_runner_policy(self):
        parts=evidence_fixture()
        path,gh=fixture.Path("unused-intent.json"),fixture.Path("unused-gh")
        responses=[mock.Mock(returncode=0,stdout=common.canonical_file_bytes(result))
                   for result in parts[2:4]]
        with mock.patch.object(fixture.subprocess,"run",side_effect=responses) as run:
            result=fixture._verify_intent_attestations(path,parts[0],gh)
        self.assertEqual(result,(parts[2],parts[3]))
        self.assertEqual([call.args[0] for call in run.call_args_list],
                         self.expected_commands(parts[0],path,gh))
        for call in run.call_args_list:
            self.assertEqual(call.kwargs,{"env":mock.ANY,"capture_output":True,"timeout":120,"check":False})

    def test_nonzero_verifier_result_rejects_even_otherwise_valid_stdout(self):
        for failed_call,label in ((0,"provenance"),(1,"policy")):
            with self.subTest(lane=label):
                parts=evidence_fixture()
                path,gh=fixture.Path("unused-intent.json"),fixture.Path("unused-gh")
                responses=[mock.Mock(returncode=0,stdout=common.canonical_file_bytes(result))
                           for result in parts[2:4]]
                responses[failed_call].returncode=1
                with mock.patch.object(fixture.subprocess,"run",side_effect=responses) as run:
                    with self.assertRaisesRegex(common.ReleaseError,"^intent signature verification failed$"):
                        fixture._verify_intent_attestations(path,parts[0],gh)
                self.assertEqual(run.call_count,failed_call+1)
                self.assertEqual([call.args[0] for call in run.call_args_list],
                                 self.expected_commands(parts[0],path,gh)[:failed_call+1])
