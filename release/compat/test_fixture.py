"""Fixture execution contracts: fake Docker plus a real local Python stdio child.

No container, network, database, build or Go invocation is made by this suite.
"""
from dataclasses import replace
import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import fixture as f
import test_stack

IMAGE = "sha256:" + "7" * 64
SOURCE, RECIPE, EXPORTER = "c" * 40, "d" * 64, "8" * 64
BINDING = f.ExportImage(IMAGE, SOURCE, RECIPE)
INPUTS = f.processes.ConfigInputs(*(hashlib.sha256(("fixture-" + k).encode()).hexdigest()
    for k in ("runtime", "owner", "jwt", "encryption")), "20000000-0000-4000-8000-000000000001")
PASSWORD = hashlib.sha256(b"fixture-login").hexdigest()


def result_for(request):
    return {"version": 1, "namespace": request["namespace"], "organization_id": request["organization_id"],
        "user_id": "20000000-0000-4000-8000-000000000002", "account_id": "20000000-0000-4000-8000-000000000003",
        "channel_account_id": "20000000-0000-4000-8000-000000000004", "account_name": request["namespace"] + "-wa",
        "email": request["namespace"] + "-client@example.test", "phone_number_id": request["phone_number_id"],
        "business_account_id": request["business_account_id"], "catalog_sha256": f._PROFILE}


def output_for(request):
    text = "\n"
    for percent, filled in ((0, 0), (50, 20), (100, 40)):
        text += "\r  Running migrations  " + "█" * filled + "\x1b[90m" + "░" * (40-filled) + "\x1b[0m " + f"{percent:3d}%"
    return (text + f._COMPLETE + f._PREFIX + json.dumps(result_for(request), separators=(",", ":")) + "\nPASS\n").encode()


class World(test_stack.DockerWorld):
    def __init__(self):
        super().__init__()
        self.images[IMAGE] = {"Id": IMAGE, "Os": "linux", "Architecture": "amd64", "Config": {
            "Entrypoint": f._ENTRY, "Cmd": [], "Env": [], "User": "65532:65532", "WorkingDir": f._WORKDIR,
            "Labels": {f.stack.SOURCE_LABEL: SOURCE, f.stack.ROLE_LABEL: "fixture", f.stack.RECIPE_LABEL: RECIPE}}}
        self.preflight = ["empty", 17, True, True, 0, 0]
        self.response = None
        self.request = None
        self.exit_code, self.stderr = 0, b""
        self.execute_hook = None
        self.unknown = None
        self.created_roles = False
        self.create_response = None

    @staticmethod
    def flags(args, start):
        values = {}
        i = start
        while i < len(args) and args[i].startswith("--"):
            flag = args[i]
            if flag in ("--internal", "--read-only", "--interactive"):
                value, i = True, i + 1
            else:
                value, i = args[i+1], i+2
            values.setdefault(flag, []).append(value)
        return values, args[i:]

    def capture(self, argv, **options):
        args = argv[5:]
        special = args[:2] == ["exec", "-i"] or args[:4] == ["container", "start", "--attach", "--interactive"] or (
            args[:2] == ["container", "create"] and IMAGE in args) or args == ["container", "rm", EXPORTER]
        if not special:
            return super().capture(argv, **options)
        assert options["stdout_limit"] == f.runtime.MIB and options["stderr_limit"] == f.runtime.MIB
        assert 0 < options["timeout"] <= 270
        self.calls.append((list(args), options))
        if self.unknown and self.unknown(args):
            raise OSError("private-" + PASSWORD)
        if args[:2] == ["exec", "-i"]:
            assert args[-14:] == ["psql", "-X", "-w", "-qAt", "-h", "/var/run/postgresql", "-p", "5432", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1"]
            assert args[-2:] == ["-v", "ON_ERROR_STOP=1"]
            sql = options["input_bytes"]
            if sql.startswith(b"SELECT"):
                assert "PGOPTIONS=-c default_transaction_read_only=on" in args
                return f.runtime.Capture(0, json.dumps(self.preflight).encode(), b"")
            assert sql.startswith(b"CREATE ROLE compat_owner ") and "PGOPTIONS=-c default_transaction_read_only=off" in args
            self.mutations.append(list(args)); self.created_roles = True
            return f.runtime.Capture(0, b"", b"")
        self.mutations.append(list(args))
        if args[:2] == ["container", "create"]:
            flags, tail = self.flags(args, 2)
            assert tail == [IMAGE, *f._COMMAND]
            cfg = copy.deepcopy(self.images[IMAGE]["Config"])
            env = self.mapping(cfg.get("Env") or [])
            env.update(self.mapping(flags.get("--env", [])))
            cfg.update(Image=IMAGE, Cmd=tail[1:], Labels=self.mapping(flags["--label"]),
                       Env=[k+"="+v for k,v in env.items()], OpenStdin=True, Tty=False)
            self.containers[EXPORTER] = {"Id": EXPORTER, "Name": "/"+flags["--name"][0], "Image": IMAGE,
                "Config": cfg, "HostConfig": {"NetworkMode": flags["--network"][0], "ReadonlyRootfs": True,
                    "RestartPolicy": {"Name": "no"}, "CapDrop": ["ALL"], "CapAdd": None,
                    "SecurityOpt": ["no-new-privileges"], "PidsLimit": 128, "Memory": 512*f.runtime.MIB,
                    "NanoCpus": 1000000000, "Tmpfs": {"/tmp": "rw,nosuid,nodev,mode=1777"}},
                "Mounts": [], "NetworkSettings": {"Networks": {}}, "RestartCount": 0,
                "State": {"Running": False, "StartedAt": "0001-01-01T00:00:00Z", "ExitCode": 0}}
            return self.create_response if self.create_response is not None else f.runtime.Capture(0, EXPORTER.encode(), b"")
        if args[:4] == ["container", "start", "--attach", "--interactive"]:
            self.request = json.loads(options["input_bytes"])
            self.containers[EXPORTER]["State"].update(Running=False, StartedAt="2026-10-11T00:01:00Z", ExitCode=self.exit_code)
            if self.execute_hook:
                self.execute_hook(self)
            output = output_for(self.request) if self.response is None else self.response(self.request)
            return f.runtime.Capture(self.exit_code, output, self.stderr)
        del self.containers[EXPORTER]
        return f.runtime.Capture(0, EXPORTER.encode(), b"")


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.world = World()
        self.owner = f.stack.OwnedStack("docker", Path(self.tmp.name), test_stack.SOURCE,
            f.stack.GraphImage(test_stack.GRAPH, test_stack.SOURCE), token=test_stack.TOKEN, capture=self.world.capture)
        self.owner.start()

    def make(self):
        return f.OwnedFixture(self.owner, BINDING)

    def test_success_exact_namespace_source_image_and_owner_cleanup(self):
        adapter = self.make()
        result = adapter.run(INPUTS, password=PASSWORD)
        self.assertEqual(result.fixture.organization_id, INPUTS.organization_id)
        self.assertEqual(result.catalog_sha256, f._PROFILE)
        self.assertNotEqual(BINDING.source_sha, self.owner.source_sha)
        self.assertEqual(self.owner._process_ids, {"fixture": EXPORTER})
        with self.assertRaises(f.proof.EvidenceError):
            self.owner.cleanup()
        adapter.close()
        self.assertEqual(self.owner._process_ids, {})
        self.assertIsNone(self.owner._process_manager)
        self.owner.cleanup()
        self.assertFalse(self.world.containers)
        self.assertFalse(adapter.evidence()["full_compatibility_proof"])

    def test_secrets_only_stdin_and_fixed_role_sql_before_export(self):
        adapter = self.make(); start=len(self.world.calls); adapter.run(INPUTS, password=PASSWORD)
        mutations = self.world.mutations[7:]
        self.assertEqual([v[:2] for v in mutations], [["exec", "-i"], ["container", "create"], ["container", "start"]])
        encoded = json.dumps([(a, {k:v for k,v in o.items() if k != "input_bytes"}) for a,o in self.world.calls[start:]])
        public = json.dumps(adapter.evidence())
        for secret in (INPUTS.owner_password, INPUTS.runtime_password, INPUTS.encryption_key, PASSWORD,
                       self.owner.fixture_inputs.graph_access_token):
            self.assertTrue(secret not in encoded, "private bytes in adapter command/environment"); self.assertTrue(secret not in public, "private bytes in public evidence")
        private = [o["input_bytes"] for a,o in self.world.calls if a[:2] == ["exec", "-i"]][-1]
        self.assertIn(b"NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION", private)
        self.assertNotIn(b"DROP", private)
        self.assertIn("@127.0.0.1:5432/compat?sslmode=disable", self.world.request["owner_dsn"])
        creates = [a for a,o in self.world.calls if a[:2] == ["container", "create"] and IMAGE in a]
        self.assertIn("container:" + self.owner._ids["pg"], creates[0])

    def test_image_contract_refuses_before_owned_mutation(self):
        original = copy.deepcopy(self.world.images[IMAGE])
        cases = (("Os", "windows"), ("Architecture", "arm64"), ("Id", test_stack.GRAPH))
        for key, value in cases:
            with self.subTest(key=key):
                self.world.images[IMAGE] = copy.deepcopy(original); self.world.images[IMAGE][key] = value
                with self.assertRaises(f.proof.EvidenceError): self.make()
                self.assertIsNone(self.owner._process_manager)
        for key,value in (("Cmd", f._COMMAND), ("Env", ["PGHOST=foreign"]), ("WorkingDir", "/tmp"),
                          ("User", "root"), ("Entrypoint", ["/bin/sh"]), ("Volumes", {"/x":{}})):
            with self.subTest(key=key):
                self.world.images[IMAGE] = copy.deepcopy(original); self.world.images[IMAGE]["Config"][key] = value
                with self.assertRaises(f.proof.EvidenceError): self.make()
        self.assertFalse(self.world.created_roles)

    def test_invalid_input_and_exclusive_manager(self):
        adapter = self.make()
        with self.assertRaises(f.proof.EvidenceError): self.make()
        for password in ("bad", "'"*64, True):
            with self.subTest(password_type=type(password)):
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS, password=password)
        self.assertFalse(adapter.evidence()["attempted"])
        self.assertFalse(self.world.created_roles)

    def test_existing_database_role_and_boolean_preflight_refuse(self):
        for preflight in (["empty",17,True,True,1,0], ["empty",17,True,True,0,1],
                          ["empty",17,1,True,0,0], ["empty",17,True,True,False,0], ["empty",16,True,True,0,0]):
            with self.subTest(preflight=preflight):
                self.world.preflight = preflight
                adapter = self.make()
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                self.assertFalse(self.world.created_roles)
                adapter.close()
                with self.assertRaises(f.proof.EvidenceError): self.make()
                # Separate simulated owner for the next independent negative.
                self.owner._fixture_attempted = False

    def test_attempt_persists_after_successful_close(self):
        adapter = self.make(); adapter.run(INPUTS,password=PASSWORD); adapter.close()
        with self.assertRaises(f.proof.EvidenceError): self.make()
        with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
        self.assertEqual(sum(a[:4] == ["container","start","--attach","--interactive"] for a,o in self.world.calls),1)

    def test_unknown_create_or_execution_never_retried_or_adopted(self):
        for phase in ("create", "start"):
            with self.subTest(phase=phase):
                adapter = self.make()
                self.world.unknown = lambda a: a[:2] == ["container",phase]
                with self.assertRaises(f.proof.InfrastructureFailure) as caught: adapter.run(INPUTS,password=PASSWORD)
                self.assertNotIn(PASSWORD,str(caught.exception))
                count = len(self.world.calls)
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                with self.assertRaises(f.proof.EvidenceError): adapter.close()
                self.assertEqual(len(self.world.calls),count)
                self.setUp()

    def test_nonzero_or_stderr_cannot_be_success(self):
        adapter = self.make(); self.world.exit_code = 1
        with self.assertRaisesRegex(f.proof.EvidenceError,"fixture-export-failed"): adapter.run(INPUTS,password=PASSWORD)
        self.assertFalse(adapter.evidence()["ambiguous"])
        adapter.close()

    def test_exit_state_mismatch_or_running_is_unknown(self):
        for key,value in (("ExitCode",2),("ExitCode",False),("Running",True),("StartedAt","0001-01-01T00:00:00Z")):
            with self.subTest(key=key):
                adapter = self.make()
                self.world.execute_hook = lambda w: w.containers[EXPORTER]["State"].update({key:value})
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                self.assertTrue(adapter.evidence()["ambiguous"])
                self.setUp()

    def test_stderr_and_unexpected_stdout_never_accepted(self):
        for stdout,stderr in ((lambda r: output_for(r)+b"extra",b""),(lambda r: output_for(r),b"warning")):
            with self.subTest(stderr=bool(stderr)):
                adapter=self.make(); self.world.response=stdout; self.world.stderr=stderr
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                adapter.close(); self.setUp()

    def test_pg_restart_before_mutation_refuses(self):
        adapter=self.make(); self.world.containers[self.owner._ids["pg"]]["State"]["StartedAt"]="changed"
        before=len(self.world.mutations)
        with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
        self.assertEqual(len(self.world.mutations),before)

    def test_pg_drift_after_export_cannot_accept(self):
        adapter=self.make()
        self.world.execute_hook=lambda w:w.containers[self.owner._ids["pg"]]["State"].update(StartedAt="changed")
        with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
        self.assertTrue(adapter.evidence()["ambiguous"])

    def test_created_exporter_already_started_or_extra_env_refuses_before_start(self):
        for which in ("started", "env", "network"):
            with self.subTest(which=which):
                adapter=self.make()
                def drift(args,value):
                    if args==["container","inspect",EXPORTER]:
                        if which=="started": value[0]["State"]["StartedAt"]="2026-01-01T00:00:00Z"
                        if which=="env": value[0]["Config"]["Env"].append("EXTRA=unexpected")
                        if which=="network": value[0]["HostConfig"]["NetworkMode"]="host"
                    return value
                self.world.transform=drift
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                self.assertIsNone(self.world.request)
                self.setUp()

    def test_cleanup_foreign_configuration_refuses_without_removal(self):
        adapter=self.make(); adapter.run(INPUTS,password=PASSWORD)
        self.world.containers[EXPORTER]["Config"]["Labels"][f.stack.LABEL]="foreign"
        before=len(self.world.mutations)
        with self.assertRaises(f.proof.EvidenceError): adapter.close()
        self.assertEqual(len(self.world.mutations),before)
        self.assertIn(EXPORTER,self.world.containers)

    def test_closed_output_grammar_and_identity_adversaries(self):
        request={"namespace":self.owner.namespace,"organization_id":INPUTS.organization_id,
                 "phone_number_id":"999000000000001","business_account_id":"999000000000002"}
        valid=output_for(request)
        self.assertEqual(f._parse_export(valid,request),result_for(request))
        for raw in (b"unexpected\n"+valid, valid+b"trailing", valid.replace(b"PASS",b"FAIL"),
                    valid.replace(b"COMPAT_FIXTURE_JSON:",b"COMPAT_FIXTURE_JSON:COMPAT_FIXTURE_JSON:"),
                    valid.replace(b"\x1b[0m ",b"\x1b[0m"),valid.replace(b"  0%",b"  1%"),
                    valid.replace(b"100%",b" 99%"),valid.replace(b"\nPASS\n",b"\nPASS\nPASS\n"),
                    valid.replace(b'"version":1',b'"version":1,"version":1'),b"\xff",b"x"*(f.runtime.MIB+1)):
            with self.subTest(size=len(raw)):
                with self.assertRaises(f.proof.EvidenceError): f._parse_export(raw,request)
        for key,value in (("organization_id","20000000-0000-4000-8000-000000000099"),
                          ("user_id",INPUTS.organization_id),("account_id","foreign"),("catalog_sha256","0"*64),
                          ("version",True),("phone_number_id","999000000000099"),("unexpected","value")):
            data=result_for(request); data[key]=value
            raw=valid[:valid.index(f._PREFIX.encode())]+f._PREFIX.encode()+json.dumps(data).encode()+b"\nPASS\n"
            with self.subTest(key=key):
                with self.assertRaises(f.proof.EvidenceError): f._parse_export(raw,request)

    def test_foreign_identifier_or_malformed_capture_never_adopted(self):
        for capture in (f.runtime.Capture(0,b"4"*64,b""), f.runtime.Capture(0,b"bad-id",b""),
                        f.runtime.Capture(0,b"\xff",b""), f.runtime.Capture(False,EXPORTER.encode(),b""),
                        f.runtime.Capture(0,EXPORTER,b"")):
            with self.subTest(kind=type(capture.stdout), exit_type=type(capture.returncode)):
                adapter=self.make(); self.world.create_response=capture
                with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
                self.assertTrue(adapter.evidence()["ambiguous"])
                self.assertEqual(self.owner._process_ids,{})
                self.assertIsNone(self.world.request)
                self.setUp()

    def test_finished_identity_and_oom_are_not_accepted(self):
        adapter=self.make()
        self.world.execute_hook=lambda w:w.containers[EXPORTER]["State"].update(OOMKilled=True)
        with self.assertRaises(f.proof.EvidenceError): adapter.run(INPUTS,password=PASSWORD)
        self.assertTrue(adapter.evidence()["ambiguous"])

    def test_real_local_child_private_stdio_complete_parse(self):
        request={"namespace":self.owner.namespace,"organization_id":INPUTS.organization_id,
                 "phone_number_id":"999000000000001","business_account_id":"999000000000002","password":PASSWORD}
        # Child consumes private JSON; only the strict synthetic result stream is returned.
        output=output_for(request)
        code="import sys,json; v=json.loads(sys.stdin.buffer.read()); assert len(v['password'])==64; sys.stdout.buffer.write("+repr(output)+")"
        capture=f.runtime.bounded_command([sys.executable,"-I","-S","-B","-c",code],
            env=dict(self.owner.env),timeout=10,stdout_limit=f.runtime.MIB,stderr_limit=f.runtime.MIB,input_bytes=json.dumps(request).encode())
        self.assertEqual(capture.returncode,0); self.assertFalse(capture.stderr)
        self.assertNotIn(PASSWORD.encode(),capture.stdout)
        self.assertEqual(f._parse_export(capture.stdout,request)["organization_id"],INPUTS.organization_id)


if __name__ == "__main__":
    unittest.main()
