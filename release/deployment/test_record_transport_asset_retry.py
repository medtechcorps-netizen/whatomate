"""Fake-only coverage for one bounded owned-asset TLS transport retry."""
import unittest

import record_transport as rt
import ship_common as common
import ship_compat
import test_record_transport as fixtures


TLS = (b'Get "https://release-assets.githubusercontent.com/asset?opaque=withheld": '
       b'tls: first record does not look like a TLS handshake\n')


class AssetTlsRetryTests(unittest.TestCase):
    setUp = fixtures.ReleaseTransportTests.setUp
    client = fixtures.ReleaseTransportTests.client
    first = fixtures.ReleaseTransportTests.first

    def prepared(self, *, deadline=700):
        self.clock.now = 100.0
        world = fixtures.World()
        document = self.first()
        raw = world.add(document)
        world.fake.releases[0]["assets"][0]["digest"] = "sha256:" + common.sha256_bytes(raw)
        client = self.client(world=world, deadline=deadline)
        snapshot = client.capture_snapshot()
        args = ["api", "--method", "GET", rt.API + f"/releases/assets/{snapshot.records[0].asset_id}",
                "-H", "Accept: application/octet-stream", "--allow-escape-sequences"]
        return client, args, raw, document

    def scripted(self, client, responses, *, after=None):
        ledger = []
        def capture(argv, **options):
            index = len(ledger)
            self.assertLess(index, len(responses), "unexpected extra physical call")
            elapsed, result = responses[index]
            ledger.append((list(argv), dict(options), result))
            self.clock.now += elapsed
            if after is not None:
                after(index)
            if isinstance(result, Exception):
                raise result
            return result
        client.budget._capture = capture
        return ledger, capture

    def run_asset(self, client, args):
        return client._run(args, code="ignored")

    def test_exact_tls_then_success_keeps_both_physical_attempts_and_bytes(self):
        for ending in (b"", b"\n", b"\r\n"):
            with self.subTest(ending=ending):
                client, args, raw, _ = self.prepared()
                first = rt.runtime.Capture(1, b"", TLS.rstrip(b"\n") + ending)
                second = rt.runtime.Capture(0, raw, b"")
                ledger, _ = self.scripted(client, [(7, first), (1, second)])
                before = client.budget.commands, client.budget.captured_bytes
                self.assertIs(self.run_asset(client, args), second.stdout)
                self.assertEqual(len(ledger), 2)
                self.assertIs(ledger[0][2], first)
                self.assertIs(ledger[1][2], second)
                self.assertEqual(ledger[0][0], ledger[1][0])
                self.assertIs(ledger[0][1]["env"], ledger[1][1]["env"])
                self.assertEqual(set(ledger[0][1]), {"env", "timeout", "stdout_limit", "stderr_limit"})
                self.assertEqual(ledger[0][1]["timeout"], 30)
                self.assertEqual(ledger[1][1]["timeout"], 23)
                self.assertEqual(ledger[0][1]["stdout_limit"], ledger[1][1]["stdout_limit"])
                self.assertEqual(ledger[0][1]["stderr_limit"], ledger[1][1]["stderr_limit"])
                self.assertEqual(client.budget.commands - before[0], 2)
                self.assertEqual(client.budget.captured_bytes - before[1], len(first.stderr) + len(raw))
                self.assertFalse(client.budget.failed)
                self.assertIsNone(client.budget._failure_category)

    def test_second_failure_is_final_and_poison_is_not_reset(self):
        for second_error in (TLS, b"another error"):
            client, args, _, _ = self.prepared()
            ledger, _ = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS)),
                                               (1, rt.runtime.Capture(1, b"", second_error))])
            with self.assertRaisesRegex(rt.TransportRefusal, "transport-command-failed$"):
                self.run_asset(client, args)
            self.assertEqual(len(ledger), 2)
            self.assertTrue(client.budget.failed)
            self.assertEqual(client.budget._failure_category, "command-nonzero")
            with self.assertRaises(rt.TransportRefusal):
                self.run_asset(client, args)
            self.assertEqual(len(ledger), 2)

    def test_nonmatching_result_never_retries(self):
        errors = [b"", b"authentication required", b"certificate validation failed", b"signature invalid",
                  TLS.replace(b"release-assets.githubusercontent.com", b"other.example"),
                  TLS.replace(b"https://", b"http://"),
                  TLS.replace(b".com/", b".com.evil/"),
                  TLS.replace(b".com/", b".com:443/"),
                  TLS.replace(b"TLS handshake", b"TLS handshake failed"),
                  b"prefix " + TLS, TLS + b"extra\n", TLS + b"\n",
                  TLS.replace(b"asset?opaque=withheld", b"asset\nprivate"),
                  TLS.replace(b"asset?opaque=withheld", b"asset\\private")]
        cases = [rt.runtime.Capture(1, b"", error) for error in errors]
        cases += [rt.runtime.Capture(2, b"", TLS), rt.runtime.Capture(-1, b"", TLS),
                  rt.runtime.Capture(1, b"partial body", TLS), rt.runtime.Capture(0, b"success", TLS)]
        for result in cases:
            with self.subTest(result=result):
                client, args, _, _ = self.prepared()
                ledger, _ = self.scripted(client, [(1, result)])
                if result.returncode == 0:
                    self.assertEqual(self.run_asset(client, args), b"success")
                else:
                    with self.assertRaises(rt.TransportRefusal):
                        self.run_asset(client, args)
                self.assertEqual(len(ledger), 1)

    def test_other_get_version_and_contract_post_never_retry(self):
        for route in ("get", "version", "contract"):
            client, _, _, _ = self.prepared()
            ledger, _ = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS))])
            with self.assertRaises(rt.TransportRefusal):
                if route == "contract":
                    client._contract_page("associations", client._snapshot.main_sha,
                                          byte_allowance=rt._CONTRACT_BYTES)
                else:
                    args = ["--version"] if route == "version" else ["api", "--method", "GET", rt.API, *rt.HEADERS]
                    self.run_asset(client, args)
            self.assertEqual(len(ledger), 1)

    def test_unowned_or_altered_asset_and_mutation_routes_are_closed(self):
        for kind in ("unowned", "post", "body", "header", "suffix", "host", "mutation"):
            client, args, _, _ = self.prepared()
            if kind == "unowned": args[3] += "0"
            if kind == "post": args[2] = "POST"
            if kind == "body": args += ["--input", "-"]
            if kind == "header": args[5] = "Accept: application/json"
            if kind == "suffix": args.pop()
            if kind == "host": args[3] = "https://api.github.com" + args[3]
            if kind == "mutation": args = ["release", "create", "untrusted"]
            ledger, _ = self.scripted(client, [])
            with self.assertRaises(rt.TransportRefusal):
                self.run_asset(client, args)
            self.assertEqual(ledger, [])

    def test_actual_attestation_scope_does_not_retry_tls_error(self):
        client, _, _, document = self.prepared()
        directory = client.work_dir / "record-0"
        client.download_manifest(document["release"], directory)
        ledger, _ = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS))])
        with self.assertRaises(common.ReleaseError):
            client.verify_file(directory / rt.records.MANIFEST_ASSET,
                               signer_digest=document["sha"], source_digest=document["sha"])
        self.assertEqual(len(ledger), 1)
        self.assertEqual(ledger[0][0][1:3], ["attestation", "verify"])
        self.assertEqual(client._verified, {})

    def test_expired_original_call_window_cannot_start_second_attempt(self):
        for elapsed in (30, 31):
            client, args, _, _ = self.prepared()
            ledger, _ = self.scripted(client, [(elapsed, rt.runtime.Capture(1, b"", TLS))])
            with self.assertRaises(rt.TransportRefusal):
                self.run_asset(client, args)
            self.assertEqual(len(ledger), 1)

    def test_inner_deadline_and_cleanup_reserve_clamp_both_attempts(self):
        client, args, raw, _ = self.prepared(deadline=120)
        ledger, _ = self.scripted(client, [(5, rt.runtime.Capture(1, b"", TLS)),
                                          (1, rt.runtime.Capture(0, raw, b""))])
        deadline = client.budget._deadline
        self.assertEqual(self.run_asset(client, args), raw)
        self.assertEqual(ledger[0][1]["timeout"], 20 - rt.CLEANUP_RESERVE)
        self.assertEqual(ledger[1][1]["timeout"], 15 - rt.CLEANUP_RESERVE)
        self.assertEqual(client.budget._deadline, deadline)
        client, args, _, _ = self.prepared(deadline=120)
        ledger, _ = self.scripted(client, [(18, rt.runtime.Capture(1, b"", TLS))])
        with self.assertRaises(rt.TransportRefusal): self.run_asset(client, args)
        self.assertEqual(len(ledger), 1)

    def test_aggregate_deadline_and_physical_accounting_are_retained(self):
        client, args, raw, _ = self.prepared()
        ledger, capture = self.scripted(client, [(5, rt.runtime.Capture(1, b"", TLS)),
                                                (1, rt.runtime.Capture(0, raw, b""))])
        aggregate = ship_compat._Budget(120, capture=capture, clock=self.clock)
        aggregate.bind(self.root, self.binary, self.binary)
        client.budget._capture = aggregate.capture
        before = client.budget.commands, client.budget.captured_bytes
        self.assertEqual(self.run_asset(client, args), raw)
        self.assertEqual([row[1]["timeout"] for row in ledger],
                         [20 - ship_compat.CLEANUP_RESERVE, 15 - ship_compat.CLEANUP_RESERVE])
        self.assertEqual(aggregate.end, 120)
        self.assertEqual(aggregate.commands, 2)
        self.assertEqual(client.budget.commands - before[0], 2)
        self.assertEqual(aggregate.captured_bytes, len(TLS) + len(raw))
        self.assertEqual(client.budget.captured_bytes - before[1], aggregate.captured_bytes)

    def test_inner_and_aggregate_caps_block_second_physical_attempt(self):
        for owner in ("inner", "aggregate"):
            for cap in ("commands", "bytes"):
                with self.subTest(owner=owner, cap=cap):
                    client, args, _, _ = self.prepared()
                    ledger, capture = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS))])
                    budget = client.budget
                    if owner == "aggregate":
                        budget = ship_compat._Budget(1000, capture=capture, clock=self.clock)
                        budget.bind(self.root, self.binary, self.binary)
                        client.budget._capture = budget.capture
                    if cap == "commands": budget.commands = rt.MAX_COMMANDS - 1
                    else: budget.captured_bytes = rt.MAX_BYTES - 65536 - len(TLS)
                    with self.assertRaises(rt.TransportRefusal): self.run_asset(client, args)
                    self.assertEqual(len(ledger), 1)
                    self.assertTrue(budget.failed)
                    self.assertTrue(client.budget.failed)
                    self.assertLessEqual(budget.commands, rt.MAX_COMMANDS)
                    self.assertLessEqual(budget.captured_bytes, rt.MAX_BYTES)

    def test_capture_exception_invalid_capture_and_directory_change_never_retry(self):
        cases = [RuntimeError("private failure"), object(),
                 rt.runtime.Capture(1, b"", TLS + b"x" * 65536)]
        for result in cases:
            client, args, _, _ = self.prepared()
            ledger, _ = self.scripted(client, [(1, result)])
            with self.assertRaises(rt.TransportRefusal): self.run_asset(client, args)
            self.assertEqual(len(ledger), 1)
            self.assertTrue(client.budget.failed)
        client, args, _, _ = self.prepared()
        def replace_directory(_):
            client._config.rename(client._config.with_name("replaced"))
            client._config.mkdir()
        ledger, _ = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS))], after=replace_directory)
        with self.assertRaises(rt.TransportRefusal): self.run_asset(client, args)
        self.assertEqual(len(ledger), 1)

    def test_retry_success_still_requires_exact_manifest_size_and_digest(self):
        client, _, raw, document = self.prepared()
        replacement = (b"x" if raw[:1] != b"x" else b"y") + raw[1:]
        ledger, _ = self.scripted(client, [(1, rt.runtime.Capture(1, b"", TLS)),
                                          (1, rt.runtime.Capture(0, replacement, b""))])
        with self.assertRaisesRegex(rt.TransportRefusal, "transport-download-bytes$"):
            client.download_manifest(document["release"], client.work_dir / "record-0")
        self.assertEqual(len(ledger), 2)
        self.assertEqual(client._downloads, {})
        self.assertEqual(client._verified, {})

    def test_first_failure_category_survives_normalization_without_changing_error(self):
        categories = {"deadline": "deadline", "capture": "capture", "capture-input": "capture",
                      "command-failed": "command-nonzero", "signature": "signature",
                      "image-verification": "signature", "bound": "bound", "manifest": "other"}
        for detail, category in categories.items():
            client, _, _, _ = self.prepared()
            self.assertIsNone(client.budget._failure_category)
            with self.assertRaises(rt.TransportRefusal) as caught:
                client.budget.fail(detail)
            self.assertEqual(str(caught.exception), "record-chain-invalid:transport-" + detail)
            self.assertEqual(client.budget._failure_category, category)
            for normalized in ("signature", "image-verification", "resolver", "closed"):
                with self.assertRaises(rt.TransportRefusal) as caught:
                    client.budget.fail(normalized)
                self.assertEqual(str(caught.exception), "record-chain-invalid:transport-" + normalized)
                self.assertEqual(client.budget._failure_category, category)


if __name__ == "__main__":
    unittest.main()
