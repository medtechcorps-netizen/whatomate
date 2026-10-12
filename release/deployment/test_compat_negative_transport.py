"""Controlled captures only; no GitHub calls or installed negative-index trust."""
from dataclasses import replace
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import compat_negative_index as policy
import compat_outage_policy as outage_loader
import compat_negative_transport as transport
import record_transport as rt
import test_compat_negative_index as fixtures


def raw(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode() + b"\n"


def http(status, value):
    body = value if type(value) is bytes else raw(value)
    return (f"HTTP/2.0 {status}\r\nContent-Type: application/json\r\nContent-Length: {len(body)}\r\n\r\n".encode() + body)


def ref_body(ref, sha):
    return {"ref": ref, "node_id": "synthetic-ref-node", "url": "https://api.github.com" + policy.API + "/git/" + ref,
            "object": {"type": "commit", "sha": sha,
                       "url": "https://api.github.com" + policy.API + "/git/commits/" + sha}}


# Exact public stdout from H4 synthetic run 38217227182, command 19.
# Creation occurred; the run refused before marker readback/signing.
# Replaying these bytes locally is parser coverage, not hosted qualification.
ACTUAL_H4_MARKER_CREATED = b'HTTP/2.0 201 Created\nAccess-Control-Allow-Origin: *\r\nAccess-Control-Expose-Headers: ETag, Link, Location, Retry-After, X-GitHub-OTP, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Used, X-RateLimit-Resource, X-RateLimit-Reset, X-OAuth-Scopes, X-Accepted-OAuth-Scopes, X-Poll-Interval, X-GitHub-Media-Type, X-GitHub-SSO, X-GitHub-Request-Id, Deprecation, Sunset, Warning\r\nCache-Control: private, max-age=60, s-maxage=60\r\nContent-Length: 1047\r\nContent-Security-Policy: default-src \'none\'\r\nContent-Type: application/json; charset=utf-8\r\nDate: Mon, 12 Oct 2026 07:21:11 GMT\r\nEtag: "b0c28daf4dcf35f63e8c0d26b39b3b12ff5a0a087592abbc88f3ebf85a9510de"\r\nLocation: https://api.github.com/repos/medtechcorps-netizen/whatomate/git/refs/tags/rereply-compat-negative-v1-12519b7f2ad020f1aceb2a17432a686a9f87ae6d-38217227182-1-971a88f6fc88c617a8c309c7fab44e02094f7e298fbb7ab6c13497a3f66cbce1-c70a039b77a2a5001bc859691ed7e2e10747ba44c2e6a76fcc40f4730a133734\r\nReferrer-Policy: origin-when-cross-origin, strict-origin-when-cross-origin\r\nServer: github.com\r\nStrict-Transport-Security: max-age=31536000; includeSubdomains; preload\r\nVary: Accept, Authorization, Cookie, X-GitHub-OTP,Accept-Encoding, Accept, X-Requested-With\r\nX-Accepted-Github-Permissions: contents=write; contents=write,workflows=write\r\nX-Content-Type-Options: nosniff\r\nX-Frame-Options: deny\r\nX-Github-Api-Version-Selected: 2022-11-28\r\nX-Github-Edge-Region: iad\r\nX-Github-Media-Type: github.v3; format=json\r\nX-Github-Request-Id: 0411:14A872:7C0B3B5:19914400:6ACC8A67\r\nX-Ratelimit-Limit: 5000\r\nX-Ratelimit-Remaining: 4975\r\nX-Ratelimit-Reset: 1791793143\r\nX-Ratelimit-Resource: core\r\nX-Ratelimit-Used: 25\r\nX-Xss-Protection: 0\r\n\r\n{"ref":"refs/tags/rereply-compat-negative-v1-12519b7f2ad020f1aceb2a17432a686a9f87ae6d-38217227182-1-971a88f6fc88c617a8c309c7fab44e02094f7e298fbb7ab6c13497a3f66cbce1-c70a039b77a2a5001bc859691ed7e2e10747ba44c2e6a76fcc40f4730a133734","node_id":"REF_kwDOTVPortoA3XJlZnMvdGFncy9yZXJlcGx5LWNvbXBhdC1uZWdhdGl2ZS12MS0xMjUxOWI3ZjJhZDAyMGYxYWNlYjJhMTc0MzJhNjg2YTlmODdhZTZkLTM4MjE3MjI3MTgyLTEtOTcxYTg4ZjZmYzg4YzYxN2E4YzMwOWM3ZmFiNDRlMDIwOTRmN2UyOThmYmI3YWI2YzEzNDk3YTNmNjZjYmNlMS1jNzBhMDM5Yjc3YTJhNTAwMWJjODU5NjkxZWQ3ZTJlMTA3NDdiYTQ0YzJlNmE3NmZjYzQwZjQ3MzBhMTMzNzM0","url":"https://api.github.com/repos/medtechcorps-netizen/whatomate/git/refs/tags/rereply-compat-negative-v1-12519b7f2ad020f1aceb2a17432a686a9f87ae6d-38217227182-1-971a88f6fc88c617a8c309c7fab44e02094f7e298fbb7ab6c13497a3f66cbce1-c70a039b77a2a5001bc859691ed7e2e10747ba44c2e6a76fcc40f4730a133734","object":{"sha":"12519b7f2ad020f1aceb2a17432a686a9f87ae6d","type":"commit","url":"https://api.github.com/repos/medtechcorps-netizen/whatomate/git/commits/12519b7f2ad020f1aceb2a17432a686a9f87ae6d"}}'


class Clock:
    def __init__(self): self.now = 100.0
    def __call__(self): return self.now


class World:
    """Strict fixed responder, reusable by publisher composition tests."""
    def __init__(self):
        self.calls = []
        self.overrides = {}
        self.refs = {}
        self.rule = fixtures.rule_response()
        self.service = rt.runtime.Capture(0, http(200, {"total_count": 0, "workflow_runs": []}), b"")
        self.post_count = 0
        self.after = None

    def __call__(self, argv, **options):
        self.calls.append((list(argv), dict(options)))
        args = argv[1:]
        if args == ["--version"]:
            key = ("version", "")
            result = rt.runtime.Capture(0, b"gh version 2.98.0 (2026-10-01)\nhttps://github.com/cli/cli\n", b"")
        else:
            if args[:4] != ["api", "--hostname", "github.com", "--method"]:
                raise AssertionError("unexpected command prefix")
            method, path = args[4:6]
            include = "--include" in args
            prefix = transport._Client._args(method, path, include=include)
            if args[:len(prefix)] != prefix:
                raise AssertionError("unexpected fixed headers")
            key = method, path
            if method == "POST":
                if path != policy.API + "/git/refs" or not include:
                    raise AssertionError("unexpected mutation endpoint")
                fields = args[len(prefix):]
                if len(fields) != 4 or fields[::2] != ["--raw-field", "--raw-field"]:
                    raise AssertionError("unexpected mutation fields")
                ref = fields[1].removeprefix("ref=")
                sha = fields[3].removeprefix("sha=")
                if fields[1] != "ref=" + ref or fields[3] != "sha=" + sha:
                    raise AssertionError("unexpected field names")
                self.post_count += 1
                self.refs[ref] = ref_body(ref, sha)
                result = rt.runtime.Capture(0, http(201, self.refs[ref]), b"")
            elif method != "GET" or args != prefix:
                raise AssertionError("unexpected command suffix")
            elif path == policy.API:
                result = rt.runtime.Capture(0, raw({"id": policy.REPOSITORY_ID, "full_name": policy.REPOSITORY}), b"")
            elif path == policy.API + "/rulesets/" + str(fixtures.setup().rule_id):
                result = rt.runtime.Capture(0, self.rule, b"")
            elif path.startswith(policy.API + "/git/matching-refs/tags/" + policy.PREFIX):
                result = rt.runtime.Capture(0, b"[]\n", b"")
            elif path == policy.ACTIONS_ENDPOINT:
                if not include: raise AssertionError("service requires framing")
                result = self.service
            elif path.startswith(policy.API + "/git/ref/tags/" + policy.PREFIX):
                if not include: raise AssertionError("ref requires framing")
                ref = "refs/" + path.split("/git/ref/", 1)[1]
                result = (rt.runtime.Capture(0, http(200, self.refs[ref]), b"") if ref in self.refs else
                          rt.runtime.Capture(1, http(404, {"message": "Not Found", "status": "404"}), b"gh: Not Found\n"))
            else:
                raise AssertionError("unexpected endpoint")
        if key in self.overrides:
            override = self.overrides[key]
            result = override(result) if callable(override) else override
        if self.after:
            self.after(key)
        return result


class NegativeTransportTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.binary = self.root / "gh.exe"
        self.binary.write_bytes(b"controlled fixture executable never launched")
        self.clock = Clock()
        self.world = World()
        self.serial = 0

    def client(self, *, publisher=False, world=None, deadline=700, setup=None):
        self.serial += 1
        config = self.root / ("config-" + str(self.serial)); config.mkdir()
        world = world or self.world
        budget = rt.OperationBudget(deadline, capture=world, clock=self.clock)
        return transport._open(transport.NegativePublisher if publisher else transport.NegativeReader,
            setup or fixtures.setup(), self.binary, "synthetic-token", config, budget=budget,
            base_env={"PATH": "synthetic-path", "GH_TOKEN": "ambient-secret", "GH_HOST": "wrong-host",
                      "GH_CONFIG_DIR": "ambient-config", "SHIP_DO_TOKEN": "private-provider-token"})

    def refused(self, client, call):
        with self.assertRaises(policy.NegativeIndexRefusal) as caught:
            call()
        self.assertIn(str(caught.exception), ("negative-index-invalid", "negative-index-changed"))
        self.assertTrue(client.budget.failed)
        self.assertFalse(client.budget.closed)
        before = client.budget.commands
        with self.assertRaises(policy.NegativeIndexRefusal): client.read_rule()
        self.assertEqual(client.budget.commands, before)

    def test_public_unconfigured_factories_precede_every_argument_effect(self):
        class Bomb:
            def __getattribute__(self, name): raise AssertionError("argument inspected")
        absent_consumer = self.root / "absent-consumer.json"
        self.assertFalse(absent_consumer.exists())
        with mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", None), \
             mock.patch.object(outage_loader, "_POLICY_PATH", absent_consumer):
            for factory in (transport.open_reader, transport.open_publisher):
                with self.subTest(factory=factory.__name__), mock.patch.object(transport, "_open", side_effect=AssertionError("client created")):
                    with self.assertRaisesRegex(policy.NegativeIndexRefusal, "^negative-index-unconfigured$"):
                        factory(Bomb(), Bomb(), Bomb(), budget=Bomb(), base_env=Bomb())
        self.assertEqual(self.world.calls, [])

    def test_public_classes_do_not_accept_constructor_trust(self):
        for cls in (transport.NegativeReader, transport.NegativePublisher):
            with self.assertRaises(policy.NegativeIndexRefusal): cls(setup=fixtures.setup())

    def test_real_factories_use_installed_raw_and_original_budget(self):
        for publisher in (False, True):
            with self.subTest(publisher=publisher):
                config = self.root / ("factory-" + str(publisher)); config.mkdir()
                budget = rt.OperationBudget(700, capture=self.world, clock=self.clock)
                consumer = self.root / "consumer.json"
                consumer.write_bytes(fixtures.wire(fixtures.consumer_dict()))
                with mock.patch.object(policy, "_INSTALLED_PRODUCER_SETUP", fixtures.setup_bytes()), \
                     mock.patch.object(outage_loader, "_POLICY_PATH", consumer):
                    factory = transport.open_publisher if publisher else transport.open_reader
                    client = factory(self.binary, "synthetic", config, budget=budget, base_env={})
                self.assertIs(client.budget, budget)
                self.assertEqual(client.setup.raw, fixtures.setup_bytes())
                self.assertEqual(budget.commands, 2)
                self.assertFalse(budget.closed)

    def test_version_repository_host_headers_and_scrubbed_env(self):
        client = self.client()
        self.assertEqual(self.world.calls[0][0], [str(self.binary), "--version"])
        self.assertEqual(self.world.calls[1][0][1:], transport._Client._args("GET", policy.API))
        for argv, options in self.world.calls:
            self.assertNotIn("synthetic-token", argv)
            self.assertEqual(options["env"]["GH_TOKEN"], "synthetic-token")
            self.assertEqual(options["env"]["GH_HOST"], "github.com")
            self.assertEqual(options["env"]["GH_CONFIG_DIR"], str(client._config))
            self.assertNotIn("SHIP_DO_TOKEN", options["env"])
            self.assertNotIn("ambient-secret", options["env"].values())
            self.assertEqual(options["stderr_limit"], 65536)
        self.assertEqual(self.world.calls[0][1]["timeout"], 5)

    def test_wrong_version_repository_or_success_stderr_refuses(self):
        values = [(('version', ''), rt.runtime.Capture(0, b"gh version 2.97.0\n", b"")),
                  (("GET", policy.API), rt.runtime.Capture(0, raw({"id": True, "full_name": policy.REPOSITORY}), b"")),
                  (("GET", policy.API), rt.runtime.Capture(0, raw({"id": policy.REPOSITORY_ID, "full_name": "other/repo"}), b"")),
                  (("GET", policy.API), rt.runtime.Capture(0, raw({"id": policy.REPOSITORY_ID, "full_name": policy.REPOSITORY}), b"private"))]
        for key, result in values:
            with self.subTest(key=key, result=result.returncode):
                world = World(); world.overrides[key] = result
                with self.assertRaises(policy.NegativeIndexRefusal): self.client(world=world)

    def test_input_paths_token_and_exact_budget_before_process(self):
        for key, value in (("binary", Path("relative")), ("token", "private\n"), ("budget", object())):
            with self.subTest(key=key):
                config = self.root / ("invalid-" + key); config.mkdir()
                args = dict(binary=self.binary, token="synthetic", config_dir=config,
                            budget=rt.OperationBudget(700, capture=self.world, clock=self.clock), base_env={})
                args[key] = value
                before = len(self.world.calls)
                with self.assertRaises(policy.NegativeIndexRefusal): transport._open(transport.NegativeReader, fixtures.setup(), **args)
                self.assertEqual(len(self.world.calls), before)

    def test_forged_setup_holder_is_reparsed(self):
        for value in (replace(fixtures.setup(), rule_id=1), replace(fixtures.setup(), epoch="other"),
                      replace(fixtures.setup(), updated_at=fixtures.setup().updated_at.replace(tzinfo=dt.timezone(dt.timedelta(hours=1))))):
            with self.subTest(value=value.rule_id):
                with self.assertRaises(policy.NegativeIndexRefusal): self.client(setup=value)
        self.assertEqual(self.world.calls, [])

    def test_raw_rule_and_exact_sha_prefix_reads_replayed_stably(self):
        client = self.client(); sha = "a" * 40
        before = client.read_rule(); refs = client.read_refs(sha)
        self.assertEqual(before, self.world.rule); self.assertEqual(refs, b"[]\n")
        self.assertEqual(client.read_rule(), before); self.assertEqual(client.read_refs(sha), refs)
        paths = [args[6] for args, _ in self.world.calls[2:]]
        self.assertEqual(paths[1], policy.API + "/git/matching-refs/tags/" + policy.PREFIX + sha + "-")
        self.assertFalse(client.budget.failed)

    def test_invalid_source_never_reaches_path_builder(self):
        for sha in ("main", "A" * 40, "a" * 39, "a" * 40 + "/", None, True):
            with self.subTest(sha=sha):
                client = self.client(world=World()); before = client.budget.commands
                self.refused(client, lambda: client.read_refs(sha))
                self.assertEqual(client.budget.commands, before)

    def test_nonempty_even_malformed_rows_are_retained_not_filtered(self):
        client = self.client(); sha = "a" * 40
        path = policy.API + "/git/matching-refs/tags/" + policy.PREFIX + sha + "-"
        observed = raw([{"ref": "malformed"}, 7, None])
        self.world.overrides[("GET", path)] = rt.runtime.Capture(0, observed, b"")
        self.assertEqual(client.read_refs(sha), observed)
        self.assertEqual(len(policy.replay_refs(observed, producer_sha=sha).rows), 3)

    def test_malformed_top_or_row_bound_is_sticky_refusal(self):
        for observed in (b"{}", raw([{}] * 257), b"[", b"[]" + b" " * 65536):
            with self.subTest(length=len(observed)):
                world = World(); client = self.client(world=world); sha = "a" * 40
                world.overrides[("GET", policy.API + "/git/matching-refs/tags/" + policy.PREFIX + sha + "-")] = rt.runtime.Capture(0, observed, b"")
                self.refused(client, lambda: client.read_refs(sha))

    def test_rule_or_refs_changed_between_reads_refuses(self):
        for which in ("rule", "refs"):
            with self.subTest(which=which):
                world = World(); client = self.client(world=world)
                if which == "rule":
                    client.read_rule(); world.rule += b" "
                    self.refused(client, client.read_rule)
                else:
                    client.read_refs("a" * 40)
                    path = policy.API + "/git/matching-refs/tags/" + policy.PREFIX + "a" * 40 + "-"
                    world.overrides[("GET", path)] = rt.runtime.Capture(0, b"[{}]\n", b"")
                    self.refused(client, lambda: client.read_refs("a" * 40))

    def test_hidden_bypass_is_preserved_as_hidden_not_empty(self):
        self.world.rule = fixtures.rule_response(hidden=True)
        client = self.client(); observed = client.read_rule()
        self.assertEqual(policy.replay_rule(observed, client.setup).bypass_visibility, "hidden-under-setup-trust")
        self.assertNotIn(b"bypass_actors", observed)

    def test_service_available_and_completed_unavailable_do_not_poison(self):
        for status in (200, 502, 503, 504):
            with self.subTest(status=status):
                world = World(); client = self.client(world=world)
                world.service = rt.runtime.Capture(0 if status == 200 else 1,
                    http(status, {"total_count": 0, "workflow_runs": []} if status == 200 else b"temporary service failure"),
                    b"" if status == 200 else b"gh: completed service error\n")
                observed = client.observe_actions_service()
                self.assertEqual(observed, world.service.stdout)
                self.assertEqual(policy.replay_http(observed, endpoint=policy.ACTIONS_ENDPOINT).status, status)
                self.assertFalse(client.budget.failed)
                self.assertEqual(client.read_rule(), world.rule)

    def test_service_status_exit_framing_and_partial_fail_closed(self):
        values = [rt.runtime.Capture(1, http(status, {"message": "failed"}), b"private") for status in (301, 401, 403, 404, 429, 500)]
        values += [rt.runtime.Capture(0, http(503, b"failed"), b""), rt.runtime.Capture(1, http(200, {}), b"private"),
                   rt.runtime.Capture(1, b"HTTP/2.0 503\r\nContent-Length: 9\r\n\r\nx", b""),
                   rt.runtime.Capture(1, b"HTTP/2.0 503\r\n\r\nHTTP/2.0 200\r\n\r\n{}", b""),
                   rt.runtime.Capture(0, http(200, {"total_count": False, "workflow_runs": []}), b"")]
        for result in values:
            with self.subTest(stdout=result.stdout[:20]):
                world = World(); client = self.client(world=world); world.service = result
                self.refused(client, client.observe_actions_service)

    def test_capture_exception_oversize_and_wrong_type_are_sticky(self):
        cases = [RuntimeError("private capture data"), rt.runtime.Capture(0, b"x" * 65537, b""),
                 rt.runtime.Capture(0, b"{}", b"x" * 65537), object()]
        for item in cases:
            with self.subTest(kind=type(item).__name__):
                world = World(); client = self.client(world=world)
                def response(_):
                    if isinstance(item, Exception): raise item
                    return item
                world.overrides[("GET", policy.API + "/rulesets/" + str(client.setup.rule_id))] = response
                self.refused(client, client.read_rule)

    def test_shared_deadline_intersection_and_final_expiry(self):
        client = self.client(deadline=110)
        self.assertLessEqual(self.world.calls[1][1]["timeout"], 7.75)
        self.world.after = lambda key: setattr(self.clock, "now", 110) if key[1].endswith(str(client.setup.rule_id)) else None
        self.refused(client, client.read_rule)

    def test_closed_failed_or_reversed_clock_cannot_acquire_new_allowance(self):
        for state in ("closed", "failed", "reversed"):
            with self.subTest(state=state):
                self.clock.now = 100; client = self.client(world=World())
                if state == "reversed": self.clock.now = 99
                elif state == "closed": client.budget.close()
                else:
                    with self.assertRaises(rt.TransportRefusal): client.budget.fail("synthetic")
                before = client.budget.commands
                with self.assertRaises(policy.NegativeIndexRefusal): client.read_rule()
                self.assertEqual(client.budget.commands, before)

    def test_local_command_and_byte_caps_and_original_outer_caps(self):
        for field, value in (("_calls", 10), ("_bytes", transport.MAX_BYTES - transport.STDERR_LIMIT)):
            with self.subTest(field=field):
                client = self.client(world=World()); setattr(client, field, value)
                before = client.budget.commands; self.refused(client, client.read_rule)
                self.assertEqual(client.budget.commands, before)
        for field, value in (("commands", rt.MAX_COMMANDS), ("captured_bytes", rt.MAX_BYTES)):
            with self.subTest(outer=field):
                client = self.client(world=World()); setattr(client.budget, field, value)
                self.refused(client, client.read_rule)

    def test_all_returned_stream_bytes_charge_both_allowances(self):
        client = self.client(); client.read_rule()
        self.world.service = rt.runtime.Capture(1, http(503, b"unavailable"), b"bounded diagnostic")
        client.observe_actions_service()
        self.assertEqual(client._bytes, client.budget.captured_bytes)
        self.assertEqual(client._calls, client.budget.commands)

    def test_binary_or_config_mutation_refuses_before_next_capture(self):
        for mode in ("binary", "config"):
            with self.subTest(mode=mode):
                world = World(); client = self.client(world=world); before = len(world.calls)
                if mode == "binary": self.binary.write_bytes(b"replaced executable")
                else: (client._config / "hosts.yml").write_text("unexpected", encoding="ascii")
                self.refused(client, client.read_rule)
                self.assertEqual(len(world.calls), before)

    def test_binary_mutated_during_capture_prevents_return(self):
        client = self.client()
        self.world.after = lambda key: self.binary.write_bytes(b"swapped")
        self.refused(client, client.read_rule)

    def test_observed_marker_creation_with_312_character_node_id(self):
        value = json.loads(ACTUAL_H4_MARKER_CREATED.split(b"\r\n\r\n", 1)[1])
        status, body, headers = policy._http_frame(ACTUAL_H4_MARKER_CREATED,
            expected_created_location=value["url"])
        self.assertEqual(status, 201)
        self.assertEqual(len(body), 1047)
        self.assertEqual(headers[b"content-length"], b"1047")
        self.assertEqual(len(value["node_id"]), 312)
        self.assertEqual(transport.NegativePublisher._ref_body(
            body, value["ref"], value["object"]["sha"]), value)

    def test_bounded_long_nodes_create_readback_and_final_rule_for_both_protocols(self):
        observed = json.loads(ACTUAL_H4_MARKER_CREATED.split(b"\r\n\r\n", 1)[1])["node_id"]
        original = ref_body
        for protocol in ("normal3", "rollback4"):
            for node in ("N" * 256, observed, "N" * 1024):
                with self.subTest(protocol=protocol, length=len(node)):
                    request = replace(fixtures.request(), protocol=protocol)
                    ref = policy.marker_ref(request)
                    world = World(); client = self.client(publisher=True, world=world)
                    def response(name, sha):
                        # Controlled opaque metadata; no node ID decoding or authority.
                        return dict(original(name, sha), node_id=node)
                    location = ("https://api.github.com" + policy.API + "/git/" + ref).encode()
                    world.overrides[("POST", policy.API + "/git/refs")] = lambda result: replace(result,
                        stdout=result.stdout.replace(b"\r\n", b"\n", 1).replace(
                            b"\r\n\r\n", b"\r\nLocation: " + location + b"\r\n\r\n"))
                    with mock.patch.dict(globals(), {"ref_body": response}):
                        receipt = client.publish_intent(request)
                    self.assertEqual(receipt, policy.IntentPersisted(request, client.setup.sha256, ref))
                    self.assertEqual(world.refs[ref]["node_id"], node)
                    self.assertEqual(world.post_count, 1)
                    self.assertEqual(client.budget.commands, 7)
                    self.assertEqual(world.calls[-2][0][5], "GET")
                    self.assertEqual(world.calls[-2][0][6], policy.API + "/git/ref/" + ref[5:])
                    self.assertEqual(world.calls[-1][0][6], policy.API + "/rulesets/" + str(client.setup.rule_id))
                    before = len(world.calls)
                    self.refused(client, lambda: client.publish_intent(request))
                    self.assertEqual(len(world.calls), before)
                    self.assertEqual(world.post_count, 1)

    def test_existing_long_marker_remains_idempotent_with_exact_readback(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        for length in (312, 1024):
            with self.subTest(length=length):
                world = World(); world.refs[ref] = dict(ref_body(ref, request.producer_sha), node_id="N" * length)
                client = self.client(publisher=True, world=world)
                self.assertEqual(client.publish_intent(request).marker_ref, ref)
                self.assertEqual(world.post_count, 0)
                self.assertEqual(client.budget.commands, 6)

    def test_invalid_node_metadata_refuses_existing_and_created_refs(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        original = ref_body
        invalid = ("", "N" * 1025, True, 312, None, [], {}, "N N", "N\tN", "N\nN", "N\rN",
                   "N\0N", "N\x1fN", "N\x7fN", "N\u0080N", "N\ufffdN")
        for existing in (False, True):
            for node in invalid:
                with self.subTest(existing=existing, node=repr(node)[:60]):
                    world = World(); client = self.client(publisher=True, world=world)
                    if existing: world.refs[ref] = dict(original(ref, request.producer_sha), node_id=node)
                    def response(name, sha): return dict(original(name, sha), node_id=node)
                    with mock.patch.dict(globals(), {"ref_body": response}):
                        self.refused(client, lambda: client.publish_intent(request))
                    self.assertEqual(world.post_count, 0 if existing else 1)
                    before = len(world.calls)
                    self.refused(client, lambda: client.publish_intent(request))
                    self.assertEqual(len(world.calls), before)

    def test_long_node_readback_change_refuses_without_another_post(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        original = ref_body
        for length in (312, 1024):
            with self.subTest(length=length):
                world = World(); client = self.client(publisher=True, world=world)
                def response(name, sha): return dict(original(name, sha), node_id="N" * length)
                def after(key):
                    if key == ("POST", policy.API + "/git/refs"):
                        world.refs[ref]["node_id"] = "M" + "N" * (length - 1)
                world.after = after
                with mock.patch.dict(globals(), {"ref_body": response}):
                    self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 1)
                self.assertEqual(client.budget.commands, 6)

    def test_long_node_does_not_relax_ref_object_or_closed_body_bindings(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        for fault in ("ref", "url", "type", "sha", "object-url", "extra", "object-extra"):
            with self.subTest(fault=fault):
                world = World(); value = dict(ref_body(ref, request.producer_sha), node_id="N" * 1024)
                if fault == "ref": value["ref"] = "refs/tags/other"
                elif fault == "url": value["url"] += "-other"
                elif fault == "type": value["object"]["type"] = "tag"
                elif fault == "sha": value["object"]["sha"] = "b" * 40
                elif fault == "object-url": value["object"]["url"] += "-other"
                elif fault == "extra": value["extra"] = "unexpected"
                else: value["object"]["extra"] = "unexpected"
                world.refs[ref] = value
                client = self.client(publisher=True, world=world)
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 0)

    def test_create_once_exact_fields_readback_and_final_rule(self):
        client = self.client(publisher=True); request = fixtures.request()
        receipt = client.publish_intent(request)
        self.assertEqual(receipt, policy.IntentPersisted(request, client.setup.sha256, policy.marker_ref(request)))
        self.assertEqual(self.world.post_count, 1); self.assertEqual(client.budget.commands, 7)
        args = next(a for a, _ in self.world.calls if "POST" in a)
        self.assertEqual(args[-4:], ["--raw-field", "ref=" + policy.marker_ref(request), "--raw-field", "sha=" + request.producer_sha])
        self.assertEqual(self.world.calls[-1][0][6], policy.API + "/rulesets/" + str(client.setup.rule_id))
        self.assertFalse(client.budget.closed)
        self.refused(client, lambda: client.publish_intent(request))
        self.assertEqual(self.world.post_count, 1)

    def test_create_exact_location_keeps_single_post_readback_and_final_rule(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        expected = "https://api.github.com" + policy.API + "/git/" + ref
        self.world.overrides[("POST", policy.API + "/git/refs")] = lambda result: replace(result,
            stdout=result.stdout.replace(b"\r\n\r\n", b"\r\nLocation: " + expected.encode() + b"\r\n\r\n"))
        client = self.client(publisher=True)
        self.assertEqual(client.publish_intent(request).marker_ref, ref)
        self.assertEqual(self.world.post_count, 1)
        self.assertEqual(client.budget.commands, 7)
        self.assertEqual(self.world.calls[-1][0][6], policy.API + "/rulesets/" + str(client.setup.rule_id))
        self.refused(client, lambda: client.publish_intent(request))
        self.assertEqual(self.world.post_count, 1)

    def test_create_location_refusal_is_sticky_and_body_authority_stays_exact(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        expected = "https://api.github.com" + policy.API + "/git/" + ref
        for fault in ("wrong-location", "wrong-body", "stderr", "exit", "status", "readback", "rule"):
            with self.subTest(fault=fault):
                world = World(); client = self.client(publisher=True, world=world)
                def override(result):
                    body = ref_body(ref, request.producer_sha)
                    if fault == "wrong-body": body["object"]["sha"] = "b" * 40
                    raw = http(200 if fault == "status" else 201, body)
                    url = expected + "-other" if fault == "wrong-location" else expected
                    raw = raw.replace(b"\r\n\r\n", b"\r\nLocation: " + url.encode() + b"\r\n\r\n")
                    if fault == "readback": world.refs[ref]["node_id"] = "changed-node"
                    if fault == "rule": world.rule += b" "
                    return rt.runtime.Capture(1 if fault == "exit" else 0, raw, b"error" if fault == "stderr" else b"")
                world.overrides[("POST", policy.API + "/git/refs")] = override
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 1)
                before = len(world.calls)
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(len(world.calls), before)

    def test_location_on_exact_get_remains_refused(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        for present in (False, True):
            with self.subTest(existing=present):
                world = World()
                if present: world.refs[ref] = ref_body(ref, request.producer_sha)
                path = policy.API + "/git/ref/" + ref[5:]
                world.overrides[("GET", path)] = lambda result: replace(result, stdout=result.stdout.replace(
                    b"\r\n\r\n", b"\r\nLocation: https://api.github.com" + policy.API.encode() + b"/git/" + ref.encode() + b"\r\n\r\n"))
                client = self.client(publisher=True, world=world)
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 0)

    def test_existing_exact_marker_is_idempotent_and_still_reread(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        self.world.refs[ref] = ref_body(ref, request.producer_sha)
        client = self.client(publisher=True); receipt = client.publish_intent(request)
        self.assertEqual(receipt.marker_ref, ref); self.assertEqual(self.world.post_count, 0)
        self.assertEqual(client.budget.commands, 6)

    def test_wrong_existing_ref_object_or_identity_never_posts(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        mutations = [("ref", "refs/tags/other"), ("node_id", True), ("url", "https://other.example/ref"),
                     ("object", {"type": "tag", "sha": request.producer_sha, "url": "wrong"}),
                     ("object", {"type": "commit", "sha": "b" * 40, "url": "wrong"})]
        for key, value in mutations:
            with self.subTest(key=key):
                world = World(); world.refs[ref] = ref_body(ref, request.producer_sha); world.refs[ref][key] = value
                client = self.client(publisher=True, world=world)
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 0)

    def test_untyped_404_and_other_ref_errors_cannot_create(self):
        request = fixtures.request(); ref = policy.marker_ref(request)
        path = policy.API + "/git/ref/" + ref.removeprefix("refs/")
        values = [rt.runtime.Capture(1, b'{"message":"Not Found"}', b"404"),
                  rt.runtime.Capture(0, http(404, {"message": "Not Found"}), b""),
                  rt.runtime.Capture(1, http(404, {"message": "Unauthorized"}), b""),
                  rt.runtime.Capture(1, http(403, {"message": "Not Found"}), b""),
                  rt.runtime.Capture(1, http(404, {"message": "Not Found", "status": 404}), b"")]
        for value in values:
            with self.subTest(body=value.stdout[:30]):
                world = World(); client = self.client(publisher=True, world=world); world.overrides[("GET", path)] = value
                self.refused(client, lambda: client.publish_intent(request)); self.assertEqual(world.post_count, 0)

    def test_ambiguous_create_has_no_retry_even_with_fresh_client_budget(self):
        client = self.client(publisher=True); request = fixtures.request()
        self.world.overrides[("POST", policy.API + "/git/refs")] = rt.runtime.Capture(1, http(502, b"ambiguous"), b"private")
        self.refused(client, lambda: client.publish_intent(request)); self.assertEqual(self.world.post_count, 1)
        config = self.root / "after-ambiguous"; config.mkdir()
        before = len(self.world.calls)
        with self.assertRaises(policy.NegativeIndexRefusal):
            transport._open(transport.NegativePublisher, fixtures.setup(), self.binary, "synthetic", config,
                            budget=client.budget, base_env={})
        self.assertEqual(len(self.world.calls), before)

    def test_post_success_without_exact_readback_never_returns_receipt(self):
        request = fixtures.request()
        for fault in ("missing", "wrong-node", "wrong-source"):
            with self.subTest(fault=fault):
                world = World(); client = self.client(publisher=True, world=world)
                def after(key):
                    if key == ("POST", policy.API + "/git/refs"):
                        ref = policy.marker_ref(request)
                        if fault == "missing": world.refs.clear()
                        elif fault == "wrong-node": world.refs[ref]["node_id"] = "different-node"
                        else: world.refs[ref]["object"]["sha"] = "b" * 40
                world.after = after
                self.refused(client, lambda: client.publish_intent(request)); self.assertEqual(world.post_count, 1)

    def test_final_rule_or_deadline_failure_leaves_intent_without_receipt(self):
        request = fixtures.request()
        for fault in ("rule", "deadline"):
            with self.subTest(fault=fault):
                self.clock.now = 100; world = World(); client = self.client(publisher=True, world=world)
                def after(key):
                    if key == ("POST", policy.API + "/git/refs"):
                        if fault == "rule": world.rule += b" "
                        else: self.clock.now = 700
                world.after = after
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(world.post_count, 1); self.assertIn(policy.marker_ref(request), world.refs)

    def test_invalid_request_produces_no_rule_or_ref_effect(self):
        for request in (replace(fixtures.request(), run_id=True), replace(fixtures.request(), producer_sha="main"),
                        replace(fixtures.request(), protocol="normal2"), replace(fixtures.request(), run_attempt=10000)):
            with self.subTest(request=request.protocol):
                client = self.client(publisher=True, world=World()); before = client.budget.commands
                self.refused(client, lambda: client.publish_intent(request))
                self.assertEqual(client.budget.commands, before)

    def test_normal_and_rollback_requests_both_bind_producer_sha(self):
        for protocol in ("normal3", "rollback4"):
            with self.subTest(protocol=protocol):
                request = replace(fixtures.request(), protocol=protocol, producer_sha="c" * 40)
                world = World(); client = self.client(publisher=True, world=world)
                result = client.publish_intent(request)
                self.assertIn("-" + request.producer_sha + "-", result.marker_ref)
                self.assertEqual(world.refs[result.marker_ref]["object"]["sha"], request.producer_sha)


if __name__ == "__main__":
    unittest.main()
