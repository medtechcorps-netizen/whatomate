"""Closed pending source contract, format 1; no runtime authority.

This module reads supplied immutable bytes only. It performs no filesystem,
Git, SQL, provider or process operations. An operation named concurrently and a
source hash are declarations, not proof of execution or of SQL semantics.
Active artifacts retain their existing bytes/format; only new pending files
must use this module's canonical JSON encoding.
"""
from dataclasses import dataclass
import hashlib
import json
import re

PREFIX = "internal/dbcatalog/golden/pending/"
PLAN = PREFIX + "history.pending.json"
GLOBALS = PREFIX + "global_tables.pending.json"
MAX_FILE_BYTES = 8 * 1024 * 1024
MAX_BUNDLE_BYTES = 32 * 1024 * 1024
MAX_ENTRIES = 1  # Format 1 is exactly one pending target, active + 1.
MAX_STEPS = 512
MAX_OBJECTS = 50000
MAX_NODES = 800000
MAX_DEPTH = 16
_ID = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
_NAME = re.compile(r"[a-z_][a-z0-9_]{0,62}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_FUNCTION = re.compile(r"[a-z_][a-z0-9_]{0,62}\([0-9a-f]{64}\)\Z")
_OPS = frozenset({"create_relation", "add_column", "create_index_concurrently",
    "create_function", "grant", "apply_tenant_rls", "seed_reconciler", "ledger_step"})
_CHILDREN = frozenset({"columns", "constraints", "indexes", "triggers", "policies"})
# Exact current catalog attribute vocabulary. This validates shape, never
# reverses the hashed expressions or derives concurrency/constant-defaults.
_ATTRS = {
    "relations": {"relkind":"kind", "rls":"bool", "force_rls":"bool", "owner":"role", "acl":"acl", "security_invoker":"bool", "view_sha256":"hash", "partition_key_sha256":"hash", "partition_bound_sha256":"hash", "parents_sha256":"hash"},
    "columns": {"position":"int", "type_sha256":"hash", "not_null":"bool", "default_sha256":"hash", "identity":"identity", "generated":"generated", "collation_sha256":"hash", "acl":"acl"},
    "constraints": {"type":"constraint", "definition_sha256":"hash", "validated":"bool", "deferrable":"bool", "deferred":"bool"},
    "indexes": {"definition_sha256":"hash", "valid":"bool", "ready":"bool", "live":"bool", "unique":"bool", "primary":"bool", "exclusion":"bool"},
    "triggers": {"definition_sha256":"hash", "enabled":"enabled", "internal":"bool", "function_sha256":"hash"},
    "functions": {"source_sha256":"hash", "definition_sha256":"hash", "security_definer":"bool", "owner":"role", "acl":"acl", "config_sha256":"hash", "volatility":"volatility", "parallel":"parallel", "strict":"bool", "leakproof":"bool", "language_sha256":"hash"},
    "policies": {"command":"command", "permissive":"bool", "roles":"roles", "using_sha256":"hash", "check_sha256":"hash"},
    "sequences": {"type_sha256":"hash", "start":"int", "increment":"int", "min":"int", "max":"int", "cache":"int", "cycle":"bool", "owner":"role", "acl":"acl", "owned_by_sha256":"hash"},
    "types": {"type":"type", "definition_sha256":"hash", "owner":"role", "acl":"acl", "enum_sha256":"hash"},
    "extensions": {"version_sha256":"hash", "relocatable":"bool", "owner":"role"},
}


class PendingRefusal(ValueError):
    def __init__(self, code):
        self.code = "pending-invalid:" + code
        super().__init__(self.code)


def _need(ok, code):
    if not ok:
        raise PendingRefusal(code)


def canonical(value):
    """Source wire encoding, not a database catalog fingerprint."""
    return (json.dumps(value, sort_keys=True, separators=(",", ":"),
                       ensure_ascii=True, allow_nan=False) + "\n").encode("ascii")


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def _fields(value, keys, code):
    _need(type(value) is dict and set(value) == set(keys), code)
    return value


def _integer(value, minimum=0, maximum=2**31-1):
    return type(value) is int and minimum <= value <= maximum


def _hash(value):
    return type(value) is str and _HASH.fullmatch(value) is not None


def _pairs(rows):
    out = {}
    for key, value in rows:
        _need(key not in out, "duplicate-key")
        out[key] = value
    return out


def _nonfinite(_):
    raise PendingRefusal("json")


def _load(raw, *, canonical_required):
    _need(type(raw) is bytes and 0 < len(raw) <= MAX_FILE_BYTES, "file-bound")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_pairs,
                           parse_constant=_nonfinite)
        stack, count = [(value, 0)], 0
        while stack:
            node, depth = stack.pop(); count += 1
            _need(count <= MAX_NODES and depth <= MAX_DEPTH, "json-bound")
            if type(node) is dict:
                _need(all(type(k) is str and len(k) <= 128 for k in node), "json-bound")
                stack.extend((v, depth+1) for v in node.values())
            elif type(node) is list:
                stack.extend((v, depth+1) for v in node)
            else:
                _need(node is None or type(node) in (str, int, bool), "json-type")
                _need(type(node) is not str or len(node) <= 16384, "json-bound")
        if canonical_required:
            _need(canonical(value) == raw, "noncanonical")
        return value
    except (UnicodeError, ValueError, TypeError, RecursionError, OverflowError) as exc:
        if type(exc) is PendingRefusal:
            raise
        raise PendingRefusal("json") from None


@dataclass(frozen=True, slots=True, order=True)
class ObjectRef:
    kind: str
    identity: str
    parent: str


@dataclass(frozen=True, slots=True)
class ObjectFact:
    ref: ObjectRef
    attributes: tuple[tuple[str, str], ...]


@dataclass(frozen=True, slots=True)
class GlobalTable:
    name: str
    reason: str
    keys: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class Step:
    id: str
    operation: str
    objects: tuple[ObjectRef, ...]
    depends_on: tuple[str, ...]
    source_sha256: str
    requires_gates: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class Entry:
    version: int
    kind: str
    min_reader_version: int
    catalog_file: str
    catalog_file_sha256: str
    globals_sha256: str
    objects: tuple[ObjectFact, ...]
    globals: tuple[GlobalTable, ...]
    steps: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class PendingPlan:
    format_version: int
    active_version: int
    active_catalog_file_sha256: str
    active_globals_file_sha256: str
    active_history_file_sha256: str
    entries: tuple[Entry, ...]
    steps: tuple[Step, ...]
    files: tuple[tuple[str, str], ...]


def _reference(row):
    _fields(row, ("kind", "identity", "parent"), "object-ref")
    kind, name, parent = row["kind"], row["identity"], row["parent"]
    _need(type(kind) is str and kind in _ATTRS and type(name) is str
          and type(parent) is str, "object-ref")
    if kind in _CHILDREN:
        _need(_NAME.fullmatch(parent) and name.startswith(parent+".")
              and _NAME.fullmatch(name[len(parent)+1:]), "object-ref")
    else:
        _need(parent == "" and (_FUNCTION.fullmatch(name) if kind == "functions"
                               else _NAME.fullmatch(name)), "object-ref")
    return ObjectRef(kind, name, parent)


def _attribute(shape, value):
    if type(value) is not str:
        return False
    if shape == "hash": return _hash(value)
    if shape == "bool": return value in ("true", "false")
    if shape == "int": return re.fullmatch(r"-?(?:0|[1-9][0-9]{0,19})", value) is not None
    if shape == "role": return value in {"MIGRATION_OWNER", "RUNTIME", "PUBLIC", "OTHER"}
    if shape == "roles":
        roles=value.split(",")
        return roles == sorted(set(roles)) and all(_attribute("role", r) for r in roles)
    if shape == "acl":
        if value == "": return True
        rows=value.split(";")
        privileges={"SELECT","INSERT","UPDATE","DELETE","TRUNCATE","REFERENCES","TRIGGER","EXECUTE","USAGE","CREATE","CONNECT","TEMPORARY","SET","ALTER_SYSTEM","MAINTAIN"}
        return len(value)<=8192 and rows==sorted(set(rows)) and all(
            (m:=re.fullmatch(r"(MIGRATION_OWNER|RUNTIME|PUBLIC|OTHER)>(MIGRATION_OWNER|RUNTIME|PUBLIC):([A-Z_]+):(true|false)", r))
            and m[3] in privileges for r in rows)
    return value in {"kind":tuple("rpvmfS"),"identity":("","a","d"),
        "generated":("","s","v"),"constraint":tuple("cfpuxnt"),
        "enabled":tuple("ODRA"),"volatility":tuple("isv"),"parallel":tuple("sru"),
        "command":tuple("*rawd"),"type":tuple("bcdeprm")}[shape]


def _catalog(value, expected_version):
    _fields(value, ("version", "objects"), "catalog")
    _need(_integer(value["version"]) and value["version"] == expected_version
          and type(value["objects"]) is list and 0 < len(value["objects"]) <= MAX_OBJECTS, "catalog")
    out=[]
    for row in value["objects"]:
        _need(type(row) is dict and {"kind","identity","attributes"} <= set(row)
              and set(row) <= {"kind","identity","parent","attributes"}, "catalog-object")
        ref=_reference({"kind":row["kind"],"identity":row["identity"],"parent":row.get("parent","")})
        attrs=_fields(row["attributes"],_ATTRS[ref.kind],"catalog-attributes")
        _need(all(_attribute(shape,attrs[key]) for key,shape in _ATTRS[ref.kind].items()),"catalog-attributes")
        out.append(ObjectFact(ref,tuple(sorted(attrs.items()))))
    refs=[o.ref for o in out]
    _need(len(set(refs))==len(refs),"catalog-duplicate")
    relations={o.ref.identity for o in out if o.ref.kind=="relations"}
    _need(all(o.ref.parent in relations for o in out if o.ref.kind in _CHILDREN),"catalog-parent")
    return tuple(sorted(out,key=lambda x:x.ref))


def _globals(value, catalog):
    _need(type(value) is list and len(value)<=1024,"globals")
    relations={o.ref.identity for o in catalog if o.ref.kind=="relations"}
    out=[]
    for row in value:
        _need(type(row) is dict and {"name","reason"} <= set(row)
              and set(row)<={"name","reason","keys"},"globals")
        name,reason,keys=row["name"],row["reason"],row.get("keys",[])
        _need(type(name) is str and _NAME.fullmatch(name) and name in relations
              and type(reason) is str and 0<len(reason)<=2048
              and not any(ord(c)<32 for c in reason),"globals")
        _need(type(keys) is list and len(keys)<=16 and all(type(k) is str and _NAME.fullmatch(k) for k in keys)
              and len(set(keys))==len(keys),"globals")
        column_names={o.ref.identity for o in catalog if o.ref.kind=="columns" and o.ref.parent==name}
        _need(all(name+"."+key in column_names for key in keys),"globals-key")
        out.append(GlobalTable(name,reason,tuple(keys)))
    _need(len({r.name for r in out})==len(out),"globals-duplicate")
    return tuple(sorted(out,key=lambda x:x.name))


def _ids(value, *, limit, nonempty=False):
    _need(type(value) is list and (0<len(value) if nonempty else True)
          and len(value)<=limit and all(type(v) is str and _ID.fullmatch(v) for v in value)
          and value==sorted(set(value)),"ids")
    return tuple(value)


def _active(catalog, globals_, history):
    rows=_load(history,canonical_required=False)
    _need(type(rows) is list and 1<=len(rows)<=1024,"active-history")
    old_version,old_floor=-1,0
    for i,row in enumerate(rows):
        _need(type(row) is dict and {"version","class","min_reader_version"}<=set(row)
              and set(row)<={"version","class","min_reader_version","apply_engine"},"active-history")
        v,floor=row["version"],row["min_reader_version"]
        _need(_integer(v) and _integer(floor) and v>old_version and old_floor<=floor<=v
              and type(row["class"]) is str and row["class"] in ("baseline","expand","data","contract")
              and type(row.get("apply_engine",False)) is bool,"active-history")
        _need((i==0 and v==floor==0 and row["class"]=="baseline")
              or (i>0 and row["class"]!="baseline"),"active-history")
        _need(floor==old_floor or row["class"]=="contract","active-history")
        old_version,old_floor=v,floor
    _need(rows[-1].get("apply_engine",False) is False,"active-engine")
    objects=_catalog(_load(catalog,canonical_required=False),old_version)
    globals_facts=_globals(_load(globals_,canonical_required=False),objects)
    return old_version,old_floor,objects,globals_facts


def _parse(active_catalog, active_globals, active_history, files):
    _need(type(files) is dict and 3<=len(files)<=MAX_ENTRIES+2
          and all(type(k) is str and type(v) is bytes for k,v in files.items()),"bundle")
    _need(sum(len(v) for v in files.values()) + sum(len(x) for x in
          (active_catalog,active_globals,active_history) if type(x) is bytes) <= MAX_BUNDLE_BYTES,"bundle-bound")
    _need(PLAN in files and GLOBALS in files,"bundle")
    active_version,floor,prior_objects,_=_active(active_catalog,active_globals,active_history)
    doc=_fields(_load(files[PLAN],canonical_required=True),
        ("format_version","active_version","active_catalog_file_sha256","active_globals_file_sha256",
         "active_history_file_sha256","globals_file_sha256","entries","steps"),"plan")
    _need(type(doc["format_version"]) is int and doc["format_version"]==1
          and _integer(doc["active_version"]) and doc["active_version"]==active_version,"plan-version")
    for key,raw in (("active_catalog_file_sha256",active_catalog),("active_globals_file_sha256",active_globals),
                    ("active_history_file_sha256",active_history),("globals_file_sha256",files[GLOBALS])):
        _need(_hash(doc[key]) and doc[key]==digest(raw),"binding")
    _need(type(doc["entries"]) is list and 1<=len(doc["entries"])<=MAX_ENTRIES
          and type(doc["steps"]) is list and 1<=len(doc["steps"])<=MAX_STEPS,"plan-count")
    global_doc=_load(files[GLOBALS],canonical_required=True)
    _need(type(global_doc) is list,"globals-bundle")
    steps=[]
    for row in doc["steps"]:
        _fields(row,("id","operation","objects","depends_on","source_sha256","requires_gates"),"step")
        _need(type(row["id"]) is str and _ID.fullmatch(row["id"])
              and type(row["operation"]) is str and row["operation"] in _OPS
              and _hash(row["source_sha256"]),"step")
        _need(type(row["objects"]) is list and 1<=len(row["objects"])<=2048,"step-objects")
        refs=tuple(_reference(r) for r in row["objects"])
        _need(refs==tuple(sorted(set(refs))),"step-objects")
        steps.append(Step(row["id"],row["operation"],refs,
            _ids(row["depends_on"],limit=MAX_STEPS),row["source_sha256"],
            _ids(row["requires_gates"],limit=32)))
    _need([s.id for s in steps]==sorted({s.id for s in steps}),"step-duplicate-order")
    step_map={s.id:s for s in steps}
    entries=[]; claimed=set(); expected_files={PLAN,GLOBALS}
    for offset,row in enumerate(doc["entries"],1):
        _fields(row,("version","class","min_reader_version","pending","catalog_file",
                     "catalog_file_sha256","globals_sha256","steps"),"entry")
        v=active_version+offset
        _need(_integer(row["version"],1) and row["version"]==v and row["pending"] is True
              and type(row["class"]) is str and row["class"] in ("expand","data","contract")
              and _integer(row["min_reader_version"]) and floor<=row["min_reader_version"]<=v
              and (row["min_reader_version"]==floor or row["class"]=="contract"),"entry")
        floor=row["min_reader_version"]
        name=PREFIX+f"catalog-v{v}.json"
        _need(row["catalog_file"]==name and name in files and _hash(row["catalog_file_sha256"])
              and row["catalog_file_sha256"]==digest(files[name]),"catalog-binding")
        expected_files.add(name)
        objects=_catalog(_load(files[name],canonical_required=True),v)
        _need(_hash(row["globals_sha256"]) and digest(files[GLOBALS])==row["globals_sha256"],"globals-binding")
        global_facts=_globals(global_doc,objects)
        ids=row["steps"]
        _need(type(ids) is list and 1<=len(ids)<=MAX_STEPS
              and all(type(i) is str and i in step_map for i in ids) and len(set(ids))==len(ids),"entry-steps")
        refs={o.ref for o in (*prior_objects,*objects)}
        old={o.ref:o for o in prior_objects}; new={o.ref:o for o in objects}
        changed={ref for ref in old.keys()|new.keys() if old.get(ref)!=new.get(ref)}
        covered=set()
        for ident in ids:
            step=step_map[ident]
            _need(ident not in claimed and set(step.depends_on)<=claimed,"dependency-order")
            _need(set(step.objects)<=refs,"unbound-object")
            covered.update(step.objects); claimed.add(ident)
        _need(changed<=covered,"uncovered-delta")
        entries.append(Entry(v,row["class"],floor,name,row["catalog_file_sha256"],
            row["globals_sha256"],objects,global_facts,tuple(ids)))
        prior_objects=objects
    _need(claimed==set(step_map) and set(files)==expected_files,"bundle-extra")
    return PendingPlan(1,active_version,doc["active_catalog_file_sha256"],doc["active_globals_file_sha256"],
        doc["active_history_file_sha256"],tuple(entries),tuple(steps),
        tuple((name,digest(raw)) for name,raw in sorted(files.items())))


def parse_bundle(*, active_catalog: bytes, active_globals: bytes, active_history: bytes,
                 files: dict[str, bytes], prior_files: dict[str, bytes] | None = None) -> PendingPlan:
    """Validate bytes, including optional prior continuity; no trust inferred.

    Prior files are reparsed under the same active-byte binding. Existing
    bundle cannot be deleted, changed or extended while active bytes stay the
    same. Format 1 supports one target with multiple named steps/gates, not
    simultaneous future catalog versions. Activation/consumption is a separate,
    not-yet-implemented operation, so changed active bytes refuse here.
    """
    result=_parse(active_catalog,active_globals,active_history,files)
    if prior_files is not None:
        previous=_parse(active_catalog,active_globals,active_history,prior_files)
        _need(result==previous,"prior-bundle-changed")
    return result
