"""Closed, bounded transport for preserved promotion data; no trust grant.

Only protected same-workflow needs.production.outputs custody can authorize the
decoded data. An unkeyed hash and reconstructed immutable types do not verify
signatures. Full evidence history is retained; no current-main reconstruction or
old artifact download occurs. The operational caller encodes/roundtrips before
effects and emits only after success. Format6 adds complete original owner
observations; historical1-5 retain their exact field sets and absence semantics.
No record-time label collection, new clock or active-contract proof is supplied.
Some otherwise valid upstream-max histories exceed these transport limits;
they refuse before effects. No attempt or source bytes are dropped to make room.
"""
import base64
import binascii
from datetime import datetime, timezone
from functools import wraps
import json
import re
import zlib

import record_writer as writer

e, c, p, r, t, d = (writer.evidence, writer.collect, writer.policy,
                    writer.resolver, writer.transport, writer.records)
s = r.schema_change
pending = r.pending_source
contract = writer.contract_authorization

VERSION = "6"
CHUNK_SIZE = 8192
CHUNK_SLOTS = tuple(f"handoff_{i:02d}" for i in range(1, 33))
MAX_ENCODED = CHUNK_SIZE * len(CHUNK_SLOTS)
MAX_COMPRESSED = MAX_ENCODED // 4 * 3
MAX_RAW = 16 * 1024 * 1024
MAX_BLOB = 4 * 1024 * 1024
MAX_EXPANDED_BYTES = 16 * 1024 * 1024
MAX_BLOBS = 4096
MAX_NODES = 100_000
MAX_DEPTH = 48
XZ_MEMORY_LIMIT = 64 * 1024 * 1024
MAX_INTEGER = 10 ** 20 - 1
METADATA = ("handoff_version", "handoff_sha256", "handoff_raw_length",
            "handoff_compressed_length", "handoff_encoded_length", "handoff_chunk_count")
OUTPUT_KEYS = METADATA + CHUNK_SLOTS


class HandoffRefusal(Exception):
    """One fixed error; raw source/evidence or compressed data is never echoed."""


def _need(value):
    if not value:
        raise HandoffRefusal("record-handoff-invalid")


def _public(function):
    @wraps(function)
    def call(*args, **kwargs):
        try:
            return function(*args, **kwargs)
        except HandoffRefusal:
            raise
        except (writer.WriterRefusal, p.PolicyRefusal, writer.common.ReleaseError, ValueError, TypeError,
                AttributeError, KeyError, IndexError, OverflowError, RecursionError, zlib.error):
            raise HandoffRefusal("record-handoff-invalid") from None
    return call


def _many(schema):
    return ("many", schema)


def _optional(schema):
    return ("union", type(None), schema)


_PAIR = ("pair", str, str)
_PAIRS = _many(_PAIR)
_STRINGS = _many(str)

# Literal wire IDs, classes AND field schemas. No annotation inspection, class
# discovery, dynamic import, arbitrary constructor, or generic object graph.
_SCHEMAS = {
    c.CollectedHistory: ("compat_collect.CollectedHistory", (
        ("history", e.VisibleHistory), ("issues", _STRINGS), ("command_count", int),
        ("captured_bytes", int), ("transcript_sha256", str))),
    e.Artifact: ("compat_evidence.Artifact", (
        ("artifact_id", int), ("name", str), ("run_id", int), ("run_attempt", int),
        ("sha", str), ("expired", bool), ("files", _many(e.ArtifactFile)))),
    e.ArtifactFile: ("compat_evidence.ArtifactFile", (("name", str), ("sha256", str), ("kind", str))),
    e.Attempt: ("compat_evidence.Attempt", (
        ("repository", str), ("workflow_path", str), ("sha", str), ("workflow_sha", str),
        ("event", str), ("ref", str), ("run_id", int), ("run_number", int), ("run_attempt", int),
        ("latest_run_attempt", int), ("status", str), ("conclusion", _optional(str)),
        ("requested_at", _optional(datetime)), ("started_at", _optional(datetime)),
        ("completed_at", _optional(datetime)), ("jobs", _many(e.Job)),
        ("measurement", ("union", e.AuthenticatedPair, e.MissingEvidence)),
        ("execution_window", _optional(e.ExecutionWindow)))),
    e.AuthenticatedPair: ("compat_evidence.AuthenticatedPair", (
        ("result", bytes), ("context", bytes), ("result_subject", e.VerifiedSubject),
        ("context_subject", e.VerifiedSubject), ("artifact", e.Artifact))),
    e.ExecutionWindow: ("compat_evidence.ExecutionWindow", (
        ("started_at", datetime), ("completed_at", datetime), ("fresh", bool))),
    e.Job: ("compat_evidence.Job", (
        ("name", str), ("job_id", int), ("check_run_id", int), ("app_id", int),
        ("run_id", int), ("run_attempt", int), ("sha", str), ("status", str), ("conclusion", _optional(str)))),
    e.VerifiedSubject: ("compat_evidence.VerifiedSubject", (
        ("subject_name", str), ("subject_sha256", str), ("predicate_type", str), ("issuer", str),
        ("signer_uri", str), ("signer_sha", str), ("source_sha", str), ("source_ref", str),
        ("runner", str), ("run_id", int), ("run_attempt", int), ("verification_reference", str))),
    e.VisibleHistory: ("compat_evidence.VisibleHistory", (
        ("sha", str), ("repository", str), ("workflow_path", str), ("observation_id", str),
        ("observed_at", datetime), ("attempts", _many(e.Attempt)), ("enumeration", str),
        ("whole_run_deletion_coverage", str))),
    p.HistoryVersion: ("record_policy.HistoryVersion", (("version", int), ("minimum", int), ("kind", str))),
    p.LegacyBaseline: ("record_policy.LegacyBaseline", (("release", str), ("manifest_sha256", str), ("source_sha", str))),
    p.LiveContract: ("record_policy.LiveContract", (
        ("catalog", d.DocumentCatalog), ("origin_release", str), ("origin_manifest_sha256", str), ("origin_source", p.SourceFacts))),
    p.PolicyChain: ("record_policy.PolicyChain", (
        ("entries", _many(p.PolicyEntry)), ("inputs", _many(p.RecordInput)), ("sources", _many(p.SourceFacts)),
        ("ancestors", _PAIRS), ("baseline", _optional(p.LegacyBaseline)), ("runtime_source_supported", bool))),
    p.PolicyEntry: ("record_policy.PolicyEntry", (
        ("release", str), ("kind", str), ("sha", str), ("images", _PAIRS), ("deployed_at", str),
        ("previous", _optional(str)), ("rolled_back_from", _optional(str)), ("record", p.RecordInput),
        ("facts", d.ManifestFacts), ("reader", p.SourceFacts), ("live", _optional(p.LiveContract)))),
    p.RecordInput: ("record_policy.RecordInput", (("raw", bytes), ("manifest_sha256", str), ("web_source_sha", str))),
    p.SourceBlob: ("record_policy.SourceBlob", (("path", str), ("raw", bytes))),
    p.SourceFacts: ("record_policy.SourceFacts", (
        ("sha", str), ("reader_version", int), ("catalog", _optional(d.DocumentCatalog)),
        ("artifacts", _PAIRS), ("history", _many(p.HistoryVersion)), ("mode", str))),
    r.BlobObservation: ("record_resolver.BlobObservation", (("path", str), ("raw", _optional(bytes)))),
    r.CatalogMaterial: ("record_resolver.CatalogMaterial", (
        ("candidate_sha", str), ("candidate_blobs", _many(p.SourceBlob)), ("baseline_sha", str),
        ("baseline_provenance", str), ("baseline_blobs", _many(r.BlobObservation)))),
    r.ChangedPath: ("record_resolver.ChangedPath", (("status", str), ("path", str))),
    r.ChangedRange: ("record_resolver.ChangedRange", (
        ("roles", _STRINGS), ("base_sha", str), ("head_sha", str), ("rows", _many(r.ChangedPath)))),
    r.ForwardAuditMaterial: ("record_resolver.ForwardAuditMaterial", (
        ("candidate_sha", str), ("drills", r.BlobObservation), ("changes", _many(r.ChangedRange)))),
    pending.PendingBlob: ("pending_source.PendingBlob", (("path", str), ("raw", bytes))),
    pending.PendingMaterial: ("pending_source.PendingMaterial", (
        ("candidate_sha", str), ("previous_sha", str), ("candidate_files", _many(pending.PendingBlob)),
        ("previous_files", _many(pending.PendingBlob)))),
    r.ForwardResolution: ("record_resolver.ForwardResolution", (
        ("resolved", r.ResolvedChain), ("candidate", p.SourceFacts), ("classification", s.PolicyClassification),
        ("workflow", bytes), ("workflow_sha256", str), ("previous_web_source_sha", str),
        ("database_base_source", str), ("catalog_material", r.CatalogMaterial), ("previous_recipe", r.WebRecipe),
        ("audit", r.ForwardAuditMaterial), ("pending", pending.PendingMaterial))),
    r.ReleaseSelection: ("record_resolver.ReleaseSelection", (
        ("index", int), ("release", str), ("record_sha", str), ("manifest_sha256", str), ("images", _PAIRS),
        ("actual_web_source_sha", str), ("reader", p.SourceFacts), ("live", _optional(p.LiveContract)))),
    r.ResolvedChain: ("record_resolver.ResolvedChain", (
        ("chain", p.PolicyChain), ("inventory", t.InventorySnapshot), ("image_bindings", _many(t.VerifiedImages)),
        ("latest", r.ReleaseSelection), ("selected", r.ReleaseSelection))),
    r.WebRecipe: ("record_resolver.WebRecipe", (("source_sha", str), ("path", str), ("raw", bytes), ("sha256", str))),
    t.InventorySnapshot: ("record_transport.InventorySnapshot", (
        ("repository_id", int), ("main_sha", str), ("records", _many(t.ReleaseIdentity)), ("tags", _many(t.TagIdentity)))),
    t.ReleaseIdentity: ("record_transport.ReleaseIdentity", (
        ("release", str), ("release_id", int), ("asset_id", int), ("size", int), ("digest", _optional(str)))),
    t.TagIdentity: ("record_transport.TagIdentity", (("release", str), ("sha", str))),
    t.VerifiedImages: ("record_transport.VerifiedImages", (
        ("release", str), ("record_sha", str), ("images", _PAIRS), ("source_shas", _PAIRS))),
    writer.CandidateImages: ("record_writer.CandidateImages", (("sha", str), ("images", _PAIRS), ("source_shas", _PAIRS))),
    writer.forward_gates.DrillGate: ("forward_gates.DrillGate", (
        ("required", bool), ("observed_at", datetime), ("source_sha256", _optional(str)),
        ("staging_restore", _optional(datetime)), ("production_fork", _optional(datetime)))),
    contract.LabelIdentity: ("contract_authorization.LabelIdentity", (
        ("node_id", str), ("repository_id", int), ("name", str),
        ("created_at", datetime), ("updated_at", datetime))),
    contract.LabelEvent: ("contract_authorization.LabelEvent", (
        ("node_id", str), ("kind", str), ("pull_node_id", str), ("label", contract.LabelIdentity),
        ("actor_type", str), ("actor_node_id", str), ("actor_login", str), ("occurred_at", datetime))),
    contract.MergedPull: ("contract_authorization.MergedPull", (
        ("node_id", str), ("number", int), ("repository_id", int), ("repository", str),
        ("base_ref", str), ("merge_sha", str), ("head_sha", str), ("merged", bool), ("draft", bool),
        ("merged_at", datetime), ("labels", _many(contract.LabelIdentity)),
        ("events", _many(contract.LabelEvent)))),
    contract.ContractObservation: ("contract_authorization.ContractObservation", (
        ("repository_id", int), ("repository", str), ("main_sha", str), ("owner_type", str),
        ("owner_node_id", str), ("owner_login", str), ("pulls", _many(contract.MergedPull)),
        ("observed_at", datetime), ("enumeration", str))),
    writer.PromotionHandoff: ("record_writer.PromotionHandoff", (
        ("resolution", r.ForwardResolution), ("collected", c.CollectedHistory),
        ("candidate_images", writer.CandidateImages), ("execution", writer.WorkflowExecution), ("facts", d.ManifestFacts),
        ("drills", writer.forward_gates.DrillGate),
        ("contract_observation", _optional(contract.ContractObservation)))),
    writer.WorkflowExecution: ("record_writer.WorkflowExecution", (
        ("repository", str), ("workflow", str), ("workflow_sha", str), ("run_id", int), ("run_attempt", int))),
    d.DocumentCatalog: ("release_record.DocumentCatalog", (
        ("catalog_version", int), ("min_reader_version", int), ("catalog_sha256", str),
        ("data_steps_sha256", str), ("seeds_sha256", str))),
    d.ManifestFacts: ("release_record.ManifestFacts", (
        ("schema_version", int), ("db_change", str), ("dormant", bool), ("reversible", bool),
        ("old_server_ok", _optional(bool)), ("catalog", _optional(d.DocumentCatalog)),
        ("compat", _optional(str)), ("compat_run_id", _optional(int)), ("staging", _optional(str)))),
    s.PolicyClassification: ("schema_change.PolicyClassification", (
        ("kind", str), ("dormant", bool), ("reasons", _STRINGS), ("summary", _STRINGS),
        ("migration_changes", _STRINGS), ("requirements", _STRINGS))),
}
_MISSING = {
    "active": e.MissingEvidence.ACTIVE, "never-started-proven": e.MissingEvidence.NEVER_STARTED,
    "started-incomplete": e.MissingEvidence.STARTED_INCOMPLETE, "artifact-expired": e.MissingEvidence.EXPIRED,
    "signing-failed": e.MissingEvidence.SIGNING_FAILED, "missing": e.MissingEvidence.MISSING,
    "unknown": e.MissingEvidence.UNKNOWN, "unqualified": e.MissingEvidence.UNQUALIFIED,
}


class _Budget:
    def __init__(self, blobs=None, *, version=6):
        self.version = version
        self.nodes = 0
        self.scalars = 0
        self.expanded_bytes = 0
        self.blobs = {} if blobs is None else blobs
        self.used = set()

    def visit(self, depth):
        self.nodes += 1
        _need(depth <= MAX_DEPTH and self.nodes <= MAX_NODES)

    def scalar(self, length):
        self.scalars += length
        _need(self.scalars <= MAX_RAW)

    def blob(self, raw):
        self.expanded_bytes += len(raw)
        _need(len(raw) <= MAX_BLOB and self.expanded_bytes <= MAX_EXPANDED_BYTES)


def _exact(row, keys):
    _need(type(row) is dict and row.keys() == set(keys))


def _b64(raw):
    return base64.b64encode(raw).decode("ascii")


def _unb64(text, maximum):
    _need(type(text) is str and len(text) <= (maximum + 2) // 3 * 4)
    try:
        raw = base64.b64decode(text, validate=True)
    except (ValueError, binascii.Error):
        raise HandoffRefusal("record-handoff-invalid") from None
    _need(len(raw) <= maximum and _b64(raw) == text)
    return raw


def _choice(value, schemas, encoding):
    for schema in schemas:
        if encoding:
            if type(value) is schema:
                return schema
        elif schema is type(None) and value is None:
            return schema
        elif schema in (str, bool, int) and type(value) is schema:
            return schema
        elif type(value) is dict and schema not in (str, bool, int, type(None)):
            tag = ("bytes" if schema is bytes else "utc" if schema is datetime
                   else "missing" if schema is e.MissingEvidence else _SCHEMAS[schema][0])
            if value.get("type") == tag:
                return schema
    raise HandoffRefusal("record-handoff-invalid")


def _value(value, schema, budget, depth, *, encoding):
    budget.visit(depth)
    if type(schema) is tuple:
        kind, *parts = schema
        if kind == "union":
            return _value(value, _choice(value, parts, encoding), budget, depth + 1, encoding=encoding)
        if encoding:
            _need(type(value) is tuple and len(value) <= MAX_NODES)
            rows = value
        else:
            _exact(value, ("type", "items")); _need(value["type"] == "tuple" and type(value["items"]) is list)
            rows = value["items"]
            _need(len(rows) <= MAX_NODES)
        _need(kind == "many" or (kind == "pair" and len(rows) == len(parts)))
        converted = [_value(item, parts[0] if kind == "many" else parts[index], budget,
                            depth + 1, encoding=encoding) for index, item in enumerate(rows)]
        return {"type": "tuple", "items": converted} if encoding else tuple(converted)
    if schema in (str, bool, int, type(None)):
        _need(type(value) is schema)
        if schema is str:
            _need(len(value) <= MAX_RAW)
            budget.scalar(len(value.encode("utf-8")))
        elif schema is int:
            _need(abs(value) <= MAX_INTEGER)
        return value
    if schema is bytes:
        if encoding:
            _need(type(value) is bytes and len(value) <= MAX_BLOB)
            budget.blob(value)
            digest = writer.common.sha256_bytes(value)
            _need(digest not in budget.blobs or budget.blobs[digest] == value)
            budget.blobs[digest] = value
            _need(len(budget.blobs) <= MAX_BLOBS)
            return {"type": "bytes", "sha256": digest}
        _exact(value, ("type", "sha256")); _need(value["type"] == "bytes")
        digest = value["sha256"]
        _need(type(digest) is str and writer.common.SHA256_RE.fullmatch(digest) and digest in budget.blobs)
        raw = budget.blobs[digest]; budget.blob(raw); budget.used.add(digest)
        return raw
    if schema is datetime:
        if encoding:
            _need(type(value) is datetime and value.tzinfo is timezone.utc and value.fold == 0)
            return {"type": "utc", "value": value.isoformat(timespec="microseconds").replace("+00:00", "Z")}
        _exact(value, ("type", "value")); text = value["value"]
        _need(value["type"] == "utc" and type(text) is str
              and re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z", text))
        result = datetime.fromisoformat(text[:-1] + "+00:00")
        _need(result.tzinfo is timezone.utc)
        return result
    if schema is e.MissingEvidence:
        if encoding:
            _need(type(value) is e.MissingEvidence and _MISSING.get(value.value) is value)
            return {"type": "missing", "value": value.value}
        _exact(value, ("type", "value"))
        _need(value["type"] == "missing" and type(value["value"]) is str and value["value"] in _MISSING)
        return _MISSING[value["value"]]
    tag, fields = _SCHEMAS[schema]
    if schema is r.ForwardResolution and budget.version in (1, 2, 3, 4):
        # Old versions never observed pending; v1 also predates source audit.
        # Literal omissions retain exact historical fields, never empty synthesis.
        fields = fields[:-2] if budget.version == 1 else fields[:-1]
    if schema is writer.PromotionHandoff and budget.version in (1, 2, 3, 4, 5):
        # Literal historical omission; absence never invents owner authority.
        fields = fields[:-1]
        if budget.version in (1, 2, 3):
            fields = fields[:-1]
    if encoding:
        _need(type(value) is schema)
        return {"type": tag, "fields": {name: _value(getattr(value, name), field, budget, depth + 1, encoding=True)
                                       for name, field in fields}}
    _exact(value, ("type", "fields")); _need(value["type"] == tag)
    _exact(value["fields"], (name for name, _ in fields))
    values = {name: _value(value["fields"][name], field, budget, depth + 1, encoding=False) for name, field in fields}
    # This constructor is exclusively a literal allowlisted class with its exact
    # field schema. NamedTuple and dataclass identities remain distinct.
    return schema(**values)


def _canonical(value):
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode("ascii")


def _pairs(items):
    result = {}
    for key, value in items:
        _need(key not in result)
        result[key] = value
    return result


def _invalid_constant(_value):
    raise HandoffRefusal("record-handoff-invalid")


def _integer(text):
    _need(len(text) <= 21)
    value = int(text); _need(abs(value) <= MAX_INTEGER)
    return value


def _scan(raw):
    # Before json.loads allocates containers, bound nesting and a conservative
    # structural-node count. JSON syntax itself is left to the strict parser.
    depth, nodes, quoted, escaped = 0, 1, False, False
    for byte in raw:
        if quoted:
            if escaped:
                escaped = False
            elif byte == 92:
                escaped = True
            elif byte == 34:
                quoted = False
        elif byte == 34:
            quoted = True
        elif byte in (123, 91):
            depth += 1; nodes += 1
        elif byte in (125, 93):
            depth -= 1
        elif byte in (44, 58):
            nodes += 1
        _need(0 <= depth <= MAX_DEPTH and nodes <= MAX_NODES)
    _need(not quoted and depth == 0)


def _replay(handoff, candidate_sha, workflow_sha, run_id, record_attempt, *, version):
    _need(type(candidate_sha) is str and writer.common.SHA1_RE.fullmatch(candidate_sha)
          and type(workflow_sha) is str and writer.common.SHA1_RE.fullmatch(workflow_sha)
          and type(run_id) is int and 1 <= run_id <= MAX_INTEGER
          and type(record_attempt) is int and 1 <= record_attempt <= MAX_INTEGER)
    _need(type(handoff) is writer.PromotionHandoff
          and handoff.candidate_images.sha == candidate_sha
          and handoff.execution == writer.WorkflowExecution(writer.common.REPOSITORY,
              d.SHIP_SIGNER, workflow_sha, run_id, 1))
    resolution = handoff.resolution
    if version == 1:
        _need(resolution.audit is None)
    else:
        _need(version in (2, 3, 4, 5, 6))
        r.validate_forward_audit(resolution.audit, candidate_sha=resolution.candidate.sha,
            database_base_source=resolution.database_base_source,
            latest_record_sha=resolution.resolved.latest.record_sha,
            selected_web_source_sha=resolution.resolved.selected.actual_web_source_sha)
    _need((version in (4, 5, 6) and type(handoff.drills) is writer.forward_gates.DrillGate)
          or (version in (1, 2, 3) and handoff.drills is None))
    if version != 6:
        _need(handoff.contract_observation is None)
    if version in (5, 6):
        _need(type(resolution.pending) is pending.PendingMaterial)
        rebuilt = writer.prepare_promotion(handoff.resolution, handoff.collected,
            handoff.candidate_images, handoff.execution, staging=handoff.facts.staging, drills=handoff.drills,
            contract_observation=handoff.contract_observation)
    else:
        _need(resolution.pending is None)
        rebuilt = writer._prepare_promotion_context2_custody(handoff.resolution, handoff.collected,
            handoff.candidate_images, handoff.execution, staging=handoff.facts.staging, drills=handoff.drills)
    _need(rebuilt == handoff)
    return handoff


def _length(value, maximum):
    _need(type(value) is str and len(value) <= 10 and re.fullmatch(r"[1-9][0-9]*", value))
    number = int(value); _need(number <= maximum)
    return number


def _inflate(compressed, maximum):
    inflater = zlib.decompressobj()
    raw = inflater.decompress(compressed, maximum + 1)
    _need(len(raw) <= maximum and inflater.eof and not inflater.unused_data and not inflater.unconsumed_tail)
    return raw


def _xz_module():
    # Optional standard-library support is needed only for the new wire. A
    # runtime without liblzma must still decode historical v1/v2 custody.
    try:
        import lzma
    except ImportError:
        raise HandoffRefusal("record-handoff-invalid") from None
    _need(lzma.is_check_supported(lzma.CHECK_CRC64))
    return lzma


def _xz_compress(raw):
    xz = _xz_module()
    try:
        return xz.compress(raw, format=xz.FORMAT_XZ, check=xz.CHECK_CRC64, preset=6)
    except xz.LZMAError:
        raise HandoffRefusal("record-handoff-invalid") from None


def _xz_inflate(compressed, maximum):
    xz = _xz_module()
    try:
        inflater = xz.LZMADecompressor(format=xz.FORMAT_XZ, memlimit=XZ_MEMORY_LIMIT)
        raw = inflater.decompress(compressed, max_length=maximum + 1)
    except xz.LZMAError:
        raise HandoffRefusal("record-handoff-invalid") from None
    _need(len(raw) <= maximum and inflater.eof and not inflater.unused_data
          and inflater.check == xz.CHECK_CRC64)
    return raw


def _frame(encoded, blobs, *, version=6):
    # Only bytes are deduplicated. Every typed node and history row remains in
    # the canonical header; raw blobs follow in exact digest order. The outer
    # metadata binds the complete frame's size and SHA256, including this prefix.
    rows = sorted(blobs.items())
    _need(type(version) is int and version in (3, 4, 5, 6))
    header = _canonical({"version": version, "handoff": encoded,
        "blobs": [{"sha256": digest, "length": len(raw)} for digest, raw in rows]})
    size = 4 + len(header) + sum(len(raw) for _, raw in rows)
    _need(0 < len(header) <= MAX_RAW - 4 and size <= MAX_RAW)
    return len(header).to_bytes(4, "big") + header + b"".join(raw for _, raw in rows)


def _json(raw):
    _scan(raw)
    value = json.loads(raw, object_pairs_hook=_pairs, parse_int=_integer,
                       parse_float=_invalid_constant, parse_constant=_invalid_constant)
    _exact(value, ("version", "handoff", "blobs"))
    _need(_canonical(value) == raw)
    return value


def _unframe(raw, *, version=6):
    _need(4 < len(raw) <= MAX_RAW)
    size = int.from_bytes(raw[:4], "big")
    _need(0 < size <= MAX_RAW - 4 and 4 + size <= len(raw))
    value = _json(raw[4:4 + size])
    _need(type(value["version"]) is int and value["version"] == version and version in (3, 4, 5, 6))
    rows = value["blobs"]
    _need(type(rows) is list and 1 <= len(rows) <= MAX_BLOBS)
    offset, total, previous, blobs = 4 + size, 0, "", {}
    for row in rows:
        _exact(row, ("sha256", "length"))
        digest, length = row["sha256"], row["length"]
        _need(type(digest) is str and writer.common.SHA256_RE.fullmatch(digest)
              and digest > previous and type(length) is int and 0 <= length <= MAX_BLOB
              and total + length <= MAX_EXPANDED_BYTES and offset + length <= len(raw))
        blob = raw[offset:offset + length]
        _need(writer.common.sha256_bytes(blob) == digest)
        blobs[digest] = blob
        offset += length; total += length; previous = digest
    _need(offset == len(raw))
    return value, blobs


def _decode_blobs(rows):
    # Bytes-only references: never an object graph or a way to omit evidence.
    _need(type(rows) is list and 1 <= len(rows) <= MAX_BLOBS)
    blobs, total, previous = {}, 0, ""
    for row in rows:
        _exact(row, ("sha256", "length", "value"))
        digest, length = row["sha256"], row["length"]
        _need(type(digest) is str and writer.common.SHA256_RE.fullmatch(digest)
              and digest > previous and type(length) is int and 0 <= length <= MAX_BLOB
              and total + length <= MAX_EXPANDED_BYTES)
        # Declared size and remaining aggregate are checked before allocation;
        # the inflater can produce at most one detection byte beyond that size.
        compressed = _unb64(row["value"], MAX_RAW)
        raw = _inflate(compressed, length)
        _need(len(raw) == length and writer.common.sha256_bytes(raw) == digest)
        blobs[digest] = raw
        total += length; previous = digest
    return blobs


def _unpack(outputs):
    _exact(outputs, OUTPUT_KEYS)
    _need(all(type(value) is str and len(value) <= CHUNK_SIZE and value.isascii() for value in outputs.values())
          and outputs["handoff_version"] in ("1", "2", "3", "4", "5", VERSION)
          and writer.common.SHA256_RE.fullmatch(outputs["handoff_sha256"]))
    encoded_size = _length(outputs["handoff_encoded_length"], MAX_ENCODED)
    compressed_size = _length(outputs["handoff_compressed_length"], MAX_COMPRESSED)
    raw_size = _length(outputs["handoff_raw_length"], MAX_RAW)
    count = _length(outputs["handoff_chunk_count"], len(CHUNK_SLOTS))
    _need(count == (encoded_size + CHUNK_SIZE - 1) // CHUNK_SIZE)
    for index, key in enumerate(CHUNK_SLOTS):
        expected = CHUNK_SIZE if index < count - 1 else encoded_size - CHUNK_SIZE * index if index == count - 1 else 0
        _need(len(outputs[key]) == expected)
    text = "".join(outputs[key] for key in CHUNK_SLOTS)
    _need(len(text) == encoded_size)
    compressed = _unb64(text, MAX_COMPRESSED)
    _need(len(compressed) == compressed_size)
    raw = (_xz_inflate(compressed, raw_size) if outputs["handoff_version"] in ("3", "4", "5", "6")
           else _inflate(compressed, raw_size))
    _need(len(raw) == raw_size
          and writer.common.sha256_bytes(raw) == outputs["handoff_sha256"])
    return raw


@_public
def decode(outputs, *, candidate_sha, workflow_sha, run_id, record_attempt):
    """Check transport and pure replay, requiring external same-run custody.

    record_attempt may increase on a record-only rerun; original production
    execution stays attempt1. V1 decodes only historical record custody with
    audit=None; it cannot be encoded again or supply future operational gates.
    V2 retains its exact zlib/JSON wire; v3 retains XZ with no drill observation.
    V4 retains its original drill observation. V5 additionally requires complete
    raw pending material and current context3 evidence. Historical versions
    1-4 cannot enter current normal preparation. V6 preserves complete owner
    observations when required; historical absence never supplies one.
    Current active contract qualification remains unsupported.
    A hash cannot authenticate hostile substituted data.
    """
    return _decode_versioned(outputs, candidate_sha=candidate_sha, workflow_sha=workflow_sha,
        run_id=run_id, record_attempt=record_attempt)[1]


def _decode_versioned(outputs, *, candidate_sha, workflow_sha, run_id, record_attempt):
    raw = _unpack(outputs)
    version = int(outputs["handoff_version"])
    if version in (3, 4, 5, 6):
        value, blobs = _unframe(raw, version=version)
    else:
        value = _json(raw)
        blobs = _decode_blobs(value["blobs"])
    _need(type(value["version"]) is int and value["version"] == version)
    budget = _Budget(blobs, version=version)
    handoff = _value(value["handoff"], writer.PromotionHandoff, budget, 0, encoding=False)
    _need(budget.used == budget.blobs.keys())
    return version, _replay(handoff, candidate_sha, workflow_sha, run_id, record_attempt, version=version)


@_public
def build_record(outputs, *, candidate_sha, workflow_sha, run_id, record_attempt,
                 publication, current_resolution):
    """Preserve validated wire version through the closed record-only replay.

    External same-run production custody and a freshly authenticated exact
    current parent remain caller obligations. No flag selects a legacy policy.
    """
    version, handoff = _decode_versioned(outputs, candidate_sha=candidate_sha,
        workflow_sha=workflow_sha, run_id=run_id, record_attempt=record_attempt)
    if version in (5, 6):
        return writer.build_promotion(handoff, publication, current_resolution)
    return writer._build_promotion_context2_custody(handoff, publication, current_resolution)


@_public
def encode(handoff):
    """Return a complete output mapping only after bounded roundtrip/replay.

    Precompute this before provider effects; emit it only after production
    success. Otherwise-valid histories exceeding capacity refuse, never truncate.
    """
    budget = _Budget()
    encoded = _value(handoff, writer.PromotionHandoff, budget, 0, encoding=True)
    raw = _frame(encoded, budget.blobs)
    compressed = _xz_compress(raw)
    _need(0 < len(compressed) <= MAX_COMPRESSED)
    text = _b64(compressed); _need(len(text) <= MAX_ENCODED)
    count = (len(text) + CHUNK_SIZE - 1) // CHUNK_SIZE
    outputs = {"handoff_version": VERSION, "handoff_sha256": writer.common.sha256_bytes(raw),
        "handoff_raw_length": str(len(raw)), "handoff_compressed_length": str(len(compressed)),
        "handoff_encoded_length": str(len(text)), "handoff_chunk_count": str(count)}
    outputs.update((key, text[i * CHUNK_SIZE:(i + 1) * CHUNK_SIZE]) for i, key in enumerate(CHUNK_SLOTS))
    for key, item in outputs.items():
        _need(writer.common.OUTPUT_KEY_RE.fullmatch(key) and writer.common.OUTPUT_VALUE_RE.fullmatch(item))
        if item:
            writer.common.require_public_text(item)
    restored = decode(outputs, candidate_sha=handoff.candidate_images.sha,
        workflow_sha=handoff.execution.workflow_sha, run_id=handoff.execution.run_id, record_attempt=1)
    _need(restored == handoff)
    return outputs
