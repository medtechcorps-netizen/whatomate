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
            need(key in self._sends, 'journal-unexpected-send')
            del self._sends[key]
            self._wamids.add(wamid)
            self._statuses[wamid] = ['sent','delivered']
            return
        if shape.get('kind') == 'webhook' and shape.get('route') == 'status':
            self._unbound_statuses.append(shape)
            need(len(self._unbound_statuses) <= 32, 'journal-status-bound')
            return
        raise proof.EvidenceError('journal-unexpected-entry')

    def _match_statuses(self):
        deferred = []
        for shape in self._unbound_statuses:
            wamid = shape.get('message_id')
            if type(wamid) is str and _WAMID.fullmatch(wamid) is not None and wamid not in self._wamids and self._sends:
                # A status goroutine can win scheduling over the send handler's
                # deferred journal append. It never counts as accepted yet.
                deferred.append(shape)
                continue
            remaining = self._statuses.get(wamid)
            need(remaining, 'journal-unexpected-status')
            expected = {'kind':'webhook','method':'POST','route':'status','status':200,
                'phone_number_id':self.fixture.phone_number_id, 'business_account_id':self.provisioned.business_account_id,
                'message_id':wamid,'delivery_status':remaining[0],'attempt':1}
            need(shape == expected and type(shape.get('attempt')) is int, 'journal-unexpected-status')
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
                'failed':self._failed, 'sealed':self._sealed, 'full_compatibility_proof':False}
