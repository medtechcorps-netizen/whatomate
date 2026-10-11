"""Continuous private Graph accounting, not a worker or compatibility verdict.

Start before any application/controller call on an owned fresh Graph. Every
successful controller invocation contributes its independently collected stub
receipts. Journal rows never grant themselves permission. The final journal
GET is appended after its returned cursor; that single known self-read remains
outside a sealed snapshot. Raw entries and synthetic identities stay private.
"""
from __future__ import annotations

from collections import Counter
from dataclasses import asdict, dataclass
import datetime
import hashlib
import json
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import controller
import fixture
import observation
import processes

proof = processes.proof
MAX_ROWS = 20_000
MAX_BYTES = 4 * 1024 * 1024
_TIME = re.compile(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z\Z')
_WAMID = re.compile(r'wamid\.[A-Za-z0-9_-]{40}\Z')


def need(ok, code='journal-evidence-invalid'):
    if not ok:
        raise proof.EvidenceError(code)


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


@dataclass(frozen=True, repr=False)
class JournalSnapshot:
    cursor: int
    rejected_cursor: int
    rows: int
    pending: int
    sha256: str
    sealed: bool


class JournalLedger:
    def __init__(self, manager: processes.OwnedProcesses, provisioned: fixture.ProvisionedFixture):
        need(type(manager) is processes.OwnedProcesses and type(provisioned) is fixture.ProvisionedFixture)
        need(type(provisioned.fixture) is controller.sweep.Fixture)
        self.manager, self.provisioned, self.fixture = manager, provisioned, provisioned.fixture
        need(self.fixture.namespace == manager.owner.namespace
             and self.fixture.product_origin == manager.owner.planned_web_origin
             and self.fixture.stub_origin == manager.owner.graph_origin
             and type(provisioned.business_account_id) is str
             and controller.sweep._DIGITS.fullmatch(provisioned.business_account_id) is not None
             and provisioned.business_account_id != self.fixture.phone_number_id)
        self._binding = controller.sweep.Sweep(self.fixture)._binding()
        self._audits, self._rows = (), []
        self._expected = Counter()
        self._sends, self._statuses, self._unbound_statuses = {}, {}, []
        self._nonces, self._wamids = set(), set()
        self._known_sends, self._residual_sends, self._residual_rows = {}, {}, set()
        self._old_period = None
        self._cursor, self._rejected_cursor, self._bytes = 0, 0, 0
        self._failed, self._sealed, self._setup = False, False, False
        self._check()
        need(not manager._ids and not manager.controller_receipts(), 'journal-must-start-before-activity')

    def _check(self):
        need(not self._failed and not self._sealed and not self.manager._protocol_failed,
             'journal-ledger-unavailable')
        self.manager._assert_all()
        pg = self.manager.owner._handles['pg']
        need(self.provisioned.owner_token == self.manager.owner.token
             and self.provisioned.pg_container_id == pg.container_id
             and self.provisioned.pg_started_at == pg.started_at, 'journal-owner-changed')
        if self._old_period is not None and not self._old_period['closed']:
            self._period_check()

    def _period_check(self):
        period = self._old_period
        need(self.manager.owner.token == period['token'] and self.manager.bindings['previous'] is period['image'],
             'journal-old-period-binding')
        events = self.manager._events
        need(events[:len(period['events'])] == period['events'], 'journal-old-period-history')
        handle = period['handle']
        if handle is None:
            need(not self.manager._ids and events == period['events'], 'journal-old-period-unbound')
            return
        web = []
        for event in events[len(period['events']):]:
            need(event.get('outcome') == 'acknowledged', 'journal-old-period-history')
            if event.get('slot') == 'web':
                need(event.get('id') == handle.container_id, 'journal-old-period-history')
                web.append(event.get('operation'))
            else:
                need(event.get('slot') == 'controller' and event.get('operation') in
                     {'create','controller','remove'}, 'journal-old-period-history')
        need(web in (['create','start','configure'], ['create','start','configure','stop','remove'],
                     ['create','start','configure','remove']), 'journal-old-period-history')
        if 'web' in self.manager._handles:
            need(web == ['create','start','configure'] and self.manager._handles['web'] is handle
                 and self.manager._ids == {'web':handle.container_id}, 'journal-old-period-binding')
        else:
            need(not self.manager._ids and web[-1] == 'remove', 'journal-old-removal-unproven')

    def _settled(self):
        return (self._expected == Counter({encoded(self._control('GET','journal')):1})
                and not self._sends and not self._statuses and not self._unbound_statuses)

    def open_old_period(self):
        """Explicit preboot checkpoint; never infer phase authority from rows."""
        try:
            self._check()
            need(self._old_period is None and not self.manager._ids, 'journal-old-period-order')
            self.capture()
            need(self._settled() and self._setup and self._known_sends, 'journal-old-period-unsettled')
            self._old_period = {'after':self._cursor, 'end':None, 'closed':False, 'handle':None,
                'observer':None, 'token':self.manager.owner.token, 'image':self.manager.bindings['previous'],
                'events':[dict(v) for v in self.manager._events]}
        except Exception as error:
            self._fail(error)

    def bind_old_period(self, handle, observer):
        """Bind the actual returned old handle before accepting any new rows."""
        try:
            need(self._old_period is not None and self._old_period['handle'] is None
                 and not self._old_period['closed'], 'journal-old-period-order')
            need(type(handle) is processes.ProcessHandle and handle.slot == 'web'
                 and handle.version == 'previous' and handle.phase == 'compat-prev-server'
                 and handle.image_id == self._old_period['image'].image_id
                 and self.manager._handles.get('web') is handle
                 and type(observer) is observation.OwnedObservation
                 and observer.manager is self.manager and observer.handle is handle
                 and observer.fixture == self.fixture and not observer._failed,
                 'journal-old-period-binding')
            self._old_period.update(handle=handle, observer=observer)
            self._check()
        except Exception as error:
            self._fail(error)

    def close_old_period(self, observer, workers):
        """After exact removal, reconcile whole-period residuals with quiet rows.

        Only extra fully attributed sends are counted (once each); their two
        statuses are completeness obligations, not extra sends. Missing worker
        evidence is allowed only for an actual terminal-boot path with zero
        residuals. Mixed worker panic/unknown plus residual remains unsupported.
        """
        try:
            self._check()
            period = self._old_period
            need(period is not None and not period['closed'] and period['handle'] is not None
                 and observer is period['observer'] and not self.manager._ids,
                 'journal-old-period-order')
            self.capture()
            need(self._settled(), 'journal-old-period-incomplete')
            if workers is None:
                need(not self._residual_sends and observer._old_boot_done and not observer._booted and not observer._startup_ready
                     and not observer._failed, 'journal-old-terminal-incomplete')
                result = None
            else:
                need(observer._workers_done and observer._booted and observer._startup_ready
                     and observer._period_attempted and observer._period_summary is not None
                     and not observer._failed, 'journal-worker-observation-incomplete')
                fields = {'execution','observed_seconds','panic_count','unknown_type_count','unexpected_stub_entries'}
                need(type(workers) is dict and set(workers) == fields
                     and workers['execution'] == observation._completed()
                     and all(type(workers[k]) is int and 0 <= workers[k] <= MAX_ROWS for k in fields - {'execution'})
                     and 60 <= workers['observed_seconds'] <= 300, 'journal-worker-observation-incomplete')
                counts, rows = observer._journal_counts, observer._journal_rows
                need(type(counts) is dict and set(counts) == {'baseline_requests','window_requests','observed_read_rows','after','cursor'}
                     and all(type(v) is int and v >= 0 for v in counts.values())
                     and period['after'] <= counts['after'] <= counts['cursor'] <= self._cursor
                     and type(rows) is list, 'journal-worker-window-binding')
                expected_rows = self._rows[counts['after']:counts['cursor']]
                need([encoded(v) for v in rows] == expected_rows, 'journal-worker-window-binding')
                own = sum(observation._own_journal_read(v) for v in rows)
                other = {v['seq'] for v in rows if not observation._own_journal_read(v)}
                need(own == counts['observed_read_rows'] == counts['window_requests']
                     and len(other) == workers['unexpected_stub_entries']
                     and other <= self._residual_rows, 'journal-worker-window-unattributed')
                need(not self._residual_sends or workers['panic_count'] == workers['unknown_type_count'] == 0,
                     'journal-mixed-worker-failure-unsupported')
                result = {**workers, 'execution':dict(workers['execution']),
                          'unexpected_stub_entries':len(self._residual_sends)}
            period.update(closed=True, end=self._cursor)
            return result
        except Exception as error:
            self._fail(error)

    def _fail(self, error):
        self._failed = True
        self.manager._protocol_failed = True
        if isinstance(error, (proof.EvidenceError, proof.InfrastructureFailure)):
            raise error
        raise proof.InfrastructureFailure('journal-capture') from None

    @staticmethod
    def _control(method, route, **extra):
        return {'kind':'control', 'method':method, 'route':route, 'status':200, **extra}

    def _expect(self, value):
        self._expected[encoded(value)] += 1

    def _receive_audits(self):
        audits = self.manager.controller_receipts()
        need(type(audits) is tuple and audits[:len(self._audits)] == self._audits,
             'journal-receipts-changed')
        for raw in audits[len(self._audits):]:
            value = controller._parse(raw)
            operation = value.get('operation')
            keys = {'operation', 'fixture_binding', 'receipt'}
            if operation == 'write': keys.add('nonce')
            if operation == 'setup': keys.add('business_account_id')
            need(set(value) == keys and value['fixture_binding'] == self._binding,
                 'journal-receipt-binding')
            need(operation in {'setup','readiness','read','write','journal','rejected'})
            # OwnedProcesses already validated the complete request/response
            # grammar; retain immutable bytes and independently bind ownership.
            calls = value['receipt']['calls']
            if operation == 'setup':
                need(not self._setup and value['business_account_id'] == self.provisioned.business_account_id,
                     'journal-setup-binding')
                self._setup = True
            if operation == 'write':
                nonce = value['nonce']
                need(self._setup and nonce not in self._nonces and len(self._nonces) < 16,
                     'journal-write-binding')
                expected = controller.sweep.Sweep(self.fixture)._expected(nonce)
                self._nonces.add(nonce)
                common = {'phone_number_id':self.fixture.phone_number_id,
                          'business_account_id':self.provisioned.business_account_id}
                self._expect({'kind':'webhook','method':'POST','route':'inbound','status':200,
                    **common, 'message_id':expected['wamid'], 'message_type':'text', 'to':expected['phone'],
                    'text_sha256':hashlib.sha256(expected['inbound'].encode()).hexdigest()})
                send = {'kind':'graph','method':'POST','route':'messages','status':200, **common,
                        'message_type':'text', 'to':expected['phone'],
                        'text_sha256':hashlib.sha256(expected['outbound'].encode()).hexdigest()}
                key = encoded(send)
                need(key not in self._sends, 'journal-write-binding')
                self._sends[key] = nonce
                self._known_sends[key] = nonce
            for call in calls:
                extra = {}
                if call['route'] == 'accounts' and call['method'] == 'POST':
                    extra = {'phone_number_id':self.fixture.phone_number_id,
                             'business_account_id':self.provisioned.business_account_id}
                elif call['route'] == 'inbound':
                    need(operation == 'write', 'journal-receipt-binding')
                    extra = {'phone_number_id':self.fixture.phone_number_id, 'message_id':expected['wamid']}
                self._expect(self._control(call['method'], call['route'], **extra))
        self._audits = audits

    def _request(self, operation, after):
        raw = controller._encode({'version':1,'operation':operation,'fixture':asdict(self.fixture),
                                  'after':after,'discard_baseline':False})
        response = controller._parse(self.manager.controller(raw))
        return controller.journal_result(response[operation], after=after, discard_baseline=False)

    def _shape(self, row):
        need(type(row) is dict and type(row.get('seq')) is int
             and type(row.get('time')) is str and _TIME.fullmatch(row['time']) is not None,
             'journal-row-invalid')
        try:
            datetime.datetime.fromisoformat(row['time'].replace('Z','+00:00'))
        except ValueError:
            raise proof.EvidenceError('journal-row-invalid') from None
        need(type(row.get('status')) is int and row['status'] == 200, 'journal-unexpected-entry')
        return {k:v for k,v in row.items() if k not in {'seq','time'}}

    def _match(self, row):
        shape = self._shape(row)
        key = encoded(shape)
        if self._expected[key] > 0:
            self._expected[key] -= 1
            if not self._expected[key]: del self._expected[key]
            return
        if shape.get('kind') == 'graph' and shape.get('route') == 'messages':
            wamid = shape.get('message_id')
            need(type(wamid) is str and _WAMID.fullmatch(wamid) is not None and wamid not in self._wamids,
                 'journal-unexpected-send')
            send = {k:v for k,v in shape.items() if k != 'message_id'}
            key = encoded(send)
            if key in self._sends:
                del self._sends[key]
            else:
                period = self._old_period
                need(period is not None and not period['closed'] and period['handle'] is not None
                     and row['seq'] > period['after'] and key in self._known_sends
                     and len(self._residual_sends) < 32, 'journal-unexpected-send')
                self._residual_sends[wamid] = row['seq']
                self._residual_rows.add(row['seq'])
            self._wamids.add(wamid)
            self._statuses[wamid] = ['sent','delivered']
            return
        if shape.get('kind') == 'webhook' and shape.get('route') == 'status':
            self._unbound_statuses.append((row['seq'], shape))
            active = self._old_period is not None and not self._old_period['closed'] and self._old_period['handle'] is not None
            need(len(self._unbound_statuses) <= (96 if active else 32), 'journal-status-bound')
            return
        raise proof.EvidenceError('journal-unexpected-entry')

    def _match_statuses(self):
        deferred = []
        for seq, shape in self._unbound_statuses:
            wamid = shape.get('message_id')
            period = self._old_period
            if type(wamid) is str and _WAMID.fullmatch(wamid) is not None and wamid not in self._wamids and (
                    self._sends or (period is not None and not period['closed'] and period['handle'] is not None)):
                # A status goroutine can win scheduling over the send handler's
                # deferred journal append. It never counts as accepted yet.
                deferred.append((seq, shape))
                continue
            remaining = self._statuses.get(wamid)
            need(remaining, 'journal-unexpected-status')
            expected = {'kind':'webhook','method':'POST','route':'status','status':200,
                'phone_number_id':self.fixture.phone_number_id, 'business_account_id':self.provisioned.business_account_id,
                'message_id':wamid,'delivery_status':remaining[0],'attempt':1}
            need(shape == expected and type(shape.get('attempt')) is int, 'journal-unexpected-status')
            if wamid in self._residual_sends:
                need(period is not None and not period['closed'] and seq > period['after'], 'journal-unexpected-status')
                self._residual_rows.add(seq)
            remaining.pop(0)
            if not remaining: del self._statuses[wamid]
        self._unbound_statuses = deferred

    def _pending(self):
        return sum(self._expected.values()) + len(self._sends) + sum(map(len,self._statuses.values())) + len(self._unbound_statuses)

    def capture(self):
        """One bounded read of each ring; no polling, reset, or success default."""
        try:
            self._check()
            rejected = self._request('rejected', self._rejected_cursor)
            need(not rejected['entries'] and rejected['cursor'] == self._rejected_cursor,
                 'journal-rejected-traffic')
            observed = self._request('journal', self._cursor)
            self._receive_audits()
            for row in observed['entries']:
                need(row['seq'] == self._cursor + 1, 'journal-history-gap')
                raw = encoded(row)
                self._bytes += len(raw)
                need(len(self._rows) < MAX_ROWS and self._bytes <= MAX_BYTES, 'journal-history-bound')
                self._match(row)
                self._rows.append(raw)
                self._cursor = row['seq']
            need(self._cursor == observed['cursor'], 'journal-history-gap')
            self._match_statuses()
            self._check()
            return self._snapshot(False)
        except Exception as error:
            self._fail(error)

    def _snapshot(self, sealed):
        return JournalSnapshot(self._cursor, self._rejected_cursor, len(self._rows), self._pending(),
                               hashlib.sha256(b'\n'.join(self._rows)).hexdigest(), sealed)

    def seal(self):
        """After all application producers stop, require complete accounting.

        Asynchronous expected statuses must already have settled while the web
        remained alive. Caller may make bounded capture() observations first;
        a failed seal consumes this ledger and never retries a mutation.
        """
        try:
            self._check()
            need(not self.manager._ids, 'journal-producers-active')
            need(self._old_period is None or self._old_period['closed'], 'journal-old-period-unclosed')
            self.capture()
            final_read = Counter({encoded(self._control('GET','journal')):1})
            need(self._expected == final_read and not self._sends and not self._statuses
                 and not self._unbound_statuses, 'journal-attribution-incomplete')
            self._sealed = True
            return self._snapshot(True)
        except Exception as error:
            self._fail(error)

    def evidence(self):
        """Safe metadata only; no rows, fixture values, IDs or case flags."""
        return {'cursor':self._cursor, 'rejected_cursor':self._rejected_cursor,
                'rows':len(self._rows), 'bytes':self._bytes, 'receipts':len(self._audits),
                'pending':self._pending(), 'sha256':hashlib.sha256(b'\n'.join(self._rows)).hexdigest(),
                'old_period':None if self._old_period is None else {
                    'after':self._old_period['after'], 'end':self._old_period['end'],
                    'closed':self._old_period['closed'], 'extra_sends':len(self._residual_sends)},
                'failed':self._failed, 'sealed':self._sealed, 'full_compatibility_proof':False}
