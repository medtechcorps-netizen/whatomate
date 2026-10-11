"""Owned process contracts under fake Docker only; no network or image build."""
from dataclasses import asdict, replace
import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import tomllib
import unittest
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))
import processes as p
import controller
import test_stack
import test_controller

PREV = p.runtime.common.IMAGE_REPOSITORY["web"] + "@sha256:" + "4" * 64
CAND, CONTROL = "sha256:" + "5" * 64, "sha256:" + "6" * 64
RECIPE = "7" * 64
INPUTS = p.ConfigInputs(*(hashlib.sha256(("synthetic-" + key).encode()).hexdigest()
                         for key in ("runtime", "owner", "jwt", "encryption")),
                       "20000000-0000-4000-8000-000000000001")


class World(test_stack.DockerWorld):
    def __init__(self):
        super().__init__()
        for reference, number, source, role in ((PREV, "4", "d" * 40, "previous"),
                                                 (CAND, "5", "e" * 40, "candidate"),
                                                 (CONTROL, "6", "f" * 40, "controller")):
            control = role == "controller"
            labels = {p.stack.SOURCE_LABEL: source, "org.opencontainers.image.source": p._SOURCE_URL,
                      "org.opencontainers.image.title": "rereply-release-web"}
            if control:
                labels.update({p.stack.ROLE_LABEL: role, p.stack.RECIPE_LABEL: RECIPE})
            self.images[reference] = {"Id": "sha256:" + number * 64, "Os": "linux", "Architecture": "amd64",
                "RepoDigests": [PREV] if role == "previous" else [],
                "Config": {"Env": [p.stack._GRAPH_DEFAULT_PATH], "Labels": labels,
                    "Entrypoint": ["python3", "-I", "-B", "/compat/controller.py"] if control else ["./rereply"],
                    "Cmd": None if control else ["server", "-config", "config.toml"],
                    "User": "65532:65532" if control else "rereply", "WorkingDir": "/compat" if control else "/app"}}
        self.configs = {}
        self.response = None
        self.response_exit = 0
        self.raise_secret = False
        self.catalog_response = b"{}\n"
        self.catalog_stderr = b""
        self.catalog_exit = 0

    @staticmethod
    def flags(args, start):
        values = {}
        i = start
        while i < len(args) and args[i].startswith("--"):
            flag = args[i]
            if flag in ("--internal", "--read-only", "--interactive"):
                value, i = True, i + 1
            else:
                value, i = args[i + 1], i + 2
            values.setdefault(flag, []).append(value)
        return values, args[i:]

    def capture(self, argv, **options):
        args = argv[5:]
        if self.raise_secret and args[:2] == ["container", "create"]:
            raise OSError("secret hostname " + INPUTS.owner_password)
        if args[:2] == ["exec", "-i"]:
            self.calls.append((list(args), options))
            self.mutations.append(list(args))
            assert args[2:4] == ["--user", "rereply"] and args[5:] == ["/bin/sh", "-ec", p._WRITE_CONFIG]
            self.configs[args[4]] = tomllib.loads(options["input_bytes"].decode())
            if self.containers[args[4]]["Config"]["Labels"][p.stack.ROLE_LABEL] in {"migration","catalog"}:
                self.containers[args[4]]["State"].update(Running=False, ExitCode=self.catalog_exit if self.containers[args[4]]["Config"]["Labels"][p.stack.ROLE_LABEL]=="catalog" else 0)
            return p.runtime.Capture(0, b"", b"")
        if args[:4] == ["container", "start", "--attach", "--interactive"]:
            self.calls.append((list(args), options)); self.mutations.append(list(args))
            self.containers[args[4]]["State"].update(Running=False, StartedAt="2026-10-11T00:00:01Z", ExitCode=self.response_exit)
            raw = self.response if self.response is not None else json.dumps({"version": 1, "status": "ok",
                 "operation": "readiness", "ready": {"web": True, "graph": True},
                 'stub_receipts':test_controller.receipts('readiness')}).encode()
            return p.runtime.Capture(self.response_exit, raw, b"")
        if args[:1] == ["logs"]:
            self.calls.append((list(args),options))
            assert self.containers[args[1]]["Config"]["Labels"][p.stack.ROLE_LABEL]=="catalog"
            return p.runtime.Capture(0,self.catalog_response,self.catalog_stderr)
        if args[:2] == ["container", "wait"]:
            self.calls.append((list(args), options))
            return p.runtime.Capture(0, str(self.containers[args[2]]["State"]["ExitCode"]).encode(), b"")
        result = super().capture(argv, **options)
        if args[:2] == ["container", "create"] and result.returncode == 0:
            old = result.stdout.decode()
            if old not in self.containers:
                return result
            identifier = hashlib.sha256(("owned-" + str(self.counter)).encode()).hexdigest()
            row = self.containers.pop(old);row["Id"] = identifier
            self.containers[identifier] = row
            self.network["Containers"][identifier] = self.network["Containers"].pop(old)
            flags, _ = self.flags(args, 2)
            row["Config"]["WorkingDir"] = flags.get("--workdir", [row["Config"].get("WorkingDir", "")])[0]
            row["Config"]["OpenStdin"] = bool(flags.get("--interactive"))
            if "--entrypoint" in flags:
                row["Config"]["Entrypoint"] = flags["--entrypoint"]
            row["RestartCount"] = 0
            if row["Config"]["Labels"][p.stack.ROLE_LABEL]=="catalog":
                row["Config"]["Tty"]=False
                row["HostConfig"]["LogConfig"]={"Type":"json-file","Config":{}}
            return p.runtime.Capture(0, identifier.encode(), b"")
        return result



def rollback_bindings(world, source_sha=test_stack.SOURCE):
    """Synthetic inspected T/L images and separate local H web/control image."""
    live_ref = p.runtime.common.IMAGE_REPOSITORY["web"] + "@" + CAND
    world.images[live_ref] = copy.deepcopy(world.images[CAND])
    world.images[live_ref]["RepoDigests"] = [live_ref]
    local = "sha256:" + "8" * 64
    world.images[local] = copy.deepcopy(world.images[CAND])
    world.images[local]["Id"] = local
    world.images[local]["Config"]["Labels"].update({p.stack.SOURCE_LABEL: source_sha,
        p.stack.ROLE_LABEL: "candidate", p.stack.RECIPE_LABEL: RECIPE})
    world.images[CONTROL]["Config"]["Labels"][p.stack.SOURCE_LABEL] = source_sha
    return (p.ImageBinding(PREV, "sha256:" + "4" * 64, "d" * 40, RECIPE),
            p.ImageBinding(live_ref, CAND, "e" * 40, RECIPE),
            p.ImageBinding(CONTROL, CONTROL, source_sha, RECIPE),
            p.ImageBinding(local, local, source_sha, RECIPE))

class ProcessTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.world = World()
        self.owner = p.stack.OwnedStack("docker", Path(self.tmp.name), test_stack.SOURCE,
            p.stack.GraphImage(test_stack.GRAPH, test_stack.SOURCE), token=test_stack.TOKEN, capture=self.world.capture)
        self.owner.start()
        self.bindings = (p.ImageBinding(PREV, "sha256:" + "4" * 64, "d" * 40, RECIPE),
                         p.ImageBinding(CAND, CAND, "e" * 40, RECIPE),
                         p.ImageBinding(CONTROL, CONTROL, "f" * 40, RECIPE))

    def manager(self):
        return p.OwnedProcesses(self.owner, *self.bindings)

    def test_optional_catalog_control_preserves_three_normal_roles(self):
        manager = self.manager()
        self.assertEqual(tuple(manager.bindings), ("previous", "candidate", "controller"))
        self.assertIs(manager.catalog_binding, manager.bindings["candidate"])
        self.assertEqual(manager.evidence_source_sha, "e" * 40)
        handle = manager.start_catalog(INPUTS)
        self.assertEqual(handle.version, "candidate")
        self.assertEqual(handle.image_id, CAND)
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_separate_h_control_routes_only_catalog_and_evidence(self):
        previous, live, controller, control = rollback_bindings(self.world)
        manager = p.OwnedProcesses(self.owner, previous, live, controller, catalog_control=control)
        self.assertIs(manager.catalog_binding, control)
        self.assertEqual(manager.evidence_source_sha, self.owner.source_sha)
        for phase, image in (("compat-fwd", live), ("compat-n1-rw", previous), ("compat-n1-ro", previous)):
            handle = manager.start_migration(phase, INPUTS)
            self.assertEqual(handle.image_id, image.image_id)
            self.assertEqual(self.world.containers[handle.container_id]["Config"]["Cmd"],
                ["-ec", p._WAIT_CONFIG + "rls-migrate -config /compat/private/config.toml"])
            self.assertEqual(manager.wait_migration(handle), 0)
            manager.remove(handle)
        for version, image in (("candidate", live), ("previous", previous)):
            handle = manager.start_web(version, INPUTS)
            self.assertEqual(handle.image_id, image.image_id)
            with self.assertRaises(p.proof.EvidenceError): manager.start_catalog(INPUTS)
            manager.remove(handle)
        handle = manager.start_catalog(INPUTS)
        self.assertEqual((handle.version, handle.image_id), ("catalog-control", control.image_id))
        self.assertEqual(self.world.containers[handle.container_id]["Config"]["Cmd"],
            ["-ec", p._WAIT_CONFIG + "catalog-snapshot -config /compat/private/config.toml -mode compare"])
        self.assertNotIn(INPUTS.owner_password, json.dumps(self.world.configs[handle.container_id]))
        self.assertEqual(manager.wait_catalog(handle).returncode, 0)
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_explicit_control_source_reference_layout_and_recipe_fail_before_creation(self):
        previous, live, controller, control = rollback_bindings(self.world)
        original = copy.deepcopy(self.world.images[control.reference])
        for failure in ("source", "controller", "signed", "role", "recipe", "entrypoint", "environment"):
            with self.subTest(failure=failure):
                self.world.images[control.reference] = copy.deepcopy(original)
                chosen, driver = control, controller
                row = self.world.images[control.reference]
                if failure == "source": chosen = replace(control, source_sha="1" * 40)
                elif failure == "controller": driver = replace(controller, source_sha="1" * 40)
                elif failure == "signed": chosen = replace(control, reference=live.reference)
                elif failure == "role": row["Config"]["Labels"][p.stack.ROLE_LABEL] = "catalog-control"
                elif failure == "recipe": del row["Config"]["Labels"][p.stack.RECIPE_LABEL]
                elif failure == "entrypoint": row["Config"]["Entrypoint"] = ["/bin/sh"]
                else: row["Config"]["Env"] = ["SECRET=private"]
                count = len(self.world.mutations)
                with self.assertRaises(p.proof.EvidenceError):
                    p.OwnedProcesses(self.owner, previous, live, driver, catalog_control=chosen)
                self.assertEqual(len(self.world.mutations), count)
                self.assertIsNone(self.owner._process_manager)

    def frame(self, operation="readiness", **extra):
        f = test_controller.fixture()
        f = replace(f, namespace=self.owner.namespace,
                    product_origin=self.owner.planned_web_origin, stub_origin=self.owner.graph_origin,
                    account_name=self.owner.namespace + "-wa", email=self.owner.namespace + "-client@example.test")
        return json.dumps({"version": 1, "operation": operation, "fixture": asdict(f), **extra}).encode()

    def test_catalog_fixed_runtime_readonly_command_and_complete_exit(self):
        m=self.manager(); h=m.start_catalog(INPUTS)
        cfg=self.world.configs[h.container_id]
        self.assertEqual(set(cfg),{"app","database"})
        self.assertEqual(set(cfg["database"]),{"url","runtime_role","rls_enabled"})
        self.assertIn("compat_runtime:"+INPUTS.runtime_password,cfg["database"]["url"])
        self.assertIn("application_name=compat-control",cfg["database"]["url"])
        self.assertNotIn("options",parse_qs(urlsplit(cfg["database"]["url"]).query))
        self.assertNotIn(INPUTS.owner_password,json.dumps(cfg))
        command=self.world.containers[h.container_id]["Config"]["Cmd"]
        self.assertEqual(command,["-ec",p._WAIT_CONFIG+"catalog-snapshot -config /compat/private/config.toml -mode compare"])
        got=m.wait_catalog(h)
        self.assertEqual(got,p.runtime.Capture(0,b"{}\n",b""))
        m.remove(h);m.close();self.owner.cleanup()

    def test_catalog_and_n1_ro_have_distinct_startup_parameter_contracts(self):
        m = self.manager()
        catalog = m.start_catalog(INPUTS)
        db = self.world.configs[catalog.container_id]["database"]
        # The catalog collector rejects DSN options and installs its own
        # read-only startup parameters before connecting.
        self.assertEqual(parse_qs(urlsplit(db["url"]).query), {
            "sslmode": ["disable"], "application_name": ["compat-control"],
        })
        self.assertNotIn("migration_url", db)
        self.assertEqual(m.wait_catalog(catalog).returncode, 0)
        m.remove(catalog)

        migration = m.start_migration("compat-n1-ro", INPUTS)
        db = self.world.configs[migration.container_id]["database"]
        for key, role in (("url", "compat_runtime"), ("migration_url", "compat_owner")):
            with self.subTest(connection=key):
                parsed = urlsplit(db[key])
                self.assertEqual(parsed.username, role)
                self.assertEqual(parse_qs(parsed.query), {
                    "sslmode": ["disable"], "application_name": ["compat-n1-ro"],
                    "options": ["-c default_transaction_read_only=on"],
                })
        self.assertEqual(m.wait_migration(migration), 0)
        m.remove(migration);m.close();self.owner.cleanup()

    def test_catalog_slot_excludes_web_migration_and_controller(self):
        m=self.manager(); h=m.start_web("previous",INPUTS); before=len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.start_catalog(INPUTS)
        self.assertEqual(len(self.world.mutations),before);m.remove(h)
        cat=m.start_catalog(INPUTS);before=len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.start_web("candidate",INPUTS)
        with self.assertRaises(p.proof.EvidenceError):m.start_migration("compat-fwd",INPUTS)
        with self.assertRaises(p.proof.EvidenceError):m._create("controller","controller","compat-control")
        self.assertEqual(len(self.world.mutations),before)
        m.remove(cat)

    def test_catalog_rotating_or_non_json_logs_refuse(self):
        m=self.manager();h=m.start_catalog(INPUTS)
        for log in ({"Type":"local","Config":{}},{"Type":"json-file","Config":{"max-size":"1m"}},None):
            self.world.containers[h.container_id]["HostConfig"]["LogConfig"]=log
            with self.assertRaises(p.proof.EvidenceError):m.wait_catalog(h)

    def test_catalog_wait_preserves_nonzero_stderr_and_rejects_false_exit(self):
        m=self.manager();self.world.catalog_exit=2;self.world.catalog_stderr=b"fixed-failure"
        h=m.start_catalog(INPUTS);got=m.wait_catalog(h)
        self.assertEqual(got.returncode,2);self.assertEqual(got.stderr,b"fixed-failure")
        self.world.containers[h.container_id]["State"]["ExitCode"]=False
        with self.assertRaises(p.proof.EvidenceError):m.wait_catalog(h)

    def test_owned_versions_swap_same_alias_and_exact_cleanup(self):
        m = self.manager()
        first = m.start_web("previous", INPUTS)
        self.assertEqual(self.owner._process_ids, {"web": first.container_id})
        count = len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.start_web("candidate", INPUTS)
        with self.assertRaises(p.proof.EvidenceError):self.owner.cleanup()
        self.assertEqual(len(self.world.mutations), count)
        m.remove(first)
        second = m.start_web("candidate", INPUTS)
        self.assertNotEqual(first.container_id, second.container_id)
        for key in (first.container_id, second.container_id):
            cfg = self.world.configs[key]
            self.assertTrue(cfg["database"]["rls_enabled"])
            self.assertNotIn("migration_url", cfg["database"])
            self.assertNotIn(INPUTS.owner_password, json.dumps(cfg))
            self.assertEqual(cfg["whatsapp"]["base_url"], self.owner.graph_origin)
        self.assertIn("application_name=compat-prev-server", self.world.configs[first.container_id]["database"]["url"])
        self.assertIn("application_name=compat-cand-server", self.world.configs[second.container_id]["database"]["url"])
        self.assertFalse(any(args[0] in {"logs", "wait"} for args, _ in self.world.calls))
        m.close();self.owner.cleanup()
        self.assertFalse(self.world.containers);self.assertIsNone(self.world.network)

    def test_migration_phase_roles_readonly_and_actual_exit(self):
        m = self.manager()
        for phase in p._PHASES:
            h = m.start_migration(phase, INPUTS)
            cfg = self.world.configs[h.container_id]
            self.assertIn("compat_owner:" + INPUTS.owner_password, cfg["database"]["migration_url"])
            self.assertIn("application_name=" + phase, cfg["database"]["migration_url"])
            self.assertEqual("default_transaction_read_only" in cfg["database"]["migration_url"], phase == "compat-n1-ro")
            self.assertEqual(m.wait_migration(h), 0)
            m.remove(h)
        m.close();self.owner.cleanup()

    def test_running_web_refuses_migration_without_mutation(self):
        m = self.manager();m.start_web("candidate", INPUTS);count = len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.start_migration("compat-fwd", INPUTS)
        self.assertEqual(len(self.world.mutations), count)

    def test_controller_private_stdin_and_remove_before_return(self):
        m = self.manager();m.start_web("previous", INPUTS)
        response = json.loads(m.controller(self.frame()))
        self.assertEqual(response["ready"], {"web": True, "graph": True})
        self.assertEqual(set(self.owner._process_ids), {"web"})
        private = [opts for args, opts in self.world.calls if args[:4] == ["container", "start", "--attach", "--interactive"]]
        self.assertEqual(len(private), 1)
        self.assertEqual(private[0]["input_bytes"], self.frame())
        self.assertEqual(private[0]["timeout"], 30)
        m.close();self.owner.cleanup()

    def test_only_two_fixed_ring_reads_allowed_without_web_and_audits_private(self):
        m=self.manager()
        for operation in ('journal','rejected'):
            window={'after':0,'cursor':0,'discard_baseline':False,'entries':[],'requests':1}
            response={'version':1,'status':'ok','operation':operation,operation:window,
                      'stub_receipts':test_controller.receipts(operation,window)}
            self.world.response=json.dumps(response).encode()
            self.assertEqual(json.loads(m.controller(self.frame(operation,after=0,discard_baseline=False))),response)
        private=m.controller_receipts()
        self.assertIsInstance(private,tuple);self.assertTrue(all(type(v) is bytes for v in private))
        self.assertEqual(len(private),2)
        self.assertNotIn('receipt',json.dumps(m.evidence()))
        count=len(self.world.mutations)
        for frame in (self.frame(),self.frame('setup',business_account_id=test_controller.BUSINESS),
                      self.frame('write',nonce=test_controller.test_sweep.NONCE)):
            with self.assertRaises(p.proof.EvidenceError):m.controller(frame)
        self.assertEqual(len(self.world.mutations),count)
        m.close();self.owner.cleanup()

    def test_same_namespace_foreign_loopback_origin_refuses_before_docker(self):
        m=self.manager();value=json.loads(self.frame('journal',after=0,discard_baseline=False))
        value['fixture']['stub_origin']='http://127.0.0.1:8090'
        count=len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.controller(json.dumps(value).encode())
        self.assertEqual(len(self.world.mutations),count)

    def test_controller_setup_private_protocol_has_own_deadline_not_sweep_flags(self):
        m = self.manager();m.start_web("previous", INPUTS)
        response = {"version": 1, "status": "ok", "operation": "setup", "registered": True,
                    'stub_receipts':test_controller.receipts('setup')}
        self.world.response = json.dumps(response).encode()
        request = self.frame("setup", business_account_id=test_controller.BUSINESS)
        self.assertEqual(json.loads(m.controller(request)), response)
        calls = [opts for args, opts in self.world.calls if args[:4] == ["container", "start", "--attach", "--interactive"]]
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["input_bytes"], request)
        self.assertEqual(calls[0]["timeout"], 100)
        self.assertNotIn("controller", self.owner._process_ids)
        self.assertFalse(m.evidence()["controller_protocol_failed"])
        self.assertNotIn(test_controller.BUSINESS, json.dumps(m.evidence()))
        m.close();self.owner.cleanup()

    def test_controller_setup_malformed_request_refuses_before_docker(self):
        m = self.manager();m.start_web("candidate", INPUTS);count = len(self.world.calls)
        for request in (self.frame("setup"), self.frame("setup", business_account_id="outside"),
                        self.frame("setup", business_account_id=test_controller.BUSINESS, payload={} )):
            with self.assertRaises(p.proof.EvidenceError):m.controller(request)
        self.assertEqual(len(self.world.calls), count)

    def test_controller_journal_exact_cursor_contract_and_deadline(self):
        m = self.manager(); m.start_web("previous", INPUTS)
        journal = {"after": 8, "cursor": 9, "discard_baseline": False, "entries": [{"seq": 9}], "requests": 1}
        response = {"version": 1, "status": "ok", "operation": "journal", "journal": journal,
                    'stub_receipts':test_controller.receipts('journal',journal)}
        self.world.response = json.dumps(response).encode()
        request = self.frame("journal", after=8, discard_baseline=False)
        self.assertEqual(json.loads(m.controller(request)), response)
        invoked = [opts for args, opts in self.world.calls if args[:4] == ["container", "start", "--attach", "--interactive"]]
        self.assertEqual(invoked[-1]["timeout"], 190)
        self.assertNotIn("cases", response)
        self.assertFalse(m._protocol_failed)
        m.close(); self.owner.cleanup()

    def test_controller_journal_foreign_window_latches_and_preserves_cleanup(self):
        m = self.manager(); m.start_web("previous", INPUTS)
        self.world.response = json.dumps({"version": 1, "status": "ok", "operation": "journal", "journal":
            {"after": 0, "cursor": 8, "discard_baseline": True, "entries": [{"seq": 8}], "requests": 1}}).encode()
        with self.assertRaises(p.proof.EvidenceError): m.controller(self.frame("journal", after=7, discard_baseline=False))
        self.assertTrue(m._protocol_failed)
        m.close(); self.owner.cleanup()

    def test_controller_setup_invalid_ack_latches_without_favorable_case_flags(self):
        for change in ({"registered": 1}, {"registered": False}, {"cases": {"flow": True}}):
            with self.subTest(change=change):
                # Each malformed result gets a separate manager: no replay after
                # the latch, and exact known-resource cleanup remains available.
                m = self.manager();m.start_web("candidate", INPUTS)
                response = {"version": 1, "status": "ok", "operation": "setup", "registered": True, **change}
                self.world.response = json.dumps(response).encode()
                with self.assertRaises(p.proof.EvidenceError):
                    m.controller(self.frame("setup", business_account_id=test_controller.BUSINESS))
                self.assertTrue(m.evidence()["controller_protocol_failed"])
                count = len(self.world.calls)
                with self.assertRaises(p.proof.EvidenceError):m.controller(self.frame())
                self.assertEqual(len(self.world.calls), count)
                self.assertNotIn("controller", self.owner._process_ids)
                m.close()
                self.owner._process_manager = None

    def test_controller_write_and_read_retain_private_records_and_false_cases(self):
        m = self.manager();m.start_web("candidate", INPUTS)
        fixture = controller._fixture(json.loads(self.frame())["fixture"])
        retained = replace(test_controller.record(), fixture_binding=controller.sweep.Sweep(fixture)._binding())
        flags = {key: key != "flow" for key in controller.sweep.proof.SWEEP_CASES}
        for operation in ("write", "read"):
            response = {"version": 1, "status": "ok", "operation": operation, "cases": flags,
                        'stub_receipts':test_controller.receipts(operation)}
            if operation == "write":response["record"] = asdict(retained)
            self.world.response = json.dumps(response).encode()
            raw = self.frame(operation, **({"nonce": retained.nonce} if operation == "write"
                                           else {"record": asdict(retained)}))
            self.assertEqual(json.loads(m.controller(raw)), response)
            self.assertFalse(m.evidence()["controller_protocol_failed"])
            self.assertNotIn("controller", self.owner._process_ids)
        self.assertNotIn(retained.contact_id, json.dumps(m.evidence()))
        m.close();self.owner.cleanup()

    def test_actual_completed_read_false_uses_normal_ack_error_still_latches(self):
        m = self.manager(); m.start_web("previous", INPUTS)
        api = test_controller.test_sweep.World()
        api.f = controller._fixture(json.loads(self.frame())["fixture"])
        retained = api.client().write(test_controller.test_sweep.NONCE)
        api.messages[retained.conversation_id][0]["message"]["content"] = "changed"
        def factory(value):
            return controller.sweep.Sweep(value, product=api.transport(), stub=api.transport(True),
                                          wall=lambda: test_controller.test_sweep.WALL, sleep=lambda _: None)
        frame = self.frame("read", record=asdict(retained))
        self.world.response = controller.handle(frame, sweep_factory=factory)
        result = json.loads(m.controller(frame))
        self.assertEqual(result["cases"], {key: key != "inbound" for key in p.proof.SWEEP_CASES})
        self.assertFalse(m.evidence()["controller_protocol_failed"])
        self.assertEqual(json.loads(m.controller_receipts()[-1])["operation"], "read")
        api.intercept = lambda method, path, body: controller.sweep.Reply(404, (), b'private') if path == "/api/chatbot/settings" else None
        self.world.response = controller.handle(frame, sweep_factory=factory)
        self.world.response_exit = 1
        with self.assertRaises(controller.sweep.SemanticFailure): m.controller(frame)
        self.assertTrue(m.evidence()["controller_protocol_failed"])
        count = len(self.world.calls)
        with self.assertRaises(p.proof.EvidenceError): m.controller(frame)
        self.assertEqual(len(self.world.calls), count)
        m.close(); self.owner.cleanup()
        self.assertFalse(self.world.containers)

    def test_controller_malformed_request_refuses_before_creation(self):
        m = self.manager();m.start_web("candidate", INPUTS);count = len(self.world.mutations)
        raw = json.loads(self.frame());raw["fixture"]["product_origin"] = "https://outside.example"
        for value in (b"NaN", json.dumps(raw).encode(), self.frame().replace(b"rereply-compat-bbbbbbbbbbbb", b"rereply-compat-cccccccccccc")):
            with self.assertRaises(p.proof.EvidenceError):m.controller(value)
        self.assertEqual(len(self.world.mutations), count)

    def test_controller_fixed_failures_remove_resource_and_raise(self):
        m = self.manager();m.start_web("candidate", INPUTS)
        self.world.response = b'{"version":1,"status":"error","failure":"semantic"}'
        self.world.response_exit = 1
        with self.assertRaises(controller.sweep.SemanticFailure):m.controller(self.frame())
        self.assertNotIn("controller", self.owner._process_ids)
        self.assertFalse(m.evidence()["ambiguous"])

    def test_controller_missing_or_fabricated_fields_never_become_flags(self):
        m = self.manager();m.start_web("candidate", INPUTS)
        self.world.response = b'{"version":1,"status":"ok","operation":"readiness","ready":{"web":1,"graph":true}}'
        with self.assertRaises(p.proof.EvidenceError):m.controller(self.frame())
        self.assertNotIn("controller", self.owner._process_ids)

    def test_invalid_controller_response_blocks_new_work_but_allows_cleanup(self):
        m = self.manager();m.start_web("candidate", INPUTS)
        self.world.response = b'{"version":1,"status":"ok","operation":"readiness"}'
        with self.assertRaises(p.proof.EvidenceError):m.controller(self.frame())
        count = len(self.world.calls)
        self.assertTrue(m.evidence()["controller_protocol_failed"])
        for action in (lambda: m.controller(self.frame()),
                       lambda: m.start_web("previous", INPUTS),
                       lambda: m.start_migration("compat-n1-rw", INPUTS)):
            with self.assertRaises(p.proof.EvidenceError):action()
        self.assertEqual(len(self.world.calls), count)
        m.close();self.owner.cleanup()
        self.assertFalse(self.world.containers);self.assertIsNone(self.world.network)

    def test_images_require_exact_source_recipe_and_no_baked_secrets(self):
        cases = ((CONTROL, lambda r: r["Config"]["Labels"].update({p.stack.RECIPE_LABEL: "0" * 64})),
                 (CAND, lambda r: r["Config"]["Labels"].update({p.stack.SOURCE_LABEL: "0" * 40})),
                 (PREV, lambda r: r.update(RepoDigests=[])),
                 (CAND, lambda r: r["Config"].update(Env=["DATABASE_URL=outside"])),
                 (CONTROL, lambda r: r["Config"].update(Env=["PYTHONPATH=/outside"])),
                 (CAND, lambda r: r["Config"].update(User="root")))
        count = len(self.world.mutations)
        for ref, change in cases:
            original = copy.deepcopy(self.world.images[ref]);change(self.world.images[ref])
            with self.subTest(ref=ref), self.assertRaises(p.proof.EvidenceError):self.manager()
            self.world.images[ref] = original
        self.assertEqual(len(self.world.mutations), count)

    def test_duplicate_manager_refuses(self):
        self.manager();count = len(self.world.calls)
        with self.assertRaises(p.proof.EvidenceError):self.manager()
        self.assertEqual(len(self.world.calls), count)

    def test_unknown_create_poison_blocks_parent_cleanup_even_without_id(self):
        m = self.manager();self.world.fail = lambda args: args[:2] == ["container", "create"]
        with self.assertRaises(p.proof.InfrastructureFailure):m.start_web("previous", INPUTS)
        count = len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.close()
        with self.assertRaises(p.proof.EvidenceError):self.owner.cleanup()
        self.assertTrue(m.evidence()["ambiguous"])
        self.assertEqual(len(self.world.mutations), count)

    def test_unknown_stop_preserves_ids_no_retry(self):
        m = self.manager();handle = m.start_web("previous", INPUTS)
        self.world.fail = lambda args: args[:2] == ["container", "stop"]
        with self.assertRaises(p.proof.InfrastructureFailure):m.remove(handle)
        count = len(self.world.mutations)
        with self.assertRaises(p.proof.EvidenceError):m.remove(handle)
        with self.assertRaises(p.proof.EvidenceError):self.owner.cleanup()
        self.assertEqual(len(self.world.mutations), count)
        self.assertEqual(self.owner._process_ids["web"], handle.container_id)

    def test_foreign_peer_and_process_drift_refuse_before_mutation(self):
        m = self.manager();handle = m.start_web("previous", INPUTS);count = len(self.world.mutations)
        self.world.network["Containers"]["f" * 64] = {}
        with self.assertRaises(p.proof.EvidenceError):m.remove(handle)
        del self.world.network["Containers"]["f" * 64]
        self.world.containers[handle.container_id]["Config"]["Env"].append("EXTRA=outside")
        with self.assertRaises(p.proof.EvidenceError):m.remove(handle)
        self.assertEqual(len(self.world.mutations), count)

    def test_secrets_only_config_stdin_no_argv_environment_or_public_evidence(self):
        m = self.manager();m.start_web("candidate", INPUTS)
        public = json.dumps(m.evidence()) + json.dumps(self.owner.evidence()) + repr(INPUTS)
        for args, opts in self.world.calls:
            public += json.dumps(args) + json.dumps(opts["env"])
        for secret in (INPUTS.runtime_password, INPUTS.owner_password, INPUTS.jwt_secret, INPUTS.encryption_key):
            self.assertTrue(secret not in public)
        self.assertTrue(any(INPUTS.runtime_password.encode() in (opts.get("input_bytes") or b"") for _,opts in self.world.calls))

    def test_transport_exception_text_is_not_disclosed(self):
        m = self.manager();self.world.raise_secret = True
        with self.assertRaises(p.proof.InfrastructureFailure) as caught:m.start_web("previous", INPUTS)
        self.assertNotIn(INPUTS.owner_password, str(caught.exception))
        self.assertNotIn("hostname", str(caught.exception))


class NullableBaselineProcessTests(unittest.TestCase):
    setUp = ProcessTests.setUp

    def manager(self):
        previous, live, controller, control = rollback_bindings(self.world)
        return p.OwnedProcesses(self.owner, previous, live, controller, catalog_control=control)

    def test_fixed_h_command_and_runtime_only_configuration(self):
        manager = self.manager()
        handle = manager.start_nullable_baseline(INPUTS)
        row = self.world.containers[handle.container_id]
        self.assertEqual(row["Config"]["Cmd"], ["-ec", p._WAIT_CONFIG +
            "catalog-snapshot -config /compat/private/config.toml -mode nullable-v2-baseline"])
        self.assertEqual(handle.image_id, manager.catalog_binding.image_id)
        self.assertNotEqual(handle.image_id, manager.bindings["candidate"].image_id)
        db = self.world.configs[handle.container_id]["database"]
        self.assertEqual(set(db), {"url", "runtime_role", "rls_enabled"})
        self.assertEqual(urlsplit(db["url"]).username, "compat_runtime")
        self.assertNotIn(INPUTS.owner_password, json.dumps(db))
        manager.wait_nullable_baseline(handle)
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_only_fixed_graph_log_gets_four_mib_other_operations_stay_one(self):
        manager = self.manager()
        self.world.catalog_response = b"x" * (p.runtime.MIB + 1)
        handle = manager.start_nullable_baseline(INPUTS)
        result = manager.wait_nullable_baseline(handle)
        self.assertEqual(result.stdout, self.world.catalog_response)
        manager.remove(handle); manager.close(); self.owner.cleanup()
        graph_logs = [(args, options) for args, options in self.world.calls if args[:1] == ["logs"]]
        self.assertEqual(len(graph_logs), 1)
        self.assertEqual(graph_logs[0][1]["stdout_limit"], 4*p.runtime.MIB)
        self.assertEqual(graph_logs[0][1]["stderr_limit"], p.runtime.MIB)
        for args, options in self.world.calls:
            if args[:1] != ["logs"]:
                self.assertEqual(options["stdout_limit"], p.runtime.MIB)
                self.assertEqual(options["stderr_limit"], p.runtime.MIB)

    def test_graph_compare_handle_confusion_and_copy_refuse_before_logs(self):
        manager = self.manager(); ordinary = manager.start_catalog(INPUTS)
        with self.assertRaises(p.proof.EvidenceError): manager.wait_nullable_baseline(ordinary)
        self.assertFalse(any(args[:1] == ["logs"] for args, _ in self.world.calls))
        manager.remove(ordinary)
        handle = manager.start_nullable_baseline(INPUTS)
        for callback in (lambda: manager.wait_catalog(handle),
                         lambda: manager.wait_nullable_baseline(replace(handle))):
            with self.assertRaises(p.proof.EvidenceError): callback()
        self.assertFalse(any(args[:1] == ["logs"] for args, _ in self.world.calls))
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_both_stream_caps_are_enforced_and_no_private_echo(self):
        manager = self.manager(); handle = manager.start_nullable_baseline(INPUTS)
        for stdout, stderr in ((b"x"*(4*p.runtime.MIB+1), b""),
                               (b"{}", b"private"+b"x"*p.runtime.MIB)):
            self.world.catalog_response, self.world.catalog_stderr = stdout, stderr
            with self.assertRaisesRegex(p.proof.EvidenceError, "catalog-log-incomplete") as caught:
                manager.wait_nullable_baseline(handle)
            self.assertNotIn("private", str(caught.exception))
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_actual_nonzero_and_stderr_are_preserved_without_success_verdict(self):
        manager = self.manager(); self.world.catalog_exit = 1
        self.world.catalog_stderr = b"catalog-snapshot:capture-failed\n"
        handle = manager.start_nullable_baseline(INPUTS)
        result = manager.wait_nullable_baseline(handle)
        self.assertEqual((result.returncode, result.stderr), (1, self.world.catalog_stderr))
        manager.remove(handle); manager.close(); self.owner.cleanup()

    def test_h_image_source_recipe_or_inspected_image_drift_refuses(self):
        manager = self.manager(); binding = manager.catalog_binding
        for field, value in (("source_sha", "b"*40), ("recipe_sha256", "0"*64)):
            manager.bindings["catalog-control"] = replace(binding, **{field: value})
            before = len(self.world.mutations)
            with self.assertRaises(p.proof.EvidenceError): manager.start_nullable_baseline(INPUTS)
            self.assertEqual(len(self.world.mutations), before)
        manager.bindings["catalog-control"] = binding
        self.world.images[binding.reference]["Config"]["Labels"][p.stack.RECIPE_LABEL] = "0"*64
        with self.assertRaises(p.proof.EvidenceError): manager.start_nullable_baseline(INPUTS)

    def test_active_peer_and_unsafe_stopped_state_refuse(self):
        manager = self.manager(); peer = manager.start_web("previous", INPUTS)
        with self.assertRaises(p.proof.EvidenceError): manager.start_nullable_baseline(INPUTS)
        manager.remove(peer); handle = manager.start_nullable_baseline(INPUTS)
        row = self.world.containers[handle.container_id]
        saved = copy.deepcopy(row)
        for mutate in (lambda r: r["HostConfig"].update(LogConfig={"Type":"local","Config":{}}),
                       lambda r: r["State"].update(OOMKilled=True),
                       lambda r: r.update(RestartCount=1),
                       lambda r: r["Config"].update(Tty=True),
                       lambda r: r["State"].update(ExitCode=False)):
            self.world.containers[handle.container_id] = copy.deepcopy(saved)
            mutate(self.world.containers[handle.container_id])
            with self.assertRaises(p.proof.EvidenceError): manager.wait_nullable_baseline(handle)

    def test_capture_deadline_failure_is_not_retried(self):
        manager = self.manager(); handle = manager.start_nullable_baseline(INPUTS)
        original = self.owner.capture; attempts = []
        def expired(argv, **options):
            if argv[5:6] == ["logs"]:
                attempts.append(tuple(argv))
                raise p.proof.InfrastructureFailure("experiment-deadline")
            return original(argv, **options)
        self.owner.capture = expired
        with self.assertRaisesRegex(p.proof.InfrastructureFailure, "experiment-deadline"):
            manager.wait_nullable_baseline(handle)
        self.assertEqual(len(attempts), 1)
        self.owner.capture = original
        manager.remove(handle); manager.close(); self.owner.cleanup()


if __name__ == "__main__":
    unittest.main(verbosity=2)
