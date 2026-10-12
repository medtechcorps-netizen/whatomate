"""Closed nullable-column observations; consistency data, never release authority.

This pure checker consumes complete observations from the matching Go observer.
It performs no I/O, executes no SQL, and neither classifies nor admits a release.
The current lane requires zero default ACLs; normal application bootstrap is
therefore deliberately unsupported until its full ACL graph is versioned.
"""
from dataclasses import dataclass
import hashlib
import json
import re

import pending_plan as catalog_format

MAX_OPERATION = 2048
MAX_OBSERVATION = 4 << 20
MAX_GRAPH = 1 << 20
MAX_DEFINITION = 256 << 10
MAX_NODES = 500000
MAX_DEPTH = 24
MAX_SLOTS = 1024
_NAME = re.compile(r"[a-z_][a-z0-9_]{0,62}\Z")
_ID = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_COUNTS = frozenset(("non_public_schemas", "non_public_objects", "other_roles",
    "default_acls", "oids", "reloptions", "statistics", "tablespaces",
    "other_acl_entries", "other_policy_roles"))
_SLOT_KEYS = frozenset(("number", "name", "type_namespace", "type_name",
    "type_modifier", "type_text", "not_null", "dropped", "local",
    "inherited_count", "default_present", "identity", "generated",
    "collation_namespace", "collation_name", "acl"))


class SemanticRefusal(ValueError):
    def __init__(self, code):
        self.code = "nullable-semantics:" + code
        super().__init__(self.code)


def _need(ok, code="format"):
    if not ok:
        raise SemanticRefusal(code)


def _digest(raw):
    return hashlib.sha256(raw).hexdigest()


def _hash(value):
    return type(value) is str and _HASH.fullmatch(value) is not None


def _name(value):
    return type(value) is str and _NAME.fullmatch(value) is not None


def _integer(value, low=0, high=2**63-1):
    return type(value) is int and low <= value <= high


def _fields(value, keys):
    _need(type(value) is dict and set(value) == set(keys))
    return value


def _json_bytes(value, *, sorted_keys=True):
    # encoding/json's escaping is normative for the new portable wire format.
    try:
        text = json.dumps(value, ensure_ascii=False, sort_keys=sorted_keys,
                          separators=(",", ":"), allow_nan=False)
        for char, escaped in (("<", "\\u003c"), (">", "\\u003e"),
                              ("&", "\\u0026"), ("\u2028", "\\u2028"),
                              ("\u2029", "\\u2029")):
            text = text.replace(char, escaped)
        return (text + "\n").encode("utf-8")
    except (TypeError, ValueError, UnicodeError, RecursionError):
        raise SemanticRefusal("json") from None


def _pairs(rows):
    result = {}
    for key, value in rows:
        _need(key not in result, "duplicate-key")
        result[key] = value
    return result


def _noninteger(_):
    raise SemanticRefusal("json-number")


def _number(text):
    _need(re.fullmatch(r"0|-?[1-9][0-9]*", text) is not None, "json-number")
    value = int(text)
    _need(-(2**63) <= value < 2**63, "json-number")
    return value


def _text(value, limit=1024, *, empty=False):
    return (type(value) is str and (empty or bool(value))
            and len(value.encode("utf-8")) <= limit
            and not any(ord(c) < 32 or ord(c) == 127 or c == "\ufffd" for c in value))


def _load(raw, limit, *, canonical=True):
    _need(type(raw) is bytes and 0 < len(raw) <= limit, "bound")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_pairs,
                           parse_int=_number, parse_float=_noninteger, parse_constant=_noninteger)
        stack, count = [(value, 0)], 0
        while stack:
            row, depth = stack.pop()
            count += 1
            _need(count <= MAX_NODES and depth <= MAX_DEPTH, "json-bound")
            if type(row) is dict:
                _need(all(type(k) is str and len(k.encode("utf-8")) <= 128
                          and "\ufffd" not in k for k in row), "json-bound")
                stack.extend((v, depth+1) for v in row.values())
            elif type(row) is list:
                stack.extend((v, depth+1) for v in row)
            else:
                _need(row is None or type(row) in (str, int, bool), "json-type")
                if type(row) is str:
                    _need(len(row.encode("utf-8")) <= MAX_DEFINITION and "\ufffd" not in row, "json-bound")
                if type(row) is int:
                    _need(-(2**63) <= row < 2**63, "json-bound")
        if canonical:
            _need(_json_bytes(value) == raw, "noncanonical")
        return value
    except (UnicodeError, ValueError, TypeError, RecursionError) as exc:
        if type(exc) is SemanticRefusal:
            raise
        raise SemanticRefusal("json") from None


@dataclass(frozen=True, slots=True)
class NullableColumnOperationV1:
    raw: bytes


@dataclass(frozen=True, slots=True)
class NullableObservationV1:
    raw: bytes


@dataclass(frozen=True, slots=True)
class NullableExplanationV1:
    raw: bytes


def _operation(raw):
    value = _fields(_load(raw, MAX_OPERATION), ("format_version", "id", "kind",
        "relation", "column", "type", "nullable", "default"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["id"]) is str and _ID.fullmatch(value["id"])
          and value["kind"] == "add_nullable_column"
          and _name(value["relation"]) and _name(value["column"])
          and value["type"] == "pg_catalog.int8" and value["nullable"] is True
          and value["default"] is None, "operation")
    return value


def parse_operation(raw):
    _operation(raw)
    return NullableColumnOperationV1(raw)


def render_statement(operation):
    _need(type(operation) is NullableColumnOperationV1, "operation")
    value = _operation(operation.raw)
    return ('ALTER TABLE ONLY "public"."' + value["relation"]
            + '" ADD COLUMN "' + value["column"] + '" pg_catalog.int8;\n').encode("ascii")


def _reference(value, kind=None):
    try:
        result = catalog_format._reference(value)
    except (catalog_format.PendingRefusal, KeyError, TypeError):
        raise SemanticRefusal("reference") from None
    _need(kind is None or result.kind == kind, "reference")
    return result


def _catalog_bytes(objects):
    # Reproduce CanonicalCatalog's struct order, sorted attribute map, omitted
    # empty parent, and final LF. This is NOT the sorted observation wrapper.
    rows = []
    for row in objects:
        entry = {"kind": row["kind"], "identity": row["identity"]}
        if row.get("parent", ""):
            entry["parent"] = row["parent"]
        entry["attributes"] = dict(sorted(row["attributes"].items()))
        rows.append(entry)
    return _json_bytes({"version": 0, "objects": rows}, sorted_keys=False)


def _snapshot(value, *, expected_defaults=0):
    _fields(value, ("version", "sha256", "objects", "unknown", "count_only", "ownership"))
    _need(type(value["version"]) is int and value["version"] == 0
          and type(value["unknown"]) is list and value["unknown"] == []
          and _hash(value["sha256"]), "snapshot")
    try:
        facts = catalog_format._catalog({"version": 0, "objects": value["objects"]}, 0)
    except (catalog_format.PendingRefusal, KeyError, TypeError):
        raise SemanticRefusal("catalog") from None
    objects = value["objects"]
    _need([(o["kind"], o["identity"]) for o in objects]
          == sorted((o["kind"], o["identity"]) for o in objects), "catalog-order")
    for obj in objects:
        _need("parent" not in obj or obj["parent"] != "", "catalog-canonical")
        attrs = obj["attributes"]
        _need(all(catalog_format._ATTRS[obj["kind"]][key] != "int"
                  or re.fullmatch(r"0|-?[1-9][0-9]*", text) is not None
                  for key, text in attrs.items()), "catalog-integer")
        _need(attrs.get("owner") != "OTHER" and "OTHER" not in attrs.get("acl", "")
              and "OTHER" not in attrs.get("roles", ""), "principal")
    _need(_digest(_catalog_bytes(objects)) == value["sha256"], "catalog-digest")
    _fields(value["count_only"], _COUNTS)
    _need(all(_integer(n) for n in value["count_only"].values()), "counts")
    _need(_integer(expected_defaults, 0, 32)
          and value["count_only"]["default_acls"] == expected_defaults
          and all(value["count_only"][k] == 0 for k in
                  ("other_acl_entries", "other_policy_roles")), "security")
    _fields(value["ownership"], ("migration_owner_owns_database", "migration_owner_owns_public_schema"))
    _need(all(v is True for v in value["ownership"].values()), "ownership")
    return {f.ref: dict(f.attributes) for f in facts}


def _definition(raw, *, row):
    _need(type(raw) is str, "definition")
    value = _load(raw.encode("utf-8"), MAX_DEFINITION, canonical=False)
    _need(type(value) is list and len(value) == 14, "definition")
    _need(all(_text(value[i], empty=True)
              for i in (0, 1, 3, 4, 7, 8, 9, 10, 11))
          and type(value[2]) is bool and _integer(value[5], -32768, 32767)
          and type(value[6]) is bool and type(value[12]) is list
          and type(value[13]) is list and value[13] == [], "definition")
    _need(value[0] == ("c" if row else "b")
          and value[1] == ("C" if row else "A")
          and value[2] is False and value[4] == "", "definition")
    _need(len(value[12]) <= MAX_SLOTS and (row or value[12] == []), "definition")
    for field in value[12]:
        _need(type(field) is list and len(field) == 5 and _integer(field[0], 1, MAX_SLOTS)
              and _name(field[1]) and _text(field[2])
              and type(field[3]) is bool and _text(field[4], 128, empty=True), "definition-field")
    return value


def _acl(value):
    _need(catalog_format._attribute("acl", value) and "OTHER" not in value, "acl")


def _slot(value, number):
    _fields(value, _SLOT_KEYS)
    _need(type(value["number"]) is int and value["number"] == number
          and _name(value["name"]) and value["type_namespace"] in ("public", "pg_catalog")
          and _name(value["type_name"]) and _integer(value["type_modifier"], -1, 2**31-1)
          and _text(value["type_text"])
          and type(value["not_null"]) is bool and value["dropped"] is False
          and value["local"] is True and type(value["inherited_count"]) is int
          and value["inherited_count"] == 0 and type(value["default_present"]) is bool
          and value["identity"] in ("", "a", "d") and value["generated"] in ("", "s", "v"), "slot")
    _acl(value["acl"])
    pair = (value["collation_namespace"], value["collation_name"])
    _need(pair in (("", ""), ("pg_catalog", "default")), "collation")
    return "" if pair == ("", "") else '"default"'


def _graph(graph, snapshot, facts, *, version=1, expected_defaults=0):
    _need(type(version) is int and version in (1, 2), "version")
    _fields(graph, ("format_version", "catalog_sha256", "relation", "row_type",
                    "array_type", "slots", "security"))
    _need(len(_json_bytes(graph)) <= MAX_GRAPH and type(graph["format_version"]) is int
          and graph["format_version"] == version and graph["catalog_sha256"] == snapshot["sha256"], "graph")
    relation = _fields(graph["relation"], ("object", "relkind", "persistence", "is_partition",
                    "of_type", "parents", "children", "row_type"))
    relation_ref = _reference(relation["object"], "relations")
    _need(relation_ref in facts and relation["relkind"] == "r" and relation["persistence"] == "p"
          and relation["is_partition"] is False and relation["of_type"] is None
          and type(relation["parents"]) is list and relation["parents"] == []
          and type(relation["children"]) is list and relation["children"] == []
          and facts[relation_ref]["relkind"] == "r", "relation")
    row = _fields(graph["row_type"], ("object", "relation", "array", "definition_json"))
    array = _fields(graph["array_type"], ("object", "element", "definition_json"))
    row_ref, array_ref = _reference(row["object"], "types"), _reference(array["object"], "types")
    _need(row_ref != array_ref and row_ref in facts and array_ref in facts
          and _reference(relation["row_type"], "types") == row_ref
          and _reference(row["relation"], "relations") == relation_ref
          and _reference(row["array"], "types") == array_ref
          and _reference(array["element"], "types") == row_ref, "dependency")
    row_definition = _definition(row["definition_json"], row=True)
    _definition(array["definition_json"], row=False)
    for entry, ref, kind in ((row, row_ref, "c"), (array, array_ref, "b")):
        _need(facts[ref]["type"] == kind
              and _digest(entry["definition_json"].encode("utf-8")) == facts[ref]["definition_sha256"], "definition-digest")
    slots = graph["slots"]
    _need(type(slots) is list and 1 <= len(slots) <= MAX_SLOTS
          and len(row_definition[12]) == len(slots), "slots")
    names = set()
    for number, slot in enumerate(slots, 1):
        collation = _slot(slot, number)
        _need(slot["name"] not in names, "slot-duplicate")
        names.add(slot["name"])
        ref = catalog_format.ObjectRef("columns", relation_ref.identity + "." + slot["name"], relation_ref.identity)
        _need(ref in facts, "slot-column")
        attrs = facts[ref]
        _need(attrs["position"] == str(number)
              and attrs["type_sha256"] == _digest(slot["type_text"].encode("utf-8"))
              and attrs["not_null"] == str(slot["not_null"]).lower()
              and (attrs["default_sha256"] != _digest(b"")) is slot["default_present"]
              and attrs["identity"] == slot["identity"] and attrs["generated"] == slot["generated"]
              and attrs["acl"] == slot["acl"]
              and attrs["collation_sha256"] == _digest(("pg_catalog.default" if collation else "").encode("utf-8"))
              and row_definition[12][number-1] == [number, slot["name"], slot["type_text"], slot["not_null"], collation], "slot-binding")
    _need({ref.identity for ref in facts if ref.kind == "columns" and ref.parent == relation_ref.identity}
          == {relation_ref.identity + "." + name for name in names}, "slot-inventory")
    security_keys = ("database_acl_sha256", "public_schema_acl_sha256",
                     "default_acl_count", "other_acl_entries", "other_policy_roles")
    if version == 2:
        security_keys += ("default_acl_rows",)
    security = _fields(graph["security"], security_keys)
    _need(all(_hash(security[k]) for k in ("database_acl_sha256", "public_schema_acl_sha256")), "security")
    for key, count in (("default_acl_count", "default_acls"), ("other_acl_entries", "other_acl_entries"),
                       ("other_policy_roles", "other_policy_roles")):
        wanted = expected_defaults if key == "default_acl_count" else 0
        _need(_integer(wanted, 0, 32) and type(security[key]) is int
              and security[key] == snapshot["count_only"][count] == wanted, "security")
    return row_definition


def _observation(raw):
    value = _fields(_load(raw, MAX_OBSERVATION), ("format_version", "postgres_major", "snapshot", "graph"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["postgres_major"]) is int and value["postgres_major"] == 17, "version")
    facts = _snapshot(value["snapshot"])
    definition = _graph(value["graph"], value["snapshot"], facts)
    return value, facts, definition


def parse_observation(raw):
    _observation(raw)
    return NullableObservationV1(raw)


def canonical_observation(observation):
    _need(type(observation) is NullableObservationV1)
    _observation(observation.raw)
    return observation.raw


def _nullable_delta(op, old, left, old_type, new, right, new_type):
    a, b = old["graph"], new["graph"]
    _need(a["relation"] == b["relation"] and a["relation"]["object"]["identity"] == op["relation"]
          and a["security"] == b["security"] and a["array_type"] == b["array_type"]
          and old["snapshot"]["count_only"] == new["snapshot"]["count_only"]
          and old["snapshot"]["ownership"] == new["snapshot"]["ownership"], "unexplained-change")
    _need(all(a["row_type"][key] == b["row_type"][key] for key in ("object", "relation", "array")), "dependency")
    slots = a["slots"]
    _need(1 <= len(slots) < MAX_SLOTS and len(b["slots"]) == len(slots)+1
          and b["slots"][:-1] == slots and all(s["name"] != op["column"] for s in slots), "append")
    expected = {"number": len(slots)+1, "name": op["column"], "type_namespace": "pg_catalog",
        "type_name": "int8", "type_modifier": -1, "type_text": "bigint", "not_null": False,
        "dropped": False, "local": True, "inherited_count": 0, "default_present": False,
        "identity": "", "generated": "", "collation_namespace": "", "collation_name": "", "acl": ""}
    _need(b["slots"][-1] == expected, "new-column")
    _need(old_type[:12] == new_type[:12] and old_type[13:] == new_type[13:]
          and new_type[12] == old_type[12] + [[len(slots)+1, op["column"], "bigint", False, ""]], "row-type")
    column = catalog_format.ObjectRef("columns", op["relation"] + "." + op["column"], op["relation"])
    row_type = _reference(a["row_type"]["object"], "types")
    _need(column not in left and set(right) == set(left) | {column}, "object-inventory")
    expected_attrs = {"position": str(len(slots)+1), "type_sha256": _digest(b"bigint"),
        "not_null": "false", "default_sha256": _digest(b""), "identity": "", "generated": "",
        "collation_sha256": _digest(b""), "acl": ""}
    _need(right[column] == expected_attrs, "new-column")
    for ref, attrs in left.items():
        if ref == row_type:
            _need(attrs["definition_sha256"] != right[ref]["definition_sha256"]
                  and {k: v for k, v in attrs.items() if k != "definition_sha256"}
                  == {k: v for k, v in right[ref].items() if k != "definition_sha256"}, "row-type")
        else:
            _need(attrs == right[ref], "unexplained-change")
    return column, row_type


def explain_transition(operation, before, after):
    _need(type(operation) is NullableColumnOperationV1
          and type(before) is NullableObservationV1 and type(after) is NullableObservationV1)
    op = _operation(operation.raw)
    old, left, old_type = _observation(before.raw)
    new, right, new_type = _observation(after.raw)
    column, row_type = _nullable_delta(op, old, left, old_type, new, right, new_type)
    value = {"format_version": 1, "operation_sha256": _digest(operation.raw),
        "statement_sha256": _digest(render_statement(operation)),
        "before_catalog_sha256": old["snapshot"]["sha256"],
        "after_catalog_sha256": new["snapshot"]["sha256"],
        "before_observation_sha256": _digest(before.raw), "after_observation_sha256": _digest(after.raw),
        "explained": [{"kind": ref.kind, "identity": ref.identity, "parent": ref.parent}
                      for ref in (column, row_type)]}
    return NullableExplanationV1(_json_bytes(value))


def canonical_explanation(explanation):
    _need(type(explanation) is NullableExplanationV1)
    value = _fields(_load(explanation.raw, MAX_OPERATION), ("format_version", "operation_sha256",
        "statement_sha256", "before_catalog_sha256", "after_catalog_sha256",
        "before_observation_sha256", "after_observation_sha256", "explained"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and all(_hash(value[k]) for k in value if k not in ("format_version", "explained")))
    _need(type(value["explained"]) is list and len(value["explained"]) == 2)
    _reference(value["explained"][0], "columns")
    _reference(value["explained"][1], "types")
    return explanation.raw
