"""Synthetic-only PostgreSQL observations; no experiment lifecycle or verdict.

There is no DSN input. SQL runs via psql on a fixed Unix socket *inside* an
explicitly bound task-owned PG17 container. No password is transported; this
requires the synthetic cluster's local trust rule, while host SCRAM remains
unchanged. The synthetic cluster administrator observes
all rows without changing owner/runtime privileges or FORCE RLS. Raw row/SQL
bytes and object names are private working data, never a public artifact.
Catalog hashing remains the existing Go catalog command's responsibility;
this module neither reimplements its normalization nor claims PR4 overlay
support. The caller still owns bootstrap, source/image binding and cleanup.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import hashlib
import json
from pathlib import Path
import re
import secrets
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof
import runtime

MAX_ROWS = 1_000_000
MAX_COLUMNS = 1600
SOCKET_DIRECTORY = "/var/run/postgresql"
LABEL = "codex.compat"
_HEX = re.compile(r"[0-9a-f]{64}\Z")
_NAME = re.compile(r"[a-z][a-z0-9_]{0,62}\Z")


def need(value, code="pg-capture-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def encoded(value) -> bytes:
    return json.dumps(value, ensure_ascii=True, separators=(",", ":"), allow_nan=False).encode("ascii")


def sha(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def identity(name: str) -> str:
    return sha(encoded(["public", name]))


def _json_lines(raw: bytes):
    need(type(raw) is bytes and len(raw) <= runtime.MAX_STREAM and raw.endswith(b"\n"))
    def pairs(items):
        out = {}
        for key, value in items:
            need(key not in out)
            out[key] = value
        return out
    try:
        return [json.loads(line, object_pairs_hook=pairs,
                           parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
                for line in raw.decode("utf-8", "strict").split('\n')[:-1]]
    except (UnicodeError, ValueError, RecursionError):
        raise proof.EvidenceError("pg-capture-invalid") from None


@dataclass(frozen=True)
class TableCapture:
    """Measured rows are discarded; private column contracts support projection."""
    _seal: object = field(repr=False)
    _inventory: dict = field(repr=False)
    _digests: dict = field(repr=False)
    _all_columns: bool = field(repr=False)

    def public(self):
        return {"complete": True, "digests": dict(self._digests)}

    def identities(self):
        return sorted(self._digests)


@dataclass(frozen=True)
class Phase:
    _seal: object = field(repr=False)
    application: str
    _token: str = field(repr=False)
    _before_out: bytes = field(repr=False)
    _before_err: bytes = field(repr=False)
    _marker_id: int = field(repr=False)
    _probe_signature: str = field(repr=False)


# Names and values are returned only inside the owned synthetic process pipe.
# r includes inheritance children; ONLY avoids counting child rows twice. A p
# relation has no own storage. Materialized views contain data and are included.
# Foreign tables are inventoried but never read; parsing refuses their presence.
_INVENTORY = """SELECT COALESCE(json_agg(json_build_array(c.relname,c.relkind,
 (SELECT COALESCE(json_agg(json_build_array(a.attname,tn.nspname,t.typname,a.atttypmod,a.attnum)
  ORDER BY a.attnum),'[]'::json) FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
  WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped)) ORDER BY c.relname COLLATE "C"),'[]'::json)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p','m','f')"""

_READ_HEADER = """BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path=pg_catalog;
SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD';
SET LOCAL intervalstyle='iso_8601'; SET LOCAL extra_float_digits=3;
SET LOCAL bytea_output='hex'; SET LOCAL standard_conforming_strings=on;
SET LOCAL statement_timeout='60000';
SET LOCAL lock_timeout='2000';
"""

_DATA_QUERIES = """SELECT pg_catalog.format(
 'SELECT json_build_array(''row'',%L,to_json(ARRAY[%s]::text[])) FROM ONLY public.%I', c.relname,
 COALESCE((SELECT string_agg(pg_catalog.format('CASE WHEN %1$I IS NULL THEN NULL ELSE %1$I::text END',a.attname),',' ORDER BY a.attnum)
 FROM pg_catalog.pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped),''), c.relname)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','m') ORDER BY c.relname COLLATE "C"
\\gexec
"""

_PROBE_INSTALL = """BEGIN;
CREATE SCHEMA compat_probe AUTHORIZATION postgres;
REVOKE ALL ON SCHEMA compat_probe FROM PUBLIC;
CREATE TABLE compat_probe.events (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 event text NOT NULL, tag text NOT NULL, application text NOT NULL, statement text NOT NULL);
CREATE FUNCTION compat_probe.mark() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path=pg_catalog AS $probe$ BEGIN
 INSERT INTO compat_probe.events(event,tag,application,statement)
 VALUES(TG_EVENT,TG_TAG,current_setting('application_name'),current_query()); END $probe$;
REVOKE ALL ON FUNCTION compat_probe.mark() FROM PUBLIC;
CREATE EVENT TRIGGER compat_probe_start ON ddl_command_start EXECUTE FUNCTION compat_probe.mark();
CREATE EVENT TRIGGER compat_probe_drop ON sql_drop EXECUTE FUNCTION compat_probe.mark();
COMMIT;
SELECT json_build_array('installed');
"""

_PROBE_STATE = """SELECT json_build_array('probe',
 (SELECT pg_catalog.pg_get_functiondef(p.oid) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='compat_probe' AND p.proname='mark' AND p.pronargs=0 AND p.proowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='postgres')),
 (SELECT COALESCE(json_agg(json_build_array(e.evtname,e.evtevent,e.evtenabled,n.nspname,p.proname,p.pronargs,p.prosecdef) ORDER BY e.evtname),'[]'::json)
 FROM pg_catalog.pg_event_trigger e JOIN pg_catalog.pg_proc p ON p.oid=e.evtfoid
 JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 WHERE e.evtname IN ('compat_probe_start','compat_probe_drop')
 AND e.evtowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='postgres')),
 (SELECT COALESCE(max(id),0) FROM compat_probe.events));
"""


class SyntheticPostgres:
    """Fixed SQL and bounded logs for a caller-owned disposable PG17 container.

    Constructor inspects ownership before SQL. No arbitrary host/DSN, command,
    user or database passthrough. The fixed database is compat; the caller has
    already bootstrapped it with a NOSUPER owner and separate NOBYPASS runtime.
    The observer uses postgres only inside this isolated synthetic container.
    """
    def __init__(self, docker_binary: str, config_dir: Path, container: str, token: str,
                 runtime_role: str, *, base_env=None, capture=runtime.bounded_command):
        need(type(container) is str and _HEX.fullmatch(container) is not None)
        need(type(token) is str and re.fullmatch(r"[0-9a-f]{32}", token) is not None)
        need(type(runtime_role) is str and _NAME.fullmatch(runtime_role) is not None and runtime_role != "postgres")
        self.binary = runtime._binary(docker_binary)
        self.config = runtime._empty_directory(config_dir)
        self.env = runtime.common.scrubbed_env(base={} if base_env is None else base_env)
        self.container, self.token, self.runtime_role = container, token, runtime_role
        self.capture = capture
        self._seal = object()
        self._phase = None
        self._control_attempted = False
        self._probe_signature = None
        self._binding = self._inspect()

    def _call(self, args, *, input_bytes=None, limit=runtime.MAX_STREAM):
        return self.capture([self.binary, "--config", str(self.config), "--context", "default", *args],
                            env=dict(self.env), timeout=90, stdout_limit=limit,
                            stderr_limit=runtime.MAX_STREAM, input_bytes=input_bytes)

    def _inspect(self):
        result = self._call(["inspect", self.container], limit=runtime.MIB)
        need(result.returncode == 0, "pg-container-unavailable")
        try:
            values = runtime.common.loads_strict(result.stdout)
            need(type(values) is list and len(values) == 1)
            v = values[0]
            need(v['Id'] == self.container and v['Config']['Image'] == runtime.POSTGRES)
            need(v['Config']['Labels'].get(LABEL) == self.token and v['State']['Running'] is True)
            need(not v['HostConfig'].get('Binds') and not v['HostConfig'].get('PortBindings'))
            need(all(m.get('Type') == 'tmpfs' for m in v.get('Mounts', [])))
            mode = v['HostConfig']['NetworkMode']
            if mode != 'none':
                networks = v['NetworkSettings']['Networks']
                need(type(networks) is dict and len(networks) == 1)
                network_id = next(iter(networks.values()))['NetworkID']
                need(type(network_id) is str and _HEX.fullmatch(network_id) is not None)
                network = self._call(['network', 'inspect', network_id], limit=runtime.MIB)
                need(network.returncode == 0)
                n = runtime.common.loads_strict(network.stdout)
                need(type(n) is list and len(n) == 1 and n[0]['Id'] == network_id)
                need(n[0]['Internal'] is True and n[0]['Labels'].get(LABEL) == self.token)
            else:
                network_id = 'none'
            need(type(v['State']['StartedAt']) is str and v['State']['StartedAt'])
            return sha(encoded([v['Id'], v['Image'], v['State']['StartedAt'], network_id]))
        except (KeyError, TypeError, runtime.common.ReleaseError):
            raise proof.EvidenceError('pg-container-invalid') from None

    def _check(self):
        need(self._inspect() == self._binding, 'pg-container-changed')

    def _state_sql(self):
        # Role identifier was restricted before construction; no raw caller SQL.
        return """SELECT json_build_array('state', current_setting('server_version_num')::int/10000,
 current_setting('transaction_read_only'),current_setting('transaction_isolation'),
 current_setting('server_encoding'),current_user='postgres',
 (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user),
 (SELECT NOT r.rolsuper AND NOT r.rolbypassrls FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles r ON r.oid=d.datdba WHERE d.datname=current_database()),
 (SELECT NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolreplication
  AND r.oid<>d.datdba AND NOT pg_catalog.pg_has_role(r.oid,d.datdba,'MEMBER')
  FROM pg_catalog.pg_roles r CROSS JOIN pg_catalog.pg_database d WHERE r.rolname='""" + self.runtime_role + """' AND d.datname=current_database()),
 (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND pid<>pg_backend_pid()),
 current_setting('autovacuum'),current_setting('log_statement'),current_setting('log_min_error_statement'),
 current_setting('log_error_verbosity'),current_setting('log_line_prefix'),current_setting('log_timezone'),current_setting('lc_messages'));
"""

    def _sql(self, script: str, *, readonly=True):
        raw = script.encode('utf-8')
        need(len(raw) <= runtime.MIB)
        result = self._call(['exec', '-i', '--env', 'PGAPPNAME=compat-observer', '--env', 'PGCLIENTENCODING=UTF8',
                             '--env', 'PGOPTIONS=-c default_transaction_read_only=' + ('on' if readonly else 'off'),
                             self.container, 'psql', '-X', '-w', '-qAt', '-h', SOCKET_DIRECTORY,
                             '-p', '5432', '-U', 'postgres', '-d', 'compat',
                             '-v', 'ON_ERROR_STOP=1'], input_bytes=raw)
        if result.returncode != 0:
            raise proof.InfrastructureFailure('capture')
        return result

    @staticmethod
    def _state(value):
        need(encoded(value) == encoded(['state', 17, 'on', 'repeatable read', 'UTF8', True, True, True, True, 0,
                       'off', 'mod', 'error', 'verbose', '%m [%p] %a %e ', 'UTC', 'C']), 'pg-state-invalid')

    def capture_tables(self, *, projection: TableCapture | None = None) -> TableCapture:
        """All current tables; existing columns only when given forward baseline.

        No projection means all current columns, suitable for D0/N-1. A baseline
        is tied to this exact collector/container. New tables are still measured
        completely; proof policy decides whether their presence is allowed.
        """
        if projection is not None:
            need(type(projection) is TableCapture and projection._seal is self._seal and projection._all_columns,
                 'pg-projection-invalid')
        self._check()
        token = secrets.token_hex(16)
        script = (_READ_HEADER + self._state_sql()
                  + "SELECT json_build_array('inventory',(" + _INVENTORY + "));\n"
                  + _DATA_QUERIES + "COMMIT;\nSELECT json_build_array('complete','" + token + "');\n")
        result = self._sql(script)
        need(result.stderr == b'', 'pg-capture-diagnostics')
        rows = _json_lines(result.stdout)
        need(len(rows) >= 3 and rows[-1] == ['complete', token])
        self._state(rows[0])
        need(type(rows[1]) is list and len(rows[1]) == 2 and rows[1][0] == 'inventory')
        inventory = self._inventory(rows[1][1])
        if projection is not None:
            need(set(projection._inventory) <= set(inventory), 'pg-projection-missing')
        encoded_rows = {name: [] for name in inventory}
        chosen = {}
        positions = {}
        for name, (kind, columns) in inventory.items():
            selected = columns
            if projection is not None and name in projection._inventory:
                old_kind, selected = projection._inventory[name]
                need(old_kind == kind and all(c in columns for c in selected), 'pg-projection-changed')
            chosen[name] = [kind, selected]
            positions[name] = [columns.index(c) for c in selected]
        need(len(rows) - 3 <= MAX_ROWS, 'pg-row-limit')
        for row in rows[2:-1]:
            need(type(row) is list and len(row) == 3 and row[0] == 'row' and type(row[1]) is str and row[1] in inventory)
            name, values = row[1:]
            need(inventory[name][0] != 'p' and type(values) is list and len(values) == len(inventory[name][1]))
            need(all(x is None or type(x) is str for x in values))
            encoded_rows[name].append(encoded([values[i] for i in positions[name]]))
        digests = {}
        for name in sorted(inventory):
            values = sorted(encoded_rows[name])
            h = hashlib.sha256(b'compat-table-v1\0')
            for part in (encoded(chosen[name]), encoded(len(values)), *values):
                h.update(len(part).to_bytes(8, 'big')); h.update(part)
            digests[identity(name)] = h.hexdigest()
        self._check()
        return TableCapture(self._seal, inventory, digests, projection is None)

    @staticmethod
    def _inventory(values):
        need(type(values) is list and 0 < len(values) <= proof.MAX_TABLES, 'pg-inventory-invalid')
        result = {}
        for value in values:
            need(type(value) is list and len(value) == 3)
            name, kind, columns = value
            need(type(name) is str and name and '\0' not in name and name not in result)
            need(kind in ('r', 'p', 'm'), 'pg-relation-unsupported')
            need(type(columns) is list and len(columns) <= MAX_COLUMNS)
            seen, last = set(), 0
            for c in columns:
                need(type(c) is list and len(c) == 5)
                need(all(type(x) is str and x and '\0' not in x for x in c[:3]))
                need(type(c[3]) is int and type(c[4]) is int and last < c[4] <= MAX_COLUMNS and c[0] not in seen)
                seen.add(c[0]); last = c[4]
            result[name] = [kind, columns]
        return result

    def install_probe(self):
        """One fixed synthetic-admin install; an existing schema must refuse."""
        need(self._probe_signature is None and self._phase is None, 'pg-probe-already-installed')
        self._check()
        state = self._sql(_READ_HEADER + self._state_sql() + 'COMMIT;\n')
        self._state(_json_lines(state.stdout)[0])
        result = self._sql(_PROBE_INSTALL, readonly=False)
        need(_json_lines(result.stdout) == [['installed']])
        self._probe_signature, _ = self._probe_state()
        self._check()

    def read_only_control(self):
        """One deliberately failing fixed CREATE, never a privilege grant.

        Separate statements make the server's failed-statement diagnostic bind
        exactly READ_ONLY_CONTROL_SQL (without a batch prefix or semicolon).
        Private client stderr is not relied on for the SQLSTATE proof.
        """
        need(not self._control_attempted and self._phase is None and self._probe_signature is not None,
             "pg-control-already-attempted")
        self._control_attempted = True
        phase = self.begin_phase("compat-control")
        args = ['exec', '-i', '--env', 'PGAPPNAME=compat-control', '--env', 'PGCLIENTENCODING=UTF8',
                '--env', 'PGOPTIONS=-c default_transaction_read_only=on', self.container,
                'psql', '-X', '-w', '-qAt', '-h', SOCKET_DIRECTORY, '-p', '5432',
                '-U', 'compat_owner', '-d', 'compat', '-v', 'ON_ERROR_STOP=1']
        # A separate read validates the startup option under the exact owner.
        state = self._call(args, input_bytes=b"SELECT json_build_array(current_user,current_setting('transaction_read_only'))")
        need(type(state.returncode) is int and state.returncode == 0 and not state.stderr
             and _json_lines(state.stdout) == [['compat_owner', 'on']], 'pg-control-session-invalid')
        self._check()
        result = self._call(args, input_bytes=proof.READ_ONLY_CONTROL_SQL.encode())
        need(type(result.returncode) is int and 1 <= result.returncode <= 255 and not result.stdout,
             'pg-control-did-not-fail')
        captured = self.finish_phase(phase)
        log = proof.parse_pg_log(captured['log']['raw'], application='compat-control', complete=True)
        expected = sha(proof.READ_ONLY_CONTROL_SQL.encode())
        need(log.error_states == ('25006',) and log.failed_statements == (('25006', expected),)
             and log.statement_hashes in ((), (expected,)) and not captured['markers']['hashes'],
             'pg-control-unproven')
        return {'execution': {'status': 'completed', 'exit_code': result.returncode, 'failure': None},
                'sqlstate': '25006', 'log': captured['log']}

    def _probe_state(self):
        rows = _json_lines(self._sql(_READ_HEADER + _PROBE_STATE + 'COMMIT;\n').stdout)
        need(len(rows) == 1 and type(rows[0]) is list and len(rows[0]) == 4)
        label, function, triggers, cursor = rows[0]
        need(label == 'probe' and type(function) is str and function)
        need(triggers == [['compat_probe_drop', 'sql_drop', 'O', 'compat_probe', 'mark', 0, True],
                          ['compat_probe_start', 'ddl_command_start', 'O', 'compat_probe', 'mark', 0, True]])
        need(type(cursor) is int and 0 <= cursor < 2**63)
        return sha(encoded([function, triggers])), cursor

    def _logs(self):
        result = self._call(['logs', self.container])
        need(result.returncode == 0, 'pg-log-capture-failed')
        return result.stdout, result.stderr

    def _barrier(self, token, boundary):
        # The fixed observer-only record is stripped by the framing parser, not
        # the proof parser. No measured client gets to emit its own boundaries.
        message = 'compat-boundary-' + token + '-' + boundary
        self._sql("DO $$ BEGIN RAISE LOG '" + message + "'; END $$;\n")

    def begin_phase(self, application: str) -> Phase:
        need(application in proof.APPLICATIONS and self._phase is None and self._probe_signature is not None)
        self._check()
        rows = _json_lines(self._sql(_READ_HEADER + self._state_sql() + 'COMMIT;\n').stdout)
        need(len(rows) == 1); self._state(rows[0])
        signature, cursor = self._probe_state()
        need(signature == self._probe_signature, 'pg-probe-changed')
        before_out, before_err = self._logs()
        need(not before_err or before_err.endswith(b'\n'), 'pg-log-truncated')
        token = secrets.token_hex(16)
        self._barrier(token, 'begin')
        phase = Phase(self._seal, application, token, before_out, before_err, cursor, signature)
        self._phase = phase
        return phase

    def finish_phase(self, phase: Phase, *, wait_seconds=3):
        """Call only after the phase process terminates; measure quiescence/end.

        No execution success is synthesized. Returned raw complete window and
        marker hashes can accompany the separately captured process result.
        A failed finish consumes this phase; callers cannot silently retry it.
        """
        need(type(phase) is Phase and phase._seal is self._seal and self._phase is phase)
        need(type(wait_seconds) in (float, int) and 0 < wait_seconds <= 10)
        self._phase = None
        self._check()
        rows = _json_lines(self._sql(_READ_HEADER + self._state_sql() + 'COMMIT;\n').stdout)
        need(len(rows) == 1); self._state(rows[0])
        signature, cursor = self._probe_state()
        need(signature == phase._probe_signature and cursor >= phase._marker_id, 'pg-probe-changed')
        marker_sql = ("SELECT json_build_array(id,event,tag,application,statement) FROM compat_probe.events WHERE id>"
                      + str(phase._marker_id) + " AND id<=" + str(cursor) + ' ORDER BY id;\n')
        marker_rows = _json_lines(self._sql(_READ_HEADER + marker_sql
                                          + "SELECT json_build_array('complete');\nCOMMIT;\n").stdout)
        need(marker_rows[-1:] == [['complete']] and len(marker_rows) - 1 <= proof.MAX_EVENTS)
        hashes, last = [], phase._marker_id
        for row in marker_rows[:-1]:
            need(type(row) is list and len(row) == 5 and type(row[0]) is int and last < row[0] <= cursor)
            need(row[1] in ('ddl_command_start', 'sql_drop') and row[3] == phase.application)
            need(all(type(x) is str for x in row[1:]))
            last = row[0]; hashes.append(sha(encoded(row[1:])))
        need(last == cursor, 'pg-marker-incomplete')
        self._barrier(phase._token, 'end')
        deadline = time.monotonic() + wait_seconds
        while True:
            out, err = self._logs()
            need(out == phase._before_out and err.startswith(phase._before_err), 'pg-log-changed')
            window = frame_window(err[len(phase._before_err):], phase._token, phase.application)
            if window is not None:
                break
            if time.monotonic() >= deadline:
                raise proof.InfrastructureFailure('capture')
            time.sleep(0.05)
        self._check()
        return {'log': {'raw': window, 'complete': True},
                'markers': {'complete': True, 'application': phase.application, 'hashes': hashes}}


def frame_window(raw: bytes, token: str, application: str):
    """Return a validated window or None while the exact END is incomplete.

    Only fixed observer RAISE LOG records (including their exact diagnostic
    forms) are removed. Every enclosed byte goes to the existing proof parser;
    foreign or unattributed records are never silently filtered.
    """
    need(type(raw) is bytes and len(raw) <= proof.MAX_LOG_BYTES, 'pg-log-bounds')
    need(re.fullmatch(r'[0-9a-f]{32}', token) is not None and application in proof.APPLICATIONS)
    if not raw.endswith(b'\n'):
        return None
    try:
        text = raw.decode('utf-8', 'strict')
    except UnicodeError:
        raise proof.EvidenceError('pg-log-encoding') from None
    text = text.replace('\r\n', '\n')
    lines = text.splitlines(keepends=True)
    boundaries = []
    i = 0
    while i < len(lines):
        match = proof._PREFIX.fullmatch(lines[i].removesuffix('\n'))
        if match and match['app'] == 'compat-observer':
            need(match['state'] == '00000' and match['level'] == 'LOG')
            need(match['body'].startswith('00000: '))
            label = match['body'][7:]
            need(label in ('compat-boundary-' + token + '-begin', 'compat-boundary-' + token + '-end'))
            if i + 3 >= len(lines):
                return None
            context, location, statement = [proof._PREFIX.fullmatch(line.removesuffix('\n')) for line in lines[i+1:i+4]]
            need(context is not None and location is not None and statement is not None)
            need(all(m['app'] == 'compat-observer' and m['pid'] == match['pid'] and m['state'] == '00000' for m in (context, location, statement)))
            need(context['level'] == 'CONTEXT' and context['body'] == 'PL/pgSQL function inline_code_block line 1 at RAISE')
            need(location['level'] == 'LOCATION' and re.fullmatch(r'exec_stmt_raise, pl_exec.c:[1-9][0-9]*', location['body']) is not None)
            need(statement['level'] == 'STATEMENT' and statement['body'] == "DO $$ BEGIN RAISE LOG '" + label + "'; END $$;")
            boundaries.append((label.rsplit('-', 1)[1], i, i+4))
            i += 4
        else:
            i += 1
    if len(boundaries) < 2:
        need(not boundaries or boundaries[0][0] == 'begin')
        return None
    need(len(boundaries) == 2 and boundaries[0][0] == 'begin' and boundaries[1][0] == 'end')
    need(boundaries[0][1] == 0 and boundaries[1][2] == len(lines), 'pg-log-outside-boundary')
    window = ''.join(lines[boundaries[0][2]:boundaries[1][1]]).encode('utf-8')
    proof.parse_pg_log(window, application=application, complete=True)
    return window
