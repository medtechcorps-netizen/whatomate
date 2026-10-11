"""Synthetic Docker/controller byte fixtures only; no live processes or network."""
from dataclasses import replace
import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import observation as o
import test_processes as tp
import test_stack
import test_controller

TIME = "2026-10-11T00:00:02.000000000Z"


def log(message, *, fields="", caller="github.com/shridarpatil/whatomate/cmd/whatomate/main.go:678", level="info", color=False):
    raw = f'timestamp=2026-10-11 00:00:02 level={level} message={json.dumps(message)} caller={caller} app=rereply'
    if fields:
        raw += " " + fields
    if color:
        raw = raw.replace("level", "\x1b[36mlevel\x1b[0m").replace("message", "\x1b[36mmessage\x1b[0m")
    return (TIME + " " + raw + "\n").encode()


def startup():
    return (log("Starting ReReply server...", color=True)
            + log("PostgreSQL tenant RLS verified", fields="runtime_role=compat_runtime")
            + log("Worker started", fields="worker_num=1")
            + log("Worker starting", caller="github.com/shridarpatil/whatomate/internal/worker/worker.go:103")
            + log("Embedded workers started", fields="count=1"))


def readrow(seq):
    return {"seq": seq, "time": TIME, "kind": "control", "method": "GET", "route": "journal", "status": 200}


class Clock:
    def __init__(self): self.now = 10.0
    def __call__(self): return self.now
    def sleep(self, seconds): self.now += seconds


class World(tp.World):
    def __init__(self):
        super().__init__()
        self.raw = startup()
        self.log_calls = 0
        self.mutate_log = None
        self.extra_rows = []
        self.window_mutation = None

    def capture(self, argv, **options):
        args = argv[5:]
        if args[:2] == ["logs", "--timestamps"]:
            self.calls.append((list(args), options))
            self.log_calls += 1
            if self.mutate_log:
                self.mutate_log(self)
            return o.processes.runtime.Capture(0, b"", self.raw)
        if args[:4] == ["container", "start", "--attach", "--interactive"]:
            request = json.loads(options["input_bytes"])
            if request["operation"] == "journal":
                baseline = request["discard_baseline"]
                rows = [readrow(11)] if baseline else [readrow(12), *self.extra_rows]
                value = {"after": request["after"], "cursor": rows[-1]["seq"],
                         "discard_baseline": baseline, "entries": rows, "requests": 1}
                if not baseline and self.window_mutation:
                    self.window_mutation(value)
                self.response = json.dumps({"version": 1, "status": "ok", "operation": "journal", "journal": value}).encode()
        result = super().capture(argv, **options)
        if args[:2] == ["container", "create"] and result.returncode == 0:
            row = self.containers[result.stdout.decode()]
            row["HostConfig"]["LogConfig"] = {"Type": "json-file", "Config": {}}
            row["Config"]["Tty"] = False
        return result


class ObservationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.world = World()
        self.owner = o.processes.stack.OwnedStack("docker", Path(self.tmp.name), test_stack.SOURCE,
            o.processes.stack.GraphImage(test_stack.GRAPH, test_stack.SOURCE), token=test_stack.TOKEN,
            capture=self.world.capture)
        self.owner.start()
        self.manager = o.processes.OwnedProcesses(self.owner,
            o.processes.ImageBinding(tp.PREV, "sha256:" + "4" * 64, "d" * 40, tp.RECIPE),
            o.processes.ImageBinding(tp.CAND, tp.CAND, "e" * 40, tp.RECIPE),
            o.processes.ImageBinding(tp.CONTROL, tp.CONTROL, "f" * 40, tp.RECIPE))
        self.handle = self.manager.start_web("previous", tp.INPUTS)
        self.fixture = replace(test_controller.fixture(), namespace=self.owner.namespace,
            product_origin=self.owner.planned_web_origin, stub_origin=self.owner.graph_origin,
            account_name=self.owner.namespace + "-wa", email=self.owner.namespace + "-client@example.test")
        self.clock = Clock()

    def observer(self, **kwargs):
        return o.OwnedObservation(self.manager, self.handle, self.fixture,
                                  clock=self.clock, sleep=kwargs.pop("sleep", self.clock.sleep), **kwargs)

    def test_positive_owned_boot_and_actual_sixty_second_window(self):
        observed = self.observer()
        boot = observed.boot()
        self.assertTrue(o.proof._boot(boot))
        worker = observed.workers()
        self.assertEqual(worker, {"execution": o._completed(), "observed_seconds": 60,
            "panic_count": 0, "unknown_type_count": 0, "unexpected_stub_entries": 0})
        self.assertEqual(observed.evidence()["journal"], {"after": 11, "cursor": 12,
            "baseline_requests": 1, "window_requests": 1, "observed_read_rows": 1})
        self.assertEqual(observed._journal_rows, [readrow(12)])
        logs = [args for args, _ in self.world.calls if args[0] == "logs"]
        self.assertTrue(all(args == ["logs", "--timestamps", self.handle.container_id] for args in logs))
        self.assertEqual(len(logs), 16)
        self.assertNotIn(self.fixture.password, json.dumps(observed.evidence()))
        self.assertFalse(observed.evidence()["full_compatibility_proof"])
        self.manager.close(); self.owner.cleanup()

    def test_start_marker_wrong_role_missing_spoof_or_duplicate_refuses_no_positive(self):
        originals = [startup().replace(b"runtime_role=compat_runtime", b"runtime_role=postgres"),
            startup().replace(b"PostgreSQL tenant RLS verified", b"PostgreSQL platform compliance guards verified"),
            startup().replace(b"cmd/whatomate/main.go", b"internal/handlers/customer.go"),
            startup() + log("PostgreSQL tenant RLS verified", fields="runtime_role=compat_runtime")]
        for raw in originals:
            self.world.raw = raw
            observed = self.observer()
            with self.assertRaises(o.proof.EvidenceError): observed.boot()
            self.assertTrue(self.manager._protocol_failed)
            self.manager._protocol_failed = False  # New independent negative fixture.
        self.assertFalse(any(args[:4] == ["container", "start", "--attach", "--interactive"] for args, _ in self.world.calls))

    def test_missing_first_log_is_infrastructure_and_no_fallback(self):
        self.world.raw = b""
        with self.assertRaises(o.proof.InfrastructureFailure): self.observer().boot()
        self.manager.close(); self.owner.cleanup()  # Failure latch still permits owned cleanup.

    def test_logs_before_observation_and_between_reads_preserve_failure_counts(self):
        self.world.raw += log("Panic recovered", level="error")
        observed = self.observer(); self.assertFalse(observed.boot()["ready"])
        self.world.raw += log("Failed to process message", level="error", fields='error="unknown job type: synthetic"')
        value = observed.workers()
        self.assertEqual((value["panic_count"], value["unknown_type_count"]), (1, 1))

    def test_worker_requires_real_start_markers_and_no_false_elapsed(self):
        self.world.raw = startup().replace(b'count=1', b'count=0')
        observed = self.observer(); observed.boot()
        with self.assertRaisesRegex(o.proof.EvidenceError, "worker-start"): observed.workers()
        self.assertEqual(self.clock.now, 10)

    def test_truncated_whole_log_poisons_and_cleanup_remains_available(self):
        observed = self.observer(); observed.boot()
        self.world.raw = self.world.raw.split(b"\n", 1)[1]
        with self.assertRaisesRegex(o.proof.EvidenceError, "truncated"): observed.workers()
        with self.assertRaises(o.proof.EvidenceError): self.manager.controller(test_controller.raw(test_controller.request("readiness")))
        self.manager.close(); self.owner.cleanup()

    def test_restart_exit_identity_and_rotation_are_never_favorable(self):
        row = self.world.containers[self.handle.container_id]
        original = copy.deepcopy(row)
        variants = [("RestartCount", 1), ("State", dict(row["State"], Running=False)),
                    ("State", dict(row["State"], StartedAt="2026-10-11T00:00:03Z")),
                    ("HostConfig", dict(row["HostConfig"], LogConfig={"Type": "local", "Config": {}})),
                    ("HostConfig", dict(row["HostConfig"], LogConfig={"Type": "json-file", "Config": {"max-size": "1m"}})),
                    ("Config", dict(row["Config"], Tty=True))]
        for key, value in variants:
            self.world.containers[self.handle.container_id] = copy.deepcopy(original)
            self.world.containers[self.handle.container_id][key] = value
            with self.assertRaises(o.proof.EvidenceError): self.observer()
        self.world.containers[self.handle.container_id] = original
        with self.assertRaises(o.proof.EvidenceError):
            o.OwnedObservation(self.manager, replace(self.handle), self.fixture)

    def test_short_frozen_or_reversed_clock_and_concurrent_action_refuse(self):
        for action in (lambda n: None, lambda n: setattr(self.clock, "now", self.clock.now - 1),
                       lambda n: self.manager._events.append({"operation": "foreign"})):
            self.world.response = None
            observed = self.observer(sleep=action); observed.boot()
            with self.assertRaises(o.proof.EvidenceError): observed.workers()
            self.manager._protocol_failed = False
            self.clock.now = 10
        self.assertFalse(any(args[0] == "logs" and "--tail" in args for args, _ in self.world.calls))

    def test_unexpected_clock_exception_is_fixed_and_never_leaks(self):
        def failure(_seconds): raise RuntimeError("secret fixture password hostname")
        observed = self.observer(sleep=failure); observed.boot()
        with self.assertRaisesRegex(o.proof.InfrastructureFailure, "^observation-operation$"):
            observed.workers()
        self.assertTrue(observed.evidence()["failed"])

    def test_go_panic_stack_tabs_remain_private_failure_evidence(self):
        raw = startup() + (TIME + " panic: synthetic\n" + TIME + " \t/app/server.go:42\n").encode()
        parsed = o.parse_logs(b"", raw, started_at=self.handle.started_at)
        self.assertEqual(parsed["panic_count"], 1)

    def test_unexpected_stub_write_is_counted_not_ignored(self):
        self.world.extra_rows = [{"seq": 13, "time": TIME, "kind": "graph", "method": "POST", "route": "messages", "status": 200}]
        observed = self.observer(); observed.boot()
        self.assertEqual(observed.workers()["unexpected_stub_entries"], 1)
        self.assertEqual(len(observed._journal_rows), 2)

    def test_extra_or_missing_control_reads_and_invalid_cursor_refuse(self):
        variants = [lambda v: v.update(entries=[], cursor=v["after"]),
                    lambda v: v["entries"].append(readrow(13)) or v.update(cursor=13),
                    lambda v: v.update(after=0),
                    lambda v: v["entries"][0].update(seq=13) or v.update(cursor=13)]
        for change in variants:
            self.world.response = None
            self.world.window_mutation = change
            observed = self.observer(); observed.boot()
            with self.assertRaises(o.proof.EvidenceError): observed.workers()
            self.manager._protocol_failed = False
            self.clock.now = 10

    def test_log_controls_utf8_incomplete_and_false_marker_in_field(self):
        bad = [b"secret\n", (TIME + " \x1b[2Jsecret\n").encode(), b"\xff\n", startup()[:-1],
               log("safe", fields='message="PostgreSQL tenant RLS verified"')]
        for raw in bad:
            with self.assertRaises(o.proof.EvidenceError): o.parse_logs(b"", raw, started_at=self.handle.started_at)
        raw = log("ordinary", fields='error="PostgreSQL tenant RLS verified" runtime_role=compat_runtime')
        value = o.parse_logs(b"", raw, started_at=self.handle.started_at)
        self.assertEqual(o._marker(value["records"], "PostgreSQL tenant RLS verified", runtime_role="compat_runtime"), 0)

    def test_worker_cannot_reuse_completed_window_or_run_before_boot(self):
        observed = self.observer()
        with self.assertRaises(o.proof.EvidenceError): observed.workers()
        self.manager._protocol_failed = False
        observed = self.observer(); observed.boot(); observed.workers()
        with self.assertRaises(o.proof.EvidenceError): observed.workers()


if __name__ == "__main__": unittest.main()
