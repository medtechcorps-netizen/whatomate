"""Complete default-ACL observations for the fixed synthetic application profile.

Pure endpoint consistency only: no SQL, source read or release authority.
Observed recognized unexpected ACLs remain diagnostic, but cannot be explained
as the reviewed application contract. V1 operations and renderer are unchanged.
"""
from dataclasses import dataclass

import catalog_semantics as base

SemanticRefusal = base.SemanticRefusal
NullableColumnOperationV1 = base.NullableColumnOperationV1
parse_operation = base.parse_operation
render_statement = base.render_statement
MAX_ROWS = 32
MAX_TUPLES = 1024
_ROLES = ("MIGRATION_OWNER", "RUNTIME")
_GRANTEES = ("MIGRATION_OWNER", "PUBLIC", "RUNTIME")
_PRIVILEGES = {
    "r": ("DELETE", "INSERT", "MAINTAIN", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE"),
    "S": ("SELECT", "UPDATE", "USAGE"),
    "f": ("EXECUTE",), "T": ("USAGE",), "n": ("CREATE", "USAGE"),
}
_CONTRACT_SHA256 = "6f51817c3516988a08b66193fedbb00a375bf8a23459c297593af3f675b37c53"
_CONTRACT_RAW = b'{"default_acl_rows":[{"acl":[{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"SELECT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"USAGE"}],"namespace":"public","object_type":"S","owner":"MIGRATION_OWNER"},{"acl":[{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"DELETE"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"INSERT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"SELECT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"UPDATE"}],"namespace":"public","object_type":"r","owner":"MIGRATION_OWNER"}],"format_version":1,"postgres_major":17,"profile":"tenant-rls-v5-pg17-default-acl-v1"}\n'


@dataclass(frozen=True, slots=True)
class NullableObservationV2:
    raw: bytes


@dataclass(frozen=True, slots=True)
class NullableExplanationV2:
    raw: bytes


def _fixed(function, *args):
    try:
        return function(*args)
    except SemanticRefusal:
        raise
    except (ValueError, TypeError, KeyError, AttributeError, RecursionError, OverflowError, UnicodeError):
        raise SemanticRefusal("v2-format") from None


def _rows(value):
    base._need(type(value) is list and len(value) <= MAX_ROWS, "default-acl-bound")
    row_keys, tuples = [], 0
    for row in value:
        base._fields(row, ("owner", "namespace", "object_type", "acl"))
        kind = row["object_type"]
        base._need(type(row["owner"]) is str and row["owner"] in _ROLES
                   and (row["namespace"] is None or row["namespace"] == "public")
                   and type(kind) is str and kind in _PRIVILEGES
                   and (kind != "n" or row["namespace"] is None), "default-acl-row")
        row_keys.append((row["owner"], row["namespace"] or "", kind))
        acl = row["acl"]
        base._need(type(acl) is list and len(acl) <= MAX_TUPLES, "default-acl-bound")
        tuples += len(acl)
        base._need(tuples <= MAX_TUPLES, "default-acl-bound")
        keys = []
        for entry in acl:
            base._fields(entry, ("grantor", "grantee", "privilege", "grantable"))
            base._need(type(entry["grantor"]) is str and entry["grantor"] in _ROLES
                       and type(entry["grantee"]) is str and entry["grantee"] in _GRANTEES
                       and type(entry["privilege"]) is str and entry["privilege"] in _PRIVILEGES[kind]
                       and type(entry["grantable"]) is bool, "default-acl-entry")
            keys.append((entry["grantor"], entry["grantee"], entry["privilege"]))
        base._need(keys == sorted(set(keys)), "default-acl-order")
    base._need(row_keys == sorted(set(row_keys)), "default-acl-order")
    return value


def _contract():
    value = base._fields(base._load(_CONTRACT_RAW, base.MAX_OPERATION),
                         ("format_version", "profile", "postgres_major", "default_acl_rows"))
    base._need(type(value["format_version"]) is int and value["format_version"] == 1
               and type(value["postgres_major"]) is int and value["postgres_major"] == 17
               and value["profile"] == "tenant-rls-v5-pg17-default-acl-v1"
               and base._digest(_CONTRACT_RAW) == _CONTRACT_SHA256, "default-acl-contract")
    return _rows(value["default_acl_rows"])


def _observation(raw):
    value = base._fields(base._load(raw, base.MAX_OBSERVATION),
                         ("format_version", "postgres_major", "snapshot", "graph"))
    base._need(type(value["format_version"]) is int and value["format_version"] == 2
               and type(value["postgres_major"]) is int and value["postgres_major"] == 17, "version")
    graph = value["graph"]
    base._need(type(graph) is dict and type(graph.get("security")) is dict, "graph")
    rows = _rows(graph["security"]["default_acl_rows"])
    facts = base._snapshot(value["snapshot"], expected_defaults=len(rows))
    definition = base._graph(graph, value["snapshot"], facts, version=2, expected_defaults=len(rows))
    return value, facts, definition


def parse_observation(raw):
    _fixed(_observation, raw)
    return NullableObservationV2(raw)


def canonical_observation(observation):
    base._need(type(observation) is NullableObservationV2)
    _fixed(_observation, observation.raw)
    return observation.raw


def _explain(operation, before, after):
    base._need(type(operation) is NullableColumnOperationV1
               and type(before) is NullableObservationV2 and type(after) is NullableObservationV2)
    op = base._operation(operation.raw)
    old, left, old_type = _observation(before.raw)
    new, right, new_type = _observation(after.raw)
    expected = _contract()
    base._need(old["graph"]["security"]["default_acl_rows"] == expected
               and new["graph"]["security"]["default_acl_rows"] == expected, "default-acl-contract")
    column, row_type = base._nullable_delta(op, old, left, old_type, new, right, new_type)
    value = {"format_version": 2, "operation_sha256": base._digest(operation.raw),
        "statement_sha256": base._digest(render_statement(operation)),
        "before_catalog_sha256": old["snapshot"]["sha256"],
        "after_catalog_sha256": new["snapshot"]["sha256"],
        "before_observation_sha256": base._digest(before.raw),
        "after_observation_sha256": base._digest(after.raw),
        "default_acl_contract_sha256": _CONTRACT_SHA256,
        "explained": [{"kind": ref.kind, "identity": ref.identity, "parent": ref.parent}
                      for ref in (column, row_type)]}
    return NullableExplanationV2(base._json_bytes(value))


def explain_transition(operation, before, after):
    return _fixed(_explain, operation, before, after)


def _explanation(raw):
    value = base._fields(base._load(raw, base.MAX_OPERATION), ("format_version", "operation_sha256",
        "statement_sha256", "before_catalog_sha256", "after_catalog_sha256",
        "before_observation_sha256", "after_observation_sha256", "explained", "default_acl_contract_sha256"))
    base._need(type(value["format_version"]) is int and value["format_version"] == 2
               and all(base._hash(value[k]) for k in value if k not in ("format_version", "explained"))
               and value["default_acl_contract_sha256"] == _CONTRACT_SHA256, "explanation")
    base._need(type(value["explained"]) is list and len(value["explained"]) == 2, "explanation")
    base._reference(value["explained"][0], "columns")
    base._reference(value["explained"][1], "types")
    return raw


def canonical_explanation(explanation):
    base._need(type(explanation) is NullableExplanationV2)
    return _fixed(_explanation, explanation.raw)
