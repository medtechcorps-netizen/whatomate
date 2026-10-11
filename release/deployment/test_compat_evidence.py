"""Synthetic policy tests: receipt construction is not signature verification."""
from dataclasses import asdict, FrozenInstanceError, replace
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat_evidence as evidence

SHA = "a" * 40
OTHER = "b" * 40
HASH = "c" * 64
CATALOG = "d" * 64
OBSERVATION = "e" * 64
T0 = datetime(2026, 10, 11, tzinfo=timezone.utc)
LATEST = evidence.ReleaseIdentity("prod-20261010T234723Z-bbbbbbbb", "1" * 64, OTHER, "sha256:" + "2" * 64)
CLASS = evidence.Classification("none", False)
AUTHORITY = tuple(evidence.CatalogBlob(p, HASH) for p in evidence.CATALOG_PATHS)


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode("ascii")


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def request(classification=CLASS, latest=LATEST):
    return evidence.NormalRequest(evidence.SourceFacts(SHA, latest, classification, CATALOG, AUTHORITY), latest, OBSERVATION)


def subject(name, raw, run, attempt):
    return evidence.VerifiedSubject(name, digest(raw), evidence.PREDICATE, evidence.ISSUER, evidence.SIGNER,
        SHA, SHA, evidence.REF, "github-hosted", run, attempt, HASH)


def pack(result, context, run=100, attempt=1):
    """Model an adapter's verified-byte boundary; performs no cryptography."""
    result_raw = result if type(result) is bytes else canonical(result)
    context_raw = context if type(context) is bytes else canonical(context)
    artifact = evidence.Artifact(run + 10_000, f"compat-result-{SHA}-{attempt}", run, attempt, SHA, False,
        (evidence.ArtifactFile("compat-result.json", digest(result_raw)),
         evidence.ArtifactFile("compat-context.json", digest(context_raw))))
    return evidence.AuthenticatedPair(result_raw, context_raw, subject("compat-result.json", result_raw, run, attempt),
        subject("compat-context.json", context_raw, run, attempt), artifact)


def pair(run=100, attempt=1, *, classification=CLASS, latest=LATEST, selected=None, target=None,
         event="push", server_false=None, migration_false=None, forward=True):
    selected = selected or latest
    result = {"sha": SHA, "previous_release": selected.release, "previous_web_digest": selected.web_digest,
        "classification": {"kind": classification.kind, "dormant": classification.dormant}, "forward": forward,
        "old_migrate_ok": {key: key != migration_false for key in evidence.MIGRATIONS},
        "old_server_ok": {key: key != server_false for key in evidence.SERVERS},
        "reversible": migration_false is None, "catalog_sha256": CATALOG, "run_id": run, "run_attempt": attempt}
    result_raw = canonical(result)
    context = {"version": 1, "repository": evidence.REPOSITORY, "workflow_path": evidence.WORKFLOW,
        "workflow_sha": SHA, "event": event, "ref": evidence.REF, "sha": SHA, "run_id": run, "run_attempt": attempt,
        "requested_rollback_target": target, "latest": asdict(latest), "selected": asdict(selected),
        "preview": {"kind": classification.kind, "dormant": classification.dormant, "blocked": False},
        "result_sha256": digest(result_raw)}
    return pack(result_raw, context, run, attempt)


def attempt(run=100, number=10, rerun=1, latest_attempt=None, minute=10, *, measurement=None,
            conclusion="success", event="push", status="completed"):
    jobs = tuple(evidence.Job(name, run * 100 + i, run * 1000 + i, 15368, run, rerun, SHA,
        "completed", "success" if conclusion == "success" else "failure") for i, name in enumerate(evidence.JOBS, 1))
    return evidence.Attempt(evidence.REPOSITORY, evidence.WORKFLOW, SHA, SHA, event, evidence.REF, run, number,
        rerun, latest_attempt or rerun, status, conclusion, T0 + timedelta(minutes=minute),
        T0 + timedelta(minutes=minute + 1), T0 + timedelta(minutes=minute + 2), jobs,
        pair(run, rerun, event=event) if measurement is None else measurement)


def history(*rows, **changes):
    return replace(evidence.VisibleHistory(SHA, evidence.REPOSITORY, evidence.WORKFLOW, OBSERVATION,
        T0 + timedelta(days=1), tuple(rows)), **changes)


def edit_pair(original, *, result=None, context=None):
    r, c = json.loads(original.result), json.loads(original.context)
    if result:
        result(r)
    if context:
        context(c)
    c["result_sha256"] = digest(canonical(r))
    return pack(r, c, r["run_id"], r["run_attempt"])


class QualificationTests(unittest.TestCase):
    def refuse(self, rows=(), req=None, code="compat-stale", hist=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            evidence.require_compat(req or request(), hist or history(*rows))
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn("PRIVATE", str(caught.exception))
        return caught.exception

    def test_positive_receipt_binds_exact_bytes_catalog_source_and_attempt(self):
        row = attempt()
        qualified = evidence.require_compat(request(), history(row))
        self.assertEqual(qualified.previous, LATEST)
        self.assertEqual((qualified.run_id, qualified.run_number, qualified.run_attempt), (100, 10, 1))
        self.assertEqual(qualified.result_sha256, digest(row.measurement.result))
        self.assertEqual(qualified.context_sha256, digest(row.measurement.context))
        self.assertEqual(qualified.catalog_authority, AUTHORITY)
        self.assertEqual(qualified.catalog_sha256, CATALOG)
        self.assertEqual(qualified.observation_id, OBSERVATION)
        self.assertTrue(qualified.forward and qualified.reversible and qualified.old_server)
        self.assertEqual(qualified.old_migrate_ok, (True, True, True))
        self.assertEqual(qualified.old_server_ok, (True,) * 5)
        with self.assertRaises(FrozenInstanceError): qualified.run_id = 999

    def test_dormant_data_is_qualified_and_active_classes_are_not_supported(self):
        dormant = evidence.Classification("data", True)
        row = attempt(measurement=pair(classification=dormant))
        self.assertEqual(evidence.require_compat(request(dormant), history(row)).classification, dormant)
        for c in (evidence.Classification("data", False), evidence.Classification("expand", False),
                  evidence.Classification("contract", False), evidence.Classification("none", True),
                  evidence.Classification("none", False, ("migration-path-changed",))):
            with self.subTest(c=c): self.refuse((attempt(),), req=request(c))

    def test_missing_and_each_unknown_state_never_become_false_flags(self):
        self.refuse(code="compat-missing")
        for state in evidence.MissingEvidence:
            with self.subTest(state=state): self.refuse((attempt(measurement=state),))

    def test_each_real_server_negative_has_distinct_policy_refusal(self):
        for flag in evidence.SERVERS:
            with self.subTest(flag=flag):
                self.refuse((attempt(measurement=pair(server_false=flag), conclusion="failure"),),
                            code="compat-old-server-incompatible")

    def test_migration_and_forward_negatives_are_not_success_or_infrastructure(self):
        for flag in ("rw", "ro"):
            with self.subTest(flag=flag):
                self.refuse((attempt(measurement=pair(migration_false=flag), conclusion="failure"),),
                            code="irreversible-needs-fix-forward-mode")
        self.refuse((attempt(measurement=pair(forward=False), conclusion="failure"),))
        self.refuse((attempt(measurement=pair(migration_false="negative_control"), conclusion="failure"),))

    def test_fresh_latest_all_four_identity_fields_and_source_base_are_required(self):
        for field, value in (("release", "prod-20261011T000000Z-bbbbbbbb"), ("manifest_sha256", "3" * 64),
                             ("sha", "c" * 40), ("web_digest", "sha256:" + "4" * 64)):
            changed = replace(LATEST, **{field: value})
            with self.subTest(field=field): self.refuse((attempt(),), req=request(latest=changed))
        self.refuse((attempt(),), req=replace(request(), latest=replace(LATEST, manifest_sha256="3" * 64)))

    def test_current_classification_catalog_and_authority_are_independent(self):
        self.refuse((attempt(),), req=request(evidence.Classification("data", True)))
        base = request()
        for source in (replace(base.source, expected_after_catalog_sha256="7" * 64),
                       replace(base.source, catalog_authority=AUTHORITY[:-1]),
                       replace(base.source, catalog_authority=tuple(reversed(AUTHORITY))),
                       replace(base.source, catalog_authority=(replace(AUTHORITY[0], sha256="PRIVATE"),) + AUTHORITY[1:])):
            self.refuse((attempt(),), req=replace(base, source=source))
        self.refuse((attempt(),), req=replace(base, mode="rollback"))

    def test_whole_run_and_all_four_publisher_bound_jobs_must_succeed(self):
        row = attempt()
        mutations = [replace(row, conclusion="failure"), replace(row, jobs=row.jobs[:-1]),
                     replace(row, jobs=(replace(row.jobs[0], app_id=1),) + row.jobs[1:]),
                     replace(row, jobs=(replace(row.jobs[0], run_attempt=2),) + row.jobs[1:]),
                     replace(row, jobs=(replace(row.jobs[0], sha=OTHER),) + row.jobs[1:]),
                     replace(row, jobs=(replace(row.jobs[0], conclusion="skipped"),) + row.jobs[1:]),
                     replace(row, jobs=(row.jobs[1],) + row.jobs[1:])]
        for changed in mutations:
            with self.subTest(changed=changed.jobs): self.refuse((changed,))

    def test_canonical_number_then_successful_attempt_selects_not_list_order_or_run_id(self):
        older = attempt(run=999, number=9, minute=1)
        first = attempt(latest_attempt=2, minute=5)
        second = attempt(rerun=2, minute=10)
        for rows in ((second, older, first), (older, first, second)):
            q = evidence.require_compat(request(), history(*rows))
            self.assertEqual((q.run_id, q.run_attempt), (100, 2))

    def test_later_execution_of_lower_number_negative_vetoes_newer_number_positive(self):
        first = attempt(run=99, number=9, latest_attempt=2, minute=1)
        rerun = attempt(run=99, number=9, rerun=2, minute=20,
            measurement=pair(99, 2, server_false="workers"), conclusion="failure")
        self.refuse((first, attempt(), rerun), code="compat-old-server-incompatible")

    def test_higher_number_earlier_failure_does_not_permanently_veto_fresh_rerun(self):
        first = attempt(latest_attempt=2, minute=1, measurement=evidence.MissingEvidence.MISSING, conclusion="failure")
        higher = attempt(run=101, number=11, minute=5, measurement=pair(101, server_false="boot"), conclusion="failure")
        second = attempt(rerun=2, minute=20)
        q = evidence.require_compat(request(), history(higher, second, first))
        self.assertEqual((q.run_id, q.run_attempt), (100, 2))

    def test_later_and_overlapping_incomplete_attempts_veto(self):
        for minute in (9, 12, 20):
            for state in evidence.MissingEvidence:
                other = attempt(run=101, number=11, minute=minute, measurement=state, conclusion="failure")
                with self.subTest(minute=minute, state=state): self.refuse((attempt(), other))
        active = replace(attempt(run=101, number=11, minute=20, measurement=evidence.MissingEvidence.ACTIVE),
                         status="in_progress", conclusion=None, completed_at=None, jobs=())
        self.refuse((attempt(), active))

    def test_overlapping_negative_and_unknown_timing_refuse(self):
        other = attempt(run=101, number=11, minute=9, measurement=pair(101, migration_false="ro"), conclusion="failure")
        self.refuse((attempt(), other), code="irreversible-needs-fix-forward-mode")
        for field in ("requested_at", "started_at", "completed_at"):
            self.refuse((attempt(), replace(other, **{field: None})))
        self.refuse((replace(attempt(), requested_at=None),))

    def test_older_unknown_or_unqualified_evidence_can_be_superseded(self):
        old = attempt(run=99, number=9, minute=1, measurement=evidence.MissingEvidence.EXPIRED, conclusion="failure")
        self.assertEqual(evidence.require_compat(request(), history(old, attempt())).run_id, 100)
        bad = replace(pair(99), context=b"PRIVATE not JSON")
        old = replace(old, measurement=bad)
        self.assertEqual(evidence.require_compat(request(), history(old, attempt())).run_id, 100)
        for field in ("requested_at", "started_at", "completed_at"):
            with self.subTest(missing=field): self.refuse((replace(old, **{field: None}), attempt()))

    def test_later_positive_may_coexist_but_canonical_candidate_selection_stays_numbered(self):
        lower = attempt(run=99, number=9, minute=20)
        self.assertEqual(evidence.require_compat(request(), history(attempt(), lower)).run_id, 100)

    def test_history_duplicates_gaps_run_identity_and_timing_refuse(self):
        row = attempt()
        rows = [(row, row), (replace(row, latest_run_attempt=2),),
                (row, attempt(run=101, number=10)),
                (replace(row, latest_run_attempt=2), attempt(number=11, rerun=2)),
                (replace(row, latest_run_attempt=2, requested_at=T0 + timedelta(minutes=15), started_at=None, completed_at=None),
                 attempt(rerun=2, minute=10)),
                (replace(row, completed_at=T0),), (replace(row, requested_at=T0.replace(tzinfo=None)),),
                (replace(row, completed_at=T0 + timedelta(days=2)),)]
        for value in rows:
            with self.subTest(rows=value): self.refuse(value)

    def test_history_scope_coverage_and_bounds_refuse(self):
        for changes in ({"sha": OTHER}, {"repository": "foreign/repo"}, {"observation_id": HASH},
                        {"workflow_path": ".github/workflows/other.yml"}, {"enumeration": "incomplete"},
                        {"whole_run_deletion_coverage": "proven-by-pagination"},
                        {"attempts": (attempt(),) * 501}, {"observed_at": T0}):
            with self.subTest(changes=changes): self.refuse(hist=history(attempt(), **changes))

    def test_explicit_rollback_pair_is_not_forward_qualification(self):
        selected = replace(LATEST, release="prod-20261009T000000Z-cccccccc", sha="c" * 40)
        row = attempt(event="workflow_dispatch", measurement=pair(event="workflow_dispatch", target=selected.release, selected=selected))
        self.assertEqual(evidence.parse_authenticated_pair(row.measurement, row).requested_target, selected.release)
        self.refuse((row,))

    def test_policy_functions_perform_no_evidence_or_environment_io(self):
        row, req, hist = attempt(), request(), history(attempt())
        with mock.patch("builtins.open", side_effect=AssertionError("I/O")), \
             mock.patch("subprocess.run", side_effect=AssertionError("I/O")), \
             mock.patch("subprocess.Popen", side_effect=AssertionError("I/O")), \
             mock.patch("socket.socket", side_effect=AssertionError("I/O")), \
             mock.patch("pathlib.Path.read_bytes", side_effect=AssertionError("I/O")), \
             mock.patch("os.getenv", side_effect=AssertionError("I/O")), \
             mock.patch("time.time", side_effect=AssertionError("clock")), \
             mock.patch("time.monotonic", side_effect=AssertionError("clock")):
            self.assertTrue(evidence.parse_authenticated_pair(row.measurement, row).positive)
            self.assertTrue(evidence.require_compat(req, hist).forward)
            self.assertEqual(evidence.assess_bypass_history(SHA, hist).state, evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE)


class PairBoundaryTests(unittest.TestCase):
    def reject(self, value, row=None):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            evidence.parse_authenticated_pair(value, row or attempt())
        self.assertNotIn("PRIVATE", str(caught.exception))

    def test_every_authentication_subject_identity_is_independently_required(self):
        original = pair()
        for side in ("result_subject", "context_subject"):
            receipt = getattr(original, side)
            changes = {"subject_name": "other.json", "subject_sha256": HASH, "predicate_type": "foreign",
                "issuer": "foreign", "signer_uri": "foreign", "signer_sha": OTHER, "source_sha": OTHER,
                "source_ref": "refs/heads/other", "runner": "self-hosted", "run_id": 101, "run_attempt": 2,
                "verification_reference": "PRIVATE"}
            for field, value in changes.items():
                with self.subTest(side=side, field=field): self.reject(replace(original, **{side: replace(receipt, **{field: value})}))
        self.reject(replace(original, context_subject=None))

    def test_artifact_exact_two_regular_files_and_current_attempt_identity(self):
        original = pair(); artifact = original.artifact
        changes = {"artifact_id": True, "name": f"compat-result-{SHA}-2", "run_id": 101, "run_attempt": 2,
            "sha": OTHER, "expired": True, "files": artifact.files + (evidence.ArtifactFile("extra", HASH),)}
        for field, value in changes.items():
            with self.subTest(field=field): self.reject(replace(original, artifact=replace(artifact, **{field: value})))
        for files in (artifact.files[:1], (artifact.files[0],) * 2,
                      (replace(artifact.files[0], kind="symlink"), artifact.files[1]),
                      (replace(artifact.files[0], name="../compat-result.json"), artifact.files[1]),
                      (replace(artifact.files[0], sha256=HASH), artifact.files[1])):
            self.reject(replace(original, artifact=replace(artifact, files=files)))

    def test_context_closed_types_source_attempt_and_selection_bindings(self):
        changes = {"version": True, "repository": "foreign/repo", "workflow_path": "other", "workflow_sha": OTHER,
            "event": "pull_request", "ref": "refs/heads/other", "sha": OTHER, "run_id": True, "run_attempt": 2,
            "requested_rollback_target": LATEST.release, "extra": "PRIVATE"}
        for field, value in changes.items():
            with self.subTest(field=field): self.reject(edit_pair(pair(), context=lambda c: c.update({field: value})))
        for mutate in (lambda c: c.pop("latest"), lambda c: c["selected"].update(manifest_sha256=HASH),
                       lambda c: c["latest"].update(extra="PRIVATE"), lambda c: c["preview"].update(blocked=0),
                       lambda c: c["preview"].update(kind="data", dormant=True)):
            self.reject(edit_pair(pair(), context=mutate))
        r, c = json.loads(pair().result), json.loads(pair().context); c["result_sha256"] = HASH
        self.reject(pack(r, c))

    def test_result_exact_schema_flags_control_catalog_and_identity(self):
        mutations = [lambda r: r.update(sha=OTHER), lambda r: r.update(run_id=True), lambda r: r.update(extra="PRIVATE"),
            lambda r: r.update(previous_release="prod-0000"), lambda r: r.update(previous_web_digest="sha256:" + "5" * 64),
            lambda r: r.update(catalog_sha256="PRIVATE"), lambda r: r.update(forward=1),
            lambda r: r["old_server_ok"].update(boot="false"), lambda r: r["old_migrate_ok"].update(negative_control=False),
            lambda r: r["old_migrate_ok"].update(rw=False), lambda r: r["old_server_ok"].pop("read")]
        for mutate in mutations:
            with self.subTest(mutate=mutate): self.reject(edit_pair(pair(), result=mutate))

    def test_duplicate_noncanonical_nonfinite_encoding_and_bounds(self):
        original = pair()
        result_cases = [original.result[:-1], b" " + original.result,
                        original.result.replace(b'"forward":true', b'"forward":true,"forward":true'),
                        original.result.replace(b'"forward":true', b'"forward":NaN'), b"\xff", b"x" * 16385]
        for raw in result_cases:
            context = json.loads(original.context); context["result_sha256"] = digest(raw)
            with self.subTest(raw=raw[:30]): self.reject(pack(raw, context))
        for raw in (original.context[:-1], b" " + original.context, b"x" * 4097,
                    original.context.replace(b'"version":1', b'"version":1,"version":1'), b"\xff"):
            self.reject(pack(original.result, raw))

    def test_direct_parser_rejects_boolean_attempt_identity(self):
        for field in ("run_id", "run_number", "run_attempt"):
            self.reject(pair(), replace(attempt(), **{field: True}))

    def test_changed_payload_without_reverified_subject_does_not_qualify(self):
        original = pair()
        changed = edit_pair(original, result=lambda r: r["old_server_ok"].update(read=False))
        self.reject(replace(changed, result_subject=original.result_subject))
        self.reject(replace(changed, context_subject=original.context_subject))

    def test_unverified_pair_never_becomes_authenticated_negative(self):
        negative = pair(server_false="read")
        negative = replace(negative, context_subject=replace(negative.context_subject, signer_uri="foreign"))
        row = attempt(measurement=negative, conclusion="failure")
        decision = evidence.assess_bypass_history(SHA, history(row))
        self.assertEqual(decision.state, evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE)
        self.assertEqual(decision.negatives, ())

    def test_real_producer_context_roundtrip_preserves_canonical_contract(self):
        # Load the real producer only in this test; no producer operation runs.
        path = Path(__file__).resolve().parents[1] / "compat/cli.py"
        spec = importlib.util.spec_from_file_location("_compat_producer_contract_test", path)
        producer = importlib.util.module_from_spec(spec); sys.modules[spec.name] = producer
        before = list(sys.path)
        try:
            spec.loader.exec_module(producer)
            ctx = producer.Context(SHA, 100, 1, None, "push", evidence.REF)
            snap = {"version": 2, "sha": SHA, "run_id": 100, "run_attempt": 1,
                "requested_rollback_target": None, "latest": asdict(LATEST), "selected": asdict(LATEST),
                "preview": {"kind": "none", "dormant": False, "blocked": False}}
            for original in (pair(), pair(server_false="read"), pair(migration_false="rw")):
                raw = producer.canonical(producer.evidence_context(original.result, snap, ctx))
                self.assertEqual(raw, original.context)
                rebound = pack(original.result, raw)
                self.assertEqual(evidence.parse_authenticated_pair(rebound, attempt()).positive,
                                 producer.accepted(json.loads(original.result)))
        finally:
            sys.path[:] = before


class BypassTests(unittest.TestCase):
    def test_visible_pagination_never_proves_no_negative_history(self):
        for h in (history(), history(attempt()), history(attempt(), enumeration="incomplete"),
                  history(attempt(measurement=evidence.MissingEvidence.NEVER_STARTED, conclusion="cancelled"))):
            state = evidence.assess_bypass_history(SHA, h)
            self.assertEqual(state.state, evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE)
            self.assertEqual(state.negatives, ())

    def test_older_negative_still_forbids_bypass_after_later_normal_recovery(self):
        old = attempt(run=99, number=9, minute=1, measurement=pair(99, server_false="boot"), conclusion="failure")
        rows = history(old, attempt())
        self.assertTrue(evidence.require_compat(request(), rows).old_server)
        decision = evidence.assess_bypass_history(SHA, rows)
        self.assertEqual(decision.state, evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)
        self.assertEqual(decision.negatives[0].result_sha256, digest(old.measurement.result))

    def test_any_exact_sha_authenticated_negative_including_explicit_target_vetoes(self):
        target = replace(LATEST, release="prod-0000")
        row = attempt(event="workflow_dispatch", conclusion="failure", measurement=pair(event="workflow_dispatch",
            selected=target, target=target.release, forward=False))
        decision = evidence.assess_bypass_history(SHA, history(row, enumeration="incomplete"))
        self.assertEqual(decision.state, evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE)

    def test_unknown_legacy_or_deleted_history_has_no_grant_state(self):
        for missing in (evidence.MissingEvidence.UNQUALIFIED, evidence.MissingEvidence.MISSING,
                        evidence.MissingEvidence.EXPIRED, evidence.MissingEvidence.SIGNING_FAILED):
            state = evidence.assess_bypass_history(SHA, history(attempt(measurement=missing, conclusion="failure")))
            self.assertEqual(state.state, evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE)
        self.assertEqual(set(evidence.BypassState), {evidence.BypassState.VETO_AUTHENTICATED_NEGATIVE,
                                                   evidence.BypassState.UNAVAILABLE_HISTORY_COVERAGE})


if __name__ == "__main__":
    unittest.main()
