"""Actual adapter composition over independent fake Docker/HTTP, no live I/O."""
from dataclasses import asdict, replace
import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parent))
import journal as j
import test_controller as tc
import test_processes as tp
import test_stack as ts
import test_sweep as sw

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
        super().__init__();self.http=GraphWorld();self.change_response=lambda x:x

    def capture(self, argv, **options):
        if argv[5:9] == ['container','start','--attach','--interactive']:
            raw=j.controller.handle(options['input_bytes'],sweep_factory=self.http.factory)
            value=self.change_response(json.loads(raw))
            self.response=json.dumps(value).encode()
            self.response_exit=0 if value['status']=='ok' else 1
        return super().capture(argv,**options)


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
