"""Once-only fixture export on an already owned synthetic PostgreSQL stack.

No import effects, builds, pulls, image adoption, arbitrary SQL, remote DSNs or
release verdicts. An image must already have been built from the reviewed source:
Linux/amd64 CGO_ENABLED=0 `go test -c ./internal/database`, /compat/database.test,
working directory /src/internal/database, plus the exact source files at
/src/internal/dbcatalog/{golden/production-v0.json,shape/production-v0.sql}.
The original catalog is embedded in the test binary. Image/source/recipe labels
are bindings to that separately reviewed build, not attestations implemented here.

The caller must close this adapter before creating OwnedProcesses. Role/DB SQL
runs before measured phases; its stdin and PostgreSQL log bytes are private.
Credentials never enter Docker arguments, environment, public evidence or disk.
The fixed Go export protocol contains synthetic identities, not credentials.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
from pathlib import Path
import re
import secrets
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import processes
import proof
import runtime
import stack
import sweep

_PROFILE = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"
_ROLE, _WORKDIR = "fixture", "/src/internal/database"
_ENTRY = ["/compat/database.test"]
_COMMAND = ["-test.run=^TestCompatFixtureExport$", "-test.timeout=4m"]
_PREFIX = "COMPAT_FIXTURE_JSON:"
_UUID4 = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\Z")
_PROGRESS = re.compile(r"\r  Running migrations  (█*)\x1b\[90m(░*)\x1b\[0m ([ ]{0,2}[0-9]{1,3})%")
_COMPLETE = "\n  \x1b[32m✓ Migration completed\x1b[0m\n\n"


def need(value, code="fixture-input-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def _encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


@dataclass(frozen=True)
class ExportImage:
    image_id: str
    source_sha: str
    recipe_sha256: str

    def __post_init__(self):
        need(type(self.image_id) is str and stack._IMAGE.fullmatch(self.image_id) is not None)
        need(type(self.source_sha) is str and stack._SHA.fullmatch(self.source_sha) is not None)
        need(type(self.recipe_sha256) is str and processes._HEX.fullmatch(self.recipe_sha256) is not None)


@dataclass(frozen=True, repr=False)
class ProvisionedFixture:
    fixture: sweep.Fixture
    business_account_id: str
    catalog_sha256: str
    pg_container_id: str
    pg_started_at: str
    owner_token: str


def _parse_export(raw, request):
    """Closed stdout grammar from the actual Go progress/export/test sources.

    No arbitrary prelude is ignored. Every byte is either the fixed progress
    sequence, its success terminator, one strict JSON object, or final PASS.
    Docker's independent exit/state checks remain necessary for acceptance.
    """
    need(type(raw) is bytes and len(raw) <= runtime.MIB, "fixture-output-invalid")
    try:
        text = raw.decode("utf-8", "strict")
    except UnicodeError:
        raise proof.EvidenceError("fixture-output-invalid") from None
    need(text.startswith("\n") and text.count(_PREFIX) == 1, "fixture-output-invalid")
    position, previous, count = 1, -1, 0
    while (match := _PROGRESS.match(text, position)) is not None:
        filled, empty, printed = match.groups()
        percent = int(printed)
        need(len(filled) + len(empty) == 40 and printed == f"{percent:3d}" and
             0 <= percent <= 100 and previous <= percent and (count != 0 or percent == 0),
             "fixture-output-invalid")
        # Both integer values are floors of the same source fraction.
        need(percent * 40 < (len(filled) + 1) * 100 and len(filled) * 100 < (percent + 1) * 40,
             "fixture-output-invalid")
        previous, position, count = percent, match.end(), count + 1
    need(2 <= count <= 4096 and previous == 100 and text.startswith(_COMPLETE + _PREFIX, position),
         "fixture-output-invalid")
    tail = text[position + len(_COMPLETE + _PREFIX):]
    need(tail.endswith("\nPASS\n") and tail.count("\n") == 2, "fixture-output-invalid")
    payload = tail[:-len("\nPASS\n")].encode()
    need(len(payload) <= 4096, "fixture-output-invalid")
    try:
        value = runtime.common.loads_strict(payload)
    except runtime.common.ReleaseError:
        raise proof.EvidenceError("fixture-output-invalid") from None
    keys = {"version", "namespace", "organization_id", "user_id", "account_id", "channel_account_id",
            "account_name", "email", "phone_number_id", "business_account_id", "catalog_sha256"}
    need(type(value) is dict and set(value) == keys and type(value["version"]) is int and value["version"] == 1,
         "fixture-output-invalid")
    expected = {key: request[key] for key in ("namespace", "organization_id", "phone_number_id", "business_account_id")}
    expected.update(account_name=request["namespace"] + "-wa", email=request["namespace"] + "-client@example.test",
                    catalog_sha256=_PROFILE)
    need(all(value.get(key) == item for key, item in expected.items()), "fixture-output-binding")
    identities = [value[key] for key in ("organization_id", "user_id", "account_id", "channel_account_id")]
    need(all(type(v) is str and _UUID4.fullmatch(v) for v in identities) and len(set(identities)) == 4,
         "fixture-output-binding")
    return value


class OwnedFixture:
    def __init__(self, owner: stack.OwnedStack, image: ExportImage):
        need(type(owner) is stack.OwnedStack and type(image) is ExportImage)
        need(not owner._closed and not owner._ambiguous and set(owner._handles) == {"pg", "redis", "graph"})
        need(owner._process_manager is None and not owner._process_ids and not getattr(owner, "_fixture_attempted", False),
             "fixture-already-attempted")
        self.owner, self.image = owner, image
        self._closed = self._ambiguous = self._attempted = self._cleanup_attempted = False
        self._id = self._started = None
        self._events = []
        self._assert_services()
        self._image = stack._json(self._read(["image", "inspect", image.image_id]))
        self._validate_image()
        # Reuse the existing exclusive manager/ID guard; do not weaken or
        # replace any OwnedStack check. Shared-network containers have no own
        # network endpoint but still must block PG cleanup until removed.
        owner._process_manager = self

    def _validate_image(self):
        row, binding = self._image, self.image
        cfg = row.get("Config", {})
        need(row.get("Id") == binding.image_id and row.get("Os") == "linux" and row.get("Architecture") == "amd64"
             and type(cfg) is dict, "fixture-image-invalid")
        labels = cfg.get("Labels") or {}
        need(type(labels) is dict and labels.get(stack.SOURCE_LABEL) == binding.source_sha
             and labels.get(stack.ROLE_LABEL) == _ROLE and labels.get(stack.RECIPE_LABEL) == binding.recipe_sha256,
             "fixture-source-invalid")
        need(cfg.get("Entrypoint") == _ENTRY and not cfg.get("Cmd") and cfg.get("WorkingDir") == _WORKDIR
             and cfg.get("User") == "65532:65532" and cfg.get("Env") in (None, [], [stack._GRAPH_DEFAULT_PATH])
             and not cfg.get("Volumes") and not cfg.get("OnBuild") and not cfg.get("Healthcheck"), "fixture-image-invalid")

    def _call(self, args, *, input_bytes=None, timeout=90):
        try:
            value = self.owner.capture([self.owner.binary, "--config", str(self.owner.config), "--context", "default", *args],
                env=dict(self.owner.env), timeout=timeout, stdout_limit=runtime.MIB, stderr_limit=runtime.MIB,
                input_bytes=input_bytes)
        except proof.InfrastructureFailure:
            raise
        except Exception:
            raise proof.InfrastructureFailure("container") from None
        need(type(value) is runtime.Capture and type(value.returncode) is int
             and type(value.stdout) is bytes and type(value.stderr) is bytes
             and len(value.stdout) <= runtime.MIB and len(value.stderr) <= runtime.MIB,
             "fixture-capture-invalid")
        return value

    def _read(self, args):
        value = self._call(args)
        need(value.returncode == 0 and not value.stderr, "fixture-read-failed")
        return value.stdout

    def _assert_services(self):
        need(not self._closed and not self._ambiguous and not self.owner._closed and not self.owner._ambiguous,
             "fixture-outcome-unknown")
        self.owner._network_check()
        for role, handle in self.owner._handles.items():
            row = self.owner._container_check(role, running=True, started_at=handle.started_at)
            need(row.get("RestartCount", 0) == 0, "fixture-service-restarted")

    def _begin(self, operation, identifier=None):
        self._assert_services()
        event = {"operation": operation, "id": identifier, "outcome": "unknown"}
        self._events.append(event)
        self._ambiguous = True
        return event

    def _ack(self, event, *, identifier=None):
        if identifier is not None:
            event["id"] = identifier
        event["outcome"] = "acknowledged"
        self._ambiguous = False

    def _sql(self, script, *, database="postgres", readonly=True):
        self._assert_services()
        need(database in {"postgres", "compat"} and type(script) is bytes and len(script) <= 8192)
        return self._call(["exec", "-i", "--env", "PGAPPNAME=compat-fixture-setup", "--env", "PGCLIENTENCODING=UTF8",
            "--env", "PGOPTIONS=-c default_transaction_read_only=" + ("on" if readonly else "off"),
            self.owner._ids["pg"], "psql", "-X", "-w", "-qAt", "-h", "/var/run/postgresql", "-p", "5432",
            "-U", "postgres", "-d", database, "-v", "ON_ERROR_STOP=1"], input_bytes=script)

    def _check(self, *, running=None, after_execution=False):
        self._assert_services()
        need(type(self._id) is str and stack._ID.fullmatch(self._id), "fixture-container-invalid")
        row = stack._json(self._read(["container", "inspect", self._id]))
        try:
            cfg, host, state = row["Config"], row["HostConfig"], row["State"]
            labels = dict(self._image["Config"].get("Labels") or {})
            labels.update({stack.LABEL: self.owner.token, stack.ROLE_LABEL: _ROLE,
                           "codex.compat.phase": "compat-fixture-setup"})
            env = stack._env(self._image["Config"].get("Env") or [])
            env["COMPAT_FIXTURE_EXPORT"] = "1"
            need(row["Id"] == self._id and row["Name"] == "/" + self.owner.namespace + "-fixture"
                 and row["Image"] == self.image.image_id and cfg["Image"] == self.image.image_id
                 and cfg["Labels"] == labels and stack._env(cfg.get("Env") or []) == env
                 and cfg.get("Entrypoint") == _ENTRY and cfg.get("Cmd") == _COMMAND
                 and cfg.get("WorkingDir") == _WORKDIR and cfg.get("User") == "65532:65532"
                 and cfg.get("OpenStdin") is True and not cfg.get("Tty"), "fixture-container-changed")
            need(host["NetworkMode"] == "container:" + self.owner._ids["pg"] and host["ReadonlyRootfs"] is True
                 and not host.get("Privileged") and not host.get("Binds") and not host.get("PortBindings")
                 and not host.get("PublishAllPorts") and not host.get("AutoRemove") and not host.get("Devices")
                 and not host.get("DeviceRequests") and not host.get("ExtraHosts") and not host.get("Dns")
                 and not host.get("PidMode") and not host.get("UTSMode") and host["RestartPolicy"]["Name"] == "no"
                 and host["CapDrop"] == ["ALL"] and not host.get("CapAdd") and host["SecurityOpt"] == ["no-new-privileges"]
                 and host["PidsLimit"] == 128 and host["Memory"] == 512 * runtime.MIB
                 and host["NanoCpus"] == 1000000000
                 and host.get("Tmpfs") == {"/tmp": "rw,nosuid,nodev,mode=1777"}, "fixture-isolation-changed")
            mounts = row.get("Mounts", [])
            need(type(mounts) is list and len(mounts) <= 1 and all(v.get("Type") == "tmpfs" and v.get("Destination") == "/tmp" for v in mounts)
                 and not row["NetworkSettings"].get("Networks"), "fixture-isolation-changed")
            need(type(state["Running"]) is bool and not state.get("Restarting") and not state.get("Paused") and not state.get("Dead") and not state.get("OOMKilled") and not state.get("Error")
                 and row.get("RestartCount", 0) == 0 and (running is None or state["Running"] is running)
                 and (state.get("StartedAt") == self._started if self._started is not None else
                      after_execution or (type(state.get("StartedAt")) is str and state["StartedAt"].startswith("0001-"))),
                 "fixture-state-changed")
            return row
        except (KeyError, TypeError, AttributeError):
            raise proof.EvidenceError("fixture-inspect-invalid") from None

    def run(self, inputs: processes.ConfigInputs, *, password: str) -> ProvisionedFixture:
        need(type(inputs) is processes.ConfigInputs and _UUID4.fullmatch(inputs.organization_id) is not None)
        need(type(password) is str and processes._HEX.fullmatch(password) is not None)
        need(not self._attempted and not getattr(self.owner, "_fixture_attempted", False), "fixture-already-attempted")
        self._attempted = self.owner._fixture_attempted = True
        self._assert_services()
        preflight = self._sql(b"SELECT json_build_array('empty',current_setting('server_version_num')::int/10000,current_user='postgres',"
            b"(SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user),"
            b"(SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname IN ('compat_owner','compat_runtime')),"
            b"(SELECT count(*) FROM pg_catalog.pg_database WHERE datname='compat'));")
        try:
            empty = runtime.common.loads_strict(preflight.stdout)
        except runtime.common.ReleaseError:
            raise proof.EvidenceError("fixture-target-not-empty") from None
        need(type(preflight.returncode) is int and preflight.returncode == 0 and not preflight.stderr
             and _encoded(empty) == _encoded(["empty", 17, True, True, 0, 0]), "fixture-target-not-empty")
        sql = ("CREATE ROLE compat_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION PASSWORD '" + inputs.owner_password + "';\n"
               "CREATE ROLE compat_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION PASSWORD '" + inputs.runtime_password + "';\n"
               "CREATE DATABASE compat OWNER compat_owner TEMPLATE template0;\n\\connect compat\n"
               "ALTER SCHEMA public OWNER TO compat_owner;\n").encode()
        event = self._begin("create-database-roles", self.owner._ids["pg"])
        value = self._call(["exec", "-i", "--env", "PGAPPNAME=compat-fixture-setup", "--env", "PGCLIENTENCODING=UTF8",
            "--env", "PGOPTIONS=-c default_transaction_read_only=off", self.owner._ids["pg"],
            "psql", "-X", "-w", "-qAt", "-h", "/var/run/postgresql", "-p", "5432", "-U", "postgres", "-d", "postgres",
            "-v", "ON_ERROR_STOP=1"], input_bytes=sql)
        need(value.returncode == 0 and not value.stdout and not value.stderr, "fixture-setup-incomplete")
        self._ack(event)
        request = {"version": 1, "namespace": self.owner.namespace, "organization_id": inputs.organization_id,
            "owner_dsn": "postgres://compat_owner:" + inputs.owner_password + "@127.0.0.1:5432/compat?sslmode=disable",
            "runtime_dsn": "postgres://compat_runtime:" + inputs.runtime_password + "@127.0.0.1:5432/compat?sslmode=disable",
            "password": password, "encryption_key": inputs.encryption_key,
            "graph_access_token": self.owner.fixture_inputs.graph_access_token,
            "phone_number_id": "999" + str(secrets.randbelow(10**12)).zfill(12)}
        request["business_account_id"] = "999" + str((int(request["phone_number_id"][3:]) + 1) % 10**12).zfill(12)
        raw = _encoded(request)
        need(len(raw) <= 16384)
        labels = dict(self._image["Config"].get("Labels") or {})
        labels.update({stack.LABEL: self.owner.token, stack.ROLE_LABEL: _ROLE, "codex.compat.phase": "compat-fixture-setup"})
        args = ["container", "create", "--name", self.owner.namespace + "-fixture", "--network", "container:" + self.owner._ids["pg"],
            "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128", "--memory", str(512*runtime.MIB),
            "--cpus", "1", "--restart", "no", "--stop-timeout", "5", "--user", "65532:65532", "--workdir", _WORKDIR,
            "--interactive", "--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777", "--env", "COMPAT_FIXTURE_EXPORT=1"]
        for key, value in labels.items():
            args += ["--label", key + "=" + value]
        event = self._begin("create-exporter")
        value = self._call(args + [self.image.image_id, *_COMMAND])
        need(value.returncode == 0 and not value.stderr, "fixture-create-incomplete")
        try:
            identifier = value.stdout.decode("ascii", "strict").strip()
        except UnicodeError:
            raise proof.EvidenceError("fixture-id-invalid") from None
        need(stack._ID.fullmatch(identifier) is not None and identifier not in self.owner._ids.values(), "fixture-id-invalid")
        self._id = identifier
        self.owner._process_ids[_ROLE] = identifier
        self._ack(event, identifier=identifier)
        self._check(running=False)
        event = self._begin("execute-exporter", identifier)
        value = self._call(["container", "start", "--attach", "--interactive", identifier], input_bytes=raw, timeout=270)
        # A returned CLI error alone cannot identify a terminal container. Keep
        # unknown outcome latched unless a fresh exact terminal state agrees.
        self._ambiguous = False
        try:
            row = self._check(running=False, after_execution=True)
        finally:
            self._ambiguous = True
        state = row["State"]
        need(type(value.returncode) is int and 0 <= value.returncode <= 255 and type(state.get("ExitCode")) is int
             and state["ExitCode"] == value.returncode and type(state.get("StartedAt")) is str
             and not state["StartedAt"].startswith("0001-"), "fixture-execution-incomplete")
        self._started = state["StartedAt"]
        self._ack(event)
        event.update(exit_code=value.returncode, stdout_bytes=len(value.stdout), stderr_bytes=len(value.stderr),
                     stdout_sha256=hashlib.sha256(value.stdout).hexdigest(), stderr_sha256=hashlib.sha256(value.stderr).hexdigest())
        need(value.returncode == 0 and not value.stderr, "fixture-export-failed")
        result = _parse_export(value.stdout, request)
        self._assert_services()
        fixture = sweep.Fixture(namespace=self.owner.namespace, product_origin=self.owner.planned_web_origin,
            stub_origin=self.owner.graph_origin, organization_id=result["organization_id"], account_id=result["account_id"],
            channel_account_id=result["channel_account_id"], account_name=result["account_name"], phone_number_id=result["phone_number_id"],
            email=result["email"], password=password, control_key=self.owner.fixture_inputs.graph_control_key)
        return ProvisionedFixture(fixture, result["business_account_id"], result["catalog_sha256"],
            self.owner._handles["pg"].container_id, self.owner._handles["pg"].started_at, self.owner.token)

    def close(self):
        need(not self._cleanup_attempted and self.owner._process_manager is self, "fixture-cleanup-refused")
        self._assert_services()
        self._cleanup_attempted = True
        if self._id is not None:
            row = self._check()
            if row["State"]["Running"]:
                event = self._begin("stop-exporter", self._id)
                value = self._call(["container", "stop", "--timeout", "5", self._id])
                need(value.returncode == 0 and not value.stderr and value.stdout.strip() == self._id.encode(), "fixture-stop-incomplete")
                self._ack(event)
            self._check(running=False)
            event = self._begin("remove-exporter", self._id)
            value = self._call(["container", "rm", self._id])
            need(value.returncode == 0 and not value.stderr and value.stdout.strip() == self._id.encode(), "fixture-remove-incomplete")
            self._ack(event)
            need(not self._read(["container", "ls", "--all", "--no-trunc", "--filter", "id=" + self._id, "--format", "{{.ID}}"]).strip(),
                 "fixture-removal-unconfirmed")
            del self.owner._process_ids[_ROLE]
        self._assert_services()
        self._closed = True
        self.owner._process_manager = None

    def evidence(self):
        return {"source_sha": self.image.source_sha, "image_id": self.image.image_id, "namespace": self.owner.namespace,
                "exporter_id": self._id, "operations": [dict(row) for row in self._events], "attempted": self._attempted,
                "ambiguous": self._ambiguous, "closed": self._closed, "full_compatibility_proof": False}
