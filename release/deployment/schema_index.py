"""Closed planned index interpretation, not proof of concurrent execution.

Only a new one-key ordinary-int8 btree index is explained. Complete raw source
catalogs and an exact definition preimage bind that intent; all other changed
objects remain residual. No SQL, source transport or runtime observation runs.
"""
from dataclasses import dataclass
import re

import catalog_semantics as wire
import pending_plan as catalog
import schema_nullable as nullable

SOURCE_PATH = "internal/dbcatalog/source-classification/index-concurrent-v1.json"
CATALOG_PATH = nullable.CATALOG_PATH
MAX_DECLARATION = 16 << 10
MAX_OPERATION = 2048
MAX_DEFINITION = 4096
MAX_DOCUMENT = 4 << 20
MAX_TOTAL = 8 << 20
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_ID = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")


class IndexSourceRefusal(ValueError):
    def __init__(self):
        super().__init__("index-source-invalid")


@dataclass(frozen=True, slots=True)
class SourceIndexDelta:
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
        raise IndexSourceRefusal()


def _budget(declaration, before_catalog, after_catalog):
    # The classifier additionally charges every other present declaration.
    # This standalone API still bounds all three raw roles before decoding.
    _need(type(declaration) is bytes and 0 < len(declaration) <= MAX_DECLARATION)
    _need(all(type(raw) is bytes and 0 < len(raw) <= MAX_DOCUMENT
              for raw in (before_catalog, after_catalog)))
    _need(len(declaration) + len(before_catalog) + len(after_catalog) <= MAX_TOTAL)


def _operation(value):
    wire._fields(value, ("format_version", "id", "kind", "schema", "relation",
        "index", "column", "method", "operator_class", "order", "nulls",
        "concurrently", "unique", "include", "predicate"))
    raw = wire._json_bytes(value)
    _need(len(raw) <= MAX_OPERATION
          and type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["id"]) is str and _ID.fullmatch(value["id"]) is not None
          and value["kind"] == "create_nonunique_index_concurrently"
          and value["schema"] == "public"
          and all(wire._name(value[key]) for key in ("relation", "index", "column"))
          and value["method"] == "btree" and value["operator_class"] == "pg_catalog.int8_ops"
          and value["order"] == "asc" and value["nulls"] == "last"
          and value["concurrently"] is True and value["unique"] is False
          and type(value["include"]) is list and value["include"] == []
          and value["predicate"] is None)
    return raw


def _statement(op):
    # No supplied statement or general SQL parser is accepted. This version
    # deliberately has no IF NOT EXISTS, INCLUDE, predicate or option grammar.
    return ('CREATE INDEX CONCURRENTLY "' + op["index"] + '" ON "public"."'
            + op["relation"] + '" USING btree ("' + op["column"]
            + '" pg_catalog.int8_ops ASC NULLS LAST);\n').encode("ascii")


def _endpoint(value, raw, role):
    wire._fields(value, ("role", "path", "bytes", "sha256", "catalog_version", "catalog_sha256"))
    document, facts = nullable._facts(raw)
    _need(value["role"] == role and value["path"] == CATALOG_PATH
          and wire._integer(value["bytes"], 1) and value["bytes"] == len(raw)
          and wire._hash(value["sha256"]) and value["sha256"] == wire._digest(raw)
          and wire._integer(value["catalog_version"])
          and value["catalog_version"] == document["version"]
          and wire._hash(value["catalog_sha256"])
          and value["catalog_sha256"] == wire._digest(wire._catalog_bytes(document["objects"])))
    return facts


def _same(value, expected):
    # Python's 1 == True must not validate PostgreSQL's typed array positions.
    if type(value) is not type(expected):
        return False
    if type(expected) is list:
        return len(value) == len(expected) and all(_same(a, b) for a, b in zip(value, expected))
    return value == expected


def _definition(raw, op):
    _need(wire._text(raw, MAX_DEFINITION))
    value = wire._load(raw.encode("utf-8"), MAX_DEFINITION, canonical=False)
    _need(type(value) is list and len(value) == 8
          and type(value[7]) is list and len(value[7]) == 1
          and type(value[7][0]) is list and len(value[7][0]) == 7)
    key = value[7][0][1]
    _need(type(key) is str and key in (op["column"], '"' + op["column"] + '"'))
    # Exact current indexSemanticsSQL array, with an existing noncollatable
    # int8 key. Raw PG JSON whitespace is retained in the catalog-bound hash.
    expected = ["btree", 1, 1, False, True, False, "",
                [[1, key, "pg_catalog", "int8_ops", "", "", 0]]]
    # The query's key array has seven fields: ordinal, deparse, opclass pair,
    # collation pair and indoption. Reject partial/extra fields explicitly.
    _need(_same(value, expected))


def _class_name(ref, attrs):
    # Standalone composites have pg_class rows even though the relation query
    # excludes relkind c. Scalar and array types do not share that namespace.
    if ref.kind in {"relations", "sequences"} or ref.kind == "types" and attrs["type"] == "c":
        return ref.identity
    if ref.kind == "indexes":
        return ref.identity.removeprefix(ref.parent + ".")
    return None


def _classify(declaration, before_catalog, after_catalog, base_sha, head_sha):
    _budget(declaration, before_catalog, after_catalog)
    _need(type(base_sha) is str and _SHA.fullmatch(base_sha) is not None
          and type(head_sha) is str and _SHA.fullmatch(head_sha) is not None and base_sha != head_sha)
    value = wire._fields(wire._load(declaration, MAX_DECLARATION),
        ("format_version", "postgres_major", "purpose", "operation", "before", "after", "index_definition_json"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["postgres_major"]) is int and value["postgres_major"] == 17
          and value["purpose"] == "concurrent-index-source-shape")
    op = value["operation"]
    operation_raw = _operation(op)
    left = _endpoint(value["before"], before_catalog, "base")
    right = _endpoint(value["after"], after_catalog, "head")
    name = op["relation"]
    relation = catalog.ObjectRef("relations", name, "")
    column = catalog.ObjectRef("columns", name + "." + op["column"], name)
    index = catalog.ObjectRef("indexes", name + "." + op["index"], name)
    row, array = catalog.ObjectRef("types", name, ""), catalog.ObjectRef("types", "_" + name, "")
    _need(all(ref in left and ref in right for ref in (relation, column, row, array)))
    _need(left[relation] == right[relation] and left[column] == right[column])
    attrs = left[relation]
    _need(attrs["relkind"] == "r"
          and attrs["partition_key_sha256"] == attrs["partition_bound_sha256"] == wire._digest(b"")
          and attrs["parents_sha256"] == wire._digest(b"[]"))
    key = left[column]
    _need(wire._integer(int(key["position"]), 1, wire.MAX_SLOTS)
          and key["type_sha256"] == wire._digest(b"bigint")
          and key["collation_sha256"] == wire._digest(b"")
          and key["identity"] == key["generated"] == "")
    _need(left[row]["type"] == right[row]["type"] == "c"
          and left[array]["type"] == right[array]["type"] == "b")
    _need(index not in left and index in right
          and not any(_class_name(ref, attrs) == op["index"] for ref, attrs in left.items())
          and [ref for ref, attrs in right.items() if _class_name(ref, attrs) == op["index"]] == [index])
    _definition(value["index_definition_json"], op)
    _need(right[index] == {"definition_sha256": wire._digest(value["index_definition_json"].encode("utf-8")),
        "valid": "true", "ready": "true", "live": "true", "unique": "false", "primary": "false", "exclusion": "false"})
    changed = {ref for ref in left.keys() | right.keys() if left.get(ref) != right.get(ref)}
    # A same-relation column/row-type change is never silently explained here.
    # The classifier may separately revalidate a disjoint raw column operation.
    return SourceIndexDelta(base_sha, head_sha, wire._digest(declaration), wire._digest(before_catalog),
        wire._digest(after_catalog), wire._digest(operation_raw), wire._digest(_statement(op)),
        (index,), tuple(sorted(changed - {index})))


def classify_source(declaration, before_catalog, after_catalog, *, base_sha, head_sha):
    """Interpret complete raw source intent; never authenticate it or execute SQL."""
    try:
        return _classify(declaration, before_catalog, after_catalog, base_sha, head_sha)
    except IndexSourceRefusal:
        raise
    except (wire.SemanticRefusal, catalog.PendingRefusal, nullable.NullableSourceRefusal,
            ValueError, TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise IndexSourceRefusal() from None
