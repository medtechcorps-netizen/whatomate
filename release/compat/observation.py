"""Bounded observations of one already-owned synthetic web process.

This is collection, not a second proof evaluator. Boot/worker rows feed proof.py.
Whole Docker streams remain private in memory. Only the pinned logf startup
grammar supplies positive evidence; opaque lines are still scanned for failures.
The current source emits an explicit tenant-RLS success message. Older images
without it remain incomplete: no FORCE-RLS control is fabricated here.

The caller runs sweeps before workers(), then grants this observer exclusive
control of the manager for its quiet >=60 second interval. Every Graph row is
retained. Only the exact number of observed journal GET rows caused by this
collector may be attributed to collection; no sweep-triggered rows are ignored.
Real web/controller integration remains a separate required validation.
"""
from __future__ import annotations

from dataclasses import asdict
from datetime import datetime
from contextlib import contextmanager
import hashlib
import json
import math
from pathlib import Path
import re
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
import controller
import processes

proof = processes.proof
_SGR = re.compile(r"\x1b\[(?:0|31|33|35|36)m")
_DOCKER_TIME = re.compile(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z) (.*)\Z")
_HEADER = re.compile(r"timestamp=\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} ")
# Pinned logf writes empty strings as key=, including empty message values.
_FIELD = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)=("(?:[^"\\]|\\["\\nrtbf/]|\\u[0-9a-fA-F]{4})*"|[^\s"]*)(?: |$)')
_UNKNOWN = re.compile(r"\bunknown(?:[- ]job)?[- ]type\b", re.IGNORECASE)
_PANIC = re.compile(r"\bpanic(?:ked)?\b", re.IGNORECASE)
_MAIN = re.compile(r"(?:^|/)cmd/whatomate/main\.go:[1-9][0-9]*\Z")
_WORKER = re.compile(r"(?:^|/)internal/worker/worker\.go:[1-9][0-9]*\Z")
_OLD_BOOT = object()


def need(value, code="observation-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def _instant(text):
    need(type(text) is str and re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z", text))
    try:
        # datetime truncates nanoseconds; only comparisons at this precision
        # are needed, and the original timestamp stays in the hashed bytes.
        return datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        raise proof.EvidenceError("observation-time-invalid") from None


def parse_logs(stdout: bytes, stderr: bytes, *, started_at: str):
    """Private bounded Docker --timestamps streams, never rendered or saved."""
    start = _instant(started_at)
    records, panic_count, unknown_count, lines = [], 0, 0, 0
    for raw in (stdout, stderr):
        need(type(raw) is bytes and len(raw) <= processes.runtime.MIB
             and (not raw or raw.endswith(b"\n")), "observation-log-incomplete")
        try:
            decoded = raw.decode("utf-8", "strict")
        except UnicodeError:
            raise proof.EvidenceError("observation-log-invalid") from None
        previous = start
        for line in decoded.splitlines():
            lines += 1
            need(lines <= 20_000, "observation-log-bounds")
            match = _DOCKER_TIME.fullmatch(line)
            need(match is not None, "observation-log-format")
            stamp = _instant(match[1])
            need(stamp >= previous, "observation-log-order")
            previous = stamp
            text = _SGR.sub("", match[2])
            need(not any((ord(char) < 32 and char != "\t") or ord(char) == 127 for char in text), "observation-log-control")
            panic_count += bool(_PANIC.search(text))
            unknown_count += bool(_UNKNOWN.search(text))
            header = _HEADER.match(text)
            if header is None:
                continue  # Opaque runtime/GORM lines are not positive markers.
            fields, pos = {}, header.end()
            while pos < len(text):
                part = _FIELD.match(text, pos)
                need(part is not None and part[1] not in fields, "observation-log-ambiguous")
                try:
                    fields[part[1]] = json.loads(part[2]) if part[2].startswith('"') else part[2]
                except ValueError:
                    raise proof.EvidenceError("observation-log-format") from None
                pos = part.end()
            need(fields.get("level") in {"debug", "info", "warn", "error", "fatal"}
                 and type(fields.get("message")) is str, "observation-log-format")
            records.append(fields)
    return {"records": records, "lines": lines, "panic_count": panic_count, "unknown_type_count": unknown_count}


def _marker(records, message, **fields):
    return sum(row.get("level") == "info" and row.get("message") == message
               and row.get("app") == "rereply" and _MAIN.search(row.get("caller", "")) is not None
               and all(row.get(key) == value for key, value in fields.items()) for row in records)


def _own_journal_read(row):
    matched = (set(row) == {"seq", "time", "kind", "method", "route", "status"}
            and row["kind"] == "control" and row["method"] == "GET"
            and row["route"] == "journal" and type(row["status"]) is int and row["status"] == 200
            and type(row["time"]) is str and _DOCKER_TIME.fullmatch(row["time"] + " ") is not None)
    if matched:
        _instant(row["time"])
    return matched


def _completed():
    # Exit status of this completed observation operation, not a claimed web
    # process exit. The exact web process is required to remain running.
    return {"status": "completed", "exit_code": 0, "failure": None}


class OwnedObservation:
    def __init__(self, manager: processes.OwnedProcesses, handle: processes.ProcessHandle,
                 fixture: controller.sweep.Fixture, *, clock=time.monotonic, sleep=time.sleep, _old_boot=None):
        need(type(manager) is processes.OwnedProcesses and type(handle) is processes.ProcessHandle
             and handle.slot == "web" and handle.version in {"previous", "candidate"}
             and handle.phase == {"previous": "compat-prev-server", "candidate": "compat-cand-server"}[handle.version])
        need(type(fixture) is controller.sweep.Fixture)
        frame = {"version": 1, "operation": "readiness", "fixture": asdict(fixture)}
        controller.parse_request(controller._encode(frame))
        need(fixture.namespace == manager.owner.namespace)
        self.manager, self.handle, self.fixture = manager, handle, fixture
        self._clock, self._sleep = clock, sleep
        self._failed, self._booted, self._workers_done = False, False, False
        self._startup_attempted, self._startup_ready, self._period_attempted = False, False, False
        self._startup_summary, self._period_summary = None, None
        self._streams = (b"", b"")
        self._journal_rows = []
        self._journal_counts = None
        self._events = len(manager._events)
        self._old_boot = _old_boot
        self._old_boot_done = False
        if _old_boot is None:
            self._check()
        else:
            need(type(_old_boot) is tuple and len(_old_boot) == 3 and _old_boot[0] is _OLD_BOOT
                 and handle.version == "previous", "observation-old-boot-context")
            proof.artifact_name(manager.bindings["candidate"].source_sha, _old_boot[2])
            need(type(_old_boot[1]) is int and 1 <= _old_boot[1] < 10**16, "observation-old-boot-context")
            self._checked_state(running=None)

    @classmethod
    def for_old_boot(cls, manager, handle, fixture, *, run_id, run_attempt, clock=time.monotonic, sleep=time.sleep):
        """Only an already returned owned previous handle; never adopt a failed start."""
        return cls(manager, handle, fixture, clock=clock, sleep=sleep,
                   _old_boot=(_OLD_BOOT, run_id, run_attempt))

    def _check(self):
        return self._checked_state(running=True)

    def _checked_state(self, *, running):
        need(not self._failed and not self.manager._protocol_failed
             and self.manager._handles.get("web") is self.handle, "observation-handle-stale")
        self.manager._assert_all()
        row = self.manager._check("web", running=running)
        host, cfg, state = row["HostConfig"], row["Config"], row["State"]
        # json-file's empty options mean no rotation. local's default bounded
        # rotation is deliberately unsupported, as are remote/lossy drivers.
        need(host.get("LogConfig") in ({"Type": "json-file", "Config": {}},
                                        {"Type": "json-file", "Config": {"max-size": "-1"}})
             and cfg.get("Tty") is False, "observation-log-driver-unproven")
        need(not state.get("OOMKilled") and not state.get("Error"), "observation-process-failed")
        return row

    def _logs(self):
        self._check()
        result = self.manager._call(["logs", "--timestamps", self.handle.container_id], timeout=10)
        if result.returncode != 0:
            raise proof.InfrastructureFailure("observation-log-capture")
        current = (result.stdout, result.stderr)
        need(all(now.startswith(before) for now, before in zip(current, self._streams)), "observation-log-truncated")
        parsed = parse_logs(*current, started_at=self.handle.started_at)
        self._streams = current
        self._check()
        return parsed

    def _old_logs(self):
        """Terminal-capable reads retain every ordinary identity/log guard."""
        self._checked_state(running=None)
        result = self.manager._call(["logs", "--timestamps", self.handle.container_id], timeout=10)
        if result.returncode != 0:
            raise proof.InfrastructureFailure("observation-log-capture")
        current = result.stdout, result.stderr
        need(all(now.startswith(before) for now, before in zip(current, self._streams)), "observation-log-truncated")
        parsed = parse_logs(*current, started_at=self.handle.started_at)
        self._streams = current
        return parsed, self._checked_state(running=None)

    def _terminal_old(self, logs, row):
        state = row["State"]
        need(state["Running"] is False and state.get("Status") == "exited", "observation-terminal-state")
        finished = _instant(state.get("FinishedAt"))
        need(finished >= _instant(self.handle.started_at), "observation-terminal-time")
        # Require the recognized defect AFTER the actual start+RLS contract in
        # the same stream. Arbitrary mentions of panic in customer fields or a
        # defect before the contract cannot prove this narrow terminal case.
        panic = 0
        for raw in self._streams:
            start_seen = rls_seen = False
            for line in raw.splitlines(keepends=True):
                match = _DOCKER_TIME.fullmatch(line.decode().rstrip("\n\r"))
                need(match is not None and _instant(match[1]) <= finished, "observation-terminal-time")
                fields = parse_logs(line, b"", started_at=self.handle.started_at)["records"]
                if _marker(fields, "Starting ReReply server..."):
                    need(not start_seen and not rls_seen, "observation-terminal-contract")
                    start_seen = True
                if _marker(fields, "PostgreSQL tenant RLS verified", runtime_role="compat_runtime"):
                    need(start_seen and not rls_seen, "observation-terminal-contract")
                    rls_seen = True
                text = _SGR.sub("", match[2])
                # Source main.go also logs infrastructure failures at fatal
                # (Redis, realtime and storage). Runtime fatal error includes
                # OOM even when Docker's OOMKilled is false. Do not qualify
                # either, including a mixed window which also has a panic.
                need(not any(r.get("level") == "fatal" for r in fields)
                     and not re.match(r"fatal error:", text), "observation-terminal-fatal-unclassified")
                is_panic = bool(re.match(r"panic:\s+\S", text))
                if is_panic:
                    need(start_seen and rls_seen, "observation-terminal-before-contract")
                    panic += 1
        counts = self._period_counts(logs)
        previous = self.manager.bindings["previous"]
        binding = {"sha": self.manager.bindings["candidate"].source_sha,
                   "previous_source_sha": previous.source_sha, "previous_web_digest": previous.reference.rsplit("@", 1)[-1],
                   "run_id": self._old_boot[1], "run_attempt": self._old_boot[2], "owner_token": self.manager.owner.token,
                   "container_id": self.handle.container_id, "started_at": self.handle.started_at}
        value = {"state": "observed-semantic-failure", "phase": "old.boot", "binding": binding,
                 "terminal": {"running": False, "exit_code": state.get("ExitCode"), "finished_at": state.get("FinishedAt"),
                              "oom_killed": False, "daemon_error": False, "restart_count": row.get("RestartCount", 0)},
                 "logs": {"complete": True, "stdout_sha256": hashlib.sha256(self._streams[0]).hexdigest(),
                          "stderr_sha256": hashlib.sha256(self._streams[1]).hexdigest(),
                          "stdout_bytes": len(self._streams[0]), "stderr_bytes": len(self._streams[1]), "lines": logs["lines"],
                          "start_markers": counts["markers"]["start"], "rls_markers": counts["markers"]["rls"],
                          "fatal_markers": 0, "panic_markers": panic}}
        proof._terminal_old_boot(value, binding)
        return value

    def wait_old_boot(self, timeout=90):
        """Return a narrow terminal observation, or None for ordinary boot().

        No HTTP request occurs here. A stopped process without complete source
        contract and recognized terminal defect remains an evidence refusal.
        A semantic return consumes this path without resetting any failure latch.
        """
        try:
            need(self._old_boot is not None and not self._old_boot_done and not self._startup_attempted
                 and not self._booted and not self._period_attempted, "observation-old-boot-order")
            self._old_boot_done = self._startup_attempted = True
            need(type(timeout) in (int, float) and math.isfinite(timeout) and 0 < timeout <= 90,
                 "observation-startup-bounds")
            with self._readonly_budget(timeout) as remaining:
                for _ in range(math.ceil(timeout) + 1):
                    if remaining() <= 0: break
                    logs, row = self._old_logs()
                    self._startup_summary = self._period_counts(logs)
                    need(all(n <= 1 for n in self._startup_summary["markers"].values()), "observation-startup-ambiguous")
                    if row["State"]["Running"] is False:
                        # A second full read after termination checks the final
                        # state and catches truncation/drift during collection.
                        logs, final = self._old_logs()
                        need(final["State"] == row["State"], "observation-terminal-changed")
                        return self._terminal_old(logs, final)
                    if all(n == 1 for n in self._startup_summary["markers"].values()) and not any(
                            self._startup_summary[k] for k in ("panic_count", "unknown_type_count", "fatal_count", "stopped_or_error_count")):
                        self._startup_ready = True
                        return None
                    if remaining() <= 0: break
                    self._sleep(min(1, remaining()))
            if not self._startup_summary or not self._startup_summary["lines"]:
                raise proof.InfrastructureFailure("observation-startup-no-log")
            raise proof.EvidenceError("observation-startup-incomplete")
        except Exception as error:
            self._fail(error)

    def _controller(self, operation, **extra):
        self._check()
        raw = controller._encode({"version": 1, "operation": operation, "fixture": asdict(self.fixture), **extra})
        response = controller._parse(self.manager.controller(raw))
        self._check()
        self._events = len(self.manager._events)
        return response

    def _poison(self):
        self._failed = True
        self.manager._protocol_failed = True  # Exact known-resource cleanup remains available.

    def _fail(self, error):
        self._poison()
        if isinstance(error, (proof.EvidenceError, proof.InfrastructureFailure, controller.sweep.SemanticFailure)):
            raise error
        raise proof.InfrastructureFailure("observation-operation") from None

    @contextmanager
    def _readonly_budget(self, timeout):
        """Cap all inherited inspect/log I/O, restoring transport even on failure.

        The single manager is exclusively owned for this synchronous read
        window. No controller, process mutation, new ID or changed capture is
        admitted. Killing/reaping a timed-out CLI retains runtime's small
        bounded teardown overhead; timeout is not a hard scheduler guarantee.
        """
        owner = self.manager.owner
        original = owner.capture
        manager_events, owner_events = len(self.manager._events), len(owner._events)
        ids = frozenset((*owner._ids.values(), *self.manager._ids.values()))
        process_ids = dict(owner._process_ids)
        network = owner._network
        previous = self._clock()
        need(type(previous) in (int, float) and math.isfinite(previous) and previous >= 0,
             "observation-clock-invalid")
        deadline = previous + timeout

        def remaining():
            nonlocal previous
            now = self._clock()
            need(type(now) in (int, float) and math.isfinite(now) and now >= previous,
                 "observation-clock-invalid")
            previous = now
            need(len(self.manager._events) == manager_events and len(owner._events) == owner_events
                 and frozenset((*owner._ids.values(), *self.manager._ids.values())) == ids
                 and owner._process_ids == process_ids and owner._process_manager is self.manager
                 and owner._network == network and owner.capture is bounded, "observation-concurrent-action")
            return deadline - now

        def bounded(argv, **options):
            need(argv[:5] == [owner.binary, "--config", str(owner.config), "--context", "default"],
                 "observation-read-command-invalid")
            args = argv[5:]
            need(args == ["network", "inspect", network]
                 or (len(args) == 3 and args[:2] == ["container", "inspect"] and args[2] in ids)
                 or args == ["logs", "--timestamps", self.handle.container_id],
                 "observation-read-command-invalid")
            budget = remaining()
            if budget <= 0:
                raise proof.InfrastructureFailure("observation-read-timeout")
            options["timeout"] = min(options["timeout"], budget)
            value = original(argv, **options)
            if remaining() <= 0:
                raise proof.InfrastructureFailure("observation-read-timeout")
            return value

        owner.capture = bounded
        try:
            yield remaining
        finally:
            # Never overwrite a concurrently installed transport.
            need(owner.capture is bounded, "observation-capture-replaced")
            owner.capture = original

    @staticmethod
    def _period_counts(logs):
        records = logs["records"]
        return {"lines": logs["lines"], "panic_count": logs["panic_count"],
                "unknown_type_count": logs["unknown_type_count"],
                "fatal_count": sum(r["level"] == "fatal" for r in records),
                "stopped_or_error_count": sum(r.get("message") in
                    {"Worker error", "Worker stopped", "Channel outbox worker error", "Channel AI reply worker error",
                     "Threads credential refresh worker error", "Shutting down...", "Embedded workers disabled, run workers separately"}
                    for r in records),
                "markers": {
                    "start": _marker(records, "Starting ReReply server..."),
                    "rls": _marker(records, "PostgreSQL tenant RLS verified", runtime_role="compat_runtime"),
                    "listening": _marker(records, "Server listening", address="0.0.0.0:8080"),
                    "embedded_workers": _marker(records, "Embedded workers started", count="1"),
                    "worker": _marker(records, "Worker started", worker_num="1"),
                    "worker_starting": sum(r.get("level") == "info" and r.get("message") == "Worker starting"
                        and r.get("app") == "rereply" and _WORKER.search(r.get("caller", "")) is not None for r in records)}}

    @staticmethod
    def _healthy_counts(value):
        need(not any(value[k] for k in ("panic_count", "unknown_type_count", "fatal_count", "stopped_or_error_count")),
             "observation-period-failed")
        need(all(n <= 1 for n in value["markers"].values()), "observation-startup-ambiguous")

    def wait_startup(self, timeout=90, poll_interval=1):
        """Only bounded log/aliveness reads; success never means HTTP readiness.

        Server listening is emitted before ListenAndServe, so boot() must
        still perform its one separate readiness request. Unknown/missing
        source markers never fall back to invented FORCE-RLS evidence.
        """
        try:
            need(not self._startup_attempted and not self._booted and not self._period_attempted,
                 "observation-startup-order")
            self._startup_attempted = True
            need(type(timeout) in (int, float) and math.isfinite(timeout) and 0 < timeout <= 90
                 and type(poll_interval) in (int, float) and poll_interval == 1, "observation-startup-bounds")
            with self._readonly_budget(timeout) as remaining:
                for _ in range(math.ceil(timeout) + 1):
                    if remaining() <= 0:
                        break
                    logs = self._logs()
                    self._startup_summary = self._period_counts(logs)
                    self._healthy_counts(self._startup_summary)
                    if all(n == 1 for n in self._startup_summary["markers"].values()):
                        self._startup_ready = True
                        return {"execution": _completed(), "startup_markers_observed": True,
                                "http_readiness_checked": False, "lines": logs["lines"]}
                    budget = remaining()
                    if budget <= 0:
                        break
                    self._sleep(min(poll_interval, budget))
            if self._startup_summary is None or not self._startup_summary["lines"]:
                raise proof.InfrastructureFailure("observation-startup-no-log")
            raise proof.EvidenceError("observation-startup-incomplete")
        except Exception as error:
            self._fail(error)

    def finish_period(self, timeout=30):
        """Final whole-log/aliveness capture before stop, not a worker verdict.

        API actions may have happened since boot; every original log byte
        must still be present. Failures retain observed counts in evidence()
        and poison dependent work while exact owned cleanup remains possible.
        """
        try:
            need(self._startup_ready and self._booted and not self._period_attempted
                 and self.handle.version in {"previous", "candidate"}, "observation-period-order")
            self._period_attempted = True
            need(type(timeout) in (int, float) and math.isfinite(timeout) and 0 < timeout <= 30,
                 "observation-period-bounds")
            with self._readonly_budget(timeout):
                logs = self._logs()
                self._period_summary = self._period_counts(logs)
                self._healthy_counts(self._period_summary)
                need(all(n == 1 for n in self._period_summary["markers"].values()), "observation-startup-incomplete")
            return {"execution": _completed(), "alive": True, "whole_logs_observed": True,
                    "panic_count": self._period_summary["panic_count"],
                    "unknown_type_count": self._period_summary["unknown_type_count"],
                    "fatal_count": self._period_summary["fatal_count"],
                    "stopped_or_error_count": self._period_summary["stopped_or_error_count"]}
        except Exception as error:
            self._fail(error)

    def boot(self):
        """One readiness check after startup; no guessed polling/retry policy."""
        try:
            need(not self._booted, "observation-boot-already-used")
            logs = self._logs()
            if not logs["lines"]:
                raise proof.InfrastructureFailure("observation-boot-no-log")
            records = logs["records"]
            need(_marker(records, "Starting ReReply server...") == 1, "observation-start-unproven")
            need(_marker(records, "PostgreSQL tenant RLS verified", runtime_role="compat_runtime") == 1,
                 "observation-startup-control-required")
            response = self._controller("readiness")
            need(response["ready"] == {"web": True, "graph": True})
            logs = self._logs()
            need(_marker(logs["records"], "Starting ReReply server...") == 1
                 and _marker(logs["records"], "PostgreSQL tenant RLS verified", runtime_role="compat_runtime") == 1,
                 "observation-startup-ambiguous")
            self._booted = True
            return {"execution": _completed(), "first_log_observed": True,
                    "ready": not (logs["panic_count"] or logs["unknown_type_count"]
                                  or any(r["level"] == "fatal" for r in logs["records"])),
                    "startup_contract_observed": True, "rls_negative_control": None}
        except Exception as error:
            self._fail(error)

    def workers(self):
        """Quiet measured interval after sweeps, with no API calls except journal GETs."""
        try:
            need(self._booted and not self._workers_done and self.handle.version == "previous",
                 "observation-workers-order")
            logs = self._logs()
            records = logs["records"]
            need(_marker(records, "Embedded workers started", count="1") == 1
                 and _marker(records, "Worker started", worker_num="1") == 1
                 and sum(r.get("level") == "info" and r.get("message") == "Worker starting"
                         and r.get("app") == "rereply" and _WORKER.search(r.get("caller", "")) is not None
                         for r in records) == 1, "observation-worker-start-unproven")
            baseline = self._controller("journal", after=0, discard_baseline=True)["journal"]
            started = self._clock()
            need(type(started) in (int, float) and 0 <= started < float("inf"), "observation-clock-invalid")
            now = started
            for _ in range(13):
                if now - started >= 60:
                    break
                self._sleep(min(5, 60 - (now - started)))
                need(len(self.manager._events) == self._events, "observation-concurrent-action")
                logs = self._logs()
                fresh = self._clock()
                need(type(fresh) in (int, float) and now <= fresh < float("inf"), "observation-clock-invalid")
                now = fresh
            need(60 <= now - started <= 120, "observation-workers-window-incomplete")
            window = self._controller("journal", after=baseline["cursor"], discard_baseline=False)["journal"]
            logs = self._logs()
            elapsed = self._clock() - started
            need(60 <= elapsed <= 300, "observation-workers-window-incomplete")
            rows = window["entries"]
            own_reads = sum(_own_journal_read(row) for row in rows)
            # Previous final baseline read + (current page requests - 1).
            # Current final GET is appended AFTER its terminal cursor. Every
            # other observed row remains in unexpected_stub_entries.
            need(own_reads == window["requests"], "observation-journal-attribution-incomplete")
            self._journal_rows = rows
            self._journal_counts = {"baseline_requests": baseline["requests"],
                                    "window_requests": window["requests"], "observed_read_rows": own_reads,
                                    "after": baseline["cursor"], "cursor": window["cursor"]}
            need(not any(r.get("level") == "fatal" or r.get("message") in
                         {"Worker error", "Worker stopped", "Channel outbox worker error", "Channel AI reply worker error",
                          "Threads credential refresh worker error", "Shutting down...", "Embedded workers disabled, run workers separately"}
                         for r in logs["records"]), "observation-worker-stopped")
            self._workers_done = True
            return {"execution": _completed(), "observed_seconds": int(elapsed),
                    "panic_count": logs["panic_count"], "unknown_type_count": logs["unknown_type_count"],
                    "unexpected_stub_entries": len(rows) - own_reads}
        except Exception as error:
            self._fail(error)

    def evidence(self):
        """Sanitized collection metadata, not public compatibility result flags."""
        return {"stream_sha256": [hashlib.sha256(raw).hexdigest() for raw in self._streams],
                "stream_bytes": [len(raw) for raw in self._streams], "journal": self._journal_counts,
                "startup_wait": self._startup_summary, "period": self._period_summary,
                "failed": self._failed, "force_rls_control_implemented": False,
                "full_compatibility_proof": False}
