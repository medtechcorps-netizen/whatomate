"""Ordered first-baseline experiment; public execution is not enabled yet.

The initial local source/build admission adapter remains disabled pending
review. run_experiment() refuses before live work until deliberately enabled;
image labels supplied by a caller never substitute for source/build evidence.
The private engine tests use mock admission and transports, not live images.
No CLI, hosted workflow, publication or production mode is implemented here.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import asdict, dataclass
import math
from pathlib import Path
import re
import secrets
import sys
import time
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent))
import catalog
import controller
import fixture
import journal
import observation
import postgres
import processes
import proof
import runtime
import stack
import schema_change
import images as image_admission

_SHA = re.compile(r"[0-9a-f]{40}\Z")
_IMAGE_SEAL = object()
_READY_SQL = b"SELECT json_build_array(current_setting('server_version_num')::int/10000,current_setting('listen_addresses'),current_user);\n"


def need(value, code="experiment-input-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def _completed(code=0):
    need(type(code) is int and 0 <= code <= 255, "experiment-exit-invalid")
    return {"status": "completed", "exit_code": code, "failure": None}


@dataclass(frozen=True, repr=False)
class _Images:
    """Internal result of the source/build verifier, never a public input."""
    seal: object
    previous: processes.ImageBinding
    candidate: processes.ImageBinding
    controller: processes.ImageBinding
    graph: stack.GraphImage
    fixture: fixture.ExportImage


def _admit_images(repo, previous, candidate_sha, docker_binary, config_dir, receipts_dir, deadline):
    need(image_admission._READY_FOR_EXECUTION, "experiment-source-build-admission-unreviewed")
    value = image_admission._admit(repo, previous, candidate_sha, docker_binary, config_dir, receipts_dir, deadline)
    need(type(value) is image_admission._Admitted and value.seal is image_admission._SEAL,
         "experiment-source-build-unproven")
    return _Images(_IMAGE_SEAL, value.previous, value.candidate, value.controller, value.graph, value.fixture)


class _Budget:
    """One exclusive transport/deadline owner, with narrower timed windows."""
    def __init__(self, owner, *, clock, seconds=2400):
        self.owner, self.clock, self.seconds = owner, clock, seconds
        self.original = owner.capture
        self.transport = self._capture
        self.token = object()
        self.started = self.previous = self.deadline = None

    def _now(self):
        now = self.clock()
        need(type(now) in (int, float) and math.isfinite(now) and now >= 0
             and (self.previous is None or now >= self.previous), "experiment-clock-invalid")
        self.previous = now
        return now

    def remaining(self):
        need(self.owner.capture is self.transport
             and getattr(self.owner, "_experiment_budget", None) is self.token
             and self.owner.token == self.owner_token and self.owner.source_sha == self.source,
             "experiment-concurrent-transport")
        left = self.deadline - self._now()
        if left <= 0:
            raise proof.InfrastructureFailure("experiment-deadline")
        return left

    def _capture(self, argv, **options):
        need(argv[:5] == [self.owner.binary, "--config", str(self.owner.config), "--context", "default"],
             "experiment-transport-invalid")
        left = self.remaining()
        timeout = options.get("timeout")
        need(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0)
        options["timeout"] = min(timeout, left)
        value = self.original(argv, **options)
        self.remaining()  # Late completion must not become acknowledged work.
        return value

    def __enter__(self):
        need(getattr(self.owner, "_experiment_budget", None) is None
             and self.owner.capture is self.original, "experiment-budget-in-use")
        self.started = self._now()
        self.deadline = self.started + self.seconds
        self.owner_token, self.source = self.owner.token, self.owner.source_sha
        self.owner._experiment_budget = self.token
        self.owner.capture = self.transport
        return self

    def __exit__(self, exc_type, *unused):
        # Never overwrite a concurrently installed transport.
        need(self.owner.capture is self.transport
             and getattr(self.owner, "_experiment_budget", None) is self.token,
             "experiment-concurrent-transport")
        try:
            if exc_type is None:
                self.remaining()
        finally:
            self.owner.capture = self.original
            del self.owner._experiment_budget

    @contextmanager
    def window(self, seconds):
        need(type(seconds) in (int, float) and 0 < seconds <= self.seconds)
        previous = self.deadline
        self.deadline = min(previous, self._now() + seconds)
        try:
            self.remaining()
            yield
        finally:
            self.deadline = previous

    def observer_call(self, method):
        """Compose only the two existing native read-only observer budgets."""
        target = getattr(method, "__self__", None)
        name = getattr(method, "__name__", None)
        need(type(target) is observation.OwnedObservation
             and name in {"wait_startup", "finish_period"}
             and getattr(method, "__func__", None) is getattr(observation.OwnedObservation, name)
             and target.manager.owner is self.owner, "experiment-observer-bridge-invalid")
        timeout = min(90 if name == "wait_startup" else 30, self.remaining())
        self.owner.capture = self.original
        try:
            return method(timeout=timeout)
        finally:
            # The inner context restores exactly the transport it received,
            # including on an exception. A foreign replacement stays untouched.
            need(self.owner.capture is self.original, "experiment-concurrent-transport")
            self.owner.capture = self.transport
            self.remaining()


def _wait_pg(owner, budget, sleep):
    """Fixed read-only socket SELECT excludes the temporary entrypoint server."""
    pg = owner._handles["pg"]
    args = ["exec", "-i", "--env", "PGAPPNAME=compat-fixture-readiness", "--env",
            "PGOPTIONS=-c default_transaction_read_only=on", pg.container_id, "psql",
            "-X", "-w", "-qAt", "-h", "/var/run/postgresql", "-p", "5432", "-U", "postgres",
            "-d", "postgres", "-v", "ON_ERROR_STOP=1"]
    with budget.window(45):
        for _ in range(225):
            budget.remaining()
            for role, handle in owner._handles.items():
                owner._container_check(role, running=True, started_at=handle.started_at)
            result = owner.capture([owner.binary, "--config", str(owner.config), "--context", "default", *args],
                env=dict(owner.env), timeout=3, input_bytes=_READY_SQL,
                stdout_limit=runtime.MIB, stderr_limit=runtime.MIB)
            need(type(result) is runtime.Capture and type(result.returncode) is int,
                 "experiment-readiness-invalid")
            if result.returncode == 0 and not result.stderr:
                try:
                    value = runtime.common.loads_strict(result.stdout)
                except runtime.common.ReleaseError:
                    raise proof.EvidenceError("experiment-readiness-invalid") from None
                need(type(value) is list and len(value) == 3 and type(value[0]) is int
                     and value in ([17, "*", "postgres"], [17, "", "postgres"]), "experiment-readiness-invalid")
                if value == [17, "*", "postgres"]:
                    return
            sleep(min(0.2, budget.remaining()))
        raise proof.InfrastructureFailure("experiment-pg-not-ready")


class _Engine:
    def __init__(self, repo, previous, candidate_sha, run_id, run_attempt, classification, contract,
                 images, docker_binary, config_dir, *, clock=time.monotonic, sleep=time.sleep, overall=None):
        need(type(images) is _Images and images.seal is _IMAGE_SEAL,
             "experiment-source-build-unproven")
        need(all(type(value) is processes.ImageBinding for value in
                 (images.previous, images.candidate, images.controller))
             and type(images.graph) is stack.GraphImage and type(images.fixture) is fixture.ExportImage,
             "experiment-source-build-unproven")
        need(images.previous.source_sha == previous.sha and images.previous.reference == contract.previous_web
             and images.candidate.source_sha == candidate_sha, "experiment-image-binding")
        self.previous, self.sha, self.run_id, self.attempt = previous, candidate_sha, run_id, run_attempt
        self.classification, self.contract, self.images = classification, contract, images
        self.clock, self.sleep = clock, sleep
        self.owner = stack.OwnedStack(docker_binary, config_dir, images.graph.source_sha, images.graph)
        need(overall is None or type(overall) is image_admission._Deadline, "experiment-overall-invalid")
        self.overall = overall
        self.seconds = 2400 if overall is None else overall.experiment_seconds()
        if overall is not None:
            self.owner.capture = overall.capture
        self._original_capture = self.owner.capture
        self._owner_token, self._owner_source = self.owner.token, self.owner.source_sha
        self.inputs = processes.ConfigInputs(*(secrets.token_hex(32) for _ in range(4)), str(uuid.uuid4()))
        self.exporter = self.manager = None
        self._attempted = False

    def _controller(self, operation, **extra):
        request = controller._encode({"version": 1, "operation": operation,
                                      "fixture": asdict(self.provisioned.fixture), **extra})
        # OwnedProcesses already checks exit/closed response schema and keeps
        # the independent completion receipts. Do not infer success from text.
        return controller._parse(self.manager.controller(request))

    def _sweep(self, operation, **extra):
        response = self._controller(operation, **extra)
        return {"execution": _completed(), "cases": controller._flags(response["cases"])}, response.get("record")

    def _settle(self):
        with self.budget.window(120):
            for index in range(12):
                self.budget.remaining()
                snapshot = self.ledger.capture()
                self.budget.remaining()
                if snapshot.pending == 1:
                    return snapshot  # Only the known deferred final journal GET.
                if index < 11:
                    self.sleep(min(1, self.budget.remaining()))
            raise proof.EvidenceError("experiment-status-settle-incomplete")

    def _server(self, version):
        handle = self.manager.start_web(version, self.inputs)
        observer = observation.OwnedObservation(self.manager, handle, self.provisioned.fixture,
                                                clock=self.clock, sleep=self.sleep)
        self.budget.observer_call(observer.wait_startup)
        boot = observer.boot()
        return handle, observer, boot

    def _stop(self, handle, observer):
        self.budget.observer_call(observer.finish_period)
        self.manager.remove(handle)
        need(not self.manager._ids, "experiment-process-still-present")
        return {"execution": _completed(), "passed": True}  # Actual exact removal+absence above.

    def _migration(self, application, *, original_columns):
        phase = self.pg.begin_phase(application)
        handle = self.manager.start_migration(application, self.inputs)
        code = self.manager.wait_migration(handle)
        self.manager.remove(handle)
        captured = self.pg.finish_phase(phase)
        observed, tables = self.catalog.after_phase(self.inputs, original_columns=original_columns)
        return {"execution": _completed(code), **captured,
                "catalog_sha256": observed.sha256, "tables": tables.public()}

    def _measure(self):
        self.owner.start()
        _wait_pg(self.owner, self.budget, self.sleep)
        self.exporter = fixture.OwnedFixture(self.owner, self.images.fixture)
        self.provisioned = self.exporter.run(self.inputs, password=secrets.token_hex(32))
        need(self.provisioned.owner_token == self.owner.token
             and self.provisioned.pg_container_id == self.owner._ids["pg"]
             and self.provisioned.pg_started_at == self.owner._handles["pg"].started_at
             and self.provisioned.fixture.organization_id == self.inputs.organization_id
             and self.provisioned.catalog_sha256 == self.contract.target_sha256,
             "experiment-fixture-binding")
        self.exporter.close()
        self.manager = processes.OwnedProcesses(self.owner, self.images.previous, self.images.candidate, self.images.controller)
        # Indirection keeps this collector inside the current bounded transport.
        self.pg = postgres.SyntheticPostgres(self.owner.binary, self.owner.config, self.owner._ids["pg"],
            self.owner.token, "compat_runtime", capture=lambda *a, **kw: self.owner.capture(*a, **kw))
        self.pg.install_probe()
        self.catalog = catalog.OwnedCatalog(self.manager, self.pg, self.contract)
        self.ledger = journal.JournalLedger(self.manager, self.provisioned)
        self.ledger.capture()  # No application has booted; no discarded history.

        handle, observed, previous_boot = self._server("previous")
        self._controller("setup", business_account_id=self.provisioned.business_account_id)
        seed, _ = self._sweep("write", nonce=secrets.token_hex(16))
        need(proof._sweep(seed), "experiment-previous-seed-failed")
        self._settle()
        self._stop(handle, observed)
        self.ledger.capture()
        baseline, before = self.catalog.initial(self.inputs)
        passes = [self._migration("compat-fwd", original_columns=True) for _ in range(2)]

        handle, observed, candidate_boot = self._server("candidate")
        candidate_write, candidate_record = self._sweep("write", nonce=secrets.token_hex(16))
        self._settle()
        self._stop(handle, observed)
        self.ledger.capture()
        d0_catalog, d0 = self.catalog.after_phase(self.inputs, original_columns=False)
        # This method's complete RRRO table capture checked actual state: zero
        # other clients, fixed role privileges and complete original inventory.
        quiet = {"candidate_stopped": not self.manager._ids, "remaining_client_sessions": 0,
                 "catalog_sha256": d0_catalog.sha256, "tables": d0.public()}
        rw = self._migration("compat-n1-rw", original_columns=False)
        ro = self._migration("compat-n1-ro", original_columns=False)
        control = self.pg.read_only_control()

        handle, observed, old_boot = self._server("previous")
        old_read, _ = self._sweep("read", record=candidate_record)  # Before old writes.
        old_write, old_record = self._sweep("write", nonce=secrets.token_hex(16))
        self._settle()
        # workers() obtains its own actual baseline and updates its event
        # latch. No manual reset; no external call within its exclusive wait.
        workers = observed.workers()
        self.ledger.capture()
        stopped = self._stop(handle, observed)
        self.old_stop = self.ledger.capture()
        need(self.old_stop.pending == 1, "experiment-old-period-incomplete")

        handle, observed, reverse_boot = self._server("candidate")
        reverse, _ = self._sweep("read", record=old_record)
        self._stop(handle, observed)
        sealed = self.ledger.seal()
        need(sealed.sealed and sealed.pending == 1, "experiment-journal-unsealed")
        final = self.catalog.observe(self.inputs)
        private = {"identity": {"sha": self.sha, "previous_release": self.previous.release,
                    "previous_web_digest": self.previous.images["web"], "run_id": self.run_id, "run_attempt": self.attempt},
            "classification": {"kind": self.classification.kind, "dormant": self.classification.dormant},
            "previous_boot": previous_boot, "expected_catalog_sha256": self.contract.target_sha256,
            "expected_preexisting_tables": list(self.contract.expected_preexisting_tables), "before_tables": before.public(),
            "forward": {"passes": passes, "allowed_statement_hashes": [], "allowed_marker_hashes": [],
                "candidate_ready": {"execution": candidate_boot["execution"], "passed": proof._boot(candidate_boot)},
                "write_sweep": candidate_write}, "quiescence": quiet,
            "old_migrate": {"rw": rw, "ro": ro, "negative_control": control},
            "old_server": {"boot": old_boot, "read_sweep": old_read, "write_sweep": old_write,
                "workers": workers, "stopped": stopped,
                "candidate_ready": {"execution": reverse_boot["execution"], "passed": proof._boot(reverse_boot)},
                "reverse_sweep": reverse}, "final_catalog_sha256": final.sha256}
        return proof.evaluate_proof(private)

    def _cleanup_transport(self):
        # A foreign transport or unfinished budget is not cleanup authority.
        # Leave it untouched, with resources unresolved, rather than invoke it.
        need(self.owner.capture is self._original_capture
             and not hasattr(self.owner, "_experiment_budget")
             and self.owner.token == self._owner_token and self.owner.source_sha == self._owner_source,
             "experiment-cleanup-transport-unowned")

    def _cleanup(self):
        self._cleanup_transport()
        try:
            if self.overall is None:
                self._close_owned()
                self._cleanup_transport()
                self.owner.cleanup()
            else:
                # Cleanup consumes the reserved part of the SAME2400s clock.
                # The global capture also clamps every actual command.
                seconds = min(image_admission._CLEANUP, self.overall.remaining())
                with _Budget(self.owner, clock=self.clock, seconds=seconds) as cleanup:
                    self._close_owned()
                    cleanup.remaining()  # Do not invoke a replacement capture.
                    self.owner.cleanup()
        except proof.EvidenceError:
            raise
        except Exception:
            raise proof.InfrastructureFailure("experiment-cleanup") from None

    def _close_owned(self):
        if self.manager is not None and not self.manager._closed:
            self.manager.close()
        elif self.exporter is not None and not self.exporter._closed:
            self.exporter.close()

    def run(self):
        need(not self._attempted, "experiment-already-attempted")
        self._attempted = True
        try:
            self._cleanup_transport()
            with _Budget(self.owner, clock=self.clock, seconds=self.seconds) as self.budget:
                result = self._measure()
        except Exception as error:
            # Budget restoration precedes cleanup, whose existing fixed command
            # bounds and unknown-outcome latches remain authoritative.
            self._cleanup()
            if isinstance(error, proof.EvidenceError):
                raise
            raise proof.InfrastructureFailure("experiment-operation") from None
        self._cleanup()
        if self.overall is not None:
            self.overall.remaining()
        raw = proof.result_bytes(result)  # Never published before cleanup.
        if self.overall is not None:
            self.overall.remaining()
        return raw


def run_experiment(repo: Path, candidate_sha: str, run_id: int, run_attempt: int,
                   release_client, *, docker_binary: str, config_dir: Path,
                   image_receipts_dir: Path | None = None, rollback_target=None):
    """Validate immutable inputs; disabled pending first-local admission review.

    No arbitrary images, observations, successful flags, phase dictionaries,
    command callbacks, SQL or private fixture inputs are accepted here.
    """
    deadline = image_admission._Deadline()
    need(isinstance(repo, Path) and repo.is_dir() and isinstance(config_dir, Path))
    need(type(candidate_sha) is str and _SHA.fullmatch(candidate_sha) is not None)
    proof.artifact_name(candidate_sha, run_attempt)
    need(type(run_id) is int and 1 <= run_id < 10**16)
    if rollback_target is not None:
        proof.validate_rollback_target(rollback_target)
    # Review must enable the complete adapter deliberately; accidental public
    # calls cannot launch a build while this slice is still under review.
    need(image_admission._READY_FOR_EXECUTION, "experiment-source-build-admission-unreviewed")
    need(isinstance(image_receipts_dir, Path), "experiment-receipts-required")
    with image_admission._verification_transport(release_client, deadline):
        previous = runtime.verified_previous(release_client, repo, rollback_target=rollback_target)
        classification = schema_change.classify(repo, previous.sha, candidate_sha, mode="forward")
        need(not classification.reasons and ((classification.kind == "none" and not classification.dormant)
             or (classification.kind == "data" and classification.dormant)), "experiment-classification-unsupported")
        contract = catalog.CatalogContract(repo, previous, candidate_sha, capture=deadline.capture)
    admitted = _admit_images(repo, previous, candidate_sha, docker_binary, config_dir, image_receipts_dir, deadline)
    return _Engine(repo, previous, candidate_sha, run_id, run_attempt, classification, contract,
                   admitted, docker_binary, config_dir, clock=deadline.clock, overall=deadline).run()
