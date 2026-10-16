"""Synthetic runner tests for the real Gh wrapper; no network or credentials."""
from contextlib import contextmanager
import copy
from dataclasses import replace
import io
import json
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import warnings
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat_adapter as adapter
import compat_evidence as evidence
import release_record as records
import test_compat_evidence as fixtures
import test_compat_rollback as rollback_fixtures
import test_ship_support as support


def archive(pair=None, entries=None, compression=zipfile.ZIP_DEFLATED):
    pair = pair or fixtures.pair()
    data = io.BytesIO()
    with warnings.catch_warnings(), zipfile.ZipFile(data, "w", compression=compression) as target:
        warnings.simplefilter("ignore", UserWarning)
        for name, raw in entries if entries is not None else (
                ("compat-result.json", pair.result), ("compat-context.json", pair.context)):
            if isinstance(name, str):
                info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                info.compress_type = compression
                info.external_attr = ((0o40775 << 16) | 0x10) if name.endswith('/') else (0o600 << 16)
                name = info
            target.writestr(name, raw)
    return data.getvalue()


def location(row=None):
    row = row or fixtures.attempt()
    return adapter.ArtifactLocation(10100, f"compat-result-{row.sha}-{row.run_attempt}",
                                    row.run_id, row.run_attempt, row.sha, False)


class SyntheticGh:
    def __init__(self, row):
        self.row = row
        self.calls = []
        self.mutate = lambda value, path: None
        self.after = lambda path: None
        self.fail_name = None

    def __call__(self, argv, **kwargs):
        self.calls.append((argv, kwargs))
        if argv[1:] == ["--version"]:
            return subprocess.CompletedProcess(argv, 0, b"gh version 2.98.0 (synthetic)\n", b"")
        if argv[1:3] != ["attestation", "verify"]:
            raise AssertionError("Unexpected non-verification command")
        path = Path(argv[3])
        if path.name == self.fail_name:
            return subprocess.CompletedProcess(argv, 1, b"PRIVATE output", b"PRIVATE error")
        row = self.row
        invocation = f"https://github.com/{evidence.REPOSITORY}/actions/runs/{row.run_id}/attempts/{row.run_attempt}"
        repo = "https://github.com/" + evidence.REPOSITORY
        cert = {"issuer": evidence.ISSUER, "buildSignerURI": evidence.SIGNER,
                "subjectAlternativeName": evidence.SIGNER, "buildSignerDigest": row.sha,
                "sourceRepositoryDigest": row.sha, "sourceRepositoryRef": evidence.REF,
                "sourceRepositoryURI": repo, "runnerEnvironment": "github-hosted",
                "runInvocationURI": invocation, "buildTrigger": row.event}
        statement = {"_type": "https://in-toto.io/Statement/v1", "predicateType": evidence.PREDICATE,
                     "subject": [{"name": path.name, "digest": {"sha256": fixtures.digest(path.read_bytes())}}],
                     "predicate": {"buildDefinition": {
                         "buildType": "https://actions.github.io/buildtypes/workflow/v1",
                         "externalParameters": {"workflow": {"path": evidence.WORKFLOW,
                             "ref": evidence.REF, "repository": repo}},
                         "resolvedDependencies": [{"digest": {"gitCommit": row.sha},
                             "uri": f"git+{repo}@{evidence.REF}"}]},
                         "runDetails": {"builder": {"id": evidence.SIGNER},
                             "metadata": {"invocationId": invocation}}}}
        value = [{"verificationResult": {"signature": {"certificate": cert}, "statement": statement}}]
        self.mutate(value, path)
        self.after(path)
        return subprocess.CompletedProcess(argv, 0, json.dumps(value).encode(), b"PRIVATE unused stderr")


@contextmanager
def harness(row=None):
    row = row or fixtures.attempt()
    fake = SyntheticGh(row)
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary).resolve()
        client = records.Gh("synthetic-gh", support.GH_TOKEN, evidence.REPOSITORY,
            expected_version=support.GH_VERSION, runner=fake, config_dir=str(root / "gh-config"))
        yield root, client, fake, row
        assert list(root.iterdir()) == [], "Owned verification directory was not removed"


class AdapterTests(unittest.TestCase):
    def reject(self, client, raw, meta, row, root):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            adapter.verify_pair(client, raw, meta, row, root)
        self.assertEqual(caught.exception.code, "compat-stale")
        self.assertNotIn("PRIVATE", str(caught.exception))
        self.assertNotIn(str(root), str(caught.exception))

    def test_archive_fixture_is_independent_of_wall_clock(self):
        pair = fixtures.pair()
        moments = ((2026, 1, 1, 0, 0, 0, 3, 1, -1), (2026, 1, 1, 0, 0, 4, 3, 1, -1))
        for compression in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED, zipfile.ZIP_BZIP2):
            with self.subTest(compression=compression):
                with mock.patch.object(zipfile.time, 'localtime', return_value=moments[0]):
                    first = archive(pair, compression=compression)
                with mock.patch.object(zipfile.time, 'localtime', return_value=moments[1]):
                    second = archive(pair, compression=compression)
                self.assertEqual(first, second)
                self.assertEqual(fixtures.digest(first), fixtures.digest(second))
                with zipfile.ZipFile(io.BytesIO(first)) as saved:
                    rows = saved.infolist()
                    self.assertEqual([row.filename for row in rows], ['compat-result.json', 'compat-context.json'])
                    self.assertTrue(all(row.date_time == (1980, 1, 1, 0, 0, 0) for row in rows))
                    self.assertTrue(all(row.compress_type == compression for row in rows))
                    self.assertTrue(all(row.external_attr == 0o600 << 16 for row in rows))
                    self.assertEqual([saved.read(row) for row in rows], [pair.result, pair.context])
        # Caller-owned special metadata must remain available to adversarial tests.
        custom = zipfile.ZipInfo('special', date_time=(2004, 2, 3, 4, 5, 6))
        custom.create_system = 3
        custom.external_attr = (stat.S_IFLNK | 0o600) << 16
        custom.compress_type = zipfile.ZIP_STORED
        raw = archive(entries=[(custom, b'target')], compression=zipfile.ZIP_DEFLATED)
        with zipfile.ZipFile(io.BytesIO(raw)) as saved:
            row = saved.infolist()[0]
            self.assertEqual((row.date_time, row.create_system, row.external_attr, row.compress_type),
                ((2004, 2, 3, 4, 5, 6), 3, (stat.S_IFLNK | 0o600) << 16, zipfile.ZIP_STORED))
            self.assertEqual(saved.read(row), b'target')

    def test_actual_gh_wrapper_verifies_both_subjects_with_exact_flags(self):
        with harness() as (root, client, fake, row):
            pair = adapter.verify_pair(client, archive(), location(), row, root)
            self.assertTrue(evidence.parse_authenticated_pair(pair, row).positive)
            self.assertEqual(pair.result, fixtures.pair().result)
            self.assertEqual(pair.context, fixtures.pair().context)
            commands = [args for args, _ in fake.calls[1:]]
            self.assertEqual([Path(c[3]).name for c in commands], list(adapter.FILES))
            for args, kwargs in fake.calls[1:]:
                for flag, expected in (("--cert-identity", evidence.SIGNER), ("--signer-digest", row.sha),
                        ("--source-digest", row.sha), ("--source-ref", evidence.REF),
                        ("--predicate-type", evidence.PREDICATE), ("--repo", evidence.REPOSITORY),
                        ("--cert-oidc-issuer", evidence.ISSUER), ("--format", "json")):
                    self.assertEqual(args[args.index(flag) + 1], expected)
                self.assertIn("--deny-self-hosted-runners", args)
                self.assertEqual(kwargs["stdin"], subprocess.DEVNULL)
                self.assertEqual(kwargs["env"]["GH_TOKEN"], support.GH_TOKEN)
            self.assertNotEqual(pair.result_subject.verification_reference, pair.context_subject.verification_reference)

    def test_complete_negative_is_authentic_evidence_and_not_qualification(self):
        original = fixtures.pair(server_false="workers")
        row = fixtures.attempt(measurement=original, conclusion="failure")
        with harness(row) as (root, client, fake, row):
            pair = adapter.verify_pair(client, archive(original), location(row), row, root)
            self.assertFalse(evidence.parse_authenticated_pair(pair, row).positive)
            decision = evidence.assess_bypass_history(row.sha, fixtures.history(replace(row, measurement=pair)))
            self.assertEqual(decision.state, evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_every_certificate_field_and_both_subjects_are_bound(self):
        for filename in adapter.FILES:
            for field in ("issuer", "buildSignerURI", "subjectAlternativeName", "buildSignerDigest",
                    "sourceRepositoryDigest", "sourceRepositoryRef", "sourceRepositoryURI",
                    "runnerEnvironment", "runInvocationURI", "buildTrigger"):
                with self.subTest(file=filename, field=field), harness() as (root, client, fake, row):
                    def mutate(value, path):
                        if path.name == filename:
                            value[0]["verificationResult"]["signature"]["certificate"][field] = "PRIVATE foreign"
                    fake.mutate = mutate
                    self.reject(client, archive(), location(), row, root)

    def test_correct_predicate_cannot_override_wrong_certificate_attempt(self):
        with harness() as (root, client, fake, row):
            fake.mutate = lambda v, p: v[0]["verificationResult"]["signature"]["certificate"].update(
                runInvocationURI=f"https://github.com/{evidence.REPOSITORY}/actions/runs/{row.run_id}/attempts/2")
            self.reject(client, archive(), location(), row, root)

    def test_each_subject_and_provenance_binding_is_required(self):
        mutations = [lambda s: s.update(_type="other"), lambda s: s.update(predicateType="other"),
            lambda s: s["subject"][0].update(name="other.json"),
            lambda s: s["subject"][0]["digest"].update(sha256="f" * 64),
            lambda s: s["subject"].append(copy.deepcopy(s["subject"][0])),
            lambda s: s["predicate"]["runDetails"]["metadata"].update(invocationId="other"),
            lambda s: s["predicate"]["runDetails"]["builder"].update(id="other"),
            lambda s: s["predicate"]["buildDefinition"].update(buildType="other"),
            lambda s: s["predicate"]["buildDefinition"]["externalParameters"]["workflow"].update(ref="other"),
            lambda s: s["predicate"]["buildDefinition"]["resolvedDependencies"][0]["digest"].update(gitCommit=fixtures.OTHER),
            lambda s: s.pop("predicate")]
        for mutate in mutations:
            with self.subTest(mutation=mutate), harness() as (root, client, fake, row):
                fake.mutate = lambda value, path: mutate(value[0]["verificationResult"]["statement"])
                self.reject(client, archive(), location(), row, root)

    def test_signature_failure_on_either_file_is_content_free(self):
        for filename in adapter.FILES:
            with self.subTest(filename=filename), harness() as (root, client, fake, row):
                fake.fail_name = filename
                self.reject(client, archive(), location(), row, root)

    def test_metadata_and_attempt_mismatch_refuse_before_any_verification(self):
        for field, value in (("artifact_id", True), ("name", "other"), ("run_id", 101),
                ("run_attempt", 2), ("run_attempt", True), ("sha", fixtures.OTHER), ("expired", True)):
            with self.subTest(field=field), harness() as (root, client, fake, row):
                self.reject(client, archive(), replace(location(), **{field: value}), row, root)
                self.assertEqual(len(fake.calls), 1)
        for field, value in (("run_id", True), ("run_number", True), ("run_attempt", True),
                ("event", "pull_request"), ("ref", "refs/heads/other"), ("workflow_sha", fixtures.OTHER)):
            with self.subTest(field=field), harness() as (root, client, fake, row):
                self.reject(client, archive(), location(), replace(row, **{field: value}), root)
                self.assertEqual(len(fake.calls), 1)

    def test_archive_inventory_aliases_and_duplicates_refuse_before_verification(self):
        pair = fixtures.pair()
        valid = [("compat-result.json", pair.result), ("compat-context.json", pair.context)]
        cases = [valid[:1], valid + [("extra", b"x")], [valid[0], valid[0]],
            [("../compat-result.json", pair.result), valid[1]],
            [("COMPAT-RESULT.json", pair.result), valid[1]],
            [("nested/compat-result.json", pair.result), valid[1]],
            [("compat-result.json/", pair.result), valid[1]]]
        for entries in cases:
            with self.subTest(entries=[str(x[0]) for x in entries]), harness() as (root, client, fake, row):
                self.reject(client, archive(entries=entries), location(), row, root)
                self.assertEqual(len(fake.calls), 1)

    def test_special_files_unsupported_compression_and_sizes_refuse(self):
        pair = fixtures.pair()
        for mode in (stat.S_IFLNK, stat.S_IFDIR, stat.S_IFCHR, stat.S_IFIFO):
            info = zipfile.ZipInfo("compat-result.json")
            info.create_system = 3; info.external_attr = (mode | 0o600) << 16
            with self.subTest(mode=mode), harness() as (root, client, fake, row):
                self.reject(client, archive(entries=[(info, pair.result), ("compat-context.json", pair.context)]), location(), row, root)
                self.assertEqual(len(fake.calls), 1)
        cases = [archive(compression=zipfile.ZIP_BZIP2), archive(entries=[("compat-result.json", b"x" * 16385),
            ("compat-context.json", pair.context)]), archive(entries=[("compat-result.json", pair.result),
            ("compat-context.json", b"x" * 4097)]), b"x" * (adapter.MAX_ARCHIVE + 1), b"", b"invalid"]
        for raw in cases:
            with self.subTest(size=len(raw)), harness() as (root, client, fake, row):
                self.reject(client, raw, location(), row, root)
                self.assertEqual(len(fake.calls), 1)

    def test_crc_corruption_and_truncation_refuse(self):
        original = archive(compression=zipfile.ZIP_STORED)
        raw = fixtures.pair().result
        damaged = original.replace(raw, b"!" + raw[1:], 1)
        for data in (damaged, original[:30], original[:-30]):
            with self.subTest(size=len(data)), harness() as (root, client, fake, row):
                self.reject(client, data, location(), row, root)
                self.assertEqual(len(fake.calls), 1)

    def test_encrypted_and_nul_aliased_archive_names_refuse(self):
        pair = fixtures.pair()
        aliased = archive(entries=[("compat-result.jsonX", pair.result), ("compat-context.json", pair.context)])
        aliased = aliased.replace(b"compat-result.jsonX", b"compat-result.json\x00")
        encrypted = bytearray(archive())
        for marker, offset in ((b"PK\x03\x04", 6), (b"PK\x01\x02", 8)):
            at = encrypted.index(marker)
            encrypted[at + offset] |= 1
        for data in (aliased, bytes(encrypted)):
            with harness() as (root, client, fake, row):
                self.reject(client, data, location(), row, root)
                self.assertEqual(len(fake.calls), 1)

    def test_noncanonical_and_cross_bound_pair_still_refuse_after_signing(self):
        original = fixtures.pair()
        for pair in (replace(original, result=original.result + b" "),
                     replace(original, context=original.context.replace(b'"run_attempt":1', b'"run_attempt":2'))):
            with harness() as (root, client, fake, row):
                self.reject(client, archive(pair), location(), row, root)

    def test_verifier_time_file_changes_and_extras_refuse(self):
        for mode in ("own", "other", "extra"):
            with self.subTest(mode=mode), harness() as (root, client, fake, row):
                def change(path):
                    target = path if mode == "own" else path.parent / ("compat-context.json" if mode == "other" else "unexpected")
                    target.write_bytes(b"PRIVATE changed")
                fake.after = change
                self.reject(client, archive(), location(), row, root)
                self.assertEqual(len(fake.calls), 2)

    def test_linked_scratch_is_not_adopted(self):
        with harness() as (root, client, fake, row):
            original = Path.is_symlink
            with mock.patch.object(Path, "is_symlink", lambda p: p == root or original(p)):
                self.reject(client, archive(), location(), row, root)
            with mock.patch.object(Path, "is_junction", lambda p: p == root, create=True):
                self.reject(client, archive(), location(), row, root)
            self.assertEqual(len(fake.calls), 1)

    def test_all_returned_attestations_are_checked_and_output_is_bounded(self):
        for mode in ("contradiction", "many", "large"):
            with self.subTest(mode=mode), harness() as (root, client, fake, row):
                def mutate(value, path):
                    if mode == "contradiction":
                        value.append(copy.deepcopy(value[0]))
                        value[1]["verificationResult"]["signature"]["certificate"]["runInvocationURI"] = "PRIVATE wrong"
                    elif mode == "many":
                        value.extend(copy.deepcopy(value[0]) for _ in range(30))
                    else:
                        value[0]["unused"] = "x" * adapter.MAX_VERIFICATION
                fake.mutate = mutate
                self.reject(client, archive(), location(), row, root)


class RollbackAdapterTests(unittest.TestCase):
    def reject(self, client, raw, meta, row, root):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            adapter.verify_rollback_pair(client, raw, meta, row, root)
        self.assertEqual(caught.exception.code, "compat-stale")
        self.assertNotIn("PRIVATE", str(caught.exception))
        self.assertNotIn(str(root), str(caught.exception))

    def test_exact_dispatch_pair_uses_actual_gh_wrapper_for_both_subjects(self):
        original = rollback_fixtures.pair()
        with harness(rollback_fixtures.attempt(measurement=original)) as (root, client, fake, row):
            pair = adapter.verify_rollback_pair(client, archive(original), location(row), row, root)
            measured = evidence.parse_authenticated_rollback_pair(pair, row)
            self.assertTrue(measured.positive)
            self.assertEqual((pair.result, pair.context), (original.result, original.context))
            self.assertEqual([Path(args[3]).name for args, _ in fake.calls[1:]], list(adapter.FILES))
            self.assertNotEqual(pair.result_subject.verification_reference,
                                pair.context_subject.verification_reference)
            for args, _ in fake.calls[1:]:
                for flag, value in (("--signer-digest", row.sha), ("--source-digest", row.sha),
                        ("--source-ref", evidence.REF), ("--cert-identity", evidence.SIGNER),
                        ("--cert-oidc-issuer", evidence.ISSUER), ("--predicate-type", evidence.PREDICATE)):
                    self.assertEqual(args[args.index(flag) + 1], value)

    def test_complete_failed_measurement_is_returned_without_success_filter(self):
        for fields in ({"server_false": "workers"}, {"migration_false": "rw"},
                       {"forward": False}, {"catalog_sha256": "9" * 64, "forward": False}):
            original = rollback_fixtures.pair(**fields)
            with self.subTest(fields=fields), harness(rollback_fixtures.attempt(
                    measurement=original, conclusion="failure")) as (root, client, fake, row):
                pair = adapter.verify_rollback_pair(client, archive(original), location(row), row, root)
                self.assertFalse(evidence.parse_authenticated_rollback_pair(pair, row).positive)
                self.assertEqual(pair.result, original.result)
                self.assertEqual(len(fake.calls), 3)

    def test_normal_contexts_and_rollback_context_cannot_cross_named_adapters(self):
        for version in (1, 2, 3):
            original = fixtures.pair(version=version, event="workflow_dispatch")
            with self.subTest(version=version), harness(fixtures.attempt(
                    event="workflow_dispatch", measurement=original)) as (root, client, fake, row):
                self.reject(client, archive(original), location(row), row, root)
                self.assertEqual(len(fake.calls), 3)
        original = rollback_fixtures.pair()
        with harness(rollback_fixtures.attempt(measurement=original)) as (root, client, fake, row):
            with self.assertRaises(evidence.CompatRefusal):
                adapter.verify_pair(client, archive(original), location(row), row, root)
            self.assertEqual(len(fake.calls), 3)

    def test_each_signature_and_exact_run_attempt_are_required(self):
        original = rollback_fixtures.pair()
        for filename in adapter.FILES:
            for mode in ("missing", "foreign-attempt"):
                with self.subTest(file=filename, mode=mode), harness(
                        rollback_fixtures.attempt(measurement=original)) as (root, client, fake, row):
                    if mode == "missing":
                        fake.fail_name = filename
                    else:
                        def mutate(value, path):
                            if path.name == filename:
                                value[0]["verificationResult"]["signature"]["certificate"].update(
                                    runInvocationURI=f"https://github.com/{evidence.REPOSITORY}/actions/runs/{row.run_id}/attempts/2")
                        fake.mutate = mutate
                    self.reject(client, archive(original), location(row), row, root)

    def test_context_limit_inventory_and_file_custody_still_apply(self):
        original = rollback_fixtures.pair()
        cases = [archive(entries=[("compat-result.json", original.result)]),
                 archive(entries=[("compat-result.json", original.result),
                                  ("compat-context.json", b"x" * 4097)])]
        for raw in cases:
            with harness(rollback_fixtures.attempt(measurement=original)) as (root, client, fake, row):
                self.reject(client, raw, location(row), row, root)
                self.assertEqual(len(fake.calls), 1)
        with harness(rollback_fixtures.attempt(measurement=original)) as (root, client, fake, row):
            fake.after = lambda path: path.write_bytes(b"PRIVATE changed")
            self.reject(client, archive(original), location(row), row, root)
            self.assertEqual(len(fake.calls), 2)

    def test_push_cannot_be_reinterpreted_as_rollback_dispatch(self):
        original = rollback_fixtures.pair()
        row = replace(rollback_fixtures.attempt(measurement=original), event="push")
        with harness(row) as (root, client, fake, row):
            self.reject(client, archive(original), location(row), row, root)


class HistoryAdapterTests(unittest.TestCase):
    def test_each_closed_protocol_authenticates_each_subject_once(self):
        for version in (1, 2, 3, 4):
            original = (rollback_fixtures.pair(server_false="workers") if version == 4
                        else fixtures.pair(version=version, event="workflow_dispatch", server_false="workers"))
            row = (rollback_fixtures.attempt(measurement=original, conclusion="failure") if version == 4
                   else fixtures.attempt(event="workflow_dispatch", measurement=original, conclusion="failure"))
            with self.subTest(version=version), harness(row) as (root, client, fake, row):
                pair = adapter.verify_history_pair(client, archive(original), location(row), row, root)
                parser = (evidence.parse_authenticated_rollback_pair if version == 4
                          else evidence.parse_authenticated_pair)
                self.assertFalse(parser(pair, row).positive)
                self.assertEqual((pair.result, pair.context), (original.result, original.context))
                self.assertEqual([Path(args[3]).name for args, _ in fake.calls[1:]], list(adapter.FILES))

    def test_unknown_bool_and_malformed_protocols_never_retry_a_signature(self):
        for version in (0, 5, True, "4"):
            original = rollback_fixtures.pair()
            context = json.loads(original.context); context["version"] = version
            original = replace(original, context=fixtures.canonical(context))
            with self.subTest(version=version), harness(rollback_fixtures.attempt()) as (root, client, fake, row):
                with self.assertRaises(evidence.CompatRefusal):
                    adapter.verify_history_pair(client, archive(original), location(row), row, root)
                self.assertEqual(len(fake.calls), 3)

    def test_named_normal_and_rollback_adapters_keep_namespace_restriction(self):
        original = fixtures.pair(version=3, event="workflow_dispatch")
        with harness(fixtures.attempt(event="workflow_dispatch")) as (root, client, fake, row):
            pair = adapter.verify_history_pair(client, archive(original), location(row), row, root)
            self.assertTrue(evidence.parse_authenticated_pair(pair, row).positive)
            with self.assertRaises(evidence.CompatRefusal):
                evidence.parse_authenticated_rollback_pair(pair, row)
            self.assertEqual(len(fake.calls), 3)

    def test_history_verifier_preserves_both_signature_requirements(self):
        original = rollback_fixtures.pair()
        for filename in adapter.FILES:
            with self.subTest(file=filename), harness(rollback_fixtures.attempt()) as (root, client, fake, row):
                fake.fail_name = filename
                with self.assertRaises(evidence.CompatRefusal) as caught:
                    adapter.verify_history_pair(client, archive(original), location(row), row, root)
                self.assertNotIn("PRIVATE", str(caught.exception))
                self.assertLessEqual(len(fake.calls), 3)


if __name__ == "__main__":
    unittest.main()
