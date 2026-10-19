"""Pure normal-compatibility request projection; no authentication or I/O.

The future operational caller must create both inputs internally through the
concrete resolver/collector and recheck freshness before effects. Immutable
dataclasses and equality with a captured main SHA are data consistency checks,
not capabilities or proof that main is still current. This module neither
grants a deleted-history bypass nor activates a release writer.
"""
from dataclasses import replace

import compat_collect as collect
import compat_evidence as evidence
import pending_source
import record_resolver as resolver
import compat_rollback_history as rollback_history
import catalog as compat_catalog

policy, common = resolver.policy, resolver.common
W0_WORKFLOW_SHA256 = "0f8ae9f3079d4a3c0749721e5100d6df60842b4811059aee02a2ee4d8ced4f6b"
P0_WORKFLOW_SHA256 = "56005fe2015e8385e6ecfc621a403c06a534ed9a5712e5ec4ec4f9c70407b941"


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


def _source_base(resolution):
    _need(type(resolution) is resolver.ForwardResolution)
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
          and classification.requirements == ("compat-required", "generated-migration-closure-ci"))
    _need(type(resolution.workflow) is bytes
          and 0 < len(resolution.workflow) <= policy.MAX_SOURCE_FILE_BYTES
          and common.sha256_bytes(resolution.workflow) == resolution.workflow_sha256)
    return latest, base, authority


def _current_workflow(resolution):
    _need(type(resolution.workflow) is bytes
          and 0 < len(resolution.workflow) <= policy.MAX_SOURCE_FILE_BYTES
          and common.sha256_bytes(resolution.workflow) == resolution.workflow_sha256 == collect.WORKFLOW_SHA256)


def _custody_workflow(resolution):
    # Exact source-owned cases; this does not confer publisher coverage.
    _need(type(resolution.workflow) is bytes
          and 0 < len(resolution.workflow) <= policy.MAX_SOURCE_FILE_BYTES
          and common.sha256_bytes(resolution.workflow) == resolution.workflow_sha256
          and resolution.workflow_sha256 in (W0_WORKFLOW_SHA256, P0_WORKFLOW_SHA256,
                                             collect.WORKFLOW_SHA256))


def _build_request_base(resolution, collected):
    _need(type(resolution) is resolver.ForwardResolution and type(collected) is collect.CollectedHistory)
    latest, base, authority = _source_base(resolution)
    candidate, classification = resolution.candidate, resolution.classification
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
    evidence._history(collected.history, candidate.sha, request.observation_id)
    return request


def _pending_projection(resolution):
    active = {row.path: row.raw for row in resolution.catalog_material.candidate_blobs}
    projection = pending_source.replay(resolution.pending,
        candidate_sha=resolution.candidate.sha, previous_sha=resolution.resolved.latest.record_sha,
        active_catalog=active[policy.GOLDEN], active_globals=active[policy.GLOBALS],
        active_history=active[policy.HISTORY])
    _need(projection.plan is None or all(entry.kind == "expand" for entry in projection.plan.entries))
    return projection


def _bind_pending_request(resolution, request):
    projection = _pending_projection(resolution)
    request = replace(request, source=replace(request.source, pending_binding=projection.binding))
    evidence._request(request)
    return request


def _build_request(resolution, collected):
    request = _build_request_base(resolution, collected)
    _current_workflow(resolution)
    return _bind_pending_request(resolution, request)


def _build_context3_custody_request(resolution, collected):
    request = _build_request_base(resolution, collected)
    _custody_workflow(resolution)
    return _bind_pending_request(resolution, request)


def _build_context2_custody_request(resolution, collected):
    _need(type(resolution) is resolver.ForwardResolution and resolution.pending is None)
    request = _build_request_base(resolution, collected)
    _need(resolution.workflow_sha256 == W0_WORKFLOW_SHA256)
    evidence._request_context2_custody(request)
    return request


def _fixed_build(function, resolution, collected):
    try:
        return function(resolution, collected)
    except evidence.CompatRefusal:
        raise
    except (policy.PolicyRefusal, common.ReleaseError, ValueError, TypeError,
            AttributeError, KeyError, IndexError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "request-resolution-invalid") from None


def build_request(resolution, collected) -> evidence.NormalRequest:
    """Project exact bound data, preserving the collector's observation ID.

    Qualification remains separate: an older unverified artifact issue may be
    safely superseded under the existing execution-window policy. No blanket
    issues-empty condition or new history/negative policy is introduced here.
    """
    return _fixed_build(_build_request, resolution, collected)


def qualify(resolution, collected) -> evidence.QualifiedCompat:
    """Use the existing normal policy; never a rollback/bypass authorization."""
    return evidence.require_compat(build_request(resolution, collected), collected.history)


def _qualify_context2_custody(resolution, collected) -> evidence.QualifiedCompat:
    """Exact old context2 replay, solely for closed original same-run custody.

    No current source resolver yields pending=None. The codec/writer must bind
    the original wire version, execution and parent before choosing this path;
    a Python object or call here does not authenticate those external facts.
    """
    request = _fixed_build(_build_context2_custody_request, resolution, collected)
    return evidence._require_context2_custody(request, collected.history)


def _qualify_context3_custody(resolution, collected) -> evidence.QualifiedCompat:
    """Closed original context3 replay, never current workflow admission."""
    request = _fixed_build(_build_context3_custody_request, resolution, collected)
    return evidence.require_compat(request, collected.history)


def _validate_source_material(resolution):
    projection = _pending_projection(resolution)
    contract = compat_catalog.CatalogContract.from_resolution(resolution)
    _need(contract.pending_binding == projection.binding)
    change = resolution.classification
    _need((change.kind == "none" and not change.dormant)
          or (change.kind == "data" and change.dormant)
          or (change.kind == "expand" and change.dormant and projection.binding.target_version is not None))
    resolver.validate_forward_audit(resolution.audit, candidate_sha=resolution.candidate.sha,
        database_base_source=resolution.database_base_source,
        latest_record_sha=resolution.resolved.latest.record_sha,
        selected_web_source_sha=resolution.resolved.selected.actual_web_source_sha)
    recipe = resolution.previous_recipe
    _need(type(recipe) is resolver.WebRecipe and recipe.path == resolver._WEB_RECIPE
          and recipe.source_sha == resolution.previous_web_source_sha
          and type(recipe.raw) is bytes and 0 < len(recipe.raw) <= policy.MAX_SOURCE_FILE_BYTES
          and common.sha256_bytes(recipe.raw) == recipe.sha256)
    return resolution


def validate_current_source(resolution):
    """Pure source validation; no fabricated collection or availability fact."""
    try:
        _source_base(resolution)
        _current_workflow(resolution)
        return _validate_source_material(resolution)
    except evidence.CompatRefusal:
        raise
    except (compat_catalog.proof.EvidenceError, policy.PolicyRefusal, common.ReleaseError,
            ValueError, TypeError, AttributeError, KeyError, IndexError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "request-resolution-invalid") from None


def _validate_source_custody(resolution):
    """Exact preserved source replay only; old workflow is not index coverage."""
    try:
        _source_base(resolution)
        _custody_workflow(resolution)
        return _validate_source_material(resolution)
    except evidence.CompatRefusal:
        raise
    except (compat_catalog.proof.EvidenceError, policy.PolicyRefusal, common.ReleaseError,
            ValueError, TypeError, AttributeError, KeyError, IndexError, RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "request-resolution-invalid") from None


def _build_rollback_request(resolution, collected):
    # Reuse the complete source/material validator before deriving a compact
    # commitment. Neither the digest nor a typed resolution authenticates the
    # source, signed chain, or its currentness outside the concrete operation.
    contract = compat_catalog.CatalogContract.from_rollback_resolution(resolution)
    material = resolver.rollback_material_digest(resolution)
    latest, selected = resolution.resolved.latest, resolution.resolved.selected
    required = resolution.requirements
    live = required.live
    identity = lambda row: evidence.ReleaseIdentity(row.release, row.manifest_sha256,
                                                    row.record_sha, dict(row.images)['web'])
    binding = evidence.RollbackBinding(resolution.producer.sha, identity(latest), identity(selected),
        evidence.DatabaseBase('retained-live-v2', live.origin_source.sha,
            evidence.OriginIdentity(live.origin_release, live.origin_manifest_sha256),
            evidence.CatalogTuple(*live.catalog)), selected.actual_web_source_sha,
        latest.actual_web_source_sha,
        evidence.RollbackSpan(len(required.span), required.non_none_count, required.span_sha256), material)
    # This complete pure replay also validates all-head coverage, raw history
    # bounds and the collected metadata. Artifact issues stay available to the
    # qualification policy; they are not a blanket veto on earlier known work.
    rollback_history.validate_collected(collected)
    history = collected.history
    _need(history.producer_sha == binding.producer_sha
          and contract.catalog_control_sha == binding.producer_sha
          and contract.target_sha256 == binding.database_base.catalog.catalog_sha256)
    observed = next((row for row in history.workflows if row.sha == binding.producer_sha), None)
    _need(observed is not None and observed.raw == resolution.workflow)
    return rollback_history.validate_request(rollback_history.RollbackRequest(
        binding, resolution.workflow, history.observation_id))


def build_rollback_request(resolution, collected) -> rollback_history.RollbackRequest:
    """Project a fresh internally resolved exact rollback span, without I/O.

    The future caller must own concrete resolution/collection and final source
    freshness before effects. This function adds no operational permission or
    deleted-history coverage and never treats context bytes as source input.
    """
    try:
        return _build_rollback_request(resolution, collected)
    except evidence.CompatRefusal:
        raise
    except (compat_catalog.proof.EvidenceError, policy.PolicyRefusal, common.ReleaseError,
            ValueError, TypeError, AttributeError, KeyError, IndexError, RecursionError, OverflowError):
        raise evidence.CompatRefusal('compat-stale', 'rollback-request-resolution-invalid') from None


def qualify_rollback(resolution, collected) -> rollback_history.QualifiedRollback:
    """Apply dedicated all-head policy; never return a RollbackDecision."""
    request = build_rollback_request(resolution, collected)
    return rollback_history.require_rollback_compat(request, collected.history)
