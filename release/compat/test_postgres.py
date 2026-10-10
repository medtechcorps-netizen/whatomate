from __future__ import annotations

import copy
import json
from pathlib import Path
import re
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import postgres as pg
import proof
import runtime


STATE = ['state', 17, 'on', 'repeatable read', 'UTF8', True, True, True, True, 0,
         'off', 'mod', 'error', 'verbose', '%m [%p] %a %e ', 'UTC', 'C']
COLUMN = ['value', 'pg_catalog', 'text', -1, 1]
TOKEN = 'a' * 32
ID = 'b' * 64


def output(*rows):
    return b''.join(json.dumps(row, ensure_ascii=False, separators=(',', ':')).encode() + b'\n' for row in rows)


def line(app, level, body, state='00000', pid='20'):
    if level in proof._PRIMARY:
        body = state + ': ' + body
    return f'2026-10-11 01:00:00.000 UTC [{pid}] {app} {state} {level}:  {body}\n'.encode()


def barrier(token, part):
    return (line('compat-observer', 'LOG', 'compat-boundary-' + token + '-' + part)
            + line('compat-observer', 'CONTEXT', 'PL/pgSQL function inline_code_block line 1 at RAISE')
            + line('compat-observer', 'LOCATION', 'exec_stmt_raise, pl_exec.c:3911')
            + line('compat-observer', 'STATEMENT', "DO $$ BEGIN RAISE LOG 'compat-boundary-" + token + '-' + part + "'; END $$;"))


class FakeDocker:
    def __init__(self):
        self.info = {'Id': ID, 'Image': 'sha256:' + 'c' * 64,
                     'Config': {'Image': runtime.POSTGRES, 'Labels': {pg.LABEL: TOKEN}},
                     'State': {'Running': True, 'StartedAt': '2026-10-11T00:00:00Z'},
                     'HostConfig': {'Binds': None, 'PortBindings': {}, 'NetworkMode': 'none'},
                     'Mounts': []}
        self.state = copy.deepcopy(STATE)
        self.inventory = [['fixture', 'r', [copy.deepcopy(COLUMN)]]]
        self.rows = [['row', 'fixture', ['alpha']], ['row', 'fixture', [None]]]
        self.calls = []
        self.stdout = b''
        self.stderr = b'boot log excluded\n'
        self.probe = False
        self.events = []
        self.function = 'fixed private synthetic function'
        self.bad_complete = False
        self.fail_sql = False
        self.mutate_after_data = False
        self.omit_end = False
        self.network = {'Id': 'd'*64, 'Internal': True, 'Labels': {pg.LABEL: TOKEN}}

    def __call__(self, argv, **options):
        self.calls.append((argv, options))
        args = argv[5:]
        if args == ['inspect', ID]:
            return runtime.Capture(0, json.dumps([self.info]).encode(), b'')
        if args[:2] == ['network', 'inspect']:
            return runtime.Capture(0, json.dumps([self.network]).encode(), b'')
        if args == ['logs', ID]:
            return runtime.Capture(0, self.stdout, self.stderr)
        if args[:2] != ['exec', '-i']:
            raise AssertionError('unexpected fixed route')
        script = options['input_bytes'].decode()
        if self.fail_sql:
            return runtime.Capture(3, b'private', b'private')
        if "json_build_array('inventory'" in script:
            token = re.search(r"json_build_array\('complete','([0-9a-f]{32})'\)", script)[1]
            data = output(self.state, ['inventory', self.inventory], *self.rows,
                          ['complete', 'wrong' if self.bad_complete else token])
            if self.mutate_after_data:
                self.info['State']['StartedAt'] = '2026-10-11T00:01:00Z'
            return runtime.Capture(0, data, b'')
        if 'CREATE SCHEMA compat_probe' in script:
            if self.probe:
                return runtime.Capture(3, b'', b'private existing schema')
            self.probe = True
            return runtime.Capture(0, output(['installed']), b'')
        if "json_build_array('probe'" in script:
            return runtime.Capture(0, output(['probe', self.function,
                [['compat_probe_drop', 'sql_drop', 'O', 'compat_probe', 'mark', 0, True],
                 ['compat_probe_start', 'ddl_command_start', 'O', 'compat_probe', 'mark', 0, True]],
                self.events[-1][0] if self.events else 0]), b'')
        if 'FROM compat_probe.events WHERE id>' in script:
            low = int(re.search(r'WHERE id>([0-9]+)', script)[1])
            return runtime.Capture(0, output(*(e for e in self.events if e[0] > low), ['complete']), b'')
        if script.startswith('DO $$'):
            token, part = re.search(r'compat-boundary-([0-9a-f]{32})-(begin|end)', script).groups()
            if not (part == 'end' and self.omit_end):
                self.stderr += barrier(token, part)
            return runtime.Capture(0, b'', b'')
        if "json_build_array('state'" in script:
            return runtime.Capture(0, output(self.state), b'')
        raise AssertionError('unexpected SQL contract')


class CollectorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.config = Path(self.tmp.name).resolve()
        self.fake = FakeDocker()
        self.db = pg.SyntheticPostgres('docker', self.config, ID, TOKEN, 'compat_runtime', capture=self.fake,
                                       base_env={'PATH': 'safe', 'PGHOST': 'private', 'DOCKER_HOST': 'private'})

    def tearDown(self):
        self.tmp.cleanup()

    def test_actual_fixed_transport_and_complete_capture(self):
        capture = self.db.capture_tables()
        self.assertEqual(set(capture.public()), {'complete', 'digests'})
        self.assertEqual(capture.identities(), [pg.identity('fixture')])
        self.assertNotIn('fixture', repr(capture))
        sql_calls = [c for c in self.fake.calls if c[0][5:7] == ['exec', '-i']]
        self.assertEqual(len(sql_calls), 1)
        argv, options = sql_calls[0]
        self.assertIn('127.0.0.1', argv)
        self.assertIn('compat', argv)
        self.assertNotIn('PGHOST', options['env'])
        self.assertNotIn('DOCKER_HOST', options['env'])
        script = options['input_bytes'].decode()
        for required in ('REPEATABLE READ READ ONLY', 'COMMIT;', '\\gexec', 'FROM ONLY public.%I', 'ARRAY[%s]::text[]'):
            self.assertIn(required, script)
        self.assertFalse(any('SELECT' in arg for arg in argv))

    def test_row_order_duplicates_null_and_length_encoding(self):
        original = self.db.capture_tables().public()
        self.fake.rows.reverse()
        self.assertEqual(self.db.capture_tables().public(), original)
        self.fake.rows.append(['row', 'fixture', ['alpha']])
        self.assertNotEqual(self.db.capture_tables().public(), original)
        self.fake.rows = [['row', 'fixture', ['']]]
        empty = self.db.capture_tables().public()
        self.fake.rows = [['row', 'fixture', [None]]]
        self.assertNotEqual(self.db.capture_tables().public(), empty)
        self.fake.rows = [['row', 'fixture', ['a\nb\t\u0085\u2028雪']]]
        self.assertTrue(self.db.capture_tables().public()['complete'])

    def test_empty_tables_and_distinct_identifier_encoding(self):
        self.fake.inventory.append(['empty', 'r', []])
        got = self.db.capture_tables()
        self.assertEqual(len(got.identities()), 2)
        self.assertNotEqual(pg.identity('a.b'), pg.identity('b'))
        self.fake.rows.append(['row', 'empty', []])
        self.assertNotEqual(self.db.capture_tables().public(), got.public())

    def test_forward_original_columns_and_d0_all_columns(self):
        baseline = self.db.capture_tables()
        self.fake.inventory[0][2].append(['new', 'pg_catalog', 'text', -1, 2])
        for row in self.fake.rows:
            row[2].append(None)
        projected = self.db.capture_tables(projection=baseline)
        self.assertEqual(projected.public(), baseline.public())
        d0 = self.db.capture_tables()
        self.assertNotEqual(d0.public(), baseline.public())
        self.fake.rows[0][2][1] = 'changed new value'
        self.assertEqual(self.db.capture_tables(projection=baseline).public(), baseline.public())
        self.assertNotEqual(self.db.capture_tables().public(), d0.public())
        with self.assertRaisesRegex(proof.EvidenceError, 'pg-projection-invalid'):
            self.db.capture_tables(projection=projected)

    def test_added_tables_visible_and_original_column_drift_refuses(self):
        baseline = self.db.capture_tables()
        self.fake.inventory.append(['new_table', 'r', []])
        self.assertEqual(len(self.db.capture_tables(projection=baseline).identities()), 2)
        for index, value in ((0, 'renamed'), (2, 'int8'), (3, 3)):
            self.fake.inventory[0][2][0][index] = value
            with self.subTest(index=index), self.assertRaisesRegex(proof.EvidenceError, 'pg-projection-changed'):
                self.db.capture_tables(projection=baseline)
            self.fake.inventory[0][2][0] = copy.deepcopy(COLUMN)
        self.fake.inventory = self.fake.inventory[1:]
        self.fake.rows = []
        with self.assertRaisesRegex(proof.EvidenceError, 'pg-projection-missing'):
            self.db.capture_tables(projection=baseline)

    def test_partitions_no_double_count_and_foreign_refusal(self):
        self.fake.inventory.append(['parent', 'p', [copy.deepcopy(COLUMN)]])
        self.assertEqual(len(self.db.capture_tables().identities()), 2)
        self.fake.rows.append(['row', 'parent', ['duplicate']])
        with self.assertRaises(proof.EvidenceError):
            self.db.capture_tables()
        self.fake.rows.pop()
        self.fake.inventory[-1][1] = 'f'
        with self.assertRaisesRegex(proof.EvidenceError, 'pg-relation-unsupported'):
            self.db.capture_tables()

    def test_role_rls_visibility_and_log_settings_are_measured(self):
        for index, value in ((1, 14), (2, 'off'), (5, False), (6, False), (7, False), (8, False),
                             (9, 1), (10, 'on'), (11, 'none'), (13, 'default'), (5, 1)):
            self.fake.state[index] = value
            with self.subTest(index=index, value=value), self.assertRaisesRegex(proof.EvidenceError, 'pg-state-invalid'):
                self.db.capture_tables()
            self.fake.state = copy.deepcopy(STATE)

    def test_partial_query_diagnostics_and_container_restart_refuse(self):
        self.fake.bad_complete = True
        with self.assertRaises(proof.EvidenceError):
            self.db.capture_tables()
        self.fake.bad_complete = False
        self.fake.fail_sql = True
        with self.assertRaisesRegex(proof.InfrastructureFailure, '^capture$'):
            self.db.capture_tables()
        self.fake.fail_sql = False
        self.fake.mutate_after_data = True
        with self.assertRaisesRegex(proof.EvidenceError, 'pg-container-changed'):
            self.db.capture_tables()

    def test_malformed_or_duplicate_inventory_cannot_publish_partial_hashes(self):
        for inventory in ([], self.fake.inventory * 2, [['x', 'r', [COLUMN, COLUMN]]], [['x', 'r', [['x']]]]):
            self.fake.inventory = copy.deepcopy(inventory)
            with self.subTest(inventory=inventory), self.assertRaises(proof.EvidenceError):
                self.db.capture_tables()

    def test_unowned_external_mount_port_or_noninternal_container_refused(self):
        original = copy.deepcopy(self.fake.info)
        for change in ('label', 'mount', 'port', 'image', 'network'):
            self.fake.info = copy.deepcopy(original)
            if change == 'label': self.fake.info['Config']['Labels'] = {}
            if change == 'mount': self.fake.info['Mounts'] = [{'Type': 'bind'}]
            if change == 'port': self.fake.info['HostConfig']['PortBindings'] = {'5432/tcp': [{}]}
            if change == 'image': self.fake.info['Config']['Image'] = 'postgres:17'
            if change == 'network':
                self.fake.info['HostConfig']['NetworkMode'] = 'x'
                self.fake.info['NetworkSettings'] = {'Networks': {'x': {'NetworkID': 'd'*64}}}
                self.fake.network['Internal'] = False
            with self.subTest(change=change), self.assertRaises(proof.EvidenceError):
                pg.SyntheticPostgres('docker', self.config, ID, TOKEN, 'compat_runtime', capture=self.fake)

    def test_fixed_probe_only_nonpublic_and_one_use_install(self):
        self.db.install_probe()
        install = next(o['input_bytes'].decode() for a,o in self.fake.calls if o.get('input_bytes') and b'CREATE SCHEMA' in o['input_bytes'])
        self.assertIn('CREATE SCHEMA compat_probe AUTHORIZATION postgres', install)
        self.assertIn('SECURITY DEFINER', install)
        self.assertIn('SET search_path=pg_catalog', install)
        self.assertNotIn('ALTER ROLE', install)
        self.assertNotIn('NO FORCE', install)
        self.assertNotIn('public.', install)
        with self.assertRaisesRegex(proof.EvidenceError, 'pg-probe-already-installed'):
            self.db.install_probe()

    def test_complete_phase_log_and_marker_are_actual_collections(self):
        self.db.install_probe()
        phase = self.db.begin_phase('compat-n1-rw')
        self.fake.stderr += line('compat-n1-rw', 'LOG', 'statement: CREATE TABLE public.synthetic (v text)')
        self.fake.events = [[1, 'ddl_command_start', 'CREATE TABLE', 'compat-n1-rw', 'CREATE TABLE public.synthetic (v text)']]
        result = self.db.finish_phase(phase)
        self.assertEqual(len(result['markers']['hashes']), 1)
        self.assertNotIn('synthetic', repr(result['markers']))
        parsed = proof.parse_pg_log(**result['log'], application='compat-n1-rw')
        self.assertEqual(len(parsed.statement_hashes), 1)
        with self.assertRaises(proof.EvidenceError):
            self.db.finish_phase(phase)

    def test_zero_markers_never_erases_rolled_back_mod_log(self):
        self.db.install_probe()
        phase = self.db.begin_phase('compat-n1-rw')
        self.fake.stderr += line('compat-n1-rw', 'LOG', 'statement: UPDATE public.synthetic SET v=1')
        result = self.db.finish_phase(phase)
        self.assertEqual(result['markers']['hashes'], [])
        self.assertEqual(len(proof.parse_pg_log(**result['log'], application='compat-n1-rw').statement_hashes), 1)

    def test_probe_drift_foreign_events_and_active_sessions_refuse(self):
        for cause in ('probe', 'event', 'sessions'):
            self.fake = FakeDocker()
            self.db = pg.SyntheticPostgres('docker', self.config, ID, TOKEN, 'compat_runtime', capture=self.fake)
            self.db.install_probe(); phase = self.db.begin_phase('compat-n1-rw')
            if cause == 'probe': self.fake.function += ' changed'
            if cause == 'event': self.fake.events = [[1,'ddl_command_start','ALTER TABLE','foreign','private SQL']]
            if cause == 'sessions': self.fake.state[9] = 1
            with self.subTest(cause=cause), self.assertRaises(proof.EvidenceError):
                self.db.finish_phase(phase)

    def test_missing_end_is_infrastructure_not_complete(self):
        self.db.install_probe(); phase = self.db.begin_phase('compat-n1-rw')
        self.fake.omit_end = True
        with self.assertRaisesRegex(proof.InfrastructureFailure, '^capture$'):
            self.db.finish_phase(phase, wait_seconds=0.01)


class FrameTests(unittest.TestCase):
    def test_empty_complete_and_multiline_windows(self):
        for raw in (b'', line('compat-fwd','LOG','statement: UPDATE public.x\n\tSET v=1')):
            whole = barrier(TOKEN,'begin') + raw + barrier(TOKEN,'end')
            self.assertEqual(pg.frame_window(whole,TOKEN,'compat-fwd'),raw)

    def test_delayed_complete_end_is_not_premature_success(self):
        start, end = barrier(TOKEN,'begin'), barrier(TOKEN,'end')
        for fragment in (b'', end[:7], end.splitlines(keepends=True)[0]):
            self.assertIsNone(pg.frame_window(start+fragment,TOKEN,'compat-fwd'))

    def test_duplicate_reordered_foreign_or_unattributed_records_refuse(self):
        begin,end=barrier(TOKEN,'begin'),barrier(TOKEN,'end')
        for whole in (end+begin, begin+end+end, begin+barrier('b'*32,'end'),
                      begin+line('foreign','LOG','statement: UPDATE x SET y=1')+end,
                      begin+b'unattributed\n'+end, begin+end+b'late record\n', b'leading\n'+begin+end):
            with self.subTest(whole=whole[:30]), self.assertRaises(proof.EvidenceError):
                pg.frame_window(whole,TOKEN,'compat-fwd')

    def test_barrier_context_cannot_hide_an_arbitrary_statement(self):
        whole = barrier(TOKEN,'begin')+barrier(TOKEN,'end')
        whole = whole.replace(b'inline_code_block line 1 at RAISE', b'private unexpected SQL', 1)
        with self.assertRaises(proof.EvidenceError):
            pg.frame_window(whole,TOKEN,'compat-fwd')


if __name__ == '__main__':
    unittest.main(verbosity=2)
