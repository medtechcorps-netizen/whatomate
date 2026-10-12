"""Closed nullable source interpretation; no observation or release authority.

Raw definition preimages are bound to complete immutable source catalogs. The
result only explains the new column and its composite type; residual changes
still belong to the classifier. No DB counts/security observations are invented.
"""
from dataclasses import dataclass
import re

import catalog_semantics as wire
import pending_plan as catalog

SOURCE_PATH = "internal/dbcatalog/profiles/nullable-column-v1/source-shape-v1.json"
CATALOG_PATH = "internal/dbcatalog/golden/catalog.json"
MAX_DOCUMENT = 4 << 20
MAX_TOTAL = 8 << 20
_SHA = re.compile(r"[0-9a-f]{40}\Z")


class NullableSourceRefusal(ValueError):
    def __init__(self):
        super().__init__("nullable-source-invalid")


@dataclass(frozen=True, slots=True)
class SourceNullableDelta:
    base_sha: str
    head_sha: str
    declaration_sha256: str
    before_sha256: str
    after_sha256: str
    operation_sha256: str
    statement_sha256: str
    explained: tuple[catalog.ObjectRef, ...]
    residual: tuple[catalog.ObjectRef, ...]


def _need(value):
    if not value:
        raise NullableSourceRefusal()


def _facts(raw):
    value = wire._fields(wire._load(raw, MAX_DOCUMENT, canonical=False), ("version", "objects"))
    _need(wire._integer(value["version"]))
    facts = catalog._catalog(value, value["version"])
    objects = value["objects"]
    _need([(r["kind"], r["identity"]) for r in objects]
          == sorted((r["kind"], r["identity"]) for r in objects))
    for obj in objects:
        _need("parent" not in obj or obj["parent"] != "")
        for key, text in obj["attributes"].items():
            if catalog._ATTRS[obj["kind"]][key] == "int":
                _need(re.fullmatch(r"0|-?[1-9][0-9]*", text) is not None
                      and -(2**63) <= int(text) < 2**63)
    return value, {f.ref: dict(f.attributes) for f in facts}


def _endpoint(value, raw, role, op):
    wire._fields(value, ("role", "path", "bytes", "sha256", "catalog_version",
        "catalog_sha256", "row_definition_json", "array_definition_json", "slots"))
    document, facts = _facts(raw)
    _need(value["role"] == role and value["path"] == CATALOG_PATH
          and wire._integer(value["bytes"], 1) and value["bytes"] == len(raw)
          and wire._hash(value["sha256"]) and value["sha256"] == wire._digest(raw)
          and wire._integer(value["catalog_version"])
          and value["catalog_version"] == document["version"]
          and wire._hash(value["catalog_sha256"])
          and value["catalog_sha256"] == wire._digest(wire._catalog_bytes(document["objects"])))
    name = op["relation"]
    _need(wire._name("_" + name))
    relation = catalog.ObjectRef("relations", name, "")
    row = catalog.ObjectRef("types", name, "")
    array = catalog.ObjectRef("types", "_" + name, "")
    _need(all(ref in facts for ref in (relation, row, array)))
    attrs = facts[relation]
    _need(attrs["relkind"] == "r"
          and attrs["partition_key_sha256"] == attrs["partition_bound_sha256"] == wire._digest(b"")
          and attrs["parents_sha256"] == wire._digest(b"[]"))
    row_definition = wire._definition(value["row_definition_json"], row=True)
    array_definition = wire._definition(value["array_definition_json"], row=False)
    # Fixed pg_catalog search path renders public type names with their schema.
    _need(row_definition[11] == "-" and array_definition[11] == "public." + name)
    for ref, definition, kind in ((row, "row_definition_json", "c"),
                                  (array, "array_definition_json", "b")):
        _need(facts[ref]["type"] == kind
              and facts[ref]["definition_sha256"] == wire._digest(value[definition].encode("utf-8")))
    slots = value["slots"]
    _need(type(slots) is list and 1 <= len(slots) <= wire.MAX_SLOTS
          and len(row_definition[12]) == len(slots))
    names = set()
    for number, slot in enumerate(slots, 1):
        collation = wire._slot(slot, number)
        _need(slot["name"] not in names)
        names.add(slot["name"])
        ref = catalog.ObjectRef("columns", name + "." + slot["name"], name)
        _need(ref in facts)
        attrs = facts[ref]
        _need(attrs["position"] == str(number)
              and attrs["type_sha256"] == wire._digest(slot["type_text"].encode())
              and attrs["not_null"] == str(slot["not_null"]).lower()
              and (attrs["default_sha256"] != wire._digest(b"")) is slot["default_present"]
              and attrs["identity"] == slot["identity"] and attrs["generated"] == slot["generated"]
              and attrs["acl"] == slot["acl"]
              and attrs["collation_sha256"] == wire._digest(("pg_catalog.default" if collation else "").encode())
              and row_definition[12][number-1] == [number, slot["name"], slot["type_text"], slot["not_null"], collation])
    _need({ref.identity for ref in facts if ref.kind == "columns" and ref.parent == name}
          == {name + "." + slot for slot in names})
    return facts, row_definition


def _budget(declaration, before_catalog, after_catalog):
    # Count every role before ANY source-declaration decoding, even aliases.
    raws = (declaration, before_catalog, after_catalog)
    _need(all(type(raw) is bytes and 0 < len(raw) <= MAX_DOCUMENT for raw in raws)
          and sum(map(len, raws)) <= MAX_TOTAL)


def _classify(declaration, before_catalog, after_catalog, base_sha, head_sha):
    _budget(declaration, before_catalog, after_catalog)
    _need(type(base_sha) is str and _SHA.fullmatch(base_sha) is not None
          and type(head_sha) is str and _SHA.fullmatch(head_sha) is not None and base_sha != head_sha)
    value = wire._fields(wire._load(declaration, MAX_DOCUMENT),
        ("format_version", "postgres_major", "purpose", "operation", "before", "after"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["postgres_major"]) is int and value["postgres_major"] == 17
          and value["purpose"] == "nullable-source-shape")
    operation_raw = wire._json_bytes(value["operation"])
    operation = wire.parse_operation(operation_raw)
    op = wire._operation(operation_raw)
    left, old_type = _endpoint(value["before"], before_catalog, "base", op)
    right, new_type = _endpoint(value["after"], after_catalog, "head", op)
    before, after = value["before"], value["after"]
    name = op["relation"]
    relation = catalog.ObjectRef("relations", name, "")
    row = catalog.ObjectRef("types", name, "")
    array = catalog.ObjectRef("types", "_" + name, "")
    column = catalog.ObjectRef("columns", name + "." + op["column"], name)
    slots = before["slots"]
    _need(len(slots) < wire.MAX_SLOTS and len(after["slots"]) == len(slots) + 1
          and after["slots"][:-1] == slots
          and all(slot["name"] != op["column"] for slot in slots))
    expected = {"number": len(slots)+1, "name": op["column"], "type_namespace": "pg_catalog",
        "type_name": "int8", "type_modifier": -1, "type_text": "bigint", "not_null": False,
        "dropped": False, "local": True, "inherited_count": 0, "default_present": False,
        "identity": "", "generated": "", "collation_namespace": "", "collation_name": "", "acl": ""}
    _need(after["slots"][-1] == expected
          and old_type[:12] == new_type[:12] and old_type[13:] == new_type[13:]
          and new_type[12] == old_type[12] + [[len(slots)+1, op["column"], "bigint", False, ""]]
          and left[relation] == right[relation] and left[array] == right[array]
          and before["array_definition_json"] == after["array_definition_json"]
          and left[row]["definition_sha256"] != right[row]["definition_sha256"]
          and {k: v for k, v in left[row].items() if k != "definition_sha256"}
          == {k: v for k, v in right[row].items() if k != "definition_sha256"})
    _need(column not in left and column in right)
    _need(right[column] == {"position": str(len(slots)+1), "type_sha256": wire._digest(b"bigint"),
        "not_null": "false", "default_sha256": wire._digest(b""), "identity": "", "generated": "",
        "collation_sha256": wire._digest(b""), "acl": ""})
    _need(all(right.get(ref) == attrs for ref, attrs in left.items()
              if ref.kind == "columns" and ref.parent == name))
    explained = (column, row)
    changed = {ref for ref in left.keys() | right.keys() if left.get(ref) != right.get(ref)}
    _need(set(explained) <= changed)
    return SourceNullableDelta(base_sha, head_sha, wire._digest(declaration),
        wire._digest(before_catalog), wire._digest(after_catalog), wire._digest(operation_raw),
        wire._digest(wire.render_statement(operation)), explained, tuple(sorted(changed - set(explained))))


def classify_source(declaration, before_catalog, after_catalog, *, base_sha, head_sha):
    """Revalidate raw source intent; output is not an authenticated capability."""
    try:
        return _classify(declaration, before_catalog, after_catalog, base_sha, head_sha)
    except NullableSourceRefusal:
        raise
    except (wire.SemanticRefusal, catalog.PendingRefusal, ValueError, TypeError,
            KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise NullableSourceRefusal() from None


# A source-classification declaration, not an extension of the no-default
# observation/runtime profile. The exact PG17 deparser template was qualified
# independently for signed int8 boundaries before this rule was implemented.
CONSTANT_SOURCE_PATH = "internal/dbcatalog/source-classification/int8-constant-column-v1.json"
_DECIMAL = re.compile(r"0|-?[1-9][0-9]{0,18}\Z")


class ConstantSourceRefusal(ValueError):
    def __init__(self):
        super().__init__("constant-source-invalid")


def _constant_operation(raw):
    op = wire._fields(wire._load(raw, wire.MAX_OPERATION),
        ("format_version", "id", "kind", "relation", "column", "type", "nullable", "default"))
    _need(type(op["format_version"]) is int and op["format_version"] == 1
          and type(op["id"]) is str and wire._ID.fullmatch(op["id"])
          and op["kind"] == "add_int8_constant_column"
          and wire._name(op["relation"]) and wire._name(op["column"])
          and op["type"] == "pg_catalog.int8" and type(op["nullable"]) is bool)
    default = wire._fields(op["default"], ("kind", "decimal"))
    decimal = default["decimal"]
    _need(default["kind"] == "int8-literal" and type(decimal) is str
          and _DECIMAL.fullmatch(decimal) is not None and -(2**63) <= int(decimal) < 2**63)
    expression = "'" + decimal + "'::bigint"
    statement = ('ALTER TABLE ONLY "public"."' + op["relation"] + '" ADD COLUMN "'
                 + op["column"] + '" pg_catalog.int8 DEFAULT ' + "'" + decimal
                 + "'::pg_catalog.int8" + ("" if op["nullable"] else " NOT NULL") + ";\n").encode("ascii")
    return op, expression, statement


def _classify_constant(declaration, before_catalog, after_catalog, base_sha, head_sha):
    _budget(declaration, before_catalog, after_catalog)
    _need(type(base_sha) is str and _SHA.fullmatch(base_sha) is not None
          and type(head_sha) is str and _SHA.fullmatch(head_sha) is not None and base_sha != head_sha)
    value = wire._fields(wire._load(declaration, MAX_DOCUMENT),
        ("format_version", "postgres_major", "purpose", "operation", "default_expression", "before", "after"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["postgres_major"]) is int and value["postgres_major"] == 17
          and value["purpose"] == "int8-constant-column-source-shape")
    operation_raw = wire._json_bytes(value["operation"])
    op, expression, statement = _constant_operation(operation_raw)
    _need(value["default_expression"] == expression)
    left, old_type = _endpoint(value["before"], before_catalog, "base", op)
    right, new_type = _endpoint(value["after"], after_catalog, "head", op)
    before, after = value["before"], value["after"]
    name = op["relation"]
    relation = catalog.ObjectRef("relations", name, "")
    row = catalog.ObjectRef("types", name, "")
    array = catalog.ObjectRef("types", "_" + name, "")
    column = catalog.ObjectRef("columns", name + "." + op["column"], name)
    slots = before["slots"]
    not_null = not op["nullable"]
    _need(len(slots) < wire.MAX_SLOTS and len(after["slots"]) == len(slots) + 1
          and after["slots"][:-1] == slots and all(slot["name"] != op["column"] for slot in slots))
    expected = {"number": len(slots)+1, "name": op["column"], "type_namespace": "pg_catalog",
        "type_name": "int8", "type_modifier": -1, "type_text": "bigint", "not_null": not_null,
        "dropped": False, "local": True, "inherited_count": 0, "default_present": True,
        "identity": "", "generated": "", "collation_namespace": "", "collation_name": "", "acl": ""}
    _need(after["slots"][-1] == expected
          and old_type[:12] == new_type[:12] and old_type[13:] == new_type[13:]
          and new_type[12] == old_type[12] + [[len(slots)+1, op["column"], "bigint", not_null, ""]]
          and left[relation] == right[relation] and left[array] == right[array]
          and before["array_definition_json"] == after["array_definition_json"]
          and left[row]["definition_sha256"] != right[row]["definition_sha256"]
          and {k: v for k, v in left[row].items() if k != "definition_sha256"}
          == {k: v for k, v in right[row].items() if k != "definition_sha256"})
    _need(column not in left and column in right)
    _need(right[column] == {"position": str(len(slots)+1), "type_sha256": wire._digest(b"bigint"),
        "not_null": str(not_null).lower(), "default_sha256": wire._digest(expression.encode("ascii")),
        "identity": "", "generated": "", "collation_sha256": wire._digest(b""), "acl": ""})
    _need(all(right.get(ref) == attrs for ref, attrs in left.items()
              if ref.kind == "columns" and ref.parent == name))
    explained = (column, row)
    changed = {ref for ref in left.keys() | right.keys() if left.get(ref) != right.get(ref)}
    _need(set(explained) <= changed)
    return SourceNullableDelta(base_sha, head_sha, wire._digest(declaration),
        wire._digest(before_catalog), wire._digest(after_catalog), wire._digest(operation_raw),
        wire._digest(statement), explained, tuple(sorted(changed - set(explained))))


def classify_constant_source(declaration, before_catalog, after_catalog, *, base_sha, head_sha):
    """Explain only the closed typed int8 source delta; never execute a default."""
    try:
        return _classify_constant(declaration, before_catalog, after_catalog, base_sha, head_sha)
    except (NullableSourceRefusal, wire.SemanticRefusal, catalog.PendingRefusal, ValueError,
            TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise ConstantSourceRefusal() from None
