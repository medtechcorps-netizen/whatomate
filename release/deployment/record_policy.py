"""Pure release-record policy preparation; no signature or execution authority.

All inputs, including SourceFacts and RecordInput, are data contracts. A caller
must authenticate complete raw records, images, immutable source blobs and Git
ancestry through the existing adapters before using these calculations. This
module does not perform I/O, replace those adapters, or enable the v2 writer.

The source decoder supports only reviewed v0 and explicit legacy reader0.
Synthetic future facts exercise the policy separately and mark every resulting
chain/decision as unsupported by the current runtime source decoder.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
from typing import NamedTuple

import release_record as records
import schema_change
import ship_common as common


GOLDEN = "internal/dbcatalog/golden/catalog.json"
HISTORY = "internal/dbcatalog/golden/history.json"
GLOBALS = "internal/dbcatalog/golden/global_tables.json"
PROFILE = "internal/dbcatalog/golden/production-v0.json"
OVERLAY = "internal/dbcatalog/shape/production-v0.sql"
SEEDS = "internal/dbcatalog/golden/seeds.json"
STEPS = "internal/dbcatalog/registry/data_steps.json"
SOURCE_PATHS = tuple(sorted((GOLDEN, HISTORY, GLOBALS, PROFILE, OVERLAY, SEEDS, STEPS)))
# These are the already-reviewed v0 source contract, not an arbitrary schema
# version registry. Future source formats require a separate reviewed decoder.
V0_PINS = (
    (GOLDEN, "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8"),
    (HISTORY, "9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513"),
    (GLOBALS, "c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3"),
    (PROFILE, "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d"),
    (OVERLAY, "48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d"),
)
V0_MEASUREMENT = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"
V0_RELEASED_SOURCE = "e0016b3595114728e7864da2adb44c29065f25ef"
MAX_SOURCE_BYTES = 8 * 1024 * 1024
MAX_SOURCE_FILE_BYTES = 4 * 1024 * 1024


class PolicyRefusal(Exception):
    """Only fixed module-owned codes; never raw source/document values."""


def _need(condition, code="record-policy-invalid"):
    if not condition:
        raise PolicyRefusal(code)


def _sha(value):
    return type(value) is str and common.SHA1_RE.fullmatch(value) is not None


def _hash(value):
    return type(value) is str and common.SHA256_RE.fullmatch(value) is not None


def _integer(value):
    return type(value) is int and 0 <= value <= 2_147_483_647


def _digest(raw):
    return hashlib.sha256(raw).hexdigest()


class SourceBlob(NamedTuple):
    path: str
    raw: bytes


class HistoryVersion(NamedTuple):
    version: int
    minimum: int
    kind: str


@dataclass(frozen=True, slots=True)
class SourceFacts:
    sha: str
    reader_version: int
    catalog: records.DocumentCatalog | None
    artifacts: tuple[tuple[str, str], ...]
    history: tuple[HistoryVersion, ...]
    mode: str  # reviewed-v0, legacy-reader0, or synthetic-policy


@dataclass(frozen=True, slots=True)
class RecordInput:
    raw: bytes
    manifest_sha256: str
    web_source_sha: str


@dataclass(frozen=True, slots=True)
class LegacyBaseline:
    """Explicit complete baseline assertion from a future trusted adapter.

    Its exact last-v1 identity and full source facts are checked below. Merely
    constructing this object does not prove the legacy live database shape.
    There is no fallback from absent metadata in an old image's source.
    """
    release: str
    manifest_sha256: str
    source_sha: str


@dataclass(frozen=True, slots=True)
class LiveContract:
    catalog: records.DocumentCatalog
    origin_release: str
    origin_manifest_sha256: str
    origin_source: SourceFacts


@dataclass(frozen=True, slots=True)
class PolicyEntry:
    release: str
    kind: str
    sha: str
    images: tuple[tuple[str, str], ...]
    deployed_at: str
    previous: str | None
    rolled_back_from: str | None
    record: RecordInput
    facts: records.ManifestFacts
    reader: SourceFacts
    live: LiveContract | None


@dataclass(frozen=True, slots=True)
class PolicyChain:
    entries: tuple[PolicyEntry, ...]
    inputs: tuple[RecordInput, ...]
    sources: tuple[SourceFacts, ...]
    ancestors: tuple[tuple[str, str], ...]
    baseline: LegacyBaseline | None
    runtime_source_supported: bool


@dataclass(frozen=True, slots=True)
class RollbackRequirements:
    """Checked span data, without compatibility or image-only permission.

    The source resolver applies its separate supported-material restrictions.
    This pure shape can describe synthetic future policy without claiming that
    the current source decoder or an actual experiment supports that policy.
    """
    target: PolicyEntry
    intermediates: tuple[PolicyEntry, ...]
    non_none_count: int
    live: LiveContract
    runtime_source_supported: bool
    span: tuple[tuple[str, str], ...]
    span_sha256: str


@dataclass(frozen=True, slots=True)
class RollbackDecision:
    target: PolicyEntry
    intermediates: tuple[PolicyEntry, ...]
    non_none_count: int
    live: LiveContract
    image_only_facts: records.ManifestFacts
    runtime_source_supported: bool


def _catalog(value):
    _need(type(value) is records.DocumentCatalog and _integer(value.catalog_version)
          and _integer(value.min_reader_version) and value.min_reader_version <= value.catalog_version)
    _need(all(_hash(getattr(value, key)) for key in
              ("catalog_sha256", "data_steps_sha256", "seeds_sha256")))


def _source(value):
    _need(type(value) is SourceFacts and _sha(value.sha) and _integer(value.reader_version))
    _need(type(value.artifacts) is tuple and type(value.history) is tuple)
    _need(type(value.mode) is str and value.mode in {"reviewed-v0", "legacy-reader0", "synthetic-policy"})
    if value.mode == "legacy-reader0":
        _need(value.reader_version == 0 and value.catalog is None and not value.artifacts and not value.history)
        return value
    _catalog(value.catalog)
    _need(value.reader_version == value.catalog.catalog_version and len(value.artifacts) == len(SOURCE_PATHS))
    _need(all(type(pair) is tuple and len(pair) == 2 and type(pair[0]) is str and _hash(pair[1])
              for pair in value.artifacts))
    _need(tuple(pair[0] for pair in value.artifacts) == SOURCE_PATHS)
    artifacts = dict(value.artifacts)
    _need(artifacts[SEEDS] == value.catalog.seeds_sha256
          and artifacts[STEPS] == value.catalog.data_steps_sha256)
    _need(1 <= len(value.history) <= 1024)
    prior = None
    for step in value.history:
        _need(type(step) is HistoryVersion and _integer(step.version) and _integer(step.minimum)
              and step.minimum <= step.version and type(step.kind) is str
              and step.kind in {"baseline", "expand", "contract"})
        if prior is None:
            _need(step == HistoryVersion(0, 0, "baseline"))
        else:
            _need(step.version > prior.version and step.minimum >= prior.minimum and step.kind != "baseline")
            _need(step.minimum == prior.minimum or step.kind == "contract")
        prior = step
    _need(prior.version == value.catalog.catalog_version and prior.minimum == value.catalog.min_reader_version)
    if value.mode == "reviewed-v0":
        _need(value.reader_version == 0 and value.history == (HistoryVersion(0, 0, "baseline"),)
              and value.catalog.catalog_sha256 == V0_MEASUREMENT
              and all(artifacts[name] == digest for name, digest in V0_PINS), "record-source-unsupported")
    return value


def reviewed_v0_source(sha: str, blobs: tuple[SourceBlob, ...]) -> SourceFacts:
    """Decode exact supplied bytes; caller must bind them to authenticated Git.

    No reads, signatures, image inspection or commit verification happen here.
    In particular this does not grant authority to a manually supplied SHA.
    """
    _need(_sha(sha) and type(blobs) is tuple and len(blobs) == len(SOURCE_PATHS), "record-source-invalid")
    _need(all(type(blob) is SourceBlob and type(blob.path) is str and type(blob.raw) is bytes
              and 0 < len(blob.raw) <= MAX_SOURCE_FILE_BYTES for blob in blobs), "record-source-invalid")
    _need(tuple(blob.path for blob in blobs) == SOURCE_PATHS
          and sum(len(blob.raw) for blob in blobs) <= MAX_SOURCE_BYTES, "record-source-invalid")
    raw = {blob.path: blob.raw for blob in blobs}
    artifacts = tuple((name, _digest(raw[name])) for name in SOURCE_PATHS)
    hashes = dict(artifacts)
    _need(all(hashes[name] == digest for name, digest in V0_PINS), "record-source-unsupported")
    try:
        profile = common.loads_strict(raw[PROFILE])
        schema_change._registry(common.loads_strict(raw[STEPS]))
        schema_change._seeds(common.loads_strict(raw[SEEDS]))
    except (common.ReleaseError, ValueError, TypeError, RecursionError):
        raise PolicyRefusal("record-source-invalid") from None
    _need(profile["released_source"] == V0_RELEASED_SOURCE and profile["live_sha256"] == V0_MEASUREMENT,
          "record-source-unsupported")
    return _source(SourceFacts(sha, 0, records.DocumentCatalog(0, 0, V0_MEASUREMENT,
                   hashes[STEPS], hashes[SEEDS]), artifacts, (HistoryVersion(0, 0, "baseline"),), "reviewed-v0"))


def legacy_reader0(sha: str) -> SourceFacts:
    """Explicit historical reader data, not a complete legacy baseline adapter."""
    return _source(SourceFacts(sha, 0, None, (), (), "legacy-reader0"))


def _document(item, *, bootstrap=False):
    _need(type(item) is RecordInput and type(item.raw) is bytes and 0 < len(item.raw) <= records.MAX_MANIFEST_BYTES
          and _hash(item.manifest_sha256) and _digest(item.raw) == item.manifest_sha256 and _sha(item.web_source_sha))
    try:
        value = common.loads_strict(item.raw)
        if bootstrap:
            _need(item.manifest_sha256 == records.BOOTSTRAP_SHA256, "record-policy-bootstrap")
            records.validate_bootstrap(value)
        else:
            records.validate_manifest(value)
        _need(item.raw == common.canonical_file_bytes(value), "record-policy-canonical")
    except (common.ReleaseError, ValueError, TypeError, RecursionError):
        raise PolicyRefusal("record-policy-document") from None
    return value


def _progress(prior: LiveContract, source: SourceFacts, facts: records.ManifestFacts):
    old, new = prior.catalog, source.catalog
    _need(new.catalog_version >= old.catalog_version and new.min_reader_version >= old.min_reader_version,
          "record-policy-catalog-regression")
    old_history = prior.origin_source.history
    _need(source.history[:len(old_history)] == old_history, "record-policy-history")
    if new.catalog_version == old.catalog_version:
        _need(source.history == old_history and new.catalog_sha256 == old.catalog_sha256,
              "record-policy-version")
        old_artifacts, new_artifacts = dict(prior.origin_source.artifacts), dict(source.artifacts)
        _need(all(old_artifacts[path] == new_artifacts[path] for path, _ in V0_PINS), "record-policy-version")
    else:
        _need(not facts.dormant and facts.db_change in {"expand", "contract"}, "record-policy-version")
    if new.min_reader_version > old.min_reader_version:
        _need(facts.db_change == "contract", "record-policy-reader-floor")
    if facts.compat == "bypassed":
        _need(new == old and source.artifacts == prior.origin_source.artifacts, "record-policy-bypass-contract")


def normalize_chain(inputs: tuple[RecordInput, ...], sources: tuple[SourceFacts, ...],
                    ancestors: tuple[tuple[str, str], ...], *, baseline: LegacyBaseline | None = None) -> PolicyChain:
    """Replay claims and source facts without authenticating them or fetching history.

    Historical v2 records are prior signed policy assertions at the caller's
    trust boundary, not reconstructed target/attempt proofs. No old compatible
    Actions artifact is needed and duplicate-image records are not collapsed.
    """
    _need(type(inputs) is tuple and 1 <= len(inputs) <= records.MAX_RECORDS + 1)
    _need(type(sources) is tuple and 1 <= len(sources) <= 2 * (records.MAX_RECORDS + 1))
    source_map = {}
    for source in sources:
        _source(source)
        _need(source.sha not in source_map, "record-policy-source-duplicate")
        source_map[source.sha] = source
    _need(type(ancestors) is tuple and len(ancestors) <= records.MAX_RECORDS)
    _need(all(type(pair) is tuple and len(pair) == 2 and all(_sha(sha) for sha in pair) for pair in ancestors))
    _need(len(set(ancestors)) == len(ancestors))
    _need(baseline is None or (type(baseline) is LegacyBaseline and type(baseline.release) is str
          and _hash(baseline.manifest_sha256) and _sha(baseline.source_sha)))
    documents = tuple(_document(item, bootstrap=index == 0) for index, item in enumerate(inputs))
    boot = documents[0]
    entries = []
    tags = set()
    seen_v2 = False
    baseline_used = False
    legacy_sources = set()
    for index, (item, document) in enumerate(zip(inputs, documents)):
        tag, sha, kind = document["release"], document["sha"], document["kind"]
        _need(tag not in tags, "record-policy-duplicate")
        tags.add(tag)
        images = tuple((name, document["images"][name]) for name in common.COMPONENTS)
        actual_web_source = (boot["legacy_signer"]["source_commit"]
                             if document["images"]["web"] == boot["images"]["web"] else sha)
        _need(item.web_source_sha == actual_web_source, "record-policy-image-source")
        _need(actual_web_source in source_map, "record-policy-source-missing")
        reader = source_map[actual_web_source]
        if index == 0:
            facts = records.ManifestFacts(1, "none", False, True, None, None, None, None, None)
        else:
            facts = records.manifest_facts(document)
        if facts.schema_version == 1:
            _need(not seen_v2, "record-policy-v1-after-v2")
            legacy_sources.add(actual_web_source)
        else:
            seen_v2 = True
        _need(reader.mode != "legacy-reader0" or actual_web_source in legacy_sources,
              "record-policy-legacy-reader-unbound")
        live = entries[-1].live if entries else None
        if index:
            previous = entries[-1]
            _need(document["previous"] == previous.release and
                  common.require_timestamp(document["deployed_at"], "record-policy-time") >
                  common.require_timestamp(previous.deployed_at, "record-policy-time"), "record-policy-link")
            if kind == "promote":
                _need(previous.sha == sha or (previous.sha, sha) in ancestors, "record-policy-ancestry")
            elif kind == "rollback":
                _need(document["rolled_back_from"] == previous.release and
                      any((older.sha, older.images) == (sha, images) for older in entries[:-1]), "record-policy-link")
            else:
                _need(kind == "restore" and (sha, images) == (previous.sha, previous.images)
                      and document["rolled_back_from"] is None, "record-policy-link")
        if baseline is not None and tag == baseline.release:
            _need(facts.schema_version == 1 and not baseline_used and
                  (index == len(documents)-1 or documents[index+1]["schema_version"] == 2),
                  "record-policy-legacy-baseline")
            _need(item.manifest_sha256 == baseline.manifest_sha256 and sha == baseline.source_sha
                  and sha in source_map and source_map[sha].catalog is not None, "record-policy-legacy-baseline")
            origin = source_map[sha]
            live = LiveContract(origin.catalog, tag, item.manifest_sha256, origin)
            baseline_used = True
        if facts.schema_version == 2:
            if kind == "promote":
                _need(sha in source_map and source_map[sha].catalog is not None, "record-policy-source-missing")
                source = source_map[sha]
                _need(facts.catalog == source.catalog, "record-policy-source-contract")
                if live is not None:
                    _progress(live, source, facts)
                elif facts.compat == "bypassed":
                    raise PolicyRefusal("record-policy-legacy-live-unavailable")
                live = LiveContract(source.catalog, tag, item.manifest_sha256, source)
            else:
                _need(live is not None, "record-policy-legacy-live-unavailable")
                _need(facts.catalog == live.catalog, "record-policy-inherited-contract")
            _need(reader.reader_version >= live.catalog.min_reader_version, "record-policy-reader-too-old")
        entries.append(PolicyEntry(tag, kind, sha, images, document["deployed_at"], document["previous"],
                                  document.get("rolled_back_from"), item, facts, reader, live))
    _need(baseline is None or baseline_used, "record-policy-legacy-baseline")
    return PolicyChain(tuple(entries), inputs, sources, ancestors, baseline,
                       all(source.mode != "synthetic-policy" for source in sources))


def _rollback_requirements(chain: PolicyChain, target_tag: str) -> RollbackRequirements:
    """Recheck every crossed record; this is not an operational decision."""
    _need(type(chain) is PolicyChain)
    # Frozen objects are still caller-constructible data. Recheck the complete
    # source/raw/link calculation before trusting a manually assembled chain.
    expected = normalize_chain(chain.inputs, chain.sources, chain.ancestors, baseline=chain.baseline)
    _need(chain == expected, "record-policy-chain-changed")
    _need(type(target_tag) is str and (target_tag == common.BOOTSTRAP_TAG
          or common.RECORD_TAG_RE.fullmatch(target_tag)), "rollback-target-invalid")
    positions = [index for index, entry in enumerate(chain.entries) if entry.release == target_tag]
    _need(len(positions) == 1, "rollback-target-invalid")
    target = chain.entries[positions[0]]
    intermediates = chain.entries[positions[0]+1:]
    live = chain.entries[-1].live
    _need(live is not None, "record-policy-legacy-live-unavailable")
    _need(all(entry.facts.reversible for entry in intermediates), "rollback-irreversible-span")
    _need(target.reader.reader_version >= live.catalog.min_reader_version, "rollback-reader-too-old")
    count = sum(entry.facts.db_change != "none" for entry in intermediates)
    span = tuple((entry.release, entry.record.manifest_sha256)
                 for entry in (target,) + intermediates)
    span_sha256 = _digest(common.canonical_payload_bytes({
        "purpose": "rollback-span-v1", "records": [list(row) for row in span],
    }))
    return RollbackRequirements(target, intermediates, count, live,
                                chain.runtime_source_supported, span, span_sha256)


def rollback_plan(chain: PolicyChain, target_tag: str) -> RollbackDecision:
    """Exact-tag policy calculation, never a writer or provider authorization."""
    required = _rollback_requirements(chain, target_tag)
    _need(required.non_none_count < 2, "rollback-qualification-unavailable")
    facts = records.ManifestFacts(2, "none", False, True, None, required.live.catalog, "bypassed", None, "bypassed")
    return RollbackDecision(required.target, required.intermediates, required.non_none_count,
                            required.live, facts, required.runtime_source_supported)
