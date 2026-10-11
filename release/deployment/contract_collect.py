"""Bounded visible owner-label observations; no caller or permission activation.

Only an already-open ReleaseGh is accepted. Two matching full captures share
its existing operation allowance. The returned immutable data is not a token
or a certificate that provider history was never erased. Pure authorization,
fresh source checks and provider admission remain separate caller obligations.
"""
from dataclasses import replace
from datetime import datetime, timedelta, timezone
import re

import contract_authorization as policy
import record_transport as transport
import ship_common as common

MAX_BYTES = 8 * 1024 * 1024
MAX_CALLS = 46
MAX_PAGES = 20
MAX_JSON_NODES = 50000
MAX_JSON_DEPTH = 20
_TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?Z\Z")
_PULL_FIELDS = {"id", "number", "merged", "isDraft", "state", "baseRefName",
                "mergeCommit", "headRefOid", "mergedAt", "repository"}
_REPO_FIELDS = {"databaseId", "nameWithOwner", "owner", "defaultBranchRef", "object"}


class ContractCollectionRefusal(policy.ContractRefusal):
    """Fixed collection codes only; raw responses and credentials stay private."""


def _need(condition, detail="shape"):
    if not condition:
        raise ContractCollectionRefusal("contract-label-" + detail)


def _object(value, fields):
    _need(type(value) is dict and set(value) == fields)
    return value


def _node(value):
    _need(policy._node(value))
    return value


def _cursor(value):
    _need(type(value) is str and 0 < len(value) <= 1024
          and all(32 <= ord(char) < 127 for char in value), "cursor")
    return value


def _timestamp(value):
    _need(type(value) is str and _TIME.fullmatch(value) is not None, "time")
    try:
        return datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError:
        raise ContractCollectionRefusal("contract-label-time") from None


def _identity(value):
    _object(value, {"databaseId", "nameWithOwner"})
    _need(type(value["databaseId"]) is int and value["databaseId"] == transport.REPOSITORY_ID
          and value["nameWithOwner"] == common.REPOSITORY, "repository")


def _connection(value):
    # totalCount, when present in a saved-shaped response, can include other
    # timeline item types. It is never an enumeration or termination authority.
    _need(type(value) is dict and {"edges", "pageInfo"} <= set(value)
          and set(value) <= {"edges", "pageInfo", "totalCount"})
    page = _object(value["pageInfo"], {"hasNextPage", "endCursor"})
    edges = value["edges"]
    _need(type(page["hasNextPage"]) is bool and type(edges) is list and len(edges) <= 100)
    if not edges:
        _need(page == {"hasNextPage": False, "endCursor": None}, "pagination")
        return [], False, None, ()
    cursors, rows, ids = [], [], set()
    for edge in edges:
        _object(edge, {"cursor", "node"})
        cursor = _cursor(edge["cursor"])
        _need(cursor not in cursors, "duplicate")
        node = edge["node"]
        _need(type(node) is dict and "id" in node)
        identity = _node(node["id"])
        _need(identity not in ids, "duplicate")
        cursors.append(cursor); rows.append(node); ids.add(identity)
    _need(page["endCursor"] == cursors[-1], "pagination")
    return rows, page["hasNextPage"], page["endCursor"], tuple(cursors)


def _pull(value, candidate, now, *, extra=None):
    _object(value, _PULL_FIELDS | ({extra} if extra else set()))
    _identity(value["repository"])
    _object(value["mergeCommit"], {"oid"})
    _need(type(value["number"]) is int and 0 < value["number"] < 2**31
          and value["merged"] is True and value["isDraft"] is False
          and value["state"] == "MERGED" and value["baseRefName"] == "main"
          and value["mergeCommit"]["oid"] == candidate
          and policy._sha(value["headRefOid"]), "pull")
    merged = _timestamp(value["mergedAt"])
    _need(merged < now, "time")
    return policy.MergedPull(_node(value["id"]), value["number"], transport.REPOSITORY_ID,
        common.REPOSITORY, "main", candidate, value["headRefOid"], True, False, merged, (), ())


def _label(value, now):
    _object(value, {"id", "name", "createdAt", "updatedAt", "repository"})
    _identity(value["repository"])
    result = policy.LabelIdentity(_node(value["id"]), transport.REPOSITORY_ID,
        value["name"], _timestamp(value["createdAt"]), _timestamp(value["updatedAt"]))
    try:
        policy._label(result, transport.REPOSITORY_ID, now)
    except policy.ContractRefusal:
        raise ContractCollectionRefusal("contract-label-label") from None
    return result


def _event(value, pull, now):
    _object(value, {"__typename", "id", "createdAt", "actor", "label", "labelable"})
    _need(value["__typename"] in ("LabeledEvent", "UnlabeledEvent"), "event")
    target = _object(value["labelable"], {"__typename", "id", "repository"})
    _identity(target["repository"])
    _need(target["__typename"] == "PullRequest" and target["id"] == pull.node_id, "event")
    actor = _object(value["actor"], {"__typename", "id", "login"})
    _need(actor["__typename"] in ("User", "Bot", "Organization")
          and type(actor["login"]) is str and policy._LOGIN.fullmatch(actor["login"]) is not None,
          "actor")
    label = _label(value["label"], now)
    occurred = _timestamp(value["createdAt"])
    _need(label.created_at <= occurred <= now, "time")
    return policy.LabelEvent(_node(value["id"]),
        "labeled" if value["__typename"] == "LabeledEvent" else "unlabeled",
        pull.node_id, label, actor["__typename"], _node(actor["id"]), actor["login"], occurred)


def _json(raw):
    try:
        value = common.loads_strict(raw)
    except common.ReleaseError:
        raise ContractCollectionRefusal("contract-label-json") from None
    stack, count = [(value, 0)], 0
    while stack:
        item, depth = stack.pop(); count += 1
        _need(count <= MAX_JSON_NODES and depth <= MAX_JSON_DEPTH, "json-bound")
        if type(item) is dict:
            stack.extend((x, depth + 1) for x in item.values())
        elif type(item) is list:
            stack.extend((x, depth + 1) for x in item)
        else:
            _need(item is None or type(item) in (str, int, bool), "json")
    _object(value, {"data"})  # Any errors/partial result or unknown envelope refuses.
    return _object(value["data"], {"repository"})["repository"]


class _Capture:
    def __init__(self, client, candidate, wall_clock):
        self.client, self.candidate, self.wall_clock = client, candidate, wall_clock
        self.start_bytes = client.budget.captured_bytes
        self.calls = 0
        self.last_time = None
        self.header = None

    def now(self):
        value = self.wall_clock()
        _need(type(value) is datetime and value.utcoffset() == timedelta(0)
              and (self.last_time is None or self.last_time <= value), "time")
        self.last_time = value
        return value

    def page(self, stage, *, number=None, after=None):
        self.client._check()
        allowance = MAX_BYTES - (self.client.budget.captured_bytes - self.start_bytes)
        _need(self.calls < MAX_CALLS and transport._CONTRACT_STDERR < allowance <= MAX_BYTES, "bound")
        self.calls += 1
        raw = self.client._contract_page(stage, self.candidate, number=number,
                                         after=after, byte_allowance=allowance)
        self.client._check()
        _need(self.client.budget.captured_bytes - self.start_bytes <= MAX_BYTES, "bound")
        root = _json(raw)
        now = self.now()
        _object(root, _REPO_FIELDS | ({"pullRequest"} if stage != "associations" else set()))
        _identity({key: root[key] for key in ("databaseId", "nameWithOwner")})
        owner = _object(root["owner"], {"__typename", "login", "id"})
        _need(owner["__typename"] == "User" and owner["login"] == common.APPROVER_LOGIN, "owner")
        owner_id = _node(owner["id"])
        branch = _object(root["defaultBranchRef"], {"name", "target"})
        _object(branch["target"], {"oid"})
        _need(branch["name"] == "main" and branch["target"]["oid"] == self.candidate, "source")
        commit = root["object"]
        _object(commit, {"__typename", "oid"} | ({"associatedPullRequests"} if stage == "associations" else set()))
        _need(commit["__typename"] == "Commit" and commit["oid"] == self.candidate, "source")
        header = (transport.REPOSITORY_ID, common.REPOSITORY, self.candidate, "User", owner_id, common.APPROVER_LOGIN)
        if self.header is None:
            self.header = header
        _need(header == self.header, "changed")
        return root, now

    def association(self):
        root, now = self.page("associations")
        rows, more, _, _ = _connection(root["object"]["associatedPullRequests"])
        _need(not more and len(rows) == 1, "association")
        return _pull(rows[0], self.candidate, now)

    def observe(self):
        pull = self.association()
        root, now = self.page("labels", number=pull.number)
        _need(_pull(root["pullRequest"], self.candidate, now, extra="labels") == pull, "changed")
        rows, more, _, _ = _connection(root["pullRequest"]["labels"])
        _need(not more and len(rows) <= policy.MAX_LABELS, "label-bound")
        labels = tuple(sorted((_label(row, now) for row in rows), key=lambda x: x.node_id))
        _need(len({label.name for label in labels}) == len(labels), "duplicate")
        events, ids, cursors = [], set(), set()
        after = None
        for _ in range(MAX_PAGES):
            root, now = self.page("events", number=pull.number, after=after)
            _need(_pull(root["pullRequest"], self.candidate, now, extra="timelineItems") == pull, "changed")
            rows, more, end, edges = _connection(root["pullRequest"]["timelineItems"])
            _need(after is None or bool(rows), "pagination")
            _need(not cursors.intersection(edges), "cursor")
            cursors.update(edges)
            for row in rows:
                event = _event(row, pull, now)
                _need(event.node_id not in ids, "duplicate")
                ids.add(event.node_id); events.append(event)
            _need(len(events) <= policy.MAX_EVENTS, "event-bound")
            if not more:
                break
            _need(len(events) < policy.MAX_EVENTS and end not in (None, after), "event-bound")
            after = end
        else:
            raise ContractCollectionRefusal("contract-label-event-bound")
        _need(self.association() == pull, "changed")
        self.client._check()
        observed = self.now()
        complete_pull = replace(pull, labels=labels, events=tuple(sorted(events, key=lambda x: x.node_id)))
        return policy.ContractObservation(*self.header, (complete_pull,), observed, "complete-visible")


def collect_contract(client, candidate_sha, *, wall_clock=lambda: datetime.now(timezone.utc)):
    """Return full stable data; pure policy and fresh provider admission are separate.

    This preparatory collector must run before ReleaseGh.recheck/operation close.
    It does not create clients, reset budgets, read credentials or authorize SQL.
    """
    _need(type(client) is transport.ReleaseGh, "client")
    success = False
    try:
        client._check()
        _need(not client._contract_collected, "repeat")
        client._contract_collected = True
        _need(policy._sha(candidate_sha) and type(client._snapshot) is transport.InventorySnapshot
              and client._snapshot.repository_id == transport.REPOSITORY_ID
              and candidate_sha == client._snapshot.main_sha, "source")
        capture = _Capture(client, candidate_sha, wall_clock)
        capture.now()
        before, after = capture.observe(), capture.observe()
        # Only collection time differs; compare all explicitly typed facts.
        _need(replace(before, observed_at=after.observed_at) == after, "changed")
        client._check()
        success = True
        return after
    except ContractCollectionRefusal:
        raise
    except Exception:
        raise ContractCollectionRefusal("contract-label-collection") from None
    finally:
        if not success:
            client.budget.failed = True
