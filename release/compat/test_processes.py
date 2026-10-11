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
        self.assertIn("default_transaction_read_only%3Don",cfg["database"]["url"])
        self.assertNotIn(INPUTS.owner_password,json.dumps(cfg))
        command=self.world.containers[h.container_id]["Config"]["Cmd"]
        self.assertEqual(command,["-ec",p._WAIT_CONFIG+"catalog-snapshot -config /compat/private/config.toml -mode compare"])
        got=m.wait_catalog(h)
        self.assertEqual(got,p.runtime.Capture(0,b"{}\n",b""))
        m.remove(h);m.close();self.owner.cleanup()

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


if __name__ == "__main__":
    unittest.main(verbosity=2)
