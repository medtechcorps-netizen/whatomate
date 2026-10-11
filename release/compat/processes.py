"""Fixed owned web/migration/controller processes, not a compatibility runner.

Callers must first verify release/image attestations or the exact candidate
build. Image source/recipe inputs are required bindings, not a replacement for
that trust policy. All images must exist: no pulls, builds or host files are
used. No bootstrap, fixture provisioning, readiness retry, SQL phase framing,
worker verdict or public proof is supplied here. Raw controller responses are
private in-memory protocol data. No command executes at import.
"""
from __future__ import annotations

from dataclasses import dataclass
import json
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof
import runtime
import stack

_HEX = re.compile(r"[0-9a-f]{64}\Z")
_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\Z")
_SOURCE_URL = "https://github.com/medtechcorps-netizen/whatomate"
_WRITE_CONFIG = ('umask 077; test ! -e /compat/private; mkdir -m 700 /compat/private; '
                 'cat > /compat/private/config.toml; test -s /compat/private/config.toml; '
                 'mkdir -m 700 /compat/uploads /compat/audio; : > /compat/ready')
_WAIT_CONFIG = ('i=0; while [ ! -f /compat/ready ]; do i=$((i+1)); '
                '[ "$i" -lt 300 ] || exit 75; sleep 0.1; done; exec /app/rereply ')
_PHASES = {"compat-fwd": "candidate", "compat-n1-rw": "previous", "compat-n1-ro": "previous"}


def need(ok, code="process-input-invalid"):
    if not ok:
        raise proof.EvidenceError(code)


@dataclass(frozen=True)
class ImageBinding:
    reference: str
    image_id: str
    source_sha: str
    recipe_sha256: str

    def __post_init__(self):
        need(type(self.image_id) is str and stack._IMAGE.fullmatch(self.image_id) is not None)
        need(type(self.reference) is str and (self.reference == self.image_id
             or runtime._WEB.fullmatch(self.reference) is not None))
        need(type(self.source_sha) is str and stack._SHA.fullmatch(self.source_sha) is not None)
        need(type(self.recipe_sha256) is str and _HEX.fullmatch(self.recipe_sha256) is not None)


@dataclass(frozen=True, repr=False)
class ConfigInputs:
    """Generated fixture material only; roles/database must already exist."""
    runtime_password: str
    owner_password: str
    jwt_secret: str
    encryption_key: str
    organization_id: str

    def __post_init__(self):
        need(all(type(v) is str and _HEX.fullmatch(v) is not None for v in
                 (self.runtime_password, self.owner_password, self.jwt_secret, self.encryption_key)))
        need(type(self.organization_id) is str and _UUID.fullmatch(self.organization_id) is not None)


@dataclass(frozen=True)
class ProcessHandle:
    slot: str
    version: str
    phase: str
    container_id: str
    image_id: str
    started_at: str


def _configuration(owner, inputs, phase):
    need(type(inputs) is ConfigInputs)
    need(phase in {"compat-prev-server", "compat-cand-server", "compat-control", *_PHASES})
    host = owner.namespace + "-pg"
    def url(user, password):
        value = f"postgres://{user}:{password}@{host}:5432/compat?sslmode=disable&application_name={phase}"
        return value + ("&options=-c%20default_transaction_read_only%3Don" if phase == "compat-n1-ro" else "")
    if phase == "compat-control":
        # Catalog CLI reads only the runtime connection; no owner credential.
        # Its collector rejects DSN options and enforces read-only at startup.
        return ('[app]\nenvironment = "test"\n[database]\nurl = ' + json.dumps(url("compat_runtime", inputs.runtime_password))
                + '\nruntime_role = "compat_runtime"\nrls_enabled = true\n').encode()
    sections = {
        "app": {"name": "ReReply", "environment": "test", "encryption_key": inputs.encryption_key},
        "server": {"host": "0.0.0.0", "port": 8080},
        "database": {"url": url("compat_runtime", inputs.runtime_password),
                     "runtime_role": "compat_runtime", "rls_enabled": True},
        "redis": {"url": "redis://" + owner.namespace + "-redis:6379/0"},
        "jwt": {"secret": inputs.jwt_secret},
        "storage": {"type": "local", "local_path": "/compat/uploads"},
        "calling": {"audio_dir": "/compat/audio"},
        "whatsapp": {"base_url": "http://" + owner.namespace + "-graph:8090",
                     "app_id": owner.fixture_inputs.graph_app_id,
                     "app_secret": owner.fixture_inputs.graph_app_secret},
        "legacy_whatsapp_reply": {"enabled": True, "allowed_organization_ids": inputs.organization_id},
        "ai": {"qwen_base_url": "http://127.0.0.1:9"},
        "google_search_console": {"auth_url": "http://127.0.0.1:9", "token_url": "http://127.0.0.1:9",
                                  "api_base_url": "http://127.0.0.1:9"},
        "meta_messenger_onboarding": {"enabled": False, "graph_base_url": "http://127.0.0.1:9"},
        "meta_instagram_onboarding": {"enabled": False, "authorization_base_url": "http://127.0.0.1:9",
                                      "token_base_url": "http://127.0.0.1:9", "graph_base_url": "http://127.0.0.1:9"},
        "threads_managed": {"enabled": False},
    }
    if phase in _PHASES:
        sections["database"]["migration_url"] = url("compat_owner", inputs.owner_password)
    text = "".join("[" + name + "]\n" + "".join(key + " = " + json.dumps(value) + "\n"
                   for key, value in values.items()) for name, values in sections.items())
    raw = text.encode("utf-8")
    need(len(raw) <= 16384)
    return raw


class OwnedProcesses:
    """One manager per live OwnedStack; every admitted peer has an exact ID.

    Web startup returns a running process/config-delivery handle, never a
    readiness or startup-contract verdict. Migration exit codes are data;
    callers must bracket execution with SyntheticPostgres.begin/finish_phase.
    A process or controller ambiguity poisons further actions without retry.
    """
    def __init__(self, owner: stack.OwnedStack, previous: ImageBinding,
                 candidate: ImageBinding, controller: ImageBinding, *,
                 catalog_control: ImageBinding | None = None):
        need(type(owner) is stack.OwnedStack and not owner._closed and not owner._ambiguous)
        need(set(owner._handles) == {"pg", "redis", "graph"} and owner._process_manager is None)
        need(all(type(v) is ImageBinding for v in (previous, candidate, controller)))
        need(runtime._WEB.fullmatch(previous.reference) is not None)
        need(controller.reference == controller.image_id)
        self.owner, self.bindings = owner, {"previous": previous, "candidate": candidate, "controller": controller}
        if catalog_control is not None:
            need(type(catalog_control) is ImageBinding
                 and runtime._WEB.fullmatch(candidate.reference) is not None
                 and catalog_control.reference == catalog_control.image_id
                 and catalog_control.source_sha == controller.source_sha == owner.source_sha,
                 "process-catalog-control-invalid")
            self.bindings["catalog-control"] = catalog_control
        self._network = owner._network
        self._ids, self._specs, self._handles, self._events = {}, {}, {}, []
        self._images, self._ambiguous, self._closed = {}, False, False
        self._protocol_failed = False
        self._controller_receipts = []  # Immutable private frames, never evidence().
        self._assert_all()
        for role, binding in self.bindings.items():
            image = stack._json(self._read(["image", "inspect", binding.reference]))
            self._validate_image(role, binding, image)
            self._images[role] = image
        owner._process_manager = self

    @property
    def catalog_binding(self):
        return self.bindings["catalog-control" if "catalog-control" in self.bindings else "candidate"]

    @property
    def evidence_source_sha(self):
        # The producer/control source H is distinct from the tested live image L.
        return self.catalog_binding.source_sha

    def _validate_image(self, role, binding, row):
        cfg = row.get("Config", {})
        need(row.get("Id") == binding.image_id and row.get("Os") == "linux"
             and row.get("Architecture") == "amd64" and type(cfg) is dict, "process-image-invalid")
        labels = cfg.get("Labels") or {}
        need(type(labels) is dict and labels.get(stack.SOURCE_LABEL) == binding.source_sha,
             "process-source-invalid")
        if runtime._WEB.fullmatch(binding.reference):
            wanted = binding.reference.split("@", 1)[1]
            need(runtime.common.IMAGE_REPOSITORY["web"] + "@" + wanted in (row.get("RepoDigests") or []),
                 "process-image-invalid")
        if role == "controller":
            need(labels.get(stack.ROLE_LABEL) == "controller"
                 and labels.get(stack.RECIPE_LABEL) == binding.recipe_sha256
                 and cfg.get("Entrypoint") == ["python3", "-I", "-B", "/compat/controller.py"]
                 and not cfg.get("Cmd") and cfg.get("User") == "65532:65532"
                 and cfg.get("WorkingDir") == "/compat", "process-image-invalid")
            env = stack._env(cfg.get("Env") or [])
            need(set(env) <= {"PATH", "LANG", "PYTHON_VERSION", "PYTHON_SHA256", "GPG_KEY"}
                 and all(re.fullmatch(r"[A-Za-z0-9_./:+-]{1,256}", value) for value in env.values()),
                 "process-image-invalid")
        else:
            need(labels.get("org.opencontainers.image.source") == _SOURCE_URL
                 and labels.get("org.opencontainers.image.title") == "rereply-release-web"
                 and cfg.get("Entrypoint") == ["./rereply"] and cfg.get("User") == "rereply"
                 and cfg.get("WorkingDir") == "/app"
                 and cfg.get("Env") in (None, [], [stack._GRAPH_DEFAULT_PATH]), "process-image-invalid")
            # Published web images predate a recipe label. The caller must
            # bind that source's recipe through the existing release verifier.
            need(stack.RECIPE_LABEL not in labels or labels[stack.RECIPE_LABEL] == binding.recipe_sha256,
                 "process-source-invalid")
            if role == "catalog-control":
                # This local web image is built under the existing fixed
                # candidate recipe/receipt. Signed endpoint images are separate.
                need(labels.get(stack.ROLE_LABEL) == "candidate"
                     and labels.get(stack.RECIPE_LABEL) == binding.recipe_sha256,
                     "process-source-invalid")
        need(not cfg.get("Volumes") and not cfg.get("OnBuild") and not cfg.get("Healthcheck"),
             "process-image-invalid")

    def _call(self, args, *, input_bytes=None, timeout=90):
        try:
            value = self.owner.capture([self.owner.binary, "--config", str(self.owner.config),
                                       "--context", "default", *args], env=dict(self.owner.env),
                                      timeout=timeout, stdout_limit=runtime.MIB, stderr_limit=runtime.MIB,
                                      input_bytes=input_bytes)
        except proof.InfrastructureFailure:
            raise
        except Exception:
            raise proof.InfrastructureFailure("container") from None
        need(type(value) is runtime.Capture and len(value.stdout) <= runtime.MIB
             and len(value.stderr) <= runtime.MIB, "process-capture-invalid")
        return value

    def _read(self, args):
        value = self._call(args)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        return value.stdout

    def _network_check(self):
        need(not self.owner._closed and not self.owner._ambiguous, "process-stack-unavailable")
        return self.owner._network_check()

    def _check(self, slot, *, running=None):
        handle = self._handles.get(slot)
        row = stack.OwnedStack._container_check(self, slot, running=running,
                                               started_at=handle.started_at if handle else None)
        need(row.get("RestartCount", 0) == 0, "process-restarted")
        cfg, spec = row["Config"], self._specs[slot]
        need(cfg.get("WorkingDir") == spec["workdir"] and bool(cfg.get("OpenStdin")) is spec["stdin"],
             "process-container-changed")
        return row

    def _assert_all(self):
        need(not self._closed and not self._ambiguous, "process-outcome-unknown")
        self._network_check()
        for role, handle in self.owner._handles.items():
            self.owner._container_check(role, running=True, started_at=handle.started_at)
        for slot in self._ids:
            self._check(slot)

    def _begin(self, operation, slot, identifier=None):
        self._assert_all()
        event = {"operation": operation, "slot": slot, "id": identifier, "outcome": "unknown"}
        self._events.append(event)
        self._ambiguous = True
        return event

    def _id_mutation(self, args, operation, slot, identifier=None):
        event = self._begin(operation, slot, identifier)
        value = self._call(args)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        try:
            observed = value.stdout.decode("ascii", "strict").strip()
        except UnicodeError:
            raise proof.EvidenceError("process-id-invalid") from None
        need(stack._ID.fullmatch(observed) is not None and (identifier is None or observed == identifier),
             "process-id-invalid")
        event.update(id=observed, outcome="acknowledged")
        self._ambiguous = False
        return observed

    def _create(self, slot, version, phase):
        need(slot in {"web", "migration", "controller", "catalog"} and slot not in self._ids)
        need(not self._ids if slot == "catalog" else "catalog" not in self._ids, "process-slot-in-use")
        need(not ({"web", "migration"} & set(self._ids)) if slot in {"web", "migration"} else True,
             "process-slot-in-use")
        image, binding = self._images[version], self.bindings[version]
        controller = slot == "controller"
        command = [] if controller else ["-ec", _WAIT_CONFIG +
            ("server" if slot == "web" else "catalog-snapshot" if slot == "catalog" else "rls-migrate")
            + " -config /compat/private/config.toml" + (" -mode compare" if slot == "catalog" else "")]
        name = self.owner.namespace + "-" + slot
        labels = dict(image["Config"].get("Labels") or {})
        labels.update({stack.LABEL: self.owner.token, stack.ROLE_LABEL: slot,
                       stack.SOURCE_LABEL: binding.source_sha, "codex.compat.phase": phase})
        tmpfs = {"/tmp": "rw,nosuid,nodev,mode=1777"}
        if not controller:
            tmpfs["/compat"] = "rw,nosuid,nodev,mode=1777"
        spec = {"name": name, "image": binding.reference, "image_id": binding.image_id,
                "env": stack._env(image["Config"].get("Env") or []), "labels": labels,
                "tmpfs": tmpfs, "user": "65532:65532" if controller else "rereply", "caps": [],
                "command": command, "entrypoint": image["Config"]["Entrypoint"] if controller else ["/bin/sh"],
                "memory": 128 * runtime.MIB if controller else 512 * runtime.MIB,
                "workdir": "/compat" if controller else "/app", "stdin": controller}
        args = ["container", "create", "--name", name, "--network", self._network, "--network-alias", name,
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
                "--memory", str(spec["memory"]), "--cpus", "1", "--restart", "no", "--stop-timeout", "5",
                "--user", spec["user"], "--workdir", spec["workdir"]]
        if controller:
            args += ["--interactive"]
        else:
            args += ["--entrypoint", "/bin/sh"]
        for key, value in labels.items():
            args += ["--label", key + "=" + value]
        for key, value in tmpfs.items():
            args += ["--tmpfs", key + ":" + value]
        identifier = self._id_mutation(args + [binding.reference, *command], "create", slot)
        need(identifier not in {*self.owner._ids.values(), *self.owner._process_ids.values()}, "process-id-invalid")
        self._ids[slot], self._specs[slot] = identifier, spec
        self.owner._process_ids[slot] = identifier
        self._check(slot, running=False)
        return identifier

    def _start(self, slot, version, phase, inputs):
        need(not self._protocol_failed, "process-controller-failed")
        raw = _configuration(self.owner, inputs, phase)  # Validate before I/O.
        identifier = self._create(slot, version, phase)
        self._id_mutation(["container", "start", identifier], "start", slot, identifier)
        row = self._check(slot, running=True)
        started = row["State"].get("StartedAt")
        need(type(started) is str and started and not started.startswith("0001-"), "process-start-invalid")
        handle = ProcessHandle(slot, version, phase, identifier, self.bindings[version].image_id, started)
        self._handles[slot] = handle
        event = self._begin("configure", slot, identifier)
        value = self._call(["exec", "-i", "--user", "rereply", identifier, "/bin/sh", "-ec", _WRITE_CONFIG],
                           input_bytes=raw, timeout=30)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        need(not value.stdout and not value.stderr, "process-configure-invalid")
        event["outcome"] = "acknowledged"
        self._ambiguous = False
        self._check(slot)
        return handle

    def start_web(self, version, inputs):
        need(version in {"previous", "candidate"})
        return self._start("web", version, "compat-prev-server" if version == "previous" else "compat-cand-server", inputs)

    def start_migration(self, phase, inputs):
        need(phase in _PHASES)
        return self._start("migration", _PHASES[phase], phase, inputs)

    def wait_migration(self, handle, *, timeout=120):
        need(type(handle) is ProcessHandle and self._handles.get("migration") is handle)
        need(type(timeout) in {int, float} and 0 < timeout <= 600)
        self._assert_all()
        value = self._call(["container", "wait", handle.container_id], timeout=timeout)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        need(re.fullmatch(rb"(?:0|[1-9][0-9]{0,2})\s*", value.stdout) is not None, "process-exit-invalid")
        code = int(value.stdout)
        need(code <= 255 and self._check("migration", running=False)["State"].get("ExitCode") == code,
             "process-exit-invalid")
        return code  # Not a migration or compatibility verdict.

    def start_catalog(self, inputs):
        """Fixed producer CLI enforces runtime read-only; no active peers."""
        version = "catalog-control" if "catalog-control" in self.bindings else "candidate"
        return self._start("catalog", version, "compat-control", inputs)

    def wait_catalog(self, handle):
        """Return actual complete CLI bytes, not an accepted catalog result."""
        need(type(handle) is ProcessHandle and self._handles.get("catalog") is handle)
        self._assert_all()
        waited = self._call(["container", "wait", handle.container_id], timeout=90)
        need(type(waited.returncode) is int and waited.returncode == 0 and not waited.stderr
             and re.fullmatch(rb"(?:0|[1-9][0-9]{0,2})\n?", waited.stdout) is not None, "catalog-exit-invalid")
        code = int(waited.stdout)
        row = self._check("catalog", running=False)
        state = row["State"]
        need(row["HostConfig"].get("LogConfig") in ({"Type":"json-file","Config":{}},
             {"Type":"json-file","Config":{"max-size":"-1"}}) and row["Config"].get("Tty") is False,
             "catalog-log-driver-unproven")
        need(code <= 255 and type(state.get("ExitCode")) is int and state["ExitCode"] == code
             and not state.get("OOMKilled") and not state.get("Error"), "catalog-exit-invalid")
        streams = self._call(["logs", handle.container_id], timeout=10)
        need(type(streams.returncode) is int and streams.returncode == 0
             and type(streams.stdout) is bytes and type(streams.stderr) is bytes, "catalog-log-incomplete")
        self._check("catalog", running=False)
        return runtime.Capture(code, streams.stdout, streams.stderr)

    def controller(self, request: bytes) -> bytes:
        """One invocation, private bytes only; no transport retry or disk output."""
        import controller
        need(not self._protocol_failed, "process-controller-failed")
        parsed = controller.parse_request(request)
        fixture = parsed["fixture"]
        need(fixture["namespace"] == self.owner.namespace, "process-controller-input-invalid")
        need(fixture['product_origin'] == self.owner.planned_web_origin
             and fixture['stub_origin'] == self.owner.graph_origin, 'process-controller-input-invalid')
        self._assert_all()
        if parsed['operation'] not in {'journal', 'rejected'}:
            need("web" in self._ids and self._check("web", running=True), "process-web-unavailable")
        else:
            need(not ({'migration','catalog'} & set(self._ids)), 'process-slot-in-use')
        need(len(self._controller_receipts) < 256, 'process-receipt-budget')
        identifier = self._create("controller", "controller", "compat-control")
        event = self._begin("controller", "controller", identifier)
        # A known container exit does not establish a valid sweep outcome.
        # Latch before execution and clear only after complete protocol checks.
        # This blocks new work but leaves exact owned cleanup available when
        # Docker outcomes are known; transport ambiguity remains stricter.
        self._protocol_failed = True
        timeout = {"setup": 100, "readiness": 30, "journal": 190, "rejected": 190, "write": 600, "read": 400}[parsed["operation"]]
        value = self._call(["container", "start", "--attach", "--interactive", identifier],
                           input_bytes=request, timeout=timeout)
        need(value.returncode in (0, 1) and not value.stderr, "process-controller-incomplete")
        row = self._check("controller", running=False)
        need(row["State"].get("ExitCode") == value.returncode, "process-controller-incomplete")
        started = row["State"].get("StartedAt")
        need(type(started) is str and started and not started.startswith("0001-"), "process-start-invalid")
        self._handles["controller"] = ProcessHandle("controller", "controller", "compat-control", identifier,
                                                     self.bindings["controller"].image_id, started)
        event["outcome"] = "acknowledged"
        self._ambiguous = False
        self.remove(self._handles["controller"])
        response = controller._parse(value.stdout)
        need(response.get("version") == 1 and type(response["version"]) is int,
             "process-controller-response-invalid")
        if response.get("status") == "error":
            need(set(response) == {"version", "status", "failure"} and value.returncode == 1,
                 "process-controller-response-invalid")
            failure = response["failure"]
            need(type(failure) is str and failure in {"infrastructure", "semantic", "evidence"},
                 "process-controller-response-invalid")
            if failure == "infrastructure":
                raise proof.InfrastructureFailure("controller")
            if failure == "semantic":
                raise controller.sweep.SemanticFailure("process-controller-semantic-failure")
            raise proof.EvidenceError("process-controller-evidence-failure")
        operation = parsed["operation"]
        payload = {"setup": "registered", "readiness": "ready", "journal": "journal", "rejected": "rejected"}.get(operation, "cases")
        keys = {"version", "status", "operation", payload, 'stub_receipts'}
        if operation == "write":
            keys.add("record")
        need(set(response) == keys and response.get("status") == "ok" and response.get("operation") == operation
             and value.returncode == 0, "process-controller-response-invalid")
        if operation in {"journal", "rejected"}:
            controller.journal_result(response[operation], after=parsed["after"],
                                      discard_baseline=parsed["discard_baseline"])
        elif operation == "setup":
            need(response["registered"] is True, "process-controller-response-invalid")
        elif operation == "readiness":
            need(response["ready"] == {"web": True, "graph": True}
                 and all(type(v) is bool for v in response["ready"].values()), "process-controller-response-invalid")
        else:
            controller._flags(response["cases"])
        if operation == "write":
            record = controller._record(response["record"], controller._fixture(fixture))
            need(record.nonce == parsed["nonce"], "process-controller-response-invalid")
        receipt = controller.validate_receipts(parsed, response)
        audit = {'operation':operation, 'fixture_binding':controller.sweep.Sweep(controller._fixture(fixture))._binding(),
                 'receipt':receipt}
        if operation == 'write': audit['nonce'] = parsed['nonce']
        if operation == 'setup': audit['business_account_id'] = parsed['business_account_id']
        private = controller._encode(audit)
        need(sum(map(len,self._controller_receipts)) + len(private) <= 4*runtime.MIB, 'process-receipt-budget')
        self._controller_receipts.append(private)
        self._protocol_failed = False
        return value.stdout

    def controller_receipts(self):
        """Private immutable snapshots for the continuous outer ledger only."""
        return tuple(self._controller_receipts)

    def remove(self, handle):
        need(type(handle) is ProcessHandle and self._handles.get(handle.slot) is handle)
        self._assert_all()
        row = self._check(handle.slot)
        if row["State"]["Running"]:
            self._id_mutation(["container", "stop", "--timeout", "5", handle.container_id],
                              "stop", handle.slot, handle.container_id)
        self._check(handle.slot, running=False)
        self._id_mutation(["container", "rm", handle.container_id], "remove", handle.slot, handle.container_id)
        absent = self._read(["container", "ls", "--all", "--no-trunc", "--filter", "id=" + handle.container_id,
                             "--format", "{{.ID}}"])
        need(not absent.strip(), "process-removal-unconfirmed")
        del self._ids[handle.slot], self._handles[handle.slot], self._specs[handle.slot]
        del self.owner._process_ids[handle.slot]
        self._assert_all()

    def close(self):
        self._assert_all()
        for handle in reversed(tuple(self._handles.values())):
            self.remove(handle)
        need(not self._ids, "process-unstarted-resource-remains")
        self._closed = True

    def evidence(self):
        return {"operations": [dict(event) for event in self._events], "container_ids": dict(self._ids),
                "ambiguous": self._ambiguous, "closed": self._closed,
                "controller_protocol_failed": self._protocol_failed,
                "release_attestation_verified_here": False, "full_compatibility_proof": False}
