from __future__ import annotations

import copy
import hashlib
import json
import sys
import unittest
from unittest import mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof


SHA = "a" * 40
CATALOG = "b" * 64
TABLE = "c" * 64
DATA = "d" * 64
CHANGED = "e" * 64


def execution(code=0):
    return {"status": "completed", "exit_code": code, "failure": None}


def check(passed=True):
    return {"execution": execution(), "passed": passed}


def tables(digest=DATA):
    return {"complete": True, "digests": {TABLE: digest}}


def line(app, level="LOG", state="00000", body="statement: UPDATE fixture SET value = 1", pid=41):
    primary = level in proof._PRIMARY
    message = state + ": " + body if primary else body
    return f"2026-10-11 01:02:03.004 UTC [{pid}] {app} {state} {level}:  {message}\n".encode()


def control_log():
    return (line("compat-control", "ERROR", "25006", "cannot execute CREATE TABLE in a read-only transaction")
            + line("compat-control", "LOCATION", "25006", "PreventCommandIfReadOnly, utility.c:414")
            + line("compat-control", "STATEMENT", "25006", proof.READ_ONLY_CONTROL_SQL))


def migration(app):
    return {"execution": execution(), "log": {"raw": b"", "complete": True},
            "markers": {"complete": True, "application": app, "hashes": []},
            "catalog_sha256": CATALOG, "tables": tables()}


def sweep():
    return {"execution": execution(), "cases": {key: True for key in sorted(proof.SWEEP_CASES)}}


def boot():
    return {"execution": execution(), "first_log_observed": True, "ready": True,
            "startup_contract_observed": True, "rls_negative_control": None}


def observations():
    return {
        "identity": {"sha": SHA, "previous_release": "prod-20261011T000000Z-aaaaaaaa",
                     "previous_web_digest": "sha256:" + "f" * 64, "run_id": 12345, "run_attempt": 2},
        "classification": {"kind": "none", "dormant": False},
        "previous_boot": boot(), "expected_catalog_sha256": CATALOG, "expected_preexisting_tables": [TABLE], "before_tables": tables(),
        "forward": {"passes": [migration("compat-fwd"), migration("compat-fwd")],
                    "allowed_statement_hashes": [], "allowed_marker_hashes": [],
                    "candidate_ready": check(), "write_sweep": sweep()},
        "quiescence": {"candidate_stopped": True, "remaining_client_sessions": 0,
                       "catalog_sha256": CATALOG, "tables": tables()},
        "old_migrate": {"rw": migration("compat-n1-rw"), "ro": migration("compat-n1-ro"),
                        "negative_control": {"execution": execution(1), "sqlstate": "25006",
                                             "log": {"raw": control_log(), "complete": True}}},
        "old_server": {"boot": boot(), "read_sweep": sweep(), "write_sweep": sweep(),
                       "workers": {"execution": execution(), "observed_seconds": 60,
                                   "panic_count": 0, "unknown_type_count": 0, "unexpected_stub_entries": 0},
                       "stopped": check(), "candidate_ready": check(), "reverse_sweep": sweep()},
        "final_catalog_sha256": CATALOG,
    }


def terminal_boot(identity=None, previous_source="e" * 40):
    identity = identity or observations()["identity"]
    return {"state": "observed-semantic-failure", "phase": "old.boot",
        "binding": {**{key: identity[key] for key in ("sha", "previous_web_digest", "run_id", "run_attempt")},
                    "previous_source_sha": previous_source, "owner_token": "1" * 32, "container_id": "2" * 64,
                    "started_at": "2026-10-11T00:00:01Z"},
        "terminal": {"running": False, "exit_code": 2, "finished_at": "2026-10-11T00:00:03Z",
                     "oom_killed": False, "daemon_error": False, "restart_count": 0},
        "logs": {"complete": True, "stdout_sha256": hashlib.sha256(b"").hexdigest(), "stderr_sha256": "3" * 64,
                 "stdout_bytes": 0, "stderr_bytes": 600, "lines": 4, "start_markers": 1, "rls_markers": 1,
                 "fatal_markers": 0, "panic_markers": 1}}


def terminal_observations():
    value = observations(); old = value["old_server"]; old["boot"] = terminal_boot(value["identity"])
    for key, phase in (("read_sweep", "old.read"), ("write_sweep", "old.write"), ("workers", "old.workers"),
                       ("candidate_ready", "reverse.boot"), ("reverse_sweep", "reverse.read")):
        old[key] = {"state": "blocked-by-semantic", "phase": phase, "cause_phase": "old.boot",
                    "reason": "retained-record-unavailable" if phase.startswith("reverse.") else "server-unavailable",
                    "binding": dict(old["boot"]["binding"])}
    return value


class MigrationEvaluatorTests(unittest.TestCase):
    @staticmethod
    def evidence():
        # Deliberately no unrelated forward/server/worker proof observations.
        return {"rw": migration("compat-n1-rw"), "ro": migration("compat-n1-ro"),
                "negative_control": {"execution": execution(1), "sqlstate": "25006",
                                     "log": {"raw": control_log(), "complete": True}}}

    def evaluate(self, value, **baseline):
        return proof.evaluate_migrations(value, **({"baseline_catalog_sha256": CATALOG,
                                                    "baseline_tables": tables()} | baseline))

    def test_standalone_exact_flags_without_unrelated_phases(self):
        value = self.evidence(); saved = copy.deepcopy(value)
        result = self.evaluate(value)
        self.assertEqual(result, {"rw": True, "ro": True, "negative_control": True})
        self.assertTrue(all(type(flag) is bool for flag in result.values()))
        self.assertEqual(value, saved)
        result["rw"] = False
        self.assertTrue(self.evaluate(value)["rw"])

    def test_swallowed_update_and_read_only_error_are_independently_false(self):
        value = self.evidence()
        value["rw"]["log"]["raw"] = line("compat-n1-rw")
        value["ro"]["log"]["raw"] = line("compat-n1-ro", "ERROR", "25006", "read only")
        # Both actors claim exit zero and retain the baseline logical data.
        self.assertEqual(self.evaluate(value), {"rw": False, "ro": False, "negative_control": True})

    def test_each_complete_rw_and_ro_violation_preserves_other_flag(self):
        mutations = {
            "exit": lambda row: row["execution"].update(exit_code=1),
            "mod": lambda row: row["log"].update(raw=line(row["markers"]["application"])),
            "error": lambda row: row["log"].update(raw=line(row["markers"]["application"], "ERROR", "42501", "permission")),
            "warning-25006": lambda row: row["log"].update(raw=line(row["markers"]["application"], "WARNING", "25006", "read only")),
            "marker": lambda row: row["markers"].update(hashes=[CHANGED]),
            "catalog": lambda row: row.update(catalog_sha256=CHANGED),
            "data": lambda row: row.update(tables=tables(CHANGED)),
            "deleted-table": lambda row: row["tables"].update(digests={}),
            "added-table": lambda row: row["tables"]["digests"].update({CHANGED: DATA}),
        }
        for mode in ("rw", "ro"):
            for name, mutate in mutations.items():
                value = self.evidence(); mutate(value[mode])
                with self.subTest(mode=mode, signal=name):
                    result = self.evaluate(value)
                    self.assertFalse(result[mode])
                    self.assertTrue(result["ro" if mode == "rw" else "rw"])
                    self.assertTrue(result["negative_control"])

    def test_control_refusal_even_when_both_migrations_are_already_false(self):
        mutations = {
            "success": lambda row: row["execution"].update(exit_code=0),
            "state": lambda row: row.update(sqlstate="42501"),
            "incomplete": lambda row: row["log"].update(complete=False),
            "missing": lambda row: row["log"].update(raw=b""),
            "wrong-query": lambda row: row["log"].update(raw=control_log().replace(proof.READ_ONLY_CONTROL_SQL.encode(), b"UPDATE fixture SET value=1")),
            "missing-statement": lambda row: row["log"].update(raw=line("compat-control", "ERROR", "25006", "read only")),
            "duplicate": lambda row: row["log"].update(raw=control_log() + control_log()),
            "other-error": lambda row: row["log"].update(raw=control_log() + line("compat-control", "ERROR", "42501", "permission")),
            "unrelated-write": lambda row: row["log"].update(raw=line("compat-control") + control_log()),
            "foreign-app": lambda row: row["log"].update(raw=control_log().replace(b"compat-control", b"compat-n1-ro")),
            "extra-field": lambda row: row.update(passed=True),
        }
        for name, mutate in mutations.items():
            value = self.evidence()
            for mode in ("rw", "ro"): value[mode]["execution"]["exit_code"] = 1
            mutate(value["negative_control"])
            with self.subTest(control=name), self.assertRaises(proof.EvidenceError): self.evaluate(value)

    def test_control_accepts_only_optional_exact_executed_create(self):
        value = self.evidence()
        value["negative_control"]["log"]["raw"] = line("compat-control", body="statement: " + proof.READ_ONLY_CONTROL_SQL) + control_log()
        self.assertTrue(self.evaluate(value)["negative_control"])

    def test_missing_and_malformed_migration_observations_never_become_flags(self):
        for mode in ("rw", "ro", "negative_control"):
            value = self.evidence(); del value[mode]
            with self.subTest(missing=mode), self.assertRaises(proof.EvidenceError): self.evaluate(value)
        mutations = [lambda row: row.pop("execution"), lambda row: row.update(execution={"status": "not-run", "exit_code": None, "failure": None}),
            lambda row: row["execution"].update(exit_code=True), lambda row: row["log"].update(complete=False),
            lambda row: row["log"].update(raw=line("compat-fwd")), lambda row: row["log"].update(raw=line("compat-n1-rw")[:-1]),
            lambda row: row["markers"].update(complete=False), lambda row: row["markers"].update(application="compat-fwd"),
            lambda row: row["markers"].update(hashes=["not-a-digest"]), lambda row: row["tables"].update(complete=False),
            lambda row: row.update(catalog_sha256="not-a-digest"), lambda row: row.update(passed=True)]
        for mode in ("rw", "ro"):
            for index, mutate in enumerate(mutations):
                value = self.evidence(); mutate(value[mode])
                with self.subTest(mode=mode, mutation=index), self.assertRaises(proof.EvidenceError): self.evaluate(value)
        value = self.evidence(); value["forward"] = {}
        with self.assertRaises(proof.EvidenceError): self.evaluate(value)

    def test_complete_nonempty_baseline_is_required_and_bounds_remain(self):
        baselines = [{"baseline_catalog_sha256": "bad"}, {"baseline_tables": None},
                     {"baseline_tables": {"complete": False, "digests": {TABLE: DATA}}},
                     {"baseline_tables": {"complete": True, "digests": {}}},
                     {"baseline_tables": {"complete": True, "digests": {TABLE: "bad"}}},
                     {"baseline_tables": {"complete": True, "digests": {TABLE: DATA}, "expected": True}}]
        for baseline in baselines:
            with self.subTest(baseline=baseline), self.assertRaises(proof.EvidenceError): self.evaluate(self.evidence(), **baseline)
        with mock.patch.object(proof, "MAX_TABLES", 0), self.assertRaises(proof.EvidenceError): self.evaluate(self.evidence())
        with mock.patch.object(proof, "MAX_LOG_BYTES", 0), self.assertRaises(proof.EvidenceError): self.evaluate(self.evidence())

    def test_infrastructure_failure_never_becomes_a_negative_advisory(self):
        for phase in ("rw", "ro", "negative_control"):
            for failure in sorted(proof._INFRA):
                value = self.evidence(); value["rw"]["execution"]["exit_code"] = 1
                value[phase]["execution"] = {"status": "infrastructure-error", "exit_code": None, "failure": failure}
                with self.subTest(phase=phase, failure=failure), self.assertRaises(proof.InfrastructureFailure): self.evaluate(value)

    def test_full_proof_delegates_and_still_requires_later_evidence(self):
        value = observations()
        with mock.patch.object(proof, "evaluate_migrations", wraps=proof.evaluate_migrations) as shared:
            result = proof.evaluate_proof(value)
        shared.assert_called_once_with(value["old_migrate"], baseline_catalog_sha256=CATALOG,
                                       baseline_tables=value["quiescence"]["tables"])
        self.assertEqual(result["old_migrate_ok"], self.evaluate(self.evidence()))
        value["old_migrate"]["rw"]["execution"]["exit_code"] = 1
        del value["old_server"]["reverse_sweep"]
        with self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_standalone_result_contains_no_raw_private_evidence(self):
        value = self.evidence(); sentinel = "PRIVATE_VALUE_MUST_NOT_ESCAPE"
        value["rw"]["log"]["raw"] = line("compat-n1-rw", body="statement: UPDATE fixture SET value='" + sentinel + "'")
        result = self.evaluate(value)
        self.assertNotIn(sentinel, json.dumps(result))
        self.assertEqual(set(result), {"rw", "ro", "negative_control"})
        # This result is intentionally not a full compatibility artifact.
        with self.assertRaises(proof.EvidenceError): proof.result_bytes(result)
        value["rw"]["log"]["raw"] = sentinel.encode() + b"\n"
        with self.assertRaises(proof.EvidenceError) as caught: self.evaluate(value)
        self.assertEqual(str(caught.exception), "pg-prefix-invalid")


class PgParserTests(unittest.TestCase):
    def test_verbose_error_statement_location_bound_without_double_count(self):
        parsed = proof.parse_pg_log(control_log(), application="compat-control", complete=True)
        self.assertEqual(parsed.records, 1)
        self.assertEqual(parsed.error_states, ("25006",))
        self.assertEqual(parsed.statement_hashes, ())
        self.assertEqual(parsed.failed_statements, (("25006", hashlib.sha256(proof.READ_ONLY_CONTROL_SQL.encode()).hexdigest()),))

    def test_statement_execute_multiline_and_crlf(self):
        for envelope in ("statement: ", "execute <unnamed>: ", "execute stmtcache_abc123: "):
            with self.subTest(envelope=envelope):
                statement = "UPDATE fixture\nSET value = 1"
                raw = line("compat-n1-rw", body=envelope + "UPDATE fixture") + b"\tSET value = 1\n"
                for input_raw in (raw, raw.replace(b"\n", b"\r\n")):
                    result = proof.parse_pg_log(input_raw, application="compat-n1-rw", complete=True)
                    self.assertEqual(result.statement_hashes, (hashlib.sha256(statement.encode()).hexdigest(),))

    def test_error_diagnostic_is_not_an_executed_mod_line(self):
        raw = line("compat-n1-ro", "ERROR", "25006", "read only")
        raw += line("compat-n1-ro", "STATEMENT", "25006", "UPDATE fixture SET value = 1")
        parsed = proof.parse_pg_log(raw, application="compat-n1-ro", complete=True)
        self.assertFalse(parsed.statement_hashes)
        self.assertEqual(parsed.error_states, ("25006",))

    def test_attribution_or_ambiguous_multiline_refuses(self):
        good = line("compat-n1-rw")
        cases = {
            "foreign-app": line("compat-fwd"),
            "missing-app": good.replace(b"compat-n1-rw", b""),
            "startup": b"2026-10-11 01:02:03.004 UTC [41]  00000 LOG:  00000: starting PostgreSQL\n",
            "orphan": b"\tUPDATE fixture SET value = 2\n",
            "unprefixed": good + b"UPDATE fixture SET value = 2\n",
            "embedded-prefix": good + b"\t" + good,
            "wrong-inner-state": good.replace(b"LOG:  00000:", b"LOG:  25006:"),
            "missing-inner-state": good.replace(b"LOG:  00000: ", b"LOG:  "),
            "partial": good[:-1],
            "blank-line": good + b"\n",
            "bad-date": good.replace(b"2026-10-11", b"2026-99-11"),
            "bare-cr": good.replace(b"UPDATE", b"UPDATE\r"),
            "utf8": b"\xff\n",
            "nul": good + b"\x00\n",
            "not-a-mod-envelope": line("compat-n1-rw", body="checkpoint complete"),
            "function-call": line("compat-n1-rw", body="statement: SELECT mutate()"),
            "anonymous-do": line("compat-n1-rw", body="statement: DO $$ BEGIN UPDATE fixture SET value=1; END $$"),
            "function-context": good + line("compat-n1-rw", "CONTEXT", body="SQL statement UPDATE fixture SET value=1"),
            "unbound-detail": line("compat-n1-rw", "STATEMENT", "25006", "UPDATE fixture SET value=1"),
            "different-pid": line("compat-n1-rw", "ERROR", "25006", "read only") + line("compat-n1-rw", "LOCATION", "25006", "site", pid=42),
            "diagnostic-after-log": good + line("compat-n1-rw", "STATEMENT", body="UPDATE fixture SET value=2"),
            "duplicate-detail": line("compat-n1-rw", "ERROR", "25006", "read only") + 2 * line("compat-n1-rw", "STATEMENT", "25006", "UPDATE fixture SET value=2"),
        }
        for name, raw in cases.items():
            with self.subTest(name=name), self.assertRaises(proof.EvidenceError):
                proof.parse_pg_log(raw, application="compat-n1-rw", complete=True)

    def test_bounds_and_completeness(self):
        cases = [b"x" * (proof.MAX_LOG_BYTES + 1), b"\n" * (proof.MAX_LOG_LINES + 1),
                 line("compat-fwd", body="statement: UPDATE " + "x" * proof.MAX_LINE_BYTES)]
        for raw in cases:
            with self.subTest(size=len(raw)), self.assertRaises(proof.EvidenceError):
                proof.parse_pg_log(raw, application="compat-fwd", complete=True)
        with self.assertRaises(proof.EvidenceError):
            proof.parse_pg_log(b"", application="compat-fwd", complete=False)
        self.assertEqual(proof.parse_pg_log(b"", application="compat-fwd", complete=True).records, 0)


class ProofTests(unittest.TestCase):
    def test_terminal_old_boot_blocks_only_server_subtree_and_retains_actual_migrations(self):
        value = terminal_observations(); result = proof.evaluate_proof(value)
        self.assertTrue(result["forward"]); self.assertTrue(result["reversible"])
        self.assertEqual(set(result["old_server_ok"].values()), {False})
        value["old_migrate"]["rw"]["execution"]["exit_code"] = 1
        result = proof.evaluate_proof(value)
        self.assertFalse(result["reversible"]); self.assertFalse(result["old_migrate_ok"]["rw"])
        raw = proof.result_bytes(result)
        for private in (b"blocked-by-semantic", b"owner_token", b"container_id", b"finished_at"):
            self.assertNotIn(private, raw)

    def test_terminal_facts_and_same_experiment_binding_are_mandatory(self):
        mutations = [
            lambda r: r.update(phase="previous.boot"),
            lambda r: r.update(execution=execution(1)),
            lambda r: r["binding"].update(sha="c" * 40),
            lambda r: r["binding"].update(run_id=True),
            lambda r: r["binding"].update(previous_web_digest="sha256:" + "c" * 64),
            lambda r: r["binding"].update(container_id="foreign"),
            lambda r: r["terminal"].update(running=True),
            lambda r: r["terminal"].update(oom_killed=True),
            lambda r: r["terminal"].update(daemon_error=True),
            lambda r: r["terminal"].update(restart_count=1),
            lambda r: r["terminal"].update(exit_code=137),
            lambda r: r["terminal"].update(exit_code=0),
            lambda r: r["terminal"].update(exit_code=1),
            lambda r: r["terminal"].update(finished_at="2026-10-10T00:00:00Z"),
            lambda r: r["logs"].update(complete=False),
            lambda r: r["logs"].update(lines=0),
            lambda r: r["logs"].update(start_markers=0),
            lambda r: r["logs"].update(rls_markers=2),
            lambda r: r["logs"].update(panic_markers=0),
            lambda r: r["logs"].update(fatal_markers=1),
        ]
        for index, mutate in enumerate(mutations):
            value = terminal_observations(); mutate(value["old_server"]["boot"])
            with self.subTest(index=index), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_blocked_nodes_cannot_invent_completion_or_arbitrary_dependency(self):
        mutations = [lambda r:r.update(execution=execution()), lambda r:r.update(cases={}),
            lambda r:r.update(observed_seconds=60), lambda r:r.update(cause_phase="old.read"),
            lambda r:r.update(cause_phase="reverse.read"), lambda r:r.update(cause_phase="missing"),
            lambda r:r.update(phase="old.write"), lambda r:r.update(reason="unavailable"),
            lambda r:r["binding"].update(container_id="3" * 64)]
        for index, mutate in enumerate(mutations):
            value = terminal_observations(); mutate(value["old_server"]["read_sweep"])
            with self.subTest(index=index), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)
        value = terminal_observations(); value["old_server"]["boot"] = boot()
        with self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_terminal_never_waives_migration_control_final_catalog_or_real_removal(self):
        for path in (("old_migrate", "rw"), ("old_migrate", "ro"), ("old_migrate", "negative_control"),
                     ("old_server", "stopped"), ("quiescence",), ("final_catalog_sha256",)):
            value = terminal_observations(); target = value
            for key in path[:-1]: target = target[key]
            target[path[-1]] = copy.deepcopy(value["old_server"]["read_sweep"])
            with self.subTest(path=path), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)
        value = terminal_observations(); value["old_server"]["stopped"] = check(False)
        with self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_complete_exact_result_and_attempt_artifact(self):
        result = proof.evaluate_proof(observations())
        self.assertTrue(result["forward"])
        self.assertTrue(result["reversible"])
        self.assertTrue(all(result["old_migrate_ok"].values()))
        self.assertTrue(all(result["old_server_ok"].values()))
        self.assertEqual(set(result), {"sha", "previous_release", "previous_web_digest", "classification", "forward",
                                      "old_migrate_ok", "old_server_ok", "reversible", "catalog_sha256", "run_id", "run_attempt"})
        raw = proof.result_bytes(result)
        self.assertEqual(json.loads(raw), result)
        self.assertEqual(proof.artifact_name(SHA, 2), "compat-result-" + SHA + "-2")
        self.assertNotEqual(proof.artifact_name(SHA, 1), proof.artifact_name(SHA, 2))

    def test_n1_rw_swallowed_update_and_each_immutable_signal(self):
        for mutation in ("exit", "mod", "marker", "digest", "catalog", "table-deleted", "new-table"):
            with self.subTest(mutation=mutation):
                value = observations()
                rw = value["old_migrate"]["rw"]
                if mutation == "exit": rw["execution"]["exit_code"] = 1
                if mutation == "mod": rw["log"]["raw"] = line("compat-n1-rw")  # exit 0, same catalog/data
                if mutation == "marker": rw["markers"]["hashes"] = [CHANGED]
                if mutation == "digest": rw["tables"] = tables(CHANGED)
                if mutation == "catalog": rw["catalog_sha256"] = CHANGED
                if mutation == "table-deleted": rw["tables"]["digests"] = {}
                if mutation == "new-table": rw["tables"]["digests"][CHANGED] = DATA
                result = proof.evaluate_proof(value)
                self.assertFalse(result["old_migrate_ok"]["rw"])
                self.assertFalse(result["reversible"])

    def test_n1_ro_25006_swallowed_by_exit_zero(self):
        for severity in ("ERROR", "WARNING", "NOTICE"):
            value = observations()
            value["old_migrate"]["ro"]["log"]["raw"] = line("compat-n1-ro", severity, "25006", "read only")
            result = proof.evaluate_proof(value)
            with self.subTest(severity=severity):
                self.assertFalse(result["old_migrate_ok"]["ro"])
                self.assertFalse(result["reversible"])

    def test_control_must_fail_exact_create_and_parser_observe_25006(self):
        for mutation in ("succeeded", "wrong-state", "missing-log", "wrong-log-state", "wrong-query", "missing-statement", "mixed-error", "duplicate-error", "unrelated-write", "foreign-app"):
            with self.subTest(mutation=mutation):
                value = observations()
                control = value["old_migrate"]["negative_control"]
                if mutation == "succeeded": control["execution"]["exit_code"] = 0
                if mutation == "wrong-state": control["sqlstate"] = "42501"
                if mutation == "missing-log": control["log"]["raw"] = b""
                if mutation == "wrong-log-state": control["log"]["raw"] = control_log().replace(b"25006", b"42501")
                if mutation == "wrong-query": control["log"]["raw"] = control_log().replace(proof.READ_ONLY_CONTROL_SQL.encode(), b"UPDATE fixture SET value=1")
                if mutation == "missing-statement": control["log"]["raw"] = line("compat-control", "ERROR", "25006", "read only")
                if mutation == "mixed-error": control["log"]["raw"] += line("compat-control", "ERROR", "42501", "permission")
                if mutation == "duplicate-error": control["log"]["raw"] += line("compat-control", "ERROR", "25006", "another refusal")
                if mutation == "unrelated-write": control["log"]["raw"] = line("compat-control") + control["log"]["raw"]
                if mutation == "foreign-app": control["log"]["raw"] = control_log().replace(b"compat-control", b"compat-n1-ro")
                with self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)
        value = observations()
        value["old_migrate"]["negative_control"]["log"]["raw"] = line("compat-control", body="statement: " + proof.READ_ONLY_CONTROL_SQL) + control_log()
        self.assertTrue(proof.evaluate_proof(value)["old_migrate_ok"]["negative_control"])

    def test_forward_requires_both_passes_catalog_data_and_authorization(self):
        classes = [{"kind": "none", "dormant": False}, {"kind": "data", "dormant": True},
                   {"kind": "expand", "dormant": False}, {"kind": "expand", "dormant": True}]
        for classification in classes:
            for index in (0, 1):
                for mutation in ("catalog", "data", "deleted", "mod", "marker", "exit"):
                    with self.subTest(classification=classification, index=index, mutation=mutation):
                        value = observations()
                        value["classification"] = classification
                        migration = value["forward"]["passes"][index]
                        if mutation == "catalog": migration["catalog_sha256"] = CHANGED
                        if mutation == "data": migration["tables"] = tables(CHANGED)
                        if mutation == "deleted": migration["tables"]["digests"] = {}
                        if mutation == "mod": migration["log"]["raw"] = line("compat-fwd")
                        if mutation == "marker": migration["markers"]["hashes"] = [CHANGED]
                        if mutation == "exit": migration["execution"]["exit_code"] = 1
                        if mutation == "deleted" and index == 1:
                            with self.assertRaisesRegex(proof.EvidenceError, "quiescence-inventory-unproven"):
                                proof.evaluate_proof(value)
                            continue
                        result = proof.evaluate_proof(value)
                        self.assertFalse(result["forward"])
                        self.assertTrue(result["reversible"])  # only N-1 migrate derives this

    def test_expand_exact_statement_and_marker_authorization(self):
        value = observations()
        value["classification"] = {"kind": "expand", "dormant": False}
        raw = line("compat-fwd", body="statement: CREATE TABLE fixture_new (value integer)")
        parsed = proof.parse_pg_log(raw, application="compat-fwd", complete=True)
        value["forward"]["allowed_statement_hashes"] = list(parsed.statement_hashes)
        value["forward"]["allowed_marker_hashes"] = [CHANGED]
        first = value["forward"]["passes"][0]
        first["log"]["raw"] = raw
        first["markers"]["hashes"] = [CHANGED]
        first["tables"]["digests"][CHANGED] = DATA
        self.assertTrue(proof.evaluate_proof(value)["forward"])
        first["log"]["raw"] += line("compat-fwd")
        self.assertFalse(proof.evaluate_proof(value)["forward"])
        value["classification"] = {"kind": "data", "dormant": True}
        with self.assertRaisesRegex(proof.EvidenceError, "classification-authorization-invalid"):
            proof.evaluate_proof(value)

    def test_no_mod_inventory_cannot_hide_addition_outside_known_catalog_hash(self):
        for classification, allowed in (({"kind": "none", "dormant": False}, False),
                                        ({"kind": "data", "dormant": True}, False),
                                        ({"kind": "expand", "dormant": True}, False),
                                        ({"kind": "expand", "dormant": False}, True)):
            value = observations()
            value["classification"] = classification
            # The known-object catalog digest intentionally stays identical.
            # Full inventory must independently reject an unknown extra table.
            for capture in (value["forward"]["passes"][0], value["forward"]["passes"][1],
                            value["quiescence"], value["old_migrate"]["rw"], value["old_migrate"]["ro"]):
                capture["tables"]["digests"][CHANGED] = DATA
            with self.subTest(classification=classification):
                result = proof.evaluate_proof(value)
                self.assertEqual(result["forward"], allowed)
                self.assertTrue(result["reversible"])

    def test_missing_and_not_run_evidence_refuses_not_false_success(self):
        variants = []
        for key in observations():
            value = observations(); del value[key]; variants.append(value)
        for path in ("forward", "rw", "ro", "control", "worker", "reverse"):
            value = observations()
            target = {"forward": value["forward"]["passes"][1], "rw": value["old_migrate"]["rw"],
                      "ro": value["old_migrate"]["ro"], "control": value["old_migrate"]["negative_control"],
                      "worker": value["old_server"]["workers"], "reverse": value["old_server"]["reverse_sweep"]}[path]
            target["execution"] = {"status": "not-run", "exit_code": None, "failure": None}
            variants.append(value)
        value = observations(); value["forward"]["passes"].pop(); variants.append(value)
        value = observations(); value["old_server"]["read_sweep"]["cases"].pop("contacts"); variants.append(value)
        value = observations(); value["old_server"]["workers"]["observed_seconds"] = 59; variants.append(value)
        value = observations(); value["before_tables"]["complete"] = False; variants.append(value)
        value = observations(); value["old_migrate"]["rw"]["log"]["complete"] = False; variants.append(value)
        value = observations(); value["old_migrate"]["rw"]["markers"]["complete"] = False; variants.append(value)
        value = observations(); value["old_migrate"]["rw"]["markers"]["application"] = "compat-fwd"; variants.append(value)
        for index, value in enumerate(variants):
            with self.subTest(index=index), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_infrastructure_never_returns_semantic_flags_even_after_false_phase(self):
        for failure in sorted(proof._INFRA):
            value = observations()
            value["forward"]["candidate_ready"]["passed"] = False
            value["old_server"]["reverse_sweep"]["execution"] = {
                "status": "infrastructure-error", "exit_code": None, "failure": failure}
            with self.subTest(failure=failure), self.assertRaises(proof.InfrastructureFailure): proof.evaluate_proof(value)
        value = observations(); value["old_server"]["boot"]["first_log_observed"] = False
        with self.assertRaises(proof.InfrastructureFailure): proof.evaluate_proof(value)

    def test_server_failures_do_not_redefine_reversibility(self):
        for section in ("boot", "read_sweep", "write_sweep", "reverse_sweep", "workers", "stopped", "candidate_ready"):
            value = observations()
            target = value["old_server"][section]
            target["execution"]["exit_code"] = 1
            result = proof.evaluate_proof(value)
            key = {"read_sweep": "read", "write_sweep": "write", "reverse_sweep": "reverse", "stopped": "reverse", "candidate_ready": "reverse"}.get(section, section)
            with self.subTest(section=section):
                self.assertFalse(result["old_server_ok"][key]); self.assertTrue(result["reversible"])
        for field in ("panic_count", "unknown_type_count", "unexpected_stub_entries"):
            value = observations(); value["old_server"]["workers"][field] = 1
            self.assertFalse(proof.evaluate_proof(value)["old_server_ok"]["workers"])

    def test_startup_contract_or_real_rls_negative_control(self):
        value = observations()
        for key in (value["previous_boot"], value["old_server"]["boot"]):
            key["startup_contract_observed"] = False
            key["rls_negative_control"] = {"execution": execution(1), "force_rls_removed": True, "refusal_observed": True}
        self.assertTrue(proof.evaluate_proof(value)["forward"])
        self.assertTrue(proof.evaluate_proof(value)["old_server_ok"]["boot"])
        value["old_server"]["boot"]["rls_negative_control"]["execution"]["exit_code"] = 0
        self.assertFalse(proof.evaluate_proof(value)["old_server_ok"]["boot"])
        value["old_server"]["boot"]["rls_negative_control"] = None
        with self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_quiescence_and_final_catalog(self):
        for key, replacement in (("candidate_stopped", False), ("remaining_client_sessions", 1)):
            value = observations(); value["quiescence"][key] = replacement
            with self.subTest(key=key), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)
        value = observations(); value["final_catalog_sha256"] = CHANGED
        self.assertFalse(proof.evaluate_proof(value)["old_server_ok"]["reverse"])

    def test_expected_table_inventory_prevents_vacuous_or_omitted_baseline(self):
        for mutation in ("empty-expected", "empty-before", "omitted", "extra", "duplicate", "unsorted", "empty-d0", "missing-d0"):
            value = observations()
            if mutation == "empty-expected":
                value["expected_preexisting_tables"] = []; value["before_tables"]["digests"] = {}
            if mutation == "empty-before": value["before_tables"]["digests"] = {}
            if mutation == "omitted": value["expected_preexisting_tables"] = [TABLE, CHANGED]
            if mutation == "extra": value["before_tables"]["digests"][CHANGED] = DATA
            if mutation == "duplicate": value["expected_preexisting_tables"] = [TABLE, TABLE]
            if mutation == "unsorted": value["expected_preexisting_tables"] = [CHANGED, TABLE]
            if mutation == "empty-d0": value["quiescence"]["tables"]["digests"] = {}
            if mutation == "missing-d0": value["forward"]["passes"][1]["tables"]["digests"][CHANGED] = DATA
            with self.subTest(mutation=mutation), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_public_privacy_exact_shape_and_fixed_errors(self):
        sentinel = "PRIVATE_SQL_VALUE_do_not_emit"
        value = observations()
        value["old_migrate"]["rw"]["log"]["raw"] = line("compat-n1-rw", body="statement: UPDATE fixture SET value = '" + sentinel + "'")
        raw = proof.result_bytes(proof.evaluate_proof(value))
        self.assertNotIn(sentinel.encode(), raw)
        self.assertNotIn(b"fixture", raw)
        self.assertNotIn(b"statement", raw)
        value["old_migrate"]["rw"]["log"]["raw"] = sentinel.encode() + b"\n"
        with self.assertRaises(proof.EvidenceError) as caught: proof.evaluate_proof(value)
        self.assertEqual(str(caught.exception), "pg-prefix-invalid")
        result = proof.evaluate_proof(observations())
        result["host"] = "forbidden"
        with self.assertRaises(proof.EvidenceError): proof.result_bytes(result)
        for key, bad in (("sha", "not-a-sha"), ("run_id", True), ("run_attempt", 0), ("previous_release", "other"),
                         ("previous_web_digest", "https://invalid.example")):
            value = observations(); value["identity"][key] = bad
            with self.subTest(key=key), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)

    def test_classification_and_result_boolean_validation(self):
        for classification in ({"kind": "none", "dormant": True}, {"kind": "data", "dormant": False},
                               {"kind": "contract", "dormant": True}, {"kind": "dormant", "dormant": True},
                               {"kind": "none", "dormant": 0}, {"kind": "none", "dormant": False, "reason": "private"}):
            value = observations(); value["classification"] = classification
            with self.subTest(classification=classification), self.assertRaises(proof.EvidenceError): proof.evaluate_proof(value)
        result = proof.evaluate_proof(observations())
        for key, replacement in (("reversible", False), ("forward", 1)):
            bad = copy.deepcopy(result); bad[key] = replacement
            with self.subTest(key=key), self.assertRaises(proof.EvidenceError): proof.result_bytes(bad)

    def test_target_regex_and_no_attempt_one_guard(self):
        for target in ("prod-0000", "prod-20261011T010203Z-0123abcd"):
            self.assertEqual(proof.validate_rollback_target(target), target)
        for target in ("", "prod-0000\n", "prod-20261011T010203Z-0123ABCD", "../prod-0000", "prod-0000;echo", 12):
            with self.subTest(target=target), self.assertRaises(proof.EvidenceError): proof.validate_rollback_target(target)
        for attempt in (1, 2, 9):
            value = observations(); value["identity"]["run_attempt"] = attempt
            self.assertEqual(proof.evaluate_proof(value)["run_attempt"], attempt)


if __name__ == "__main__":
    unittest.main()
