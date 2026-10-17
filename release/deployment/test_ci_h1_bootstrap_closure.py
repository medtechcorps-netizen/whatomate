"""CI source eligibility for one paired H1 bootstrap closure refresh.

Disposable Git histories prove the finite source shape and rejection paths.
Synthetic digests do not prove generated Go hashes or exact compatibility;
those remain separate checks. Existing CISourceGuardTests stay untouched.
"""
from __future__ import annotations

import copy
from contextlib import ExitStack
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import schema_change as sc
import source_git
import test_pending_plan as pending_fixture
import test_ship_schema_change_v2 as fixture
import test_ship_support as support


ADDITIONS = (
    "internal/handlers/inbound_continuation_admission.go",
    "internal/handlers/inbound_continuation_capacity.go",
)
TARGET = "BackfillChatbotFlowGraph"
TARGET_FUNC = "github.com/shridarpatil/whatomate/internal/handlers.BackfillChatbotFlowGraph"
BACKFILL_SOURCE = "internal/handlers/chatbot_flow_migration.go"
PROFILE = "internal/dbcatalog/golden/production-v0.json"
OVERLAY = "internal/dbcatalog/shape/production-v0.sql"
ACTIVE = (sc.CATALOG, sc.HISTORY, sc.GLOBALS, PROFILE, OVERLAY, sc.SEEDS)


def wire(value):
    # Independent Go MarshalIndent-compatible field order for ASCII fixtures.
    return json.dumps(value, indent=2, ensure_ascii=False) + "\n"


def registry_row(identity, *, target=False):
    result = {
        "id": identity,
        "rev": 1,
        "func": TARGET_FUNC if target else "database." + identity,
        "kind": "baseline-pre-ledger" if target else "seed-reconciler",
        "baseline": target,
        "idempotent": not target,
        "n1_safe": not target,
        "requires_gates": [],
        "closure_files": sorted(["cmd/whatomate/main.go", BACKFILL_SOURCE]) if target else ["cmd/whatomate/main.go"],
        "closure_sha256": fixture.H,
    }
    return result


class H1BootstrapClosureCISourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = support.TempRepo(Path(self.temp.name) / "repo")
        other = [registry_row("FixtureSeed%02d" % index) for index in range(20)]
        other[1]["parent"] = other[0]["id"]
        other[-1].update(kind="baseline-pre-ledger", baseline=True, idempotent=False, n1_safe=False)
        self.rows = other[:19] + [registry_row(TARGET, target=True)] + other[19:]
        self.migration = {
            "version": 1,
            "roots": list(sc.MIGRATION_ROOTS),
            "closure_files": sorted(["cmd/whatomate/main.go", "internal/database/tenant.go", BACKFILL_SOURCE]),
            "closure_sha256": fixture.H,
        }
        self.active = {
            sc.CATALOG: fixture.text(fixture.catalog()),
            sc.HISTORY: fixture.text(fixture.history()),
            sc.GLOBALS: "[]\n",
            PROFILE: '{"fixture":"profile"}\n',
            OVERLAY: "-- synthetic overlay\n",
            sc.SEEDS: wire({"version": 0, "synthetic_rows": [{"id": "unchanged-seed", "value": "fixed"}]}),
        }
        self.files = {
            **self.active,
            sc.REGISTRY: wire(self.rows),
            sc.MIGRATION_PATH: wire(self.migration),
            "cmd/whatomate/main.go": fixture.MAIN,
            "internal/database/tenant.go": fixture.TENANT,
            BACKFILL_SOURCE: "package handlers\n\nfunc BackfillChatbotFlowGraph() {}\n",
            "README.md": "synthetic baseline\n",
        }
        self.base = self.repo.commit(self.files)

    def revised(self):
        rows, migration = copy.deepcopy(self.rows), copy.deepcopy(self.migration)
        target = next(row for row in rows if row["id"] == TARGET)
        for value in (target, migration):
            value["closure_files"] = sorted(value["closure_files"] + list(ADDITIONS))
            value["closure_sha256"] = fixture.J
        return rows, migration

    def paired(self, rows=None, migration=None, extra=None):
        default_rows, default_migration = self.revised()
        return self.repo.commit({
            sc.REGISTRY: wire(default_rows if rows is None else rows),
            sc.MIGRATION_PATH: wire(default_migration if migration is None else migration),
            **{path: "package handlers\n" for path in ADDITIONS},
            **(extra or {}),
        })

    def check(self, head, base=None):
        return sc.check_ci_source(self.repo.root, self.base if base is None else base, head)

    def refused(self, head, base=None):
        self.assertTrue(self.check(head, base), "source eligibility must remain refused")

    def target(self, rows):
        return next(row for row in rows if row["id"] == TARGET)

    def test_paired_refresh_is_ci_only_and_preserves_all_twenty_other_rows(self):
        rows, migration = self.revised()
        self.assertEqual(len(rows), 21)
        self.assertEqual([r["id"] for r in rows], [r["id"] for r in self.rows])
        self.assertEqual([r for r in rows if r["id"] != TARGET], [r for r in self.rows if r["id"] != TARGET])
        head = self.paired(rows, migration)
        with ExitStack() as stack:
            for name in ("_ci_seed_removal", "_pr18_observer_transition", "_pr18_managed_owner_local",
                         "_pr18_managed_replication_local", "_pr18_planner_public_owner_local",
                         "_pr18_verification_repair_local", "_pr18_run_metadata_local"):
                stack.enter_context(mock.patch.object(sc, name, side_effect=AssertionError("unrelated admission route")))
            self.assertEqual(self.check(head), ())
        self.assertEqual(sc.check(self.repo.root, self.base, head), ("migration-path-changed:closure",))
        with self.assertRaises(sc.SchemaChangeBlocked):
            sc.guard(self.repo.root, self.base, head)
        policy = sc.classify_v2(self.repo.root, self.base, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons), ("none", False, ()))
        self.assertEqual(policy.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(policy.migration_changes, ("generated migration closure changed",))

    def test_product_predecessors_and_repeated_paired_states_are_allowed(self):
        self.repo.commit({ADDITIONS[0]: "package handlers\n", "README.md": "product before refresh\n"})
        self.repo.commit({ADDITIONS[1]: "package handlers\n"})
        self.paired()
        head = self.repo.commit({"README.md": "ordinary successor\n"})
        self.assertEqual(self.check(head), ())

    def test_only_first_parent_metadata_states_form_the_admitted_transition(self):
        self.repo.git("checkout", "--quiet", "-b", "side-history", self.base)
        side_rows, _ = self.revised()
        self.target(side_rows)["closure_sha256"] = "c" * 64
        self.repo.commit({sc.REGISTRY: wire(side_rows)})
        self.repo.checkout(self.base)
        self.paired()
        self.repo.git("merge", "--quiet", "--no-ff", "--strategy=ours", "-m", "first-parent fixture", "side-history")
        self.assertEqual(self.check(self.repo.head()), ())

    def test_synthetic_pr_merge_accepts_paired_refresh_with_base_as_first_parent(self):
        self.repo.git("checkout", "--quiet", "-b", "synthetic-pr", self.base)
        self.paired()
        self.repo.checkout(self.base)
        self.repo.git("merge", "--quiet", "--no-ff", "-m", "synthetic PR merge", "synthetic-pr")
        head = self.repo.head()
        self.assertEqual(self.repo.git("rev-parse", head + "^1").strip(), self.base)
        self.assertEqual(self.check(head), ())

    def test_ordinary_unchanged_registry_and_legacy_clean_paths_remain(self):
        migration = copy.deepcopy(self.migration)
        migration["closure_sha256"] = fixture.J
        head = self.repo.commit({sc.MIGRATION_PATH: wire(migration)})
        self.assertEqual(self.check(head), ())
        self.repo.checkout(self.base)
        legacy = self.repo.commit({PROFILE: None, OVERLAY: None})
        head = self.repo.commit({"README.md": "legacy clean\n"})
        with mock.patch.object(sc, "_classify_v2", side_effect=AssertionError("legacy path must stay independent")):
            self.assertEqual(self.check(head, legacy), ())

    def test_target_identity_revision_flags_parent_and_gates_are_fixed(self):
        changes = {
            "func": "database.OtherBackfill", "rev": 2, "kind": "seed-reconciler",
            "baseline": False, "idempotent": True, "n1_safe": True,
            "requires_gates": ["synthetic_gate"], "parent": "FixtureSeed00",
        }
        for field, value in changes.items():
            with self.subTest(field=field):
                self.repo.checkout(self.base)
                rows, migration = self.revised()
                self.target(rows)[field] = value
                self.refused(self.paired(rows, migration))
        for field, value in changes.items():
            with self.subTest(both_endpoints=field):
                self.repo.checkout(self.base)
                old = copy.deepcopy(self.rows)
                self.target(old)[field] = value
                base = self.repo.commit({sc.REGISTRY: wire(old)})
                rows, migration = self.revised()
                self.target(rows)[field] = value
                self.refused(self.paired(rows, migration), base)

    def test_target_absence_rename_duplication_and_inventory_reorder_refuse(self):
        for mutation in ("remove", "rename", "duplicate", "new-row", "reorder", "move-target"):
            with self.subTest(mutation=mutation):
                self.repo.checkout(self.base)
                rows, migration = self.revised()
                target = self.target(rows)
                if mutation == "remove":
                    rows.remove(target)
                elif mutation == "rename":
                    target["id"] = "OtherBackfill"
                elif mutation == "duplicate":
                    rows.append(copy.deepcopy(target))
                elif mutation == "new-row":
                    rows.append(registry_row("AddedSeed"))
                elif mutation == "reorder":
                    rows[0], rows[1] = rows[1], rows[0]
                else:
                    rows.remove(target)
                    rows.insert(0, target)
                self.refused(self.paired(rows, migration))
        self.repo.checkout(self.base)
        missing = [copy.deepcopy(row) for row in self.rows if row["id"] != TARGET]
        base = self.repo.commit({sc.REGISTRY: wire(missing)})
        rows, migration = self.revised()
        rows = [row for row in rows if row["id"] != TARGET]
        rows[-1]["closure_files"] = sorted(rows[-1]["closure_files"] + list(ADDITIONS))
        rows[-1]["closure_sha256"] = fixture.J
        self.refused(self.paired(rows, migration), base)

    def test_all_twenty_other_rows_including_seeds_remain_identical(self):
        for index in range(len(self.rows)):
            if self.rows[index]["id"] == TARGET:
                continue
            with self.subTest(identity=self.rows[index]["id"]):
                self.repo.checkout(self.base)
                rows, migration = self.revised()
                rows[index]["rev"] += 1
                self.refused(self.paired(rows, migration))
        self.repo.checkout(self.base)
        rows, migration = self.revised()
        rows[1]["parent"] = "FixtureSeed02"
        self.refused(self.paired(rows, migration))

    def test_both_closures_have_exact_two_additions_and_no_other_list_drift(self):
        for scope in ("target", "migration"):
            for mutation in ("missing-admission", "missing-capacity", "remove-old", "extra", "duplicate", "unsorted"):
                with self.subTest(scope=scope, mutation=mutation):
                    self.repo.checkout(self.base)
                    rows, migration = self.revised()
                    value = self.target(rows) if scope == "target" else migration
                    paths = value["closure_files"]
                    if mutation.startswith("missing-"):
                        paths.remove(ADDITIONS[0 if mutation.endswith("admission") else 1])
                    elif mutation == "remove-old":
                        paths.remove("cmd/whatomate/main.go")
                    elif mutation == "extra":
                        paths.append("internal/handlers/unrelated.go")
                        paths.sort()
                    elif mutation == "duplicate":
                        paths.append(paths[-1])
                    else:
                        paths.reverse()
                    self.refused(self.paired(rows, migration, {"internal/handlers/unrelated.go": "package handlers\n"}))

    def test_each_closure_hash_must_change_and_be_well_formed(self):
        for scope in ("target", "migration"):
            for digest in (fixture.H, "C" * 64, "c" * 63, "invalid", None):
                with self.subTest(scope=scope, digest=digest):
                    self.repo.checkout(self.base)
                    rows, migration = self.revised()
                    (self.target(rows) if scope == "target" else migration)["closure_sha256"] = digest
                    self.refused(self.paired(rows, migration))

    def test_manifest_roots_version_and_additions_already_in_old_closure_refuse(self):
        for mutation in ("roots", "version", "old-target-addition", "old-migration-addition"):
            with self.subTest(mutation=mutation):
                self.repo.checkout(self.base)
                rows, migration = self.revised()
                base = self.base
                if mutation == "roots":
                    migration["roots"] = migration["roots"][:-1]
                elif mutation == "version":
                    migration["version"] = 2
                else:
                    old_rows, old_migration = copy.deepcopy(self.rows), copy.deepcopy(self.migration)
                    value = self.target(old_rows) if mutation == "old-target-addition" else old_migration
                    value["closure_files"] = sorted(value["closure_files"] + [ADDITIONS[0]])
                    base = self.repo.commit({sc.REGISTRY: wire(old_rows), sc.MIGRATION_PATH: wire(old_migration), ADDITIONS[0]: "package handlers\n"})
                self.refused(self.paired(rows, migration), base)

    def test_registry_and_manifest_canonical_encoding_at_both_endpoints(self):
        for path in (sc.REGISTRY, sc.MIGRATION_PATH):
            for where in ("old", "new"):
                for mutation in ("whitespace", "key-order", "duplicate-key", "empty", "malformed"):
                    with self.subTest(path=path, where=where, mutation=mutation):
                        self.repo.checkout(self.base)
                        rows, migration = self.revised()
                        value = (self.rows if where == "old" else rows) if path == sc.REGISTRY else (self.migration if where == "old" else migration)
                        raw = wire(value)
                        if mutation == "whitespace":
                            raw += "\n"
                        elif mutation == "key-order":
                            raw = fixture.text(value)
                        elif mutation == "duplicate-key":
                            raw = raw.replace('"rev": 1,', '"rev": 1, "rev": 1,', 1) if path == sc.REGISTRY else raw.replace('"version": 1,', '"version": 1, "version": 1,', 1)
                        elif mutation == "empty":
                            raw = ""
                        else:
                            raw = "{not-json}\n"
                        if where == "old":
                            base = self.repo.commit({path: raw})
                            self.refused(self.paired(rows, migration), base)
                        else:
                            self.refused(self.paired(rows, migration, {path: raw}))

    def test_new_source_files_must_both_exist_and_be_nonempty(self):
        for path in ADDITIONS:
            for raw in (None, ""):
                with self.subTest(path=path, raw=raw):
                    self.repo.checkout(self.base)
                    self.refused(self.paired(extra={path: raw}))

    def test_separate_registry_and_manifest_refreshes_are_not_laundered(self):
        rows, migration = self.revised()
        for first in (sc.REGISTRY, sc.MIGRATION_PATH):
            with self.subTest(first=first):
                self.repo.checkout(self.base)
                self.repo.commit({first: wire(rows if first == sc.REGISTRY else migration)})
                self.refused(self.paired())

    def test_third_paired_state_and_update_undo_update_are_refused(self):
        for kind in ("third-target-hash", "third-migration-hash", "third-pair", "other-row-undo", "new-old-new"):
            with self.subTest(kind=kind):
                self.repo.checkout(self.base)
                rows, migration = self.revised()
                if kind == "new-old-new":
                    self.paired()
                    self.repo.commit({sc.REGISTRY: self.files[sc.REGISTRY], sc.MIGRATION_PATH: self.files[sc.MIGRATION_PATH]})
                elif kind == "other-row-undo":
                    changed = copy.deepcopy(self.rows)
                    changed[-1]["requires_gates"] = ["synthetic_gate"]
                    self.repo.commit({sc.REGISTRY: wire(changed)})
                else:
                    if kind in ("third-target-hash", "third-pair"):
                        self.target(rows)["closure_sha256"] = "c" * 64
                    if kind in ("third-migration-hash", "third-pair"):
                        migration["closure_sha256"] = "c" * 64
                    self.paired(rows, migration)
                self.refused(self.paired())

    def test_removal_restore_of_either_contract_is_refused(self):
        for path in (sc.REGISTRY, sc.MIGRATION_PATH):
            with self.subTest(path=path):
                self.repo.checkout(self.base)
                self.repo.commit({path: None})
                self.refused(self.paired())

    def test_all_six_active_artifacts_cannot_change_disappear_or_empty_then_restore(self):
        for path in ACTIVE:
            for mutation in ("changed", "removed", "empty"):
                with self.subTest(path=path, mutation=mutation):
                    self.repo.checkout(self.base)
                    changed = self.active[path] + "\n" if mutation == "changed" else None if mutation == "removed" else ""
                    self.repo.commit({path: changed})
                    self.refused(self.paired(extra={path: self.active[path]}))

    def test_endpoint_active_seed_rows_and_artifact_bytes_remain_fixed(self):
        for path in ACTIVE:
            with self.subTest(path=path):
                self.repo.checkout(self.base)
                self.refused(self.paired(extra={path: self.active[path] + "\n"}))
        self.repo.checkout(self.base)
        seeds = json.loads(self.active[sc.SEEDS])
        seeds["synthetic_rows"][0]["value"] = "changed"
        self.refused(self.paired(extra={sc.SEEDS: wire(seeds)}))

    def test_nonregular_metadata_active_and_added_source_blobs_refuse(self):
        for index, path in enumerate((sc.REGISTRY, sc.MIGRATION_PATH, *ACTIVE, *ADDITIONS)):
            for mode in ("120000", "160000"):
                with self.subTest(path=path, mode=mode):
                    # Each deliberately nonregular index lives in a fresh
                    # disposable repository; no dirty-mode checkout is reused.
                    case = support.TempRepo(Path(self.temp.name) / ("mode-%d-%s" % (index, mode)))
                    base = case.commit(self.files)
                    rows, migration = self.revised()
                    head = case.commit({sc.REGISTRY: wire(rows), sc.MIGRATION_PATH: wire(migration),
                                        **{added: "package handlers\n" for added in ADDITIONS}})
                    oid = case.git("rev-parse", head + ":" + path).strip() if mode == "120000" else base
                    case.git("update-index", "--add", "--cacheinfo", mode + "," + oid + "," + path)
                    case.git("commit", "--quiet", "-m", "nonregular fixture")
                    self.assertTrue(sc.check_ci_source(case.root, base, case.head()))

    def test_new_branch_respects_per_blob_size_and_complete_reads(self):
        head = self.paired()
        real_git = sc._git
        for path in (sc.REGISTRY, sc.MIGRATION_PATH, PROFILE, *ADDITIONS):
            oid = self.repo.git("rev-parse", head + ":" + path).strip()
            for fault in ("size", "partial"):
                with self.subTest(path=path, fault=fault):
                    def capture(root, args):
                        if args == ["cat-file", "-s", oid] and fault == "size":
                            return str(sc.MAX_ARTIFACT_BYTES + 1).encode()
                        raw = real_git(root, args)
                        if args == ["cat-file", "blob", oid] and fault == "partial":
                            return raw[:-1]
                        return raw
                    with mock.patch.object(sc, "_git", side_effect=capture):
                        self.refused(head)

    def test_shared_artifact_budget_cannot_be_reset_by_new_admission(self):
        intermediate = self.repo.commit({"README.md": "old metadata predecessor\n"})
        head = self.paired()
        real_helper = sc._ci_h1_bootstrap_closure
        observed, attempted = [], []
        uncached = (intermediate, sc.CATALOG)
        def wrapper(reader, base, candidate, old, new):
            # This fixture has no pending plan or tenant-profile declaration.
            # Legacy scans cache intermediate registry/manifest documents, but
            # the new helper first reads its intermediate active catalog.
            self.assertNotIn(uncached, reader.cache)
            observed.append(reader)
            reader.total = 64 * 1024 * 1024
            real_blob = reader.blob
            def record_blob(revision, path):
                if (revision, path) == uncached:
                    self.assertNotIn(uncached, reader.cache)
                    attempted.append(uncached)
                return real_blob(revision, path)
            with mock.patch.object(reader, "blob", side_effect=record_blob):
                return real_helper(reader, base, candidate, old, new)
        with mock.patch.object(sc, "_ci_h1_bootstrap_closure", side_effect=wrapper):
            self.refused(head)
        self.assertEqual(len(observed), 1)
        self.assertEqual(attempted, [uncached])
        self.assertEqual(observed[0].total, 64 * 1024 * 1024 + len(self.active[sc.CATALOG].encode()))

    def test_history_commit_cap_and_nonancestor_base_remain_refusals(self):
        self.repo.commit({"README.md": "one\n"})
        self.repo.commit({"README.md": "two\n"})
        head = self.paired()
        with mock.patch.object(sc, "MAX_HISTORY_COMMITS", 2):
            self.refused(head)
        self.repo.checkout(self.base)
        foreign = self.repo.commit({"README.md": "sibling base\n"})
        self.refused(head, foreign)

    def test_full_first_parent_transcript_cannot_omit_or_invent_states(self):
        self.repo.commit({"README.md": "old metadata predecessor\n"})
        self.paired()
        head = self.repo.commit({"README.md": "new metadata successor\n"})
        history = self.repo.git("rev-list", "--first-parent", "--reverse", self.base + ".." + head).encode()
        commits = history.splitlines()
        self.assertEqual(len(commits), 3)
        transcripts = {
            "empty": b"", "missing-newline": history.rstrip(b"\n"),
            "omit-prefix": b"\n".join(commits[1:]) + b"\n",
            "omit-middle": commits[0] + b"\n" + commits[2] + b"\n",
            "omit-suffix": b"\n".join(commits[:-1]) + b"\n",
            "duplicate": history + commits[-1] + b"\n",
            "reverse": b"\n".join(reversed(commits)) + b"\n",
            "include-base": self.base.encode() + b"\n" + history,
            "malformed": b"not-a-commit\n", "non-ascii": b"\xff\n",
        }
        real_helper, real_git = sc._ci_h1_bootstrap_closure, sc._git
        for name, raw in transcripts.items():
            with self.subTest(transcript=name):
                intercepted = []
                def capture(root, args):
                    if args[:3] == ["rev-list", "--first-parent", "--reverse"]:
                        intercepted.append(tuple(args))
                        return raw
                    return real_git(root, args)
                def wrapper(*args):
                    with mock.patch.object(sc, "_git", side_effect=capture):
                        return real_helper(*args)
                with mock.patch.object(sc, "_ci_h1_bootstrap_closure", side_effect=wrapper):
                    self.refused(head)
                self.assertEqual(len(intercepted), 1)
        for invalid_parent in (b"", b"f" * 40 + b"\n", self.base.encode() + b"\nextra\n"):
            with self.subTest(parent=invalid_parent):
                intercepted = []
                def capture(root, args):
                    if args[:2] == ["rev-parse", "--verify"] and args[-1].endswith("^1"):
                        intercepted.append(tuple(args))
                        return invalid_parent
                    return real_git(root, args)
                def wrapper(*args):
                    with mock.patch.object(sc, "_git", side_effect=capture):
                        return real_helper(*args)
                with mock.patch.object(sc, "_ci_h1_bootstrap_closure", side_effect=wrapper):
                    self.refused(head)
                self.assertTrue(intercepted)

    def test_transport_refusal_at_added_source_is_not_swallowed(self):
        head = self.paired()
        real_git = sc._git
        called = []
        def capture(root, args):
            if args[:2] == ["ls-tree", "-z"] and args[-1] == ADDITIONS[0]:
                called.append(tuple(args))
                raise source_git.SourceGitRefusal("git-failed:source-budget")
            return real_git(root, args)
        with mock.patch.object(sc, "_git", side_effect=capture):
            with self.assertRaises(source_git.SourceGitRefusal):
                self.check(head)
        self.assertTrue(called)

    def test_named_migration_and_additional_policy_obligations_are_not_waived(self):
        head = self.paired(extra={"cmd/whatomate/main.go": fixture.MAIN.replace("println(1)", "println(7)")})
        self.refused(head)
        self.repo.checkout(self.base)
        head = self.paired()
        policy = sc.classify_v2(self.repo.root, self.base, head)
        variants = [policy._replace(kind=kind) for kind in ("data", "expand", "contract")]
        variants += [policy._replace(dormant=True), policy._replace(reasons=("synthetic-refusal",)),
                     policy._replace(requirements=(*policy.requirements, "future-authority")),
                     policy._replace(migration_changes=(*policy.migration_changes, "other migration"))]
        for variant in variants:
            with self.subTest(policy=variant), mock.patch.object(sc, "_classify_v2", return_value=variant):
                self.refused(head)
        legacy = sc.classify(self.repo.root, self.base, head)
        with mock.patch.object(sc, "_classify", return_value=legacy._replace(reasons=(*legacy.reasons, "synthetic-refusal"))):
            self.refused(head)

    def test_pending_introduction_and_remove_restore_remain_refused(self):
        args = pending_fixture.fixture()
        pending = {path: raw.decode() for path, raw in args["files"].items()}
        active = {sc.CATALOG: args["active_catalog"].decode(), sc.GLOBALS: args["active_globals"].decode(), sc.HISTORY: args["active_history"].decode()}
        base = self.repo.commit({**self.files, **active})
        self.refused(self.paired(extra=pending), base)
        self.repo.checkout(self.base)
        retained = self.repo.commit({**self.files, **active, **pending})
        self.assertEqual(self.check(self.paired(), retained), ())
        self.repo.commit({path: None for path in pending})
        self.refused(self.paired(extra=pending), retained)


if __name__ == "__main__":
    unittest.main()
