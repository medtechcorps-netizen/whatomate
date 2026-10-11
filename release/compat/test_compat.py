"""Mock admission/adapter orchestration plus real local budget composition.

No Docker, GitHub, database, image builds or real compatibility runs occur.
The public production admission refusal is tested separately from mock runs.
"""
from dataclasses import asdict, replace
from contextlib import nullcontext
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
        self.worker_unknown=0
        self.terminal_old_boot=False
        self.terminal_previous_source=BASE
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
    @classmethod
    def for_old_boot(cls,manager,handle,fixture,*,run_id,run_attempt,**kwargs):
        value=cls(manager,handle,fixture,**kwargs)
        value.identity={'sha':SHA,'previous_web_digest':DIGEST,'run_id':run_id,'run_attempt':run_attempt}
        return value
    def wait_old_boot(self,timeout=90):
        self.w.event('startup.'+self.handle.version)
        if self.w.terminal_old_boot:
            self.w.event('terminal.old.boot')
            return tp.terminal_boot(self.identity,previous_source=self.w.terminal_previous_source)
        return None
    def wait_startup(self,timeout=90):self.w.event('startup.'+self.handle.version)
    def boot(self):self.w.event('boot.'+self.handle.version);return tp.boot()
    def finish_period(self,timeout=30):self.w.event('finish.'+self.handle.version);return {}
    def workers(self):
        self.w.event('workers.begin');self.w.clock.sleep(60);self.w.event('workers.end')
        return {'execution':tp.execution(),'observed_seconds':60,'panic_count':self.w.worker_panic,
                'unknown_type_count':self.w.worker_unknown,'unexpected_stub_entries':0}


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

    def test_actual_terminal_branch_keeps_all_independent_phases_and_causal_rows(self):
        self.w.terminal_old_boot=True
        original=c.proof.evaluate_proof; observed=[]
        def evaluate(value):observed.append(value);return original(value)
        with patch.object(c.proof,'evaluate_proof',side_effect=evaluate):raw=self.engine().run()
        result=json.loads(raw); private=observed[0];trace=self.w.trace
        self.assertTrue(result['forward']);self.assertTrue(result['reversible'])
        self.assertFalse(any(result['old_server_ok'].values()))
        self.assertEqual([x for x in trace if x.startswith('controller.')],
                         ['controller.setup','controller.write','controller.write'])
        self.assertEqual(trace.count('web.previous'),2);self.assertEqual(trace.count('web.candidate'),1)
        self.assertEqual(trace.count('migration.compat-fwd'),2)
        self.assertEqual(trace.count('migration.compat-n1-rw'),1);self.assertEqual(trace.count('migration.compat-n1-ro'),1)
        self.assertIn('catalog.d0',trace);self.assertIn('pg.control',trace)
        self.assertNotIn('workers.begin',trace);self.assertNotIn('controller.read',trace)
        self.assertLess(trace.index('pg.control'),trace.index('terminal.old.boot'))
        self.assertLess(trace.index('terminal.old.boot'),trace.index('ledger.seal'))
        self.assertLess(trace.index('ledger.seal'),trace.index('catalog.final'))
        self.assertEqual(trace.count('remove.web'),3);self.assertEqual(trace[-1],'stack.cleanup')
        for key in ('read_sweep','write_sweep','workers','candidate_ready','reverse_sweep'):
            node=private['old_server'][key]
            self.assertEqual(node['state'],'blocked-by-semantic')
            self.assertEqual(node['cause_phase'],'old.boot')
            self.assertEqual(set(node),{'state','phase','cause_phase','reason','binding'})
        self.assertTrue(private['old_server']['stopped']['passed'])
        self.assertNotIn('binding',raw.decode());self.assertNotIn('blocked-by-semantic',raw.decode())

    def test_terminal_does_not_bypass_control_seal_final_catalog_or_cleanup(self):
        for failure in ('control','ledger.seal','catalog.final','cleanup'):
            with self.subTest(failure=failure):
                self.w.trace.clear();self.w.terminal_old_boot=True
                self.w.bad_control=failure=='control';self.w.cleanup_fail=failure=='cleanup'
                self.w.fail=failure if failure in {'ledger.seal','catalog.final'} else None
                with patch.object(c.proof,'result_bytes') as output:
                    with self.assertRaises(c.proof.EvidenceError):self.engine().run()
                    output.assert_not_called()
                self.assertEqual(self.w.trace[-1],'stack.cleanup')
        self.w.fail=None;self.w.bad_control=False;self.w.cleanup_fail=False

    def test_terminal_removal_failure_and_start_without_handle_never_fabricate(self):
        original_start=FakeManager.start_web;original_remove=FakeManager.remove
        for failure in ('start','remove'):
            with self.subTest(failure=failure):
                self.w.trace.clear();self.w.terminal_old_boot=True;counts={'previous':0,'remove':0}
                def start(manager,version,inputs):
                    if version=='previous':counts['previous']+=1
                    if failure=='start' and version=='previous' and counts['previous']==2:
                        manager.w.event('start-refused-no-handle')
                        raise c.proof.EvidenceError('process-refused')
                    return original_start(manager,version,inputs)
                def remove(manager,handle):
                    counts['remove']+=1
                    if failure=='remove' and counts['remove']==3:
                        raise c.proof.EvidenceError('process-removal-unproven')
                    return original_remove(manager,handle)
                with patch.object(FakeManager,'start_web',start),patch.object(FakeManager,'remove',remove), \
                     patch.object(c.proof,'result_bytes') as output:
                    with self.assertRaises(c.proof.EvidenceError):self.engine().run()
                    output.assert_not_called()
                self.assertNotIn('ledger.seal',self.w.trace)
                if failure=='start':self.assertNotIn('terminal.old.boot',self.w.trace)
                self.assertEqual(self.w.trace[-1],'stack.cleanup')

    def test_foreign_terminal_source_is_not_a_blocked_result(self):
        self.w.terminal_old_boot=True
        self.w.terminal_previous_source='f'*40
        with patch.object(c.proof,'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.EvidenceError,'terminal-source'):self.engine().run()
            output.assert_not_called()
        self.assertNotIn('ledger.seal',self.w.trace)

    def test_completed_row_worker_and_forward_negative_flags_not_defaulted(self):
        self.w.false_case='read';self.w.worker_panic=1
        self.w.digests['forward']='f'*64
        result=json.loads(self.engine().run())
        self.assertFalse(result['forward']);self.assertFalse(result['old_server_ok']['read'])
        self.assertFalse(result['old_server_ok']['reverse']);self.assertFalse(result['old_server_ok']['workers'])

    def http_write_case(self, write_number, case, *, late_failure=False):
        """Real controller/Sweep plus fake HTTP; other adapters remain ordered mocks."""
        http = tc.test_sweep.WriteReadbackWorld([case]); frames = []; writes = []
        original = FakeManager.controller
        def controller(manager, raw):
            request = c.controller.parse_request(raw); op = request['operation']
            if op == 'setup': return original(manager, raw)
            manager.w.event('controller.' + op)
            http.f = c.controller._fixture(request['fixture'])
            if op == 'write':
                writes.append(request['nonce'])
                http.mismatch_readbacks = {http.readbacks + 1} if len(writes) == write_number else set()
            if late_failure and op == 'write' and len(writes) == write_number:
                final_read = http.readbacks + 2
                http.intercept = lambda m, p, b: c.controller.sweep.Reply(401, (), b'private') if (
                    http.readbacks == final_read and p == '/api/chatbot/settings') else None
            def factory(value):
                return c.controller.sweep.Sweep(value, product=http.transport(), stub=http.transport(True),
                    wall=lambda: tc.test_sweep.WALL, sleep=lambda _: None)
            response = c.controller.handle(raw, sweep_factory=factory)
            value = c.controller._parse(response); frames.append((request, value))
            # Emulate the real manager's refusal: a failed private frame is not
            # an execution-completed row. Never assign proof flags on this path.
            c.need(value['status'] == 'ok', 'mock-controller-failed')
            c.controller.validate_receipts(request, value)
            return response
        engine = self.engine()
        engine.inputs = replace(engine.inputs, organization_id=tc.test_sweep.ORG)
        return engine, http, frames, controller

    def test_real_write_readback_false_keeps_reverse_reads_and_complete_engine_order(self):
        for write_number in (2, 3):
            for case in c.proof.SWEEP_CASES:
                with self.subTest(write_number=write_number, case=case):
                    self.w.trace.clear()
                    engine, http, frames, controller = self.http_write_case(write_number, case)
                    with patch.object(FakeManager, 'controller', controller): result = json.loads(engine.run())
                    self.assertEqual(result['forward'], write_number != 2)
                    self.assertEqual(result['old_server_ok']['write'], write_number != 3)
                    self.assertTrue(result['old_server_ok']['read'])
                    self.assertTrue(result['old_server_ok']['reverse'])
                    self.assertTrue(result['old_server_ok']['workers']); self.assertTrue(result['reversible'])
                    write_frames = [value for request, value in frames if request['operation'] == 'write']
                    self.assertEqual(write_frames[write_number - 1]['cases'],
                                     {key: key != case for key in c.proof.SWEEP_CASES})
                    read_frames = [(request, value) for request, value in frames if request['operation'] == 'read']
                    self.assertEqual(read_frames[0][0]['record'], write_frames[1]['record'])
                    self.assertEqual(read_frames[1][0]['record'], write_frames[2]['record'])
                    self.assertTrue(all(all(value['cases'].values()) for _, value in read_frames))
                    self.assertEqual(http.readbacks, 8)  # 3x(initial + follow-up), 2 independent reads.
                    self.assertEqual(len(http.contacts), 3)
                    trace = self.w.trace
                    self.assertEqual([v for v in trace if v.startswith('controller.')],
                        ['controller.setup', 'controller.write', 'controller.write', 'controller.read',
                         'controller.write', 'controller.read'])
                    self.assertEqual(trace.count('migration.compat-fwd'), 2)
                    self.assertIn('migration.compat-n1-rw', trace); self.assertIn('migration.compat-n1-ro', trace)
                    self.assertIn('pg.control', trace)
                    self.assertLess(trace.index('workers.end'), len(trace) - 1 - trace[::-1].index('controller.read'))
                    self.assertLess(trace.index('ledger.seal'), trace.index('catalog.final'))
                    self.assertEqual(trace[-1], 'stack.cleanup')

    def test_actual_write_false_followed_by_incomplete_readback_never_emits_result(self):
        engine, http, frames, controller = self.http_write_case(2, 'contacts', late_failure=True)
        with patch.object(FakeManager, 'controller', controller), patch.object(c.proof, 'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.EvidenceError, 'mock-controller-failed'): engine.run()
            output.assert_not_called()
        self.assertEqual(frames[-1][1], {'version': 1, 'status': 'error', 'failure': 'semantic'})
        self.assertEqual(http.readbacks, 4)
        self.assertNotIn('workers.begin', self.w.trace)
        self.assertEqual(self.w.trace[-1], 'stack.cleanup')

    def test_previous_seed_false_is_still_missing_baseline_not_compatibility_result(self):
        engine, http, _, controller = self.http_write_case(1, 'template')
        with patch.object(FakeManager, 'controller', controller), patch.object(c.proof, 'result_bytes') as output:
            with self.assertRaisesRegex(c.proof.EvidenceError, 'previous-seed-failed'): engine.run()
            output.assert_not_called()
        self.assertEqual(http.readbacks, 2)
        self.assertNotIn('catalog.initial', self.w.trace)
        self.assertEqual(self.w.trace[-1], 'stack.cleanup')

    def test_living_worker_false_still_performs_reverse_seal_catalog_and_cleanup(self):
        for kind in ('panic','unknown'):
            with self.subTest(kind=kind):
                self.w.trace.clear();self.w.worker_panic=int(kind=='panic');self.w.worker_unknown=int(kind=='unknown')
                result=json.loads(self.engine().run());trace=self.w.trace
                self.assertFalse(result['old_server_ok']['workers'])
                self.assertTrue(result['old_server_ok']['boot']);self.assertTrue(result['old_server_ok']['reverse'])
                self.assertTrue(result['forward']);self.assertTrue(result['reversible'])
                self.assertEqual([v for v in trace if v.startswith('controller.')],
                    ['controller.setup','controller.write','controller.write','controller.read','controller.write','controller.read'])
                self.assertEqual(trace.count('web.candidate'),2)
                self.assertLess(trace.index('workers.end'),len(trace)-1-trace[::-1].index('controller.read'))
                self.assertLess(trace.index('ledger.seal'),trace.index('catalog.final'))
                self.assertEqual(trace[-1],'stack.cleanup')

    def test_living_worker_false_does_not_suppress_required_later_failures(self):
        self.w.worker_panic=1
        for point in ('ledger.seal','catalog.final','stack.cleanup'):
            with self.subTest(point=point):
                self.w.trace.clear();self.w.fail=point
                with patch.object(c.proof,'result_bytes') as output:
                    with self.assertRaises(c.proof.EvidenceError):self.engine().run()
                    output.assert_not_called()
        self.w.fail=None

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

    def test_overall_reserved_cleanup_and_result_share_absolute_deadline(self):
        overall=c.image_admission._Deadline(clock=self.w.clock)
        self.w.clock.now+=1500
        engine=c._Engine(self.p,PREVIOUS,SHA,1,1,c.schema_change.Classification('none',False,(),()),
            FakeContract(),images(),'docker',self.p,clock=self.w.clock,sleep=self.w.clock.sleep,overall=overall)
        self.assertEqual(engine.seconds,720)
        # Use the real overall wrapper with the existing fake Docker boundary.
        with patch.object(c.runtime,'bounded_command',side_effect=engine.owner.raw_capture):
            result=json.loads(engine.run())
        self.assertTrue(result['forward']);self.assertTrue(engine.owner._closed)
        self.assertLessEqual(overall.remaining(),840)

    def test_elapsed_overall_never_invokes_cleanup_or_publishes(self):
        overall=c.image_admission._Deadline(clock=self.w.clock)
        engine=c._Engine(self.p,PREVIOUS,SHA,1,1,c.schema_change.Classification('none',False,(),()),
            FakeContract(),images(),'docker',self.p,clock=self.w.clock,sleep=self.w.clock.sleep,overall=overall)
        def measure():
            engine.owner.start();self.w.clock.now+=2401
            raise c.proof.InfrastructureFailure('synthetic-incomplete')
        with patch.object(engine,'_measure',side_effect=measure),patch.object(c.proof,'result_bytes') as encode:
            with self.assertRaisesRegex(c.proof.InfrastructureFailure,'admission-deadline'):engine.run()
            encode.assert_not_called()
        self.assertNotIn('stack.cleanup',self.w.trace);self.assertFalse(engine.owner._closed)

    def test_overall_cleanup_foreign_replacement_never_called(self):
        overall=c.image_admission._Deadline(clock=self.w.clock)
        engine=c._Engine(self.p,PREVIOUS,SHA,1,1,c.schema_change.Classification('none',False,(),()),
            FakeContract(),images(),'docker',self.p,clock=self.w.clock,sleep=self.w.clock.sleep,overall=overall)
        foreign_calls=[]
        def foreign(*args,**kwargs):foreign_calls.append(1)
        def close(manager):manager.owner.capture=foreign
        with patch.object(c.runtime,'bounded_command',side_effect=engine.owner.raw_capture), \
             patch.object(FakeManager,'close',close),patch.object(c.proof,'result_bytes') as encode:
            with self.assertRaisesRegex(c.proof.EvidenceError,'concurrent-transport'):engine.run()
            encode.assert_not_called()
        self.assertEqual(foreign_calls,[]);self.assertIs(engine.owner.capture,foreign)
        self.assertNotIn('stack.cleanup',self.w.trace)


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.p=Path(self.tmp.name)

    def test_public_entrypoint_cannot_accept_arbitrary_images_or_proof(self):
        classification=c.schema_change.Classification('none',False,(),())
        with patch.object(c.runtime,'verified_previous',return_value=PREVIOUS) as verify, \
             patch.object(c.schema_change,'classify',return_value=classification) as classify, \
             patch.object(c.catalog,'CatalogContract',return_value=FakeContract()), \
             patch.object(c.stack,'OwnedStack') as docker:
            with self.assertRaisesRegex(c.proof.EvidenceError,'source-build-admission-unreviewed'):
                c.run_experiment(self.p,SHA,123,2,object(),docker_binary='docker',config_dir=self.p)
            verify.assert_not_called();classify.assert_not_called()
            docker.assert_not_called()
            with self.assertRaises(TypeError):
                c.run_experiment(self.p,SHA,123,2,object(),docker_binary='docker',config_dir=self.p,images=images())

    def test_active_or_blocked_classifier_never_reaches_image_or_docker(self):
        for kind,dormant,reasons in [('expand',False,()),('contract',False,()),('data',False,()),('none',False,('blocked',))]:
            with patch.object(c.runtime,'verified_previous',return_value=PREVIOUS), \
                 patch.object(c.schema_change,'classify',return_value=c.schema_change.Classification(kind,dormant,reasons,())), \
                 patch.object(c.image_admission,'_READY_FOR_EXECUTION',True), \
                 patch.object(c.image_admission,'_verification_transport',return_value=nullcontext()), \
                 patch.object(c,'_admit_images') as admission:
                with self.assertRaisesRegex(c.proof.EvidenceError,'classification-unsupported'):
                    c.run_experiment(self.p,SHA,1,1,object(),docker_binary='docker',config_dir=self.p,image_work_dir=self.p)
                admission.assert_not_called()

    def test_source_run_attempt_and_rollback_inputs_validate_before_verification(self):
        with patch.object(c.runtime,'verified_previous') as verify:
            for sha,run,attempt,target in [('bad',1,1,None),(SHA,True,1,None),(SHA,1,0,None),(SHA,1,1,'main')]:
                with self.assertRaises(c.proof.EvidenceError):
                    c.run_experiment(self.p,sha,run,attempt,object(),docker_binary='docker',config_dir=self.p,rollback_target=target)
            verify.assert_not_called()

    def test_previous_sha_expectation_is_restrictive_and_checked_before_admission(self):
        with patch.object(c.runtime,'verified_previous') as verify:
            for value in ('short','A'*40,False,1,'a'*40+'\n'):
                with self.subTest(value=value),self.assertRaisesRegex(c.proof.EvidenceError,'previous-expectation-invalid'):
                    c.run_experiment(self.p,SHA,1,1,object(),docker_binary='docker',config_dir=self.p,
                                     expected_previous_sha=value)
            verify.assert_not_called()
        with patch.object(c.image_admission,'_READY_FOR_EXECUTION',True), \
             patch.object(c.image_admission,'_verification_transport',return_value=nullcontext()), \
             patch.object(c.runtime,'verified_previous',return_value=PREVIOUS) as verify, \
             patch.object(c.schema_change,'classify') as classify,patch.object(c,'_admit_images') as admission:
            with self.assertRaisesRegex(c.proof.EvidenceError,'previous-expectation-mismatch'):
                c.run_experiment(self.p,SHA,1,1,object(),docker_binary='docker',config_dir=self.p,
                                 image_work_dir=self.p,expected_previous_sha='f'*40)
            verify.assert_called_once();classify.assert_not_called();admission.assert_not_called()

    def test_same_deadline_spans_verification_catalog_admission_engine(self):
        clock=to.Clock();deadline=c.image_admission._Deadline(clock=clock);seen={}
        def verify(*args,**kwargs):clock.now+=100;return PREVIOUS
        def admission(*args,**kwargs):
            self.assertIs(args[-1],deadline);clock.now+=1400;return images()
        def contract(*args,**kwargs):
            seen['capture']=kwargs['capture'];return FakeContract()
        class Engine:
            def __init__(inner,*args,**kwargs):
                self.assertIs(kwargs['overall'],deadline)
                self.assertEqual(deadline.experiment_seconds(),720)
            def run(inner):return b'mock-result'
        with patch.object(c.image_admission,'_Deadline',return_value=deadline), \
             patch.object(c.image_admission,'_READY_FOR_EXECUTION',True), \
             patch.object(c.image_admission,'_verification_transport',return_value=nullcontext()), \
             patch.object(c.runtime,'verified_previous',side_effect=verify), \
             patch.object(c.schema_change,'classify',return_value=c.schema_change.Classification('none',False,(),())), \
             patch.object(c.catalog,'CatalogContract',side_effect=contract), \
             patch.object(c,'_admit_images',side_effect=admission),patch.object(c,'_Engine',Engine):
            self.assertEqual(c.run_experiment(self.p,SHA,1,1,object(),docker_binary='docker',config_dir=self.p,
                                             image_work_dir=self.p,expected_previous_sha=PREVIOUS.sha),b'mock-result')
        self.assertIs(seen['capture'].__self__,deadline)


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

    def test_native_terminal_observer_bridge_restores_and_preserves_failure_latches(self):
        h=self.harness();h.terminal();observer=h.old_observer();original=h.owner.capture
        with c._Budget(h.owner,clock=h.clock,seconds=200) as budget:
            with budget.window(7):value=budget.observer_call(observer.wait_old_boot)
            self.assertEqual(value['state'],'observed-semantic-failure')
            self.assertIs(h.owner.capture,budget.transport)
        self.assertIs(h.owner.capture,original)
        self.assertFalse(h.manager._protocol_failed)
        self.assertTrue(all(options.get('timeout',0)<=7 for args,options in h.world.calls
                            if args[:2]==['logs','--timestamps']))
        h.manager.remove(h.handle);h.manager.close();h.owner.cleanup()

    def test_native_living_worker_semantic_finish_uses_same_owned_budget_bridge(self):
        h=self.harness();observer,worker=h.measured_worker_failure();original=h.owner.capture
        with c._Budget(h.owner,clock=h.clock,seconds=100) as budget:
            with budget.window(7):last=budget.observer_call(observer.finish_period)
            self.assertEqual(last['panic_count'],worker['panic_count'])
            self.assertIs(h.owner.capture,budget.transport)
            self.assertFalse(h.manager._protocol_failed)
        self.assertIs(h.owner.capture,original)
        h.manager.remove(h.handle);h.manager.close();h.owner.cleanup()

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
