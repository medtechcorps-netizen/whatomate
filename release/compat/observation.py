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
import hashlib
import json
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
_FIELD = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)=("(?:[^"\\]|\\["\\nrtbf/]|\\u[0-9a-fA-F]{4})*"|[^\s"]+)(?: |$)')
_UNKNOWN = re.compile(r"\bunknown(?:[- ]job)?[- ]type\b", re.IGNORECASE)
_PANIC = re.compile(r"\bpanic(?:ked)?\b", re.IGNORECASE)
_MAIN = re.compile(r"(?:^|/)cmd/whatomate/main\.go:[1-9][0-9]*\Z")
_WORKER = re.compile(r"(?:^|/)internal/worker/worker\.go:[1-9][0-9]*\Z")


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
                 fixture: controller.sweep.Fixture, *, clock=time.monotonic, sleep=time.sleep):
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
        self._streams = (b"", b"")
        self._journal_rows = []
        self._journal_counts = None
        self._events = len(manager._events)
        self._check()

    def _check(self):
        need(not self._failed and not self.manager._protocol_failed
             and self.manager._handles.get("web") is self.handle, "observation-handle-stale")
        self.manager._assert_all()
        row = self.manager._check("web", running=True)
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
                "failed": self._failed, "force_rls_control_implemented": False,
                "full_compatibility_proof": False}
