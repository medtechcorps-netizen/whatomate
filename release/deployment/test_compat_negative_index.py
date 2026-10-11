"""Pure synthetic negative-index grammar; fixtures are not installed setup."""
from dataclasses import FrozenInstanceError, replace
import datetime as dt
import hashlib
import json
import unittest
from unittest import mock

import compat_negative_index as policy

H = "a" * 40
NOW = dt.datetime(2026, 10, 11, 22, 15, 58, 435908, tzinfo=dt.timezone.utc)


def wire(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def rule_dict():
    return {
        "id": 12345, "name": "rereply-compat-negative-v1", "target": "tag",
        "source_type": "Repository", "source": "medtechcorps-netizen/whatomate", "enforcement": "active",
        "conditions": {"ref_name": {"include": ["refs/tags/rereply-compat-negative-v1-*"], "exclude": []}},
        "rules": [{"type": "update", "parameters": {"update_allows_fetch_and_merge": False}}, {"type": "deletion"}],
        "bypass_actors": [], "created_at": "2026-10-12T06:08:39.569+08:00", "updated_at": "2026-10-12T06:08:39.591+08:00",
        "node_id": "SYNTHETIC_RULE_12345", "current_user_can_bypass": "never",
        "_links": {"self": {"href": "https://api.github.com/repos/medtechcorps-netizen/whatomate/rulesets/12345"},
                   "html": {"href": "https://github.com/medtechcorps-netizen/whatomate/rules/12345"}},
    }


def rule_response(*, hidden=False):
    value = rule_dict()
    if hidden:
        del value["bypass_actors"]
    return wire(value)


def witness_dict():
    original = rule_dict()
    normalized = {k: v for k, v in rule_dict().items() if k not in {"node_id", "current_user_can_bypass", "_links"}}
    normalized["rules"].reverse()
    normalized["created_at"] = "2026-10-11T22:08:39.569000Z"
    normalized["updated_at"] = "2026-10-11T22:08:39.591000Z"
    return {
        "format_version": 1, "repository": "medtechcorps-netizen/whatomate", "repository_id": 1297344686,
        "epoch": "rereply-compat-negative-v1", "prefix": "rereply-compat-negative-v1-",
        "complete_original_post_response": original, "complete_privileged_response": rule_dict(), "rule": normalized,
        "observed_at": "2026-10-11T22:15:58.435908+00:00", "old_rule_unchanged": True, "privileged_read": True,
        "original_old_rule_response_sha256": "1"*64, "original_post_response_sha256": digest(wire(original)),
        "original_refused_result_sha256": "2"*64, "request_sha256": "3"*64,
        "qualification": "Synthetic fixture only; no setup, publisher or consumer authorization.",
        "time_check": "Distinct creation and update instants retained; no actual operational claim.",
    }


def setup_dict():
    return {"format_version": 1, "repository": "medtechcorps-netizen/whatomate", "repository_id": 1297344686,
            "epoch": "rereply-compat-negative-v1", "prefix": "rereply-compat-negative-v1-",
            "setup_witness": wire(witness_dict()).decode(), "setup_review_sha256": "4"*64}


def setup_bytes():
    return wire(setup_dict())


def setup():
    return policy.parse_producer_setup(setup_bytes())


def request(**changes):
    return replace(policy.PublicationRequest(H, 123456789, 1, "b"*64, "c"*64, "normal3"), **changes)


def consumer_dict():
    return {"format_version": 1, "producer_setup": setup_bytes().decode(), "lineage": [
        {"producer_sha": "d"*40, "inventory": [
            {"path": ".github/workflows/compat.yml", "mode": "100644", "bytes": 1234, "sha256": "e"*64},
            {"path": "release/compat/negative_index.py", "mode": "100644", "bytes": 4567, "sha256": "f"*64}]}]}


def http(status=200, body=None, *, newline=b"\r\n", headers=None):
    if body is None:
        body = wire({"total_count": 0, "workflow_runs": []}) if status == 200 else wire({"message": "Synthetic service failure"})
    values = [(b"Content-Type", b"application/json; charset=utf-8"), (b"Content-Length", str(len(body)).encode())]
    if headers is not None:
        values = headers
    return newline.join([b"HTTP/2.0 " + str(status).encode() + b" Synthetic", *(k+b": "+v for k, v in values)]) + newline*2 + body


class NegativeIndexTests(unittest.TestCase):
    def refused_setup(self, value):
        with self.assertRaisesRegex(policy.NegativeIndexRefusal, "^negative-index-invalid$"):
            policy.parse_producer_setup(wire(value))

    def witness_mutation(self, mutate):
        value, witness = setup_dict(), witness_dict()
        mutate(witness)
        value["setup_witness"] = wire(witness).decode()
        self.refused_setup(value)

    def test_unconfigured_loaders_have_no_effects_or_fallback(self):
        self.assertIsNone(policy._INSTALLED_PRODUCER_SETUP)
        self.assertIsNone(policy._INSTALLED_CONSUMER_POLICY)
        with mock.patch.object(policy, "_load", side_effect=AssertionError("parse before configured")):
            for loader in (policy.load_producer_setup, policy.load_consumer_policy):
                with self.assertRaisesRegex(policy.NegativeIndexRefusal, "^negative-index-unconfigured$"): loader()

    def test_original_offset_bytes_and_distinct_fractional_instants_preserved(self):
        value = setup()
        self.assertEqual(value.raw, setup_bytes())
        self.assertEqual(value.setup_witness, wire(witness_dict()))
        self.assertIn(b"06:08:39.569+08:00", value.setup_witness)
        self.assertEqual(value.updated_at-value.created_at, dt.timedelta(milliseconds=22))
        self.assertEqual(value.created_at, dt.datetime(2026,10,11,22,8,39,569000,tzinfo=dt.timezone.utc))
        self.assertEqual(value.witness_observed_at, NOW)
        self.assertEqual(value.sha256, digest(setup_bytes()))

    def test_returned_holders_are_immutable_and_have_no_mutable_nested_fields(self):
        value = policy.parse_consumer_policy(wire(consumer_dict()))
        self.assertIs(type(value.lineage), tuple)
        self.assertIs(type(value.lineage[0].inventory), tuple)
        with self.assertRaises(FrozenInstanceError): value.setup.rule_id = 1
        self.assertEqual(value.sha256, digest(value.raw))

    def test_setup_unknown_missing_and_wrong_exact_scalar_types_refuse(self):
        for key in setup_dict():
            value = setup_dict(); del value[key]
            with self.subTest(missing=key): self.refused_setup(value)
        for key, bad in (("extra", 1), ("format_version", True), ("repository_id", True), ("repository", "other/repo"),
                         ("epoch", "bad/epoch"), ("prefix", "prod-"), ("setup_witness", witness_dict()), ("setup_review_sha256", "A"*64)):
            value=setup_dict(); value[key]=bad
            with self.subTest(key=key): self.refused_setup(value)

    def test_witness_canonical_raw_text_cannot_be_reconstructed_or_reformatted(self):
        value=setup_dict(); value["setup_witness"]=json.dumps(witness_dict(), indent=2)+"\n"; self.refused_setup(value)
        value=setup_dict(); value["setup_witness"]=value["setup_witness"].rstrip(); self.refused_setup(value)
        raw=setup_bytes()
        for bad in (raw.rstrip(), b" "+raw, raw.replace(b'"format_version":1', b'"format_version":1,"format_version":1', 1)):
            with self.subTest(raw=bad[:20]), self.assertRaises(policy.NegativeIndexRefusal): policy.parse_producer_setup(bad)

    def test_witness_requires_complete_privileged_observation_and_outer_review(self):
        for key in ("privileged_read", "old_rule_unchanged"):
            self.witness_mutation(lambda w, key=key: w.__setitem__(key, False))
        self.witness_mutation(lambda w: w["complete_privileged_response"].pop("bypass_actors"))
        self.witness_mutation(lambda w: w["complete_original_post_response"].pop("bypass_actors"))
        self.witness_mutation(lambda w: w.__setitem__("setup_review_sha256", "f"*64))

    def test_original_raw_response_hash_and_normalized_rule_disagreement_refuse(self):
        self.witness_mutation(lambda w: w.__setitem__("original_post_response_sha256", "0"*64))
        self.witness_mutation(lambda w: w["rule"].__setitem__("id", 54321))
        self.witness_mutation(lambda w: w["complete_privileged_response"].__setitem__("updated_at", "2026-10-12T06:08:39.592+08:00"))

    def test_rfc3339_naive_precision_leap_unknown_offset_and_invalid_dates_refuse(self):
        for bad in ("2026-10-11T22:15:58", "2026-10-11T22:15:58.1234567Z", "2026-10-11T22:15:60Z",
                    "2026-10-11T22:15:58-00:00", "2026-02-30T22:15:58Z", "2026-10-11 22:15:58Z", "2026-10-11T22:15:58+24:00",
                    "2026-10-11T22:15:58+07:60", "2026-10-11T22:15:58-07:60", "2026-10-11T22:15:58-24:00"):
            with self.subTest(time=bad): self.witness_mutation(lambda w: w.__setitem__("observed_at", bad))

    def test_original_chronology_and_exact_normalized_timestamp_are_enforced(self):
        self.witness_mutation(lambda w: w.__setitem__("observed_at", "2026-10-11T22:08:39.580Z"))
        self.witness_mutation(lambda w: w["rule"].__setitem__("created_at", "2026-10-12T06:08:39.569+08:00"))
        self.witness_mutation(lambda w: w["rule"].__setitem__("created_at", "2026-10-11T22:08:39.592000Z"))

    def test_preparse_byte_depth_node_unicode_and_numeric_bounds(self):
        for raw in (bytearray(setup_bytes()), b"x"*(policy.MAX_CONFIG+1), b"["*17+b"0"+b"]"*17):
            with mock.patch.object(policy.json, "loads", side_effect=AssertionError("parsed unbounded input")):
                with self.assertRaises(policy.NegativeIndexRefusal): policy.parse_producer_setup(raw)
        for raw in (b"\xff", b'"\\ud800"', b'[NaN]', b'[1.0]', b'[1e2]', wire([0]*8193)):
            with self.subTest(raw=raw[:20]), self.assertRaises(policy.NegativeIndexRefusal): policy.replay_refs(raw, producer_sha=H)

    def test_forged_setup_projection_reparsed_instead_of_trusted(self):
        real=setup()
        for bad in (replace(real, rule_id=54321), replace(real, prefix="prod-"), replace(real, setup_witness=b"{}\n"),
                    replace(real, created_at=real.created_at.replace(fold=1)), replace(real, raw=b"{}\n")):
            with self.assertRaises(policy.NegativeIndexRefusal): policy.replay_rule(rule_response(), bad)
        with self.assertRaises(policy.NegativeIndexRefusal): policy.replay_rule(rule_response(), object())

    def test_privileged_empty_and_hidden_runtime_are_distinct(self):
        explicit=policy.replay_rule(rule_response(), setup())
        hidden=policy.replay_rule(rule_response(hidden=True), setup())
        self.assertEqual(explicit.bypass_visibility,"returned-empty")
        self.assertEqual(hidden.bypass_visibility,"hidden-under-setup-trust")
        self.assertNotEqual(explicit.raw,hidden.raw)

    def test_rule_identity_source_times_and_protection_cannot_drift(self):
        mutations = (("id", 999), ("name", "other"), ("target", "branch"), ("source_type", "Organization"),
                     ("source", "other/repo"), ("enforcement", "evaluate"), ("updated_at", "2026-10-12T06:08:39.592+08:00"),
                     ("bypass_actors", [{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"always"}]),
                     ("current_user_can_bypass", "always"), ("unknown", False))
        for key,bad in mutations:
            value=rule_dict(); value[key]=bad
            with self.subTest(key=key), self.assertRaises(policy.NegativeIndexRefusal): policy.replay_rule(wire(value),setup())

    def test_rule_filters_extra_creation_rules_and_bool_int_confusion_refuse(self):
        variants=[]
        v=rule_dict();v["conditions"]["ref_name"]["exclude"]=["refs/tags/ignored"];variants.append(v)
        v=rule_dict();v["conditions"]["ref_name"]["include"].append("refs/tags/prod-*");variants.append(v)
        v=rule_dict();v["rules"].append({"type":"creation"});variants.append(v)
        v=rule_dict();v["rules"][0]["parameters"]["update_allows_fetch_and_merge"]=0;variants.append(v)
        v=rule_dict();v["rules"][1]["parameters"]={};variants.append(v)
        v=rule_dict();v["rules"][1]={"type":"update","parameters":{"update_allows_fetch_and_merge":False}};variants.append(v)
        for value in variants:
            with self.subTest(value=value), self.assertRaises(policy.NegativeIndexRefusal):policy.replay_rule(wire(value),setup())

    def test_rule_links_are_checked_but_never_followed(self):
        for bad in ("https://example.com/secret", "https://github.com/other/repo/rules/12345"):
            value=rule_dict();value["_links"]["html"]["href"]=bad
            with self.assertRaises(policy.NegativeIndexRefusal):policy.replay_rule(wire(value),setup())

    def test_consumer_complete_sorted_inventory_is_data_not_coverage(self):
        value=policy.parse_consumer_policy(wire(consumer_dict()))
        self.assertEqual(value.lineage[0].producer_sha,"d"*40)
        self.assertEqual(value.lineage[0].inventory[0].path,".github/workflows/compat.yml")
        self.assertFalse(hasattr(value,"covered"));self.assertFalse(hasattr(value,"allow_outage"))

    def test_consumer_duplicates_unsorted_and_empty_lineage_refuse(self):
        variants=[]
        v=consumer_dict();v["lineage"]=[];variants.append(v)
        v=consumer_dict();v["lineage"]*=2;variants.append(v)
        v=consumer_dict();v["lineage"][0]["inventory"].reverse();variants.append(v)
        v=consumer_dict();v["lineage"][0]["inventory"]*=2;variants.append(v)
        for value in variants:
            with self.assertRaises(policy.NegativeIndexRefusal):policy.parse_consumer_policy(wire(value))

    def test_consumer_source_paths_modes_and_exact_types_are_closed(self):
        for key,bad in (("path","../outside"),("path","/absolute"),("path","C:/outside"),("path","x\\y"),
                        ("path","x/*"),("path","x/./y"),("mode","120000"),("bytes",True),("sha256","A"*64)):
            value=consumer_dict();value["lineage"][0]["inventory"][0][key]=bad
            with self.subTest(key=key,bad=bad),self.assertRaises(policy.NegativeIndexRefusal):policy.parse_consumer_policy(wire(value))

    def test_inventory_caps_charge_repeated_source_across_lineages(self):
        value=consumer_dict()
        for row in value["lineage"][0]["inventory"]:row["bytes"]=4<<20
        policy.parse_consumer_policy(wire(value))
        value["lineage"].append({"producer_sha":"e"*40,"inventory":[dict(value["lineage"][0]["inventory"][0])]})
        with self.assertRaises(policy.NegativeIndexRefusal):policy.parse_consumer_policy(wire(value))
        value=consumer_dict();value["lineage"][0]["inventory"][0]["bytes"]=(4<<20)+1
        with self.assertRaises(policy.NegativeIndexRefusal):policy.parse_consumer_policy(wire(value))

    def test_marker_uses_producer_H_and_exact_two_subject_hashes(self):
        expected="refs/tags/rereply-compat-negative-v1-"+H+"-123456789-1-"+"b"*64+"-"+"c"*64
        self.assertEqual(policy.marker_ref(request()),expected)
        self.assertEqual(policy.marker_ref(request(protocol="rollback4")),expected)
        self.assertNotEqual(policy.marker_ref(request(producer_sha="f"*40)),expected)
        self.assertEqual(policy.marker_ref(request(run_id=10**16-1,run_attempt=9999)).split("-")[-3],"9999")

    def test_request_closed_type_bounds_protocol_and_booleans_refuse(self):
        for key,bad in (("producer_sha","A"*40),("run_id",True),("run_id",10**16),("run_id",0),("run_attempt",10000),
                        ("run_attempt","01"),("context_sha256","a"*40),("result_sha256","sha256:"+"b"*64),("protocol","normal2")):
            with self.subTest(key=key),self.assertRaises(policy.NegativeIndexRefusal):policy.marker_ref(request(**{key:bad}))
        with self.assertRaises(policy.NegativeIndexRefusal):policy.validate_publication_request(object.__new__(policy.PublicationRequest))

    def test_all_matching_rows_are_retained_including_malformed_or_wrong_object(self):
        rows=[{"ref": "refs/tags/not-a-marker","object":{"type":"tag","sha":"f"*40}}, {}, None, "broken"]
        value=policy.replay_refs(wire(rows),producer_sha=H)
        self.assertEqual(value.rows,tuple(wire(r) for r in rows))
        self.assertEqual(value.raw,wire(rows))
        self.assertEqual(policy.replay_refs(b"[]\n",producer_sha=H).rows,())

    def test_ref_bounds_malformed_envelope_and_no_partial_sha_refuse(self):
        for raw in (b"{}\n", b"[", wire([{}]*257),b"x"*(policy.MAX_BODY+1)):
            with self.assertRaises(policy.NegativeIndexRefusal):policy.replay_refs(raw,producer_sha=H)
        for sha in (H[:8],H+"a",H.upper(),None):
            with self.assertRaises(policy.NegativeIndexRefusal):policy.replay_refs(b"[]",producer_sha=sha)

    def test_http_complete_available_and_three_unavailable_statuses_are_distinct(self):
        for status in (200,502,503,504):
            raw=http(status)
            value=policy.replay_http(raw,endpoint=policy.ACTIONS_ENDPOINT)
            self.assertEqual(value.raw,raw);self.assertEqual(value.status,status)
            self.assertEqual(value.state,"available" if status==200 else "unavailable")
        self.assertEqual(policy.replay_http(http(newline=b"\n"),endpoint=policy.ACTIONS_ENDPOINT).state,"available")

    def test_http_auth_rate_redirect_unknown_or_wrong_endpoint_never_outage(self):
        for status in (201,301,302,400,401,403,404,429,500):
            with self.subTest(status=status),self.assertRaises(policy.NegativeIndexRefusal):policy.replay_http(http(status),endpoint=policy.ACTIONS_ENDPOINT)
        with self.assertRaises(policy.NegativeIndexRefusal):policy.replay_http(http(503),endpoint=policy.API+"/other")

    def test_http_partial_length_multiple_blocks_folded_and_duplicate_headers_refuse(self):
        variants=[http()[:-1],http()+b"extra",b"HTTP/2.0 503 Synthetic\r\n\r\n"+http(),
                  http(503,body=b"\r\n"+http()),http(503,body=b"first body\n\n"+http()),
                  http(503,body=b"first body\r\n \t"+http()),
                  http(headers=[(b"Content-Type",b"application/json"),(b"Content-Type",b"application/json")]),
                  http(headers=[(b" Location",b"/elsewhere")]),http(headers=[(b"Location",b"https://elsewhere")]),
                  http(headers=[(b"Transfer-Encoding",b"chunked")]),b"HTTP/2.0 503"+b"x"*(policy.MAX_HEADERS+policy.MAX_BODY)]
        for raw in variants:
            with self.subTest(raw=raw[:60]),self.assertRaises(policy.NegativeIndexRefusal):policy.replay_http(raw,endpoint=policy.ACTIONS_ENDPOINT)

    def test_http_malformed_success_payload_does_not_synthesize_unavailability(self):
        for body in (b"not-json",wire({"message":"unknown"}),wire({"total_count":True,"workflow_runs":[]}),
                     wire({"total_count":1,"workflow_runs":[{},{}]}),wire({"total_count":0,"workflow_runs":[{}]})):
            with self.assertRaises(policy.NegativeIndexRefusal):policy.replay_http(http(body=body),endpoint=policy.ACTIONS_ENDPOINT)

    def test_snapshot_retains_all_raw_observations_and_original_fractional_times(self):
        value=policy.replay_index(setup(),rule_response(),b"[]\n",rule_response(),b"[]\n",producer_sha=H,started_at=NOW-dt.timedelta(microseconds=1),observed_at=NOW)
        self.assertEqual(value.started_at.microsecond,435907)
        self.assertEqual(value.before_rule.raw,value.after_rule.raw)
        self.assertEqual(value.before_refs.rows,())
        self.assertFalse(hasattr(value,"covered"));self.assertFalse(hasattr(value,"fresh"))
        rows=wire([{}])
        value=policy.replay_index(setup(),rule_response(),rows,rule_response(),rows,producer_sha=H,started_at=NOW,observed_at=NOW)
        self.assertEqual(len(value.after_refs.rows),1)

    def test_snapshot_changed_rule_visibility_or_refs_refuses_instead_of_recovery(self):
        for after_rule,after_refs in ((rule_response(hidden=True),b"[]\n"),(rule_response(),wire([{}]))):
            with self.assertRaisesRegex(policy.NegativeIndexRefusal,"^negative-index-changed$"):
                policy.replay_index(setup(),rule_response(),b"[]\n",after_rule,after_refs,producer_sha=H,started_at=NOW,observed_at=NOW)

    def test_snapshot_naive_fold_offset_and_backwards_clocks_refuse(self):
        for start,end in ((NOW,NOW-dt.timedelta(microseconds=1)),(NOW.replace(tzinfo=None),NOW),
                          (NOW.replace(fold=1),NOW),(NOW,NOW.astimezone(dt.timezone(dt.timedelta(hours=8))))):
            with self.assertRaises(policy.NegativeIndexRefusal):
                policy.replay_index(setup(),rule_response(),b"[]\n",rule_response(),b"[]\n",producer_sha=H,started_at=start,observed_at=end)

    def test_error_messages_are_fixed_and_do_not_echo_response_or_caller_text(self):
        self.assertEqual(str(policy.NegativeIndexRefusal("private token should not escape")),"negative-index-invalid")
        with self.assertRaisesRegex(policy.NegativeIndexRefusal,"^negative-index-invalid$"):
            policy.replay_rule(b'{"secret":"private-response"}',setup())


if __name__ == "__main__":
    unittest.main()
