"""Pure policy for a narrow post-merge owner label; no collection or authority.

A future bounded GitHub adapter must bind these facts to the authenticated
repository/current main, enumerate the complete visible association/label event
connections, and recheck mutable state before provider access. Constructing a
dataclass or an unkeyed digest does none of those things. This module is not
selected by shipping and does not authorize database changes.

REST label-event names alone are insufficient. The projection requires GraphQL
label node identity and its non-null creation/update times. The definition must
predate the effective owner event; later edits, including renames, refuse. A
deleted/null/partial connection must be rejected by the adapter, not projected
as an empty connection. GitHub's returned visible timeline remains the source;
this is not an independent certificate that service history was never erased.
"""
from dataclasses import dataclass
from datetime import datetime, timedelta
import hashlib
import json
import re

import ship_common as common

LABEL_NAME = "db:contract"
MAX_EVENTS = 2000
MAX_LABELS = 100
_NODE = re.compile(r"[A-Za-z0-9_+/=-]{1,256}\Z")
_LOGIN = re.compile(r"[A-Za-z0-9][A-Za-z0-9-]{0,38}(?:\[bot\])?\Z")


class ContractRefusal(Exception):
    """Fixed reason codes only; no raw provider values in exceptions."""


@dataclass(frozen=True, slots=True)
class LabelIdentity:
    node_id: str
    repository_id: int
    name: str
    created_at: datetime
    updated_at: datetime


@dataclass(frozen=True, slots=True)
class LabelEvent:
    node_id: str
    kind: str
    pull_node_id: str
    label: LabelIdentity
    actor_type: str
    actor_node_id: str
    actor_login: str
    occurred_at: datetime


@dataclass(frozen=True, slots=True)
class MergedPull:
    node_id: str
    number: int
    repository_id: int
    repository: str
    base_ref: str
    merge_sha: str
    head_sha: str
    merged: bool
    draft: bool
    merged_at: datetime
    labels: tuple[LabelIdentity, ...]
    events: tuple[LabelEvent, ...]


@dataclass(frozen=True, slots=True)
class ContractObservation:
    repository_id: int
    repository: str
    main_sha: str
    owner_type: str
    owner_node_id: str
    owner_login: str
    pulls: tuple[MergedPull, ...]
    observed_at: datetime
    enumeration: str


@dataclass(frozen=True, slots=True)
class ContractReceipt:
    candidate_sha: str
    repository_id: int
    pull_number: int
    pull_node_id: str
    label_node_id: str
    event_node_id: str
    owner_node_id: str
    merged_at: datetime
    label_updated_at: datetime
    labeled_at: datetime
    observed_at: datetime
    evidence_sha256: str


def _need(condition, code="invalid"):
    if not condition:
        raise ContractRefusal("contract-label-" + code)


def _id(value):
    return type(value) is int and 0 < value < 2 ** 63


def _node(value):
    return type(value) is str and _NODE.fullmatch(value) is not None


def _time(value):
    return type(value) is datetime and value.utcoffset() == timedelta(0)


def _sha(value):
    return type(value) is str and common.SHA1_RE.fullmatch(value) is not None


def _label(value, repository_id, now):
    _need(type(value) is LabelIdentity and _node(value.node_id)
          and _id(value.repository_id) and value.repository_id == repository_id
          and type(value.name) is str and 0 < len(value.name) <= 50
          and all(ord(char) >= 32 and ord(char) != 127 for char in value.name)
          and _time(value.created_at) and _time(value.updated_at)
          and value.created_at <= value.updated_at <= now)
    return [value.node_id, value.repository_id, value.name,
            value.created_at.isoformat(), value.updated_at.isoformat()]


def require_contract_label(observation, *, candidate_sha, repository_id, now):
    """Validate data, not its origin; adapter/caller rechecks remain mandatory.

Only a single associated, non-draft, merged-main PR for exactly candidate_sha
is supported. The effective label must have been applied by the repository's
User owner after the merge. Pre-merge authorization and ambiguous timestamps
are deliberately outside this initial subset. No author/merged_by fallback.
"""
    _need(_sha(candidate_sha) and _id(repository_id) and _time(now), "input-invalid")
    value = observation
    _need(type(value) is ContractObservation)
    _need(_id(value.repository_id) and value.repository_id == repository_id
          and type(value.repository) is str and value.repository == common.REPOSITORY
          and _sha(value.main_sha) and value.main_sha == candidate_sha, "source")
    _need(type(value.owner_type) is str and value.owner_type == "User"
          and _node(value.owner_node_id) and type(value.owner_login) is str
          and value.owner_login == common.APPROVER_LOGIN, "owner")
    _need(_time(value.observed_at) and value.observed_at <= now
          and type(value.enumeration) is str and value.enumeration == "complete-visible", "coverage")
    _need(type(value.pulls) is tuple and len(value.pulls) == 1, "association")
    pull = value.pulls[0]
    _need(type(pull) is MergedPull and _node(pull.node_id) and _id(pull.number)
          and _id(pull.repository_id) and pull.repository_id == repository_id
          and type(pull.repository) is str and pull.repository == common.REPOSITORY
          and type(pull.base_ref) is str and pull.base_ref == "main"
          and _sha(pull.merge_sha) and pull.merge_sha == candidate_sha
          and _sha(pull.head_sha) and pull.merged is True and pull.draft is False
          and _time(pull.merged_at) and pull.merged_at < value.observed_at, "pull")
    _need(type(pull.labels) is tuple and len(pull.labels) <= MAX_LABELS
          and type(pull.events) is tuple and 1 <= len(pull.events) <= MAX_EVENTS, "coverage")
    labels, label_ids, label_names = [], set(), set()
    for label in pull.labels:
        labels.append(_label(label, repository_id, value.observed_at))
        _need(label.node_id not in label_ids and label.name not in label_names)
        label_ids.add(label.node_id); label_names.add(label.name)
    matching = [label for label in pull.labels if label.name == LABEL_NAME]
    _need(len(matching) == 1, "missing")
    current = matching[0]
    rows, ids, relevant = [], set(), []
    for event in pull.events:
        _need(type(event) is LabelEvent and _node(event.node_id) and event.node_id not in ids
              and type(event.kind) is str and event.kind in ("labeled", "unlabeled")
              and _node(event.pull_node_id) and event.pull_node_id == pull.node_id
              and type(event.actor_type) is str and event.actor_type in ("User", "Bot", "Organization")
              and _node(event.actor_node_id) and type(event.actor_login) is str
              and _LOGIN.fullmatch(event.actor_login) is not None
              and _time(event.occurred_at) and event.occurred_at <= value.observed_at)
        identity = _label(event.label, repository_id, value.observed_at)
        _need(event.label.created_at <= event.occurred_at)
        ids.add(event.node_id)
        rows.append([event.node_id, event.kind, event.pull_node_id, identity,
                     event.actor_type, event.actor_node_id, event.actor_login, event.occurred_at.isoformat()])
        if event.label.node_id == current.node_id or event.label.name == LABEL_NAME:
            _need(event.label == current, "identity")
            relevant.append(event)
    _need(relevant, "missing")
    relevant.sort(key=lambda event: event.occurred_at)
    _need(all(left.occurred_at < right.occurred_at for left, right in zip(relevant, relevant[1:])),
          "ambiguous-order")
    present, effective = False, None
    for event in relevant:
        _need((event.kind == "labeled") != present, "lifecycle")
        present = event.kind == "labeled"
        effective = event if present else None
    _need(present and effective is not None, "revoked")
    _need(effective.actor_type == "User" and effective.actor_node_id == value.owner_node_id
          and effective.actor_login == common.APPROVER_LOGIN, "owner")
    _need(pull.merged_at < effective.occurred_at, "premerge")
    _need(current.updated_at < effective.occurred_at, "definition-changed")
    # Only stable observed facts go in the digest; collection time stays in the
    # receipt. A later fresh capture of the same facts can therefore be compared.
    facts = [value.repository_id, value.repository, value.main_sha, value.owner_type,
             value.owner_node_id, value.owner_login, value.enumeration, pull.node_id,
             pull.number, pull.repository_id, pull.repository, pull.base_ref,
             pull.merge_sha, pull.head_sha, pull.merged, pull.draft, pull.merged_at.isoformat(),
             sorted(labels), sorted(rows)]
    digest = hashlib.sha256(json.dumps(facts, sort_keys=True, separators=(",", ":"),
                                      ensure_ascii=True).encode("ascii")).hexdigest()
    return ContractReceipt(candidate_sha, repository_id, pull.number, pull.node_id,
        current.node_id, effective.node_id, value.owner_node_id, pull.merged_at,
        current.updated_at, effective.occurred_at, value.observed_at, digest)
