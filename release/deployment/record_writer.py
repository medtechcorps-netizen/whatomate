"""Pure schema2 construction, not publication or authentication authority.

The operational caller must create source/image/evidence inputs with concrete
adapters and preserve authenticated same-workflow/run custody after deployment.
These immutable models are only consistency projections. There is deliberately
no arbitrary v2-field mapping or network access here. The separate closed
handoff codec and ship caller preserve custody; these models alone grant none.
Original drill/owner policy is replayed at preserved UTC times, never recollected.
Existing schema1 writing and its legacy-only route are intact.

New promotion qualification uses the preserved predeployment capture. It never
reinterprets later main as the deployed candidate or fetches expired evidence.
Already published exact bytes use a separate idempotency path first. Image-only
v2 records require a retained live contract; absent legacy metadata, outage
bypass, multi-span qualification, and PR20 fixed-forward remain unsupported.
"""
from dataclasses import dataclass
from functools import wraps

import compat_collect as collect
import compat_evidence as evidence
import compat_request as request
import record_resolver as resolver
import forward_gates
import contract_authorization

policy, records, common, transport = resolver.policy, resolver.records, resolver.common, resolver.transport


class WriterRefusal(Exception):
    """Fixed public code only; never raw document or adapter exception text."""


def _need(value, code="record-writer-invalid"):
    if not value:
        raise WriterRefusal(code)


def _public(function):
    @wraps(function)
    def call(*args, **kwargs):
        try:
            return function(*args, **kwargs)
        except WriterRefusal:
            raise
        except (evidence.CompatRefusal, policy.PolicyRefusal, common.ReleaseError, forward_gates.DrillRefusal, contract_authorization.ContractRefusal,
                ValueError, TypeError, AttributeError, KeyError, IndexError,
                RecursionError, OverflowError):
            raise WriterRefusal("record-writer-invalid") from None
    return call


@dataclass(frozen=True, slots=True)
class WorkflowExecution:
    repository: str
    workflow: str
    workflow_sha: str
    run_id: int
    run_attempt: int


@dataclass(frozen=True, slots=True)
class CandidateImages:
    """Actual candidate verification projection; no release tag exists yet.

    A caller must verify each fixed repository's signatures before constructing
    this object. Matching digest/source strings do not perform that verification.
    """
    sha: str
    images: tuple[tuple[str, str], ...]
    source_shas: tuple[tuple[str, str], ...]


@dataclass(frozen=True, slots=True)
class PublicationFacts:
    execution: WorkflowExecution
    deployed_at: str
    built_at: str | None
    environment_values_sha256: str
    non_source_projection_sha256: str


@dataclass(frozen=True, slots=True)
class PromotionHandoff:
    """Private preserved inputs; neither a transferable receipt nor wire schema.

    CollectedHistory preserves paired bytes/subject hashes and verification
    references, not all signature bundle bytes. A later cross-job adapter must
    establish custody or independently verify bounded exact bundles. It cannot
    turn a deserialized dataclass into authority.
    """
    resolution: resolver.ForwardResolution
    collected: collect.CollectedHistory
    candidate_images: CandidateImages
    execution: WorkflowExecution
    facts: records.ManifestFacts
    drills: forward_gates.DrillGate | None = None
    contract_observation: contract_authorization.ContractObservation | None = None


def _execution(value):
    _need(type(value) is WorkflowExecution and type(value.repository) is str and value.repository == common.REPOSITORY
          and type(value.workflow) is str and value.workflow == records.SHIP_SIGNER
          and type(value.run_id) is int and type(value.run_attempt) is int
          and 1 <= value.run_attempt <= 500)
    common.require_sha1(value.workflow_sha, "record-writer-invalid")
    common.require_run_id(value.run_id, "record-writer-invalid")


def _publication(value):
    _need(type(value) is PublicationFacts)
    _execution(value.execution)
    common.require_timestamp(value.deployed_at, "record-writer-invalid")
    if value.built_at is not None:
        common.require_timestamp(value.built_at, "record-writer-invalid")
    common.require_sha256(value.environment_values_sha256, "record-writer-invalid")
    common.require_sha256(value.non_source_projection_sha256, "record-writer-invalid")


def _candidate_images(value, sha):
    _need(type(value) is CandidateImages and value.sha == sha
          and type(value.images) is tuple and len(value.images) == len(common.COMPONENTS)
          and all(type(row) is tuple and len(row) == 2 for row in value.images)
          and tuple(row[0] for row in value.images) == common.COMPONENTS
          and type(value.source_shas) is tuple
          and value.source_shas == tuple((part, sha) for part in common.COMPONENTS))
    for _, digest in value.images:
        common.require_digest(digest, "record-writer-invalid")
    _need(len({digest for _, digest in value.images}) == len(common.COMPONENTS))


def _resolved(value):
    """Check complete projection consistency without selecting a new target."""
    _need(type(value) is resolver.ResolvedChain and type(value.chain) is policy.PolicyChain)
    chain = value.chain
    _need(chain.baseline is None and chain.runtime_source_supported is True
          and policy.normalize_chain(chain.inputs, chain.sources, chain.ancestors) == chain)
    _need(type(value.latest) is resolver.ReleaseSelection and type(value.latest.index) is int
          and value.latest == resolver._selection(chain, len(chain.entries) - 1)
          and type(value.selected) is resolver.ReleaseSelection and type(value.selected.index) is int
          and 0 <= value.selected.index < len(chain.entries)
          and value.selected == resolver._selection(chain, value.selected.index))
    inventory = value.inventory
    _need(type(inventory) is transport.InventorySnapshot and type(inventory.repository_id) is int
          and inventory.repository_id == transport.REPOSITORY_ID)
    common.require_sha1(inventory.main_sha, "record-writer-invalid")
    entries = {row.release: row for row in chain.entries[1:]}
    _need(type(inventory.records) is tuple and len(inventory.records) == len(entries)
          and all(type(row) is transport.ReleaseIdentity for row in inventory.records)
          and tuple(row.release for row in inventory.records) == tuple(sorted(entries)))
    for row in inventory.records:
        entry = entries[row.release]
        _need(all(type(n) is int and n > 0 for n in (row.release_id, row.asset_id, row.size))
              and row.size == len(entry.record.raw)
              and (row.digest is None or row.digest == "sha256:" + entry.record.manifest_sha256))
    _need(len({row.release_id for row in inventory.records}) == len(entries)
          and len({row.asset_id for row in inventory.records}) == len(entries)
          and type(inventory.tags) is tuple and len(inventory.tags) == len(entries)
          and all(type(row) is transport.TagIdentity for row in inventory.tags)
          and tuple((row.release, row.sha) for row in inventory.tags)
          == tuple((tag, entries[tag].sha) for tag in sorted(entries)))
    _need(type(value.image_bindings) is tuple and len(value.image_bindings) == len(chain.entries))
    bootstrap = common.loads_strict(chain.inputs[0].raw)
    for entry, binding in zip(chain.entries, value.image_bindings, strict=True):
        sources = tuple((part, bootstrap["legacy_signer"]["source_commit"]
                        if dict(entry.images)[part] == bootstrap["images"][part] else entry.sha)
                        for part in common.COMPONENTS)
        _need(type(binding) is transport.VerifiedImages and binding.release == entry.release
              and binding.record_sha == entry.sha and binding.images == entry.images
              and binding.source_shas == sources)
    return chain


@_public
def prepare_promotion(resolution, collected, candidate_images, execution, *, staging, drills=None,
                      contract_observation=None):
    """Current production requires observed pending material and context3."""
    _need(type(resolution) is resolver.ForwardResolution
          and type(resolution.pending) is resolver.pending_source.PendingMaterial
          and type(drills) is forward_gates.DrillGate)
    qualified = request.qualify(resolution, collected)
    return _promotion_handoff(resolution, collected, candidate_images, execution,
                              qualified, staging=staging, drills=drills,
                              contract_observation=contract_observation)


@_public
def _prepare_promotion_context2_custody(resolution, collected, candidate_images, execution, *, staging, drills=None):
    """Historical record replay only; the closed codec selects this route.

    No operational caller may use this instead of current source admission.
    These immutable values are data, not a transferable custody capability.
    """
    _need(type(resolution) is resolver.ForwardResolution and resolution.pending is None)
    qualified = request._qualify_context2_custody(resolution, collected)
    return _promotion_handoff(resolution, collected, candidate_images, execution,
                              qualified, staging=staging, drills=drills, contract_observation=None)


def _contract_custody(resolution, collected, drills, observation):
    """Pure original-time replay; no current label read or retry-time clock."""
    receipt = forward_gates.validate_preserved_contract(resolution, observation)
    if observation is not None:
        _need(type(drills) is forward_gates.DrillGate
              and collected.history.observed_at <= observation.observed_at <= drills.observed_at)
    return receipt


def _promotion_handoff(resolution, collected, candidate_images, execution, qualified, *, staging, drills, contract_observation):
    _execution(execution)
    _need(type(staging) is str and staging in ("passed", "bypassed"))
    if drills is None:
        # Exact historical custody only. Current production/encoder requires an
        # observation; an old exempt handoff is not upgraded during record retry.
        change = resolution.classification
        _need((change.kind == "none" and change.dormant is False)
              or (change.kind in ("expand", "data") and change.dormant is True))
    else:
        forward_gates.validate_preserved_drills(resolution, drills)
        _need(drills.observed_at >= collected.history.observed_at)
    _contract_custody(resolution, collected, drills, contract_observation)
    candidate = resolution.candidate
    _candidate_images(candidate_images, candidate.sha)
    # The bridge binds the profile measurement and exact source-authority pins.
    # Keep the complete migration aggregate explicit; server/forward flags can
    # never supply reversibility, and a normal receipt cannot authorize rollback.
    reversible = all(qualified.old_migrate_ok)
    _need(qualified.candidate_sha == candidate.sha and qualified.forward is True
          and qualified.old_server is True and reversible is True and qualified.reversible is True
          and qualified.catalog_sha256 == candidate.catalog.catalog_sha256)
    facts = records.ManifestFacts(2, resolution.classification.kind, resolution.classification.dormant,
        reversible, qualified.old_server, candidate.catalog, "ok", qualified.run_id, staging)
    return PromotionHandoff(resolution, collected, candidate_images, execution, facts, drills, contract_observation)


def _serialize(kind, sha, images, previous, publication, facts, *, reconciled=False):
    _publication(publication)
    _need(type(reconciled) is bool)
    release = "prod-" + common.compact_timestamp(publication.deployed_at) + "-" + sha[:8]
    raw = records.build_manifest({
        "kind": kind, "release": release, "sha": sha, "images": dict(images),
        "previous": previous, "rolled_back_from": previous if kind == "rollback" else None,
        "reconciled": reconciled, "built_at": publication.built_at,
        "deployed_at": publication.deployed_at, "workflow_sha": publication.execution.workflow_sha,
        "environment_values_sha256": publication.environment_values_sha256,
        "non_source_projection_sha256": publication.non_source_projection_sha256,
    })
    value = common.loads_strict(raw)
    value.update(schema_version=2, db_change=facts.db_change, dormant=facts.dormant,
        reversible=facts.reversible, old_server_ok="unproven" if facts.old_server_ok is None else facts.old_server_ok,
        compat=facts.compat, compat_run_id=facts.compat_run_id, staging=facts.staging,
        fixed_forward_from=None, **facts.catalog._asdict())
    records.validate_manifest(value)
    return common.canonical_file_bytes(value)


def _promotion_sources(chain, candidate):
    """Project only the preserved candidate onto a fresh historical view.

    A plain resolver does not load an unreleased candidate. If that candidate
    already supplied a v1 image, its plain reader0 facts can also be less
    complete than the original forward capture. Only that exact source may be
    upgraded; every other historical fact remains subject to full equality.
    """
    _need(type(candidate) is policy.SourceFacts and candidate.mode == "reviewed-v0"
          and candidate.reader_version == 0, "record-writer-parent-changed")
    sources = {source.sha: source for source in chain.sources}
    prior = sources.get(candidate.sha)
    if prior is not None and prior != candidate:
        _need(prior == policy.legacy_reader0(candidate.sha)
              and any(entry.facts.schema_version == 1 and entry.record.web_source_sha == candidate.sha
                      for entry in chain.entries), "record-writer-parent-changed")
    sources[candidate.sha] = candidate
    return tuple(sources[sha] for sha in sorted(sources))


def _append(chain, raw, web_source, *, candidate=None):
    sources, ancestors = chain.sources, chain.ancestors
    if candidate is not None:
        sources = _promotion_sources(chain, candidate)
        parent = chain.entries[-1].sha
        # resolve_forward already checked parent->candidate and DBorigin->candidate
        # in the source operation. This is its link projection, not a new caller-
        # supplied ancestry capability or a post-deadline Git request.
        edge = (parent, candidate.sha)
        if parent != candidate.sha and edge not in ancestors:
            ancestors += (edge,)
    item = policy.RecordInput(raw, common.sha256_bytes(raw), web_source)
    policy.normalize_chain(chain.inputs + (item,), sources, ancestors)
    return raw


@_public
def build_promotion(handoff, publication, current_resolution):
    """Append after deployment against the exact preserved parent, not new main.

    The caller authenticates current_resolution freshly and establishes same-run
    custody. Replaying preserved paired bytes here neither downloads old evidence
    nor requalifies the candidate against the record job's current main.
    """
    _need(type(handoff) is PromotionHandoff)
    expected = prepare_promotion(handoff.resolution, handoff.collected, handoff.candidate_images,
                                 handoff.execution, staging=handoff.facts.staging, drills=handoff.drills,
                                 contract_observation=handoff.contract_observation)
    _need(handoff == expected, "record-writer-handoff-changed")
    return _append_promotion(handoff, publication, current_resolution)


@_public
def _build_promotion_context2_custody(handoff, publication, current_resolution):
    """Only exact decoded historical custody can select this private path."""
    _need(type(handoff) is PromotionHandoff)
    expected = _prepare_promotion_context2_custody(handoff.resolution, handoff.collected,
        handoff.candidate_images, handoff.execution, staging=handoff.facts.staging, drills=handoff.drills)
    _need(handoff == expected, "record-writer-handoff-changed")
    return _append_promotion(handoff, publication, current_resolution)


def _append_promotion(handoff, publication, current_resolution):
    _publication(publication)
    _need(publication.execution == handoff.execution, "record-writer-execution-changed")
    chain = _resolved(current_resolution)
    before = handoff.resolution.resolved
    # Current main and newly assigned HTTP inventory metadata are intentionally
    # not compared; complete signed history/source/image facts must be unchanged.
    candidate = handoff.resolution.candidate
    _need(current_resolution.selected == current_resolution.latest
          and current_resolution.image_bindings == before.image_bindings,
          "record-writer-parent-changed")
    # Compare the complete signed history after the sole permitted enrichment.
    # This also accounts for candidate-overlap reader facts inside selections;
    # neither current main nor any unrelated source is replaced or invented.
    chain = policy.normalize_chain(chain.inputs, _promotion_sources(chain, candidate),
                                   chain.ancestors)
    _need(chain == before.chain, "record-writer-parent-changed")
    raw = _serialize("promote", candidate.sha, handoff.candidate_images.images, before.latest.release,
                     publication, handoff.facts)
    return _append(chain, raw, candidate.sha, candidate=candidate)


@_public
def build_image_only(current_resolution, publication, *, reconciled=False):
    """Exact selected zero/one-change span, never a compatibility experiment.

    Pure policy is recomputed; no caller decision or normal-forward receipt is
    accepted. All-v1 live=None remains the separately guarded schema1 route.
    """
    chain = _resolved(current_resolution)
    decision = policy.rollback_plan(chain, current_resolution.selected.release)
    _need(decision.runtime_source_supported is True)
    target = decision.target
    kind = "restore" if current_resolution.selected == current_resolution.latest else "rollback"
    raw = _serialize(kind, target.sha, target.images, current_resolution.latest.release,
                     publication, decision.image_only_facts, reconciled=reconciled)
    return _append(chain, raw, target.record.web_source_sha)


@_public
def published_state(current_resolution, raw):
    """Exact authenticated final bytes or absent; absent grants no permission.

    Invoke this before preparing new metadata on a publication retry. It needs
    neither expired Actions artifacts nor a fabricated historical target tag.
    Draft release state and actual signature checks belong to the caller.
    """
    chain = _resolved(current_resolution)
    _need(type(raw) is bytes and 0 < len(raw) <= records.MAX_MANIFEST_BYTES)
    value = records.validate_manifest(common.loads_strict(raw))
    _need(common.canonical_file_bytes(value) == raw)
    matches = tuple(row for row in chain.entries if row.release == value["release"])
    if not matches:
        return "absent"
    _need(matches == (chain.entries[-1],) and matches[0].record.raw == raw
          and matches[0].record.manifest_sha256 == common.sha256_bytes(raw), "record-writer-published-conflict")
    return "published"
