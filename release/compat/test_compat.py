"""Mock admission/adapter orchestration plus real local budget composition.

No Docker, GitHub, database, image builds or real compatibility runs occur.
The public production admission refusal is tested separately from mock runs.
"""
from dataclasses import asdict
import json
from pathlib import Path
from types import SimpleNamespace
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat as c
import test_controller as tc
import test_observation as to
import test_proof as tp
import test_stack as ts

_WORLD = None
BASE = c.catalog._BASE
SHA = 'a' * 40
DIGEST = 'sha256:' + 'f' * 64
PREVIOUS = c.runtime.release_record.Entry('prod-20261011T000000Z-aaaaaaaa','deploy',BASE,
    {'web':DIGEST},'1'*64,'2026-10-11T00:00:00Z',None,None,False,{})


class World:
    def __init__(self):
        self.trace=[]; self.clock=to.Clock(); self.fail=None; self.cleanup_fail=False
        self.codes={}; self.logs={}; self.markers={}; self.catalogs={}; self.digests={}
        self.false_case=None; self.worker_panic=0; self.bad_control=False
        self.pending=1; self.rows=0; self.readiness=[b'[17,"*","postgres"]\n']
        self.record_nonces=[]; self.ledger_reads=0; self.operations=[]

    def event(self, name):
        self.trace.append(name)
        if self.fail==name:raise c.proof.InfrastructureFailure('mock-incomplete')


class FakeStack:
    def __init__(self, binary, config, source, graph):
        self.w=_WORLD; self.binary=binary; self.config=config; self.source_sha=source
        self.capture=self.raw_capture; self.env={}; self.token=ts.TOKEN
        self.namespace=tc.fixture().namespace; self._closed=False
        self._handles={}; self._ids={}; self._events=[]; self._process_ids={}; self._process_manager=None
        self.planned_web_origin=tc.fixture().product_origin; self.graph_origin=tc.fixture().stub_origin
        self.fixture_inputs=SimpleNamespace(graph_control_key=tc.fixture().control_key)
        self.w.owner=self

    def start(self):
        self.w.event('stack.start')
        self._ids={role:str(i)*64 for i,role in enumerate(('pg','redis','graph'),1)}
        self._handles={role:SimpleNamespace(container_id=v,started_at='started') for role,v in self._ids.items()}

    def _container_check(self,*args,**kwargs):return {}

    def raw_capture(self, argv, **options):
        self.w.operations.append((argv,options))
        self.w.event('pg.readiness')
        assert options['input_bytes']==c._READY_SQL
        value=self.w.readiness.pop(0) if len(self.w.readiness)>1 else self.w.readiness[0]
        return c.runtime.Capture(0,value,b'')

    def cleanup(self):
        self.w.event('stack.cleanup')
        if self.w.cleanup_fail:raise c.proof.EvidenceError('mock-cleanup-refused')
        self._closed=True


class FakeFixture:
    def __init__(self,owner,image):self.owner=owner;self._closed=False
    def run(self,inputs,*,password):
        self.owner.w.event('fixture.run')
        from dataclasses import replace
        value=replace(tc.fixture(),organization_id=inputs.organization_id,password=password)
        return c.fixture.ProvisionedFixture(value,tc.BUSINESS,tp.CATALOG,self.owner._ids['pg'],'started',self.owner.token)
    def close(self):self.owner.w.event('fixture.close');self._closed=True


class FakeManager:
    def __init__(self,owner,*bindings):
        self.owner=owner;self.w=owner.w;self._ids={};self._closed=False;self._events=[]
        self.w.manager=self

    def start_web(self,version,inputs):
        self.w.event('web.'+version)
        handle=SimpleNamespace(version=version,phase='web',slot='web')
        self._ids['web']=handle;return handle

    def start_migration(self,phase,inputs):
        self.w.event('migration.'+phase)
        handle=SimpleNamespace(phase=phase,slot='migration')
        self._ids['migration']=handle;return handle

    def wait_migration(self,handle):
        self.w.event('wait.'+handle.phase);return self.w.codes.get(handle.phase,0)

    def remove(self,handle):
        self.w.event('remove.'+handle.slot)
        assert self._ids[handle.slot] is handle;del self._ids[handle.slot]

    def controller(self,raw):
        request=c.controller.parse_request(raw);op=request['operation']
        self.w.event('controller.'+op)
        response={'version':1,'status':'ok','operation':op,'stub_receipts':tc.receipts(op)}
        if op=='setup':response['registered']=True
        else:
            response['cases']={key:True for key in c.proof.SWEEP_CASES}
            if self.w.false_case==op:response['cases']['settings']=False
            if op=='write':
                self.w.rows+=1;nonce=request['nonce'];self.w.record_nonces.append(nonce)
                value=c.controller.sweep.RecordSet(c.controller.sweep.Sweep(c.controller._fixture(request['fixture']))._binding(),nonce,
                    *(f'20000000-0000-4000-8000-{self.w.rows*10+i:012d}' for i in range(6)))
                response['record']=asdict(value)
        c.controller.validate_receipts(request,response)
        return c.controller._encode(response)

    def close(self):self.w.event('manager.close');self._ids.clear();self._closed=True


class FakePG:
    def __init__(self,*args,**kwargs):self.w=_WORLD
    def install_probe(self):self.w.event('probe.install')
    def begin_phase(self,phase):self.w.event('pg.begin.'+phase);return phase
    def finish_phase(self,phase):
        self.w.event('pg.finish.'+phase);self.w.phase=phase
        return {'log':{'raw':self.w.logs.get(phase,b''),'complete':True},
                'markers':{'complete':True,'application':phase,'hashes':self.w.markers.get(phase,[])}}
    def read_only_control(self):
        self.w.event('pg.control')
        return {'execution':tp.execution(1),'sqlstate':'25006','log':{
            'raw':b'' if self.w.bad_control else tp.control_log(),'complete':True}}


class FakeContract:
    target_sha256=tp.CATALOG
    expected_preexisting_tables=(tp.TABLE,)
    previous_web=c.runtime.common.IMAGE_REPOSITORY['web']+'@'+DIGEST


class FakeCatalog:
    def __init__(self,*args):self.w=_WORLD;self.count=0
    def initial(self,inputs):
        self.w.event('catalog.initial');return self.observation(),self.tables(tp.DATA)
    def after_phase(self,inputs,*,original_columns=False):
        self.count+=1;label='forward' if original_columns else ('d0' if self.count==3 else self.w.phase)
        self.w.event('catalog.'+label)
        return self.observation(self.w.catalogs.get(label,tp.CATALOG)),self.tables(
            self.w.digests.get(label,tp.DATA if original_columns else tp.CHANGED))
    def observe(self,inputs):self.w.event('catalog.final');return self.observation()
    @staticmethod
    def observation(value=tp.CATALOG):return c.catalog.CatalogObservation(value,'a'*64,123)
    @staticmethod
    def tables(value):return c.postgres.TableCapture(object(),{}, {tp.TABLE:value},True)


class FakeObservation:
    def __init__(self,manager,handle,fixture,**kwargs):self.manager=manager;self.handle=handle;self.w=manager.w
    def wait_startup(self,timeout=90):self.w.event('startup.'+self.handle.version)
    def boot(self):self.w.event('boot.'+self.handle.version);return tp.boot()
    def finish_period(self,timeout=30):self.w.event('finish.'+self.handle.version);return {}
    def workers(self):
        self.w.event('workers.begin');self.w.clock.sleep(60);self.w.event('workers.end')
        return {'execution':tp.execution(),'observed_seconds':60,'panic_count':self.w.worker_panic,
                'unknown_type_count':0,'unexpected_stub_entries':0}


class FakeLedger:
    def __init__(self,manager,fixture):self.w=manager.w;self.manager=manager
    def capture(self):
        self.w.event('ledger.capture');self.w.ledger_reads+=1
        return c.journal.JournalSnapshot(self.w.ledger_reads,0,self.w.ledger_reads,self.w.pending,'a'*64,False)
    def seal(self):
        self.w.event('ledger.seal');assert not self.manager._ids
        return c.journal.JournalSnapshot(self.w.ledger_reads,0,self.w.ledger_reads,1,'a'*64,True)


def images():
    image=lambda source:c.processes.ImageBinding('sha256:'+'4'*64,'sha256:'+'4'*64,source,'5'*64)
    previous=c.processes.ImageBinding(FakeContract.previous_web,'sha256:'+'4'*64,BASE,'5'*64)
    return c._Images(c._IMAGE_SEAL,previous,image(SHA),image('c'*40),
        c.stack.GraphImage(ts.GRAPH,ts.SOURCE),c.fixture.ExportImage('sha256:'+'6'*64,SHA,'5'*64))


class EngineTests(unittest.TestCase):
    def setUp(self):
        global _WORLD
        self.w=_WORLD=World();self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.p=Path(self.tmp.name)
        for module,name,cls in [(c.stack,'OwnedStack',FakeStack),(c.fixture,'OwnedFixture',FakeFixture),
                (c.processes,'OwnedProcesses',FakeManager),(c.postgres,'SyntheticPostgres',FakePG),
                (c.catalog,'OwnedCatalog',FakeCatalog),(c.observation,'OwnedObservation',FakeObservation),
                (c.journal,'JournalLedger',FakeLedger)]:
            active=patch.object(module,name,cls);active.start();self.addCleanup(active.stop)

    def engine(self):
        return c._Engine(self.p,PREVIOUS,SHA,12345,2,c.schema_change.Classification('none',False,(),()),
            FakeContract(),images(),'docker',self.p,clock=self.w.clock,sleep=self.w.clock.sleep)

    def test_complete_order_real_evaluator_and_cleanup_before_public_encoding(self):
        engine=self.engine()
        original=c.proof.result_bytes
        def encode(result):
            self.assertTrue(self.w.owner._closed)
            self.assertEqual(self.w.trace[-1],'stack.cleanup')
            return original(result)
        with patch.object(c.proof,'result_bytes',side_effect=encode):raw=engine.run()
        result=json.loads(raw)
        self.assertTrue(result['forward']);self.assertTrue(result['reversible'])
        self.assertTrue(all(result['old_server_ok'].values()))
        trace=self.w.trace
        self.assertLess(trace.index('fixture.close'),trace.index('web.previous'))
        self.assertLess(trace.index('ledger.capture'),trace.index('web.previous'))
        self.assertEqual(trace.count('migration.compat-fwd'),2)
        for phase in ['compat-fwd','compat-n1-rw','compat-n1-ro']:
            self.assertLess(trace.index('pg.begin.'+phase),trace.index('migration.'+phase))
            self.assertLess(trace.index('wait.'+phase),trace.index('pg.finish.'+phase))
        self.assertEqual([x for x in trace if x.startswith('controller.')],
            ['controller.setup','controller.write','controller.write','controller.read','controller.write','controller.read'])
        self.assertEqual(trace[trace.index('workers.begin'):trace.index('workers.end')+1],['workers.begin','workers.end'])
        self.assertLess(trace.index('ledger.seal'),trace.index('catalog.final'))
        self.assertEqual(len(set(self.w.record_nonces)),3)
        self.assertNotIn(tc.fixture().namespace,raw.decode());self.assertNotIn(engine.inputs.owner_password,raw.decode())
        with self.assertRaisesRegex(c.proof.EvidenceError,'already-attempted'):engine.run()

    def test_marker_mod_digest_catalog_and_exit_are_completed_negative_migrations(self):
        for field in ('markers','logs','digests','catalogs','codes'):
            with self.subTest(field=field):
                self.w.trace.clear()
                value={'markers':['a'*64],'logs':tp.line('compat-n1-rw'),'digests':'f'*64,'catalogs':'f'*64,'codes':1}[field]
                getattr(self.w,field)['compat-n1-rw']=value
                result=json.loads(self.engine().run())
                self.assertFalse(result['old_migrate_ok']['rw']);self.assertFalse(result['reversible'])
                getattr(self.w,field).clear()

    def test_completed_row_worker_and_forward_negative_flags_not_defaulted(self):
        self.w.false_case='read';self.w.worker_panic=1
        self.w.digests['forward']='f'*64
        result=json.loads(self.engine().run())
        self.assertFalse(result['forward']);self.assertFalse(result['old_server_ok']['read'])
        self.assertFalse(result['old_server_ok']['reverse']);self.assertFalse(result['old_server_ok']['workers'])

    def test_missing_25006_control_produces_no_result(self):
        self.w.bad_control=True
        with self.assertRaisesRegex(c.proof.EvidenceError,'negative-control'):self.engine().run()
        self.assertEqual(self.w.trace[-1],'stack.cleanup')

    def test_first_failed_phase_never_replayed_or_fabricated(self):
        for point in ['fixture.run','startup.previous','controller.write','wait.compat-fwd','pg.finish.compat-n1-rw','ledger.seal']:
            with self.subTest(point=point):
                self.w.trace.clear();self.w.fail=point
                with self.assertRaises(c.proof.InfrastructureFailure):self.engine().run()
                self.assertEqual(self.w.trace.count(point),1)
                self.assertEqual(self.w.trace[-1],'stack.cleanup')
        self.w.fail=None

    def test_cleanup_refusal_suppresses_fully_measured_result(self):
        self.w.cleanup_fail=True
        with patch.object(c.proof,'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.EvidenceError,'cleanup'):self.engine().run()
            output.assert_not_called()

    def test_foreign_transport_or_budget_never_receives_cleanup_commands(self):
        for change in ('capture','budget','source','token'):
            with self.subTest(change=change):
                self.w.trace.clear();engine=self.engine();calls=[]
                def foreign(*args,**kwargs):calls.append((args,kwargs));raise AssertionError('foreign invoked')
                def measure():
                    engine.owner.start()
                    if change=='capture':engine.owner.capture=foreign
                    elif change=='budget':engine.owner._experiment_budget=object()
                    elif change=='source':engine.owner.source_sha='f'*40
                    else:engine.owner.token='0'*32
                    raise c.proof.InfrastructureFailure('incomplete')
                with patch.object(engine,'_measure',side_effect=measure),patch.object(c.proof,'result_bytes') as output:
                    with self.assertRaisesRegex(c.proof.EvidenceError,'cleanup-transport-unowned'):engine.run()
                    output.assert_not_called()
                self.assertEqual(calls,[]);self.assertNotIn('stack.cleanup',self.w.trace)
                self.assertFalse(engine.owner._closed)
                if change=='capture':self.assertIs(engine.owner.capture,foreign)

    def test_cleanup_rechecks_after_manager_close_before_stack(self):
        engine=self.engine();calls=[]
        def foreign(*args,**kwargs):calls.append((args,kwargs))
        def close(manager):
            manager.w.event('manager.close');manager._closed=True
            manager.owner.capture=foreign
        with patch.object(FakeManager,'close',close),patch.object(c.proof,'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.EvidenceError,'cleanup-transport-unowned'):engine.run()
            output.assert_not_called()
        self.assertIs(engine.owner.capture,foreign);self.assertEqual(calls,[])
        self.assertNotIn('stack.cleanup',self.w.trace)

    def test_expired_known_budget_restores_transport_then_cleans(self):
        engine=self.engine();original=engine.owner.capture
        def measure():
            engine.owner.start();self.w.clock.now+=2401
            engine.budget.remaining()
        with patch.object(engine,'_measure',side_effect=measure),patch.object(c.proof,'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.InfrastructureFailure,'experiment-deadline'):engine.run()
            output.assert_not_called()
        self.assertIs(engine.owner.capture,original);self.assertFalse(hasattr(engine.owner,'_experiment_budget'))
        self.assertEqual(self.w.trace[-1],'stack.cleanup')

    def test_settle_is_twelve_observations_no_write_retry(self):
        self.w.pending=3
        with self.assertRaisesRegex(c.proof.EvidenceError,'settle-incomplete'):self.engine().run()
        self.assertEqual(self.w.trace.count('controller.write'),1)
        self.assertEqual(self.w.ledger_reads,13) # initial + 12 pending observations

    def test_socket_readiness_excludes_temporary_server_and_copies_no_credentials(self):
        self.w.readiness=[b'[17,"","postgres"]\n',b'[17,"*","postgres"]\n']
        self.engine().run()
        self.assertEqual(self.w.trace.count('pg.readiness'),2)
        for argv,options in self.w.operations:
            self.assertIn('/var/run/postgresql',argv);self.assertIn('default_transaction_read_only=on',' '.join(argv))
            self.assertEqual(options['input_bytes'],c._READY_SQL)

    def test_wrong_readiness_state_or_malformed_output_refuses_before_fixture(self):
        for raw in [b'[16,"*","postgres"]',b'[17.0,"*","postgres"]',b'[17,"*","foreign"]',b'{}',b'invalid']:
            with self.subTest(raw=raw):
                self.w.trace.clear();self.w.readiness=[raw]
                with self.assertRaises(c.proof.EvidenceError):self.engine().run()
                self.assertNotIn('fixture.run',self.w.trace)

    def test_temporary_server_readiness_is_bounded_even_with_frozen_clock(self):
        self.w.readiness=[b'[17,"","postgres"]'];self.w.clock.sleep=lambda _:None
        with self.assertRaises(c.proof.InfrastructureFailure):self.engine().run()
        self.assertEqual(self.w.trace.count('pg.readiness'),225)
        self.assertNotIn('fixture.run',self.w.trace)

    def test_foreign_or_unsealed_admission_refuses_before_stack(self):
        good=images()
        from dataclasses import replace
        for value in [object(),replace(good,seal=object()),replace(good,candidate=good.previous)]:
            with self.assertRaises(c.proof.EvidenceError):
                c._Engine(self.p,PREVIOUS,SHA,1,1,None,FakeContract(),value,'docker',self.p)
        self.assertEqual(self.w.trace,[])


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.p=Path(self.tmp.name)

    def test_public_entrypoint_cannot_accept_arbitrary_images_or_proof(self):
        classification=c.schema_change.Classification('none',False,(),())
        with patch.object(c.runtime,'verified_previous',return_value=PREVIOUS) as verify, \
             patch.object(c.schema_change,'classify',return_value=classification) as classify, \
             patch.object(c.catalog,'CatalogContract',return_value=FakeContract()), \
             patch.object(c.stack,'OwnedStack') as docker:
            with self.assertRaisesRegex(c.proof.EvidenceError,'source-build-admission-unimplemented'):
                c.run_experiment(self.p,SHA,123,2,object(),docker_binary='docker',config_dir=self.p)
            verify.assert_called_once();classify.assert_called_once_with(self.p,BASE,SHA,mode='forward')
            docker.assert_not_called()
            with self.assertRaises(TypeError):
                c.run_experiment(self.p,SHA,123,2,object(),docker_binary='docker',config_dir=self.p,images=images())

    def test_active_or_blocked_classifier_never_reaches_image_or_docker(self):
        for kind,dormant,reasons in [('expand',False,()),('contract',False,()),('data',False,()),('none',False,('blocked',))]:
            with patch.object(c.runtime,'verified_previous',return_value=PREVIOUS), \
                 patch.object(c.schema_change,'classify',return_value=c.schema_change.Classification(kind,dormant,reasons,())), \
                 patch.object(c,'_admit_images') as admission:
                with self.assertRaises(c.proof.EvidenceError):
                    c.run_experiment(self.p,SHA,1,1,object(),docker_binary='docker',config_dir=self.p)
                admission.assert_not_called()

    def test_source_run_attempt_and_rollback_inputs_validate_before_verification(self):
        with patch.object(c.runtime,'verified_previous') as verify:
            for sha,run,attempt,target in [('bad',1,1,None),(SHA,True,1,None),(SHA,1,0,None),(SHA,1,1,'main')]:
                with self.assertRaises(c.proof.EvidenceError):
                    c.run_experiment(self.p,sha,run,attempt,object(),docker_binary='docker',config_dir=self.p,rollback_target=target)
            verify.assert_not_called()


class BudgetTests(unittest.TestCase):
    def harness(self):
        h=to.ObservationTests('test_positive_owned_boot_and_actual_sixty_second_window')
        h.setUp();self.addCleanup(h.doCleanups);return h

    def test_actual_native_observer_nested_budgets_restore_and_limit(self):
        h=self.harness();observer=h.observer();original=h.owner.capture
        with c._Budget(h.owner,clock=h.clock,seconds=200) as budget:
            with budget.window(12):budget.observer_call(observer.wait_startup)
            observer.boot()
            with budget.window(7):budget.observer_call(observer.finish_period)
            self.assertIs(h.owner.capture,budget.transport)
        self.assertIs(h.owner.capture,original)
        timeouts=[options['timeout'] for _,options in h.world.calls]
        self.assertIn(7,timeouts)
        h.manager.close();h.owner.cleanup()

    def test_native_observer_exception_restores_both_layers(self):
        h=self.harness();observer=h.observer();h.world.raw=to.startup().replace(b'runtime_role=compat_runtime',b'runtime_role=postgres')
        original=h.owner.capture
        with self.assertRaises(c.proof.EvidenceError):
            with c._Budget(h.owner,clock=h.clock) as budget:budget.observer_call(observer.wait_startup)
        self.assertIs(h.owner.capture,original)
        h.manager.close();h.owner.cleanup()

    def test_bridge_rejects_arbitrary_method_or_other_owner_before_call(self):
        h=self.harness();other=self.harness();observer=h.observer()
        with c._Budget(h.owner,clock=h.clock) as budget:
            for method in [observer.boot,lambda:None,other.observer().wait_startup]:
                with self.assertRaises(c.proof.EvidenceError):budget.observer_call(method)

    def test_foreign_capture_left_by_inner_observer_is_not_clobbered(self):
        h=self.harness();observer=h.observer();original=h.owner.capture
        foreign=lambda *a,**kw:None
        h.world.mutate_log=lambda w:setattr(h.owner,'capture',foreign)
        with self.assertRaises(c.proof.EvidenceError):
            with c._Budget(h.owner,clock=h.clock) as budget:budget.observer_call(observer.wait_startup)
        self.assertIs(h.owner.capture,foreign)
        h.owner.capture=original

    def test_outer_late_capture_reversal_and_replacement_refuse(self):
        for failure in ('late','reverse','replace'):
            h=self.harness();original=h.owner.capture
            def transport(argv,**options):
                if failure=='late':h.clock.now+=11
                elif failure=='reverse':h.clock.now-=1
                else:h.owner.capture=lambda *a,**kw:None
                return c.runtime.Capture(0,b'',b'')
            h.owner.capture=transport
            with self.subTest(failure=failure),self.assertRaises(c.proof.EvidenceError):
                with c._Budget(h.owner,clock=h.clock,seconds=10):
                    h.owner.capture([h.owner.binary,'--config',str(h.owner.config),'--context','default','logs','id'],timeout=30)
            if failure!='replace':self.assertIs(h.owner.capture,transport)
            h.owner.capture=original


if __name__=='__main__':unittest.main(verbosity=2)
