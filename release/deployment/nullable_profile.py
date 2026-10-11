"""Pure source binding then complete nullable-observation comparison.

These are consistency data, not Git/signature, image, history, or release
capabilities. A future concrete caller must independently authenticate retained
origin and candidate source. No active classifier or runtime caller uses this
module. Source golden and production-v0 remain distinct catalog contracts.
"""
from dataclasses import dataclass, field
import re

import catalog_semantics as wire
import catalog_semantics_v2 as semantic
import pending_plan as catalog_format

MAX_DOCUMENT = 4 << 20
MAX_TOTAL = 8 << 20
MAX_DESCRIPTOR = 16 << 10
MAX_RESULT = 16 << 10
PROFILE = "nullable-int8-no-default-pg17-observation2"
DEFAULT_ACL_SHA256 = "6f51817c3516988a08b66193fedbb00a375bf8a23459c297593af3f675b37c53"
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_RELEASE = re.compile(r"prod-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}\Z")
_PATH = re.compile(r"[A-Za-z0-9_./-]{1,256}\Z")
_SLOTS = ("profile", "operation", "before_catalog", "after_catalog")


class ProfileRefusal(ValueError):
    def __init__(self, code):
        # All call sites use these fixed labels; never interpolate input.
        if code not in {"format", "bound", "source", "identity", "descriptor",
                        "catalog", "operation", "expected", "semantic", "holder", "result"}:
            code = "format"
        self.code = "nullable-profile:" + code
        super().__init__(self.code)


def _need(condition, code="format"):
    if not condition:
        raise ProfileRefusal(code)


def _fixed(function, *args):
    try:
        return function(*args)
    except ProfileRefusal:
        raise
    except (wire.SemanticRefusal, catalog_format.PendingRefusal):
        raise ProfileRefusal("semantic") from None
    except (ValueError, TypeError, KeyError, AttributeError, RecursionError,
            OverflowError, UnicodeError):
        raise ProfileRefusal("format") from None


@dataclass(frozen=True, slots=True)
class SourceBlob:
    source_sha: str
    path: str
    raw: bytes = field(repr=False)


@dataclass(frozen=True, slots=True)
class CatalogCoordinates:
    version: int
    min_reader: int
    catalog_sha256: str


@dataclass(frozen=True, slots=True)
class RetainedOrigin:
    release: str
    manifest_sha256: str
    source_sha: str
    catalog: CatalogCoordinates


@dataclass(frozen=True, slots=True)
class CandidateSource:
    source_sha: str
    catalog: CatalogCoordinates


@dataclass(frozen=True, slots=True)
class ProfileMaterial:
    profile: SourceBlob
    operation: SourceBlob
    before_catalog: SourceBlob
    after_catalog: SourceBlob


@dataclass(frozen=True, slots=True, init=False, repr=False)
class BoundNullableProfile:
    _origin: bytes
    _candidate: bytes
    _sources: bytes
    _profile: bytes
    _operation: bytes
    _before: bytes
    _after: bytes

    def __init__(self):
        raise ProfileRefusal("holder")


@dataclass(frozen=True, slots=True, init=False, repr=False)
class NullableProfileExplanation:
    _profile: BoundNullableProfile
    _before: bytes
    _after: bytes
    _raw: bytes

    def __init__(self):
        raise ProfileRefusal("holder")


def _opaque(cls, **values):
    result = object.__new__(cls)
    for key, value in values.items():
        object.__setattr__(result, key, value)
    return result


def _sha(value):
    return type(value) is str and _SHA.fullmatch(value) is not None


def _path(value):
    return (type(value) is str and _PATH.fullmatch(value) is not None
            and all(part not in ("", ".", "..") for part in value.split("/")))


def _fields(value, keys):
    _need(type(value) is dict and set(value) == set(keys))
    return value


def _bytes(raw, limit):
    _need(type(raw) is bytes and 0 < len(raw) <= limit, "bound")


def _budget(raws, observations=()):
    _need(type(raws) is tuple and len(raws) == 4
          and type(observations) is tuple and len(observations) in (0, 2), "bound")
    for raw, limit in zip(raws, (MAX_DESCRIPTOR, wire.MAX_OPERATION,
                               MAX_DOCUMENT, MAX_DOCUMENT)):
        _bytes(raw, limit)
    for raw in observations:
        _bytes(raw, MAX_DOCUMENT)
    # Every role slot is counted, even when it contains the very same bytes.
    _need(sum(len(raw) for raw in raws + observations) <= MAX_TOTAL, "bound")


def _coordinates(value):
    _need(type(value) is CatalogCoordinates and wire._integer(value.version)
          and wire._integer(value.min_reader) and value.min_reader <= value.version
          and wire._hash(value.catalog_sha256), "identity")
    return {"catalog_version": value.version, "min_reader": value.min_reader,
            "catalog_sha256": value.catalog_sha256}


def _identities(origin, candidate):
    _need(type(origin) is RetainedOrigin and type(candidate) is CandidateSource,
          "identity")
    _need(type(origin.release) is str and _RELEASE.fullmatch(origin.release)
          and wire._hash(origin.manifest_sha256) and _sha(origin.source_sha)
          and _sha(candidate.source_sha), "identity")
    old = {"release": origin.release, "manifest_sha256": origin.manifest_sha256,
           "source_sha": origin.source_sha, **_coordinates(origin.catalog)}
    new = {"source_sha": candidate.source_sha, **_coordinates(candidate.catalog)}
    return old, new


def _blob(value):
    _need(type(value) is SourceBlob and _sha(value.source_sha) and _path(value.path)
          and type(value.raw) is bytes, "source")
    return {"source_sha": value.source_sha, "path": value.path,
            "bytes": len(value.raw), "sha256": wire._digest(value.raw)}


def _catalog(raw):
    # Reuse complete parent/reference/attribute facts, without manufacturing a
    # Snapshot's security, counts, ownership or unknown-object observations.
    value = wire._load(raw, MAX_DOCUMENT, canonical=False)
    try:
        catalog_format._catalog(value, 0)
    except catalog_format.PendingRefusal:
        raise ProfileRefusal("catalog") from None
    _need(type(value["version"]) is int and value["version"] == 0, "catalog")
    objects = value["objects"]
    _need([(o["kind"], o["identity"]) for o in objects]
          == sorted((o["kind"], o["identity"]) for o in objects), "catalog")
    for obj in objects:
        _need("parent" not in obj or obj["parent"] != "", "catalog")
        for key, text in obj["attributes"].items():
            if catalog_format._ATTRS[obj["kind"]][key] == "int":
                _need(re.fullmatch(r"0|-?[1-9][0-9]*", text) is not None
                      and -(2**63) <= int(text) < 2**63, "catalog")
    _need(wire._catalog_bytes(objects) == raw, "catalog")
    return value


def _reference(row, blob, role, extra):
    _fields(row, ("role", "path", "bytes", "sha256", *extra))
    _need(row["role"] == role and row["path"] == blob.path
          and _path(row["path"]) and wire._integer(row["bytes"], 1)
          and row["bytes"] == len(blob.raw) and wire._hash(row["sha256"])
          and row["sha256"] == wire._digest(blob.raw), "descriptor")


def _bind(origin, candidate, material):
    _need(type(material) is ProfileMaterial, "source")
    blobs = tuple(getattr(material, name) for name in _SLOTS)
    _need(all(type(blob) is SourceBlob for blob in blobs), "source")
    raws = tuple(blob.raw for blob in blobs)
    _budget(raws)  # Before any document is decoded.
    old, new = _identities(origin, candidate)
    sources = dict(zip(_SLOTS, (_blob(blob) for blob in blobs)))
    _need(material.profile.source_sha == candidate.source_sha
          and material.operation.source_sha == candidate.source_sha
          and material.after_catalog.source_sha == candidate.source_sha
          and material.before_catalog.source_sha == origin.source_sha, "source")
    seen = {}
    for blob in blobs:
        key = (blob.source_sha, blob.path)
        _need(key not in seen or seen[key] == blob.raw, "source")
        seen[key] = blob.raw
    descriptor = _fields(wire._load(material.profile.raw, MAX_DESCRIPTOR),
        ("format_version", "profile", "postgres_major", "observation_version",
         "default_acl_contract_sha256", "renderer", "operation", "before", "after"))
    _need(type(descriptor["format_version"]) is int and descriptor["format_version"] == 1
          and descriptor["profile"] == PROFILE
          and type(descriptor["postgres_major"]) is int and descriptor["postgres_major"] == 17
          and type(descriptor["observation_version"]) is int and descriptor["observation_version"] == 2
          and descriptor["default_acl_contract_sha256"] == DEFAULT_ACL_SHA256
          and descriptor["renderer"] == "nullable-column-v1", "descriptor")
    op_ref = descriptor["operation"]
    _reference(op_ref, material.operation, "candidate", ("id", "statement_sha256"))
    operation = semantic.parse_operation(material.operation.raw)
    op = wire._operation(operation.raw)
    _need(op_ref["id"] == op["id"] and wire._hash(op_ref["statement_sha256"])
          and op_ref["statement_sha256"] == wire._digest(semantic.render_statement(operation)),
          "operation")
    catalogs = []
    for label, blob, role, coordinates in (
            ("before", material.before_catalog, "retained_origin", origin.catalog),
            ("after", material.after_catalog, "candidate", candidate.catalog)):
        row = descriptor[label]
        _reference(row, blob, role, ("catalog_wire_version", "catalog_version",
                                    "min_reader", "catalog_sha256"))
        _need(type(row["catalog_wire_version"]) is int and row["catalog_wire_version"] == 0
              and wire._integer(row["catalog_version"]) and wire._integer(row["min_reader"])
              and {key: row[key] for key in _coordinates(coordinates)} == _coordinates(coordinates)
              and row["catalog_sha256"] == wire._digest(blob.raw), "identity")
        catalogs.append(_catalog(blob.raw))
    # Binding does not infer an actual transition; explanation checks all raw
    # observations and expected objects in a separate stage.
    bound = _opaque(BoundNullableProfile, _origin=wire._json_bytes(old),
        _candidate=wire._json_bytes(new), _sources=wire._json_bytes(sources),
        _profile=raws[0], _operation=raws[1], _before=raws[2], _after=raws[3])
    return bound, old, new, sources, operation, catalogs


def bind_source_profile(origin, candidate, material):
    return _fixed(_bind, origin, candidate, material)[0]


def _unpack(profile, observations):
    _need(type(profile) is BoundNullableProfile, "holder")
    raws = (profile._profile, profile._operation, profile._before, profile._after)
    _budget(raws, observations)
    for raw in (profile._origin, profile._candidate, profile._sources):
        _bytes(raw, MAX_DESCRIPTOR)
    old = _fields(wire._load(profile._origin, MAX_DESCRIPTOR),
        ("release", "manifest_sha256", "source_sha", "catalog_version", "min_reader", "catalog_sha256"))
    new = _fields(wire._load(profile._candidate, MAX_DESCRIPTOR),
        ("source_sha", "catalog_version", "min_reader", "catalog_sha256"))
    sources = _fields(wire._load(profile._sources, MAX_DESCRIPTOR), _SLOTS)
    blobs = []
    for name, raw in zip(_SLOTS, raws):
        row = _fields(sources[name], ("source_sha", "path", "bytes", "sha256"))
        _need(wire._integer(row["bytes"], 1) and row["bytes"] == len(raw)
              and wire._hash(row["sha256"]) and row["sha256"] == wire._digest(raw), "source")
        blobs.append(SourceBlob(row["source_sha"], row["path"], raw))
    origin = RetainedOrigin(old["release"], old["manifest_sha256"], old["source_sha"],
        CatalogCoordinates(old["catalog_version"], old["min_reader"], old["catalog_sha256"]))
    candidate = CandidateSource(new["source_sha"],
        CatalogCoordinates(new["catalog_version"], new["min_reader"], new["catalog_sha256"]))
    result = _bind(origin, candidate, ProfileMaterial(*blobs))
    _need(result[0] == profile, "holder")
    return result


def _explain(profile, before, after):
    bound, old, new, sources, operation, catalogs = _unpack(profile, (before, after))
    left, right = semantic.parse_observation(before), semantic.parse_observation(after)
    for observation, expected, catalog_hash in (
            (left, catalogs[0], old["catalog_sha256"]),
            (right, catalogs[1], new["catalog_sha256"])):
        # Parsing above establishes the real graph/security/counts independently.
        actual = wire._load(observation.raw, MAX_DOCUMENT)["snapshot"]
        _need(actual["objects"] == expected["objects"]
              and actual["sha256"] == catalog_hash, "expected")
    semantic_result = semantic.canonical_explanation(
        semantic.explain_transition(operation, left, right))
    value = {"format_version": 1, "purpose": "source-bound-nullable-explanation",
             "profile": PROFILE, "origin": old, "candidate": new,
             "source": sources, "semantic": wire._load(semantic_result, wire.MAX_OPERATION)}
    raw = wire._json_bytes(value)
    _bytes(raw, MAX_RESULT)
    return _opaque(NullableProfileExplanation, _profile=bound,
                   _before=before, _after=after, _raw=raw)


def explain_profile(profile, *, before, after):
    return _fixed(_explain, profile, before, after)


def _canonical(explanation):
    _need(type(explanation) is NullableProfileExplanation, "holder")
    _bytes(explanation._raw, MAX_RESULT)
    recomputed = _explain(explanation._profile, explanation._before, explanation._after)
    _need(recomputed._raw == explanation._raw, "result")
    return recomputed._raw


def canonical_explanation(value):
    return _fixed(_canonical, value)
