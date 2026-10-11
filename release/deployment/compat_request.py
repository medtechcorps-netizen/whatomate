"""Pure normal-compatibility request projection; no authentication or I/O.

The future operational caller must create both inputs internally through the
concrete resolver/collector and recheck freshness before effects. Immutable
dataclasses and equality with a captured main SHA are data consistency checks,
not capabilities or proof that main is still current. This module neither
grants a deleted-history bypass nor activates a release writer.
"""
import compat_collect as collect
import compat_evidence as evidence
import record_resolver as resolver

policy, common = resolver.policy, resolver.common


def _need(value):
    if not value:
        raise evidence.CompatRefusal("compat-stale", "request-resolution-invalid")


def _chain(resolved):
    _need(type(resolved) is resolver.ResolvedChain and type(resolved.chain) is policy.PolicyChain)
    chain = resolved.chain
    _need(chain.baseline is None and chain.runtime_source_supported is True)
    # Reuse the existing complete raw-document/link/source policy rather than
    # treating a manually assembled set of derived entries as a complete chain.
    _need(policy.normalize_chain(chain.inputs, chain.sources, chain.ancestors) == chain)
    latest = resolver._selection(chain, len(chain.entries) - 1)
    _need(type(resolved.latest) is resolver.ReleaseSelection and resolved.latest == latest
          and type(resolved.latest.index) is int
          and type(resolved.selected) is resolver.ReleaseSelection and resolved.selected == latest
          and type(resolved.selected.index) is int)
    inventory = resolved.inventory
    transport = resolver.transport
    _need(type(inventory) is transport.InventorySnapshot
          and type(inventory.repository_id) is int and inventory.repository_id == transport.REPOSITORY_ID
          and type(inventory.main_sha) is str and common.SHA1_RE.fullmatch(inventory.main_sha))
    entries = {row.release: row for row in chain.entries[1:]}
    _need(type(inventory.records) is tuple and len(inventory.records) == len(entries)
          and all(type(row) is transport.ReleaseIdentity for row in inventory.records)
          and tuple(row.release for row in inventory.records) == tuple(sorted(entries)))
    for row in inventory.records:
        entry = entries[row.release]
        _need(all(type(value) is int and value > 0 for value in (row.release_id, row.asset_id, row.size))
              and row.size == len(entry.record.raw)
              and (row.digest is None or row.digest == "sha256:" + entry.record.manifest_sha256))
    _need(len({row.release_id for row in inventory.records}) == len(entries)
          and len({row.asset_id for row in inventory.records}) == len(entries))
    _need(type(inventory.tags) is tuple and len(inventory.tags) == len(entries)
          and all(type(row) is transport.TagIdentity for row in inventory.tags)
          and tuple((row.release, row.sha) for row in inventory.tags)
          == tuple((tag, entries[tag].sha) for tag in sorted(entries)))
    _need(type(resolved.image_bindings) is tuple and len(resolved.image_bindings) == len(chain.entries))
    boot = common.loads_strict(chain.inputs[0].raw)
    for entry, binding in zip(chain.entries, resolved.image_bindings, strict=True):
        expected_sources = tuple((part, boot["legacy_signer"]["source_commit"]
            if dict(entry.images)[part] == boot["images"][part] else entry.sha)
            for part in common.COMPONENTS)
        _need(type(binding) is transport.VerifiedImages and binding.release == entry.release
              and binding.record_sha == entry.sha and binding.images == entry.images
              and binding.source_shas == expected_sources)
    return latest


def _material(resolution, latest):
    candidate, material = resolution.candidate, resolution.catalog_material
    policy._source(candidate)
    _need(candidate.mode == "reviewed-v0" and type(material) is resolver.CatalogMaterial
          and material.candidate_sha == candidate.sha)
    pins = dict(policy.V0_PINS)
    paths = tuple(sorted(pins))
    _need(type(material.candidate_blobs) is tuple and len(material.candidate_blobs) == len(paths)
          and all(type(row) is policy.SourceBlob and type(row.raw) is bytes
                  and 0 < len(row.raw) <= policy.MAX_SOURCE_FILE_BYTES for row in material.candidate_blobs)
          and tuple(row.path for row in material.candidate_blobs) == paths
          and sum(len(row.raw) for row in material.candidate_blobs) <= policy.MAX_SOURCE_BYTES)
    blobs = {row.path: row.raw for row in material.candidate_blobs}
    artifacts = dict(candidate.artifacts)
    _need(all(common.sha256_bytes(blobs[path]) == pins[path] == artifacts[path] for path in paths))
    profile = common.loads_strict(blobs[policy.PROFILE])
    _need(profile["live_sha256"] == candidate.catalog.catalog_sha256 == policy.V0_MEASUREMENT)
    _need(type(material.baseline_blobs) is tuple and len(material.baseline_blobs) == len(paths)
          and all(type(row) is resolver.BlobObservation for row in material.baseline_blobs)
          and tuple(row.path for row in material.baseline_blobs) == paths)
    absent = ()
    if latest.live is None:
        _need(resolution.database_base_source == latest.record_sha
              and material.baseline_sha == latest.actual_web_source_sha)
        if material.baseline_sha == policy.V0_RELEASED_SOURCE:
            _need(material.baseline_provenance == "legacy-e001-absent-overlay")
            absent = (policy.PROFILE, policy.OVERLAY)
        else:
            _need(material.baseline_provenance == "legacy-equal-reviewed-v0")
        base = evidence.DatabaseBase("legacy-source-only", latest.record_sha, None, None)
    else:
        live = latest.live
        policy._source(live.origin_source)
        _need(live.origin_source.mode == "reviewed-v0" and live.catalog == live.origin_source.catalog
              and resolution.database_base_source == live.origin_source.sha == material.baseline_sha
              and material.baseline_provenance == "retained-live-v2")
        base = evidence.DatabaseBase("retained-live-v2", live.origin_source.sha,
            evidence.OriginIdentity(live.origin_release, live.origin_manifest_sha256),
            evidence.CatalogTuple(*live.catalog))
    for row in material.baseline_blobs:
        _need(row.raw is None if row.path in absent else type(row.raw) is bytes and row.raw == blobs[row.path])
    return base, tuple(evidence.CatalogBlob(path, artifacts[path]) for path in paths)


def _build_request(resolution, collected):
    _need(type(resolution) is resolver.ForwardResolution and type(collected) is collect.CollectedHistory)
    latest = _chain(resolution.resolved)
    candidate = resolution.candidate
    _need(type(candidate) is policy.SourceFacts and candidate.sha == resolution.resolved.inventory.main_sha)
    for historical in resolution.resolved.chain.sources:
        _need(historical.sha != candidate.sha or historical == candidate)
    base, authority = _material(resolution, latest)
    _need(resolution.previous_web_source_sha == latest.actual_web_source_sha)
    classification = resolution.classification
    _need(type(classification) is resolver.schema_change.PolicyClassification
          and type(classification.kind) is str and type(classification.dormant) is bool)
    for rows in (classification.reasons, classification.summary,
                 classification.migration_changes, classification.requirements):
        _need(type(rows) is tuple and len(rows) <= 8192
              and all(type(value) is str and len(value) <= 8192 for value in rows))
    _need(not classification.reasons
          and ((classification.kind == "none" and not classification.dormant)
               or (classification.kind == "data" and classification.dormant))
          and classification.requirements == ("compat-required", "generated-migration-closure-ci"))
    _need(type(resolution.workflow) is bytes
          and 0 < len(resolution.workflow) <= policy.MAX_SOURCE_FILE_BYTES
          and common.sha256_bytes(resolution.workflow) == resolution.workflow_sha256 == collect.WORKFLOW_SHA256)
    _need(type(collected.history) is evidence.VisibleHistory
          and collected.history.sha == candidate.sha
          and type(collected.issues) is tuple
          and all(value in ("artifact-unverified", "inventory-changed", "inventory-unavailable", "collection-deadline")
                  for value in collected.issues)
          and type(collected.command_count) is int and 0 < collected.command_count <= collect.MAX_COMMANDS
          and type(collected.captured_bytes) is int and 0 <= collected.captured_bytes <= collect.MAX_BYTES
          and type(collected.transcript_sha256) is str and common.SHA256_RE.fullmatch(collected.transcript_sha256))
    identity = evidence.ReleaseIdentity(latest.release, latest.manifest_sha256,
                                        latest.record_sha, dict(latest.images)["web"])
    source = evidence.SourceFacts(candidate.sha, identity,
        evidence.Classification(classification.kind, classification.dormant, classification.reasons),
        candidate.catalog.catalog_sha256, authority, base, latest.actual_web_source_sha)
    request = evidence.NormalRequest(source, identity, collected.history.observation_id)
    evidence._request(request)
    evidence._history(collected.history, candidate.sha, request.observation_id)
    return request


def build_request(resolution, collected) -> evidence.NormalRequest:
    """Project exact bound data, preserving the collector's observation ID.

    Qualification remains separate: an older unverified artifact issue may be
    safely superseded under the existing execution-window policy. No blanket
    issues-empty condition or new history/negative policy is introduced here.
    """
    try:
        return _build_request(resolution, collected)
    except evidence.CompatRefusal:
        raise
    except (policy.PolicyRefusal, common.ReleaseError, ValueError, TypeError,
            AttributeError, KeyError, IndexError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "request-resolution-invalid") from None


def qualify(resolution, collected) -> evidence.QualifiedCompat:
    """Use the existing normal policy; never a rollback/bypass authorization."""
    return evidence.require_compat(build_request(resolution, collected), collected.history)
