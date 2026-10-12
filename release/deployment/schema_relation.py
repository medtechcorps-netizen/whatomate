"""Closed new-relation source templates; no Git, SQL or runtime authority.

Only a complete new global/direct-tenant creation bundle is explained. The
statement hash covers CREATE TABLE and tenant ENABLE/FORCE, not an executable
migration. Fixed policy-template intent and every policy/ACL row are separately
bound. All other changed catalog refs remain residual.
"""
from dataclasses import dataclass
import json
import re

import catalog_semantics as wire
import pending_plan as catalog
import schema_nullable as nullable

SOURCE_PATH = "internal/dbcatalog/source-classification/relation-v1.json"
CATALOG_PATH = nullable.CATALOG_PATH
GLOBALS_PATH = "internal/dbcatalog/golden/global_tables.json"
TENANT_PATH = "internal/database/tenant.go"
MAX_DECLARATION = 64 << 10
MAX_DOCUMENT = 4 << 20
MAX_GLOBALS = 256 << 10
MAX_TENANT_SOURCE = 512 << 10
MAX_TOTAL = 8 << 20
MAX_PROFILES = 64
POLICY_TEMPLATE = "direct-tenant-v5-pg17-v1"
RUNTIME_PREDICATE = "(organization_id = (NULLIF(current_setting('app.current_organization_id'::text, true), ''::text))::uuid)"
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_NAME = re.compile(r"[a-z_][a-z0-9_]{0,61}\Z")
_ID = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
_RESERVED = r"DirectTenantTables|RelatedTenantTables|DirectTenantTableExemptions|tenantRLSAdditiveProfileV[0-9]+"
_CONSTANTS = ("tenantRLSRoutingVersion", "tenantRLSAdditiveProfileVersion",
              "tenantPolicyFingerprintSignature", "tenantAdditivePolicyFingerprintSignature")
_LITERAL = re.compile(r'//[^\n]*|/\*[\s\S]*?\*/|`[^`]*`|"(?:\\[\s\S]|[^"\\])*"|\'(?:\\[\s\S]|[^\'\\])*\'')


class RelationSourceRefusal(ValueError):
    def __init__(self):
        super().__init__("relation-source-invalid")


@dataclass(frozen=True, slots=True)
class RelationColumn:
    name: str
    type: str
    nullable: bool
    default: None


@dataclass(frozen=True, slots=True)
class RelationOperation:
    id: str
    mode: str
    schema: str
    relation: str
    columns: tuple[RelationColumn, ...]


@dataclass(frozen=True, slots=True)
class SourceRelationDelta:
    base_sha: str
    head_sha: str
    declaration_sha256: str
    before_catalog_sha256: str
    after_catalog_sha256: str
    before_globals_sha256: str
    after_globals_sha256: str
    before_tenant_source_sha256: str
    after_tenant_source_sha256: str
    operation: RelationOperation
    operation_sha256: str
    statement_sha256: str
    explained: tuple[catalog.ObjectRef, ...]
    residual: tuple[catalog.ObjectRef, ...]


@dataclass(frozen=True, slots=True)
class TenantInventory:
    direct: tuple[str, ...]
    related: tuple[tuple[str, str], ...]
    exemptions: tuple[tuple[str, str], ...]
    v1: tuple[str, ...]
    profiles: tuple[tuple[int, tuple[str, ...]], ...]
    declarations: tuple[tuple[str, bytes], ...]
    constants: tuple[tuple[str, bytes], ...]


def _need(value):
    if not value:
        raise RelationSourceRefusal()


def _facts(raw):
    return nullable._facts(raw)


def _mask(source):
    return _LITERAL.sub(lambda m: re.sub(r"[^\n]", " ", m.group()), source)


def _builtin_len_calls(code):
    # This source grammar admits only the existing len(identifier) expression
    # form. A name in a declaration, signature or selector cannot establish
    # that a reserved slice is merely being read. Comments/literals are already
    # masked, so whitespace cannot disguise a qualified call or declaration.
    tokens = list(re.finditer(r"[A-Za-z_][A-Za-z0-9_]*|[^\s]", code))
    cursor = 0
    while cursor < len(tokens):
        token = tokens[cursor]
        cursor += 1
        if token.group() != "func":
            continue
        stack, previous = [], token
        while cursor < len(tokens):
            current = tokens[cursor]
            value = current.group()
            # A body starts only outside the complete parameter/result types.
            # Anonymous struct/interface braces in a header are type bodies.
            if not stack and (value == ";" or "\n" in code[previous.end():current.start()]):
                _need(previous.group() != "func")
                break
            if value == "{" and not stack and previous.group() not in {"struct", "interface"}:
                break
            _need(value != "len")
            if value in {"(", "[", "{"}:
                stack.append(value)
            elif value in {")", "]", "}"}:
                # A nested function type ends at its containing delimiter.
                if not stack:
                    break
                _need(stack.pop() == {")": "(", "]": "[", "}": "{"}[value])
            previous = current
            cursor += 1
    for declaration in re.finditer(r"\b(?:var|const|type)\s*\(", code):
        depth, cursor = 1, declaration.end()
        while cursor < len(code) and depth:
            depth += (code[cursor] == "(") - (code[cursor] == ")")
            cursor += 1
        _need(depth == 0 and re.compile(r"\blen\b").search(code, declaration.end(), cursor) is None)
    previous = None
    expression_prefixes = {"(", ",", "=", ":", "+", "-", "*", "/", "%", "!", "<", ">", "&", "|", "^",
                           "if", "return", "switch", "case", "range"}
    for token in tokens:
        if token.group() == "len":
            _need(previous in expression_prefixes
                  and re.compile(r"\s*\(\s*[A-Za-z_][A-Za-z0-9_]*\s*\)").match(code, token.end()) is not None)
        previous = token.group()


def _members(body, mapping):
    # Only literal gofmt collections, including existing raw-string related
    # predicates. No expression evaluation, aliases or concatenation.
    token = re.compile(r'\s+|//[^\n]*|/\*[\s\S]*?\*/|`[^`]*`|"(?:\\[\s\S]|[^"\\])*"|[:,]')
    values = []
    offset = 0
    for match in token.finditer(body):
        _need(match.start() == offset)
        offset = match.end()
        item = match.group()
        if item.isspace() or item.startswith(("//", "/*")):
            continue
        if item.startswith('"'):
            values.append(("text", json.loads(item)))
        elif item.startswith('`'):
            values.append(("text", item[1:-1]))
        else:
            values.append(("punctuation", item))
    _need(offset == len(body))
    width = 4 if mapping else 2
    _need(len(values) % width == 0 and len(values) // width <= 1024)
    rows = []
    for start in range(0, len(values), width):
        part = values[start:start+width]
        _need(part[0][0] == "text" and wire._name(part[0][1]) and part[-1] == ("punctuation", ","))
        if mapping:
            _need(part[1] == ("punctuation", ":") and part[2][0] == "text")
            expression = part[2][1]
            _need(type(expression) is str and 0 < len(expression.encode()) <= 4096
                  and all(ord(c) >= 32 and ord(c) != 127 and c != "\ufffd" or c in "\r\n\t" for c in expression))
            rows.append((part[0][1], expression))
        else:
            rows.append(part[0][1])
    names = [row[0] if mapping else row for row in rows]
    _need(len(names) == len(set(names)))
    return tuple(rows)


def _tenant_inventory(raw):
    """Bounded literal inventory shared with profile replay, never execution."""
    _need(type(raw) is bytes and 0 < len(raw) <= MAX_TENANT_SOURCE)
    source = raw.decode("utf-8", "strict")
    _need("\0" not in source and "\ufffd" not in source)
    code = _mask(source)
    _need("/*" not in code and '"' not in code and "`" not in code)
    _builtin_len_calls(code)
    names = set(re.findall(r"\b(?:" + _RESERVED + r")\b", code))
    required = {"DirectTenantTables", "RelatedTenantTables", "DirectTenantTableExemptions", "tenantRLSAdditiveProfileV1"}
    _need(required <= names and len(names) <= MAX_PROFILES + 4)
    parsed = {}
    spans = {}
    for name in sorted(names):
        mapping = name in {"RelatedTenantTables", "DirectTenantTableExemptions"}
        shape = r"map\[string\]string" if mapping else r"\[\]string"
        declarations = list(re.finditer(r"(?m)^var " + name + r" = " + shape + r"\{", code))
        _need(len(declarations) == 1)
        decl = declarations[0]
        _need(code[:decl.start()].count("{") == code[:decl.start()].count("}"))
        opening = decl.end()-1
        end = code.find("}", opening+1)
        _need(end >= 0 and "{" not in code[opening+1:end])
        parsed[name] = _members(source[opening+1:end], mapping)
        spans[name] = source[decl.start():end+1].encode()
        # Existing production reads are len(name) or range name. Any other
        # reserved-name occurrence (shadow, assignment, indexing, alias, etc.)
        # is outside this closed source grammar rather than presumed benign.
        for use in re.finditer(r"\b" + name + r"\b", code):
            if decl.start() <= use.start() <= end:
                continue
            prefix, suffix = code[:use.start()], code[use.end():]
            _need((re.search(r"\blen\s*\(\s*$", prefix) is not None and re.match(r"\s*\)", suffix) is not None)
                  or (re.search(r"\brange\s+$", prefix) is not None and re.match(r"\s*\{", suffix) is not None))
    constants = []
    fixed_constants = {
        "tenantRLSRoutingVersion": "5", "tenantRLSAdditiveProfileVersion": "1",
        "tenantPolicyFingerprintSignature": '"public.rereply_tenant_policy_fingerprint()"',
        "tenantAdditivePolicyFingerprintSignature": '"public.rereply_tenant_policy_additive_fingerprint_v1()"',
    }
    for name in _CONSTANTS:
        found = []
        for match in re.finditer(r"(?m)^const\b", code):
            start = match.end()
            while start < len(code) and code[start].isspace():
                start += 1
            if start < len(code) and code[start] == "(":
                end = code.find("\n)", start)
                _need(end >= 0)
                end += 2
            else:
                end = code.find("\n", start)
                if end < 0:
                    end = len(code)
            if re.search(r"\b" + name + r"\b", code[start:end]):
                found.append(source[match.start():end].encode())
        _need(len(found) == 1)
        body = found[0].decode()
        _need(len(re.findall(r"(?m)^\s*" + name + r"\s*=\s*" + re.escape(fixed_constants[name]) + r"\s*$", body)) == 1)
        constants.append((name, found[0]))
    profiles = []
    for name in sorted(names - required):
        decimal = name.removeprefix("tenantRLSAdditiveProfileV")
        _need(re.fullmatch(r"[1-9][0-9]{0,9}", decimal) is not None)
        number = int(decimal)
        _need(2 <= number <= 2147483647 and 1 <= len(parsed[name]) <= 16
              and parsed[name] == tuple(sorted(parsed[name])))
        profiles.append((number, parsed[name]))
    profiles.sort()
    _need(all(number == index+2 for index, (number, _) in enumerate(profiles)))
    direct = parsed["DirectTenantTables"]
    related = parsed["RelatedTenantTables"]
    exempt = parsed["DirectTenantTableExemptions"]
    v1 = parsed["tenantRLSAdditiveProfileV1"]
    _need(not set(direct).intersection(dict(related)) and not set(direct).intersection(dict(exempt))
          and not set(dict(related)).intersection(dict(exempt)))
    additive = list(v1) + [member for _, members in profiles for member in members]
    _need(len(additive) == len(set(additive)) and set(additive) <= set(direct))
    return TenantInventory(direct, related, exempt, v1, tuple(profiles),
                           tuple(sorted(spans.items())), tuple(constants))


def _globals(raw, facts):
    value = wire._load(raw, MAX_GLOBALS, canonical=False)
    catalog._globals(value, tuple(catalog.ObjectFact(ref, tuple(sorted(attrs.items()))) for ref, attrs in facts.items()))
    _need(type(value) is list and len(value) <= 1024)
    output = {}
    for row in value:
        _need(type(row) is dict and {"name", "reason"} <= set(row) <= {"name", "reason", "keys"})
        name = row["name"]
        _need(wire._name(name) and name not in output and wire._text(row["reason"], 2048) and row["reason"])
        keys = row.get("keys", [])
        _need(type(keys) is list and len(keys) <= 16 and all(wire._name(k) for k in keys) and len(keys) == len(set(keys)))
        output[name] = row
    return output


def _budget(declaration, before_catalog, after_catalog, before_globals, after_globals, before_tenant, after_tenant):
    roles = ((declaration, MAX_DECLARATION), (before_catalog, MAX_DOCUMENT), (after_catalog, MAX_DOCUMENT),
             (before_globals, MAX_GLOBALS), (after_globals, MAX_GLOBALS),
             (before_tenant, MAX_TENANT_SOURCE), (after_tenant, MAX_TENANT_SOURCE))
    _need(all(type(raw) is bytes and 0 < len(raw) <= limit for raw, limit in roles)
          and sum(len(raw) for raw, _ in roles) <= MAX_TOTAL)


def _operation(value):
    wire._fields(value, ("format_version", "id", "kind", "mode", "schema", "relation", "columns", "policy_template"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["id"]) is str and _ID.fullmatch(value["id"]) is not None
          and type(value["kind"]) is str and value["kind"] == "create_ordinary_relation"
          and type(value["schema"]) is str and value["schema"] == "public"
          and type(value["relation"]) is str and _NAME.fullmatch(value["relation"]) is not None
          and type(value["mode"]) is str and value["mode"] in ("global", "direct-tenant"))
    tenant = value["mode"] == "direct-tenant"
    _need((type(value["policy_template"]) is str and value["policy_template"] == POLICY_TEMPLATE)
          if tenant else value["policy_template"] is None)
    expected = (("organization_id", "pg_catalog.uuid"), ("value", "pg_catalog.int8")) if tenant else (("value", "pg_catalog.int8"),)
    cols = value["columns"]
    _need(type(cols) is list and len(cols) == len(expected))
    columns = []
    for col, (name, typ) in zip(cols, expected):
        wire._fields(col, ("name", "type", "nullable", "default"))
        _need(type(col["name"]) is str and col["name"] == name and type(col["type"]) is str
              and col["type"] == typ and col["nullable"] is True and col["default"] is None)
        columns.append(RelationColumn(name, typ, True, None))
    return RelationOperation(value["id"], value["mode"], "public", value["relation"], tuple(columns))


def _operation_value(op):
    _need(type(op) is RelationOperation and type(op.columns) is tuple and all(type(c) is RelationColumn for c in op.columns))
    value = {"format_version": 1, "id": op.id, "kind": "create_ordinary_relation", "mode": op.mode,
             "schema": op.schema, "relation": op.relation, "policy_template": POLICY_TEMPLATE if op.mode == "direct-tenant" else None,
             "columns": [{"name": c.name, "type": c.type, "nullable": c.nullable, "default": c.default} for c in op.columns]}
    _need(_operation(value) == op)
    return value


def render_statement(operation):
    """CREATE TABLE + ENABLE/FORCE only; policies are not executable role SQL."""
    _operation_value(operation)
    target = '"public"."' + operation.relation + '"'
    raw = 'CREATE TABLE ' + target + ' (' + ', '.join('"' + c.name + '" ' + c.type for c in operation.columns) + ');\n'
    if operation.mode == "direct-tenant":
        raw += 'ALTER TABLE ' + target + ' ENABLE ROW LEVEL SECURITY;\nALTER TABLE ' + target + ' FORCE ROW LEVEL SECURITY;\n'
    return raw.encode("ascii")


def _endpoint(value, raw_catalog, raw_globals, raw_tenant, role):
    wire._fields(value, ("role", "catalog", "globals", "tenant_source"))
    _need(value["role"] == role)
    document, facts = _facts(raw_catalog)
    for name, raw, path in (("catalog", raw_catalog, CATALOG_PATH), ("globals", raw_globals, GLOBALS_PATH), ("tenant_source", raw_tenant, TENANT_PATH)):
        fields = ("path", "bytes", "sha256") + (("catalog_version", "catalog_sha256") if name == "catalog" else ())
        binding = wire._fields(value[name], fields)
        _need(binding["path"] == path and wire._integer(binding["bytes"], 1) and binding["bytes"] == len(raw)
              and wire._hash(binding["sha256"]) and binding["sha256"] == wire._digest(raw))
        if name == "catalog":
            _need(wire._integer(binding["catalog_version"]) and binding["catalog_version"] == document["version"]
                  and wire._hash(binding["catalog_sha256"])
                  and binding["catalog_sha256"] == wire._digest(wire._catalog_bytes(document["objects"])))
    return facts, _globals(raw_globals, facts), _tenant_inventory(raw_tenant)


def _acl(privileges):
    return ";".join(sorted("MIGRATION_OWNER>" + grantee + ":" + privilege + ":false" for grantee, rights in privileges for privilege in rights))


TABLE_ACL = _acl((("MIGRATION_OWNER", ("DELETE", "INSERT", "MAINTAIN", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE")), ("RUNTIME", ("DELETE", "INSERT", "SELECT", "UPDATE"))))
TYPE_ACL = _acl((("MIGRATION_OWNER", ("USAGE",)), ("PUBLIC", ("USAGE",))))


def _relation_attrs(tenant):
    return {"relkind": "r", "rls": str(tenant).lower(), "force_rls": str(tenant).lower(), "owner": "MIGRATION_OWNER", "acl": TABLE_ACL,
            "security_invoker": "false", "view_sha256": wire._digest(b""), "partition_key_sha256": wire._digest(b""),
            "partition_bound_sha256": wire._digest(b""), "parents_sha256": wire._digest(b"[]")}


def _policy_attrs(migration):
    predicate = "true" if migration else RUNTIME_PREDICATE
    return {"command": "*", "permissive": "true", "roles": "MIGRATION_OWNER" if migration else "RUNTIME",
            "using_sha256": wire._digest(predicate.encode()), "check_sha256": wire._digest(predicate.encode())}


def _validate_direct_protection(facts, *, table):
    # Called only after complete endpoint parsing; no supplied policy preimage.
    _need(type(facts) is dict and type(table) is str and _NAME.fullmatch(table) is not None)
    relation = catalog.ObjectRef("relations", table, "")
    _need(facts.get(relation) == _relation_attrs(True))
    column = catalog.ObjectRef("columns", table + ".organization_id", table)
    _need(column in facts and facts[column]["type_sha256"] == wire._digest(b"uuid")
          and facts[column]["identity"] == facts[column]["generated"] == facts[column]["acl"] == ""
          and facts[column]["collation_sha256"] == wire._digest(b""))
    expected = {catalog.ObjectRef("policies", table + "." + name, table): _policy_attrs(migration)
                for name, migration in (("rereply_migration_access", True), ("rereply_tenant_isolation", False))}
    _need({ref: attrs for ref, attrs in facts.items() if ref.kind == "policies" and ref.parent == table} == expected)


def _class_name(ref, attrs):
    if ref.kind in {"relations", "sequences"} or ref.kind == "types" and attrs["type"] == "c":
        return ref.identity
    if ref.kind == "indexes":
        return ref.identity.removeprefix(ref.parent + ".")
    return None


def _bundle(facts, op, row_raw, array_raw):
    name = op.relation
    tenant = op.mode == "direct-tenant"
    relation = catalog.ObjectRef("relations", name, "")
    row, array = catalog.ObjectRef("types", name, ""), catalog.ObjectRef("types", "_" + name, "")
    row_value, array_value = wire._definition(row_raw, row=True), wire._definition(array_raw, row=False)
    slots = [[i, c.name, "uuid" if c.type == "pg_catalog.uuid" else "bigint", False, ""] for i, c in enumerate(op.columns, 1)]
    expected_row = ["c", "C", False, "-", "", -1, False, "d", "x",
                    "record_in(cstring,oid,integer)", "record_out(record)", "-", slots, []]
    expected_array = ["b", "A", False, "-", "", -1, False, "d", "x",
                      "array_in(cstring,oid,integer)", "array_out(anyarray)", "public." + name, [], []]
    _need(row_value == expected_row and array_value == expected_array)
    # Exact PostgreSQL jsonb framing was qualified independently. Preserve
    # punctuation inside strings; alternate caller formatting is not a source
    # preimage for the fixed query-derived definition hashes.
    _need(row_raw == json.dumps(expected_row, ensure_ascii=False, separators=(", ", ": "))
          and array_raw == json.dumps(expected_array, ensure_ascii=False, separators=(", ", ": ")))
    expected = {relation: _relation_attrs(tenant), row: {"type": "c", "definition_sha256": wire._digest(row_raw.encode()), "owner": "MIGRATION_OWNER", "acl": TYPE_ACL, "enum_sha256": wire._digest(b"[]")},
                array: {"type": "b", "definition_sha256": wire._digest(array_raw.encode()), "owner": "MIGRATION_OWNER", "acl": TYPE_ACL, "enum_sha256": wire._digest(b"[]")}}
    for i, col in enumerate(op.columns, 1):
        expected[catalog.ObjectRef("columns", name + "." + col.name, name)] = {"position": str(i), "type_sha256": wire._digest(("uuid" if col.type == "pg_catalog.uuid" else "bigint").encode()),
            "not_null": "false", "default_sha256": wire._digest(b""), "identity": "", "generated": "", "collation_sha256": wire._digest(b""), "acl": ""}
    if tenant:
        _validate_direct_protection(facts, table=name)
        for policy, migration in (("rereply_migration_access", True), ("rereply_tenant_isolation", False)):
            expected[catalog.ObjectRef("policies", name + "." + policy, name)] = _policy_attrs(migration)
    _need(all(facts.get(ref) == attrs for ref, attrs in expected.items()))
    # No unmatched children, even a structurally valid extra index/constraint.
    _need({ref for ref in facts if ref.parent == name} == {ref for ref in expected if ref.parent == name})
    _need({ref for ref, attrs in facts.items() if _class_name(ref, attrs) == name} == {relation, row})
    return set(expected)


def _classify(declaration, before_catalog, after_catalog, before_globals, after_globals, before_tenant, after_tenant, base_sha, head_sha):
    _budget(declaration, before_catalog, after_catalog, before_globals, after_globals, before_tenant, after_tenant)
    _need(type(base_sha) is str and _SHA.fullmatch(base_sha) is not None and type(head_sha) is str and _SHA.fullmatch(head_sha) is not None and base_sha != head_sha)
    value = wire._fields(wire._load(declaration, MAX_DECLARATION), ("format_version", "postgres_major", "purpose", "operation", "before", "after", "row_definition_json", "array_definition_json"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1 and type(value["postgres_major"]) is int and value["postgres_major"] == 17 and value["purpose"] == "ordinary-relation-source-shape")
    op = _operation(value["operation"])
    left, old_globals, old_tenant = _endpoint(value["before"], before_catalog, before_globals, before_tenant, "before")
    right, new_globals, new_tenant = _endpoint(value["after"], after_catalog, after_globals, after_tenant, "after")
    name = op.relation
    _need(not any(_class_name(ref, attrs) in (name, "_" + name) or ref.kind == "types" and ref.identity in (name, "_" + name) for ref, attrs in left.items()))
    _need(not any(_class_name(ref, attrs) == "_" + name for ref, attrs in right.items()))
    explained = _bundle(right, op, value["row_definition_json"], value["array_definition_json"])
    _need(not explained.intersection(left))
    if op.mode == "global":
        _need(name not in old_globals and name in new_globals and "keys" not in new_globals[name])
        _need({k: v for k, v in new_globals.items() if k != name} == old_globals and before_tenant == after_tenant)
        _need(name not in set(new_tenant.direct) | set(dict(new_tenant.related))
              | {member for _, members in new_tenant.profiles for member in members} | set(new_tenant.v1))
    else:
        _need(old_globals == new_globals and name not in new_globals and name not in dict(new_tenant.exemptions))
        _need(set(new_tenant.direct) == set(old_tenant.direct) | {name} and name not in old_tenant.direct
              and tuple(item for item in new_tenant.direct if item != name) == old_tenant.direct
              and old_tenant.related == new_tenant.related and old_tenant.exemptions == new_tenant.exemptions
              and old_tenant.v1 == new_tenant.v1 and old_tenant.constants == new_tenant.constants)
        _need(new_tenant.profiles == old_tenant.profiles + ((len(old_tenant.profiles)+2, (name,)),))
        before_spans, after_spans = dict(old_tenant.declarations), dict(new_tenant.declarations)
        _need(all(after_spans.get(k) == v for k, v in before_spans.items() if k != "DirectTenantTables"))
    changed = {ref for ref in left.keys() | right.keys() if left.get(ref) != right.get(ref)}
    _need(explained <= changed)
    return SourceRelationDelta(base_sha, head_sha, wire._digest(declaration), wire._digest(before_catalog), wire._digest(after_catalog),
        wire._digest(before_globals), wire._digest(after_globals), wire._digest(before_tenant), wire._digest(after_tenant), op,
        wire._digest(wire._json_bytes(value["operation"])), wire._digest(render_statement(op)), tuple(sorted(explained)), tuple(sorted(changed-explained)))


def classify_source(declaration, before_catalog, after_catalog, before_globals, after_globals, before_tenant_source, after_tenant_source, *, base_sha, head_sha):
    """Reparse complete source roles; result is not a source or runtime grant."""
    try:
        return _classify(declaration, before_catalog, after_catalog, before_globals, after_globals, before_tenant_source, after_tenant_source, base_sha, head_sha)
    except RelationSourceRefusal:
        raise
    except (wire.SemanticRefusal, catalog.PendingRefusal, nullable.NullableSourceRefusal, ValueError, TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise RelationSourceRefusal() from None
