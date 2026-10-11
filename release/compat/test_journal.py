"""Actual adapter composition over independent fake Docker/HTTP, no live I/O."""
from dataclasses import asdict, replace
import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parent))
import journal as j
import test_controller as tc
import test_processes as tp
import test_stack as ts
import test_sweep as sw
import test_observation as to

TIME = '2026-10-11T00:00:02Z'


class GraphWorld(sw.World):
    def __init__(self):
        super().__init__()
        self.business = tc.BUSINESS
        self.accounts, self.rejected = [], []
        self.status_queue, self.auto_statuses = [], True

    def add(self, **row):
        super().add(time=TIME, **row)

    def status(self, wamid, status, **extra):
        self.add(kind='webhook',method='POST',route='status',status=200,
                 phone_number_id=self.f.phone_number_id,business_account_id=self.business,
                 message_id=wamid,delivery_status=status,attempt=1,**extra)

    def settle(self):
        for wamid in self.status_queue:
            self.status(wamid,'sent');self.status(wamid,'delivered')
        self.status_queue.clear()

    def product(self, method, path, value):
        result = super().product(method,path,value)
        if path.endswith('/legacy-whatsapp-replies'):
            wamid = 'wamid.' + str(len(self.entries)).zfill(40)
            self.entries[-1].update(message_id=wamid,business_account_id=self.business)
            self.status_queue.append(wamid)
            if self.auto_statuses:self.settle()
        return result

    def stub(self, method, path, value):
        if path.startswith('/_control/journal?') or path.startswith('/_control/rejected?'):
            ring = path.split('/')[2].split('?')[0]
            after = int(path.split('after=')[1].split('&')[0])
            rows = self.entries if ring == 'journal' else self.rejected
            result = {'entries':[dict(row) for row in rows if row['seq']>after][:1000],
                      'latest':rows[-1]['seq'] if rows else 0}
            # Exact Graph defer semantics: this successful GET is NOT in its
            # own returned snapshot, even for the separate rejected ring.
            self.add(kind='control',method='GET',route=ring,status=200)
            return result
        if path == '/_control/accounts':
            if method == 'POST':
                self.accounts = [dict(value)]
                self.add(kind='control',method='POST',route='accounts',status=200,
                         phone_number_id=self.f.phone_number_id,business_account_id=self.business)
                return {'ok':True}
            self.add(kind='control',method='GET',route='accounts',status=200)
            return {'accounts':copy.deepcopy(self.accounts)}
        result = super().stub(method,path,value)
        self.entries[-1].update(method='POST',business_account_id=self.business,message_type='text',
                               to=value['from'],text_sha256=hashlib.sha256(value['text'].encode()).hexdigest())
        self.add(kind='control',method='POST',route='inbound',status=200,
                 phone_number_id=self.f.phone_number_id,message_id=value['message_id'])
        return result

    def factory(self, fixture):
        assert fixture == self.f
        return j.controller.sweep.Sweep(fixture,product=self.transport(),stub=self.transport(True),
                                       wall=lambda:sw.WALL,sleep=lambda _:None)


class DockerWorld(tp.World):
    def __init__(self):
        super().__init__();self.http=GraphWorld();self.change_response=lambda x:x;self.raw=to.startup()

    def capture(self, argv, **options):
        if argv[5:7] == ['logs','--timestamps']:
            self.calls.append((argv[5:],options))
            return j.processes.runtime.Capture(0,b'',self.raw)
        if argv[5:9] == ['container','start','--attach','--interactive']:
            def ready(origin, namespace, role):
                assert namespace == self.http.f.namespace
                assert origin == (self.http.f.product_origin if role=='web' else self.http.f.stub_origin)
                def request(method,path,headers,body,timeout):
                    assert method=='GET' and path==('/ready' if role=='web' else '/readyz')
                    return j.controller.sweep.Reply(200,(),b'{}')
                return SimpleNamespace(origin=origin,request=request)
            raw=j.controller.handle(options['input_bytes'],sweep_factory=self.http.factory,http_factory=ready)
            value=self.change_response(json.loads(raw))
            self.response=json.dumps(value).encode()
            self.response_exit=0 if value['status']=='ok' else 1
        result=super().capture(argv,**options)
        if argv[5:7] == ['container','create'] and result.returncode==0:
            row=self.containers[result.stdout.decode()]
            row['HostConfig']['LogConfig']={'Type':'json-file','Config':{}}
            row['Config']['Tty']=False
        return result


class JournalTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.world=DockerWorld()
        self.owner=j.processes.stack.OwnedStack('docker',Path(self.tmp.name),ts.SOURCE,
            j.processes.stack.GraphImage(ts.GRAPH,ts.SOURCE),token=ts.TOKEN,capture=self.world.capture)
        self.owner.start()
        self.manager=j.processes.OwnedProcesses(self.owner,
            j.processes.ImageBinding(tp.PREV,'sha256:'+'4'*64,'d'*40,tp.RECIPE),
            j.processes.ImageBinding(tp.CAND,tp.CAND,'e'*40,tp.RECIPE),
            j.processes.ImageBinding(tp.CONTROL,tp.CONTROL,'f'*40,tp.RECIPE))
        self.f=replace(tc.fixture(),namespace=self.owner.namespace,product_origin=self.owner.planned_web_origin,
            stub_origin=self.owner.graph_origin,account_name=self.owner.namespace+'-wa',
            email=self.owner.namespace+'-client@example.test')
        self.world.http.f=self.f
        pg=self.owner._handles['pg']
        self.provisioned=j.fixture.ProvisionedFixture(self.f,tc.BUSINESS,'0'*64,pg.container_id,pg.started_at,self.owner.token)
        self.ledger=j.JournalLedger(self.manager,self.provisioned)

    def request(self, operation, **extra):
        return json.loads(self.manager.controller(j.controller._encode(
            {'version':1,'operation':operation,'fixture':asdict(self.f),**extra})))

    def web(self):
        self.handle=self.manager.start_web('previous',tp.INPUTS)
        self.request('setup',business_account_id=tc.BUSINESS)

    def write(self):
        return self.request('write',nonce=sw.NONCE)

    def old_period(self, *, sleep=None):
        self.ledger.capture();self.web();self.write();self.ledger.capture()
        self.manager.remove(self.handle);self.ledger.capture()
        self.ledger.open_old_period()
        self.handle=self.manager.start_web('previous',tp.INPUTS)
        self.clock=to.Clock()
        self.observer=j.observation.OwnedObservation.for_old_boot(self.manager,self.handle,self.f,
            run_id=123,run_attempt=1,clock=self.clock,sleep=sleep or self.clock.sleep)
        self.assertIsNone(self.observer.wait_old_boot())
        self.assertTrue(self.observer.boot()['ready'])
        self.ledger.bind_old_period(self.handle,self.observer)

    def extra_send(self, *, suffix='z', statuses=True, status_first=False, **changed):
        shape=next(dict(v) for v in self.world.http.entries if v.get('kind')=='graph')
        for key in ('seq','time'):shape.pop(key)
        shape.update(message_id='wamid.'+suffix*40,**changed)
        if statuses and status_first:
            self.world.http.status(shape['message_id'],'sent');self.world.http.status(shape['message_id'],'delivered')
        self.world.http.add(**shape)
        if statuses and not status_first:
            self.world.http.status(shape['message_id'],'sent');self.world.http.status(shape['message_id'],'delivered')
        return shape['message_id']

    def finish_old_period(self, workers):
        self.ledger.capture()
        self.observer.finish_period()
        self.manager.remove(self.handle)
        return self.ledger.close_old_period(self.observer,workers)

    def test_exact_extra_send_before_quiet_is_false_once_after_actual_complete_period(self):
        self.old_period();self.extra_send()
        self.assertEqual(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        self.assertEqual(workers['unexpected_stub_entries'],0)
        complete=self.finish_old_period(workers)
        self.assertEqual(complete['unexpected_stub_entries'],1)
        self.assertEqual(workers['unexpected_stub_entries'],0) # No caller row mutation.
        self.assertEqual(complete['observed_seconds'],60)
        self.assertFalse(self.manager._protocol_failed);self.assertFalse(self.ledger._failed)
        self.assertTrue(self.ledger.seal().sealed)
        self.manager.close();self.owner.cleanup()
        public=json.dumps(self.ledger.evidence())
        for private in (self.f.phone_number_id,tc.BUSINESS,sw.NONCE,'wamid.'+'z'*40):
            self.assertNotIn(private,public)

    def test_quiet_extra_send_and_two_statuses_reconcile_to_one_send_not_three(self):
        def sleep(seconds):
            if self.clock.now==10:self.extra_send(status_first=True)
            self.clock.sleep(seconds)
        self.old_period(sleep=sleep)
        workers=self.observer.workers()
        self.assertEqual(workers['unexpected_stub_entries'],3)
        self.assertEqual(self.finish_old_period(workers)['unexpected_stub_entries'],1)
        self.assertTrue(self.ledger.seal().sealed)

    def test_postquiet_pre_stop_extra_is_in_whole_period(self):
        self.old_period();workers=self.observer.workers();self.extra_send()
        self.assertEqual(self.finish_old_period(workers)['unexpected_stub_entries'],1)
        self.assertTrue(self.ledger.seal().sealed)

    def test_expected_old_write_remains_expected_and_does_not_count_as_extra(self):
        self.old_period()
        self.request('write',nonce='abcdef0123456789abcdef0123456789')
        self.assertEqual(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        self.assertEqual(self.finish_old_period(workers)['unexpected_stub_entries'],0)
        self.assertTrue(self.ledger.seal().sealed)

    def test_residual_statuses_can_cross_capture_but_missing_never_complete(self):
        self.old_period();wamid=self.extra_send(statuses=False)
        self.assertEqual(self.ledger.capture().pending,3)
        self.world.http.status(wamid,'sent')
        self.assertEqual(self.ledger.capture().pending,2)
        self.world.http.status(wamid,'delivered')
        self.assertEqual(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        self.assertEqual(self.finish_old_period(workers)['unexpected_stub_entries'],1)

    def test_missing_expected_or_residual_status_refuses_close_and_latches(self):
        self.old_period();self.extra_send(statuses=False)
        workers=self.observer.workers()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-incomplete'):
            self.finish_old_period(workers)
        self.assertTrue(self.ledger._failed);self.assertTrue(self.manager._protocol_failed)

    def test_residual_may_not_hide_missing_expected_send_or_foreign_fields(self):
        for changed in ({'to':'999000000009999'},{'phone_number_id':'999000000009998'},
                        {'business_account_id':'999000000009997'},{'text_sha256':'c'*64},
                        {'status':201},{'message_type':'image'},{'route':'other'},{'unknown':'private'}):
            with self.subTest(changed=changed):
                self.setUp();self.old_period();self.extra_send(**changed)
                with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()
                self.assertTrue(self.manager._protocol_failed)

    def test_extra_send_outside_bound_period_and_after_close_stays_refusal(self):
        self.ledger.capture();self.web();self.write();self.ledger.capture();self.extra_send()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-send'):self.ledger.capture()
        self.setUp();self.old_period();workers=self.observer.workers()
        self.finish_old_period(workers);self.extra_send()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-send'):self.ledger.seal()

    def test_old_boundary_pending_receipts_foreign_handle_and_new_app_refuse(self):
        self.ledger.capture();self.web();self.world.http.auto_statuses=False;self.write()
        self.manager.remove(self.handle)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-unsettled'):self.ledger.open_old_period()
        self.setUp();self.old_period()
        self.manager.remove(self.handle);self.handle=self.manager.start_web('candidate',tp.INPUTS)
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_window_tampering_and_unattributed_control_remain_refusal(self):
        self.old_period();workers=self.observer.workers()
        self.observer._journal_rows[0]['time']='2026-10-11T00:00:03Z'
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-worker-window-binding'):
            self.finish_old_period(workers)
        self.setUp();self.old_period()
        self.world.http.add(kind='control',method='POST',route='reset',status=200)
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_rejected_traffic_wrong_status_order_duplicate_and_gap_stay_refusal(self):
        for defect in ('rejected','status-order','duplicate','gap','attempt'):
            with self.subTest(defect=defect):
                self.setUp();self.old_period();wamid=self.extra_send()
                rows=self.world.http.entries
                statuses=[v for v in rows if v.get('message_id')==wamid and v.get('route')=='status']
                if defect=='rejected':self.world.http.rejected=[{'seq':1,'time':TIME,'status':401}]
                elif defect=='status-order':statuses[0]['delivery_status']='delivered'
                elif defect=='duplicate':self.world.http.status(wamid,'sent')
                elif defect=='attempt':statuses[0]['attempt']=2
                else:rows[-1]['seq']+=1
                with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_unclosed_period_and_unknown_status_never_seal(self):
        self.old_period();self.manager.remove(self.handle)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-unclosed'):self.ledger.seal()
        self.setUp();self.old_period();self.world.http.status('wamid.'+'q'*40,'sent')
        self.assertGreater(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        with self.assertRaises(j.proof.EvidenceError):self.finish_old_period(workers)

    def test_pending_status_before_send_is_not_evidence_until_complete_and_mixed_refuses(self):
        self.old_period();wamid='wamid.'+'z'*40
        self.world.http.status(wamid,'sent');self.world.http.status(wamid,'delivered')
        self.assertEqual(self.ledger.capture().pending,3)
        self.extra_send(statuses=False)
        self.assertEqual(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        workers['panic_count']=1
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-mixed-worker-failure-unsupported'):
            self.finish_old_period(workers)

    def test_other_known_extra_does_not_consume_missing_expected_send(self):
        self.old_period()
        self.request('write',nonce='abcdef0123456789abcdef0123456789')
        rows=self.world.http.entries
        missing=next(v['message_id'] for v in reversed(rows) if v.get('kind')=='graph')
        self.world.http.entries=[v for v in rows if v.get('message_id')!=missing]
        for seq,row in enumerate(self.world.http.entries,1):row['seq']=seq
        self.extra_send()
        self.assertGreater(self.ledger.capture().pending,1)
        workers=self.observer.workers()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-incomplete'):
            self.finish_old_period(workers)

    def test_binding_cannot_be_replayed_or_replace_handle_image_or_observer(self):
        self.old_period()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-order'):
            self.ledger.bind_old_period(self.handle,self.observer)
        self.setUp();self.old_period()
        self.manager.bindings['previous']=replace(self.manager.bindings['previous'],source_sha='a'*40)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-binding'):self.ledger.capture()
        self.setUp();self.old_period();workers=self.observer.workers()
        self.ledger.capture();self.observer.finish_period();self.manager.remove(self.handle)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-old-period-order'):
            self.ledger.close_old_period(object(),workers)

    def test_bounded_residual_count_cannot_turn_unbounded_traffic_into_result(self):
        self.old_period()
        for index in range(33):
            self.extra_send(suffix='abcdefghijklmnopqrstuvwxyz0123456'[index])
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-send'):self.ledger.capture()
        self.assertEqual(len(self.ledger._residual_sends),32)
        self.assertTrue(self.manager._protocol_failed)

    def test_terminal_period_can_close_without_worker_fabrication_only_with_zero_residual(self):
        self.ledger.capture();self.web();self.write();self.ledger.capture()
        self.manager.remove(self.handle);self.ledger.capture();self.ledger.open_old_period()
        self.handle=self.manager.start_web('previous',tp.INPUTS)
        self.world.raw=to.startup()+(to.TIME+' panic: synthetic old defect\n').encode()
        self.world.containers[self.handle.container_id]['State'].update(Running=False,Status='exited',ExitCode=2,
            FinishedAt='2026-10-11T00:00:03Z',OOMKilled=False,Error='')
        clock=to.Clock()
        observer=j.observation.OwnedObservation.for_old_boot(self.manager,self.handle,self.f,
            run_id=123,run_attempt=1,clock=clock,sleep=clock.sleep)
        terminal=observer.wait_old_boot()
        self.assertEqual(terminal['state'],'observed-semantic-failure')
        self.ledger.bind_old_period(self.handle,observer)
        self.manager.remove(self.handle)
        self.assertIsNone(self.ledger.close_old_period(observer,None))
        self.assertFalse(observer._workers_done);self.assertFalse(observer._booted)
        self.assertTrue(self.ledger.seal().sealed)

    def test_fresh_preboot_two_writes_read_and_poststop_seal(self):
        initial=self.ledger.capture()
        self.assertEqual(initial.pending,1)
        self.web(); first=self.write()
        self.ledger.capture()
        self.request('read',record=first['record'])
        second=self.request('write',nonce='abcdef0123456789abcdef0123456789')
        observed=self.ledger.capture()
        self.assertEqual(observed.pending,1)
        self.manager.remove(self.handle)
        sealed=self.ledger.seal()
        self.assertTrue(sealed.sealed)
        self.assertEqual(sealed.pending,1) # Explicit final GET outside snapshot.
        self.assertEqual(self.world.http.entries[-1]['route'],'journal')
        self.assertEqual(sealed.cursor,self.world.http.entries[-1]['seq']-1)
        self.assertNotEqual(first['record']['contact_id'],second['record']['contact_id'])
        for private in (self.f.password,self.f.control_key,self.f.phone_number_id,tc.BUSINESS,sw.NONCE):
            self.assertNotIn(private,json.dumps(self.ledger.evidence()))
            self.assertNotIn(private,repr(sealed))
        self.assertFalse(self.ledger.evidence()['full_compatibility_proof'])
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()
        self.manager.close();self.owner.cleanup()

    def test_expected_async_statuses_remain_pending_until_both_complete(self):
        self.ledger.capture();self.web();self.world.http.auto_statuses=False;self.write()
        pending=self.ledger.capture();self.assertEqual(pending.pending,3)
        self.world.http.settle()
        self.assertEqual(self.ledger.capture().pending,1)
        self.manager.remove(self.handle);self.assertTrue(self.ledger.seal().sealed)

    def test_missing_status_seal_fails_and_cannot_replay(self):
        self.ledger.capture();self.web();self.world.http.auto_statuses=False;self.write()
        self.manager.remove(self.handle)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-attribution-incomplete'):self.ledger.seal()
        count=len(self.world.calls)
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()
        self.assertEqual(len(self.world.calls),count)

    def test_prequiet_unrelated_send_is_never_discarded_by_internal_sweep_baselines(self):
        self.ledger.capture();self.web()
        self.world.http.add(kind='graph',method='POST',route='messages',status=200,
                            message_id='wamid.'+'z'*40,message_type='text',to='999000000000001',
                            phone_number_id=self.f.phone_number_id,business_account_id=tc.BUSINESS,
                            text_sha256='a'*64)
        self.write()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-send'):self.ledger.capture()

    def test_preboot_foreign_control_and_bad_status_refuse(self):
        self.world.http.add(kind='control',method='POST',route='reset',status=200)
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-entry'):self.ledger.capture()

    def test_rejected_bad_token_traffic_fails_without_public_payload(self):
        self.world.http.rejected=[{'seq':1,'time':TIME,'kind':'graph','status':401,'route':'auth','error':'secret'}]
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-rejected-traffic') as got:self.ledger.capture()
        self.assertNotIn('secret',str(got.exception));self.assertNotIn('secret',json.dumps(self.ledger.evidence()))

    def test_rejected_initial_truncation_is_not_accepted_unpublished(self):
        self.world.http.rejected=[{'seq':5}]
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_ring_truncation_duplicate_reset_or_missing_rows_refuse(self):
        self.ledger.capture()
        self.world.http.entries[0]['seq']=9
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_status_wrong_business_or_duplicate_never_allowed(self):
        self.ledger.capture();self.web();self.write()
        row=next(r for r in self.world.http.entries if r.get('delivery_status')=='sent')
        row['business_account_id']='999000000000099'
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-unexpected-status'):self.ledger.capture()

    def test_duplicate_status_cannot_consume_another_status(self):
        self.ledger.capture();self.web();self.write()
        row=next(r for r in self.world.http.entries if r.get('delivery_status')=='sent')
        self.world.http.add(**{k:v for k,v in row.items() if k not in {'seq','time'}})
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_send_text_or_message_id_drift_refuses(self):
        self.ledger.capture();self.web();self.write()
        row=next(r for r in self.world.http.entries if r.get('kind')=='graph')
        row['text_sha256']='b'*64
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_receipt_counts_tamper_refuses_before_ledger_can_accept(self):
        def corrupt(value):
            if value.get('operation')=='journal':value['stub_receipts']['calls'].append({'method':'GET','route':'journal','status':200})
            return value
        self.world.change_response=corrupt
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()
        self.assertTrue(self.manager._protocol_failed)

    def test_forged_consistent_receipt_count_still_needs_actual_rows(self):
        def corrupt(value):
            if value.get('operation')=='journal':
                value['journal']['requests']+=1;value['stub_receipts']['windows'][0]['requests']+=1
                value['stub_receipts']['calls'].append({'method':'GET','route':'journal','status':200})
            return value
        self.world.change_response=corrupt
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-attribution-incomplete'):self.ledger.seal()

    def test_immutable_receipt_prefix_and_owner_binding_required(self):
        self.ledger.capture();self.manager._controller_receipts[0]=b'{}'
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-receipts-changed'):self.ledger.capture()

    def test_start_after_activity_or_foreign_provisioned_fixture_refuses(self):
        self.ledger.capture()
        with self.assertRaises(j.proof.EvidenceError):j.JournalLedger(self.manager,self.provisioned)
        with self.assertRaises(j.proof.EvidenceError):j.JournalLedger(self.manager,replace(self.provisioned,owner_token='f'*32))

    def test_no_seal_while_web_running_and_no_success_default(self):
        self.web()
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-producers-active'):self.ledger.seal()
        self.assertFalse(self.ledger.evidence()['sealed'])

    def test_private_storage_bound_refuses(self):
        self.ledger._bytes=j.MAX_BYTES
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-history-bound'):self.ledger.capture()

    def test_badtoken_after_successful_window_is_not_lost_before_seal(self):
        self.ledger.capture();self.web();self.write();self.ledger.capture()
        self.manager.remove(self.handle)
        self.world.http.rejected=[{'seq':1,'time':TIME,'kind':'graph','method':'POST','route':'auth','status':401}]
        with self.assertRaisesRegex(j.proof.EvidenceError,'journal-rejected-traffic'):self.ledger.seal()

    def test_ring_page_overflow_or_removed_final_sequence_refuses(self):
        original=self.world.http.mutate
        self.world.http.mutate=lambda method,path,value: ({'entries':[],'latest':1} if '/journal?' in path else original(method,path,value))
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_missing_control_receipt_cannot_hide_an_observed_call(self):
        self.ledger.capture();self.web();self.write()
        # The controller parser refuses it before it can become an audit; a
        # consistent smaller receipt likewise leaves an unmatched actual row.
        def corrupt(value):
            if value.get('operation')=='journal':value.pop('stub_receipts')
            return value
        self.world.change_response=corrupt
        with self.assertRaises(j.proof.EvidenceError):self.ledger.capture()

    def test_status_scheduling_before_send_append_remains_pending_not_allowed(self):
        self.ledger.capture();self.web();self.write()
        rows=self.world.http.entries
        index=next(i for i,row in enumerate(rows) if row.get('kind')=='graph')
        send=rows.pop(index);rows.insert(index+2,send)
        for seq,row in enumerate(rows,1):row['seq']=seq
        self.assertEqual(self.ledger.capture().pending,1)
        self.manager.remove(self.handle);self.assertTrue(self.ledger.seal().sealed)


if __name__=='__main__':unittest.main(verbosity=2)
