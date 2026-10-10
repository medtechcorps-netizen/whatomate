"""Owned synthetic PG/Redis/Graph containers, not a compatibility runner.

No commands run at import. Images must already exist. The Graph image's source
labels are a local build binding, not a signature or proof of build provenance;
the future caller must separately validate its exact-source build. No builds,
pulls, host ports, host mounts, arbitrary environment, controller, web process,
catalog bootstrap or relay service is implemented here. PR8's canonical stack
names PG17, Redis and the existing Graph stub; relay integration is separate.

Created resources are recorded before the next action. An unknown command
outcome is never retried or recovered by name; evidence() retains the bounded
ownership/intention record. Cleanup only acts on recorded, freshly inspected
IDs. A foreign network attachment, altered configuration or ambiguous outcome
refuses cleanup. A future in-network controller must be integrated explicitly,
not given a Docker socket or silently exempted from this ownership check.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import re
import secrets
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof
import runtime

LABEL = "codex.compat"
ROLE_LABEL = "codex.compat.role"
SOURCE_LABEL = "org.opencontainers.image.revision"
RECIPE_LABEL = "codex.compat.recipe-sha256"
GRAPH_RECIPE_SHA256 = "484b0aca0b9f4833d7a0338b6b5492e0602d51027b4aa7929b94df8c88f7f62a"
_ID = re.compile(r"[0-9a-f]{64}\Z")
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_TOKEN = re.compile(r"[0-9a-f]{32}\Z")
_IMAGE = re.compile(r"sha256:[0-9a-f]{64}\Z")
_ROLES = ("pg", "redis", "graph")
_PG_COMMAND = ["postgres", "-c", "unix_socket_directories=/var/run/postgresql",
               "-c", "autovacuum=off", "-c", "log_statement=mod",
               "-c", "log_min_error_statement=error", "-c", "log_error_verbosity=verbose",
               "-c", "log_line_prefix=%m [%p] %a %e ", "-c", "log_timezone=UTC",
               "-c", "lc_messages=C"]


def need(ok, code="stack-input-invalid"):
    if not ok:
        raise proof.EvidenceError(code)


def _json(raw):
    try:
        value = runtime.common.loads_strict(raw)
        need(type(value) is list and len(value) == 1 and type(value[0]) is dict)
        return value[0]
    except (runtime.common.ReleaseError, TypeError, ValueError):
        raise proof.EvidenceError("stack-inspect-invalid") from None


def _env(values):
    need(type(values) is list and all(type(v) is str and "=" in v for v in values))
    result = {}
    for value in values:
        key, item = value.split("=", 1)
        need(key and key not in result, "stack-image-invalid")
        result[key] = item
    return result


@dataclass(frozen=True)
class GraphImage:
    image_id: str
    source_sha: str

    def __post_init__(self):
        need(type(self.image_id) is str and _IMAGE.fullmatch(self.image_id) is not None)
        need(type(self.source_sha) is str and _SHA.fullmatch(self.source_sha) is not None)


@dataclass(frozen=True, repr=False)
class SyntheticInputs:
    """Generated ephemeral fixture material; never credentials from a host."""
    postgres_password: str
    graph_control_key: str
    graph_access_token: str
    graph_app_secret: str
    graph_app_id: str


@dataclass(frozen=True)
class ServiceHandle:
    role: str
    container_id: str
    image_id: str
    network_id: str
    hostname: str
    port: int
    started_at: str


class OwnedStack:
    def __init__(self, docker_binary: str, config_dir: Path, source_sha: str,
                 graph_image: GraphImage, *, token: str | None = None,
                 capture=runtime.bounded_command, base_env=None):
        need(type(source_sha) is str and _SHA.fullmatch(source_sha) is not None)
        need(type(graph_image) is GraphImage and graph_image.source_sha == source_sha)
        token = secrets.token_hex(16) if token is None else token
        need(type(token) is str and _TOKEN.fullmatch(token) is not None)
        self.binary = runtime._binary(docker_binary)
        self.config = runtime._empty_directory(config_dir)
        self.env = runtime.common.scrubbed_env(base={} if base_env is None else base_env)
        self.capture, self.source_sha, self.graph_image = capture, source_sha, graph_image
        self.token, self.namespace = token, "rereply-compat-" + token[:12]
        self._inputs = SyntheticInputs(*(secrets.token_hex(32) for _ in range(4)),
                                       "999" + str(int(token[:10], 16)).zfill(13))
        self._network = None
        self._images, self._specs, self._ids, self._handles = {}, {}, {}, {}
        self._events = []
        self._attempted = self._closed = self._ambiguous = self._cleanup_attempted = False

    @property
    def fixture_inputs(self) -> SyntheticInputs:
        return self._inputs

    @property
    def planned_web_origin(self) -> str:
        """Reserved callback identity only; no web container exists yet."""
        return "http://" + self.namespace + "-web:8080"

    @property
    def graph_origin(self) -> str:
        need("graph" in self._handles and not self._closed)
        return "http://" + self._handles["graph"].hostname + ":8090"

    def evidence(self) -> dict:
        """Public-safe resource/operation metadata, never args or environments."""
        return {"source_sha": self.source_sha, "namespace": self.namespace,
                "network_id": self._network, "container_ids": dict(self._ids),
                "operations": [dict(row) for row in self._events],
                "ambiguous": self._ambiguous, "closed": self._closed,
                "cleanup_attempted": self._cleanup_attempted,
                "complete_compatibility_proof": False}

    def _call(self, args):
        try:
            value = self.capture([self.binary, "--config", str(self.config), "--context", "default", *args],
                                 env=dict(self.env), timeout=90,
                                 stdout_limit=runtime.MIB, stderr_limit=runtime.MIB)
        except proof.InfrastructureFailure:
            raise
        except (OSError, ValueError):
            raise proof.InfrastructureFailure("container") from None
        need(type(value) is runtime.Capture and len(value.stdout) <= runtime.MIB
             and len(value.stderr) <= runtime.MIB, "stack-capture-invalid")
        return value

    def _read(self, args):
        value = self._call(args)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        return value.stdout

    def _mutation(self, args, operation, role, expected_id=None):
        need(not self._ambiguous and not self._closed, "stack-outcome-unknown")
        event = {"operation": operation, "role": role, "id": expected_id, "outcome": "unknown"}
        self._events.append(event)
        self._ambiguous = True
        value = self._call(args)
        if value.returncode != 0:
            raise proof.InfrastructureFailure("container")
        try:
            identifier = value.stdout.decode("ascii", "strict").strip()
        except UnicodeError:
            raise proof.EvidenceError("stack-id-invalid") from None
        need(_ID.fullmatch(identifier) is not None
             and (expected_id is None or identifier == expected_id), "stack-id-invalid")
        event.update(id=identifier, outcome="acknowledged")
        self._ambiguous = False
        return identifier

    def _labels(self, role):
        return {LABEL: self.token, ROLE_LABEL: role, SOURCE_LABEL: self.source_sha}

    def _inspect_images(self):
        # Inspect all inputs before the first resource mutation. No pulls here.
        pinned = runtime.Docker(self.binary, self.config, capture=self.capture, base_env=self.env)
        for role, image in (("pg", runtime.POSTGRES), ("redis", runtime.REDIS)):
            row = pinned.inspect(image)
            need(type(row.get("Id")) is str and _IMAGE.fullmatch(row["Id"]) is not None)
            self._images[role] = row
        row = _json(self._read(["image", "inspect", self.graph_image.image_id]))
        cfg = row.get("Config", {})
        need(row.get("Id") == self.graph_image.image_id and row.get("Os") == "linux"
             and row.get("Architecture") == "amd64" and type(cfg) is dict, "stack-image-invalid")
        labels = cfg.get("Labels", {}) or {}
        need(type(labels) is dict and labels.get(SOURCE_LABEL) == self.source_sha and labels.get(ROLE_LABEL) == "graph"
             and labels.get(RECIPE_LABEL) == GRAPH_RECIPE_SHA256, "stack-source-binding-invalid")
        need(cfg.get("Entrypoint") == ["/graph-stub"] and not cfg.get("Cmd")
             and cfg.get("User") == "65532:65532" and not cfg.get("Env")
             and not cfg.get("Volumes") and not cfg.get("OnBuild")
             and not cfg.get("Healthcheck"), "stack-image-invalid")
        self._images["graph"] = row
        for item in self._images.values():
            need(type(item.get("Config")) is dict, "stack-image-invalid")
            need(type(item["Config"].get("Labels") or {}) is dict, "stack-image-invalid")
            _env(item["Config"].get("Env") or [])

    def _network_check(self):
        need(type(self._network) is str and _ID.fullmatch(self._network) is not None)
        row = _json(self._read(["network", "inspect", self._network]))
        need(row.get("Id") == self._network and row.get("Name") == self.namespace
             and row.get("Driver") == "bridge" and row.get("Scope") == "local"
             and row.get("Internal") is True and not row.get("Ingress")
             and not row.get("EnableIPv6") and row.get("Labels") == self._labels("network"),
             "stack-network-changed")
        peers = row.get("Containers") or {}
        need(type(peers) is dict and set(peers) <= set(self._ids.values()), "stack-foreign-attachment")
        return row

    def _spec(self, role):
        name = self.namespace + "-" + role
        image = {"pg": runtime.POSTGRES, "redis": runtime.REDIS, "graph": self.graph_image.image_id}[role]
        values, tmpfs, user, caps, command = {}, {}, "", [], []
        if role == "pg":
            values = {"POSTGRES_USER": "postgres", "POSTGRES_DB": "postgres",
                      "POSTGRES_PASSWORD": self._inputs.postgres_password,
                      "POSTGRES_INITDB_ARGS": "--auth-local=trust --auth-host=scram-sha-256"}
            tmpfs = {"/var/lib/postgresql/data": "rw,nosuid,nodev,mode=0700",
                     "/var/run/postgresql": "rw,nosuid,nodev,mode=1777", "/tmp": "rw,nosuid,nodev,mode=1777"}
            caps = ["CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID"]
            command = list(_PG_COMMAND)
        elif role == "redis":
            user = "redis"
            tmpfs = {"/data": "rw,nosuid,nodev,mode=1777"}
            command = ["redis-server", "--save", "", "--appendonly", "no", "--protected-mode", "no"]
        else:
            user = "65532:65532"
            values = {"STUB_ENVIRONMENT": "local", "STUB_CONTROL_KEY": self._inputs.graph_control_key,
                      "STUB_ACCESS_TOKENS": self._inputs.graph_access_token,
                      "STUB_APP_ID": self._inputs.graph_app_id, "STUB_APP_SECRET": self._inputs.graph_app_secret,
                      "STUB_CALLBACK_ORIGIN": self.planned_web_origin, "STUB_LISTEN_ADDR": ":8090"}
        labels = dict(self._images[role]["Config"].get("Labels") or {})
        labels.update(self._labels(role))
        env = _env(self._images[role]["Config"].get("Env") or [])
        env.update(values)
        return {"name": name, "image": image, "image_id": self._images[role]["Id"],
                "env": env, "overrides": values, "labels": labels, "tmpfs": tmpfs,
                "user": user or self._images[role]["Config"].get("User", ""), "caps": caps,
                "command": command or (self._images[role]["Config"].get("Cmd") or []),
                "entrypoint": self._images[role]["Config"].get("Entrypoint"),
                "memory": 512 * runtime.MIB if role == "pg" else 128 * runtime.MIB}

    def _container_check(self, role, *, running=None, started_at=None):
        self._network_check()
        spec, identifier = self._specs[role], self._ids[role]
        row = _json(self._read(["container", "inspect", identifier]))
        try:
            cfg, host, state = row["Config"], row["HostConfig"], row["State"]
            need(row["Id"] == identifier and row["Name"] == "/" + spec["name"]
                 and row["Image"] == spec["image_id"] and cfg["Image"] == spec["image"]
                 and cfg["Labels"] == spec["labels"] and _env(cfg["Env"] or []) == spec["env"]
                 and cfg.get("User", "") == spec["user"] and (cfg.get("Cmd") or []) == spec["command"]
                 and cfg.get("Entrypoint") == spec["entrypoint"], "stack-container-changed")
            need(host["NetworkMode"] == self._network and host["ReadonlyRootfs"] is True
                 and not host.get("Privileged") and not host.get("Binds") and not host.get("PortBindings")
                 and not host.get("PublishAllPorts") and not host.get("AutoRemove")
                 and not host.get("Devices") and not host.get("DeviceRequests")
                 and not host.get("ExtraHosts") and not host.get("Dns")
                 and not host.get("PidMode") and not host.get("UTSMode")
                 and host["RestartPolicy"]["Name"] == "no" and host["CapDrop"] == ["ALL"]
                 and (host.get("CapAdd") or []) == spec["caps"]
                 and host["SecurityOpt"] == ["no-new-privileges"]
                 and host["PidsLimit"] == 128 and host["Memory"] == spec["memory"]
                 and host["NanoCpus"] == 1000000000 and (host.get("Tmpfs") or {}) == spec["tmpfs"],
                 "stack-isolation-changed")
            mounts = row.get("Mounts", [])
            # --tmpfs is recorded in HostConfig.Tmpfs; some engines omit those
            # mounts from Mounts. Any reported mount must still be a known tmpfs.
            need(type(mounts) is list and len(mounts) == len({m.get("Destination") for m in mounts})
                 and all(m.get("Type") == "tmpfs" for m in mounts)
                 and {m.get("Destination") for m in mounts} <= set(spec["tmpfs"]), "stack-mount-changed")
            networks = row["NetworkSettings"]["Networks"]
            need(type(networks) is dict and len(networks) == 1, "stack-network-changed")
            peer = next(iter(networks.values()))
            aliases = peer.get("Aliases") or []
            need(type(aliases) is list and all(type(alias) is str for alias in aliases)
                 and spec["name"] in aliases and set(aliases) <= {spec["name"], identifier, identifier[:12]},
                 "stack-network-changed")
            # Docker may allocate the endpoint only at start. This exception
            # permits no active/previously-started container or foreign endpoint.
            pending_endpoint = (state.get("Running") is False and started_at is None
                                and str(state.get("StartedAt", "")).startswith("0001-")
                                and peer.get("NetworkID") == "" and not peer.get("EndpointID")
                                and not peer.get("IPAddress") and not peer.get("GlobalIPv6Address"))
            need(peer["NetworkID"] == self._network or pending_endpoint, "stack-network-changed")
            need(type(state["Running"]) is bool and not state.get("Restarting")
                 and not state.get("Paused") and not state.get("Dead")
                 and (running is None or state["Running"] is running), "stack-service-not-running")
            need(started_at is None or state.get("StartedAt") == started_at, "stack-service-restarted")
            return row
        except (KeyError, TypeError, AttributeError):
            raise proof.EvidenceError("stack-inspect-invalid") from None

    def start(self) -> tuple[ServiceHandle, ...]:
        """Create once and confirm processes running; not health/readiness proof.

        The database is initially empty. Role/database/catalog installation is
        deliberately left to the reviewed loopback bootstrap adapter.
        """
        need(not self._attempted and not self._closed, "stack-already-attempted")
        self._attempted = True
        self._inspect_images()
        args = ["network", "create", "--internal", "--driver", "bridge"]
        for key, value in self._labels("network").items():
            args += ["--label", key + "=" + value]
        self._network = self._mutation(args + [self.namespace], "create", "network")
        self._network_check()
        for role in _ROLES:
            self._network_check()
            spec = self._spec(role)
            self._specs[role] = spec
            args = ["container", "create", "--name", spec["name"], "--network", self._network,
                    "--network-alias", spec["name"], "--read-only", "--cap-drop", "ALL",
                    "--security-opt", "no-new-privileges", "--pids-limit", "128",
                    "--memory", str(spec["memory"]), "--cpus", "1", "--restart", "no", "--stop-timeout", "5"]
            for cap in spec["caps"]:
                args += ["--cap-add", cap]
            if spec["user"]:
                args += ["--user", spec["user"]]
            for key, value in spec["labels"].items():
                args += ["--label", key + "=" + value]
            for key, value in spec["tmpfs"].items():
                args += ["--tmpfs", key + ":" + value]
            for key, value in spec["overrides"].items():
                args += ["--env", key + "=" + value]
            self._ids[role] = self._mutation(args + [spec["image"], *spec["command"]], "create", role)
            self._container_check(role, running=False)
            self._mutation(["container", "start", self._ids[role]], "start", role, self._ids[role])
            row = self._container_check(role, running=True)
            started = row["State"].get("StartedAt")
            need(type(started) is str and started and not started.startswith("0001-"), "stack-service-not-running")
            self._handles[role] = ServiceHandle(role, self._ids[role], spec["image_id"], self._network,
                                                spec["name"], {"pg": 5432, "redis": 6379, "graph": 8090}[role], started)
        return tuple(self._handles[r] for r in _ROLES)

    def cleanup(self) -> None:
        """One removal per exact recorded resource; no force/prune/adoption."""
        need(not self._closed and not self._ambiguous and not self._cleanup_attempted, "stack-outcome-unknown")
        self._cleanup_attempted = True
        if self._network is None:
            need(not self._ids)
            self._closed = True
            return
        self._network_check()
        # Refuse a changed peer before deleting any otherwise-valid resource.
        for role in self._ids:
            handle = self._handles.get(role)
            self._container_check(role, started_at=handle.started_at if handle else None)
        for role in reversed(tuple(self._ids)):
            handle = self._handles.get(role)
            row = self._container_check(role, started_at=handle.started_at if handle else None)
            identifier = self._ids[role]
            if row["State"]["Running"]:
                self._mutation(["container", "stop", "--time", "5", identifier], "stop", role, identifier)
            self._container_check(role, running=False, started_at=handle.started_at if handle else None)
            self._mutation(["container", "rm", identifier], "remove", role, identifier)
            absent = self._read(["container", "ls", "--all", "--no-trunc", "--filter", "id=" + identifier,
                                 "--format", "{{.ID}}"])
            need(not absent.strip(), "stack-removal-unconfirmed")
            # Keep the public ID record; the network may only lose known peers.
        row = self._network_check()
        need(not row.get("Containers"), "stack-network-not-empty")
        self._mutation(["network", "rm", self._network], "remove", "network", self._network)
        absent = self._read(["network", "ls", "--no-trunc", "--filter", "id=" + self._network,
                             "--format", "{{.ID}}"])
        need(not absent.strip(), "stack-removal-unconfirmed")
        self._closed = True
