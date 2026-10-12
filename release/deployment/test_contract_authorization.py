"""Synthetic owner-label provenance cases; no GitHub calls or label writes."""
from dataclasses import FrozenInstanceError, replace
from datetime import datetime, timedelta, timezone
import unittest

import contract_authorization as policy

NOW = datetime(2026, 10, 11, 12, 0, tzinfo=timezone.utc)
SHA = "a" * 40
REPO = 123


def fixture():
    label = policy.LabelIdentity("LABEL_1", REPO, policy.LABEL_NAME,
        NOW - timedelta(days=10), NOW - timedelta(days=2))
    event = policy.LabelEvent("EVENT_1", "labeled", "PULL_1", label,
        "User", "OWNER_1", policy.common.APPROVER_LOGIN, NOW - timedelta(minutes=1))
    pull = policy.MergedPull("PULL_1", 30, REPO, policy.common.REPOSITORY, "main",
        SHA, "b" * 40, True, False, NOW - timedelta(hours=1), (label,), (event,))
    return policy.ContractObservation(REPO, policy.common.REPOSITORY, SHA,
        "User", "OWNER_1", policy.common.APPROVER_LOGIN, (pull,), NOW, "complete-visible")


def change_pull(value, **changes):
    return replace(value, pulls=(replace(value.pulls[0], **changes),))


def change_event(value, **changes):
    return change_pull(value, events=(replace(value.pulls[0].events[0], **changes),))


def change_label(value, **changes):
    label = replace(value.pulls[0].labels[0], **changes)
    return change_pull(value, labels=(label,), events=(replace(value.pulls[0].events[0], label=label),))


class ContractLabelTests(unittest.TestCase):
    def check(self, value=None, **changes):
        args = dict(candidate_sha=SHA, repository_id=REPO, now=NOW)
        args.update(changes)
        return policy.require_contract_label(fixture() if value is None else value, **args)

    def refuse(self, value, code=None, **changes):
        with self.assertRaises(policy.ContractRefusal) as caught:
            self.check(value, **changes)
        self.assertTrue(str(caught.exception).startswith("contract-label-"))
        self.assertNotIn("PRIVATE", str(caught.exception))
        if code: self.assertEqual(str(caught.exception), "contract-label-" + code)

    def test_exact_postmerge_owner_event_produces_bound_immutable_receipt(self):
        result = self.check()
        self.assertEqual((result.candidate_sha, result.repository_id, result.pull_number), (SHA, REPO, 30))
        self.assertEqual((result.owner_node_id, result.label_node_id, result.event_node_id),
                         ("OWNER_1", "LABEL_1", "EVENT_1"))
        self.assertEqual(result.observed_at, NOW)
        self.assertRegex(result.evidence_sha256, "^[0-9a-f]{64}$")
        with self.assertRaises(FrozenInstanceError): result.pull_number = 31

    def test_source_repository_and_current_main_are_exact(self):
        for changes in ({"repository_id": REPO + 1}, {"repository": "PRIVATE/fork"},
                        {"main_sha": "c" * 40}, {"repository_id": True}):
            self.refuse(replace(fixture(), **changes), "source")

    def test_wrong_base_repository_or_merge_sha_is_not_association_authority(self):
        for changes in ({"repository_id": REPO + 1}, {"repository": "PRIVATE/fork"},
                        {"base_ref": "PRIVATE"}, {"merge_sha": "c" * 40}, {"head_sha": "short"}):
            self.refuse(change_pull(fixture(), **changes), "pull")

    def test_zero_and_multiple_associated_pulls_refuse(self):
        value = fixture()
        for pulls in ((), value.pulls * 2, list(value.pulls)):
            self.refuse(replace(value, pulls=pulls), "association")

    def test_unmerged_draft_and_nonboolean_state_refuse(self):
        for changes in ({"merged": False}, {"draft": True}, {"merged": 1}, {"draft": 0}):
            self.refuse(change_pull(fixture(), **changes), "pull")

    def test_premerge_and_equal_second_event_are_not_final_source_authority(self):
        value = fixture()
        for time in (value.pulls[0].merged_at, value.pulls[0].merged_at - timedelta(seconds=1)):
            self.refuse(change_event(value, occurred_at=time), "premerge")

    def test_repository_owner_must_be_configured_user(self):
        for changes in ({"owner_type": "Organization"}, {"owner_type": "Bot"},
                        {"owner_login": "someone"}, {"owner_node_id": None}):
            self.refuse(replace(fixture(), **changes), "owner")

    def test_actor_login_and_node_identity_are_both_required(self):
        for changes in ({"actor_type": "Bot"}, {"actor_type": "Organization"},
                        {"actor_login": "someone"}, {"actor_node_id": "OTHER_USER"}):
            self.refuse(change_event(fixture(), **changes), "owner")
        self.refuse(change_event(fixture(), actor_node_id=None))

    def test_missing_current_label_and_missing_event_refuse(self):
        self.refuse(change_pull(fixture(), labels=()), "missing")
        self.refuse(change_pull(fixture(), events=()), "coverage")
        self.refuse(change_label(fixture(), name="db:other"), "missing")

    def test_rest_name_only_or_foreign_label_identity_is_insufficient(self):
        value = fixture()
        label = value.pulls[0].labels[0]
        for changes in ({"node_id": "OTHER_LABEL"}, {"name": "renamed"}):
            self.refuse(change_event(value, label=replace(label, **changes)), "identity")
        self.refuse(change_event(value, label={"name": policy.LABEL_NAME, "color": "fff"}))
        self.refuse(change_label(value, repository_id=REPO + 1))

    def test_label_definition_must_predate_effective_owner_event(self):
        value = fixture(); event = value.pulls[0].events[0]
        for changed in (event.occurred_at, event.occurred_at + timedelta(seconds=1)):
            self.refuse(change_label(value, updated_at=changed), "definition-changed")
        # A prior edit can be authorized only by a later owner application.
        self.check(change_label(value, updated_at=event.occurred_at - timedelta(seconds=1)))

    def test_null_future_or_reversed_definition_dates_refuse(self):
        for changes in ({"created_at": None}, {"updated_at": None},
                        {"updated_at": NOW + timedelta(seconds=1)}, {"created_at": NOW}):
            self.refuse(change_label(fixture(), **changes))

    def test_unlabel_revokes_even_if_current_labels_disagree(self):
        value = fixture(); first = value.pulls[0].events[0]
        removed = replace(first, node_id="EVENT_2", kind="unlabeled", occurred_at=NOW - timedelta(seconds=30))
        self.refuse(change_pull(value, events=(first, removed)), "revoked")

    def test_nonowner_reapplication_cannot_reuse_old_owner_event(self):
        value = fixture(); first = value.pulls[0].events[0]
        removed = replace(first, node_id="EVENT_2", kind="unlabeled", occurred_at=NOW - timedelta(seconds=40))
        relabel = replace(first, node_id="EVENT_3", actor_login="someone", actor_node_id="OTHER_USER",
                          occurred_at=NOW - timedelta(seconds=20))
        self.refuse(change_pull(value, events=(first, removed, relabel)), "owner")
        valid = replace(relabel, actor_login=first.actor_login, actor_node_id=first.actor_node_id)
        self.assertEqual(self.check(change_pull(value, events=(first, removed, valid))).event_node_id, "EVENT_3")

    def test_same_timestamp_lifecycle_is_ambiguous_not_sorted_by_id(self):
        value = fixture(); first = value.pulls[0].events[0]
        other = replace(first, node_id="EVENT_2", kind="unlabeled")
        for events in ((first, other), (other, first)):
            self.refuse(change_pull(value, events=events), "ambiguous-order")

    def test_duplicate_ids_and_impossible_lifecycle_refuse(self):
        value = fixture(); first = value.pulls[0].events[0]
        self.refuse(change_pull(value, events=(first, first)))
        self.refuse(change_event(value, kind="unlabeled"), "lifecycle")
        extra = replace(first, node_id="EVENT_2", occurred_at=NOW - timedelta(seconds=30))
        self.refuse(change_pull(value, events=(first, extra)), "lifecycle")

    def test_foreign_pull_unknown_event_and_missing_actor_refuse(self):
        for changes in ({"pull_node_id": "OTHER_PULL"}, {"kind": "renamed"},
                        {"actor_type": None}, {"actor_login": "PRIVATE\n"}):
            self.refuse(change_event(fixture(), **changes))

    def test_incomplete_unknown_and_non_tuples_do_not_establish_coverage(self):
        for enumeration in ("complete", "truncated", "unknown", True):
            self.refuse(replace(fixture(), enumeration=enumeration), "coverage")
        value = fixture()
        self.refuse(change_pull(value, labels=list(value.pulls[0].labels)), "coverage")
        self.refuse(change_pull(value, events=list(value.pulls[0].events)), "coverage")

    def test_bounds_fail_without_tail_truncation(self):
        value = fixture(); pull = value.pulls[0]
        self.refuse(change_pull(value, labels=pull.labels * 101), "coverage")
        self.refuse(change_pull(value, events=pull.events * 2001), "coverage")
        for changes in ({"node_id": "x" * 257}, {"node_id": "PRIVATE\n"}, {"name": "x" * 51}):
            self.refuse(change_label(value, **changes))

    def test_duplicate_current_labels_by_name_or_node_are_invalid(self):
        value = fixture(); label = value.pulls[0].labels[0]
        for extra in (label, replace(label, node_id="LABEL_2"), replace(label, name="other")):
            self.refuse(change_pull(value, labels=(label, extra)))

    def test_time_and_identity_inputs_are_closed(self):
        value = fixture()
        for changes in ({"now": NOW.replace(tzinfo=None)}, {"now": NOW.astimezone(timezone(timedelta(hours=8)))},
                        {"now": "PRIVATE"}, {"repository_id": True}, {"candidate_sha": "PRIVATE"}):
            self.refuse(value, "input-invalid", **changes)
        self.refuse(replace(value, observed_at=NOW + timedelta(seconds=1)), "coverage")
        self.refuse(change_event(value, occurred_at=NOW + timedelta(seconds=1)))
        self.refuse(change_pull(value, merged_at=NOW), "pull")

    def test_full_timeline_and_source_are_bound_but_collection_time_is_separate(self):
        value = fixture(); result = self.check(value)
        fresh = replace(value, observed_at=NOW + timedelta(seconds=1))
        self.assertEqual(self.check(fresh, now=fresh.observed_at).evidence_sha256, result.evidence_sha256)
        first = value.pulls[0].events[0]
        label = replace(first.label, node_id="LABEL_2", name="documentation")
        other = replace(first, node_id="EVENT_2", label=label, actor_login="someone", actor_node_id="OTHER_USER")
        changed = change_pull(value, events=(first, other))
        self.assertNotEqual(self.check(changed).evidence_sha256, result.evidence_sha256)
        reversed_ = change_pull(changed, events=(other, first))
        self.assertEqual(self.check(changed).evidence_sha256, self.check(reversed_).evidence_sha256)
        self.assertNotEqual(self.check(change_pull(value, head_sha="c" * 40)).evidence_sha256,
                            result.evidence_sha256)


if __name__ == "__main__":
    unittest.main()
