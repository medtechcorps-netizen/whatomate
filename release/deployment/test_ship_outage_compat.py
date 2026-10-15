"""Real composed outage source/transport boundaries with synthetic commands."""
import ast
import hashlib
from contextlib import contextmanager
from dataclasses import FrozenInstanceError, replace
from datetime import timedelta
from pathlib import Path
import unittest
from unittest import mock

import ship_outage_compat as ship
import test_compat_collect as actions
import test_compat_evidence as evidence_fixture
import test_compat_request as bridge_fixture
import test_compat_negative_index as index_fixture
import test_compat_negative_transport as index_world
import test_record_resolver as source


def namespace_bytes(files, sha):
    """Complete synthetic Git tree projection, including real blob/tree OIDs.

    This models only the fixed read command; it never imports these sources.
    Actual Git traversal has separate disposable-repository coverage.
    """
    paths = ship.resolver.source_git._PUBLISHER_PATHS
    entries = {}
    for (revision, path), raw in files.items():
        if revision == sha and any(path == root or path.startswith(root + "/") for root in paths):
            oid = hashlib.sha1(b"blob " + str(len(raw)).encode("ascii") + b"\0" + raw).hexdigest()
            entries[path] = ("100644", "blob", oid)
    for path in tuple(entries):
        parent = path.rpartition("/")[0]
        while parent:
            if parent not in entries: entries[parent] = ("040000", "tree", None)
            parent = parent.rpartition("/")[0]
    for path in sorted(entries, key=lambda item: item.count("/"), reverse=True):
        if entries[path][1] != "tree": continue
        children = [(name.rpartition("/")[2], value) for name, value in entries.items()
                    if name.rpartition("/")[0] == path]
        children.sort(key=lambda row: row[0] + ("/" if row[1][1] == "tree" else ""))
        raw = b"".join((mode.lstrip("0") + " " + name).encode("ascii") + b"\0" + bytes.fromhex(oid)
                       for name, (mode, kind, oid) in children)
        entries[path] = ("040000", "tree", hashlib.sha1(b"tree " + str(len(raw)).encode() + b"\0" + raw).hexdigest())
    ordered = sorted(entries, key=lambda path: path + ("/" if entries[path][1] == "tree" else ""))
    return b"".join((" ".join(entries[path]) + "\t" + path + "\0").encode("ascii") for path in ordered)


class OutageCase:
    """Fixture methods execute only when root starts the frozen test cohort."""
    def __init__(self, testcase, *, unavailable=True, full_closure=False):
        source.ForwardResolverTests.setUpClass()
        self.source = source.ForwardResolverTests()
        self.source.setUp()
        testcase.addCleanup(self.source.doCleanups)
        self.root, self.repo, self.gh, self.git = (self.source.root, self.source.repo,
                                                self.source.gh, self.source.git)
        self.clock, self.world = self.source.clock, self.source.world
        self.sha, self.now, self.producer = evidence_fixture.SHA, actions.NOW, "d" * 40
        self.source.add(version=1, images=self.source.boot["images"])
        self.origin = self.source.add()
        self.source.profile(self.origin["sha"])
        self.source.candidate(self.sha)
        self.world.commits.add(self.producer)
        self.policy_path = self.root / "consumer.json"
        self.inventory = {}
        files = self._closure() if full_closure else {
            ".github/workflows/compat.yml": (source.ROOT / ".github/workflows/compat.yml").read_bytes(),
            "release/compat/negative_index.py": b"# complete synthetic publisher module\n",
            "release/compat/attest_evidence.py": b"# complete synthetic preparer module\n"}
        # The fixture's complete source namespace is distinct from its executed
        # inventory. Ordinary source facts already include drills.json at H.
        for (revision, path), raw in tuple(self.world.files.items()):
            if revision == self.sha and path.startswith(("release/compat/", "release/deployment/")):
                self.world.files[self.producer, path] = raw
        if full_closure:
            for directory in (source.ROOT / "release/compat", source.ROOT / "release/deployment"):
                for path in directory.rglob("*"):
                    # Interpreter-generated checkout caches are not part of
                    # this controlled committed-source fixture. Adversarial
                    # tracked caches are installed explicitly by tests below.
                    if path.is_file() and "__pycache__" not in path.parts and path.suffix not in (".pyc", ".pyo"):
                        relative = path.relative_to(source.ROOT).as_posix()
                        # Model tooling already present at the synthetic database
                        # origin; full closure is not a new finite repair at H.
                        for sha in (self.origin["sha"], self.producer, self.sha):
                            self.world.files[sha, relative] = path.read_bytes()
        for path, raw in files.items(): self._install_blob(path, raw)
        self.namespace_response = lambda sha, raw: raw
        # One shared Git seam serves both direct resolver captures and the
        # composed preflight. ControlledWorld.__call__ still owns its trace and
        # before/after hooks; later test wrappers can delegate to this function.
        original_git = self.world.git
        def git(args):
            sg = ship.resolver.source_git
            if (len(args) == 11 and tuple(args[:5]) == sg._PUBLISHER_TREE_ARGS
                    and args[6] == "--" and tuple(args[7:]) == sg._PUBLISHER_PATHS):
                raw = namespace_bytes(self.world.files, args[5])
                return ship.runtime.Capture(0, self.namespace_response(args[5], raw), b"")
            return original_git(args)
        self.world.git = git
        self._policy()
        self.serial = 0
        self.expected = self._resolve_expected()
        self.actions = actions.World(); self.actions.add()
        bridge_fixture.bind_pairs(self.actions, self.expected)
        if not unavailable:
            self.actions.artifacts[100] = []
        self.negative_world = index_world.World()
        if unavailable:
            self.negative_world.service = ship.runtime.Capture(1, index_fixture.http(503), b"synthetic503")
        self.calls, self.phase = [], None
        self.after = lambda phase, argv: None
        self.clock.now = 100.0
        self.deadline = 1000.0
        self.scratch = self.root / "outage-preflight"

    def _closure(self):
        # Conservative complete local Python import graph; no project import.
        # Fixed source inputs read by validators are also retained independently.
        modules = {p.stem: p for directory in (source.ROOT / "release/deployment", source.ROOT / "release/compat")
                   for p in directory.glob("*.py") if not p.name.startswith("test_")}
        todo, seen = [source.ROOT / "release/compat/negative_index.py",
                      source.ROOT / "release/compat/attest_evidence.py"], {}
        while todo:
            path = todo.pop()
            relative = path.relative_to(source.ROOT).as_posix()
            if relative in seen: continue
            raw = path.read_bytes(); seen[relative] = raw
            for node in ast.walk(ast.parse(raw)):
                names = ([row.name.split(".")[0] for row in node.names] if isinstance(node, ast.Import)
                         else [node.module.split(".")[0]] if isinstance(node, ast.ImportFrom) and node.module else [])
                for name in names:
                    if name in modules: todo.append(modules[name])
        for path in (*source.policy.SOURCE_PATHS, ".github/workflows/compat.yml"):
            seen[path] = (source.ROOT / path).read_bytes()
        return seen

    def _install_blob(self, path, raw):
        self.inventory[path] = raw
        for sha in (self.producer, self.sha): self.world.files[sha, path] = raw

    def _policy(self):
        value = index_fixture.consumer_dict()
        value["lineage"] = [{"producer_sha": self.producer, "inventory": [
            {"path": path, "mode": "100644", "bytes": len(raw), "sha256": index_fixture.digest(raw)}
            for path, raw in sorted(self.inventory.items())]}]
        self.policy_raw = index_fixture.wire(value)
        self.policy_path.write_bytes(self.policy_raw)
        self.world.files[self.sha, ship.loader.DATA_PATH] = self.policy_raw

    def _resolve_expected(self):
        self.serial += 1
        phase = self.root / ("expected-" + str(self.serial)); phase.mkdir()
        config, work = phase / "config", phase / "work"; config.mkdir(); work.mkdir()
        self.clock.now = 100.0
        budget = ship.transport.OperationBudget(1000, capture=self.world, clock=self.clock)
        client = ship.transport.ReleaseGh(self.gh, source.support.GH_TOKEN, config, work,
                                         budget=budget, base_env={})
        result = ship.resolver.resolve_forward(client, self.repo, self.git, self.sha)
        self.clock.now = 100.0
        return result

    def add_source(self, path, raw):
        self._install_blob(path, raw); self._policy()
        self.expected = self._resolve_expected()
        bridge_fixture.bind_pairs(self.actions, self.expected)

    @contextmanager
    def installed(self):
        with mock.patch.object(ship.loader, "_POLICY_PATH", self.policy_path):
            yield self

    def capture(self, argv, **options):
        parts = Path(options["env"].get("GH_CONFIG_DIR", "")).parts
        for phase in ("source-before", "actions", "source-after", "negative-index"):
            if phase in parts: self.phase = phase
        self.calls.append((self.phase, list(argv), dict(options)))
        if self.phase == "negative-index":
            result = self.negative_world(argv, **options)
        elif self.phase == "actions":
            raw = self.actions.capture(argv, **options)
            result = ship.runtime.Capture(raw.returncode, raw.stdout, raw.stderr)
        else:
            result = self.world(argv, **options)
        self.clock.now += 0.001
        self.after(self.phase, argv)
        return result

    def run(self, **changes):
        options = dict(gh_binary=self.gh, git_binary=self.git, token=source.support.GH_TOKEN,
            scratch_dir=self.scratch, deadline=self.deadline, capture=self.capture, clock=self.clock,
            wall_clock=lambda: self.now, base_env={"PATH": "synthetic-only", "SHIP_DO_TOKEN": "PRIVATE"})
        options.update(changes)
        return ship._preflight(self.repo, self.sha, **options)


def outage_case(testcase, *, unavailable=True, full_closure=False):
    return OutageCase(testcase, unavailable=unavailable, full_closure=full_closure)


class OutagePreflightTests(unittest.TestCase):
    def make(self, **options):
        return outage_case(self, **options)

    def refusal(self, case, **changes):
        with case.installed(), self.assertRaises(Exception) as caught:
            case.run(**changes)
        self.assertNotIn("PRIVATE", str(caught.exception))
        self.assertIsNone(source.records._GIT_OUTPUT_TRANSPORT.get())
        return caught.exception

    def test_real_503_has_no_actions_and_retains_complete_source_index_and_budget(self):
        case = self.make()
        with case.installed(), mock.patch.object(ship.collect, "ActionsGh", side_effect=AssertionError("Actions reached")):
            value = case.run()
        self.assertIs(type(value), ship.OutagePreflight)
        self.assertIs(type(value.availability), ship.ServiceUnavailable)
        self.assertEqual(value.source.admission.resolution, case.expected)
        self.assertEqual(value.command_count, len(case.calls))
        self.assertEqual(len(case.negative_world.calls), 7)
        self.assertEqual(ship.validate_preserved(value), value)
        self.assertGreater(value.valid_until, value.checked_at)
        self.assertLessEqual(value.valid_until, value.entry_at + timedelta(seconds=900))
        self.assertFalse(any(phase == "actions" for phase, _, _ in case.calls))
        self.assertTrue(all("SHIP_DO_TOKEN" not in opts["env"] for _, _, opts in case.calls))
        with self.assertRaises(FrozenInstanceError): value.remaining_us = 0

    def test_real_missing_retains_both_snapshots_and_exact_terminal_windows(self):
        case = self.make(unavailable=False)
        with case.installed(): value = case.run()
        self.assertIs(type(value.availability), ship.MissingExpiredEvidence)
        history = value.availability.collected
        self.assertEqual(len(history.before.attempts), 1)
        self.assertEqual(history.before, history.after)
        self.assertEqual(value, ship.validate_preserved(value))
        self.assertTrue(any(phase == "actions" for phase, _, _ in case.calls))

    def test_empty_complete_inventory_is_missing_only_with_index_coverage(self):
        case = self.make(unavailable=False)
        case.actions.runs.clear(); case.actions.attempts.clear()
        with case.installed(): value = case.run()
        self.assertEqual(value.availability.collected.normal.history.attempts, ())

    def test_expired_artifact_is_replayed_from_complete_raw_metadata(self):
        case = self.make(unavailable=False)
        case.actions.add(run=101, number=11, minute=20)
        bridge_fixture.bind_pairs(case.actions, case.expected)
        case.actions.artifacts[101][0]["expired"] = True
        case.actions.artifacts[101][0]["expires_at"] = actions.stamp(case.now - timedelta(seconds=1))
        with case.installed(): value = case.run()
        self.assertIs(type(value), ship.OutagePreflight)
        self.assertEqual(value, ship.validate_preserved(value))

    def test_absent_policy_delegates_normal_without_any_negative_calls(self):
        case = self.make()
        case.policy_path.unlink()
        with case.installed(): value = case.run()
        self.assertIs(type(value), ship.normal.NormalPreflight)
        self.assertEqual(case.negative_world.calls, [])

    def test_qualifying_normal_proof_survives_changed_missing_or_nonancestor_closure(self):
        for change in ("changed", "missing", "ancestry"):
            with self.subTest(change=change):
                case = self.make()
                case.negative_world.service = ship.runtime.Capture(0, index_fixture.http(200), b"")
                if change == "changed": case.world.files[case.sha, "release/compat/negative_index.py"] += b"# change\n"
                elif change == "missing": del case.world.files[case.sha, "release/compat/negative_index.py"]
                else: case.world.bad_pairs.add((case.producer, case.sha))
                with case.installed(): value = case.run()
                self.assertIs(type(value), ship.normal.NormalPreflight)
                self.assertEqual(len(case.negative_world.calls), 3)

    def test_same_uncovered_cases_refuse_service_unavailability(self):
        for change in ("changed", "missing", "ancestry"):
            with self.subTest(change=change):
                case = self.make()
                if change == "changed": case.world.files[case.sha, "release/compat/negative_index.py"] += b"# change\n"
                elif change == "missing": del case.world.files[case.sha, "release/compat/negative_index.py"]
                else: case.world.bad_pairs.add((case.producer, case.sha))
                self.assertIn("uncovered", str(self.refusal(case)))

    def test_real_authenticated_false_is_never_outage_even_with_later_missing(self):
        case = self.make(unavailable=False)
        case.actions.add(run=99, number=9, minute=1, negative=True)
        bridge_fixture.bind_pairs(case.actions, case.expected)
        self.refusal(case)
        self.assertEqual(len(case.negative_world.calls), 3)

    def test_failed_positive_or_active_no_work_does_not_become_missing(self):
        for change in ("failed", "active", "no-work"):
            with self.subTest(change=change):
                case = self.make(unavailable=False)
                if change == "failed":
                    case.actions.add(run=101, number=11, minute=20)
                    case.actions.runs[101]["conclusion"] = "failure"
                    case.actions.set_job(101, 1, "compat", conclusion="failure")
                    job = next(row for row in case.actions.jobs[101, 1] if row["name"] == "compat")
                    job["steps"][-3]["conclusion"] = "failure"
                    bridge_fixture.bind_pairs(case.actions, case.expected)
                elif change == "active":
                    case.actions.runs[100]["status"] = "in_progress"
                    case.actions.runs[100]["conclusion"] = None
                else:
                    for row in case.actions.jobs[100, 1]: row["steps"] = []
                self.refusal(case)

    def test_nonempty_or_changing_marker_inventory_refuses(self):
        for changed in (False, True):
            with self.subTest(changed=changed):
                case = self.make()
                endpoint = ship.negative.API + "/git/matching-refs/tags/" + ship.negative.PREFIX + case.sha + "-"
                seen = []
                def response(result):
                    seen.append(1)
                    return ship.runtime.Capture(0, b"[]\n" if changed and len(seen) == 1 else b"[{}]\n", b"")
                case.negative_world.overrides["GET", endpoint] = response
                self.refusal(case)

    def test_transport_timeout_and_auth_service_never_construct_actions(self):
        for status in (401, 403, 429, 301):
            with self.subTest(status=status):
                case = self.make()
                case.negative_world.service = ship.runtime.Capture(1, index_fixture.http(status), b"private")
                with mock.patch.object(ship.collect, "ActionsGh", side_effect=AssertionError("Actions reached")):
                    self.refusal(case)
                self.assertFalse(any(phase == "actions" for phase, _, _ in case.calls))

    def test_source_policy_changes_after_service_refuse_before_result(self):
        case = self.make()
        def after(phase, argv):
            if phase == "negative-index" and ship.negative.ACTIONS_ENDPOINT in argv:
                case.world.files[case.sha, ship.loader.DATA_PATH] += b" "
        case.after = after
        self.refusal(case)

    def test_original_deadline_exhaustion_has_no_fallback(self):
        case = self.make()
        def after(phase, argv):
            if phase == "negative-index": case.clock.now = case.deadline
        case.after = after
        self.refusal(case)
        self.assertFalse(any(phase == "actions" for phase, _, _ in case.calls))

    def test_fractional_clock_and_finite_original_validity_replay(self):
        case = self.make(); case.now += timedelta(microseconds=900000)
        with case.installed(): value = case.run()
        self.assertEqual(value.checked_at.microsecond, 900000)
        for changed in (replace(value, remaining_us=True), replace(value, remaining_us=0),
                        replace(value, remaining_us=900000001), replace(value, command_count=True),
                        replace(value, command_count=6001), replace(value, captured_bytes=129 << 20),
                        replace(value, valid_until=value.valid_until + timedelta(microseconds=1)),
                        replace(value, entry_at=value.checked_at + timedelta(seconds=1))):
            with self.subTest(value=changed), self.assertRaises(ship.evidence.CompatRefusal):
                ship.validate_preserved(changed)

    def test_retained_replay_never_loads_policy_or_samples_new_clock(self):
        case = self.make()
        with case.installed(): value = case.run()
        with mock.patch.object(ship.loader, "load_consumer_policy", side_effect=AssertionError("loader")):
            self.assertEqual(ship._validate_preserved_custody(value), value)

    def test_source_raw_or_role_tamper_refuses(self):
        case = self.make()
        with case.installed(): value = case.run()
        coverage = value.source.coverage
        for row in (replace(coverage.blobs[0], source_sha="f"*40),
                    replace(coverage.blobs[0], raw=b"foreign"),
                    replace(coverage.blobs[0], mode="120000")):
            altered = replace(coverage, blobs=(row, *coverage.blobs[1:]))
            with self.assertRaises(ship.evidence.CompatRefusal):
                ship.validate_preserved(replace(value, source=replace(value.source, coverage=altered)))

    def test_preserved_index_observation_and_service_types_are_rederived(self):
        case = self.make()
        with case.installed(): value = case.run()
        variants = (replace(value, availability=replace(value.availability,
                        observation=replace(value.availability.observation, status=200))),
                    replace(value, index=replace(value.index, observed_at=value.checked_at + timedelta(seconds=1))),
                    replace(value, index=replace(value.index, setup_sha256="f"*64)))
        for changed in variants:
            with self.assertRaises(ship.evidence.CompatRefusal): ship.validate_preserved(changed)


class PublisherNamespacePreflightTests(unittest.TestCase):
    make = OutagePreflightTests.make
    refusal = OutagePreflightTests.refusal

    def test_exact_logical_roles_and_commands_survive_raw_custody(self):
        case = self.make()
        with case.installed(): value = case.run()
        rows = value.source.coverage.namespaces
        self.assertEqual(tuple(row.source_sha for row in rows), (case.producer, case.sha))
        self.assertEqual(rows[0].raw, namespace_bytes(case.world.files, case.producer))
        self.assertEqual(rows[1].raw, namespace_bytes(case.world.files, case.sha))
        calls = [(phase, args, opts) for phase, args, opts in case.calls if "--full-tree" in args]
        self.assertEqual([phase for phase, _, _ in calls], ["source-before"] * 2 + ["source-after"] * 2)
        self.assertTrue(all(opts["stdout_limit"] == 65536 for _, _, opts in calls))
        self.assertEqual(value, ship.validate_preserved(value))

    def test_added_packages_stdlib_shadows_and_candidate_binaries_are_uncovered(self):
        for path in ("release/compat/public_evidence/__init__.py", "release/deployment/json.py",
                     "release/compat/__pycache__/public_evidence.cpython-312.pyc",
                     "release/compat/public_evidence.pyo", "release/compat/public_evidence.abi3.so",
                     "release/compat/public_evidence.PYD"):
            with self.subTest(path=path):
                case = self.make(); original = case.policy_raw
                case.world.files[case.sha, path] = b"synthetic shadow"
                self.assertNotIn(path, case.inventory)
                self.assertEqual(case.policy_raw, original)
                self.assertIn("uncovered", str(self.refusal(case)))

    def test_preexisting_dormant_cache_or_native_content_never_grants_coverage(self):
        for path in ("release/compat/__pycache__/public_evidence.cpython-312.pyc",
                     "release/compat/unused.PYC", "release/compat/unused.so", "release/compat/unused.pyd"):
            with self.subTest(path=path):
                case = self.make()
                case.world.files[case.producer, path] = b"ignored old cache"
                case.world.files[case.sha, path] = b"different candidate executable"
                self.assertIn("uncovered", str(self.refusal(case)))

    def test_ancestor_and_nested_attributes_are_uncovered_even_if_unchanged(self):
        for path in (".gitattributes", "release/.gitattributes", "release/compat/.gitattributes",
                     "release/deployment/fixtures/.gitattributes"):
            with self.subTest(path=path):
                case = self.make()
                for sha in (case.producer, case.sha): case.world.files[sha, path] = b"*.py filter=unexpected\n"
                self.assertIn("uncovered", str(self.refusal(case)))

    def test_topology_and_native_changes_do_not_poison_a_qualifying_normal_proof(self):
        for path in ("release/compat/public_evidence/__init__.py", "release/compat/unused.so", ".gitattributes"):
            with self.subTest(path=path):
                case = self.make()
                case.world.files[case.sha, path] = b"foreign"
                case.negative_world.service = ship.runtime.Capture(0, index_fixture.http(200), b"")
                with case.installed(): value = case.run()
                self.assertIs(type(value), ship.normal.NormalPreflight)
                self.assertEqual(len(case.negative_world.calls), 3)

    def test_existing_unimported_content_can_change_without_freezing_tree_oids(self):
        case = self.make()
        path = "release/compat/qualify_negative_index.py"
        case.world.files[case.producer, path] = b"old nonimported helper"
        case.world.files[case.sha, path] = b"changed nonimported helper"
        with case.installed(): value = case.run()
        rows = value.source.coverage.namespaces
        self.assertNotEqual(rows[0].raw, rows[1].raw)
        self.assertEqual(value, ship.validate_preserved(value))

    def test_preparer_bytes_are_pinned_separately_from_topology(self):
        case = self.make(); path = "release/compat/attest_evidence.py"
        self.assertIn(path, case.inventory)
        case.world.files[case.sha, path] += b"# changed behavior\n"
        self.assertIn("uncovered", str(self.refusal(case)))

    def test_before_after_namespace_change_is_not_hidden_by_equal_listed_blobs(self):
        case = self.make()
        def after(phase, argv):
            if phase == "negative-index" and ship.negative.ACTIONS_ENDPOINT in argv:
                case.world.files[case.sha, "release/compat/new_package/__init__.py"] = b"new"
        case.after = after
        self.assertIn("source-changed", str(self.refusal(case)))

    def test_raw_capture_truncation_bound_and_deadline_refuse_without_actions(self):
        for change in ("truncated", "overbound", "deadline"):
            with self.subTest(change=change):
                case = self.make()
                def response(sha, raw):
                    if change == "deadline": case.clock.now = case.deadline
                    return raw[:-1] if change == "truncated" else b"x" * 65537 if change == "overbound" else raw
                case.namespace_response = response
                self.refusal(case)
                self.assertFalse(any(phase == "actions" for phase, _, _ in case.calls))

    def test_raw_holder_role_inventory_and_leaf_oid_are_replayed(self):
        case = self.make()
        with case.installed(): value = case.run()
        coverage = value.source.coverage; rows = coverage.namespaces
        variants = (rows[:-1], rows + (rows[0],), rows[::-1],
                    (replace(rows[0], source_sha='f' * 40), rows[1]),
                    (replace(rows[0], raw=True), rows[1]))
        # Same topology, wrong metadata for a separately pinned executable.
        raw = rows[0].raw
        for line in raw.split(b"\0"):
            if line.endswith(b"\trelease/compat/negative_index.py"):
                wrong = line.split(b"\t")[0].rsplit(b" ", 1)[0] + b" " + b"f" * 40 + b"\trelease/compat/negative_index.py"
                variants += ((replace(rows[0], raw=raw.replace(line, wrong)), rows[1]),)
        for changed in variants:
            with self.subTest(changed=changed), self.assertRaises(ship.evidence.CompatRefusal):
                ship.validate_preserved(replace(value, source=replace(value.source,
                    coverage=replace(coverage, namespaces=changed))))

    def test_namespace_bytes_share_original_source_cap_before_parsing(self):
        case = self.make()
        with case.installed(): value = case.run()
        coverage = value.source.coverage
        payload = b'x' * ship.negative.MAX_SOURCE
        blobs = tuple(replace(row, raw=payload) for row in coverage.blobs)
        with mock.patch.object(ship.resolver.source_git, 'publisher_namespace', side_effect=AssertionError('parsed early')):
            with self.assertRaisesRegex(ship.evidence.CompatRefusal, 'source-bound'):
                ship.validate_preserved(replace(value, source=replace(value.source,
                    coverage=replace(coverage, blobs=blobs))))


class OutageWindowPreservationTests(unittest.TestCase):
    make = OutagePreflightTests.make

    def track_budget(self):
        original = ship.normal._Budget
        budgets = []
        def create(*args, **kwargs):
            value = original(*args, **kwargs)
            budgets.append(value)
            return value
        return budgets, mock.patch.object(ship.normal, "_Budget", side_effect=create)

    def test_installed_policy_keeps_original_900_for_unavailable_and_normal_results(self):
        for unavailable in (True, False):
            with self.subTest(unavailable=unavailable):
                case = self.make()
                case.deadline = 2000.0
                if not unavailable:
                    case.negative_world.service = ship.runtime.Capture(0, index_fixture.http(200), b"")
                budgets, tracking = self.track_budget()
                with case.installed(), tracking:
                    value = case.run()
                self.assertEqual(len(budgets), 1)
                self.assertEqual(budgets[0].end, 1000.0)
                self.assertTrue(budgets[0].closed)
                self.assertFalse(budgets[0].failed)
                self.assertEqual(value.command_count, len(case.calls))
                if unavailable:
                    self.assertIs(type(value), ship.OutagePreflight)
                    self.assertLessEqual(value.remaining_us, 900000000)
                    self.assertLessEqual(value.valid_until, value.entry_at + timedelta(seconds=900))
                    self.assertEqual(ship.validate_preserved(value), value)
                else:
                    self.assertIs(type(value), ship.normal.NormalPreflight)

    def test_installed_policy_expiry_and_shorter_caller_deadline_do_not_reset(self):
        for deadline, expected_end in ((2000.0, 1000.0), (250.0, 250.0)):
            with self.subTest(deadline=deadline):
                case = self.make()
                case.deadline = deadline
                case.after = lambda phase, argv: setattr(case.clock, "now", expected_end)
                budgets, tracking = self.track_budget()
                with case.installed(), tracking, self.assertRaises((ship.evidence.CompatRefusal, ship.common.ReleaseError)):
                    case.run()
                self.assertEqual(len(budgets), 1)
                self.assertEqual(budgets[0].end, expected_end)
                self.assertTrue(budgets[0].closed)
                self.assertTrue(budgets[0].failed)
                self.assertEqual(len(case.calls), 1)
                self.assertFalse(any(phase in ("actions", "negative-index", "source-after")
                                     for phase, _, _ in case.calls))
                self.assertIsNone(source.records._GIT_OUTPUT_TRANSPORT.get())

    def test_absent_policy_uses_normal_2400_without_creating_outage_allowance(self):
        case = self.make()
        case.policy_path.unlink()
        case.deadline = 3000.0
        budgets, tracking = self.track_budget()
        with case.installed(), tracking:
            value = case.run()
        self.assertEqual(len(budgets), 1)
        self.assertEqual(budgets[0].end, 2500.0)
        self.assertTrue(budgets[0].closed)
        self.assertFalse(budgets[0].failed)
        self.assertIs(type(value), ship.normal.NormalPreflight)
        self.assertEqual(value.command_count, len(case.calls))
        self.assertEqual(case.negative_world.calls, [])


if __name__ == "__main__":
    unittest.main()
