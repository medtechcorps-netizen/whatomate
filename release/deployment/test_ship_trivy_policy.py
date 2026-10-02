from __future__ import annotations

import datetime as dt
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import trivy_policy


TODAY = dt.date(2026, 10, 4)
REASON = "# reason: fixed upstream; the patched base image lands with the next snapshot"


class TrivyPolicyTests(unittest.TestCase):
    def check(self, text: str | bytes) -> int:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "ship.trivyignore"
            path.write_bytes(text if isinstance(text, bytes) else text.encode("ascii"))
            return trivy_policy.check(path, TODAY)

    def refused(self, text: str | bytes) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^trivy-exception-invalid$"):
            self.check(text)

    def test_empty_and_comment_only_files_pass(self) -> None:
        self.assertEqual(self.check(""), 0)
        self.assertEqual(self.check("# header\n\n# more\n"), 0)

    def test_valid_entries(self) -> None:
        text = f"{REASON}\nCVE-2026-12345 exp:2026-11-30\n\n# context\n{REASON}\nGHSA-2345-6789-cfgh exp:2027-01-02\n"
        self.assertEqual(self.check(text), 2)

    def test_malformed_ids_and_lines(self) -> None:
        for line in ("CVE-26-1 exp:2026-11-30", "CVE-2026-123 exp:2026-11-30", "GHSA-aaaa-bbbb-cccc exp:2026-11-30",
                     "CVE-2026-12345", "CVE-2026-12345 exp:2026-11-30 extra", " CVE-2026-12345 exp:2026-11-30",
                     "aws-access-key-id exp:2026-11-30", "generic-api-key", "CVE-2026-12345 exp:2026-02-30"):
            with self.subTest(line=line):
                self.refused(f"{REASON}\n{line}\n")

    def test_reason_is_required_directly_above(self) -> None:
        self.refused("CVE-2026-12345 exp:2026-11-30\n")
        self.refused(f"{REASON}\n\nCVE-2026-12345 exp:2026-11-30\n")
        self.refused("# reason: short\nCVE-2026-12345 exp:2026-11-30\n")
        self.refused("# reason: " + "x" * 201 + "\nCVE-2026-12345 exp:2026-11-30\n")
        self.refused(f"{REASON}\nCVE-2026-12345 exp:2026-11-30\nCVE-2026-12346 exp:2026-11-30\n")

    def test_expiry_window(self) -> None:
        self.refused(f"{REASON}\nCVE-2026-12345 exp:2026-10-04\n")
        self.refused(f"{REASON}\nCVE-2026-12345 exp:2026-10-01\n")
        self.assertEqual(self.check(f"{REASON}\nCVE-2026-12345 exp:2027-01-02\n"), 1)
        self.refused(f"{REASON}\nCVE-2026-12345 exp:2027-01-03\n")

    def test_duplicates_and_count(self) -> None:
        self.refused(f"{REASON}\nCVE-2026-12345 exp:2026-11-30\n{REASON}\nCVE-2026-12345 exp:2026-12-01\n")
        entries = "".join(f"{REASON}\nCVE-2026-{10000 + index} exp:2026-11-30\n" for index in range(50))
        self.assertEqual(self.check(entries), 50)
        self.refused(entries + f"{REASON}\nCVE-2026-99999 exp:2026-11-30\n")

    def test_encoding_and_size(self) -> None:
        self.refused(f"{REASON}\r\nCVE-2026-12345 exp:2026-11-30\r\n")
        self.refused(b"\xef\xbb\xbf# header\n")
        self.refused("# caf\u00e9\n".encode("utf-8"))
        self.refused("# no trailing newline")
        self.refused("# x\n" * 5000)
        self.refused("# tab\there\n")

    def test_committed_file_passes_with_an_injected_clock(self) -> None:
        self.assertEqual(trivy_policy.check(trivy_policy.POLICY_PATH, TODAY), 0)
        self.assertEqual(trivy_policy.check(trivy_policy.POLICY_PATH, dt.date(2030, 1, 1)), 0)

    def test_today_must_be_a_date(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^internal-error"):
            trivy_policy.check(trivy_policy.POLICY_PATH, dt.datetime(2026, 10, 4))  # type: ignore[arg-type]


if __name__ == "__main__":
    unittest.main()
