"""Actual ReleaseGh capture seam; synthetic GraphQL data, no live queries."""
import copy
from dataclasses import FrozenInstanceError
from datetime import datetime, timedelta, timezone
import hashlib
from pathlib import Path
import tempfile
import unittest

import contract_authorization as policy
import contract_collect as collect
import record_transport as rt
import ship_common as common
import test_record_transport as transport_tests
import test_ship_support as support

NOW = datetime(2026, 10, 11, 12, 0, tzinfo=timezone.utc)
SHA = "a" * 40


def stamp(value):
    return value.isoformat().replace("+00:00", "Z")


def identity():
    return {"databaseId": rt.REPOSITORY_ID, "nameWithOwner": common.REPOSITORY}


def label():
    return {"id": "LABEL_1", "name": "db:contract", "createdAt": stamp(NOW - timedelta(days=10)),
            "updatedAt": stamp(NOW - timedelta(days=3)), "repository": identity()}


def pull():
    # Exact saved GraphQL enum: merged PR268 has state MERGED, not REST CLOSED.
    return {"id": "PULL_1", "number": 268, "merged": True, "isDraft": False,
            "state": "MERGED", "baseRefName": "main", "mergeCommit": {"oid": SHA},
            "headRefOid": "b" * 40, "mergedAt": stamp(NOW - timedelta(days=2)),
            "repository": identity()}


def event(index=0):
    return {"__typename": "LabeledEvent" if index % 2 == 0 else "UnlabeledEvent",
            "id": f"EVENT_{index:04}", "createdAt": stamp(NOW - timedelta(days=1) + timedelta(seconds=index)),
            "actor": {"__typename": "User", "id": "OWNER_1", "login": common.APPROVER_LOGIN},
            "label": label(), "labelable": {"__typename": "PullRequest", "id": "PULL_1", "repository": identity()}}


def connection(rows, prefix, *, offset=0, more=False):
    edges = [{"cursor": f"{prefix}-{index + offset + 1}", "node": row}
             for index, row in enumerate(rows)]
    return {"edges": edges, "pageInfo": {"hasNextPage": more,
            "endCursor": edges[-1]["cursor"] if edges else None}}


class World(transport_tests.World):
    def __init__(self):
        super().__init__()
        self.pulls = [pull()]
        self.labels = [label()]
        self.events = [event()]
        self.graph_calls = []
        self.hook = None
        self.raw = None
        self.pad = False
        self.page_size = 100

    def __call__(self, argv, **options):
        if argv[1:5] != ["api", "--method", "POST", "graphql"]:
            return super().__call__(argv, **options)
        self.calls.append((list(argv), dict(options)))
        args = argv[1:]
        assert tuple(args[4:8]) == rt.HEADERS
        variables = {}
        for index in range(8, len(args), 2):
            flag, pair = args[index:index+2]
            key, value = pair.split("=", 1)
            assert flag == ("-F" if key == "number" else "-f")
            assert key not in variables
            variables[key] = int(value) if flag == "-F" else value
        query = variables.pop("query")
        stages = [key for key, value in rt._CONTRACT_QUERIES.items() if value == query]
        assert len(stages) == 1
        stage = stages[0]
        expected = {"owner", "name", "candidate"} | ({"number"} if stage != "associations" else set())
        assert set(variables) in (expected, expected | {"after"})
        assert variables["owner"] == "medtechcorps-netizen" and variables["name"] == "whatomate"
        assert variables["candidate"] == SHA
        if stage != "associations": assert variables["number"] == 268
        self.graph_calls.append((stage, dict(variables), dict(options)))
        root = {**identity(), "owner": {"__typename": "User", "id": "OWNER_1", "login": common.APPROVER_LOGIN},
                "defaultBranchRef": {"name": "main", "target": {"oid": SHA}},
                "object": {"__typename": "Commit", "oid": SHA}}
        if stage == "associations":
            root["object"]["associatedPullRequests"] = connection(copy.deepcopy(self.pulls), "pull")
        else:
            root["pullRequest"] = copy.deepcopy(self.pulls[0])
            if stage == "labels":
                root["pullRequest"]["labels"] = connection(copy.deepcopy(self.labels), "label")
            else:
                after = variables.get("after")
                start = int(after.removeprefix("event-")) if after is not None else 0
                rows = copy.deepcopy(self.events[start:start+self.page_size])
                root["pullRequest"]["timelineItems"] = connection(rows, "event", offset=start,
                    more=start + len(rows) < len(self.events))
        payload = {"data": {"repository": root}}
        if self.hook:
            self.hook(stage, sum(row[0] == stage for row in self.graph_calls), variables, payload)
        raw = self.raw if self.raw is not None else common.canonical_file_bytes(payload)
        if self.pad: raw += b" " * (options["stdout_limit"] - len(raw))
        return rt.runtime.Capture(0, raw, b"")


class ContractCollectorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binary = self.root / "gh.exe"; self.binary.write_bytes(b"not executed")
        self.serial = 0
        self.clock = transport_tests.Clock()

    def client(self, world=None, *, snapshot=True):
        self.serial += 1
        base = self.root / str(self.serial); base.mkdir()
        config, work = base / "config", base / "work"; config.mkdir(); work.mkdir()
        world = world or World()
        budget = rt.OperationBudget(700, capture=world, clock=self.clock)
        client = rt.ReleaseGh(self.binary, support.GH_TOKEN, config, work, budget=budget,
                             base_env={"PATH": "/usr/bin", "SHIP_DO_TOKEN": "PRIVATE"})
        if snapshot: client.capture_snapshot()
        return client, world

    def observed(self, client):
        return collect.collect_contract(client, SHA, wall_clock=lambda: NOW)

    def refused(self, world, code=None):
        client, _ = self.client(world)
        with self.assertRaises(collect.ContractCollectionRefusal) as caught:
            self.observed(client)
        if code: self.assertEqual(str(caught.exception), "contract-label-" + code)
        self.assertNotIn("PRIVATE", str(caught.exception))
        self.assertTrue(client.budget.failed)
        count = len(world.calls)
        with self.assertRaises(collect.ContractCollectionRefusal): self.observed(client)
        self.assertEqual(len(world.calls), count)
        return client

    def test_full_observation_replays_policy_and_existing_final_recheck(self):
        client, world = self.client(); original = client.budget
        obs = self.observed(client)
        self.assertIs(type(obs), policy.ContractObservation)
        self.assertEqual(obs.pulls[0].events[0].node_id, "EVENT_0000")
        self.assertEqual(obs.pulls[0].labels[0].node_id, "LABEL_1")
        self.assertEqual(obs.enumeration, "complete-visible")
        receipt = policy.require_contract_label(obs, candidate_sha=SHA, repository_id=rt.REPOSITORY_ID, now=NOW)
        self.assertEqual(receipt.event_node_id, "EVENT_0000")
        self.assertEqual([row[0] for row in world.graph_calls], ["associations", "labels", "events", "associations"] * 2)
        self.assertIs(client.budget, original); self.assertFalse(original.failed or original.closed)
        with self.assertRaises(FrozenInstanceError): obs.pulls = ()
        client.recheck(client._snapshot, selected_tag="prod-0000", latest_tag="prod-0000")
        self.assertTrue(client._finished)
        original.close()

    def test_literal_queries_match_reviewed_documents(self):
        expected = {"associations": "ec29f21f5a8dcde217a0886a46a72ff02beb8ee6f725fc8f631abb98cf9c8bae",
                    "labels": "0a904164dbbef1f117148119c6d42f49c8b3dfcfcf0be30122d9e390d1e35af0",
                    "events": "8de376f67a14591619cdee5a09d3f7fd50b7799e80346566f75ac9822a7b3ca4"}
        self.assertEqual({name: hashlib.sha256(raw.encode()).hexdigest() for name, raw in rt._CONTRACT_QUERIES.items()}, expected)
        self.assertTrue(all("totalCount" not in raw and "mutation" not in raw for raw in rt._CONTRACT_QUERIES.values()))

    def test_saved_filtered_count_shape_is_not_fabricated_authorization(self):
        world = World(); world.labels = []; world.events = []
        def hook(stage, count, variables, payload):
            if stage == "events": payload["data"]["repository"]["pullRequest"]["timelineItems"]["totalCount"] = 60
        world.hook = hook
        client, _ = self.client(world); obs = self.observed(client)
        self.assertEqual(obs.pulls[0].events, ()); self.assertEqual(obs.pulls[0].labels, ())
        with self.assertRaises(policy.ContractRefusal):
            policy.require_contract_label(obs, candidate_sha=SHA, repository_id=rt.REPOSITORY_ID, now=NOW)
        self.assertFalse(client.budget.failed)

    def test_complete_twenty_page_boundary_and_full_history_retained(self):
        world = World(); world.events = [event(i) for i in range(2000)]
        client, _ = self.client(world); obs = self.observed(client)
        self.assertEqual(len(obs.pulls[0].events), 2000)
        self.assertEqual(len(world.graph_calls), 46)
        self.assertEqual(sum(row[0] == "events" for row in world.graph_calls), 40)
        # Final unlabel remains present in the observation, so policy refuses.
        with self.assertRaisesRegex(policy.ContractRefusal, "revoked"):
            policy.require_contract_label(obs, candidate_sha=SHA, repository_id=rt.REPOSITORY_ID, now=NOW)

    def test_page_bound_and_empty_continuation_refuse(self):
        world = World(); world.events = [event(i) for i in range(2001)]
        self.refused(world, "event-bound")
        self.assertEqual(sum(row[0] == "events" for row in world.graph_calls), 20)
        world = World(); world.events = [event(i) for i in range(21)]; world.page_size = 1
        self.refused(world, "event-bound")
        world = World(); world.events = [event(i) for i in range(101)]
        def empty(stage, count, variables, payload):
            if stage == "events" and variables.get("after"):
                payload["data"]["repository"]["pullRequest"]["timelineItems"] = connection([], "event")
        world.hook = empty; self.refused(world, "pagination")

    def test_cross_page_duplicate_ids_cursors_and_nonadvancement_refuse(self):
        for mode in ("id", "cursor", "end", "cycle"):
            with self.subTest(mode=mode):
                world = World(); world.events = [event(i) for i in range(101)]
                def hook(stage, count, variables, payload):
                    if stage != "events" or not variables.get("after"): return
                    connection_ = payload["data"]["repository"]["pullRequest"]["timelineItems"]
                    if mode == "id": connection_["edges"][0]["node"]["id"] = "EVENT_0000"
                    if mode == "cursor":
                        connection_["edges"][0]["cursor"] = "event-1"
                        connection_["pageInfo"]["endCursor"] = "event-1"
                    if mode == "end": connection_["pageInfo"]["endCursor"] = "PRIVATE"
                    if mode == "cycle":
                        connection_["edges"][0]["cursor"] = variables["after"]
                        connection_["pageInfo"] = {"hasNextPage": True, "endCursor": variables["after"]}
                world.hook = hook; self.refused(world)

    def test_multiple_or_incomplete_associations_are_never_filtered(self):
        for mode in ("none", "multiple", "more", "duplicate", "foreign"):
            with self.subTest(mode=mode):
                world = World()
                if mode == "none": world.pulls = []
                if mode in ("multiple", "duplicate"):
                    other = pull()
                    if mode == "multiple": other.update(id="PULL_2", number=269, baseRefName="other")
                    world.pulls.append(other)
                if mode == "foreign": world.pulls[0]["repository"]["databaseId"] = 42
                def hook(stage, count, variables, payload):
                    if stage == "associations" and mode == "more":
                        payload["data"]["repository"]["object"]["associatedPullRequests"]["pageInfo"]["hasNextPage"] = True
                world.hook = hook; self.refused(world)

    def test_pull_source_state_and_owner_bindings_refuse(self):
        mutations = [lambda root: root.update(databaseId=True),
            lambda root: root.update(nameWithOwner="PRIVATE/repo"),
            lambda root: root["owner"].update(__typename="Organization"),
            lambda root: root["owner"].update(login="PRIVATE"),
            lambda root: root["defaultBranchRef"]["target"].update(oid="b"*40),
            lambda root: root["object"].update(oid="b"*40),
            lambda root: root["object"].update(__typename="Tag")]
        for mutation in mutations:
            world = World(); world.hook = lambda stage, count, variables, payload: mutation(payload["data"]["repository"])
            self.refused(world)
        for key, value in (("merged", False), ("isDraft", True), ("state", "CLOSED"),
                ("number", True), ("baseRefName", "feature"), ("mergeCommit", {"oid": "b"*40}),
                ("mergeCommit", None), ("headRefOid", "main"), ("mergedAt", stamp(NOW))):
            world = World(); world.pulls[0][key] = value; self.refused(world)

    def test_label_connection_bound_duplicates_and_missing_identity_refuse(self):
        for mode in ("more", "count", "id", "name", "null", "time", "repository"):
            with self.subTest(mode=mode):
                world = World()
                if mode == "count": world.labels = [dict(label(), id=f"L_{i}", name=f"label-{i}") for i in range(101)]
                if mode == "id": world.labels.append(dict(label(), name="other"))
                if mode == "name": world.labels.append(dict(label(), id="LABEL_2"))
                if mode == "null": world.labels[0]["updatedAt"] = None
                if mode == "time": world.labels[0]["updatedAt"] = stamp(NOW + timedelta(seconds=1))
                if mode == "repository": world.labels[0]["repository"]["databaseId"] = 42
                def hook(stage, count, variables, payload):
                    if stage == "labels" and mode == "more":
                        payload["data"]["repository"]["pullRequest"]["labels"]["pageInfo"]["hasNextPage"] = True
                world.hook = hook; self.refused(world)

    def test_event_null_foreign_and_unknown_union_never_disappear(self):
        mutations = [lambda row: row.update(actor=None), lambda row: row.update(label=None),
            lambda row: row.update(labelable=None), lambda row: row.update(__typename="Comment"),
            lambda row: row["actor"].update(__typename="Unknown"),
            lambda row: row["actor"].update(id="PRIVATE\n"),
            lambda row: row["labelable"].update(id="PULL_OTHER"),
            lambda row: row["labelable"].update(__typename="Issue"),
            lambda row: row["labelable"]["repository"].update(databaseId=42),
            lambda row: row.update(createdAt=stamp(NOW + timedelta(seconds=1))),
            lambda row: row.update(createdAt=stamp(NOW - timedelta(days=11)))]
        for mutation in mutations:
            world = World(); mutation(world.events[0]); self.refused(world)

    def test_policy_not_collection_decides_effective_owner_and_postmerge(self):
        for mode in ("other-user", "bot", "premerge", "definition", "renamed", "reapplied", "tied"):
            with self.subTest(mode=mode):
                world = World()
                if mode == "other-user": world.events[0]["actor"].update(id="OTHER", login="another")
                if mode == "bot": world.events[0]["actor"].update(__typename="Bot", id="BOT", login="automation[bot]")
                if mode == "premerge": world.events[0]["createdAt"] = world.pulls[0]["mergedAt"]
                if mode == "definition":
                    world.labels[0]["updatedAt"] = stamp(NOW)
                    world.events[0]["label"] = copy.deepcopy(world.labels[0])
                if mode == "renamed": world.events[0]["label"]["id"] = "OTHER_LABEL"
                if mode in ("reapplied", "tied"):
                    world.events = [event(i) for i in range(3)]
                    if mode == "reapplied": world.events[-1]["actor"].update(id="OTHER", login="another")
                    else: world.events[-1]["createdAt"] = world.events[-2]["createdAt"]
                client, _ = self.client(world); obs = self.observed(client)
                self.assertEqual(len(obs.pulls[0].events), len(world.events))
                with self.assertRaises(policy.ContractRefusal):
                    policy.require_contract_label(obs, candidate_sha=SHA, repository_id=rt.REPOSITORY_ID, now=NOW)
                self.assertFalse(client.budget.failed)

    def test_two_complete_passes_and_closing_association_detect_drift(self):
        for mode in ("closing-pull", "second-label", "second-event", "owner", "second-empty"):
            with self.subTest(mode=mode):
                world = World()
                def hook(stage, count, variables, payload):
                    root = payload["data"]["repository"]
                    if mode == "closing-pull" and stage == "associations" and count == 2:
                        root["object"]["associatedPullRequests"]["edges"][0]["node"]["headRefOid"] = "c"*40
                    if mode == "second-label" and stage == "labels" and count == 2:
                        root["pullRequest"]["labels"]["edges"][0]["node"]["updatedAt"] = stamp(NOW - timedelta(hours=12))
                    if mode == "second-event" and stage == "events" and count == 2:
                        root["pullRequest"]["timelineItems"]["edges"][0]["node"]["actor"]["id"] = "OTHER"
                    if mode == "owner" and stage == "labels": root["owner"]["id"] = "OWNER_2"
                    if mode == "second-empty" and stage == "events" and count == 2:
                        root["pullRequest"]["timelineItems"] = connection([], "event")
                world.hook = hook; self.refused(world, "changed")

    def test_collection_times_can_advance_without_comparing_timestamps_as_facts(self):
        client, world = self.client(); tick = iter(NOW + timedelta(microseconds=i) for i in range(20))
        obs = collect.collect_contract(client, SHA, wall_clock=lambda: next(tick))
        self.assertGreater(obs.observed_at, NOW)
        self.assertEqual(len(world.graph_calls), 8)

    def test_time_shape_backward_and_future_observations_refuse(self):
        for times in ([NOW, NOW - timedelta(seconds=1)], [None], [NOW.replace(tzinfo=None)]):
            client, world = self.client(); iterator = iter(times)
            with self.assertRaises(collect.ContractCollectionRefusal):
                collect.collect_contract(client, SHA, wall_clock=lambda: next(iterator))
            self.assertTrue(client.budget.failed)
        for value in ("2026-10-11", "2026-10-11T12:00:00+00:00", "2026-99-11T12:00:00Z", "PRIVATE", 123):
            world = World(); world.events[0]["createdAt"] = value; self.refused(world)

    def test_partial_unknown_and_duplicate_json_envelopes_refuse(self):
        for raw in (b'{"data":{},"data":{}}', b'{"data":{"repository":null}}', b'{"data":NaN}',
                    b'{"data":{},"errors":[{"message":"PRIVATE"}]}', b'[]', b'null', b'\xff',
                    b'{"data":{},"extensions":{}}', b'{"data":1.5}'):
            world = World(); world.raw = raw; self.refused(world)
        world = World(); world.raw = b'[' * 22 + b'0' + b']' * 22; self.refused(world, "json-bound")
        world = World(); world.raw = b'[' + b','.join([b'0'] * 50001) + b']'; self.refused(world, "json-bound")

    def test_cursor_edge_and_pageinfo_types_refuse(self):
        def mutate(mode, value):
            if mode == "null": value["edges"] = None
            elif mode == "missing": value["edges"][0].pop("node")
            elif mode == "cursor": value["edges"][0]["cursor"] = "PRIVATE\n"
            elif mode == "long": value["edges"][0]["cursor"] = "a" * 1025
            elif mode == "flag": value["pageInfo"]["hasNextPage"] = 0
            elif mode == "end": value["pageInfo"]["endCursor"] = None
            elif mode == "duplicate": value["edges"].append(copy.deepcopy(value["edges"][0]))
        for mode in ("null", "missing", "cursor", "long", "flag", "end", "duplicate"):
            world = World()
            def hook(stage, count, variables, payload):
                if stage == "events": mutate(mode, payload["data"]["repository"]["pullRequest"]["timelineItems"])
            world.hook = hook; self.refused(world)

    def test_existing_snapshot_open_scope_and_once_only_are_mandatory(self):
        client, world = self.client(snapshot=False)
        with self.assertRaises(collect.ContractCollectionRefusal): self.observed(client)
        self.assertEqual(world.graph_calls, [])
        for mode in ("finished", "closed", "failed", "wrong-candidate", "scope"):
            client, world = self.client()
            if mode == "finished": client._finished = True
            if mode == "closed": client.budget.close()
            if mode == "failed": client.budget.failed = True
            if mode == "scope": client._image_scope = ("foreign",)
            with self.assertRaises(collect.ContractCollectionRefusal):
                collect.collect_contract(client, "b"*40 if mode == "wrong-candidate" else SHA, wall_clock=lambda: NOW)
            self.assertEqual(world.graph_calls, []); self.assertTrue(client.budget.failed)
        client, world = self.client(); self.observed(client); count = len(world.calls)
        with self.assertRaisesRegex(collect.ContractCollectionRefusal, "repeat"): self.observed(client)
        self.assertEqual(len(world.calls), count)
        with self.assertRaises(collect.ContractCollectionRefusal): collect.collect_contract(object(), SHA)

    def test_original_command_byte_and_cleanup_reserve_cannot_reset(self):
        for mode in ("commands", "bytes", "deadline"):
            client, world = self.client()
            if mode == "commands": client.budget.commands = rt.MAX_COMMANDS
            if mode == "bytes": client.budget.captured_bytes = rt.MAX_BYTES - rt._CONTRACT_STDERR
            if mode == "deadline": self.clock.now = 700 - rt.CLEANUP_RESERVE
            with self.assertRaises(collect.ContractCollectionRefusal): self.observed(client)
            self.assertEqual(world.graph_calls, []); self.assertTrue(client.budget.failed)
            self.clock.now = 100
        client, world = self.client(); self.clock.now = 697
        self.observed(client)
        self.assertTrue(all(options["timeout"] == 0.75 for _, _, options in world.graph_calls))

    def test_collector_aggregate_allowance_never_exceeds_eight_mib(self):
        world = World(); world.events = [event(i) for i in range(101)]; world.pad = True
        client, _ = self.client(world); initial = client.budget.captured_bytes
        with self.assertRaises(collect.ContractCollectionRefusal): self.observed(client)
        self.assertLessEqual(client.budget.captured_bytes - initial, collect.MAX_BYTES)
        self.assertLessEqual(len(world.graph_calls), 9)
        self.assertTrue(client.budget.failed)
        remaining = collect.MAX_BYTES
        for _, _, options in world.graph_calls:
            self.assertLessEqual(options["stdout_limit"] + options["stderr_limit"], remaining)
            remaining -= options["stdout_limit"]

    def test_transport_failure_and_capture_exception_never_expose_private_response(self):
        for mode in ("stderr", "nonzero", "exception", "overflow"):
            client, world = self.client()
            def capture(argv, **options):
                if mode == "exception": raise RuntimeError("PRIVATE auth and response")
                if mode == "overflow": return rt.runtime.Capture(0, b"x" * (options["stdout_limit"] + 1), b"")
                return rt.runtime.Capture(1 if mode == "nonzero" else 0, b"PRIVATE stdout", b"PRIVATE stderr" if mode == "stderr" else b"")
            client.budget._capture = capture
            with self.assertRaises(collect.ContractCollectionRefusal) as caught: self.observed(client)
            self.assertEqual(str(caught.exception), "contract-label-collection")
            self.assertTrue(client.budget.failed); self.assertIsNone(client._contract_request)


if __name__ == "__main__":
    unittest.main()
