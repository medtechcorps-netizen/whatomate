from __future__ import annotations

import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import stack as s

SOURCE = "a" * 40
TOKEN = "b" * 32
GRAPH = "sha256:" + "3" * 64
NETWORK = "9" * 64


class DockerWorld:
    def __init__(self):
        self.calls, self.mutations = [], []
        self.network, self.containers = None, {}
        self.fail, self.transform = None, None
        self.counter = 0
        self.images = {}
        for ref, number, repo, cfg in (
            (s.runtime.POSTGRES, "1", "postgres", {"Env": ["PATH=/usr/bin", "PG_MAJOR=17"],
             "Entrypoint": ["docker-entrypoint.sh"], "Cmd": ["postgres"], "User": "", "Labels": {}}),
            (s.runtime.REDIS, "2", "redis", {"Env": ["PATH=/usr/bin"],
             "Entrypoint": ["docker-entrypoint.sh"], "Cmd": ["redis-server"], "User": "", "Labels": {}}),
            (GRAPH, "3", None, {"Env": None, "Entrypoint": ["/graph-stub"], "Cmd": None,
             "User": "65532:65532", "Labels": {s.SOURCE_LABEL: SOURCE, s.ROLE_LABEL: "graph",
              s.RECIPE_LABEL: s.GRAPH_RECIPE_SHA256}})):
            self.images[ref] = {"Id": "sha256:" + number * 64, "Os": "linux", "Architecture": "amd64",
                                "Config": cfg, "RepoDigests": [repo + "@" + ref.split("@", 1)[1]] if repo else []}

    @staticmethod
    def flags(args, start):
        values = {}
        i = start
        while i < len(args) and args[i].startswith("--"):
            flag = args[i]
            if flag in ("--internal", "--read-only"):
                value, i = True, i + 1
            else:
                value, i = args[i + 1], i + 2
            values.setdefault(flag, []).append(value)
        return values, args[i:]

    @staticmethod
    def mapping(values):
        return dict(v.split("=", 1) for v in values)

    def capture(self, argv, **options):
        assert argv[:2] == ["docker", "--config"] and argv[3:5] == ["--context", "default"]
        assert options["stdout_limit"] <= s.runtime.MIB and options["stderr_limit"] <= s.runtime.MIB
        assert 0 < options["timeout"] <= 180
        args = argv[5:]
        self.calls.append((list(args), options))
        mutation = tuple(args[:2]) in {("network", "create"), ("network", "rm"),
                                      ("container", "create"), ("container", "start"),
                                      ("container", "stop"), ("container", "rm")}
        if mutation:
            self.mutations.append(list(args))
        if self.fail and self.fail(args):
            return s.runtime.Capture(1, b"private malformed output", b"synthetic password and host details")
        if args[:2] == ["image", "inspect"]:
            result = [self.images[args[2]]]
        elif args[:2] == ["network", "create"]:
            flags, tail = self.flags(args, 2)
            self.network = {"Id": NETWORK, "Name": tail[0], "Driver": flags["--driver"][0],
                            "Scope": "local", "Internal": flags["--internal"][0], "Ingress": False,
                            "EnableIPv6": False, "Labels": self.mapping(flags["--label"]), "Containers": {}}
            result = NETWORK
        elif args[:2] == ["network", "inspect"]:
            if self.network is None:
                return s.runtime.Capture(1, b"", b"not found")
            result = [self.network]
        elif args[:2] == ["container", "create"]:
            flags, tail = self.flags(args, 2)
            reference, command = tail[0], tail[1:]
            self.counter += 1
            identifier = str(self.counter + 3) * 64
            image = self.images[reference]
            cfg = copy.deepcopy(image["Config"])
            env = self.mapping(cfg.get("Env") or [])
            env.update(self.mapping(flags.get("--env", [])))
            cfg.update(Image=reference, User=flags.get("--user", [cfg.get("User", "")])[0],
                       Cmd=command, Labels=self.mapping(flags["--label"]),
                       Env=[k + "=" + v for k, v in env.items()])
            tmpfs = dict(v.split(":", 1) for v in flags.get("--tmpfs", []))
            host = {"NetworkMode": flags["--network"][0], "ReadonlyRootfs": flags["--read-only"][0],
                    "RestartPolicy": {"Name": flags["--restart"][0]}, "CapDrop": flags["--cap-drop"],
                    "CapAdd": flags.get("--cap-add"), "SecurityOpt": flags["--security-opt"],
                    "PidsLimit": int(flags["--pids-limit"][0]), "Memory": int(flags["--memory"][0]),
                    "NanoCpus": int(flags["--cpus"][0]) * 1000000000, "Tmpfs": tmpfs,
                    "Binds": None, "PortBindings": {}, "Privileged": False, "AutoRemove": False}
            name = flags["--name"][0]
            self.containers[identifier] = {"Id": identifier, "Name": "/" + name, "Image": image["Id"],
                "Config": cfg, "HostConfig": host, "State": {"Running": False, "StartedAt": "0001-01-01T00:00:00Z"},
                "Mounts": [{"Type": "tmpfs", "Destination": key} for key in tmpfs],
                "NetworkSettings": {"Networks": {self.network["Name"]: {"NetworkID": NETWORK,
                                                                          "Aliases": flags["--network-alias"]}}}}
            self.network["Containers"][identifier] = {"Name": name}
            result = identifier
        elif args[:2] == ["container", "inspect"]:
            if args[2] not in self.containers:
                return s.runtime.Capture(1, b"", b"not found")
            result = [self.containers[args[2]]]
        elif args[:2] == ["container", "start"]:
            self.containers[args[2]]["State"].update(Running=True, StartedAt="2026-10-11T00:00:01Z")
            result = args[2]
        elif args[:2] == ["container", "stop"]:
            self.containers[args[-1]]["State"]["Running"] = False
            result = args[-1]
        elif args[:2] == ["container", "rm"]:
            assert self.containers[args[2]]["State"]["Running"] is False
            del self.containers[args[2]]
            del self.network["Containers"][args[2]]
            result = args[2]
        elif args[:2] == ["container", "ls"]:
            identifier = args[args.index("--filter") + 1][3:]
            result = identifier if identifier in self.containers else ""
        elif args[:2] == ["network", "rm"]:
            assert not self.network["Containers"]
            self.network = None
            result = args[2]
        elif args[:2] == ["network", "ls"]:
            result = NETWORK if self.network else ""
        else:
            raise AssertionError("Unexpected command")
        result = copy.deepcopy(result)
        if self.transform:
            result = self.transform(args, result)
        data = result.encode() if type(result) is str else json.dumps(result).encode()
        return s.runtime.Capture(0, data, b"")


class StackTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.world = DockerWorld()
        self.stack = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                 token=TOKEN, capture=self.world.capture)

    def test_owned_three_services_and_exact_cleanup_ids(self):
        handles = self.stack.start()
        self.assertEqual([h.role for h in handles], ["pg", "redis", "graph"])
        self.assertTrue(all(h.network_id == NETWORK for h in handles))
        self.assertEqual(self.stack.graph_origin, "http://rereply-compat-bbbbbbbbbbbb-graph:8090")
        self.assertEqual(self.stack.planned_web_origin, "http://rereply-compat-bbbbbbbbbbbb-web:8080")
        self.assertFalse(any("web" == h.role or "relay" == h.role for h in handles))
        self.stack.cleanup()
        self.assertFalse(self.world.containers)
        self.assertIsNone(self.world.network)
        removals = [x for x in self.world.mutations if x[1] == "rm"]
        self.assertEqual([x[2] for x in removals], [h.container_id for h in reversed(handles)] + [NETWORK])
        self.assertTrue(self.stack.evidence()["closed"])
        self.assertEqual(len(self.stack.evidence()["container_ids"]), 3)
        self.assertFalse(self.stack.evidence()["complete_compatibility_proof"])
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()

    def test_exact_pins_settings_and_no_ports_mounts_or_external_graph(self):
        self.stack.start()
        creates = [x for x in self.world.mutations if x[:2] == ["container", "create"]]
        self.assertEqual(len(creates), 3)
        for args in creates:
            self.assertNotIn("--publish", args); self.assertNotIn("-p", args)
            self.assertNotIn("--mount", args); self.assertNotIn("--volume", args)
            self.assertNotIn("--privileged", args)
        self.assertIn(s.runtime.POSTGRES, creates[0]); self.assertIn(s.runtime.REDIS, creates[1])
        pg = self.world.containers[self.stack._ids["pg"]]
        self.assertEqual(pg["Config"]["Cmd"], s._PG_COMMAND)
        self.assertIn("log_timezone=UTC", pg["Config"]["Cmd"])
        self.assertIn("lc_messages=C", pg["Config"]["Cmd"])
        graph = self.world.containers[self.stack._ids["graph"]]
        env = self.world.mapping(graph["Config"]["Env"])
        self.assertEqual(env["STUB_CALLBACK_ORIGIN"], self.stack.planned_web_origin)
        self.assertEqual(env["STUB_ENVIRONMENT"], "local")
        self.assertEqual(graph["Config"]["Entrypoint"], ["/graph-stub"])

    def test_graph_empty_or_exact_default_path_survives_exact_container_check(self):
        for image_env in (None, [], [s._GRAPH_DEFAULT_PATH]):
            with self.subTest(image_env=image_env):
                world = DockerWorld()
                world.images[GRAPH]["Config"]["Env"] = copy.deepcopy(image_env)
                owned = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                     token=TOKEN, capture=world.capture)
                owned.start()
                env = world.mapping(world.containers[owned._ids["graph"]]["Config"]["Env"])
                self.assertEqual(env.get("PATH"), s._GRAPH_DEFAULT_PATH.split("=", 1)[1] if image_env else None)
                self.assertEqual(env, owned._specs["graph"]["env"])
                owned.cleanup()
                self.assertTrue(owned.evidence()["closed"])

    def test_graph_path_duplicates_additions_and_alterations_refuse_before_mutation(self):
        for image_env in ([s._GRAPH_DEFAULT_PATH, s._GRAPH_DEFAULT_PATH],
                          [s._GRAPH_DEFAULT_PATH, "EXTRA=value"],
                          [s._GRAPH_DEFAULT_PATH, "STUB_ENVIRONMENT=external"],
                          ["PATH=/usr/bin"], ["PATH="], ["path=/usr/bin"],
                          ["LD_PRELOAD=/other"]):
            with self.subTest(image_env=image_env):
                world = DockerWorld()
                world.images[GRAPH]["Config"]["Env"] = image_env
                owned = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                     token=TOKEN, capture=world.capture)
                with self.assertRaises(s.proof.EvidenceError):
                    owned.start()
                self.assertEqual(world.mutations, [])

    def test_graph_default_path_post_create_drift_still_refuses_cleanup(self):
        self.world.images[GRAPH]["Config"]["Env"] = [s._GRAPH_DEFAULT_PATH]
        self.stack.start()
        row = self.world.containers[self.stack._ids["graph"]]
        original = copy.deepcopy(row["Config"]["Env"])
        for changed in ([v for v in original if not v.startswith("PATH=")],
                        ["PATH=/other" if v.startswith("PATH=") else v for v in original],
                        original + [s._GRAPH_DEFAULT_PATH], original + ["EXTRA=value"]):
            with self.subTest(changed=changed):
                row["Config"]["Env"] = changed
                with self.assertRaises(s.proof.EvidenceError):
                    self.stack._container_check("graph")
        count = len(self.world.mutations)
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)

    def test_graph_recipe_is_existing_source_not_duplicate_server(self):
        path = Path(__file__).resolve().parents[2] / "docker/staging/graph-stub.Dockerfile"
        self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(), s.GRAPH_RECIPE_SHA256)
        self.assertIn(b"./release/staging/graphstub/cmd/graph-stub", path.read_bytes())

    def test_docker_canonical_capabilities_keep_exact_allowlist_and_cleanup(self):
        def canonical(args, value):
            if args[:2] == ["container", "inspect"] and value[0]["Config"]["Image"] == s.runtime.POSTGRES:
                value[0]["HostConfig"]["CapAdd"] = ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER",
                                                    "CAP_SETGID", "CAP_SETUID"]
            return value
        self.world.transform = canonical
        self.stack.start()
        self.stack.cleanup()
        self.assertTrue(self.stack.evidence()["closed"])
        self.assertFalse(self.world.containers)
        self.assertIsNone(self.world.network)

    def test_capability_additions_duplicates_missing_or_altered_refuse(self):
        expected = ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETGID", "CAP_SETUID"]
        for observed in (expected + ["CAP_SYS_ADMIN"], expected + ["CAP_CHOWN"], expected[:-1],
                         ["CHOWN", *expected[1:]], ["cap_chown", *expected[1:]],
                         ["CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "CHOWN"]):
            with self.subTest(observed=observed):
                world = DockerWorld()
                def changed(args, value):
                    if args[:2] == ["container", "inspect"]:
                        value[0]["HostConfig"]["CapAdd"] = observed
                    return value
                world.transform = changed
                owned = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                     token=TOKEN, capture=world.capture)
                with self.assertRaises(s.proof.EvidenceError):
                    owned.start()
                count = len(world.mutations)
                self.assertFalse(any(args[:2] == ["container", "start"] for args in world.mutations))
                with self.assertRaises(s.proof.EvidenceError):
                    owned.cleanup()
                self.assertEqual(len(world.mutations), count)

    def test_stack_scram_and_collector_local_socket_compose_without_password(self):
        import postgres as pg
        from test_postgres import FakeDocker
        handles = self.stack.start()
        container = handles[0].container_id
        row = self.world.containers[container]
        env = self.world.mapping(row["Config"]["Env"])
        self.assertEqual(env["POSTGRES_INITDB_ARGS"], "--auth-local=trust --auth-host=scram-sha-256")
        self.assertIn("unix_socket_directories=" + pg.SOCKET_DIRECTORY, row["Config"]["Cmd"])
        self.assertIn(pg.SOCKET_DIRECTORY, row["HostConfig"]["Tmpfs"])
        sql = FakeDocker()
        secret = self.stack.fixture_inputs.postgres_password
        def composed(argv, **options):
            args = argv[5:]
            if args == ["inspect", container]:
                return s.runtime.Capture(0, json.dumps([row]).encode(), b"")
            if args == ["network", "inspect", NETWORK]:
                return s.runtime.Capture(0, json.dumps([self.world.network]).encode(), b"")
            self.assertEqual(args[:2], ["exec", "-i"])
            self.assertIn(container, args)
            self.assertNotIn(secret, json.dumps(argv) + json.dumps(options["env"]))
            self.assertNotIn(secret.encode(), options["input_bytes"])
            # Model actual SCRAM refusal of the old passwordless TCP route.
            if args[args.index("-h") + 1] != pg.SOCKET_DIRECTORY:
                return s.runtime.Capture(3, b"", b"password required")
            return sql(argv, **options)
        db = pg.SyntheticPostgres("docker", Path(self.tmp.name), container, TOKEN, "compat_runtime", capture=composed)
        result = db.capture_tables().public()
        self.assertTrue(result["complete"])
        self.assertNotIn(secret, json.dumps(result) + json.dumps(self.stack.evidence()))

    def test_source_architecture_entrypoint_or_baked_env_refuse_before_mutation(self):
        for change in (lambda x: x.update(Architecture="arm64"),
                       lambda x: x["Config"]["Labels"].update({s.SOURCE_LABEL: "c" * 40}),
                       lambda x: x["Config"]["Labels"].update({s.RECIPE_LABEL: "c" * 64}),
                       lambda x: x["Config"]["Labels"].update({s.ROLE_LABEL: "relay"}),
                       lambda x: x["Config"].update(Entrypoint=["/other"]),
                       lambda x: x["Config"].update(Env=["DATABASE_URL=external"]),
                       lambda x: x["Config"].update(Volumes={"/host": {}})):
            world = DockerWorld(); change(world.images[GRAPH])
            stack = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                 token=TOKEN, capture=world.capture)
            with self.subTest(change=change), self.assertRaises(s.proof.EvidenceError):
                stack.start()
            self.assertEqual(world.mutations, [])

    def test_pinned_digest_and_malformed_image_id_refuse(self):
        for field, value in (("RepoDigests", ["postgres@sha256:" + "f" * 64]), ("Id", "tag:latest")):
            world = DockerWorld(); world.images[s.runtime.POSTGRES][field] = value
            stack = s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE),
                                 token=TOKEN, capture=world.capture)
            with self.assertRaises(s.proof.EvidenceError):
                stack.start()
            self.assertEqual(world.mutations, [])

    def test_inputs_and_config_are_restricted_before_commands(self):
        for value in ("tag:latest", "sha256:" + "g" * 64, GRAPH + "\n--privileged"):
            with self.assertRaises(s.proof.EvidenceError):
                s.GraphImage(value, SOURCE)
        with self.assertRaises(s.proof.EvidenceError):
            s.OwnedStack("docker", Path(self.tmp.name), "c" * 40, s.GraphImage(GRAPH, SOURCE))
        with self.assertRaises(s.proof.EvidenceError):
            s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE), token="../bad")
        (Path(self.tmp.name) / "config.json").write_text('{}')
        with self.assertRaises(s.proof.EvidenceError):
            s.OwnedStack("docker", Path(self.tmp.name), SOURCE, s.GraphImage(GRAPH, SOURCE))
        self.assertEqual(self.world.calls, [])

    def test_ambiguous_network_create_is_never_retried_or_adopted(self):
        self.world.fail = lambda args: args[:2] == ["network", "create"]
        with self.assertRaises(s.proof.InfrastructureFailure):
            self.stack.start()
        for action in (self.stack.start, self.stack.cleanup):
            with self.assertRaises(s.proof.EvidenceError):
                action()
        self.assertEqual(len(self.world.mutations), 1)
        self.assertTrue(self.stack.evidence()["ambiguous"])
        self.assertIsNone(self.stack.evidence()["network_id"])

    def test_malformed_created_id_keeps_unknown_outcome_and_known_network(self):
        self.world.transform = lambda args, value: "short-id" if args[:2] == ["container", "create"] else value
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.start()
        self.assertEqual(self.stack.evidence()["network_id"], NETWORK)
        self.assertTrue(self.stack.evidence()["ambiguous"])
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertFalse(any(x[1] == "rm" for x in self.world.mutations))

    def test_foreign_network_attachment_refuses_all_cleanup(self):
        self.stack.start()
        self.world.network["Containers"]["f" * 64] = {"Name": "foreign"}
        count = len(self.world.mutations)
        with self.assertRaisesRegex(s.proof.EvidenceError, "stack-foreign-attachment"):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)
        self.assertFalse(self.stack.evidence()["closed"])

    def test_changed_ownership_or_isolation_refuses_before_any_cleanup(self):
        self.stack.start()
        identifier = self.stack._ids["pg"]
        original = copy.deepcopy(self.world.containers[identifier])
        changes = (
            lambda r: r["Config"]["Labels"].update({s.LABEL: "f" * 32}),
            lambda r: r.update(Image="sha256:" + "f" * 64),
            lambda r: r["Config"]["Env"].append("EXTRA=value"),
            lambda r: r["HostConfig"].update(PortBindings={"5432/tcp": [{"HostPort": "5432"}]}),
            lambda r: r["HostConfig"].update(Binds=["/host:/x"]),
            lambda r: r["HostConfig"].update(Privileged=True),
            lambda r: r["HostConfig"].update(NetworkMode="host"),
            lambda r: r["HostConfig"].update(SecurityOpt=[]),
            lambda r: r["Mounts"].append({"Type": "volume", "Destination": "/x"}),
            lambda r: r["NetworkSettings"]["Networks"].update(other={"NetworkID": "f" * 64}),
            lambda r: next(iter(r["NetworkSettings"]["Networks"].values()))["Aliases"].append("external-host"),
            lambda r: r["State"].update(StartedAt="2026-10-11T00:00:02Z"))
        count = len(self.world.mutations)
        for change in changes:
            self.world.containers[identifier] = copy.deepcopy(original)
            change(self.world.containers[identifier])
            with self.subTest(change=change), self.assertRaises(s.proof.EvidenceError):
                self.stack._container_check("pg", started_at=self.stack._handles["pg"].started_at)
        # A real cleanup checks every peer before removing an otherwise-valid graph.
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)

    def test_changed_network_internal_or_label_refuses(self):
        self.stack.start()
        for field, value in (("Internal", False), ("Driver", "host"), ("Name", "another"),
                             ("Labels", {}), ("EnableIPv6", True)):
            original = self.world.network[field]
            self.world.network[field] = value
            with self.assertRaises(s.proof.EvidenceError):
                self.stack._network_check()
            self.world.network[field] = original

    def test_unknown_stop_refuses_further_cleanup_and_keeps_all_ids(self):
        self.stack.start()
        self.world.fail = lambda args: args[:2] == ["container", "stop"]
        with self.assertRaises(s.proof.InfrastructureFailure):
            self.stack.cleanup()
        count = len(self.world.mutations)
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)
        self.assertEqual(len(self.stack.evidence()["container_ids"]), 3)
        self.assertTrue(self.stack.evidence()["ambiguous"])

    def test_cleanup_uses_timeout_without_deprecated_stdout_pollution(self):
        def deprecated_warning(args, value):
            if args[:2] == ["container", "stop"] and "--time" in args:
                return "Flag --time has been deprecated, use --timeout instead\n" + value
            return value
        self.world.transform = deprecated_warning
        handles = self.stack.start()
        self.stack.cleanup()
        stops = [args for args in self.world.mutations if args[:2] == ["container", "stop"]]
        self.assertEqual(stops, [["container", "stop", "--timeout", "5", handle.container_id]
                                 for handle in reversed(handles)])
        self.assertTrue(self.stack.evidence()["closed"])
        self.assertFalse(self.world.containers)
        self.assertIsNone(self.world.network)

    def test_stop_stdout_noise_still_refuses_without_retry_or_removal(self):
        self.stack.start()
        self.world.transform = lambda args, value: ("unexpected stdout\n" + value
            if args[:2] == ["container", "stop"] else value)
        with self.assertRaisesRegex(s.proof.EvidenceError, "stack-id-invalid"):
            self.stack.cleanup()
        count = len(self.world.mutations)
        self.assertTrue(self.stack.evidence()["ambiguous"])
        self.assertEqual(len(self.stack.evidence()["container_ids"]), 3)
        self.assertFalse(any(args[:2] == ["container", "rm"] for args in self.world.mutations))
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)

    def test_removal_needs_observed_absence_and_does_not_retry(self):
        self.stack.start()
        self.world.transform = lambda args, value: "f" * 64 if args[:2] == ["container", "ls"] else value
        with self.assertRaisesRegex(s.proof.EvidenceError, "stack-removal-unconfirmed"):
            self.stack.cleanup()
        count = len(self.world.mutations)
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.cleanup()
        self.assertEqual(len(self.world.mutations), count)
        self.assertFalse(self.stack.evidence()["closed"])

    def test_running_is_not_readiness_and_dead_service_refuses(self):
        def alter(args, value):
            if args[:2] == ["container", "inspect"] and value[0]["State"]["Running"]:
                value[0]["State"]["Running"] = False
            return value
        self.world.transform = alter
        with self.assertRaisesRegex(s.proof.EvidenceError, "stack-service-not-running"):
            self.stack.start()
        self.assertFalse(self.stack.evidence()["complete_compatibility_proof"])

    def test_tmpfs_inspect_omission_and_unstarted_endpoint_are_narrow(self):
        def shape(args, value):
            if args[:2] == ["container", "inspect"]:
                value[0]["Mounts"] = []
                if not value[0]["State"]["Running"] and value[0]["State"]["StartedAt"].startswith("0001-"):
                    next(iter(value[0]["NetworkSettings"]["Networks"].values()))["NetworkID"] = ""
            return value
        self.world.transform = shape
        self.stack.start()
        self.world.transform = None
        row = self.world.containers[self.stack._ids["pg"]]
        peer = next(iter(row["NetworkSettings"]["Networks"].values()))
        peer["NetworkID"] = ""
        with self.assertRaises(s.proof.EvidenceError):
            self.stack._container_check("pg")
        row["State"]["Running"] = False
        with self.assertRaises(s.proof.EvidenceError):
            self.stack._container_check("pg")
        row["State"]["StartedAt"] = "0001-01-01T00:00:00Z"
        peer["IPAddress"] = "172.20.0.5"
        with self.assertRaises(s.proof.EvidenceError):
            self.stack._container_check("pg")

    def test_public_evidence_and_errors_do_not_disclose_synthetic_secrets(self):
        self.world.fail = lambda args: args[:2] == ["container", "create"]
        with self.assertRaises(s.proof.InfrastructureFailure) as caught:
            self.stack.start()
        text = json.dumps(self.stack.evidence()) + str(caught.exception) + repr(self.stack.fixture_inputs)
        for value in vars(self.stack.fixture_inputs).values():
            self.assertNotIn(value, text)
        self.assertNotIn("synthetic password", text)
        self.assertNotIn("STUB_", text)

    def test_no_resource_cleanup_after_image_preflight_refusal(self):
        self.world.images[GRAPH]["Config"]["User"] = "root"
        with self.assertRaises(s.proof.EvidenceError):
            self.stack.start()
        self.stack.cleanup()
        self.assertEqual(self.world.mutations, [])
        self.assertTrue(self.stack.evidence()["closed"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
