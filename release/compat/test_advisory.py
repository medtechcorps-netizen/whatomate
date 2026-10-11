"""Real owned adapter composition over a strict fake Docker/PG transport only."""
import copy
from dataclasses import replace
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import advisory as a
import test_catalog
import test_postgres
import test_processes


class World(test_catalog.World):
    def __init__(self):
        super().__init__()
        self.sql = test_postgres.FakeDocker()
        for row in self.inventory:
            if row[0] == "permissions":
                row[2] = [[key, "pg_catalog", "text", -1, i] for i, key in enumerate(("resource", "action", "description"), 1)]
        self.sql.inventory, self.sql.state = self.inventory, self.state
        self.sql.rows = [["row", "permissions", ["users", "read", "synthetic description"]]]
        self.target_count = 1
        self.session_user, self.session_role, self.session_owner = "compat_owner", True, True
        self.session_readonly = None
        self.actor_calls = []
        self.actor_mutation = None
        self.after_actor = None
        self.session_raw = None
        self.ro_logged_mod = False
        self.log_mutation = None
        self.transport_error = False
        self.after_control = None

    def capture(self, argv, **options):
        args = argv[5:]
        identifier = next((key for key, row in self.containers.items() if row.get("Config", {}).get("Image") == a.runtime.POSTGRES), None)
        if identifier and args == ["logs", identifier]:
            self.calls.append((list(args), options))
            return a.runtime.Capture(0, self.sql.stdout, self.sql.stderr)
        if args[:2] == ["exec", "-i"] and "--user" not in args:
            self.calls.append((list(args), options))
            raw = options["input_bytes"]
            if raw in (a._SESSION.encode(), a._ACTOR.encode()):
                application = next(v.split("=", 1)[1] for v in args if v.startswith("PGAPPNAME="))
                readonly = application == "compat-n1-ro"
                self.assert_actor_args(args, application, readonly, identifier)
                if raw == a._SESSION.encode():
                    expected = self.session_readonly or ("on" if readonly else "off")
                    output = self.session_raw or test_postgres.output(["advisory-session", self.session_user, "compat",
                        expected, self.session_role, self.session_owner, self.target_count])
                    return a.runtime.Capture(0, output, b"")
                self.actor_calls.append(application)
                if self.transport_error:
                    raise a.proof.InfrastructureFailure("capture")
                if readonly:
                    log = test_postgres.line(application, "ERROR", "cannot execute UPDATE in a read-only transaction", "25006")
                    log += test_postgres.line(application, "STATEMENT", a._UPDATE, "25006")
                    if self.ro_logged_mod:
                        log = test_postgres.line(application, "LOG", "statement: " + a._UPDATE) + log
                    result = a.runtime.Capture(0, b"COMPAT_ADVISORY_ACTOR_FINISHED\n", b"private psql error")
                else:
                    log = test_postgres.line(application, "LOG", "statement: " + a._UPDATE)
                    result = a.runtime.Capture(0, b"1\nCOMPAT_ADVISORY_ACTOR_FINISHED\n", b"")
                # REL_17_11 command.c: quit consumes no argument, then
                # HandleSlashCmds warns for the unconsumed 0 on stderr.
                # This source-derived model is not a real psql invocation.
                if b"\\quit 0\n" in raw:
                    result = a.runtime.Capture(result.returncode, result.stdout,
                        result.stderr + b'\\quit: extra argument "0" ignored\n')
                if self.log_mutation:
                    log = self.log_mutation(application, log)
                self.sql.stderr += log
                if self.actor_mutation:
                    result = self.actor_mutation(application, result)
                if self.after_actor:
                    self.after_actor(application)
                return result
            result = self.sql(argv, **options)
            if raw == a.proof.READ_ONLY_CONTROL_SQL.encode() and self.after_control:
                self.after_control()
            return result
        return super().capture(argv, **options)

    @staticmethod
    def assert_actor_args(args, application, readonly, identifier):
        assert args == ["exec", "-i", "--env", "PGAPPNAME=" + application, "--env", "PGCLIENTENCODING=UTF8",
            "--env", "PGOPTIONS=-c default_transaction_read_only=" + ("on" if readonly else "off")
            + " -c statement_timeout=60000 -c lock_timeout=2000", identifier,
            "psql", "-X", "-w", "-qAt", "-h", a.postgres.SOCKET_DIRECTORY, "-p", "5432",
            "-U", "compat_owner", "-d", "compat", "-v", "ON_ERROR_STOP=1"]


class AdvisoryTests(unittest.TestCase):
    def make(self):
        fixture = test_catalog.CatalogTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        with mock.patch.object(test_catalog, "World", World):
            observed = fixture.setup_owned()
        # One bound fake transport object stands in for the real shared capture.
        observed.pg.capture = observed.manager.owner.capture
        self.world, self.catalog = fixture.world, observed
        self.pg, self.manager = observed.pg, observed.manager
        self.owner = self.manager.owner
        return a.OwnedSwallowedUpdate(observed, test_processes.INPUTS)

    def refused(self, actor, code=None):
        with self.assertRaises(a.proof.EvidenceError) as raised:
            actor.collect()
        if code is not None:
            self.assertEqual(str(raised.exception), code)
        count = len(self.world.calls)
        with self.assertRaisesRegex(a.proof.EvidenceError, "advisory-already-attempted"):
            actor.collect()
        self.assertEqual(len(self.world.calls), count)
        self.assertTrue(self.manager._protocol_failed)
        self.assertTrue(self.catalog._failed)

    def test_fixed_update_and_corrected_argument_free_actor_hashes(self):
        self.assertEqual(a.postgres.sha(a._ACTOR.encode()), "5aa17473d9b1b64ab50b64ea6b1e4c0c8d95e98e844f3dbb3a773b37d6757b3e")
        self.assertEqual(a.postgres.sha(a._UPDATE.encode()), "abc599a7aeb21a7a7cbdfdca05775a94dc3e31f94ba73d4a619d87dfb493a69a")
        # PostgreSQL 17 documents \q / \quit without an exit-code argument.
        self.assertEqual(a._ACTOR.splitlines(), [r"\set ON_ERROR_STOP off", a._UPDATE,
            r"\echo COMPAT_ADVISORY_ACTOR_FINISHED", r"\quit"])

    def test_former_quit_argument_warning_is_refused_without_relaxing_stderr(self):
        old = a._ACTOR.replace("\\quit\n", "\\quit 0\n")
        self.assertEqual(a.postgres.sha(old.encode()), "cf67bf5c47c5632da951cb0fb8c3d1b2119746aa844c70f5a5aef8e2678eaa07")
        with mock.patch.object(a, "_ACTOR", old), mock.patch.object(a, "_ACTOR_SHA", a.postgres.sha(old.encode())):
            actor = self.make()
            self.refused(actor, "advisory-actor-rw-stderr")
        self.assertEqual(self.world.actor_calls, ["compat-n1-rw"])

    def test_stream_failures_have_fixed_phase_specific_codes_and_no_retry(self):
        for mode in ("rw", "ro"):
            for stream in ("stdout", "stderr"):
                with self.subTest(mode=mode, stream=stream):
                    actor = self.make()
                    def mutate(app, result):
                        if app != "compat-n1-" + mode: return result
                        if stream == "stdout":
                            return a.runtime.Capture(0, result.stdout + b"private-extra\n", result.stderr)
                        return a.runtime.Capture(0, result.stdout, b"private-warning" if mode == "rw" else b"")
                    self.world.actor_mutation = mutate
                    self.refused(actor, "advisory-actor-" + mode + "-" + stream)
                    self.assertEqual(self.world.actor_calls.count("compat-n1-" + mode), 1)

    def test_actual_adapters_collect_and_share_detector_without_fake_full_phases(self):
        actor = self.make()
        with mock.patch.object(a.proof, "evaluate_migrations", wraps=a.proof.evaluate_migrations) as evaluator:
            result = actor.collect()
        evaluator.assert_called_once()
        facts = result.provenance()
        self.assertEqual(facts["migration_flags"], {"rw": False, "ro": False, "negative_control": True})
        self.assertEqual(self.world.actor_calls, ["compat-n1-rw", "compat-n1-ro"])
        self.assertEqual(facts["table_count"], 127)
        self.assertEqual(facts["rw"]["returned_rows"], 1)
        self.assertEqual(facts["ro"]["returned_rows"], 0)
        self.assertEqual(facts["rw"]["mod_count"], 1)
        self.assertEqual(facts["ro"]["error_states"], ["25006"])
        self.assertEqual(facts["actor_kind"], "synthetic-psql-fixture")
        self.assertFalse(facts["actor_is_signed_previous_binary"])
        self.assertFalse(facts["cleanup_verified"])
        self.assertEqual(set(result._observations), {"rw", "ro", "negative_control"})
        self.assertTrue(self.pg._control_attempted)
        self.assertIsNone(self.pg._phase)
        self.assertFalse(self.manager._ids)
        self.assertFalse(self.manager._protocol_failed)
        self.assertEqual(self.world.sql.events, [])
        self.assertEqual(len(self.world.data_calls), 0)  # Data came from real PG collector via fake SQL, not caller maps.
        with self.assertRaises(a.proof.EvidenceError): a.proof.result_bytes(facts)
        count = len(self.world.calls)
        with self.assertRaisesRegex(a.proof.EvidenceError, "advisory-already-attempted"): actor.collect()
        with self.assertRaisesRegex(a.proof.EvidenceError, "advisory-already-attempted"):
            a.OwnedSwallowedUpdate(self.catalog, test_processes.INPUTS).collect()
        self.assertEqual(len(self.world.calls), count)

    def test_optional_ro_logged_update_is_still_detected(self):
        actor = self.make(); self.world.ro_logged_mod = True
        result = actor.collect().provenance()
        self.assertEqual(result["ro"]["mod_count"], 1)
        self.assertFalse(result["migration_flags"]["ro"])

    def test_fixed_session_and_exact_target_count_before_mutation(self):
        for key, value in (("target_count", 0), ("target_count", 2), ("target_count", True),
                           ("session_user", "postgres"), ("session_role", False), ("session_role", 1),
                           ("session_owner", False), ("session_readonly", "on"),
                           ("session_raw", b"[]\n"), ("session_raw", b"{}\n{}\n")):
            with self.subTest(key=key, value=value):
                actor = self.make(); setattr(self.world, key, value); self.refused(actor)
                self.assertEqual(self.world.actor_calls, [])

    def test_target_count_after_actor_is_required(self):
        actor = self.make(); self.world.after_actor = lambda app: setattr(self.world, "target_count", 0)
        self.refused(actor)
        self.assertEqual(self.world.actor_calls, ["compat-n1-rw"])

    def test_actual_complete_data_and_source_inventory_cannot_be_vacuous(self):
        for case in ("missing-permissions", "empty", "missing-row", "changed-data", "changed-column", "partial"):
            with self.subTest(case=case):
                actor = self.make()
                if case == "missing-permissions": self.world.sql.inventory[:] = [x for x in self.world.sql.inventory if x[0] != "permissions"]
                if case == "empty": self.world.sql.inventory[:] = []
                if case == "partial": self.world.sql.bad_complete = True
                if case == "missing-row": self.world.after_actor = lambda app: self.world.sql.rows.clear()
                if case == "changed-data": self.world.after_actor = lambda app: self.world.sql.rows[0][2].__setitem__(2, "changed")
                if case == "changed-column": self.world.after_actor = lambda app: next(x for x in self.world.sql.inventory if x[0] == "permissions")[2][2].__setitem__(3, 12)
                self.refused(actor)

    def test_phase_nonzero_truncated_duplicate_or_extra_stdout_refuses_without_retry(self):
        for mode in ("compat-n1-rw", "compat-n1-ro"):
            for kind in ("exit", "truncated", "duplicate", "foreign", "wrong-count", "stderr"):
                with self.subTest(mode=mode, kind=kind):
                    actor = self.make()
                    def mutate(app, result):
                        if app != mode: return result
                        if kind == "exit": return a.runtime.Capture(1, result.stdout, result.stderr)
                        if kind == "stderr": return a.runtime.Capture(0, result.stdout, b"unexpected" if app.endswith("rw") else b"")
                        raw = {"truncated": result.stdout[:-1], "duplicate": result.stdout + result.stdout,
                               "foreign": result.stdout + b"private-extra\n", "wrong-count": b"2\nCOMPAT_ADVISORY_ACTOR_FINISHED\n"}[kind]
                        return a.runtime.Capture(0, raw, result.stderr)
                    self.world.actor_mutation = mutate
                    self.refused(actor)
                    self.assertEqual(self.world.actor_calls.count(mode), 1)

    def test_bad_log_windows_never_become_advisory_success(self):
        for mode in ("compat-n1-rw", "compat-n1-ro"):
            for kind in ("missing", "duplicate", "wrong-query", "foreign", "warning", "unframed", "wrong-state"):
                with self.subTest(mode=mode, kind=kind):
                    actor = self.make()
                    def mutate(app, log):
                        if app != mode: return log
                        if kind == "missing": return b""
                        if kind == "duplicate": return log + log
                        if kind == "wrong-query": return log.replace(a._UPDATE.encode(), b"UPDATE public.contacts SET name=name;")
                        if kind == "foreign": return log.replace(app.encode(), b"foreign")
                        if kind == "warning": return log + test_postgres.line(app, "WARNING", "extra", "01000")
                        if kind == "unframed": return b"unattributed\n" + log
                        return log.replace(b"25006", b"42501") if app.endswith("ro") else log.replace(b"00000", b"42501")
                    self.world.log_mutation = mutate
                    self.refused(actor)

    def test_independent_create_control_is_never_replaced_by_the_update(self):
        for name, value in (("control_code", 0), ("control_missing", True), ("control_state", "42501"),
                            ("control_sql", a._UPDATE), ("control_owner", ["compat_owner", "off"])):
            with self.subTest(name=name):
                actor = self.make(); setattr(self.world.sql, name, value); self.refused(actor)
                self.assertEqual(self.world.actor_calls, ["compat-n1-rw", "compat-n1-ro"])

    def test_catalog_changes_and_actual_cli_failure_refuse(self):
        for kind in ("sha", "unknown", "exit"):
            with self.subTest(kind=kind):
                actor = self.make()
                def change(app):
                    if kind == "exit": self.world.catalog_exit = 1
                    else:
                        row = json.loads(self.world.catalog_response)
                        if kind == "sha": row["sha256"] = "a" * 64
                        else: row["unknown"] = [{"kind": "relations", "count": 1}]
                        self.world.catalog_response = (json.dumps(row) + "\n").encode()
                self.world.after_actor = change
                self.refused(actor)

    def test_identity_transport_source_and_idle_drift_refuse(self):
        changes = [lambda: setattr(self.pg, "container", "f" * 64), lambda: setattr(self.pg, "token", "f" * 32),
            lambda: setattr(self.pg, "_binding", "changed"), lambda: setattr(self.pg, "_seal", object()),
            lambda: setattr(self.pg, "runtime_role", "postgres"), lambda: setattr(self.owner, "source_sha", "f" * 40),
            lambda: setattr(self.contract, "candidate_sha", "f" * 40),
            lambda: self.manager.bindings.update(previous=replace(self.manager.bindings["previous"])),
            lambda: self.manager._ids.update(web="f" * 64), lambda: setattr(self.manager, "_protocol_failed", True),
            lambda: setattr(self.pg, "capture", lambda *args, **kwargs: self.fail("foreign transport called"))]
        for index in range(len(changes)):
            with self.subTest(index=index):
                actor = self.make(); self.contract = self.catalog.contract
                changes[index](); self.refused(actor)
                self.assertEqual(self.world.actor_calls, [])

    def test_post_actor_ownership_drift_or_transport_exception_is_consumed(self):
        actor = self.make(); self.world.transport_error = True; self.refused(actor)
        self.assertEqual(self.world.actor_calls, ["compat-n1-rw"])
        actor = self.make(); self.world.after_actor = lambda app: setattr(self.pg, "_binding", "drift")
        self.refused(actor)
        self.assertEqual(self.world.actor_calls, ["compat-n1-rw"])

    def test_probe_changes_unexpected_ddl_or_pg_restart_refuse(self):
        for kind in ("probe", "ddl", "restart", "sessions"):
            with self.subTest(kind=kind):
                actor = self.make()
                def mutate(app):
                    if kind == "probe": self.world.sql.function += " changed"
                    if kind == "ddl": self.world.sql.events.append([1, "ddl_command_start", "ALTER TABLE", app, "ALTER TABLE public.permissions"])
                    if kind == "restart": self.world.containers[self.pg.container]["State"]["StartedAt"] = "2026-10-11T01:00:00Z"
                    if kind == "sessions": self.world.sql.state[9] = 1
                self.world.after_actor = mutate
                self.refused(actor)
                self.assertEqual(self.world.actor_calls, ["compat-n1-rw"])

    def test_final_capture_after_independent_control_remains_mandatory(self):
        for kind in ("catalog", "data", "count", "owner", "capture"):
            with self.subTest(kind=kind):
                actor = self.make()
                def mutate():
                    if kind == "catalog":
                        row = json.loads(self.world.catalog_response); row["sha256"] = "a" * 64
                        self.world.catalog_response = (json.dumps(row) + "\n").encode()
                    if kind == "data": self.world.sql.rows[0][2][2] = "late-change"
                    if kind == "count": self.world.target_count = 2
                    if kind == "owner": self.owner.token = "e" * 32
                    if kind == "capture": self.world.sql.bad_complete = True
                self.world.after_control = mutate
                self.refused(actor)
                self.assertEqual(self.world.actor_calls, ["compat-n1-rw", "compat-n1-ro"])

    def test_active_probe_sessions_or_prior_capture_do_not_get_adopted(self):
        for kind in ("probe", "control", "baseline", "phase", "sessions"):
            with self.subTest(kind=kind):
                actor = self.make()
                if kind == "probe": self.pg._probe_signature = "previous"
                if kind == "control": self.pg._control_attempted = True
                if kind == "baseline": self.catalog._initial_attempted = True
                if kind == "phase": self.pg._phase = object()
                if kind == "sessions": self.world.sql.state[9] = 1
                self.refused(actor)
                self.assertEqual(self.world.actor_calls, [])

    def test_public_provenance_excludes_raw_sql_rows_credentials_and_owned_ids(self):
        actor = self.make(); result = actor.collect(); value = result.provenance()
        raw = json.dumps(value)
        for secret in ("synthetic description", "private psql error", a._UPDATE, a._SESSION,
                       self.owner.token, self.pg.container, test_processes.INPUTS.owner_password):
            self.assertNotIn(secret, raw)
        self.assertNotIn("compat-result", raw)
        self.assertNotIn("private", repr(result))
        value["migration_flags"]["rw"] = True
        self.assertFalse(result.provenance()["migration_flags"]["rw"])

    def test_api_cannot_receive_raw_observations_dsn_or_sql(self):
        with self.assertRaises(a.proof.EvidenceError): a.OwnedSwallowedUpdate({}, test_processes.INPUTS)
        actor = self.make()
        with self.assertRaises(TypeError): actor.collect(sql=a._UPDATE)
        with self.assertRaises(TypeError): actor.collect(observations={})
        with self.assertRaises(TypeError): a.OwnedSwallowedUpdate(self.catalog, test_processes.INPUTS, dsn="private")
        with mock.patch.object(a, "_ACTOR", "UPDATE arbitrary;"):
            with self.assertRaisesRegex(a.proof.EvidenceError, "advisory-sql-changed"):
                a.OwnedSwallowedUpdate(self.catalog, test_processes.INPUTS)


if __name__ == "__main__":
    unittest.main(verbosity=2)
