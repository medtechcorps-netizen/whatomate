"""Prospective symbolic tenant profiles; source consistency, never execution.

Version-5/core and version-1 remain unchanged. Complete raw comparisons are
replayed before a new function or source declaration can be explained. No
observed catalog, physical role OID or caller-provided verdict supplies intent.
"""
from dataclasses import dataclass
import re

import catalog_semantics as wire
import pending_plan as catalog
import schema_nullable as nullable
import schema_relation as relation

SOURCE_PATH = "internal/dbcatalog/source-classification/tenant-additive-v1.json"
CATALOG_PATH = nullable.CATALOG_PATH
GLOBALS_PATH = "internal/dbcatalog/golden/global_tables.json"
TENANT_PATH = "internal/database/tenant.go"
MAX_DECLARATION = 64 << 10
MAX_GLOBALS = 256 << 10
MAX_TENANT = 512 << 10
MAX_DOCUMENT = 4 << 20
MAX_TOTAL = 8 << 20
_DOMAIN = b"rereply-tenant-additive-policy-symbolic-v1\x00"
_EXPRESSION = "(organization_id = (NULLIF(current_setting('app.current_organization_id'::text, true), ''::text))::uuid)"
_ACL = "MIGRATION_OWNER>MIGRATION_OWNER:EXECUTE:false;MIGRATION_OWNER>RUNTIME:EXECUTE:false"
_PREFIX = "rereply_tenant_policy_additive_fingerprint_v"
_CORE = ("rereply_tenant_policy_fingerprint", _PREFIX + "1", "rereply_rls_routing_version")


class TenantProfileSourceRefusal(ValueError):
    def __init__(self):
        super().__init__("tenant-profile-source-invalid")


@dataclass(frozen=True, slots=True)
class ProfileIdentity:
    version: int
    members: tuple[str, ...]
    payload_sha256: str
    policy_digest: str
    function_ref: catalog.ObjectRef
    function_source_sha256: str
    function_definition_sha256: str
    function_config_sha256: str


@dataclass(frozen=True, slots=True)
class TenantDeclarationChange:
    name: str
    before_declaration_sha256: str | None
    after_declaration_sha256: str


@dataclass(frozen=True, slots=True)
class SourceTenantProfileDelta:
    base_sha: str
    head_sha: str
    declaration_sha256: str
    relation_declaration_sha256: str | None
    before_catalog_sha256: str
    after_catalog_sha256: str
    before_globals_sha256: str
    after_globals_sha256: str
    before_tenant_source_sha256: str
    after_tenant_source_sha256: str
    introduced_version: int | None
    retained_versions: tuple[int, ...]
    profiles: tuple[ProfileIdentity, ...]
    tenant_changes: tuple[TenantDeclarationChange, ...]
    explained: tuple[catalog.ObjectRef, ...]
    residual: tuple[catalog.ObjectRef, ...]


def _need(ok):
    if not ok:
        raise TenantProfileSourceRefusal()


def _raw_budget(raw_roles):
    _need(type(raw_roles) is tuple and len(raw_roles) == 6)
    limits = (MAX_DOCUMENT, MAX_DOCUMENT, MAX_GLOBALS, MAX_GLOBALS, MAX_TENANT, MAX_TENANT)
    _need(all(type(raw) is bytes and 0 < len(raw) <= limit for raw, limit in zip(raw_roles, limits)))
    total = sum(map(len, raw_roles))
    _need(total <= MAX_TOTAL)
    return total


def _budget(declaration, relation_declaration, raw_roles):
    total = _raw_budget(raw_roles)
    _need(type(declaration) is bytes and 0 < len(declaration) <= MAX_DECLARATION)
    _need(relation_declaration is None or type(relation_declaration) is bytes
          and 0 < len(relation_declaration) <= MAX_DECLARATION)
    _need(len(declaration) + (len(relation_declaration) if relation_declaration is not None else 0)
          + total <= MAX_TOTAL)


def _pin(value, raw, path):
    wire._fields(value, ("path", "bytes", "sha256"))
    _need(value["path"] == path and wire._integer(value["bytes"], 1)
          and value["bytes"] == len(raw) and wire._hash(value["sha256"])
          and value["sha256"] == wire._digest(raw))


def _endpoint(value, raw_catalog, raw_globals, raw_tenant, role):
    wire._fields(value, ("role", "catalog", "globals", "tenant_source"))
    _need(value["role"] == role)
    binding = wire._fields(value["catalog"],
        ("path", "bytes", "sha256", "catalog_version", "catalog_sha256"))
    _pin({key: binding[key] for key in ("path", "bytes", "sha256")}, raw_catalog, CATALOG_PATH)
    document, facts = nullable._facts(raw_catalog)
    _need(wire._integer(binding["catalog_version"]) and binding["catalog_version"] == document["version"]
          and wire._hash(binding["catalog_sha256"])
          and binding["catalog_sha256"] == wire._digest(wire._catalog_bytes(document["objects"])))
    _pin(value["globals"], raw_globals, GLOBALS_PATH)
    _pin(value["tenant_source"], raw_tenant, TENANT_PATH)
    # Full globals grammar; no fabricated security/count envelope.
    globals_ = _globals(raw_globals, facts)
    return facts, globals_, relation._tenant_inventory(raw_tenant)


def _globals(raw, facts):
    rows = tuple(catalog.ObjectFact(ref, tuple(sorted(attrs.items()))) for ref, attrs in facts.items())
    return {row.name for row in catalog._globals(wire._load(raw, MAX_GLOBALS, canonical=False), rows)}


def _ref(version):
    return catalog.ObjectRef("functions", _PREFIX + str(version) + "(" + wire._digest(b"") + ")", "")


def _payload(version, members, facts):
    _need(wire._integer(version, 2, 2147483647) and type(members) is tuple and 1 <= len(members) <= 16
          and members == tuple(sorted(set(members)))
          and all(wire._name(name) and wire._name("_" + name) for name in members))
    rows = []
    for table in members:
        relation._validate_direct_protection(facts, table=table)
        rows.append({"schema": "public", "table": table, "policies": [
            {"name": "rereply_migration_access", "command": "*", "permissive": True,
             "roles": ["MIGRATION_OWNER"], "using_expression": "true", "check_expression": "true"},
            {"name": "rereply_tenant_isolation", "command": "*", "permissive": True,
             "roles": ["RUNTIME"], "using_expression": _EXPRESSION, "check_expression": _EXPRESSION}]})
    value = {"format_version": 1, "postgres_major": 17, "profile_version": version, "members": rows}
    _need(len(wire._json_bytes(value)) <= MAX_DECLARATION)
    return value


def _function(version, members, facts):
    payload = _payload(version, members, facts)
    raw = wire._json_bytes(payload)
    digest = wire._digest(_DOMAIN + raw)
    token = "v" + str(version) + ":sym1:" + digest
    source = "SELECT '" + token + "'::text"
    definition = ("CREATE OR REPLACE FUNCTION public." + _PREFIX + str(version) + "()\n"
        " RETURNS text\n LANGUAGE sql\n IMMUTABLE\n SET search_path TO 'pg_catalog'\n"
        "AS $function$" + source + "$function$\n")
    config = '["search_path=pg_catalog"]'
    expected = {"source_sha256": wire._digest(source.encode()), "definition_sha256": wire._digest(definition.encode()),
        "security_definer": "false", "owner": "MIGRATION_OWNER", "acl": _ACL,
        "config_sha256": wire._digest(config.encode()), "volatility": "i", "parallel": "u",
        "strict": "false", "leakproof": "false", "language_sha256": wire._digest(b"sql")}
    ref = _ref(version)
    _need(facts.get(ref) == expected)
    identity = ProfileIdentity(version, members, wire._digest(raw), digest, ref,
        expected["source_sha256"], expected["definition_sha256"], expected["config_sha256"])
    return identity, {"payload": payload, "function_source": source,
                      "function_definition": definition, "function_config": config}


def _identities(inventory, facts, globals_):
    profiles = inventory.profiles
    _need(tuple(n for n, _ in profiles) == tuple(range(2, 2 + len(profiles))))
    additive = set(inventory.v1)
    output, descriptions = [], []
    for version, members in profiles:
        _need(not additive.intersection(members) and set(members) <= set(inventory.direct)
              and not set(members).intersection(dict(inventory.related))
              and not set(members).intersection(globals_))
        additive.update(members)
        item, description = _function(version, members, facts)
        output.append(item)
        descriptions.append(description)
    actual = {ref for ref in facts if ref.kind == "functions" and ref.identity.startswith(_PREFIX)
              and re.match(re.escape(_PREFIX) + r"(?:[2-9]|[1-9][0-9]+)\(", ref.identity)}
    _need(actual == {item.function_ref for item in output})
    return tuple(output), descriptions


def _inventory_transition(before, after):
    _need(before.related == after.related and before.exemptions == after.exemptions
          and before.v1 == after.v1 and before.constants == after.constants)
    old, new = dict(before.declarations), dict(after.declarations)
    for name in ("RelatedTenantTables", "DirectTenantTableExemptions", "tenantRLSAdditiveProfileV1"):
        _need(old.get(name) == new.get(name))
    _need(after.profiles[:len(before.profiles)] == before.profiles
          and len(after.profiles) in (len(before.profiles), len(before.profiles) + 1))
    for version, _ in before.profiles:
        name = "tenantRLSAdditiveProfileV" + str(version)
        _need(old[name] == new[name])
    added = () if len(after.profiles) == len(before.profiles) else after.profiles[-1][1]
    _need(len(added) <= 1 and not set(added).intersection(before.direct)
          and tuple(name for name in after.direct if name not in added) == before.direct
          and set(after.direct) - set(before.direct) == set(added))
    def core(item):
        union = set(item.v1)
        for _, members in item.profiles:
            union.update(members)
        return (set(item.direct) | set(dict(item.related))) - union
    _need(core(before) == core(after))
    if not added:
        _need(old == new)
        return None, ()
    version = after.profiles[-1][0]
    name = "tenantRLSAdditiveProfileV" + str(version)
    _need(name not in old and set(new) - set(old) == {name})
    changes = tuple(TenantDeclarationChange(label, wire._digest(old[label]) if label in old else None,
        wire._digest(new[label])) for label in ("DirectTenantTables", name))
    return version, changes


def _core_rows(left, right):
    for name in _CORE:
        ref = catalog.ObjectRef("functions", name + "(" + wire._digest(b"") + ")", "")
        _need(ref in left and left[ref] == right.get(ref))


def _classify(declaration, relation_declaration, raws, base_sha, head_sha):
    _budget(declaration, relation_declaration, raws)
    _need(type(base_sha) is str and nullable._SHA.fullmatch(base_sha)
          and type(head_sha) is str and nullable._SHA.fullmatch(head_sha) and base_sha != head_sha)
    value = wire._fields(wire._load(declaration, MAX_DECLARATION),
        ("format_version", "postgres_major", "purpose", "before", "after", "relation", "profiles"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and type(value["postgres_major"]) is int and value["postgres_major"] == 17
          and value["purpose"] == "tenant-additive-source-shape")
    bc, ac, bg, ag, bt, at = raws
    left, old_globals, old = _endpoint(value["before"], bc, bg, bt, "before")
    right, new_globals, new = _endpoint(value["after"], ac, ag, at, "after")
    relation_delta = None
    if relation_declaration is None:
        _need(value["relation"] is None)
    else:
        _pin(value["relation"], relation_declaration, relation.SOURCE_PATH)
        relation_delta = relation.classify_source(relation_declaration, *raws, base_sha=base_sha, head_sha=head_sha)
        _need((relation_delta.base_sha, relation_delta.head_sha) == (base_sha, head_sha)
              and tuple(getattr(relation_delta, key) for key in (
                  "before_catalog_sha256", "after_catalog_sha256", "before_globals_sha256",
                  "after_globals_sha256", "before_tenant_source_sha256", "after_tenant_source_sha256"))
              == tuple(map(wire._digest, raws)))
    introduced, changes = _inventory_transition(old, new)
    _core_rows(left, right)
    prior, _ = _identities(old, left, old_globals)
    profiles, descriptions = _identities(new, right, new_globals)
    _need(profiles and profiles[:len(prior)] == prior
          and wire._json_bytes(value["profiles"]) == wire._json_bytes(descriptions))
    explained = ()
    if introduced is not None:
        item = profiles[-1]
        _need(relation_delta is not None and relation_delta.operation.mode == "direct-tenant"
              and relation_delta.operation.schema == "public"
              and item.members == (relation_delta.operation.relation,)
              and item.function_ref not in left)
        explained = (item.function_ref,)
    elif relation_delta is not None:
        _need(relation_delta.operation.mode == "global")
    changed = {ref for ref in left.keys() | right.keys() if left.get(ref) != right.get(ref)}
    _need(set(explained) <= changed)
    return SourceTenantProfileDelta(base_sha, head_sha, wire._digest(declaration),
        None if relation_declaration is None else wire._digest(relation_declaration),
        *map(wire._digest, raws), introduced, tuple(item.version for item in prior), profiles,
        changes, explained, tuple(sorted(changed - set(explained))))


def classify_source(declaration, relation_declaration, before_catalog, after_catalog,
                    before_globals, after_globals, before_tenant_source, after_tenant_source,
                    *, base_sha, head_sha):
    """Replay complete source bytes; a typed output is not an authority token."""
    try:
        return _classify(declaration, relation_declaration,
            (before_catalog, after_catalog, before_globals, after_globals, before_tenant_source, after_tenant_source),
            base_sha, head_sha)
    except TenantProfileSourceRefusal:
        raise
    except (ValueError, TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise TenantProfileSourceRefusal() from None


def validate_retained_pair(before_catalog, after_catalog, before_globals, after_globals,
                           before_tenant_source, after_tenant_source):
    """Immutable first-parent profile history, under caller's original allowance.

    This does not explain additions or authorize a new profile. The current
    endpoint declaration is independently replayed by classify_source.
    """
    try:
        raws = (before_catalog, after_catalog, before_globals, after_globals, before_tenant_source, after_tenant_source)
        _raw_budget(raws)
        _, left = nullable._facts(before_catalog)
        _, right = nullable._facts(after_catalog)
        old = relation._tenant_inventory(before_tenant_source)
        new = relation._tenant_inventory(after_tenant_source)
        old_globals = _globals(before_globals, left)
        new_globals = _globals(after_globals, right)
        _inventory_transition(old, new)
        _core_rows(left, right)
        prior, _ = _identities(old, left, old_globals)
        current, _ = _identities(new, right, new_globals)
        _need(current[:len(prior)] == prior)
    except TenantProfileSourceRefusal:
        raise
    except (ValueError, TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise TenantProfileSourceRefusal() from None
