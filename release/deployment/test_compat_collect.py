"""Real inherited verifier + bounded Actions transport, synthetic responses only."""
from contextlib import contextmanager
import copy
from dataclasses import replace
from datetime import timedelta
import json
import hashlib
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat_collect as collect
import compat_evidence as evidence
import test_compat_adapter as signature
import test_compat_evidence as fixture
import test_ship_support as support

WORKFLOW = (Path(__file__).resolve().parents[2] / ".github/workflows/compat.yml").read_bytes()
REQUEST = collect.CollectionRequest(fixture.SHA, WORKFLOW)
NOW = fixture.T0 + timedelta(days=1)
W0_WORKFLOW_SHA256 = "0f8ae9f3079d4a3c0749721e5100d6df60842b4811059aee02a2ee4d8ced4f6b"
P_WORKFLOW_SHA256 = "73c655c829fa36a109dfee621f1efa4529cf89ffa112599a8ff1bb8391b7eff9"
P0_WORKFLOW_SHA256 = "56005fe2015e8385e6ecfc621a403c06a534ed9a5712e5ec4ec4f9c70407b941"
P0_WORKFLOW = (Path(__file__).resolve().parent / "fixtures/compat-workflow-p0.yml").read_bytes()
assert hashlib.sha256(P0_WORKFLOW).hexdigest() == P0_WORKFLOW_SHA256
W0_WORKFLOW = (Path(__file__).resolve().parent / "fixtures/compat-workflow-w0.yml").read_bytes()
assert hashlib.sha256(W0_WORKFLOW).hexdigest() == W0_WORKFLOW_SHA256


@contextmanager
def w0_epoch():
    """Construct original custody with fixed old raw source, never a verdict mock.

    Only the exact original current pin and synthetic fixture inputs change.
    Every qualification/codec/verifier stays real. All transition assertions
    run after this scope restores installed P, including on fixture failure.
    """
    import test_compat_rollback_history as old_history
    import test_compat_rollback_collect as old_collector
    import compat_envelope
    global WORKFLOW, REQUEST
    assert hashlib.sha256(W0_WORKFLOW).hexdigest() == W0_WORKFLOW_SHA256
    before = collect.WORKFLOW_SHA256, WORKFLOW, REQUEST
    before_fixture_workflows = old_history.WORKFLOW, old_collector.WORKFLOW
    assert before[0] in (P_WORKFLOW_SHA256, W0_WORKFLOW_SHA256)
    assert compat_envelope.WORKFLOW_SHA256 == P_WORKFLOW_SHA256
    original_read = Path.read_bytes
    source_path = Path(__file__).resolve().parents[2] / '.github/workflows/compat.yml'
    def read_source(path):
        # This only controls the source-fixture's one immutable workflow blob.
        # It does not replace any production resolver/verifier/parser result.
        return W0_WORKFLOW if path == source_path else original_read(path)
    try:
        with mock.patch.object(collect, 'WORKFLOW_SHA256', W0_WORKFLOW_SHA256), \
             mock.patch.object(sys.modules[__name__], 'WORKFLOW', W0_WORKFLOW), \
             mock.patch.object(sys.modules[__name__], 'REQUEST', collect.CollectionRequest(fixture.SHA, W0_WORKFLOW)), \
             mock.patch.object(old_history, 'WORKFLOW', W0_WORKFLOW), \
             mock.patch.object(old_collector, 'WORKFLOW', W0_WORKFLOW), \
             mock.patch.object(Path, 'read_bytes', read_source):
            yield
    finally:
        assert (collect.WORKFLOW_SHA256, WORKFLOW, REQUEST) == before
        assert (old_history.WORKFLOW, old_collector.WORKFLOW) == before_fixture_workflows
        assert Path.read_bytes is original_read
        assert compat_envelope.WORKFLOW_SHA256 == P_WORKFLOW_SHA256


@contextmanager
def p0_epoch():
    """Construct previous-P custody with fixed old raw source, never a verdict mock.

    Only the exact original current pin and synthetic fixture inputs change.
    Every qualification/codec/verifier stays real. All transition assertions
    run after this scope restores installed P, including on fixture failure.
    """
    import test_compat_rollback_history as old_history
    import test_compat_rollback_collect as old_collector
    import compat_envelope
    global WORKFLOW, REQUEST
    assert hashlib.sha256(P0_WORKFLOW).hexdigest() == P0_WORKFLOW_SHA256
    before = collect.WORKFLOW_SHA256, WORKFLOW, REQUEST
    before_fixture_workflows = old_history.WORKFLOW, old_collector.WORKFLOW
    assert before[0] in (P_WORKFLOW_SHA256, P0_WORKFLOW_SHA256)
    assert compat_envelope.WORKFLOW_SHA256 == P_WORKFLOW_SHA256
    original_read = Path.read_bytes
    source_path = Path(__file__).resolve().parents[2] / '.github/workflows/compat.yml'
    def read_source(path):
        # This only controls the source-fixture's one immutable workflow blob.
        # It does not replace any production resolver/verifier/parser result.
        return P0_WORKFLOW if path == source_path else original_read(path)
    try:
        with mock.patch.object(collect, 'WORKFLOW_SHA256', P0_WORKFLOW_SHA256), \
             mock.patch.object(sys.modules[__name__], 'WORKFLOW', P0_WORKFLOW), \
             mock.patch.object(sys.modules[__name__], 'REQUEST', collect.CollectionRequest(fixture.SHA, P0_WORKFLOW)), \
             mock.patch.object(old_history, 'WORKFLOW', P0_WORKFLOW), \
             mock.patch.object(old_collector, 'WORKFLOW', P0_WORKFLOW), \
             mock.patch.object(Path, 'read_bytes', read_source):
            yield
    finally:
        assert (collect.WORKFLOW_SHA256, WORKFLOW, REQUEST) == before
        assert (old_history.WORKFLOW, old_collector.WORKFLOW) == before_fixture_workflows
        assert Path.read_bytes is original_read
        assert compat_envelope.WORKFLOW_SHA256 == P_WORKFLOW_SHA256


def stamp(value):
    return value.isoformat().replace("+00:00", "Z")


class Clock:
    def __init__(self): self.value = 100.0
    def __call__(self): return self.value


class World:
    def __init__(self):
        self.clock = Clock()
        self.calls, self.runs, self.attempts, self.jobs, self.checks, self.artifacts = [], {}, {}, {}, {}, {}
        self.archives, self.pairs, self.signers = {}, {}, {}
        self.workflow_reads = 0
        self.hook = lambda path, value: value
        self.capture_hook = lambda result: result
        self.sign_mutate = lambda value, path: None
        self.verifying = None
        self.steps = collect._workflow(REQUEST)

    def add(self, run=100, number=10, attempts=1, minute=10, *, negative=False, failure=False, event="push"):
        created = fixture.T0 + timedelta(minutes=minute)
        repo = {"id":collect.REPOSITORY_ID,"full_name":evidence.REPOSITORY}
        for attempt in range(1, attempts+1):
            began = created + timedelta(minutes=(attempt-1)*10, seconds=1)
            suite = run * 1000 + attempt
            rows, checks = [], []
            for i, name in enumerate(evidence.JOBS):
                jid, cid = run*10000+attempt*100+i, run*100000+attempt*100+i
                started = began + timedelta(seconds=10+i*20)
                end = started + timedelta(seconds=15)
                conclusion = "failure" if (negative or failure) and name == "compat" else "success"
                steps = [{"number":j,"name":s,"status":"completed","conclusion":"success",
                    "started_at":stamp(started+timedelta(seconds=j)),
                    "completed_at":stamp(started+timedelta(seconds=j))}
                    for j,s in enumerate(["Set up job",*self.steps[name],"Post Check out the exact candidate","Complete job"],1)]
                if conclusion == "failure": steps[-3]["conclusion"] = "failure"
                rows.append({"id":jid,"name":name,"run_id":run,"run_attempt":attempt,"head_sha":fixture.SHA,
                    "status":"completed","conclusion":conclusion,"created_at":stamp(started-timedelta(seconds=1)),
                    "started_at":stamp(started),"completed_at":stamp(end),"steps":steps,
                    "url":collect.API_URL+f"/actions/jobs/{jid}","html_url":collect.WEB+f"/actions/runs/{run}/job/{jid}",
                    "check_run_url":collect.API_URL+f"/check-runs/{cid}"})
                checks.append({"id":cid,"name":name,"app":{"id":15368},"check_suite":{"id":suite},
                    "head_sha":fixture.SHA,"status":"completed","conclusion":conclusion,
                    "started_at":stamp(started),"completed_at":stamp(end),"url":rows[-1]["check_run_url"],
                    "html_url":rows[-1]["html_url"],"details_url":rows[-1]["html_url"]})
            row = {"id":run,"name":"Compatibility","repository":repo,"head_repository":repo,
                "head_sha":fixture.SHA,"head_branch":"main","event":event,"path":evidence.WORKFLOW,
                "workflow_id":900,"run_number":number,"run_attempt":attempt,"check_suite_id":suite,
                "status":"completed","conclusion":"failure" if negative or failure else "success",
                "created_at":stamp(created),"run_started_at":stamp(began),"updated_at":stamp(end),
                "url":collect.API_URL+f"/actions/runs/{run}","html_url":collect.WEB+f"/actions/runs/{run}"}
            self.attempts[run,attempt] = row; self.jobs[run,attempt] = rows; self.checks[suite] = checks
            pair = fixture.pair(run, attempt, server_false="workers" if negative else None, event=event)
            self.pairs[run,attempt] = pair
            if not failure:
                artifact_id = run*100 + attempt
                archive = signature.archive(pair)
                self.archives[artifact_id] = archive, (run,attempt)
                self.artifacts.setdefault(run,[]).append({"id":artifact_id,"name":f"compat-result-{fixture.SHA}-{attempt}",
                    "size_in_bytes":len(archive),"expired":False,"created_at":stamp(began+timedelta(seconds=69)),
                    "updated_at":stamp(began+timedelta(seconds=69)),"expires_at":stamp(NOW+timedelta(days=6)),
                    "workflow_run":{"id":run,"repository_id":collect.REPOSITORY_ID,"head_repository_id":collect.REPOSITORY_ID,
                        "head_sha":fixture.SHA,"head_branch":"main"},
                    "url":collect.API_URL+f"/actions/artifacts/{artifact_id}",
                    "archive_download_url":collect.API_URL+f"/actions/artifacts/{artifact_id}/zip"})
            self.signers[run,attempt] = signature.SyntheticGh(fixture.attempt(run, number, attempt, event=event))
        self.runs[run] = self.attempts[run,attempts]
        self.artifacts.setdefault(run, [])

    def capture(self, argv, **options):
        self.calls.append((list(argv),options)); self.clock.value += 0.001
        if argv[1:] == ["--version"]:
            result = subprocess.CompletedProcess(argv,0,b"gh version 2.98.0 (fake)\n",b"")
        elif argv[1:3] == ["attestation","verify"]:
            signer = self.signers[self.verifying]
            signer.mutate = self.sign_mutate
            result = signer(argv, **options)
        else:
            assert argv[1:4] == ["api","--method","GET"]
            path = argv[4]; relative = path.removeprefix(collect.API)
            if relative.endswith("/zip"):
                identity = int(relative.split("/")[-2])
                raw, self.verifying = self.archives[identity]
                result = subprocess.CompletedProcess(argv,0,raw,b"")
            else:
                from urllib.parse import parse_qs, urlsplit
                url = urlsplit(relative); pieces = url.path.strip("/").split("/")
                query = parse_qs(url.query)
                if pieces == ["actions","workflows","compat.yml"]:
                    self.workflow_reads += 1
                    value = {"id":900,"name":"Compatibility","path":evidence.WORKFLOW,"state":"active"}
                elif pieces[:2] == ["actions","workflows"]:
                    rows = [r for r in self.runs.values() if r['event']==query['event'][0]]
                    page=int(query['page'][0]); value={"total_count":len(rows),"workflow_runs":rows[(page-1)*100:page*100]}
                elif pieces[:2] == ["actions","runs"] and len(pieces)==5:
                    value = self.attempts[int(pieces[2]),int(pieces[4])]
                elif pieces[:2] == ["actions","runs"] and pieces[-1]=="jobs":
                    rows=self.jobs[int(pieces[2]),int(pieces[4])];value={"total_count":len(rows),"jobs":rows}
                elif pieces[:1] == ["check-suites"]:
                    rows=self.checks[int(pieces[1])];value={"total_count":len(rows),"check_runs":rows}
                elif pieces[:2] == ["actions","runs"] and pieces[-1]=="artifacts":
                    rows=self.artifacts[int(pieces[2])];value={"total_count":len(rows),"artifacts":rows}
                else: raise AssertionError("Unmodeled endpoint")
                value = self.hook(path, copy.deepcopy(value))
                result = subprocess.CompletedProcess(argv,0,json.dumps(value).encode(),b"")
        return self.capture_hook(result)

    def set_job(self, run, attempt, name, **changes):
        job=next(j for j in self.jobs[run,attempt] if j['name']==name)
        job.update(changes)
        check=next(c for c in self.checks[self.attempts[run,attempt]['check_suite_id']] if c['name']==name)
        check.update({k:v for k,v in changes.items() if k in ('status','conclusion','started_at','completed_at')})


@contextmanager
def harness(world=None, seconds=300):
    world=world or World()
    if not world.runs: world.add()
    with tempfile.TemporaryDirectory() as directory:
        root=Path(directory).resolve(); config=root/'gh';scratch=root/'subjects'
        config.mkdir();scratch.mkdir()
        client=collect.ActionsGh('synthetic-gh',support.GH_TOKEN,config,
            seconds=seconds,base_env={},capture=world.capture,clock=world.clock)
        yield world,client,scratch
        assert not any(scratch.iterdir()), 'Verifier-owned directories leaked'


def execute(client,scratch):
    return collect.collect_history(REQUEST,client,scratch,wall_clock=lambda:NOW)


def qualify(collected):
    return evidence.require_compat(replace(fixture.request(),observation_id=collected.history.observation_id),collected.history)


class CollectorTests(unittest.TestCase):
    def test_real_gh_verifier_positive_dispatch_and_push(self):
        for event in ('push','workflow_dispatch'):
            world=World();world.add(event=event)
            with self.subTest(event=event),harness(world) as (w,c,s):
                result=execute(c,s);qualified=qualify(result)
                self.assertEqual(qualified.run_id,100)
                self.assertEqual(result.history.enumeration,'complete-visible')
                self.assertEqual(result.history.whole_run_deletion_coverage,'unavailable')
                verifies=[a for a,_ in w.calls if a[1:3]==['attestation','verify']]
                self.assertEqual(len(verifies),2)
                self.assertTrue(all('--cert-oidc-issuer' in a and '--deny-self-hosted-runners' in a for a in verifies))
                self.assertEqual(w.workflow_reads,2)
                self.assertTrue(all(k['timeout']<=60 for _,k in w.calls))

    def test_complete_negative_survives_failed_whole_run(self):
        world=World();world.add(negative=True)
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertIsInstance(result.history.attempts[0].measurement,evidence.AuthenticatedPair)
            with self.assertRaisesRegex(evidence.CompatRefusal,'compat-old-server-incompatible'): qualify(result)
            self.assertEqual(evidence.assess_bypass_history(fixture.SHA,result.history).state,
                             evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_failed_known_skipped_dependants_do_not_globally_veto_later_dispatch(self):
        world=World();world.add(failure=True)
        world.set_job(100,1,'release-preview',conclusion='failure')
        world.set_job(100,1,'compat-run',conclusion='skipped',steps=[])
        world.set_job(100,1,'compat-attest',conclusion='skipped',steps=[])
        world.add(200,20,minute=60,event='workflow_dispatch')
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertIsNotNone(result.history.attempts[0].completed_at)
            self.assertEqual(qualify(result).run_id,200)

    def test_skipped_job_is_never_success_evidence(self):
        world=World();world.add();world.set_job(100,1,'compat-attest',conclusion='skipped',steps=[])
        with harness(world) as (w,c,s):
            with self.assertRaises(evidence.CompatRefusal): qualify(execute(c,s))

    def test_actual_shaped_skipped_inverted_interval_does_not_create_unknown_work(self):
        world=World();world.add(failure=True)
        skipped=world.jobs[100,1][-1]
        start=collect._date(skipped['started_at'])
        world.set_job(100,1,'compat-attest',conclusion='skipped',steps=[],
            started_at=stamp(start),completed_at=stamp(start-timedelta(seconds=1)))
        world.add(200,20,minute=60,event='workflow_dispatch')
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertEqual(result.history.attempts[0].completed_at,
                             collect._date(world.jobs[100,1][2]['completed_at']))
            self.assertEqual(qualify(result).run_id,200)
            self.assertEqual(next(j for j in result.history.attempts[0].jobs if j.name=='compat-attest').conclusion,'skipped')

    def test_reused_attempt_label_and_new_created_time_do_not_prove_execution(self):
        world=World();world.add(attempts=2)
        original=copy.deepcopy(world.jobs[100,1][0])
        world.set_job(100,2,'release-preview',started_at=original['started_at'],completed_at=original['completed_at'],steps=original['steps'])
        with harness(world) as (w,c,s):
            result=execute(c,s);rerun=result.history.attempts[1]
            self.assertIsNone(rerun.requested_at)
            self.assertIsNone(rerun.completed_at)
            self.assertNotIn('release-preview',[j.name for j in rerun.jobs])
            with self.assertRaises(evidence.CompatRefusal): qualify(result)

    def test_fresh_rerun_qualifies_from_actual_execution_not_unknown_request(self):
        world=World();world.add(attempts=2)
        with harness(world) as (w,c,s):
            result=execute(c,s)
            rerun=result.history.attempts[1];qualified=qualify(result)
            self.assertIsNone(rerun.requested_at)
            self.assertEqual(qualified.run_attempt,2)
            self.assertIsNone(qualified.requested_at)
            self.assertEqual(qualified.started_at,collect._date(w.attempts[100,2]['run_started_at']))
            self.assertEqual(qualified.execution_window,rerun.execution_window)
            self.assertTrue(rerun.execution_window.fresh)
            self.assertEqual(rerun.execution_window.started_at,min(collect._date(j['started_at']) for j in w.jobs[100,2]))
            self.assertEqual(rerun.execution_window.completed_at,max(collect._date(j['completed_at']) for j in w.jobs[100,2]))
            self.assertLess(qualified.started_at,qualified.execution_window.started_at)

    def test_same_run_fresh_rerun_recovers_after_complete_negative_but_bypass_keeps_it(self):
        world=World();world.add(attempts=2)
        world.attempts[100,1]['conclusion']='failure'
        world.set_job(100,1,'compat',conclusion='failure')
        world.archives[10001]=(signature.archive(fixture.pair(100,1,server_false='workers')),(100,1))
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertEqual(qualify(result).run_attempt,2)
            self.assertEqual(evidence.assess_bypass_history(fixture.SHA,result.history).state,
                             evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_success_label_with_failed_or_missing_step_cannot_create_window(self):
        for mode in ('failed','missing'):
            world=World();world.add();world.add(200,20,minute=60)
            rows=world.jobs[100,1][0]['steps']
            if mode=='failed':rows[1]['conclusion']='failure'
            else:rows.pop(1)
            with self.subTest(mode=mode),harness(world) as (w,c,s):
                result=execute(c,s)
                self.assertIsNone(result.history.attempts[0].execution_window)
                with self.assertRaises(evidence.CompatRefusal):qualify(result)

    def test_fully_accounted_cancelled_work_can_be_superseded_without_fabricating_skipped_times(self):
        for executed_name in evidence.JOBS:
            world=World();world.add(failure=True)
            world.runs[100]['conclusion']='cancelled'
            for name in evidence.JOBS:
                if name==executed_name:
                    world.set_job(100,1,name,conclusion='cancelled')
                else:
                    world.set_job(100,1,name,conclusion='skipped',steps=[],
                        started_at=stamp(NOW-timedelta(seconds=1)),completed_at=stamp(NOW-timedelta(seconds=2)))
            world.add(200,20,minute=60,event='workflow_dispatch')
            with self.subTest(executed=executed_name),harness(world) as (w,c,s):
                result=execute(c,s);cancelled=result.history.attempts[0]
                actual=next(j for j in w.jobs[100,1] if j['name']==executed_name)
                self.assertEqual(len(cancelled.jobs),4)
                self.assertEqual(cancelled.execution_window,evidence.ExecutionWindow(
                    collect._date(actual['started_at']),collect._date(actual['completed_at']),True))
                self.assertEqual(qualify(result).run_id,200)

    def test_cancelled_without_actual_work_or_with_reused_work_stays_unknown(self):
        for mode in ('none','reused','missing'):
            world=World();world.add(failure=True)
            world.runs[100]['conclusion']='cancelled'
            original=copy.deepcopy(world.jobs[100,1][0])
            for name in evidence.JOBS:world.set_job(100,1,name,conclusion='skipped',steps=[])
            if mode=='reused':
                for step in original['steps']:
                    for key in ('started_at','completed_at'):
                        step[key]=stamp(collect._date(step[key])-timedelta(minutes=1))
                world.set_job(100,1,'release-preview',conclusion='cancelled',steps=original['steps'],
                    started_at=stamp(collect._date(original['started_at'])-timedelta(minutes=1)),
                    completed_at=stamp(collect._date(original['completed_at'])-timedelta(minutes=1)))
            if mode=='missing':
                world.jobs[100,1].pop();world.checks[100001].pop()
            world.add(200,20,minute=60,event='workflow_dispatch')
            with self.subTest(mode=mode),harness(world) as (w,c,s):
                result=execute(c,s)
                self.assertIsNone(result.history.attempts[0].execution_window)
                with self.assertRaises(evidence.CompatRefusal):qualify(result)

    def test_cancellation_skip_allowance_requires_terminal_cancelled_attempt_and_empty_steps(self):
        for mode in ('failure','active','nonempty'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                w.runs[100]['conclusion']='cancelled'
                changes={'conclusion':'skipped'}
                if mode!='nonempty':changes['steps']=[]
                w.set_job(100,1,'compat',**changes)
                if mode=='failure':w.runs[100]['conclusion']='failure'
                elif mode=='active':w.runs[100].update(status='in_progress',conclusion=None)
                with self.assertRaises(evidence.CompatRefusal):execute(c,s)

    def test_cancelled_skip_keeps_exact_check_publisher_and_identity_requirements(self):
        with harness() as (w,c,s):
            w.runs[100]['conclusion']='cancelled'
            w.set_job(100,1,'compat',conclusion='skipped',steps=[])
            next(j for j in w.checks[100001] if j['name']=='compat')['app']['id']=1
            with self.assertRaises(evidence.CompatRefusal):execute(c,s)

    def test_same_run_reversed_processing_boundaries_refuse_even_if_jobs_are_fresh(self):
        world=World();world.add(attempts=2)
        world.attempts[100,2]['run_started_at']=world.attempts[100,1]['created_at']
        with harness(world) as (w,c,s):
            with self.assertRaises(evidence.CompatRefusal):execute(c,s)

    def test_same_run_reversed_fresh_windows_refuse_despite_ordered_processing_starts(self):
        world=World();world.add(attempts=2)
        shift=timedelta(minutes=30)
        for job in world.jobs[100,1]:
            for step in job['steps']:
                for key in ('started_at','completed_at'):
                    step[key]=stamp(collect._date(step[key])+shift)
            world.set_job(100,1,job['name'],**{
                key:stamp(collect._date(job[key])+shift)
                for key in ('created_at','started_at','completed_at')})
        world.attempts[100,1]['updated_at']=stamp(collect._date(world.attempts[100,1]['updated_at'])+shift)
        for key in ('created_at','updated_at'):
            world.artifacts[100][0][key]=stamp(collect._date(world.artifacts[100][0][key])+shift)
        self.assertLess(world.attempts[100,1]['run_started_at'],world.attempts[100,2]['run_started_at'])
        with harness(world) as (w,c,s):
            with self.assertRaisesRegex(evidence.CompatRefusal,'compat-stale'):execute(c,s)

    def test_authentic_negative_retained_when_second_inventory_changes(self):
        world=World();world.add(negative=True)
        def change(path,value):
            if world.workflow_reads==2 and path.endswith('/attempts/1'):
                value['updated_at']=stamp(NOW)
            return value
        world.hook=change
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertEqual(result.history.enumeration,'incomplete')
            self.assertIsInstance(result.history.attempts[0].measurement,evidence.AuthenticatedPair)
            self.assertEqual(evidence.assess_bypass_history(fixture.SHA,result.history).state,evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_each_identity_field_or_unknown_workflow_refuses(self):
        mutations=[('head_sha',fixture.OTHER),('head_branch','other'),('event','pull_request'),
            ('path','.github/workflows/ship.yml'),('workflow_id',901),('run_attempt',True),
            ('url','https://private.invalid/secret'),('repository',{'id':1,'full_name':evidence.REPOSITORY})]
        for key,value in mutations:
            with self.subTest(key=key),harness() as (w,c,s):
                if key=='event':
                    def foreign_event(path,row):
                        if 'workflow_runs' in row:
                            for run in row['workflow_runs']:run['event']='pull_request'
                        return row
                    w.hook=foreign_event
                else:w.runs[100][key]=value
                with self.assertRaises(evidence.CompatRefusal) as caught: execute(c,s)
                self.assertNotIn('private',str(caught.exception))
                self.assertFalse(any('/zip' in a for args,_ in w.calls for a in args))

    def test_missing_or_modified_reviewed_workflow_refuses_before_requests(self):
        for raw in (b'',WORKFLOW+b'\n',WORKFLOW.replace(b'evidence_ready',b'other_ready')):
            with harness() as (w,c,s):
                with self.assertRaises(evidence.CompatRefusal):
                    collect.collect_history(collect.CollectionRequest(fixture.SHA,raw),c,s,wall_clock=lambda:NOW)
                self.assertEqual(len(w.calls),1)

    def test_check_app_suite_identity_and_times_bound(self):
        for key,value in [('app',{'id':1}),('check_suite',{'id':1}),('head_sha',fixture.OTHER),
                          ('name','other'),('started_at',stamp(fixture.T0)),('details_url','https://private.invalid/')]:
            with self.subTest(key=key),harness() as (w,c,s):
                w.checks[100001][0][key]=value
                with self.assertRaises(evidence.CompatRefusal): execute(c,s)

    def test_missing_jobs_cannot_derive_complete_envelope(self):
        with harness() as (w,c,s):
            w.jobs[100,1].pop();w.checks[100001].pop()
            result=execute(c,s)
            self.assertIsNone(result.history.attempts[0].completed_at)
            with self.assertRaises(evidence.CompatRefusal): qualify(result)

    def test_job_steps_bind_reviewed_names_order_and_complete_execution(self):
        for mutation in ('unknown','order','out-of-period','failed-success-job'):
            with self.subTest(mutation=mutation),harness() as (w,c,s):
                rows=w.jobs[100,1][0]['steps']
                if mutation=='unknown': rows[1]['name']='PRIVATE unknown'
                elif mutation=='order': rows[1]['name'],rows[2]['name']=rows[2]['name'],rows[1]['name']
                elif mutation=='out-of-period': rows[1]['started_at']=stamp(fixture.T0)
                else: rows[1]['conclusion']='failure'
                with self.assertRaises(evidence.CompatRefusal): qualify(execute(c,s))

    def test_active_attempt_never_qualifies(self):
        with harness() as (w,c,s):
            w.runs[100].update(status='in_progress',conclusion=None)
            w.artifacts[100]=[]
            result=execute(c,s)
            self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.ACTIVE)
            self.assertIsNone(result.history.attempts[0].completed_at)
            with self.assertRaises(evidence.CompatRefusal): qualify(result)

    def test_old_attempt_gap_is_not_silently_omitted(self):
        world=World();world.add(attempts=2)
        def alias(path,value):
            if path.endswith('/attempts/1'): return world.attempts[100,2]
            return value
        world.hook=alias
        with harness(world) as (w,c,s):
            with self.assertRaises(evidence.CompatRefusal): execute(c,s)

    def test_missing_expired_and_verification_failed_remain_unknown(self):
        for mode in ('missing','expired','signature','wrong-attempt'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                if mode=='missing': w.artifacts[100]=[]
                elif mode=='expired': w.artifacts[100][0]['expired']=True
                elif mode=='signature': w.signers[100,1].fail_name='compat-context.json'
                else:
                    w.sign_mutate=lambda v,p:v[0]['verificationResult']['signature']['certificate'].update(runInvocationURI='PRIVATE old attempt')
                result=execute(c,s)
                self.assertIsInstance(result.history.attempts[0].measurement,evidence.MissingEvidence)
                with self.assertRaises(evidence.CompatRefusal): qualify(result)

    def test_canonical_context_and_result_must_match_actual_run(self):
        with harness() as (w,c,s):
            w.archives[10001]=(signature.archive(fixture.pair(run=999)),(100,1))
            result=execute(c,s)
            self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.UNKNOWN)

    def test_same_named_artifact_foreign_run_or_duplicate_refuses(self):
        for mode in ('run','duplicate','name','oversized'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                row=w.artifacts[100][0]
                if mode=='run': row['workflow_run']['id']=999
                elif mode=='duplicate': w.artifacts[100].append(dict(row,id=20001))
                elif mode=='name': row['name']=row['name'].replace('-1','-2')
                else: row['size_in_bytes']=65537
                with self.assertRaises(evidence.CompatRefusal): execute(c,s)

    def test_no_optional_api_digest_representation_assumption(self):
        with harness() as (w,c,s):
            w.artifacts[100][0]['digest']='sha256:'+'f'*64
            self.assertTrue(qualify(execute(c,s)).forward)

    def test_metadata_timestamps_and_types_are_strict(self):
        for field,value in [('created_at','tomorrow'),('created_at',True),('updated_at','2099-01-01T00:00:00Z'),
                            ('run_started_at','2026-10-11T00:00:00')]:
            with self.subTest(field=field,value=value),harness() as (w,c,s):
                w.runs[100][field]=value
                with self.assertRaises(evidence.CompatRefusal): execute(c,s)

    def test_collector_and_download_are_once_only(self):
        with harness() as (w,c,s):
            execute(c,s)
            with self.assertRaises(evidence.CompatRefusal): execute(c,s)
            with self.assertRaises(evidence.CompatRefusal):
                c._run(['api','--method','GET',collect.API+'/actions/artifacts/10001/zip','--allow-escape-sequences'],code='unused')
            self.assertEqual(sum(args[-1]=='--allow-escape-sequences' for args,_ in w.calls),1)

    def test_command_grammar_rejects_mutation_urls_pagination_and_arbitrary_verify(self):
        commands=[['api','--method','POST',collect.API+'/actions/runs/100/rerun'],
            ['api','--paginate','--slurp',collect.API+'/actions/runs'],
            ['api','--method','GET','https://private.invalid/',*collect.HEADERS],
            ['attestation','verify','PRIVATE', '--format','json'],
            ['api','--method','GET',collect.API+'/actions/artifacts/999/zip','--allow-escape-sequences']]
        with harness() as (w,c,s):
            for args in commands:
                with self.subTest(args=args),self.assertRaises(evidence.CompatRefusal): c._run(args,code='unused')
            self.assertEqual(len(w.calls),1)

    def test_fixed_private_errors_and_bounded_capture_options(self):
        with harness() as (w,c,s):
            w.capture_hook=lambda p:subprocess.CompletedProcess(p.args,1,b'PRIVATE stdout',b'PRIVATE secret')
            with self.assertRaises(evidence.CompatRefusal) as caught: execute(c,s)
            self.assertNotIn('PRIVATE',str(caught.exception))
            self.assertTrue(all(options['stderr_limit']<=65536 for _,options in w.calls))
            self.assertTrue(all(options['env']['GH_HOST']=='github.com' for _,options in w.calls))

    def test_deadline_late_output_and_command_caps_refuse(self):
        for mode in ('deadline','backward','count','bytes'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                if mode=='deadline': w.clock.value+=301
                elif mode=='backward': w.clock.value-=1
                elif mode=='count': c._commands=collect.MAX_COMMANDS
                else: c._bytes=collect.MAX_BYTES
                with self.assertRaises(evidence.CompatRefusal): execute(c,s)
                self.assertEqual(len(w.calls),1)
        with harness() as (w,c,s):
            def late(result): w.clock.value+=301;return result
            w.capture_hook=late
            with self.assertRaises(evidence.CompatRefusal): execute(c,s)

    def test_partial_second_pass_keeps_verified_negative_when_budget_expires(self):
        world=World();world.add(negative=True)
        def expire(path,value):
            if world.workflow_reads==2:world.clock.value+=301
            return value
        world.hook=expire
        with harness(world) as (w,c,s):
            result=execute(c,s)
            self.assertIsInstance(result.history.attempts[0].measurement,evidence.AuthenticatedPair)
            self.assertEqual(result.history.enumeration,'incomplete')
            self.assertIn('collection-deadline',result.issues)

    def test_json_duplicate_nonfinite_and_zip_bounds(self):
        for raw in (b'{"id":1,"id":2}',b'{"total_count":NaN}'):
            with harness() as (w,c,s):
                w.capture_hook=lambda p:subprocess.CompletedProcess(p.args,0,raw,b'')
                with self.assertRaises(evidence.CompatRefusal): execute(c,s)
        with harness() as (w,c,s):
            w.archives[10001]=(b'x'*65537,(100,1))
            result=execute(c,s)
            self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.UNKNOWN)

    def test_artifact_must_be_created_within_its_actual_attempt(self):
        with harness() as (w,c,s):
            w.artifacts[100][0]['created_at']=w.runs[100]['created_at']
            result=execute(c,s)
            self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.UNKNOWN)
            self.assertFalse(any(args[-1]=='--allow-escape-sequences' for args,_ in w.calls))

    def test_artifact_and_job_changes_at_final_barrier_refuse_positive(self):
        for mode in ('artifact','job','run','workflow'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                def change(path,row):
                    if w.workflow_reads==2:
                        if mode=='artifact' and 'artifacts' in row:row['artifacts'][0]['expired']=True
                        elif mode=='job' and 'jobs' in row:row['jobs'][0]['conclusion']='failure'
                        elif mode=='run' and 'workflow_runs' in row:
                            row['workflow_runs']=[];row['total_count']=0
                        elif mode=='workflow' and path.endswith('/compat.yml'):row['state']='disabled_manually'
                    return row
                w.hook=change
                result=execute(c,s)
                self.assertEqual(result.history.enumeration,'incomplete')
                with self.assertRaises(evidence.CompatRefusal):qualify(result)

    def test_later_known_failed_or_active_execution_vetoes_earlier_positive(self):
        for active in (False,True):
            world=World();world.add();world.add(200,20,minute=60,failure=True)
            if active:world.runs[200].update(status='in_progress',conclusion=None)
            with self.subTest(active=active),harness(world) as (w,c,s):
                with self.assertRaises(evidence.CompatRefusal):qualify(execute(c,s))

    def test_publication_of_one_signed_file_cannot_create_a_pair(self):
        with harness() as (w,c,s):
            w.signers[100,1].fail_name='compat-context.json'
            result=execute(c,s)
            self.assertEqual(len(w.signers[100,1].calls),2)
            self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.UNKNOWN)
            self.assertIsNone(c._scope);self.assertIsNone(c._verification_args)

    def test_both_subject_invocations_and_result_digest_checked(self):
        for name in ('compat-result.json','compat-context.json'):
            with self.subTest(name=name),harness() as (w,c,s):
                def mismatch(value,path):
                    if path.name==name:
                        value[0]['verificationResult']['statement']['subject'][0]['digest']['sha256']='f'*64
                w.sign_mutate=mismatch
                result=execute(c,s)
                self.assertEqual(result.history.attempts[0].measurement,evidence.MissingEvidence.UNKNOWN)

    def test_capture_stream_and_aggregate_caps_even_with_untrusted_test_transport(self):
        for mode in ('stdout','stderr','aggregate'):
            with self.subTest(mode=mode),harness() as (w,c,s):
                if mode=='aggregate':c._bytes=collect.MAX_BYTES-1
                else:
                    w.capture_hook=lambda p:subprocess.CompletedProcess(p.args,0,
                        b'x'*(4*1024*1024+1) if mode=='stdout' else p.stdout,
                        b'x'*65537 if mode=='stderr' else b'')
                with self.assertRaises(evidence.CompatRefusal):execute(c,s)

    def test_combined_capture_allowances_reserve_stderr_inside_aggregate_budget(self):
        with harness() as (w,c,s):
            c._bytes=collect.MAX_BYTES-100000
            c.get(collect.API+'/actions/workflows/compat.yml')
            options=w.calls[-1][1]
            self.assertEqual(options['stderr_limit'],collect.STDERR_LIMIT)
            self.assertEqual(options['stdout_limit']+options['stderr_limit'],100000)
            count=len(w.calls);c._bytes=collect.MAX_BYTES-collect.STDERR_LIMIT
            with self.assertRaises(evidence.CompatRefusal):c.get(collect.API+'/actions/workflows/compat.yml')
            self.assertEqual(len(w.calls),count)

    def test_capture_cleanup_tail_reserved_inside_shared_deadline(self):
        with harness() as (w,c,s):
            w.clock.value=c._deadline-3
            c.get(collect.API+'/actions/workflows/compat.yml')
            self.assertAlmostEqual(w.calls[-1][1]['timeout'],3-collect.CLEANUP_RESERVE)
            count=len(w.calls);w.clock.value=c._deadline-collect.CLEANUP_RESERVE
            with self.assertRaises(evidence.CompatRefusal):c.get(collect.API+'/actions/workflows/compat.yml')
            self.assertEqual(len(w.calls),count)

    def test_config_identity_and_private_scope_refuse_arbitrary_file(self):
        with harness() as (w,c,s):
            with self.assertRaises(evidence.CompatRefusal):c._verify('PRIVATE',[],{},code='unused')
            with mock.patch.object(collect,'_directory',return_value=(-1,-1)):
                with self.assertRaises(evidence.CompatRefusal):execute(c,s)
            self.assertEqual(len(w.calls),1)

    def test_no_whole_run_deletion_coverage_even_when_visible_history_empty(self):
        with harness() as (w,c,s):
            w.runs.clear()
            result=execute(c,s)
            self.assertEqual(result.history.attempts,())
            self.assertEqual(evidence.assess_bypass_history(fixture.SHA,result.history).state,
                evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE)


class PaginationTests(unittest.TestCase):
    def test_exact_full_page_uses_bounded_empty_sentinel(self):
        class Pages:
            def __init__(self):self.paths=[]
            def get(self,path):
                self.paths.append(path)
                page=int(path.rsplit('=',1)[1])
                return {'total_count':100,'rows':[{'id':i} for i in range(1,101)] if page==1 else []}
        client=Pages();rows=collect._pages(client,'/fixed?filter=all','rows',100)
        self.assertEqual(len(rows),100);self.assertEqual(len(client.paths),2)

    def test_maximum_500_entries_is_complete_with_sixth_empty_page(self):
        class Pages:
            def __init__(self):self.calls=0
            def get(self,path):
                self.calls+=1;page=int(path.rsplit('=',1)[1])
                return {'total_count':500,'rows':[{'id':i} for i in range((page-1)*100+1,min(page*100,500)+1)]}
        client=Pages()
        self.assertEqual(len(collect._pages(client,'/fixed?','rows',500)),500)
        self.assertEqual(client.calls,6)

    def test_page_duplicate_count_drift_truncation_and_bound(self):
        for mode in ('duplicate','drift','truncated','oversize','overbound'):
            class Pages:
                def get(self,path):
                    page=int(path.rsplit('=',1)[1]);total=101
                    rows=[{'id':i} for i in range(1,101)] if page==1 else [{'id':101}]
                    if mode=='duplicate' and page==2:rows=[{'id':1}]
                    elif mode=='drift' and page==2:total=102
                    elif mode=='truncated' and page==2:rows=[]
                    elif mode=='oversize':rows=[{'id':i} for i in range(1,102)]
                    elif mode=='overbound':total=501
                    return {'total_count':total,'rows':rows}
            with self.subTest(mode=mode),self.assertRaises(evidence.CompatRefusal):collect._pages(Pages(),'/fixed?','rows',500)

    def test_sum_of_attempts_bound_not_just_run_count(self):
        with harness() as (w,c,s):
            w.runs[100]['run_attempt']=500
            w.add(200,20)
            with self.assertRaises(evidence.CompatRefusal):execute(c,s)
            self.assertFalse(any('/attempts/' in str(a) for a,_ in w.calls))


class PureEnvelopeParityTests(unittest.TestCase):
    def test_actual_normal_collection_and_pure_replay_keep_the_same_windows(self):
        from dataclasses import replace
        import compat_envelope as envelope
        for failure in (False, True):
            world = World()
            world.add(event='workflow_dispatch', negative=failure)
            with self.subTest(failure=failure), harness(world) as (w,c,s):
                collected = execute(c,s)
                original = collected.history.attempts[0]
                run = copy.deepcopy(w.attempts[100,1])
                run['latest_run_attempt'] = 1
                raw = fixture.canonical({'version':1, 'run':run,
                    'jobs':sorted(w.jobs[100,1],key=lambda v:v['id']),
                    'checks':sorted(w.checks[run['check_suite_id']],key=lambda v:v['id']),
                    'artifacts':sorted(w.artifacts[100],key=lambda v:v['id'])})
                replayed = envelope.replay(envelope.WorkflowObservation(fixture.SHA,WORKFLOW),
                    envelope.EnvelopeFacts(raw), observed_at=collected.history.observed_at)
                self.assertEqual(replace(replayed.attempt,measurement=original.measurement),original)


class NormalGetPolicyTests(unittest.TestCase):
    def test_normal_client_still_refuses_all_head_inventory_before_capture(self):
        with harness() as (world, client, scratch):
            before = len(world.calls)
            with self.assertRaises(evidence.CompatRefusal):
                client.get(collect.API + '/actions/workflows/900/runs?branch=main&event=push&per_page=100&page=1')
            self.assertEqual(len(world.calls), before)

    def test_fixed_normal_query_and_arbitrary_url_boundary_remain(self):
        with harness() as (world, client, scratch):
            rows = client.get(collect.API + f'/actions/workflows/900/runs?head_sha={fixture.SHA}&branch=main&event=push&per_page=100&page=1')
            self.assertEqual(len(rows['workflow_runs']), 1)
            before = len(world.calls)
            for path in ('https://PRIVATE.invalid', collect.API + '/actions/runs/100/jobs',
                         collect.API + '/actions/workflows/900/runs?per_page=100&page=1'):
                with self.assertRaises(evidence.CompatRefusal): client.get(path)
            self.assertEqual(len(world.calls), before)


if __name__=='__main__':
    unittest.main()


class OutageCollectorTests(unittest.TestCase):
    """The ordinary synthetic command world still performs real pair verification."""
    def retained(self, world=None, *, empty=False, moments=None):
        with harness(world) as (world, client, scratch):
            if empty:
                world.runs.clear()
            clock = (lambda: next(moments)) if moments is not None else lambda:NOW
            result = collect.collect_outage_history(REQUEST, client, scratch, wall_clock=clock)
            self.assertEqual(world.workflow_reads, 2)
            self.assertEqual(result.normal.command_count, len(world.calls))
            return result

    def request(self, value):
        return replace(fixture.request(), observation_id=value.normal.history.observation_id)

    def missing(self, value):
        return collect.require_missing_expired(self.request(value), value, workflow=WORKFLOW)

    def invalid(self, value, detail=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            collect.validate_outage_history(value, workflow=WORKFLOW, producer_sha=fixture.SHA)
        if detail is not None:
            self.assertEqual(caught.exception.detail, detail)

    def altered(self, value, change):
        raw = json.loads(value.before.attempts[0].raw)
        change(raw)
        fact = collect.envelope.EnvelopeFacts(fixture.canonical(raw))
        return replace(value, before=replace(value.before, attempts=(fact, *value.before.attempts[1:])),
                       after=replace(value.after, attempts=(fact, *value.after.attempts[1:])))

    def test_positive_normal_result_and_exact_existing_capture_parity(self):
        world = World(); world.add()
        with harness(world) as (w, c, scratch):
            normal = execute(c, scratch)
            trace = [(a, o['stdout_limit'], o['stderr_limit']) for a, o in w.calls
                     if a[1:3] != ['attestation', 'verify']]
        world = World(); world.add()
        with harness(world) as (w, c, scratch):
            retained = collect.collect_outage_history(REQUEST, c, scratch, wall_clock=lambda:NOW)
            self.assertEqual(retained.normal, normal)
            self.assertEqual(trace, [(a, o['stdout_limit'], o['stderr_limit']) for a, o in w.calls
                                   if a[1:3] != ['attestation', 'verify']])
            self.assertEqual(qualify(retained.normal).run_id, 100)
            self.assertEqual(len([a for a, _ in w.calls if a[1:3] == ['attestation', 'verify']]), 2)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'outage-history-no-missing-evidence'):
            self.missing(retained)

    def test_complete_empty_inventory_is_missing_not_deletion_proof(self):
        result = self.retained(empty=True)
        self.assertIs(self.missing(result), result)
        self.assertEqual(result.before.runs, b'[]\n')
        self.assertEqual(result.before.attempts, ())
        self.assertEqual(result.normal.history.whole_run_deletion_coverage, 'unavailable')

    def test_genuine_missing_and_expired_are_derived_from_raw_inventory(self):
        for expired in (False, True):
            world = World(); world.add()
            if expired:
                world.artifacts[100][0]['expired'] = True
            else:
                world.artifacts[100].clear()
            with self.subTest(expired=expired):
                result = self.retained(world)
                self.assertIs(self.missing(result), result)
                expected = evidence.MissingEvidence.EXPIRED if expired else evidence.MissingEvidence.MISSING
                self.assertIs(result.normal.history.attempts[0].measurement, expected)

    def test_earlier_positive_and_later_missing_preserve_normal_veto(self):
        world = World(); world.add(); world.add(200, 20, minute=60)
        world.artifacts[200].clear()
        result = self.retained(world)
        with self.assertRaises(evidence.CompatRefusal):
            qualify(result.normal)
        self.assertIs(self.missing(result), result)

    def test_unsuperseded_actual_negative_vetoes_missing_branch(self):
        world = World(); world.add(negative=True); world.add(200, 20, minute=60)
        world.add(300, 30, minute=90); world.artifacts[300].clear()
        result = self.retained(world)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'compat-old-server-incompatible'):
            self.missing(result)
        world = World(); world.add(negative=True); world.add(200, 20, minute=60)
        result = self.retained(world)
        self.assertEqual(qualify(result.normal).run_id, 200)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'compat-old-server-incompatible'):
            self.missing(result)

    def test_failed_positive_is_not_missing_even_alongside_missing_row(self):
        world = World(); world.add(); world.add(200, 20, minute=60)
        world.artifacts[200].clear()
        world.attempts[100, 1]['conclusion'] = 'cancelled'
        result = self.retained(world)
        self.assertIsNotNone(result.normal.history.attempts[0].execution_window)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'outage-history-positive-unqualified'):
            self.missing(result)

    def test_wrong_bound_positive_is_not_missing(self):
        world = World(); world.add(); world.add(200, 20, minute=60); world.artifacts[200].clear()
        result = self.retained(world)
        request = self.request(result)
        changed = replace(request, source=replace(request.source, expected_after_catalog_sha256='f'*64))
        evidence._request(changed)
        with self.assertRaisesRegex(evidence.CompatRefusal, 'outage-history-positive-unqualified'):
            collect.require_missing_expired(changed, result, workflow=WORKFLOW)

    def test_signature_failure_is_unknown_and_cannot_be_missing(self):
        world = World(); world.add()
        world.sign_mutate = lambda value, path: value[0]['verificationResult'].update({'statement':{}})
        result = self.retained(world)
        self.assertIs(result.normal.history.attempts[0].measurement, evidence.MissingEvidence.UNKNOWN)
        self.assertEqual(result.normal.issues, ('artifact-unverified',))
        with self.assertRaises(evidence.CompatRefusal):
            self.missing(result)

    def test_reused_and_partial_steps_cannot_supply_outage_window(self):
        for change in ('reused', 'partial'):
            world = World(); world.add(attempts=2); world.artifacts[100].clear()
            job = world.jobs[100, 2][0]
            if change == 'reused':
                job['created_at'] = world.attempts[100, 1]['run_started_at']
            else:
                job['steps'] = [step for step in job['steps'] if step['name'] != world.steps[job['name']][0]]
            with self.subTest(change=change):
                result = self.retained(world)
                with self.assertRaises(evidence.CompatRefusal): self.missing(result)

    def test_active_and_no_work_are_not_missing(self):
        for mode in ('active', 'no-work'):
            world = World(); world.add(); world.artifacts[100].clear()
            if mode == 'active':
                world.attempts[100, 1].update(status='in_progress', conclusion=None)
            else:
                world.attempts[100, 1]['conclusion'] = 'cancelled'
                for name in evidence.JOBS:
                    world.set_job(100, 1, name, conclusion='skipped', steps=[])
            with self.subTest(mode=mode):
                result = self.retained(world)
                with self.assertRaises(evidence.CompatRefusal): self.missing(result)

    def test_original_snapshot_times_and_fractional_clock_replay(self):
        start = NOW + timedelta(microseconds=123)
        moments = iter(start + timedelta(microseconds=n) for n in range(4))
        result = self.retained(moments=moments)
        self.assertEqual(result.started_at, start)
        self.assertEqual(result.before.observed_at, start + timedelta(microseconds=1))
        self.assertEqual(result.after.observed_at, start + timedelta(microseconds=2))
        self.assertEqual(result.normal.history.observed_at, start + timedelta(microseconds=3))
        for value in (replace(result, started_at=start + timedelta(seconds=1)),
                      replace(result, before=replace(result.before, observed_at=start.replace(tzinfo=None))),
                      replace(result, after=replace(result.after, observed_at=start.replace(fold=1)))):
            self.invalid(value, 'outage-history-clock')

    def test_envelope_uses_its_original_snapshot_time(self):
        result = self.retained()
        too_early = fixture.T0 + timedelta(minutes=10)
        value = replace(result, started_at=too_early, before=replace(result.before, observed_at=too_early))
        self.invalid(value, 'collect-time')

    def test_each_raw_role_and_normal_commitment_are_bound(self):
        result = self.retained()
        for value in (replace(result, before=replace(result.before, workflow_id=901)),
                      replace(result, after=replace(result.after, runs=b'[]\n')),
                      replace(result, normal=replace(result.normal, transcript_sha256='a'*64)),
                      replace(result, normal=replace(result.normal, history=replace(result.normal.history,
                          observation_id='b'*64)))):
            self.invalid(value)

    def test_complete_run_listing_cannot_omit_or_change_attempts(self):
        world = World(); world.add(attempts=2)
        result = self.retained(world)
        for facts in (result.before.attempts[:1], tuple(reversed(result.before.attempts)),
                      (result.before.attempts[0], result.before.attempts[0])):
            value = replace(result, before=replace(result.before, attempts=facts),
                            after=replace(result.after, attempts=facts))
            self.invalid(value)

    def test_closed_raw_keys_canonical_bytes_and_types_are_required(self):
        result = self.retained()
        for change in (lambda v:v.update(extra=1), lambda v:v.update(version=True),
                       lambda v:v['run'].update(latest_run_attempt=True),
                       lambda v:v['run']['repository'].update(extra=1),
                       lambda v:v['jobs'][0]['steps'][0].update(extra=1)):
            self.invalid(self.altered(result, change))
        fact = collect.envelope.EnvelopeFacts(b' ' + result.before.attempts[0].raw)
        self.invalid(replace(result, before=replace(result.before, attempts=(fact,)),
                             after=replace(result.after, attempts=(fact,))))

    def test_complete_check_inventory_and_identity_mutants_refuse(self):
        result = self.retained()
        for change in (lambda v:v['checks'].pop(),
                       lambda v:v['checks'][0]['app'].update(id=1),
                       lambda v:v['checks'][0].update(head_sha='b'*40),
                       lambda v:v['jobs'][0].update(run_attempt=2),
                       lambda v:v['artifacts'][0]['workflow_run'].update(head_sha='b'*40)):
            self.invalid(self.altered(result, change))

    def test_pair_measurement_cannot_be_relabelled_missing_or_expired(self):
        result = self.retained(); original = result.normal.history.attempts[0]
        for measurement in (evidence.MissingEvidence.MISSING, evidence.MissingEvidence.EXPIRED,
                            evidence.MissingEvidence.UNKNOWN):
            history = replace(result.normal.history, attempts=(replace(original, measurement=measurement),))
            self.invalid(replace(result, normal=replace(result.normal, history=history)))

    def test_authenticated_pair_must_bind_exact_raw_artifact(self):
        result = self.retained(); original = result.normal.history.attempts[0]
        pair = replace(original.measurement, artifact=replace(original.measurement.artifact, artifact_id=99))
        history = replace(result.normal.history, attempts=(replace(original, measurement=pair),))
        self.invalid(replace(result, normal=replace(result.normal, history=history)), 'outage-history-pair')

    def test_supplied_window_is_not_an_authority(self):
        result = self.retained(); original = result.normal.history.attempts[0]
        shifted = replace(original.execution_window, started_at=original.execution_window.started_at+timedelta(microseconds=1))
        history = replace(result.normal.history, attempts=(replace(original, execution_window=shifted),))
        self.invalid(replace(result, normal=replace(result.normal, history=history)), 'outage-history-derived')

    def test_metadata_bounds_fatal_issues_and_noncanonical_order_refuse(self):
        result = self.retained()
        for fields in ({'command_count':True}, {'command_count':0}, {'command_count':6001},
                       {'captured_bytes':-1}, {'captured_bytes':collect.MAX_BYTES+1},
                       {'issues':('inventory-changed',)}, {'issues':('collection-deadline',)},
                       {'issues':('inventory-unavailable',)}, {'issues':('artifact-unverified', 'artifact-unverified')}):
            self.invalid(replace(result, normal=replace(result.normal, **fields)))

    def test_raw_aggregate_is_checked_before_json_decode_and_counts_both_roles(self):
        result = self.retained()
        fact = collect.envelope.EnvelopeFacts(b'x' * collect.envelope.MAX_RAW_BYTES)
        snap = replace(result.before, attempts=(fact, fact))
        value = replace(result, before=snap, after=snap)
        with mock.patch.object(collect.common, 'loads_strict', side_effect=AssertionError('decoded over-bound data')):
            self.invalid(value, 'outage-history-bound')

    def test_changed_and_failed_second_inventory_refuse_without_new_pass(self):
        for failed in (False, True):
            world = World(); world.add()
            def changed(path, value):
                if world.workflow_reads == 2 and path.endswith('/actions/workflows/compat.yml'):
                    if failed: raise evidence.CompatRefusal('compat-stale', 'synthetic-read-failure')
                    value['id'] = 901
                return value
            world.hook = changed
            with self.subTest(failed=failed), harness(world) as (w, c, scratch):
                with self.assertRaises(evidence.CompatRefusal):
                    collect.collect_outage_history(REQUEST, c, scratch, wall_clock=lambda:NOW)
                self.assertEqual(w.workflow_reads, 2)

    def test_current_write_and_exact_w0_custody_routes_are_separate(self):
        with w0_epoch():
            result = self.retained(empty=True)
            request = self.request(result)
        self.assertEqual(collect.WORKFLOW_SHA256, P_WORKFLOW_SHA256)
        with self.assertRaises(evidence.CompatRefusal) as caught:
            collect.validate_outage_history(result, workflow=W0_WORKFLOW, producer_sha=fixture.SHA)
        self.assertEqual(caught.exception.detail, 'collect-workflow-source')
        self.assertIs(collect._validate_outage_history_custody(result, workflow=W0_WORKFLOW,
            producer_sha=fixture.SHA), result)
        self.assertIs(collect._require_missing_expired_custody(request, result,
            workflow=W0_WORKFLOW), result)
        with self.assertRaises(evidence.CompatRefusal):
            collect._validate_outage_history_custody(result, workflow=W0_WORKFLOW+b'\n', producer_sha=fixture.SHA)

    def test_previous_p_custody_replays_but_cannot_create_current_evidence(self):
        with p0_epoch():
            result = self.retained(empty=True)
            request = self.request(result)
        self.assertEqual(collect.WORKFLOW_SHA256, P_WORKFLOW_SHA256)
        with self.assertRaises(evidence.CompatRefusal) as caught:
            collect.validate_outage_history(result, workflow=P0_WORKFLOW, producer_sha=fixture.SHA)
        self.assertEqual(caught.exception.detail, 'collect-workflow-source')
        self.assertIs(collect._validate_outage_history_custody(result, workflow=P0_WORKFLOW,
            producer_sha=fixture.SHA), result)
        self.assertIs(collect._require_missing_expired_custody(request, result,
            workflow=P0_WORKFLOW), result)
        with self.assertRaises(evidence.CompatRefusal):
            collect._validate_outage_history_custody(result, workflow=P0_WORKFLOW+b'\n', producer_sha=fixture.SHA)

    def test_request_identity_and_observation_are_required(self):
        result = self.retained(empty=True)
        for request in (replace(self.request(result), observation_id='a'*64),
                        replace(self.request(result), source=replace(self.request(result).source, sha='b'*40))):
            with self.assertRaises(evidence.CompatRefusal):
                collect.require_missing_expired(request, result, workflow=WORKFLOW)

    def test_holders_are_immutable_and_container_substitutions_refuse(self):
        from dataclasses import FrozenInstanceError
        result = self.retained()
        with self.assertRaises(FrozenInstanceError): result.started_at = NOW
        self.invalid(replace(result, before=replace(result.before, attempts=list(result.before.attempts))))
        self.invalid(replace(result, normal=replace(result.normal, history=replace(result.normal.history,
            attempts=list(result.normal.history.attempts)))))


    def test_capture_limit_counts_both_snapshots_and_signed_pair_bytes(self):
        baseline = self.retained()
        raw_bytes = sum(len(snap.runs) + sum(len(row.raw) for row in snap.attempts)
                        for snap in (baseline.before, baseline.after))
        pair_bytes = sum(len(row.measurement.result) + len(row.measurement.context)
                         for row in baseline.normal.history.attempts)
        self.assertGreater(pair_bytes, 0)
        with mock.patch.object(collect, 'MAX_OUTAGE_RAW_BYTES', raw_bytes + pair_bytes - 1):
            with harness() as (world, client, scratch):
                with self.assertRaises(evidence.CompatRefusal):
                    collect.collect_outage_history(REQUEST, client, scratch, wall_clock=lambda:NOW)
                self.assertEqual(world.workflow_reads, 2)
        self.assertIsNotNone(qualify(baseline.normal))

    def test_normal_then_outage_cannot_reuse_or_refresh_the_same_client(self):
        with harness() as (world, client, scratch):
            execute(client, scratch)
            calls = len(world.calls)
            with self.assertRaisesRegex(evidence.CompatRefusal, 'collect-client-reuse'):
                collect.collect_outage_history(REQUEST, client, scratch, wall_clock=lambda:NOW)
            self.assertEqual(len(world.calls), calls)


class WorkflowTransitionTests(unittest.TestCase):
    def test_current_collector_refuses_real_w0_before_fetch(self):
        world = World(); world.add()
        with harness(world) as (world, client, scratch):
            before = list(world.calls)
            with self.assertRaises(evidence.CompatRefusal):
                collect.collect_history(collect.CollectionRequest(fixture.SHA, W0_WORKFLOW),
                    client, scratch, wall_clock=lambda:NOW)
            self.assertEqual(world.calls, before)
        self.assertEqual([args[1:] for args, _ in world.calls], [['--version']])

    def test_current_collector_refuses_previous_p_before_fetch(self):
        world = World(); world.add()
        with harness(world) as (world, client, scratch):
            before = list(world.calls)
            with self.assertRaises(evidence.CompatRefusal):
                collect.collect_history(collect.CollectionRequest(fixture.SHA, P0_WORKFLOW),
                    client, scratch, wall_clock=lambda:NOW)
            self.assertEqual(world.calls, before)
        self.assertEqual([args[1:] for args, _ in world.calls], [['--version']])

    def test_previous_epoch_restores_current_on_fixture_failure(self):
        original = WORKFLOW, REQUEST
        with self.assertRaisesRegex(RuntimeError, '^fixture-failed$'):
            with p0_epoch():
                self.assertEqual(collect.WORKFLOW_SHA256, P0_WORKFLOW_SHA256)
                self.assertEqual(WORKFLOW, P0_WORKFLOW)
                raise RuntimeError('fixture-failed')
        self.assertEqual(collect.WORKFLOW_SHA256, P_WORKFLOW_SHA256)
        self.assertEqual((WORKFLOW, REQUEST), original)

    def test_original_epoch_restores_actual_p_on_fixture_failure(self):
        original = WORKFLOW, REQUEST
        with self.assertRaisesRegex(RuntimeError, '^fixture-failed$'):
            with w0_epoch():
                self.assertEqual(collect.WORKFLOW_SHA256, W0_WORKFLOW_SHA256)
                self.assertEqual(WORKFLOW, W0_WORKFLOW)
                raise RuntimeError('fixture-failed')
        self.assertEqual(collect.WORKFLOW_SHA256, P_WORKFLOW_SHA256)
        self.assertEqual((WORKFLOW, REQUEST), original)
        self.assertEqual(hashlib.sha256(WORKFLOW).hexdigest(), P_WORKFLOW_SHA256)
