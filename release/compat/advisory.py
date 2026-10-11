"""Owned synthetic swallowed-UPDATE collection, not a compatibility runner.

The fixed psql actor is deliberately faulty. It is not the signed previous
binary. This collector accepts no SQL, DSN, observations or image overrides.
The caller owns verified image admission, fixture provisioning, the overall
deadline and exact cleanup. Returned private observations are not an advisory
publication: cleanup and source/provenance sealing remain harness obligations.
No Docker, SQL or file operation executes at import.
"""
from __future__ import annotations

import copy
from dataclasses import dataclass, field
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import catalog

postgres, processes = catalog.postgres, catalog.processes
proof, runtime = catalog.proof, catalog.runtime

_UPDATE = "UPDATE public.permissions SET description=description WHERE resource='users' AND action='read' RETURNING 1;"
# PostgreSQL 17's \quit takes no argument; an extra 0 produces a stderr warning.
# With ON_ERROR_STOP off, its ordinary successful exit preserves this faulty actor.
_ACTOR = "\\set ON_ERROR_STOP off\n" + _UPDATE + "\n\\echo COMPAT_ADVISORY_ACTOR_FINISHED\n\\quit\n"
_UPDATE_SHA = "abc599a7aeb21a7a7cbdfdca05775a94dc3e31f94ba73d4a619d87dfb493a69a"
_ACTOR_SHA = "5aa17473d9b1b64ab50b64ea6b1e4c0c8d95e98e844f3dbb3a773b37d6757b3e"
_SESSION = """SELECT json_build_array('advisory-session',current_user,current_database(),
 current_setting('transaction_read_only'),
 (SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=current_user),
 (SELECT datdba=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user)
  FROM pg_catalog.pg_database WHERE datname=current_database()),
 (SELECT count(*) FROM public.permissions WHERE resource='users' AND action='read'));
"""


def need(ok, code="advisory-invalid"):
    if not ok:
        raise proof.EvidenceError(code)


@dataclass(frozen=True, repr=False)
class AdvisoryObservation:
    """Private captures plus bounded provenance, without cleanup/overall proof."""
    _baseline: dict = field(repr=False)
    _observations: dict = field(repr=False)
    _provenance: dict = field(repr=False)

    def provenance(self):
        return copy.deepcopy(self._provenance)


class OwnedSwallowedUpdate:
    def __init__(self, observer: catalog.OwnedCatalog, inputs: processes.ConfigInputs):
        need(type(observer) is catalog.OwnedCatalog and type(inputs) is processes.ConfigInputs)
        need(type(observer.manager) is processes.OwnedProcesses
             and type(observer.pg) is postgres.SyntheticPostgres
             and type(observer.contract) is catalog.CatalogContract)
        self.catalog, self.inputs = observer, inputs
        self.manager, self.pg, self.contract = observer.manager, observer.pg, observer.contract
        self.owner = self.manager.owner
        self._token, self._container = self.owner.token, self.pg.container
        self._owner_source, self._pg_binding = self.owner.source_sha, self.pg._binding
        self._pg_seal, self._contract_seal = self.pg._seal, self.contract._seal
        self._bindings = dict(self.manager.bindings)
        self._contract = copy.deepcopy(self.contract.public())
        self._capture = self.owner.capture
        self._attempted = False
        need(self.pg.capture is self._capture, "advisory-transport-mismatch")
        need(postgres.sha(_ACTOR.encode()) == _ACTOR_SHA and postgres.sha(_UPDATE.encode()) == _UPDATE_SHA,
             "advisory-sql-changed")

    def _identity(self):
        need(self.catalog.manager is self.manager and self.catalog.pg is self.pg
             and self.catalog.contract is self.contract and self.manager.owner is self.owner,
             "advisory-owner-changed")
        need(self.owner.token == self._token and self.pg.token == self._token
             and self.pg.container == self._container == self.owner._ids.get("pg")
             and self.owner.source_sha == self._owner_source and self.pg._binding == self._pg_binding
             and self.pg._seal is self._pg_seal and self.pg.runtime_role == "compat_runtime",
             "advisory-pg-changed")
        need(self.owner.capture is self._capture and self.pg.capture is self._capture,
             "advisory-transport-changed")
        need(set(self.manager.bindings) == set(self._bindings)
             and all(self.manager.bindings[k] is v for k, v in self._bindings.items())
             and self.contract._seal is self._contract_seal and self.contract.public() == self._contract,
             "advisory-source-changed")
        need(not self.manager._ids and not self.manager._protocol_failed and not self.catalog._failed,
             "advisory-phase-active")
        self.manager._assert_all()
        self.pg._check()

    def _idle(self):
        self._identity()
        self.catalog._quiescent()

    def _args(self, application, readonly):
        need(application in ("compat-n1-rw", "compat-n1-ro") and type(readonly) is bool)
        need(readonly is (application == "compat-n1-ro"))
        return ["exec", "-i", "--env", "PGAPPNAME=" + application, "--env", "PGCLIENTENCODING=UTF8",
                "--env", "PGOPTIONS=-c default_transaction_read_only=" + ("on" if readonly else "off")
                + " -c statement_timeout=60000 -c lock_timeout=2000", self._container,
                "psql", "-X", "-w", "-qAt", "-h", postgres.SOCKET_DIRECTORY, "-p", "5432",
                "-U", "compat_owner", "-d", "compat", "-v", "ON_ERROR_STOP=1"]

    def _call(self, application, readonly, script):
        self._identity()
        # Only module constants reach this private method; no caller SQL API.
        need(script in (_SESSION, _ACTOR), "advisory-sql-changed")
        result = self.pg._call(self._args(application, readonly), input_bytes=script.encode())
        need(type(result) is runtime.Capture and type(result.returncode) is int
             and type(result.stdout) is bytes and type(result.stderr) is bytes
             and len(result.stdout) <= runtime.MAX_STREAM and len(result.stderr) <= runtime.MAX_STREAM,
             "advisory-capture-invalid")
        self._identity()
        return result

    def _session(self, application, readonly):
        self._idle()
        result = self._call(application, readonly, _SESSION)
        need(result.returncode == 0 and not result.stderr, "advisory-session-failed")
        rows = postgres._json_lines(result.stdout)
        # Encoding comparison distinguishes JSON true from numeric one.
        need(postgres.encoded(rows) == postgres.encoded([["advisory-session", "compat_owner", "compat",
             "on" if readonly else "off", True, True, 1]]), "advisory-session-invalid")
        self._idle()

    def _tables(self, tables, baseline=None):
        need(type(tables) is postgres.TableCapture and tables._seal is self._pg_seal
             and tables._all_columns, "advisory-table-capture-invalid")
        self.contract.check_initial_tables(tables)
        need(postgres.identity("permissions") in tables.identities(), "advisory-target-uncaptured")
        if baseline is not None:
            need(tables.public() == baseline.public() and tables._inventory == baseline._inventory,
                 "advisory-data-changed")

    def _phase(self, mode, baseline, baseline_catalog):
        application, readonly = "compat-n1-" + mode, mode == "ro"
        self._session(application, readonly)
        phase = self.pg.begin_phase(application)
        result = self._call(application, readonly, _ACTOR)
        need(result.returncode == 0, "advisory-actor-exit")
        expected = b"COMPAT_ADVISORY_ACTOR_FINISHED\n" if readonly else b"1\nCOMPAT_ADVISORY_ACTOR_FINISHED\n"
        need(result.stdout == expected,
             "advisory-actor-ro-stdout" if readonly else "advisory-actor-rw-stdout")
        need(bool(result.stderr) if readonly else not result.stderr,
             "advisory-actor-ro-stderr" if readonly else "advisory-actor-rw-stderr")
        captured = self.pg.finish_phase(phase)
        self._idle()
        log = proof.parse_pg_log(**captured["log"], application=application)
        need(captured["markers"]["hashes"] == [], "advisory-unexpected-ddl")
        if readonly:
            need(log.error_states == ("25006",) and log.failed_statements == (("25006", _UPDATE_SHA),)
                 and (log.statement_hashes, log.sqlstates, log.records) in
                     (((), ("25006",), 1), ((_UPDATE_SHA,), ("00000", "25006"), 2)),
                 "advisory-ro-unproven")
        else:
            need(log.statement_hashes == (_UPDATE_SHA,) and not log.failed_statements
                 and not log.error_states and log.sqlstates == ("00000",) and log.records == 1,
                 "advisory-rw-unproven")
        observed, tables = self.catalog.after_phase(self.inputs, original_columns=False)
        self._tables(tables, baseline)
        need(observed.sha256 == baseline_catalog.sha256, "advisory-catalog-changed")
        self._session(application, readonly)
        raw = {"execution": {"status": "completed", "exit_code": result.returncode, "failure": None},
               **captured, "catalog_sha256": observed.sha256, "tables": tables.public()}
        provenance = {"exit_code": result.returncode, "returned_rows": 0 if readonly else 1,
            "stdout_bytes": len(result.stdout), "stdout_sha256": postgres.sha(result.stdout),
            "stderr_bytes": len(result.stderr), "stderr_sha256": postgres.sha(result.stderr),
            "log_bytes": len(captured["log"]["raw"]), "log_sha256": log.sha256,
            "mod_count": len(log.statement_hashes), "error_states": list(log.error_states),
            "marker_count": len(captured["markers"]["hashes"]), "catalog_sha256": observed.sha256,
            "tables_unchanged": True, "target_count_before_after": [1, 1]}
        return raw, provenance

    def collect(self) -> AdvisoryObservation:
        need(not self._attempted and not getattr(self.pg, "_advisory_attempted", False),
             "advisory-already-attempted")
        self._attempted = True
        self.pg._advisory_attempted = True
        try:
            self._idle()
            need(not self.catalog._initial_attempted and self.catalog._baseline is None
                 and self.pg._probe_signature is None and not self.pg._control_attempted,
                 "advisory-not-fresh")
            self.pg.install_probe()
            seen, baseline = self.catalog.initial(self.inputs)
            self._tables(baseline)
            rw, rw_provenance = self._phase("rw", baseline, seen)
            ro, ro_provenance = self._phase("ro", baseline, seen)
            self._idle()
            control = self.pg.read_only_control()
            self._idle()
            final_catalog, final_tables = self.catalog.after_phase(self.inputs, original_columns=False)
            self._tables(final_tables, baseline)
            need(final_catalog.sha256 == seen.sha256, "advisory-catalog-changed")
            self._session("compat-n1-ro", True)
            observations = {"rw": rw, "ro": ro, "negative_control": control}
            flags = proof.evaluate_migrations(observations, baseline_catalog_sha256=seen.sha256,
                                              baseline_tables=baseline.public())
            need(flags == {"rw": False, "ro": False, "negative_control": True}, "advisory-detector-not-triggered")
            self._idle()
            control_log = proof.parse_pg_log(**control["log"], application="compat-control")
            provenance = {"version": 1, "actor_kind": "synthetic-psql-fixture",
                "actor_is_signed_previous_binary": False, "collection_complete": True, "cleanup_verified": False,
                "candidate_source": self.contract.candidate_sha, "previous_source_context": self.contract.previous_sha,
                "actor_stdin_sha256": _ACTOR_SHA, "update_statement_sha256": _UPDATE_SHA,
                "baseline_catalog_sha256": seen.sha256, "final_catalog_sha256": final_catalog.sha256,
                "table_count": len(baseline.identities()), "permissions_identity": postgres.identity("permissions"),
                "rw": rw_provenance, "ro": ro_provenance, "migration_flags": flags,
                "control": {"exit_code": control["execution"]["exit_code"], "sqlstate": control["sqlstate"],
                            "log_bytes": len(control["log"]["raw"]), "log_sha256": control_log.sha256}}
            return AdvisoryObservation({"catalog_sha256": seen.sha256, "tables": baseline.public()},
                                       observations, provenance)
        except Exception:
            self.manager._protocol_failed = True
            self.catalog._failed = True
            raise
