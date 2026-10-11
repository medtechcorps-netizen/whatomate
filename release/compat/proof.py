"""Pure compatibility evidence checks; no Docker, GitHub, database or file I/O.

The caller must collect complete, correctly ordered observations. This module
does not authenticate the caller, classify source, verify signatures, or run a
compatibility experiment. See docs/compat.md for the deliberately narrow log
window contract and the remaining runner responsibilities.
"""

from __future__ import annotations

import datetime
import hashlib
import json
import re
from dataclasses import dataclass
from typing import Any


APPLICATIONS = frozenset({
    "compat-prev-server", "compat-cand-server", "compat-fwd",
    "compat-n1-rw", "compat-n1-ro", "compat-control",
})
SWEEP_CASES = frozenset({"contacts", "inbound", "outbound", "template", "flow", "settings"})
MAX_LOG_BYTES = 8 * 1024 * 1024
MAX_LOG_LINES = 100_000
MAX_LINE_BYTES = 64 * 1024
MAX_TABLES = 10_000
MAX_EVENTS = 10_000
READ_ONLY_CONTROL_SQL = "CREATE TABLE compat_probe.read_only_control (sentinel integer)"
RECORD_TAG_RE = re.compile(r"prod-([0-9]{8}T[0-9]{6}Z)-([0-9a-f]{8})\Z")
_HEX = re.compile(r"[0-9a-f]{64}\Z")
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_PREFIX = re.compile(
    r"(?P<time>[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}) UTC "
    r"\[(?P<pid>[1-9][0-9]{0,9})\] (?P<app>[a-z0-9-]+) (?P<state>[0-9A-Z]{5}) "
    r"(?P<level>LOG|ERROR|FATAL|PANIC|WARNING|NOTICE|INFO|DEBUG[1-5]|DETAIL|HINT|CONTEXT|STATEMENT|LOCATION): +(?P<body>.*)\Z"
)
_PRIMARY = frozenset({"LOG", "ERROR", "FATAL", "PANIC", "WARNING", "NOTICE", "INFO",
                      "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5"})
_EXECUTE = re.compile(r"execute (?:<unnamed>|[A-Za-z_][A-Za-z0-9_]{0,62}): (.+)\Z", re.DOTALL)
_MOD_HEAD = re.compile(r"(?:INSERT|UPDATE|DELETE|MERGE|CREATE|ALTER|DROP|TRUNCATE|COMMENT|GRANT|REVOKE|REINDEX|REFRESH)\b", re.IGNORECASE)
_INFRA = frozenset({"pull", "network", "boot-timeout", "capture", "container", "process"})


class EvidenceError(ValueError):
    """Incomplete, malformed or ambiguous evidence: no result is produced."""


class InfrastructureFailure(EvidenceError):
    """The experiment did not establish a semantic result; no flags exist."""


def _need(condition: bool, code: str = "evidence-invalid") -> None:
    if not condition:
        raise EvidenceError(code)


def _fields(value: Any, keys: set[str]) -> dict[str, Any]:
    _need(type(value) is dict and set(value) == keys)
    return value


def _boolean(value: Any) -> bool:
    _need(type(value) is bool)
    return value


def _number(value: Any, maximum: int = 1_000_000) -> int:
    _need(type(value) is int and 0 <= value <= maximum)
    return value


def _digest(value: Any) -> str:
    _need(type(value) is str and _HEX.fullmatch(value) is not None)
    return value


def _hashes(value: Any) -> tuple[str, ...]:
    _need(type(value) in (tuple, list) and len(value) <= MAX_EVENTS)
    return tuple(_digest(item) for item in value)


def validate_rollback_target(value: Any) -> str:
    _need(type(value) is str and (value == "prod-0000" or RECORD_TAG_RE.fullmatch(value) is not None),
          "rollback-target-invalid")
    return value


def artifact_name(sha: Any, run_attempt: Any) -> str:
    _need(type(sha) is str and _SHA.fullmatch(sha) is not None, "identity-invalid")
    _need(type(run_attempt) is int and 1 <= run_attempt <= 1_000_000, "identity-invalid")
    return f"compat-result-{sha}-{run_attempt}"


@dataclass(frozen=True)
class PgLog:
    application: str
    sha256: str
    records: int
    statement_hashes: tuple[str, ...]
    error_states: tuple[str, ...]
    failed_statements: tuple[tuple[str, str], ...]
    sqlstates: tuple[str, ...]


def parse_pg_log(raw: bytes, *, application: str, complete: bool) -> PgLog:
    """Parse one complete proof-period application window, never arbitrary stderr.

    UTC %m [%p] %a %e and VERBOSE are required. Tab-prefixed physical
    continuations belong only to the preceding field. Context/function traces,
    foreign applications and unknown LOG forms refuse. STATEMENT diagnostics
    accompany errors and are not counted a second time as executed statements.
    A genuinely empty, explicitly complete window is possible with mod logging;
    execution completion and the independently logged negative control remain
    mandatory in evaluate_proof().
    """
    _need(type(application) is str and application in APPLICATIONS, "pg-application-invalid")
    _need(complete is True, "pg-window-incomplete")
    _need(type(raw) is bytes and len(raw) <= MAX_LOG_BYTES, "pg-log-bounds")
    try:
        text = raw.decode("utf-8", errors="strict")
    except UnicodeError:
        raise EvidenceError("pg-log-encoding") from None
    _need(not text or text.endswith("\n"), "pg-log-truncated")
    _need(all(ord(c) >= 32 or c in "\t\n\r" for c in text), "pg-log-control")
    text = text.replace("\r\n", "\n")
    _need("\r" not in text, "pg-log-control")
    lines = text[:-1].split("\n") if text else []
    _need(len(lines) <= MAX_LOG_LINES, "pg-log-bounds")
    fields: list[dict[str, Any]] = []
    for line in lines:
        _need(len(line.encode("utf-8")) <= MAX_LINE_BYTES, "pg-log-bounds")
        if line.startswith("\t"):
            _need(bool(fields) and fields[-1]["level"] in {"LOG", "STATEMENT", "DETAIL", "HINT"},
                  "pg-continuation-ambiguous")
            # A full prefix embedded in a continuation is ambiguous, not a
            # second uncounted event. Raw SQL never appears in the error.
            _need(_PREFIX.fullmatch(line[1:]) is None, "pg-continuation-ambiguous")
            fields[-1]["parts"].append(line[1:])
            continue
        match = _PREFIX.fullmatch(line)
        _need(match is not None, "pg-prefix-invalid")
        field = match.groupdict()
        _need(field["app"] == application, "pg-application-mismatch")
        try:
            datetime.datetime.strptime(field["time"], "%Y-%m-%d %H:%M:%S.%f")
        except ValueError:
            raise EvidenceError("pg-time-invalid") from None
        field["parts"] = [field.pop("body")]
        fields.append(field)
    statements: list[str] = []
    errors: list[str] = []
    failed_statements: list[tuple[str, str]] = []
    sqlstates: list[str] = []
    primary: dict[str, Any] | None = None
    details: set[str] = set()
    records = 0
    for field in fields:
        level, body, state = field["level"], "\n".join(field["parts"]), field["state"]
        if level not in _PRIMARY:
            _need(primary is not None and all(field[k] == primary[k] for k in ("pid", "app", "state")),
                  "pg-detail-unbound")
            _need(level not in details, "pg-detail-duplicate")
            details.add(level)
            _need(level != "CONTEXT", "pg-function-context-unsupported")
            if level == "STATEMENT":
                _need(primary["level"] in {"ERROR", "FATAL", "PANIC"}, "pg-statement-unbound")
                _need(bool(body), "pg-message-empty")
                failed_statements.append((state, hashlib.sha256(body.encode("utf-8")).hexdigest()))
            continue
        primary, details = field, set()
        records += 1
        sqlstates.append(state)
        _need(body.startswith(state + ": "), "pg-verbose-state-mismatch")
        message = body[len(state) + 2:]
        _need(bool(message), "pg-message-empty")
        if level in {"ERROR", "FATAL", "PANIC"}:
            _need(state != "00000", "pg-error-state-invalid")
            errors.append(state)
        elif level == "LOG":
            _need(state == "00000", "pg-log-state-invalid")
            if message.startswith("statement: "):
                statement = message[len("statement: "):]
            else:
                match = _EXECUTE.fullmatch(message)
                _need(match is not None, "pg-log-form-unsupported")
                statement = match.group(1)
            _need(_MOD_HEAD.match(statement) is not None, "pg-statement-form-unsupported")
            statements.append(hashlib.sha256(statement.encode("utf-8")).hexdigest())
    _need(len(statements) <= MAX_EVENTS and len(errors) <= MAX_EVENTS, "pg-log-bounds")
    return PgLog(application, hashlib.sha256(raw).hexdigest(), records, tuple(statements), tuple(errors), tuple(failed_statements), tuple(sqlstates))


def _execution(value: Any) -> int:
    row = _fields(value, {"status", "exit_code", "failure"})
    if row["status"] == "infrastructure-error":
        _need(type(row["failure"]) is str and row["failure"] in _INFRA, "execution-invalid")
        raise InfrastructureFailure("infrastructure-" + row["failure"])
    _need(row["status"] == "completed" and row["failure"] is None, "execution-incomplete")
    return _number(row["exit_code"], 255)


def _check(value: Any) -> bool:
    row = _fields(value, {"execution", "passed"})
    code = _execution(row["execution"])
    passed = _boolean(row["passed"])
    return code == 0 and passed


def _tables(value: Any) -> dict[str, str]:
    row = _fields(value, {"complete", "digests"})
    _need(row["complete"] is True, "table-capture-incomplete")
    pairs = row["digests"]
    _need(type(pairs) is dict and len(pairs) <= MAX_TABLES, "table-capture-invalid")
    return {_digest(key): _digest(value) for key, value in pairs.items()}


def _log(value: Any, application: str) -> PgLog:
    row = _fields(value, {"raw", "complete"})
    return parse_pg_log(row["raw"], application=application, complete=row["complete"])


def _migration(value: Any, application: str) -> tuple[int, PgLog, tuple[str, ...], str, dict[str, str]]:
    row = _fields(value, {"execution", "log", "markers", "catalog_sha256", "tables"})
    code = _execution(row["execution"])
    log = _log(row["log"], application)
    markers = _fields(row["markers"], {"complete", "application", "hashes"})
    _need(markers["complete"] is True and markers["application"] == application, "marker-capture-invalid")
    return code, log, _hashes(markers["hashes"]), _digest(row["catalog_sha256"]), _tables(row["tables"])


def _sweep(value: Any) -> bool:
    row = _fields(value, {"execution", "cases"})
    code = _execution(row["execution"])
    cases = _fields(row["cases"], set(SWEEP_CASES))
    flags = [_boolean(item) for item in cases.values()]
    return code == 0 and all(flags)


def _boot(value: Any) -> bool:
    row = _fields(value, {"execution", "first_log_observed", "ready", "startup_contract_observed", "rls_negative_control"})
    code = _execution(row["execution"])
    if not _boolean(row["first_log_observed"]):
        raise InfrastructureFailure("infrastructure-boot-no-log")
    ready = _boolean(row["ready"])
    contract = _boolean(row["startup_contract_observed"])
    control = row["rls_negative_control"]
    if contract:
        _need(control is None, "startup-control-ambiguous")
        proven = True
    else:
        control = _fields(control, {"execution", "force_rls_removed", "refusal_observed"})
        control_code = _execution(control["execution"])
        removed, refused = _boolean(control["force_rls_removed"]), _boolean(control["refusal_observed"])
        proven = control_code != 0 and removed and refused
    return code == 0 and ready and proven


def _terminal_old_boot(value: Any, identity: dict[str, Any]) -> dict[str, Any]:
    """Only the post-migration old boot may have this private terminal form.

    These are collector facts, not authentication of an arbitrary dictionary.
    The public runner accepts no observations; the owned observer establishes
    the source/handle, complete log window and stopped state before returning.
    Migration/control execution shapes deliberately remain unchanged.
    """
    row = _fields(value, {"state", "phase", "binding", "terminal", "logs"})
    _need(row["state"] == "observed-semantic-failure" and row["phase"] == "old.boot", "terminal-phase-invalid")
    binding = _fields(row["binding"], {"sha", "previous_source_sha", "previous_web_digest", "run_id", "run_attempt",
                                      "owner_token", "container_id", "started_at"})
    for key in ("sha", "previous_web_digest", "run_id", "run_attempt"):
        _need(type(binding[key]) is type(identity[key]) and binding[key] == identity[key], "terminal-identity-invalid")
    _need(type(binding["previous_source_sha"]) is str and _SHA.fullmatch(binding["previous_source_sha"]), "terminal-source-invalid")
    _need(type(binding["owner_token"]) is str and re.fullmatch(r"[0-9a-f]{32}", binding["owner_token"]), "terminal-owner-invalid")
    _digest(binding["container_id"])
    terminal = _fields(row["terminal"], {"running", "exit_code", "finished_at", "oom_killed", "daemon_error", "restart_count"})
    _need(terminal["running"] is False and terminal["oom_killed"] is False and terminal["daemon_error"] is False
          and type(terminal["restart_count"]) is int and terminal["restart_count"] == 0, "terminal-state-invalid")
    code = _number(terminal["exit_code"], 255)
    _need(code == 2, "terminal-exit-unsupported")  # Only an observed Go panic in this first slice.
    def instant(raw):
        _need(type(raw) is str and re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z", raw), "terminal-time-invalid")
        try:
            return datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))
        except ValueError:
            raise EvidenceError("terminal-time-invalid") from None
    _need(instant(terminal["finished_at"]) >= instant(binding["started_at"]), "terminal-time-invalid")
    logs = _fields(row["logs"], {"complete", "stdout_sha256", "stderr_sha256", "stdout_bytes", "stderr_bytes",
                                "lines", "start_markers", "rls_markers", "fatal_markers", "panic_markers"})
    _need(logs["complete"] is True, "terminal-log-incomplete")
    for stream in ("stdout", "stderr"):
        _digest(logs[stream + "_sha256"])
        _number(logs[stream + "_bytes"], 1024 * 1024)
    _need(logs["stdout_bytes"] + logs["stderr_bytes"] > 0 and _number(logs["lines"], 20_000) > 0,
          "terminal-log-incomplete")
    _need(_number(logs["start_markers"]) == _number(logs["rls_markers"]) == 1, "terminal-contract-unproven")
    fatal, panic = _number(logs["fatal_markers"], 20_000), _number(logs["panic_markers"], 20_000)
    _need(fatal == 0 and panic > 0, "terminal-defect-unproven")
    return binding


def _blocked_old_server(value: Any, phase: str, binding: dict[str, Any]) -> None:
    row = _fields(value, {"state", "phase", "cause_phase", "reason", "binding"})
    reason = "retained-record-unavailable" if phase in {"reverse.boot", "reverse.read"} else "server-unavailable"
    copied = _fields(row["binding"], set(binding))
    _need(row["state"] == "blocked-by-semantic" and row["phase"] == phase and row["cause_phase"] == "old.boot"
          and row["reason"] == reason and all(type(copied[k]) is type(v) and copied[k] == v for k, v in binding.items()),
          "blocked-cause-invalid")


def _classification(value: Any) -> dict[str, Any]:
    row = _fields(value, {"kind", "dormant"})
    _need(type(row["kind"]) is str and row["kind"] in {"none", "data", "expand", "contract"}, "classification-invalid")
    dormant = _boolean(row["dormant"])
    _need((row["kind"] != "none" or not dormant) and (row["kind"] != "data" or dormant)
          and (row["kind"] != "contract" or not dormant), "classification-invalid")
    return dict(row)


def evaluate_migrations(value: Any, *, baseline_catalog_sha256: Any, baseline_tables: Any) -> dict[str, bool]:
    """Check complete raw migration/control observations against an actual D0.

    Return only migration flags, not a compatibility result. The caller must
    independently establish the baseline's complete inventory, quiescence,
    ownership and capture order; this pure helper authenticates none of them.
    No forward, server or worker observations are required or synthesized.
    Invalid/missing evidence and an unproven control raise, never return false.
    """
    s0, d0 = _digest(baseline_catalog_sha256), _tables(baseline_tables)
    # evaluate_proof already establishes a nonempty verified table inventory.
    # Standalone advisory callers must not turn a missing D0 into empty equality.
    _need(bool(d0), "migration-baseline-incomplete")
    old = _fields(value, {"rw", "ro", "negative_control"})
    rw = _migration(old["rw"], "compat-n1-rw")
    ro = _migration(old["ro"], "compat-n1-ro")
    control = _fields(old["negative_control"], {"execution", "sqlstate", "log"})
    control_code, control_log = _execution(control["execution"]), _log(control["log"], "compat-control")
    _need(control_code != 0 and control["sqlstate"] == "25006" and "25006" in control_log.error_states,
          "negative-control-unproven")
    # The control is a deliberately failed CREATE TABLE, never a successful
    # write or a different error accidentally accompanying a 25006 message.
    _need(control_log.error_states == ("25006",), "negative-control-ambiguous")
    expected_control = hashlib.sha256(READ_ONLY_CONTROL_SQL.encode("utf-8")).hexdigest()
    _need(control_log.failed_statements == (("25006", expected_control),), "negative-control-statement-unproven")
    _need(control_log.statement_hashes in ((), (expected_control,)), "negative-control-ambiguous")
    migration_flags = []
    for code, log, markers, catalog, tables in (rw, ro):
        migration_flags.append(code == 0 and not log.error_states and "25006" not in log.sqlstates and not log.statement_hashes and not markers
                               and catalog == s0 and tables == d0)
    return {"rw": migration_flags[0], "ro": migration_flags[1], "negative_control": True}


def evaluate_proof(value: Any) -> dict[str, Any]:
    """Return the exact public result only after every required input is checked.

    Completed semantic failures produce false flags. Missing observations,
    infrastructure failures, invalid control and ambiguous logs raise instead.
    The caller must never publish a favorable partial result on an exception.
    """
    row = _fields(value, {"identity", "classification", "previous_boot", "expected_catalog_sha256", "expected_preexisting_tables",
                          "before_tables", "forward", "quiescence", "old_migrate", "old_server", "final_catalog_sha256"})
    identity = _fields(row["identity"], {"sha", "previous_release", "previous_web_digest", "run_id", "run_attempt"})
    artifact_name(identity["sha"], identity["run_attempt"])
    validate_rollback_target(identity["previous_release"])
    _need(type(identity["previous_web_digest"]) is str and re.fullmatch(r"sha256:[0-9a-f]{64}", identity["previous_web_digest"]) is not None,
          "identity-invalid")
    _need(type(identity["run_id"]) is int and 1 <= identity["run_id"] < 10**16, "identity-invalid")
    classification = _classification(row["classification"])
    previous_boot = _boot(row["previous_boot"])
    expected = _digest(row["expected_catalog_sha256"])
    expected_tables = _hashes(row["expected_preexisting_tables"])
    _need(bool(expected_tables) and list(expected_tables) == sorted(set(expected_tables)), "table-inventory-invalid")
    before = _tables(row["before_tables"])
    _need(set(before) == set(expected_tables), "table-inventory-mismatch")
    fwd = _fields(row["forward"], {"passes", "allowed_statement_hashes", "allowed_marker_hashes", "candidate_ready", "write_sweep"})
    _need(type(fwd["passes"]) in (list, tuple) and len(fwd["passes"]) == 2, "forward-passes-incomplete")
    # Evaluate both passes and every later phase, even if an earlier semantic
    # flag is false: short-circuiting must not hide missing/infra evidence.
    passes = [_migration(item, "compat-fwd") for item in fwd["passes"]]
    allowed_statements = _hashes(fwd["allowed_statement_hashes"])
    allowed_markers = _hashes(fwd["allowed_marker_hashes"])
    no_mod = classification["kind"] == "none" or classification["dormant"]
    if no_mod:
        _need(not allowed_statements and not allowed_markers, "classification-authorization-invalid")
    immutable = classification["kind"] in {"none", "expand"} or classification["dormant"]
    pass_flags = []
    for code, log, markers, catalog, tables in passes:
        data_ok = not immutable or all(key in tables and tables[key] == digest for key, digest in before.items())
        inventory_ok = not no_mod or set(tables) == set(before)
        pass_flags.append(code == 0 and not log.error_states and "25006" not in log.sqlstates and catalog == expected and data_ok and inventory_ok
                          and set(log.statement_hashes) <= set(allowed_statements)
                          and set(markers) <= set(allowed_markers))
    candidate_ready, candidate_write = _check(fwd["candidate_ready"]), _sweep(fwd["write_sweep"])
    quiet = _fields(row["quiescence"], {"candidate_stopped", "remaining_client_sessions", "catalog_sha256", "tables"})
    _need(_boolean(quiet["candidate_stopped"]) and _number(quiet["remaining_client_sessions"]) == 0,
          "quiescence-unproven")
    d0, s0 = _tables(quiet["tables"]), _digest(quiet["catalog_sha256"])
    _need(set(expected_tables) <= set(d0) and set(d0) == set(passes[1][4]), "quiescence-inventory-unproven")
    old_migrate = evaluate_migrations(row["old_migrate"], baseline_catalog_sha256=s0,
                                      baseline_tables=quiet["tables"])
    server = _fields(row["old_server"], {"boot", "read_sweep", "write_sweep", "workers", "stopped", "candidate_ready", "reverse_sweep"})
    terminal = type(server["boot"]) is dict and server["boot"].get("state") == "observed-semantic-failure"
    if terminal:
        binding = _terminal_old_boot(server["boot"], identity)
        for key, phase in (("read_sweep", "old.read"), ("write_sweep", "old.write"), ("workers", "old.workers"),
                           ("candidate_ready", "reverse.boot"), ("reverse_sweep", "reverse.read")):
            _blocked_old_server(server[key], phase, binding)
        # Actual removal is mandatory, not another blocked observation.
        stopped = _check(server["stopped"])
        _need(stopped, "terminal-removal-unproven")
        boot = read = write = worker_ok = rebooted = reverse = False
    else:
        boot, read, write = _boot(server["boot"]), _sweep(server["read_sweep"]), _sweep(server["write_sweep"])
        workers = _fields(server["workers"], {"execution", "observed_seconds", "panic_count", "unknown_type_count", "unexpected_stub_entries"})
        worker_code = _execution(workers["execution"])
        seconds = _number(workers["observed_seconds"], 2700)
        panics = _number(workers["panic_count"])
        unknown = _number(workers["unknown_type_count"])
        unexpected = _number(workers["unexpected_stub_entries"])
        _need(seconds >= 60 or worker_code != 0 or panics > 0 or unknown > 0 or unexpected > 0, "workers-window-incomplete")
        worker_ok = worker_code == 0 and seconds >= 60 and panics == unknown == unexpected == 0
        stopped, rebooted, reverse = _check(server["stopped"]), _check(server["candidate_ready"]), _sweep(server["reverse_sweep"])
    final_catalog = _digest(row["final_catalog_sha256"])
    forward = previous_boot and all(pass_flags) and candidate_ready and candidate_write and s0 == expected
    result = {
        **identity, "classification": classification, "forward": forward,
        "old_migrate_ok": old_migrate,
        "old_server_ok": {"boot": boot, "read": read, "write": write,
                          "reverse": stopped and rebooted and reverse and final_catalog == expected,
                          "workers": worker_ok},
        "reversible": all(old_migrate.values()), "catalog_sha256": final_catalog,
    }
    validate_result(result)
    return result


def validate_result(value: Any) -> None:
    row = _fields(value, {"sha", "previous_release", "previous_web_digest", "classification", "forward", "old_migrate_ok",
                          "old_server_ok", "reversible", "catalog_sha256", "run_id", "run_attempt"})
    artifact_name(row["sha"], row["run_attempt"])
    validate_rollback_target(row["previous_release"])
    _need(type(row["previous_web_digest"]) is str and re.fullmatch(r"sha256:[0-9a-f]{64}", row["previous_web_digest"]) is not None,
          "identity-invalid")
    _need(type(row["run_id"]) is int and 1 <= row["run_id"] < 10**16, "identity-invalid")
    _classification(row["classification"])
    _digest(row["catalog_sha256"])
    _boolean(row["forward"])
    migrate = _fields(row["old_migrate_ok"], {"rw", "ro", "negative_control"})
    server = _fields(row["old_server_ok"], {"boot", "read", "write", "reverse", "workers"})
    for flag in (*migrate.values(), *server.values()):
        _boolean(flag)
    _need(migrate["negative_control"] is True, "negative-control-unproven")
    _need(_boolean(row["reversible"]) == all(migrate.values()), "reversible-invalid")


def result_bytes(value: Any) -> bytes:
    validate_result(value)
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode("ascii")
