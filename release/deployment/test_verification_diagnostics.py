"""Source-owned refusal diagnostics over real preflight fixtures; no live tools."""
from contextlib import redirect_stderr
import io
from pathlib import Path
import unittest
from unittest import mock

import ship_compat as ship
import test_ship_compat as fixtures


class VerificationDiagnosticTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fixtures.PreflightTests.setUpClass()

    def scenario(self):
        case = fixtures.PreflightTests()
        case.setUp()
        self.addCleanup(case.doCleanups)
        return case

    def report(self, case, capture):
        output = io.StringIO()
        with redirect_stderr(output), self.assertRaises((ship.common.ReleaseError, ship.evidence.CompatRefusal)) as caught:
            case.run_preflight(capture=capture)
        text = output.getvalue()
        self.assertEqual(len(text.splitlines()), 1)
        self.assertRegex(text, r"^verification-refused phase=(input|source-before|actions|source-after|qualification) "
                         r"first_failure_phase=(input|source-before|actions|source-after|qualification) "
                         r"first_failure_category=(deadline|capture|command-nonzero|signature|bound|other) "
                         r"sampled_elapsed_ms_lower_bound=[0-9]+ commands=[0-9]+ captured_bytes=[0-9]+\n$")
        for private in ("PRIVATE", "https://", "gh.exe", "--format", "Traceback"):
            self.assertNotIn(private, text)
        self.assertIsNone(fixtures.source.records._GIT_OUTPUT_TRANSPORT.get())
        return caught.exception, text

    def fault(self, case, phase, mode):
        original = case.capture
        fired = []
        def capture(argv, **options):
            parts = Path(options["env"].get("GH_CONFIG_DIR", "")).parts
            target = (argv[1:3] == ["attestation", "verify"] if mode == "signature"
                      else argv[1:4] == ["api", "--method", "GET"])
            raw = original(argv, **options)
            if phase in parts and target and not fired:
                fired.append(1)
                if mode == "capture":
                    raise RuntimeError("PRIVATE https://signed.invalid/secret --format")
                if mode == "nonzero":
                    return ship.runtime.Capture(1, b"", b"PRIVATE transport bytes")
                return ship.runtime.Capture(0, b"[]", b"")
            return raw
        return capture, fired

    def test_source_first_cause_survives_signature_client_and_resolver_wrappers(self):
        for phase in ("source-before", "source-after"):
            for mode, category in (("capture", "capture"), ("nonzero", "command-nonzero"), ("signature", "signature")):
                with self.subTest(phase=phase, mode=mode):
                    case = self.scenario()
                    capture, fired = self.fault(case, phase, mode)
                    error, line = self.report(case, capture)
                    self.assertEqual(error.code, "record-chain-invalid:transport-resolver")
                    self.assertEqual(fired, [1])
                    self.assertIn("first_failure_phase=" + phase, line)
                    self.assertIn("first_failure_category=" + category, line)

    def test_actions_capture_nonzero_and_signature_have_distinct_fixed_categories(self):
        for mode, category in (("capture", "capture"), ("nonzero", "command-nonzero"), ("signature", "signature")):
            with self.subTest(mode=mode):
                case = self.scenario()
                capture, fired = self.fault(case, "actions", mode)
                error, line = self.report(case, capture)
                self.assertIs(type(error), ship.evidence.CompatRefusal)
                self.assertEqual(fired, [1])
                self.assertIn("first_failure_phase=actions", line)
                self.assertIn("first_failure_category=" + category, line)

    def test_exact_source_git_cleanup_deadline_keeps_original_public_refusal(self):
        case = self.scenario()
        original = case.capture
        advanced = []
        def capture(argv, **options):
            raw = original(argv, **options)
            if argv[0] == str(case.case.git) and not advanced:
                advanced.append(1)
                # Original source allowance starts at100 and ends at1000. This
                # leaves positive time but less than the Git cleanup reserve.
                case.case.clock.now = 997.76
            return raw
        error, line = self.report(case, capture)
        self.assertEqual(advanced, [1])
        self.assertEqual(error.code, "record-chain-invalid:transport-resolver")
        self.assertIn("first_failure_phase=source-before", line)
        self.assertIn("first_failure_category=deadline", line)

    def test_exception_text_does_not_relabel_capture_as_deadline(self):
        case = self.scenario()
        original = case.capture
        def capture(argv, **options):
            raw = original(argv, **options)
            if argv[1:4] == ["api", "--method", "GET"]:
                raise TimeoutError("PRIVATE source-deadline signature https://secret.invalid")
            return raw
        _, line = self.report(case, capture)
        self.assertIn("first_failure_category=capture", line)
        self.assertNotIn("first_failure_category=deadline", line)

    def test_success_emits_no_failure_and_keeps_complete_public_result(self):
        case = self.scenario()
        output = io.StringIO()
        with redirect_stderr(output):
            value = case.run_preflight()
        self.assertEqual(output.getvalue(), "")
        self.assertIs(type(value), ship.NormalPreflight)
        self.assertEqual(value.resolution, case.expected)
        self.assertEqual(value.command_count, len(case.calls))

    def test_diagnostic_output_failure_does_not_replace_public_refusal(self):
        case = self.scenario()
        capture, fired = self.fault(case, "source-before", "nonzero")
        sink = mock.Mock()
        sink.write.side_effect = OSError("PRIVATE")
        with mock.patch.object(ship.sys, "stderr", sink), self.assertRaises(ship.common.ReleaseError) as caught:
            case.run_preflight(capture=capture)
        self.assertEqual(caught.exception.code, "record-chain-invalid:transport-resolver")
        self.assertEqual(fired, [1])

    def test_first_category_is_sticky_and_untrusted_labels_never_print(self):
        case = self.scenario()
        budget = ship._Budget(1000, capture=case.capture, clock=case.case.clock)
        budget._phase = "actions"
        budget.note_failure("capture")
        budget.note_failure("signature")
        self.assertEqual(budget._failure_category, "capture")
        budget._failure_category = "PRIVATE https://secret.invalid"
        budget._failure_phase = "PRIVATE"
        output = io.StringIO()
        with redirect_stderr(output):
            budget.report_refusal()
        self.assertIn("first_failure_category=other", output.getvalue())
        self.assertNotIn("PRIVATE", output.getvalue())


    def test_final_close_deadline_reports_without_replacing_original_refusal(self):
        case = self.scenario()
        original = ship._Budget.close
        closed = []
        def expire(budget):
            if not budget.failed:
                case.case.clock.now = budget.end
            try:
                return original(budget)
            finally:
                closed.append(budget.closed)
        with mock.patch.object(ship._Budget, "close", expire):
            error, line = self.report(case, case.capture)
        self.assertIs(type(error), ship.evidence.CompatRefusal)
        self.assertEqual((error.code, error.detail), ("compat-stale", "preflight-deadline"))
        self.assertEqual(closed, [True])
        self.assertIn("phase=qualification", line)
        self.assertIn("first_failure_category=deadline", line)


if __name__ == "__main__":
    unittest.main()
